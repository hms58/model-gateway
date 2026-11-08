package panel

import (
	"net"
	"net/http"
	"strings"

	"model-gateway/internal/config"
)

func bearerOf(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

// unset 表示本进程启动时没有配置管理口令，管理面处于「初始化模式」。
func (s *Server) unset() bool {
	return strings.TrimSpace(s.adminTokenSHA256) == ""
}

// adminOK 判断管理面口令是否正确：只比对 SHA-256，服务端不留明文。
func (s *Server) adminOK(r *http.Request) bool {
	return config.MatchAdminToken(bearerOf(r), s.adminTokenSHA256)
}

// authorized 是管理面的唯一放行判据。
//
// 未设口令时进入初始化模式：只放行本机回环（本机浏览器或 ssh 隧道），
// 让用户先在配置页设好口令再重启；非回环来源一律拒绝，避免「无口令」被远程利用。
func (s *Server) authorized(r *http.Request) bool {
	if s.adminOK(r) {
		return true
	}
	if s.unset() {
		return isLoopback(r)
	}
	return false
}

// isLoopback 判断请求是否来自本机，只认 TCP 对端地址。
//
// 刻意不采信 X-Forwarded-For：该头完全由客户端控制，而这里决定的是「未设口令时
// 谁能拿到管理面」——管理面可改 base_url，一旦被伪造成回环就等于把上游 api_key
// 引到攻击者地址。反代场景请设好口令（口令校验与来源无关），不要靠伪造头放行。
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
