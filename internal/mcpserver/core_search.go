package mcpserver

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func coreSearchTool() mcp.Tool {
	return mcp.NewTool(CoreToolSearch,
		mcp.WithDescription("Retrieve evidence from the selected libraries. mode chooses how the "+
			"document source is searched (hybrid | semantic | keyword); sources choose which "+
			"material is searched and are reported separately, because wiki pages and graph "+
			"relations have their own query path and are not part of the chunk index."+
			" match=literal switches the document source to literal matching for identifiers, "+
			"amounts and codes."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Question or phrase to search for")),
		mcp.WithArray("knowledge_base_ids", mcp.Description("Libraries to search; omit to search "+
			"everything this caller may read"), mcp.WithStringItems()),
		mcp.WithString("mode", mcp.Description("hybrid (default) | semantic | keyword"),
			mcp.Enum("hybrid", "semantic", "keyword")),
		mcp.WithString("match", mcp.Description("literal: exact substring matching over chunks "+
			"instead of ranked retrieval"), mcp.Enum("literal")),
		mcp.WithArray("sources", mcp.Description("document (default) | wiki | graph"),
			mcp.WithStringItems()),
		mcp.WithNumber("limit", mcp.Description("Maximum results per source (default 10, max 30)")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func (s *Server) handleCoreSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := coreArgs(req)
	query := strings.TrimSpace(stringValue(args["query"]))
	if query == "" {
		return errorResult(invalidArgument("query is required")), nil
	}
	kbIDs, err := coreKnowledgeBaseIDs(args)
	if err != nil {
		return errorResult(err), nil
	}
	sources, err := coreStringList(args, "sources")
	if err != nil {
		return errorResult(err), nil
	}
	if len(sources) == 0 {
		sources = []string{"document"}
	}
	mode, err := coreStringArg(args, "mode", "hybrid")
	if err != nil {
		return errorResult(err), nil
	}
	match, err := coreStringArg(args, "match", "")
	if err != nil {
		return errorResult(err), nil
	}

	parts := map[string]any{}
	failed := []map[string]any{}
	failedSources := []string{}
	attempt := func(source string, call func() (*mcp.CallToolResult, error)) {
		result, callErr := call()
		switch {
		case callErr != nil:
			failed = append(failed, map[string]any{"source": source, "error": callErr.Error()})
			failedSources = append(failedSources, source)
		case result != nil && result.IsError:
			failed = append(failed, map[string]any{"source": source, "error": mcpErrorText(result)})
			failedSources = append(failedSources, source)
		default:
			parts[source] = structuredOf(result)
		}
	}

	for _, source := range sources {
		switch source {
		case "document":
			documentArgs := map[string]any{"query": query, "mode": mode}
			if len(kbIDs) > 0 {
				documentArgs["knowledge_base_ids"] = kbIDs
			}
			if limit, ok := coreLimit(args); ok {
				documentArgs["limit"] = limit
			}
			if match == "literal" {
				attempt("document", func() (*mcp.CallToolResult, error) {
					return s.handleGrepChunks(ctx, legacyRequest(documentArgs))
				})
				continue
			}
			attempt("document", func() (*mcp.CallToolResult, error) {
				return s.handleSearchKnowledge(ctx, legacyRequest(documentArgs))
			})
		case "wiki":
			wikiArgs := map[string]any{"query": query}
			if len(kbIDs) > 0 {
				wikiArgs["knowledge_base_ids"] = kbIDs
			}
			if limit, ok := coreLimit(args); ok {
				wikiArgs["limit"] = limit
			}
			attempt("wiki", func() (*mcp.CallToolResult, error) {
				return s.handleWikiSearch(ctx, legacyRequest(wikiArgs))
			})
		case "graph":
			if len(kbIDs) == 0 {
				failed = append(failed, map[string]any{
					"source": "graph",
					"error":  "the graph source requires an explicit knowledge_base_ids selection",
				})
				failedSources = append(failedSources, "graph")
				continue
			}
			attempt("graph", func() (*mcp.CallToolResult, error) {
				return s.handleKnowledgeGraphQuery(ctx, legacyRequest(map[string]any{
					"knowledge_base_ids": kbIDs,
					"query":              query,
				}))
			})
		default:
			return errorResult(invalidArgument(
				"source %q is not supported; supported sources: %s",
				source, strings.Join(coreSearchSources(), ", "))), nil
		}
	}

	payload := map[string]any{
		"query":   query,
		"mode":    mode,
		"sources": sources,
		"results": parts,
	}
	if match == "literal" {
		payload["match"] = "literal"
	}
	if len(failedSources) > 0 {
		// A source the caller explicitly asked for must never degrade into an
		// empty result set: the response says the run is partial and which part
		// failed.
		payload["partial"] = true
		payload["failed_sources"] = failedSources
		payload["failures"] = failed
	}
	return jsonResult(payload)
}

func coreLimit(args map[string]any) (int, bool) {
	raw, ok := args["limit"]
	if !ok || raw == nil {
		return 0, false
	}
	value, ok := raw.(float64)
	if !ok {
		return 0, false
	}
	limit := int(value)
	if limit < 1 {
		return 0, false
	}
	if limit > 30 {
		limit = 30
	}
	return limit, true
}
