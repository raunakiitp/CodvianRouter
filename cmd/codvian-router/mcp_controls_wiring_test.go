package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/mcptools"
)

// CHECKLIST 392 (M9.6 follow-up): production registry
// construction (newMCPRegistry) — controls engaged iff the
// operator configured mcp.controls; legacy allow-all otherwise.

// countingToolServer is a ToolServer that counts dispatches and
// can be made to fail, so tests can prove a denied call never
// reached the server and a failed dispatch still consumed budget.
type countingToolServer struct {
	name  string
	tools []mcp.Tool
	calls int
	fail  error
}

func (s *countingToolServer) Name() string { return s.name }

func (s *countingToolServer) ListTools(context.Context) ([]mcp.Tool, error) {
	return s.tools, nil
}

func (s *countingToolServer) CallTool(_ context.Context, name string, _ map[string]any) (string, error) {
	s.calls++
	if s.fail != nil {
		return "", s.fail
	}
	return "ok:" + name, nil
}

func mcpWiringTestServers(t *testing.T) (*countingToolServer, []mcptools.ToolServer) {
	t.Helper()
	srv := &countingToolServer{name: "calc", tools: []mcp.Tool{
		mcptools.StaticTool("echo", "Echo", nil),
		mcptools.StaticTool("other", "Other", nil),
	}}
	return srv, []mcptools.ToolServer{srv}
}

func mcpWiringDiscover(t *testing.T, reg *mcptools.Registry) {
	t.Helper()
	reg.Discover(context.Background())
	if got := len(reg.Discovered()); got != 2 {
		t.Fatalf("discovered %d tools, want 2", got)
	}
}

func TestNewMCPRegistryUsesControlsWhenConfigured(t *testing.T) {
	srv, servers := mcpWiringTestServers(t)
	mc := &config.MCPConfig{Mode: "selector", MaxIterations: 10,
		Controls: &mcptools.Controls{Allow: []string{"calc__echo"}, MaxCalls: 10}}
	reg := newMCPRegistry(mc, servers)
	mcpWiringDiscover(t, reg)

	controls, set := reg.Controls()
	if !set {
		t.Fatal("Controls() reports controls NOT engaged, want engaged")
	}
	if len(controls.Allow) != 1 || controls.Allow[0] != "calc__echo" {
		t.Fatalf("Controls().Allow = %v, want [calc__echo]", controls.Allow)
	}
	if !reg.Snapshot().ControlsSet {
		t.Fatal("Snapshot().ControlsSet = false, want true")
	}
	if out, err := reg.Execute(context.Background(), "calc__echo", nil); err != nil || out != "ok:echo" {
		t.Fatalf("allowlisted execute = %q, %v; want ok:echo, nil", out, err)
	}
	if _, err := reg.Execute(context.Background(), "calc__other", nil); !errors.Is(err, mcptools.ErrToolNotAllowed) {
		t.Fatalf("non-allowlisted execute err = %v, want ErrToolNotAllowed", err)
	}
	if srv.calls != 1 {
		t.Fatalf("server dispatches = %d, want 1 (the denied call must never reach the server)", srv.calls)
	}
}

func TestNewMCPRegistryLegacyWhenControlsAbsent(t *testing.T) {
	_, servers := mcpWiringTestServers(t)
	mc := &config.MCPConfig{Mode: "selector", MaxIterations: 10}
	reg := newMCPRegistry(mc, servers)
	mcpWiringDiscover(t, reg)

	if _, set := reg.Controls(); set {
		t.Fatal("Controls() reports controls engaged with no controls section, want legacy")
	}
	if reg.Snapshot().ControlsSet {
		t.Fatal("Snapshot().ControlsSet = true with no controls section, want false")
	}
	for _, tool := range []string{"calc__echo", "calc__other"} {
		if _, err := reg.Execute(context.Background(), tool, nil); err != nil {
			t.Fatalf("legacy execute %s: %v, want nil (allow-all)", tool, err)
		}
	}
}

func TestNewMCPRegistryNilConfigIsLegacy(t *testing.T) {
	_, servers := mcpWiringTestServers(t)
	reg := newMCPRegistry(nil, servers)
	mcpWiringDiscover(t, reg)
	if _, set := reg.Controls(); set {
		t.Fatal("nil config engaged controls, want legacy allow-all")
	}
	if _, err := reg.Execute(context.Background(), "calc__echo", nil); err != nil {
		t.Fatalf("nil-config execute: %v, want nil", err)
	}
}

func TestNewMCPRegistryEmptyControlsDeniesAll(t *testing.T) {
	srv, servers := mcpWiringTestServers(t)
	mc := &config.MCPConfig{Mode: "selector", MaxIterations: 10,
		Controls: &mcptools.Controls{}}
	reg := newMCPRegistry(mc, servers)
	mcpWiringDiscover(t, reg)
	if _, err := reg.Execute(context.Background(), "calc__echo", nil); !errors.Is(err, mcptools.ErrToolNotAllowed) {
		t.Fatalf("empty-controls execute err = %v, want ErrToolNotAllowed (fail-closed)", err)
	}
	if srv.calls != 0 {
		t.Fatalf("server dispatches = %d, want 0 (denied before dispatch)", srv.calls)
	}
}

func TestNewMCPRegistryMaxCallsEnforced(t *testing.T) {
	_, servers := mcpWiringTestServers(t)
	mc := &config.MCPConfig{Mode: "selector", MaxIterations: 10,
		Controls: &mcptools.Controls{Allow: []string{"*"}, MaxCalls: 1}}
	reg := newMCPRegistry(mc, servers)
	mcpWiringDiscover(t, reg)
	if _, err := reg.Execute(context.Background(), "calc__echo", nil); err != nil {
		t.Fatalf("first execute: %v, want nil", err)
	}
	if _, err := reg.Execute(context.Background(), "calc__echo", nil); !errors.Is(err, mcptools.ErrBudgetExhausted) {
		t.Fatalf("second execute err = %v, want ErrBudgetExhausted", err)
	}
}

func TestNewMCPRegistryMaxDurationEnforced(t *testing.T) {
	_, servers := mcpWiringTestServers(t)
	mc := &config.MCPConfig{Mode: "selector", MaxIterations: 10,
		Controls: &mcptools.Controls{Allow: []string{"*"}, MaxDuration: time.Millisecond}}
	reg := newMCPRegistry(mc, servers)
	mcpWiringDiscover(t, reg)
	// Sleep past the budget (not a 1ns race: the sleep
	// guarantees the window is spent before the call).
	time.Sleep(2 * time.Millisecond)
	if _, err := reg.Execute(context.Background(), "calc__echo", nil); !errors.Is(err, mcptools.ErrBudgetExhausted) {
		t.Fatalf("execute with spent wall-clock budget err = %v, want ErrBudgetExhausted", err)
	}
}

// TestNewMCPRegistryFailedDispatchConsumesBudget pins the
// DOCUMENTED failed-dispatch rule (M9.6 review nit 1): the budget
// reservation is made before dispatch and is NOT refunded when
// the server call fails, so a failed dispatch counts as one spent
// call against MaxCalls. A refund would need a change to the
// mcptools choke point itself (outside this wiring slice).
func TestNewMCPRegistryFailedDispatchConsumesBudget(t *testing.T) {
	srv, servers := mcpWiringTestServers(t)
	srv.fail = errors.New("boom: server exploded")
	mc := &config.MCPConfig{Mode: "selector", MaxIterations: 10,
		Controls: &mcptools.Controls{Allow: []string{"*"}, MaxCalls: 1}}
	reg := newMCPRegistry(mc, servers)
	mcpWiringDiscover(t, reg)

	if _, err := reg.Execute(context.Background(), "calc__echo", nil); err == nil || errors.Is(err, mcptools.ErrBudgetExhausted) {
		t.Fatalf("failing execute err = %v, want the server error (not a budget error)", err)
	}
	if got := reg.BudgetStatus().CallsUsed; got != 1 {
		t.Fatalf("BudgetStatus().CallsUsed = %d after one failed dispatch, want 1 (failed dispatches consume budget)", got)
	}
	if _, err := reg.Execute(context.Background(), "calc__echo", nil); !errors.Is(err, mcptools.ErrBudgetExhausted) {
		t.Fatalf("execute after failed dispatch err = %v, want ErrBudgetExhausted (no refund)", err)
	}
}
