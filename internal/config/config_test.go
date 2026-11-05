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

func TestValidateRejectsDuplicateMember(t *testing.T) {
	cfg := validConfig()
	cfg.ModelGroups[0].Members = append(cfg.ModelGroups[0].Members, cfg.ModelGroups[0].Members[0])
	if err := Validate(&cfg); !errors.Is(err, ErrInvalidGroup) {
		t.Fatalf("期望 ErrInvalidGroup，实际 %v", err)
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
