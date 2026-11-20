// Breaker must never trip on 4xx (CHECKLIST 151): a bad key (401),
// a bad request (400/422) or a forbidden (403) is a property of
// the request or the operator's key — never a provider-health
// signal. the reference router's RESILIENCE_GUIDE trips the breaker only on
// 408/5xx; LiteLLM's reliability docs agree. These tests use fake
// clients (no network) and the real Engine.
package router

import (
	"context"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// breakerStatesByID indexes BreakerStates by provider ID for the
// assertions below.
func breakerStatesByID(e *Engine) map[string]BreakerStatus {
	out := map[string]BreakerStatus{}
	for _, st := range e.BreakerStates() {
		out[st.ProviderID] = st
	}
	return out
}

// TestBreakerNeverTripsOn4xx: five consecutive 401s (and, in turn,
// 400s, 403s and 422s) leave the breaker CLOSED with zero counted
// failures, and the provider's live models still route afterwards —
// while five 500s trip it exactly as before, and a bare transport
// error trips it too.
func TestBreakerNeverTripsOn4xx(t *testing.T) {
	ctx := context.Background()
	chain := chainFor("a")
	for _, tc := range []struct {
		name       string
		err        error
		wantTrips  bool
		wantReason string // BreakerStates LastReason when it trips ("" when it must not)
	}{
		{"bad-key-401", &provider.UpstreamError{StatusCode: 401, Retryable: false, Body: `{"error":{"message":"Incorrect API key","type":"invalid_request_error"}}`}, false, ""},
		{"bad-request-400", &provider.UpstreamError{StatusCode: 400, Retryable: false, Body: `{"error":{"message":"temperature must be <= 1","type":"invalid_request_error"}}`}, false, ""},
		{"forbidden-403", &provider.UpstreamError{StatusCode: 403, Retryable: false, Body: `{"error":{"message":"nope","type":"forbidden"}}`}, false, ""},
		{"unprocessable-422", &provider.UpstreamError{StatusCode: 422, Retryable: false, Body: `{"error":{"message":"bad field","type":"invalid_request_error"}}`}, false, ""},
		{"server-500", &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}, true, provider.ReasonUpstreamError},
		{"timeout-408", &provider.UpstreamError{StatusCode: 408, Retryable: true, Body: "request timed out"}, true, provider.ReasonTimeout},
		{"transport-error", errors.New("dial tcp: connection refused"), true, provider.ReasonTransportError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failing := errClient(tc.err)
			eng := NewEngine(map[string]provider.Client{"a": failing}, nil, testConfig())
			// Five consecutive failures: exactly the breaker
			// threshold, so a breaker-counted error trips it
			// here and nowhere else.
			for i := 0; i < 5; i++ {
				if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err == nil {
					t.Fatalf("request %d: want the upstream error, got nil", i+1)
				}
				if failing.chatCalls() != i+1 {
					t.Fatalf("request %d: provider called %d times, want %d (every request must still reach the provider)", i+1, failing.chatCalls(), i+1)
				}
				st := breakerStatesByID(eng)["a"]
				// The breaker may only open on the 5th
				// breaker-counted failure (the threshold) —
				// never earlier, and never at all for a 4xx.
				if i < 4 && st.Open {
					t.Fatalf("request %d: breaker OPEN after %d failures, want closed (threshold is 5)", i+1, i+1)
				}
				if !tc.wantTrips && st.Open {
					t.Fatalf("request %d: breaker OPEN after %d failures, want closed (4xx must never trip it)", i+1, i+1)
				}
				if !tc.wantTrips && st.ConsecutiveFailures != 0 {
					t.Fatalf("request %d: consecutive failures = %d, want 0 (a 4xx must not count toward the breaker)", i+1, st.ConsecutiveFailures)
				}
			}
			st := breakerStatesByID(eng)["a"]
			if tc.wantTrips {
				if !st.Open {
					t.Fatalf("breaker = %+v, want OPEN after 5 breaker-counted failures", st)
				}
				if st.ConsecutiveFailures != 5 {
					t.Fatalf("consecutive failures = %d, want 5", st.ConsecutiveFailures)
				}
				if st.LastReason != tc.wantReason {
					t.Fatalf("last trip reason = %q, want %q", st.LastReason, tc.wantReason)
				}
				if st.RemainingSeconds <= 0 {
					t.Fatalf("remaining seconds = %d, want > 0 while open", st.RemainingSeconds)
				}
				if st.Ignored4xx != 0 {
					t.Fatalf("ignored 4xx = %d, want 0 (nothing here was a 4xx)", st.Ignored4xx)
				}
			} else {
				if st.Open || st.ConsecutiveFailures != 0 {
					t.Fatalf("breaker = %+v, want CLOSED with 0 counted failures after 5x %s", st, tc.name)
				}
				if st.LastReason != "" {
					t.Fatalf("last reason = %q, want empty (no breaker-counted failure happened)", st.LastReason)
				}
				if st.Ignored4xx != 5 {
					t.Fatalf("ignored 4xx = %d, want 5 (every 4xx is counted as breaker-ignored)", st.Ignored4xx)
				}
			}
		})
	}
}

// TestBreakerNeverTripsOn4xxStillRoutable: after five 401s the
// provider's live models still serve — the next request with a
// working key succeeds on the same provider, proving the breaker
// did not take the whole provider out.
func TestBreakerNeverTripsOn4xxStillRoutable(t *testing.T) {
	ctx := context.Background()
	chain := chainFor("a")
	badKey := &provider.UpstreamError{StatusCode: 401, Retryable: false, Body: `{"error":{"message":"Incorrect API key","type":"invalid_request_error"}}`}
	flaky := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, badKey
	}}
	eng := NewEngine(map[string]provider.Client{"a": flaky}, nil, testConfig())
	for i := 0; i < 5; i++ {
		if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err == nil {
			t.Fatalf("401 request %d: want the auth error, got nil", i+1)
		}
	}
	if st := breakerStatesByID(eng)["a"]; st.Open || st.ConsecutiveFailures != 0 {
		t.Fatalf("breaker = %+v, want CLOSED with 0 counted failures after 5x 401", st)
	}
	// The key is fixed (same provider, same model): the very next
	// request must serve — the breaker never took the provider
	// out, so no half-open probe or open window stands in the
	// way.
	flaky.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "live model answers"}, nil
	}
	resp, trace, err := eng.ChatWithChain(ctx, chain, coolReq("m"))
	if err != nil {
		t.Fatalf("request after the key fix: %v (the provider must still be routable)", err)
	}
	if resp.Content != "live model answers" {
		t.Fatalf("content = %q, want the live model's answer", resp.Content)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Outcome != "success" {
		t.Fatalf("trace = %+v, want exactly one successful attempt (no skipped_breaker)", trace.Attempts)
	}
	// The success clears the breaker state; the ignored-4xx
	// count is cumulative (it counts every 4xx that never
	// tripped the breaker, across the provider's lifetime) and
	// is deliberately not reset by a success.
	if st := breakerStatesByID(eng)["a"]; st.Open || st.ConsecutiveFailures != 0 {
		t.Fatalf("breaker after success = %+v, want CLOSED with 0 counted failures", st)
	}
}

// TestBreakerNeverTripsOn4xxSoftMode: the same rule holds in
// soft-demote mode — a 401 neither demotes nor trips, and a 500
// still demotes (never trips) there.
func TestBreakerNeverTripsOn4xxSoftMode(t *testing.T) {
	ctx := context.Background()
	chain := chainFor("a", "b")
	badKey := &provider.UpstreamError{StatusCode: 401, Retryable: false, Body: `{"error":{"message":"Incorrect API key"}}`}
	eng := softEngine(t, map[string]provider.Client{"a": errClient(badKey), "b": okClient("bee")})
	for i := 0; i < 5; i++ {
		if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err == nil {
			t.Fatalf("401 request %d: want the auth error, got nil", i+1)
		}
	}
	if st := breakerStatesByID(eng)["a"]; st.Open || st.ConsecutiveFailures != 0 || st.Ignored4xx != 5 {
		t.Fatalf("breaker (soft mode) = %+v, want CLOSED, 0 counted, 5 ignored", st)
	}
	if got := eng.CooldownTargets(); len(got) != 0 {
		t.Fatalf("soft mode cooling targets = %+v, want empty (a 401 cools nothing)", got)
	}
}

// TestBreakerNeverTripsOn4xxStreaming: five consecutive 401
// stream-start failures leave the breaker CLOSED, and the next
// stream on the same provider still serves.
func TestBreakerNeverTripsOn4xxStreaming(t *testing.T) {
	ctx := context.Background()
	chain := chainFor("a")
	badKey := &provider.UpstreamError{StatusCode: 401, Retryable: false, Body: `{"error":{"message":"Incorrect API key"}}`}
	flaky := streamClient(nil, badKey)
	eng := NewEngine(map[string]provider.Client{"a": flaky}, nil, testConfig())
	for i := 0; i < 5; i++ {
		ch, err := eng.ChatStreamWithChain(ctx, chain, coolReq("m"))
		if err != nil {
			t.Fatalf("stream %d start: %v", i+1, err)
		}
		var streamErr error
		for ev := range ch {
			if ev.Err != nil {
				streamErr = ev.Err
			}
		}
		if streamErr == nil {
			t.Fatalf("stream %d: want the 401 error event, got none", i+1)
		}
	}
	if st := breakerStatesByID(eng)["a"]; st.Open || st.ConsecutiveFailures != 0 || st.Ignored4xx != 5 {
		t.Fatalf("breaker (streaming) = %+v, want CLOSED, 0 counted, 5 ignored", st)
	}
	// The key is fixed: the next stream serves on the same
	// provider.
	flaky.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if onDelta != nil {
			if err := onDelta("live"); err != nil {
				return nil, err
			}
		}
		return &provider.ChatResponse{Content: "live"}, nil
	}
	ch, err := eng.ChatStreamWithChain(ctx, chain, coolReq("m"))
	if err != nil {
		t.Fatalf("stream after the key fix: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream after the key fix: %v", ev.Err)
		}
		got += ev.Delta
	}
	if got != "live" {
		t.Fatalf("stream content = %q, want %q", got, "live")
	}
}

// TestIsClientErrorFailure: the taxonomy verdict behind the rule —
// 4xx statuses and client-error reasons are neutral; 408/5xx and
// transport errors are not. A 429 is neutral too, but it never
// reaches the breaker path (the rate-limit branch runs first).
func TestIsClientErrorFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"400", &provider.UpstreamError{StatusCode: 400, Body: "bad request"}, true},
		{"401", &provider.UpstreamError{StatusCode: 401, Body: "bad key"}, true},
		{"402", &provider.QuotaError{Message: "pay up", StatusCode: 402}, true},
		{"403", &provider.UpstreamError{StatusCode: 403, Body: "forbidden"}, true},
		{"404-model-not-found", &provider.UpstreamError{StatusCode: 404, Body: "model not found"}, true},
		{"422", &provider.UpstreamError{StatusCode: 422, Body: "bad field"}, true},
		{"408", &provider.UpstreamError{StatusCode: 408, Retryable: true, Body: "timeout"}, false},
		{"500", &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}, false},
		{"529", &provider.UpstreamError{StatusCode: 529, Retryable: true, Body: "overloaded"}, false},
		{"transport", errors.New("dial tcp: connection refused"), false},
		{"unrepresentable", &provider.UpstreamError{StatusCode: 0, Body: "x"}, false}, // bare status-0 with no reason is not a client error
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClientErrorFailure(tc.err); got != tc.want {
				t.Fatalf("isClientErrorFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	// A wire-level unrepresentable request (no HTTP status at
	// all) is a client problem too — it classifies by reason.
	if got := isClientErrorFailure(&wire.UnrepresentableError{Reason: "temperature > 1"}); !got {
		t.Fatalf("isClientErrorFailure(unrepresentable) = false, want true")
	}
}
