package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func validConfig() Config {
	cfg := Default()
	cfg.Products = []Product{{
		ID: "workbuddy", Name: "WorkBuddy", Protocol: ProtocolOpenAIChat,
		BaseURL: "http://127.0.0.1:7863", APIKey: "sk-test", Enabled: true,
	}}
	cfg.ModelGroups = []Group{{
		Name:    "gpt-5",
		Enabled: true,
		Members: []Member{{
			Product: "workbuddy", Model: "gpt-5", Priority: 0, Weight: 100, Enabled: true,
			Effective: Effective{Start: "2026-11-01 00:00:00", End: "2026-12-01 23:59:59"},
		}},
	}}
	return cfg
}

func TestValidateAcceptsDefaultPlusOneGroup(t *testing.T) {
	cfg := validConfig()
	if err := Validate(&cfg); err != nil {
		t.Fatalf("合法配置被拒：%v", err)
	}
}

func TestValidateRejectsInvertedWindow(t *testing.T) {
	cfg := validConfig()
	cfg.ModelGroups[0].Members[0].Effective = Effective{
		Start: "2026-12-01 00:00:00", End: "2026-11-01 00:00:00",
	}
	if err := Validate(&cfg); !errors.Is(err, ErrInvalidWindow) {
		t.Fatalf("期望 ErrInvalidWindow，实际 %v", err)
	}
}

// 每日时段允许跨天（23:00-07:00）与同天（09:00-18:00），秒可省略。
func TestValidateAcceptsDailyWindow(t *testing.T) {
	tests := []struct {
		name   string
		window DailyWindow
	}{
		{"跨天", DailyWindow{Start: "23:00", End: "07:00"}},
		{"同天", DailyWindow{Start: "09:00", End: "18:00"}},
		{"带秒", DailyWindow{Start: "23:00:30", End: "07:00:15"}},
		{"两端留空", DailyWindow{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ModelGroups[0].Members[0].Daily = tc.window
			if err := Validate(&cfg); err != nil {
				t.Fatalf("合法每日时段被拒：%v", err)
			}
		})
	}
}

func TestValidateRejectsInvalidDailyWindow(t *testing.T) {
	tests := []struct {
		name   string
		window DailyWindow
	}{
		{"只填起点", DailyWindow{Start: "23:00"}},
		{"只填终点", DailyWindow{End: "07:00"}},
		{"起止相同", DailyWindow{Start: "23:00", End: "23:00"}},
		{"格式非法", DailyWindow{Start: "23点", End: "07:00"}},
		{"越界时刻", DailyWindow{Start: "24:00", End: "07:00"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ModelGroups[0].Members[0].Daily = tc.window
			if err := Validate(&cfg); !errors.Is(err, ErrInvalidWindow) {
				t.Fatalf("期望 ErrInvalidWindow，实际 %v", err)
			}
		})
	}
}

// 成员重复不再拦截：同模型可以配多条（通常用每日时段区分用途），
// 完全相同的两行也放行——运行态共享，不会放大故障。
func TestValidateAllowsDuplicateMember(t *testing.T) {
	cfg := validConfig()
	cfg.ModelGroups[0].Members = append(cfg.ModelGroups[0].Members, cfg.ModelGroups[0].Members[0])
	if err := Validate(&cfg); err != nil {
		t.Fatalf("重复成员应放行：%v", err)
	}
}

// 同一模型可以按时段重复配置：白天一条、夜间一条，只要时段不同就允许。
func TestValidateAllowsSameModelWithDifferentWindows(t *testing.T) {
	cfg := validConfig()
	first := cfg.ModelGroups[0].Members[0]
	first.Daily = DailyWindow{Start: "09:00", End: "18:00"}
	second := first
	second.Weight = 50
	second.Daily = DailyWindow{Start: "18:00", End: "09:00"}
	cfg.ModelGroups[0].Members = []Member{first, second}
	if err := Validate(&cfg); err != nil {
		t.Fatalf("同模型不同每日时段应允许：%v", err)
	}

	// 连时段都一样的第三行也放行：成员重复不再做拦截。
	third := second
	third.Weight = 10
	cfg.ModelGroups[0].Members = []Member{first, second, third}
	if err := Validate(&cfg); err != nil {
		t.Fatalf("完全相同的成员也应放行，实际 %v", err)
	}
}

// 产品权重只用于面板排序：0 表示未设置，负值会让排序方向反过来，保存时就拒掉。
func TestValidateRejectsNegativeProductWeight(t *testing.T) {
	cfg := validConfig()
	cfg.Products[0].Weight = -1
	if err := Validate(&cfg); err == nil {
		t.Fatal("负权重应被拒")
	}

	cfg.Products[0].Weight = 0
	if err := Validate(&cfg); err != nil {
		t.Fatalf("0 权重表示未设置，应允许：%v", err)
	}
}

// 协议留待请求期判定：先允许登记，避免填错协议导致整份配置存不下去。
func TestValidateAllowsUnknownProtocol(t *testing.T) {
	cfg := validConfig()
	cfg.Products[0].Protocol = "anthropic"
	if err := Validate(&cfg); err != nil {
		t.Fatalf("保存阶段不应拒绝未知协议：%v", err)
	}
}

func TestValidateRejectsUnknownTokenGroup(t *testing.T) {
	cfg := validConfig()
	cfg.Tokens = []Token{{ID: "tk_01", Enabled: true, Groups: []string{"not-exist"}}}
	if err := Validate(&cfg); err == nil {
		t.Fatal("引用不存在的组应当校验失败")
	}
}

func TestSaveWithVersionConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modelgw.json")
	cfg := validConfig()
	if err := SaveWithVersion(path, &cfg, 0); err != nil {
		t.Fatalf("首次保存失败：%v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if loaded.Version != 1 {
		t.Fatalf("期望版本 1，实际 %d", loaded.Version)
	}

	stale := validConfig()
	if err := SaveWithVersion(path, &stale, 0); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("期望版本冲突，实际 %v", err)
	}

	next := validConfig()
	next.Listen = ":15110"
	if err := SaveWithVersion(path, &next, loaded.Version); err != nil {
		t.Fatalf("正确版本应能保存：%v", err)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("覆盖写应保留 .bak：%v", err)
	}
}

// 磁盘上的旧配置即使已经不合法（手改坏 weight、残留明文口令段），
// 也必须能用正确的版本号覆盖保存：版本检查只读 version 字段。
// 复用 Load 做检查会让「配置被改坏」直接变成「面板永久无法保存」，只能去改文件。
func TestSaveWithVersionToleratesInvalidExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modelgw.json")
	broken := `{"version":7,"listen":":15100","products":[{"id":"p","base_url":"http://x","weight":0}]}`
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatalf("准备坏配置失败：%v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("前提不成立：这份配置应当校验失败")
	}
	version, err := ReadVersion(path)
	if err != nil || version != 7 {
		t.Fatalf("读版本应只解析 version 字段：%d %v", version, err)
	}

	cfg := validConfig()
	if err := SaveWithVersion(path, &cfg, 6); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本不符仍应拒绝，实际 %v", err)
	}
	if err := SaveWithVersion(path, &cfg, 7); err != nil {
		t.Fatalf("版本相符应能保存并把配置修回来：%v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("保存后应是合法配置：%v", err)
	}
}

// Windows 编辑器常写带 BOM 的 UTF-8；配置读取必须容忍 BOM，否则会报「不是合法 JSON」。
func TestLoadToleratesUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modelgw.json")
	raw, err := json.Marshal(validConfig())
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if err := os.WriteFile(path, append([]byte{0xEF, 0xBB, 0xBF}, raw...), 0o644); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("带 BOM 的配置应能加载：%v", err)
	}
}

func TestParseTimeEmptyMeansUnbounded(t *testing.T) {
	_, has, err := ParseTime("")
	if err != nil || has {
		t.Fatalf("空串应当表示不限制：has=%v err=%v", has, err)
	}
	if Location() == nil {
		t.Fatal("时区不应为空")
	}
}
