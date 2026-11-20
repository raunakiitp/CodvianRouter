package config

import "testing"

// M5.9: quota-schedule + webhook config validation.
func TestConfigQuotaSchedulesValidation(t *testing.T) {
	validPool := QuotaPoolConfig{
		ID: "pool1", Name: "Pool 1",
		Dimensions:  []QuotaPoolDimensionConfig{{Unit: "requests", Window: "daily", Limit: 100}},
		Allocations: []QuotaPoolAllocationConfig{{KeyID: "k1", Weight: 50, Policy: "hard"}},
	}
	validSched := QuotaScheduleConfig{
		ID: "s1", PoolID: "pool1", Days: []int{1, 2, 3}, StartMinute: 540, EndMinute: 1020,
		Mode: "allow", Priority: 1, Enabled: true,
	}
	valid := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.GatewayKeys = []GatewayKey{{ID: "k1", Name: "k1", KeyHash: "h1"}}
		cfg.QuotaPools = []QuotaPoolConfig{validPool}
		cfg.QuotaSchedules = []QuotaScheduleConfig{validSched}
		return cfg
	}
	f90 := 90.0
	cases := []struct {
		name string
		mut  func(c *Config)
	}{
		{"valid", func(c *Config) {}},
		{"empty id", func(c *Config) { c.QuotaSchedules[0].ID = "" }},
		{"duplicate id", func(c *Config) { c.QuotaSchedules = append(c.QuotaSchedules, c.QuotaSchedules[0]) }},
		{"empty pool", func(c *Config) { c.QuotaSchedules[0].PoolID = "" }},
		{"unknown pool", func(c *Config) { c.QuotaSchedules[0].PoolID = "nope" }},
		{"day 7", func(c *Config) { c.QuotaSchedules[0].Days = []int{7} }},
		{"day -1", func(c *Config) { c.QuotaSchedules[0].Days = []int{-1} }},
		{"start 1440", func(c *Config) { c.QuotaSchedules[0].StartMinute = 1440 }},
		{"end 0", func(c *Config) { c.QuotaSchedules[0].EndMinute = 0 }},
		{"end 1441", func(c *Config) { c.QuotaSchedules[0].EndMinute = 1441 }},
		{"bad mode", func(c *Config) { c.QuotaSchedules[0].Mode = "maybe" }},
		{"bad timezone", func(c *Config) { c.QuotaSchedules[0].Timezone = "No/Such_Zone" }},
		{"good timezone", func(c *Config) { c.QuotaSchedules[0].Timezone = "Asia/Kolkata" }},
		{"reserve bad window", func(c *Config) { c.QuotaSchedules[0].Reserves = []ScheduleReserve{{Window: "yearly", Percent: 20}} }},
		{"reserve percent 101", func(c *Config) { c.QuotaSchedules[0].Reserves = []ScheduleReserve{{Window: "daily", Percent: 101}} }},
		{"reserve window any ok", func(c *Config) { c.QuotaSchedules[0].Reserves = []ScheduleReserve{{Window: "any", Percent: 20}} }},
		{"budget zero", func(c *Config) { c.QuotaSchedules[0].BudgetValue = new(float64) }},
		{"budget without unit", func(c *Config) { c.QuotaSchedules[0].BudgetValue = &f90 }},
		{"budget bad unit", func(c *Config) {
			c.QuotaSchedules[0].BudgetValue = &f90
			c.QuotaSchedules[0].BudgetUnit = "percent"
			c.QuotaSchedules[0].BudgetWindow = "daily"
		}},
		{"budget bad window", func(c *Config) {
			c.QuotaSchedules[0].BudgetValue = &f90
			c.QuotaSchedules[0].BudgetUnit = "requests"
			c.QuotaSchedules[0].BudgetWindow = "yearly"
		}},
		{"budget unit without value", func(c *Config) { c.QuotaSchedules[0].BudgetUnit = "requests" }},
		{"budget valid", func(c *Config) {
			c.QuotaSchedules[0].BudgetValue = &f90
			c.QuotaSchedules[0].BudgetUnit = "requests"
			c.QuotaSchedules[0].BudgetWindow = "daily"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mut(c)
			err := c.Validate()
			// The "valid"/"good timezone"/"reserve window any ok"/
			// "budget valid" cases must pass; the rest must fail.
			wantOK := tc.name == "valid" || tc.name == "good timezone" || tc.name == "reserve window any ok" || tc.name == "budget valid"
			if wantOK && err != nil {
				t.Errorf("want valid, got %v", err)
			}
			if !wantOK && err == nil {
				t.Error("want a validation error, got nil")
			}
		})
	}
}

func TestConfigWebhooksValidation(t *testing.T) {
	cases := []struct {
		name string
		wh   []WebhookConfig
		ok   bool
	}{
		{"none", nil, true},
		{"valid", []WebhookConfig{{ID: "ops", URL: "https://example.com/hook", Events: []string{"quota.exceeded"}}}, true},
		{"valid budget.warning", []WebhookConfig{{ID: "ops", URL: "https://example.com/hook", Events: []string{"budget.warning"}}}, true},
		{"no events (subscribes to nothing)", []WebhookConfig{{ID: "ops", URL: "https://example.com/hook"}}, true},
		{"empty id", []WebhookConfig{{ID: "", URL: "https://example.com/hook"}}, false},
		{"duplicate id", []WebhookConfig{{ID: "a", URL: "https://a.example.com"}, {ID: "a", URL: "https://b.example.com"}}, false},
		{"ftp url", []WebhookConfig{{ID: "a", URL: "ftp://example.com/hook"}}, false},
		{"no host", []WebhookConfig{{ID: "a", URL: "https:///hook"}}, false},
		{"unknown event", []WebhookConfig{{ID: "a", URL: "https://example.com/hook", Events: []string{"usage.daily"}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig(t.TempDir())
			c.Webhooks = tc.wh
			if err := c.Validate(); (err == nil) != tc.ok {
				t.Errorf("Validate() err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestConfigQuotaSchedulesCloneDeepCopies(t *testing.T) {
	budget := 90.0
	c := DefaultConfig(t.TempDir())
	c.QuotaSchedules = []QuotaScheduleConfig{{
		ID: "s1", PoolID: "pool1", Days: []int{1, 2}, StartMinute: 540, EndMinute: 1020,
		Mode: "allow", Enabled: true,
		Reserves: []ScheduleReserve{{Window: "daily", Percent: 20}}, BudgetValue: &budget, BudgetUnit: "requests", BudgetWindow: "daily",
	}}
	c.Webhooks = []WebhookConfig{{ID: "ops", URL: "https://example.com/hook", Events: []string{"quota.exceeded"}}}
	clone := c.Clone()
	clone.QuotaSchedules[0].Days[0] = 6
	clone.QuotaSchedules[0].Reserves[0].Percent = 99
	*clone.QuotaSchedules[0].BudgetValue = 1
	clone.Webhooks[0].Events[0] = "other"
	if c.QuotaSchedules[0].Days[0] != 1 || c.QuotaSchedules[0].Reserves[0].Percent != 20 || *c.QuotaSchedules[0].BudgetValue != 90 || c.Webhooks[0].Events[0] != "quota.exceeded" {
		t.Errorf("clone shares storage with the original: %+v / %+v", c.QuotaSchedules[0], c.Webhooks[0])
	}
}
