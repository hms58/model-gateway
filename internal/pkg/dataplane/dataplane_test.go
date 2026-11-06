package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// 不同上游对同一份请求的宽容度不同（如 Gemini 兼容层会拒掉某些 OpenAI 字段），
// 400 也必须换成员再试；但请求兼容性问题不该让成员进冷却。
func TestChatCompletionsFailsOverOn400(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		_ = json.Unmarshal(raw, &payload)
		var model string
		_ = json.Unmarshal(payload["model"], &model)
		if model == "primary" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"model is not specified"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	members := []config.Member{
		{Product: "workbuddy", Model: "primary", Priority: 0, Weight: 100, Enabled: true},
		{Product: "trae", Model: "backup", Priority: 10, Weight: 100, Enabled: true},
	}
	fx := newFixture(t, upstream.URL, members)
	handler := New(fx.store)

	recorder := doChat(t, handler, fx.plain)
	if recorder.Code != http.StatusOK {
		t.Fatalf("400 后应降级成功，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("400 应换成员，实际调用 %d 次", calls)
	}
	if snap := fx.store.State().Snapshot(config.MemberKey("gpt-5", members[0])); snap.CooldownUntil != "" || snap.BreakerUntil != "" {
		t.Fatalf("400 不应冷却成员：%+v", snap)
	}
}

// 额度耗尽 / 地区不支持这类 400 重试同一成员也没用：降级的同时把成员摘一段时间，
// 否则每个请求都会先白撞一次。
func TestChatCompletionsParksMemberOnUnavailable400(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"额度耗尽", `{"error":{"message":"You exceeded your current quota, please check your plan and billing details."}}`},
		{"地区不支持", `{"error":{"message":"User location is not supported for the API use."}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var payload map[string]json.RawMessage
				_ = json.Unmarshal(raw, &payload)
				var model string
				_ = json.Unmarshal(payload["model"], &model)
				if model == "primary" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[]}`))
			}))
			defer upstream.Close()

			members := []config.Member{
				{Product: "workbuddy", Model: "primary", Priority: 0, Weight: 100, Enabled: true},
				{Product: "trae", Model: "backup", Priority: 10, Weight: 100, Enabled: true},
			}
			fx := newFixture(t, upstream.URL, members)

			recorder := doChat(t, New(fx.store), fx.plain)
			if recorder.Code != http.StatusOK {
				t.Fatalf("应降级成功，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
			snap := fx.store.State().Snapshot(config.MemberKey("gpt-5", members[0]))
			if snap.CooldownUntil == "" {
				t.Fatalf("额度/地区类 400 应把成员摘掉：%+v", snap)
			}
		})
	}
}

// 协议/参数系统性变更会让同一成员每次都 400：连续达到阈值后必须摘掉，
// 否则每个请求都要先白撞一次。
func TestChatCompletionsCoolsMemberAfterRepeated400(t *testing.T) {
	var primaryCalls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		_ = json.Unmarshal(raw, &payload)
		var model string
		_ = json.Unmarshal(payload["model"], &model)
		if model == "primary" {
			atomic.AddInt64(&primaryCalls, 1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unsupported parameter"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	// backup 权重必须低于 primary：同权重时 prefer_last_ok 会把上次成功的 backup 提到层首，
	// primary 就再也不会被选中，连续失败也就无从累积。
	members := []config.Member{
		{Product: "workbuddy", Model: "primary", Priority: 0, Weight: 100, Enabled: true},
		{Product: "trae", Model: "backup", Priority: 10, Weight: 10, Enabled: true},
	}
	fx := newFixture(t, upstream.URL, members)
	handler := New(fx.store)

	// 默认规则：400 连续 5 次 → 60s。
	for i := 0; i < 5; i++ {
		if recorder := doChat(t, handler, fx.plain); recorder.Code != http.StatusOK {
			t.Fatalf("第 %d 次应降级成功，实际 %d", i+1, recorder.Code)
		}
	}
	if snap := fx.store.State().Snapshot(config.MemberKey("gpt-5", members[0])); snap.CooldownUntil == "" {
		t.Fatalf("连续 400 达到阈值应冷却：%+v", snap)
	}
	calls := atomic.LoadInt64(&primaryCalls)
	if calls != 5 {
		t.Fatalf("前 5 次应各打一次 primary，实际 %d", calls)
	}

	// 冷却期内不再打 primary，直接走 backup。
	if recorder := doChat(t, handler, fx.plain); recorder.Code != http.StatusOK {
		t.Fatalf("冷却期应直接走 backup，实际 %d", recorder.Code)
	}
	if got := atomic.LoadInt64(&primaryCalls); got != calls {
		t.Fatalf("冷却期内不应再请求 primary，实际 %d 次", got)
	}
}

// status_rules 可自定义任意状态码的阈值与时长，覆盖默认规则。
func TestStatusRulesOverrideDefaultThreshold(t *testing.T) {
	var primaryCalls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		_ = json.Unmarshal(raw, &payload)
		var model string
		_ = json.Unmarshal(payload["model"], &model)
		if model == "primary" {
			atomic.AddInt64(&primaryCalls, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	members := []config.Member{
		{Product: "workbuddy", Model: "primary", Priority: 0, Weight: 100, Enabled: true},
		{Product: "trae", Model: "backup", Priority: 10, Weight: 10, Enabled: true},
	}
	fx := newFixture(t, upstream.URL, members)
	cfg := *fx.store.Snapshot()
	cfg.Routing.Cooldown.StatusRules = []config.StatusRule{{Status: 429, Threshold: 2, Duration: "30s"}}
	if err := fx.store.Save(cfg, cfg.Version); err != nil {
		t.Fatalf("保存 status_rules 失败：%v", err)
	}
	handler := New(fx.store)

	for i := 0; i < 2; i++ {
		if recorder := doChat(t, handler, fx.plain); recorder.Code != http.StatusOK {
			t.Fatalf("第 %d 次应降级成功，实际 %d", i+1, recorder.Code)
		}
	}
	if snap := fx.store.State().Snapshot(config.MemberKey("gpt-5", members[0])); snap.CooldownUntil == "" {
		t.Fatalf("自定义阈值 2 次应冷却：%+v", snap)
	}
}

// 全部成员都 400 时，把最后一个上游的响应原样回给客户端，
// 而不是自造的 all members failed —— 客户端才能看出是谁在拒什么。
func TestChatCompletionsAllClientErrorsReturnUpstreamBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"free tier quota exceeded"}}`))
	}))
	defer upstream.Close()

	fx := newFixture(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "primary", Priority: 0, Weight: 100, Enabled: true},
		{Product: "trae", Model: "backup", Priority: 10, Weight: 100, Enabled: true},
	})
	handler := New(fx.store)

	recorder := doChat(t, handler, fx.plain)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("全部 400 应返回 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "free tier quota exceeded") {
		t.Fatalf("应原样透传上游错误体：%s", recorder.Body.String())
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

// 流式请求的 400 同样要降级，且不能把成员打进冷却（与非流式口径一致）。
func TestStreamFailsOverOn400WithoutCooldown(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad field"}}`))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: hello\n\n"))
	}))
	defer good.Close()

	members := []config.Member{
		{Product: "workbuddy", Model: "primary", Priority: 0, Weight: 100, Enabled: true},
		{Product: "trae", Model: "backup", Priority: 10, Weight: 100, Enabled: true},
	}
	fx := newFixture(t, bad.URL, members)
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
		t.Fatalf("400 后应降级成功，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "data: hello") {
		t.Fatalf("应透传流式内容：%s", recorder.Body.String())
	}
	if snap := fx.store.State().Snapshot(config.MemberKey("gpt-5", members[0])); snap.CooldownUntil != "" || snap.BreakerUntil != "" {
		t.Fatalf("400 不应冷却成员：%+v", snap)
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

// 请求日志要能回答「谁打进来的」并带上用量维度：来源 IP、UA、客户端请求 ID、
// 输入/输出 token、缓存命中率、积分。上游没给的维度必须留空，不能编造 0。
func TestRequestLogCapturesSourceAndUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":8500,"completion_tokens":5,"total_tokens":8505,"prompt_tokens_details":{"cached_tokens":8400},"credit":0.12}}`))
	}))
	defer upstream.Close()

	member := config.Member{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true}
	fx := newFixture(t, upstream.URL, []config.Member{member})
	logger := reqlog.New(t.TempDir(), 1)
	defer logger.Close()
	handler := New(fx.store).WithLogger(logger)

	body, _ := json.Marshal(map[string]interface{}{"model": "gpt-5", "messages": []interface{}{}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fx.plain)
	req.Header.Set("X-Request-Id", "trace-abc-123")
	req.Header.Set("User-Agent", "PicoClaw/0.3.1")
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	recorder := httptest.NewRecorder()
	handler.ChatCompletions(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	entries := logger.Recent(1)
	if len(entries) != 1 {
		t.Fatalf("应写入一条请求日志，实际 %d", len(entries))
	}
	entry := entries[0]
	if entry.ClientRequestID != "trace-abc-123" {
		t.Fatalf("客户端请求 ID 未采集：%q", entry.ClientRequestID)
	}
	if entry.UA != "PicoClaw/0.3.1" {
		t.Fatalf("UA 未采集：%q", entry.UA)
	}
	if entry.ClientIP != "203.0.113.7" {
		t.Fatalf("应取 X-Forwarded-For 首段：%q", entry.ClientIP)
	}
	if entry.PromptTokens != 8500 || entry.CompletionTokens != 5 {
		t.Fatalf("输入/输出 token 未采集：%+v", entry)
	}
	if entry.CacheHitTokens != 8400 {
		t.Fatalf("缓存命中 token 未采集：%d", entry.CacheHitTokens)
	}
	// 8400/8500 ≈ 0.988
	if rate := entry.CacheHitRate; rate < 0.98 || rate > 0.99 {
		t.Fatalf("缓存命中率计算错误：%v", rate)
	}
	if !entry.CreditKnown || entry.Credit != 0.12 {
		t.Fatalf("积分未采集：%+v", entry)
	}
	if entry.RequestID == "" {
		t.Fatal("网关自身的 request_id 应始终有值")
	}
}

// 上游不给缓存/积分维度时，日志必须留空而不是写 0，否则面板会把「无数据」读成「未命中/免费」。
func TestRequestLogOmitsAbsentUsageDimensions(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":2,"total_tokens":102}}`))
	}))
	defer upstream.Close()

	member := config.Member{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true}
	fx := newFixture(t, upstream.URL, []config.Member{member})
	logger := reqlog.New(t.TempDir(), 1)
	defer logger.Close()
	handler := New(fx.store).WithLogger(logger)

	recorder := doChat(t, handler, fx.plain)
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", recorder.Code)
	}
	entries := logger.Recent(1)
	if len(entries) != 1 {
		t.Fatalf("应写入一条请求日志，实际 %d", len(entries))
	}
	entry := entries[0]
	if entry.CacheHitTokens != 0 || entry.CacheHitRate != 0 {
		t.Fatalf("上游未给缓存维度时不应写值：%+v", entry)
	}
	if entry.CreditKnown {
		t.Fatalf("上游未给积分时 CreditKnown 应为 false：%+v", entry)
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

// 一次尝试耗时超过冷却基数时，已试成员会在重试期间复活。
// 修复前第 3 次选路会重新命中它并被打成「无可用成员」，只试 2 个就 502。
func TestChatCompletionsRetriesAllMembersWhenCooldownExpiresMidFlight(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "非流式"
		if stream {
			name = "流式"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(120 * time.Millisecond)
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
			}))
			defer upstream.Close()

			fx := newFixture(t, upstream.URL, []config.Member{
				{Product: "workbuddy", Model: "m1", Weight: 100, Enabled: true},
				{Product: "workbuddy", Model: "m2", Weight: 90, Enabled: true},
				{Product: "trae", Model: "m3", Weight: 80, Enabled: true},
				{Product: "trae", Model: "m4", Weight: 70, Enabled: true},
			})
			cfg := *fx.store.Snapshot()
			cfg.Routing.MaxAttempts = 4
			cfg.Routing.Cooldown.Base = "50ms"
			if err := fx.store.Save(cfg, cfg.Version); err != nil {
				t.Fatalf("改冷却配置失败：%v", err)
			}

			body, _ := json.Marshal(map[string]interface{}{
				"model": "gpt-5", "stream": stream, "messages": []interface{}{},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+fx.plain)
			recorder := httptest.NewRecorder()
			New(fx.store).ChatCompletions(recorder, req)

			var payload struct {
				Error struct {
					Attempts []Attempt `json:"attempts"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("响应不是合法 JSON：%v / %s", err, recorder.Body.String())
			}
			if len(payload.Error.Attempts) != 4 {
				t.Fatalf("应尝试 4 个不同成员，实际 %d：%+v", len(payload.Error.Attempts), payload.Error.Attempts)
			}
			seen := make(map[string]bool, 4)
			for _, attempt := range payload.Error.Attempts {
				if seen[attempt.Model] {
					t.Fatalf("成员被重复选中：%+v", payload.Error.Attempts)
				}
				seen[attempt.Model] = true
			}
		})
	}
}

func TestTransportCooldownScalesBaseWithElapsed(t *testing.T) {
	scaled := transportCooldown(config.Cooldown{Base: "2s", Max: "60s"}, 30*time.Second)
	parsed, err := time.ParseDuration(scaled.Base)
	if err != nil || parsed != 30*time.Second {
		t.Fatalf("30s 超时应把冷却基数抬到 30s，实际 %q", scaled.Base)
	}
	plain := transportCooldown(config.Cooldown{Base: "2s", Max: "60s"}, 10*time.Millisecond)
	if plain.Base != "2s" {
		t.Fatalf("短失败不应改动冷却基数，实际 %q", plain.Base)
	}
}

func TestNoMemberRecordsServiceUnavailable(t *testing.T) {
	member := config.Member{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true}
	fx := newFixture(t, "http://127.0.0.1:9", []config.Member{member})
	cfg := *fx.store.Snapshot()
	fx.store.State().Failure(config.MemberKey("gpt-5", member), cfg.Routing.Cooldown)

	logger := reqlog.New(t.TempDir(), 1)
	defer logger.Close()
	handler := New(fx.store).WithLogger(logger)
	recorder := doChat(t, handler, fx.plain)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("无可用成员应返回 503，实际 %d", recorder.Code)
	}
	entries := logger.Recent(1)
	if len(entries) != 1 {
		t.Fatalf("应写入一条请求日志，实际 %d", len(entries))
	}
	if entries[0].HTTPStatus != http.StatusServiceUnavailable || entries[0].Outcome != "no_member" {
		t.Fatalf("日志状态码应为 503/no_member，实际 %+v", entries[0])
	}
}

// 客户端超时断开后 r.Context() 已取消；修复前会继续用这个 ctx 把剩余成员
// 全部 0ms 打一遍并误冷却，制造出一批集中 no_member。
func TestChatCompletionsStopsOnClientCancel(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "非流式"
		if stream {
			name = "流式"
		}
		t.Run(name, func(t *testing.T) {
			var calls int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt64(&calls, 1)
				time.Sleep(300 * time.Millisecond)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer upstream.Close()

			members := []config.Member{
				{Product: "workbuddy", Model: "m1", Weight: 100, Enabled: true},
				{Product: "trae", Model: "m2", Weight: 90, Enabled: true},
				{Product: "workbuddy", Model: "m3", Weight: 80, Enabled: true},
				{Product: "trae", Model: "m4", Weight: 70, Enabled: true},
			}
			fx := newFixture(t, upstream.URL, members)
			logger := reqlog.New(t.TempDir(), 1)
			defer logger.Close()
			handler := New(fx.store).WithLogger(logger)

			body, _ := json.Marshal(map[string]interface{}{
				"model": "gpt-5", "stream": stream, "messages": []interface{}{},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+fx.plain)
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			req = req.WithContext(ctx)
			go func() {
				time.Sleep(100 * time.Millisecond)
				cancel()
			}()

			recorder := httptest.NewRecorder()
			handler.ChatCompletions(recorder, req)

			if recorder.Code != statusClientClosed {
				t.Fatalf("客户端断开应记 499，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
			if got := atomic.LoadInt64(&calls); got != 1 {
				t.Fatalf("客户端断开后不应继续降级，实际下游调用 %d 次", got)
			}
			for _, member := range members {
				key := config.MemberKey("gpt-5", member)
				if snap := fx.store.State().Snapshot(key); snap.CooldownUntil != "" || snap.BreakerUntil != "" {
					t.Fatalf("客户端断开不应冷却成员 %s：%+v", member.Model, snap)
				}
			}
			entries := logger.Recent(1)
			if len(entries) != 1 || entries[0].Outcome != "client_canceled" {
				t.Fatalf("日志应记 client_canceled，实际 %+v", entries)
			}
		})
	}
}
