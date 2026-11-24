package ledger

import (
	"fmt"
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

// OpenWithRecovery opens the ledger and, when the existing file cannot
// be used at all, QUARANTINES it (renames it aside, preserving every
// byte) and opens a fresh one — instead of the process refusing to
// start (AUD-REL-005).
//
// The defect this addresses: Open returns an error for every failure
// mode (WAL, busy_timeout, schema, column migrations), and the caller
// treated any error as fatal. A corrupt ledger.db therefore made the
// gateway unstartable until an operator moved the file by hand, with no
// signal about what was wrong.
//
// Data-safety rules: a file that exists and merely failed to OPEN is
// never deleted and never overwritten in place — it is renamed to
// "<path>.corrupt-<unix seconds>" and the move is reported. A brand new
// path (nothing to lose) is opened normally. The returned string is the
// quarantine path, empty when nothing was quarantined.
func OpenWithRecovery(path string) (*Ledger, string, error) {
	l, err := Open(path)
	if err == nil {
		return l, "", nil
	}
	if _, statErr := os.Stat(path); statErr != nil {
		// Nothing on disk to quarantine: the failure is environmental
		// (permissions, a bad directory) and must stay fatal.
		return nil, "", err
	}
	quarantine := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	if renameErr := os.Rename(path, quarantine); renameErr != nil {
		// Cannot move it aside: the original error is the honest answer.
		return nil, "", fmt.Errorf("%w (quarantine %q also failed: %v)", err, quarantine, renameErr)
	}
	logf("ledger: %s could not be opened (%v); the file was preserved at %s and a fresh ledger was created", path, err, quarantine)
	// The WAL sidecars belong to the quarantined file: move them too so
	// the fresh ledger cannot pick up a stale write-ahead log.
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := path + suffix
		if _, statErr := os.Stat(sidecar); statErr == nil {
			_ = os.Rename(sidecar, quarantine+suffix)
		}
	}
	fresh, freshErr := Open(path)
	if freshErr != nil {
		return nil, quarantine, fmt.Errorf("ledger: fresh ledger after quarantining %q: %w", quarantine, freshErr)
	}
	return fresh, quarantine, nil
}

// IsCorrupt reports whether an Open error means the file is not a usable
// SQLite database (as opposed to an environmental failure such as
// permissions or a missing directory). It exists so callers and tests
// can tell the two apart without string-matching at the call site.
func IsCorrupt(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{"file is not a database", "database disk image is malformed"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// QuarantinedPath returns the sibling path a corrupt ledger at path
// would be moved to (used by operators and tests).
func QuarantinedPath(path string, at time.Time) string {
	return filepath.Clean(fmt.Sprintf("%s.corrupt-%d", path, at.Unix()))
}
