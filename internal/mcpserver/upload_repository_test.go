package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These tests exercise the real GORM repository against an in-memory SQLite
// database, so the conditional-UPDATE lease logic is verified as SQL rather than
// only through the in-memory fake. They need CGO (mattn/go-sqlite3), which is why
// weknora-check runs them in the Linux container.

func newReceiptTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	// A distinct shared-cache name per test keeps parallel tests isolated.
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared&_foreign_keys=on", name)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := AutoMigrateUploadReceipts(db); err != nil {
		t.Fatalf("migrate mcp_uploads: %v", err)
	}
	// AutoMigrate cannot express a PARTIAL unique index (WHERE idempotency_key
	// <> ''), so the table it builds lacks the real deployment's idempotency
	// guarantee — and a test that only used AutoMigrate would pass while the
	// shipped constraint was never exercised. The indexes therefore come from
	// the SHIPPED migration, which also proves that DDL parses and creates what
	// the repository relies on.
	applyMigrationIndexes(t, db)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// applyMigrationIndexes runs every CREATE INDEX statement from the SQLite
// mcp_uploads migration against the test database.
func applyMigrationIndexes(t *testing.T, db *gorm.DB) {
	t.Helper()
	path := filepath.Join(repoRootFromTest(t), "migrations", "sqlite", "000035_mcp_uploads.up.sql")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration %s: %v", path, err)
	}
	// Only the index statements matter: the table already exists via
	// AutoMigrate, and CREATE TABLE IF NOT EXISTS would be a no-op anyway.
	statements := regexp.MustCompile(`(?is)CREATE\s+(?:UNIQUE\s+)?INDEX[^;]*;`).
		FindAllString(string(body), -1)
	if len(statements) == 0 {
		t.Fatalf("%s contains no CREATE INDEX statement", path)
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("apply index from the migration: %v\n%s", err, statement)
		}
	}
	// A silently-absent unique index would make the idempotency test pass for
	// the wrong reason, so its existence is asserted explicitly.
	var count int
	if err := db.Raw(
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`,
		"idx_mcp_uploads_idempotency",
	).Scan(&count).Error; err != nil {
		t.Fatalf("inspect sqlite_master: %v", err)
	}
	if count != 1 {
		t.Fatalf("the migration's idempotency index was not created (found %d)", count)
	}
}

func seedReceipt(t *testing.T, repo UploadReceiptRepository, state string) UploadRecord {
	t.Helper()
	now := time.Now().UTC()
	record := UploadRecord{
		UploadID:          newUploadID(),
		EndpointID:        "ep-1",
		CallerWorkspaceID: 7,
		CallerStorageID:   "web_user:user-1",
		OwnerTenantID:     99,
		KnowledgeBaseID:   kbA,
		FileName:          "notes.txt",
		FileSize:          12,
		SHA256:            "abc",
		State:             state,
		CreatedAt:         now,
		ExpiresAt:         now.Add(UploadPayloadTTL),
		ReceiptExpiresAt:  now.Add(UploadReceiptTTL),
	}
	if err := repo.Create(context.Background(), &record); err != nil {
		t.Fatalf("create receipt: %v", err)
	}
	return record
}

// The lookup must be scoped to the full caller identity: this is the SQL
// equivalent of the fake's identity check and the reason upload ids are not
// capabilities.
func TestReceiptLookupIsScopedToTheCaller(t *testing.T) {
	repo := NewUploadReceiptRepository(newReceiptTestDB(t))
	record := seedReceipt(t, repo, UploadStateStaged)

	got, err := repo.Get(context.Background(), uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	})
	if err != nil || got == nil {
		t.Fatalf("the owning caller must resolve the receipt: %v", err)
	}

	for name, lookup := range map[string]uploadLookup{
		"another principal": {UploadID: record.UploadID, EndpointID: "ep-1", CallerWorkspaceID: 7, CallerStorageID: "web_user:user-2"},
		"another workspace": {UploadID: record.UploadID, EndpointID: "ep-1", CallerWorkspaceID: 99, CallerStorageID: "web_user:user-1"},
		"another endpoint":  {UploadID: record.UploadID, EndpointID: "ep-2", CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1"},
		"the owner tenant":  {UploadID: record.UploadID, EndpointID: "ep-1", CallerWorkspaceID: 99, CallerStorageID: "web_user:owner"},
	} {
		missing, lookupErr := repo.Get(context.Background(), lookup)
		if lookupErr != nil {
			t.Fatalf("%s: unexpected error %v", name, lookupErr)
		}
		if missing != nil {
			t.Fatalf("%s must NOT resolve another caller's upload", name)
		}
	}
}

// Concurrent claims: exactly one wins while the lease is live, and a second try
// reports "not claimed" rather than overwriting the first.
func TestReceiptClaimIsExclusive(t *testing.T) {
	repo := NewUploadReceiptRepository(newReceiptTestDB(t))
	record := seedReceipt(t, repo, UploadStateStaged)
	ctx := context.Background()
	expires := time.Now().UTC().Add(uploadLeaseTTL)

	first, err := repo.ClaimForImport(ctx, record.UploadID, "lease-1", expires)
	if err != nil || !first {
		t.Fatalf("the first claim must win: claimed=%v err=%v", first, err)
	}
	second, err := repo.ClaimForImport(ctx, record.UploadID, "lease-2", expires)
	if err != nil {
		t.Fatalf("second claim errored: %v", err)
	}
	if second {
		t.Fatal("a second claim must not win while the lease is live")
	}
	// The live lease must still be the first one.
	after, err := repo.Get(ctx, uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after == nil || after.State != UploadStateImporting {
		t.Fatalf("state after a lost claim race = %+v", after)
	}
}

// A crashed importer's lease is recoverable: the claim takes over, and the stale
// lease token can no longer write the receipt.
func TestReceiptExpiredLeaseIsRecoverableAndStaleWriteRefused(t *testing.T) {
	repo := NewUploadReceiptRepository(newReceiptTestDB(t))
	record := seedReceipt(t, repo, UploadStateStaged)
	ctx := context.Background()

	past := time.Now().UTC().Add(-2 * time.Minute)
	if claimed, err := repo.ClaimForImport(ctx, record.UploadID, "stale-lease", past); err != nil || !claimed {
		t.Fatalf("initial claim: %v %v", claimed, err)
	}
	// The recovering attempt takes the lease over.
	if claimed, err := repo.ClaimForImport(ctx, record.UploadID, "fresh-lease",
		time.Now().UTC().Add(uploadLeaseTTL)); err != nil || !claimed {
		t.Fatalf("an expired lease must be recoverable: claimed=%v err=%v", claimed, err)
	}
	// The crashed attempt must NOT be able to complete the receipt now.
	err := repo.MarkImported(ctx, record.UploadID, "stale-lease", "doc-should-not-stick", false, true)
	if err == nil {
		t.Fatal("a stale lease must not be able to write the receipt")
	}
	after, err := repo.Get(ctx, uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.DocumentID != "" {
		t.Fatalf("the stale write leaked a document id: %q", after.DocumentID)
	}
	// The fresh lease can complete it.
	if err := repo.MarkImported(ctx, record.UploadID, "fresh-lease", "doc-1", true, true); err != nil {
		t.Fatalf("the live lease must be able to complete: %v", err)
	}
}

// Renew and fail are both lease-conditional.
func TestReceiptRenewAndFailRequireTheLease(t *testing.T) {
	repo := NewUploadReceiptRepository(newReceiptTestDB(t))
	record := seedReceipt(t, repo, UploadStateStaged)
	ctx := context.Background()
	if claimed, err := repo.ClaimForImport(ctx, record.UploadID, "lease-1",
		time.Now().UTC().Add(uploadLeaseTTL)); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if ok, err := repo.RenewLease(ctx, record.UploadID, "lease-1",
		time.Now().UTC().Add(uploadLeaseTTL)); err != nil || !ok {
		t.Fatalf("renew with the live token must succeed: %v %v", ok, err)
	}
	if ok, err := repo.RenewLease(ctx, record.UploadID, "other-lease",
		time.Now().UTC().Add(uploadLeaseTTL)); err != nil || ok {
		t.Fatalf("renew with a foreign token must fail: %v %v", ok, err)
	}
	if err := repo.MarkFailed(ctx, record.UploadID, "other-lease", CodeProcessingError, "x"); err == nil {
		t.Fatal("fail with a foreign token must be refused")
	}
	if err := repo.MarkFailed(ctx, record.UploadID, "lease-1", CodeProcessingError, "boom"); err != nil {
		t.Fatalf("fail with the live token: %v", err)
	}
	after, err := repo.Get(ctx, uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.State != UploadStateStaged || after.LastErrorCode != CodeProcessingError {
		t.Fatalf("after a failure the upload must be retryable and diagnosed: %+v", after)
	}
}

// The idempotency unique index must exist and be scoped to non-empty keys, so
// keyless uploads do not collide.
func TestReceiptIdempotencyIndexAllowsManyEmptyKeys(t *testing.T) {
	db := newReceiptTestDB(t)
	repo := NewUploadReceiptRepository(db)
	ctx := context.Background()

	if _, err := repo.FindByIdempotencyKey(ctx, "ep-1", 7, "web_user:user-1", "key-1"); err != nil {
		t.Fatalf("lookup on an empty table: %v", err)
	}
	first := seedReceipt(t, repo, UploadStateStaged)
	// Two rows with the SAME key must be impossible.
	duplicate := UploadRecord{
		UploadID: newUploadID(), EndpointID: first.EndpointID,
		CallerWorkspaceID: first.CallerWorkspaceID, CallerStorageID: first.CallerStorageID,
		OwnerTenantID: first.OwnerTenantID, KnowledgeBaseID: first.KnowledgeBaseID,
		IdempotencyKey: "same-key", FileName: "a.txt", FileSize: 1, State: UploadStateStaged,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
		ReceiptExpiresAt: time.Now().UTC().Add(2 * time.Hour),
	}
	if err := repo.Create(ctx, &duplicate); err != nil {
		t.Fatalf("first keyed insert: %v", err)
	}
	second := duplicate
	second.UploadID = newUploadID()
	if err := repo.Create(ctx, &second); err == nil {
		t.Fatal("a duplicate idempotency key must violate the unique index")
	}
	// But many keyless rows must be allowed (the partial index excludes '').
	for i := 0; i < 3; i++ {
		keyless := duplicate
		keyless.UploadID = newUploadID()
		keyless.IdempotencyKey = ""
		if err := repo.Create(ctx, &keyless); err != nil {
			t.Fatalf("keyless insert %d must be allowed: %v", i, err)
		}
	}
}

// Sweep must expire unused payloads and leave live leases alone.
func TestReceiptSweepExpiresPayloadsRespectingLeases(t *testing.T) {
	db := newReceiptTestDB(t)
	repo := NewUploadReceiptRepository(db)
	ctx := context.Background()
	stale := seedReceipt(t, repo, UploadStateStaged)
	leased := seedReceipt(t, repo, UploadStateStaged)
	if claimed, err := repo.ClaimForImport(ctx, leased.UploadID, "lease",
		time.Now().UTC().Add(uploadLeaseTTL)); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	// Push both past their byte deadline. The repository interface deliberately
	// exposes no "age this row" method, so the test reaches the database handle
	// it created itself rather than widening the production API for a test.
	past := time.Now().UTC().Add(-time.Hour)
	for _, id := range []string{stale.UploadID, leased.UploadID} {
		if err := db.Model(&UploadReceipt{}).Where("id = ?", id).
			Update("expires_at", past).Error; err != nil {
			t.Fatalf("age the row: %v", err)
		}
	}
	payloads, _, err := repo.Sweep(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if payloads != 1 {
		t.Fatalf("exactly one unused payload should expire, got %d", payloads)
	}
	after, err := repo.Get(ctx, uploadLookup{
		UploadID: leased.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.State != UploadStateImporting {
		t.Fatalf("a leased import must not be expired, state = %q", after.State)
	}
}

// TestClaimRefusesPayloadsPastDeadline: the payload TTL guard added to
// ClaimForImport. A staged row whose bytes are already past expires_at must
// not be claimable: the next sweep is about to delete the file, and reading
// it would race with that delete.
func TestClaimRefusesPayloadsPastDeadline(t *testing.T) {
	db := newReceiptTestDB(t)
	repo := NewUploadReceiptRepository(db)
	ctx := context.Background()
	row := seedReceipt(t, repo, UploadStateStaged)
	if err := db.Model(&UploadReceipt{}).Where("id = ?", row.UploadID).
		Update("expires_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
		t.Fatalf("age the row: %v", err)
	}
	claimed, err := repo.ClaimForImport(ctx, row.UploadID, "lease",
		time.Now().UTC().Add(uploadLeaseTTL))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Fatalf("a payload past its deadline must not be claimable")
	}
	after, err := repo.Get(ctx, uploadLookup{
		UploadID: row.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.State != UploadStateStaged {
		t.Fatalf("expired-payload claim must leave state staged, got %q", after.State)
	}
}

// TestReceiptSweepKeepsLiveLeaseEvenAfterReceiptDeadline: a row whose
// receipt_expires_at is in the past but whose lease is still live must
// survive the row-driven receipt sweep, because the in-flight import
// depends on the bookkeeping.
func TestReceiptSweepKeepsLiveLeaseEvenAfterReceiptDeadline(t *testing.T) {
	db := newReceiptTestDB(t)
	repo := NewUploadReceiptRepository(db)
	ctx := context.Background()
	row := seedReceipt(t, repo, UploadStateStaged)
	claimed, err := repo.ClaimForImport(ctx, row.UploadID, "lease",
		time.Now().UTC().Add(time.Hour))
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := db.Model(&UploadReceipt{}).Where("id = ?", row.UploadID).
		Update("receipt_expires_at", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("age receipt: %v", err)
	}
	_, receipts, err := repo.Sweep(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("live lease must shield the receipt from deletion, got %d", receipts)
	}
	after, err := repo.Get(ctx, uploadLookup{
		UploadID: row.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after == nil {
		t.Fatal("live lease must keep the row")
	}
	if after.State != UploadStateImporting {
		t.Fatalf("state must remain importing under a live lease, got %q", after.State)
	}
}

// TestSweepExpiresCrashedImporting: an importing row whose lease AND payload
// are both past their deadlines is a crashed import. The sweep must mark it
// expired so subsequent status calls do not report "still being worked on"
// for a row whose bytes are already gone.
func TestSweepExpiresCrashedImporting(t *testing.T) {
	db := newReceiptTestDB(t)
	repo := NewUploadReceiptRepository(db)
	ctx := context.Background()
	row := seedReceipt(t, repo, UploadStateStaged)
	claimed, err := repo.ClaimForImport(ctx, row.UploadID, "lease",
		time.Now().UTC().Add(time.Hour))
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := db.Model(&UploadReceipt{}).Where("id = ?", row.UploadID).
		Updates(map[string]any{
			"expires_at":       time.Now().UTC().Add(-2 * time.Hour),
			"lease_expires_at": time.Now().UTC().Add(-time.Hour),
		}).Error; err != nil {
		t.Fatalf("age row: %v", err)
	}
	payloads, _, err := repo.Sweep(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if payloads < 1 {
		t.Fatalf("crashed importing row must be marked expired, got %d", payloads)
	}
	after, err := repo.Get(ctx, uploadLookup{
		UploadID: row.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after == nil {
		t.Fatal("the row must still be visible after a crashed-importing sweep pass")
	}
	if after.State != UploadStateExpired {
		t.Fatalf("crashed importing must be marked expired, got %q", after.State)
	}
	if after.LeaseToken != "" {
		t.Fatalf("crashed row must have its lease cleared, got %q", after.LeaseToken)
	}
}

// TestCompleteSurvivesCallerContextCancel: the receipt write must use a
// detached context with a bounded deadline, not the caller's request
// context, so a request cancel / handler timeout does not leave a
// successfully created document invisible to a follow-up status call.
// This test drives the storage layer directly: a ctx that is already
// cancelled must still let Complete persist the receipt.
func TestCompleteSurvivesCallerContextCancel(t *testing.T) {
	root := t.TempDir()
	repo := newFakeReceiptRepo()
	staging := NewStagedUploadStorage(root, repo)
	record, _, err := staging.Stage(context.Background(), baseUploadRequest(kbA),
		bytes.NewReader([]byte("hello, the receipt write must use a detached context")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	lease, err := staging.Claim(context.Background(), uploadLookup{
		UploadID:          record.UploadID,
		EndpointID:        "ep-1",
		CallerWorkspaceID: 7,
		CallerStorageID:   "web_user:user-1",
	}, uploadLeaseTTL)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v %v", lease, err)
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // simulate the request context being cancelled before Complete runs.
	if err := staging.Complete(cancelledCtx, lease, UploadCompletion{
		DocumentID:   "doc-cancelled",
		Deduplicated: false,
		Queued:       true,
	}); err != nil {
		t.Fatalf("Complete must survive caller ctx cancel, got %v", err)
	}
	row, err := repo.Get(context.Background(), uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	})
	if err != nil {
		t.Fatalf("get after complete: %v", err)
	}
	if row == nil {
		t.Fatal("receipt must persist after Complete despite caller cancel")
	}
	if row.State != UploadStateImported {
		t.Fatalf("receipt must be imported, got %q", row.State)
	}
	if row.DocumentID != "doc-cancelled" {
		t.Fatalf("document id must be recorded, got %q", row.DocumentID)
	}
}

// The classification must hold against the REAL migration index: the SQLite
// driver reports the indexed column list, not the index name, so a
// name-only matcher would silently miss every conflict on SQLite and Stage
// would surface a raw database error instead of the replay/conflict policy.
func TestIdempotencyIndexConflictIsClassifiedAgainstRealSQLite(t *testing.T) {
	repo := NewUploadReceiptRepository(newReceiptTestDB(t))
	ctx := context.Background()
	now := time.Now().UTC()
	build := func(id, key string) *UploadRecord {
		return &UploadRecord{
			UploadID: id, EndpointID: "ep-1", CallerWorkspaceID: 7,
			CallerStorageID: "web_user:user-1", OwnerTenantID: 99,
			KnowledgeBaseID: kbA, IdempotencyKey: key, FileName: "notes.txt",
			FileSize: 1, SHA256: "abc", State: UploadStateStaged,
			CreatedAt: now, ExpiresAt: now.Add(UploadPayloadTTL),
			ReceiptExpiresAt: now.Add(UploadReceiptTTL),
		}
	}

	if err := repo.Create(ctx, build("id-1", "k-1")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Same caller scope + same key: the idempotency index violation.
	err := repo.Create(ctx, build("id-2", "k-1"))
	if err == nil {
		t.Fatal("the unique index must reject the duplicate key")
	}
	if !IsIdempotencyKeyConflict(err) {
		t.Fatalf("the idempotency index violation must classify, got: %v", err)
	}

	// Same key from a DIFFERENT caller is not a conflict (the index includes
	// the caller scope).
	other := build("id-3", "k-1")
	other.CallerStorageID = "web_user:user-2"
	if err := repo.Create(ctx, other); err != nil {
		t.Fatalf("another caller may reuse the key: %v", err)
	}

	// Primary-key collision: uniqueness on mcp_uploads.id is NOT this index.
	if err := repo.Create(ctx, build("id-1", "k-2")); err == nil {
		t.Fatal("the primary key must reject the duplicate id")
	} else if IsIdempotencyKeyConflict(err) {
		t.Fatalf("a primary-key collision must not classify as the idempotency index: %v", err)
	}

	// Empty keys never collide (partial index) and generic errors never match.
	if err := repo.Create(ctx, build("id-4", "")); err != nil {
		t.Fatalf("empty-key create one: %v", err)
	}
	if err := repo.Create(ctx, build("id-5", "")); err != nil {
		t.Fatalf("empty-key create two: %v", err)
	}
	if IsIdempotencyKeyConflict(errors.New("database is locked")) {
		t.Fatal("a lock error must never classify as the idempotency conflict")
	}
	if IsIdempotencyKeyConflict(nil) {
		t.Fatal("nil must not classify")
	}
}
