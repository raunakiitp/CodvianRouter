package router

// P2-ROUTE-002: Engine.FallbackChain (single) has no callers - add a test
// pinning its behavior instead of deleting the public API.

import (
	"testing"
)

// TestP2_ROUTE_002_FallbackChainHasTest pins the behavior of the public
// FallbackChain API method so it isn't accidentally removed as dead code.
func TestP2_ROUTE_002_FallbackChainHasTest(t *testing.T) {
	e := NewEngine(nil, nil, Config{})

	// No chain exists yet
	_, ok := e.FallbackChain("nonexistent")
	if ok {
		t.Error("FallbackChain returned ok=true for nonexistent model")
	}

	// Register a chain
	err := e.SetFallbackChain("test-model", []FallbackEntry{
		{Provider: "provider-a", Priority: 1, Enabled: true},
		{Provider: "provider-b", Priority: 2, Enabled: true},
	})
	if err != nil {
		t.Fatalf("SetFallbackChain: %v", err)
	}

	entries, ok := e.FallbackChain("test-model")
	if !ok {
		t.Fatal("FallbackChain returned ok=false for existing model")
	}
	if len(entries) != 2 {
		t.Fatalf("FallbackChain returned %d entries, want 2", len(entries))
	}
	if entries[0].Provider != "provider-a" {
		t.Errorf("entries[0].Provider = %q, want %q", entries[0].Provider, "provider-a")
	}
	if entries[1].Provider != "provider-b" {
		t.Errorf("entries[1].Provider = %q, want %q", entries[1].Provider, "provider-b")
	}
}
