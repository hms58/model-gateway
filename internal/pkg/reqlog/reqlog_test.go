package reqlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWritePersistsJsonlAndRing(t *testing.T) {
	dir := t.TempDir()
	logger := New(dir, 14)
	defer logger.Close()

	logger.Write(Entry{Group: "gpt-5", Product: "workbuddy", Outcome: "ok", HTTPStatus: 200})
	logger.Write(Entry{Group: "gpt-5", Product: "trae", Outcome: "quota", HTTPStatus: 429})
	logger.Flush()

	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("应生成一个按天文件：%v %v", files, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil {
		t.Fatalf("读日志失败：%v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("期望 2 行，实际 %d：%s", len(lines), raw)
	}

	recent := logger.Recent(1)
	if len(recent) != 1 || recent[0].Product != "trae" {
		t.Fatalf("ring 应返回最新一条：%+v", recent)
	}
}

func TestDroppedWhenQueueFull(t *testing.T) {
	logger := newLogger(t.TempDir(), 14, 1, false) // 不启动写协程，队列必然打满
	logger.Write(Entry{Group: "a"})
	logger.Write(Entry{Group: "b"})
	if got := logger.Dropped(); got != 1 {
		t.Fatalf("队列满应丢弃 1 条，实际 %d", got)
	}
}

func TestCleanupRemovesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "requests-20200101.jsonl")
	recent := filepath.Join(dir, "requests-"+time.Now().Format("20060102")+".jsonl")
	for _, path := range []string{old, recent} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("准备文件失败：%v", err)
		}
	}

	logger := New(dir, 7)
	defer logger.Close()
	if err := logger.Cleanup(); err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("过期文件应被删除")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("近期文件应保留：%v", err)
	}
}
