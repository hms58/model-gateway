package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-gateway/internal/config"
)

func TestRewriteModelKeepsOtherFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := RewriteModel(body, "glm-5.2")
	if err != nil {
		t.Fatalf("改写失败：%v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatalf("输出不是合法 JSON：%v", err)
	}
	var model string
	if err := json.Unmarshal(fields["model"], &model); err != nil || model != "glm-5.2" {
		t.Fatalf("model 未改写：%s err=%v", model, err)
	}
	if string(fields["messages"]) != `[{"role":"user","content":"hi"}]` {
		t.Fatalf("messages 被改写：%s", fields["messages"])
	}
	if string(fields["temperature"]) != "0.7" {
		t.Fatalf("temperature 被改写：%s", fields["temperature"])
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		status int
		want   Outcome
	}{
		{200, OutcomeOK},
		{400, OutcomeClientError},
		{422, OutcomeClientError},
		{401, OutcomeProductAuth},
		{403, OutcomeProductAuth},
		{402, OutcomeQuota},
		{429, OutcomeQuota},
		{500, OutcomeUpstream},
		{503, OutcomeUpstream},
		{408, OutcomeUpstream},
	}
	for _, tc := range tests {
		if got := Classify(tc.status); got != tc.want {
			t.Fatalf("状态 %d 期望 %s，实际 %s", tc.status, tc.want, got)
		}
	}
}

type flushRecorder struct {
	header  http.Header
	code    int
	body    bytes.Buffer
	flushes int
}

func (f *flushRecorder) Header() http.Header { return f.header }
func (f *flushRecorder) Write(p []byte) (int, error) {
	return f.body.Write(p)
}
func (f *flushRecorder) WriteHeader(code int) { f.code = code }
func (f *flushRecorder) Flush()               { f.flushes++ }

// 缓存命中与积分要从各上游的不同字段名里取到，且不能被后续空分片覆盖。
func TestStreamCopyExtractsCacheHitAndCredit(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		wantHit    int
		wantCredit float64
		wantKnown  bool
	}{
		{
			name:    "prompt_tokens_details.cached_tokens（主流形态）",
			line:    `data: {"usage":{"prompt_tokens":8500,"completion_tokens":5,"total_tokens":8505,"prompt_tokens_details":{"cached_tokens":8400},"credit":0.12}}`,
			wantHit: 8400, wantCredit: 0.12, wantKnown: true,
		},
		{
			name:    "prompt_cache_hit_tokens（workbuddy 形态）",
			line:    `data: {"usage":{"prompt_tokens":1000,"prompt_cache_hit_tokens":900,"prompt_cache_miss_tokens":100,"credit":0.1}}`,
			wantHit: 900, wantCredit: 0.1, wantKnown: true,
		},
		{
			name:    "真实值优先于 0 兼容别名",
			line:    `data: {"usage":{"prompt_tokens":21000,"cached_tokens":0,"cache_read_input_tokens":0,"prompt_tokens_details":{"cached_tokens":20864}}}`,
			wantHit: 20864, wantKnown: false,
		},
		{
			name:    "无缓存维度也不无中生有",
			line:    `data: {"usage":{"prompt_tokens":100,"completion_tokens":2,"total_tokens":102}}`,
			wantHit: 0, wantKnown: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &flushRecorder{header: http.Header{}}
			usage, err := StreamCopy(recorder, strings.NewReader(tc.line+"\n\n"), recorder)
			if err != nil {
				t.Fatalf("流式拷贝失败：%v", err)
			}
			if usage.CacheHitTokens != tc.wantHit {
				t.Fatalf("命中 token 应为 %d，实际 %d", tc.wantHit, usage.CacheHitTokens)
			}
			if usage.HasCredit != tc.wantKnown {
				t.Fatalf("积分是否可知应为 %v，实际 %v", tc.wantKnown, usage.HasCredit)
			}
			if tc.wantKnown && usage.Credit != tc.wantCredit {
				t.Fatalf("积分应为 %v，实际 %v", tc.wantCredit, usage.Credit)
			}
		})
	}
}

// 流式末帧常只带 usage 的一小部分：已取到的值不能被空分片冲掉。
func TestStreamCopyKeepsUsageAcrossChunks(t *testing.T) {
	recorder := &flushRecorder{header: http.Header{}}
	stream := `data: {"usage":{"prompt_tokens":5000,"prompt_tokens_details":{"cached_tokens":4800}}}` + "\n\n" +
		`data: {"usage":{"completion_tokens":120,"total_tokens":5120}}` + "\n\n"
	usage, err := StreamCopy(recorder, strings.NewReader(stream), recorder)
	if err != nil {
		t.Fatalf("流式拷贝失败：%v", err)
	}
	if usage.PromptTokens != 5000 || usage.CacheHitTokens != 4800 {
		t.Fatalf("前一分片的 prompt/命中值被冲掉：%+v", usage)
	}
	if usage.CompletionTokens != 120 || usage.TotalTokens != 5120 {
		t.Fatalf("后一分片的补值未合并：%+v", usage)
	}
}

func TestStreamCopyFlushesPerChunk(t *testing.T) {
	recorder := &flushRecorder{header: http.Header{}}
	usage, err := StreamCopy(recorder,
		strings.NewReader("data: a\n\ndata: b\n\ndata: {\"usage\":1}\n\n"), recorder)
	if err != nil {
		t.Fatalf("流式拷贝失败：%v", err)
	}
	if recorder.flushes < 3 {
		t.Fatalf("每块都应 flush，实际 %d 次", recorder.flushes)
	}
	if !usage.Seen {
		t.Fatal("应识别到 usage 行")
	}
}

// usage 必须解析出具体 token 数，否则日志里的 tokens 恒为 0。
func TestStreamCopyParsesUsageTokens(t *testing.T) {
	recorder := &flushRecorder{header: http.Header{}}
	usage, err := StreamCopy(recorder,
		strings.NewReader("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22,\"total_tokens\":33}}\n\n"), recorder)
	if err != nil {
		t.Fatalf("流式拷贝失败：%v", err)
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 22 || usage.TotalTokens != 33 {
		t.Fatalf("usage 解析错误：%+v", usage)
	}
}

// 上游异常时可能回一整行超大 JSON（或长流里始终没有换行）：
// 出站响应必须有单行上限，否则整行会被读进内存（入站已有 32MB 限制，出站此前没有）。
func TestStreamCopyRejectsOversizedLine(t *testing.T) {
	recorder := &flushRecorder{header: http.Header{}}
	oversized := "data: " + strings.Repeat("a", maxLineBytes+1024)
	if _, err := StreamCopy(recorder, strings.NewReader(oversized), recorder); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("超长单行应返回 ErrLineTooLong，实际 %v", err)
	}
}

// 非流式响应本身就是单行大 JSON：远超初始缓冲但未超上限时必须完整转发并解析 usage。
func TestStreamCopyHandlesLongLegalLine(t *testing.T) {
	recorder := &flushRecorder{header: http.Header{}}
	payload := `{"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33},"pad":"` +
		strings.Repeat("x", 200<<10) + `"}`
	usage, err := StreamCopy(recorder, strings.NewReader(payload), recorder)
	if err != nil {
		t.Fatalf("合法长行不应报错：%v", err)
	}
	if usage.PromptTokens != 11 || usage.TotalTokens != 33 {
		t.Fatalf("长行也要解析出 usage：%+v", usage)
	}
	if recorder.body.Len() != len(payload) {
		t.Fatalf("长行必须完整转发：期望 %d 字节，实际 %d", len(payload), recorder.body.Len())
	}
}

func TestForwarderRewritesModelAndAuth(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer server.Close()

	product := config.Product{ID: "workbuddy", BaseURL: server.URL + "/", APIKey: "sk-product"}
	forwarder := NewForwarder()
	resp, err := forwarder.Do(context.Background(), product, []byte(`{"model":"gpt-5","messages":[]}`), "gpt-5-real")
	if err != nil {
		t.Fatalf("转发失败：%v", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("路径错误：%s", gotPath)
	}
	if gotAuth != "Bearer sk-product" {
		t.Fatalf("鉴权头未替换：%s", gotAuth)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &fields); err != nil {
		t.Fatalf("下游收到非法 JSON：%v", err)
	}
	var model string
	_ = json.Unmarshal(fields["model"], &model)
	if model != "gpt-5-real" {
		t.Fatalf("下游 model 未改写：%s", model)
	}
	if resp.Status != http.StatusTooManyRequests {
		t.Fatalf("状态码未透传：%d", resp.Status)
	}
	if Classify(resp.Status) != OutcomeQuota {
		t.Fatalf("429 应归类 quota：%s", Classify(resp.Status))
	}
	if string(resp.Body) != `{"error":"rate limited"}` {
		t.Fatalf("响应体未透传：%s", resp.Body)
	}
}

// 产品级 first_byte 必须真正生效：它是「换成员」的判定依据。
func TestForwarderHonorsProductFirstByteTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	product := config.Product{
		ID: "slow", BaseURL: server.URL, APIKey: "sk",
		Timeout: config.Timeout{Connect: "1s", FirstByte: "50ms", Total: "5s"},
	}
	forwarder := NewForwarder()
	if _, err := forwarder.Do(context.Background(), product, []byte(`{"model":"x"}`), "x"); err == nil {
		t.Fatal("超过 first_byte 应返回错误（触发降级）")
	}
}
