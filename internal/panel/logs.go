package panel

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/reqlog"
)

const (
	// 默认与上限都是 100：日志按天扫全量文件后在内存里排序，
	// 单页过大既拖慢面板，也失去分页意义。
	defaultPageSize = 100
	maxPageSize     = 100
)

// WithLogs 指定日志目录（为空时查询接口返回空集）。
func (s *Server) WithLogs(dir string) *Server {
	s.logsDir = dir
	return s
}

func (s *Server) logsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	query := r.URL.Query()
	from := parseDay(query.Get("from"), time.Now().AddDate(0, 0, -1))
	to := parseDay(query.Get("to"), time.Now())
	filter := logFilter{
		Outcome: strings.TrimSpace(query.Get("outcome")),
		Product: strings.TrimSpace(query.Get("product")),
		Group:   strings.TrimSpace(query.Get("group")),
		Token:   strings.TrimSpace(query.Get("token")),
		Keyword: strings.TrimSpace(query.Get("q")),
		Status:  atoi(query.Get("status"), 0),
	}

	entries := s.queryLogs(from, to, filter)
	page := atoi(query.Get("page"), 1)
	size := atoi(query.Get("size"), defaultPageSize)
	if page < 1 {
		page = 1
	}
	// 超上限时夹到上限而不是回落默认值：显式传 size=200 应拿到 100 条。
	if size < 1 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	total := len(entries)
	pages := 0
	if total > 0 {
		pages = (total + size - 1) / size
	}
	if pages > 0 && page > pages {
		page = pages // 数据变少时把越界页夹回最后一页，避免返回空列表
	}
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total": total, "page": page, "size": size, "pages": pages,
		"data": entries[start:end],
	})
}

func (s *Server) logsOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	today := time.Now()
	entries := s.queryLogs(today, today, logFilter{})
	total := len(entries)
	okCount := 0
	downgraded := 0
	durations := make([]int64, 0, total)
	for _, entry := range entries {
		if entry.Outcome == "ok" {
			okCount++
		}
		if len(entry.Attempts) > 1 {
			downgraded++
		}
		durations = append(durations, entry.DurationMs)
	}
	rate := 0.0
	if total > 0 {
		rate = float64(okCount) / float64(total)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"today_requests": total, "success_rate": rate, "downgraded": downgraded,
		"p95_ms": p95(durations), "dropped": s.droppedCount(),
	})
}

// p95 直接对耗时排序取分位；overview 只跑当日数据，成本可控。
func p95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(float64(len(sorted)) * 0.95)
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func (s *Server) droppedCount() int64 {
	if s.logger == nil {
		return 0
	}
	return s.logger.Dropped()
}

// logFilter 是日志查询的过滤条件；空值表示不限制。
type logFilter struct {
	Outcome string
	Product string
	Group   string
	Token   string
	Keyword string
	Status  int
}

func (f logFilter) match(entry reqlog.Entry) bool {
	if f.Outcome == "downgraded" {
		if len(entry.Attempts) <= 1 {
			return false
		}
	} else if f.Outcome != "" && entry.Outcome != f.Outcome {
		return false
	}
	if f.Product != "" && entry.Product != f.Product {
		return false
	}
	if f.Group != "" && entry.Group != f.Group {
		return false
	}
	if f.Status > 0 && entry.HTTPStatus != f.Status {
		return false
	}
	if f.Token != "" && !strings.Contains(entry.TokenName+entry.TokenID, f.Token) {
		return false
	}
	if f.Keyword != "" && !strings.Contains(entry.Group+entry.Model+entry.TokenName+entry.RequestID, f.Keyword) {
		return false
	}
	return true
}

// queryLogs 扫描日期范围内的按天文件，按时间倒序返回匹配项。
func (s *Server) queryLogs(from, to time.Time, filter logFilter) []reqlog.Entry {
	if s.logsDir == "" {
		return nil
	}
	names, err := os.ReadDir(s.logsDir)
	if err != nil {
		return nil
	}
	wanted := map[string]struct{}{}
	for day := truncateDay(from); !day.After(truncateDay(to)); day = day.AddDate(0, 0, 1) {
		wanted["requests-"+day.Format("20060102")+".jsonl"] = struct{}{}
	}

	entries := make([]reqlog.Entry, 0, 128)
	for _, name := range names {
		if _, ok := wanted[name.Name()]; !ok {
			continue
		}
		entries = append(entries, readEntries(filepath.Join(s.logsDir, name.Name()))...)
	}
	filtered := entries[:0]
	for _, entry := range entries {
		if filter.match(entry) {
			filtered = append(filtered, entry)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Time > filtered[j].Time })
	return filtered
}

func readEntries(path string) []reqlog.Entry {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	out := make([]reqlog.Entry, 0, 64)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry reqlog.Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		out = append(out, entry)
	}
	// 读文件途中出错（截断 / I/O 错误）不能当成「正常读完」：
	// 保留已成功解析的部分，避免把残缺结果当作完整分页数据。
	if err := scanner.Err(); err != nil {
		return out
	}
	return out
}

func parseDay(raw string, fallback time.Time) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	for _, layout := range []string{"2006-01-02", config.TimeLayout} {
		if parsed, err := time.ParseInLocation(layout, raw, config.Location()); err == nil {
			return parsed
		}
	}
	return fallback
}

func truncateDay(value time.Time) time.Time {
	local := value.In(config.Location())
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, config.Location())
}

func atoi(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}
