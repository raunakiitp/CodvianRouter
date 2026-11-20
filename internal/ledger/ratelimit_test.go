package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
)

func TestLearnedRateLimits(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now().UTC()

	sig := adaptive.RateLimitSignals{
		HasRequests: true, RequestsLimit: 100, RequestsRemaining: 10,
		HasTokens: true, TokensLimit: 1000, TokensRemaining: 900,
		ObservedAt: now,
	}
	if err := l.LearnRateLimit(ctx, "openai", "key1", sig, now); err != nil {
		t.Fatal(err)
	}
	// Empty signals persist nothing.
	if err := l.LearnRateLimit(ctx, "openai", "key1", adaptive.RateLimitSignals{ObservedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	got, found, err := l.LearnedSignalsForKey(ctx, "openai", "key1", now)
	if err != nil || !found {
		t.Fatalf("LearnedSignalsForKey = %+v, %v, %v", got, found, err)
	}
	if !got.HasRequests || got.RequestsLimit != 100 || got.RequestsRemaining != 10 {
		t.Errorf("requests = %+v, want 100/10", got)
	}
	if !got.HasTokens || got.TokensLimit != 1000 || got.TokensRemaining != 900 {
		t.Errorf("tokens = %+v, want 1000/900", got)
	}
	// A newer signal overwrites (upsert), per key independently.
	if err := l.LearnRateLimit(ctx, "openai", "key1", adaptive.RateLimitSignals{
		HasRequests: true, RequestsLimit: 100, RequestsRemaining: 5, ObservedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := l.LearnRateLimit(ctx, "openai", "key2", adaptive.RateLimitSignals{
		HasTokens: true, TokensLimit: 500, TokensRemaining: 500, ObservedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	got, _, _ = l.LearnedSignalsForKey(ctx, "openai", "key1", now)
	if got.RequestsRemaining != 5 || !got.HasTokens || got.TokensRemaining != 900 {
		t.Errorf("after upsert = %+v, want requests 5 + tokens 900 kept", got)
	}
	got2, found2, _ := l.LearnedSignalsForKey(ctx, "openai", "key2", now)
	if !found2 || !got2.HasTokens || got2.TokensLimit != 500 {
		t.Errorf("key2 = %+v, %v, want tokens 500", got2, found2)
	}
	if _, found3, _ := l.LearnedSignalsForKey(ctx, "anthropic", "key1", now); found3 {
		t.Error("unknown provider returned a signal")
	}
	// Stale rows are invisible: past the 5-minute TTL.
	stale, _, _ := l.LearnedSignalsForKey(ctx, "openai", "key1", now.Add(6*time.Minute))
	if stale.HasRequests || stale.HasTokens {
		t.Errorf("stale read = %+v, want empty", stale)
	}
	// And past a reset.
	reset := now.Add(time.Minute)
	if err := l.LearnRateLimit(ctx, "openai", "key3", adaptive.RateLimitSignals{
		HasTokens: true, TokensLimit: 100, TokensRemaining: 1,
		TokensResetAt: &reset, ObservedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := l.LearnedSignalsForKey(ctx, "openai", "key3", now.Add(2*time.Minute)); found {
		t.Error("signal past its reset still returned")
	}
	// Prune removes exactly the stale rows.
	n, err := l.PruneLearnedRateLimits(ctx, now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("pruned = %d rows, want 4 (key1 x2, key2 x1, key3 x1)", n)
	}
	if rows, _ := l.LearnedRateLimits(ctx, now.Add(6*time.Minute)); len(rows) != 0 {
		t.Errorf("rows after prune = %d, want 0", len(rows))
	}
}
