package config

import (
	"strings"
	"testing"
)

// TestPluginsConfigValidation proves the plugins block only accepts
// known built-in names, rejects empties and duplicates, and that an
// absent block is valid (no plugins = gateway unchanged).
func TestPluginsConfigValidation(t *testing.T) {
	base := func() *Config {
		c := &Config{Version: CurrentVersion, Listen: "127.0.0.1:20128", DataDir: t.TempDir()}
		return c
	}
	// Absent block: valid.
	if err := base().Validate(); err != nil {
		t.Fatalf("absent plugins block: %v", err)
	}
	// Known built-ins, in order: valid.
	c := base()
	c.Plugins = &PluginsConfig{Enabled: []string{"response-log", "header-add"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("built-ins: %v", err)
	}
	// Unknown name: rejected, naming it.
	c = base()
	c.Plugins = &PluginsConfig{Enabled: []string{"evil-plugin"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "evil-plugin") {
		t.Fatalf("err = %v, want unknown name named", err)
	}
	// Empty entry: rejected.
	c = base()
	c.Plugins = &PluginsConfig{Enabled: []string{""}}
	if err := c.Validate(); err == nil {
		t.Fatal("empty plugin entry accepted")
	}
	// Duplicate: rejected.
	c = base()
	c.Plugins = &PluginsConfig{Enabled: []string{"header-add", "header-add"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "header-add") {
		t.Fatalf("err = %v, want duplicate named", err)
	}
	// A plugin binary path is not a field: it must survive as an
	// unknown extension, never as a plugin — the block has no path
	// field at all, so there is nothing to smuggle a path through.
	c = base()
	c.Plugins = &PluginsConfig{Enabled: []string{"header-add"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid block: %v", err)
	}
	// Cost-cap (CHECKLIST 239): a cap with the plugin enabled is
	// valid; a negative cap is rejected; a cap without the plugin
	// enabled is rejected (it would silently do nothing).
	cap := 0.01
	c = base()
	c.Plugins = &PluginsConfig{Enabled: []string{"cost-cap"}, CostCapUSD: &cap}
	if err := c.Validate(); err != nil {
		t.Fatalf("cost-cap with cap: %v", err)
	}
	neg := -0.01
	c = base()
	c.Plugins = &PluginsConfig{Enabled: []string{"cost-cap"}, CostCapUSD: &neg}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "cost_cap_usd_per_request") {
		t.Fatalf("err = %v, want negative cap named", err)
	}
	c = base()
	c.Plugins = &PluginsConfig{Enabled: []string{"response-log"}, CostCapUSD: &cap}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "cost-cap") {
		t.Fatalf("err = %v, want cap-without-plugin named", err)
	}
}
