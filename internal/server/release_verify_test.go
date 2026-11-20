package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestDashboardReleaseVerify serves the committed release-verify
// record (web/release-verify.json, the 267-268 snapshot proof
// re-run by 289-290) and checks the feed reports its exact
// recorded numbers — never an invented pass, never a live re-run.
// The recorded run is an UNSIGNED snapshot, so its verdict is
// FAILED, correctly: the bundle/signature checks fail by design
// and the checks that can pass on an unsigned snapshot all pass.
// Items 307-308: the record also carries the `gate` verdict —
// the in-job verify is a post-publish tripwire by final design,
// never a pre-publish gate — and the test pins that wording too.
func TestDashboardReleaseVerify(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/release-verify", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Recorded bool   `json:"recorded"`
		Tag      string `json:"tag"`
		Date     string `json:"date"`
		Verdict  string `json:"verdict"`
		ExitCode int    `json:"exit_code"`
		Checks   struct {
			Checksums struct {
				Checked int `json:"checked"`
				Matched int `json:"matched"`
			} `json:"checksums"`
			Archives struct {
				Expected  int `json:"expected"`
				OK        int `json:"ok"`
				WindowsOK int `json:"windows_ok"`
			} `json:"archives"`
			SBOM struct {
				Expected int `json:"expected"`
				OK       int `json:"ok"`
			} `json:"sbom"`
			Bundles struct {
				Expected int `json:"expected"`
				OK       int `json:"ok"`
				Failed   int `json:"failed"`
			} `json:"bundles"`
			Signatures struct {
				Expected int `json:"expected"`
				OK       int `json:"ok"`
				Failed   int `json:"failed"`
			} `json:"signatures"`
			Attestations struct {
				Expected int `json:"expected"`
				Skipped  int `json:"skipped"`
			} `json:"attestations"`
		} `json:"checks"`
		Totals struct {
			OK      int `json:"ok"`
			Failed  int `json:"failed"`
			Skipped int `json:"skipped"`
			Total   int `json:"total"`
		} `json:"totals"`
		WindowsExecution string `json:"windows_execution"`
		CIWiring         string `json:"ci_wiring"`
		// Items 307-308: the record carries the gate verdict — the
		// in-job verify is a post-publish tripwire by final design,
		// never a pre-publish gate.
		Gate string `json:"gate"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !res.Recorded {
		t.Errorf("recorded = false, want the recorded 267-268 snapshot run")
	}
	if res.Tag != "v0.0.0-SNAPSHOT-none" {
		t.Errorf("tag = %q, want v0.0.0-SNAPSHOT-none", res.Tag)
	}
	if res.Date != "2026-11-18" {
		t.Errorf("date = %q, want 2026-11-18", res.Date)
	}
	// Unsigned snapshot: FAILED, exit 1 — the honest verdict,
	// never rewritten as a pass.
	if res.Verdict != "FAILED" {
		t.Errorf("verdict = %q, want FAILED (unsigned snapshot)", res.Verdict)
	}
	if res.ExitCode != 1 {
		t.Errorf("exit_code = %d, want 1", res.ExitCode)
	}
	if res.Checks.Checksums.Checked != 12 || res.Checks.Checksums.Matched != 12 {
		t.Errorf("checksums = %d/%d, want 12/12", res.Checks.Checksums.Matched, res.Checks.Checksums.Checked)
	}
	if res.Checks.Archives.Expected != 6 || res.Checks.Archives.OK != 6 || res.Checks.Archives.WindowsOK != 2 {
		t.Errorf("archives = %d/%d (windows %d), want 6/6 (windows 2)", res.Checks.Archives.OK, res.Checks.Archives.Expected, res.Checks.Archives.WindowsOK)
	}
	if res.Checks.SBOM.Expected != 6 || res.Checks.SBOM.OK != 6 {
		t.Errorf("sbom = %d/%d, want 6/6", res.Checks.SBOM.OK, res.Checks.SBOM.Expected)
	}
	if res.Checks.Bundles.Expected != 7 || res.Checks.Bundles.OK != 0 || res.Checks.Bundles.Failed != 7 {
		t.Errorf("bundles = ok %d failed %d of %d, want ok 0 failed 7 of 7", res.Checks.Bundles.OK, res.Checks.Bundles.Failed, res.Checks.Bundles.Expected)
	}
	if res.Checks.Signatures.Expected != 7 || res.Checks.Signatures.OK != 0 || res.Checks.Signatures.Failed != 7 {
		t.Errorf("signatures = ok %d failed %d of %d, want ok 0 failed 7 of 7", res.Checks.Signatures.OK, res.Checks.Signatures.Failed, res.Checks.Signatures.Expected)
	}
	if res.Checks.Attestations.Expected != 6 || res.Checks.Attestations.Skipped != 6 {
		t.Errorf("attestations = skipped %d of %d, want skipped 6 of 6", res.Checks.Attestations.Skipped, res.Checks.Attestations.Expected)
	}
	if res.Totals.OK != 13 || res.Totals.Failed != 14 || res.Totals.Skipped != 6 || res.Totals.Total != 33 {
		t.Errorf("totals = ok %d failed %d skipped %d total %d, want 13/14/6/33", res.Totals.OK, res.Totals.Failed, res.Totals.Skipped, res.Totals.Total)
	}
	if !strings.Contains(res.WindowsExecution, "unproven") {
		t.Errorf("windows_execution = %q, want it to say execution is unproven", res.WindowsExecution)
	}
	if !strings.Contains(res.CIWiring, "289-290") || !strings.Contains(res.CIWiring, "first") {
		t.Errorf("ci_wiring = %q, want it to name the 289-290 wiring and the pending first CI run", res.CIWiring)
	}
	if !strings.Contains(res.Gate, "TRIPWIRE") || !strings.Contains(res.Gate, "307-308") || !strings.Contains(res.Gate, "Pro") {
		t.Errorf("gate = %q, want the 307-308 verdict: post-publish tripwire by final design, pre-publish gate needing GoReleaser Pro", res.Gate)
	}
}
