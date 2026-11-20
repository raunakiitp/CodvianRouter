package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cataudit"
)

// modelsDevStub is a minimal models.dev api.json document: one
// provider, one model. The audit compares the compiled-in catalog
// against whatever this serves, so the remote counts in the report
// prove the --models-dev-url flag was honoured.
const modelsDevStub = `{
  "openai": {
    "id": "openai",
    "name": "OpenAI",
    "doc": "https://example.invalid/openai",
    "api": "https://api.openai.com/v1",
    "models": {
      "gpt-4": {"id": "gpt-4", "status": "stable"}
    }
  }
}`

// TestRunCatalogAuditFlags is the run-level proof for CHECKLIST
// 271-272: `catalog audit --out-dir` (and its sibling flags)
// reach the handler end to end through run(), not just parseArgs.
// Before the fix the global parser rejected --out-dir as
// "unknown flag" and the audit never ran.
func TestRunCatalogAuditFlags(t *testing.T) {
	// Isolated HOME + working dir: the audit must not touch the
	// repo's real docs/ or web/ when no flags are given.
	// (t.Chdir needs go1.24; the module is go1.22.)
	isolateUserDirs(t)
	work := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(modelsDevStub))
	}))
	defer srv.Close()

	stamp := time.Now().Format("2006-01-02")
	outDir := filepath.Join(work, "audit-out")
	webJSON := filepath.Join(work, "web", "catalog-audit.json")

	// Space form, all flags: the report lands in --out-dir, the
	// dashboard JSON at --web-json, the remote catalog comes from
	// --models-dev-url, reachability is skipped, and an empty
	// --triage-summary= disables triage embedding.
	if code := run([]string{"catalog", "audit",
		"--out-dir", outDir,
		"--web-json", webJSON,
		"--models-dev-url", srv.URL,
		"--skip-reachability",
		"--concurrency", "2",
		"--probe-timeout", "5",
		"--triage-summary="}); code != 0 {
		t.Fatalf("catalog audit exit = %d, want 0", code)
	}
	jsonPath := filepath.Join(outDir, "catalog-audit-"+stamp+".json")
	mdPath := filepath.Join(outDir, "catalog-audit-"+stamp+".md")
	for _, p := range []string{jsonPath, mdPath, webJSON} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected report file %s: %v", p, err)
		}
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var rep cataudit.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("report JSON: %v", err)
	}
	if rep.Remote.Providers != 1 || rep.Remote.Models != 1 {
		t.Fatalf("remote = %d providers / %d models, want 1 / 1 (stub catalog)", rep.Remote.Providers, rep.Remote.Models)
	}
	if rep.SourceURL != srv.URL {
		t.Fatalf("source URL = %q, want the stub server", rep.SourceURL)
	}
	if rep.Catalog.Total == 0 {
		t.Fatalf("catalog total = 0, want the compiled-in catalog")
	}
	webData, err := os.ReadFile(webJSON)
	if err != nil {
		t.Fatal(err)
	}
	var webRep cataudit.Report
	if err := json.Unmarshal(webData, &webRep); err != nil {
		t.Fatalf("dashboard JSON: %v", err)
	}
	if webRep.Remote.Providers != 1 {
		t.Fatalf("dashboard JSON remote providers = %d, want 1", webRep.Remote.Providers)
	}

	// = form: same run, different out dir.
	outDir2 := filepath.Join(work, "audit-out-2")
	webJSON2 := filepath.Join(work, "web2", "catalog-audit.json")
	if code := run([]string{"catalog", "audit",
		"--out-dir=" + outDir2,
		"--web-json=" + webJSON2,
		"--models-dev-url=" + srv.URL,
		"--skip-reachability",
		"--triage-summary="}); code != 0 {
		t.Fatalf("catalog audit (= form) exit = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(outDir2, "catalog-audit-"+stamp+".json")); err != nil {
		t.Fatalf("expected = form report: %v", err)
	}

	// Validation errors surface as usage errors.
	for _, args := range [][]string{
		{"catalog", "audit", "--concurrency", "0"},
		{"catalog", "audit", "--probe-timeout=abc"},
		{"catalog", "audit", "stray"},
		{"catalog", "bogus"},
	} {
		if code := run(args); code != 2 {
			t.Errorf("run(%v) exit = %d, want 2", args, code)
		}
	}
}
