package dataplane

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/reqlog"
	"model-gateway/internal/pkg/store"
	"model-gateway/internal/pkg/token"
)

type fixture struct {
	store     *store.Store
	plain     string
	statePath string
}

func newFixture(t *testing.T, upstream string, members []config.Member) fixture {
	t.Helper()
	plain, prefix, sha, err := token.Generate()
	if err != nil {
		t.Fatalf("生成 Token 失败：%v", err)
	}

	cfg := config.Default()
	cfg.Products = []config.Product{
		{ID: "workbuddy", Enabled: true, BaseURL: upstream, APIKey: "sk-primary"},
		{ID: "trae", Enabled: true, BaseURL: upstream, APIKey: "sk-backup"},
	}
	cfg.ModelGroups = []config.Group{{Name: "gpt-5", Enabled: true, Members: members}}
	cfg.Tokens = []config.Token{{
		ID: "tk_01", Name: "test", Enabled: true, KeyPrefix: prefix, KeySHA256: sha, Groups: []string{"gpt-5"},
	}}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "modelgw.json")
	if err := config.SaveAtomic(cfgPath, &cfg); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}
	statePath := filepath.Join(dir, "state.json")
	st, err := store.Open(cfgPath, statePath)
	if err != nil {
		t.Fatalf("打开 store 失败：%v", err)
	}
	return fixture{store: st, plain: plain, statePath: statePath}
}

func doChat(t *testing.T, handler *Handler, key string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"model": "gpt-5", "messages": []interface{}{}})
	if err != nil {
		t.Fatalf("构造请求体失败：%v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	handler.ChatCompletions(recorder, req)
	return recorder
}

func TestChatCompletionsForwardsAndRewritesModel(t *testing.T) {
	var sawModel, sawAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		_ = json.Unmarshal(raw, &payload)
		_ = json.Unmarshal(payload["model"], &sawModel)
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	fx := newFixture(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "gpt-5-real", Priority: 0, Weight: 100, Enabled: true},
	})
	handler := New(fx.store)

	recorder := doChat(t, handler, fx.plain)
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if sawModel != "gpt-5-real" {
		t.Fatalf("下游 model 未改写：%s", sawModel)
	}
	if sawAuth != "Bearer sk-primary" {
		t.Fatalf("下游鉴权头未替换：%s", sawAuth)
	}
}

func TestChatCompletionsRejectsInvalidToken(t *testing.T) {
	var called int64
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&called, 1)
	}))
	defer upstream.Close()

	fx := newFixture(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true},
	})
	handler := New(fx.store)

	recorder := doChat(t, handler, "mg-invalid")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d", recorder.Code)
	}
	if atomic.LoadInt64(&called) != 0 {
		t.Fatal("鉴权失败不应打下游")
	}
}

func TestChatCompletionsFailsOverOn429(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		_ = json.Unmarshal(raw, &payload)
		var model string
		_ = json.Unmarshal(payload["model"], &model)
		if model == "primary" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	fx := newFixture(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "primary", Priority: 0, Weight: 100, Enabled: true},
		{Product: "trae", Model: "backup", Priority: 10, Weight: 100, Enabled: true},
	})
	handler := New(fx.store)

	recorder := doChat(t, handler, fx.plain)
	if recorder.Code != http.StatusOK {
		t.Fatalf("降级后应成功，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("期望两次下游调用，实际 %d", calls)
	}
}

func TestChatCompletionsPassesThrough400WithoutRetry(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer upstream.Close()

	fx := newFixture(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "primary", Priority: 0, Weight: 100, Enabled: true},
		{Product: "trae", Model: "backup", Priority: 10, Weight: 100, Enabled: true},
	})
	handler := New(fx.store)

	recorder := doChat(t, handler, fx.plain)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("400 应原样透传，实际 %d", recorder.Code)
	}
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("400 不应换成员，实际调用 %d 次", calls)
	}
}

func TestChatCompletionsAllMembersFailedReturnsAttempts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer upstream.Close()

	fx := newFixture(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "primary", Priority: 0, Weight: 100, Enabled: true},
		{Product: "trae", Model: "backup", Priority: 10, Weight: 100, Enabled: true},
	})
	handler := New(fx.store)

	recorder := doChat(t, handler, fx.plain)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("应返回最后一次错误，实际 %d", recorder.Code)
	}
	var payload struct {
		Error struct {
			Attempts []Attempt `json:"attempts"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(payload.Error.Attempts) != 2 {
		t.Fatalf("attempts 应记录两次尝试，实际 %+v", payload.Error.Attempts)
	}
}

func TestHealthzReportsAlive(t *testing.T) {
	fx := newFixture(t, "http://127.0.0.1:9", []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true},
	})
	handler := New(fx.store)

	recorder := httptest.NewRecorder()
	handler.Healthz(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"status":"ok"`) {
		t.Fatalf("健康检查应返回存活标记：%s", recorder.Body.String())
	}
}

func TestStreamFailsOverBeforeFirstByte(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: hello\n\n"))
	}))
	defer good.Close()

	fx := newFixture(t, bad.URL, []config.Member{
		{Product: "workbuddy", Model: "primary", Priority: 0, Weight: 100, Enabled: true},
		{Product: "trae", Model: "backup", Priority: 10, Weight: 100, Enabled: true},
	})
	cfg := *fx.store.Snapshot()
	cfg.Products[0].BaseURL = bad.URL
	cfg.Products[1].BaseURL = good.URL
	if err := fx.store.Save(cfg, cfg.Version); err != nil {
		t.Fatalf("改产品地址失败：%v", err)
	}

	body, _ := json.Marshal(map[string]interface{}{"model": "gpt-5", "stream": true, "messages": []interface{}{}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fx.plain)
	recorder := httptest.NewRecorder()
	New(fx.store).ChatCompletions(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("首字节前失败应换成员并成功，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "data: hello") {
		t.Fatalf("应透传流式内容：%s", recorder.Body.String())
	}
}

func TestChatCompletionsUnknownGroupReturns404(t *testing.T) {
	fx := newFixture(t, "http://127.0.0.1:9", []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true},
	})
	handler := New(fx.store)

	body, _ := json.Marshal(map[string]interface{}{"model": "not-exist", "messages": []interface{}{}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fx.plain)
	recorder := httptest.NewRecorder()
	handler.ChatCompletions(recorder, req)

	// spec 只要求 4xx 且不打下游：Token 组权限先于组存在性判定，未授权组返回 403。
	if recorder.Code < 400 || recorder.Code >= 500 {
		t.Fatalf("未知/未授权模型组应返回 4xx，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

func TestListModelsOnlyExposesGroupNames(t *testing.T) {
	fx := newFixture(t, "http://127.0.0.1:9", []config.Member{
		{Product: "workbuddy", Model: "gpt-5-real", Weight: 100, Enabled: true},
	})
	handler := New(fx.store)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+fx.plain)
	recorder := httptest.NewRecorder()
	handler.ListModels(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", recorder.Code)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(payload.Data) != 1 || payload.Data[0].ID != "gpt-5" {
		t.Fatalf("应只返回组名：%+v", payload.Data)
	}
}

// 降级排障依赖 attempts 与 outcome：日志必须记全每次尝试与最终结果。
func TestChatCompletionsLogsAttemptsAndOutcome(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt64(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`))
	}))
	defer upstream.Close()

	fx := newFixture(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Priority: 0, Weight: 100, Enabled: true},
		{Product: "trae", Model: "gpt-5", Priority: 1, Weight: 100, Enabled: true},
	})
	logger := reqlog.New(t.TempDir(), 1)
	defer logger.Close()
	handler := New(fx.store).WithLogger(logger)

	if recorder := doChat(t, handler, fx.plain); recorder.Code != http.StatusOK {
		t.Fatalf("期望降级后 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	logger.Flush()

	entries := logger.Recent(1)
	if len(entries) != 1 {
		t.Fatalf("应落一条请求日志，实际 %d 条", len(entries))
	}
	entry := entries[0]
	if len(entry.Attempts) != 2 {
		t.Fatalf("应记录 2 次尝试，实际 %d：%+v", len(entry.Attempts), entry.Attempts)
	}
	if entry.Outcome != "ok" || entry.HTTPStatus != http.StatusOK {
		t.Fatalf("最终结果应为 ok/200，实际 %s/%d", entry.Outcome, entry.HTTPStatus)
	}
	if entry.TokenID == "" || entry.TokenName != "test" {
		t.Fatalf("日志应带 Token 标识：%+v", entry)
	}
	if entry.Product == "" || entry.Model == "" {
		t.Fatalf("日志应带命中成员：%+v", entry)
	}
	if entry.TotalTokens != 33 || entry.PromptTokens != 11 || entry.CompletionTokens != 22 {
		t.Fatalf("日志应带 usage：%+v", entry)
	}
}

// 未知模型必须回 404，不能被「Token 无权限」的 403 掩盖。
func TestUnknownModelReturns404(t *testing.T) {
	fx := newFixture(t, "http://127.0.0.1:9", []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true},
	})
	handler := New(fx.store)

	body := `{"model":"no-such-group","messages":[{"role":"user","content":"hi"}]}`
	payload := strings.NewReader(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", payload)
	req.Header.Set("Authorization", "Bearer "+fx.plain)
	recorder := httptest.NewRecorder()
	handler.ChatCompletions(recorder, req)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未知模型应 404，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// 组间完全独立：A 组全挂时返回 502 + attempts，绝不回退到 B 组的成员。
func TestGroupFailureDoesNotFallBackToOtherGroup(t *testing.T) {
	var backupHits int64
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&backupHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer backup.Close()

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()

	plain, prefix, sha, err := token.Generate()
	if err != nil {
		t.Fatalf("生成 Token 失败：%v", err)
	}
	cfg := config.Default()
	cfg.Products = []config.Product{
		{ID: "dead", Enabled: true, BaseURL: dead.URL, APIKey: "sk-dead"},
		{ID: "backup", Enabled: true, BaseURL: backup.URL, APIKey: "sk-backup"},
	}
	cfg.ModelGroups = []config.Group{
		{Name: "group-a", Enabled: true, Members: []config.Member{
			{Product: "dead", Model: "m", Priority: 0, Weight: 100, Enabled: true},
		}},
		{Name: "group-b", Enabled: true, Members: []config.Member{
			{Product: "backup", Model: "m", Priority: 0, Weight: 100, Enabled: true},
		}},
	}
	cfg.Tokens = []config.Token{{
		ID: "tk_01", Name: "t", Enabled: true, KeyPrefix: prefix, KeySHA256: sha, Groups: []string{"*"},
	}}

	dir := t.TempDir()
	path := filepath.Join(dir, "modelgw.json")
	if err := config.SaveAtomic(path, &cfg); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}
	st, err := store.Open(path, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("打开 store 失败：%v", err)
	}
	handler := New(st)

	body := `{"model":"group-a","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plain)
	recorder := httptest.NewRecorder()
	handler.ChatCompletions(recorder, req)

	// 全部失败时透传最后一次上游状态（500），并附 attempts 明细。
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("组内全失败应透传 500，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if atomic.LoadInt64(&backupHits) != 0 {
		t.Fatal("不应跨组回退到其它模型组的成员")
	}
	if strings.Contains(recorder.Body.String(), "backup") {
		t.Fatalf("attempts 只应包含本组成员：%s", recorder.Body.String())
	}
}

// 非 openai-chat 的协议可登记，但请求期必须给出 20608 且不打下游。
func TestUnsupportedProtocolRejectedAtRequestTime(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
	}))
	defer upstream.Close()

	fx := newFixture(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true},
	})
	cfg := *fx.store.Snapshot()
	cfg.Products[0].Protocol = "anthropic-messages"
	if err := fx.store.Save(cfg, cfg.Version); err != nil {
		t.Fatalf("保存未知协议失败：%v", err)
	}
	handler := New(fx.store)

	recorder := doChat(t, handler, fx.plain)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "20608") {
		t.Fatalf("应返回 20608：%s", recorder.Body.String())
	}
	if atomic.LoadInt64(&calls) != 0 {
		t.Fatal("协议不支持时不应打下游")
	}
}

// 重启恢复依赖 state.json：Token 计数与产品健康摘要必须一起落盘。
func TestStateFileCarriesProductAndTokenStats(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	fx := newFixture(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true},
	})
	handler := New(fx.store)
	if recorder := doChat(t, handler, fx.plain); recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	if got := fx.store.State().TokenSnapshot("tk_01").RequestCount; got != 1 {
		t.Fatalf("Token 计数应累加，实际 %d", got)
	}
	if got := fx.store.State().ProductSnapshot("workbuddy").LastOKAt; got == "" {
		t.Fatal("产品最近成功时间应被记录")
	}
	if err := fx.store.SaveState(); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	raw, err := os.ReadFile(fx.statePath)
	if err != nil {
		t.Fatalf("读取 state.json 失败：%v", err)
	}
	for _, want := range []string{`"products"`, `"tokens"`, `"request_count"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("state.json 缺少 %s：%s", want, raw)
		}
	}
}

// 401/403 属产品凭据问题：换产品仍可能成功，但不应把成员打进冷却。
func TestProductAuthFailureDoesNotCoolMember(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	member := config.Member{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true}
	fx := newFixture(t, upstream.URL, []config.Member{member})
	handler := New(fx.store)

	if recorder := doChat(t, handler, fx.plain); recorder.Code < 400 {
		t.Fatalf("期望失败响应，实际 %d", recorder.Code)
	}
	key := config.MemberKey("gpt-5", member)
	if snapshot := fx.store.State().Snapshot(key); snapshot.CooldownUntil != "" || snapshot.BreakerUntil != "" {
		t.Fatalf("产品鉴权失败不应冷却成员：%+v", snapshot)
	}
}
