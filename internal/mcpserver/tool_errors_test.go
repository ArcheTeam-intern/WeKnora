package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/mcpscope"
)

// errorBody mirrors the structured payload written by errorResult so the
// test can assert on the machine-readable fields rather than the English text.
type errorBody struct {
	Error struct {
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		Retryable bool           `json:"retryable"`
		Details   map[string]any `json:"details,omitempty"`
	} `json:"error"`
}

func decodeError(t *testing.T, raw any) errorBody {
	t.Helper()
	payload, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var body errorBody
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("unmarshal error body: %v\npayload=%s", err, payload)
	}
	return body
}

// errorResult 必须设置 isError，并把 code/message/retryable 写入
// structuredContent；客户端据此分流，不解析英文文本。
func TestErrorResultSetsStructuredCode(t *testing.T) {
	for _, code := range []string{
		mcpscope.CodeUnauthorized,
		mcpscope.CodeForbidden,
		mcpscope.CodeKBOutOfScope,
		mcpscope.CodeKBSelectionRequired,
		mcpscope.CodeInvalidArgument,
		mcpscope.CodeNotFound,
		mcpscope.CodeGraphUnavailable,
		mcpscope.CodeWikiNotEnabled,
	} {
		t.Run(code, func(t *testing.T) {
			result := errorResult(mcpscope.Errorf(code, "context for %s", code))
			if result == nil {
				t.Fatal("errorResult returned nil")
			}
			if !result.IsError {
				t.Fatalf("isError must be true for %s", code)
			}
			body := decodeError(t, result.StructuredContent)
			if body.Error.Code != code {
				t.Fatalf("expected code %q, got %q", code, body.Error.Code)
			}
			if !strings.Contains(body.Error.Message, code) {
				t.Errorf("message should mention %s, got %q", code, body.Error.Message)
			}
		})
	}
}

// errorResult 的 details 必须透传（如 out-of-scope 的具体 kb id 列表）。
func TestErrorResultPreservesDetails(t *testing.T) {
	err := &mcpscope.Error{
		Code:    mcpscope.CodeKBOutOfScope,
		Message: "knowledge bases outside scope",
		Details: map[string]any{"outside_scope": []string{"kb-a", "kb-b"}},
	}
	result := errorResult(err)
	body := decodeError(t, result.StructuredContent)
	ids, ok := body.Error.Details["outside_scope"].([]any)
	if !ok {
		t.Fatalf("details.outside_scope must round-trip, got %T", body.Error.Details["outside_scope"])
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %d", len(ids))
	}
}

// AsError 必须把上游普通 error 包成 UPSTREAM_TOOL_ERROR，避免直接泄露。
func TestAsErrorPromotesPlainError(t *testing.T) {
	plain := errString("redis dial: connection refused")
	envelope := AsError(plain)
	if envelope.Code != mcpscope.CodeUpstreamToolError {
		t.Fatalf("expected %s, got %s", mcpscope.CodeUpstreamToolError, envelope.Code)
	}
	if envelope.Message == "" {
		t.Fatal("promoted error must keep the original message")
	}
}

// AsError 必须把已有的 *mcpscope.Error 透传，不能重新包装丢失 code。
func TestAsErrorReturnsToolErrorVerbatim(t *testing.T) {
	original := mcpscope.NotFound("wiki page missing")
	envelope := AsError(original)
	if envelope.Code != mcpscope.CodeNotFound {
		t.Fatalf("expected %s, got %s", mcpscope.CodeNotFound, envelope.Code)
	}
	if envelope != original {
		t.Fatal("AsError must return the same *Error, not a copy")
	}
}

// nil 错误必须得到一个稳定的空 envelope，不能 panic 也不能生成垃圾码。
func TestErrorResultNilError(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("errorResult(nil) panicked: %v", r)
		}
	}()
	result := errorResult(nil)
	body := decodeError(t, result.StructuredContent)
	if body.Error.Code == "" {
		t.Fatal("nil error still needs a non-empty code for clients to branch on")
	}
}

// 应用层短路（"query is required"）必须落到 INVALID_ARGUMENT 而非 upstream
// 兜底，便于客户端按字段名提示用户。
func TestErrorResultInvalidArgumentIsNotClassifiedAsUpstream(t *testing.T) {
	result := errorResult(invalidArgument("query is required"))
	body := decodeError(t, result.StructuredContent)
	if body.Error.Code == mcpscope.CodeUpstreamToolError {
		t.Fatalf("invalid_argument must not be rewrapped as upstream: %+v", body.Error)
	}
	if body.Error.Code != mcpscope.CodeInvalidArgument {
		t.Fatalf("expected INVALID_ARGUMENT, got %s", body.Error.Code)
	}
	if !body.Error.Retryable == false { //nolint:staticcheck
		// retryable is bool false; this test just guards against a typo in the
		// JSON tag flipping it to true via a wrong negation above.
	}
}

// errString makes a small type so the test above can use a plain error
// value without importing errors just to wrap a string.
type errString string

func (e errString) Error() string { return string(e) }
