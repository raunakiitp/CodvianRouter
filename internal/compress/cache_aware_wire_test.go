package compress

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Cache-aware PrefixFreeze wiring (PARITY.md Area 10 row :939,
// BURNDOWN.md Tier 1 row 8). Each test is a TOGGLE proof: the
// asserted behaviour must change when the wire it proves is
// removed — a test that passes with the wire deleted is marked
// as such and does not count as wiring evidence (none here are).
//
// The PrefixFreeze observer lives in internal/degrade (it cannot
// move here: degrade imports compress, so the reverse import
// would close a cycle). Apply therefore takes the freeze decision
// as a callback (ApplyOptions.FrozenPrefix, supplied in production
// by degrade.PrefixFreeze.FreezeChecker). These tests prove the
// Apply side of that seam with stub callbacks: a frozen prefix
// freezes the system prompt for a caching provider even under
// mode "never", and never for a non-caching provider.

// TestCacheAwareFrozenPrefix proves the frozen-prefix freeze: a
// caching provider with a frozen (recurring, cacheable) prefix
// takes the cache-aware path — the system prompt comes back
// byte-identical and the request honestly reports not fitting —
// even when the preserve mode is "never". Every control must
// truncate instead: without a frozen prefix, without an observer,
// and for a non-caching provider (no cache to protect, so the
// freeze must not fire there). Any case fails if the FrozenPrefix
// wire is removed: with it deleted, the caching+frozen case
// truncates like the rest.
func TestCacheAwareFrozenPrefix(t *testing.T) {
	frozen := func([]provider.Message) bool { return true }
	notFrozen := func([]provider.Message) bool { return false }
	cases := []struct {
		name     string
		preserve string
		opts     ApplyOptions
		frozen   bool
	}{
		{"caching + frozen prefix freezes under never", "never", ApplyOptions{IsCachingProvider: true, FrozenPrefix: frozen}, true},
		{"caching + frozen prefix freezes under whenNoCache", "whenNoCache", ApplyOptions{IsCachingProvider: true, FrozenPrefix: frozen}, true},
		{"caching + not-frozen prefix truncates under never", "never", ApplyOptions{IsCachingProvider: true, FrozenPrefix: notFrozen}, false},
		{"caching + frozen prefix without observer truncates under never", "never", ApplyOptions{IsCachingProvider: true}, false},
		{"non-caching + frozen prefix truncates under never", "never", ApplyOptions{FrozenPrefix: frozen}, false},
		{"cache_control + frozen prefix freezes under never", "never", ApplyOptions{HasCacheControl: true, FrozenPrefix: frozen}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs, _ := g22FreezeFixture()
			// The frozen expectation is the LOSSLESS normal
			// form, not the raw input: the whitespace step
			// legitimately strips the fixture's trailing
			// space, and "frozen" means lossy left it alone
			// after that.
			wantSys := findRole(Normalise(msgs), "system").Content
			budget := g22FreezeBudget(t, msgs)
			out, res := ApplyWithOptions("gpt-4o", msgs, g22LossyReq(tc.preserve, budget), nil, nil, tc.opts)
			sys := findRole(out, "system")
			if sys == nil {
				t.Fatalf("no system message in output: %+v", out)
			}
			if tc.frozen {
				if sys.Content != wantSys {
					t.Fatalf("frozen system prompt must be byte-identical to its normal form; got truncated to %d chars, want %d", len(sys.Content), len(wantSys))
				}
				if res.AfterTokens <= budget {
					t.Fatalf("frozen request must honestly miss the budget: AfterTokens=%d budget=%d res=%+v", res.AfterTokens, budget, res)
				}
			} else {
				if sys.Content == wantSys {
					t.Fatalf("unfrozen system prompt must be truncated to fit, but came back intact")
				}
				if !strings.Contains(sys.Content, truncationMarker) {
					t.Fatalf("unfrozen system truncation must carry the marker, got %q", sys.Content)
				}
				if res.AfterTokens > budget {
					t.Fatalf("unfrozen request must fit the budget: AfterTokens=%d budget=%d res=%+v", res.AfterTokens, budget, res)
				}
			}
			if !res.LossyApplied {
				t.Fatalf("lossy must have applied, got %+v", res)
			}
		})
	}
}

// TestCacheAwareFrozenPrefixDowngrades proves the frozen-prefix
// freeze rides the same cache-aware path as the mode downgrade:
// aggressive for a caching provider with a frozen prefix runs
// as standard AND leaves the system prompt intact. The
// non-caching control keeps the requested mode (the downgrade
// never fires without a caching provider).
func TestCacheAwareFrozenPrefixDowngrades(t *testing.T) {
	msgs, _ := g22FreezeFixture()
	wantSys := findRole(Normalise(msgs), "system").Content
	budget := g22FreezeBudget(t, msgs)
	frozen := func([]provider.Message) bool { return true }
	req := &provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: budget, Mode: "aggressive", PreserveSystemPromptMode: "never"}
	out, res := ApplyWithOptions("gpt-4o", msgs, req, nil, nil, ApplyOptions{IsCachingProvider: true, FrozenPrefix: frozen})
	if res.Mode != "standard" {
		t.Fatalf("aggressive with a caching provider must downgrade to standard, got %q", res.Mode)
	}
	sys := findRole(out, "system")
	if sys == nil {
		t.Fatalf("no system message in output: %+v", out)
	}
	if sys.Content != wantSys {
		t.Fatalf("downgraded run with a frozen prefix must leave the system prompt intact; got truncated to %d chars, want %d", len(sys.Content), len(wantSys))
	}
	_, res = ApplyWithOptions("gpt-4o", msgs, req, nil, nil, ApplyOptions{FrozenPrefix: frozen})
	if res.Mode != "aggressive" {
		t.Fatalf("aggressive without a caching provider must stay aggressive, got %q", res.Mode)
	}
}
