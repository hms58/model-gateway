package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"model-gateway/internal/pkg/usage"
)

type usagePayload struct {
	Totals struct {
		Requests        int64   `json:"requests"`
		Errors          int64   `json:"errors"`
		SuccessRate     float64 `json:"success_rate"`
		TotalTokens     int64   `json:"total_tokens"`
		CacheHitTokens  int64   `json:"cache_hit_tokens"`
		CacheMissTokens int64   `json:"cache_miss_tokens"`
		CacheHitRate    float64 `json:"cache_hit_rate"`
		AvgLatencyMs    float64 `json:"avg_latency_ms"`
	} `json:"totals"`
	Series []struct {
		Date        string `json:"date"`
		Requests    int64  `json:"requests"`
		TotalTokens int64  `json:"total_tokens"`
	} `json:"series"`
	ByProductModel []struct {
		Product     string `json:"product"`
		Model       string `json:"model"`
		Requests    int64  `json:"requests"`
		TotalTokens int64  `json:"total_tokens"`
	} `json:"by_product_model"`
}

func decodeUsage(t *testing.T, path string, handler http.Handler) usagePayload {
	t.Helper()
	recorder := call(t, handler, http.MethodGet, path, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s 期望 200，实际 %d：%s", path, recorder.Code, recorder.Body.String())
	}
	var payload usagePayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("%s 响应不是合法 JSON：%v", path, err)
	}
	return payload
}

// 统计接口要把卡片汇总、按天趋势、按产品+模型 Top 一次给全。
func TestUsageEndpointReturnsAggregates(t *testing.T) {
	server, _ := newServer(t)
	recorder := usage.New("")
	now := time.Now()
	recorder.Add(now, "workbuddy", "gpt-5", usage.Delta{
		Tokens: true, PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120,
		CacheHitTokens: 60, LatencyMs: 250, HasLatency: true,
	}, true)
	recorder.Add(now, "trae", "gpt-5", usage.Delta{}, false)

	payload := decodeUsage(t, "/api/modelgw/usage?days=7", server.WithUsage(recorder).Handler())
	if payload.Totals.Requests != 2 || payload.Totals.Errors != 1 {
		t.Fatalf("请求/失败数不对：%+v", payload.Totals)
	}
	if payload.Totals.TotalTokens != 120 || payload.Totals.CacheHitTokens != 60 {
		t.Fatalf("token 与缓存命中不对：%+v", payload.Totals)
	}
	// 命中率 = 命中 / prompt = 60/100；成功率 = 1/2。
	if payload.Totals.CacheHitRate != 60 || payload.Totals.SuccessRate != 50 {
		t.Fatalf("比率口径不对：%+v", payload.Totals)
	}
	if len(payload.Series) != 7 {
		t.Fatalf("days=7 应返回 7 个按天点，实际 %d", len(payload.Series))
	}
	if len(payload.ByProductModel) != 2 {
		t.Fatalf("Top 应按产品+模型分成 2 行，实际 %d", len(payload.ByProductModel))
	}
	// 有 token 的行排前面；纯失败行也要保留请求数。
	if payload.ByProductModel[0].Product != "workbuddy" || payload.ByProductModel[0].TotalTokens != 120 {
		t.Fatalf("Top 排序不对：%+v", payload.ByProductModel[0])
	}
	if payload.ByProductModel[1].Requests != 1 || payload.ByProductModel[1].TotalTokens != 0 {
		t.Fatalf("失败行应只有请求数：%+v", payload.ByProductModel[1])
	}
}

// 没接线时统计接口返回空快照而不是 500：宁可页面空着，也不要让面板整页报错。
func TestUsageEndpointWithoutRecorder(t *testing.T) {
	server, _ := newServer(t)
	payload := decodeUsage(t, "/api/modelgw/usage", server.Handler())
	if payload.Totals.Requests != 0 || len(payload.Series) != 0 {
		t.Fatalf("未注入记录器时应返回空快照：%+v", payload)
	}
}

// days 显式给 0 表示全部历史，不能被静默当成默认 7 天（否则「全部」看起来像筛选没生效）。
func TestUsageEndpointAllHistory(t *testing.T) {
	server, _ := newServer(t)
	recorder := usage.New("")
	recorder.Add(time.Now().AddDate(0, 0, -40), "workbuddy", "gpt-5",
		usage.Delta{Tokens: true, TotalTokens: 33}, true)

	handler := server.WithUsage(recorder).Handler()
	recent := decodeUsage(t, "/api/modelgw/usage?days=7", handler)
	if recent.Totals.TotalTokens != 0 {
		t.Fatalf("近 7 天不该包含 40 天前的数据：%+v", recent.Totals)
	}
	all := decodeUsage(t, "/api/modelgw/usage?days=0", handler)
	if all.Totals.TotalTokens != 33 {
		t.Fatalf("days=0 应统计全部历史：%+v", all.Totals)
	}
}

// 统计页必须是导航里可点的一页，且带齐三块视图的容器。
func TestPanelExposesUsageView(t *testing.T) {
	server, _ := newServer(t)
	recorder := call(t, server.Handler(), http.MethodGet, "/panel/usage", nil)
	body := recorder.Body.String()
	for _, want := range []string{
		`data-view="usage"`, `id="usage"`, `id="usage-kpis"`, `id="usage-chart"`,
		`id="usage-top-body"`, "function loadUsage", "function renderUsageChart",
		// 按天明细已并入柱子悬停提示，指标仍要能在提示里找到。
		"function pctPlain", "平均耗时 ", "缓存命中 ",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("统计页缺少 %q", want)
		}
	}
	// 按天明细表刻意移除：token 分量由柱体表达，再长一张表就是把同一批数字显示两遍。
	if strings.Contains(body, "usage-days-body") {
		t.Fatalf("按天明细表应已移除（指标改挂柱子悬停提示）")
	}
}
