// Package files is the local disk store behind POST/GET/DELETE
// /v1/files (M8.7, Area 14 §14a "Files" row).
//
// The store is LOCAL DISK ONLY: an uploaded file is written under
// the router's data dir (<data-dir>/files/) and never leaves the
// machine — there is no upstream upload, no provider call, and no
// network access of any kind. Content is addressed by the
// server-generated id, never by a user-supplied path: the on-disk
// file for id X is <root>/X, and X must match ^file-[0-9a-f]{24}$,
// so a crafted id cannot escape the directory. Filenames are
// sanitised to a bare base name on upload (see SanitizeFilename).
//
// Metadata (id, bytes, created_at, filename, purpose) lives in a
// JSON index (<root>/index.json) written atomically (temp file +
// rename) and fsync'd on every mutation, alongside an fsync of the
// content file before the index records it — so a crash cannot
// leave the index pointing at a half-written file.
//
// Purposes are the six OpenAI upload purposes, verified against
// the OpenAI API reference (developers.openai.com, Files / Upload
// file, 2026-11-19): assistants, batch, fine-tune, vision,
// user_data, evals. The File object's extra output purposes
// (assistants_output, batch_output, fine-tune-results) are
// server-generated only and are NOT accepted on upload.
//
// UPLOAD CAP (corrected — AUD-DOC-004): an upload IS capped, at
// filesMaxUploadBytes = 512 MiB per file, enforced by the HTTP layer
// before the body reaches this store (internal/server/files_api.go).
// The previous text here claimed ""there is no upload-size cap""; that
// was stale and wrong. Uploads stream to a temp file on disk rather
// than memory, so within the cap the cost is disk, not RAM.
// AGGREGATE AND PER-KEY DISK QUOTA (P2-SEC-05 / AUD-SEC-006): maxDiskBytes
// bounds all files in this store and maxKeyDiskBytes bounds any single key,
// so one key cannot fill the disk with many individually-legal uploads.
// Usage is tracked in CreateOwned/CreateServerGeneratedOwned and freed on
// DeleteFor; Usage() and KeyUsage() expose the counters for observability.
package files

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ValidPurposes is the fixed list of purposes accepted on upload
// (OpenAI Files API, verified 2026-11-19 — see package doc).
var ValidPurposes = []string{"assistants", "batch", "fine-tune", "vision", "user_data", "evals"}

// ValidPurpose reports whether purpose is one of ValidPurposes.
func ValidPurpose(purpose string) bool {
	for _, p := range ValidPurposes {
		if p == purpose {
			return true
		}
	}
	return false
}

// File is one stored file, in the OpenAI File-object shape: id,
// object ("file"), bytes, created_at (unix seconds), filename,
// purpose, status ("processed" — a local file is usable as soon as
// the upload lands; there is no async processing step). No
// expires_at: this store does not expire files, and inventing one
// would lie about a policy that does not exist.
type File struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
	Status    string `json:"status"`
	// OwnerKeyID is the gateway key that uploaded the file
	// (AUD-SEC-010). "" means the file is unowned — uploaded while
	// the deployment had no key identity (auth-relaxed), or created
	// by a server-generated path before ownership was recorded. An
	// unowned file is visible ONLY to an unowned caller, exactly like
	// a batch record: an empty owner is never a wildcard.
	OwnerKeyID string `json:"owner_key_id,omitempty"`
}

// visibleTo reports whether a file may be exposed to callerID.
//
// An IDENTIFIED caller sees only the files it owns; an UNIDENTIFIED
// caller sees only unowned files. Neither direction is a wildcard
// (AUD-SEC-010, mirroring batchVisible after AUD-SEC-001).
func visibleTo(f File, callerID string) bool {
	if callerID == "" {
		return f.OwnerKeyID == ""
	}
	return f.OwnerKeyID == callerID
}

var (
	// ErrNotFound is returned when no file with the given id exists.
	ErrNotFound = errors.New("files: file not found")
	// ErrInvalidID is returned when an id is not a store-issued id.
	ErrInvalidID = errors.New("files: invalid file id")
	// ErrInvalidPurpose is returned when purpose is not in ValidPurposes.
	ErrInvalidPurpose = errors.New("files: invalid purpose")
	// ErrInvalidFilename is returned when a filename sanitises to nothing.
	ErrInvalidFilename = errors.New("files: invalid filename")
)

// fileIDPattern is the only id shape the store ever issues or
// accepts: "file-" plus 24 lowercase hex chars (12 random bytes).
// Anything else — including "..", "/", or a user-chosen name — is
// rejected before it can touch a path.
var fileIDPattern = regexp.MustCompile(`^file-[0-9a-f]{24}$`)

// ValidID reports whether id is a store-issued id shape.
func ValidID(id string) bool { return fileIDPattern.MatchString(id) }

// newID generates a fresh store id.
func newID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("files: random id: %w", err)
	}
	return "file-" + hex.EncodeToString(b[:]), nil
}

// SanitizeFilename reduces an uploaded filename to a safe bare
// base name: path separators (either flavour) and their leading
// components are dropped, surrounding whitespace is trimmed, and
// control characters are stripped. "../../etc/passwd" becomes
// "passwd"; "a/b/c.txt" becomes "c.txt". An empty result, ".", or
// ".." is an error — the caller must reject the upload, not guess
// a name.
func SanitizeFilename(name string) (string, error) {
	// Drop everything up to the last separator of either flavour.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	name = b.String()
	switch name {
	case "", ".", "..":
		return "", ErrInvalidFilename
	}
	return name, nil
}

// Store is a local disk file store rooted at dir: content files
// live directly under dir (one file per id), metadata in
// dir/index.json. All methods are safe for concurrent use; a
// process-wide mutex serialises index reads/writes so two
// concurrent uploads cannot lose each other's index entry.
type Store struct {
	root string
	mu   sync.Mutex
	// legacyIndex records that the index last read was in the
	// pre-ownership v1 format (a bare array). The caller logs it once so
	// unowned legacy records are never invisible to the OPERATOR, only
	// to unauthenticated callers (AUD-SEC-010).
	legacyIndex bool
	// totalBytes is the aggregate bytes of all content files in this
	// store, tracked for the maxDiskBytes quota (P2-SEC-05).
	totalBytes int64
	// keyBytes is per-key disk usage, tracked for the maxKeyDiskBytes
	// quota (P2-SEC-05).
	keyBytes map[string]int64
}

// New returns a store rooted at dir. The directory (0700, like the
// router's data dir) is created on first write, not here, so
// merely constructing a store never touches disk.
func New(dir string) *Store {
	return &Store{root: dir, keyBytes: make(map[string]int64)}
}

// Root returns the store's root directory.
func (s *Store) Root() string { return s.root }

// maxDiskBytes is the aggregate disk quota for all files in this store:
// one key cannot fill the disk with many individually-legal uploads.
const maxDiskBytes = 10 << 30 // 10 GiB

// maxKeyDiskBytes is the per-key disk quota.
const maxKeyDiskBytes = 1 << 30 // 1 GiB

// ErrDiskQuota is returned when an upload would exceed the aggregate or
// per-key disk quota.
var ErrDiskQuota = errors.New("files: disk quota exceeded")

// StaleUploadTempAge is how long a staging temp must sit

const StaleUploadTempAge = time.Hour

// SweepStaleUploadTemps removes orphaned staging temps
// (upload-*.tmp) and orphaned atomic-index temps (index-*.tmp)
// from the store dir: the residue a process crash (SIGKILL)
// mid-upload or mid-index-write leaves behind, when no deferred
// Remove or write-failure cleanup ever runs. Only files matching
// one of the two temp patterns are touched — content files
// (store ids), index.json, and anything else in the dir are never
// candidates — and only when their last modification is older
// than olderThan, so a live (or recently stalled) upload or a
// live index write is never deleted: a temp being written has a
// fresh mtime. A missing store dir holds no orphans and is not
// an error. The store mutex is held for the whole sweep, so no
// Create in this process can be staging a temp while the sweep
// runs; any upload-*.tmp or index-*.tmp seen under the lock
// belongs to another (dead) process. Per-file errors do not abort
// the sweep: the count of removed files is returned alongside
// the first error encountered, if any.
//
// The name is historical (upload temps were the first orphan
// class); it now also sweeps index temps. It is kept unchanged
// so the startup call site in internal/server/files_api.go needs
// no edit.
func (s *Store) SweepStaleUploadTemps(olderThan time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("files: sweep staging temps: %w", err)
	}
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	var firstErr error
	for _, e := range entries {
		name := e.Name()
		isTemp := (strings.HasPrefix(name, "upload-") || strings.HasPrefix(name, "index-")) && strings.HasSuffix(name, ".tmp")
		if e.IsDir() || !isTemp {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("files: sweep staging temps: stat %s: %w", name, err)
			}
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue // live or recently stalled upload: never delete
		}
		if err := os.Remove(filepath.Join(s.root, name)); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("files: sweep staging temps: remove %s: %w", name, err)
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

func (s *Store) indexPath() string { return filepath.Join(s.root, "index.json") }
func (s *Store) contentPath(id string) string {
	return filepath.Join(s.root, id)
}

// indexEnvelope is the persisted index format from v2 on. The v1 format
// was a bare JSON array of File, which had no room for a format marker
// and (before AUD-SEC-010) no owner field at all. Loading accepts BOTH;
// saving always writes v2. The marker is what lets an operator (and a
// test) tell a pre-ownership index from a current one.
type indexEnvelope struct {
	Version int    `json:"version"`
	Files   []File `json:"files"`
}

// indexVersion is the format saveIndexLocked writes.
const indexVersion = 2

// loadIndexLocked reads the index; a missing index is an empty
// store, not an error. A corrupt index is an error — silently
// dropping metadata would orphan content files.
func (s *Store) loadIndexLocked() ([]File, error) {
	raw, err := os.ReadFile(s.indexPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("files: read index: %w", err)
	}
	var env indexEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Version > 0 {
		s.legacyIndex = env.Version < indexVersion
		return env.Files, nil
	}
	// Either the v1 bare array, or something genuinely unreadable.
	var legacy []File
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, fmt.Errorf("files: corrupt index %s: %w", s.indexPath(), err)
	}
	s.legacyIndex = true
	return legacy, nil
}

// legacyIndexLoaded reports whether the index on disk predates ownership
// scoping (a v1 bare array) — the caller logs it once, so an operator
// learns that unowned records exist instead of wondering where their
// older uploads went.
func (s *Store) legacyIndexLoaded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.loadIndexLocked()
	return err == nil && s.legacyIndex
}

// LegacyUnowned lists records that have no owner: they predate owner
// scoping, or were written by a path that could not identify its caller.
// They are NOT visible to any authenticated key (see visibleTo — an
// empty caller id matches only these, and a real key id never does), so
// this listing is the forensic view an administrator needs before
// reclaiming them with ReclaimLegacy. It never mutates anything.
func (s *Store) LegacyUnowned() ([]File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.loadIndexLocked()
	if err != nil {
		return nil, err
	}
	out := make([]File, 0)
	for _, f := range idx {
		if f.OwnerKeyID == "" {
			out = append(out, f)
		}
	}
	return out, nil
}

// ReclaimLegacy assigns ownership of unowned records to ownerKeyID —
// the supported migration for files that predate owner scoping. An
// empty owner is refused: that is exactly the state being repaired, and
// accepting it would let the wildcard state back in. With no ids, every
// unowned record is reclaimed; with ids, only those (and only if they
// are in fact unowned — a record owned by another key is never
// reassigned here, so this cannot be used to steal a file). Returns how
// many were reclaimed and how many unowned records remain.
func (s *Store) ReclaimLegacy(ownerKeyID string, ids []string) (int, int, error) {
	if strings.TrimSpace(ownerKeyID) == "" {
		return 0, 0, errors.New("files: reclaim requires a non-empty owner key id")
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !ValidID(id) {
			return 0, 0, fmt.Errorf("files: reclaim: invalid file id %q", id)
		}
		want[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.loadIndexLocked()
	if err != nil {
		return 0, 0, err
	}
	reclaimed := 0
	for i := range idx {
		if idx[i].OwnerKeyID != "" {
			continue
		}
		if len(want) > 0 && !want[idx[i].ID] {
			continue
		}
		idx[i].OwnerKeyID = ownerKeyID
		reclaimed++
	}
	if reclaimed > 0 {
		if err := s.saveIndexLocked(idx); err != nil {
			return 0, 0, err
		}
	}
	remaining := 0
	for _, f := range idx {
		if f.OwnerKeyID == "" {
			remaining++
		}
	}
	return reclaimed, remaining, nil
}

// saveIndexLocked writes the index atomically (temp file in the
// same directory + rename) and fsyncs both the file and the
// directory, so a recorded file is durable, not merely buffered.
func (s *Store) saveIndexLocked(idx []File) error {
	raw, err := json.Marshal(indexEnvelope{Version: indexVersion, Files: idx})
	if err != nil {
		return fmt.Errorf("files: encode index: %w", err)
	}
	if idx == nil {
		raw, err = json.Marshal(indexEnvelope{Version: indexVersion, Files: []File{}})
		if err != nil {
			return fmt.Errorf("files: encode index: %w", err)
		}
	}
	s.legacyIndex = false
	tmp, err := os.CreateTemp(s.root, "index-*.tmp")
	if err != nil {
		return fmt.Errorf("files: write index: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("files: write index: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("files: fsync index: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("files: write index: %w", err)
	}
	if err := os.Rename(tmpName, s.indexPath()); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("files: commit index: %w", err)
	}
	return fsyncDir(s.root)
}

// fsyncDir flushes a directory entry to disk (best effort on
// platforms that do not support it — the rename above is already
// the atomicity guarantee; the fsync is the durability one).
// On Windows the directory fsync is skipped: Go opens directories
// without FILE_FLAG_BACKUP_SEMANTICS, and FlushFileBuffers rejects
// such a handle with "Access is denied". The content and index
// files are still fsync'd individually, which is the durability
// that matters; the directory entry is best-effort only.
func fsyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Create stores src under a fresh id with the given filename and
// purpose. The content is streamed to a temp file, fsync'd, then
// renamed into place BEFORE the index records it, so the index
// never points at missing content; if the index write then fails,
// the content file is removed again.
// Create stores a file with no owner. Prefer CreateOwned: an unowned
// file is only visible to unowned callers (AUD-SEC-010).
func (s *Store) Create(filename, purpose string, src io.Reader) (*File, error) {
	return s.CreateOwned(filename, purpose, "", src)
}

// CreateOwned stores a file owned by owner (the uploading key's id, or
// "" for a genuinely unowned upload).
func (s *Store) CreateOwned(filename, purpose, owner string, src io.Reader) (*File, error) {
	if !ValidPurpose(purpose) {
		return nil, fmt.Errorf("%w: %q (valid: %s)", ErrInvalidPurpose, purpose, strings.Join(ValidPurposes, ", "))
	}
	clean, err := SanitizeFilename(filename)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", err, filename)
	}
	if src == nil {
		return nil, fmt.Errorf("files: no content")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return nil, fmt.Errorf("files: create store dir: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tmp, err := os.CreateTemp(s.root, "upload-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("files: stage upload: %w", err)
	}
	tmpName := tmp.Name()
	n, copyErr := io.Copy(tmp, src)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("files: store upload: copy=%v fsync=%v close=%v", copyErr, syncErr, closeErr)
	}
	// P2-SEC-05 / AUD-SEC-006: reject before committing if this upload
	// would exceed the per-key or aggregate disk quota.
	if s.keyBytes[owner]+n > maxKeyDiskBytes {
		_ = os.Remove(tmpName)
		return nil, ErrDiskQuota
	}
	if s.totalBytes+n > maxDiskBytes {
		_ = os.Remove(tmpName)
		return nil, ErrDiskQuota
	}
	if err := os.Rename(tmpName, s.contentPath(id)); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("files: commit upload: %w", err)
	}
	if err := fsyncDir(s.root); err != nil {
		_ = os.Remove(s.contentPath(id))
		return nil, fmt.Errorf("files: fsync store dir: %w", err)
	}

	f := File{
		ID:         id,
		Object:     "file",
		Bytes:      n,
		CreatedAt:  time.Now().Unix(),
		Filename:   clean,
		Purpose:    purpose,
		Status:     "processed",
		OwnerKeyID: owner,
	}
	idx, err := s.loadIndexLocked()
	if err != nil {
		_ = os.Remove(s.contentPath(id))
		return nil, err
	}
	idx = append(idx, f)
	if err := s.saveIndexLocked(idx); err != nil {
		_ = os.Remove(s.contentPath(id))
		return nil, err
	}
	// Quota accounting: record the committed bytes only after the index
	// successfully reflects them, so the counter never leads the store.
	s.keyBytes[owner] += n
	s.totalBytes += n
	return &f, nil
}

// ServerGeneratedPurposes are the file purposes only the server
// itself may mint (M8.8 Batch API output/error files, and the
// other server-generated outputs OpenAI names): they are never
// accepted on upload — Create still rejects them — because a
// client claiming its upload IS a batch output would be lying
// about provenance. CreateServerGenerated is the only path that
// writes them.
var ServerGeneratedPurposes = []string{"batch_output", "batch_error", "assistants_output", "fine-tune-results"}

// ValidServerGeneratedPurpose reports whether purpose is one of
// ServerGeneratedPurposes.
func ValidServerGeneratedPurpose(purpose string) bool {
	for _, p := range ServerGeneratedPurposes {
		if p == purpose {
			return true
		}
	}
	return false
}

// CreateServerGenerated stores src under a fresh id exactly like
// Create, except the purpose must be a server-generated purpose
// (see ServerGeneratedPurposes) instead of an upload purpose.
// Callers are server internals (the Batch worker writing its
// output/error .jsonl files); it is never reachable from an
// upload endpoint.
// CreateServerGenerated stores a server-generated file with no owner.
// Prefer CreateServerGeneratedOwned (AUD-SEC-010).
func (s *Store) CreateServerGenerated(filename, purpose string, src io.Reader) (*File, error) {
	return s.CreateServerGeneratedOwned(filename, purpose, "", src)
}

// CreateServerGeneratedOwned stores a server-generated file owned by
// owner — the batch owner for a batch output/error file.
func (s *Store) CreateServerGeneratedOwned(filename, purpose, owner string, src io.Reader) (*File, error) {
	if !ValidServerGeneratedPurpose(purpose) {
		return nil, fmt.Errorf("%w: %q (server-generated purposes: %s)", ErrInvalidPurpose, purpose, strings.Join(ServerGeneratedPurposes, ", "))
	}
	clean, err := SanitizeFilename(filename)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", err, filename)
	}
	if src == nil {
		return nil, fmt.Errorf("files: no content")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return nil, fmt.Errorf("files: create store dir: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tmp, err := os.CreateTemp(s.root, "upload-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("files: stage upload: %w", err)
	}
	tmpName := tmp.Name()
	n, copyErr := io.Copy(tmp, src)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("files: store upload: copy=%v fsync=%v close=%v", copyErr, syncErr, closeErr)
	}
	// P2-SEC-05 / AUD-SEC-006: reject before committing if this upload
	// would exceed the per-key or aggregate disk quota.
	if s.keyBytes[owner]+n > maxKeyDiskBytes {
		_ = os.Remove(tmpName)
		return nil, ErrDiskQuota
	}
	if s.totalBytes+n > maxDiskBytes {
		_ = os.Remove(tmpName)
		return nil, ErrDiskQuota
	}
	if err := os.Rename(tmpName, s.contentPath(id)); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("files: commit upload: %w", err)
	}
	if err := fsyncDir(s.root); err != nil {
		_ = os.Remove(s.contentPath(id))
		return nil, fmt.Errorf("files: fsync store dir: %w", err)
	}

	f := File{
		ID:         id,
		Object:     "file",
		Bytes:      n,
		CreatedAt:  time.Now().Unix(),
		Filename:   clean,
		Purpose:    purpose,
		Status:     "processed",
		OwnerKeyID: owner,
	}
	idx, err := s.loadIndexLocked()
	if err != nil {
		_ = os.Remove(s.contentPath(id))
		return nil, err
	}
	idx = append(idx, f)
	if err := s.saveIndexLocked(idx); err != nil {
		_ = os.Remove(s.contentPath(id))
		return nil, err
	}
	// Quota accounting: record the committed bytes only after the index
	// successfully reflects them, so the counter never leads the store.
	s.keyBytes[owner] += n
	s.totalBytes += n
	return &f, nil
}

// Get returns the metadata for id, or ErrNotFound. An id that is
// not a store-issued shape is ErrInvalidID — it never reaches a
// path lookup.
// Get returns a file's metadata, unowned-caller scoped.
// Prefer GetFor for request paths (AUD-SEC-010).
func (s *Store) Get(id string) (*File, error) { return s.GetFor(id, "") }

// GetFor returns a file's metadata when callerID may see it. A file
// owned by another key is reported exactly like a missing one, so the
// response never confirms that a foreign file exists.
func (s *Store) GetFor(id, callerID string) (*File, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.loadIndexLocked()
	if err != nil {
		return nil, err
	}
	for i := range idx {
		if idx[i].ID == id && visibleTo(idx[i], callerID) {
			f := idx[i]
			return &f, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
}

// List returns the unowned caller's files. Prefer ListFor for request
// paths (AUD-SEC-010).
func (s *Store) List() ([]File, error) { return s.ListFor("") }

// ListFor returns the files callerID may see, newest first
// (created_at desc, id desc as a stable tie-break), matching the
// OpenAI list ordering.
func (s *Store) ListFor(callerID string) ([]File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.loadIndexLocked()
	if err != nil {
		return nil, err
	}
	kept := make([]File, 0, len(idx))
	for _, f := range idx {
		if visibleTo(f, callerID) {
			kept = append(kept, f)
		}
	}
	idx = kept
	if idx == nil {
		idx = []File{}
	}
	sort.Slice(idx, func(i, j int) bool {
		if idx[i].CreatedAt != idx[j].CreatedAt {
			return idx[i].CreatedAt > idx[j].CreatedAt
		}
		return idx[i].ID > idx[j].ID
	})
	return idx, nil
}

// Delete removes the file and its index entry. It reports whether
// anything was deleted; deleting an unknown id is not an error
// here (the HTTP layer answers 404 — see internal/server) but
// returns false.
// Delete removes an unowned file. Prefer DeleteFor on request paths
// (AUD-SEC-010).
func (s *Store) Delete(id string) (bool, error) { return s.DeleteFor(id, "") }

// DeleteFor removes a file the caller owns. A foreign id reports
// "nothing deleted" (the HTTP layer answers 404), so a key can neither
// delete another key's file nor learn that it exists.
func (s *Store) DeleteFor(id, callerID string) (bool, error) {
	if !ValidID(id) {
		return false, fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.loadIndexLocked()
	if err != nil {
		return false, err
	}
	kept := idx[:0]
	found := false
	var freedBytes int64
	var freedOwner string
	for _, f := range idx {
		if f.ID == id {
			if !visibleTo(f, callerID) {
				// Keep a foreign file in the index: we are not
				// entitled to remove it, and reporting "not found"
				// must not have side effects.
				kept = append(kept, f)
				continue
			}
			found = true
			freedBytes = f.Bytes
			freedOwner = f.OwnerKeyID
			continue
		}
		kept = append(kept, f)
	}
	if !found {
		return false, nil
	}
	// Remove content first, then the index entry: a crash between
	// the two leaves at most an orphaned content file with no
	// index entry (unreachable), never an index entry pointing at
	// missing content.
	if err := os.Remove(s.contentPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("files: remove content: %w", err)
	}
	if err := s.saveIndexLocked(kept); err != nil {
		return false, err
	}
	// Free quota accounting: return the deleted file's bytes to the
	// per-key and aggregate counters so future uploads can use them.
	s.keyBytes[freedOwner] -= freedBytes
	if s.keyBytes[freedOwner] < 0 {
		s.keyBytes[freedOwner] = 0
	}
	s.totalBytes -= freedBytes
	if s.totalBytes < 0 {
		s.totalBytes = 0
	}
	return true, nil
}

// Usage returns the aggregate bytes of all content files in this store
// (P2-SEC-05 / AUD-SEC-006).
func (s *Store) Usage() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalBytes
}

// KeyUsage returns the bytes of content files owned by key (P2-SEC-05 /
// AUD-SEC-006).
func (s *Store) KeyUsage(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keyBytes[key]
}

// Open returns a reader for the file's content plus its metadata.
// The content path is built from the validated id only — the
// caller can never steer it with a user-supplied path.
// Open returns a reader for an unowned file. Prefer OpenFor on request
// paths (AUD-SEC-010).
func (s *Store) Open(id string) (io.ReadCloser, *File, error) { return s.OpenFor(id, "") }

// OpenFor returns a reader for a file the caller may see, plus its
// metadata. The content path is built from the validated id only — the
// caller can never steer it with a user-supplied path.
func (s *Store) OpenFor(id, callerID string) (io.ReadCloser, *File, error) {
	f, err := s.GetFor(id, callerID)
	if err != nil {
		return nil, nil, err
	}
	rc, err := os.Open(s.contentPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %q (index entry without content)", ErrNotFound, id)
		}
		return nil, nil, fmt.Errorf("files: open content: %w", err)
	}
	return rc, f, nil
}
