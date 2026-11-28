package config

import (
	"strings"
	"testing"
)

// A2-3b: the fusion judge + panel knobs are config-file fields, and a
// nonsense value is a load error (never a silent "use the default").
//
// FAILS-WHEN-OFF: remove the validation in config.go and the negative /
// incoherent values below load fine (the "want error" cases fail).

func fusionConfig(t *testing.T, routing *RoutingConfig) *Config {
	t.Helper()
	cfg := DefaultConfig(t.TempDir())
	cfg.Routing = routing
	return cfg
}

func TestFusionRoutingFieldsAccepted(t *testing.T) {
	cfg := fusionConfig(t, &RoutingConfig{
		FusionJudgeModel:         "judge-model",
		FusionMinPanel:           2,
		FusionMaxPanel:           40,
		FusionStragglerGraceMs:   8000,
		FusionPanelHardTimeoutMs: 90000,
	})
	if err := ValidateFile(writeTempConfig(t, cfg)); err != nil {
		t.Fatalf("ValidateFile(valid fusion knobs) = %v, want nil", err)
	}
}

func TestFusionRoutingNegativeValuesRejected(t *testing.T) {
	cases := []struct {
		name    string
		routing *RoutingConfig
		want    string
	}{
		{"negative min panel", &RoutingConfig{FusionMinPanel: -1}, "fusion_min_panel"},
		{"negative max panel", &RoutingConfig{FusionMaxPanel: -1}, "fusion_max_panel"},
		{"negative straggler grace", &RoutingConfig{FusionStragglerGraceMs: -1}, "fusion_straggler_grace_ms"},
		{"negative hard timeout", &RoutingConfig{FusionPanelHardTimeoutMs: -1}, "fusion_panel_hard_timeout_ms"},
		{"min above max", &RoutingConfig{FusionMinPanel: 5, FusionMaxPanel: 2}, "fusion_min_panel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFile(writeTempConfig(t, fusionConfig(t, tc.routing)))
			if err == nil {
				t.Fatalf("ValidateFile = nil, want an error naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateFile error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}
