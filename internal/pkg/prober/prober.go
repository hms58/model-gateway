// Package prober 负责「异常成员的异步恢复」。
//
// 背景：选路只按 weight 降序取第一个可用成员，低权重成员一旦冷却 / 熔断，
// 只有等更高权重的成员全都失败时才会有真实流量落到它身上 —— 也就是可能出现
// 「已经恢复了，但永远没机会被验证」。这里用后台定时探活补上这条路：
// 对已到期（或即将到期）的成员打一次下游 /v1/models（零 token 成本），
// 探到即调 State.Probe(ok) 清掉冷却 / 熔断。
package prober

import (
	"context"
	"sync"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/probe"
	"model-gateway/internal/pkg/store"
	"model-gateway/internal/router"
)

// probeTimeout 是单次探活的超时：探活本身要快，不能拖住下一轮扫描。
const probeTimeout = 30 * time.Second

// target 是「某个组里的某个成员」，key 需要组名才能算。
type target struct {
	group  string
	member config.Member
}

// Prober 按配置间隔扫描并探活；同一产品在 MinGap 内只打一次上游。
type Prober struct {
	store *store.Store

	mu   sync.Mutex
	last map[string]time.Time
}

func New(st *store.Store) *Prober {
	return &Prober{store: st, last: map[string]time.Time{}}
}

// Run 周期扫描直到 stop 关闭。
// 间隔与开关每轮都从配置快照读取，面板改完不用重启（改小了下一轮就生效）。
func (p *Prober) Run(stop <-chan struct{}) {
	for {
		settings := p.store.Snapshot().Probe
		interval := parseDuration(settings.Interval, 60*time.Second)
		select {
		case <-stop:
			return
		case <-time.After(interval):
		}
		if !p.store.Snapshot().Probe.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), interval)
		p.ProbeOnce(ctx)
		cancel()
	}
}

// ProbeOnce 扫一遍配置，对处于冷却 / 熔断、且解除时间在 dueWindow 内的启用成员做探活。
// 返回本轮探测的成员数与恢复数，便于测试与观测。
func (p *Prober) ProbeOnce(ctx context.Context) (checked, recovered int) {
	cfg := p.store.Snapshot()
	if !cfg.Probe.Enabled {
		return 0, 0
	}
	now := time.Now().In(config.Location())
	state := p.store.State()

	products := make(map[string]config.Product, len(cfg.Products))
	for _, product := range cfg.Products {
		products[product.ID] = product
	}

	// 按产品聚合：一个产品在本轮只发一次探活（节流在 allow 里）。
	pending := map[string][]target{}
	for _, group := range cfg.ModelGroups {
		if !group.Enabled {
			continue
		}
		for _, member := range group.Members {
			if !member.Enabled {
				continue
			}
			// 默认关闭：只有成员上显式开启「异步嗅探」才参与，避免无谓的真实请求。
			if !member.Probe {
				continue
			}
			product, ok := products[member.Product]
			if !ok || !product.Enabled {
				continue
			}
			if !router.InWindow(member, now) {
				continue
			}
			status := state.StatusOf(config.MemberKey(group.Name, member))
			// 只对异常成员探活：正常成员不打扰（正常与否由真实请求结果决定）。
			if status.State == "ok" {
				continue
			}
			if !dueSoon(status.UnlockAt, now, 30*time.Second) {
				continue
			}
			pending[member.Product] = append(pending[member.Product], target{group: group.Name, member: member})
		}
	}

	minGap := parseDuration(cfg.Probe.MinGap, 30*time.Second)
	for productID, items := range pending {
		if !p.allow(productID, minGap, time.Now()) {
			continue
		}
		product := products[productID]
		for _, item := range items {
			key := config.MemberKey(item.group, item.member)
			checked++
			result := probe.Model(ctx, product, item.member.Model, probeTimeout)
			if !result.OK {
				state.Probe(key, false, result.Reason)
				continue
			}
			state.Probe(key, true, "")
			recovered++
		}
	}
	if checked > 0 {
		p.store.MarkDirty()
	}
	return checked, recovered
}

// allow 做产品级节流：同一产品在 gap 内只打一次上游，避免多成员重复探测。
func (p *Prober) allow(productID string, gap time.Duration, now time.Time) bool {
	// 时钟回拨时 now 可能早于上次记录，直接放行而不是永久卡住。
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.last[productID]; ok && now.After(last) && now.Sub(last) < gap {
		return false
	}
	p.last[productID] = now
	return true
}

// dueSoon 判断解除时间是否为空（立即）、已过期，或在 window 内到期。
func dueSoon(unlockAt string, now time.Time, window time.Duration) bool {
	if unlockAt == "" {
		return true
	}
	until, err := time.ParseInLocation(config.TimeLayout, unlockAt, config.Location())
	if err != nil {
		return true
	}
	return !until.After(now.Add(window))
}

func parseDuration(raw string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
