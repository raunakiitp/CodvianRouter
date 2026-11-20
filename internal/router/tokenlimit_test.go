package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// M5.3: per-key token limits, engine side — a breached target is
// skipped (trace "skipped_token_limit") and the next target serves;
// when every target is breached the first breach is the request
// error; a check error fails closed; a nil check is a no-op.

func tokenLimitReq() provider.ChatRequest {
	req := coolReq("m")
	req.CallerID = "k1"
	return req
}

func TestEngineTokenLimitSkipsBreachedTarget(t *testing.T) {
	a := errClient(&provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "must not be called"})
	b := okClient("bee")
	cfg := testConfig()
	cfg.TokenLimitCheck = func(ctx context.Context, keyID, providerID, model string) (*TokenLimitBreach, error) {
		if keyID != "k1" || model != "m" {
			t.Errorf("check called with keyID=%q model=%q, want k1/m", keyID, model)
		}
		if providerID == "a" {
			return &TokenLimitBreach{Scope: "provider", ScopeValue: "a", Limit: 100, Used: 100, NextReset: time.Now().Add(time.Hour)}, nil
		}
		return nil, nil
	}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	resp, trace, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), tokenLimitReq())
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
	if outcome(trace, 0) != "skipped_token_limit" {
		t.Errorf("trace[0] outcome = %q, want skipped_token_limit", outcome(trace, 0))
	}
}

func TestEngineTokenLimitAllBreachedReturnsBreach(t *testing.T) {
	a := errClient(&provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "must not be called"})
	b := errClient(&provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "must not be called"})
	cfg := testConfig()
	cfg.TokenLimitCheck = func(ctx context.Context, keyID, providerID, model string) (*TokenLimitBreach, error) {
		return &TokenLimitBreach{Scope: "global", Limit: 100, Used: 150, NextReset: time.Now().Add(time.Hour)}, nil
	}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	_, trace, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), tokenLimitReq())
	var breach *TokenLimitBreach
	if !errors.As(err, &breach) {
		t.Fatalf("err = %v, want a *TokenLimitBreach", err)
	}
	if breach.Limit != 100 || breach.Used != 150 {
		t.Errorf("breach = %+v, want global 100/150", breach)
	}
	if a.chatCalls() != 0 || b.chatCalls() != 0 {
		t.Errorf("no provider may be called when all are breached: a=%d b=%d", a.chatCalls(), b.chatCalls())
	}
	if outcome(trace, 0) != "skipped_token_limit" || outcome(trace, 1) != "skipped_token_limit" {
		t.Errorf("trace outcomes = %q/%q, want skipped_token_limit twice", outcome(trace, 0), outcome(trace, 1))
	}
}

func TestEngineTokenLimitCheckErrorFailsClosed(t *testing.T) {
	a := okClient("aye")
	cfg := testConfig()
	checkErr := errors.New("ledger down")
	cfg.TokenLimitCheck = func(ctx context.Context, keyID, providerID, model string) (*TokenLimitBreach, error) {
		return nil, checkErr
	}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	_, _, err := eng.ChatWithChain(context.Background(), chainFor("a"), tokenLimitReq())
	if !errors.Is(err, checkErr) {
		t.Fatalf("err = %v, want the check error (fail closed)", err)
	}
	if a.chatCalls() != 0 {
		t.Errorf("provider a was called %d times, want 0 (fail closed)", a.chatCalls())
	}
}

func TestEngineTokenLimitNilCheckAndAnonymousNoop(t *testing.T) {
	// No TokenLimitCheck configured: the request serves normally.
	a := okClient("aye")
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, testConfig())
	resp, _, err := eng.ChatWithChain(context.Background(), chainFor("a"), tokenLimitReq())
	if err != nil || resp.Content != "aye" {
		t.Fatalf("resp=%v err=%v, want aye/nil", resp, err)
	}
	// A check that fails the test if called, with an anonymous
	// request (no CallerID): the check must never run.
	eng2 := NewEngine(map[string]provider.Client{"a": a}, nil, func() Config {
		cfg := testConfig()
		cfg.TokenLimitCheck = func(ctx context.Context, keyID, providerID, model string) (*TokenLimitBreach, error) {
			t.Error("TokenLimitCheck must not run for an anonymous request")
			return nil, nil
		}
		return cfg
	}())
	resp, _, err = eng2.ChatWithChain(context.Background(), chainFor("a"), coolReq("m"))
	if err != nil || resp.Content != "aye" {
		t.Fatalf("anonymous resp=%v err=%v, want aye/nil", resp, err)
	}
}
