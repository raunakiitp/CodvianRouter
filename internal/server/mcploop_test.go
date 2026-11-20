package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/mcptools"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// MCP selector mode (CHECKLIST 141): the agent loop through the
// real HTTP handler — one client POST, in-process follow-up model
// calls, no second HTTP request.

// scriptedEngine is an Engine that returns scripted Chat responses
// in order and records every request. It has no HTTP client and no
// URL: the only way the loop can reach the model is through this
// in-process Chat, which is the no-HTTP-re-entry property under
// test.
type scriptedEngine struct {
	mu        sync.Mutex
	responses []*provider.ChatResponse
	requests  []provider.ChatRequest
}

func (e *scriptedEngine) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests = append(e.requests, req)
	if len(e.responses) == 0 {
		return &provider.ChatResponse{Content: "out of script", Model: "m"}, nil
	}
	resp := e.responses[0]
	e.responses = e.responses[1:]
	return resp, nil
}

func (e *scriptedEngine) ChatStream(_ context.Context, req provider.ChatRequest, _ func(string) error) (*provider.ChatResponse, error) {
	return e.Chat(context.Background(), req)
}

func (e *scriptedEngine) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.requests)
}

func mcpToolCallResponse(id, name, args string) *provider.ChatResponse {
	return &provider.ChatResponse{
		Content:      "",
		Model:        "m",
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

func mcpSelectorTestRegistry(t *testing.T, mode string, maxIter int) *mcptools.Registry {
	t.Helper()
	echo := mcptools.NewStaticServer("calc", []mcp.Tool{
		mcptools.StaticTool("echo", "Echo the text back",
			map[string]any{"text": map[string]any{"type": "string"}}),
	}, map[string]func(context.Context, map[string]any) (string, error){
		"echo": func(_ context.Context, args map[string]any) (string, error) {
			text, _ := args["text"].(string)
			return "echo:" + text, nil
		},
	})
	r := mcptools.New([]mcptools.ToolServer{echo}, mode, maxIter)
	r.Discover(context.Background())
	return r
}

func TestChatCompletionsMCPLoopRunsInProcess(t *testing.T) {
	eng := &scriptedEngine{responses: []*provider.ChatResponse{
		mcpToolCallResponse("c1", mcptools.MetaExecuteTool, `{"tool":"calc__echo","arguments":{"text":"hi"}}`),
		{Content: "final answer", Model: "m", FinishReason: "stop"},
	}}
	reg := mcpSelectorTestRegistry(t, mcptools.ModeSelector, 0)
	srv := newTestServer(t, eng, WithMCPTools(reg))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"use the echo tool"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Choices []struct {
			Message provider.Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != "final answer" {
		t.Fatalf("choices = %+v, want one final-answer choice", body.Choices)
	}
	// The loop made exactly one follow-up model call in-process:
	// the test made ONE HTTP request and the engine saw two Chat
	// calls — there is no second HTTP request for the loop to
	// have re-entered through.
	if got := eng.calls(); got != 2 {
		t.Fatalf("engine Chat called %d times, want 2 (initial + 1 follow-up)", got)
	}
	if got := rec.Header().Get("X-Codvian-MCP-Mode"); got != "selector" {
		t.Errorf("X-Codvian-MCP-Mode = %q, want selector", got)
	}
	if got := rec.Header().Get("X-Codvian-MCP-Iterations"); got != "1" {
		t.Errorf("X-Codvian-MCP-Iterations = %q, want 1", got)
	}
	if got := rec.Header().Get("X-Codvian-MCP-Capped"); got != "" {
		t.Errorf("X-Codvian-MCP-Capped = %q, want empty (not capped)", got)
	}
	// The first engine call saw exactly the two meta-tools.
	eng.mu.Lock()
	first := eng.requests[0]
	eng.mu.Unlock()
	if len(first.Tools) != 2 {
		t.Fatalf("first engine call tools = %d, want exactly 2 meta-tools", len(first.Tools))
	}
	// The registry counted the real execute call.
	if st := reg.Stats(); st.ExecuteCalls != 1 || st.ExecuteByTool["calc__echo"] != 1 {
		t.Fatalf("registry stats = %+v, want 1 execute of calc__echo", st)
	}
}

func TestChatCompletionsMCPClientToolPassThrough(t *testing.T) {
	eng := &scriptedEngine{responses: []*provider.ChatResponse{
		mcpToolCallResponse("c1", "get_weather", `{"city":"Pune"}`),
	}}
	reg := mcpSelectorTestRegistry(t, mcptools.ModeSelector, 0)
	srv := newTestServer(t, eng, WithMCPTools(reg))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"weather","parameters":{"type":"object","properties":{}}}}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Choices []struct {
			Message provider.Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(body.Choices) != 1 || len(body.Choices[0].Message.ToolCalls) != 1 ||
		body.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("choices = %+v, want the client's get_weather call passed through", body.Choices)
	}
	if got := eng.calls(); got != 1 {
		t.Fatalf("engine Chat called %d times, want 1 (no loop for client tools)", got)
	}
}

func TestChatCompletionsMCPDisabledWithoutRegistry(t *testing.T) {
	eng := &scriptedEngine{responses: []*provider.ChatResponse{
		{Content: "plain", Model: "m", FinishReason: "stop"},
	}}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-MCP-Mode"); got != "" {
		t.Errorf("X-Codvian-MCP-Mode = %q without a registry, want empty", got)
	}
}

func TestChatCompletionsMCPLoopCapHeader(t *testing.T) {
	eng := &scriptedEngine{}
	for i := 0; i < 5; i++ {
		eng.responses = append(eng.responses,
			mcpToolCallResponse("c", mcptools.MetaExecuteTool, `{"tool":"calc__echo","arguments":{"text":"x"}}`))
	}
	reg := mcpSelectorTestRegistry(t, mcptools.ModeSelector, 1)
	srv := newTestServer(t, eng, WithMCPTools(reg))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"loop"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-MCP-Capped"); got != "true" {
		t.Errorf("X-Codvian-MCP-Capped = %q, want true", got)
	}
	if got := eng.calls(); got != 2 {
		t.Fatalf("engine Chat called %d times, want 2 (1 + cap 1)", got)
	}
}

func TestHandleMCPSelectorFeed(t *testing.T) {
	reg := mcpSelectorTestRegistry(t, mcptools.ModeSelector, 0)
	// One real execute call first, so the feed's counters are live.
	if _, err := reg.Execute(context.Background(), "calc__echo", map[string]any{"text": "x"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithMCPTools(reg), WithListenAddr("127.0.0.1:20128"))
	req := httptest.NewRequest(http.MethodGet, "/dashboard/mcp", nil)
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/mcp status = %d, want 200", rec.Code)
	}
	var out mcpResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if out.Mode != "selector" {
		t.Errorf("mode = %q, want selector", out.Mode)
	}
	if out.MaxIterations != mcptools.DefaultMaxIterations {
		t.Errorf("max_iterations = %d, want %d", out.MaxIterations, mcptools.DefaultMaxIterations)
	}
	if len(out.Servers) != 1 || out.Servers[0].Name != "calc" || !out.Servers[0].OK || out.Servers[0].Tools != 1 {
		t.Errorf("servers = %+v, want one healthy calc server with 1 tool", out.Servers)
	}
	if len(out.SelectorTools) != 2 || out.SelectorTools[0] != mcptools.MetaGetTool || out.SelectorTools[1] != mcptools.MetaExecuteTool {
		t.Errorf("selector_tools = %v, want the 2 meta-tools", out.SelectorTools)
	}
	if out.DirectCount != 1 || len(out.DirectTools) != 1 || out.DirectTools[0].Qualified != "calc__echo" {
		t.Errorf("direct tools = %+v (count %d), want 1 calc__echo", out.DirectTools, out.DirectCount)
	}
	if out.ToolCalls.ExecuteCalls != 1 || out.ToolCalls.ExecuteByTool["calc__echo"] != 1 {
		t.Errorf("tool_calls = %+v, want 1 execute of calc__echo", out.ToolCalls)
	}
	if len(out.Tools) != 24 {
		t.Errorf("stdio tools = %d, want 24", len(out.Tools))
	}
}

func TestHandleMCPFeedModeOffWithoutRegistry(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	req := httptest.NewRequest(http.MethodGet, "/dashboard/mcp", nil)
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/mcp status = %d, want 200", rec.Code)
	}
	var out mcpResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if out.Mode != "off" || out.MaxIterations != 0 || len(out.Servers) != 0 || out.DirectCount != 0 {
		t.Errorf("no-registry feed = mode %q max %d servers %d direct %d, want off/0/0/0",
			out.Mode, out.MaxIterations, len(out.Servers), out.DirectCount)
	}
	if strings.Contains(rec.Body.String(), "calc") {
		t.Errorf("no-registry feed mentions a server: %s", rec.Body.String())
	}
}
