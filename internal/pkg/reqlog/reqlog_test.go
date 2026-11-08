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
	for _, line := range lines {
		if !strings.HasPrefix(line, `{"time":"`) {
			t.Fatalf("每条都应有 time 字段：%s", line)
		}
	}

	recent := logger.Recent(1)
	if len(recent) != 1 || recent[0].Product != "trae" {
		t.Fatalf("ring 应返回最新一条：%+v", recent)
	}
}

func TestDroppedWhenQueueFull(t *testing.T) {
	logger := newLogger(t.TempDir(), 14, 0, 1, false) // 不启动写协程，队列必然打满
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
	oldPart := filepath.Join(dir, "requests-20200101.3.jsonl")
	for _, path := range []string{old, recent, oldPart} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("准备文件失败：%v", err)
		}
	}
	// 非本包命名规则的文件不参与清理，避免误删用户文件。
	foreign := filepath.Join(dir, "requests-20200101.txt")
	if err := os.WriteFile(foreign, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备文件失败：%v", err)
	}

	logger := New(dir, 7)
	defer logger.Close()
	if err := logger.Cleanup(); err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	for _, path := range []string{old, oldPart} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("过期文件应被删除：%s", path)
		}
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("近期文件应保留：%v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("非本包文件不应被删除：%v", err)
	}
}

// max_file_mb 必须真的生效：单个文件写满后滚动到同日下一个分片，
// 否则配置项只是「写进示例 JSON 的承诺」。
func TestRotatesWhenFileExceedsLimit(t *testing.T) {
	dir := t.TempDir()
	// 每条约 90 字节，上限 200 字节 → 每个分片放 2 条。
	logger := newLogger(dir, 7, 200, 16, true)
	defer logger.Close()

	for i := 0; i < 6; i++ {
		logger.Write(Entry{Group: "gpt-5", Product: "workbuddy", Outcome: "ok", HTTPStatus: 200})
	}
	logger.Flush()

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败：%v", err)
	}
	if len(files) < 2 {
		t.Fatalf("超过大小上限应滚动出多个分片，实际 %d 个", len(files))
	}
	total := 0
	for _, item := range files {
		raw, err := os.ReadFile(filepath.Join(dir, item.Name()))
		if err != nil {
			t.Fatalf("读分片失败：%v", err)
		}
		if _, ok := DayOfFile(item.Name()); !ok {
			t.Fatalf("分片命名应可被 DayOfFile 解析：%s", item.Name())
		}
		total += strings.Count(string(raw), "\n")
	}
	if total != 6 {
		t.Fatalf("滚动不能丢日志，期望 6 条，实际 %d", total)
	}
}

// 分片文件名带 .N，按天清理与日志查询都必须能认出来。
func TestDayOfFileParsesRolledNames(t *testing.T) {
	tests := []struct {
		name string
		want string
		ok   bool
	}{
		{"requests-20261108.jsonl", "20261108", true},
		{"requests-20261108.1.jsonl", "20261108", true},
		{"requests-20261108.12.jsonl", "20261108", true},
		{"requests-2026110.jsonl", "", false},
		{"requests-abcdefgh.jsonl", "", false},
		{"other-20261108.jsonl", "", false},
	}
	for _, tc := range tests {
		got, ok := DayOfFile(tc.name)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("%s 期望 (%q,%v)，实际 (%q,%v)", tc.name, tc.want, tc.ok, got, ok)
		}
	}
}
