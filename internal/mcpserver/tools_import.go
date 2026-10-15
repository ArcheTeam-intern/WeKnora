package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/application/access"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
)

// This file holds the MCP half of the upload contract:
//
//  1. import_document_file(knowledge_base_id, upload_id) — imports a file that
//     WeKnora already staged through POST /mcp/:endpoint_id/uploads. File bytes
//     are NEVER accepted here; this tool only names an upload WeKnora owns.
//  2. get_upload_status(upload_id) — the recovery path for a page or upload flow
//     whose response was lost. It is a page-facing tool and is deliberately NOT
//     in the ReAct tool whitelist (see the Python client's allowlist).
//
// The import order is fixed by the plan, and is the reason this handler is
// structured the way it is:
//
//	1. capture the authenticated ORIGINAL caller workspace / principal / endpoint
//	2. look the upload up AS THE ORIGINAL CALLER, verifying endpoint, the
//	   immutable target knowledge base, expiry, size and hash
//	3. re-authorize write access to the target knowledge base
//	4. atomically claim the upload (staged -> importing) with a lease token
//	5. only then switch to the resource OWNER context and call the existing
//	   file import service
//	6. persist the imported receipt before the payload is deleted, using the
//	   ORIGINAL caller identity rather than the owner's
//
// Step 5 is what makes shared knowledge bases work; steps 1-4 and 6 are what keep
// a shared knowledge base from turning the owner into the uploader.

// importFileHeaderField mirrors the HTTP upload form field name, so the service
// path sees exactly what it sees for a browser upload.
const importFileHeaderField = "file"

// uploadLeaseTTL bounds one import attempt. The importer renews while working; a
// lease older than this is recoverable after a crash.
const uploadLeaseTTL = 120 * time.Second

// uploadLeaseRenewEvery is how often a running import extends its lease. It must
// stay well under uploadLeaseTTL so a slow import does not lose its claim.
const uploadLeaseRenewEvery = 15 * time.Second

func importDocumentFileTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolImportDocumentFile,
		mcp.WithDescription("Import a file that was previously uploaded to this workspace's upload endpoint "+
			"into a knowledge base. Only the upload id returned by that endpoint is accepted: the bytes live "+
			"inside WeKnora, never in this request. Parsing, indexing, wiki and graph generation then run "+
			"exactly as they do for a browser upload. Returns the document id, whether an identical document "+
			"already existed, and whether the indexing task was queued."),
		mcp.WithString("knowledge_base_id", mcp.Required(), mcp.Description("Knowledge base id or exact name")),
		mcp.WithString("upload_id", mcp.Required(),
			mcp.Description("Upload id returned by POST /mcp/{endpoint_id}/uploads")),
		mcp.WithObject("process_config",
			mcp.Description("Optional parse configuration for THIS import. The only supported key is "+
				"parser_engine_rules: a list of {file_types: [\"pdf\"], engine: \"<name from list_parser_engines>\"}. "+
				"Omit to use the knowledge base's own parser configuration. Engine names are validated "+
				"against the engines available to this workspace; unknown or unavailable engines are refused "+
				"instead of silently falling back.")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
	)
}

func getUploadStatusTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolGetUploadStatus,
		mcp.WithDescription("Report the state of one upload: whether its bytes are still staged, whether the "+
			"import already created a document, and the last error. Use it to recover after a timeout instead "+
			"of uploading and importing the same file again."),
		mcp.WithString("upload_id", mcp.Required(), mcp.Description("Upload id to look up")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

// parseImportProcessConfig validates the optional process_config argument and
// turns it into KnowledgeProcessOverrides.
//
// The MCP surface deliberately exposes exactly one knob — parser_engine_rules —
// and validates it against the engines this deployment actually offers for the
// calling workspace. Arbitrary config keys, vendor endpoints, cloud keys and
// deployment-widening switches stay unreachable; an unknown or unavailable
// engine is refused up front so the caller never believes a parser ran when the
// knowledge base default was used instead.
func (s *Server) parseImportProcessConfig(
	ctx context.Context, req mcp.CallToolRequest,
) (*types.KnowledgeProcessOverrides, error) {
	raw, ok := req.GetArguments()["process_config"]
	if !ok || raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, invalidArgument("process_config cannot be read")
	}
	var parsed struct {
		ParserEngineRules []struct {
			FileTypes []string `json:"file_types"`
			Engine    string   `json:"engine"`
		} `json:"parser_engine_rules"`
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		return nil, invalidArgument(
			"process_config only supports parser_engine_rules entries of {file_types, engine}: %v", err)
	}
	if len(parsed.ParserEngineRules) == 0 {
		return nil, invalidArgument("process_config must contain at least one parser_engine_rules entry")
	}

	engines, _ := s.listParserEngines(ctx)
	byName := make(map[string]types.ParserEngineInfo, len(engines))
	available := make([]string, 0, len(engines))
	for _, e := range engines {
		byName[strings.ToLower(e.Name)] = e
		if e.Available {
			available = append(available, e.Name)
		}
	}
	sort.Strings(available)

	rules := make([]types.ParserEngineRule, 0, len(parsed.ParserEngineRules))
	for _, rule := range parsed.ParserEngineRules {
		engine := strings.TrimSpace(rule.Engine)
		info, known := byName[strings.ToLower(engine)]
		if !known || !info.Available {
			return nil, invalidArgument(
				"parser engine %q is not available on this deployment; available engines: %s",
				engine, strings.Join(available, ", "))
		}
		fileTypes := make([]string, 0, len(rule.FileTypes))
		seen := map[string]bool{}
		for _, ft := range rule.FileTypes {
			ft = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(ft, ".")))
			if ft == "" || seen[ft] {
				continue
			}
			seen[ft] = true
			fileTypes = append(fileTypes, ft)
		}
		if len(fileTypes) == 0 {
			return nil, invalidArgument("parser_engine_rules entry for engine %q needs file_types", engine)
		}
		if len(info.FileTypes) > 0 {
			supported := make(map[string]bool, len(info.FileTypes))
			for _, ft := range info.FileTypes {
				supported[strings.ToLower(strings.TrimPrefix(ft, "."))] = true
			}
			for _, ft := range fileTypes {
				if !supported[ft] {
					return nil, invalidArgument(
						"engine %q cannot parse %q; supported: %s",
						engine, ft, strings.Join(info.FileTypes, ", "))
				}
			}
		}
		rules = append(rules, types.ParserEngineRule{FileTypes: fileTypes, Engine: engine})
	}
	return &types.KnowledgeProcessOverrides{ParserEngineRules: rules}, nil
}

func (s *Server) handleImportDocumentFile(
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
	uploadID := strings.TrimSpace(req.GetString("upload_id", ""))
	if uploadID == "" {
		return errorResult(invalidArgument("upload_id is required")), nil
	}
	if !uploadIDPattern.MatchString(uploadID) {
		return errorResult(invalidArgument("upload_id must be a UUID")), nil
	}
	// Optional parser choice: validated before any state changes so a bad engine
	// name fails the call instead of silently parsing with the KB default.
	overrides, err := s.parseImportProcessConfig(ctx, req)
	if err != nil {
		return errorResult(err), nil
	}
	staging := s.uploadStaging()
	if staging == nil {
		return errorResult(&ToolError{
			Code:    CodeUpstreamUnavailable,
			Message: "upload staging is not configured on this deployment",
		}), nil
	}

	// Step 1: capture the ORIGINAL caller identity before anything switches
	// context. Every receipt operation below uses these values.
	callerWorkspaceID, ok := types.TenantIDFromContext(ctx)
	if !ok || callerWorkspaceID == 0 {
		return errorResult(forbidden(
			"the request carries no workspace, so a staged upload cannot be attributed")), nil
	}
	principal, ok := types.PrincipalFromContext(ctx)
	if !ok || principal.StorageID() == "" {
		return errorResult(forbidden(
			"the request carries no verified caller identity, so a staged upload cannot be attributed")), nil
	}
	lookup := uploadLookup{
		UploadID:          uploadID,
		EndpointID:        ep.ID,
		CallerWorkspaceID: callerWorkspaceID,
		CallerStorageID:   principal.StorageID(),
	}

	// Step 2: resolve the upload as the original caller and freeze its target.
	upload, err := staging.Get(ctx, lookup)
	if err != nil {
		return errorResult(err), nil
	}
	scope := RequestScopeOf(ctx)
	if scope.Narrows() && !scope.Contains(upload.KnowledgeBaseID) {
		return errorResult(outOfScope(
			"upload %s targets knowledge base %s, which is outside the selected range for this request",
			uploadID, upload.KnowledgeBaseID)), nil
	}
	kbs, err := s.selectKnowledgeBases(ctx, ep, []string{selector})
	if err != nil {
		return errorResult(err), nil
	}
	kb := kbs[0]
	if !strings.EqualFold(kb.ID, upload.KnowledgeBaseID) {
		// The target knowledge base is immutable once bytes are staged: a
		// redirect would let one upload's authorization be reused against
		// another knowledge base.
		return errorResult(&ToolError{
			Code: CodeInvalidArgument,
			Message: fmt.Sprintf(
				"upload %s targets knowledge base %s, not %s; an upload cannot be redirected to another "+
					"knowledge base", uploadID, upload.KnowledgeBaseID, kb.ID),
		}), nil
	}

	// A replayed import returns the stored receipt instead of importing again:
	// this is what makes a lost response recoverable without a duplicate.
	if upload.State == UploadStateImported && upload.DocumentID != "" {
		replay := map[string]any{
			"knowledge_base_id": upload.KnowledgeBaseID,
			"upload_id":         upload.UploadID,
			"document":          map[string]any{"id": upload.DocumentID},
			"deduplicated":      upload.Deduplicated,
			"accepted":          true,
			"queued":            upload.Queued,
			"replayed":          true,
			"note":              "this upload was already imported; the stored receipt is replayed",
		}
		if overrides != nil {
			replay["process_config_applied"] = false
			replay["note"] = "this upload was already imported; the stored receipt is replayed. " +
				"The requested parser configuration was NOT re-applied — reparse the document to change it"
		}
		return jsonResult(replay)
	}

	// Step 3: the write check uses the original caller's own grants. The
	// ownership gate (creator-or-admin) runs before the grant so a Viewer or a
	// non-creator Contributor is refused even though ResolveKB would mint a
	// same-tenant Admin grant for them.
	if err := s.requireKBWriteOwnership(ctx, kb); err != nil {
		return errorResult(err), nil
	}
	scopedCtx, err := s.scopedKBContext(ctx, kb, types.OrgRoleEditor)
	if err != nil {
		return errorResult(err), nil
	}
	if err := access.RequireKBWrite(scopedCtx, kb); err != nil {
		return errorResult(forbidden(
			"this caller is not allowed to write to knowledge base %s", kb.ID)), nil
	}

	// Step 4: claim atomically. Exactly one concurrent import may proceed.
	lease, err := staging.Claim(ctx, lookup, uploadLeaseTTL)
	if err != nil {
		return errorResult(err), nil
	}
	if lease == nil {
		return errorResult(&ToolError{
			Code:      CodeImportInProgress,
			Message:   "this upload is already being imported; poll get_upload_status in about two seconds",
			Retryable: true,
			Details:   map[string]any{"upload_id": uploadID},
		}), nil
	}

	fail := func(cause error, code string) (*mcp.CallToolResult, error) {
		if recordErr := staging.Fail(ctx, lease, code, cause.Error()); recordErr != nil {
			logger.Warnf(ctx, "[mcpserver] could not record the upload failure for %s: %v",
				uploadID, recordErr)
		}
		return errorResult(&ToolError{Code: code, Message: cause.Error()}), nil
	}

	payload, fileName, err := staging.OpenPayload(ctx, lease)
	if err != nil {
		return fail(err, CodeUploadNotFound)
	}
	defer func() { _ = payload.Close() }()

	fileHeader, cleanup, err := fileHeaderFromReader(payload, fileName, upload.FileSize)
	if err != nil {
		return fail(err, CodeInternal)
	}
	defer cleanup()

	// A running import keeps its lease alive so a slow parse cannot let a second
	// attempt take over mid-flight.
	renewCtx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	go renewLeaseUntilDone(renewCtx, staging, lease)

	// Step 5: the import itself runs under the resource OWNER's context.
	created, importErr := s.knowledgeService.CreateKnowledgeFromFile(
		scopedCtx, kb.ID, fileHeader, nil, nil, "", nil, askChannel, overrides,
	)
	duplicate := false
	var dupErr *types.DuplicateKnowledgeError
	if importErr != nil {
		if errors.As(importErr, &dupErr) {
			duplicate = true
			if created == nil {
				created = dupErr.Knowledge
			}
		} else {
			return fail(importErr, classifyImportError(importErr))
		}
	}
	if created == nil {
		return fail(errors.New("the import returned no document"), CodeProcessingError)
	}

	// `queued` reflects the real facts: a knowledge row can exist with a failed
	// enqueue, and that must never be reported as a silent success.
	queued := created.ParseStatus != types.ParseStatusFailed && created.ErrorMessage == ""

	// Step 6: persist the receipt BEFORE the payload is released. A failure
	// here is NOT a full success: the document exists but the upload receipt
	// was not recorded, so a replay later cannot be answered from the receipt.
	// Report it as a retryable error that names the created document — the
	// caller can recover via get_upload_status / get_document_status instead of
	// re-uploading the same bytes.
	if err := staging.Complete(ctx, lease, UploadCompletion{
		DocumentID:   created.ID,
		Deduplicated: duplicate,
		Queued:       queued,
	}); err != nil {
		return errorResult(&ToolError{
			Code:      CodeInternal,
			Message:   fmt.Sprintf("document %s was created but the upload receipt could not be persisted", created.ID),
			Retryable: true,
			Details: map[string]any{
				"knowledge_id": created.ID,
				"upload_id":    uploadID,
			},
		}), nil
	}

	result := map[string]any{
		"knowledge_base_id": kb.ID,
		"upload_id":         uploadID,
		"document":          summarizeKnowledge(created),
		"deduplicated":      duplicate,
		"accepted":          true,
		"queued":            queued,
		"file_name":         fileName,
		"file_size":         upload.FileSize,
		"processing_error":  created.ErrorMessage,
		"note": "The knowledge record exists now; indexing runs asynchronously. " +
			"Use get_document_status to watch it.",
	}
	if !queued {
		result["note"] = "The knowledge record exists but its indexing did not start; the document is not " +
			"searchable until the existing admin UI re-parses it."
	}
	if overrides != nil {
		result["process_config"] = overrides.ParserEngineRules
		result["process_config_applied"] = !duplicate
		if duplicate {
			result["note"] = "An identical document already existed: its existing parse result was reused, " +
				"so the requested parser configuration was NOT re-applied. Reparse the existing document " +
				"to change its parser."
		}
	}
	return jsonResult(result)
}

func (s *Server) handleGetUploadStatus(
	ctx context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	uploadID := strings.TrimSpace(req.GetString("upload_id", ""))
	if uploadID == "" {
		return errorResult(invalidArgument("upload_id is required")), nil
	}
	if !uploadIDPattern.MatchString(uploadID) {
		return errorResult(invalidArgument("upload_id must be a UUID")), nil
	}
	staging := s.uploadStaging()
	if staging == nil {
		return errorResult(&ToolError{
			Code:    CodeUpstreamUnavailable,
			Message: "upload staging is not configured on this deployment",
		}), nil
	}
	callerWorkspaceID, _ := types.TenantIDFromContext(ctx)
	principal, _ := types.PrincipalFromContext(ctx)
	upload, err := staging.Get(ctx, uploadLookup{
		UploadID:          uploadID,
		EndpointID:        ep.ID,
		CallerWorkspaceID: callerWorkspaceID,
		CallerStorageID:   principal.StorageID(),
	})
	if err != nil {
		return errorResult(err), nil
	}
	// Reading an upload is a by-id read and therefore respects the frozen
	// selection: narrowing must not be bypassable through the upload surface.
	if scope := RequestScopeOf(ctx); scope.Narrows() && !scope.Contains(upload.KnowledgeBaseID) {
		return errorResult(outOfScope(
			"upload %s targets a knowledge base outside the selected range", uploadID)), nil
	}
	return jsonResult(map[string]any{
		"upload_id":          upload.UploadID,
		"knowledge_base_id":  upload.KnowledgeBaseID,
		"state":              upload.State,
		"document_id":        upload.DocumentID,
		"deduplicated":       upload.Deduplicated,
		"queued":             upload.Queued,
		"file_name":          upload.FileName,
		"file_size":          upload.FileSize,
		"sha256":             upload.SHA256,
		"expires_at":         upload.ExpiresAt.UTC().Format(time.RFC3339),
		"receipt_expires_at": upload.ReceiptExpiresAt.UTC().Format(time.RFC3339),
		"processing_error":   upload.LastErrorMessage,
		"error_code":         upload.LastErrorCode,
	})
}

// renewLeaseUntilDone keeps an import's lease alive until the context ends.
func renewLeaseUntilDone(ctx context.Context, staging UploadStaging, lease *UploadLease) {
	ticker := time.NewTicker(uploadLeaseRenewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := staging.Renew(ctx, lease); err != nil {
				logger.Warnf(ctx, "[mcpserver] could not renew the import lease for %s: %v",
					lease.Record.UploadID, err)
				return
			}
		}
	}
}

// classifyImportError maps the service layer's error kinds onto tool codes so the
// client does not have to read message text.
func classifyImportError(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "too large"), strings.Contains(message, "文件大小"):
		return CodeFileTooLarge
	case strings.Contains(message, "file type"), strings.Contains(message, "文件类型"),
		strings.Contains(message, "invalid file"):
		return CodeInvalidFileType
	case strings.Contains(message, "quota"):
		return CodeForbidden
	default:
		return CodeProcessingError
	}
}
