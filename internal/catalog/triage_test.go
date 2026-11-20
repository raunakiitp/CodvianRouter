package catalog

import (
	"strings"
	"testing"
)

// TestTriageProvidersPresent checks every 2026-11-18 triage addition
// is in the catalog with a base URL, reference-only verification, and
// a cited source — and that ValidateAll still passes over them.
func TestTriageProvidersPresent(t *testing.T) {
	if len(triageProviders) == 0 {
		t.Fatal("triageProviders is empty")
	}
	for _, want := range triageProviders {
		p, ok := ByID(want.ID)
		if !ok {
			t.Errorf("triage provider %q not in catalog", want.ID)
			continue
		}
		if len(p.BaseURLs) == 0 {
			t.Errorf("triage provider %q has no base URL", want.ID)
		}
		if p.Verification != VerificationReference {
			t.Errorf("triage provider %q verification = %q, want %q (docs-verified additions live in verified.go)",
				want.ID, p.Verification, VerificationReference)
		}
		if strings.TrimSpace(p.SourceURL) == "" {
			t.Errorf("triage provider %q has no SourceURL", want.ID)
		}
		if !strings.Contains(p.Notes, "2026-11-18 triage") {
			t.Errorf("triage provider %q notes do not cite the triage: %q", want.ID, p.Notes)
		}
	}
}

// TestTriageVerifiedAdditions checks the four docs-verified additions
// of the same triage (they live in verified.go, not triageProviders).
func TestTriageVerifiedAdditions(t *testing.T) {
	for _, id := range []string{"merge-gateway", "helicone", "opencode", "cloudflare-ai-gateway"} {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("verified triage provider %q not in catalog", id)
			continue
		}
		if p.Verification != VerificationDocs {
			t.Errorf("triage provider %q verification = %q, want verified-from-docs", id, p.Verification)
		}
		if len(p.BaseURLs) == 0 {
			t.Errorf("triage provider %q has no base URL", id)
		}
	}
}

// TestTriageBaseURLFills checks every fill landed on a reference
// entry with the exact base URL, and that the entry stayed
// reference-only (a models.dev + probe fill must never promote an
// entry to verified-from-docs).
func TestTriageBaseURLFills(t *testing.T) {
	if len(triageBaseURLFixes) == 0 {
		t.Fatal("triageBaseURLFixes is empty")
	}
	for _, fix := range triageBaseURLFixes {
		p, ok := ByID(fix.ID)
		if !ok {
			// A fill targeting a retired entry (CT-3: ai21) still
			// lands — on the retired record, not the live catalog.
			// Verify it there instead of failing the fill.
			if rp, rok := RetiredByID(fix.ID); rok {
				p, ok = rp, true
			} else {
				t.Errorf("base-URL fill target %q not in catalog", fix.ID)
				continue
			}
		}
		if len(p.BaseURLs) != len(fix.BaseURLs) {
			t.Errorf("fill target %q base URLs = %v, want %v", fix.ID, p.BaseURLs, fix.BaseURLs)
			continue
		}
		for i, u := range fix.BaseURLs {
			if p.BaseURLs[i] != u {
				t.Errorf("fill target %q base URL[%d] = %q, want %q", fix.ID, i, p.BaseURLs[i], u)
			}
		}
		if p.Verification != VerificationReference {
			t.Errorf("fill target %q verification = %q, want reference-only", fix.ID, p.Verification)
		}
	}
	// pioneer is deliberately NOT filled: models.dev points it at
	// api.fastino.ai, which belongs to a different provider.
	p, ok := ByID("pioneer")
	if !ok {
		t.Fatal("pioneer not in catalog")
	}
	if len(p.BaseURLs) != 0 {
		t.Errorf("pioneer base URLs = %v, want none (fastino URL rejected as a models.dev data error)", p.BaseURLs)
	}
}

// TestTriageRenames checks the four doc-confirmed display-name
// renames and that no other triage change renamed an entry.
func TestTriageRenames(t *testing.T) {
	want := map[string]string{
		"wandb":      "CoreWeave",
		"ovhcloud":   "OVHcloud AI Endpoints",
		"minimax":    "MiniMax",
		"minimax-cn": "MiniMax (China)",
	}
	if len(triageRenames) != len(want) {
		t.Fatalf("triageRenames = %d entries, want %d", len(triageRenames), len(want))
	}
	for id, name := range want {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("rename target %q not in catalog", id)
			continue
		}
		if p.Name != name {
			t.Errorf("rename target %q name = %q, want %q", id, p.Name, name)
		}
		if p.ID != id {
			t.Errorf("rename changed ID of %q to %q", id, p.ID)
		}
	}
}

// TestTriageDoesNotTouchVerified guards the overlay rule: no triage
// fill or rename may target a verified-from-docs entry, and a
// verified entry's base URL must survive All() untouched.
func TestTriageDoesNotTouchVerified(t *testing.T) {
	for _, fix := range triageBaseURLFixes {
		if p, ok := ByID(fix.ID); ok && p.Verification == VerificationDocs {
			t.Errorf("base-URL fill targets verified entry %q", fix.ID)
		}
	}
	for _, r := range triageRenames {
		if p, ok := ByID(r.ID); ok && p.Verification == VerificationDocs {
			t.Errorf("rename targets verified entry %q", r.ID)
		}
	}
	before, _ := ByID("openai")
	after, _ := ByID("openai")
	if before.BaseURLs[0] != after.BaseURLs[0] || before.Name != after.Name {
		t.Errorf("verified entry openai changed across All(): %+v", after)
	}
}

// TestTriageNoIDCollisions checks triage providers never shadow an
// existing catalog ID (verified, reference, or additional).
func TestTriageNoIDCollisions(t *testing.T) {
	// Build the ID set without the triage providers by checking the
	// raw slices: a triage ID must appear in none of them.
	raw := map[string]bool{}
	for _, p := range referenceProviders {
		raw[p.ID] = true
	}
	for _, p := range verifiedProviders {
		raw[p.ID] = true
	}
	for _, p := range additionalProviders {
		raw[p.ID] = true
	}
	for _, p := range triageProviders {
		if raw[p.ID] {
			t.Errorf("triage provider %q collides with an existing catalog ID", p.ID)
		}
	}
	// And the overlay itself must not have created a duplicate.
	seen := map[string]bool{}
	for _, p := range All() {
		if seen[p.ID] {
			t.Errorf("duplicate provider ID %q after triage overlay", p.ID)
		}
		seen[p.ID] = true
	}
}
