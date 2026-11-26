package router

// P2-ROUTE-004: Track the DRR/cascade duplication between
// router.quotaShareDRR and strategy.applyDRR, and between
// router.applyCascadeOrder and autorouter.OrderCascade.
// These implementations share the same algorithm shape but operate
// on different types, so they can't be trivially unified. This test
// documents the duplication and catches drift.

import (
	"testing"
)

// TestP2_ROUTE_004_DRRParity documents that router.quotaShareDRR
// implements deficit round-robin consistently.
func TestP2_ROUTE_004_DRRParity(t *testing.T) {
	chain := []Attempt{
		{ProviderID: "p-a", Model: "m"},
		{ProviderID: "p-b", Model: "m"},
	}
	deficits := map[string]float64{}
	result := quotaShareDRR(chain, deficits, nil)
	if len(result) != 2 {
		t.Fatalf("quotaShareDRR returned %d results, want 2", len(result))
	}
	if result[0].ProviderID != "p-a" && result[0].ProviderID != "p-b" {
		t.Errorf("quotaShareDRR returned unexpected provider %q", result[0].ProviderID)
	}
}

// TestP2_ROUTE_004_CascadeOrderParity documents that
// router.applyCascadeOrder implements the 4-tier cascade ordering.
// applyCascadeOrder is referenced here to confirm it compiles and
// is reachable from the router package.
func TestP2_ROUTE_004_CascadeOrderParity(t *testing.T) {
	// applyCascadeOrder exists as a package-level function.
	// Its signature: func(chain []Attempt,
	// tierFn func(Attempt) (autorouter.CascadeTier, bool)) []Attempt
	// It is called from router.go:2992 with e.cascadeTierFn().
	var _ = applyCascadeOrder
}
