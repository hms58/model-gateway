package panel

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/reqlog"
)

const (
	// 默认与上限都是 100：单页过大既拖慢面板，也失去分页意义。
	defaultPageSize = 100
	maxPageSize     = 100
	// maxRetainedEntries 是单次查询在内存里最多保留的条数。
	// 一页最多 100 条，保留 2 万条足够覆盖 200 页以内的翻页；
	// 更深的页会按实际需要的条数重扫一次（见 logsList）。
	maxRetainedEntries = 20000
	// 提示词详情的分片口径：结构接口只回每条消息的预览，正文按 offset/limit 分片拉，
	// 一次几十万字符的 system 提示词不会把抽屉和浏览器一起卡死。
	promptPreviewRunes  = 400
	promptChunkRunes    = 4096
	promptChunkMaxRunes = 16384
	// maxRequestIDLen 限制 request_id 入参长度：详情接口只认本网关生成的短 id，
	// 不做任意长串的目录扫描。
	maxRequestIDLen = 64
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

	// 只保留「翻到这一页所需的最多条数」，而不是整份结果：
	// 单日可达百万条，全量保留后再排序会让一次翻页把常驻内存顶上去。
	need := page * size
	if need > maxRetainedEntries || need < size {
		need = maxRetainedEntries
	}
	window := newEntryWindow(need)
	total := s.scanLogs(from, to, filter, window.push)

	pages := 0
	if total > 0 {
		pages = (total + size - 1) / size
	}
	if pages > 0 && page > pages {
		page = pages // 数据变少时把越界页夹回最后一页，避免返回空列表
	}
	start := (page - 1) * size
	if end := start + size; end > window.len() && end <= total {
		// 深翻页时首轮只保留了上限内的条目：按实际需要的条数重扫一次。
		window = newEntryWindow(end)
		s.scanLogs(from, to, filter, window.push)
	}

	entries := window.newestFirst()
	if start > len(entries) {
		start = len(entries)
	}
	end := start + size
	if end > len(entries) {
		end = len(entries)
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
	total := 0
	okCount := 0
	downgraded := 0
	durations := make([]int64, 0, 256)
	// 流式累计而不是先取回整天的 entry 切片：概览只需要计数与耗时，
	// 保留完整条目会让每次自动刷新都把当日日志全量读进内存。
	s.scanLogs(today, today, logFilter{}, func(entry reqlog.Entry) {
		total++
		if entry.Outcome == "ok" {
			okCount++
		}
		if len(entry.Attempts) > 1 {
			downgraded++
		}
		durations = append(durations, entry.DurationMs)
	})
	rate := 0.0
	if total > 0 {
		rate = float64(okCount) / float64(total)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"today_requests": total, "success_rate": rate, "downgraded": downgraded,
		"p95_ms": p95(durations), "dropped": s.droppedCount(),
	})
}

// logPrompt 返回一次请求的提示词详情，分两级：
//
//	不带 message 参数：只回结构（每条消息的角色、原文长度、预览片段）；
//	带 message + offset/limit：只回这条消息的一段正文。
//
// 提示词动辄几十万字符，一次全量返回会让抽屉直接卡死，也把详情接口变成大文件下载。
func (s *Server) logPrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	query := r.URL.Query()
	requestID := strings.TrimSpace(query.Get("request_id"))
	if requestID == "" || len(requestID) > maxRequestIDLen {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "缺少合法的 request_id")
		return
	}
	prompt, found := reqlog.ReadPromptIn(s.logsDir, promptDayKey(query.Get("date")), requestID)
	if !found {
		// 不是 404：旧日志本来就没有提示词快照，前端要能平静地提示「这条没记录」。
		writeJSON(w, http.StatusOK, map[string]interface{}{"request_id": requestID, "found": false})
		return
	}
	if raw := strings.TrimSpace(query.Get("message")); raw != "" {
		writePromptChunk(w, prompt, raw, query)
		return
	}

	messages := make([]map[string]interface{}, 0, len(prompt.Messages))
	for index, message := range prompt.Messages {
		messages = append(messages, map[string]interface{}{
			"index": index, "role": message.Role, "chars": message.Chars,
			"stored_chars": utf8.RuneCountInString(message.Content),
			"truncated":    message.Truncated,
			"preview":      reqlog.ClipRunes(message.Content, promptPreviewRunes),
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"request_id": prompt.RequestID, "found": true, "time": prompt.Time, "group": prompt.Group,
		"chars": prompt.Chars, "stored_chars": prompt.Stored, "truncated": prompt.Truncated,
		// omitted 是无文本消息条数（工具调用等），前端汇总到头部，不渲染空块。
		"omitted":  prompt.Omitted,
		"messages": messages,
	})
}

// writePromptChunk 只回一条消息的一段正文：offset / limit 按字符（rune）计，
// 客户端拿 next_offset 继续拉，直到 done=true。
func writePromptChunk(w http.ResponseWriter, prompt reqlog.Prompt, rawIndex string, query url.Values) {
	index := atoi(rawIndex, -1)
	if index < 0 || index >= len(prompt.Messages) {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "message 越界")
		return
	}
	message := prompt.Messages[index]
	content := []rune(message.Content)
	stored := len(content)

	offset := atoi(query.Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	if offset > stored {
		offset = stored
	}
	limit := atoi(query.Get("limit"), promptChunkRunes)
	if limit < 1 {
		limit = promptChunkRunes
	}
	if limit > promptChunkMaxRunes {
		limit = promptChunkMaxRunes
	}
	end := offset + limit
	if end > stored {
		end = stored
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"request_id": prompt.RequestID, "index": index, "role": message.Role,
		"chars": message.Chars, "stored_chars": stored,
		"offset": offset, "end": end, "next_offset": end,
		"content": string(content[offset:end]),
		// done 只表示「这一段读完了」；原文本身没存全要看 truncated。
		"done": end >= stored, "truncated": message.Truncated,
	})
}

// promptDayKey 把面板传的日期（YYYY-MM-DD）转成日志文件名用的 YYYYMMDD。
// 传空或非法时回落到今天：绝大多数详情点击都发生在当天。
func promptDayKey(raw string) string {
	return truncateDay(parseDay(raw, time.Now())).Format("20060102")
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
	if f.Keyword != "" {
		haystack := entry.Group + entry.Model + entry.TokenName + entry.RequestID
		// 提示词摘要里的片段也参与关键字匹配：日志列表里能直接按「问了什么」搜。
		if entry.Prompt != nil {
			haystack += entry.Prompt.Preview
		}
		if !strings.Contains(haystack, f.Keyword) {
			return false
		}
	}
	return true
}

// scanLogs 按写入顺序（旧 → 新）逐条回调命中 filter 的日志，返回命中总数。
//
// 逐条回调而不是整批返回：调用方只关心计数或最近 N 条，整批驻留内存没有意义。
func (s *Server) scanLogs(from, to time.Time, filter logFilter, visit func(reqlog.Entry)) int {
	count := 0
	for _, path := range s.logFiles(from, to) {
		readEntries(path, func(entry reqlog.Entry) {
			if !filter.match(entry) {
				return
			}
			count++
			visit(entry)
		})
	}
	return count
}

// logFiles 返回日期范围内的日志文件，按「日期升序 + 同日分片序号升序」排列。
//
// 这个顺序就是时间序：日志是追加写的，文件内行序也是时间序，
// 因此「最新一页」可以在一次顺序扫描里用定长缓冲拿到，不必全量读取后再排序。
func (s *Server) logFiles(from, to time.Time) []string {
	if s.logsDir == "" {
		return nil
	}
	names, err := os.ReadDir(s.logsDir)
	if err != nil {
		return nil
	}
	wanted := make(map[string]struct{}, 8)
	for day := truncateDay(from); !day.After(truncateDay(to)); day = day.AddDate(0, 0, 1) {
		wanted[day.Format("20060102")] = struct{}{}
	}

	type logFile struct {
		path string
		day  string
		part int
	}
	files := make([]logFile, 0, len(names))
	for _, name := range names {
		if name.IsDir() {
			continue
		}
		day, ok := reqlog.DayOfFile(name.Name())
		if !ok {
			continue
		}
		if _, want := wanted[day]; !want {
			continue
		}
		part, ok := reqlog.PartOfFile(name.Name())
		if !ok {
			continue
		}
		files = append(files, logFile{path: filepath.Join(s.logsDir, name.Name()), day: day, part: part})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].day != files[j].day {
			return files[i].day < files[j].day
		}
		return files[i].part < files[j].part
	})
	paths := make([]string, 0, len(files))
	for _, item := range files {
		paths = append(paths, item.path)
	}
	return paths
}

// entryWindow 是只保留最近 limit 条的定长环形缓冲。
// 日志按时间顺序读入，所以直接覆盖最旧的一条即可得到「最新 limit 条」。
type entryWindow struct {
	items []reqlog.Entry
	limit int
	next  int
	full  bool
}

func newEntryWindow(limit int) *entryWindow {
	if limit < 1 {
		limit = 1
	}
	return &entryWindow{items: make([]reqlog.Entry, 0, limit), limit: limit}
}

func (w *entryWindow) push(entry reqlog.Entry) {
	if len(w.items) < w.limit {
		w.items = append(w.items, entry)
		w.next = len(w.items) % w.limit
		return
	}
	w.items[w.next] = entry
	w.next = (w.next + 1) % w.limit
	w.full = true
}

func (w *entryWindow) len() int { return len(w.items) }

// newestFirst 返回窗口内按时间倒序（新 → 旧）的条目。
func (w *entryWindow) newestFirst() []reqlog.Entry {
	out := make([]reqlog.Entry, 0, len(w.items))
	if !w.full {
		for i := len(w.items) - 1; i >= 0; i-- {
			out = append(out, w.items[i])
		}
		return out
	}
	for i := 0; i < len(w.items); i++ {
		out = append(out, w.items[(w.next+i)%len(w.items)])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// readEntries 逐行解析并回调；解析失败的行跳过，不打断整份文件。
func readEntries(path string, visit func(reqlog.Entry)) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

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
		visit(entry)
	}
	// 读文件途中出错（截断 / I/O 错误）时保留已解析部分：
	// 已有的结果仍然可用，残缺部分不会凭空产生条目。
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
