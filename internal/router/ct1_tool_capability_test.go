// CT-1 — direct-path tool-capability pre-check and
// capability-mismatch 400 failover (GAP-TRACK). The auto path was
// already strict (ModelHasCapability); these tests prove the
// direct path now is too.
package router

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// toolMismatch400 is the 400 an upstream returns when the target
// model does not support tool / function calls.
func toolMismatch400() error {
	return &provider.UpstreamError{StatusCode: 400, Body: "This model does not support tool use"}
}

func toolReq(model string) provider.ChatRequest {
	req := coolReq(model)
	req.Tools = []provider.ToolDefinition{{Type: "function"}}
	return req
}

// capabilityHook builds a Config.ToolCapability from a
// providerID -> (capable, known) table for tests.
func capabilityHook(table map[string][2]bool) func(string, string) (bool, bool) {
	return func(providerID, _ string) (bool, bool) {
		if v, ok := table[providerID]; ok {
			return v[0], v[1]
		}
		return false, false
	}
}

// TestCT1ToolPrecheckSkipsIncapableModel: a tool-bearing request
// never dials a confirmed-incapable model — the chain skips it
// (no attempt spent) and the confirmed-capable target serves.
func TestCT1ToolPrecheckSkipsIncapableModel(t *testing.T) {
	ctx := context.Background()
	a := errClient(&provider.UpstreamError{StatusCode: 500, Body: "must never be dialled"})
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{
		HedgeDelay: -1,
		ToolCapability: capabilityHook(map[string][2]bool{
			"a": {false, true}, // confirmed incapable
			"b": {true, true},  // confirmed capable
		}),
	})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), toolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("chat = %v, %v, want bee + nil", resp, err)
	}
	if got := a.chatCalls(); got != 0 {
		t.Fatalf("incapable provider A called %d times, want 0 (pre-check skips it)", got)
	}
	if got := b.chatCalls(); got != 1 {
		t.Fatalf("capable provider B called %d times, want 1", got)
	}
}

// TestCT1CapabilityMismatch400FailsOver: the target model's
// capability is UNKNOWN (pre-check cannot skip it), it 400s on
// tool support — the request fails over to the confirmed-capable
// target instead of ending in a terminal 400.
func TestCT1CapabilityMismatch400FailsOver(t *testing.T) {
	ctx := context.Background()
	a := errClient(toolMismatch400())
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{
		HedgeDelay: -1,
		ToolCapability: capabilityHook(map[string][2]bool{
			"a": {false, false}, // unknown: pre-check fails open
			"b": {true, true},   // confirmed capable
		}),
	})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), toolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("chat = %v, %v, want bee + nil (mismatch 400 must fail over, not end terminal)", resp, err)
	}
	if got := a.chatCalls(); got != 1 {
		t.Fatalf("provider A called %d times, want 1", got)
	}
	if got := b.chatCalls(); got != 1 {
		t.Fatalf("provider B called %d times, want 1", got)
	}
}

// TestCT1CapableModelProceedsDirectly: a confirmed-capable model
// serves a tool-bearing request on the first attempt — no skip,
// no failover.
func TestCT1CapableModelProceedsDirectly(t *testing.T) {
	ctx := context.Background()
	a := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{
		HedgeDelay: -1,
		ToolCapability: capabilityHook(map[string][2]bool{
			"a": {true, true},
		}),
	})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a"), toolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("chat = %v, %v, want bee + nil", resp, err)
	}
	if got := a.chatCalls(); got != 1 {
		t.Fatalf("provider A called %d times, want 1", got)
	}
}

// TestCT1Mismatch400TerminalWithoutCapableTarget: when the chain
// holds NO confirmed-capable target, a capability-mismatch 400
// stays terminal — the next (unknown) target is never dialled on
// a guess, and the client still sees the real provider error.
func TestCT1Mismatch400TerminalWithoutCapableTarget(t *testing.T) {
	ctx := context.Background()
	a := errClient(toolMismatch400())
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{
		HedgeDelay: -1,
		ToolCapability: capabilityHook(map[string][2]bool{
			"a": {false, false}, // unknown
			"b": {false, false}, // unknown: no confirmed-capable target
		}),
	})
	_, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), toolReq("m"))
	if err == nil {
		t.Fatalf("want the capability-mismatch error, got nil")
	}
	if got := b.chatCalls(); got != 0 {
		t.Fatalf("provider B called %d times, want 0 (no confirmed-capable target, 400 stays terminal)", got)
	}
}

// TestCT1NonToolRequestIgnoresCapability: the pre-check applies
// only to tool-bearing requests — a plain request still dials a
// confirmed-incapable model (it may serve text fine).
func TestCT1NonToolRequestIgnoresCapability(t *testing.T) {
	ctx := context.Background()
	a := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{
		HedgeDelay: -1,
		ToolCapability: capabilityHook(map[string][2]bool{
			"a": {false, true}, // confirmed incapable of tools
		}),
	})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("chat = %v, %v, want bee + nil (plain request must still dial the model)", resp, err)
	}
	if got := a.chatCalls(); got != 1 {
		t.Fatalf("provider A called %d times, want 1", got)
	}
}

// TestCT1SingleFailoverOnly: the mismatch failover is spent
// exactly ONCE per request — when the confirmed-capable failover
// target ALSO 400s on tool support, the chain stops there; the
// third confirmed-capable target is never dialled.
func TestCT1SingleFailoverOnly(t *testing.T) {
	ctx := context.Background()
	a := errClient(toolMismatch400())
	b := errClient(toolMismatch400())
	c := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, nil, Config{
		HedgeDelay:             -1,
		MaxProvidersPerRequest: 3,
		ToolCapability: capabilityHook(map[string][2]bool{
			"a": {false, false}, // unknown
			"b": {true, true},   // confirmed capable (still 400s: flag was wrong)
			"c": {true, true},   // confirmed capable — must NOT be tried
		}),
	})
	_, _, err := eng.ChatWithChain(ctx, chainFor("a", "b", "c"), toolReq("m"))
	if err == nil {
		t.Fatalf("want the capability-mismatch error, got nil")
	}
	if got := c.chatCalls(); got != 0 {
		t.Fatalf("provider C called %d times, want 0 (failover is single-use)", got)
	}
	if got := b.chatCalls(); got != 1 {
		t.Fatalf("provider B called %d times, want 1", got)
	}
}

// TestIsCapabilityMismatchErr: only a 400 whose body names
// tool/function-call support counts — other 400s, non-400s and
// nil do not.
func TestIsCapabilityMismatchErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"400 tool use not supported", &provider.UpstreamError{StatusCode: 400, Body: "This model does not support tool use"}, true},
		{"400 function calling not supported", &provider.UpstreamError{StatusCode: 400, Body: "Function calling is not supported for this model"}, true},
		{"400 unsupported tools param", &provider.UpstreamError{StatusCode: 400, Body: "Unsupported parameter: 'tools' is not supported with this model"}, true},
		{"400 tools not supported", &provider.UpstreamError{StatusCode: 400, Body: "tools are not supported by gpt-3.5-turbo"}, true},
		{"400 other", &provider.UpstreamError{StatusCode: 400, Body: "bad request: temperature out of range"}, false},
		{"400 model not found", &provider.UpstreamError{StatusCode: 400, Body: "model 'ghost-9' not found"}, false},
		{"500 tool text", &provider.UpstreamError{StatusCode: 500, Body: "tool use failed upstream"}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isCapabilityMismatchErr(tc.err); got != tc.want {
			t.Errorf("%s: isCapabilityMismatchErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestToolCapabilityFunc: the registry adapter reads the stored
// tri-state flag — confirmed capable / confirmed incapable /
// unknown — and fails open (unknown) for an unknown provider,
// an unknown model, and a nil registry. Never a guess.
func TestToolCapabilityFunc(t *testing.T) {
	if capable, known := ToolCapabilityFunc(nil)("p1", "m1"); capable || known {
		t.Fatalf("nil registry: (%v, %v), want (false, false)", capable, known)
	}
	dir := t.TempDir()
	raw := `[{"id":"p1","name":"P One","base_url":"https://example.invalid","wire_format":"openai","enabled":true,"models":[
		{"id":"m-capable","tools":true},
		{"id":"m-incapable","tools":false},
		{"id":"m-unknown"}
	]}]`
	if err := os.WriteFile(filepath.Join(dir, "providers.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("write store: %v", err)
	}
	reg, err := provider.NewFileRegistry(filepath.Join(dir, "providers.json"))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	fn := ToolCapabilityFunc(reg)
	cases := []struct {
		providerID, model string
		wantCapable       bool
		wantKnown         bool
	}{
		{"p1", "m-capable", true, true},
		{"p1", "m-incapable", false, true},
		{"p1", "m-unknown", false, false},
		{"p1", "no-such-model", false, false},
		{"no-such-provider", "m-capable", false, false},
	}
	for _, tc := range cases {
		if capable, known := fn(tc.providerID, tc.model); capable != tc.wantCapable || known != tc.wantKnown {
			t.Errorf("(%s, %s): (%v, %v), want (%v, %v)",
				tc.providerID, tc.model, capable, known, tc.wantCapable, tc.wantKnown)
		}
	}
}
