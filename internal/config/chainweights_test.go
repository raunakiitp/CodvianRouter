// Chain per-target weights (M3 Phase C, builder C3): config round-trip,
// Clone deep-copy, and load-time validation for
// routing.chain_weights.
package config

import (
	"encoding/json"
	"math"
	"testing"
)

// TestConfigChainWeightsRoundTrip: chain_weights survive a JSON
// marshal/unmarshal round-trip and pass Validate, and Clone
// deep-copies the nested map.
func TestConfigChainWeightsRoundTrip(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{
		GlobalStrategy:  "weighted",
		ChainStrategies: map[string]string{"gpt-4o": "weighted"},
		ChainWeights: map[string]map[string]float64{
			"gpt-4o":      {"openai/gpt-4o": 3, "anthropic/gpt-4o": 1, "x/gpt-4o": 0},
			"other-model": {"a/m1": 0.5},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Routing == nil {
		t.Fatal("routing block lost in round-trip")
	}
	if back.Routing.ChainWeights["gpt-4o"]["openai/gpt-4o"] != 3 ||
		back.Routing.ChainWeights["gpt-4o"]["anthropic/gpt-4o"] != 1 ||
		back.Routing.ChainWeights["gpt-4o"]["x/gpt-4o"] != 0 ||
		back.Routing.ChainWeights["other-model"]["a/m1"] != 0.5 {
		t.Errorf("chain_weights did not round-trip: %+v", back.Routing.ChainWeights)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round-tripped config failed Validate: %v", err)
	}
	// Clone deep-copies the nested map: mutating the clone's inner
	// map (or adding a chain) must not reach the original.
	clone := cfg.Clone()
	clone.Routing.ChainWeights["gpt-4o"]["openai/gpt-4o"] = 99
	if cfg.Routing.ChainWeights["gpt-4o"]["openai/gpt-4o"] != 3 {
		t.Error("Clone shared ChainWeights inner map with the original")
	}
	clone.Routing.ChainWeights["new-chain"] = map[string]float64{"a/m1": 1}
	if _, ok := cfg.Routing.ChainWeights["new-chain"]; ok {
		t.Error("Clone shared ChainWeights outer map with the original")
	}
}

// TestConfigChainWeightsValidation: load-time rules — weights must be
// finite and >= 0 (negative/NaN/Inf rejected naming the field); a
// chain with all-zero weights is rejected; unknown chain keys are
// allowed; a missing map means all weights 1 (valid).
func TestConfigChainWeightsValidation(t *testing.T) {
	base := func() *Config { return DefaultConfig(t.TempDir()) }
	bad := []struct {
		name string
		mut  func(*Config)
	}{
		{"negative weight", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{"m": {"a/m1": -1}}
		}},
		{"NaN weight", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{"m": {"a/m1": math.NaN()}}
		}},
		{"+Inf weight", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{"m": {"a/m1": math.Inf(1)}}
		}},
		{"-Inf weight", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{"m": {"a/m1": math.Inf(-1)}}
		}},
		{"all-zero weights", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{"m": {"a/m1": 0, "b/m2": 0}}
		}},
		{"single zero weight", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{"m": {"a/m1": 0}}
		}},
		{"empty chain key", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{"": {"a/m1": 1}}
		}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			c.Routing = &RoutingConfig{}
			tc.mut(c)
			if err := c.Validate(); err == nil {
				t.Errorf("%s: Validate passed; want an error naming routing.chain_weights", tc.name)
			}
		})
	}
	good := []struct {
		name string
		mut  func(*Config)
	}{
		{"missing map", func(c *Config) {}},
		{"empty map", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{}
		}},
		{"empty inner map", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{"m": {}}
		}},
		{"unknown chain key allowed", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{"never-seen-model": {"a/m1": 2}}
		}},
		{"unknown target key allowed", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{"m": {"z/m9": 3}}
		}},
		{"zero mixed with positive", func(c *Config) {
			c.Routing.ChainWeights = map[string]map[string]float64{"m": {"a/m1": 0, "b/m2": 1}}
		}},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			c.Routing = &RoutingConfig{}
			tc.mut(c)
			if err := c.Validate(); err != nil {
				t.Errorf("%s: Validate = %v, want nil", tc.name, err)
			}
		})
	}
}
