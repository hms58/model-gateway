package egress

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-gateway/internal/config"
)

func TestProxyDefaultsToDirect(t *testing.T) {
	client, err := Client(config.Product{ID: "p1"}, 5*time.Second, 30*time.Second, 30*time.Second, false)
	if err != nil {
		t.Fatalf("默认直连不应报错：%v", err)
	}
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("代理默认关闭时不应设置 Transport.Proxy")
	}
	if client.Timeout != 30*time.Second {
		t.Fatalf("非流式总超时应为 30s，实际 %v", client.Timeout)
	}
}

func TestProxyCacheSeparatesProductsAndConfigs(t *testing.T) {
	base := config.Product{ID: "p1"}
	a, _ := Client(base, 5*time.Second, 30*time.Second, 30*time.Second, false)
	b, _ := Client(base, 5*time.Second, 30*time.Second, 30*time.Second, false)
	if a != b {
		t.Fatal("相同配置应复用同一个 client（连接池）")
	}
	withProxy := config.Product{ID: "p1", Proxy: config.Proxy{Enabled: true, URL: "socks5://127.0.0.1:1080"}}
	c, err := Client(withProxy, 5*time.Second, 30*time.Second, 30*time.Second, false)
	if err != nil {
		t.Fatalf("socks5 代理应可用：%v", err)
	}
	if c == a {
		t.Fatal("代理配置变化后必须换 client，否则面板改完不生效")
	}
	if c.Transport.(*http.Transport).Proxy == nil {
		t.Fatal("socks5 代理应设置 Transport.Proxy")
	}
	other := config.Product{ID: "p2"}
	d, _ := Client(other, 5*time.Second, 30*time.Second, 30*time.Second, false)
	if d == a {
		t.Fatal("不同产品不应共享 client")
	}
	stream, _ := Client(base, 5*time.Second, 30*time.Second, 30*time.Second, true)
	if stream.Timeout != 0 {
		t.Fatalf("流式不应设总超时，实际 %v", stream.Timeout)
	}
}

func TestProxyURLValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		ok   bool
	}{
		{"http", "http://127.0.0.1:7890", true},
		{"https", "https://proxy.example.com:443", true},
		{"socks5", "socks5://user:pass@127.0.0.1:1080", true},
		{"socks5h", "socks5h://127.0.0.1:1080", true},
		{"缺端口", "socks5://127.0.0.1", true}, // host 存在即放行，端口由拨号时报错
		{"无协议", "127.0.0.1:7890", false},
		{"不支持的协议", "ftp://127.0.0.1:21", false},
		{"空地址", "", false},
		{"无 host", "http://", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.ParseProxyURL(config.Proxy{Enabled: true, URL: tc.url})
			if (err == nil) != tc.ok {
				t.Fatalf("%q 期望 ok=%v，实际 err=%v", tc.url, tc.ok, err)
			}
		})
	}
	if _, err := Client(config.Product{ID: "bad", Proxy: config.Proxy{Enabled: true, URL: "ftp://x"}},
		5*time.Second, 30*time.Second, 30*time.Second, false); err == nil {
		t.Fatal("非法代理必须让请求直接失败，而不是静默直连")
	}
}

func TestMaskProxyURL(t *testing.T) {
	raw := "socks5://user:secret@127.0.0.1:1080"
	masked := MaskProxyURL(raw)
	if strings.Contains(masked, "secret") || !strings.Contains(masked, "********") {
		t.Fatalf("代理密码未掩码：%q", masked)
	}
	plain := "http://127.0.0.1:7890"
	if MaskProxyURL(plain) != plain {
		t.Fatalf("无密码的 URL 不应被改写：%q", MaskProxyURL(plain))
	}
	if MaskProxyURL("") != "" {
		t.Fatal("空 URL 掩码应为空")
	}
}

// HTTP 代理真实生效：上游之外的请求都应打到代理服务器上。
func TestHTTPProxyActuallyUsed(t *testing.T) {
	var seen int
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer proxyServer.Close()

	product := config.Product{
		ID: "p", Proxy: config.Proxy{Enabled: true, URL: proxyServer.URL},
	}
	client, err := Client(product, 5*time.Second, 30*time.Second, 30*time.Second, false)
	if err != nil {
		t.Fatalf("构建代理 client 失败：%v", err)
	}
	// 目标地址故意用一个不可能直连的 host，只有走代理才能成功。
	req, _ := http.NewRequest(http.MethodGet, "http://unreachable.invalid/v1/models", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求应经代理完成，实际失败：%v", err)
	}
	defer resp.Body.Close()
	if seen != 1 {
		t.Fatalf("代理服务器应收到 1 次请求，实际 %d", seen)
	}
}
