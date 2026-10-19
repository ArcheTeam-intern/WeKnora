package repository

import (
	"context"
	"testing"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newSessionRepositoryForTest(t *testing.T) (interfaces.SessionRepository, *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Session{}))

	return NewSessionRepository(db), db
}

func createSessionForTest(t *testing.T, db *gorm.DB, tenantID uint64, userID string) *types.Session {
	t.Helper()

	session := &types.Session{
		TenantID: tenantID,
		UserID:   userID,
		Title:    userID + " session",
	}
	if userID == "" {
		session.Title = "legacy tenant session"
	}
	require.NoError(t, db.Create(session).Error)

	return session
}

func countActiveSessionsForTest(t *testing.T, db *gorm.DB, id string) int64 {
	t.Helper()

	var count int64
	require.NoError(t, db.Model(&types.Session{}).Where("id = ?", id).Count(&count).Error)
	return count
}

func sessionIDsForTest(sessions []*types.Session) []string {
	ids := make([]string, 0, len(sessions))
	for _, session := range sessions {
		ids = append(ids, session.ID)
	}
	return ids
}

func TestSessionRepositoryGetAndListHonorUserScope(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	ctx := context.Background()
	aliceSession := createSessionForTest(t, db, 1, "alice")
	bobSession := createSessionForTest(t, db, 1, "bob")
	legacySession := createSessionForTest(t, db, 1, "")
	_ = createSessionForTest(t, db, 2, "bob")

	_, err := repo.Get(ctx, 1, "bob", aliceSession.ID)
	require.ErrorIs(t, err, apperrors.ErrSessionNotFound)

	got, err := repo.Get(ctx, 1, "bob", bobSession.ID)
	require.NoError(t, err)
	require.Equal(t, bobSession.ID, got.ID)

	got, err = repo.Get(ctx, 1, "bob", legacySession.ID)
	require.NoError(t, err)
	require.Equal(t, legacySession.ID, got.ID)

	sessions, err := repo.GetByTenantID(ctx, 1, "bob")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{bobSession.ID, legacySession.ID}, sessionIDsForTest(sessions))

	paged, total, err := repo.GetPagedByTenantID(ctx, 1, "bob", &types.Pagination{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.ElementsMatch(t, []string{bobSession.ID, legacySession.ID}, sessionIDsForTest(paged))
}

func TestSessionRepositoryUpdateHonorsUserScope(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	ctx := context.Background()
	aliceSession := createSessionForTest(t, db, 1, "alice")

	rows, err := repo.Update(ctx, &types.Session{
		ID:       aliceSession.ID,
		TenantID: aliceSession.TenantID,
		Title:    "bob update attempt",
	}, "bob")
	require.NoError(t, err)
	require.Zero(t, rows)

	var unchanged types.Session
	require.NoError(t, db.First(&unchanged, "id = ?", aliceSession.ID).Error)
	require.Equal(t, aliceSession.Title, unchanged.Title)

	rows, err = repo.Update(ctx, &types.Session{
		ID:       aliceSession.ID,
		TenantID: aliceSession.TenantID,
		Title:    "alice updated session",
	}, "alice")
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)

	var changed types.Session
	require.NoError(t, db.First(&changed, "id = ?", aliceSession.ID).Error)
	require.Equal(t, "alice updated session", changed.Title)
}

func TestSessionRepositoryDeleteHonorsUserScope(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	ctx := context.Background()
	aliceSession := createSessionForTest(t, db, 1, "alice")
	bobSession := createSessionForTest(t, db, 1, "bob")

	rows, err := repo.Delete(ctx, 1, "bob", aliceSession.ID)
	require.NoError(t, err)
	require.Zero(t, rows)
	require.EqualValues(t, 1, countActiveSessionsForTest(t, db, aliceSession.ID))

	rows, err = repo.Delete(ctx, 1, "bob", bobSession.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	require.Zero(t, countActiveSessionsForTest(t, db, bobSession.ID))
}

func TestSessionRepositoryBatchDeleteHonorsUserScope(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	ctx := context.Background()
	aliceSession := createSessionForTest(t, db, 1, "alice")
	bobSession := createSessionForTest(t, db, 1, "bob")
	legacySession := createSessionForTest(t, db, 1, "")

	rows, err := repo.BatchDelete(ctx, 1, "bob", []string{aliceSession.ID, bobSession.ID, legacySession.ID})
	require.NoError(t, err)
	require.EqualValues(t, 2, rows)
	require.EqualValues(t, 1, countActiveSessionsForTest(t, db, aliceSession.ID))
	require.Zero(t, countActiveSessionsForTest(t, db, bobSession.ID))
	require.Zero(t, countActiveSessionsForTest(t, db, legacySession.ID))
}

func TestSessionRepositoryDeleteAllHonorsUserScope(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	ctx := context.Background()
	aliceSession := createSessionForTest(t, db, 1, "alice")
	bobSession := createSessionForTest(t, db, 1, "bob")
	legacySession := createSessionForTest(t, db, 1, "")
	otherTenantSession := createSessionForTest(t, db, 2, "bob")

	rows, err := repo.DeleteAllByTenantID(ctx, 1, "bob")
	require.NoError(t, err)
	require.EqualValues(t, 2, rows)
	require.EqualValues(t, 1, countActiveSessionsForTest(t, db, aliceSession.ID))
	require.Zero(t, countActiveSessionsForTest(t, db, bobSession.ID))
	require.Zero(t, countActiveSessionsForTest(t, db, legacySession.ID))
	require.EqualValues(t, 1, countActiveSessionsForTest(t, db, otherTenantSession.ID))
}

// im_channel_sessions row for QueryPaged source-filter tests. Mirrors the columns
// the LEFT JOIN projects; kept local to avoid importing internal/im.
type testIMChannelSession struct {
	ID          string `gorm:"primaryKey"`
	SessionID   string
	Platform    string
	ChatID      string
	ThreadID    string
	UserID      string
	AgentID     string
	IMChannelID string `gorm:"column:im_channel_id"`
	TenantID    uint64
	DeletedAt   gorm.DeletedAt
}

func (testIMChannelSession) TableName() string { return "im_channel_sessions" }

func listItemIDsForTest(items []*types.SessionListItem) []string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

// A /clear (or session recycling) soft-deletes the IM mapping and starts a fresh
// session. The old session must stay under its IM platform, not leak into "web"
// (which would happen if the source-filter join excluded soft-deleted mappings).
func TestSessionRepositoryQueryPagedKeepsClearedIMSessionsOutOfWeb(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	require.NoError(t, db.AutoMigrate(&testIMChannelSession{}))
	ctx := context.Background()

	web := createSessionForTest(t, db, 1, "alice")     // never bound to IM -> web
	active := createSessionForTest(t, db, 1, "alice")  // active IM mapping
	cleared := createSessionForTest(t, db, 1, "alice") // IM mapping soft-deleted by /clear

	require.NoError(t, db.Create(&testIMChannelSession{
		ID: "m-active", SessionID: active.ID, Platform: "wecom", TenantID: 1,
	}).Error)
	clearedMapping := &testIMChannelSession{
		ID: "m-cleared", SessionID: cleared.ID, Platform: "wecom", TenantID: 1,
	}
	require.NoError(t, db.Create(clearedMapping).Error)
	require.NoError(t, db.Delete(clearedMapping).Error) // soft-delete, as /clear does

	webItems, _, err := repo.QueryPaged(ctx, &types.SessionListQuery{
		TenantID: 1, UserID: "alice", Source: "web", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	require.Equal(t, []string{web.ID}, listItemIDsForTest(webItems),
		"web must exclude sessions that ever had an IM mapping, including cleared ones")

	wecomItems, _, err := repo.QueryPaged(ctx, &types.SessionListQuery{
		TenantID: 1, UserID: "alice", Source: "wecom", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{active.ID, cleared.ID}, listItemIDsForTest(wecomItems),
		"wecom must include both the active and the cleared IM session")
}

func TestSessionRepositoryQueryPagedSplitsWebAndEmbedSessions(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	require.NoError(t, db.AutoMigrate(&testIMChannelSession{}))
	ctx := context.Background()

	web := createSessionForTest(t, db, 1, "alice")
	embed := createSessionForTest(t, db, 1, "alice")
	require.NoError(t, db.Model(&types.Session{}).Where("id = ?", embed.ID).
		Update("description", types.EmbedSessionMarkerPrefix+"ch-1").Error)

	webItems, _, err := repo.QueryPaged(ctx, &types.SessionListQuery{
		TenantID: 1, UserID: "alice", Source: "web", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	require.Equal(t, []string{web.ID}, listItemIDsForTest(webItems))

	embedItems, _, err := repo.QueryPaged(ctx, &types.SessionListQuery{
		TenantID: 1, UserID: "alice", Source: "embed:ch-1", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	require.Equal(t, []string{embed.ID}, listItemIDsForTest(embedItems))
}

// The "web" source is user chats only; API-key sessions live in the
// admin-only "api" bucket and must never leak into a tenant-wide web listing.
// Legacy tenant-level rows (user_id "") must still show up in web.
func TestSessionRepositoryQueryPagedWebExcludesAPIKeySessions(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	require.NoError(t, db.AutoMigrate(&testIMChannelSession{}))
	ctx := context.Background()

	legacy := createSessionForTest(t, db, 1, "") // legacy tenant web row
	_ = createSessionForTest(t, db, 1, types.SessionOwnerAPITenantKeyPrefix+"1:10")
	_ = createSessionForTest(t, db, 1, types.SessionOwnerAPIExternalUserPrefix+"1:alice")

	items, _, err := repo.QueryPaged(ctx, &types.SessionListQuery{
		TenantID: 1, UserID: "", Source: "web", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	require.Equal(t, []string{legacy.ID}, listItemIDsForTest(items),
		"web must keep legacy tenant rows but exclude API-key sessions")
}

func TestSessionRepositoryGetIMPlatform(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	require.NoError(t, db.AutoMigrate(&testIMChannelSession{}))
	ctx := context.Background()

	imSession := createSessionForTest(t, db, 1, "alice")
	require.NoError(t, db.Create(&testIMChannelSession{
		ID: "ics-1", SessionID: imSession.ID, Platform: "feishu",
	}).Error)
	webSession := createSessionForTest(t, db, 1, "alice")

	platform, err := repo.GetIMPlatform(ctx, 1, imSession.ID)
	require.NoError(t, err)
	require.Equal(t, "feishu", platform)

	platform, err = repo.GetIMPlatform(ctx, 1, webSession.ID)
	require.NoError(t, err)
	require.Equal(t, "", platform)

	// Cross-tenant lookup must not leak the mapping.
	platform, err = repo.GetIMPlatform(ctx, 2, imSession.ID)
	require.NoError(t, err)
	require.Equal(t, "", platform)
}

func TestSessionRepositoryQueryPagedAPISourceReturnsAllTenantAPIKeySessions(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	require.NoError(t, db.AutoMigrate(&testIMChannelSession{}))
	ctx := context.Background()

	// API requests without and with external-user identity, plus a web user and
	// cross-tenant API sessions.
	key1 := createSessionForTest(t, db, 1, types.SessionOwnerAPITenantKeyPrefix+"1:10")
	key2 := createSessionForTest(t, db, 1, types.SessionOwnerAPITenantKeyPrefix+"1:20")
	directHeader := createSessionForTest(t, db, 1, types.SessionOwnerAPIExternalUserPrefix+"1:alice")
	signedToken := createSessionForTest(t, db, 1, types.SessionOwnerAPIExternalUserPrefix+"1:bob")
	_ = createSessionForTest(t, db, 1, "alice")
	_ = createSessionForTest(t, db, 2, types.SessionOwnerAPITenantKeyPrefix+"2:30")
	_ = createSessionForTest(t, db, 2, types.SessionOwnerAPIExternalUserPrefix+"2:mallory")

	// The admin view clears UserID, so every API-key session in the tenant is
	// returned regardless of which key created it.
	items, total, err := repo.QueryPaged(ctx, &types.SessionListQuery{
		TenantID: 1, UserID: "", Source: types.SessionSourceAPI, Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	require.EqualValues(t, 4, total)
	require.ElementsMatch(
		t,
		[]string{key1.ID, key2.ID, directHeader.ID, signedToken.ID},
		listItemIDsForTest(items),
	)
}

// Maintenance sessions (skill install / remove) are real sessions with real
// transcripts, but they are infrastructure, not conversations. They must be
// invisible in every bucket — including the unfiltered listing, which is why
// the exclusion lives in applyBase and not in the "web" source branch.
func TestSessionRepositoryQueryPagedHidesMaintenanceSessions(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	require.NoError(t, db.AutoMigrate(&testIMChannelSession{}))
	ctx := context.Background()

	web := createSessionForTest(t, db, 1, "alice")
	maintenance := createSessionForTest(t, db, 1, "alice")
	require.NoError(t, db.Model(&types.Session{}).Where("id = ?", maintenance.ID).
		Update("description", types.SkillMaintenanceSessionMarker+"install").Error)

	for _, source := range []string{"", "web"} {
		items, total, err := repo.QueryPaged(ctx, &types.SessionListQuery{
			TenantID: 1, UserID: "alice", Source: source, Page: 1, PageSize: 50,
		})
		require.NoError(t, err)
		require.Equal(t, []string{web.ID}, listItemIDsForTest(items),
			"source=%q must not list the maintenance session", source)
		require.EqualValues(t, 1, total,
			"source=%q count must not include the maintenance session", source)
	}
}

// TestSessionRepositoryAdoptLastRequestState pins the compare-and-set of the
// legacy-adoption write: the "still unbound" decision lives inside the
// UPDATE, so a second adoption never overwrites the winner's binding, and
// the unbound test mirrors what the Go side can read (NULL, empty, junk and
// endpoint_id-less UI state are all adoptable; anything with a non-empty
// endpoint_id is already bound). Scope rules must match the other writers.
func TestSessionRepositoryAdoptLastRequestState(t *testing.T) {
	repo, db := newSessionRepositoryForTest(t)
	ctx := context.Background()
	binding := func(kb string) *types.SessionLastRequestState {
		return &types.SessionLastRequestState{
			EndpointID: "ep-1", AskAgentID: "agent-1", KnowledgeBaseIDs: []string{kb},
		}
	}

	// NULL agent_config: first adoption wins, the second is a no-op.
	nilRow := createSessionForTest(t, db, 1, "alice")
	affected, err := repo.AdoptLastRequestState(ctx, 1, "alice", nilRow.ID, binding("kb-1"))
	require.NoError(t, err)
	require.EqualValues(t, 1, affected)
	affected, err = repo.AdoptLastRequestState(ctx, 1, "alice", nilRow.ID, binding("kb-2"))
	require.NoError(t, err)
	require.EqualValues(t, 0, affected, "a bound row must not be re-bound by adoption")
	got, err := repo.Get(ctx, 1, "alice", nilRow.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"kb-1"}, got.LastRequestState.KnowledgeBaseIDs,
		"the winner's binding must survive the loser's write attempt")

	// A UI-memo row (agent_config present, no endpoint_id) predates any
	// binding and must be adoptable.
	uiRow := createSessionForTest(t, db, 1, "alice")
	require.NoError(t, db.Model(&types.Session{}).Where("id = ?", uiRow.ID).
		Update("agent_config", &types.SessionLastRequestState{AgentID: "a", ModelID: "m"}).Error)
	affected, err = repo.AdoptLastRequestState(ctx, 1, "alice", uiRow.ID, binding("kb-1"))
	require.NoError(t, err)
	require.EqualValues(t, 1, affected, "endpoint_id-less UI state is unbound")

	// Junk the Go Scan silently ignores must read as unbound here too — a
	// fatal condition would block adoption of such legacy rows.
	junk := createSessionForTest(t, db, 1, "alice")
	require.NoError(t, db.Exec("UPDATE sessions SET agent_config = ? WHERE id = ?", "not-json", junk.ID).Error)
	affected, err = repo.AdoptLastRequestState(ctx, 1, "alice", junk.ID, binding("kb-1"))
	require.NoError(t, err)
	require.EqualValues(t, 1, affected, "unreadable agent_config must be adoptable, not fatal")

	// An explicitly empty endpoint_id counts as unbound (the Go-side rule
	// is bound iff EndpointID != "").
	emptyEP := createSessionForTest(t, db, 1, "alice")
	require.NoError(t, db.Model(&types.Session{}).Where("id = ?", emptyEP.ID).
		Update("agent_config", &types.SessionLastRequestState{EndpointID: "", AskAgentID: "x"}).Error)
	affected, err = repo.AdoptLastRequestState(ctx, 1, "alice", emptyEP.ID, binding("kb-1"))
	require.NoError(t, err)
	require.EqualValues(t, 1, affected)

	// Scope rules: wrong tenant or wrong owner is a no-op, never a write.
	other := createSessionForTest(t, db, 2, "bob")
	affected, err = repo.AdoptLastRequestState(ctx, 1, "alice", other.ID, binding("kb-1"))
	require.NoError(t, err)
	require.EqualValues(t, 0, affected, "another tenant's row must not be adoptable")
	affected, err = repo.AdoptLastRequestState(ctx, 1, "mallory", nilRow.ID, binding("kb-1"))
	require.NoError(t, err)
	require.EqualValues(t, 0, affected, "another user must not adopt the row")
	got, err = repo.Get(ctx, 1, "alice", nilRow.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"kb-1"}, got.LastRequestState.KnowledgeBaseIDs)

	// A missing session is a zero-row no-op, matching UpdateLastRequestState.
	affected, err = repo.AdoptLastRequestState(ctx, 1, "alice", "gone", binding("kb-1"))
	require.NoError(t, err)
	require.EqualValues(t, 0, affected)

	// A state that could not satisfy the SQL condition is refused up front.
	_, err = repo.AdoptLastRequestState(ctx, 1, "alice", nilRow.ID, &types.SessionLastRequestState{})
	require.Error(t, err, "adoption requires a state that names its endpoint")
}
