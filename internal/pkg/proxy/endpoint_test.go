package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"model-gateway/internal/config"
)

// 转发必须走产品自定义的 chat_path（Gemini 兼容层没有 /v1 段）。
func TestDoResponseUsesCustomChatPath(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	product := config.Product{
		ID: "gemini", BaseURL: upstream.URL, APIKey: "g-key",
		ChatPath: "/v1beta/openai/chat/completions",
	}
	body, _ := json.Marshal(map[string]interface{}{"model": "m", "messages": []interface{}{}})
	response, err := NewForwarder().DoResponse(context.Background(), product, body, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("转发失败：%v", err)
	}
	defer response.Body.Close()
	if gotPath != "/v1beta/openai/chat/completions" {
		t.Fatalf("应请求自定义 chat_path，实际 %q", gotPath)
	}
}

// 流式与非流式必须用同一个端点，避免「非流式能通、流式 404」。
func TestDoStreamResponseUsesCustomChatPath(t *testing.T) {
	var gotPath, gotAccept, gotVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotVersion = r.URL.Query().Get("api-version")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer upstream.Close()

	product := config.Product{
		ID: "azure", BaseURL: upstream.URL, APIKey: "k",
		ChatPath: "/openai/deployments/gpt4o/chat/completions?api-version=2024-06-01",
	}
	body, _ := json.Marshal(map[string]interface{}{"model": "m", "messages": []interface{}{}})
	response, err := NewForwarder().DoStreamResponse(context.Background(), product, body, "gpt-4o")
	if err != nil {
		t.Fatalf("流式转发失败：%v", err)
	}
	defer response.Body.Close()
	if gotPath != "/openai/deployments/gpt4o/chat/completions" {
		t.Fatalf("路径错误：%q", gotPath)
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("流式应带 Accept 头，实际 %q", gotAccept)
	}
	// Azure 的 api-version 必须原样保留在查询串里，不能被当成路径转义掉。
	if gotVersion != "2024-06-01" {
		t.Fatalf("查询串丢失：api-version=%q", gotVersion)
	}
}
