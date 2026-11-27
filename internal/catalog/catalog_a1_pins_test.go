package catalog

// A1 (Gap 1) pins: catalog fill progress and catalog-shape drift.
//
// These numbers are updated DELIBERATELY as each base-URL batch lands.
// They exist so that a silent regression — an entry losing its URL, a
// category growing unroutable entries, a filled entry losing its
// docs-verified status — fails the build instead of shipping.

import (
	"strings"
	"testing"
)

// filledA1Batch1 are the entries filled in the first A1 base-URL batch,
// each against the provider's own documentation (2026-11-27).
var filledA1Batch1 = map[string]string{
	"deepinfra":  "https://api.deepinfra.com/v1/openai",
	"hyperbolic": "https://api.hyperbolic.ai/inference/v1",
	"nscale":     "https://inference.api.nscale.com/v1",
	"voyage-ai":  "https://api.voyageai.com/v1",
	"llm7":       "https://api.llm7.io/v1",
	"jina-ai":    "https://api.jina.ai/v1",
}

// TestA1Batch1EntriesFilled proves every entry filled in this batch
// kept its verified shape: the documented base URL, docs verification,
// and a real documentation SourceURL (never "reference-metadata").
func TestA1Batch1EntriesFilled(t *testing.T) {
	byID := make(map[string]Provider, len(All()))
	for _, p := range All() {
		byID[p.ID] = p
	}
	for id, wantURL := range filledA1Batch1 {
		p, ok := byID[id]
		if !ok {
			t.Errorf("%s: missing from the live catalog", id)
			continue
		}
		if len(p.BaseURLs) != 1 || p.BaseURLs[0] != wantURL {
			t.Errorf("%s: BaseURLs = %v, want exactly [%s]", id, p.BaseURLs, wantURL)
		}
		if p.Verification != VerificationDocs {
			t.Errorf("%s: Verification = %q, want docs-verified", id, p.Verification)
		}
		if !strings.HasPrefix(p.SourceURL, "https://") || p.SourceURL == "reference-metadata" {
			t.Errorf("%s: SourceURL = %q, want the provider's own docs URL", id, p.SourceURL)
		}
		if len(p.Endpoints) == 0 {
			t.Errorf("%s: no endpoints declared — a filled entry must say which surfaces it serves", id)
		}
		for _, e := range p.Endpoints {
			if e.AuthKind == "" {
				t.Errorf("%s: endpoint %q has no auth kind", id, e.Endpoint)
			}
		}
	}
}

// TestCatalogShapePins pins the live catalog totals so drift is loud.
// Update these numbers in the same commit that changes the catalog,
// with the reason in the commit message.
func TestCatalogShapePins(t *testing.T) {
	const wantTotal = 609
	nilByCategory := map[Category]int{
		CategoryAPIKey:        87, // 105 originally; 99 after A1 batch 1; 98 after D2; 92 after batch 2; 87 after batch 3
		CategoryWebCookie:     37,
		CategoryOAuth:         28,
		CategorySearch:        21,
		CategoryLocal:         13,
		CategoryAudio:         12,
		CategoryNoAuth:        10,
		CategoryCloudAgent:    3,
		CategoryUpstreamProxy: 2,
	}
	all := All()
	if len(all) != wantTotal {
		t.Fatalf("live catalog = %d entries, want %d (a fill must overlay an existing ID, not add one)", len(all), wantTotal)
	}
	got := map[Category]int{}
	for _, p := range all {
		if len(p.BaseURLs) == 0 {
			got[p.Category]++
		}
	}
	total := 0
	for c, want := range nilByCategory {
		if got[c] != want {
			t.Errorf("category %q: %d entries without a base URL, want %d — update the pin deliberately if this is intended", c, got[c], want)
		}
		total += want
	}
	sum := 0
	for _, n := range got {
		sum += n
	}
	if sum != total {
		t.Errorf("live nil base-URL entries = %d, want %d across all categories", sum, total)
	}
}

// TestFilledEntriesHaveDocsSource is the general honesty rule for the
// whole catalog: any entry that claims docs verification must cite a
// source that is not the generic reference-metadata placeholder (an
// external docs URL must be https), and any entry with a base URL must
// declare at least one wire format (otherwise it cannot be dialed).
// Codvian's own preset entry legitimately cites the internal registry
// as its source — it has no third-party docs to point at.
func TestFilledEntriesHaveDocsSource(t *testing.T) {
	for _, p := range All() {
		if p.Verification == VerificationDocs {
			src := strings.TrimSpace(p.SourceURL)
			if src == "" || src == "reference-metadata" {
				t.Errorf("%s: docs-verified but SourceURL = %q", p.ID, src)
			}
			if strings.HasPrefix(src, "http://") {
				t.Errorf("%s: docs source must be https, got %q", p.ID, src)
			}
		}
		if len(p.BaseURLs) > 0 && len(p.WireFormats) == 0 {
			t.Errorf("%s: has a base URL but declares no wire format", p.ID)
		}
	}
}
