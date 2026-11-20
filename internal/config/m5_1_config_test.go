// M5.1 config surface: key_budgets window fields round-trip
// through JSON, pass Validate, and Clone deep-copies the limit
// pointers; out-of-range values fail at load naming the field.
package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigKeyBudgetWindowsRoundTrip(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.GatewayKeys = []GatewayKey{{ID: "k1", Name: "k1", KeyHash: "h1"}}
	daily, weekly, monthly := 5.0, 20.0, 50.0
	cfg.KeyBudgets = map[string]KeyBudgetConfig{
		"k1": {
			LimitUSD:         100,
			UnpricedPolicy:   "fail_closed",
			DailyLimitUSD:    &daily,
			WeeklyLimitUSD:   &weekly,
			MonthlyLimitUSD:  &monthly,
			WarningThreshold: 0.5,
			ResetInterval:    "weekly",
			ResetTime:        "09:30",
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	b := back.KeyBudgets["k1"]
	if b.DailyLimitUSD == nil || *b.DailyLimitUSD != 5 ||
		b.WeeklyLimitUSD == nil || *b.WeeklyLimitUSD != 20 ||
		b.MonthlyLimitUSD == nil || *b.MonthlyLimitUSD != 50 ||
		b.WarningThreshold != 0.5 || b.ResetInterval != "weekly" || b.ResetTime != "09:30" {
		t.Errorf("window fields did not round-trip: %+v", b)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round-tripped config failed Validate: %v", err)
	}
	clone := cfg.Clone()
	*clone.KeyBudgets["k1"].DailyLimitUSD = 99
	if *cfg.KeyBudgets["k1"].DailyLimitUSD != 5 {
		t.Error("Clone shared KeyBudgetConfig limit pointers with the original")
	}
}

func TestConfigKeyBudgetWindowsValidation(t *testing.T) {
	valid := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.GatewayKeys = []GatewayKey{{ID: "k1", Name: "k1", KeyHash: "h1"}}
		cfg.KeyBudgets = map[string]KeyBudgetConfig{"k1": {LimitUSD: 100}}
		return cfg
	}
	cases := []struct {
		name string
		set  func(*KeyBudgetConfig)
		want string
	}{
		{"daily negative", func(b *KeyBudgetConfig) { v := -1.0; b.DailyLimitUSD = &v }, "daily_limit_usd"},
		{"weekly negative", func(b *KeyBudgetConfig) { v := -1.0; b.WeeklyLimitUSD = &v }, "weekly_limit_usd"},
		{"monthly negative", func(b *KeyBudgetConfig) { v := -1.0; b.MonthlyLimitUSD = &v }, "monthly_limit_usd"},
		{"interval bogus", func(b *KeyBudgetConfig) { b.ResetInterval = "fortnightly" }, "reset_interval"},
		{"reset time 25:00", func(b *KeyBudgetConfig) { b.ResetTime = "25:00" }, "reset_time"},
		{"reset time 9:30", func(b *KeyBudgetConfig) { b.ResetTime = "9:30" }, "reset_time"},
		{"warning threshold 1.5", func(b *KeyBudgetConfig) { b.WarningThreshold = 1.5 }, "warning_threshold"},
		{"warning threshold -0.1", func(b *KeyBudgetConfig) { b.WarningThreshold = -0.1 }, "warning_threshold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			b := cfg.KeyBudgets["k1"]
			tc.set(&b)
			cfg.KeyBudgets["k1"] = b
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected Validate to reject %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name field %q", err.Error(), tc.want)
			}
		})
	}
}

func TestConfigKeyRequestLimitsValidation(t *testing.T) {
	valid := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.GatewayKeys = []GatewayKey{{ID: "k1", Name: "k1", KeyHash: "h1", RequestsPerDay: 100,
			RateLimits: []RateLimitRule{{Limit: 10, WindowSeconds: 60}}}}
		return cfg
	}
	if err := valid().Validate(); err != nil {
		t.Errorf("valid RPD + rule failed Validate: %v", err)
	}
	cases := []struct {
		name string
		set  func(*GatewayKey)
		want string
	}{
		{"RPD negative", func(k *GatewayKey) { k.RequestsPerDay = -1 }, "requests_per_day"},
		{"rule limit 0", func(k *GatewayKey) { k.RateLimits[0].Limit = 0 }, "rate_limits[0].limit"},
		{"rule limit negative", func(k *GatewayKey) { k.RateLimits[0].Limit = -5 }, "rate_limits[0].limit"},
		{"rule window 0", func(k *GatewayKey) { k.RateLimits[0].WindowSeconds = 0 }, "rate_limits[0].window_seconds"},
		{"rule window negative", func(k *GatewayKey) { k.RateLimits[0].WindowSeconds = -60 }, "rate_limits[0].window_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			k := cfg.GatewayKeys[0]
			tc.set(&k)
			cfg.GatewayKeys[0] = k
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected Validate to reject %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err.Error(), tc.want)
			}
		})
	}
}

// M5.3: per-key token limits — config validation + Clone.
func TestConfigKeyTokenLimitsValidation(t *testing.T) {
	valid := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		enabled := true
		cfg.GatewayKeys = []GatewayKey{{ID: "k1", Name: "k1", KeyHash: "h1", TokenLimits: []TokenLimit{
			{Scope: "global", Limit: 1000, ResetInterval: "daily"},
			{Scope: "model", ScopeValue: "gpt-4o", Limit: 100, ResetInterval: "weekly", ResetTime: "09:30", Enabled: &enabled},
		}}}
		return cfg
	}
	if err := valid().Validate(); err != nil {
		t.Errorf("valid token limits failed Validate: %v", err)
	}
	cases := []struct {
		name string
		set  func(*TokenLimit)
		want string
	}{
		{"scope empty", func(tl *TokenLimit) { tl.Scope = "" }, "token_limits[0].scope"},
		{"scope bogus", func(tl *TokenLimit) { tl.Scope = "tenant" }, "token_limits[0].scope"},
		{"model without value", func(tl *TokenLimit) { tl.Scope = "model"; tl.ScopeValue = "" }, "scope_value"},
		{"provider without value", func(tl *TokenLimit) { tl.Scope = "provider"; tl.ScopeValue = "" }, "scope_value"},
		{"global with value", func(tl *TokenLimit) { tl.ScopeValue = "gpt-4o" }, "scope_value"},
		{"limit 0", func(tl *TokenLimit) { tl.Limit = 0 }, "token_limits[0].limit"},
		{"limit negative", func(tl *TokenLimit) { tl.Limit = -5 }, "token_limits[0].limit"},
		{"interval bogus", func(tl *TokenLimit) { tl.ResetInterval = "fortnightly" }, "reset_interval"},
		{"reset time 25:00", func(tl *TokenLimit) { tl.ResetTime = "25:00" }, "reset_time"},
		{"reset time 9:30", func(tl *TokenLimit) { tl.ResetTime = "9:30" }, "reset_time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tl := cfg.GatewayKeys[0].TokenLimits[0]
			tc.set(&tl)
			cfg.GatewayKeys[0].TokenLimits[0] = tl
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected Validate to reject %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err.Error(), tc.want)
			}
		})
	}
	// Clone deep-copies the limits and the Enabled pointer.
	cfg := valid()
	clone := cfg.Clone()
	clone.GatewayKeys[0].TokenLimits[0].Limit = 999
	*clone.GatewayKeys[0].TokenLimits[1].Enabled = false
	if cfg.GatewayKeys[0].TokenLimits[0].Limit != 1000 {
		t.Error("Clone shared TokenLimits slice with the original")
	}
	if !*cfg.GatewayKeys[0].TokenLimits[1].Enabled {
		t.Error("Clone shared TokenLimit.Enabled pointer with the original")
	}
}
