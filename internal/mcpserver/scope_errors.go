package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Tencent/WeKnora/internal/mcpscope"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
)

// Type aliases keep the tool code readable while the primitives live in the
// leaf mcpscope package (shared with the middleware that parses the header
// before the MCP handler runs).
type (
	// RequestScope is the caller's frozen knowledge-base selection.
	RequestScope = mcpscope.Scope
	// ToolError is a knowledge-tool failure with a stable code.
	ToolError = mcpscope.Error
)

// Structured error codes re-exported for the tool handlers.
const (
	CodeUnauthorized        = mcpscope.CodeUnauthorized
	CodeForbidden           = mcpscope.CodeForbidden
	CodeKBSelectionRequired = mcpscope.CodeKBSelectionRequired
	CodeKBOutOfScope        = mcpscope.CodeKBOutOfScope
	CodeUploadNotFound      = mcpscope.CodeUploadNotFound
	CodeUploadExpired       = mcpscope.CodeUploadExpired
	CodeImportInProgress    = mcpscope.CodeImportInProgress
	CodeInvalidFileType     = mcpscope.CodeInvalidFileType
	CodeFileTooLarge        = mcpscope.CodeFileTooLarge
	CodeIdempotencyConflict = mcpscope.CodeIdempotencyConflict
	CodeUpstreamUnavailable = mcpscope.CodeUpstreamUnavailable
	CodeRateLimited         = mcpscope.CodeRateLimited
	CodeInvalidArgument     = mcpscope.CodeInvalidArgument
	CodeNotFound            = mcpscope.CodeNotFound
	CodeInternal            = mcpscope.CodeInternal
	CodeProcessingError     = mcpscope.CodeProcessingError
	CodeUpstreamToolError   = mcpscope.CodeUpstreamToolError
	CodeGraphUnavailable    = mcpscope.CodeGraphUnavailable
	CodeGraphNotConfigured  = mcpscope.CodeGraphNotConfigured
	CodeWikiNotEnabled      = mcpscope.CodeWikiNotEnabled
	CodeUploadLeaseConflict = mcpscope.CodeUploadLeaseConflict
	CodeEnqueueFailed       = mcpscope.CodeEnqueueFailed

	// KnowledgeBaseScopeHeader is the optional selection header.
	KnowledgeBaseScopeHeader = mcpscope.KnowledgeBaseScopeHeader
	// MaxRequestKnowledgeBases bounds one request's selection.
	MaxRequestKnowledgeBases = mcpscope.MaxRequestKnowledgeBases
)

// Errorf, InvalidArgument, Forbidden, NotFound and OutOfScope are thin wrappers
// so handlers read naturally without importing the leaf package everywhere.
func Errorf(code, format string, args ...any) *ToolError {
	return mcpscope.Errorf(code, format, args...)
}

func invalidArgument(format string, args ...any) *ToolError {
	return mcpscope.InvalidArgument(format, args...)
}

func forbidden(format string, args ...any) *ToolError {
	return mcpscope.Forbidden(format, args...)
}

func notFound(format string, args ...any) *ToolError {
	return mcpscope.NotFound(format, args...)
}

func outOfScope(format string, args ...any) *ToolError {
	return mcpscope.OutOfScope(format, args...)
}

// AsError extracts a ToolError, synthesising an upstream-tool-error when the
// failure came from somewhere else.
func AsError(err error) *ToolError {
	return mcpscope.AsError(err)
}

// writeJSONBody encodes a JSON response body.
func writeJSONBody(w io.Writer, body any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(body)
}

// RequestScopeOf returns the caller's frozen knowledge-base selection. An absent
// value yields the "header not sent" scope, which does not narrow anything.
func RequestScopeOf(ctx context.Context) RequestScope {
	return mcpscope.Of(ctx)
}

// StagingEnvVar overrides the upload staging root inside the WeKnora process.
// The directory belongs to WeKnora: it writes the bytes itself and cleans them
// up once the import receipt is persisted.
const StagingEnvVar = "WEKNORA_MCP_STAGING_ROOT"

// DefaultStagingRoot is used when StagingEnvVar is unset. It is relative so it
// resolves inside the process working directory, matching the other local
// storage defaults.
const DefaultStagingRoot = "data/mcp-staging"

// errorResult renders a ToolError as an MCP tool result carrying the code in
// structuredContent so clients do not parse English text.
func errorResult(err error) *mcp.CallToolResult {
	toolErr := mcpscope.AsError(err)
	payload := map[string]any{
		"code":      toolErr.Code,
		"message":   toolErr.Message,
		"retryable": toolErr.Retryable,
	}
	if len(toolErr.Details) > 0 {
		payload["details"] = toolErr.Details
	}
	result := mcp.NewToolResultError(toolErr.Message)
	result.StructuredContent = map[string]any{"error": payload}
	return result
}

// applySelection narrows allowed to the caller's selection.
//
// Every selected id must exist in allowed: an unknown or out-of-scope id is
// refused outright rather than silently dropped, because dropping it would
// quietly answer from a different range than the caller asked for.
func applySelection(allowed []*types.KnowledgeBase, scope RequestScope) ([]*types.KnowledgeBase, error) {
	if !scope.Narrows() {
		return allowed, nil
	}
	if len(allowed) == 0 {
		return nil, &ToolError{
			Code: CodeKBOutOfScope,
			Message: "none of the selected knowledge bases is available to this caller " +
				"on this endpoint",
		}
	}
	byID := make(map[string]*types.KnowledgeBase, len(allowed))
	for _, kb := range allowed {
		byID[strings.ToLower(kb.ID)] = kb
	}
	out := make([]*types.KnowledgeBase, 0, len(scope.Selected))
	missing := make([]string, 0)
	for _, id := range scope.Selected {
		kb, ok := byID[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		out = append(out, kb)
	}
	if len(missing) > 0 {
		return nil, &ToolError{
			Code: CodeKBOutOfScope,
			Message: fmt.Sprintf(
				"knowledge base(s) %s are outside this caller's authorized range on this endpoint",
				strings.Join(missing, ", "),
			),
			Details: map[string]any{"outside_scope": missing},
		}
	}
	return out, nil
}
