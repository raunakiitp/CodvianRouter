// CachedTokenFraction tests (M6.5-wiring): the read-only
// per-(provider, model) cached-token fraction the cache-optimized
// strategy orders on. Nothing here estimates: the fraction is
// provider-reported cached tokens over provider-reported prompt
// tokens, from successful rows with a known split — and a pair
// with no such rows reports ok=false, never a guessed 0.
package ledger

import (
	"context"
	"testing"
	"time"
)

func TestCachedTokenFraction(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	since := time.Now().Add(-time.Hour)

	// openai/gpt-4o: 800 cached of 1000 prompt, then 0 of 200 —
	// fraction 800/1200 = 2/3.
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
	// anthropic/claude-x: 100 cached of 500 — fraction 0.2.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "anthropic", Model: "claude-x",
		PromptTokens: 500, CompletionTokens: 30, TotalTokens: 530,
		CachedTokens: 100, SplitKnown: true, OK: true, LatencyMS: 80,
	}, nil); err != nil {
		t.Fatalf("RecordUsage 3: %v", err)
	}
	// openai/gpt-4o-mini: a failed attempt only — no usable row.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o-mini",
		OK: false, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage failed: %v", err)
	}
	// openai/gpt-4o-unknown: cached tokens without a known split
	// — stored as 0 with no prompt, so no measurement.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "openai", Model: "gpt-4o-unknown",
		TotalTokens: 500, CachedTokens: 400, SplitKnown: false, OK: true, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage unknown-split: %v", err)
	}

	f, ok, err := l.CachedTokenFraction(ctx, "openai", "gpt-4o", since)
	if err != nil {
		t.Fatalf("CachedTokenFraction: %v", err)
	}
	if !ok {
		t.Fatalf("CachedTokenFraction gpt-4o ok = false, want true")
	}
	if f != 800.0/1200.0 {
		t.Errorf("CachedTokenFraction gpt-4o = %v, want %v (800/1200)", f, 800.0/1200.0)
	}

	f, ok, err = l.CachedTokenFraction(ctx, "anthropic", "claude-x", since)
	if err != nil {
		t.Fatalf("CachedTokenFraction: %v", err)
	}
	if !ok || f != 0.2 {
		t.Errorf("CachedTokenFraction claude-x = (%v, %v), want (0.2, true)", f, ok)
	}

	// A pair with no rows at all: no measurement.
	if _, ok, err := l.CachedTokenFraction(ctx, "openai", "no-such-model", since); err != nil {
		t.Fatalf("CachedTokenFraction unknown pair: %v", err)
	} else if ok {
		t.Errorf("CachedTokenFraction unknown pair ok = true, want false (no data, never a guessed 0)")
	}

	// A pair with only a failed row: no measurement.
	if _, ok, err := l.CachedTokenFraction(ctx, "openai", "gpt-4o-mini", since); err != nil {
		t.Fatalf("CachedTokenFraction failed-only pair: %v", err)
	} else if ok {
		t.Errorf("CachedTokenFraction failed-only pair ok = true, want false")
	}

	// A pair with only an unknown-split row: no measurement.
	if _, ok, err := l.CachedTokenFraction(ctx, "openai", "gpt-4o-unknown", since); err != nil {
		t.Fatalf("CachedTokenFraction unknown-split pair: %v", err)
	} else if ok {
		t.Errorf("CachedTokenFraction unknown-split pair ok = true, want false")
	}

	// A window after every row: no measurement in-window.
	if _, ok, err := l.CachedTokenFraction(ctx, "openai", "gpt-4o", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CachedTokenFraction future window: %v", err)
	} else if ok {
		t.Errorf("CachedTokenFraction future window ok = true, want false")
	}
}
