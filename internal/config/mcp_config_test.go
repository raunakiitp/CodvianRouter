package config

import (
	"testing"
)

// MCP selector mode (CHECKLIST 141): config validation + Clone.

func TestMCPConfigValidate(t *testing.T) {
	base := func() *Config {
		c := DefaultConfig(t.TempDir())
		c.MCP = &MCPConfig{Mode: "selector", MaxIterations: 10, Servers: []MCPServerConfig{
			{Name: "calc", Command: "python3", Args: []string{"calc.py"}},
		}}
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid MCP config: %v", err)
	}
	// Nil block is valid (no servers configured).
	c := base()
	c.MCP = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("nil MCP block: %v", err)
	}
	// Empty mode means selector.
	c = base()
	c.MCP.Mode = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("empty mode: %v", err)
	}
	c = base()
	c.MCP.Mode = "Direct"
	if err := c.Validate(); err != nil {
		t.Fatalf("Direct mode: %v", err)
	}
	c = base()
	c.MCP.Mode = "bogus"
	if err := c.Validate(); err == nil {
		t.Fatal("bogus mode accepted, want a validation error naming mcp.mode")
	}
	c = base()
	c.MCP.MaxIterations = 51
	if err := c.Validate(); err == nil {
		t.Fatal("max_iterations 51 accepted, want a validation error")
	}
	c = base()
	c.MCP.MaxIterations = 50
	if err := c.Validate(); err != nil {
		t.Fatalf("max_iterations 50: %v", err)
	}
	c = base()
	c.MCP.Servers = append(c.MCP.Servers, MCPServerConfig{Name: "calc", Command: "other"})
	if err := c.Validate(); err == nil {
		t.Fatal("duplicate server name accepted, want a validation error")
	}
	c = base()
	c.MCP.Servers[0].Name = ""
	if err := c.Validate(); err == nil {
		t.Fatal("empty server name accepted, want a validation error")
	}
	c = base()
	c.MCP.Servers[0].Command = ""
	if err := c.Validate(); err == nil {
		t.Fatal("empty server command accepted, want a validation error")
	}
}

func TestMCPConfigEffective(t *testing.T) {
	if got := (&MCPConfig{}).EffectiveMode(); got != "selector" {
		t.Errorf("empty mode = %q, want selector", got)
	}
	if got := (&MCPConfig{Mode: "DIRECT"}).EffectiveMode(); got != "direct" {
		t.Errorf("DIRECT = %q, want direct", got)
	}
	if got := (&MCPConfig{}).EffectiveMaxIterations(); got != 10 {
		t.Errorf("unset max iterations = %d, want 10", got)
	}
	if got := (&MCPConfig{MaxIterations: 4}).EffectiveMaxIterations(); got != 4 {
		t.Errorf("max iterations 4 = %d, want 4", got)
	}
}

func TestMCPConfigCloneDeepCopies(t *testing.T) {
	c := DefaultConfig(t.TempDir())
	c.MCP = &MCPConfig{Mode: "direct", MaxIterations: 7, Servers: []MCPServerConfig{
		{Name: "s", Command: "cmd", Args: []string{"a"}},
	}}
	cp := c.Clone()
	cp.MCP.Mode = "selector"
	cp.MCP.Servers[0].Name = "changed"
	cp.MCP.Servers[0].Args[0] = "changed"
	if c.MCP.Mode != "direct" || c.MCP.Servers[0].Name != "s" || c.MCP.Servers[0].Args[0] != "a" {
		t.Fatalf("Clone shared MCP state with the original: %+v", c.MCP)
	}
}
