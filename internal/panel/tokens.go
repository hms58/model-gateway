package panel

import (
	crand "crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/token"
)

// CodeTokenNotFound 与根 proto 的 ErrTokenNotFound 对齐。
const CodeTokenNotFound = 20609

type tokenView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	KeyPrefix    string   `json:"key_prefix"`
	Enabled      bool     `json:"enabled"`
	Groups       []string `json:"groups"`
	CreatedAt    string   `json:"created_at"`
	RequestCount int64    `json:"request_count"`
	LastUsedAt   string   `json:"last_used_at,omitempty"`
}

func viewOf(record config.Token) tokenView {
	return tokenView{
		ID: record.ID, Name: record.Name, KeyPrefix: record.KeyPrefix,
		Enabled: record.Enabled, Groups: record.Groups, CreatedAt: record.CreatedAt,
	}
}

// withUsage 把运行态里的使用计数并进视图，列表页可直接看「最近使用 / 请求数」。
func (s *Server) withUsage(view tokenView) tokenView {
	usage := s.store.State().TokenSnapshot(view.ID)
	view.RequestCount = usage.RequestCount
	view.LastUsedAt = usage.LastUsedAt
	return view
}

// newTokenID 用「秒级时间 + 4 位随机」避免同秒创建多个 Token 时 id 撞车。
func newTokenID() string {
	var buf [2]byte
	if _, err := crand.Read(buf[:]); err != nil {
		return "tk_" + time.Now().Format("20060102150405")
	}
	return "tk_" + time.Now().Format("20060102150405") + "_" + hex.EncodeToString(buf[:])
}

func (s *Server) tokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.store.Snapshot()
		views := make([]tokenView, 0, len(cfg.Tokens))
		for _, record := range cfg.Tokens {
			views = append(views, s.withUsage(viewOf(record)))
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"version": cfg.Version, "data": views})
	case http.MethodPost:
		var req struct {
			Version int      `json:"version"`
			Name    string   `json:"name"`
			Groups  []string `json:"groups"`
		}
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
			return
		}
		plain, prefix, sha, err := token.Generate()
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInvalidParam, "生成 Token 失败")
			return
		}
		record := config.Token{
			ID:        newTokenID(),
			Name:      req.Name,
			KeyPrefix: prefix,
			KeySHA256: sha,
			Enabled:   true,
			Groups:    req.Groups,
			CreatedAt: time.Now().In(config.Location()).Format(config.TimeLayout),
		}
		cfg := *s.store.Snapshot()
		cfg.Tokens = append(cfg.Tokens, record)
		if err := s.store.Save(cfg, req.Version); err != nil {
			writeSaveError(w, err)
			return
		}
		// 明文只在此响应出现一次；列表与后续接口只返回前缀。
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"plain": plain, "token": viewOf(record), "version": s.store.Snapshot().Version,
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
	}
}

func (s *Server) tokenItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/modelgw/tokens/")
	if id == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "缺少 Token id")
		return
	}

	cfg := *s.store.Snapshot()
	index := -1
	for i := range cfg.Tokens {
		if cfg.Tokens[i].ID == id {
			index = i
		}
	}
	if index < 0 {
		writeError(w, http.StatusNotFound, CodeTokenNotFound, "Token 不存在："+id)
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			Version int      `json:"version"`
			Name    *string  `json:"name"`
			Enabled *bool    `json:"enabled"`
			Groups  []string `json:"groups"`
		}
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
			return
		}
		if req.Name != nil {
			cfg.Tokens[index].Name = *req.Name
		}
		if req.Enabled != nil {
			cfg.Tokens[index].Enabled = *req.Enabled
		}
		if req.Groups != nil {
			cfg.Tokens[index].Groups = req.Groups
		}
		if err := s.store.Save(cfg, req.Version); err != nil {
			writeSaveError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"version": s.store.Snapshot().Version})
	case http.MethodDelete:
		cfg.Tokens = append(cfg.Tokens[:index], cfg.Tokens[index+1:]...)
		if err := s.store.Save(cfg, parseVersion(r)); err != nil {
			writeSaveError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
	}
}
