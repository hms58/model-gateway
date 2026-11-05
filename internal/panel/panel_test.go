package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/store"
)

func newServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Products = []config.Product{{ID: "workbuddy", Enabled: true, BaseURL: "http://127.0.0.1:7863"}}
	cfg.ModelGroups = []config.Group{{
		Name: "gpt-5", Enabled: true,
		Members: []config.Member{{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true}},
	}}
	dir := t.TempDir()
	path := filepath.Join(dir, "modelgw.json")
	if err := config.SaveAtomic(path, &cfg); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}
	st, err := store.Open(path, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("打开 store 失败：%v", err)
	}
	return New(st, config.HashAdminToken("admin-token")), st
}

func call(t *testing.T, handler http.Handler, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("构造请求体失败：%v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer admin-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

// newBootstrapServer 模拟「配置里没写口令」的首次启动：管理面进入初始化模式。
func newBootstrapServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Products = []config.Product{{ID: "workbuddy", Enabled: true, BaseURL: "http://127.0.0.1:7863"}}
	dir := t.TempDir()
	path := filepath.Join(dir, "modelgw.json")
	if err := config.SaveAtomic(path, &cfg); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}
	st, err := store.Open(path, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("打开 store 失败：%v", err)
	}
	return New(st, ""), st
}

// callFrom 指定来源地址发一次请求，用于验证「初始化模式只允许本机」。
func callFrom(t *testing.T, handler http.Handler, remoteAddr, forwarded, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(nil))
	req.RemoteAddr = remoteAddr
	if forwarded != "" {
		req.Header.Set("X-Forwarded-For", forwarded)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

// 首次启动没有口令：本机访问免登录，可以直接进配置页设口令。
func TestBootstrapModeAllowsLoopbackWithoutCredential(t *testing.T) {
	server, _ := newBootstrapServer(t)
	if got := callFrom(t, server.Handler(), "127.0.0.1:5555", "", http.MethodGet, "/api/modelgw/products"); got.Code != http.StatusOK {
		t.Fatalf("初始化模式应放行本机访问，实际 %d：%s", got.Code, got.Body.String())
	}
	if got := callFrom(t, server.Handler(), "[::1]:5555", "", http.MethodGet, "/api/modelgw/settings"); got.Code != http.StatusOK {
		t.Fatalf("IPv6 回环也应放行，实际 %d", got.Code)
	}
}

// 反代把客户端地址放进 X-Forwarded-For：首段是回环才放行。
func TestBootstrapModeTrustsForwardedLoopback(t *testing.T) {
	server, _ := newBootstrapServer(t)
	if got := callFrom(t, server.Handler(), "10.0.0.2:5555", "127.0.0.1", http.MethodGet, "/api/modelgw/products"); got.Code != http.StatusOK {
		t.Fatalf("XFF 首段为回环应放行，实际 %d", got.Code)
	}
}

// 初始化模式绝不放过远程来源，否则「无口令」等于裸奔。
func TestBootstrapModeRejectsRemoteSource(t *testing.T) {
	server, _ := newBootstrapServer(t)
	recorder := callFrom(t, server.Handler(), "192.168.1.20:5555", "", http.MethodGet, "/api/modelgw/products")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("远程来源应拒绝，实际 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "尚未设置管理口令") {
		t.Fatalf("应说明初始化模式的原因：%s", recorder.Body.String())
	}

	spoofed := callFrom(t, server.Handler(), "192.168.1.20:5555", "203.0.113.9", http.MethodGet, "/api/modelgw/products")
	if spoofed.Code != http.StatusUnauthorized {
		t.Fatalf("XFF 首段非回环应拒绝，实际 %d", spoofed.Code)
	}
}

// 设了口令之后，回环来源也必须带正确口令，不能再靠「本机」白名单。
func TestConfiguredTokenStillRequiresCredentialFromLoopback(t *testing.T) {
	server, _ := newServer(t)
	recorder := callFrom(t, server.Handler(), "127.0.0.1:5555", "", http.MethodGet, "/api/modelgw/products")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("已设口令时本机也必须带口令，实际 %d", recorder.Code)
	}
}

// 初始化模式设置的第一个口令：写盘为摘要，重启后生效。
func TestBootstrapSetsFirstAdminToken(t *testing.T) {
	server, st := newBootstrapServer(t)
	payload := settingsPayload(st, func(settings map[string]interface{}) {
		settings["startup"].(map[string]interface{})["admin_token"] = "first-real-token"
	})
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("构造请求体失败：%v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/modelgw/settings", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("初始化模式应允许写配置，实际 %d：%s", rec.Code, rec.Body.String())
	}

	cfg := st.Snapshot()
	if cfg.AdminToken != "" {
		t.Fatalf("不应保留明文：%q", cfg.AdminToken)
	}
	if cfg.AdminTokenSHA256 != config.HashAdminToken("first-real-token") {
		t.Fatalf("口令摘要不正确：%s", cfg.AdminTokenSHA256)
	}
}

func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) int {
	t.Helper()
	var payload struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("错误响应不是合法 JSON：%v（%s）", err, recorder.Body.String())
	}
	return payload.Error.Code
}

func TestRejectsMissingAdminToken(t *testing.T) {
	server, _ := newServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/modelgw/products", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d", recorder.Code)
	}
}

func TestSaveProductBumpsVersion(t *testing.T) {
	server, st := newServer(t)
	version := st.Snapshot().Version
	recorder := call(t, server.Handler(), http.MethodPost, "/api/modelgw/products", map[string]interface{}{
		"version": version,
		"product": map[string]interface{}{
			"id": "trae", "name": "Trae", "protocol": "openai-chat",
			"base_url": "http://127.0.0.1:7864", "api_key": "sk-x", "enabled": true,
		},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if st.Snapshot().Version != version+1 {
		t.Fatalf("版本应 +1：%d → %d", version, st.Snapshot().Version)
	}
	if len(st.Snapshot().Products) != 2 {
		t.Fatalf("产品应变为 2 个：%+v", st.Snapshot().Products)
	}
}

func TestSaveProductStaleVersionReturns409(t *testing.T) {
	server, _ := newServer(t)
	recorder := call(t, server.Handler(), http.MethodPost, "/api/modelgw/products", map[string]interface{}{
		"version": 99,
		"product": map[string]interface{}{"id": "trae", "enabled": true, "base_url": "http://127.0.0.1:7864"},
	})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("期望 409，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if code := errorCode(t, recorder); code != CodeVersionConflict {
		t.Fatalf("期望 20607，实际 %d", code)
	}
}

func TestDeleteReferencedProductRejected(t *testing.T) {
	server, _ := newServer(t)
	recorder := call(t, server.Handler(), http.MethodDelete, "/api/modelgw/products/workbuddy?version=1", nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if code := errorCode(t, recorder); code != CodeProductInUse {
		t.Fatalf("期望 20602，实际 %d", code)
	}
}

func TestSaveGroupWithDuplicateMemberRejected(t *testing.T) {
	server, st := newServer(t)
	version := st.Snapshot().Version
	recorder := call(t, server.Handler(), http.MethodPost, "/api/modelgw/model-groups", map[string]interface{}{
		"version": version,
		"group": map[string]interface{}{
			"name": "gpt-5", "enabled": true,
			"members": []map[string]interface{}{
				{"product": "workbuddy", "model": "gpt-5", "weight": 100, "enabled": true},
				{"product": "workbuddy", "model": "gpt-5", "weight": 100, "enabled": true},
			},
		},
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if code := errorCode(t, recorder); code != CodeMemberDuplicate {
		t.Fatalf("期望 20605，实际 %d", code)
	}
}

func TestFetchModelsCachesResult(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":"glm-5"}]}`))
	}))
	defer upstream.Close()

	server, _ := newServer(t)
	// 把已有产品的地址指到假下游
	cfg := *server.store.Snapshot()
	cfg.Products[0].BaseURL = upstream.URL
	cfg.Products[0].APIKey = "sk-x"
	if err := server.store.Save(cfg, cfg.Version); err != nil {
		t.Fatalf("改 product 失败：%v", err)
	}

	handler := server.Handler()
	for i := 0; i < 2; i++ {
		recorder := call(t, handler, http.MethodGet, "/api/modelgw/catalog/fetch?product=workbuddy", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("第 %d 次拉取失败：%d %s", i+1, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), `"gpt-5"`) {
			t.Fatalf("响应缺少模型：%s", recorder.Body.String())
		}
	}
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("缓存期内应只请求一次，实际 %d", calls)
	}

	if recorder := call(t, handler, http.MethodGet, "/api/modelgw/catalog/fetch?product=workbuddy&refresh=1", nil); recorder.Code != http.StatusOK {
		t.Fatalf("强制刷新失败：%d", recorder.Code)
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("refresh 应重新请求，实际 %d", calls)
	}
}

// 面板必须是四个可用页面，而不是骨架：缺任一导航项即视为回退。
func TestPanelExposesFourViews(t *testing.T) {
	server, _ := newServer(t)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/panel/products", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	body := recorder.Body.String()

	for _, want := range []string{"产品", "模型组", "请求日志", "Token", `data-view="logs"`, `data-view="tokens"`, "embed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("面板缺少 %q：%s", want, body)
		}
	}
}

func TestPanelSpaFallbackAndEmbeddable(t *testing.T) {
	server, _ := newServer(t)
	handler := server.Handler()

	for _, path := range []string{"/panel/", "/panel/products", "/panel/groups"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s 应返回 200，实际 %d", path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "<!doctype html>") {
			t.Fatalf("%s 应返回 SPA 入口：%s", path, recorder.Body.String())
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s 的 HTML 应不缓存，实际 %q", path, got)
		}
		if got := recorder.Header().Get("X-Frame-Options"); got != "" {
			t.Fatalf("%s 不应设置 X-Frame-Options，实际 %q", path, got)
		}
	}
}

func TestCreateTokenReturnsPlainOnlyOnce(t *testing.T) {
	server, st := newServer(t)
	version := st.Snapshot().Version
	recorder := call(t, server.Handler(), http.MethodPost, "/api/modelgw/tokens", map[string]interface{}{
		"version": version, "name": "脚本", "groups": []string{"*"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("创建 Token 失败：%d %s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		Plain string `json:"plain"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil || !strings.HasPrefix(created.Plain, "mg-") {
		t.Fatalf("创建响应应含明文 key：%v %s", err, recorder.Body.String())
	}

	list := call(t, server.Handler(), http.MethodGet, "/api/modelgw/tokens", nil)
	if strings.Contains(list.Body.String(), created.Plain) {
		t.Fatal("列表接口不应返回明文 key")
	}
	if !strings.Contains(list.Body.String(), "mg-") {
		t.Fatalf("列表应返回前缀：%s", list.Body.String())
	}
}

func TestDisableTokenTakesEffect(t *testing.T) {
	server, st := newServer(t)
	version := st.Snapshot().Version
	created := call(t, server.Handler(), http.MethodPost, "/api/modelgw/tokens", map[string]interface{}{
		"version": version, "name": "t", "groups": []string{"*"},
	})
	var payload struct {
		Token struct {
			ID string `json:"id"`
		} `json:"token"`
	}
	_ = json.Unmarshal(created.Body.Bytes(), &payload)

	if recorder := call(t, server.Handler(), http.MethodPut, "/api/modelgw/tokens/"+payload.Token.ID, map[string]interface{}{
		"version": st.Snapshot().Version, "enabled": false,
	}); recorder.Code != http.StatusOK {
		t.Fatalf("禁用失败：%d %s", recorder.Code, recorder.Body.String())
	}
	if st.Snapshot().Tokens[0].Enabled {
		t.Fatal("禁用后快照里应为停用状态")
	}
}

func TestLogsListFiltersAndPaginates(t *testing.T) {
	server, _ := newServer(t)
	dir := t.TempDir()
	day := time.Now().In(config.Location()).Format("20060102")
	lines := strings.Join([]string{
		`{"time":"2026-11-05 00:01:00","group":"gpt-5","product":"workbuddy","token_name":"脚本","outcome":"ok","http_status":200}`,
		`{"time":"2026-11-05 00:02:00","group":"gpt-5","outcome":"quota","http_status":429,"attempts":[{"product":"a","model":"m","status":429,"ms":10},{"product":"b","model":"n","status":200,"ms":12}]}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "requests-"+day+".jsonl"), []byte(lines+"\n"), 0o644); err != nil {
		t.Fatalf("准备日志失败：%v", err)
	}
	handler := server.WithLogs(dir).Handler()

	ok := call(t, handler, http.MethodGet, "/api/modelgw/logs?outcome=ok", nil)
	if !strings.Contains(ok.Body.String(), `"total":1`) {
		t.Fatalf("outcome 过滤应命中 1 条：%s", ok.Body.String())
	}
	downgraded := call(t, handler, http.MethodGet, "/api/modelgw/logs?outcome=downgraded", nil)
	if !strings.Contains(downgraded.Body.String(), `"total":1`) {
		t.Fatalf("downgraded 过滤应命中 1 条：%s", downgraded.Body.String())
	}
	page := call(t, handler, http.MethodGet, "/api/modelgw/logs?page=2&size=1", nil)
	if !strings.Contains(page.Body.String(), `"page":2`) || !strings.Contains(page.Body.String(), `"total":2`) {
		t.Fatalf("分页结果不正确：%s", page.Body.String())
	}
	byProduct := call(t, handler, http.MethodGet, "/api/modelgw/logs?product=workbuddy", nil)
	if !strings.Contains(byProduct.Body.String(), `"total":1`) {
		t.Fatalf("product 过滤应命中 1 条：%s", byProduct.Body.String())
	}
	byStatus := call(t, handler, http.MethodGet, "/api/modelgw/logs?status=429", nil)
	if !strings.Contains(byStatus.Body.String(), `"total":1`) {
		t.Fatalf("status 过滤应命中 1 条：%s", byStatus.Body.String())
	}
	byKeyword := call(t, handler, http.MethodGet, "/api/modelgw/logs?q=脚本", nil)
	if !strings.Contains(byKeyword.Body.String(), `"total":1`) {
		t.Fatalf("关键字过滤应命中 1 条：%s", byKeyword.Body.String())
	}
}

func TestLogsOverviewCountsDowngrade(t *testing.T) {
	server, _ := newServer(t)
	dir := t.TempDir()
	day := time.Now().In(config.Location()).Format("20060102")
	lines := strings.Join([]string{
		`{"time":"2026-11-05 00:01:00","outcome":"ok","http_status":200}`,
		`{"time":"2026-11-05 00:02:00","outcome":"quota","http_status":429,"attempts":[{"product":"a"},{"product":"b"}]}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "requests-"+day+".jsonl"), []byte(lines+"\n"), 0o644); err != nil {
		t.Fatalf("准备日志失败：%v", err)
	}

	recorder := call(t, server.WithLogs(dir).Handler(), http.MethodGet, "/api/modelgw/logs/overview", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("概览失败：%d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"today_requests":2`) ||
		!strings.Contains(recorder.Body.String(), `"downgraded":1`) {
		t.Fatalf("概览统计不正确：%s", recorder.Body.String())
	}
}

// 面板静态页免鉴权：只有 /api/modelgw/* 需要 admin_token，SPA 自身要能被匿名加载。
func TestPanelStaticStaysOpenWithoutCredentials(t *testing.T) {
	server, _ := newServer(t)
	handler := server.Handler()

	panelReq := httptest.NewRequest(http.MethodGet, "/panel/", nil)
	panelRecorder := httptest.NewRecorder()
	handler.ServeHTTP(panelRecorder, panelReq)
	if panelRecorder.Code != http.StatusOK {
		t.Fatalf("面板静态页应免鉴权，实际 %d", panelRecorder.Code)
	}
}

// 连通性测试必须真的打下游 /v1/models，并带回耗时与模型数。
func TestProductTestConnReportsLatencyAndModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("应请求 /v1/models，实际 %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("应带产品 key，实际 %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":"gpt-4o"}]}`))
	}))
	defer upstream.Close()

	server, st := newServer(t)
	cfg := *st.Snapshot()
	cfg.Products[0].BaseURL = upstream.URL
	cfg.Products[0].APIKey = "sk-test"
	if err := st.Save(cfg, cfg.Version); err != nil {
		t.Fatalf("准备产品失败：%v", err)
	}

	recorder := call(t, server.Handler(), http.MethodPost, "/api/modelgw/products/workbuddy/test", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		OK        bool  `json:"ok"`
		Count     int   `json:"count"`
		LatencyMs int64 `json:"latency_ms"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if !payload.OK || payload.Count != 2 {
		t.Fatalf("连通结果不正确：%+v", payload)
	}
}

// 权重即优先级：上移/下移交换相邻成员的 weight；到顶继续上移应报错。
func TestMoveMemberSwapsWeight(t *testing.T) {
	server, st := newServer(t)
	cfg := *st.Snapshot()
	cfg.ModelGroups[0].Members = []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true},
		{Product: "workbuddy", Model: "gpt-4o", Weight: 10, Enabled: true},
	}
	if err := st.Save(cfg, cfg.Version); err != nil {
		t.Fatalf("准备模型组失败：%v", err)
	}

	move := func(direction, model string) *httptest.ResponseRecorder {
		return call(t, server.Handler(), http.MethodPost, "/api/modelgw/model-groups/gpt-5/members/move",
			map[string]interface{}{
				"version": st.Snapshot().Version, "product": "workbuddy",
				"model": model, "direction": direction,
			})
	}

	if recorder := move("down", "gpt-5"); recorder.Code != http.StatusOK {
		t.Fatalf("下移失败：%d %s", recorder.Code, recorder.Body.String())
	}
	if got := st.Snapshot().ModelGroups[0].Members[0].Weight; got != 10 {
		t.Fatalf("下移后权重应为 10，实际 %d", got)
	}

	if recorder := move("up", "gpt-5"); recorder.Code != http.StatusOK {
		t.Fatalf("上移失败：%d %s", recorder.Code, recorder.Body.String())
	}
	if got := st.Snapshot().ModelGroups[0].Members[0].Weight; got != 100 {
		t.Fatalf("上移后权重应为 100，实际 %d", got)
	}

	recorder := move("up", "gpt-5")
	if recorder.Code != http.StatusBadRequest || errorCode(t, recorder) != CodeInvalidParam {
		t.Fatalf("到顶再上移应返回 400/20600，实际 %d %s", recorder.Code, recorder.Body.String())
	}
}

// 产品 api_key 不出明文：列表只回掩码；用掩码保存视为「未修改」，保留原 key。
func TestProductAPIKeyIsMaskedAndPreserved(t *testing.T) {
	server, st := newServer(t)
	cfg := *st.Snapshot()
	cfg.Products[0].APIKey = "sk-secret-123456"
	if err := st.Save(cfg, cfg.Version); err != nil {
		t.Fatalf("准备产品失败：%v", err)
	}

	list := call(t, server.Handler(), http.MethodGet, "/api/modelgw/products", nil)
	if strings.Contains(list.Body.String(), "sk-secret-123456") {
		t.Fatalf("列表不应返回明文 key：%s", list.Body.String())
	}
	var payload struct {
		Data []struct {
			APIKey string `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &payload); err != nil || len(payload.Data) == 0 {
		t.Fatalf("解析产品列表失败：%v", err)
	}
	masked := payload.Data[0].APIKey
	if masked == "" || strings.Contains(masked, "secret") {
		t.Fatalf("掩码不正确：%q", masked)
	}

	product := map[string]interface{}{
		"id": "workbuddy", "enabled": true,
		"base_url": "http://127.0.0.1:7863", "api_key": masked,
	}
	if recorder := call(t, server.Handler(), http.MethodPost, "/api/modelgw/products",
		map[string]interface{}{"version": st.Snapshot().Version, "product": product}); recorder.Code != http.StatusOK {
		t.Fatalf("掩码保存失败：%d %s", recorder.Code, recorder.Body.String())
	}
	if got := st.Snapshot().Products[0].APIKey; got != "sk-secret-123456" {
		t.Fatalf("掩码保存应保留原 key，实际 %q", got)
	}

	product["api_key"] = "sk-new-999999"
	if recorder := call(t, server.Handler(), http.MethodPost, "/api/modelgw/products",
		map[string]interface{}{"version": st.Snapshot().Version, "product": product}); recorder.Code != http.StatusOK {
		t.Fatalf("新 key 保存失败：%d %s", recorder.Code, recorder.Body.String())
	}
	if got := st.Snapshot().Products[0].APIKey; got != "sk-new-999999" {
		t.Fatalf("新 key 应覆盖，实际 %q", got)
	}
}

// 成员运行状态：只列启用成员，能给出故障原因与解除时间；嗅探成功应清掉冷却。
func TestMemberStatusAndSniff(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 嗅探改为真实请求：只有 gpt-5 被上游接单，其它模型回 404。
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("嗅探应打 /v1/chat/completions，实际 %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "gpt-5") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("{\"code\":11102,\"msg\":\"model not available\"}"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"choices\":[{\"message\":{\"content\":\"ok\"}}]}"))
	}))
	defer upstream.Close()

	server, st := newServer(t)
	cfg := *st.Snapshot()
	cfg.Products[0].BaseURL = upstream.URL
	cfg.Products[0].APIKey = "sk-test"
	cfg.ModelGroups[0].Members = []config.Member{
		{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true},
		{Product: "workbuddy", Model: "gpt-4o", Weight: 10, Enabled: false},
	}
	if err := st.Save(cfg, cfg.Version); err != nil {
		t.Fatalf("准备配置失败：%v", err)
	}
	// 造一次失败：成员进入冷却并带原因。
	key := config.MemberKey("gpt-5", cfg.ModelGroups[0].Members[0])
	st.State().Failure(key, cfg.Routing.Cooldown)
	st.State().SetLastError(key, "upstream（HTTP 500）")

	status := call(t, server.Handler(), http.MethodGet, "/api/modelgw/model-groups/gpt-5/members/status", nil)
	if status.Code != http.StatusOK {
		t.Fatalf("状态查询失败：%d %s", status.Code, status.Body.String())
	}
	if strings.Contains(status.Body.String(), "gpt-4o") {
		t.Fatalf("只应列出启用成员：%s", status.Body.String())
	}
	for _, want := range []string{"\"state\":\"cooldown\"", "upstream（HTTP 500）", "unlock_at"} {
		if !strings.Contains(status.Body.String(), want) {
			t.Fatalf("状态缺少 %s：%s", want, status.Body.String())
		}
	}

	sniff := call(t, server.Handler(), http.MethodPost, "/api/modelgw/model-groups/gpt-5/members/sniff",
		map[string]interface{}{"version": st.Snapshot().Version, "product": "workbuddy", "model": "gpt-5"})
	if sniff.Code != http.StatusOK || !strings.Contains(sniff.Body.String(), "\"ok\":true") {
		t.Fatalf("嗅探应成功：%d %s", sniff.Code, sniff.Body.String())
	}
	after := st.State().StatusOf(key)
	if after.State != "ok" || after.ProbedAt == "" || after.FailStreak != 0 {
		t.Fatalf("嗅探成功应清掉冷却并记录时间：%+v", after)
	}

	missing := call(t, server.Handler(), http.MethodPost, "/api/modelgw/model-groups/gpt-5/members/sniff",
		map[string]interface{}{"version": st.Snapshot().Version, "product": "workbuddy", "model": "gpt-4o"})
	if missing.Code != http.StatusOK || !strings.Contains(missing.Body.String(), "\"ok\":false") {
		t.Fatalf("上游目录没有的模型应报未恢复：%d %s", missing.Code, missing.Body.String())
	}
}

// 模型列表页要有数据与更新时间；配置页要能读改通用参数。
func TestModelsListAndSettings(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":"gpt-4o"}]}`))
	}))
	defer upstream.Close()

	server, st := newServer(t)
	cfg := *st.Snapshot()
	cfg.Products[0].BaseURL = upstream.URL
	cfg.Products[0].APIKey = "sk-test"
	if err := st.Save(cfg, cfg.Version); err != nil {
		t.Fatalf("准备产品失败：%v", err)
	}

	refreshed := call(t, server.Handler(), http.MethodPost, "/api/modelgw/models/refresh", map[string]interface{}{"product": "workbuddy"})
	if refreshed.Code != http.StatusOK {
		t.Fatalf("刷新模型失败：%d %s", refreshed.Code, refreshed.Body.String())
	}
	list := call(t, server.Handler(), http.MethodGet, "/api/modelgw/models?q=gpt-4", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("模型列表失败：%d %s", list.Code, list.Body.String())
	}
	for _, want := range []string{"gpt-4o", "updated_at", "workbuddy"} {
		if !strings.Contains(list.Body.String(), want) {
			t.Fatalf("模型列表缺少 %s：%s", want, list.Body.String())
		}
	}
	if strings.Contains(list.Body.String(), "gpt-5") {
		t.Fatalf("关键字过滤应只留 gpt-4o：%s", list.Body.String())
	}

	settings := call(t, server.Handler(), http.MethodGet, "/api/modelgw/settings", nil)
	if settings.Code != http.StatusOK || !strings.Contains(settings.Body.String(), "probe") {
		t.Fatalf("读取配置失败：%d %s", settings.Code, settings.Body.String())
	}
	payload := map[string]interface{}{
		"version": st.Snapshot().Version,
		"settings": map[string]interface{}{
			"routing": map[string]interface{}{
				"max_attempts": 3, "prefer_last_ok": true,
				"cooldown": map[string]interface{}{"base": "3s", "max": "90s", "fail_threshold": 3, "open_duration": "2m"},
				"sticky":   map[string]interface{}{"enabled": false, "ttl": "5m"},
			},
			"probe":   map[string]interface{}{"enabled": true, "interval": "15s", "min_gap": "10s"},
			"logging": map[string]interface{}{"dir": "./logs", "retention_days": 7, "max_file_mb": 64},
		},
	}
	saved := call(t, server.Handler(), http.MethodPost, "/api/modelgw/settings", payload)
	if saved.Code != http.StatusOK {
		t.Fatalf("保存配置失败：%d %s", saved.Code, saved.Body.String())
	}
	after := st.Snapshot()
	if after.Probe.Interval != "15s" || after.Routing.MaxAttempts != 3 || after.Logging.RetentionDays != 7 {
		t.Fatalf("配置未落盘：%+v", after)
	}

	bad := call(t, server.Handler(), http.MethodPost, "/api/modelgw/settings", map[string]interface{}{
		"version":  st.Snapshot().Version,
		"settings": map[string]interface{}{"probe": map[string]interface{}{"enabled": true, "interval": "1s"}},
	})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("过小的探活间隔应被拒：%d %s", bad.Code, bad.Body.String())
	}
}

// settingsPayload 按当前快照拼一份完整配置页 payload：
// routing / probe / logging 是全量替换语义，前端每次提交的都是全量。
func settingsPayload(st *store.Store, mutate func(map[string]interface{})) map[string]interface{} {
	cfg := st.Snapshot()
	settings := map[string]interface{}{
		"startup": map[string]interface{}{"listen": cfg.Listen, "data_dir": cfg.DataDir},
		"routing": map[string]interface{}{
			"max_attempts": cfg.Routing.MaxAttempts, "prefer_last_ok": cfg.Routing.PreferLastOK,
			"cooldown": map[string]interface{}{
				"base": cfg.Routing.Cooldown.Base, "max": cfg.Routing.Cooldown.Max,
				"fail_threshold": cfg.Routing.Cooldown.FailThreshold,
				"open_duration":  cfg.Routing.Cooldown.OpenDuration,
			},
			"sticky": map[string]interface{}{"enabled": cfg.Routing.Sticky.Enabled, "ttl": cfg.Routing.Sticky.TTL},
		},
		"probe": map[string]interface{}{
			"enabled": cfg.Probe.Enabled, "interval": cfg.Probe.Interval, "min_gap": cfg.Probe.MinGap,
		},
		"logging": map[string]interface{}{
			"dir": cfg.Logging.Dir, "retention_days": cfg.Logging.RetentionDays, "max_file_mb": cfg.Logging.MaxFileMB,
		},
	}
	if mutate != nil {
		mutate(settings)
	}
	return map[string]interface{}{"version": cfg.Version, "settings": settings}
}

// 监听地址 / 数据目录 / 管理口令都在配置页可改，但只在下次启动生效：
// 写盘后当前进程仍用旧值，口令只存 SHA-256、读取接口不回显。
func TestSettingsSaveStartupParamsForNextBoot(t *testing.T) {
	server, st := newServer(t)
	before := st.Snapshot().Listen

	payload := settingsPayload(st, func(settings map[string]interface{}) {
		settings["startup"] = map[string]interface{}{
			"listen": ":9090", "data_dir": "./next-data", "admin_token": "new-secret-token",
		}
	})
	recorder := call(t, server.Handler(), http.MethodPost, "/api/modelgw/settings", payload)
	if recorder.Code != http.StatusOK {
		t.Fatalf("保存启动参数失败：%d %s", recorder.Code, recorder.Body.String())
	}

	after := st.Snapshot()
	if after.Listen != ":9090" || after.DataDir != "./next-data" {
		t.Fatalf("启动参数未落盘：listen=%s data_dir=%s", after.Listen, after.DataDir)
	}
	if after.AdminToken != "" {
		t.Fatalf("明文口令不允许留在配置里：%q", after.AdminToken)
	}
	if after.AdminTokenSHA256 != config.HashAdminToken("new-secret-token") {
		t.Fatalf("口令摘要不正确：%s", after.AdminTokenSHA256)
	}
	if config.HashAdminToken("new-secret-token") == "new-secret-token" {
		t.Fatal("摘要函数不应返回明文")
	}

	// 当前进程的鉴权仍用启动时的口令：新口令要重启后才生效。
	if got := call(t, server.Handler(), http.MethodGet, "/api/modelgw/products", nil); got.Code != http.StatusOK {
		t.Fatalf("旧口令在本次进程内应继续有效：%d", got.Code)
	}

	read := call(t, server.Handler(), http.MethodGet, "/api/modelgw/settings", nil)
	if read.Code != http.StatusOK {
		t.Fatalf("读取配置失败：%d", read.Code)
	}
	if strings.Contains(read.Body.String(), "new-secret-token") ||
		strings.Contains(read.Body.String(), after.AdminTokenSHA256) {
		t.Fatalf("读取接口不应回显口令或摘要：%s", read.Body.String())
	}
	if !strings.Contains(read.Body.String(), `"admin_token_set":true`) {
		t.Fatalf("应能告知「已设置口令」：%s", read.Body.String())
	}
	// 配置页展示「已保存待生效」的地址，同时给出当前进程实际监听值供对照。
	if !strings.Contains(read.Body.String(), `"listen":":9090"`) {
		t.Fatalf("读取接口应回显已保存的监听地址：%s", read.Body.String())
	}
	if !strings.Contains(read.Body.String(), `"active_listen":"`+before+`"`) {
		t.Fatalf("读取接口应回显当前实际监听地址 %s：%s", before, read.Body.String())
	}
	if !strings.Contains(read.Body.String(), `"pending_restart":true`) {
		t.Fatalf("改了监听地址应标记待重启：%s", read.Body.String())
	}
}

// 只改运行参数（不带 startup）时不能把监听地址清空。
func TestSettingsSaveKeepsStartupWhenOmitted(t *testing.T) {
	server, st := newServer(t)
	before := st.Snapshot().Listen

	payload := settingsPayload(st, func(settings map[string]interface{}) {
		delete(settings, "startup")
		settings["probe"].(map[string]interface{})["enabled"] = false
	})
	if recorder := call(t, server.Handler(), http.MethodPost, "/api/modelgw/settings", payload); recorder.Code != http.StatusOK {
		t.Fatalf("保存失败：%d %s", recorder.Code, recorder.Body.String())
	}
	if got := st.Snapshot().Listen; got != before {
		t.Fatalf("未提交 startup 时不应改动监听地址：%s -> %s", before, got)
	}
	if st.Snapshot().Probe.Enabled {
		t.Fatal("探活开关未生效")
	}
}

func TestRollbackRestoresPreviousConfig(t *testing.T) {
	server, st := newServer(t)
	version := st.Snapshot().Version
	if recorder := call(t, server.Handler(), http.MethodPost, "/api/modelgw/products", map[string]interface{}{
		"version": version,
		"product": map[string]interface{}{"id": "trae", "enabled": true, "base_url": "http://127.0.0.1:7864"},
	}); recorder.Code != http.StatusOK {
		t.Fatalf("准备保存失败：%d", recorder.Code)
	}
	after := st.Snapshot().Version

	if recorder := call(t, server.Handler(), http.MethodPost, "/api/modelgw/config/rollback", nil); recorder.Code != http.StatusOK {
		t.Fatalf("回滚失败：%d %s", recorder.Code, recorder.Body.String())
	}
	if len(st.Snapshot().Products) != 1 {
		t.Fatalf("回滚后产品应回到 1 个：%+v", st.Snapshot().Products)
	}
	if st.Snapshot().Version <= after {
		t.Fatalf("回滚后版本号应继续递增：%d → %d", after, st.Snapshot().Version)
	}
}
