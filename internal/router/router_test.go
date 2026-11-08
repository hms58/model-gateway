package router

import (
	"errors"
	"testing"
	"time"

	"model-gateway/internal/config"
)

func mustParse(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, has, err := config.ParseTime(raw)
	if err != nil || !has {
		t.Fatalf("解析时间失败 %q：%v", raw, err)
	}
	return parsed
}

func baseCfg() *config.Config {
	cfg := config.Default()
	cfg.Products = []config.Product{
		{ID: "workbuddy", Enabled: true, BaseURL: "http://127.0.0.1:7863"},
		{ID: "trae", Enabled: true, BaseURL: "http://127.0.0.1:7864"},
	}
	return &cfg
}

func TestPickEffectiveWindowBoundary(t *testing.T) {
	const start = "2026-11-01 00:00:00"
	const end = "2026-12-01 23:59:59"
	tests := []struct {
		name string
		now  string
		want bool
	}{
		{"早于开始一秒", "2026-10-31 23:59:59", false},
		{"恰好开始", start, true},
		{"恰好结束", end, true},
		{"晚于结束一秒", "2026-12-02 00:00:00", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg()
			cfg.ModelGroups = []config.Group{{
				Name: "gpt-5", Enabled: true,
				Members: []config.Member{{
					Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true,
					Effective: config.Effective{Start: start, End: end},
				}},
			}}
			now := mustParse(t, tc.now)
			st := NewState(func() time.Time { return now })
			_, _, err := Pick(cfg, st, "gpt-5", "")
			if tc.want && err != nil {
				t.Fatalf("边界内应可选中：%v", err)
			}
			if !tc.want && !errors.Is(err, ErrNoMember) {
				t.Fatalf("区间外应无候选，实际 %v", err)
			}
		})
	}
}

// 权重即优先级：weight 越大越先选，与列表顺序无关。
func TestPickPrefersHigherWeight(t *testing.T) {
	cfg := baseCfg()
	cfg.ModelGroups = []config.Group{{
		Name: "gpt-5", Enabled: true,
		Members: []config.Member{
			{Product: "trae", Model: "light", Weight: 10, Enabled: true},
			{Product: "workbuddy", Model: "heavy", Weight: 100, Enabled: true},
		},
	}}
	st := NewState(func() time.Time { return mustParse(t, "2026-11-15 12:00:00") })
	for i := 0; i < 50; i++ {
		member, _, err := Pick(cfg, st, "gpt-5", "")
		if err != nil {
			t.Fatalf("选路失败：%v", err)
		}
		if member.Model != "heavy" {
			t.Fatalf("应选权重最高的成员，实际 %s", member.Model)
		}
	}
}

// 同权重按列表顺序，不做随机：顺序就是降级链。
func TestPickEqualWeightKeepsListOrder(t *testing.T) {
	cfg := baseCfg()
	cfg.Routing.PreferLastOK = false
	cfg.ModelGroups = []config.Group{{
		Name: "gpt-5", Enabled: true,
		Members: []config.Member{
			{Product: "workbuddy", Model: "first", Weight: 50, Enabled: true},
			{Product: "trae", Model: "second", Weight: 50, Enabled: true},
		},
	}}
	st := NewState(func() time.Time { return mustParse(t, "2026-11-15 12:00:00") })
	for i := 0; i < 20; i++ {
		member, _, err := Pick(cfg, st, "gpt-5", "")
		if err != nil {
			t.Fatalf("选路失败：%v", err)
		}
		if member.Model != "first" {
			t.Fatalf("同权重应取列表第一个，实际 %s", member.Model)
		}
	}
}

// prefer_last_ok 只在同权重层内提升，不能越过更高权重的成员。
func TestPreferLastOKDoesNotCrossWeight(t *testing.T) {
	cfg := baseCfg()
	cfg.Routing.PreferLastOK = true
	cfg.ModelGroups = []config.Group{{
		Name: "gpt-5", Enabled: true,
		Members: []config.Member{
			{Product: "workbuddy", Model: "high", Weight: 100, Enabled: true},
			{Product: "trae", Model: "low", Weight: 90, Enabled: true},
		},
	}}
	now := mustParse(t, "2026-11-15 12:00:00")
	st := NewState(func() time.Time { return now })
	st.Success("gpt-5|trae|low")

	member, _, err := Pick(cfg, st, "gpt-5", "")
	if err != nil {
		t.Fatalf("选路失败：%v", err)
	}
	if member.Model != "high" {
		t.Fatalf("lastOK 不应跨权重层提升，实际 %s", member.Model)
	}
}

func TestPickSkipsCooldownMember(t *testing.T) {
	cfg := baseCfg()
	cfg.ModelGroups = []config.Group{{
		Name: "gpt-5", Enabled: true,
		Members: []config.Member{{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true}},
	}}
	now := mustParse(t, "2026-11-15 12:00:00")
	st := NewState(func() time.Time { return now })
	key := config.MemberKey("gpt-5", cfg.ModelGroups[0].Members[0])
	st.Failure(key, cfg.Routing.Cooldown)

	if _, _, err := Pick(cfg, st, "gpt-5", ""); !errors.Is(err, ErrNoMember) {
		t.Fatalf("冷却期应无候选，实际 %v", err)
	}
}

func TestBreakerHalfOpenAllowsSingleProbe(t *testing.T) {
	cfg := baseCfg()
	cfg.Routing.Cooldown.FailThreshold = 1
	cfg.Routing.Cooldown.OpenDuration = "1s"
	cfg.ModelGroups = []config.Group{{
		Name: "gpt-5", Enabled: true,
		Members: []config.Member{{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true}},
	}}
	now := mustParse(t, "2026-11-15 12:00:00")
	st := NewState(func() time.Time { return now })
	key := config.MemberKey("gpt-5", cfg.ModelGroups[0].Members[0])
	st.Failure(key, cfg.Routing.Cooldown)

	if _, _, err := Pick(cfg, st, "gpt-5", ""); !errors.Is(err, ErrNoMember) {
		t.Fatalf("熔断打开期应无候选，实际 %v", err)
	}

	now = now.Add(2 * time.Second)
	if _, _, err := Pick(cfg, st, "gpt-5", ""); err != nil {
		t.Fatalf("半开应放行一个探测：%v", err)
	}
	if _, _, err := Pick(cfg, st, "gpt-5", ""); !errors.Is(err, ErrNoMember) {
		t.Fatalf("探测令牌只放行一次，实际 %v", err)
	}
}

// 探测名额必须可归还：请求以 401/断开等「既不算成功也不算失败」的方式结束时，
// 不归还会让成员永久停在名额被占状态，既不被选中也不被探活。
func TestReleaseProbeFreesHalfOpenSlot(t *testing.T) {
	cfg := baseCfg()
	cooldown := config.Cooldown{Base: "2s", Max: "60s", FailThreshold: 1, OpenDuration: "1s"}
	cfg.Routing.Cooldown = cooldown
	cfg.ModelGroups = []config.Group{{
		Name: "gpt-5", Enabled: true,
		Members: []config.Member{{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true}},
	}}
	now := mustParse(t, "2026-11-15 12:00:00")
	st := NewState(func() time.Time { return now })
	key := config.MemberKey("gpt-5", cfg.ModelGroups[0].Members[0])
	st.Failure(key, cooldown)
	now = now.Add(2 * time.Second)

	if _, _, err := Pick(cfg, st, "gpt-5", ""); err != nil {
		t.Fatalf("半开应放行一个探测：%v", err)
	}
	if _, _, err := Pick(cfg, st, "gpt-5", ""); !errors.Is(err, ErrNoMember) {
		t.Fatalf("名额占用期间不应再被选中，实际 %v", err)
	}
	// 探测状态必须可观测：报成 ok 会让面板与 prober 都以为它正常。
	if got := st.StatusOf(key).State; got != "probing" {
		t.Fatalf("名额占用期间状态应为 probing，实际 %s", got)
	}

	st.ReleaseProbe(key)
	if _, _, err := Pick(cfg, st, "gpt-5", ""); err != nil {
		t.Fatalf("归还名额后应重新可探测：%v", err)
	}
	// 幂等：重复归还（例如成功路径已清）不应报错或误删其它状态。
	st.ReleaseProbe(key)
	st.ReleaseProbe("")
}

func TestPickHonoursStickySession(t *testing.T) {
	cfg := baseCfg()
	cfg.ModelGroups = []config.Group{{
		Name: "gpt-5", Enabled: true,
		Members: []config.Member{
			{Product: "workbuddy", Model: "primary", Priority: 0, Weight: 100, Enabled: true},
			{Product: "trae", Model: "backup", Priority: 10, Weight: 100, Enabled: true},
		},
	}}
	now := mustParse(t, "2026-11-15 12:00:00")
	st := NewState(func() time.Time { return now })
	st.BindSticky("session-1", "gpt-5|trae|backup", time.Minute)

	member, _, err := Pick(cfg, st, "gpt-5", "session-1")
	if err != nil {
		t.Fatalf("选路失败：%v", err)
	}
	if member.Model != "backup" {
		t.Fatalf("粘性命中应复用 backup，实际 %s", member.Model)
	}
}

func TestPickRejectsUnknownGroup(t *testing.T) {
	cfg := baseCfg()
	st := NewState(nil)
	if _, _, err := Pick(cfg, st, "missing", ""); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("期望 ErrGroupNotFound，实际 %v", err)
	}
}

func TestPickExcludingSkipsTriedMembers(t *testing.T) {
	cfg := baseCfg()
	cfg.ModelGroups = []config.Group{{
		Name: "gpt-5", Enabled: true,
		Members: []config.Member{
			{Product: "workbuddy", Model: "primary", Weight: 100, Enabled: true},
			{Product: "workbuddy", Model: "second", Weight: 90, Enabled: true},
			{Product: "trae", Model: "third", Weight: 80, Enabled: true},
		},
	}}
	st := NewState(nil)
	first, firstKey, err := Pick(cfg, st, "gpt-5", "")
	if err != nil {
		t.Fatalf("首次选路失败：%v", err)
	}
	second, secondKey, err := PickExcluding(cfg, st, "gpt-5", "", map[string]struct{}{firstKey: {}})
	if err != nil {
		t.Fatalf("排除已试成员后应仍有候选：%v", err)
	}
	if first.Model != "primary" || second.Model != "second" {
		t.Fatalf("降级顺序错误：%s -> %s", first.Model, second.Model)
	}
	if secondKey == firstKey {
		t.Fatal("已试成员不应被重新选中")
	}
}

// 30s 首包超时只配 2s 冷却时，已试成员会在下一次选路前复活；
// 排除必须在候选阶段生效，否则降级链会被误判为「无可用成员」。
func TestPickExcludingAfterCooldownExpires(t *testing.T) {
	cfg := baseCfg()
	cfg.Routing.Cooldown = config.Cooldown{Base: "2s", Max: "60s", FailThreshold: 3, OpenDuration: "5m"}
	cfg.ModelGroups = []config.Group{{
		Name: "gpt-5", Enabled: true,
		Members: []config.Member{
			{Product: "workbuddy", Model: "primary", Weight: 100, Enabled: true},
			{Product: "workbuddy", Model: "second", Weight: 90, Enabled: true},
		},
	}}
	now := mustParse(t, "2026-11-06 06:00:00")
	st := NewState(func() time.Time { return now })
	_, firstKey, err := Pick(cfg, st, "gpt-5", "")
	if err != nil {
		t.Fatalf("首次选路失败：%v", err)
	}
	st.Failure(firstKey, cfg.Routing.Cooldown)
	now = now.Add(30 * time.Second)
	if _, _, err := Pick(cfg, st, "gpt-5", ""); err != nil {
		t.Fatalf("冷却过期后成员应重新可用：%v", err)
	}
	member, _, err := PickExcluding(cfg, st, "gpt-5", "", map[string]struct{}{firstKey: {}})
	if err != nil {
		t.Fatalf("冷却复活不应打断降级链：%v", err)
	}
	if member.Model != "second" {
		t.Fatalf("应降级到下一个成员，实际 %s", member.Model)
	}
}

// Available 是纯读：选路评估全部候选时不能把半开探测名额用光。
func TestAvailableDoesNotConsumeProbeSlot(t *testing.T) {
	cfg := baseCfg()
	cfg.Routing.Cooldown = config.Cooldown{Base: "2s", Max: "60s", FailThreshold: 1, OpenDuration: "1s"}
	members := []config.Member{{Product: "workbuddy", Model: "primary", Weight: 100, Enabled: true}}
	cfg.ModelGroups = []config.Group{{Name: "gpt-5", Enabled: true, Members: members}}
	now := mustParse(t, "2026-11-06 06:00:00")
	st := NewState(func() time.Time { return now })
	key := config.MemberKey("gpt-5", members[0])
	st.Failure(key, cfg.Routing.Cooldown)
	now = now.Add(2 * time.Second)
	for i := 0; i < 3; i++ {
		if !st.Available(key) {
			t.Fatal("Available 纯读，重复调用不应消耗探测名额")
		}
	}
	if !st.ClaimProbe(key) {
		t.Fatal("首个 ClaimProbe 应抢到半开名额")
	}
	if st.ClaimProbe(key) {
		t.Fatal("半开名额只能被占用一次")
	}
	if st.Available(key) {
		t.Fatal("名额占用后成员应不再参与选路")
	}
}

// 每日时段跨天：23:00-07:00 表示当天 23:00 到次日 07:00，两端闭区间。
func TestPickDailyWindowCrossDay(t *testing.T) {
	tests := []struct {
		name string
		now  string
		want bool
	}{
		{"早于起点一秒", "2026-11-05 22:59:59", false},
		{"恰好起点", "2026-11-05 23:00:00", true},
		{"当天深夜", "2026-11-05 23:59:59", true},
		{"次日凌晨", "2026-11-06 03:00:00", true},
		{"恰好终点", "2026-11-06 07:00:00", true},
		{"晚于终点一秒", "2026-11-06 07:00:01", false},
		{"次日中午", "2026-11-06 12:00:00", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg()
			cfg.ModelGroups = []config.Group{{
				Name: "gpt-5", Enabled: true,
				Members: []config.Member{{
					Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true,
					Daily: config.DailyWindow{Start: "23:00", End: "07:00"},
				}},
			}}
			now := mustParse(t, tc.now)
			st := NewState(func() time.Time { return now })
			_, _, err := Pick(cfg, st, "gpt-5", "")
			if tc.want && err != nil {
				t.Fatalf("时段内应可选中：%v", err)
			}
			if !tc.want && !errors.Is(err, ErrNoMember) {
				t.Fatalf("时段外应无候选，实际 %v", err)
			}
		})
	}
}

// 每日时段同天：09:00-18:00 只在当天这个区间内生效。
func TestPickDailyWindowSameDay(t *testing.T) {
	tests := []struct {
		name string
		now  string
		want bool
	}{
		{"早于起点一秒", "2026-11-06 08:59:59", false},
		{"恰好起点", "2026-11-06 09:00:00", true},
		{"区间内", "2026-11-06 12:00:00", true},
		{"恰好终点", "2026-11-06 18:00:00", true},
		{"晚于终点一秒", "2026-11-06 18:00:01", false},
		{"次日区间内", "2026-11-07 12:00:00", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg()
			cfg.ModelGroups = []config.Group{{
				Name: "gpt-5", Enabled: true,
				Members: []config.Member{{
					Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true,
					Daily: config.DailyWindow{Start: "09:00:00", End: "18:00:00"},
				}},
			}}
			now := mustParse(t, tc.now)
			st := NewState(func() time.Time { return now })
			_, _, err := Pick(cfg, st, "gpt-5", "")
			if tc.want && err != nil {
				t.Fatalf("时段内应可选中：%v", err)
			}
			if !tc.want && !errors.Is(err, ErrNoMember) {
				t.Fatalf("时段外应无候选，实际 %v", err)
			}
		})
	}
}

// 「限时生效」与每日时段同时开启：两者都命中才生效。
func TestPickRequiresBothWindows(t *testing.T) {
	tests := []struct {
		name string
		now  string
		want bool
	}{
		{"日期区间内且每日时段内", "2026-11-06 23:30:00", true},
		{"日期区间内但每日时段外", "2026-11-06 12:00:00", false},
		{"每日时段内但日期区间外", "2026-11-10 23:30:00", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg()
			cfg.ModelGroups = []config.Group{{
				Name: "gpt-5", Enabled: true,
				Members: []config.Member{{
					Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true,
					Effective: config.Effective{Start: "2026-11-01 00:00:00", End: "2026-11-08 23:59:59"},
					Daily:     config.DailyWindow{Start: "23:00", End: "07:00"},
				}},
			}}
			now := mustParse(t, tc.now)
			st := NewState(func() time.Time { return now })
			_, _, err := Pick(cfg, st, "gpt-5", "")
			if tc.want && err != nil {
				t.Fatalf("两个时段都命中应可选中：%v", err)
			}
			if !tc.want && !errors.Is(err, ErrNoMember) {
				t.Fatalf("任一时段未命中都应无候选，实际 %v", err)
			}
		})
	}
}

// 按状态码计数：达到阈值才冷却；一次成功清零，避免历史失败攒成冷却。
func TestCountStatusCoolsAtThresholdAndResetsOnSuccess(t *testing.T) {
	now := mustParse(t, "2026-11-15 12:00:00")
	st := NewState(func() time.Time { return now })
	rule := config.StatusRule{Status: 400, Threshold: 2, Duration: "60s"}
	key := "gpt-5|workbuddy|m"

	if st.CountStatus(key, rule, time.Minute, "HTTP 400") {
		t.Fatal("首次失败不应冷却")
	}
	if !st.CountStatus(key, rule, time.Minute, "HTTP 400") {
		t.Fatal("达到阈值应冷却")
	}
	if st.Snapshot(key).CooldownUntil == "" {
		t.Fatal("应写入冷却时间")
	}
	if st.Available(key) {
		t.Fatal("冷却期内不应参与选路")
	}

	st.Success(key)
	snap := st.Snapshot(key)
	if snap.StatusStreaks != nil || snap.CooldownUntil != "" {
		t.Fatalf("成功应清空连续计数与冷却：%+v", snap)
	}
}
