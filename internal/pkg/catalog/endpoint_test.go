package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"model-gateway/internal/config"
)

// 自定义 models_path 必须生效，且产品自定义头要一并带上（与转发/探活保持一致）。
func TestFetchUsesCustomModelsPathAndHeaders(t *testing.T) {
	var gotPath, gotAuth, gotExtra string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotExtra = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gemini-2.5-pro"}]}`))
	}))
	defer upstream.Close()

	product := config.Product{
		ID: "gemini", BaseURL: upstream.URL, APIKey: "g-key",
		// Gemini 兼容层：没有 /v1 段。
		ModelsPath: "/v1beta/openai/models",
		Headers:    map[string]string{"X-Api-Key": "extra-token"},
	}
	models, err := New().Fetch(context.Background(), product, true)
	if err != nil {
		t.Fatalf("拉取失败：%v", err)
	}
	if gotPath != "/v1beta/openai/models" {
		t.Fatalf("应请求自定义 models_path，实际 %q", gotPath)
	}
	if gotAuth != "Bearer g-key" {
		t.Fatalf("应带产品 key，实际 %q", gotAuth)
	}
	if gotExtra != "extra-token" {
		t.Fatalf("产品自定义头未透传，实际 %q", gotExtra)
	}
	if len(models) != 1 || models[0] != "gemini-2.5-pro" {
		t.Fatalf("模型解析错误：%v", models)
	}
}

// 留空时仍走默认 /v1/models，不能因为新增字段破坏存量配置。
func TestFetchDefaultsToV1Models(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	product := config.Product{ID: "p", BaseURL: upstream.URL + "/", APIKey: "k"}
	if _, err := New().Fetch(context.Background(), product, true); err != nil {
		t.Fatalf("拉取失败：%v", err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("默认应请求 /v1/models，实际 %q", gotPath)
	}
}
