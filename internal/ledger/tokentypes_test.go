package ledger

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// Token-type split storage (CHECKLIST 145-146): input / output /
// cached / reasoning / cache-write tokens are stored separately
// wherever the upstream reports the split; unknown stays unknown,
// never zero-filled.
func TestTokenTypeSplitStorage(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()

	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "openai", Model: "gpt-4o",
		PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200,
		CachedTokens: 800, ReasoningTokens: 150, CacheWriteTokens: 0,
		SplitKnown: true, OK: true, LatencyMS: 100,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "anthropic", Model: "claude-x",
		PromptTokens: 500, CompletionTokens: 100, TotalTokens: 600,
		CachedTokens: 400, ReasoningTokens: 0, CacheWriteTokens: 500,
		SplitKnown: true, OK: true, LatencyMS: 200,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	events, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("RecentEvents = %d rows, want 2", len(events))
	}
	// Newest first: anthropic row first.
	anth := events[0]
	if anth.Provider != "anthropic" || anth.CachedTokens != 400 || anth.CacheWriteTokens != 500 || anth.ReasoningTokens != 0 {
		t.Errorf("anthropic row = %+v, want cached=400 cache_write=500 reasoning=0", anth)
	}
	if anth.LocalEstimate {
		t.Errorf("anthropic row LocalEstimate = true, want false (provider-reported split)")
	}
	oai := events[1]
	if oai.CachedTokens != 800 || oai.ReasoningTokens != 150 || oai.CacheWriteTokens != 0 {
		t.Errorf("openai row = %+v, want cached=800 reasoning=150 cache_write=0", oai)
	}

	sum, err := led.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.PromptTokens != 1500 || sum.CompletionTokens != 300 || sum.CachedTokens != 1200 {
		t.Errorf("summary prompt/completion/cached = %d/%d/%d, want 1500/300/1200",
			sum.PromptTokens, sum.CompletionTokens, sum.CachedTokens)
	}
	if sum.ReasoningTokens != 150 || sum.CacheWriteTokens != 500 {
		t.Errorf("summary reasoning/cache_write = %d/%d, want 150/500",
			sum.ReasoningTokens, sum.CacheWriteTokens)
	}
	if sum.LocalEstimateTokens != 0 || sum.RequestsWithLocalEstimate != 0 {
		t.Errorf("summary local estimate = %d tokens / %d requests, want 0/0",
			sum.LocalEstimateTokens, sum.RequestsWithLocalEstimate)
	}
	if len(sum.ByProvider) != 2 {
		t.Fatalf("ByProvider = %d groups, want 2", len(sum.ByProvider))
	}
	for _, p := range sum.ByProvider {
		switch p.Provider {
		case "openai":
			if p.ReasoningTokens != 150 || p.CacheWriteTokens != 0 {
				t.Errorf("openai provider reasoning/cache_write = %d/%d, want 150/0", p.ReasoningTokens, p.CacheWriteTokens)
			}
		case "anthropic":
			if p.ReasoningTokens != 0 || p.CacheWriteTokens != 500 {
				t.Errorf("anthropic provider reasoning/cache_write = %d/%d, want 0/500", p.ReasoningTokens, p.CacheWriteTokens)
			}
		}
	}
	for _, m := range sum.ByModel {
		if m.Model == "gpt-4o" && m.ReasoningTokens != 150 {
			t.Errorf("gpt-4o model reasoning = %d, want 150", m.ReasoningTokens)
		}
		if m.Model == "claude-x" && m.CacheWriteTokens != 500 {
			t.Errorf("claude-x model cache_write = %d, want 500", m.CacheWriteTokens)
		}
	}
}

// Subset counts are clamped to their parent count, never stored
// larger: a malformed upstream report can never claim more cached
// than prompt, more reasoning than completion, or more cache
// writes than prompt tokens. Negatives clamp to 0.
func TestTokenTypeSplitClamping(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "x", Model: "m",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		CachedTokens: 999, ReasoningTokens: 999, CacheWriteTokens: 999,
		SplitKnown: true, OK: true,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "x", Model: "m",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		CachedTokens: -3, ReasoningTokens: -3, CacheWriteTokens: -3,
		SplitKnown: true, OK: true,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	events, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("rows = %d, want 2", len(events))
	}
	// Newest first: the negative row.
	if events[0].CachedTokens != 0 || events[0].ReasoningTokens != 0 || events[0].CacheWriteTokens != 0 {
		t.Errorf("negative row stored %+v, want all subset counts 0", events[0])
	}
	if events[1].CachedTokens != 10 || events[1].ReasoningTokens != 5 || events[1].CacheWriteTokens != 10 {
		t.Errorf("oversized row stored %+v, want cached=10 reasoning=5 cache_write=10", events[1])
	}
}

// Local-estimate fallback (CHECKLIST 145): when the upstream
// reports no usage, the local prompt count is stored with
// split unknown, local_estimate=1 and cost 0 — labelled, never
// presented as a reported split. A known split always supersedes
// an estimate.
func TestLocalEstimateStorage(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	prices := Prices{{Model: "m"}: {PromptPerToken: 0.01, CompletionPerToken: 0.02}}

	// Estimate row: no split, prompt estimate 42.
	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "x", Model: "m",
		PromptTokens: 42, LocalEstimate: true, OK: true, LatencyMS: 50,
	}, prices); err != nil {
		t.Fatalf("RecordUsage estimate: %v", err)
	}
	// Known split with the estimate flag set: the split wins and
	// the flag is cleared (the provider's numbers supersede).
	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "x", Model: "m",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		LocalEstimate: true, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("RecordUsage split: %v", err)
	}

	events, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("rows = %d, want 2", len(events))
	}
	split := events[0] // newest: known split
	if split.LocalEstimate || !split.SplitKnown || split.PromptTokens != 10 || split.CostUSD == 0 {
		t.Errorf("split row = %+v, want LocalEstimate=false SplitKnown=true prompt=10 cost>0", split)
	}
	est := events[1]
	if !est.LocalEstimate || est.SplitKnown {
		t.Errorf("estimate row = %+v, want LocalEstimate=true SplitKnown=false", est)
	}
	if est.PromptTokens != 42 || est.TotalTokens != 42 || est.CompletionTokens != 0 {
		t.Errorf("estimate row tokens = %d/%d/%d, want prompt=42 total=42 completion=0",
			est.PromptTokens, est.TotalTokens, est.CompletionTokens)
	}
	if est.CostUSD != 0 {
		t.Errorf("estimate row cost = %v, want 0 (an estimate is never priced)", est.CostUSD)
	}

	sum, err := led.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.LocalEstimateTokens != 42 || sum.RequestsWithLocalEstimate != 1 {
		t.Errorf("summary local estimate = %d tokens / %d requests, want 42/1",
			sum.LocalEstimateTokens, sum.RequestsWithLocalEstimate)
	}
	if sum.PromptTokens != 52 {
		t.Errorf("summary prompt = %d, want 52 (10 reported + 42 estimated)", sum.PromptTokens)
	}
}

// The async batch writer carries the new columns exactly like the
// synchronous path (the cached_tokens regression, item 131, was
// an async-only drop — this pins all three new columns plus the
// estimate flag the same way).
func TestAsyncWriterCarriesTokenTypes(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	w := NewAsyncWriter(led, nil, AsyncWriterConfig{})
	defer w.Close()
	w.Record(Usage{
		Provider: "openai", Model: "gpt-4o",
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
		CachedTokens: 80, ReasoningTokens: 30, CacheWriteTokens: 0,
		SplitKnown: true, OK: true, LatencyMS: 10,
	})
	w.Record(Usage{
		Provider: "x", Model: "m",
		PromptTokens: 42, LocalEstimate: true, OK: true, LatencyMS: 10,
	})
	w.Flush()
	events, err := led.RecentEvents(context.Background(), 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("rows = %d, want 2", len(events))
	}
	est := events[0]
	if !est.LocalEstimate || est.PromptTokens != 42 {
		t.Errorf("async estimate row = %+v, want LocalEstimate=true prompt=42", est)
	}
	split := events[1]
	if split.CachedTokens != 80 || split.ReasoningTokens != 30 || split.CacheWriteTokens != 0 {
		t.Errorf("async split row = %+v, want cached=80 reasoning=30 cache_write=0", split)
	}
}

// Export -> import round-trips the full token-type split, and a
// re-import inserts zero rows (dedup covers the new columns).
func TestImportRoundTripsTokenTypes(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	events := []UsageEvent{
		{TS: now, GatewayKeyID: "k", Provider: "openai", Model: "gpt-4o",
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
			CachedTokens: 80, ReasoningTokens: 30, SplitKnown: true, OK: true, LatencyMS: 10},
		{TS: now, GatewayKeyID: "k", Provider: "anthropic", Model: "claude-x",
			PromptTokens: 500, CompletionTokens: 100, TotalTokens: 600,
			CachedTokens: 400, CacheWriteTokens: 500, SplitKnown: true, OK: true, LatencyMS: 20},
		{TS: now, GatewayKeyID: "k", Provider: "x", Model: "m",
			PromptTokens: 42, TotalTokens: 42, LocalEstimate: true, OK: true, LatencyMS: 5},
	}
	imported, dups, err := led.ImportEvents(ctx, events)
	if err != nil {
		t.Fatalf("ImportEvents: %v", err)
	}
	if imported != 3 || dups != 0 {
		t.Fatalf("imported=%d dups=%d, want 3/0", imported, dups)
	}
	imported, dups, err = led.ImportEvents(ctx, events)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if imported != 0 || dups != 3 {
		t.Fatalf("re-import imported=%d dups=%d, want 0/3", imported, dups)
	}
	got, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3", len(got))
	}
	byModel := map[string]UsageEvent{}
	for _, e := range got {
		byModel[e.Model] = e
	}
	if byModel["gpt-4o"].ReasoningTokens != 30 || byModel["gpt-4o"].CachedTokens != 80 {
		t.Errorf("gpt-4o after import = %+v, want reasoning=30 cached=80", byModel["gpt-4o"])
	}
	if byModel["claude-x"].CacheWriteTokens != 500 {
		t.Errorf("claude-x after import = %+v, want cache_write=500", byModel["claude-x"])
	}
	if !byModel["m"].LocalEstimate || byModel["m"].PromptTokens != 42 {
		t.Errorf("estimate row after import = %+v, want LocalEstimate=true prompt=42", byModel["m"])
	}
}

// Import validation rejects split contradictions in the new
// fields, and estimate rows that smuggle in a cost or a
// provider-reported subset count.
func TestImportValidatesTokenTypes(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	bad := []UsageEvent{
		{TS: now, Provider: "x", Model: "m", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, ReasoningTokens: 6, SplitKnown: true, OK: true},
		{TS: now, Provider: "x", Model: "m", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CachedTokens: 11, SplitKnown: true, OK: true},
		{TS: now, Provider: "x", Model: "m", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CacheWriteTokens: 11, SplitKnown: true, OK: true},
		{TS: now, Provider: "x", Model: "m", PromptTokens: 42, TotalTokens: 42, LocalEstimate: true, CostUSD: 0.01, OK: true},
		{TS: now, Provider: "x", Model: "m", PromptTokens: 42, TotalTokens: 42, LocalEstimate: true, CachedTokens: 5, OK: true},
		{TS: now, Provider: "x", Model: "m", PromptTokens: 42, CompletionTokens: 3, TotalTokens: 45, LocalEstimate: true, OK: true},
	}
	for i, e := range bad {
		if _, _, err := led.ImportEvents(ctx, []UsageEvent{e}); err == nil {
			t.Errorf("event %d (%+v): import accepted, want rejection", i, e)
		}
	}
	got, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("rows after rejected imports = %d, want 0 (never partial)", len(got))
	}
}

// The rolling 5-hour key budget (item 139, the one the config
// file's key_budgets block populates at startup) constrains the
// pre-send estimate exactly like the legacy cap: a config-driven
// cap that only lives in the rolling store used to be invisible
// to CheckBudgetForEstimate, so a prompt that alone exceeded it
// was never refused pre-send. Either store can refuse; the
// tightest remaining wins.
func TestCheckBudgetForEstimateRollingKeyBudget(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	led.FailClosed = true
	if err := led.SetKeyBudget(ctx, "k1", 0.0001, "fail_closed"); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}
	// No spend yet, no legacy row: a $0.0005 estimate alone
	// exceeds the $0.0001 rolling cap.
	if _, _, err := led.CheckBudgetForEstimate(ctx, "k1", 0.0005); err == nil {
		t.Errorf("estimate $0.0005 vs rolling cap $0.0001: allowed, want refusal")
	} else if !IsBudgetExceeded(err) {
		t.Errorf("refusal error = %T %v, want *BudgetExceededError", err, err)
	} else if be := err.(*BudgetExceededError); be.CapUSD != 0.0001 || be.SpentUSD != 0 {
		t.Errorf("refusal = %+v, want cap=0.0001 spent=0 (rolling numbers)", be)
	}
	// A $0.00005 estimate fits.
	if rem, _, err := led.CheckBudgetForEstimate(ctx, "k1", 0.00005); err != nil {
		t.Errorf("estimate $0.00005 under rolling cap $0.0001: %v, want allow", err)
	} else if rem != 0.0001 {
		t.Errorf("remaining = %v, want 0.0001 (rolling cap minus zero spend)", rem)
	}
	// Spend $0.07 of a $0.10 rolling cap, then a $0.05 estimate
	// would pass it.
	if err := led.SetKeyBudget(ctx, "k2", 0.10, "fail_closed"); err != nil {
		t.Fatalf("SetKeyBudget k2: %v", err)
	}
	if _, err := led.RecordUsage(ctx, Usage{GatewayKeyID: "k2", Provider: "x", Model: "m",
		PromptTokens: 7, CompletionTokens: 0, TotalTokens: 7, SplitKnown: true, OK: true},
		Prices{{Model: "m"}: {PromptPerToken: 0.01}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, err := led.CheckBudgetForEstimate(ctx, "k2", 0.05); err == nil {
		t.Errorf("spent $0.07 + estimate $0.05 past rolling cap $0.10: allowed, want refusal")
	}
	if _, _, err := led.CheckBudgetForEstimate(ctx, "k2", 0.02); err != nil {
		t.Errorf("spent $0.07 + estimate $0.02 under rolling cap $0.10: %v, want allow", err)
	}
	// Cap 0 = no spend allowed: any positive estimate refuses.
	if err := led.SetKeyBudget(ctx, "k3", 0, "fail_closed"); err != nil {
		t.Fatalf("SetKeyBudget k3: %v", err)
	}
	if _, _, err := led.CheckBudgetForEstimate(ctx, "k3", 0.00001); err == nil {
		t.Errorf("estimate vs $0 rolling cap: allowed, want refusal")
	}
	// FailClosed off: over-cap estimate allows (with a warning),
	// exactly like the legacy store.
	led.FailClosed = false
	if _, warn, err := led.CheckBudgetForEstimate(ctx, "k1", 0.0005); err != nil {
		t.Errorf("FailClosed off, over-cap estimate: %v, want allow", err)
	} else if !warn {
		t.Errorf("FailClosed off, over-cap estimate: warn=false, want true")
	}
	// Both stores set: the legacy cap is huge, the rolling cap is
	// tiny — the rolling store refuses and its remaining wins.
	led.FailClosed = true
	if err := led.SetBudget(ctx, "k1", 100.0, ""); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	if rem, _, err := led.CheckBudgetForEstimate(ctx, "k1", 0.0005); err == nil {
		t.Errorf("legacy cap $100 + rolling cap $0.0001: allowed, want rolling refusal")
	} else if rem != 0.0001 {
		t.Errorf("remaining = %v, want 0.0001 (rolling remaining is tighter)", rem)
	}
}
func TestCheckBudgetForEstimate(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	led.FailClosed = true
	if err := led.SetBudget(ctx, "k1", 1.00, ""); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	// No spend yet: a $0.40 estimate allows.
	if _, _, err := led.CheckBudgetForEstimate(ctx, "k1", 0.40); err != nil {
		t.Errorf("estimate $0.40 under a $1 cap: %v, want allow", err)
	}
	// Spend $0.70, then a $0.40 estimate would pass the cap.
	if _, err := led.RecordUsage(ctx, Usage{GatewayKeyID: "k1", Provider: "x", Model: "m",
		PromptTokens: 70, CompletionTokens: 0, TotalTokens: 70, SplitKnown: true, OK: true},
		Prices{{Model: "m"}: {PromptPerToken: 0.01}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, err := led.CheckBudgetForEstimate(ctx, "k1", 0.40); err == nil {
		t.Errorf("spent $0.70 + estimate $0.40 past a $1 cap: allowed, want refusal")
	} else if !IsBudgetExceeded(err) {
		t.Errorf("refusal error = %T %v, want *BudgetExceededError", err, err)
	}
	// A $0.20 estimate still fits ($0.70 + $0.20 < $1).
	if _, _, err := led.CheckBudgetForEstimate(ctx, "k1", 0.20); err != nil {
		t.Errorf("estimate $0.20 under remaining $0.30: %v, want allow", err)
	}
	// Zero estimate: degrades to the exhausted check (not
	// exhausted yet, so allows).
	if _, _, err := led.CheckBudgetForEstimate(ctx, "k1", 0); err != nil {
		t.Errorf("zero estimate: %v, want allow", err)
	}
	// No budget row: unlimited whatever the estimate.
	if rem, _, err := led.CheckBudgetForEstimate(ctx, "no-such-key", 100.0); err != nil {
		t.Errorf("unbudgeted key: %v, want allow", err)
	} else if !math.IsInf(rem, 1) {
		t.Errorf("unbudgeted key remaining = %v, want +Inf", rem)
	}
}
