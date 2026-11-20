// M9.4 — per-caller memory store (memory_add / memory_search /
// memory_clear).
//
// Why a new store: no existing store fits the memory tools.
// feedback.db (internal/feedback) records routing signals only —
// decision id, tier, model, latency — never free text, and its
// schema has no place for an operator note. The M6 caches are
// prompt/response caches keyed by content hash with TTL eviction:
// a memory entry stored there would expire, collide with cache
// keys, and be served as a cache answer. So memory lives in its
// own file: one JSON line per entry under the data dir
// (<data-dir>/mcp/memory.jsonl, next to the M9.1 audit log).
//
// What the store is, honestly: a local substring-match store.
// Search is case-insensitive substring match over the caller's own
// entries only — no embeddings, no vector index, no external
// service, no neural similarity. Ranking is by occurrence count
// (how many times the query occurs in the entry), then by
// recency; the returned "matches" field IS the occurrence count,
// not a relevance score, so a caller can see exactly why an entry
// ranked where it did. Entries are namespaced by caller id: a
// caller can only add, search, or clear their own entries —
// memory_clear deletes only the caller's rows, never anyone
// else's. File: dir 0700, file 0600 (same posture as the audit
// log); writes append, clear rewrites atomically (temp + rename).
// Entry count is capped per caller (MaxMemoryEntriesPerCaller,
// same 10,000 as the audit log): an Add that would exceed the
// cap evicts the caller's oldest entries first (file order is
// insertion order), so one busy caller can never grow
// memory.jsonl unboundedly or crowd out another caller — the
// cap is per caller, never global.
package mcp

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MaxMemoryTextLen bounds one memory entry: 10,000 runes
// (characters). Longer text is rejected by Add, never silently
// truncated — a truncated memory would misremember what the
// operator stored. The count is runes, not bytes: a 10,000-rune
// CJK entry is 30,000 bytes and is a legal entry, not 3x over
// the limit.
const MaxMemoryTextLen = 10000

// MaxMemoryEntriesPerCaller caps how many entries one caller
// may hold (same 10,000 as the audit log's MaxAuditEntries):
// when an Add would exceed it, the caller's OLDEST entries are
// evicted first, so the file never grows unboundedly. 10,000
// entries at ~10 KB worst case each (a max-length entry) is
// ~100 MB in the absolute worst case, but a typical short entry
// is ~100 bytes, so a full store is ~1 MB — the cap is a count,
// not a byte size, because entry sizes are already bounded by
// MaxMemoryTextLen.
const MaxMemoryEntriesPerCaller = 10000

// MemoryEntry is one stored memory, as the tools report it. Caller
// is deliberately absent from the JSON: search results are always
// the caller's own entries, so echoing the caller id back adds
// nothing and would leak the namespace key into outputs.
type MemoryEntry struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	// Matches is the number of (non-overlapping, case-insensitive)
	// occurrences of the search query in Text. It is a count, not
	// a relevance score — see the file header.
	Matches int `json:"matches,omitempty"`
}

// memoryRecord is the on-disk shape: MemoryEntry plus its caller
// namespace, one JSON object per line.
type memoryRecord struct {
	ID        string    `json:"id"`
	Caller    string    `json:"caller"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// MemoryStore is a file-backed per-caller memory store. The zero
// value is unusable — use NewMemoryStore. All methods are safe
// for concurrent use on the same instance.
type MemoryStore struct {
	mu   sync.Mutex
	path string
	// maxEntries is the per-caller cap (MaxMemoryEntriesPerCaller
	// by default). Tests shrink it to prove rotation at the
	// boundary without adding 10,000 entries — the same
	// in-memory-cap pattern the audit log uses (maxEntries).
	maxEntries int
	// counts is the per-caller entry count, scanned once from
	// the file on first use (loadCountsLocked) and maintained
	// on Add/Clear/eviction, so Add can enforce the cap with an
	// O(1) counter check instead of re-reading the file on
	// every append. countsLoaded is false until that first scan.
	counts       map[string]int
	countsLoaded bool
}

var (
	memoryStoresMu sync.Mutex
	// memoryStores returns the same instance per path, so two
	// callers that build a store for the same file (e.g. the
	// stdio Source on every call) share one mutex instead of
	// racing an append against a clear-rewrite on the same file.
	memoryStores = map[string]*MemoryStore{}
)

// NewMemoryStore returns the shared store for path. The file is
// created lazily on first Add (never by Search/Clear — a read
// tool must not create files as a side effect).
func NewMemoryStore(path string) *MemoryStore {
	memoryStoresMu.Lock()
	defer memoryStoresMu.Unlock()
	if s, ok := memoryStores[path]; ok {
		return s
	}
	s := &MemoryStore{path: path, maxEntries: MaxMemoryEntriesPerCaller}
	memoryStores[path] = s
	return s
}

// Path returns the backing file path (tests + diagnostics).
func (s *MemoryStore) Path() string { return s.path }

// Cap returns the per-caller entry cap this store evicts at.
func (s *MemoryStore) Cap() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxEntries
}

// stripPathError unwraps an os path error to its underlying
// errno ("permission denied", "no such file or directory"):
// *PathError, *LinkError and *SyscallError all embed the full
// local path in their Error(), and that path must never reach a
// remote MCP caller in a -32602 message (H4).
// The unwrap is recursive: a *PathError may wrap a
// *SyscallError (or vice versa via %w), and returning the inner
// wrapper would keep the redundant "open: " syscall prefix
// instead of the bare errno the doc comment promises — the
// caller-facing message would read "open: no such file or
// directory" rather than "no such file or directory".
// Mirrors stripCachePathError (cmd/codvian-router/doctor_configure_mcp.go).
func stripPathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return stripPathError(pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return stripPathError(le.Err)
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		return stripPathError(se.Err)
	}
	return err
}

// storeIOError logs a filesystem failure in full — including
// the local path — to the server log (stderr, operator only;
// stdout is the MCP protocol) and returns the path-free error
// a caller sees: the operation plus the underlying errno, never
// the path. The path is operator-diagnostic information; the
// remote caller gets the kind of failure, not the layout of
// the operator's disk.
func storeIOError(op string, err error) error {
	log.Printf("mcp: memory store %s: %v", op, err)
	return fmt.Errorf("mcp: memory store %s: %w", op, stripPathError(err))
}

func newMemoryID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mcp: memory id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// pathBlockedByFile reports whether path cannot exist because an
// ancestor is a regular file rather than a directory. It walks up
// from path's parent to the first ancestor that exists; a file there
// means the store path is unusable. On Windows a path component that
// is a file surfaces as ENOENT, indistinguishable from "file absent"
// from the open error alone, so the walk is the only way to tell a
// broken path from an empty store.
func pathBlockedByFile(path string) bool {
	dir := filepath.Dir(path)
	for {
		fi, err := os.Stat(dir)
		if err == nil {
			return !fi.IsDir()
		}
		if !errors.Is(err, os.ErrNotExist) {
			return true // cannot even stat the parent: unusable
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false // nothing exists above: genuinely absent
		}
		dir = parent
	}
}

// readRecordsLocked returns every record in the file. A missing
// file is an empty store, not an error. A corrupt line is skipped
// (same posture as the audit log's List) — one bad line must not
// hide every other memory.
func (s *MemoryStore) readRecordsLocked() ([]memoryRecord, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if pathBlockedByFile(s.path) {
				return nil, storeIOError("read", err)
			}
			return nil, nil
		}
		return nil, storeIOError("read", err)
	}
	defer f.Close()
	var out []memoryRecord
	sc := bufio.NewScanner(f)
	// Max token is bytes, not runes: a max-length entry of
	// 4-byte runes is 4x MaxMemoryTextLen bytes on disk (plus
	// JSON framing), so the buffer must hold that or a legal
	// multibyte entry could be stored by Add and never read
	// back. 64 KiB initial buffer, 4*limit+4 KiB max.
	sc.Buffer(make([]byte, 0, 64*1024), MaxMemoryTextLen*4+4096)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec memoryRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, storeIOError("read", err)
	}
	return out, nil
}

// loadCountsLocked scans the file once and counts entries per
// caller, so Add can enforce the per-caller cap with an O(1)
// counter check instead of re-reading the file on every append
// (the audit log keeps the same kind of in-memory counter,
// counted at Open). The counts can go stale if another process
// appends to the same file — the cap is then enforced late,
// never early: a stale count only ever delays eviction, it can
// never drop an entry while the caller is still under the cap.
// That "late, never early" holds for appends only. If another
// process deletes or clears the file instead, the count goes
// stale-HIGH: evictOldestLocked's `dropped == 0` early return
// leaves the stale count in place (it only resyncs the count
// after a rewrite that actually dropped something), so a later
// Add can evict a fresh entry while the caller is actually
// under the cap. Single-process in practice — the store is
// only ever opened by one server process — so this is a
// documented edge, not a live path.
// The caller must hold s.mu.
func (s *MemoryStore) loadCountsLocked() error {
	if s.countsLoaded {
		return nil
	}
	records, err := s.readRecordsLocked()
	if err != nil {
		return err
	}
	s.counts = make(map[string]int, len(records))
	for _, rec := range records {
		s.counts[rec.Caller]++
	}
	s.countsLoaded = true
	return nil
}

// rewriteLocked atomically replaces the file with kept (temp
// file in the same directory + rename, 0600) — the same rewrite
// Clear has always used, shared here with eviction. The caller
// must hold s.mu.
func (s *MemoryStore) rewriteLocked(kept []memoryRecord) error {
	dir := filepath.Dir(s.path)
	if dir == "" || dir == "." {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".memory-rewrite-*")
	if err != nil {
		return storeIOError("rewrite temp file", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return storeIOError("rewrite temp file", err)
	}
	w := bufio.NewWriter(tmp)
	for _, rec := range kept {
		raw, err := json.Marshal(rec)
		if err != nil {
			tmp.Close()
			return fmt.Errorf("mcp: memory store rewrite marshal: %w", err)
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			tmp.Close()
			return storeIOError("rewrite write", err)
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return storeIOError("rewrite write", err)
	}
	if err := tmp.Close(); err != nil {
		return storeIOError("rewrite write", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return storeIOError("rewrite replace", err)
	}
	return nil
}

// evictOldestLocked drops the n oldest entries of caller —
// file order is insertion order (append-only), so the oldest
// are the first of the caller's lines — by rewriting the file
// without them, and updates the in-memory count to match the
// rewritten file. The caller must hold s.mu, and counts must
// already be loaded.
func (s *MemoryStore) evictOldestLocked(caller string, n int) error {
	records, err := s.readRecordsLocked()
	if err != nil {
		return err
	}
	kept := make([]memoryRecord, 0, len(records))
	dropped := 0
	for _, rec := range records {
		if rec.Caller == caller && dropped < n {
			dropped++
			continue
		}
		kept = append(kept, rec)
	}
	if dropped == 0 {
		return nil
	}
	if err := s.rewriteLocked(kept); err != nil {
		return err
	}
	remaining := 0
	for _, rec := range kept {
		if rec.Caller == caller {
			remaining++
		}
	}
	s.counts[caller] = remaining
	return nil
}

// Add stores text under caller and returns the stored entry. An
// empty caller, empty text, or text over MaxMemoryTextLen is an
// error, never a silent store of something else. Those validation
// failures wrap ErrInvalidInput, so a tool handler can tell them
// apart from a backing-store failure and report them as-is; a
// store failure is path-free (see storeIOError). When the caller
// is already at the per-caller cap, the oldest entries are
// evicted first (evictOldestLocked) so the store never exceeds
// MaxMemoryEntriesPerCaller entries for that caller — other
// callers' entries are untouched.
func (s *MemoryStore) Add(caller, text string) (MemoryEntry, error) {
	if strings.TrimSpace(caller) == "" {
		return MemoryEntry{}, fmt.Errorf("%w: memory_add requires a caller id", ErrInvalidInput)
	}
	if strings.TrimSpace(text) == "" {
		return MemoryEntry{}, fmt.Errorf("%w: memory_add requires non-empty text", ErrInvalidInput)
	}
	if n := utf8.RuneCountInString(text); n > MaxMemoryTextLen {
		return MemoryEntry{}, fmt.Errorf("%w: memory text is %d characters, over the %d limit", ErrInvalidInput, n, MaxMemoryTextLen)
	}
	id, err := newMemoryID()
	if err != nil {
		return MemoryEntry{}, err
	}
	rec := memoryRecord{ID: id, Caller: caller, Text: text, CreatedAt: time.Now().UTC()}
	raw, err := json.Marshal(rec)
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("mcp: memory_add marshal: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return MemoryEntry{}, storeIOError("create directory", err)
		}
	}
	if err := s.loadCountsLocked(); err != nil {
		return MemoryEntry{}, err
	}
	if s.counts[caller] >= s.maxEntries {
		// Evict down to one free slot (cap-1), so the append
		// below lands exactly at the cap. A count above the
		// cap — a stale count, or entries written by another
		// process — evicts the same way, down to cap-1.
		if err := s.evictOldestLocked(caller, s.counts[caller]-s.maxEntries+1); err != nil {
			return MemoryEntry{}, err
		}
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return MemoryEntry{}, storeIOError("open", err)
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return MemoryEntry{}, storeIOError("write", err)
	}
	s.counts[caller]++
	return MemoryEntry{ID: rec.ID, Text: rec.Text, CreatedAt: rec.CreatedAt}, nil
}

// Search returns the caller's entries containing query
// (case-insensitive substring), ranked by occurrence count
// descending, then by recency descending. An empty query is an
// error: returning everything would leak the store's size and
// contents to a fishing caller. Matches is the raw occurrence
// count — see MemoryEntry.
func (s *MemoryStore) Search(caller, query string) ([]MemoryEntry, error) {
	if strings.TrimSpace(caller) == "" {
		return nil, fmt.Errorf("%w: memory_search requires a caller id", ErrInvalidInput)
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: memory_search requires a non-empty query", ErrInvalidInput)
	}
	needle := strings.ToLower(query)
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readRecordsLocked()
	if err != nil {
		return nil, err
	}
	out := []MemoryEntry{}
	for _, rec := range records {
		if rec.Caller != caller {
			continue
		}
		n := strings.Count(strings.ToLower(rec.Text), needle)
		if n == 0 {
			continue
		}
		out = append(out, MemoryEntry{ID: rec.ID, Text: rec.Text, CreatedAt: rec.CreatedAt, Matches: n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Matches != out[j].Matches {
			return out[i].Matches > out[j].Matches
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// Clear deletes every entry stored under caller and returns how
// many were removed. Other callers' entries are kept: the file is
// rewritten (temp + rename) with only their lines. A caller with
// no entries clears 0 and rewrites nothing.
func (s *MemoryStore) Clear(caller string) (int, error) {
	if strings.TrimSpace(caller) == "" {
		return 0, fmt.Errorf("%w: memory_clear requires a caller id", ErrInvalidInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readRecordsLocked()
	if err != nil {
		return 0, err
	}
	kept := make([]memoryRecord, 0, len(records))
	removed := 0
	for _, rec := range records {
		if rec.Caller == caller {
			removed++
			continue
		}
		kept = append(kept, rec)
	}
	if removed == 0 {
		return 0, nil
	}
	if err := s.rewriteLocked(kept); err != nil {
		return 0, err
	}
	if s.countsLoaded {
		s.counts[caller] = 0
	}
	return removed, nil
}
