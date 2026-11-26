// A2A retention prune-loop tests (M9.8 follow-up,
// docs/design/a2a-retention-design.md §6 Step 3): the loop prunes
// an old terminal task end to end, starts exactly once, stops
// cleanly on cancel, skips when disabled, and survives prune
// failures. The loop's own tests use a real store; the store-level
// prune/cap behaviour is proven in internal/a2a/retention_test.go.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/a2a"
	"github.com/raunakiitp/codvian-router/internal/config"

	_ "modernc.org/sqlite"
)

const pruneTestOwner = "prune-test-owner"

func openPruneStore(t *testing.T) *a2a.Store {
	t.Helper()
	s, err := a2a.Open(t.TempDir())
	if err != nil {
		t.Fatalf("a2a.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mustCompletedPruneTask creates a task and drives it to Completed
// (creation + working + completed = 3 events).
func mustCompletedPruneTask(t *testing.T, s *a2a.Store) *a2a.Task {
	t.Helper()
	task, err := s.Create(pruneTestOwner, json.RawMessage(`{"role":"user","content":"hi"}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Transition(pruneTestOwner, task.ID, a2a.StateWorking, nil, ""); err != nil {
		t.Fatalf("Transition to working: %v", err)
	}
	if _, err := s.Transition(pruneTestOwner, task.ID, a2a.StateCompleted, json.RawMessage(`{"content":"done"}`), ""); err != nil {
		t.Fatalf("Transition to completed: %v", err)
	}
	return task
}

// backdateA2ATaskUpdatedAt sets a task's updated_at via a second
// SQLite connection to the store file. Test-only: the store has no
// backdate API and must not gain one. The store's single writer
// connection stays open; WAL + a busy timeout let the UPDATE land.
// The DSN is a canonical file:/// URI (filepath.ToSlash + leading
// "/", mirroring a2a.OpenWithLimits): on Windows the raw backslash
// path becomes host "C:" plus a %5C-escaped path, which SQLite
// cannot open — the PRAGMA then fails with "out of memory (1)".
func backdateA2ATaskUpdatedAt(t *testing.T, s *a2a.Store, id string, at time.Time) {
	t.Helper()
	slashPath := filepath.ToSlash(s.Path())
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	dsn := (&url.URL{Scheme: "file", Path: slashPath, RawQuery: "_txlock=immediate"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open second sqlite connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		t.Fatalf("busy timeout: %v", err)
	}
	res, err := db.Exec(`UPDATE a2a_tasks SET updated_at = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		t.Fatalf("backdate task %s: %v", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("backdate rows affected: %v", err)
	}
	if n != 1 {
		t.Fatalf("backdate updated %d rows, want 1", n)
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use: the prune
// loop's logger writes to it from the loop goroutine while the
// test reads it — a plain bytes.Buffer races under -race (Gate 1).
// Only the two tests that read the log while the loop runs use it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// TestA2APruneLoopPrunesOldTerminalTask: the loop, started with a
// short test interval, prunes a 10-day-old terminal task on its
// immediate first run and stops cleanly.
func TestA2APruneLoopPrunesOldTerminalTask(t *testing.T) {
	s := openPruneStore(t)
	task := mustCompletedPruneTask(t, s)
	backdateA2ATaskUpdatedAt(t, s, task.ID, time.Now().UTC().AddDate(0, 0, -10))

	var logBuf syncBuffer
	logger := log.New(&logBuf, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := DefaultA2APruneOptions()
	opts.Interval = 20 * time.Millisecond
	loop := StartA2APruneLoop(ctx, s, opts, logger)
	defer loop.Stop()

	waitFor(t, 5*time.Second, "old terminal task to be pruned", func() bool {
		_, err := s.Get(pruneTestOwner, task.ID)
		return errors.Is(err, a2a.ErrNotFound)
	})
	if _, err := s.Events(pruneTestOwner, task.ID); !errors.Is(err, a2a.ErrNotFound) {
		t.Fatalf("Events after prune err = %v, want ErrNotFound (events pruned too)", err)
	}
	if !strings.Contains(logBuf.String(), "a2a: prune removed 1 tasks (3 events)") {
		t.Fatalf("prune log = %q, want a line reporting 1 task (3 events) removed", logBuf.String())
	}

	loop.Stop()
	if loop.Running() {
		t.Fatal("loop still Running after Stop")
	}
}

// TestA2APruneLoopExactlyOnce: Start twice starts one loop — with
// a 10s interval only the immediate first run may fire, so the
// prune stub must be called exactly once despite two Starts. After
// Stop the loop is restartable, and Stop is clean and idempotent.
func TestA2APruneLoopExactlyOnce(t *testing.T) {
	s := openPruneStore(t)
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	opts := DefaultA2APruneOptions()
	opts.Interval = 10 * time.Second // only the immediate run can fire
	loop := NewA2APruneLoop(s, opts, logger)
	var calls atomic.Int32
	loop.prune = func(now time.Time) (int, int, error) {
		calls.Add(1)
		return 0, 0, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if started := loop.Start(ctx); !started {
		t.Fatal("first Start returned false, want true")
	}
	if started := loop.Start(ctx); started {
		t.Fatal("second Start returned true, want false (exactly one loop)")
	}
	if !loop.Running() {
		t.Fatal("loop not Running after Start")
	}
	// Only one immediate run may have happened: two loops would
	// have called prune twice.
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("prune calls = %d, want exactly 1 (one loop, immediate run only)", n)
	}

	loop.Stop()
	if loop.Running() {
		t.Fatal("loop still Running after Stop")
	}
	loop.Stop() // idempotent

	// Restartable after Stop: a fresh Start runs once more.
	if started := loop.Start(ctx); !started {
		t.Fatal("Start after Stop returned false, want true")
	}
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("prune calls after restart = %d, want 2", n)
	}
	loop.Stop()
}

// TestA2APruneLoopStopsOnCancel: the blocking loop returns promptly
// when its context is cancelled, mid-tick.
func TestA2APruneLoopStopsOnCancel(t *testing.T) {
	s := openPruneStore(t)
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	opts := DefaultA2APruneOptions()
	opts.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunA2APruneLoop(ctx, s, opts, logger)
	}()

	time.Sleep(50 * time.Millisecond) // let the immediate run + a tick happen
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunA2APruneLoop did not return within 2s of context cancel")
	}
}

// TestA2APruneLoopDisabled: explicit prune_enabled=false starts no
// loop, calls prune never, and logs the choice once.
func TestA2APruneLoopDisabled(t *testing.T) {
	s := openPruneStore(t)
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	opts := DefaultA2APruneOptions()
	opts.PruneEnabled = boolPtr(false)
	opts.Interval = 10 * time.Millisecond
	loop := NewA2APruneLoop(s, opts, logger)
	var calls atomic.Int32
	loop.prune = func(now time.Time) (int, int, error) {
		calls.Add(1)
		return 0, 0, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if started := loop.Start(ctx); started {
		t.Fatal("Start with pruning disabled returned true, want false")
	}
	if loop.Running() {
		t.Fatal("disabled loop is Running")
	}
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("prune calls = %d, want 0 when disabled", n)
	}
	if !strings.Contains(logBuf.String(), "a2a: pruning disabled by config") {
		t.Fatalf("log = %q, want the pruning-disabled line", logBuf.String())
	}
	loop.Stop() // no-op, must not hang
}

// TestA2APruneLoopFailureIsNotFatal: a prune that always errors is
// logged per run and never kills the loop or blocks Stop.
func TestA2APruneLoopFailureIsNotFatal(t *testing.T) {
	s := openPruneStore(t)
	var logBuf syncBuffer
	logger := log.New(&logBuf, "", 0)

	opts := DefaultA2APruneOptions()
	opts.Interval = 10 * time.Millisecond
	loop := NewA2APruneLoop(s, opts, logger)
	loop.prune = func(now time.Time) (int, int, error) {
		return 0, 0, errors.New("boom")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if started := loop.Start(ctx); !started {
		t.Fatal("Start returned false, want true")
	}
	time.Sleep(50 * time.Millisecond)
	if !loop.Running() {
		t.Fatal("loop stopped after prune failures, want it still running")
	}
	if !strings.Contains(logBuf.String(), "a2a: prune failed: boom") {
		t.Fatalf("log = %q, want the prune-failed line", logBuf.String())
	}
	loop.Stop()
	if loop.Running() {
		t.Fatal("loop still Running after Stop")
	}
}

// TestA2APruneLoopNilStore: Start refuses a nil store instead of
// panicking, and Stop stays a no-op.
func TestA2APruneLoopNilStore(t *testing.T) {
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)
	loop := NewA2APruneLoop(nil, DefaultA2APruneOptions(), logger)
	if started := loop.Start(context.Background()); started {
		t.Fatal("Start with nil store returned true, want false")
	}
	if !strings.Contains(logBuf.String(), "a2a: prune loop not started: nil store") {
		t.Fatalf("log = %q, want the nil-store line", logBuf.String())
	}
	loop.Stop()
}

// TestA2APruneLoopPanicIsContained (AUD-REL-006): a panic inside
// the prune function must end the loop goroutine — logging the
// panic and resetting the running state — instead of crashing the
// whole process (an unrecovered goroutine panic is fatal). After
// the panic the loop is NOT running, Stop is a no-op, and the
// panic text is in the log.
func TestA2APruneLoopPanicIsContained(t *testing.T) {
	s := openPruneStore(t)
	var logBuf syncBuffer
	logger := log.New(&logBuf, "", 0)

	opts := DefaultA2APruneOptions()
	opts.Interval = 10 * time.Millisecond
	loop := NewA2APruneLoop(s, opts, logger)
	loop.prune = func(now time.Time) (int, int, error) {
		panic("prune exploded")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if started := loop.Start(ctx); !started {
		t.Fatal("Start returned false, want true")
	}
	// Wait for the panic to unwind. If recovery did not work this
	// test process itself dies — the assertion below only runs
	// when containment succeeded.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && loop.Running() {
		time.Sleep(5 * time.Millisecond)
	}
	if loop.Running() {
		t.Fatal("loop still Running after a panic in prune, want the goroutine ended")
	}
	if !strings.Contains(logBuf.String(), "a2a: prune loop panicked and stopped: prune exploded") {
		t.Fatalf("log = %q, want the contained-panic line", logBuf.String())
	}
	// Stop on a loop that died panicking must be a safe no-op.
	loop.Stop()
	if loop.Running() {
		t.Fatal("loop Running after Stop following a panic")
	}
}

// TestA2APruneOptionsDefaults: the design defaults (7 days, 1,000
// tasks, pruning on, 1h interval) and the zero-value-means-default
// rule, plus the Limits() bridge to the store.
func TestA2APruneOptionsDefaults(t *testing.T) {
	def := DefaultA2APruneOptions()
	if def.EffectiveRetentionDays() != 7 {
		t.Fatalf("default retention = %d, want 7", def.EffectiveRetentionDays())
	}
	if def.EffectiveMaxTasksPerKey() != 1000 {
		t.Fatalf("default cap = %d, want 1000", def.EffectiveMaxTasksPerKey())
	}
	if !def.EffectivePruneEnabled() {
		t.Fatal("default prune_enabled = false, want true")
	}
	if def.EffectiveInterval() != time.Hour {
		t.Fatalf("default interval = %s, want 1h", def.EffectiveInterval())
	}
	if lim := def.Limits(); lim != (a2a.Limits{RetentionDays: 7, MaxTasksPerKey: 1000}) {
		t.Fatalf("default Limits() = %+v, want {7 1000}", lim)
	}

	zero := A2APruneOptions{}
	if zero.EffectiveRetentionDays() != 7 || zero.EffectiveMaxTasksPerKey() != 1000 ||
		!zero.EffectivePruneEnabled() || zero.EffectiveInterval() != time.Hour {
		t.Fatalf("zero options resolve to retention=%d cap=%d enabled=%v interval=%s, want 7/1000/true/1h",
			zero.EffectiveRetentionDays(), zero.EffectiveMaxTasksPerKey(), zero.EffectivePruneEnabled(), zero.EffectiveInterval())
	}

	explicit := A2APruneOptions{RetentionDays: 30, MaxTasksPerKey: 50, PruneEnabled: boolPtr(false), Interval: time.Minute}
	if explicit.EffectiveRetentionDays() != 30 || explicit.EffectiveMaxTasksPerKey() != 50 ||
		explicit.EffectivePruneEnabled() || explicit.EffectiveInterval() != time.Minute {
		t.Fatalf("explicit options resolve to retention=%d cap=%d enabled=%v interval=%s, want 30/50/false/1m",
			explicit.EffectiveRetentionDays(), explicit.EffectiveMaxTasksPerKey(), explicit.EffectivePruneEnabled(), explicit.EffectiveInterval())
	}
}

// TestA2AConfigNilMeansDesignDefaults: with no "a2a" block in
// config, a2aPruneOptions resolves to the design defaults (7-day
// retention, 1,000 tasks per key, pruning on).
func TestA2AConfigNilMeansDesignDefaults(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	if srv.cfg.A2A != nil {
		t.Fatalf("test server config A2A = %+v, want nil", srv.cfg.A2A)
	}
	opts := srv.a2aPruneOptions()
	if opts.EffectiveRetentionDays() != 7 || opts.EffectiveMaxTasksPerKey() != 1000 || !opts.EffectivePruneEnabled() {
		t.Fatalf("nil-config prune options = retention=%d cap=%d enabled=%v, want 7/1000/true",
			opts.EffectiveRetentionDays(), opts.EffectiveMaxTasksPerKey(), opts.EffectivePruneEnabled())
	}
}

// TestA2AConfigRetentionWiredThroughService: a non-default
// retention/cap from config is honoured end to end — the store
// opened by a2aService enforces cap 2 (oldest terminal evicted
// first) and retention 1 day (a 2-day-old terminal task prunes,
// a recent one survives), and the prune loop runs.
func TestA2AConfigRetentionWiredThroughService(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	srv.cfg.A2A = &config.A2AConfig{RetentionDays: 1, MaxTasksPerKey: 2, PruneEnabled: boolPtr(true)}
	defer srv.a2aStopPruneLoop()

	opts := srv.a2aPruneOptions()
	if opts.EffectiveRetentionDays() != 1 || opts.EffectiveMaxTasksPerKey() != 2 || !opts.EffectivePruneEnabled() {
		t.Fatalf("a2aPruneOptions from config = retention=%d cap=%d enabled=%v, want 1/2/true",
			opts.EffectiveRetentionDays(), opts.EffectiveMaxTasksPerKey(), opts.EffectivePruneEnabled())
	}

	svc, err := srv.a2aService()
	if err != nil {
		t.Fatalf("a2aService: %v", err)
	}
	if srv.a2aPruneLoop == nil || !srv.a2aPruneLoop.Running() {
		t.Fatal("prune loop not running after a2aService with config prune_enabled=true")
	}
	store := svc.Store()

	// Cap 2 from config: creating a 3rd terminal task evicts
	// the oldest terminal task first (design §3).
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, mustCompletedPruneTask(t, store).ID)
	}
	if tasks, err := svc.ListTasks(pruneTestOwner); err != nil || len(tasks) > 2 {
		t.Fatalf("ListTasks after 3 creates with cap 2 = %d tasks (err %v), want <= 2", len(tasks), err)
	}
	if _, err := store.Get(pruneTestOwner, ids[0]); !errors.Is(err, a2a.ErrNotFound) {
		t.Fatalf("oldest task %s survived the config cap-2 eviction (err %v), want ErrNotFound", ids[0], err)
	}
	for _, id := range ids[1:] {
		if _, err := store.Get(pruneTestOwner, id); err != nil {
			t.Fatalf("surviving task %s unreadable after cap eviction: %v", id, err)
		}
	}

	// Retention 1 day from config: a 2-day-old terminal task is
	// pruned by PruneExpired, a recent terminal task survives.
	backdateA2ATaskUpdatedAt(t, store, ids[1], time.Now().UTC().AddDate(0, 0, -2))
	tasks, events, err := store.PruneExpired(time.Now().UTC())
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if tasks != 1 || events != 3 {
		t.Fatalf("PruneExpired removed %d tasks (%d events), want 1 task (3 events) at 1-day retention", tasks, events)
	}
	if _, err := store.Get(pruneTestOwner, ids[1]); !errors.Is(err, a2a.ErrNotFound) {
		t.Fatalf("2-day-old terminal task %s survived 1-day retention (err %v), want ErrNotFound", ids[1], err)
	}
	if _, err := store.Get(pruneTestOwner, ids[2]); err != nil {
		t.Fatalf("recent terminal task %s pruned by 1-day retention: %v", ids[2], err)
	}
}

// TestA2AConfigPruneDisabledStartsNoLoop: config
// prune_enabled=false starts no prune loop, but the per-key cap
// from the same config block is still enforced by the store.
func TestA2AConfigPruneDisabledStartsNoLoop(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	srv.cfg.A2A = &config.A2AConfig{RetentionDays: 1, MaxTasksPerKey: 2, PruneEnabled: boolPtr(false)}
	defer srv.a2aStopPruneLoop()

	if srv.a2aPruneOptions().EffectivePruneEnabled() {
		t.Fatal("a2aPruneOptions with config prune_enabled=false reports enabled")
	}
	svc, err := srv.a2aService()
	if err != nil {
		t.Fatalf("a2aService: %v", err)
	}
	if srv.a2aPruneLoop != nil && srv.a2aPruneLoop.Running() {
		t.Fatal("prune loop running with config prune_enabled=false, want no loop")
	}
	store := svc.Store()
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, mustCompletedPruneTask(t, store).ID)
	}
	if _, err := store.Get(pruneTestOwner, ids[0]); !errors.Is(err, a2a.ErrNotFound) {
		t.Fatalf("oldest task %s survived the config cap-2 eviction with pruning off (err %v), want ErrNotFound", ids[0], err)
	}
}
