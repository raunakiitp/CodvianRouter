// M4.2 — breaker auth profiles, backoff escalation, provider-level
// failure-window breaker, DEGRADED / HALF_OPEN states.
package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestBreakerProfilesMatchReference: the profile table is the reference router's
// PROVIDER_PROFILES verbatim (open-sse/config/constants.ts).
func TestBreakerProfilesMatchReference(t *testing.T) {
	oauth := BreakerProfileFor(AuthProfileOAuth)
	if oauth.OpenThreshold != 8 || oauth.ResetTimeout != 60*time.Second || oauth.DegradeThreshold != 5 ||
		oauth.MaxBackoffMultiplier != 8 || oauth.BackoffEscalationCount != 2 ||
		oauth.ProviderFailureThreshold != 10 || oauth.ProviderFailureWindow != 15*time.Minute || oauth.ProviderCooldown != 5*time.Minute {
		t.Fatalf("oauth profile = %+v, want 8/60s/degrade 5/x8/after 2/window 10/15m/cool 5m", oauth)
	}
	api := BreakerProfileFor(AuthProfileAPIKey)
	if api.OpenThreshold != 12 || api.ResetTimeout != 30*time.Second || api.DegradeThreshold != 7 ||
		api.MaxBackoffMultiplier != 4 || api.BackoffEscalationCount != 3 ||
		api.ProviderFailureThreshold != 15 || api.ProviderFailureWindow != 30*time.Minute || api.ProviderCooldown != 10*time.Minute {
		t.Fatalf("api-key profile = %+v, want 12/30s/degrade 7/x4/after 3/window 15/30m/cool 10m", api)
	}
	local := BreakerProfileFor(AuthProfileLocal)
	if local.OpenThreshold != 2 || local.ResetTimeout != 15*time.Second ||
		local.ProviderFailureThreshold != 2 || local.ProviderFailureWindow != 5*time.Minute || local.ProviderCooldown != time.Minute {
		t.Fatalf("local profile = %+v, want 2/15s/window 2/5m/cool 1m", local)
	}
	// Unknown profile names resolve to API-key, never an error.
	if got := BreakerProfileFor("nope"); got.OpenThreshold != 12 {
		t.Fatalf("unknown profile threshold = %d, want 12 (api-key default)", got.OpenThreshold)
	}
}

// TestBreakerEscalationLadder: the reset doubles every
// BackoffEscalationCount trips, capped at MaxBackoffMultiplier.
func TestBreakerEscalationLadder(t *testing.T) {
	oauth := BreakerProfileFor(AuthProfileOAuth)
	want := map[int]time.Duration{
		1: 60 * time.Second, 2: 60 * time.Second,
		3: 120 * time.Second, 4: 120 * time.Second,
		5: 240 * time.Second, 6: 240 * time.Second,
		7: 480 * time.Second, 8: 480 * time.Second, 20: 480 * time.Second,
	}
	for trips, w := range want {
		if got := escalatedResetTimeout(oauth, trips); got != w {
			t.Fatalf("oauth trip %d: reset = %v, want %v", trips, got, w)
		}
	}
	api := BreakerProfileFor(AuthProfileAPIKey)
	for trips, w := range map[int]time.Duration{
		1: 30 * time.Second, 3: 30 * time.Second,
		4: 60 * time.Second, 6: 60 * time.Second,
		7: 120 * time.Second, 20: 120 * time.Second,
	} {
		if got := escalatedResetTimeout(api, trips); got != w {
			t.Fatalf("api-key trip %d: reset = %v, want %v", trips, got, w)
		}
	}
}

// profileEngine builds an engine whose providers resolve to the
// given profiles (by provider ID).
func profileEngine(profiles map[string]AuthProfile, clients map[string]provider.Client) *Engine {
	return NewEngine(clients, nil, Config{
		HedgeDelay: -1,
		AuthProfileFor: func(providerID string) AuthProfile {
			if p, ok := profiles[providerID]; ok {
				return p
			}
			return AuthProfileAPIKey
		},
	})
}

// TestBreakerPerProfileThresholds: OAuth trips at 8, API-key at 12,
// local at 2 — the same failure count opens one provider's breaker
// and leaves another's closed.
func TestBreakerPerProfileThresholds(t *testing.T) {
	ctx := context.Background()
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	for _, tc := range []struct {
		profile AuthProfile
		tripsAt int
	}{
		{AuthProfileOAuth, 8},
		{AuthProfileAPIKey, 12},
		{AuthProfileLocal, 2},
	} {
		t.Run(string(tc.profile), func(t *testing.T) {
			a := errClient(fail500)
			eng := profileEngine(map[string]AuthProfile{"a": tc.profile},
				map[string]provider.Client{"a": a, "b": okClient("bee")})
			chain := chainFor("a", "b")
			for i := 1; i <= tc.tripsAt; i++ {
				if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
					t.Fatalf("req %d: %v", i, err)
				}
				st := breakerStatesByID(eng)["a"]
				if i < tc.tripsAt && st.Open {
					t.Fatalf("req %d: breaker OPEN after %d failures, want closed (threshold is %d)", i, i, tc.tripsAt)
				}
				if i == tc.tripsAt && !st.Open {
					t.Fatalf("breaker = %+v, want OPEN after %d failures", st, tc.tripsAt)
				}
			}
			if got := a.chatCalls(); got != tc.tripsAt {
				t.Fatalf("provider called %d times, want %d (every request reached it until the trip)", got, tc.tripsAt)
			}
			// The next request skips the tripped provider.
			if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
				t.Fatalf("post-trip req: %v", err)
			}
			if got := a.chatCalls(); got != tc.tripsAt {
				t.Fatalf("provider called %d times after trip, want %d (skipped while open)", got, tc.tripsAt)
			}
		})
	}
}

// TestBreakerDegradedState: at the degrade threshold the breaker
// reports DEGRADED (still serving) and only OPEN at the open
// threshold. OAuth degrades at 5, opens at 8.
func TestBreakerDegradedState(t *testing.T) {
	ctx := context.Background()
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	a := errClient(fail500)
	eng := profileEngine(map[string]AuthProfile{"a": AuthProfileOAuth},
		map[string]provider.Client{"a": a, "b": okClient("bee")})
	chain := chainFor("a", "b")
	for i := 1; i <= 4; i++ {
		if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
	}
	if st := breakerStatesByID(eng)["a"]; st.State != BreakerClosed {
		t.Fatalf("after 4 failures state = %q, want CLOSED (degrade is 5)", st.State)
	}
	for i := 5; i <= 7; i++ {
		if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
	}
	if st := breakerStatesByID(eng)["a"]; st.State != BreakerDegraded || st.Open {
		t.Fatalf("after 7 failures state = %+v, want DEGRADED, not open", st)
	}
	// The degraded provider still serves the next request's
	// failover path (it is tried, then the sibling serves).
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
		t.Fatalf("req 8: %v", err)
	}
	if st := breakerStatesByID(eng)["a"]; st.State != BreakerOpen || !st.Open {
		t.Fatalf("after 8 failures state = %+v, want OPEN", st)
	}
}

// TestBreakerEscalationEngine: the open duration escalates per trip
// through the real breaker functions (local profile: 15 s, x2
// after 2 trips). The open window is fast-forwarded by moving
// openedAt into the past — the test drives breakerAllow (probe
// claim) + breakerFailure (probe failure), exactly the path a real
// half-open probe takes.
func TestBreakerEscalationEngine(t *testing.T) {
	eng := profileEngine(map[string]AuthProfile{"a": AuthProfileLocal},
		map[string]provider.Client{"a": errClient(&provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"})})
	trip := func() {
		eng.breakerFailure("a", "upstream_error")
		eng.breakerFailure("a", "upstream_error")
	}
	// Trip 1: open 15 s.
	trip()
	if st := breakerStatesByID(eng)["a"]; st.Trips != 1 || st.OpenFor != 15*time.Second {
		t.Fatalf("trip 1: %+v, want Trips 1 OpenFor 15s", st)
	}
	// Fast-forward past the window, claim the probe, fail it:
	// trip 2, still 15 s (escalation starts after 2 cycles).
	// Both breakers are fast-forwarded: the local window breaker
	// (2 failures / 1 min cooldown) tripped on the same failures
	// as the consecutive breaker, and it gates the probe too.
	fastForwardBreakers := func(eng *Engine) {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		eng.breakers["a"].openedAt = time.Now().Add(-time.Minute)
		if wb := eng.windowBreakers["a"]; wb != nil {
			wb.openUntil = time.Now().Add(-time.Minute)
		}
	}
	fastForwardBreakers(eng)
	if !eng.breakerAllow("a") {
		t.Fatalf("probe after window: breakerAllow = false, want true (half-open probe)")
	}
	eng.breakerFailure("a", "upstream_error")
	if st := breakerStatesByID(eng)["a"]; st.Trips != 2 || st.OpenFor != 15*time.Second {
		t.Fatalf("trip 2: %+v, want Trips 2 OpenFor 15s", st)
	}
	// Trip 3: the reset doubles to 30 s (the x2 cap for local).
	fastForwardBreakers(eng)
	if !eng.breakerAllow("a") {
		t.Fatalf("probe 2: breakerAllow = false, want true")
	}
	eng.breakerFailure("a", "upstream_error")
	if st := breakerStatesByID(eng)["a"]; st.Trips != 3 || st.OpenFor != 30*time.Second {
		t.Fatalf("trip 3: %+v, want Trips 3 OpenFor 30s (escalated)", st)
	}
	// A success resets the escalation: the next trip is back at
	// the base 15 s.
	eng.breakerSuccess("a")
	trip()
	if st := breakerStatesByID(eng)["a"]; st.Trips != 1 || st.OpenFor != 15*time.Second {
		t.Fatalf("after success, trip 1: %+v, want Trips 1 OpenFor 15s", st)
	}
}

// TestBreakerExplicitOverride: an operator-set BreakerThreshold /
// BreakerOpenFor overrides the profile table uniformly, with a
// FIXED open duration (no escalation).
func TestBreakerExplicitOverride(t *testing.T) {
	ctx := context.Background()
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	eng := NewEngine(map[string]provider.Client{"a": errClient(fail500), "b": okClient("bee")}, nil,
		Config{HedgeDelay: -1, BreakerThreshold: 3, BreakerOpenFor: 50 * time.Millisecond,
			AuthProfileFor: func(string) AuthProfile { return AuthProfileOAuth }})
	chain := chainFor("a", "b")
	for i := 0; i < 3; i++ {
		if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
			t.Fatalf("req %d: %v", i+1, err)
		}
	}
	st := breakerStatesByID(eng)["a"]
	if !st.Open || st.Trips != 1 || st.OpenFor != 50*time.Millisecond {
		t.Fatalf("override trip: %+v, want OPEN Trips 1 OpenFor 50ms (not the oauth 60s)", st)
	}
	// Second trip after the window: still exactly 50 ms — an
	// explicit override never escalates.
	time.Sleep(60 * time.Millisecond)
	if !eng.breakerAllow("a") {
		t.Fatalf("probe after override window: breakerAllow = false, want true")
	}
	eng.breakerFailure("a", "upstream_error")
	if st := breakerStatesByID(eng)["a"]; st.Trips != 2 || st.OpenFor != 50*time.Millisecond {
		t.Fatalf("override trip 2: %+v, want Trips 2 OpenFor still 50ms", st)
	}
}

// TestProviderWindowBreaker: interleaved successes reset the
// consecutive count but NOT the window — two failures inside the
// local 5-minute window (threshold 2) open the provider-level
// breaker for 1 minute, even with 0 consecutive failures.
// Toggle: without the window breaker the 4th request would call
// the provider again (it just succeeded once and failed once).
func TestProviderWindowBreaker(t *testing.T) {
	ctx := context.Background()
	var calls int
	flap := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls%2 == 1 {
			return nil, &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	eng := profileEngine(map[string]AuthProfile{"a": AuthProfileLocal},
		map[string]provider.Client{"a": flap})
	chain := chainFor("a")
	// Req 1: fail (window 1, consecutive 1).
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err == nil {
		t.Fatalf("req 1: want the 500, got nil")
	}
	// Req 2: succeed (consecutive resets to 0; window stays 1).
	if resp, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil || resp.Content != "ok" {
		t.Fatalf("req 2 = %v, %v, want ok + nil", resp, err)
	}
	// Req 3: fail (window 2 = threshold → window breaker opens).
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err == nil {
		t.Fatalf("req 3: want the 500, got nil")
	}
	st := breakerStatesByID(eng)["a"]
	if !st.WindowOpen || !st.Open || st.State != BreakerOpen {
		t.Fatalf("after flapping: %+v, want WindowOpen + Open + state OPEN", st)
	}
	if st.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive failures = %d, want 1 (the window, not the consecutive count, tripped)", st.ConsecutiveFailures)
	}
	if st.WindowFailures != 2 {
		t.Fatalf("window failures = %d, want 2", st.WindowFailures)
	}
	// Req 4: the provider is skipped entirely (window open),
	// despite having just succeeded once and failed once.
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); !isNoAvailableAttempt(err) {
		t.Fatalf("req 4 err = %v, want ErrNoAvailableAttempt (window breaker skips the provider)", err)
	}
	if calls != 3 {
		t.Fatalf("provider called %d times, want 3 (req 4 never dialled)", calls)
	}
}

func isNoAvailableAttempt(err error) bool {
	return errors.Is(err, ErrNoAvailableAttempt)
}

// TestAuthProfileForKind (M4.9): the catalog auth-kind string maps
// to the breaker profile — oauth is OAuth, none/local/no-auth are
// local runtimes, every stored-key kind (and unknown/empty) is the
// API-key profile.
func TestAuthProfileForKind(t *testing.T) {
	cases := []struct {
		kind string
		want AuthProfile
	}{
		{"oauth", AuthProfileOAuth},
		{"OAuth", AuthProfileOAuth},
		{"none", AuthProfileLocal},
		{"local", AuthProfileLocal},
		{"no-auth", AuthProfileLocal},
		{"bearer", AuthProfileAPIKey},
		{"api-key", AuthProfileAPIKey},
		{"x-api-key", AuthProfileAPIKey},
		{"x-goog-api-key", AuthProfileAPIKey},
		{"free", AuthProfileAPIKey},
		{"", AuthProfileAPIKey},
		{"cookie", AuthProfileAPIKey},
		{"something-new", AuthProfileAPIKey},
	}
	for _, tc := range cases {
		if got := AuthProfileForKind(tc.kind); got != tc.want {
			t.Errorf("AuthProfileForKind(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}
