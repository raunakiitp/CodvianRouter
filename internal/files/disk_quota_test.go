package files

import (
	"bytes"
	"errors"
	"testing"
)

// TestDiskQuotaRejectsOverLimit verifies that CreateOwned returns
// ErrDiskQuota when a single upload would exceed the per-key quota.
func TestDiskQuotaRejectsOverLimit(t *testing.T) {
	st := New(t.TempDir())
	const overLimit = maxKeyDiskBytes + 1
	content := make([]byte, overLimit)
	_, err := st.CreateOwned("big.txt", "assistants", "key-1", bytes.NewReader(content))
	if err == nil {
		t.Fatal("expected ErrDiskQuota, got nil")
	}
	if !errors.Is(err, ErrDiskQuota) {
		t.Fatalf("expected ErrDiskQuota, got %v", err)
	}
}

// TestDiskQuotaAllowsUnderLimit verifies that an upload under the
// per-key quota succeeds and usage is tracked.
func TestDiskQuotaAllowsUnderLimit(t *testing.T) {
	st := New(t.TempDir())
	content := []byte("hello world")
	f, err := st.CreateOwned("small.txt", "assistants", "key-1", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("CreateOwned: %v", err)
	}
	if st.KeyUsage("key-1") != int64(len(content)) {
		t.Fatalf("KeyUsage(key-1) = %d, want %d", st.KeyUsage("key-1"), len(content))
	}
	if st.Usage() != int64(len(content)) {
		t.Fatalf("Usage() = %d, want %d", st.Usage(), len(content))
	}
	_ = f
}

// TestDiskQuotaFreedOnDelete verifies that deleting a file frees its
// bytes from the quota counters.
func TestDiskQuotaFreedOnDelete(t *testing.T) {
	st := New(t.TempDir())
	content := []byte("hello world")
	f, err := st.CreateOwned("small.txt", "assistants", "key-1", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("CreateOwned: %v", err)
	}
	if st.KeyUsage("key-1") != int64(len(content)) {
		t.Fatalf("KeyUsage before delete = %d, want %d", st.KeyUsage("key-1"), len(content))
	}
	ok, err := st.DeleteFor(f.ID, "key-1")
	if err != nil || !ok {
		t.Fatalf("DeleteFor: ok=%v err=%v", ok, err)
	}
	if st.KeyUsage("key-1") != 0 {
		t.Fatalf("KeyUsage after delete = %d, want 0", st.KeyUsage("key-1"))
	}
	if st.Usage() != 0 {
		t.Fatalf("Usage after delete = %d, want 0", st.Usage())
	}
}

// TestDiskQuotaTracksAggregate verifies that Usage() tracks the
// aggregate bytes across all keys.
func TestDiskQuotaTracksAggregate(t *testing.T) {
	st := New(t.TempDir())
	a := []byte("aaaa")
	b := []byte("bbbbbbbbbb")
	if _, err := st.CreateOwned("a.txt", "assistants", "key-1", bytes.NewReader(a)); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if _, err := st.CreateOwned("b.txt", "assistants", "key-2", bytes.NewReader(b)); err != nil {
		t.Fatalf("second upload: %v", err)
	}
	want := int64(len(a) + len(b))
	if st.Usage() != want {
		t.Fatalf("Usage() = %d, want %d", st.Usage(), want)
	}
	if st.KeyUsage("key-1") != int64(len(a)) {
		t.Fatalf("KeyUsage(key-1) = %d, want %d", st.KeyUsage("key-1"), len(a))
	}
	if st.KeyUsage("key-2") != int64(len(b)) {
		t.Fatalf("KeyUsage(key-2) = %d, want %d", st.KeyUsage("key-2"), len(b))
	}
}
