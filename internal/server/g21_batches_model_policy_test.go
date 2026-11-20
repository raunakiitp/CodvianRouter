// G21 — per-key model policy (ModelAllowed / ForcedModel) on
// POST /v1/batches (GAP-TRACK §9, G20 Blocker B1).
//
// The hole: handleBatchesCreate never read the input file's
// models and never called checkKeyPolicy, so a restricted or
// forced-model key could spend on any model by uploading a batch
// file. The fix gates at create time in handleBatchesCreate via
// enforceBatchModelPolicy (batches_api.go): every distinct model
// of the file's valid lines is run through checkKeyPolicy — the
// same helper the chat path runs — as a /v1/chat/completions
// request, before any batch row exists. The whole batch is
// refused 403 if ANY line names a model the key may not use.
//
// Slot rule (proven below with an RPM=1 key, G20-style): a
// refusal consumes no traffic slot, and an admitted create
// consumes exactly one — the batch path admitted no traffic at
// all before this slice, and one slot per create is the chat
// rule of one slot per HTTP request.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// g21BatchJSONL renders one chat-completion request line per
// model, with distinct custom_ids.
func g21BatchJSONL(t *testing.T, models ...string) string {
	t.Helper()
	var sb strings.Builder
	for i, model := range models {
		line, _ := json.Marshal(map[string]any{
			"custom_id": "req-" + string(rune('a'+i)),
			"method":    "POST",
			"url":       "/v1/chat/completions",
			"body":      map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		})
		sb.Write(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// g21CreateBatchRec posts a batch create for content through
// the real mux (guard + handleBatchesCreate) under the fixture
// key and returns the recorder.
func g21CreateBatchRec(t *testing.T, srv *Server, s *keyBudgetSetup, content string) *httptest.ResponseRecorder {
	t.Helper()
	in := seedBatchInput(t, srv, content)
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions",
		"completion_window": "24h",
	})
	return doReqPeer(t, srv, http.MethodPost, "/v1/batches", string(body), s.raw, loopbackPeer)
}

// g21BatchCount lists the fixture key's batches through the real
// mux and returns how many rows exist.
func g21BatchCount(t *testing.T, srv *Server, s *keyBudgetSetup) int {
	t.Helper()
	rec := doReqPeer(t, srv, http.MethodGet, "/v1/batches", "", s.raw, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("list batches: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var list batchListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode batch list: %v: %s", err, rec.Body.String())
	}
	return len(list.Data)
}

func TestG21BatchRestrictedModelRefused(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{ModelAccessMode: "restricted", AllowedModels: []string{"model-a"}})
	g20SetRPM(s, 1)
	srv := s.server(t)

	// One bad line poisons the whole batch, even with an allowed
	// line first: the refusal is atomic, not per-line.
	rec := g21CreateBatchRec(t, srv, s, g21BatchJSONL(t, "model-a", "model-b"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_model_not_allowed") {
		t.Fatalf("batch naming an unlisted model: status = %d, want 403 key_model_not_allowed: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the refused batch must never reach the engine", s.eng.chatCalls.Load())
	}
	if n := g21BatchCount(t, srv, s); n != 0 {
		t.Fatalf("batch rows = %d, want 0 — a refused create must not create a batch row", n)
	}

	// The refusal consumed no traffic slot: with RPM=1 the
	// admitted chat below still succeeds.
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("model-a", false), s.raw, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat with listed model after refused batch: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestG21BatchForcedModelRefused(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{ForcedModel: "model-a"})
	srv := s.server(t)

	rec := g21CreateBatchRec(t, srv, s, g21BatchJSONL(t, "model-b"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_forced_model") {
		t.Fatalf("batch naming a non-pinned model: status = %d, want 403 key_forced_model: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the refused batch must never reach the engine", s.eng.chatCalls.Load())
	}
	if n := g21BatchCount(t, srv, s); n != 0 {
		t.Fatalf("batch rows = %d, want 0 — a refused create must not create a batch row", n)
	}

	// The pinned model is accepted and executes.
	rec = g21CreateBatchRec(t, srv, s, g21BatchJSONL(t, "model-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("batch with the pinned model: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	created := decodeBatch(t, rec)
	done := waitBatchStatus(t, srv, created.ID, "completed")
	if done.RequestCounts.Total != 1 || done.RequestCounts.Completed != 1 {
		t.Fatalf("counts %+v, want total 1 completed 1", done.RequestCounts)
	}
	if s.eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1", s.eng.chatCalls.Load())
	}
}

func TestG21BatchAllowedModelAcceptedAndExecutes(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{ModelAccessMode: "restricted", AllowedModels: []string{"model-a"}})
	g20SetRPM(s, 1)
	srv := s.server(t)

	rec := g21CreateBatchRec(t, srv, s, g21BatchJSONL(t, "model-a", "model-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("batch with listed models: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	created := decodeBatch(t, rec)

	// The admitted create consumed exactly one traffic slot: a
	// second admitted request is now over the RPM=1 ceiling —
	// even though the batch itself executes two lines.
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("model-a", false), s.raw, loopbackPeer)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("chat after admitted batch create: status = %d, want 429 (one slot consumed): %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d before the worker runs, want 0 — the 429 chat must never reach the engine", s.eng.chatCalls.Load())
	}

	// The batch still executes both of its lines for real.
	done := waitBatchStatus(t, srv, created.ID, "completed")
	if done.RequestCounts.Total != 2 || done.RequestCounts.Completed != 2 || done.RequestCounts.Failed != 0 {
		t.Fatalf("counts %+v, want total 2 completed 2 failed 0", done.RequestCounts)
	}
	if s.eng.chatCalls.Load() != 2 {
		t.Fatalf("engine Chat calls = %d, want 2 (one per line)", s.eng.chatCalls.Load())
	}
	if n := g21BatchCount(t, srv, s); n != 1 {
		t.Fatalf("batch rows = %d, want 1", n)
	}
}

// TestG21BatchEndpointAllowlistUsesChatEndpoint pins the
// endpoint axis of the gate: a batch's lines execute as
// /v1/chat/completions, so the policy is checked as a chat
// request — a key allowed on /v1/chat/completions may batch,
// while a key allowed only on /v1/batches may not spend on chat
// through a batch file.
func TestG21BatchEndpointAllowlistUsesChatEndpoint(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{AllowedEndpoints: []string{"/v1/chat/completions"}})
	srv := s.server(t)
	rec := g21CreateBatchRec(t, srv, s, g21BatchJSONL(t, "model-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("batch with chat-endpoint key: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	created := decodeBatch(t, rec)
	waitBatchStatus(t, srv, created.ID, "completed")

	s2, _ := keyStatusSetup(t)
	setFixturePolicy(s2, config.KeyPolicy{AllowedEndpoints: []string{"/v1/batches"}})
	srv2 := s2.server(t)
	rec = g21CreateBatchRec(t, srv2, s2, g21BatchJSONL(t, "model-a"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_endpoint_not_allowed") {
		t.Fatalf("batch with batches-only key: status = %d, want 403 key_endpoint_not_allowed: %s", rec.Code, rec.Body.String())
	}
	if s2.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0", s2.eng.chatCalls.Load())
	}
	if n := g21BatchCount(t, srv2, s2); n != 0 {
		t.Fatalf("batch rows = %d, want 0", n)
	}
}
