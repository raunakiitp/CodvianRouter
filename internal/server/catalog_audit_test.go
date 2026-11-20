package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestDashboardCatalogAudit serves the committed audit report
// (web/catalog-audit.json, produced by the live 2026-11-18 run)
// and checks the feed reports its exact headline numbers —
// never an invented audit, never a live re-fetch.
func TestDashboardCatalogAudit(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/catalog-audit", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res struct {
		GeneratedAt string `json:"generated_at"`
		SourceURL   string `json:"source_url"`
		Catalog     struct {
			Total            int `json:"total"`
			VerifiedFromDocs int `json:"verified_from_docs"`
			ReferenceOnly    int `json:"reference_only"`
			Unverified       int `json:"unverified"`
			NoBaseURL        int `json:"no_base_url"`
		} `json:"catalog"`
		Remote struct {
			Providers int `json:"providers"`
			Models    int `json:"models"`
		} `json:"remote"`
		Counts       map[string]int `json:"counts_by_kind"`
		Reachability struct {
			Checked         int `json:"checked"`
			Reachable       int `json:"reachable"`
			Unreachable     int `json:"unreachable"`
			LoopbackSkipped int `json:"loopback_skipped"`
		} `json:"reachability"`
		Findings []struct {
			Kind string `json:"kind"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if res.GeneratedAt == "" || !strings.HasPrefix(res.GeneratedAt, "2026-11-18") {
		t.Errorf("generated_at = %q, want the 2026-11-18 audit", res.GeneratedAt)
	}
	if res.SourceURL != "https://models.dev/api.json" {
		t.Errorf("source_url = %q, want models.dev api.json", res.SourceURL)
	}
	// Headline numbers after the 2026-11-18 triage round 5
	// (CHECKLIST 301-302): 191 providers added cumulatively (17
	// verified from docs, 174 reference-only), 43 missing base
	// URLs filled in the round-1 ledger plus the `wafer` fill
	// in round 2, the `inference-net` fill in round 3 and the
	// `xiaomi-mimo` + `friendliai` fills in round 4, and 5
	// of 12 possible renames applied as docs-proven ID moves.
	// Round 5 adds no fills and no moves (the 3 pending possible
	// renames stay pending).
	if res.Catalog.Total != 614 || res.Catalog.VerifiedFromDocs != 65 ||
		res.Catalog.ReferenceOnly != 545 || res.Catalog.Unverified != 4 ||
		res.Catalog.NoBaseURL != 234 {
		t.Errorf("catalog = %+v, want 614 / 65 / 545 / 4 / 234", res.Catalog)
	}
	if res.Remote.Providers != 324 || res.Remote.Models != 9981 {
		t.Errorf("remote = %+v, want 324 providers / 9981 models", res.Remote)
	}
	if res.Reachability.Checked != 940 || res.Reachability.Reachable != 917 ||
		res.Reachability.Unreachable != 23 || res.Reachability.LoopbackSkipped != 7 {
		t.Errorf("reachability = %+v, want 940 / 917 / 23 / loopback 7", res.Reachability)
	}
	// The DaoXE fix landed before this audit ran, so the audit
	// must show exactly two base_url_differs: fastrouter (the
	// DaoXE-era one) and privatemode-ai (round 2's deliberate
	// docs-over-models.dev choice — the catalog uses the
	// docs-verified proxyless cloud base, models.dev lists the
	// local proxy endpoint) — and no daoxe finding at all.
	if res.Counts["base_url_differs"] != 2 {
		t.Errorf("base_url_differs = %d, want 2 (fastrouter + privatemode-ai)", res.Counts["base_url_differs"])
	}
	if res.Counts["missing_in_catalog"] != 37 || res.Counts["missing_in_modelsdev"] != 327 {
		t.Errorf("missing counts = %d/%d, want 37/327", res.Counts["missing_in_catalog"], res.Counts["missing_in_modelsdev"])
	}
	// The triage record is embedded in the audit JSON, so the
	// dashboard can show verified-added vs still-candidate counts.
	if b := rec.Body.String(); !strings.Contains(b, `"added": 191`) || !strings.Contains(b, `"triage"`) {
		t.Errorf("audit JSON must embed the triage record (191 added)")
	}
	body := rec.Body.String()
	if strings.Contains(body, `"catalog_id": "daoxe"`) || strings.Contains(body, `"catalog_id":"daoxe"`) {
		t.Errorf("audit must contain no daoxe finding after the fix")
	}
	// No secrets ever ride this feed: no gateway keys, no API
	// keys, no hashes.
	for _, needle := range []string{"crk_", "api_key", "sha256:"} {
		if strings.Contains(body, needle) {
			t.Errorf("catalog-audit feed must never contain %q", needle)
		}
	}
}

// TestDashboardCatalogAuditMethodNotAllowed proves the feed is
// GET-only like the other dashboard feeds.
func TestDashboardCatalogAuditMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodPost, "/dashboard/catalog-audit", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}
