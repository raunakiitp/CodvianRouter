package router

// Upstream rate-limit learning (M5.13): the engine learns from the
// rate-limit headers every upstream response carries, and paces the
// Gradient2 cap from them — slowing down BEFORE the 429 instead of
// discovering the limit by hitting it.
//
// The pacing is per provider (the limiter's existing granularity):
// the most restrictive learned signal across the provider's keys
// wins, because the limiter only ever shrinks the cap on a learned
// signal and the cap is shared by the provider's keys. The
// PERSISTENCE is per (provider, key) — the ledger row keeps each
// key's own signal, so a restart reloads every key's latest numbers
// and the tightest one paces the provider again.
//
// The live path is the in-memory limiter (synchronous, on every
// response). The ledger write is durability only: it runs in a
// goroutine and its errors are dropped — a slow or failing store
// must never add latency to the hot path or fail a request that
// already succeeded upstream.

import (
	"context"
	"errors"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// RateLimitStore persists learned upstream rate limits. Only the
// write side lives here: the read side (startup load) is the
// ledger's LearnedRateLimits, called by main.
type RateLimitStore interface {
	LearnRateLimit(ctx context.Context, providerID, keyID string, sig adaptive.RateLimitSignals, now time.Time) error
}

// learnRateLimit paces the provider's adaptive cap from one parsed
// signal, records the measured remaining fraction as the engine's
// quota hint (so the next request's ordering sees a real number
// instead of the unknown default), and persists it per
// (provider, key). A nil or empty signal is a no-op — an upstream
// that sends no headers teaches nothing, and the limiter keeps its
// Gradient2 behaviour. The recorded fraction expires at the
// earliest window reset (capped at the 5-minute signal TTL), so a
// provider that reported 0 remaining is skipped only until its
// window refills instead of forever.
func (e *Engine) learnRateLimit(att Attempt, sig *adaptive.RateLimitSignals) {
	if sig == nil || sig.Empty() {
		return
	}
	now := time.Now()
	e.limiter.ApplyLearnedLimit(att.ProviderID, *sig, now)
	// Only a signal that actually carries request/token counts
	// teaches a fraction — a Retry-After-only signal has no ratio
	// (RemainingFraction would read 0 for "nothing learned").
	if sig.HasRequests || sig.HasTokens {
		e.setLearnedQuotaHint(att.ProviderID, int(sig.RemainingFraction()*100), quotaHintExpiresAt(*sig, now))
		// The same measurement at per-key granularity (B2-09 /
		// the sector baseline #20561): the engine knows which key made
		// this request, so the key's own remaining fraction is
		// recorded alongside the provider's — the per-key quota
		// signal the account-rotation cap check reads. Same
		// fraction, same expiry, no separate invention.
		e.setLearnedKeyQuotaHint(att.ProviderID, att.KeyID, int(sig.RemainingFraction()*100), quotaHintExpiresAt(*sig, now))
	}
	if e.cfg.RateLimitStore == nil {
		return
	}
	store := e.cfg.RateLimitStore
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = store.LearnRateLimit(ctx, att.ProviderID, att.KeyID, *sig, now)
	}()
}

// setLearnedQuotaHint records a measured remaining-quota fraction
// (0..100) for a provider, expiring at expiresAt. It is the
// live-path producer for the quota hint: every upstream chat/stream
// response (and every 429) flows through learnRateLimit, so the hint
// tracks the real remaining budget instead of staying at the unknown
// default. Synchronous and cheap (one map write under e.mu) —
// unlike Registry.SetQuotaHint it never touches disk, so it is safe
// on the hot path.
func (e *Engine) setLearnedQuotaHint(providerID string, hint int, expiresAt time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.quotaHints[providerID] = quotaHintEntry{hint: hint, expiresAt: expiresAt}
}

// setLearnedKeyQuotaHint records a measured remaining-quota
// fraction (0..100) for one (provider, key) pair, expiring at
// expiresAt — the per-key twin of setLearnedQuotaHint (B2-09 /
// the sector baseline #20561). Same cost (one map write under e.mu), same
// source (real upstream rate-limit headers, via learnRateLimit).
func (e *Engine) setLearnedKeyQuotaHint(providerID, keyID string, hint int, expiresAt time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.keyQuotaHints[ClientKey(providerID, keyID)] = quotaHintEntry{hint: hint, expiresAt: expiresAt}
}

// LearnedKeyQuotaHint reports the engine's recorded
// remaining-quota fraction for one (provider, key) pair (0..100)
// together with the instant it stops being trustworthy — the
// per-key twin of LearnedQuotaHint, with identical expiry-drop
// semantics: ok is false when the engine holds no entry for the
// key, or when a learned fraction has expired; expired entries
// are dropped, so the key is eligible again and the next
// response can teach a fresh fraction. A zero expiresAt means
// the entry never expires (operator form, never written by the
// live path). This is the read side the account-rotation cap
// check uses (PARITY row :509 / B2-09): the measured per-key
// remaining finally reaches rotation, instead of rotation
// seeing only the provider-level hint.
func (e *Engine) LearnedKeyQuotaHint(providerID, keyID string) (hint int, expiresAt time.Time, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, found := e.keyQuotaHints[ClientKey(providerID, keyID)]
	if !found {
		return 0, time.Time{}, false
	}
	if !h.expiresAt.IsZero() && !time.Now().Before(h.expiresAt) {
		delete(e.keyQuotaHints, ClientKey(providerID, keyID))
		return 0, time.Time{}, false
	}
	return h.hint, h.expiresAt, true
}

// RestoreLearnedKeyQuotaHint records a persisted per-key
// remaining-quota fraction at startup (the restore in main.go
// replays the ledger's per-(provider, key) learned signals).
// It is the per-key twin of the SetQuotaHint restore, but it
// carries an expiry — unlike SetQuotaHint, a restored 0 is
// safe here: the entry expires at the upstream window reset,
// so the key is retried after its window refills instead of
// staying skipped. A signal with no request/token counts
// teaches nothing and is a no-op.
func (e *Engine) RestoreLearnedKeyQuotaHint(providerID, keyID string, sig adaptive.RateLimitSignals, now time.Time) {
	if !sig.HasRequests && !sig.HasTokens {
		return
	}
	e.setLearnedKeyQuotaHint(providerID, keyID, int(sig.RemainingFraction()*100), quotaHintExpiresAt(sig, now))
}

// LearnedQuotaHint reports the engine's recorded remaining-quota
// fraction for a provider (0..100) together with the instant it
// stops being trustworthy — the earliest upstream window reset the
// fraction was learned from (see quotaHintExpiresAt), capped at the
// signal TTL. The entry is whatever setLearnedQuotaHint recorded
// from real upstream rate-limit headers, a positive fraction
// restored at startup from the persisted learned signal, or an
// operator override (SetQuotaHint, which never expires): always a
// real recorded number — a measurement, a persisted measurement,
// or an explicit operator decision — never invented. ok is false
// when the engine holds no entry for the provider, or when a
// learned fraction has expired; expired entries are dropped,
// exactly as quotaHint does, so the provider is eligible again and
// the next response can teach a fresh fraction. A zero expiresAt
// means the entry never expires (operator form): callers that need
// a deadline must treat it as "no expiry known", never as a
// deadline of their own invention. This is the read side the
// strategy signals use (PARITY rows :495/:497): the measured
// fraction and its reset instant finally reach the registry,
// instead of the learned hint feeding only filterZeroQuota.
func (e *Engine) LearnedQuotaHint(providerID string) (hint int, expiresAt time.Time, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, found := e.quotaHints[providerID]
	if !found {
		return 0, time.Time{}, false
	}
	if !h.expiresAt.IsZero() && !time.Now().Before(h.expiresAt) {
		delete(e.quotaHints, providerID)
		return 0, time.Time{}, false
	}
	return h.hint, h.expiresAt, true
}

// quotaHintExpiresAt returns when a learned fraction stops being
// trustworthy: the earliest window reset the signal carries, capped
// at the 5-minute signal TTL (adaptive.RateLimitTTL) from
// observation — matching RateLimitSignals.Fresh. A signal with no
// reset time expires at the TTL.
func quotaHintExpiresAt(sig adaptive.RateLimitSignals, now time.Time) time.Time {
	observed := sig.ObservedAt
	if observed.IsZero() {
		observed = now
	}
	exp := observed.Add(adaptive.RateLimitTTL)
	for _, reset := range []*time.Time{sig.RequestsResetAt, sig.TokensResetAt} {
		if reset != nil && !reset.IsZero() && reset.After(now) && reset.Before(exp) {
			exp = *reset
		}
	}
	return exp
}

// learnRateLimitError paces from a 429 error that carries parsed
// signals (the OpenAI-compatible path attaches them in
// mapStatusErrorForEndpoint). Other errors carry no signals.
func (e *Engine) learnRateLimitError(att Attempt, err error) {
	var rlErr *provider.RateLimitError
	if errors.As(err, &rlErr) && rlErr.Signals != nil {
		e.learnRateLimit(att, rlErr.Signals)
	}
}

// ApplyLearnedRateLimit paces one provider's cap from a persisted
// signal (the startup load in main.go). No persistence: the signal
// is already stored.
func (e *Engine) ApplyLearnedRateLimit(providerID string, sig adaptive.RateLimitSignals, now time.Time) {
	e.limiter.ApplyLearnedLimit(providerID, sig, now)
}
