package prober

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/store"
)

// newProber 造一个「一个产品 + 一个组 + 一个成员」的最小环境（组级异步嗅探已开启）。
func newProber(t *testing.T, upstream string, members []config.Member) (*Prober, *store.Store) {
	t.Helper()
	return newProberGroup(t, upstream, config.Group{Name: "gpt-5", Enabled: true, Probe: true, Members: members})
}

// newProberGroup 允许自定义整组（用来验证组级开关）。
func newProberGroup(t *testing.T, upstream string, group config.Group) (*Prober, *store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe = config.Probe{Enabled: true, Interval: "1s", MinGap: "1s"}
	cfg.Products = []config.Product{{ID: "workbuddy", Enabled: true, BaseURL: upstream, APIKey: "sk-test"}}
	cfg.ModelGroups = []config.Group{group}

	dir := t.TempDir()
	path := filepath.Join(dir, "modelgw.json")
	if err := config.SaveAtomic(path, &cfg); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}
	st, err := store.Open(path, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("打开 store 失败：%v", err)
	}
	return New(st), st
}

// chatStub 模拟下游 chat 接口：healthy=false 时回 503，用来表达「模型仍不可用」。
func chatStub(t *testing.T, healthy bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("探活应打 /v1/chat/completions，实际 %s", r.URL.Path)
		}
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":11102,"msg":"model not available"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
}

// 开启异步嗅探的成员异常后，探到真实请求成功即恢复（无需真实业务流量）。
func TestProbeOnceRecoversBrokenMember(t *testing.T) {
	upstream := chatStub(t, true)
	defer upstream.Close()

	prober, st := newProber(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 10, Enabled: true},
	})
	cfg := st.Snapshot()
	key := config.MemberKey("gpt-5", cfg.ModelGroups[0].Members[0])
	cooldown := cfg.Routing.Cooldown
	cooldown.FailThreshold = 1
	cooldown.OpenDuration = "1s" // 让解除时间落在 30s 窗口内
	st.State().Failure(key, cooldown)
	if got := st.State().StatusOf(key).State; got == "ok" {
		t.Fatalf("准备失败：成员应处于冷却/熔断，实际 %s", got)
	}

	checked, recovered := prober.ProbeOnce(context.Background())
	if checked != 1 || recovered != 1 {
		t.Fatalf("应探测 1 个并恢复 1 个，实际 checked=%d recovered=%d", checked, recovered)
	}
	if got := st.State().StatusOf(key); got.State != "ok" || got.ProbedAt == "" {
		t.Fatalf("探到应恢复并记录时间：%+v", got)
	}
}

// 真实请求仍失败 → 保持异常，并把原因写回状态。
func TestProbeOnceKeepsFailingMember(t *testing.T) {
	upstream := chatStub(t, false)
	defer upstream.Close()

	prober, st := newProber(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 10, Enabled: true},
	})
	cfg := st.Snapshot()
	key := config.MemberKey("gpt-5", cfg.ModelGroups[0].Members[0])
	cooldown := cfg.Routing.Cooldown
	cooldown.FailThreshold = 1
	cooldown.OpenDuration = "1s"
	st.State().Failure(key, cooldown)

	checked, recovered := prober.ProbeOnce(context.Background())
	if checked != 1 || recovered != 0 {
		t.Fatalf("应探测 1 个但恢复 0 个，实际 checked=%d recovered=%d", checked, recovered)
	}
	if status := st.State().StatusOf(key); !strings.Contains(status.Reason, "503") {
		t.Fatalf("应记录真实请求失败原因：%+v", status)
	}
}

// 组级开关关闭 → 组内所有成员都不探（即便成员上残留旧版 probe=true）。
func TestProbeOnceSkipsWhenGroupSwitchOff(t *testing.T) {
	upstream := chatStub(t, true)
	defer upstream.Close()

	prober, st := newProberGroup(t, upstream.URL, config.Group{
		Name: "gpt-5", Enabled: true, Probe: false,
		Members: []config.Member{
			// 存量配置里成员级 probe 可能还是 true，必须被组开关压过。
			{Product: "workbuddy", Model: "gpt-5", Weight: 10, Enabled: true, Probe: true},
		},
	})
	cfg := st.Snapshot()
	key := config.MemberKey("gpt-5", cfg.ModelGroups[0].Members[0])
	cooldown := cfg.Routing.Cooldown
	cooldown.FailThreshold = 1
	cooldown.OpenDuration = "1s"
	st.State().Failure(key, cooldown)

	if checked, _ := prober.ProbeOnce(context.Background()); checked != 0 {
		t.Fatalf("组开关关闭时不应探测任何成员，实际 %d", checked)
	}
}

// 组开关开启后，组内所有异常成员都参与，不需要逐个成员标记。
func TestProbeOnceCoversAllMembersWhenGroupSwitchOn(t *testing.T) {
	upstream := chatStub(t, true)
	defer upstream.Close()

	prober, st := newProberGroup(t, upstream.URL, config.Group{
		Name: "gpt-5", Enabled: true, Probe: true,
		Members: []config.Member{
			{Product: "workbuddy", Model: "m1", Weight: 20, Enabled: true},
			{Product: "workbuddy", Model: "m2", Weight: 10, Enabled: true},
		},
	})
	cfg := st.Snapshot()
	cooldown := cfg.Routing.Cooldown
	cooldown.FailThreshold = 1
	cooldown.OpenDuration = "1s"
	for _, member := range cfg.ModelGroups[0].Members {
		st.State().Failure(config.MemberKey("gpt-5", member), cooldown)
	}

	// MinGap=1s 会在第二轮挡住同一产品；这里直接重置节流时间验证两个成员都能探。
	prober.last = map[string]time.Time{}
	checked, recovered := prober.ProbeOnce(context.Background())
	if checked != 2 || recovered != 2 {
		t.Fatalf("组开关开启应覆盖全部成员，实际 checked=%d recovered=%d", checked, recovered)
	}
}

// 正常成员不探，避免无意义打下游。
func TestProbeOnceSkipsHealthyMember(t *testing.T) {
	upstream := chatStub(t, true)
	defer upstream.Close()

	prober, _ := newProber(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "healthy", Weight: 10, Enabled: true},
	})
	if checked, _ := prober.ProbeOnce(context.Background()); checked != 0 {
		t.Fatalf("正常成员不应探测，实际 %d", checked)
	}
}

// 解除时间还在未来（例如 1 小时后）→ 不提前探测，避免无意义打下游。
func TestProbeOnceSkipsNotDueMember(t *testing.T) {
	upstream := chatStub(t, true)
	defer upstream.Close()

	prober, st := newProber(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 10, Enabled: true},
	})
	cfg := st.Snapshot()
	key := config.MemberKey("gpt-5", cfg.ModelGroups[0].Members[0])
	cooldown := cfg.Routing.Cooldown
	cooldown.FailThreshold = 1
	cooldown.OpenDuration = "1h"
	st.State().Failure(key, cooldown)

	if checked, _ := prober.ProbeOnce(context.Background()); checked != 0 {
		t.Fatalf("未到解除时间不应探测，实际 %d", checked)
	}
}
