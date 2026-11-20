package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Soft-demotion cooldown (research-log entry 183): a retryable
// failure demotes the target to the end of the order instead of
// skipping it. These tests use fake clients (no network) and the
// real Engine.

// --- duration math ---

func TestSoftCooldownDurationDoublesToFiveMinutes(t *testing.T) {
	// No Retry-After: 5 s base, doubling per consecutive failure.
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 300 * time.Second, 300 * time.Second}
	for i, w := range want {
		if got := softCooldownDuration(0, false, i+1); got != w {
			t.Errorf("softCooldownDuration(0, absent, %d) = %v, want %v", i+1, got, w)
		}
	}
	// Retry-After is the base, floored at 10s (CHECKLIST 168): a
	// present 7 s hint cools for 10 s, then 20, 40, ... capped at
	// 5 min — a poisoned or tiny hint can never cool for less.
	if got := softCooldownDuration(7*time.Second, true, 1); got != 10*time.Second {
		t.Errorf("softCooldownDuration(7s, present, 1) = %v, want 10s floor", got)
	}
	if got := softCooldownDuration(7*time.Second, true, 2); got != 20*time.Second {
		t.Errorf("softCooldownDuration(7s, present, 2) = %v, want 20s", got)
	}
	// The floor covers an explicit 0s hint too; 10s itself is
	// honoured as-is, and a 59s hint is not floored.
	if got := softCooldownDuration(0, true, 1); got != 10*time.Second {
		t.Errorf("softCooldownDuration(0s, present, 1) = %v, want 10s floor", got)
	}
	if got := softCooldownDuration(1*time.Second, true, 1); got != 10*time.Second {
		t.Errorf("softCooldownDuration(1s, present, 1) = %v, want 10s floor", got)
	}
	if got := softCooldownDuration(5*time.Second, true, 1); got != 10*time.Second {
		t.Errorf("softCooldownDuration(5s, present, 1) = %v, want 10s floor", got)
	}
	if got := softCooldownDuration(10*time.Second, true, 1); got != 10*time.Second {
		t.Errorf("softCooldownDuration(10s, present, 1) = %v, want 10s", got)
	}
	if got := softCooldownDuration(59*time.Second, true, 1); got != 59*time.Second {
		t.Errorf("softCooldownDuration(59s, present, 1) = %v, want 59s", got)
	}
	if got := softCooldownDuration(120*time.Second, true, 3); got != 300*time.Second {
		t.Errorf("softCooldownDuration(120s, present, 3) = %v, want 5m cap", got)
	}
	if got := softCooldownDuration(3600*time.Second, true, 1); got != 300*time.Second {
		t.Errorf("softCooldownDuration(3600s, present, 1) = %v, want 5m cap", got)
	}
}

func TestIsCooldownMode(t *testing.T) {
	for _, m := range []string{"", "hard", "soft-demote", "HARD", " Soft-Demote "} {
		if !IsCooldownMode(m) {
			t.Errorf("IsCooldownMode(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"soft", "demote", "skip", "off"} {
		if IsCooldownMode(m) {
			t.Errorf("IsCooldownMode(%q) = true, want false", m)
		}
	}
	if got := NormalizeCooldownMode(""); got != CooldownHard {
		t.Errorf("NormalizeCooldownMode(\"\") = %q, want hard", got)
	}
	if got := NormalizeCooldownMode("bogus"); got != CooldownHard {
		t.Errorf("NormalizeCooldownMode(\"bogus\") = %q, want hard", got)
	}
}

// --- helpers ---

func softEngine(t *testing.T, clients map[string]provider.Client) *Engine {
	t.Helper()
	// HedgeDelay -1 disables hedging so call order is exactly
	// the chain order (no parallel second attempt muddying it).
	return NewEngine(clients, nil, Config{HedgeDelay: -1, CooldownMode: CooldownSoftDemote})
}

func coolReq(model string) provider.ChatRequest {
	return provider.ChatRequest{Model: model, Messages: []provider.Message{{Role: RoleUser, Content: "hi"}}}
}

func chainFor(models ...string) []Attempt {
	chain := make([]Attempt, 0, len(models))
	for _, m := range models {
		chain = append(chain, Attempt{ProviderID: m, Model: "m"})
	}
	return chain
}

// TestSoftDemotionOrdering: after provider A fails retryably, the
// next request tries healthy B first and A last — A is demoted,
// never skipped.
func TestSoftDemotionOrdering(t *testing.T) {
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	a := errClient(fail500)
	b := okClient("bee")
	eng := softEngine(t, map[string]provider.Client{"a": a, "b": b})
	ctx := context.Background()
	chain := chainFor("a", "b")

	// Request 1: A fails, B serves.
	resp, _, err := eng.ChatWithChain(ctx, chain, coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("req1 = %v, %v, want bee + nil", resp, err)
	}
	if got := a.chatCalls(); got != 1 {
		t.Fatalf("req1: A called %d times, want 1", got)
	}
	// A must now be cooling (soft) — and the feed must say so.
	targets := eng.CooldownTargets()
	if len(targets) != 1 || targets[0].ProviderID != "a" || !targets[0].Demoted || targets[0].Mode != CooldownSoftDemote {
		t.Fatalf("CooldownTargets = %+v, want one demoted soft target for a", targets)
	}
	if targets[0].Failures != 1 || targets[0].RemainingSeconds < 1 || targets[0].RemainingSeconds > 5 {
		t.Fatalf("CooldownTargets[0] = %+v, want failures=1, remaining 1..5s", targets[0])
	}

	// Request 2: B fails too, so the order is observable — B must
	// be tried FIRST and A LAST (demoted), and A must still be
	// tried (not skipped). Rebuild the engine (state is
	// per-engine), re-fail A, then fail B as well.
	a2 := errClient(fail500)
	b2 := okClient("bee")
	eng2 := softEngine(t, map[string]provider.Client{"a": a2, "b": b2})
	if _, _, err := eng2.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
		t.Fatalf("eng2 req1: %v", err)
	}
	// Now make B fail as well, and record the call order.
	var order []string
	b2.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		order = append(order, "b")
		return nil, fail500
	}
	a2.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		order = append(order, "a")
		return nil, fail500
	}
	if _, _, err := eng2.ChatWithChain(ctx, chain, coolReq("m")); err == nil {
		t.Fatalf("eng2 req2: want an error (both targets failing), got nil")
	}
	if len(order) != 2 || order[0] != "b" || order[1] != "a" {
		t.Fatalf("req2 call order = %v, want [b a] (demoted A last)", order)
	}
}

// TestSoftDemotionNeverSkips: in soft mode a cooling target is
// still tried when it is the only target — the request fails with
// the upstream error, never with ErrNoAvailableAttempt. In hard
// mode the same sequence skips it.
func TestSoftDemotionNeverSkips(t *testing.T) {
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	ctx := context.Background()
	chain := chainFor("a")

	// Soft mode: the second request must call A again (call count
	// 2), proving a cooling target is tried, not skipped — and
	// the error is the upstream error, never ErrNoAvailableAttempt.
	count := 0
	a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		count++
		return nil, fail500
	}}
	soft := softEngine(t, map[string]provider.Client{"a": a})
	_, _, _ = soft.ChatWithChain(ctx, chain, coolReq("m"))
	_, _, err := soft.ChatWithChain(ctx, chain, coolReq("m"))
	if err == nil {
		t.Fatalf("soft req2: want error, got nil")
	}
	if errors.Is(err, ErrNoAvailableAttempt) {
		t.Fatalf("soft req2: got ErrNoAvailableAttempt — a demoted target must still be tried")
	}
	if count != 2 {
		t.Fatalf("soft: A called %d times, want 2 (tried on both requests)", count)
	}

	// Hard mode contrast: after a 429 the cooling provider is
	// skipped on the next request (a healthy sibling serves
	// instead), so it is never called again while cooling. Two
	// providers: A 429s, B serves; on the next request A must
	// still be skipped (call count stays 1).
	rl := &provider.RateLimitError{RetryAfter: 30 * time.Second, RetryAfterPresent: true, Body: "slow down"}
	hardCount := 0
	ha := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		hardCount++
		return nil, rl
	}}
	hard := NewEngine(map[string]provider.Client{"a": ha, "b": okClient("bee")}, nil, Config{HedgeDelay: -1})
	hardChain := chainFor("a", "b")
	resp, _, err := hard.ChatWithChain(ctx, hardChain, coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("hard req1 = %v, %v, want bee + nil", resp, err)
	}
	resp, _, err = hard.ChatWithChain(ctx, hardChain, coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("hard req2 = %v, %v, want bee + nil", resp, err)
	}
	if hardCount != 1 {
		t.Fatalf("hard: A called %d times, want 1 (skipped while cooling)", hardCount)
	}
}

// TestSoftDemotionSuccessClears: a success on the cooling target
// ends its cooldown (feed goes empty, failure count resets).
func TestSoftDemotionSuccessClears(t *testing.T) {
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	ctx := context.Background()
	chain := chainFor("a", "b")
	a := errClient(fail500)
	b := okClient("bee")
	eng := softEngine(t, map[string]provider.Client{"a": a, "b": b})
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
		t.Fatalf("req1: %v", err)
	}
	if got := len(eng.CooldownTargets()); got != 1 {
		t.Fatalf("cooling targets after failure = %d, want 1", got)
	}
	// A recovers: next request where A is tried first (only A in
	// the chain) succeeds, and the cooldown clears.
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "a-ok"}, nil
	}
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != nil || resp.Content != "a-ok" {
		t.Fatalf("recovery req = %v, %v, want a-ok + nil", resp, err)
	}
	if got := eng.CooldownTargets(); len(got) != 0 {
		t.Fatalf("CooldownTargets after success = %+v, want empty", got)
	}
}

// TestSoftDemotionRetryAfterHonored: a 429 with Retry-After cools
// the target for (about) that many seconds, floored at 10s
// (CHECKLIST 168 — a present 7s hint cools for 10s); without it, 5 s.
func TestSoftDemotionRetryAfterHonored(t *testing.T) {
	ctx := context.Background()
	chain := chainFor("a", "b")

	rl := &provider.RateLimitError{RetryAfter: 7 * time.Second, RetryAfterPresent: true, Body: "slow down"}
	eng := softEngine(t, map[string]provider.Client{"a": errClient(rl), "b": okClient("bee")})
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
		t.Fatalf("req1: %v", err)
	}
	targets := eng.CooldownTargets()
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want 1", targets)
	}
	if targets[0].RemainingSeconds < 9 || targets[0].RemainingSeconds > 10 {
		t.Fatalf("Retry-After 7s (floored to 10s): remaining = %ds, want 9..10", targets[0].RemainingSeconds)
	}

	// No Retry-After: 5 s base.
	rl0 := &provider.RateLimitError{Body: "slow down"}
	eng2 := softEngine(t, map[string]provider.Client{"a": errClient(rl0), "b": okClient("bee")})
	if _, _, err := eng2.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
		t.Fatalf("req1 (no RA): %v", err)
	}
	targets = eng2.CooldownTargets()
	if len(targets) != 1 || targets[0].RemainingSeconds < 1 || targets[0].RemainingSeconds > 5 {
		t.Fatalf("no Retry-After: targets = %+v, want remaining 1..5s", targets)
	}
}

// TestSoftDemotionPerChainMode: soft for one chain, hard for
// another — the mode resolves per model, not just globally.
func TestSoftDemotionPerChainMode(t *testing.T) {
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	ctx := context.Background()
	eng := NewEngine(map[string]provider.Client{
		"a": errClient(fail500),
		"b": okClient("bee"),
	}, nil, Config{
		HedgeDelay: -1,
		ChainCooldownMode: func(model string) string {
			if model == "soft-model" {
				return CooldownSoftDemote
			}
			return CooldownHard
		},
	})
	// Soft chain: A fails, B serves; A cools soft.
	if _, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("soft-model")); err != nil {
		t.Fatalf("soft chain req: %v", err)
	}
	targets := eng.CooldownTargets()
	if len(targets) != 1 || targets[0].Mode != CooldownSoftDemote {
		t.Fatalf("soft chain targets = %+v, want one soft-demote", targets)
	}
	// Hard chain with a 429: A cools hard and is skipped.
	rl := &provider.RateLimitError{RetryAfter: 30 * time.Second, RetryAfterPresent: true, Body: "slow"}
	engHard := NewEngine(map[string]provider.Client{
		"a": errClient(rl),
		"b": okClient("bee"),
	}, nil, Config{HedgeDelay: -1})
	if _, _, err := engHard.ChatWithChain(ctx, chainFor("a", "b"), coolReq("hard-model")); err != nil {
		t.Fatalf("hard chain req: %v", err)
	}
	targets = engHard.CooldownTargets()
	if len(targets) != 1 || targets[0].Mode != CooldownHard || targets[0].Demoted {
		t.Fatalf("hard chain targets = %+v, want one hard (not demoted)", targets)
	}
}

// TestSoftDemotionTaxonomyStartsCooldown: 402/404 retryable
// failures (per the shared taxonomy) start a soft cooldown; a 400
// does not.
func TestSoftDemotionTaxonomyStartsCooldown(t *testing.T) {
	ctx := context.Background()
	chain := chainFor("a", "b")
	for _, status := range []int{402, 404} {
		errUp := &provider.UpstreamError{StatusCode: status, Retryable: true, Body: "target problem"}
		eng := softEngine(t, map[string]provider.Client{"a": errClient(errUp), "b": okClient("bee")})
		if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
			t.Fatalf("status %d req: %v", status, err)
		}
		if got := len(eng.CooldownTargets()); got != 1 {
			t.Fatalf("status %d: cooling targets = %d, want 1", status, got)
		}
	}
	// A 402 arriving as a QuotaError (the OpenAI client's mapping)
	// also demotes.
	qe := &provider.QuotaError{Message: "pay up", StatusCode: 402}
	eng := softEngine(t, map[string]provider.Client{"a": errClient(qe), "b": okClient("bee")})
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
		t.Fatalf("quota 402 req: %v", err)
	}
	if got := len(eng.CooldownTargets()); got != 1 {
		t.Fatalf("quota 402: cooling targets = %d, want 1", got)
	}
	// A 400 (request property) must NOT start a soft cooldown —
	// and it stops the chain (entry 183: 400/403/422 stop), so
	// the fallback target is never dialled for it.
	bad := &provider.UpstreamError{StatusCode: 400, Retryable: false, Body: "bad request"}
	bCalls := 0
	bCount := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		bCalls++
		return &provider.ChatResponse{Content: "bee"}, nil
	}}
	eng400 := softEngine(t, map[string]provider.Client{"a": errClient(bad), "b": bCount})
	if _, _, err := eng400.ChatWithChain(ctx, chain, coolReq("m")); err == nil {
		t.Fatalf("400 req: want the 400 error, got nil")
	}
	if bCalls != 0 {
		t.Fatalf("400 req: fallback called %d times, want 0 (chain stops on a terminal error)", bCalls)
	}
	if got := eng400.CooldownTargets(); len(got) != 0 {
		t.Fatalf("400: cooling targets = %+v, want empty", got)
	}
}

// TestSoftDemotionRuntimeSetters: the dashboard setters change the
// mode live, per chain, without a restart.
func TestSoftDemotionRuntimeSetters(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": okClient("x")}, nil, Config{HedgeDelay: -1})
	if got := eng.cooldownModeFor("m"); got != CooldownHard {
		t.Fatalf("default mode = %q, want hard", got)
	}
	eng.SetCooldownMode("soft-demote")
	if got := eng.cooldownModeFor("m"); got != CooldownSoftDemote {
		t.Fatalf("after SetCooldownMode: %q, want soft-demote", got)
	}
	eng.SetChainCooldownMode("m", "hard")
	if got := eng.cooldownModeFor("m"); got != CooldownHard {
		t.Fatalf("per-chain override: %q, want hard", got)
	}
	eng.SetChainCooldownMode("m", "")
	if got := eng.cooldownModeFor("m"); got != CooldownSoftDemote {
		t.Fatalf("after clearing chain override: %q, want soft-demote", got)
	}
	eng.SetCooldownMode("")
	if got := eng.cooldownModeFor("m"); got != CooldownHard {
		t.Fatalf("after clearing global: %q, want hard", got)
	}
	g, chains := eng.CooldownState()
	if g != "" || len(chains) != 0 {
		t.Fatalf("CooldownState = (%q, %v), want (\"\", empty) after clearing", g, chains)
	}
}

// TestSoftDemotionStreaming: a retryable stream failure demotes
// the target too (same rule, streaming path).
func TestSoftDemotionStreaming(t *testing.T) {
	ctx := context.Background()
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	eng2 := softEngine(t, map[string]provider.Client{"a": streamClient(nil, fail500), "b": streamClient([]string{"bee"}, nil)})
	ch, err := eng2.ChatStreamWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream err: %v", ev.Err)
		}
		got += ev.Delta
	}
	if got != "bee" {
		t.Fatalf("stream content = %q, want bee", got)
	}
	targets := eng2.CooldownTargets()
	if len(targets) != 1 || targets[0].ProviderID != "a" || !targets[0].Demoted {
		t.Fatalf("streaming targets = %+v, want one demoted target for a", targets)
	}
}
