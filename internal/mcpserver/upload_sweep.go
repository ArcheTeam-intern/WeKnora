package mcpserver

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/runtime"
)

// StagingSweepRunner is the production scheduler for the staged-upload cleanup.
// UploadStaging.Sweep itself is pure logic; without a caller it only ever ran
// from tests, so leftover payload bytes accumulated until the process happened
// to restart. The runner closes that gap.
//
// It follows the house pattern of the audit-log retention runner: a bare
// time.Ticker in a background goroutine — no cron dependency — because the
// payload TTL is 24h and "approximately hourly, eventually" is all the cleanup
// needs. The staging service is resolved once per tick so a server built
// without a database or staging root simply stays dormant instead of failing.
type StagingSweepRunner struct {
	resolve    func() UploadStaging
	interval   time.Duration
	startDelay time.Duration

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
	// started mirrors AuditLogRetentionRunner: set inside startOnce.Do before
	// the goroutine is wired, so Stop can tell "never started" apart from
	// "running" without blocking on a doneCh nobody closes.
	started atomic.Bool
}

const (
	// stagingSweepInterval is the sweep cadence. The payload TTL is 24h, so an
	// hourly sweep bounds any leftover bytes to TTL + ~1h; anything faster just
	// repeats empty scans.
	stagingSweepInterval = time.Hour
	// stagingSweepStartupDelay holds the first sweep until shortly after boot so
	// it does not compete with migrations and the startup request flood, while
	// still clearing crash leftovers the same hour.
	stagingSweepStartupDelay = 5 * time.Minute
)

// NewStagingSweepRunner builds the runner. resolve is called once per tick;
// returning nil (no database, no staging root configured) skips that tick.
func NewStagingSweepRunner(resolve func() UploadStaging) *StagingSweepRunner {
	return &StagingSweepRunner{
		resolve:    resolve,
		interval:   stagingSweepInterval,
		startDelay: stagingSweepStartupDelay,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// Start spins up the sweep loop. Repeat calls are a no-op.
func (r *StagingSweepRunner) Start(ctx context.Context) {
	if r == nil || r.resolve == nil {
		return
	}
	r.startOnce.Do(func() {
		r.started.Store(true)
		logger.Infof(ctx, "[mcpserver] staged-upload sweep starting: interval=%s startup_delay=%s",
			r.interval, r.startDelay)
		go r.loop()
	})
}

// Stop signals the loop to exit and blocks until it returns. Idempotent; a
// runner that was never started stops immediately.
func (r *StagingSweepRunner) Stop() {
	r.StopWithin(0)
}

// StopWithin is Stop with a bound: a positive timeout gives up waiting while a
// sweep is still inside its own deadline, instead of holding later shutdown
// hooks.
func (r *StagingSweepRunner) StopWithin(timeout time.Duration) {
	if r == nil || !r.started.Load() {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
	if runtime.WaitFor(r.doneCh, timeout) {
		return
	}
	logger.Warnf(context.Background(),
		"[mcpserver] staged-upload sweep still running after %s; continuing shutdown", timeout)
}

// loop runs the cadence. Each tick uses a fresh background context because the
// context passed to Start belongs to container init and is cancelled as soon as
// init returns.
func (r *StagingSweepRunner) loop() {
	defer close(r.doneCh)

	timer := time.NewTimer(r.startDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.stopCh:
		return
	}

	r.runOnce()

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.runOnce()
		case <-r.stopCh:
			return
		}
	}
}

// runOnce performs one sweep with a generous deadline; a stalled attempt is
// dropped and retried on the next tick.
func (r *StagingSweepRunner) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	staging := r.resolve()
	if staging == nil {
		logger.Debugf(ctx, "[mcpserver] staged-upload sweep skipped: staging is not configured")
		return
	}
	payloads, receipts, err := staging.Sweep(ctx)
	if err != nil {
		logger.Warnf(ctx,
			"[mcpserver] staged-upload sweep failed (retried next tick): payloads=%d receipts=%d err=%v",
			payloads, receipts, err)
		return
	}
	if payloads > 0 || receipts > 0 {
		logger.Infof(ctx,
			"[mcpserver] staged-upload sweep complete: payloads_removed=%d receipts_removed=%d",
			payloads, receipts)
	} else {
		logger.Debugf(ctx, "[mcpserver] staged-upload sweep complete: nothing to remove")
	}
}
