package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"model-gateway/internal/config"
)

// at 构造一个按 config.Location() 解释的时间点，避免测试跑在非 UTC+8 机器上时跨天。
func at(year int, month time.Month, day, hour int) time.Time {
	return time.Date(year, month, day, hour, 0, 0, 0, config.Location())
}

func assertNear(t *testing.T, label string, got, want float64) {
	t.Helper()
	if diff := got - want; diff > 0.01 || diff < -0.01 {
		t.Fatalf("%s 期望 %.2f，实际 %.2f", label, want, got)
	}
}

// 一次成功 + 一次成功 + 一次失败：请求数、成功率、token、缓存命中率、平均耗时都要对。
func TestSnapshotAggregatesTotals(t *testing.T) {
	recorder := New("")
	// 固定「现在」：否则数据日期一旦超出近 7 天窗口，这个用例会在真实时间流过后自己失败。
	recorder.now = func() time.Time { return at(2026, time.November, 6, 12) }
	now := at(2026, time.November, 6, 10)
	recorder.Add(now, "workbuddy", "glm-5.3", Delta{
		Tokens: true, PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200,
		CacheHitTokens: 800, LatencyMs: 100, HasLatency: true,
	}, true)
	recorder.Add(now, "workbuddy", "glm-5.3", Delta{
		Tokens: true, PromptTokens: 500, CompletionTokens: 100, TotalTokens: 600,
		LatencyMs: 300, HasLatency: true,
	}, true)
	// 失败请求：没有 token，也不该把 5s 的失败耗时算进「成功请求平均耗时」。
	recorder.Add(now, "trae", "glm-5.3", Delta{LatencyMs: 5000, HasLatency: true}, false)

	snapshot := recorder.Snapshot(Window{Days: 7})
	totals := snapshot.Totals
	if totals.Requests != 3 || totals.Errors != 1 {
		t.Fatalf("请求/失败数不对：%+v", totals)
	}
	assertNear(t, "成功率", totals.SuccessRate, 200.0/3)
	if totals.PromptTokens != 1500 || totals.CompletionTokens != 300 || totals.TotalTokens != 1800 {
		t.Fatalf("token 合计不对：%+v", totals)
	}
	if totals.CacheHitTokens != 800 || totals.CacheMissTokens != 700 {
		t.Fatalf("缓存命中/未命中不对：%+v", totals)
	}
	assertNear(t, "缓存命中率", totals.CacheHitRate, 800.0/1500*100)
	assertNear(t, "平均耗时", totals.AvgLatencyMs, 200)
	if len(snapshot.ByProductModel) != 2 {
		t.Fatalf("应按产品+模型分成 2 行，实际 %d", len(snapshot.ByProductModel))
	}
	// Top 按 token 降序：workbuddy 有 token，trae 只有失败请求。
	if snapshot.ByProductModel[0].Product != "workbuddy" || snapshot.ByProductModel[0].TotalTokens != 1800 {
		t.Fatalf("Top 排序不对：%+v", snapshot.ByProductModel[0])
	}
	if snapshot.ByProductModel[1].Requests != 1 || snapshot.ByProductModel[1].Errors != 1 {
		t.Fatalf("失败行应保留请求数与失败数：%+v", snapshot.ByProductModel[1])
	}
}

// 近 7 天必须是 7 个连续的点：中间没请求的日期补 0，否则横轴会被压缩成几根挤在一起的柱子。
func TestSnapshotFillsMissingDays(t *testing.T) {
	recorder := New("")
	recorder.now = func() time.Time { return at(2026, time.November, 6, 12) }
	recorder.Add(at(2026, time.November, 2, 9), "workbuddy", "glm-5.3",
		Delta{Tokens: true, TotalTokens: 10}, true)
	recorder.Add(at(2026, time.November, 6, 9), "workbuddy", "glm-5.3",
		Delta{Tokens: true, TotalTokens: 20}, true)

	snapshot := recorder.Snapshot(Window{Days: 7})
	if len(snapshot.Series) != 7 {
		t.Fatalf("近 7 天应输出 7 个点，实际 %d：%+v", len(snapshot.Series), snapshot.Series)
	}
	if snapshot.Series[0].Date != "2026-10-31" || snapshot.Series[6].Date != "2026-11-06" {
		t.Fatalf("日期区间不对：%s ~ %s", snapshot.Series[0].Date, snapshot.Series[6].Date)
	}
	if snapshot.Series[0].Requests != 0 {
		t.Fatalf("空日期应补 0 点：%+v", snapshot.Series[0])
	}
	if snapshot.Series[6].TotalTokens != 20 {
		t.Fatalf("今天应累计 20 token：%+v", snapshot.Series[6])
	}
	// 同一小时两次请求也要按天归并成一天，而不是各出一个点。
	if snapshot.Series[2].Date != "2026-11-02" || snapshot.Series[2].TotalTokens != 10 {
		t.Fatalf("11-02 应累计 10 token：%+v", snapshot.Series[2])
	}
}

// 小时桶到期折叠成日桶后，长期趋势（全部历史）仍然读得到。
func TestRollupKeepsLongTermTrend(t *testing.T) {
	recorder := New("")
	recorder.Add(at(2026, time.May, 1, 3), "workbuddy", "glm-5.3", Delta{
		Tokens: true, PromptTokens: 60, CompletionTokens: 40, TotalTokens: 100,
		CacheHitTokens: 30, LatencyMs: 500, HasLatency: true,
	}, true)
	recorder.Rollup(at(2026, time.November, 6, 0))

	recorder.mu.Lock()
	if len(recorder.buckets) != 1 {
		recorder.mu.Unlock()
		t.Fatalf("折叠后应只剩 1 个日桶，实际 %d", len(recorder.buckets))
	}
	for _, item := range recorder.buckets {
		if item.Scope != "d:2026-05-01" {
			recorder.mu.Unlock()
			t.Fatalf("折叠后的 scope 不对：%s", item.Scope)
		}
	}
	recorder.mu.Unlock()

	snapshot := recorder.Snapshot(Window{})
	if snapshot.Totals.TotalTokens != 100 || snapshot.Totals.CacheHitTokens != 30 {
		t.Fatalf("折叠不能丢数据：%+v", snapshot.Totals)
	}
	if len(snapshot.Series) != 1 || snapshot.Series[0].Date != "2026-05-01" {
		t.Fatalf("折叠后趋势应按天输出：%+v", snapshot.Series)
	}
}

// 重启不丢：落盘后重新打开，累计量一致；正文被截断时回退 .bak。
func TestPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	recorder := New(path)
	recorder.Add(at(2026, time.November, 5, 8), "workbuddy", "glm-5.3", Delta{
		Tokens: true, TotalTokens: 123, LatencyMs: 80, HasLatency: true,
	}, true)
	recorder.Save()
	// 第二次落盘才会写出 .bak（保留上一版）。
	recorder.Add(at(2026, time.November, 5, 9), "workbuddy", "glm-5.3",
		Delta{Tokens: true, TotalTokens: 77}, true)
	recorder.Save()

	reopened := New(path)
	if got := reopened.Snapshot(Window{}).Totals.TotalTokens; got != 200 {
		t.Fatalf("重开后 token 应恢复为 200，实际 %d", got)
	}

	if err := os.WriteFile(path, []byte(`{"version":1,"buckets":[`), 0o644); err != nil {
		t.Fatalf("构造损坏正文失败：%v", err)
	}
	fromBackup := New(path)
	if got := fromBackup.Snapshot(Window{}).Totals.TotalTokens; got != 123 {
		t.Fatalf("正文损坏时应回退 .bak（123），实际 %d", got)
	}
}

// 显式区间按天切，to 为开区间：统计 11-05 当天不含 11-06。
func TestSnapshotExplicitWindow(t *testing.T) {
	recorder := New("")
	recorder.Add(at(2026, time.November, 4, 23), "workbuddy", "glm-5.3", Delta{Tokens: true, TotalTokens: 1}, true)
	recorder.Add(at(2026, time.November, 5, 1), "workbuddy", "glm-5.3", Delta{Tokens: true, TotalTokens: 2}, true)
	recorder.Add(at(2026, time.November, 6, 1), "workbuddy", "glm-5.3", Delta{Tokens: true, TotalTokens: 4}, true)

	snapshot := recorder.Snapshot(Window{
		From: at(2026, time.November, 5, 0),
		To:   at(2026, time.November, 6, 0),
	})
	if snapshot.Totals.Requests != 1 || snapshot.Totals.TotalTokens != 2 {
		t.Fatalf("显式区间应只含 11-05：%+v", snapshot.Totals)
	}
	if snapshot.WindowFrom == "" || snapshot.WindowTo == "" {
		t.Fatalf("显式区间应回显生效范围：%+v", snapshot)
	}
}

// 上游字段矛盾（命中数 > prompt）时命中率不能超过 100%。
func TestCacheHitClamped(t *testing.T) {
	recorder := New("")
	recorder.now = func() time.Time { return at(2026, time.November, 6, 12) }
	recorder.Add(at(2026, time.November, 6, 10), "workbuddy", "glm-5.3", Delta{
		Tokens: true, PromptTokens: 10, TotalTokens: 10, CacheHitTokens: 99,
	}, true)
	totals := recorder.Snapshot(Window{Days: 7}).Totals
	assertNear(t, "缓存命中率", totals.CacheHitRate, 100)
	if totals.CacheMissTokens != 0 {
		t.Fatalf("命中数截顶后未命中应为 0：%+v", totals)
	}
}

// nil 记录器必须安全：没接线时面板不该 panic。
func TestNilRecorderIsSafe(t *testing.T) {
	var recorder *Recorder
	recorder.Add(time.Now(), "p", "m", Delta{}, true)
	recorder.Rollup(time.Now())
	recorder.Save()
	recorder.Start()
	recorder.Stop()
	snapshot := recorder.Snapshot(Window{Days: 7})
	if snapshot.Totals.Requests != 0 || snapshot.Generated == "" {
		t.Fatalf("空记录器应返回空快照：%+v", snapshot)
	}
	if recorder.Describe() != "disabled" {
		t.Fatalf("空记录器摘要应为 disabled：%s", recorder.Describe())
	}
}
