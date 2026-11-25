package router

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// Retry-After abandon rule + floor (CHECKLIST 167-168; Retry-After >= 60s ends retries:
// Retry-After >= 60s ends retries for that target; 10s floor, research-log entry 146).
// floor, research-log entry 146). These tests use fake clients (no
// network) and the real Engine.

func TestClassifyRetryAfter(t *testing.T) {
	cap := 30 * time.Second
	for _, tc := range []struct {
		name     string
		ra       time.Duration
		present  bool
		wantWait time.Duration
		wantDec  string
		wantAb   bool
	}{
		{"absent hint backs off", 0, false, 0, "", false},
		{"explicit 0s floors", 0, true, 10 * time.Second, RetryDecisionFloored, false},
		{"1s floors", time.Second, true, 10 * time.Second, RetryDecisionFloored, false},
		{"5s floors", 5 * time.Second, true, 10 * time.Second, RetryDecisionFloored, false},
		{"9.999s floors", 9999 * time.Millisecond, true, 10 * time.Second, RetryDecisionFloored, false},
		{"10s honoured", 10 * time.Second, true, 10 * time.Second, RetryDecisionHonoured, false},
		{"20s honoured", 20 * time.Second, true, 20 * time.Second, RetryDecisionHonoured, false},
		{"45s capped at 30s", 45 * time.Second, true, 30 * time.Second, RetryDecisionCapped, false},
		{"59s capped at 30s", 59 * time.Second, true, 30 * time.Second, RetryDecisionCapped, false},
		{"59s honoured with a 120s cap", 59 * time.Second, true, 59 * time.Second, RetryDecisionHonoured, false},
		{"60s abandons", 60 * time.Second, true, 0, RetryDecisionAbandoned, true},
		{"60s abandons even with a 120s cap", 60 * time.Second, true, 0, RetryDecisionAbandoned, true},
		{"120s abandons", 120 * time.Second, true, 0, RetryDecisionAbandoned, true},
		{"3600s abandons", 3600 * time.Second, true, 0, RetryDecisionAbandoned, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cap
			if tc.name == "59s honoured with a 120s cap" || tc.name == "60s abandons even with a 120s cap" {
				c = 120 * time.Second
			}
			wait, dec, ab := classifyRetryAfter(tc.ra, tc.present, c)
			if wait != tc.wantWait || dec != tc.wantDec || ab != tc.wantAb {
				t.Fatalf("classifyRetryAfter(%v, present=%v, cap=%v) = (%v, %q, %v), want (%v, %q, %v)",
					tc.ra, tc.present, c, wait, dec, ab, tc.wantWait, tc.wantDec, tc.wantAb)
			}
		})
	}
}

func TestRateLimitWaitFloorAndAbandon(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig()) // RetryAfterCap 30s
	for _, tc := range []struct {
		name    string
		ra      time.Duration
		present bool
		want    time.Duration
	}{
		{"explicit 0s waits the 10s floor", 0, true, 10 * time.Second},
		{"1s waits the 10s floor", time.Second, true, 10 * time.Second},
		{"5s waits the 10s floor", 5 * time.Second, true, 10 * time.Second},
		{"59s waits the 30s cap", 59 * time.Second, true, 30 * time.Second},
		{"60s waits nothing (abandoned)", 60 * time.Second, true, 0},
		{"120s waits nothing (abandoned)", 120 * time.Second, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eng.rateLimitWait(tc.ra, tc.present, 1); got != tc.want {
				t.Fatalf("rateLimitWait(%v, present=%v) = %v, want %v", tc.ra, tc.present, got, tc.want)
			}
		})
	}
	// An absent hint keeps exponential backoff (base 1ms, max 5ms
	// in testConfig): short, jittered, never the 10s floor.
	if got := eng.rateLimitWait(0, false, 1); got <= 0 || got > 5*time.Millisecond {
		t.Fatalf("rateLimitWait(absent) = %v, want (0, 5ms] backoff", got)
	}
}

// TestRetryAfterAbandonMovesToNextTarget: a 120s Retry-After on the
// first target abandons THAT target — the same request succeeds via
// the healthy target, the abandoned target is hit exactly once, and
// the request finishes fast (no 30s sleep, no same-target retry).
func TestRetryAfterAbandonMovesToNextTarget(t *testing.T) {
	a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.RateLimitError{RetryAfter: 120 * time.Second, RetryAfterPresent: true, Body: "slow down"}
	}}
	b := okClient("b-ok")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	start := time.Now()
	resp, trace, err := eng.ChatWithChain(context.Background(), chain, testReq())
	elapsed := time.Since(start)
	if err != nil || resp.Content != "b-ok" {
		t.Fatalf("Chat: resp=%v err=%v, want b-ok + nil", resp, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("elapsed = %v, want < 2s (an abandoned target must not park the request)", elapsed)
	}
	if a.chatCalls() != 1 {
		t.Fatalf("abandoned target calls = %d, want 1 (never retried)", a.chatCalls())
	}
	if b.chatCalls() != 1 {
		t.Fatalf("healthy target calls = %d, want 1", b.chatCalls())
	}
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %v, want 2", trace)
	}
	if outcome(trace, 0) != "rate_limited" {
		t.Fatalf("attempt 0 outcome = %q, want rate_limited", outcome(trace, 0))
	}
	ra := trace.Attempts[0].RetryAfter
	if ra == nil {
		t.Fatalf("attempt 0 has no Retry-After record, want the 120s hint + abandoned decision")
	}
	if ra.Seconds != 120 || ra.Decision != RetryDecisionAbandoned || ra.WaitSeconds != 0 {
		t.Fatalf("attempt 0 retry_after = %+v, want {120 abandoned 0}", ra)
	}
	if trace.Attempts[1].RetryAfter != nil {
		t.Fatalf("attempt 1 retry_after = %+v, want nil (the healthy target carried no hint)", trace.Attempts[1].RetryAfter)
	}
}

// TestRetryAfterFiftyNineSecondsRetriesSameTarget: 59s does NOT
// abandon. The engine's RetryAfterCap is 100ms here so the capped
// 59s wait is fast — the point is the retry happens at all, which
// proves 59s is not abandoned. (The 60s boundary is pinned by
// TestRetryAfterSixtySecondsAbandonsSingleTarget with the same cap.)
func TestRetryAfterFiftyNineSecondsRetriesSameTarget(t *testing.T) {
	var calls int
	a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			return nil, &provider.RateLimitError{RetryAfter: 59 * time.Second, RetryAfterPresent: true, Body: "slow down"}
		}
		return &provider.ChatResponse{Content: "a-ok"}, nil
	}}
	cfg := testConfig()
	cfg.MaxAttempts = 3
	cfg.RetryAfterCap = 100 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	start := time.Now()
	resp, trace, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
	elapsed := time.Since(start)
	if err != nil || resp.Content != "a-ok" {
		t.Fatalf("Chat: resp=%v err=%v, want a-ok + nil", resp, err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("elapsed = %v, want < 5s (the 59s wait is capped at 100ms)", elapsed)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (59s retries the same target)", calls)
	}
	ra := trace.Attempts[0].RetryAfter
	if ra == nil || ra.Seconds != 59 || ra.Decision != RetryDecisionCapped || ra.WaitSeconds != 0.1 {
		t.Fatalf("attempt 0 retry_after = %+v, want {59 capped 0.1}", ra)
	}
}

// TestRetryAfterSixtySecondsAbandonsSingleTarget: with no healthy
// target left, a >= 60s Retry-After fails honestly and fast — the
// target is hit exactly once, the request never sleeps out the wait
// and retries it.
func TestRetryAfterSixtySecondsAbandonsSingleTarget(t *testing.T) {
	for _, ra := range []time.Duration{60 * time.Second, 120 * time.Second} {
		t.Run(ra.String(), func(t *testing.T) {
			a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
				return nil, &provider.RateLimitError{RetryAfter: ra, RetryAfterPresent: true, Body: "slow down"}
			}}
			cfg := testConfig()
			cfg.MaxAttempts = 3
			cfg.RetryAfterCap = 100 * time.Millisecond
			eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
			start := time.Now()
			_, trace, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
			elapsed := time.Since(start)
			if err == nil {
				t.Fatalf("Chat err = nil, want the rate-limit error (no target left)")
			}
			if elapsed > 2*time.Second {
				t.Fatalf("elapsed = %v, want < 2s (an abandoned target must not park the request)", elapsed)
			}
			if a.chatCalls() != 1 {
				t.Fatalf("calls = %d, want 1 (never retried)", a.chatCalls())
			}
			if trace == nil || len(trace.Attempts) != 1 {
				t.Fatalf("trace attempts = %v, want 1", trace)
			}
			got := trace.Attempts[0].RetryAfter
			if got == nil || got.Seconds != ra.Seconds() || got.Decision != RetryDecisionAbandoned || got.WaitSeconds != 0 {
				t.Fatalf("attempt 0 retry_after = %+v, want {%v abandoned 0}", got, ra.Seconds())
			}
		})
	}
}

// TestRetryAfterAbandonStreaming: the abandon rule holds on the
// streaming path too — a 120s Retry-After before any content fails
// over to the healthy target in the same request, and abandons a
// lone target fast instead of sleeping and retrying it.
func TestRetryAfterAbandonStreaming(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	rl120 := &provider.RateLimitError{RetryAfter: 120 * time.Second, RetryAfterPresent: true, Body: "slow down"}

	t.Run("fails over to healthy target", func(t *testing.T) {
		a := streamClient(nil, rl120)
		b := streamClient([]string{"bee"}, nil)
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		ch, err := eng.ChatStreamWithChain(context.Background(), chain, testReq())
		if err != nil {
			t.Fatalf("ChatStreamWithChain: %v", err)
		}
		start := time.Now()
		out, err := collectStream(t, ch)
		if time.Since(start) > 2*time.Second {
			t.Fatalf("stream took %v, want < 2s", time.Since(start))
		}
		if err != nil || out != "bee" {
			t.Fatalf("stream = %q, err = %v, want bee + nil", out, err)
		}
		if a.streamCalls() != 1 || b.streamCalls() != 1 {
			t.Fatalf("stream calls a=%d b=%d, want 1/1", a.streamCalls(), b.streamCalls())
		}
		trace := eng.LastTrace()
		if trace == nil || len(trace.Attempts) != 2 {
			t.Fatalf("trace = %+v, want 2 attempts", trace)
		}
		ra := trace.Attempts[0].RetryAfter
		if ra == nil || ra.Seconds != 120 || ra.Decision != RetryDecisionAbandoned {
			t.Fatalf("attempt 0 retry_after = %+v, want {120 abandoned 0}", ra)
		}
	})

	t.Run("lone target abandons fast", func(t *testing.T) {
		a := streamClient(nil, rl120)
		eng := NewEngine(map[string]provider.Client{"a": a}, nil, testConfig())
		ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
		if err != nil {
			t.Fatalf("ChatStreamWithChain: %v", err)
		}
		start := time.Now()
		_, err = collectStream(t, ch)
		if time.Since(start) > 2*time.Second {
			t.Fatalf("stream took %v, want < 2s", time.Since(start))
		}
		if err == nil {
			t.Fatalf("stream err = nil, want the rate-limit error")
		}
		if a.streamCalls() != 1 {
			t.Fatalf("stream calls = %d, want 1 (never retried)", a.streamCalls())
		}
	})
}

// TestRetryAfterFloorSoftCooldown: in soft-demote mode a present 2s
// Retry-After cools the target for the 10s floor, not 2s — the
// soft cooldown is part of the retry path.
func TestRetryAfterFloorSoftCooldown(t *testing.T) {
	ctx := context.Background()
	chain := chainFor("a", "b")
	rl := &provider.RateLimitError{RetryAfter: 2 * time.Second, RetryAfterPresent: true, Body: "slow down"}
	eng := softEngine(t, map[string]provider.Client{"a": errClient(rl), "b": okClient("bee")})
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
		t.Fatalf("req: %v", err)
	}
	targets := eng.CooldownTargets()
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want 1", targets)
	}
	if targets[0].RemainingSeconds < 9 || targets[0].RemainingSeconds > 10 {
		t.Fatalf("Retry-After 2s (floored to 10s): remaining = %ds, want 9..10", targets[0].RemainingSeconds)
	}
}

// TestRetryInfoFor ensures the trace record distinguishes a present
// hint from an absent one: no hint records nothing, never a 0s hint.
func TestRetryInfoFor(t *testing.T) {
	if got := retryInfoFor(0, false, 30*time.Second); got != nil {
		t.Fatalf("absent hint: got %+v, want nil", got)
	}
	got := retryInfoFor(0, true, 30*time.Second)
	if got == nil || got.Seconds != 0 || got.Decision != RetryDecisionFloored || got.WaitSeconds != 10 {
		t.Fatalf("explicit 0s hint: got %+v, want {0 floored 10}", got)
	}
	// The record round-trips through the trace JSON the dashboard
	// feed serves (GET /dashboard/trace).
	tr := &routetrace.RouteTrace{}
	tr.AddWithPrereadRetry("a", "m", 3, "rate_limited", nil, "rate_limited", "", 0, 0, got)
	if tr.Attempts[0].RetryAfter == nil || tr.Attempts[0].RetryAfter.Decision != RetryDecisionFloored {
		t.Fatalf("trace attempt retry_after = %+v, want the floored record", tr.Attempts[0].RetryAfter)
	}
}
