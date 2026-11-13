package reqlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"model-gateway/internal/config"
)

// 提示词记录的截断口径。
//
// PromptLimitChars 是单条请求落盘的字符上限（按 rune 计）：一次请求的提示词动辄几十万字符，
// 不截断会让日志目录被单条请求吃掉几十 MB；PromptPreviewChars 是列表里展示的片段长度，
// 详情页按消息分片拉取，不会一次性把整段提示词塞进响应。
const (
	PromptLimitChars   = 16 * 1024
	PromptPreviewChars = 200
)

// Entry 是一条请求日志；字段与方案 §4.6 对齐，按天落 JSONL。
// 不落响应正文与任何凭据；ClientIP / UA 是调用来源，便于回答「这条请求是谁打进来的」。
//
// 提示词只落摘要（Prompt）：完整内容另存 prompts-YYYYMMDD.jsonl，由详情接口按 request_id 读取。
// 列表一次 100 条，如果每条都带上百 KB 提示词，一次翻页就是几十 MB。
type Entry struct {
	Time      string `json:"time"`
	RequestID string `json:"request_id,omitempty"`
	// ClientRequestID 是客户端自己带的请求 id（X-Request-Id / X-Trace-Id 等），
	// 便于把网关日志与上游或客户端日志对上；客户端没带就为空。
	ClientRequestID  string    `json:"client_request_id,omitempty"`
	TokenID          string    `json:"token_id,omitempty"`
	TokenName        string    `json:"token_name,omitempty"`
	Group            string    `json:"group"`
	Product          string    `json:"product"`
	Model            string    `json:"model"`
	Attempts         []Attempt `json:"attempts,omitempty"`
	Stream           bool      `json:"stream"`
	HTTPStatus       int       `json:"http_status"`
	Outcome          string    `json:"outcome"`
	TTFBMs           int64     `json:"ttfb_ms"`
	DurationMs       int64     `json:"duration_ms"`
	PromptTokens     int       `json:"prompt_tokens,omitempty"`
	CompletionTokens int       `json:"completion_tokens,omitempty"`
	TotalTokens      int       `json:"total_tokens,omitempty"`
	// CacheHitTokens 是上游前缀缓存命中的 prompt token 数，CacheHitRate 由它推算。
	// 上游未提供该维度时两者省略（0 值），不要读成「命中率为 0」。
	CacheHitTokens int64   `json:"cache_hit_tokens,omitempty"`
	CacheHitRate   float64 `json:"cache_hit_rate,omitempty"`
	// Credit 是本次消耗的积分；CreditKnown 区分「上游没给」与「消耗 0」。
	Credit      float64 `json:"credit,omitempty"`
	CreditKnown bool    `json:"credit_known,omitempty"`
	// UpstreamError 是最后一次「上游拒绝请求」（400/422）的响应摘要，截断后落盘。
	// 不同上游对同一份请求的宽容度不同，这条摘要用来回答「哪个上游在拒什么」。
	UpstreamError string `json:"upstream_error,omitempty"`
	ClientIP      string `json:"client_ip,omitempty"`
	UA            string `json:"ua,omitempty"`
	// Prompt 是提示词摘要：列表接口只回这一小段，完整内容走详情接口。
	Prompt *PromptSummary `json:"prompt,omitempty"`
}

type Attempt struct {
	Product string `json:"product"`
	Model   string `json:"model"`
	Status  int    `json:"status"`
	Code    string `json:"code,omitempty"`
	MS      int64  `json:"ms"`
}

// PromptSummary 是随请求日志落盘的提示词摘要。
// 字段刻意保持小：列表页一次回 100 条，摘要只占几十字节。
type PromptSummary struct {
	// Messages 恒为 1：只记本次用户输入；Chars 是输入的原文字符数（按 rune 计）。
	Messages int `json:"messages"`
	Chars    int `json:"chars"`
	// Omitted 是历史消息条数（含历史提问、system / assistant / tool），都不进详情。
	Omitted int `json:"omitted,omitempty"`
	// Preview 是列表里展示的片段（通常取自最后一条 user 消息）。
	Preview string `json:"preview,omitempty"`
	// Truncated 表示原文超过落盘上限，详情看到的内容不完整。
	Truncated bool `json:"truncated,omitempty"`
	// Stored 表示完整快照已经落盘、可以点详情；队列满被丢弃时为 false。
	Stored bool `json:"stored,omitempty"`
}

// Prompt 是一次请求的提示词快照；只存角色与文本，不存响应与凭据。
type Prompt struct {
	RequestID string          `json:"request_id"`
	Time      string          `json:"time"`
	Group     string          `json:"group,omitempty"`
	Messages  []PromptMessage `json:"messages,omitempty"`
	// Omitted 是历史消息条数（含历史提问与调用链），语义同 PromptSummary.Omitted。
	Omitted int `json:"omitted,omitempty"`
	// Chars 是原文总字符数，Stored 是实际落盘的字符数，Truncated 表示被上限截断。
	Chars     int  `json:"chars"`
	Stored    int  `json:"stored"`
	Truncated bool `json:"truncated,omitempty"`
}

// PromptMessage 是一条消息的快照：Chars 是原文长度，Content 可能只是前一段（Truncated）。
type PromptMessage struct {
	Role      string `json:"role"`
	Chars     int    `json:"chars"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

const RingSize = 1000

// promptQueueSize 比请求日志队列浅：提示词单条可达数十 KB，
// 队列深度直接按「条数 × 单条体积」放大常驻内存。
const promptQueueSize = 256

// filePrefix / promptPrefix / fileSuffix 定义按天日志文件名：
// 请求日志 requests-YYYYMMDD.jsonl，提示词快照 prompts-YYYYMMDD.jsonl；
// 超过 max_file_mb 时同一日期续写 .1.jsonl、.2 …
const (
	filePrefix   = "requests-"
	promptPrefix = "prompts-"
	fileSuffix   = ".jsonl"
	dayLayout    = "20060102"
)

// Logger 用有界队列 + 单写 goroutine：转发路径只入队，满了丢弃并计数，绝不阻塞。
type Logger struct {
	dir       string
	retention int
	// maxBytes 是单个日志文件的大小上限（0 表示不滚动），来自 logging.max_file_mb。
	maxBytes int64
	now      func() time.Time
	ring     []Entry
	next     int
	mu       sync.RWMutex
	ch       chan Entry
	promptCh chan Prompt
	flush    chan chan struct{}
	stop     chan struct{}
	wg       sync.WaitGroup
	dropped  int64
	started  bool

	// 以下字段只由写 goroutine 访问，无需加锁。
	requests *fileWriter
	prompts  *fileWriter
}

func New(dir string, retentionDays int) *Logger {
	return newLogger(dir, retentionDays, 0, 1024, true)
}

// WithMaxFileMB 设置单个日志文件的大小上限；<=0 表示不滚动（保持按天单文件）。
func (l *Logger) WithMaxFileMB(mb int) *Logger {
	if l == nil {
		return nil
	}
	if mb > 0 {
		// 原子写：两个 fileWriter 都在自己的 goroutine 里读这个上限。
		atomic.StoreInt64(&l.maxBytes, int64(mb)<<20)
	}
	return l
}

func newLogger(dir string, retentionDays int, maxBytes int64, buffer int, startWorker bool) *Logger {
	now := time.Now
	logger := &Logger{
		dir:       dir,
		retention: retentionDays,
		maxBytes:  maxBytes,
		now:       now,
		ring:      make([]Entry, 0, RingSize),
		ch:        make(chan Entry, buffer),
		promptCh:  make(chan Prompt, promptQueueSize),
		flush:     make(chan chan struct{}),
		stop:      make(chan struct{}),
	}
	// 两个文件族共用同一个上限：提示词文件同样按体积滚动，
	// 否则单日提示词会攒成一个几百 MB、删也删不动的文件。
	logger.requests = newFileWriter(dir, filePrefix, &logger.maxBytes, now)
	logger.prompts = newFileWriter(dir, promptPrefix, &logger.maxBytes, now)
	if startWorker {
		logger.started = true
		logger.wg.Add(1)
		go logger.loop()
	}
	return logger
}

// Write 非阻塞入队；队列满则丢弃并累加计数。
func (l *Logger) Write(entry Entry) {
	if entry.Time == "" {
		entry.Time = l.now().In(config.Location()).Format(config.TimeLayout)
	}
	select {
	case l.ch <- entry:
		l.pushRing(entry)
	default:
		atomic.AddInt64(&l.dropped, 1)
	}
}

// WritePrompt 非阻塞入队一条提示词快照；返回 false 表示没写成功（未启用目录或队列满）。
// 返回值让调用方如实标注摘要里的 stored，避免面板给一个点不开的「详情」。
func (l *Logger) WritePrompt(prompt Prompt) bool {
	if l == nil || l.dir == "" {
		return false
	}
	select {
	case l.promptCh <- prompt:
		return true
	default:
		atomic.AddInt64(&l.dropped, 1)
		return false
	}
}

// Dropped 返回被丢弃的条数。
func (l *Logger) Dropped() int64 { return atomic.LoadInt64(&l.dropped) }

// Recent 返回内存 ring 中最近的 n 条（新到旧）。
func (l *Logger) Recent(n int) []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if total := len(l.ring); total > 0 {
		if n <= 0 || n > total {
			n = total
		}
		out := make([]Entry, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, l.ring[(l.next-1-i+total*2)%total])
		}
		return out
	}
	return nil
}

// Flush 等待队列写完（测试与退出前调用）。
func (l *Logger) Flush() {
	if !l.started {
		return
	}
	done := make(chan struct{})
	select {
	case l.flush <- done:
		<-done
	case <-l.stop:
	}
}

// Close 停止写协程并做最后一次清理。
func (l *Logger) Close() {
	if !l.started {
		return
	}
	close(l.stop)
	l.wg.Wait()
	l.started = false
}

func (l *Logger) loop() {
	defer l.wg.Done()
	defer func() {
		l.requests.close()
		l.prompts.close()
	}()
	for {
		select {
		case entry := <-l.ch:
			l.append(entry)
		case prompt := <-l.promptCh:
			l.appendPrompt(prompt)
		case done := <-l.flush:
			l.drain()
			close(done)
		case <-l.stop:
			l.drain()
			return
		}
	}
}

// drain 把队列里剩余条目写完再返回（请求日志与提示词两条队列都要排空）。
func (l *Logger) drain() {
	for {
		select {
		case entry := <-l.ch:
			l.append(entry)
		case prompt := <-l.promptCh:
			l.appendPrompt(prompt)
		default:
			return
		}
	}
}

// append 只由写 goroutine 调用：句柄与尺寸状态都无并发。
func (l *Logger) append(entry Entry) {
	if l.dir == "" {
		return
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.requests.write(append(raw, '\n'))
}

// appendPrompt 落一条提示词快照；与请求日志同目录，靠文件名前缀区分。
func (l *Logger) appendPrompt(prompt Prompt) {
	if l.dir == "" {
		return
	}
	raw, err := json.Marshal(prompt)
	if err != nil {
		return
	}
	l.prompts.write(append(raw, '\n'))
}

// fileWriter 是「按天切换 + 按大小滚动」的追加写入器，只允许单个 goroutine 调用。
//
// 句柄复用是关键：逐条 open/close 会让每条日志付出 3~4 次 syscall，
// 而写日志是单 goroutine，直接卡住整条日志链路的吞吐。
type fileWriter struct {
	dir    string
	prefix string
	// limit 指向 Logger.maxBytes：运行期改上限不必重建写入器，且写入方与读取方
	// 不会因为一个普通字段的读写踩出数据竞争。
	limit *int64
	now   func() time.Time
	file  *os.File
	day   string
	seq   int
	bytes int64
}

func newFileWriter(dir, prefix string, limit *int64, now func() time.Time) *fileWriter {
	return &fileWriter{dir: dir, prefix: prefix, limit: limit, now: now}
}

func (w *fileWriter) write(raw []byte) {
	file, err := w.fileFor(int64(len(raw)))
	if err != nil {
		return
	}
	if _, err := file.Write(raw); err != nil {
		// 写失败（磁盘满 / 句柄失效）后必须丢句柄：否则后续所有条目都会
		// 静默写进一个坏掉的 fd，日志看起来「在写」实际全丢。
		w.close()
		return
	}
	w.bytes += int64(len(raw))
}

// fileFor 返回当前该写入的文件，必要时按天切换或按大小滚动。
func (w *fileWriter) fileFor(incoming int64) (*os.File, error) {
	maxBytes := atomic.LoadInt64(w.limit)
	day := w.now().In(config.Location()).Format(dayLayout)
	if w.file != nil {
		if w.day != day {
			w.close()
		} else if maxBytes <= 0 || w.bytes+incoming <= maxBytes {
			return w.file, nil
		} else {
			// 当前分片已满：关掉后续写到下一个分片。
			w.close()
			w.seq++
		}
	}
	if w.file == nil && w.day != day {
		w.day, w.seq = day, 0
	}
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return nil, err
	}
	// 重启后继续往后写：跳过已满的历史分片，避免开一个必然立刻滚动的空文件。
	for {
		path := w.pathOf(w.day, w.seq)
		if size, ok := fileSize(path); ok {
			if maxBytes > 0 && size >= maxBytes {
				w.seq++
				continue
			}
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		size, _ := fileSize(path)
		w.file, w.bytes = file, size
		return file, nil
	}
}

func (w *fileWriter) pathOf(day string, seq int) string {
	name := w.prefix + day
	if seq > 0 {
		name += fmt.Sprintf(".%d", seq)
	}
	return filepath.Join(w.dir, name+fileSuffix)
}

func (w *fileWriter) close() {
	if w.file == nil {
		return
	}
	_ = w.file.Close()
	w.file = nil
	w.bytes = 0
}

func fileSize(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return info.Size(), true
}

func (l *Logger) pushRing(entry Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ring) < RingSize {
		l.ring = append(l.ring, entry)
		l.next = len(l.ring) % RingSize
		return
	}
	l.ring[l.next] = entry
	l.next = (l.next + 1) % RingSize
}

// RunCleanup 启动即清理一次，之后按 interval 周期清理；stop 关闭时退出。
// 由 bootstrap 接线，保证保留期在运行期真正生效（不依赖外部 cron）。
func (l *Logger) RunCleanup(stop <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	_ = l.Cleanup()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = l.Cleanup()
		case <-stop:
			return
		}
	}
}

// Cleanup 删除超过保留期的按天文件（请求日志与提示词快照，含滚动产生的 .N 分片）。
func (l *Logger) Cleanup() error {
	if l.dir == "" || l.retention <= 0 {
		return nil
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return err
	}
	cutoff := l.now().In(config.Location()).AddDate(0, 0, -l.retention)
	for _, item := range entries {
		if item.IsDir() {
			continue
		}
		day, ok := DayOfFile(item.Name())
		if !ok {
			day, ok = PromptDayOfFile(item.Name())
		}
		if !ok {
			continue
		}
		date, err := time.ParseInLocation(dayLayout, day, config.Location())
		if err != nil {
			continue
		}
		if date.Before(cutoff) {
			_ = os.Remove(filepath.Join(l.dir, item.Name()))
		}
	}
	return nil
}

// DayOfFile 从按天日志文件名里取出日期（YYYYMMDD）；不是本包写的文件返回 false。
// 支持请求日志的滚动分片形态 requests-YYYYMMDD.N.jsonl。
func DayOfFile(name string) (string, bool) { return dayOfFile(name, filePrefix) }

// PromptDayOfFile 是提示词快照文件的日期解析，规则与 DayOfFile 一致。
func PromptDayOfFile(name string) (string, bool) { return dayOfFile(name, promptPrefix) }

func dayOfFile(name, prefix string) (string, bool) {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, fileSuffix) {
		return "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), fileSuffix)
	if idx := strings.IndexByte(rest, '.'); idx >= 0 {
		rest = rest[:idx]
	}
	if len(rest) != 8 {
		return "", false
	}
	// 只认纯数字日期：否则 requests-abcdefgh.jsonl 这类同名文件会被当成日志参与清理。
	for i := 0; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return "", false
		}
	}
	return rest, true
}

// PartOfFile 返回按天文件的分片序号：requests-YYYYMMDD.jsonl 为 0，.N 为 N。
// 日志查询按 (日期, 序号) 升序读才能还原写入顺序，进而按时间倒序取最新一页。
func PartOfFile(name string) (int, bool) { return partOfFile(name, filePrefix) }

// PromptPartOfFile 是提示词快照文件的分片序号解析。
func PromptPartOfFile(name string) (int, bool) { return partOfFile(name, promptPrefix) }

func partOfFile(name, prefix string) (int, bool) {
	if _, ok := dayOfFile(name, prefix); !ok {
		return 0, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), fileSuffix)
	idx := strings.IndexByte(rest, '.')
	if idx < 0 {
		return 0, true
	}
	seq, err := strconv.Atoi(rest[idx+1:])
	if err != nil || seq < 0 {
		return 0, false
	}
	return seq, true
}

// ReadPromptIn 在 dir 下指定日期（YYYYMMDD）的提示词文件里按 request_id 找快照。
//
// 从最新的分片往前扫：详情是低频点击，不值得为它维护一份内存索引；
// 只对「行内出现过该 id」的行做反序列化，避免为无关行解析几十 KB 的 JSON。
func ReadPromptIn(dir, day, requestID string) (Prompt, bool) {
	if dir == "" || day == "" || requestID == "" {
		return Prompt{}, false
	}
	needle := []byte(requestID)
	for _, path := range promptFilesOfDay(dir, day) {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		// 单条提示词最多 PromptLimitChars 个字符，JSON 转义后仍在百 KB 量级；
		// 给 4MB 余量，避免一整行被 scanner 截断后当成「文件里没有」。
		scanner.Buffer(make([]byte, 64*1024), 4<<20)
		var found Prompt
		hit := false
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.Contains(line, needle) {
				continue
			}
			var prompt Prompt
			if err := json.Unmarshal(line, &prompt); err != nil {
				continue
			}
			if prompt.RequestID != requestID {
				continue
			}
			found, hit = prompt, true
			break
		}
		file.Close()
		if hit {
			return found, true
		}
	}
	return Prompt{}, false
}

// promptFilesOfDay 返回当天提示词文件，按分片序号从大到小（新 → 旧）。
func promptFilesOfDay(dir, day string) []string {
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type chunk struct {
		path string
		seq  int
	}
	chunks := make([]chunk, 0, 4)
	for _, name := range names {
		if name.IsDir() {
			continue
		}
		if got, ok := PromptDayOfFile(name.Name()); !ok || got != day {
			continue
		}
		seq, ok := PromptPartOfFile(name.Name())
		if !ok {
			continue
		}
		chunks = append(chunks, chunk{path: filepath.Join(dir, name.Name()), seq: seq})
	}
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].seq > chunks[j].seq })
	paths := make([]string, 0, len(chunks))
	for _, item := range chunks {
		paths = append(paths, item.path)
	}
	return paths
}

// CaptureChatPrompt 从 OpenAI chat 请求体里抽出**本次用户原始输入**，产出快照与列表摘要。
//
// chat 请求的 messages 是整段对话历史：每轮都会把之前所有 user / assistant / tool 再发一遍，
// 所以一次请求里会有多条 user。日志只回答「这次用户问了什么」，因此只取最后一条 user 消息，
// 其余消息（历史提问、system、assistant、tool）只计入 Omitted，不落盘。
//
// 客户端常在 user 消息里注入 <additional_data> 等上下文，并用 <user_query> 包住真正输入；
// 有这对标签时只取标签内的文本，没有则整条保留（兼容普通 OpenAI 客户端）。
// content 为数组时只拼文本分段，图片 / 音频等其它分段记成 [类型] 占位，绝不抄 data:base64。
//
// 只有一条内容，超过 limit 时直接截前 limit 个字符；响应体本来就不记。
// 返回 ok=false 表示请求体里没有可记录的用户输入。
func CaptureChatPrompt(body []byte, limit int) (Prompt, PromptSummary, bool) {
	var parsed struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Messages) == 0 {
		return Prompt{}, PromptSummary{}, false
	}
	if limit <= 0 {
		limit = PromptLimitChars
	}

	// 只找最后一条有文本的 user 消息；前面的 user 是历史提问，同样不记。
	lastText := ""
	for _, message := range parsed.Messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		if text := contentText(message.Content); text != "" {
			lastText = text
		}
	}
	if lastText == "" {
		return Prompt{}, PromptSummary{}, false
	}

	text := extractUserQuery(lastText)
	size := utf8.RuneCountInString(text)
	keep := size
	truncated := false
	if keep > limit {
		keep = limit
		truncated = true
	}
	omitted := len(parsed.Messages) - 1

	snapshot := Prompt{
		Messages:  []PromptMessage{{Role: "user", Chars: size, Content: ClipRunes(text, keep), Truncated: truncated}},
		Omitted:   omitted,
		Chars:     size,
		Stored:    keep,
		Truncated: truncated,
	}
	summary := PromptSummary{
		Messages:  1,
		Omitted:   omitted,
		Chars:     size,
		Preview:   ClipRunes(text, PromptPreviewChars),
		Truncated: truncated,
	}
	return snapshot, summary, true
}

// extractUserQuery 从带 <user_query> 标签的 user 消息里取出用户原始输入；
// 没有成对标签、或标签内容为空时原样返回，兼容普通 OpenAI 客户端。
func extractUserQuery(text string) string {
	const openTag, closeTag = "<user_query>", "</user_query>"
	start := strings.Index(text, openTag)
	if start < 0 {
		return text
	}
	start += len(openTag)
	end := strings.Index(text[start:], closeTag)
	if end < 0 {
		return text
	}
	inner := strings.TrimSpace(text[start : start+end])
	if inner == "" {
		return text
	}
	return inner
}

// contentText 把 messages[].content 归一成文本：字符串原样返回，分段数组只拼文本，
// 其它分段（image_url / input_audio …）用 [类型] 占位，避免把 base64 抄进日志。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == "text" || part.Text != "" {
			builder.WriteString(part.Text)
			continue
		}
		if part.Type != "" {
			builder.WriteString("[" + part.Type + "]")
		}
	}
	return builder.String()
}

// ClipRunes 保留前 n 个字符（按 rune 截断，不会切坏 UTF-8）。
func ClipRunes(text string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	count := 0
	for index := range text {
		if count == n {
			return text[:index]
		}
		count++
	}
	return text
}
