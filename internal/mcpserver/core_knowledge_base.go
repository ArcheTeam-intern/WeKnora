package mcpserver

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func coreKnowledgeBaseTool() mcp.Tool {
	return mcp.NewTool(CoreToolKnowledgeBase,
		mcp.WithDescription("Knowledge bases: list the libraries this caller can see, read one "+
			"library's capabilities and processing status, or create a new library. Actions: list, "+
			"get, create."),
		mcp.WithString("action", mcp.Required(),
			mcp.Description("list | get | create"), mcp.Enum(CoreActionList, CoreActionGet, CoreActionCreate)),
		mcp.WithString("knowledge_base_id",
			mcp.Description("Library id (get)")),
		mcp.WithString("name", mcp.Description("New library name (create)")),
		mcp.WithString("description", mcp.Description("New library description (create)")),
		mcp.WithBoolean("include_capabilities",
			mcp.Description("get: include the parser catalogue, the published QA profiles and "+
				"selectability on this endpoint (default true)")),
		mcp.WithBoolean("enable_wiki", mcp.Description("create: generate wiki pages (default false)")),
		mcp.WithBoolean("enable_graph", mcp.Description("create: extract a knowledge graph "+
			"(default false)")),
		mcp.WithBoolean("enable_keyword", mcp.Description("create: keyword indexing (default true)")),
		mcp.WithBoolean("enable_vector", mcp.Description("create: semantic indexing (default true)")),
		mcp.WithArray("graph_entities", mcp.Description("create: entity type names to extract, "+
			"e.g. [\"人员\",\"项目\"]; required in practice for a usable graph"),
			mcp.WithStringItems()),
		mcp.WithArray("graph_relations", mcp.Description("create: relations as \"source,target,type\", "+
			"e.g. [\"人员,项目,负责\"]"), mcp.WithStringItems()),
		mcp.WithString("embedding_model_id", mcp.Description("create: override the workspace embedding model")),
		mcp.WithString("summary_model_id", mcp.Description("create: override the workspace QA model")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
	)
}

func (s *Server) handleCoreKnowledgeBase(
	ctx context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	args := coreArgs(req)
	action, err := coreActionOf(CoreToolKnowledgeBase, args)
	if err != nil {
		return errorResult(err), nil
	}
	switch action {
	case CoreActionList:
		return s.handleListKnowledgeBases(ctx, legacyRequest(map[string]any{}))
	case CoreActionGet:
		kbID, err := requireKnowledgeBaseID(args)
		if err != nil {
			return errorResult(err), nil
		}
		include := true
		if raw, ok := args["include_capabilities"].(bool); ok {
			include = raw
		}
		detail, err := s.coreLibraryDetail(ctx, kbID, include)
		if err != nil {
			return errorResult(&ToolError{Code: CodeNotFound, Message: err.Error()}), nil
		}
		return jsonResult(map[string]any{"knowledge_base_id": kbID, "knowledge_base": detail})
	case CoreActionCreate:
		// The creation tool keeps its own model inheritance, graph type
		// validation and endpoint-range reporting; the core surface only
		// forwards the caller's intent.
		forward := map[string]any{}
		for _, key := range []string{
			"name", "description", "enable_wiki", "enable_graph", "enable_keyword", "enable_vector",
			"graph_entities", "graph_relations", "embedding_model_id", "summary_model_id",
		} {
			if value, ok := args[key]; ok {
				forward[key] = value
			}
		}
		if strings.TrimSpace(stringValue(forward["name"])) == "" {
			return errorResult(invalidArgument("name is required to create a knowledge base")), nil
		}
		result, err := s.handleCreateKnowledgeBase(ctx, legacyRequest(forward))
		if err != nil {
			return nil, err
		}
		return result, nil
	default:
		return errorResult(invalidArgument("action %q is not supported by %s", action, CoreToolKnowledgeBase)), nil
	}
}

func coreArgs(req mcp.CallToolRequest) map[string]any {
	if args, ok := req.Params.Arguments.(map[string]any); ok {
		return args
	}
	return map[string]any{}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
