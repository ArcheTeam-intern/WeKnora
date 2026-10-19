package mcpserver

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// These are the permanent form of the review's 2026-10-19 Standards probe:
// the first-time adoption of a legacy unbound session must be a
// compare-and-set, not a read-check-write. The barrier makes two requests
// observe the unbound snapshot together, exactly like two real requests that
// interleave their session reads; planAskSessions.AdoptSessionScopeBinding
// then serializes the CAS write the same way the repository's conditional
// UPDATE does (writes were already mutex-serialized; that is not the bug).

// concurrentAdoptionSessions adds the rendezvous: the first two
// GetSessionByID calls block until both have arrived, each receiving a copy
// of the row as it was at read time. Later reads — the loser's
// re-validation, resolveAskSession inside the forwarded ask — pass the live
// row through unchanged.
type concurrentAdoptionSessions struct {
	*planAskSessions
	mu    sync.Mutex
	reads int
	ready chan struct{}
}

func (s *concurrentAdoptionSessions) GetSessionByID(_ context.Context, _ uint64, id string) (*types.Session, error) {
	s.mu.Lock()
	row, ok := s.rows[id]
	if !ok {
		s.mu.Unlock()
		return nil, errors.New("session not found")
	}
	copyRow := *row
	s.reads++
	if s.reads == 2 {
		close(s.ready)
	}
	wait := s.reads <= 2
	s.mu.Unlock()
	if wait {
		<-s.ready
	}
	return &copyRow, nil
}

// AdoptSessionScopeBinding keeps the CAS write serialized under the same
// mutex the barrier reads use, so the adoption order cannot observe a torn
// row.
func (s *concurrentAdoptionSessions) AdoptSessionScopeBinding(
	ctx context.Context, sessionID string, state *types.SessionLastRequestState,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.planAskSessions.AdoptSessionScopeBinding(ctx, sessionID, state)
}

func legacyAdoptionFixture() (*Server, *planAskSessions, context.Context) {
	srv, rows := planAskServer()
	ep := planAskEndpoint()
	ep.KnowledgeBaseIDs = types.StringArray{"kb-1", "kb-2"}
	// A legacy row: owned by the caller, no binding of any kind.
	rows.rows["legacy-session"] = &types.Session{ID: "legacy-session", TenantID: 1, UserID: "user-1"}
	srv.sessionService = &concurrentAdoptionSessions{
		planAskSessions: rows,
		ready:           make(chan struct{}),
	}
	return srv, rows, planAskContext(ep, "user-1")
}

// runAdoptionRace fires one core ask for each kb in scopes against the same
// legacy session and reports how many actually executed.
func runAdoptionRace(t *testing.T, srv *Server, ctx context.Context, scopes []string) int {
	t.Helper()
	results := make(chan *mcp.CallToolResult, len(scopes))
	callErrs := make(chan error, len(scopes))
	for _, kb := range scopes {
		go func(kb string) {
			res, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
				"question": "barrier adoption", "session_id": "legacy-session",
				"knowledge_base_ids": []string{kb},
			}))
			results <- res
			callErrs <- err
		}(kb)
	}
	success := 0
	for i := 0; i < len(scopes); i++ {
		require.NoError(t, <-callErrs)
		res := <-results
		if !res.IsError {
			success++
		}
	}
	return success
}

// Different effective scopes racing to adopt one legacy session must not
// both execute: the review's probe saw 2 successes where at most 1 is
// allowed. The loser is refused by the re-validation of the winner's
// binding, before its question is written.
func TestCoreAskConcurrentLegacyAdoptionRefusesDifferentScope(t *testing.T) {
	srv, rows, ctx := legacyAdoptionFixture()

	require.Equal(t, 1, runAdoptionRace(t, srv, ctx, []string{"kb-1", "kb-2"}),
		"different effective scopes concurrently adopting the same legacy session must not both execute")

	bound := rows.rows["legacy-session"].LastRequestState
	require.NotNil(t, bound, "the winner's binding must be stored")
	require.Len(t, bound.KnowledgeBaseIDs, 1, "exactly one scope won the adoption")
	require.Contains(t, []string{"kb-1", "kb-2"}, bound.KnowledgeBaseIDs[0])
}

// Same effective scope is not a conflict: the loser re-validates an
// identical binding and both questions run. Refusing this would break the
// documented same-scope continuation guarantee under racing clients.
func TestCoreAskConcurrentLegacyAdoptionAllowsSameScope(t *testing.T) {
	srv, rows, ctx := legacyAdoptionFixture()

	require.Equal(t, 2, runAdoptionRace(t, srv, ctx, []string{"kb-1", "kb-1"}),
		"same-scope concurrent adoption must both proceed")
	bound := rows.rows["legacy-session"].LastRequestState
	require.NotNil(t, bound)
	require.Equal(t, []string{"kb-1"}, bound.KnowledgeBaseIDs)
}

// After a won adoption, the protection applies immediately: a later
// different scope is refused, which the legacy row was not (the adopted
// session behaves like a freshly bound one).
func TestCoreAskAdoptedSessionIsBoundLikeAFreshOne(t *testing.T) {
	srv, rows, ctx := legacyAdoptionFixture()
	require.Equal(t, 1, runAdoptionRace(t, srv, ctx, []string{"kb-1", "kb-2"}))
	winner := rows.rows["legacy-session"].LastRequestState.KnowledgeBaseIDs[0]

	loser := []string{"kb-2"}
	if winner == "kb-2" {
		loser = []string{"kb-1"}
	}
	res, err := srv.handleCoreAsk(ctx, coreCall(CoreToolAsk, map[string]any{
		"question": "after adoption", "session_id": "legacy-session",
		"knowledge_base_ids": loser,
	}))
	require.NoError(t, err)
	require.True(t, res.IsError, "the loser scope stays refused after adoption settled")
	require.Equal(t, "INVALID_ARGUMENT", errorCodeOf(res))
}
