package mcpserver

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// The disk scan must converge on what is actually on disk, not only on what the
// receipt rows report: a crash between a receipt write and the payload delete,
// or a directory whose row is already gone, would otherwise hold its bytes
// until manual intervention.
func TestSweepRemovesOrphanDirsOnDisk(t *testing.T) {
	staging, _, _ := newTestStaging(t)
	ctx := context.Background()

	live, _, err := staging.Stage(ctx, baseUploadRequest(kbA), bytes.NewReader([]byte("live")))
	if err != nil {
		t.Fatalf("stage live: %v", err)
	}
	liveDir := filepath.Dir(live.PayloadPath)

	// An import whose receipt was written but whose bytes were not yet removed
	// (the Complete crash window): state=imported, so only the disk scan can
	// see the leftover.
	imported, _, err := staging.Stage(ctx, baseUploadRequest(kbA), bytes.NewReader([]byte("imported")))
	if err != nil {
		t.Fatalf("stage imported: %v", err)
	}
	lease, err := staging.Claim(ctx, uploadLookup{
		UploadID: imported.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	}, uploadLeaseTTL)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := staging.Complete(ctx, lease, UploadCompletion{DocumentID: "doc-1"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Recreate the leftover bytes the crash window leaves behind, then age the
	// directory past the grace window so the sweep may touch it.
	importedDir := filepath.Dir(imported.PayloadPath)
	if err := os.MkdirAll(importedDir, 0o750); err != nil {
		t.Fatalf("recreate %s: %v", importedDir, err)
	}
	if err := os.WriteFile(filepath.Join(importedDir, "payload"), []byte("leftover"), 0o640); err != nil {
		t.Fatalf("write leftover payload: %v", err)
	}
	ageDir(t, importedDir)

	// A directory with no receipt row at all, and one that is young enough to
	// be mid-Stage (Stage writes the bytes before the row exists).
	orphanDir := filepath.Join(filepath.Dir(liveDir), "aaaaaab1-bbbb-4ccc-8ddd-eeeeffff0001")
	freshDir := filepath.Join(filepath.Dir(liveDir), "aaaaaab1-bbbb-4ccc-8ddd-eeeeffff0002")
	for _, dir := range []string{orphanDir, freshDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	ageDir(t, orphanDir)

	if _, _, err := staging.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := os.Stat(liveDir); err != nil {
		t.Fatalf("a staged upload inside its TTL must keep its bytes: %v", err)
	}
	if _, err := os.Stat(importedDir); !os.IsNotExist(err) {
		t.Fatalf("leftover bytes of an imported receipt must be removed, stat err=%v", err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("a directory with no receipt row must be removed, stat err=%v", err)
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Fatalf("a young directory may be mid-Stage and must survive the sweep: %v", err)
	}
}

// ageDir recreates the directory with an old modification time so the sweep's
// grace window does not protect it.
func ageDir(t *testing.T, dir string) string {
	t.Helper()
	old := time.Now().Add(-2 * sweepDirGrace)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("age %s: %v", dir, err)
	}
	return dir
}

// An importing row past its payload deadline still owns its bytes: the lease
// may be picked up by crash recovery, which re-imports the same bytes.
func TestSweepKeepsImportingBytesPastPayloadTTL(t *testing.T) {
	staging, repo, _ := newTestStaging(t)
	ctx := context.Background()

	record, _, err := staging.Stage(ctx, baseUploadRequest(kbA), bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	lookup := uploadLookup{
		UploadID: record.UploadID, EndpointID: "ep-1",
		CallerWorkspaceID: 7, CallerStorageID: "web_user:user-1",
	}
	if _, err := staging.Claim(ctx, lookup, uploadLeaseTTL); err != nil {
		t.Fatalf("claim: %v", err)
	}
	repo.rows[record.UploadID].ExpiresAt = time.Now().UTC().Add(-time.Hour)
	dir := filepath.Dir(record.PayloadPath)

	if _, _, err := staging.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("crash recovery needs the bytes of an importing row: %v", err)
	}
}

// LivePayloadRecords drives the disk scan's keep-set; verify the SQL predicate
// against the real repository: staged and failed rows are live, importing rows
// are live in any lease state, imported and expired rows are not — their bytes
// are exactly what the scan exists to remove.
func TestLivePayloadRecordsForTheDiskScan(t *testing.T) {
	repo := NewUploadReceiptRepository(newReceiptTestDB(t))
	ctx := context.Background()

	staged := seedReceipt(t, repo, UploadStateStaged)
	failed := seedReceipt(t, repo, UploadStateFailed)
	importing := seedReceipt(t, repo, UploadStateImporting)
	imported := seedReceipt(t, repo, UploadStateImported)
	expired := seedReceipt(t, repo, UploadStateExpired)

	live, err := repo.LivePayloadRecords(ctx)
	if err != nil {
		t.Fatalf("live payload records: %v", err)
	}
	got := map[string]bool{}
	for _, record := range live {
		got[record.UploadID] = true
	}
	for _, record := range []UploadRecord{staged, failed, importing} {
		if !got[record.UploadID] {
			t.Fatalf("state %q must count as a live payload (id %s)", record.State, record.UploadID)
		}
	}
	for _, record := range []UploadRecord{imported, expired} {
		if got[record.UploadID] {
			t.Fatalf("state %q must not count as a live payload (id %s)", record.State, record.UploadID)
		}
	}
}

// The runner resolves the staging service per tick, sweeps once after the
// startup delay, and stops cleanly — including when nothing is configured.
func TestStagingSweepRunnerSweepsThenStops(t *testing.T) {
	var sweeps atomic.Int64
	runner := NewStagingSweepRunner(func() UploadStaging {
		sweeps.Add(1)
		return nil // nothing configured; the sweep must still tick and stop
	})
	runner.startDelay = 5 * time.Millisecond
	runner.interval = time.Hour

	runner.Start(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for sweeps.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if sweeps.Load() == 0 {
		t.Fatal("the runner never ran its first sweep")
	}

	done := make(chan struct{})
	go func() { runner.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked although the sweep loop must exit on demand")
	}
	if sweeps.Load() > 1 {
		t.Fatalf("the sweep must not tick faster than its interval, ran %d times", sweeps.Load())
	}
}

// A repeat Start must not spawn a second loop: the ResourceCleaner wiring and
// any future duplicate call site stay harmless.
func TestStagingSweepRunnerStartIsIdempotent(t *testing.T) {
	var sweeps atomic.Int64
	runner := NewStagingSweepRunner(func() UploadStaging {
		sweeps.Add(1)
		return nil
	})
	runner.startDelay = time.Hour // never fires; only loop count would matter
	runner.interval = time.Hour

	runner.Start(context.Background())
	runner.Start(context.Background())
	runner.Stop()
	if sweeps.Load() != 0 {
		t.Fatalf("no sweep may run before its startup delay, ran %d times", sweeps.Load())
	}
}
