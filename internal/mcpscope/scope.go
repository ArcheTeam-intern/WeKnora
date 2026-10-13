// Package mcpscope holds the request-scoping primitives shared by the MCP
// server surface and the middleware that authenticates it.
//
// It is a leaf package on purpose: the middleware must parse and freeze the
// caller's knowledge-base selection BEFORE the MCP handler runs, and the tools
// must read that frozen selection, but middleware importing the whole mcpserver
// package (with its service graph) would be both heavy and cycle-prone. Keeping
// the selection type, its parser and the machine-readable error codes here means
// both sides depend on a tiny package instead of on each other.
package mcpscope

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// KnowledgeBaseScopeHeader is the optional request header carrying the caller's
// knowledge-base selection.
const KnowledgeBaseScopeHeader = "X-WeKnora-Knowledge-Base-Ids"

// MaxRequestKnowledgeBases bounds one request's selection. The plan fixes it at
// 10 so a single run cannot fan out over a whole workspace.
const MaxRequestKnowledgeBases = 10

// Scope is the caller's per-request knowledge-base selection.
//
// It only ever NARROWS what the caller may already do: it never grants anything.
// The effective range for every knowledge tool is
//
//	user authorization ∩ endpoint range ∩ request selection
//
// Two states are distinct and must not be conflated:
//
//   - the header is absent  -> Selected == nil, Present == false.
//     Legacy machine clients keep the historical behaviour (the endpoint's whole
//     range).
//   - the header is present -> Selected holds the canonical, deduplicated ids.
//     An empty value is a caller error, not "unrestricted".
type Scope struct {
	// Selected is the canonical selection in stable order. Nil when the header
	// was not sent at all.
	Selected []string
	// Present reports whether the header was sent, so an explicitly empty
	// selection can be rejected instead of silently meaning "everything".
	Present bool
	// Version changes whenever the selection changes, so a late event from a
	// cancelled run can be discarded by version.
	Version string
}

// scopeKey is the context key. Unexported so callers must go through WithScope.
type scopeKey struct{}

// WithScope attaches a scope to ctx.
func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// FromContext returns the scope attached to ctx. ok is false when no header was
// ever parsed, which is treated exactly like an absent header.
func FromContext(ctx context.Context) (Scope, bool) {
	scope, ok := ctx.Value(scopeKey{}).(Scope)
	return scope, ok
}

// Of is the convenience form used by tool handlers: an absent value yields the
// "header not sent" scope, which does not narrow anything.
func Of(ctx context.Context) Scope {
	scope, ok := FromContext(ctx)
	if !ok {
		return Scope{}
	}
	return scope
}

// Narrows reports whether this scope actually restricts anything. Handlers use it
// to skip intersection work on the legacy path.
func (s Scope) Narrows() bool { return s.Present && len(s.Selected) > 0 }

// Contains reports whether kbID is inside the frozen selection. It is the check
// every by-id read (document, wiki page, knowledge base status, upload target)
// must pass, so narrowing cannot be bypassed by naming a resource directly.
func (s Scope) Contains(kbID string) bool {
	if !s.Present {
		return true
	}
	target := strings.ToLower(strings.TrimSpace(kbID))
	for _, id := range s.Selected {
		if id == target {
			return true
		}
	}
	return false
}

// SelectedIDs returns a copy of the selection (empty slice when not narrowed).
func (s Scope) SelectedIDs() []string {
	if !s.Present {
		return nil
	}
	return append([]string(nil), s.Selected...)
}

// RequireSelection reports an error when a tool needs an explicit knowledge-base
// selection but the caller sent an empty one. It is separate from "header
// absent" so legacy machine clients that never send the header are unaffected.
func (s Scope) RequireSelection(toolName string) error {
	if s.Present && len(s.Selected) == 0 {
		return &Error{
			Code: CodeKBSelectionRequired,
			Message: fmt.Sprintf(
				"tool %q needs an explicit knowledge base selection", toolName),
		}
	}
	return nil
}

// uuidPattern accepts the canonical 8-4-4-4-12 form.
var uuidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`,
)

// Parse validates a raw header value into a frozen scope.
//
// It is only called when the header exists: an empty or whitespace-only value is
// an error, because an empty selection must NOT be read as "unrestricted".
func Parse(raw string) (Scope, error) {
	if strings.TrimSpace(raw) == "" {
		return Scope{}, &Error{
			Code: CodeKBSelectionRequired,
			Message: "the knowledge base selection header is present but empty; " +
				"select at least one knowledge base",
		}
	}
	seen := map[string]struct{}{}
	ids := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if !uuidPattern.MatchString(id) {
			return Scope{}, &Error{
				Code:    CodeInvalidArgument,
				Message: fmt.Sprintf("knowledge base id %q is not a UUID", id),
			}
		}
		canonical := strings.ToLower(id)
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		ids = append(ids, canonical)
	}
	if len(ids) == 0 {
		return Scope{}, &Error{
			Code:    CodeKBSelectionRequired,
			Message: "the knowledge base selection header contains no usable id",
		}
	}
	if len(ids) > MaxRequestKnowledgeBases {
		return Scope{}, &Error{
			Code: CodeInvalidArgument,
			Message: fmt.Sprintf("at most %d knowledge bases may be selected per request",
				MaxRequestKnowledgeBases),
		}
	}
	// Deterministic order: the version must not depend on header order.
	sort.Strings(ids)
	return Scope{Selected: ids, Present: true, Version: versionOf(ids)}, nil
}

// versionOf derives a short, stable version string for a selection.
func versionOf(ids []string) string {
	if len(ids) == 0 {
		return "all"
	}
	sum := sha256.Sum256([]byte(strings.Join(ids, ",")))
	return hex.EncodeToString(sum[:])[:16]
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// Error is a knowledge-tool failure with a stable machine-readable code.
//
// The plan requires business failures to arrive as MCP isError=true WITH a
// structuredContent.error carrying {code, message, retryable}, so the Python
// client branches on the code instead of guessing from English text.
type Error struct {
	Code      string
	Message   string
	Retryable bool
	// Details is optional structured context (missing ids, current state, ...).
	Details map[string]any
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// RetryableNow reports whether a retry could reasonably succeed.
func (e *Error) RetryableNow() bool { return e != nil && e.Retryable }

// Structured error codes shared by the WeKnora tools and the Python client.
const (
	CodeUnauthorized        = "UNAUTHORIZED"
	CodeForbidden           = "FORBIDDEN"
	CodeKBSelectionRequired = "KB_SELECTION_REQUIRED"
	CodeKBOutOfScope        = "KB_OUT_OF_SCOPE"
	CodeUploadNotFound      = "UPLOAD_NOT_FOUND"
	CodeUploadExpired       = "UPLOAD_EXPIRED"
	CodeImportInProgress    = "IMPORT_IN_PROGRESS"
	CodeInvalidFileType     = "INVALID_FILE_TYPE"
	CodeFileTooLarge        = "FILE_TOO_LARGE"
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	CodeUpstreamUnavailable = "UPSTREAM_UNAVAILABLE"
	CodeRateLimited         = "RATE_LIMITED"
	CodeInvalidArgument     = "INVALID_ARGUMENT"
	CodeNotFound            = "NOT_FOUND"
	CodeInternal            = "INTERNAL_ERROR"
	CodeProcessingError     = "PROCESSING_ERROR"
	CodeUpstreamToolError   = "UPSTREAM_TOOL_ERROR"
	CodeGraphUnavailable    = "GRAPH_UNAVAILABLE"
	CodeGraphNotConfigured  = "GRAPH_EXTRACTION_NOT_CONFIGURED"
	CodeWikiNotEnabled      = "WIKI_NOT_ENABLED"
	CodeUploadLeaseConflict = "UPLOAD_LEASE_CONFLICT"
	CodeEnqueueFailed       = "ENQUEUE_FAILED"
)

// Errorf builds an Error with a formatted message.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// InvalidArgument reports a caller mistake in the tool arguments.
func InvalidArgument(format string, args ...any) *Error {
	return Errorf(CodeInvalidArgument, format, args...)
}

// Forbidden reports an authorization refusal.
func Forbidden(format string, args ...any) *Error {
	return Errorf(CodeForbidden, format, args...)
}

// NotFound reports a missing resource.
func NotFound(format string, args ...any) *Error {
	return Errorf(CodeNotFound, format, args...)
}

// OutOfScope reports that a named resource is outside the frozen selection or
// the endpoint range.
func OutOfScope(format string, args ...any) *Error {
	return Errorf(CodeKBOutOfScope, format, args...)
}

// AsError extracts an *Error from err, synthesising an upstream-tool-error when
// the failure came from somewhere else.
func AsError(err error) *Error {
	var toolErr *Error
	if errors.As(err, &toolErr) {
		return toolErr
	}
	return &Error{Code: CodeUpstreamToolError, Message: err.Error()}
}
