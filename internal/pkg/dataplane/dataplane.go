package dataplane

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/proxy"
	"model-gateway/internal/pkg/reqlog"
	"model-gateway/internal/pkg/store"
	"model-gateway/internal/pkg/token"
	"model-gateway/internal/router"
)

// MaxBodyBytes 限制请求体，避免超大 messages 打爆内存。
const MaxBodyBytes = 32 << 20

type Handler struct {
	store     *store.Store
	forwarder *proxy.Forwarder
	mu        sync.Mutex
	rnd       *rand.Rand
	logger    *reqlog.Logger
}

// WithLogger 注入请求日志器；未注入时不记日志（不影响转发）。
func (h *Handler) WithLogger(logger *reqlog.Logger) *Handler {
	h.logger = logger
	return h
}

func New(st *store.Store) *Handler {
	return &Handler{
		store:     st,
		forwarder: proxy.NewForwarder(),
		rnd:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Attempt 是返回给客户端的降级明细。
type Attempt struct {
	Product string `json:"product"`
	Model   string `json:"model"`
	Status  int    `json:"status"`
	Code    string `json:"code,omitempty"`
	MS      int64  `json:"ms"`
}

func (h *Handler) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ListModels 只返回组名，不暴露产品与真实模型。
func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	cfg := h.store.Snapshot()
	record, ok := authenticate(r, cfg)
	if !ok {
		writeError(w, http.StatusUnauthorized, "虚拟 Token 无效")
		return
	}

	items := make([]map[string]string, 0, len(cfg.ModelGroups))
	for _, group := range cfg.ModelGroups {
		if !group.Enabled || !token.Allowed(record, group.Name) {
			continue
		}
		items = append(items, map[string]string{"id": group.Name, "object": "model"})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"object": "list", "data": items})
}

func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	cfg := h.store.Snapshot()
	record, ok := authenticate(r, cfg)
	if !ok {
		writeError(w, http.StatusUnauthorized, "虚拟 Token 无效")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "请求体超过 32MB 上限")
		return
	}
	groupName, err := modelOf(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "请求体缺少可解析的 model 字段")
		return
	}
	// 先判组是否存在（404），再判权限（403）：否则未知模型会被误报成越权。
	if !groupExists(cfg, groupName) {
		writeError(w, http.StatusNotFound, "模型组不存在")
		return
	}
	if !token.Allowed(record, groupName) {
		writeError(w, http.StatusForbidden, "该 Token 无权访问此模型组")
		return
	}

	maxAttempts := cfg.Routing.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	sessionKey := strings.TrimSpace(r.Header.Get("X-Session-Id"))
	if streamRequest(body) {
		h.streamChat(w, r, cfg, body, groupName, sessionKey, record)
		return
	}
	tried := make(map[string]struct{}, maxAttempts)
	attempts := make([]Attempt, 0, maxAttempts)

	started := time.Now()
	var lastStatus int
	var lastOutcome proxy.Outcome
	var lastTTFB int64
	var lastUsage proxy.Usage
	var lastProduct string
	defer func() {
		h.noteUsage(record.ID, lastProduct, lastOutcome)
		entry := reqlog.Entry{
			RequestID: requestID(), ClientIP: clientIP(r), UA: r.UserAgent(),
			TokenID: record.ID, TokenName: record.Name, Group: groupName, Stream: false,
			DurationMs: time.Since(started).Milliseconds(),
			HTTPStatus: lastStatus, Outcome: string(lastOutcome),
			TTFBMs: lastTTFB, Attempts: toReqlogAttempts(attempts),
			PromptTokens: lastUsage.PromptTokens, CompletionTokens: lastUsage.CompletionTokens,
			TotalTokens: lastUsage.TotalTokens,
		}
		if len(attempts) > 0 {
			last := attempts[len(attempts)-1]
			entry.Product, entry.Model = last.Product, last.Model
		}
		h.writeLog(entry)
	}()

	for i := 0; i < maxAttempts; i++ {
		member, key, pickErr := h.pick(cfg, groupName, sessionKey, tried)
		if pickErr != nil {
			if errors.Is(pickErr, router.ErrGroupNotFound) {
				writeError(w, http.StatusNotFound, "模型组不存在")
				return
			}
			if errors.Is(pickErr, router.ErrGroupDisabled) {
				writeError(w, http.StatusForbidden, "模型组已禁用")
				return
			}
			break
		}
		tried[key] = struct{}{}

		product := productOf(cfg, member.Product)
		lastProduct = member.Product
		if !supportedProtocol(product) {
			lastStatus, lastOutcome = http.StatusBadRequest, proxy.Outcome("unsupported_protocol")
			attempts = append(attempts, Attempt{Product: member.Product, Model: member.Model, Status: http.StatusBadRequest, Code: string(lastOutcome)})
			writeProtocolError(w)
			return
		}
		started := time.Now()
		response, callErr := h.forwarder.DoResponse(r.Context(), product, body, member.Model)
		ttfb := time.Since(started)
		elapsed := ttfb.Milliseconds()
		if callErr != nil {
			h.fail(key, cfg.Routing.Cooldown, shortError(callErr))
			attempts = append(attempts, Attempt{Product: member.Product, Model: member.Model, Status: 0, Code: string(proxy.OutcomeUpstream), MS: elapsed})
			lastStatus = http.StatusBadGateway
			lastOutcome = proxy.OutcomeUpstream
			continue
		}

		outcome := proxy.Classify(response.StatusCode)
		attempts = append(attempts, Attempt{
			Product: member.Product, Model: member.Model,
			Status: response.StatusCode, Code: string(outcome), MS: elapsed,
		})
		lastStatus = response.StatusCode
		lastOutcome = outcome
		lastTTFB = ttfb.Milliseconds()

		if outcome == proxy.OutcomeOK {
			h.succeed(key)
			h.bindSticky(cfg, sessionKey, key)
			copyHeader(w.Header(), response.Header)
			w.WriteHeader(response.StatusCode)
			flusher, _ := w.(http.Flusher)
			usage, copyErr := proxy.StreamCopy(w, response.Body, flusher)
			response.Body.Close()
			lastUsage = usage
			if copyErr != nil {
				// 已经写出字节：只记录，不重试。
				lastOutcome = proxy.Outcome("mid_stream")
				h.fail(key, cfg.Routing.Cooldown, "mid_stream")
			}
			return
		}

		if outcome == proxy.OutcomeClientError {
			// 400 / 422 属请求本身有问题，换成员只会把所有下游打一遍。
			payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			copyHeader(w.Header(), response.Header)
			w.WriteHeader(response.StatusCode)
			_, _ = w.Write(payload)
			return
		}

		_, _ = io.CopyN(io.Discard, response.Body, 512)
		response.Body.Close()

		// 401 / 403 是产品凭据问题：换产品仍可能成功，但不该冷却成员。
		if outcome != proxy.OutcomeProductAuth {
			h.fail(key, cfg.Routing.Cooldown, statusReason(outcome, response.StatusCode))
		}
	}

	if lastStatus == 0 {
		lastOutcome = proxy.Outcome("no_member")
		writeError(w, http.StatusServiceUnavailable, "无可用成员")
		return
	}
	// 不复制上游头：上游的 Content-Length / 压缩标记与这里的自有 JSON 不一致，会导致客户端截断。
	writeJSON(w, lastStatus, map[string]interface{}{
		"error": map[string]interface{}{
			"message":  "all members failed",
			"type":     "upstream_error",
			"attempts": attempts,
		},
	})
}

func (h *Handler) pick(cfg *config.Config, groupName, sessionKey string, tried map[string]struct{}) (config.Member, string, error) {
	// rand.Rand 非并发安全：选路期间持锁；已完成失败计数的成员会被冷却过滤，不会重复命中。
	h.mu.Lock()
	member, key, err := router.Pick(cfg, h.store.State(), groupName, sessionKey, h.rnd)
	h.mu.Unlock()
	if err != nil {
		return config.Member{}, "", err
	}
	if _, seen := tried[key]; seen {
		return config.Member{}, "", router.ErrNoMember
	}
	return member, key, nil
}

func (h *Handler) writeLog(entry reqlog.Entry) {
	if h.logger == nil {
		return
	}
	h.logger.Write(entry)
}

// noteUsage 维护 Token 计数与产品健康摘要：请求热路径上只做内存写 + 脏标记。
func (h *Handler) noteUsage(tokenID, product string, outcome proxy.Outcome) {
	if h.store == nil {
		return
	}
	state := h.store.State()
	state.NoteTokenUse(tokenID)
	if product != "" {
		if outcome == proxy.OutcomeOK {
			state.NoteProductOK(product)
		} else {
			state.NoteProductError(product, string(outcome))
		}
	}
	h.store.MarkDirty()
}

func toReqlogAttempts(items []Attempt) []reqlog.Attempt {
	out := make([]reqlog.Attempt, 0, len(items))
	for _, item := range items {
		out = append(out, reqlog.Attempt{
			Product: item.Product, Model: item.Model,
			Status: item.Status, Code: item.Code, MS: item.MS,
		})
	}
	return out
}

func (h *Handler) fail(key string, cooldown config.Cooldown, reason string) {
	h.store.State().Failure(key, cooldown)
	h.store.State().SetLastError(key, reason)
	h.store.MarkDirty()
}

// shortError 把网络错误裁到可读长度，面板直接展示。
func shortError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > 160 {
		text = text[:160]
	}
	return text
}

// statusReason 给「上游返回非 2xx」一个可展示的原因。
func statusReason(outcome proxy.Outcome, status int) string {
	return string(outcome) + "（HTTP " + strconv.Itoa(status) + "）"
}

func (h *Handler) succeed(key string) {
	h.store.State().Success(key)
	h.store.MarkDirty()
}

func (h *Handler) bindSticky(cfg *config.Config, sessionKey, key string) {
	if !cfg.Routing.Sticky.Enabled {
		return
	}
	ttl, err := time.ParseDuration(cfg.Routing.Sticky.TTL)
	if err != nil || ttl <= 0 {
		return
	}
	h.store.State().BindSticky(sessionKey, key, ttl)
}

func authenticate(r *http.Request, cfg *config.Config) (config.Token, bool) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	record, err := token.Find(cfg.Tokens, raw)
	if err != nil {
		return config.Token{}, false
	}
	return record, true
}

func modelOf(body []byte) (string, error) {
	var head struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return "", err
	}
	if strings.TrimSpace(head.Model) == "" {
		return "", errors.New("缺少 model")
	}
	return head.Model, nil
}

func groupExists(cfg *config.Config, name string) bool {
	for _, group := range cfg.ModelGroups {
		if group.Name == name {
			return true
		}
	}
	return false
}

// supportedProtocol 一期只实现 openai-chat；空值按默认协议处理。
func supportedProtocol(product config.Product) bool {
	return product.Protocol == "" || product.Protocol == config.ProtocolOpenAIChat
}

// writeProtocolError 与根 proto 的 20608 对齐，供面板/客户端区分「配置问题」与「下游故障」。
func writeProtocolError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":{"code":20608,"message":"下游协议不支持","type":"gateway_error"}}`))
}

// requestID 生成短随机 id，便于把一次请求的日志与下游排障对上。
func requestID() string {
	var buf [8]byte
	if _, err := crand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf[:])
}

// clientIP 优先取反代写入的 X-Forwarded-For 首段，其次 RemoteAddr。
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if idx := strings.IndexByte(forwarded, ','); idx > 0 {
			return strings.TrimSpace(forwarded[:idx])
		}
		return strings.TrimSpace(forwarded)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func productOf(cfg *config.Config, id string) config.Product {
	for _, product := range cfg.Products {
		if product.ID == id {
			return product
		}
	}
	return config.Product{}
}

// hopByHop 是逐跳头：透传会破坏本进程的连接管理或与自有响应体长度冲突。
var hopByHop = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	"Content-Length":      {},
}

func copyHeader(dst, src http.Header) {
	for name, values := range src {
		if _, skip := hopByHop[http.CanonicalHeaderKey(name)]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{"message": message, "type": "gateway_error"},
	})
}
