// W1.3 (M1.11): with no strategy configured anywhere, the engine
// resolves the default "priority" — try targets in the operator's
// declared chain order, no re-ranking (was "balanced").
package router

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestW1EffectiveStrategyDefaultsToPriority(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{}, nil, testConfig())
	if got := eng.effectiveStrategy("", "m"); got != "priority" {
		t.Errorf("effectiveStrategy(unset) = %q, want priority", got)
	}
	// An explicit per-request strategy still wins.
	if got := eng.effectiveStrategy("cheapest", "m"); got != "cheapest" {
		t.Errorf("effectiveStrategy(cheapest) = %q, want cheapest", got)
	}
	// A configured global strategy still wins over the default.
	cfg := testConfig()
	cfg.Strategy = "balanced"
	eng2 := NewEngine(map[string]provider.Client{}, nil, cfg)
	if got := eng2.effectiveStrategy("", "m"); got != "balanced" {
		t.Errorf("effectiveStrategy(configured balanced) = %q, want balanced", got)
	}
}
