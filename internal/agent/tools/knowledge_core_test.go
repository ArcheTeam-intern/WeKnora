package tools

import (
	"strings"
	"testing"
)

// The knowledge-core profile must be a real closure: retired capabilities are
// dropped even when a stored agent configuration still lists them, and the
// knowledge tools an agent did ask for survive untouched.
func TestFilterToolsForCoreProfileDropsRetiredCapabilities(t *testing.T) {
	asked := []string{
		ToolSearchKnowledge,
		ToolReadDocument,
		ToolListDocuments,
		ToolQueryKnowledgeGraph,
		ToolWikiSearch,
		ToolWikiReadPage,
		ToolWikiWritePage,
		ToolThinking,
		// Everything below is retired by the profile.
		ToolWebSearch,
		ToolWebFetch,
		ToolSearchMemory,

		ToolShellExec,
		ToolReadFile,
		ToolListSandboxFiles,
		ToolWriteSandboxFile,
		ToolEditSandboxFile,
		ToolWriteSkillFile,
		ToolEditSkillFile,
		LegacyToolExecuteSkillScript,
		LegacyToolReadSkill,
	}
	got := FilterToolsForCoreProfile(asked)
	want := []string{
		ToolSearchKnowledge, ToolReadDocument, ToolListDocuments, ToolQueryKnowledgeGraph,
		ToolWikiSearch, ToolWikiReadPage, ToolWikiWritePage, ToolThinking,
	}
	if len(got) != len(want) {
		t.Fatalf("filtered tools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("filtered tools = %v, want %v (order must be preserved)", got, want)
		}
	}
	for _, retired := range KnowledgeCoreRetiredTools() {
		for _, name := range got {
			if name == retired {
				t.Fatalf("retired tool %q survived the knowledge-core filter", retired)
			}
		}
	}
}

// Filtering means "keep what was asked for", never "add what the profile
// likes": an agent that only wanted retrieval must not gain the wiki tools.
func TestFilterToolsForCoreProfileNeverInjectsTools(t *testing.T) {
	got := FilterToolsForCoreProfile([]string{ToolSearchKnowledge})
	if len(got) != 1 || got[0] != ToolSearchKnowledge {
		t.Fatalf("filter must not inject tools, got %v", got)
	}
}

// A configuration that only lists retired capabilities leaves the agent with
// nothing, which the caller turns into a refusal instead of silently granting
// the defaults.
func TestFilterToolsForCoreProfileRefusesEmptyResult(t *testing.T) {
	if got := FilterToolsForCoreProfile([]string{ToolShellExec, ToolWebSearch}); got != nil {
		t.Fatalf("retired-only allowlist must filter to nil, got %v", got)
	}
	if got := FilterToolsForCoreProfile(KnowledgeCoreRetiredTools()); got != nil {
		t.Fatalf("the retired list itself must filter to nil, got %v", got)
	}
}

// Duplicates in a stored allowlist must not reach the model twice.
func TestFilterToolsForCoreProfileDeduplicates(t *testing.T) {
	got := FilterToolsForCoreProfile([]string{ToolSearchKnowledge, ToolSearchKnowledge, ToolWikiSearch})
	if len(got) != 2 {
		t.Fatalf("expected duplicates removed, got %v", got)
	}
	if strings.Join(got, ",") != ToolSearchKnowledge+","+ToolWikiSearch {
		t.Fatalf("unexpected order after dedup: %v", got)
	}
}
