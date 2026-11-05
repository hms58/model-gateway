package panel

import (
	"net/http"
	"strings"
)

// models 列出各产品的模型（来自落盘的 models.json，缺失/过期由 Fetch 回源），
// 支持按产品过滤与关键字搜索，并带回每个产品的更新时间。
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	cfg := s.store.Snapshot()
	snapshot := s.catalog.Snapshot()
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	productFilter := strings.TrimSpace(r.URL.Query().Get("product"))

	rows := make([]map[string]interface{}, 0, len(cfg.Products))
	for _, product := range cfg.Products {
		if productFilter != "" && product.ID != productFilter {
			continue
		}
		entry := snapshot[product.ID]
		matched := make([]string, 0, len(entry.Models))
		for _, id := range entry.Models {
			if query != "" && !strings.Contains(strings.ToLower(id), query) {
				continue
			}
			matched = append(matched, id)
		}
		rows = append(rows, map[string]interface{}{
			"id": product.ID, "name": product.Name, "enabled": product.Enabled,
			"models": matched, "count": len(entry.Models), "matched": len(matched),
			"updated_at": entry.UpdatedAt, "failure": entry.Failure,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"version": cfg.Version, "data": rows, "ttl": "10m",
	})
}

// refreshModels 强制回源并写回 models.json；product 为空表示刷新所有启用产品。
func (s *Server) refreshModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	var req struct {
		Product string `json:"product"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
			return
		}
	}

	cfg := s.store.Snapshot()
	updated := 0
	failed := make([]string, 0)
	for _, product := range cfg.Products {
		if !product.Enabled {
			continue
		}
		if req.Product != "" && product.ID != req.Product {
			continue
		}
		if _, err := s.catalog.Fetch(r.Context(), product, true); err != nil {
			failed = append(failed, product.ID)
			continue
		}
		updated++
	}
	if req.Product != "" && updated == 0 && len(failed) > 0 {
		writeError(w, http.StatusBadGateway, CodeFetchModels, "拉取模型失败："+failed[0])
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"updated": updated, "failed": failed, "data": s.catalog.Snapshot(),
	})
}
