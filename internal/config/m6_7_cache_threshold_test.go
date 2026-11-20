// M6.7 config cache.semantic_threshold: a configured value
// must be within [0.85, 1.0] or Validate rejects it (never
// silently clamped, following the file's convention for
// auto_router.threshold); 0 means unset and selects the 0.85
// default.
package config

import (
	"strings"
	"testing"
)

func TestM67CacheSemanticThresholdValidation(t *testing.T) {
	invalid := []float64{0.84, 0.5, 0.1, -0.1, 1.01, 2.0}
	for _, v := range invalid {
		cfg := DefaultConfig(t.TempDir())
		cfg.Cache = &CacheConfig{SemanticThreshold: v}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate with cache.semantic_threshold=%v: want an error, got nil", v)
		} else if !strings.Contains(err.Error(), "cache.semantic_threshold") {
			t.Fatalf("Validate with cache.semantic_threshold=%v: err = %v, want it to name cache.semantic_threshold", v, err)
		}
	}
	valid := []float64{0, 0.85, 0.9, 1.0}
	for _, v := range valid {
		cfg := DefaultConfig(t.TempDir())
		cfg.Cache = &CacheConfig{SemanticThreshold: v}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate with cache.semantic_threshold=%v: want nil, got %v", v, err)
		}
	}
	// A nil Cache block is valid too (all cache settings unset).
	if err := DefaultConfig(t.TempDir()).Validate(); err != nil {
		t.Fatalf("Validate with nil Cache: want nil, got %v", err)
	}
}

func TestM67CacheEffectiveSemanticThreshold(t *testing.T) {
	var nilCfg *CacheConfig
	if got := nilCfg.EffectiveSemanticThreshold(); got != 0.85 {
		t.Fatalf("nil EffectiveSemanticThreshold = %v, want 0.85", got)
	}
	if got := (&CacheConfig{}).EffectiveSemanticThreshold(); got != 0.85 {
		t.Fatalf("unset EffectiveSemanticThreshold = %v, want 0.85", got)
	}
	if got := (&CacheConfig{SemanticThreshold: 0.92}).EffectiveSemanticThreshold(); got != 0.92 {
		t.Fatalf("configured EffectiveSemanticThreshold = %v, want 0.92", got)
	}
}
