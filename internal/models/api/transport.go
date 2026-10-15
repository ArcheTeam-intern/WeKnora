package api

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// LLM 调用超时配置。仅作为"上层未设置 deadline 时"的兜底，避免 hung 请求
// 永久阻塞 worker。如果上层 ctx 已经设置了 deadline（无论比默认更短还是更长），
// 都会原样尊重，不再叠加默认超时。可通过环境变量覆盖：
//   - WEKNORA_LLM_CHAT_TIMEOUT_SECONDS    非流式调用兜底超时（默认 600s）
//   - WEKNORA_LLM_STREAM_TIMEOUT_SECONDS  流式调用兜底超时（默认 1800s）
var (
	DefaultChatTimeout   = envDurationSeconds("WEKNORA_LLM_CHAT_TIMEOUT_SECONDS", 300*time.Second)
	DefaultStreamTimeout = envDurationSeconds("WEKNORA_LLM_STREAM_TIMEOUT_SECONDS", 600*time.Second)
)

// envDurationSeconds 读取以"秒"为单位的环境变量，解析失败或非正值时回退到 fallback。
func envDurationSeconds(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return time.Duration(n) * time.Second
}

// WithLLMTimeout 仅在上层 ctx 没有 deadline 时附加一个兜底超时；
// 如果上层已显式设置 deadline（无论更短或更长），则原样返回，
// 让调用方对自己的超时策略拥有最终决定权。
func WithLLMTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// FirstByteTimeout bounds how long a model call waits for the first response
// byte (transport-level; generation itself is governed by the caller's context
// deadline and streaming keeps the body open afterwards). A local proxy / TUN
// (Clash and friends) can silently drop an idle keep-alive connection, after
// which the next request on that connection stalls with no error at all:
// observed live as an MCP `ask` sitting until its 240s deadline ("read
// response: context deadline exceeded") while fresh connections (curl)
// answered the same vendor in under a second. With this bound the stall
// surfaces as a transport timeout the caller can retry. IdleConnTimeout stays
// below that middlebox window so dead connections are retired before they can
// be reused. Override with WEKNORA_LLM_RESPONSE_HEADER_TIMEOUT_SECONDS for
// vendors that only flush headers after generation completes.
var FirstByteTimeout = envDurationSeconds("WEKNORA_LLM_RESPONSE_HEADER_TIMEOUT_SECONDS", 60*time.Second)

// newLLMTransport builds the shared transport with an explicit first-byte
// bound, so tests can exercise the timeout/retry behaviour against a real
// HTTP server instead of a mock that skips the transport.
func newLLMTransport(firstByteTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           secutils.SSRFSafeDialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: firstByteTimeout,
		// Below the middlebox idle window so dead connections retire before
		// they can be reused.
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConnsPerHost: 5,
	}
}

var httpTransport = newLLMTransport(FirstByteTimeout)

// HTTPClient is the shared SSRF-safe client every protocol uses.
var HTTPClient = secutils.NewSSRFSafeHTTPClientWithTransport(
	secutils.SSRFSafeHTTPClientConfig{Timeout: 0, MaxRedirects: 10},
	httpTransport,
)
