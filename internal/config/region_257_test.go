package config

// Region-aware failover order (CHECKLIST 257): validation for
// operator-declared regions and model equivalents — both are
// operator facts, never guessed, and both fail loudly at load
// when malformed.

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func region257Config(t *testing.T) *Config {
	t.Helper()
	c := DefaultConfig(t.TempDir())
	c.Providers = []ProviderConfig{{
		ID: "prov-a", Name: "A", BaseURL: "https://a.example/v1",
		Enabled: true,
	}}
	return c
}

func TestValidateModelEquivalents(t *testing.T) {
	t.Run("valid equivalents pass", func(t *testing.T) {
		c := region257Config(t)
		c.Routing = &RoutingConfig{ModelEquivalents: map[string][]string{
			"gpt-4o": {"claude-sonnet-4", "llama-4-maverick"},
		}}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate = %v, want nil", err)
		}
	})
	t.Run("self equivalent rejected", func(t *testing.T) {
		c := region257Config(t)
		c.Routing = &RoutingConfig{ModelEquivalents: map[string][]string{
			"gpt-4o": {"gpt-4o"},
		}}
		if err := c.Validate(); err == nil {
			t.Fatalf("Validate = nil, want an error naming the self-equivalent")
		}
	})
	t.Run("duplicate equivalent rejected", func(t *testing.T) {
		c := region257Config(t)
		c.Routing = &RoutingConfig{ModelEquivalents: map[string][]string{
			"gpt-4o": {"claude-sonnet-4", "claude-sonnet-4"},
		}}
		if err := c.Validate(); err == nil {
			t.Fatalf("Validate = nil, want an error naming the duplicate")
		}
	})
	t.Run("empty key and empty entry rejected", func(t *testing.T) {
		c := region257Config(t)
		c.Routing = &RoutingConfig{ModelEquivalents: map[string][]string{"": {"x"}}}
		if err := c.Validate(); err == nil {
			t.Fatalf("Validate = nil, want an error for the empty model key")
		}
		c = region257Config(t)
		c.Routing = &RoutingConfig{ModelEquivalents: map[string][]string{"gpt-4o": {""}}}
		if err := c.Validate(); err == nil {
			t.Fatalf("Validate = nil, want an error for the empty equivalent")
		}
	})
}

func TestValidateRegions(t *testing.T) {
	t.Run("declared regions pass", func(t *testing.T) {
		c := region257Config(t)
		c.Providers[0].Region = "us-east-1"
		c.Providers[0].Models = []provider.Model{{ID: "m", Region: "eu-west-1"}}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate = %v, want nil", err)
		}
	})
	t.Run("whitespace region rejected", func(t *testing.T) {
		c := region257Config(t)
		c.Providers[0].Region = "us east 1"
		if err := c.Validate(); err == nil {
			t.Fatalf("Validate = nil, want an error naming the provider region")
		}
		c = region257Config(t)
		c.Providers[0].Models = []provider.Model{{ID: "m", Region: "eu west"}}
		if err := c.Validate(); err == nil {
			t.Fatalf("Validate = nil, want an error naming the model region")
		}
	})
}
