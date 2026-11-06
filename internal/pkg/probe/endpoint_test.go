package probe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"model-gateway/internal/config"
)

// 探活必须走产品自定义的 chat_path，否则 Gemini/Azure 这类端点永远探不通。
func TestModelUsesCustomChatPath(t *testing.T) {
	var gotPath, gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		gotModel, _ = payload["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	product := config.Product{
		ID: "gemini", BaseURL: upstream.URL, APIKey: "g-key",
		ChatPath: "/v1beta/openai/chat/completions",
	}
	result := Model(context.Background(), product, "gemini-2.5-pro", 5*time.Second)
	if !result.OK {
		t.Fatalf("探活应成功，实际失败：%s", result.Reason)
	}
	if gotPath != "/v1beta/openai/chat/completions" {
		t.Fatalf("应请求自定义 chat_path，实际 %q", gotPath)
	}
	if gotModel != "gemini-2.5-pro" {
		t.Fatalf("应把成员模型写进请求体，实际 %q", gotModel)
	}
}

// 留空时仍打默认 /v1/chat/completions。
func TestModelDefaultsToV1ChatCompletions(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	product := config.Product{ID: "p", BaseURL: upstream.URL + "/", APIKey: "k"}
	if result := Model(context.Background(), product, "m", 5*time.Second); !result.OK {
		t.Fatalf("探活应成功，实际失败：%s", result.Reason)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("默认应请求 /v1/chat/completions，实际 %q", gotPath)
	}
}
