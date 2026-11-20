package router

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Multi-key pooling tests (CHECKLIST 175-176): one provider holds
// several API keys; the router tries the preferred key first, fails
// over to the next key on 401/429/quota, cools a dead key per-key
// (never the whole provider), and holds reserve keys back until
// every preferred key is exhausted. Per-key spend is attributed to
// the key that served (see fakeKeyLedger below).

var errBadKey401 = &provider.UpstreamError{StatusCode: 401, Retryable: false, Body: `{"error":{"message":"Incorrect API key"}}`}
var errRate429 = &provider.RateLimitError{Body: "slow down"}
var errQuota402 = &provider.QuotaError{Message: "pay up", StatusCode: 402}

// keyPoolChain builds a chain for provider "a" with two preferred
// keys (k1, k2) and one reserve key (k3), in pool order.
func keyPoolChain() []Attempt {
	return []Attempt{
		{ProviderID: "a", Model: "m", KeyID: "key_1"},
		{ProviderID: "a", Model: "m", KeyID: "key_2"},
		{ProviderID: "a", Model: "m", KeyID: "key_3", Reserve: true},
	}
}

// keyPoolClients registers one fake client per (provider, key)
// identity, keyed by ClientKey.
func keyPoolClients(t *testing.T, clients map[string]*fakeClient) map[string]provider.Client {
	t.Helper()
	out := make(map[string]provider.Client, len(clients))
	for keyID, fc := range clients {
		out[ClientKey("a", keyID)] = fc
	}
	return out
}

func TestKeyPoolPreferred401FailsOverToSecondKey(t *testing.T) {
	k1 := errClient(errBadKey401)
	k2 := okClient("from-k2")
	k3 := okClient("from-k3-reserve")
	eng := NewEngine(keyPoolClients(t, map[string]*fakeClient{"key_1": k1, "key_2": k2, "key_3": k3}), nil, testConfig())
	resp, trace, err := eng.ChatWithChain(context.Background(), keyPoolChain(), testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "from-k2" {
		t.Fatalf("content = %q, want %q", resp.Content, "from-k2")
	}
	if k1.chatCalls() != 1 || k2.chatCalls() != 1 {
		t.Fatalf("calls = k1 %d, k2 %d, want 1, 1", k1.chatCalls(), k2.chatCalls())
	}
	if k3.chatCalls() != 0 {
		t.Fatalf("reserve key called %d times, want 0 (a preferred key served)", k3.chatCalls())
	}
	if len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %d, want 2", len(trace.Attempts))
	}
	if trace.Attempts[0].KeyID != "key_1" || trace.Attempts[1].KeyID != "key_2" {
		t.Fatalf("trace key_ids = %q, %q, want key_1, key_2", trace.Attempts[0].KeyID, trace.Attempts[1].KeyID)
	}
	if trace.Attempts[0].Reason != "auth_failed" {
		t.Fatalf("first attempt reason = %q, want auth_failed", trace.Attempts[0].Reason)
	}
}

func TestKeyPool429FailsOverToSecondKey(t *testing.T) {
	k1 := errClient(errRate429)
	k2 := okClient("from-k2")
	eng := NewEngine(keyPoolClients(t, map[string]*fakeClient{"key_1": k1, "key_2": k2}), nil, testConfig())
	resp, _, err := eng.ChatWithChain(context.Background(), keyPoolChain()[:2], testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "from-k2" {
		t.Fatalf("content = %q, want %q", resp.Content, "from-k2")
	}
}

func TestKeyPoolQuota402FailsOverToSecondKey(t *testing.T) {
	k1 := errClient(errQuota402)
	k2 := okClient("from-k2")
	eng := NewEngine(keyPoolClients(t, map[string]*fakeClient{"key_1": k1, "key_2": k2}), nil, testConfig())
	resp, _, err := eng.ChatWithChain(context.Background(), keyPoolChain()[:2], testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "from-k2" {
		t.Fatalf("content = %q, want %q", resp.Content, "from-k2")
	}
}

func TestKeyPoolDeadKeyCooledPerKeyOnly(t *testing.T) {
	ctx := context.Background()
	k1 := errClient(errBadKey401)
	k2 := okClient("from-k2")
	eng := NewEngine(keyPoolClients(t, map[string]*fakeClient{"key_1": k1, "key_2": k2}), nil, testConfig())
	// First request: k1 401s, k2 serves. The dead key cools.
	if _, _, err := eng.ChatWithChain(ctx, keyPoolChain()[:2], testReq()); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	targets := eng.CooldownTargets()
	if len(targets) != 1 || targets[0].ProviderID != "a" || targets[0].KeyID != "key_1" {
		t.Fatalf("cooldown targets = %+v, want exactly one (a, key_1)", targets)
	}
	// Second request: k1 is skipped (cooled), k2 serves directly —
	// the provider is NOT skipped as a whole.
	resp, trace, err := eng.ChatWithChain(ctx, keyPoolChain()[:2], testReq())
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if resp.Content != "from-k2" {
		t.Fatalf("content = %q, want %q", resp.Content, "from-k2")
	}
	if k1.chatCalls() != 1 {
		t.Fatalf("k1 called %d times, want 1 (cooled on the second request)", k1.chatCalls())
	}
	if k2.chatCalls() != 2 {
		t.Fatalf("k2 called %d times, want 2", k2.chatCalls())
	}
	found := false
	for _, at := range trace.Attempts {
		if at.Outcome == "skipped_cooldown" && at.KeyID == "key_1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("second trace has no skipped_cooldown entry for key_1: %+v", trace.Attempts)
	}
}

func TestKeyPoolReserveOnlyWhenPreferredExhausted(t *testing.T) {
	ctx := context.Background()
	// Case 1: a preferred key serves — the reserve is never tried.
	k1 := okClient("from-k1")
	k3 := okClient("from-k3-reserve")
	eng := NewEngine(keyPoolClients(t, map[string]*fakeClient{"key_1": k1, "key_3": k3}), nil, testConfig())
	if _, _, err := eng.ChatWithChain(ctx, keyPoolChain(), testReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if k3.chatCalls() != 0 {
		t.Fatalf("reserve called %d times while a preferred key served, want 0", k3.chatCalls())
	}
	// Case 2: every preferred key fails — the reserve serves.
	k1f := errClient(errBadKey401)
	k2f := errClient(errRate429)
	k3ok := okClient("from-k3-reserve")
	eng2 := NewEngine(keyPoolClients(t, map[string]*fakeClient{"key_1": k1f, "key_2": k2f, "key_3": k3ok}), nil, testConfig())
	resp, trace, err := eng2.ChatWithChain(ctx, keyPoolChain(), testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "from-k3-reserve" {
		t.Fatalf("content = %q, want %q", resp.Content, "from-k3-reserve")
	}
	if len(trace.Attempts) != 3 || trace.Attempts[2].KeyID != "key_3" {
		t.Fatalf("trace = %+v, want the reserve attempt last", trace.Attempts)
	}
	// Case 3: the reserve sorts last even when listed first in
	// the chain — the tail partition holds it back.
	eng3 := NewEngine(keyPoolClients(t, map[string]*fakeClient{"key_1": okClient("from-k1"), "key_3": okClient("from-k3")}), nil, testConfig())
	reserveFirst := []Attempt{
		{ProviderID: "a", Model: "m", KeyID: "key_3", Reserve: true},
		{ProviderID: "a", Model: "m", KeyID: "key_1"},
	}
	if _, trace, err := eng3.ChatWithChain(ctx, reserveFirst, testReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	} else if len(trace.Attempts) != 1 || trace.Attempts[0].KeyID != "key_1" {
		t.Fatalf("trace = %+v, want only the preferred key tried", trace.Attempts)
	}
}

// fakeKeyLedger records (gateway key, provider, provider key) per
// attempt, so tests can prove spend is attributed to the key that
// served — never pooled into one number for the provider.
type fakeKeyLedger struct {
	mu    sync.Mutex
	calls []keyLedgerCall
}

type keyLedgerCall struct {
	gatewayKeyID  string
	provider      string
	providerKeyID string
	model         string
	ok            bool
}

func (l *fakeKeyLedger) RecordAttemptWithProviderKey(gatewayKeyID, providerName, providerKeyID, model string, ok bool, latencyMs int64, usage provider.Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, keyLedgerCall{gatewayKeyID: gatewayKeyID, provider: providerName, providerKeyID: providerKeyID, model: model, ok: ok})
}

func (l *fakeKeyLedger) RecordAttemptForKey(keyID, providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	l.RecordAttemptWithProviderKey(keyID, providerName, "", model, ok, latencyMs, usage)
}

func (l *fakeKeyLedger) RecordAttempt(providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	l.RecordAttemptWithProviderKey("", providerName, "", model, ok, latencyMs, usage)
}

func TestKeyPoolLedgerAttributionPerKey(t *testing.T) {
	led := &fakeKeyLedger{}
	k1 := errClient(errBadKey401)
	k2 := okClient("from-k2")
	eng := NewEngine(keyPoolClients(t, map[string]*fakeClient{"key_1": k1, "key_2": k2}), led, testConfig())
	req := testReq()
	req.CallerID = "gw-key-7"
	if _, _, err := eng.ChatWithChain(context.Background(), keyPoolChain()[:2], req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	led.mu.Lock()
	defer led.mu.Unlock()
	if len(led.calls) != 2 {
		t.Fatalf("ledger calls = %d, want 2 (one per key)", len(led.calls))
	}
	if led.calls[0].providerKeyID != "key_1" || led.calls[0].ok {
		t.Fatalf("first call = %+v, want key_1 failed", led.calls[0])
	}
	if led.calls[1].providerKeyID != "key_2" || !led.calls[1].ok {
		t.Fatalf("second call = %+v, want key_2 succeeded", led.calls[1])
	}
	if led.calls[0].gatewayKeyID != "gw-key-7" || led.calls[1].gatewayKeyID != "gw-key-7" {
		t.Fatalf("gateway key not carried: %+v", led.calls)
	}
}

func TestKeyPoolBreakerStaysClosedOn401(t *testing.T) {
	ctx := context.Background()
	k1 := errClient(errBadKey401)
	k2 := okClient("from-k2")
	eng := NewEngine(keyPoolClients(t, map[string]*fakeClient{"key_1": k1, "key_2": k2}), nil, testConfig())
	for i := 0; i < 5; i++ {
		if _, _, err := eng.ChatWithChain(ctx, keyPoolChain()[:2], testReq()); err != nil {
			t.Fatalf("Chat %d: %v", i+1, err)
		}
	}
	// The dead key cooled (per-key), the provider's breaker never
	// tripped: five bad-key failures are not a provider outage.
	for _, st := range eng.BreakerStates() {
		if st.Open || st.ConsecutiveFailures != 0 {
			t.Fatalf("breaker = %+v, want CLOSED with 0 counted failures", st)
		}
	}
}

func TestKeyPoolSingleKey401StillStopsChain(t *testing.T) {
	// Legacy single-key provider: a 401 is terminal — the chain
	// stops and the fallback provider is never dialled. (This is
	// the behaviour the pool exception deliberately preserves.)
	a := errClient(errBadKey401)
	b := okClient("from-b")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	_, trace, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err == nil {
		t.Fatal("Chat: want the auth error, got nil")
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Reason != "auth_failed" {
		t.Fatalf("trace = %+v, want one auth_failed attempt", trace.Attempts)
	}
	if b.chatCalls() != 0 {
		t.Fatalf("fallback provider called %d times after a single-key 401, want 0", b.chatCalls())
	}
	if got := eng.CooldownTargets(); len(got) != 0 {
		t.Fatalf("cooldown targets = %+v, want empty (a legacy 401 cools nothing)", got)
	}
}

func TestKeyPoolStreamingPreCommit401FailsOver(t *testing.T) {
	ctx := context.Background()
	k1 := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		return nil, errBadKey401
	}}
	k2 := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if onDelta != nil {
			if err := onDelta("hello"); err != nil {
				return nil, err
			}
		}
		return &provider.ChatResponse{Content: "hello"}, nil
	}}
	eng := NewEngine(keyPoolClients(t, map[string]*fakeClient{"key_1": k1, "key_2": k2}), nil, testConfig())
	ch, err := eng.ChatStreamWithChain(ctx, keyPoolChain()[:2], testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	var got strings.Builder
	var streamErr error
	timeout := time.After(10 * time.Second)
drain:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				break drain
			}
			if ev.Err != nil {
				streamErr = ev.Err
			}
			got.WriteString(ev.Delta)
		case <-timeout:
			t.Fatal("stream did not finish within 10s")
		}
	}
	if streamErr != nil {
		t.Fatalf("stream error: %v", streamErr)
	}
	if got.String() != "hello" {
		t.Fatalf("stream content = %q, want %q", got.String(), "hello")
	}
	if k1.streamCalls() != 1 || k2.streamCalls() != 1 {
		t.Fatalf("stream calls = k1 %d, k2 %d, want 1, 1", k1.streamCalls(), k2.streamCalls())
	}
}

// TestKeyPoolTraceCarriesKeyID proves the route trace names the key
// that served each attempt (dashboard Live Routing shows it).
func TestKeyPoolTraceCarriesKeyID(t *testing.T) {
	eng := NewEngine(keyPoolClients(t, map[string]*fakeClient{"key_1": errClient(errBadKey401), "key_2": okClient("from-k2")}), nil, testConfig())
	_, trace, err := eng.ChatWithChain(context.Background(), keyPoolChain()[:2], testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace = %+v, want 2 attempts", trace)
	}
	for i, want := range []string{"key_1", "key_2"} {
		if trace.Attempts[i].KeyID != want {
			t.Fatalf("trace attempt %d key_id = %q, want %q", i, trace.Attempts[i].KeyID, want)
		}
	}
}
