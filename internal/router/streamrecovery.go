// Stream-recovery early retry (M4.8, the sector baseline streamRecovery.ts /
// constants.ts STREAM_RECOVERY.EARLY_RETRY_MAX).
//
// When an upstream stream fails BEFORE any byte reaches the client
// (the preread holdback is still uncommitted), the failure is
// invisible to the client — so the same target's stream is
// transparently re-opened, up to 4 times (5 total attempts), before
// the chain spends a failover on the next target. A truncation, an
// empty stream, or a transport blip is often a one-off; re-opening
// the same target recovers it without burning the chain.
//
// The re-open is narrow on purpose: only truncations and transport
// failures (see isStreamEarlyRetryable). Status-bearing errors
// never re-open — a 5xx at stream start fails over at once, exactly
// as the Chat loop does (M4.5: 500 is breaker-owned, never
// same-host retried). 429s have their own retry path,
// model-not-found / client errors are properties of the target or
// the request (re-opening is pointless), refusals route to the
// refusal chain, and cancellations never retry. Every re-open
// counts toward MaxAttempts, and the loop-top breaker/cooldown
// gates still apply — a tripped breaker stops the re-opens. A
// re-open also never eats the attempt the next target needs (see
// attemptsLeaveOne): with MaxAttempts 5 a truncating first target
// re-opens at most 3 times, leaving one attempt for the failover.
package router

import (
	"errors"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// earlyRetryMax is the most transparent re-opens of one target's
// stream while the holdback is still uncommitted (the sector baseline: 4,
// "5 total attempts = 4 retries").
const earlyRetryMax = 4

// attemptsLeaveOne reports whether one more attempt may be spent
// while still leaving an attempt for the next target (when one
// exists). Same-target retries — transient, early-retry, wait
// budget — must not eat the attempt the next target needs: with
// MaxAttempts 5, a truncating first target re-opened 4 times
// would otherwise starve the failover entirely and fail a request
// its sibling could have served. attemptsUsed is the count before
// the next dispatch; spending one makes it attemptsUsed+1, so one
// must remain for the next target after that.
func attemptsLeaveOne(attemptsUsed, maxAttempts int, hasNext bool) bool {
	reserve := 0
	if hasNext {
		reserve = 1
	}
	return attemptsUsed+reserve < maxAttempts
}

// isStreamEarlyRetryable reports whether a pre-commit stream
// failure is worth re-opening the same target for. The retry
// covers TRUNCATIONS — a stream that ends without content or
// without a terminal marker, and untyped transport failures —
// the failures stream recovery exists for (the sector-typical
// TruncatedStreamError / EMPTY_TURN_RETRY_MAX). It deliberately
// does NOT cover status-bearing errors: a 5xx at stream start is
// a provider failure owned by the breaker (M4.5 excludes 500 from
// same-host retry for the same reason), a 429 has its own retry,
// and 4xx/model errors are properties of the target or the
// request. Those fail over at once, exactly as the Chat loop does.
func isStreamEarlyRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Status-bearing errors fail over at once — never re-opened.
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		return false
	}
	var rle *provider.RateLimitError
	if errors.As(err, &rle) {
		return false
	}
	var qe *provider.QuotaError
	if errors.As(err, &qe) {
		return false
	}
	// A panic in the provider client is a bug, not a blip —
	// re-opening the same panicking client cannot heal it.
	var pe *PanicError
	if errors.As(err, &pe) {
		return false
	}
	if !isRetryableFailure(err) {
		return false
	}
	// A dead model, a client error, an unrepresentable request
	// or a refusal will fail identically on a re-open.
	if isModelNotFoundErr(err) || isClientErrorFailure(err) {
		return false
	}
	if isReasoningUnrepresentableErr(err) {
		return false
	}
	if isContentRefusalErr(err) {
		return false
	}
	return true
}
