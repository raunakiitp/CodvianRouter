// M10-2 Providers section: grouped catalog-as-addable-groups page.
// Proves the section is served in the live dashboard HTML with the
// Free Tier group, the configured-only filter + count, per-group
// Test-all buttons and the Test Results modal — and that the two
// feeds it reads (GET /api/catalog, GET /api/providers) answer with
// the shapes the section codes against. The section never invents
// provider data: every row comes from those feeds.
//
// BLOCKED (feed gap, documented here so a later backend slice can
// close it — the UI must not fake it in the meantime):
//   - GET /api/providers exposes no per-provider key expiry, so
//     there is no expiration banner on provider cards.
//
// CLOSED by the M10 feed-field slice (2026-11-19): GET /api/catalog
// now carries has_free_tier / category from the catalog, and
// GET /api/providers now carries last_error from the health
// checker's last status. At runtime the Free Tier group renders real
// rows from the live feed (entries with has_free_tier === true) and
// the Test Results modal surfaces the real last_error text —
// "No data yet" appears nowhere in the rendered group. The static
// HTML served by /dashboard only carries the pre-fetch "Loading…"
// count and the empty-state copy ("No free-tier providers — the
// catalog has no free-tier entries, or the catalog fetch failed"),
// shown before the feed loads or when the catalog has no free-tier
// entries / the fetch fails; that copy is a placeholder, not the
// group's content. free_tier_note stays out of the feed on purpose
// (notes embed URLs; the feed carries no URL).
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/health"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestDashboardM10_2ProvidersSection(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="view-providers"`,
		`id="catalogCard"`,
		`id="catalogListBody"`,
		`id="catalogSearch"`,
		`id="catalogCount"`,
		// Configured-only filter + configured count (Q19).
		`id="catalogConfiguredOnly"`,
		`id="catalogConfiguredCount"`,
		`id="testAllConfiguredBtn"`,
		// Free Tier Providers group (Q20): always visible, renders
		// real free-tier rows from the live feed at runtime; the
		// served HTML carries only the pre-fetch count and the
		// empty-state copy (shown when the catalog has no
		// free-tier entries or the fetch fails).
		`id="freeTierCard"`,
		`Free Tier Providers`,
		`id="freeTierCount"`,
		`id="freeTierListBody"`,
		`id="freeTierEmpty"`,
		`No free-tier providers — the catalog has no free-tier entries, or the catalog fetch failed.`,
		`id="testAllFreeTierBtn"`,
		// Per-group Test all (Q11) + Test Results modal (Q21).
		`id="provTestModal"`,
		`id="provTestModalSummary"`,
		`id="provTestModalBody"`,
		`id="provTestModalClose"`,
		// Groups render from the live feeds, never hard-coded.
		`Configured providers`,
		`/api/catalog`,
		`/api/providers`,
		// Provider-card Connect is wired (critic fix 2026-11-19):
		// a disabled provider's Connect button POSTs
		// {enabled:true} to /api/providers/{id}/enabled.
		`+ "/enabled"`,
		`JSON.stringify({enabled: true})`,
		`Connecting…`,
		// Provider-key rotation is wired (critic fix 2026-11-19):
		// Enter in the "paste a replacement key" box POSTs the
		// new key to /api/providers/{id}/keys/{keyId}/rotate.
		`+ "/rotate"`,
		`{api_key: apiKey}`,
		`Key rotated on `,
		// The modal reports last-probe results, never a fresh probe.
		`passed`,
		`Last probe results from GET /api/providers`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard Providers section missing %q", want)
		}
	}
	if strings.Contains(body, "Playground") {
		t.Errorf("dashboard must not contain a Playground")
	}
	// The section must not invent a per-provider on-demand probe
	// endpoint: there is none, Test-all re-reads /api/providers.
	if strings.Contains(body, "/api/providers/test") {
		t.Errorf("dashboard Providers section must not call a non-existent probe endpoint")
	}
	// The old unwired Connect toast must be gone — the button
	// POSTs to /api/providers/{id}/enabled now (critic fix
	// 2026-11-19).
	if strings.Contains(body, "Dashboard connect isn't wired yet") {
		t.Errorf("dashboard still carries the unwired connect toast — Connect is wired now")
	}
	// The old unwired rotate toast must be gone — Enter in the
	// replacement-key box POSTs to the rotate endpoint now
	// (critic fix 2026-11-19).
	if strings.Contains(body, "Key rotation from the dashboard isn't wired yet") {
		t.Errorf("dashboard still carries the unwired rotate toast — rotation is wired now")
	}
}

func TestDashboardM10_2CatalogFeed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/api/catalog", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/catalog status = %d, want 200", rec.Code)
	}
	var feed struct {
		Providers []map[string]any `json:"providers"`
		Total     int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("GET /api/catalog body is not JSON: %v", err)
	}
	if feed.Total <= 0 || len(feed.Providers) != feed.Total {
		t.Fatalf("catalog total = %d, providers = %d, want total > 0 and matching", feed.Total, len(feed.Providers))
	}
	for _, p := range feed.Providers {
		for _, field := range []string{"id", "name", "verification", "tos_grey"} {
			if _, ok := p[field]; !ok {
				t.Errorf("catalog entry %v missing field %q", p["id"], field)
			}
		}
	}
	// Feed fields the dashboard groups/filters on (M10 feed-field
	// slice): every entry carries has_free_tier (bool) and category
	// (non-empty string, straight from the catalog). At least one
	// catalog entry is a verified free tier, so the Free Tier group
	// has real rows to render. free_tier_note is deliberately NOT
	// in the feed (6 notes embed upstream URLs; the feed carries
	// no URL — see catalogEntryView).
	freeCount := 0
	for _, p := range feed.Providers {
		free, ok := p["has_free_tier"]
		if !ok {
			t.Errorf("catalog entry %v missing field %q", p["id"], "has_free_tier")
			continue
		}
		isFree, isBool := free.(bool)
		if !isBool {
			t.Errorf("catalog entry %v has_free_tier = %v (%T), want a bool", p["id"], free, free)
			continue
		}
		if isFree {
			freeCount++
		}
		cat, ok := p["category"]
		if !ok {
			t.Errorf("catalog entry %v missing field %q", p["id"], "category")
			continue
		}
		catStr, isStr := cat.(string)
		if !isStr || strings.TrimSpace(catStr) == "" {
			t.Errorf("catalog entry %v category = %v (%T), want a non-empty string", p["id"], cat, cat)
		}
		if _, ok := p["free_tier_note"]; ok {
			t.Errorf("catalog entry %v carries free_tier_note, want it absent (notes embed URLs; the feed carries no URL)", p["id"])
		}
	}
	if freeCount == 0 {
		t.Errorf("no catalog entry has has_free_tier = true, want at least one verified free tier")
	}
}

func TestDashboardM10_2ProvidersFeed(t *testing.T) {
	lister := fakeDashboardProviders{providers: []provider.DashboardProvider{
		{ID: "p1", Name: "Groq", Enabled: true, WireFormat: "openai", Verification: "verified-from-docs", AuthKind: "bearer", CatalogID: "groq", Models: 2},
		{ID: "p2", Name: "DeepSeek", Enabled: true, WireFormat: "anthropic", Verification: "from-reference-metadata-only", AuthKind: "x-api-key", CatalogID: "deepseek", Models: 1},
		{ID: "p3", Name: "My Custom", Enabled: false, WireFormat: "openai", Verification: "unverified", AuthKind: "bearer", Models: 0},
	}}
	healthLister := fakeHealthStatuses{statuses: []health.Status{
		{ProviderID: "p1", ProviderName: "Groq", OK: true},
		{ProviderID: "p2", ProviderName: "DeepSeek", OK: false, LastError: "boom"},
	}}
	srv := newTestServer(t, &fakeEngine{}, WithDashboardProviders(lister), WithHealthChecker(healthLister))
	rec := doReq(t, srv, http.MethodGet, "/api/providers", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/providers status = %d, want 200", rec.Code)
	}
	var feed struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("GET /api/providers body is not JSON: %v", err)
	}
	if feed.Providers == nil {
		t.Errorf("GET /api/providers providers = nil, want an (empty) array")
	}
	if len(feed.Providers) != 3 {
		t.Fatalf("GET /api/providers providers = %d, want 3", len(feed.Providers))
	}
	// last_error (M10 feed-field slice): the key is present on
	// every entry, taken from the health checker's last status —
	// the down provider's real error, honest "" for the ok provider
	// and the never-probed one. The error breakdown (Q18) reads it.
	wantErr := map[string]string{"p1": "", "p2": "boom", "p3": ""}
	for _, p := range feed.Providers {
		id, _ := p["id"].(string)
		raw, ok := p["last_error"]
		if !ok {
			t.Errorf("provider %v missing field %q", id, "last_error")
			continue
		}
		msg, isStr := raw.(string)
		if !isStr {
			t.Errorf("provider %v last_error = %v (%T), want a string", id, raw, raw)
			continue
		}
		if want, known := wantErr[id]; known && msg != want {
			t.Errorf("provider %v last_error = %q, want %q", id, msg, want)
		}
	}
}
