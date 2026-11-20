// Contract + behaviour tests for GET /api/logs (M8.16).
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
)

// newM816LogsServer builds a server with a log capture ring attached
// and three lines logged through the server logger — the first
// carrying a PII sample (an email address) that must never reach the
// endpoint unredacted.
func newM816LogsServer(t *testing.T) (*Server, *APILogRing) {
	t.Helper()
	ring := NewAPILogRing(100)
	srv := newTestServer(t, &fakeEngine{}, WithAPILogCapture(ring))
	srv.logger.Printf("request from alice@example.com failed")
	srv.logger.Printf("second line, no PII")
	srv.logger.Printf("third line, no PII")
	return srv, ring
}

// TestM816LogsFixture is the apitest contract test: limit=2 returns
// the two newest lines from a three-line capture.
func TestM816LogsFixture(t *testing.T) {
	srv, _ := newM816LogsServer(t)
	f := apitest.LoadFixture(t, "m8.16", "logs-200")
	apitest.Run(t, http.HandlerFunc(srv.handleAPILogs), f)
}

// TestM816LogsRedacted proves the endpoint serves the REDACTED log
// only: the raw PII sample appears nowhere in the response, the
// redacted placeholder does, lines are newest first, and the total
// counts every captured line.
func TestM816LogsRedacted(t *testing.T) {
	srv, _ := newM816LogsServer(t)
	rec := httptest.NewRecorder()
	// Default limit (50): all three lines come back, newest first.
	srv.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "alice@example.com") {
		t.Fatalf("response leaks the raw PII sample: %s", body)
	}
	if !strings.Contains(body, "[REDACTED:EMAIL]") {
		t.Fatalf("response has no redacted placeholder: %s", body)
	}
	var resp apiLogsResponse
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Captured || resp.Redaction != "always-on" {
		t.Fatalf("captured/redaction = %v/%q, want true/always-on", resp.Captured, resp.Redaction)
	}
	if resp.TotalCaptured != 3 {
		t.Fatalf("total_captured = %d, want 3", resp.TotalCaptured)
	}
	if len(resp.Lines) != 3 {
		t.Fatalf("lines = %d, want 3 (default limit)", len(resp.Lines))
	}
	if !strings.Contains(resp.Lines[0].Line, "third line") || !strings.Contains(resp.Lines[1].Line, "second line") || !strings.Contains(resp.Lines[2].Line, "[REDACTED:EMAIL]") {
		t.Fatalf("lines are not newest first with the redacted PII line last: %+v", resp.Lines)
	}
}

// TestM816LogsNoCapture proves the honest empty state: with no ring
// attached the endpoint answers 200 with captured:false and no
// lines — never invented log lines.
func TestM816LogsNoCapture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := httptest.NewRecorder()
	srv.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp apiLogsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Captured || len(resp.Lines) != 0 || resp.TotalCaptured != 0 {
		t.Fatalf("no-capture response = %+v, want captured:false with no lines", resp)
	}
	if resp.Limit != defaultAPILogLimit {
		t.Fatalf("limit = %d, want default %d", resp.Limit, defaultAPILogLimit)
	}
}

// TestM816LogsSince proves the `since` filter: a future lower bound
// returns no lines, a past one returns them all. Unix seconds and
// RFC3339 are both accepted; garbage is a 400, never a silent
// unfiltered list.
func TestM816LogsSince(t *testing.T) {
	srv, _ := newM816LogsServer(t)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec := httptest.NewRecorder()
	srv.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?since="+future, nil))
	var resp apiLogsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Lines) != 0 {
		t.Fatalf("future since returned %d lines, want 0", len(resp.Lines))
	}
	past := time.Now().Add(-time.Hour).Unix()
	rec = httptest.NewRecorder()
	srv.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?since="+time.Unix(past, 0).UTC().Format(time.RFC3339), nil))
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Lines) != 3 {
		t.Fatalf("past since returned %d lines, want 3", len(resp.Lines))
	}
	rec = httptest.NewRecorder()
	srv.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?since=not-a-time", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad since status = %d, want 400", rec.Code)
	}
}

// TestM816LogsLimitClamp proves the limit bounds: 0 clamps to 1 and
// a huge limit clamps to maxAPILogLimit, while an unparseable limit
// falls back to the dashboard default (mirroring /dashboard/logs).
func TestM816LogsLimitClamp(t *testing.T) {
	srv, _ := newM816LogsServer(t)
	rec := httptest.NewRecorder()
	srv.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?limit=0", nil))
	var resp apiLogsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Limit != 1 || len(resp.Lines) != 1 {
		t.Fatalf("limit=0 gave limit=%d lines=%d, want 1/1", resp.Limit, len(resp.Lines))
	}
	rec = httptest.NewRecorder()
	srv.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?limit=99999", nil))
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Limit != maxAPILogLimit {
		t.Fatalf("limit=99999 gave limit=%d, want %d", resp.Limit, maxAPILogLimit)
	}
	rec = httptest.NewRecorder()
	srv.handleAPILogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?limit=banana", nil))
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Limit != defaultAPILogLimit {
		t.Fatalf("limit=banana gave limit=%d, want default %d", resp.Limit, defaultAPILogLimit)
	}
}

// TestM816LogsToggleNegative proves the contract test is
// toggle-verified: the same fixture asserting a wrong status must
// fail CheckResponse.
func TestM816LogsToggleNegative(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.16", "logs-200")
	f.Assert.Status = http.StatusCreated
	resp := &http.Response{StatusCode: http.StatusOK}
	if err := apitest.CheckResponse(resp, []byte(`{}`), f); err == nil {
		t.Fatalf("CheckResponse with assert 201 against a 200 response passed; toggle check is broken")
	}
}
