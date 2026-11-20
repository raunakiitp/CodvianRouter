// H4 + memory entry-count cap tests (M9 bug-hunt): the store's
// errors never carry a local path, validation failures are
// distinguishable as invalid input, and one caller is capped at
// MaxMemoryEntriesPerCaller entries with oldest-evicted rotation
// that never touches another caller's entries.
package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// TestStripPathError: *SyscallError unwraps to the bare errno,
// like stripCachePathError in cmd/codvian-router — plus the
// pre-existing *PathError / *LinkError behaviour as regression
// cover. A wrapped error (fmt.Errorf %w) unwraps the same way.
// The unwrap is recursive: a *PathError wrapping a
// *SyscallError (or vice versa) must still collapse to the bare
// errno — before the fix this returned the *SyscallError
// ("open: no such file or directory"), not syscall.ENOENT.
func TestStripPathError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"syscall error", &os.SyscallError{Syscall: "open", Err: syscall.ENOENT}, syscall.ENOENT},
		{"path error", &os.PathError{Op: "stat", Path: "/secret/data/memory.jsonl", Err: syscall.EACCES}, syscall.EACCES},
		{"link error", &os.LinkError{Op: "rename", Old: "/secret/a", New: "/secret/b", Err: syscall.EXDEV}, syscall.EXDEV},
		{"wrapped syscall error", fmt.Errorf("wrap: %w", &os.SyscallError{Syscall: "read", Err: syscall.EIO}), syscall.EIO},
		{"path error wrapping syscall error", &os.PathError{Op: "open", Path: "/secret/data/memory.jsonl", Err: &os.SyscallError{Syscall: "open", Err: syscall.ENOENT}}, syscall.ENOENT},
		{"link error wrapping syscall error", &os.LinkError{Op: "rename", Old: "/secret/a", New: "/secret/b", Err: &os.SyscallError{Syscall: "rename", Err: syscall.EXDEV}}, syscall.EXDEV},
		{"syscall error wrapping path error", &os.SyscallError{Syscall: "open", Err: &os.PathError{Op: "open", Path: "/secret/x", Err: syscall.EACCES}}, syscall.EACCES},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripPathError(tc.err); got != tc.want {
				t.Errorf("stripPathError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	// A plain error passes through untouched.
	if err := errors.New("boom"); stripPathError(err) != err {
		t.Error("stripPathError(plain error) did not pass the error through untouched")
	}
}

func TestMemoryStoreDefaultCap(t *testing.T) {
	st := NewMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	if got := st.Cap(); got != MaxMemoryEntriesPerCaller {
		t.Errorf("Cap() = %d, want MaxMemoryEntriesPerCaller %d", got, MaxMemoryEntriesPerCaller)
	}
	if MaxMemoryEntriesPerCaller != 10000 {
		t.Errorf("MaxMemoryEntriesPerCaller = %d, want 10000 (same as the audit log's MaxAuditEntries)", MaxMemoryEntriesPerCaller)
	}
}

func TestMemoryStoreValidationErrorsAreInvalidInput(t *testing.T) {
	st := NewMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	if _, err := st.Add("", "hello"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Add with empty caller: err = %v, want it to wrap ErrInvalidInput", err)
	}
	if _, err := st.Add("key-a", ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Add with empty text: err = %v, want it to wrap ErrInvalidInput", err)
	}
	if _, err := st.Add("key-a", strings.Repeat("x", MaxMemoryTextLen+1)); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Add over the length limit: err = %v, want it to wrap ErrInvalidInput", err)
	}
	if _, err := st.Search("key-a", ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Search with empty query: err = %v, want it to wrap ErrInvalidInput", err)
	}
	if _, err := st.Clear(""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Clear with empty caller: err = %v, want it to wrap ErrInvalidInput", err)
	}
}

func TestMemoryStoreIOErrorIsPathFree(t *testing.T) {
	dir := t.TempDir()
	// A regular file where the store's directory should be: every
	// filesystem operation fails with a *PathError carrying the
	// full path — the error a caller sees must not.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	st := NewMemoryStore(filepath.Join(blocker, "memory.jsonl"))
	if _, err := st.Add("key-a", "hello"); err == nil {
		t.Fatal("Add to an unusable store: err = nil, want a failure")
	} else {
		if strings.Contains(err.Error(), dir) {
			t.Errorf("Add error leaks the path: %q", err.Error())
		}
		// The underlying errno text differs by platform: Unix
		// reports ENOTDIR ("not a directory"); Windows has no
		// ENOTDIR and reports ERROR_PATH_NOT_FOUND ("cannot find
		// the path specified") when a path component is a file.
		wantErrno := "not a directory"
		if runtime.GOOS == "windows" {
			wantErrno = "cannot find the path specified"
		}
		if !strings.Contains(err.Error(), wantErrno) {
			t.Errorf("Add error = %q, want the underlying errno (%s) preserved", err.Error(), wantErrno)
		}
		if errors.Is(err, ErrInvalidInput) {
			t.Errorf("Add store failure wraps ErrInvalidInput, want a plain store error: %v", err)
		}
	}
	if _, err := st.Search("key-a", "hello"); err == nil {
		t.Fatal("Search on an unusable store: err = nil, want a failure")
	} else if strings.Contains(err.Error(), dir) {
		t.Errorf("Search error leaks the path: %q", err.Error())
	}
	if _, err := st.Clear("key-a"); err == nil {
		t.Fatal("Clear on an unusable store: err = nil, want a failure")
	} else if strings.Contains(err.Error(), dir) {
		t.Errorf("Clear error leaks the path: %q", err.Error())
	}
}

func TestMemoryStoreCapEvictsOldestPerCaller(t *testing.T) {
	st := NewMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	// Shrink the cap for the test (same package, so the field is
	// reachable — the audit log's rotation test does the same):
	// 4 adds against a cap of 3 must drop exactly the oldest.
	st.maxEntries = 3
	for i := 1; i <= 4; i++ {
		if _, err := st.Add("key-a", fmt.Sprintf("cap-a-%d", i)); err != nil {
			t.Fatalf("Add cap-a-%d: %v", i, err)
		}
	}
	if _, err := st.Add("key-b", "cap-b-1"); err != nil {
		t.Fatalf("Add cap-b-1: %v", err)
	}
	mine, err := st.Search("key-a", "cap-a-")
	if err != nil {
		t.Fatalf("Search key-a: %v", err)
	}
	if len(mine) != 3 {
		t.Fatalf("key-a entries = %d, want 3 (cap)", len(mine))
	}
	seen := map[string]bool{}
	for _, e := range mine {
		seen[e.Text] = true
	}
	for _, want := range []string{"cap-a-2", "cap-a-3", "cap-a-4"} {
		if !seen[want] {
			t.Errorf("key-a is missing %q after rotation: %v", want, seen)
		}
	}
	if seen["cap-a-1"] {
		t.Errorf("oldest entry cap-a-1 survived rotation: %v", seen)
	}
	// The other caller is untouched by key-a's rotation.
	theirs, err := st.Search("key-b", "cap-b-")
	if err != nil {
		t.Fatalf("Search key-b: %v", err)
	}
	if len(theirs) != 1 || theirs[0].Text != "cap-b-1" {
		t.Errorf("key-b entries = %v, want [cap-b-1]", theirs)
	}
}

func TestMemoryStoreClearKeepsCapCountsConsistent(t *testing.T) {
	st := NewMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	st.maxEntries = 3
	for i := 1; i <= 3; i++ {
		if _, err := st.Add("key-a", fmt.Sprintf("round1-%d", i)); err != nil {
			t.Fatalf("Add round1-%d: %v", i, err)
		}
	}
	if _, err := st.Add("key-b", "keep-me"); err != nil {
		t.Fatalf("Add keep-me: %v", err)
	}
	if n, err := st.Clear("key-a"); err != nil || n != 3 {
		t.Fatalf("Clear key-a = %d, err %v, want 3, nil", n, err)
	}
	// The count resets with the clear: three fresh adds must all
	// survive — a stale count would evict one spuriously.
	for i := 1; i <= 3; i++ {
		if _, err := st.Add("key-a", fmt.Sprintf("round2-%d", i)); err != nil {
			t.Fatalf("Add round2-%d: %v", i, err)
		}
	}
	mine, err := st.Search("key-a", "round2-")
	if err != nil {
		t.Fatalf("Search key-a: %v", err)
	}
	if len(mine) != 3 {
		t.Errorf("key-a entries after clear + 3 adds = %d, want 3 (no spurious eviction)", len(mine))
	}
	theirs, err := st.Search("key-b", "keep-")
	if err != nil {
		t.Fatalf("Search key-b: %v", err)
	}
	if len(theirs) != 1 {
		t.Errorf("key-b entries = %d, want 1 (clear touched only key-a)", len(theirs))
	}
}

// TestMemoryStoreCap10000AddsEvictsOldestOnlyForCaller is the
// literal cap proof at the real default: adding entry 10,001
// for one caller evicts that caller's oldest entry and keeps
// exactly 10,000, while another caller's entries are untouched.
// The in-memory per-caller counter keeps this fast: only the
// 10,001st Add reads + rewrites the file, the first 10,000 are
// plain appends.
func TestMemoryStoreCap10000AddsEvictsOldestOnlyForCaller(t *testing.T) {
	st := NewMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	for i := 1; i <= MaxMemoryEntriesPerCaller+1; i++ {
		if _, err := st.Add("key-a", fmt.Sprintf("cap entry %05d", i)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if _, err := st.Add("key-b", "other caller one"); err != nil {
		t.Fatalf("Add key-b one: %v", err)
	}
	if _, err := st.Add("key-b", "other caller two"); err != nil {
		t.Fatalf("Add key-b two: %v", err)
	}
	mine, err := st.Search("key-a", "cap entry")
	if err != nil {
		t.Fatalf("Search key-a: %v", err)
	}
	if len(mine) != MaxMemoryEntriesPerCaller {
		t.Fatalf("key-a entries = %d, want %d after 10,001 adds", len(mine), MaxMemoryEntriesPerCaller)
	}
	seen := map[string]bool{}
	for _, e := range mine {
		seen[e.Text] = true
	}
	if seen["cap entry 00001"] {
		t.Errorf("oldest entry (cap entry 00001) survived the 10,001st add")
	}
	if !seen["cap entry 00002"] {
		t.Errorf("cap entry 00002 is missing: the wrong entry was evicted")
	}
	if !seen["cap entry 10001"] {
		t.Errorf("newest entry (cap entry 10001) is missing after its own add")
	}
	theirs, err := st.Search("key-b", "other caller")
	if err != nil {
		t.Fatalf("Search key-b: %v", err)
	}
	if len(theirs) != 2 {
		t.Errorf("key-b entries = %d, want 2 (key-a's rotation touched only key-a)", len(theirs))
	}
}
