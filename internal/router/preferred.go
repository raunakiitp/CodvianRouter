// Preferred-account response-start failover (M4.8, the sector baseline
// preferredConnectionFailover.ts; chat.ts:2507) — opt-in.
//
// A preferred account that never starts responding is worse than a
// slow one: the request sits on a dead credential while a sibling
// account of the same provider could serve. When
// Config.PreferredFailoverAfter is set (e.g. 20 s), the FIRST
// streaming attempt of a request — the preferred account — gets a
// response-start deadline: if no first byte arrives within it, the
// attempt is cancelled and the chain rotates to ONE sibling (the
// immediate next attempt, same provider, different key). Only once
// per request; the sibling gets no second rotation.
//
// The deadline is armed only when the sibling is the immediate
// next attempt (key-pool order puts sibling keys adjacent, so this
// is the normal shape) and only for a non-hedged first attempt —
// the hedge already races a slow first pick. The deadline is
// disarmed the moment the first byte commits (onCommit stops the
// timer); a rotation only ever fires pre-commit, so the client
// never sees a partial body followed by a switch. A rotation
// cancellation is a deadline, not a caller cancellation: the
// streamFailed branch bypasses the terminal check for it (a
// context.Canceled error would otherwise stop the chain) and the
// attempt is breaker-neutral, like any other cancellation.
//
// Non-streaming attempts need no rotation: the per-target timeout
// (M4.7c, 120 s) already bounds time-to-first-response and fails
// over on it. Default OFF: the failover only runs when the
// operator opts in.
package router

import "time"

// PreferredFailoverAfterDefault is the response-start deadline to
// use when the operator enables the failover without a value —
// documented here; the config field itself is a plain duration
// (0 = off) so there is no magic default to discover.
const PreferredFailoverAfterDefault = 20 * time.Second

// preferredSiblingNext reports whether the immediate next attempt
// in the chain is a sibling of the attempt at idx: same provider,
// different key (a different account of the same provider). That
// is the only rotation the failover ever makes.
func preferredSiblingNext(active []Attempt, idx int) bool {
	if idx+1 >= len(active) {
		return false
	}
	cur, next := active[idx], active[idx+1]
	return next.ProviderID == cur.ProviderID && next.KeyID != cur.KeyID
}
