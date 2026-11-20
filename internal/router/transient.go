// Same-host transient retry (M4.5, OmniRoute transientBackendRetry.ts).
//
// A 502/503/504 from a target is usually a blip on that host, not
// a dead provider: retrying the SAME host — up to 3 times, with
// decorrelated full-jitter backoff (base 200 ms, cap 2,000 ms) —
// recovers it without spending the chain on a failover. Only after
// the retries are spent does the chain move on. The retry re-enters
// the attempt loop without advancing the index, so the
// breaker/cooldown/budget gates still apply on every retry: a
// tripped breaker stops the retries (skipped_breaker) instead of
// being ignored, and every retry counts toward MaxAttempts.
//
// Decorrelated full jitter (the AWS Architecture Blog form): each
// sleep is drawn uniformly from [base, min(cap, prev*3)], with
// prev starting at base. The ladder is per request (transientPrev
// in the loop); the count is per target (reset when the attempt
// identity changes).
package router

import (
	"context"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TransientMaxRetries is the most same-host retries after the
// first failure (OmniRoute: max 3).
const TransientMaxRetries = 3

// TransientBackoffBase is the first transient-retry delay (200 ms).
const TransientBackoffBase = 200 * time.Millisecond

// TransientBackoffCap is the most a transient retry ever waits (2 s).
const TransientBackoffCap = 2 * time.Second

// isTransientStatus reports whether err is a same-host transient
// failure: an HTTP 502, 503 or 504 (OmniRoute's transient set).
// 500 is NOT transient (a 500 is a provider failure, owned by the
// breaker); 408/429 have their own retry paths; transport errors
// have no status and fail over instead.
func isTransientStatus(err error) bool {
	if err == nil {
		return false
	}
	switch provider.ClassifyError(err, provider.EndpointChatCompletions).Status {
	case 502, 503, 504:
		return true
	}
	return false
}

// transientDelay draws the next decorrelated full-jitter delay from
// prev (0 on the first retry): uniform in [base, min(cap, prev*3)].
// The engine's rand is used (randMu-guarded), never a fresh source.
// G11 Area 2: the base is the configured FailoverRetryDelay
// (default 200 ms) — see failover_policy.go.
func (e *Engine) transientDelay(prev time.Duration) time.Duration {
	base := e.failoverRetryDelayBase()
	cap_ := TransientBackoffCap
	hi := base
	if prev > 0 {
		hi = prev * 3
		if hi > cap_ {
			hi = cap_
		}
		if hi < base {
			hi = base
		}
	}
	e.randMu.Lock()
	f := e.rand.Float64()
	e.randMu.Unlock()
	return base + time.Duration(f*float64(hi-base))
}

// transientSleep sleeps d (or until ctx ends) and reports whether
// the sleep completed. It is e.sleep with the transient name, so
// the call sites read as what they are.
func (e *Engine) transientSleep(ctx context.Context, d time.Duration) bool {
	return e.sleep(ctx, d)
}
