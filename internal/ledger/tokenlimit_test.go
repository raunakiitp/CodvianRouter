package ledger

import (
	"context"
	"strings"
	"testing"
	"time"
)

// M5.3: per-key token limits — scope (global / provider / model),
// calendar reset windows, most-restrictive breach, and the ledger
// as the counter (usage_events.total_tokens, successful rows only).

func tokenLimitCtx() context.Context { return context.Background() }

func TestCheckKeyTokenLimitsScopes(t *testing.T) {
	l := openKeyBudgetLedger(t)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	// 60 tokens today on openai/gpt-4o.
	seedUsageRow(t, l, now.Add(-time.Hour), "k1", "openai", "gpt-4o", 0.01, 1, 1, 1, 0, 60)

	global := []TokenLimitConfig{{Scope: "global", Limit: 100, ResetInterval: "daily", Enabled: true}}
	if breach, err := l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "openai", "gpt-4o", global, now); err != nil || breach != nil {
		t.Errorf("60/100 global: breach=%v err=%v, want nil/nil", breach, err)
	}
	// 50 more tokens push the window to 110: global breaches.
	seedUsageRow(t, l, now.Add(-30*time.Minute), "k1", "openai", "gpt-4o", 0.01, 1, 1, 1, 0, 50)
	if breach, err := l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "openai", "gpt-4o", global, now); err != nil || breach == nil {
		t.Errorf("110/100 global: breach=%v err=%v, want a breach", breach, err)
	} else if breach.Scope != "global" || breach.Limit != 100 || breach.Used != 110 || breach.Remaining != 0 {
		t.Errorf("breach = %+v, want global 100/110/0", breach)
	}

	// Model scope fires only for its model.
	modelHit := []TokenLimitConfig{{Scope: "model", ScopeValue: "gpt-4o", Limit: 100, ResetInterval: "daily", Enabled: true}}
	if breach, _ := l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "openai", "gpt-4o", modelHit, now); breach == nil {
		t.Error("model limit on gpt-4o must breach at 110/100")
	}
	modelMiss := []TokenLimitConfig{{Scope: "model", ScopeValue: "claude-3", Limit: 100, ResetInterval: "daily", Enabled: true}}
	if breach, _ := l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "openai", "claude-3", modelMiss, now); breach != nil {
		t.Errorf("model limit on claude-3 must not breach (no usage): %v", breach)
	}
	// Provider scope fires only for its provider; an unknown
	// provider ("") skips provider-scoped limits entirely.
	provHit := []TokenLimitConfig{{Scope: "provider", ScopeValue: "openai", Limit: 100, ResetInterval: "daily", Enabled: true}}
	if breach, _ := l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "openai", "gpt-4o", provHit, now); breach == nil {
		t.Error("provider limit on openai must breach at 110/100")
	}
	provMiss := []TokenLimitConfig{{Scope: "provider", ScopeValue: "anthropic", Limit: 100, ResetInterval: "daily", Enabled: true}}
	if breach, _ := l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "anthropic", "claude-3", provMiss, now); breach != nil {
		t.Errorf("provider limit on anthropic must not breach (no usage): %v", breach)
	}
	if breach, _ := l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "", "gpt-4o", provHit, now); breach != nil {
		t.Errorf("provider limit with unknown provider must be skipped: %v", breach)
	}
	// A different key is unaffected.
	if breach, _ := l.CheckKeyTokenLimits(tokenLimitCtx(), "k2", "openai", "gpt-4o", global, now); breach != nil {
		t.Errorf("another key must not share the counter: %v", breach)
	}
}

func TestCheckKeyTokenLimitsWindow(t *testing.T) {
	l := openKeyBudgetLedger(t)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	limits := []TokenLimitConfig{{Scope: "global", Limit: 100, ResetInterval: "daily", Enabled: true}}
	// Yesterday's 200 tokens never count toward today's window.
	seedUsageRow(t, l, now.AddDate(0, 0, -1), "k1", "openai", "gpt-4o", 0.01, 1, 1, 1, 0, 200)
	if breach, _ := l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "openai", "gpt-4o", limits, now); breach != nil {
		t.Errorf("yesterday's usage must not breach today: %v", breach)
	}
	// 100 tokens today breaches exactly at the limit.
	seedUsageRow(t, l, now.Add(-time.Hour), "k1", "openai", "gpt-4o", 0.01, 1, 1, 1, 0, 100)
	if breach, _ := l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "openai", "gpt-4o", limits, now); breach == nil {
		t.Error("100/100 must breach (used >= limit)")
	}
	// Reset time shifts the window: with a 09:30 reset, spend at
	// 08:00 belongs to yesterday's window.
	l2 := openKeyBudgetLedger(t)
	limits930 := []TokenLimitConfig{{Scope: "global", Limit: 100, ResetInterval: "daily", ResetTime: "09:30", Enabled: true}}
	seedUsageRow(t, l2, time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC), "k1", "openai", "gpt-4o", 0.01, 1, 1, 1, 0, 100)
	if breach, _ := l2.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "openai", "gpt-4o", limits930, time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)); breach != nil {
		t.Errorf("08:00 spend must belong to the previous window under a 09:30 reset: %v", breach)
	}
}

func TestCheckKeyTokenLimitsMostRestrictive(t *testing.T) {
	l := openKeyBudgetLedger(t)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	// 990 total (all gpt-4o): global 1000 has 10 left, model 100
	// has 5 left — the model breach wins (smallest remaining).
	seedUsageRow(t, l, now.Add(-time.Hour), "k1", "openai", "gpt-4o", 0.01, 1, 1, 1, 0, 990)
	limits := []TokenLimitConfig{
		{Scope: "global", Limit: 1000, ResetInterval: "daily", Enabled: true},
		{Scope: "model", ScopeValue: "gpt-4o", Limit: 100, ResetInterval: "daily", Enabled: true},
	}
	// The model limit needs usage >= 100 on gpt-4o — it has 990,
	// so both breach; the model one has the smaller remaining.
	breach, err := l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "openai", "gpt-4o", limits, now)
	if err != nil || breach == nil {
		t.Fatalf("breach=%v err=%v, want the model breach", breach, err)
	}
	if breach.Scope != "model" || breach.Remaining != 0 {
		t.Errorf("breach = %+v, want model scope with remaining 0 (1000-990=10 global vs 100-990<0 model)", breach)
	}
	// Tie-break: equal remaining (0) goes to the smaller limit.
	limits2 := []TokenLimitConfig{
		{Scope: "global", Limit: 1000, ResetInterval: "daily", Enabled: true},
		{Scope: "model", ScopeValue: "gpt-4o", Limit: 990, ResetInterval: "daily", Enabled: true},
	}
	breach, _ = l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "openai", "gpt-4o", limits2, now)
	if breach == nil || breach.Scope != "model" || breach.Limit != 990 {
		t.Errorf("tie-break breach = %+v, want the model limit (990 < 1000)", breach)
	}
}

func TestCheckKeyTokenLimitsIgnoresFailedAndDisabled(t *testing.T) {
	l := openKeyBudgetLedger(t)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	// 500 tokens on FAILED rows (ok=0) consume no billable tokens.
	seedUsageRow(t, l, now.Add(-time.Hour), "k1", "openai", "gpt-4o", 0.01, 0, 1, 1, 0, 500)
	limits := []TokenLimitConfig{
		{Scope: "global", Limit: 100, ResetInterval: "daily", Enabled: true},
		{Scope: "global", Limit: 10, ResetInterval: "daily", Enabled: false},
		{Scope: "global", Limit: 10, ResetInterval: "daily", Enabled: true, ResetTime: "bogus"},
		{Scope: "global", Limit: 0, ResetInterval: "daily", Enabled: true},
		{Scope: "bogus", Limit: 10, ResetInterval: "daily", Enabled: true},
	}
	if breach, err := l.CheckKeyTokenLimits(tokenLimitCtx(), "k1", "openai", "gpt-4o", limits, now); err != nil || breach != nil {
		t.Errorf("failed rows + disabled/malformed/zero/bogus limits: breach=%v err=%v, want nil/nil", breach, err)
	}
}

func TestTokenLimitBreachError(t *testing.T) {
	b := &TokenLimitBreach{
		Scope: "model", ScopeValue: "gpt-4o", Limit: 100, Used: 120,
		NextReset: time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC),
	}
	msg := b.Error()
	for _, want := range []string{"model gpt-4o", "100 tokens", "120 used", "2026-11-19T00:00:00Z"} {
		if !strings.Contains(msg, want) {
			t.Errorf("breach message %q does not contain %q", msg, want)
		}
	}
	g := &TokenLimitBreach{Scope: "global", Limit: 50, Used: 50,
		NextReset: time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)}
	if !strings.Contains(g.Error(), "global limit of 50") {
		t.Errorf("global breach message = %q, want 'global limit of 50'", g.Error())
	}
}
