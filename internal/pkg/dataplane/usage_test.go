package dataplane

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/usage"
)

// 成功请求要进用量累计器（token / 缓存命中 / 耗时），并按「实际命中的产品+模型」归行。
func TestUsageRecorderCapturesServedRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":60}}}`))
	}))
	defer upstream.Close()

	fx := newFixture(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "gpt-5-real", Weight: 100, Enabled: true},
	})
	recorder := usage.New("")
	handler := New(fx.store).WithUsage(recorder)

	if got := doChat(t, handler, fx.plain); got.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", got.Code, got.Body.String())
	}

	snapshot := recorder.Snapshot(usage.Window{Days: 1})
	totals := snapshot.Totals
	if totals.Requests != 1 || totals.Errors != 0 {
		t.Fatalf("请求/失败数不对：%+v", totals)
	}
	if totals.PromptTokens != 100 || totals.CompletionTokens != 20 || totals.TotalTokens != 120 {
		t.Fatalf("token 不对：%+v", totals)
	}
	// 未命中 = prompt - 命中；命中率才能算出来。
	if totals.CacheHitTokens != 60 || totals.CacheMissTokens != 40 {
		t.Fatalf("缓存命中不对：%+v", totals)
	}
	if len(snapshot.ByProductModel) != 1 {
		t.Fatalf("应有一行产品+模型：%+v", snapshot.ByProductModel)
	}
	row := snapshot.ByProductModel[0]
	if row.Product != "workbuddy" || row.Model != "gpt-5-real" {
		t.Fatalf("应记录实际命中的成员：%+v", row)
	}
}

// 上游整体失败：计请求数与失败数，但不产生 token，也不采样耗时（否则平均耗时会被超时拉爆）。
func TestUsageRecorderCountsFailuresWithoutTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota"}}`))
	}))
	defer upstream.Close()

	fx := newFixture(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "gpt-5-real", Weight: 100, Enabled: true},
	})
	recorder := usage.New("")
	handler := New(fx.store).WithUsage(recorder)

	if got := doChat(t, handler, fx.plain); got.Code == http.StatusOK {
		t.Fatalf("上游 429 不该返回 200：%s", got.Body.String())
	}
	totals := recorder.Snapshot(usage.Window{Days: 1}).Totals
	if totals.Requests != 1 || totals.Errors != 1 {
		t.Fatalf("失败请求也要计入请求数：%+v", totals)
	}
	if totals.TotalTokens != 0 || totals.AvgLatencyMs != 0 {
		t.Fatalf("失败请求不该产生 token 与耗时样本：%+v", totals)
	}
}
