package panel

import (
	"net/http"
	"strings"
	"time"

	"model-gateway/internal/pkg/usage"
)

const (
	// defaultUsageDays 是统计页的默认跨度：7 天，正好覆盖「最近一周每天用了多少 token」。
	defaultUsageDays = 7
	// maxUsageDays 只是防呆上限（10 年），避免异常参数把补空点的循环撑爆。
	maxUsageDays = 3660
)

// WithUsage 注入用量累计器（统计页数据源）；未注入时统计接口返回空快照。
// 与 WithLogs 的降级口径一致：宁可页面空着，也不要让「没接线」变成 500。
func (s *Server) WithUsage(recorder *usage.Recorder) *Server {
	s.usage = recorder
	return s
}

// usageSnapshot 返回统计页一次拉取的全部数据：卡片汇总、按天趋势、按产品+模型 Top。
//
// 参数二选一：
//   - days=N：近 N 天（含今天），默认 7；
//   - from=YYYY-MM-DD&to=YYYY-MM-DD：显式区间，两端都含当天。
//
// 两者同时给出时以显式区间为准（窗口优先级由 usage.Window 解析）。
func (s *Server) usageSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	query := r.URL.Query()
	window := usage.Window{Days: defaultUsageDays}
	// days 显式给 0 表示「全部历史」；不给才用默认 7 天。
	// 两者必须区分开：否则「全部」会被静默当成「近 7 天」，页面上看起来像筛选没生效。
	if raw := strings.TrimSpace(query.Get("days")); raw != "" {
		window.Days = atoi(raw, defaultUsageDays)
		if window.Days < 0 {
			window.Days = 0
		}
		if window.Days > maxUsageDays {
			window.Days = maxUsageDays
		}
	}
	if raw := strings.TrimSpace(query.Get("from")); raw != "" {
		window.From = parseDay(raw, time.Time{})
	}
	if raw := strings.TrimSpace(query.Get("to")); raw != "" {
		if day := parseDay(raw, time.Time{}); !day.IsZero() {
			// 前端传的 to 是「含当天」，累计器按开区间判，所以往后推一天。
			window.To = day.AddDate(0, 0, 1)
		}
	}
	writeJSON(w, http.StatusOK, s.usage.Snapshot(window))
}
