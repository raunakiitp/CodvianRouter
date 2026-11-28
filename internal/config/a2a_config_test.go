package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func a2aTestBoolPtr(b bool) *bool { return &b }

// A2A enable flag (M9.8): the A2A surface is OFF unless the operator
// opts in. The default must stay false — the sector-typical keyless-by-
// default A2A (GHSA-v54m-6rm3-p565) is never copied.

func TestA2AEnabledDefaultFalse(t *testing.T) {
	c := DefaultConfig(t.TempDir())
	if c.A2AEnabled {
		t.Fatal("DefaultConfig: A2AEnabled is true, want false (A2A is off unless the operator enables it)")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("DefaultConfig with A2AEnabled=false fails validation: %v", err)
	}
}

func TestA2AConfigDefaults(t *testing.T) {
	// DefaultConfig leaves A2A nil (defaults live in the
	// accessors, not in the literal — design §4).
	c := DefaultConfig(t.TempDir())
	if c.A2A != nil {
		t.Fatalf("DefaultConfig: A2A = %+v, want nil (defaults live in the accessors)", c.A2A)
	}
	var nilCfg *A2AConfig
	if got := nilCfg.EffectiveRetentionDays(); got != 7 {
		t.Fatalf("nil A2AConfig retention = %d, want 7", got)
	}
	if got := nilCfg.EffectiveMaxTasksPerKey(); got != 1000 {
		t.Fatalf("nil A2AConfig cap = %d, want 1000", got)
	}
	if !nilCfg.EffectivePruneEnabled() {
		t.Fatal("nil A2AConfig prune_enabled = false, want true")
	}
	// Zero values also mean defaults (0-means-default rule).
	zero := &A2AConfig{}
	if got := zero.EffectiveRetentionDays(); got != 7 {
		t.Fatalf("zero A2AConfig retention = %d, want 7", got)
	}
	if got := zero.EffectiveMaxTasksPerKey(); got != 1000 {
		t.Fatalf("zero A2AConfig cap = %d, want 1000", got)
	}
	if !zero.EffectivePruneEnabled() {
		t.Fatal("zero A2AConfig prune_enabled = false, want true")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("DefaultConfig (A2A nil) fails validation: %v", err)
	}
}

func TestA2AConfigExplicitValuesRoundTrip(t *testing.T) {
	c := DefaultConfig(t.TempDir())
	c.A2AEnabled = true
	c.A2A = &A2AConfig{RetentionDays: 30, MaxTasksPerKey: 50, PruneEnabled: a2aTestBoolPtr(false)}
	if got := c.A2A.EffectiveRetentionDays(); got != 30 {
		t.Fatalf("explicit retention = %d, want 30", got)
	}
	if got := c.A2A.EffectiveMaxTasksPerKey(); got != 50 {
		t.Fatalf("explicit cap = %d, want 50", got)
	}
	if c.A2A.EffectivePruneEnabled() {
		t.Fatal("explicit prune_enabled=false reads back true")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("explicit A2A config fails validation: %v", err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.A2A == nil {
		t.Fatal("JSON round-trip lost the a2a block")
	}
	if back.A2A.RetentionDays != 30 || back.A2A.MaxTasksPerKey != 50 {
		t.Fatalf("JSON round-trip a2a = %+v, want {30 50}", back.A2A)
	}
	if back.A2A.PruneEnabled == nil || *back.A2A.PruneEnabled {
		t.Fatalf("JSON round-trip prune_enabled = %v, want explicit false", back.A2A.PruneEnabled)
	}
	// Clone deep-copies the block (and its PruneEnabled
	// pointer): mutating the clone must not move the live one.
	clone := c.Clone()
	if clone.A2A == nil || clone.A2A.RetentionDays != 30 {
		t.Fatalf("Clone lost A2A: %+v", clone.A2A)
	}
	clone.A2A.RetentionDays = 1
	*clone.A2A.PruneEnabled = true
	if c.A2A.RetentionDays != 30 || *c.A2A.PruneEnabled {
		t.Fatalf("Clone mutation leaked into live config: %+v (prune_enabled=%v)", c.A2A, *c.A2A.PruneEnabled)
	}
}

func TestA2AConfigValidateRejectsNegatives(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *A2AConfig
		want string
	}{
		{"negative retention", &A2AConfig{RetentionDays: -1}, "a2a.retention_days"},
		{"negative cap", &A2AConfig{MaxTasksPerKey: -5}, "a2a.max_tasks_per_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig(t.TempDir())
			c.A2A = tc.cfg
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v, want an error naming %s", tc.cfg, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %q, want it to name %s", err.Error(), tc.want)
			}
		})
	}
}

func TestA2AEnabledOptIn(t *testing.T) {
	c := DefaultConfig(t.TempDir())
	c.A2AEnabled = true
	if err := c.Validate(); err != nil {
		t.Fatalf("A2AEnabled=true fails validation: %v", err)
	}
	// JSON round-trip: an enabled config stays enabled on reload,
	// and an omitted field reloads as false (omitempty + zero value).
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.A2AEnabled {
		t.Fatal("JSON round-trip lost A2AEnabled=true")
	}
	c.A2AEnabled = false
	raw, err = json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back2 Config
	if err := json.Unmarshal(raw, &back2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back2.A2AEnabled {
		t.Fatal("JSON round-trip of A2AEnabled=false came back true")
	}
}
