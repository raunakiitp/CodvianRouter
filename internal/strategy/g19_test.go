// G19 Area 1 tests: 16-factor input fixes, LKGP TTL + breaker
// override, reset-aware continuous remaining.
package strategy

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

func TestG19SixteenInputsMeasuredZeroSuccessScoresZero(t *testing.T) {
	// A measured 0% success rate (2 attempts, both failed) is a
	// real 0, not unknown: the reliability factor must score 0,
	// never neutral 0.5.
	in := SixteenInputsFromSignals(scoring.Signals{Attempts: 2, SuccessRate: 0})
	if in.SuccessRate != 0 {
		t.Fatalf("SuccessRate = %v, want 0 (measured, not unknown)", in.SuccessRate)
	}
	if got := scoring.FactorReliability(in); got != 0 {
		t.Fatalf("FactorReliability = %v, want 0", got)
	}
	// Unmeasured (no attempts) still reads neutral.
	in = SixteenInputsFromSignals(scoring.Signals{Attempts: 0, SuccessRate: 0})
	if in.SuccessRate != -1 {
		t.Fatalf("unmeasured SuccessRate = %v, want -1 (unknown)", in.SuccessRate)
	}
}

func TestG19SixteenInputsPriceTierClassified(t *testing.T) {
	// Tier comes from the real price now (blended proxy in this
	// scorer): $0 free, <= $1/M cheap, above premium; unknown
	// price stays unclassified.
	cases := []struct {
		cost float64
		want scoring.ModelTier
	}{
		{0, scoring.ModelTierFree},
		{0.5, scoring.ModelTierCheap},
		{1.0, scoring.ModelTierCheap},
		{5, scoring.ModelTierPremium},
		{-1, scoring.ModelTierUnknown},
	}
	for _, tc := range cases {
		in := SixteenInputsFromSignals(scoring.Signals{CostUSDPer1M: tc.cost})
		if in.ModelTier != tc.want {
			t.Fatalf("cost %v -> tier %v, want %v", tc.cost, in.ModelTier, tc.want)
		}
	}
}

func TestG19LKGPRecordExpiresAfterTTL(t *testing.T) {
	// RT-2: a pin older than LKGPRecordTTL reports missing on
	// Lookup and is dropped on Load. Written via Save with a
	// hand-aged UpdatedAt (RecordSuccess always stamps now).
	s := NewLKGPStore()
	s.RecordSuccess("chain-a", "prov-a", "model-a", "")
	s.RecordSuccess("chain-b", "prov-b", "model-b", "")
	path := filepath.Join(t.TempDir(), "lkgp.json")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f lkgpFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	old := time.Now().Add(-LKGPRecordTTL - time.Minute)
	rec := f.Records["chain-a"]
	rec.UpdatedAt = old
	f.Records["chain-a"] = rec
	data, err = json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dst := NewLKGPStore()
	if err := dst.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := dst.Lookup("chain-a"); ok {
		t.Fatalf("expired pin chain-a still looks up after Load, want missing")
	}
	if _, ok := dst.Lookup("chain-b"); !ok {
		t.Fatalf("fresh pin chain-b missing after Load, want present")
	}
}

func TestG19LKGPRegistryLatestSkipsExpired(t *testing.T) {
	// Latest()/LatestRecord() skip expired pins, so auto/lkgp
	// and the auto-pool session input never stick to a stale
	// last-good.
	r := NewLKGPRegistry()
	r.RecordSuccessFor("chain-old", "prov-old", "model-old", "")
	r.RecordSuccessFor("chain-new", "prov-new", "model-new", "")
	// Age only the old record: rewrite the store through a
	// Save/Load round-trip with an aged timestamp.
	path := filepath.Join(t.TempDir(), "lkgp.json")
	if err := r.Store().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f lkgpFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	old := time.Now().Add(-LKGPRecordTTL - time.Minute)
	rec := f.Records["chain-old"]
	rec.UpdatedAt = old
	f.Records["chain-old"] = rec
	// Make the new record strictly newest.
	recNew := f.Records["chain-new"]
	recNew.UpdatedAt = time.Now()
	f.Records["chain-new"] = recNew
	data, err = json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dst := NewLKGPRegistry()
	if err := dst.Store().Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := dst.Latest()
	if !ok || got != "model-new" {
		t.Fatalf("Latest() = %q, %v, want model-new, true", got, ok)
	}
	rec2, ok := dst.LatestRecord()
	if !ok || rec2.Provider != "prov-new" || rec2.Model != "model-new" {
		t.Fatalf("LatestRecord() = %+v, %v, want prov-new/model-new", rec2, ok)
	}
}

func TestG19LKGPBreakerOpenDisablesStickiness(t *testing.T) {
	// A last-good provider whose breaker is positively OPEN is
	// never hoisted first: the declared order stands. The same
	// pin with a closed breaker hoists as before.
	model := "g19-lkgp-model"
	defer DefaultLKGPRegistry.Store().Clear(model)
	DefaultLKGPRegistry.RecordSuccessFor(model, "dead-prov", model, "")
	chain := []router.Attempt{
		{ProviderID: "live-prov", Model: model},
		{ProviderID: "dead-prov", Model: model},
	}
	signals := func(provider, m string) scoring.Signals {
		return scoring.Signals{BreakerOpen: provider == "dead-prov"}
	}
	factory := HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil)
	hook := factory("lkgp", router.StrategyHookContext{})
	ordered := hook(chain)
	if ordered[0].ProviderID != "live-prov" {
		t.Fatalf("OPEN pin hoisted first: order %v, want live-prov first", ordered)
	}
	// Breaker closed: the pin hoists.
	signalsClosed := func(provider, m string) scoring.Signals {
		return scoring.Signals{BreakerOpen: false}
	}
	factory = HookFactoryWithCacheAffinity(signalsClosed, scoring.Weights{}, nil)
	ordered = factory("lkgp", router.StrategyHookContext{})(chain)
	if ordered[0].ProviderID != "dead-prov" {
		t.Fatalf("closed pin did not hoist: order %v, want dead-prov first", ordered)
	}
}

func TestG19ResetAwareUsesQuotaRemainingWhenUtilUnknown(t *testing.T) {
	// Unknown utilisation (Util -1) falls back to the measured
	// remaining fraction: with a far reset (no pressure term)
	// the score is proportional to the fraction, and an
	// exhausted fraction scores 0 — not the old full-remaining
	// fail-open.
	now := time.Now()
	farReset := now.Add(30 * 24 * time.Hour)
	mk := func(remaining float64) QuotaSnapshot {
		return QuotaSnapshot{Util5h: -1, Util7d: -1, QuotaRemaining: remaining, ResetAt: farReset}
	}
	hi := ResetAwareScore(mk(0.9), now)
	mid := ResetAwareScore(mk(0.3), now)
	lo := ResetAwareScore(mk(0), now)
	if !(hi > mid && mid > lo) {
		t.Fatalf("scores hi=%v mid=%v lo=%v, want hi > mid > lo", hi, mid, lo)
	}
	if lo != 0 {
		t.Fatalf("exhausted score = %v, want 0", lo)
	}
	if math.Abs(hi-0.32*0.9) > 1e-9 {
		t.Fatalf("hi score = %v, want %v (0.32 * remaining, far reset)", hi, 0.32*0.9)
	}
	// Measured utilisation still wins over the fraction: a
	// half-used window scores from the utilisation, not the hint.
	measured := ResetAwareScore(QuotaSnapshot{Util5h: 0.5, Util7d: 0.5, QuotaRemaining: 0.9, ResetAt: farReset}, now)
	if math.Abs(measured-0.32*0.5) > 1e-9 {
		t.Fatalf("measured-util score = %v, want %v (0.32 * 0.5)", measured, 0.32*0.5)
	}
}
