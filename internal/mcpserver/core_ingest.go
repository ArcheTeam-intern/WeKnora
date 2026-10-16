package mcpserver

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func coreIngestTool() mcp.Tool {
	return mcp.NewTool(CoreToolIngest,
		mcp.WithDescription("Bring a file into a knowledge base and follow its processing. "+
			"Action submit takes the file bytes and starts parsing; action status reports upload, "+
			"parse, index, wiki and graph phases for an ingest id. Neither action waits for parsing "+
			"or wiki generation to finish."),
		mcp.WithString("action", mcp.Required(),
			mcp.Description("submit | status"), mcp.Enum(CoreActionSubmit, CoreActionStatus)),
		mcp.WithString("knowledge_base_id", mcp.Description("Target library (submit)")),
		mcp.WithObject("file", mcp.Description("submit: {name, encoding:\"base64\", content}. "+
			"The bytes travel as base64 inside this request and never through the model context.")),
		mcp.WithObject("process_config", mcp.Description("submit: parser choice, e.g. "+
			"{\"parser_engine_rules\":[{\"file_types\":[\"pdf\"],\"engine\":\"<engine from "+
			"knowledge_base.get>\"}]}")),
		mcp.WithString("idempotency_key", mcp.Description("submit: identifies one user upload "+
			"operation; repeating it with the same bytes replays the stored receipt")),
		mcp.WithString("ingest_id", mcp.Description("status: the ingest id returned by submit")),
		mcp.WithString("document_id", mcp.Description("status: optionally read the document phase "+
			"directly, when the caller already knows it")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
	)
}

func (s *Server) handleCoreIngest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := coreArgs(req)
	action, err := coreActionOf(CoreToolIngest, args)
	if err != nil {
		return errorResult(err), nil
	}
	switch action {
	case CoreActionSubmit:
		return s.coreIngestSubmit(ctx, args)
	case CoreActionStatus:
		return s.coreIngestStatus(ctx, args)
	default:
		return errorResult(invalidArgument("action %q is not supported by %s", action, CoreToolIngest)), nil
	}
}

// coreIngestSubmit wraps "stage the bytes, persist the receipt, import, enqueue"
// as one call. It reuses the existing upload and import handlers verbatim, so
// the staging lease, the idempotency receipt and the dedup contract stay the
// same ones the legacy tools use.
func (s *Server) coreIngestSubmit(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	kbID, err := requireKnowledgeBaseID(args)
	if err != nil {
		return errorResult(err), nil
	}
	file, ok := args["file"].(map[string]any)
	if !ok {
		return errorResult(invalidArgument("file is required and must be an object")), nil
	}
	name := strings.TrimSpace(stringValue(file["name"]))
	content := stringValue(file["content"])
	encoding := strings.ToLower(strings.TrimSpace(stringValue(file["encoding"])))
	if name == "" {
		return errorResult(invalidArgument("file.name is required")), nil
	}
	if content == "" {
		return errorResult(invalidArgument("file.content is required")), nil
	}
	if encoding != "" && encoding != "base64" {
		return errorResult(invalidArgument(
			"file.encoding %q is not supported; send base64 bytes", encoding)), nil
	}

	uploadArgs := map[string]any{
		"knowledge_base_id": kbID,
		"file_name":         name,
		"content":           content,
	}
	if key := strings.TrimSpace(stringValue(args["idempotency_key"])); key != "" {
		uploadArgs["idempotency_key"] = key
	}
	uploadResult, err := s.handleUploadFile(ctx, legacyRequest(uploadArgs))
	if err != nil {
		return nil, err
	}
	if uploadResult != nil && uploadResult.IsError {
		return coreErrorResult(uploadResult), nil
	}
	upload := structuredOf(uploadResult)
	uploadID := stringFromAny(upload, "upload_id")
	if uploadID == "" {
		if receipt, ok := upload["receipt"].(map[string]any); ok {
			uploadID = stringFromAny(receipt, "upload_id")
		}
	}
	if uploadID == "" {
		return errorResult(&ToolError{
			Code:    CodeInternal,
			Message: "the upload was accepted but returned no upload id; nothing was imported",
		}), nil
	}

	importArgs := map[string]any{"knowledge_base_id": kbID, "upload_id": uploadID}
	if config, ok := args["process_config"].(map[string]any); ok && len(config) > 0 {
		importArgs["process_config"] = config
	}
	importResult, err := s.handleImportDocumentFile(ctx, legacyRequest(importArgs))
	if err != nil {
		return nil, err
	}
	if importResult != nil && importResult.IsError {
		// The upload is staged and its receipt is persisted, so a caller can
		// retry the import with the same upload id instead of re-sending bytes.
		payload := map[string]any{
			"ingest_id": uploadID,
			"upload_id": uploadID,
			"stage":     "staged",
			"error":     mcpErrorText(importResult),
			"note": "the file is staged; retry ingest_document.status or import with the same " +
				"upload_id instead of uploading the bytes again",
		}
		result, jsonErr := jsonResult(payload)
		if jsonErr != nil {
			return nil, jsonErr
		}
		result.IsError = true
		return result, nil
	}
	imported := structuredOf(importResult)
	documentID := documentIDFromImport(imported)
	payload := map[string]any{
		"ingest_id":      uploadID,
		"upload_id":      uploadID,
		"document_id":    documentID,
		"deduplicated":   imported["deduplicated"],
		"queued":         imported["queued"],
		"process_config": structuredValue(imported["process_config"]),
		"file_name":      imported["file_name"],
		"file_size":      imported["file_size"],
		"stage":          "queued",
		"note":           imported["note"],
	}
	if _, ok := imported["process_config_applied"]; ok {
		payload["process_config_applied"] = imported["process_config_applied"]
	}
	return jsonResult(payload)
}

// documentIDFromImport reads the created document's id out of an import
// payload. The value under "document" is the typed documentSummary the legacy
// handler wraps into the generic map, so a plain map assertion returns nothing
// — the same shape mismatch that made the document status read as unknown.
// Both shapes are accepted so the field survives either handler style.
func documentIDFromImport(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if document, ok := structuredValue(payload["document"]).(map[string]any); ok {
		return stringFromAny(document, "id")
	}
	return ""
}

// coreIngestStatus reports the phases of one ingest. Parsing finished does not
// mean the index is searchable, and neither means wiki or graph generation
// finished, so the phases are reported separately rather than folded into a
// single percentage nobody can measure.
func (s *Server) coreIngestStatus(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	ingestID := strings.TrimSpace(stringValue(args["ingest_id"]))
	documentID := strings.TrimSpace(stringValue(args["document_id"]))
	if ingestID == "" && documentID == "" {
		return errorResult(invalidArgument("ingest_id or document_id is required")), nil
	}

	payload := map[string]any{}
	stage := "unknown"
	stageKnown := false
	if ingestID != "" {
		uploadResult, err := s.handleGetUploadStatus(ctx, legacyRequest(map[string]any{"upload_id": ingestID}))
		if err != nil {
			return nil, err
		}
		if uploadResult != nil && uploadResult.IsError {
			return coreErrorResult(uploadResult), nil
		}
		upload := structuredOf(uploadResult)
		payload["upload"] = upload
		if state, ok := upload["state"].(string); ok && state != "" {
			stage = state
			stageKnown = true
		}
		// The upload payload carries the real document id; legacy handlers
		// named the same field "document_id". Prefer it over a stale
		// knowledge_id so the round-trip "submit -> status" resolves to the
		// same document.
		if documentID == "" {
			if id, ok := upload["document_id"].(string); ok && id != "" {
				documentID = id
			} else if id, ok := upload["knowledge_id"].(string); ok && id != "" {
				documentID = id
			}
		}
	}
	if documentID != "" {
		documentResult, err := s.handleGetDocumentStatus(ctx, legacyRequest(map[string]any{"knowledge_id": documentID}))
		if err != nil {
			return nil, err
		}
		// structuredOf returns nil when the underlying handler wrapped a
		// typed value (the document status report is a struct, not a generic
		// map). Fall back to building a generic shape from the typed report
		// so the downstream phases lookup is uniform.
		doc, docKnown := s.ingestDocumentStatusPayload(ctx, documentResult)
		if docKnown {
			payload["document"] = doc
			payload["document_id"] = documentID
		} else if documentResult != nil && documentResult.IsError {
			payload["document_error"] = mcpErrorText(documentResult)
		}
	}
	payload["stage"] = stage
	if !stageKnown {
		stage = "unknown"
	}
	payload["phases"] = coreIngestPhases(payload)
	return jsonResult(payload)
}

// ingestDocumentStatusPayload reads the typed documentStatusReport out of
// a legacy tool result and normalises it to a generic map. structuredOf
// returns nil because the legacy handler wraps a struct, not a JSON map;
// callers that want the parse/index fields have to walk the typed value.
func (s *Server) ingestDocumentStatusPayload(
	ctx context.Context, result *mcp.CallToolResult,
) (map[string]any, bool) {
	if result == nil || result.IsError {
		return nil, false
	}
	outer, ok := result.StructuredContent.(map[string]any)
	if !ok {
		return nil, false
	}
	// The legacy handler wraps the typed documentStatusReport into the payload
	// map, so the nested value is normalised through the same converter the
	// rest of the core surface uses instead of a hand-written type assertion
	// that only matches one exact Go type.
	status, ok := structuredValue(outer["status"]).(map[string]any)
	if !ok {
		return nil, false
	}
	out := map[string]any{
		"document_id":            stringFromAny(status, "knowledge_id"),
		"knowledge_id":           stringFromAny(status, "knowledge_id"),
		"title":                  stringFromAny(status, "title"),
		"parse_status":           stringFromAny(status, "parse_status"),
		"parse_status_known":     status["parse_status_known"],
		"index_status":           stringFromAny(status, "index_status"),
		"enable_status":          stringFromAny(status, "enable_status"),
		"summary_status":         stringFromAny(status, "summary_status"),
		"pending_subtasks_count": status["pending_subtasks_count"],
		"pending_subtasks_known": status["pending_subtasks_known"],
		"error_message":          stringFromAny(status, "error_message"),
		"processed_at":           stringFromAny(status, "processed_at"),
		"updated_at":             stringFromAny(status, "updated_at"),
		"file_type":              stringFromAny(status, "file_type"),
		"file_size":              status["file_size"],
		"channel":                stringFromAny(status, "channel"),
		"note":                   stringFromAny(status, "note"),
		"applied_parser_engine":  stringFromAny(status, "applied_parser_engine"),
	}
	if rules, ok := status["requested_parser_engine_rules"]; ok && rules != nil {
		out["requested_parser_engine_rules"] = rules
	}
	return out, true
}

// coreIngestPhases normalises the underlying payloads into the phase view
// the main product renders, keeping "disabled", "not_configured" and
// "failed" distinguishable from each other. The document status report
// only carries parse/index state; wiki and graph live on the knowledge
// base row, not on a single document, so the per-document status maps
// wiki/graph to "not_reported" rather than guessing from a field that
// does not exist on the document report.
func coreIngestPhases(payload map[string]any) map[string]string {
	phases := map[string]string{
		"upload": "unknown",
		"parse":  "unknown",
		"index":  "unknown",
		"wiki":   "not_reported",
		"graph":  "not_reported",
	}
	if upload, ok := payload["upload"].(map[string]any); ok {
		phases["upload"] = firstNonEmpty(stringFromAny(upload, "state"), "unknown")
	}
	document, ok := payload["document"].(map[string]any)
	if !ok {
		return phases
	}
	phases["parse"] = firstNonEmpty(stringFromAny(document, "parse_status"), "unknown")
	phases["index"] = firstNonEmpty(stringFromAny(document, "index_status"), "unknown")
	return phases
}

func stringFromAny(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	return strings.TrimSpace(stringValue(payload[key]))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
