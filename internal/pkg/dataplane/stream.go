package dataplane

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/proxy"
	"model-gateway/internal/pkg/reqlog"
	"model-gateway/internal/router"
)

// streamRequest 只关心是否需要流式，其余字段原样透传。
func streamRequest(body []byte) bool {
	var head struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return false
	}
	return head.Stream
}

// streamChat 处理 stream=true：首字节写出前失败仍换成员；写出后失败只记录不重试。
func (h *Handler) streamChat(w http.ResponseWriter, r *http.Request, cfg *config.Config,
	body []byte, groupName, sessionKey string, record config.Token) {

	maxAttempts := cfg.Routing.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	tried := make(map[string]struct{}, maxAttempts)
	attempts := make([]Attempt, 0, maxAttempts)

	started := time.Now()
	var lastStatus int
	var lastOutcome proxy.Outcome
	var lastTTFB int64
	var lastUsage proxy.Usage
	var lastProduct string
	// 400/422 的上游响应要留着：所有成员都拒绝时把它原样回给客户端。
	var lastPayload []byte
	var lastHeader http.Header
	var lastUpstreamError string
	defer func() {
		h.noteUsage(record.ID, lastProduct, lastOutcome)
		entry := reqlog.Entry{
			RequestID: requestID(), ClientRequestID: clientRequestID(r),
			ClientIP: clientIP(r), UA: r.UserAgent(),
			TokenID: record.ID, TokenName: record.Name, Group: groupName, Stream: true,
			DurationMs: time.Since(started).Milliseconds(),
			HTTPStatus: lastStatus, Outcome: string(lastOutcome),
			TTFBMs: lastTTFB, Attempts: toReqlogAttempts(attempts),
			PromptTokens: lastUsage.PromptTokens, CompletionTokens: lastUsage.CompletionTokens,
			TotalTokens:    lastUsage.TotalTokens,
			CacheHitTokens: int64(lastUsage.CacheHitTokens),
			CacheHitRate:   cacheHitRate(lastUsage.CacheHitTokens, lastUsage.PromptTokens),
			Credit:         lastUsage.Credit,
			CreditKnown:    lastUsage.HasCredit,
			UpstreamError:  lastUpstreamError,
		}
		if len(attempts) > 0 {
			last := attempts[len(attempts)-1]
			entry.Product, entry.Model = last.Product, last.Model
		}
		h.writeLog(entry)
		h.recordUsage(entry, lastUsage, lastOutcome == proxy.OutcomeOK)
	}()

	for i := 0; i < maxAttempts; i++ {
		if clientGone(r) {
			lastStatus, lastOutcome = statusClientClosed, proxy.OutcomeClientCanceled
			writeError(w, statusClientClosed, "客户端已断开")
			return
		}
		member, key, pickErr := h.pick(cfg, groupName, sessionKey, tried)
		if pickErr != nil {
			if errors.Is(pickErr, router.ErrGroupNotFound) {
				lastStatus, lastOutcome = http.StatusNotFound, proxy.Outcome("group_not_found")
				writeError(w, http.StatusNotFound, "模型组不存在")
				return
			}
			if errors.Is(pickErr, router.ErrGroupDisabled) {
				lastStatus, lastOutcome = http.StatusForbidden, proxy.Outcome("group_disabled")
				writeError(w, http.StatusForbidden, "模型组已禁用")
				return
			}
			if len(attempts) == 0 {
				lastStatus = http.StatusServiceUnavailable
				lastOutcome = proxy.Outcome("no_member")
				writeError(w, http.StatusServiceUnavailable, "无可用成员")
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
		response, err := h.forwarder.DoStreamResponse(r.Context(), product, body, member.Model)
		if err != nil {
			elapsed := time.Since(started)
			// 客户端断开会让 r.Context() 取消：后续重试必然 0ms 失败，
			// 且这不是上游故障，不能把成员打进冷却。
			if clientGone(r) {
				lastStatus, lastOutcome = statusClientClosed, proxy.OutcomeClientCanceled
				attempts = append(attempts, Attempt{Product: member.Product, Model: member.Model, Code: string(lastOutcome), MS: elapsed.Milliseconds()})
				writeError(w, statusClientClosed, "客户端已断开")
				return
			}
			h.fail(key, transportCooldown(cfg.Routing.Cooldown, elapsed), shortError(err))
			attempts = append(attempts, Attempt{Product: member.Product, Model: member.Model, Code: string(proxy.OutcomeUpstream), MS: elapsed.Milliseconds()})
			lastStatus = http.StatusBadGateway
			lastOutcome = proxy.OutcomeUpstream
			continue
		}
		ttfb := time.Since(started)

		outcome := proxy.Classify(response.StatusCode)
		if outcome != proxy.OutcomeOK {
			// 首字节尚未写出：读掉少量 body 便于分类后关闭，换下一个成员。
			payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			attempts = append(attempts, Attempt{
				Product: member.Product, Model: member.Model,
				Status: response.StatusCode, Code: string(outcome),
				MS: time.Since(started).Milliseconds(),
			})
			lastStatus, lastOutcome, lastTTFB = response.StatusCode, outcome, ttfb.Milliseconds()
			// 400 / 422 不一定是请求本身错：不同上游对同一份请求的宽容度不同，
			// 换成员仍可能成功。保留响应体继续降级；只有「重试也没用」的那类
			// （额度耗尽 / 地区不支持 / 凭据无效）才把成员摘一段时间。
			if outcome == proxy.OutcomeClientError {
				lastPayload, lastHeader = payload, response.Header.Clone()
				lastUpstreamError = upstreamErrorSummary(payload)
				if unavailableClientError(lastUpstreamError) {
					h.park(key, cfg.Routing.Cooldown.UnavailableDuration, "上游不可用（HTTP 400）")
				} else if rule, ok := cfg.Routing.Cooldown.RuleFor(response.StatusCode); ok {
					h.countStatus(key, rule)
				}
				continue
			}
			// 其他状态码命中 status_rules 时走独立计数，不再叠加指数退避。
			if rule, ok := cfg.Routing.Cooldown.RuleFor(response.StatusCode); ok {
				h.countStatus(key, rule)
				continue
			}
			// 401 / 403 是产品凭据问题：不冷却成员，直接换下一个产品。
			if outcome != proxy.OutcomeProductAuth {
				h.fail(key, cfg.Routing.Cooldown, statusReason(outcome, response.StatusCode))
			}
			continue
		}

		copyHeader(w.Header(), response.Header)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(response.StatusCode)
		flusher, _ := w.(http.Flusher)

		lastStatus, lastTTFB = response.StatusCode, ttfb.Milliseconds()
		usage, copyErr := proxy.StreamCopy(w, response.Body, flusher)
		response.Body.Close()
		lastUsage = usage
		attempts = append(attempts, Attempt{
			Product: member.Product, Model: member.Model, Status: response.StatusCode,
			Code: string(proxy.OutcomeOK), MS: time.Since(started).Milliseconds(),
		})
		if copyErr != nil {
			// 已写出首字节：按 mid_stream 记失败，不再重试。
			lastOutcome = proxy.Outcome("mid_stream")
			attempts[len(attempts)-1].Code = string(lastOutcome)
			h.fail(key, cfg.Routing.Cooldown, "mid_stream")
		} else {
			lastOutcome = proxy.OutcomeOK
			h.succeed(key)
			h.bindSticky(cfg, sessionKey, key)
		}
		return
	}

	// 最后一次尝试是「上游拒绝请求」：把它的响应原样回给客户端。
	if lastOutcome == proxy.OutcomeClientError && len(lastPayload) > 0 {
		copyHeader(w.Header(), lastHeader)
		w.WriteHeader(lastStatus)
		_, _ = w.Write(lastPayload)
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]interface{}{
		"error": map[string]interface{}{
			"message": "all members failed", "type": "upstream_error", "attempts": attempts,
		},
	})
}
