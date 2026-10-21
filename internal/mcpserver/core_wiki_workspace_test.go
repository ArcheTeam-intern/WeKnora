package mcpserver

import (
	"context"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/mcpscope"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestWikiWorkspacePermissionsAreExplicitAndIndependent(t *testing.T) {
	for action, permission := range map[string]string{"get": types.MCPEndpointToolWikiReadPage, "revisions": types.MCPEndpointToolWikiReadPage, "graph": types.MCPEndpointToolWikiGraph, "create": "wiki_workspace", "update": "wiki_workspace", "delete": "wiki_workspace", "restore": "wiki_workspace"} {
		p, err := resolveCoreAction("wiki_workspace", action, nil)
		require.NoError(t, err)
		require.Equal(t, []string{permission}, p.Tools)
		require.NotEmpty(t, missingCorePermissions(&types.MCPEndpoint{Tools: types.StringArray{types.MCPEndpointToolWikiSearch}}, p))
	}
	require.NotContains(t, types.DefaultMCPEndpointTools(), "wiki_workspace")
	require.Contains(t, types.MCPEndpointCapabilitiesForTools([]string{"wiki_workspace"}), string(types.APIKeyCapabilityIngest))
}

func wikiWorkspaceFixture(t *testing.T) (*Server, context.Context, *types.KnowledgeBase) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.WikiFolder{}, &types.WikiPage{}, &types.WikiPageRevision{}))
	kb := &types.KnowledgeBase{ID: "kb-wiki", TenantID: 1, CreatorID: "owner", IndexingStrategy: types.IndexingStrategy{WikiEnabled: true}}
	srv := newScopeTestServer(kb)
	srv.wikiService = service.NewWikiPageService(repository.NewWikiPageRepository(db), nil, nil, nil, nil, nil)
	ep := &types.MCPEndpoint{ID: "wiki", TenantID: 1, Tools: types.StringArray{"wiki_workspace", types.MCPEndpointToolWikiReadPage, types.MCPEndpointToolWikiGraph}}
	ctx := mcpCallContext(1, ep)
	ctx = types.WithCaller(ctx, types.Caller{TenantID: 1, UserID: "owner", Role: types.TenantRoleContributor})
	ctx = context.WithValue(ctx, types.UserIDContextKey, "owner")
	ctx = types.WithPrincipal(ctx, types.Principal{Type: types.PrincipalWebUser, ID: "owner"})
	return srv, ctx, kb
}

func wikiWorkspaceCall(t *testing.T, srv *Server, ctx context.Context, action string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	args["action"] = action
	args["knowledge_base_id"] = "kb-wiki"
	r, err := srv.handleCoreWikiWorkspace(ctx, coreCall("wiki_workspace", args))
	require.NoError(t, err)
	require.NotNil(t, r)
	return r
}

func TestWikiWorkspaceNativeCRUDRevisionsGraphAndProvenance(t *testing.T) {
	srv, ctx, kb := wikiWorkspaceFixture(t)
	source := &types.WikiPage{TenantID: 1, KnowledgeBaseID: kb.ID, Slug: "concept/first", Title: "First", Content: "original", PageType: types.WikiPageTypeConcept, SourceRefs: types.StringArray{"doc-a|Evidence"}, ChunkRefs: types.StringArray{"chunk-a"}}
	created, err := srv.wikiService.CreatePage(types.WithWikiEditSource(ctx, types.WikiEditSourcePipeline), source)
	require.NoError(t, err)
	r := wikiWorkspaceCall(t, srv, ctx, "create", map[string]any{"slug": "concept/second", "title": "Second", "content": "[[concept/first]]", "page_type": "concept", "tenant_id": 99, "source_refs": []string{"injected"}})
	require.False(t, r.IsError, mcpErrorText(r))
	second := structuredOf(r)["page"].(*types.WikiPage)
	require.Equal(t, uint64(1), second.TenantID)
	require.Empty(t, second.SourceRefs)
	require.Equal(t, types.WikiEditSourceUser, second.LastEditSource)
	graph := wikiWorkspaceCall(t, srv, ctx, "graph", map[string]any{"center": "concept/first", "depth": 2})
	require.False(t, graph.IsError, mcpErrorText(graph))
	require.Len(t, structuredOf(graph)["graph"].(*types.WikiGraphData).Edges, 1)
	canonical := "![image](resource://images/original.png)"
	updated := wikiWorkspaceCall(t, srv, ctx, "update", map[string]any{"slug": created.Slug, "version": created.Version, "content": canonical, "source_refs": []string{"injected"}, "tenant_id": 99})
	require.False(t, updated.IsError, mcpErrorText(updated))
	page := structuredOf(updated)["page"].(*types.WikiPage)
	require.Equal(t, created.Version+1, page.Version)
	require.Equal(t, source.SourceRefs, page.SourceRefs)
	require.Equal(t, source.ChunkRefs, page.ChunkRefs)
	require.Equal(t, types.WikiEditSourceUser, page.LastEditSource)
	require.Equal(t, "owner", page.LastEditorID)
	stale := wikiWorkspaceCall(t, srv, ctx, "update", map[string]any{"slug": created.Slug, "version": created.Version, "content": "stale"})
	require.True(t, stale.IsError)
	require.Equal(t, "CONFLICT", structuredOf(stale)["error"].(map[string]any)["code"])
	read := wikiWorkspaceCall(t, srv, ctx, "get", map[string]any{"slug": created.Slug})
	require.Equal(t, canonical, structuredOf(read)["page"].(*types.WikiPage).Content)
	require.Equal(t, "unverified_source", structuredOf(read)["page"].(*types.WikiPage).SourceRefsV2[0].ProvenanceStatus)
	srv.limiter = newCoreTestLimiter()
	guarded, err := srv.guardCoreTool(srv.handleCoreWikiWorkspace)(ctx, coreCall("wiki_workspace", map[string]any{"action": "get", "knowledge_base_id": kb.ID, "slug": created.Slug}))
	require.NoError(t, err)
	require.False(t, guarded.IsError, mcpErrorText(guarded))
	require.Equal(t, canonical, structuredOf(guarded)["page"].(*types.WikiPage).Content)
	history := wikiWorkspaceCall(t, srv, ctx, "revisions", map[string]any{"slug": created.Slug})
	require.False(t, history.IsError, mcpErrorText(history))
	snap := wikiWorkspaceCall(t, srv, ctx, "revisions", map[string]any{"slug": created.Slug, "version": 1})
	require.False(t, snap.IsError, mcpErrorText(snap))
	require.Equal(t, "original", structuredOf(snap)["revision"].(*types.WikiPageRevision).Content)
	restored := wikiWorkspaceCall(t, srv, ctx, "restore", map[string]any{"slug": created.Slug, "version": 1})
	require.False(t, restored.IsError, mcpErrorText(restored))
	require.Equal(t, source.SourceRefs, structuredOf(restored)["page"].(*types.WikiPage).SourceRefs)
	deleted := wikiWorkspaceCall(t, srv, ctx, "delete", map[string]any{"slug": created.Slug})
	require.False(t, deleted.IsError, mcpErrorText(deleted))
	gone := wikiWorkspaceCall(t, srv, ctx, "get", map[string]any{"slug": created.Slug})
	require.True(t, gone.IsError)
}

func TestWikiWorkspaceWriteDenialsDoNotMutate(t *testing.T) {
	srv, ctx, _ := wikiWorkspaceFixture(t)
	for _, role := range []types.TenantRole{types.TenantRoleViewer, types.TenantRoleContributor} {
		deniedCtx := types.WithCaller(ctx, types.Caller{TenantID: 1, UserID: "other", Role: role})
		deniedCtx = types.WithPrincipal(deniedCtx, types.Principal{Type: types.PrincipalWebUser, ID: "other"})
		denied := wikiWorkspaceCall(t, srv, deniedCtx, "create", map[string]any{"slug": "concept/denied", "title": "Denied"})
		require.True(t, denied.IsError)
	}
	viewerOwner := types.WithCaller(ctx, types.Caller{TenantID: 1, UserID: "owner", Role: types.TenantRoleViewer})
	require.True(t, wikiWorkspaceCall(t, srv, viewerOwner, "create", map[string]any{"slug": "concept/denied"}).IsError)
	ep := &types.MCPEndpoint{ID: "readonly", TenantID: 1, Tools: types.StringArray{types.MCPEndpointToolWikiReadPage}}
	readOnly := context.WithValue(ctx, types.MCPEndpointContextKey, ep)
	require.True(t, wikiWorkspaceCall(t, srv, readOnly, "create", map[string]any{"slug": "concept/denied"}).IsError)
	outside := context.WithValue(ctx, types.MCPEndpointContextKey, &types.MCPEndpoint{ID: "other", Tools: types.StringArray{"wiki_workspace"}, KnowledgeBaseIDs: types.StringArray{"kb-other"}})
	require.True(t, wikiWorkspaceCall(t, srv, outside, "create", map[string]any{"slug": "concept/denied"}).IsError)
	selected := mcpscope.WithScope(ctx, mcpscope.Scope{Present: true, Selected: []string{"kb-other"}})
	require.True(t, wikiWorkspaceCall(t, srv, selected, "create", map[string]any{"slug": "concept/denied"}).IsError)
	retrieveOnly := types.WithTenantAPIKeyScope(ctx, types.TenantAPIKeyScope{KnowledgeBaseIDs: types.StringArray{"kb-wiki"}, Capabilities: types.StringArray{string(types.APIKeyCapabilityRetrieve)}})
	require.True(t, wikiWorkspaceCall(t, srv, retrieveOnly, "create", map[string]any{"slug": "concept/denied"}).IsError)
	_, err := srv.wikiService.GetPageBySlug(ctx, "kb-wiki", "concept/denied")
	require.ErrorIs(t, err, repository.ErrWikiPageNotFound)
}

func TestWikiWorkspaceMalformedVersionsAndUnavailableService(t *testing.T) {
	srv, ctx, _ := wikiWorkspaceFixture(t)
	for _, value := range []any{-1, 1.5, "1", false} {
		require.True(t, wikiWorkspaceCall(t, srv, ctx, "update", map[string]any{"slug": "concept/x", "version": value}).IsError)
	}
	srv.wikiService = nil
	require.True(t, wikiWorkspaceCall(t, srv, ctx, "get", map[string]any{"slug": "concept/x"}).IsError)
}

func TestWikiWorkspaceUpdateRejectsExplicitEmptyEnums(t *testing.T) {
	srv, ctx, _ := wikiWorkspaceFixture(t)
	created := wikiWorkspaceCall(t, srv, ctx, "create", map[string]any{"slug": "concept/enum", "title": "Enum", "content": "body", "page_type": "concept", "status": "published"})
	require.False(t, created.IsError, mcpErrorText(created))
	for _, key := range []string{"page_type", "status"} {
		for _, value := range []string{"", "  "} {
			r := wikiWorkspaceCall(t, srv, ctx, "update", map[string]any{"slug": "concept/enum", "version": 1, key: value})
			require.True(t, r.IsError, "%s=%q must be refused", key, value)
		}
	}
}

func TestWikiWorkspacePageDoesNotTrustCachedProvenance(t *testing.T) {
	srv, ctx, kb := wikiWorkspaceFixture(t)
	page := &types.WikiPage{KnowledgeBaseID: kb.ID, SourceRefs: types.StringArray{"private-doc|Label"}, SourceRefsV2: []types.SourceRefV2{{KnowledgeID: "private-doc", Quote: "fabricated quote", ProvenanceStatus: "verified"}}}
	result, err := srv.wikiWorkspacePageResult(ctx, page, kb)
	require.NoError(t, err)
	returned := structuredOf(result)["page"].(*types.WikiPage)
	require.NotSame(t, page, returned)
	require.Equal(t, "unverified_source", returned.SourceRefsV2[0].ProvenanceStatus)
	require.Empty(t, returned.SourceRefsV2[0].KnowledgeID)
	require.Empty(t, returned.SourceRefsV2[0].Quote)
	require.Equal(t, "verified", page.SourceRefsV2[0].ProvenanceStatus, "response derivation must not mutate the native page")
}

func TestWikiWorkspaceGuardsKeepEditableResourceReferencesCanonical(t *testing.T) {
	f := newResourceURLFixture(t)
	f.ep.Tools = append(f.ep.Tools, types.MCPEndpointToolWikiReadPage, types.MCPEndpointToolWikiWorkspace)
	f.srv.limiter = newCoreTestLimiter()
	content := "![figure](" + f.ref + ")"
	payload := map[string]any{"page": &types.WikiPage{Content: content}}
	ordinary, err := jsonResult(payload)
	require.NoError(t, err)
	rewritten := f.rewrite(ordinary)
	require.NotEqual(t, content, structuredOf(rewritten)["page"].(map[string]any)["content"], "fixture must resolve canonical references into transport URLs")
	next := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) { return jsonResult(payload) }
	req := coreCall("wiki_workspace", map[string]any{"action": "get", "knowledge_base_id": "kb-1", "slug": "concept/resource"})
	core, err := f.srv.guardCoreTool(next)(f.ctx, req)
	require.NoError(t, err)
	require.False(t, core.IsError, mcpErrorText(core))
	require.Equal(t, content, structuredOf(core)["page"].(*types.WikiPage).Content)
	legacy, err := f.srv.guardTool(next)(f.ctx, req)
	require.NoError(t, err)
	require.False(t, legacy.IsError, mcpErrorText(legacy))
	require.Equal(t, content, structuredOf(legacy)["page"].(*types.WikiPage).Content)
}
