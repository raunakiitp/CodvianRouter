// Connection-cooldown hardening (G11 Area 2, parity tracker Area 2 row
// "Connection cooldown (one key/account)").
//
// the sector-typical evidence is settings.ts:105-116 (per-auth-profile
// transient cooldown bases) and PROVIDER_PROFILES.transientCooldown
// (the earlier implementation/config/constants.ts:296-344): OAuth 5 s, API-key 3 s,
// Local 2 s. Ours had one hard 5 s base for every profile (the soft
// ladder's softCooldownBase) and no anti-thundering-herd guard, so
// N cooled-down keys all became eligible in the same millisecond.
//
// (a) Per-auth-profile cooldown bases: CooldownBaseFor follows the
// existing per-profile breaker thresholds in breaker_profile.go as
// the pattern — the profile comes from the same AuthProfileFor
// resolution the breaker uses, so a provider's cooldown base and
// its breaker thresholds can never disagree about which profile it
// is. The base feeds the soft-demotion doubling ladder
// (startSoftCooldown): a present upstream Retry-After still wins
// (floored at 10 s, CHECKLIST 168) — the profile base only replaces
// the old uniform 5 s when no hint is present. Hard-mode 429
// cooldowns are hint-driven first (retryAfterHonouredWait) and
// fall back to the same per-auth base when no hint is present
// (:529F) — a hint the upstream sent is authoritative, and a
// profile base must never shrink a hinted wait.
//
// (b) Anti-thundering-herd guard: CooldownStaggerFor assigns every
// (provider, key) identity a deterministic release stagger of
// 0–499 ms (FNV-1a of the client key, so the same key always gets
// the same stagger and tests are stable without a clock). The
// request-loop admission gates (onCooldownStaggered, softCooling)
// hold a key until its nominal end PLUS its stagger, so keys whose
// cooldowns expire at the same instant are re-admitted spread over
// half a second instead of all at once. The exact onCooldown stays
// exact for Nadir's decision cooldown and the hedge-availability
// check, whose timing contracts must not move. The stagger is
// admission-only: the dashboard feed (CooldownTargets) still
// reports the nominal end, and the sleep-then-retry path (which
// clears the cooldown before re-attempting) is unaffected.
// Deliberate divergence from the sector baseline: the sector baseline has no such
// guard at all — the stagger is ours, added because a local router
// with two or three keys re-admitting in the same millisecond is
// exactly the thundering herd the cooldown exists to prevent.
package router

import (
	"hash/fnv"
	"time"
)

// CooldownBaseFor returns the transient-cooldown base for one auth
// profile (the sector baseline PROVIDER_PROFILES.transientCooldown, verbatim:
// OAuth 5 s — "session tokens, short recovery"; API-key 3 s —
// "API providers recover faster"; Local 2 s — "local, very fast
// recovery"). Unknown profiles take the API-key base, the same
// default the breaker table uses.
func CooldownBaseFor(p AuthProfile) time.Duration {
	switch p {
	case AuthProfileOAuth:
		return 5 * time.Second
	case AuthProfileLocal:
		return 2 * time.Second
	default:
		return 3 * time.Second
	}
}

// cooldownProfileFor resolves one provider's auth profile for its
// cooldown base — the same AuthProfileFor resolution the breaker
// uses (breakerProfile), so the two can never disagree.
func (e *Engine) cooldownProfileFor(providerID string) AuthProfile {
	if e.cfg.AuthProfileFor != nil {
		return e.cfg.AuthProfileFor(providerID)
	}
	return AuthProfileAPIKey
}

// cooldownBaseFor is the transient-cooldown base for one provider.
func (e *Engine) cooldownBaseFor(providerID string) time.Duration {
	return CooldownBaseFor(e.cooldownProfileFor(providerID))
}

// cooldownStaggerMax is the widest release stagger (500 ms): keys
// expiring together are re-admitted spread over half a second.
// Wide enough to break a synchronised retry wave, narrow enough
// that no key waits a meaningful extra time.
const cooldownStaggerMax = 500 * time.Millisecond

// CooldownStaggerFor returns the deterministic release stagger for
// one (provider, key) identity: FNV-1a of the client key, mod the
// stagger window. Deterministic per key (the same key always
// staggers the same amount — a retry wave stays spread instead of
// re-synchronising on a fresh random draw), and stable across
// restarts, so a restart does not reshuffle the release order.
func CooldownStaggerFor(key string) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return time.Duration(h.Sum64()%uint64(cooldownStaggerMax)) * time.Nanosecond
}

// effectiveCooldownUntil is a cooldown's admission end: the nominal
// end plus the key's release stagger (the anti-thundering-herd
// guard). The stored `until` is unchanged — only the gate moves.
func (e *Engine) effectiveCooldownUntil(key string, until time.Time) time.Time {
	return until.Add(CooldownStaggerFor(key))
}

// softCooldownDurationFor is softCooldownDuration with an explicit
// base: the profile base when no Retry-After hint is present
// (floored at 10 s when one is), doubling per consecutive failure,
// capped at 5 min. See softCooldownDuration for the full rule.
func softCooldownDurationFor(base time.Duration, retryAfter time.Duration, present bool, failures int) time.Duration {
	if present {
		base = retryAfter
		if base < RetryAfterFloor {
			base = RetryAfterFloor
		}
	}
	if failures <= 1 {
		if base > softCooldownMax {
			return softCooldownMax
		}
		return base
	}
	shift := failures - 1
	if shift > 10 {
		shift = 10
	}
	d := base * time.Duration(1<<uint(shift))
	if d <= 0 || d > softCooldownMax {
		return softCooldownMax
	}
	return d
}
