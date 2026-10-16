package mcpserver

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
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

	// Bind the effective scope, mode and profile to the session so a later
	// ask with the same session_id is refused when the caller switches
	// any of them. The bound kb set is sorted and de-duplicated so the same
	// libraries passed in a different order still hit the existing
	// session; the bound mode/profile match exactly.
	boundKBs := sortDedupStrings(kbIDs)
	effectiveMode := mode
	if effectiveMode == "" {
		effectiveMode = "quick-answer"
	}
	effectiveProfile := profileID
	requestedSessionID := strings.TrimSpace(stringValue(args["session_id"]))
	if requestedSessionID != "" {
		if err := s.bindAskSession(ctx, requestedSessionID, boundKBs, effectiveMode, effectiveProfile); err != nil {
			return errorResult(err), nil
		}
	}
	forward := map[string]any{"question": question}
	forward["mode"] = effectiveMode
	if effectiveProfile != "" {
		forward["profile_id"] = effectiveProfile
	}
	if requestedSessionID != "" {
		forward["session_id"] = requestedSessionID
	}
	if len(boundKBs) > 0 {
		forward["knowledge_base_ids"] = boundKBs
	}
	result, callErr := s.handleAsk(ctx, legacyRequest(forward))
	if callErr != nil {
		return nil, callErr
	}
	// Bind the effective scope, mode and profile to the session that
	// carried this ask so a future ask with the same session_id can be
	// refused if the caller switches any of them. Existing sessions
	// without a binding stay unbound (legacy behaviour) and a missing
	// LastRequestState on reuse is a no-op.
	if err := s.writeAskSessionBinding(ctx, result, boundKBs, effectiveMode, effectiveProfile); err != nil {
		// Best-effort: failing to record the binding must not fail the
		// ask. A future reuse falls through to the "unbound legacy row"
		// path and the caller can retry with a new session id.
		logger.Warnf(ctx, "write ask session binding failed for result: %v", err)
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

// sortDedupStrings normalises a kb id list so {a,b} and {b,a} bind to the
// same session.
func sortDedupStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// bindAskSession checks an existing session_id against the effective scope,
// mode and profile of this call. The session was created by a previous ask
// and stores its binding in LastRequestState; on a mismatch the caller must
// start a new session (the legacy 404 path), or the call is refused with a
// clear error so the caller can decide to drop the session_id.
func (s *Server) bindAskSession(
	ctx context.Context, sessionID string, kbIDs []string, mode, profileID string,
) error {
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return &ToolError{Code: CodeUnauthorized, Message: "unauthorized"}
	}
	ownerID := types.SessionOwnerIDFromContext(ctx)
	if ownerID == "" {
		ownerID = types.MCPEndpointPrincipal(ep.TenantID, ep.ID).StorageID()
	}
	session, err := s.sessionService.GetSessionByID(ctx, ep.TenantID, sessionID)
	if err != nil || session == nil || session.UserID != ownerID {
		return &ToolError{Code: CodeInvalidArgument,
			Message: "session_id does not belong to this caller; start a new session instead"}
	}
	if session.LastRequestState == nil {
		// Legacy unbound row (predates the binding migration): let the caller
		// keep using it for now. New code paths create sessions with
		// LastRequestState set on the first ask, so this path is taken by
		// already-running sessions only.
		return nil
	}
	boundKBs := sortDedupStrings(session.LastRequestState.KnowledgeBaseIDs)
	boundMode := session.LastRequestState.Mode
	boundProfile := session.LastRequestState.ProfileID
	if !sliceEqual(boundKBs, kbIDs) {
		return &ToolError{Code: CodeInvalidArgument,
			Message: "session_id was bound to a different knowledge_base set; start a new session"}
	}
	if boundMode != mode {
		return &ToolError{Code: CodeInvalidArgument,
			Message: "session_id was bound to a different mode; start a new session"}
	}
	if boundProfile != profileID {
		return &ToolError{Code: CodeInvalidArgument,
			Message: "session_id was bound to a different profile_id; start a new session"}
	}
	return nil
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeAskSessionBinding records the effective scope, mode and profile of
// this ask on the session the call ran against. The legacy description
// column is left alone; the binding lives in LastRequestState, which the
// chat UI also reads to rehydrate the input bar on reopen.
func (s *Server) writeAskSessionBinding(
	ctx context.Context, result *mcp.CallToolResult, kbIDs []string, mode, profileID string,
) error {
	sessionID, err := sessionIDFromAskResult(ctx, result)
	if err != nil || sessionID == "" {
		return err
	}
	state := &types.SessionLastRequestState{
		KnowledgeBaseIDs: append([]string(nil), kbIDs...),
		Mode:             mode,
		ProfileID:        profileID,
	}
	return s.sessionService.UpdateSessionLastRequestState(ctx, sessionID, state)
}

// sessionIDFromAskResult pulls the session id from an ask tool result. The
// legacy ask handler always embeds it in the structured payload under
// "session_id"; core_ask forwards the payload untouched.
func sessionIDFromAskResult(ctx context.Context, result *mcp.CallToolResult) (string, error) {
	if result == nil {
		return "", nil
	}
	outer, ok := result.StructuredContent.(map[string]any)
	if !ok {
		return "", nil
	}
	if s, ok := outer["session_id"].(string); ok {
		return s, nil
	}
	return "", nil
}
