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
// OPEN LIMIT (flagged, not invented): there is no upload-size cap.
// No generic file-upload size limit exists anywhere in this
// codebase (the only upload cap, dbImportMaxBytes 10 MiB in
// internal/server/dbimport.go, is specific to DB export files and
// was not copied here — copying it would invent a files limit).
// Uploads stream to a temp file on disk rather than memory, so an
// upload is bounded by disk space, not RAM; a real cap (e.g. the
// 200 MiB OpenAI Batch limit, or a per-purpose limit) is a
// coordinator decision for M8.8 or later.
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
}

// New returns a store rooted at dir. The directory (0700, like the
// router's data dir) is created on first write, not here, so
// merely constructing a store never touches disk.
func New(dir string) *Store { return &Store{root: dir} }

// Root returns the store's root directory.
func (s *Store) Root() string { return s.root }

// StaleUploadTempAge is how long a staging temp must sit
// unmodified before the orphan sweeper treats it as abandoned
// (see SweepStaleUploadTemps). It covers both orphan classes:
// upload-*.tmp staging files and index-*.tmp atomic-index temps
// (see saveIndexLocked). One hour is deliberately generous: a
// live upload writes continuously, so its staging temp's mtime
// is never an hour old — even a 512 MiB upload over a slow link
// keeps touching the file while bytes flow — and an index write
// is a single short write+rename, so a live index temp is never
// an hour old either. Only a temp no process is writing to any
// more (a crash mid-copy or mid-index-write, or a client stalled
// past any plausible resume) goes that long untouched.
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
	var idx []File
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("files: corrupt index %s: %w", s.indexPath(), err)
	}
	return idx, nil
}

// saveIndexLocked writes the index atomically (temp file in the
// same directory + rename) and fsyncs both the file and the
// directory, so a recorded file is durable, not merely buffered.
func (s *Store) saveIndexLocked(idx []File) error {
	raw, err := json.Marshal(idx)
	if err != nil {
		return fmt.Errorf("files: encode index: %w", err)
	}
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
func (s *Store) Create(filename, purpose string, src io.Reader) (*File, error) {
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
	if err := os.Rename(tmpName, s.contentPath(id)); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("files: commit upload: %w", err)
	}
	if err := fsyncDir(s.root); err != nil {
		_ = os.Remove(s.contentPath(id))
		return nil, fmt.Errorf("files: fsync store dir: %w", err)
	}

	f := File{
		ID:        id,
		Object:    "file",
		Bytes:     n,
		CreatedAt: time.Now().Unix(),
		Filename:  clean,
		Purpose:   purpose,
		Status:    "processed",
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
func (s *Store) CreateServerGenerated(filename, purpose string, src io.Reader) (*File, error) {
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
	if err := os.Rename(tmpName, s.contentPath(id)); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("files: commit upload: %w", err)
	}
	if err := fsyncDir(s.root); err != nil {
		_ = os.Remove(s.contentPath(id))
		return nil, fmt.Errorf("files: fsync store dir: %w", err)
	}

	f := File{
		ID:        id,
		Object:    "file",
		Bytes:     n,
		CreatedAt: time.Now().Unix(),
		Filename:  clean,
		Purpose:   purpose,
		Status:    "processed",
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
	return &f, nil
}

// Get returns the metadata for id, or ErrNotFound. An id that is
// not a store-issued shape is ErrInvalidID — it never reaches a
// path lookup.
func (s *Store) Get(id string) (*File, error) {
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
		if idx[i].ID == id {
			f := idx[i]
			return &f, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
}

// List returns all files, newest first (created_at desc, id desc
// as a stable tie-break), matching the OpenAI list ordering.
func (s *Store) List() ([]File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.loadIndexLocked()
	if err != nil {
		return nil, err
	}
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
func (s *Store) Delete(id string) (bool, error) {
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
	for _, f := range idx {
		if f.ID == id {
			found = true
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
	return true, nil
}

// Open returns a reader for the file's content plus its metadata.
// The content path is built from the validated id only — the
// caller can never steer it with a user-supplied path.
func (s *Store) Open(id string) (io.ReadCloser, *File, error) {
	f, err := s.Get(id)
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
