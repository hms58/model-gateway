package panel

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/catalog"
	"model-gateway/internal/pkg/probe"
	"model-gateway/internal/pkg/reqlog"
	"model-gateway/internal/pkg/store"
	"model-gateway/internal/router"
)

// 管理面错误码：响应体里的 error.code，客户端据此区分失败原因。
const (
	CodeInvalidParam    = 20600
	CodeProductNotFound = 20601
	CodeProductInUse    = 20602
	CodeGroupNotFound   = 20603
	CodeGroupDuplicate  = 20604
	CodeMemberDuplicate = 20605
	CodeInvalidWindow   = 20606
	CodeVersionConflict = 20607
	CodeFetchModels     = 20610
)

type Server struct {
	store *store.Store
	// adminTokenSHA256 是启动时读到的管理面口令摘要；配置页改动只在下次启动生效。
	adminTokenSHA256 string
	// activeListen 是当前进程实际监听的地址，用于和「已保存待生效」的值做对照。
	activeListen string
	catalog      *catalog.Client
	logsDir      string
	logger       *reqlog.Logger
}

func New(st *store.Store, adminTokenSHA256 string) *Server {
	cfg := st.Snapshot()
	return &Server{
		store: st, adminTokenSHA256: adminTokenSHA256,
		activeListen: cfg.Listen, catalog: catalog.New(),
	}
}

// WithModelsPath 把模型缓存落到 <dir>/models.json：重启后优先读文件，过期或缺失才回源。
func (s *Server) WithModelsPath(dir string) *Server {
	if strings.TrimSpace(dir) == "" {
		return s
	}
	s.catalog = catalog.NewWithFile(filepath.Join(dir, "models.json"))
	return s
}

// WithLogger 注入请求日志器（概览里的 dropped 计数与实时 ring）。
func (s *Server) WithLogger(logger *reqlog.Logger) *Server {
	s.logger = logger
	return s
}

// Handler 返回管理面路由：`/api/modelgw/*` 需 admin_token，`/panel/*` 只提供静态页。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/modelgw/products", s.auth(http.HandlerFunc(s.products)))
	mux.Handle("/api/modelgw/products/", s.auth(http.HandlerFunc(s.productItem)))
	mux.Handle("/api/modelgw/model-groups", s.auth(http.HandlerFunc(s.groups)))
	mux.Handle("/api/modelgw/model-groups/", s.auth(http.HandlerFunc(s.groupItem)))
	mux.Handle("/api/modelgw/catalog/fetch", s.auth(http.HandlerFunc(s.fetchModels)))
	mux.Handle("/api/modelgw/models", s.auth(http.HandlerFunc(s.models)))
	mux.Handle("/api/modelgw/models/refresh", s.auth(http.HandlerFunc(s.refreshModels)))
	mux.Handle("/api/modelgw/settings", s.auth(http.HandlerFunc(s.settings)))
	mux.Handle("/api/modelgw/tokens", s.auth(http.HandlerFunc(s.tokens)))
	mux.Handle("/api/modelgw/tokens/", s.auth(http.HandlerFunc(s.tokenItem)))
	mux.Handle("/api/modelgw/logs", s.auth(http.HandlerFunc(s.logsList)))
	mux.Handle("/api/modelgw/logs/overview", s.auth(http.HandlerFunc(s.logsOverview)))
	mux.Handle("/api/modelgw/config/rollback", s.auth(http.HandlerFunc(s.rollback)))
	mux.Handle("/api/modelgw/config/version", s.auth(http.HandlerFunc(s.version)))
	mux.Handle("/panel/", s.staticHandler())
	return mux
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		if s.unset() {
			writeError(w, http.StatusUnauthorized, CodeInvalidParam,
				"尚未设置管理口令：初始化只允许本机访问，请在配置页设置口令后重启")
			return
		}
		writeError(w, http.StatusUnauthorized, CodeInvalidParam, "管理面凭据无效")
	})
}

func (s *Server) products(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.store.Snapshot()
		views := make([]interface{}, 0, len(cfg.Products))
		for _, product := range cfg.Products {
			// 明文 key 不出本机：列表只回掩码，保存时按掩码识别“未修改”。
			safe := product
			safe.APIKey = maskAPIKey(product.APIKey)
			views = append(views, struct {
				config.Product
				Health router.ProductState `json:"health"`
			}{Product: safe, Health: s.store.State().ProductSnapshot(product.ID)})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"version": cfg.Version, "data": views})
	case http.MethodPost:
		var req struct {
			Version int             `json:"version"`
			Product json.RawMessage `json:"product"`
		}
		var product config.Product
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
			return
		}
		if err := json.Unmarshal(req.Product, &product); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, "product 字段不可解析")
			return
		}
		if err := s.saveProduct(product, req.Version); err != nil {
			writeSaveError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"version": s.store.Snapshot().Version})
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
	}
}

func (s *Server) saveProduct(product config.Product, version int) error {
	cfg := *s.store.Snapshot()
	if strings.TrimSpace(product.ID) == "" {
		return &apiError{http.StatusBadRequest, CodeInvalidParam, "产品 id 不能为空"}
	}
	replaced := false
	for i := range cfg.Products {
		if cfg.Products[i].ID == product.ID {
			// 面板回填的是掩码：留空或等于掩码都表示「不改 key」，保留原值。
			incoming := strings.TrimSpace(product.APIKey)
			if incoming == "" || incoming == maskAPIKey(cfg.Products[i].APIKey) {
				product.APIKey = cfg.Products[i].APIKey
			}
			cfg.Products[i] = product
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Products = append(cfg.Products, product)
	}
	if err := s.store.Save(cfg, version); err != nil {
		return err
	}
	// 产品地址 / key 变更后旧模型列表不再可信，清缓存避免最长 10 分钟继续用旧数据。
	s.catalog.Invalidate(product.ID)
	return nil
}

func (s *Server) productItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/modelgw/products/")
	if id == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "缺少产品 id")
		return
	}
	if strings.HasSuffix(id, "/test") {
		s.testProduct(w, r, strings.TrimSuffix(id, "/test"))
		return
	}

	switch r.Method {
	case http.MethodDelete:
		cfg := *s.store.Snapshot()
		for _, group := range cfg.ModelGroups {
			for _, member := range group.Members {
				if member.Product == id {
					writeError(w, http.StatusBadRequest, CodeProductInUse, "产品仍被模型组引用的组名："+group.Name)
					return
				}
			}
		}
		kept := cfg.Products[:0]
		for _, product := range cfg.Products {
			if product.ID != id {
				kept = append(kept, product)
			}
		}
		if len(kept) == len(cfg.Products) {
			writeError(w, http.StatusNotFound, CodeProductNotFound, "产品不存在："+id)
			return
		}
		cfg.Products = kept
		if err := s.store.Save(cfg, parseVersion(r)); err != nil {
			writeSaveError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut:
		var req struct {
			Version int  `json:"version"`
			Enabled bool `json:"enabled"`
		}
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
			return
		}
		cfg := *s.store.Snapshot()
		found := false
		for i := range cfg.Products {
			if cfg.Products[i].ID == id {
				cfg.Products[i].Enabled = req.Enabled
				found = true
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, CodeProductNotFound, "产品不存在："+id)
			return
		}
		if err := s.store.Save(cfg, req.Version); err != nil {
			writeSaveError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"version": s.store.Snapshot().Version})
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
	}
}

func (s *Server) groups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"version": s.store.Snapshot().Version, "data": s.store.Snapshot().ModelGroups})
	case http.MethodPost:
		var req struct {
			Version int             `json:"version"`
			Group   json.RawMessage `json:"group"`
		}
		var group config.Group
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
			return
		}
		if err := json.Unmarshal(req.Group, &group); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, "group 字段不可解析")
			return
		}
		cfg := *s.store.Snapshot()
		replaced := false
		for i := range cfg.ModelGroups {
			if cfg.ModelGroups[i].Name == group.Name {
				cfg.ModelGroups[i] = group
				replaced = true
			}
		}
		if !replaced {
			cfg.ModelGroups = append(cfg.ModelGroups, group)
		}
		if err := s.store.Save(cfg, req.Version); err != nil {
			writeSaveError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"version": s.store.Snapshot().Version})
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
	}
}

func (s *Server) groupItem(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/modelgw/model-groups/")
	if name == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "缺少组名")
		return
	}
	if strings.HasSuffix(name, "/members/move") {
		s.moveMember(w, r, strings.TrimSuffix(name, "/members/move"))
		return
	}
	if strings.HasSuffix(name, "/members/status") {
		s.memberStatus(w, r, strings.TrimSuffix(name, "/members/status"))
		return
	}
	if strings.HasSuffix(name, "/members/sniff") {
		s.sniffMember(w, r, strings.TrimSuffix(name, "/members/sniff"))
		return
	}

	cfg := *s.store.Snapshot()
	index := -1
	for i := range cfg.ModelGroups {
		if cfg.ModelGroups[i].Name == name {
			index = i
		}
	}
	if index < 0 {
		writeError(w, http.StatusNotFound, CodeGroupNotFound, "模型组不存在："+name)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		cfg.ModelGroups = append(cfg.ModelGroups[:index], cfg.ModelGroups[index+1:]...)
		if err := s.store.Save(cfg, parseVersion(r)); err != nil {
			writeSaveError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut:
		var req struct {
			Version int  `json:"version"`
			Enabled bool `json:"enabled"`
		}
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
			return
		}
		cfg.ModelGroups[index].Enabled = req.Enabled
		if err := s.store.Save(cfg, req.Version); err != nil {
			writeSaveError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"version": s.store.Snapshot().Version})
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
	}
}

// maskAPIKey 只保留前 4 位与后 4 位，其余打码；过短的 key 全部打码。
func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", 8) + key[len(key)-4:]
}

// memberStatus 返回组内「启用成员」的可用性：状态、原因、解除时间、最近成功 / 嗅探。
func (s *Server) memberStatus(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	cfg := s.store.Snapshot()
	var group *config.Group
	for i := range cfg.ModelGroups {
		if cfg.ModelGroups[i].Name == name {
			group = &cfg.ModelGroups[i]
			break
		}
	}
	if group == nil {
		writeError(w, http.StatusNotFound, CodeGroupNotFound, "模型组不存在："+name)
		return
	}

	products := make(map[string]config.Product, len(cfg.Products))
	for _, product := range cfg.Products {
		products[product.ID] = product
	}
	now := time.Now().In(config.Location())
	rows := make([]map[string]interface{}, 0, len(group.Members))
	for _, member := range group.Members {
		if !member.Enabled {
			continue
		}
		status := s.store.State().StatusOf(config.MemberKey(name, member))
		state, reason := status.State, status.Reason
		product, ok := products[member.Product]
		switch {
		case !ok:
			state, reason = "product_missing", "产品已删除"
		case !product.Enabled:
			state, reason = "product_disabled", "产品已停用"
		case !router.InWindow(member, now):
			state, reason = "window_out", "不在生效时段"
		}
		rows = append(rows, map[string]interface{}{
			"product": member.Product, "model": member.Model, "weight": member.Weight,
			"state": state, "reason": reason, "unlock_at": status.UnlockAt,
			"last_ok_at": status.LastOKAt, "probed_at": status.ProbedAt,
			"fail_streak": status.FailStreak,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"group": name, "data": rows})
}

// sniffMember 主动探活：发一次最小真实请求（max_tokens=1）。
// /v1/models 只能说明产品在线，模型本身能不能被接单必须真打一次。
// 探到即视为恢复，清掉该成员的冷却与熔断；探不到把原因记到成员状态里。
func (s *Server) sniffMember(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	var req struct {
		Product string `json:"product"`
		Model   string `json:"model"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}

	cfg := s.store.Snapshot()
	var member *config.Member
	for i := range cfg.ModelGroups {
		if cfg.ModelGroups[i].Name != name {
			continue
		}
		for j := range cfg.ModelGroups[i].Members {
			current := &cfg.ModelGroups[i].Members[j]
			if current.Product == req.Product && current.Model == req.Model {
				member = current
			}
		}
	}
	if member == nil {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "组内不存在该成员")
		return
	}

	key := config.MemberKey(name, *member)
	state := s.store.State()
	var product config.Product
	found := false
	for _, item := range cfg.Products {
		if item.ID == member.Product {
			product, found = item, true
		}
	}
	if !found {
		state.Probe(key, false, "产品已删除")
		s.store.MarkDirty()
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "reason": "产品已删除"})
		return
	}
	if !product.Enabled {
		state.Probe(key, false, "产品已停用")
		s.store.MarkDirty()
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "reason": "产品已停用"})
		return
	}

	result := probe.Model(r.Context(), product, member.Model, 30*time.Second)
	if !result.OK {
		state.Probe(key, false, result.Reason)
		s.store.MarkDirty()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok": false, "latency_ms": result.Latency.Milliseconds(), "reason": result.Reason,
		})
		return
	}
	state.Probe(key, true, "")
	s.store.MarkDirty()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "latency_ms": result.Latency.Milliseconds(),
	})
}

func clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// moveMember 调整成员的优先级层：up 提到上一个非空层，down 降到下一个非空层。
// 层内顺序由 weight 表达（加权随机），因此不做层内重排。
func (s *Server) moveMember(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	var req struct {
		Version   int    `json:"version"`
		Product   string `json:"product"`
		Model     string `json:"model"`
		Direction string `json:"direction"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}
	if req.Direction != "up" && req.Direction != "down" {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "direction 只支持 up / down")
		return
	}

	cfg := *s.store.Snapshot()
	index := -1
	for i := range cfg.ModelGroups {
		if cfg.ModelGroups[i].Name == name {
			index = i
		}
	}
	if index < 0 {
		writeError(w, http.StatusNotFound, CodeGroupNotFound, "模型组不存在："+name)
		return
	}

	group := &cfg.ModelGroups[index]
	memberAt := -1
	for i := range group.Members {
		if group.Members[i].Product == req.Product && group.Members[i].Model == req.Model {
			memberAt = i
			break
		}
	}
	if memberAt < 0 {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "组内不存在该成员："+req.Product+"/"+req.Model)
		return
	}

	// 权重即优先级：相邻成员按 weight 降序排列，交换两者 weight 即为一次上移/下移。
	order := make([]int, len(group.Members))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return group.Members[order[a]].Weight > group.Members[order[b]].Weight
	})
	position := -1
	for i, index := range order {
		if index == memberAt {
			position = i
			break
		}
	}

	neighbor := position - 1
	if req.Direction == "down" {
		neighbor = position + 1
	}
	if neighbor < 0 || neighbor >= len(order) {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "已经在最前/最后，无法继续移动")
		return
	}
	other := order[neighbor]

	mine, theirs := group.Members[memberAt].Weight, group.Members[other].Weight
	group.Members[memberAt].Weight = theirs
	group.Members[other].Weight = mine
	if mine == theirs {
		// 同权重时交换数组位置：同权重按列表顺序选路，位置即顺序。
		group.Members[memberAt], group.Members[other] = group.Members[other], group.Members[memberAt]
	}
	if err := s.store.Save(cfg, req.Version); err != nil {
		writeSaveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"version": s.store.Snapshot().Version, "weight": theirs,
	})
}

// testProduct 是显式连通性测试：强制刷新模型列表并返回耗时与模型数。
// 探测失败仍回 200（ok=false），避免把「产品不可用」当成网关自身故障。
func (s *Server) testProduct(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	var target config.Product
	found := false
	for _, product := range s.store.Snapshot().Products {
		if product.ID == id {
			target, found = product, true
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, CodeProductNotFound, "产品不存在："+id)
		return
	}

	started := time.Now()
	models, err := s.catalog.Fetch(r.Context(), target, true)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok": false, "latency_ms": latency, "error": "拉取 /v1/models 失败",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "latency_ms": latency, "count": len(models), "models": models,
	})
}

// fetchModels 由服务端代带产品 key 拉取模型列表，浏览器不跨域；refresh=1 绕过缓存。
func (s *Server) fetchModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	id := r.URL.Query().Get("product")
	if id == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "缺少 product")
		return
	}
	var target config.Product
	found := false
	for _, product := range s.store.Snapshot().Products {
		if product.ID == id {
			target = product
			found = true
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, CodeProductNotFound, "产品不存在："+id)
		return
	}

	models, err := s.catalog.Fetch(r.Context(), target, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeError(w, http.StatusBadGateway, CodeFetchModels, "拉取模型列表失败："+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": models})
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidParam, "方法不支持")
		return
	}
	if err := s.store.Rollback(); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "回滚失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"version": s.store.Snapshot().Version})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"version": s.store.Snapshot().Version})
}

type apiError struct {
	Status  int
	Code    int
	Message string
}

func (e *apiError) Error() string { return e.Message }

func writeSaveError(w http.ResponseWriter, err error) {
	var api *apiError
	if errors.As(err, &api) {
		writeError(w, api.Status, api.Code, api.Message)
		return
	}
	switch {
	case errors.Is(err, config.ErrVersionConflict):
		writeError(w, http.StatusConflict, CodeVersionConflict, "配置版本冲突，请刷新后重试")
	case errors.Is(err, config.ErrInvalidWindow):
		writeError(w, http.StatusBadRequest, CodeInvalidWindow, err.Error())
	case errors.Is(err, config.ErrInvalidGroup):
		// 组校验类错误按语义细分：只有「组名重复」才是 20604，其余归参数非法。
		message := err.Error()
		switch {
		case strings.Contains(message, "组名重复"):
			writeError(w, http.StatusBadRequest, CodeGroupDuplicate, message)
		case strings.Contains(message, "成员重复"):
			writeError(w, http.StatusBadRequest, CodeMemberDuplicate, message)
		default:
			writeError(w, http.StatusBadRequest, CodeInvalidParam, message)
		}
	case errors.Is(err, config.ErrUnsupportedProtocol):
		writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
	}
}

func parseVersion(r *http.Request) int {
	raw := r.URL.Query().Get("version")
	if raw == "" {
		raw = r.Header.Get("X-Config-Version")
	}
	value := 0
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return 0
		}
		value = value*10 + int(ch-'0')
	}
	return value
}

func decode(r *http.Request, target interface{}) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{"code": code, "message": message},
	})
}
