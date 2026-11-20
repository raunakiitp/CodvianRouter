// M9.8 A2A retention prune loop (docs/design/a2a-retention-design.md
// §2/§6 Step 3): a small self-contained background goroutine that
// prunes expired terminal tasks once an hour, first run immediately
// after the A2A store opens.
//
// There is no scheduler or shutdown registry in this codebase, so
// the loop is started from the lazy A2A open path (a2aService) and
// runs until its context is cancelled; process exit ends it, like
// the other background goroutines in this package. The loop never
// blocks shutdown (it selects on ctx.Done), and a prune failure is
// logged, never fatal and never surfaced to a gateway-key caller.
//
// Wiring (integration-B, NOT in this slice): a2aService opens the
// store via a2a.OpenWithLimits with the effective config values and
// starts exactly one loop, guarded next to a2aMu/a2aSvc/a2aCancels.
// This file only defines the loop and its options; it starts nothing
// on its own.
package server

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/a2a"
)

// A2A retention defaults (design §4). The prune interval is a fixed
// code constant, NOT a config key: the operator knob is the
// retention window, not the sweep frequency.
const (
	// DefaultA2ARetentionDays is the default terminal-task
	// retention window in days.
	DefaultA2ARetentionDays = 7
	// DefaultA2AMaxTasksPerKey is the default per-key task cap.
	DefaultA2AMaxTasksPerKey = 1000
	// a2aPruneInterval is the fixed hourly sweep (§2).
	a2aPruneInterval = time.Hour
)

// A2APruneOptions tunes the A2A retention prune loop. Every field
// is explicit: the loop never reads config — the wiring slice passes
// the effective config values in here. Zero values mean the design
// defaults (see the Effective* accessors); PruneEnabled is a
// pointer so "absent" (nil) is distinguishable from an explicit
// false, exactly like config.A2AConfig.PruneEnabled.
type A2APruneOptions struct {
	// RetentionDays is the terminal-task retention window in days.
	// 0/absent = 7.
	RetentionDays int
	// MaxTasksPerKey is the per-key task cap enforced by the store
	// at Create time. The loop carries it so one options value
	// describes the whole retention policy; the loop itself does
	// not enforce the cap. 0/absent = 1000. See Limits().
	MaxTasksPerKey int
	// PruneEnabled turns the prune job on/off. Nil/absent = true
	// (pruning is ON whenever A2A is enabled); explicit false
	// skips pruning entirely and logs once at Start.
	PruneEnabled *bool
	// Interval is the sweep period. 0/absent = 1h (the fixed
	// constant); the field exists so tests can use a short
	// interval. Production MUST leave it 0 — the interval is not
	// an operator knob.
	Interval time.Duration
}

// DefaultA2APruneOptions returns the design defaults: 7-day
// retention, 1,000 tasks per key, pruning on, hourly sweep.
func DefaultA2APruneOptions() A2APruneOptions {
	return A2APruneOptions{
		RetentionDays:  DefaultA2ARetentionDays,
		MaxTasksPerKey: DefaultA2AMaxTasksPerKey,
		PruneEnabled:   boolPtr(true),
		Interval:       a2aPruneInterval,
	}
}

// EffectiveRetentionDays returns the retention window in days:
// the option, or 7 when unset (0) or negative.
func (o A2APruneOptions) EffectiveRetentionDays() int {
	if o.RetentionDays <= 0 {
		return DefaultA2ARetentionDays
	}
	return o.RetentionDays
}

// EffectiveMaxTasksPerKey returns the per-key task cap: the option,
// or 1000 when unset (0) or negative.
func (o A2APruneOptions) EffectiveMaxTasksPerKey() int {
	if o.MaxTasksPerKey <= 0 {
		return DefaultA2AMaxTasksPerKey
	}
	return o.MaxTasksPerKey
}

// EffectivePruneEnabled returns whether pruning runs: true unless
// the operator explicitly set false.
func (o A2APruneOptions) EffectivePruneEnabled() bool {
	if o.PruneEnabled == nil {
		return true
	}
	return *o.PruneEnabled
}

// EffectiveInterval returns the sweep period: the option, or 1h
// when unset (0) or negative.
func (o A2APruneOptions) EffectiveInterval() time.Duration {
	if o.Interval <= 0 {
		return a2aPruneInterval
	}
	return o.Interval
}

// Limits returns the store limits matching these options, for the
// wiring slice to pass to a2a.OpenWithLimits — one options value
// feeds both the store and the loop, so they can never disagree.
func (o A2APruneOptions) Limits() a2a.Limits {
	return a2a.Limits{
		RetentionDays:  o.EffectiveRetentionDays(),
		MaxTasksPerKey: o.EffectiveMaxTasksPerKey(),
	}
}

// A2APruneLoop is one prune-loop goroutine. Start is exactly-once:
// a second Start while the loop runs returns false and starts no
// second goroutine. Stop cancels the loop and waits for it to exit;
// it is safe to call on a loop that never started.
type A2APruneLoop struct {
	store  *a2a.Store
	opts   A2APruneOptions
	logger *log.Logger
	// prune runs one prune pass for now and returns the tasks and
	// events removed. The default calls the store with a cutoff
	// derived from the retention option; tests replace it with a
	// counting stub to observe the loop without waiting out a
	// 7-day retention window.
	prune func(now time.Time) (tasks, events int, err error)

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewA2APruneLoop builds the loop without starting it. A nil logger
// means log.Default(); a nil store means Start refuses to start
// (and logs why) instead of panicking.
func NewA2APruneLoop(store *a2a.Store, opts A2APruneOptions, logger *log.Logger) *A2APruneLoop {
	if logger == nil {
		logger = log.Default()
	}
	l := &A2APruneLoop{store: store, opts: opts, logger: logger}
	l.prune = func(now time.Time) (int, int, error) {
		if store == nil {
			return 0, 0, errors.New("a2a: prune with nil store")
		}
		return store.PruneExpiredBefore(now.AddDate(0, 0, -opts.EffectiveRetentionDays()))
	}
	return l
}

// Start starts the loop exactly once and reports whether this call
// started it. The first prune runs immediately, then hourly. It
// returns false (and starts nothing) when the loop is already
// running, when pruning is disabled by option, or when the store is
// nil. The loop stops when ctx is cancelled or Stop is called.
func (l *A2APruneLoop) Start(ctx context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return false
	}
	if l.store == nil {
		l.logger.Printf("a2a: prune loop not started: nil store")
		return false
	}
	if !l.opts.EffectivePruneEnabled() {
		l.logger.Printf("a2a: pruning disabled by config")
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	l.done = make(chan struct{})
	l.running = true
	go func() {
		defer func() {
			l.mu.Lock()
			l.running = false
			l.mu.Unlock()
			close(l.done)
		}()
		l.run(runCtx)
	}()
	return true
}

// Stop cancels the loop (if running) and waits for its goroutine to
// exit. A no-op on a loop that never started, and safe to call twice.
func (l *A2APruneLoop) Stop() {
	l.mu.Lock()
	cancel, done := l.cancel, l.done
	l.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
}

// Running reports whether the loop goroutine is currently running.
func (l *A2APruneLoop) Running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

// run is the loop body: prune immediately, then on every tick,
// until ctx is done. A prune failure is logged, never fatal — the
// next tick retries.
func (l *A2APruneLoop) run(ctx context.Context) {
	l.pruneOnce()
	ticker := time.NewTicker(l.opts.EffectiveInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.pruneOnce()
		}
	}
}

// pruneOnce runs one prune pass and logs exactly one line per run:
// tasks/events removed and duration, or the error on failure.
func (l *A2APruneLoop) pruneOnce() {
	start := time.Now()
	tasks, events, err := l.prune(time.Now().UTC())
	if err != nil {
		l.logger.Printf("a2a: prune failed: %v", err)
		return
	}
	l.logger.Printf("a2a: prune removed %d tasks (%d events) in %s", tasks, events, time.Since(start).Round(time.Millisecond))
}

// StartA2APruneLoop creates and starts the prune loop with opts and
// returns the handle; use loop.Stop to stop it. This is the call
// integration-B makes from a2aService (once, guarded by a2aMu, next
// to a2aSvc) — see the file header. When pruning is disabled the
// returned loop is simply not running (Running() == false).
func StartA2APruneLoop(ctx context.Context, store *a2a.Store, opts A2APruneOptions, logger *log.Logger) *A2APruneLoop {
	loop := NewA2APruneLoop(store, opts, logger)
	loop.Start(ctx)
	return loop
}

// RunA2APruneLoop runs the prune loop in the calling goroutine
// until ctx is done: prune immediately, then on every tick. It is
// the blocking form of the loop (used by A2APruneLoop internally
// via run, and directly by tests with a short interval).
func RunA2APruneLoop(ctx context.Context, store *a2a.Store, opts A2APruneOptions, logger *log.Logger) {
	NewA2APruneLoop(store, opts, logger).run(ctx)
}
