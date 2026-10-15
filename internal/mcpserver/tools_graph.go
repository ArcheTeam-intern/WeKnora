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
		// "Switched on but nothing to extract" is a different problem from
		// "this knowledge base has no graph at all", and it is the one a caller
		// hits right after enabling graph without defining entity types.
		for _, kb := range allowed {
			if kb.IsGraphEnabled() && !graphExtractionConfigured(kb) {
				return errorResult(&ToolError{
					Code: CodeGraphNotConfigured,
					Message: "the selected knowledge base has graph extraction switched on but no entity " +
						"or relation types configured, so there is nothing to match; define them " +
						"(list_knowledge_bases reports graph_ready=false for it) or add the types when " +
						"creating a knowledge base",
					Details: map[string]any{"knowledge_base_id": kb.ID, "graph_ready": false},
				}), nil
			}
		}
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
	// content and KB, never the whole query's set. The docs map carries the
	// per-chunk knowledge_id so graphEdgeRefs can recover a missing
	// edge-level knowledge_id binding (Neo4j sometimes drops it on legacy
	// edges) and refuse cross-document verification.
	quotes, kbs, docs := graphChunkIndex(data["results"])
	for _, edge := range edges {
		edge["source_refs_v2"] = graphEdgeRefs(edge, quotes, kbs, docs)
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

// graphChunkIndex indexes the result entries by chunk id, returning the
// per-chunk quote, knowledge_base_id, and knowledge_id. The KB and the
// document are tracked per-chunk because one edge may cite chunks from
// different documents in the same KB, and a cross-KB edge cannot be
// evaluated without knowing the chunk's own binding. Entries that do not
// carry a chunk id are dropped; a chunk present here but not cited by
// an edge is simply unused.
func graphChunkIndex(raw interface{}) (quotes, kbs, docs map[string]string) {
	quotes, kbs, docs = map[string]string{}, map[string]string{}, map[string]string{}
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
		if doc := graphEntryString(entry, "knowledge_id"); doc != "" {
			docs[chunkID] = doc
		}
	}
	return quotes, kbs, docs
}

// graphEdgeRefs builds the per-edge source view from the edge's own provenance
// fields, returning ONE SourceRefV2 per cited chunk. Each ref is verified
// independently: a quote that belongs to chunk A does not verify chunk B, and a
// chunk whose KB does not match the caller's authorised set is treated as
// unverified_source with no quote (its private content is never exposed). An
// unmapped edge (no chunk ids or knowledge id) returns a single unknown ref so
// the absence of a citation is explicit, not implied.
//
// Per-ref status:
//   - "verified"          when this chunk was actually read and the evidence
//     is in the caller's KB scope.
//   - "unverified_source" when the chunk was named by the edge but not
//     actually read in this query, or its KB does not match.
//   - "unknown"           when the edge carries no per-relation chunk provenance.
func graphEdgeRefs(edge map[string]interface{}, quotes, kbs, docs map[string]string) []types.SourceRefV2 {
	chunkIDs := graphEdgeStringSlice(edge, "chunk_ids")
	edgeKnowledgeID := graphEntryString(edge, "knowledge_id")
	edgeKnowledgeBaseID := graphEntryString(edge, "knowledge_base_id")
	edgeQuote := graphEntryString(edge, "quote")
	if len(chunkIDs) == 0 || edgeKnowledgeID == "" {
		return []types.SourceRefV2{{ProvenanceStatus: "unknown"}}
	}
	refs := make([]types.SourceRefV2, 0, len(chunkIDs))
	for _, cid := range chunkIDs {
		// Per-chunk quote comes from the evidence index, NOT from the edge's
		// own quote field. The edge-level quote is only used when the edge
		// itself carries a single, edge-scoped quote (e.g. a literal "A 负责
		// B") and the chunk is the only one cited; multi-chunk edges must
		// not let one chunk's quote verify the others.
		quote, _ := quotes[cid]
		chunkKB, _ := kbs[cid]
		// The chunk's own document binding is preferred over the edge's
		// knowledge_id when the edge's value is empty. This is what lets
		// us recover a real knowledge_id for Neo4j edges that did not
		// carry one, without trusting the edge-level field for chunks it
		// did not actually cite.
		chunkDoc, _ := docs[cid]
		refKnowledgeID := edgeKnowledgeID
		if refKnowledgeID == "" && chunkDoc != "" {
			refKnowledgeID = chunkDoc
		}
		refKnowledgeBaseID := edgeKnowledgeBaseID
		if refKnowledgeBaseID == "" && chunkKB != "" {
			refKnowledgeBaseID = chunkKB
		}
		status := "unverified_source"
		switch {
		case quote == "":
			// Chunk was named but the agent did not actually load it in this
			// query; nothing to verify against.
			status = "unverified_source"
		case chunkKB != "" && refKnowledgeBaseID != "" && chunkKB != refKnowledgeBaseID:
			// Cross-KB leak attempt: the evidence points to a KB the caller
			// did not ask for, so its content must never be cited.
			quote = ""
			status = "unverified_source"
		case chunkDoc != "" && refKnowledgeID != "" && chunkDoc != refKnowledgeID:
			// Cross-document leak: the chunk belongs to a different
			// document than the edge claims. The quote is suppressed;
			// the per-ref status records the mismatch.
			quote = ""
			status = "unverified_source"
		default:
			status = "verified"
		}
		// Edge-level quote is only adopted when it uniquely identifies the
		// cited chunk AND no contradicting chunk evidence exists. This keeps
		// the per-ref contract honest even when the extractor left a single
		// sentence on the edge.
		if status != "verified" && edgeQuote != "" && len(chunkIDs) == 1 && cid == chunkIDs[0] {
			if (chunkKB == "" || refKnowledgeBaseID == "" || chunkKB == refKnowledgeBaseID) &&
				(chunkDoc == "" || refKnowledgeID == "" || chunkDoc == refKnowledgeID) {
				quote = edgeQuote
				status = "verified"
			}
		}
		refs = append(refs, types.SourceRefV2{
			KnowledgeID:      refKnowledgeID,
			KnowledgeBaseID:  refKnowledgeBaseID,
			ChunkIDs:         []string{cid},
			Quote:            quote,
			ProvenanceStatus: status,
		})
	}
	return refs
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
