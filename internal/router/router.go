package router

import (
	"errors"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"

	"model-gateway/internal/config"
)

var (
	ErrGroupNotFound = errors.New("模型组不存在")
	ErrGroupDisabled = errors.New("模型组已禁用")
	ErrNoMember      = errors.New("无可用成员")
)

// MemberState 是成员的可恢复运行态，字段名与 state.json 对齐。
type MemberState struct {
	FailStreak    int    `json:"fail_streak"`
	CooldownUntil string `json:"cooldown_until"`
	BreakerUntil  string `json:"breaker_until"`
	LastOKAt      string `json:"last_ok_at"`
	// LastError 是最近一次失败的可读原因，面板据此显示「为什么故障」。
	LastError string `json:"last_error,omitempty"`
	// ProbedAt 是最近一次主动嗅探的时间。
	ProbedAt string `json:"probed_at,omitempty"`
	// StatusStreaks 是按状态码的连续失败计数（key 为状态码），用于 status_rules 判定；
	// 任何一次成功都会清空，避免「历史失败」把后来的偶发错误攒成冷却。
	StatusStreaks map[string]int `json:"status_streaks,omitempty"`
}

type stickyEntry struct {
	key    string
	expire time.Time
}

// ProductState 是产品的健康摘要，供面板展示「最近成功 / 最近错误」。
type ProductState struct {
	LastOKAt  string `json:"last_ok_at,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// TokenState 是虚拟 Token 的使用计数，随 state.json 恢复。
type TokenState struct {
	RequestCount int64  `json:"request_count,omitempty"`
	LastUsedAt   string `json:"last_used_at,omitempty"`
}

// Snapshot 是 state.json 的全部可恢复内容。
type Snapshot struct {
	Members  map[string]MemberState
	Products map[string]ProductState
	Tokens   map[string]TokenState
}

// State 保存冷却 / 熔断 / 粘性；粘性允许丢失，冷却与熔断需可导出恢复。
type State struct {
	mu       sync.Mutex
	members  map[string]*MemberState
	products map[string]*ProductState
	tokens   map[string]*TokenState
	sticky   map[string]stickyEntry
	probes   map[string]bool
	now      func() time.Time
}

func NewState(now func() time.Time) *State {
	if now == nil {
		now = time.Now
	}
	return &State{
		members:  map[string]*MemberState{},
		products: map[string]*ProductState{},
		tokens:   map[string]*TokenState{},
		sticky:   map[string]stickyEntry{},
		probes:   map[string]bool{},
		now:      now,
	}
}

// Now 供调用方与测试统一时钟。
func (s *State) Now() time.Time { return s.now() }

// Load 用持久化状态覆盖内存态（启动恢复）；粘性不恢复。
func (s *State) Load(saved Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members = make(map[string]*MemberState, len(saved.Members))
	for key, item := range saved.Members {
		copied := cloneMemberState(item)
		s.members[key] = &copied
	}
	s.products = make(map[string]*ProductState, len(saved.Products))
	for key, item := range saved.Products {
		copied := item
		s.products[key] = &copied
	}
	s.tokens = make(map[string]*TokenState, len(saved.Tokens))
	for key, item := range saved.Tokens {
		copied := item
		s.tokens[key] = &copied
	}
}

// Export 导出可落盘的最小集合。
func (s *State) Export() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{
		Members:  make(map[string]MemberState, len(s.members)),
		Products: make(map[string]ProductState, len(s.products)),
		Tokens:   make(map[string]TokenState, len(s.tokens)),
	}
	for key, item := range s.members {
		out.Members[key] = cloneMemberState(*item)
	}
	for key, item := range s.products {
		out.Products[key] = *item
	}
	for key, item := range s.tokens {
		out.Tokens[key] = *item
	}
	return out
}

// ProductSnapshot 返回产品健康摘要的副本。
func (s *State) ProductSnapshot(id string) ProductState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item, ok := s.products[id]; ok {
		return *item
	}
	return ProductState{}
}

// NoteProductOK 记录产品最近成功时间并清空错误。
func (s *State) NoteProductOK(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.products[id] == nil {
		s.products[id] = &ProductState{}
	}
	s.products[id].LastOKAt = s.now().In(config.Location()).Format(config.TimeLayout)
	s.products[id].LastError = ""
}

// NoteProductError 记录产品最近一次失败原因（产品级，不影响成员冷却）。
func (s *State) NoteProductError(id, message string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.products[id] == nil {
		s.products[id] = &ProductState{}
	}
	s.products[id].LastError = message
}

// NoteTokenUse 累加 Token 使用次数并刷新最近使用时间。
func (s *State) NoteTokenUse(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens[id] == nil {
		s.tokens[id] = &TokenState{}
	}
	s.tokens[id].RequestCount++
	s.tokens[id].LastUsedAt = s.now().In(config.Location()).Format(config.TimeLayout)
}

// TokenSnapshot 返回 Token 使用统计的副本。
func (s *State) TokenSnapshot(id string) TokenState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item, ok := s.tokens[id]; ok {
		return *item
	}
	return TokenState{}
}

// Snapshot 返回成员的只读副本。
func (s *State) Snapshot(key string) MemberState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item, ok := s.members[key]; ok {
		return *item
	}
	return MemberState{}
}

// Success 清零失败计数并记录最近成功时间。
func (s *State) Success(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.ensure(key)
	item.FailStreak = 0
	item.CooldownUntil = ""
	item.BreakerUntil = ""
	item.LastOKAt = s.now().In(config.Location()).Format(config.TimeLayout)
	item.LastError = ""
	item.StatusStreaks = nil
	delete(s.probes, key)
}

// MemberStatus 是面板展示用的成员可用性摘要。
type MemberStatus struct {
	State      string `json:"state"` // ok | cooldown | breaker
	Reason     string `json:"reason,omitempty"`
	UnlockAt   string `json:"unlock_at,omitempty"`
	LastOKAt   string `json:"last_ok_at,omitempty"`
	ProbedAt   string `json:"probed_at,omitempty"`
	FailStreak int    `json:"fail_streak,omitempty"`
}

// StatusOf 计算成员当前是否参与选路，以及被摘除的原因与解除时间。
func (s *State) StatusOf(key string) MemberStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.members[key]
	if !ok {
		return MemberStatus{State: "ok"}
	}
	out := MemberStatus{
		State: "ok", Reason: item.LastError, LastOKAt: item.LastOKAt,
		ProbedAt: item.ProbedAt, FailStreak: item.FailStreak,
	}
	now := s.now()
	if until := parseTime(item.BreakerUntil); !until.IsZero() && until.After(now) {
		out.State, out.UnlockAt = "breaker", item.BreakerUntil
		return out
	}
	if until := parseTime(item.CooldownUntil); until.After(now) {
		out.State, out.UnlockAt = "cooldown", item.CooldownUntil
	}
	if out.Reason == "" && out.State != "ok" {
		out.Reason = "上游连续失败"
	}
	return out
}

// SetLastError 记录成员最近一次失败原因（成功时由 Success 清空）。
func (s *State) SetLastError(key, reason string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure(key).LastError = reason
}

// Probe 记录一次主动嗅探：healthy=true 视为已恢复，清掉冷却与熔断并刷新成功时间。
func (s *State) Probe(key string, healthy bool, reason string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.ensure(key)
	now := s.now().In(config.Location()).Format(config.TimeLayout)
	item.ProbedAt = now
	if !healthy {
		item.LastError = reason
		return
	}
	item.FailStreak = 0
	item.CooldownUntil = ""
	item.BreakerUntil = ""
	item.LastOKAt = now
	item.LastError = ""
	item.StatusStreaks = nil
	delete(s.probes, key)
}

// Failure 累计失败并按阈值进入软冷却；达到阈值时开熔断。
func (s *State) Failure(key string, cooldown config.Cooldown) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.ensure(key)
	item.FailStreak++
	now := s.now()
	base := parseDuration(cooldown.Base, 2*time.Second)
	max := parseDuration(cooldown.Max, 60*time.Second)

	if item.FailStreak >= cooldown.FailThreshold && cooldown.FailThreshold > 0 {
		open := parseDuration(cooldown.OpenDuration, 5*time.Minute)
		// 半开失败再开一个周期，周期翻倍但封顶 1 小时。
		if item.BreakerUntil != "" {
			open *= 2
			if open > time.Hour {
				open = time.Hour
			}
		}
		item.BreakerUntil = now.Add(open).In(config.Location()).Format(config.TimeLayout)
		delete(s.probes, key)
		return
	}

	backoff := base
	for i := 1; i < item.FailStreak; i++ {
		backoff *= 2
		if backoff >= max {
			break
		}
	}
	if backoff > max {
		backoff = max
	}
	item.CooldownUntil = now.Add(backoff).In(config.Location()).Format(config.TimeLayout)
}

// SetCooldown 直接把成员摘到指定时刻，不累加失败计数。
// 用于「上游明确不可用」这类重试也无用的错误（额度耗尽、地区不支持、凭据无效）：
// 它们不代表成员响应能力下降，不该走指数退避，也不该攒成熔断。
func (s *State) SetCooldown(key string, duration time.Duration, reason string) {
	if key == "" || duration <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.ensure(key)
	item.CooldownUntil = s.now().Add(duration).In(config.Location()).Format(config.TimeLayout)
	item.LastError = reason
}

// CountStatus 按状态码累加连续失败计数；达到阈值时把成员摘掉并清零该状态码计数。
// 返回 true 表示本次已触发冷却。命中规则的错误不再叠加 fail_streak，
// 这样「偶发同状态码」不会被指数退避提前放大成熔断。
func (s *State) CountStatus(key string, rule config.StatusRule, duration time.Duration, reason string) bool {
	if key == "" || rule.Threshold <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.ensure(key)
	if item.StatusStreaks == nil {
		item.StatusStreaks = map[string]int{}
	}
	code := strconv.Itoa(rule.Status)
	item.StatusStreaks[code]++
	item.LastError = reason
	if item.StatusStreaks[code] < rule.Threshold {
		return false
	}
	delete(item.StatusStreaks, code)
	if len(item.StatusStreaks) == 0 {
		item.StatusStreaks = nil
	}
	item.CooldownUntil = s.now().Add(duration).In(config.Location()).Format(config.TimeLayout)
	return true
}

// Available 纯读判断成员是否参与选路，不消耗半开探测名额。
// 名额改由 ClaimProbe 在真正选中并准备转发时占用，避免选路阶段把
// 所有到期成员的探测名额一次性用光，导致并发请求集体「无可用成员」。
func (s *State) Available(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.members[key]
	if !ok {
		return true
	}
	now := s.now()
	if until := parseTime(item.CooldownUntil); until.After(now) {
		return false
	}
	if until := parseTime(item.BreakerUntil); !until.IsZero() {
		if until.After(now) || s.probes[key] {
			return false
		}
	}
	return true
}

// ClaimProbe 为熔断已到期的成员抢占半开探测名额，只对实际选中的成员调用。
// 非半开成员不占名额；抢占失败说明名额已被其它请求拿走，应换下一个候选。
func (s *State) ClaimProbe(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.members[key]
	if !ok {
		return true
	}
	until := parseTime(item.BreakerUntil)
	if until.IsZero() || until.After(s.now()) {
		return true
	}
	if s.probes[key] {
		return false
	}
	s.probes[key] = true
	return true
}

// BindSticky 记录会话命中，TTL 内优先复用。
func (s *State) BindSticky(sessionKey, memberKey string, ttl time.Duration) {
	if sessionKey == "" || ttl <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sticky[sessionKey] = stickyEntry{key: memberKey, expire: s.now().Add(ttl)}
}

func (s *State) stickyHit(sessionKey string) string {
	if sessionKey == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.sticky[sessionKey]
	if !ok || s.now().After(entry.expire) {
		delete(s.sticky, sessionKey)
		return ""
	}
	return entry.key
}

// cloneMemberState 深拷贝可变字段：StatusStreaks 是 map，快照与运行态不能共享底层数组。
func cloneMemberState(item MemberState) MemberState {
	if item.StatusStreaks != nil {
		streaks := make(map[string]int, len(item.StatusStreaks))
		for code, count := range item.StatusStreaks {
			streaks[code] = count
		}
		item.StatusStreaks = streaks
	}
	return item
}

func (s *State) ensure(key string) *MemberState {
	item, ok := s.members[key]
	if !ok {
		item = &MemberState{}
		s.members[key] = item
	}
	return item
}

func parseTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.ParseInLocation(config.TimeLayout, raw, config.Location())
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func parseDuration(raw string, fallback time.Duration) time.Duration {
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// InWindow 判断成员在当前时间是否处于生效时段；面板与选路共用同一判定。
// 「限时生效」（带日期的绝对区间）与「每日时段」同时开启时，两者都要命中。
func InWindow(member config.Member, at time.Time) bool { return inWindow(member, at) }

func inWindow(member config.Member, now time.Time) bool {
	return inDateWindow(member.Effective, now) && inDailyWindow(member.Daily, now)
}

// inDateWindow 判定绝对区间闭区间：任一端为空表示该端不限制。
func inDateWindow(window config.Effective, now time.Time) bool {
	start, hasStart, err := config.ParseTime(window.Start)
	if err != nil {
		return false
	}
	end, hasEnd, err := config.ParseTime(window.End)
	if err != nil {
		return false
	}
	if hasStart && now.Before(start) {
		return false
	}
	if hasEnd && now.After(end) {
		return false
	}
	return true
}

// inDailyWindow 判定每天重复的时段：两端都空表示全天生效；
// start 晚于 end 表示跨天（now >= start 或 now <= end），否则按同天闭区间判定。
func inDailyWindow(window config.DailyWindow, now time.Time) bool {
	start, hasStart, err := config.ParseClock(window.Start)
	if err != nil {
		return false
	}
	end, hasEnd, err := config.ParseClock(window.End)
	if err != nil {
		return false
	}
	if !hasStart && !hasEnd {
		return true
	}
	local := now.In(config.Location())
	second := local.Hour()*3600 + local.Minute()*60 + local.Second()
	switch {
	case hasStart && hasEnd && start > end:
		return second >= start || second <= end
	case hasStart && hasEnd:
		return second >= start && second <= end
	case hasStart:
		return second >= start
	default:
		return second <= end
	}
}

// Pick 选出一个成员并返回其唯一键；不需要排除已试成员时用它。
func Pick(cfg *config.Config, st *State, groupName, sessionKey string, rnd *rand.Rand) (config.Member, string, error) {
	return PickExcluding(cfg, st, groupName, sessionKey, nil, rnd)
}

// PickExcluding 在候选过滤阶段剔除本次请求已尝试过的成员。
// 排除必须发生在选路内部：Pick 每次都返回权重最高的可用成员，若靠调用方
// 「选完再判重」，冷却在长超时期间过期后已试成员会重新被选中，
// 被误判成「无可用成员」而提前断掉整条降级链。
func PickExcluding(cfg *config.Config, st *State, groupName, sessionKey string,
	excluded map[string]struct{}, rnd *rand.Rand) (config.Member, string, error) {
	if rnd == nil {
		rnd = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	var group *config.Group
	for i := range cfg.ModelGroups {
		if cfg.ModelGroups[i].Name == groupName {
			group = &cfg.ModelGroups[i]
			break
		}
	}
	if group == nil {
		return config.Member{}, "", ErrGroupNotFound
	}
	if !group.Enabled {
		return config.Member{}, "", ErrGroupDisabled
	}

	products := make(map[string]config.Product, len(cfg.Products))
	for _, p := range cfg.Products {
		products[p.ID] = p
	}

	now := st.Now()
	candidates := make([]config.Member, 0, len(group.Members))
	for _, member := range group.Members {
		if !member.Enabled {
			continue
		}
		product, ok := products[member.Product]
		if !ok || !product.Enabled {
			continue
		}
		if !inWindow(member, now) {
			continue
		}
		key := config.MemberKey(groupName, member)
		if !st.Available(key) {
			continue
		}
		if _, seen := excluded[key]; seen {
			continue
		}
		candidates = append(candidates, member)
	}
	if len(candidates) == 0 {
		return config.Member{}, "", ErrNoMember
	}

	if hit := st.stickyHit(sessionKey); hit != "" {
		for _, member := range candidates {
			key := config.MemberKey(groupName, member)
			if key != hit {
				continue
			}
			if st.ClaimProbe(key) {
				return member, key, nil
			}
			break
		}
	}

	// 权重即优先级：weight 越大越先选；同权重按列表顺序（顺序即降级链）。
	// 与 workbuddy2api 的模型组同口径，不再有独立的 priority 概念。
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Weight > candidates[j].Weight
	})

	// prefer_last_ok 只在本权重层内把上次成功的成员提到层首，绝不跨权重层提升。
	if cfg.Routing.PreferLastOK {
		top := candidates[0].Weight
		layerEnd := 0
		for layerEnd < len(candidates) && candidates[layerEnd].Weight == top {
			layerEnd++
		}
		if layerEnd > 1 {
			best, bestAt := -1, ""
			for i := 0; i < layerEnd; i++ {
				key := config.MemberKey(groupName, candidates[i])
				lastOK := st.Snapshot(key).LastOKAt
				if lastOK != "" && lastOK > bestAt {
					best, bestAt = i, lastOK
				}
			}
			if best > 0 {
				member := candidates[best]
				copy(candidates[1:best+1], candidates[0:best])
				candidates[0] = member
			}
		}
	}

	// 只对真正选中的成员抢占半开名额：抢占失败就顺延到下一个候选。
	for _, member := range candidates {
		key := config.MemberKey(groupName, member)
		if st.ClaimProbe(key) {
			return member, key, nil
		}
	}
	return config.Member{}, "", ErrNoMember
}
