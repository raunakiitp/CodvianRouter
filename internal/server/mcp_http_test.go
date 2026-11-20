// M9.2 MCP remote transports (SSE + Streamable HTTP): live HTTP
// tests against a real ServeHTTP mux with real gateway keys.
// No mocks of the transport itself: sessions, rate limits,
// toggles, scope rejection and per-request identity are all
// exercised through the wire, the way a real MCP client sees
// them. Streaming tests (SSE GET, Streamable GET) run against a
// real httptest server on loopback.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// mcpHTTPKeys is the key set one test server is built with. Each
// key carries exactly the scopes its name promises, so a test can
// prove the transport honours (or refuses) that grant.
type mcpHTTPKeys struct {
	admin, read, ops, memory, cache, wide, star, chat, mixed, empty string
	cacheID                                                         string // gateway-key id of the cache key (for the tenant assertion)
	count                                                           int    // keys issued (for the gateway_keys assertion)
}

func newMCPHTTPTestServer(t *testing.T) (*Server, *mcpHTTPKeys) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	keys := &mcpHTTPKeys{}
	issue := func(name string, scopes []string) string {
		k, raw := issueScopedKey(t, cfg, name, scopes)
		keys.count++
		if name == "cache" {
			keys.cacheID = k.ID
		}
		return raw
	}
	keys.admin = issue("admin", []string{"chat", "admin"})
	keys.read = issue("reader", []string{"mcp:read:status"})
	keys.ops = issue("ops", []string{"mcp:manage:ops"})
	keys.memory = issue("memory", []string{"mcp:manage:memory"})
	keys.cache = issue("cache", []string{"mcp:manage:cache", "mcp:destructive"})
	keys.wide = issue("wide", []string{"mcp:read:*"})
	keys.star = issue("star", []string{"*"})
	keys.chat = issue("chat", []string{"chat"})
	keys.mixed = issue("mixed", []string{"chat", "mcp:read:status"})
	keys.empty = issue("empty", []string{})
	srv := newTestServer(t, &fakeEngine{},
		WithConfig(cfg),
		WithDashboardProviders(mcpTestProviders{providers: []provider.DashboardProvider{
			{ID: "p1", Name: "Coder", Enabled: true, WireFormat: "openai"},
			{ID: "p2", Name: "Chatter", Enabled: true, WireFormat: "anthropic"},
		}}),
		WithRegistry(mcpTestRegistry{models: []Model{
			{ID: "a", ProviderID: "p1", OwnedBy: "Coder"},
			{ID: "b", ProviderID: "p1", OwnedBy: "Coder"},
			{ID: "c", ProviderID: "p2", OwnedBy: "Chatter"},
		}}),
	)
	return srv, keys
}

// enableMCPTransport flips one transport on via the M9.7 toggle
// endpoint (admin key). A test that needs a live transport calls
// this first: both transports default OFF (fail-closed).
func enableMCPTransport(t *testing.T, srv *Server, adminKey, transport string) {
	t.Helper()
	rec := doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"`+transport+`","enabled":true}`, adminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable %s: status %d, want 200: %s", transport, rec.Code, rec.Body.String())
	}
}

// disableMCPTransport flips one transport off.
func disableMCPTransport(t *testing.T, srv *Server, adminKey, transport string) {
	t.Helper()
	rec := doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"`+transport+`","enabled":false}`, adminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable %s: status %d, want 200: %s", transport, rec.Code, rec.Body.String())
	}
}

// doMCPStreamPost posts one JSON-RPC message to the Streamable
// transport with the given key and session id.
func doMCPStreamPost(t *testing.T, srv *Server, key, sessionID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/stream", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// rpcBody builds one JSON-RPC request body.
func rpcBody(id int, method string, params string) string {
	if params == "" {
		params = "{}"
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
}

// rpcNotification builds one JSON-RPC notification body (no id).
func rpcNotification(method string, params string) string {
	if params == "" {
		params = "{}"
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s}`, method, params)
}

// decodeEnvelope unmarshals a JSON-RPC envelope from a recorder.
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not a JSON-RPC envelope: %v (body %q)", err, rec.Body.String())
	}
	return env
}

// rpcErrorCode returns the error code of a JSON-RPC error
// envelope, or -1 when the envelope carries a result.
func rpcErrorCode(env map[string]any) float64 {
	errObj, ok := env["error"].(map[string]any)
	if !ok {
		return -1
	}
	code, _ := errObj["code"].(float64)
	return code
}

// mcpInitialize opens a Streamable session and returns its id.
func mcpInitialize(t *testing.T, srv *Server, key string) string {
	t.Helper()
	rec := doMCPStreamPost(t, srv, key, "", rpcBody(1, "initialize", `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	sessionID := rec.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize: no Mcp-Session-Id response header")
	}
	env := decodeEnvelope(t, rec)
	result, ok := env["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize: no result in envelope: %s", rec.Body.String())
	}
	if result["protocolVersion"] != mcp.ProtocolVersion {
		t.Fatalf("initialize: protocolVersion = %v, want %q", result["protocolVersion"], mcp.ProtocolVersion)
	}
	// The client confirms initialisation with a notification:
	// the transport must answer 202 with no body.
	rec = doMCPStreamPost(t, srv, key, sessionID, rpcNotification("notifications/initialized", ""))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notifications/initialized: status %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("notifications/initialized: body %q, want empty", rec.Body.String())
	}
	return sessionID
}

// mcpToolCall calls one tool over an open Streamable session and
// returns the envelope.
func mcpToolCall(t *testing.T, srv *Server, key, sessionID string, id int, name string, args string) map[string]any {
	t.Helper()
	if args == "" {
		args = "{}"
	}
	params := fmt.Sprintf(`{"name":%q,"arguments":%s}`, name, args)
	rec := doMCPStreamPost(t, srv, key, sessionID, rpcBody(id, "tools/call", params))
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call %s: status %d, want 200: %s", name, rec.Code, rec.Body.String())
	}
	return decodeEnvelope(t, rec)
}

// TestMCPHTTPStatusShape pins the GET /api/mcp/status shape the
// M9.7 dashboard builds its toggles against: both transports
// disabled and zero sessions by default (fail-closed).
func TestMCPHTTPStatusShape(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	// GET /api/mcp/status is AuthExempt by M9.7's design (flags
	// and counts only, no keys) — it answers 200 even without a
	// key, with the exact shape the dashboard builds its toggles
	// against.
	rec := doReq(t, srv, http.MethodGet, "/api/mcp/status", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/mcp/status without key: status %d, want 200 (the endpoint is AuthExempt)", rec.Code)
	}
	want := `{"sse":{"enabled":false,"sessions":0},"streamable":{"enabled":false,"sessions":0}}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("status body = %q, want exactly %q", rec.Body.String(), want)
	}
	rec = doReq(t, srv, http.MethodGet, "/api/mcp/status", "", keys.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/mcp/status: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("status body with key = %q, want exactly %q", rec.Body.String(), want)
	}
}

// mcpStatus reads GET /api/mcp/status (AuthExempt) and decodes
// it into the typed response.
func mcpStatus(t *testing.T, srv *Server) mcpStatusResponse {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/api/mcp/status", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/mcp/status: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out mcpStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /api/mcp/status: not a status response: %v (%s)", err, rec.Body.String())
	}
	return out
}

// TestMCPHTTPStatusLiveSessionCounts proves GET /api/mcp/status
// reports LIVE session counts (the mcpTransportStatus → counts()
// seam): 0 with no session, 1 after one initialize, 2 after a
// second, dropping back as each session is closed with DELETE.
func TestMCPHTTPStatusLiveSessionCounts(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")
	if st := mcpStatus(t, srv); st.SSE.Sessions != 0 || st.Streamable.Sessions != 0 {
		t.Fatalf("sessions before any session = sse %d streamable %d, want 0/0", st.SSE.Sessions, st.Streamable.Sessions)
	}
	idA := mcpInitialize(t, srv, keys.read)
	if st := mcpStatus(t, srv); st.Streamable.Sessions != 1 || st.SSE.Sessions != 0 {
		t.Fatalf("sessions after one initialize = sse %d streamable %d, want 0/1", st.SSE.Sessions, st.Streamable.Sessions)
	}
	idB := mcpInitialize(t, srv, keys.ops)
	if st := mcpStatus(t, srv); st.Streamable.Sessions != 2 {
		t.Fatalf("sessions after two initializes = %d, want 2", st.Streamable.Sessions)
	}
	rec := doReq(t, srv, http.MethodDelete, "/api/mcp/stream/"+idA, "", keys.read)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE session A: status %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if st := mcpStatus(t, srv); st.Streamable.Sessions != 1 {
		t.Fatalf("sessions after closing one = %d, want 1", st.Streamable.Sessions)
	}
	rec = doReq(t, srv, http.MethodDelete, "/api/mcp/stream/"+idB, "", keys.ops)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE session B: status %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if st := mcpStatus(t, srv); st.Streamable.Sessions != 0 {
		t.Fatalf("sessions after closing both = %d, want 0", st.Streamable.Sessions)
	}
}

// TestMCPHTTPNoKey401 proves every transport route refuses a
// keyless request, even in a server built without auth-relaxing.
func TestMCPHTTPNoKey401(t *testing.T) {
	srv, _ := newMCPHTTPTestServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/mcp/sse"},
		{http.MethodPost, "/api/mcp/sse"},
		{http.MethodPost, "/api/mcp/stream"},
		{http.MethodGet, "/api/mcp/stream"},
		{http.MethodDelete, "/api/mcp/stream/some-id"},
	} {
		rec := doReq(t, srv, tc.method, tc.path, "{}", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without key: status %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

// TestMCPHTTPTransportToggle proves the runtime toggle: disabled
// (the default) answers 503, enabled serves, disabled again
// answers 503, re-enabled serves again. A non-admin key may not
// flip the toggle (403); an unknown transport is a 400.
func TestMCPHTTPTransportToggle(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	// Disabled by default: 503 on both transports.
	rec := doMCPStreamPost(t, srv, keys.read, "", rpcBody(1, "initialize", `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("streamable disabled: status %d, want 503: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/api/mcp/sse", "", keys.read)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("SSE disabled: status %d, want 503: %s", rec.Code, rec.Body.String())
	}
	// A non-admin key may not flip the toggle.
	rec = doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"streamable","enabled":true}`, keys.read)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("toggle with read key: status %d, want 403", rec.Code)
	}
	// An unknown transport is a 400.
	rec = doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"smoke-signal","enabled":true}`, keys.admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("toggle unknown transport: status %d, want 400", rec.Code)
	}
	// Enable: serves.
	enableMCPTransport(t, srv, keys.admin, "streamable")
	sessionID := mcpInitialize(t, srv, keys.read)
	// Disable: 503 again.
	disableMCPTransport(t, srv, keys.admin, "streamable")
	rec = doMCPStreamPost(t, srv, keys.read, sessionID, rpcBody(2, "tools/list", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("streamable re-disabled: status %d, want 503: %s", rec.Code, rec.Body.String())
	}
	// Re-enable: the session still works.
	enableMCPTransport(t, srv, keys.admin, "streamable")
	env := mcpToolCall(t, srv, keys.read, sessionID, 3, "router_status", "")
	if code := rpcErrorCode(env); code != -1 {
		t.Fatalf("router_status after re-enable: error code %v, want a result: %v", code, env)
	}
}

// TestMCPHTTPStarScopeRejected proves a key carrying a bare "*"
// scope is rejected at the transport with 403 naming the scope
// — "*" is the local-operator-only grant and must never reach a
// network identity.
func TestMCPHTTPStarScopeRejected(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")
	rec := doMCPStreamPost(t, srv, keys.star, "", rpcBody(1, "initialize", `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("star key: status %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not a valid MCP scope") || !strings.Contains(rec.Body.String(), "*") {
		t.Fatalf("star key: 403 body %q must name the offending scope", rec.Body.String())
	}
	// The same key is refused on the SSE transport too.
	enableMCPTransport(t, srv, keys.admin, "sse")
	rec = doReq(t, srv, http.MethodGet, "/api/mcp/sse", "", keys.star)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("star key on SSE: status %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// TestMCPHTTPAdminAndChatScopesRejected proves the M9.1 N4 rule:
// a key with HTTP-namespace scopes (admin, chat, or a mix with
// an mcp: scope) gains NO MCP tools — the guard's "admin implies
// every scope" must not leak across namespaces.
func TestMCPHTTPAdminAndChatScopesRejected(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")
	for name, key := range map[string]string{
		"admin": keys.admin,
		"chat":  keys.chat,
		"mixed": keys.mixed,
	} {
		rec := doMCPStreamPost(t, srv, key, "", rpcBody(1, "initialize", `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s key: status %d, want 403: %s", name, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), "not a valid MCP scope") {
			t.Errorf("%s key: 403 body %q must name the invalid MCP scope", name, rec.Body.String())
		}
	}
}

// TestMCPHTTPStreamableInitializeAndCall is the happy-path live
// flow over Streamable HTTP: initialize, tools/list, and a tool
// call per scope class, all against live server state.
func TestMCPHTTPStreamableInitializeAndCall(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")
	sessionID := mcpInitialize(t, srv, keys.read)

	// tools/list: the 3 built-ins are present (M9.3/M9.4/M9.5
	// register more; the count is theirs, the presence is ours).
	rec := doMCPStreamPost(t, srv, keys.read, sessionID, rpcBody(2, "tools/list", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec)
	result, _ := env["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		if m, ok := tool.(map[string]any); ok {
			names[m["name"].(string)] = true
		}
	}
	for _, want := range []string{"router_status", "provider_list", "model_list"} {
		if !names[want] {
			t.Errorf("tools/list missing built-in %q (got %v)", want, names)
		}
	}

	// router_status: live gateway-key count.
	env = mcpToolCall(t, srv, keys.read, sessionID, 3, "router_status", "")
	if code := rpcErrorCode(env); code != -1 {
		t.Fatalf("router_status: error code %v: %v", code, env)
	}
	status, _ := env["result"].(map[string]any)["structuredContent"].(map[string]any)
	if got := status["gateway_keys"]; got != float64(keys.count) {
		t.Errorf("router_status gateway_keys = %v, want %d (live count)", got, keys.count)
	}

	// The read key is narrow: provider_list / model_list are
	// outside mcp:read:status and must be denied -32002.
	for _, name := range []string{"provider_list", "model_list"} {
		env = mcpToolCall(t, srv, keys.read, sessionID, 4, name, "")
		if code := rpcErrorCode(env); code != mcp.ScopeDeniedCode {
			t.Errorf("%s with read-only key: error code %v, want -32002", name, code)
		}
	}

	// The mcp:read:* wildcard key can call all three.
	wideSession := mcpInitialize(t, srv, keys.wide)
	for _, name := range []string{"router_status", "provider_list", "model_list"} {
		env = mcpToolCall(t, srv, keys.wide, wideSession, 5, name, "")
		if code := rpcErrorCode(env); code != -1 {
			t.Errorf("%s with mcp:read:* key: error code %v, want a result: %v", name, code, env)
			continue
		}
		body, _ := json.Marshal(env["result"].(map[string]any)["structuredContent"])
		for _, want := range []string{"p1", "p2"} {
			if name == "provider_list" && !strings.Contains(string(body), want) {
				t.Errorf("provider_list result %s missing provider %q", body, want)
			}
		}
		for _, want := range []string{"a", "b", "c"} {
			if name == "model_list" && !strings.Contains(string(body), want) {
				t.Errorf("model_list result %s missing model %q", body, want)
			}
		}
	}

	// The ops key can call db_health (mcp:manage:ops) and gets a
	// live verdict from the data-dir files.
	opsSession := mcpInitialize(t, srv, keys.ops)
	env = mcpToolCall(t, srv, keys.ops, opsSession, 6, "db_health", "")
	if code := rpcErrorCode(env); code != -1 {
		t.Fatalf("db_health with ops key: error code %v: %v", code, env)
	}
	health, _ := env["result"].(map[string]any)["structuredContent"].(map[string]any)
	if health["healthy"] != true {
		t.Errorf("db_health healthy = %v, want true (no database files exist yet, nothing is broken)", health["healthy"])
	}
}

// TestMCPHTTPEmptyScopeKey proves a key with no scopes at all can
// initialise and list tools, but every tool call is denied
// -32002 by the MCP layer (fail-closed, not a transport error).
func TestMCPHTTPEmptyScopeKey(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")
	sessionID := mcpInitialize(t, srv, keys.empty)
	rec := doMCPStreamPost(t, srv, keys.empty, sessionID, rpcBody(2, "tools/list", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list with empty-scope key: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	env := mcpToolCall(t, srv, keys.empty, sessionID, 3, "router_status", "")
	if code := rpcErrorCode(env); code != mcp.ScopeDeniedCode {
		t.Fatalf("router_status with empty-scope key: error code %v, want -32002", code)
	}
}

// TestMCPHTTPUnknownSession proves the session-addressing rules:
// a non-initialize without an id is a 400, an unknown or foreign
// id is a 404 on POST, GET and DELETE alike.
func TestMCPHTTPUnknownSession(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")
	// Non-initialize without a session id: 400.
	rec := doMCPStreamPost(t, srv, keys.read, "", rpcBody(1, "tools/list", ""))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("tools/list without session id: status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec)
	if code := rpcErrorCode(env); code != mcpUnknownSessionCode {
		t.Errorf("tools/list without session id: JSON-RPC code %v, want -32000", code)
	}
	// Unknown id, non-initialize: 404.
	rec = doMCPStreamPost(t, srv, keys.read, "deadbeefdeadbeefdeadbeefdeadbeef", rpcBody(1, "tools/list", ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("tools/list with unknown session id: status %d, want 404: %s", rec.Code, rec.Body.String())
	}
	// Unknown id on GET: 404.
	req := httptest.NewRequest(http.MethodGet, "/api/mcp/stream", nil)
	req.Header.Set("Authorization", "Bearer "+keys.read)
	req.Header.Set("Mcp-Session-Id", "deadbeefdeadbeefdeadbeefdeadbeef")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("GET with unknown session id: status %d, want 404: %s", rec2.Code, rec2.Body.String())
	}
	// Unknown id on DELETE: 404.
	rec = doReq(t, srv, http.MethodDelete, "/api/mcp/stream/deadbeefdeadbeefdeadbeefdeadbeef", "", keys.read)
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown session: status %d, want 404: %s", rec.Code, rec.Body.String())
	}
	// An initialize with a stale id opens a FRESH session
	// (auto-recovery after a restart or idle eviction).
	rec = doMCPStreamPost(t, srv, keys.read, "deadbeefdeadbeefdeadbeefdeadbeef", rpcBody(1, "initialize", `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize with stale id: status %d, want 200 (fresh session): %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Mcp-Session-Id"); got == "" || got == "deadbeefdeadbeefdeadbeefdeadbeef" {
		t.Fatalf("initialize with stale id: Mcp-Session-Id %q, want a fresh id", got)
	}
}

// TestMCPHTTPSessionIsolation proves no session state leaks
// across keys: B's requests against A's session are 404s, and
// after A deletes the session it is gone for everyone.
func TestMCPHTTPSessionIsolation(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")
	sessionA := mcpInitialize(t, srv, keys.read)
	// B cannot use A's session.
	rec := doMCPStreamPost(t, srv, keys.ops, sessionA, rpcBody(2, "tools/list", ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign key on session: status %d, want 404: %s", rec.Code, rec.Body.String())
	}
	// B cannot delete A's session.
	rec = doReq(t, srv, http.MethodDelete, "/api/mcp/stream/"+sessionA, "", keys.ops)
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign key DELETE: status %d, want 404: %s", rec.Code, rec.Body.String())
	}
	// A can still use it.
	env := mcpToolCall(t, srv, keys.read, sessionA, 3, "router_status", "")
	if code := rpcErrorCode(env); code != -1 {
		t.Fatalf("owner call after foreign attempts: error code %v", code)
	}
	// A deletes it: 204, then it is gone.
	rec = doReq(t, srv, http.MethodDelete, "/api/mcp/stream/"+sessionA, "", keys.read)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner DELETE: status %d, want 204: %s", rec.Code, rec.Body.String())
	}
	rec = doMCPStreamPost(t, srv, keys.read, sessionA, rpcBody(4, "tools/list", ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("call after DELETE: status %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestMCPHTTPIdentityReachesHandlersOverHTTP proves the
// follow-up fix end to end: a remote cache_flush call sees the
// per-request gateway key as its tenant — not "anonymous", not
// the server default.
func TestMCPHTTPIdentityReachesHandlersOverHTTP(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")
	sessionID := mcpInitialize(t, srv, keys.cache)
	env := mcpToolCall(t, srv, keys.cache, sessionID, 2, "cache_flush", "")
	if code := rpcErrorCode(env); code != -1 {
		t.Fatalf("cache_flush: error code %v: %v", code, env)
	}
	flush, _ := env["result"].(map[string]any)["structuredContent"].(map[string]any)
	if flush["tenant"] != keys.cacheID {
		t.Fatalf("cache_flush tenant = %v, want the gateway-key id %q (the per-request identity, not anonymous)", flush["tenant"], keys.cacheID)
	}
	if flush["flushed"] != float64(0) {
		t.Errorf("cache_flush flushed = %v, want 0 (the test cache holds nothing)", flush["flushed"])
	}
}

// TestMCPHTTPRateLimit proves the transport rate limiter trips
// at exactly the configured per-key burst: of 45 identical
// requests from one key, the first 30 pass the limiter (404,
// unknown session) and the next 15 answer 429 with Retry-After
// (per-key burst 30, refill 1/s = 60/min sustained), while a
// different key is unaffected. The exact per-IP burst (120,
// 300/min) and both refill rates are pinned deterministically
// in TestMCPRateLimiterExactLimits and over HTTP in
// TestMCPHTTPRateLimitIPBucket. Timing assumption: the 45-request
// loop runs in ~10 ms, far below the 1/s refill — a slow run
// under heavy load could refill a token mid-loop and shift the
// exact 30/15 split, so treat a marginal failure here as load
// noise, not a limiter regression.
func TestMCPHTTPRateLimit(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "sse")
	limited, notFound := 0, 0
	for i := 0; i < 45; i++ {
		rec := doReq(t, srv, http.MethodPost, "/api/mcp/sse?session_id=deadbeefdeadbeefdeadbeefdeadbeef", rpcBody(1, "tools/list", ""), keys.read)
		switch rec.Code {
		case http.StatusTooManyRequests:
			limited++
			if rec.Header().Get("Retry-After") == "" {
				t.Errorf("429 without Retry-After header")
			}
		case http.StatusNotFound:
			notFound++
		default:
			t.Errorf("request %d: status %d, want 404 (before the bucket empties) or 429 (after)", i, rec.Code)
		}
	}
	if limited != 15 {
		t.Errorf("429s = %d, want exactly 15 of 45 (the first 30 spend the per-key burst of 30, the rest trip it)", limited)
	}
	if notFound != 30 {
		t.Errorf("404s = %d, want exactly 30 of 45 (the per-key burst is 30)", notFound)
	}
	// A different key still gets 404, not 429: the bucket is
	// per-key, not global.
	rec := doReq(t, srv, http.MethodPost, "/api/mcp/sse?session_id=deadbeefdeadbeefdeadbeefdeadbeef", rpcBody(1, "tools/list", ""), keys.ops)
	if rec.Code != http.StatusNotFound {
		t.Errorf("different key after trip: status %d, want 404 (its own bucket is full)", rec.Code)
	}
}

// TestMCPRateLimiterExactLimits pins the transport rate limiter's
// exact numbers with a frozen clock (no wall-clock flakiness):
// per-key burst exactly 30 with refill 1/s (60/min sustained),
// per-IP burst exactly 120 with refill 5/s (300/min sustained),
// and per-key / per-IP isolation — exhausting one bucket never
// touches another. NOTE: there is no 15/min limit anywhere in
// the limiter; the "15" in TestMCPHTTPRateLimit is the count of
// 429s in its 45-request burst (45 - 30), not a limit.
func TestMCPRateLimiterExactLimits(t *testing.T) {
	l := newMCPRateLimiter()
	now := time.Now()
	// Per-key: exactly 30 allowed, the 31st denied.
	for i := 0; i < int(mcpRateKeyBurst); i++ {
		if !l.allowKey("key-A", now) {
			t.Fatalf("per-key request %d: denied, want the first %d allowed (burst is %v)", i+1, int(mcpRateKeyBurst), mcpRateKeyBurst)
		}
	}
	if l.allowKey("key-A", now) {
		t.Errorf("per-key request 31: allowed, want denied (burst is exactly %v)", mcpRateKeyBurst)
	}
	// A different key has its own full bucket.
	if !l.allowKey("key-B", now) {
		t.Errorf("different key: denied, want allowed (per-key buckets are separate)")
	}
	// Refill is exactly 1/s: half a second buys nothing, a full
	// second buys exactly one token.
	if l.allowKey("key-A", now.Add(500*time.Millisecond)) {
		t.Errorf("per-key after 500ms: allowed, want denied (refill is 1/s, half a token is not spendable)")
	}
	if !l.allowKey("key-A", now.Add(time.Second)) {
		t.Errorf("per-key after 1s: denied, want allowed (one refilled token)")
	}
	if l.allowKey("key-A", now.Add(time.Second)) {
		t.Errorf("per-key second request after 1s: allowed, want denied (only one token refilled)")
	}
	// 60s refills the whole burst (60 tokens, capped at 30).
	for i := 0; i < int(mcpRateKeyBurst); i++ {
		if !l.allowKey("key-A", now.Add(61*time.Second)) {
			t.Fatalf("per-key request %d after 60s refill: denied, want the full burst of %d back", i+1, int(mcpRateKeyBurst))
		}
	}
	if l.allowKey("key-A", now.Add(61*time.Second)) {
		t.Errorf("per-key request 31 after 60s refill: allowed, want denied (refill caps at the burst)")
	}
	// Per-IP: exactly 120 allowed, the 121st denied.
	for i := 0; i < int(mcpRateIPBurst); i++ {
		if !l.allowIP("203.0.113.9", now) {
			t.Fatalf("per-IP request %d: denied, want the first %d allowed (burst is %v)", i+1, int(mcpRateIPBurst), mcpRateIPBurst)
		}
	}
	if l.allowIP("203.0.113.9", now) {
		t.Errorf("per-IP request 121: allowed, want denied (burst is exactly %v)", mcpRateIPBurst)
	}
	// A different IP has its own full bucket.
	if !l.allowIP("198.51.100.7", now) {
		t.Errorf("different IP: denied, want allowed (per-IP buckets are separate)")
	}
	// 60s refills the whole IP burst (300 tokens, capped at 120).
	for i := 0; i < int(mcpRateIPBurst); i++ {
		if !l.allowIP("203.0.113.9", now.Add(61*time.Second)) {
			t.Fatalf("per-IP request %d after 60s refill: denied, want the full burst of %d back", i+1, int(mcpRateIPBurst))
		}
	}
	if l.allowIP("203.0.113.9", now.Add(61*time.Second)) {
		t.Errorf("per-IP request 121 after 60s refill: allowed, want denied (refill caps at the burst)")
	}
}

// TestMCPHTTPRateLimitIPBucket proves the per-IP bucket over HTTP:
// four keys × 30 requests from one IP spend exactly the 120-token
// IP bucket, so a fifth fresh key from the same IP is 429 — while
// the same key from a different IP still passes (404, unknown
// session). Each key stays inside its own 30-token per-key bucket,
// so only the IP bucket can be the tripper. The limiter clock is
// frozen for the test (Gate 1): the fill loops take far longer
// than ~30 ms under -race, and the 5/s IP refill would otherwise
// top the bucket up mid-loop and shift the exact counts.
func TestMCPHTTPRateLimitIPBucket(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	frozen := time.Now()
	mcpHTTPStateFor(srv).rate.now = func() time.Time { return frozen }
	enableMCPTransport(t, srv, keys.admin, "sse")
	ipA := "203.0.113.9:1234"
	ipB := "198.51.100.7:1234"
	fill := []string{keys.read, keys.ops, keys.memory, keys.cache}
	for _, key := range fill {
		for i := 0; i < int(mcpRateKeyBurst); i++ {
			rec := doReqPeer(t, srv, http.MethodPost, "/api/mcp/sse?session_id=deadbeefdeadbeefdeadbeefdeadbeef", rpcBody(1, "tools/list", ""), key, ipA)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("fill request %d from IP-A: status %d, want 404 (inside both the per-key burst of 30 and the per-IP burst of 120)", i+1, rec.Code)
			}
		}
	}
	// The IP bucket is now exactly spent: a fresh key from the
	// same IP trips it.
	rec := doReqPeer(t, srv, http.MethodPost, "/api/mcp/sse?session_id=deadbeefdeadbeefdeadbeefdeadbeef", rpcBody(1, "tools/list", ""), keys.wide, ipA)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("fresh key from IP-A after 120 requests: status %d, want 429 (per-IP burst is exactly 120)", rec.Code)
	}
	// The same fresh key from a different IP is unaffected.
	rec = doReqPeer(t, srv, http.MethodPost, "/api/mcp/sse?session_id=deadbeefdeadbeefdeadbeefdeadbeef", rpcBody(1, "tools/list", ""), keys.wide, ipB)
	if rec.Code != http.StatusNotFound {
		t.Errorf("fresh key from IP-B after IP-A tripped: status %d, want 404 (per-IP buckets are separate)", rec.Code)
	}
}

// TestMCPHTTPMalformedBatchCreatesNoSession proves a batch (JSON
// array body) never opens a session: the session is only created
// once the message parses as a single object, so a malformed
// batch leaves no orphan session behind — with no session id the
// batch is a 400, against a stale id it is a 404, and the live
// counts stay 0 either way. A single-object initialize still
// opens exactly one session (positive control).
func TestMCPHTTPMalformedBatchCreatesNoSession(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")
	st := mcpHTTPStateFor(srv)
	if sse, streamable := st.counts(); sse != 0 || streamable != 0 {
		t.Fatalf("sessions before = sse %d streamable %d, want 0/0", sse, streamable)
	}
	initParams := `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`
	batch := `[{"jsonrpc":"2.0","id":1,"method":"initialize","params":` + initParams + `}]`
	rec := doMCPStreamPost(t, srv, keys.read, "", batch)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("batch without session id: status %d, want 400 (batches never open a session): %s", rec.Code, rec.Body.String())
	}
	rec = doMCPStreamPost(t, srv, keys.read, "", `[{"jsonrpc":"2.0","id":1,"method":"initialize",`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed batch without session id: status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	rec = doMCPStreamPost(t, srv, keys.read, "deadbeefdeadbeefdeadbeefdeadbeef", batch)
	if rec.Code != http.StatusNotFound {
		t.Errorf("batch with stale session id: status %d, want 404 (batches never open a session): %s", rec.Code, rec.Body.String())
	}
	if sse, streamable := st.counts(); sse != 0 || streamable != 0 {
		t.Errorf("sessions after malformed batches = sse %d streamable %d, want 0/0 (orphan session)", sse, streamable)
	}
	if out := mcpStatus(t, srv); out.Streamable.Sessions != 0 {
		t.Errorf("GET /api/mcp/status after malformed batches: streamable.sessions = %d, want 0", out.Streamable.Sessions)
	}
	id := mcpInitialize(t, srv, keys.read)
	if _, streamable := st.counts(); streamable != 1 {
		t.Errorf("sessions after a single-object initialize = %d, want 1", streamable)
	}
	rec = doReq(t, srv, http.MethodDelete, "/api/mcp/stream/"+id, "", keys.read)
	if rec.Code != http.StatusNoContent {
		t.Errorf("DELETE the control session: status %d, want 204", rec.Code)
	}
}

// TestMCPHTTPSessionCapsAndEviction is the white-box half of the
// session lifecycle: per-key and per-transport caps, idle
// eviction, and the open-stream exemption.
func TestMCPHTTPSessionCapsAndEviction(t *testing.T) {
	srv, _ := newMCPHTTPTestServer(t)
	st := mcpHTTPStateFor(srv)
	st.mu.Lock()
	// Per-key cap: 16 sessions for one key, the 17th refused.
	var ids []string
	for i := 0; i < mcpMaxSessionsPerKey; i++ {
		sess, err := st.newSessionLocked(mcpTransportStreamable, "key-A")
		if err != nil {
			st.mu.Unlock()
			t.Fatalf("session %d for key-A: %v", i, err)
		}
		ids = append(ids, sess.id)
	}
	if _, err := st.newSessionLocked(mcpTransportStreamable, "key-A"); err == nil {
		st.mu.Unlock()
		t.Fatal("17th session for one key: no error, want the per-key cap to refuse it")
	}
	// Per-transport cap: fill the table to 64 across keys, the
	// 65th refused.
	for k := 1; k <= 3; k++ {
		for i := 0; i < mcpMaxSessionsPerKey; i++ {
			if _, err := st.newSessionLocked(mcpTransportStreamable, fmt.Sprintf("key-%d", k)); err != nil {
				st.mu.Unlock()
				t.Fatalf("fill session %d for key-%d: %v", i, k, err)
			}
		}
	}
	if _, err := st.newSessionLocked(mcpTransportStreamable, "key-4"); err == nil {
		st.mu.Unlock()
		t.Fatal("65th session on the transport: no error, want the per-transport cap to refuse it")
	}
	// Session ids are 32 hex chars (128 bits of crypto/rand).
	for _, id := range ids {
		if len(id) != 32 {
			t.Errorf("session id %q: length %d, want 32 hex chars", id, len(id))
		}
		for _, c := range id {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				t.Errorf("session id %q: not lowercase hex", id)
				break
			}
		}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate session id %q", id)
		}
		seen[id] = true
	}
	// Idle eviction: backdate one session past the timeout, then
	// a count (which sweeps) drops it — but a session with an
	// open stream survives the sweep. The sweep itself is due at
	// most once a minute, so the test backdates lastSweep as well
	// to make it due.
	victim := st.streamSessions[ids[0]]
	victim.lastActivity = time.Now().Add(-mcpSessionIdleTimeout - time.Minute)
	open := st.streamSessions[ids[1]]
	open.lastActivity = time.Now().Add(-mcpSessionIdleTimeout - time.Minute)
	open.streams = 1
	st.lastSweep = time.Now().Add(-mcpSessionSweepInterval - time.Second)
	st.mu.Unlock()
	sse, streamable := st.counts()
	if streamable != mcpMaxSessionsPerTransport-1 {
		t.Errorf("sessions after sweep = %d, want %d (one idle evicted, one with an open stream kept)", streamable, mcpMaxSessionsPerTransport-1)
	}
	if sse != 0 {
		t.Errorf("SSE sessions = %d, want 0", sse)
	}
}

// TestValidMCPGrant is the table for the transport's scope gate:
// known mcp: scopes and trailing-* wildcards that cover at least
// one known scope pass; everything else — bare "*", HTTP
// scopes, mid-string wildcards, malformed mcp: names — fails.
func TestValidMCPGrant(t *testing.T) {
	cases := []struct {
		scope string
		want  bool
	}{
		{"mcp:read:status", true},
		{"mcp:read:providers", true},
		{"mcp:read:models", true},
		{"mcp:manage:routing", true},
		{"mcp:manage:cost", true},
		{"mcp:manage:cache", true},
		{"mcp:manage:memory", true},
		{"mcp:manage:ops", true},
		{"mcp:destructive", true},
		{"mcp:read:*", true},
		{"mcp:manage:*", true},
		{"mcp:*", true},
		{"*", false},            // the local-operator-only grant
		{"admin", false},        // HTTP namespace: must not cross over
		{"chat", false},         // HTTP namespace: must not cross over
		{"models", false},       // HTTP namespace: must not cross over
		{"mcp:evil:*", false},   // wildcard over nothing known
		{"mcp:", false},         // malformed
		{"mcp:read", false},     // not a known scope, not a wildcard
		{"mcp:*:status", false}, // mid-string * is literal, matches nothing
		{"", false},
	}
	for _, c := range cases {
		if got := validMCPGrant(c.scope); got != c.want {
			t.Errorf("validMCPGrant(%q) = %v, want %v", c.scope, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Live-server streaming tests (real TCP, real SSE streams).
// ---------------------------------------------------------------------------

// sseEvent is one parsed SSE event.
type sseEvent struct {
	event string
	data  string
}

// readSSEEvent reads one SSE event (lines until a blank line)
// from r. Comment lines (": ping") are skipped.
func readSSEEvent(t *testing.T, r *bufio.Reader) (sseEvent, error) {
	t.Helper()
	var ev sseEvent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return ev, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if ev.event != "" || ev.data != "" {
				return ev, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // heartbeat comment
		}
		if strings.HasPrefix(line, "event: ") {
			ev.event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			if ev.data != "" {
				ev.data += "\n"
			}
			ev.data += strings.TrimPrefix(line, "data: ")
		}
	}
}

// readSSEEventTimeout reads one event or fails the test after d.
func readSSEEventTimeout(t *testing.T, ch <-chan sseEvent, errCh <-chan error, d time.Duration) sseEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case err := <-errCh:
		t.Fatalf("SSE stream read failed: %v", err)
	case <-time.After(d):
		t.Fatal("timed out waiting for an SSE event")
	}
	return sseEvent{}
}

// streamEvents pumps SSE events from r into ch until the stream
// ends (then errCh gets the read error).
func streamEvents(t *testing.T, r *bufio.Reader, ch chan<- sseEvent, errCh chan<- error) {
	t.Helper()
	for {
		ev, err := readSSEEvent(t, r)
		if err != nil {
			errCh <- err
			return
		}
		ch <- ev
	}
}

// TestMCPHTTPSSEFlow is the end-to-end SSE flow over a live
// server: GET opens the stream (endpoint event), POST sends
// initialize (202, response on the stream), a tool call round
// trips the same way, and closing the GET terminates the
// session.
func TestMCPHTTPSSEFlow(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "sse")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client := ts.Client()

	// GET opens the stream.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/mcp/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+keys.read)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/mcp/sse: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/mcp/sse: status %d, want 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("GET /api/mcp/sse Content-Type = %q, want text/event-stream", ct)
	}
	events := make(chan sseEvent, 8)
	errs := make(chan error, 1)
	go streamEvents(t, bufio.NewReader(resp.Body), events, errs)
	ev := readSSEEventTimeout(t, events, errs, 5*time.Second)
	if ev.event != "endpoint" {
		t.Fatalf("first SSE event = %q, want endpoint", ev.event)
	}
	sessionID := strings.TrimPrefix(ev.data, "/api/mcp/sse?session_id=")
	if sessionID == ev.data || sessionID == "" {
		t.Fatalf("endpoint data = %q, want /api/mcp/sse?session_id=<id>", ev.data)
	}

	// POST initialize: 202, response on the stream.
	post := func(body string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/mcp/sse?session_id="+sessionID, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+keys.read)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /api/mcp/sse: %v", err)
		}
		return resp
	}
	resp2 := post(rpcBody(1, "initialize", `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`))
	if resp2.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Fatalf("POST initialize: status %d, want 202: %s", resp2.StatusCode, body)
	}
	resp2.Body.Close()
	ev = readSSEEventTimeout(t, events, errs, 5*time.Second)
	if ev.event != "message" {
		t.Fatalf("initialize response event = %q, want message", ev.event)
	}
	env := map[string]any{}
	if err := json.Unmarshal([]byte(ev.data), &env); err != nil {
		t.Fatalf("initialize response is not JSON: %v (data %q)", err, ev.data)
	}
	result, _ := env["result"].(map[string]any)
	if result["protocolVersion"] != mcp.ProtocolVersion {
		t.Fatalf("initialize protocolVersion = %v, want %q", result["protocolVersion"], mcp.ProtocolVersion)
	}

	// A tool call round-trips the same way. The read key cannot
	// call model_list (mcp:read:models) — the -32002 denial
	// itself proves the stream carries real tool results.
	resp2 = post(rpcBody(2, "tools/call", `{"name":"model_list","arguments":{}}`))
	if resp2.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Fatalf("POST tools/call: status %d, want 202: %s", resp2.StatusCode, body)
	}
	resp2.Body.Close()
	ev = readSSEEventTimeout(t, events, errs, 5*time.Second)
	if ev.event != "message" {
		t.Fatalf("tool-call response event = %q, want message", ev.event)
	}
	env = map[string]any{}
	if err := json.Unmarshal([]byte(ev.data), &env); err != nil {
		t.Fatalf("tool-call response is not JSON: %v", err)
	}
	if code := rpcErrorCode(env); code != mcp.ScopeDeniedCode {
		t.Fatalf("model_list over SSE with read-only key: error code %v, want -32002", code)
	}

	// Closing the GET terminates the session: a further POST is
	// a 404.
	resp.Body.Close()
	time.Sleep(100 * time.Millisecond) // let the handler notice the disconnect
	resp2 = post(rpcBody(3, "tools/list", ""))
	if resp2.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Fatalf("POST after GET closed: status %d, want 404 (session terminated): %s", resp2.StatusCode, body)
	}
	resp2.Body.Close()
}

// TestMCPHTTPStreamableGETStream proves the Streamable GET stream
// stays open for server→client messages and, unlike SSE, closing
// it does NOT terminate the session: the session still answers
// POSTs afterwards, until DELETE.
func TestMCPHTTPStreamableGETStream(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client := ts.Client()

	sessionID := mcpInitialize(t, srv, keys.read)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/mcp/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+keys.read)
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/mcp/stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("GET /api/mcp/stream: status %d, want 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("GET /api/mcp/stream Content-Type = %q, want text/event-stream", ct)
	}
	// Close the stream: the session must survive.
	resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	env := mcpToolCall(t, srv, keys.read, sessionID, 2, "router_status", "")
	if code := rpcErrorCode(env); code != -1 {
		t.Fatalf("call after GET closed: error code %v, want a result (session survives)", code)
	}
	// DELETE terminates it.
	rec := doReq(t, srv, http.MethodDelete, "/api/mcp/stream/"+sessionID, "", keys.read)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status %d, want 204: %s", rec.Code, rec.Body.String())
	}
	rec = doMCPStreamPost(t, srv, keys.read, sessionID, rpcBody(3, "tools/list", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("call after DELETE: status %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// mcpToolResult extracts the structuredContent of a successful
// tools/call envelope, failing the test otherwise.
func mcpToolResult(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	result, _ := env["result"].(map[string]any)
	if result == nil {
		t.Fatalf("no result in envelope: %v", env)
	}
	sc, _ := result["structuredContent"].(map[string]any)
	if sc == nil {
		t.Fatalf("no structuredContent in result: %v", result)
	}
	return sc
}

// mcpToolError extracts the (code, message) of an error envelope,
// failing the test when the envelope carries a result instead.
func mcpToolError(t *testing.T, env map[string]any) (float64, string) {
	t.Helper()
	errObj, _ := env["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected an error envelope, got: %v", env)
	}
	code, _ := errObj["code"].(float64)
	msg, _ := errObj["message"].(string)
	return code, msg
}

// TestMCPHTTPM93LedgerBackend proves the M9.3 backend is reachable
// over the Streamable HTTP transport against the serve process's
// live ledger: cost_report / check_quota / set_budget_guard read
// and write real ledger rows, pick_fastest_model ranks on measured
// latency, explain/simulate dry-run from the live provider feed,
// set_routing_strategy persists, and create_combo succeeds through
// the same ComboStore.Create path POST /v1/combos runs. The cost
// assertions also prove caller-only scoping end to end: the
// reader's report covers exactly its own usage, never the other
// key's rows seeded alongside it.
func TestMCPHTTPM93LedgerBackend(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	readKey, readRaw := issueScopedKey(t, cfg, "cost-reader", []string{"mcp:read:cost"})
	manageKey, manageRaw := issueScopedKey(t, cfg, "cost-manager", []string{"mcp:manage:cost"})
	_, routerRaw := issueScopedKey(t, cfg, "router", []string{"mcp:read:routing", "mcp:manage:routing"})
	_, wideRaw := issueScopedKey(t, cfg, "wide", []string{"mcp:read:*"})
	_, adminRaw := issueScopedKey(t, cfg, "admin", []string{"chat", "admin"})
	// Combo validation reads providers from cfg (not the
	// dashboard feed), so p1/p2 must exist here for
	// create_combo to validate — with keys, so the created
	// combo is usable.
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Name: "Coder", BaseURL: "https://p1.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "openai", Models: []provider.Model{{ID: "a"}, {ID: "b"}}, Enabled: true},
		{ID: "p2", Name: "Chatter", BaseURL: "https://p2.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "anthropic", Models: []provider.Model{{ID: "c"}}, Enabled: true},
	}

	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	prices := ledger.Prices{
		ledger.PriceKey{Model: "a"}: {PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6},
		ledger.PriceKey{Model: "c"}: {PromptPerToken: 5.0 / 1e6, CompletionPerToken: 15.0 / 1e6},
	}
	seed := func(keyID, providerID, model string, prompt, completion int, latency int64) {
		t.Helper()
		if _, err := led.RecordUsage(context.Background(), ledger.Usage{
			GatewayKeyID: keyID, Provider: providerID, Model: model,
			PromptTokens: prompt, CompletionTokens: completion,
			SplitKnown: true, OK: true, LatencyMS: latency,
		}, prices); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	// Reader's own usage: a = 1000 prompt + 500 completion
	// ($0.01 + $0.015 = $0.025, 120 ms), c = 100 prompt
	// ($0.0005, 90 ms). Another key's identical a-usage must
	// never appear in the reader's report.
	seed(readKey.ID, "p1", "a", 1000, 500, 120)
	seed(readKey.ID, "p2", "c", 100, 0, 90)
	seed("other-key", "p1", "a", 1000, 500, 120)

	book := config.NewPriceBook()
	book.Entries["a"] = config.ModelPrice{InputUSDPer1M: usdPer1M(10), OutputUSDPer1M: usdPer1M(30)}
	book.Entries["c"] = config.ModelPrice{InputUSDPer1M: usdPer1M(5), OutputUSDPer1M: usdPer1M(15)}
	// One routing group, so get_combo_metrics has a combo to
	// aggregate (members p1 + p2 cover all three seeded rows).
	cfg.EnsureRoutingMu()
	cfg.LockRouting()
	cfg.Routing = &config.RoutingConfig{Groups: map[string][]string{"fast": {"p1", "p2"}}}
	cfg.UnlockRouting()

	eng := &strategyFakeEngine{}
	srv := newTestServer(t, eng,
		WithConfig(cfg), WithLedger(led), WithLedgerPrices(prices), WithPriceBook(book),
		WithDashboardProviders(mcpTestProviders{providers: []provider.DashboardProvider{
			{ID: "p1", Name: "Coder", Enabled: true, WireFormat: "openai"},
			{ID: "p2", Name: "Chatter", Enabled: true, WireFormat: "anthropic"},
		}}),
		WithRegistry(mcpTestRegistry{models: []Model{
			{ID: "a", ProviderID: "p1", OwnedBy: "Coder"},
			{ID: "c", ProviderID: "p2", OwnedBy: "Chatter"},
		}}),
		WithRoutingPersist(func(update RoutingUpdate) error {
			cfg.EnsureRoutingMu()
			cfg.LockRouting()
			defer cfg.UnlockRouting()
			if cfg.Routing == nil {
				cfg.Routing = &config.RoutingConfig{}
			}
			if update.Global != nil {
				cfg.Routing.GlobalStrategy = *update.Global
			}
			if cfg.Routing.ChainStrategies == nil {
				cfg.Routing.ChainStrategies = map[string]string{}
			}
			for m, st := range update.Chains {
				cfg.Routing.ChainStrategies[m] = st
			}
			return nil
		}),
	)
	enableMCPTransport(t, srv, adminRaw, "streamable")

	// The source-level availability flag agrees with the wiring.
	src := &serverMCPSource{srv: srv}
	if !src.ModelStatsAvailable() {
		t.Errorf("ModelStatsAvailable() = false with a ledger wired, want true")
	}
	if got := src.ModelStats(); len(got) != 2 {
		t.Errorf("ModelStats() = %d rows, want 2 (models a and c)", len(got))
	}

	wideSess := mcpInitialize(t, srv, wideRaw)
	readSess := mcpInitialize(t, srv, readRaw)
	manageSess := mcpInitialize(t, srv, manageRaw)
	routerSess := mcpInitialize(t, srv, routerRaw)

	// pick_fastest_model ranks on measured latency: c at 90 ms
	// beats a at 120 ms.
	sc := mcpToolResult(t, mcpToolCall(t, srv, wideRaw, wideSess, 2, "pick_fastest_model", `{}`))
	if sc["model"] != "c" || sc["provider"] != "p2" || sc["mean_latency_ms"] != float64(90) {
		t.Errorf("pick_fastest_model = %v, want model c / provider p2 / 90 ms", sc)
	}
	// explain_route / simulate_route dry-run from the live feed.
	sc = mcpToolResult(t, mcpToolCall(t, srv, wideRaw, wideSess, 3, "explain_route", `{"model":"a"}`))
	route, _ := sc["route"].(map[string]any)
	if route["strategy"] != "priority" {
		t.Errorf("explain_route strategy = %v, want priority", route["strategy"])
	}
	attempts, _ := route["attempts"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("explain_route attempts = %v, want exactly 1 (p1 offers a)", attempts)
	}
	att, _ := attempts[0].(map[string]any)
	if att["provider"] != "p1" || att["price_known"] != true {
		t.Errorf("explain_route attempt = %v, want provider p1 / price_known true", att)
	}
	sc = mcpToolResult(t, mcpToolCall(t, srv, wideRaw, wideSess, 4, "simulate_route", `{"model":"a","prompt_tokens":1000}`))
	sim, _ := sc["simulation"].(map[string]any)
	if sim["cost_known"] != true {
		t.Errorf("simulate_route cost_known = %v, want true", sim["cost_known"])
	}
	if est, _ := sim["estimated_prompt_cost_usd"].(float64); math.Abs(est-0.01) > 1e-9 {
		t.Errorf("simulate_route estimated cost = %v, want 0.01 (1000 tokens at $10/1M)", est)
	}
	// get_combo_metrics aggregates the ledger over the combo.
	sc = mcpToolResult(t, mcpToolCall(t, srv, wideRaw, wideSess, 5, "get_combo_metrics", `{"name":"fast"}`))
	metrics, _ := sc["metrics"].(map[string]any)
	if metrics["requests"] != float64(3) {
		t.Errorf("combo metrics requests = %v, want 3 (all seeded rows are combo members)", metrics["requests"])
	}
	if metrics["latency_known"] != true || metrics["mean_latency_ms"] != float64(110) {
		t.Errorf("combo metrics latency = %v / %v, want known / 110 ms mean", metrics["latency_known"], metrics["mean_latency_ms"])
	}
	// cost_report with no key_id covers the caller's own usage
	// only: 2 requests, $0.0255 — never the other key's rows.
	sc = mcpToolResult(t, mcpToolCall(t, srv, readRaw, readSess, 6, "cost_report", `{}`))
	rep, _ := sc["cost_report"].(map[string]any)
	if rep["key_id"] != readKey.ID {
		t.Errorf("cost_report key_id = %v, want the caller's own key %q", rep["key_id"], readKey.ID)
	}
	if rep["requests"] != float64(2) {
		t.Errorf("cost_report requests = %v, want 2 (own usage only)", rep["requests"])
	}
	if cost, _ := rep["cost_usd"].(float64); math.Abs(cost-0.0255) > 1e-9 {
		t.Errorf("cost_report cost_usd = %v, want 0.0255", cost)
	}
	if rep["cost_known"] != true {
		t.Errorf("cost_report cost_known = %v, want true (both models priced)", rep["cost_known"])
	}
	// check_quota for the caller's own key: no budget row yet.
	sc = mcpToolResult(t, mcpToolCall(t, srv, readRaw, readSess, 7, "check_quota", fmt.Sprintf(`{"key_id":%q}`, readKey.ID)))
	q, _ := sc["quota"].(map[string]any)
	if q["has_budget"] != false {
		t.Errorf("check_quota has_budget = %v, want false (no budget set)", q["has_budget"])
	}
	// A cross-key read over HTTP is rejected, not served.
	if code, msg := mcpToolError(t, mcpToolCall(t, srv, readRaw, readSess, 8, "check_quota", `{"key_id":"other-key"}`)); code != -32602 || !strings.Contains(msg, "not the authenticated caller") {
		t.Errorf("cross-key check_quota code/msg = %v / %q, want -32602 naming the caller mismatch", code, msg)
	}
	// set_budget_guard writes a real ledger budget for the
	// caller's own key.
	sc = mcpToolResult(t, mcpToolCall(t, srv, manageRaw, manageSess, 9, "set_budget_guard", fmt.Sprintf(`{"key_id":%q,"limit_usd":10}`, manageKey.ID)))
	if sc["guard_set"] != true {
		t.Errorf("set_budget_guard guard_set = %v, want true", sc["guard_set"])
	}
	stored, err := led.GetKeyBudget(context.Background(), manageKey.ID)
	if err != nil {
		t.Fatalf("GetKeyBudget for the guarded key: %v", err)
	}
	if stored.LimitUSD != 10 {
		t.Errorf("stored budget limit = %v, want 10", stored.LimitUSD)
	}
	// A cross-key guard over HTTP is rejected and stores nothing.
	if code, msg := mcpToolError(t, mcpToolCall(t, srv, manageRaw, manageSess, 10, "set_budget_guard", `{"key_id":"other-key","limit_usd":10}`)); code != -32602 || !strings.Contains(msg, "not the authenticated caller") {
		t.Errorf("cross-key set_budget_guard code/msg = %v / %q, want -32602 naming the caller mismatch", code, msg)
	}
	if _, err := led.GetKeyBudget(context.Background(), "other-key"); err == nil {
		t.Errorf("rejected cross-key guard still stored a budget for other-key")
	}
	// set_routing_strategy persists (read-back shows it) and
	// applies to the engine.
	sc = mcpToolResult(t, mcpToolCall(t, srv, routerRaw, routerSess, 11, "set_routing_strategy", `{"strategy":"cheapest"}`))
	strat, _ := sc["strategy"].(map[string]any)
	if strat["name"] != "cheapest" {
		t.Errorf("set_routing_strategy current = %v, want cheapest", strat["name"])
	}
	if eng.global != "cheapest" {
		t.Errorf("engine global strategy = %q, want cheapest (runtime apply)", eng.global)
	}
	// create_combo now succeeds over HTTP through the same
	// ComboStore.Create path POST /v1/combos runs, and the
	// created combo is visible via list_combos and via the
	// normal REST list/get paths.
	sc = mcpToolResult(t, mcpToolCall(t, srv, routerRaw, routerSess, 12, "create_combo", `{"name":"new","members":["p1"]}`))
	if sc["created"] != true {
		t.Errorf("create_combo created = %v, want true (result: %v)", sc["created"], sc)
	}
	created, _ := sc["combo"].(map[string]any)
	if created["name"] != "new" || created["strategy"] != "priority" {
		t.Errorf("create_combo combo = %v, want name new / strategy priority", created)
	}
	createdMembers, _ := created["members"].([]any)
	if len(createdMembers) != 1 || createdMembers[0] != "p1" {
		t.Errorf("create_combo members = %v, want [p1]", createdMembers)
	}
	// Visible via list_combos.
	sc = mcpToolResult(t, mcpToolCall(t, srv, routerRaw, routerSess, 13, "list_combos", `{}`))
	listed, _ := sc["combos"].([]any)
	found := false
	for _, c := range listed {
		if cm, _ := c.(map[string]any); cm["name"] == "new" {
			found = true
		}
	}
	if !found {
		t.Errorf("list_combos after create_combo = %v, want it to contain new", listed)
	}
	// Visible via the normal REST list/get paths (the admin key
	// implies every scope).
	rec := doReq(t, srv, http.MethodGet, "/v1/combos", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/combos after MCP create: status %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"new"`) {
		t.Errorf("GET /v1/combos body = %s, want it to list new", rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/v1/combos/new", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /v1/combos/new after MCP create: status %d, body %s", rec.Code, rec.Body.String())
	}
	// Shared behaviour: the same bad input fails with the
	// identical validation text on the MCP path and the REST
	// path — one implementation, two transports.
	wantMsg := `chain[0].provider: unknown provider "ghost"`
	if code, msg := mcpToolError(t, mcpToolCall(t, srv, routerRaw, routerSess, 14, "create_combo", `{"name":"bad","members":["ghost"]}`)); code != -32602 || !strings.Contains(msg, wantMsg) {
		t.Errorf("bad create_combo code/msg = %v / %q, want -32602 containing %q", code, msg, wantMsg)
	}
	rec = doReq(t, srv, http.MethodPost, "/v1/combos", `{"name":"bad","chain":["ghost"],"strategy":"priority"}`, adminRaw)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad POST /v1/combos status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	} else {
		// The REST body is JSON, so the quotes in the message
		// arrive backslash-escaped: decode and compare the
		// message field exactly, not the raw body.
		var restErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &restErr); err != nil {
			t.Fatalf("decode bad POST /v1/combos error: %v (body %s)", err, rec.Body.String())
		}
		if restErr.Error.Message != wantMsg {
			t.Errorf("bad POST /v1/combos message = %q, want %q (the MCP path's text)", restErr.Error.Message, wantMsg)
		}
	}
}

// TestMCPHTTPPickFastestModelBackendUnavailable proves the N4
// message over the real HTTP transport: a serve process with no
// ledger wired answers pick_fastest_model with "backend is not
// available", never "no measured model latency yet".
func TestMCPHTTPPickFastestModelBackendUnavailable(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	// No ledger on this server: the source reports unavailable.
	if src := (&serverMCPSource{srv: srv}); src.ModelStatsAvailable() {
		t.Errorf("ModelStatsAvailable() = true with no ledger wired, want false")
	}
	if got := (&serverMCPSource{srv: srv}).ModelStats(); len(got) != 0 {
		t.Errorf("ModelStats() = %v with no ledger, want empty", got)
	}
	enableMCPTransport(t, srv, keys.admin, "streamable")
	sess := mcpInitialize(t, srv, keys.wide)
	if code, msg := mcpToolError(t, mcpToolCall(t, srv, keys.wide, sess, 2, "pick_fastest_model", `{}`)); code != -32602 || !strings.Contains(msg, "backend is not available") {
		t.Errorf("pick_fastest_model code/msg = %v / %q, want -32602 saying the backend is not available", code, msg)
	}
}

// ---------------------------------------------------------------------------
// M9.10 gate P1-4 + P1-5: first SSE success-path tool calls, and
// Streamable calls for the 9 tools never called over any HTTP
// transport (best_combo_for_task, cache_stats, db_stats,
// heartbeat, memory_add, memory_clear, memory_search,
// switch_combos, tool_search).
// ---------------------------------------------------------------------------

// m910Keys is the key set one M9.10 test server is built with.
// Each key carries exactly the scopes its name promises, except
// routerDual, which deliberately holds both routing scopes so
// one session can both switch_combos and list_combos (the P1-5
// same-session clear read); the ID fields are the gateway-key
// ids, needed for the tenant-isolation assertions (memory_* /
// cache_flush-adjacent checks must name the caller key id,
// never "anonymous").
type m910Keys struct {
	admin, read, empty, wide, routerRead, routerManage, routerDual, costRead, ops, memory, memory2, memoryDestr, cache string
	routerReadID, costReadID, memoryID, memory2ID, memoryDestrID, cacheID                                              string
	count                                                                                                              int
}

// newM910MCPHTTPTestServer builds a server with a live ledger in
// the config data dir (so db_stats reports a real ledger.db with
// real row counts, and check_quota / best_combo_for_task read
// measured history), one routing combo "fast" over p1+p2, and a
// key per scope class. The ledger holds 2 seeded usage rows for
// the router-read key: model a on p1 (1000 prompt + 500
// completion, 120 ms) and model c on p2 (100 prompt, 90 ms).
func newM910MCPHTTPTestServer(t *testing.T) (*Server, *m910Keys) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	keys := &m910Keys{}
	issue := func(name string, scopes []string) (raw, id string) {
		k, raw := issueScopedKey(t, cfg, name, scopes)
		keys.count++
		return raw, k.ID
	}
	keys.admin, _ = issue("admin", []string{"chat", "admin"})
	keys.read, _ = issue("reader", []string{"mcp:read:status"})
	keys.empty, _ = issue("empty", []string{})
	keys.wide, _ = issue("wide", []string{"mcp:read:*"})
	keys.routerRead, keys.routerReadID = issue("router-reader", []string{"mcp:read:routing"})
	keys.routerManage, _ = issue("router-manager", []string{"mcp:manage:routing"})
	// Dual-scope routing key: holds both mcp:manage:routing and
	// mcp:read:routing, so one session can switch_combos and
	// then list_combos — the P1-5 same-session read after a
	// clear. No other key holds both.
	keys.routerDual, _ = issue("router-dual", []string{"mcp:manage:routing", "mcp:read:routing"})
	keys.costRead, keys.costReadID = issue("cost-reader", []string{"mcp:read:cost"})
	keys.ops, _ = issue("ops", []string{"mcp:manage:ops"})
	keys.memory, keys.memoryID = issue("memory", []string{"mcp:manage:memory"})
	keys.memory2, keys.memory2ID = issue("memory-2", []string{"mcp:manage:memory"})
	keys.memoryDestr, keys.memoryDestrID = issue("memory-destr", []string{"mcp:manage:memory", "mcp:destructive"})
	keys.cache, keys.cacheID = issue("cache", []string{"mcp:manage:cache", "mcp:destructive"})
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Name: "Coder", BaseURL: "https://p1.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "openai", Models: []provider.Model{{ID: "a"}, {ID: "b"}}, Enabled: true},
		{ID: "p2", Name: "Chatter", BaseURL: "https://p2.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "anthropic", Models: []provider.Model{{ID: "c"}}, Enabled: true},
	}
	cfg.EnsureRoutingMu()
	cfg.LockRouting()
	cfg.Routing = &config.RoutingConfig{Groups: map[string][]string{"fast": {"p1", "p2"}}}
	cfg.UnlockRouting()

	led, err := ledger.Open(filepath.Join(cfg.DataDir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	prices := ledger.Prices{
		ledger.PriceKey{Model: "a"}: {PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6},
		ledger.PriceKey{Model: "c"}: {PromptPerToken: 5.0 / 1e6, CompletionPerToken: 15.0 / 1e6},
	}
	seed := func(keyID, providerID, model string, prompt, completion int, latency int64) {
		t.Helper()
		if _, err := led.RecordUsage(context.Background(), ledger.Usage{
			GatewayKeyID: keyID, Provider: providerID, Model: model,
			PromptTokens: prompt, CompletionTokens: completion,
			SplitKnown: true, OK: true, LatencyMS: latency,
		}, prices); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	seed(keys.routerReadID, "p1", "a", 1000, 500, 120)
	seed(keys.routerReadID, "p2", "c", 100, 0, 90)

	book := config.NewPriceBook()
	book.Entries["a"] = config.ModelPrice{InputUSDPer1M: usdPer1M(10), OutputUSDPer1M: usdPer1M(30)}
	book.Entries["c"] = config.ModelPrice{InputUSDPer1M: usdPer1M(5), OutputUSDPer1M: usdPer1M(15)}

	srv := newTestServer(t, &strategyFakeEngine{},
		WithConfig(cfg), WithLedger(led), WithLedgerPrices(prices), WithPriceBook(book),
		WithDashboardProviders(mcpTestProviders{providers: []provider.DashboardProvider{
			{ID: "p1", Name: "Coder", Enabled: true, WireFormat: "openai"},
			{ID: "p2", Name: "Chatter", Enabled: true, WireFormat: "anthropic"},
		}}),
		WithRegistry(mcpTestRegistry{models: []Model{
			{ID: "a", ProviderID: "p1", OwnedBy: "Coder"},
			{ID: "b", ProviderID: "p1", OwnedBy: "Coder"},
			{ID: "c", ProviderID: "p2", OwnedBy: "Chatter"},
		}}),
	)
	return srv, keys
}

// m910OpenSSE opens one live SSE stream (GET /api/mcp/sse) with
// the given key and returns the session id plus the event pump.
// The caller must call the returned close func (it closes the
// GET body, which terminates the session).
func m910OpenSSE(t *testing.T, ts *httptest.Server, key string) (sessionID string, events chan sseEvent, errs chan error, close func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	client := ts.Client()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/mcp/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/mcp/sse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("GET /api/mcp/sse: status %d, want 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("GET /api/mcp/sse Content-Type = %q, want text/event-stream", ct)
	}
	events = make(chan sseEvent, 8)
	errs = make(chan error, 1)
	go streamEvents(t, bufio.NewReader(resp.Body), events, errs)
	ev := readSSEEventTimeout(t, events, errs, 5*time.Second)
	if ev.event != "endpoint" {
		resp.Body.Close()
		t.Fatalf("first SSE event = %q, want endpoint", ev.event)
	}
	sessionID = strings.TrimPrefix(ev.data, "/api/mcp/sse?session_id=")
	if sessionID == ev.data || sessionID == "" {
		resp.Body.Close()
		t.Fatalf("endpoint data = %q, want /api/mcp/sse?session_id=<id>", ev.data)
	}
	return sessionID, events, errs, func() {
		resp.Body.Close()
		cancel()
	}
}

// m910SSEPost sends one JSON-RPC message over an open SSE
// session (POST /api/mcp/sse?session_id=) and returns the
// response envelope read off the stream. The POST itself must
// answer 202; the result arrives as a "message" event.
func m910SSEPost(t *testing.T, ts *httptest.Server, key, sessionID string, events <-chan sseEvent, errs <-chan error, body string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/mcp/sse?session_id="+sessionID, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/mcp/sse: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/mcp/sse: status %d, want 202: %s", resp.StatusCode, b)
	}
	ev := readSSEEventTimeout(t, events, errs, 5*time.Second)
	if ev.event != "message" {
		t.Fatalf("SSE response event = %q, want message", ev.event)
	}
	env := map[string]any{}
	if err := json.Unmarshal([]byte(ev.data), &env); err != nil {
		t.Fatalf("SSE response is not JSON: %v (data %q)", err, ev.data)
	}
	return env
}

// m910SSEInitialize runs the initialize handshake over an open
// SSE session and checks the protocol version on the stream.
func m910SSEInitialize(t *testing.T, ts *httptest.Server, key, sessionID string, events <-chan sseEvent, errs <-chan error) {
	t.Helper()
	env := m910SSEPost(t, ts, key, sessionID, events, errs, rpcBody(1, "initialize", `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`))
	result, _ := env["result"].(map[string]any)
	if result["protocolVersion"] != mcp.ProtocolVersion {
		t.Fatalf("SSE initialize protocolVersion = %v, want %q", result["protocolVersion"], mcp.ProtocolVersion)
	}
}

// m910SSEToolCall calls one tool over an open SSE session and
// returns the response envelope from the stream.
func m910SSEToolCall(t *testing.T, ts *httptest.Server, key, sessionID string, events <-chan sseEvent, errs <-chan error, id int, name, args string) map[string]any {
	t.Helper()
	if args == "" {
		args = "{}"
	}
	params := fmt.Sprintf(`{"name":%q,"arguments":%s}`, name, args)
	return m910SSEPost(t, ts, key, sessionID, events, errs, rpcBody(id, "tools/call", params))
}

// TestMCPHTTPSSESuccessPathM910 is the P1-4 first SSE
// success-path coverage: one successful tools/call per tool
// family over SSE — router_status (built-in), best_combo_for_task
// (routing), tool_search (search), check_quota (cost), db_stats
// (ops) — each a full round trip (GET opens the stream, POST
// sends the call, the success result arrives on the stream).
// Before this test the only SSE tool call anywhere was a -32002
// denial probe of model_list in TestMCPHTTPSSEFlow.
func TestMCPHTTPSSESuccessPathM910(t *testing.T) {
	srv, keys := newM910MCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "sse")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// The mcp:read:* wildcard key covers the read families
	// (status, routing, cost) on one stream.
	wideSession, wideEvents, wideErrs, closeWide := m910OpenSSE(t, ts, keys.wide)
	defer closeWide()
	m910SSEInitialize(t, ts, keys.wide, wideSession, wideEvents, wideErrs)

	// Built-in family: router_status reports the live key count.
	env := m910SSEToolCall(t, ts, keys.wide, wideSession, wideEvents, wideErrs, 2, "router_status", "")
	if code := rpcErrorCode(env); code != -1 {
		t.Fatalf("router_status over SSE: error code %v: %v", code, env)
	}
	status := mcpToolResult(t, env)
	if status["gateway_keys"] != float64(keys.count) {
		t.Errorf("SSE router_status gateway_keys = %v, want %d (live count)", status["gateway_keys"], keys.count)
	}

	// Routing family: best_combo_for_task ranks on the seeded
	// measured history — "fast chat" prefers fastest, combo
	// "fast" is the only candidate, metrics known.
	env = m910SSEToolCall(t, ts, keys.wide, wideSession, wideEvents, wideErrs, 3, "best_combo_for_task", `{"task":"fast chat"}`)
	if code := rpcErrorCode(env); code != -1 {
		t.Fatalf("best_combo_for_task over SSE: error code %v: %v", code, env)
	}
	sc := mcpToolResult(t, env)
	combo, _ := sc["combo"].(map[string]any)
	if combo["name"] != "fast" {
		t.Errorf("SSE best_combo_for_task combo = %v, want fast", combo["name"])
	}
	if sc["preferred_strategy"] != "fastest" {
		t.Errorf("SSE best_combo_for_task preferred_strategy = %v, want fastest", sc["preferred_strategy"])
	}
	if sc["metrics_known"] != true {
		t.Errorf("SSE best_combo_for_task metrics_known = %v, want true (seeded ledger history)", sc["metrics_known"])
	}

	// Search family: tool_search over SSE — the mcp:read:*
	// wildcard covers mcp:read:status, and "cache" matches
	// exactly the three tools whose name or description
	// mentions the cache (same match as the Streamable
	// tool_search check in TestMCPHTTPM910NeverCalledTools).
	env = m910SSEToolCall(t, ts, keys.wide, wideSession, wideEvents, wideErrs, 4, "tool_search", `{"query":"cache"}`)
	if code := rpcErrorCode(env); code != -1 {
		t.Fatalf("tool_search over SSE: error code %v: %v", code, env)
	}
	sc = mcpToolResult(t, env)
	if sc["count"] != float64(3) {
		t.Errorf("SSE tool_search cache count = %v, want 3 (cache_stats, cache_flush, db_health)", sc["count"])
	}
	hits, _ := sc["tools"].([]any)
	names := map[string]bool{}
	for _, h := range hits {
		if hm, _ := h.(map[string]any); hm["name"] != nil {
			names[hm["name"].(string)] = true
		}
	}
	for _, want := range []string{"cache_stats", "cache_flush", "db_health"} {
		if !names[want] {
			t.Errorf("SSE tool_search cache missing %q (got %v)", want, names)
		}
	}

	// Cost family: check_quota for the caller's own key — no
	// budget row yet, so has_budget is false. The quota/budget
	// tools force key_id to the authenticated caller, so this
	// call runs on the cost-reader key's own stream; passing
	// another key's id is honestly rejected -32602 (proven by
	// the cross-key call below, not just claimed).
	costSession, costEvents, costErrs, closeCost := m910OpenSSE(t, ts, keys.costRead)
	defer closeCost()
	m910SSEInitialize(t, ts, keys.costRead, costSession, costEvents, costErrs)
	env = m910SSEToolCall(t, ts, keys.costRead, costSession, costEvents, costErrs, 2, "check_quota", fmt.Sprintf(`{"key_id":%q}`, keys.costReadID))
	if code := rpcErrorCode(env); code != -1 {
		t.Fatalf("check_quota over SSE: error code %v: %v", code, env)
	}
	sc = mcpToolResult(t, env)
	quota, _ := sc["quota"].(map[string]any)
	if quota["key_id"] != keys.costReadID {
		t.Errorf("SSE check_quota key_id = %v, want %q", quota["key_id"], keys.costReadID)
	}
	if quota["has_budget"] != false {
		t.Errorf("SSE check_quota has_budget = %v, want false (no budget set)", quota["has_budget"])
	}

	// Cross-key check_quota: naming another key's id is
	// rejected -32602, naming the caller mismatch — a gateway
	// key may only check its own quota (same rejection as the
	// Streamable cross-key check in TestMCPHTTPM93LedgerBackend).
	env = m910SSEToolCall(t, ts, keys.costRead, costSession, costEvents, costErrs, 3, "check_quota", fmt.Sprintf(`{"key_id":%q}`, keys.routerReadID))
	if code, msg := mcpToolError(t, env); code != -32602 || !strings.Contains(msg, "not the authenticated caller") {
		t.Errorf("SSE cross-key check_quota code/msg = %v / %q, want -32602 naming the caller mismatch", code, msg)
	}

	// Ops family: db_stats on its own stream, with the ops key
	// (mcp:manage:ops). The ledger lives in the data dir, so
	// the report is live: ledger.db exists, with the 2 seeded
	// usage_events rows.
	opsSession, opsEvents, opsErrs, closeOps := m910OpenSSE(t, ts, keys.ops)
	defer closeOps()
	m910SSEInitialize(t, ts, keys.ops, opsSession, opsEvents, opsErrs)
	env = m910SSEToolCall(t, ts, keys.ops, opsSession, opsEvents, opsErrs, 2, "db_stats", "")
	if code := rpcErrorCode(env); code != -1 {
		t.Fatalf("db_stats over SSE: error code %v: %v", code, env)
	}
	sc = mcpToolResult(t, env)
	files, _ := sc["files"].([]any)
	var ledgerFile map[string]any
	for _, f := range files {
		if fm, _ := f.(map[string]any); fm["name"] == "ledger.db" {
			ledgerFile = fm
		}
	}
	if ledgerFile == nil {
		t.Fatalf("SSE db_stats files = %v, want a ledger.db entry", files)
	}
	if ledgerFile["exists"] != true {
		t.Errorf("SSE db_stats ledger.db exists = %v, want true (live ledger in the data dir)", ledgerFile["exists"])
	}
	if size, _ := ledgerFile["size_bytes"].(float64); size <= 0 {
		t.Errorf("SSE db_stats ledger.db size_bytes = %v, want > 0", ledgerFile["size_bytes"])
	}
	rows, _ := ledgerFile["rows"].(map[string]any)
	if rows["usage_events"] != float64(2) {
		t.Errorf("SSE db_stats ledger.db usage_events = %v, want 2 (seeded rows)", rows["usage_events"])
	}
}

// TestMCPHTTPM910NeverCalledTools is the P1-5 coverage for the 9
// tools never called over any HTTP transport before it, each over
// POST /api/mcp/stream with success + scope-denial calls:
// best_combo_for_task, cache_stats, db_stats, heartbeat,
// memory_add, memory_clear, memory_search, switch_combos,
// tool_search. memory_* assertions are tenant-scoped: a second
// memory key must not see the first key's entries, and
// memory_clear reports (and clears) only the caller's own key id.
func TestMCPHTTPM910NeverCalledTools(t *testing.T) {
	srv, keys := newM910MCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")

	routerReadSess := mcpInitialize(t, srv, keys.routerRead)
	routerManageSess := mcpInitialize(t, srv, keys.routerManage)
	routerDualSess := mcpInitialize(t, srv, keys.routerDual)
	opsSess := mcpInitialize(t, srv, keys.ops)
	memSess := mcpInitialize(t, srv, keys.memory)
	mem2Sess := mcpInitialize(t, srv, keys.memory2)
	memDestrSess := mcpInitialize(t, srv, keys.memoryDestr)
	cacheSess := mcpInitialize(t, srv, keys.cache)
	readSess := mcpInitialize(t, srv, keys.read)
	emptySess := mcpInitialize(t, srv, keys.empty)

	// best_combo_for_task: success on measured history, and
	// scope denial for a key without mcp:read:routing.
	sc := mcpToolResult(t, mcpToolCall(t, srv, keys.routerRead, routerReadSess, 2, "best_combo_for_task", `{"task":"fast chat"}`))
	combo, _ := sc["combo"].(map[string]any)
	if combo["name"] != "fast" {
		t.Errorf("best_combo_for_task combo = %v, want fast", combo["name"])
	}
	if sc["preferred_strategy"] != "fastest" {
		t.Errorf("best_combo_for_task preferred_strategy = %v, want fastest", sc["preferred_strategy"])
	}
	if sc["metrics_known"] != true {
		t.Errorf("best_combo_for_task metrics_known = %v, want true (seeded ledger history)", sc["metrics_known"])
	}
	if sc["candidates_seen"] != float64(1) {
		t.Errorf("best_combo_for_task candidates_seen = %v, want 1", sc["candidates_seen"])
	}
	// A cost cap the measured combo cannot satisfy excludes it;
	// with one combo left out, no candidate remains — honest
	// -32602, not a guess. (The not-applied reporting for
	// unknown-cost combos is covered at the stdio layer by
	// TestM93BestComboForTask.)
	if code, _ := mcpToolError(t, mcpToolCall(t, srv, keys.routerRead, routerReadSess, 3, "best_combo_for_task", `{"task":"fast chat","max_cost_usd":0.0000001}`)); code != -32602 {
		t.Errorf("best_combo_for_task under an impossible cost cap: code %v, want -32602", code)
	}
	if code, _ := mcpToolError(t, mcpToolCall(t, srv, keys.read, readSess, 4, "best_combo_for_task", `{"task":"fast chat"}`)); code != mcp.ScopeDeniedCode {
		t.Errorf("best_combo_for_task with read-only key: code %v, want -32002", code)
	}

	// switch_combos: activate "fast", prove list_combos reports
	// it active, unknown name fails, and a read-only routing
	// key is denied (mcp:manage:routing required).
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.routerManage, routerManageSess, 5, "switch_combos", `{"name":"fast"}`))
	if sc["active_combo"] != "fast" {
		t.Errorf("switch_combos active_combo = %v, want fast", sc["active_combo"])
	}
	// list_combos needs mcp:read:routing. The active combo is
	// per-session state (H1), so the read key's session does
	// NOT inherit the switch the manage key made on its own
	// session: fast must show inactive here. Same-session
	// visibility (switch, then list on that session) is
	// covered by TestMCPHTTPActiveComboIsPerSession.
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.routerRead, routerReadSess, 6, "list_combos", `{}`))
	listed, _ := sc["combos"].([]any)
	activeSeen := false
	for _, c := range listed {
		if cm, _ := c.(map[string]any); cm["name"] == "fast" && cm["active"] == true {
			activeSeen = true
		}
	}
	if activeSeen {
		t.Errorf("list_combos after switch_combos = %v, want fast NOT marked active on another session (per-session active combo, H1)", listed)
	}
	if code, msg := mcpToolError(t, mcpToolCall(t, srv, keys.routerManage, routerManageSess, 7, "switch_combos", `{"name":"nope"}`)); code != -32602 || !strings.Contains(msg, "unknown combo") {
		t.Errorf("switch_combos unknown combo code/msg = %v / %q, want -32602 naming the unknown combo", code, msg)
	}
	if code, _ := mcpToolError(t, mcpToolCall(t, srv, keys.routerRead, routerReadSess, 8, "switch_combos", `{"name":"fast"}`)); code != mcp.ScopeDeniedCode {
		t.Errorf("switch_combos with read-routing key: code %v, want -32002", code)
	}
	// Clear the active selection on the same session
	// (active=false): the clear reply itself reports
	// active_combo nil — the H1 per-session clear, mirrored
	// here for the P1-5 fixture (before this the only
	// {"active":false} call in the file was the H1 test's).
	// list_combos cannot read this session's state: it needs
	// mcp:read:routing, which the manage key does not hold,
	// so the clear reply is the read of this session. The
	// dual-scope key makes the same-session read possible on
	// its own session, right below (a session is bound to
	// exactly one gateway key, so the dual key cannot read
	// the manage session — it replays the clear on its own).
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.routerManage, routerManageSess, 29, "switch_combos", `{"name":"fast","active":false}`))
	if sc["active_combo"] != nil {
		t.Errorf("switch_combos clear active_combo = %v, want nil", sc["active_combo"])
	}

	// Same-session read after a clear, via the dual-scope key:
	// activate fast, clear it, then list_combos on that same
	// session must report no active combo. (The manage
	// session's own clear above cannot be list-read: its key
	// lacks mcp:read:routing and sessions are key-bound.)
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.routerDual, routerDualSess, 30, "switch_combos", `{"name":"fast"}`))
	if sc["active_combo"] != "fast" {
		t.Errorf("switch_combos (dual key) active_combo = %v, want fast", sc["active_combo"])
	}
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.routerDual, routerDualSess, 31, "switch_combos", `{"name":"fast","active":false}`))
	if sc["active_combo"] != nil {
		t.Errorf("switch_combos clear (dual key) active_combo = %v, want nil", sc["active_combo"])
	}
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.routerDual, routerDualSess, 32, "list_combos", `{}`))
	if sc["active_combo"] != nil {
		t.Errorf("list_combos after clear (dual key) active_combo = %v, want nil", sc["active_combo"])
	}

	// cache_stats: no cache is wired on this server, so every
	// layer honestly reports present=false, never zero-filled.
	// Denial for a key without mcp:manage:cache.
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.cache, cacheSess, 9, "cache_stats", `{}`))
	for _, layer := range []string{"exact", "semantic", "persistent"} {
		l, _ := sc[layer].(map[string]any)
		if l == nil {
			t.Errorf("cache_stats missing %q layer: %v", layer, sc)
			continue
		}
		if l["present"] != false {
			t.Errorf("cache_stats %s present = %v, want false (no cache wired on this server)", layer, l["present"])
		}
	}
	if code, _ := mcpToolError(t, mcpToolCall(t, srv, keys.read, readSess, 10, "cache_stats", `{}`)); code != mcp.ScopeDeniedCode {
		t.Errorf("cache_stats with read-only key: code %v, want -32002", code)
	}

	// db_stats: live ledger.db in the data dir, 2 seeded rows.
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.ops, opsSess, 11, "db_stats", `{}`))
	files, _ := sc["files"].([]any)
	var ledgerFile map[string]any
	for _, f := range files {
		if fm, _ := f.(map[string]any); fm["name"] == "ledger.db" {
			ledgerFile = fm
		}
	}
	if ledgerFile == nil {
		t.Fatalf("db_stats files = %v, want a ledger.db entry", files)
	}
	if ledgerFile["exists"] != true {
		t.Errorf("db_stats ledger.db exists = %v, want true", ledgerFile["exists"])
	}
	rows, _ := ledgerFile["rows"].(map[string]any)
	if rows["usage_events"] != float64(2) {
		t.Errorf("db_stats ledger.db usage_events = %v, want 2 (seeded rows)", rows["usage_events"])
	}
	if code, _ := mcpToolError(t, mcpToolCall(t, srv, keys.read, readSess, 12, "db_stats", `{}`)); code != mcp.ScopeDeniedCode {
		t.Errorf("db_stats with read-only key: code %v, want -32002", code)
	}

	// heartbeat: live snapshot — protocol version, all 24 tools
	// announced, audit log enabled (the HTTP transport opens
	// one at <data-dir>/mcp/audit.jsonl whenever a DataDir is
	// configured — unlike the stdio test fake, which wires
	// none). Denial for a key with no scopes at all.
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.read, readSess, 13, "heartbeat", `{}`))
	if sc["protocol_version"] != mcp.ProtocolVersion {
		t.Errorf("heartbeat protocol_version = %v, want %q", sc["protocol_version"], mcp.ProtocolVersion)
	}
	if sc["tool_count"] != float64(len(mcp.Tools())) {
		t.Errorf("heartbeat tool_count = %v, want %d (len(Tools()))", sc["tool_count"], len(mcp.Tools()))
	}
	if uptime, _ := sc["uptime_seconds"].(float64); uptime < 0 {
		t.Errorf("heartbeat uptime_seconds = %v, want >= 0 (live server uptime)", sc["uptime_seconds"])
	}
	if sc["audit_log"] != "enabled" {
		t.Errorf("heartbeat audit_log = %v, want enabled (HTTP transport opens <data-dir>/mcp/audit.jsonl)", sc["audit_log"])
	}
	if code, _ := mcpToolError(t, mcpToolCall(t, srv, keys.empty, emptySess, 14, "heartbeat", `{}`)); code != mcp.ScopeDeniedCode {
		t.Errorf("heartbeat with empty-scope key: code %v, want -32002", code)
	}

	// tool_search: substring match, empty query returns the full
	// catalog, non-string query is invalid params, and a key
	// with no scopes is denied (mcp:read:status required).
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.read, readSess, 15, "tool_search", `{"query":"cache"}`))
	// "cache" matches cache_stats, cache_flush, and db_health
	// (whose description mentions the persistent cache).
	if sc["count"] != float64(3) {
		t.Errorf("tool_search cache count = %v, want 3 (cache_stats, cache_flush, db_health)", sc["count"])
	}
	hits, _ := sc["tools"].([]any)
	names := map[string]bool{}
	for _, h := range hits {
		if hm, _ := h.(map[string]any); hm["name"] != nil {
			names[hm["name"].(string)] = true
		}
	}
	for _, want := range []string{"cache_stats", "cache_flush", "db_health"} {
		if !names[want] {
			t.Errorf("tool_search cache missing %q (got %v)", want, names)
		}
	}
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.read, readSess, 16, "tool_search", `{}`))
	if sc["count"] != float64(len(mcp.Tools())) {
		t.Errorf("tool_search empty query count = %v, want %d (full catalog)", sc["count"], len(mcp.Tools()))
	}
	if code, msg := mcpToolError(t, mcpToolCall(t, srv, keys.read, readSess, 17, "tool_search", `{"query":42}`)); code != -32602 || !strings.Contains(msg, "query must be a string") {
		t.Errorf("tool_search non-string query code/msg = %v / %q, want -32602 naming the query type", code, msg)
	}
	if code, _ := mcpToolError(t, mcpToolCall(t, srv, keys.empty, emptySess, 18, "tool_search", `{"query":"cache"}`)); code != mcp.ScopeDeniedCode {
		t.Errorf("tool_search with empty-scope key: code %v, want -32002", code)
	}

	// memory_*: tenant isolation end to end. The memory key adds
	// a note; a DIFFERENT memory key searching the same text
	// finds nothing (caller-namespaced store, not anonymous).
	// The owner finds it; a memory key without the destructive
	// scope cannot clear; the destructive memory key adds and
	// clears only its own note, reporting its own key id — and
	// the first key's note survives that clear untouched.
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.memory, memSess, 19, "memory_add", `{"text":"prefers dark mode"}`))
	if sc["id"] == nil || sc["id"] == "" {
		t.Errorf("memory_add returned no id: %v", sc)
	}
	if sc["text"] != "prefers dark mode" {
		t.Errorf("memory_add text = %v, want the stored note back", sc["text"])
	}
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.memory2, mem2Sess, 20, "memory_search", `{"query":"dark mode"}`))
	if sc["count"] != float64(0) {
		t.Errorf("memory_search by a different key count = %v, want 0 (tenant isolation)", sc["count"])
	}
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.memory, memSess, 21, "memory_search", `{"query":"DARK"}`))
	if sc["count"] != float64(1) {
		t.Fatalf("memory_search by the owner count = %v, want 1 (case-insensitive substring)", sc["count"])
	}
	entries, _ := sc["entries"].([]any)
	e, _ := entries[0].(map[string]any)
	if e["text"] != "prefers dark mode" {
		t.Errorf("memory_search entry = %v, want the stored note", e["text"])
	}
	if code, _ := mcpToolError(t, mcpToolCall(t, srv, keys.memory, memSess, 22, "memory_clear", `{}`)); code != mcp.ScopeDeniedCode {
		t.Errorf("memory_clear without mcp:destructive: code %v, want -32002", code)
	}
	if code, _ := mcpToolError(t, mcpToolCall(t, srv, keys.read, readSess, 23, "memory_add", `{"text":"x"}`)); code != mcp.ScopeDeniedCode {
		t.Errorf("memory_add with read-only key: code %v, want -32002", code)
	}
	if code, msg := mcpToolError(t, mcpToolCall(t, srv, keys.memory, memSess, 24, "memory_add", `{"text":"  "}`)); code != -32602 || !strings.Contains(msg, "non-empty") {
		t.Errorf("memory_add empty text code/msg = %v / %q, want -32602 naming the non-empty rule", code, msg)
	}
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.memoryDestr, memDestrSess, 25, "memory_add", `{"text":"destructive key note"}`))
	if sc["text"] != "destructive key note" {
		t.Errorf("memory_add (destructive key) text = %v, want its own note", sc["text"])
	}
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.memoryDestr, memDestrSess, 26, "memory_clear", `{}`))
	if sc["cleared"] != float64(1) {
		t.Errorf("memory_clear cleared = %v, want 1 (only the caller's own note)", sc["cleared"])
	}
	if sc["caller"] != keys.memoryDestrID {
		t.Errorf("memory_clear caller = %v, want the gateway-key id %q (per-request identity, not anonymous)", sc["caller"], keys.memoryDestrID)
	}
	if sc["scope"] != "caller-only" {
		t.Errorf("memory_clear scope = %v, want caller-only", sc["scope"])
	}
	// The first key's note survived the other tenant's clear.
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.memory, memSess, 27, "memory_search", `{"query":"dark mode"}`))
	if sc["count"] != float64(1) {
		t.Errorf("memory_search after another tenant's clear count = %v, want 1 (other tenants untouched)", sc["count"])
	}
	// And the destructive key's own store is now empty.
	sc = mcpToolResult(t, mcpToolCall(t, srv, keys.memoryDestr, memDestrSess, 28, "memory_search", `{"query":"destructive"}`))
	if sc["count"] != float64(0) {
		t.Errorf("memory_search after own clear count = %v, want 0", sc["count"])
	}
	// memory_search scope denial: the read-only key holds
	// mcp:read:status only, not mcp:manage:memory, so the
	// search is denied -32002 (the one denial branch the
	// other never-called tools above all have).
	if code, _ := mcpToolError(t, mcpToolCall(t, srv, keys.read, readSess, 30, "memory_search", `{"query":"dark mode"}`)); code != mcp.ScopeDeniedCode {
		t.Errorf("memory_search with read-only key: code %v, want -32002", code)
	}
}

// ---------------------------------------------------------------------------
// MCP transport hardening: H1 (per-session active combo), H2 (GET
// stream cap), H4 (inspectDBs path leak), H6 (request context
// threading incl. the CreateCombo ctx.Err() guard). See
// internal/server/mcp_http.go and internal/mcp/mcp.go.
// ---------------------------------------------------------------------------

// mcpHardeningTestServer builds a server with two routing groups
// (g1 = {p1}, g2 = {p2}), providers p1/p2 with keys, and one
// routing key per caller (both read+manage routing), for the
// H1/H6 tests below. It returns the server and the two routing
// keys' raw bearer tokens plus the admin token (for the
// transport toggle).
func mcpHardeningTestServer(t *testing.T) (*Server, string, string, string) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	_, routerA := issueScopedKey(t, cfg, "router-a", []string{"mcp:read:routing", "mcp:manage:routing"})
	_, routerB := issueScopedKey(t, cfg, "router-b", []string{"mcp:read:routing", "mcp:manage:routing"})
	_, adminRaw := issueScopedKey(t, cfg, "admin", []string{"chat", "admin"})
	// Combo validation reads providers from cfg, so p1/p2 must
	// exist here with keys for create_combo to validate.
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Name: "Coder", BaseURL: "https://p1.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "openai", Models: []provider.Model{{ID: "a"}}, Enabled: true},
		{ID: "p2", Name: "Chatter", BaseURL: "https://p2.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "anthropic", Models: []provider.Model{{ID: "c"}}, Enabled: true},
	}
	cfg.EnsureRoutingMu()
	cfg.LockRouting()
	cfg.Routing = &config.RoutingConfig{Groups: map[string][]string{"g1": {"p1"}, "g2": {"p2"}}}
	cfg.UnlockRouting()
	srv := newTestServer(t, &strategyFakeEngine{},
		WithConfig(cfg),
		WithDashboardProviders(mcpTestProviders{providers: []provider.DashboardProvider{
			{ID: "p1", Name: "Coder", Enabled: true, WireFormat: "openai"},
			{ID: "p2", Name: "Chatter", Enabled: true, WireFormat: "anthropic"},
		}}),
		WithRegistry(mcpTestRegistry{models: []Model{
			{ID: "a", ProviderID: "p1", OwnedBy: "Coder"},
			{ID: "c", ProviderID: "p2", OwnedBy: "Chatter"},
		}}),
	)
	return srv, routerA, routerB, adminRaw
}

// TestMCPHTTPActiveComboIsPerSession proves H1: the active combo
// is per session, not server-global. Key A switches to g1; key B
// on a different session must not see it — its explain_route for
// a model only p2 offers succeeds (before the fix it failed with
// `combo "g1" offers no provider for this model`, because B saw
// combo:"g1"), its list_combos shows no active combo, and the two
// sessions can then hold different selections (and clear one
// without touching the other).
func TestMCPHTTPActiveComboIsPerSession(t *testing.T) {
	srv, routerA, routerB, adminRaw := mcpHardeningTestServer(t)
	enableMCPTransport(t, srv, adminRaw, "streamable")
	sessA := mcpInitialize(t, srv, routerA)
	sessB := mcpInitialize(t, srv, routerB)

	// A switches to g1.
	sc := mcpToolResult(t, mcpToolCall(t, srv, routerA, sessA, 2, "switch_combos", `{"name":"g1"}`))
	if sc["active_combo"] != "g1" {
		t.Fatalf("A switch_combos active_combo = %v, want g1", sc["active_combo"])
	}
	// B explains a model only p2 offers: g1 offers no provider
	// for it, so a leaked g1 would error here instead.
	sc = mcpToolResult(t, mcpToolCall(t, srv, routerB, sessB, 3, "explain_route", `{"model":"c"}`))
	route, _ := sc["route"].(map[string]any)
	if route == nil {
		t.Fatalf("B explain_route has no route: %v", sc)
	}
	if route["combo"] != nil {
		t.Errorf("B explain_route combo = %v, want none (A's g1 leaked into B's session)", route["combo"])
	}
	attempts, _ := route["attempts"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("B explain_route attempts = %v, want exactly 1 (p2 offers c)", attempts)
	}
	att, _ := attempts[0].(map[string]any)
	if att["provider"] != "p2" {
		t.Errorf("B explain_route attempt = %v, want provider p2", att)
	}
	// B's list_combos shows no active combo; A's shows g1.
	sc = mcpToolResult(t, mcpToolCall(t, srv, routerB, sessB, 4, "list_combos", `{}`))
	if sc["active_combo"] != nil {
		t.Errorf("B list_combos active_combo = %v, want nil (A's g1 leaked)", sc["active_combo"])
	}
	sc = mcpToolResult(t, mcpToolCall(t, srv, routerA, sessA, 5, "list_combos", `{}`))
	if sc["active_combo"] != "g1" {
		t.Errorf("A list_combos active_combo = %v, want g1", sc["active_combo"])
	}
	// B switches to g2; A still sees g1.
	sc = mcpToolResult(t, mcpToolCall(t, srv, routerB, sessB, 6, "switch_combos", `{"name":"g2"}`))
	if sc["active_combo"] != "g2" {
		t.Fatalf("B switch_combos active_combo = %v, want g2", sc["active_combo"])
	}
	sc = mcpToolResult(t, mcpToolCall(t, srv, routerA, sessA, 7, "list_combos", `{}`))
	if sc["active_combo"] != "g1" {
		t.Errorf("A list_combos active_combo = %v after B switched, want g1", sc["active_combo"])
	}
	// A clears its selection; B keeps g2.
	sc = mcpToolResult(t, mcpToolCall(t, srv, routerA, sessA, 8, "switch_combos", `{"name":"g1","active":false}`))
	if sc["active_combo"] != nil {
		t.Errorf("A clear active_combo = %v, want nil", sc["active_combo"])
	}
	sc = mcpToolResult(t, mcpToolCall(t, srv, routerB, sessB, 9, "list_combos", `{}`))
	if sc["active_combo"] != "g2" {
		t.Errorf("B list_combos active_combo = %v after A cleared, want g2", sc["active_combo"])
	}
}

// TestMCPHTTPStreamGetSecondStreamRefused proves H2: a second
// concurrent GET stream on one Streamable session is refused
// with 409 (one stream per session), instead of holding another
// goroutine + connection the idle sweep will never evict. Once
// the first stream closes, a fresh GET succeeds — the cap lifts
// with the stream, it is not a permanent ban.
func TestMCPHTTPStreamGetSecondStreamRefused(t *testing.T) {
	srv, keys := newMCPHTTPTestServer(t)
	enableMCPTransport(t, srv, keys.admin, "streamable")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	sess := mcpInitialize(t, srv, keys.wide)

	openStream := func(ctx context.Context) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/mcp/stream", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+keys.wide)
		req.Header.Set("Mcp-Session-Id", sess)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("GET /api/mcp/stream: %v", err)
		}
		return resp
	}

	// First GET holds the session's one stream.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	resp1 := openStream(ctx1)
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("first GET /api/mcp/stream: status %d, want 200: %s", resp1.StatusCode, body)
	}
	// Second concurrent GET is refused.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	resp2 := openStream(ctx2)
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("second GET /api/mcp/stream: status %d, want 409: %s", resp2.StatusCode, body)
	}
	if !strings.Contains(string(body), "already has an open") {
		t.Errorf("second GET body = %q, want it to say the session already has an open stream", body)
	}
	// Close the first stream; a fresh GET then succeeds.
	cancel1()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx3, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
		resp3 := openStream(ctx3)
		if resp3.StatusCode == http.StatusOK {
			resp3.Body.Close()
			cancel3()
			break
		}
		resp3.Body.Close()
		cancel3()
		if time.Now().After(deadline) {
			t.Fatalf("GET after closing the first stream: status %d, want 200 (the cap never lifted)", resp3.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMCPHTTPInspectDBsHidesDataDirPath proves the H4
// inspectDBs half: when a database file cannot be counted, the
// error the remote caller sees is a generic "unavailable"
// message — the sqlite error (whose DSN embeds the full
// data-dir path) is logged server-side only. The failure is
// still surfaced (DBHealth goes unhealthy), just without the
// path.
func TestMCPHTTPInspectDBsHidesDataDirPath(t *testing.T) {
	dataDir := t.TempDir()
	// A ledger.db the inspector can stat but cannot count: a
	// directory in place of the file.
	if err := os.Mkdir(filepath.Join(dataDir, "ledger.db"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig(dataDir)
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))
	src := &serverMCPSource{srv: srv, st: mcpHTTPStateFor(srv)}
	health := src.DBHealth()
	if health.Healthy {
		t.Errorf("DBHealth().Healthy = true with an unreadable ledger.db, want false (the failure must still surface)")
	}
	found := false
	for _, f := range health.Files {
		if strings.Contains(f.Error, dataDir) {
			t.Errorf("DBHealth file %q error %q leaks the data-dir path", f.Name, f.Error)
		}
		if f.Name == "ledger.db" {
			found = true
			if f.Error == "" {
				t.Errorf("ledger.db error is empty, want a generic unavailable message")
			} else if f.Error != "count usage_events: unavailable" {
				t.Errorf("ledger.db error = %q, want the generic %q", f.Error, "count usage_events: unavailable")
			}
		}
	}
	if !found {
		t.Errorf("no ledger.db entry in DBHealth files: %+v", health.Files)
	}
	// Same invariant via DBStats.
	for _, f := range src.DBStats().Files {
		if strings.Contains(f.Error, dataDir) {
			t.Errorf("DBStats file %q error %q leaks the data-dir path", f.Name, f.Error)
		}
	}
}

// TestMCPHTTPCreateComboCancelledContextAborts proves the H6
// threading reaches ComboStore.Create — and with it the
// CreateCombo ctx.Err() → 503 guard (review nit 1), which could
// never fire while CreateCombo passed context.Background(). A
// tools/call that arrives on an already-cancelled request
// context is aborted before any persist, and the combo is not
// created.
func TestMCPHTTPCreateComboCancelledContextAborts(t *testing.T) {
	srv, routerRaw, _, adminRaw := mcpHardeningTestServer(t)
	enableMCPTransport(t, srv, adminRaw, "streamable")
	sess := mcpInitialize(t, srv, routerRaw)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client went away before dispatch
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/stream",
		strings.NewReader(rpcBody(2, "tools/call", `{"name":"create_combo","arguments":{"name":"cancelled","members":["p1"]}}`))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+routerRaw)
	req.Header.Set("Mcp-Session-Id", sess)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancelled create_combo: HTTP status %d, want 200 with a JSON-RPC error envelope: %s", rec.Code, rec.Body.String())
	}
	code, msg := mcpToolError(t, decodeEnvelope(t, rec))
	if code != -32602 || !strings.Contains(msg, "context canceled") {
		t.Errorf("cancelled create_combo code/msg = %v / %q, want -32602 naming the cancelled context", code, msg)
	}
	// The combo was not created.
	sc := mcpToolResult(t, mcpToolCall(t, srv, routerRaw, sess, 3, "list_combos", `{}`))
	combos, _ := sc["combos"].([]any)
	for _, c := range combos {
		if combo, _ := c.(map[string]any); combo["name"] == "cancelled" {
			t.Errorf("list_combos contains %q after the cancelled create — the aborted create persisted it", "cancelled")
		}
	}
}
