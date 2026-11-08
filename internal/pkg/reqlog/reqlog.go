package reqlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"model-gateway/internal/config"
)

// Entry 是一条请求日志；字段与方案 §4.6 对齐，按天落 JSONL。
// 不落提示词、响应正文与任何凭据；ClientIP / UA 是调用来源，便于回答「这条请求是谁打进来的」。
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
}

type Attempt struct {
	Product string `json:"product"`
	Model   string `json:"model"`
	Status  int    `json:"status"`
	Code    string `json:"code,omitempty"`
	MS      int64  `json:"ms"`
}

const RingSize = 1000

// filePrefix / fileSuffix 定义按天日志文件名：requests-YYYYMMDD.jsonl，
// 超过 max_file_mb 时同一日期续写 requests-YYYYMMDD.1.jsonl、.2 …
const (
	filePrefix = "requests-"
	fileSuffix = ".jsonl"
	dayLayout  = "20060102"
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
	flush    chan chan struct{}
	stop     chan struct{}
	wg       sync.WaitGroup
	dropped  int64
	started  bool

	// 以下字段只在写 goroutine 内访问（append 由 loop 单线程调用），无需加锁。
	file      *os.File
	fileDay   string
	fileSeq   int
	fileBytes int64
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
		l.maxBytes = int64(mb) << 20
	}
	return l
}

func newLogger(dir string, retentionDays int, maxBytes int64, buffer int, startWorker bool) *Logger {
	logger := &Logger{
		dir:       dir,
		retention: retentionDays,
		maxBytes:  maxBytes,
		now:       time.Now,
		ring:      make([]Entry, 0, RingSize),
		ch:        make(chan Entry, buffer),
		flush:     make(chan chan struct{}),
		stop:      make(chan struct{}),
	}
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
	defer l.closeFile()
	for {
		select {
		case entry := <-l.ch:
			l.append(entry)
		case done := <-l.flush:
			l.drain()
			close(done)
		case <-l.stop:
			l.drain()
			return
		}
	}
}

// drain 把队列里剩余条目写完再返回。
func (l *Logger) drain() {
	for {
		select {
		case entry := <-l.ch:
			l.append(entry)
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
	raw = append(raw, '\n')
	file, err := l.fileFor(int64(len(raw)))
	if err != nil {
		return
	}
	if _, err := file.Write(raw); err != nil {
		// 写失败（磁盘满 / 句柄失效）后必须丢句柄：否则后续所有条目都会
		// 静默写进一个坏掉的 fd，日志看起来「在写」实际全丢。
		l.closeFile()
		return
	}
	l.fileBytes += int64(len(raw))
}

// fileFor 返回当前该写入的文件，必要时按天切换或按大小滚动。
// 句柄复用是关键：逐条 open/close 会让每条日志付出 3~4 次 syscall，
// 而写日志是单 goroutine，直接卡住整条日志链路的吞吐。
func (l *Logger) fileFor(incoming int64) (*os.File, error) {
	day := l.now().In(config.Location()).Format(dayLayout)
	if l.file != nil {
		if l.fileDay != day {
			l.closeFile()
		} else if l.maxBytes <= 0 || l.fileBytes+incoming <= l.maxBytes {
			return l.file, nil
		} else {
			// 当前分片已满：关掉后续写到下一个分片。
			l.closeFile()
			l.fileSeq++
		}
	}
	if l.file == nil && l.fileDay != day {
		l.fileDay, l.fileSeq = day, 0
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return nil, err
	}
	// 重启后继续往后写：跳过已满的历史分片，避免开一个必然立刻滚动的空文件。
	for {
		path := l.pathOf(l.fileDay, l.fileSeq)
		if size, ok := fileSize(path); ok {
			if l.maxBytes > 0 && size >= l.maxBytes {
				l.fileSeq++
				continue
			}
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		size, _ := fileSize(path)
		l.file, l.fileBytes = file, size
		return file, nil
	}
}

func (l *Logger) pathOf(day string, seq int) string {
	name := filePrefix + day
	if seq > 0 {
		name += fmt.Sprintf(".%d", seq)
	}
	return filepath.Join(l.dir, name+fileSuffix)
}

func (l *Logger) closeFile() {
	if l.file == nil {
		return
	}
	_ = l.file.Close()
	l.file = nil
	l.fileBytes = 0
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

// Cleanup 删除超过保留期的按天文件（含滚动产生的 .N 分片）。
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
func DayOfFile(name string) (string, bool) {
	if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
		return "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
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
func PartOfFile(name string) (int, bool) {
	if _, ok := DayOfFile(name); !ok {
		return 0, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
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
