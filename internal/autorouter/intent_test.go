package autorouter

import (
	"strings"
	"testing"
)

func TestParseEffort(t *testing.T) {
	for in, want := range map[string]Effort{
		"": " ", "low": EffortLow, "LOW": EffortLow, " medium ": EffortMedium, "high": EffortHigh,
	} {
		_ = want
		got, err := ParseEffort(in)
		if err != nil {
			t.Fatalf("ParseEffort(%q) error: %v", in, err)
		}
		if in == "" {
			if got != "" {
				t.Fatalf("ParseEffort(%q) = %q, want empty", in, got)
			}
			continue
		}
		if got != Effort(strings.ToLower(strings.TrimSpace(in))) {
			t.Fatalf("ParseEffort(%q) = %q", in, got)
		}
	}
	for _, bad := range []string{"huge", "min", "low-medium", "0"} {
		if _, err := ParseEffort(bad); err == nil {
			t.Fatalf("ParseEffort(%q) must error", bad)
		}
	}
}

func TestParseIntentModel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		tier IntentTier
		ok   bool
	}{
		{"intent/fast", IntentFast, true},
		{"intent/balanced", IntentBalanced, true},
		{"intent/best", IntentBest, true},
		{"intent", "", false},
		{"intent/", "", false},
		{"intent/unknown", "", false},
		{"intent/fast ", IntentFast, true},
		{"auto/fast", "", false},
		{"gpt-4o", "", false},
		{"", "", false},
	} {
		tier, ok := ParseIntentModel(tc.in)
		if ok != tc.ok || tier != tc.tier {
			t.Fatalf("ParseIntentModel(%q) = (%q, %v), want (%q, %v)", tc.in, tier, ok, tc.tier, tc.ok)
		}
	}
	if !IsVirtualModel("intent/fast") || !IsVirtualModel("auto") {
		t.Fatal("intent/fast and auto must both be virtual models")
	}
	if IsVirtualModel("intent/unknown") || IsVirtualModel("intent") {
		t.Fatal("an unknown intent tier name must not be a virtual model")
	}
}

func TestResolveIntentMapped(t *testing.T) {
	r := New(Config{
		IntentTiers: map[IntentTier]IntentMapping{
			IntentFast:     {Model: "model-cheap", Effort: EffortLow},
			IntentBest:     {Model: "model-strong", Effort: EffortHigh},
			IntentBalanced: {Model: "model-mid"},
		},
		DefaultModel: "fallback-model",
	})
	model, dec, ok := r.Resolve("intent/fast", "any prompt at all")
	if !ok || model != "model-cheap" {
		t.Fatalf("Resolve(intent/fast) = (%q, ok=%v), want (model-cheap, true)", model, ok)
	}
	if dec.IntentTier != IntentFast || dec.Effort != EffortLow {
		t.Fatalf("decision intent_tier=%q effort=%q, want fast/low", dec.IntentTier, dec.Effort)
	}
	if dec.Confidence != 0 {
		t.Fatalf("intent decision confidence = %v, want 0 (no classification happened)", dec.Confidence)
	}
	if !strings.Contains(dec.Reason, "operator-defined") {
		t.Fatalf("reason %q must say the mapping is operator-defined", dec.Reason)
	}
	// No effort pinned: the decision carries no effort, never an invented one.
	if _, dec, ok := r.Resolve("intent/balanced", ""); !ok || dec.Effort != "" {
		t.Fatalf("Resolve(intent/balanced) = (ok=%v, effort=%q), want (true, \"\")", ok, dec.Effort)
	}
	// The prompt is irrelevant to an intent: an empty prompt still resolves.
	if model, _, ok := r.Resolve("intent/best", ""); !ok || model != "model-strong" {
		t.Fatalf("Resolve(intent/best, empty prompt) = (%q, ok=%v), want (model-strong, true)", model, ok)
	}
}

func TestResolveIntentUnmappedErrorsHonestly(t *testing.T) {
	r := New(Config{
		IntentTiers:  map[IntentTier]IntentMapping{IntentFast: {Model: "model-cheap"}},
		DefaultModel: "fallback-model",
	})
	model, dec, ok := r.Resolve("intent/best", "hello")
	if ok || model != "" {
		t.Fatalf("Resolve(intent/best) = (%q, ok=%v), want (\"\", false)", model, ok)
	}
	if dec.Model != "" {
		t.Fatalf("unmapped decision carries Model %q — no silent default model", dec.Model)
	}
	if !strings.Contains(dec.Reason, `"best"`) || !strings.Contains(dec.Reason, "not mapped") {
		t.Fatalf("reason %q must name the tier and say it is not mapped", dec.Reason)
	}
	// With no tiers at all, every intent tier errors the same way.
	empty := New(Config{DefaultModel: "fallback-model"})
	if _, dec, ok := empty.Resolve("intent/fast", "hello"); ok || !strings.Contains(dec.Reason, `"fast"`) {
		t.Fatalf("unconfigured router Resolve(intent/fast) = (ok=%v, reason=%q), want ok=false naming \"fast\"", ok, dec.Reason)
	}
}

func TestSetIntentTierValidation(t *testing.T) {
	r := New(Config{DefaultModel: "m"})
	if err := r.SetIntentTier("turbo", IntentMapping{Model: "m"}); err == nil {
		t.Fatal("unknown tier must error")
	}
	if err := r.SetIntentTier(IntentFast, IntentMapping{Model: ""}); err == nil {
		t.Fatal("empty model must error")
	}
	if err := r.SetIntentTier(IntentFast, IntentMapping{Model: "m", Effort: "huge"}); err == nil {
		t.Fatal("unknown effort must error")
	}
	if err := r.SetIntentTier(IntentFast, IntentMapping{Model: "m", Effort: EffortLow}); err != nil {
		t.Fatalf("valid mapping must apply: %v", err)
	}
	if m, ok := r.ResolveIntent(IntentFast); !ok || m.Model != "m" || m.Effort != EffortLow {
		t.Fatalf("ResolveIntent(fast) = (%+v, %v), want (m/low, true)", m, ok)
	}
	// Redefining a tier replaces it (the operator's definition wins).
	if err := r.SetIntentTier(IntentFast, IntentMapping{Model: "m2", Effort: EffortHigh}); err != nil {
		t.Fatalf("redefine must apply: %v", err)
	}
	if m, _ := r.ResolveIntent(IntentFast); m.Model != "m2" || m.Effort != EffortHigh {
		t.Fatalf("redefined mapping = %+v, want m2/high", m)
	}
}

func TestNewDropsBadIntentTiers(t *testing.T) {
	r := New(Config{
		IntentTiers: map[IntentTier]IntentMapping{
			IntentFast:     {Model: "good"},
			"turbo":        {Model: "bad-tier"},
			IntentBest:     {Model: ""},
			IntentBalanced: {Model: "bad-effort", Effort: "huge"},
		},
		DefaultModel: "fallback-model",
	})
	if got := r.IntentTiers(); len(got) != 1 || got[IntentFast].Model != "good" {
		t.Fatalf("IntentTiers() = %v, want only fast=good", got)
	}
}
