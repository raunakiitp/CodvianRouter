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
	"strings"
	"testing"
)

// mockUpstream replays the fixture's recorded response verbatim: the
// canned stand-in for a provider. It never calls a real provider.
func mockUpstream(t *testing.T, recorded FixtureResponse) *httptest.Server {
	t.Helper()
	raw, err := json.Marshal(recorded.Body)
	if err != nil {
		t.Fatalf("marshal recorded response: %v", err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range recorded.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(recorded.Status)
		_, _ = w.Write(raw)
	}))
}

// demoProxy is a TEST-ONLY forwarding handler (not production code):
// it forwards the request body to the mock upstream and copies the
// upstream status/body back. It exists so the harness's Run path can
// be exercised end to end without the real server's config.
func demoProxy(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadGateway)
			return
		}
		upReq, err := http.NewRequest(http.MethodPost, upstreamURL+r.URL.Path, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "build upstream request", http.StatusBadGateway)
			return
		}
		upReq.Header.Set("Content-Type", "application/json")
		upResp, err := http.DefaultClient.Do(upReq)
		if err != nil {
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
			return
		}
		defer upResp.Body.Close()
		upBody, err := io.ReadAll(upResp.Body)
		if err != nil {
			http.Error(w, "read upstream", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upResp.StatusCode)
		_, _ = w.Write(upBody)
	})
}

func TestLoadFixtureExample(t *testing.T) {
	f := LoadFixture(t, "m8.0", "chat-completions-200")
	if f.SchemaVersion != 1 || !f.IsFixture {
		t.Fatalf("example fixture: schema_version=%d fixture=%v", f.SchemaVersion, f.IsFixture)
	}
	if f.Method != "POST" || f.Path != "/v1/chat/completions" {
		t.Fatalf("example fixture: method=%q path=%q", f.Method, f.Path)
	}
	if f.Provider != "mock-upstream" || f.Model != "mock-model" {
		t.Fatalf("example fixture: provider=%q model=%q", f.Provider, f.Model)
	}
	if f.RecordedResponse.Status != 200 || f.Assert.Status != 200 {
		t.Fatalf("example fixture: recorded=%d assert=%d", f.RecordedResponse.Status, f.Assert.Status)
	}
}

func TestRunChatCompletions200(t *testing.T) {
	f := LoadFixture(t, "m8.0", "chat-completions-200")
	upstream := mockUpstream(t, f.RecordedResponse)
	defer upstream.Close()
	Run(t, demoProxy(t, upstream.URL), f)
}

// TestCheckResponseWrongStatusFails is the toggle proof for the
// harness's core assertion path: the same 200 response must FAIL
// when the fixture expects a different status.
func TestCheckResponseWrongStatusFails(t *testing.T) {
	f := LoadFixture(t, "m8.0", "chat-completions-200")
	f.Assert.Status = 201 // wrong on purpose: the response is 200
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	body, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded body: %v", err)
	}
	if err := CheckResponse(resp, body, f); err == nil {
		t.Fatal("CheckResponse accepted a 200 response against assert.status 201")
	} else if !strings.Contains(err.Error(), "status") {
		t.Fatalf("wrong-status error should name status, got: %v", err)
	}
}

func TestCheckResponseWrongJSONPathFails(t *testing.T) {
	f := LoadFixture(t, "m8.0", "chat-completions-200")
	f.Assert.JSONPaths["choices.0.message.role"] = "system" // wrong on purpose
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	body, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded body: %v", err)
	}
	if err := CheckResponse(resp, body, f); err == nil {
		t.Fatal("CheckResponse accepted a wrong choices.0.message.role")
	}
}

func TestCheckResponseMissingRequiredFieldFails(t *testing.T) {
	f := LoadFixture(t, "m8.0", "chat-completions-200")
	f.Assert.RequiredFields = []string{"nonexistent_field"}
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	body, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded body: %v", err)
	}
	if err := CheckResponse(resp, body, f); err == nil {
		t.Fatal("CheckResponse accepted a response missing a required field")
	}
}

func TestCheckResponsePresenceOnlyNullPasses(t *testing.T) {
	f := LoadFixture(t, "m8.0", "chat-completions-200")
	f.Assert.JSONPaths = map[string]any{"choices.0.message.content": nil} // must exist, any value
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	body, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded body: %v", err)
	}
	if err := CheckResponse(resp, body, f); err != nil {
		t.Fatalf("presence-only null assertion should pass: %v", err)
	}
}

func TestCheckResponseNonJSONBodyFails(t *testing.T) {
	f := LoadFixture(t, "m8.0", "chat-completions-200")
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	if err := CheckResponse(resp, []byte("not json{"), f); err == nil {
		t.Fatal("CheckResponse accepted a non-JSON body with JSON assertions")
	}
}

func TestLoadFixtureRejectsNonFixture(t *testing.T) {
	dir := t.TempDir()
	// "fixture": false must be rejected: the file is not labelled
	// recorded mock data.
	raw := `{"schema_version":1,"slice":"m8.0","name":"bad","fixture":false,` +
		`"method":"GET","path":"/v1/models","provider":"mock-upstream","model":"mock-model",` +
		`"recorded_response":{"status":200},"assert":{"status":200}}`
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write bad fixture: %v", err)
	}
	if _, err := loadFixtureFile(path); err == nil {
		t.Fatal("loader accepted a fixture with \"fixture\": false")
	}
}

func TestLoadFixtureRejectsBadSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	raw := `{"schema_version":2,"slice":"m8.0","name":"bad","fixture":true,` +
		`"method":"GET","path":"/v1/models","provider":"mock-upstream","model":"mock-model",` +
		`"recorded_response":{"status":200},"assert":{"status":200}}`
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write bad fixture: %v", err)
	}
	if _, err := loadFixtureFile(path); err == nil {
		t.Fatal("loader accepted schema_version 2")
	}
}

// writeFixtureFile writes raw JSON to a temp file and returns its path.
func writeFixtureFile(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write bad fixture: %v", err)
	}
	return path
}

// TestLoadFixtureRejectsUnknownField: DisallowUnknownFields must
// reject a fixture carrying a field the schema does not define.
func TestLoadFixtureRejectsUnknownField(t *testing.T) {
	raw := `{"schema_version":1,"slice":"m8.0","name":"bad","fixture":true,` +
		`"method":"GET","path":"/v1/models","provider":"mock-upstream","model":"mock-model",` +
		`"recorded_response":{"status":200},"assert":{"status":200},"bogus_field":1}`
	if _, err := loadFixtureFile(writeFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted a fixture with an unknown field")
	}
}

// TestLoadFixtureRejectsMissingRequiredField: a fixture missing a
// required field (method here) must fail validation.
func TestLoadFixtureRejectsMissingRequiredField(t *testing.T) {
	raw := `{"schema_version":1,"slice":"m8.0","name":"bad","fixture":true,` +
		`"path":"/v1/models","provider":"mock-upstream","model":"mock-model",` +
		`"recorded_response":{"status":200},"assert":{"status":200}}`
	if _, err := loadFixtureFile(writeFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted a fixture with no method")
	}
}

// TestCheckFixtureIdentityMismatch: the slice/name check LoadFixture
// applies must reject both kinds of mismatch and accept a match.
func TestCheckFixtureIdentityMismatch(t *testing.T) {
	f := Fixture{Slice: "m8.0", Name: "chat-completions-200"}
	if err := checkFixtureIdentity(f, "m8.2", "chat-completions-200"); err == nil {
		t.Fatal("checkFixtureIdentity accepted a slice mismatch")
	}
	if err := checkFixtureIdentity(f, "m8.0", "other-name"); err == nil {
		t.Fatal("checkFixtureIdentity accepted a name mismatch")
	}
	if err := checkFixtureIdentity(f, "m8.0", "chat-completions-200"); err != nil {
		t.Fatalf("checkFixtureIdentity rejected a matching fixture: %v", err)
	}
}

// TestLoadFixtureRejectsBadStatus: recorded/assert statuses outside
// 100–599 must fail validation.
func TestLoadFixtureRejectsBadStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded int
		assert   int
	}{
		{"recorded 99", 99, 200},
		{"recorded 600", 600, 200},
		{"assert 99", 200, 99},
		{"assert 600", 200, 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"schema_version":1,"slice":"m8.0","name":"bad","fixture":true,`+
				`"method":"GET","path":"/v1/models","provider":"mock-upstream","model":"mock-model",`+
				`"recorded_response":{"status":%d},"assert":{"status":%d}}`, tc.recorded, tc.assert)
			if _, err := loadFixtureFile(writeFixtureFile(t, raw)); err == nil {
				t.Fatalf("loader accepted recorded=%d assert=%d", tc.recorded, tc.assert)
			}
		})
	}
}

func TestLoadFixtureMissingFileErrors(t *testing.T) {
	if _, err := loadFixtureFile(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatal("loader accepted a missing fixture file")
	}
}

func TestFixturePathRejectsTraversal(t *testing.T) {
	for _, bad := range [][2]string{{"..", "x"}, {"m8.0", "../x"}, {"a/b", "x"}} {
		if _, err := FixturePath(bad[0], bad[1]); err == nil {
			t.Fatalf("FixturePath accepted slice=%q name=%q", bad[0], bad[1])
		}
	}
}

func TestJSONLookup(t *testing.T) {
	var v any
	if err := json.Unmarshal([]byte(`{"a":{"b":[{"c":42}]}}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := jsonLookup(v, "a.b.0.c")
	if !ok || got != float64(42) {
		t.Fatalf("jsonLookup a.b.0.c: got %v, ok=%v", got, ok)
	}
	if _, ok := jsonLookup(v, "a.b.1.c"); ok {
		t.Fatal("jsonLookup accepted an out-of-range index")
	}
	if _, ok := jsonLookup(v, "a.missing"); ok {
		t.Fatal("jsonLookup accepted a missing key")
	}
}
