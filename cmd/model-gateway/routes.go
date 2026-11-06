package main

import (
	"net/http"

	"model-gateway/internal/pkg/dataplane"
)

// routes 把数据面与管理面挂到同一个 mux：
// 数据面是裸 net/http handler（虚拟 Token 自鉴权），管理面自带 admin_token 鉴权与 SPA。
func routes(gateway *dataplane.Handler, management http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", gateway.Healthz)
	mux.HandleFunc("/v1/chat/completions", gateway.ChatCompletions)
	mux.HandleFunc("/v1/models", gateway.ListModels)
	// /admin/ 与 /panel/ 是同一个管理面的等价入口。
	mux.Handle("/panel/", management)
	mux.Handle("/admin/", management)
	mux.Handle("/api/modelgw/", management)
	return mux
}
