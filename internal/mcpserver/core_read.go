package mcpserver

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func coreReadTool() mcp.Tool {
	return mcp.NewTool(CoreToolRead,
		mcp.WithDescription("Browse and re-read the original material for citation checks. "+
			"action=list is the catalogue: for source_type=document it lists a library's documents, "+
			"for source_type=wiki it lists the wiki pages. action=read returns the text: document "+
			"reads are paginated chunks and accept a query to pull only the matching ones; wiki "+
			"reads take a page slug. Every returned item is re-authorized for this caller."),
		mcp.WithString("action", mcp.Required(),
			mcp.Description("list | read"), mcp.Enum(CoreActionList, CoreActionRead)),
		mcp.WithString("source_type", mcp.Description("document (default) | wiki"),
			mcp.Enum(coreReadSourceTypes()...)),
		mcp.WithString("knowledge_base_id", mcp.Description("Library to browse (list) or the "+
			"wiki's library (read with source_type=wiki)")),
		mcp.WithString("knowledge_id", mcp.Description("Document id (read with source_type=document)")),
		mcp.WithString("slug", mcp.Description("Wiki page slug (read with source_type=wiki)")),
		mcp.WithString("query", mcp.Description("read: return only chunks containing this phrase")),
		mcp.WithNumber("offset", mcp.Description("read: chunk offset (default 0)")),
		mcp.WithNumber("limit", mcp.Description("read: number of chunks to return")),
		mcp.WithNumber("page", mcp.Description("list: page number (default 1)")),
		mcp.WithNumber("page_size", mcp.Description("list: items per page")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func (s *Server) handleCoreRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := coreArgs(req)
	action, err := coreActionOf(CoreToolRead, args)
	if err != nil {
		return errorResult(err), nil
	}
	source, err := coreStringArg(args, "source_type", "document")
	if err != nil {
		return errorResult(err), nil
	}
	switch action {
	case CoreActionList:
		return s.coreReadList(ctx, source, args)
	case CoreActionRead:
		return s.coreReadOne(ctx, source, args)
	default:
		return errorResult(invalidArgument("action %q is not supported by %s", action, CoreToolRead)), nil
	}
}

func (s *Server) coreReadList(
	ctx context.Context, source string, args map[string]any,
) (*mcp.CallToolResult, error) {
	kbID, err := requireKnowledgeBaseID(args)
	if err != nil {
		return errorResult(err), nil
	}
	forward := map[string]any{"knowledge_base_id": kbID}
	for _, key := range []string{"page", "page_size"} {
		if value, ok := args[key]; ok {
			forward[key] = value
		}
	}
	if source == "wiki" {
		result, callErr := s.handleWikiIndex(ctx, legacyRequest(forward))
		if callErr != nil {
			return nil, callErr
		}
		if result != nil && result.IsError {
			return coreErrorResult(result), nil
		}
		return jsonResult(map[string]any{
			"source_type":       "wiki",
			"knowledge_base_id": kbID,
			"wiki":              structuredOf(result),
		})
	}
	result, callErr := s.handleListDocuments(ctx, legacyRequest(forward))
	if callErr != nil {
		return nil, callErr
	}
	if result != nil && result.IsError {
		return coreErrorResult(result), nil
	}
	return jsonResult(map[string]any{
		"source_type":       "document",
		"knowledge_base_id": kbID,
		"documents":         structuredOf(result),
	})
}

// coreReadOne re-reads one piece of evidence. For a graph relation the caller
// reads the document or chunk the relation cites — this tool stays the single
// reading path, so a citation is always verified against the stored text.
func (s *Server) coreReadOne(
	ctx context.Context, source string, args map[string]any,
) (*mcp.CallToolResult, error) {
	forward := map[string]any{}
	for _, key := range []string{"query", "offset", "limit"} {
		if value, ok := args[key]; ok {
			forward[key] = value
		}
	}
	if source == "wiki" {
		slug := strings.TrimSpace(stringValue(args["slug"]))
		if slug == "" {
			return errorResult(invalidArgument("slug is required to read a wiki page")), nil
		}
		forward["slug"] = slug
		if kbID := strings.TrimSpace(stringValue(args["knowledge_base_id"])); kbID != "" {
			forward["knowledge_base_id"] = kbID
		}
		result, callErr := s.handleWikiReadPage(ctx, legacyRequest(forward))
		if callErr != nil {
			return nil, callErr
		}
		if result != nil && result.IsError {
			return coreErrorResult(result), nil
		}
		return jsonResult(map[string]any{
			"source_type": "wiki",
			"slug":        slug,
			"page":        structuredOf(result),
		})
	}
	knowledgeID := strings.TrimSpace(stringValue(args["knowledge_id"]))
	if knowledgeID == "" {
		return errorResult(invalidArgument("knowledge_id is required to read a document")), nil
	}
	forward["knowledge_id"] = knowledgeID
	result, callErr := s.handleReadDocument(ctx, legacyRequest(forward))
	if callErr != nil {
		return nil, callErr
	}
	if result != nil && result.IsError {
		return coreErrorResult(result), nil
	}
	return jsonResult(map[string]any{
		"source_type": "document",
		"document":    structuredOf(result),
	})
}
