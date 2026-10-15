package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// timeoutErr mimics what ResponseHeaderTimeout produces on a keep-alive
// connection a middlebox dropped silently.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "net/http: timeout awaiting response headers" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// flakyRoundTripper fails the first `failures` attempts with `failErr`,
// recording every request body so replayability can be asserted.
type flakyRoundTripper struct {
	attempts int
	bodies   []string
	failures int
	failErr  error
}

func (t *flakyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.attempts++
	if req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		t.bodies = append(t.bodies, string(data))
	}
	if t.attempts <= t.failures {
		return nil, t.failErr
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func newFlakyEndpoint(t *testing.T, rt *flakyRoundTripper) Endpoint {
	t.Helper()
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	return Endpoint{BaseURL: "http://127.0.0.1", Client: &http.Client{Transport: rt}}
}

// A timeout awaiting response headers — the dropped-keep-alive shape — is
// retried once on a fresh connection with the identical body, so a stalled
// QA turn becomes a fast success instead of a 240s deadline.
func TestDoRetriesOneTimeoutWithTheSameBody(t *testing.T) {
	rt := &flakyRoundTripper{failures: 1, failErr: timeoutErr{}}
	endpoint := newFlakyEndpoint(t, rt)

	var out map[string]any
	err := endpoint.PostJSON(context.Background(), endpoint.Resolve("/chat/completions"),
		map[string]any{"model": "m", "messages": []string{"hi"}}, &out)

	require.NoError(t, err)
	assert.Equal(t, true, out["ok"])
	assert.Equal(t, 2, rt.attempts, "exactly one retry is allowed")
	require.Len(t, rt.bodies, 2)
	assert.Equal(t, rt.bodies[0], rt.bodies[1], "the retried request must replay the same body")
	assert.Contains(t, rt.bodies[0], `"model":"m"`)
}

// Any other transport failure keeps its single attempt here: the caller's
// own policy (PostJSONWithRetry) owns those retries.
func TestDoDoesNotRetryNonTimeoutFailures(t *testing.T) {
	rt := &flakyRoundTripper{failures: 1, failErr: errors.New("connection refused")}
	endpoint := newFlakyEndpoint(t, rt)

	var out map[string]any
	err := endpoint.PostJSON(context.Background(), endpoint.Resolve("/chat/completions"),
		map[string]any{"a": 1}, &out)

	require.Error(t, err)
	assert.Equal(t, 1, rt.attempts)
	var transportErr *TransportError
	assert.True(t, errors.As(err, &transportErr))
}

// Once the caller's own context is done its budget is gone: no retry.
func TestDoDoesNotRetryAfterCallerDeadline(t *testing.T) {
	rt := &flakyRoundTripper{failures: 1, failErr: timeoutErr{}}
	endpoint := newFlakyEndpoint(t, rt)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out map[string]any
	err := endpoint.PostJSON(ctx, endpoint.Resolve("/chat/completions"), map[string]any{"a": 1}, &out)

	require.Error(t, err)
	assert.Equal(t, 1, rt.attempts)
}

// A vendor answering with a status is not a transport stall: no retry.
func TestDoDoesNotRetryHTTPStatusFailures(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	statusRT := &statusRoundTripper{}
	endpoint := Endpoint{BaseURL: "http://127.0.0.1", Client: &http.Client{Transport: statusRT}}
	var out map[string]any

	err := endpoint.PostJSON(context.Background(), endpoint.Resolve("/chat/completions"), map[string]any{}, &out)
	require.Error(t, err)
	var httpErr *HTTPError
	assert.True(t, errors.As(err, &httpErr))
	assert.Equal(t, 1, statusRT.attempts)
}

type statusRoundTripper struct{ attempts int }

func (t *statusRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.attempts++
	return &http.Response{
		StatusCode: 400,
		Body:       io.NopCloser(strings.NewReader(`{"error":"bad request"}`)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

// 真实 HTTP 回归：ResponseHeaderTimeout 的真实错误形状（被 Go 包装成
// context.DeadlineExceeded）在调用方仍存活时必须触发恰好一次重试，
// 且重试使用可重放的同一 body。这证明了 mock 测试不能证明的传输行为。
func TestDoRetriesRealResponseHeaderTimeoutOverHTTP(t *testing.T) {
	var attempts atomic.Int32
	firstStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// 第一次：比首包超时更慢，触发真实的 ResponseHeaderTimeout。
			select {
			case firstStarted <- struct{}{}:
			default:
			}
			time.Sleep(400 * time.Millisecond)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	transport := newLLMTransport(50 * time.Millisecond)
	endpoint := Endpoint{BaseURL: server.URL, Client: &http.Client{Transport: transport}}

	var out map[string]any
	err := endpoint.PostJSON(context.Background(), endpoint.Resolve("/chat/completions"),
		map[string]any{"model": "m", "messages": []string{"hi"}}, &out)

	require.NoError(t, err, "首包超时应在同一 body 上重试并成功")
	assert.Equal(t, int32(2), attempts.Load(), "必须恰好两次尝试")
	assert.Equal(t, true, out["ok"])
}

// 调用方自己的 context 过期则绝不重试：那是预算耗尽，不是死连接。
func TestDoDoesNotRetryCallerDeadlineOverHTTP(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	// 传输层首包窗口很宽：超时只可能来自调用方自己的 deadline。
	transport := newLLMTransport(10 * time.Second)
	endpoint := Endpoint{BaseURL: server.URL, Client: &http.Client{Transport: transport}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	var out map[string]any
	err := endpoint.PostJSON(ctx, endpoint.Resolve("/chat/completions"),
		map[string]any{"a": 1}, &out)

	require.Error(t, err)
	assert.LessOrEqual(t, attempts.Load(), int32(1), "调用方 deadline 过期不得重试")
}
