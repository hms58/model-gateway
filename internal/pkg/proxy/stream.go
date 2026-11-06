package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"model-gateway/internal/config"
)

// Usage 是从响应里提取到的用量；Seen 表示确实出现过 usage 字段。
type Usage struct {
	Seen             bool
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CacheHitTokens 是上游前缀缓存命中的 prompt token 数。
	// 各上游字段名不统一（见 cacheHitOf），取到非零即记，取不到保持 0。
	CacheHitTokens int
	// Credit 是本次请求消耗的积分；HasCredit 区分「没给」和「给了 0」。
	Credit    float64
	HasCredit bool
}

// StreamCopy 把下游响应体逐块写给客户端并立即 Flush，不做整包缓冲。
// 同时按行提取 usage（SSE 末尾的 token_usage 与非流式的响应体都能命中）。
func StreamCopy(dst http.ResponseWriter, src io.Reader, flusher http.Flusher) (Usage, error) {
	reader := bufio.NewReader(src)
	usage := Usage{}
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, writeErr := dst.Write(line); writeErr != nil {
				return mergeUsage(usage, line), writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
			usage = mergeUsage(usage, line)
		}
		if readErr == io.EOF {
			return usage, nil
		}
		if readErr != nil {
			return usage, readErr
		}
	}
}

// mergeUsage 只对含 "usage" 的行做一次最小反序列化，其余行原样放行。
func mergeUsage(current Usage, line []byte) Usage {
	if !bytes.Contains(line, []byte(`"usage"`)) {
		return current
	}
	start := bytes.IndexByte(line, '{')
	end := bytes.LastIndexByte(line, '}')
	if start < 0 || end <= start {
		return current
	}
	var payload struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(line[start:end+1], &payload); err != nil {
		// 出现了 usage 但结构不是预期形态：只标记，不猜数值。
		current.Seen = true
		return current
	}
	usage := payload.Usage
	if len(usage) == 0 {
		current.Seen = true
		return current
	}
	next := Usage{
		Seen:             true,
		PromptTokens:     numberAt(usage, "prompt_tokens"),
		CompletionTokens: numberAt(usage, "completion_tokens"),
		TotalTokens:      numberAt(usage, "total_tokens"),
		CacheHitTokens:   cacheHitOf(usage),
	}
	if credit, ok := creditOf(usage); ok {
		next.Credit, next.HasCredit = credit, true
	}
	// 后续分片可能只带部分字段：保留已取到的非零值，避免被空分片覆盖。
	if next.PromptTokens == 0 {
		next.PromptTokens = current.PromptTokens
	}
	if next.CompletionTokens == 0 {
		next.CompletionTokens = current.CompletionTokens
	}
	if next.TotalTokens == 0 {
		next.TotalTokens = current.TotalTokens
	}
	if next.CacheHitTokens == 0 {
		next.CacheHitTokens = current.CacheHitTokens
	}
	if !next.HasCredit && current.HasCredit {
		next.Credit, next.HasCredit = current.Credit, true
	}
	return next
}

// cacheHitOf 取上游前缀缓存命中 token。各 *2api 字段名不统一，按优先级探测：
// prompt_tokens_details.cached_tokens / prompt_cache_hit_tokens /
// cache_read_input_tokens / cached_tokens / input_tokens_details.cached_tokens。
// 有些上游会同时给出真实值和 0 兼容别名，所以取第一个非零，而不是第一个存在的。
func cacheHitOf(usage map[string]any) int {
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if value := numberAt(details, "cached_tokens"); value > 0 {
			return value
		}
	}
	for _, key := range []string{"prompt_cache_hit_tokens", "cache_read_input_tokens", "cached_tokens"} {
		if value := numberAt(usage, key); value > 0 {
			return value
		}
	}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if value := numberAt(details, "cached_tokens"); value > 0 {
			return value
		}
	}
	return 0
}

// creditOf 取本次请求消耗的积分；未提供时返回 false（区别于「消耗 0 积分」）。
func creditOf(usage map[string]any) (float64, bool) {
	for _, key := range []string{"credit", "credits", "cost"} {
		if raw, ok := usage[key]; ok {
			if value, ok := numberOf(raw); ok {
				return value, true
			}
		}
	}
	return 0, false
}

// numberAt 读取 JSON 数字字段；字段缺失或不是数字一律按 0。
func numberAt(object map[string]any, key string) int {
	value, ok := numberOf(object[key])
	if !ok {
		return 0
	}
	return int(value)
}

// numberOf 把 json.Unmarshal 到 any 的数字统一成 float64。
func numberOf(raw any) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case json.Number:
		parsed, err := value.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

// DoStreamResponse 发起流式请求：只返回响应头与 body 流，由调用方逐块转发。
// 首字节是否已写出由调用方掌握，这是「能否换成员」的分界点。
func (f *Forwarder) DoStreamResponse(ctx context.Context, product config.Product,
	body []byte, model string) (*http.Response, error) {

	request, err := newChatRequest(ctx, product, body, model)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "text/event-stream")
	client, err := f.clientFor(product, true)
	if err != nil {
		return nil, err
	}
	return client.Do(request)
}

// DoResponse 发起非流式请求：同样只返回响应头与未读取的 body，
// 由调用方用 StreamCopy 边读边写，避免整包驻留内存。
func (f *Forwarder) DoResponse(ctx context.Context, product config.Product,
	body []byte, model string) (*http.Response, error) {

	request, err := newChatRequest(ctx, product, body, model)
	if err != nil {
		return nil, err
	}
	client, err := f.clientFor(product, false)
	if err != nil {
		return nil, err
	}
	return client.Do(request)
}

func newChatRequest(ctx context.Context, product config.Product, body []byte, model string) (*http.Request, error) {
	rewritten, err := RewriteModel(body, model)
	if err != nil {
		return nil, err
	}
	url := product.ChatEndpoint()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rewritten))
	if err != nil {
		return nil, err
	}
	for name, value := range product.Headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+product.APIKey)
	return request, nil
}
