package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// These tests are the permanent form of the closure-review counterexamples:
// V1 (the endpoint's published profile must actually run), V2 (the binding is
// the effective scope and endpoint, not the request arguments) and V3 (a
// binding that cannot be persisted must not hand back a continuable session).
// The controls stay alongside them so a fix cannot pass by refusing
// everything.

// planAskSessions is a session service that records the persisted binding and
// can fail that write on demand.
type planAskSessions struct {
	interfaces.SessionService
	rows map[string]*types.Session
	// failBinding makes StoreSessionScopeBinding fail without touching the row.
	failBinding bool
	// zeroRowBinding makes StoreSessionScopeBinding report a no-op update, the
	// other failure mode the review called out.
	zeroRowBinding bool
	creates        int
	nextID         int
}

func (s *planAskSessions) CreateSession(_ context.Context, session *types.Session) (*types.Session, error) {
	if s.rows == nil {
		s.rows = map[string]*types.Session{}
	}
	s.creates++
	s.nextID++
	id := "session-" + string(rune('0'+s.nextID))
	session.ID = id
	s.rows[id] = session
	return session, nil
}

func (s *planAskSessions) SetSessionOwnerID(_ context.Context, _ uint64, sessionID, ownerID string) error {
	row, ok := s.rows[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	row.UserID = ownerID
	return nil
}

func (s *planAskSessions) GetSessionByID(_ context.Context, _ uint64, id string) (*types.Session, error) {
	row, ok := s.rows[id]
	if !ok {
		return nil, errors.New("session not found")
	}
	return row, nil
}

func (s *planAskSessions) StoreSessionScopeBinding(
	_ context.Context, sessionID string, state *types.SessionLastRequestState,
) error {
	if s.failBinding {
		return errors.New("simulated binding persistence failure")
	}
	if s.zeroRowBinding {
		return errors.New("session was not updated (missing or out of scope)")
	}
	row, ok := s.rows[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	row.LastRequestState = state
	return nil
}

// UpdateSessionLastRequestState is the historical best-effort write. The fake
// keeps it so a regression to the best-effort path makes these tests fail with
// an assertion instead of a nil-interface panic.
func (s *planAskSessions) UpdateSessionLastRequestState(
	_ context.Context, sessionID string, state *types.SessionLastRequestState,
) error {
	if s.failBinding {
		return errors.New("simulated binding persistence failure")
	}
	row, ok := s.rows[sessionID]
	if !ok {
		return nil
	}
	row.LastRequestState = state
	return nil
}

func (s *planAskSessions) KnowledgeQA(ctx context.Context, _ *types.QARequest, bus *event.EventBus) error {
	return bus.EmitAndWait(ctx, event.Event{
		Type: event.EventAgentFinalAnswer,
		Data: event.AgentFinalAnswerData{Content: "ok", Done: true},
	})
}

// planAskServer wires the fixtures over two knowledge bases so a scope change
// is expressible.
func planAskServer() (*Server, *planAskSessions) {
	sessions := &planAskSessions{rows: map[string]*types.Session{}}
	srv := &Server{
		kbService: &stubKBService{kbs: map[string]*types.KnowledgeBase{
			"kb-1": {ID: "kb-1", TenantID: 1, Name: "KB one"},
			"kb-2": {ID: "kb-2", TenantID: 1, Name: "KB two"},
		}},
		kbShareService: &stubKBShareService{shared: map[string]types.OrgMemberRole{
			"kb-1": types.OrgRoleViewer,
			"kb-2": types.OrgRoleViewer,
		}},
		tenantService: &stubTenantService{tenants: map[uint64]*types.Tenant{1: {ID: 1}}},
		agentService: &stubAgentService{agents: map[string]*types.CustomAgent{
			types.BuiltinQuickAnswerID: {ID: types.BuiltinQuickAnswerID, IsBuiltin: true},
			types.BuiltinSmartReasoningID: {
				ID: types.BuiltinSmartReasoningID, IsBuiltin: true,
			},
			"published-profile": {ID: "published-profile", TenantID: 1},
		}},
		sessionService: sessions,
		messageService: &planAskMessages{sessions: sessions},
	}
	return srv, sessions
}

// planAskMessages resolves messages exactly like the real service: through the
// session row and its owner scope.
type planAskMessages struct {
	interfaces.MessageService
	sessions *planAskSessions
}

func (m *planAskMessages) CreateMessage(ctx context.Context, message *types.Message) (*types.Message, error) {
	row, ok := m.sessions.rows[message.SessionID]
	if !ok || row.UserID != types.SessionOwnerIDFromContext(ctx) {
		return nil, errors.New("session not found")
	}
	message.ID = "message-" + message.Role
	return message, nil
}

func (m *planAskMessages) UpdateMessage(context.Context, *types.Message) error { return nil }

func planAskEndpoint() *types.MCPEndpoint {
	return &types.MCPEndpoint{
		ID: "ep-1", TenantID: 1, Name: "acceptance endpoint",
		KnowledgeBaseIDs: types.StringArray{"kb-1"},
		Tools:            types.StringArray{types.MCPEndpointToolAsk},
	}
}

func planAskContext(ep *types.MCPEndpoint, userID string) context.Context {
	ctx := mcpCallContext(1, ep)
	ctx = context.WithValue(ctx, types.UserIDContextKey, userID)
	return types.WithPrincipal(ctx, types.Principal{Type: types.PrincipalWebUser, ID: userID})
}

// --------------------------------------------------------------------------
// V1: the endpoint's default agent and its published profile must run
// --------------------------------------------------------------------------

func TestCoreAskRunsTheEndpointPublishedProfile(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"omitted mode":        {"question": "q", "knowledge_base_ids": []string{"kb-1"}},
		"explicit profile_id": {"question": "q", "knowledge_base_ids": []string{"kb-1"}, "profile_id": "published-profile"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := planAskServer()
			ep := planAskEndpoint()
			ep.DefaultAgentID = "published-profile"

			result, err := srv.handleCoreAsk(planAskContext(ep, "user-1"), coreCall(CoreToolAsk, args))
			require.NoError(t, err)
			require.False(t, result.IsError, "ask must succeed: %s", mcpErrorText(result))
			require.Equal(t, "published-profile", structuredOf(result)["agent_id"],
				"the endpoint's published configuration must be the one that answers")
		})
	}
}

func TestCoreAskExplicitModeStillPinsItsBuiltin(t *testing.T) {
	srv, _ := planAskServer()
	ep := planAskEndpoint()
	ep.DefaultAgentID = "published-profile"

	result, err := srv.handleCoreAsk(planAskContext(ep, "user-1"), coreCall(CoreToolAsk, map[string]any{
		"question": "q", "knowledge_base_ids": []string{"kb-1"}, "mode": "quick-answer",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", mcpErrorText(result))
	require.Equal(t, types.BuiltinQuickAnswerID, structuredOf(result)["agent_id"],
		"an explicit mode must override the endpoint default")
}

func TestCoreAskFallsBackToTheBuiltinWhenTheEndpointPublishesNothing(t *testing.T) {
	srv, _ := planAskServer()
	ep := planAskEndpoint() // no DefaultAgentID

	result, err := srv.handleCoreAsk(planAskContext(ep, "user-1"), coreCall(CoreToolAsk, map[string]any{
		"question": "q", "knowledge_base_ids": []string{"kb-1"},
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", mcpErrorText(result))
	require.Equal(t, types.BuiltinQuickAnswerID, structuredOf(result)["agent_id"])
}

func TestCoreAskRefusesUnknownProfilesAndMixedChoices(t *testing.T) {
	srv, _ := planAskServer()
	ep := planAskEndpoint()
	ep.DefaultAgentID = "published-profile"
	ctx := planAskContext(ep, "user-1")

	for name, args := range map[string]map[string]any{
		"profile the endpoint does not publish": {"question": "q", "profile_id": "some-internal-agent"},
		"profile combined with a mode":          {"question": "q", "profile_id": "published-profile", "mode": "quick-answer"},
		"unknown mode":                          {"question": "q", "mode": "reasoning-please"},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, args))
			require.NoError(t, err)
			require.True(t, result.IsError, "%s must be refused", name)
			require.Equal(t, "INVALID_ARGUMENT", errorCodeOf(result), "refusal: %s", mcpErrorText(result))
		})
	}
}

// --------------------------------------------------------------------------
// V2: the binding is the effective scope and the endpoint
// --------------------------------------------------------------------------

// The review's counterexample: the first ask omits the selector (so the
// endpoint range decides), and the endpoint range then changes. The old code
// bound nil and let the second ask run against the new range.
func TestCoreAskBindsTheEffectiveScopeNotTheRequestArguments(t *testing.T) {
	srv, sessions := planAskServer()
	ep := planAskEndpoint()
	ctx := planAskContext(ep, "user-1")

	first, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{"question": "first"}))
	require.NoError(t, err)
	require.False(t, first.IsError, "%s", mcpErrorText(first))
	require.Equal(t, []string{"kb-1"}, sessions.rows["session-1"].LastRequestState.KnowledgeBaseIDs,
		"the binding must record the resolved scope, not the empty request")

	// The endpoint's effective range moves to kb-2; the caller keeps omitting
	// the selector. Same session id, different scope -> refused.
	ep.KnowledgeBaseIDs = types.StringArray{"kb-2"}
	second, err := srv.handleCoreAsk(planAskContext(ep, "user-1"), coreCall(CoreToolAsk, map[string]any{
		"question": "next", "session_id": "session-1",
	}))
	require.NoError(t, err)
	require.True(t, second.IsError,
		"an omitted selector whose effective scope changed must be refused: %+v", structuredOf(second))
}

// Equivalent requests must not be mistaken for a scope change: the same set
// expressed as an omission or as an explicit list binds the same session.
func TestCoreAskTreatsEquivalentScopeExpressionsAsTheSameBinding(t *testing.T) {
	srv, _ := planAskServer()
	ep := planAskEndpoint()
	ctx := planAskContext(ep, "user-1")

	first, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{"question": "first"}))
	require.NoError(t, err)
	require.False(t, first.IsError, "%s", mcpErrorText(first))

	explicit, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
		"question": "same scope by id", "session_id": "session-1", "knowledge_base_ids": []string{"kb-1"},
	}))
	require.NoError(t, err)
	require.False(t, explicit.IsError,
		"the endpoint range and an explicit single id are the same scope: %s", mcpErrorText(explicit))

	byName, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
		"question": "same scope by name", "session_id": "session-1", "knowledge_base_ids": []string{"KB one"},
	}))
	require.NoError(t, err)
	require.False(t, byName.IsError, "a name resolving to the same id is the same scope: %s", mcpErrorText(byName))
}

func TestCoreAskRefusesASessionFromAnotherEndpoint(t *testing.T) {
	srv, _ := planAskServer()
	ep := planAskEndpoint()
	ctx := planAskContext(ep, "user-1")

	first, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
		"question": "first", "knowledge_base_ids": []string{"kb-1"},
	}))
	require.NoError(t, err)
	require.False(t, first.IsError, "%s", mcpErrorText(first))

	otherEP := planAskEndpoint()
	otherEP.ID = "ep-2"
	otherEP.Name = "another endpoint"
	reused, err := srv.handleCoreAsk(planAskContext(otherEP, "user-1"), coreCall(CoreToolAsk, map[string]any{
		"question": "next", "session_id": "session-1", "knowledge_base_ids": []string{"kb-1"},
	}))
	require.NoError(t, err)
	require.True(t, reused.IsError,
		"the same user on another endpoint must not reuse the session: %+v", structuredOf(reused))
}

// A binding that records the agent also catches an endpoint default that
// moves between two asks, because the effective agent changes.
func TestCoreAskRefusesWhenTheAnsweringAgentChanges(t *testing.T) {
	srv, _ := planAskServer()
	ep := planAskEndpoint()
	ctx := planAskContext(ep, "user-1")

	first, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
		"question": "first", "knowledge_base_ids": []string{"kb-1"},
	}))
	require.NoError(t, err)
	require.False(t, first.IsError, "%s", mcpErrorText(first))

	ep.DefaultAgentID = "published-profile"
	next, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
		"question": "next", "session_id": "session-1", "knowledge_base_ids": []string{"kb-1"},
	}))
	require.NoError(t, err)
	require.True(t, next.IsError, "a changed answering configuration must be refused: %+v", structuredOf(next))
}

// Controls: the same scope keeps working, a different caller is still
// refused, and a session can be started when the requested one is rejected.
func TestCoreAskKeepsTheExistingReuseControls(t *testing.T) {
	srv, _ := planAskServer()
	ep := planAskEndpoint()
	ep.KnowledgeBaseIDs = types.StringArray{"kb-1", "kb-2"}
	ctx := planAskContext(ep, "user-1")

	first, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
		"question": "first", "knowledge_base_ids": []string{"kb-1"},
	}))
	require.NoError(t, err)
	require.False(t, first.IsError, "%s", mcpErrorText(first))

	same, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
		"question": "same", "session_id": "session-1", "knowledge_base_ids": []string{"kb-1"},
	}))
	require.NoError(t, err)
	require.False(t, same.IsError, "same scope must continue: %s", mcpErrorText(same))

	// The same set in another order is not a scope change.
	reordered, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
		"question": "reordered", "session_id": "session-1",
		"knowledge_base_ids": []string{"kb-2", "kb-1"},
	}))
	require.NoError(t, err)
	require.True(t, reordered.IsError, "a different set must be refused even in a different order")

	changed, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
		"question": "changed", "session_id": "session-1", "knowledge_base_ids": []string{"kb-2"},
	}))
	require.NoError(t, err)
	require.True(t, changed.IsError, "an explicit scope change must be refused")

	other, err := srv.handleCoreAsk(planAskContext(ep, "user-2"), coreCall(CoreToolAsk, map[string]any{
		"question": "other", "session_id": "session-1", "knowledge_base_ids": []string{"kb-1"},
	}))
	require.NoError(t, err)
	require.True(t, other.IsError, "another caller must not continue the session")

	// A fresh session at the new scope is accepted, which is the documented
	// recovery path.
	fresh, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
		"question": "fresh", "knowledge_base_ids": []string{"kb-2"},
	}))
	require.NoError(t, err)
	require.False(t, fresh.IsError, "a new session at the new scope must work: %s", mcpErrorText(fresh))
}

// --------------------------------------------------------------------------
// V3: a binding that cannot be persisted must not produce a usable session
// --------------------------------------------------------------------------

func TestCoreAskRefusesWhenTheBindingCannotBePersisted(t *testing.T) {
	for name, configure := range map[string]func(*planAskSessions){
		"repository error": func(s *planAskSessions) { s.failBinding = true },
		"zero-row update":  func(s *planAskSessions) { s.zeroRowBinding = true },
	} {
		t.Run(name, func(t *testing.T) {
			srv, sessions := planAskServer()
			ep := planAskEndpoint()
			ep.KnowledgeBaseIDs = types.StringArray{"kb-1", "kb-2"}
			configure(sessions)
			ctx := planAskContext(ep, "user-1")

			first, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
				"question": "first", "knowledge_base_ids": []string{"kb-1"},
			}))
			require.NoError(t, err)
			require.True(t, first.IsError,
				"an unpersisted binding must not yield a session: %+v", structuredOf(first))
			require.Contains(t, mcpErrorText(first)+jsonDetails(first), "binding")

			// Nothing was written for it, so there is no unbound row a later
			// call could mistake for a legacy session.
			for _, row := range sessions.rows {
				require.Nil(t, row.LastRequestState)
			}

			// A second call with a different scope must not succeed on the
			// strength of the failed one.
			second, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
				"question": "next", "session_id": "session-1", "knowledge_base_ids": []string{"kb-2"},
			}))
			require.NoError(t, err)
			require.True(t, second.IsError, "the failed binding must not become a usable session")
		})
	}
}

// A row written before the binding existed is adopted on its next use — and
// the adoption is strict, so it is either protected or refused.
func TestCoreAskAdoptsLegacySessionsStrictly(t *testing.T) {
	srv, sessions := planAskServer()
	ep := planAskEndpoint()
	ep.KnowledgeBaseIDs = types.StringArray{"kb-1", "kb-2"}
	// A legacy row: owner set, no last-request state at all.
	sessions.rows["session-1"] = &types.Session{ID: "session-1", TenantID: 1, UserID: "user-1"}

	adopted, err := srv.handleCoreAsk(planAskContext(ep, "user-1"), coreCall(CoreToolAsk, map[string]any{
		"question": "legacy", "session_id": "session-1", "knowledge_base_ids": []string{"kb-1"},
	}))
	require.NoError(t, err)
	require.False(t, adopted.IsError, "a legacy session must stay usable: %s", mcpErrorText(adopted))
	bound := sessions.rows["session-1"].LastRequestState
	require.NotNil(t, bound, "the legacy row must be bound before the run")
	require.Equal(t, "ep-1", bound.EndpointID)
	require.Equal(t, []string{"kb-1"}, bound.KnowledgeBaseIDs)

	// And the adoption immediately protects: a different scope is refused.
	changed, err := srv.handleCoreAsk(planAskContext(ep, "user-1"), coreCall(CoreToolAsk, map[string]any{
		"question": "changed", "session_id": "session-1", "knowledge_base_ids": []string{"kb-2"},
	}))
	require.NoError(t, err)
	require.True(t, changed.IsError, "an adopted session must be protected like a new one")

	// A legacy session whose adoption cannot be stored is refused outright.
	sessions.rows["session-2"] = &types.Session{ID: "session-2", TenantID: 1, UserID: "user-1"}
	sessions.failBinding = true
	refused, err := srv.handleCoreAsk(planAskContext(ep, "user-1"), coreCall(CoreToolAsk, map[string]any{
		"question": "legacy again", "session_id": "session-2", "knowledge_base_ids": []string{"kb-1"},
	}))
	require.NoError(t, err)
	require.True(t, refused.IsError, "an adoption that cannot be stored must not run")
}

// errorCodeOf reads the machine-readable code out of an error result, the
// same way a client does.
func errorCodeOf(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	payload, ok := result.StructuredContent.(map[string]any)
	if !ok {
		return ""
	}
	raw, ok := payload["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := raw["code"].(string)
	return code
}

// jsonDetails renders the error details so assertions can look for the reason.
func jsonDetails(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	if payload, ok := result.StructuredContent.(map[string]any); ok {
		raw, err := json.Marshal(payload)
		if err == nil {
			return string(raw)
		}
	}
	return ""
}
