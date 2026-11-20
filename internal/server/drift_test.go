package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// Token-count drift feed (CHECKLIST 229-230): GET /dashboard/usage
// carries the live estimate-vs-reported drift per (provider, model)
// in summary.drift, computed from ledger rows that have BOTH a
// provider-reported prompt count and a local estimate. Rows without
// an estimate are excluded, never treated as agreement — and an
// empty ledger answers an empty list, never invented drift.
func TestDashboardUsageDrift(t *testing.T) {
	ctx := context.Background()
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	seed := []ledger.Usage{
		// openai/model-a: local 90 vs reported 100 (-10%), local
		// 110 vs reported 100 (+10%): mean 0, mean|.| 10, max 10.
		{Provider: "openai", Model: "model-a", PromptTokens: 100, CompletionTokens: 5, SplitKnown: true, OK: true, LocalEstimatePromptTokens: 90},
		{Provider: "openai", Model: "model-a", PromptTokens: 100, CompletionTokens: 5, SplitKnown: true, OK: true, LocalEstimatePromptTokens: 110},
		// No estimate measured: excluded from drift.
		{Provider: "openai", Model: "model-a", PromptTokens: 100, CompletionTokens: 5, SplitKnown: true, OK: true},
		// Split unknown: excluded from drift.
		{Provider: "anthropic", Model: "model-b", TotalTokens: 40, OK: false, LocalEstimatePromptTokens: 35},
	}
	for _, u := range seed {
		if _, err := led.RecordUsage(ctx, u, nil); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}
	}
	srv := newTestServer(t, &fakeEngine{}, WithLedger(led))
	var resp usageResponse
	getJSON(t, srv, "/dashboard/usage?range=24h", &resp)
	if len(resp.Summary.Drift) != 1 {
		t.Fatalf("usage drift rows = %d, want 1: %+v", len(resp.Summary.Drift), resp.Summary.Drift)
	}
	d := resp.Summary.Drift[0]
	if d.Provider != "openai" || d.Model != "model-a" {
		t.Errorf("drift row = %s/%s, want openai/model-a", d.Provider, d.Model)
	}
	if d.Compared != 2 {
		t.Errorf("drift compared = %d, want 2", d.Compared)
	}
	if d.MeanErrPct != 0 || d.MeanAbsErrPct != 10 || d.MaxAbsErrPct != 10 {
		t.Errorf("drift errors = mean %v | mean|.| %v | max %v, want 0 | 10 | 10", d.MeanErrPct, d.MeanAbsErrPct, d.MaxAbsErrPct)
	}
}

func TestDashboardUsageDriftEmpty(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	srv := newTestServer(t, &fakeEngine{}, WithLedger(led))
	var resp usageResponse
	getJSON(t, srv, "/dashboard/usage?range=24h", &resp)
	if resp.Summary.Drift == nil {
		t.Errorf("usage drift = nil, want an empty list (the dashboard renders an honest empty state from [])")
	}
	if len(resp.Summary.Drift) != 0 {
		t.Errorf("usage drift = %d rows on an empty ledger, want 0", len(resp.Summary.Drift))
	}
}
