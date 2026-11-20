package config

import "testing"

// M5.11: internal scoped budgets — config validation + Clone.
func TestConfigInternalBudgetsValidation(t *testing.T) {
	valid := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.GatewayKeys = []GatewayKey{{ID: "k1", Name: "k1", KeyHash: "h1"}}
		cfg.QuotaPools = []QuotaPoolConfig{{
			ID: "pool1", Name: "Pool 1",
			Dimensions:  []QuotaPoolDimensionConfig{{Unit: "requests", Window: "daily", Limit: 100}},
			Allocations: []QuotaPoolAllocationConfig{{KeyID: "k1", Weight: 50, Policy: "hard"}},
		}}
		cfg.InternalBudgets = []InternalBudgetConfig{{
			ID: "b1", Scope: "global", Period: "daily", LimitType: "currency", LimitValue: 5, WarningThreshold: 0.8, Enabled: true,
		}}
		return cfg
	}
	cases := []struct {
		name string
		set  func(*Config)
		ok   bool
	}{
		{"valid global", func(c *Config) {}, true},
		{"valid provider", func(c *Config) { c.InternalBudgets[0].Scope = "provider"; c.InternalBudgets[0].ProviderID = "openai" }, true},
		{"valid model", func(c *Config) { c.InternalBudgets[0].Scope = "model"; c.InternalBudgets[0].ModelID = "gpt-4o" }, true},
		{"valid pool", func(c *Config) { c.InternalBudgets[0].Scope = "pool"; c.InternalBudgets[0].PoolID = "pool1" }, true},
		{"valid tokens weekly", func(c *Config) {
			c.InternalBudgets[0].Period = "weekly"
			c.InternalBudgets[0].LimitType = "tokens"
			c.InternalBudgets[0].LimitValue = 1000
		}, true},
		{"valid requests monthly", func(c *Config) {
			c.InternalBudgets[0].Period = "monthly"
			c.InternalBudgets[0].LimitType = "requests"
			c.InternalBudgets[0].LimitValue = 100
		}, true},
		{"zero threshold means default", func(c *Config) { c.InternalBudgets[0].WarningThreshold = 0 }, true},
		{"empty id", func(c *Config) { c.InternalBudgets[0].ID = "" }, false},
		{"duplicate id", func(c *Config) { c.InternalBudgets = append(c.InternalBudgets, c.InternalBudgets[0]) }, false},
		{"bad scope", func(c *Config) { c.InternalBudgets[0].Scope = "key" }, false},
		{"provider without id", func(c *Config) { c.InternalBudgets[0].Scope = "provider" }, false},
		{"model without id", func(c *Config) { c.InternalBudgets[0].Scope = "model" }, false},
		{"pool without id", func(c *Config) { c.InternalBudgets[0].Scope = "pool" }, false},
		{"pool unknown", func(c *Config) { c.InternalBudgets[0].Scope = "pool"; c.InternalBudgets[0].PoolID = "nope" }, false},
		{"bad period", func(c *Config) { c.InternalBudgets[0].Period = "hourly" }, false},
		{"bad limit type", func(c *Config) { c.InternalBudgets[0].LimitType = "usd" }, false},
		{"zero limit", func(c *Config) { c.InternalBudgets[0].LimitValue = 0 }, false},
		{"negative limit", func(c *Config) { c.InternalBudgets[0].LimitValue = -5 }, false},
		{"threshold 1.5", func(c *Config) { c.InternalBudgets[0].WarningThreshold = 1.5 }, false},
		{"threshold -0.1", func(c *Config) { c.InternalBudgets[0].WarningThreshold = -0.1 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.set(c)
			if err := c.Validate(); (err == nil) != tc.ok {
				t.Errorf("Validate() err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
