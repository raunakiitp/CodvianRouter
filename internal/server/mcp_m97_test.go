// M9.7 dashboard MCP feeds: tool list with scopes, transport
// status toggles, and the paginated audit view. All tests go
// through ServeHTTP (doReq), so they prove the routes are WIRED
// and live-served under the guard table, not just declared.
package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/mcp"
)

func mcpTestAuditLog(t *testing.T) *mcp.AuditLog {
	t.Helper()
	log, err := mcp.OpenAuditLog(filepath.Join(t.TempDir(), "mcp", "audit.jsonl"))
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}
	return log
}

func mcpRecord(t *testing.T, log *mcp.AuditLog, tool, decision string, args map[string]any) {
	t.Helper()
	entry := mcp.AuditEntry{
		Tool: tool, CallerKind: mcp.KindLocalOperator, CallerID: "local-operator",
		Decision:  mcp.AuditDecision(decision),
		InputHash: mcp.HashArguments(args),
	}
	if decision == "allow" {
		entry.OutputSummary = mcp.SummarizeOutput(map[string]any{"ok": true})
	} else {
		entry.ErrorCode = mcp.ScopeDeniedCode
		entry.Reason = "scope denied"
	}
	if err := log.Record(entry); err != nil {
		t.Fatalf("Record %s: %v", tool, err)
	}
}

func TestHandleMCPToolsScopes(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/mcp", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/mcp status = %d, want 200", rec.Code)
	}
	var out mcpResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	// Every tool in the feed must carry exactly the scopes the
	// M9.1 scope table assigns it — the feed and the table can
	// never drift apart.
	if len(out.Tools) != len(mcp.Tools()) {
		t.Fatalf("tools = %d, want %d (mcp.Tools())", len(out.Tools), len(mcp.Tools()))
	}
	for _, ft := range out.Tools {
		want, known := mcp.RequiredScopes(ft.Name)
		if !known {
			t.Errorf("tool %q has no scope assignment", ft.Name)
			continue
		}
		if len(ft.Scopes) != len(want) {
			t.Errorf("tool %q scopes = %v, want %v", ft.Name, ft.Scopes, want)
			continue
		}
		for i := range want {
			if ft.Scopes[i] != want[i] {
				t.Errorf("tool %q scopes = %v, want %v", ft.Name, ft.Scopes, want)
			}
		}
		if len(ft.Scopes) == 0 {
			t.Errorf("tool %q has an empty scope list (a tool must never be scopeless)", ft.Name)
		}
	}
}

func TestHandleMCPStatusShape(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	// Unauthenticated GET: the status feed is AuthExempt (flags
	// and counts only, like the other dashboard GET feeds).
	rec := doReq(t, srv, http.MethodGet, "/api/mcp/status", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/mcp/status status = %d, want 200", rec.Code)
	}
	// Exact M9.2 shape: {"sse":{"enabled":bool,"sessions":n},
	// "streamable":{"enabled":bool,"sessions":n}} — and nothing else.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("status keys = %v, want exactly [sse streamable]", keysOf(raw))
	}
	var out mcpStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status shape: %v", err)
	}
	// Both transports default OFF (fail-closed) with no sessions
	// (no sessions are open yet — counts are live, see
	// TestMCPHTTPStatusLiveSessionCounts).
	if out.SSE.Enabled || out.Streamable.Enabled {
		t.Errorf("default status = sse %v streamable %v, want both disabled", out.SSE.Enabled, out.Streamable.Enabled)
	}
	if out.SSE.Sessions != 0 || out.Streamable.Sessions != 0 {
		t.Errorf("default sessions = sse %d streamable %d, want 0/0", out.SSE.Sessions, out.Streamable.Sessions)
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestHandleMCPStatusToggle(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	get := func() mcpStatusResponse {
		t.Helper()
		rec := doReq(t, srv, http.MethodGet, "/api/mcp/status", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/mcp/status status = %d, want 200", rec.Code)
		}
		var out mcpStatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("status shape: %v", err)
		}
		return out
	}
	// Unauthenticated POST is refused (admin scope required).
	rec := doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"sse","enabled":true}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST status = %d, want 401", rec.Code)
	}
	if got := get(); got.SSE.Enabled {
		t.Error("unauthenticated POST flipped the toggle")
	}
	// Enable SSE with the test key; GET reflects it.
	rec = doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"sse","enabled":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST enable sse status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := get(); !got.SSE.Enabled || got.Streamable.Enabled {
		t.Errorf("after enabling sse: sse %v streamable %v, want true/false", got.SSE.Enabled, got.Streamable.Enabled)
	}
	// Enable streamable, then disable SSE again.
	rec = doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"streamable","enabled":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST enable streamable status = %d, want 200", rec.Code)
	}
	rec = doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"sse","enabled":false}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST disable sse status = %d, want 200", rec.Code)
	}
	if got := get(); got.SSE.Enabled || !got.Streamable.Enabled {
		t.Errorf("after flip: sse %v streamable %v, want false/true", got.SSE.Enabled, got.Streamable.Enabled)
	}
	// Unknown transport and malformed JSON are 400, never a
	// silent no-op.
	rec = doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"grpc","enabled":true}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST unknown transport status = %d, want 400", rec.Code)
	}
	rec = doReq(t, srv, http.MethodPost, "/api/mcp/status", `{not json`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST malformed JSON status = %d, want 400", rec.Code)
	}
	if got := get(); got.SSE.Enabled || !got.Streamable.Enabled {
		t.Errorf("bad POSTs changed state: sse %v streamable %v, want false/true", got.SSE.Enabled, got.Streamable.Enabled)
	}
}

func TestHandleMCPStatusMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodDelete, "/api/mcp/status", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/mcp/status status = %d, want 405", rec.Code)
	}
}

func TestHandleMCPAuditFeed(t *testing.T) {
	log := mcpTestAuditLog(t)
	secret := "sk-live-SECRET-DO-NOT-LEAK"
	mcpRecord(t, log, "router_status", "allow", map[string]any{"note": secret})
	mcpRecord(t, log, "provider_list", "deny", map[string]any{"n": 1})
	srv := newTestServer(t, &fakeEngine{}, WithMCPAuditLog(log))
	rec := doReq(t, srv, http.MethodGet, "/api/mcp/audit", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/mcp/audit status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The raw secret must appear NOWHERE in the feed body.
	if strings.Contains(body, secret) || strings.Contains(body, "sk-live") {
		t.Fatalf("audit feed leaks raw input: %s", body)
	}
	var out mcpAuditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if !out.Available || out.Total != 2 || len(out.Entries) != 2 {
		t.Fatalf("available/total/entries = %v/%d/%d, want true/2/2", out.Available, out.Total, len(out.Entries))
	}
	if out.Cap != mcp.MaxAuditEntries {
		t.Errorf("cap = %d, want MaxAuditEntries %d", out.Cap, mcp.MaxAuditEntries)
	}
	if out.Dropped != 0 {
		t.Errorf("dropped = %d, want 0 (no rotation yet)", out.Dropped)
	}
	// Newest first: the deny (recorded second) comes first.
	if out.Entries[0].Tool != "provider_list" || out.Entries[1].Tool != "router_status" {
		t.Errorf("entries order = %q, %q, want provider_list, router_status (newest first)",
			out.Entries[0].Tool, out.Entries[1].Tool)
	}
	// Redaction as stored: input is a hash, output truncated.
	allow := out.Entries[1]
	if allow.InputHash == "" || len(allow.InputHash) != 64 {
		t.Errorf("allow entry input_hash = %q, want a 64-hex SHA-256", allow.InputHash)
	}
	if allow.InputHash != mcp.HashArguments(map[string]any{"note": secret}) {
		t.Errorf("allow entry input_hash does not match HashArguments of the recorded args")
	}
	if len([]rune(allow.OutputSummary)) > mcp.MaxOutputSummaryChars {
		t.Errorf("output_summary is %d runes, want <= %d", len([]rune(allow.OutputSummary)), mcp.MaxOutputSummaryChars)
	}
	deny := out.Entries[0]
	if deny.Decision != mcp.AuditDeny || deny.ErrorCode != mcp.ScopeDeniedCode {
		t.Errorf("deny entry decision/code = %v/%d, want deny/%d", deny.Decision, deny.ErrorCode, mcp.ScopeDeniedCode)
	}
}

func TestHandleMCPAuditPagination(t *testing.T) {
	log := mcpTestAuditLog(t)
	for i := 1; i <= 5; i++ {
		mcpRecord(t, log, "router_status", "allow", map[string]any{"n": i})
	}
	srv := newTestServer(t, &fakeEngine{}, WithMCPAuditLog(log))
	rec := doReq(t, srv, http.MethodGet, "/api/mcp/audit?limit=2&offset=2", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/mcp/audit?limit=2&offset=2 status = %d, want 200", rec.Code)
	}
	var out mcpAuditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if out.Total != 5 || len(out.Entries) != 2 || out.Limit != 2 || out.Offset != 2 {
		t.Fatalf("total/entries/limit/offset = %d/%d/%d/%d, want 5/2/2/2", out.Total, len(out.Entries), out.Limit, out.Offset)
	}
	// Offset past the end is an empty page, not an error.
	rec = doReq(t, srv, http.MethodGet, "/api/mcp/audit?limit=50&offset=99", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET offset past end status = %d, want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if out.Total != 5 || len(out.Entries) != 0 {
		t.Fatalf("offset past end: total/entries = %d/%d, want 5/0", out.Total, len(out.Entries))
	}
}

func TestHandleMCPAuditUnavailable(t *testing.T) {
	// No audit log wired: honest empty state, not an error.
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/api/mcp/audit", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/mcp/audit status = %d, want 200", rec.Code)
	}
	var out mcpAuditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if out.Available || out.Total != 0 || len(out.Entries) != 0 {
		t.Fatalf("unwired audit: available/total/entries = %v/%d/%d, want false/0/0", out.Available, out.Total, len(out.Entries))
	}
	if out.Cap != mcp.MaxAuditEntries {
		t.Errorf("unwired audit cap = %d, want MaxAuditEntries %d", out.Cap, mcp.MaxAuditEntries)
	}
}

func TestHandleMCPAuditMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodPost, "/api/mcp/audit", "{}", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/mcp/audit status = %d, want 405", rec.Code)
	}
}

func TestDashboardMCPSectionRenders(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="view-mcp"`,
		"Remote transports",
		"Audit log",
		"Required scopes",
		`id="mcpAuditBody"`,
		`id="mcpSSEToggle"`,
		`id="mcpStreamToggle"`,
		`id="mcpAuditPrev"`,
		`id="mcpAuditNext"`,
		"/api/mcp/status",
		"/api/mcp/audit",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard MCP section missing %q", want)
		}
	}
}
