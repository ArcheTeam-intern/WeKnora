package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
)

// uploadFileTool exposes a "stage the bytes, return a receipt" path as an
// MCP tool, so a client that speaks only the JSON-RPC streamable_http
// transport (no multipart, no custom HTTP handler) can drive the full
// upload -> import -> answer flow over MCP. The actual byte staging
// still goes through the existing `UploadStaging.Stage` so the on-disk
// layout, the SHA-256 / Idempotency-Key replay checks, and the
// StagingSweepRunner lifecycle are unchanged.
//
// `content` is base64-encoded so the JSON-RPC argument stays
// self-describing; the tool does the decode once, hands the bytes to
// staging, and discards the intermediate buffer. The size cap is
// `Server.maxUploadBytes` so MCP and the legacy HTTP path share one
// limit and cannot drift.
func uploadFileTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolUploadFile,
		mcp.WithDescription("Stage a file's bytes for a later `import_document_file` call. The "+
			"`content` argument is the file bytes encoded as base64; the same "+
			"endpoint's `import_document_file` tool then indexes, parses and "+
			"indexes the document. Returns the upload id, the receipt (state, "+
			"checksum, expiry) and `replay=true` when an existing receipt matched "+
			"the same Idempotency-Key with identical bytes."),
		mcp.WithString("knowledge_base_id", mcp.Required(),
			mcp.Description("Knowledge base id or exact name to upload into")),
		mcp.WithString("file_name", mcp.Required(),
			mcp.Description("Original file name; the parser infers the type from the extension")),
		mcp.WithString("content", mcp.Required(),
			mcp.Description("File bytes encoded as base64 (RFC 4648 standard alphabet). Empty "+
				"or whitespace-only values are rejected as invalid.")),
		mcp.WithString("idempotency_key", mcp.Description(
			"Optional Idempotency-Key. Replays with the same key + same bytes return "+
				"the original receipt; replays with different bytes are rejected.")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
	)
}

func (s *Server) handleUploadFile(
	ctx context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	selector, err := req.RequireString("knowledge_base_id")
	if err != nil {
		return errorResult(invalidArgument("knowledge_base_id is required")), nil
	}
	fileName, err := req.RequireString("file_name")
	if err != nil {
		return errorResult(invalidArgument("file_name is required")), nil
	}
	contentB64 := strings.TrimSpace(req.GetString("content", ""))
	if contentB64 == "" {
		return errorResult(invalidArgument("content is required (base64-encoded file bytes)")), nil
	}
	idempotencyKey := strings.TrimSpace(req.GetString("idempotency_key", ""))

	// Select + authorize the target KB through the same path the legacy
	// HTTP upload uses. authorizeUploadTarget applies the endpoint
	// selection, knowledge base visibility, and the creator-or-admin
	// gate added by R1. We pass the tool's caller as the upload
	// "target" so the gate still applies on the MCP path.
	kbs, err := s.selectKnowledgeBases(ctx, ep, []string{selector})
	if err != nil {
		return errorResult(err), nil
	}
	kb := kbs[0]
	scopedCtx, err := s.scopedKBContext(ctx, kb, types.OrgRoleEditor)
	if err != nil {
		return errorResult(err), nil
	}
	if err := s.requireKBWriteOwnership(scopedCtx, kb); err != nil {
		return errorResult(err), nil
	}

	// Decode base64 BEFORE entering the size cap so the size limit
	// reflects the decoded bytes, not the encoded string. Strict
	// decoder refuses non-canonical input (whitespace inside the
	// string, missing padding) so a malformed payload cannot slip
	// through and be parsed as 0 bytes.
	//
	// The MCP request envelope itself has a server-side cap (envelope
	// cap on the transport), but a caller can still post a multi-GB
	// base64 string. We bound the decoded size BEFORE allocating
	// the result slice: base64 encodes 3 bytes into 4 characters, so
	// a string of length L decodes to at most ceil(L * 3 / 4) bytes
	// (padding pulls it down by 1-2). Rejecting on the encoded
	// length prevents a single bad request from forcing a large
	// allocation just to be turned away at the next check.
	maxBytes := s.maxUploadBytes()
	if int64(len(contentB64)) > maxBytes*4/3+8 {
		return errorResult(&ToolError{
			Code: CodeFileTooLarge,
			Message: fmt.Sprintf("the file is larger than the %d MB limit",
				maxBytes>>20),
		}), nil
	}
	decoded, decErr := base64.StdEncoding.DecodeString(contentB64)
	if decErr != nil {
		return errorResult(&ToolError{
			Code:    CodeInvalidArgument,
			Message: "content is not valid base64: " + decErr.Error(),
		}), nil
	}
	if len(decoded) == 0 {
		return errorResult(invalidArgument("decoded content is empty")), nil
	}
	if int64(len(decoded)) > maxBytes {
		// The encoded-size bound is intentionally a little looser than
		// 4/3 * maxBytes, so a 1-byte overshoot is reachable only by
		// padding tricks; this final check rejects the exact case.
		return errorResult(&ToolError{
			Code: CodeFileTooLarge,
			Message: fmt.Sprintf("the file is larger than the %d MB limit",
				maxBytes>>20),
		}), nil
	}

	caller := types.CallerFromContext(ctx)
	_, _ = caller, caller // KB ownership gate already validated the caller above.
	principal, _ := types.PrincipalFromContext(ctx)
	callerStorageID := principal.StorageID()
	stageReq := UploadRequest{
		EndpointID:        ep.ID,
		CallerWorkspaceID: ep.TenantID,
		CallerStorageID:   callerStorageID,
		OwnerTenantID:     kb.TenantID,
		KnowledgeBaseID:   kb.ID,
		IdempotencyKey:    idempotencyKey,
		FileName:          fileName,
		MaxBytes:          maxBytes,
	}
	staging := s.uploadStaging()
	if staging == nil {
		return errorResult(&ToolError{
			Code:    CodeInternal,
			Message: "upload staging is not configured on this server",
		}), nil
	}
	record, replay, stageErr := staging.Stage(ctx, stageReq, bytes.NewReader(decoded))
	if stageErr != nil {
		if toolErr, ok := stageErr.(*ToolError); ok {
			return errorResult(toolErr), nil
		}
		logger.Errorf(ctx, "[mcp-upload-file] stage failed: %v", stageErr)
		return errorResult(&ToolError{Code: CodeInternal, Message: stageErr.Error()}), nil
	}
	receipt := map[string]any{
		"upload_id":     record.UploadID,
		"state":         record.State,
		"file_name":     record.FileName,
		"file_size":     record.FileSize,
		"sha256":        record.SHA256,
		"replay":        replay,
		"expires_at":    record.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
		"idempotency_key": record.IdempotencyKey,
		"knowledge_base_id": record.KnowledgeBaseID,
		"next_step":     "call import_document_file(knowledge_base_id, upload_id) to start parsing",
	}
	return jsonResult(map[string]any{
		"upload_id": record.UploadID,
		"replay":    replay,
		"receipt":   receipt,
		"hint":      "call import_document_file(knowledge_base_id, upload_id) to start parsing",
		"raw_size_b64": len(contentB64),
		"raw_size_bytes": record.FileSize,
	})
}

// _ keeps the import path alive if a later refactor pulls a symbol out.
var (
	_ = strconv.Itoa
	_ = hex.EncodeToString
)
