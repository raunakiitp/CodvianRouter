package a2a

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testOwnerA = "owner-a"
	testOwnerB = "owner-b"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustService(t *testing.T, s *Store) *Service {
	t.Helper()
	svc, err := NewService(s)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func mustCreate(t *testing.T, s *Store, owner string) *Task {
	t.Helper()
	task, err := s.Create(owner, json.RawMessage(`{"role":"user","content":"hi"}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if task.State != StateSubmitted {
		t.Fatalf("new task state = %q, want %q", task.State, StateSubmitted)
	}
	return task
}

func eventCount(t *testing.T, s *Store, owner, id string) int {
	t.Helper()
	evs, err := s.Events(owner, id)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	return len(evs)
}

// --- Owner id ---------------------------------------------------------

func TestOwnerIDForKey(t *testing.T) {
	h := OwnerIDForKey("key-123")
	if len(h) != 64 {
		t.Fatalf("OwnerIDForKey length = %d, want 64 (full SHA-256 hex, not a prefix)", len(h))
	}
	sum := sha256.Sum256([]byte("key-123"))
	if h != hex.EncodeToString(sum[:]) {
		t.Fatalf("OwnerIDForKey = %q, want full SHA-256 hex %q", h, hex.EncodeToString(sum[:]))
	}
	// Deterministic, and distinct keys give distinct owners.
	if OwnerIDForKey("key-123") != h {
		t.Fatal("OwnerIDForKey not deterministic")
	}
	if OwnerIDForKey("key-456") == h {
		t.Fatal("different key ids gave the same owner id")
	}
	// Empty key id yields no owner: there is no ownerless access.
	if OwnerIDForKey("") != "" {
		t.Fatal("OwnerIDForKey(\"\") must be empty")
	}
}

func TestEmptyOwnerRejected(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Create("", json.RawMessage(`{}`)); !errors.Is(err, ErrEmptyOwner) {
		t.Fatalf("Create with empty owner: err = %v, want ErrEmptyOwner", err)
	}
	if _, err := s.Get("", "x"); !errors.Is(err, ErrEmptyOwner) {
		t.Fatalf("Get with empty owner: err = %v, want ErrEmptyOwner", err)
	}
	if _, err := s.ListByOwner(""); !errors.Is(err, ErrEmptyOwner) {
		t.Fatalf("ListByOwner with empty owner: err = %v, want ErrEmptyOwner", err)
	}
	if _, err := s.Transition("", "x", StateWorking, nil, ""); !errors.Is(err, ErrEmptyOwner) {
		t.Fatalf("Transition with empty owner: err = %v, want ErrEmptyOwner", err)
	}
	if _, err := s.Events("", "x"); !errors.Is(err, ErrEmptyOwner) {
		t.Fatalf("Events with empty owner: err = %v, want ErrEmptyOwner", err)
	}
	if _, err := s.Cancel("", "x"); !errors.Is(err, ErrEmptyOwner) {
		t.Fatalf("Cancel with empty owner: err = %v, want ErrEmptyOwner", err)
	}
}

// --- State transitions -------------------------------------------------

func TestStateTransitionLegality(t *testing.T) {
	// Full 5x5 table: 5 legal moves below, all 20 remaining pairs
	// illegal. Every pair is exercised, not a sample.
	legal := [][2]State{
		{StateSubmitted, StateWorking},
		{StateSubmitted, StateCancelled},
		{StateWorking, StateCompleted},
		{StateWorking, StateFailed},
		{StateWorking, StateCancelled},
	}
	for _, mv := range legal {
		s := openTestStore(t)
		task := mustCreate(t, s, testOwnerA)
		if mv[0] == StateWorking {
			if _, err := s.Transition(testOwnerA, task.ID, StateWorking, nil, ""); err != nil {
				t.Fatalf("setup %s -> working: %v", mv[0], err)
			}
		}
		if _, err := s.Transition(testOwnerA, task.ID, mv[1], nil, ""); err != nil {
			t.Fatalf("legal transition %s -> %s failed: %v", mv[0], mv[1], err)
		}
	}

	illegal := [][2]State{
		{StateSubmitted, StateCompleted},
		{StateSubmitted, StateFailed},
		{StateSubmitted, StateSubmitted},
		{StateWorking, StateWorking},
		{StateWorking, StateSubmitted},
		{StateCompleted, StateSubmitted},
		{StateCompleted, StateWorking},
		{StateCompleted, StateFailed},
		{StateCompleted, StateCompleted},
		{StateCompleted, StateCancelled},
		{StateFailed, StateSubmitted},
		{StateFailed, StateWorking},
		{StateFailed, StateFailed},
		{StateFailed, StateCompleted},
		{StateFailed, StateCancelled},
		{StateCancelled, StateSubmitted},
		{StateCancelled, StateWorking},
		{StateCancelled, StateFailed},
		{StateCancelled, StateCompleted},
		{StateCancelled, StateCancelled},
	}
	for _, mv := range illegal {
		s := openTestStore(t)
		task := mustCreate(t, s, testOwnerA)
		if mv[0] != StateSubmitted {
			// Drive the task to the desired starting state first.
			switch mv[0] {
			case StateWorking:
				_, _ = s.Transition(testOwnerA, task.ID, StateWorking, nil, "")
			case StateCompleted:
				_, _ = s.Transition(testOwnerA, task.ID, StateWorking, nil, "")
				_, _ = s.Transition(testOwnerA, task.ID, StateCompleted, json.RawMessage(`{}`), "")
			case StateFailed:
				_, _ = s.Transition(testOwnerA, task.ID, StateWorking, nil, "")
				_, _ = s.Transition(testOwnerA, task.ID, StateFailed, nil, "boom")
			case StateCancelled:
				_, _ = s.Transition(testOwnerA, task.ID, StateCancelled, nil, "")
			}
		}
		before := eventCount(t, s, testOwnerA, task.ID)
		_, err := s.Transition(testOwnerA, task.ID, mv[1], nil, "")
		if !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("illegal transition %s -> %s: err = %v, want ErrIllegalTransition", mv[0], mv[1], err)
		}
		// An illegal transition writes NO event and leaves the state alone.
		if got := eventCount(t, s, testOwnerA, task.ID); got != before {
			t.Fatalf("illegal transition %s -> %s wrote an event (%d -> %d)", mv[0], mv[1], before, got)
		}
		got, err := s.Get(testOwnerA, task.ID)
		if err != nil {
			t.Fatalf("Get after illegal transition: %v", err)
		}
		if got.State != mv[0] {
			t.Fatalf("state after illegal %s -> %s = %q, want %q", mv[0], mv[1], got.State, mv[0])
		}
	}
}

// --- Owner isolation ---------------------------------------------------

func TestOwnerIsolation(t *testing.T) {
	s := openTestStore(t)
	task := mustCreate(t, s, testOwnerA)

	// Owner B Get: not-found, exactly as if the task did not exist.
	if _, err := s.Get(testOwnerB, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner B Get of owner A's task: err = %v, want ErrNotFound", err)
	}
	// Owner B List: sees nothing of A's.
	list, err := s.ListByOwner(testOwnerB)
	if err != nil {
		t.Fatalf("owner B List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("owner B List returned %d tasks, want 0", len(list))
	}
	// Owner B Events / Cancel / Transition: not-found as well.
	if _, err := s.Events(testOwnerB, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner B Events: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Cancel(testOwnerB, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner B Cancel: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Transition(testOwnerB, task.ID, StateWorking, nil, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner B Transition: err = %v, want ErrNotFound", err)
	}
	// Owner A still sees it, untouched by B's attempts.
	got, err := s.Get(testOwnerA, task.ID)
	if err != nil {
		t.Fatalf("owner A Get after B's attempts: %v", err)
	}
	if got.State != StateSubmitted {
		t.Fatalf("owner A's task state = %q after B's attempts, want submitted", got.State)
	}
	// Unknown id is also ErrNotFound for either owner.
	if _, err := s.Get(testOwnerA, "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestNoOwnerlessPathByCodeSearch asserts, by reading this package's
// own source, that no exported Store/Service method lacks an owner
// parameter and that no list-all/get-all method exists. A future
// method that forgets the owner parameter fails this test.
func TestNoOwnerlessPathByCodeSearch(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "func (s *Store)") && !strings.HasPrefix(line, "func (s *Service)") {
				continue
			}
			// Skip unexported helpers and non-data methods.
			if strings.Contains(line, "func (s *Store) Close") || strings.Contains(line, "func (s *Store) Path") {
				continue
			}
			// CountsByState is the deliberate, documented
			// exception: an operator-only aggregate returning
			// five totals and no owner/task identifiers,
			// consumed only by the MANAGEMENT-tier, admin-scoped
			// GET /api/a2a/status endpoint. It takes no owner
			// because it must total across all owners; the
			// method comment in store.go states the isolation
			// argument. Any other owner-less method still fails.
			if strings.Contains(line, "func (s *Store) CountsByState") {
				continue
			}
			// PruneExpired / PruneExpiredBefore are the second
			// deliberate exception: retention is a SQL predicate
			// over (state, updated_at) across all owners, not a
			// read-then-delete loop and not a per-owner read —
			// there is no owner to scope it to, and no task
			// content, id, or owner is ever returned. The
			// method comments in store.go state the argument.
			if strings.Contains(line, "func (s *Store) PruneExpired") ||
				strings.Contains(line, "func (s *Store) PruneExpiredBefore") {
				continue
			}
			// Service.Store is the third deliberate exception:
			// it returns the backing store itself — no task
			// data, no owner, no id — so there is nothing to
			// scope it to. Its only consumer is the
			// MANAGEMENT-tier, admin-scoped GET /api/a2a/status
			// opener, which reuses the lazily-opened service's
			// store instead of opening a second one; every
			// data method on the returned store is still
			// owner-scoped. The method comment in service.go
			// states the argument.
			if strings.Contains(line, "func (s *Service) Store()") {
				continue
			}
			if !strings.Contains(line, "owner") || !strings.Contains(line, "string") {
				t.Errorf("%s: method %q has no owner string parameter — every data method must be owner-scoped", name, line)
			}
			lower := strings.ToLower(line)
			if strings.Contains(lower, "listall") || strings.Contains(lower, "getall") || strings.Contains(lower, "alltasks") {
				t.Errorf("%s: list-all style method found: %q", name, line)
			}
		}
	}
}

// --- Event log ----------------------------------------------------------

func TestEventLogAppendOnly(t *testing.T) {
	s := openTestStore(t)
	task := mustCreate(t, s, testOwnerA)
	// Creation writes exactly one event: "" -> submitted.
	evs, err := s.Events(testOwnerA, task.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 1 || evs[0].FromState != "" || evs[0].ToState != StateSubmitted {
		t.Fatalf("creation events = %+v, want exactly one (\"\" -> submitted)", evs)
	}
	// Two transitions add exactly two more events, in order.
	if _, err := s.Transition(testOwnerA, task.ID, StateWorking, nil, ""); err != nil {
		t.Fatalf("to working: %v", err)
	}
	if _, err := s.Transition(testOwnerA, task.ID, StateCompleted, json.RawMessage(`{"ok":true}`), ""); err != nil {
		t.Fatalf("to completed: %v", err)
	}
	evs, err = s.Events(testOwnerA, task.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	want := [][2]State{
		{"", StateSubmitted},
		{StateSubmitted, StateWorking},
		{StateWorking, StateCompleted},
	}
	if len(evs) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(evs), len(want), evs)
	}
	for i, w := range want {
		if evs[i].FromState != w[0] || evs[i].ToState != w[1] {
			t.Fatalf("event %d = %q -> %q, want %q -> %q", i, evs[i].FromState, evs[i].ToState, w[0], w[1])
		}
		if i > 0 && evs[i].ID <= evs[i-1].ID {
			t.Fatalf("event ids not increasing: %d then %d", evs[i-1].ID, evs[i].ID)
		}
		if evs[i].TaskID != task.ID {
			t.Fatalf("event %d task id = %q, want %q", i, evs[i].TaskID, task.ID)
		}
	}
}

// --- Persistence ---------------------------------------------------------

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	a := mustCreate(t, s1, testOwnerA)
	b := mustCreate(t, s1, testOwnerB)
	if _, err := s1.Transition(testOwnerA, a.ID, StateWorking, nil, ""); err != nil {
		t.Fatalf("transition: %v", err)
	}
	// The store lives at <data-dir>/a2a/tasks.db.
	if _, err := os.Stat(filepath.Join(dir, "a2a", "tasks.db")); err != nil {
		t.Fatalf("tasks.db not at <data-dir>/a2a/tasks.db: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	gotA, err := s2.Get(testOwnerA, a.ID)
	if err != nil {
		t.Fatalf("Get A after reopen: %v", err)
	}
	if gotA.State != StateWorking {
		t.Fatalf("task A state after reopen = %q, want working", gotA.State)
	}
	if gotA.Message == nil || string(gotA.Message) == "" {
		t.Fatal("task A message lost across reopen")
	}
	gotB, err := s2.Get(testOwnerB, b.ID)
	if err != nil {
		t.Fatalf("Get B after reopen: %v", err)
	}
	if gotB.State != StateSubmitted {
		t.Fatalf("task B state after reopen = %q, want submitted", gotB.State)
	}
	list, err := s2.ListByOwner(testOwnerA)
	if err != nil {
		t.Fatalf("List A after reopen: %v", err)
	}
	if len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("List A after reopen = %d tasks, want exactly A's", len(list))
	}
	// Events survive the reopen too.
	if n := eventCount(t, s2, testOwnerA, a.ID); n != 2 {
		t.Fatalf("events after reopen = %d, want 2 (create + working)", n)
	}
}

// --- Cancel ---------------------------------------------------------------

func TestCancelFromSubmittedAndWorking(t *testing.T) {
	s := openTestStore(t)
	// Cancel from Submitted.
	a := mustCreate(t, s, testOwnerA)
	cancelled, err := s.Cancel(testOwnerA, a.ID)
	if err != nil {
		t.Fatalf("Cancel from submitted: %v", err)
	}
	if cancelled.State != StateCancelled {
		t.Fatalf("state after cancel = %q, want cancelled", cancelled.State)
	}
	// Cancel from Working.
	b := mustCreate(t, s, testOwnerA)
	if _, err := s.Transition(testOwnerA, b.ID, StateWorking, nil, ""); err != nil {
		t.Fatalf("to working: %v", err)
	}
	cancelled, err = s.Cancel(testOwnerA, b.ID)
	if err != nil {
		t.Fatalf("Cancel from working: %v", err)
	}
	if cancelled.State != StateCancelled {
		t.Fatalf("state after cancel = %q, want cancelled", cancelled.State)
	}
	// Cancel of a terminal task is illegal and writes no event.
	before := eventCount(t, s, testOwnerA, b.ID)
	if _, err := s.Cancel(testOwnerA, b.ID); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("second Cancel: err = %v, want ErrIllegalTransition", err)
	}
	if got := eventCount(t, s, testOwnerA, b.ID); got != before {
		t.Fatalf("illegal cancel wrote an event (%d -> %d)", before, got)
	}
}

// --- Service / RunTask ------------------------------------------------------

func TestRunTaskHappyPath(t *testing.T) {
	s := openTestStore(t)
	svc := mustService(t, s)
	task, err := svc.CreateTask(testOwnerA, json.RawMessage(`{"prompt":"hello"}`))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	var sawOwner, sawMsg string
	proc := func(ctx context.Context, owner string, message json.RawMessage) (json.RawMessage, error) {
		sawOwner, sawMsg = owner, string(message)
		return json.RawMessage(`{"reply":"world"}`), nil
	}
	done, err := svc.RunTask(context.Background(), testOwnerA, task.ID, proc)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if done.State != StateCompleted {
		t.Fatalf("RunTask state = %q, want completed", done.State)
	}
	if string(done.Result) != `{"reply":"world"}` {
		t.Fatalf("RunTask result = %s, want processor's reply", done.Result)
	}
	if done.Error != "" {
		t.Fatalf("RunTask error = %q, want empty", done.Error)
	}
	if sawOwner != testOwnerA || sawMsg != `{"prompt":"hello"}` {
		t.Fatalf("processor got owner=%q msg=%q, want %q / original message", sawOwner, sawMsg, testOwnerA)
	}
	// Full event trail: create, working, completed.
	evs, err := s.Events(testOwnerA, task.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 3 || evs[2].ToState != StateCompleted {
		t.Fatalf("events after RunTask = %+v, want 3 ending in completed", evs)
	}
	// A completed task cannot run again.
	if _, err := svc.RunTask(context.Background(), testOwnerA, task.ID, proc); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("second RunTask: err = %v, want ErrIllegalTransition", err)
	}
}

func TestRunTaskFailurePath(t *testing.T) {
	s := openTestStore(t)
	svc := mustService(t, s)
	task, err := svc.CreateTask(testOwnerA, json.RawMessage(`{"prompt":"hello"}`))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	procErr := errors.New("router: no provider available")
	proc := func(ctx context.Context, owner string, message json.RawMessage) (json.RawMessage, error) {
		return nil, procErr
	}
	done, err := svc.RunTask(context.Background(), testOwnerA, task.ID, proc)
	if err != nil {
		t.Fatalf("RunTask with failing processor returned error %v; want the terminal task with nil error", err)
	}
	// Failed with the REAL error — never a fabricated success.
	if done.State != StateFailed {
		t.Fatalf("RunTask state = %q, want failed", done.State)
	}
	if done.Error != procErr.Error() {
		t.Fatalf("RunTask error = %q, want the real processor error %q", done.Error, procErr.Error())
	}
	if len(done.Result) != 0 {
		t.Fatalf("RunTask result = %s on failure, want empty (no fabricated result)", done.Result)
	}
	evs, err := s.Events(testOwnerA, task.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 3 || evs[2].ToState != StateFailed {
		t.Fatalf("events after failed RunTask = %+v, want 3 ending in failed", evs)
	}
}

func TestRunTaskNilProcessor(t *testing.T) {
	s := openTestStore(t)
	svc := mustService(t, s)
	task := mustCreate(t, s, testOwnerA)
	if _, err := svc.RunTask(context.Background(), testOwnerA, task.ID, nil); err == nil {
		t.Fatal("RunTask with nil processor accepted, want an error")
	}
	// Nil processor runs nothing: task is still Submitted.
	got, err := s.Get(testOwnerA, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateSubmitted {
		t.Fatalf("state after nil-processor RunTask = %q, want submitted", got.State)
	}
}

func TestRunTaskWrongOwner(t *testing.T) {
	s := openTestStore(t)
	svc := mustService(t, s)
	task, err := svc.CreateTask(testOwnerA, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	called := false
	proc := func(ctx context.Context, owner string, message json.RawMessage) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`{}`), nil
	}
	if _, err := svc.RunTask(context.Background(), testOwnerB, task.ID, proc); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RunTask wrong owner: err = %v, want ErrNotFound", err)
	}
	if called {
		t.Fatal("processor ran for the wrong owner")
	}
}

func TestServiceListIsOwnerScoped(t *testing.T) {
	s := openTestStore(t)
	svc := mustService(t, s)
	if _, err := svc.CreateTask(testOwnerA, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CreateTask A: %v", err)
	}
	if _, err := svc.CreateTask(testOwnerA, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CreateTask A2: %v", err)
	}
	if _, err := svc.CreateTask(testOwnerB, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CreateTask B: %v", err)
	}
	listA, err := svc.ListTasks(testOwnerA)
	if err != nil {
		t.Fatalf("ListTasks A: %v", err)
	}
	if len(listA) != 2 {
		t.Fatalf("ListTasks A = %d, want 2", len(listA))
	}
	listB, err := svc.ListTasks(testOwnerB)
	if err != nil {
		t.Fatalf("ListTasks B: %v", err)
	}
	if len(listB) != 1 {
		t.Fatalf("ListTasks B = %d, want 1", len(listB))
	}
}

// --- Review-nit fixes (W3-A) -------------------------------------------

func TestNewServiceNilStore(t *testing.T) {
	if _, err := NewService(nil); err == nil {
		t.Fatal("NewService(nil) accepted, want an error")
	}
	s := openTestStore(t)
	if _, err := NewService(s); err != nil {
		t.Fatalf("NewService(valid store): err = %v, want nil", err)
	}
}

// TestTransitionResultErrorPlacement proves the Transition rule:
// result is stored only on Completed, errStr only on Failed. A
// misplaced payload is rejected with ErrIllegalTransition and the
// task is left exactly as it was — no state change, no new event.
func TestTransitionResultErrorPlacement(t *testing.T) {
	s := openTestStore(t)
	task := mustCreate(t, s, testOwnerA)

	reject := func(target State, result json.RawMessage, errStr string, label string) {
		t.Helper()
		beforeState := func() State {
			got, err := s.Get(testOwnerA, task.ID)
			if err != nil {
				t.Fatalf("Get before %s: %v", label, err)
			}
			return got.State
		}()
		beforeEvents := eventCount(t, s, testOwnerA, task.ID)
		if _, err := s.Transition(testOwnerA, task.ID, target, result, errStr); !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("%s: err = %v, want ErrIllegalTransition", label, err)
		}
		got, err := s.Get(testOwnerA, task.ID)
		if err != nil {
			t.Fatalf("Get after %s: %v", label, err)
		}
		if got.State != beforeState {
			t.Fatalf("%s changed state %q -> %q, want unchanged", label, beforeState, got.State)
		}
		if n := eventCount(t, s, testOwnerA, task.ID); n != beforeEvents {
			t.Fatalf("%s wrote an event (%d -> %d), want none", label, beforeEvents, n)
		}
	}

	// From Submitted: result on Working, error on Working.
	reject(StateWorking, json.RawMessage(`{"x":1}`), "", "result on Working")
	reject(StateWorking, nil, "boom", "error on Working")
	// Move to Working (clean) for the remaining cases.
	if _, err := s.Transition(testOwnerA, task.ID, StateWorking, nil, ""); err != nil {
		t.Fatalf("to working: %v", err)
	}
	reject(StateCompleted, nil, "boom", "error on Completed")
	reject(StateFailed, json.RawMessage(`{"x":1}`), "", "result on Failed")
	reject(StateCancelled, json.RawMessage(`{"x":1}`), "", "result on Cancelled")
	reject(StateCancelled, nil, "boom", "error on Cancelled")

	// Positive controls: result on Completed and error on Failed land.
	done, err := s.Transition(testOwnerA, task.ID, StateCompleted, json.RawMessage(`{"ok":true}`), "")
	if err != nil {
		t.Fatalf("result on Completed: %v", err)
	}
	if string(done.Result) != `{"ok":true}` {
		t.Fatalf("result on Completed stored %s, want {\"ok\":true}", done.Result)
	}
	b := mustCreate(t, s, testOwnerA)
	if _, err := s.Transition(testOwnerA, b.ID, StateWorking, nil, ""); err != nil {
		t.Fatalf("b to working: %v", err)
	}
	failed, err := s.Transition(testOwnerA, b.ID, StateFailed, nil, "boom")
	if err != nil {
		t.Fatalf("error on Failed: %v", err)
	}
	if failed.Error != "boom" {
		t.Fatalf("error on Failed stored %q, want %q", failed.Error, "boom")
	}
}

// TestForeignKeysEnforced proves the schema's REFERENCES
// a2a_tasks(id) is actually enforced: the pragma is on for the
// store's connection, and an event for a nonexistent task fails.
func TestForeignKeysEnforced(t *testing.T) {
	s := openTestStore(t)
	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, want 1 (ON)", fk)
	}
	_, err := s.db.Exec(
		`INSERT INTO a2a_task_events (task_id, from_state, to_state, at) VALUES (?, '', 'submitted', ?)`,
		"no-such-task", time.Now().UTC().Format(time.RFC3339Nano))
	if err == nil {
		t.Fatal("event insert for a nonexistent task succeeded, want a FOREIGN KEY failure")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("event insert error = %v, want a FOREIGN KEY constraint failure", err)
	}
}

// TestStorePermissions asserts tasks.db is 0600 and the a2a/
// directory is 0700, both on a fresh open and when reopening over
// a pre-existing looser directory and file.
func TestStorePermissions(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Defer the close: the explicit Close below is skipped when an
	// assertion above it fails, and an open tasks.db blocks the
	// TempDir cleanup on Windows. sql.DB.Close is idempotent, so the
	// deferred call after the explicit one is a no-op.
	defer s.Close()
	assertPerms(t, dir)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Loosen both, then reopen: Open must tighten them again.
	if err := os.Chmod(filepath.Join(dir, "a2a"), 0o755); err != nil {
		t.Fatalf("loosen dir: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "a2a", "tasks.db"), 0o644); err != nil {
		t.Fatalf("loosen db file: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	assertPerms(t, dir)
}

func assertPerms(t *testing.T, dir string) {
	t.Helper()
	// Windows has no Unix permission bits: os.Chmod only toggles the
	// read-only attribute and os.Stat reports 0777 for directories
	// and 0666 for files, so exact 0700/0600 modes are unrepresentable
	// there — the tighten calls in Open still run and must not error,
	// but the mode assertions are Unix-only.
	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(filepath.Join(dir, "a2a"))
	if err != nil {
		t.Fatalf("stat a2a dir: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("a2a dir mode = %04o, want 0700", fi.Mode().Perm())
	}
	fi, err = os.Stat(filepath.Join(dir, "a2a", "tasks.db"))
	if err != nil {
		t.Fatalf("stat tasks.db: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("tasks.db mode = %04o, want 0600", fi.Mode().Perm())
	}
}

// TestTransitionConcurrentRace races two Store handles on the same
// file through the same transition (Submitted -> Working). Exactly
// one must win; the loser must get a typed error
// (ErrIllegalTransition once it re-reads the winner's new state, or
// ErrBusy if the lock never frees) — never the driver's raw
// "database is locked" string — and the transition must apply
// exactly once (one event, state Working).
func TestTransitionConcurrentRace(t *testing.T) {
	for iter := 0; iter < 3; iter++ {
		dir := t.TempDir()
		s1, err := Open(dir)
		if err != nil {
			t.Fatalf("iter %d Open s1: %v", iter, err)
		}
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("iter %d Open s2: %v", iter, err)
		}
		task := mustCreate(t, s1, testOwnerA)
		stores := []*Store{s1, s2}
		start := make(chan struct{})
		results := make([]error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, err := stores[i].Transition(testOwnerA, task.ID, StateWorking, nil, "")
				results[i] = err
			}(i)
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, err := range results {
			if err == nil {
				wins++
				continue
			}
			if !errors.Is(err, ErrIllegalTransition) && !errors.Is(err, ErrBusy) {
				t.Fatalf("iter %d loser error = %v, want ErrIllegalTransition or ErrBusy", iter, err)
			}
			if strings.Contains(err.Error(), "database is locked") {
				t.Fatalf("iter %d loser error is a raw lock error: %v", iter, err)
			}
		}
		if wins != 1 {
			t.Fatalf("iter %d winners = %d, want exactly 1 (errs %v)", iter, wins, results)
		}
		got, err := s1.Get(testOwnerA, task.ID)
		if err != nil {
			t.Fatalf("iter %d Get: %v", iter, err)
		}
		if got.State != StateWorking {
			t.Fatalf("iter %d final state = %q, want working", iter, got.State)
		}
		if n := eventCount(t, s1, testOwnerA, task.ID); n != 2 {
			t.Fatalf("iter %d events = %d, want 2 (create + one working transition, no double-apply)", iter, n)
		}
		_ = s1.Close()
		_ = s2.Close()
	}
}

// TestCountsByStateTotalsAcrossOwners proves CountsByState returns
// only aggregate totals across all owners: two owners' tasks are
// summed per state, every one of the five states is present (even
// at zero), and the returned map's keys are exactly the five state
// names — no owner id, task id, or any other identifier appears.
// If the method were removed or returned per-owner data, this
// fails: the handler's counts depend on it.
func TestCountsByStateTotalsAcrossOwners(t *testing.T) {
	s := openTestStore(t)
	// Empty store: all five states present, all zero.
	empty, err := s.CountsByState()
	if err != nil {
		t.Fatalf("CountsByState on empty store: %v", err)
	}
	if len(empty) != 5 {
		t.Fatalf("empty counts has %d keys, want 5 (one per state)", len(empty))
	}
	for _, st := range []State{StateSubmitted, StateWorking, StateCompleted, StateFailed, StateCancelled} {
		if empty[st] != 0 {
			t.Fatalf("empty counts[%q] = %d, want 0", st, empty[st])
		}
	}

	// Owner A: 2 submitted, 1 working, 1 completed, 1 failed.
	mustCreate(t, s, testOwnerA)
	mustCreate(t, s, testOwnerA)
	working, err := s.Create(testOwnerA, json.RawMessage(`{"role":"user","content":"hi"}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Transition(testOwnerA, working.ID, StateWorking, nil, ""); err != nil {
		t.Fatalf("Transition to working: %v", err)
	}
	done, err := s.Create(testOwnerA, json.RawMessage(`{"role":"user","content":"hi"}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Transition(testOwnerA, done.ID, StateWorking, nil, ""); err != nil {
		t.Fatalf("Transition to working: %v", err)
	}
	if _, err := s.Transition(testOwnerA, done.ID, StateCompleted, json.RawMessage(`{}`), ""); err != nil {
		t.Fatalf("Transition to completed: %v", err)
	}
	failed, err := s.Create(testOwnerA, json.RawMessage(`{"role":"user","content":"hi"}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Transition(testOwnerA, failed.ID, StateWorking, nil, ""); err != nil {
		t.Fatalf("Transition to working: %v", err)
	}
	if _, err := s.Transition(testOwnerA, failed.ID, StateFailed, nil, "boom"); err != nil {
		t.Fatalf("Transition to failed: %v", err)
	}
	// Owner B: 1 submitted, 1 cancelled — totals must include B.
	mustCreate(t, s, testOwnerB)
	cancelled, err := s.Create(testOwnerB, json.RawMessage(`{"role":"user","content":"hi"}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Transition(testOwnerB, cancelled.ID, StateCancelled, nil, ""); err != nil {
		t.Fatalf("Transition to cancelled: %v", err)
	}

	counts, err := s.CountsByState()
	if err != nil {
		t.Fatalf("CountsByState: %v", err)
	}
	want := map[State]int{
		StateSubmitted: 3, StateWorking: 1, StateCompleted: 1,
		StateFailed: 1, StateCancelled: 1,
	}
	if len(counts) != len(want) {
		t.Fatalf("counts has %d keys %v, want exactly the 5 states", len(counts), counts)
	}
	for st, n := range want {
		if counts[st] != n {
			t.Errorf("counts[%q] = %d, want %d (full map %v)", st, counts[st], n, counts)
		}
	}
	// Assert by construction: keys are only the five state names,
	// so no owner or task identifier can be present in the result.
	for k := range counts {
		switch k {
		case StateSubmitted, StateWorking, StateCompleted, StateFailed, StateCancelled:
		default:
			t.Errorf("counts has unexpected key %q (not a state name)", k)
		}
		if string(k) == testOwnerA || string(k) == testOwnerB {
			t.Errorf("counts key %q is an owner id", k)
		}
	}
}

// TestRunTaskCancelledDuringProcessorReturnsCancelled: a cancel
// that lands while the processor runs wins over the completion —
// RunTask returns the Cancelled task with a nil error, never an
// "a2a: mark completed: illegal state transition" error, and the
// processor's result is not stored.
func TestRunTaskCancelledDuringProcessorReturnsCancelled(t *testing.T) {
	s := openTestStore(t)
	svc := mustService(t, s)
	task := mustCreate(t, s, testOwnerA)
	proc := func(ctx context.Context, owner string, message json.RawMessage) (json.RawMessage, error) {
		if _, err := svc.CancelTask(owner, task.ID); err != nil {
			t.Errorf("CancelTask inside processor: %v", err)
		}
		return json.RawMessage(`{"reply":"too late"}`), nil
	}
	done, err := svc.RunTask(context.Background(), testOwnerA, task.ID, proc)
	if err != nil {
		t.Fatalf("RunTask with mid-flight cancel returned error %v; want the Cancelled task with nil error", err)
	}
	if done.State != StateCancelled {
		t.Fatalf("RunTask state = %q, want cancelled", done.State)
	}
	if len(done.Result) != 0 {
		t.Fatalf("RunTask result = %s on a cancelled task, want empty (result must not be stored)", done.Result)
	}
	if evs, err := s.Events(testOwnerA, task.ID); err != nil {
		t.Fatalf("Events: %v", err)
	} else if len(evs) != 3 || evs[2].ToState != StateCancelled {
		t.Fatalf("events after cancelled RunTask = %+v, want 3 ending in cancelled", evs)
	}
}

// TestRunTaskCancelledDuringFailingProcessorReturnsCancelled: the
// same cancellation-wins rule on the failure path — a processor
// error on a task cancelled mid-flight must not surface as an
// "a2a: mark failed: illegal state transition" error either.
func TestRunTaskCancelledDuringFailingProcessorReturnsCancelled(t *testing.T) {
	s := openTestStore(t)
	svc := mustService(t, s)
	task := mustCreate(t, s, testOwnerA)
	procErr := errors.New("router: context canceled")
	proc := func(ctx context.Context, owner string, message json.RawMessage) (json.RawMessage, error) {
		if _, err := svc.CancelTask(owner, task.ID); err != nil {
			t.Errorf("CancelTask inside processor: %v", err)
		}
		return nil, procErr
	}
	done, err := svc.RunTask(context.Background(), testOwnerA, task.ID, proc)
	if err != nil {
		t.Fatalf("RunTask with mid-flight cancel returned error %v; want the Cancelled task with nil error", err)
	}
	if done.State != StateCancelled {
		t.Fatalf("RunTask state = %q, want cancelled", done.State)
	}
	if done.Error != "" {
		t.Fatalf("RunTask error = %q on a cancelled task, want empty (the processor error must not overwrite the cancellation)", done.Error)
	}
}
