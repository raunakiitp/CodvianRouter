// Provider-level failure-window breaker (M4.2, the reference router
// PROVIDER_PROFILES providerFailureThreshold / providerFailureWindowMs
// / providerCooldownMs).
//
// The consecutive-failure breaker trips on N failures IN A ROW. A
// provider that fails 9 times, succeeds once, and fails 9 more
// times never trips it — the success resets the count — while its
// users still see a ~90% failure rate. The window breaker closes
// that hole: it counts breaker-counted failures per provider inside
// a rolling window (OAuth 10 / 15 min, API-key 15 / 30 min, local
// 2 / 5 min) regardless of interleaved successes, and opens the
// provider-level breaker for the profile's cooldown (5 / 10 / 1
// min) when the window fills. Successes never erase window events
// — only time prunes them — so a flapping provider stays open
// until it actually goes quiet.
//
// The window breaker is per PROVIDER (all keys together), where the
// consecutive breaker is also per provider: the two are different
// trip rules on the same provider, not different scopes. A provider
// is skipped while EITHER is open.
package router

import "time"

// windowBreakerState is one provider's live failure-window record:
// the timestamps of its recent breaker-counted failures, and when
// a tripped window breaker reopens (zero when closed).
type windowBreakerState struct {
	events    []time.Time
	openUntil time.Time
	trips     int
}

// windowRecordFailure records one breaker-counted failure for the
// provider at now, and reports whether the window breaker TRIPPED
// on this failure (i.e. it was closed and the window just filled).
// Caller holds e.mu. Events older than the profile window are
// pruned first, so the slice never grows past the threshold + the
// failures inside one window.
func (e *Engine) windowRecordFailure(provider string, prof BreakerProfile, now time.Time) bool {
	st := e.windowBreakers[provider]
	if st == nil {
		st = &windowBreakerState{}
		e.windowBreakers[provider] = st
	}
	if !st.openUntil.IsZero() && now.Before(st.openUntil) {
		// Already open: the failure still counts (it extends
		// nothing — the cooldown is fixed at trip time), but
		// it cannot re-trip.
		st.events = append(st.events, now)
		return false
	}
	cutoff := now.Add(-prof.ProviderFailureWindow)
	kept := st.events[:0]
	for _, ev := range st.events {
		if !ev.Before(cutoff) {
			kept = append(kept, ev)
		}
	}
	st.events = kept
	st.events = append(st.events, now)
	if len(st.events) >= prof.ProviderFailureThreshold {
		st.openUntil = now.Add(prof.ProviderCooldown)
		st.trips++
		return true
	}
	return false
}

// windowOpen reports whether the provider's window breaker is open
// at now. Caller holds e.mu.
func (e *Engine) windowOpenLocked(provider string, now time.Time) bool {
	st := e.windowBreakers[provider]
	return st != nil && !st.openUntil.IsZero() && now.Before(st.openUntil)
}

// windowFailuresLocked counts the provider's breaker-counted
// failures inside its current window. Caller holds e.mu.
func (e *Engine) windowFailuresLocked(provider string, prof BreakerProfile, now time.Time) int {
	st := e.windowBreakers[provider]
	if st == nil {
		return 0
	}
	cutoff := now.Add(-prof.ProviderFailureWindow)
	n := 0
	for _, ev := range st.events {
		if !ev.Before(cutoff) {
			n++
		}
	}
	return n
}
