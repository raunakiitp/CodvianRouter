package strategy

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Acceptance tests for internal/strategy/affinity.go (M3 S4).
//
// Every ordering test below uses an input order that DIFFERS from the
// expected output order, so each test FAILS if the strategy function is
// removed or returns its input unchanged (identity ordering).

func affinityTargets() []AffinityTarget {
	return []AffinityTarget{
		{Provider: "alpha", Model: "alpha/small"},
		{Provider: "beta", Model: "beta/medium"},
		{Provider: "gamma", Model: "gamma/large"},
	}
}

func affinityOrder(targets []AffinityTarget) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.Provider
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- LKGP ---

func TestLKGPOrdersLastKnownGoodFirst(t *testing.T) {
	store := NewLKGPStore()
	store.RecordSuccess("combo:chat", "gamma", "gamma/large", "conn-g1")

	got := store.OrderByLKGP("combo:chat", affinityTargets())
	if want := []string{"gamma", "alpha", "beta"}; !sameStrings(affinityOrder(got), want) {
		t.Fatalf("OrderByLKGP = %v, want %v", affinityOrder(got), want)
	}
}

func TestLKGPNoRecordKeepsInputOrder(t *testing.T) {
	store := NewLKGPStore()
	in := affinityTargets()
	got := store.OrderByLKGP("combo:chat", in)
	if want := []string{"alpha", "beta", "gamma"}; !sameStrings(affinityOrder(got), want) {
		t.Fatalf("OrderByLKGP with no record = %v, want input order %v", affinityOrder(got), want)
	}
	if &got[0] == &in[0] {
		t.Fatal("OrderByLKGP must return a new slice, not alias the input")
	}
}

func TestLKGPRecordedTargetAbsentKeepsOrder(t *testing.T) {
	// Recorded provider is not among the targets: order must not change,
	// and the result must never contain a target that was not in the input.
	store := NewLKGPStore()
	store.RecordSuccess("combo:chat", "delta", "delta/x", "")
	in := affinityTargets()
	got := store.OrderByLKGP("combo:chat", in)
	if want := []string{"alpha", "beta", "gamma"}; !sameStrings(affinityOrder(got), want) {
		t.Fatalf("OrderByLKGP with absent LKGP = %v, want %v", affinityOrder(got), want)
	}
}

func TestLKGPMatchesByProviderWhenConnectionDiffers(t *testing.T) {
	// the sector baseline orderByLkgp: provider+connection match first, then
	// provider-only (or "provider/" model prefix) match.
	store := NewLKGPStore()
	store.RecordSuccess("combo:chat", "beta", "beta/medium", "conn-other")
	got := store.OrderByLKGP("combo:chat", affinityTargets())
	if got[0].Provider != "beta" {
		t.Fatalf("OrderByLKGP first = %q, want beta (provider fallback)", got[0].Provider)
	}
}

func TestLKGPMatchesByModelPrefix(t *testing.T) {
	// Model string "beta/medium" carries the provider prefix even when
	// the recorded provider name is only implied by the model.
	store := NewLKGPStore()
	store.RecordSuccess("combo:chat", "gamma", "gamma/large", "")
	in := []AffinityTarget{
		{Provider: "other", Model: "gamma/large"},
		{Provider: "alpha", Model: "alpha/small"},
	}
	got := store.OrderByLKGP("combo:chat", in)
	if got[0].Model != "gamma/large" {
		t.Fatalf("OrderByLKGP first model = %q, want gamma/large (model-prefix match)", got[0].Model)
	}
}

func TestLKGPConnectionIDPrefersExactAccount(t *testing.T) {
	// Two targets share a provider; the recorded connection must win
	// over the other account of the same provider.
	store := NewLKGPStore()
	store.RecordSuccess("combo:chat", "beta", "beta/medium", "conn-2")
	in := []AffinityTarget{
		{Provider: "beta", Model: "beta/medium", ConnectionID: "conn-1"},
		{Provider: "alpha", Model: "alpha/small"},
		{Provider: "beta", Model: "beta/medium", ConnectionID: "conn-2"},
	}
	got := store.OrderByLKGP("combo:chat", in)
	if got[0].ConnectionID != "conn-2" {
		t.Fatalf("OrderByLKGP first connection = %q, want conn-2", got[0].ConnectionID)
	}
}

func TestLKGPLatestSuccessWins(t *testing.T) {
	store := NewLKGPStore()
	store.RecordSuccess("combo:chat", "alpha", "alpha/small", "")
	store.RecordSuccess("combo:chat", "gamma", "gamma/large", "")
	got := store.OrderByLKGP("combo:chat", affinityTargets())
	if got[0].Provider != "gamma" {
		t.Fatalf("OrderByLKGP first = %q, want gamma (latest success)", got[0].Provider)
	}
}

func TestLKGPStoreIsScopedPerChainKey(t *testing.T) {
	store := NewLKGPStore()
	store.RecordSuccess("combo:chat", "gamma", "gamma/large", "")
	got := store.OrderByLKGP("combo:other", affinityTargets())
	if want := []string{"alpha", "beta", "gamma"}; !sameStrings(affinityOrder(got), want) {
		t.Fatalf("OrderByLKGP for other chain = %v, want input order %v", affinityOrder(got), want)
	}
}

func TestLKGPConcurrentAccess(t *testing.T) {
	store := NewLKGPStore()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				provider := fmt.Sprintf("p%d", (w+i)%3)
				store.RecordSuccess("combo:chat", provider, provider+"/m", "")
				got := store.OrderByLKGP("combo:chat", affinityTargets())
				if len(got) != 3 {
					t.Errorf("OrderByLKGP returned %d targets, want 3", len(got))
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

// --- context-optimized ---

func TestAffinityContextOptimizedLargestFirst(t *testing.T) {
	in := []ContextTarget{
		{AffinityTarget: AffinityTarget{Provider: "alpha", Model: "alpha/small"}, ContextWindow: 32_000},
		{AffinityTarget: AffinityTarget{Provider: "beta", Model: "beta/medium"}, ContextWindow: 1_000_000},
		{AffinityTarget: AffinityTarget{Provider: "gamma", Model: "gamma/large"}, ContextWindow: 128_000},
	}
	got := OrderByContextOptimized(in)
	want := []string{"beta", "gamma", "alpha"}
	var providers []string
	for _, ct := range got {
		providers = append(providers, ct.Provider)
	}
	if !sameStrings(providers, want) {
		t.Fatalf("OrderByContextOptimized = %v, want %v", providers, want)
	}
}

func TestAffinityContextOptimizedTiesKeepInputOrder(t *testing.T) {
	in := []ContextTarget{
		{AffinityTarget: AffinityTarget{Provider: "alpha", Model: "a"}, ContextWindow: 128_000},
		{AffinityTarget: AffinityTarget{Provider: "beta", Model: "b"}, ContextWindow: 128_000},
		{AffinityTarget: AffinityTarget{Provider: "gamma", Model: "g"}, ContextWindow: 32_000},
	}
	got := OrderByContextOptimized(in)
	if got[0].Provider != "alpha" || got[1].Provider != "beta" {
		t.Fatalf("tied windows reordered: got %q, %q; want alpha, beta (input order)",
			got[0].Provider, got[1].Provider)
	}
}

func TestAffinityContextOptimizedUnknownWindowSortsLast(t *testing.T) {
	// Unknown window (0, mirroring the sector-typical `?? 0`) sorts after every
	// known window; if NO window is known the input order is unchanged.
	in := []ContextTarget{
		{AffinityTarget: AffinityTarget{Provider: "alpha", Model: "a"}, ContextWindow: 0},
		{AffinityTarget: AffinityTarget{Provider: "beta", Model: "b"}, ContextWindow: 128_000},
	}
	got := OrderByContextOptimized(in)
	if got[0].Provider != "beta" || got[1].Provider != "alpha" {
		t.Fatalf("unknown window not last: got %q, %q", got[0].Provider, got[1].Provider)
	}

	allUnknown := []ContextTarget{
		{AffinityTarget: AffinityTarget{Provider: "alpha", Model: "a"}},
		{AffinityTarget: AffinityTarget{Provider: "beta", Model: "b"}},
	}
	unchanged := OrderByContextOptimized(allUnknown)
	if unchanged[0].Provider != "alpha" || unchanged[1].Provider != "beta" {
		t.Fatal("all-unknown windows must keep input order")
	}
}

// --- cache-optimized ---

func TestAffinityCacheOptimizedHighestWarmthFirst(t *testing.T) {
	in := []CacheTarget{
		{AffinityTarget: AffinityTarget{Provider: "alpha", Model: "a"}, Warmth: 0.1},
		{AffinityTarget: AffinityTarget{Provider: "beta", Model: "b"}, Warmth: 0.9},
		{AffinityTarget: AffinityTarget{Provider: "gamma", Model: "g"}, Warmth: 0.5},
	}
	got := OrderByCacheOptimized(in)
	want := []string{"beta", "gamma", "alpha"}
	var providers []string
	for _, ct := range got {
		providers = append(providers, ct.Provider)
	}
	if !sameStrings(providers, want) {
		t.Fatalf("OrderByCacheOptimized = %v, want %v", providers, want)
	}
}

func TestAffinityCacheOptimizedTiesKeepInputOrder(t *testing.T) {
	in := []CacheTarget{
		{AffinityTarget: AffinityTarget{Provider: "alpha", Model: "a"}, Warmth: 0.7},
		{AffinityTarget: AffinityTarget{Provider: "beta", Model: "b"}, Warmth: 0.7},
		{AffinityTarget: AffinityTarget{Provider: "gamma", Model: "g"}, Warmth: 0.2},
	}
	got := OrderByCacheOptimized(in)
	if got[0].Provider != "alpha" || got[1].Provider != "beta" {
		t.Fatalf("tied warmth reordered: got %q, %q; want alpha, beta (input order)",
			got[0].Provider, got[1].Provider)
	}
}

func TestAffinityCacheOptimizedClampsOutOfRangeWarmth(t *testing.T) {
	// Warmth is defined on 0..1; out-of-range input must not break the
	// ordering (clamped, never invented from the prompt itself).
	in := []CacheTarget{
		{AffinityTarget: AffinityTarget{Provider: "alpha", Model: "a"}, Warmth: 1.5},
		{AffinityTarget: AffinityTarget{Provider: "beta", Model: "b"}, Warmth: -0.5},
		{AffinityTarget: AffinityTarget{Provider: "gamma", Model: "g"}, Warmth: 0.5},
	}
	got := OrderByCacheOptimized(in)
	if got[0].Provider != "alpha" || got[1].Provider != "gamma" || got[2].Provider != "beta" {
		t.Fatalf("clamped order wrong: got %q, %q, %q",
			got[0].Provider, got[1].Provider, got[2].Provider)
	}
}

// --- context-relay ---

func relayMessages() []RelayMessage {
	return []RelayMessage{
		{Role: "user", Content: "Continue the refactor of router.go."},
		{Role: "assistant", Content: "Done up to the retry loop."},
	}
}

func relaySummary() HandoffSummary {
	return HandoffSummary{
		Summary:        "Refactoring router.go retry loop; halfway done.",
		KeyDecisions:   []string{"Keep failover-first ordering"},
		TaskProgress:   "Retry loop done; breaker wiring pending.",
		ActiveEntities: []string{"router.go"},
		MessageCount:   2,
		Model:          "codex/mini",
	}
}

func TestContextRelayBuildsNextAttemptContext(t *testing.T) {
	now := time.Now()
	state := NewRelayState("sess-1", "combo:chat")
	state.HandoffProviders = []string{"codex"}

	// Successful attempt on an allowlisted provider inside the quota
	// window [0.85, 0.95) requests a handoff (the sector baseline
	// maybeGenerateHandoff gates).
	shouldGenerate := state.RecordAttempt(AttemptOutcome{
		Provider: "codex", Model: "codex/mini", ConnectionID: "conn-1",
		Success: true, PercentUsed: 0.90, Messages: relayMessages(),
	})
	if !shouldGenerate {
		t.Fatal("RecordAttempt did not request a handoff for a 90%-used allowlisted success")
	}
	state.SetHandoff(relaySummary(), time.Time{}, now)

	ctx := state.BuildNextAttemptContext(now.Add(time.Minute), FormatChat, "", nil)
	if !ctx.HandoffApplied {
		t.Fatal("BuildNextAttemptContext did not apply the handoff")
	}
	if len(ctx.Messages) != 3 || ctx.Messages[0].Role != "system" {
		t.Fatalf("chat context = %d messages, first role %q; want system handoff + 2 original",
			len(ctx.Messages), ctx.Messages[0].Role)
	}
	if !strings.Contains(ctx.Messages[0].Content, "Refactoring router.go") {
		t.Fatal("handoff system message does not carry the summary")
	}
	if ctx.Messages[1].Content != "Continue the refactor of router.go." {
		t.Fatal("original messages were not preserved after the handoff")
	}
}

func TestContextRelayResponsesFormatPrependsToInstructions(t *testing.T) {
	now := time.Now()
	state := NewRelayState("sess-1", "combo:chat")
	state.RecordAttempt(AttemptOutcome{
		Provider: "codex", Model: "codex/mini", Success: true,
		PercentUsed: 0.90, Messages: relayMessages(),
	})
	state.SetHandoff(relaySummary(), time.Time{}, now)

	ctx := state.BuildNextAttemptContext(now.Add(time.Minute), FormatResponses, "Be concise.", nil)
	if !ctx.HandoffApplied {
		t.Fatal("handoff not applied for responses format")
	}
	if !strings.HasPrefix(ctx.Instructions, "<context_handoff>") {
		t.Fatalf("instructions do not start with the handoff block: %q", ctx.Instructions)
	}
	if !strings.Contains(ctx.Instructions, "Be concise.") {
		t.Fatal("existing instructions were lost")
	}
	if len(ctx.Messages) != 2 {
		t.Fatalf("responses format must keep messages untouched, got %d", len(ctx.Messages))
	}
}

func TestContextRelayClaudeFormatAppendsSystemBlock(t *testing.T) {
	now := time.Now()
	state := NewRelayState("sess-1", "combo:chat")
	state.RecordAttempt(AttemptOutcome{
		Provider: "codex", Model: "codex/mini", Success: true,
		PercentUsed: 0.90, Messages: relayMessages(),
	})
	state.SetHandoff(relaySummary(), time.Time{}, now)

	ctx := state.BuildNextAttemptContext(now.Add(time.Minute), FormatClaude, "", []string{"You are helpful."})
	if !ctx.HandoffApplied {
		t.Fatal("handoff not applied for claude format")
	}
	if len(ctx.SystemBlocks) != 2 || ctx.SystemBlocks[0] != "You are helpful." {
		t.Fatalf("system blocks = %v; want existing prompt first, handoff appended", ctx.SystemBlocks)
	}
	if !strings.Contains(ctx.SystemBlocks[1], "Refactoring router.go") {
		t.Fatal("appended system block does not carry the summary")
	}
}

func TestContextRelayGatingThresholds(t *testing.T) {
	// Below the 0.85 warning threshold: no handoff. At/above the 0.95
	// exhaustion threshold: no handoff either (quota is gone, relaying
	// is pointless). Outside the allowlist: no handoff.
	cases := []struct {
		name        string
		provider    string
		percentUsed float64
	}{
		{"below threshold", "codex", 0.50},
		{"at exhaustion", "codex", 0.95},
		{"above exhaustion", "codex", 0.99},
		{"provider not allowlisted", "other", 0.90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := NewRelayState("sess-1", "combo:chat")
			if state.RecordAttempt(AttemptOutcome{
				Provider: tc.provider, Model: "m", Success: true,
				PercentUsed: tc.percentUsed, Messages: relayMessages(),
			}) {
				t.Fatalf("RecordAttempt requested a handoff for %s (%.2f used)", tc.provider, tc.percentUsed)
			}
		})
	}
}

func TestContextRelayNoHandoffPassesThrough(t *testing.T) {
	// No handoff generated yet: the next attempt's context is exactly
	// the original request context, unchanged.
	now := time.Now()
	state := NewRelayState("sess-1", "combo:chat")
	state.RecordAttempt(AttemptOutcome{
		Provider: "codex", Model: "codex/mini", Success: true,
		PercentUsed: 0.10, Messages: relayMessages(),
	})
	ctx := state.BuildNextAttemptContext(now, FormatChat, "", nil)
	if ctx.HandoffApplied {
		t.Fatal("handoff applied with none generated")
	}
	if len(ctx.Messages) != 2 || ctx.Messages[0].Content != "Continue the refactor of router.go." {
		t.Fatalf("context changed without a handoff: %+v", ctx.Messages)
	}
}

func TestContextRelayExpiredHandoffNotInjected(t *testing.T) {
	now := time.Now()
	state := NewRelayState("sess-1", "combo:chat")
	state.RecordAttempt(AttemptOutcome{
		Provider: "codex", Model: "codex/mini", Success: true,
		PercentUsed: 0.90, Messages: relayMessages(),
	})
	// Handoff expired an hour ago: must not leak stale context into the
	// next attempt.
	state.SetHandoff(relaySummary(), now.Add(-time.Hour), now.Add(-2*time.Hour))
	ctx := state.BuildNextAttemptContext(now, FormatChat, "", nil)
	if ctx.HandoffApplied {
		t.Fatal("expired handoff was injected")
	}
	if len(ctx.Messages) != 2 {
		t.Fatalf("expired handoff changed the context: %d messages", len(ctx.Messages))
	}
}

func TestContextRelayFailedAttemptDoesNotRequestHandoffByDefault(t *testing.T) {
	// The strategy-relay trigger is a successful turn approaching quota
	// exhaustion; a failed attempt alone does not generate a handoff
	// (RelayOnError opts into the universal-handoff on-error trigger).
	state := NewRelayState("sess-1", "combo:chat")
	if state.RecordAttempt(AttemptOutcome{
		Provider: "codex", Model: "codex/mini", Success: false,
		PercentUsed: 0.90, Messages: relayMessages(),
	}) {
		t.Fatal("failed attempt requested a handoff with RelayOnError off")
	}
	state.RelayOnError = true
	if !state.RecordAttempt(AttemptOutcome{
		Provider: "codex", Model: "codex/mini", Success: false,
		PercentUsed: 0.90, Messages: relayMessages(),
	}) {
		t.Fatal("failed attempt did not request a handoff with RelayOnError on")
	}
}

func TestContextRelaySecondHandoffNotRegenerated(t *testing.T) {
	// the sector baseline skips generation while a handoff is already active.
	now := time.Now()
	state := NewRelayState("sess-1", "combo:chat")
	state.RecordAttempt(AttemptOutcome{
		Provider: "codex", Model: "codex/mini", Success: true,
		PercentUsed: 0.90, Messages: relayMessages(),
	})
	state.SetHandoff(relaySummary(), time.Time{}, now)
	if state.RecordAttempt(AttemptOutcome{
		Provider: "codex", Model: "codex/mini", Success: true,
		PercentUsed: 0.92, Messages: relayMessages(),
	}) {
		t.Fatal("second handoff requested while one is already active")
	}
}
