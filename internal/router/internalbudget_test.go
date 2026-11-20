package router

import (
	"context"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// M5.11: internal scoped budgets, engine side — a target whose
// provider is over its internal budget is skipped (trace
// "skipped_internal_budget") and the next target serves; when
// every target is breached the first breach is the request error;
// a check error fails closed; a nil check is a no-op. Unlike the
// token-limit check, the budget check applies to anonymous
// requests too (the cap is on the operator's total spend).

func TestEngineInternalBudgetSkipsBreachedProvider(t *testing.T) {
	a := errClient(&provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "must not be called"})
	b := okClient("bee")
	cfg := testConfig()
	cfg.InternalBudgetCheck = func(ctx context.Context, providerID string) (*InternalBudgetBreach, error) {
		if providerID == "a" {
			return &InternalBudgetBreach{BudgetID: "p1", Provider: "a", LimitType: "currency", Limit: 3, Used: 4, Remaining: 0, Period: "daily"}, nil
		}
		return nil, nil
	}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	// Anonymous request (no CallerID): the budget still applies.
	resp, trace, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), coolReq("m"))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "bee" {
		t.Errorf("content = %q, want bee (served from b)", resp.Content)
	}
	if a.chatCalls() != 0 {
		t.Errorf("breached provider a was called %d times, want 0", a.chatCalls())
	}
	if b.chatCalls() != 1 {
		t.Errorf("provider b was called %d times, want 1", b.chatCalls())
	}
	if outcome(trace, 0) != "skipped_internal_budget" {
		t.Errorf("trace[0] outcome = %q, want skipped_internal_budget", outcome(trace, 0))
	}
}

func TestEngineInternalBudgetAllBreachedReturnsBreach(t *testing.T) {
	a := errClient(&provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "must not be called"})
	b := errClient(&provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "must not be called"})
	cfg := testConfig()
	cfg.InternalBudgetCheck = func(ctx context.Context, providerID string) (*InternalBudgetBreach, error) {
		return &InternalBudgetBreach{BudgetID: "p1", Provider: providerID, LimitType: "currency", Limit: 3, Used: 4, Remaining: 0, Period: "daily"}, nil
	}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	_, trace, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), coolReq("m"))
	var breach *InternalBudgetBreach
	if !errors.As(err, &breach) {
		t.Fatalf("err = %v (%T), want *InternalBudgetBreach", err, err)
	}
	if breach.BudgetID != "p1" || breach.Provider != "a" {
		t.Errorf("breach = %+v, want p1/a (the first breach)", breach)
	}
	if outcome(trace, 0) != "skipped_internal_budget" || outcome(trace, 1) != "skipped_internal_budget" {
		t.Errorf("trace outcomes = %q/%q, want skipped_internal_budget twice", outcome(trace, 0), outcome(trace, 1))
	}
}

func TestEngineInternalBudgetCheckErrorFailsClosed(t *testing.T) {
	a := okClient("aye")
	cfg := testConfig()
	want := errors.New("ledger unavailable")
	cfg.InternalBudgetCheck = func(ctx context.Context, providerID string) (*InternalBudgetBreach, error) {
		return nil, want
	}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	_, _, err := eng.ChatWithChain(context.Background(), chainFor("a"), coolReq("m"))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want the ledger error (fail closed)", err)
	}
	if a.chatCalls() != 0 {
		t.Errorf("provider a was called %d times, want 0", a.chatCalls())
	}
}

func TestEngineInternalBudgetNilCheckNoop(t *testing.T) {
	a := okClient("aye")
	cfg := testConfig()
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	resp, _, err := eng.ChatWithChain(context.Background(), chainFor("a"), coolReq("m"))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "aye" {
		t.Errorf("content = %q, want aye", resp.Content)
	}
}
