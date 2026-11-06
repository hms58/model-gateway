package panel

import (
	"net"
	"net/http"
	"strings"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/version"
)

// startupView 是「只有重启才生效」的启动参数。
// 口令只出现在入参里，读取接口永远不回显（连摘要都不返回）。
type startupView struct {
	Listen  string `json:"listen"`
	DataDir string `json:"data_dir"`
	// AdminToken 是明文口令，仅用于写入；空表示「不修改」。
	AdminToken string `json:"admin_token"`
}

// settingsView 是配置页可改的运行参数（产品 / Token / 组另有页面）。
type settingsView struct {
	Startup startupView    `json:"startup"`
	Routing config.Routing `json:"routing"`
	Probe   config.Probe   `json:"probe"`
	Logging config.Logging `json:"logging"`
}

// settings 读写通用配置：选路 / 熔断 / 探活 / 日志即时生效，
// 监听地址、数据目录、管理口令写盘后需重启。
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.store.Snapshot()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"version": cfg.Version,
			"settings": settingsView{
				Startup: startupView{Listen: cfg.Listen, DataDir: cfg.DataDir},
				Routing: cfg.Routing, Probe: cfg.Probe, Logging: cfg.Logging,
			},
			"admin_token_set": strings.TrimSpace(cfg.AdminTokenSHA256) != "",
			// 初始化模式：口令为空且请求来自本机，此时管理面免登录，但只允许本机。
			"admin_bootstrap": s.unset(),
			// 构建版本号：发布包由 ldflags 注入，本地构建为 dev。
			"build_version": version.Version,
			// 只读信息：让面板能说清「哪些项改了要重启」，以及当前进程实际用的值。
			"restart_required": []string{"listen", "data_dir", "admin_token"},
			"active_listen":    s.activeListen,
			"pending_restart":  strings.TrimSpace(cfg.Listen) != strings.TrimSpace(s.activeListen),
		})
	case http.MethodPost:
		var req struct {
			Version  int          `json:"version"`
			Settings settingsView `json:"settings"`
		}
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
			return
		}
		if msg := validateSettings(req.Settings); msg != "" {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, msg)
			return
		}
		cfg := s.store.Snapshot().Clone()
		// 启动参数一律「留空即不修改」：只改选路/探活的调用方不会被顺手清掉监听地址。
		if listen := strings.TrimSpace(req.Settings.Startup.Listen); listen != "" {
			cfg.Listen = listen
		}
		if dir := strings.TrimSpace(req.Settings.Startup.DataDir); dir != "" {
			cfg.DataDir = dir
		}
		if plain := strings.TrimSpace(req.Settings.Startup.AdminToken); plain != "" {
			cfg.AdminToken = ""
			cfg.AdminTokenSHA256 = config.HashAdminToken(plain)
		}
		// 老前端或脚本可能不带这两项：缺省时沿用当前值，避免一次保存把它们顺手清掉。
		cooldown := req.Settings.Routing.Cooldown
		if strings.TrimSpace(cooldown.UnavailableDuration) == "" {
			cooldown.UnavailableDuration = cfg.Routing.Cooldown.UnavailableDuration
		}
		if cooldown.StatusRules == nil {
			cooldown.StatusRules = cfg.Routing.Cooldown.StatusRules
		}
		cfg.Routing = req.Settings.Routing
		cfg.Routing.Cooldown = cooldown
		cfg.Probe = req.Settings.Probe
		cfg.Logging = req.Settings.Logging
		if err := s.store.Save(cfg, req.Version); err != nil {
			writeSaveError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"version": s.store.Snapshot().Version})
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
	}
}

// validateSettings 只做「填错就存不下去」的止损，不做业务推断。
func validateSettings(view settingsView) string {
	if listen := strings.TrimSpace(view.Startup.Listen); listen != "" {
		if msg := validateListen(listen); msg != "" {
			return msg
		}
	}
	if len(strings.TrimSpace(view.Startup.AdminToken)) > 0 && len(strings.TrimSpace(view.Startup.AdminToken)) < 8 {
		return "管理口令至少 8 位"
	}
	if view.Routing.MaxAttempts < 1 {
		return "最大尝试次数必须 ≥ 1"
	}
	for name, raw := range map[string]string{
		"cooldown.base":        view.Routing.Cooldown.Base,
		"cooldown.max":         view.Routing.Cooldown.Max,
		"open_duration":        view.Routing.Cooldown.OpenDuration,
		"unavailable_duration": view.Routing.Cooldown.UnavailableDuration,
		"sticky.ttl":           view.Routing.Sticky.TTL,
		"probe.interval":       view.Probe.Interval,
		"probe.min_gap":        view.Probe.MinGap,
	} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 {
			return name + " 不是合法时长（示例 5s / 2m）"
		}
	}
	if view.Routing.Cooldown.FailThreshold < 0 {
		return "熔断阈值不能为负"
	}
	if msg := validateStatusRules(view.Routing.Cooldown.StatusRules); msg != "" {
		return msg
	}
	if view.Probe.Enabled && strings.TrimSpace(view.Probe.Interval) != "" {
		if value, err := time.ParseDuration(view.Probe.Interval); err == nil && value < 5*time.Second {
			return "探活间隔不能小于 5s（避免打爆下游）"
		}
	}
	if view.Logging.RetentionDays < 1 {
		return "日志保留天数必须 ≥ 1"
	}
	if view.Logging.Dir == "" {
		return "日志目录不能为空"
	}
	return ""
}

// validateStatusRules 校验「按状态码冷却」规则，口径与 config.validateStatusRules 一致，
// 只是错误信息面向面板用户。
func validateStatusRules(rules []config.StatusRule) string {
	seen := make(map[int]struct{}, len(rules))
	for _, rule := range rules {
		if rule.Status < 100 || rule.Status > 599 {
			return "按状态码冷却的状态码必须在 100-599 之间"
		}
		if _, dup := seen[rule.Status]; dup {
			return "按状态码冷却的状态码不能重复"
		}
		seen[rule.Status] = struct{}{}
		if rule.Threshold < 0 {
			return "按状态码冷却的阈值不能为负"
		}
		if rule.Threshold == 0 {
			continue
		}
		if value, err := time.ParseDuration(strings.TrimSpace(rule.Duration)); err != nil || value <= 0 {
			return "按状态码冷却的时长不合法（示例 60s / 10m）"
		}
	}
	return ""
}

// validateListen 用 net.SplitHostPort 提前拦住「端口写成裸数字」这类重启后才发现的问题。
func validateListen(raw string) string {
	_, port, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return "监听地址格式应为 host:port（示例 :8080）"
	}
	if strings.TrimSpace(port) == "" {
		return "监听地址缺少端口（示例 :8080）"
	}
	return ""
}
