package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// staticServerWithTools builds a StaticServer with n tools named
// tool-0..tool-(n-1), each echoing its arguments as JSON.
func staticServerWithTools(t *testing.T, name string, n int) *StaticServer {
	t.Helper()
	tools := make([]mcp.Tool, 0, n)
	handlers := map[string]func(ctx context.Context, args map[string]any) (string, error){}
	for i := 0; i < n; i++ {
		toolName := fmt.Sprintf("tool-%d", i)
		tools = append(tools, StaticTool(toolName, "test tool "+toolName,
			map[string]any{"x": map[string]any{"type": "string"}}))
		tn := toolName
		handlers[tn] = func(_ context.Context, args map[string]any) (string, error) {
			raw, _ := json.Marshal(args)
			return "called " + tn + " with " + string(raw), nil
		}
	}
	return NewStaticServer(name, tools, handlers)
}

func TestSelectorInjectsExactlyTwoMetaTools(t *testing.T) {
	// 1 server with 3 tools, 3 servers with 5 tools each, 10
	// servers with 4 tools each: selector always injects exactly
	// the two meta-tools, no matter how many tools exist.
	cases := []struct {
		servers int
		tools   int
	}{
		{servers: 1, tools: 3},
		{servers: 3, tools: 5},
		{servers: 10, tools: 4},
	}
	for _, tc := range cases {
		servers := make([]ToolServer, 0, tc.servers)
		for i := 0; i < tc.servers; i++ {
			servers = append(servers, staticServerWithTools(t, fmt.Sprintf("srv-%d", i), tc.tools))
		}
		r := New(servers, ModeSelector, 0)
		r.Discover(context.Background())
		total := tc.servers * tc.tools
		if got := len(r.Discovered()); got != total {
			t.Fatalf("servers=%d tools=%d: discovered %d, want %d", tc.servers, tc.tools, got, total)
		}
		req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
		if added := r.Inject(&req); added != 2 {
			t.Fatalf("servers=%d tools=%d: Inject added %d, want exactly 2", tc.servers, tc.tools, added)
		}
		if len(req.Tools) != 2 {
			t.Fatalf("servers=%d: request has %d tools, want 2", tc.servers, len(req.Tools))
		}
		if req.Tools[0].Function.Name != MetaGetTool || req.Tools[1].Function.Name != MetaExecuteTool {
			t.Fatalf("selector tools = %q, %q, want %q, %q",
				req.Tools[0].Function.Name, req.Tools[1].Function.Name, MetaGetTool, MetaExecuteTool)
		}
		if got := len(MetaTools()); got != 2 {
			t.Fatalf("MetaTools() = %d, want 2", got)
		}
	}
}

func TestDirectInjectsAllTools(t *testing.T) {
	servers := []ToolServer{
		staticServerWithTools(t, "alpha", 3),
		staticServerWithTools(t, "beta", 2),
	}
	r := New(servers, ModeDirect, 0)
	r.Discover(context.Background())
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	if added := r.Inject(&req); added != 5 {
		t.Fatalf("direct Inject added %d, want 5 (all discovered tools)", added)
	}
	names := map[string]bool{}
	for _, td := range req.Tools {
		names[td.Function.Name] = true
	}
	for _, want := range []string{"alpha__tool-0", "alpha__tool-1", "alpha__tool-2", "beta__tool-0", "beta__tool-1"} {
		if !names[want] {
			t.Errorf("direct tools missing %q (have %v)", want, names)
		}
	}
	// The meta-tools are NOT injected in direct mode.
	if names[MetaGetTool] || names[MetaExecuteTool] {
		t.Errorf("direct mode injected a meta-tool; direct injects discovered tools only")
	}
}

func TestInjectNoServersInjectsNothing(t *testing.T) {
	r := New(nil, ModeSelector, 0)
	req := provider.ChatRequest{Model: "m"}
	if added := r.Inject(&req); added != 0 || len(req.Tools) != 0 {
		t.Fatalf("no servers: Inject added %d, want 0 (nothing to select from)", added)
	}
}

func TestInjectNeverDuplicates(t *testing.T) {
	r := New([]ToolServer{staticServerWithTools(t, "s", 2)}, ModeSelector, 0)
	r.Discover(context.Background())
	req := provider.ChatRequest{Model: "m", Tools: MetaTools()}
	if added := r.Inject(&req); added != 0 {
		t.Fatalf("client-supplied meta-tools: Inject added %d, want 0 (no duplicates)", added)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(req.Tools))
	}
}

func TestModeNormalisation(t *testing.T) {
	if got := New(nil, "", 0).Mode(); got != ModeSelector {
		t.Errorf("empty mode = %q, want selector", got)
	}
	if got := New(nil, "bogus", 0).Mode(); got != ModeSelector {
		t.Errorf("unknown mode = %q, want selector", got)
	}
	if got := New(nil, "DIRECT", 0).Mode(); got != ModeDirect {
		t.Errorf("DIRECT mode = %q, want direct", got)
	}
	if got := New(nil, ModeSelector, 0).MaxIterations(); got != DefaultMaxIterations {
		t.Errorf("default max iterations = %d, want %d", got, DefaultMaxIterations)
	}
	if got := New(nil, ModeSelector, 999).MaxIterations(); got != MaxIterationsLimit {
		t.Errorf("oversize max iterations = %d, want clamped to %d", got, MaxIterationsLimit)
	}
}

func TestDiscoverDownServerDoesNotHideHealthyOnes(t *testing.T) {
	down := NewStaticServer("down", nil, nil)
	// A server whose ListTools fails: wrap StaticServer's success
	// with a failing one via a tiny local implementation.
	failing := &failingServer{name: "failing"}
	r := New([]ToolServer{failing, down, staticServerWithTools(t, "ok", 2)}, ModeSelector, 0)
	_ = down
	r.Discover(context.Background())
	if got := len(r.Discovered()); got != 2 {
		t.Fatalf("discovered = %d, want 2 from the healthy server", got)
	}
	st := r.Statuses()
	if len(st) != 3 {
		t.Fatalf("statuses = %d, want 3", len(st))
	}
	if st[0].OK || st[0].LastError == "" {
		t.Errorf("failing server status = ok=%v err=%q, want ok=false with an error", st[0].OK, st[0].LastError)
	}
	if !st[2].OK || st[2].Tools != 2 {
		t.Errorf("healthy server status = %+v, want ok with 2 tools", st[2])
	}
}

type failingServer struct{ name string }

func (f *failingServer) Name() string { return f.name }
func (f *failingServer) ListTools(_ context.Context) ([]mcp.Tool, error) {
	return nil, fmt.Errorf("connection refused (test)")
}
func (f *failingServer) CallTool(_ context.Context, _ string, _ map[string]any) (string, error) {
	return "", fmt.Errorf("connection refused (test)")
}

// scriptedChat is a fake model: it returns the scripted responses
// in order and records every request it received. It has no HTTP
// client, no URL, and no server behind it — it is a plain function,
// which is exactly the point of the no-HTTP-re-entry tests: the
// loop can only reach the model through this in-process function.
type scriptedChat struct {
	responses []*provider.ChatResponse
	requests  []provider.ChatRequest
}

func (s *scriptedChat) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	s.requests = append(s.requests, req)
	if len(s.responses) == 0 {
		return nil, fmt.Errorf("scriptedChat: out of scripted responses (call %d)", len(s.requests))
	}
	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp, nil
}

func toolCallResponse(id, name, args string) *provider.ChatResponse {
	return &provider.ChatResponse{
		Content:      "",
		FinishReason: "tool_calls",
		ToolCalls: []wire.ToolCall{{
			ID:   id,
			Type: "function",
			Function: wire.ToolCallFunction{
				Name:      name,
				Arguments: args,
			},
		}},
	}
}

func textResponse(text string) *provider.ChatResponse {
	return &provider.ChatResponse{Content: text, FinishReason: "stop"}
}

func TestLoopRunsMetaToolsInProcess(t *testing.T) {
	r := New([]ToolServer{staticServerWithTools(t, "calc", 1)}, ModeSelector, 0)
	r.Discover(context.Background())
	chat := &scriptedChat{responses: []*provider.ChatResponse{
		// 1st call: the model lists tools, then runs one.
		toolCallResponse("c1", MetaGetTool, `{"query":"tool-0"}`),
		toolCallResponse("c2", MetaExecuteTool, `{"tool":"calc__tool-0","arguments":{"x":"42"}}`),
		textResponse("the tool said hi"),
	}}
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "use the tool"}}}
	resp, result, err := r.RunAgentLoop(context.Background(), chat.Chat, req)
	if err != nil {
		t.Fatalf("RunAgentLoop error: %v", err)
	}
	if resp.Content != "the tool said hi" {
		t.Fatalf("final content = %q, want the model's final text", resp.Content)
	}
	if len(chat.requests) != 3 {
		t.Fatalf("model called %d times, want 3 (initial + 2 follow-ups)", len(chat.requests))
	}
	if result.Iterations != 2 || result.ToolCalls != 2 || result.Capped {
		t.Fatalf("loop result = %+v, want iterations=2 tool_calls=2 capped=false", result)
	}
	// The follow-up requests carry the assistant tool-call turn
	// plus the tool result — built in-process, never via HTTP.
	second := chat.requests[1]
	if len(second.Messages) != 3 { // user, assistant(tool call), tool result
		t.Fatalf("2nd request has %d messages, want 3", len(second.Messages))
	}
	if second.Messages[2].Role != "tool" || !strings.Contains(second.Messages[2].Content, "tool-0") {
		t.Fatalf("2nd request tool result = %+v, want the mcp_tools_get listing", second.Messages[2])
	}
	third := chat.requests[2]
	if !strings.Contains(third.Messages[4].Content, "called tool-0") {
		t.Fatalf("3rd request tool result = %q, want the executed tool output", third.Messages[4].Content)
	}
	// Every model call in the loop saw the injected meta-tools.
	for i, rq := range chat.requests {
		if len(rq.Tools) != 2 {
			t.Fatalf("request %d has %d tools, want exactly 2 meta-tools", i, len(rq.Tools))
		}
	}
	st := r.Stats()
	if st.GetCalls != 1 || st.ExecuteCalls != 1 {
		t.Fatalf("counters get=%d execute=%d, want 1/1", st.GetCalls, st.ExecuteCalls)
	}
	if st.ExecuteByTool["calc__tool-0"] != 1 {
		t.Fatalf("per-tool count = %v, want calc__tool-0: 1", st.ExecuteByTool)
	}
}

func TestLoopCapEnforcedWithHonestStop(t *testing.T) {
	r := New([]ToolServer{staticServerWithTools(t, "s", 1)}, ModeSelector, 3)
	r.Discover(context.Background())
	// A model that never stops calling tools.
	chat := &scriptedChat{}
	for i := 0; i < 10; i++ {
		chat.responses = append(chat.responses,
			toolCallResponse(fmt.Sprintf("c%d", i), MetaExecuteTool, `{"tool":"s__tool-0","arguments":{}}`))
	}
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "loop forever"}}}
	resp, result, err := r.RunAgentLoop(context.Background(), chat.Chat, req)
	if err != nil {
		t.Fatalf("RunAgentLoop error: %v", err)
	}
	if resp == nil {
		t.Fatal("capped loop returned no response")
	}
	// 1 initial call + exactly 3 follow-ups, then the honest stop.
	if len(chat.requests) != 4 {
		t.Fatalf("model called %d times, want 4 (1 + cap 3)", len(chat.requests))
	}
	if result.Iterations != 3 || !result.Capped {
		t.Fatalf("loop result = %+v, want iterations=3 capped=true", result)
	}
	if st := r.Stats(); st.LoopCapped != 1 {
		t.Fatalf("loop_capped counter = %d, want 1", st.LoopCapped)
	}
}

func TestLoopPassesClientOwnedToolsThrough(t *testing.T) {
	// A model calling a tool the client defined (not an MCP tool)
	// must reach the client untouched: the loop returns the
	// response after exactly one model call, with no tool result
	// invented for a tool the registry cannot run.
	r := New([]ToolServer{staticServerWithTools(t, "s", 1)}, ModeSelector, 0)
	r.Discover(context.Background())
	chat := &scriptedChat{responses: []*provider.ChatResponse{
		toolCallResponse("c1", "get_weather", `{"city":"Pune"}`),
	}}
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "weather?"}}}
	resp, result, err := r.RunAgentLoop(context.Background(), chat.Chat, req)
	if err != nil {
		t.Fatalf("RunAgentLoop error: %v", err)
	}
	if len(chat.requests) != 1 || result.ToolCalls != 0 || result.Iterations != 0 {
		t.Fatalf("calls=%d result=%+v, want 1 call and no loop work", len(chat.requests), result)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("response tool calls = %+v, want the client's get_weather call passed through", resp.ToolCalls)
	}
}

func TestLoopNoHTTPReentry(t *testing.T) {
	// The loop's only route to the model is the ChatFunc it was
	// given. This test proves the structural point: run the loop
	// with a ChatFunc that records its calls and no HTTP server
	// anywhere in the process, and assert the loop completes with
	// exactly the in-process calls — there is no URL, no client,
	// and no second request for it to re-enter through. (A loop
	// that went back over HTTP would have nothing to dial: the
	// test binary listens on no port.)
	r := New([]ToolServer{staticServerWithTools(t, "s", 1)}, ModeSelector, 0)
	r.Discover(context.Background())
	chat := &scriptedChat{responses: []*provider.ChatResponse{
		toolCallResponse("c1", MetaExecuteTool, `{"tool":"s__tool-0","arguments":{"x":"1"}}`),
		textResponse("done"),
	}}
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "go"}}}
	resp, _, err := r.RunAgentLoop(context.Background(), chat.Chat, req)
	if err != nil {
		t.Fatalf("RunAgentLoop error: %v", err)
	}
	if resp.Content != "done" || len(chat.requests) != 2 {
		t.Fatalf("content=%q calls=%d, want done/2 in-process calls", resp.Content, len(chat.requests))
	}
	// The requests the fake saw are the only traffic that exists:
	// each carries the same model + growing message list, and no
	// request ever left the process (there is no HTTP client in
	// this package — `go list` would show no net/http import in
	// the loop path; asserted here by the absence of any dial:
	// the test would fail with "out of scripted responses" if the
	// loop tried to call the model any other way).
	if got := chat.requests[1].Messages[2].Content; !strings.Contains(got, "called tool-0") {
		t.Fatalf("tool result = %q, want the executed tool output", got)
	}
}

func TestLoopDirectModeExecutesDiscoveredTools(t *testing.T) {
	r := New([]ToolServer{staticServerWithTools(t, "s", 2)}, ModeDirect, 0)
	r.Discover(context.Background())
	chat := &scriptedChat{responses: []*provider.ChatResponse{
		toolCallResponse("c1", "s__tool-1", `{"x":"9"}`),
		textResponse("direct done"),
	}}
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "go"}}}
	resp, result, err := r.RunAgentLoop(context.Background(), chat.Chat, req)
	if err != nil {
		t.Fatalf("RunAgentLoop error: %v", err)
	}
	if resp.Content != "direct done" || result.ToolCalls != 1 {
		t.Fatalf("content=%q toolcalls=%d, want direct done/1", resp.Content, result.ToolCalls)
	}
	// Direct mode injected all 5... (2 tools) into the first call.
	if len(chat.requests[0].Tools) != 2 {
		t.Fatalf("direct first call tools = %d, want 2 discovered tools", len(chat.requests[0].Tools))
	}
}

func TestLoopUnknownToolIsAnErrorResultNotAFailure(t *testing.T) {
	r := New([]ToolServer{staticServerWithTools(t, "s", 1)}, ModeSelector, 0)
	r.Discover(context.Background())
	chat := &scriptedChat{responses: []*provider.ChatResponse{
		toolCallResponse("c1", MetaExecuteTool, `{"tool":"nope__missing","arguments":{}}`),
		textResponse("recovered"),
	}}
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "go"}}}
	resp, result, err := r.RunAgentLoop(context.Background(), chat.Chat, req)
	if err != nil {
		t.Fatalf("RunAgentLoop error: %v", err)
	}
	if resp.Content != "recovered" || result.ToolCalls != 1 {
		t.Fatalf("content=%q toolcalls=%d, want recovered/1", resp.Content, result.ToolCalls)
	}
	got := chat.requests[1].Messages[2].Content
	if !strings.HasPrefix(got, "error:") {
		t.Fatalf("unknown-tool result = %q, want an error: ... result the model can see", got)
	}
}

func TestExecuteAmbiguousPlainName(t *testing.T) {
	r := New([]ToolServer{
		staticServerWithTools(t, "one", 1),
		staticServerWithTools(t, "two", 1),
	}, ModeSelector, 0)
	r.Discover(context.Background())
	if _, err := r.Execute(context.Background(), "tool-0", nil); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("plain ambiguous name error = %v, want an ambiguity error", err)
	}
	if out, err := r.Execute(context.Background(), "two__tool-0", nil); err != nil || !strings.Contains(out, "called tool-0") {
		t.Fatalf("qualified execute = %q, %v, want the tool output", out, err)
	}
}

func TestSnapshotHonestEmptyState(t *testing.T) {
	r := New(nil, ModeSelector, 0)
	snap := r.Snapshot()
	if snap.Mode != ModeSelector || snap.MaxIterations != DefaultMaxIterations {
		t.Fatalf("snapshot = %+v, want selector/10", snap)
	}
	if snap.DirectCount != 0 || len(snap.Servers) != 0 {
		t.Fatalf("snapshot with no servers: direct=%d servers=%d, want 0/0", snap.DirectCount, len(snap.Servers))
	}
	if len(snap.SelectorTools) != 2 {
		t.Fatalf("selector tools = %v, want exactly the 2 meta-tools", snap.SelectorTools)
	}
}

// --- stdio server against a real mock MCP server process ---

// mockMCPServer is a minimal MCP server over stdio, in Python: it
// answers initialize, tools/list (echo + add) and tools/call. It is
// testdata, not product code — a stand-in for a real MCP server.
const mockMCPServer = `import sys, json
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
    except Exception:
        send({"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": "parse error"}})
        continue
    method = req.get("method", "")
    rid = req.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": rid, "result": {"protocolVersion": "2025-03-26", "capabilities": {}, "serverInfo": {"name": "mock-mcp", "version": "0"}}})
    elif method == "notifications/initialized":
        continue
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": rid, "result": {"tools": [
            {"name": "echo", "description": "Echo the text back", "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}}},
            {"name": "add", "description": "Add two numbers", "inputSchema": {"type": "object", "properties": {"a": {"type": "number"}, "b": {"type": "number"}}}}]}})
    elif method == "tools/call":
        name = req.get("params", {}).get("name", "")
        args = req.get("params", {}).get("arguments", {}) or {}
        if name == "echo":
            out = str(args.get("text", ""))
        elif name == "add":
            total = float(args.get("a", 0)) + float(args.get("b", 0))
            out = str(int(total)) if total == int(total) else str(total)
        else:
            send({"jsonrpc": "2.0", "id": rid, "error": {"code": -32602, "message": "unknown tool: " + name}})
            continue
        send({"jsonrpc": "2.0", "id": rid, "result": {"content": [{"type": "text", "text": out}]}})
    elif method == "ping":
        send({"jsonrpc": "2.0", "id": rid, "result": {}})
    else:
        send({"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "method not found: " + method}})
`

// pythonInterpreter returns a verified working Python interpreter
// for the mock MCP servers, or skips the test when none exists.
// Precedence: $PYTHON, then python3 / python / py from PATH.
// Every candidate is VERIFIED by running `<cand> --version` and
// requiring exit 0: on Windows, python3 can be a dead Microsoft
// Store stub — exec.LookPath finds it, but running it exits 9009
// ("Python was not found") instead of running the script, so a
// LookPath-only check passes and every stdio spawn then fails
// with "initialize: EOF". A candidate that fails --version is
// skipped, not used.
func pythonInterpreter(t *testing.T) string {
	t.Helper()
	candidates := []string{}
	if env := os.Getenv("PYTHON"); env != "" {
		candidates = append(candidates, env)
	}
	candidates = append(candidates, "python3", "python", "py")
	for _, cand := range candidates {
		path, err := exec.LookPath(cand)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = exec.CommandContext(ctx, path, "--version").Run()
		cancel()
		if err == nil {
			return path
		}
	}
	t.Skip("no working Python interpreter found ($PYTHON unset; python3/python/py all failed --version) — cannot spawn the mock MCP server")
	return ""
}

func writeMockMCPServer(t *testing.T) (interp, path string) {
	t.Helper()
	interp = pythonInterpreter(t)
	dir := t.TempDir()
	path = filepath.Join(dir, "mock_mcp.py")
	if err := os.WriteFile(path, []byte(mockMCPServer), 0o644); err != nil {
		t.Fatalf("writing mock MCP server: %v", err)
	}
	return interp, path
}

func TestStdioServerListAndCall(t *testing.T) {
	interp, path := writeMockMCPServer(t)
	srv := NewStdioServerWithTimeout("mock", interp, []string{path}, 10*1000*1000*1000)
	defer srv.Close()
	ctx := context.Background()
	tools, err := srv.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "add" {
		t.Fatalf("tools = %+v, want echo + add", tools)
	}
	out, err := srv.CallTool(ctx, "echo", map[string]any{"text": "hello"})
	if err != nil || out != "hello" {
		t.Fatalf("echo = %q, %v, want hello", out, err)
	}
	out, err = srv.CallTool(ctx, "add", map[string]any{"a": 2, "b": 3})
	if err != nil || out != "5" {
		t.Fatalf("add = %q, %v, want 5", out, err)
	}
	if _, err := srv.CallTool(ctx, "nope", nil); err == nil {
		t.Fatal("unknown tool call succeeded, want an error")
	}
}

func TestStdioServerSurvivesCallerContextCancel(t *testing.T) {
	// Regression: the child process was once spawned with
	// exec.CommandContext bound to the short-lived Discover
	// context, so cancelling that context killed the server and
	// every later tools/call failed with "broken pipe". The
	// process must outlive the context that started it.
	interp, path := writeMockMCPServer(t)
	srv := NewStdioServerWithTimeout("mock", interp, []string{path}, 10*1000*1000*1000)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := srv.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	cancel()
	// The server must still answer after the starting context
	// is gone — repeatedly, not just once.
	for i := 0; i < 3; i++ {
		out, err := srv.CallTool(context.Background(), "echo", map[string]any{"text": "alive"})
		if err != nil || out != "alive" {
			t.Fatalf("call %d after cancel: out=%q err=%v, want alive", i, out, err)
		}
	}
}

func TestStdioServerDownIsHonest(t *testing.T) {
	srv := NewStdioServerWithTimeout("missing", "definitely-not-a-real-binary-xyz", nil, 1000*1000*1000)
	defer srv.Close()
	if _, err := srv.ListTools(context.Background()); err == nil {
		t.Fatal("ListTools on a missing binary succeeded, want an error")
	}
	// Sticky: the same error again, not a respawn loop.
	if _, err := srv.ListTools(context.Background()); err == nil {
		t.Fatal("second ListTools succeeded, want the sticky start error")
	}
}

func TestRegistryWithStdioServerEndToEnd(t *testing.T) {
	interp, path := writeMockMCPServer(t)
	srv := NewStdioServerWithTimeout("mock", interp, []string{path}, 10*1000*1000*1000)
	defer srv.Close()
	r := New([]ToolServer{srv}, ModeSelector, 0)
	r.Discover(context.Background())
	if got := len(r.Discovered()); got != 2 {
		t.Fatalf("discovered = %d, want 2 from the stdio server", got)
	}
	st := r.Statuses()
	if len(st) != 1 || !st[0].OK {
		t.Fatalf("statuses = %+v, want one healthy server", st)
	}
	out, err := r.Execute(context.Background(), "mock__echo", map[string]any{"text": "via-registry"})
	if err != nil || out != "via-registry" {
		t.Fatalf("registry execute = %q, %v, want via-registry", out, err)
	}
}

// --- M1.8 acceptance tests (written FIRST, before the fix) ---

// slowHandshakeMockMCPServer sleeps 10 s before answering
// initialize, so any short per-call timeout fires mid-handshake:
// callLocked's timeout branch kills the process (killLocked nils
// s.cmd) and startLocked must then fail honestly — not panic
// dereferencing the nil s.cmd (stdio.go:164).
const slowHandshakeMockMCPServer = `import sys, json, time
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    method = req.get("method", "")
    rid = req.get("id")
    if method == "initialize":
        time.sleep(10)
        send({"jsonrpc": "2.0", "id": rid, "result": {"protocolVersion": "2025-03-26", "capabilities": {}, "serverInfo": {"name": "slow-mock", "version": "0"}}})
    elif method == "notifications/initialized":
        continue
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": rid, "result": {"tools": [{"name": "echo", "description": "Echo", "inputSchema": {"type": "object", "properties": {}}}]}})
    elif method == "tools/call":
        send({"jsonrpc": "2.0", "id": rid, "result": {"content": [{"type": "text", "text": "slow-echo"}]}})
`

// slowCallMockMCPServer answers initialize and tools/list at once
// but sleeps 10 s before answering every tools/call, so a short
// timeout fires mid-call (not mid-handshake): the call is killed,
// the next call must respawn a fresh process, and the orphaned
// read goroutine from the timed-out call must never interleave
// with (or race) the new process's stream.
const slowCallMockMCPServer = `import sys, json, time
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    method = req.get("method", "")
    rid = req.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": rid, "result": {"protocolVersion": "2025-03-26", "capabilities": {}, "serverInfo": {"name": "slow-call-mock", "version": "0"}}})
    elif method == "notifications/initialized":
        continue
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": rid, "result": {"tools": [{"name": "echo", "description": "Echo", "inputSchema": {"type": "object", "properties": {}}}]}})
    elif method == "tools/call":
        time.sleep(10)
        send({"jsonrpc": "2.0", "id": rid, "result": {"content": [{"type": "text", "text": "slow-echo"}]}})
`

func writeSlowMockMCPServer(t *testing.T, script string) (interp, path string) {
	t.Helper()
	interp = pythonInterpreter(t)
	dir := t.TempDir()
	path = filepath.Join(dir, "slow_mock_mcp.py")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatalf("writing slow mock MCP server: %v", err)
	}
	return interp, path
}

func TestStdioServerSlowHandshakeTimeoutDoesNotPanic(t *testing.T) {
	interp, path := writeSlowMockMCPServer(t, slowHandshakeMockMCPServer)
	srv := NewStdioServerWithTimeout("slow", interp, []string{path}, 200*time.Millisecond)
	defer srv.Close()
	// Must return a timeout error, NOT panic: pre-fix this
	// panics at stdio.go:164 (s.cmd is nil after killLocked).
	_, err := srv.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools on a server that never answers initialize succeeded, want a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("ListTools error = %v, want a timeout error", err)
	}
	// The start failure is sticky: a second call returns the same
	// error (still no panic, no respawn loop).
	if _, err := srv.ListTools(context.Background()); err == nil {
		t.Fatal("second ListTools succeeded, want the sticky start error")
	}
	// Close after a killed process must be safe.
	if err := srv.Close(); err != nil {
		t.Fatalf("Close after timeout = %v, want nil", err)
	}
}

func TestStdioServerSlowCallTimeoutThenRetryDoesNotRace(t *testing.T) {
	interp, path := writeSlowMockMCPServer(t, slowCallMockMCPServer)
	// 2 s timeout (was 200 ms): the timeout must be long enough
	// for a python3 spawn + initialize handshake on a loaded
	// machine, and short enough that the 10 s tools/call sleep
	// still times out. 200 ms proved too tight under full-gate
	// load (40 packages testing in parallel on 1.5 shared CPUs:
	// initialize took >200 ms and ListTools failed before the
	// test reached the slow call — 2026-11-18, Phase C), while
	// the test passes in isolation at ~0.5 s. The timeout is a
	// test parameter, not a product SLA: the product's own
	// timeout behaviour is unchanged, and the 10 s sleep still
	// times out at 2 s with 5x margin.
	srv := NewStdioServerWithTimeout("slow-call", interp, []string{path}, 2*time.Second)
	defer srv.Close()
	ctx := context.Background()
	if _, err := srv.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	// First call times out and kills the process.
	if _, err := srv.CallTool(ctx, "echo", nil); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("first CallTool error = %v, want a timeout error", err)
	}
	// Retry respawns a fresh process (timeout is not a sticky start
	// failure) and must also time out honestly — never return a
	// stray line from the orphaned goroutine of the first call.
	if _, err := srv.CallTool(ctx, "echo", nil); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("retry CallTool error = %v, want a timeout error", err)
	}
}

// discoverWithTimeout runs Discover and fails fast if it does
// not return in time — a regressed collision loop must fail the
// test, not hang the whole suite.
func discoverWithTimeout(t *testing.T, r *Registry, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		r.Discover(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("Discover did not terminate within %s (infinite collision loop?)", timeout)
	}
}

func TestDiscover64CharCollisionGetsTruncatedSuffix(t *testing.T) {
	// 60-char server + "__tool-…" truncates to the same 64-char
	// base for every tool ("a"*60 + "__to"): the second tool must
	// get a deterministic "__2" suffix that still fits in 64
	// chars (base truncated to make room), not the base itself.
	server := strings.Repeat("a", 60)
	tools := []mcp.Tool{
		StaticTool("tool-0000", "first", nil),
		StaticTool("tool-0001", "second", nil),
	}
	r := New([]ToolServer{NewStaticServer(server, tools, nil)}, ModeSelector, 0)
	discoverWithTimeout(t, r, 10*time.Second)
	got := r.Discovered()
	if len(got) != 2 {
		t.Fatalf("discovered = %d, want 2", len(got))
	}
	base := strings.Repeat("a", 60) + "__to"
	if got[0].Qualified != base || len(got[0].Qualified) != 64 {
		t.Fatalf("first qualified = %q, want %q (64 chars)", got[0].Qualified, base)
	}
	if got[1].Qualified == got[0].Qualified {
		t.Fatalf("second qualified = %q, same as first — collision not resolved", got[1].Qualified)
	}
	if len(got[1].Qualified) != 64 || !strings.HasSuffix(got[1].Qualified, "__2") {
		t.Fatalf("second qualified = %q, want a 64-char name ending in __2", got[1].Qualified)
	}
}

func TestDiscoverThousandColliding64CharNamesTerminates(t *testing.T) {
	// 1000 tools that all sanitise/truncate to the same 64-char
	// qualified name. Pre-fix Discover loops forever here:
	// base+"__2" truncated to 64 == base, so used[q] never clears.
	server := strings.Repeat("a", 60)
	tools := make([]mcp.Tool, 0, 1000)
	for i := 0; i < 1000; i++ {
		tools = append(tools, StaticTool(fmt.Sprintf("tool-%04d", i), "collision", nil))
	}
	r := New([]ToolServer{NewStaticServer(server, tools, nil)}, ModeSelector, 0)
	discoverWithTimeout(t, r, 10*time.Second)
	got := r.Discovered()
	if len(got) != 1000 {
		t.Fatalf("discovered = %d, want 1000 (no tool silently dropped)", len(got))
	}
	seen := map[string]bool{}
	for _, d := range got {
		if len(d.Qualified) > 64 {
			t.Fatalf("qualified %q is %d chars, want <= 64", d.Qualified, len(d.Qualified))
		}
		if seen[d.Qualified] {
			t.Fatalf("duplicate qualified name %q", d.Qualified)
		}
		seen[d.Qualified] = true
	}
	// Deterministic: a second Discover produces the identical list.
	discoverWithTimeout(t, r, 10*time.Second)
	again := r.Discovered()
	if len(again) != len(got) {
		t.Fatalf("second Discover = %d tools, want %d", len(again), len(got))
	}
	for i := range got {
		if again[i].Qualified != got[i].Qualified {
			t.Fatalf("second Discover tool %d = %q, want %q (suffixes must be deterministic)", i, again[i].Qualified, got[i].Qualified)
		}
	}
}

func TestUniqueQualifiedNameHashFallbackBounded(t *testing.T) {
	// Force the numeric-suffix search past its bound: pre-fill
	// used with the base and every "__<n>" candidate up to the
	// cap. The helper must still return promptly, with a
	// deterministic hash-suffixed name <= 64 chars that is not
	// in used — never loop.
	base := strings.Repeat("b", 64)
	used := map[string]bool{base: true}
	for n := 2; n <= maxNameSuffixAttempts; n++ {
		suffix := "__" + strconv.Itoa(n)
		keep := 64 - len(suffix)
		used[base[:keep]+suffix] = true
	}
	got := uniqueQualifiedName(base, "srv", "tool", used)
	if len(got) > 64 {
		t.Fatalf("fallback name %q is %d chars, want <= 64", got, len(got))
	}
	if used[got] {
		t.Fatalf("fallback name %q is already used", got)
	}
	if again := uniqueQualifiedName(base, "srv", "tool", used); again != got {
		t.Fatalf("fallback not deterministic: %q then %q", got, again)
	}
	other := uniqueQualifiedName(base, "srv", "other-tool", used)
	if other == got {
		t.Fatalf("different tools got the same fallback name %q", got)
	}
	if len(other) > 64 || used[other] {
		t.Fatalf("other-tool fallback = %q, want a fresh name <= 64 chars", other)
	}
}
