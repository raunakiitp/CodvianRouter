// M4.4 — 429 classification: quota-text, concurrency-limit, and
// Retry-After-from-body. All tests use fake clients (no network)
// and the real Engine.
package router

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestQuota429AbandonsTarget: a 429 whose body says the account is
// out of quota is quota_exhausted — the target is abandoned for
// this request and never retried, even when it is the only target.
// Toggle: without the abandon, a lone-target quota 429 sleeps the
// backoff and retries the same target (2 calls); with it, exactly
// 1 call and the trace decision reads "quota_exhausted".
func TestQuota429AbandonsTarget(t *testing.T) {
	ctx := context.Background()
	quota := &provider.RateLimitError{Body: "You exceeded your current quota, please check your plan and billing details"}
	eng := NewEngine(map[string]provider.Client{"a": errClient(quota)}, nil, Config{HedgeDelay: -1, MaxAttempts: 5})
	_, _, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	if err == nil {
		t.Fatalf("lone quota 429: want an error, got nil")
	}
	// Exactly one call: no same-target retry after a quota 429.
	failing := engLastClientCalls(t, eng, "a")
	if failing != 1 {
		t.Fatalf("lone quota 429: provider called %d times, want 1 (abandoned, never retried)", failing)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].RetryAfter == nil || tr.Attempts[0].RetryAfter.Decision != RetryDecisionQuotaExhausted {
		t.Fatalf("trace = %+v, want one attempt with RetryAfter decision %q", tr, RetryDecisionQuotaExhausted)
	}
	// The target cools for the quota cooldown (60 s, no hint): the
	// next request must not hammer the empty account.
	targets := eng.CooldownTargets()
	if len(targets) != 1 || targets[0].RemainingSeconds < 55 || targets[0].RemainingSeconds > 60 {
		t.Fatalf("cooldown targets = %+v, want one target cooling ~60s", targets)
	}
	// ... and the breaker never trips on a 429.
	if st := breakerStatesByID(eng)["a"]; st.ConsecutiveFailures != 0 || st.Open {
		t.Fatalf("breaker = %+v, want closed with 0 counted failures", st)
	}
}

// TestQuota429FailsOverImmediately: with a healthy sibling, a quota
// 429 serves from the sibling and the trace still names the quota
// decision on the failed attempt.
func TestQuota429FailsOverImmediately(t *testing.T) {
	ctx := context.Background()
	quota := &provider.RateLimitError{Body: "insufficient_quota: you have no credits left"}
	eng := NewEngine(map[string]provider.Client{"a": errClient(quota), "b": okClient("bee")}, nil, Config{HedgeDelay: -1})
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("quota failover = %v, %v, want bee + nil", resp, err)
	}
	if len(trace.Attempts) != 2 || trace.Attempts[0].RetryAfter == nil || trace.Attempts[0].RetryAfter.Decision != RetryDecisionQuotaExhausted {
		t.Fatalf("trace attempts = %+v, want first attempt decision %q", trace.Attempts, RetryDecisionQuotaExhausted)
	}
}

// TestConcurrency429CoolsOneSecond: a concurrency-limit 429 fails
// over now and cools the target for exactly 1 s — never the 10 s
// Retry-After floor, never the doubling backoff ladder. Toggle: a
// plain no-hint 429 sets NO hard cooldown at all, so the 1 s
// cooldown target below exists only because the concurrency
// classification ran.
func TestConcurrency429CoolsOneSecond(t *testing.T) {
	ctx := context.Background()
	conc := &provider.RateLimitError{Body: "too many concurrent requests for this model"}
	eng := NewEngine(map[string]provider.Client{"a": errClient(conc), "b": okClient("bee")}, nil, Config{HedgeDelay: -1})
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("concurrency failover = %v, %v, want bee + nil", resp, err)
	}
	if len(trace.Attempts) != 2 || trace.Attempts[0].RetryAfter == nil || trace.Attempts[0].RetryAfter.Decision != RetryDecisionConcurrencyLimited {
		t.Fatalf("trace attempts = %+v, want first attempt decision %q", trace.Attempts, RetryDecisionConcurrencyLimited)
	}
	targets := eng.CooldownTargets()
	if len(targets) != 1 || targets[0].RemainingSeconds != 1 {
		t.Fatalf("cooldown targets = %+v, want one target cooling exactly 1s", targets)
	}
	if st := breakerStatesByID(eng)["a"]; st.ConsecutiveFailures != 0 || st.Open {
		t.Fatalf("breaker = %+v, want closed with 0 counted failures", st)
	}
}

// TestConcurrency429NoBackoffClimb: a lone concurrency-limit target
// is retried after exactly its 1 s cool — not after the 200 ms
// exponential-backoff base, and not after a doubled 400 ms. With
// MaxAttempts 2 the request makes exactly 2 calls ~1 s apart.
func TestConcurrency429NoBackoffClimb(t *testing.T) {
	ctx := context.Background()
	conc := &provider.RateLimitError{Body: "concurrency limit reached"}
	eng := NewEngine(map[string]provider.Client{"a": errClient(conc)}, nil, Config{HedgeDelay: -1, MaxAttempts: 2})
	start := time.Now()
	_, _, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("lone concurrency 429: want an error, got nil")
	}
	if got := engLastClientCalls(t, eng, "a"); got != 2 {
		t.Fatalf("provider called %d times, want 2 (initial + one 1s-cool retry)", got)
	}
	if elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("elapsed = %v, want ~1s (the 1s concurrency cool, not 200ms backoff)", elapsed)
	}
}

// engLastClientCalls counts how many Chat calls the named fake client
// served. The test engines above build their clients inline, so the
// count is read back through the engine's client map — which the
// Engine does not expose. Instead the caller passes the fake in;
// this helper exists only to keep the assertions readable and will
// fail the test if the client is not a *fakeClient.
func engLastClientCalls(t *testing.T, eng *Engine, id string) int {
	t.Helper()
	eng.clientsMu.RLock()
	c, ok := eng.clients[id]
	eng.clientsMu.RUnlock()
	if !ok {
		t.Fatalf("engine has no client %q", id)
	}
	fc, ok := c.(*fakeClient)
	if !ok {
		t.Fatalf("client %q is %T, want *fakeClient", id, c)
	}
	return fc.chatCalls()
}
