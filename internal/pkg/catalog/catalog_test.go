package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"model-gateway/internal/config"
)

func TestFetchCachesForTTL(t *testing.T) {
	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":"glm-5"}]}`))
	}))
	defer server.Close()

	client := New()
	client.now = func() time.Time { return time.Unix(1000, 0) }
	product := config.Product{ID: "workbuddy", BaseURL: server.URL, APIKey: "sk-x"}

	models, err := client.Fetch(context.Background(), product, false)
	if err != nil || len(models) != 2 {
		t.Fatalf("首次拉取失败：%v %v", models, err)
	}
	if _, err := client.Fetch(context.Background(), product, false); err != nil {
		t.Fatalf("缓存命中失败：%v", err)
	}
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("TTL 内不应重复请求，实际 %d 次", calls)
	}

	client.now = func() time.Time { return time.Unix(1000, 0).Add(11 * time.Minute) }
	if _, err := client.Fetch(context.Background(), product, false); err != nil {
		t.Fatalf("过期后应重新拉取：%v", err)
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("过期后应再请求一次，实际 %d 次", calls)
	}
}

func TestRefreshBypassesCache(t *testing.T) {
	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"}]}`))
	}))
	defer server.Close()

	client := New()
	product := config.Product{ID: "workbuddy", BaseURL: server.URL}
	if _, err := client.Fetch(context.Background(), product, false); err != nil {
		t.Fatalf("首次拉取失败：%v", err)
	}
	if _, err := client.Fetch(context.Background(), product, true); err != nil {
		t.Fatalf("强制刷新失败：%v", err)
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("refresh 应绕过缓存，实际 %d 次", calls)
	}
}

func TestFailureUsesNegativeCache(t *testing.T) {
	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	client := New()
	product := config.Product{ID: "trae", BaseURL: server.URL}
	if _, err := client.Fetch(context.Background(), product, false); !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("期望 ErrFetchFailed，实际 %v", err)
	}
	if _, err := client.Fetch(context.Background(), product, false); !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("负缓存应继续返回错误，实际 %v", err)
	}
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("负缓存期内不应重复请求，实际 %d 次", calls)
	}
}

// 模型列表要能落盘：重启后优先读文件，文件缺失/过期才回源。
func TestFetchPersistsAndReloadsFromFile(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":"gpt-4o"}]}`))
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "models.json")
	product := config.Product{ID: "workbuddy", BaseURL: upstream.URL, APIKey: "sk-test"}

	first := NewWithFile(path)
	models, err := first.Fetch(context.Background(), product, false)
	if err != nil || len(models) != 2 {
		t.Fatalf("首次拉取失败：%v %v", models, err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("首次应打一次上游，实际 %d", hits)
	}

	// 模拟重启：新 client 指向同一个文件，不应再打上游。
	second := NewWithFile(path)
	reloaded, err := second.Fetch(context.Background(), product, false)
	if err != nil || len(reloaded) != 2 {
		t.Fatalf("重启后应读到文件缓存：%v %v", reloaded, err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("文件未过期不应回源，实际请求 %d 次", hits)
	}
	if entry := second.Snapshot()["workbuddy"]; entry.UpdatedAt == "" {
		t.Fatal("应记录模型列表更新时间")
	}

	// refresh=true 强制回源
	if _, err := second.Fetch(context.Background(), product, true); err != nil {
		t.Fatalf("强制刷新失败：%v", err)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("refresh 应回源一次，实际 %d", hits)
	}
}
