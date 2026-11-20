package adaptive

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRateLimitHeadersOpenAI(t *testing.T) {
	now := time.Now().UTC()
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "5000")
	h.Set("x-ratelimit-remaining-requests", "4990")
	h.Set("x-ratelimit-reset-requests", "6m0s")
	h.Set("x-ratelimit-limit-tokens", "200000")
	h.Set("x-ratelimit-remaining-tokens", "150000")
	h.Set("x-ratelimit-reset-tokens", "1m30s")
	sig := ParseRateLimitHeaders(h, now)
	if !sig.HasRequests || sig.RequestsLimit != 5000 || sig.RequestsRemaining != 4990 {
		t.Errorf("requests = %+v, want 5000/4990", sig)
	}
	if sig.RequestsResetAt == nil || sig.RequestsResetAt.Sub(now) != 6*time.Minute {
		t.Errorf("requests reset = %v, want now+6m", sig.RequestsResetAt)
	}
	if !sig.HasTokens || sig.TokensLimit != 200000 || sig.TokensRemaining != 150000 {
		t.Errorf("tokens = %+v, want 200000/150000", sig)
	}
	if sig.TokensResetAt == nil || sig.TokensResetAt.Sub(now) != 90*time.Second {
		t.Errorf("tokens reset = %v, want now+90s", sig.TokensResetAt)
	}
	// Most restrictive fraction wins: tokens 0.75 < requests 0.998.
	if got := sig.RemainingFraction(); got != 0.75 {
		t.Errorf("RemainingFraction = %v, want 0.75", got)
	}
	if got := sig.Saturation(); got != 0.25 {
		t.Errorf("Saturation = %v, want 0.25", got)
	}
	if !sig.Fresh(now) || sig.Empty() {
		t.Errorf("Fresh/Empty = %v/%v, want true/false", sig.Fresh(now), sig.Empty())
	}
}

func TestParseRateLimitHeadersAnthropic(t *testing.T) {
	now := time.Now().UTC()
	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-limit", "1000")
	h.Set("anthropic-ratelimit-requests-remaining", "999")
	h.Set("anthropic-ratelimit-tokens-limit", "100000")
	h.Set("anthropic-ratelimit-tokens-remaining", "80000")
	h.Set("anthropic-ratelimit-tokens-reset", "2026-11-18T17:00:00Z")
	sig := ParseRateLimitHeaders(h, now)
	if !sig.HasRequests || sig.RequestsLimit != 1000 || sig.RequestsRemaining != 999 {
		t.Errorf("requests = %+v, want 1000/999", sig)
	}
	if !sig.HasTokens || sig.TokensLimit != 100000 || sig.TokensRemaining != 80000 {
		t.Errorf("tokens = %+v, want 100000/80000", sig)
	}
	wantReset, _ := time.Parse(time.RFC3339, "2026-11-18T17:00:00Z")
	if sig.TokensResetAt == nil || !sig.TokensResetAt.Equal(wantReset) {
		t.Errorf("tokens reset = %v, want %v", sig.TokensResetAt, wantReset)
	}
}

func TestParseRateLimitHeadersGenericAndFallbacks(t *testing.T) {
	now := time.Now().UTC()
	// Generic pair, no -requests variants.
	h := http.Header{}
	h.Set("x-ratelimit-limit", "60")
	h.Set("x-ratelimit-remaining", "0")
	h.Set("Retry-After", "120")
	sig := ParseRateLimitHeaders(h, now)
	if !sig.HasRequests || sig.RequestsLimit != 60 || sig.RequestsRemaining != 0 {
		t.Errorf("requests = %+v, want 60/0", sig)
	}
	if sig.RetryAfter == nil || *sig.RetryAfter != 120*time.Second {
		t.Errorf("RetryAfter = %v, want 120s", sig.RetryAfter)
	}
	if got := sig.RemainingFraction(); got != 0 {
		t.Errorf("RemainingFraction = %v, want 0 (exhausted)", got)
	}
	// Anthropic input-tokens triple is the fallback when the base
	// tokens triple is absent.
	h2 := http.Header{}
	h2.Set("anthropic-ratelimit-input-tokens-limit", "50000")
	h2.Set("anthropic-ratelimit-input-tokens-remaining", "40000")
	h2.Set("anthropic-ratelimit-input-tokens-reset", "2026-11-18T17:00:00Z")
	sig2 := ParseRateLimitHeaders(h2, now)
	if !sig2.HasTokens || sig2.TokensLimit != 50000 || sig2.TokensRemaining != 40000 {
		t.Errorf("tokens = %+v, want 50000/40000 via input-tokens fallback", sig2)
	}
	// An unparseable limit ignores the whole family, never half.
	h3 := http.Header{}
	h3.Set("x-ratelimit-limit-requests", "not-a-number")
	h3.Set("x-ratelimit-remaining-requests", "10")
	if sig3 := ParseRateLimitHeaders(h3, now); sig3.HasRequests {
		t.Errorf("unparseable limit parsed: %+v", sig3)
	}
	// Empty headers parse to an empty signal.
	if sig4 := ParseRateLimitHeaders(http.Header{}, now); !sig4.Empty() {
		t.Errorf("empty headers = %+v, want empty", sig4)
	}
}

func TestParseGoDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"6m0s":     6 * time.Minute,
		"1s":       time.Second,
		"1h30m15s": time.Hour + 30*time.Minute + 15*time.Second,
		"1.5s":     1500 * time.Millisecond,
		"500ms":    500 * time.Millisecond,
		"2m":       2 * time.Minute,
	}
	for in, want := range cases {
		got, ok := parseGoDuration(in)
		if !ok || got != want {
			t.Errorf("parseGoDuration(%q) = %v, %v; want %v, true", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "6m0s garbage", "abc", "6x", "-5s", "1.2.3s"} {
		if _, ok := parseGoDuration(bad); ok {
			t.Errorf("parseGoDuration(%q) parsed, want rejection", bad)
		}
	}
}

func TestRateLimitSignalsFreshness(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(time.Minute)
	sig := RateLimitSignals{
		HasTokens: true, TokensLimit: 100, TokensRemaining: 50,
		TokensResetAt: &reset, ObservedAt: now,
	}
	if !sig.Fresh(now) || !sig.Fresh(now.Add(30*time.Second)) {
		t.Error("fresh signal reported stale")
	}
	// Past the reset, the numbers are history.
	if sig.Fresh(now.Add(2 * time.Minute)) {
		t.Error("signal past its reset reported fresh")
	}
	// Past the 5-minute TTL, stale even without a reset.
	old := RateLimitSignals{HasRequests: true, RequestsLimit: 100, RequestsRemaining: 50, ObservedAt: now.Add(-6 * time.Minute)}
	if old.Fresh(now) {
		t.Error("signal older than the TTL reported fresh")
	}
}

func TestLimiterApplyLearnedLimitPacesCap(t *testing.T) {
	now := time.Now().UTC()
	l := New(Config{})
	// 10% remaining: the cap paces to MaxLimit * 0.1 = 10.
	l.ApplyLearnedLimit("openai", RateLimitSignals{
		HasTokens: true, TokensLimit: 100, TokensRemaining: 10, ObservedAt: now,
	}, now)
	if got := l.Limit("openai"); got != 10 {
		t.Errorf("Limit after 10%% pacing = %v, want 10", got)
	}
	if paced, ok := l.PacedLimit("openai", now); !ok || paced != 10 {
		t.Errorf("PacedLimit = %v, %v; want 10, true", paced, ok)
	}
	// A healthier signal never raises the cap back up.
	l.ApplyLearnedLimit("openai", RateLimitSignals{
		HasTokens: true, TokensLimit: 100, TokensRemaining: 99, ObservedAt: now,
	}, now)
	if got := l.Limit("openai"); got != 10 {
		t.Errorf("Limit after healthy signal = %v, want 10 (pacing only shrinks)", got)
	}
	// 0% remaining: the floor (2) — a probe stays possible.
	l2 := New(Config{})
	l2.ApplyLearnedLimit("openai", RateLimitSignals{
		HasRequests: true, RequestsLimit: 100, RequestsRemaining: 0, ObservedAt: now,
	}, now)
	if got := l2.Limit("openai"); got != DefaultMinLimit {
		t.Errorf("Limit at 0%% = %v, want floor %v", got, DefaultMinLimit)
	}
	// Stale signals never pace.
	l3 := New(Config{})
	l3.ApplyLearnedLimit("openai", RateLimitSignals{
		HasTokens: true, TokensLimit: 100, TokensRemaining: 1,
		ObservedAt: now.Add(-6 * time.Minute),
	}, now)
	if got := l3.Limit("openai"); got != DefaultInitialLimit {
		t.Errorf("Limit after stale signal = %v, want initial %v", got, DefaultInitialLimit)
	}
	if _, ok := l3.PacedLimit("openai", now); ok {
		t.Error("PacedLimit reported a stale signal")
	}
	// Empty signals are ignored entirely.
	l4 := New(Config{})
	l4.ApplyLearnedLimit("openai", RateLimitSignals{ObservedAt: now}, now)
	if _, ok := l4.StateFor("openai"); ok {
		t.Error("empty signal created limiter state")
	}
	// A nil limiter no-ops.
	var nilLimiter *Limiter
	nilLimiter.ApplyLearnedLimit("openai", RateLimitSignals{HasTokens: true, TokensLimit: 1, TokensRemaining: 0, ObservedAt: now}, now)
}
