// Package probe 用「一次最小真实请求」验证某个产品能否服务某个模型。
//
// 为什么不用 /v1/models：那个接口只能说明「产品在线、目录里有这个模型 id」，
// 上游完全可能目录里挂着但实际调用报 6004 / 404 / 5xx。要判断模型本身是否恢复，
// 只能真的发一次请求（max_tokens=1，成本可忽略）。
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/egress"
)

// Result 是一次模型探活的结果。
type Result struct {
	OK      bool
	Latency time.Duration
	Reason  string
}

// Model 对 product 发一次最小 chat 请求，验证 model 是否能被接单。
func Model(ctx context.Context, product config.Product, model string, timeout time.Duration) Result {
	if product.Protocol != "" && product.Protocol != config.ProtocolOpenAIChat {
		return Result{Reason: "协议不支持：" + product.Protocol}
	}
	if strings.TrimSpace(product.BaseURL) == "" {
		return Result{Reason: "产品未配置 base_url"}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	payload, err := json.Marshal(map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		return Result{Reason: "构造探活请求失败"}
	}

	url := product.ChatEndpoint()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return Result{Reason: "构造探活请求失败"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+product.APIKey)
	for name, value := range product.Headers {
		request.Header.Set(name, value)
	}

	// 探活必须和聊天转发走同一套代理，否则「配了代理的业务请求成功、嗅探却一直失败」。
	client, err := egress.Client(product, 5*time.Second, timeout, timeout, false)
	if err != nil {
		return Result{Reason: clip(err.Error(), 160)}
	}
	started := time.Now()
	response, err := client.Do(request)
	latency := time.Since(started)
	if err != nil {
		return Result{Latency: latency, Reason: clip(err.Error(), 160)}
	}
	defer response.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return Result{OK: true, Latency: latency}
	}
	reason := fmt.Sprintf("HTTP %d", response.StatusCode)
	if snippet := strings.TrimSpace(string(body)); snippet != "" {
		reason += "：" + snippet
	}
	return Result{Latency: latency, Reason: clip(reason, 200)}
}

func clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
