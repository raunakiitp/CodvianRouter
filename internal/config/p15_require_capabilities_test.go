// P1.5 routing.require_capabilities config: known names load, unknown
// names fail at Validate with the vocabulary named, and the requirement
// round-trips JSON and Clone without sharing the slice.
package config

import (
	"encoding/json"
	"testing"
)

func TestRequireCapabilitiesValidation(t *testing.T) {
	valid := [][]string{
		{"vision"},
		{"tool_calling"},
		{"vision", "audio_input", "streaming"},
		nil,
	}
	for i, caps := range valid {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{RequireCapabilities: caps}
		if err := cfg.Validate(); err != nil {
			t.Errorf("valid case %d (%v) failed Validate: %v", i, caps, err)
		}
	}

	invalid := []string{"", "Vision", "tool-calling", "telepathy", "tool_calling "}
	for _, name := range invalid {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{RequireCapabilities: []string{name}}
		if err := cfg.Validate(); err == nil {
			t.Errorf("routing.require_capabilities %q passed Validate; want an error", name)
		}
	}
}

func TestRequireCapabilitiesRoundTripAndClone(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{RequireCapabilities: []string{"vision", "tool_calling"}}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back.Routing.RequireCapabilities) != 2 || back.Routing.RequireCapabilities[0] != "vision" {
		t.Fatalf("require_capabilities did not round-trip: %v", back.Routing.RequireCapabilities)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("round-tripped config failed Validate: %v", err)
	}

	clone := cfg.Clone()
	clone.Routing.RequireCapabilities[0] = "audio_input"
	if cfg.Routing.RequireCapabilities[0] != "vision" {
		t.Fatal("Clone shared the RequireCapabilities slice with the original")
	}

	snap := cfg.RoutingSnapshot()
	snap.RequireCapabilities[1] = "video"
	if cfg.Routing.RequireCapabilities[1] != "tool_calling" {
		t.Fatal("RoutingSnapshot shared the RequireCapabilities slice with the live config")
	}
}
