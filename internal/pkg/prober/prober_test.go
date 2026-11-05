package prober

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/store"
)

// newProber 造一个「一个产品 + 一个组 + 一个成员」的最小环境。
func newProber(t *testing.T, upstream string, members []config.Member) (*Prober, *store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe = config.Probe{Enabled: true, Interval: "1s", MinGap: "1s"}
	cfg.Products = []config.Product{{ID: "workbuddy", Enabled: true, BaseURL: upstream, APIKey: "sk-test"}}
	cfg.ModelGroups = []config.Group{{Name: "gpt-5", Enabled: true, Members: members}}

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
		{Product: "workbuddy", Model: "gpt-5", Weight: 10, Enabled: true, Probe: true},
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
		{Product: "workbuddy", Model: "gpt-5", Weight: 10, Enabled: true, Probe: true},
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

// 未开启「异步嗅探」的成员不探；正常成员也不探。
func TestProbeOnceSkipsDisabledSwitchAndHealthy(t *testing.T) {
	upstream := chatStub(t, true)
	defer upstream.Close()

	prober, st := newProber(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "no-probe", Weight: 20, Enabled: true},
		{Product: "workbuddy", Model: "healthy", Weight: 10, Enabled: true, Probe: true},
	})
	cfg := st.Snapshot()
	key := config.MemberKey("gpt-5", cfg.ModelGroups[0].Members[0])
	cooldown := cfg.Routing.Cooldown
	cooldown.FailThreshold = 1
	cooldown.OpenDuration = "1s"
	st.State().Failure(key, cooldown)

	if checked, _ := prober.ProbeOnce(context.Background()); checked != 0 {
		t.Fatalf("未开开关的异常成员与正常成员都不应探测，实际 %d", checked)
	}
}

// 解除时间还在未来（例如 1 小时后）→ 不提前探测，避免无意义打下游。
func TestProbeOnceSkipsNotDueMember(t *testing.T) {
	upstream := chatStub(t, true)
	defer upstream.Close()

	prober, st := newProber(t, upstream.URL, []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 10, Enabled: true, Probe: true},
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
