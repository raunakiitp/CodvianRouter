package catalog

import (
	"strings"
	"testing"
)

// TestTriageRound5ProvidersPresent checks every 2026-11-18 triage
// round-5 (final) reference-only addition is in the catalog with a
// base URL, reference-only verification, and a cited source — and
// that ValidateAll still passes over them.
func TestTriageRound5ProvidersPresent(t *testing.T) {
	if len(triageProvidersRound5) == 0 {
		t.Fatal("triageProvidersRound5 is empty")
	}
	for _, want := range triageProvidersRound5 {
		p, ok := ByID(want.ID)
		if !ok {
			t.Errorf("triage round-5 provider %q not in catalog", want.ID)
			continue
		}
		if len(p.BaseURLs) == 0 {
			t.Errorf("triage round-5 provider %q has no base URL", want.ID)
		}
		if p.Verification != VerificationReference {
			t.Errorf("triage round-5 provider %q verification = %q, want %q (docs-verified additions live in verified.go)", want.ID, p.Verification, VerificationReference)
		}
		if strings.TrimSpace(p.SourceURL) == "" {
			t.Errorf("triage round-5 provider %q has no SourceURL", want.ID)
		}
		if !strings.Contains(p.Notes, "2026-11-18 triage round 5") {
			t.Errorf("triage round-5 provider %q notes do not cite the triage: %q", want.ID, p.Notes)
		}
	}
}

// TestTriageRound5VerifiedAdditions checks the five docs-verified
// additions of round 5 (they live in verified.go, not
// triageProvidersRound5): salad-cloud, verda, confidential-ai,
// therouter, llmtech.
func TestTriageRound5VerifiedAdditions(t *testing.T) {
	for _, id := range []string{"salad-cloud", "verda", "confidential-ai", "therouter", "llmtech"} {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("verified triage round-5 provider %q not in catalog", id)
			continue
		}
		if p.Verification != VerificationDocs {
			t.Errorf("triage round-5 provider %q verification = %q, want verified-from-docs", id, p.Verification)
		}
		if len(p.BaseURLs) == 0 {
			t.Errorf("triage round-5 provider %q has no base URL", id)
		}
		if !strings.Contains(p.Notes, "2026-11-18 triage round 5") {
			t.Errorf("triage round-5 provider %q notes do not cite the triage: %q", id, p.Notes)
		}
	}
}

// TestTriageRound5NoIDMoves pins the round-5 rename outcome: the 3
// still-pending possible renames (charm-hyper, qiniu, regolo) were
// re-checked against the providers' own docs and still state no
// canonical machine ID, so no ID move was applied — the catalog IDs
// resolve, the models.dev IDs do not.
func TestTriageRound5NoIDMoves(t *testing.T) {
	for _, id := range []string{"charm-hyper", "qiniu", "regolo"} {
		if _, ok := ByID(id); !ok {
			t.Errorf("round-5 pending rename: catalog ID %q no longer resolves", id)
		}
	}
	for _, id := range []string{"hyper", "qiniu-ai", "regolo-ai"} {
		if _, ok := ByID(id); ok {
			t.Errorf("round-5 pending rename: models.dev ID %q resolves — an ID move was applied without docs proof", id)
		}
	}
}

// TestTriageRound5Counts pins the round-5 ledger: 44 triaged = 44
// added (5 verified from docs + 39 reference-only) + 0 rejected +
// 0 pending, and 0 candidates untouched of the 228 total — the
// triage is complete.
func TestTriageRound5Counts(t *testing.T) {
	if got := len(triageProvidersRound5); got != 39 {
		t.Errorf("reference-only round-5 providers = %d, want 39", got)
	}
	verified := 0
	for _, id := range []string{"salad-cloud", "verda", "confidential-ai", "therouter", "llmtech"} {
		if p, ok := ByID(id); ok && p.Verification == VerificationDocs {
			verified++
		}
	}
	if verified != 5 {
		t.Errorf("verified round-5 providers = %d, want 5", verified)
	}
	// Cumulative: rounds 1–5 added 191 of the 228 candidates.
	if got := len(triageProviders) + len(triageProvidersRound2) + len(triageProvidersRound3) + len(triageProvidersRound4) + len(triageProvidersRound5); got != 174 {
		t.Errorf("cumulative reference-only triage providers = %d, want 174", got)
	}
}

// TestTriageRound5LoopbackUnprobed pins the lynkr precedent: a
// loopback base URL is carried as the local default and is never
// treated as a probed-live endpoint.
func TestTriageRound5LoopbackUnprobed(t *testing.T) {
	p, ok := ByID("lynkr")
	if !ok {
		t.Fatal("triage round-5 provider \"lynkr\" not in catalog")
	}
	if len(p.BaseURLs) != 1 || !strings.HasPrefix(p.BaseURLs[0], "http://127.0.0.1:") {
		t.Errorf("lynkr base URLs = %v, want a single 127.0.0.1 default", p.BaseURLs)
	}
	if !strings.Contains(p.Notes, "not probed from here") {
		t.Errorf("lynkr notes do not record the unprobed loopback: %q", p.Notes)
	}
}
