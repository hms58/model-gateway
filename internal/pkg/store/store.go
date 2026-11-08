package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/router"
)

// stateFile 是 `state.json` 的落盘结构：只存可恢复的最小集合。
type stateFile struct {
	Version  int                            `json:"version"`
	Members  map[string]router.MemberState  `json:"members"`
	Products map[string]router.ProductState `json:"products,omitempty"`
	Tokens   map[string]router.TokenState   `json:"tokens,omitempty"`
}

// Store 持有配置快照与运行态：数据面只读快照，控制面整体替换，互不阻塞。
type Store struct {
	// saveMu 串行化「读版本 → 比对 → 写盘」整段：版本检查是 read-check-write，
	// 两个并发保存读到同一版本时会双双通过检查，后写覆盖先写、静默丢改动。
	saveMu    sync.Mutex
	mu        sync.RWMutex
	cfgPath   string
	statePath string
	snapshot  atomic.Value // *config.Config
	state     *router.State
	version   int
	// stateVersion 只用于 state.json 的版本号，与配置版本互不相干。
	stateVersion int
	dirty        int32
}

// Open 加载配置与运行态；state 文件不存在属正常首次启动。
func Open(cfgPath, statePath string) (*Store, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	s := &Store{
		cfgPath:   cfgPath,
		statePath: statePath,
		state:     router.NewState(nil),
		version:   cfg.Version,
	}
	s.snapshot.Store(&cfg)
	if err := s.LoadState(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// Snapshot 返回当前配置快照；调用方只读，不要修改其字段。
func (s *Store) Snapshot() *config.Config {
	value, _ := s.snapshot.Load().(*config.Config)
	return value
}

// State 返回运行态（冷却 / 熔断 / 粘性）。
func (s *Store) State() *router.State { return s.state }

// Save 按版本号保存配置并替换快照；版本不一致返回 config.ErrVersionConflict。
func (s *Store) Save(cfg config.Config, expect int) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	if err := config.SaveWithVersion(s.cfgPath, &cfg, expect); err != nil {
		return err
	}
	saved, err := config.Load(s.cfgPath)
	if err != nil {
		return err
	}
	s.snapshot.Store(&saved)
	s.mu.Lock()
	s.version = saved.Version
	s.mu.Unlock()
	return nil
}

// Reload 重新读盘并替换快照（面板或外部改动后调用）。
func (s *Store) Reload() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		return err
	}
	s.snapshot.Store(&cfg)
	s.mu.Lock()
	s.version = cfg.Version
	s.mu.Unlock()
	return nil
}

// Rollback 用 `.bak` 恢复上一版配置；版本号继续递增，不回退。
// 回滚是「配置被改坏」时的自救通道，所以读当前版本同样只解析 version 字段，
// 不走全量校验（否则当前文件非法时连回滚都做不到）。
func (s *Store) Rollback() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	raw, err := os.ReadFile(s.cfgPath + ".bak")
	if err != nil {
		return err
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var restored config.Config
	if err := json.Unmarshal(raw, &restored); err != nil {
		return err
	}
	version, err := config.ReadVersion(s.cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	restored.Version = version + 1
	if err := config.SaveAtomic(s.cfgPath, &restored); err != nil {
		return err
	}
	s.snapshot.Store(&restored)
	s.mu.Lock()
	s.version = restored.Version
	s.mu.Unlock()
	return nil
}

// LoadState 从 `state.json` 恢复冷却与熔断状态。
func (s *Store) LoadState() error {
	raw, err := os.ReadFile(s.statePath)
	if err != nil {
		return err
	}
	var payload stateFile
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	s.state.Load(router.Snapshot{
		Members:  payload.Members,
		Products: payload.Products,
		Tokens:   payload.Tokens,
	})
	return nil
}

// SaveState 原子写回运行态。
//
// 脏标记在导出之前清掉、写盘失败再置回：反过来（写完才清）会把导出与清标记
// 之间发生的变更一起清掉，那次改动就要等下一次变更才可能落盘。
func (s *Store) SaveState() error {
	atomic.StoreInt32(&s.dirty, 0)

	s.mu.Lock()
	// state.json 用自己的版本号：借配置版本会在每次落盘时改动 s.version，
	// 让「配置版本」与「运行态版本」互相污染。
	s.stateVersion++
	version := s.stateVersion
	s.mu.Unlock()

	snapshot := s.state.Export()
	payload := stateFile{
		Version:  version,
		Members:  snapshot.Members,
		Products: snapshot.Products,
		Tokens:   snapshot.Tokens,
	}
	if err := s.writeStateFile(payload); err != nil {
		atomic.StoreInt32(&s.dirty, 1)
		return err
	}
	return nil
}

func (s *Store) writeStateFile(payload stateFile) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.statePath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath)
}

// MarkDirty 标记运行态需要落盘；请求热路径只做一次原子写，不碰磁盘。
func (s *Store) MarkDirty() { atomic.StoreInt32(&s.dirty, 1) }

// FlushIfDirty 有变更才落盘，供定时 flush 与退出前调用。
func (s *Store) FlushIfDirty() error {
	if atomic.LoadInt32(&s.dirty) == 0 {
		return nil
	}
	return s.SaveState()
}

// RunStateFlusher 定时合并落盘，stop 关闭时强制 flush 一次。
func (s *Store) RunStateFlusher(stop <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = s.FlushIfDirty()
		case <-stop:
			_ = s.SaveState()
			return
		}
	}
}
