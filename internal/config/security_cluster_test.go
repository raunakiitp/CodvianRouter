// Security-cluster config validation tests (CHECKLIST 245):
// deployment pins are range-checked at load, and a gateway key
// naming an unregistered guardrail fails at load.
package config

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestConfigFixedParamsValidation(t *testing.T) {
	base := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.Providers = []ProviderConfig{{
			ID:      "p1",
			BaseURL: "http://127.0.0.1:9",
			Models: []provider.Model{{
				ID:          "m1",
				FixedParams: &provider.ModelFixedParams{Temperature: ptrFloat(0.2), TopP: ptrFloat(0.9), MaxTokens: ptrInt(50)},
			}},
		}}
		return cfg
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid fixed params failed Validate: %v", err)
	}
	// Out-of-range pins fail, naming the field.
	for _, tc := range []struct {
		name string
		fp   *provider.ModelFixedParams
		want string
	}{
		{"temperature below 0", &provider.ModelFixedParams{Temperature: ptrFloat(-0.1)}, "fixed_params.temperature"},
		{"temperature above 2", &provider.ModelFixedParams{Temperature: ptrFloat(2.5)}, "fixed_params.temperature"},
		{"top_p zero", &provider.ModelFixedParams{TopP: ptrFloat(0)}, "fixed_params.top_p"},
		{"top_p above 1", &provider.ModelFixedParams{TopP: ptrFloat(1.5)}, "fixed_params.top_p"},
		{"max_tokens zero", &provider.ModelFixedParams{MaxTokens: ptrInt(0)}, "fixed_params.max_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			cfg.Providers[0].Models[0].FixedParams = tc.fp
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("got nil, want an error naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err.Error(), tc.want)
			}
		})
	}
	// Clone deep-copies the pin: mutating the clone's pin must
	// not move the original's.
	cfg := base()
	clone := cfg.Clone()
	*clone.Providers[0].Models[0].FixedParams.Temperature = 1.5
	if *cfg.Providers[0].Models[0].FixedParams.Temperature != 0.2 {
		t.Errorf("Clone shared the FixedParams pointer: original now %v", *cfg.Providers[0].Models[0].FixedParams.Temperature)
	}
}

func TestConfigGatewayKeyGuardrailNamesValidation(t *testing.T) {
	base := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.Guardrails = &GuardrailsConfig{External: []ExternalGuardrailConfig{
			{Name: "toxicity", Kind: "toxicity", Mode: "async", Endpoint: "http://127.0.0.1:9/check"},
		}}
		cfg.GatewayKeys = []GatewayKey{{ID: "k1", Name: "test", Guardrails: []string{"pii", "toxicity"}}}
		return cfg
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("registered guardrail names failed Validate: %v", err)
	}
	// A typo'd name fails at load, naming the key and the name.
	cfg := base()
	cfg.GatewayKeys[0].Guardrails = []string{"pii", "toxicty"}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("got nil, want an error naming the unknown guardrail")
	}
	if !strings.Contains(err.Error(), "toxicty") || !strings.Contains(err.Error(), "k1") {
		t.Errorf("error = %q, want it to name the key and the unknown guardrail", err.Error())
	}
	// A key with no guardrail list is fine.
	cfg = base()
	cfg.GatewayKeys[0].Guardrails = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("key without guardrails failed Validate: %v", err)
	}
	// Clone deep-copies the guardrail list.
	cfg = base()
	clone := cfg.Clone()
	clone.GatewayKeys[0].Guardrails[0] = "secret"
	if cfg.GatewayKeys[0].Guardrails[0] != "pii" {
		t.Errorf("Clone shared the Guardrails slice: original now %v", cfg.GatewayKeys[0].Guardrails)
	}
}

func ptrFloat(f float64) *float64 { return &f }
func ptrInt(i int) *int           { return &i }
