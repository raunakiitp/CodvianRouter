package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Helpers shared with mcp_test.go (same package): newTestServer()
// (fake Source: 2 providers, 2 models, uptime -1, LocalOperator
// identity) and call(t, s, msg).

// toolCallResult drives one tools/call through Handle and returns
// the decoded result object, failing the test on a JSON-RPC error.
func toolCallResult(t *testing.T, s *Server, name string, argsJSON string) map[string]any {
	t.Helper()
	out := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+argsJSON+`}}`)
	if out["error"] != nil {
		t.Fatalf("tools/call %s returned error: %v", name, out["error"])
	}
	result, _ := out["result"].(map[string]any)
	return result
}

// structured returns the structuredContent payload of a tool
// result, failing the test when it is absent.
func structured(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("tool result has no structuredContent: %v", result)
	}
	return sc
}

// toolNames extracts the "name" of every entry in a tool_search
// "tools" list.
func toolNames(t *testing.T, sc map[string]any) []string {
	t.Helper()
	raw, ok := sc["tools"].([]any)
	if !ok {
		t.Fatalf("tool_search result has no tools list: %v", sc)
	}
	names := make([]string, 0, len(raw))
	for _, e := range raw {
		m, _ := e.(map[string]any)
		name, _ := m["name"].(string)
		names = append(names, name)
	}
	return names
}

func TestM95ProtocolVersionIs20250326(t *testing.T) {
	// Literal pin, not `== ProtocolVersion`: a revert of the
	// const to 2024-11-05 must fail this test. (mcp_test.go's
	// TestInitialize only asserts result == ProtocolVersion, so
	// it passes for ANY const value — it cannot catch a revert.)
	if ProtocolVersion != "2025-03-26" {
		t.Fatalf("ProtocolVersion = %q, want %q", ProtocolVersion, "2025-03-26")
	}
	out := call(t, newTestServer(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	result, _ := out["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" {
		t.Errorf("initialize protocolVersion = %v, want 2025-03-26", result["protocolVersion"])
	}
}

func TestM95ToolSearchRegisteredWithStatusScope(t *testing.T) {
	for _, name := range []string{"tool_search", "heartbeat"} {
		scopes, known := RequiredScopes(name)
		if !known {
			t.Errorf("tool %s has no scope assignment", name)
			continue
		}
		if len(scopes) != 1 || scopes[0] != ScopeReadStatus {
			t.Errorf("tool %s scopes = %v, want [%s]", name, scopes, ScopeReadStatus)
		}
	}
	found := map[string]bool{}
	for _, tool := range Tools() {
		found[tool.Name] = true
		if tool.Name == "tool_search" || tool.Name == "heartbeat" {
			if tool.Description == "" || tool.InputSchema == nil {
				t.Errorf("tool %s has empty description/inputSchema", tool.Name)
			}
		}
	}
	if !found["tool_search"] || !found["heartbeat"] {
		t.Errorf("Tools() = %v, want tool_search and heartbeat present", found)
	}
}

func TestM95ToolSearchExactNameMatch(t *testing.T) {
	s := newTestServer()
	sc := structured(t, toolCallResult(t, s, "tool_search", `{"query":"provider_list"}`))
	if sc["count"] != float64(1) {
		t.Fatalf("tool_search provider_list count = %v, want 1", sc["count"])
	}
	if names := toolNames(t, sc); len(names) != 1 || names[0] != "provider_list" {
		t.Fatalf("tool_search provider_list names = %v, want [provider_list]", names)
	}
	// The match carries its description and its required scopes.
	raw, _ := sc["tools"].([]any)
	entry, _ := raw[0].(map[string]any)
	if entry["description"] == "" {
		t.Errorf("tool_search entry has empty description: %v", entry)
	}
	scopes, _ := entry["scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != ScopeReadProviders {
		t.Errorf("provider_list scopes = %v, want [%s]", scopes, ScopeReadProviders)
	}
}

func TestM95ToolSearchSubstringMatch(t *testing.T) {
	s := newTestServer()
	// "model" matches model_list by name and router_status by
	// description ("provider/model counts"); every returned tool
	// must contain the substring in its name or description
	// (property assertion, not an exact count — M9.3/M9.4 tools
	// may legitimately match the same substring later).
	sc := structured(t, toolCallResult(t, s, "tool_search", `{"query":"model"}`))
	names := toolNames(t, sc)
	if len(names) == 0 {
		t.Fatalf("tool_search \"model\" returned no matches")
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["model_list"] {
		t.Errorf("tool_search \"model\" names = %v, want model_list present", names)
	}
	for _, tool := range Tools() {
		if !found[tool.Name] {
			continue
		}
		if !strings.Contains(strings.ToLower(tool.Name), "model") &&
			!strings.Contains(strings.ToLower(tool.Description), "model") {
			t.Errorf("tool_search returned %s, which contains neither \"model\" in name nor description", tool.Name)
		}
	}
	// Case-insensitive: "MODEL" finds the same set.
	sc2 := structured(t, toolCallResult(t, s, "tool_search", `{"query":"MODEL"}`))
	if names2 := toolNames(t, sc2); len(names2) != len(names) {
		t.Errorf("tool_search \"MODEL\" names = %v, want same %d as \"model\" %v", names2, len(names), names)
	}
}

func TestM95ToolSearchNoMatchReturnsEmptyList(t *testing.T) {
	s := newTestServer()
	sc := structured(t, toolCallResult(t, s, "tool_search", `{"query":"zzzz-no-such-tool"}`))
	if sc["count"] != float64(0) {
		t.Fatalf("tool_search no-match count = %v, want 0", sc["count"])
	}
	raw, ok := sc["tools"].([]any)
	if !ok {
		t.Fatalf("tool_search no-match tools is not a list: %v", sc["tools"])
	}
	if len(raw) != 0 {
		t.Fatalf("tool_search no-match tools = %v, want empty list", raw)
	}
	// The empty list must serialise as [] (not null): the
	// no-match case is a valid empty result, not an error.
	text, _ := json.Marshal(sc["tools"])
	if string(text) != "[]" {
		t.Errorf("tool_search no-match tools serialises as %s, want []", text)
	}
}

func TestM95ToolSearchEmptyQueryReturnsFullList(t *testing.T) {
	s := newTestServer()
	for _, argsJSON := range []string{`{}`, `{"query":""}`, `{"query":"   "}`} {
		sc := structured(t, toolCallResult(t, s, "tool_search", argsJSON))
		names := toolNames(t, sc)
		if len(names) != len(Tools()) {
			t.Fatalf("tool_search %s returned %d tools, want full list %d (%v)", argsJSON, len(names), len(Tools()), names)
		}
		if sc["count"] != float64(len(Tools())) {
			t.Errorf("tool_search %s count = %v, want %d", argsJSON, sc["count"], len(Tools()))
		}
	}
	// No arguments at all: same full list.
	sc := structured(t, toolCallResult(t, s, "tool_search", `null`))
	if len(toolNames(t, sc)) != len(Tools()) {
		t.Errorf("tool_search with null arguments did not return the full list")
	}
}

func TestM95ToolSearchNonStringQueryIsInvalidParams(t *testing.T) {
	s := newTestServer()
	out := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":42}}}`)
	errBody, _ := out["error"].(map[string]any)
	if errBody["code"] != float64(-32602) {
		t.Fatalf("tool_search non-string query code = %v, want -32602", errBody["code"])
	}
}

func TestM95ToolSearchScopeDenied(t *testing.T) {
	s := newTestServer()
	// A caller with only mcp:read:models cannot call tool_search.
	out := s.HandleWithIdentity(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":"provider"}}}`),
		GatewayKeyClient("key-1", []string{ScopeReadModels}))
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	errBody, _ := decoded["error"].(map[string]any)
	if errBody["code"] != float64(ScopeDeniedCode) {
		t.Fatalf("tool_search without mcp:read:status code = %v, want %d", errBody["code"], ScopeDeniedCode)
	}
	// The same caller with mcp:read:status is allowed; the
	// mcp:read:* wildcard also covers it.
	for _, scopes := range [][]string{{ScopeReadStatus}, {"mcp:read:*"}} {
		raw := s.HandleWithIdentity(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":"provider"}}}`),
			GatewayKeyClient("key-1", scopes))
		var d map[string]any
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("response is not JSON: %v", err)
		}
		if d["error"] != nil {
			t.Errorf("tool_search with scopes %v denied: %v", scopes, d["error"])
		}
	}
}

func TestM95HeartbeatReturnsLiveSnapshot(t *testing.T) {
	s := newTestServer()
	sc := structured(t, toolCallResult(t, s, "heartbeat", `{}`))
	// Fake source uptime is -1 (no serving loop) — reported
	// honestly, never zero-filled.
	if sc["uptime_seconds"] != float64(-1) {
		t.Errorf("heartbeat uptime_seconds = %v, want -1 (fake source)", sc["uptime_seconds"])
	}
	if sc["tool_count"] != float64(len(Tools())) {
		t.Errorf("heartbeat tool_count = %v, want %d (len(Tools()))", sc["tool_count"], len(Tools()))
	}
	if sc["protocol_version"] != "2025-03-26" {
		t.Errorf("heartbeat protocol_version = %v, want 2025-03-26", sc["protocol_version"])
	}
	if sc["audit_log"] != "disabled" {
		t.Errorf("heartbeat audit_log = %v, want disabled (no audit log wired)", sc["audit_log"])
	}
}

func TestM95HeartbeatAuditEnabledWhenWired(t *testing.T) {
	s := newTestServer()
	log, err := OpenAuditLog(t.TempDir() + "/audit.jsonl")
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}
	s.WithAuditLog(log)
	sc := structured(t, toolCallResult(t, s, "heartbeat", `{}`))
	if sc["audit_log"] != "enabled" {
		t.Errorf("heartbeat audit_log = %v, want enabled (audit log wired)", sc["audit_log"])
	}
}

func TestM95HeartbeatScopeDenied(t *testing.T) {
	s := newTestServer()
	raw := s.HandleWithIdentity(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"heartbeat","arguments":{}}}`),
		GatewayKeyClient("key-1", []string{ScopeReadProviders}))
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	errBody, _ := d["error"].(map[string]any)
	if errBody["code"] != float64(ScopeDeniedCode) {
		t.Fatalf("heartbeat without mcp:read:status code = %v, want %d", errBody["code"], ScopeDeniedCode)
	}
}

// --- Cardinality controls -------------------------------------

func visibleToolNames(s *Server) []string {
	names := make([]string, 0)
	for _, t := range s.visibleTools() {
		names = append(names, t.Name)
	}
	return names
}

func TestM95ToolFilterNoFilterShowsAll(t *testing.T) {
	s := newTestServer()
	if got := visibleToolNames(s); len(got) != len(Tools()) {
		t.Fatalf("unfiltered visibleTools = %v, want all %d tools", got, len(Tools()))
	}
	if !s.isToolVisible("provider_list") || !s.isToolVisible("tool_search") {
		t.Errorf("unfiltered server hides a tool: %v", visibleToolNames(s))
	}
}

func TestM95ToolFilterAllowList(t *testing.T) {
	// Allow-list includes heartbeat AND tool_search itself:
	// calling a filtered-out tool answers -32602 — so a test
	// that CALLS a tool must keep it visible, and assert the
	// count from inside the filter. (The pure allow-list
	// predicate is covered by the visibleTools assertions below
	// and in DenyList.)
	s := newTestServer().WithToolFilter([]string{"provider_list", "heartbeat", "tool_search"}, nil)
	if got := visibleToolNames(s); len(got) != 3 || got[0] != "provider_list" || got[1] != "heartbeat" || got[2] != "tool_search" {
		t.Fatalf("allow-list visibleTools = %v, want [provider_list heartbeat tool_search] (Tools() order)", got)
	}
	if s.isToolVisible("router_status") || s.isToolVisible("model_list") {
		t.Errorf("allow-list server still shows a non-allowed tool: %v", visibleToolNames(s))
	}
	// tool_search and heartbeat honour the same filter end to end.
	sc := structured(t, toolCallResult(t, s, "tool_search", `{"query":""}`))
	if names := toolNames(t, sc); len(names) != 3 || names[0] != "provider_list" || names[1] != "heartbeat" || names[2] != "tool_search" {
		t.Fatalf("tool_search under allow-list = %v, want [provider_list heartbeat tool_search]", names)
	}
	hb := structured(t, toolCallResult(t, s, "heartbeat", `{}`))
	if hb["tool_count"] != float64(3) {
		t.Errorf("heartbeat tool_count under allow-list = %v, want 3", hb["tool_count"])
	}
}

func TestM95ToolFilterDenyList(t *testing.T) {
	s := newTestServer().WithToolFilter(nil, []string{"provider_list"})
	for _, n := range visibleToolNames(s) {
		if n == "provider_list" {
			t.Fatalf("deny-list visibleTools still contains provider_list: %v", visibleToolNames(s))
		}
	}
	if s.isToolVisible("provider_list") {
		t.Errorf("deny-list server reports provider_list visible")
	}
	if !s.isToolVisible("router_status") {
		t.Errorf("deny-list server hides an unlisted tool: %v", visibleToolNames(s))
	}
	// tool_search never returns a denied tool, even on a query
	// that matches its name exactly.
	sc := structured(t, toolCallResult(t, s, "tool_search", `{"query":"provider_list"}`))
	if sc["count"] != float64(0) {
		t.Fatalf("tool_search for denied provider_list count = %v, want 0", sc["count"])
	}
}

func TestM95ToolFilterDenyListToolSearchItself(t *testing.T) {
	// The filter is name-agnostic: deny-listing tool_search
	// itself hides it from tools/list and makes a call to it
	// answer -32602, exactly like a filtered-out provider_list
	// — filtered-call coverage is not provider_list-only.
	s := newTestServer().WithToolFilter(nil, []string{"tool_search"})
	if s.isToolVisible("tool_search") {
		t.Errorf("deny-list server reports tool_search visible")
	}
	out := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	for _, tool := range tools {
		m, _ := tool.(map[string]any)
		if m["name"] == "tool_search" {
			t.Fatalf("tools/list still announces filtered-out tool_search: %v", tools)
		}
	}
	code, msg := callErrCode(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":""}}}`)
	if code != -32602 {
		t.Errorf("tools/call of filtered-out tool_search: code = %v, want -32602 (got %q)", code, msg)
	}
	// A visible tool still works through the same path.
	if code := callToolErr(t, s, "provider_list", map[string]any{}); code != 0 {
		t.Errorf("tools/call of visible provider_list: code = %v, want 0", code)
	}
}

func TestM95ToolFilterDenyBeatsAllow(t *testing.T) {
	s := newTestServer().WithToolFilter([]string{"provider_list", "model_list"}, []string{"provider_list"})
	if got := visibleToolNames(s); len(got) != 1 || got[0] != "model_list" {
		t.Fatalf("deny-beats-allow visibleTools = %v, want [model_list]", got)
	}
}

func TestM95ToolFilterMaxTools(t *testing.T) {
	// Tools() order is builtins first, then extras sorted by
	// name — the cap keeps the first n in that order. Expected
	// lists are computed from Tools(), not hard-coded: M9.3/M9.4
	// tools sort among the extras, so a hard-coded 4th tool
	// would go stale the moment another slice lands a tool.
	all := Tools()
	if len(all) < 3 {
		t.Fatalf("Tools() = %v, want at least the 3 builtins", all)
	}
	// A tight cap is asserted at the visibleTools level only.
	s2 := newTestServer().WithMaxTools(2)
	if got := visibleToolNames(s2); len(got) != 2 || got[0] != all[0].Name || got[1] != all[1].Name {
		t.Fatalf("maxTools=2 visibleTools = %v, want first two of Tools() %v", got, all[:2])
	}
	// The heartbeat assertion uses a cap that keeps heartbeat
	// itself visible: once the mcp.go tools/call wiring lands, a
	// call to a filtered-out tool answers -32602.
	hbIdx := -1
	for i, tool := range all {
		if tool.Name == "heartbeat" {
			hbIdx = i
			break
		}
	}
	if hbIdx < 0 {
		t.Fatalf("heartbeat not in Tools() = %v", all)
	}
	s := newTestServer().WithMaxTools(hbIdx + 1)
	if got := visibleToolNames(s); len(got) != hbIdx+1 {
		t.Fatalf("maxTools=%d visibleTools = %v, want first %d of Tools()", hbIdx+1, got, hbIdx+1)
	}
	for i, name := range visibleToolNames(s) {
		if name != all[i].Name {
			t.Fatalf("maxTools=%d visibleTools[%d] = %s, want %s", hbIdx+1, i, name, all[i].Name)
		}
	}
	hb := structured(t, toolCallResult(t, s, "heartbeat", `{}`))
	if hb["tool_count"] != float64(hbIdx+1) {
		t.Errorf("heartbeat tool_count under maxTools=%d = %v, want %d", hbIdx+1, hb["tool_count"], hbIdx+1)
	}
	// MaxTools <= 0 means no cap (never a silent tail-drop).
	for _, n := range []int{0, -1} {
		s2 := newTestServer().WithMaxTools(n)
		if got := len(visibleToolNames(s2)); got != len(Tools()) {
			t.Errorf("maxTools=%d visibleTools = %d, want no cap (%d)", n, got, len(Tools()))
		}
	}
	// Cap composes with allow/deny: filter first, then cap.
	s3 := newTestServer().WithToolFilter(nil, []string{"router_status"}).WithMaxTools(2)
	if got := visibleToolNames(s3); len(got) != 2 || got[0] != "provider_list" || got[1] != "model_list" {
		t.Fatalf("deny+maxTools visibleTools = %v, want [provider_list model_list]", got)
	}
}

func TestM95ToolFilterUnknownNamesIgnored(t *testing.T) {
	s := newTestServer().WithToolFilter([]string{"no-such-tool"}, nil)
	if got := visibleToolNames(s); len(got) != 0 {
		t.Fatalf("allow-list of an unknown name visibleTools = %v, want []", got)
	}
	s2 := newTestServer().WithToolFilter(nil, []string{"no-such-tool"})
	if got := len(visibleToolNames(s2)); got != len(Tools()) {
		t.Fatalf("deny-list of an unknown name visibleTools = %d, want all %d", got, len(Tools()))
	}
}

func TestM95ToolFilterIsPerServer(t *testing.T) {
	a := newTestServer().WithToolFilter([]string{"provider_list"}, nil)
	b := newTestServer().WithToolFilter(nil, []string{"provider_list"})
	if got := visibleToolNames(a); len(got) != 1 || got[0] != "provider_list" {
		t.Fatalf("server A visibleTools = %v, want [provider_list]", got)
	}
	for _, n := range visibleToolNames(b) {
		if n == "provider_list" {
			t.Fatalf("server B visibleTools still contains provider_list: %v", visibleToolNames(b))
		}
	}
	// A third server with no option is unfiltered — setting a
	// filter on one server never leaks into another.
	c := newTestServer()
	if got := len(visibleToolNames(c)); got != len(Tools()) {
		t.Fatalf("server C visibleTools = %d, want all %d", got, len(Tools()))
	}
}

func TestM95ToolFilterSurvivesWithIdentityCopy(t *testing.T) {
	// REGRESSION (found 2026-11-19): tools/call dispatches
	// handlers on s.withIdentity(id), a shallow copy of the
	// server (mcp.go: `cp := *s`). A filter stored keyed by
	// *Server alone is invisible to the copy, so tool_search
	// and heartbeat used to answer UNFILTERED through Handle
	// while visibleTools() on the original answered filtered.
	// Each server here has its own Source (newTestServer builds
	// a fresh &fakeSource), so the two filters must stay
	// independent end to end, through Handle, on both servers.
	// Both allow-lists include tool_search itself: calling a
	// filtered-out tool answers -32602, so a test that CALLS
	// tool_search must keep it visible.
	a := newTestServer().WithToolFilter([]string{"provider_list", "heartbeat", "tool_search"}, nil)
	b := newTestServer().WithToolFilter([]string{"model_list", "heartbeat", "tool_search"}, nil)
	scA := structured(t, toolCallResult(t, a, "tool_search", `{"query":""}`))
	if names := toolNames(t, scA); len(names) != 3 || names[0] != "provider_list" || names[1] != "heartbeat" || names[2] != "tool_search" {
		t.Fatalf("tool_search on server A = %v, want [provider_list heartbeat tool_search]", names)
	}
	scB := structured(t, toolCallResult(t, b, "tool_search", `{"query":""}`))
	if names := toolNames(t, scB); len(names) != 3 || names[0] != "model_list" || names[1] != "heartbeat" || names[2] != "tool_search" {
		t.Fatalf("tool_search on server B = %v, want [model_list heartbeat tool_search]", names)
	}
	hbA := structured(t, toolCallResult(t, a, "heartbeat", `{}`))
	if hbA["tool_count"] != float64(3) {
		t.Errorf("heartbeat on server A tool_count = %v, want 3", hbA["tool_count"])
	}
	hbB := structured(t, toolCallResult(t, b, "heartbeat", `{}`))
	if hbB["tool_count"] != float64(3) {
		t.Errorf("heartbeat on server B tool_count = %v, want 3", hbB["tool_count"])
	}
}

func TestM95ToolFilterCallerSliceCannotWiden(t *testing.T) {
	allow := []string{"provider_list"}
	s := newTestServer().WithToolFilter(allow, nil)
	allow[0] = "router_status" // mutate the caller's slice
	if got := visibleToolNames(s); len(got) != 1 || got[0] != "provider_list" {
		t.Fatalf("filter widened by caller mutation: visibleTools = %v, want [provider_list]", got)
	}
}

func TestM95ApplyToolFilterPure(t *testing.T) {
	tools := []Tool{{Name: "b"}, {Name: "a"}, {Name: "c"}}
	out := applyToolFilter(tools, &ToolFilter{Allow: []string{"c", "a"}, Deny: []string{"a"}})
	if len(out) != 1 || out[0].Name != "c" {
		t.Fatalf("applyToolFilter = %v, want [c]", out)
	}
	// Input order preserved, input slice unmodified.
	out2 := applyToolFilter(tools, nil)
	if len(out2) != 3 || tools[0].Name != "b" {
		t.Fatalf("applyToolFilter(nil) mutated input or reordered: %v", out2)
	}
}

func TestM95ToolFilterCallOfFilteredToolReturns32602(t *testing.T) {
	// The mcp.go wiring: tools/list announces s.visibleTools()
	// and tools/call checks s.isToolVisible BEFORE the scope
	// check, so a filtered-out tool is indistinguishable from
	// an unknown tool — -32602, never a scope denial.
	s := newTestServer().WithToolFilter(nil, []string{"provider_list"})
	log, _ := tempAuditLog(t)
	s.WithAuditLog(log)
	// tools/list omits it.
	out := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	for _, tool := range tools {
		m, _ := tool.(map[string]any)
		if m["name"] == "provider_list" {
			t.Fatalf("tools/list still announces filtered-out provider_list: %v", tools)
		}
	}
	// tools/call answers -32602 even for the LocalOperator, who
	// holds every scope — the call must fail on visibility, not
	// pass the scope check and reach dispatch.
	code, msg := callErrCode(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"provider_list","arguments":{}}}`)
	if code != -32602 {
		t.Errorf("tools/call of filtered-out provider_list: code = %v, want -32602 (got %q)", code, msg)
	}
	// Visibility runs BEFORE the scope check: a caller with NO
	// scopes must still get -32602, not -32002 — a scope denial
	// would reveal the tool exists.
	raw := s.HandleWithIdentity(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"provider_list","arguments":{}}}`), Anonymous())
	var anonOut map[string]any
	if err := json.Unmarshal(raw, &anonOut); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	errBody, _ := anonOut["error"].(map[string]any)
	if errBody == nil {
		t.Fatalf("anonymous tools/call of filtered-out provider_list succeeded: %v", anonOut)
	}
	if errBody["code"] != float64(-32602) {
		t.Errorf("anonymous tools/call of filtered-out provider_list: code = %v, want -32602 (a -32002 would leak the tool's existence)", errBody["code"])
	}
	// A visible tool still works through the same path.
	if code := callToolErr(t, s, "router_status", map[string]any{}); code != 0 {
		t.Errorf("tools/call of visible router_status: code = %v, want 0", code)
	}
	// The filtered calls are audited as denials, not silently
	// dropped: two -32602 denies (LocalOperator, then
	// anonymous), then the visible tool's allow. Mirrors
	// TestDispatchAuditsAllowAndDeny (enforcement_test.go).
	entries, err := log.List(0)
	if err != nil || len(entries) != 3 {
		t.Fatalf("audit entries = %d, err %v, want 3 (2 filtered denies + 1 allow)", len(entries), err)
	}
	// List is newest-first: reverse to call order.
	byCall := []AuditEntry{entries[2], entries[1], entries[0]}
	if byCall[0].Decision != AuditDeny || byCall[0].Tool != "provider_list" || byCall[0].ErrorCode != -32602 {
		t.Errorf("entry 0 = %+v, want deny/provider_list/-32602", byCall[0])
	}
	if byCall[1].Decision != AuditDeny || byCall[1].Tool != "provider_list" || byCall[1].ErrorCode != -32602 {
		t.Errorf("entry 1 = %+v, want deny/provider_list/-32602", byCall[1])
	}
	if byCall[2].Decision != AuditAllow || byCall[2].Tool != "router_status" {
		t.Errorf("entry 2 = %+v, want allow/router_status", byCall[2])
	}
}
