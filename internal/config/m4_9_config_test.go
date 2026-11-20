// M4.9 config surface: the routing failover knobs round-trip
// through JSON, pass Validate, and Clone deep-copies the pointer
// blocks; out-of-range values fail at load naming the field.
package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConfigM49RoundTrip: every M4.9 routing field survives a JSON
// marshal/unmarshal round-trip and passes Validate.
func TestConfigM49RoundTrip(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{
		MaxAttempts:              12,
		PerTargetTimeoutMs:       45000,
		PredictiveTtftMs:         800,
		PreferredFailoverAfterMs: 20000,
		ModelLockout: &ModelLockoutConfig{
			Enabled:        true,
			ErrorCodes:     []int{403, 404, 429},
			BaseCooldownMs: 120000,
			MaxCooldownMs:  1800000,
			MaxSteps:       10,
		},
		WaitBudgets: &WaitBudgetFileConfig{
			Enabled:           true,
			MaxWaitPerRetryMs: 90000,
			MaxRetries:        5,
			TotalBudgetMs:     300000,
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
	rc := back.Routing
	if rc == nil {
		t.Fatal("routing block lost in round-trip")
	}
	if rc.MaxAttempts != 12 || rc.PerTargetTimeoutMs != 45000 || rc.PredictiveTtftMs != 800 || rc.PreferredFailoverAfterMs != 20000 {
		t.Errorf("scalar knobs did not round-trip: %+v", rc)
	}
	if rc.ModelLockout == nil || !rc.ModelLockout.Enabled || rc.ModelLockout.BaseCooldownMs != 120000 || rc.ModelLockout.MaxCooldownMs != 1800000 || rc.ModelLockout.MaxSteps != 10 || len(rc.ModelLockout.ErrorCodes) != 3 || rc.ModelLockout.ErrorCodes[2] != 429 {
		t.Errorf("model_lockout did not round-trip: %+v", rc.ModelLockout)
	}
	if rc.WaitBudgets == nil || !rc.WaitBudgets.Enabled || rc.WaitBudgets.MaxWaitPerRetryMs != 90000 || rc.WaitBudgets.MaxRetries != 5 || rc.WaitBudgets.TotalBudgetMs != 300000 {
		t.Errorf("wait_budgets did not round-trip: %+v", rc.WaitBudgets)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round-tripped config failed Validate: %v", err)
	}
}

// TestConfigM49Validation: out-of-range M4.9 values fail at load,
// each naming its own field; the boundary values pass.
func TestConfigM49Validation(t *testing.T) {
	valid := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{
			MaxAttempts:              200,
			PerTargetTimeoutMs:       -1,
			PredictiveTtftMs:         0,
			PreferredFailoverAfterMs: 0,
		}
		return cfg
	}
	if err := valid().Validate(); err != nil {
		t.Errorf("boundary values should validate: %v", err)
	}
	cases := []struct {
		name string
		set  func(*RoutingConfig)
		want string
	}{
		{"max_attempts 0 passes (default)", func(rc *RoutingConfig) { rc.MaxAttempts = 0 }, ""},
		{"max_attempts 201", func(rc *RoutingConfig) { rc.MaxAttempts = 201 }, "max_attempts"},
		{"max_attempts -1", func(rc *RoutingConfig) { rc.MaxAttempts = -1 }, "max_attempts"},
		{"per_target_timeout -2", func(rc *RoutingConfig) { rc.PerTargetTimeoutMs = -2 }, "per_target_timeout_ms"},
		{"predictive_ttft -1", func(rc *RoutingConfig) { rc.PredictiveTtftMs = -1 }, "predictive_ttft_ms"},
		{"preferred_failover -1", func(rc *RoutingConfig) { rc.PreferredFailoverAfterMs = -1 }, "preferred_failover_after_ms"},
		{"lockout base negative", func(rc *RoutingConfig) { rc.ModelLockout = &ModelLockoutConfig{BaseCooldownMs: -1} }, "model_lockout.base_cooldown_ms"},
		{"lockout max negative", func(rc *RoutingConfig) { rc.ModelLockout = &ModelLockoutConfig{MaxCooldownMs: -1} }, "model_lockout.max_cooldown_ms"},
		{"lockout steps negative", func(rc *RoutingConfig) { rc.ModelLockout = &ModelLockoutConfig{MaxSteps: -1} }, "model_lockout.max_steps"},
		{"lockout code 99", func(rc *RoutingConfig) { rc.ModelLockout = &ModelLockoutConfig{ErrorCodes: []int{99}} }, "model_lockout.error_codes"},
		{"lockout code 600", func(rc *RoutingConfig) { rc.ModelLockout = &ModelLockoutConfig{ErrorCodes: []int{600}} }, "model_lockout.error_codes"},
		{"wait per-retry negative", func(rc *RoutingConfig) { rc.WaitBudgets = &WaitBudgetFileConfig{MaxWaitPerRetryMs: -1} }, "wait_budgets.max_wait_per_retry_ms"},
		{"wait retries negative", func(rc *RoutingConfig) { rc.WaitBudgets = &WaitBudgetFileConfig{MaxRetries: -1} }, "wait_budgets.max_retries"},
		{"wait total negative", func(rc *RoutingConfig) { rc.WaitBudgets = &WaitBudgetFileConfig{TotalBudgetMs: -1} }, "wait_budgets.total_budget_ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.set(cfg.Routing)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Errorf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected Validate to reject %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name field %q", err.Error(), tc.want)
			}
		})
	}
}

// TestConfigM49CloneDeepCopies: mutating a clone's lockout block
// (pointer, error-code slice) or wait-budget block must not reach
// the original.
func TestConfigM49CloneDeepCopies(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{
		ModelLockout: &ModelLockoutConfig{Enabled: true, ErrorCodes: []int{403, 429}, MaxSteps: 10},
		WaitBudgets:  &WaitBudgetFileConfig{Enabled: true, MaxRetries: 5},
	}
	clone := cfg.Clone()
	clone.Routing.ModelLockout.ErrorCodes[0] = 500
	clone.Routing.ModelLockout.MaxSteps = 1
	clone.Routing.WaitBudgets.MaxRetries = 1
	if cfg.Routing.ModelLockout.ErrorCodes[0] != 403 || cfg.Routing.ModelLockout.MaxSteps != 10 {
		t.Error("Clone shared ModelLockout with the original")
	}
	if cfg.Routing.WaitBudgets.MaxRetries != 5 {
		t.Error("Clone shared WaitBudgets with the original")
	}
}
