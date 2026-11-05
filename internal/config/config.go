// Package config 定义网关的业务配置模型、校验与原子写。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

type Product struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Protocol string            `json:"protocol"`
	BaseURL  string            `json:"base_url"`
	APIKey   string            `json:"api_key"`
	Enabled  bool              `json:"enabled"`
	Timeout  Timeout           `json:"timeout"`
	Headers  map[string]string `json:"headers"`
	Note     string            `json:"note"`
}

// Effective 是闭区间，两端留空表示该端不限制。
type Effective struct {
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
	// Probe 控制该成员是否参与「异常后自动探活」；默认关闭，按成员逐个开启。
	Probe     bool      `json:"probe,omitempty"`
	Effective Effective `json:"effective"`
}

type Group struct {
	Name    string   `json:"name"`
	Enabled bool     `json:"enabled"`
	Members []Member `json:"members"`
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
				Base:          "2s",
				Max:           "60s",
				FailThreshold: 3,
				OpenDuration:  "5m",
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
