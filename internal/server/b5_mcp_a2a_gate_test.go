package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/mcp"
)

// TestBlockB5MCPAndA2AGate validates the complete end-to-end integration
// of Block B5 (Area 9: MCP Server & Agent-to-Agent Handoff).
func TestBlockB5MCPAndA2AGate(t *testing.T) {
	// 1. A2A Subsystem: Agent Card Discovery & Status.
	t.Run("A2AAgentCardAndStatus", func(t *testing.T) {
		srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true, WithListenAddr("127.0.0.1:20128"))

		// Agent Card discovery endpoint (GET /.well-known/agent.json).
		rec := doReq(t, srv, http.MethodGet, "/.well-known/agent.json", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /.well-known/agent.json status = %d, want 200", rec.Code)
		}
		var card map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
			t.Fatalf("decode agent card: %v", err)
		}
		if card["name"] != "Codvian Router" {
			t.Errorf("agent card name = %v, want 'Codvian Router'", card["name"])
		}
		skills, ok := card["skills"].([]any)
		if !ok || len(skills) < 12 {
			t.Errorf("agent card skills count = %d, want >= 6", len(skills))
		}

		// A2A Status endpoint (GET /api/a2a/status).
		rec = doReq(t, srv, http.MethodGet, "/api/a2a/status", "", a2aKeyOne)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/a2a/status status = %d, want 200", rec.Code)
		}
		var st map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatalf("decode /api/a2a/status: %v", err)
		}
		if st["enabled"] != true {
			t.Errorf("a2a status enabled = %v, want true", st["enabled"])
		}
	})

	// 2. A2A End-to-End Tasks & Owner Isolation.
	t.Run("A2ATasksAndOwnerIsolation", func(t *testing.T) {
		srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)

		// Owner 1 submits a task via message/send.
		code, out := a2aPost(t, srv, a2aSendBody("message/send", `{"message":{"parts":[{"text":"Hello from Owner 1"}]}}`), a2aKeyOne)
		if code != http.StatusOK {
			t.Fatalf("message/send status = %d, want 200; out: %v", code, out)
		}
		task := a2aTask(t, out)
		taskID, ok := task["id"].(string)
		if !ok || taskID == "" {
			t.Fatalf("missing task id in response: %v", out)
		}

		// Owner 1 retrieves its own task.
		code, out = a2aPost(t, srv, a2aSendBody("tasks/get", `{"id":"`+taskID+`"}`), a2aKeyOne)
		if code != http.StatusOK {
			t.Fatalf("owner1 tasks/get status = %d, want 200", code)
		}
		gotTask := a2aTask(t, out)
		if gotTask["id"] != taskID {
			t.Errorf("got task id %v, want %s", gotTask["id"], taskID)
		}

		// Owner 2 attempts to retrieve Owner 1's task -> must fail (task not found).
		code, out = a2aPost(t, srv, a2aSendBody("tasks/get", `{"id":"`+taskID+`"}`), a2aKeyTwo)
		if code != http.StatusOK {
			t.Fatalf("owner2 tasks/get status = %d, want 200 (JSON-RPC error)", code)
		}
		errCode := a2aErrCode(t, out)
		errMsg := a2aErrMessage(t, out)
		if errCode == 0 || !strings.Contains(strings.ToLower(errMsg), "not found") {
			t.Errorf("owner2 tasks/get error = (%d, %s), want task not found error; out: %v", errCode, errMsg, out)
		}
	})

	// 3. MCP Subsystem: Live Transport, Tool Discovery & Scope Enforcement.
	t.Run("MCPToolsAndScopeEnforcement", func(t *testing.T) {
		srv, keys := newMCPHTTPTestServer(t)
		enableMCPTransport(t, srv, keys.admin, "streamable")

		// Initialize session using read-scoped key (mcp:read:status).
		sessionID := mcpInitialize(t, srv, keys.read)

		// Verify tool discovery (tools/list): all 24 tools registered.
		rec := doMCPStreamPost(t, srv, keys.read, sessionID, rpcBody(100, "tools/list", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("tools/list status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		env := decodeEnvelope(t, rec)
		result, _ := env["result"].(map[string]any)
		tools, _ := result["tools"].([]any)
		if len(tools) != 32 {
			t.Fatalf("tools/list count = %d, want 32", len(tools))
		}

		// Allowed call under mcp:read:status: router_status should succeed.
		env = mcpToolCall(t, srv, keys.read, sessionID, 101, "router_status", "")
		if code := rpcErrorCode(env); int(code) != -1 {
			t.Fatalf("router_status with mcp:read:status failed with error code %v: %v", code, env)
		}

		// Denied call under mcp:read:status: set_routing_strategy requires mcp:manage:routing.
		env = mcpToolCall(t, srv, keys.read, sessionID, 102, "set_routing_strategy", `{"strategy":"fastest"}`)
		if code := rpcErrorCode(env); int(code) != mcp.ScopeDeniedCode {
			t.Fatalf("set_routing_strategy with read key: error code = %v, want %d (ScopeDeniedCode)", code, mcp.ScopeDeniedCode)
		}

		// Denied call under mcp:read:status: provider_list requires mcp:read:providers.
		env = mcpToolCall(t, srv, keys.read, sessionID, 103, "provider_list", "")
		if code := rpcErrorCode(env); int(code) != mcp.ScopeDeniedCode {
			t.Fatalf("provider_list with read-only status key: error code = %v, want %d", code, mcp.ScopeDeniedCode)
		}

		// Wildcard read key (mcp:read:*) can call provider_list.
		wideSession := mcpInitialize(t, srv, keys.wide)
		env = mcpToolCall(t, srv, keys.wide, wideSession, 104, "provider_list", "")
		if code := rpcErrorCode(env); int(code) != -1 {
			t.Fatalf("provider_list with wide key failed with error code %v: %v", code, env)
		}
	})
}
