package server

// LiteLLM v1.111.0-rc.1 failover cluster, server half (CHECKLIST
// 243-244, research-log entries 222-224): the no-replay strip (b),
// per-turn session admission (f), admission before I/O (g), and
// naming the deployment in stream error alerts (h).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/responses"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// traceCaptureEngine is a captureEngine that also exposes a route
// trace, so stream-error tests can name the deployment that
// failed the stream.
type traceCaptureEngine struct {
	*captureEngine
	trace *routetrace.RouteTrace
}

func (e *traceCaptureEngine) LastTrace() *routetrace.RouteTrace { return e.trace }

// ---------------------------------------------------------------------------
// (b) Responses fallback must never replay a tool call the user
// already saw.
// ---------------------------------------------------------------------------

func TestStripDanglingToolCalls(t *testing.T) {
	call := func(id string) provider.ToolCall {
		return provider.ToolCall{ID: id, Type: "function", Function: provider.ToolCallFunction{Name: "f", Arguments: "{}"}}
	}
	msgs := []provider.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{call("call_1"), call("call_2"), call("")}},
		{Role: "tool", Content: "sunny", ToolCallID: "call_1"},
		{Role: "user", Content: "and?"},
	}
	out, stripped := stripDanglingToolCalls(msgs)
	if stripped != 1 {
		t.Fatalf("stripped = %d, want 1 (only the dangling call_2)", stripped)
	}
	if len(out) != len(msgs) {
		t.Fatalf("len(out) = %d, want %d: messages are kept, only calls are dropped", len(out), len(msgs))
	}
	kept := out[1].ToolCalls
	if len(kept) != 2 || kept[0].ID != "call_1" || kept[1].ID != "" {
		t.Fatalf("kept calls = %+v, want [call_1, id-less]", kept)
	}
	// The caller's messages are never mutated.
	if len(msgs[1].ToolCalls) != 3 {
		t.Fatalf("input messages mutated: %d tool calls left", len(msgs[1].ToolCalls))
	}
	// Nothing dangling, nothing stripped.
	if _, n := stripDanglingToolCalls([]provider.Message{
		{Role: "assistant", ToolCalls: []provider.ToolCall{call("call_1")}},
		{Role: "tool", Content: "x", ToolCallID: "call_1"},
	}); n != 0 {
		t.Fatalf("stripped = %d, want 0 for a fully paired transcript", n)
	}
}

// TestResponsesContinuationStripsDanglingToolCall proves the
// fallback request excludes a tool call the user already saw: a
// FAILED parent response whose transcript ends in a delivered
// tool call with no result is continued — the engine receives no
// dangling call (so no fallback target can re-emit it), while
// the stored transcript keeps the full history and the strip is
// counted on the dashboard feed.
func TestResponsesContinuationStripsDanglingToolCall(t *testing.T) {
	eng := &captureEngine{resp: provider.ChatResponse{Content: "ok"}}
	srv := newTestServer(t, eng)
	transcript := []provider.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{
			ID: "call_9", Type: "function",
			Function: provider.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Paris"}`},
		}}},
	}
	raw, err := json.Marshal(transcript)
	if err != nil {
		t.Fatal(err)
	}
	srv.responses.Put(responses.Record{
		ID: "resp_parent", Model: "m", ProviderID: "prov-a",
		Status: responses.StatusFailed, TranscriptJSON: raw,
	})
	rec := doReq(t, srv, "POST", "/v1/responses",
		`{"model":"m","previous_response_id":"resp_parent","input":"continue"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := eng.lastReq()
	for _, m := range got.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID == "call_9" {
				t.Fatalf("engine request still carries dangling tool call %q: the fallback request must exclude it", tc.ID)
			}
		}
	}
	// The stored transcript keeps the call: only the engine
	// request was stripped, never the record.
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created response: %v", err)
	}
	stored, ok := srv.responses.Get(created.ID)
	if !ok {
		t.Fatal("created response was not stored")
	}
	var storedTranscript []provider.Message
	if err := json.Unmarshal(stored.TranscriptJSON, &storedTranscript); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range storedTranscript {
		for _, tc := range m.ToolCalls {
			if tc.ID == "call_9" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("stored transcript lost the tool call: the strip must not rewrite history")
	}
	// The strip is counted on the responses feed.
	feed := doReq(t, srv, "GET", "/dashboard/responses", "", "")
	var f responsesFeed
	if err := json.Unmarshal(feed.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode responses feed: %v", err)
	}
	if f.StrippedToolCalls < 1 {
		t.Fatalf("stripped_tool_calls = %d, want >= 1", f.StrippedToolCalls)
	}
}

// ---------------------------------------------------------------------------
// (f) Per-turn token admission for long-lived Responses sessions.
// ---------------------------------------------------------------------------

// TestResponsesSessionTokenAdmission proves a turn's tokens are
// counted before the next turn is admitted: a session at 950
// counted tokens with a 1000-token cap admits a small turn (and
// the stored record's cumulative total grows), but refuses a
// turn whose prompt would push it over the cap — 429, zero
// engine calls, counted on the feed.
func TestResponsesSessionTokenAdmission(t *testing.T) {
	eng := &captureEngine{resp: provider.ChatResponse{Content: "ok"}}
	srv := newTestServer(t, eng, WithResponsesSessionTokenCap(1000))
	base := []provider.Message{{Role: "user", Content: "hi"}}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	srv.responses.Put(responses.Record{
		ID: "resp_parent", Model: "m", ProviderID: "prov-a",
		Status: responses.StatusCompleted, TranscriptJSON: raw,
		SessionInputTokens: 900, SessionOutputTokens: 50, // 950 used
	})
	before := eng.reqCount()
	rec := doReq(t, srv, "POST", "/v1/responses",
		`{"model":"m","previous_response_id":"resp_parent","input":"ok"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("small turn: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if eng.reqCount() != before+1 {
		t.Fatalf("engine calls = %d, want %d: the admitted turn must reach the engine", eng.reqCount(), before+1)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created response: %v", err)
	}
	stored, ok := srv.responses.Get(created.ID)
	if !ok {
		t.Fatal("created response was not stored")
	}
	total := stored.SessionInputTokens + stored.SessionOutputTokens
	if total <= 950 {
		t.Fatalf("session total = %d, want > 950: the turn's tokens must accumulate", total)
	}
	if stored.TurnInputTokens <= 0 {
		t.Fatalf("turn input tokens = %d, want > 0", stored.TurnInputTokens)
	}
	// A turn whose prompt would push the session over the cap is
	// refused before any provider is dialled.
	longInput := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 30)
	body, _ := json.Marshal(map[string]any{
		"model": "m", "previous_response_id": "resp_parent", "input": longInput,
	})
	before = eng.reqCount()
	rec = doReq(t, srv, "POST", "/v1/responses", string(body), "test-key-123")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap turn: status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "session_token_limit") {
		t.Fatalf("over-cap turn body = %s, want the session_token_limit type", rec.Body.String())
	}
	if eng.reqCount() != before {
		t.Fatalf("engine calls = %d, want %d: the refused turn must never reach the engine", eng.reqCount(), before)
	}
	feed := doReq(t, srv, "GET", "/dashboard/responses", "", "")
	var f responsesFeed
	if err := json.Unmarshal(feed.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode responses feed: %v", err)
	}
	if f.SessionAdmissionRefused != 1 {
		t.Fatalf("session_admission_refused = %d, want 1", f.SessionAdmissionRefused)
	}
	// The feed carries the configured cap itself (CHECKLIST
	// 263–264): the dashboard names the cap the refusals were
	// judged against, and 0 would mean counting-only.
	if f.SessionTokenCap != 1000 {
		t.Fatalf("session_token_cap = %d, want 1000", f.SessionTokenCap)
	}
	// The feed entry carries the session's cumulative tokens.
	found := false
	for _, e := range f.Responses {
		if e.ID == created.ID {
			found = true
			if e.SessionTokens != total {
				t.Fatalf("entry session_tokens = %d, want %d", e.SessionTokens, total)
			}
		}
	}
	if !found {
		t.Fatalf("responses feed has no entry for %s", created.ID)
	}
}

// TestResponsesSessionTokenCapOffCountsWithoutRefusing proves the
// default (no cap configured): turns are counted and reported,
// never refused.
func TestResponsesSessionTokenCapOffCountsWithoutRefusing(t *testing.T) {
	eng := &captureEngine{resp: provider.ChatResponse{Content: "ok"}}
	srv := newTestServer(t, eng) // no cap option: 0 = counting only
	raw, err := json.Marshal([]provider.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	srv.responses.Put(responses.Record{
		ID: "resp_parent", Model: "m", ProviderID: "prov-a",
		Status: responses.StatusCompleted, TranscriptJSON: raw,
		SessionInputTokens: 900000, SessionOutputTokens: 50000,
	})
	rec := doReq(t, srv, "POST", "/v1/responses",
		`{"model":"m","previous_response_id":"resp_parent","input":"ok"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no cap configured: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// (g) Refuse blocked and over-limit keys before reading the body.
// ---------------------------------------------------------------------------

// TestPreBodyAdmissionRefusesOverBudgetKey proves a key already
// over its budget is refused BEFORE the body is read: the same
// request with a malformed body answers 402 (not 400) — the
// handler never decoded it — and the engine is never called.
func TestPreBodyAdmissionRefusesOverBudgetKey(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 0.01, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 1000, 1000) // $0.04 of a $0.01 cap
	rec := doReq(t, srv, "POST", "/v1/chat/completions", `{"model":`, s.raw)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("garbage body + over-budget key: status = %d, want 402 (admission before decode): %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0", s.eng.chatCalls.Load())
	}
	feed := doReq(t, srv, "GET", "/dashboard/guardrails", "", "")
	var f guardrailsFeed
	if err := json.Unmarshal(feed.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode guardrails feed: %v", err)
	}
	if f.EarlyRefusals < 1 {
		t.Fatalf("early_refusals = %d, want >= 1", f.EarlyRefusals)
	}
	// Control: the same garbage body under an uncapped key is a
	// 400 — the body IS read and decoded when admission passes.
	s2 := newKeyBudgetSetup(t)
	srv2 := s2.server(t)
	if err := s2.led.SetKeyBudget(context.Background(), s2.id, 10, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, srv2, "POST", "/v1/chat/completions", `{"model":`, s2.raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage body + uncapped key: status = %d, want 400 (body read normally): %s", rec.Code, rec.Body.String())
	}
}

// TestPreBodyAdmissionRefusesOverTrafficKey proves a key already
// at its requests-per-minute ceiling is refused BEFORE the body
// is read: after one admitted request consumes the single RPM
// slot, a second request with a malformed body answers 429 (not
// 400), and the engine is called exactly once.
func TestPreBodyAdmissionRefusesOverTrafficKey(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	key, raw, err := config.IssueKey("rpm-one", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	key.RequestsPerMinute = 1
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi"}}}
	srv := newTestServer(t, eng, WithConfig(cfg))
	body := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	rec := doReq(t, srv, "POST", "/v1/chat/completions", body, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1", eng.chatCalls.Load())
	}
	// The RPM slot is spent: a malformed body must still answer
	// 429 — admission runs before decode.
	rec = doReq(t, srv, "POST", "/v1/chat/completions", `{"model":`, raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("garbage body + over-traffic key: status = %d, want 429 (admission before decode): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_rate_limit") {
		t.Fatalf("body = %s, want the key_rate_limit type", rec.Body.String())
	}
	if eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1: the refused request must never reach the engine", eng.chatCalls.Load())
	}
	// The peek records nothing: the refusal itself consumed no
	// slot, so the count of early refusals is the only trace.
	feed := doReq(t, srv, "GET", "/dashboard/guardrails", "", "")
	var f guardrailsFeed
	if err := json.Unmarshal(feed.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode guardrails feed: %v", err)
	}
	if f.EarlyRefusals < 1 {
		t.Fatalf("early_refusals = %d, want >= 1", f.EarlyRefusals)
	}
}

// ---------------------------------------------------------------------------
// (h) Name the deployment in stream error alerts.
// ---------------------------------------------------------------------------

// stubProviderLister is a DashboardProviderLister with a fixed
// ID -> display-name mapping, for the deployment-naming tests.
type stubProviderLister struct {
	providers []provider.DashboardProvider
}

func (l stubProviderLister) DashboardProviders() []provider.DashboardProvider {
	return l.providers
}

// TestStreamErrorNamesDeploymentByDisplayName proves the alert
// names the deployment the way the operator configured it: the
// trace carries the registry ID, the frame carries the display
// name — an alert naming "prov_…" cannot be triaged at a glance.
func TestStreamErrorNamesDeploymentByDisplayName(t *testing.T) {
	eng := &traceCaptureEngine{
		captureEngine: &captureEngine{
			deltas:   []string{"hel", "lo "},
			chunkErr: errors.New("boom"),
			resp:     provider.ChatResponse{Content: "hello"},
		},
		trace: &routetrace.RouteTrace{Attempts: []routetrace.AttemptTrace{
			{Provider: "prov-123", Outcome: "stream_error"},
		}},
	}
	lister := stubProviderLister{providers: []provider.DashboardProvider{
		{ID: "prov-123", Name: "flaky"},
		{ID: "prov-456", Name: "live"},
	}}
	srv := newTestServer(t, eng, WithDashboardProviders(lister))
	body := `{"model":"m","messages":[{"role":"user","content":"hello"}],"stream":true}`
	rec := doReq(t, srv, "POST", "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE with a terminal error frame): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"deployment":"flaky"`) {
		t.Fatalf("SSE body = %s, want the error frame to name the display name flaky, not the registry id", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "prov-123") {
		t.Fatalf("SSE body = %s, want no registry id in the alert", rec.Body.String())
	}
}

// TestStreamErrorUnknownDeploymentKeepsItsID proves the fallback:
// a deployment the lister does not know is named by its ID,
// never dropped and never guessed.
func TestStreamErrorUnknownDeploymentKeepsItsID(t *testing.T) {
	eng := &traceCaptureEngine{
		captureEngine: &captureEngine{
			deltas:   []string{"hel"},
			chunkErr: errors.New("boom"),
			resp:     provider.ChatResponse{Content: "hello"},
		},
		trace: &routetrace.RouteTrace{Attempts: []routetrace.AttemptTrace{
			{Provider: "prov-unknown", Outcome: "stream_error"},
		}},
	}
	srv := newTestServer(t, eng, WithDashboardProviders(stubProviderLister{}))
	body := `{"model":"m","messages":[{"role":"user","content":"hello"}],"stream":true}`
	rec := doReq(t, srv, "POST", "/v1/chat/completions", body, "test-key-123")
	if !strings.Contains(rec.Body.String(), `"deployment":"prov-unknown"`) {
		t.Fatalf("SSE body = %s, want the unknown deployment named by its id", rec.Body.String())
	}
}

// TestStreamErrorNamesDeployment proves a committed stream that
// fails carries the deployment in its SSE error frame: the alert
// names the provider that failed it, and the naming is counted
// on the guardrails feed.
func TestStreamErrorNamesDeployment(t *testing.T) {
	eng := &traceCaptureEngine{
		captureEngine: &captureEngine{
			deltas:   []string{"hel", "lo "},
			chunkErr: errors.New("boom"),
			resp:     provider.ChatResponse{Content: "hello"},
		},
		trace: &routetrace.RouteTrace{Attempts: []routetrace.AttemptTrace{
			{Provider: "prov-a", Outcome: "stream_error"},
		}},
	}
	srv := newTestServer(t, eng)
	body := `{"model":"m","messages":[{"role":"user","content":"hello"}],"stream":true}`
	rec := doReq(t, srv, "POST", "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE with a terminal error frame): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"deployment":"prov-a"`) {
		t.Fatalf("SSE body = %s, want the error frame to name deployment prov-a", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream provider error") {
		t.Fatalf("SSE body = %s, want the upstream error frame", rec.Body.String())
	}
	feed := doReq(t, srv, "GET", "/dashboard/guardrails", "", "")
	var f guardrailsFeed
	if err := json.Unmarshal(feed.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode guardrails feed: %v", err)
	}
	if f.StreamErrorsNamed < 1 {
		t.Fatalf("stream_errors_named = %d, want >= 1", f.StreamErrorsNamed)
	}
}

// TestStreamErrorWithoutTraceNamesNothing proves the deployment is
// never invented: when the engine exposes no trace, the error
// frame carries no deployment field at all.
func TestStreamErrorWithoutTraceNamesNothing(t *testing.T) {
	eng := &captureEngine{
		deltas:   []string{"hel"},
		chunkErr: errors.New("boom"),
		resp:     provider.ChatResponse{Content: "hello"},
	}
	srv := newTestServer(t, eng)
	body := `{"model":"m","messages":[{"role":"user","content":"hello"}],"stream":true}`
	rec := doReq(t, srv, "POST", "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE with a terminal error frame): %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"deployment"`) {
		t.Fatalf("SSE body = %s, want no deployment field when no trace names one", rec.Body.String())
	}
}
