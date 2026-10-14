package mcpserver

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// askScopeSessions records the owner bound by resolveAskSession and serves
// GetSessionByID like the real tenant-scoped repository (no user predicate —
// the owner comparison is the scope guard, exactly like production).
type askScopeSessions struct {
	interfaces.SessionService
	rows map[string]*types.Session
}

func (s *askScopeSessions) CreateSession(_ context.Context, session *types.Session) (*types.Session, error) {
	if s.rows == nil {
		s.rows = map[string]*types.Session{}
	}
	session.ID = "session-1"
	s.rows[session.ID] = session
	return session, nil
}

func (s *askScopeSessions) SetSessionOwnerID(_ context.Context, _ uint64, sessionID, ownerID string) error {
	row, ok := s.rows[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	row.UserID = ownerID
	return nil
}

func (s *askScopeSessions) GetSessionByID(_ context.Context, _ uint64, id string) (*types.Session, error) {
	row, ok := s.rows[id]
	if !ok {
		return nil, errors.New("session not found")
	}
	return row, nil
}

func (s *askScopeSessions) KnowledgeQA(ctx context.Context, _ *types.QARequest, bus *event.EventBus) error {
	return bus.EmitAndWait(ctx, event.Event{
		Type: event.EventAgentFinalAnswer,
		Data: event.AgentFinalAnswerData{Content: "ok", Done: true},
	})
}

// askScopeMessages mirrors messageService.CreateMessage's session lookup:
// the stored owner must equal types.SessionOwnerIDFromContext(ctx), the way
// sessionRepo.Get's user scope does in production. A mismatch means
// "session not found".
type askScopeMessages struct {
	interfaces.MessageService
	sessions map[string]*types.Session
}

func (m *askScopeMessages) CreateMessage(ctx context.Context, message *types.Message) (*types.Message, error) {
	row, ok := m.sessions[message.SessionID]
	if !ok || row.UserID != types.SessionOwnerIDFromContext(ctx) {
		return nil, errors.New("session not found")
	}
	message.ID = "message-" + message.Role
	return message, nil
}

func (m *askScopeMessages) UpdateMessage(context.Context, *types.Message) error { return nil }

// newAskScopeServer wires the minimum the ask tool needs, sharing one
// session map between the session and message fakes.
func newAskScopeServer() (*Server, *askScopeSessions) {
	kb := &types.KnowledgeBase{ID: "kb-1", TenantID: 1, Name: "KB"}
	sessions := &askScopeSessions{rows: map[string]*types.Session{}}
	srv := &Server{
		kbService:      &stubKBService{kbs: map[string]*types.KnowledgeBase{kb.ID: kb}},
		kbShareService: &stubKBShareService{shared: map[string]types.OrgMemberRole{kb.ID: types.OrgRoleViewer}},
		tenantService:  &stubTenantService{tenants: map[uint64]*types.Tenant{1: {ID: 1}}},
		agentService: &stubAgentService{agents: map[string]*types.CustomAgent{
			types.BuiltinQuickAnswerID: {ID: types.BuiltinQuickAnswerID, IsBuiltin: true},
		}},
		sessionService: sessions,
		messageService: &askScopeMessages{sessions: sessions.rows},
	}
	return srv, sessions
}

func askScopeEndpoint() *types.MCPEndpoint {
	return &types.MCPEndpoint{
		ID: "ep", TenantID: 1, KnowledgeBaseIDs: types.StringArray{"kb-1"},
		Tools: types.StringArray{types.MCPEndpointToolAsk},
	}
}

func callAsk(ctx context.Context, srv *Server) (*mcp.CallToolResult, error) {
	return srv.handleAsk(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Arguments: map[string]any{"question": "hello", "knowledge_base_ids": []string{"kb-1"}},
	}})
}

// Regression: in user mode the ask tool bound the session to the endpoint's
// machine principal while CreateMessage resolved it through the caller's
// real user id, so every user-mode ask failed with
// "failed to create user message: session not found". The session owner must
// be the caller's session identity.
func TestAskBindsSessionOwnerToTheCallingUser(t *testing.T) {
	srv, sessions := newAskScopeServer()
	ep := askScopeEndpoint()
	ctx := mcpCallContext(1, ep)
	ctx = context.WithValue(ctx, types.UserIDContextKey, "user-1")
	ctx = types.WithPrincipal(ctx, types.Principal{Type: types.PrincipalWebUser, ID: "user-1"})

	result, err := callAsk(ctx, srv)
	require.NoError(t, err)
	require.False(t, result.IsError, "user-mode ask must succeed: %#v", result.Content)
	require.Equal(t, "user-1", sessions.rows["session-1"].UserID,
		"the session must be bound to the calling user, not the endpoint principal")
}

// Machine mode keeps its historical binding: the endpoint principal owns the
// session (Watch out — types.SessionOwnerIDFromContext returns exactly this
// storage id for a machine-mode principal).
func TestAskBindsSessionOwnerToTheEndpointPrincipalInMachineMode(t *testing.T) {
	srv, sessions := newAskScopeServer()
	ep := askScopeEndpoint()
	ctx := mcpCallContext(1, ep)
	ctx = types.WithPrincipal(ctx, types.MCPEndpointPrincipal(ep.TenantID, ep.ID))

	result, err := callAsk(ctx, srv)
	require.NoError(t, err)
	require.False(t, result.IsError, "%#v", result.Content)
	require.Equal(t, types.MCPEndpointPrincipal(ep.TenantID, ep.ID).StorageID(),
		sessions.rows["session-1"].UserID)
}

// A continuation by a different caller must not resolve: the owner check
// keeps one user out of another user's session even though both are valid
// members of the workspace.
func TestAskRejectsContinuingAnotherCallersSession(t *testing.T) {
	srv, sessions := newAskScopeServer()
	ep := askScopeEndpoint()
	ownerCtx := mcpCallContext(1, ep)
	ownerCtx = context.WithValue(ownerCtx, types.UserIDContextKey, "user-1")
	ownerCtx = types.WithPrincipal(ownerCtx, types.Principal{Type: types.PrincipalWebUser, ID: "user-1"})
	first, err := callAsk(ownerCtx, srv)
	require.NoError(t, err)
	require.False(t, first.IsError)
	_ = sessions

	intruderCtx := mcpCallContext(1, ep)
	intruderCtx = context.WithValue(intruderCtx, types.UserIDContextKey, "user-2")
	intruderCtx = types.WithPrincipal(intruderCtx, types.Principal{Type: types.PrincipalWebUser, ID: "user-2"})
	result, err := srv.handleAsk(intruderCtx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Arguments: map[string]any{"question": "hi", "knowledge_base_ids": []string{"kb-1"},
			"session_id": "session-1"},
	}})
	require.NoError(t, err)
	require.True(t, result.IsError, "another caller's session must not be continuable")
}
