package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"model-gateway/internal/config"
)

// Outcome 是请求日志与降级判定共用的结果分类。
type Outcome string

const (
	OutcomeOK          Outcome = "ok"
	OutcomeClientError Outcome = "client_error"
	OutcomeProductAuth Outcome = "product_auth"
	OutcomeQuota       Outcome = "quota"
	OutcomeUpstream    Outcome = "upstream"
)

// Classify 把下游状态码映射成分类；换不换成员由调用方按分类决定。
func Classify(status int) Outcome {
	switch {
	case status >= 200 && status < 300:
		return OutcomeOK
	case status == 400 || status == 422:
		return OutcomeClientError
	case status == 401 || status == 403:
		return OutcomeProductAuth
	case status == 402 || status == 429:
		return OutcomeQuota
	default:
		return OutcomeUpstream
	}
}

// RewriteModel 只替换 model 字段：解码为 RawMessage 保留其余字段原样，
// 避免 messages 大数组被反复序列化、浮点精度被改写。
func RewriteModel(body []byte, model string) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	fields["model"] = raw
	return json.Marshal(fields)
}

type Response struct {
	Status   int
	Header   http.Header
	Body     []byte
	TTFB     time.Duration
	Duration time.Duration
}

// Forwarder 按产品超时构建并复用 http.Client：平台默认 30s 超时会掐断长请求，
// 首包超时才是降级依据（见 config.Product.Timeout）。
type Forwarder struct {
	clients sync.Map // 超时指纹 -> *http.Client
}

func NewForwarder() *Forwarder { return &Forwarder{} }

func parseDuration(raw string, fallback time.Duration) time.Duration {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// clientFor 按产品超时取 client：流式不设总超时（否则长流被截断），
// 非流式用 total 兜底；connect 与首包超时由 Transport 承担。
func (f *Forwarder) clientFor(product config.Product, streaming bool) *http.Client {
	connect := parseDuration(product.Timeout.Connect, 5*time.Second)
	firstByte := parseDuration(product.Timeout.FirstByte, 30*time.Second)
	total := time.Duration(0)
	if !streaming {
		total = parseDuration(product.Timeout.Total, 300*time.Second)
	}
	key := fmt.Sprintf("%s|%s|%s|%s|%t", product.ID, connect, firstByte, total, streaming)
	if cached, ok := f.clients.Load(key); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: connect}).DialContext,
			ResponseHeaderTimeout: firstByte,
			MaxIdleConnsPerHost:   32,
		},
		Timeout: total,
	}
	actual, _ := f.clients.LoadOrStore(key, client)
	return actual.(*http.Client)
}

// Do 发起一次非流式转发并把响应体读全（供测试与需要完整 body 的调用方）；
// 数据面走 DoResponse + StreamCopy，不整包驻留内存。
func (f *Forwarder) Do(ctx context.Context, product config.Product, body []byte, model string) (Response, error) {
	started := time.Now()
	response, err := f.DoResponse(ctx, product, body, model)
	if err != nil {
		return Response{}, err
	}
	defer response.Body.Close()
	ttfb := time.Since(started)

	payload, err := io.ReadAll(response.Body)
	finished := time.Now()
	if err != nil {
		return Response{}, err
	}
	return Response{
		Status:   response.StatusCode,
		Header:   response.Header.Clone(),
		Body:     payload,
		TTFB:     ttfb,
		Duration: finished.Sub(started),
	}, nil
}
