package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/mcptools"
)

// CHECKLIST 392 (M9.6 follow-up): mcp.controls wiring — config
// parse, validation, round-trip, Clone.

func mcpControlsBase(t *testing.T) *Config {
	t.Helper()
	c := DefaultConfig(t.TempDir())
	c.MCP = &MCPConfig{Mode: "selector", MaxIterations: 10, Servers: []MCPServerConfig{
		{Name: "calc", Command: "python3", Args: []string{"calc.py"}},
	}}
	return c
}

func TestMCPControlsAbsentIsLegacy(t *testing.T) {
	c := mcpControlsBase(t)
	if err := c.Validate(); err != nil {
		t.Fatalf("MCP without controls: %v", err)
	}
	if c.MCP.Controls != nil {
		t.Fatalf("Controls = %+v, want nil (absent = legacy allow-all)", c.MCP.Controls)
	}
	if c.MCP.ControlsEngaged() {
		t.Fatal("ControlsEngaged() = true with no controls section, want false")
	}
	var nilCfg *MCPConfig
	if nilCfg.ControlsEngaged() {
		t.Fatal("nil *MCPConfig ControlsEngaged() = true, want false")
	}
}

func TestMCPControlsLoadFromFile(t *testing.T) {
	// Full Load path: JSON on disk -> Unmarshal -> Validate.
	c := mcpControlsBase(t)
	c.MCP.Controls = &mcptools.Controls{
		Allow:       []string{"calc__echo", "calc__*"},
		Deny:        []string{"calc__danger"},
		MaxCalls:    25,
		MaxDuration: 5 * time.Minute,
	}
	path := t.TempDir() + "/config.json"
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.MCP == nil || loaded.MCP.Controls == nil {
		t.Fatal("loaded Controls is nil, want the configured controls")
	}
	got := loaded.MCP.Controls
	if len(got.Allow) != 2 || got.Allow[0] != "calc__echo" || got.Allow[1] != "calc__*" {
		t.Errorf("Allow = %v, want [calc__echo calc__*]", got.Allow)
	}
	if len(got.Deny) != 1 || got.Deny[0] != "calc__danger" {
		t.Errorf("Deny = %v, want [calc__danger]", got.Deny)
	}
	if got.MaxCalls != 25 {
		t.Errorf("MaxCalls = %d, want 25", got.MaxCalls)
	}
	if got.MaxDuration != 5*time.Minute {
		t.Errorf("MaxDuration = %s, want 5m0s", got.MaxDuration)
	}
	if !loaded.MCP.ControlsEngaged() {
		t.Error("ControlsEngaged() = false after loading a controls section, want true")
	}
}

func TestMCPControlsRoundTripMarshal(t *testing.T) {
	c := mcpControlsBase(t)
	c.MCP.Controls = &mcptools.Controls{Allow: []string{"*"}, MaxCalls: 3, MaxDuration: 90 * time.Second}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Duration is written as a human-readable string, not nanoseconds.
	if !strings.Contains(string(raw), `"max_duration":"1m30s"`) {
		t.Errorf("marshalled config lacks \"max_duration\":\"1m30s\": %s", raw)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("Validate after round-trip: %v", err)
	}
	got := back.MCP.Controls
	if got == nil || got.MaxDuration != 90*time.Second || got.MaxCalls != 3 ||
		len(got.Allow) != 1 || got.Allow[0] != "*" {
		t.Errorf("round-tripped Controls = %+v, want Allow=[*] MaxCalls=3 MaxDuration=1m30s", got)
	}
}

func TestMCPControlsNumericNanosecondsAccepted(t *testing.T) {
	// The verbatim encoding/json shape of time.Duration (a JSON
	// number of nanoseconds) still loads: 300000000000ns = 5m.
	raw := `{"version":1,"mcp":{"mode":"selector","max_iterations":10,` +
		`"servers":[{"name":"calc","command":"python3"}],` +
		`"controls":{"allow":["*"],"max_duration":300000000000}}}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("Unmarshal numeric max_duration: %v", err)
	}
	if c.MCP == nil || c.MCP.Controls == nil || c.MCP.Controls.MaxDuration != 5*time.Minute {
		t.Fatalf("MaxDuration = %v, want 5m0s", c.MCP.Controls)
	}
}

func TestMCPControlsEmptyPresentIsEnforcedNotError(t *testing.T) {
	// "controls": {} is present-but-empty: fail-closed deny-all
	// by design, NOT a validation error.
	raw := `{"version":1,"listen":"127.0.0.1:20128","mcp":{"mode":"selector",` +
		`"servers":[{"name":"calc","command":"python3"}],` +
		`"controls":{}}}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// The bare doc omits unrelated required fields; fill them
	// from a default so Validate exercises only the controls.
	c.DataDir = t.TempDir()
	if err := c.Validate(); err != nil {
		t.Fatalf("empty controls rejected, want valid (fail-closed deny-all): %v", err)
	}
	if c.MCP.Controls == nil || !c.MCP.ControlsEngaged() {
		t.Fatal("empty controls section must be present + engaged, got nil/false")
	}
}

func TestMCPControlsInvalidRejected(t *testing.T) {
	cases := []struct {
		name string
		// mutate applies the case to a base config; raw, when
		// non-empty, is decoded instead (parse-level cases).
		mutate func(c *MCPConfig)
		raw    string
		want   string // substring the error must contain
	}{
		{"negative max_calls", func(mc *MCPConfig) {
			mc.Controls = &mcptools.Controls{Allow: []string{"*"}, MaxCalls: -1}
		}, "", "mcp.controls.max_calls"},
		{"negative max_duration", func(mc *MCPConfig) {
			mc.Controls = &mcptools.Controls{Allow: []string{"*"}, MaxDuration: -time.Minute}
		}, "", "mcp.controls.max_duration"},
		{"blank allow entry", func(mc *MCPConfig) {
			mc.Controls = &mcptools.Controls{Allow: []string{"calc__echo", "  "}}
		}, "", "mcp.controls.allow[1]"},
		{"empty deny entry", func(mc *MCPConfig) {
			mc.Controls = &mcptools.Controls{Allow: []string{"*"}, Deny: []string{""}}
		}, "", "mcp.controls.deny[0]"},
		{"bad duration string", nil,
			`{"mcp":{"servers":[{"name":"calc","command":"x"}],` +
				`"controls":{"allow":["*"],"max_duration":"5x"}}}`,
			"mcp.controls.max_duration"},
		{"boolean max_duration", nil,
			`{"mcp":{"servers":[{"name":"calc","command":"x"}],` +
				`"controls":{"allow":["*"],"max_duration":true}}}`,
			"mcp.controls.max_duration"},
		{"float max_duration", nil,
			`{"mcp":{"servers":[{"name":"calc","command":"x"}],` +
				`"controls":{"allow":["*"],"max_duration":1.5}}}`,
			"mcp.controls.max_duration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.raw != "" {
				var c Config
				err = json.Unmarshal([]byte(tc.raw), &c)
				if err == nil {
					err = c.Validate()
				}
			} else {
				c := mcpControlsBase(t)
				tc.mutate(c.MCP)
				err = c.Validate()
			}
			if err == nil {
				t.Fatalf("invalid controls accepted, want an error naming %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %s", err.Error(), tc.want)
			}
		})
	}
}

func TestMCPControlsCloneDeepCopies(t *testing.T) {
	c := mcpControlsBase(t)
	c.MCP.Controls = &mcptools.Controls{
		Allow: []string{"calc__echo"}, Deny: []string{"calc__no"},
		MaxCalls: 5, MaxDuration: time.Minute,
	}
	cp := c.Clone()
	if cp.MCP.Controls == c.MCP.Controls {
		t.Fatal("Clone shares the Controls pointer with the original")
	}
	cp.MCP.Controls.Allow[0] = "changed"
	cp.MCP.Controls.Deny[0] = "changed"
	cp.MCP.Controls.MaxCalls = 99
	if c.MCP.Controls.Allow[0] != "calc__echo" || c.MCP.Controls.Deny[0] != "calc__no" ||
		c.MCP.Controls.MaxCalls != 5 {
		t.Fatalf("Clone shared Controls state with the original: %+v", c.MCP.Controls)
	}
	// Absent controls stay absent through Clone.
	c2 := mcpControlsBase(t)
	if c2.Clone().MCP.Controls != nil {
		t.Fatal("Clone invented Controls for a config without them")
	}
}
