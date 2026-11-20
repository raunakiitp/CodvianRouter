package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/feedback"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// autoDecisionEngine wraps an Engine and reports a fixed last
// auto-routing decision, like router.Engine does after a real
// "auto/*" request.
type autoDecisionEngine struct {
	Engine
	dec autorouter.Decision
}

func (e *autoDecisionEngine) LastAutoDecision() (autorouter.Decision, bool) {
	if e.dec.ID == "" {
		return autorouter.Decision{}, false
	}
	return e.dec, true
}

func openTestFeedbackStore(t *testing.T) *feedback.Store {
	t.Helper()
	s, err := feedback.Open(filepath.Join(t.TempDir(), "feedback.db"))
	if err != nil {
		t.Fatalf("feedback.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func decodeFeedbackSummary(t *testing.T, recBody []byte) feedback.Summary {
	t.Helper()
	var sum feedback.Summary
	if err := json.Unmarshal(recBody, &sum); err != nil {
		t.Fatalf("decode feedback summary %s: %v", recBody, err)
	}
	return sum
}

func TestFeedbackEmptyState(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithFeedback(openTestFeedbackStore(t)))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/feedback", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/feedback: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sum := decodeFeedbackSummary(t, rec.Body.Bytes())
	if sum.Total != 0 || len(sum.Tiers) != 4 || len(sum.Recent) != 0 {
		t.Errorf("empty summary = %+v, want total 0, 4 tiers, no recent", sum)
	}
	if sum.Enabled {
		t.Error("empty summary: enabled=true, want false (learning defaults off)")
	}
	if sum.MinSamples != feedback.MinSamples || sum.MaxNudge != feedback.MaxNudge {
		t.Errorf("rule params = min %d max %v, want %d/%v", sum.MinSamples, sum.MaxNudge, feedback.MinSamples, feedback.MaxNudge)
	}
	for _, row := range sum.Tiers {
		if row.Weight != 0 || row.Samples != 0 || row.Active {
			t.Errorf("tier %s: %+v, want all-zero inactive", row.Tier, row)
		}
	}
}

func TestFeedbackNoStore(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	if rec := doReq(t, srv, http.MethodGet, "/dashboard/feedback", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET without store: status = %d, want 503", rec.Code)
	}
	if rec := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{"decision_id":"dec-1","signal":"accept"}`, "test-key-123"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST without store: status = %d, want 503", rec.Code)
	}
	if rec := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{"decision_id":"dec-1","signal":"accept"}`, "test-key-123"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST without store: status = %d, want 503", rec.Code)
	}
}

// TestFeedbackPostValidation covers the whole POST validation
// ladder: bad signal 400, unknown decision 404, happy path 201,
// idempotent repeat 200, conflicting signal 409.
func TestFeedbackPostValidation(t *testing.T) {
	store := openTestFeedbackStore(t)
	router := autorouter.New(autorouter.Config{DefaultModel: "model-x"})
	dec := router.Classify("auto", "Tell me a funny joke about cats", 0)
	// Register the decision as served (what the server does on a
	// real /v1 response) — an unregistered id must stay unknown.
	if err := store.RegisterDecision(context.Background(), feedback.DecisionRef{
		ID: dec.ID, Tier: string(dec.Tier), Model: "model-x", LatencyMicros: dec.LatencyMicros,
	}); err != nil {
		t.Fatalf("RegisterDecision: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(router), WithFeedback(store))

	badSignal := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{"decision_id":"`+dec.ID+`","signal":"love"}`, "test-key-123")
	if badSignal.Code != http.StatusBadRequest {
		t.Errorf("bad signal: status = %d, want 400", badSignal.Code)
	}
	unknown := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{"decision_id":"dec-never-served","signal":"accept"}`, "test-key-123")
	if unknown.Code != http.StatusNotFound {
		t.Errorf("unknown decision: status = %d, want 404", unknown.Code)
	}
	empty := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{"decision_id":"","signal":"accept"}`, "test-key-123")
	if empty.Code != http.StatusBadRequest {
		t.Errorf("empty decision id: status = %d, want 400", empty.Code)
	}
	malformed := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{not json`, "test-key-123")
	if malformed.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status = %d, want 400", malformed.Code)
	}

	created := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{"decision_id":"`+dec.ID+`","signal":"accept"}`, "test-key-123")
	if created.Code != http.StatusCreated {
		t.Fatalf("happy path: status = %d, body = %s, want 201", created.Code, created.Body.String())
	}
	var ev feedback.Event
	if err := json.Unmarshal(created.Body.Bytes(), &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if ev.DecisionID != dec.ID || ev.Signal != feedback.SignalAccept || ev.Tier != string(dec.Tier) {
		t.Errorf("event = %+v, want decision %s / accept / tier %s", ev, dec.ID, dec.Tier)
	}

	// Same signal again: 200, same event id (idempotent retry).
	repeat := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{"decision_id":"`+dec.ID+`","signal":"accept"}`, "test-key-123")
	if repeat.Code != http.StatusOK {
		t.Errorf("repeat accept: status = %d, want 200", repeat.Code)
	}
	var ev2 feedback.Event
	if err := json.Unmarshal(repeat.Body.Bytes(), &ev2); err != nil {
		t.Fatal(err)
	}
	if ev2.ID != ev.ID {
		t.Errorf("repeat event id = %d, want %d (no duplicate row)", ev2.ID, ev.ID)
	}

	// Different signal for the same decision: 409.
	conflict := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{"decision_id":"`+dec.ID+`","signal":"discard"}`, "test-key-123")
	if conflict.Code != http.StatusConflict {
		t.Errorf("conflicting signal: status = %d, want 409", conflict.Code)
	}

	// GET now shows exactly one signal, on the right tier, with
	// zero weight (1 sample < 10 minimum).
	rec := doReq(t, srv, http.MethodGet, "/dashboard/feedback", "", "")
	sum := decodeFeedbackSummary(t, rec.Body.Bytes())
	if sum.Total != 1 {
		t.Errorf("total = %d, want 1", sum.Total)
	}
	for _, row := range sum.Tiers {
		if row.Tier == string(dec.Tier) {
			if row.Samples != 1 || row.Counts.Accept != 1 || row.Weight != 0 || row.Active {
				t.Errorf("tier %s: %+v, want 1 sample / 1 accept / weight 0 / inactive", row.Tier, row)
			}
		}
	}
}

// TestFeedbackPreviewDecisionIsUnknown: a decision that was only
// classified by the dashboard preview (GET /dashboard/auto-route)
// is never registered, so feedback for it is a 404 — previews can
// never train the router.
func TestFeedbackPreviewDecisionIsUnknown(t *testing.T) {
	store := openTestFeedbackStore(t)
	router := autorouter.New(autorouter.Config{DefaultModel: "model-x"})
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(router), WithFeedback(store))

	rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto&prompt="+url.QueryEscape("Tell me a funny joke about cats"), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("classify: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dec autorouter.Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatal(err)
	}
	if dec.ID == "" {
		t.Fatal("preview decision has no ID")
	}
	post := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{"decision_id":"`+dec.ID+`","signal":"accept"}`, "test-key-123")
	if post.Code != http.StatusNotFound {
		t.Errorf("preview decision feedback: status = %d, want 404", post.Code)
	}
}

// TestFeedbackServedDecisionHeader wires the full path: a real
// /v1/chat/completions auto request returns the decision id in a
// header, and that id accepts feedback.
func TestFeedbackServedDecisionHeader(t *testing.T) {
	store := openTestFeedbackStore(t)
	router := autorouter.New(autorouter.Config{DefaultModel: "model-x"})
	dec := router.Classify("auto", "Tell me a funny joke about cats", 0)
	eng := &autoDecisionEngine{
		Engine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ha", Model: "model-x"}},
		dec:    dec,
	}
	// The engine above needs its Chat to work; fakeEngine is the
	// embedded Engine, so Chat delegates to it.
	srv := newTestServer(t, eng, WithAutoRouter(router), WithFeedback(store))

	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"Tell me a funny joke about cats"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	id := rec.Header().Get("X-Codvian-Auto-Decision-Id")
	if id == "" {
		t.Fatal("response lacks X-Codvian-Auto-Decision-Id")
	}
	if id != dec.ID {
		t.Errorf("header decision id = %q, want the engine's decision %q", id, dec.ID)
	}
	posted := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{"decision_id":"`+id+`","signal":"discard"}`, "test-key-123")
	if posted.Code != http.StatusCreated {
		t.Errorf("feedback for served decision: status = %d, body = %s, want 201", posted.Code, posted.Body.String())
	}
}

// TestFeedbackWeightsReachTheRouter: with learning enabled and 10
// accepts on chat, a borderline chat prompt that raw-falls-back
// routes to chat — and the decision reports the learned nudge.
func TestFeedbackWeightsReachTheRouter(t *testing.T) {
	store := openTestFeedbackStore(t)
	store.SetEnabled(true)
	router := autorouter.New(autorouter.Config{DefaultModel: "model-x"})
	prompt := "Tell me a funny joke about cats"
	raw := router.Classify("auto", prompt, 0)
	if raw.Tier != autorouter.TierChat {
		t.Fatalf("setup: raw tier = %s, want chat", raw.Tier)
	}
	threshold := raw.Confidence + 0.02
	if threshold > 1 {
		t.Fatalf("setup: raw confidence %.3f too close to 1", raw.Confidence)
	}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("dec-seed-%d", i)
		if err := store.RegisterDecision(context.Background(), feedback.DecisionRef{ID: id, Tier: "chat", Model: "model-x"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.RecordSignal(context.Background(), id, feedback.SignalAccept); err != nil {
			t.Fatal(err)
		}
	}
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(router), WithFeedback(store))

	// Without the threshold override the prompt routes to chat
	// either way; with the override just above the raw score, only
	// the learned nudge can push it over.
	rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto&prompt="+url.QueryEscape(prompt)+
		fmt.Sprintf("&threshold=%.3f", threshold), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("classify with learned weights: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dec autorouter.Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatal(err)
	}
	if dec.Fallback || dec.Tier != autorouter.TierChat {
		t.Errorf("learned decision: tier=%s fallback=%v, want chat/routed: %+v", dec.Tier, dec.Fallback, dec)
	}
	if got := dec.LearnedNudge["chat"]; got != 0.05 {
		t.Errorf("LearnedNudge[chat] = %v, want 0.05", got)
	}
	if got := dec.LearnedSamples["chat"]; got != 10 {
		t.Errorf("LearnedSamples[chat] = %v, want 10", got)
	}
	if !strings.Contains(dec.Reason, "learned nudge") {
		t.Errorf("reason %q does not name the nudge", dec.Reason)
	}

	// The GET summary agrees: chat active at +0.05.
	sum := decodeFeedbackSummary(t, doReq(t, srv, http.MethodGet, "/dashboard/feedback", "", "").Body.Bytes())
	for _, row := range sum.Tiers {
		if row.Tier == "chat" {
			if !row.Active || row.Weight != 0.05 || row.Samples != 10 {
				t.Errorf("summary chat row = %+v, want active/0.05/10", row)
			}
		}
	}
}

// TestFeedbackServedDecisionStoresScores: a real served auto
// request registers its decision WITH the raw per-tier scores, so
// the A/B evaluation can replay it later. The proof is through the
// public API: after one signal, the summary's eval reports the
// decision as evaluated (not excluded for missing scores).
func TestFeedbackServedDecisionStoresScores(t *testing.T) {
	store := openTestFeedbackStore(t)
	router := autorouter.New(autorouter.Config{DefaultModel: "model-x"})
	dec := router.Classify("auto", "Tell me a funny joke about cats", 0)
	if len(dec.Scores) != 4 {
		t.Fatalf("setup: decision scores = %v, want all 4 tiers", dec.Scores)
	}
	eng := &autoDecisionEngine{
		Engine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ha", Model: "model-x"}},
		dec:    dec,
	}
	srv := newTestServer(t, eng, WithAutoRouter(router), WithFeedback(store))
	// Cutoff before the request: the signal below lands in the
	// test set (the default 70/30 split would put a lone signal
	// in train and leave nothing to evaluate).
	before := time.Now()
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"Tell me a funny joke about cats"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	id := rec.Header().Get("X-Codvian-Auto-Decision-Id")
	if id == "" {
		t.Fatal("response lacks X-Codvian-Auto-Decision-Id")
	}
	posted := doReq(t, srv, http.MethodPost, "/dashboard/feedback", `{"decision_id":"`+id+`","signal":"discard"}`, "test-key-123")
	if posted.Code != http.StatusCreated {
		t.Fatalf("feedback for served decision: status = %d, body = %s, want 201", posted.Code, posted.Body.String())
	}
	// A second before the request: timestamps are millisecond
	// precision, so the cutoff must not share the request's
	// millisecond (a signal in the same millisecond would sort
	// into train).
	ev, err := store.EvaluateAt(context.Background(), before.Add(-time.Second))
	if err != nil {
		t.Fatalf("EvaluateAt: %v", err)
	}
	// One signal, no train evidence: no nudge exists, so the
	// policies agree — verdict no-flip. The decision must count
	// as evaluated (scores present), not excluded.
	if ev.Evaluated != 1 || ev.Excluded != 0 {
		t.Errorf("eval evaluated/excluded = %d/%d, want 1/0 (scores must round-trip from the served decision)", ev.Evaluated, ev.Excluded)
	}
	if ev.Verdict != feedback.EvalNoFlip {
		t.Errorf("eval verdict = %q, want no-flip for a single signal with no train evidence", ev.Verdict)
	}
	// And the dashboard summary carries the same eval.
	sum := decodeFeedbackSummary(t, doReq(t, srv, http.MethodGet, "/dashboard/feedback", "", "").Body.Bytes())
	if sum.Eval == nil {
		t.Fatalf("summary has no eval — the A/B evaluation should always be present")
	}
}
