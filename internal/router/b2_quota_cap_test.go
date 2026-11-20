// B2-09 (parity tracker:509): quota-share cap on account rotation
// (the reference router #20561, equal-first). The cap is MEASURED, not
// configured: learnRateLimit records the per-key remaining fraction
// from real upstream rate-limit headers, and orderAccountCapCheck
// sinks at-cap accounts (measured remaining 0, live entry) to the
// tail after each strategy's ordering — never dropping, never
// reordering within either side, failing open when none or all are
// at cap. These tests pin all three pieces; each one FAILS with
// the feature off (see the per-test notes).
package router

import (
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
)

// b2CapKeyIDs returns the KeyIDs of a chain in order, for order
// assertions that must see keys, not providers (all attempts here
// share one provider).
func b2CapKeyIDs(chain []Attempt) []string {
	got := make([]string, len(chain))
	for i, att := range chain {
		got[i] = att.KeyID
	}
	return got
}

// b2CapChain builds one (provider, model) group of attempts, one per
// key, in declared order.
func b2CapChain(keys ...string) []Attempt {
	chain := make([]Attempt, len(keys))
	for i, k := range keys {
		chain[i] = Attempt{ProviderID: "p1", Model: "m", KeyID: k}
	}
	return chain
}

// b2CapOrder asserts the key order of got, failing with both orders
// shown.
func b2CapOrder(t *testing.T, got []Attempt, want ...string) {
	t.Helper()
	keys := b2CapKeyIDs(got)
	if len(keys) != len(want) {
		t.Fatalf("key order = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("key order = %v, want %v", keys, want)
		}
	}
}

// TestB2LearnRateLimitRecordsPerKeyHint: learnRateLimit records the
// measured fraction per KEY, not just per provider. FAILS if the
// setLearnedKeyQuotaHint call is removed from learnRateLimit (the
// per-key entry never appears), or if the recording is provider-wide
// only (k2 would then read k1's fraction).
func TestB2LearnRateLimitRecordsPerKeyHint(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	att := Attempt{ProviderID: "p1", Model: "m", KeyID: "k1"}
	sig := &adaptive.RateLimitSignals{
		HasRequests:       true,
		RequestsLimit:     100,
		RequestsRemaining: 30,
		ObservedAt:        time.Now(),
	}
	eng.learnRateLimit(att, sig)

	// Per-key: k1 learned 30, live.
	hint, _, ok := eng.LearnedKeyQuotaHint("p1", "k1")
	if !ok || hint != 30 {
		t.Fatalf("LearnedKeyQuotaHint(p1,k1) = (%d, ok=%v), want (30, true)", hint, ok)
	}
	// Provider twin still recorded alongside (3cca53b behaviour).
	hint, _, ok = eng.LearnedQuotaHint("p1")
	if !ok || hint != 30 {
		t.Fatalf("LearnedQuotaHint(p1) = (%d, ok=%v), want (30, true)", hint, ok)
	}
	// Per-key granularity: a sibling key of the same provider and
	// the same key of another provider read nothing.
	if _, _, ok := eng.LearnedKeyQuotaHint("p1", "k2"); ok {
		t.Fatalf("LearnedKeyQuotaHint(p1,k2) unexpectedly ok — per-key signal leaked across keys")
	}
	if _, _, ok := eng.LearnedKeyQuotaHint("p2", "k1"); ok {
		t.Fatalf("LearnedKeyQuotaHint(p2,k1) unexpectedly ok — per-key signal leaked across providers")
	}
}

// TestB2LearnedKeyQuotaHintDropsExpired: the per-key accessor mirrors
// the provider one's expiry-drop — an expired entry reads as absent
// (and is dropped, so the key is eligible again), a zero expiresAt
// never expires. FAILS if the expiry-drop is missing (the expired
// entry would still read ok).
func TestB2LearnedKeyQuotaHintDropsExpired(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())

	eng.setLearnedKeyQuotaHint("p1", "k1", 40, time.Now().Add(-time.Minute))
	if _, _, ok := eng.LearnedKeyQuotaHint("p1", "k1"); ok {
		t.Fatalf("expired per-key hint still reads ok — expiry-drop missing")
	}
	// Dropped, not lingering: a second read is still absent.
	if _, _, ok := eng.LearnedKeyQuotaHint("p1", "k1"); ok {
		t.Fatalf("expired per-key hint reappeared on second read")
	}

	// Zero expiresAt (operator form) never expires.
	eng.setLearnedKeyQuotaHint("p1", "k1", 40, time.Time{})
	hint, _, ok := eng.LearnedKeyQuotaHint("p1", "k1")
	if !ok || hint != 40 {
		t.Fatalf("zero-expiry per-key hint = (%d, ok=%v), want (40, true)", hint, ok)
	}
}

// TestB2RestoreLearnedKeyQuotaHint: the startup restore replays the
// ledger's per-key rows — including a restored 0 (safe here, unlike
// SetQuotaHint, because the entry carries the window-reset expiry)
// — and is a no-op for a signal with no request/token counts.
// FAILS if the restore never records (the cap would not survive a
// restart), or if it records empty signals.
func TestB2RestoreLearnedKeyQuotaHint(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	now := time.Now()

	// A 0 IS restored: the key is at cap until its window refills.
	eng.RestoreLearnedKeyQuotaHint("p1", "k1", adaptive.RateLimitSignals{
		HasRequests:       true,
		RequestsLimit:     100,
		RequestsRemaining: 0,
		ObservedAt:        now,
	}, now)
	hint, _, ok := eng.LearnedKeyQuotaHint("p1", "k1")
	if !ok || hint != 0 {
		t.Fatalf("restored per-key hint = (%d, ok=%v), want (0, true)", hint, ok)
	}

	// Tokens-only signal restores its own fraction.
	eng.RestoreLearnedKeyQuotaHint("p1", "k2", adaptive.RateLimitSignals{
		HasTokens:       true,
		TokensLimit:     50,
		TokensRemaining: 25,
		ObservedAt:      now,
	}, now)
	hint, _, ok = eng.LearnedKeyQuotaHint("p1", "k2")
	if !ok || hint != 50 {
		t.Fatalf("restored tokens-only per-key hint = (%d, ok=%v), want (50, true)", hint, ok)
	}

	// No counts teaches nothing: no entry appears.
	eng.RestoreLearnedKeyQuotaHint("p1", "k3", adaptive.RateLimitSignals{}, now)
	if _, _, ok := eng.LearnedKeyQuotaHint("p1", "k3"); ok {
		t.Fatalf("empty restore unexpectedly recorded a per-key hint")
	}
}

// TestB2CapCheckSinksAtCapAccountsToTail: orderAccountCapCheck is a
// stable partition — at-cap accounts (measured remaining 0, live
// entry) sink to the tail, under-cap accounts keep their strategy
// order first, and within-side order is untouched. FAILS if the cap
// check does not sink at-cap accounts (k2 would stay second).
func TestB2CapCheckSinksAtCapAccountsToTail(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	live := time.Now().Add(time.Hour)
	eng.setLearnedKeyQuotaHint("p1", "k2", 0, live)  // at cap
	eng.setLearnedKeyQuotaHint("p1", "k3", 50, live) // under cap
	// k1: no signal at all — not at cap.

	got := eng.orderAccountCapCheck(b2CapChain("k1", "k2", "k3"))
	b2CapOrder(t, got, "k1", "k3", "k2")

	// The at-cap account in the middle sinks; the under-cap pair
	// keeps its relative order.
	eng = NewEngine(nil, nil, testConfig())
	eng.setLearnedKeyQuotaHint("p1", "k1", 0, live)
	eng.setLearnedKeyQuotaHint("p1", "k2", 10, live)
	eng.setLearnedKeyQuotaHint("p1", "k3", 90, live)
	got = eng.orderAccountCapCheck(b2CapChain("k1", "k2", "k3"))
	b2CapOrder(t, got, "k2", "k3", "k1")
}

// TestB2CapCheckFailOpen: the cap check changes nothing when no
// account is at cap, when every account is at cap, or when the only
// 0-fraction entry is expired (a refilled window reads as not at
// cap). FAILS if the check reorders or drops in any of these cases.
func TestB2CapCheckFailOpen(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	live := time.Now().Add(time.Hour)

	// No signal anywhere: order unchanged.
	b2CapOrder(t, eng.orderAccountCapCheck(b2CapChain("k1", "k2", "k3")), "k1", "k2", "k3")

	// All at cap: order unchanged — a capped account can still
	// serve when it is the only option.
	for _, k := range []string{"k1", "k2", "k3"} {
		eng.setLearnedKeyQuotaHint("p1", k, 0, live)
	}
	b2CapOrder(t, eng.orderAccountCapCheck(b2CapChain("k1", "k2", "k3")), "k1", "k2", "k3")

	// Expired 0 entry: not at cap, order unchanged.
	eng = NewEngine(nil, nil, testConfig())
	eng.setLearnedKeyQuotaHint("p1", "k2", 0, time.Now().Add(-time.Minute))
	b2CapOrder(t, eng.orderAccountCapCheck(b2CapChain("k1", "k2", "k3")), "k1", "k2", "k3")
}

// TestB2OrderAccountAttemptsAppliesCapAfterStrategy: the cap check
// runs inside orderAccountAttempts, after the strategy's ordering —
// the default strategy (fill-first, declared order) orders the
// group, then the at-cap account sinks to the tail. FAILS if the
// orderAccountCapCheck call is removed from orderAccountAttempts
// (k1 would lead), or if the strategy order is disturbed (k2/k3
// would swap).
func TestB2OrderAccountAttemptsAppliesCapAfterStrategy(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())

	// Control: no signal — declared order stands.
	b2CapOrder(t, eng.orderAccountAttempts(b2CapChain("k1", "k2", "k3")), "k1", "k2", "k3")

	// k1 at cap: it sinks to the tail, k2/k3 keep strategy order.
	eng.setLearnedKeyQuotaHint("p1", "k1", 0, time.Now().Add(time.Hour))
	b2CapOrder(t, eng.orderAccountAttempts(b2CapChain("k1", "k2", "k3")), "k2", "k3", "k1")
}
