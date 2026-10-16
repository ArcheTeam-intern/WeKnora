package mcpserver

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func coreAskTool() mcp.Tool {
	return mcp.NewTool(CoreToolAsk,
		mcp.WithDescription("Ask the platform's native knowledge QA. mode selects the built-in "+
			"answering path (quick-answer uses the fixed retrieval pipeline, smart-reasoning lets "+
			"the knowledge agent iterate over retrieval tools); omitting it uses the endpoint's "+
			"configured default. The answer comes back with its citations, and the returned "+
			"session_id continues the same conversation as long as the scope and mode stay the same."),
		mcp.WithString("question", mcp.Required(), mcp.Description("The question to answer")),
		mcp.WithArray("knowledge_base_ids", mcp.Description("Libraries this question may use; "+
			"omit to use the endpoint configuration"), mcp.WithStringItems()),
		mcp.WithString("mode", mcp.Description("quick-answer | smart-reasoning; omit for the "+
			"endpoint default"), mcp.Enum("quick-answer", "smart-reasoning")),
		mcp.WithString("profile_id", mcp.Description("A QA profile published by this endpoint "+
			"(see knowledge_base.get). Internal agent ids are not accepted.")),
		mcp.WithString("session_id", mcp.Description("Continue a previous answer's conversation")),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func (s *Server) handleCoreAsk(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := coreArgs(req)
	question := strings.TrimSpace(stringValue(args["question"]))
	if question == "" {
		return errorResult(invalidArgument("question is required")), nil
	}
	mode, err := coreStringArg(args, "mode", "")
	if err != nil {
		return errorResult(err), nil
	}
	profileID := strings.TrimSpace(stringValue(args["profile_id"]))
	if profileID != "" {
		// A profile is not an agent id: the only id a caller may name is the one
		// this endpoint publishes. Anything else would let a caller select an
		// internal agent the deployment never offered.
		ep, err := endpointFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError("unauthorized"), nil
		}
		if profileID != ep.DefaultAgentID || profileID == "" {
			return errorResult(&ToolError{
				Code: CodeInvalidArgument,
				Message: "profile_id is not a profile published by this endpoint; call " +
					"knowledge_base.get and use a qa_profiles entry, or omit profile_id",
				Details: map[string]any{"published_profiles": ep.DefaultAgentID},
			}), nil
		}
		if mode != "" {
			return errorResult(invalidArgument(
				"profile_id and mode cannot be combined: the profile already fixes the mode")), nil
		}
	}
	kbIDs, err := coreKnowledgeBaseIDs(args)
	if err != nil {
		return errorResult(err), nil
	}

	forward := map[string]any{"question": question}
	if mode != "" {
		forward["mode"] = mode
	}
	if sessionID := strings.TrimSpace(stringValue(args["session_id"])); sessionID != "" {
		forward["session_id"] = sessionID
	}
	if len(kbIDs) > 0 {
		forward["knowledge_base_ids"] = kbIDs
	}
	result, callErr := s.handleAsk(ctx, legacyRequest(forward))
	if callErr != nil {
		return nil, callErr
	}
	if result != nil && result.IsError {
		return coreErrorResult(result), nil
	}
	payload := structuredOf(result)
	if payload == nil {
		return result, nil
	}
	// Surface the effective scope and mode: a caller must be able to tell what
	// the platform actually used, not what it hoped it asked for.
	if _, ok := payload["mode"]; !ok {
		payload["mode"] = mode
	}
	if _, ok := payload["knowledge_base_ids"]; !ok && len(kbIDs) > 0 {
		payload["knowledge_base_ids"] = kbIDs
	}
	return jsonResult(payload)
}
