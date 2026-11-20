package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/files"
)

// filesFixtureID is the placeholder id the m8.7 fixtures carry in
// their paths. Store ids are random, so fixture tests seed a file
// and rewrite this placeholder to the seeded id before dispatch
// (see filesFixtureHandler) — the fixture pins the contract
// (shape, status, fields), not the random id.
const filesFixtureID = "file-000000000000000000000001"

// newFilesServer builds a Server whose data dir is a temp dir, so
// the files store is real disk under the test's control. Routes
// are NOT registered in guard.go yet (coordinator's job), so tests
// call srv.handleFiles directly — the same handler the route will
// point at.
func newFilesServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	return New(nil, WithConfig(cfg))
}

// seedFilesFile stores "hello files" (11 bytes) as seed.txt / batch
// and returns its metadata.
func seedFilesFile(t *testing.T, srv *Server) *files.File {
	t.Helper()
	st, err := srv.filesStore()
	if err != nil {
		t.Fatal(err)
	}
	f, err := st.Create("seed.txt", "batch", strings.NewReader("hello files"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// filesFixtureHandler wraps srv.handleFiles for apitest.Run: it
// rewrites the fixture's placeholder id to the seeded file's real
// id, so a fixed fixture path can address the randomly-id'd seed.
func filesFixtureHandler(srv *Server, seed *files.File) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seed != nil {
			r.URL.Path = strings.ReplaceAll(r.URL.Path, filesFixtureID, seed.ID)
		}
		srv.handleFiles(w, r)
	})
}

// doFilesUpload POSTs a multipart upload to handleFiles directly.
func doFilesUpload(t *testing.T, srv *Server, filename, purpose, content string, withFile bool) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if withFile {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if purpose != "" {
		if err := mw.WriteField("purpose", purpose); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.handleFiles(rec, req)
	return rec
}

func decodeFilesObject(t *testing.T, rec *httptest.ResponseRecorder) files.File {
	t.Helper()
	var f files.File
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatalf("response is not a file object: %v: %s", err, rec.Body.String())
	}
	return f
}

// TestFilesUploadRoundTrip: upload -> get -> content -> list ->
// delete -> get 404. The whole slice in one flow.
func TestFilesUploadRoundTrip(t *testing.T) {
	srv := newFilesServer(t)
	rec := doFilesUpload(t, srv, "input.jsonl", "batch", "{\"a\":1}\n", true)
	if rec.Code != 200 {
		t.Fatalf("upload: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	f := decodeFilesObject(t, rec)
	if f.Object != "file" || f.Status != "processed" || f.Filename != "input.jsonl" || f.Purpose != "batch" || f.Bytes != 8 {
		t.Fatalf("upload object = %+v", f)
	}
	if !files.ValidID(f.ID) || f.CreatedAt <= 0 {
		t.Fatalf("upload id/created_at invalid: %+v", f)
	}

	// GET one.
	req := httptest.NewRequest("GET", "/v1/files/"+f.ID, nil)
	getRec := httptest.NewRecorder()
	srv.handleFiles(getRec, req)
	if getRec.Code != 200 {
		t.Fatalf("get: status %d: %s", getRec.Code, getRec.Body.String())
	}
	if got := decodeFilesObject(t, getRec); got != f {
		t.Fatalf("get = %+v, want %+v", got, f)
	}

	// GET content: exact bytes back.
	req = httptest.NewRequest("GET", "/v1/files/"+f.ID+"/content", nil)
	contentRec := httptest.NewRecorder()
	srv.handleFiles(contentRec, req)
	if contentRec.Code != 200 {
		t.Fatalf("content: status %d: %s", contentRec.Code, contentRec.Body.String())
	}
	if contentRec.Body.String() != "{\"a\":1}\n" {
		t.Fatalf("content = %q, want exact uploaded bytes", contentRec.Body.String())
	}
	if ct := contentRec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type = %q, want application/octet-stream", ct)
	}

	// GET list contains it.
	req = httptest.NewRequest("GET", "/v1/files", nil)
	listRec := httptest.NewRecorder()
	srv.handleFiles(listRec, req)
	if listRec.Code != 200 {
		t.Fatalf("list: status %d: %s", listRec.Code, listRec.Body.String())
	}
	var list filesListResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list is not JSON: %v: %s", err, listRec.Body.String())
	}
	if list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != f.ID {
		t.Fatalf("list = %+v", list)
	}

	// DELETE, then GET answers 404.
	req = httptest.NewRequest("DELETE", "/v1/files/"+f.ID, nil)
	delRec := httptest.NewRecorder()
	srv.handleFiles(delRec, req)
	if delRec.Code != 200 {
		t.Fatalf("delete: status %d: %s", delRec.Code, delRec.Body.String())
	}
	var del filesDeleteResponse
	if err := json.Unmarshal(delRec.Body.Bytes(), &del); err != nil {
		t.Fatalf("delete response is not JSON: %v: %s", err, delRec.Body.String())
	}
	if !del.Deleted || del.ID != f.ID || del.Object != "file" {
		t.Fatalf("delete = %+v", del)
	}
	req = httptest.NewRequest("GET", "/v1/files/"+f.ID, nil)
	afterRec := httptest.NewRecorder()
	srv.handleFiles(afterRec, req)
	if afterRec.Code != 404 {
		t.Fatalf("get after delete: status %d, want 404: %s", afterRec.Code, afterRec.Body.String())
	}
}

// repeatReader yields b (here 'a') until limit bytes have been
// read, without materialising them: the 512 MiB cap test streams
// past the cap instead of allocating half a gigabyte.
type repeatReader struct {
	b     byte
	left  int64
	chunk []byte
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	if r.chunk == nil {
		r.chunk = bytes.Repeat([]byte{r.b}, 32<<10)
	}
	n := copy(p, r.chunk)
	if int64(n) > r.left {
		n = int(r.left)
	}
	r.left -= int64(n)
	return n, nil
}

// TestFilesUploadOverCap413: a 513 MiB file part is refused with
// 413 while streaming — the handler never buffers the whole file
// and nothing is stored. The body is generated on the fly (no
// 513 MiB allocation). TMPDIR is pointed at a scratch dir so the
// test also proves the streaming parser spills nothing there:
// the old ParseMultipartForm path spilled the part to a temp file
// under os.TempDir() (a problem on this VM, where /tmp is a 512M
// tmpfs); the multipart.Reader rewrite removed that spill, so
// the scratch dir must stay empty and no staging temp may remain
// in the store dir either.
func TestFilesUploadOverCap413(t *testing.T) {
	srv := newFilesServer(t) // before Setenv: the data dir must not sit under tmpRoot
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	tmpRoot, err := os.MkdirTemp(home, ".files-cap-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpRoot)
	t.Setenv("TMPDIR", tmpRoot)

	const boundary = "files-cap-test-boundary"
	prefix := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"purpose\"\r\n\r\n" +
		"batch\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"big.bin\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n"
	body := io.MultiReader(strings.NewReader(prefix), &repeatReader{b: 'a', left: filesMaxUploadBytes + 1<<20})
	req := httptest.NewRequest("POST", "/v1/files", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	rec := httptest.NewRecorder()
	srv.handleFiles(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap upload: status %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "512 MiB") {
		t.Fatalf("over-cap upload body %q does not name the 512 MiB cap", rec.Body.String())
	}
	// Nothing was stored: the store is still empty.
	st, err := srv.filesStore()
	if err != nil {
		t.Fatal(err)
	}
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("store holds %d file(s) after a refused upload, want 0", len(list))
	}
	// Nothing spilled into os.TempDir(), and no staging temp was
	// left behind in the store dir either.
	if entries, err := os.ReadDir(tmpRoot); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("os.TempDir() holds %d entries after a refused over-cap upload, want 0: %v", len(entries), entries)
	}
	assertNoTempFiles(t, st.Root())
}

// assertNoTempFiles fails if dir holds any *.tmp staging file —
// the residue a streaming upload must never leave behind. A
// missing dir is fine (nothing was staged yet).
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind in %s: %s", dir, e.Name())
		}
	}
}

// failAfterReader yields 'a' bytes until left runs out, then
// fails with err — a client disconnect mid-upload.
type failAfterReader struct {
	left  int64
	err   error
	chunk []byte
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, r.err
	}
	if r.chunk == nil {
		r.chunk = bytes.Repeat([]byte{'a'}, 32<<10)
	}
	n := copy(p, r.chunk)
	if int64(n) > r.left {
		n = int(r.left)
	}
	r.left -= int64(n)
	return n, nil
}

// doFilesUploadStreamed POSTs a multipart upload with the file
// part streamed from content (never buffered whole) and the
// purpose field first, so the file part streams straight into
// the store without spooling.
func doFilesUploadStreamed(t *testing.T, srv *Server, filename, purpose string, content io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	const boundary = "files-stream-test-boundary"
	prefix := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"purpose\"\r\n\r\n" +
		purpose + "\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"" + filename + "\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n"
	suffix := "\r\n--" + boundary + "--\r\n"
	body := io.MultiReader(strings.NewReader(prefix), content, strings.NewReader(suffix))
	req := httptest.NewRequest("POST", "/v1/files", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	rec := httptest.NewRecorder()
	srv.handleFiles(rec, req)
	return rec
}

// TestFilesUploadNoTempInOsTempDir: a 40 MiB upload — above the
// old 32 MiB ParseMultipartForm spill threshold — leaves NO temp
// file in os.TempDir() and no staging temp in the store dir. The
// upload streams straight into the store; the whole 40 MiB is
// never held in RAM (generated on the fly) and never spilled to
// /tmp.
func TestFilesUploadNoTempInOsTempDir(t *testing.T) {
	srv := newFilesServer(t)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	const size = 40 << 20
	rec := doFilesUploadStreamed(t, srv, "big.bin", "batch", &repeatReader{b: 'a', left: size})
	if rec.Code != 200 {
		t.Fatalf("upload: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if f := decodeFilesObject(t, rec); f.Bytes != size {
		t.Fatalf("stored bytes = %d, want %d", f.Bytes, size)
	}
	if entries, err := os.ReadDir(scratch); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("os.TempDir() holds %d entries after a %d-byte upload, want 0: %v", len(entries), size, entries)
	}
	st, err := srv.filesStore()
	if err != nil {
		t.Fatal(err)
	}
	assertNoTempFiles(t, st.Root())
}

// TestFilesUploadFileBeforePurpose: multipart field order must
// not matter — a file part that arrives before the purpose field
// is spooled under the data dir and committed once the purpose
// is known, with the same 200 + file object as the purpose-first
// order, and no spool temp left behind.
func TestFilesUploadFileBeforePurpose(t *testing.T) {
	srv := newFilesServer(t)
	const boundary = "files-order-test-boundary"
	prefix := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"order.jsonl\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n"
	middle := "\r\n--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"purpose\"\r\n\r\n" +
		"batch\r\n--" + boundary + "--\r\n"
	body := io.MultiReader(strings.NewReader(prefix), strings.NewReader("{\"a\":1}\n"), strings.NewReader(middle))
	req := httptest.NewRequest("POST", "/v1/files", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	rec := httptest.NewRecorder()
	srv.handleFiles(rec, req)
	if rec.Code != 200 {
		t.Fatalf("upload: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	f := decodeFilesObject(t, rec)
	if f.Filename != "order.jsonl" || f.Purpose != "batch" || f.Bytes != 8 {
		t.Fatalf("upload object = %+v", f)
	}
	st, err := srv.filesStore()
	if err != nil {
		t.Fatal(err)
	}
	assertNoTempFiles(t, st.Root())
}

// spoolGateReader serves data verbatim until pauseAt bytes have
// been delivered, then blocks until releaseGate is called — a
// client that stalls mid-upload. The handler is therefore
// observable mid-copy: for a file-before-purpose upload it is
// stuck in io.Copy into the spool file, so the spool must exist
// at that moment (post-request assertions can never see it: the
// deferred Remove cleans it on every exit).
type spoolGateReader struct {
	data        []byte
	pos         int
	pauseAt     int
	paused      chan struct{}
	release     chan struct{}
	pauseOnce   sync.Once
	releaseOnce sync.Once
	released    bool
}

func (r *spoolGateReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if r.pos >= r.pauseAt && !r.released {
		r.pauseOnce.Do(func() { close(r.paused) })
		<-r.release
		r.released = true
	}
	n := len(r.data) - r.pos
	if !r.released && r.pos+n > r.pauseAt {
		n = r.pauseAt - r.pos
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

func (r *spoolGateReader) releaseGate() {
	r.releaseOnce.Do(func() { close(r.release) })
}

// listUploadTemps returns the upload-*.tmp entries in dir (a
// missing dir holds none).
func listUploadTemps(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var temps []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "upload-") && strings.HasSuffix(e.Name(), ".tmp") {
			temps = append(temps, e.Name())
		}
	}
	return temps
}

// TestFilesUploadSpoolLocationInFlight: pins the spool location
// by observing it DURING a file-before-purpose upload. The body
// stalls mid-file-part; while the handler is blocked in io.Copy
// into the spool, at least one upload-*.tmp must exist under the
// store dir (st.Root()) and none under os.TempDir(). Releasing
// the stall lets the upload finish normally (200, exact bytes,
// no temp left behind). This is the negative control's
// counterpart: swapping the spool to os.TempDir() must fail this
// test, where every post-request assertion passes.
func TestFilesUploadSpoolLocationInFlight(t *testing.T) {
	srv := newFilesServer(t) // before Setenv: the data dir must not sit under tmpRoot
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	st, err := srv.filesStore()
	if err != nil {
		t.Fatal(err)
	}

	const boundary = "files-spool-test-boundary"
	prefix := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"spool.bin\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n"
	const fileSize = 8192
	suffix := "\r\n--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"purpose\"\r\n\r\n" +
		"batch\r\n--" + boundary + "--\r\n"
	body := prefix + strings.Repeat("a", fileSize) + suffix
	gate := &spoolGateReader{
		data:    []byte(body),
		pauseAt: len(prefix) + 1024, // stall mid-file-part, after the spool is created
		paused:  make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(gate.releaseGate)

	req := httptest.NewRequest("POST", "/v1/files", gate)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleFiles(rec, req)
	}()

	// Wait until the handler is stalled mid-copy (10s is generous
	// under load; the stall is deterministic, not timing-based).
	select {
	case <-gate.paused:
	case <-time.After(10 * time.Second):
		gate.releaseGate()
		t.Fatalf("upload did not stall mid-file-part within 10s (handler finished=%v)", isDone(done))
	}

	// Poll briefly for the spool to appear (scheduling lag between
	// the stall signal and the CreateTemp is possible, but the
	// stall itself guarantees the handler is inside io.Copy).
	var temps []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if temps = listUploadTemps(t, st.Root()); len(temps) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(temps) == 0 {
		gate.releaseGate()
		t.Fatalf("no upload-*.tmp spool under store dir %s while the file-before-purpose upload is stalled mid-copy", st.Root())
	}
	// And nothing spooled under os.TempDir() at the same moment.
	if tmpTemps := listUploadTemps(t, scratch); len(tmpTemps) > 0 {
		gate.releaseGate()
		t.Fatalf("upload-*.tmp spool under os.TempDir() (%s) while stalled: %v", scratch, tmpTemps)
	}
	if entries, err := os.ReadDir(scratch); err != nil {
		gate.releaseGate()
		t.Fatal(err)
	} else if len(entries) != 0 {
		gate.releaseGate()
		t.Fatalf("os.TempDir() holds %d entries while the upload is stalled, want 0: %v", len(entries), entries)
	}

	// Release the stall: the upload must finish normally.
	gate.releaseGate()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("upload did not finish within 10s after releasing the stall")
	}
	if rec.Code != 200 {
		t.Fatalf("upload: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if f := decodeFilesObject(t, rec); f.Bytes != fileSize || f.Filename != "spool.bin" || f.Purpose != "batch" {
		t.Fatalf("upload object = %+v", f)
	}
	assertNoTempFiles(t, st.Root())
}

// isDone reports whether ch is already closed, without blocking.
func isDone(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestFilesUploadDisconnectNoTemp: a client that disconnects
// mid-upload (the body errors after 1 MiB) gets a 400, nothing
// is stored, and no temp file is left behind — in os.TempDir(),
// or in the store dir — in either field order: purpose-first
// (direct stream into the store) and file-first (spooled under
// the data dir until the purpose arrives).
func TestFilesUploadDisconnectNoTemp(t *testing.T) {
	for _, fileFirst := range []bool{false, true} {
		name := "purpose-first"
		if fileFirst {
			name = "file-first"
		}
		t.Run(name, func(t *testing.T) {
			srv := newFilesServer(t)
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			const boundary = "files-disconnect-test-boundary"
			var prefix string
			if fileFirst {
				prefix = "--" + boundary + "\r\n" +
					"Content-Disposition: form-data; name=\"file\"; filename=\"cut.bin\"\r\n" +
					"Content-Type: application/octet-stream\r\n\r\n"
			} else {
				prefix = "--" + boundary + "\r\n" +
					"Content-Disposition: form-data; name=\"purpose\"\r\n\r\n" +
					"batch\r\n" +
					"--" + boundary + "\r\n" +
					"Content-Disposition: form-data; name=\"file\"; filename=\"cut.bin\"\r\n" +
					"Content-Type: application/octet-stream\r\n\r\n"
			}
			body := io.MultiReader(strings.NewReader(prefix), &failAfterReader{left: 1 << 20, err: io.ErrUnexpectedEOF})
			req := httptest.NewRequest("POST", "/v1/files", body)
			req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
			rec := httptest.NewRecorder()
			srv.handleFiles(rec, req)
			if rec.Code != 400 {
				t.Fatalf("disconnect mid-upload: status %d, want 400: %s", rec.Code, rec.Body.String())
			}
			st, err := srv.filesStore()
			if err != nil {
				t.Fatal(err)
			}
			if list, err := st.List(); err != nil {
				t.Fatal(err)
			} else if len(list) != 0 {
				t.Fatalf("store holds %d file(s) after a disconnected upload, want 0", len(list))
			}
			if entries, err := os.ReadDir(scratch); err != nil {
				t.Fatal(err)
			} else if len(entries) != 0 {
				t.Fatalf("os.TempDir() holds %d entries after a disconnected upload, want 0: %v", len(entries), entries)
			}
			assertNoTempFiles(t, st.Root())
		})
	}
}

// TestFilesUploadStartupSweepRemovesStaleTemp: the startup orphan
// sweeper — a stale upload-*.tmp planted in the store dir before
// the server's first files request (simulating a crash orphan
// from an earlier process) is removed by that first request,
// while a fresh temp (a live upload) survives. The sweep runs
// once per store dir, not per request: a stale temp planted
// AFTER the first sweep survives the next request.
func TestFilesUploadStartupSweepRemovesStaleTemp(t *testing.T) {
	srv := newFilesServer(t)
	dir := filepath.Join(srv.cfg.DataDir, "files")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	plant := func(name string, mtime time.Time) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("orphan"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	plant("upload-crash123.tmp", now.Add(-2*time.Hour)) // stale: must go
	plant("upload-live456.tmp", now)                    // fresh: must stay

	// First files request triggers the once-per-dir startup sweep.
	req := httptest.NewRequest("GET", "/v1/files", nil)
	rec := httptest.NewRecorder()
	srv.handleFiles(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "upload-crash123.tmp")); !os.IsNotExist(err) {
		t.Fatalf("stale staging temp still exists after first files request (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "upload-live456.tmp")); err != nil {
		t.Fatalf("fresh staging temp was swept, want it kept: %v", err)
	}

	// The sweep ran once: a stale temp planted AFTER it survives
	// the next request.
	plant("upload-late789.tmp", now.Add(-2*time.Hour))
	req = httptest.NewRequest("GET", "/v1/files", nil)
	rec = httptest.NewRecorder()
	srv.handleFiles(rec, req)
	if rec.Code != 200 {
		t.Fatalf("second list: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "upload-late789.tmp")); err != nil {
		t.Fatalf("temp planted after the startup sweep was removed, want the sweep once-only: %v", err)
	}
}

// TestFilesUploadTraversalFilename: a traversal filename is stored
// under its base name — the stored file can never escape the store
// dir via its name.
func TestFilesUploadTraversalFilename(t *testing.T) {
	srv := newFilesServer(t)
	rec := doFilesUpload(t, srv, "../../evil.txt", "user_data", "x", true)
	if rec.Code != 200 {
		t.Fatalf("upload: status %d: %s", rec.Code, rec.Body.String())
	}
	if f := decodeFilesObject(t, rec); f.Filename != "evil.txt" {
		t.Fatalf("stored filename = %q, want evil.txt", f.Filename)
	}
}

// TestFilesUploadValidation: bad purpose, missing purpose, and
// missing file are all honest 400s; nothing is stored.
func TestFilesUploadValidation(t *testing.T) {
	srv := newFilesServer(t)
	cases := []struct {
		name     string
		filename string
		purpose  string
		withFile bool
	}{
		{"bad purpose", "a.txt", "training", true},
		{"output-only purpose", "a.txt", "batch_output", true},
		{"missing purpose", "a.txt", "", true},
		{"missing file", "", "batch", false},
	}
	for _, c := range cases {
		rec := doFilesUpload(t, srv, c.filename, c.purpose, "x", c.withFile)
		if rec.Code != 400 {
			t.Fatalf("%s: status %d, want 400: %s", c.name, rec.Code, rec.Body.String())
		}
	}
	st, err := srv.filesStore()
	if err != nil {
		t.Fatal(err)
	}
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("rejected uploads stored %d file(s)", len(list))
	}
}

// TestFilesUnknownIDs: get/delete/content for unknown or malformed
// ids answer 404 — including traversal-shaped ids, which must
// never reach the filesystem.
func TestFilesUnknownIDs(t *testing.T) {
	srv := newFilesServer(t)
	ids := []string{"file-ffffffffffffffffffffffff", "../index.json", "..", "file-zzz"}
	for _, id := range ids {
		for _, tc := range []struct {
			method string
			path   string
		}{
			{"GET", "/v1/files/" + id},
			{"DELETE", "/v1/files/" + id},
			{"GET", "/v1/files/" + id + "/content"},
		} {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.handleFiles(rec, req)
			if rec.Code != 404 {
				t.Fatalf("%s %s: status %d, want 404: %s", tc.method, tc.path, rec.Code, rec.Body.String())
			}
		}
	}
}

// TestFilesEmptyList: an empty store lists an empty array, not null.
func TestFilesEmptyList(t *testing.T) {
	srv := newFilesServer(t)
	req := httptest.NewRequest("GET", "/v1/files", nil)
	rec := httptest.NewRecorder()
	srv.handleFiles(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("empty list body = %s, want data:[]", rec.Body.String())
	}
}

// TestFilesNoConfig: a server with no config (no data dir) answers
// 503, never a faked file or a crash.
func TestFilesNoConfig(t *testing.T) {
	srv := New(nil)
	req := httptest.NewRequest("GET", "/v1/files", nil)
	rec := httptest.NewRecorder()
	srv.handleFiles(rec, req)
	if rec.Code != 503 {
		t.Fatalf("list with no config: status %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

// TestFilesMethodNotAllowed: wrong methods on the collection path
// are 405s.
func TestFilesMethodNotAllowed(t *testing.T) {
	srv := newFilesServer(t)
	req := httptest.NewRequest("DELETE", "/v1/files", nil)
	rec := httptest.NewRecorder()
	srv.handleFiles(rec, req)
	if rec.Code != 405 {
		t.Fatalf("DELETE /v1/files: status %d, want 405", rec.Code)
	}
}

// TestFilesRoutesProposeScopes: the registration table the
// coordinator consumes — five routes, all TierManagement with a
// proposed scope (see FilesRoutes doc).
func TestFilesRoutesProposeScopes(t *testing.T) {
	routes := FilesRoutes()
	if len(routes) != 5 {
		t.Fatalf("FilesRoutes = %d routes, want 5", len(routes))
	}
	for _, r := range routes {
		if r.Scope != "chat" && r.Scope != "models" {
			t.Fatalf("route %s %s proposes scope %q, want chat or models", r.Method, r.Path, r.Scope)
		}
		if r.Description == "" {
			t.Fatalf("route %s %s has no description", r.Method, r.Path)
		}
	}
}

// --- apitest fixture contract tests (one per endpoint) ---

func TestFilesFixtureList200(t *testing.T) {
	srv := newFilesServer(t)
	seed := seedFilesFile(t, srv)
	f := apitest.LoadFixture(t, "m8.7", "files-list-200")
	apitest.Run(t, filesFixtureHandler(srv, seed), f)
}

func TestFilesFixtureGet200(t *testing.T) {
	srv := newFilesServer(t)
	seed := seedFilesFile(t, srv)
	f := apitest.LoadFixture(t, "m8.7", "files-get-200")
	apitest.Run(t, filesFixtureHandler(srv, seed), f)
}

func TestFilesFixtureGet404(t *testing.T) {
	srv := newFilesServer(t)
	f := apitest.LoadFixture(t, "m8.7", "files-get-404")
	apitest.Run(t, filesFixtureHandler(srv, nil), f)
}

func TestFilesFixtureDelete200(t *testing.T) {
	srv := newFilesServer(t)
	seed := seedFilesFile(t, srv)
	f := apitest.LoadFixture(t, "m8.7", "files-delete-200")
	apitest.Run(t, filesFixtureHandler(srv, seed), f)
	// The fixture's delete really deleted the seed.
	st, err := srv.filesStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(seed.ID); err == nil {
		t.Fatalf("fixture delete left the seeded file in the store")
	}
}

func TestFilesFixtureDelete404(t *testing.T) {
	srv := newFilesServer(t)
	f := apitest.LoadFixture(t, "m8.7", "files-delete-404")
	apitest.Run(t, filesFixtureHandler(srv, nil), f)
}

func TestFilesFixtureContent200(t *testing.T) {
	srv := newFilesServer(t)
	seed := seedFilesFile(t, srv)
	f := apitest.LoadFixture(t, "m8.7", "files-content-200")
	apitest.Run(t, filesFixtureHandler(srv, seed), f)
}

func TestFilesFixtureContent404(t *testing.T) {
	srv := newFilesServer(t)
	f := apitest.LoadFixture(t, "m8.7", "files-content-404")
	apitest.Run(t, filesFixtureHandler(srv, nil), f)
}

func TestFilesFixtureUpload400(t *testing.T) {
	srv := newFilesServer(t)
	f := apitest.LoadFixture(t, "m8.7", "files-upload-400")
	apitest.Run(t, filesFixtureHandler(srv, nil), f)
}

// TestFilesFixtureToggle: the contract assertions are real — the
// same fixture with a wrong expected status must fail the check.
// (Proves the positive fixture tests above would fail if the
// handler regressed; mirrors the harness's own toggle tests.)
func TestFilesFixtureToggle(t *testing.T) {
	srv := newFilesServer(t)
	seed := seedFilesFile(t, srv)
	f := apitest.LoadFixture(t, "m8.7", "files-list-200")
	f.Assert.Status = 201 // wrong: the handler answers 200
	srvHTTP := httptest.NewServer(filesFixtureHandler(srv, seed))
	defer srvHTTP.Close()
	resp, err := srvHTTP.Client().Get(srvHTTP.URL + "/v1/files")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := apitest.CheckResponse(resp, body, f); err == nil {
		t.Fatalf("CheckResponse with assert.status 201 against a 200 response passed; toggle verification failed")
	}
}
