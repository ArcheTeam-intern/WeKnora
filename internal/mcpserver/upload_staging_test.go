package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReceiptRepo is an in-memory UploadReceiptRepository. The real one is a
// conditional-UPDATE SQL implementation verified in the Linux container; this
// fake lets the staging LOGIC (identity scoping, leases, expiry, idempotency) be
// tested without CGO, which is what makes these tests runnable on Windows too.
type fakeReceiptRepo struct {
	mu      sync.Mutex
	rows    map[string]*UploadRecord
	created int
}

func newFakeReceiptRepo() *fakeReceiptRepo {
	return &fakeReceiptRepo{rows: map[string]*UploadRecord{}}
}

func (f *fakeReceiptRepo) Create(_ context.Context, record *UploadRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	copyRecord := *record
	// Mirror the real SQL receipt row: PayloadPath is derived from identity and
	// is never persisted (UploadReceipt has no such column), so a record read
	// back has no path until the staging layer rebuilds it. Keeping it here
	// would let tests pass against a fake that is more permissive than the DB.
	copyRecord.PayloadPath = ""
	f.rows[record.UploadID] = &copyRecord
	f.created++
	return nil
}

func (f *fakeReceiptRepo) Get(_ context.Context, lookup uploadLookup) (*UploadRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[lookup.UploadID]
	if !ok {
		return nil, nil
	}
	// The full identity must match, exactly like the SQL WHERE clause.
	if row.EndpointID != lookup.EndpointID ||
		row.CallerWorkspaceID != lookup.CallerWorkspaceID ||
		row.CallerStorageID != lookup.CallerStorageID {
		return nil, nil
	}
	copyRecord := *row
	return &copyRecord, nil
}

func (f *fakeReceiptRepo) FindByIdempotencyKey(
	_ context.Context, endpointID string, callerWorkspaceID uint64, callerStorageID, key string,
) (*UploadRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.EndpointID == endpointID && row.CallerWorkspaceID == callerWorkspaceID &&
			row.CallerStorageID == callerStorageID && row.IdempotencyKey == key && key != "" {
			copyRecord := *row
			return &copyRecord, nil
		}
	}
	return nil, nil
}

func (f *fakeReceiptRepo) ClaimForImport(
	_ context.Context, uploadID, leaseToken string, expiresAt time.Time,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[uploadID]
	if !ok {
		return false, nil
	}
	now := time.Now().UTC()
	claimable := row.State == UploadStateStaged ||
		(row.State == UploadStateImporting && (row.LeaseExpiresAt == nil || row.LeaseExpiresAt.Before(now)))
	if !claimable {
		return false, nil
	}
	row.State = UploadStateImporting
	row.LeaseToken = leaseToken
	row.LeaseExpiresAt = &expiresAt
	return true, nil
}

func (f *fakeReceiptRepo) RenewLease(
	_ context.Context, uploadID, leaseToken string, expiresAt time.Time,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[uploadID]
	if !ok || row.LeaseToken != leaseToken {
		return false, nil
	}
	row.LeaseExpiresAt = &expiresAt
	return true, nil
}

func (f *fakeReceiptRepo) MarkImported(
	_ context.Context, uploadID, leaseToken, documentID string, deduplicated, queued bool,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[uploadID]
	if !ok || row.LeaseToken != leaseToken {
		return &ToolError{Code: CodeUploadLeaseConflict, Message: "lease lost"}
	}
	row.State = UploadStateImported
	row.DocumentID = documentID
	row.Deduplicated = deduplicated
	row.Queued = queued
	row.LeaseToken = ""
	row.LeaseExpiresAt = nil
	return nil
}

func (f *fakeReceiptRepo) MarkFailed(
	_ context.Context, uploadID, leaseToken, code, message string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[uploadID]
	if !ok || row.LeaseToken != leaseToken {
		return &ToolError{Code: CodeUploadLeaseConflict, Message: "lease lost"}
	}
	row.State = UploadStateStaged
	row.LastErrorCode = code
	row.LastErrorMessage = message
	row.LeaseToken = ""
	row.LeaseExpiresAt = nil
	return nil
}

func (f *fakeReceiptRepo) Sweep(_ context.Context, now time.Time) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	payloads, receipts := 0, 0
	for id, row := range f.rows {
		if row.State == UploadStateImported {
			if row.ReceiptExpiresAt.Before(now) {
				delete(f.rows, id)
				receipts++
			}
			continue
		}
		if row.ExpiresAt.Before(now) && row.LeaseToken == "" {
			row.State = UploadStateExpired
			payloads++
		}
	}
	return payloads, receipts, nil
}

func (f *fakeReceiptRepo) ExpiredPayloadRecords(_ context.Context, now time.Time) ([]UploadRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []UploadRecord
	for _, row := range f.rows {
		if row.State == UploadStateImported {
			continue
		}
		if row.ExpiresAt.Before(now) && row.LeaseToken == "" {
			out = append(out, *row)
		}
	}
	return out, nil
}

func (f *fakeReceiptRepo) LivePayloadRecords(_ context.Context) ([]UploadRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []UploadRecord
	for _, row := range f.rows {
		switch row.State {
		case UploadStateStaged, UploadStateImporting, UploadStateFailed:
			out = append(out, *row)
		}
	}
	return out, nil
}

func newTestStaging(t *testing.T) (UploadStaging, *fakeReceiptRepo, string) {
	t.Helper()
	root := t.TempDir()
	repo := newFakeReceiptRepo()
	return NewStagedUploadStorage(root, repo), repo, root
}

func baseUploadRequest(kbID string) UploadRequest {
	return UploadRequest{
		EndpointID:        "ep-1",
		CallerWorkspaceID: 7,
		CallerStorageID:   "web_user:user-1",
		OwnerTenantID:     7,
		KnowledgeBaseID:   kbID,
		FileName:          "notes.txt",
		MaxBytes:          1 << 20,
	}
}

// Stage must lay the payload out under the caller identity, never under a
// client-supplied path.
func TestStageUsesVerifiedCallerIdentityForThePath(t *testing.T) {
	staging, _, root := newTestStaging(t)
	record, existing, err := staging.Stage(context.Background(),
		baseUploadRequest(kbA), bytes.NewReader([]byte("hello weknora")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if existing {
		t.Fatal("a fresh upload must not be reported as existing")
	}
	if record.FileSize != 13 || record.SHA256 == "" {
		t.Fatalf("size/hash not recorded: %+v", record)
	}
	// The identity segment is the sha256 of the principal storage id.
	wantIdentity := sha256HexForTest("web_user:user-1")
	wantDir := filepath.Join(root, "7", wantIdentity, record.UploadID)
	if record.PayloadPath != filepath.Join(wantDir, "payload") {
		t.Fatalf("payload path %q, want %q", record.PayloadPath, filepath.Join(wantDir, "payload"))
	}
	if _, statErr := os.Stat(record.PayloadPath); statErr != nil {
		t.Fatalf("payload not on disk: %v", statErr)
	}
}

// A receipt read back from the database carries no payload path; the staging
// layer must rebuild it from the persisted identity so import can still open
// the bytes (regression: the path was dropped on read, making every import fail
// with "payload no longer on disk").
func TestGetRebuildsDerivedPayloadPath(t *testing.T) {
	staging, repo, root := newTestStaging(t)
	record, existing, err := staging.Stage(context.Background(),
		baseUploadRequest(kbA), bytes.NewReader([]byte("hello weknora")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if existing {
		t.Fatal("a fresh upload must not be reported as existing")
	}
	// The fake repo models the SQL row: Create stripped PayloadPath. Confirm
	// that, then prove Get rebuilds it.
	stored, _ := repo.Get(context.Background(), uploadLookup{
		UploadID:          record.UploadID,
		EndpointID:        record.EndpointID,
		CallerWorkspaceID: record.CallerWorkspaceID,
		CallerStorageID:   record.CallerStorageID,
	})
	if stored == nil || stored.PayloadPath != "" {
		t.Fatalf("fake repo must not persist PayloadPath; got %+v", stored)
	}
	got, err := staging.Get(context.Background(), uploadLookup{
		UploadID:          record.UploadID,
		EndpointID:        record.EndpointID,
		CallerWorkspaceID: record.CallerWorkspaceID,
		CallerStorageID:   record.CallerStorageID,
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := filepath.Join(root, "7", sha256HexForTest("web_user:user-1"), record.UploadID, "payload")
	if got.PayloadPath != want {
		t.Fatalf("Get must rebuild the payload path; got %q, want %q", got.PayloadPath, want)
	}
	if _, statErr := os.Stat(got.PayloadPath); statErr != nil {
		t.Fatalf("rebuilt payload path is not on disk: %v", statErr)
	}
}

// A file name that tries to escape is reduced to its base name.
func TestStageStripsDirectoryComponents(t *testing.T) {
	staging, _, _ := newTestStaging(t)
	req := baseUploadRequest(kbA)
	req.FileName = "../../etc/passwd.txt"
	record, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if record.FileName != "passwd.txt" {
		t.Fatalf("file name = %q, want the base name", record.FileName)
	}
}

// Unsupported extension and oversize body are refused with their own codes.
func TestStageRejectsUnsupportedAndOversize(t *testing.T) {
	staging, _, _ := newTestStaging(t)
	req := baseUploadRequest(kbA)
	req.FileName = "payload.exe"
	_, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("MZ")))
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeInvalidFileType {
		t.Fatalf("expected INVALID_FILE_TYPE, got %v", err)
	}

	small := baseUploadRequest(kbA)
	small.MaxBytes = 4
	_, _, err = staging.Stage(context.Background(), small, bytes.NewReader([]byte("0123456789")))
	if !errors.As(err, &toolErr) || toolErr.Code != CodeFileTooLarge {
		t.Fatalf("expected FILE_TOO_LARGE, got %v", err)
	}
	// A rejected upload must leave nothing importable behind.
	if _, statErr := os.Stat(filepath.Join(staging.(*stagedUploadStorage).root, "7")); statErr == nil {
		entries, _ := os.ReadDir(filepath.Join(staging.(*stagedUploadStorage).root, "7"))
		for _, entry := range entries {
			identityDir := filepath.Join(staging.(*stagedUploadStorage).root, "7", entry.Name())
			sub, _ := os.ReadDir(identityDir)
			if len(sub) > 0 {
				t.Fatalf("a rejected upload left %d directory(ies) behind", len(sub))
			}
		}
	}
}

// duplicate_key_conflict: the same Idempotency-Key with different content or a
// different knowledge base is a conflict, not a silent second upload.
func TestDuplicateKeyConflict(t *testing.T) {
	staging, _, _ := newTestStaging(t)
	first := baseUploadRequest(kbA)
	first.IdempotencyKey = "5c2b9a34-0f4d-4a6b-9d21-7c0f2a1b3c4d"
	record, existing, err := staging.Stage(context.Background(), first, bytes.NewReader([]byte("abc")))
	if err != nil || existing {
		t.Fatalf("first stage: %v existing=%v", err, existing)
	}

	// Same key, same target, same name and size -> the existing upload is replayed.
	replay := baseUploadRequest(kbA)
	replay.IdempotencyKey = first.IdempotencyKey
	replayed, existing, err := staging.Stage(context.Background(), replay, bytes.NewReader([]byte("abc")))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !existing || replayed.UploadID != record.UploadID {
		t.Fatalf("a replayed key must reuse the upload, got %+v existing=%v", replayed, existing)
	}

	// Same key, different target knowledge base -> conflict.
	other := baseUploadRequest(kbB)
	other.IdempotencyKey = first.IdempotencyKey
	_, _, err = staging.Stage(context.Background(), other, bytes.NewReader([]byte("abc")))
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeIdempotencyConflict {
		t.Fatalf("expected IDEMPOTENCY_CONFLICT, got %v", err)
	}
}

// shared_upload_preserves_caller: an upload staged by a shared knowledge base's
// caller is resolved by that caller and NOT by the owner tenant.
func TestSharedUploadPreservesCaller(t *testing.T) {
	staging, _, _ := newTestStaging(t)
	req := baseUploadRequest(kbA)
	// A shared knowledge base owned by another workspace.
	req.OwnerTenantID = 99
	record, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}

	// The original caller resolves it.
	if _, err := staging.Get(context.Background(), uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	}); err != nil {
		t.Fatalf("the original caller must resolve its own upload: %v", err)
	}

	// The owner tenant must NOT be able to resolve it by pretending to be the
	// caller: the storage id is part of the key.
	_, err = staging.Get(context.Background(), uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 99, CallerStorageID: "web_user:user-1",
	})
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeUploadNotFound {
		t.Fatalf("another workspace must not resolve this upload, got %v", err)
	}
	// Nor may a different user in the same workspace.
	_, err = staging.Get(context.Background(), uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-2",
	})
	if !errors.As(err, &toolErr) || toolErr.Code != CodeUploadNotFound {
		t.Fatalf("another user must not resolve this upload, got %v", err)
	}
	// Nor an id that belongs to another endpoint.
	_, err = staging.Get(context.Background(), uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-2",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	})
	if !errors.As(err, &toolErr) || toolErr.Code != CodeUploadNotFound {
		t.Fatalf("another endpoint must not resolve this upload, got %v", err)
	}
}

// concurrent_import_single_document: only one claim may succeed while a live
// lease is held.
func TestConcurrentImportSingleClaim(t *testing.T) {
	staging, _, _ := newTestStaging(t)
	req := baseUploadRequest(kbA)
	record, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	lookup := uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	}
	first, err := staging.Claim(context.Background(), lookup, uploadLeaseTTL)
	if err != nil || first == nil {
		t.Fatalf("first claim: lease=%v err=%v", first, err)
	}
	second, err := staging.Claim(context.Background(), lookup, uploadLeaseTTL)
	if err != nil {
		t.Fatalf("second claim errored: %v", err)
	}
	if second != nil {
		t.Fatal("a second concurrent claim must not succeed while the lease is live")
	}
}

// expired_import_lease_recovers: a crashed importer's lease can be taken over.
func TestExpiredImportLeaseRecovers(t *testing.T) {
	staging, repo, _ := newTestStaging(t)
	req := baseUploadRequest(kbA)
	record, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	lookup := uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	}
	lease, err := staging.Claim(context.Background(), lookup, uploadLeaseTTL)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	// Simulate a crash: the lease deadline passes with no completion.
	past := time.Now().UTC().Add(-time.Minute)
	repo.rows[record.UploadID].LeaseExpiresAt = &past

	recovered, err := staging.Claim(context.Background(), lookup, uploadLeaseTTL)
	if err != nil {
		t.Fatalf("recovering claim errored: %v", err)
	}
	if recovered == nil {
		t.Fatal("an expired lease must be recoverable")
	}
	if recovered.LeaseToken == lease.LeaseToken {
		t.Fatal("the recovering attempt must get a new lease token")
	}
}

// lost_response_replays_receipt: a second import of an already imported upload
// replays the receipt, and the receipt survives the payload cleanup.
func TestLostResponseReplaysReceipt(t *testing.T) {
	staging, _, _ := newTestStaging(t)
	req := baseUploadRequest(kbA)
	record, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	lookup := uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	}
	lease, err := staging.Claim(context.Background(), lookup, uploadLeaseTTL)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := staging.Complete(context.Background(), lease, UploadCompletion{
		DocumentID: "doc-1", Deduplicated: false, Queued: true,
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// The payload is gone, but the receipt must still resolve.
	if _, statErr := os.Stat(record.PayloadPath); !os.IsNotExist(statErr) {
		t.Fatalf("the payload should be cleaned up after the receipt is written: %v", statErr)
	}
	replayed, err := staging.Get(context.Background(), lookup)
	if err != nil {
		t.Fatalf("the receipt must survive the payload cleanup: %v", err)
	}
	if replayed.State != UploadStateImported || replayed.DocumentID != "doc-1" || !replayed.Queued {
		t.Fatalf("replayed receipt = %+v", replayed)
	}
}

// A failure returns the upload to a retryable state and keeps its diagnosis.
func TestFailureIsRecoverableAndRecorded(t *testing.T) {
	staging, _, _ := newTestStaging(t)
	req := baseUploadRequest(kbA)
	record, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	lookup := uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	}
	lease, err := staging.Claim(context.Background(), lookup, uploadLeaseTTL)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := staging.Fail(context.Background(), lease, CodeProcessingError, "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	after, err := staging.Get(context.Background(), lookup)
	if err != nil {
		t.Fatalf("get after failure: %v", err)
	}
	if after.State != UploadStateStaged {
		t.Fatalf("a failed import must be retryable, state = %q", after.State)
	}
	if after.LastErrorCode != CodeProcessingError || after.LastErrorMessage != "boom" {
		t.Fatalf("the failure must be recorded: %+v", after)
	}
}

// A staged upload past its byte deadline reports UPLOAD_EXPIRED rather than
// pretending it can still be imported.
func TestExpiredUploadReportsExpired(t *testing.T) {
	staging, repo, _ := newTestStaging(t)
	req := baseUploadRequest(kbA)
	record, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	repo.rows[record.UploadID].ExpiresAt = past

	_, err = staging.Get(context.Background(), uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	})
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != CodeUploadExpired {
		t.Fatalf("expected UPLOAD_EXPIRED, got %v", err)
	}
}

// The sweep must not touch an upload whose lease is still held.
func TestSweepRespectsLiveLease(t *testing.T) {
	staging, repo, _ := newTestStaging(t)
	req := baseUploadRequest(kbA)
	record, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	lookup := uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	}
	if _, err := staging.Claim(context.Background(), lookup, uploadLeaseTTL); err != nil {
		t.Fatalf("claim: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	repo.rows[record.UploadID].ExpiresAt = past

	payloads, _, err := staging.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if payloads != 0 {
		t.Fatalf("a leased import must not be swept, removed %d", payloads)
	}
}

// A file name is validated before it can reach any path or header.
func TestSanitizeUploadFileName(t *testing.T) {
	if _, err := sanitizeUploadFileName("a.exe"); err == nil {
		t.Fatal(".exe must be refused")
	}
	for _, name := range []string{"a.txt", "A.MD", "b.docx", "c.pdf", "d.markdown"} {
		if _, err := sanitizeUploadFileName(name); err != nil {
			t.Fatalf("%s should be accepted: %v", name, err)
		}
	}
	got, err := sanitizeUploadFileName("dir/sub/report.pdf")
	if err != nil || got != "report.pdf" {
		t.Fatalf("got %q err %v, want report.pdf", got, err)
	}
	if _, err := sanitizeUploadFileName(""); err == nil {
		t.Fatal("an empty name must be refused")
	}
}

func TestSanitizeMultipartFilenameStripsQuoting(t *testing.T) {
	if got := sanitizeMultipartFilename("a\"b\r\n.txt"); strings.ContainsAny(got, "\"\r\n") {
		t.Fatalf("quoting must be stripped, got %q", got)
	}
	if got := sanitizeMultipartFilename(""); got != "document" {
		t.Fatalf("an empty name must fall back to a safe default, got %q", got)
	}
}

func sha256HexForTest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// --- concurrent first-time uploads sharing one Idempotency-Key --------------

// racingReceiptRepo models the mcp_uploads idempotency unique index and adds a
// two-party barrier: both callers finish their initial lookup before either
// Create runs, so the "both miss, then race Create" interleaving is
// deterministic instead of sleep-timed. createErr, when set, replaces Create
// entirely so error passthrough can be tested.
type racingReceiptRepo struct {
	*fakeReceiptRepo
	barrier chan struct{}
	mu      sync.Mutex
	lookups int
	// lookupMisses makes the next N lookups answer "not found", modelling the
	// race window where a winner row exists but the initial lookup ran before
	// it was committed.
	lookupMisses int
	createErr    error
}

func (r *racingReceiptRepo) FindByIdempotencyKey(
	ctx context.Context, endpointID string, callerWorkspaceID uint64, callerStorageID, key string,
) (*UploadRecord, error) {
	if r.barrier != nil && key != "" {
		r.mu.Lock()
		r.lookups++
		if r.lookups == 2 {
			close(r.barrier)
		}
		r.mu.Unlock()
		<-r.barrier
	}
	r.mu.Lock()
	if r.lookupMisses > 0 {
		r.lookupMisses--
		r.mu.Unlock()
		return nil, nil
	}
	r.mu.Unlock()
	return r.fakeReceiptRepo.FindByIdempotencyKey(ctx, endpointID, callerWorkspaceID, callerStorageID, key)
}

func (r *racingReceiptRepo) Create(_ context.Context, record *UploadRecord) error {
	if r.createErr != nil {
		return r.createErr
	}
	r.fakeReceiptRepo.mu.Lock()
	defer r.fakeReceiptRepo.mu.Unlock()
	if record.IdempotencyKey != "" {
		for _, row := range r.fakeReceiptRepo.rows {
			if row.EndpointID == record.EndpointID &&
				row.CallerWorkspaceID == record.CallerWorkspaceID &&
				row.CallerStorageID == record.CallerStorageID &&
				row.IdempotencyKey == record.IdempotencyKey {
				return fmt.Errorf(
					"constraint failed: UNIQUE constraint failed: index '%s' (2067)",
					idempotencyIndexName)
			}
		}
	}
	copyRecord := *record
	copyRecord.PayloadPath = ""
	r.fakeReceiptRepo.rows[record.UploadID] = &copyRecord
	r.fakeReceiptRepo.created++
	return nil
}

// stageUploadDirs counts the upload directories still on disk: one per upload id
// whose bytes were not cleaned up.
func stageUploadDirs(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && uploadIDPattern.MatchString(d.Name()) {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk staging root: %v", err)
	}
	return count
}

type stageOutcome struct {
	record UploadRecord
	replay bool
	err    error
}

func stagePair(t *testing.T, staging UploadStaging, req UploadRequest, first, second string) (stageOutcome, stageOutcome) {
	t.Helper()
	results := make(chan stageOutcome, 2)
	for _, payload := range []string{first, second} {
		go func(payload string) {
			record, replay, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte(payload)))
			results <- stageOutcome{record: record, replay: replay, err: err}
		}(payload)
	}
	return <-results, <-results
}

func newRacingStaging(t *testing.T) (UploadStaging, *fakeReceiptRepo, *racingReceiptRepo, string) {
	t.Helper()
	root := t.TempDir()
	base := newFakeReceiptRepo()
	racing := &racingReceiptRepo{fakeReceiptRepo: base, barrier: make(chan struct{})}
	return NewStagedUploadStorage(root, racing), base, racing, root
}

// Two first-time uploads of the same key and identical bytes: both must
// succeed, resolve to the SAME upload id, and exactly one is a replay. The
// loser's duplicate bytes are cleaned up.
func TestConcurrentFirstUploadSameKeyReplaysTheWinner(t *testing.T) {
	staging, _, _, root := newRacingStaging(t)
	req := baseUploadRequest(kbA)
	req.IdempotencyKey = "race-key-same"

	a, b := stagePair(t, staging, req, "identical-bytes", "identical-bytes")

	if a.err != nil || b.err != nil {
		t.Fatalf("both racers must succeed: a=%v b=%v", a.err, b.err)
	}
	if a.record.UploadID != b.record.UploadID {
		t.Fatalf("both callers must resolve to the winner: %s vs %s", a.record.UploadID, b.record.UploadID)
	}
	if a.replay == b.replay {
		t.Fatalf("exactly one caller is the replay: a.replay=%v b.replay=%v", a.replay, b.replay)
	}
	if dirs := stageUploadDirs(t, root); dirs != 1 {
		t.Fatalf("only the winner's bytes may remain, found %d staged dirs", dirs)
	}
	winner, err := staging.Get(context.Background(), uploadLookup{
		UploadID: a.record.UploadID, EndpointID: req.EndpointID,
		CallerWorkspaceID: req.CallerWorkspaceID, CallerStorageID: req.CallerStorageID,
	})
	if err != nil {
		t.Fatalf("the winner receipt must stay readable: %v", err)
	}
	if _, err := os.Stat(winner.PayloadPath); err != nil {
		t.Fatalf("the winner's bytes must stay on disk: %v", err)
	}
}

// The same race with DIFFERENT bytes: one wins, the other gets the same
// explicit conflict the sequential replay path returns, the winner is not
// overwritten, and the loser's bytes are cleaned up.
func TestConcurrentFirstUploadDifferentBytesConflicts(t *testing.T) {
	staging, base, _, root := newRacingStaging(t)
	req := baseUploadRequest(kbA)
	req.IdempotencyKey = "race-key-different"

	a, b := stagePair(t, staging, req, "winner-bytes", "loser-bytes")

	var loser stageOutcome
	for _, outcome := range []stageOutcome{a, b} {
		if outcome.err != nil {
			loser = outcome
		}
	}
	if loser.err == nil {
		t.Fatalf("one racer must lose the key: a=%v/%v b=%v/%v", a.err, a.replay, b.err, b.replay)
	}
	var toolErr *ToolError
	if !errors.As(loser.err, &toolErr) || toolErr.Code != CodeIdempotencyConflict {
		t.Fatalf("the loser must get IDEMPOTENCY_CONFLICT, got %v", loser.err)
	}
	if dirs := stageUploadDirs(t, root); dirs != 1 {
		t.Fatalf("the loser's bytes must be cleaned up, found %d staged dirs", dirs)
	}
	base.mu.Lock()
	defer base.mu.Unlock()
	if len(base.rows) != 1 {
		t.Fatalf("exactly the winner receipt must exist, found %d", len(base.rows))
	}
}

// Only the idempotency-index violation may recover. A lock timeout, a
// disconnected database or a primary-key collision has no winner to fall back
// to and must surface unchanged.
func TestStageSurfacesRealCreateErrorsInsteadOfFakingReplay(t *testing.T) {
	for name, injected := range map[string]error{
		"database locked":      errors.New("database is locked"),
		"database unavailable": errors.New("database unavailable"),
		"primary key conflict": errors.New("UNIQUE constraint failed: mcp_uploads.id"),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			base := newFakeReceiptRepo()
			racing := &racingReceiptRepo{fakeReceiptRepo: base, createErr: injected}
			staging := NewStagedUploadStorage(root, racing)
			req := baseUploadRequest(kbA)
			req.IdempotencyKey = "key-" + name

			_, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("payload")))
			if err == nil {
				t.Fatal("the real database error must surface")
			}
			var toolErr *ToolError
			if errors.As(err, &toolErr) {
				t.Fatalf("a failure with no winner must not be reported as %s: %v", toolErr.Code, err)
			}
			if !strings.Contains(err.Error(), injected.Error()) {
				t.Fatalf("the original error must survive, got %v", err)
			}
			if len(base.rows) != 0 {
				t.Fatalf("no receipt may be written when Create fails")
			}
			if dirs := stageUploadDirs(t, root); dirs != 0 {
				t.Fatalf("the failed upload must clean its staged bytes, found %d dirs", dirs)
			}
		})
	}
}

// A real database failure must not masquerade as an idempotent replay even
// when a matching winner receipt already exists: with the previous
// "every Create error is the index race" heuristic this returned
// IDEMPOTENCY_CONFLICT (or a bogus replay) instead of the lock error.
func TestStageLockErrorWithExistingWinnerSurfacesTheRealError(t *testing.T) {
	root := t.TempDir()
	base := newFakeReceiptRepo()
	req := baseUploadRequest(kbA)
	req.IdempotencyKey = "locked-with-winner"
	// Seed the winner the failed Create would have collided with.
	winner := UploadRecord{
		UploadID: newUploadID(), EndpointID: req.EndpointID,
		CallerWorkspaceID: req.CallerWorkspaceID, CallerStorageID: req.CallerStorageID,
		OwnerTenantID: req.OwnerTenantID, KnowledgeBaseID: req.KnowledgeBaseID,
		IdempotencyKey: req.IdempotencyKey, FileName: req.FileName,
		FileSize: 3, SHA256: sha256HexForTest("old"), State: UploadStateStaged,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(UploadPayloadTTL),
		ReceiptExpiresAt: time.Now().UTC().Add(UploadReceiptTTL),
	}
	if err := base.Create(context.Background(), &winner); err != nil {
		t.Fatalf("seed winner: %v", err)
	}
	locked := errors.New("database is locked")
	// The seeded winner exists but the first lookup ran "before" it committed.
	racing := &racingReceiptRepo{fakeReceiptRepo: base, lookupMisses: 1, createErr: locked}
	staging := NewStagedUploadStorage(root, racing)

	_, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("new")))
	if err == nil {
		t.Fatal("the lock error must surface")
	}
	if !strings.Contains(err.Error(), locked.Error()) {
		t.Fatalf("the original lock error must survive, got %v", err)
	}
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		t.Fatalf("a lock error must never become %s", toolErr.Code)
	}
}

// A genuine index conflict whose winner cannot be re-read keeps the original
// error instead of reporting a replay that cannot be resolved.
func TestStageConflictWithoutReadableWinnerKeepsTheRealError(t *testing.T) {
	root := t.TempDir()
	base := newFakeReceiptRepo()
	racing := &racingReceiptRepo{
		fakeReceiptRepo: base,
		createErr: fmt.Errorf("constraint failed: UNIQUE constraint failed: index '%s' (2067)",
			idempotencyIndexName),
	}
	staging := NewStagedUploadStorage(root, racing)
	req := baseUploadRequest(kbA)
	req.IdempotencyKey = "orphan-conflict"

	_, _, err := staging.Stage(context.Background(), req, bytes.NewReader([]byte("payload")))
	if err == nil {
		t.Fatal("an unresolvable conflict must fail")
	}
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		t.Fatalf("no winner is readable, so no idempotency ToolError may be fabricated: %v", err)
	}
	if dirs := stageUploadDirs(t, root); dirs != 0 {
		t.Fatalf("staged bytes must be cleaned up, found %d dirs", dirs)
	}
}

// The unique index is partial (WHERE idempotency_key <> ”): two keyless
// uploads must both create receipts, each with its own upload id.
func TestStageEmptyKeyAllowsConcurrentCreates(t *testing.T) {
	root := t.TempDir()
	base := newFakeReceiptRepo()
	racing := &racingReceiptRepo{fakeReceiptRepo: base}
	staging := NewStagedUploadStorage(root, racing)
	req := baseUploadRequest(kbA) // no Idempotency-Key

	a, b := stagePair(t, staging, req, "one", "two")
	if a.err != nil || b.err != nil {
		t.Fatalf("keyless uploads must not conflict: a=%v b=%v", a.err, b.err)
	}
	if a.replay || b.replay {
		t.Fatalf("keyless uploads are never replays: a=%v b=%v", a.replay, b.replay)
	}
	if a.record.UploadID == b.record.UploadID {
		t.Fatalf("keyless uploads must keep distinct ids, both got %s", a.record.UploadID)
	}
	if dirs := stageUploadDirs(t, root); dirs != 2 {
		t.Fatalf("both keyless payloads must remain, found %d dirs", dirs)
	}
}
