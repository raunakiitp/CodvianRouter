// MCP fixture harness (M9.0) — the MCP equivalent of the M8.0 HTTP
// apitest harness (fixture.go). Same conventions: stdlib only; the
// loader rejects fixture != true, schema_version != 1, path traversal
// and unknown fields; fixtures are RECORDED mock data, never live
// provider data.
//
// A fixture is one recorded JSON-RPC 2.0 request/response exchange
// with the MCP stdio server, plus the assertions a contract test must
// check. Fixtures live under tests/fixtures/mcp/<name>.json and are
// recorded by running the REAL internal/mcp Server (mock-backed,
// deterministic Source — 2 providers, 2 models, fixed status) over
// stdio and capturing the response verbatim; see TestRecordMCPFixtures
// in mcp_fixture_test.go, which re-records them and refuses to write
// a fixture whose assertions do not match the captured response.
//
// Design choice — in-process, not a subprocess: apitest cannot import
// cmd/main, and driving the built binary would need a config file and
// a registry, making the tests slow and config-dependent. Driving
// mcp.Serve in-process exercises the exact same Handle/Serve code
// path the `codvian-router mcp` subcommand wraps (that subcommand is
// a thin adapter: registryMCPSource + mcp.New(...).Serve(os.Stdin,
// os.Stdout); its own round-trip is already covered by
// cmd/codvian-router/doctor_configure_mcp_test.go). The Source the
// harness uses is a deterministic mock, so responses are reproducible
// and recordable — the same discipline as the HTTP harness's mock
// upstream. No fixture invents a price, a capability, or a provider
// behaviour: the recorded responses carry only the mock registry's
// counts, ids and names.
//
// Schema (version 1):
//
//	{
//	  "schema_version": 1, "slice": "m9.0", "name": "<name>",
//	  "fixture": true,
//	  "recorded_from": "<how/when the response was recorded>",
//	  "method": "<JSON-RPC method, e.g. tools/call>",
//	  "request": {"jsonrpc": "2.0", "id": 1, "method": "...", "params": {...}},
//	  "recorded_response": {"jsonrpc": "2.0", "id": 1, "result": {...}},
//	  "assert": {
//	    "id": 1, "expect_error": false,
//	    "error_code": -32602,          // required when expect_error is true
//	    "required_fields": ["result.protocolVersion"],
//	    "json_paths": {"result.protocolVersion": "2025-03-26"},
//	    "json_lengths": {"result.tools": 24}   // array length pin
//	  }
//	}
//
// Assertions are looked up in the whole response envelope, so
// "result.protocolVersion" checks a success payload and "error.code"
// checks an error payload. Each json_paths entry is a dotted path
// (numeric segments index arrays, e.g. "result.tools.0.name"); a null
// expected value means "the field must exist, any value". Numbers
// compare by JSON value, so 2 and 2.0 are equal.
//
// Usage:
//
//	f := apitest.LoadMCPFixture(t, "initialize")
//	apitest.RunMCP(t, serverUnderTest, f)
//
// RunMCP drives one request/response exchange through srv.Serve
// (in-memory stdio) and fails the test on the first mismatch.
// RunMCPSession drives a sequence of fixtures through a single
// Serve session, in order — the realistic client flow (initialize →
// tools/list → tools/call) — and asserts every response.
package apitest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/mcp"
)

// MCPFixture is one recorded JSON-RPC request/response exchange plus
// its assertions. See the package doc comment for the schema.
type MCPFixture struct {
	SchemaVersion int    `json:"schema_version"`
	Slice         string `json:"slice"`
	Name          string `json:"name"`
	// IsFixture must be true. It labels the file as recorded mock
	// data, never live provider data. The loader rejects anything else.
	IsFixture bool `json:"fixture"`
	// RecordedFrom notes how the recorded response was produced, e.g.
	// "recorded 2026-11-19 by running the real internal/mcp Server
	// (mock-backed deterministic Source) in-process over stdio".
	RecordedFrom string `json:"recorded_from,omitempty"`
	// Method is the JSON-RPC method of the request (initialize,
	// tools/list, tools/call, ping, ...). It must match
	// Request's method.
	Method string `json:"method"`
	// Request is the exact JSON-RPC request object sent, verbatim.
	Request json.RawMessage `json:"request"`
	// RecordedResponse is the exact JSON-RPC response object the
	// server returned when the fixture was recorded, verbatim.
	RecordedResponse json.RawMessage `json:"recorded_response"`
	Assert           MCPAssert       `json:"assert"`
	Notes            string          `json:"notes,omitempty"`
}

// MCPAssert declares what the contract test checks about the live
// response. Paths are resolved against the whole response envelope.
type MCPAssert struct {
	// ID is the expected JSON-RPC response id; it must equal the
	// request id. Required (the loader rejects a missing id).
	ID any `json:"id"`
	// ExpectError is true for fixtures whose response is a JSON-RPC
	// error (unknown tool, unknown method, ...).
	ExpectError bool `json:"expect_error"`
	// ErrorCode is the expected JSON-RPC error code when
	// ExpectError is true (e.g. -32602). Required in that case.
	ErrorCode *int `json:"error_code,omitempty"`
	// RequiredFields are dotted JSON paths that must exist in the
	// response envelope (numeric segments index arrays).
	RequiredFields []string `json:"required_fields,omitempty"`
	// JSONPaths maps a dotted JSON path to its expected value. A
	// null expected value means "must exist, any value".
	JSONPaths map[string]any `json:"json_paths,omitempty"`
	// JSONLengths maps a dotted JSON path to the expected length of
	// the array at that path (e.g. "result.tools": 24 pins the
	// tool count, so a 25th tool fails even when every pinned
	// name still matches). json_paths cannot express array
	// length, so this is the length pin.
	JSONLengths map[string]int `json:"json_lengths,omitempty"`
}

// MCPFixturePath resolves the on-disk path of the MCP fixture
// (name). Name must be a plain file name: no separators, no "..".
func MCPFixturePath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("apitest: mcp fixture name must be non-empty")
	}
	if strings.Contains(name, "/") || strings.Contains(name, `\`) || name == "." || name == ".." {
		return "", fmt.Errorf("apitest: mcp fixture name must be a plain file name, got %q", name)
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("apitest: cannot locate package directory")
	}
	// This file lives in <repo>/internal/apitest; MCP fixtures live
	// in <repo>/tests/fixtures/mcp/<name>.json.
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "tests", "fixtures", "mcp")
	return filepath.Join(dir, name+".json"), nil
}

// LoadMCPFixture loads and validates the MCP fixture (name), failing
// the test on any problem (missing file, bad JSON, schema violation).
func LoadMCPFixture(t *testing.T, name string) MCPFixture {
	t.Helper()
	path, err := MCPFixturePath(name)
	if err != nil {
		t.Fatalf("apitest: mcp fixture path: %v", err)
	}
	f, err := loadMCPFixtureFile(path)
	if err != nil {
		t.Fatalf("apitest: load mcp fixture %s: %v", name, err)
	}
	if f.Name != name {
		t.Fatalf("apitest: mcp fixture %s declares name=%q", name, f.Name)
	}
	return f
}

// loadMCPFixtureFile reads and validates one MCP fixture file.
func loadMCPFixtureFile(path string) (MCPFixture, error) {
	var f MCPFixture
	raw, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := validateMCPFixture(f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// mcpEnvelope is the shape validateMCPFixture decodes requests and
// recorded responses into. ID is raw so a string, number or null id
// round-trips verbatim.
type mcpEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

// validateMCPFixture enforces the required fields, the fixture label,
// and self-consistency between the request, the recorded response
// and the assertions.
func validateMCPFixture(f MCPFixture) error {
	switch {
	case f.SchemaVersion != 1:
		return fmt.Errorf("schema_version must be 1, got %d", f.SchemaVersion)
	case !f.IsFixture:
		return fmt.Errorf("fixture must be true: this file is recorded mock data, not live provider data")
	case f.Slice == "":
		return fmt.Errorf("slice is required")
	case f.Name == "":
		return fmt.Errorf("name is required")
	case f.Method == "":
		return fmt.Errorf("method is required")
	case len(f.Request) == 0:
		return fmt.Errorf("request is required")
	case len(f.RecordedResponse) == 0:
		return fmt.Errorf("recorded_response is required")
	case f.Assert.ID == nil:
		return fmt.Errorf("assert.id is required (it must equal the request id)")
	case f.Assert.ExpectError && (f.Assert.ErrorCode == nil || *f.Assert.ErrorCode == 0):
		return fmt.Errorf("assert.error_code is required when expect_error is true")
	}
	var req mcpEnvelope
	if err := json.Unmarshal(f.Request, &req); err != nil {
		return fmt.Errorf("request is not a JSON-RPC object: %v", err)
	}
	if req.JSONRPC != "2.0" {
		return fmt.Errorf("request.jsonrpc must be \"2.0\", got %q", req.JSONRPC)
	}
	if req.Method != f.Method {
		return fmt.Errorf("request.method %q does not match fixture method %q", req.Method, f.Method)
	}
	if len(req.ID) == 0 {
		return fmt.Errorf("request.id is required")
	}
	var resp mcpEnvelope
	if err := json.Unmarshal(f.RecordedResponse, &resp); err != nil {
		return fmt.Errorf("recorded_response is not a JSON-RPC object: %v", err)
	}
	if resp.JSONRPC != "2.0" {
		return fmt.Errorf("recorded_response.jsonrpc must be \"2.0\", got %q", resp.JSONRPC)
	}
	if !jsonValuesEqual(resp.ID, f.Assert.ID) {
		return fmt.Errorf("recorded_response.id %s does not match assert.id %v", resp.ID, f.Assert.ID)
	}
	if f.Assert.ExpectError && len(resp.Error) == 0 {
		return fmt.Errorf("expect_error is true but recorded_response has no error")
	}
	if !f.Assert.ExpectError && len(resp.Error) != 0 {
		return fmt.Errorf("expect_error is false but recorded_response carries an error: %s", resp.Error)
	}
	if !f.Assert.ExpectError && len(resp.Result) == 0 {
		return fmt.Errorf("expect_error is false but recorded_response has no result")
	}
	return nil
}

// runMCPExchanges drives requests through srv.Serve over in-memory
// stdio (one JSON-RPC message per line in, one response line per
// request out) and returns the response lines in order. It is the
// shared engine behind RunMCP and RunMCPSession.
func runMCPExchanges(t *testing.T, srv *mcp.Server, requests [][]byte) [][]byte {
	t.Helper()
	var in bytes.Buffer
	for _, req := range requests {
		// The stdio transport is newline-delimited: one JSON-RPC
		// message per line. A fixture file pretty-prints its
		// request over many lines, so compact it to a single line
		// before sending; the bytes are the same JSON.
		var compact bytes.Buffer
		if err := json.Compact(&compact, req); err != nil {
			t.Fatalf("apitest: mcp request is not valid JSON: %v (%s)", err, req)
		}
		in.Write(compact.Bytes())
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	if err := srv.Serve(&in, &out); err != nil {
		t.Fatalf("apitest: mcp Serve: %v", err)
	}
	trimmed := bytes.TrimSpace(out.Bytes())
	if len(trimmed) == 0 {
		return nil
	}
	return bytes.Split(trimmed, []byte("\n"))
}

// RunMCP drives one fixture exchange through srv (in-memory stdio)
// and fails the test on the first contract mismatch.
func RunMCP(t *testing.T, srv *mcp.Server, f MCPFixture) {
	t.Helper()
	responses := runMCPExchanges(t, srv, [][]byte{f.Request})
	if len(responses) != 1 {
		t.Fatalf("apitest: mcp fixture %s: server wrote %d response lines, want 1", f.Name, len(responses))
	}
	if err := CheckMCPResponse(responses[0], f); err != nil {
		t.Fatalf("apitest: mcp fixture %s: %v", f.Name, err)
	}
}

// RunMCPSession drives a sequence of fixtures through a single
// Serve session, in order, and fails the test on the first mismatch
// in any exchange. This is the realistic client flow: initialize →
// tools/list → tools/call, over one stdio session.
func RunMCPSession(t *testing.T, srv *mcp.Server, fixtures ...MCPFixture) {
	t.Helper()
	if len(fixtures) == 0 {
		t.Fatal("apitest: RunMCPSession needs at least one fixture")
	}
	requests := make([][]byte, len(fixtures))
	for i, f := range fixtures {
		requests[i] = f.Request
	}
	responses := runMCPExchanges(t, srv, requests)
	if len(responses) != len(fixtures) {
		t.Fatalf("apitest: mcp session: server wrote %d response lines, want %d", len(responses), len(fixtures))
	}
	for i, f := range fixtures {
		if err := CheckMCPResponse(responses[i], f); err != nil {
			t.Fatalf("apitest: mcp fixture %s (session step %d): %v", f.Name, i, err)
		}
	}
}

// CheckMCPResponse checks one live JSON-RPC response against the
// fixture's assertions and returns the first mismatch as an error.
// RunMCP/RunMCPSession fail the test on this error; tests use
// CheckMCPResponse directly to prove the assertion path fails on a
// wrong expectation (toggle verification).
func CheckMCPResponse(resp []byte, f MCPFixture) error {
	var decoded any
	if err := json.Unmarshal(resp, &decoded); err != nil {
		return fmt.Errorf("response is not JSON: %v (body: %.200s)", err, resp)
	}
	gotRPC, ok := jsonLookup(decoded, "jsonrpc")
	if !ok || gotRPC != "2.0" {
		return fmt.Errorf("jsonrpc: got %v, want \"2.0\" (body: %.200s)", gotRPC, resp)
	}
	gotID, ok := jsonLookup(decoded, "id")
	if !ok {
		return fmt.Errorf("response has no id (body: %.200s)", resp)
	}
	if !jsonValuesEqual(gotID, f.Assert.ID) {
		return fmt.Errorf("id: got %v, want %v", gotID, f.Assert.ID)
	}
	_, hasResult := jsonLookup(decoded, "result")
	_, hasError := jsonLookup(decoded, "error")
	if f.Assert.ExpectError {
		if !hasError {
			return fmt.Errorf("expected a JSON-RPC error, got a result (body: %.200s)", resp)
		}
		if hasResult {
			return fmt.Errorf("error response also carries a result (body: %.200s)", resp)
		}
		gotCode, ok := jsonLookup(decoded, "error.code")
		if !ok {
			return fmt.Errorf("error response has no error.code (body: %.200s)", resp)
		}
		if f.Assert.ErrorCode != nil && !jsonValuesEqual(gotCode, *f.Assert.ErrorCode) {
			return fmt.Errorf("error.code: got %v, want %d", gotCode, *f.Assert.ErrorCode)
		}
	} else {
		if hasError {
			return fmt.Errorf("expected a result, got a JSON-RPC error (body: %.200s)", resp)
		}
		if !hasResult {
			return fmt.Errorf("response has no result (body: %.200s)", resp)
		}
	}
	for _, path := range f.Assert.RequiredFields {
		if _, ok := jsonLookup(decoded, path); !ok {
			return fmt.Errorf("required field %q missing from response", path)
		}
	}
	for path, want := range f.Assert.JSONPaths {
		got, ok := jsonLookup(decoded, path)
		if !ok {
			return fmt.Errorf("json path %q missing from response", path)
		}
		if want == nil {
			continue // presence-only assertion
		}
		if !jsonValuesEqual(got, want) {
			return fmt.Errorf("json path %q: got %v, want %v", path, got, want)
		}
	}
	for path, want := range f.Assert.JSONLengths {
		got, ok := jsonLookup(decoded, path)
		if !ok {
			return fmt.Errorf("json path %q missing from response", path)
		}
		arr, ok := got.([]any)
		if !ok {
			return fmt.Errorf("json path %q: got %T, want an array of length %d", path, got, want)
		}
		if len(arr) != want {
			return fmt.Errorf("json path %q: got array length %d, want %d", path, len(arr), want)
		}
	}
	return nil
}
