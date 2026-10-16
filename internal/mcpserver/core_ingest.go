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
	documentID := ""
	if document, ok := imported["document"].(map[string]any); ok {
		documentID = stringFromAny(document, "id")
	}
	payload := map[string]any{
		"ingest_id":      uploadID,
		"upload_id":      uploadID,
		"document_id":    documentID,
		"deduplicated":   imported["deduplicated"],
		"queued":         imported["queued"],
		"process_config": imported["process_config"],
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
		stage = stringFromAny(upload, "state")
		if documentID == "" {
			documentID = stringFromAny(upload, "knowledge_id")
		}
	}
	if documentID != "" {
		documentResult, err := s.handleGetDocumentStatus(ctx, legacyRequest(map[string]any{"knowledge_id": documentID}))
		if err != nil {
			return nil, err
		}
		if documentResult != nil && !documentResult.IsError {
			payload["document"] = structuredOf(documentResult)
			payload["document_id"] = documentID
		} else if documentResult != nil {
			payload["document_error"] = mcpErrorText(documentResult)
		}
	}
	payload["stage"] = stage
	payload["phases"] = coreIngestPhases(payload)
	return jsonResult(payload)
}

// coreIngestPhases normalises the underlying payloads into the phase view the
// main product renders, keeping "disabled", "not_configured" and "failed"
// distinguishable from each other.
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
	status, _ := document["status"].(map[string]any)
	if status == nil {
		return phases
	}
	phases["parse"] = firstNonEmpty(stringFromAny(status, "parse_status"), "unknown")
	phases["index"] = firstNonEmpty(stringFromAny(status, "index_status"), "unknown")
	phases["wiki"] = firstNonEmpty(stringFromAny(status, "wiki_status"), "not_reported")
	phases["graph"] = firstNonEmpty(stringFromAny(status, "graph_status"), "not_reported")
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
