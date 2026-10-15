package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// idempotencyIndexName is the unique index that arbitrates concurrent
// first-time uploads sharing one Idempotency-Key (see the mcp_uploads
// migrations). Create() can only lose that race as a violation of THIS index.
const idempotencyIndexName = "idx_mcp_uploads_idempotency"

// IsIdempotencyKeyConflict reports whether a Create() failure is the
// mcp_uploads idempotency-index violation — the one error Stage may recover
// from by re-reading the winner.
//
// Two driver shapes exist. PostgreSQL names the constraint
// (`duplicate key value violates unique constraint "idx_mcp_uploads_idempotency"`);
// SQLite (mattn/go-sqlite3) reports the indexed COLUMN list instead
// (`UNIQUE constraint failed: mcp_uploads.endpoint_id, ...`) because the index
// is a plain partial index. Both are matched here, but only in forms that
// can belong to this index: the exact four-column set (any order) or the index
// name. A primary-key collision ("UNIQUE constraint failed: mcp_uploads.id"
// alone), a lock timeout or a disconnected database matches neither and must
// surface as the real error — widening this to every uniqueness/constraint
// failure would let Stage fabricate an idempotent replay with no winner to
// recover from. upload_repository_test.go pins the SQLite shape against the
// real migration index.
func IsIdempotencyKeyConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, idempotencyIndexName) {
		return true
	}
	return strings.Contains(msg, "UNIQUE constraint failed:") &&
		strings.Contains(msg, "mcp_uploads.endpoint_id") &&
		strings.Contains(msg, "mcp_uploads.caller_workspace_id") &&
		strings.Contains(msg, "mcp_uploads.caller_principal_storage_id") &&
		strings.Contains(msg, "mcp_uploads.idempotency_key")
}

// UploadReceipt is the persisted row for one staged upload (table mcp_uploads).
//
// It lives in WeKnora's own database on purpose: the receipt has to survive the
// byte cleanup and a service restart, which is what makes a lost response
// recoverable without creating a duplicate document. The thin Python adapter
// keeps no such state.
type UploadReceipt struct {
	ID                       string     `gorm:"column:id;type:varchar(36);primaryKey"`
	EndpointID               string     `gorm:"column:endpoint_id;type:varchar(36);not null;index:idx_mcp_uploads_scope"`
	CallerWorkspaceID        uint64     `gorm:"column:caller_workspace_id;not null;index:idx_mcp_uploads_scope"`
	CallerPrincipalStorageID string     `gorm:"column:caller_principal_storage_id;type:varchar(128);not null;index:idx_mcp_uploads_scope"`
	OwnerTenantID            uint64     `gorm:"column:owner_tenant_id;not null"`
	KnowledgeBaseID          string     `gorm:"column:knowledge_base_id;type:varchar(36);not null;index"`
	IdempotencyKey           string     `gorm:"column:idempotency_key;type:varchar(64);not null;default:'';index:idx_mcp_uploads_idem"`
	FileName                 string     `gorm:"column:file_name;type:varchar(255);not null"`
	FileSize                 int64      `gorm:"column:file_size;not null"`
	SHA256                   string     `gorm:"column:sha256;type:varchar(64);not null;default:''"`
	State                    string     `gorm:"column:state;type:varchar(16);not null;index"`
	DocumentID               string     `gorm:"column:document_id;type:varchar(36);not null;default:''"`
	Deduplicated             bool       `gorm:"column:deduplicated;not null;default:false"`
	Queued                   bool       `gorm:"column:queued;not null;default:false"`
	LastErrorCode            string     `gorm:"column:last_error_code;type:varchar(48);not null;default:''"`
	LastErrorMessage         string     `gorm:"column:last_error_message;type:text;not null;default:''"`
	LeaseToken               string     `gorm:"column:lease_token;type:varchar(36);not null;default:''"`
	LeaseExpiresAt           *time.Time `gorm:"column:lease_expires_at"`
	CreatedAt                time.Time  `gorm:"column:created_at;not null"`
	ExpiresAt                time.Time  `gorm:"column:expires_at;not null;index"`
	ReceiptExpiresAt         time.Time  `gorm:"column:receipt_expires_at;not null;index"`
}

// TableName pins the table name to the migration's.
func (UploadReceipt) TableName() string { return "mcp_uploads" }

func (r UploadReceipt) toRecord() UploadRecord {
	return UploadRecord{
		UploadID:          r.ID,
		EndpointID:        r.EndpointID,
		CallerWorkspaceID: r.CallerWorkspaceID,
		CallerStorageID:   r.CallerPrincipalStorageID,
		OwnerTenantID:     r.OwnerTenantID,
		KnowledgeBaseID:   r.KnowledgeBaseID,
		IdempotencyKey:    r.IdempotencyKey,
		FileName:          r.FileName,
		FileSize:          r.FileSize,
		SHA256:            r.SHA256,
		State:             r.State,
		DocumentID:        r.DocumentID,
		Deduplicated:      r.Deduplicated,
		Queued:            r.Queued,
		LastErrorCode:     r.LastErrorCode,
		LastErrorMessage:  r.LastErrorMessage,
		LeaseToken:        r.LeaseToken,
		LeaseExpiresAt:    r.LeaseExpiresAt,
		CreatedAt:         r.CreatedAt,
		ExpiresAt:         r.ExpiresAt,
		ReceiptExpiresAt:  r.ReceiptExpiresAt,
	}
}

// UploadReceiptRepository persists upload receipts.
//
// The lease methods take a lease_token condition so an importer that lost its
// lease can never overwrite the receipt written by the attempt that took over.
type UploadReceiptRepository interface {
	Create(ctx context.Context, record *UploadRecord) error
	Get(ctx context.Context, lookup uploadLookup) (*UploadRecord, error)
	FindByIdempotencyKey(
		ctx context.Context, endpointID string, callerWorkspaceID uint64,
		callerStorageID, key string,
	) (*UploadRecord, error)
	// ClaimForImport atomically moves staged -> importing, or takes over an
	// importing row whose lease expired (crash recovery). It returns false when
	// another live attempt holds the lease.
	ClaimForImport(ctx context.Context, uploadID, leaseToken string, expiresAt time.Time) (bool, error)
	RenewLease(ctx context.Context, uploadID, leaseToken string, expiresAt time.Time) (bool, error)
	MarkImported(
		ctx context.Context, uploadID, leaseToken, documentID string,
		deduplicated, queued bool,
	) error
	MarkFailed(ctx context.Context, uploadID, leaseToken, code, message string) error
	// Sweep expires payloads (state -> expired) and removes old receipts.
	Sweep(ctx context.Context, now time.Time) (payloadsRemoved, receiptsRemoved int, err error)
	// ExpiredPayloadRecords lists the records whose bytes are past their payload
	// deadline and no longer leased, so the storage layer can delete the files
	// before Sweep flips the rows to expired. Live leases are never listed.
	ExpiredPayloadRecords(ctx context.Context, now time.Time) ([]UploadRecord, error)
	// LivePayloadRecords lists every row whose payload bytes may still be
	// claimed: staged and failed rows, plus importing rows in any lease state
	// because a crashed attempt must be recoverable against the same bytes.
	// The disk scan keeps these directories and removes the rest; imported and
	// expired rows are deliberately excluded so the scan also retries deletions
	// a previous sweep missed.
	LivePayloadRecords(ctx context.Context) ([]UploadRecord, error)
}

type uploadReceiptRepository struct {
	db *gorm.DB
}

// NewUploadReceiptRepository builds the GORM-backed receipt repository.
func NewUploadReceiptRepository(db *gorm.DB) UploadReceiptRepository {
	return &uploadReceiptRepository{db: db}
}

func (r *uploadReceiptRepository) Create(ctx context.Context, record *UploadRecord) error {
	row := UploadReceipt{
		ID:                       record.UploadID,
		EndpointID:               record.EndpointID,
		CallerWorkspaceID:        record.CallerWorkspaceID,
		CallerPrincipalStorageID: record.CallerStorageID,
		OwnerTenantID:            record.OwnerTenantID,
		KnowledgeBaseID:          record.KnowledgeBaseID,
		IdempotencyKey:           record.IdempotencyKey,
		FileName:                 record.FileName,
		FileSize:                 record.FileSize,
		SHA256:                   record.SHA256,
		State:                    record.State,
		CreatedAt:                record.CreatedAt,
		ExpiresAt:                record.ExpiresAt,
		ReceiptExpiresAt:         record.ReceiptExpiresAt,
	}
	return r.db.WithContext(ctx).Create(&row).Error
}

// scopeQuery pins every lookup to the full caller identity. A missing predicate
// here would be a cross-user read, so the query is built in one place.
func (r *uploadReceiptRepository) scopeQuery(
	ctx context.Context, lookup uploadLookup,
) *gorm.DB {
	return r.db.WithContext(ctx).
		Where("id = ?", lookup.UploadID).
		Where("endpoint_id = ?", lookup.EndpointID).
		Where("caller_workspace_id = ?", lookup.CallerWorkspaceID).
		Where("caller_principal_storage_id = ?", lookup.CallerStorageID)
}

func (r *uploadReceiptRepository) Get(
	ctx context.Context, lookup uploadLookup,
) (*UploadRecord, error) {
	var row UploadReceipt
	err := r.scopeQuery(ctx, lookup).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record := row.toRecord()
	return &record, nil
}

func (r *uploadReceiptRepository) FindByIdempotencyKey(
	ctx context.Context, endpointID string, callerWorkspaceID uint64,
	callerStorageID, key string,
) (*UploadRecord, error) {
	var row UploadReceipt
	err := r.db.WithContext(ctx).
		Where("endpoint_id = ?", endpointID).
		Where("caller_workspace_id = ?", callerWorkspaceID).
		Where("caller_principal_storage_id = ?", callerStorageID).
		Where("idempotency_key = ?", key).
		Order("created_at DESC").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record := row.toRecord()
	return &record, nil
}

func (r *uploadReceiptRepository) ClaimForImport(
	ctx context.Context, uploadID, leaseToken string, expiresAt time.Time,
) (bool, error) {
	now := time.Now().UTC()
	// One conditional UPDATE is the whole concurrency control: staged rows and
	// importing rows with an expired lease are both claimable, and the WHERE
	// clause decides which. No read-then-write race exists.
	//
	// The payload deadline guard is essential: a row whose bytes are already
	// past expires_at must NOT be claimable, because reading the payload file
	// would race with the next sweep that deletes it. The state path that
	// takes over a crashed import keeps the guard so a row whose lease just
	// expired but whose bytes are also past the deadline is not silently
	// re-imported.
	result := r.db.WithContext(ctx).
		Model(&UploadReceipt{}).
		Where("id = ?", uploadID).
		Where("expires_at > ?", now).
		Where("state = ? OR (state = ? AND (lease_expires_at IS NULL OR lease_expires_at < ?))",
			UploadStateStaged, UploadStateImporting, now).
		Updates(map[string]any{
			"state":            UploadStateImporting,
			"lease_token":      leaseToken,
			"lease_expires_at": expiresAt,
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *uploadReceiptRepository) RenewLease(
	ctx context.Context, uploadID, leaseToken string, expiresAt time.Time,
) (bool, error) {
	result := r.db.WithContext(ctx).
		Model(&UploadReceipt{}).
		Where("id = ? AND lease_token = ?", uploadID, leaseToken).
		Update("lease_expires_at", expiresAt)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *uploadReceiptRepository) MarkImported(
	ctx context.Context, uploadID, leaseToken, documentID string,
	deduplicated, queued bool,
) error {
	result := r.db.WithContext(ctx).
		Model(&UploadReceipt{}).
		Where("id = ? AND lease_token = ?", uploadID, leaseToken).
		Updates(map[string]any{
			"state":              UploadStateImported,
			"document_id":        documentID,
			"deduplicated":       deduplicated,
			"queued":             queued,
			"lease_token":        "",
			"lease_expires_at":   nil,
			"last_error_code":    "",
			"last_error_message": "",
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		// The receipt was taken over by another attempt: this one must not claim
		// success, but the document does exist, so the error says so.
		return &ToolError{
			Code: CodeUploadLeaseConflict,
			Message: fmt.Sprintf(
				"document %s was created but the upload receipt is now owned by another attempt; "+
					"re-read it with get_upload_status", documentID),
		}
	}
	return nil
}

func (r *uploadReceiptRepository) MarkFailed(
	ctx context.Context, uploadID, leaseToken, code, message string,
) error {
	// A failed import returns the row to `staged` so the caller can retry with
	// the same upload id while the bytes are still retained.
	result := r.db.WithContext(ctx).
		Model(&UploadReceipt{}).
		Where("id = ? AND lease_token = ?", uploadID, leaseToken).
		Updates(map[string]any{
			"state":              UploadStateStaged,
			"last_error_code":    code,
			"last_error_message": message,
			"lease_token":        "",
			"lease_expires_at":   nil,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return &ToolError{
			Code:    CodeUploadLeaseConflict,
			Message: "the import lease for this upload is no longer held by this attempt",
		}
	}
	return nil
}

func (r *uploadReceiptRepository) Sweep(
	_ context.Context, now time.Time,
) (int, int, error) {
	// Payloads: any row that never reached `imported` and is past its byte
	// deadline. A live lease is respected so a long import is not cut off.
	payloads := r.db.Model(&UploadReceipt{}).
		Where("state IN ?", []string{UploadStateStaged, UploadStateFailed, UploadStateExpired}).
		Where("expires_at < ?", now).
		Where("lease_token = ''").
		Update("state", UploadStateExpired)
	if payloads.Error != nil {
		return 0, 0, payloads.Error
	}
	// Crashed imports: an importing row whose lease AND payload are both
	// past their deadlines. Without this pass the row stays in `importing`
	// forever, the bytes get cleaned up by the next orphan scan, and a
	// later status call sees an `importing` row with no payload. Marking
	// it `expired` lets the caller tell "the bytes are gone" from "still
	// being worked on", and lines it up with the sweep's contract that
	// terminal states are imported / failed / expired.
	crashed := r.db.Model(&UploadReceipt{}).
		Where("state = ?", UploadStateImporting).
		Where("expires_at < ?", now).
		Where("lease_token <> ''").
		Where("lease_expires_at < ?", now).
		Updates(map[string]any{
			"state":            UploadStateExpired,
			"lease_token":      "",
			"lease_expires_at": nil,
		})
	if crashed.Error != nil {
		return int(payloads.RowsAffected), 0, crashed.Error
	}
	// Receipts: only after their own, longer deadline, AND only when the
	// row is not held by a live import lease. The lease guard closes a
	// real gap: receipt_expires_at (72h) is comfortably longer than the
	// typical import, but a long-running import that exceeds 72h must not
	// have its bookkeeping deleted out from under it. The filter still
	// cleans up genuinely abandoned rows; the worst case is "an active
	// import holds its row a few hours past the deadline" instead of
	// "the receipt vanishes and a follow-up status call cannot find it".
	receipts := r.db.Where("receipt_expires_at < ?", now).
		Where("NOT (lease_token <> '' AND lease_expires_at > ?)", now).
		Delete(&UploadReceipt{})
	if receipts.Error != nil {
		return int(payloads.RowsAffected) + int(crashed.RowsAffected), 0, receipts.Error
	}
	return int(payloads.RowsAffected) + int(crashed.RowsAffected),
		int(receipts.RowsAffected), nil
}

// ExpiredPayloadRecords returns the records whose bytes are past their payload
// deadline and no longer held by a live lease. The storage layer deletes the
// files first (so a partial failure can be retried against the same rows), then
// Sweep flips them to expired. It deliberately does not return imported rows:
// their bytes are already removed at Complete.
func (r *uploadReceiptRepository) ExpiredPayloadRecords(
	_ context.Context, now time.Time,
) ([]UploadRecord, error) {
	var rows []UploadReceipt
	err := r.db.Where("state IN ?", []string{UploadStateStaged, UploadStateFailed, UploadStateExpired}).
		Where("expires_at < ?", now).
		Where("lease_token = ''").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]UploadRecord, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].toRecord())
	}
	return out, nil
}

// LivePayloadRecords returns the rows the disk sweep must not touch: staged and
// failed rows still inside their payload TTL, plus importing rows regardless of
// age or lease — an expired lease on an importing row is exactly the crash
// recovery case, and recovery re-imports the same bytes.
func (r *uploadReceiptRepository) LivePayloadRecords(ctx context.Context) ([]UploadRecord, error) {
	var rows []UploadReceipt
	err := r.db.WithContext(ctx).
		Where("state IN ?", []string{UploadStateStaged, UploadStateImporting, UploadStateFailed}).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]UploadRecord, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].toRecord())
	}
	return out, nil
}

// AutoMigrateUploadReceipts creates the table when the schema migration has not
// run yet (for example in a unit test with an in-memory database).
func AutoMigrateUploadReceipts(db *gorm.DB) error {
	if db == nil {
		return errors.New("no database")
	}
	return db.AutoMigrate(&UploadReceipt{})
}
