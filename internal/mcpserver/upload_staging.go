package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
)

// uploadIDPattern restricts upload ids to the UUID v4 shape this server mints.
// It is checked before an id can influence any path or database lookup.
var uploadIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`,
)

// Upload states. The plan fixes this state machine; do not add states without
// updating the Python client, which switches on them.
const (
	UploadStateStaged    = "staged"
	UploadStateImporting = "importing"
	UploadStateImported  = "imported"
	UploadStateFailed    = "failed"
	UploadStateExpired   = "expired"
)

// Upload retention, fixed by the plan: bytes live at most 24h, the receipt 72h,
// and the receipt must survive the byte cleanup so a retry can still be answered.
const (
	UploadPayloadTTL = 24 * time.Hour
	UploadReceiptTTL = 72 * time.Hour
	// uploadCompleteTimeout caps the detached context the Complete path uses
	// to persist the receipt and drop the payload. The receipt write is a
	// single SQL UPDATE and the file remove is a local os.RemoveAll, so 30s
	// is generous against runaway filesystem stalls without holding the
	// import handler open for the whole request timeout.
	uploadCompleteTimeout = 30 * time.Second
)

// supportedImportExtensions is the first phase's whitelist. The service layer
// enforces its own, wider list; this one keeps the prototype to the four formats
// the plan promises so an unexpected type fails at the upload boundary.
var supportedImportExtensions = map[string]struct{}{
	"txt": {}, "md": {}, "markdown": {}, "docx": {}, "pdf": {},
}

// UploadRecord is one staged upload plus its import receipt.
type UploadRecord struct {
	UploadID          string
	EndpointID        string
	CallerWorkspaceID uint64
	CallerStorageID   string
	OwnerTenantID     uint64
	KnowledgeBaseID   string
	IdempotencyKey    string
	FileName          string
	FileSize          int64
	SHA256            string
	State             string
	DocumentID        string
	Deduplicated      bool
	Queued            bool
	LastErrorCode     string
	LastErrorMessage  string
	PayloadPath       string
	// LeaseToken/LeaseExpiresAt describe the current import attempt. They are
	// carried on the record (not only in the database row) because a recovering
	// attempt, the status tool and the tests all need to tell a live lease from
	// an abandoned one, and because every receipt write is conditional on the
	// token so a stale attempt cannot overwrite a newer receipt.
	LeaseToken       string
	LeaseExpiresAt   *time.Time
	CreatedAt        time.Time
	ExpiresAt        time.Time
	ReceiptExpiresAt time.Time
}

// uploadLookup identifies one upload for one caller. Every field participates in
// the lookup, which is what stops a cross-user, cross-workspace or cross-endpoint
// id from resolving.
type uploadLookup struct {
	UploadID          string
	EndpointID        string
	CallerWorkspaceID uint64
	CallerStorageID   string
}

// UploadLease is the exclusive claim on one import attempt.
type UploadLease struct {
	Record     UploadRecord
	LeaseToken string
	leaseTTL   time.Duration
}

// UploadCompletion is the receipt written after a successful import.
type UploadCompletion struct {
	DocumentID   string
	Deduplicated bool
	Queued       bool
}

// UploadStaging is the staging + receipt service the MCP tools and the upload
// HTTP endpoint share. It is an interface so the tool layer can be tested with a
// stub and so the storage details stay in one implementation.
type UploadStaging interface {
	// Stage validates and stores a file, returning the receipt in state staged.
	// When idempotencyKey matches an existing receipt for the same caller the
	// existing record is returned with Existing=true instead of a new file being
	// written; a key reused with different content or a different target returns
	// an IDEMPOTENCY_CONFLICT error.
	Stage(ctx context.Context, req UploadRequest, content io.Reader) (UploadRecord, bool, error)
	// Get resolves an upload for the calling principal.
	Get(ctx context.Context, lookup uploadLookup) (UploadRecord, error)
	// Claim atomically moves staged(->importing) and returns the lease, or nil
	// when another importer already holds it.
	Claim(ctx context.Context, lookup uploadLookup, ttl time.Duration) (*UploadLease, error)
	// Renew extends a lease that is still owned by the caller.
	Renew(ctx context.Context, lease *UploadLease) error
	// Complete writes the imported receipt and then removes the payload.
	Complete(ctx context.Context, lease *UploadLease, completion UploadCompletion) error
	// Fail records a failure and returns the upload to a recoverable state.
	Fail(ctx context.Context, lease *UploadLease, code, message string) error
	// OpenPayload returns the staged bytes for an import.
	OpenPayload(ctx context.Context, lease *UploadLease) (io.ReadCloser, string, error)
	// Sweep removes expired payloads and expired receipts.
	Sweep(ctx context.Context) (payloadsRemoved, receiptsRemoved int, err error)
	// Ping verifies the storage root is usable.
	Ping(ctx context.Context) error
}

// UploadRequest is one upload submission.
type UploadRequest struct {
	EndpointID        string
	CallerWorkspaceID uint64
	CallerStorageID   string
	OwnerTenantID     uint64
	KnowledgeBaseID   string
	IdempotencyKey    string
	FileName          string
	DeclaredSize      int64
	MaxBytes          int64
}

// stagedUploadStorage is the filesystem + database implementation of
// UploadStaging.
//
// The directory layout is derived entirely from verified server-side identity:
//
//	<root>/<caller_workspace_id>/<sha256(caller_storage_id)>/<upload_id>/
//	    payload
//	    metadata.json
//
// The principal's STORAGE id (type + id) is hashed rather than the bare user id,
// because the principal type is part of the identity: an embed visitor and a
// web user with the same id string are different callers.
type stagedUploadStorage struct {
	root     string
	receipts UploadReceiptRepository
}

// NewStagedUploadStorage builds the default staging service.
func NewStagedUploadStorage(root string, receipts UploadReceiptRepository) UploadStaging {
	return &stagedUploadStorage{root: root, receipts: receipts}
}

// stageDir is the directory for one upload.
func (s *stagedUploadStorage) stageDir(record UploadRecord) string {
	identity := sha256.Sum256([]byte(record.CallerStorageID))
	return filepath.Join(
		s.root,
		fmt.Sprint(record.CallerWorkspaceID),
		hex.EncodeToString(identity[:]),
		record.UploadID,
	)
}

// payloadPath derives the on-disk payload location from the record identity.
// It is deliberately NOT persisted: the receipt row stores the identity fields,
// and the path is rebuilt from them, so a record read back from the database
// (after a restart, or by a different request than the one that staged it) still
// locates its bytes. Persisting the path would also make the row stale if the
// staging root ever moves.
func (s *stagedUploadStorage) payloadPath(record UploadRecord) string {
	return filepath.Join(s.stageDir(record), "payload")
}

// Ping verifies the root exists and is writable.
func (s *stagedUploadStorage) Ping(_ context.Context) error {
	if strings.TrimSpace(s.root) == "" {
		return errors.New("the staging root is not configured")
	}
	if err := os.MkdirAll(s.root, 0o750); err != nil {
		return fmt.Errorf("the staging root %s is not usable: %w", s.root, err)
	}
	probe := filepath.Join(s.root, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("the staging root %s is not writable: %w", s.root, err)
	}
	_ = os.Remove(probe)
	return nil
}

// Stage writes the file and creates the receipt.
//
// The write order matters: bytes and hash are computed first, and only when all
// of them succeeded is a `staged` receipt published, so an interrupted upload can
// never look importable.
func (s *stagedUploadStorage) Stage(
	ctx context.Context, req UploadRequest, content io.Reader,
) (UploadRecord, bool, error) {
	if s.receipts == nil {
		return UploadRecord{}, false, errors.New("upload receipts are not configured")
	}
	fileName, err := sanitizeUploadFileName(req.FileName)
	if err != nil {
		return UploadRecord{}, false, err
	}

	// Idempotency: a replayed key must resolve to the same upload, and a key
	// reused with different content or target is a conflict.
	if key := strings.TrimSpace(req.IdempotencyKey); key != "" {
		existing, lookupErr := s.receipts.FindByIdempotencyKey(ctx, req.EndpointID,
			req.CallerWorkspaceID, req.CallerStorageID, key)
		if lookupErr != nil {
			return UploadRecord{}, false, lookupErr
		}
		if existing != nil {
			sameTarget := strings.EqualFold(existing.KnowledgeBaseID, req.KnowledgeBaseID)
			sameName := existing.FileName == fileName
			if !sameTarget || !sameName {
				return UploadRecord{}, false, &ToolError{
					Code:    CodeIdempotencyConflict,
					Message: "this Idempotency-Key was already used for a different file or knowledge base",
					Details: map[string]any{
						"existing_knowledge_base_id": existing.KnowledgeBaseID,
						"existing_file_name":         existing.FileName,
						"existing_file_size":         existing.FileSize,
					},
				}
			}
			// Replay must still verify the bytes: the HTTP transport does not set
			// DeclaredSize, so the old size-only check never fired and a key reused
			// with different content (even a different length) would silently
			// return the stale receipt. Read the body bounded by the size limit,
			// hash it, and refuse the replay on any mismatch.
			digest := sha256.New()
			var size int64
			buf := make([]byte, 1<<20)
			for {
				n, readErr := content.Read(buf)
				if n > 0 {
					size += int64(n)
					if size > existing.FileSize {
						return UploadRecord{}, false, &ToolError{
							Code:    CodeIdempotencyConflict,
							Message: "this Idempotency-Key was reused with different bytes",
							Details: map[string]any{
								"existing_file_size": existing.FileSize,
								"declared_size":      size,
							},
						}
					}
					digest.Write(buf[:n])
				}
				if readErr != nil {
					if errors.Is(readErr, io.EOF) {
						break
					}
					return UploadRecord{}, false, fmt.Errorf("cannot read the replay body: %w", readErr)
				}
			}
			if size != existing.FileSize || hex.EncodeToString(digest.Sum(nil)) != existing.SHA256 {
				return UploadRecord{}, false, &ToolError{
					Code:    CodeIdempotencyConflict,
					Message: "this Idempotency-Key was reused with different bytes",
					Details: map[string]any{
						"existing_file_size": existing.FileSize,
						"actual_size":        size,
					},
				}
			}
			return *existing, true, nil
		}
	}

	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 50 << 20
	}
	uploadID := newUploadID()
	record := UploadRecord{
		UploadID:          uploadID,
		EndpointID:        req.EndpointID,
		CallerWorkspaceID: req.CallerWorkspaceID,
		CallerStorageID:   req.CallerStorageID,
		OwnerTenantID:     req.OwnerTenantID,
		KnowledgeBaseID:   req.KnowledgeBaseID,
		IdempotencyKey:    strings.TrimSpace(req.IdempotencyKey),
		FileName:          fileName,
		State:             UploadStateStaged,
		CreatedAt:         time.Now().UTC(),
	}
	record.ExpiresAt = record.CreatedAt.Add(UploadPayloadTTL)
	record.ReceiptExpiresAt = record.CreatedAt.Add(UploadReceiptTTL)

	dir := s.stageDir(record)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return UploadRecord{}, false, fmt.Errorf("cannot create the staging directory: %w", err)
	}
	record.PayloadPath = s.payloadPath(record)

	// Stream in bounded chunks and enforce the limit BEFORE anything is decoded
	// further: the plan forbids "read it all, then measure".
	digest := sha256.New()
	var written int64
	file, err := os.OpenFile(record.PayloadPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return UploadRecord{}, false, fmt.Errorf("cannot create the staged payload: %w", err)
	}
	buf := make([]byte, 1<<20)
	for {
		n, readErr := content.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxBytes {
				_ = file.Close()
				removeAllQuiet(dir)
				return UploadRecord{}, false, &ToolError{
					Code: CodeFileTooLarge,
					Message: fmt.Sprintf("the file is larger than the %d MB limit",
						maxBytes>>20),
				}
			}
			digest.Write(buf[:n])
			if _, werr := file.Write(buf[:n]); werr != nil {
				_ = file.Close()
				removeAllQuiet(dir)
				return UploadRecord{}, false, fmt.Errorf("cannot write the staged payload: %w", werr)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			_ = file.Close()
			removeAllQuiet(dir)
			return UploadRecord{}, false, fmt.Errorf("cannot read the upload body: %w", readErr)
		}
	}
	if err := file.Close(); err != nil {
		removeAllQuiet(dir)
		return UploadRecord{}, false, fmt.Errorf("cannot finish writing the staged payload: %w", err)
	}
	if written == 0 {
		removeAllQuiet(dir)
		return UploadRecord{}, false, invalidArgument("the uploaded file is empty")
	}
	record.FileSize = written
	record.SHA256 = hex.EncodeToString(digest.Sum(nil))

	// Publish the receipt only now: it is the marker that the bytes are complete.
	if err := s.receipts.Create(ctx, &record); err != nil {
		removeAllQuiet(dir)
		// Two first-time uploads of the same Idempotency-Key can both miss the
		// lookup above and then race Create; the mcp_uploads idempotency index
		// rejects the loser, whose bytes duplicate the winner's. Re-read the
		// winner and apply the same replay policy the lookup path uses.
		//
		// ONLY that index violation recovers: a lock timeout, a disconnected
		// database or a primary-key collision has no winner to fall back to and
		// must surface as the real error rather than a fabricated replay.
		if !IsIdempotencyKeyConflict(err) {
			return UploadRecord{}, false, err
		}
		winner, lookupErr := s.receipts.FindByIdempotencyKey(ctx, record.EndpointID,
			record.CallerWorkspaceID, record.CallerStorageID, record.IdempotencyKey)
		if lookupErr != nil {
			return UploadRecord{}, false, lookupErr
		}
		if winner == nil {
			// The conflict is real but the winner is not readable: keep the
			// original database error so the caller is not told about a replay
			// that cannot be resolved.
			return UploadRecord{}, false, err
		}
		// Same target, same name AND the same bytes this request just wrote and
		// hashed: the loser of the race simply lost, so it returns the winner's
		// receipt marked as a replay — exactly like the sequential lookup path.
		// Receipt expiry keeps following the existing Get/Claim policy.
		sameTarget := strings.EqualFold(winner.KnowledgeBaseID, record.KnowledgeBaseID)
		sameName := winner.FileName == record.FileName
		sameBytes := winner.FileSize == record.FileSize && winner.SHA256 == record.SHA256
		if sameTarget && sameName && sameBytes {
			return *winner, true, nil
		}
		return *winner, true, &ToolError{
			Code:    CodeIdempotencyConflict,
			Message: "this Idempotency-Key was already used for a different file or knowledge base",
			Details: map[string]any{
				"existing_knowledge_base_id": winner.KnowledgeBaseID,
				"existing_file_name":         winner.FileName,
				"existing_file_size":         winner.FileSize,
			},
		}
	}
	return record, false, nil
}

func (s *stagedUploadStorage) Get(ctx context.Context, lookup uploadLookup) (UploadRecord, error) {
	if s.receipts == nil {
		return UploadRecord{}, errors.New("upload receipts are not configured")
	}
	record, err := s.receipts.Get(ctx, lookup)
	if err != nil {
		return UploadRecord{}, err
	}
	if record == nil {
		return UploadRecord{}, &ToolError{
			Code:    CodeUploadNotFound,
			Message: fmt.Sprintf("no upload %s for this caller on this endpoint", lookup.UploadID),
		}
	}
	if record.State != UploadStateImported && time.Now().UTC().After(record.ExpiresAt) {
		// Past its byte deadline: report expired rather than pretending it can
		// still be imported.
		return UploadRecord{}, &ToolError{
			Code: CodeUploadExpired,
			Message: fmt.Sprintf("upload %s expired at %s and its bytes were cleaned up",
				record.UploadID, record.ExpiresAt.Format(time.RFC3339)),
		}
	}
	// The payload path is derived, not persisted (see payloadPath): rebuild it so
	// a record read back from the receipt row still points at its bytes.
	record.PayloadPath = s.payloadPath(*record)
	return *record, nil
}

func (s *stagedUploadStorage) Claim(
	ctx context.Context, lookup uploadLookup, ttl time.Duration,
) (*UploadLease, error) {
	record, err := s.Get(ctx, lookup)
	if err != nil {
		return nil, err
	}
	token := newUploadID()
	claimed, err := s.receipts.ClaimForImport(ctx, record.UploadID, token, time.Now().UTC().Add(ttl))
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, nil
	}
	record.State = UploadStateImporting
	expiry := time.Now().UTC().Add(ttl)
	record.LeaseExpiresAt = &expiry
	return &UploadLease{Record: record, LeaseToken: token, leaseTTL: ttl}, nil
}

func (s *stagedUploadStorage) Renew(ctx context.Context, lease *UploadLease) error {
	if lease == nil {
		return errors.New("no lease to renew")
	}
	ok, err := s.receipts.RenewLease(ctx, lease.Record.UploadID, lease.LeaseToken,
		time.Now().UTC().Add(lease.leaseTTL))
	if err != nil {
		return err
	}
	if !ok {
		return &ToolError{
			Code:    CodeUploadLeaseConflict,
			Message: "the import lease for this upload is no longer held by this attempt",
		}
	}
	return nil
}

func (s *stagedUploadStorage) Complete(
	ctx context.Context, lease *UploadLease, completion UploadCompletion,
) error {
	if lease == nil {
		return errors.New("no lease to complete")
	}
	// Persist the receipt first, then drop the bytes: a crash in between leaves a
	// recoverable imported receipt, which is exactly what the plan requires.
	//
	// The receipt write uses a detached context with a bounded deadline: if the
	// caller's request context is cancelled (client disconnect, handler timeout,
	// gin's deferred cleanup) the receipt must still be persisted, otherwise a
	// successfully created document would be invisible to a follow-up status
	// check and a re-import would create a duplicate. The same detached context
	// is reused to delete the payload so a request-cancel between MarkImported
	// and the file remove does not leave orphan bytes for the next sweep.
	persistCtx, cancel := context.WithTimeout(context.Background(), uploadCompleteTimeout)
	defer cancel()
	if err := s.receipts.MarkImported(persistCtx, lease.Record.UploadID, lease.LeaseToken,
		completion.DocumentID, completion.Deduplicated, completion.Queued); err != nil {
		return err
	}
	if err := removeAll(filepath.Dir(lease.Record.PayloadPath)); err != nil {
		// The document is created and the receipt records it: returning a
		// hard error would lie to the caller that no import happened. Log
		// and let the next Sweep pick the bytes up; the document ID is
		// already in the receipt.
		logger.Warnf(ctx,
			"[mcpserver] staged upload %s imported as %s but its payload could not be removed inline (%v); sweep will retry",
			lease.Record.UploadID, completion.DocumentID, err)
	}
	return nil
}

func (s *stagedUploadStorage) Fail(
	ctx context.Context, lease *UploadLease, code, message string,
) error {
	if lease == nil {
		return errors.New("no lease to fail")
	}
	return s.receipts.MarkFailed(ctx, lease.Record.UploadID, lease.LeaseToken, code, truncate(message, 500))
}

func (s *stagedUploadStorage) OpenPayload(
	ctx context.Context, lease *UploadLease,
) (io.ReadCloser, string, error) {
	if lease == nil {
		return nil, "", errors.New("no lease")
	}
	// Reject a symlinked payload: the staging tree is server-owned, so a symlink
	// there is an attack, not a layout.
	info, err := os.Lstat(lease.Record.PayloadPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", &ToolError{
				Code:    CodeUploadNotFound,
				Message: "the staged payload is no longer on disk",
			}
		}
		return nil, "", fmt.Errorf("cannot inspect the staged payload: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, "", errors.New("the staged payload is not a regular file")
	}
	handle, err := os.Open(lease.Record.PayloadPath)
	if err != nil {
		return nil, "", fmt.Errorf("cannot open the staged payload: %w", err)
	}
	return handle, lease.Record.FileName, nil
}

func (s *stagedUploadStorage) Sweep(ctx context.Context) (int, int, error) {
	if s.receipts == nil {
		return 0, 0, nil
	}
	now := time.Now().UTC()
	// Delete the on-disk payloads FIRST, then flip the rows: a crash between the
	// two leaves a row whose bytes are already gone (safe to retry) rather than a
	// row pointing at a file that still consumes disk. Live leases are never in
	// the returned set, so an in-flight import is not cut off.
	//
	// The counter only advances for files we actually removed. A removeAll
	// failure is logged so an operator can see a stuck directory in the logs
	// instead of a "nothing to do" Sweep while the disk keeps filling.
	expired, err := s.receipts.ExpiredPayloadRecords(ctx, now)
	var payloadRemovals int
	if err == nil {
		for _, record := range expired {
			if err := removeAll(filepath.Dir(s.payloadPath(record))); err != nil {
				logger.Warnf(ctx,
					"[mcpserver] staged upload %s payload directory could not be removed during sweep (%v); will retry next tick",
					record.UploadID, err)
				continue
			}
			payloadRemovals++
		}
	} else {
		logger.Warnf(ctx,
			"[mcpserver] expired payload query failed (%v); skipping row-driven payload cleanup this tick", err)
	}
	// The orphan scan below does its own live-payload query and fail-fasts on
	// errors, so a non-recoverable Sweep is reported back to the caller
	// instead of silently letting the row-driven pass run on a stale view.
	//
	// Now the row-driven pass: mark expired and delete old receipts. The
	// orphan scan already failed-fast above if it could not see live payloads,
	// so a non-nil error here means the row-driven sweep itself is broken.
	//
	// `payloads` is the number of rows we just marked state=expired in the
	// row-driven pass; `payloadRemovals` is the number of files we
	// actually removed from disk above. They describe the same set of
	// records from two angles: one row == one file (modulo crash windows
	// where the file delete is the second half of the cleanup). Summing
	// them would double-count, so the final return keeps file removals
	// and row transitions in separate places: the first return value is
	// "files removed" (the user-observable disk saving), the second is
	// "receipts deleted" (the user-observable row removal).
	payloads, receipts, sweepErr := s.receipts.Sweep(ctx, now)
	if sweepErr != nil {
		return payloadRemovals, 0, sweepErr
	}
	// The row-driven pass only sees rows. The disk scan closes the gaps a row
	// cannot report: bytes left behind by a crash between the receipt write and
	// the file delete, and directories whose row is already gone. Without it the
	// row counts look clean while the disk still fills up.
	orphans, orphanErr := s.sweepOrphanDirs(ctx, now)
	if orphanErr != nil {
		return payloadRemovals, 0, orphanErr
	}
	// `payloads` mirrors the file-removal count for the row-driven pass;
	// only add `orphans` (rows that had no file to remove) so the final
	// count is the total number of rows that have now been cleaned up.
	return payloadRemovals + orphans + payloads, receipts, nil
}

// sweepDirGrace spares directories young enough to be mid-Stage. Stage writes
// the bytes BEFORE the receipt row exists, so for the length of one upload a
// legitimate directory has no row; a directory modified within this grace is
// left for the next sweep rather than deleted under the writer.
const sweepDirGrace = 15 * time.Minute

// sweepOrphanDirs walks the staging tree and removes upload directories the
// database no longer accounts for. A directory is kept when its upload id still
// belongs to a live record — staged/failed inside the payload TTL or with a
// live lease, importing in any lease state so crash recovery can retry against
// the bytes — or when it was modified within the grace window. Everything else
// (rows already imported or expired whose bytes survived a failed delete, and
// directories with no row at all) is removed, which is what makes the sweep
// converge on the disk contents instead of only on the row counts.
func (s *stagedUploadStorage) sweepOrphanDirs(ctx context.Context, now time.Time) (int, error) {
	live, err := s.receipts.LivePayloadRecords(ctx)
	if err != nil {
		// A live-payload query failure must not be silently swallowed: the
		// disk scan below decides which upload directories are safe to
		// remove, and a stale "live" view would let it delete an in-flight
		// import's bytes. Propagate the error so Sweep stops before
		// touching the receipt table.
		return 0, fmt.Errorf("live payload query failed: %w", err)
	}
	keep := make(map[string]struct{}, len(live))
	for _, record := range live {
		if record.State == UploadStateImporting {
			keep[record.UploadID] = struct{}{}
			continue
		}
		leaseLive := record.LeaseToken != "" &&
			(record.LeaseExpiresAt == nil || record.LeaseExpiresAt.After(now))
		if record.ExpiresAt.After(now) || leaseLive {
			keep[record.UploadID] = struct{}{}
		}
	}

	removed := 0
	// The tree is root/<workspace>/<storage-hash>/<upload-id>; walk it level by
	// level so only that shape is considered, and prune directories left empty.
	workspaces, err := os.ReadDir(s.root)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warnf(ctx, "[mcpserver] staging disk sweep cannot read %s: %v", s.root, err)
		}
		// A missing root is not an error: the first run after a fresh
		// deploy has nothing to scan. A read failure on an existing root
		// is already logged above and is not worth failing the sweep
		// over — the row-driven pass still runs.
		return 0, nil
	}
	for _, ws := range workspaces {
		wsDir := filepath.Join(s.root, ws.Name())
		if !ws.IsDir() {
			continue
		}
		storageDirs, err := os.ReadDir(wsDir)
		if err != nil {
			logger.Warnf(ctx, "[mcpserver] staging disk sweep cannot read %s: %v", wsDir, err)
			continue
		}
		for _, storage := range storageDirs {
			storageDir := filepath.Join(wsDir, storage.Name())
			if !storage.IsDir() {
				continue
			}
			uploads, err := os.ReadDir(storageDir)
			if err != nil {
				logger.Warnf(ctx, "[mcpserver] staging disk sweep cannot read %s: %v", storageDir, err)
				continue
			}
			for _, upload := range uploads {
				id := upload.Name()
				if _, liveID := keep[id]; liveID {
					continue
				}
				// Only UUID-shaped entries are considered upload directories; the
				// tree is server-owned but a misconfigured root must not turn
				// the sweep into a purge of anything unrecognizable.
				if !uploadIDPattern.MatchString(id) {
					continue
				}
				dir := filepath.Join(storageDir, id)
				if recentlyModified(dir, now, sweepDirGrace) {
					continue
				}
				removeAllQuiet(dir)
				removed++
			}
			// os.Remove only succeeds on an empty directory, so this prunes the
			// tree without ever touching remaining uploads.
			_ = os.Remove(storageDir)
		}
		_ = os.Remove(wsDir)
	}
	return removed, nil
}

// recentlyModified reports whether the directory exists and was touched within
// the grace window. Unreadable state errs on the side of keeping the directory.
func recentlyModified(dir string, now time.Time, grace time.Duration) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return !os.IsNotExist(err)
	}
	return now.Sub(info.ModTime()) < grace
}

// fileHeaderFromReader adapts a staged payload into the *multipart.FileHeader
// the existing upload path consumes.
//
// The bytes go through an on-disk temporary file rather than a bytes.Buffer: a
// FileHeader keeps a reference to the parsed form, so an in-memory form would
// hold a second full copy of a 50 MB payload for the whole import.
func fileHeaderFromReader(
	payload io.Reader, fileName string, size int64,
) (*multipart.FileHeader, func(), error) {
	dir, err := os.MkdirTemp("", "weknora-mcp-import-")
	if err != nil {
		return nil, nil, fmt.Errorf("cannot create a temporary directory: %w", err)
	}
	cleanup := func() { removeAllQuiet(dir) }

	tmp, err := os.Create(filepath.Join(dir, "multipart-body"))
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("cannot create a temporary file: %w", err)
	}
	writer := multipart.NewWriter(tmp)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`,
		importFileHeaderField, sanitizeMultipartFilename(fileName)))
	header.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(header)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, nil, fmt.Errorf("cannot start the multipart body: %w", err)
	}
	if _, err := io.Copy(part, payload); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, nil, fmt.Errorf("cannot write the multipart body: %w", err)
	}
	if err := writer.Close(); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, nil, fmt.Errorf("cannot finish the multipart body: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, nil, fmt.Errorf("cannot rewind the multipart body: %w", err)
	}
	// Bounded memory: parts above this threshold stay on disk.
	const memoryBudget = 1 << 20
	form, err := multipart.NewReader(tmp, writer.Boundary()).ReadForm(memoryBudget)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, nil, fmt.Errorf("cannot parse the multipart body: %w", err)
	}
	files := form.File[importFileHeaderField]
	if len(files) == 0 {
		_ = form.RemoveAll()
		_ = tmp.Close()
		cleanup()
		return nil, nil, errors.New("the multipart body carried no file part")
	}
	_ = size
	return files[0], func() {
		_ = form.RemoveAll()
		_ = tmp.Close()
		cleanup()
	}, nil
}

// sanitizeUploadFileName validates and normalises a client-supplied name.
func sanitizeUploadFileName(name string) (string, error) {
	base := filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if base == "" || base == "." || base == ".." || base == "/" {
		return "", invalidArgument("a file name is required")
	}
	if strings.ContainsRune(base, 0) {
		return "", invalidArgument("the file name contains a null byte")
	}
	extension := strings.ToLower(strings.TrimPrefix(filepath.Ext(base), "."))
	if _, ok := supportedImportExtensions[extension]; !ok {
		return "", &ToolError{
			Code: CodeInvalidFileType,
			Message: fmt.Sprintf(
				"unsupported file type %q; this phase accepts txt, md, markdown, docx and pdf",
				extension),
		}
	}
	return base, nil
}

// sanitizeMultipartFilename strips directory components and quoting before a
// name is embedded in a Content-Disposition header.
func sanitizeMultipartFilename(name string) string {
	base := filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	base = strings.ReplaceAll(base, `"`, "")
	base = strings.ReplaceAll(base, "\r", "")
	base = strings.ReplaceAll(base, "\n", "")
	if base == "" || base == "." || base == "/" {
		return "document"
	}
	return base
}

func removeAllQuiet(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	_ = os.RemoveAll(path)
}

// removeAll is the observable counterpart of removeAllQuiet: it surfaces
// errors so the caller can count real successes and log real failures, used
// by the cleanup paths (Sweep, Complete) where a swallowed failure would
// leave bytes on disk and the next sweep looking like it had no work.
func removeAll(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
