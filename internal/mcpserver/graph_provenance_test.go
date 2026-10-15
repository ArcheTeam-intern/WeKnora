package mcpserver

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestGraphSourcesVerifyEachChunkAndBinding(t *testing.T) {
	edge := map[string]interface{}{"id": "relation", "chunk_ids": []string{"c1", "unread", "foreign"}, "knowledge_id": "d1", "knowledge_base_id": "kb", "quote": "one quote"}
	data := map[string]interface{}{"results": []map[string]interface{}{
		{"chunk_id": "c1", "knowledge_id": "d1", "knowledge_base_id": "kb", "content": "one quote"},
		{"chunk_id": "foreign", "knowledge_id": "d2", "knowledge_base_id": "other", "content": "private quote"},
	}}
	enrichGraphEdgesWithSourceRefsV2(data, map[string]interface{}{"edges": []map[string]interface{}{edge}}, []*types.KnowledgeBase{{ID: "kb"}})
	refs := edge["source_refs_v2"].([]types.SourceRefV2)
	if len(refs) != 3 { t.Fatalf("want per-chunk refs, got %+v", refs) }
	if refs[0].ProvenanceStatus != "verified" || refs[0].Quote != "one quote" { t.Fatalf("readable source = %+v", refs[0]) }
	for _, ref := range refs[1:] {
		if ref.ProvenanceStatus == "verified" || ref.Quote != "" || ref.KnowledgeBaseID == "other" { t.Fatalf("unread/mismatched source leaked evidence: %+v", ref) }
	}
	if edge["id"] != "relation" { t.Fatal("relation identity changed") }
}

// TestUnmappedGraphEdgesMustNotClaimAllQueryEvidence is the plan §8 task 6
// regression: an edge without its own per-relation chunk provenance must not
// borrow the whole query's evidence set. Each unmapped edge exposes an unknown
// provenance entry with no knowledge/chunk ids, never a verified citation.
func TestUnmappedGraphEdgesMustNotClaimAllQueryEvidence(t *testing.T) {
	kbID := "00000000-0000-0000-0000-000000000001"
	data := map[string]interface{}{
		"results": []interface{}{
			map[string]interface{}{"chunk_id": "c1", "knowledge_id": "d1", "knowledge_base_id": kbID, "content": "A 负责 B"},
			map[string]interface{}{"chunk_id": "c2", "knowledge_id": "d2", "knowledge_base_id": kbID, "content": "C 依赖 D"},
		},
	}
	first := map[string]interface{}{"id": "r1", "source": "A", "target": "B"}
	second := map[string]interface{}{"id": "r2", "source": "C", "target": "D"}
	view := map[string]interface{}{"edges": []interface{}{first, second}}
	enrichGraphEdgesWithSourceRefsV2(data, view, []*types.KnowledgeBase{{ID: kbID}})
	for _, edge := range []map[string]interface{}{first, second} {
		refs, ok := edge["source_refs_v2"].([]types.SourceRefV2)
		if !ok || len(refs) == 0 {
			t.Fatal("unmapped edge must explicitly expose unknown provenance")
		}
		for _, ref := range refs {
			if ref.ProvenanceStatus != "unknown" || ref.KnowledgeID != "" || len(ref.ChunkIDs) != 0 {
				t.Fatalf("unmapped edge borrowed query-wide evidence: %+v", ref)
			}
		}
	}
}

// TestMappedGraphEdgesKeepTheirOwnEvidence is the follow-up the plan requires:
// once an edge carries its own chunk provenance, it cites exactly those chunks
// and no other relation's. The edge map keys mirror the per-relation fields the
// enrichment reads (chunk_ids + knowledge_id + quote), so this test pins the
// contract even before the Neo4j write path persists them.
func TestMappedGraphEdgesKeepTheirOwnEvidence(t *testing.T) {
	kbID := "00000000-0000-0000-0000-000000000001"
	data := map[string]interface{}{
		"results": []interface{}{
			map[string]interface{}{"chunk_id": "c1", "knowledge_id": "d1", "knowledge_base_id": kbID, "content": "A 负责 B"},
			map[string]interface{}{"chunk_id": "c2", "knowledge_id": "d2", "knowledge_base_id": kbID, "content": "C 依赖 D"},
		},
	}
	first := map[string]interface{}{"id": "r1", "source": "A", "target": "B",
		"chunk_ids": []interface{}{"c1"}, "knowledge_id": "d1", "knowledge_base_id": kbID, "quote": "A 负责 B"}
	second := map[string]interface{}{"id": "r2", "source": "C", "target": "D",
		"chunk_ids": []interface{}{"c2"}, "knowledge_id": "d2", "knowledge_base_id": kbID, "quote": "C 依赖 D"}
	view := map[string]interface{}{"edges": []interface{}{first, second}}
	enrichGraphEdgesWithSourceRefsV2(data, view, []*types.KnowledgeBase{{ID: kbID}})

	refs1, _ := first["source_refs_v2"].([]types.SourceRefV2)
	if len(refs1) != 1 || refs1[0].KnowledgeID != "d1" || len(refs1[0].ChunkIDs) != 1 || refs1[0].ChunkIDs[0] != "c1" {
		t.Fatalf("r1 must cite only its own chunk d1/c1: %+v", refs1)
	}
	if refs1[0].ProvenanceStatus != "verified" {
		t.Fatalf("r1 has a quote, so its provenance must be verified: %+v", refs1)
	}
	refs2, _ := second["source_refs_v2"].([]types.SourceRefV2)
	if len(refs2) != 1 || refs2[0].KnowledgeID != "d2" || len(refs2[0].ChunkIDs) != 1 || refs2[0].ChunkIDs[0] != "c2" {
		t.Fatalf("r2 must cite only its own chunk d2/c2: %+v", refs2)
	}
}

// TestGraphEdgeWithoutQuoteIsNotVerified pins the per-ref rule: an edge with a
// chunk but no readable quote is unverified_source, never verified just because
// another edge in the same query had a quote.
func TestGraphEdgeWithoutQuoteIsNotVerified(t *testing.T) {
	kbID := "00000000-0000-0000-0000-000000000001"
	data := map[string]interface{}{
		"results": []interface{}{
			map[string]interface{}{"chunk_id": "c1", "knowledge_id": "d1", "knowledge_base_id": kbID, "content": ""},
		},
	}
	edge := map[string]interface{}{"id": "r1", "source": "A", "target": "B",
		"chunk_ids": []interface{}{"c1"}, "knowledge_id": "d1", "knowledge_base_id": kbID, "quote": ""}
	view := map[string]interface{}{"edges": []interface{}{edge}}
	enrichGraphEdgesWithSourceRefsV2(data, view, []*types.KnowledgeBase{{ID: kbID}})
	refs, _ := edge["source_refs_v2"].([]types.SourceRefV2)
	if len(refs) != 1 {
		t.Fatalf("expected one ref, got %+v", refs)
	}
	if refs[0].ProvenanceStatus != "unverified_source" {
		t.Fatalf("no quote must be unverified_source, got %q", refs[0].ProvenanceStatus)
	}
}
