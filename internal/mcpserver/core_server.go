package mcpserver

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// coreInstructions is the surface description the client sees.
const coreInstructions = "WeKnora knowledge core: five tools for the knowledge workflow. " +
	"knowledge_base lists, inspects and creates libraries; ingest_document submits file bytes and " +
	"reports processing status; search_knowledge retrieves evidence from documents, wiki pages and " +
	"the knowledge graph; read_document browses and re-reads the original text for citation checks; " +
	"ask answers a question with the platform's native QA modes. Every action requires the " +
	"corresponding permission on this endpoint."

// coreToolCatalog lists exactly the five core tools, in discovery order.
func (s *Server) coreToolCatalog() []server.ServerTool {
	return []server.ServerTool{
		{Tool: coreKnowledgeBaseTool(), Handler: s.handleCoreKnowledgeBase},
		{Tool: coreIngestTool(), Handler: s.handleCoreIngest},
		{Tool: coreSearchTool(), Handler: s.handleCoreSearch},
		{Tool: coreReadTool(), Handler: s.handleCoreRead},
		{Tool: coreAskTool(), Handler: s.handleCoreAsk},
	}
}

// CoreToolNames returns the names the core surface publishes, for the router's
// documentation and for tests.
//
// Three of them (search_knowledge, read_document, ask) carry the same name as a
// legacy tool on purpose: the plan fixes this five-tool contract, and the two
// surfaces are mounted on different paths (/mcp/core/:id versus /mcp/:id), so
// no single tools/list ever mixes the two catalogues. Their schemas differ —
// the core versions are action- and source-oriented — which is exactly why they
// must not be published side by side.
func CoreToolNames() []string {
	return []string{CoreToolKnowledgeBase, CoreToolIngest, CoreToolSearch, CoreToolRead, CoreToolAsk}
}

// coreMCPServer builds (once) the second MCP server that exposes only the core
// tools. It shares every dependency, the rate limiter and the endpoint
// repository with the legacy surface: this is a different tool list, not a
// second process, database or queue.
func (s *Server) coreMCPServer() *server.MCPServer {
	s.coreOnce.Do(func() {
		core := server.NewMCPServer(
			serverName,
			serverVersion,
			server.WithToolCapabilities(false),
			server.WithRecovery(),
			server.WithToolHandlerMiddleware(s.guardCoreTool),
			server.WithInstructions(coreInstructions),
		)
		core.AddTools(s.coreToolCatalog()...)
		s.coreMCP = core
		s.coreHandler = server.NewStreamableHTTPServer(core, server.WithStateLess(true))
	})
	return s.coreMCP
}

// CoreHandler returns the http.Handler to mount under /mcp/core/:endpoint_id.
// The caller must run the same MCPEndpointAuth first, so the identity, endpoint
// and request scope are already on the context.
func (s *Server) CoreHandler() http.Handler {
	s.coreMCPServer()
	return s.coreHandler
}

// guardCoreTool is the core surface's counterpart to guardTool. It enforces the
// action-level permission mapping, the rate limit and the endpoint bookkeeping,
// and it never consults the legacy tool allowlist directly — guessing a core
// tool name the endpoint did not grant still fails, because the underlying
// permission is missing.
func (s *Server) guardCoreTool(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ep, err := endpointFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError("unauthorized"), nil
		}
		if !isCoreToolName(req.Params.Name) {
			return errorResult(&ToolError{
				Code:    CodeForbidden,
				Message: fmt.Sprintf("tool %q is not part of the knowledge core surface", req.Params.Name),
			}), nil
		}
		args, ok := req.Params.Arguments.(map[string]any)
		if !ok && req.Params.Arguments != nil {
			return errorResult(invalidArgument("arguments must be an object")), nil
		}
		action, err := coreActionOf(req.Params.Name, args)
		if err != nil {
			return errorResult(err), nil
		}
		perm, err := resolveCoreAction(req.Params.Name, action, args)
		if err != nil {
			return errorResult(err), nil
		}
		if missing := missingCorePermissions(ep, perm); len(missing) > 0 {
			return errorResult(corePermissionError(req.Params.Name, action, missing)), nil
		}
		if !s.limiter.Allow(ctx, ep.ID, ep.RateLimitPerMinute) {
			return errorResult(&ToolError{
				Code:      CodeRateLimited,
				Message:   "rate limit exceeded for this endpoint, retry shortly",
				Retryable: true,
			}), nil
		}
		s.touchLastUsed(ctx, ep.ID)
		result, err := next(ctx, req)
		if err != nil {
			return result, err
		}
		return s.rewriteResourceURLs(ctx, ep, result), nil
	}
}

func isCoreToolName(name string) bool {
	for _, candidate := range CoreToolNames() {
		if candidate == name {
			return true
		}
	}
	return false
}

// coreRequirementFor exposes the permission table to tests and to the
// documentation generator without duplicating it.
func coreRequirementFor(tool, action string, args map[string]any) ([]string, error) {
	perm, err := resolveCoreAction(tool, action, args)
	if err != nil {
		return nil, err
	}
	return append([]string{}, perm.Tools...), nil
}

// legacyRequest rebuilds a call request for one of the existing handlers. The
// core tools aggregate the same business functions the legacy tools call
// directly; nothing here fabricates JSON-RPC traffic or calls back into the
// REST API.
func legacyRequest(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: args}}
}

// structuredOf returns the structured payload of a tool result, or nil.
func structuredOf(result *mcp.CallToolResult) map[string]any {
	if result == nil {
		return nil
	}
	if payload, ok := result.StructuredContent.(map[string]any); ok {
		return payload
	}
	return nil
}

// coreErrorResult converts a failure from an underlying handler into the core
// surface's error shape, preserving code and details.
func coreErrorResult(result *mcp.CallToolResult) *mcp.CallToolResult {
	if result == nil {
		return errorResult(&ToolError{Code: CodeInternal, Message: "no result", Retryable: false})
	}
	if payload := structuredOf(result); payload != nil {
		if _, ok := payload["error"]; ok {
			return result
		}
	}
	if result.IsError {
		return result
	}
	return errorResult(&ToolError{Code: CodeInternal, Message: "the underlying call failed", Retryable: false})
}

// coreKnowledgeBaseIDs reads the knowledge base selection a core call carries.
func coreKnowledgeBaseIDs(args map[string]any) ([]string, error) {
	return coreStringList(args, "knowledge_base_ids")
}

// mergedResult renders several underlying payloads as one core response,
// keeping per-part identity so a caller can tell which part failed.
func mergedResult(parts map[string]any, notes ...string) (*mcp.CallToolResult, error) {
	payload := map[string]any{"parts": parts}
	if len(notes) > 0 {
		payload["notes"] = notes
	}
	return jsonResult(payload)
}

// requireKnowledgeBaseID validates the single-library actions.
func requireKnowledgeBaseID(args map[string]any) (string, error) {
	value, err := coreStringArg(args, "knowledge_base_id", "")
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", invalidArgument("knowledge_base_id is required")
	}
	return value, nil
}

// endpointKnowledgeBaseScope is the caller's own view of one library, used by
// knowledge_base.get to answer "what can I do with this library here".
func (s *Server) coreLibraryDetail(
	ctx context.Context, kbID string, includeCapabilities bool,
) (map[string]any, error) {
	statusResult, err := s.handleGetKnowledgeBaseStatus(ctx, legacyRequest(map[string]any{
		"knowledge_base_id": kbID,
	}))
	if err != nil {
		return nil, err
	}
	if statusResult != nil && statusResult.IsError {
		return nil, fmt.Errorf("%s", mcpErrorText(statusResult))
	}
	status := structuredOf(statusResult)
	if status == nil {
		return nil, fmt.Errorf("knowledge base status is unavailable")
	}
	detail := map[string]any{"status": status}
	if !includeCapabilities {
		return detail, nil
	}
	enginesResult, err := s.handleListParserEngines(ctx, legacyRequest(map[string]any{}))
	if err != nil {
		return nil, err
	}
	if payload := structuredOf(enginesResult); payload != nil {
		detail["parser_engines"] = payload
	}
	// Published QA profiles: the endpoint's configured default agent is the
	// only profile a caller may ask for by id (see ask). Reporting it keeps the
	// caller from inventing internal agent ids.
	if ep, err := endpointFromContext(ctx); err == nil && ep != nil {
		profiles := []map[string]any{{"id": "", "label": "端点默认", "description": "该端点配置的默认问答模式"}}
		if agentID := ep.DefaultAgentID; agentID != "" {
			profiles = append(profiles, map[string]any{
				"id": agentID, "label": "端点发布的知识型配置",
				"description": "管理员在该端点上发布的问答配置",
			})
		}
		detail["qa_profiles"] = profiles
		detail["selectable_on_endpoint"] = ep.AllowsKnowledgeBase(kbID)
	}
	return detail, nil
}

// mcpErrorText extracts the human-readable error text from a tool result.
func mcpErrorText(result *mcp.CallToolResult) string {
	if result == nil {
		return "unknown error"
	}
	if payload := structuredOf(result); payload != nil {
		if raw, ok := payload["error"].(map[string]any); ok {
			if message, ok := raw["message"].(string); ok && message != "" {
				return message
			}
		}
	}
	for _, content := range result.Content {
		if text, ok := content.(mcp.TextContent); ok && text.Text != "" {
			return text.Text
		}
	}
	return "the underlying call failed"
}

// coreKnowledgeBaseAllowed reports whether the endpoint serves this library at
// all, so a create can report selectability instead of failing later.
func coreKnowledgeBaseAllowed(ep *types.MCPEndpoint, kbID string) bool {
	return ep.AllowsKnowledgeBase(kbID)
}
