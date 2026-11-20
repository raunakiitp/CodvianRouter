// Soft-demotion cooldown (research-log entry 183, SparkRouter).
//
// The hard cooldown (entries 10/43) SKIPS a cooling provider: while
// its cooldown runs, the engine never tries it. That is safe when a
// chain has many healthy targets, but a local router often has two
// or three — skipping the cooling one can leave nothing to try.
//
// The soft-demotion cooldown is the alternative, opt-in per router
// or per chain (config `routing.cooldown_mode`, default `hard` —
// behaviour is unchanged unless the operator opts in):
//
//   - On a retryable failure of a target (rate limit, 5xx/529,
//     402/404, transport error — see isRetryableFailure), the target
//     starts a cooldown of the upstream's Retry-After in seconds
//     (floored at 10s when present — CHECKLIST 168), otherwise 5 s,
//     doubling with each consecutive failure up to 5 min. A success
//     ends the cooldown.
//   - While cooling, the target moves to the END of the chain order
//     — never skipped. It is still tried once, so a request fails
//     only if every target fails.
//   - In soft mode a retryable failure never trips the circuit
//     breaker: tripping it would skip the provider outright, which
//     is exactly what demotion refuses to do. Non-retryable 4xx
//     failures (400/401/403/422, unrepresentable input) never trip
//     the breaker in either mode — a bad key or a bad request is a
//     property of the request or the operator's key, not a
//     provider-health signal (see router.isClientErrorFailure);
//     only 408/5xx and transport failures count toward it.
package router

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Cooldown modes. CooldownHard is the default: a cooling provider
// is skipped until its cooldown ends. CooldownSoftDemote demotes it
// to the end of the order instead — still tried once per request.
const (
	CooldownHard       = "hard"
	CooldownSoftDemote = "soft-demote"
)

// IsCooldownMode reports whether s names a known cooldown mode
// ("" counts: it means "inherit the default", i.e. hard).
func IsCooldownMode(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", CooldownHard, CooldownSoftDemote:
		return true
	}
	return false
}

// NormalizeCooldownMode lowercases/trims s and resolves "" to
// CooldownHard. Unknown names also resolve to CooldownHard — the
// engine never errors on a mode name; config validation rejects
// unknown names at load time instead.
func NormalizeCooldownMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case CooldownSoftDemote:
		return CooldownSoftDemote
	default:
		return CooldownHard
	}
}

// Soft-cooldown timing (entry 183): base 5 s, or the upstream's
// Retry-After when one was given — floored at 10s (CHECKLIST 168:
// a present Retry-After below 10s is treated as 10s in the retry
// path, so a poisoned or tiny hint can never cause a retry storm;
// an explicit 0 floors to 10s, an absent hint keeps the 5 s base);
// doubling per consecutive failure; hard cap 5 min.
const (
	softCooldownBase = 5 * time.Second
	softCooldownMax  = 5 * time.Minute
)

// softCooldownState is one provider's live soft cooldown: when it
// ends, and how many consecutive retryable failures built it.
type softCooldownState struct {
	until    time.Time
	failures int
}

// CoolingTarget is one provider currently cooling, for the
// dashboard feed (GET /dashboard/cooldown). Mode is "hard" or
// "soft-demote"; RemainingSeconds is the whole seconds left until
// the cooldown ends (at least 1 while active); Failures is the
// consecutive-failure count that built a soft cooldown (0 for a hard
// one, which carries no count); Demoted is true when the target is
// moved to the end of the order instead of skipped (soft mode only).
// KeyID names the cooling key inside a multi-key pool (CHECKLIST
// 175): "" means the provider's legacy single key — the whole
// provider cools. A non-empty KeyID means only that key cools; the
// provider's other keys keep serving.
type CoolingTarget struct {
	ProviderID       string
	KeyID            string
	Mode             string
	Until            time.Time
	RemainingSeconds int64
	Failures         int
	Demoted          bool
}

// cooldownModeFor resolves the cooldown mode for one chain (model).
// Precedence, highest first: a runtime per-chain override
// (SetChainCooldownMode), the configured ChainCooldownMode for the
// model, a runtime global override (SetCooldownMode), the
// configured global CooldownMode, and finally hard.
func (e *Engine) cooldownModeFor(model string) string {
	e.mu.Lock()
	runtimeChain, hasRuntimeChain := e.runtimeCooldownChains[model]
	runtimeGlobal := e.runtimeCooldownMode
	e.mu.Unlock()
	if hasRuntimeChain && runtimeChain != "" {
		return NormalizeCooldownMode(runtimeChain)
	}
	if e.cfg.ChainCooldownMode != nil {
		if m := e.cfg.ChainCooldownMode(model); m != "" {
			return NormalizeCooldownMode(m)
		}
	}
	if runtimeGlobal != "" {
		return NormalizeCooldownMode(runtimeGlobal)
	}
	return NormalizeCooldownMode(e.cfg.CooldownMode)
}

// SetCooldownMode overrides the global cooldown mode at runtime
// (e.g. from the dashboard). "" clears the override and restores
// the configured Config.CooldownMode. Unknown names are stored
// verbatim and resolve to hard via NormalizeCooldownMode.
func (e *Engine) SetCooldownMode(mode string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runtimeCooldownMode = strings.TrimSpace(mode)
}

// SetChainCooldownMode overrides the cooldown mode for one chain
// (model) at runtime. "" clears the override for that model (it
// inherits again).
func (e *Engine) SetChainCooldownMode(model, mode string) {
	if model == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if strings.TrimSpace(mode) == "" {
		delete(e.runtimeCooldownChains, model)
		return
	}
	e.runtimeCooldownChains[model] = strings.TrimSpace(mode)
}

// CooldownState returns the runtime cooldown-mode overrides: the
// runtime global ("" when unset — the configured global applies)
// and a copy of the runtime per-chain overrides. Configured values
// (from Config) are not included: this is the mutable state only,
// so the dashboard can tell what the operator changed live.
func (e *Engine) CooldownState() (global string, chains map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	chains = make(map[string]string, len(e.runtimeCooldownChains))
	for k, v := range e.runtimeCooldownChains {
		chains[k] = v
	}
	return e.runtimeCooldownMode, chains
}

// softCooldownDuration computes the next soft-cooldown length for a
// target with failures consecutive retryable failures (1-based:
// failures==1 is the first failure). Base is the upstream's
// Retry-After when one was given (present), floored at
// RetryAfterFloor (10s) — a present 0s/1s/5s hint cools for 10s,
// never less; else the legacy 5 s base; each further consecutive failure doubles
// it; the result is capped at 5 min.
//
// The legacy 5 s base is kept for this wrapper (existing tests
// assert it). The live path (startSoftCooldown) uses
// softCooldownDurationFor with the per-auth-profile base (G11 Area
// 2: OAuth 5 s / API-key 3 s / Local 2 s — see cooldown_auth.go).
func softCooldownDuration(retryAfter time.Duration, present bool, failures int) time.Duration {
	return softCooldownDurationFor(softCooldownBase, retryAfter, present, failures)
}

// startSoftCooldown records one more consecutive retryable failure
// for one (provider, key) identity — key is a ClientKey, so a
// multi-key provider cools one key at a time (CHECKLIST 175) —
// and (re)starts its soft cooldown. retryAfter is the upstream's
// Retry-After hint and present whether one was given (an absent
// hint cools from the 5 s base; a present hint floors at 10s —
// see softCooldownDuration).
func (e *Engine) startSoftCooldown(key string, retryAfter time.Duration, present bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.softCooldowns[key]
	if st == nil {
		st = &softCooldownState{}
		e.softCooldowns[key] = st
	}
	st.failures++
	// G11 Area 2: the ladder base is per-auth-profile (OAuth 5 s /
	// API-key 3 s / Local 2 s) instead of the uniform 5 s — see
	// cooldownBaseFor / softCooldownDurationFor.
	providerID, _ := SplitClientKey(key)
	st.until = time.Now().Add(softCooldownDurationFor(e.cooldownBaseFor(providerID), retryAfter, present, st.failures))
}

// setSoftCooldownUntil sets one (provider, key) identity's soft
// cooldown to end after exactly d, without touching its consecutive
// failure count (M4.4): a concurrency-limit 429 cools for 1 s and
// must not climb the backoff ladder, so it neither increments the
// count nor inherits the doubling — the plain startSoftCooldown
// would floor the hint at 10 s and count the failure.
func (e *Engine) setSoftCooldownUntil(key string, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.softCooldowns[key]
	if st == nil {
		st = &softCooldownState{}
		e.softCooldowns[key] = st
	}
	st.until = time.Now().Add(d)
}

// clearSoftCooldown ends one (provider, key) identity's soft
// cooldown (a success ends the cooldown — key is a ClientKey, so
// a success on one key never clears another key's cooldown). It
// also resets the consecutive-failure count, so the next failure
// starts again from the base duration.
func (e *Engine) clearSoftCooldown(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.softCooldowns, key)
}

// softCooling reports whether one (provider, key) identity — key
// is a ClientKey — is inside a live soft cooldown, and its state.
// An expired entry is pruned and reports not-cooling (the failure
// count survives expiry: the next failure still doubles from it,
// because the failures were consecutive — only a success resets
// the count).
func (e *Engine) softCooling(key string) (until time.Time, failures int, cooling bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.softCooldowns[key]
	if st == nil {
		return time.Time{}, 0, false
	}
	// G11 Area 2: the admission end is the nominal end plus the
	// key's release stagger (anti-thundering-herd guard — see
	// cooldown_auth.go). The expiry check uses the staggered
	// end, so a key is not re-admitted at full priority inside
	// its own stagger window.
	until = e.effectiveCooldownUntil(key, st.until)
	if !time.Now().Before(until) {
		if time.Now().After(until) {
			// Expired: the demotion lifts, but the count stays
			// until a success clears it.
			st.until = time.Time{}
		}
		return time.Time{}, 0, false
	}
	return until, st.failures, true
}

// CooldownTargets returns every provider currently cooling — soft
// cooldowns (demoted to the end of the order) and hard cooldowns
// (skipped until they end) — in provider-ID order, then key-ID
// order. Cooldowns are keyed by (provider, key) (CHECKLIST 175):
// a multi-key provider reports one entry per cooling key, so the
// dashboard can show exactly which key is down instead of blaming
// the whole provider. Empty before any retryable failure: the
// honest empty state, never invented targets. A hard cooldown entry
// carries Failures 0 (the hard path keeps no consecutive-failure
// count) and Demoted false.
func (e *Engine) CooldownTargets() []CoolingTarget {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []CoolingTarget{}
	for key, st := range e.softCooldowns {
		if st == nil || !now.Before(st.until) {
			continue
		}
		remaining := int64(math.Ceil(st.until.Sub(now).Seconds()))
		if remaining < 1 {
			remaining = 1
		}
		providerID, keyID := SplitClientKey(key)
		out = append(out, CoolingTarget{
			ProviderID:       providerID,
			KeyID:            keyID,
			Mode:             CooldownSoftDemote,
			Until:            st.until,
			RemainingSeconds: remaining,
			Failures:         st.failures,
			Demoted:          true,
		})
	}
	for key, until := range e.cooldowns {
		if !now.Before(until) {
			continue
		}
		remaining := int64(math.Ceil(until.Sub(now).Seconds()))
		if remaining < 1 {
			remaining = 1
		}
		providerID, keyID := SplitClientKey(key)
		out = append(out, CoolingTarget{
			ProviderID:       providerID,
			KeyID:            keyID,
			Mode:             CooldownHard,
			Until:            until,
			RemainingSeconds: remaining,
			Failures:         0,
			Demoted:          false,
		})
	}
	// Sort by provider ID, then key ID (stable, deterministic feed order).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j].ProviderID < out[j-1].ProviderID ||
			(out[j].ProviderID == out[j-1].ProviderID && out[j].KeyID < out[j-1].KeyID)); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// isRetryableFailure reports whether err is a retryable failure of
// that target — the kind that starts (or extends) a soft cooldown.
// Rate limits, retryable upstream errors (5xx/529/402/404 per the
// shared taxonomy), a 402 quota error (that target's billing), and
// untyped transport errors from a real client call (connect/read
// failures) all count. Context cancellations, the engine's own
// deadline, limiter sheds, and non-retryable typed errors (400/401/
// 403/422, unrepresentable input) do not.
func isRetryableFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, adaptive.ErrLimited) {
		return false
	}
	var rle *provider.RateLimitError
	if errors.As(err, &rle) {
		return true
	}
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		return ue.Retryable
	}
	var qe *provider.QuotaError
	if errors.As(err, &qe) {
		// 402 is that target's billing/quota state; another
		// target may serve. Other quota errors (e.g. a 403
		// insufficient-quota on a key shared by every target)
		// are not demoted — they are not that target's alone.
		return qe.StatusCode == 402
	}
	// An untyped error from a real client call is a transport
	// failure (connect/read/request) — retryable by definition.
	return true
}

// retryAfterOf extracts the Retry-After hint from an error (0 and
// present=false when the error carries none — only RateLimitError
// carries one; other retryable errors cool down from the 5 s base).
func retryAfterOf(err error) (time.Duration, bool) {
	if ra, present, ok := rateLimitHint(err); ok {
		return ra, present
	}
	return 0, false
}
