package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
