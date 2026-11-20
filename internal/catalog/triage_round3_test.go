package catalog

import (
	"strings"
	"testing"
)

// TestTriageRound3ProvidersPresent checks every 2026-11-18 triage
// round-3 reference-only addition is in the catalog with a base
// URL, reference-only verification, and a cited source — and that
// ValidateAll still passes over them.
func TestTriageRound3ProvidersPresent(t *testing.T) {
	if len(triageProvidersRound3) == 0 {
		t.Fatal("triageProvidersRound3 is empty")
	}
	for _, want := range triageProvidersRound3 {
		p, ok := ByID(want.ID)
		if !ok {
			t.Errorf("triage round-3 provider %q not in catalog", want.ID)
			continue
		}
		if len(p.BaseURLs) == 0 {
			t.Errorf("triage round-3 provider %q has no base URL", want.ID)
		}
		if p.Verification != VerificationReference {
			t.Errorf("triage round-3 provider %q verification = %q, want %q (docs-verified additions live in verified.go)",
				want.ID, p.Verification, VerificationReference)
		}
		if strings.TrimSpace(p.SourceURL) == "" {
			t.Errorf("triage round-3 provider %q has no SourceURL", want.ID)
		}
		if !strings.Contains(p.Notes, "2026-11-18 triage round 3") {
			t.Errorf("triage round-3 provider %q notes do not cite the triage: %q", want.ID, p.Notes)
		}
	}
}

// TestTriageRound3VerifiedAdditions checks the three docs-verified
// additions of round 3 (they live in verified.go, not
// triageProvidersRound3).
func TestTriageRound3VerifiedAdditions(t *testing.T) {
	for _, id := range []string{"klavo", "huawei-maas", "stackit"} {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("verified triage round-3 provider %q not in catalog", id)
			continue
		}
		if p.Verification != VerificationDocs {
			t.Errorf("triage round-3 provider %q verification = %q, want verified-from-docs", id, p.Verification)
		}
		if len(p.BaseURLs) == 0 {
			t.Errorf("triage round-3 provider %q has no base URL", id)
		}
	}
}

// TestTriageRound3BaseURLFills checks the round-3 fill landed on a
// reference entry with the exact base URL, and that the entry stayed
// reference-only.
func TestTriageRound3BaseURLFills(t *testing.T) {
	if len(triageBaseURLFixesRound3) == 0 {
		t.Fatal("triageBaseURLFixesRound3 is empty")
	}
	for _, fix := range triageBaseURLFixesRound3 {
		p, ok := ByID(fix.ID)
		if !ok {
			t.Errorf("round-3 fill target %q not in catalog", fix.ID)
			continue
		}
		if len(p.BaseURLs) != len(fix.BaseURLs) {
			t.Errorf("round-3 fill target %q base URLs = %v, want %v", fix.ID, p.BaseURLs, fix.BaseURLs)
			continue
		}
		for i, u := range fix.BaseURLs {
			if p.BaseURLs[i] != u {
				t.Errorf("round-3 fill target %q base URL[%d] = %q, want %q", fix.ID, i, p.BaseURLs[i], u)
			}
		}
		if p.Verification != VerificationReference {
			t.Errorf("round-3 fill target %q verification = %q, want reference-only", fix.ID, p.Verification)
		}
	}
}

// TestTriageRound3IDMoves checks every docs-proven round-3 ID move:
// the entry resolves under the new (models.dev) ID with its display
// name unchanged, the old ID still resolves via the alias, and a
// move-carried base URL landed on the reference entry.
func TestTriageRound3IDMoves(t *testing.T) {
	if len(triageIDMovesRound3) == 0 {
		t.Fatal("triageIDMovesRound3 is empty")
	}
	for _, m := range triageIDMovesRound3 {
		p, ok := ByID(m.NewID)
		if !ok {
			t.Errorf("round-3 ID move target %q (was %q) not in catalog", m.NewID, m.OldID)
			continue
		}
		if p.ID == m.OldID {
			t.Errorf("round-3 ID move %q did not change the ID", m.OldID)
		}
		if p.Alias != m.NewAlias {
			t.Errorf("round-3 ID move %q alias = %q, want %q (the old ID)", m.NewID, p.Alias, m.NewAlias)
		}
		if _, ok := ByID(m.OldID); ok {
			t.Errorf("round-3 ID move %q: old ID still resolves via ByID, want alias-only", m.OldID)
		}
		if q, ok := ByAlias(m.OldID); !ok || q.ID != m.NewID {
			t.Errorf("round-3 ID move %q: ByAlias(old ID) = %v, want the moved entry", m.OldID, q.ID)
		}
		if len(m.BaseURLs) != 0 {
			if len(p.BaseURLs) != len(m.BaseURLs) {
				t.Errorf("round-3 ID move %q base URLs = %v, want %v", m.NewID, p.BaseURLs, m.BaseURLs)
			} else {
				for i, u := range m.BaseURLs {
					if p.BaseURLs[i] != u {
						t.Errorf("round-3 ID move %q base URL[%d] = %q, want %q", m.NewID, i, p.BaseURLs[i], u)
					}
				}
			}
		}
		if !strings.Contains(p.Notes, "Catalog ID moved 2026-11-18 triage round 3") {
			t.Errorf("round-3 ID move %q notes do not cite the move: %q", m.NewID, p.Notes)
		}
	}
	// The cloudflare move targets a verified-from-docs entry that
	// already carries the same templated base: the move must not
	// downgrade it or replace the base.
	if p, ok := ByID("cloudflare-workers-ai"); !ok {
		t.Errorf("round-3 ID move target %q not in catalog", "cloudflare-workers-ai")
	} else {
		if p.Verification != VerificationDocs {
			t.Errorf("cloudflare-workers-ai verification = %q, want verified-from-docs (the move must not downgrade it)", p.Verification)
		}
		if len(p.BaseURLs) != 1 || p.BaseURLs[0] != "https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1" {
			t.Errorf("cloudflare-workers-ai base URLs = %v, want the verified {account_id} template unchanged", p.BaseURLs)
		}
		if p.Name != "Cloudflare Workers AI" {
			t.Errorf("cloudflare-workers-ai name = %q, want the display name unchanged", p.Name)
		}
	}
	// The other three moves target reference-only entries and must
	// stay reference-only with their display names unchanged.
	for id, wantName := range map[string]string{
		"snowflake-cortex":   "Snowflake Cortex",
		"github-copilot":     "GitHub Copilot",
		"alibaba-token-plan": "Alibaba Token Plan",
	} {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("round-3 ID move target %q not in catalog", id)
			continue
		}
		if p.Verification != VerificationReference {
			t.Errorf("%s verification = %q, want reference-only", id, p.Verification)
		}
		if p.Name != wantName {
			t.Errorf("%s name = %q, want %q (the display name never changes in a move)", id, p.Name, wantName)
		}
	}
}

// TestTriageRound3Counts pins the round-3 ledger: 46 triaged = 34
// added (3 verified from docs + 31 reference-only) + 9 rejected +
// 3 pending, and 90 candidates untouched of the 228 total.
func TestTriageRound3Counts(t *testing.T) {
	if got := len(triageProvidersRound3); got != 31 {
		t.Errorf("reference-only round-3 providers = %d, want 31", got)
	}
	verified := 0
	for _, id := range []string{"klavo", "huawei-maas", "stackit"} {
		if p, ok := ByID(id); ok && p.Verification == VerificationDocs {
			verified++
		}
	}
	if verified != 3 {
		t.Errorf("verified round-3 providers = %d, want 3", verified)
	}
	if got := len(triageIDMovesRound3); got != 4 {
		t.Errorf("round-3 ID moves = %d, want 4", got)
	}
}
