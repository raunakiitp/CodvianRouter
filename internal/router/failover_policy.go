// Cross-provider attempt policy + failover-before-retry knobs
// (G11 Area 2, parity tracker Area 2 rows "Default cross-provider
// policy: up to 3 provider attempts; prefixed provider/model never
// rerouted" and "Failover-before-retry + retry knobs").
//
// the reference router's evidence: reference provider-failover doc ("The
// default cross-provider policy allows up to three provider
// attempts... A prefixed id (provider/model) is never rerouted to
// another provider") and comboConfig.ts:113,164-178 (maxRetries /
// retryDelayMs / maxSetRetries; combo defaults maxRetries 1,
// retryDelayMs 2000 ms).
//
// Cross-provider default: at most DefaultMaxProvidersPerRequest
// (3) DISTINCT providers are attempted per request. Same-provider
// retries (another key of the same provider, a transient retry)
// do not consume the budget — the budget is about providers, not
// attempts. The overall cap stays MaxAttempts (5): the 3-provider
// default sits under it, so a request with three failing providers
// stops after three, and a request with one provider retrying
// still stops at five attempts.
//
// A prefixed provider/model is never rerouted. At engine level
// the prefix arrives as routing.only naming exactly one provider
// (RequestRouting.Only — the same control the per-provider v1
// routes set, provider_v1.go): applyContract narrows the chain
// to it before the loop, and the loop guard below re-checks every
// attempt, so no path — refusal chains included — can ever dial
// another provider for the request. This is deliberately NOT
// ChatRequest.PinProviderID: the continuation pin is a
// preference, not a narrowing (TestResponsesContinuationPin — a
// pinned provider that fails DOES fail over to the rest of the
// chain, and an unknown pin keeps the full chain). Conflating
// the two broke that test; the narrowing lives on routing.only
// only. Whether the server translates a "provider/model" model
// string into routing.only on the plain chat path is NOT verified
// in this slice — see the G11 report; the engine enforces the
// narrowing for any caller that sets it.
//
// Failover-before-retry knobs (reference-compatible semantics):
//   - FailoverMaxRetries: max same-target retries after the first
//     failure, per target (the transient 502/503/504 path). 0
//     means the default (3 — the reference router's transientBackendRetry
//     max, and our existing TransientMaxRetries); negative means
//     no same-target retry at all. Failover-first is preserved:
//     429s still fail over immediately and never consume a retry.
//   - FailoverRetryDelay: the base delay between same-target
//     retries (decorrelated full-jitter ladder, transient.go).
//     0 means the default (200 ms, TransientBackoffBase).
//     Deliberate divergence: the reference router's combo default is 2000 ms
//     — ours stays 200 ms, because a 2 s floor on a same-host
//     blip retry would make the failover-first path (which is
//     usually faster than any wait) pointless for the common case.
//   - FailoverMaxSetRetries: max same-target retries across the
//     whole set (the whole chain for this request). 0 means no
//     set cap (the reference router's maxSetRetries is optional the same
//     way); a positive value stops same-target retries request-
//     wide once spent, even when the per-target budget remains.
package router

import (
	"time"

	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// DefaultMaxProvidersPerRequest is the default cross-provider
// attempt budget: at most this many distinct providers per request.
const DefaultMaxProvidersPerRequest = 3

// DefaultFailoverMaxRetries is the default same-target retry
// budget (the reference router transientBackendRetry max 3, our existing
// TransientMaxRetries).
const DefaultFailoverMaxRetries = TransientMaxRetries

// DefaultFailoverRetryDelay is the default base delay between
// same-target retries (TransientBackoffBase, 200 ms).
const DefaultFailoverRetryDelay = TransientBackoffBase

// maxProvidersPerRequest returns the configured cross-provider
// budget (at least 1 — withDefaults floors 0 to the default, and
// a negative value is clamped here so a misconfiguration can
// never make the loop attempt nothing).
func (e *Engine) maxProvidersPerRequest() int {
	if e.cfg.MaxProvidersPerRequest < 1 {
		return DefaultMaxProvidersPerRequest
	}
	return e.cfg.MaxProvidersPerRequest
}

// resolvedOnly returns the request's routing.only list as the
// contract resolved it (nil when the request carries no contract
// or no only-list). The RESOLVED list is what the guard must use:
// applyContract resolves display names to provider IDs
// ("Cheap One" → "cheap"), so the raw request list would compare
// a display name against a provider ID and refuse the only
// provider the request is allowed to use. A nil contract means
// no narrowing was requested (or it was rejected upstream with a
// 400 before the loop) — the guard then narrows nothing.
func resolvedOnly(contract *routetrace.RoutingContract) []string {
	if contract == nil {
		return nil
	}
	return contract.Only
}

// providerAttemptAllowed reports whether the next attempt may try
// `provider` in this request: false when routing.only narrows the
// request to other providers (a prefixed provider/model is never
// rerouted), and false when `provider` is a new provider and the
// distinct-provider budget is already spent. `tried` is the
// request's map of providers already attempted. Same-provider
// retries are always allowed by this check (they are governed by
// MaxAttempts and the retry knobs instead). Note the pin is
// deliberately absent here: PinProviderID is a preference, not a
// narrowing (see the file header).
func providerAttemptAllowed(only []string, tried map[string]bool, provider string, maxProviders int) bool {
	if len(only) > 0 {
		allowed := false
		for _, p := range only {
			if p == provider {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	if !tried[provider] && len(tried) >= maxProviders {
		return false
	}
	return true
}

// failoverMaxRetries returns the configured per-target same-target
// retry budget: 0 means the default (3); a negative value means
// no same-target retry.
func (e *Engine) failoverMaxRetries() int {
	if e.cfg.FailoverMaxRetries < 0 {
		return 0
	}
	if e.cfg.FailoverMaxRetries == 0 {
		return DefaultFailoverMaxRetries
	}
	return e.cfg.FailoverMaxRetries
}

// failoverRetryDelayBase returns the configured base delay between
// same-target retries: 0 means the default (200 ms).
func (e *Engine) failoverRetryDelayBase() time.Duration {
	if e.cfg.FailoverRetryDelay <= 0 {
		return DefaultFailoverRetryDelay
	}
	return e.cfg.FailoverRetryDelay
}

// transientRetryAllowed reports whether one more same-target
// transient retry may run: the per-target budget must have room
// AND the set-wide budget (when configured) must have room.
func (e *Engine) transientRetryAllowed(transientCount, setRetriesUsed int) bool {
	if transientCount >= e.failoverMaxRetries() {
		return false
	}
	if e.cfg.FailoverMaxSetRetries > 0 && setRetriesUsed >= e.cfg.FailoverMaxSetRetries {
		return false
	}
	return true
}
