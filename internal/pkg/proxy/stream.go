package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"model-gateway/internal/config"
)

// Usage 是从响应里提取到的 token 用量；Seen 表示确实出现过 usage 字段。
type Usage struct {
	Seen             bool
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
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
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(line[start:end+1], &payload); err != nil {
		// 出现了 usage 但结构不是预期形态：只标记，不猜数值。
		current.Seen = true
		return current
	}
	usage := payload.Usage
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
		current.Seen = true
		return current
	}
	return Usage{
		Seen:             true,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
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
	return f.clientFor(product, true).Do(request)
}

// DoResponse 发起非流式请求：同样只返回响应头与未读取的 body，
// 由调用方用 StreamCopy 边读边写，避免整包驻留内存。
func (f *Forwarder) DoResponse(ctx context.Context, product config.Product,
	body []byte, model string) (*http.Response, error) {

	request, err := newChatRequest(ctx, product, body, model)
	if err != nil {
		return nil, err
	}
	return f.clientFor(product, false).Do(request)
}

func newChatRequest(ctx context.Context, product config.Product, body []byte, model string) (*http.Request, error) {
	rewritten, err := RewriteModel(body, model)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(product.BaseURL, "/") + "/v1/chat/completions"
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
