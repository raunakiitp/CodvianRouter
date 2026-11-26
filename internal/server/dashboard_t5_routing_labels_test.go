// Phase 2/T5 — Live Routing view depth, regression-pinned against the
// EXACT inline script served by GET /dashboard (node --check first,
// then the pure trace-label helpers driven with fixture attempts).
//
// The backend emits real outcomes (routetrace.AttemptTrace.Outcome):
// hedged_cancelled (a hedge race loser), skipped_breaker /
// skipped_cooldown / skipped_zero_quota / skipped_max_attempts
// (candidates never dialled). Before T5 those rows rendered their raw
// outcome strings; these tests pin the human explanations AND the
// hedge winner/loser tagging derived from the request's real hedge
// decision.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func dashboardInlineScript(t *testing.T) string {
	t.Helper()
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	start := strings.Index(body, "<script>")
	if start < 0 {
		t.Fatal("dashboard has no inline script")
	}
	rest := body[start+len("<script>"):]
	end := strings.Index(rest, "</script>")
	if end < 0 {
		t.Fatal("inline script is not closed")
	}
	return rest[:end]
}

func TestDashboardT5RoutingLabelsUnderNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not on PATH — cannot run the inline-script check (install node or set NODE_PATH)")
	}
	script := dashboardInlineScript(t)
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "inline.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("node", "--check", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("node --check on the served inline script failed: %v: %s", err, out)
	}

	start := strings.Index(script, "function traceReasonLabel")
	end := strings.Index(script, "function renderTraceInto")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("trace label helpers not found in the served script")
	}
	pure := script[start:end]

	driver := `
function escHtml(s){ return String(s == null ? "" : s); }
function fmtNum(n){ return String(n); }
var traceHedge = "";
var __out = {};
` + pure + `
__out.loser      = traceReasonLabel("", "hedged_cancelled");
__out.breaker    = traceReasonLabel("upstream_error", "skipped_breaker");
__out.cooldown   = traceReasonLabel("rate_limited", "skipped_cooldown");
__out.zeroQuota  = traceReasonLabel("", "skipped_zero_quota");
__out.maxAtt     = traceReasonLabel("", "skipped_max_attempts");
__out.successCtl = traceReasonLabel("", "success");
traceHedge = "armed";
__out.winArmed  = hedgeRoleLabel({outcome: "success"});
__out.losArmed  = hedgeRoleLabel({outcome: "hedged_cancelled"});
__out.errArmed  = hedgeRoleLabel({outcome: "error"});
traceHedge = "";
__out.winOff    = hedgeRoleLabel({outcome: "success"});
console.log(JSON.stringify(__out));
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
		Loser     string `json:"loser"`
		Breaker   string `json:"breaker"`
		Cooldown  string `json:"cooldown"`
		ZeroQuota string `json:"zeroQuota"`
		MaxAtt    string `json:"maxAtt"`
		Success   string `json:"successCtl"`
		WinArmed  string `json:"winArmed"`
		LosArmed  string `json:"losArmed"`
		ErrArmed  string `json:"errArmed"`
		WinOff    string `json:"winOff"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("driver output not JSON: %v (%s)", err, raw)
	}
	for _, c := range []struct{ name, val, want string }{
		{"hedged_cancelled", got.Loser, "hedge loser"},
		{"skipped_breaker", got.Breaker, "breaker"},
		{"skipped_cooldown", got.Cooldown, "cooldown"},
		{"skipped_zero_quota", got.ZeroQuota, "quota"},
		{"skipped_max_attempts", got.MaxAtt, "attempt budget"},
	} {
		if !strings.Contains(c.val, c.want) {
			t.Errorf("%s label = %q, want it to contain %q", c.name, c.val, c.want)
		}
	}
	if got.Success != "served the request" {
		t.Errorf("success label changed: %q", got.Success)
	}
	if !strings.Contains(got.WinArmed, "hedge winner") {
		t.Errorf("armed winner = %q, want the hedge-winner tag", got.WinArmed)
	}
	if !strings.Contains(got.LosArmed, "hedge loser") {
		t.Errorf("armed loser = %q, want the hedge-loser tag", got.LosArmed)
	}
	if got.ErrArmed != "" {
		t.Errorf("armed non-success/non-loser attempt got a hedge role: %q", got.ErrArmed)
	}
	if got.WinOff != "" {
		t.Errorf("unhedged success got a hedge role: %q — roles derive from the request's real hedge decision", got.WinOff)
	}
}
