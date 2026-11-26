package config

// p2_capability_config_test.go — audit finding P2-PROV-002.
//
// routing.require_capabilities must not accept a name that nothing can ever
// record: the filter would then skip every attempt and the operator would see
// ErrNoAvailableAttempt with no explanation. The four unpopulatable names are
// rejected at Validate with a reason; the four api_* names are accepted (and
// are populated by the capability builder from the wire rule).

import (
	"strings"
	"testing"
)

func TestRequireCapabilitiesRejectsUnpopulatableNames(t *testing.T) {
	rejected := []string{"image_output", "audio_output", "structured_output", "json_mode"}
	for _, name := range rejected {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{RequireCapabilities: []string{name}}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("require_capabilities %q passed Validate; want a clear rejection", name)
			continue
		}
		if !strings.Contains(err.Error(), "cannot be satisfied") {
			t.Errorf("require_capabilities %q error = %v; want it to say the capability cannot be satisfied", name, err)
		}
		if !strings.Contains(err.Error(), "populates it") {
			t.Errorf("require_capabilities %q error = %v; want it to name the missing population source", name, err)
		}
	}
}

func TestRequireCapabilitiesAcceptsPopulatedEndpointNames(t *testing.T) {
	// These four used to be accepted-but-never-populated; the capability
	// builder now records them from the wire rule, so requiring them is
	// meaningful and must load.
	for _, name := range []string{"api_embeddings", "api_rerank", "api_images", "api_audio"} {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{RequireCapabilities: []string{name}}
		if err := cfg.Validate(); err != nil {
			t.Errorf("require_capabilities %q failed Validate: %v (the builder populates it now)", name, err)
		}
	}
}
