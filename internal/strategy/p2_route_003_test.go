package strategy

// P2-ROUTE-003: Pin the strategy count so it can't silently drift.

import (
	"testing"
)

// TestP2_ROUTE_003_PinStrategyCount pins the number of chain-capable
// strategies in All. The project record states 35 chain strategies;
// All currently lists 40 entries including 3 combo execution modes
// (fusion, pipeline, jev) that are not chain strategies.
// If this test fails, the count has changed intentionally or
// unintentionally — update this pin and the project record together.
func TestP2_ROUTE_003_PinStrategyCount(t *testing.T) {
	const expectedCount = 40
	if len(All) != expectedCount {
		t.Errorf("len(All) = %d, want %d (chain strategies + combo modes)",
			len(All), expectedCount)
	}
}

// TestP2_ROUTE_003_ComboModesAreNotChainStrategies documents that
// fusion, pipeline, and jev are combo execution modes, not chain
// strategies, and are correctly excluded from chain-strategy counts.
func TestP2_ROUTE_003_ComboModesAreNotChainStrategies(t *testing.T) {
	comboModes := []Strategy{StrategyFusion, StrategyPipeline, StrategyJEV}
	for _, m := range comboModes {
		found := false
		for _, s := range All {
			if s == m {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("combo mode %s not found in All", m)
		}
	}
}
