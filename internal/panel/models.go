package panel

import (
	"net/http"
	"strings"
)

// models 列出各产品的模型（来自落盘的 models.json，缺失/过期由 Fetch 回源），
// 支持按产品过滤与关键字搜索，并带回每个产品的更新时间。
//
// 停用的产品整组不列出：它的模型既不能加进模型组，也不该出现在目录里制造噪音。
// 停用的**模型**仍然列出（否则停用后就再也点不回来），用 disabled 标记交给前端置灰。
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
		if !product.Enabled {
			continue
		}
		if productFilter != "" && product.ID != productFilter {
			continue
		}
		entry := snapshot[product.ID]
		matched := make([]string, 0, len(entry.Models))
		// disabled 只报「目录里真实存在且被停用」的项：配置里残留的旧模型名不进列表，
		// 否则前端会拿一个目录里没有的 id 去渲染行。
		disabled := make([]string, 0, len(product.DisabledModels))
		for _, id := range entry.Models {
			if product.ModelDisabled(id) {
				disabled = append(disabled, id)
			}
			if query != "" && !strings.Contains(strings.ToLower(id), query) {
				continue
			}
			matched = append(matched, id)
		}
		rows = append(rows, map[string]interface{}{
			"id": product.ID, "name": product.Name, "enabled": product.Enabled,
			"models": matched, "disabled": disabled,
			"count": len(entry.Models), "matched": len(matched),
			"enabled_count": len(entry.Models) - len(disabled),
			"updated_at":    entry.UpdatedAt, "failure": entry.Failure,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"version": cfg.Version, "data": rows, "ttl": "10m",
	})
}

// toggleModel 停用 / 启用某个产品下的单个模型。
//
// 约束（与 config.Validate 同源，接口层先给出可读错误）：
//   - 模型必须在该产品的当前目录里 —— 防止拼错的名字被写进停用集，永远匹配不上；
//   - 已被模型组引用的模型不能停用 —— 否则会出现「面板说停用、流量照旧打过去」；
//   - 走配置版本号乐观锁。
func (s *Server) toggleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	var req struct {
		Version int    `json:"version"`
		Product string `json:"product"`
		Model   string `json:"model"`
		Enabled bool   `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}
	req.Product = strings.TrimSpace(req.Product)
	req.Model = strings.TrimSpace(req.Model)
	if req.Product == "" || req.Model == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "product / model 不能为空")
		return
	}

	cfg := s.store.Snapshot().Clone()
	index := -1
	for i := range cfg.Products {
		if cfg.Products[i].ID == req.Product {
			index = i
			break
		}
	}
	if index < 0 {
		writeError(w, http.StatusNotFound, CodeProductNotFound, "产品不存在："+req.Product)
		return
	}

	known := false
	for _, id := range s.catalog.Snapshot()[req.Product].Models {
		if id == req.Model {
			known = true
			break
		}
	}
	if !known {
		writeError(w, http.StatusBadRequest, CodeInvalidParam,
			"模型不在该产品的目录里："+req.Model+"（先刷新一次模型列表再操作）")
		return
	}

	if !req.Enabled {
		used := make([]string, 0, 2)
		for _, group := range cfg.ModelGroups {
			for _, member := range group.Members {
				if member.Product == req.Product && member.Model == req.Model {
					used = append(used, group.Name)
				}
			}
		}
		if len(used) > 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidParam,
				"该模型正被模型组使用："+strings.Join(used, "、")+"；请先从组里移除再停用")
			return
		}
	}

	cfg.Products[index].SetModelDisabled(req.Model, !req.Enabled)
	if err := s.store.Save(cfg, req.Version); err != nil {
		writeSaveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"version":         s.store.Snapshot().Version,
		"disabled_models": cfg.Products[index].DisabledModels,
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
