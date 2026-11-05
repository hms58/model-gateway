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

// isLoopback 判断请求是否来自本机：优先 X-Forwarded-For 首段（反代场景），
// 没有该头时看 RemoteAddr。只认回环地址，不认「看起来像内网」的地址。
func isLoopback(r *http.Request) bool {
	host := ""
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		host = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	if host == "" {
		if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			host = h
		} else {
			host = r.RemoteAddr
		}
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
