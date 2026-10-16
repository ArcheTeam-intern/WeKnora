package mcpserver

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// The legacy handlers wrap typed structs straight into their structured
// payload map. structuredOf only asserts the outer map, so every nested read
// has to go through structuredValue; a plain `.(map[string]any)` assertion
// silently yields nothing. That mismatch is what made the core ingest status
// report unknown phases and an empty document id, so both shapes are pinned
// here.

func TestStructuredValueNormalizesTypedPayloads(t *testing.T) {
	report := documentStatusReport{
		KnowledgeID: "doc-1",
		ParseStatus: "completed",
		IndexStatus: "completed",
		FileSize:    1234,
	}
	outer, ok := structuredValue(map[string]any{"status": report}).(map[string]any)
	require.True(t, ok, "a typed struct must normalise to a map")
	normalised, ok := outer["status"].(map[string]any)
	require.True(t, ok, "the nested status must normalise too")
	require.Equal(t, "doc-1", normalised["knowledge_id"])
	require.Equal(t, "completed", normalised["parse_status"])
	require.EqualValues(t, 1234, normalised["file_size"])

	summary := documentSummary{ID: "doc-2", Title: "acceptance-record.pdf", ParseStatus: "processing"}
	outer, ok = structuredValue(map[string]any{"document": summary}).(map[string]any)
	require.True(t, ok)
	normalised, ok = outer["document"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "doc-2", normalised["id"])

	// Values that are already generic keep their shape.
	already := map[string]any{"id": "doc-3"}
	normalised, ok = structuredValue(already).(map[string]any)
	require.True(t, ok)
	require.Equal(t, "doc-3", normalised["id"])

	// Slices of structs arrive as generic lists.
	list, ok := structuredValue([]documentSummary{{ID: "a"}, {ID: "b"}}).([]any)
	require.True(t, ok)
	require.Len(t, list, 2)

	require.Nil(t, structuredValue(nil))
}

func TestDocumentIDFromImportReadsBothShapes(t *testing.T) {
	require.Equal(t, "doc-typed", documentIDFromImport(map[string]any{
		"document": documentSummary{ID: "doc-typed"},
	}), "the import payload wraps the typed summary")
	require.Equal(t, "doc-map", documentIDFromImport(map[string]any{
		"document": map[string]any{"id": "doc-map"},
	}))
	require.Empty(t, documentIDFromImport(map[string]any{"document": nil}))
	require.Empty(t, documentIDFromImport(nil))
}

func TestIngestStatusPayloadReadsTypedReport(t *testing.T) {
	server := &Server{}
	result := mcp.NewToolResultStructured(map[string]any{
		"knowledge_base_id": "kb-1",
		"status": documentStatusReport{
			KnowledgeID: "doc-9",
			Title:       "acceptance-record.pdf",
			ParseStatus: "completed",
			IndexStatus: "completed",
			FileType:    "pdf",
		},
	}, "text")

	payload, ok := server.ingestDocumentStatusPayload(context.Background(), result)
	require.True(t, ok, "a typed document status report must be readable")
	require.Equal(t, "doc-9", payload["document_id"])
	require.Equal(t, "completed", payload["parse_status"])
	require.Equal(t, "completed", payload["index_status"])

	phases := coreIngestPhases(map[string]any{
		"upload":   map[string]any{"state": "imported"},
		"document": payload,
	})
	require.Equal(t, "imported", phases["upload"])
	require.Equal(t, "completed", phases["parse"])
	require.Equal(t, "completed", phases["index"])

	// An error result carries no document payload: the caller must not read
	// "unknown" phases out of a failure.
	_, ok = server.ingestDocumentStatusPayload(context.Background(), mcp.NewToolResultError("boom"))
	require.False(t, ok)
}
