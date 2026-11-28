// Breaker auth profiles (M4.2, the sector baseline the earlier implementation/config/constants.ts
// PROVIDER_PROFILES, verified 2026-11-18).
//
// the sector baseline trips its circuit breaker per auth profile, not
// uniformly: an OAuth session tolerates more failures before
// opening (8) but rests longer (60 s); an API-key provider opens
// at 12 and rests 30 s; a local backend opens at 2 and rests 15 s.
// The numbers below are the sector-typical verbatim, including the
// DEGRADED entry thresholds (5 / 7), the backoff-escalation
// ceilings (reset x8 OAuth / x4 API-key, escalating after 2 / 3
// open cycles), and the provider-level failure-window breaker
// (10 failures / 15 min / 5 min cooldown for OAuth; 15 / 30 min /
// 10 min for API-key; 2 / 5 min / 1 min for local).
//
// Local has no DEGRADED threshold or backoff ceiling in the sector baseline
// ("Not yet wired into getProviderProfile()"): ours-only defaults
// are degrade 1 (any failure degrades), escalation x2 after 2
// open cycles, documented as such below — never presented as
// the sector-typical numbers.
//
// The profile table governs when Config.BreakerThreshold /
// BreakerOpenFor are unset (0). An operator who sets them
// overrides the table for EVERY provider (the old uniform
// behaviour, kept for explicit configuration and for tests).
package router

import (
	"strings"
	"time"
)

// AuthProfile is how a provider authenticates: it selects the
// breaker profile (thresholds, reset, escalation, window).
type AuthProfile string

const (
	// AuthProfileOAuth is a session/OAuth provider (Vertex,
	// Codex-style sessions): most tolerant, longest rest.
	AuthProfileOAuth AuthProfile = "oauth"
	// AuthProfileAPIKey is an API-key provider: the default when
	// nothing else is known about a provider.
	AuthProfileAPIKey AuthProfile = "api-key"
	// AuthProfileLocal is a localhost inference backend (Ollama,
	// LM Studio, vLLM): trips fast, recovers fast.
	AuthProfileLocal AuthProfile = "local"
)

// BreakerProfile is one auth profile's breaker numbers (all
// the sector baseline PROVIDER_PROFILES values unless marked ours-only).
type BreakerProfile struct {
	// DegradeThreshold is the consecutive-failure count that
	// enters DEGRADED (still serving, flagged unhealthy).
	DegradeThreshold int
	// OpenThreshold is the consecutive-failure count that opens
	// the breaker.
	OpenThreshold int
	// ResetTimeout is the base open duration before a half-open
	// probe is allowed.
	ResetTimeout time.Duration
	// MaxBackoffMultiplier caps the escalation of ResetTimeout
	// (reset x up to this, doubling per EscalationCount trips).
	MaxBackoffMultiplier int
	// BackoffEscalationCount is how many open cycles it takes
	// to double the reset timeout (escalate after this many).
	BackoffEscalationCount int
	// ProviderFailureThreshold is the provider-level window
	// breaker: this many failures inside ProviderFailureWindow
	// open the provider-level breaker.
	ProviderFailureThreshold int
	// ProviderFailureWindow is the window the provider-level
	// breaker counts failures in.
	ProviderFailureWindow time.Duration
	// ProviderCooldown is how long the provider-level breaker
	// stays open once tripped.
	ProviderCooldown time.Duration
}

// DefaultBreakerProfiles is the the sector baseline PROVIDER_PROFILES table
// (local degrade/escalation are ours-only defaults, see above).
func DefaultBreakerProfiles() map[AuthProfile]BreakerProfile {
	return map[AuthProfile]BreakerProfile{
		AuthProfileOAuth: {
			DegradeThreshold:         5,
			OpenThreshold:            8,
			ResetTimeout:             60 * time.Second,
			MaxBackoffMultiplier:     8,
			BackoffEscalationCount:   2,
			ProviderFailureThreshold: 10,
			ProviderFailureWindow:    15 * time.Minute,
			ProviderCooldown:         5 * time.Minute,
		},
		AuthProfileAPIKey: {
			DegradeThreshold:         7,
			OpenThreshold:            12,
			ResetTimeout:             30 * time.Second,
			MaxBackoffMultiplier:     4,
			BackoffEscalationCount:   3,
			ProviderFailureThreshold: 15,
			ProviderFailureWindow:    30 * time.Minute,
			ProviderCooldown:         10 * time.Minute,
		},
		AuthProfileLocal: {
			// Ours-only: the sector baseline leaves local unwired.
			DegradeThreshold:         1,
			OpenThreshold:            2,
			ResetTimeout:             15 * time.Second,
			MaxBackoffMultiplier:     2,
			BackoffEscalationCount:   2,
			ProviderFailureThreshold: 2,
			ProviderFailureWindow:    5 * time.Minute,
			ProviderCooldown:         1 * time.Minute,
		},
	}
}

// BreakerProfileFor resolves a profile name to its table entry.
// Unknown names (including "") resolve to the API-key profile —
// the engine never errors on a profile name.
func BreakerProfileFor(p AuthProfile) BreakerProfile {
	if prof, ok := DefaultBreakerProfiles()[p]; ok {
		return prof
	}
	return DefaultBreakerProfiles()[AuthProfileAPIKey]
}

// escalatedResetTimeout is the open duration for the Nth open cycle
// (N >= 1) under profile p: the base reset, doubled every
// BackoffEscalationCount cycles, capped at MaxBackoffMultiplier x
// the base. Trip 1 always waits the base reset — escalation only
// ever lengthens later trips, never the first.
func escalatedResetTimeout(p BreakerProfile, trips int) time.Duration {
	if trips < 1 {
		trips = 1
	}
	count := p.BackoffEscalationCount
	if count < 1 {
		count = 1
	}
	doublings := (trips - 1) / count
	mult := 1
	for i := 0; i < doublings && mult < p.MaxBackoffMultiplier; i++ {
		mult *= 2
	}
	if mult > p.MaxBackoffMultiplier {
		mult = p.MaxBackoffMultiplier
	}
	d := p.ResetTimeout * time.Duration(mult)
	if d <= 0 {
		return p.ResetTimeout
	}
	return d
}

// Breaker state names, for BreakerStatus.State and the dashboard.
const (
	// BreakerClosed: below the degrade threshold.
	BreakerClosed = "CLOSED"
	// BreakerDegraded: at/above the degrade threshold, below the
	// open threshold — still serving, flagged unhealthy.
	BreakerDegraded = "DEGRADED"
	// BreakerOpen: tripped, inside its (possibly escalated) open window.
	BreakerOpen = "OPEN"
	// BreakerHalfOpen: the open window elapsed; a probe is allowed
	// (or already in flight).
	BreakerHalfOpen = "HALF_OPEN"
)

// AuthProfileForKind maps a catalog auth-kind string to the
// breaker profile (M4.9): "oauth" is an OAuth provider; "none",
// "local" and "no-auth" are local/no-credential runtimes; anything
// else — bearer, api-key, x-api-key, free, ... — is a stored key
// and takes the API-key profile, which is also the default for an
// unknown or empty kind.
func AuthProfileForKind(kind string) AuthProfile {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "oauth":
		return AuthProfileOAuth
	case "none", "local", "no-auth":
		return AuthProfileLocal
	default:
		return AuthProfileAPIKey
	}
}
