package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
	"gorm.io/gorm"
)

// createKnowledgeBaseTool is the entry-point write tool: there is no knowledge
// base yet, so the caller is gated on its workspace role instead of a KB grant
// (mirrors the REST rule for POST /knowledge-bases, which uses the
// Contributor floor).
func createKnowledgeBaseTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolCreateKnowledgeBase,
		mcp.WithDescription("Create a new knowledge base in the caller's workspace and return it. "+
			"Use it before uploading documents into a fresh library. Vector (semantic) and keyword "+
			"search are on by default; wiki and knowledge-graph generation can be asked for here. "+
			"Embedding, summary and OCR models default to the workspace's configured models, so a "+
			"new knowledge base is immediately usable."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Knowledge base name; must be unique "+
			"enough to be selected later by name")),
		mcp.WithString("description", mcp.Description("Optional description shown next to the name")),
		mcp.WithBoolean("enable_wiki", mcp.Description("Generate wiki pages for documents added later "+
			"(default false)")),
		mcp.WithBoolean("enable_graph", mcp.Description("Extract a knowledge graph from documents added "+
			"later (default false)")),
		mcp.WithBoolean("enable_keyword", mcp.Description("Enable keyword (BM25) indexing (default true)")),
		mcp.WithBoolean("enable_vector", mcp.Description("Enable semantic vector indexing (default true)")),
		mcp.WithArray("graph_entities", mcp.Description("Entity type names to extract when "+
			"enable_graph is true, e.g. [\"人员\",\"项目\"]. Without them the graph pipeline runs "+
			"but cannot pull out relations."), mcp.WithStringItems()),
		mcp.WithArray("graph_relations", mcp.Description("Relations to extract, one per entry, "+
			"written as \"source,target,type\" — e.g. [\"人员,项目,负责\"]."), mcp.WithStringItems()),
		mcp.WithString("embedding_model_id", mcp.Description("Override the workspace's embedding model")),
		mcp.WithString("summary_model_id", mcp.Description("Override the workspace's knowledge QA model")),
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
	if graph {
		kb.ExtractConfig = graphExtractionConfig(req)
	}
	// A knowledge base with no models cannot index anything ("failed to get
	// embedding model: model ID cannot be empty") and a KB without a vision
	// model cannot read scanned pages. Inherit the workspace's models unless the
	// caller named others; this is what makes an MCP-created library usable on
	// the spot instead of failing at the first import.
	if err := s.applyWorkspaceModelDefaults(ctx, kb, req); err != nil {
		return errorResult(err), nil
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
		"models": map[string]any{
			"embedding_model_id": created.EmbeddingModelID,
			"summary_model_id":   created.SummaryModelID,
			"vlm_enabled":        created.VLMConfig.Enabled,
		},
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

// graphExtractionConfig builds the extract config from the caller's entity and
// relation lists. Each relation is written as "source,target,type"; entries
// that do not parse are skipped rather than failing the whole creation.
func graphExtractionConfig(req mcp.CallToolRequest) *types.ExtractConfig {
	config := &types.ExtractConfig{Enabled: true}
	for _, name := range stringSlice(req, "graph_entities") {
		config.Nodes = append(config.Nodes, &types.GraphNode{Name: name})
	}
	for _, raw := range stringSlice(req, "graph_relations") {
		parts := strings.Split(raw, ",")
		if len(parts) != 3 {
			continue
		}
		from, to, kind := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		if from == "" || to == "" || kind == "" {
			continue
		}
		config.Relations = append(config.Relations, &types.GraphRelation{Node1: from, Node2: to, Type: kind})
	}
	return config
}

func stringSlice(req mcp.CallToolRequest, key string) []string {
	raw := req.GetStringSlice(key, nil)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// applyWorkspaceModelDefaults fills in the tenant's configured models when the
// caller did not name any, so the new knowledge base can actually parse, index
// and OCR documents. Values already present on the request win.
func (s *Server) applyWorkspaceModelDefaults(
	ctx context.Context, kb *types.KnowledgeBase, req mcp.CallToolRequest,
) error {
	embedding := strings.TrimSpace(req.GetString("embedding_model_id", ""))
	summary := strings.TrimSpace(req.GetString("summary_model_id", ""))
	if embedding != "" || summary != "" {
		kb.EmbeddingModelID = embedding
		kb.SummaryModelID = summary
		return nil
	}
	if s.db == nil {
		return nil // nothing to inherit from; the caller's other fields still apply
	}
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return nil
	}
	// Workspace model rows are the same ones the UI uses; a default row wins,
	// otherwise the oldest configured model of that type is used.
	pick := func(modelType types.ModelType) string {
		var id string
		err := s.db.WithContext(ctx).
			Table("models").
			Select("id").
			Where("tenant_id = ? AND type = ? AND deleted_at IS NULL", tenantID, string(modelType)).
			Order("is_default DESC, created_at ASC").
			Limit(1).
			Scan(&id).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return ""
		}
		return id
	}
	kb.EmbeddingModelID = pick(types.ModelTypeEmbedding)
	kb.SummaryModelID = pick(types.ModelTypeKnowledgeQA)
	if kb.EmbeddingModelID == "" {
		return &ToolError{
			Code: CodeInvalidArgument,
			Message: "the workspace has no embedding model configured; configure one before creating " +
				"a knowledge base, or pass embedding_model_id",
		}
	}
	// Vision model: without it, scanned pages are indexed as empty text.
	if vlmID := pick(types.ModelTypeVLLM); vlmID != "" {
		kb.VLMConfig = types.VLMConfig{Enabled: true, ModelID: vlmID}
	}
	return nil
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
	if err := s.endpointRepo.Update(ctx, &updated); err != nil {
		return fmt.Errorf("endpoint range update failed: %w", err)
	}
	return nil
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
