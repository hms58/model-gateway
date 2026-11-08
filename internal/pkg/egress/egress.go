// Package egress 统一构建访问下游产品所用的 http.Client。
//
// 三条出网路径（聊天转发 / 拉取模型目录 / 探活）都必须遵守同一个产品的代理设置，
// 否则会出现「聊天走代理成功、面板拉取模型却直连失败」这种难排查的分裂行为。
// Go 标准库 net/http 原生支持 http/https/socks5/socks5h 代理 URL，无需第三方依赖。
package egress

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"model-gateway/internal/config"
)

// clientKey 决定什么时候必须换一个新 client：产品、代理、超时任一变化都换。
// 不缓存就无法连接池复用；缓存不区分这些字段又会让面板改完代理不生效。
type clientKey struct {
	productID string
	proxy     string
	connect   time.Duration
	firstByte time.Duration
	total     time.Duration
	streaming bool
}

var clients sync.Map // clientKey -> *http.Client

// Client 返回按产品代理与超时配置构建的 client，带连接池复用。
// streaming=true 时不设总超时（长流不能被掐断），首包超时由 Transport 承担。
func Client(product config.Product, connect, firstByte, total time.Duration, streaming bool) (*http.Client, error) {
	proxyURL, err := config.ParseProxyURL(product.Proxy)
	if err != nil {
		return nil, fmt.Errorf("产品 %s 代理配置非法：%w", product.ID, err)
	}
	proxyFingerprint := ""
	if proxyURL != nil {
		proxyFingerprint = proxyURL.String()
	}
	if streaming {
		total = 0
	}
	key := clientKey{
		productID: product.ID, proxy: proxyFingerprint,
		connect: connect, firstByte: firstByte, total: total, streaming: streaming,
	}
	if cached, ok := clients.Load(key); ok {
		return cached.(*http.Client), nil
	}

	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: connect}).DialContext,
		ResponseHeaderTimeout: firstByte,
		MaxIdleConnsPerHost:   32,
	}
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	client := &http.Client{Transport: transport, Timeout: total}
	actual, _ := clients.LoadOrStore(key, client)
	return actual.(*http.Client), nil
}

// Invalidate 清空某产品的 client 缓存，并关闭其空闲连接。
//
// Desc 里写的「配置保存后无需手动调用」不成立：面板保存是一次全量覆盖，
// 删掉再重建同名产品时 clientKey 完全相同，旧 client 会被直接复用；
// 且每个被淘汰的 client 仍握着最多 32 条空闲连接，不显式关闭就一直占着。
// 所以产品保存 / 删除时必须调用这里（见 panel.saveProduct 与删除分支）。
func Invalidate(productID string) {
	clients.Range(func(key, value any) bool {
		item, ok := key.(clientKey)
		if !ok || item.productID != productID {
			return true
		}
		if client, ok := value.(*http.Client); ok {
			if transport, ok := client.Transport.(*http.Transport); ok {
				transport.CloseIdleConnections()
			}
		}
		clients.Delete(key)
		return true
	})
}

// MaskProxyURL 隐藏代理 URL 里的密码，供管理面回显；探针与日志都不应出现明文。
// 形如 socks5://user:pass@host:1080 -> socks5://user:****@host:1080。
func MaskProxyURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	if _, hasPassword := parsed.User.Password(); !hasPassword {
		return raw
	}
	parsed.User = url.UserPassword(parsed.User.Username(), "********")
	// url.String() 会把星号百分号编码成 %2A；掩码要保持人眼可读且能与
	// 面板回填的值逐字比较，所以还原成字面星号。
	return strings.ReplaceAll(parsed.String(), "%2A", "*")
}
