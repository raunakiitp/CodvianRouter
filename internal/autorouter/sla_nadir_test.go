// Auto-variant tests for sla-aware and nadir (M3 Area 1, PARITY
// rows 394, 396; row 756 Default Auto Variant).
package autorouter

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/sla"
)

func TestIsKnownAutoIDSLAAndNadir(t *testing.T) {
	for _, id := range []string{"auto/sla-aware", "auto/sla", "auto/nadir"} {
		if !IsKnownAutoID(id) {
			t.Errorf("IsKnownAutoID(%q) = false, want true", id)
		}
	}
}

func TestResolveAutoSLAAwarePicksCompliant(t *testing.T) {
	catalog := []CatalogModel{
		{Provider: "slow", Model: "m-slow", LatencyMs: 5000, HasLatency: true, Quality: 0.5, HasQuality: true, PriceUSDPerMTok: 2, HasPrice: true},
		{Provider: "good", Model: "m-good", LatencyMs: 100, HasLatency: true, Quality: 0.99, HasQuality: true, PriceUSDPerMTok: 5, HasPrice: true},
		{Provider: "cheap", Model: "m-cheap-bad", LatencyMs: 100, HasLatency: true, Quality: 0.5, HasQuality: true, PriceUSDPerMTok: 0.1, HasPrice: true},
	}
	res, err := ResolveAutoVariant("auto/sla-aware", catalog, "")
	if err != nil {
		t.Fatalf("auto/sla-aware errored: %v", err)
	}
	if res.Strategy != "sla-aware" {
		t.Errorf("strategy = %q, want sla-aware", res.Strategy)
	}
	if res.Model != "m-good" {
		t.Errorf("auto/sla-aware picked %q, want m-good (only model meeting the 2000ms/5%% SLA)", res.Model)
	}
	res, err = ResolveAutoVariant("auto/sla", catalog, "")
	if err != nil {
		t.Fatalf("auto/sla errored: %v", err)
	}
	if res.Model != "m-good" {
		t.Errorf("auto/sla picked %q, want m-good (alias of sla-aware)", res.Model)
	}
}

func TestResolveAutoNadirErrorsHonestly(t *testing.T) {
	// The pure resolver has no endpoint and no prompt, so it
	// must refuse auto/nadir instead of picking a model without
	// a decision. The engine dispatch path intercepts auto/nadir
	// before this resolver (see router/nadir.go).
	catalog := []CatalogModel{{Provider: "provA", Model: "m1"}}
	_, err := ResolveAutoVariant("auto/nadir", catalog, "")
	if err == nil {
		t.Fatalf("auto/nadir resolved without error — the pure resolver must refuse it")
	}
	if !strings.Contains(err.Error(), "dispatch path") {
		t.Errorf("auto/nadir error = %q, want it to name the dispatch path", err.Error())
	}
}

func TestResolveAutoSLAAwareHonoursConfiguredPolicy(t *testing.T) {
	// G5 (b): the configured SLA policy changes the pick. Under
	// the default policy (5% errors) m-strict-ok wins — it is
	// the only model meeting the error objective — but under a
	// loose 20% error policy m-loose-ok wins: it is faster and
	// far cheaper, and its 10% error rate is inside the
	// configured objective.
	catalog := []CatalogModel{
		{Provider: "a", Model: "m-loose-ok", LatencyMs: 100, HasLatency: true, Quality: 0.90, HasQuality: true, PriceUSDPerMTok: 0.1, HasPrice: true},
		{Provider: "b", Model: "m-strict-ok", LatencyMs: 1900, HasLatency: true, Quality: 0.99, HasQuality: true, PriceUSDPerMTok: 10, HasPrice: true},
	}
	res, err := ResolveAutoVariant("auto/sla-aware", catalog, "")
	if err != nil {
		t.Fatalf("default policy: auto/sla-aware errored: %v", err)
	}
	if res.Model != "m-strict-ok" {
		t.Errorf("default policy: picked %q, want m-strict-ok", res.Model)
	}
	loose := sla.Policy{TargetP95Ms: 2000, MaxErrorRate: 0.20}
	res, err = ResolveAutoVariantWithPolicy("auto/sla-aware", catalog, "", loose)
	if err != nil {
		t.Fatalf("loose policy: auto/sla-aware errored: %v", err)
	}
	if res.Model != "m-loose-ok" {
		t.Errorf("loose policy (20%% errors): picked %q, want m-loose-ok", res.Model)
	}
}
