// MCPSSE fixture harness (M9.10) — the SSE-transport equivalent of
// the Streamable-HTTP fixture harness (mcp_http_fixture.go), which is
// itself the HTTP equivalent of the stdio MCP fixture harness
// (mcp_fixture.go). Same conventions: stdlib only (plus the packages
// under test); the loader rejects fixture != true, schema_version
// != 1, path traversal and unknown fields; fixtures are RECORDED data
// from a real server over the wire, never live provider data.
//
// A fixture is one recorded JSON-RPC 2.0 request/response exchange
// over SSE, plus the assertions a contract test must check — the
// POST status (202: the POST only accepts the message, the result
// arrives on the stream) and the JSON-RPC envelope. Fixtures live
// under tests/fixtures/mcp-sse/<name>.json and are recorded by
// running the REAL internal/server Server over httptest and capturing
// the stream's message event verbatim; see TestRecordMCPSSEFixtures
// in mcp_sse_fixture_test.go, which re-records them and refuses to
// write a fixture whose assertions do not match the captured
// response. The test file builds the server (from exported
// production API only — server.New, config.IssueKey, the With*
// options); this file only drives it over HTTP.
//
// How SSE differs from Streamable (and why this is a separate
// harness, not a flag on the Streamable one):
//
//   - The session opens with GET /api/mcp/sse, not with the
//     initialize POST. The first event on the stream is "endpoint"
//     with data "/api/mcp/sse?session_id=<id>"; that id addresses
//     every later POST. There is no Mcp-Session-Id header.
//   - A request is POST /api/mcp/sse?session_id=<id> with the
//     JSON-RPC object. The POST answers 202 Accepted with an empty
//     body; the JSON-RPC response arrives later as a "message"
//     event on the GET stream. The recorded_http_status of every
//     SSE fixture is therefore 202, and the recorded_response is
//     the message event's data, not the POST body.
//   - The client does NOT send notifications/initialized after
//     initialize over SSE (the Streamable flow does): the existing
//     SSE tests drive tools/call straight after the initialize
//     round trip.
//   - Closing the GET terminates the session: a further POST is a
//     404. (Streamable sessions survive a closed GET until DELETE.)
//
// Design choice — real HTTP, in-process server: the runner serves
// the production Server on an httptest server on loopback and talks
// to it with a plain http.Client, the way a real MCP client sees
// it (gateway-key auth, endpoint event, 202 posts, message events).
// The runner itself never imports internal/server and knows nothing
// about how the handler is built: it drives an http.Handler
// supplied by the caller. That separation is load-bearing, not
// style — internal/server's test files import internal/apitest, so
// if this package (the library half) imported internal/server,
// building the server test binary would hit an import cycle.
// Server construction lives in mcp_sse_fixture_test.go, which may
// import server freely.
//
// Schema (version 1) — the Streamable MCPHTTPFixture schema with
// the transport changed and the session-header assertion removed
// (SSE has no session header):
//
//	{
//	  "schema_version": 1, "slice": "m9.10", "name": "<name>",
//	  "fixture": true, "transport": "sse",
//	  "recorded_from": "<how/when the response was recorded>",
//	  "key": "<name of the test gateway key used, e.g. \"wide\">",
//	  "method": "<JSON-RPC method, e.g. tools/call>",
//	  "request": {"jsonrpc": "2.0", "id": 1, "method": "...", "params": {...}},
//	  "recorded_response": {"jsonrpc": "2.0", "id": 1, "result": {...}},
//	  "recorded_http_status": 202,
//	  "assert": {
//	    "id": 1, "expect_error": false,
//	    "error_code": -32602,          // required when expect_error is true
//	    "required_fields": ["result.protocolVersion"],
//	    "json_paths": {"result.protocolVersion": "2025-03-26"},
//	    "http_status": 202
//	  }
//	}
//
// Assertions are looked up in the whole response envelope, so
// "result.protocolVersion" checks a success payload and "error.code"
// checks an error payload. Each json_paths entry is a dotted path
// (numeric segments index arrays); a null expected value means "the
// field must exist, any value". Numbers compare by JSON value, so 2
// and 2.0 are equal. Values that are random per run (gateway-key
// ids when keys are issued randomly, memory entry ids, uptime,
// ledger file sizes) must be asserted presence-only (null) or not
// at all — never pinned to the recorded value, or the fixture would
// fail on the next run. The SSE fixture servers issue gateway keys
// with FIXED ids (see mcp_sse_fixture_test.go), so key ids in
// responses ARE deterministic there and may be pinned exactly.
// The recorded_response is the verbatim capture; only the assert
// block is checked at test time.
//
// Usage:
//
//	client := apitest.NewMCPSSEClient(ts.URL, wideRawKey)
//	apitest.RunMCPSSESession(t, client,
//	    apitest.LoadMCPSSEFixture(t, "initialize"),
//	    apitest.LoadMCPSSEFixture(t, "tools-list"),
//	    apitest.LoadMCPSSEFixture(t, "tools-call-router-status"))
//
// RunMCPSSESession opens the stream, drives the whole session
// through one client (initialize first, then tools/list and
// tools/call), and closes the stream at the end. RunMCPSSE drives
// a single fixture exchange through an already-open stream.
package apitest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// MCPSSEFixture is one recorded SSE JSON-RPC exchange plus its
// assertions. See the package doc comment for the schema.
type MCPSSEFixture struct {
	SchemaVersion int    `json:"schema_version"`
	Slice         string `json:"slice"`
	Name          string `json:"name"`
	// IsFixture must be true. It labels the file as recorded data,
	// never live provider data. The loader rejects anything else.
	IsFixture bool `json:"fixture"`
	// Transport is the MCP HTTP transport the exchange was recorded
	// over. This harness only records "sse".
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
	// server sent as a "message" event on the stream when the
	// fixture was recorded, verbatim.
	RecordedResponse json.RawMessage `json:"recorded_response"`
	// RecordedHTTPStatus is the HTTP status of the POST that
	// carried the exchange (202: the POST only accepts the
	// message). It must equal Assert.HTTPStatus.
	RecordedHTTPStatus int          `json:"recorded_http_status"`
	Assert             MCPSSEAssert `json:"assert"`
	Notes              string       `json:"notes,omitempty"`
}

// MCPSSEAssert is the stdio MCPAssert plus the HTTP layer: the
// expected POST status. Every SSE POST answers 202.
type MCPSSEAssert struct {
	MCPAssert
	// HTTPStatus is the expected HTTP status of the POST that
	// carries the exchange (202 for SSE).
	HTTPStatus int `json:"http_status"`
}

// asMCPFixture converts an SSE fixture to the stdio fixture shape
// so the envelope assertions reuse CheckMCPResponse /
// validateMCPFixture unchanged.
func (f MCPSSEFixture) asMCPFixture() MCPFixture {
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

// MCPSSEFixturePath resolves the on-disk path of the SSE MCP
// fixture (name). Name must be a plain file name: no separators,
// no "..".
func MCPSSEFixturePath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("apitest: mcp-sse fixture name must be non-empty")
	}
	if strings.Contains(name, "/") || strings.Contains(name, `\`) || name == "." || name == ".." {
		return "", fmt.Errorf("apitest: mcp-sse fixture name must be a plain file name, got %q", name)
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("apitest: cannot locate package directory")
	}
	// This file lives in <repo>/internal/apitest; SSE fixtures
	// live in <repo>/tests/fixtures/mcp-sse/<name>.json.
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "tests", "fixtures", "mcp-sse")
	return filepath.Join(dir, name+".json"), nil
}

// LoadMCPSSEFixture loads and validates the SSE MCP fixture
// (name), failing the test on any problem (missing file, bad JSON,
// schema violation).
func LoadMCPSSEFixture(t *testing.T, name string) MCPSSEFixture {
	t.Helper()
	path, err := MCPSSEFixturePath(name)
	if err != nil {
		t.Fatalf("apitest: mcp-sse fixture path: %v", err)
	}
	f, err := loadMCPSSEFixtureFile(path)
	if err != nil {
		t.Fatalf("apitest: load mcp-sse fixture %s: %v", name, err)
	}
	if f.Name != name {
		t.Fatalf("apitest: mcp-sse fixture %s declares name=%q", name, f.Name)
	}
	return f
}

// loadMCPSSEFixtureFile reads and validates one SSE MCP fixture
// file.
func loadMCPSSEFixtureFile(path string) (MCPSSEFixture, error) {
	var f MCPSSEFixture
	raw, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := validateMCPSSEFixture(f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// validateMCPSSEFixture enforces the SSE fixture schema on top of
// the stdio envelope validation (reused verbatim).
func validateMCPSSEFixture(f MCPSSEFixture) error {
	if err := validateMCPFixture(f.asMCPFixture()); err != nil {
		return err
	}
	switch {
	case f.Transport != "sse":
		return fmt.Errorf("transport must be \"sse\", got %q", f.Transport)
	case f.Key == "":
		return fmt.Errorf("key is required (name of the test gateway key the exchange was recorded with)")
	case f.RecordedHTTPStatus < 100 || f.RecordedHTTPStatus > 599:
		return fmt.Errorf("recorded_http_status must be a valid HTTP status, got %d", f.RecordedHTTPStatus)
	case f.Assert.HTTPStatus < 100 || f.Assert.HTTPStatus > 599:
		return fmt.Errorf("assert.http_status must be a valid HTTP status, got %d", f.Assert.HTTPStatus)
	case f.RecordedHTTPStatus != f.Assert.HTTPStatus:
		return fmt.Errorf("recorded_http_status %d does not match assert.http_status %d", f.RecordedHTTPStatus, f.Assert.HTTPStatus)
	}
	return nil
}

// CheckMCPSSEResponse checks one live SSE exchange against the
// fixture's assertions and returns the first mismatch as an error:
// the POST status first, then the JSON-RPC envelope carried by the
// stream's "message" event, via CheckMCPResponse.
func CheckMCPSSEResponse(postStatus int, messageData []byte, f MCPSSEFixture) error {
	if postStatus != f.Assert.HTTPStatus {
		return fmt.Errorf("http status: got %d, want %d (message data: %.200s)", postStatus, f.Assert.HTTPStatus, messageData)
	}
	if len(messageData) == 0 {
		return fmt.Errorf("no message event data for fixture %s (POST status %d)", f.Name, postStatus)
	}
	return CheckMCPResponse(messageData, f.asMCPFixture())
}

// ---------------------------------------------------------------------------
// SSE event parsing (mirrors the readSSEEvent/streamEvents helpers
// in internal/server/mcp_http_test.go — the same wire format).
// ---------------------------------------------------------------------------

// mcpSSEEvent is one parsed SSE event.
type mcpSSEEvent struct {
	event string
	data  string
}

// readMCPSSEEvent reads one SSE event (lines until a blank line)
// from r. Comment lines (": ping") are skipped.
func readMCPSSEEvent(r *bufio.Reader) (mcpSSEEvent, error) {
	var ev mcpSSEEvent
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

// streamMCPSSEEvents pumps SSE events from r into ch until the
// stream ends (then errCh gets the read error).
func streamMCPSSEEvents(r *bufio.Reader, ch chan<- mcpSSEEvent, errCh chan<- error) {
	for {
		ev, err := readMCPSSEEvent(r)
		if err != nil {
			errCh <- err
			return
		}
		ch <- ev
	}
}

// readMCPSSEEventTimeout reads one event or fails the test after d.
func readMCPSSEEventTimeout(t *testing.T, ch <-chan mcpSSEEvent, errCh <-chan error, d time.Duration) mcpSSEEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case err := <-errCh:
		t.Fatalf("SSE stream read failed: %v", err)
	case <-time.After(d):
		t.Fatal("timed out waiting for an SSE event")
	}
	return mcpSSEEvent{}
}

// ---------------------------------------------------------------------------
// Transport client
// ---------------------------------------------------------------------------

// MCPSSEClient is one SSE MCP client: a base URL, one gateway key,
// and the open GET stream once Open has run. It talks to the
// server with a plain http.Client over loopback — no in-process
// shortcut, so auth, the endpoint event, 202 posts and message
// events are exercised exactly as a real client sees them.
//
// Two HTTP clients: the GET stream stays open for the life of the
// session, so it uses a client with no total timeout; POSTs use a
// client with a 15s timeout (plus a per-request context).
type MCPSSEClient struct {
	baseURL    string
	key        string
	sessionID  string
	getClient  *http.Client
	postClient *http.Client
	getResp    *http.Response
	getCancel  context.CancelFunc
	events     chan mcpSSEEvent
	errs       chan error
}

// NewMCPSSEClient returns a client for baseURL authenticating with
// the raw gateway key. No stream is open until Open.
func NewMCPSSEClient(baseURL, rawKey string) *MCPSSEClient {
	return &MCPSSEClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		key:        rawKey,
		getClient:  &http.Client{},
		postClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// SessionID returns the open session id, or "" before Open.
func (c *MCPSSEClient) SessionID() string { return c.sessionID }

// Open opens the SSE stream: GET /api/mcp/sse, require HTTP 200
// plus Content-Type text/event-stream, then read the first event
// (which must be "endpoint") and record the session id from its
// data. The stream stays open until Close.
func (c *MCPSSEClient) Open(t *testing.T) string {
	t.Helper()
	if c.sessionID != "" {
		t.Fatalf("apitest: mcp-sse client already has session %s; use a fresh client per stream", c.sessionID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/mcp/sse", nil)
	if err != nil {
		cancel()
		t.Fatalf("apitest: mcp-sse build GET: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.getClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("apitest: mcp-sse GET /api/mcp/sse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		t.Fatalf("apitest: mcp-sse GET /api/mcp/sse: status %d, want 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		cancel()
		t.Fatalf("apitest: mcp-sse GET /api/mcp/sse Content-Type = %q, want text/event-stream", ct)
	}
	c.getResp = resp
	c.getCancel = cancel
	c.events = make(chan mcpSSEEvent, 8)
	c.errs = make(chan error, 1)
	go streamMCPSSEEvents(bufio.NewReader(resp.Body), c.events, c.errs)
	ev := readMCPSSEEventTimeout(t, c.events, c.errs, 5*time.Second)
	if ev.event != "endpoint" {
		c.Close(t)
		t.Fatalf("apitest: mcp-sse first event = %q, want endpoint", ev.event)
	}
	sessionID := strings.TrimPrefix(ev.data, "/api/mcp/sse?session_id=")
	if sessionID == ev.data || sessionID == "" {
		c.Close(t)
		t.Fatalf("apitest: mcp-sse endpoint data = %q, want /api/mcp/sse?session_id=<id>", ev.data)
	}
	c.sessionID = sessionID
	return sessionID
}

// mcpSSEInitializeRequest is the initialize request the client
// sends when opening a session outside a fixture (standalone test
// setup). It matches the recorded initialize fixture exactly.
const mcpSSEInitializeRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"mcp-sse-fixture-client","version":"0"}}}`

// Post sends one JSON-RPC request object to POST
// /api/mcp/sse?session_id=<id> with the client's key and returns
// the POST status plus the "message" event data carrying the
// JSON-RPC response. The POST itself answers 202 with an empty
// body; the result arrives on the stream. When the POST status is
// not 202 there is no message event to wait for, so the data is
// nil and the caller reports the status mismatch. It performs no
// assertions — callers that drive fixtures use RunMCPSSE /
// CheckMCPSSEResponse.
func (c *MCPSSEClient) Post(t *testing.T, request json.RawMessage) (postStatus int, messageData []byte) {
	t.Helper()
	if c.sessionID == "" {
		t.Fatalf("apitest: mcp-sse POST: no stream open; call Open first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/mcp/sse?session_id="+c.sessionID, bytes.NewReader(request))
	if err != nil {
		t.Fatalf("apitest: mcp-sse build POST: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.postClient.Do(req)
	if err != nil {
		t.Fatalf("apitest: mcp-sse POST /api/mcp/sse: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("apitest: mcp-sse read POST response body: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		return resp.StatusCode, body
	}
	if strings.TrimSpace(string(body)) != "" {
		t.Fatalf("apitest: mcp-sse POST: body %q, want empty (202 with the result on the stream)", body)
	}
	ev := readMCPSSEEventTimeout(t, c.events, c.errs, 5*time.Second)
	if ev.event != "message" {
		t.Fatalf("apitest: mcp-sse response event = %q, want message", ev.event)
	}
	return resp.StatusCode, []byte(ev.data)
}

// Initialize runs the initialize handshake over the open stream:
// POST initialize (id 1) and require a result reporting protocol
// 2025-03-26. Unlike Streamable, no notifications/initialized is
// sent afterwards — the SSE tests drive tools/call straight after
// the initialize round trip. This is the raw setup step standalone
// fixture tests use before driving a tools/call fixture; the
// recorded initialize fixture itself is driven through RunMCPSSE,
// which performs the same POST with the fixture's assertions.
func (c *MCPSSEClient) Initialize(t *testing.T) {
	t.Helper()
	status, data := c.Post(t, json.RawMessage(mcpSSEInitializeRequest))
	if status != http.StatusAccepted {
		t.Fatalf("apitest: mcp-sse initialize: POST status %d, want 202: %s", status, data)
	}
	var env map[string]any
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("apitest: mcp-sse initialize: response is not JSON: %v (data %q)", err, data)
	}
	if _, hasError := env["error"]; hasError {
		t.Fatalf("apitest: mcp-sse initialize: server returned an error: %s", data)
	}
	result, _ := env["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("apitest: mcp-sse initialize: protocolVersion = %v, want 2025-03-26", result["protocolVersion"])
	}
}

// Close ends the session by closing the GET stream (which
// terminates the session server-side) and clears the client's
// session id. It is a no-op when no stream is open.
func (c *MCPSSEClient) Close(t *testing.T) {
	t.Helper()
	if c.getResp != nil {
		c.getResp.Body.Close()
		c.getResp = nil
	}
	if c.getCancel != nil {
		c.getCancel()
		c.getCancel = nil
	}
	c.sessionID = ""
}

// RunMCPSSE drives one fixture exchange through the client and
// fails the test on the first contract mismatch. The stream must
// already be open (Open, or RunMCPSSESession) — the initialize
// fixture is just another POST on that stream, because the SSE
// session opens at GET, not at initialize.
func RunMCPSSE(t *testing.T, c *MCPSSEClient, f MCPSSEFixture) {
	t.Helper()
	if c.sessionID == "" {
		t.Fatalf("apitest: mcp-sse fixture %s: no stream open; call Open first (or drive the whole flow with RunMCPSSESession)", f.Name)
	}
	status, data := c.Post(t, f.Request)
	if err := CheckMCPSSEResponse(status, data, f); err != nil {
		t.Fatalf("apitest: mcp-sse fixture %s: %v", f.Name, err)
	}
}

// RunMCPSSESession drives a sequence of fixtures through one
// client stream, in order, and closes the stream at the end. The
// first fixture must be initialize; the rest run over that stream.
// This is the realistic client flow: GET opens the stream →
// initialize → tools/list → tools/call — over SSE.
func RunMCPSSESession(t *testing.T, c *MCPSSEClient, fixtures ...MCPSSEFixture) {
	t.Helper()
	if len(fixtures) == 0 {
		t.Fatal("apitest: RunMCPSSESession needs at least one fixture")
	}
	if fixtures[0].Method != "initialize" {
		t.Fatalf("apitest: RunMCPSSESession: first fixture must be initialize, got %q", fixtures[0].Method)
	}
	c.Open(t)
	defer c.Close(t)
	for _, f := range fixtures {
		RunMCPSSE(t, c, f)
	}
}

// ---------------------------------------------------------------------------
// Test-server helpers (handler-agnostic: this file never imports
// internal/server — see the package doc comment on the import
// cycle).
// ---------------------------------------------------------------------------

// EnableMCPHTTPSSE flips the SSE transport on via the real toggle
// endpoint (POST /api/mcp/status with the admin key). A test that
// needs a live transport calls this first: both transports default
// OFF (fail-closed).
func EnableMCPHTTPSSE(t *testing.T, baseURL, adminRawKey string) {
	t.Helper()
	body := strings.NewReader(`{"transport":"sse","enabled":true}`)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/mcp/status", body)
	if err != nil {
		t.Fatalf("apitest: mcp-sse build toggle request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+adminRawKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("apitest: mcp-sse enable sse: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apitest: mcp-sse enable sse: status %d, want 200: %s", resp.StatusCode, respBody)
	}
}
