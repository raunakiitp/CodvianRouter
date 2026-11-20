// M10-3 dashboard Models section: table-only Models view with live
// Models/Combos tabs, a Type column (wire_format) and a Health
// column driven by the real GET /v1/models/health endpoint, plus a
// per-row Test button that re-checks that provider's probe result.
// Proves the section is served in the live dashboard HTML and reads
// the live feeds — it never invents Capabilities / Max output /
// Free-tier columns, because GET /dashboard/models carries none of
// those fields (B9 verify-first: has_free + key-creation URL absent,
// so no free-tier marking is rendered and B9 stays queued).
package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestDashboardM10_3(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Scope the section assertions to the Models section itself:
	// other builders' regions (e.g. Providers, which legitimately
	// discusses has_free_tier on GET /api/catalog) share the page.
	secStart := strings.Index(body, `id="view-models"`)
	secEnd := strings.Index(body, `id="view-autorouter"`)
	if secStart < 0 || secEnd <= secStart {
		t.Fatalf("dashboard Models section bounds not found (view-models .. view-autorouter)")
	}
	section := body[secStart:secEnd]
	// Markers in the section's static HTML.
	for _, want := range []string{
		`id="view-models"`,
		// Q24 title/subtitle.
		"Browse and health-test models and combo routing pipelines",
		// Q25 tabs with live counts from the feeds.
		`id="modelsTabModels"`,
		`id="modelsTabCombos"`,
		`id="modelsTabModelsCount"`,
		`id="modelsTabCombosCount"`,
		`id="modelsTabPanelModels"`,
		`id="modelsTabPanelCombos"`,
		`id="combosBody"`,
		`id="combosEmpty"`,
		// Table-only: thead columns the live feed actually supplies.
		"<th>Model</th>",
		"<th>Provider</th>",
		"<th>Type</th>",
		`<th class="num">Context</th>`,
		`<th class="num">Input / 1M</th>`,
		`<th class="num">Output / 1M</th>`,
		"<th>Price source</th>",
		`<th class="num">Requests</th>`,
		"<th>Fit</th>",
		"<th>Health</th>",
		"<th>Status</th>",
		`id="modelBody"`,
		// Price-source + synced markers (item 180) and refresh.
		"models.dev",
		`id="modelSyncStatus"`,
		`id="modelRefreshBtn"`,
		// Honest note: columns the feed does not supply are absent.
		"not carried by the live models feed",
		// Per-pair price truth link (plan §5 M10-3).
		"/dashboard/prices",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("dashboard Models section missing %q", want)
		}
	}
	// Markers in the section's JS — scoped to the Models JS block
	// (the "Models: live catalog only" section of the <script>,
	// up to the next JS section), not the whole page: a marker
	// appearing in another region must not mask missing Models
	// wiring. The live feeds the section reads, never invented data.
	jsStart := strings.Index(body, `/* ---------- Models: live catalog only ---------- */`)
	jsEnd := strings.Index(body, `/* ---------- Routing groups: live member health (CHECKLIST 174) ---------- */`)
	if jsStart < 0 || jsEnd <= jsStart {
		t.Fatalf("dashboard Models JS bounds not found (Models .. Routing groups)")
	}
	modelsJS := body[jsStart:jsEnd]
	for _, want := range []string{
		"/dashboard/models",
		"/v1/models/health",
		"/v1/combos",
		"data-test-model",
		"/dashboard/models/refresh",
	} {
		if !strings.Contains(modelsJS, want) {
			t.Errorf("dashboard Models JS missing %q", want)
		}
	}
	// Columns the feed does not supply must not exist as columns —
	// a missing column is honestly absent, never filled with
	// invented values. (The words appear in the honest-note prose
	// only, never as a <th>.)
	for _, absent := range []string{
		"<th>Capabilities",
		"<th>Max output",
		"<th>Free",
		"<th>Custom",
		"has_free",
		"key-creation",
	} {
		if strings.Contains(section, absent) {
			t.Errorf("dashboard Models section must not render %q (feed does not supply it)", absent)
		}
	}
	// No Playground anywhere in the dashboard.
	if strings.Contains(body, "Playground") {
		t.Errorf("dashboard must not contain a Playground")
	}
}

func TestDashboardM10_3FeedsExist(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	// The models feed itself: B9 verify-first — it must NOT carry
	// has_free or a key-creation URL (if it ever does, M10-3 renders
	// them and this test must be updated, not silently passed).
	rec := doReq(t, srv, http.MethodGet, "/dashboard/models", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/models status = %d, want 200", rec.Code)
	}
	for _, absent := range []string{"has_free", "key_url", "key_creation_url", "signup_url"} {
		if strings.Contains(rec.Body.String(), absent) {
			t.Errorf("GET /dashboard/models unexpectedly carries %q — B9 is no longer zero-work; render it and update this test", absent)
		}
	}
	// The real health-test endpoint the Health column + Test button
	// read: registered, provider-scoped probes, unknown when unprobed.
	rec = doReq(t, srv, http.MethodGet, "/v1/models/health", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models/health status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"probe_scope":"provider"`) {
		t.Errorf("GET /v1/models/health missing provider probe_scope, body = %s", rec.Body.String())
	}
	// The combos feed behind the Combos tab.
	rec = doReq(t, srv, http.MethodGet, "/v1/combos", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/combos status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"combos"`) {
		t.Errorf("GET /v1/combos missing combos list, body = %s", rec.Body.String())
	}
}
