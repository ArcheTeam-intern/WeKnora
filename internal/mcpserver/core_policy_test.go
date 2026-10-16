package mcpserver

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// TestCoreSearchLiteralRequiresGrepPermission is the permanent form of the
// review counterexample: an endpoint that allows search_knowledge but not
// grep_chunks must not be able to reach the literal path, and an endpoint that
// allows grep_chunks must keep working.
func TestCoreSearchLiteralRequiresGrepPermission(t *testing.T) {
	plain := &types.MCPEndpoint{Tools: types.StringArray{
		types.MCPEndpointToolSearchKnowledge,
	}}
	literalPerm, err := resolveCoreAction(CoreToolSearch, CoreActionSingle,
		map[string]any{"query": "x", "match": "literal"})
	require.NoError(t, err)
	require.Contains(t, literalPerm.Tools, types.MCPEndpointToolGrepChunks,
		"the literal path runs grep_chunks, so that permission must be required")
	require.Equal(t, []string{types.MCPEndpointToolGrepChunks},
		missingCorePermissions(plain, literalPerm),
		"an endpoint without grep_chunks must be refused by name")

	both := &types.MCPEndpoint{Tools: types.StringArray{
		types.MCPEndpointToolSearchKnowledge, types.MCPEndpointToolGrepChunks,
	}}
	require.Empty(t, missingCorePermissions(both, literalPerm))

	// The default (ranked) path must not require the grep permission, so the
	// fix cannot be "always require both".
	rankedPerm, err := resolveCoreAction(CoreToolSearch, CoreActionSingle, map[string]any{"query": "x"})
	require.NoError(t, err)
	require.Empty(t, missingCorePermissions(plain, rankedPerm))
	require.NotContains(t, rankedPerm.Tools, types.MCPEndpointToolGrepChunks)
}

// TestCoreSearchMatchValues pins the match contract: only the empty default and
// the documented literal mode are accepted. An unsupported mode must be refused
// with an invalid-argument error rather than silently answered with ranked
// retrieval.
func TestCoreSearchMatchValues(t *testing.T) {
	for _, match := range []any{"regex", "exact", "LITERAL ", 7, map[string]any{}} {
		_, err := resolveCoreAction(CoreToolSearch, CoreActionSingle,
			map[string]any{"query": "x", "match": match})
		require.Errorf(t, err, "match=%v must be refused", match)
	}
	// An explicitly empty value means "use the default mode" and stays allowed.
	perm, err := resolveCoreAction(CoreToolSearch, CoreActionSingle,
		map[string]any{"query": "x", "match": " "})
	require.NoError(t, err)
	require.NotContains(t, perm.Tools, types.MCPEndpointToolGrepChunks)
}

// TestCoreSourcePermissionsStayIndependent guards the other half of the review
// finding: fixing the document branch (or the literal gate) must not let an
// endpoint reach the wiki or the graph without their own permissions.
func TestCoreSourcePermissionsStayIndependent(t *testing.T) {
	documentOnly := &types.MCPEndpoint{Tools: types.StringArray{
		types.MCPEndpointToolSearchKnowledge,
		types.MCPEndpointToolListDocuments,
		types.MCPEndpointToolReadDocument,
	}}

	wikiSearch, err := resolveCoreAction(CoreToolSearch, CoreActionSingle,
		map[string]any{"query": "x", "sources": []any{"document", "wiki"}})
	require.NoError(t, err)
	require.Equal(t, []string{types.MCPEndpointToolWikiSearch}, missingCorePermissions(documentOnly, wikiSearch),
		"asking for the wiki source must require wiki_search even when the document source is allowed")

	graphSearch, err := resolveCoreAction(CoreToolSearch, CoreActionSingle,
		map[string]any{"query": "x", "sources": []any{"graph"}})
	require.NoError(t, err)
	require.Equal(t, []string{types.MCPEndpointToolQueryKnowledgeGraph},
		missingCorePermissions(documentOnly, graphSearch))

	wikiRead, err := resolveCoreAction(CoreToolRead, CoreActionRead,
		map[string]any{"source_type": "wiki", "slug": "x"})
	require.NoError(t, err)
	require.Equal(t, []string{types.MCPEndpointToolWikiReadPage},
		missingCorePermissions(documentOnly, wikiRead))

	documentRead, err := resolveCoreAction(CoreToolRead, CoreActionRead,
		map[string]any{"knowledge_id": "k"})
	require.NoError(t, err)
	require.Empty(t, missingCorePermissions(documentOnly, documentRead))

	_, err = resolveCoreAction(CoreToolSearch, CoreActionSingle,
		map[string]any{"query": "x", "sources": []any{"elsewhere"}})
	require.Error(t, err, "an unknown source must be refused by name")
}

// TestCoreActionTableIsComplete pins the action surface so a new action cannot
// ship without a permission entry (which would make it unreachable or, worse,
// silently permitted).
func TestCoreActionTableIsComplete(t *testing.T) {
	require.Equal(t, []string{CoreToolKnowledgeBase, CoreToolIngest, CoreToolSearch, CoreToolRead, CoreToolAsk},
		CoreToolNames())

	expected := map[string][]string{
		CoreToolKnowledgeBase: {CoreActionList, CoreActionGet, CoreActionCreate},
		CoreToolIngest:        {CoreActionSubmit, CoreActionStatus},
		CoreToolSearch:        {CoreActionSingle},
		CoreToolRead:          {CoreActionList, CoreActionRead},
		CoreToolAsk:           {CoreActionSingle},
	}
	for tool, actions := range expected {
		require.ElementsMatchf(t, actions, coreActionsFor(tool), "actions published by %s", tool)
		_, err := resolveCoreAction(tool, "not-an-action", map[string]any{})
		require.Errorf(t, err, "%s must refuse an unknown action by name", tool)
	}
}
