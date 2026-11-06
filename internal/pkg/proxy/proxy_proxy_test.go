package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"model-gateway/internal/config"
)

// 产品开启 HTTP 代理后，聊天转发必须真的经过代理；目标地址用直连必失败的域名。
func TestDoResponseUsesProductProxy(t *testing.T) {
	var seenPath string
	var seenAuth string
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer proxyServer.Close()

	product := config.Product{
		ID: "p", BaseURL: "http://unreachable.invalid", APIKey: "sk-test",
		Timeout: config.Timeout{Connect: "5s", FirstByte: "30s", Total: "300s"},
		Proxy:   config.Proxy{Enabled: true, URL: proxyServer.URL},
	}
	body, _ := json.Marshal(map[string]interface{}{"model": "gpt-5", "messages": []interface{}{}})
	response, err := NewForwarder().DoResponse(context.Background(), product, body, "gpt-5")
	if err != nil {
		t.Fatalf("经代理转发应成功，实际失败：%v", err)
	}
	defer response.Body.Close()
	if seenPath != "/v1/chat/completions" {
		t.Fatalf("代理应收到聊天请求，实际路径 %q", seenPath)
	}
	if seenAuth != "Bearer sk-test" {
		t.Fatalf("代理请求应带产品 key，实际 %q", seenAuth)
	}
}

// 代理配置非法时请求必须直接报错，不能静默直连到真实上游。
func TestDoResponseRejectsInvalidProxy(t *testing.T) {
	product := config.Product{
		ID: "p", BaseURL: "http://127.0.0.1:9",
		Proxy: config.Proxy{Enabled: true, URL: "ftp://127.0.0.1:21"},
	}
	body, _ := json.Marshal(map[string]interface{}{"model": "gpt-5", "messages": []interface{}{}})
	if _, err := NewForwarder().DoResponse(context.Background(), product, body, "gpt-5"); err == nil {
		t.Fatal("非法代理应让请求失败")
	}
}

// 关掉代理后立即恢复直连，且不能复用带代理的 client。
func TestClientCacheDoesNotLeakProxyAcrossProducts(t *testing.T) {
	var proxied int
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied++
		w.WriteHeader(http.StatusOK)
	}))
	defer proxyServer.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	forwarder := NewForwarder()
	body, _ := json.Marshal(map[string]interface{}{"model": "m", "messages": []interface{}{}})
	withProxy := config.Product{ID: "with-proxy", BaseURL: upstream.URL,
		Proxy: config.Proxy{Enabled: true, URL: proxyServer.URL}}
	resp, err := forwarder.DoResponse(context.Background(), withProxy, body, "m")
	if err != nil {
		t.Fatalf("带代理请求失败：%v", err)
	}
	resp.Body.Close()
	if proxied != 1 {
		t.Fatalf("代理应收到 1 次请求，实际 %d", proxied)
	}

	direct := config.Product{ID: "direct", BaseURL: upstream.URL}
	resp2, err := forwarder.DoResponse(context.Background(), direct, body, "m")
	if err != nil {
		t.Fatalf("直连请求失败：%v", err)
	}
	resp2.Body.Close()
	if proxied != 1 {
		t.Fatalf("直连产品不应再走代理，代理收到 %d 次", proxied)
	}
}
