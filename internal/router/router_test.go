package router

import (
	"errors"
	"math/rand"
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
			_, _, err := Pick(cfg, st, "gpt-5", "", rand.New(rand.NewSource(1)))
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
	rnd := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		member, _, err := Pick(cfg, st, "gpt-5", "", rnd)
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
		member, _, err := Pick(cfg, st, "gpt-5", "", rand.New(rand.NewSource(int64(i))))
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

	member, _, err := Pick(cfg, st, "gpt-5", "", rand.New(rand.NewSource(1)))
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

	if _, _, err := Pick(cfg, st, "gpt-5", "", rand.New(rand.NewSource(1))); !errors.Is(err, ErrNoMember) {
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

	if _, _, err := Pick(cfg, st, "gpt-5", "", rand.New(rand.NewSource(1))); !errors.Is(err, ErrNoMember) {
		t.Fatalf("熔断打开期应无候选，实际 %v", err)
	}

	now = now.Add(2 * time.Second)
	if _, _, err := Pick(cfg, st, "gpt-5", "", rand.New(rand.NewSource(1))); err != nil {
		t.Fatalf("半开应放行一个探测：%v", err)
	}
	if _, _, err := Pick(cfg, st, "gpt-5", "", rand.New(rand.NewSource(1))); !errors.Is(err, ErrNoMember) {
		t.Fatalf("探测令牌只放行一次，实际 %v", err)
	}
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

	member, _, err := Pick(cfg, st, "gpt-5", "session-1", rand.New(rand.NewSource(1)))
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
	if _, _, err := Pick(cfg, st, "missing", "", nil); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("期望 ErrGroupNotFound，实际 %v", err)
	}
}
