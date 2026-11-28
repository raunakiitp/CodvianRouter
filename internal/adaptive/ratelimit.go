package adaptive

// Upstream rate-limit learning (M5.13): parse the rate-limit headers
// an upstream sends on every response and pace the Gradient2 cap
// from them, so the router slows down BEFORE the 429 instead of
// discovering the limit by hitting it.
//
// The parser is a faithful port of the sector-typical storeRateLimitHeaders
// (src/lib/quota/saturationSignals.ts): request headers (Anthropic
// anthropic-ratelimit-requests-*, OpenAI x-ratelimit-limit-requests /
// -remaining-requests, generic x-ratelimit-limit / -remaining) and
// token headers (first usable triple wins: Anthropic tokens, OpenAI
// x-ratelimit-*-tokens, Anthropic input-tokens, Anthropic
// output-tokens). Resets normalize to absolute times: Anthropic
// RFC3339 → parsed, OpenAI Go-style durations ("6m0s", "1.5s") →
// now + duration, plain seconds → now + seconds, HTTP dates →
// parsed. Entries live 5 minutes (the sector-typical RL_HEADER_TTL_MS).
//
// Pacing rule: the learned remaining fraction (remaining / limit,
// most restrictive of the request/token signals) scales the cap:
// paced = clamp(MaxLimit * fraction, MinLimit, MaxLimit). At 90%
// remaining the cap is ~90 (no real constraint on a default-20
// limiter); at 10% it is 10; at 0% it is the floor (2) — a slow
// probe stays possible, matching the limiter's floor philosophy.
// The pacing only ever shrinks the stored cap, never raises it, so
// when a window resets the cap recovers gradually through normal
// Gradient2 growth instead of flooding the upstream at once.

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitTTL is how long a learned signal stays usable: 5
// minutes, the sector-typical RL_HEADER_TTL_MS.
const RateLimitTTL = 5 * time.Minute

// RateLimitSignals is one parsed set of upstream rate-limit headers.
// Either half may be absent (an upstream that only sends token
// headers, or only request headers).
type RateLimitSignals struct {
	RequestsLimit     float64
	RequestsRemaining float64
	RequestsResetAt   *time.Time
	HasRequests       bool

	TokensLimit     float64
	TokensRemaining float64
	TokensResetAt   *time.Time
	HasTokens       bool

	// RetryAfter is the Retry-After header when present (seconds
	// or an HTTP date, normalized to a duration from ObservedAt).
	RetryAfter *time.Duration

	ObservedAt time.Time
}

// Saturation is the most restrictive upstream saturation across the
// signals present: 1 - remaining/limit, clamped to 0..1. 0 means no
// signal (or a full budget).
func (s RateLimitSignals) Saturation() float64 {
	sat := 0.0
	if s.HasRequests && s.RequestsLimit > 0 {
		if v := 1 - s.RequestsRemaining/s.RequestsLimit; v > sat {
			sat = v
		}
	}
	if s.HasTokens && s.TokensLimit > 0 {
		if v := 1 - s.TokensRemaining/s.TokensLimit; v > sat {
			sat = v
		}
	}
	if sat < 0 {
		return 0
	}
	if sat > 1 {
		return 1
	}
	return sat
}

// RemainingFraction is the most restrictive remaining fraction
// (remaining / limit) across the signals present, 1 when no signal
// is present (nothing learned constrains the cap).
func (s RateLimitSignals) RemainingFraction() float64 {
	frac := 1.0
	any := false
	if s.HasRequests && s.RequestsLimit > 0 {
		if v := s.RequestsRemaining / s.RequestsLimit; v < frac {
			frac = v
		}
		any = true
	}
	if s.HasTokens && s.TokensLimit > 0 {
		if v := s.TokensRemaining / s.TokensLimit; v < frac {
			frac = v
		}
		any = true
	}
	if !any || frac < 0 {
		return 0
	}
	if frac > 1 {
		return 1
	}
	return frac
}

// Fresh reports whether the signals are still usable at now: within
// the 5-minute TTL, and no signal's window has reset yet (a reset
// window means the budget refilled — the old numbers are history).
func (s RateLimitSignals) Fresh(now time.Time) bool {
	if s.ObservedAt.IsZero() || now.Sub(s.ObservedAt) > RateLimitTTL {
		return false
	}
	if s.HasRequests && s.RequestsResetAt != nil && !now.Before(*s.RequestsResetAt) {
		return false
	}
	if s.HasTokens && s.TokensResetAt != nil && !now.Before(*s.TokensResetAt) {
		return false
	}
	return true
}

// Empty reports whether the signals carry nothing at all.
func (s RateLimitSignals) Empty() bool {
	return !s.HasRequests && !s.HasTokens && s.RetryAfter == nil
}

// ParseRateLimitHeaders parses an upstream response's rate-limit
// headers into signals. h is case-insensitive (http.Header.Get).
// A header family with an unparseable or non-positive limit, or a
// missing remaining, is ignored — never half-applied.
func ParseRateLimitHeaders(h http.Header, now time.Time) RateLimitSignals {
	sig := RateLimitSignals{ObservedAt: now}
	// Request headers: Anthropic first, then OpenAI's
	// -requests variants, then the generic pair.
	if limit, remaining, ok := parseLimitPair(
		h.Get("anthropic-ratelimit-requests-limit"),
		h.Get("anthropic-ratelimit-requests-remaining"),
		h.Get("x-ratelimit-limit-requests"),
		h.Get("x-ratelimit-remaining-requests"),
		h.Get("x-ratelimit-limit"),
		h.Get("x-ratelimit-remaining")); ok {
		sig.HasRequests = true
		sig.RequestsLimit = limit
		sig.RequestsRemaining = remaining
		if reset := parseReset(now, h.Get("x-ratelimit-reset-requests"), h.Get("x-ratelimit-reset")); reset != nil {
			sig.RequestsResetAt = reset
		}
	}
	// Token headers: first usable triple wins.
	triples := [][3]string{
		{"anthropic-ratelimit-tokens-limit", "anthropic-ratelimit-tokens-remaining", "anthropic-ratelimit-tokens-reset"},
		{"x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens", "x-ratelimit-reset-tokens"},
		{"anthropic-ratelimit-input-tokens-limit", "anthropic-ratelimit-input-tokens-remaining", "anthropic-ratelimit-input-tokens-reset"},
		{"anthropic-ratelimit-output-tokens-limit", "anthropic-ratelimit-output-tokens-remaining", "anthropic-ratelimit-output-tokens-reset"},
	}
	for _, tr := range triples {
		if limit, remaining, ok := parseLimitPair(h.Get(tr[0]), h.Get(tr[1]), "", "", "", ""); ok {
			sig.HasTokens = true
			sig.TokensLimit = limit
			sig.TokensRemaining = remaining
			if reset := parseReset(now, h.Get(tr[2])); reset != nil {
				sig.TokensResetAt = reset
			}
			break
		}
	}
	// Retry-After: integer seconds or an HTTP date.
	if ra := h.Get("Retry-After"); strings.TrimSpace(ra) != "" {
		if d, ok := parseRetryAfterDuration(ra, now); ok {
			sig.RetryAfter = &d
		}
	}
	return sig
}

// parseLimitPair parses a (limit, remaining) pair from up to three
// candidate header pairs, in order: the first pair whose limit
// parses to a finite positive number AND whose remaining parses to
// a finite number wins. Remaining clamps at 0 (a negative remaining
// is a "0 remaining" signal, not a negative budget).
func parseLimitPair(limit1, remaining1, limit2, remaining2, limit3, remaining3 string) (limit, remaining float64, ok bool) {
	for _, pair := range [][2]string{{limit1, remaining1}, {limit2, remaining2}, {limit3, remaining3}} {
		if strings.TrimSpace(pair[0]) == "" || strings.TrimSpace(pair[1]) == "" {
			continue
		}
		l, err1 := strconv.ParseFloat(strings.TrimSpace(pair[0]), 64)
		r, err2 := strconv.ParseFloat(strings.TrimSpace(pair[1]), 64)
		if err1 != nil || err2 != nil {
			continue
		}
		if l <= 0 || l != l || l > 1e15 || r != r || r > 1e15 {
			continue
		}
		if r < 0 {
			r = 0
		}
		return l, r, true
	}
	return 0, 0, false
}

// parseReset normalizes a reset header to an absolute time: an
// RFC3339 timestamp (Anthropic), a Go-style duration ("6m0s",
// "1.5s", "1h30m15s" — OpenAI), plain seconds, or an HTTP date.
// The first candidate that parses wins; nil when nothing parses.
func parseReset(now time.Time, candidates ...string) *time.Time {
	for _, raw := range candidates {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		// RFC3339 timestamp (Anthropic reset headers).
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return &t
		}
		// Go-style duration (OpenAI reset headers): bounded,
		// non-overlapping segments, same shape as the sector-typical
		// parseDurationMs.
		if d, ok := parseGoDuration(s); ok {
			t := now.Add(d)
			return &t
		}
		// Plain seconds from now.
		if secs, err := strconv.ParseFloat(s, 64); err == nil && secs >= 0 && secs < 1e9 {
			t := now.Add(time.Duration(secs * float64(time.Second)))
			return &t
		}
		// HTTP date.
		if t, err := http.ParseTime(s); err == nil {
			return &t
		}
	}
	return nil
}

// parseGoDuration parses a Go-style duration string ("6m0s",
// "1s", "1h30m15s", "1.5s", "500ms") into a duration. It rejects
// anything with unmatched trailing text, so "6m0s garbage" does
// not parse as 6 minutes.
func parseGoDuration(s string) (time.Duration, bool) {
	var total time.Duration
	rest := s
	matched := false
	for rest != "" {
		// Leading number (integer or decimal).
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 {
			return 0, false
		}
		num, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil || num < 0 {
			return 0, false
		}
		rest = rest[i:]
		// Unit.
		var unit time.Duration
		switch {
		case strings.HasPrefix(rest, "ms"):
			unit, rest = time.Millisecond, rest[2:]
		case strings.HasPrefix(rest, "h"):
			unit, rest = time.Hour, rest[1:]
		case strings.HasPrefix(rest, "m"):
			unit, rest = time.Minute, rest[1:]
		case strings.HasPrefix(rest, "s"):
			unit, rest = time.Second, rest[1:]
		default:
			return 0, false
		}
		total += time.Duration(num * float64(unit))
		matched = true
	}
	return total, matched
}

// parseRetryAfterDuration parses a Retry-After header value:
// integer seconds, or an HTTP date (relative to now).
func parseRetryAfterDuration(raw string, now time.Time) (time.Duration, bool) {
	s := strings.TrimSpace(raw)
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(s); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}
