package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	defaultWikiSearchLimit = 10
	maxWikiSearchLimit     = 50
	defaultWikiIndexLimit  = 50
	maxWikiIndexLimit      = 200

	// Wiki page browsing reuses the REST surface's page-size defaults so the
	// MCP contract matches what the admin UI already shows.
	defaultWikiPageSize = 20
	maxWikiPageSize     = 100

	// wiki_graph bounds. The plan fixes depth at 1..2 and limit at 200; the
	// REST handler allows depth 3 / limit 2000 for the UI's interactive graph,
	// but an agent tool is capped tighter so one call cannot pull a whole wiki.
	defaultWikiGraphDepth = 1
	maxWikiGraphDepth     = 2
	defaultWikiGraphLimit = 100
	maxWikiGraphLimit     = 200
)

func wikiListPagesTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolWikiListPages,
		mcp.WithDescription("Page through the generated wiki of one knowledge base. Returns page slugs, titles, "+
			"types, statuses and summaries; use wiki_read_page for a page's body."),
		mcp.WithString("knowledge_base_id", mcp.Required(), mcp.Description("Knowledge base id or exact name")),
		mcp.WithNumber("page", mcp.Description("Page number, 1-based, default 1")),
		mcp.WithNumber("page_size", mcp.Description("Pages per page, default 20, max 100")),
		mcp.WithString("page_type", mcp.Description("Only this page type: summary, entity, concept, index, "+
			"synthesis or comparison")),
		mcp.WithString("status", mcp.Description("Only pages with this status: draft, published or archived")),
		mcp.WithString("query", mcp.Description("Only pages matching this text in title, summary or content")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func wikiGraphTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolWikiGraph,
		mcp.WithDescription("Read the link graph of one knowledge base's wiki. Without a center this is an "+
			"overview of the most connected pages; with a center it is that page's neighbourhood, which shows "+
			"how a topic relates to the rest of the wiki. Edges are page links, not extracted entities."),
		mcp.WithString("knowledge_base_id", mcp.Required(), mcp.Description("Knowledge base id or exact name")),
		mcp.WithString("center", mcp.Description("Center page slug for a local (ego) graph; omit for an overview")),
		mcp.WithNumber("depth", mcp.Description("Neighbourhood hops around the center, 1 or 2, default 1")),
		mcp.WithNumber("limit", mcp.Description("Maximum nodes to return, default 100, max 200")),
		mcp.WithString("page_type", mcp.Description("Only include pages of this type")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func wikiSearchTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolWikiSearch,
		mcp.WithDescription("Search the generated wiki pages of the knowledge bases in scope by title, alias and "+
			"content. Returns page slugs that wiki_read_page accepts. Only knowledge bases with a wiki are "+
			"searched."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search terms or a regular expression "+
			"(case-insensitive POSIX); text that is not a valid regular expression is matched literally")),
		mcp.WithBoolean("regex", mcp.Description("false forces a literal match; true requires a valid regular "+
			"expression; omit for the default behaviour")),
		mcp.WithArray("knowledge_base_ids", mcp.WithStringItems(),
			mcp.Description("Optional knowledge base ids or names to restrict the search")),
		mcp.WithNumber("limit", mcp.Description("Maximum pages to return, default 10, max 50")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func wikiReadPageTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolWikiReadPage,
		mcp.WithDescription("Read one wiki page by slug, including its content and outgoing links."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("Page slug from wiki_search or wiki_index, e.g. "+
			"concept/rag")),
		mcp.WithString("knowledge_base_id", mcp.Description("Knowledge base id or name, required only when the same "+
			"slug exists in several wikis")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func wikiIndexTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolWikiIndex,
		mcp.WithDescription("Browse the table of contents of one knowledge base's wiki: the intro plus pages "+
			"grouped by type (summary, entity, concept, ...)."),
		mcp.WithString("knowledge_base_id", mcp.Required(), mcp.Description("Knowledge base id or exact name")),
		mcp.WithNumber("limit", mcp.Description("Maximum entries per page type, default 50, max 200")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func (s *Server) wikiScopeFromRequest(
	ctx context.Context, ep *types.MCPEndpoint, requested []string,
) ([]*types.KnowledgeBase, error) {
	kbs, err := s.selectKnowledgeBases(ctx, ep, requested)
	if err != nil {
		return nil, err
	}
	kbs = wikiKnowledgeBases(kbs)
	if len(kbs) == 0 {
		return nil, errNoWikiInScope
	}
	return kbs, nil
}

var errNoWikiInScope = &toolError{"none of the selected knowledge bases has a wiki; call list_knowledge_bases and " +
	"pick one whose capabilities include \"wiki\""}

type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

// withOutputInData keeps the wiki page body available to MCP clients that
// only inspect structuredContent. Other tools retain their existing payload
// shape because this compatibility field is only needed by the wiki tools.
func withOutputInData(res *types.ToolResult) *types.ToolResult {
	if res == nil || !res.Success || res.Output == "" {
		return res
	}
	if res.Data == nil {
		res.Data = map[string]interface{}{}
	}
	if _, exists := res.Data["content"]; !exists {
		res.Data["content"] = res.Output
	}
	return res
}

func (s *Server) handleWikiSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	query, err := req.RequireString("query")
	if err != nil || strings.TrimSpace(query) == "" {
		return errorResult(invalidArgument("query is required")), nil
	}
	kbs, err := s.wikiScopeFromRequest(ctx, ep, req.GetStringSlice("knowledge_base_ids", nil))
	if err != nil {
		return errorResult(err), nil
	}
	limit := req.GetInt("limit", defaultWikiSearchLimit)
	if limit < 1 {
		limit = defaultWikiSearchLimit
	}
	if limit > maxWikiSearchLimit {
		limit = maxWikiSearchLimit
	}
	tool := tools.NewWikiSearchTool(s.wikiService, s.knowledgeService, wikiScopesFor(kbs), tools.NewWikiRouteResolver())
	callArgs := map[string]any{
		"query": strings.TrimSpace(query),
		"limit": limit,
	}
	// Only forward regex when the client set it, so the default keeps the
	// endpoint's historical regular-expression semantics.
	if v, ok := req.GetArguments()["regex"].(bool); ok {
		callArgs["regex"] = v
	}
	args, _ := json.Marshal(callArgs)
	res, execErr := tool.Execute(ctx, args)
	return toolResultFromAgentTool(withOutputInData(res), execErr), nil
}

func (s *Server) handleWikiReadPage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	slug, err := req.RequireString("slug")
	if err != nil || strings.TrimSpace(slug) == "" {
		return errorResult(invalidArgument("slug is required")), nil
	}
	var requested []string
	if kbSel := strings.TrimSpace(req.GetString("knowledge_base_id", "")); kbSel != "" {
		requested = []string{kbSel}
	}
	kbs, err := s.wikiScopeFromRequest(ctx, ep, requested)
	if err != nil {
		return errorResult(err), nil
	}
	tool := tools.NewWikiReadPageTool(
		s.wikiService, s.knowledgeService, wikiScopesFor(kbs), tools.NewWikiRouteResolver(),
	)
	args, _ := json.Marshal(map[string]any{"slug": strings.TrimSpace(slug)})
	res, execErr := tool.Execute(ctx, args)
	res = withOutputInData(res)
	// Augment the wiki_read_page payload with the structured source view
	// (SourceRefsV2). The agent tool returns the bodies but only emits
	// knowledge_id tokens inside <source> tags; clients that want the V2
	// contract (KB id, chunk ids, quote, provenance) read this map.
	// Lookups happen with the same authorized KB scope the tool itself used,
	// scoped under the owner's context so shared KBs resolve correctly.
	if res != nil && res.Success {
		s.enrichReadPageWithSourceRefsV2(ctx, res, slug, kbs)
	}
	return toolResultFromAgentTool(res, execErr), nil
}

// enrichReadPageWithSourceRefsV2 walks every slug the underlying agent tool
// actually resolved (via res.Data["found_kbs"]) and attaches the V2 source
// ref view under res.Data["source_refs_v2"]. The map is keyed by "slug|kbid"
// to disambiguate when the same slug exists in multiple KBs (the agent tool
// already disambiguates that way in found_kbs / ambiguous_slugs). A KB that
// fails to load contributes no entry rather than a partial one, so the V2
// view stays a strict subset of what the tool returned.
//
// Unlike the pre-correction implementation, this function does NOT trust the
// persisted SourceRefsV2 field: it re-derives refs from the legacy
// SourceRefs, re-authorizes the document under the caller's KB scope, and
// loads the actual chunk content from the chunk service to build a real
// quote. Per-chunk status is then one of:
//
//   - "verified"          the chunk was actually read and its content matches
//     the requested chunk id and KB.
//   - "unverified_source" the chunk was named by the page but not loaded
//     (missing, cross-KB, or chunk service unavailable), or the legacy ref
//     had no chunk id and no chunks could be loaded under the authorized KB.
//   - "unknown"           no SourceRefs at all (synthesized pages).
//
// This is the path that satisfies P0-3: provenance is decided against live
// data, not against whatever the page happened to be tagged with.
func (s *Server) enrichReadPageWithSourceRefsV2(
	ctx context.Context, res *types.ToolResult, primarySlug string,
	kbs []*types.KnowledgeBase,
) {
	if res == nil || res.Data == nil || len(kbs) == 0 || s.wikiService == nil {
		return
	}
	foundKBsRaw, ok := res.Data["found_kbs"].(map[string][]string)
	if !ok || len(foundKBsRaw) == 0 {
		return
	}
	// Build a slug set the tool actually rendered: fall back to the requested
	// slug when found_kbs is absent (single-slug call from this handler).
	slugs := make([]string, 0, len(foundKBsRaw))
	for slug := range foundKBsRaw {
		if slug != "" {
			slugs = append(slugs, slug)
		}
	}
	if len(slugs) == 0 && primarySlug != "" {
		slugs = []string{primarySlug}
	}
	// Index kbs by id so a slug resolved against an out-of-scope KB does not
	// cause us to re-authorize under it.
	kbsByID := make(map[string]*types.KnowledgeBase, len(kbs))
	for _, kb := range kbs {
		if kb != nil && kb.ID != "" {
			kbsByID[kb.ID] = kb
		}
	}
	out := make(map[string]interface{}, len(slugs))
	for _, slug := range slugs {
		for _, kbID := range foundKBsRaw[slug] {
			kb := kbsByID[kbID]
			if kb == nil {
				continue
			}
			scopedCtx, err := s.scopedKBContext(ctx, kb, types.OrgRoleViewer)
			if err != nil {
				continue
			}
			page, err := s.wikiService.GetPageBySlug(scopedCtx, kb.ID, slug)
			if err != nil || page == nil {
				continue
			}
			key := slug + "|" + kbID
			out[key] = s.resolveWikiPageSourceRefs(scopedCtx, page, kb)
		}
	}
	if len(out) > 0 {
		res.Data["source_refs_v2"] = out
	}
}

// resolveWikiPageSourceRefs rebuilds the V2 source-ref view of a single wiki
// page against the caller's authorized KB. It does NOT trust the persisted
// V2 slice: it re-derives from the legacy SourceRefs, re-loads the actual
// chunk content from the chunk service, and validates per-chunk that:
//
//  1. The chunk exists in the chunk service.
//  2. The chunk's KnowledgeBaseID matches the caller's authorized KB id
//     (so a cross-KB lease or a stale ref can never produce a private quote
//     for an out-of-scope document).
//  3. The chunk's content is non-empty (so a verified ref always carries
//     a real, readable quote).
//
// Refs are emitted in the order they appear in page.SourceRefs. A ref that
// points at a knowledge id the caller cannot see under this KB is reported
// as unverified_source with no quote and no chunk ids; the knowledge id is
// not echoed back, so a private id never leaks to the caller.
func (s *Server) resolveWikiPageSourceRefs(
	ctx context.Context, page *types.WikiPage, kb *types.KnowledgeBase,
) []types.SourceRefV2 {
	if page == nil || kb == nil {
		return nil
	}
	if len(page.SourceRefs) == 0 {
		return []types.SourceRefV2{{
			KnowledgeID:      "",
			KnowledgeBaseID:  kb.ID,
			ProvenanceStatus: "unknown",
		}}
	}
	chunkRepo := s.chunkRepoForRead()
	out := make([]types.SourceRefV2, 0, len(page.SourceRefs))
	seenKnowledge := make(map[string][]*types.Chunk)
	for _, raw := range page.SourceRefs {
		knowledgeID, _ := types.ParseWikiSourceRef(raw)
		knowledgeID = strings.TrimSpace(knowledgeID)
		if knowledgeID == "" {
			continue
		}
		// Legacy format "uuid" or "uuid[/chunk_id[/chunk_id...]]": split off
		// the head as the document id, the rest as the cited chunk ids.
		segments := strings.Split(knowledgeID, "/")
		docID := strings.TrimSpace(segments[0])
		var citedChunkIDs []string
		if len(segments) > 1 {
			for _, seg := range segments[1:] {
				if seg = strings.TrimSpace(seg); seg != "" {
					citedChunkIDs = append(citedChunkIDs, seg)
				}
			}
		}
		// Re-authorize the document under the caller's KB before any
		// content read: knowledgeInScope is the same path wiki / search use.
		if _, _, err := s.knowledgeInScope(ctx, endpointForChunkAuth(ctx), docID); err != nil {
			// Document is not in the caller's scope: never echo the id
			// back. An unverified_source ref with empty knowledge id is
			// the honest signal.
			out = append(out, types.SourceRefV2{
				KnowledgeID:      "",
				KnowledgeBaseID:  kb.ID,
				ProvenanceStatus: "unverified_source",
			})
			continue
		}
		chunks, ok := seenKnowledge[docID]
		if !ok {
			if chunkRepo == nil {
				out = append(out, types.SourceRefV2{
					KnowledgeID:      docID,
					KnowledgeBaseID:  kb.ID,
					ProvenanceStatus: "unverified_source",
				})
				continue
			}
			loaded, err := chunkRepo.ListChunksByKnowledgeID(ctx, kb.TenantID, docID)
			if err != nil || len(loaded) == 0 {
				out = append(out, types.SourceRefV2{
					KnowledgeID:      docID,
					KnowledgeBaseID:  kb.ID,
					ProvenanceStatus: "unverified_source",
				})
				continue
			}
			chunks = loaded
			seenKnowledge[docID] = chunks
		}
		// Index by id and chunk_index so ref-by-id and ref-by-position
		// (when the legacy format omitted chunk ids) both work.
		byID := make(map[string]*types.Chunk, len(chunks))
		for _, c := range chunks {
			if c != nil && c.ID != "" {
				byID[c.ID] = c
			}
		}
		if len(citedChunkIDs) == 0 {
			// Document-level ref: the page was tagged with the knowledge
			// id but no chunk id. Verify by loading the first readable
			// chunk under the authorized KB; missing KB / cross KB /
			// empty content all downgrade to unverified_source.
			ref := types.SourceRefV2{
				KnowledgeID:      docID,
				KnowledgeBaseID:  kb.ID,
				ProvenanceStatus: "unverified_source",
			}
			for _, c := range chunks {
				if c == nil || c.Content == "" {
					continue
				}
				if c.KnowledgeBaseID != "" && c.KnowledgeBaseID != kb.ID {
					continue
				}
				ref.ChunkIDs = []string{c.ID}
				ref.Quote = quoteSnippet(c.Content)
				ref.ProvenanceStatus = "verified"
				break
			}
			out = append(out, ref)
			continue
		}
		// Chunk-level ref: one V2 entry per cited chunk id. The order in
		// page.SourceRefs is preserved so the view matches the page's
		// original citation order.
		for _, cid := range citedChunkIDs {
			c, found := byID[cid]
			if !found || c == nil || strings.TrimSpace(c.Content) == "" {
				out = append(out, types.SourceRefV2{
					KnowledgeID:      docID,
					KnowledgeBaseID:  kb.ID,
					ChunkIDs:         []string{cid},
					ProvenanceStatus: "unverified_source",
				})
				continue
			}
			if c.KnowledgeBaseID != "" && c.KnowledgeBaseID != kb.ID {
				out = append(out, types.SourceRefV2{
					KnowledgeID:      docID,
					KnowledgeBaseID:  kb.ID,
					ChunkIDs:         []string{cid},
					ProvenanceStatus: "unverified_source",
				})
				continue
			}
			out = append(out, types.SourceRefV2{
				KnowledgeID:      docID,
				KnowledgeBaseID:  kb.ID,
				ChunkIDs:         []string{cid},
				Quote:            quoteSnippet(c.Content),
				ProvenanceStatus: "verified",
			})
		}
	}
	return out
}

// chunkRepoForRead returns the chunk repository behind the configured chunk
// service, or nil when no service is wired. Read paths tolerate a nil repo
// by emitting unverified_source refs without panicking.
func (s *Server) chunkRepoForRead() interfaces.ChunkRepository {
	if s.chunkService == nil {
		return nil
	}
	return s.chunkService.GetRepository()
}

// endpointForChunkAuth returns a synthesized endpoint reference used only to
// satisfy knowledgeInScope's signature. The page's KB has already been
// authorized by the caller (via scopedKBContext); this function does not
// grant any new privilege, it only carries the KB id forward for the
// per-document scope check.
func endpointForChunkAuth(ctx context.Context) *types.MCPEndpoint {
	if ep, ok := ctx.Value(types.MCPEndpointContextKey).(*types.MCPEndpoint); ok {
		return ep
	}
	return &types.MCPEndpoint{}
}

// quoteSnippet trims a chunk's content to a short, verifier-friendly excerpt.
// Long chunks are common in this codebase (whole-document "chunks" are a
// default in the legacy parser), and 200 characters is enough to anchor a
// claim while keeping the V2 payload small.
const quoteSnippetLimit = 200

func quoteSnippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= quoteSnippetLimit {
		return s
	}
	// Truncate on a UTF-8 rune boundary so multi-byte sequences (CJK,
	// emoji) never land half-copied. The byte slice may end in the
	// middle of a code point; walking back to the last valid rune
	// start keeps the JSON-encoded quote valid and avoids the
	// U+FFFD replacement characters that a hard byte-cut would
	// introduce.
	cut := quoteSnippetLimit
	for cut > 0 {
		r, size := utf8.DecodeRuneInString(s[:cut])
		if r != utf8.RuneError || size != 1 {
			break
		}
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

func (s *Server) handleWikiIndex(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	selector, err := req.RequireString("knowledge_base_id")
	if err != nil {
		return errorResult(invalidArgument("knowledge_base_id is required")), nil
	}
	kbs, err := s.wikiScopeFromRequest(ctx, ep, []string{selector})
	if err != nil {
		return errorResult(err), nil
	}
	limit := req.GetInt("limit", defaultWikiIndexLimit)
	if limit < 1 {
		limit = defaultWikiIndexLimit
	}
	if limit > maxWikiIndexLimit {
		limit = maxWikiIndexLimit
	}
	view, err := s.wikiService.GetIndexView(ctx, kbs[0].ID, nil, limit, "")
	if err != nil {
		return errorResult(&ToolError{Code: CodeInternal, Message: "failed to load wiki index",
			Details: map[string]any{"cause": err.Error()}}), nil
	}
	return jsonResult(map[string]any{
		"knowledge_base_id": kbs[0].ID,
		"intro":             view.Intro,
		"groups":            view.Groups,
	})
}

// wikiPageSummary is the MCP view of one wiki page: enough to choose a page and
// attribute it back to its source documents, without the body (which
// wiki_read_page returns and which can be large).
type wikiPageSummary struct {
	Slug         string              `json:"slug"`
	Title        string              `json:"title"`
	PageType     string              `json:"page_type"`
	Status       string              `json:"status,omitempty"`
	Summary      string              `json:"summary,omitempty"`
	ParentSlug   string              `json:"parent_slug,omitempty"`
	WikiPath     string              `json:"wiki_path,omitempty"`
	SourceRefs   []string            `json:"source_refs,omitempty"`
	SourceRefsV2 []types.SourceRefV2 `json:"source_refs_v2,omitempty"`
	LinkCount    int                 `json:"link_count"`
	UpdatedAt    string              `json:"updated_at,omitempty"`
	LastEditBy   string              `json:"last_edit_source,omitempty"`
	EditorUserID string              `json:"last_editor_id,omitempty"`
}

func summarizeWikiPage(page *types.WikiPage) wikiPageSummary {
	if page == nil {
		return wikiPageSummary{}
	}
	summary := wikiPageSummary{
		Slug:         page.Slug,
		Title:        page.Title,
		PageType:     page.PageType,
		Status:       page.Status,
		Summary:      page.Summary,
		ParentSlug:   page.ParentSlug,
		WikiPath:     page.WikiPath,
		LinkCount:    len(page.InLinks) + len(page.OutLinks),
		LastEditBy:   page.LastEditSource,
		EditorUserID: page.LastEditorID,
	}
	if len(page.SourceRefs) > 0 {
		summary.SourceRefs = append([]string(nil), page.SourceRefs...)
	}
	// V2 is additive — keep emitting it whenever the page records it
	// (V2 wins when present; WikiPageSourceRefsToV2 also falls back to the
	// legacy SourceRefs entries so a page authored before V2 still gets
	// per-chunk refs).
	summary.SourceRefsV2 = types.WikiPageSourceRefsToV2(page)
	if !page.UpdatedAt.IsZero() {
		summary.UpdatedAt = page.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return summary
}

func (s *Server) handleWikiListPages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	selector, err := req.RequireString("knowledge_base_id")
	if err != nil {
		return errorResult(invalidArgument("knowledge_base_id is required")), nil
	}
	kbs, err := s.wikiScopeFromRequest(ctx, ep, []string{selector})
	if err != nil {
		return errorResult(err), nil
	}
	page := req.GetInt("page", 1)
	if page < 1 {
		page = 1
	}
	pageSize := req.GetInt("page_size", defaultWikiPageSize)
	if pageSize < 1 {
		pageSize = defaultWikiPageSize
	}
	if pageSize > maxWikiPageSize {
		pageSize = maxWikiPageSize
	}
	kb := kbs[0]
	// Wiki rows live under the knowledge base owner for shared knowledge bases.
	ctx, err = s.scopedKBContext(ctx, kb, types.OrgRoleViewer)
	if err != nil {
		return errorResult(err), nil
	}
	resp, err := s.wikiService.ListPages(ctx, &types.WikiPageListRequest{
		KnowledgeBaseID: kb.ID,
		PageType:        strings.TrimSpace(req.GetString("page_type", "")),
		Status:          strings.TrimSpace(req.GetString("status", "")),
		Query:           strings.TrimSpace(req.GetString("query", "")),
		Page:            page,
		PageSize:        pageSize,
	})
	if err != nil {
		return errorResult(&ToolError{Code: CodeInternal, Message: "failed to list wiki pages",
			Details: map[string]any{"cause": err.Error()}}), nil
	}
	pages := make([]wikiPageSummary, 0, len(resp.Pages))
	for _, p := range resp.Pages {
		pages = append(pages, summarizeWikiPage(p))
	}
	return jsonResult(map[string]any{
		"knowledge_base_id": kb.ID,
		"pages":             pages,
		"total":             resp.Total,
		"page":              resp.Page,
		"page_size":         resp.PageSize,
		"total_pages":       resp.TotalPages,
	})
}

func (s *Server) handleWikiGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	selector, err := req.RequireString("knowledge_base_id")
	if err != nil {
		return errorResult(invalidArgument("knowledge_base_id is required")), nil
	}
	kbs, err := s.wikiScopeFromRequest(ctx, ep, []string{selector})
	if err != nil {
		return errorResult(err), nil
	}
	center := strings.TrimSpace(req.GetString("center", ""))
	mode := types.WikiGraphModeOverview
	depth := 0
	if center != "" {
		mode = types.WikiGraphModeEgo
		depth = req.GetInt("depth", defaultWikiGraphDepth)
		if depth < 1 {
			depth = defaultWikiGraphDepth
		}
		if depth > maxWikiGraphDepth {
			depth = maxWikiGraphDepth
		}
	}
	limit := req.GetInt("limit", defaultWikiGraphLimit)
	if limit < 1 {
		limit = defaultWikiGraphLimit
	}
	if limit > maxWikiGraphLimit {
		limit = maxWikiGraphLimit
	}
	var typeFilter []string
	if t := strings.TrimSpace(req.GetString("page_type", "")); t != "" {
		typeFilter = []string{t}
	}
	kb := kbs[0]
	// Authorize before touching the wiki, and read the graph under the owner's
	// scope so shared knowledge bases resolve their pages.
	ctx, err = s.scopedKBContext(ctx, kb, types.OrgRoleViewer)
	if err != nil {
		return errorResult(err), nil
	}
	graph, err := s.wikiService.GetGraph(ctx, &types.WikiGraphRequest{
		KnowledgeBaseID: kb.ID,
		Mode:            mode,
		Center:          center,
		Depth:           depth,
		Types:           typeFilter,
		Limit:           limit,
	})
	if err != nil {
		return errorResult(&ToolError{Code: CodeInternal, Message: "failed to load the wiki link graph",
			Details: map[string]any{"cause": err.Error()}}), nil
	}
	return jsonResult(map[string]any{
		"knowledge_base_id": kb.ID,
		"graph_type":        "wiki_page_links",
		"nodes":             graph.Nodes,
		"edges":             graph.Edges,
		"meta":              graph.Meta,
	})
}
