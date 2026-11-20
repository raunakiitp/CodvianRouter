// Operator-declared forced-reasoning policy (CHECKLIST 311-312):
// a policy forces an effort on a model or route whatever the
// client asked for, unless it allows the client to override —
// and a bad rule fails config validation with the field named.
package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestForcedEffortLookup(t *testing.T) {
	cfg := &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{
		{Model: "auto/coding", Effort: "high"},
		{Model: "model-b", Effort: "high", AllowClientOverride: true},
		{Model: "*", Effort: "medium"},
	}}
	for _, tc := range []struct {
		model string
		want  ForcedEffortPolicy
		ok    bool
	}{
		{"auto/coding", ForcedEffortPolicy{Model: "auto/coding", Effort: "high"}, true},
		{"model-b", ForcedEffortPolicy{Model: "model-b", Effort: "high", AllowClientOverride: true}, true},
		{"model-a", ForcedEffortPolicy{Model: "*", Effort: "medium"}, true}, // wildcard fallback
		{"gpt-5", ForcedEffortPolicy{Model: "*", Effort: "medium"}, true},   // wildcard fallback
	} {
		got, ok := cfg.ForcedEffortFor(tc.model)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("ForcedEffortFor(%q) = %+v, %v; want %+v, %v", tc.model, got, ok, tc.want, tc.ok)
		}
	}
	// Exact beats wildcard even when the wildcard is declared first.
	cfg2 := &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{
		{Model: "*", Effort: "low"},
		{Model: "model-a", Effort: "high"},
	}}
	if got, ok := cfg2.ForcedEffortFor("model-a"); !ok || got.Effort != "high" {
		t.Fatalf("ForcedEffortFor(model-a) = %+v, %v; want high exact over wildcard low", got, ok)
	}
	// First exact match wins among exact rules.
	cfg3 := &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{
		{Model: "model-a", Effort: "high"},
		{Model: "model-a", Effort: "low"},
	}}
	if got, ok := cfg3.ForcedEffortFor("model-a"); !ok || got.Effort != "high" {
		t.Fatalf("ForcedEffortFor(model-a) = %+v, %v; want first exact high", got, ok)
	}
	// No rules, or a nil receiver, never matches.
	if _, ok := (&ReasoningConfig{}).ForcedEffortFor("model-a"); ok {
		t.Fatal("empty ReasoningConfig matched a model")
	}
	if _, ok := (*ReasoningConfig)(nil).ForcedEffortFor("model-a"); ok {
		t.Fatal("nil ReasoningConfig matched a model")
	}
}

func TestForcedEffortValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *ReasoningConfig
		want string // substring of the error, "" = valid
	}{
		{"empty list valid", &ReasoningConfig{}, ""},
		{"nil valid", nil, ""},
		{"one rule valid", &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{{Model: "auto/coding", Effort: "high"}}}, ""},
		{"wildcard valid", &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{{Model: "*", Effort: "low"}}}, ""},
		{"none effort valid (forcing no reasoning is a real policy)", &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{{Model: "model-a", Effort: "none"}}}, ""},
		{"override flag valid", &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{{Model: "model-a", Effort: "high", AllowClientOverride: true}}}, ""},
		{"empty model rejected", &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{{Model: "", Effort: "high"}}}, "empty model"},
		{"blank model rejected", &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{{Model: "   ", Effort: "high"}}}, "empty model"},
		{"unknown effort rejected", &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{{Model: "model-a", Effort: "ultra"}}}, "not a known effort level"},
		{"unnormalized effort rejected", &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{{Model: "model-a", Effort: "High"}}}, "not a known effort level"},
		{"duplicate model rejected", &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{{Model: "model-a", Effort: "high"}, {Model: "model-a", Effort: "low"}}}, "two rules for model"},
		{"duplicate wildcard rejected", &ReasoningConfig{ForcedEffort: []ForcedEffortPolicy{{Model: "*", Effort: "high"}, {Model: "*", Effort: "low"}}}, "two rules for model"},
	} {
		err := tc.cfg.ValidateForcedEffort()
		if tc.want == "" && err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s: err = %v, want substring %q", tc.name, err, tc.want)
		}
	}
}

func TestForcedEffortJSONRoundTrip(t *testing.T) {
	raw := `{"forced_effort":[{"model":"auto/coding","effort":"high"},{"model":"model-b","effort":"low","allow_client_override":true}]}`
	var cfg ReasoningConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cfg.ForcedEffort) != 2 || cfg.ForcedEffort[0].Effort != "high" || !cfg.ForcedEffort[1].AllowClientOverride {
		t.Fatalf("decoded = %+v, want 2 rules with override on the second", cfg.ForcedEffort)
	}
	if got, ok := cfg.ForcedEffortFor("model-b"); !ok || !got.AllowClientOverride {
		t.Fatalf("ForcedEffortFor(model-b) = %+v, %v; want override rule", got, ok)
	}
}
