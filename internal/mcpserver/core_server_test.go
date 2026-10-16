package mcpserver

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/ratelimit"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
)

// The core surface publishes exactly five tools, and no legacy name may leak
// into it: the main product's discovery has to be stable, and a caller who
// guessed a legacy tool name must not reach it here.
func TestCoreSurfacePublishesExactlyFiveTools(t *testing.T) {
	srv := &Server{}
	catalog := srv.coreToolCatalog()
	if len(catalog) != 5 {
		t.Fatalf("core surface publishes %d tools, want 5", len(catalog))
	}
	got := make([]string, 0, len(catalog))
	for _, entry := range catalog {
		got = append(got, entry.Tool.Name)
	}
	want := []string{CoreToolKnowledgeBase, CoreToolIngest, CoreToolSearch, CoreToolRead, CoreToolAsk}
	sort.Strings(got)
	sorted := append([]string{}, want...)
	sort.Strings(sorted)
	if strings.Join(got, ",") != strings.Join(sorted, ",") {
		t.Fatalf("core tools = %v, want %v", got, want)
	}
	// No legacy registry name may appear on the core surface.
	for _, name := range got {
		if !isCoreToolName(name) {
			t.Fatalf("%q is not a core tool name", name)
		}
	}
	for _, legacy := range []string{
		types.MCPEndpointToolListKnowledgeBases,
		types.MCPEndpointToolUploadFile,
		types.MCPEndpointToolImportDocumentFile,
		types.MCPEndpointToolGrepChunks,
		types.MCPEndpointToolWikiIndex,
		"execute",
	} {
		if isCoreToolName(legacy) {
			t.Fatalf("legacy name %q must not be reachable on the core surface", legacy)
		}
	}
}

// Every permission requirement is expressed in legacy endpoint names, so an
// endpoint configured before the core surface existed keeps working without a
// data migration, and an endpoint that never enabled a capability cannot get
// it by guessing the aggregated name.
func TestCoreActionPermissionsMapOntoLegacyTools(t *testing.T) {
	cases := []struct {
		tool    string
		args    map[string]any
		action  string
		wantKey []string
	}{
		{CoreToolKnowledgeBase, map[string]any{"action": "list"}, CoreActionList,
			[]string{types.MCPEndpointToolListKnowledgeBases}},
		{CoreToolKnowledgeBase, map[string]any{"action": "get"}, CoreActionGet,
			[]string{types.MCPEndpointToolGetKBStatus, types.MCPEndpointToolListParserEngines}},
		{CoreToolKnowledgeBase, map[string]any{"action": "create"}, CoreActionCreate,
			[]string{types.MCPEndpointToolCreateKnowledgeBase}},
		{CoreToolIngest, map[string]any{"action": "submit"}, CoreActionSubmit,
			[]string{types.MCPEndpointToolUploadFile, types.MCPEndpointToolImportDocumentFile}},
		{CoreToolIngest, map[string]any{"action": "status"}, CoreActionStatus,
			[]string{types.MCPEndpointToolGetUploadStatus, types.MCPEndpointToolGetDocumentStatus}},
		{CoreToolSearch, map[string]any{"sources": []any{"document"}}, CoreActionSingle,
			[]string{types.MCPEndpointToolSearchKnowledge}},
		{CoreToolSearch, map[string]any{"sources": []any{"document", "wiki"}}, CoreActionSingle,
			[]string{types.MCPEndpointToolSearchKnowledge, types.MCPEndpointToolWikiSearch}},
		{CoreToolSearch, map[string]any{"sources": []any{"graph"}}, CoreActionSingle,
			[]string{types.MCPEndpointToolSearchKnowledge, types.MCPEndpointToolQueryKnowledgeGraph}},
		{CoreToolRead, map[string]any{"action": "list"}, CoreActionList,
			[]string{types.MCPEndpointToolListDocuments}},
		{CoreToolRead, map[string]any{"action": "list", "source_type": "wiki"}, CoreActionList,
			[]string{types.MCPEndpointToolListDocuments, types.MCPEndpointToolWikiIndex}},
		{CoreToolRead, map[string]any{"action": "read"}, CoreActionRead,
			[]string{types.MCPEndpointToolReadDocument}},
		{CoreToolRead, map[string]any{"action": "read", "source_type": "wiki"}, CoreActionRead,
			[]string{types.MCPEndpointToolReadDocument, types.MCPEndpointToolWikiReadPage}},
		{CoreToolAsk, map[string]any{"question": "q"}, CoreActionSingle,
			[]string{types.MCPEndpointToolAsk}},
	}
	for _, c := range cases {
		got, err := coreRequirementFor(c.tool, c.action, c.args)
		if err != nil {
			t.Fatalf("%s.%s: %v", c.tool, c.action, err)
		}
		if strings.Join(got, ",") != strings.Join(c.wantKey, ",") {
			t.Fatalf("%s.%s requires %v, want %v", c.tool, c.action, got, c.wantKey)
		}
	}
}

// Unknown actions and unknown sources are refused by name; they must never fall
// back to a broader behaviour.
func TestCoreUnsupportedActionsAndSourcesAreRefused(t *testing.T) {
	if _, err := coreRequirementFor(CoreToolKnowledgeBase, "delete", map[string]any{}); err == nil {
		t.Fatal("an unsupported action must be refused")
	}
	if _, err := coreRequirementFor(CoreToolSearch, CoreActionSingle,
		map[string]any{"sources": []any{"memory"}}); err == nil {
		t.Fatal("an unsupported source must be refused")
	}
	if _, err := coreRequirementFor(CoreToolRead, CoreActionList,
		map[string]any{"source_type": "sandbox"}); err == nil {
		t.Fatal("an unsupported read source must be refused")
	}
	// A tool with several actions needs one: silently picking "list" would be a
	// silent privilege guess.
	if _, err := coreActionOf(CoreToolKnowledgeBase, map[string]any{}); err == nil {
		t.Fatal("a missing action must be refused when the tool has several")
	}
	// A single-action tool does not require the caller to spell it out.
	if action, err := coreActionOf(CoreToolSearch, map[string]any{}); err != nil || action != CoreActionSingle {
		t.Fatalf("single-action tool default = %q, %v", action, err)
	}
}

// The guard refuses a call whose endpoint does not carry the underlying
// permission, and reports which permission is missing instead of "forbidden".
func TestCoreGuardRefusesMissingLegacyPermission(t *testing.T) {
	ep := &types.MCPEndpoint{
		ID:       "ep-1",
		TenantID: 1,
		Tools:    []string{types.MCPEndpointToolListKnowledgeBases},
	}
	ctx := coreGuardTestContext(1, ep)
	srv := &Server{limiter: newCoreTestLimiter()}

	// knowledge_base.list is allowed by the endpoint above.
	called := false
	next := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		return jsonResult(map[string]any{"ok": true})
	}
	if _, err := srv.guardCoreTool(next)(ctx, coreCall(CoreToolKnowledgeBase, map[string]any{"action": "list"})); err != nil {
		t.Fatalf("allowed call failed: %v", err)
	}
	if !called {
		t.Fatal("the allowed call must reach the handler")
	}

	// ingest_document.submit needs upload_file + import_document_file, neither
	// of which this endpoint enabled.
	called = false
	result, err := srv.guardCoreTool(next)(ctx, coreCall(CoreToolIngest, map[string]any{
		"action": "submit", "file": map[string]any{"name": "a.pdf", "content": "x"},
		"knowledge_base_id": "kb-1",
	}))
	if err != nil {
		t.Fatalf("guard returned a transport error: %v", err)
	}
	if called || result == nil || !result.IsError {
		t.Fatalf("a call without the underlying permission must be refused: %#v", result)
	}
	text := mcpErrorText(result)
	for _, want := range []string{types.MCPEndpointToolUploadFile, types.MCPEndpointToolImportDocumentFile} {
		if !strings.Contains(text, want) {
			t.Fatalf("the refusal should name the missing permission %q: %s", want, text)
		}
	}

	// Three core names are deliberately shared with the legacy catalogue
	// (search_knowledge, read_document, ask, per the plan's five-tool contract);
	// the legacy-only names must never be reachable here.
	for _, legacyOnly := range []string{
		types.MCPEndpointToolGrepChunks,
		types.MCPEndpointToolWikiIndex,
		types.MCPEndpointToolUploadFile,
		types.MCPEndpointToolImportDocumentFile,
		types.MCPEndpointToolListDocuments,
	} {
		if isCoreToolName(legacyOnly) {
			t.Fatalf("legacy-only name %q must not be reachable on the core surface", legacyOnly)
		}
	}
	shared := map[string]bool{}
	for _, name := range CoreToolNames() {
		for _, legacy := range []string{
			types.MCPEndpointToolSearchKnowledge,
			types.MCPEndpointToolReadDocument,
			types.MCPEndpointToolAsk,
		} {
			if name == legacy {
				shared[name] = true
			}
		}
	}
	if len(shared) != 3 {
		t.Fatalf("the shared legacy names are exactly search_knowledge/read_document/ask: %v", shared)
	}
}

// coreGuardTestContext reuses the shared MCP call context: the guard reads the
// endpoint, the caller and the API-key scope from exactly those keys.
func coreGuardTestContext(tenantID uint64, ep *types.MCPEndpoint) context.Context {
	return mcpCallContext(tenantID, ep)
}

// newCoreTestLimiter returns the same limiter the server builds, without redis:
// the limiter falls back to an in-process counter, which is what the guard test
// exercises.
func newCoreTestLimiter() *ratelimit.Limiter {
	return ratelimit.New(nil, rateLimitKeyPrefix, time.Minute, "")
}

func coreCall(tool string, args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Name: tool, Arguments: args}}
}
