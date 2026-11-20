// Retry-After abandon rule + floor (CHECKLIST 167-168, competitor
// sweep gap; Portkey: Retry-After >= 60s ends retries for that
// target; CLIProxyAPI 10s floor, research-log entry 146).
//
// The two rules, and where each one binds:
//
//   - Abandon: a Retry-After of 60s or more ends retries for THAT
//     target in this request. The engine moves to the next target
//     immediately, or fails honestly if none remain — it never parks
//     the request sleeping out the wait and then retries the same
//     target. The abandon is per-target, never per-request: healthy
//     targets are still tried.
//   - Floor: a present Retry-After below 10s is treated as 10s in
//     the retry path — the same-target retry wait, the hard cooldown
//     and the soft-demotion cooldown. A poisoned or tiny hint
//     (including an explicit "0") can never cause a retry storm.
//
// A Retry-After that is absent or unparsable is NOT floored: it
// means "no hint", and the engine keeps exponential backoff for it.
// The distinction is carried by RateLimitError.RetryAfterPresent
// (and the wire error structs): a present 0 floors to 10s, an
// absent hint backs off. Treating "no hint" as a 10s wait would
// stall every header-less 429 for 10s — the opposite of the
// router's fail-fast design (CHECKLIST 104: a 429 fails over
// immediately without waiting out Retry-After).
//
// Interplay with RetryAfterCap (default 30s): the cap still bounds
// the wait for a honoured hint (a 45s hint waits 30s), and it
// bounds the cross-request cooldown of an abandoned target (a 120s
// hint cools the target for 30s — the router never honours more
// than the cap of waiting). The cap never turns an abandon into a
// retry: >= 60s abandons however small the cap is.
package router

import (
	"errors"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// RetryAfterAbandonAt is the abandon threshold: a present
// Retry-After at or above this ends retries for that target.
const RetryAfterAbandonAt = 60 * time.Second

// RetryAfterFloor is the minimum wait a present Retry-After is
// honoured with in the retry path.
const RetryAfterFloor = 10 * time.Second

// Retry-After decisions recorded on the route trace (see
// routetrace.RetryAfterInfo) and rendered by the dashboard's Live
// Routing view. Exactly one applies per rate-limited attempt.
const (
	// RetryDecisionAbandoned: the hint was >= 60s; the target
	// was abandoned for this request (no same-target retry).
	RetryDecisionAbandoned = "abandoned"
	// RetryDecisionQuotaExhausted: the 429 body signals quota exhaustion;
	// the target is abandoned for this request (M4.4).
	RetryDecisionQuotaExhausted = "quota_exhausted"
	// RetryDecisionConcurrencyLimited: the 429 is a concurrency-limit
	// rejection; the target cools for 1 s and the request fails over now
	// (M4.4).
	RetryDecisionConcurrencyLimited = "concurrency_limited"
	// RetryDecisionFloored: the hint was present but below 10s;
	// the retry path waited the 10s floor instead.
	RetryDecisionFloored = "floored"
	// RetryDecisionCapped: the hint was honoured but cut down
	// to Config.RetryAfterCap.
	RetryDecisionCapped = "capped"
	// RetryDecisionHonoured: the hint was waited as given.
	RetryDecisionHonoured = "honoured"
)

// ConcurrencyLimitCooldown is the cooldown a concurrency-limit 429
// earns (M4.4, OmniRoute concurrencyLimit429.ts COOLDOWN 1,000 ms):
// fail over now, cool 1 s, no backoff climb, no breaker failure.
const ConcurrencyLimitCooldown = 1 * time.Second

// QuotaExhaustedCooldown is the cooldown a quota-exhausted 429 earns
// when it carries no Retry-After hint (M4.4): the account cannot
// serve, so the target rests a minute instead of being retried
// into the same empty quota. A present hint cools for the honoured
// (floored/capped) wait instead.
const QuotaExhaustedCooldown = 60 * time.Second

// rateLimitHint extracts the Retry-After hint from an error: the
// duration, whether a hint was present at all, and whether the
// error is a rate limit. Only *provider.RateLimitError carries a
// hint; every other error reports present=false.
func rateLimitHint(err error) (retryAfter time.Duration, present bool, isRateLimit bool) {
	var rle *provider.RateLimitError
	if errors.As(err, &rle) {
		return rle.RetryAfter, rle.RetryAfterPresent, true
	}
	return 0, false, false
}

// retryAfterAbandons reports whether a present Retry-After ends
// retries for its target. An absent hint never abandons.
func retryAfterAbandons(retryAfter time.Duration, present bool) bool {
	return present && retryAfter >= RetryAfterAbandonAt
}

// retryAfterHonouredWait is the wait a present Retry-After is
// honoured with, before the abandon rule is applied: floored at
// 10s, capped at cap (a non-positive cap means "no cap"). It is 0
// for an absent hint. The abandon rule short-circuits it in
// classifyRetryAfter; the cross-request cooldowns use it directly
// (an abandoned target still cools for the capped wait, so the
// next request does not hammer it immediately).
func retryAfterHonouredWait(retryAfter time.Duration, present bool, cap time.Duration) time.Duration {
	if !present {
		return 0
	}
	if retryAfter < RetryAfterFloor {
		return RetryAfterFloor
	}
	if cap > 0 && retryAfter > cap {
		return cap
	}
	return retryAfter
}

// classifyRetryAfter decides what the retry path does with one
// Retry-After hint. It returns the wait to sleep before retrying
// the same target (0 when the target is abandoned — the caller
// must not sleep-and-retry it), the decision string for the trace,
// and whether the target is abandoned for this request. An absent
// hint returns wait 0, decision "" and abandon false: the caller
// falls back to exponential backoff.
func classifyRetryAfter(retryAfter time.Duration, present bool, cap time.Duration) (wait time.Duration, decision string, abandon bool) {
	if !present {
		return 0, "", false
	}
	if retryAfterAbandons(retryAfter, present) {
		return 0, RetryDecisionAbandoned, true
	}
	wait = retryAfterHonouredWait(retryAfter, present, cap)
	switch {
	case retryAfter < RetryAfterFloor:
		return wait, RetryDecisionFloored, false
	case cap > 0 && retryAfter > cap:
		return wait, RetryDecisionCapped, false
	default:
		return wait, RetryDecisionHonoured, false
	}
}
