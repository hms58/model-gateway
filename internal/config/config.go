// Package config 定义网关的业务配置模型、校验与原子写。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProtocolOpenAIChat 是当前唯一实现的协议；其他值可保存，但请求时按不支持处理。
const ProtocolOpenAIChat = "openai-chat"

// TimeLayout 是生效时间窗的存储格式，统一按 Location() 解释。
const TimeLayout = "2006-01-02 15:04:05"

var (
	ErrVersionConflict     = errors.New("配置版本冲突")
	ErrInvalidWindow       = errors.New("生效时间窗非法")
	ErrInvalidGroup        = errors.New("模型组非法")
	ErrUnsupportedProtocol = errors.New("下游协议不支持")
	// ErrPathRequired 表示没有给出业务配置文件位置。
	ErrPathRequired = errors.New("必须通过 -c 或环境变量 MODELGW_CONFIG 指定业务配置文件")
)

// Windows 容器可能缺 zoneinfo：加载失败时退化为固定 +8，避免时间窗整体偏移。
var shanghai = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}()

// Location 返回时间窗解释时区。
func Location() *time.Location { return shanghai }

type Timeout struct {
	Connect   string `json:"connect" mapstructure:"connect"`
	FirstByte string `json:"first_byte" mapstructure:"first_byte"`
	Total     string `json:"total" mapstructure:"total"`
}

// Proxy 是产品级出网代理；默认关闭（Enabled=false，直连）。
// URL 形如 http://host:port、https://host:port、socks5://host:port、socks5h://host:port，
// 认证信息放在 URL 的 userinfo 里：socks5://user:pass@host:1080。
// socks5h 让代理侧解析目标域名，避免本地 DNS 泄漏；socks5 由本机解析。
type Proxy struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url,omitempty"`
}

// ParseProxyURL 校验并返回可交给 http.ProxyURL 的 URL；未启用时返回 (nil, nil)。
func ParseProxyURL(p Proxy) (*url.URL, error) {
	if !p.Enabled {
		return nil, nil
	}
	raw := strings.TrimSpace(p.URL)
	if raw == "" {
		return nil, errors.New("代理已开启但地址为空")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("代理地址不可解析：%w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q（仅 http/https/socks5/socks5h）", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("代理地址缺少 host:port")
	}
	return parsed, nil
}

// 默认端点路径：绝大多数 OpenAI 兼容上游都是这两个。
const (
	DefaultChatPath   = "/v1/chat/completions"
	DefaultModelsPath = "/v1/models"
)

type Product struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Protocol string            `json:"protocol"`
	BaseURL  string            `json:"base_url"`
	APIKey   string            `json:"api_key"`
	Enabled  bool              `json:"enabled"`
	Timeout  Timeout           `json:"timeout"`
	Headers  map[string]string `json:"headers"`
	// ChatPath / ModelsPath 覆盖默认端点路径，用于非标准 OpenAI 兼容端点：
	// Gemini 兼容层是 /v1beta/openai/chat/completions（没有 /v1 段），
	// Azure 是 /openai/deployments/<d>/chat/completions?api-version=...。
	// 留空 = 用 DefaultChatPath / DefaultModelsPath。允许带查询串。
	ChatPath   string `json:"chat_path,omitempty"`
	ModelsPath string `json:"models_path,omitempty"`
	Proxy      Proxy  `json:"proxy,omitempty"`
	// DisabledModels 是该产品目录里被手工停用的模型 id 原文（按产品隔离、按模型名精确匹配）。
	// 停用只影响「能不能被加进模型组 / 出现在候选里」，不影响已经在用的成员——
	// 停用与在用互斥，Validate 会拒绝两者同时成立。
	// 放在配置里而不是运行态：刷新模型目录时无需迁移，重启、备份、回滚都跟着走。
	DisabledModels []string `json:"disabled_models,omitempty"`
	Note           string   `json:"note"`
}

// ModelDisabled 判断某个模型 id 是否被停用。精确匹配（区分 cn: / global: 前缀）：
// 同名模型在不同产品下、同产品不同域下都是独立开关。
func (p Product) ModelDisabled(model string) bool {
	for _, id := range p.DisabledModels {
		if id == model {
			return true
		}
	}
	return false
}

// SetModelDisabled 设置停用状态；重复设置幂等，取消到空时把切片置 nil，
// 避免配置文件里留下一个空数组。
func (p *Product) SetModelDisabled(model string, disabled bool) {
	if disabled {
		if !p.ModelDisabled(model) {
			p.DisabledModels = append(p.DisabledModels, model)
		}
		return
	}
	kept := make([]string, 0, len(p.DisabledModels))
	for _, id := range p.DisabledModels {
		if id != model {
			kept = append(kept, id)
		}
	}
	if len(kept) == 0 {
		p.DisabledModels = nil
		return
	}
	p.DisabledModels = kept
}

// ChatEndpoint 返回聊天补全的完整 URL。
func (p Product) ChatEndpoint() string {
	return joinEndpoint(p.BaseURL, p.ChatPath, DefaultChatPath)
}

// ModelsEndpoint 返回模型列表的完整 URL。
func (p Product) ModelsEndpoint() string {
	return joinEndpoint(p.BaseURL, p.ModelsPath, DefaultModelsPath)
}

// joinEndpoint 把 base_url 与路径拼成完整 URL。
// 直接字符串拼接而不是 url.JoinPath：后者会把查询串当成路径的一部分转义掉，
// 而 Azure 的 api-version 必须留在查询串里。
func joinEndpoint(baseURL, path, fallback string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.TrimSpace(path) == "" {
		path = fallback
	}
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// ValidateEndpointPath 校验自定义端点路径：必须是站内路径，不能是完整 URL。
// 否则「base_url + path」会拼出 https://a/https://b 这种不可请求的地址。
func ValidateEndpointPath(field, path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil
	}
	if strings.Contains(trimmed, "://") {
		return fmt.Errorf("%s 只能是站内路径（如 /v1/chat/completions），不能是完整 URL", field)
	}
	if !strings.HasPrefix(trimmed, "/") {
		return fmt.Errorf("%s 必须以 / 开头", field)
	}
	return nil
}

// Effective 是闭区间，两端留空表示该端不限制。
type Effective struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// DailyWindow 是每天重复的生效时段（如 23:00-07:00），闭区间。
// 起点晚于终点表示跨天：当天 start 起、次日 end 止；起点早于终点表示同天。
// 两端留空表示不限制；填就必须两端都填且不相同。
// 与 Effective（带日期的绝对区间）同时开启时，两者都命中才生效。
type DailyWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type Member struct {
	Product string `json:"product"`
	Model   string `json:"model"`
	// Priority 已废弃：权重即优先级（weight 越大越先选，同权重按列表顺序）。
	// 保留字段只为兼容存量配置，选路与面板都不再使用。
	Priority int  `json:"priority,omitempty"`
	Weight   int  `json:"weight"`
	Enabled  bool `json:"enabled"`
	// Probe 已废弃：异步嗅探改为按模型组开关（Group.Probe）。
	// 保留字段只为兼容存量配置，探活判定只读 Group.Probe。
	Probe     bool        `json:"probe,omitempty"`
	Effective Effective   `json:"effective"`
	Daily     DailyWindow `json:"daily"`
}

type Group struct {
	Name    string   `json:"name"`
	Enabled bool     `json:"enabled"`
	Members []Member `json:"members"`
	// Probe 控制本组是否参与「异常后自动探活」：开启后组内所有成员都参与，
	// 关闭则全不参与。按组开关比按成员开关少一堆配置，也不会出现同组内一半开一半关。
	Probe bool `json:"probe,omitempty"`
}

type Token struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	KeyPrefix string   `json:"key_prefix"`
	KeySHA256 string   `json:"key_sha256"`
	Enabled   bool     `json:"enabled"`
	Groups    []string `json:"groups"`
	CreatedAt string   `json:"created_at"`
}

type Cooldown struct {
	Base          string `json:"base"`
	Max           string `json:"max"`
	FailThreshold int    `json:"fail_threshold"`
	OpenDuration  string `json:"open_duration"`
	// UnavailableDuration 是「上游明确不可用」类 400 的冷却时长：
	// 额度耗尽、地区不支持、凭据无效这些错误重试同一成员没有意义，
	// 摘掉一段时间比每个请求都白撞一次更划算。留空按 10m 处理。
	UnavailableDuration string `json:"unavailable_duration"`
	// StatusRules 按状态码配置「连续失败达到阈值就冷却多久」，
	// 用于覆盖通用指数退避（例如 400 连续 5 次说明协议/参数系统性不兼容）。
	// 留空时使用内置规则：400 / 422 连续 5 次 → 60s。
	StatusRules []StatusRule `json:"status_rules,omitempty"`
}

// StatusRule 是单条「状态码 → 阈值 → 冷却时长」规则。
// 命中规则的状态码走独立计数，不再叠加 fail_streak；threshold <= 0 表示禁用该规则。
type StatusRule struct {
	Status    int    `json:"status"`
	Threshold int    `json:"threshold"`
	Duration  string `json:"duration"`
}

// DefaultStatusRules 是未配置 status_rules 时的内置兜底：
// 400 / 422 连续 5 次说明不是「个别请求不兼容」，而是协议或参数系统性变了。
func DefaultStatusRules() []StatusRule {
	return []StatusRule{
		{Status: 400, Threshold: 5, Duration: "60s"},
		{Status: 422, Threshold: 5, Duration: "60s"},
	}
}

// RuleFor 返回该状态码对应的规则；显式配置优先，未配置时回落到内置规则。
// 第二个返回值表示是否命中（threshold <= 0 的规则视为未命中）。
func (c Cooldown) RuleFor(status int) (StatusRule, bool) {
	rules := c.StatusRules
	if len(rules) == 0 {
		rules = DefaultStatusRules()
	}
	for _, rule := range rules {
		if rule.Status == status && rule.Threshold > 0 {
			return rule, true
		}
	}
	return StatusRule{}, false
}

// RuleDuration 返回规则冷却时长，非法或留空时回落到 fallback。
func (r StatusRule) RuleDuration(fallback time.Duration) time.Duration {
	parsed, err := time.ParseDuration(strings.TrimSpace(r.Duration))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

type Sticky struct {
	Enabled bool   `json:"enabled"`
	TTL     string `json:"ttl"`
}

type Routing struct {
	MaxAttempts  int      `json:"max_attempts"`
	Cooldown     Cooldown `json:"cooldown"`
	PreferLastOK bool     `json:"prefer_last_ok"`
	Sticky       Sticky   `json:"sticky"`
}

type Logging struct {
	Dir           string `json:"dir"`
	RetentionDays int    `json:"retention_days"`
	MaxFileMB     int    `json:"max_file_mb"`
}

type Config struct {
	Version int    `json:"version"`
	Listen  string `json:"listen"`
	// AdminToken 只为识别存量明文配置：一旦非空即校验失败，不允许写盘。
	AdminToken string `json:"admin_token,omitempty"`
	// AdminTokenSHA256 是管理面口令的 SHA-256（64 位小写十六进制），明文永不落盘。
	AdminTokenSHA256 string    `json:"admin_token_sha256,omitempty"`
	DataDir          string    `json:"data_dir"`
	Routing          Routing   `json:"routing"`
	Probe            Probe     `json:"probe"`
	Logging          Logging   `json:"logging"`
	Products         []Product `json:"products"`
	ModelGroups      []Group   `json:"model_groups"`
	Tokens           []Token   `json:"tokens"`
}

// Probe 是「异常成员后台探活」设置：探到就恢复，避免低权重成员永远等不到真实流量验证。
type Probe struct {
	Enabled  bool   `json:"enabled"`
	Interval string `json:"interval"` // 扫描间隔
	MinGap   string `json:"min_gap"`  // 同一产品两次探活的最小间隔
}

// Default 返回可直接落盘的默认配置（数据面与面板的默认值都集中在这里）。
// AdminTokenSHA256 留空：默认不带任何凭据，管理面在配置页设好口令前不可用。
func Default() Config {
	return Config{
		Listen:  ":8080",
		DataDir: "./data",
		Probe:   Probe{Enabled: true, Interval: "60s", MinGap: "30s"},
		Routing: Routing{
			MaxAttempts: 4,
			Cooldown: Cooldown{
				Base:                "2s",
				Max:                 "60s",
				FailThreshold:       3,
				OpenDuration:        "5m",
				UnavailableDuration: "10m",
			},
			PreferLastOK: true,
			Sticky:       Sticky{Enabled: true, TTL: "30m"},
		},
		Logging: Logging{Dir: "./logs", RetentionDays: 14, MaxFileMB: 64},
	}
}

// ParseTime 解析时间窗字段；空串表示该端不限制。
func ParseTime(raw string) (time.Time, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.ParseInLocation(TimeLayout, raw, shanghai)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("时间格式应为 %s：%w", TimeLayout, err)
	}
	return parsed, true, nil
}

// clockLayouts 是每日时段的两种可写格式，秒可省略。
var clockLayouts = []string{"15:04:05", "15:04"}

// ParseClock 把每日时段端点解析成「当天 0 点起的秒数」；空串表示该端不限制。
// 只比时刻不比日期，所以跨天区间不必带上「次日」这种额外信息。
func ParseClock(raw string) (int, bool, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, false, nil
	}
	for _, layout := range clockLayouts {
		parsed, err := time.ParseInLocation(layout, text, shanghai)
		if err == nil {
			return parsed.Hour()*3600 + parsed.Minute()*60 + parsed.Second(), true, nil
		}
	}
	return 0, false, fmt.Errorf("时间格式应为 HH:MM 或 HH:MM:SS：%q", raw)
}

// validateDaily 校验每日时段：两端都留空表示不限制；要填就都填，且起止不能相同。
func validateDaily(window DailyWindow) error {
	start, hasStart, err := ParseClock(window.Start)
	if err != nil {
		return err
	}
	end, hasEnd, err := ParseClock(window.End)
	if err != nil {
		return err
	}
	switch {
	case !hasStart && !hasEnd:
		return nil
	case hasStart != hasEnd:
		return errors.New("起止必须同时填写或同时留空")
	case start == end:
		return errors.New("起止不能相同（全天生效请留空）")
	}
	return nil
}

func memberKey(group, product, model string) string {
	return group + "|" + product + "|" + model
}

// MemberKey 是运行态与日志引用成员的唯一键。
func MemberKey(group string, member Member) string {
	return memberKey(group, member.Product, member.Model)
}

// Load 读取配置文件；文件不存在按 os.ErrNotExist 返回。
// Windows 编辑器常把 JSON 存成带 BOM 的 UTF-8，这里先剥掉 BOM 再解析。
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("配置不是合法 JSON：%w", err)
	}
	if err := Validate(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Clone 深拷贝配置。
//
// 管理面「读快照 → 改 → 保存」必须先 Clone：Snapshot() 返回的是共享底层数组的
// 指针，`cfg := *snap` 只复制 slice header，`cfg.Products[i] = x` 会直接写进
// 正在运行的快照。一旦保存被校验拒绝，磁盘没变、内存却被改坏了——运行中的路由
// 立刻用了非法配置，重启后又"自己恢复"，极难排查。
func (c Config) Clone() Config {
	out := c
	out.Products = make([]Product, len(c.Products))
	for i, product := range c.Products {
		if product.Headers != nil {
			headers := make(map[string]string, len(product.Headers))
			for key, value := range product.Headers {
				headers[key] = value
			}
			product.Headers = headers
		}
		// DisabledModels 是可变切片：不复制的话 append 可能写进正在运行的快照。
		if product.DisabledModels != nil {
			product.DisabledModels = append([]string(nil), product.DisabledModels...)
		}
		out.Products[i] = product
	}
	out.ModelGroups = make([]Group, len(c.ModelGroups))
	for i, group := range c.ModelGroups {
		group.Members = append([]Member(nil), group.Members...)
		out.ModelGroups[i] = group
	}
	out.Tokens = make([]Token, len(c.Tokens))
	for i, token := range c.Tokens {
		token.Groups = append([]string(nil), token.Groups...)
		out.Tokens[i] = token
	}
	return out
}

// Validate 覆盖保存前的全量校验：任何一项不过都不落盘。
func Validate(cfg *Config) error {
	if cfg == nil {
		return errors.New("配置为空")
	}
	if strings.TrimSpace(cfg.Listen) == "" {
		return errors.New("listen 不能为空")
	}
	// 明文口令一旦落盘就等于泄漏：只接受 SHA-256，且必须是 64 位十六进制。
	if strings.TrimSpace(cfg.AdminToken) != "" {
		return ErrPlainAdminToken
	}
	if raw := strings.TrimSpace(cfg.AdminTokenSHA256); raw != "" && !validSHA256(raw) {
		return errors.New("admin_token_sha256 必须是 64 位十六进制 SHA-256")
	}
	if cfg.Routing.MaxAttempts < 1 {
		return errors.New("routing.max_attempts 必须 >= 1")
	}
	if err := validateStatusRules(cfg.Routing.Cooldown.StatusRules); err != nil {
		return err
	}

	products := make(map[string]Product, len(cfg.Products))
	for _, p := range cfg.Products {
		if strings.TrimSpace(p.ID) == "" {
			return errors.New("产品 id 不能为空")
		}
		if _, dup := products[p.ID]; dup {
			return fmt.Errorf("产品 id 重复：%s", p.ID)
		}
		// protocol 允许保存任何值（便于先登记后适配），但请求期只放行 openai-chat，
		// 其他值由数据面返回 20608；这样面板不会因为填错协议而整份配置存不下去。
		if p.Enabled && strings.TrimSpace(p.BaseURL) == "" {
			return fmt.Errorf("产品 %s 已启用但 base_url 为空", p.ID)
		}
		// 代理地址填错会等到真实请求才炸，且症状像上游故障；保存时就拦住。
		if _, err := ParseProxyURL(p.Proxy); err != nil {
			return fmt.Errorf("产品 %s 的代理配置非法：%w", p.ID, err)
		}
		// 端点路径同理：拼错只会在真实请求时 404，且看起来像上游挂了。
		if err := ValidateEndpointPath("chat_path", p.ChatPath); err != nil {
			return fmt.Errorf("产品 %s：%w", p.ID, err)
		}
		if err := ValidateEndpointPath("models_path", p.ModelsPath); err != nil {
			return fmt.Errorf("产品 %s：%w", p.ID, err)
		}
		products[p.ID] = p
	}

	groups := make(map[string]struct{}, len(cfg.ModelGroups))
	for _, g := range cfg.ModelGroups {
		name := strings.TrimSpace(g.Name)
		if name == "" {
			return errors.New("模型组名称不能为空")
		}
		if _, dup := groups[name]; dup {
			return fmt.Errorf("%w：组名重复 %s", ErrInvalidGroup, name)
		}
		groups[name] = struct{}{}

		seen := make(map[string]struct{}, len(g.Members))
		for _, m := range g.Members {
			if strings.TrimSpace(m.Model) == "" {
				return fmt.Errorf("%w：组 %s 存在空模型名", ErrInvalidGroup, name)
			}
			if _, ok := products[m.Product]; !ok {
				return fmt.Errorf("%w：组 %s 引用了不存在的产品 %s", ErrInvalidGroup, name, m.Product)
			}
			if m.Weight <= 0 {
				return fmt.Errorf("%w：组 %s 的成员 %s 权重必须为正", ErrInvalidGroup, name, m.Model)
			}
			key := memberKey(name, m.Product, m.Model)
			if _, dup := seen[key]; dup {
				return fmt.Errorf("%w：组 %s 成员重复 %s|%s", ErrInvalidGroup, name, m.Product, m.Model)
			}
			seen[key] = struct{}{}

			start, hasStart, err := ParseTime(m.Effective.Start)
			if err != nil {
				return fmt.Errorf("%w：%v", ErrInvalidWindow, err)
			}
			end, hasEnd, err := ParseTime(m.Effective.End)
			if err != nil {
				return fmt.Errorf("%w：%v", ErrInvalidWindow, err)
			}
			if hasStart && hasEnd && start.After(end) {
				return fmt.Errorf("%w：组 %s 成员 %s 的 start 晚于 end", ErrInvalidWindow, name, m.Model)
			}
			if err := validateDaily(m.Daily); err != nil {
				return fmt.Errorf("%w：组 %s 成员 %s 的每日时段非法：%v", ErrInvalidWindow, name, m.Model, err)
			}
		}
	}

	// 停用与「在用」互斥：停用集里的 (产品, 模型) 不能出现在任何模型组里。
	// 否则面板显示「已停用」，流量却照旧打过去，症状是「停用了还在花钱」这种极难排查的分裂状态。
	// 放在 Validate 而不是只在接口里拦：手改配置文件、以及「先停用、后有人加进组」的并发窗口都要兜住。
	for _, g := range cfg.ModelGroups {
		for _, m := range g.Members {
			if product, ok := products[m.Product]; ok && product.ModelDisabled(m.Model) {
				return fmt.Errorf("%w：组 %s 的成员 %s|%s 已被停用，请先启用该模型或移除该成员",
					ErrInvalidGroup, g.Name, m.Product, m.Model)
			}
		}
	}

	for _, t := range cfg.Tokens {
		if strings.TrimSpace(t.ID) == "" {
			return errors.New("Token id 不能为空")
		}
		for _, g := range t.Groups {
			if g == "*" {
				continue
			}
			if _, ok := groups[g]; !ok {
				return fmt.Errorf("Token %s 引用了不存在的模型组 %s", t.ID, g)
			}
		}
	}
	return nil
}

// validateStatusRules 校验按状态码的冷却规则：状态码合法、阈值非负、时长可解析、同一状态码不重复。
func validateStatusRules(rules []StatusRule) error {
	seen := make(map[int]struct{}, len(rules))
	for _, rule := range rules {
		if rule.Status < 100 || rule.Status > 599 {
			return fmt.Errorf("status_rules 的状态码非法：%d", rule.Status)
		}
		if _, dup := seen[rule.Status]; dup {
			return fmt.Errorf("status_rules 状态码重复：%d", rule.Status)
		}
		seen[rule.Status] = struct{}{}
		if rule.Threshold < 0 {
			return fmt.Errorf("status_rules 状态 %d 的 threshold 不能为负", rule.Status)
		}
		if rule.Threshold == 0 {
			continue
		}
		if parsed, err := time.ParseDuration(strings.TrimSpace(rule.Duration)); err != nil || parsed <= 0 {
			return fmt.Errorf("status_rules 状态 %d 的 duration 非法：%q", rule.Status, rule.Duration)
		}
	}
	return nil
}

// SaveAtomic 写 `.tmp` → 保留 `.bak` → rename，保证磁盘上不出现半文件。
func SaveAtomic(path string, cfg *Config) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", old, 0o644)
	}
	return os.Rename(tmp, path)
}

// SaveWithVersion 按乐观锁保存：磁盘版本与 expect 不一致时返回 ErrVersionConflict。
func SaveWithVersion(path string, cfg *Config, expect int) error {
	current, err := Load(path)
	switch {
	case err == nil:
		if current.Version != expect {
			return ErrVersionConflict
		}
	case errors.Is(err, os.ErrNotExist):
		if expect != 0 {
			return ErrVersionConflict
		}
	default:
		return err
	}
	cfg.Version = expect + 1
	return SaveAtomic(path, cfg)
}
