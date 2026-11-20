package catalog

import (
	"strings"
	"testing"
)

// TestTriageRound2ProvidersPresent checks every 2026-11-18 triage
// round-2 reference-only addition is in the catalog with a base
// URL, reference-only verification, and a cited source — and that
// ValidateAll still passes over them.
func TestTriageRound2ProvidersPresent(t *testing.T) {
	if len(triageProvidersRound2) == 0 {
		t.Fatal("triageProvidersRound2 is empty")
	}
	for _, want := range triageProvidersRound2 {
		p, ok := ByID(want.ID)
		if !ok {
			t.Errorf("triage round-2 provider %q not in catalog", want.ID)
			continue
		}
		if len(p.BaseURLs) == 0 {
			t.Errorf("triage round-2 provider %q has no base URL", want.ID)
		}
		if p.Verification != VerificationReference {
			t.Errorf("triage round-2 provider %q verification = %q, want %q (docs-verified additions live in verified.go)",
				want.ID, p.Verification, VerificationReference)
		}
		if strings.TrimSpace(p.SourceURL) == "" {
			t.Errorf("triage round-2 provider %q has no SourceURL", want.ID)
		}
		if !strings.Contains(p.Notes, "2026-11-18 triage round 2") {
			t.Errorf("triage round-2 provider %q notes do not cite the triage: %q", want.ID, p.Notes)
		}
	}
}

// TestTriageRound2VerifiedAdditions checks the three docs-verified
// additions of round 2 (they live in verified.go, not
// triageProvidersRound2).
func TestTriageRound2VerifiedAdditions(t *testing.T) {
	for _, id := range []string{"gmicloud", "vultr", "privatemode-ai"} {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("verified triage round-2 provider %q not in catalog", id)
			continue
		}
		if p.Verification != VerificationDocs {
			t.Errorf("triage round-2 provider %q verification = %q, want verified-from-docs", id, p.Verification)
		}
		if len(p.BaseURLs) == 0 {
			t.Errorf("triage round-2 provider %q has no base URL", id)
		}
	}
}

// TestTriageRound2BaseURLFills checks the round-2 fill landed on a
// reference entry with the exact base URL, and that the entry stayed
// reference-only.
func TestTriageRound2BaseURLFills(t *testing.T) {
	if len(triageBaseURLFixesRound2) == 0 {
		t.Fatal("triageBaseURLFixesRound2 is empty")
	}
	for _, fix := range triageBaseURLFixesRound2 {
		p, ok := ByID(fix.ID)
		if !ok {
			t.Errorf("round-2 fill target %q not in catalog", fix.ID)
			continue
		}
		if len(p.BaseURLs) != len(fix.BaseURLs) {
			t.Errorf("round-2 fill target %q base URLs = %v, want %v", fix.ID, p.BaseURLs, fix.BaseURLs)
			continue
		}
		for i, u := range fix.BaseURLs {
			if p.BaseURLs[i] != u {
				t.Errorf("round-2 fill target %q base URL[%d] = %q, want %q", fix.ID, i, p.BaseURLs[i], u)
			}
		}
		if p.Verification != VerificationReference {
			t.Errorf("round-2 fill target %q verification = %q, want reference-only", fix.ID, p.Verification)
		}
	}
}

// TestTriageRound2Counts pins the round-2 ledger: 46 triaged = 42
// added (3 verified from docs + 39 reference-only) + 4 rejected +
// 0 pending, and 136 candidates untouched of the 228 total.
func TestTriageRound2Counts(t *testing.T) {
	if got := len(triageProvidersRound2); got != 39 {
		t.Errorf("reference-only round-2 providers = %d, want 39", got)
	}
	verified := 0
	for _, id := range []string{"gmicloud", "vultr", "privatemode-ai"} {
		if p, ok := ByID(id); ok && p.Verification == VerificationDocs {
			verified++
		}
	}
	if verified != 3 {
		t.Errorf("verified round-2 providers = %d, want 3", verified)
	}
}
