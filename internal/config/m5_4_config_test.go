package config

import (
	"strings"
	"testing"
)

// M5.4: fair-share quota pools — config validation + Clone.
func TestConfigQuotaPoolsValidation(t *testing.T) {
	valid := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.GatewayKeys = []GatewayKey{
			{ID: "k1", Name: "k1", KeyHash: "h1"},
			{ID: "k2", Name: "k2", KeyHash: "h2"},
		}
		cap := 10.0
		cfg.QuotaPools = []QuotaPoolConfig{{
			ID: "pool1", Name: "team", SaturationThreshold: 0.5,
			Dimensions: []QuotaPoolDimensionConfig{
				{Unit: "requests", Window: "daily", Limit: 100},
			},
			Allocations: []QuotaPoolAllocationConfig{
				{KeyID: "k1", Weight: 50, Policy: "hard"},
				{KeyID: "k2", Weight: 50, Policy: "soft", CapValue: &cap, CapUnit: "requests"},
			},
		}}
		return cfg
	}
	if err := valid().Validate(); err != nil {
		t.Errorf("valid pool failed Validate: %v", err)
	}
	cases := []struct {
		name string
		set  func(*Config)
		want string
	}{
		{"empty id", func(c *Config) { c.QuotaPools[0].ID = "" }, "empty id"},
		{"duplicate id", func(c *Config) { c.QuotaPools = append(c.QuotaPools, c.QuotaPools[0]) }, "duplicate id"},
		{"empty name", func(c *Config) { c.QuotaPools[0].Name = "" }, "name must not be empty"},
		{"threshold 1.5", func(c *Config) { c.QuotaPools[0].SaturationThreshold = 1.5 }, "saturation_threshold"},
		{"threshold -0.1", func(c *Config) { c.QuotaPools[0].SaturationThreshold = -0.1 }, "saturation_threshold"},
		{"account_count -1", func(c *Config) { c.QuotaPools[0].AccountCount = -1 }, "account_count"},
		{"no dimensions", func(c *Config) { c.QuotaPools[0].Dimensions = nil }, "at least one dimension"},
		{"unit percent", func(c *Config) { c.QuotaPools[0].Dimensions[0].Unit = "percent" }, "dimensions[0].unit"},
		{"unit bogus", func(c *Config) { c.QuotaPools[0].Dimensions[0].Unit = "bytes" }, "dimensions[0].unit"},
		{"window bogus", func(c *Config) { c.QuotaPools[0].Dimensions[0].Window = "yearly" }, "dimensions[0].window"},
		{"limit 0", func(c *Config) { c.QuotaPools[0].Dimensions[0].Limit = 0 }, "dimensions[0].limit"},
		{"duplicate dimension", func(c *Config) {
			c.QuotaPools[0].Dimensions = append(c.QuotaPools[0].Dimensions, c.QuotaPools[0].Dimensions[0])
		}, "duplicate dimension"},
		{"allocation empty key", func(c *Config) { c.QuotaPools[0].Allocations[0].KeyID = "" }, "empty key_id"},
		{"allocation unknown key", func(c *Config) { c.QuotaPools[0].Allocations[0].KeyID = "ghost" }, "does not exist"},
		{"allocation key twice", func(c *Config) {
			c.QuotaPools[0].Allocations = append(c.QuotaPools[0].Allocations, c.QuotaPools[0].Allocations[0])
		}, "allocates key"},
		{"weight 101", func(c *Config) { c.QuotaPools[0].Allocations[0].Weight = 101 }, "weight must be between 0 and 100"},
		{"policy bogus", func(c *Config) { c.QuotaPools[0].Allocations[0].Policy = "weird" }, "policy"},
		{"cap 0", func(c *Config) { v := 0.0; c.QuotaPools[0].Allocations[0].CapValue = &v }, "cap_value must be positive"},
		{"cap unit bogus", func(c *Config) {
			v := 5.0
			c.QuotaPools[0].Allocations[0].CapValue = &v
			c.QuotaPools[0].Allocations[0].CapUnit = "percent"
		}, "cap_unit"},
		{"cap unit without value", func(c *Config) { c.QuotaPools[0].Allocations[0].CapUnit = "requests" }, "cap_unit must be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.set(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected Validate to reject %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
	// Clone deep-copies the pool slices and the cap pointer.
	cfg := valid()
	clone := cfg.Clone()
	clone.QuotaPools[0].Dimensions[0].Limit = 999
	clone.QuotaPools[0].Allocations[1].Weight = 1
	*clone.QuotaPools[0].Allocations[1].CapValue = 999
	if cfg.QuotaPools[0].Dimensions[0].Limit != 100 {
		t.Error("Clone shared QuotaPools dimensions with the original")
	}
	if cfg.QuotaPools[0].Allocations[1].Weight != 50 || *cfg.QuotaPools[0].Allocations[1].CapValue != 10 {
		t.Error("Clone shared QuotaPools allocations/cap with the original")
	}
}
