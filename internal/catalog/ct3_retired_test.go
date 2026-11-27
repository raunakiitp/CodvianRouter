package catalog

import (
	"strings"
	"testing"
)

// CT-3 — Dead providers listed in the live catalog feed with free-tier
// flags. The four entries below were retired from the live surface:
// blackbox + galadriel are deprecated in the the reference router reference
// (dead public inference endpoints, sweep-noted); ai21's hosted
// Jamba/Maestro APIs sunset; bytez has no base URL recorded in either
// catalog and cannot be added or routed as listed. No shutdown date
// could be verified for any of the four, so RetiredOn is "" for all
// of them — never invented (see the RetiredReason on each entry).

// ct3RetiredIDs is the CT-3 retirement set, in catalog order.
// A1 (2026-11-27, owner decision D2) added cheaperinference to the
// retired set — same mechanism, separate reason, so the count pins
// move 4 -> 5 and the live total 610 -> 609. Item 6 (2026-11-27)
// added anyscale and kluster: both inference platforms are shut down
// and their own docs sites no longer document the API hosts, so the
// unverified rows could only be retired — count 5 -> 7, total 609 -> 607.
var ct3RetiredIDs = []string{"blackbox", "galadriel", "ai21", "bytez", "cheaperinference", "anyscale", "kluster"}

func ct3RetiredSet(t *testing.T) map[string]Provider {
	t.Helper()
	retired := RetiredProviders()
	if len(retired) != len(ct3RetiredIDs) {
		t.Fatalf("RetiredProviders = %d entries, want %d (%v)", len(retired), len(ct3RetiredIDs), ct3RetiredIDs)
	}
	got := map[string]Provider{}
	for _, p := range retired {
		if !p.Retired {
			t.Fatalf("RetiredProviders returned %q with Retired=false", p.ID)
		}
		if strings.TrimSpace(p.RetiredReason) == "" {
			t.Fatalf("retired provider %q has no RetiredReason", p.ID)
		}
		if p.RetiredOn != "" {
			t.Fatalf("retired provider %q has RetiredOn %q, want empty (no shutdown date verified)", p.ID, p.RetiredOn)
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("retired provider %q fails Validate: %v", p.ID, err)
		}
		got[p.ID] = p
	}
	for _, id := range ct3RetiredIDs {
		if _, ok := got[id]; !ok {
			t.Fatalf("RetiredProviders is missing %q", id)
		}
	}
	return got
}

// TestCT3RetiredProvidersExcludedFromAll — acceptance (1): the four dead
// IDs are absent from All() (and therefore from the GET /api/catalog
// feed, which serves All() verbatim), while remaining retrievable with
// their retirement reason through RetiredByID.
func TestCT3RetiredProvidersExcludedFromAll(t *testing.T) {
	retired := ct3RetiredSet(t)
	for _, id := range ct3RetiredIDs {
		if _, ok := ByID(id); ok {
			t.Errorf("ByID(%q) found a retired provider, want absent from the live catalog", id)
		}
		for _, p := range All() {
			if p.ID == id {
				t.Errorf("All() still contains retired provider %q", id)
			}
		}
		for _, p := range FreeTierProviders() {
			if p.ID == id {
				t.Errorf("FreeTierProviders() still contains retired provider %q", id)
			}
		}
		got, ok := RetiredByID(id)
		if !ok {
			t.Errorf("RetiredByID(%q) = false, want the retired entry with its reason", id)
			continue
		}
		if got.ID != id || got.RetiredReason != retired[id].RetiredReason {
			t.Errorf("RetiredByID(%q) = %+v, want the retired entry", id, got)
		}
	}
	// Aliases resolve to the retired entry too: an add-by-alias
	// attempt ("bb" for blackbox) must get the retirement reason,
	// not an "unknown provider" error.
	if got, ok := RetiredByID("bb"); !ok || got.ID != "blackbox" {
		t.Errorf("RetiredByID(%q) = %+v, want the blackbox entry", "bb", got)
	}
	if _, ok := ByAlias("bb"); ok {
		t.Errorf("ByAlias(%q) found a retired provider, want absent from the live catalog", "bb")
	}
	// A live provider is unaffected by the retirement machinery.
	if _, ok := ByID("groq"); !ok {
		t.Errorf("ByID(%q) = false, want a live provider still present", "groq")
	}
	if IsRetired("groq") {
		t.Errorf("IsRetired(%q) = true, want false for a live provider", "groq")
	}
	// The live total drops by exactly the retired count — a delta,
	// not a hardcoded total (CT-8 re-measures the headline after this).
	if got, want := len(All()), len(allIncludingRetired())-len(retired); got != want {
		t.Errorf("len(All()) = %d, want %d (full catalog minus %d retired)", got, want, len(retired))
	}
}

// TestCT3FreeTierCountDropsByRetiredFreeTierCount — acceptance (2):
// FreeTierProviders()/FreeTierCount() drop by exactly the dead
// providers' free-tier count, re-measured here from the full catalog
// — no hardcoded 165/162 totals.
func TestCT3FreeTierCountDropsByRetiredFreeTierCount(t *testing.T) {
	retiredFree := 0
	for _, p := range RetiredProviders() {
		if p.HasFreeTier {
			retiredFree++
		}
	}
	if retiredFree == 0 {
		t.Fatal("no retired provider carries HasFreeTier; the delta to measure is 0, test is vacuous")
	}
	fullFree := 0
	for _, p := range allIncludingRetired() {
		if p.HasFreeTier {
			fullFree++
		}
	}
	if got := FreeTierCount(); got != fullFree-retiredFree {
		t.Errorf("FreeTierCount() = %d, want %d (full-catalog free-tier %d minus %d retired free-tier)", got, fullFree-retiredFree, fullFree, retiredFree)
	}
	if got := len(FreeTierProviders()); got != FreeTierCount() {
		t.Errorf("len(FreeTierProviders()) = %d, want FreeTierCount() %d", got, FreeTierCount())
	}
}

// TestCT3RetiredProviderCannotBeAddedByCatalogID — acceptance (3):
// add-by-catalog-ID resolves through ByID/ByAlias (registry
// lookupCatalog), which no longer resolve a retired ID — so the add
// cannot proceed — and the refusal reason is available through
// RetiredByID, not a generic no-base-URL error. (The registry-side
// wording change that consults RetiredByID lives in
// internal/provider/registry.go, owned by CT-2 in this wave; this test
// pins the catalog half that makes it possible.)
func TestCT3RetiredProviderCannotBeAddedByCatalogID(t *testing.T) {
	for _, id := range ct3RetiredIDs {
		if _, ok := ByID(id); ok {
			t.Errorf("ByID(%q) resolves a retired provider, so add-by-catalog-ID could still reach it", id)
		}
		if _, ok := ByAlias(id); ok {
			t.Errorf("ByAlias(%q) resolves a retired provider, so add-by-catalog-ID could still reach it", id)
		}
		p, ok := RetiredByID(id)
		if !ok {
			t.Errorf("RetiredByID(%q) = false, want the retired entry so the refusal can name the reason", id)
			continue
		}
		if !strings.Contains(strings.ToLower(p.RetiredReason), "retired") && !strings.Contains(p.RetiredReason, "sunset") && !strings.Contains(p.RetiredReason, "404") && !strings.Contains(p.RetiredReason, "no longer resolves") {
			t.Errorf("RetiredByID(%q).RetiredReason = %q, want a reason naming what happened to the provider", id, p.RetiredReason)
		}
	}
	// A retired entry without a reason is invalid data, not a
	// silently addable one.
	if err := (Provider{ID: "x", Name: "X", Verification: VerificationReference, Retired: true}).Validate(); err == nil {
		t.Error("Validate accepted a retired provider with no RetiredReason, want an error")
	}
}
