package store

import (
	"errors"
	"path/filepath"
	"testing"

	"model-gateway/internal/config"
)

func writeConfig(t *testing.T, dir string) string {
	t.Helper()
	cfg := config.Default()
	cfg.Products = []config.Product{{ID: "workbuddy", Enabled: true, BaseURL: "http://127.0.0.1:7863"}}
	cfg.ModelGroups = []config.Group{{
		Name: "gpt-5", Enabled: true,
		Members: []config.Member{{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true}},
	}}
	path := filepath.Join(dir, "modelgw.json")
	if err := config.SaveAtomic(path, &cfg); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}
	return path
}

func TestOpenLoadsSnapshot(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(writeConfig(t, dir), filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	if got := st.Snapshot(); got == nil || len(got.ModelGroups) != 1 || got.ModelGroups[0].Name != "gpt-5" {
		t.Fatalf("快照内容不正确：%+v", got)
	}
}

func TestSaveRejectsStaleVersion(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir)
	st, err := Open(path, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}

	next := *st.Snapshot()
	if err := st.Save(next, 99); !errors.Is(err, config.ErrVersionConflict) {
		t.Fatalf("期望版本冲突，实际 %v", err)
	}
	if err := st.Save(next, next.Version); err != nil {
		t.Fatalf("正确版本应能保存：%v", err)
	}
}

func TestStateRoundTripAfterRestart(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeConfig(t, dir)
	statePath := filepath.Join(dir, "state.json")

	st, err := Open(cfgPath, statePath)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	key := config.MemberKey("gpt-5", config.Member{Product: "workbuddy", Model: "gpt-5"})
	st.State().Failure(key, st.Snapshot().Routing.Cooldown)
	st.MarkDirty()
	if err := st.FlushIfDirty(); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}

	reopened, err := Open(cfgPath, statePath)
	if err != nil {
		t.Fatalf("重新打开失败：%v", err)
	}
	if reopened.State().Available(key) {
		t.Fatal("冷却状态未从 state.json 恢复")
	}
}

func TestFlushIfDirtyNoopsWhenClean(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(writeConfig(t, dir), filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	if err := st.FlushIfDirty(); err != nil {
		t.Fatalf("无改动时不应报错：%v", err)
	}
}
