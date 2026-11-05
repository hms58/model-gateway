package reqlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"model-gateway/internal/config"
)

// Entry 是一条请求日志；字段与方案 §4.6 对齐，按天落 JSONL。
type Entry struct {
	Time             string    `json:"time"`
	RequestID        string    `json:"request_id,omitempty"`
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
	ClientIP         string    `json:"client_ip,omitempty"`
	UA               string    `json:"ua,omitempty"`
}

type Attempt struct {
	Product string `json:"product"`
	Model   string `json:"model"`
	Status  int    `json:"status"`
	Code    string `json:"code,omitempty"`
	MS      int64  `json:"ms"`
}

const RingSize = 1000

// Logger 用有界队列 + 单写 goroutine：转发路径只入队，满了丢弃并计数，绝不阻塞。
type Logger struct {
	dir       string
	retention int
	now       func() time.Time
	ring      []Entry
	next      int
	mu        sync.RWMutex
	ch        chan Entry
	flush     chan chan struct{}
	stop      chan struct{}
	wg        sync.WaitGroup
	dropped   int64
	started   bool
}

func New(dir string, retentionDays int) *Logger {
	return newLogger(dir, retentionDays, 1024, true)
}

func newLogger(dir string, retentionDays, buffer int, startWorker bool) *Logger {
	logger := &Logger{
		dir:       dir,
		retention: retentionDays,
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

func (l *Logger) append(entry Entry) {
	if l.dir == "" {
		return
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(l.dir, "requests-"+l.now().In(config.Location()).Format("20060102")+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = file.Write(append(raw, '\n'))
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

// Cleanup 删除超过保留期的按天文件。
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
		if item.IsDir() || !strings.HasPrefix(item.Name(), "requests-") {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(item.Name(), "requests-"), ".jsonl")
		date, err := time.ParseInLocation("20060102", raw, config.Location())
		if err != nil {
			continue
		}
		if date.Before(cutoff) {
			_ = os.Remove(filepath.Join(l.dir, item.Name()))
		}
	}
	return nil
}
