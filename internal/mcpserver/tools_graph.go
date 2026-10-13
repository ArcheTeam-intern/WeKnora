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

// enrichGraphEdgesWithSourceRefsV2 attaches SourceRefsV2 entries to each edge
// inside the local_graph view. Each edge is cited from ITS OWN per-relation
// chunk provenance (chunk_ids + knowledge_id + knowledge_base_id + quote), never
// from the query-wide evidence set: assigning every chunk to every edge would
// make one relation's quote look like another relation's source. An edge that
// carries no per-relation provenance is marked unknown rather than borrowing
// evidence, so a client can tell "this relation has no verified source" from
// "this relation is backed by chunk X".
//
// Provenance is decided per edge (and per ref), not per query:
//   - "verified"          when the edge's own quote is non-empty.
//   - "unverified_source" when the edge has chunks but no readable quote.
//   - "unknown"           when the edge has no per-relation chunk provenance.
func enrichGraphEdgesWithSourceRefsV2(
	data map[string]interface{}, graphView interface{}, _ []*types.KnowledgeBase,
) {
	view, ok := graphView.(map[string]interface{})
	if !ok {
		return
	}
	edges := mapSliceOf(view["edges"])
	if len(edges) == 0 {
		return
	}
	// The agent tool already loaded the evidence chunks into data["results"];
	// index them by chunk id so each edge can cite exactly its own chunks'
	// content and KB, never the whole query's set.
	quotes, kbs := graphChunkIndex(data["results"])
	for _, edge := range edges {
		edge["source_refs_v2"] = graphEdgeRefs(edge, quotes, kbs)
	}
}

// mapSliceOf normalizes a value that may be either []map[string]interface{}
// (the in-process shape the business tool builds) or []interface{} (a
// JSON-decoded shape) into the former. The MCP enrichment must accept both:
// the business tool returns the native slice, and only an HTTP round-trip would
// turn it into []interface{}, which the MCP layer never does for in-process calls.
func mapSliceOf(raw interface{}) []map[string]interface{} {
	switch v := raw.(type) {
	case []map[string]interface{}:
		return v
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(v))
		for _, e := range v {
			if m, ok := e.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

// graphChunkIndex indexes the result entries by chunk id, returning the content
// quote and knowledge_base_id for each. Entries that do not carry a chunk id are
// dropped; a chunk present here but not cited by an edge is simply unused.
func graphChunkIndex(raw interface{}) (quotes, kbs map[string]string) {
	quotes, kbs = map[string]string{}, map[string]string{}
	for _, entry := range mapSliceOf(raw) {
		chunkID := graphEntryString(entry, "chunk_id")
		if chunkID == "" {
			continue
		}
		if content := graphEntryString(entry, "content"); content != "" {
			quotes[chunkID] = content
		}
		if kb := graphEntryString(entry, "knowledge_base_id"); kb != "" {
			kbs[chunkID] = kb
		}
	}
	return quotes, kbs
}

// graphEdgeRefs builds the per-edge source view from the edge's own provenance
// fields, resolving each cited chunk's quote and knowledge base from the indexed
// evidence. An unmapped edge (no chunk ids or knowledge id) returns a single
// unknown ref so the absence of a citation is explicit, not implied.
func graphEdgeRefs(edge map[string]interface{}, quotes, kbs map[string]string) []types.SourceRefV2 {
	chunkIDs := graphEdgeStringSlice(edge, "chunk_ids")
	knowledgeID := graphEntryString(edge, "knowledge_id")
	if len(chunkIDs) == 0 || knowledgeID == "" {
		return []types.SourceRefV2{{ProvenanceStatus: "unknown"}}
	}
	knowledgeBaseID := graphEntryString(edge, "knowledge_base_id")
	quote := graphEntryString(edge, "quote")
	for _, cid := range chunkIDs {
		if quote == "" {
			quote = quotes[cid]
		}
		if knowledgeBaseID == "" {
			knowledgeBaseID = kbs[cid]
		}
	}
	status := "unverified_source"
	if quote != "" {
		status = "verified"
	}
	return []types.SourceRefV2{{
		KnowledgeID:      knowledgeID,
		KnowledgeBaseID:  knowledgeBaseID,
		ChunkIDs:         chunkIDs,
		Quote:            quote,
		ProvenanceStatus: status,
	}}
}

// graphEdgeStringSlice reads a string-slice field that may surface as either
// []interface{} (JSON decode) or []string (in-process build).
func graphEdgeStringSlice(edge map[string]interface{}, key string) []string {
	raw, ok := edge[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// graphEntryString pulls a string field out of an edge map.
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
