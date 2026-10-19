package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Tencent/WeKnora/internal/types"
)

func coreAskTool() mcp.Tool {
	return mcp.NewTool(CoreToolAsk,
		mcp.WithDescription("Ask the platform's native knowledge QA. mode selects a built-in "+
			"answering path (quick-answer uses the fixed retrieval pipeline, smart-reasoning lets "+
			"the knowledge agent iterate over retrieval tools); omitting it runs the agent this "+
			"endpoint is configured with, and profile_id names that published configuration "+
			"explicitly. The answer comes back with its citations, the applied agent_id and "+
			"knowledge_base_ids, and the returned session_id continues the same conversation as "+
			"long as the effective scope, the agent and the endpoint stay the same."),
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

// askPlan is the resolved execution plan of one core ask: which agent answers,
// under which effective knowledge-base scope, and how the caller expressed
// it. It is built once, before any session validation, so the binding check
// and the recorded binding describe the run that actually executes instead of
// a paraphrase of the raw arguments.
type askPlan struct {
	// Agent is the agent that will answer: the built-in pinned by an explicit
	// mode, the endpoint's published profile (named or defaulted to), or the
	// quick-answer fallback when the endpoint configured nothing.
	Agent *types.CustomAgent
	// Mode is the caller-facing mode label: "", "quick-answer" or
	// "smart-reasoning". "" means the endpoint default decided.
	Mode string
	// ProfileID is the published profile the caller named, if any.
	ProfileID string
	// KBIDs is the effective retrieval scope: canonical ids of the knowledge
	// bases this question may read, resolved through the endpoint range and
	// the caller's authorization — never the raw request strings. An empty
	// known set is a real scope ("reads nothing"), distinct from the nil an
	// unresolved plan would carry.
	KBIDs []string
	// kbScopeKnown reports whether KBIDs was resolved to an explicit set
	// (request selection, endpoint range, caller-visible fallback, or the
	// agent's own configuration re-authorized — resolveAskKnowledgeBases
	// resolves all of these). When false the scope could not be pinned here;
	// validateAskSessionBinding then refuses, rather than skips, a comparison
	// against a session that was bound to a known set.
	kbScopeKnown bool
}

func (s *Server) handleCoreAsk(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := coreArgs(req)
	question := strings.TrimSpace(stringValue(args["question"]))
	if question == "" {
		return errorResult(invalidArgument("question is required")), nil
	}
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return mcp.NewToolResultError("unauthorized"), nil
	}

	plan, err := s.resolveAskPlan(ctx, ep, args)
	if err != nil {
		return errorResult(err), nil
	}

	sessionID := strings.TrimSpace(stringValue(args["session_id"]))
	if sessionID != "" {
		bound, err := s.validateAskSessionBinding(ctx, ep, sessionID, plan)
		if err != nil {
			return errorResult(err), nil
		}
		// A session that predates the binding is adopted here, before the
		// run. The adoption is a compare-and-set on "still unbound" and not
		// a read-check-write: two concurrent first-time adoptions of
		// different scopes cannot both execute, and a row can never stay
		// unbound after the caller has been told it works.
		if !bound {
			if err := s.adoptAskSessionBinding(ctx, ep, sessionID, plan); err != nil {
				return errorResult(err), nil
			}
		}
	} else {
		// A brand-new session is created WITH its binding, so an unbound
		// session can only ever be a row written by an older build. That is
		// what keeps "legacy compatibility" from doubling as a silent escape
		// hatch for a binding that failed to persist.
		created, err := s.createAskSession(ctx, ep, question)
		if err != nil {
			return errorResult(&ToolError{Code: CodeInternal, Message: err.Error()}), nil
		}
		sessionID = created.ID
		if err := s.storeAskSessionBinding(ctx, sessionID, ep, plan); err != nil {
			return errorResult(bindingPersistenceError(err)), nil
		}
	}

	forward := map[string]any{"question": question, "session_id": sessionID}
	// The plan already resolved the agent: forwarding the mode/profile is
	// only a label for the legacy payload. An empty mode is forwarded as an
	// empty string on purpose — the legacy resolver then picks the endpoint
	// default, which resolveAskPlan already validated and pinned.
	if plan.Mode != "" {
		forward["mode"] = plan.Mode
	}
	if plan.ProfileID != "" {
		forward["profile_id"] = plan.ProfileID
	}
	if len(plan.KBIDs) > 0 {
		forward["knowledge_base_ids"] = plan.KBIDs
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
	return jsonResult(payload)
}

// bindingPersistenceError renders the refusal for a session whose binding
// could not be stored. The run does not start, so no answer was produced for
// a session that is not protected.
func bindingPersistenceError(cause error) *ToolError {
	return &ToolError{
		Code:    CodeInternal,
		Message: "the session scope binding could not be persisted, so the question was not run",
		Details: map[string]any{"reason": cause.Error()},
	}
}

// resolveAskPlan turns the caller's arguments into the execution plan: which
// agent answers and which knowledge bases it reads. Everything downstream —
// the permission to reuse a session, the binding written back, the forwarded
// arguments — is derived from this plan, so the three can never disagree.
//
// Resolution order mirrors the legacy ask handler exactly (mode pins a
// built-in agent; otherwise the endpoint's published profile; otherwise
// quick-answer), except it happens here so the choice is inspectable before
// the run rather than implied by which arguments were forwarded.
func (s *Server) resolveAskPlan(
	ctx context.Context, ep *types.MCPEndpoint, args map[string]any,
) (*askPlan, error) {
	mode, err := coreStringArg(args, "mode", "")
	if err != nil {
		return nil, err
	}
	profileID := strings.TrimSpace(stringValue(args["profile_id"]))
	plan := &askPlan{Mode: mode, ProfileID: profileID}

	if profileID != "" {
		// A profile is not an agent id: the only id a caller may name is the
		// one this endpoint publishes. Anything else would let a caller select
		// an internal agent the deployment never offered.
		if profileID != ep.DefaultAgentID {
			return nil, &ToolError{
				Code: CodeInvalidArgument,
				Message: "profile_id is not a profile published by this endpoint; call " +
					"knowledge_base.get and use a qa_profiles entry, or omit profile_id",
				Details: map[string]any{"published_profiles": ep.DefaultAgentID},
			}
		}
		if mode != "" {
			return nil, invalidArgument(
				"profile_id and mode cannot be combined: the profile already fixes the answering path")
		}
	}
	if mode != "" && profileID == "" {
		if _, ok := askModeAgents[mode]; !ok {
			return nil, invalidArgument(
				"mode %q is not one of the native QA modes: quick-answer, smart-reasoning", mode)
		}
	}

	// This call is the V1 fix in one place. An explicit mode pins its built-in
	// agent; an omitted mode or a named profile leaves the choice to the
	// endpoint configuration. The previous revision hard-coded quick-answer
	// before forwarding, silently overriding both the endpoint default and a
	// published profile.
	agent, err := s.resolveAskAgent(ctx, ep, plan.Mode)
	if err != nil {
		return nil, &ToolError{Code: CodeInvalidArgument, Message: err.Error()}
	}
	plan.Agent = agent

	kbIDs, err := coreKnowledgeBaseIDs(args)
	if err != nil {
		return nil, err
	}
	resolved, err := s.resolveAskKnowledgeBases(ctx, ep, agent, kbIDs)
	if err != nil {
		return nil, &ToolError{Code: CodeInvalidArgument, Message: err.Error()}
	}
	plan.KBIDs = sortDedupStrings(resolved)
	plan.kbScopeKnown = resolved != nil
	return plan, nil
}

// validateAskSessionBinding checks a session reuse against the execution plan
// of this call: another endpoint, another answering agent (an endpoint
// default change shows up here too), another caller-facing mode/profile, or
// another effective knowledge-base scope. The owner check runs first,
// unchanged.
//
// The boolean return reports whether the session already carries a binding.
// false means the row predates the binding (written by a build without it);
// the caller adopts it with a strict write before running, so a failure there
// refuses the call instead of leaving an unprotected session in circulation.
func (s *Server) validateAskSessionBinding(
	ctx context.Context, ep *types.MCPEndpoint, sessionID string, plan *askPlan,
) (bool, error) {
	ownerID := types.SessionOwnerIDFromContext(ctx)
	if ownerID == "" {
		ownerID = types.MCPEndpointPrincipal(ep.TenantID, ep.ID).StorageID()
	}
	session, err := s.sessionService.GetSessionByID(ctx, ep.TenantID, sessionID)
	if err != nil || session == nil || session.UserID != ownerID {
		return false, &ToolError{Code: CodeInvalidArgument,
			Message: "session_id does not belong to this caller; start a new session instead"}
	}
	bound := session.LastRequestState
	if bound == nil || bound.EndpointID == "" {
		// No binding: a row from an older build. The caller adopts it; a
		// session created by this surface always has one (see handleCoreAsk).
		return false, nil
	}
	if bound.EndpointID != ep.ID {
		return true, &ToolError{Code: CodeInvalidArgument,
			Message: "session_id belongs to a different MCP endpoint; start a new session"}
	}
	if bound.AskAgentID != plan.Agent.ID {
		return true, &ToolError{Code: CodeInvalidArgument,
			Message: "session_id was bound to a different answering configuration; start a new session"}
	}
	if bound.Mode != plan.Mode {
		return true, &ToolError{Code: CodeInvalidArgument,
			Message: "session_id was bound to a different mode; start a new session"}
	}
	if bound.ProfileID != plan.ProfileID {
		return true, &ToolError{Code: CodeInvalidArgument,
			Message: "session_id was bound to a different profile_id; start a new session"}
	}
	if plan.kbScopeKnown {
		if !sliceEqual(sortDedupStrings(bound.KnowledgeBaseIDs), plan.KBIDs) {
			return true, &ToolError{Code: CodeInvalidArgument,
				Message: "session_id was bound to a different knowledge_base set; start a new session"}
		}
	} else if len(bound.KnowledgeBaseIDs) > 0 {
		// Defence in depth: resolveAskKnowledgeBases resolves every scope —
		// including the agent's own default — to a canonical set, so a plan
		// with an unknown scope should not exist. If one ever does, it must
		// not continue a session bound to an explicit set: an unchecked
		// default is exactly the explicit->agent-owned drift the review
		// caught (2026-10-19 P2). Start fresh instead.
		return true, &ToolError{Code: CodeInvalidArgument,
			Message: "this request cannot pin its knowledge-base scope; pass knowledge_base_ids explicitly or start a new session"}
	}
	return true, nil
}

// bindingStateFor renders the plan as the stored binding. Fresh sessions and
// legacy adoptions write the same shape, so the two paths can never
// disagree about what a binding means.
func bindingStateFor(ep *types.MCPEndpoint, plan *askPlan) *types.SessionLastRequestState {
	state := &types.SessionLastRequestState{
		Mode:       plan.Mode,
		ProfileID:  plan.ProfileID,
		EndpointID: ep.ID,
		AskAgentID: plan.Agent.ID,
	}
	if plan.kbScopeKnown {
		state.KnowledgeBaseIDs = append([]string(nil), plan.KBIDs...)
	}
	return state
}

// storeAskSessionBinding writes the plan onto a session this surface just
// created. Strict by design: StoreSessionScopeBinding reports a repository
// error AND a zero-row update, and either one must stop the call — an
// unpersisted binding would read as "unbound" on the next reuse.
func (s *Server) storeAskSessionBinding(
	ctx context.Context, sessionID string, ep *types.MCPEndpoint, plan *askPlan,
) error {
	return s.sessionService.StoreSessionScopeBinding(ctx, sessionID, bindingStateFor(ep, plan))
}

// adoptAskSessionBinding binds a session row that predates the binding. The
// service performs it as a compare-and-set ("write only while still
// unbound"), so a concurrent adoption cannot quietly overwrite the other's:
// the CAS loser re-validates the binding that actually won. Same scope runs
// on; a different scope — or a row that vanished or somehow shows no binding
// after a lost write — is refused before any question message is written
// (review 2026-10-19 Standards P2).
func (s *Server) adoptAskSessionBinding(
	ctx context.Context, ep *types.MCPEndpoint, sessionID string, plan *askPlan,
) error {
	adopted, err := s.sessionService.AdoptSessionScopeBinding(ctx, sessionID, bindingStateFor(ep, plan))
	if err != nil {
		return bindingPersistenceError(err)
	}
	if adopted {
		return nil
	}
	bound, err := s.validateAskSessionBinding(ctx, ep, sessionID, plan)
	if err != nil {
		return err
	}
	if !bound {
		return &ToolError{
			Code:    CodeInternal,
			Message: "the session binding was being adopted concurrently; retry the question",
		}
	}
	return nil
}

// createAskSession creates the session a new core ask runs in, owned by the
// caller. resolveAskSession performs the same steps for the legacy tool; the
// core surface does it up front because the binding must exist before the
// first message is written.
func (s *Server) createAskSession(
	ctx context.Context, ep *types.MCPEndpoint, question string,
) (*types.Session, error) {
	ownerID := types.SessionOwnerIDFromContext(ctx)
	if ownerID == "" {
		ownerID = types.MCPEndpointPrincipal(ep.TenantID, ep.ID).StorageID()
	}
	title := question
	if runes := []rune(title); len(runes) > askSessionTitleMaxLen {
		title = string(runes[:askSessionTitleMaxLen])
	}
	created, err := s.sessionService.CreateSession(ctx, &types.Session{
		TenantID:    ep.TenantID,
		Title:       title,
		Description: "MCP endpoint " + ep.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	if err := s.sessionService.SetSessionOwnerID(ctx, ep.TenantID, created.ID, ownerID); err != nil {
		return nil, fmt.Errorf("failed to bind session: %w", err)
	}
	created.UserID = ownerID
	return created, nil
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
