package catalog

import (
	"strings"
	"testing"
)

// G7 — Area 3 catalog gaps (parity tracker Area 3). One test per row.
// Exact counts below are the catalog as measured 2026-11-19; a
// deliberate catalog change (triage round, new provider) must
// update them here, never silently.

// TestVerifiedOverlayPreservesReferenceFreeTier — parity tracker:518,
// WRONG row: All() replaced the whole entry with the verified one,
// dropping HasFreeTier for 11 providers. The overlay now merges:
// verified facts win, reference-only facts (free tier, notes)
// survive. cloudflare-ai is checked under its post-move ID
// cloudflare-workers-ai (triage round 3); novita-ai is a control
// (reference-only, renamed in round 4, never overlaid).
func TestVerifiedOverlayPreservesReferenceFreeTier(t *testing.T) {
	// merged ID -> reference ID (two entries were renamed by
	// triage ID moves after the overlay).
	refID := map[string]string{
		"cerebras": "cerebras", "cloudflare-workers-ai": "cloudflare-ai",
		"deepseek": "deepseek", "fireworks": "fireworks",
		"gemini": "gemini", "groq": "groq", "mistral": "mistral",
		"nvidia": "nvidia", "ollama-cloud": "ollama-cloud",
		"openrouter": "openrouter", "vertex": "vertex",
		"novita-ai": "novita",
	}
	for id, rid := range refID {
		p, ok := ByID(id)
		if !ok {
			t.Fatalf("%q not in catalog", id)
		}
		if !p.HasFreeTier {
			t.Fatalf("%q lost HasFreeTier in the verified overlay (WRONG-row regression)", id)
		}
		// The note survives verbatim — including an empty note
		// (ollama-cloud, vertex record a free tier with no note).
		var refNote string
		for _, rp := range referenceProviders {
			if rp.ID == rid {
				refNote = rp.FreeTierNote
			}
		}
		if p.FreeTierNote != refNote {
			t.Fatalf("%q FreeTierNote = %q, want the reference note %q", id, p.FreeTierNote, refNote)
		}
	}
	// The notes survive verbatim, not just the flag.
	if got, _ := ByID("groq"); !strings.Contains(got.FreeTierNote, "Free plan") {
		t.Fatalf("groq FreeTierNote = %q, want the reference note", got.FreeTierNote)
	}
	if got, _ := ByID("openrouter"); !strings.Contains(got.FreeTierNote, ":free") {
		t.Fatalf("openrouter FreeTierNote = %q, want the reference note", got.FreeTierNote)
	}
	// …and the verified facts still win where the verified entry
	// states them (the merge is not a revert to reference data).
	if got, _ := ByID("groq"); len(got.BaseURLs) == 0 || got.BaseURLs[0] != "https://api.groq.com/openai/v1" {
		t.Fatalf("groq base URLs = %v, want the verified base", got.BaseURLs)
	}
	if got, _ := ByID("openai"); got.Verification != VerificationDocs {
		t.Fatalf("openai verification = %q, want verified-from-docs", got.Verification)
	}
}

// TestBaseURLCoverage — parity tracker:500, PARTIAL row: 380/614 entries
// have a base URL. The coverage API is the honest measurement:
// routable (concrete) vs template-only vs absent, with the real
// gap isolated as API-capable providers without a base URL.
func TestBaseURLCoverage(t *testing.T) {
	cov := MeasureBaseURLCoverage()
	if cov.Total != Count() {
		t.Fatalf("coverage total = %d, catalog count = %d", cov.Total, Count())
	}
	// CT-8: the headline total is pinned so README/docs cannot drift
	// silently again — 614 at triage round 5 minus 4 CT-3 retirements.
	if cov.Total != 609 {
		t.Fatalf("catalog total = %d, want 609 (614 at triage round 5, minus 4 CT-3 retirements and the D2 cheaperinference retirement)", cov.Total)
	}
	// A1 batches 1-16 (2026-11-27): seventy-eight API-key entries gained a
	// docs-verified base URL (deepinfra, hyperbolic, nscale, lambda-ai, featherless-ai, moonshot, yi, qianfan, heroku,
	// voyage-ai, llm7, jina-ai, oneminai, charm-hyper, dahl, freetheai, freemodel-dev,
	// naga-ac, openference-api, openvecta, glm-cn, glmt, gitlawb, gitlawb-gmi,
	// xiaomi-mimo-token-plan, volcengine-agent-plan, hcnsec, agnes-cn, apmix, kie,
	// baidu, 360ai, kimi-coding-apikey, sparkdesk, qwen-cloud-token-plan, vertex-partner),
	// all concrete except vertex-partner's documented {LOCATION} template — with
	// 457 / routable 444 / template-only 13; batch 16 added fourteen Item-2
	// rows (two audio, four search, eight local), all loopback/concrete — with
	// 471 / routable 458 / template-only 13. The 609 total is unchanged because
	// each fill overlays an existing reference ID.
	if cov.WithBaseURL != 471 || cov.Routable != 458 || cov.TemplateOnly != 13 {
		t.Fatalf("coverage = with %d / routable %d / template-only %d, want 471 / 458 / 13 (457 / 444 / 13 after A1 batch 15; 379 / 370 / 9 originally)",
			cov.WithBaseURL, cov.Routable, cov.TemplateOnly)
	}
	if cov.WithBaseURL+cov.WithoutByCategory[CategoryAPIKey]+cov.WithoutByCategory[CategoryAudio]+
		cov.WithoutByCategory[CategoryCloudAgent]+cov.WithoutByCategory[CategoryLocal]+
		cov.WithoutByCategory[CategoryNoAuth]+cov.WithoutByCategory[CategoryOAuth]+
		cov.WithoutByCategory[CategorySearch]+cov.WithoutByCategory[CategoryUpstreamProxy]+
		cov.WithoutByCategory[CategoryWebCookie] != cov.Total {
		t.Fatalf("coverage parts do not sum to total: %+v", cov)
	}
	if cov.Routable+cov.TemplateOnly != cov.WithBaseURL {
		t.Fatalf("routable %d + template-only %d != with-base-URL %d", cov.Routable, cov.TemplateOnly, cov.WithBaseURL)
	}
	// A1 batches 1-15 filled 78 of the 105 api-key entries and batch 16
	// filled six of the audio/search rows, so the API-capable-without-base-URL
	// figure drops 138 -> 53 (api-key 26 + audio 10 + search 17).
	if cov.APICapableWithout != 53 {
		t.Fatalf("API-capable without base URL = %d, want 53 (api-key 26 + audio 10 + search 17; 138 originally)", cov.APICapableWithout)
	}
	if cov.WithoutByCategory[CategoryAPIKey] != 26 {
		t.Fatalf("api-key without base URL = %d, want 26 (105 originally; 99 after batch 1; 98 after D2; 92 after batch 2; 87 after batch 3; 84 after batch 4; 79 after batch 5; 75 after batch 6; 71 after batch 7; 69 after batch 8; 64 after batch 9; 60 after batch 10; 58 after batch 11; 55 after batch 12; 52 after batch 13; 50 after batch 14; 26 after batch 15)", cov.WithoutByCategory[CategoryAPIKey])
	}
}

func TestProviderRoutable(t *testing.T) {
	cases := []struct {
		urls []string
		want bool
	}{
		{[]string{"https://api.example.com/v1"}, true},
		{[]string{"http://localhost:11434"}, true}, // loopback serves locally
		{[]string{"https://{resource}.openai.azure.com/openai/v1/"}, false},
		{[]string{"https://{LOCATION}-aiplatform.googleapis.com/v1"}, false},
		{[]string{"https://{resource}.example.com", "https://api.example.com/v1"}, true}, // one concrete base suffices
		{[]string{}, false},
		{[]string{"not a url"}, false},
	}
	for _, tc := range cases {
		if got := (Provider{BaseURLs: tc.urls}).Routable(); got != tc.want {
			t.Fatalf("Routable(%v) = %v, want %v", tc.urls, got, tc.want)
		}
	}
}

// TestStaticModelsField — parity tracker:503, PARTIAL row: no static
// per-provider model list existed. The field, accessor and
// deep-copy now exist; the seed data itself is still empty
// (populating it needs a models.dev snapshot import — a data
// task, not invented IDs), so this test proves the mechanism.
func TestStaticModelsField(t *testing.T) {
	p := Provider{ID: "x", Name: "X", Models: []string{"a-1", "b-2"}}
	if got := p.StaticModels(); len(got) != 2 || got[0] != "a-1" {
		t.Fatalf("StaticModels = %v", got)
	}
	// StaticModels returns a copy, and Clone deep-copies the list.
	got := p.StaticModels()
	got[0] = "tampered"
	if p.Models[0] != "a-1" {
		t.Fatal("StaticModels shares the list with the provider")
	}
	c := p.Clone()
	c.Models[0] = "tampered"
	if p.Models[0] != "a-1" {
		t.Fatal("Clone shares the Models list with the original")
	}
	// Validation rejects an empty model ID.
	if err := (Provider{ID: "x", Name: "X", Models: []string{"ok", ""}}).Validate(); err == nil {
		t.Fatal("empty static model ID validated without error")
	}
}

// TestFreeTierProviders — parity tracker:511, PARTIAL row: HasFreeTier
// existed but was surfaced nowhere. The catalog side now exposes
// the free-tier set; server/dashboard surfacing is a follow-up
// (server files are another builder's, dashboard is M10).
func TestFreeTierProviders(t *testing.T) {
	if got := FreeTierCount(); got != 162 {
		t.Fatalf("FreeTierCount = %d, want 162 (157 reference + 4 additional + 1 verified-only; CT-3 retired blackbox, ai21, bytez out of the 165)", got)
	}
	for _, p := range FreeTierProviders() {
		if !p.HasFreeTier {
			t.Fatalf("FreeTierProviders returned %q without HasFreeTier", p.ID)
		}
	}
}

// TestFreeTierPoolsAndRanking — parity tracker:512, MISSING row: no
// pools, no budget aggregation, no rankings. Pools group the
// verified free-tier set by category and the ranking orders it by
// evidence strength (docs-verified first). No token budget is
// aggregated: no verified per-provider free budget exists to
// aggregate, and inventing one would breach the honesty rules.
func TestFreeTierPoolsAndRanking(t *testing.T) {
	pools := FreeTierPools()
	if len(pools) != 7 {
		t.Fatalf("free-tier pools = %d, want 7", len(pools))
	}
	sum := 0
	for _, pool := range pools {
		if len(pool.Providers) == 0 {
			t.Fatalf("pool %q is empty", pool.Name)
		}
		for _, p := range pool.Providers {
			if string(p.Category) != pool.Name || !p.HasFreeTier {
				t.Fatalf("pool %q contains %q (category %q, free=%v)", pool.Name, p.ID, p.Category, p.HasFreeTier)
			}
		}
		sum += len(pool.Providers)
	}
	if sum != FreeTierCount() {
		t.Fatalf("pools hold %d providers, free-tier count is %d", sum, FreeTierCount())
	}
	s := SummarizeFreeTier()
	// VerifiedDocs moved 18 -> 51 with the A1 batch fills (33
	// free-tier entries became docs-verified across batches 1-16; their
	// free-tier notes were already present and unchanged).
	if s.Total != 162 || s.Pools != 7 || s.VerifiedDocs != 51 || s.WithNote != 145 {
		t.Fatalf("free-tier summary = %+v, want {Total 162, VerifiedDocs 51, WithNote 145, Pools 7} (CT-3 retired 3 free-tier entries, all with notes; A1 batches added 33 docs-verified)", s)
	}
	ranked := FreeTierRanking()
	if len(ranked) != FreeTierCount() {
		t.Fatalf("ranking holds %d, want %d", len(ranked), FreeTierCount())
	}
	if ranked[0].Verification != VerificationDocs {
		t.Fatalf("ranking starts with %q (%s), want a docs-verified entry first", ranked[0].ID, ranked[0].Verification)
	}
	again := FreeTierRanking()
	for i := range ranked {
		if ranked[i].ID != again[i].ID {
			t.Fatal("FreeTierRanking is not deterministic across calls")
		}
	}
}

// TestKeyCreationURLs — parity tracker:513, MISSING row: no field, no
// API exposure. The field exists now, seeded with 18 key pages
// from the reference router reference hints (see keycreation.go); the rest
// need per-provider research, and API/dashboard exposure is a
// follow-up (server is another builder's, dashboard is M10).
func TestKeyCreationURLs(t *testing.T) {
	n := 0
	for _, p := range All() {
		if p.KeyCreationURL != "" {
			n++
			if !strings.HasPrefix(p.KeyCreationURL, "https://") {
				t.Fatalf("%q key URL = %q, want https", p.ID, p.KeyCreationURL)
			}
			if p.KeyCreationURL == strings.TrimSuffix(p.Website, "/") {
				t.Fatalf("%q key URL duplicates its Website; the seed must only record key pages", p.ID)
			}
		}
	}
	if n != 18 {
		t.Fatalf("entries with a key-creation URL = %d, want 18", n)
	}
	for id, want := range map[string]string{
		"beatapi":               "https://beatapi.io/dashboard/apikeys",
		"magnific":              "https://magnific.com/user/api-keys",
		"oneminai":              "https://docs.1min.ai/docs/api/create-api-key",
		"cloudflare-workers-ai": "https://dash.cloudflare.com",
	} {
		got, ok := KeyCreationURLFor(id)
		if !ok || got != want {
			t.Fatalf("KeyCreationURLFor(%q) = %q, %v; want %q", id, got, ok, want)
		}
	}
	if _, ok := KeyCreationURLFor("openai"); ok {
		t.Fatal("openai has a key-creation URL, but none was seeded for it")
	}
	if _, ok := KeyCreationURLFor("no-such-provider"); ok {
		t.Fatal("KeyCreationURLFor returned a URL for an unknown provider")
	}
}
