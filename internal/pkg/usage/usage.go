// Package usage 按 (时间片, 产品, 模型) 累计请求与 token 用量，供面板「统计」页展示。
//
// 为什么不直接扫 internal/pkg/reqlog 落盘的按天 JSONL：
//   - 那些文件受 logging.retention_days 约束（默认 14 天），到期即删，
//     「上个月每天用了多少 token」会永久消失；
//   - 统计需要长期趋势，所以这里自带聚合与折叠，与日志保留期解耦。
//
// 只统计 token / 请求数 / 成功率 / 耗时 / 缓存命中率，**不含任何积分维度**：
// 积分是上游计费口径，网关只做聚合与治理，不重复记账。
//
// 保留策略（分片粒度自动降级，总量因此有界）：
//   - 近 hourlyKeep 小时内：小时桶（看当天尖峰）
//   - 更早：折叠为日桶，**永久保留**（看长期趋势）
//   - 桶数超过 maxBuckets 时从最旧的小时桶开始折，避免异常流量把内存撑爆
//
// 落盘：<data_dir>/usage.json，原子替换（保留 .bak）+ 30s 防抖，重启不丢。
package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"model-gateway/internal/config"
)

const (
	// hourlyKeep 小时桶的保留时长；超出后折叠为日桶。
	hourlyKeep = 90 * 24 * time.Hour
	// flushInterval 防抖落盘间隔。
	flushInterval = 30 * time.Second
	// hourLayout / dayLayout 分片键的时间格式（本地时区，与用户直觉一致）。
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
	// fileVersion 是 usage.json 的当前格式版本；旧版本缺失字段按零值加载，旧数据不丢弃。
	fileVersion = 1
	// unknown 是没有命中任何成员（鉴权失败 / 组不存在 / 无可用成员）时的占位维度。
	// 这类请求没有 token，但必须计入请求数与失败数，否则成功率会虚高。
	unknown = "(unknown)"
	// maxSeriesDays 是补齐连续日期时的上限：超过它说明窗口异常（或数据跨了十年），
	// 此时只输出有数据的日期，不做补零，避免一次请求生成几十万个空点。
	maxSeriesDays = 3660
)

// maxBuckets 桶数硬上限。超过时立即触发一次折叠，避免异常流量把内存/文件撑爆。
// var 而非 const：测试要把它调小才能覆盖该守卫。
var maxBuckets = 200_000

// bucket 一个 (时间片, 产品, 模型) 的累计量。
// JSON 字段名刻意取短，因为桶数量会随时间增长。
type bucket struct {
	Scope   string `json:"s"`
	Product string `json:"p"`
	Model   string `json:"m"`
	Req     int64  `json:"q"` // 请求数（含失败）
	Err     int64  `json:"e"` // 失败数（outcome != ok）
	PT      int64  `json:"pt"`
	CT      int64  `json:"ct"`
	TT      int64  `json:"tt"`
	CH      int64  `json:"ch,omitempty"` // prompt 缓存命中 token
	CM      int64  `json:"cm,omitempty"` // prompt 缓存未命中 token
	LatMs   int64  `json:"l"`            // 成功请求的耗时累计（ms）
	LatN    int64  `json:"ln"`           // 成功请求的耗时样本数
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// Recorder 并发安全的用量记录器。
type Recorder struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*bucket // key: scope|product|model
	dirty   bool
	// now 只用于解析「今天」这类相对窗口；生产固定 time.Now，测试可注入固定时间。
	now func() time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	started  bool
}

// New 创建记录器。path 为空时禁用落盘（纯内存，测试用）。
func New(path string) *Recorder {
	r := &Recorder{
		path:    path,
		buckets: make(map[string]*bucket),
		now:     time.Now,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return r
}

// Start 启动后台防抖落盘与折叠。Stop 前一直运行。
func (r *Recorder) Start() {
	if r == nil || r.started {
		return
	}
	r.started = true
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-ticker.C:
				r.mu.Lock()
				n := len(r.buckets)
				r.mu.Unlock()
				if n > maxBuckets {
					r.Rollup(time.Now())
				}
				r.flush(false)
			}
		}
	}()
}

// Stop 停止后台循环并做最后一次落盘。
func (r *Recorder) Stop() {
	if r == nil || !r.started {
		return
	}
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
	r.started = false
}

// Delta 一次请求的用量增量。
type Delta struct {
	// Tokens 为 true 表示上游确实回了 usage；false 时不累加任何 token 字段，
	// 避免把「没测到」记成「用了 0」。
	Tokens           bool
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CacheHitTokens   int64
	// LatencyMs 只对成功的请求采样：把超时与失败也算进均值会让「平均耗时」失去意义。
	LatencyMs  int64
	HasLatency bool
}

// Add 记录一次请求。
//
// ok=false 表示这次请求最终失败（传输错误 / 上游 >=400 / 中途断流）。失败请求通常
// 没有可用的 usage，但**仍要计入请求数与失败数**——成功率就是靠这一列算出来的。
func (r *Recorder) Add(now time.Time, product, model string, d Delta, ok bool) {
	if r == nil {
		return
	}
	if product == "" {
		product = unknown
	}
	if model == "" {
		model = unknown
	}
	// 时区与请求日志、配置里的时间窗统一用 config.Location()：
	// 统计页的「某天」必须和日志页按天分文件的口径一致，否则跨零点会对不上账。
	scope := "h:" + now.In(config.Location()).Format(hourLayout)
	key := scope + "|" + product + "|" + model

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		b = &bucket{Scope: scope, Product: product, Model: model}
		r.buckets[key] = b
	}
	b.Req++
	if !ok {
		b.Err++
	}
	if d.Tokens {
		b.PT += d.PromptTokens
		b.CT += d.CompletionTokens
		if d.TotalTokens > 0 {
			b.TT += d.TotalTokens
		} else if d.PromptTokens+d.CompletionTokens > 0 {
			// 上游没给 total：用 pt+ct 兜底，保证总量口径连续。
			b.TT += d.PromptTokens + d.CompletionTokens
		}
		if d.PromptTokens > 0 {
			// 未命中 = prompt - 命中：上游没报命中数就是「这一整段都没走缓存」，
			// 与请求日志里 cache_hit_rate = hit/prompt 的口径一致。
			// 只统计「报了命中」的请求会把不支持缓存的上游算成高命中率。
			hit := d.CacheHitTokens
			if hit < 0 {
				hit = 0
			}
			if hit > d.PromptTokens {
				// 上游字段互相矛盾时不猜：命中数按 prompt 截顶，避免命中率超过 100%。
				hit = d.PromptTokens
			}
			b.CH += hit
			b.CM += d.PromptTokens - hit
		}
	}
	if ok && d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	r.dirty = true
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶（按本地日历日）；桶数超 maxBuckets
// 时额外从最旧的小时桶开始折（否则那个硬上限是空转）。
// 幂等：同一小时反复折叠不会重复计数（先累加再删源桶）。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	cutoff := now.Add(-hourlyKeep)

	r.mu.Lock()
	defer r.mu.Unlock()

	type move struct {
		from, to string
		ts       time.Time
	}
	// 候选 = 全部小时桶（该不该折在下面的应用循环里判：年龄达标 或 桶数超硬上限）。
	var cand []move
	for k, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, "h:") {
			continue
		}
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), config.Location())
		if err != nil {
			continue
		}
		day := "d:" + ts.Format(dayLayout)
		cand = append(cand, move{k, day + "|" + b.Product + "|" + b.Model, ts})
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].ts.Before(cand[j].ts) })
	folded := 0
	for _, m := range cand {
		src := r.buckets[m.from]
		if src == nil {
			continue
		}
		// 没到龄且桶数没超限 → 保留细粒度；超限时逐个折最旧的，直到回到上限内
		// （折一个不一定减少总量：目标日桶可能是新建的，所以按当前桶数判而非折叠次数）。
		if !m.ts.Before(cutoff) && len(r.buckets) <= maxBuckets {
			continue
		}
		dst := r.buckets[m.to]
		if dst == nil {
			cp := *src
			cp.Scope = strings.SplitN(m.to, "|", 2)[0]
			dst = &cp
			r.buckets[m.to] = dst
		} else {
			dst.Req += src.Req
			dst.Err += src.Err
			dst.PT += src.PT
			dst.CT += src.CT
			dst.TT += src.TT
			dst.CH += src.CH
			dst.CM += src.CM
			dst.LatMs += src.LatMs
			dst.LatN += src.LatN
		}
		delete(r.buckets, m.from)
		folded++
	}
	if folded > 0 {
		r.dirty = true
		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度，现 %d 桶）", folded, hourlyKeep, len(r.buckets))
	}
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := readWithBackup(r.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		r.buckets[b.Scope+"|"+b.Product+"|"+b.Model] = &b
	}
	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(r.buckets), r.path)
	return nil
}

// readWithBackup 正文坏（掉电留全 0、被截断）时回退 .bak，统计不归零。
func readWithBackup(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil && json.Valid(raw) {
		return raw, nil
	}
	backup, backupErr := os.ReadFile(path + ".bak")
	if backupErr == nil && json.Valid(backup) {
		return backup, nil
	}
	return raw, err
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{Version: fileVersion, Saved: time.Now().Format(time.RFC3339), Buckets: make([]bucket, 0, len(r.buckets))}
	for _, b := range r.buckets {
		snap.Buckets = append(snap.Buckets, *b)
	}
	r.dirty = false
	r.mu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[usage] 建目录失败: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("[usage] 写临时文件失败: %v", err)
		return
	}
	// 保留上一版做 .bak：正文被截断时还能回到上一份可用统计。
	if old, readErr := os.ReadFile(r.path); readErr == nil {
		_ = os.WriteFile(r.path+".bak", old, 0o644)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("[usage] 原子替换失败: %v", err)
	}
}

// Save 立即落盘（关闭前或面板「刷新」时调用）。
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- 聚合 ----

// Agg 一组累计量。成功率与缓存命中率都是**百分比（0-100）**，
// 前端直接拼 %；没有样本时保持 0，由前端显示「—」而不是 0%。
type Agg struct {
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	SuccessRate      float64 `json:"success_rate"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheHitTokens   int64   `json:"cache_hit_tokens"`
	CacheMissTokens  int64   `json:"cache_miss_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
}

// acc 是聚合过程中的累加器：Agg 只放已算好的结果，均值需要样本数才能
// 正确加权（不能对各桶的均值再取平均），所以样本数留在这里。
type acc struct {
	Agg
	latSum     int64
	latSamples int64
}

func (a *acc) add(b *bucket) {
	a.Requests += b.Req
	a.Errors += b.Err
	a.PromptTokens += b.PT
	a.CompletionTokens += b.CT
	a.TotalTokens += b.TT
	a.CacheHitTokens += b.CH
	a.CacheMissTokens += b.CM
	a.latSum += b.LatMs
	a.latSamples += b.LatN
}

func (a *acc) finish() Agg {
	out := a.Agg
	if a.Requests > 0 {
		out.SuccessRate = float64(a.Requests-a.Errors) / float64(a.Requests) * 100
	}
	if a.latSamples > 0 {
		out.AvgLatencyMs = float64(a.latSum) / float64(a.latSamples)
	}
	if total := a.CacheHitTokens + a.CacheMissTokens; total > 0 {
		out.CacheHitRate = float64(a.CacheHitTokens) / float64(total) * 100
	}
	return out
}

// Point 按天趋势上的一个点。
type Point struct {
	Date string `json:"date"`
	Agg
}

// ProductModelAgg 按 (产品, 模型) 聚合的一行，用于「Token 用量 Top」。
type ProductModelAgg struct {
	Product string `json:"product"`
	Model   string `json:"model"`
	Agg
}

// Snapshot 面板一次拉取的全部统计视图数据。
type Snapshot struct {
	Totals Agg `json:"totals"`
	// Series 固定按天：小时桶在聚合时按日历日归并，窗口内有数据缺口时补 0 点，
	// 保证横轴连续（否则「近 7 天」会变成 3 根柱子挤在一起）。
	Series         []Point           `json:"series"`
	ByProductModel []ProductModelAgg `json:"by_product_model"`
	Buckets        int               `json:"buckets"`
	FileBytes      int64             `json:"file_bytes"`
	Since          string            `json:"since,omitempty"`
	// WindowFrom/WindowTo 本次实际生效的统计区间（本地时间，RFC3339），供面板回显口径。
	// 空串 = 该侧不设界（全部历史 / 到今天为止）。
	WindowFrom string `json:"window_from,omitempty"`
	WindowTo   string `json:"window_to,omitempty"`
	Generated  string `json:"generated"`
}

// Window 统计窗口。三种口径按优先级解析（见 bounds）：
//   - From/To 任一非零 → 显式区间 [From, To)，按本地日历日切
//   - 否则 Days>0      → 近 Days 天（含今天，按本地日历日）
//   - 否则             → 全部历史
type Window struct {
	Days int
	From time.Time
	To   time.Time
}

// bounds 解析出实际生效的 [from, to)；零值表示该侧不设界。
// to 是**开区间**：传 11-07 表示统计到 11-06 整天为止。
func (w Window) bounds(now time.Time) (time.Time, time.Time) {
	if !w.From.IsZero() || !w.To.IsZero() {
		return dayStart(w.From), dayStart(w.To)
	}
	if w.Days > 0 {
		today := dayStart(now)
		return today.AddDate(0, 0, -(w.Days - 1)), time.Time{}
	}
	return time.Time{}, time.Time{}
}

// bucketTime 把桶 scope 解析成本地时间；脏 scope 返回 false（不进任何口径）。
func bucketTime(scope string) (time.Time, bool) {
	if strings.HasPrefix(scope, "h:") {
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(scope, "h:"), config.Location())
		return ts, err == nil
	}
	ts, err := time.ParseInLocation(dayLayout, strings.TrimPrefix(scope, "d:"), config.Location())
	return ts, err == nil
}

// Snapshot 聚合**所选窗口内**的桶，产出面板一次拉取的全部统计视图数据。
// 卡片汇总、按天趋势、Top 榜**全部**按同一窗口口径统计——切范围时所有数字随之变化，
// 避免出现「卡片是全部历史、趋势只改分片」这种被读成「筛选没生效」的口径。
func (r *Recorder) Snapshot(w Window) Snapshot {
	if r == nil {
		return Snapshot{Generated: time.Now().Format(time.RFC3339)}
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	from, to := w.bounds(now)
	windowed := !from.IsZero() || !to.IsZero()

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	var total acc
	pmAgg := map[string]*acc{}
	dayAgg := map[string]*acc{}
	since := ""
	matched := 0

	for i := range bs {
		b := &bs[i]
		if since == "" || b.Scope < since {
			since = b.Scope
		}
		ts, ok := bucketTime(b.Scope)
		if !ok {
			// 解析失败的脏桶不进任何口径（也不该出现在趋势里）。
			continue
		}
		day := ts.Format(dayLayout)
		if windowed {
			if !from.IsZero() && ts.Before(from) {
				continue
			}
			if !to.IsZero() && !ts.Before(to) {
				continue
			}
		}
		matched++
		total.add(b)

		key := b.Product + "\x00" + b.Model
		if pmAgg[key] == nil {
			pmAgg[key] = &acc{}
		}
		pmAgg[key].add(b)

		if dayAgg[day] == nil {
			dayAgg[day] = &acc{}
		}
		dayAgg[day].add(b)
	}

	snap := Snapshot{
		Totals:         total.finish(),
		ByProductModel: productModelKeyed(pmAgg),
		Buckets:        matched,
		Generated:      now.Format(time.RFC3339),
	}

	// 按天趋势：先补窗口内的空日（保持横轴连续），再按日期升序输出。
	for _, day := range windowDays(from, to, now) {
		if dayAgg[day] == nil {
			dayAgg[day] = &acc{}
		}
	}
	days := make([]string, 0, len(dayAgg))
	for day := range dayAgg {
		days = append(days, day)
	}
	sort.Strings(days)
	for _, day := range days {
		snap.Series = append(snap.Series, Point{Date: day, Agg: dayAgg[day].finish()})
	}

	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			snap.FileBytes = fi.Size()
		}
	}
	// since 去掉 scope 前缀（"h:2026-11-06T13" → "2026-11-06T13"）给前端展示；
	// 无任何桶时保持空（无数据不伪造起点）。
	snap.Since = strings.TrimPrefix(strings.TrimPrefix(since, "h:"), "d:")
	// 回显实际生效的区间：只有显式区间才回显。预设天数由前端自己知道，
	// 全部历史没有区间——两者回显都会变成噪音。
	if !w.From.IsZero() && !w.To.IsZero() {
		snap.WindowFrom = from.Format(time.RFC3339)
		snap.WindowTo = to.Format(time.RFC3339)
	}
	return snap
}

// windowDays 列出窗口覆盖的日期（升序）；无界或跨度过大时返回 nil（不补空点）。
func windowDays(from, to time.Time, now time.Time) []string {
	if from.IsZero() {
		return nil
	}
	end := to
	if end.IsZero() {
		end = dayStart(now).AddDate(0, 0, 1)
	}
	if !end.After(from) {
		return nil
	}
	if int(end.Sub(from).Hours()/24) > maxSeriesDays {
		return nil
	}
	out := make([]string, 0, int(end.Sub(from).Hours()/24)+1)
	for day := from; day.Before(end); day = day.AddDate(0, 0, 1) {
		out = append(out, day.Format(dayLayout))
	}
	return out
}

func dayStart(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	local := value.In(config.Location())
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, config.Location())
}

func productModelKeyed(m map[string]*acc) []ProductModelAgg {
	out := make([]ProductModelAgg, 0, len(m))
	for key, value := range m {
		product, model, _ := strings.Cut(key, "\x00")
		out = append(out, ProductModelAgg{Product: product, Model: model, Agg: value.finish()})
	}
	// 按 token 总量降序；同量按请求数降序，再按产品/模型升序，保证输出稳定（前端不抖）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		if out[i].Product != out[j].Product {
			return out[i].Product < out[j].Product
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Describe 返回一行人类可读的占用摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	var size int64
	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			size = fi.Size()
		}
	}
	return fmt.Sprintf("%d buckets, file %d bytes", n, size)
}
