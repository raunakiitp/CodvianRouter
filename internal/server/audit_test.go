// M10 B4: GET /dashboard/audit — audit feed tests.
//
// The route is NOT registered yet (the coordinator owns
// registration + the guard-table row — see auditGuardRoute in
// audit.go), so these tests call s.handleDashboardAudit directly
// instead of going through ServeHTTP; a 404 from ServeHTTP here
// would only prove the coordinator has not landed the
// registration yet, not anything about the handler. Once the
// route is registered, TestAuditGuardRow pins the exact row the
// coordinator must have landed.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/mcp"
)

// getAudit calls the handler directly (see file doc) and decodes
// the feed. The secret-looking values used across tests are
// sk-shaped on purpose: guardrails.RedactLogLine must catch them
// (openai-key rule), so a leak would be a real redaction failure,
// not a pattern the redactor was never asked to catch.
func getAudit(t *testing.T, srv *Server, query string) (int, string, auditResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/audit"+query, nil)
	rec := httptest.NewRecorder()
	srv.handleDashboardAudit(rec, req)
	var out auditResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("audit response is not JSON: %v: %s", err, rec.Body.String())
		}
	}
	return rec.Code, rec.Body.String(), out
}

func TestAuditFeedShape(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	code, _, out := getAudit(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// All four categories are present even with nothing wired.
	if !out.GuardDenials.Available {
		t.Error("guard_denials.available = false, want true (live counters exist even at zero)")
	}
	if out.GuardDenials.Total != 0 || out.GuardDenials.LocalOnlyDenials != 0 {
		t.Errorf("guard_denials total/local_only = %d/%d, want 0/0 on a fresh server",
			out.GuardDenials.Total, out.GuardDenials.LocalOnlyDenials)
	}
	if len(out.GuardDenials.Entries) != 0 {
		t.Errorf("guard_denials entries = %d, want 0 (no per-event denial log exists)", len(out.GuardDenials.Entries))
	}
	if out.GuardDenials.Note == "" {
		t.Error("guard_denials must carry a note explaining the counters-only shape")
	}
	// Key lifecycle + management POSTs: honestly empty — no
	// structured source exists yet (see audit.go file doc).
	for _, cat := range []struct {
		name string
		c    auditEmptyCategory
	}{
		{"key_lifecycle", out.KeyLifecycle},
		{"management_posts", out.ManagementPosts},
	} {
		if cat.c.Available {
			t.Errorf("%s.available = true, want false (no structured source exists yet)", cat.name)
		}
		if cat.c.Total != 0 || len(cat.c.Entries) != 0 {
			t.Errorf("%s total/entries = %d/%d, want 0/0", cat.name, cat.c.Total, len(cat.c.Entries))
		}
		if cat.c.Note == "" {
			t.Errorf("%s must carry a note explaining why it is empty", cat.name)
		}
	}
	if out.MCPAudit.Available {
		t.Error("mcp_audit.available = true, want false with no audit log wired")
	}
	if out.MCPAudit.Total != 0 || len(out.MCPAudit.Entries) != 0 {
		t.Errorf("mcp_audit total/entries = %d/%d, want 0/0 unwired", out.MCPAudit.Total, len(out.MCPAudit.Entries))
	}
	if out.Limit != 50 || out.Offset != 0 {
		t.Errorf("limit/offset = %d/%d, want 50/0 (the /api/mcp/audit defaults)", out.Limit, out.Offset)
	}
}

func TestAuditRealDenialCounted(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	// A real guard denial, not a poked counter: a Tier 1 route
	// from a non-loopback peer is refused 403 and increments
	// localOnlyDenials (guard.go requireLocalOnly).
	rec := doReqPeer(t, srv, http.MethodPost, "/admin/bootstrap", "{}", "", "203.0.113.7:1234")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback bootstrap status = %d, want 403", rec.Code)
	}
	_, _, out := getAudit(t, srv, "")
	if out.GuardDenials.LocalOnlyDenials != 1 {
		t.Errorf("local_only_denials = %d, want 1 after one real refusal", out.GuardDenials.LocalOnlyDenials)
	}
	if out.GuardDenials.Total != 1 {
		t.Errorf("guard_denials total = %d, want 1", out.GuardDenials.Total)
	}
	// Still no per-event rows: the counter is real, the event
	// list is honestly empty.
	if len(out.GuardDenials.Entries) != 0 {
		t.Errorf("guard_denials entries = %d, want 0", len(out.GuardDenials.Entries))
	}
}

func TestAuditRedaction(t *testing.T) {
	// Seed the MCP audit log with an entry carrying
	// secret-looking values in the two places a stored entry
	// can hold free text: Reason and OutputSummary (message
	// content). The feed must emit neither.
	const reasonSecret = "sk-live-abcdefghij12345678"
	const outputSecret = "sk-proj-abcdefghij1234567890"
	const outputPrompt = "hello secret prompt text"
	log := mcpTestAuditLog(t)
	entry := mcp.AuditEntry{
		Timestamp:     time.Now().UTC(),
		Tool:          "router_status",
		CallerKind:    mcp.KindGatewayKey,
		CallerID:      "key-abc",
		Decision:      mcp.AuditDeny,
		Reason:        "scope denied for caller key " + reasonSecret,
		InputHash:     "deadbeef-input-hash",
		OutputSummary: "result contained " + outputSecret + " and prompt " + outputPrompt,
		ErrorCode:     mcp.ScopeDeniedCode,
	}
	if err := log.Record(entry); err != nil {
		t.Fatalf("Record: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithMCPAuditLog(log))
	code, body, out := getAudit(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !out.MCPAudit.Available || out.MCPAudit.Total != 1 || len(out.MCPAudit.Entries) != 1 {
		t.Fatalf("mcp_audit available/total/entries = %v/%d/%d, want true/1/1",
			out.MCPAudit.Available, out.MCPAudit.Total, len(out.MCPAudit.Entries))
	}
	for _, needle := range []string{reasonSecret, outputSecret, outputPrompt, "output_summary"} {
		if strings.Contains(body, needle) {
			t.Fatalf("audit feed leaks %q: %s", needle, body)
		}
	}
	// The redacted identifiers that ARE safe still arrive: the
	// tool, the decision, the input hash, the caller id.
	joined := out.MCPAudit.Entries[0]
	if joined.Tool != "router_status" || joined.Decision != mcp.AuditDeny ||
		joined.InputHash != "deadbeef-input-hash" || joined.CallerID != "key-abc" {
		t.Errorf("projected entry = %+v, want tool router_status, decision deny, input hash + caller id intact", joined)
	}
	if !strings.Contains(joined.Reason, "[REDACTED") {
		t.Errorf("reason = %q, want the secret replaced by a redaction marker", joined.Reason)
	}
}

func TestAuditKeyEventStaysHonestAndSecretFree(t *testing.T) {
	// Drive a REAL key lifecycle event — disable a gateway key
	// via POST /admin/keys/{id} — then prove the feed neither
	// invents a key-lifecycle row for it nor leaks any key
	// material. Key events exist only as logger lines
	// (keystatus.go), so the honest category stays empty.
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	rec := doReq(t, srv, "POST", "/admin/keys/"+s.id, `{"disabled":true}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	_, body, out := getAudit(t, srv, "")
	if out.KeyLifecycle.Available || out.KeyLifecycle.Total != 0 || len(out.KeyLifecycle.Entries) != 0 {
		t.Errorf("key_lifecycle after a real disable = available %v total %d entries %d, want false/0/0 (no structured store yet)",
			out.KeyLifecycle.Available, out.KeyLifecycle.Total, len(out.KeyLifecycle.Entries))
	}
	for _, needle := range []string{s.raw, adminRaw} {
		if strings.Contains(body, needle) {
			t.Fatalf("audit feed leaks a gateway key value")
		}
	}
}

func TestAuditMCPPagination(t *testing.T) {
	log := mcpTestAuditLog(t)
	base := time.Now().UTC()
	for i := 0; i < 3; i++ {
		entry := mcp.AuditEntry{
			Timestamp:  base.Add(time.Duration(i) * time.Minute),
			Tool:       "tool-" + string(rune('a'+i)),
			CallerKind: mcp.KindLocalOperator,
			CallerID:   "local-operator",
			Decision:   mcp.AuditAllow,
			InputHash:  mcp.HashArguments(map[string]any{"n": i}),
		}
		if err := log.Record(entry); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	srv := newTestServer(t, &fakeEngine{}, WithMCPAuditLog(log))
	_, _, out := getAudit(t, srv, "?limit=2&offset=1")
	if out.MCPAudit.Total != 3 {
		t.Errorf("mcp_audit total = %d, want 3", out.MCPAudit.Total)
	}
	if len(out.MCPAudit.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 for limit=2&offset=1", len(out.MCPAudit.Entries))
	}
	// Newest first: offset 1 skips tool-c (newest), leaving
	// tool-b then tool-a.
	if out.MCPAudit.Entries[0].Tool != "tool-b" || out.MCPAudit.Entries[1].Tool != "tool-a" {
		t.Errorf("page = %q/%q, want tool-b/tool-a (newest first)",
			out.MCPAudit.Entries[0].Tool, out.MCPAudit.Entries[1].Tool)
	}
	if out.Limit != 2 || out.Offset != 1 {
		t.Errorf("limit/offset = %d/%d, want 2/1", out.Limit, out.Offset)
	}
	// Offset past the end: total stands, entries empty — never
	// an error.
	_, _, out = getAudit(t, srv, "?limit=50&offset=99")
	if out.MCPAudit.Total != 3 || len(out.MCPAudit.Entries) != 0 {
		t.Errorf("offset past end: total/entries = %d/%d, want 3/0", out.MCPAudit.Total, len(out.MCPAudit.Entries))
	}
}

func TestAuditMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	req := httptest.NewRequest(http.MethodPost, "/dashboard/audit", nil)
	rec := httptest.NewRecorder()
	srv.handleDashboardAudit(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
}

func TestAuditGuardRow(t *testing.T) {
	// Pins the exact guard-table row the coordinator must land
	// in guard.go (and the registration line in server.go).
	// The handler enforces no auth itself — the row is the
	// enforcement — so this test is the contract: Tier
	// MANAGEMENT, admin scope, never auth-exempt.
	row := auditGuardRoute()
	if row.Method != "GET" || row.Path != "/dashboard/audit" {
		t.Errorf("guard row = %s %s, want GET /dashboard/audit", row.Method, row.Path)
	}
	if row.Tier != TierManagement {
		t.Errorf("guard row tier = %s, want MANAGEMENT", row.Tier)
	}
	if len(row.Scopes) != 1 || row.Scopes[0] != "admin" {
		t.Errorf("guard row scopes = %v, want [admin]", row.Scopes)
	}
	if row.AuthExempt {
		t.Error("guard row must NOT be auth-exempt: the audit feed is operator-only")
	}
	if row.Description == "" {
		t.Error("guard row needs a description naming what the feed carries")
	}
}
