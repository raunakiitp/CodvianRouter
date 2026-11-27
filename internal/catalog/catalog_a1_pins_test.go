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
	const wantTotal = 607 // 609 before the Item-6 anyscale/kluster retirements
	nilByCategory := map[Category]int{
		CategoryAPIKey:        26, // 105 originally; 99 after A1 batch 1; 98 after D2; 92 after batch 2; 87 after batch 3; 84 after batch 4; 79 after batch 5; 75 after batch 6; 71 after batch 7; 69 after batch 8; 64 after batch 9; 60 after batch 10; 58 after batch 11; 55 after batch 12; 52 after batch 13; 50 after batch 14; 26 after batch 15
		CategoryWebCookie:     37,
		CategoryOAuth:         15, // 28 originally; 15 after the A3 item 7 batch (13 docs-verified fills; the 15 that remain have no documented base in their own docs)
		CategorySearch:        17, // 21 originally; 17 after batch 16 (exa, x-search, perplexity, kimi-search)
		CategoryLocal:         0,  // 13 originally; 5 after batch 16 (vllm, llama-cpp, xinference, docker-model-runner, lemonade, oobabooga, mlx-gemma, mlx-qwen); 0 after the A3 item 7 batch (opencode-server, llamafile, triton, sdwebui, comfyui)
		CategoryAudio:         10, // 12 originally; 10 after batch 16 (fishaudio, inworld)
		CategoryNoAuth:        10, // A3 item 5 (2026-11-27): all ten stay nil - the five CT-4 entry-403 rows must stay un-addable (ct4_test.go), the five CLI bridges own their endpoints (mechanisms in internal/noauth), and the three web surfaces have no documented endpoint
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

// A3 item 5 (2026-11-27) pins: the no-auth mechanisms behind the ten
// CategoryNoAuth entries (internal/noauth), pinned here from the
// catalog side so the rows cannot silently drift.

// A3 item 5 (2026-11-27) pin: the ten no-auth mechanisms live in
// internal/noauth (keyless endpoints, CLI bridges, documented limits);
// the catalog rows deliberately stay base-URL-less: the five CT-4
// entry-403 providers must remain un-addable (ct4_test.go), the five
// CLI bridges own their endpoints, and three web surfaces have no
// documented anonymous endpoint. This test pins the stay-nil shape so
// a future fill must go through the CT-4 pin deliberately.
func TestNoAuthRowsStayNilMechanismsInNoauthPkg(t *testing.T) {
	for _, id := range []string{
		"aihorde", "auggie", "cloudflare-playground", "codex-app-server",
		"devin-cli-agentic", "duckduckgo-web", "minimax-code", "uncloseai",
		"veoaifree-web", "zcode",
	} {
		p, ok := ByID(id)
		if !ok {
			t.Fatalf("%s: missing from the live catalog", id)
		}
		if len(p.BaseURLs) != 0 {
			t.Errorf("%s: BaseURLs = %v, want nil — no-auth rows keep no base URL; the connection mechanism lives in internal/noauth", id, p.BaseURLs)
		}
		if p.DefaultAuthKind() != AuthNone {
			t.Errorf("%s: DefaultAuthKind = %q, want none (the empty-key allowance the router applies)", id, p.DefaultAuthKind())
		}
		if reason := p.UnroutableReason(); !strings.Contains(reason, "not documented yet") && !strings.Contains(reason, "not yet") {
			t.Errorf("%s: unroutable reason = %q, want the pending-verification statement", id, reason)
		}
	}
}

// A3 item 7 (2026-11-27) pins: the OAuth + Local base-URL fills, each
// verified against the provider's own documentation.
var filledA3OAuth = map[string]string{
	"codex":       "https://chatgpt.com/backend-api/codex",
	"kimi-coding": "https://api.kimi.com/coding/v1",
	"kilocode":    "https://ai-gateway.kilo.ai/api/v1",
	"cline":       "https://api.cline.bot/api/v1",
	"openference": "https://api.openference.com/v1",
	"qoder":       "https://api.qoder.com/api/v1/cloud",
	"devin-cli":   "https://api.devin.ai/v1",
	"amazon-q":    "https://q.us-east-1.amazonaws.com",
	"kiro":        "https://q.us-east-1.amazonaws.com",
	"cursor":      "https://api2.cursor.sh",
	"ghe-copilot": "https://api.enterprise.githubcopilot.com",
	"gitlab-duo":  "https://{gitlab-instance}/api/v4",
	"renderhour":  "https://renderhour.com/api/mcp",
}

var filledA3Local = map[string]string{
	"opencode-server": "http://127.0.0.1:4096",
	"llamafile":       "http://localhost:8080/v1",
	"triton":          "http://localhost:9000/v1",
	"sdwebui":         "http://127.0.0.1:7860",
	"comfyui":         "http://127.0.0.1:8188",
}

// TestA3FillsEntriesFilled proves every entry filled in the A3 item 7
// batch kept its verified shape: the documented base URL, docs
// verification, a real documentation SourceURL, and a declared wire
// format.
func TestA3FillsEntriesFilled(t *testing.T) {
	for _, batch := range []map[string]string{filledA3OAuth, filledA3Local} {
		for id, wantURL := range batch {
			p, ok := ByID(id)
			if !ok {
				t.Errorf("%s: missing from the live catalog", id)
				continue
			}
			if len(p.BaseURLs) == 0 || p.BaseURLs[0] != wantURL {
				t.Errorf("%s: BaseURLs = %v, want first %s", id, p.BaseURLs, wantURL)
			}
			if p.Verification != VerificationDocs {
				t.Errorf("%s: Verification = %q, want docs-verified", id, p.Verification)
			}
			if !strings.HasPrefix(p.SourceURL, "https://") {
				t.Errorf("%s: SourceURL = %q, want the provider's own docs URL", id, p.SourceURL)
			}
			if len(p.WireFormats) == 0 {
				t.Errorf("%s: has a base URL but declares no wire format", id)
			}
			if p.Category != CategoryOAuth && p.Category != CategoryLocal {
				t.Errorf("%s: category = %q, want oauth or local", id, p.Category)
			}
		}
	}
}
