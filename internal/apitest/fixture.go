// Package apitest is the contract-test harness for M8 (API breadth).
//
// Fixture schema (version 1). A fixture is a JSON file under
// tests/fixtures/v1/<slice>/<name>.json — one recorded request/response
// pair plus the assertions the contract test must check. Every other M8
// slice's Definition of Done depends on this schema: do not change it
// without updating docs/API_FIXTURES.md and every fixture in the tree.
//
// A fixture is RECORDED MOCK DATA, never live provider data. The
// "recorded_response" is what a mock upstream returned when the fixture
// was recorded (or a hand-written canned response in the same shape);
// "provider" names the mock upstream (e.g. "mock-upstream"), never a
// real provider account, and "model" names the mock model. Unknown
// prices stay $0/unknown — fixtures must never invent a price, a
// capability, or a provider behaviour. A file with "fixture": false (or
// a missing "fixture" field) is rejected by the loader.
//
// Required fields: schema_version (= 1), slice, name, fixture (= true),
// method, path, provider, model, recorded_response.status, assert.status.
// Optional: recorded_from (how/when the response was recorded), request
// (headers, body), recorded_response.headers/body, assert.required_fields,
// assert.json_paths, notes.
//
// Assertions: assert.status is the expected HTTP status. Each entry of
// assert.json_paths is a dotted path into the JSON response body
// (numeric segments index arrays, e.g. "choices.0.message.role") with
// the expected value; a null expected value means "the field must exist,
// any value". assert.required_fields lists dotted paths that must exist.
// Numbers compare by JSON value, so 200 and 200.0 are equal.
//
// Usage (from a _test.go file in any package):
//
//	f := apitest.LoadFixture(t, "m8.2", "chat-completions-200")
//	apitest.Run(t, handlerUnderTest, f)
//
// Run serves handlerUnderTest on an httptest server, sends the
// fixture's request, and fails the test on the first mismatch (status,
// required field, or JSON path). The test itself wires the mock
// upstream the handler talks to — typically an httptest server whose
// handler replays f.RecordedResponse — and passes the handler under
// test, not the mock, to Run.
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
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Fixture is one recorded request/response pair plus its assertions.
// See the package doc comment and docs/API_FIXTURES.md for the schema.
type Fixture struct {
	SchemaVersion int    `json:"schema_version"`
	Slice         string `json:"slice"`
	Name          string `json:"name"`
	// IsFixture must be true. It labels the file as recorded mock data,
	// never live provider data. The loader rejects anything else.
	IsFixture bool `json:"fixture"`
	// RecordedFrom notes how the recorded response was produced, e.g.
	// "mock-upstream: canned OpenAI-shaped response, 2026-11-19".
	RecordedFrom     string          `json:"recorded_from,omitempty"`
	Method           string          `json:"method"`
	Path             string          `json:"path"`
	Request          FixtureRequest  `json:"request,omitempty"`
	Provider         string          `json:"provider"`
	Model            string          `json:"model"`
	RecordedResponse FixtureResponse `json:"recorded_response"`
	Assert           FixtureAssert   `json:"assert"`
	Notes            string          `json:"notes,omitempty"`
}

// FixtureRequest is the request the contract test sends.
type FixtureRequest struct {
	Headers map[string]string `json:"headers,omitempty"`
	Body    any               `json:"body,omitempty"`
}

// FixtureResponse is the response the mock upstream returned when the
// fixture was recorded. Tests replay it from their mock upstream.
type FixtureResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    any               `json:"body,omitempty"`
}

// FixtureAssert declares what the contract test checks about the
// response from the handler under test.
type FixtureAssert struct {
	Status int `json:"status"`
	// RequiredFields are dotted JSON paths that must exist in the
	// response body (numeric segments index arrays).
	RequiredFields []string `json:"required_fields,omitempty"`
	// JSONPaths maps a dotted JSON path to its expected value. A null
	// expected value means "must exist, any value".
	JSONPaths map[string]any `json:"json_paths,omitempty"`
}

// FixturePath resolves the on-disk path of the fixture (slice, name).
// Slice and name must be plain file names: no separators, no "..".
func FixturePath(slice, name string) (string, error) {
	if slice == "" || name == "" {
		return "", fmt.Errorf("apitest: slice and name must both be non-empty")
	}
	for _, part := range []string{slice, name} {
		if strings.Contains(part, "/") || strings.Contains(part, `\`) || part == "." || part == ".." {
			return "", fmt.Errorf("apitest: slice/name must be plain file names, got %q", part)
		}
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("apitest: cannot locate package directory")
	}
	// This file lives in <repo>/internal/apitest; fixtures live in
	// <repo>/tests/fixtures/v1/<slice>/<name>.json.
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "tests", "fixtures", "v1", slice)
	return filepath.Join(dir, name+".json"), nil
}

// LoadFixture loads and validates the fixture (slice, name), failing
// the test on any problem (missing file, bad JSON, schema violation).
func LoadFixture(t *testing.T, slice, name string) Fixture {
	t.Helper()
	path, err := FixturePath(slice, name)
	if err != nil {
		t.Fatalf("apitest: fixture path: %v", err)
	}
	f, err := loadFixtureFile(path)
	if err != nil {
		t.Fatalf("apitest: load fixture %s/%s: %v", slice, name, err)
	}
	if err := checkFixtureIdentity(f, slice, name); err != nil {
		t.Fatalf("%v", err)
	}
	return f
}

// checkFixtureIdentity verifies a loaded fixture declares the slice
// and name it was requested as. It is a separate function (rather
// than inline in LoadFixture) so the check is unit-testable: a
// t.Fatalf inside LoadFixture cannot be observed by a test.
func checkFixtureIdentity(f Fixture, slice, name string) error {
	if f.Slice != slice || f.Name != name {
		return fmt.Errorf("apitest: fixture %s/%s declares slice=%q name=%q", slice, name, f.Slice, f.Name)
	}
	return nil
}

// loadFixtureFile reads and validates one fixture file.
func loadFixtureFile(path string) (Fixture, error) {
	var f Fixture
	raw, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := validateFixture(f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// validateFixture enforces the required fields and the fixture label.
func validateFixture(f Fixture) error {
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
	case f.Path == "" || !strings.HasPrefix(f.Path, "/"):
		return fmt.Errorf("path is required and must start with /, got %q", f.Path)
	case f.Provider == "":
		return fmt.Errorf("provider is required (name the mock upstream, never a real provider account)")
	case f.Model == "":
		return fmt.Errorf("model is required (name the mock model)")
	case f.RecordedResponse.Status < 100 || f.RecordedResponse.Status > 599:
		return fmt.Errorf("recorded_response.status must be a valid HTTP status, got %d", f.RecordedResponse.Status)
	case f.Assert.Status < 100 || f.Assert.Status > 599:
		return fmt.Errorf("assert.status must be a valid HTTP status, got %d", f.Assert.Status)
	}
	return nil
}

// Run serves handler on an httptest server, sends the fixture's
// request, and fails the test on the first contract mismatch. The
// handler under test is expected to talk to a mock upstream wired by
// the test (typically replaying f.RecordedResponse).
func Run(t *testing.T, handler http.Handler, f Fixture) {
	t.Helper()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	var body io.Reader
	if f.Request.Body != nil {
		raw, err := json.Marshal(f.Request.Body)
		if err != nil {
			t.Fatalf("apitest: marshal fixture request body: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(f.Method, srv.URL+f.Path, body)
	if err != nil {
		t.Fatalf("apitest: build request: %v", err)
	}
	for k, v := range f.Request.Headers {
		req.Header.Set(k, v)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("apitest: %s %s: %v", f.Method, f.Path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("apitest: read response body: %v", err)
	}
	if err := CheckResponse(resp, respBody, f); err != nil {
		t.Fatalf("apitest: fixture %s/%s: %v", f.Slice, f.Name, err)
	}
}

// CheckResponse checks one response against the fixture's assertions
// and returns the first mismatch as an error. Run fails the test on
// this error; tests use CheckResponse directly to prove the assertion
// path fails on a wrong expectation (toggle verification).
func CheckResponse(resp *http.Response, body []byte, f Fixture) error {
	if resp.StatusCode != f.Assert.Status {
		return fmt.Errorf("status: got %d, want %d (body: %.200s)", resp.StatusCode, f.Assert.Status, body)
	}
	if len(f.Assert.RequiredFields) == 0 && len(f.Assert.JSONPaths) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return fmt.Errorf("response body is not JSON: %v (body: %.200s)", err, body)
	}
	for _, path := range f.Assert.RequiredFields {
		if _, ok := jsonLookup(decoded, path); !ok {
			return fmt.Errorf("required field %q missing from response body", path)
		}
	}
	for path, want := range f.Assert.JSONPaths {
		got, ok := jsonLookup(decoded, path)
		if !ok {
			return fmt.Errorf("json path %q missing from response body", path)
		}
		if want == nil {
			continue // presence-only assertion
		}
		if !jsonValuesEqual(got, want) {
			return fmt.Errorf("json path %q: got %v, want %v", path, got, want)
		}
	}
	return nil
}

// jsonLookup resolves a dotted path into decoded JSON. Numeric
// segments index arrays; other segments index objects.
func jsonLookup(v any, path string) (any, bool) {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// jsonValuesEqual compares two JSON values after a marshal/unmarshal
// round-trip, so 200 (int) and 200.0 (float64) compare equal.
func jsonValuesEqual(a, b any) bool {
	na, err := normalizeJSONValue(a)
	if err != nil {
		return false
	}
	nb, err := normalizeJSONValue(b)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(na, nb)
}

func normalizeJSONValue(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
