// Batch API tests (M8.8 — POST/GET/DELETE /v1/batches, [id],
// [id]/cancel, delete-completed).
//
// Execution is real: batches run through a counting fake
// engine (the Engine interface handleChatCompletions uses), so
// a completed batch's output file holds genuine engine
// responses, not canned JSON. The mock upstream is the fake
// engine itself — no network, no real provider account.
//
// Tests call srv.handleBatches directly (auth is the guard
// wrapper's job, proven by the live smoke test at the bottom,
// which goes through srv.Handler() with a real gateway key).
// Fixture tests use the M8.0 apitest harness.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/files"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

const batchFixtureFileID = "file-000000000000000000000001"

// countingBatchEngine is a fake Engine that counts Chat calls
// and can block inside Chat until its context is cancelled
// (block=true) — the cancel test's slow batch. A failing engine
// (failErr != nil) fails every line, proving the error file.
type countingBatchEngine struct {
	mu      sync.Mutex
	calls   int
	block   bool
	failErr error
	resp    provider.ChatResponse
}

func (e *countingBatchEngine) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *countingBatchEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	if e.block {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e.failErr != nil {
		return nil, e.failErr
	}
	resp := e.resp
	return &resp, nil
}

func (e *countingBatchEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return e.Chat(ctx, req)
}

func okBatchEngine() *countingBatchEngine {
	return &countingBatchEngine{resp: provider.ChatResponse{
		Content: "hello from mock",
		Model:   "mock-model",
		Usage:   provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}}
}

// newBatchesServer builds a Server whose data dir is a temp dir
// (real disk files + batch index) and whose engine is eng.
func newBatchesServer(t *testing.T, eng Engine) *Server {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	return New(eng, WithConfig(cfg))
}

// seedBatchInput stores content as a purpose=batch file and
// returns it.
func seedBatchInput(t *testing.T, srv *Server, content string) *files.File {
	t.Helper()
	st, err := srv.filesStore()
	if err != nil {
		t.Fatal(err)
	}
	f, err := st.Create("input.jsonl", "batch", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// batchJSONL renders n chat-completion request lines.
func batchJSONL(t *testing.T, ids ...string) string {
	t.Helper()
	var sb strings.Builder
	for _, id := range ids {
		line, _ := json.Marshal(map[string]any{
			"custom_id": id,
			"method":    "POST",
			"url":       "/v1/chat/completions",
			"body":      map[string]any{"model": "mock-model", "messages": []any{map[string]any{"role": "user", "content": "hi " + id}}},
		})
		sb.Write(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// doBatches sends one request at the handler (no auth wrapper)
// and returns the recorder.
func doBatches(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.handleBatches(rec, req)
	return rec
}

// decodeBatch decodes a Batch object from a recorder.
func decodeBatch(t *testing.T, rec *httptest.ResponseRecorder) *Batch {
	t.Helper()
	var b Batch
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("response is not a Batch object: %v (body: %.300s)", err, rec.Body.String())
	}
	return &b
}

// waitBatchStatus polls the store until the batch reaches one
// of the wanted statuses (or the deadline passes).
func waitBatchStatus(t *testing.T, srv *Server, id string, want ...string) *Batch {
	t.Helper()
	st, err := srv.batchesStore()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b := st.get(id, ""); b != nil {
			for _, w := range want {
				if string(b.Status) == w {
					return b
				}
			}
			if batchTerminal(b.Status) {
				t.Fatalf("batch %s reached terminal status %q, wanted %v (body: %+v)", id, b.Status, want, b)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("batch %s did not reach %v within 10s", id, want)
	return nil
}

// readBatchFile reads a Files-store file's full content.
func readBatchFile(t *testing.T, srv *Server, id string) string {
	t.Helper()
	st, err := srv.filesStore()
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := st.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBatchesCreateCompletesWithRealOutput(t *testing.T) {
	eng := okBatchEngine()
	srv := newBatchesServer(t, eng)
	in := seedBatchInput(t, srv, batchJSONL(t, "req-a", "req-b"))
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions",
		"completion_window": "24h", "metadata": map[string]any{"job": "test"},
	})
	rec := doBatches(t, srv, "POST", "/v1/batches", string(body))
	if rec.Code != 200 {
		t.Fatalf("create: status %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	created := decodeBatch(t, rec)
	if created.Object != "batch" || created.Endpoint != "/v1/chat/completions" {
		t.Fatalf("create: bad shape: %+v", created)
	}
	if created.Status != batchValidating && created.Status != batchInProgress {
		t.Fatalf("create: status %q, want validating or in_progress", created.Status)
	}
	if created.ExpiresAt == nil || *created.ExpiresAt <= created.CreatedAt {
		t.Fatalf("create: expires_at %v, want > created_at %d", created.ExpiresAt, created.CreatedAt)
	}
	done := waitBatchStatus(t, srv, created.ID, "completed")
	if done.RequestCounts.Total != 2 || done.RequestCounts.Completed != 2 || done.RequestCounts.Failed != 0 {
		t.Fatalf("counts %+v, want total 2 completed 2 failed 0", done.RequestCounts)
	}
	if done.OutputFileID == nil {
		t.Fatal("completed batch has no output_file_id")
	}
	if done.ErrorFileID != nil {
		t.Fatalf("completed batch has error_file_id %v, want nil", *done.ErrorFileID)
	}
	if done.Usage == nil || done.Usage.PromptTokens != 20 || done.Usage.CompletionTokens != 10 || done.Usage.TotalTokens != 30 {
		t.Fatalf("usage %+v, want prompt 20 completion 10 total 30", done.Usage)
	}
	if done.Model == nil || *done.Model != "mock-model" {
		t.Fatalf("model %v, want mock-model", done.Model)
	}
	if done.CompletedAt == nil || done.InProgressAt == nil {
		t.Fatal("completed batch is missing in_progress_at/completed_at")
	}
	if eng.Calls() != 2 {
		t.Fatalf("engine Chat calls %d, want 2 (one per line)", eng.Calls())
	}
	// The output file holds one real output line per request.
	out := readBatchFile(t, srv, *done.OutputFileID)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("output file has %d lines, want 2:\n%s", len(lines), out)
	}
	for i, ln := range lines {
		var row struct {
			CustomID string `json:"custom_id"`
			Response struct {
				StatusCode int `json:"status_code"`
				Body       struct {
					Choices []struct {
						Message struct {
							Content string `json:"content"`
							Role    string `json:"role"`
						} `json:"message"`
					} `json:"choices"`
				} `json:"body"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(ln), &row); err != nil {
			t.Fatalf("output line %d is not JSON: %v", i, err)
		}
		if row.Response.StatusCode != 200 {
			t.Fatalf("output line %d status %d, want 200", i, row.Response.StatusCode)
		}
		if len(row.Response.Body.Choices) != 1 || row.Response.Body.Choices[0].Message.Content != "hello from mock" {
			t.Fatalf("output line %d has no real engine content: %.200s", i, ln)
		}
	}
	// Output file is a batch_output file in the Files store.
	st, _ := srv.filesStore()
	f, err := st.Get(*done.OutputFileID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Purpose != "batch_output" {
		t.Fatalf("output file purpose %q, want batch_output", f.Purpose)
	}
}

func TestBatchesCancelStopsWorker(t *testing.T) {
	eng := &countingBatchEngine{block: true, resp: provider.ChatResponse{Content: "x", Model: "mock-model"}}
	srv := newBatchesServer(t, eng)
	in := seedBatchInput(t, srv, batchJSONL(t, "c1", "c2", "c3"))
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec := doBatches(t, srv, "POST", "/v1/batches", string(body))
	created := decodeBatch(t, rec)
	// Wait until the worker is inside the first (blocked) Chat.
	deadline := time.Now().Add(10 * time.Second)
	for eng.Calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if eng.Calls() == 0 {
		t.Fatal("worker never started the first line")
	}
	waitBatchStatus(t, srv, created.ID, "in_progress")
	rec = doBatches(t, srv, "POST", "/v1/batches/"+created.ID+"/cancel", "")
	if rec.Code != 200 {
		t.Fatalf("cancel: status %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	cancelled := waitBatchStatus(t, srv, created.ID, "cancelled")
	if cancelled.CancellingAt == nil || cancelled.CancelledAt == nil {
		t.Fatal("cancelled batch is missing cancelling_at/cancelled_at")
	}
	if cancelled.RequestCounts.Completed != 0 {
		t.Fatalf("cancelled batch completed %d lines, want 0 (the in-flight line must abort, not finish)", cancelled.RequestCounts.Completed)
	}
	// No further lines start after the cancel.
	time.Sleep(200 * time.Millisecond)
	if n := eng.Calls(); n != 1 {
		t.Fatalf("engine Chat calls %d after cancel, want exactly 1 (no further lines started)", n)
	}
	// Cancel on a terminal batch is a 409.
	rec = doBatches(t, srv, "POST", "/v1/batches/"+created.ID+"/cancel", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel on cancelled batch: status %d, want 409", rec.Code)
	}
}

func TestBatchesEngineErrorsGoToErrorFile(t *testing.T) {
	eng := &countingBatchEngine{failErr: context.DeadlineExceeded, resp: provider.ChatResponse{}}
	srv := newBatchesServer(t, eng)
	in := seedBatchInput(t, srv, batchJSONL(t, "e1", "e2"))
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec := doBatches(t, srv, "POST", "/v1/batches", string(body))
	created := decodeBatch(t, rec)
	done := waitBatchStatus(t, srv, created.ID, "completed")
	if done.RequestCounts.Failed != 2 || done.RequestCounts.Completed != 0 {
		t.Fatalf("counts %+v, want failed 2 completed 0", done.RequestCounts)
	}
	if done.OutputFileID != nil || done.ErrorFileID == nil {
		t.Fatalf("output %v error %v, want output nil error set", done.OutputFileID, done.ErrorFileID)
	}
	errContent := readBatchFile(t, srv, *done.ErrorFileID)
	lines := strings.Split(strings.TrimSpace(errContent), "\n")
	if len(lines) != 2 {
		t.Fatalf("error file has %d lines, want 2:\n%s", len(lines), errContent)
	}
	var row struct {
		CustomID string `json:"custom_id"`
		Error    struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil || row.CustomID != "e1" || row.Error.Code == "" {
		t.Fatalf("error line 0 malformed: %.200s (err %v)", lines[0], err)
	}
	st, _ := srv.filesStore()
	f, err := st.Get(*done.ErrorFileID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Purpose != "batch_error" {
		t.Fatalf("error file purpose %q, want batch_error", f.Purpose)
	}
}

func TestBatchesValidationErrors(t *testing.T) {
	srv := newBatchesServer(t, okBatchEngine())

	// Unknown input file -> 404.
	rec := doBatches(t, srv, "POST", "/v1/batches", `{"input_file_id":"file-000000000000000000000099","endpoint":"/v1/chat/completions","completion_window":"24h"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown input file: status %d, want 404", rec.Code)
	}
	// Bad endpoint -> 400; bad window -> 400; missing file id -> 400.
	for _, tc := range []struct {
		name, body string
	}{
		{"endpoint", `{"input_file_id":"file-000000000000000000000001","endpoint":"/v1/embeddings","completion_window":"24h"}`},
		{"window", `{"input_file_id":"file-000000000000000000000001","endpoint":"/v1/chat/completions","completion_window":"1h"}`},
		{"missing-file", `{"endpoint":"/v1/chat/completions","completion_window":"24h"}`},
		{"metadata-object", `{"input_file_id":"file-000000000000000000000001","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":"not-an-object"}`},
	} {
		rec := doBatches(t, srv, "POST", "/v1/batches", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400 (body: %s)", tc.name, rec.Code, rec.Body.String())
		}
	}
	// Non-batch purpose input file -> 400.
	st, _ := srv.filesStore()
	other, err := st.Create("other.txt", "evals", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	rec = doBatches(t, srv, "POST", "/v1/batches", `{"input_file_id":"`+other.ID+`","endpoint":"/v1/chat/completions","completion_window":"24h"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-batch purpose: status %d, want 400", rec.Code)
	}
	// Unknown batch id -> 404 on get, cancel and delete.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/batches/batch-000000000000000000000000"},
		{"POST", "/v1/batches/batch-000000000000000000000000/cancel"},
		{"DELETE", "/v1/batches/batch-000000000000000000000000"},
	} {
		rec := doBatches(t, srv, tc.method, tc.path, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: status %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

func TestBatchesDuplicateCustomIDAndBadLines(t *testing.T) {
	srv := newBatchesServer(t, okBatchEngine())
	content := batchJSONL(t, "dup", "ok1") +
		"{\"custom_id\":\"dup\",\"method\":\"POST\",\"url\":\"/v1/chat/completions\",\"body\":{\"model\":\"mock-model\",\"messages\":[{\"role\":\"user\",\"content\":\"again\"}]}}\n" +
		"{\"method\":\"POST\",\"url\":\"/v1/chat/completions\",\"body\":{\"model\":\"mock-model\",\"messages\":[{\"role\":\"user\",\"content\":\"no id\"}]}}\n" +
		"not json at all\n" +
		"{\"custom_id\":\"bad-url\",\"method\":\"POST\",\"url\":\"/v1/embeddings\",\"body\":{\"model\":\"mock-model\",\"messages\":[{\"role\":\"user\",\"content\":\"x\"}]}}\n"
	in := seedBatchInput(t, srv, content)
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec := doBatches(t, srv, "POST", "/v1/batches", string(body))
	created := decodeBatch(t, rec)
	done := waitBatchStatus(t, srv, created.ID, "completed")
	// 2 valid lines execute (dup, ok1); 3 bad lines are skipped.
	if done.RequestCounts.Total != 2 || done.RequestCounts.Completed != 2 {
		t.Fatalf("counts %+v, want total 2 completed 2", done.RequestCounts)
	}
	if done.Errors == nil || done.Errors.Code != "validation_failed" {
		t.Fatalf("errors %+v, want validation_failed with per-line data", done.Errors)
	}
	if len(done.Errors.Data) != 4 {
		t.Fatalf("errors.data has %d entries, want 4 (duplicate line 3, missing custom_id line 4, bad JSON line 5, bad url line 6): %+v", len(done.Errors.Data), done.Errors.Data)
	}
	// 1-based line numbers: duplicate is line 3, missing id line 4.
	found := map[int]bool{}
	for _, e := range done.Errors.Data {
		found[e.Line] = true
	}
	for _, want := range []int{3, 4, 5, 6} {
		if !found[want] {
			t.Fatalf("errors.data missing line %d: %+v", want, done.Errors.Data)
		}
	}
}

func TestBatchesZeroValidLinesFails(t *testing.T) {
	srv := newBatchesServer(t, okBatchEngine())
	in := seedBatchInput(t, srv, "not json\n{\"custom_id\":\"x\"}\n")
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec := doBatches(t, srv, "POST", "/v1/batches", string(body))
	created := decodeBatch(t, rec)
	failed := waitBatchStatus(t, srv, created.ID, "failed")
	if failed.Errors == nil || failed.Errors.Code != "invalid_input_file" {
		t.Fatalf("errors %+v, want invalid_input_file", failed.Errors)
	}
	if len(failed.Errors.Data) != 2 {
		t.Fatalf("errors.data %+v, want 2 line errors", failed.Errors.Data)
	}
	if failed.FailedAt == nil || failed.OutputFileID != nil {
		t.Fatalf("failed batch: failed_at %v output %v", failed.FailedAt, failed.OutputFileID)
	}
}

func TestBatchesDeleteAndDeleteCompleted(t *testing.T) {
	eng := &countingBatchEngine{block: true, resp: provider.ChatResponse{Content: "x"}}
	srv := newBatchesServer(t, eng)
	in := seedBatchInput(t, srv, batchJSONL(t, "d1"))
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	// Running batch: delete -> 409.
	rec := doBatches(t, srv, "POST", "/v1/batches", string(body))
	running := decodeBatch(t, rec)
	rec = doBatches(t, srv, "DELETE", "/v1/batches/"+running.ID, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete running batch: status %d, want 409", rec.Code)
	}
	// delete-completed must NOT touch the running batch.
	rec = doBatches(t, srv, "DELETE", "/v1/batches/delete-completed", "")
	if rec.Code != 200 {
		t.Fatalf("delete-completed: status %d, want 200", rec.Code)
	}
	var swept struct {
		Data         []string `json:"data"`
		DeletedCount int      `json:"deleted_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &swept); err != nil || swept.DeletedCount != 0 {
		t.Fatalf("delete-completed swept %v, want 0 while a batch runs", rec.Body.String())
	}
	// Cancel it, then delete -> 200, then get -> 404.
	_ = doBatches(t, srv, "POST", "/v1/batches/"+running.ID+"/cancel", "")
	waitBatchStatus(t, srv, running.ID, "cancelled")
	rec = doBatches(t, srv, "DELETE", "/v1/batches/"+running.ID, "")
	if rec.Code != 200 {
		t.Fatalf("delete terminal batch: status %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	rec = doBatches(t, srv, "GET", "/v1/batches/"+running.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: status %d, want 404", rec.Code)
	}
	// A second completed batch is swept by delete-completed.
	eng.block = false
	in2 := seedBatchInput(t, srv, batchJSONL(t, "d2"))
	body2, _ := json.Marshal(map[string]any{
		"input_file_id": in2.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec = doBatches(t, srv, "POST", "/v1/batches", string(body2))
	second := decodeBatch(t, rec)
	waitBatchStatus(t, srv, second.ID, "completed")
	rec = doBatches(t, srv, "DELETE", "/v1/batches/delete-completed", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &swept); err != nil || swept.DeletedCount != 1 || len(swept.Data) != 1 || swept.Data[0] != second.ID {
		t.Fatalf("delete-completed swept %v, want exactly [%s]", rec.Body.String(), second.ID)
	}
}

func TestBatchesListLimit(t *testing.T) {
	srv := newBatchesServer(t, okBatchEngine())
	for _, bad := range []string{"abc", "0", "-1", "101", "1.5"} {
		rec := doBatches(t, srv, "GET", "/v1/batches?limit="+bad, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("limit=%s: status %d, want 400", bad, rec.Code)
		}
	}
	// limit=1 with two batches: newest first, has_more true.
	in := seedBatchInput(t, srv, batchJSONL(t, "l1"))
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec := doBatches(t, srv, "POST", "/v1/batches", string(body))
	first := decodeBatch(t, rec)
	waitBatchStatus(t, srv, first.ID, "completed")
	rec = doBatches(t, srv, "POST", "/v1/batches", string(body))
	second := decodeBatch(t, rec)
	waitBatchStatus(t, srv, second.ID, "completed")
	rec = doBatches(t, srv, "GET", "/v1/batches?limit=1", "")
	var list batchListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != second.ID || !list.HasMore {
		t.Fatalf("limit=1 list: %+v, want newest (%s) + has_more", list, second.ID)
	}
	if list.FirstID == nil || *list.FirstID != second.ID || list.LastID == nil || *list.LastID != second.ID {
		t.Fatalf("limit=1 list first/last: %v / %v", list.FirstID, list.LastID)
	}
	// Default limit lists both, newest first.
	rec = doBatches(t, srv, "GET", "/v1/batches", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 2 || list.Data[0].ID != second.ID || list.Data[1].ID != first.ID || list.HasMore {
		t.Fatalf("default list: %+v, want [newest, oldest], has_more false", list)
	}
}

func TestBatchesPersistenceAcrossRestart(t *testing.T) {
	eng := okBatchEngine()
	srv := newBatchesServer(t, eng)
	dir := srv.cfg.DataDir
	in := seedBatchInput(t, srv, batchJSONL(t, "p1"))
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec := doBatches(t, srv, "POST", "/v1/batches", string(body))
	created := decodeBatch(t, rec)
	waitBatchStatus(t, srv, created.ID, "completed")
	// Simulate a restart: drop the cached store so the next
	// server reloads batches.json from disk.
	batchStores.Delete(filepath.Join(dir, "batches"))
	srv2 := New(eng, WithConfig(config.DefaultConfig(dir)))
	rec = doBatches(t, srv2, "GET", "/v1/batches/"+created.ID, "")
	if rec.Code != 200 {
		t.Fatalf("get after restart: status %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	restored := decodeBatch(t, rec)
	if restored.Status != batchCompleted || restored.RequestCounts.Completed != 1 {
		t.Fatalf("restored batch: %+v, want completed with 1 completion", restored)
	}
}

// TestBatchesRestartMarksRunningFailed proves the honest
// restart rule: a batch still in progress when the server
// restarts is marked failed (server_restart), never silently
// re-run or left in_progress forever.
func TestBatchesRestartMarksRunningFailed(t *testing.T) {
	eng := &countingBatchEngine{block: true, resp: provider.ChatResponse{Content: "x"}}
	srv := newBatchesServer(t, eng)
	dir := srv.cfg.DataDir
	in := seedBatchInput(t, srv, batchJSONL(t, "r1", "r2"))
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec := doBatches(t, srv, "POST", "/v1/batches", string(body))
	created := decodeBatch(t, rec)
	waitBatchStatus(t, srv, created.ID, "in_progress")
	// Keep the pre-restart store object: the worker holds it,
	// and it is the only handle that can cancel the still-live
	// worker goroutine at the end of the test.
	oldSt, err := srv.batchesStore()
	if err != nil {
		t.Fatal(err)
	}
	batchStores.Delete(filepath.Join(dir, "batches"))
	srv2 := New(eng, WithConfig(config.DefaultConfig(dir)))
	rec = doBatches(t, srv2, "GET", "/v1/batches/"+created.ID, "")
	restored := decodeBatch(t, rec)
	if restored.Status != batchFailed || restored.Errors == nil || restored.Errors.Code != "server_restart" {
		t.Fatalf("restored running batch: status %q errors %+v, want failed/server_restart", restored.Status, restored.Errors)
	}
	// Clean up the first server's still-blocked worker so the
	// test process does not leak a goroutine.
	_, _ = oldSt.cancelBatch(created.ID, "")
}

// TestBatchesLiveSmoke goes through the real registered mux
// (guard rows + server.go registrations) with a gateway key:
// POST create + GET retrieve over HTTP must answer 200.
func TestBatchesLiveSmoke(t *testing.T) {
	eng := okBatchEngine()
	cfg := config.DefaultConfig(t.TempDir())
	srv := New(eng, WithConfig(cfg), WithStaticKeys("test-key-123"))
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := httpSrv.Client()

	in := seedBatchInput(t, srv, batchJSONL(t, "s1"))
	payload, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	req, _ := http.NewRequest("POST", httpSrv.URL+"/v1/batches", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key-123")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("live POST /v1/batches: status %d, want 200 (body: %.300s)", resp.StatusCode, raw)
	}
	created := decodeBatchBytes(t, raw)
	req, _ = http.NewRequest("GET", httpSrv.URL+"/v1/batches/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("live GET /v1/batches/{id}: status %d, want 200 (body: %.300s)", resp.StatusCode, raw)
	}
	// No key at all must not reach the handler (401).
	req, _ = http.NewRequest("GET", httpSrv.URL+"/v1/batches", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("live GET /v1/batches without a key: status %d, want 401", resp.StatusCode)
	}
	// The batch still completes through the live server.
	waitBatchStatus(t, srv, created.ID, "completed")
}

// TestBatchesNoEngineIs503 proves a batch is never accepted
// when there is no engine to execute it (fake work refused).
func TestBatchesNoEngineIs503(t *testing.T) {
	srv := newBatchesServer(t, nil)
	in := seedBatchInput(t, srv, batchJSONL(t, "n1"))
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec := doBatches(t, srv, "POST", "/v1/batches", string(body))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("create without engine: status %d, want 503", rec.Code)
	}
}

// decodeBatchBytes decodes a Batch object from raw bytes.
func decodeBatchBytes(t *testing.T, raw []byte) *Batch {
	t.Helper()
	var b Batch
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("response is not a Batch object: %v (body: %.300s)", err, raw)
	}
	return &b
}

// --- Fixture (apitest harness) tests -------------------------------------

const batchFixtureBatchID = "batch-000000000000000000000000"

func batchesFixtureHandler(t *testing.T, srv *Server, seed *files.File, batchID string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seed != nil && r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			raw = bytes.ReplaceAll(raw, []byte(batchFixtureFileID), []byte(seed.ID))
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r.ContentLength = int64(len(raw))
		}
		if batchID != "" {
			r.URL.Path = strings.ReplaceAll(r.URL.Path, batchFixtureBatchID, batchID)
		}
		srv.handleBatches(w, r)
	})
}

func TestBatchesFixtures(t *testing.T) {
	eng := okBatchEngine()
	srv := newBatchesServer(t, eng)
	seed := seedBatchInput(t, srv, batchJSONL(t, "f1"))
	h := batchesFixtureHandler(t, srv, seed, "")

	// create-200 runs the real worker; the fixture asserts the
	// create snapshot shape (status validating).
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.8", "create-200"))
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.8", "create-400-endpoint"))
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.8", "create-400-window"))
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.8", "create-404-file"))
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.8", "list-200"))
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.8", "get-404"))

	// The fixture-created batch must still complete for real.
	// Find it via the list endpoint (newest first).
	rec := doBatches(t, srv, "GET", "/v1/batches?limit=1", "")
	var list batchListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 {
		t.Fatalf("expected 1 batch from fixture create, got %d", len(list.Data))
	}
	waitBatchStatus(t, srv, list.Data[0].ID, "completed")

	// delete-200 runs against the fixture-created (completed) batch.
	hd := batchesFixtureHandler(t, srv, seed, list.Data[0].ID)
	apitest.Run(t, hd, apitest.LoadFixture(t, "m8.8", "delete-200"))

	// delete-completed-200 runs with exactly one terminal batch in
	// the store, so deleted_count is 1.
	body, _ := json.Marshal(map[string]any{
		"input_file_id": seed.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec = doBatches(t, srv, "POST", "/v1/batches", string(body))
	second := decodeBatch(t, rec)
	waitBatchStatus(t, srv, second.ID, "completed")
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.8", "delete-completed-200"))

	// cancel-200 needs a RUNNING batch at cancel time, so it runs
	// against a separate blocking-engine server.
	engB := &countingBatchEngine{block: true, resp: provider.ChatResponse{Content: "x", Model: "mock-model"}}
	srvB := newBatchesServer(t, engB)
	inB := seedBatchInput(t, srvB, batchJSONL(t, "c1", "c2", "c3"))
	bodyB, _ := json.Marshal(map[string]any{
		"input_file_id": inB.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec = doBatches(t, srvB, "POST", "/v1/batches", string(bodyB))
	running := decodeBatch(t, rec)
	deadline := time.Now().Add(10 * time.Second)
	for engB.Calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	waitBatchStatus(t, srvB, running.ID, "in_progress")
	hb := batchesFixtureHandler(t, srvB, inB, running.ID)
	apitest.Run(t, hb, apitest.LoadFixture(t, "m8.8", "cancel-200"))
	waitBatchStatus(t, srvB, running.ID, "cancelled")
}
