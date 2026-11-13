package dataplane

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/proxy"
	"model-gateway/internal/pkg/reqlog"
	"model-gateway/internal/pkg/store"
	"model-gateway/internal/pkg/token"
	"model-gateway/internal/pkg/usage"
	"model-gateway/internal/router"
)

// MaxBodyBytes 限制请求体，避免超大 messages 打爆内存。
const MaxBodyBytes = 32 << 20

// maxErrorBodyBytes 是「非 2xx 响应」最多读入的字节数：够分类、够透传给客户端，
// 又不会因为上游回一整包错误页把内存吃掉。
const maxErrorBodyBytes = 1 << 20

// statusClientClosed 沿用 Nginx 的 499：客户端在收到响应前断开，便于日志区分。
const statusClientClosed = 499

// clientGone 判断入站请求是否已断开：此时下游失败不是上游故障，
// 不应继续降级，更不该把成员打进冷却。
func clientGone(r *http.Request) bool {
	return r.Context().Err() != nil
}

type Handler struct {
	store     *store.Store
	forwarder *proxy.Forwarder
	logger    *reqlog.Logger
	usage     *usage.Recorder
}

// WithLogger 注入请求日志器；未注入时不记日志（不影响转发）。
func (h *Handler) WithLogger(logger *reqlog.Logger) *Handler {
	h.logger = logger
	return h
}

// WithUsage 注入用量累计器（面板「统计」页的数据源）；未注入时不统计。
// 与请求日志相互独立：日志受保留期限制会删文件，累计器自己折叠后长期保留。
func (h *Handler) WithUsage(recorder *usage.Recorder) *Handler {
	h.usage = recorder
	return h
}

func New(st *store.Store) *Handler {
	return &Handler{store: st, forwarder: proxy.NewForwarder()}
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

	routing := relayRequest{
		cfg:        cfg,
		body:       body,
		groupName:  groupName,
		sessionKey: strings.TrimSpace(r.Header.Get("X-Session-Id")),
		record:     record,
		streaming:  streamRequest(body),
	}
	h.relay(w, r, routing)
}

// relayRequest 是一次降级链的固定输入；流式与非流式只差 streaming 一个布尔位。
type relayRequest struct {
	cfg        *config.Config
	body       []byte
	groupName  string
	sessionKey string
	record     config.Token
	streaming  bool
}

// relayState 累积整条降级链的观测数据，供 defer 里统一落日志与用量。
//
// 抽出来是因为流式与非流式必须产出**同一套** attempts / outcome / status 口径：
// 两份拷贝曾经各自演化（失败码一个回 lastStatus、一个回 502），
// 排查时「同一个故障在流式与非流式日志里对不上」。
type relayState struct {
	attempts          []Attempt
	status            int
	outcome           proxy.Outcome
	ttfbMs            int64
	usage             proxy.Usage
	product           string
	lastPayload       []byte
	lastHeader        http.Header
	lastUpstreamError string
}

// relay 是数据面唯一的降级主循环：选路 → 转发 → 分类 → 冷却 / 计数 → 降级。
// streaming 只影响三处：用哪个转发方法、成功时是否强制 text/event-stream、请求名。
func (h *Handler) relay(w http.ResponseWriter, r *http.Request, req relayRequest) {
	maxAttempts := req.cfg.Routing.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	tried := make(map[string]struct{}, maxAttempts)
	state := &relayState{attempts: make([]Attempt, 0, maxAttempts)}

	started := time.Now()
	// 半开探测名额必须归还：401/403、status_rules 命中、客户端断开这三条分支
	// 都不会走 Success/Probe，漏还的名额会让成员永久「不可选也不被探活」。
	// 每次尝试结束（下一轮选路前）就归还，名额只在本次尝试期间被占用。
	var claimedProbes []string
	releaseProbes := func() {
		for _, key := range claimedProbes {
			h.store.State().ReleaseProbe(key)
		}
		claimedProbes = claimedProbes[:0]
	}
	defer releaseProbes()
	defer func() {
		h.noteUsage(req.record.ID, state.product, state.outcome)
		// 时间在日志里显式赋值：提示词快照要用同一个时间戳，
		// 否则两处各取一次 now 会出现「同一次请求两条记录时间不一致」。
		entry := reqlog.Entry{
			Time:      time.Now().In(config.Location()).Format(config.TimeLayout),
			RequestID: requestID(), ClientRequestID: clientRequestID(r),
			ClientIP: clientIP(r), UA: r.UserAgent(),
			TokenID: req.record.ID, TokenName: req.record.Name,
			Group: req.groupName, Stream: req.streaming,
			DurationMs: time.Since(started).Milliseconds(),
			HTTPStatus: state.status, Outcome: string(state.outcome),
			TTFBMs: state.ttfbMs, Attempts: toReqlogAttempts(state.attempts),
			PromptTokens: state.usage.PromptTokens, CompletionTokens: state.usage.CompletionTokens,
			TotalTokens:    state.usage.TotalTokens,
			CacheHitTokens: int64(state.usage.CacheHitTokens),
			CacheHitRate:   cacheHitRate(state.usage.CacheHitTokens, state.usage.PromptTokens),
			Credit:         state.usage.Credit,
			UpstreamError:  state.lastUpstreamError,
			CreditKnown:    state.usage.HasCredit,
		}
		if len(state.attempts) > 0 {
			last := state.attempts[len(state.attempts)-1]
			entry.Product, entry.Model = last.Product, last.Model
		}
		// 提示词：日志里只放摘要（列表一次 100 条，不能每条都带上百 KB 原文），
		// 完整快照另存 prompts-*.jsonl，由面板详情接口按 request_id 分片读取。
		// 没接日志器时直接跳过解析：多一次整包 Unmarshal 对转发路径没有价值。
		if h.logger != nil {
			if prompt, summary, ok := reqlog.CaptureChatPrompt(req.body, reqlog.PromptLimitChars); ok {
				prompt.RequestID, prompt.Time, prompt.Group = entry.RequestID, entry.Time, req.groupName
				summary.Stored = h.writePrompt(prompt)
				entry.Prompt = &summary
			}
		}
		h.writeLog(entry)
		h.recordUsage(entry, state.usage, state.outcome == proxy.OutcomeOK)
	}()

	for i := 0; i < maxAttempts; i++ {
		releaseProbes() // 上一轮尝试已得出结论，名额不该跨尝试持有
		if clientGone(r) {
			state.status, state.outcome = statusClientClosed, proxy.OutcomeClientCanceled
			writeError(w, statusClientClosed, "客户端已断开")
			return
		}
		member, key, pickErr := h.pick(req.cfg, req.groupName, req.sessionKey, tried)
		if pickErr != nil {
			switch {
			case errors.Is(pickErr, router.ErrGroupNotFound):
				state.status, state.outcome = http.StatusNotFound, proxy.Outcome("group_not_found")
				writeError(w, http.StatusNotFound, "模型组不存在")
			case errors.Is(pickErr, router.ErrGroupDisabled):
				state.status, state.outcome = http.StatusForbidden, proxy.Outcome("group_disabled")
				writeError(w, http.StatusForbidden, "模型组已禁用")
			case len(state.attempts) == 0:
				state.status, state.outcome = http.StatusServiceUnavailable, proxy.Outcome("no_member")
				writeError(w, http.StatusServiceUnavailable, "无可用成员")
			default:
				// 已试成员全被排除：跳出循环走「最后一次上游响应」收尾。
			}
			if len(state.attempts) == 0 || errors.Is(pickErr, router.ErrGroupNotFound) ||
				errors.Is(pickErr, router.ErrGroupDisabled) {
				return
			}
			break
		}
		tried[key] = struct{}{}
		claimedProbes = append(claimedProbes, key)

		product := productOf(req.cfg, member.Product)
		state.product = member.Product
		if !supportedProtocol(product) {
			state.status, state.outcome = http.StatusBadRequest, proxy.Outcome("unsupported_protocol")
			state.attempts = append(state.attempts, Attempt{
				Product: member.Product, Model: member.Model,
				Status: http.StatusBadRequest, Code: string(state.outcome),
			})
			writeProtocolError(w)
			return
		}

		attemptStarted := time.Now()
		response, callErr := h.forward(r.Context(), req, product, member.Model)
		ttfb := time.Since(attemptStarted)
		elapsed := ttfb.Milliseconds()
		if callErr != nil {
			// 客户端断开会让 r.Context() 取消：后续重试必然 0ms 失败，
			// 且这不是上游故障，不能把成员打进冷却。
			if clientGone(r) {
				state.status, state.outcome = statusClientClosed, proxy.OutcomeClientCanceled
				state.attempts = append(state.attempts, Attempt{
					Product: member.Product, Model: member.Model, Code: string(state.outcome), MS: elapsed,
				})
				writeError(w, statusClientClosed, "客户端已断开")
				return
			}
			h.fail(key, transportCooldown(req.cfg.Routing.Cooldown, ttfb), shortError(callErr))
			state.attempts = append(state.attempts, Attempt{
				Product: member.Product, Model: member.Model, Code: string(proxy.OutcomeUpstream), MS: elapsed,
			})
			state.status = http.StatusBadGateway
			state.outcome = proxy.OutcomeUpstream
			continue
		}

		outcome := proxy.Classify(response.StatusCode)
		state.attempts = append(state.attempts, Attempt{
			Product: member.Product, Model: member.Model,
			Status: response.StatusCode, Code: string(outcome), MS: elapsed,
		})
		state.status, state.outcome = response.StatusCode, outcome
		state.ttfbMs = elapsed

		if outcome == proxy.OutcomeOK {
			h.succeed(key)
			h.bindSticky(req.cfg, req.sessionKey, key)
			copyHeader(w.Header(), response.Header)
			if req.streaming {
				w.Header().Set("Content-Type", "text/event-stream")
			}
			w.WriteHeader(response.StatusCode)
			flusher, _ := w.(http.Flusher)
			usage, copyErr := proxy.StreamCopy(w, response.Body, flusher)
			response.Body.Close()
			state.usage = usage
			if copyErr != nil {
				// 已经写出字节：只记录，不重试。
				state.outcome = proxy.Outcome("mid_stream")
				state.attempts[len(state.attempts)-1].Code = string(state.outcome)
				h.fail(key, req.cfg.Routing.Cooldown, "mid_stream")
			}
			return
		}

		// 非 2xx：读掉有限字节再关闭。400/422 的响应体要留着（所有成员都拒时原样回给客户端），
		// 其余只用来让连接可复用。
		payload, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))
		response.Body.Close()

		if outcome == proxy.OutcomeClientError {
			state.lastPayload, state.lastHeader = payload, response.Header.Clone()
			state.lastUpstreamError = upstreamErrorSummary(payload)
			// 额度耗尽 / 地区不支持 / 凭据无效：重试同一成员也没用，摘一段时间；
			// 参数与 schema 不兼容则只降级，不把成员算成故障。
			if unavailableClientError(state.lastUpstreamError) {
				h.park(key, req.cfg.Routing.Cooldown.UnavailableDuration, "上游不可用（HTTP 400）")
			} else if rule, ok := req.cfg.Routing.Cooldown.RuleFor(response.StatusCode); ok {
				// 连续同状态码达到阈值：说明不是个别请求不兼容，而是协议/参数系统性变了，
				// 继续每个请求都白撞一次只会拖慢全部流量。
				h.countStatus(key, rule)
			}
			continue
		}

		// 其他状态码命中 status_rules 时同样走独立计数，不再叠加指数退避。
		if rule, ok := req.cfg.Routing.Cooldown.RuleFor(response.StatusCode); ok {
			h.countStatus(key, rule)
			continue
		}
		// 401 / 403 是产品凭据问题：换产品仍可能成功，但不该冷却成员。
		if outcome != proxy.OutcomeProductAuth {
			h.fail(key, req.cfg.Routing.Cooldown, statusReason(outcome, response.StatusCode))
		}
	}

	if state.status == 0 {
		state.status, state.outcome = http.StatusServiceUnavailable, proxy.Outcome("no_member")
		writeError(w, http.StatusServiceUnavailable, "无可用成员")
		return
	}
	// 最后一次尝试是「上游拒绝请求」：把它的响应原样回给客户端。
	// 客户端能直接看到是哪个上游、拒了什么，比自造的 all members failed 更有用。
	if state.outcome == proxy.OutcomeClientError && len(state.lastPayload) > 0 {
		copyHeader(w.Header(), state.lastHeader)
		w.WriteHeader(state.status)
		_, _ = w.Write(state.lastPayload)
		return
	}
	// 不复制上游头：上游的 Content-Length / 压缩标记与这里的自有 JSON 不一致，会导致客户端截断。
	writeJSON(w, state.status, map[string]interface{}{
		"error": map[string]interface{}{
			"message":  "all members failed",
			"type":     "upstream_error",
			"attempts": state.attempts,
		},
	})
}

// forward 按 stream 与否选择转发方法：唯一影响 I/O 形态的分支。
func (h *Handler) forward(ctx context.Context, req relayRequest, product config.Product, model string) (*http.Response, error) {
	if req.streaming {
		return h.forwarder.DoStreamResponse(ctx, product, req.body, model)
	}
	return h.forwarder.DoResponse(ctx, product, req.body, model)
}

// pick 选出一个未试成员。选路本身无需随机源，所以这里不持锁：
// 并发请求各自独立评估候选，共享状态由 State 自己的锁保护。
func (h *Handler) pick(cfg *config.Config, groupName, sessionKey string, tried map[string]struct{}) (config.Member, string, error) {
	return router.PickExcluding(cfg, h.store.State(), groupName, sessionKey, tried)
}

// transportCooldown 把传输层失败的实际耗时作为冷却基数下限。
// 首包超时 30s 却只冷却 2s 时，成员会在下一次尝试结束前就复活，
// 被反复选中并把并发请求一起打进冷却，表现为集中「无可用成员」。
func transportCooldown(base config.Cooldown, elapsed time.Duration) config.Cooldown {
	parsed, err := time.ParseDuration(base.Base)
	if err != nil || parsed <= 0 {
		parsed = 2 * time.Second
	}
	if elapsed > parsed {
		base.Base = elapsed.String()
	}
	return base
}

func (h *Handler) writeLog(entry reqlog.Entry) {
	if h.logger == nil {
		return
	}
	h.logger.Write(entry)
}

// writePrompt 落一条提示词快照；返回 false 表示没写成功（没接日志器或队列满），
// 调用方据此把摘要里的 stored 置为 false，面板就不会给出一个点不开的「详情」。
func (h *Handler) writePrompt(prompt reqlog.Prompt) bool {
	if h.logger == nil {
		return false
	}
	return h.logger.WritePrompt(prompt)
}

// recordUsage 把一次请求计入用量累计器（面板「统计」页的数据源）。
// 请求数与失败数按请求记；token 只在这次请求成功且上游确实回了 usage 时累加——
// 失败请求身上的 usage 往往是上一次尝试的残留，记进去会把 token 统计打偏。
// 耗时只对成功请求采样，所以面板上的「平均耗时」= 成功请求平均耗时。
func (h *Handler) recordUsage(entry reqlog.Entry, result proxy.Usage, ok bool) {
	if h.usage == nil {
		return
	}
	delta := usage.Delta{LatencyMs: entry.DurationMs, HasLatency: ok}
	if ok && result.Seen {
		delta.Tokens = true
		delta.PromptTokens = int64(result.PromptTokens)
		delta.CompletionTokens = int64(result.CompletionTokens)
		delta.TotalTokens = int64(result.TotalTokens)
		delta.CacheHitTokens = int64(result.CacheHitTokens)
	}
	h.usage.Add(time.Now(), entry.Product, entry.Model, delta, ok)
}

// noteUsage 维护 Token 计数与产品健康摘要：请求热路径上只做内存写 + 脏标记。
func (h *Handler) noteUsage(tokenID, product string, outcome proxy.Outcome) {
	if h.store == nil {
		return
	}
	state := h.store.State()
	state.NoteTokenUse(tokenID)
	if product != "" {
		switch outcome {
		case proxy.OutcomeOK:
			state.NoteProductOK(product)
		case proxy.OutcomeClientCanceled:
			// 客户端断开与上游无关，不污染产品健康摘要。
		default:
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
	return clipRunes(err.Error(), 160)
}

// upstreamErrorSummary 把上游 400/422 的响应压成一行摘要落进请求日志：
// 排查「哪个上游挑剔什么参数」时不必再靠外部复现。只保留前 200 个字符。
func upstreamErrorSummary(payload []byte) string {
	// 先按字节截断再转 rune：payload 最大 1MB，整串转 []rune 是 4 倍临时分配。
	text := strings.Join(strings.Fields(string(clipBytes(payload, 4<<10))), " ")
	return clipRunes(text, 200)
}

// unavailableClientError 判断 400/422 是否属于「重试同一成员也没用」的情况：
// 额度耗尽、地区不支持、凭据无效。其余（参数 / schema 不兼容）换成员仍可能成功。
func unavailableClientError(summary string) bool {
	text := strings.ToLower(summary)
	for _, marker := range []string{
		"quota", "rate limit", "rate_limit", "resource_exhausted", "resource exhausted",
		"billing", "exceeded your current", "location is not supported",
		"api key not valid", "invalid api key",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// park 把成员直接摘掉一段时间（不走指数退避、不累加失败计数），并落盘运行态。
func (h *Handler) park(key, raw, reason string) {
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		duration = 10 * time.Minute
	}
	h.store.State().SetCooldown(key, duration, reason)
	h.store.MarkDirty()
}

// countStatus 按 status_rules 累加连续失败；达到阈值时把成员摘掉并落盘运行态。
func (h *Handler) countStatus(key string, rule config.StatusRule) {
	reason := "HTTP " + strconv.Itoa(rule.Status) + " 连续失败 " + strconv.Itoa(rule.Threshold) + " 次"
	if h.store.State().CountStatus(key, rule, rule.RuleDuration(10*time.Minute), reason) {
		h.store.MarkDirty()
	}
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

// clientRequestID 取客户端自带的请求 id，便于把网关日志与上下游日志对上。
// 只认常见的几个头：客户端没带就留空，不自己编造（网关自己的 id 另有 request_id）。
func clientRequestID(r *http.Request) string {
	for _, name := range []string{"X-Request-Id", "X-Request-ID", "X-Trace-Id", "X-Correlation-Id", "Request-Id"} {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return clipRunes(value, 128)
		}
	}
	return ""
}

// cacheHitRate 由命中 token 与 prompt token 推算；拿不到 prompt 时返回 0（字段省略不展示）。
func cacheHitRate(hit, prompt int) float64 {
	if hit <= 0 || prompt <= 0 || hit > prompt {
		return 0
	}
	return float64(hit) / float64(prompt)
}

// clipBytes 按字节截断，只用于「随后还要继续解析」的场景（如 JSON 摘要），
// 不用于直接展示——字节截断会把中文切成乱码。
func clipBytes(text []byte, limit int) []byte {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// clipRunes 按字符截断，保证面板展示的中文不被切成非法 UTF-8。
func clipRunes(text string, limit int) string {
	if utf8RuneCount(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit])
}

// utf8RuneCount 统计字符数：ASCII 走快路径，避免为短文本分配 []rune。
func utf8RuneCount(text string) int {
	count := 0
	for i := 0; i < len(text); i++ {
		// UTF-8 续字节形如 10xxxxxx，跳过它们即可按首字节计数。
		if text[i]&0xC0 != 0x80 {
			count++
		}
	}
	return count
}

// clientIP 优先取反代写入的 X-Forwarded-For 首段，其次 RemoteAddr。
// 仅用于日志记录：是否放行请求由 Token / 组权限决定，不依赖来源地址。
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
