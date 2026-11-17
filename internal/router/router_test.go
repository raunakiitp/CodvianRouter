package router

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/server"
)

// The engine satisfies the server's Engine + StreamResumer contracts.
var (
	_ server.Engine        = (*Engine)(nil)
	_ server.StreamResumer = (*Engine)(nil)
)

// ---------------------------------------------------------------------------
// Fakes.
// ---------------------------------------------------------------------------

type fakeLedger struct {
	mu    sync.Mutex
	calls []ledgerCall
}

type ledgerCall struct {
	provider  string
	model     string
	ok        bool
	latencyMs int64
	tokens    int
}

func (l *fakeLedger) RecordAttempt(provider, model string, ok bool, latencyMs int64, tokens int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, ledgerCall{provider: provider, model: model, ok: ok, latencyMs: latencyMs, tokens: tokens})
}

func (l *fakeLedger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

type fakeClient struct {
	mu         sync.Mutex
	chats      int
	streams    int
	chatFn     func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error)
	streamFn   func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error)
	reqs       []provider.ChatRequest
	streamReqs []provider.ChatRequest
}

func (f *fakeClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	f.mu.Lock()
	f.chats++
	f.reqs = append(f.reqs, req)
	fn := f.chatFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (f *fakeClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	f.mu.Lock()
	f.streams++
	f.streamReqs = append(f.streamReqs, req)
	fn := f.streamFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, req, onDelta)
	}
	if onDelta != nil {
		if err := onDelta("ok"); err != nil {
			return nil, err
		}
	}
	return &provider.ChatResponse{Content: "ok"}, nil
}

// ListModels and TestKey complete the provider.Client interface; the
// router never calls them.
func (f *fakeClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (f *fakeClient) TestKey(ctx context.Context) error                { return nil }

func (f *fakeClient) chatCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chats
}

func (f *fakeClient) streamCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams
}

func (f *fakeClient) lastStreamReq() (provider.ChatRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.streamReqs) == 0 {
		return provider.ChatRequest{}, false
	}
	return f.streamReqs[len(f.streamReqs)-1], true
}

func okClient(content string) *fakeClient {
	return &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: content}, nil
	}}
}

func errClient(err error) *fakeClient {
	return &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, err
	}}
}

// delayClient waits d (or until ctx ends) before succeeding.
func delayClient(d time.Duration, content string) *fakeClient {
	return &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		select {
		case <-time.After(d):
			return &provider.ChatResponse{Content: content}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
}

// streamClient emits deltas via onDelta then either failErr or a
// completed response.
func streamClient(deltas []string, failErr error) *fakeClient {
	return &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		var sb strings.Builder
		for _, d := range deltas {
			sb.WriteString(d)
			if err := onDelta(d); err != nil {
				return nil, err
			}
		}
		if failErr != nil {
			return nil, failErr
		}
		return &provider.ChatResponse{Content: sb.String()}, nil
	}}
}

// collectStream drains a ChatStream channel, returning the joined
// deltas and the terminal error (nil on Done).
func collectStream(t *testing.T, ch <-chan StreamEvent) (string, error) {
	t.Helper()
	var sb strings.Builder
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return sb.String(), errors.New("stream closed without Done or error")
			}
			if ev.Err != nil {
				return sb.String(), ev.Err
			}
			sb.WriteString(ev.Delta)
			if ev.Done {
				return sb.String(), nil
			}
		case <-timeout:
			t.Fatal("timed out waiting for stream to finish")
			return sb.String(), errors.New("timeout")
		}
	}
}

func testReq() provider.ChatRequest {
	return provider.ChatRequest{Messages: []provider.Message{{Role: RoleUser, Content: "hi"}}}
}

// testConfig returns a Config with short timings so tests run fast.
func testConfig() Config {
	return Config{
		MaxAttempts:      5,
		HedgeDelay:       -1, // hedging off unless a test enables it
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
	}
}

func outcome(trace *RouteTrace, i int) string {
	if trace == nil || i >= len(trace.Attempts) {
		return "<missing>"
	}
	return trace.Attempts[i].Outcome
}

// ---------------------------------------------------------------------------
// Fallback, quota, trace, ledger.
// ---------------------------------------------------------------------------

func TestChatFallbackFirstFailsSecondSucceeds(t *testing.T) {
	a := errClient(errors.New("boom"))
	b := okClient("from-b")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, trace, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m1"}, {ProviderID: "b", Model: "m2"}}, testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "from-b" {
		t.Fatalf("content = %q, want %q", resp.Content, "from-b")
	}
	// provider.ChatResponse carries no serving-provider field; the
	// trace records which attempt served the request.
	if trace.Attempts[1].Provider != "b" {
		t.Fatalf("provider = %q, want %q", trace.Attempts[1].Provider, "b")
	}
	if len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %d, want 2", len(trace.Attempts))
	}
	if outcome(trace, 0) != "error" || outcome(trace, 1) != "success" {
		t.Fatalf("outcomes = %q, %q", outcome(trace, 0), outcome(trace, 1))
	}
	if trace.Attempts[0].Provider != "a" || trace.Attempts[1].Model != "m2" {
		t.Fatalf("trace = %+v", trace.Attempts)
	}
}

func TestChatQuotaPrefersHighestAndSkipsZero(t *testing.T) {
	low := okClient("low")
	high := okClient("high")
	zero := okClient("zero")
	cfg := testConfig()
	cfg.QuotaHint = func(id string) int {
		switch id {
		case "high":
			return 90
		case "low":
			return 10
		default:
			return 0
		}
	}
	eng := NewEngine(map[string]provider.Client{"low": low, "high": high, "zero": zero}, nil, cfg)
	resp, trace, err := eng.ChatWithChain(context.Background(),
		[]Attempt{{ProviderID: "low", Model: "m"}, {ProviderID: "zero", Model: "m"}, {ProviderID: "high", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "high" {
		t.Fatalf("content = %q, want high first", resp.Content)
	}
	if low.chatCalls() != 0 || zero.chatCalls() != 0 {
		t.Fatalf("low calls = %d, zero calls = %d, want 0/0", low.chatCalls(), zero.chatCalls())
	}
	if len(trace.Attempts) != 2 { // 1 skipped_zero_quota + 1 success
		t.Fatalf("trace attempts = %d, want 2", len(trace.Attempts))
	}
	if outcome(trace, 0) != "skipped_zero_quota" || outcome(trace, 1) != "success" {
		t.Fatalf("outcomes = %q, %q", outcome(trace, 0), outcome(trace, 1))
	}
}

func TestChatAllZeroQuotaReturnsError(t *testing.T) {
	zero := okClient("zero")
	cfg := testConfig()
	cfg.QuotaHint = func(string) int { return 0 }
	eng := NewEngine(map[string]provider.Client{"zero": zero}, nil, cfg)
	_, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "zero", Model: "m"}}, testReq())
	if !errors.Is(err, ErrNoAvailableAttempt) {
		t.Fatalf("err = %v, want ErrNoAvailableAttempt", err)
	}
	if zero.chatCalls() != 0 {
		t.Fatalf("zero-quota provider was called %d times", zero.chatCalls())
	}
}

func TestChatLedgerAndTokens(t *testing.T) {
	a := errClient(errors.New("down"))
	b := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok", Usage: provider.Usage{TotalTokens: 42}}, nil
	}}
	led := &fakeLedger{}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, led, testConfig())
	_, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m1"}, {ProviderID: "b", Model: "m2"}}, testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if led.count() != 2 {
		t.Fatalf("ledger calls = %d, want 2", led.count())
	}
	if led.calls[0].ok || !led.calls[1].ok {
		t.Fatalf("ledger ok flags = %+v", led.calls)
	}
	if led.calls[1].tokens != 42 {
		t.Fatalf("ledger tokens = %d, want 42", led.calls[1].tokens)
	}
}

func TestChatEmptyChainAndUnknownProvider(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{}, nil, testConfig())
	if _, _, err := eng.ChatWithChain(context.Background(), nil, testReq()); err == nil {
		t.Fatal("empty chain: want error")
	}
	b := okClient("b-ok")
	eng = NewEngine(map[string]provider.Client{"b": b}, nil, testConfig())
	resp, trace, err := eng.ChatWithChain(context.Background(),
		[]Attempt{{ProviderID: "ghost", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "b-ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if outcome(trace, 0) != "error" {
		t.Fatalf("first outcome = %q, want error", outcome(trace, 0))
	}
}

// ---------------------------------------------------------------------------
// 429 / Retry-After / cooldown.
// ---------------------------------------------------------------------------

func TestChatRateLimitThenSuccess(t *testing.T) {
	a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.RateLimitError{RetryAfter: 0, Body: "slow down"}
	}}
	b := okClient("b-ok")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, trace, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "b-ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if outcome(trace, 0) != "rate_limited" {
		t.Fatalf("first outcome = %q, want rate_limited", outcome(trace, 0))
	}
}

func TestChatAlways429RetriesWithinBudget(t *testing.T) {
	a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.RateLimitError{RetryAfter: 0}
	}}
	cfg := testConfig()
	cfg.MaxAttempts = 3
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	_, trace, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
	if err == nil {
		t.Fatal("want provider.RateLimitError")
	}
	var rle *provider.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %T %v, want *provider.RateLimitError", err, err)
	}
	if a.chatCalls() != 3 {
		t.Fatalf("calls = %d, want 3 (attempt budget)", a.chatCalls())
	}
	if len(trace.Attempts) != 3 {
		t.Fatalf("trace attempts = %d, want 3", len(trace.Attempts))
	}
}

func TestChatCooldownSkipsProvider(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return nil, &provider.RateLimitError{RetryAfter: 500 * time.Millisecond}
		}
		return &provider.ChatResponse{Content: "a-ok"}, nil
	}}
	b := okClient("b-ok")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}

	resp, _, err := eng.ChatWithChain(context.Background(), chain, testReq())
	if err != nil || resp.Content != "b-ok" {
		t.Fatalf("first Chat: resp=%q err=%v", resp.Content, err)
	}
	// A is now on cooldown for 500ms: the second call must skip it even
	// though it would succeed now.
	resp, trace, err := eng.ChatWithChain(context.Background(), chain, testReq())
	if err != nil || resp.Content != "b-ok" {
		t.Fatalf("second Chat: resp=%q err=%v", resp.Content, err)
	}
	if a.chatCalls() != 1 {
		t.Fatalf("a calls = %d, want 1 (cooldown should skip it)", a.chatCalls())
	}
	if b.chatCalls() != 2 {
		t.Fatalf("b calls = %d, want 2", b.chatCalls())
	}
	if outcome(trace, 0) != "skipped_cooldown" {
		t.Fatalf("first outcome = %q, want skipped_cooldown", outcome(trace, 0))
	}
}

func TestChatRetryAfterHonoured(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return nil, &provider.RateLimitError{RetryAfter: 30 * time.Millisecond}
		}
		return &provider.ChatResponse{Content: "a-ok"}, nil
	}}
	cfg := testConfig()
	cfg.MaxAttempts = 3
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	start := time.Now()
	resp, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
	elapsed := time.Since(start)
	if err != nil || resp.Content != "a-ok" {
		t.Fatalf("Chat: resp=%q err=%v", resp.Content, err)
	}
	if a.chatCalls() != 2 {
		t.Fatalf("calls = %d, want 2", a.chatCalls())
	}
	if elapsed < 25*time.Millisecond {
		t.Fatalf("elapsed = %v, want >= 25ms (Retry-After 30ms honoured)", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Circuit breaker.
// ---------------------------------------------------------------------------

func TestCircuitBreakerOpensAfterThreshold(t *testing.T) {
	a := errClient(errors.New("down"))
	b := okClient("b-ok")
	cfg := testConfig()
	cfg.BreakerThreshold = 5
	cfg.BreakerOpenFor = time.Minute
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	for i := 0; i < 5; i++ {
		resp, _, err := eng.ChatWithChain(context.Background(), chain, testReq())
		if err != nil || resp.Content != "b-ok" {
			t.Fatalf("call %d: resp=%q err=%v", i, resp.Content, err)
		}
	}
	if a.chatCalls() != 5 {
		t.Fatalf("a calls = %d, want 5", a.chatCalls())
	}
	// 6th call: breaker open, A must be skipped, not called.
	_, trace, err := eng.ChatWithChain(context.Background(), chain, testReq())
	if err != nil {
		t.Fatalf("6th Chat: %v", err)
	}
	if a.chatCalls() != 5 {
		t.Fatalf("a calls = %d after breaker opened, want 5", a.chatCalls())
	}
	if outcome(trace, 0) != "skipped_breaker" {
		t.Fatalf("first outcome = %q, want skipped_breaker", outcome(trace, 0))
	}
}

func TestCircuitBreakerHalfOpenProbe(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			return nil, errors.New("down")
		}
		return &provider.ChatResponse{Content: "a-ok"}, nil
	}}
	b := okClient("b-ok")
	cfg := testConfig()
	cfg.BreakerThreshold = 2
	cfg.BreakerOpenFor = 40 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}

	// Two failures open the breaker.
	for i := 0; i < 2; i++ {
		if _, _, err := eng.ChatWithChain(context.Background(), chain, testReq()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if a.chatCalls() != 2 {
		t.Fatalf("a calls = %d, want 2", a.chatCalls())
	}
	// Still open: skipped.
	if _, _, err := eng.ChatWithChain(context.Background(), chain, testReq()); err != nil {
		t.Fatalf("call 3: %v", err)
	}
	if a.chatCalls() != 2 {
		t.Fatalf("a calls = %d while open, want 2", a.chatCalls())
	}
	// Wait out the open period: half-open probe is allowed and succeeds.
	time.Sleep(60 * time.Millisecond)
	resp, _, err := eng.ChatWithChain(context.Background(), chain, testReq())
	if err != nil || resp.Content != "a-ok" {
		t.Fatalf("probe call: resp=%q err=%v", resp.Content, err)
	}
	if a.chatCalls() != 3 {
		t.Fatalf("a calls = %d, want 3 (probe ran)", a.chatCalls())
	}
	// Breaker closed again: A is tried first on the next request.
	resp, _, err = eng.ChatWithChain(context.Background(), chain, testReq())
	if err != nil || resp.Content != "a-ok" {
		t.Fatalf("post-probe call: resp=%q err=%v", resp.Content, err)
	}
	if a.chatCalls() != 4 {
		t.Fatalf("a calls = %d, want 4", a.chatCalls())
	}
}

// ---------------------------------------------------------------------------
// Attempt budget.
// ---------------------------------------------------------------------------

func TestChatAttemptBudgetCapsTotalCalls(t *testing.T) {
	clients := map[string]provider.Client{}
	chain := make([]Attempt, 0, 8)
	for i := 0; i < 8; i++ {
		id := string(rune('a' + i))
		clients[id] = errClient(errors.New("down"))
		chain = append(chain, Attempt{ProviderID: id, Model: "m"})
	}
	cfg := testConfig()
	cfg.MaxAttempts = 5
	eng := NewEngine(clients, nil, cfg)
	_, trace, err := eng.ChatWithChain(context.Background(), chain, testReq())
	if err == nil {
		t.Fatal("want error, all providers fail")
	}
	if len(trace.Attempts) != 5 {
		t.Fatalf("trace attempts = %d, want 5 (budget)", len(trace.Attempts))
	}
	total := 0
	for _, c := range clients {
		total += c.(*fakeClient).chatCalls()
	}
	if total != 5 {
		t.Fatalf("total provider calls = %d, want 5", total)
	}
}

// ---------------------------------------------------------------------------
// Hedging.
// ---------------------------------------------------------------------------

func TestChatHedgeSlowFirstFastSecond(t *testing.T) {
	a := delayClient(300*time.Millisecond, "slow")
	b := okClient("fast")
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	start := time.Now()
	resp, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "fast" {
		t.Fatalf("content = %q, want fast (hedge winner)", resp.Content)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("elapsed = %v, hedge should have returned in well under 250ms", elapsed)
	}
	if b.chatCalls() != 1 {
		t.Fatalf("b calls = %d, want 1", b.chatCalls())
	}
}

func TestChatHedgeFirstFastWinsSecondNeverStarts(t *testing.T) {
	a := okClient("fast-a")
	b := okClient("slow-b")
	cfg := testConfig()
	cfg.HedgeDelay = 50 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	resp, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "fast-a" {
		t.Fatalf("content = %q", resp.Content)
	}
	if b.chatCalls() != 0 {
		t.Fatalf("b calls = %d, want 0 (hedge never fired)", b.chatCalls())
	}
}

func TestChatHedgeFirstErrorsSecondSucceeds(t *testing.T) {
	a := errClient(errors.New("boom"))
	b := delayClient(30*time.Millisecond, "from-b")
	cfg := testConfig()
	cfg.HedgeDelay = 10 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	// A fails fast, before the hedge delay: plain fallback to B.
	resp, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil || resp.Content != "from-b" {
		t.Fatalf("resp=%q err=%v", resp.Content, err)
	}
}

// ---------------------------------------------------------------------------
// Streaming failover / resume.
// ---------------------------------------------------------------------------

func TestStreamMidFailureResumesWithPartial(t *testing.T) {
	a := streamClient([]string{"Hello ", "brave "}, errors.New("boom"))
	b := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if err := onDelta("world"); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "world"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	out, err := collectStream(t, ch)
	if err != nil {
		t.Fatalf("stream err: %v (partial %q)", err, out)
	}
	if out != "Hello brave world" {
		t.Fatalf("stream = %q, want %q", out, "Hello brave world")
	}
	// The resuming request must carry the partial as an assistant message.
	req, ok := b.lastStreamReq()
	if !ok {
		t.Fatal("b never received a stream request")
	}
	if len(req.Messages) != 2 {
		t.Fatalf("b request messages = %+v, want original + assistant partial", req.Messages)
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != RoleAssistant || last.Content != "Hello brave " {
		t.Fatalf("resume message = %+v, want assistant %q", last, "Hello brave ")
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace = %+v, want 2 attempts", trace)
	}
	if outcome(trace, 0) != "stream_error" || outcome(trace, 1) != "success" {
		t.Fatalf("outcomes = %q, %q", outcome(trace, 0), outcome(trace, 1))
	}
}

func TestStreamFailBeforeAnyDeltaPlainFailover(t *testing.T) {
	a := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		return nil, errors.New("nope")
	}}
	b := streamClient([]string{"bee"}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	out, err := collectStream(t, ch)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if out != "bee" {
		t.Fatalf("stream = %q, want %q", out, "bee")
	}
	// No partial existed, so B gets the original request unchanged.
	req, _ := b.lastStreamReq()
	if len(req.Messages) != 1 {
		t.Fatalf("b request messages = %+v, want original only", req.Messages)
	}
}

func TestStreamAllAttemptsFailDeliversError(t *testing.T) {
	a := streamClient(nil, errors.New("a-down"))
	b := streamClient(nil, errors.New("b-down"))
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	_, err = collectStream(t, ch)
	if err == nil {
		t.Fatal("want terminal stream error")
	}
}

func TestStreamHedgeSlowFirstToken(t *testing.T) {
	a := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		select {
		case <-time.After(300 * time.Millisecond):
			if err := onDelta("slow"); err != nil {
				return nil, err
			}
			return &provider.ChatResponse{Content: "slow"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	b := streamClient([]string{"fast"}, nil)
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	start := time.Now()
	out, err := collectStream(t, ch)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if out != "fast" {
		t.Fatalf("stream = %q, want %q", out, "fast")
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatalf("stream took %v, hedge should have won fast", time.Since(start))
	}
}

func TestStreamEmptyChain(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{}, nil, testConfig())
	if _, err := eng.ChatStreamWithChain(context.Background(), nil, testReq()); err == nil {
		t.Fatal("empty chain: want error")
	}
}

// ---------------------------------------------------------------------------
// Server-facing API (server.Engine / StreamResumer contracts).
// ---------------------------------------------------------------------------

func TestServerChatUsesDefaultChain(t *testing.T) {
	a := errClient(errors.New("down"))
	b := okClient("from-b")
	// No ChainFor: the default chain is every client, sorted by ID.
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, err := eng.Chat(context.Background(), provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "from-b" {
		t.Fatalf("content = %q, want from-b", resp.Content)
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace = %+v, want 2 attempts", trace)
	}
	if trace.Attempts[0].Provider != "a" || trace.Attempts[1].Provider != "b" {
		t.Fatalf("trace providers = %+v", trace.Attempts)
	}
}

func TestServerChatStream(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": streamClient([]string{"Hello ", "world"}, nil)}, nil, testConfig())
	var got strings.Builder
	resp, err := eng.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}, func(delta string) error {
		got.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got.String() != "Hello world" {
		t.Fatalf("deltas = %q, want %q", got.String(), "Hello world")
	}
	if resp.Content != "Hello world" {
		t.Fatalf("response content = %q, want %q", resp.Content, "Hello world")
	}
}

func TestServerChatStreamFromSkipsSentPrefix(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": streamClient([]string{"Hello ", "world"}, nil)}, nil, testConfig())
	var got strings.Builder
	resp, err := eng.ChatStreamFrom(context.Background(), provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}, len("Hello "), func(delta string) error {
		got.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamFrom: %v", err)
	}
	if got.String() != "world" {
		t.Fatalf("deltas = %q, want %q (already-sent prefix skipped)", got.String(), "world")
	}
	if resp.Content != "Hello world" {
		t.Fatalf("response content = %q, want full content", resp.Content)
	}
}

func TestServerChatStreamFromSkipBeyondContent(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": streamClient([]string{"short"}, nil)}, nil, testConfig())
	var got strings.Builder
	resp, err := eng.ChatStreamFrom(context.Background(), provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}, 100, func(delta string) error {
		got.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamFrom: %v", err)
	}
	if got.String() != "" {
		t.Fatalf("deltas = %q, want none (skip past end)", got.String())
	}
	if resp.Content != "short" {
		t.Fatalf("response content = %q, want %q", resp.Content, "short")
	}
}
