package ledger

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func recordTestUsage(t *testing.T, l *Ledger, u Usage, prices Prices) {
	t.Helper()
	if _, err := l.RecordUsage(context.Background(), u, prices); err != nil {
		t.Fatalf("RecordUsage %+v: %v", u, err)
	}
}

// TestLedgerFeeds proves the dashboard live feeds read real ledger
// rows: recent events (newest first, with filters), range summaries
// with previous-window bounds, and time buckets that always cover
// the whole window (empty buckets included).
func TestLedgerFeeds(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}

	recordTestUsage(t, l, Usage{Provider: "openai", Model: "model-a", PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, SplitKnown: true, OK: true, LatencyMS: 120}, prices)
	recordTestUsage(t, l, Usage{Provider: "openai", Model: "model-a", PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300, SplitKnown: true, OK: true, LatencyMS: 200}, prices)
	recordTestUsage(t, l, Usage{Provider: "anthropic", Model: "model-b", TotalTokens: 40, SplitKnown: false, OK: false, LatencyMS: 5000}, prices)
	recordTestUsage(t, l, Usage{Provider: "openai", Model: "model-a", OK: true, LatencyMS: 80, Shared: true}, prices)

	// --- RecentEvents: newest first, filters honest ---
	all, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents all: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("RecentEvents all = %d rows, want 4", len(all))
	}
	if !all[0].Shared || all[0].TotalTokens != 0 || all[0].CostUSD != 0 {
		t.Errorf("newest row = %+v, want the shared waiter (zero tokens, zero cost)", all[0])
	}
	if all[3].Provider != "openai" || all[3].PromptTokens != 100 {
		t.Errorf("oldest row = %+v, want the first recorded usage", all[3])
	}
	ok, err := l.RecentEvents(ctx, 10, LogOK)
	if err != nil {
		t.Fatalf("RecentEvents ok: %v", err)
	}
	if len(ok) != 3 {
		t.Errorf("RecentEvents ok = %d rows, want 3", len(ok))
	}
	failed, err := l.RecentEvents(ctx, 10, LogFailed)
	if err != nil {
		t.Fatalf("RecentEvents failed: %v", err)
	}
	if len(failed) != 1 || failed[0].Provider != "anthropic" {
		t.Errorf("RecentEvents failed = %+v, want the one anthropic failure", failed)
	}
	limited, err := l.RecentEvents(ctx, 2, LogAll)
	if err != nil || len(limited) != 2 {
		t.Errorf("RecentEvents limit 2 = %d rows, err %v, want 2", len(limited), err)
	}

	// --- Summary: totals, split handling, per-provider/model ---
	since := time.Now().Add(-time.Hour)
	sum, err := l.Summary(ctx, since, time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Requests != 4 || sum.OK != 3 || sum.Errors != 1 {
		t.Errorf("summary counts = %d/%d/%d, want 4/3/1", sum.Requests, sum.OK, sum.Errors)
	}
	if sum.PromptTokens != 300 || sum.CompletionTokens != 150 {
		t.Errorf("summary tokens = %d/%d, want 300/150", sum.PromptTokens, sum.CompletionTokens)
	}
	if sum.TotalTokens != 490 {
		t.Errorf("summary total tokens = %d, want 490", sum.TotalTokens)
	}
	// The split-unknown row contributes its 40 tokens only to the
	// unknown-split bucket, and its cost is 0.
	if sum.UnknownSplitTokens != 40 {
		t.Errorf("summary unknown-split tokens = %d, want 40", sum.UnknownSplitTokens)
	}
	// Cost: (100*0.001 + 50*0.002) + (200*0.001 + 100*0.002) = 0.2 + 0.4.
	// (Float comparison with a tolerance: 0.1 + 0.2 style sums are
	// not exact in binary floating point.)
	if math.Abs(sum.CostUSD-0.6) > 1e-9 {
		t.Errorf("summary cost = %v, want 0.6", sum.CostUSD)
	}
	if len(sum.ByProvider) != 2 {
		t.Fatalf("summary by_provider = %d rows, want 2", len(sum.ByProvider))
	}
	byProv := map[string]ProviderUsage{}
	for _, p := range sum.ByProvider {
		byProv[p.Provider] = p
	}
	if byProv["openai"].Requests != 3 || math.Abs(byProv["openai"].CostUSD-0.6) > 1e-9 {
		t.Errorf("openai summary = %+v, want 3 requests, $0.60", byProv["openai"])
	}
	if byProv["anthropic"].Requests != 1 || byProv["anthropic"].Errors != 1 {
		t.Errorf("anthropic summary = %+v, want 1 request, 1 error", byProv["anthropic"])
	}
	if len(sum.ByModel) != 2 {
		t.Fatalf("summary by_model = %d rows, want 2", len(sum.ByModel))
	}

	// --- Summary windows: [since, until) really bounds both ends ---
	// (The empty window starts a second in the future, so a row
	// recorded in the same millisecond as the boundary cannot
	// sneak in — ts has millisecond resolution.)
	empty, err := l.Summary(ctx, time.Now().Add(time.Second), time.Time{})
	if err != nil {
		t.Fatalf("Summary empty window: %v", err)
	}
	if empty.Requests != 0 {
		t.Errorf("summary of an empty window = %d requests, want 0", empty.Requests)
	}
	before, err := l.Summary(ctx, time.Now().Add(-2*time.Hour), since)
	if err != nil {
		t.Fatalf("Summary previous window: %v", err)
	}
	if before.Requests != 0 {
		t.Errorf("summary of the window before all rows = %d, want 0", before.Requests)
	}

	// --- Buckets: whole window covered, requests in the last bucket ---
	// A 2h window with 1h buckets is exactly 2 buckets: [now-2h,
	// now-1h) and [now-1h, now). The rows were all recorded just
	// before `now`, so they land in the last bucket.
	buckets, err := l.Buckets(ctx, time.Now().Add(-2*time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("Buckets = %d buckets, want 2 (2h window, 1h width)", len(buckets))
	}
	if buckets[0].Requests != 0 {
		t.Errorf("first bucket = %d requests, want 0", buckets[0].Requests)
	}
	last := buckets[1]
	if last.Requests != 4 || last.Errors != 1 || last.Tokens != 490 {
		t.Errorf("last bucket = %+v, want 4 requests, 1 error, 490 tokens", last)
	}
	if _, err := l.Buckets(ctx, time.Now(), 0); err == nil {
		t.Errorf("Buckets with zero width: got no error, want one")
	}
}

// TestBucketsEdgeClampMerges is the regression test for the
// TestDashboardLiveFeeds flake (~25% of runs): rows recorded in the
// same millisecond as Buckets' internal `now` get bucket index b == n
// and clamp onto the last bucket. The old code ASSIGNED the clamped
// group over the last bucket, discarding the rows the last bucket
// already held — so the first seeded row vanished from the chart
// whenever the seed and the Buckets call landed in the same
// millisecond. The fix accumulates groups instead.
//
// The timestamps are fixed relative to the window anchor (not to the
// wall clock), so the collision is deterministic: row A is native to
// the last bucket (b == 23) and rows B/C sit exactly on the window
// end (b == 24, clamped to 23). All three must be counted.
func TestBucketsEdgeClampMerges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	since := time.Now().Add(-24 * time.Hour)
	insert := func(ts time.Time, provider, model string, prompt, completion, total int, splitKnown int, cost float64, ok, latency int) {
		t.Helper()
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer raw.Close()
		if _, err := raw.ExecContext(ctx, `INSERT INTO usage_events
			(ts, gateway_key_id, provider, model, prompt_tokens, completion_tokens,
			 total_tokens, tokens_split_known, cost_usd, ok, latency_ms, shared)
			VALUES (?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
			ts.UnixMilli(), provider, model, prompt, completion, total, splitKnown, cost, ok, latency); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// Native to the last bucket: b == 23.
	insert(since.Add(23*time.Hour+30*time.Minute), "openai", "model-a", 100, 50, 150, 1, 0.2, 1, 120)
	// Exactly on the window end: b == 24, clamped onto bucket 23.
	insert(since.Add(24*time.Hour), "openai", "model-a", 200, 100, 300, 1, 0.4, 1, 200)
	insert(since.Add(24*time.Hour), "anthropic", "model-b", 0, 0, 40, 0, 0, 0, 5000)

	buckets, err := l.Buckets(ctx, since, time.Hour)
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	if len(buckets) != 24 {
		t.Fatalf("Buckets = %d buckets, want 24", len(buckets))
	}
	last := buckets[23]
	if last.Requests != 3 || last.Errors != 1 || last.Tokens != 490 {
		t.Errorf("last bucket = %+v, want 3 requests, 1 error, 490 tokens (native + clamped groups merged)", last)
	}
	if math.Abs(last.CostUSD-0.6) > 1e-9 {
		t.Errorf("last bucket cost = %v, want 0.6", last.CostUSD)
	}
	wantMean := (120.0 + 200.0 + 5000.0) / 3.0
	if math.Abs(last.MeanLatencyMS-wantMean) > 1e-9 {
		t.Errorf("last bucket mean latency = %v, want %v (request-weighted across merged groups)", last.MeanLatencyMS, wantMean)
	}
	for i, b := range buckets[:23] {
		if b.Requests != 0 {
			t.Errorf("bucket %d = %d requests, want 0", i, b.Requests)
		}
	}
}
