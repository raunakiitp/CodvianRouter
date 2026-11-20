package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/feedback"
	"github.com/raunakiitp/codvian-router/internal/learned"
)

func decodeLearnedSummary(t *testing.T, body []byte) learned.Summary {
	t.Helper()
	var sum learned.Summary
	if err := json.Unmarshal(body, &sum); err != nil {
		t.Fatalf("decode learned summary %s: %v", body, err)
	}
	return sum
}

// seedLearnedEvidence records n labelled outcomes in store,
// through the real RegisterDecision + RecordSignal path, with
// the same separable score->outcome shape the learned package's
// own tests use: the served tier's own heuristic score is 0.9
// on wins and 0.3 on losses, other tiers 0.1, tiers
// round-robin, win/loss alternating by round — so every tier
// sees both outcomes and the model can learn the mapping. The
// 1 ms sleep between signals keeps signal timestamps ordered
// (the learned model splits train/test by time).
func seedLearnedEvidence(t *testing.T, store *feedback.Store, n int) {
	t.Helper()
	tiers := []string{"coding", "chat", "reasoning", "quick-qa"}
	for i := 0; i < n; i++ {
		tier := tiers[i%len(tiers)]
		win := (i/len(tiers))%2 == 0
		own := 0.3
		sig := feedback.SignalDiscard
		if win {
			own = 0.9
			sig = feedback.SignalAccept
		}
		scores := map[string]float64{"coding": 0.1, "chat": 0.1, "reasoning": 0.1, "quick-qa": 0.1}
		scores[tier] = own
		id := fmt.Sprintf("dec-learned-%d", i)
		if err := store.RegisterDecision(context.Background(), feedback.DecisionRef{
			ID: id, Tier: tier, Model: "seed-model", TierScores: scores,
		}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		if _, _, err := store.RecordSignal(context.Background(), id, sig); err != nil {
			t.Fatalf("signal %s: %v", id, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLearnedEndpointNoStore(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	if rec := doReq(t, srv, http.MethodGet, "/dashboard/learned", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /dashboard/learned without a store: status = %d, want 503", rec.Code)
	}
}

func TestLearnedEndpointEmptyState(t *testing.T) {
	router := autorouter.New(autorouter.Config{DefaultModel: "model-x"})
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(router), WithFeedback(openTestFeedbackStore(t)))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/learned", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/learned: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sum := decodeLearnedSummary(t, rec.Body.Bytes())
	if sum.Status != learned.StatusOff || sum.Evidence != 0 {
		t.Errorf("empty store: status = %q evidence = %d, want off/0 (honest empty state, never a fake accuracy)", sum.Status, sum.Evidence)
	}
	if sum.Enabled {
		t.Error("flag off by default: enabled = true, want false")
	}
}

func TestLearnedEndpointLearningWithFewSignals(t *testing.T) {
	router := autorouter.New(autorouter.Config{DefaultModel: "model-x"})
	store := openTestFeedbackStore(t)
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(router), WithFeedback(store))
	seedLearnedEvidence(t, store, 5)
	rec := doReq(t, srv, http.MethodGet, "/dashboard/learned", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/learned: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sum := decodeLearnedSummary(t, rec.Body.Bytes())
	if sum.Status != learned.StatusLearning || sum.Evidence != 5 {
		t.Errorf("5 signals: status = %q evidence = %d, want learning/5", sum.Status, sum.Evidence)
	}
}

func TestLearnedEndpointActivatesOnRealEvidence(t *testing.T) {
	router := autorouter.New(autorouter.Config{DefaultModel: "model-x"})
	store := openTestFeedbackStore(t)
	srv := newTestServer(t, &fakeEngine{},
		WithAutoRouter(router), WithFeedback(store), WithLearnedRouter(true))
	seedLearnedEvidence(t, store, 100)
	rec := doReq(t, srv, http.MethodGet, "/dashboard/learned", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/learned: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sum := decodeLearnedSummary(t, rec.Body.Bytes())
	if sum.Status != learned.StatusActive {
		t.Fatalf("100 separable outcomes: status = %q (note: %s), calibration error %.3f, log-loss %.3f vs base %.3f, want active",
			sum.Status, sum.GateNote, sum.CalibrationErr, sum.LogLoss, sum.BaseLogLoss)
	}
	if sum.Evidence != 100 {
		t.Errorf("evidence = %d, want 100", sum.Evidence)
	}
	if sum.CalibrationErr > learned.MaxECE {
		t.Errorf("calibration error = %.3f, want <= %.2f", sum.CalibrationErr, learned.MaxECE)
	}
	// The live auto-route preview must now carry the learned
	// win probabilities — the whole path (store -> refresher ->
	// router -> decision) is wired, not just the feed.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto&prompt=Write+a+Python+function+to+sort+a+list", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/auto-route: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dec autorouter.Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatalf("decode auto-route decision %s: %v", rec.Body.String(), err)
	}
	if !dec.LearnedActive {
		t.Errorf("auto-route decision: learned_active = false, want true (model is active)")
	}
	// The bar here is deliberately directional, not 0.7+: the
	// seed vectors hold the non-served tiers at a constant 0.1,
	// while a real prompt's other-tier scores differ (a coding
	// prompt still scores somewhat on reasoning/chat), so the
	// trained model predicts a middling-but-correct probability
	// for it (observed 0.609 on this tree) rather than the ~0.95
	// it gives an exactly on-distribution vector (see the
	// learned package's own test). What matters end to end is
	// that the model is active and favours the right tier; the
	// calibration gate — measured on held-out pairs, ECE <=
	// 0.15 — is the real quality bar, and it passed above.
	if p := dec.LearnedWinProb["coding"]; p <= 0.5 {
		t.Errorf("auto-route decision: learned win-prob for coding = %.3f, want > 0.5 (scores %v)", p, dec.Scores)
	}
	best := ""
	for tier, p := range dec.LearnedWinProb {
		if best == "" || p > dec.LearnedWinProb[best] {
			best = tier
		}
	}
	if best != "coding" {
		t.Errorf("auto-route decision: highest learned win-prob is %s, want coding (probs %v)", best, dec.LearnedWinProb)
	}
}

func TestLearnedEndpointFlagOffReportsReadyButInactive(t *testing.T) {
	router := autorouter.New(autorouter.Config{DefaultModel: "model-x"})
	store := openTestFeedbackStore(t)
	// Flag off (the default): the same 100 outcomes that
	// activate the model must NOT apply to routing.
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(router), WithFeedback(store))
	seedLearnedEvidence(t, store, 100)
	rec := doReq(t, srv, http.MethodGet, "/dashboard/learned", "", "")
	sum := decodeLearnedSummary(t, rec.Body.Bytes())
	if sum.Status != learned.StatusLearning {
		t.Errorf("flag off: status = %q, want learning (ready-but-off, never active)", sum.Status)
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto&prompt=Write+a+Python+function+to+sort+a+list", "", "")
	var dec autorouter.Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatalf("decode auto-route decision: %v", err)
	}
	if dec.LearnedActive || len(dec.LearnedWinProb) != 0 {
		t.Errorf("flag off: auto-route decision carries learned signal (active=%v probs=%v), want none", dec.LearnedActive, dec.LearnedWinProb)
	}
}
