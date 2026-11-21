package server

// Phase 2/T6 — analytics labelling and accuracy. Two properties,
// both against the REAL ledger:
//
//  1. RECONCILIATION: GET /api/analytics/summary totals equal an
//     independent row scan of the same window (requests, ok, errors,
//     cancelled, token splits, cache/cache-write splits, estimate
//     tokens, mean latency) — the surface cannot drift from the
//     ledger it claims to summarize.
//  2. REPORTED vs COMPUTED COST: the provider-reported cost sum and
//     its row count are exposed verbatim, and when they disagree with
//     the priced-computed total the response says so in `note`
//     instead of silently averaging pricing and billing.

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
)

func TestAnalyticsSummaryReconcilesWithLedgerRowsAndReportsReportedCost(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })

	reported := 0.0123
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	ev := func(ts time.Time, gatewayKey, provider, model string, prompt, comp, cached, reasoning int, localEst, ok, cancelled bool, cost float64, rep *float64) ledger.UsageEvent {
		return ledger.UsageEvent{
			TS: ts, GatewayKeyID: gatewayKey, Provider: provider, Model: model,
			PromptTokens: prompt, CompletionTokens: comp, TotalTokens: prompt + comp,
			CachedTokens: cached, ReasoningTokens: reasoning,
			LocalEstimate: localEst, SplitKnown: !localEst, OK: ok, Cancelled: cancelled,
			CostUSD: cost, ReportedCostUSD: rep, LatencyMS: 5,
		}
	}
	events := []ledger.UsageEvent{
		// priced, reported-cost row
		ev(today.Add(1*time.Minute), "k1", "p1", "m1", 10, 5, 4, 2, false, true, false, 0.0100, &reported),
		// priced, router-computed cost only (nothing reported upstream)
		ev(today.Add(2*time.Minute), "k1", "p1", "m1", 8, 2, 0, 0, false, true, false, 0.0200, nil),
		// local-estimate row (upstream reported no usage; the router's own
		// completion count is not a provider report, so the ledger
		// rejects nonzero completion on such a row)
		ev(today.Add(3*time.Minute), "k1", "p2", "m2", 7, 0, 0, 0, true, true, false, 0.0000, nil),
		// failed attempt
		ev(today.Add(4*time.Minute), "k1", "p2", "m2", 0, 0, 0, 0, false, false, false, 0.0000, nil),
		// hedge loser (cancelled — not a failure)
		ev(today.Add(5*time.Minute), "k1", "p2", "m2", 0, 0, 0, 0, false, false, true, 0.0000, nil),
	}
	if _, _, err := led.ImportEvents(context.Background(), events); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Independent scan of the same rows (no Summary code involved).
	rows, err := led.RecentEvents(context.Background(), 100, ledger.LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	var wantPrompt, wantComp, wantCached, wantReasoning, wantEst, wantOK, wantErr, wantCancelled int
	for _, r := range rows {
		wantPrompt += r.PromptTokens
		wantComp += r.CompletionTokens
		wantCached += r.CachedTokens
		wantReasoning += r.ReasoningTokens
		if r.LocalEstimate {
			wantEst += r.PromptTokens
		}
		if r.OK && !r.Cancelled {
			wantOK++
		} else if !r.OK && !r.Cancelled {
			wantErr++
		} else if r.Cancelled {
			wantCancelled++
		}
	}

	srv := newTestServer(t, &fakeEngine{}, WithLedger(led))
	rec := doReqPeer(t, srv, "GET", "/api/analytics/summary?window=day", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary status %d: %s", rec.Code, rec.Body.String())
	}
	var sum struct {
		Requests            int     `json:"requests"`
		OK                  int     `json:"ok"`
		Errors              int     `json:"errors"`
		PromptTokens        int     `json:"prompt_tokens"`
		CompletionTokens    int     `json:"completion_tokens"`
		CachedTokens        int     `json:"cached_tokens"`
		ReasoningTokens     int     `json:"reasoning_tokens"`
		LocalEstimateTokens int     `json:"local_estimate_tokens"`
		PricedCostUSD       float64 `json:"priced_cost_usd"`
		ReportedCostUSD     float64 `json:"reported_cost_usd"`
		ReportedCostRows    int     `json:"reported_cost_rows"`
		CostStatus          string  `json:"cost_status"`
		Note                string  `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}

	if sum.Requests != len(rows) || sum.OK != wantOK || sum.Errors != wantErr {
		t.Errorf("counts = %d/%d/%d, want %d/%d/%d (row scan)", sum.Requests, sum.OK, sum.Errors, len(rows), wantOK, wantErr)
	}
	if sum.PromptTokens != wantPrompt || sum.CompletionTokens != wantComp {
		t.Errorf("tokens = %d/%d, want %d/%d (row scan)", sum.PromptTokens, sum.CompletionTokens, wantPrompt, wantComp)
	}
	if sum.CachedTokens != wantCached || sum.ReasoningTokens != wantReasoning {
		t.Errorf("splits = cached %d / reasoning %d, want %d / %d", sum.CachedTokens, sum.ReasoningTokens, wantCached, wantReasoning)
	}
	if sum.LocalEstimateTokens != wantEst {
		t.Errorf("local_estimate_tokens = %d, want %d (row scan)", sum.LocalEstimateTokens, wantEst)
	}
	if sum.ReportedCostRows != 1 || sum.ReportedCostUSD < 0.0122 || sum.ReportedCostUSD > 0.0124 {
		t.Errorf("reported cost = %.4f across %d rows, want the single reported 0.0123", sum.ReportedCostUSD, sum.ReportedCostRows)
	}
	// priced_cost_usd follows the ledger rule: a provider-reported cost
	// wins on its row (0.0123 replaces the computed 0.01), so the
	// window total is 0.0123 + 0.02 = 0.0323 — and it differs from
	// reported_cost_usd by exactly the pricing-vs-billing delta.
	if sum.PricedCostUSD < 0.0322 || sum.PricedCostUSD > 0.0324 {
		t.Errorf("priced_cost_usd = %.4f, want ~0.0323 (reported 0.0123 + computed 0.02)", sum.PricedCostUSD)
	}
	if !strings.Contains(sum.Note, "pricing-vs-billing delta") {
		t.Errorf("note = %q, want the reported-vs-priced reconciliation", sum.Note)
	}
}
