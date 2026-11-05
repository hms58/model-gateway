package dataplane

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/pkg/proxy"
	"model-gateway/internal/pkg/reqlog"
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
	defer func() {
		h.noteUsage(record.ID, lastProduct, lastOutcome)
		entry := reqlog.Entry{
			RequestID: requestID(), ClientIP: clientIP(r), UA: r.UserAgent(),
			TokenID: record.ID, TokenName: record.Name, Group: groupName, Stream: true,
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
			if len(attempts) == 0 {
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
			h.fail(key, cfg.Routing.Cooldown, shortError(err))
			attempts = append(attempts, Attempt{Product: member.Product, Model: member.Model, Code: string(proxy.OutcomeUpstream), MS: time.Since(started).Milliseconds()})
			lastStatus = http.StatusBadGateway
			lastOutcome = proxy.OutcomeUpstream
			continue
		}
		ttfb := time.Since(started)

		outcome := proxy.Classify(response.StatusCode)
		if outcome != proxy.OutcomeOK {
			// 首字节尚未写出：读掉少量 body 便于分类后关闭，换下一个成员。
			_, _ = io.CopyN(io.Discard, response.Body, 512)
			response.Body.Close()
			attempts = append(attempts, Attempt{
				Product: member.Product, Model: member.Model,
				Status: response.StatusCode, Code: string(outcome),
				MS: time.Since(started).Milliseconds(),
			})
			lastStatus, lastOutcome, lastTTFB = response.StatusCode, outcome, ttfb.Milliseconds()
			// 401 / 403 是产品凭据问题：不冷却成员，直接换下一个产品。
			if outcome != proxy.OutcomeProductAuth {
				h.fail(key, cfg.Routing.Cooldown, statusReason(outcome, response.StatusCode))
			}
			if outcome == proxy.OutcomeClientError {
				writeError(w, response.StatusCode, "上游拒绝请求")
				return
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

	writeJSON(w, http.StatusBadGateway, map[string]interface{}{
		"error": map[string]interface{}{
			"message": "all members failed", "type": "upstream_error", "attempts": attempts,
		},
	})
}
