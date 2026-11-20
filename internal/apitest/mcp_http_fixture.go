// MCP Streamable-HTTP fixture harness (M9.10) — the HTTP-transport
// equivalent of the stdio MCP fixture harness (mcp_fixture.go).
// Same conventions: stdlib only (plus the packages under test);
// the loader rejects fixture != true, schema_version != 1, path
// traversal and unknown fields; fixtures are RECORDED data from a
// real server over the wire, never live provider data.
//
// A fixture is one recorded JSON-RPC 2.0 request/response exchange
// over POST /api/mcp/stream, plus the assertions a contract test
// must check — the HTTP status, the Mcp-Session-Id header where
// the exchange opens a session, and the JSON-RPC envelope. Fixtures
// live under tests/fixtures/mcp-http/<name>.json and are recorded by
// running the REAL internal/server Server over httptest and capturing
// the response verbatim; see TestRecordMCPHTTPFixtures in
// mcp_http_fixture_test.go, which re-records them and refuses to
// write a fixture whose assertions do not match the captured
// response. The test file builds the server (from exported
// production API only — server.New, config.IssueKey, the With*
// options); this file only drives it over HTTP.
//
// Design choice — real HTTP, in-process server: the runner serves
// the production Server on an httptest server on loopback and talks
// to it with a plain http.Client, the way a real MCP client sees
// it (gateway-key auth, session header, 202 notifications, 204
// terminate). The runner itself never imports internal/server and
// knows nothing about how the handler is built: it drives an
// http.Handler supplied by the caller (StartMCPHTTPTestServer).
// That separation is load-bearing, not style — internal/server's
// test files import internal/apitest, so if this package (the
// library half) imported internal/server, building the server test
// binary would hit an import cycle. Server construction (the stub
// engine, the p1/p2 + a/b/c feed, the ledger seeding) therefore
// lives in mcp_http_fixture_test.go, which may import server
// freely: a package's own _test.go files are never compiled when
// another package imports it. Nothing in production code was
// modified to accommodate this harness.
//
// Schema (version 1) — the stdio MCPFixture schema plus the HTTP
// layer:
//
//	{
//	  "schema_version": 1, "slice": "m9.10", "name": "<name>",
//	  "fixture": true, "transport": "streamable",
//	  "recorded_from": "<how/when the response was recorded>",
//	  "key": "<name of the test gateway key used, e.g. \"wide\">",
//	  "method": "<JSON-RPC method, e.g. tools/call>",
//	  "request": {"jsonrpc": "2.0", "id": 1, "method": "...", "params": {...}},
//	  "recorded_response": {"jsonrpc": "2.0", "id": 1, "result": {...}},
//	  "recorded_http_status": 200,
//	  "assert": {
//	    "id": 1, "expect_error": false,
//	    "error_code": -32602,          // required when expect_error is true
//	    "required_fields": ["result.protocolVersion"],
//	    "json_paths": {"result.protocolVersion": "2025-03-26"},
//	    "http_status": 200,
//	    "require_session_header": true // only for initialize
//	  }
//	}
//
// Assertions are looked up in the whole response envelope, so
// "result.protocolVersion" checks a success payload and "error.code"
// checks an error payload. Each json_paths entry is a dotted path
// (numeric segments index arrays); a null expected value means "the
// field must exist, any value". Numbers compare by JSON value, so 2
// and 2.0 are equal. Values that are random per run (gateway-key
// ids, session ids, uptime, ledger window timestamps) must be
// asserted presence-only (null) or not at all — never pinned to the
// recorded value, or the fixture would fail on the next run with a
// fresh key. The recorded_response is the verbatim capture; only
// the assert block is checked at test time.
//
// Usage:
//
//	client := apitest.NewMCPHTTPClient(ts.URL, wideRawKey)
//	apitest.RunMCPHTTPSession(t, client,
//	    apitest.LoadMCPHTTPFixture(t, "initialize"),
//	    apitest.LoadMCPHTTPFixture(t, "tools-list"),
//	    apitest.LoadMCPHTTPFixture(t, "tools-call-router-status"))
//
// RunMCPHTTPSession drives the whole session through one client:
// initialize opens the session (asserting Mcp-Session-Id issuance),
// each later exchange carries the session header, and the session
// is terminated with DELETE at the end (204). RunMCPHTTP drives a
// single fixture exchange through an already-open session (or the
// initialize exchange itself, which opens it).
package apitest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// MCPHTTPFixture is one recorded Streamable-HTTP JSON-RPC exchange
// plus its assertions. See the package doc comment for the schema.
type MCPHTTPFixture struct {
	SchemaVersion int    `json:"schema_version"`
	Slice         string `json:"slice"`
	Name          string `json:"name"`
	// IsFixture must be true. It labels the file as recorded data,
	// never live provider data. The loader rejects anything else.
	IsFixture bool `json:"fixture"`
	// Transport is the MCP HTTP transport the exchange was recorded
	// over. This harness only records "streamable".
	Transport string `json:"transport"`
	// RecordedFrom notes how the recorded response was produced.
	RecordedFrom string `json:"recorded_from,omitempty"`
	// Key names the test gateway key the exchange was recorded
	// with (e.g. "wide", "read"). Informational: the test maps the
	// name to the raw key it issued for its own server; the runner
	// never reads this field to choose credentials.
	Key string `json:"key"`
	// Method is the JSON-RPC method of the request. It must match
	// Request's method.
	Method string `json:"method"`
	// Request is the exact JSON-RPC request object sent, verbatim.
	Request json.RawMessage `json:"request"`
	// RecordedResponse is the exact JSON-RPC response object the
	// server returned when the fixture was recorded, verbatim.
	RecordedResponse json.RawMessage `json:"recorded_response"`
	// RecordedHTTPStatus is the HTTP status of the recorded
	// exchange (200 for a JSON-RPC response). It must equal
	// Assert.HTTPStatus.
	RecordedHTTPStatus int           `json:"recorded_http_status"`
	Assert             MCPHTTPAssert `json:"assert"`
	Notes              string        `json:"notes,omitempty"`
}

// MCPHTTPAssert is the stdio MCPAssert plus the HTTP layer: the
// expected HTTP status and whether the exchange must issue a
// Mcp-Session-Id header (initialize only).
type MCPHTTPAssert struct {
	MCPAssert
	// HTTPStatus is the expected HTTP status of the exchange.
	HTTPStatus int `json:"http_status"`
	// RequireSessionHeader is true when the response must carry a
	// non-empty Mcp-Session-Id header (initialize).
	RequireSessionHeader bool `json:"require_session_header,omitempty"`
}

// asMCPFixture converts an HTTP fixture to the stdio fixture shape
// so the envelope assertions reuse CheckMCPResponse /
// validateMCPFixture unchanged.
func (f MCPHTTPFixture) asMCPFixture() MCPFixture {
	return MCPFixture{
		SchemaVersion:    f.SchemaVersion,
		Slice:            f.Slice,
		Name:             f.Name,
		IsFixture:        f.IsFixture,
		RecordedFrom:     f.RecordedFrom,
		Method:           f.Method,
		Request:          f.Request,
		RecordedResponse: f.RecordedResponse,
		Assert:           f.Assert.MCPAssert,
		Notes:            f.Notes,
	}
}

// MCPHTTPFixturePath resolves the on-disk path of the Streamable
// HTTP MCP fixture (name). Name must be a plain file name: no
// separators, no "..".
func MCPHTTPFixturePath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("apitest: mcp-http fixture name must be non-empty")
	}
	if strings.Contains(name, "/") || strings.Contains(name, `\`) || name == "." || name == ".." {
		return "", fmt.Errorf("apitest: mcp-http fixture name must be a plain file name, got %q", name)
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("apitest: cannot locate package directory")
	}
	// This file lives in <repo>/internal/apitest; Streamable HTTP
	// fixtures live in <repo>/tests/fixtures/mcp-http/<name>.json.
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "tests", "fixtures", "mcp-http")
	return filepath.Join(dir, name+".json"), nil
}

// LoadMCPHTTPFixture loads and validates the Streamable HTTP MCP
// fixture (name), failing the test on any problem (missing file,
// bad JSON, schema violation).
func LoadMCPHTTPFixture(t *testing.T, name string) MCPHTTPFixture {
	t.Helper()
	path, err := MCPHTTPFixturePath(name)
	if err != nil {
		t.Fatalf("apitest: mcp-http fixture path: %v", err)
	}
	f, err := loadMCPHTTPFixtureFile(path)
	if err != nil {
		t.Fatalf("apitest: load mcp-http fixture %s: %v", name, err)
	}
	if f.Name != name {
		t.Fatalf("apitest: mcp-http fixture %s declares name=%q", name, f.Name)
	}
	return f
}

// loadMCPHTTPFixtureFile reads and validates one Streamable HTTP
// MCP fixture file.
func loadMCPHTTPFixtureFile(path string) (MCPHTTPFixture, error) {
	var f MCPHTTPFixture
	raw, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := validateMCPHTTPFixture(f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// validateMCPHTTPFixture enforces the HTTP fixture schema on top of
// the stdio envelope validation (reused verbatim).
func validateMCPHTTPFixture(f MCPHTTPFixture) error {
	if err := validateMCPFixture(f.asMCPFixture()); err != nil {
		return err
	}
	switch {
	case f.Transport != "streamable":
		return fmt.Errorf("transport must be \"streamable\", got %q", f.Transport)
	case f.Key == "":
		return fmt.Errorf("key is required (name of the test gateway key the exchange was recorded with)")
	case f.RecordedHTTPStatus < 100 || f.RecordedHTTPStatus > 599:
		return fmt.Errorf("recorded_http_status must be a valid HTTP status, got %d", f.RecordedHTTPStatus)
	case f.Assert.HTTPStatus < 100 || f.Assert.HTTPStatus > 599:
		return fmt.Errorf("assert.http_status must be a valid HTTP status, got %d", f.Assert.HTTPStatus)
	case f.RecordedHTTPStatus != f.Assert.HTTPStatus:
		return fmt.Errorf("recorded_http_status %d does not match assert.http_status %d", f.RecordedHTTPStatus, f.Assert.HTTPStatus)
	case f.Assert.RequireSessionHeader && f.Method != "initialize":
		return fmt.Errorf("require_session_header is only meaningful for initialize, got method %q", f.Method)
	}
	return nil
}

// CheckMCPHTTPResponse checks one live Streamable HTTP exchange
// against the fixture's assertions and returns the first mismatch
// as an error: HTTP status first, then the Mcp-Session-Id header
// when required, then the JSON-RPC envelope via CheckMCPResponse.
func CheckMCPHTTPResponse(status int, sessionHeader string, body []byte, f MCPHTTPFixture) error {
	if status != f.Assert.HTTPStatus {
		return fmt.Errorf("http status: got %d, want %d (body: %.200s)", status, f.Assert.HTTPStatus, body)
	}
	if f.Assert.RequireSessionHeader && sessionHeader == "" {
		return fmt.Errorf("response carries no Mcp-Session-Id header (body: %.200s)", body)
	}
	return CheckMCPResponse(body, f.asMCPFixture())
}

// ---------------------------------------------------------------------------
// Transport client
// ---------------------------------------------------------------------------

// MCPHTTPClient is one Streamable HTTP MCP client: a base URL, one
// gateway key, and the session id once initialize has opened it.
// It talks to the server with a plain http.Client over loopback —
// no in-process shortcut, so auth, session addressing and status
// codes are exercised exactly as a real client sees them.
type MCPHTTPClient struct {
	baseURL    string
	key        string
	sessionID  string
	httpClient *http.Client
}

// NewMCPHTTPClient returns a client for baseURL authenticating with
// the raw gateway key. No session is open until Initialize.
func NewMCPHTTPClient(baseURL, rawKey string) *MCPHTTPClient {
	return &MCPHTTPClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		key:        rawKey,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// SessionID returns the open session id, or "" before Initialize.
func (c *MCPHTTPClient) SessionID() string { return c.sessionID }

// mcpHTTPInitializeRequest is the initialize request the client
// sends when opening a session outside a fixture (standalone test
// setup). It matches the recorded initialize fixture exactly.
const mcpHTTPInitializeRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"mcp-http-fixture-client","version":"0"}}}`

// Post sends one JSON-RPC request object to POST /api/mcp/stream
// with the client's key (and the session header when a session is
// open) and returns the HTTP status, the Mcp-Session-Id response
// header, and the raw body. It performs no assertions — callers
// that drive fixtures use RunMCPHTTP / CheckMCPHTTPResponse.
func (c *MCPHTTPClient) Post(t *testing.T, request json.RawMessage) (status int, sessionHeader string, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/mcp/stream", bytes.NewReader(request))
	if err != nil {
		t.Fatalf("apitest: mcp-http build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		t.Fatalf("apitest: mcp-http POST /api/mcp/stream: %v", err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("apitest: mcp-http read response body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Mcp-Session-Id"), body
}

// Initialize opens a session: POST initialize (no session header),
// require HTTP 200 plus a Mcp-Session-Id, then confirm with a
// notifications/initialized notification (202, empty body). It
// records the session id on the client and returns it. This is the
// raw setup step standalone fixture tests use before driving a
// tools/call fixture; the recorded initialize fixture itself is
// driven through RunMCPHTTP, which performs the same steps with
// the fixture's assertions.
func (c *MCPHTTPClient) Initialize(t *testing.T) string {
	t.Helper()
	status, sessionID, body := c.Post(t, json.RawMessage(mcpHTTPInitializeRequest))
	if status != http.StatusOK {
		t.Fatalf("apitest: mcp-http initialize: status %d, want 200: %s", status, body)
	}
	if sessionID == "" {
		t.Fatalf("apitest: mcp-http initialize: no Mcp-Session-Id response header: %s", body)
	}
	c.sessionID = sessionID
	// The client confirms initialisation with a notification:
	// the transport must answer 202 with no body.
	status, _, body = c.Post(t, json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`))
	if status != http.StatusAccepted {
		t.Fatalf("apitest: mcp-http notifications/initialized: status %d, want 202: %s", status, body)
	}
	if strings.TrimSpace(string(body)) != "" {
		t.Fatalf("apitest: mcp-http notifications/initialized: body %q, want empty", body)
	}
	return sessionID
}

// Terminate ends the session with DELETE /api/mcp/stream/{id}
// (204) and clears the client's session id. It is a no-op when no
// session is open.
func (c *MCPHTTPClient) Terminate(t *testing.T) {
	t.Helper()
	if c.sessionID == "" {
		return
	}
	id := c.sessionID
	req, err := http.NewRequest(http.MethodDelete, c.baseURL+"/api/mcp/stream/"+id, nil)
	if err != nil {
		t.Fatalf("apitest: mcp-http build DELETE: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		t.Fatalf("apitest: mcp-http DELETE session: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("apitest: mcp-http DELETE session %s: status %d, want 204: %s", id, resp.StatusCode, body)
	}
	c.sessionID = ""
}

// RunMCPHTTP drives one fixture exchange through the client and
// fails the test on the first contract mismatch. The initialize
// fixture opens the session (capturing Mcp-Session-Id and sending
// notifications/initialized); any other fixture requires an open
// session — open one first with Initialize, or drive the whole
// flow with RunMCPHTTPSession.
func RunMCPHTTP(t *testing.T, c *MCPHTTPClient, f MCPHTTPFixture) {
	t.Helper()
	if f.Method == "initialize" {
		if c.sessionID != "" {
			t.Fatalf("apitest: mcp-http fixture %s: client already has session %s; use a fresh client per initialize", f.Name, c.sessionID)
		}
		status, sessionID, body := c.Post(t, f.Request)
		if err := CheckMCPHTTPResponse(status, sessionID, body, f); err != nil {
			t.Fatalf("apitest: mcp-http fixture %s: %v", f.Name, err)
		}
		c.sessionID = sessionID
		// Confirm initialisation exactly as Initialize does.
		status, _, body = c.Post(t, json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`))
		if status != http.StatusAccepted {
			t.Fatalf("apitest: mcp-http fixture %s: notifications/initialized: status %d, want 202: %s", f.Name, status, body)
		}
		if strings.TrimSpace(string(body)) != "" {
			t.Fatalf("apitest: mcp-http fixture %s: notifications/initialized: body %q, want empty", f.Name, body)
		}
		return
	}
	if c.sessionID == "" {
		t.Fatalf("apitest: mcp-http fixture %s: no session open; initialize first (Initialize or RunMCPHTTPSession)", f.Name)
	}
	status, _, body := c.Post(t, f.Request)
	if err := CheckMCPHTTPResponse(status, "", body, f); err != nil {
		t.Fatalf("apitest: mcp-http fixture %s: %v", f.Name, err)
	}
}

// RunMCPHTTPSession drives a sequence of fixtures through one
// client session, in order, and terminates the session at the end.
// The first fixture must be initialize (it opens the session); the
// rest run over that session. This is the realistic client flow:
// initialize → tools/list → tools/call — over Streamable HTTP.
func RunMCPHTTPSession(t *testing.T, c *MCPHTTPClient, fixtures ...MCPHTTPFixture) {
	t.Helper()
	if len(fixtures) == 0 {
		t.Fatal("apitest: RunMCPHTTPSession needs at least one fixture")
	}
	if fixtures[0].Method != "initialize" {
		t.Fatalf("apitest: RunMCPHTTPSession: first fixture must be initialize, got %q", fixtures[0].Method)
	}
	for i, f := range fixtures {
		RunMCPHTTP(t, c, f)
		_ = i
	}
	c.Terminate(t)
}

// ---------------------------------------------------------------------------
// Test-server helpers (handler-agnostic: this file never imports
// internal/server — see the package doc comment on the import
// cycle. The caller builds the handler however it needs to, e.g.
// with server.New, and hands it here to be served on loopback.)
// ---------------------------------------------------------------------------

// IssueMCPHTTPKey issues a gateway key with the given scopes into
// cfg and returns the stored key and its raw secret — the exported
// equivalent of the issueScopedKey helper in internal/server tests
// (config.IssueKey + Config.AddGatewayKey). The raw secret is a
// test credential for the loopback server only; never log it.
func IssueMCPHTTPKey(t *testing.T, cfg *config.Config, name string, scopes []string) (config.GatewayKey, string) {
	t.Helper()
	k, raw, err := config.IssueKey(name, scopes, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(k); err != nil {
		t.Fatal(err)
	}
	return k, raw
}

// StartMCPHTTPTestServer serves handler on an httptest server on
// loopback and registers a cleanup that closes it. The Streamable
// transport is still DISABLED after this returns (fail-closed
// default) — enable it with EnableMCPHTTPStreamable.
func StartMCPHTTPTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// EnableMCPHTTPStreamable flips the Streamable transport on via
// the real toggle endpoint (POST /api/mcp/status with the admin
// key). A test that needs a live transport calls this first: both
// transports default OFF (fail-closed).
func EnableMCPHTTPStreamable(t *testing.T, baseURL, adminRawKey string) {
	t.Helper()
	body := strings.NewReader(`{"transport":"streamable","enabled":true}`)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/mcp/status", body)
	if err != nil {
		t.Fatalf("apitest: mcp-http build toggle request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+adminRawKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("apitest: mcp-http enable streamable: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apitest: mcp-http enable streamable: status %d, want 200: %s", resp.StatusCode, respBody)
	}
}
