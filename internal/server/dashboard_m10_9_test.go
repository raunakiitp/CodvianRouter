// M10-9 dashboard Audit page (new view): guard denials, key
// lifecycle, management actions, and the MCP audit — read-only,
// live from GET /dashboard/audit (server.go:1393, guard.go:115,
// Tier Management + admin scope; handler + shape in
// internal/server/audit.go).
//
// The feed's four categories are rendered exactly as provided:
// guard_denials is live counters only (no per-event rows exist),
// key_lifecycle and management_posts are honestly empty (no
// structured source exists yet), and mcp_audit is the only
// per-event list — newest first, paginated by limit/offset.
// The feed offers NO actor/action filter and NO pagination for
// the three non-MCP categories, so the page has no filter
// controls; the section says so. Read-only: no audit mutation
// controls exist, and none are rendered.
//
// Proves the section is served in the live dashboard HTML, not
// just present in the file.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/mcp"
)

// auditSection returns the served HTML of <section id="view-audit">.
func auditSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="view-audit"`)
	if start < 0 {
		t.Fatalf("dashboard has no view-audit section")
	}
	rest := body[start:]
	end := strings.Index(rest, "</section>")
	if end < 0 {
		t.Fatalf("view-audit section is not closed")
	}
	return rest[:end]
}

// auditInlineScript returns the served dashboard's single inline
// <script> block.
func auditInlineScript(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<script>")
	if start < 0 {
		t.Fatalf("dashboard has no inline script")
	}
	rest := body[start+len("<script>"):]
	end := strings.Index(rest, "</script>")
	if end < 0 {
		t.Fatalf("inline script is not closed")
	}
	return rest[:end]
}

// m10_9Function returns the source of the named top-level
// dashboard JS function, from its "function <name>(" line to the
// next top-level "function " line (or the end of the served HTML).
func m10_9Function(t *testing.T, body, name string) string {
	t.Helper()
	start := strings.Index(body, "function "+name+"(")
	if start < 0 {
		t.Fatalf("dashboard JS has no function %s", name)
	}
	rest := body[start:]
	if end := strings.Index(rest, "\n  function "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// m10_9AdminSetup builds a server with an MCP audit log and a
// gateway key carrying the admin scope the audit feed requires,
// and returns the server plus the raw key.
func m10_9AdminSetup(t *testing.T, log *mcp.AuditLog) (*Server, string) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	key, raw, err := config.IssueKey("test-admin", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatalf("IssueKey: %v", err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatalf("AddGatewayKey: %v", err)
	}
	return newTestServer(t, &fakeEngine{}, WithConfig(cfg), WithMCPAuditLog(log)), raw
}

// m10_9SeedMCPLog records three MCP audit entries (newest first
// after the feed sorts them) and returns the log. One entry
// carries a secret-looking reason and a secret-bearing output
// summary: the feed must redact the reason and drop the summary,
// so a leak would be a real redaction failure.
func m10_9SeedMCPLog(t *testing.T) *mcp.AuditLog {
	t.Helper()
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
		if i == 2 {
			entry.Decision = mcp.AuditDeny
			entry.Reason = "scope denied for caller key sk-live-abcdefghij12345678"
			entry.OutputSummary = "result contained sk-proj-abcdefghij1234567890"
			entry.ErrorCode = mcp.ScopeDeniedCode
		}
		if err := log.Record(entry); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	return log
}

func TestDashboardM10_9AuditRenders(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	section := auditSection(t, body)

	for _, want := range []string{
		// Guard denials: live counters.
		`id="m10_9DenialsTotal"`,
		`id="m10_9DenialsTotalNum"`,
		`id="m10_9LocalOnly"`,
		`id="m10_9GuardrailBlocked"`,
		`id="m10_9SecRefTotal"`,
		`id="m10_9SecRefBody"`,
		`id="m10_9SecRefEmpty"`,
		`id="m10_9DenialsNote"`,
		// Key lifecycle + management posts: honest empty states.
		`id="m10_9KeyStatus"`,
		`id="m10_9KeyNote"`,
		`id="m10_9KeyEmpty"`,
		`id="m10_9MgmtStatus"`,
		`id="m10_9MgmtNote"`,
		`id="m10_9MgmtEmpty"`,
		// MCP audit: per-event table + pagination.
		`id="m10_9McpCount"`,
		`id="m10_9McpBody"`,
		`id="m10_9McpEmpty"`,
		`id="m10_9McpNote"`,
		`id="m10_9McpMeta"`,
		`id="m10_9McpPage"`,
		`id="m10_9McpPrevBtn"`,
		`id="m10_9McpNextBtn"`,
		// Admin-key flow + refresh.
		`id="m10_9KeyCard"`,
		`id="m10_9LoadBtn"`,
		`id="m10_9RefreshBtn"`,
		`id="m10_9ErrorCard"`,
		// Labels that pin the honest scope.
		"Guard denials",
		"Key lifecycle",
		"Management actions",
		"MCP audit",
		"GET /dashboard/audit",
		"No key lifecycle events recorded yet",
		"No management actions recorded yet",
		"No MCP tool calls audited yet",
		// The feed has no actor/action filter: the page says so
		// instead of offering a filter that filters nothing.
		"The feed offers no actor/action filter",
		// The operator key is never stored: the page says to
		// retype it before Refresh or paging, or the load aborts.
		"The operator key is never stored, so retype it in the sidebar before Refresh or paging (Older/Newer), or the load aborts.",
		// Read-only: no audit mutation controls exist.
		"read-only",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("dashboard Audit section missing %q", want)
		}
	}

	// The nav slot is wired: the Audit nav item exists and the
	// section it points at exists (previously an orphan slot).
	if !strings.Contains(body, `data-view="audit"`) {
		t.Errorf("dashboard nav has no data-view=audit item")
	}

	// The loader reads exactly the B4 feed — a whole-body search
	// would pass on labels and comments elsewhere in the file
	// that merely mention the path, so match the fetch( call
	// inside the loader function itself.
	if !strings.Contains(m10_9Function(t, body, "m10_9LoadAudit"), `fetch("/dashboard/audit?limit="`) {
		t.Errorf("dashboard Audit JS m10_9LoadAudit must fetch GET /dashboard/audit with limit/offset")
	}
	// The loader pages the MCP join (limit/offset) and asks for
	// the admin key on 401/403 instead of rendering a fake empty
	// list.
	loader := m10_9Function(t, body, "m10_9LoadAudit")
	for _, want := range []string{"m10_9Offset", "adminHeaders()", "401", "403"} {
		if !strings.Contains(loader, want) {
			t.Errorf("dashboard Audit JS m10_9LoadAudit missing %q", want)
		}
	}

	// Negative controls: no filter inputs (the feed supports
	// none), no mutation controls (none exist), no invented
	// per-event denial/key/management rows.
	for _, banned := range []string{
		`id="m10_9ActorFilter"`,
		`id="m10_9ActionFilter"`,
		"filter by actor",
		"Filter by actor",
		"<form",
		"Delete audit",
		"Clear audit",
		" coming soon",
		"Coming soon",
	} {
		if strings.Contains(section, banned) {
			t.Errorf("Audit section must not contain %q", banned)
		}
	}
	lower := strings.ToLower(section)
	for _, banned := range []string{"delete", "revoke", "purge"} {
		if strings.Contains(lower, banned) {
			t.Errorf("Audit section must not contain a %q mutation control", banned)
		}
	}
}

func TestDashboardM10_9FeedValuesEqualSmoke(t *testing.T) {
	log := m10_9SeedMCPLog(t)
	srv, adminRaw := m10_9AdminSetup(t, log)

	// The feed is admin-scoped: without a key it must refuse,
	// with the admin key it answers 200.
	rec := doReq(t, srv, http.MethodGet, "/dashboard/audit", "", "")
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("GET /dashboard/audit without a key status = %d, want 401 or 403", rec.Code)
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/audit", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/audit with admin key status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var feed auditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("GET /dashboard/audit decode: %v (body %q)", err, rec.Body.String())
	}
	// MCP join: 3 seeded entries, newest first.
	if !feed.MCPAudit.Available || feed.MCPAudit.Total != 3 || len(feed.MCPAudit.Entries) != 3 {
		t.Fatalf("mcp_audit available/total/entries = %v/%d/%d, want true/3/3",
			feed.MCPAudit.Available, feed.MCPAudit.Total, len(feed.MCPAudit.Entries))
	}
	if feed.MCPAudit.Entries[0].Tool != "tool-c" || feed.MCPAudit.Entries[2].Tool != "tool-a" {
		t.Errorf("mcp_audit order = %q..%q, want tool-c..tool-a (newest first)",
			feed.MCPAudit.Entries[0].Tool, feed.MCPAudit.Entries[2].Tool)
	}
	// Pagination: limit=2&offset=1 leaves tool-b then tool-a.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/audit?limit=2&offset=1", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/audit?limit=2&offset=1 status = %d, want 200", rec.Code)
	}
	var page auditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("paged audit decode: %v", err)
	}
	if page.MCPAudit.Total != 3 || len(page.MCPAudit.Entries) != 2 ||
		page.MCPAudit.Entries[0].Tool != "tool-b" || page.MCPAudit.Entries[1].Tool != "tool-a" {
		t.Errorf("paged mcp_audit total/entries = %d/%d, want 3/2 tool-b/tool-a",
			page.MCPAudit.Total, len(page.MCPAudit.Entries))
	}
	if page.Limit != 2 || page.Offset != 1 {
		t.Errorf("limit/offset = %d/%d, want 2/1", page.Limit, page.Offset)
	}
	// No secret material survives the feed: the seeded reason's
	// key is redacted and the output summary never leaves.
	for _, needle := range []string{"sk-live-abcdefghij12345678", "sk-proj-abcdefghij1234567890", "output_summary"} {
		if strings.Contains(rec.Body.String(), needle) {
			t.Errorf("audit feed leaks %q", needle)
		}
	}

	// A real guard denial is counted live: a Tier 1 route from a
	// non-loopback peer is refused 403 and increments the counter.
	rec = doReqPeer(t, srv, http.MethodPost, "/admin/bootstrap", "{}", "", "203.0.113.7:1234")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback bootstrap status = %d, want 403", rec.Code)
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/audit", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/audit after denial status = %d, want 200", rec.Code)
	}
	var after auditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("audit decode after denial: %v", err)
	}
	if after.GuardDenials.LocalOnlyDenials != 1 || after.GuardDenials.Total != 1 {
		t.Errorf("guard_denials local_only/total = %d/%d, want 1/1 after one real refusal",
			after.GuardDenials.LocalOnlyDenials, after.GuardDenials.Total)
	}
	// Still no per-event rows: the counter is real, the event
	// list is honestly empty.
	if len(after.GuardDenials.Entries) != 0 {
		t.Errorf("guard_denials entries = %d, want 0", len(after.GuardDenials.Entries))
	}
	// Key lifecycle + management posts stay honestly empty: no
	// structured source exists yet, so nothing is reconstructed.
	if after.KeyLifecycle.Available || after.KeyLifecycle.Total != 0 || len(after.KeyLifecycle.Entries) != 0 {
		t.Errorf("key_lifecycle = available %v total %d entries %d, want false/0/0",
			after.KeyLifecycle.Available, after.KeyLifecycle.Total, len(after.KeyLifecycle.Entries))
	}
	if after.ManagementPosts.Available || after.ManagementPosts.Total != 0 || len(after.ManagementPosts.Entries) != 0 {
		t.Errorf("management_posts = available %v total %d entries %d, want false/0/0",
			after.ManagementPosts.Available, after.ManagementPosts.Total, len(after.ManagementPosts.Entries))
	}
}

// The page's pure helpers run under node, against the exact code
// served in the dashboard: node --check on the whole extracted
// inline script, then the pure block driven with the REAL feed
// response fetched above — the projected rows must equal the feed
// entries field by field, and the empty feed must render empty
// rows, never invented ones.
func TestDashboardM10_9PureHelpersUnderNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH — cannot run the inline-script check")
	}
	log := m10_9SeedMCPLog(t)
	srv, adminRaw := m10_9AdminSetup(t, log)
	rec := doReq(t, srv, http.MethodGet, "/dashboard/audit", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/audit status = %d, want 200", rec.Code)
	}
	var feed auditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("audit decode: %v", err)
	}
	// Drive one real denial through first so the denial rows
	// are non-empty under node too.
	rec = doReqPeer(t, srv, http.MethodPost, "/admin/bootstrap", "{}", "", "203.0.113.7:1234")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback bootstrap status = %d, want 403", rec.Code)
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/audit", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/audit after denial status = %d, want 200", rec.Code)
	}
	var feedAfter auditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &feedAfter); err != nil {
		t.Fatalf("audit decode after denial: %v", err)
	}
	if feedAfter.GuardDenials.Total != 1 {
		t.Fatalf("guard_denials total = %d, want 1 after one real refusal", feedAfter.GuardDenials.Total)
	}

	srv2 := newTestServer(t, &fakeEngine{})
	rec = doReq(t, srv2, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	script := auditInlineScript(t, rec.Body.String())
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "inline.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("node", "--check", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("node --check on the extracted inline script failed: %v: %s", err, out)
	}
	start := strings.Index(script, "/* M10-9-PURE-START */")
	end := strings.Index(script, "/* M10-9-PURE-END */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("pure helper block markers (M10-9-PURE-START/END) not found in the inline script")
	}
	pure := script[start:end]

	mustJSON := func(t *testing.T, v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal feed for node driver: %v", err)
		}
		return string(b)
	}
	driver := pure + `
var feed = ` + mustJSON(t, feedAfter) + `;
var emptyFeed = {guard_denials: {available: true, total: 0, local_only_denials: 0, guardrail_blocked: 0, security_refusals_total: 0, entries: [], note: "n"}, key_lifecycle: {available: false, total: 0, entries: [], note: "n"}, management_posts: {available: false, total: 0, entries: [], note: "n"}, mcp_audit: {available: false, total: 0, entries: [], cap: 0, dropped: 0, note: "n"}, limit: 50, offset: 0};
var out = {
  mcpRows: m10_9McpRows(feed),
  denialRows: m10_9DenialRows(feed),
  emptyMcp: m10_9McpRows(emptyFeed),
  emptyDenials: m10_9DenialRows(emptyFeed),
  pageFull: m10_9McpPageLabel(3, 0, 3),
  pageMid: m10_9McpPageLabel(3, 1, 2),
  pageEmpty: m10_9McpPageLabel(0, 0, 0),
  fmtTime: m10_9FmtTime("2026-11-19T10:20:30Z"),
  fmtTimeBad: m10_9FmtTime("not-a-time"),
  fmtTimeEmpty: m10_9FmtTime("")
};
console.log(JSON.stringify(out));
`
	driverPath := filepath.Join(dir, "driver.js")
	if err := os.WriteFile(driverPath, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("node", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v: %s", err, raw)
	}
	var got struct {
		McpRows []struct {
			Timestamp  string `json:"timestamp"`
			Tool       string `json:"tool"`
			CallerKind string `json:"callerKind"`
			CallerID   string `json:"callerId"`
			Decision   string `json:"decision"`
			Reason     string `json:"reason"`
			InputHash  string `json:"inputHash"`
			ErrorCode  int    `json:"errorCode"`
		} `json:"mcpRows"`
		DenialRows []struct {
			Label string `json:"label"`
			Count int    `json:"count"`
		} `json:"denialRows"`
		EmptyMcp     []struct{} `json:"emptyMcp"`
		EmptyDenials []struct{} `json:"emptyDenials"`
		PageFull     string     `json:"pageFull"`
		PageMid      string     `json:"pageMid"`
		PageEmpty    string     `json:"pageEmpty"`
		FmtTime      string     `json:"fmtTime"`
		FmtTimeBad   string     `json:"fmtTimeBad"`
		FmtTimeEmpty string     `json:"fmtTimeEmpty"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode node driver output: %v (raw %s)", err, raw)
	}

	// MCP rows equal the feed entries field by field, newest first.
	if len(got.McpRows) != 3 {
		t.Fatalf("mcpRows = %d, want 3", len(got.McpRows))
	}
	for i, want := range feedAfter.MCPAudit.Entries {
		g := got.McpRows[i]
		if g.Tool != want.Tool || g.CallerKind != string(want.CallerKind) ||
			g.CallerID != want.CallerID || g.Decision != string(want.Decision) ||
			g.InputHash != want.InputHash || g.ErrorCode != want.ErrorCode {
			t.Errorf("mcpRows[%d] = %+v, want tool %q caller %q/%q decision %q hash %q code %d",
				i, g, want.Tool, want.CallerKind, want.CallerID, want.Decision, want.InputHash, want.ErrorCode)
		}
	}
	// The newest row is the denied one: its reason is redacted
	// (the secret is gone) and no output summary survives the
	// projection — the rows carry no output_summary key at all.
	if !strings.Contains(got.McpRows[0].Reason, "[REDACTED") {
		t.Errorf("mcpRows[0].reason = %q, want the secret replaced by a redaction marker", got.McpRows[0].Reason)
	}
	for _, needle := range []string{"sk-live-abcdefghij12345678", "sk-proj-abcdefghij1234567890", "output_summary"} {
		if strings.Contains(string(raw), needle) {
			t.Errorf("node driver output leaks %q", needle)
		}
	}
	// Denial rows: one real refusal, labelled by its source.
	if len(got.DenialRows) != 0 {
		// security_refusals is empty on this path (the refusal
		// is a loopback denial, counted in local_only_denials,
		// not in the per-label map) — the rows must be empty
		// rather than invented from the total.
		t.Errorf("denialRows = %+v, want [] (loopback refusals have no per-label row)", got.DenialRows)
	}
	// Empty feed renders empty rows, never invented ones.
	if len(got.EmptyMcp) != 0 || len(got.EmptyDenials) != 0 {
		t.Errorf("empty feed rows = %d/%d, want 0/0", len(got.EmptyMcp), len(got.EmptyDenials))
	}
	if got.PageFull != "Showing 1–3 of 3" {
		t.Errorf("pageFull = %q, want \"Showing 1–3 of 3\"", got.PageFull)
	}
	if got.PageMid != "Showing 2–3 of 3" {
		t.Errorf("pageMid = %q, want \"Showing 2–3 of 3\"", got.PageMid)
	}
	if got.PageEmpty != "No entries" {
		t.Errorf("pageEmpty = %q, want \"No entries\"", got.PageEmpty)
	}
	if got.FmtTime != "2026-11-19 10:20:30 UTC" {
		t.Errorf("fmtTime = %q, want \"2026-11-19 10:20:30 UTC\"", got.FmtTime)
	}
	if got.FmtTimeBad != "—" || got.FmtTimeEmpty != "—" {
		t.Errorf("fmtTime bad/empty = %q/%q, want \"—\"/\"—\"", got.FmtTimeBad, got.FmtTimeEmpty)
	}
}
