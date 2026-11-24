// AUD-SEC-010 regression tests at the HTTP layer: file routes enforce
// ownership, including direct object-id access and enumeration.
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/files"
)

// p13FilesUpload uploads a file with one credential header.
func p13FilesUpload(t *testing.T, srv *Server, header, value, filename, purpose, content string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("purpose", purpose); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(header, value)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func p13FilesKeyed(t *testing.T) (*Server, string, string, string) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	srv := p13Server(t, cfg, okBatchEngine())
	_, rawA := p13VendorKey(t, cfg, "files-key-a", []string{"chat"}, nil)
	_, rawB := p13VendorKey(t, cfg, "files-key-b", []string{"chat"}, nil)
	_, rawAdmin := p13VendorKey(t, cfg, "files-key-admin", []string{"chat", "admin"}, nil)
	return srv, rawA, rawB, rawAdmin
}

// TestAUD_SEC_010FilesOwnershipIsolation covers the nine required
// properties in one round trip set.
func TestAUD_SEC_010FilesOwnershipIsolation(t *testing.T) {
	srv, rawA, rawB, rawAdmin := p13FilesKeyed(t)

	// (1) Key A uploads and can access its own file.
	rec := p13FilesUpload(t, srv, "X-Api-Key", rawA, "mine.jsonl", "batch", `{"a":1}`+"\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("A upload: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var f files.File
	if err := json.NewDecoder(rec.Body).Decode(&f); err != nil {
		t.Fatal(err)
	}
	if f.ID == "" {
		t.Fatal("A upload: no file id")
	}
	recA := httptest.NewRecorder()
	srv.ServeHTTP(recA, p13Req(http.MethodGet, "/v1/files/"+f.ID, "X-Api-Key", rawA, ""))
	if recA.Code != http.StatusOK {
		t.Fatalf("A get own file: status %d, want 200 (%s)", recA.Code, recA.Body.String())
	}

	// (1b) A downloads its own content.
	recA = httptest.NewRecorder()
	srv.ServeHTTP(recA, p13Req(http.MethodGet, "/v1/files/"+f.ID+"/content", "X-Api-Key", rawA, ""))
	if recA.Code != http.StatusOK {
		t.Fatalf("A download own file: status %d, want 200", recA.Code)
	}
	if body := recA.Body.String(); !strings.Contains(body, `{"a":1}`) {
		t.Fatalf("A download body = %q, want the uploaded content", body)
	}

	// (1c) A's list contains it.
	recA = httptest.NewRecorder()
	srv.ServeHTTP(recA, p13Req(http.MethodGet, "/v1/files", "X-Api-Key", rawA, ""))
	if recA.Code != http.StatusOK {
		t.Fatalf("A list: status %d, want 200", recA.Code)
	}
	var listA struct {
		Data []files.File `json:"data"`
	}
	if err := json.NewDecoder(recA.Body).Decode(&listA); err != nil {
		t.Fatal(err)
	}
	if len(listA.Data) != 1 || listA.Data[0].ID != f.ID {
		t.Fatalf("A list = %+v, want exactly its own file", listA.Data)
	}

	// (2) Key B cannot list A's file.
	recB := httptest.NewRecorder()
	srv.ServeHTTP(recB, p13Req(http.MethodGet, "/v1/files", "X-Api-Key", rawB, ""))
	if recB.Code != http.StatusOK {
		t.Fatalf("B list: status %d, want 200", recB.Code)
	}
	var listB struct {
		Data []files.File `json:"data"`
	}
	if err := json.NewDecoder(recB.Body).Decode(&listB); err != nil {
		t.Fatal(err)
	}
	for _, x := range listB.Data {
		if x.ID == f.ID {
			t.Fatalf("cross-key leak: B's list contains A's file %s", f.ID)
		}
	}

	// (3) B cannot download A's content.
	recB = httptest.NewRecorder()
	srv.ServeHTTP(recB, p13Req(http.MethodGet, "/v1/files/"+f.ID+"/content", "X-Api-Key", rawB, ""))
	if recB.Code != http.StatusNotFound {
		t.Fatalf("B download A's file: status %d, want 404", recB.Code)
	}

	// (5) B cannot read A's metadata.
	recB = httptest.NewRecorder()
	srv.ServeHTTP(recB, p13Req(http.MethodGet, "/v1/files/"+f.ID, "X-Api-Key", rawB, ""))
	if recB.Code != http.StatusNotFound {
		t.Fatalf("B get A's metadata: status %d, want 404", recB.Code)
	}

	// (4) B cannot delete A's file, and the file survives.
	recB = httptest.NewRecorder()
	srv.ServeHTTP(recB, p13Req(http.MethodDelete, "/v1/files/"+f.ID, "X-Api-Key", rawB, ""))
	if recB.Code != http.StatusNotFound {
		t.Fatalf("B delete A's file: status %d, want 404", recB.Code)
	}
	recA = httptest.NewRecorder()
	srv.ServeHTTP(recA, p13Req(http.MethodGet, "/v1/files/"+f.ID, "X-Api-Key", rawA, ""))
	if recA.Code != http.StatusOK {
		t.Fatalf("A's file disappeared after B's delete attempt: status %d", recA.Code)
	}

	// (9) An admin-scoped key has no implicit bypass: files stay
	// owner-scoped, so the admin key sees only what it uploaded.
	recAdm := httptest.NewRecorder()
	srv.ServeHTTP(recAdm, p13Req(http.MethodGet, "/v1/files/"+f.ID, "X-Api-Key", rawAdmin, ""))
	if recAdm.Code != http.StatusNotFound {
		t.Fatalf("admin-scoped key read another key's file: status %d, want 404 (no implicit bypass)", recAdm.Code)
	}

	// (8) Same-owner flows still work end to end: A deletes its own file.
	recA = httptest.NewRecorder()
	srv.ServeHTTP(recA, p13Req(http.MethodDelete, "/v1/files/"+f.ID, "X-Api-Key", rawA, ""))
	if recA.Code != http.StatusOK {
		t.Fatalf("A delete own file: status %d, want 200 (%s)", recA.Code, recA.Body.String())
	}
}

// TestAUD_SEC_010EmptyAndBadCredentialsFailClosed covers properties 6
// and 7: an empty identity reaches no owned file, and an unresolvable
// credential is refused rather than bucket-ed with "no identity".
func TestAUD_SEC_010EmptyAndBadCredentialsFailClosed(t *testing.T) {
	srv, rawA, _, _ := p13FilesKeyed(t)

	// Seed one owned file and one unowned (auth-relaxed) file.
	rec := p13FilesUpload(t, srv, "X-Api-Key", rawA, "owned.jsonl", "batch", "owned\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("seed owned: %d", rec.Code)
	}
	var owned files.File
	if err := json.NewDecoder(rec.Body).Decode(&owned); err != nil {
		t.Fatal(err)
	}
	st, err := srv.filesStore()
	if err != nil {
		t.Fatal(err)
	}
	unowned, err := st.Create("unowned.jsonl", "batch", strings.NewReader("unowned\n"))
	if err != nil {
		t.Fatal(err)
	}

	// (6) No credential: the route requires a key, so 401 — and the
	// store rule (asserted in internal/files) keeps owned files out of
	// an unowned caller's reach either way.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, p13Req(http.MethodGet, "/v1/files/"+owned.ID, "", "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: status %d, want 401", rec.Code)
	}

	// (7) A malformed credential does not authenticate.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, p13Req(http.MethodGet, "/v1/files/"+owned.ID, "Authorization", "Bearer not-a-real-key", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad credential: status %d, want 401", rec.Code)
	}

	// A non-Bearer scheme is not a credential.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, p13Req(http.MethodGet, "/v1/files/"+owned.ID, "Authorization", "Basic "+rawA, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Basic scheme: status %d, want 401", rec.Code)
	}

	// The unowned file is reachable by the store's unowned caller but
	// never through an identified key's HTTP request.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, p13Req(http.MethodGet, "/v1/files/"+unowned.ID, "X-Api-Key", rawA, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("identified key read an unowned file: status %d, want 404", rec.Code)
	}
}

// TestAUD_SEC_010BatchInputFileOwnership: a key cannot reference
// another key's file as a batch input.
func TestAUD_SEC_010BatchInputFileOwnership(t *testing.T) {
	srv, rawA, rawB, _ := p13FilesKeyed(t)

	rec := p13FilesUpload(t, srv, "X-Api-Key", rawA, "input.jsonl", "batch", `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"mock-model","messages":[{"role":"user","content":"hi"}]}}`+"\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("A upload batch input: %d %s", rec.Code, rec.Body.String())
	}
	var in files.File
	if err := json.NewDecoder(rec.Body).Decode(&in); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	// Key B tries to use A's file as its batch input → 404, no batch.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, p13Req(http.MethodPost, "/v1/batches", "X-Api-Key", rawB, string(body)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("B using A's input file: status %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	// Key A can.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, p13Req(http.MethodPost, "/v1/batches", "X-Api-Key", rawA, string(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("A using its own input file: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestAUD_SEC_010BatchOutputFileIsOwnerScoped: the batch's output file
// belongs to the batch owner.
func TestAUD_SEC_010BatchOutputFileIsOwnerScoped(t *testing.T) {
	srv, rawA, rawB, _ := p13FilesKeyed(t)

	rec := p13FilesUpload(t, srv, "X-Api-Key", rawA, "in.jsonl", "batch", `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"mock-model","messages":[{"role":"user","content":"hi"}]}}`+"\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d", rec.Code)
	}
	var in files.File
	if err := json.NewDecoder(rec.Body).Decode(&in); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, p13Req(http.MethodPost, "/v1/batches", "X-Api-Key", rawA, string(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID           string `json:"id"`
		OutputFileID string `json:"output_file_id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	done := waitBatchStatus(t, srv, created.ID, "completed")
	if done.OutputFileID == nil {
		t.Fatal("completed batch has no output_file_id")
	}
	outID := *done.OutputFileID

	// The owner can download it; another key cannot.
	recA := httptest.NewRecorder()
	srv.ServeHTTP(recA, p13Req(http.MethodGet, "/v1/files/"+outID+"/content", "X-Api-Key", rawA, ""))
	if recA.Code != http.StatusOK {
		t.Fatalf("owner download of batch output: status %d, want 200", recA.Code)
	}
	recB := httptest.NewRecorder()
	srv.ServeHTTP(recB, p13Req(http.MethodGet, "/v1/files/"+outID+"/content", "X-Api-Key", rawB, ""))
	if recB.Code != http.StatusNotFound {
		t.Fatalf("foreign download of batch output: status %d, want 404", recB.Code)
	}
	// And it must not appear in the foreign key's list.
	recB = httptest.NewRecorder()
	srv.ServeHTTP(recB, p13Req(http.MethodGet, "/v1/files", "X-Api-Key", rawB, ""))
	if recB.Code != http.StatusOK {
		t.Fatalf("B list: %d", recB.Code)
	}
	raw, _ := io.ReadAll(recB.Body)
	if strings.Contains(string(raw), outID) {
		t.Fatalf("B's file list leaks the batch output id %s", outID)
	}
}
