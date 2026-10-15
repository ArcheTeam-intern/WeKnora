package mcpserver

import (
	"context"
	"errors"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
)

// createKnowledgeBaseTool is the entry-point write tool: there is no knowledge
// base yet, so the caller is gated on its workspace role instead of a KB grant
// (mirrors the REST rule for POST /knowledge-bases, which uses the
// Contributor floor).
func createKnowledgeBaseTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolCreateKnowledgeBase,
		mcp.WithDescription("Create a new knowledge base in the caller's workspace and return it. "+
			"Use it before uploading documents into a fresh library. Vector (semantic) and keyword "+
			"search are on by default; wiki and knowledge-graph generation can be asked for here."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Knowledge base name; must be unique "+
			"enough to be selected later by name")),
		mcp.WithString("description", mcp.Description("Optional description shown next to the name")),
		mcp.WithBoolean("enable_wiki", mcp.Description("Generate wiki pages for documents added later "+
			"(default false)")),
		mcp.WithBoolean("enable_graph", mcp.Description("Extract a knowledge graph from documents added "+
			"later (default false)")),
		mcp.WithBoolean("enable_keyword", mcp.Description("Enable keyword (BM25) indexing (default true)")),
		mcp.WithBoolean("enable_vector", mcp.Description("Enable semantic vector indexing (default true)")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
	)
}

// authorizeKBCreation is the entry-point gate, mirroring the two write paths
// the other mutating tools already follow:
//   - a scoped API key must hold the ingest capability;
//   - a web user must hold Contributor or higher in the workspace, the same
//     floor the REST route uses before a resource exists to own.
func authorizeKBCreation(ctx context.Context) error {
	if scope, ok := types.TenantAPIKeyScopeFromContext(ctx); ok && !scope.FullAccess &&
		!scope.HasCapability(types.APIKeyCapabilityIngest) {
		return forbidden("this endpoint is not allowed to create knowledge bases")
	}
	if principal, ok := types.PrincipalFromContext(ctx); ok && principal.Type == types.PrincipalWebUser {
		caller := types.CallerFromContext(ctx)
		if !caller.Role.HasPermission(types.TenantRoleContributor) {
			return forbidden("creating a knowledge base requires contributor permission or higher")
		}
	}
	return nil
}

func (s *Server) handleCreateKnowledgeBase(
	ctx context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return mcp.NewToolResultError("unauthorized"), nil
	}
	name := strings.TrimSpace(req.GetString("name", ""))
	if name == "" {
		return mcp.NewToolResultError("name is required"), nil
	}
	if len([]rune(name)) > 200 {
		return mcp.NewToolResultError("name is too long (max 200 characters)"), nil
	}
	if err := authorizeKBCreation(ctx); err != nil {
		return errorResult(err), nil
	}
	// The service stamps CreatorID from the REST user-id context key, which the
	// MCP path never sets: it resolves the same human through the caller. Bridge
	// it so a knowledge base created by a web user belongs to that user (later
	// writes are creator-or-admin), and leave machine-mode creations
	// tenant-owned exactly as the REST API-key path does.
	if principal, ok := types.PrincipalFromContext(ctx); ok && principal.Type == types.PrincipalWebUser {
		if caller := types.CallerFromContext(ctx); caller.UserID != "" {
			ctx = context.WithValue(ctx, types.UserIDContextKey, caller.UserID)
		}
	}

	// Absent flags keep the deployment defaults; only an explicit false
	// disables a pipeline.
	keyword := req.GetBool("enable_keyword", true)
	vector := req.GetBool("enable_vector", true)
	wiki := req.GetBool("enable_wiki", false)
	graph := req.GetBool("enable_graph", false)

	kb := &types.KnowledgeBase{
		Name:        name,
		Description: strings.TrimSpace(req.GetString("description", "")),
		Type:        types.KnowledgeBaseTypeDocument,
		IndexingStrategy: types.IndexingStrategy{
			VectorEnabled:  vector,
			KeywordEnabled: keyword,
			WikiEnabled:    wiki,
			GraphEnabled:   graph,
		},
	}
	// IsGraphEnabled requires both the flag and an enabled ExtractConfig;
	// without it the pipeline flag alone would silently extract nothing.
	if graph {
		kb.ExtractConfig = &types.ExtractConfig{Enabled: true}
	}

	created, err := s.kbService.CreateKnowledgeBase(ctx, kb)
	if err != nil {
		return errorResult(&ToolError{
			Code:    CodeInternal,
			Message: "failed to create the knowledge base",
			Details: map[string]any{"cause": err.Error()},
		}), nil
	}

	result := map[string]any{
		"knowledge_base": summarizeKnowledgeBase(created),
		"note": "The knowledge base exists now; upload documents into it with upload_file + " +
			"import_document_file, or add_document.",
	}
	// A restricted endpoint only serves the knowledge bases it was configured
	// with. Creation through the endpoint is a clear statement of intent to use
	// the new library here, so extend its own range instead of handing back an
	// id the very next call would reject as out of scope.
	if ep.RestrictsKnowledgeBases() && !containsID(ep.KnowledgeBaseIDs, created.ID) {
		scope := map[string]any{
			"restricted":        true,
			"endpoint_id":       ep.ID,
			"knowledge_base_id": created.ID,
		}
		if err := s.extendEndpointRange(ctx, ep, created.ID); err != nil {
			scope["added_to_endpoint_range"] = false
			scope["add_error"] = err.Error()
			scope["message"] = "this endpoint is restricted to a fixed set of knowledge bases and the " +
				"new one could not be added to its range: add it there (or clear the range) before " +
				"using it from this endpoint"
		} else {
			scope["added_to_endpoint_range"] = true
			scope["message"] = "the new knowledge base was added to this endpoint's range, so this " +
				"endpoint can select it right away"
		}
		result["endpoint_scope"] = scope
	}
	return jsonResult(result)
}

// extendEndpointRange appends one knowledge base to a restricted endpoint's
// allowlist. The stored row is reloaded through the repository update so the
// change survives the request; a failure is reported to the caller rather than
// failing the creation that already happened.
func (s *Server) extendEndpointRange(ctx context.Context, ep *types.MCPEndpoint, kbID string) error {
	if s.endpointRepo == nil {
		return errors.New("endpoint storage is unavailable")
	}
	updated := *ep // copy: never mutate the endpoint carried by the request
	ids := make([]string, 0, len(ep.KnowledgeBaseIDs)+1)
	ids = append(ids, ep.KnowledgeBaseIDs...)
	updated.KnowledgeBaseIDs = append(ids, kbID)
	return s.endpointRepo.Update(ctx, &updated)
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
