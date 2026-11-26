package router

// P2-ROUTE-001: Declarative per-model fallback chains are wired into
// the request path via chainFor consulting FallbackChainFor.

import (
	"testing"
)

// TestP2_ROUTE_001_FallbackChainWiredIntoChainFor verifies that
// chainFor consults FallbackChainFor for models with a declared chain.
func TestP2_ROUTE_001_FallbackChainWiredIntoChainFor(t *testing.T) {
	e := NewEngine(nil, nil, Config{})

	// Register a fallback chain for "test-model"
	err := e.SetFallbackChain("test-model", []FallbackEntry{
		{Provider: "provider-a", Priority: 1, Enabled: true},
		{Provider: "provider-b", Priority: 2, Enabled: true},
	})
	if err != nil {
		t.Fatalf("SetFallbackChain: %v", err)
	}

	// chainFor should return the chain's providers in priority order
	chain := e.chainFor("test-model")
	if len(chain) != 2 {
		t.Fatalf("chainFor returned %d attempts, want 2", len(chain))
	}
	if chain[0].ProviderID != "provider-a" {
		t.Errorf("chain[0].ProviderID = %q, want %q", chain[0].ProviderID, "provider-a")
	}
	if chain[1].ProviderID != "provider-b" {
		t.Errorf("chain[1].ProviderID = %q, want %q", chain[1].ProviderID, "provider-b")
	}
}

// TestP2_ROUTE_001_FallbackChainFallsThroughToEmpty verifies that
// chainFor returns an empty chain when no chain exists and no clients.
func TestP2_ROUTE_001_FallbackChainFallsThroughToEmpty(t *testing.T) {
	e := NewEngine(nil, nil, Config{})

	// No chain registered, no clients configured
	chain := e.chainFor("other-model")
	if len(chain) != 0 {
		t.Fatalf("chainFor returned %d attempts, want 0", len(chain))
	}
}

// TestP2_ROUTE_001_FallbackChainSkipsDisabled verifies that disabled
// entries are excluded from the chain.
func TestP2_ROUTE_001_FallbackChainSkipsDisabled(t *testing.T) {
	e := NewEngine(nil, nil, Config{})

	err := e.SetFallbackChain("disabled-model", []FallbackEntry{
		{Provider: "provider-disabled", Priority: 1, Enabled: false},
		{Provider: "provider-enabled", Priority: 2, Enabled: true},
	})
	if err != nil {
		t.Fatalf("SetFallbackChain: %v", err)
	}

	chain := e.chainFor("disabled-model")
	if len(chain) != 1 {
		t.Fatalf("chainFor returned %d attempts, want 1 (disabled excluded)", len(chain))
	}
	if chain[0].ProviderID != "provider-enabled" {
		t.Errorf("chain[0].ProviderID = %q, want %q", chain[0].ProviderID, "provider-enabled")
	}
}
