package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/application/access"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// This file implements the binary upload transport:
//
//	POST /mcp/:endpoint_id/uploads
//	Content-Type: multipart/form-data
//	fields: knowledge_base_id, file
//	headers: the same three the MCP surface uses
//
// It is the binary channel of the same integration service. It ONLY stages a
// file: it never creates a document, never starts parsing and never returns a
// document id. The import stays a MCP tool call, so there is exactly one code
// path that creates knowledge.
//
// Every path component, hash, size, expiry and ownership check lives here or in
// the staging service; the thin Python adapter forwards bytes and nothing else.

const (
	// uploadFormMemory bounds how much of the multipart envelope stays in RAM
	// before spilling to disk.
	uploadFormMemory = 1 << 20
	// uploadEnvelopeSlack is the fixed room the multipart boundaries and the
	// knowledge_base_id field need on top of the file limit. Mirrors the
	// existing HTTP upload handler's constant.
	uploadEnvelopeSlack = 1 << 20
	// uploadFormFieldName matches the existing HTTP upload handler's field name.
	uploadFormFieldName = "file"
	// idempotencyHeader carries the one-per-upload-action key.
	idempotencyHeader = "Idempotency-Key"
)

// maxUploadBytes is the effective size ceiling, resolved from MAX_FILE_SIZE_MB
// through the same helper the existing upload handler uses.
func (s *Server) maxUploadBytes() int64 {
	return s.fileSizeLimit()
}

// HandleUpload serves POST /mcp/:endpoint_id/uploads.
//
// The caller must run middleware.MCPEndpointAuth first: this handler trusts the
// identity on the request context, which is what makes the staging directory a
// function of verified identity rather than of client input.
func (s *Server) HandleUpload() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ep, err := endpointFromContext(ctx)
		if err != nil {
			writeUploadError(w, http.StatusUnauthorized, &ToolError{
				Code:    CodeUnauthorized,
				Message: "unauthorized",
			})
			return
		}
		// The upload endpoint stages bytes that only import_document_file can
		// consume, so an endpoint without that tool must not accept uploads.
		if !ep.HasTool(types.MCPEndpointToolImportDocumentFile) {
			writeUploadError(w, http.StatusForbidden, &ToolError{
				Code: CodeForbidden,
				Message: "this endpoint does not enable " +
					types.MCPEndpointToolImportDocumentFile + ", so it cannot accept uploads",
			})
			return
		}
		staging := s.uploadStaging()
		if staging == nil {
			writeUploadError(w, http.StatusServiceUnavailable, &ToolError{
				Code:    CodeUpstreamUnavailable,
				Message: "upload staging is not configured on this deployment",
			})
			return
		}

		callerWorkspaceID, ok := types.TenantIDFromContext(ctx)
		if !ok || callerWorkspaceID == 0 {
			writeUploadError(w, http.StatusForbidden, &ToolError{
				Code:    CodeForbidden,
				Message: "the request carries no workspace, so an upload cannot be attributed",
			})
			return
		}
		principal, ok := types.PrincipalFromContext(ctx)
		if !ok || principal.StorageID() == "" {
			writeUploadError(w, http.StatusForbidden, &ToolError{
				Code: CodeForbidden,
				Message: "the request carries no verified caller identity, so an upload " +
					"cannot be attributed",
			})
			return
		}

		// Enforce the size ceiling BEFORE the multipart parser buffers anything.
		maxBytes := s.maxUploadBytes()
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes+uploadEnvelopeSlack)

		reader, err := r.MultipartReader()
		if err != nil {
			writeUploadError(w, http.StatusBadRequest, &ToolError{
				Code:    CodeInvalidArgument,
				Message: "the request must be multipart/form-data",
			})
			return
		}

		// The window between "we know the knowledge base" and "we have the
		// streamed bytes" is handled by streaming the file part directly: the
		// knowledge_base_id field is required to appear before the file part, so
		// the caller's authorization is decided before any byte is stored.
		request := UploadRequest{
			EndpointID:        ep.ID,
			CallerWorkspaceID: callerWorkspaceID,
			CallerStorageID:   principal.StorageID(),
			IdempotencyKey:    strings.TrimSpace(r.Header.Get(idempotencyHeader)),
			MaxBytes:          maxBytes,
		}
		if request.IdempotencyKey != "" && !uploadIDPattern.MatchString(request.IdempotencyKey) {
			writeUploadError(w, http.StatusBadRequest, &ToolError{
				Code:    CodeInvalidArgument,
				Message: idempotencyHeader + " must be a UUID",
			})
			return
		}

		var kb *types.KnowledgeBase
		var fileHandled bool
		for {
			part, partErr := reader.NextPart()
			if errors.Is(partErr, io.EOF) {
				break
			}
			if partErr != nil {
				if isBodyTooLarge(partErr) {
					writeUploadError(w, http.StatusRequestEntityTooLarge, &ToolError{
						Code: CodeFileTooLarge,
						Message: fmt.Sprintf("the upload is larger than the %d MB limit",
							maxBytes>>20),
					})
					return
				}
				writeUploadError(w, http.StatusBadRequest, &ToolError{
					Code:    CodeInvalidArgument,
					Message: "the multipart body could not be read: " + partErr.Error(),
				})
				return
			}

			switch part.FormName() {
			case "knowledge_base_id":
				raw, readErr := io.ReadAll(io.LimitReader(part, 4096))
				_ = part.Close()
				if readErr != nil {
					writeUploadError(w, http.StatusBadRequest, &ToolError{
						Code:    CodeInvalidArgument,
						Message: "knowledge_base_id could not be read",
					})
					return
				}
				selector := strings.TrimSpace(string(raw))
				if selector == "" {
					writeUploadError(w, http.StatusBadRequest, &ToolError{
						Code:    CodeInvalidArgument,
						Message: "knowledge_base_id is required",
					})
					return
				}
				if fileHandled {
					// The plan fixes the field order; refuse rather than guess.
					writeUploadError(w, http.StatusBadRequest, &ToolError{
						Code: CodeInvalidArgument,
						Message: "knowledge_base_id must be sent before the file part so the " +
							"caller is authorized before any byte is stored",
					})
					return
				}
				resolved, kbErr := s.authorizeUploadTarget(ctx, ep, selector)
				if kbErr != nil {
					writeUploadToolError(w, kbErr)
					return
				}
				kb = resolved
				request.KnowledgeBaseID = resolved.ID
				request.OwnerTenantID = resolved.TenantID
			case uploadFormFieldName:
				if kb == nil {
					writeUploadError(w, http.StatusBadRequest, &ToolError{
						Code: CodeInvalidArgument,
						Message: "knowledge_base_id must be sent before the file part so the " +
							"caller is authorized before any byte is stored",
					})
					return
				}
				request.FileName = part.FileName()
				record, existing, stageErr := staging.Stage(ctx, request, part)
				_ = part.Close()
				fileHandled = true
				if stageErr != nil {
					writeUploadToolError(w, stageErr)
					return
				}
				status := http.StatusCreated
				if existing {
					status = http.StatusOK
				}
				logger.Infof(ctx,
					"[mcp-upload] staged %s (%d bytes) for kb=%s caller_ws=%d endpoint=%s replay=%v",
					record.UploadID, record.FileSize, record.KnowledgeBaseID,
					record.CallerWorkspaceID, record.EndpointID, existing)
				writeUploadJSON(w, status, map[string]any{
					"success": true,
					"data": map[string]any{
						"upload_id":         record.UploadID,
						"knowledge_base_id": record.KnowledgeBaseID,
						"file_name":         record.FileName,
						"file_size":         record.FileSize,
						"sha256":            record.SHA256,
						"expires_at":        record.ExpiresAt.UTC().Format(time.RFC3339),
					},
				})
				return
			default:
				// Unknown fields are drained so the next part can be read.
				_, _ = io.Copy(io.Discard, io.LimitReader(part, 4096))
				_ = part.Close()
			}
		}
		writeUploadError(w, http.StatusBadRequest, &ToolError{
			Code:    CodeInvalidArgument,
			Message: "the request carried no file part",
		})
	})
}

// authorizeUploadTarget resolves the knowledge base and applies the full
// authorization chain BEFORE any byte is stored:
//
//	request selection ∩ endpoint range ∩ user authorization ∩ write role
//
// It mirrors the existing upload route's rule (owner or admin/editor), which is
// why it goes through scopedKBContext with OrgRoleEditor rather than only
// checking access.RequireKBWrite afterwards.
func (s *Server) authorizeUploadTarget(
	ctx context.Context, ep *types.MCPEndpoint, selector string,
) (*types.KnowledgeBase, error) {
	kbs, err := s.selectKnowledgeBases(ctx, ep, []string{selector})
	if err != nil {
		return nil, err
	}
	kb := kbs[0]
	// User-mode write gate before any grant is minted: the REST file route uses
	// OwnedKBOrAdmin, and the MCP surface must apply the same creator-or-admin
	// rule rather than letting ResolveKB grant same-tenant Admin to any member.
	if err := s.requireKBWriteOwnership(ctx, kb); err != nil {
		return nil, err
	}
	scoped, err := s.scopedKBContext(ctx, kb, types.OrgRoleEditor)
	if err != nil {
		return nil, err
	}
	if err := access.RequireKBWrite(scoped, kb); err != nil {
		return nil, forbidden("this caller is not allowed to write to knowledge base %s", kb.ID)
	}
	return kb, nil
}

func isBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

func writeUploadJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = writeJSONBody(w, body)
}

func writeUploadError(w http.ResponseWriter, status int, err *ToolError) {
	writeUploadJSON(w, status, map[string]any{
		"success": false,
		"error": map[string]any{
			"code":      err.Code,
			"message":   err.Message,
			"retryable": err.Retryable,
		},
	})
}

// writeUploadToolError maps a tool error onto the HTTP status the plan fixes:
// input 400, resource 404/410, conflict 409, size 413, rate limit 429, service
// 502/503.
func writeUploadToolError(w http.ResponseWriter, err error) {
	toolErr := AsError(err)
	status := http.StatusBadRequest
	switch toolErr.Code {
	case CodeUnauthorized:
		status = http.StatusUnauthorized
	case CodeForbidden:
		status = http.StatusForbidden
	case CodeKBOutOfScope, CodeImportInProgress, CodeIdempotencyConflict, CodeUploadLeaseConflict:
		status = http.StatusConflict
	case CodeUploadNotFound:
		status = http.StatusNotFound
	case CodeUploadExpired:
		status = http.StatusGone
	case CodeFileTooLarge:
		status = http.StatusRequestEntityTooLarge
	case CodeRateLimited:
		status = http.StatusTooManyRequests
	case CodeUpstreamUnavailable:
		status = http.StatusServiceUnavailable
	case CodeInvalidFileType, CodeInvalidArgument, CodeKBSelectionRequired:
		status = http.StatusBadRequest
	default:
		status = http.StatusInternalServerError
	}
	writeUploadError(w, status, toolErr)
}
