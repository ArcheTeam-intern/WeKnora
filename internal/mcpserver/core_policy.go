package mcpserver

import (
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// Core tool names. The main product discovers exactly these five; the legacy
// catalogue stays published on /mcp/:endpoint_id until its clients migrate.
const (
	CoreToolKnowledgeBase = "knowledge_base"
	CoreToolIngest        = "ingest_document"
	CoreToolSearch        = "search_knowledge"
	CoreToolRead          = "read_document"
	CoreToolAsk           = "ask"
)

// Core actions. A tool without an `action` parameter (search_knowledge, ask)
// has exactly one implicit action, named here so the permission table can be
// keyed on (tool, action) uniformly.
const (
	CoreActionList   = "list"
	CoreActionGet    = "get"
	CoreActionCreate = "create"
	CoreActionSubmit = "submit"
	CoreActionStatus = "status"
	CoreActionRead   = "read"
	CoreActionSingle = "single"
)

// corePermission is the set of legacy endpoint permissions a core call needs.
//
// The core surface is an aggregation, not a second authorization system: every
// action maps onto the legacy tools the endpoint already had to enable, and all
// of them must be enabled. That keeps existing端点数据可读 (an operator who
// enabled upload_file + import_document_file gets ingest_document.submit, and
// one who did not still cannot get it by guessing the new name).
type corePermission struct {
	// Tools must all be enabled on the endpoint allowlist.
	Tools []string
	// Base is the action-independent requirement added when a call reaches a
	// particular source (wiki/graph) or file shape.
	Note string
}

// coreActionPermissions is the whole authorization table for the core surface.
var coreActionPermissions = map[string]corePermission{
	CoreToolKnowledgeBase + "." + CoreActionList: {
		Tools: []string{types.MCPEndpointToolListKnowledgeBases},
	},
	CoreToolKnowledgeBase + "." + CoreActionGet: {
		// The detail view answers "what can this caller do here", so it needs
		// the status tool and the parser catalogue: the catalogue is how the
		// caller learns which engines it may ask for on submit.
		Tools: []string{types.MCPEndpointToolGetKBStatus, types.MCPEndpointToolListParserEngines},
	},
	CoreToolKnowledgeBase + "." + CoreActionCreate: {
		Tools: []string{types.MCPEndpointToolCreateKnowledgeBase},
	},
	CoreToolIngest + "." + CoreActionSubmit: {
		Tools: []string{types.MCPEndpointToolUploadFile, types.MCPEndpointToolImportDocumentFile},
	},
	CoreToolIngest + "." + CoreActionStatus: {
		Tools: []string{types.MCPEndpointToolGetUploadStatus, types.MCPEndpointToolGetDocumentStatus},
	},
	CoreToolSearch + "." + CoreActionSingle: {
		// Only the document source is covered here; the wiki and graph sources
		// add their own tool below when the caller asks for them.
		Tools: []string{types.MCPEndpointToolSearchKnowledge},
	},
	CoreToolRead + "." + CoreActionList: {
		Tools: []string{types.MCPEndpointToolListDocuments},
	},
	CoreToolRead + "." + CoreActionRead: {
		Tools: []string{types.MCPEndpointToolReadDocument},
	},
	CoreToolAsk + "." + CoreActionSingle: {
		Tools: []string{types.MCPEndpointToolAsk},
	},
}

// coreSourcePermissions adds the per-source requirement for the aggregated
// search and read tools, so allowing the document source never implies the
// wiki or the graph.
var coreSourcePermissions = map[string][]string{
	CoreToolSearch + ".wiki":    {types.MCPEndpointToolWikiSearch},
	CoreToolSearch + ".graph":   {types.MCPEndpointToolQueryKnowledgeGraph},
	CoreToolRead + ".wiki.list": {types.MCPEndpointToolWikiIndex},
	CoreToolRead + ".wiki.read": {types.MCPEndpointToolWikiReadPage},
}

// coreReadSourceTypes are the sources read_document understands.
func coreReadSourceTypes() []string { return []string{"document", "wiki"} }

// coreSearchSources are the sources search_knowledge understands.
func coreSearchSources() []string { return []string{"document", "wiki", "graph"} }

// resolveCoreAction validates the caller's action for a core tool and returns
// the permission it needs. An unknown action is refused by name — guessing an
// action must not fall back to a broader one.
func resolveCoreAction(tool, action string, args map[string]any) (corePermission, error) {
	key := tool + "." + action
	perm, ok := coreActionPermissions[key]
	if !ok {
		return corePermission{}, invalidArgument(
			"action %q is not supported by %s; supported actions: %s",
			action, tool, strings.Join(coreActionsFor(tool), ", "))
	}
	// Source-specific requirements are additive and must be present before the
	// aggregated tool touches that source.
	switch tool {
	case CoreToolSearch:
		sources, err := coreStringList(args, "sources")
		if err != nil {
			return corePermission{}, err
		}
		for _, source := range sources {
			if source == "document" {
				continue
			}
			extra, ok := coreSourcePermissions[CoreToolSearch+"."+source]
			if !ok {
				return corePermission{}, invalidArgument(
					"source %q is not supported by %s; supported sources: %s",
					source, tool, strings.Join(coreSearchSources(), ", "))
			}
			perm.Tools = append(perm.Tools, extra...)
		}
	case CoreToolRead:
		source, err := coreStringArg(args, "source_type", "document")
		if err != nil {
			return corePermission{}, err
		}
		if source == "document" {
			break
		}
		extra, ok := coreSourcePermissions[CoreToolRead+"."+source+"."+action]
		if !ok {
			return corePermission{}, invalidArgument(
				"source_type %q is not supported by %s.%s; supported sources: %s",
				source, tool, action, strings.Join(coreReadSourceTypes(), ", "))
		}
		perm.Tools = append(perm.Tools, extra...)
	}
	return perm, nil
}

// coreActionsFor lists the actions a core tool publishes, for error messages
// and for the protocol test.
func coreActionsFor(tool string) []string {
	out := make([]string, 0, 3)
	for _, action := range []string{CoreActionList, CoreActionGet, CoreActionCreate, CoreActionSubmit, CoreActionStatus, CoreActionRead} {
		if _, ok := coreActionPermissions[tool+"."+action]; ok {
			out = append(out, action)
		}
	}
	if len(out) == 0 {
		out = append(out, CoreActionSingle)
	}
	return out
}

// coreActionOf reads the action from the request arguments, defaulting to the
// single implicit action for tools that have exactly one.
func coreActionOf(tool string, args map[string]any) (string, error) {
	raw, _ := args["action"].(string)
	action := strings.TrimSpace(raw)
	if action != "" {
		return action, nil
	}
	actions := coreActionsFor(tool)
	if len(actions) == 1 {
		return actions[0], nil
	}
	return "", invalidArgument("action is required for %s; supported actions: %s",
		tool, strings.Join(actions, ", "))
}

func coreStringArg(args map[string]any, key, fallback string) (string, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return fallback, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", invalidArgument("%s must be a string", key)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	return value, nil
}

// coreStringList reads a list-of-strings argument. A single string is accepted
// because MCP clients routinely collapse one-element arrays.
func coreStringList(args map[string]any, key string) ([]string, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}
	switch typed := raw.(type) {
	case string:
		value := strings.TrimSpace(typed)
		if value == "" {
			return nil, nil
		}
		return []string{value}, nil
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, invalidArgument("%s must be a list of strings", key)
			}
			if text = strings.TrimSpace(text); text != "" {
				out = append(out, text)
			}
		}
		return out, nil
	case []string:
		return typed, nil
	default:
		return nil, invalidArgument("%s must be a list of strings", key)
	}
}

// missingCorePermissions reports which required legacy tools the endpoint does
// not enable, so a refusal says what to turn on instead of "forbidden".
func missingCorePermissions(ep *types.MCPEndpoint, perm corePermission) []string {
	missing := make([]string, 0, len(perm.Tools))
	for _, name := range perm.Tools {
		if !ep.HasTool(name) {
			missing = append(missing, name)
		}
	}
	return missing
}

// corePermissionError renders the refusal for a call whose endpoint lacks one
// of the underlying permissions.
func corePermissionError(tool, action string, missing []string) *ToolError {
	return &ToolError{
		Code: CodeForbidden,
		Message: fmt.Sprintf(
			"%s.%s is not enabled on this endpoint: it requires the %s permission(s)",
			tool, action, strings.Join(missing, ", ")),
		Details: map[string]any{"missing_tools": missing},
	}
}
