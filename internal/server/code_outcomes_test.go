package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/feedback"
)

// TestCodeOutcomesEmpty: with nothing recorded the evidence feed
// answers an honest empty list with the documented default and
// floor — never invented traffic.
func TestCodeOutcomesEmpty(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/code-outcomes", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/code-outcomes: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp codeOutcomesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Models) != 0 {
		t.Errorf("models = %+v, want empty", resp.Models)
	}
	if resp.DefaultSolveRate != autorouter.DefaultSolveRate || resp.MinSamples != autorouter.MinCodeSamples {
		t.Errorf("defaults = %.2f/%d, want %.2f/%d", resp.DefaultSolveRate, resp.MinSamples, autorouter.DefaultSolveRate, autorouter.MinCodeSamples)
	}
}

// TestFeedbackPostFeedsCodeOutcomes: a recorded accept on a
// coding-tier decision lands in the auto/code evidence feed;
// a reasoning-tier accept does not (the mode's evidence is
// coding-task outcomes only); a duplicate signal is not
// double-counted.
func TestFeedbackPostFeedsCodeOutcomes(t *testing.T) {
	store := openTestFeedbackStore(t)
	router := autorouter.New(autorouter.Config{DefaultModel: "model-code"})
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(router), WithFeedback(store))

	post := func(decID, signal string) int {
		rec := doReq(t, srv, http.MethodPost, "/dashboard/feedback",
			`{"decision_id":"`+decID+`","signal":"`+signal+`"}`, "test-key-123")
		return rec.Code
	}
	get := func() codeOutcomesResponse {
		rec := doReq(t, srv, http.MethodGet, "/dashboard/code-outcomes", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/code-outcomes: status = %d", rec.Code)
		}
		var resp codeOutcomesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	// Ten coding-tier decisions for model-code, all accepted.
	for i := 0; i < 10; i++ {
		dec := router.Classify("auto", "Write a Python function that sorts a list of numbers", 0)
		if err := store.RegisterDecision(context.Background(), feedback.DecisionRef{
			ID: dec.ID, Tier: "coding", Model: "model-code", LatencyMicros: dec.LatencyMicros,
		}); err != nil {
			t.Fatalf("RegisterDecision: %v", err)
		}
		if code := post(dec.ID, "accept"); code != http.StatusCreated {
			t.Fatalf("POST accept %d: status = %d, want 201", i, code)
		}
	}
	resp := get()
	if len(resp.Models) != 1 {
		t.Fatalf("models = %+v, want exactly [model-code]", resp.Models)
	}
	row := resp.Models[0]
	if row.Model != "model-code" || row.Attempts != 10 || row.Solved != 10 {
		t.Errorf("row = %+v, want model-code / 10 attempts / 10 solved", row)
	}
	if !row.Evidence || row.SolveRate != 1 {
		t.Errorf("row = %+v, want measured evidence with solve rate 1.0 (10 accepts at the floor)", row)
	}

	// A reasoning-tier accept for another model must NOT enter
	// the code-mode evidence (different task class).
	dec := router.Classify("auto", "Prove that the square root of two is irrational", 0)
	if err := store.RegisterDecision(context.Background(), feedback.DecisionRef{
		ID: dec.ID, Tier: "reasoning", Model: "model-reason", LatencyMicros: dec.LatencyMicros,
	}); err != nil {
		t.Fatalf("RegisterDecision: %v", err)
	}
	if code := post(dec.ID, "accept"); code != http.StatusCreated {
		t.Fatalf("POST reasoning accept: status = %d, want 201", code)
	}
	if resp := get(); len(resp.Models) != 1 {
		t.Errorf("models after reasoning accept = %+v, want still only [model-code]", resp.Models)
	}
}

// TestAutoRouteCodeShowsScores: the dashboard preview endpoint
// resolves auto/code end to end and the decision carries the
// cost-per-solved-task breakdown with the no-evidence flag.
func TestAutoRouteCodeShowsScores(t *testing.T) {
	router := autorouter.New(autorouter.Config{
		TierModels: map[autorouter.Tier][]string{
			autorouter.TierCoding: {"cheap-fail", "pricey-solve"},
		},
		CodePrices: func(m string) (float64, bool) {
			switch m {
			case "cheap-fail":
				return 0.50, true
			case "pricey-solve":
				return 5.00, true
			}
			return 0, false
		},
		CodeOutcomes: autorouter.NewOutcomeStore(),
	})
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(router))
	rec := doReq(t, srv, http.MethodGet,
		"/dashboard/auto-route?model=auto%2Fcode&prompt="+`Write+a+Python+function+that+sorts+a+list`, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/auto-route auto/code: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dec autorouter.Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatalf("decode decision: %v (body %s)", err, rec.Body.String())
	}
	if dec.Model != "cheap-fail" {
		// No evidence: price order wins, so the cheap model
		// resolves — the honest no-evidence default.
		t.Errorf("model = %q, want cheap-fail (no evidence: price order at the neutral default)", dec.Model)
	}
	if len(dec.CodeScores) != 2 {
		t.Fatalf("code_scores = %+v, want 2 candidates", dec.CodeScores)
	}
	if !dec.CodeNoEvidence {
		t.Error("no-evidence decision must set code_no_evidence")
	}
}

// TestCodeOutcomesRestoredAtStartup (item 265 — the restart proof
// at the server level): outcomes recorded against a feedback store,
// the store closed, a fresh handle opened over the same DB file, a
// new server built on it — the evidence feed shows the restored
// tally with persisted=true, so the model keeps its measured rate
// across a restart instead of re-earning it.
func TestCodeOutcomesRestoredAtStartup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "feedback.db")
	store, err := feedback.Open(path)
	if err != nil {
		t.Fatalf("feedback.Open: %v", err)
	}
	for i := 0; i < 10; i++ {
		id := "dec-" + string(rune('a'+i)) // a..j, one per outcome
		if err := store.RegisterDecision(ctx, feedback.DecisionRef{ID: id, Tier: "coding", Model: "model-code", LatencyMicros: 1}); err != nil {
			t.Fatalf("RegisterDecision: %v", err)
		}
		if _, _, err := store.RecordSignal(ctx, id, feedback.SignalAccept); err != nil {
			t.Fatalf("RecordSignal: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Fresh handle over the same file: the restart.
	restored, err := feedback.Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	srv := newTestServer(t, &fakeEngine{}, WithFeedback(restored))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/code-outcomes", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/code-outcomes: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp codeOutcomesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Persisted {
		t.Error("persisted = false, want true (feedback store wired and restore ran)")
	}
	if len(resp.Models) != 1 {
		t.Fatalf("models = %+v, want exactly [model-code] restored from the store", resp.Models)
	}
	row := resp.Models[0]
	if row.Model != "model-code" || row.Attempts != 10 || row.Solved != 10 {
		t.Errorf("row = %+v, want model-code / 10 attempts / 10 solved", row)
	}
	if !row.Evidence || row.SolveRate != 1 {
		t.Errorf("row = %+v, want measured evidence with solve rate 1.0", row)
	}
}
