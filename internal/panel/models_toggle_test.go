package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/catalog"
)

// withCatalog 把「产品 → 模型清单」写进临时 models.json 并接到面板上：
// 面板的模型目录来自落盘文件，测试里必须造出这份文件，否则目录是空的。
func withCatalog(t *testing.T, server *Server, models map[string][]string) *Server {
	t.Helper()
	dir := t.TempDir()
	stamp := time.Now().In(config.Location()).Format(config.TimeLayout)
	payload := make(map[string]catalog.Entry, len(models))
	for id, list := range models {
		payload[id] = catalog.Entry{Models: list, UpdatedAt: stamp, Source: "upstream"}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化模型目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), raw, 0o644); err != nil {
		t.Fatalf("写模型目录失败：%v", err)
	}
	return server.WithModelsPath(dir)
}

func toggleBody(version int, product, model string, enabled bool) map[string]interface{} {
	return map[string]interface{}{
		"version": version, "product": product, "model": model, "enabled": enabled,
	}
}

// 正在被模型组使用的模型不能停用：否则面板显示停用、流量照旧打过去。
func TestToggleModelRejectsModelInUse(t *testing.T) {
	server, store := newServer(t)
	handler := withCatalog(t, server, map[string][]string{"workbuddy": {"gpt-5", "gpt-5-mini"}}).Handler()

	recorder := call(t, handler, http.MethodPost, "/api/modelgw/models/toggle",
		toggleBody(store.Snapshot().Version, "workbuddy", "gpt-5", false))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("停用在用模型应返回 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	for _, want := range []string{"模型组使用", "gpt-5"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("错误信息应包含 %q：%s", want, recorder.Body.String())
		}
	}
	if store.Snapshot().Products[0].ModelDisabled("gpt-5") {
		t.Fatal("被拒绝的停用不该写进配置")
	}
}

// 停用未使用的模型：模型页仍列出（否则点不回来），但候选池里不再出现。
func TestToggleModelHidesFromCandidates(t *testing.T) {
	server, store := newServer(t)
	handler := withCatalog(t, server, map[string][]string{"workbuddy": {"gpt-5", "gpt-5-mini"}}).Handler()

	recorder := call(t, handler, http.MethodPost, "/api/modelgw/models/toggle",
		toggleBody(store.Snapshot().Version, "workbuddy", "gpt-5-mini", false))
	if recorder.Code != http.StatusOK {
		t.Fatalf("停用应成功，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !store.Snapshot().Products[0].ModelDisabled("gpt-5-mini") {
		t.Fatal("停用状态应写进配置")
	}

	listing := call(t, handler, http.MethodGet, "/api/modelgw/models", nil)
	var payload struct {
		Data []struct {
			ID           string   `json:"id"`
			Models       []string `json:"models"`
			Disabled     []string `json:"disabled"`
			EnabledCount int      `json:"enabled_count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listing.Body.Bytes(), &payload); err != nil {
		t.Fatalf("模型列表不是合法 JSON：%v", err)
	}
	if len(payload.Data) != 1 {
		t.Fatalf("应只有 1 个启用产品，实际 %d", len(payload.Data))
	}
	row := payload.Data[0]
	if len(row.Models) != 2 {
		t.Fatalf("停用的模型仍要在列表里（否则无法恢复）：%+v", row.Models)
	}
	if len(row.Disabled) != 1 || row.Disabled[0] != "gpt-5-mini" {
		t.Fatalf("disabled 应只含被停用的那个：%+v", row.Disabled)
	}
	if row.EnabledCount != 1 {
		t.Fatalf("启用数应为 1，实际 %d", row.EnabledCount)
	}

	// 模型组编辑器的候选池走 /catalog/fetch：停用的模型不能出现在这里。
	candidates := call(t, handler, http.MethodGet, "/api/modelgw/catalog/fetch?product=workbuddy", nil)
	if strings.Contains(candidates.Body.String(), "gpt-5-mini") {
		t.Fatalf("停用模型不该进候选池：%s", candidates.Body.String())
	}
	if !strings.Contains(candidates.Body.String(), "gpt-5") {
		t.Fatalf("启用模型应保留在候选池：%s", candidates.Body.String())
	}

	// 再启用回来：状态与候选池都要恢复。
	recorder = call(t, handler, http.MethodPost, "/api/modelgw/models/toggle",
		toggleBody(store.Snapshot().Version, "workbuddy", "gpt-5-mini", true))
	if recorder.Code != http.StatusOK {
		t.Fatalf("启用应成功，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if store.Snapshot().Products[0].ModelDisabled("gpt-5-mini") {
		t.Fatal("启用后配置里不该还有该模型")
	}
	candidates = call(t, handler, http.MethodGet, "/api/modelgw/catalog/fetch?product=workbuddy", nil)
	if !strings.Contains(candidates.Body.String(), "gpt-5-mini") {
		t.Fatalf("启用后应回到候选池：%s", candidates.Body.String())
	}
}

// 目录里没有的模型名不允许写进停用集：拼错的名字永远匹配不上，只会变成脏配置。
func TestToggleModelRejectsUnknownModel(t *testing.T) {
	server, store := newServer(t)
	handler := withCatalog(t, server, map[string][]string{"workbuddy": {"gpt-5"}}).Handler()

	recorder := call(t, handler, http.MethodPost, "/api/modelgw/models/toggle",
		toggleBody(store.Snapshot().Version, "workbuddy", "gpt-5-typo", false))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("未知模型应返回 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// 停用的产品整组不列出：它的模型既不能加进组，也不该在目录里制造噪音。
func TestModelsSkipsDisabledProducts(t *testing.T) {
	server, store := newServer(t)
	cfg := store.Snapshot().Clone()
	cfg.Products = append(cfg.Products, config.Product{
		ID: "trae", Name: "TRAE", Enabled: false, BaseURL: "http://127.0.0.1:7864",
	})
	if err := store.Save(cfg, cfg.Version); err != nil {
		t.Fatalf("保存配置失败：%v", err)
	}
	handler := withCatalog(t, server, map[string][]string{
		"workbuddy": {"gpt-5"}, "trae": {"glm-5.3"},
	}).Handler()

	body := call(t, handler, http.MethodGet, "/api/modelgw/models", nil).Body.String()
	if strings.Contains(body, `"trae"`) {
		t.Fatalf("停用产品不该出现在模型列表：%s", body)
	}
	if !strings.Contains(body, `"workbuddy"`) {
		t.Fatalf("启用产品应保留：%s", body)
	}
}

// 模型页必须有停用/启用入口，否则状态存下来也没法改回去。
func TestPanelModelsOfferToggle(t *testing.T) {
	server, _ := newServer(t)
	body := call(t, server.Handler(), http.MethodGet, "/panel/models", nil).Body.String()
	// 行高亮与「停用置灰」样式必须存在：按钮在行尾，悬停太淡会点错行，
	// 停用行不置灰则要靠读状态列的文字才能分清哪些模型不能选。
	for _, want := range []string{
		`data-act="toggle-model"`, "停用的产品不会出现在这里",
		"#models-body tr.card:hover td", "#models-body tr.card.off td",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("模型页缺少 %q", want)
		}
	}
}
