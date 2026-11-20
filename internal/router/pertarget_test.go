// M4.7c — per-target timeout (default 120 s; bounds one
// non-streaming attempt's time-to-first-response).
package router

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// hangClient never answers until its ctx ends — a hung provider.
func hangClient() *fakeClient {
	return &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
}

// TestPerTargetTimeoutFailsOver: a hung provider is cut at the
// per-target timeout (100 ms here) and the chain fails over — the
// request does not hang, and the timeout counts as a breaker
// failure (a hung provider is a health signal, not a cancellation).
func TestPerTargetTimeoutFailsOver(t *testing.T) {
	ctx := context.Background()
	a := hangClient()
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1, PerTargetTimeout: 100 * time.Millisecond})
	start := time.Now()
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	elapsed := time.Since(start)
	if err != nil || resp.Content != "bee" {
		t.Fatalf("chat = %v, %v, want bee + nil (hung A cut at the timeout, failover to B)", resp, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("hung-provider request took %v, want < 2s (per-target timeout 100ms)", elapsed)
	}
	if got := a.chatCalls(); got != 1 {
		t.Fatalf("provider A called %d times, want 1", got)
	}
	// The failed attempt carries the timeout error in the trace
	// (as redacted text — the trace stores the error string).
	found := false
	for _, att := range trace.Attempts {
		if att.Provider == "a" && strings.Contains(att.Error, "per-target timeout") {
			found = true
		}
	}
	if !found {
		t.Fatalf("trace has no per-target-timeout error for A (attempts: %+v)", trace.Attempts)
	}
	if st := breakerStatesByID(eng)["a"]; st.ConsecutiveFailures != 1 {
		t.Fatalf("breaker consecutive failures = %d, want 1 (a hung provider counts, it is not a cancellation)", st.ConsecutiveFailures)
	}
}

// TestPerTargetTimeoutDefaults: 0 means the 120 s default, an
// explicit value is honoured, a negative value disables the
// timeout (the attempt ctx is the request ctx itself).
func TestPerTargetTimeoutDefaults(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": okClient("x")}, nil, Config{})
	if got := eng.perTargetTimeout(); got != DefaultPerTargetTimeout {
		t.Fatalf("perTargetTimeout() with 0 = %v, want %v (the 120s default)", got, DefaultPerTargetTimeout)
	}
	eng = NewEngine(map[string]provider.Client{"a": okClient("x")}, nil, Config{PerTargetTimeout: 5 * time.Second})
	if got := eng.perTargetTimeout(); got != 5*time.Second {
		t.Fatalf("perTargetTimeout() with 5s = %v, want 5s", got)
	}
	eng = NewEngine(map[string]provider.Client{"a": okClient("x")}, nil, Config{PerTargetTimeout: -1})
	if got := eng.perTargetTimeout(); got != 0 {
		t.Fatalf("perTargetTimeout() with -1 = %v, want 0 (disabled)", got)
	}
	ctx := context.Background()
	callCtx, cancel := eng.perTargetCtx(ctx)
	defer cancel()
	if callCtx != ctx {
		t.Fatalf("disabled per-target timeout returned a derived ctx, want the request ctx itself")
	}
}

// TestPerTargetTimeoutRequestDeadlineWins: when the request's own
// deadline fires first, the failure is the caller's deadline, not
// a per-target timeout — the engine never mislabels a cancelled
// request as a hung provider.
func TestPerTargetTimeoutRequestDeadlineWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	a := hangClient()
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{HedgeDelay: -1, PerTargetTimeout: 10 * time.Second})
	_, trace, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	if err == nil {
		t.Fatalf("want an error (request deadline), got nil")
	}
	for _, att := range trace.Attempts {
		if strings.Contains(att.Error, "per-target timeout") {
			t.Fatalf("attempt error names a per-target timeout, want the request deadline (it fired first)")
		}
	}
	if st := breakerStatesByID(eng)["a"]; st.ConsecutiveFailures != 0 {
		t.Fatalf("breaker consecutive failures = %d, want 0 (a caller-cancelled request is not a provider failure)", st.ConsecutiveFailures)
	}
}
