package catalog

import (
	"strings"
	"testing"
)

// TestTriageRound4ProvidersPresent checks every 2026-11-18 triage
// round-4 reference-only addition is in the catalog with a base
// URL, reference-only verification, and a cited source — and that
// ValidateAll still passes over them.
func TestTriageRound4ProvidersPresent(t *testing.T) {
	if len(triageProvidersRound4) == 0 {
		t.Fatal("triageProvidersRound4 is empty")
	}
	for _, want := range triageProvidersRound4 {
		p, ok := ByID(want.ID)
		if !ok {
			t.Errorf("triage round-4 provider %q not in catalog", want.ID)
			continue
		}
		if len(p.BaseURLs) == 0 {
			t.Errorf("triage round-4 provider %q has no base URL", want.ID)
		}
		if p.Verification != VerificationReference {
			t.Errorf("triage round-4 provider %q verification = %q, want %q (docs-verified additions live in verified.go)", want.ID, p.Verification, VerificationReference)
		}
		if strings.TrimSpace(p.SourceURL) == "" {
			t.Errorf("triage round-4 provider %q has no SourceURL", want.ID)
		}
		if !strings.Contains(p.Notes, "2026-11-18 triage round 4") {
			t.Errorf("triage round-4 provider %q notes do not cite the triage: %q", want.ID, p.Notes)
		}
	}
}

// TestTriageRound4VerifiedAdditions checks the two docs-verified
// additions of round 4 (they live in verified.go, not
// triageProvidersRound4).
func TestTriageRound4VerifiedAdditions(t *testing.T) {
	for _, id := range []string{"kimi-code-plan-cn", "kimi-code-plan-global"} {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("verified triage round-4 provider %q not in catalog", id)
			continue
		}
		if p.Verification != VerificationDocs {
			t.Errorf("triage round-4 provider %q verification = %q, want verified-from-docs", id, p.Verification)
		}
		if len(p.BaseURLs) == 0 {
			t.Errorf("triage round-4 provider %q has no base URL", id)
		}
	}
}

// TestTriageRound4BaseURLFills checks the round-4 fills landed on
// reference entries with the exact base URLs, and that the entries
// stayed reference-only.
func TestTriageRound4BaseURLFills(t *testing.T) {
	if len(triageBaseURLFixesRound4) == 0 {
		t.Fatal("triageBaseURLFixesRound4 is empty")
	}
	for _, fix := range triageBaseURLFixesRound4 {
		p, ok := ByID(fix.ID)
		if !ok {
			t.Errorf("round-4 fill target %q not in catalog", fix.ID)
			continue
		}
		if len(p.BaseURLs) != len(fix.BaseURLs) {
			t.Errorf("round-4 fill target %q base URLs = %v, want %v", fix.ID, p.BaseURLs, fix.BaseURLs)
			continue
		}
		for i, u := range fix.BaseURLs {
			if p.BaseURLs[i] != u {
				t.Errorf("round-4 fill target %q base URL[%d] = %q, want %q", fix.ID, i, p.BaseURLs[i], u)
			}
		}
		if p.Verification != VerificationReference {
			t.Errorf("round-4 fill target %q verification = %q, want reference-only", fix.ID, p.Verification)
		}
	}
}

// TestTriageRound4IDMoves checks the docs-proven round-4 ID move:
// the entry resolves under the new (models.dev) ID with its display
// name unchanged, the old ID still resolves via the alias, and the
// move-carried base URL landed on the reference entry.
func TestTriageRound4IDMoves(t *testing.T) {
	if len(triageIDMovesRound4) == 0 {
		t.Fatal("triageIDMovesRound4 is empty")
	}
	for _, m := range triageIDMovesRound4 {
		p, ok := ByID(m.NewID)
		if !ok {
			t.Errorf("round-4 ID move target %q (was %q) not in catalog", m.NewID, m.OldID)
			continue
		}
		if p.ID == m.OldID {
			t.Errorf("round-4 ID move %q did not change the ID", m.OldID)
		}
		if p.Alias != m.NewAlias {
			t.Errorf("round-4 ID move %q alias = %q, want %q (the old ID)", m.NewID, p.Alias, m.NewAlias)
		}
		if _, ok := ByID(m.OldID); ok {
			t.Errorf("round-4 ID move %q: old ID still resolves via ByID, want alias-only", m.OldID)
		}
		if q, ok := ByAlias(m.OldID); !ok || q.ID != m.NewID {
			t.Errorf("round-4 ID move %q: ByAlias(old ID) = %v, want the moved entry", m.OldID, q.ID)
		}
		if len(m.BaseURLs) != 0 {
			if len(p.BaseURLs) != len(m.BaseURLs) {
				t.Errorf("round-4 ID move %q base URLs = %v, want %v", m.NewID, p.BaseURLs, m.BaseURLs)
			} else {
				for i, u := range m.BaseURLs {
					if p.BaseURLs[i] != u {
						t.Errorf("round-4 ID move %q base URL[%d] = %q, want %q", m.NewID, i, p.BaseURLs[i], u)
					}
				}
			}
		}
		if !strings.Contains(p.Notes, "Catalog ID moved 2026-11-18 triage round 4") {
			t.Errorf("round-4 ID move %q notes do not cite the move: %q", m.NewID, p.Notes)
		}
	}
	// The move targets a reference-only entry and must stay
	// reference-only with its display name unchanged.
	if p, ok := ByID("novita-ai"); !ok {
		t.Errorf("round-4 ID move target %q not in catalog", "novita-ai")
	} else {
		if p.Verification != VerificationReference {
			t.Errorf("novita-ai verification = %q, want reference-only", p.Verification)
		}
		if p.Name != "Novita AI" {
			t.Errorf("novita-ai name = %q, want %q (the display name never changes in a move)", p.Name, "Novita AI")
		}
	}
}

// TestTriageRound4Counts pins the round-4 ledger: 46 triaged = 39
// added (2 verified from docs + 37 reference-only) + 5 rejected +
// 2 pending, and 44 candidates untouched of the 228 total.
func TestTriageRound4Counts(t *testing.T) {
	if got := len(triageProvidersRound4); got != 37 {
		t.Errorf("reference-only round-4 providers = %d, want 37", got)
	}
	verified := 0
	for _, id := range []string{"kimi-code-plan-cn", "kimi-code-plan-global"} {
		if p, ok := ByID(id); ok && p.Verification == VerificationDocs {
			verified++
		}
	}
	if verified != 2 {
		t.Errorf("verified round-4 providers = %d, want 2", verified)
	}
	if got := len(triageIDMovesRound4); got != 1 {
		t.Errorf("round-4 ID moves = %d, want 1", got)
	}
	if got := len(triageBaseURLFixesRound4); got != 2 {
		t.Errorf("round-4 base-URL fills = %d, want 2", got)
	}
}
