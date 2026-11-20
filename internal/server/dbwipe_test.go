package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/feedback"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// fakeHistoryClearer stands in for the health checker in wipe
// tests: it records calls and returns fixed counts. The real
// checker clearing (ring + file + .bak + restart) is proven in
// internal/health's TestCheckerClearHistory.
type fakeHistoryClearer struct {
	calls        int
	samples      int
	filesRemoved bool
	err          error
}

func (f *fakeHistoryClearer) ClearHistory() (int, bool, error) {
	f.calls++
	return f.samples, f.filesRemoved, f.err
}

// dbWipeServer is a server with a seeded ledger (2 usage events +
// a spent budget), a seeded feedback store (2 decisions + 2
// signals) and a fake probe-history clearer, plus the raw gateway
// key and the stores for post-wipe assertions.
type dbWipeServer struct {
	srv     *Server
	raw     string
	led     *ledger.Ledger
	fb      *feedback.Store
	clearer *fakeHistoryClearer
	flushes *int
}

func newDBWipeServer(t *testing.T) *dbWipeServer {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	key, raw, err := config.IssueKey("test", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	cfg.Providers = []provider.ProviderConfig{{
		ID: "openai", Name: "OpenAI", WireFormat: "openai",
		Models:  []provider.Model{{ID: "gpt-4o-mini"}},
		Enabled: true,
	}}
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	ctx := context.Background()
	if err := led.SetBudget(ctx, key.ID, 10, "monthly"); err != nil {
		t.Fatal(err)
	}
	for _, u := range []ledger.Usage{
		{GatewayKeyID: key.ID, Provider: "openai", Model: "gpt-4o-mini", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, SplitKnown: true, OK: true, LatencyMS: 120},
		{GatewayKeyID: key.ID, Provider: "openai", Model: "gpt-4o-mini", PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28, SplitKnown: true, OK: false, LatencyMS: 90},
	} {
		if _, err := led.RecordUsage(ctx, u, ledger.Prices{}); err != nil {
			t.Fatal(err)
		}
	}
	fb, err := feedback.Open(filepath.Join(t.TempDir(), "feedback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fb.Close() })
	for _, d := range []feedback.DecisionRef{
		{ID: "dec-1", Tier: "chat", Model: "gpt-4o-mini", LatencyMicros: 100},
		{ID: "dec-2", Tier: "coding", Model: "gpt-4o-mini", LatencyMicros: 200},
	} {
		if err := fb.RegisterDecision(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := fb.RecordSignal(ctx, "dec-1", feedback.SignalAccept); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fb.RecordSignal(ctx, "dec-2", feedback.SignalDiscard); err != nil {
		t.Fatal(err)
	}
	clearer := &fakeHistoryClearer{samples: 7, filesRemoved: true}
	flushes := 0
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg), WithLedger(led), WithFeedback(fb),
		WithHealthHistory(clearer), WithLedgerFlushHook(func() { flushes++ }))
	return &dbWipeServer{srv: srv, raw: raw, led: led, fb: fb, clearer: clearer, flushes: &flushes}
}

// armWipe calls POST /admin/db/wipe/arm and returns the token.
func armWipe(t *testing.T, srv *Server, key string) string {
	t.Helper()
	rec := doReqPeer(t, srv, "POST", "/admin/db/wipe/arm", `{"confirm": "WIPE DATABASE"}`, key, loopbackPeer)
	if rec.Code != 201 {
		t.Fatalf("arm wipe: status %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		ConfirmationToken string `json:"confirmation_token"`
		ExpiresInSeconds  int    `json:"expires_in_seconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("arm wipe response: %v", err)
	}
	if payload.ConfirmationToken == "" {
		t.Fatal("arm wipe returned an empty token")
	}
	if payload.ExpiresInSeconds != 300 {
		t.Errorf("arm wipe expires_in_seconds = %d, want 300", payload.ExpiresInSeconds)
	}
	return payload.ConfirmationToken
}

func wipeBody(token string) string {
	return `{"confirm": "WIPE DATABASE", "confirmation_token": "` + token + `"}`
}

// TestDBWipeCredentialBoundary proves the three-check boundary in
// order: no key is 401 (even relaxed), key without the phrase is
// 400, phrase without the second credential is 403, and a bogus
// token is 403. A token is single-use: the same token cannot wipe
// twice.
func TestDBWipeCredentialBoundary(t *testing.T) {
	w := newDBWipeServer(t)
	srv, raw := w.srv, w.raw

	// No key: 401 on both steps.
	rec := doReqPeer(t, srv, "POST", "/admin/db/wipe/arm", `{"confirm": "WIPE DATABASE"}`, "", loopbackPeer)
	if rec.Code != 401 {
		t.Errorf("arm without key: status %d, want 401", rec.Code)
	}
	rec = doReqPeer(t, srv, "POST", "/admin/db/wipe", wipeBody("anything"), "", loopbackPeer)
	if rec.Code != 401 {
		t.Errorf("wipe without key: status %d, want 401", rec.Code)
	}
	// Key but no/wrong phrase: 400 on both steps.
	rec = doReqPeer(t, srv, "POST", "/admin/db/wipe/arm", ``, raw, loopbackPeer)
	if rec.Code != 400 {
		t.Errorf("arm key without phrase: status %d, want 400", rec.Code)
	}
	rec = doReqPeer(t, srv, "POST", "/admin/db/wipe", `{"confirm": "WIPE", "confirmation_token": "x"}`, raw, loopbackPeer)
	if rec.Code != 400 {
		t.Errorf("wipe wrong phrase: status %d, want 400", rec.Code)
	}
	// Phrase but no second credential: 403.
	rec = doReqPeer(t, srv, "POST", "/admin/db/wipe", `{"confirm": "WIPE DATABASE"}`, raw, loopbackPeer)
	if rec.Code != 403 {
		t.Errorf("wipe phrase without token: status %d, want 403", rec.Code)
	}
	rec = doReqPeer(t, srv, "POST", "/admin/db/wipe", `{"confirm": "WIPE DATABASE", "confirmation_token": ""}`, raw, loopbackPeer)
	if rec.Code != 403 {
		t.Errorf("wipe empty token: status %d, want 403", rec.Code)
	}
	// Phrase + bogus token: 403 (indistinguishable from expired).
	rec = doReqPeer(t, srv, "POST", "/admin/db/wipe", wipeBody("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"), raw, loopbackPeer)
	if rec.Code != 403 {
		t.Errorf("wipe bogus token: status %d, want 403", rec.Code)
	}
	// A real token works once, then the same token is spent: 403.
	tok := armWipe(t, srv, raw)
	rec = doReqPeer(t, srv, "POST", "/admin/db/wipe", wipeBody(tok), raw, loopbackPeer)
	if rec.Code != 200 {
		t.Fatalf("wipe with real token: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	rec = doReqPeer(t, srv, "POST", "/admin/db/wipe", wipeBody(tok), raw, loopbackPeer)
	if rec.Code != 403 {
		t.Errorf("wipe replayed token: status %d, want 403 (single-use)", rec.Code)
	}
	// Two arms mint two different tokens; both are live until spent.
	tok1 := armWipe(t, srv, raw)
	tok2 := armWipe(t, srv, raw)
	if tok1 == tok2 {
		t.Error("two arms minted the same token")
	}
	rec = doReqPeer(t, srv, "POST", "/admin/db/wipe", wipeBody(tok1), raw, loopbackPeer)
	if rec.Code != 200 {
		t.Errorf("wipe with first of two tokens: status %d, want 200", rec.Code)
	}

	// Relaxed Tier 3 auth must not open either Tier 2 step.
	cfg := config.DefaultConfig(t.TempDir())
	key, _, err := config.IssueKey("test", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	relaxed := newTestServer(t, &fakeEngine{}, WithAuthRelaxed(true), WithConfig(cfg))
	rec = doReqPeer(t, relaxed, "POST", "/admin/db/wipe/arm", `{"confirm": "WIPE DATABASE"}`, "", loopbackPeer)
	if rec.Code != 401 {
		t.Errorf("relaxed arm without key: status %d, want 401", rec.Code)
	}
	rec = doReqPeer(t, relaxed, "POST", "/admin/db/wipe", wipeBody("x"), "", loopbackPeer)
	if rec.Code != 401 {
		t.Errorf("relaxed wipe without key: status %d, want 401", rec.Code)
	}

	// Tier 2 is auth-gated, not loopback-gated: a non-loopback peer
	// with a valid key, phrase and token still wipes.
	tok = armWipe(t, srv, raw)
	rec = doReqPeer(t, srv, "POST", "/admin/db/wipe", wipeBody(tok), raw, nonLoopbackPeer)
	if rec.Code != 200 {
		t.Errorf("wipe non-loopback + key + phrase + token: status %d, want 200", rec.Code)
	}
}

// TestDBWipeTokenExpiry proves an expired token is refused: the
// token is deleted on the failed attempt, so it cannot be retried
// into validity.
func TestDBWipeTokenExpiry(t *testing.T) {
	w := newDBWipeServer(t)
	tok := armWipe(t, w.srv, w.raw)
	sum := sha256.Sum256([]byte(tok))
	w.srv.wipeMu.Lock()
	w.srv.wipeTokens[sum] = time.Now().Add(-time.Minute)
	w.srv.wipeMu.Unlock()
	rec := doReqPeer(t, w.srv, "POST", "/admin/db/wipe", wipeBody(tok), w.raw, loopbackPeer)
	if rec.Code != 403 {
		t.Errorf("wipe expired token: status %d, want 403", rec.Code)
	}
	w.srv.wipeMu.Lock()
	_, stillThere := w.srv.wipeTokens[sum]
	w.srv.wipeMu.Unlock()
	if stillThere {
		t.Error("expired token was not consumed on the failed attempt")
	}
}

// TestDBWipeWipesDataPreservesConfig is the scope proof: ledger
// events, budget spend, feedback decisions/signals and probe
// history are gone, while the config (providers, provider keys,
// gateway keys, budget caps) is untouched — and the response says
// so, with no secret material in it.
func TestDBWipeWipesDataPreservesConfig(t *testing.T) {
	w := newDBWipeServer(t)
	ctx := context.Background()
	sum, err := w.led.Summary(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 2 {
		t.Fatalf("ledger before wipe: %d requests, want 2", sum.Requests)
	}
	fbSum, err := w.fb.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fbSum.Total != 2 {
		t.Fatalf("feedback before wipe: %d signals, want 2", fbSum.Total)
	}

	tok := armWipe(t, w.srv, w.raw)
	rec := doReqPeer(t, w.srv, "POST", "/admin/db/wipe", wipeBody(tok), w.raw, loopbackPeer)
	if rec.Code != 200 {
		t.Fatalf("wipe: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Wiped struct {
			UsageEvents               int64 `json:"usage_events"`
			BudgetsSpentReset         int64 `json:"budgets_spent_reset"`
			FeedbackDecisions         int64 `json:"feedback_decisions"`
			FeedbackSignals           int64 `json:"feedback_signals"`
			HealthHistorySamples      int64 `json:"health_history_samples"`
			HealthHistoryFilesRemoved bool  `json:"health_history_files_removed"`
		} `json:"wiped"`
		Irreversible bool     `json:"irreversible"`
		Preserved    []string `json:"preserved"`
		Note         string   `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("wipe response: %v", err)
	}
	if payload.Wiped.UsageEvents != 2 || payload.Wiped.BudgetsSpentReset != 1 ||
		payload.Wiped.FeedbackDecisions != 2 || payload.Wiped.FeedbackSignals != 2 ||
		payload.Wiped.HealthHistorySamples != 7 || !payload.Wiped.HealthHistoryFilesRemoved {
		t.Errorf("wiped counts = %+v, want 2 events / 1 budget / 2 decisions / 2 signals / 7 samples / files removed", payload.Wiped)
	}
	if !payload.Irreversible {
		t.Error("wipe response irreversible = false, want true")
	}
	if !strings.Contains(strings.Join(payload.Preserved, " "), "provider keys") {
		t.Errorf("wipe response preserved list = %v, want it to name provider keys", payload.Preserved)
	}
	if !strings.Contains(payload.Note, "Irreversible") {
		t.Errorf("wipe response note = %q, want it to say irreversible", payload.Note)
	}
	// No secret material anywhere in the response.
	for _, leaked := range []string{"base_url", "api_key", w.raw} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Errorf("wipe response contains %q", leaked)
		}
	}
	// The data is actually gone.
	sum, err = w.led.Summary(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 0 || sum.CostUSD != 0 || sum.TotalTokens != 0 {
		t.Errorf("ledger after wipe = %+v, want zero requests/cost/tokens", sum)
	}
	fbSum, err = w.fb.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fbSum.Total != 0 {
		t.Errorf("feedback after wipe Total = %d, want 0", fbSum.Total)
	}
	if w.clearer.calls != 1 {
		t.Errorf("probe-history clearer calls = %d, want 1", w.clearer.calls)
	}
	if *w.flushes != 1 {
		t.Errorf("ledger flush hook calls = %d, want 1 (flush runs before the delete)", *w.flushes)
	}
	// The config is untouched: still one provider, still one key,
	// and the budget cap still 10 with spend reset to zero.
	cfg := w.srv.cfg
	if len(cfg.Providers) != 1 || cfg.Providers[0].ID != "openai" {
		t.Errorf("config providers after wipe = %+v, want the one configured provider", cfg.Providers)
	}
	if len(cfg.GatewayKeys) != 1 {
		t.Errorf("config gateway keys after wipe = %d, want 1 (keys are preserved, never wiped)", len(cfg.GatewayKeys))
	}
}

// TestDBWipeFlushHookRunsFirst proves the ordering the wipe
// depends on: a row queued in the async writer and flushed by the
// hook lands BEFORE the delete, so it is wiped too instead of
// resurrecting history afterwards.
func TestDBWipeFlushHookRunsFirst(t *testing.T) {
	w := newDBWipeServer(t)
	ctx := context.Background()
	// Replace the counting hook with one that records a row, the
	// way a real AsyncWriter.Flush would deliver queued rows.
	w.srv.ledgerFlushHook = func() {
		_, _ = w.led.RecordUsage(ctx, ledger.Usage{
			GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o-mini",
			PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2,
			SplitKnown: true, OK: true, LatencyMS: 1,
		}, ledger.Prices{})
	}
	tok := armWipe(t, w.srv, w.raw)
	rec := doReqPeer(t, w.srv, "POST", "/admin/db/wipe", wipeBody(tok), w.raw, loopbackPeer)
	if rec.Code != 200 {
		t.Fatalf("wipe: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Wiped struct {
			UsageEvents int64 `json:"usage_events"`
		} `json:"wiped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Wiped.UsageEvents != 3 {
		t.Errorf("wiped usage_events = %d, want 3 (2 seeded + 1 flushed by the hook)", payload.Wiped.UsageEvents)
	}
	sum, err := w.led.Summary(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 0 {
		t.Errorf("ledger after wipe = %d requests, want 0 (flushed row must not survive)", sum.Requests)
	}
}

// TestDBWipeUnwiredStores proves a server with no stores wired
// still answers honestly: 200 with zero counts and a note per
// store, never a pretend wipe and never a 500.
func TestDBWipeUnwiredStores(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	tok := armWipe(t, srv, "test-key-123")
	rec := doReqPeer(t, srv, "POST", "/admin/db/wipe", wipeBody(tok), "test-key-123", loopbackPeer)
	if rec.Code != 200 {
		t.Fatalf("wipe with no stores: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Wiped struct {
			UsageEvents int64 `json:"usage_events"`
		} `json:"wiped"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Wiped.UsageEvents != 0 {
		t.Errorf("wiped usage_events = %d, want 0", payload.Wiped.UsageEvents)
	}
	if len(payload.Notes) != 3 {
		t.Errorf("wipe notes = %v, want one per unwired store (ledger, feedback, history)", payload.Notes)
	}
}
