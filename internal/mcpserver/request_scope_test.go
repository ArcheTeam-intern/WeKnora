package mcpserver

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/mcpscope"
	"github.com/Tencent/WeKnora/internal/types"
)

// These tests cover the plan's mandatory request-scope behaviour. The names are
// fixed by §7.2 so they can be grepped.

func kbWithID(id string) *types.KnowledgeBase {
	return &types.KnowledgeBase{ID: id, Name: "kb-" + id, TenantID: 1}
}

const (
	kbA = "11111111-1111-1111-1111-111111111111"
	kbB = "22222222-2222-2222-2222-222222222222"
	kbC = "33333333-3333-3333-3333-333333333333"
)

// An endpoint that allows A and B, with only A selected, must not serve B.
func TestSelectedARejectsB(t *testing.T) {
	scope, err := mcpscope.Parse(kbA)
	if err != nil {
		t.Fatalf("parse scope: %v", err)
	}
	allowed := []*types.KnowledgeBase{kbWithID(kbA), kbWithID(kbB)}

	narrowed, err := applySelection(allowed, scope)
	if err != nil {
		t.Fatalf("selecting A must succeed: %v", err)
	}
	if len(narrowed) != 1 || narrowed[0].ID != kbA {
		t.Fatalf("selection must narrow to A, got %+v", narrowed)
	}

	// Narrowing to A must not make B unreachable in general: with the header
	// absent (a legacy machine client) B is still inside the endpoint's range.
	// What must be refused is a selection naming a knowledge base the endpoint
	// does not offer at all, because that is a scope violation, not a narrowing.
	if _, err := applySelection(allowed, mustScope(t, kbC)); err == nil {
		t.Fatal("selecting a knowledge base outside the endpoint's range must be refused")
	}
	// A selection of B alone is a legitimate narrowing of an endpoint that
	// allows B; the frozen value must simply be B, never A.
	onlyB := mustScope(t, kbB)
	if onlyB.Version == scope.Version {
		t.Fatal("different selections must have different scope versions")
	}
	selected, err := applySelection(allowed, onlyB)
	if err != nil {
		t.Fatalf("selecting B on an endpoint that allows B must succeed: %v", err)
	}
	if len(selected) != 1 || selected[0].ID != kbB {
		t.Fatalf("selection must be exactly B, got %+v", selected)
	}
}

// selectedA_rejectsB_read: a by-id read of a resource in B must fail while the
// frozen selection is A, even though B is inside the endpoint's range.
func TestSelectedARejectsBRead(t *testing.T) {
	scope := mustScope(t, kbA)
	if scope.Contains(kbB) {
		t.Fatal("a scope frozen on A must not contain B")
	}
	if !scope.Contains(kbA) {
		t.Fatal("a scope frozen on A must contain A")
	}
	// The legacy path (no header) still admits B, which is what keeps existing
	// machine clients working.
	if !(mcpscope.Scope{}).Contains(kbB) {
		t.Fatal("an absent selection must not narrow anything")
	}
}

// An empty selection is a caller error, never "unrestricted".
func TestEmptySelectionRejected(t *testing.T) {
	for _, raw := range []string{"", "   ", ","} {
		if _, err := mcpscope.Parse(raw); err == nil {
			t.Fatalf("empty selection %q must be rejected", raw)
		}
	}
	var parseErr *mcpscope.Error
	_, err := mcpscope.Parse("")
	if !asToolError(err, &parseErr) {
		t.Fatalf("expected a structured error, got %v", err)
	}
	if parseErr.Code != mcpscope.CodeKBSelectionRequired {
		t.Fatalf("code = %q, want %q", parseErr.Code, mcpscope.CodeKBSelectionRequired)
	}
}

// A non-UUID, a duplicate and an over-long selection each have their own answer.
func TestSelectionValidation(t *testing.T) {
	if _, err := mcpscope.Parse("not-a-uuid"); err == nil {
		t.Fatal("a non-UUID id must be rejected")
	}
	dup, err := mcpscope.Parse(kbA + "," + kbA + "," + kbB)
	if err != nil {
		t.Fatalf("duplicates must be deduplicated, not rejected: %v", err)
	}
	if len(dup.Selected) != 2 {
		t.Fatalf("deduplicated selection = %v", dup.Selected)
	}
	// Header order must not change the frozen version, so a re-ordered retry is
	// recognised as the same scope.
	reordered, err := mcpscope.Parse(kbB + "," + kbA)
	if err != nil {
		t.Fatalf("reordered selection: %v", err)
	}
	if reordered.Version != dup.Version {
		t.Fatalf("scope version must not depend on header order: %q vs %q",
			dup.Version, reordered.Version)
	}

	many := ""
	for i := 0; i < mcpscope.MaxRequestKnowledgeBases+1; i++ {
		if i > 0 {
			many += ","
		}
		many += uuidForIndex(i)
	}
	if _, err := mcpscope.Parse(many); err == nil {
		t.Fatalf("more than %d ids must be rejected", mcpscope.MaxRequestKnowledgeBases)
	}
}

// An id outside the caller's authorized set is refused rather than dropped,
// because dropping it would answer from a different range than was requested.
func TestOutOfScopeSelectionRefusedNotDropped(t *testing.T) {
	allowed := []*types.KnowledgeBase{kbWithID(kbA)}
	_, err := applySelection(allowed, mustScope(t, kbA+","+kbC))
	if err == nil {
		t.Fatal("a selection containing an unauthorized id must be refused")
	}
	var toolErr *ToolError
	if !asToolError(err, &toolErr) || toolErr.Code != CodeKBOutOfScope {
		t.Fatalf("expected KB_OUT_OF_SCOPE, got %v", err)
	}
	if outside, ok := toolErr.Details["outside_scope"].([]string); !ok || len(outside) != 1 {
		t.Fatalf("the refusal must name the offending id, got %+v", toolErr.Details)
	}
}

// A tool that needs a knowledge base refuses an explicitly empty selection, while
// the discovery and status tools stay callable.
func TestRequireSelectionOnlyForContentTools(t *testing.T) {
	// A present-but-empty scope cannot be produced by Parse (it errors first), so
	// it is constructed directly: this is the state the guard must reject.
	empty := mcpscope.Scope{Present: true, Selected: nil}
	if err := empty.RequireSelection("search_knowledge"); err == nil {
		t.Fatal("a content tool must refuse an empty selection")
	}
	if _, optional := scopeOptionalTools[types.MCPEndpointToolListKnowledgeBases]; !optional {
		t.Fatal("list_knowledge_bases must be callable before a selection exists")
	}
	for _, name := range []string{
		types.MCPEndpointToolGetUploadStatus,
		types.MCPEndpointToolImportDocumentFile,
		types.MCPEndpointToolGetDocumentStatus,
		types.MCPEndpointToolGetKBStatus,
	} {
		if _, optional := scopeOptionalTools[name]; !optional {
			t.Fatalf("%s must be callable without a selection", name)
		}
	}
	if _, optional := scopeOptionalTools[types.MCPEndpointToolSearchKnowledge]; optional {
		t.Fatal("search_knowledge must require a selection")
	}
}

// The scope survives on the context so every handler sees the same frozen value.
func TestRequestScopeTravelsOnContext(t *testing.T) {
	scope := mustScope(t, kbA)
	ctx := mcpscope.WithScope(context.Background(), scope)
	got := RequestScopeOf(ctx)
	if !got.Present || len(got.Selected) != 1 || got.Selected[0] != kbA {
		t.Fatalf("scope lost on the context: %+v", got)
	}
	if RequestScopeOf(context.Background()).Present {
		t.Fatal("an unset scope must read as absent, not as empty-and-present")
	}
}

// --- helpers ---

func mustScope(t *testing.T, raw string) mcpscope.Scope {
	t.Helper()
	scope, err := mcpscope.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return scope
}

// asToolError is a small wrapper so the tests do not each import errors.
func asToolError(err error, target **mcpscope.Error) bool {
	if err == nil {
		return false
	}
	typed, ok := err.(*mcpscope.Error)
	if ok && target != nil {
		*target = typed
	}
	return ok
}

func uuidForIndex(i int) string {
	const digits = "0123456789abcdef"
	// 8-4-4-4-12 with the index in the last group.
	suffix := ""
	for j := 0; j < 12; j++ {
		suffix = string(digits[(i+j)%16]) + suffix
	}
	return "00000000-0000-4000-8000-" + suffix
}
