// B2-36 (PARITY.md:536): auto/thrifty must serve from the quota-hinted
// rung. The resolver-level ladder is already covered in
// internal/autorouter (TestAutoVariantThriftyRungOrder); what was
// missing is the ENGINE path: the registry quota hint (HasQuota +
// QuotaHeadroom) flows through liveAutoCatalog into RemainingPercent,
// and the exhausted subscription rung is gated out of the served
// ladder. These tests carry ONLY the registry signal — no
// RemainingPercent/HasRemaining preset — so they FAIL if the engine
// enrichment is removed.
package router

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
)

// b2ThriftyCatalog is a quota-hinted thrifty fixture: three
// subscription models at 1% / 30% / 80% remaining (the 1% one is
// below the 2% exit cutoff and must be gated out), then one model
// per lower rung.
func b2ThriftyCatalog() []autorouter.CatalogModel {
	return []autorouter.CatalogModel{
		{Provider: "subA", Model: "sub-low", Billing: "subscription", OverageHardStop: true, HasQuota: true, QuotaHeadroom: 0.01},
		{Provider: "subB", Model: "sub-mid", Billing: "subscription", OverageHardStop: true, HasQuota: true, QuotaHeadroom: 0.30},
		{Provider: "subC", Model: "sub-high", Billing: "subscription", OverageHardStop: true, HasQuota: true, QuotaHeadroom: 0.80},
		{Provider: "keyA", Model: "keyless-m", Billing: "keyless"},
		{Provider: "cheapA", Model: "cheap-m", Billing: "metered", Tier: "cheap"},
		{Provider: "premA", Model: "prem-m", Billing: "metered", Tier: "premium"},
	}
}

func b2ThriftyDispatch(t *testing.T, catalog []autorouter.CatalogModel) (string, []string, bool) {
	t.Helper()
	eng := NewEngine(nil, nil, testConfig())
	SetAutoCatalog(func() []autorouter.CatalogModel { return catalog })
	t.Cleanup(func() { SetAutoCatalog(nil) })
	req, disp, handled := eng.dispatchAutoVariant(context.Background(), b2RankerReq("auto/thrifty"))
	if !handled {
		t.Fatalf("auto/thrifty not handled")
	}
	if disp.Err != nil {
		t.Fatalf("auto/thrifty dispatch error: %v", disp.Err)
	}
	return req.Model, disp.Ordered, disp.OrderedExplicit
}

func TestB2ThriftyQuotaHintsGateExhaustedRung(t *testing.T) {
	model, ordered, explicit := b2ThriftyDispatch(t, b2ThriftyCatalog())
	want := []string{"sub-high", "sub-mid", "keyless-m", "cheap-m", "prem-m"}
	if len(ordered) != len(want) {
		t.Fatalf("thrifty ladder = %v, want %v (1%% subscription rung gated out)", ordered, want)
	}
	for i, m := range want {
		if ordered[i] != m {
			t.Fatalf("thrifty ladder = %v, want %v", ordered, want)
		}
	}
	if model != "sub-high" {
		t.Fatalf("thrifty served %q, want sub-high (first surviving rung, most remaining in-rung)", model)
	}
	if !explicit {
		t.Fatalf("thrifty OrderedExplicit = false, want true (multi-model ladder order is final)")
	}
}

// TestB2ThriftyWithoutHintsServesExhaustedRung is the control: the
// same catalog with every quota hint stripped serves the 1% model
// first — proving the gating above comes from the hints, not from
// rung order alone. If the engine ignored hints entirely, the two
// tests would serve the same model.
func TestB2ThriftyWithoutHintsServesExhaustedRung(t *testing.T) {
	bare := b2ThriftyCatalog()
	for i := range bare {
		bare[i].HasQuota = false
		bare[i].QuotaHeadroom = 0
	}
	model, ordered, _ := b2ThriftyDispatch(t, bare)
	want := []string{"sub-low", "sub-mid", "sub-high", "keyless-m", "cheap-m", "prem-m"}
	if len(ordered) != len(want) {
		t.Fatalf("hintless thrifty ladder = %v, want %v (unknown quota admitted)", ordered, want)
	}
	for i, m := range want {
		if ordered[i] != m {
			t.Fatalf("hintless thrifty ladder = %v, want %v", ordered, want)
		}
	}
	if model != "sub-low" {
		t.Fatalf("hintless thrifty served %q, want sub-low (no hints: rung order decides)", model)
	}
}
