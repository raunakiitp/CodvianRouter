// Retention tests (M9.8 follow-up, docs/design/a2a-retention-design.md
// §6 Step 2): PruneExpired and the per-key cap in Create.
package a2a

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// backdateTask sets a task's updated_at to at. Test-only SQL: the
// store has no backdate API and must not gain one.
func backdateTask(t *testing.T, s *Store, id string, at time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE a2a_tasks SET updated_at = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatalf("backdate task %s: %v", id, err)
	}
}

// backdateTaskCreated sets both timestamps: a task created long ago
// whose last transition (updated_at) was recent.
func backdateTaskCreated(t *testing.T, s *Store, id string, createdAt, updatedAt time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE a2a_tasks SET created_at = ?, updated_at = ? WHERE id = ?`,
		createdAt.UTC().Format(time.RFC3339Nano), updatedAt.UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatalf("backdate task %s: %v", id, err)
	}
}

func mustTransition(t *testing.T, s *Store, owner, id string, to State) {
	t.Helper()
	var err error
	switch to {
	case StateWorking:
		_, err = s.Transition(owner, id, StateWorking, nil, "")
	case StateCompleted:
		_, err = s.Transition(owner, id, StateCompleted, json.RawMessage(`{"content":"done"}`), "")
	case StateFailed:
		_, err = s.Transition(owner, id, StateFailed, nil, "boom")
	case StateCancelled:
		_, err = s.Cancel(owner, id)
	default:
		t.Fatalf("mustTransition: unsupported target %q", to)
	}
	if err != nil {
		t.Fatalf("Transition to %s: %v", to, err)
	}
}

func mustComplete(t *testing.T, s *Store, owner, id string) {
	t.Helper()
	mustTransition(t, s, owner, id, StateWorking)
	mustTransition(t, s, owner, id, StateCompleted)
}

func assertGone(t *testing.T, s *Store, owner, id string) {
	t.Helper()
	if _, err := s.Get(owner, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(%s) err = %v, want ErrNotFound", id, err)
	}
	if _, err := s.Events(owner, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Events(%s) err = %v, want ErrNotFound", id, err)
	}
}

func assertPresent(t *testing.T, s *Store, owner, id string) {
	t.Helper()
	if _, err := s.Get(owner, id); err != nil {
		t.Fatalf("Get(%s): %v, want task present", id, err)
	}
}

// TestPruneExpiredDeletesTerminalOnly: old terminal tasks AND their
// events are pruned, recent terminal tasks survive, and
// submitted/working tasks survive however old they are. Returned
// counts are exact.
func TestPruneExpiredDeletesTerminalOnly(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -10)

	oldCompleted := mustCreate(t, s, testOwnerA)
	mustComplete(t, s, testOwnerA, oldCompleted.ID)
	backdateTask(t, s, oldCompleted.ID, old)

	oldFailed := mustCreate(t, s, testOwnerA)
	mustTransition(t, s, testOwnerA, oldFailed.ID, StateWorking)
	mustTransition(t, s, testOwnerA, oldFailed.ID, StateFailed)
	backdateTask(t, s, oldFailed.ID, old)

	oldCancelled := mustCreate(t, s, testOwnerA)
	mustTransition(t, s, testOwnerA, oldCancelled.ID, StateCancelled)
	backdateTask(t, s, oldCancelled.ID, old)

	recentCompleted := mustCreate(t, s, testOwnerA)
	mustComplete(t, s, testOwnerA, recentCompleted.ID)

	oldSubmitted := mustCreate(t, s, testOwnerA)
	backdateTask(t, s, oldSubmitted.ID, old)

	oldWorking := mustCreate(t, s, testOwnerA)
	mustTransition(t, s, testOwnerA, oldWorking.ID, StateWorking)
	backdateTask(t, s, oldWorking.ID, old)

	// Events per task: creation (1) + one per transition.
	// completed/failed: 3 each; cancelled-from-submitted: 2.
	tasks, events, err := s.PruneExpired(now)
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if tasks != 3 {
		t.Fatalf("PruneExpired tasks = %d, want 3", tasks)
	}
	if events != 8 {
		t.Fatalf("PruneExpired events = %d, want 8 (3+3+2)", events)
	}

	assertGone(t, s, testOwnerA, oldCompleted.ID)
	assertGone(t, s, testOwnerA, oldFailed.ID)
	assertGone(t, s, testOwnerA, oldCancelled.ID)
	assertPresent(t, s, testOwnerA, recentCompleted.ID)
	assertPresent(t, s, testOwnerA, oldSubmitted.ID)
	assertPresent(t, s, testOwnerA, oldWorking.ID)

	counts, err := s.CountsByState()
	if err != nil {
		t.Fatalf("CountsByState: %v", err)
	}
	if counts[StateCompleted] != 1 || counts[StateSubmitted] != 1 || counts[StateWorking] != 1 ||
		counts[StateFailed] != 0 || counts[StateCancelled] != 0 {
		t.Fatalf("CountsByState after prune = %v, want completed/submitted/working 1 each, failed/cancelled 0", counts)
	}
}

// TestPruneDeletesEventsBeforeTasks proves the FK order: with
// foreign_keys=ON and no ON DELETE CASCADE, deleting a task row
// while its events exist fails — so a prune that deleted tasks
// first would return that error and prune nothing. PruneExpired
// must succeed and remove both the task and its events.
func TestPruneDeletesEventsBeforeTasks(t *testing.T) {
	s := openTestStore(t)
	task := mustCreate(t, s, testOwnerA)
	mustComplete(t, s, testOwnerA, task.ID)
	if n := eventCount(t, s, testOwnerA, task.ID); n != 3 {
		t.Fatalf("event count = %d, want 3 before prune", n)
	}

	// Task-first deletion must fail on the FK constraint.
	if _, err := s.db.Exec(`DELETE FROM a2a_tasks WHERE id = ?`, task.ID); err == nil {
		t.Fatal("task-first DELETE succeeded, want a FOREIGN KEY failure (events still reference the task)")
	} else if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("task-first DELETE err = %v, want a FOREIGN KEY constraint failure", err)
	}
	assertPresent(t, s, testOwnerA, task.ID)

	// The prune (events first) must succeed and remove everything.
	tasks, events, err := s.PruneExpiredBefore(time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("PruneExpiredBefore: %v", err)
	}
	if tasks != 1 || events != 3 {
		t.Fatalf("PruneExpiredBefore = (%d tasks, %d events), want (1, 3)", tasks, events)
	}
	assertGone(t, s, testOwnerA, task.ID)
}

// TestPruneExpiredUsesUpdatedAt: a task created 10 days ago but
// completed yesterday is NOT pruned — the cutoff is measured from
// updated_at (when it became terminal), never created_at. Once its
// updated_at ages past 7 days, the same task IS pruned.
func TestPruneExpiredUsesUpdatedAt(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()

	task := mustCreate(t, s, testOwnerA)
	mustComplete(t, s, testOwnerA, task.ID)
	// Created 10 days ago, completed (updated) 24 hours ago.
	backdateTaskCreated(t, s, task.ID, now.AddDate(0, 0, -10), now.Add(-24*time.Hour))

	tasks, events, err := s.PruneExpired(now)
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if tasks != 0 || events != 0 {
		t.Fatalf("PruneExpired = (%d, %d), want (0, 0): task updated yesterday must survive", tasks, events)
	}
	assertPresent(t, s, testOwnerA, task.ID)

	// Same task, now 8 days since it became terminal: pruned.
	backdateTask(t, s, task.ID, now.AddDate(0, 0, -8))
	tasks, events, err = s.PruneExpired(now)
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if tasks != 1 || events != 3 {
		t.Fatalf("PruneExpired = (%d, %d), want (1, 3)", tasks, events)
	}
	assertGone(t, s, testOwnerA, task.ID)
}

// TestPruneExpiredRespectsStoreRetention: a store opened with a
// 1-day retention prunes a 2-day-old terminal task but keeps a
// 12-hour-old one; a zero Limits value means the 7-day default.
func TestPruneExpiredRespectsStoreRetention(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenWithLimits(dir, Limits{RetentionDays: 1})
	if err != nil {
		t.Fatalf("OpenWithLimits: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC()

	oldTask := mustCreate(t, s, testOwnerA)
	mustComplete(t, s, testOwnerA, oldTask.ID)
	backdateTask(t, s, oldTask.ID, now.Add(-48*time.Hour))

	recentTask := mustCreate(t, s, testOwnerA)
	mustComplete(t, s, testOwnerA, recentTask.ID)
	backdateTask(t, s, recentTask.ID, now.Add(-12*time.Hour))

	tasks, events, err := s.PruneExpired(now)
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if tasks != 1 || events != 3 {
		t.Fatalf("PruneExpired = (%d, %d), want (1, 3)", tasks, events)
	}
	assertGone(t, s, testOwnerA, oldTask.ID)
	assertPresent(t, s, testOwnerA, recentTask.ID)

	// Zero Limits = defaults: 7-day retention prunes an 8-day-old
	// terminal task.
	s2, err := OpenWithLimits(t.TempDir(), Limits{})
	if err != nil {
		t.Fatalf("OpenWithLimits defaults: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	d := mustCreate(t, s2, testOwnerA)
	mustComplete(t, s2, testOwnerA, d.ID)
	backdateTask(t, s2, d.ID, now.AddDate(0, 0, -8))
	tasks, _, err = s2.PruneExpired(now)
	if err != nil {
		t.Fatalf("PruneExpired (defaults): %v", err)
	}
	if tasks != 1 {
		t.Fatalf("PruneExpired with default retention = %d tasks, want 1", tasks)
	}
}

func TestPruneExpiredEmptyStore(t *testing.T) {
	s := openTestStore(t)
	tasks, events, err := s.PruneExpired(time.Now().UTC())
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if tasks != 0 || events != 0 {
		t.Fatalf("PruneExpired on empty store = (%d, %d), want (0, 0)", tasks, events)
	}
}

// TestCapEvictionOldestTerminalFirst: with a cap of 3, the 4th
// create evicts the OLDEST terminal task; the 3 newest survive and
// ListTasks returns at most 3 rows.
func TestCapEvictionOldestTerminalFirst(t *testing.T) {
	s, err := OpenWithLimits(t.TempDir(), Limits{MaxTasksPerKey: 3})
	if err != nil {
		t.Fatalf("OpenWithLimits: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC()

	var ids [3]string
	for i := range ids {
		task := mustCreate(t, s, testOwnerA)
		mustComplete(t, s, testOwnerA, task.ID)
		ids[i] = task.ID
	}
	// Distinct updated_at ordering, oldest first.
	backdateTask(t, s, ids[0], now.AddDate(0, 0, -3))
	backdateTask(t, s, ids[1], now.AddDate(0, 0, -2))
	backdateTask(t, s, ids[2], now.AddDate(0, 0, -1))

	fourth := mustCreate(t, s, testOwnerA)

	assertGone(t, s, testOwnerA, ids[0])
	assertPresent(t, s, testOwnerA, ids[1])
	assertPresent(t, s, testOwnerA, ids[2])
	assertPresent(t, s, testOwnerA, fourth.ID)

	listed, err := s.ListByOwner(testOwnerA)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("ListByOwner = %d tasks, want 3 (cap bounds the unbounded read)", len(listed))
	}
}

// TestCapEvictsTerminalKeepsNonTerminal: with a cap of 2, a create
// past the cap evicts the terminal task and keeps the live one.
func TestCapEvictsTerminalKeepsNonTerminal(t *testing.T) {
	s, err := OpenWithLimits(t.TempDir(), Limits{MaxTasksPerKey: 2})
	if err != nil {
		t.Fatalf("OpenWithLimits: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	terminal := mustCreate(t, s, testOwnerA)
	mustComplete(t, s, testOwnerA, terminal.ID)
	live := mustCreate(t, s, testOwnerA)

	third := mustCreate(t, s, testOwnerA)

	assertGone(t, s, testOwnerA, terminal.ID)
	assertPresent(t, s, testOwnerA, live.ID)
	assertPresent(t, s, testOwnerA, third.ID)
}

// TestCapRejectAllNonTerminal: with a cap of 2 and both tasks live,
// the 3rd create returns ErrTaskLimitExceeded naming the cap, and
// the first two are untouched.
func TestCapRejectAllNonTerminal(t *testing.T) {
	s, err := OpenWithLimits(t.TempDir(), Limits{MaxTasksPerKey: 2})
	if err != nil {
		t.Fatalf("OpenWithLimits: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	first := mustCreate(t, s, testOwnerA)
	second := mustCreate(t, s, testOwnerA)

	_, err = s.Create(testOwnerA, json.RawMessage(`{"role":"user","content":"third"}`))
	if !errors.Is(err, ErrTaskLimitExceeded) {
		t.Fatalf("Create past cap err = %v, want ErrTaskLimitExceeded", err)
	}
	if !strings.Contains(err.Error(), "2 tasks") {
		t.Fatalf("Create past cap err = %q, want the cap (2 tasks) named", err.Error())
	}

	assertPresent(t, s, testOwnerA, first.ID)
	assertPresent(t, s, testOwnerA, second.ID)
	listed, err := s.ListByOwner(testOwnerA)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("ListByOwner = %d tasks, want 2", len(listed))
	}
}

// TestCapConcurrentCreatesNeverExceedsCap: 20 racing creates
// against a cap of 5 leave exactly 5 tasks; the losers get
// ErrTaskLimitExceeded, never a raw lock error, and the count never
// exceeds the cap.
func TestCapConcurrentCreatesNeverExceedsCap(t *testing.T) {
	s, err := OpenWithLimits(t.TempDir(), Limits{MaxTasksPerKey: 5})
	if err != nil {
		t.Fatalf("OpenWithLimits: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const goroutines = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	created, rejected, other := 0, 0, 0
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Create(testOwnerA, json.RawMessage(`{"role":"user","content":"race"}`))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, ErrTaskLimitExceeded):
				rejected++
			default:
				other++
				t.Errorf("Create race err = %v, want nil or ErrTaskLimitExceeded", err)
			}
		}()
	}
	wg.Wait()
	if created != 5 {
		t.Fatalf("created = %d, want 5 (the cap)", created)
	}
	if rejected != goroutines-5 {
		t.Fatalf("rejected = %d, want %d", rejected, goroutines-5)
	}
	if other != 0 {
		t.Fatalf("other errors = %d, want 0", other)
	}
	listed, err := s.ListByOwner(testOwnerA)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(listed) != 5 {
		t.Fatalf("ListByOwner = %d tasks, want 5 (never above the cap)", len(listed))
	}
}
