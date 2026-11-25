package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AUD-REL-005 regression: a corrupt ledger must not make startup
// impossible. Open returns an error for every failure mode and the
// caller treated any error as fatal, so an unusable ledger.db required
// manual intervention with no diagnostic.
func TestAUD_REL_005CorruptLedgerIsQuarantinedAndRecreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	garbage := []byte("this is not a sqlite database at all, not even close")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}

	// The plain Open must fail on this file (the defect's precondition).
	if l, err := Open(path); err == nil {
		_ = l.Close()
		t.Fatal("Open succeeded on a garbage file: the fixture does not reproduce the defect")
	}

	var logged []string
	Logf = func(format string, args ...any) { logged = append(logged, format) }
	defer func() { Logf = nil }()

	l, quarantined, err := OpenWithRecovery(path)
	if err != nil {
		t.Fatalf("OpenWithRecovery: %v", err)
	}
	defer l.Close()
	if quarantined == "" {
		t.Fatal("no quarantine path reported for a corrupt ledger")
	}
	if len(logged) == 0 {
		t.Error("the quarantine was not reported to the log hook")
	}

	// The unusable file is PRESERVED byte-for-byte, never deleted.
	got, err := os.ReadFile(quarantined)
	if err != nil {
		t.Fatalf("quarantined file is missing: %v", err)
	}
	if string(got) != string(garbage) {
		t.Fatalf("quarantined content changed:\n got %q\nwant %q", got, garbage)
	}
	if !strings.Contains(quarantined, ".corrupt-") {
		t.Errorf("quarantine path %q does not name the corruption", quarantined)
	}

	// A fresh, WORKING ledger is in use.
	ctx := context.Background()
	if err := l.SetBudget(ctx, "key-1", 100, "monthly"); err != nil {
		t.Fatalf("fresh ledger unusable: %v", err)
	}
	b, err := l.GetBudget(ctx, "key-1")
	if err != nil || b.CapUSD != 100 {
		t.Fatalf("fresh ledger read = %+v, %v", b, err)
	}
}

// TestAUD_REL_005HealthyLedgerIsUntouched: recovery must never trigger on
// a good file.
func TestAUD_REL_005HealthyLedgerIsUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	l, quarantined, err := OpenWithRecovery(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetBudget(context.Background(), "k", 5, "monthly"); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if quarantined != "" {
		t.Fatalf("a healthy ledger was quarantined: %q", quarantined)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			t.Fatalf("unexpected quarantine artefact %q", e.Name())
		}
	}
	// Re-open succeeds and the data is still there.
	l2, q2, err := OpenWithRecovery(path)
	if err != nil || q2 != "" {
		t.Fatalf("re-open: err=%v quarantine=%q", err, q2)
	}
	defer l2.Close()
	if b, err := l2.GetBudget(context.Background(), "k"); err != nil || b.CapUSD != 5 {
		t.Fatalf("data lost across recovery open: %+v %v", b, err)
	}
}

// TestAUD_REL_005EnvironmentalFailureStaysFatal: when there is nothing on
// disk to quarantine, the error must surface (never a silent fresh ledger
// where an operator expected their data).
func TestAUD_REL_005EnvironmentalFailureStaysFatal(t *testing.T) {
	dir := t.TempDir()
	// A FILE where a directory is needed: MkdirAll fails, so no ledger
	// file exists and nothing may be quarantined.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "ledger.db")

	l, quarantined, err := OpenWithRecovery(path)
	if err == nil {
		_ = l.Close()
		t.Fatal("an environmental failure was reported as success")
	}
	if quarantined != "" {
		t.Fatalf("quarantined %q although no unusable file existed", quarantined)
	}
}

// TestAUD_REL_005IsCorruptClassifies separates "not a database" from an
// environmental error so callers can tell them apart.
func TestAUD_REL_005IsCorruptClassifies(t *testing.T) {
	if !IsCorrupt(errors.New("ledger: enable WAL: file is not a database (26)")) {
		t.Error("a 'file is not a database' error was not classified as corruption")
	}
	if !IsCorrupt(errors.New("database disk image is malformed")) {
		t.Error("a malformed-image error was not classified as corruption")
	}
	if IsCorrupt(errors.New("ledger: create directory: access is denied")) {
		t.Error("an environmental error was classified as corruption")
	}
	if IsCorrupt(nil) {
		t.Error("nil classified as corruption")
	}
}
