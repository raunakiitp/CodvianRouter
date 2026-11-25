package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AUD-REL-005 review blocker: quarantine ONLY on positively identified
// corruption. A valid database must never be renamed merely because an
// operational failure (busy, locked, permissions, I/O, disk full, an
// unsupported pragma, a migration error) made Open fail.
//
// The first implementation quarantined after ANY open error, so a
// transient lock or a permission problem would have renamed a healthy
// ledger aside and silently started an empty one. These tests pin the
// safe behaviour for each failure class.

// p1305ForceOpenError points openLedger at a forced failure for the
// duration of a test, so failure classes that cannot be produced
// portably on this host (migration errors, I/O errors) are still
// exercised end to end.
func p1305ForceOpenError(t *testing.T, err error) {
	t.Helper()
	orig := openLedger
	openLedger = func(string) (*Ledger, error) { return nil, err }
	t.Cleanup(func() { openLedger = orig })
}

// p1305HealthyLedger creates a valid ledger with data at path.
func p1305HealthyLedger(t *testing.T, path string) {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	if err := l.SetBudget(context.Background(), "key-1", 42, "monthly"); err != nil {
		t.Fatalf("seed budget: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close seed ledger: %v", err)
	}
}

// TestAUD_REL_005OperationalFailuresNeverQuarantine covers the failure
// classes the review named: each must return the error and leave the
// database, and its sidecars, exactly where they were.
func TestAUD_REL_005OperationalFailuresNeverQuarantine(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"sqlite_busy", errors.New("ledger: enable WAL: database is locked (5) (SQLITE_BUSY)")},
		{"sqlite_locked", errors.New("ledger: schema: database table is locked (6) (SQLITE_LOCKED)")},
		{"permission", errors.New("ledger: open: unable to open database file: access is denied")},
		{"io_error", errors.New("ledger: enable WAL: disk I/O error (10)")},
		{"disk_full", errors.New("ledger: insert: database or disk is full (13)")},
		{"unsupported_pragma", errors.New("ledger: pragma: unsupported pragma busy_timeout")},
		{"migration", errors.New("ledger: migration 12 (add team_id): no such column: team_id")},
		{"too_many_open", errors.New("ledger: open: unable to open database file: too many open files")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ledger.db")
			p1305HealthyLedger(t, path)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			p1305ForceOpenError(t, tc.err)

			l, quarantined, err := OpenWithRecovery(path)
			if err == nil {
				_ = l.Close()
				t.Fatal("an operational failure was reported as success")
			}
			if quarantined != "" {
				t.Fatalf("quarantined %q for an operational failure (%s): a valid database must never be renamed", quarantined, tc.name)
			}
			if !errors.Is(err, tc.err) && err.Error() != tc.err.Error() {
				t.Errorf("error not surfaced to the caller: got %v, want %v", err, tc.err)
			}
			// The database is untouched, byte for byte.
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("the ledger was moved or removed: %v", err)
			}
			if string(after) != string(before) {
				t.Fatal("the ledger content changed")
			}
			// No quarantine artefact exists.
			for _, e := range p1305Entries(t, dir) {
				if IsQuarantinePath(e) {
					t.Fatalf("unexpected quarantine artefact %q", e)
				}
			}
			// And the data is still readable.
			l2, err := Open(path)
			if err != nil {
				t.Fatalf("valid ledger no longer opens: %v", err)
			}
			defer l2.Close()
			b, err := l2.GetBudget(context.Background(), "key-1")
			if err != nil || b.CapUSD != 42 {
				t.Fatalf("valid ledger data lost: %+v %v", b, err)
			}
		})
	}
}

// TestAUD_REL_005BusyDatabaseIsNotCorrupt holds a real write lock on a
// valid ledger and checks the two classifiers agree it is not corrupt.
func TestAUD_REL_005BusyDatabaseIsNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	p1305HealthyLedger(t, path)

	holder, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Skipf("cannot take an exclusive lock on this host: %v", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()

	if reason := ledgerCorruptReason(path); reason != "" {
		t.Fatalf("a locked (busy) database was classified as corrupt: %q", reason)
	}
	if IsCorrupt(errors.New("database is locked (5) (SQLITE_BUSY)")) {
		t.Fatal("SQLITE_BUSY classified as corruption")
	}
}

// TestAUD_REL_005CorruptionIsPositivelyIdentified exercises the real
// probe against a real corrupt file (not a fabricated error string).
func TestAUD_REL_005CorruptionIsPositivelyIdentified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	// A file that looks like a database but is not: valid header, garbage
	// pages, so quick_check itself reports the corruption.
	garbage := append([]byte("SQLite format 3\x00"), make([]byte, 4096)...)
	for i := 100; i < len(garbage); i += 7 {
		garbage[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	reason := ledgerCorruptReason(path)
	if reason == "" {
		t.Skip("this SQLite build accepted the fixture as readable; corruption detection exercised by the plain-garbage test")
	}
	if !strings.Contains(strings.ToLower(reason), "malformed") &&
		!strings.Contains(strings.ToLower(reason), "not a database") &&
		!strings.Contains(strings.ToLower(reason), "quick_check") {
		t.Fatalf("unexpected corruption reason: %q", reason)
	}
}

// TestAUD_REL_005CorruptLedgerIsQuarantinedEndToEnd keeps the original
// end-to-end behaviour (plain garbage -> quarantine -> fresh ledger
// works, original preserved byte-for-byte).
func TestAUD_REL_005CorruptLedgerIsQuarantinedEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	garbage := []byte("this is not a sqlite database at all, not even close")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	// The plain Open must fail on this file (the precondition).
	if l, err := Open(path); err == nil {
		_ = l.Close()
		t.Fatal("Open succeeded on a garbage file: the fixture does not reproduce the defect")
	}

	var logged []string
	Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	defer func() { Logf = nil }()

	l, quarantined, err := OpenWithRecovery(path)
	if err != nil {
		t.Fatalf("OpenWithRecovery: %v", err)
	}
	defer l.Close()
	if quarantined == "" {
		t.Fatal("no quarantine path reported for a genuinely corrupt ledger")
	}
	if len(logged) == 0 || !strings.Contains(logged[0], "corrupt") {
		t.Errorf("the quarantine was not reported: %v", logged)
	}
	got, err := os.ReadFile(quarantined)
	if err != nil {
		t.Fatalf("quarantined file is missing (forensic evidence lost): %v", err)
	}
	if string(got) != string(garbage) {
		t.Fatal("quarantined content changed")
	}
	if !IsQuarantinePath(quarantined) {
		t.Errorf("quarantine path %q is not recognisable", quarantined)
	}
	ctx := context.Background()
	if err := l.SetBudget(ctx, "key-1", 100, "monthly"); err != nil {
		t.Fatalf("fresh ledger unusable: %v", err)
	}
	// Reopening after recovery (a restart) is clean and keeps the data.
	_ = l.Close()
	l2, q2, err := OpenWithRecovery(path)
	if err != nil || q2 != "" {
		t.Fatalf("reopen after recovery: err=%v quarantine=%q", err, q2)
	}
	defer l2.Close()
	if b, err := l2.GetBudget(ctx, "key-1"); err != nil || b.CapUSD != 100 {
		t.Fatalf("data written after recovery was lost: %+v %v", b, err)
	}
}

// TestAUD_REL_005CorruptLedgerTakesItsSidecars: the quarantined file's
// WAL/SHM must move with it, so the fresh ledger cannot adopt a stale
// write-ahead log.
func TestAUD_REL_005CorruptLedgerTakesItsSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	if err := os.WriteFile(path, []byte("garbage, not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(path+suffix, []byte("stale-"+suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l, quarantined, err := OpenWithRecovery(path)
	if err != nil {
		t.Fatalf("OpenWithRecovery: %v", err)
	}
	defer l.Close()
	if quarantined == "" {
		t.Fatal("no quarantine for a genuinely corrupt ledger")
	}
	if _, err := os.Stat(quarantined); err != nil {
		t.Fatalf("the corrupt main file was not preserved: %v", err)
	}
	// The safety property is that the FRESH ledger must not adopt a
	// sidecar belonging to the corrupt file. Note the failed real Open
	// may already have discarded the stale -wal/-shm itself (SQLite does
	// that when it refuses a database), so a sidecar present now must
	// either be the quarantined file's or the fresh database's own — and
	// never the stale content this test wrote.
	for _, suffix := range []string{"-wal", "-shm"} {
		next, err := os.ReadFile(path + suffix)
		if err != nil {
			continue // gone: the failed open discarded it, which is safe
		}
		if strings.Contains(string(next), "stale-") {
			t.Fatalf("the fresh ledger adopted stale sidecar content at %s", path+suffix)
		}
	}
}

// TestAUD_REL_005NoQuarantineKeepsSidecars: an operational failure must
// leave the sidecars alone too.
func TestAUD_REL_005NoQuarantineKeepsSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	p1305HealthyLedger(t, path)
	// A sidecar present at failure time.
	if err := os.WriteFile(path+"-wal", []byte("in-flight-wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	p1305ForceOpenError(t, errors.New("ledger: enable WAL: database is locked (5) (SQLITE_BUSY)"))

	if _, quarantined, err := OpenWithRecovery(path); err == nil || quarantined != "" {
		t.Fatalf("busy failure: err=%v quarantine=%q, want the error and no quarantine", err, quarantined)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("the WAL sidecar was moved during an operational failure: %v", err)
	}
}

// TestAUD_REL_005RecoveryFailureIsReported: if the fresh ledger cannot be
// created either, the caller learns both the quarantine path and the
// second error.
func TestAUD_REL_005RecoveryFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	if err := os.WriteFile(path, []byte("garbage, not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	orig := openLedger
	openLedger = func(p string) (*Ledger, error) {
		calls++
		if calls == 1 {
			return orig(p) // the real corruption failure
		}
		return nil, errors.New("ledger: create directory: disk is full")
	}
	t.Cleanup(func() { openLedger = orig })

	l, quarantined, err := OpenWithRecovery(path)
	if l != nil {
		_ = l.Close()
		t.Fatal("a failed recovery returned a ledger")
	}
	if err == nil {
		t.Fatal("a failed recovery reported success")
	}
	if quarantined == "" {
		t.Fatal("the quarantine path was not reported")
	}
	if !strings.Contains(err.Error(), "disk is full") {
		t.Errorf("the second failure was not surfaced: %v", err)
	}
	if _, statErr := os.Stat(quarantined); statErr != nil {
		t.Errorf("the corrupt file was not preserved: %v", statErr)
	}
}

// TestAUD_REL_005HealthyLedgerNeverQuarantined: a good file is never touched.
func TestAUD_REL_005HealthyLedgerNeverQuarantined(t *testing.T) {
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
	for _, e := range p1305Entries(t, dir) {
		if IsQuarantinePath(e) {
			t.Fatalf("unexpected quarantine artefact %q", e)
		}
	}
}

// TestAUD_REL_005DirectoryInPlaceOfLedger: an unopenable path that is not
// a database file at all must not be renamed away either.
func TestAUD_REL_005DirectoryInPlaceOfLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, quarantined, err := OpenWithRecovery(path); err == nil || quarantined != "" {
		t.Fatalf("err=%v quarantine=%q, want the error and no quarantine", err, quarantined)
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		t.Fatalf("the directory was moved or removed: %v", err)
	}
}

func p1305Entries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}
