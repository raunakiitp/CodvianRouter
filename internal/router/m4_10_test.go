// M4.10 divergence proof: a 402 (payment required) fails over to
// the next target instead of stopping the chain. OmniRoute
// classifies 402 as quota_exhausted, retryable: false
// (failureClassification.ts:70); Codvian deliberately diverges —
// billing/quota is per-target, so provider A's empty balance says
// nothing about provider B. The 402 is breaker-neutral (a client
// error, not a provider failure) and never retried on the same
// target.
package router

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestPaymentRequired402FailsOver: chain [A 402, B ok] serves from
// B; A is called exactly once (never same-target retried) and its
// breaker stays closed.
func TestPaymentRequired402FailsOver(t *testing.T) {
	a := errClient(&provider.QuotaError{Message: "insufficient balance", StatusCode: 402})
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	resp, _, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), coolReq("m"))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "bee" {
		t.Fatalf("content = %q, want %q", resp.Content, "bee")
	}
	if got := a.chatCalls(); got != 1 {
		t.Errorf("provider a called %d times, want 1 (never same-target retried)", got)
	}
	if got := b.chatCalls(); got != 1 {
		t.Errorf("provider b called %d times, want 1", got)
	}
	if st := breakerStatesByID(eng)["a"]; st.ConsecutiveFailures != 0 || st.Open {
		t.Errorf("402 must not trip the breaker: %+v", st)
	}
}
