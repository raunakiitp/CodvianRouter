package catalog

import (
	"strings"
	"testing"
)

func TestValidateAll(t *testing.T) {
	if err := ValidateAll(); err != nil {
		t.Fatalf("ValidateAll: %v", err)
	}
}

func TestCountExceedsReference(t *testing.T) {
	// the sector baseline documents 394 providers (README, Nov 2026). The
	// catalog must hold MORE than that, plus custom-provider support.
	if got := Count(); got <= 394 {
		t.Fatalf("catalog count = %d, want > 394 (the sector-typical count)", got)
	}
}

func TestNoDuplicateNormalizedNames(t *testing.T) {
	// Catches a verified overlay whose ID does not match the reference
	// entry it was meant to replace: the catalog would then hold the
	// provider twice under two IDs.
	seen := map[string]string{}
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	for _, p := range All() {
		n := norm(p.Name)
		if prev, ok := seen[n]; ok && prev != p.ID {
			t.Fatalf("duplicate provider name %q under IDs %q and %q (overlay ID mismatch?)", p.Name, prev, p.ID)
		}
		seen[n] = p.ID
	}
}

func TestCountByVerificationAddsUp(t *testing.T) {
	counts := CountByVerification()
	total := 0
	for _, n := range counts {
		total += n
	}
	if total != Count() {
		t.Fatalf("verification counts sum to %d, catalog count is %d", total, Count())
	}
	// 21 research-verified providers + Claude Code OAuth + Codvian.
	if counts[VerificationDocs] < 23 {
		t.Fatalf("verified-from-docs = %d, want >= 23", counts[VerificationDocs])
	}
	if counts[VerificationReference] == 0 {
		t.Fatal("no from-reference-metadata-only entries; reference import is empty")
	}
}

func TestVerifiedEntriesHaveBaseURLs(t *testing.T) {
	for _, p := range All() {
		if p.Verification == VerificationDocs && len(p.BaseURLs) == 0 {
			t.Fatalf("verified provider %q has no base URL", p.ID)
		}
	}
}

func TestVerifiedOverlayReplacesReference(t *testing.T) {
	p, ok := ByID("openai")
	if !ok {
		t.Fatal("openai not in catalog")
	}
	if p.Verification != VerificationDocs {
		t.Fatalf("openai verification = %q, want verified-from-docs", p.Verification)
	}
	if len(p.BaseURLs) == 0 || p.BaseURLs[0] != "https://api.openai.com/v1" {
		t.Fatalf("openai base URLs = %v", p.BaseURLs)
	}
	// Anthropic's verified entry must carry the current version header.
	a, ok := ByID("anthropic")
	if !ok {
		t.Fatal("anthropic not in catalog")
	}
	ep, ok := a.Endpoint(EndpointMessages, "")
	if !ok {
		t.Fatal("anthropic has no messages endpoint")
	}
	if ep.ExtraHeaders["anthropic-version"] != AnthropicVersion {
		t.Fatalf("anthropic-version = %q, want %q", ep.ExtraHeaders["anthropic-version"], AnthropicVersion)
	}
	if AnthropicVersion == "2023-06-01" {
		t.Fatal("anthropic-version is the superseded 2023-06-01 value")
	}
}

func TestByAlias(t *testing.T) {
	for alias, wantID := range map[string]string{
		"azure": "azure-openai",
		"cc":    "claude",
		"hf":    "huggingface",
	} {
		p, ok := ByAlias(alias)
		if !ok {
			t.Fatalf("alias %q not found", alias)
		}
		if p.ID != wantID {
			t.Fatalf("alias %q -> %q, want %q", alias, p.ID, wantID)
		}
	}
}

func TestSearch(t *testing.T) {
	got := Search("open")
	found := map[string]bool{}
	for _, p := range got {
		found[p.ID] = true
	}
	for _, want := range []string{"openai", "openrouter"} {
		if !found[want] {
			t.Fatalf("search %q missing %q (got %d results)", "open", want, len(got))
		}
	}
	if len(Search("")) != Count() {
		t.Fatal("empty search must return the whole catalog")
	}
}

func TestByCategoryCoversReferenceBreakdown(t *testing.T) {
	// the sector-typical own breakdown: 233 API key, 25 OAuth, 35 web-cookie,
	// 14 local, 13 no-auth (+ search/audio/proxy/cloud/system). Our
	// catalog must hold each family, not just API-key providers.
	for _, c := range []Category{CategoryAPIKey, CategoryOAuth, CategoryLocal, CategoryNoAuth, CategoryWebCookie} {
		if n := len(ByCategory(c)); n == 0 {
			t.Fatalf("no providers in category %q", c)
		}
	}
}

func TestToSGreyProvidersAreFlagged(t *testing.T) {
	for _, p := range ByCategory(CategoryWebCookie) {
		if !p.ToSGrey {
			t.Fatalf("web-cookie provider %q is not flagged ToSGrey", p.ID)
		}
	}
}

func TestProviderCloneIsDeep(t *testing.T) {
	p, _ := ByID("anthropic")
	c := p.Clone()
	c.Endpoints[0].ExtraHeaders["anthropic-version"] = "tampered"
	again, _ := ByID("anthropic")
	if again.Endpoints[0].ExtraHeaders["anthropic-version"] == "tampered" {
		t.Fatal("Clone shares ExtraHeaders map with the catalog")
	}
}
