package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/responses"
)

// TestResponsesContinuationSurvivesRestart (CHECKLIST 315–316):
// a response created on one server instance is continued on a
// second instance built over the same persistence file — the
// "restart". The continuation serves 200 with the prior
// transcript in context (the engine sees the first user turn,
// the first assistant answer, then the new turn), and the
// retrieve serves the same object the create served — not a
// 404, not a fresh conversation.
func TestResponsesContinuationSurvivesRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "responses.json")
	eng := &captureEngine{resp: provider.ChatResponse{
		Content: "hello back", Model: "gpt-4o",
		Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 4, TotalTokens: 9},
	}}
	srv1 := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithResponsesFile(file))
	rec := doReq(t, srv1, http.MethodPost, "/v1/responses",
		`{"model":"gpt-4o","input":"hi"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	obj1 := decodeResponsesBody(t, rec)
	// The create persisted the transcript to the file.
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("persistence file missing after create: %v", err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("persistence file is not valid JSON: %s", data)
	}

	// The restart: a fresh server over the same file.
	srv2 := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithResponsesFile(file))
	srv2.RestoreResponses()

	// Retrieve serves the same object, not a 404.
	rec2 := doReq(t, srv2, http.MethodGet, "/v1/responses/"+obj1.ID, "", "test-key-123")
	if rec2.Code != http.StatusOK {
		t.Fatalf("retrieve after restart: status = %d, want 200 (body %s)", rec2.Code, rec2.Body.String())
	}
	var obj2 responsesResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &obj2); err != nil {
		t.Fatalf("decode retrieve: %v", err)
	}
	if obj2.ID != obj1.ID || obj2.Output[0].Content[0].Text != "hello back" {
		t.Fatalf("retrieve = %+v, want the same object as the create", obj2)
	}

	// Continuation serves 200 with the prior transcript in
	// context: the engine sees the first user turn, the first
	// assistant answer, then the new turn.
	rec3 := doReq(t, srv2, http.MethodPost, "/v1/responses",
		`{"model":"gpt-4o","input":"second turn","previous_response_id":"`+obj1.ID+`"}`,
		"test-key-123")
	if rec3.Code != http.StatusOK {
		t.Fatalf("continuation after restart: status = %d, want 200 (body %s)", rec3.Code, rec3.Body.String())
	}
	obj3 := decodeResponsesBody(t, rec3)
	if obj3.PreviousResponseID == nil || *obj3.PreviousResponseID != obj1.ID {
		t.Fatalf("continuation previous_response_id = %v, want %s", obj3.PreviousResponseID, obj1.ID)
	}
	req := eng.lastReq()
	if len(req.Messages) != 3 {
		t.Fatalf("engine saw %d messages, want 3 (prior user + prior assistant + new user): %+v",
			len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != "user" || req.Messages[0].Content != "hi" ||
		req.Messages[1].Role != "assistant" || req.Messages[1].Content != "hello back" ||
		req.Messages[2].Role != "user" || req.Messages[2].Content != "second turn" {
		t.Fatalf("engine messages = %+v, want the prior transcript + the new turn", req.Messages)
	}

	// The feed reports the restore honestly: 1 restored, 2
	// stored (the original + its continuation), and no content.
	feedRec := doReq(t, srv2, http.MethodGet, "/dashboard/responses", "", "")
	if feedRec.Code != http.StatusOK {
		t.Fatalf("feed: status = %d, want 200", feedRec.Code)
	}
	var f responsesFeed
	if err := json.Unmarshal(feedRec.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if f.Restored != 1 || f.Stored != 2 {
		t.Fatalf("feed restored=%d stored=%d, want restored 1 stored 2", f.Restored, f.Stored)
	}
	if got := feedRec.Body.String(); strings.Contains(got, "hello back") || strings.Contains(got, `"input"`) {
		t.Fatalf("feed leaks response content: %s", got)
	}
}

// TestResponsesDeleteSurvivesRestart: a deleted response stays
// deleted across a restart — the persistence file is rewritten
// without it, so the restart never resurrects it.
func TestResponsesDeleteSurvivesRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "responses.json")
	eng := &captureEngine{resp: provider.ChatResponse{Content: "hi back", Model: "m"}}
	srv1 := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithResponsesFile(file))
	rec := doReq(t, srv1, http.MethodPost, "/v1/responses",
		`{"model":"m","input":"hi"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status = %d, want 200", rec.Code)
	}
	id := decodeResponsesBody(t, rec).ID
	del := doReq(t, srv1, http.MethodDelete, "/v1/responses/"+id, "", "test-key-123")
	if del.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, want 200 (body %s)", del.Code, del.Body.String())
	}

	srv2 := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithResponsesFile(file))
	srv2.RestoreResponses()
	if rec := doReq(t, srv2, http.MethodGet, "/v1/responses/"+id, "", "test-key-123"); rec.Code != http.StatusNotFound {
		t.Fatalf("retrieve after restart: status = %d, want 404", rec.Code)
	}
	if rec := doReq(t, srv2, http.MethodPost, "/v1/responses",
		`{"model":"m","input":"again","previous_response_id":"`+id+`"}`,
		"test-key-123"); rec.Code != http.StatusNotFound {
		t.Fatalf("continuation after restart: status = %d, want 404", rec.Code)
	}
}

// TestResponsesRestoreCorruptFileStartsEmpty: a corrupt
// persistence file never fails the boot — the server logs it
// and starts with an empty store, and creates keep working.
func TestResponsesRestoreCorruptFileStartsEmpty(t *testing.T) {
	file := filepath.Join(t.TempDir(), "responses.json")
	if err := os.WriteFile(file, []byte(`{corrupt`), 0o600); err != nil {
		t.Fatal(err)
	}
	eng := &captureEngine{resp: provider.ChatResponse{Content: "ok", Model: "m"}}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithResponsesFile(file))
	srv.RestoreResponses() // must not panic
	feedRec := doReq(t, srv, http.MethodGet, "/dashboard/responses", "", "")
	var f responsesFeed
	if err := json.Unmarshal(feedRec.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if f.Stored != 0 || f.Restored != 0 {
		t.Fatalf("feed = %+v, want an empty store after a corrupt file", f)
	}
	rec := doReq(t, srv, http.MethodPost, "/v1/responses",
		`{"model":"m","input":"hi"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("create after corrupt restore: status = %d, want 200", rec.Code)
	}
}

// TestResponsesRestoreSkipsExpired: a record older than the TTL
// in the file is not restored — a restart never resurrects a
// response the live store would already have dropped.
func TestResponsesRestoreSkipsExpired(t *testing.T) {
	file := filepath.Join(t.TempDir(), "responses.json")
	old := responses.Record{
		ID: "resp_old", Model: "m", Status: responses.StatusCompleted,
		CreatedAt:      time.Now().Add(-3 * time.Hour).Unix(),
		TranscriptJSON: json.RawMessage(`[{"role":"user","content":"hi"}]`),
		ResponseJSON:   json.RawMessage(`{"id":"resp_old","object":"response"}`),
	}
	if err := responses.SaveFile(file, []responses.Record{old}); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	eng := &captureEngine{resp: provider.ChatResponse{Content: "ok", Model: "m"}}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithResponsesFile(file))
	srv.RestoreResponses()
	if rec := doReq(t, srv, http.MethodGet, "/v1/responses/resp_old", "", "test-key-123"); rec.Code != http.StatusNotFound {
		t.Fatalf("retrieve expired: status = %d, want 404", rec.Code)
	}
}

// TestResponsesRestoreIsIdempotent: restoring twice restores
// nothing the second time — the live store wins.
func TestResponsesRestoreIsIdempotent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "responses.json")
	eng := &captureEngine{resp: provider.ChatResponse{Content: "ok", Model: "m"}}
	srv1 := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithResponsesFile(file))
	rec := doReq(t, srv1, http.MethodPost, "/v1/responses",
		`{"model":"m","input":"hi"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status = %d, want 200", rec.Code)
	}
	srv2 := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithResponsesFile(file))
	srv2.RestoreResponses()
	srv2.RestoreResponses()
	feedRec := doReq(t, srv2, http.MethodGet, "/dashboard/responses", "", "")
	var f responsesFeed
	if err := json.Unmarshal(feedRec.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if f.Restored != 1 || f.Stored != 1 {
		t.Fatalf("feed restored=%d stored=%d, want restored 1 stored 1", f.Restored, f.Stored)
	}
}
