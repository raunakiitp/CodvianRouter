package mcptools

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// countingServer is a StaticServer whose handler counts every
// dispatch — the proof that a denied call never reached the server.
type countingServer struct {
	*StaticServer
	calls atomic.Int64
	sleep time.Duration
}

func newCountingServer(name string, tools ...string) *countingServer {
	cs := &countingServer{}
	toolList := make([]mcp.Tool, 0, len(tools))
	handlers := map[string]func(context.Context, map[string]any) (string, error){}
	for _, tn := range tools {
		toolList = append(toolList, StaticTool(tn, "test tool "+tn, map[string]any{}))
		name := tn
		handlers[name] = func(_ context.Context, _ map[string]any) (string, error) {
			cs.calls.Add(1)
			if cs.sleep > 0 {
				time.Sleep(cs.sleep)
			}
			return "called " + name, nil
		}
	}
	cs.StaticServer = NewStaticServer(name, toolList, handlers)
	return cs
}

// usageServer is a ToolServer that reports token usage (and, when
// configured, a USD cost) for every call — the stand-in for a
// future server that actually knows its usage.
type usageServer struct {
	*StaticServer
	usage    ToolUsage
	calls    atomic.Int64
	nilUsage bool
}

func newUsageServer(name, tool string, usage ToolUsage, nilUsage bool) *usageServer {
	us := &usageServer{usage: usage, nilUsage: nilUsage}
	handlers := map[string]func(context.Context, map[string]any) (string, error){
		tool: func(_ context.Context, _ map[string]any) (string, error) {
			us.calls.Add(1)
			return "used " + tool, nil
		},
	}
	us.StaticServer = NewStaticServer(name, []mcp.Tool{StaticTool(tool, "usage tool", map[string]any{})}, handlers)
	return us
}

func (s *usageServer) CallToolWithUsage(ctx context.Context, name string, args map[string]any) (string, *ToolUsage, error) {
	out, err := s.StaticServer.CallTool(ctx, name, args)
	if err != nil {
		return "", nil, err
	}
	if s.nilUsage {
		return out, nil, nil
	}
	u := s.usage
	return out, &u, nil
}

func hardenedRegistry(t *testing.T, c Controls, servers ...ToolServer) *Registry {
	t.Helper()
	r := NewWithControls(servers, ModeSelector, 0, c)
	r.Discover(context.Background())
	return r
}

func TestAllowlistAllowedToolExecutes(t *testing.T) {
	cs := newCountingServer("calc", "echo")
	r := hardenedRegistry(t, Controls{Allow: []string{"calc__echo"}}, cs)
	out, err := r.Execute(context.Background(), "calc__echo", nil)
	if err != nil || out != "called echo" {
		t.Fatalf("allowed execute = %q, %v, want called echo", out, err)
	}
	if cs.calls.Load() != 1 {
		t.Fatalf("server calls = %d, want 1", cs.calls.Load())
	}
}

func TestAllowlistUnlistedToolDeniedBeforeDispatch(t *testing.T) {
	cs := newCountingServer("calc", "echo", "secret")
	r := hardenedRegistry(t, Controls{Allow: []string{"calc__echo"}}, cs)
	_, err := r.Execute(context.Background(), "calc__secret", nil)
	if !errors.Is(err, ErrToolNotAllowed) {
		t.Fatalf("unlisted execute error = %v, want ErrToolNotAllowed", err)
	}
	if cs.calls.Load() != 0 {
		t.Fatalf("server was contacted %d times for a denied tool, want 0", cs.calls.Load())
	}
	if got := r.Stats().AllowlistDenied; got != 1 {
		t.Fatalf("allowlist_denied = %d, want 1", got)
	}
}

func TestAllowlistDenyByDefault(t *testing.T) {
	cs := newCountingServer("calc", "echo")
	// Zero Controls on the hardened constructor: empty Allow
	// denies everything.
	r := hardenedRegistry(t, Controls{}, cs)
	if _, err := r.Execute(context.Background(), "calc__echo", nil); !errors.Is(err, ErrToolNotAllowed) {
		t.Fatalf("zero-controls execute error = %v, want ErrToolNotAllowed (deny by default)", err)
	}
	// Same via the runtime API on a legacy registry.
	r2 := New([]ToolServer{cs}, ModeSelector, 0)
	r2.Discover(context.Background())
	if _, err := r2.Execute(context.Background(), "calc__echo", nil); err != nil {
		t.Fatalf("legacy registry before SetControls: %v, want success", err)
	}
	r2.SetControls(Controls{})
	if _, err := r2.Execute(context.Background(), "calc__echo", nil); !errors.Is(err, ErrToolNotAllowed) {
		t.Fatalf("SetControls(empty) execute error = %v, want ErrToolNotAllowed", err)
	}
	if cs.calls.Load() != 1 {
		t.Fatalf("server calls = %d, want 1 (only the pre-controls call)", cs.calls.Load())
	}
}

func TestAllowlistWildcardMatches(t *testing.T) {
	cs := newCountingServer("calc", "echo", "add")
	other := newCountingServer("fs", "read")
	// Server wildcard.
	r := hardenedRegistry(t, Controls{Allow: []string{"calc__*"}}, cs, other)
	if _, err := r.Execute(context.Background(), "calc__add", nil); err != nil {
		t.Fatalf("calc__* should allow calc__add: %v", err)
	}
	if _, err := r.Execute(context.Background(), "fs__read", nil); !errors.Is(err, ErrToolNotAllowed) {
		t.Fatalf("calc__* should deny fs__read: %v", err)
	}
	// Global wildcard.
	r2 := hardenedRegistry(t, Controls{Allow: []string{"*"}}, cs, other)
	if _, err := r2.Execute(context.Background(), "fs__read", nil); err != nil {
		t.Fatalf("* should allow fs__read: %v", err)
	}
	// Plain tool name matches on any server.
	r3 := hardenedRegistry(t, Controls{Allow: []string{"read"}}, cs, other)
	if _, err := r3.Execute(context.Background(), "fs__read", nil); err != nil {
		t.Fatalf("plain name should allow fs__read: %v", err)
	}
	if _, err := r3.Execute(context.Background(), "calc__echo", nil); !errors.Is(err, ErrToolNotAllowed) {
		t.Fatalf("plain name read should deny calc__echo: %v", err)
	}
}

func TestAllowlistDenyOverridesAllow(t *testing.T) {
	cs := newCountingServer("calc", "echo", "wipe")
	r := hardenedRegistry(t, Controls{Allow: []string{"calc__*"}, Deny: []string{"calc__wipe"}}, cs)
	if _, err := r.Execute(context.Background(), "calc__echo", nil); err != nil {
		t.Fatalf("allowed tool denied: %v", err)
	}
	if _, err := r.Execute(context.Background(), "calc__wipe", nil); !errors.Is(err, ErrToolNotAllowed) {
		t.Fatalf("deny-listed tool error = %v, want ErrToolNotAllowed", err)
	}
	if cs.calls.Load() != 1 {
		t.Fatalf("server calls = %d, want 1 (wipe never dispatched)", cs.calls.Load())
	}
}

func TestBudgetCallCountCapRefusesNPlusOne(t *testing.T) {
	cs := newCountingServer("calc", "echo")
	r := hardenedRegistry(t, Controls{Allow: []string{"*"}, MaxCalls: 2}, cs)
	for i := 0; i < 2; i++ {
		if _, err := r.Execute(context.Background(), "calc__echo", nil); err != nil {
			t.Fatalf("call %d: %v, want success", i+1, err)
		}
	}
	_, err := r.Execute(context.Background(), "calc__echo", nil)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("3rd call error = %v, want ErrBudgetExhausted", err)
	}
	if cs.calls.Load() != 2 {
		t.Fatalf("server calls = %d, want 2 (3rd never dispatched)", cs.calls.Load())
	}
	if got := r.Stats().BudgetDenied; got != 1 {
		t.Fatalf("budget_denied = %d, want 1", got)
	}
	if !r.BudgetStatus().Exhausted {
		t.Fatalf("BudgetStatus.Exhausted = false after cap hit, want true")
	}
}

func TestBudgetWallClockRefusesAfterExpiry(t *testing.T) {
	cs := newCountingServer("calc", "echo")
	// Nanosecond budget: already spent before the first call.
	r := hardenedRegistry(t, Controls{Allow: []string{"*"}, MaxDuration: time.Nanosecond}, cs)
	// Let the clock advance past the 1 ns budget before the
	// first Execute: on Windows two time.Now calls a few
	// hundred ns apart can land in the same QPC tick, so
	// time.Since(budgetWindowStart) reads 0 and the budget
	// looks unspent. The sleep makes "already spent" true on
	// every clock; the production check is unchanged.
	time.Sleep(2 * time.Millisecond)
	if _, err := r.Execute(context.Background(), "calc__echo", nil); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("expired-budget error = %v, want ErrBudgetExhausted", err)
	}
	if cs.calls.Load() != 0 {
		t.Fatalf("server calls = %d, want 0", cs.calls.Load())
	}
	// Slow-handler variant: a 25 ms budget survives the first
	// 40 ms call (checked before dispatch) but not the second.
	slow := newCountingServer("calc", "echo")
	slow.sleep = 40 * time.Millisecond
	r2 := hardenedRegistry(t, Controls{Allow: []string{"*"}, MaxDuration: 25 * time.Millisecond}, slow)
	if _, err := r2.Execute(context.Background(), "calc__echo", nil); err != nil {
		t.Fatalf("first call within budget: %v", err)
	}
	if _, err := r2.Execute(context.Background(), "calc__echo", nil); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second call after wall-clock spent: %v, want ErrBudgetExhausted", err)
	}
	if slow.calls.Load() != 1 {
		t.Fatalf("slow server calls = %d, want 1", slow.calls.Load())
	}
}

func TestBudgetResetRestartsWindow(t *testing.T) {
	cs := newCountingServer("calc", "echo")
	r := hardenedRegistry(t, Controls{Allow: []string{"*"}, MaxCalls: 1}, cs)
	if _, err := r.Execute(context.Background(), "calc__echo", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := r.Execute(context.Background(), "calc__echo", nil); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("call after cap: %v, want ErrBudgetExhausted", err)
	}
	r.ResetBudget()
	if _, err := r.Execute(context.Background(), "calc__echo", nil); err != nil {
		t.Fatalf("call after ResetBudget: %v, want success", err)
	}
	if cs.calls.Load() != 2 {
		t.Fatalf("server calls = %d, want 2", cs.calls.Load())
	}
}

func TestCostUsageAbsentIsUnknown(t *testing.T) {
	cs := newCountingServer("calc", "echo")
	r := hardenedRegistry(t, Controls{Allow: []string{"*"}}, cs)
	res, err := r.ExecuteDetailed(context.Background(), "calc__echo", nil)
	if err != nil {
		t.Fatalf("ExecuteDetailed: %v", err)
	}
	if res.Cost.UsageKnown || res.Cost.USDKnown {
		t.Fatalf("plain-server cost = %+v, want UNKNOWN (UsageKnown=false, USDKnown=false)", res.Cost)
	}
	if res.Text != "called echo" || res.Server != "calc" || res.Tool != "echo" {
		t.Fatalf("result = %+v, want calc/echo text", res)
	}
}

func TestCostUsagePresentReportsTokensWithoutUSD(t *testing.T) {
	us := newUsageServer("llm", "summarise", ToolUsage{InputTokens: 120, OutputTokens: 40}, false)
	r := hardenedRegistry(t, Controls{Allow: []string{"*"}}, us)
	res, err := r.ExecuteDetailed(context.Background(), "llm__summarise", nil)
	if err != nil {
		t.Fatalf("ExecuteDetailed: %v", err)
	}
	if !res.Cost.UsageKnown || res.Cost.InputTokens != 120 || res.Cost.OutputTokens != 40 {
		t.Fatalf("cost = %+v, want usage 120/40 known", res.Cost)
	}
	if res.Cost.USDKnown {
		t.Fatalf("cost = %+v, want USD UNKNOWN when server reports no price", res.Cost)
	}
}

func TestCostServerReportedUSDIsReported(t *testing.T) {
	us := newUsageServer("llm", "summarise", ToolUsage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.0025, HasCost: true}, false)
	r := hardenedRegistry(t, Controls{Allow: []string{"*"}}, us)
	res, err := r.ExecuteDetailed(context.Background(), "llm__summarise", nil)
	if err != nil {
		t.Fatalf("ExecuteDetailed: %v", err)
	}
	if !res.Cost.USDKnown || res.Cost.USD != 0.0025 {
		t.Fatalf("cost = %+v, want USD 0.0025 known", res.Cost)
	}
}

func TestCostNilUsageFromReportingServerIsUnknown(t *testing.T) {
	us := newUsageServer("llm", "summarise", ToolUsage{}, true)
	r := hardenedRegistry(t, Controls{Allow: []string{"*"}}, us)
	res, err := r.ExecuteDetailed(context.Background(), "llm__summarise", nil)
	if err != nil {
		t.Fatalf("ExecuteDetailed: %v", err)
	}
	if res.Cost.UsageKnown || res.Cost.USDKnown {
		t.Fatalf("nil-usage cost = %+v, want UNKNOWN", res.Cost)
	}
}

func TestAgentLoopEnforcesAllowlist(t *testing.T) {
	cs := newCountingServer("calc", "echo")
	// The model tries a tool that is NOT allowlisted via
	// mcp_tools_execute: the loop must surface the denial as a
	// tool result and never contact the server.
	other := newCountingServer("fs", "wipe")
	r2 := NewWithControls([]ToolServer{cs, other}, ModeSelector, 0, Controls{Allow: []string{"calc__echo"}})
	r2.Discover(context.Background())
	chat := &scriptedChat{responses: []*provider.ChatResponse{
		toolCallResponse("c1", MetaExecuteTool, `{"tool":"fs__wipe","arguments":{}}`),
		textResponse("denied, understood"),
	}}
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "wipe it"}}}
	resp, _, err := r2.RunAgentLoop(context.Background(), chat.Chat, req)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	if resp.Content != "denied, understood" {
		t.Fatalf("final content = %q", resp.Content)
	}
	if other.calls.Load() != 0 {
		t.Fatalf("denied server contacted %d times, want 0", other.calls.Load())
	}
	// The denial reached the model as a tool result.
	toolMsg := chat.requests[1].Messages[len(chat.requests[1].Messages)-1]
	if !strings.Contains(toolMsg.Content, "not allowlisted") {
		t.Fatalf("tool result to model = %q, want a not-allowlisted denial", toolMsg.Content)
	}
}

func TestSetControlsRuntimeAPIAndSnapshot(t *testing.T) {
	cs := newCountingServer("calc", "echo")
	r := New([]ToolServer{cs}, ModeSelector, 0)
	r.Discover(context.Background())
	if _, ok := r.Controls(); ok {
		t.Fatalf("legacy registry ControlsSet = true, want false")
	}
	r.SetControls(Controls{Allow: []string{"calc__echo"}, MaxCalls: 5, MaxDuration: time.Minute})
	c, ok := r.Controls()
	if !ok || len(c.Allow) != 1 || c.MaxCalls != 5 {
		t.Fatalf("Controls() = %+v, %v, want allow 1 / max 5 / set", c, ok)
	}
	snap := r.Snapshot()
	if !snap.ControlsSet || snap.Budget.MaxCalls != 5 || snap.Budget.MaxDuration != time.Minute {
		t.Fatalf("snapshot = %+v, want controls engaged with budget 5/1m", snap)
	}
	if snap.Stats.AllowlistDenied != 0 || snap.Stats.BudgetDenied != 0 {
		t.Fatalf("snapshot stats = %+v, want zero denials", snap.Stats)
	}
}

func TestWildcardMatchTable(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "anything", true},
		{"calc__*", "calc__echo", true},
		{"calc__*", "fs__echo", false},
		{"*__echo", "calc__echo", true},
		{"calc__e*o", "calc__echo", true},
		{"echo", "echo", true},
		{"echo", "echo2", false},
		{"", "echo", false},
		{"calc__echo", "calc__echo", true},
	}
	for _, tc := range cases {
		if got := wildcardMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}
