package mcpserver

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/Tencent/WeKnora/internal/application/access"
	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
)

func coreWikiWorkspaceTool() mcp.Tool {
	return mcp.NewTool(CoreToolWikiWorkspace,
		mcp.WithDescription("Manage native wiki pages and revision history, or read the native wiki page-link graph (not the extracted entity graph). Writes require explicit wiki_workspace endpoint permission and native library write access. Editable Markdown retains canonical resource:// URLs. Update requires the current version; restore version selects a historical revision."),
		mcp.WithString("action", mcp.Required(), mcp.Enum("get", "create", "update", "delete", "revisions", "restore", "graph")),
		mcp.WithString("knowledge_base_id", mcp.Required(), mcp.Description("Knowledge base id or exact name")),
		mcp.WithString("slug", mcp.Description("Required for page and revision actions")),
		mcp.WithString("title"), mcp.WithString("content"), mcp.WithString("summary"),
		mcp.WithString("page_type"), mcp.WithString("status"),
		mcp.WithNumber("version", mcp.Description("Positive current version for update; historical target version for restore; optional snapshot version for revisions")),
		mcp.WithNumber("limit"), mcp.WithNumber("offset"), mcp.WithString("center"), mcp.WithNumber("depth"),
	)
}

// wikiWorkspaceInt rejects fractional and malformed version/pagination values.
func wikiWorkspaceInt(args map[string]any, key string, fallback, min, max int) (int, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return fallback, nil
	}
	var n float64
	switch v := raw.(type) {
	case float64:
		n = v
	case int:
		n = float64(v)
	default:
		return 0, invalidArgument("%s must be an integer", key)
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n < float64(min) || n > float64(max) {
		return 0, invalidArgument("%s must be an integer between %d and %d", key, min, max)
	}
	return int(n), nil
}

func wikiWorkspaceError(err error) *mcp.CallToolResult {
	switch {
	case errors.Is(err, repository.ErrWikiPageNotFound):
		return errorResult(notFound("wiki page or revision was not found"))
	case errors.Is(err, repository.ErrWikiPageConflict):
		return errorResult(&ToolError{Code: "CONFLICT", Message: "wiki page was modified by someone else; reload before retrying"})
	case errors.Is(err, service.ErrWikiRevertToCurrentVersion):
		return errorResult(invalidArgument("cannot restore the current version"))
	default:
		return errorResult(err)
	}
}

// A page response carries a fresh evidence view; persisted source labels are
// never proof that the document or quoted chunks were read by this caller.
func (s *Server) wikiWorkspacePageResult(ctx context.Context, page *types.WikiPage, kb *types.KnowledgeBase) (*mcp.CallToolResult, error) {
	if page == nil {
		return errorResult(notFound("wiki page was not found")), nil
	}
	copy := *page
	if len(page.SourceRefs) > 0 && s.knowledgeService == nil {
		copy.SourceRefsV2 = []types.SourceRefV2{{KnowledgeBaseID: kb.ID, ProvenanceStatus: "unverified_source"}}
	} else {
		copy.SourceRefsV2 = s.resolveWikiPageSourceRefs(ctx, page, kb)
	}
	return jsonResult(map[string]any{"page": &copy})
}

func (s *Server) handleCoreWikiWorkspace(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	action, err := coreActionOf(CoreToolWikiWorkspace, args)
	if err != nil {
		return errorResult(err), nil
	}
	perm, err := resolveCoreAction(CoreToolWikiWorkspace, action, args)
	if err != nil {
		return errorResult(err), nil
	}
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	// Keep direct invocation and the registered middleware equally restrictive.
	if missing := missingCorePermissions(ep, perm); len(missing) > 0 {
		return errorResult(corePermissionError(CoreToolWikiWorkspace, action, missing)), nil
	}
	kbID, err := requireKnowledgeBaseID(args)
	if err != nil {
		return errorResult(err), nil
	}
	if s.kbService == nil || s.wikiService == nil {
		return errorResult(&ToolError{Code: CodeUpstreamUnavailable, Message: "native wiki service is unavailable"}), nil
	}
	kb, err := s.knowledgeBaseInScope(ctx, kbID)
	if err != nil {
		return errorResult(err), nil
	}
	if !kb.IsWikiEnabled() {
		return errorResult(&ToolError{Code: CodeWikiNotEnabled, Message: "wiki is not enabled for this knowledge base"}), nil
	}
	write := action == CoreActionCreate || action == CoreActionUpdate || action == CoreActionDelete || action == CoreActionRestore
	role := types.OrgRoleViewer
	if write {
		if principal, ok := types.PrincipalFromContext(ctx); ok && principal.Type == types.PrincipalWebUser && !types.CallerFromContext(ctx).Role.HasPermission(types.TenantRoleContributor) {
			return errorResult(forbidden("viewers may only read the wiki workspace")), nil
		}
		if err := s.requireKBWriteOwnership(ctx, kb); err != nil {
			return errorResult(err), nil
		}
		role = types.OrgRoleEditor
	}
	_, ctx, err = s.authorizeKB(ctx, kb, role)
	if err != nil {
		return errorResult(forbidden("not allowed to access this wiki at the required permission")), nil
	}
	if write {
		if err := access.RequireKBWrite(ctx, kb); err != nil {
			return errorResult(forbidden("wiki writes require the ingest capability and an explicit library write grant")), nil
		}
		ctx = types.WithWikiEditSource(ctx, types.WikiEditSourceUser)
	}
	slug, err := coreStringArg(args, "slug", "")
	if err != nil {
		return errorResult(err), nil
	}
	if action != CoreActionGraph && slug == "" {
		return errorResult(invalidArgument("slug is required")), nil
	}
	version, err := wikiWorkspaceInt(args, "version", 0, 0, math.MaxInt32)
	if err != nil {
		return errorResult(err), nil
	}
	switch action {
	case CoreActionGraph:
		center, e := coreStringArg(args, "center", "")
		if e != nil {
			return errorResult(e), nil
		}
		depth, e := wikiWorkspaceInt(args, "depth", defaultWikiGraphDepth, 1, maxWikiGraphDepth)
		if e != nil {
			return errorResult(e), nil
		}
		limit, e := wikiWorkspaceInt(args, "limit", defaultWikiGraphLimit, 1, maxWikiGraphLimit)
		if e != nil {
			return errorResult(e), nil
		}
		pageType, e := coreStringArg(args, "page_type", "")
		if e != nil {
			return errorResult(e), nil
		}
		if pageType != "" && !types.IsValidWikiPageType(pageType) {
			return errorResult(invalidArgument("invalid page_type")), nil
		}
		mode := "overview"
		if center != "" {
			mode = "ego"
		}
		var filters []string
		if pageType != "" {
			filters = []string{pageType}
		}
		graph, e := s.wikiService.GetGraph(ctx, &types.WikiGraphRequest{KnowledgeBaseID: kb.ID, Mode: mode, Center: center, Depth: depth, Limit: limit, Types: filters})
		if e != nil {
			return wikiWorkspaceError(e), nil
		}
		return jsonResult(map[string]any{"graph": graph})
	case CoreActionRevisions:
		if version > 0 {
			revision, e := s.wikiService.GetRevision(ctx, kb.ID, slug, version)
			if e != nil {
				return wikiWorkspaceError(e), nil
			}
			return jsonResult(map[string]any{"revision": revision})
		}
		limit, e := wikiWorkspaceInt(args, "limit", 20, 1, 100)
		if e != nil {
			return errorResult(e), nil
		}
		offset, e := wikiWorkspaceInt(args, "offset", 0, 0, math.MaxInt32)
		if e != nil {
			return errorResult(e), nil
		}
		revisions, e := s.wikiService.ListRevisions(ctx, kb.ID, slug, limit, offset)
		if e != nil {
			return wikiWorkspaceError(e), nil
		}
		return jsonResult(map[string]any{"revisions": revisions})
	case CoreActionRestore:
		if version < 1 {
			return errorResult(invalidArgument("version is required and must be positive")), nil
		}
		page, e := s.wikiService.RevertPageToVersion(ctx, kb.ID, slug, version)
		if e != nil {
			return wikiWorkspaceError(e), nil
		}
		return s.wikiWorkspacePageResult(ctx, page, kb)
	case CoreActionDelete:
		// Native DeletePage has no version precondition. Do not promise a CAS
		// using a separate preflight lookup, which would still race with writes.
		if e := s.wikiService.DeletePage(ctx, kb.ID, slug); e != nil {
			return wikiWorkspaceError(e), nil
		}
		return jsonResult(map[string]any{"deleted": true, "slug": slug})
	}
	var page types.WikiPage
	if action == CoreActionCreate {
		page = types.WikiPage{KnowledgeBaseID: kb.ID, TenantID: kb.TenantID, Slug: slug}
	} else {
		existing, e := s.wikiService.GetPageBySlug(ctx, kb.ID, slug)
		if e != nil {
			return wikiWorkspaceError(e), nil
		}
		if existing == nil {
			return errorResult(notFound("wiki page was not found")), nil
		}
		if action == CoreActionGet {
			return s.wikiWorkspacePageResult(ctx, existing, kb)
		}
		if version < 1 {
			return errorResult(invalidArgument("version is required and must be positive")), nil
		}
		if version != existing.Version {
			return errorResult(&ToolError{Code: "CONFLICT", Message: "wiki page was modified by someone else", Details: map[string]any{"current_version": existing.Version}}), nil
		}
		// Copy native state, replacing only editable fields. Source refs, identity,
		// folder placement and provenance remain owned by the native service.
		page = *existing
	}
	for key, target := range map[string]*string{"title": &page.Title, "content": &page.Content, "summary": &page.Summary, "page_type": &page.PageType, "status": &page.Status} {
		if raw, ok := args[key]; ok {
			value, ok := raw.(string)
			if !ok {
				return errorResult(invalidArgument("%s must be a string", key)), nil
			}
			if key == "title" || key == "page_type" || key == "status" {
				value = strings.TrimSpace(value)
			}
			if action == CoreActionUpdate && ((key == "page_type" && !types.IsValidWikiPageType(value)) || (key == "status" && !types.IsValidWikiPageStatus(value))) {
				return errorResult(invalidArgument("invalid %s", key)), nil
			}
			*target = value
		}
	}
	if page.PageType != "" && !types.IsValidWikiPageType(page.PageType) {
		return errorResult(invalidArgument("invalid page_type")), nil
	}
	if page.Status != "" && !types.IsValidWikiPageStatus(page.Status) {
		return errorResult(invalidArgument("invalid status")), nil
	}
	var saved *types.WikiPage
	if action == CoreActionCreate {
		saved, err = s.wikiService.CreatePage(ctx, &page)
	} else {
		saved, err = s.wikiService.UpdatePage(ctx, &page)
	}
	if err != nil {
		return wikiWorkspaceError(err), nil
	}
	return s.wikiWorkspacePageResult(ctx, saved, kb)
}
