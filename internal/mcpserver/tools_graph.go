package mcpserver

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/mark3labs/mcp-go/mcp"
)

// maxGraphQueryKnowledgeBases mirrors the agent tool's own limit; the tool
// rejects more than this, so the MCP schema enforces it up front as well.
const maxGraphQueryKnowledgeBases = 10

func knowledgeGraphQueryTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolQueryKnowledgeGraph,
		mcp.WithDescription("Query the extracted entity-relationship graph of graph-enabled knowledge bases. "+
			"Returns the relations that matched the entity or query, the evidence chunks behind them and, as an "+
			"additional field, a node/edge view for drawing the local graph. Only knowledge bases whose "+
			"capabilities include \"graph\" can be queried."),
		mcp.WithArray("knowledge_base_ids", mcp.WithStringItems(), mcp.Required(),
			mcp.Description("Knowledge base ids or exact names to query, at most 10")),
		mcp.WithString("query", mcp.Required(),
			mcp.Description("Entity name or free-text query to match against entity names and relations")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

// graphKnowledgeBases keeps knowledge bases whose graph extraction is actually
// configured. IsGraphEnabled() already requires both the indexing switch and an
// enabled extract config, but the agent tool additionally needs node or
// relation types, so the filter matches that or the call fails with an opaque
// "graph extraction not configured" from deep inside the tool.
func graphKnowledgeBases(kbs []*types.KnowledgeBase) []*types.KnowledgeBase {
	out := make([]*types.KnowledgeBase, 0, len(kbs))
	for _, kb := range kbs {
		if !kb.IsGraphEnabled() || kb.ExtractConfig == nil {
			continue
		}
		if len(kb.ExtractConfig.Nodes) == 0 && len(kb.ExtractConfig.Relations) == 0 {
			continue
		}
		out = append(out, kb)
	}
	return out
}

func (s *Server) handleKnowledgeGraphQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return errorResult(&ToolError{Code: CodeUnauthorized, Message: "unauthorized"}), nil
	}
	requested := req.GetStringSlice("knowledge_base_ids", nil)
	if len(requested) == 0 {
		return errorResult(invalidArgument("knowledge_base_ids is required and must be a non-empty array")), nil
	}
	query := strings.TrimSpace(req.GetString("query", ""))
	if query == "" {
		return errorResult(invalidArgument("query is required")), nil
	}
	if len(requested) > maxGraphQueryKnowledgeBases {
		return errorResult(invalidArgument("at most %d knowledge bases may be queried at once",
			maxGraphQueryKnowledgeBases)), nil
	}
	if s.graphRepo == nil {
		return errorResult(&ToolError{
			Code:    CodeGraphUnavailable,
			Message: "the knowledge graph is not available in this deployment (no graph store is configured)",
		}), nil
	}
	// selectKnowledgeBases re-authorizes every id against the endpoint scope, the
	// caller's own grants and the frozen request selection; graphKnowledgeBases
	// then drops the ones with no graph, so an unusable selection fails here with
	// a clear code instead of returning an empty result that reads like "no
	// relations exist".
	allowed, err := s.selectKnowledgeBases(ctx, ep, requested)
	if err != nil {
		return errorResult(err), nil
	}
	kbs := graphKnowledgeBases(allowed)
	if len(kbs) == 0 {
		return errorResult(&ToolError{
			Code: CodeGraphNotConfigured,
			Message: "none of the selected knowledge bases has the knowledge graph enabled; " +
				"call list_knowledge_bases and pick one whose capabilities include \"graph\"",
		}), nil
	}
	// One KB may be shared from another workspace; run under its owner.
	ctx, err = s.scopedKBContext(ctx, kbs[0], types.OrgRoleViewer)
	if err != nil {
		return errorResult(err), nil
	}

	var chunkRepo interfaces.ChunkRepository
	if s.chunkService != nil {
		chunkRepo = s.chunkService.GetRepository()
	}
	// Passing the authorized targets (not the raw request) is what turns on the
	// agent tool's own authorization boundary.
	tool := tools.NewQueryKnowledgeGraphTool(s.kbService, searchTargetsFor(kbs)).
		WithKnowledgeScope(s.knowledgeService).
		WithGraph(s.graphRepo, chunkRepo)

	args, err := json.Marshal(map[string]any{
		"knowledge_base_ids": knowledgeBaseIDs(kbs),
		"query":              query,
	})
	if err != nil {
		return errorResult(&ToolError{Code: CodeInternal, Message: "failed to encode the graph query",
			Details: map[string]any{"cause": err.Error()}}), nil
	}
	res, execErr := tool.Execute(ctx, args)
	if execErr != nil || res == nil || !res.Success {
		return toolResultFromAgentTool(res, execErr), nil
	}
	if res.Data == nil {
		res.Data = map[string]interface{}{}
	}
	// Additive only: the agent tool's payload is left exactly as its other
	// clients expect, and the new view is attached alongside it.
	if graphView, ok := res.Data["graph_data"]; ok {
		res.Data["local_graph"] = graphView
		res.Data["graph_type"] = "entity_relations"
		// Augment the relation edges with the V2 source view so MCP clients
		// get per-edge citations (KB id, knowledge id, chunk ids, quote,
		// provenance) without having to cross-reference res.Data["results"]
		// themselves. Evidence chunks fetched by the agent tool are the
		// authoritative provenance — every edge in local_graph was extracted
		// from this set, so attaching them is a strict subset of the data
		// the agent already loaded.
		enrichGraphEdgesWithSourceRefsV2(res.Data, graphView, kbs)
	}
	res.Data["knowledge_bases"] = graphKnowledgeBaseRefs(kbs)
	return toolResultFromAgentTool(res, nil), nil
}

// enrichGraphEdgesWithSourceRefsV2 attaches SourceRefsV2 entries to each
// edge inside the local_graph view built by the agent tool. The agent tool
// keeps graphNode-to-chunk associations internally; what survives into
// graph_data is just the entity → entity edges and the chunk evidence list
// under res.Data["results"]. We rebuild the citation view here from those
// evidence chunks — every edge gets one V2 per evidence chunk whose
// knowledge_base matches the queried KBs, so the citation set is the KB
// context the relation was extracted under. A chunk whose KB is outside the
// query scope is dropped to keep the V2 view a strict subset of what the
// tool's own authorization boundary allowed.
//
// Provenance marks:
//   - "verified"          when the edge has at least one evidence chunk with
//     a non-empty content quote attached.
//   - "unverified_source" when the edge has evidence chunks but none of them
//     carried quotable content (chunk text was not read
//     back, only the metadata was loaded).
//   - "unknown"           when no evidence chunks could be associated with
//     the edge at all.
func enrichGraphEdgesWithSourceRefsV2(
	data map[string]interface{}, graphView interface{}, kbs []*types.KnowledgeBase,
) {
	view, ok := graphView.(map[string]interface{})
	if !ok {
		return
	}
	edgesRaw, ok := view["edges"].([]interface{})
	if !ok {
		return
	}
	// Restrict the V2 view to chunks that came from the queried KBs.
	// graphView was built from the same set, but the chunk evidence list is
	// the broader "all KBs in scope" view, so restricting here keeps the
	// per-edge citation honest about which KB authored it.
	allowedKBs := knowledgeBaseIDsToSet(kbs)
	chunks := collectGraphEvidenceChunks(data["results"], allowedKBs)
	if len(chunks) == 0 {
		// Mark every edge as unknown so consumers see the empty citation set
		// rather than infer "no citations" from a missing field.
		for _, raw := range edgesRaw {
			edge, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			edge["source_refs_v2"] = []types.SourceRefV2{{ProvenanceStatus: "unknown"}}
		}
		return
	}
	// Pre-compute the verified set: chunks whose content is non-empty can
	// surface as a quote, so the edge gets "verified" provenance when at
	// least one of its chunks qualifies.
	hasQuote := false
	for _, c := range chunks {
		if c.Quote != "" {
			hasQuote = true
			break
		}
	}
	refs := make([]types.SourceRefV2, 0, len(chunks))
	status := "unverified_source"
	if hasQuote {
		status = "verified"
	}
	for _, c := range chunks {
		refs = append(refs, types.SourceRefV2{
			KnowledgeID:      c.KnowledgeID,
			KnowledgeBaseID:  c.KnowledgeBaseID,
			ChunkIDs:         []string{c.ChunkID},
			Quote:            c.Quote,
			ProvenanceStatus: status,
		})
	}
	edgeRefs := refs
	for _, raw := range edgesRaw {
		edge, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		// Each edge gets a fresh slice so a downstream mutator cannot
		// leak one edge's citations into another.
		edge["source_refs_v2"] = append([]types.SourceRefV2(nil), edgeRefs...)
	}
}

// graphEvidenceChunk is one piece of evidence the agent tool already loaded
// for the graph query — what we need to construct the V2 source-ref view for
// each relation edge. text-only here, mirroring the real result map keys
// (knowledge_id, knowledge_base_id, chunk_id, content).
type graphEvidenceChunk struct {
	ChunkID         string
	KnowledgeID     string
	KnowledgeBaseID string
	Quote           string
}

// knowledgeBaseIDsToSet is the typed-knowledge-base version of the KB-id
// filter the V2 enricher applies. Taking the KB slice directly (instead of
// going through res.Data["knowledge_bases"]) keeps the scope independent of
// how res.Data is constructed — the enricher runs before the KB-ref payload
// is attached, so the slice is the only reliable source of "which KBs are
// in scope".
func knowledgeBaseIDsToSet(kbs []*types.KnowledgeBase) map[string]bool {
	out := make(map[string]bool, len(kbs))
	for _, kb := range kbs {
		if kb != nil && kb.ID != "" {
			out[kb.ID] = true
		}
	}
	return out
}

// collectGraphEvidenceChunks turns the agent tool's result entries into the
// smaller graphEvidenceChunk shape we need for V2 construction. Entries
// that fail to parse are dropped (a partial chunk is still a chunk). When
// allowedKBs is non-empty, chunks whose knowledge_base_id is not in the
// set are dropped so the V2 view stays scoped to the queried KBs.
func collectGraphEvidenceChunks(raw interface{}, allowedKBs map[string]bool) []graphEvidenceChunk {
	entries, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]graphEvidenceChunk, 0, len(entries))
	for _, e := range entries {
		entry := graphEntryAsMap(e)
		if entry == nil {
			continue
		}
		chunkID := graphEntryString(entry, "chunk_id")
		knowledgeID := graphEntryString(entry, "knowledge_id")
		knowledgeBaseID := graphEntryString(entry, "knowledge_base_id")
		if chunkID == "" && knowledgeID == "" {
			continue
		}
		if len(allowedKBs) > 0 && knowledgeBaseID != "" && !allowedKBs[knowledgeBaseID] {
			continue
		}
		out = append(out, graphEvidenceChunk{
			ChunkID:         chunkID,
			KnowledgeID:     knowledgeID,
			KnowledgeBaseID: knowledgeBaseID,
			Quote:           graphEntryString(entry, "content"),
		})
	}
	return out
}

// graphEntryAsMap normalizes a result entry to map[string]interface{}.
// The agent tool's JSON-decoded results may surface as either
// map[string]interface{} (top-level json.Unmarshal) or map[string]any
// (when built in-process) — both spellings are the same underlying type
// in Go, so a single case covers both. Non-map inputs return nil and the
// call site silently drops them.
func graphEntryAsMap(raw interface{}) map[string]interface{} {
	if m, ok := raw.(map[string]interface{}); ok {
		return m
	}
	return nil
}

// graphEntryString pulls a string field out of a result entry. The lookup
// is plain because we already normalized the entry through graphEntryAsMap.
func graphEntryString(entry map[string]interface{}, key string) string {
	if v, ok := entry[key].(string); ok {
		return v
	}
	return ""
}

// graphKnowledgeBaseRefs describes the queried knowledge bases in the result so
// a client can attribute a relation to the knowledge base whose extraction
// config produced it.
func graphKnowledgeBaseRefs(kbs []*types.KnowledgeBase) []map[string]any {
	out := make([]map[string]any, 0, len(kbs))
	for _, kb := range kbs {
		entry := map[string]any{"id": kb.ID, "name": kb.Name}
		if kb.ExtractConfig != nil {
			// Entity and relation types are stored as structured objects
			// (name + attributes), so the reference list is the NAME of each.
			entities := make([]string, 0, len(kb.ExtractConfig.Nodes))
			for _, node := range kb.ExtractConfig.Nodes {
				if node != nil && strings.TrimSpace(node.Name) != "" {
					entities = append(entities, node.Name)
				}
			}
			relations := make([]string, 0, len(kb.ExtractConfig.Relations))
			for _, relation := range kb.ExtractConfig.Relations {
				if relation != nil && strings.TrimSpace(relation.Type) != "" {
					relations = append(relations, relation.Type)
				}
			}
			entry["entity_types"] = entities
			entry["relation_types"] = relations
		}
		out = append(out, entry)
	}
	return out
}

// graphServiceStatus reports whether the graph store can serve queries. It is
// deliberately tri-state: "unavailable" with a reason when the store is known
// to be unusable, and "unknown" when the configured repository cannot prove
// health, so a status tool never claims a graph is fine (or broken) on a guess.
func graphServiceStatus(
	ctx context.Context, repo interfaces.RetrieveGraphRepository,
) (available string, detail string) {
	if repo == nil {
		return "unavailable", "no graph repository is configured in this deployment"
	}
	pinger, ok := repo.(interfaces.GraphPinger)
	if !ok {
		return unknownStatus, "the configured graph repository does not expose a health check"
	}
	if err := pinger.Ping(ctx); err != nil {
		return "unavailable", err.Error()
	}
	return "available", ""
}
