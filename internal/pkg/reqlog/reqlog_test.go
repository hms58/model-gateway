package reqlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-gateway/internal/config"
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
	// 提示词快照与请求日志同一套命名规则，但前缀不同，不能互相误认。
	if _, ok := DayOfFile("prompts-20261108.jsonl"); ok {
		t.Fatal("DayOfFile 不应把提示词文件当成请求日志")
	}
	if day, ok := PromptDayOfFile("prompts-20261108.3.jsonl"); !ok || day != "20261108" {
		t.Fatalf("提示词分片应可解析：%q %v", day, ok)
	}
	if _, ok := PromptDayOfFile("requests-20261108.jsonl"); ok {
		t.Fatal("PromptDayOfFile 不应把请求日志当成提示词快照")
	}
}

// chat 请求的 messages 是整段对话历史：只保留最后一条 user 的本次输入，
// 历史提问与 system / assistant / tool 都只计入 omitted。
func TestCaptureChatPromptKeepsOnlyLatestUserInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5","messages":[
		{"role":"system","content":"你是助手"},
		{"role":"user","content":"第一轮问了什么"},
		{"role":"assistant","content":"回答"},
		{"role":"tool","content":"工具结果"},
		{"role":"user","content":"这次真正想问的问题"},
		{"role":"assistant","content":null}
	]}`)
	prompt, summary, ok := CaptureChatPrompt(body, PromptLimitChars)
	if !ok {
		t.Fatal("含用户输入的请求体应可抽取提示词")
	}
	if summary.Messages != 1 || len(prompt.Messages) != 1 {
		t.Fatalf("只应有 1 条本次输入：%d / %d", summary.Messages, len(prompt.Messages))
	}
	if prompt.Messages[0].Role != "user" || prompt.Messages[0].Content != "这次真正想问的问题" {
		t.Fatalf("保留的应是最后一条 user 输入：%+v", prompt.Messages[0])
	}
	if summary.Preview != "这次真正想问的问题" {
		t.Fatalf("预览应是本次输入，实际 %q", summary.Preview)
	}
	if prompt.Omitted != 5 || summary.Omitted != 5 {
		t.Fatalf("其余 5 条历史消息应计入 omitted：%d / %d", prompt.Omitted, summary.Omitted)
	}
}

// 客户端注入的 <additional_data> 不属于用户输入：只取 <user_query> 内的原文。
func TestCaptureChatPromptExtractsUserQuery(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":"历史问题"},
		{"role":"user","content":"<additional_data>\ncurrent_date: Monday, November 9, 2026\n</additional_data>\n\n<user_query>\n帮我改一下这个按钮\n</user_query>"}
	]}`)
	prompt, summary, ok := CaptureChatPrompt(body, PromptLimitChars)
	if !ok {
		t.Fatal("应可抽取提示词")
	}
	if prompt.Messages[0].Content != "帮我改一下这个按钮" {
		t.Fatalf("只应保留 <user_query> 内的原文：%q", prompt.Messages[0].Content)
	}
	if strings.Contains(prompt.Messages[0].Content, "additional_data") || summary.Preview != "帮我改一下这个按钮" {
		t.Fatalf("注入上下文不应进入快照或预览：%q / %q", prompt.Messages[0].Content, summary.Preview)
	}
	if summary.Chars != 9 || prompt.Omitted != 1 {
		t.Fatalf("字符数与 omitted 不对：chars=%d omitted=%d", summary.Chars, prompt.Omitted)
	}
}

// 普通 OpenAI 客户端没有 <user_query> 标签：整条保留；图片分段只记占位，不抄 base64。
func TestCaptureChatPromptFallsBackToRawText(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"你是助手"},
		{"role":"user","content":[{"type":"text","text":"这张图是什么"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","content":null}
	]}`)
	prompt, summary, ok := CaptureChatPrompt(body, PromptLimitChars)
	if !ok {
		t.Fatal("应可抽取提示词")
	}
	if prompt.Messages[0].Content != "这张图是什么[image_url]" {
		t.Fatalf("无标签时应整条保留：%q", prompt.Messages[0].Content)
	}
	if strings.Contains(prompt.Messages[0].Content, "base64") {
		t.Fatalf("提示词不应抄入 base64：%s", prompt.Messages[0].Content)
	}
	if got := prompt.Messages[0].Chars; got != 17 {
		t.Fatalf("字符数应按 rune 计，实际 %d", got)
	}
	if prompt.Omitted != 2 || summary.Omitted != 2 {
		t.Fatalf("其余 2 条消息应计入 omitted：%d / %d", prompt.Omitted, summary.Omitted)
	}

	// 不是 chat 请求、或没有用户输入时，不产出快照。
	if _, _, ok := CaptureChatPrompt([]byte(`{"model":"gpt-5"}`), PromptLimitChars); ok {
		t.Fatal("没有 messages 的请求体不应产出提示词")
	}
	noUser := []byte(`{"messages":[{"role":"system","content":"你是助手"},{"role":"assistant","content":null},{"role":"tool","content":"结果"}]}`)
	if _, _, ok := CaptureChatPrompt(noUser, PromptLimitChars); ok {
		t.Fatal("没有用户输入时不应产出提示词快照")
	}
}

// 只有一条内容：超上限直接截前 limit 个字符；上限为 0 回落默认。
func TestCaptureChatPromptTruncatesSingleInput(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"0123456789abcdefghij"}]}`)
	prompt, summary, ok := CaptureChatPrompt(body, 8)
	if !ok {
		t.Fatal("应可抽取提示词")
	}
	if prompt.Messages[0].Content != "01234567" || !prompt.Messages[0].Truncated {
		t.Fatalf("应截到 8 个字符：%+v", prompt.Messages[0])
	}
	if summary.Chars != 20 || prompt.Stored != 8 || !summary.Truncated {
		t.Fatalf("截断口径不对：chars=%d stored=%d truncated=%v", summary.Chars, prompt.Stored, summary.Truncated)
	}
	if _, zero, _ := CaptureChatPrompt(body, 0); zero.Truncated {
		t.Fatal("limit<=0 应回落到默认上限，20 字符不应被截断")
	}
}

// 提示词快照与请求日志分文件落盘：日志里只有摘要，全文能按 request_id 读回来。
func TestWritePromptPersistsAndReadsByRequestID(t *testing.T) {
	dir := t.TempDir()
	logger := New(dir, 14)
	defer logger.Close()

	now := time.Now().In(config.Location())
	day := now.Format("20060102")
	prompt := Prompt{
		RequestID: "rid-1", Time: now.Format(config.TimeLayout), Group: "gpt-5",
		Messages: []PromptMessage{{Role: "user", Chars: 6, Content: "整段提示词内容"}},
		Chars:    6, Stored: 6,
	}
	if !logger.WritePrompt(prompt) {
		t.Fatal("提示词应入队成功")
	}
	logger.Write(Entry{
		RequestID: "rid-1", Group: "gpt-5",
		Prompt: &PromptSummary{Messages: 1, Chars: 6, Preview: "预览片段", Stored: true},
	})
	logger.Flush()

	got, ok := ReadPromptIn(dir, day, "rid-1")
	if !ok {
		t.Fatal("应可按 request_id 读到提示词快照")
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "整段提示词内容" {
		t.Fatalf("快照内容不对：%+v", got)
	}
	if _, ok := ReadPromptIn(dir, day, "rid-missing"); ok {
		t.Fatal("不存在的 request_id 不应命中")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "requests-"+day+".jsonl"))
	if err != nil {
		t.Fatalf("读请求日志失败：%v", err)
	}
	if !strings.Contains(string(raw), "预览片段") {
		t.Fatalf("请求日志应带摘要预览：%s", raw)
	}
	if strings.Contains(string(raw), "整段提示词内容") {
		t.Fatalf("请求日志不应带提示词原文：%s", raw)
	}
}

// 提示词文件同样按大小滚动：详情查询必须能跨分片命中。
func TestReadPromptInReadsRolledParts(t *testing.T) {
	dir := t.TempDir()
	day := time.Now().In(config.Location()).Format("20060102")
	files := map[string]string{
		"prompts-" + day + ".jsonl":   `{"request_id":"rid-old","messages":[]}`,
		"prompts-" + day + ".1.jsonl": `{"request_id":"rid-new","messages":[{"role":"user","chars":2,"content":"分片"}]}`,
	}
	for name, line := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(line+"\n"), 0o644); err != nil {
			t.Fatalf("准备提示词文件失败：%v", err)
		}
	}
	got, ok := ReadPromptIn(dir, day, "rid-new")
	if !ok || got.Messages[0].Content != "分片" {
		t.Fatalf("应跨分片命中：%+v %v", got, ok)
	}
}

// 提示词文件也在保留期内：过期分片必须一起删掉，否则日志目录只会越来越大。
func TestCleanupRemovesExpiredPromptFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "prompts-20200101.jsonl")
	recent := filepath.Join(dir, "prompts-"+time.Now().In(config.Location()).Format("20060102")+".jsonl")
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
		t.Fatalf("过期提示词文件应被删除：%s", old)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("近期提示词文件应保留：%v", err)
	}
}

// 按 rune 截断不能切坏 UTF-8，也不能越界。
func TestClipRunes(t *testing.T) {
	if got := ClipRunes("中文abc", 2); got != "中文" {
		t.Fatalf("按字符截断错误：%q", got)
	}
	if got := ClipRunes("中文", 0); got != "" {
		t.Fatalf("上限 0 应返回空串：%q", got)
	}
	if got := ClipRunes("中文", 99); got != "中文" {
		t.Fatalf("不足上限时应原样返回：%q", got)
	}
}
