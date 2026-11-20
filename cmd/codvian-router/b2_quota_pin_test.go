// Package main — B2 quota pin tests (PARITY rows :495/:497).
//
// These tests pin openStack's learned-quota overlay (applyLearnedQuota,
// cmd/codvian-router/main.go) at the strategy hook: the engine's measured
// quota hint (Engine.LearnedQuotaHint) overlaid onto QuotaRemainingPct +
// QuotaHint + ExpiresAt on BOTH signalsFn return paths. The overlay lives
// in a closure inside openStack, so the only way to reach it from a test
// is through the real stack: build one with openStack, teach its engine,
// and read the serving order off the route trace of a real
// ChatWithChainStrategy call.
//
// The mock upstreams always answer 200 with a minimal valid chat
// completion, so exactly one attempt runs per probe and the trace's
// first entry IS the hook's top-ranked provider — no failover, no
// breaker, no hedge involved. The pin is the ORDER CHANGE with the
// hint: with the overlay off, declared order stands.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// quotaPinUpstream is a fake OpenAI-compatible upstream for one test
// provider. It always answers 200 with a minimal valid chat completion.
// When rateLimit is set, the response also carries rate-limit headers
// teaching a 50%-remaining lesson with a 15-minute reset — the real
// upstream signal learnRateLimit records as a learned hint WITH an
// expiry (see TestB2QuotaPinExpiryFirstLearnedDeadline).
type quotaPinUpstream struct {
	rateLimit atomic.Bool
}

func (u *quotaPinUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u.rateLimit.Load() {
		w.Header().Set("x-ratelimit-limit-requests", "100")
		w.Header().Set("x-ratelimit-remaining-requests", "50")
		w.Header().Set("x-ratelimit-reset-requests", "15m")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
}

// newQuotaPinStack builds a real stack (openStack) over two no-auth
// providers — prov-full and prov-half, both serving model "m" from
// httptest upstreams — in a throwaway temp data dir. The returned
// stack's engine is the production engine with the production
// signalsFn and StrategyHook; its mock half upstream is returned so
// the caller can flip its rate-limit headers on for the learning step.
func newQuotaPinStack(t *testing.T) (*stack, *quotaPinUpstream) {
	t.Helper()
	half := &quotaPinUpstream{}
	fullSrv := httptest.NewServer(&quotaPinUpstream{})
	halfSrv := httptest.NewServer(half)
	t.Cleanup(func() { fullSrv.Close(); halfSrv.Close() })

	dir := t.TempDir()
	cfgJSON, err := json.Marshal(map[string]any{
		"version":  1,
		"listen":   "127.0.0.1:20128",
		"data_dir": filepath.Join(dir, "data"),
		"providers": []map[string]any{
			// quota_hint -1 (unknown): no quota data is configured,
			// so the engine treats both providers as ample and the
			// snapshots read them as full via the QuotaHint
			// fallback — the "no learned hint" baseline is a tie
			// at full, and declared order stands. (A configured
			// quota_hint of 0 would mean "spent" and the engine's
			// zero-quota filter would drop the whole chain.)
			{"id": "prov-full", "name": "prov-full", "base_url": fullSrv.URL, "wire_format": "openai", "enabled": true, "quota_hint": -1, "models": []map[string]any{{"id": "m"}}},
			{"id": "prov-half", "name": "prov-half", "base_url": halfSrv.URL, "wire_format": "openai", "enabled": true, "quota_hint": -1, "models": []map[string]any{{"id": "m"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, cfgJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := openStack(options{configPath: cfgPath})
	if err != nil {
		t.Fatalf("openStack: %v", err)
	}
	// Close the stack's SQLite stores before the test ends: on
	// Windows an open DB file blocks t.TempDir cleanup and fails
	// the test (the serve paths defer these same closes). The
	// reasoning cache is persistent whenever DataDir is set, so
	// it needs the shutdown sync+close too.
	t.Cleanup(func() {
		st.syncCachesOnShutdown(context.Background())
		if st.asyncLed != nil {
			st.asyncLed.Close()
		}
		if st.led != nil {
			_ = st.led.Close()
		}
		if st.feedback != nil {
			_ = st.feedback.Close()
		}
	})
	return st, half
}

// firstServed runs one chat request down the declared chain
// [prov-full, prov-half] under the named strategy and returns the
// provider that served it — the hook's top-ranked attempt. The mock
// upstreams always answer 200, so exactly one attempt runs.
func firstServed(t *testing.T, st *stack, strategy string) string {
	t.Helper()
	chain := []router.Attempt{{ProviderID: "prov-full", Model: "m"}, {ProviderID: "prov-half", Model: "m"}}
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	_, trace, err := st.engine.ChatWithChainStrategy(context.Background(), chain, req, strategy)
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(%s): %v", strategy, err)
	}
	if trace == nil || len(trace.Attempts) == 0 {
		t.Fatalf("ChatWithChainStrategy(%s): no attempts recorded on the trace", strategy)
	}
	return trace.Attempts[0].Provider
}

// TestB2QuotaPinResetAwareOrdering pins row :495 (reset-aware): the
// reset-aware order CHANGES with the engine's learned quota hint.
//
// No hint taught, both providers read as full (the QuotaHint
// fallback for unknown quota) and score identically,
// so declared order stands. Teach prov-half a measured 50% fraction
// and reset-aware hoists it ahead of the untaught provider — the
// engine's quota-hint pre-sort would keep prov-full (ample) first,
// so only the overlay's measured fraction can produce this order.
// Teach both (half 50%, full 100%) and half STILL ranks first:
// with no known reset, reset-aware's reset pressure (urgency 0.5 on
// an unknown reset) rewards the half-spent window over the full one
// (0.33 vs 0.32). The pin is the change with the hint, not a
// "half below full" direction — that direction belongs to
// expiry-first (see TestB2QuotaPinExpiryFirstLearnedDeadline).
// With the overlay off, all three probes keep declared order and
// this test FAILS.
func TestB2QuotaPinResetAwareOrdering(t *testing.T) {
	st, _ := newQuotaPinStack(t)

	if got := firstServed(t, st, "reset-aware"); got != "prov-full" {
		t.Fatalf("reset-aware, no hint: first = %q, want prov-full (declared order)", got)
	}
	st.engine.SetQuotaHint("prov-half", 50)
	if got := firstServed(t, st, "reset-aware"); got != "prov-half" {
		t.Fatalf("reset-aware, half=50: first = %q, want prov-half (measured fraction re-ranks it)", got)
	}
	st.engine.SetQuotaHint("prov-full", 100)
	if got := firstServed(t, st, "reset-aware"); got != "prov-half" {
		t.Fatalf("reset-aware, half=50 full=100: first = %q, want prov-half (reset pressure)", got)
	}
}

// TestB2QuotaPinExpiryFirstLearnedDeadline pins row :497
// (expiry-first): the deadline the expiry-first strategy spends
// against is the LEARNED reset instant from the overlay's ExpiresAt.
//
// Operator hints alone carry no expiry (SetQuotaHint never expires),
// so with only operator hints expiry-first spends the usable
// fraction alone and full (100%) serves before half (50%). Then
// prov-half serves one real response carrying rate-limit headers
// (50% remaining, reset in 15m): learnRateLimit records a learned
// hint WITH an expiry, the overlay sets ExpiresAt to it, and
// expiry-first flips — half's usable 0.5 against a near deadline
// scores 2.0, above full's 1.0. The engine's quota-hint pre-sort
// still says full first, so only the overlay's ExpiresAt can
// produce this flip. With the overlay off, the final probe keeps
// prov-full first and this test FAILS.
func TestB2QuotaPinExpiryFirstLearnedDeadline(t *testing.T) {
	st, half := newQuotaPinStack(t)

	st.engine.SetQuotaHint("prov-full", 100)
	st.engine.SetQuotaHint("prov-half", 50)
	if got := firstServed(t, st, "expiry-first"); got != "prov-full" {
		t.Fatalf("expiry-first, operator hints only: first = %q, want prov-full (usable fraction alone)", got)
	}

	// Teach prov-half a real upstream lesson: one served response
	// carrying rate-limit headers. The mock's headers say 50 of 100
	// requests remain with the window resetting in 15 minutes.
	half.rateLimit.Store(true)
	learnReq := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	if _, _, err := st.engine.ChatWithChainStrategy(context.Background(), []router.Attempt{{ProviderID: "prov-half", Model: "m"}}, learnReq, "priority"); err != nil {
		t.Fatalf("learning call: %v", err)
	}
	hint, expiresAt, ok := st.engine.LearnedQuotaHint("prov-half")
	if !ok || hint != 50 || expiresAt.IsZero() || time.Until(expiresAt) <= 0 {
		t.Fatalf("LearnedQuotaHint(prov-half) = (%d, %v, %v), want (50, future, true)", hint, expiresAt, ok)
	}

	if got := firstServed(t, st, "expiry-first"); got != "prov-half" {
		t.Fatalf("expiry-first, learned deadline: first = %q, want prov-half (learned reset instant is the deadline)", got)
	}
}
