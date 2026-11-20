// G17 Area 11 — Idempotency-Key billing safety at the LEDGER seam.
//
// The store-level test (TestIdempotencyClaimThenReplayNoSecondRow)
// proves a replay creates no second IDEMPOTENCY row. These tests
// prove the property the billing row cares about: two identical
// deliveries — one execution + one replay — leave exactly ONE
// usage row in the ledger, at the original cost.
//
// The tests drive the exact call-site sequence handleChatCompletions
// will run once the coordinator pastes the verbatim wiring lines
// from the G17 report (Begin → execute/record → Finish → Begin →
// replay): the engine step is the ledger's own RecordUsage, the
// same write Engine.recordAttempt performs, so the row counted
// here is a real usage row, not a store row.
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// idemLedgerRows counts usage rows in the test ledger.
func idemLedgerRows(t *testing.T, s *keyBudgetSetup) []ledger.UsageEvent {
	t.Helper()
	ev, err := s.led.RecentEvents(context.Background(), 10, ledger.LogAll)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	return ev
}

func TestIdempotencyTwoDeliveriesOneLedgerRow(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	ctx := context.Background()
	body := `{"model":"priced-model","messages":[{"role":"user","content":"hi"}]}`
	hash := idempotencyRequestHash(http.MethodPost, "/v1/chat/completions", []byte(body))
	respBody := []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`)

	// Delivery 1: fresh key claims, executes, records ONE usage
	// row ($0.025: 1000×$10/1M + 500×$30/1M), completes.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req1.Header.Set("Idempotency-Key", "key-1")
	b1, err := srv.idempotencyBegin(ctx, s.id, req1, hash)
	if err != nil || b1.State != IdemProceed {
		t.Fatalf("delivery 1 begin = %+v, err = %v; want IdemProceed", b1, err)
	}
	if _, err := s.led.RecordUsage(ctx, ledger.Usage{
		GatewayKeyID: s.id, Provider: "p", Model: "priced-model",
		PromptTokens: 1000, CompletionTokens: 500, SplitKnown: true, OK: true,
	}, analyticsPrices()); err != nil {
		t.Fatalf("delivery 1 record usage: %v", err)
	}
	srv.idempotencyFinish(ctx, b1, http.StatusOK, respBody, "application/json", 0.025, true)
	if rows := idemLedgerRows(t, s); len(rows) != 1 {
		t.Fatalf("ledger rows after delivery 1 = %d, want 1", len(rows))
	}

	// Delivery 2: same key, same body — the ORIGINAL response
	// replays and nothing executes, so no second usage row.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("Idempotency-Key", "key-1")
	b2, err := srv.idempotencyBegin(ctx, s.id, req2, hash)
	if err != nil || b2.State != IdemReplay {
		t.Fatalf("delivery 2 begin = %+v, err = %v; want IdemReplay", b2, err)
	}
	rec := httptest.NewRecorder()
	if !idempotencyReplay(rec, b2.Record) {
		t.Fatalf("replay refused a completed record")
	}
	if rec.Code != http.StatusOK || rec.Body.String() != string(respBody) {
		t.Fatalf("replayed response = %d %q, want 200 %q", rec.Code, rec.Body.String(), respBody)
	}
	if got := rec.Header().Get("X-Idempotency-Replayed"); got != "true" {
		t.Fatalf("X-Idempotency-Replayed = %q, want \"true\"", got)
	}
	if rows := idemLedgerRows(t, s); len(rows) != 1 {
		t.Fatalf("ledger rows after replay = %d, want 1 (no second billing row)", len(rows))
	}
	if rows := idemLedgerRows(t, s); rows[0].CostUSD != 0.025 {
		t.Fatalf("ledger row cost = %v, want 0.025 (billed once)", rows[0].CostUSD)
	}
}

func TestIdempotencyReleaseFreesFailedAttempt(t *testing.T) {
	st := idemTestStore(t)
	ctx := context.Background()
	hash := idemHash(`{"model":"m"}`)

	claim, err := st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("first claim = %+v, err = %v; want IdemProceed", claim, err)
	}
	// The attempt failed before a response was served: release
	// it instead of completing it.
	if err := st.Release(ctx, "gw-a", "key-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// A retry executes fresh — no 409, no stale in-flight row.
	claim, err = st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("retry claim = %+v, err = %v; want IdemProceed after release", claim, err)
	}
	if n := idemCount(t, st); n != 1 {
		t.Fatalf("rows after release + reclaim = %d, want 1", n)
	}
}

func TestIdempotencyReleaseNeverDropsCompletedRow(t *testing.T) {
	st := idemTestStore(t)
	ctx := context.Background()
	hash := idemHash(`{"model":"m"}`)

	claim, err := st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("claim = %+v, err = %v; want IdemProceed", claim, err)
	}
	if err := st.Complete(ctx, "gw-a", "key-1", http.StatusOK, []byte(`{}`), "application/json", 0.01, true); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Release after completion must not delete the replayable row.
	if err := st.Release(ctx, "gw-a", "key-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	claim, err = st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemReplay {
		t.Fatalf("claim after release = %+v, err = %v; want IdemReplay (completed row kept)", claim, err)
	}
}
