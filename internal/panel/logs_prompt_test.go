package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/reqlog"
)

// promptFixture 准备一份「请求日志摘要 + 提示词快照」，返回日志目录与当天日期。
func promptFixture(t *testing.T, content string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	day := time.Now().In(config.Location()).Format("20060102")

	snapshot, err := json.Marshal(reqlog.Prompt{
		RequestID: "rid-1", Time: "2026-11-09 10:00:00", Group: "gpt-5",
		Messages: []reqlog.PromptMessage{{Role: "user", Chars: utf8.RuneCountInString(content), Content: content}},
		Chars:    utf8.RuneCountInString(content), Stored: utf8.RuneCountInString(content),
	})
	if err != nil {
		t.Fatalf("构造提示词快照失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts-"+day+".jsonl"), append(snapshot, '\n'), 0o644); err != nil {
		t.Fatalf("写提示词快照失败：%v", err)
	}
	entry := `{"time":"2026-11-09 10:00:00","request_id":"rid-1","group":"gpt-5","outcome":"ok","http_status":200,` +
		`"prompt":{"messages":1,"chars":` + strconv.Itoa(utf8.RuneCountInString(content)) + `,"preview":"预览","stored":true}}`
	if err := os.WriteFile(filepath.Join(dir, "requests-"+day+".jsonl"), []byte(entry+"\n"), 0o644); err != nil {
		t.Fatalf("写请求日志失败：%v", err)
	}
	return dir, day
}

// 详情接口分两级：结构只回预览，正文按 offset/limit 分片拉，
// 无论哪一级都不能把整段提示词一次性返回。
func TestLogPromptDetailIsPaged(t *testing.T) {
	content := strings.Repeat("长", promptChunkRunes+120)
	dir, day := promptFixture(t, content)
	server, _ := newServer(t)
	handler := server.WithLogs(dir).Handler()

	structure := call(t, handler, http.MethodGet, "/api/modelgw/logs/prompt?request_id=rid-1&date="+day, nil)
	if structure.Code != http.StatusOK {
		t.Fatalf("结构接口应 200，实际 %d：%s", structure.Code, structure.Body.String())
	}
	var view struct {
		Found    bool `json:"found"`
		Chars    int  `json:"chars"`
		Stored   int  `json:"stored_chars"`
		Messages []struct {
			Index       int    `json:"index"`
			Role        string `json:"role"`
			Chars       int    `json:"chars"`
			StoredChars int    `json:"stored_chars"`
			Preview     string `json:"preview"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(structure.Body.Bytes(), &view); err != nil {
		t.Fatalf("结构响应不是合法 JSON：%v", err)
	}
	if !view.Found || len(view.Messages) != 1 || view.Messages[0].StoredChars != utf8.RuneCountInString(content) {
		t.Fatalf("结构详情不对：%+v", view)
	}
	if utf8.RuneCountInString(view.Messages[0].Preview) != promptPreviewRunes {
		t.Fatalf("预览应截到 %d 字符，实际 %d", promptPreviewRunes, utf8.RuneCountInString(view.Messages[0].Preview))
	}
	if strings.Contains(structure.Body.String(), content) {
		t.Fatal("结构接口不应返回提示词全文")
	}

	chunk := call(t, handler, http.MethodGet, "/api/modelgw/logs/prompt?request_id=rid-1&message=0", nil)
	var part struct {
		Content    string `json:"content"`
		NextOffset int    `json:"next_offset"`
		Chars      int    `json:"chars"`
		Done       bool   `json:"done"`
	}
	if err := json.Unmarshal(chunk.Body.Bytes(), &part); err != nil {
		t.Fatalf("分片响应不是合法 JSON：%v", err)
	}
	if utf8.RuneCountInString(part.Content) != promptChunkRunes || part.Done || part.NextOffset != promptChunkRunes {
		t.Fatalf("首片应正好 %d 字符且未读完：%+v", promptChunkRunes, part)
	}

	// 第二片把剩下的取完：拼起来必须等于原文。
	rest := call(t, handler, http.MethodGet,
		"/api/modelgw/logs/prompt?request_id=rid-1&message=0&offset="+strconv.Itoa(part.NextOffset), nil)
	var tail struct {
		Content string `json:"content"`
		Done    bool   `json:"done"`
	}
	if err := json.Unmarshal(rest.Body.Bytes(), &tail); err != nil {
		t.Fatalf("尾片响应不是合法 JSON：%v", err)
	}
	if !tail.Done || part.Content+tail.Content != content {
		t.Fatalf("分片拼接应等于原文：done=%v 长度=%d", tail.Done, utf8.RuneCountInString(part.Content+tail.Content))
	}
}

// 旧日志没有提示词快照：接口回 200 + found=false，让面板平静地提示，而不是报错。
func TestLogPromptMissingSnapshotIsNotAnError(t *testing.T) {
	server, _ := newServer(t)
	dir, _ := promptFixture(t, "abc")
	handler := server.WithLogs(dir).Handler()

	recorder := call(t, handler, http.MethodGet, "/api/modelgw/logs/prompt?request_id=rid-unknown", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("未知 request_id 应 200，实际 %d", recorder.Code)
	}
	var payload struct {
		Found bool `json:"found"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil || payload.Found {
		t.Fatalf("应回 found=false：%s", recorder.Body.String())
	}

	if bad := call(t, handler, http.MethodGet, "/api/modelgw/logs/prompt", nil); bad.Code != http.StatusBadRequest {
		t.Fatalf("缺 request_id 应 400，实际 %d", bad.Code)
	}
	if bad := call(t, handler, http.MethodGet, "/api/modelgw/logs/prompt?request_id=rid-1&message=9", nil); bad.Code != http.StatusBadRequest {
		t.Fatalf("message 越界应 400，实际 %d", bad.Code)
	}
}

// 列表接口只带摘要：面板一次翻 100 条，不能把提示词原文塞进列表响应。
func TestLogsListOnlyCarriesPromptSummary(t *testing.T) {
	content := strings.Repeat("长", 500)
	dir, _ := promptFixture(t, content)
	server, _ := newServer(t)
	recorder := call(t, server.WithLogs(dir).Handler(), http.MethodGet, "/api/modelgw/logs", nil)

	body := recorder.Body.String()
	if !strings.Contains(body, `"preview":"预览"`) || !strings.Contains(body, `"stored":true`) {
		t.Fatalf("列表应带提示词摘要：%s", body)
	}
	if strings.Contains(body, content) {
		t.Fatal("列表不应带提示词原文")
	}
	// 摘要预览参与关键字搜索：能按「问了什么」筛日志。
	hit := call(t, server.WithLogs(dir).Handler(), http.MethodGet, "/api/modelgw/logs?q=预览", nil)
	if !strings.Contains(hit.Body.String(), `"total":1`) {
		t.Fatalf("关键字应能搜到提示词预览：%s", hit.Body.String())
	}
}

// 日志页必须把提示词做成「列里片段 + 抽屉详情」：列表不展示全文，详情按消息分片加载。
func TestPanelLogsPromptColumnAndDrawer(t *testing.T) {
	server, _ := newServer(t)
	req := httptest.NewRequest(http.MethodGet, "/panel/logs", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	body := recorder.Body.String()

	for _, want := range []string{
		"<th>提示词</th>", "function promptText", `data-act="log-prompt"`,
		"function openLogPrompt", "function promptDrawer", "function loadPromptChunk",
		"/logs/prompt?", "prompt-wide", `class="prompt-body"`, "data-act=\"prompt-more\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("日志页缺少提示词详情能力 %q", want)
		}
	}
}
