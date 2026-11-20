// Chain per-target weights (M3 Phase C, builder C3): Resolve applies
// the operator-configured per-target weights for one chain to an
// ordering-target list, and ValidateWeights mirrors the config-load
// rules for reuse in tests.
package strategy

import (
	"math"
	"math/rand"
	"testing"
)

// TestChainWeightsResolveAppliesWeights: configured weights land on
// the matching targets, by "provider/model" key.
func TestChainWeightsResolveAppliesWeights(t *testing.T) {
	in := []OrderingTarget{
		{Provider: "a", Model: "m1", Weight: 1},
		{Provider: "b", Model: "m2", Weight: 1},
		{Provider: "c", Model: "m3", Weight: 1},
	}
	cfg := map[string]map[string]float64{
		"gpt-4o": {"a/m1": 3, "b/m2": 0, "c/m3": 1.5},
	}
	got := Resolve("gpt-4o", in, cfg)
	want := []float64{3, 0, 1.5}
	for i, w := range want {
		if got[i].Weight != w {
			t.Fatalf("target %d: Weight = %v, want %v", i, got[i].Weight, w)
		}
	}
}

// TestChainWeightsResolveDefaultsToOne: a missing map, a missing chain
// entry, and a missing target entry all mean weight 1 (uniform).
func TestChainWeightsResolveDefaultsToOne(t *testing.T) {
	in := []OrderingTarget{
		{Provider: "a", Model: "m1", Weight: 7},
		{Provider: "b", Model: "m2", Weight: 7},
	}
	// Nil map.
	got := Resolve("gpt-4o", in, nil)
	for i := range got {
		if got[i].Weight != 1 {
			t.Fatalf("nil cfg: target %d Weight = %v, want 1", i, got[i].Weight)
		}
	}
	// Empty map.
	got = Resolve("gpt-4o", in, map[string]map[string]float64{})
	for i := range got {
		if got[i].Weight != 1 {
			t.Fatalf("empty cfg: target %d Weight = %v, want 1", i, got[i].Weight)
		}
	}
	// Chain key absent (other chains configured).
	got = Resolve("gpt-4o", in, map[string]map[string]float64{
		"claude-x": {"a/m1": 9},
	})
	for i := range got {
		if got[i].Weight != 1 {
			t.Fatalf("missing chain: target %d Weight = %v, want 1", i, got[i].Weight)
		}
	}
	// Target key absent from a configured chain: that target gets 1,
	// the configured one gets its weight.
	got = Resolve("gpt-4o", in, map[string]map[string]float64{
		"gpt-4o": {"a/m1": 4},
	})
	if got[0].Weight != 4 || got[1].Weight != 1 {
		t.Fatalf("missing target: Weights = %v, %v, want 4, 1", got[0].Weight, got[1].Weight)
	}
	// Empty inner map means no overrides: all 1.
	got = Resolve("gpt-4o", in, map[string]map[string]float64{"gpt-4o": {}})
	for i := range got {
		if got[i].Weight != 1 {
			t.Fatalf("empty chain map: target %d Weight = %v, want 1", i, got[i].Weight)
		}
	}
}

// TestChainWeightsResolveIgnoresUnknownKeys: a target key in the
// config that matches no target in the list changes nothing and
// never errors; neither does a chain key for another chain.
func TestChainWeightsResolveIgnoresUnknownKeys(t *testing.T) {
	in := []OrderingTarget{
		{Provider: "a", Model: "m1", Weight: 1},
		{Provider: "b", Model: "m2", Weight: 1},
	}
	cfg := map[string]map[string]float64{
		"gpt-4o":      {"z/m9": 99, "a/m1": 2},
		"other-chain": {"a/m1": 50},
	}
	got := Resolve("gpt-4o", in, cfg)
	if got[0].Weight != 2 || got[1].Weight != 1 {
		t.Fatalf("Weights = %v, %v, want 2, 1", got[0].Weight, got[1].Weight)
	}
}

// TestChainWeightsResolveNeverMutatesInput: Resolve returns a new
// slice; the caller's targets (and their weights) are untouched,
// and mutating the result does not reach the input.
func TestChainWeightsResolveNeverMutatesInput(t *testing.T) {
	in := []OrderingTarget{
		{Provider: "a", Model: "m1", Score: 0.1, Attempts: 5, QuotaRemaining: 0, Weight: 7},
		{Provider: "b", Model: "m2", Score: 0.9, Attempts: 1, QuotaRemaining: 9, Weight: 8},
	}
	snapshot := append([]OrderingTarget{}, in...)
	cfg := map[string]map[string]float64{"gpt-4o": {"a/m1": 3}}
	got := Resolve("gpt-4o", in, cfg)
	if got[0].Weight != 3 {
		t.Fatalf("resolved Weight = %v, want 3", got[0].Weight)
	}
	for i := range in {
		if in[i] != snapshot[i] {
			t.Fatalf("input mutated at index %d: %+v vs %+v", i, in[i], snapshot[i])
		}
	}
	got[1].Weight = 123
	if in[1].Weight != snapshot[1].Weight {
		t.Fatalf("mutating the result reached the input: %v", in[1].Weight)
	}
	// Empty / nil input resolves to an empty list, never a panic.
	if out := Resolve("gpt-4o", nil, cfg); len(out) != 0 {
		t.Fatalf("Resolve(nil) = %v, want empty", out)
	}
}

// TestChainWeightsResolveFeedsWeightedDraw: the resolved weights are
// real draw weights — a target with the whole weight always wins
// the WeightedDraw, and a zero-weight target never wins while a
// positive weight exists.
func TestChainWeightsResolveFeedsWeightedDraw(t *testing.T) {
	in := []OrderingTarget{
		{Provider: "a", Model: "m1"},
		{Provider: "b", Model: "m2"},
		{Provider: "c", Model: "m3"},
	}
	cfg := map[string]map[string]float64{"gpt-4o": {"a/m1": 0, "b/m2": 1, "c/m3": 0}}
	resolved := Resolve("gpt-4o", in, cfg)
	for _, seed := range []int64{1, 2, 3, 4, 5} {
		got, ok := WeightedDraw(resolved, rand.New(rand.NewSource(seed)))
		if !ok || got.Key() != "b/m2" {
			t.Fatalf("seed %d: WeightedDraw after Resolve = %v, want b/m2", seed, got.Key())
		}
	}
}

// TestChainWeightsValidateWeights: the mirrored rules — weights must
// be finite and >= 0; a chain with all-zero weights is rejected;
// unknown chain keys are allowed; a missing/empty map is valid.
func TestChainWeightsValidateWeights(t *testing.T) {
	bad := []struct {
		name string
		cfg  map[string]map[string]float64
	}{
		{"negative weight", map[string]map[string]float64{"m": {"a/m1": -0.5}}},
		{"NaN weight", map[string]map[string]float64{"m": {"a/m1": math.NaN()}}},
		{"+Inf weight", map[string]map[string]float64{"m": {"a/m1": math.Inf(1)}}},
		{"-Inf weight", map[string]map[string]float64{"m": {"a/m1": math.Inf(-1)}}},
		{"all-zero weights", map[string]map[string]float64{"m": {"a/m1": 0, "b/m2": 0}}},
		{"single zero weight", map[string]map[string]float64{"m": {"a/m1": 0}}},
		{"all-zero in one chain of many", map[string]map[string]float64{
			"good": {"a/m1": 2}, "bad": {"a/m1": 0, "b/m2": 0},
		}},
		{"empty chain key", map[string]map[string]float64{"": {"a/m1": 1}}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateWeights(tc.cfg); err == nil {
				t.Errorf("ValidateWeights(%v) passed; want an error", tc.cfg)
			}
		})
	}
	good := []struct {
		name string
		cfg  map[string]map[string]float64
	}{
		{"nil map", nil},
		{"empty map", map[string]map[string]float64{}},
		{"empty inner map", map[string]map[string]float64{"m": {}}},
		{"unknown chain key", map[string]map[string]float64{"never-seen-model": {"a/m1": 2}}},
		{"unknown target key", map[string]map[string]float64{"m": {"z/m9": 3}}},
		{"zero mixed with positive", map[string]map[string]float64{"m": {"a/m1": 0, "b/m2": 1}}},
		{"fractional weights", map[string]map[string]float64{"m": {"a/m1": 0.25, "b/m2": 0.75}}},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateWeights(tc.cfg); err != nil {
				t.Errorf("ValidateWeights(%v) = %v, want nil", tc.cfg, err)
			}
		})
	}
}
