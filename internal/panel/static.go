package panel

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webAssets embed.FS

// staticHandler 提供面板 SPA：HTML 不缓存、静态资源长缓存、未命中回落入口页。
// 不设置 X-Frame-Options，允许被自建门户 iframe 承载。
func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "面板资源缺失", http.StatusInternalServerError)
		})
	}
	files := http.FileServer(http.FS(sub))
	index, readErr := fs.ReadFile(sub, "index.html")
	if readErr != nil {
		index = []byte("<!doctype html><title>modelgw</title>")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /panel 与 /admin 是等价前缀：剥掉后按同一套静态资源解析。
		path := r.URL.Path
		if strings.HasPrefix(path, "/panel") {
			path = path[len("/panel"):]
		} else if strings.HasPrefix(path, "/admin") {
			path = path[len("/admin"):]
		}
		if path == "" || path == "/" || !exists(sub, path) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(index)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		r.URL.Path = path
		files.ServeHTTP(w, r)
	})
}

func exists(fsys fs.FS, path string) bool {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return false
	}
	info, err := fs.Stat(fsys, path)
	return err == nil && !info.IsDir()
}
