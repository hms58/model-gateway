package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"model-gateway/internal/config"
)

var ErrFetchFailed = errors.New("拉取下游模型列表失败")

// Entry 是某产品模型列表的落盘形态：模型、更新时间、来源与最近一次失败原因。
type Entry struct {
	Models    []string `json:"models"`
	UpdatedAt string   `json:"updated_at,omitempty"`
	Source    string   `json:"source,omitempty"` // upstream
	Failure   string   `json:"failure,omitempty"`
}

type entry struct {
	models  []string
	expire  time.Time
	failure bool
}

// Client 缓存并持久化产品模型列表：
// 内存缓存 → 文件缓存（未过期）→ 上游拉取；重启后优先用文件，过期或缺失才回源。
type Client struct {
	httpClient *http.Client
	OKTTL      time.Duration
	FailTTL    time.Duration
	now        func() time.Time

	mu   sync.Mutex
	path string
	file map[string]Entry // 与 models.json 同步的落盘内容
	// 内存命中层：避免每次都读文件
	cache map[string]entry
}

func New() *Client {
	return NewWithFile("")
}

// NewWithFile 指定模型缓存的落盘文件；path 为空表示只走内存。
func NewWithFile(path string) *Client {
	client := &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		OKTTL:      10 * time.Minute,
		FailTTL:    5 * time.Minute,
		now:        time.Now,
		path:       path,
		file:       map[string]Entry{},
		cache:      map[string]entry{},
	}
	client.loadFile()
	return client
}

// Fetch 返回产品的模型 id 列表；refresh=true 强制回源。
// 上游失败但文件里有历史数据时，返回历史数据（标记 failure），保证面板/选路不至于空白。
func (c *Client) Fetch(ctx context.Context, product config.Product, refresh bool) ([]string, error) {
	if !refresh {
		if models, ok, err := c.lookup(product.ID); ok {
			return models, err
		}
		if models, ok := c.fromFile(product.ID); ok {
			return models, nil
		}
	}

	models, err := c.fetch(ctx, product)
	if err != nil {
		c.storeFailure(product.ID, err)
		if stale, ok := c.stale(product.ID); ok {
			return stale, nil
		}
		return nil, err
	}
	c.store(product.ID, models)
	return models, nil
}

// Snapshot 返回当前已知的全部产品模型（含更新时间），供面板列表页展示。
func (c *Client) Snapshot() map[string]Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]Entry, len(c.file))
	for id, item := range c.file {
		copied := item
		copied.Models = append([]string(nil), item.Models...)
		out[id] = copied
	}
	return out
}

// Invalidate 清掉某产品的缓存（产品配置变更后调用）；落盘数据保留，避免误删历史。
func (c *Client) Invalidate(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, id)
}

func (c *Client) lookup(id string) ([]string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.cache[id]
	if !ok || c.now().After(item.expire) {
		return nil, false, nil
	}
	if item.failure {
		return nil, true, ErrFetchFailed
	}
	return item.models, true, nil
}

// fromFile 把「文件里未过期」的模型提升为内存缓存并返回。
func (c *Client) fromFile(id string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.file[id]
	if !ok || len(item.Models) == 0 || item.UpdatedAt == "" {
		return nil, false
	}
	updated, err := time.ParseInLocation(config.TimeLayout, item.UpdatedAt, config.Location())
	if err != nil || c.now().After(updated.Add(c.OKTTL)) {
		return nil, false
	}
	c.cache[id] = entry{models: append([]string(nil), item.Models...), expire: updated.Add(c.OKTTL)}
	return append([]string(nil), item.Models...), true
}

// stale 返回文件里的历史数据（即使已过期），用于上游失败时的兜底。
func (c *Client) stale(id string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.file[id]
	if !ok || len(item.Models) == 0 {
		return nil, false
	}
	return append([]string(nil), item.Models...), true
}

func (c *Client) store(id string, models []string) {
	c.mu.Lock()
	c.cache[id] = entry{models: models, expire: c.now().Add(c.OKTTL)}
	c.file[id] = Entry{
		Models:    append([]string(nil), models...),
		UpdatedAt: c.now().In(config.Location()).Format(config.TimeLayout),
		Source:    "upstream",
	}
	c.mu.Unlock()
	c.saveFile()
}

func (c *Client) storeFailure(id string, err error) {
	c.mu.Lock()
	c.cache[id] = entry{expire: c.now().Add(c.FailTTL), failure: true}
	if item, ok := c.file[id]; ok {
		item.Failure = err.Error()
		c.file[id] = item
	} else {
		c.file[id] = Entry{Failure: err.Error()}
	}
	c.mu.Unlock()
	c.saveFile()
}

func (c *Client) loadFile() {
	if c.path == "" {
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var payload map[string]Entry
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, item := range payload {
		c.file[id] = item
	}
}

func (c *Client) saveFile() {
	if c.path == "" {
		return
	}
	c.mu.Lock()
	payload, err := json.MarshalIndent(c.file, "", "  ")
	c.mu.Unlock()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return
	}
	temp := c.path + ".tmp"
	if err := os.WriteFile(temp, payload, 0o644); err != nil {
		return
	}
	_ = os.Rename(temp, c.path)
}

func (c *Client) fetch(ctx context.Context, product config.Product) ([]string, error) {
	url := strings.TrimRight(product.BaseURL, "/") + "/v1/models"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+product.APIKey)

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, ErrFetchFailed
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrFetchFailed
	}

	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, ErrFetchFailed
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, ErrFetchFailed
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID != "" {
			models = append(models, item.ID)
		}
	}
	return models, nil
}
