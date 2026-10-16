package tools

// The knowledge-core product profile keeps exactly one tool surface reachable
// from an agent run: retrieval, document reading, the knowledge graph, the wiki
// (including the write tools the asynchronous wiki pipeline needs) and the
// planning helpers. Everything the profile retires — public web search,
// long-term memory, conversation-history search, the shell, sandbox files,
// skills and the browser — is dropped here even when a stored custom agent or
// an old preset still lists it.
//
// Filtering is deliberately "keep what was asked for, drop what is retired"
// rather than "inject the knowledge tools": a preset that never wanted the wiki
// tools must not gain them from the profile switch.
var knowledgeCoreToolNames = map[string]struct{}{
	ToolSearchKnowledge:     {},
	ToolReadDocument:        {},
	ToolListDocuments:       {},
	ToolQueryKnowledgeGraph: {},
	ToolWikiSearch:          {},
	ToolWikiReadPage:        {},
	ToolWikiWritePage:       {},
	ToolWikiReplaceText:     {},
	ToolWikiRenamePage:      {},
	ToolWikiDeletePage:      {},
	ToolWikiFlagIssue:       {},
	ToolWikiReadIssue:       {},
	ToolWikiUpdateIssue:     {},
	ToolThinking:            {},
	ToolTodoWrite:           {},
	ToolDataAnalysis:        {},
	ToolDataSchema:          {},
}

// KnowledgeCoreRetiredTools names the capabilities the knowledge-core profile
// never lets an agent reach, for tests and for the startup log.
func KnowledgeCoreRetiredTools() []string {
	return []string{
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
}

// FilterToolsForCoreProfile returns the subset of allowed that the
// knowledge-core profile still permits, preserving the caller's order and
// dropping duplicates. A nil result means the agent may run no tool at all,
// which is the correct outcome for a configuration that only listed retired
// capabilities.
func FilterToolsForCoreProfile(allowed []string) []string {
	if len(allowed) == 0 {
		return nil
	}
	out := make([]string, 0, len(allowed))
	seen := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		if _, ok := knowledgeCoreToolNames[name]; !ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
