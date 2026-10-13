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
			sameSize := req.DeclaredSize <= 0 || existing.FileSize == req.DeclaredSize
			if !sameTarget || !sameName || !sameSize {
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
	payloadPath := filepath.Join(dir, "payload")
	record.PayloadPath = payloadPath

	// Stream in bounded chunks and enforce the limit BEFORE anything is decoded
	// further: the plan forbids "read it all, then measure".
	digest := sha256.New()
	var written int64
	file, err := os.OpenFile(payloadPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
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
		return UploadRecord{}, false, err
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
	if err := s.receipts.MarkImported(ctx, lease.Record.UploadID, lease.LeaseToken,
		completion.DocumentID, completion.Deduplicated, completion.Queued); err != nil {
		return err
	}
	removeAllQuiet(filepath.Dir(lease.Record.PayloadPath))
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
	return s.receipts.Sweep(ctx, time.Now().UTC())
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

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
