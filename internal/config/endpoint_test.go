package config

import "testing"

// 端点拼接必须覆盖默认值、尾部斜杠、缺前导斜杠与带查询串（Azure）四种形态。
func TestProductEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name       string
		product    Product
		wantChat   string
		wantModels string
	}{
		{
			name:       "全部默认",
			product:    Product{BaseURL: "https://api.openai.com"},
			wantChat:   "https://api.openai.com/v1/chat/completions",
			wantModels: "https://api.openai.com/v1/models",
		},
		{
			name:       "base_url 尾部斜杠",
			product:    Product{BaseURL: "https://openrouter.ai/api/"},
			wantChat:   "https://openrouter.ai/api/v1/chat/completions",
			wantModels: "https://openrouter.ai/api/v1/models",
		},
		{
			name: "Gemini 兼容层（无 /v1 段）",
			product: Product{
				BaseURL:    "https://generativelanguage.googleapis.com/v1beta/openai",
				ChatPath:   "/chat/completions",
				ModelsPath: "/models",
			},
			wantChat:   "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			wantModels: "https://generativelanguage.googleapis.com/v1beta/openai/models",
		},
		{
			name: "Azure 带查询串",
			product: Product{
				BaseURL:  "https://x.openai.azure.com",
				ChatPath: "/openai/deployments/gpt4o/chat/completions?api-version=2024-06-01",
			},
			wantChat:   "https://x.openai.azure.com/openai/deployments/gpt4o/chat/completions?api-version=2024-06-01",
			wantModels: "https://x.openai.azure.com/v1/models",
		},
		{
			name:       "自定义路径缺前导斜杠也要容错",
			product:    Product{BaseURL: "https://api.x.com", ChatPath: "v2/chat", ModelsPath: "v2/models"},
			wantChat:   "https://api.x.com/v2/chat",
			wantModels: "https://api.x.com/v2/models",
		},
		{
			name:       "路径只有空白视为默认",
			product:    Product{BaseURL: "https://api.x.com", ChatPath: "   ", ModelsPath: "  "},
			wantChat:   "https://api.x.com/v1/chat/completions",
			wantModels: "https://api.x.com/v1/models",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.product.ChatEndpoint(); got != tc.wantChat {
				t.Fatalf("chat 端点应为 %q，实际 %q", tc.wantChat, got)
			}
			if got := tc.product.ModelsEndpoint(); got != tc.wantModels {
				t.Fatalf("models 端点应为 %q，实际 %q", tc.wantModels, got)
			}
		})
	}
}

func TestValidateEndpointPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		ok   bool
	}{
		{"空值用默认", "", true},
		{"正常路径", "/v1/chat/completions", true},
		{"带查询串", "/openai/deployments/x/chat/completions?api-version=2024-06-01", true},
		{"缺前导斜杠", "v1/chat/completions", false},
		{"完整 URL", "https://api.openai.com/v1/chat/completions", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEndpointPath("chat_path", tc.path)
			if (err == nil) != tc.ok {
				t.Fatalf("%q 期望 ok=%v，实际 err=%v", tc.path, tc.ok, err)
			}
		})
	}
}

// 非法路径必须在保存时被拦下，而不是等到真实请求 404。
func TestConfigRejectsInvalidEndpointPath(t *testing.T) {
	cfg := validConfig()
	cfg.Products[0].ChatPath = "https://evil.example/v1/chat/completions"
	if err := Validate(&cfg); err == nil {
		t.Fatal("完整 URL 作为 chat_path 应校验失败")
	}

	cfg = validConfig()
	cfg.Products[0].ModelsPath = "v1/models"
	if err := Validate(&cfg); err == nil {
		t.Fatal("缺前导斜杠的 models_path 应校验失败")
	}

	// Gemini 兼容层的合法配置必须能保存。
	cfg = validConfig()
	cfg.Products[0].BaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	cfg.Products[0].ChatPath = "/chat/completions"
	cfg.Products[0].ModelsPath = "/models"
	if err := Validate(&cfg); err != nil {
		t.Fatalf("Gemini 兼容层配置应合法：%v", err)
	}
}

// 产品后台地址只放行 http/https 绝对地址：面板会把它原样渲染成可点击链接。
func TestValidateConsoleURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		ok   bool
	}{
		{"留空表示不配置", "", true},
		{"纯空白视为不配置", "   ", true},
		{"http", "http://127.0.0.1:7863/admin", true},
		{"https 带查询串", "https://console.example.com/p/x?tab=1", true},
		{"缺 scheme", "127.0.0.1:7863/admin", false},
		{"javascript scheme", "javascript:alert(1)", false},
		{"file scheme", "file:///tmp/x", false},
		{"只有 scheme 没有主机", "http://", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConsoleURL(tc.url)
			if (err == nil) != tc.ok {
				t.Fatalf("%q 期望 ok=%v，实际 err=%v", tc.url, tc.ok, err)
			}
		})
	}
}

func TestConfigRejectsInvalidConsoleURL(t *testing.T) {
	cfg := validConfig()
	cfg.Products[0].ConsoleURL = "javascript:alert(1)"
	if err := Validate(&cfg); err == nil {
		t.Fatal("javascript: 作为 console_url 应校验失败")
	}

	cfg = validConfig()
	cfg.Products[0].ConsoleURL = "http://127.0.0.1:7863/admin"
	if err := Validate(&cfg); err != nil {
		t.Fatalf("合法的 console_url 应能保存：%v", err)
	}
}
