// Files API (M8.7 — POST/GET/DELETE /v1/files, GET
// /v1/files/{id}/content; Area 14 §14a "Files" row).
//
// OpenAI-compatible file objects backed by a LOCAL DISK store
// only (internal/files): an upload is written under the router's
// data dir (<data-dir>/files/) and never leaves the machine —
// there is no upstream upload, no provider call, and no network
// access. That is the whole point of this slice: the Batch API
// (M8.8) needs a local place to hold its input .jsonl files, and
// fine-tuning/evals need the same.
//
// Route registration and auth live with the coordinator (guard.go
// / server.go New() — NOT this file): every route below is
// TierManagement, gateway-key auth, proposed scope "chat" (the
// Responses object-store routes use "chat" for the same
// read/write-over-stored-objects shape; see FilesRoutes).
// Handlers here assume auth already happened; they answer 503
// only when the server has no config (hence no data dir to store
// under).
//
// Shapes follow the OpenAI Files API: a file object is
// {id, object: "file", bytes, created_at, filename, purpose,
// status: "processed"}; the list is {object: "list", data: [...]};
// delete returns {id, object: "file", deleted: true}. Unknown ids
// answer 404 — never a silent success. The content endpoint
// streams the stored bytes as application/octet-stream.
//
// UPLOAD-SIZE CAP (M8.7 review verdict, landed here): one file is
// capped at filesMaxUploadBytes = 512 MiB, enforced with
// http.MaxBytesReader around the body while the multipart framing
// is streamed (multipart.Reader), answering 413 on overflow. The
// value is not invented: the OpenAI reference states "Individual
// files can be up to 512 MB", and the reference router enforces the same
// 512 MB cap (src/app/api/v1/files/route.ts: 120,
// MAX_FILE_BYTES = 512 * 1024 * 1024). The cap covers the whole
// request body, as it did when ParseMultipartForm was the parser
// — the rewrite changed how the body is read, not what the cap
// measures.
//
// DELIBERATE DIVERGENCE from the reference router (better, not a copy):
// the reference router checks file.size AFTER parsing the whole multipart
// body (route.ts:120-130) and then buffers the ENTIRE file in
// RAM (Buffer.from(await file.arrayBuffer()), route.ts:134),
// so a 512 MB upload costs 512 MB of heap before the cap can
// even apply — and it answers 400, not 413, for an over-limit
// file. This gateway refuses while streaming (the reader errors
// past the cap, before the store ever sees the bytes), never
// holds the whole file in RAM (the store streams to disk), and
// answers 413 Request Entity Too Large, the correct semantic
// for a payload over the limit.
//
// STREAMING (G3(c) follow-up, landed here): the upload path no
// longer calls ParseMultipartForm, which spilled every part over
// 32 MiB to a temp file under os.TempDir() (/tmp, a 512M tmpfs
// on this VM) — an upload near the cap could fail at parse time
// on a small /tmp even with data-dir space available. The
// handler now reads the framing itself with multipart.Reader:
// a file part whose purpose is already known streams straight
// into the store (which stages under the data dir and renames
// into place), so no temp file is created anywhere outside the
// data dir and nothing is ever held whole in RAM. The one
// exception is field order: multipart is forward-only, and the
// store needs the purpose before it will accept a file, so a
// file part that arrives BEFORE the purpose field is spooled to
// a temp file under the store dir (<data-dir>/files, never
// os.TempDir()) until the purpose arrives; the spool is removed
// on every exit path (success, rejection, disconnect, parse
// error) by a deferred Remove in handleFilesUpload, and
// Store.Create removes its own staging temp on every in-process
// copy failure. A process crash (SIGKILL) mid-copy can still
// leave an upload-*.tmp spool or staging temp behind — no
// deferred Remove runs then. Those orphans are reaped by the
// startup sweeper: the first filesStore() call in a process runs
// Store.SweepStaleUploadTemps, which removes only upload-*.tmp
// files unmodified for over StaleUploadTempAge (1h) — a live
// upload's temp is written continuously, so its mtime is never
// that old — and runs once per store dir, before any upload in
// this process can start.
package server

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/raunakiitp/codvian-router/internal/files"
)

// filesMaxPurposeBytes bounds a purpose field read while
// streaming: a purpose is one of six short strings, so anything
// over 1 KiB is not a purpose at all and is rejected as invalid
// rather than read without limit.
const filesMaxPurposeBytes = 1024

// filesMaxUploadBytes caps one uploaded file at 512 MiB (M8.7
// review verdict). The value comes from the OpenAI reference
// ("Individual files can be up to 512 MB") and matches
// the reference router's MAX_FILE_BYTES (512 * 1024 * 1024); it is
// enforced with http.MaxBytesReader while streaming with multipart.Reader
// and answers 413 on overflow — see the file header for why
// this is stricter and earlier than the reference router's post-parse
// 400. Per-purpose tightening (batch / fine-tune .jsonl
// 200 MB) can follow in M8.8.
const filesMaxUploadBytes = 512 << 20

// filesRoute describes one files route for the coordinator to
// register in guard.go (TierManagement, gateway-key auth) and
// server.go New(). Scope is the proposal; the coordinator assigns
// the final scope at registration.
type filesRoute struct {
	Method      string
	Path        string
	Scope       string // proposed gateway-key scope
	Description string
}

// FilesRoutes returns the five M8.7 routes with their proposed
// scopes. All propose "chat": files are inputs to chat-adjacent
// work (batch input, fine-tune data, eval sets), and the closest
// existing analogue — the Responses stored-object API
// (POST/GET/list/DELETE /v1/responses) — scopes every one of its
// routes "chat" as well.
func FilesRoutes() []filesRoute {
	return []filesRoute{
		{Method: "POST", Path: "/v1/files", Scope: "chat", Description: "Upload a file (multipart form-data: file + purpose) to the local disk store; returns the OpenAI-shaped file object."},
		{Method: "GET", Path: "/v1/files", Scope: "chat", Description: "List stored files, newest first."},
		{Method: "GET", Path: "/v1/files/{id}", Scope: "chat", Description: "Retrieve one stored file's metadata by id; unknown ids answer 404."},
		{Method: "DELETE", Path: "/v1/files/{id}", Scope: "chat", Description: "Delete one stored file (content + index entry); unknown ids answer 404, never a silent success."},
		{Method: "GET", Path: "/v1/files/{id}/content", Scope: "chat", Description: "Download one stored file's bytes; unknown ids answer 404."},
	}
}

// filesStores caches one files.Store per data dir, so concurrent
// requests share the store's in-process index mutex instead of
// each opening a fresh one. Keyed by the resolved files dir.
var filesStores sync.Map // string -> *files.Store

// filesStore returns the local disk store for this server's data
// dir (<data-dir>/files), creating it on first use. A server with
// no config has no data dir and cannot store files (503). The
// first use of a store dir in this process also runs the orphan
// sweeper once (Store.SweepStaleUploadTemps): any upload-*.tmp
// left by a crashed earlier process is reaped then, before any
// upload in this process can start, so the sweep can never meet
// a live temp. A sweep failure is logged, never fatal — a stale
// temp is hygiene, not a reason to refuse files.
func (s *Server) filesStore() (*files.Store, error) {
	if s.cfg == nil || s.cfg.DataDir == "" {
		return nil, errors.New("files unavailable: no config (no data dir) configured")
	}
	dir := filepath.Join(s.cfg.DataDir, "files")
	if st, ok := filesStores.Load(dir); ok {
		return st.(*files.Store), nil
	}
	actual, loaded := filesStores.LoadOrStore(dir, files.New(dir))
	st := actual.(*files.Store)
	if !loaded {
		if unowned, err := st.LegacyUnowned(); err != nil {
			s.logger.Printf("files: could not read %s while checking for pre-ownership records: %v", dir, err)
		} else if len(unowned) > 0 {
			// Operational and forensic visibility: these records are
			// private (no key can see them) but they must not be
			// invisible to the operator, or legitimate older uploads
			// would look lost (AUD-SEC-010).
			s.logger.Printf("files: %d file record(s) under %s predate ownership scoping and have NO owner; they are not visible to any gateway key. List them with GET /admin/files/legacy and assign them with POST /admin/files/reclaim", len(unowned), dir)
		}
		if n, err := st.SweepStaleUploadTemps(files.StaleUploadTempAge); err != nil {
			s.logger.Printf("files: orphan staging sweep failed: %v (removed %d stale upload temp(s) before error)", err, n)
		} else if n > 0 {
			s.logger.Printf("files: orphan staging sweep removed %d stale upload temp(s) under %s", n, dir)
		}
	}
	return st, nil
}

// filesListResponse is the GET /v1/files payload: OpenAI's
// {object: "list", data: [...]} shape. Data is never null — an
// empty store answers an empty array.
type filesListResponse struct {
	Object string       `json:"object"`
	Data   []files.File `json:"data"`
}

// filesDeleteResponse is the DELETE /v1/files/{id} payload.
type filesDeleteResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

// handleFiles is the dispatcher for the whole /v1/files surface:
// POST /v1/files (upload), GET /v1/files (list),
// GET|DELETE /v1/files/{id}, GET /v1/files/{id}/content. The
// coordinator may register it once per path pattern or call the
// individual handleFiles* methods; both go through here.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/files")
	rest = strings.TrimPrefix(rest, "/")
	switch {
	case rest == "":
		switch r.Method {
		case http.MethodPost:
			s.handleFilesUpload(w, r)
		case http.MethodGet:
			s.handleFilesList(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case strings.HasSuffix(rest, "/content"):
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleFilesContent(w, r, strings.TrimSuffix(rest, "/content"))
	default:
		switch r.Method {
		case http.MethodGet:
			s.handleFilesGet(w, r, rest)
		case http.MethodDelete:
			s.handleFilesDelete(w, r, rest)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

// filesStreamBody wraps the capped upload body and remembers
// the first non-EOF read error, so the handler can still tell a
// 512 MiB cap overflow (413) from a client disconnect or corrupt
// framing (400) after the error has passed through io.Copy and
// the store, which re-wrap it as plain text (their %v, not %w).
// Close closes the original request body; the MaxBytesReader
// itself needs no closing.
type filesStreamBody struct {
	r   io.Reader
	rc  io.Closer
	err error
}

func (b *filesStreamBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF && b.err == nil {
		b.err = err
	}
	return n, err
}

func (b *filesStreamBody) Close() error {
	if b.rc != nil {
		return b.rc.Close()
	}
	return nil
}

// filesUploadReadError answers a failure while streaming the
// multipart body: 413 when the read failed because the body
// passed the 512 MiB cap, 400 for a disconnect, corrupt framing,
// or any other read failure.
func filesUploadReadError(w http.ResponseWriter, body *filesStreamBody, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) || errors.As(body.err, &mbe) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large: one file is capped at 512 MiB")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid request: expected a multipart upload with fields \"file\" (the file) and \"purpose\"")
}

// filesUploadStoreError maps a store.Create failure to the upload
// response. A cap overflow or a stream failure mid-copy is not a
// server failure: the cap is 413, a disconnect/corrupt stream is
// 400 (the store already removed its staging temp in both
// cases). A store validation failure is 400; anything else is a
// genuine 500.
func (s *Server) filesUploadStoreError(w http.ResponseWriter, body *filesStreamBody, err error) {
	var mbe *http.MaxBytesError
	if errors.As(body.err, &mbe) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large: one file is capped at 512 MiB")
		return
	}
	if body.err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: expected a multipart upload with fields \"file\" (the file) and \"purpose\"")
		return
	}
	if errors.Is(err, files.ErrInvalidPurpose) || errors.Is(err, files.ErrInvalidFilename) {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	s.logger.Printf("files upload: store failed: %v", err)
	writeServerError(w, http.StatusInternalServerError, "could not store the uploaded file")
}

// handleFilesUpload serves POST /v1/files: multipart form-data
// with fields `file` (the upload) and `purpose` (one of the six
// OpenAI upload purposes). The file is stored locally and the
// OpenAI-shaped file object is returned with HTTP 200.
//
// The body is streamed with multipart.Reader — never parsed with
// ParseMultipartForm, never held whole in RAM, and never spilled
// to os.TempDir(). The 512 MiB cap (filesMaxUploadBytes) is
// enforced by http.MaxBytesReader on the whole body while
// streaming, exactly as before: over-cap answers 413. Field
// order does not matter (multipart is forward-only, so a file
// part that arrives before the purpose field is spooled to a
// temp file under the store dir until the purpose is known —
// see the file header); the first `file` part and the first
// `purpose` field win, as with FormFile/FormValue before.
// Unknown fields are ignored. Every validation the old path did
// is preserved: missing/invalid purpose is 400, a missing file
// part is 400, an invalid filename is 400, and a non-multipart
// body is 400.
func (s *Server) handleFilesUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// AUD-SEC-010: the upload is owned by the authenticated key.
	owner, ok := s.callerForOwnership(w, r)
	if !ok {
		return
	}
	st, err := s.filesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	// The 512 MiB cap is enforced here, on the whole request
	// body, while streaming: the reader errors past the cap, so
	// an over-limit upload is refused mid-stream instead of
	// being buffered and then rejected. filesStreamBody records
	// the raw read error so a cap overflow can still be told
	// apart from a disconnect after io.Copy / the store wrap it.
	body := &filesStreamBody{r: http.MaxBytesReader(w, r.Body, filesMaxUploadBytes), rc: r.Body}
	r.Body = body
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: expected a multipart upload with fields \"file\" (the file) and \"purpose\"")
		return
	}

	var (
		purpose     string
		purposeSeen bool
		sawFile     bool
		spoolPath   string // data-dir temp holding a file part that arrived before its purpose
		spoolName   string // that part's filename, committed with it
	)
	// A spooled file part is removed on EVERY exit path —
	// success, rejection, disconnect, parse error — so no temp
	// survives the request. (The purpose-first path never
	// creates one; the store removes its own staging temp on
	// copy failure.)
	defer func() {
		if spoolPath != "" {
			_ = os.Remove(spoolPath)
		}
	}()

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			filesUploadReadError(w, body, err)
			return
		}
		switch part.FormName() {
		case "purpose":
			if purposeSeen {
				continue // first purpose wins, as FormValue did
			}
			raw, err := io.ReadAll(io.LimitReader(part, filesMaxPurposeBytes+1))
			if err != nil {
				filesUploadReadError(w, body, err)
				return
			}
			if len(raw) > filesMaxPurposeBytes {
				writeError(w, http.StatusBadRequest, "invalid request: purpose is not a valid file purpose (one of: "+strings.Join(files.ValidPurposes, ", ")+")")
				return
			}
			purpose, purposeSeen = string(raw), true
			if !files.ValidPurpose(purpose) {
				writeError(w, http.StatusBadRequest, "invalid request: purpose "+quoteForError(purpose)+" is not a valid file purpose (one of: "+strings.Join(files.ValidPurposes, ", ")+")")
				return
			}
		case "file":
			if sawFile {
				continue // first file part wins, as FormFile did; NextPart skips the rest
			}
			sawFile = true
			if purposeSeen {
				// Purpose first (the usual order): stream the
				// part straight into the store — no spool, no
				// temp, no whole-file buffering.
				f, err := st.CreateOwned(part.FileName(), purpose, owner, part)
				if err != nil {
					s.filesUploadStoreError(w, body, err)
					return
				}
				writeJSON(w, http.StatusOK, f)
				return
			}
			// File before purpose: multipart is forward-only
			// and the store needs the purpose to accept the
			// file, so the bytes must wait somewhere until
			// the purpose field arrives. That somewhere is a
			// temp file under the store dir — never
			// os.TempDir() — removed by the deferred Remove
			// above on every exit path.
			if err := os.MkdirAll(st.Root(), 0o700); err != nil {
				s.logger.Printf("files upload: spool dir failed: %v", err)
				writeServerError(w, http.StatusInternalServerError, "could not store the uploaded file")
				return
			}
			tmp, err := os.CreateTemp(st.Root(), "upload-*.tmp")
			if err != nil {
				s.logger.Printf("files upload: spool failed: %v", err)
				writeServerError(w, http.StatusInternalServerError, "could not store the uploaded file")
				return
			}
			spoolPath, spoolName = tmp.Name(), part.FileName()
			if _, err := io.Copy(tmp, part); err != nil {
				_ = tmp.Close()
				filesUploadReadError(w, body, err)
				return
			}
			if err := tmp.Close(); err != nil {
				s.logger.Printf("files upload: spool close failed: %v", err)
				writeServerError(w, http.StatusInternalServerError, "could not store the uploaded file")
				return
			}
		default:
			// Unknown field: ignored, as before; NextPart
			// skips its unread content.
		}
	}

	if !sawFile {
		writeError(w, http.StatusBadRequest, "invalid request: no \"file\" field — upload the file as multipart form-data")
		return
	}
	if spoolPath == "" {
		// Unreachable: a seen file part either streamed straight
		// into the store (purpose known, returned above) or was
		// spooled (purpose unknown). Kept as a guard, not a
		// silent success.
		writeServerError(w, http.StatusInternalServerError, "could not store the uploaded file")
		return
	}
	if !purposeSeen {
		writeError(w, http.StatusBadRequest, "invalid request: missing \"purpose\" (one of: "+strings.Join(files.ValidPurposes, ", ")+")")
		return
	}
	// File arrived before purpose: commit the spooled part now
	// that the purpose is known and validated. The spool itself
	// is removed by the deferred Remove above.
	src, err := os.Open(spoolPath)
	if err != nil {
		s.logger.Printf("files upload: spool open failed: %v", err)
		writeServerError(w, http.StatusInternalServerError, "could not store the uploaded file")
		return
	}
	defer src.Close()
	f, err := st.CreateOwned(spoolName, purpose, owner, src)
	if err != nil {
		s.filesUploadStoreError(w, body, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// handleFilesList serves GET /v1/files: all stored files, newest
// first. `after`/`limit` pagination is NOT implemented — OpenAI
// supports it, this store does not yet; the response carries the
// full list, and that gap is stated here, not hidden behind a
// silently-ignored query param. (M8.8 batches will want `limit`;
// adding it is a follow-up, not a silent default.)
func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, ok := s.callerForOwnership(w, r)
	if !ok {
		return
	}
	st, err := s.filesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	list, err := st.ListFor(owner)
	if err != nil {
		s.logger.Printf("files list: %v", err)
		writeServerError(w, http.StatusInternalServerError, "could not list stored files")
		return
	}
	writeJSON(w, http.StatusOK, filesListResponse{Object: "list", Data: list})
}

// handleFilesGet serves GET /v1/files/{id}: the file object for
// one stored file; unknown ids answer 404.
func (s *Server) handleFilesGet(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, ok := s.callerForOwnership(w, r)
	if !ok {
		return
	}
	st, err := s.filesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	f, err := s.filesGet(w, st, id, owner)
	if err != nil {
		return // s.filesGet already wrote the error
	}
	writeJSON(w, http.StatusOK, f)
}

// handleFilesDelete serves DELETE /v1/files/{id}: removes the
// content and its index entry; unknown ids answer 404.
func (s *Server) handleFilesDelete(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, ok := s.callerForOwnership(w, r)
	if !ok {
		return
	}
	st, err := s.filesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if !files.ValidID(id) {
		writeError(w, http.StatusNotFound, "file not found: "+id)
		return
	}
	deleted, err := st.DeleteFor(id, owner)
	if err != nil {
		s.logger.Printf("files delete: %v", err)
		writeServerError(w, http.StatusInternalServerError, "could not delete the stored file")
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "file not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, filesDeleteResponse{ID: id, Object: "file", Deleted: true})
}

// handleFilesContent serves GET /v1/files/{id}/content: the stored
// bytes as application/octet-stream. Unknown ids answer 404.
func (s *Server) handleFilesContent(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, ok := s.callerForOwnership(w, r)
	if !ok {
		return
	}
	st, err := s.filesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	rc, f, err := st.OpenFor(id, owner)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) || errors.Is(err, files.ErrInvalidID) {
			writeError(w, http.StatusNotFound, "file not found: "+id)
			return
		}
		s.logger.Printf("files content: %v", err)
		writeServerError(w, http.StatusInternalServerError, "could not read the stored file")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(f.Filename))
	w.Header().Set("Content-Length", strconv.FormatInt(f.Bytes, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		s.logger.Printf("files content: stream failed for %s: %v", f.ID, err)
	}
}

// filesGet is the shared id lookup for handleFilesGet: an invalid
// or unknown id writes the 404 and returns (nil, error) so the
// caller just returns.
func (s *Server) filesGet(w http.ResponseWriter, st *files.Store, id, owner string) (*files.File, error) {
	f, err := st.GetFor(id, owner)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) || errors.Is(err, files.ErrInvalidID) {
			writeError(w, http.StatusNotFound, "file not found: "+id)
			return nil, err
		}
		s.logger.Printf("files get: %v", err)
		writeServerError(w, http.StatusInternalServerError, "could not read the stored file")
		return nil, err
	}
	return f, nil
}

// quoteForError renders a short user-supplied value inside an
// error message without letting newlines or quotes break the JSON
// error body (the encoder would escape them anyway; this keeps
// the message one line and readable).
func quoteForError(v string) string {
	v = strings.ReplaceAll(v, "\n", " ")
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, `"`, "'")
	if len(v) > 64 {
		v = v[:64] + "…"
	}
	return `"` + v + `"`
}
