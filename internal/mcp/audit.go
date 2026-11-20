// Package mcp — audit log (M9.1, CHECKLIST 382).
//
// Every tool call — allowed AND denied — is appended to an
// append-only JSONL log, one entry per line, under the router's
// data dir at <data-dir>/mcp/audit.jsonl. Shape reference only
// (never copied code): OmniRoute's SQLite `mcp_tool_audit` table
// (open-sse/mcp-server/audit.ts) — inputs SHA-256-hashed, outputs
// truncated to 200 chars. We keep the same two redaction rules
// and the same 200-char output cap, but persist as JSONL instead
// of SQLite: the router has no SQLite dependency in this package,
// a JSONL line is appendable atomically with O_APPEND + fsync and
// readable with any line reader, and the log needs no queries
// beyond "latest N entries" (the M9.7 dashboard audit view).
//
// Redaction rules (both load-bearing, both tested):
//   - Inputs are NEVER stored raw: only the SHA-256 hex of the
//     canonical JSON of the arguments is recorded. Tool arguments
//     may contain secrets (the guardrail already scans them for
//     that reason), so storing them would turn the audit log into
//     a secret store. The hash still lets an operator prove a
//     given input was (or was not) the one logged, without the log
//     itself revealing it.
//   - Outputs are truncated to 200 chars (MaxOutputSummaryChars):
//     enough to identify what a call returned, not enough to leak
//     a full provider/model dump into a log an operator might
//     paste into a bug report. Truncation is by rune, so it never
//     splits a UTF-8 character.
//   - The caller id recorded is the identity's stable id — the
//     local-operator marker, or a gateway key id. Key MATERIAL is
//     never recorded anywhere in this package.
//
// Persistence follows the files store's pattern
// (internal/files/store.go): directory created 0700, file 0600,
// each record appended under a mutex with a single write + fsync,
// so a crash cannot interleave two entries or lose a buffered
// one. A record failure never fails the tool call itself: the
// dispatcher serves the call and the failure is kept on
// LastError() so M9.2/M9.7 can surface it (availability over
// audit completeness — a tool call is an in-memory read, and
// refusing it because the disk is full would be a self-inflicted
// outage; the gap is disclosed in the threat model, not hidden).
// There is NO delete or truncate API: the log is append-only by
// construction. Rotation is out of scope for this slice (M9.7
// follow-up: cap file size / entry count before the dashboard
// reads it unboundedly).
package mcp

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxOutputSummaryChars is the output-truncation cap, matching
// OmniRoute's 200-char audit summary.
const MaxOutputSummaryChars = 200

// MaxAuditEntries is the audit-log entry cap (M9.7 rotation): the
// log never grows past this many entries. When a Record would
// exceed it, the OLDEST entries are rotated out first — the newest
// entries are never lost to make room, and every rotation is
// counted on Dropped() so the dashboard can show it instead of
// silently dropping history. 10,000 entries at ~300 bytes each is
// ~3 MiB on disk: large enough to hold weeks of tool calls, small
// enough that the M9.7 dashboard read (List, newest-first) stays
// cheap. The cap is a count, not a byte size: entry sizes are
// bounded already (hashed inputs, 200-rune output summaries), so
// a count cap bounds the file.
const MaxAuditEntries = 10000

// AuditDecision is the recorded outcome of a tool call.
type AuditDecision string

const (
	// AuditAllow is a call that passed scope + guardrail and was
	// dispatched.
	AuditAllow AuditDecision = "allow"
	// AuditDeny is a call refused before dispatch (scope denial,
	// guardrail refusal, or unknown tool).
	AuditDeny AuditDecision = "deny"
)

// AuditEntry is one logged tool call. Timestamp is RFC 3339 nano
// on disk (time.Time marshals that way). InputHash is the SHA-256
// hex of the canonical JSON of the call's arguments — never the
// arguments themselves. OutputSummary is the call's result JSON
// truncated to MaxOutputSummaryChars runes (empty for denied
// calls, which produced no output). ErrorCode is the JSON-RPC
// error code for denied calls (e.g. -32002 scope, -32001
// guardrail, -32602 unknown tool) and 0 for allowed calls.
type AuditEntry struct {
	Timestamp     time.Time     `json:"timestamp"`
	Tool          string        `json:"tool"`
	CallerKind    ClientKind    `json:"caller_kind"`
	CallerID      string        `json:"caller_id"`
	Decision      AuditDecision `json:"decision"`
	Reason        string        `json:"reason,omitempty"`
	InputHash     string        `json:"input_hash"`
	OutputSummary string        `json:"output_summary,omitempty"`
	ErrorCode     int           `json:"error_code,omitempty"`
}

// HashArguments returns the SHA-256 hex of the canonical JSON of
// args (encoding/json sorts map keys, so the hash is deterministic
// for the same arguments). A nil args map hashes as "null". This
// is the ONLY form in which inputs ever reach the log.
func HashArguments(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		// Arguments arrived as JSON, so marshalling cannot fail
		// in practice; if it ever did, hash the Go formatting
		// rather than storing the raw value.
		raw = []byte(fmt.Sprintf("%v", args))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SummarizeOutput renders payload as JSON and truncates it to
// MaxOutputSummaryChars runes. Truncation is by rune so multi-byte
// characters are never split.
func SummarizeOutput(payload any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(fmt.Sprintf("%v", payload))
	}
	runes := []rune(string(raw))
	if len(runes) > MaxOutputSummaryChars {
		runes = runes[:MaxOutputSummaryChars]
	}
	return string(runes)
}

// AuditLog is an append-only JSONL audit log at path. All methods
// are safe for concurrent use; a mutex serialises appends so two
// concurrent tool calls cannot interleave lines.
//
// Rotation (M9.7): the log is capped at maxEntries entries
// (MaxAuditEntries by default). Record rotates the OLDEST entries
// out when the cap would be exceeded, so the file never grows
// unboundedly and the dashboard never reads it unboundedly.
// entries tracks the live entry count (counted at Open, then
// maintained on Record/rotation); dropped counts entries removed
// by rotation in this process's lifetime — it resets on restart,
// because a rotation that happened before this process opened the
// log left no record of itself. The dashboard shows both numbers
// so a rotation is visible, never silent.
type AuditLog struct {
	path       string
	mu         sync.Mutex
	lastErr    error
	maxEntries int
	entries    int
	dropped    int
}

// OpenAuditLog opens (creating it and its parent directory if
// needed) the audit log at path. The directory is created 0700
// and the file 0600 — the log records who called what, which is
// operator-private. Permissions are tightened with Chmod on every
// open, not just at creation (M9.1 SECURITY REVIEW N2): MkdirAll
// and OpenFile do not chmod pre-existing paths, so a pre-existing
// 0755 directory or 0644 file would otherwise stay loose.
// Merely opening the log writes nothing: an empty log file is a
// log with zero entries, not an error. The returned log is ready
// to Record.
func OpenAuditLog(path string) (*AuditLog, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mcp: create audit dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mcp: tighten audit dir perms: %w", err)
	}
	// Probe writability now, so a read-only data dir fails here
	// (loud, at startup) instead of on the first Record.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mcp: open audit log: %w", err)
	}
	// Tighten a pre-existing file's permissions too (N2): a 0644
	// log left by an older version is fixed on open, not left
	// world-readable.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mcp: tighten audit log perms: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("mcp: open audit log: %w", err)
	}
	l := &AuditLog{path: path, maxEntries: MaxAuditEntries}
	// Count the entries already on disk (same parse rule as
	// List: non-empty, parseable lines only), so the first
	// Record after a restart rotates at the right point instead
	// of treating a full log as empty.
	if n, err := l.countEntries(); err != nil {
		return nil, fmt.Errorf("mcp: count audit log: %w", err)
	} else {
		l.entries = n
	}
	return l, nil
}

// countEntries returns the number of parseable entries in the log
// file (a missing file counts as 0). It takes the mutex, so it is
// only called from OpenAuditLog (before the log escapes) and from
// tests.
func (l *AuditLog) countEntries() (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("mcp: read audit log: %w", err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // same skip-corrupt rule as List
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("mcp: read audit log: %w", err)
	}
	return n, nil
}

// Cap returns the entry cap this log rotates at.
func (l *AuditLog) Cap() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxEntries
}

// Dropped returns how many entries rotation has removed in this
// process's lifetime (0 until the first rotation; resets on
// restart — see the AuditLog doc comment).
func (l *AuditLog) Dropped() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

// Path returns the log file's path.
func (l *AuditLog) Path() string { return l.path }

// LastError returns the most recent Record failure, or nil. A
// Record failure never fails the tool call it was logging (see
// package doc); this is how M9.2/M9.7 can notice and surface it.
func (l *AuditLog) LastError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastErr
}

// Record appends one entry: a single O_APPEND write of one JSON
// line, then fsync, so the entry is durable and cannot interleave
// with another entry. A zero Timestamp is stamped with time.Now
// (UTC). Record never panics on a bad entry; a marshal failure is
// returned as an error like any write failure.
func (l *AuditLog) Record(e AuditEntry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		l.setLastErr(fmt.Errorf("mcp: encode audit entry: %w", err))
		// Read the stored error back under the lock (M9.1
		// SECURITY REVIEW N7): reading l.lastErr directly here
		// would race with a concurrent Record.
		return l.LastError()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		l.lastErr = fmt.Errorf("mcp: open audit log for append: %w", err)
		return l.lastErr
	}
	// Best-effort tighten (N2): a file loosened after Open keeps
	// 0600 on every append. A chmod failure here never fails the
	// entry — the write below is what matters.
	_ = f.Chmod(0o600)
	_, werr := f.Write(append(raw, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		l.lastErr = fmt.Errorf("mcp: write audit entry: write=%v fsync=%v close=%v", werr, serr, cerr)
		return l.lastErr
	}
	l.lastErr = nil
	l.entries++
	// Rotation (M9.7): the cap is enforced here, on the append
	// path, so the file can never exceed it by more than one
	// entry. A rotation failure is kept on LastError but never
	// fails the entry just written — the entry is already
	// durable; the file is only over-cap until the next Record
	// retries the rotation.
	if l.entries > l.maxEntries {
		if rerr := l.rotateLocked(); rerr != nil {
			l.lastErr = rerr
		}
	}
	return nil
}

// rotateLocked drops the oldest entries so the log holds at most
// maxEntries, newest first in time order preserved. The caller
// must hold l.mu. It rewrites the file via a temp file in the same
// directory + rename (atomic: a crash leaves the old file or the
// new one, never a half-written log), with 0600 permissions and an
// fsync of both the file and the directory. l.entries and
// l.dropped are updated to match the rewritten file.
func (l *AuditLog) rotateLocked() error {
	f, err := os.Open(l.path)
	if err != nil {
		return fmt.Errorf("mcp: rotate audit log: %w", err)
	}
	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		lines = append(lines, cp)
	}
	serr := sc.Err()
	_ = f.Close()
	if serr != nil {
		return fmt.Errorf("mcp: rotate audit log: %w", serr)
	}
	keep := l.maxEntries
	if keep < 0 {
		keep = 0
	}
	dropped := 0
	if len(lines) > keep {
		dropped = len(lines) - keep
		lines = lines[len(lines)-keep:]
	}
	dir := filepath.Dir(l.path)
	tmp, err := os.CreateTemp(dir, ".audit-rotate-*")
	if err != nil {
		return fmt.Errorf("mcp: rotate audit log: %w", err)
	}
	tmpName := tmp.Name()
	// Best effort cleanup on failure: the rename below moves it
	// away on success, so a leftover here means rotation failed.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("mcp: rotate audit log: %w", err)
	}
	for _, line := range lines {
		if _, err := tmp.Write(append(line, '\n')); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("mcp: rotate audit log: %w", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("mcp: rotate audit log: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("mcp: rotate audit log: %w", err)
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		return fmt.Errorf("mcp: rotate audit log: %w", err)
	}
	// Fsync the directory so the rename itself is durable.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	l.entries = len(lines)
	l.dropped += dropped
	return nil
}

func (l *AuditLog) setLastErr(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastErr = err
}

// List returns audit entries newest-first. limit <= 0 means all
// entries. A malformed line is skipped (a corrupt line must not
// break the audit view — the entries around it are still evidence),
// and a missing log file is zero entries, not an error.
func (l *AuditLog) List(limit int) ([]AuditEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []AuditEntry{}, nil
		}
		return nil, fmt.Errorf("mcp: read audit log: %w", err)
	}
	defer f.Close()
	var entries []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // skip corrupt line, keep the rest
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mcp: read audit log: %w", err)
	}
	// Newest first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// Count returns the number of entries in the log (malformed lines
// excluded, same rule as List). A missing log file counts as 0.
func (l *AuditLog) Count() (int, error) {
	entries, err := l.List(0)
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}
