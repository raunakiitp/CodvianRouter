package ledger

// Cached tokens (CHECKLIST 132): provider-reported cached input
// tokens are stored alongside a known split, aggregated in Summary
// (totals + per-provider + per-(provider, model) combo), and served
// to the dashboard. Nothing here estimates: a row only counts what
// the upstream reported.

import (
	"context"
	"testing"
	"time"
)

func TestRecordUsageCachedTokens(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)

	// Two requests on one combo, one with cached tokens.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 1000, CompletionTokens: 50, TotalTokens: 1050,
		CachedTokens: 800, SplitKnown: true, OK: true, LatencyMS: 100,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 200, CompletionTokens: 20, TotalTokens: 220,
		CachedTokens: 0, SplitKnown: true, OK: true, LatencyMS: 90,
	}, nil); err != nil {
		t.Fatalf("RecordUsage 2: %v", err)
	}
	// A different combo with cached tokens, plus a failed attempt
	// (no usage reported) and a shared waiter row (forced zero).
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "anthropic", Model: "claude-x",
		PromptTokens: 500, CompletionTokens: 30, TotalTokens: 530,
		CachedTokens: 100, SplitKnown: true, OK: true, LatencyMS: 80,
	}, nil); err != nil {
		t.Fatalf("RecordUsage 3: %v", err)
	}
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		OK: false, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage failed: %v", err)
	}
	if _, err := l.RecordShared(ctx, "openai", "gpt-4o", true, 5); err != nil {
		t.Fatalf("RecordShared: %v", err)
	}

	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("events = %d, want 5", len(events))
	}
	// Newest first: shared row, failed row, anthropic, gpt-4o #2, #1.
	if events[0].CachedTokens != 0 || !events[0].Shared {
		t.Fatalf("shared row = %+v, want cached 0 + shared", events[0])
	}
	if events[1].CachedTokens != 0 || events[1].OK {
		t.Fatalf("failed row = %+v, want cached 0 + not ok", events[1])
	}
	if events[2].CachedTokens != 100 || events[2].Provider != "anthropic" {
		t.Fatalf("anthropic row = %+v, want cached 100", events[2])
	}
	if events[3].CachedTokens != 0 || events[3].PromptTokens != 200 {
		t.Fatalf("gpt-4o #2 row = %+v, want cached 0 prompt 200", events[3])
	}
	if events[4].CachedTokens != 800 || events[4].PromptTokens != 1000 {
		t.Fatalf("gpt-4o #1 row = %+v, want cached 800 prompt 1000", events[4])
	}

	sum, err := l.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Requests != 5 {
		t.Fatalf("summary requests = %d, want 5", sum.Requests)
	}
	if sum.CachedTokens != 900 {
		t.Fatalf("summary cached = %d, want 900", sum.CachedTokens)
	}
	if sum.RequestsWithCachedTokens != 2 {
		t.Fatalf("summary requests-with-cached = %d, want 2", sum.RequestsWithCachedTokens)
	}
	if sum.PromptTokens != 1700 {
		t.Fatalf("summary prompt = %d, want 1700", sum.PromptTokens)
	}
	// Per-combo aggregate: gpt-4o has 4 requests (two served, one
	// failed, one shared waiter — the failed and shared rows carry
	// cached 0), 800 cached of 1200 prompt.
	byCombo := map[string]ModelUsage{}
	for _, m := range sum.ByModel {
		byCombo[m.Provider+"/"+m.Model] = m
	}
	gpt := byCombo["openai/gpt-4o"]
	if gpt.Requests != 4 || gpt.CachedTokens != 800 || gpt.RequestsWithCachedTokens != 1 || gpt.PromptTokens != 1200 {
		t.Fatalf("gpt-4o combo = %+v, want requests 4 cached 800 with-cached 1 prompt 1200", gpt)
	}
	claude := byCombo["anthropic/claude-x"]
	if claude.Requests != 1 || claude.CachedTokens != 100 || claude.RequestsWithCachedTokens != 1 {
		t.Fatalf("claude-x combo = %+v, want requests 1 cached 100 with-cached 1", claude)
	}
	// Per-provider aggregate mirrors the same counts.
	byProv := map[string]ProviderUsage{}
	for _, p := range sum.ByProvider {
		byProv[p.Provider] = p
	}
	if byProv["openai"].CachedTokens != 800 || byProv["openai"].RequestsWithCachedTokens != 1 {
		t.Fatalf("openai provider = %+v, want cached 800 with-cached 1", byProv["openai"])
	}
}

func TestRecordUsageCachedTokensHonestyRules(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)

	// Cached tokens without a known split are not stored: a cached
	// count is meaningless without the prompt split it belongs to.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		TotalTokens: 500, CachedTokens: 400, SplitKnown: false, OK: true, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// Cached tokens above the prompt count are clamped, never stored
	// as a larger-than-prompt claim.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110,
		CachedTokens: 999, SplitKnown: true, OK: true, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage 2: %v", err)
	}
	// A shared row can never carry cached tokens, whatever the
	// caller passed (the leader's row carries the real usage).
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110,
		CachedTokens: 90, SplitKnown: true, OK: true, LatencyMS: 10, Shared: true,
	}, nil); err != nil {
		t.Fatalf("RecordUsage shared: %v", err)
	}
	// A negative cached count is stored as 0, never negative.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o",
		PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110,
		CachedTokens: -5, SplitKnown: true, OK: true, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage negative: %v", err)
	}

	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	// Newest first.
	if events[0].CachedTokens != 0 {
		t.Fatalf("negative cached stored as %d, want 0", events[0].CachedTokens)
	}
	if events[1].CachedTokens != 0 || !events[1].Shared {
		t.Fatalf("shared row cached = %d, want 0", events[1].CachedTokens)
	}
	if events[2].CachedTokens != 100 {
		t.Fatalf("clamped cached = %d, want 100 (the prompt count)", events[2].CachedTokens)
	}
	if events[3].CachedTokens != 0 || events[3].TotalTokens != 500 {
		t.Fatalf("unknown-split row = %+v, want cached 0 total 500", events[3])
	}
	sum, err := l.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.CachedTokens != 100 || sum.RequestsWithCachedTokens != 1 {
		t.Fatalf("summary cached = %d with-cached = %d, want 100 / 1", sum.CachedTokens, sum.RequestsWithCachedTokens)
	}
}
