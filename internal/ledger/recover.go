package ledger

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Logf is an optional hook for the package's own operational notices
// (a quarantined corrupt ledger today). It is a var so the caller can
// wire a real logger; nil means notices are dropped, never panicked.
var Logf func(format string, args ...any)

func logf(format string, args ...any) {
	if Logf != nil {
		Logf(format, args...)
	}
}

// maxProbeBytes bounds the integrity probe's file copy. Above it the
// probe declines to form a verdict (and therefore nothing is ever
// quarantined): copying a very large ledger at startup is not
// acceptable, and a truncated copy could produce a FALSE corruption
// verdict, which would be data loss. An operator inspects such a file
// explicitly.
const maxProbeBytes = 1 << 30 // 1 GiB

// openLedger is the Open used by OpenWithRecovery. A variable so tests
// can force a specific failure (I/O, migration, recovery-failure paths)
// without having to produce that failure for real on the host.
var openLedger = Open

// corruptionMarkers are the messages SQLite emits when a file is not a
// usable database. They are POSITIVE evidence of corruption; their
// absence is never evidence of health (see ledgerCorruptReason).
var corruptionMarkers = []string{
	"file is not a database",
	"database disk image is malformed",
	"malformed database schema",
	"file is encrypted or is not a database",
}

// operationalMarkers are failures that must NEVER be treated as
// corruption: the database may be perfectly valid, and renaming it
// aside would destroy or strand a healthy ledger (AUD-REL-005 review).
var operationalMarkers = []string{
	"database is locked",
	"busy",
	"unable to open database file",
	"disk i/o error",
	"readonly",
	"read-only",
	"permission",
	"access is denied",
	"disk is full",
	"no space left",
	"out of memory",
	"interrupted",
	"not authorized",
	"cannot open",
	"no such file",
	"is a directory",
	"unsupported",
	"migration",
	"no such table",
	"no such column",
}

func matchesAny(msg string, markers []string) bool {
	lower := strings.ToLower(msg)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// ledgerCorruptReason POSITIVELY identifies a corrupt database by asking
// SQLite itself, and returns a human-readable reason when it is. It
// returns "" for a valid database, for an operational failure (the file
// may be fine and merely unreachable right now) and whenever it cannot
// tell — in which case the caller must NOT quarantine.
//
// The probe runs `PRAGMA quick_check(1)` against a COPY of the main
// database file, never the original: SQLite mutates sidecar state when
// it opens a database (it discards a stale -wal/-shm), and a diagnostic
// must not touch a database that may be perfectly good. A file too large
// to copy bounded (maxProbeBytes) gets NO verdict, which fails safe: no
// verdict means no quarantine.
func ledgerCorruptReason(path string) string {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return "" // nothing to inspect
	}
	if st.Size() > maxProbeBytes {
		logf("ledger: %s is %d bytes; too large to integrity-probe without copying, so it will NOT be quarantined (an operator must inspect it)", path, st.Size())
		return ""
	}
	src, err := os.Open(path)
	if err != nil {
		return "" // unreadable: an operational problem, not a verdict
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ledger-probe-*.db")
	if err != nil {
		return ""
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return ""
	}
	if err := tmp.Close(); err != nil {
		return ""
	}

	// busy_timeout(0): the probe must REPORT a busy database, not wait.
	db, err := sql.Open("sqlite", tmpName+"?_pragma=busy_timeout(0)")
	if err != nil {
		return ""
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var res string
	if err := db.QueryRow("PRAGMA quick_check(1)").Scan(&res); err != nil {
		msg := err.Error()
		if matchesAny(msg, operationalMarkers) {
			return "" // locked / unreadable / I/O: not a corruption verdict
		}
		if matchesAny(msg, corruptionMarkers) {
			return msg
		}
		return ""
	}
	if strings.TrimSpace(res) == "" || strings.EqualFold(strings.TrimSpace(res), "ok") {
		return ""
	}
	return "quick_check: " + res
}

// OpenWithRecovery opens the ledger and recovers ONLY from positively
// identified corruption: the unusable file is renamed aside (never
// deleted, never overwritten) and a fresh ledger is opened, so a corrupt
// database no longer makes the gateway unstartable (AUD-REL-005).
//
// Everything else — busy, locked, permissions, I/O, disk full, an
// unsupported pragma, a migration failure — is returned as an error
// with the original database left exactly where it was and its WAL
// sidecars untouched. Those are operational failures: the data may be
// perfectly good, and a "recovery" that renamed it would be data loss.
// A valid database is NEVER renamed merely because Open failed.
//
// The returned string is the quarantine path ("" when nothing was
// quarantined) so the caller can report it.
func OpenWithRecovery(path string) (*Ledger, string, error) {
	l, err := openLedger(path)
	if err == nil {
		return l, "", nil
	}

	// Decide from positive evidence, never from "Open returned an error".
	// The probe returns a reason only for POSITIVE corruption; Open's own
	// error is consulted as a second opinion.
	reason := ledgerCorruptReason(path)
	if reason == "" && IsCorrupt(err) {
		reason = err.Error()
	}
	if reason == "" {
		// Operational failure: report it, move nothing.
		return nil, "", err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		return nil, "", err
	}

	quarantine := QuarantinedPath(path, time.Now())
	if renameErr := os.Rename(path, quarantine); renameErr != nil {
		// Cannot move it aside: the original error is the honest answer.
		return nil, "", fmt.Errorf("%w (quarantine %q also failed: %v)", err, quarantine, renameErr)
	}
	logf("ledger: %s is corrupt (%s); the file was preserved at %s and a fresh ledger was created", path, reason, quarantine)
	// Only a file we are quarantining takes its sidecars with it: the
	// fresh ledger must not adopt a write-ahead log that belongs to the
	// corrupt file. A file we do NOT quarantine keeps its sidecars.
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := path + suffix
		if _, statErr := os.Stat(sidecar); statErr == nil {
			if mvErr := os.Rename(sidecar, quarantine+suffix); mvErr != nil {
				logf("ledger: could not move sidecar %s aside: %v", sidecar, mvErr)
			}
		}
	}
	fresh, freshErr := openLedger(path)
	if freshErr != nil {
		return nil, quarantine, fmt.Errorf("ledger: fresh ledger after quarantining %q: %w", quarantine, freshErr)
	}
	return fresh, quarantine, nil
}

// IsCorrupt reports whether an error carries positive evidence that the
// file is not a usable SQLite database. An operational failure (busy,
// locked, permissions, I/O, disk full, an unsupported pragma, a
// migration failure) is NOT corruption, however much it looks like a
// startup failure.
func IsCorrupt(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if matchesAny(msg, operationalMarkers) {
		return false
	}
	return matchesAny(msg, corruptionMarkers)
}

// QuarantinedPath returns the sibling path a corrupt ledger at path
// would be moved to (used by operators and tests).
func QuarantinedPath(path string, at time.Time) string {
	return filepath.Clean(fmt.Sprintf("%s.corrupt-%d", path, at.Unix()))
}

// IsQuarantinePath reports whether a path looks like a quarantined
// ledger produced by OpenWithRecovery.
func IsQuarantinePath(path string) bool {
	return strings.Contains(path, ".corrupt-")
}
