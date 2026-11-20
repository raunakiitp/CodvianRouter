package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// NOTE: the engine's server.Engine / StreamResumer conformance is
// asserted in internal/server (strategies_test.go): asserting it
// here would import server from the router tests, and server now
// imports router (strategy reporting) — an import cycle in test.

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
	usage     provider.Usage
}

func (l *fakeLedger) RecordAttempt(provider, model string, ok bool, latencyMs int64, usage provider.Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, ledgerCall{provider: provider, model: model, ok: ok, latencyMs: latencyMs, usage: usage})
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
	// wireFormat is the wire this fake speaks, for the reasoning
	// handoff label ("" = unknown: no label, never a guessed one).
	wireFormat string
}

// WireFormat reports the wire this fake speaks (see wireFormat).
func (f *fakeClient) WireFormat() string { return f.wireFormat }

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

// prereadChunkClient streams chunk-aware: it emits scripted chunks in
// order, then returns chunkErr (or a completed response). It lets
// preread tests control events the text-only fakeClient cannot
// produce (empty chunks, finish-only chunks, tool-call deltas) —
// bridgeStream prefers ChatStreamChunks when present.
type prereadChunkClient struct {
	mu         sync.Mutex
	streams    int
	streamReqs []provider.ChatRequest
	chunkFn    func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error)
}

func (c *prereadChunkClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *prereadChunkClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	// bridgeStream never calls this when ChatStreamChunks exists;
	// it is only here to satisfy provider.Client.
	if onDelta != nil {
		if err := onDelta("ok"); err != nil {
			return nil, err
		}
	}
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *prereadChunkClient) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	c.mu.Lock()
	c.streams++
	c.streamReqs = append(c.streamReqs, req)
	fn := c.chunkFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, req, onChunk)
	}
	return &provider.ChatResponse{}, nil
}

// ListModels and TestKey complete the provider.Client interface; the
// router never calls them.
func (c *prereadChunkClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *prereadChunkClient) TestKey(ctx context.Context) error                { return nil }

func (c *prereadChunkClient) streamCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams
}

// chunkClient emits chunks in order via onChunk, then returns err
// (or a completed response when err is nil).
func prereadChunks(chunks []provider.StreamChunk, err error) *prereadChunkClient {
	return &prereadChunkClient{chunkFn: func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		var sb strings.Builder
		for _, ck := range chunks {
			sb.WriteString(ck.Content)
			if err := onChunk(ck); err != nil {
				return nil, err
			}
		}
		if err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: sb.String()}, nil
	}}
}

// collectStream drains a ChatStream channel, returning the joined
// deltas and the terminal error (nil on Done). It drains until the
// channel CLOSES, not just until the Done/Err event: the engine
// publishes LastTrace in a defer that runs after the terminal event
// is sent, so returning on Done would race the trace (same class of
// bug the server drain fixed for CHECKLIST 133 — a consumer that
// returns on Done can read a nil/stale LastTrace).
func collectStream(t *testing.T, ch <-chan StreamEvent) (string, error) {
	t.Helper()
	var sb strings.Builder
	var terminalErr error
	sawTerminal := false
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				if !sawTerminal {
					return sb.String(), errors.New("stream closed without Done or error")
				}
				return sb.String(), terminalErr
			}
			if ev.Err != nil {
				terminalErr = ev.Err
				sawTerminal = true
				continue
			}
			sb.WriteString(ev.Delta)
			if ev.Done {
				sawTerminal = true
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

func outcome(trace *routetrace.RouteTrace, i int) string {
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

// registryWithHints builds a registry backed by a temp JSON file
// holding providers with the given quota hints (fraction 0..1, or
// provider.QuotaUnknown). No network is involved: the file is written
// directly and loaded through the registry's Store.
func registryWithHints(t *testing.T, hints map[string]float64) *provider.Registry {
	t.Helper()
	cfgs := make([]provider.ProviderConfig, 0, len(hints))
	for id, hint := range hints {
		cfgs = append(cfgs, provider.ProviderConfig{
			ID: id, Name: id, BaseURL: "https://" + id + ".example.com",
			WireFormat: provider.WireFormatOpenAI, Enabled: true, QuotaHint: hint,
		})
	}
	data, err := json.Marshal(cfgs)
	if err != nil {
		t.Fatalf("marshal provider configs: %v", err)
	}
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write provider store: %v", err)
	}
	reg := provider.NewRegistry(provider.NewJSONFileStore(path))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry Load: %v", err)
	}
	return reg
}

func TestQuotaHintFuncMapping(t *testing.T) {
	reg := registryWithHints(t, map[string]float64{
		"prov-high": 0.9, "prov-half": 0.5, "prov-zero": 0,
		"prov-unknown": provider.QuotaUnknown, "prov-full": 1,
	})
	fn := QuotaHintFunc(reg)
	cases := []struct {
		id   string
		want int
	}{
		{"prov-high", 90},
		{"prov-half", 50},
		{"prov-zero", 0},
		{"prov-unknown", 100},
		{"prov-full", 100},
		{"no-such-provider", 100},
	}
	for _, c := range cases {
		if got := fn(c.id); got != c.want {
			t.Errorf("QuotaHintFunc(%q) = %d, want %d", c.id, got, c.want)
		}
	}
	if got := QuotaHintFunc(nil)("anything"); got != 100 {
		t.Errorf("QuotaHintFunc(nil) = %d, want 100", got)
	}
}

func TestChatQuotaHintFuncFromRegistry(t *testing.T) {
	reg := registryWithHints(t, map[string]float64{
		"prov-low": 0.1, "prov-high": 0.9, "prov-zero": 0,
	})
	low := okClient("low")
	high := okClient("high")
	zero := okClient("zero")
	cfg := testConfig()
	cfg.QuotaHint = QuotaHintFunc(reg)
	eng := NewEngine(map[string]provider.Client{"prov-low": low, "prov-high": high, "prov-zero": zero}, nil, cfg)
	chain := []Attempt{{ProviderID: "prov-low", Model: "m"}, {ProviderID: "prov-zero", Model: "m"}, {ProviderID: "prov-high", Model: "m"}}

	resp, trace, err := eng.ChatWithChain(context.Background(), chain, testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "high" {
		t.Fatalf("content = %q, want high first (highest hint)", resp.Content)
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

	// Live update: drain the high provider's hint to 0 — no restart,
	// no new engine — and the next request must skip it.
	if err := reg.SetQuotaHint("prov-high", 0); err != nil {
		t.Fatalf("SetQuotaHint: %v", err)
	}
	resp, trace, err = eng.ChatWithChain(context.Background(), chain, testReq())
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if resp.Content != "low" {
		t.Fatalf("second content = %q, want low (high now zero-quota)", resp.Content)
	}
	if high.chatCalls() != 1 {
		t.Fatalf("high calls = %d, want 1 (skipped after live update)", high.chatCalls())
	}
	if low.chatCalls() != 1 {
		t.Fatalf("low calls = %d, want 1", low.chatCalls())
	}
	if len(trace.Attempts) != 3 { // 2 skipped_zero_quota + 1 success
		t.Fatalf("second trace attempts = %d, want 3", len(trace.Attempts))
	}
	last := trace.Attempts[len(trace.Attempts)-1]
	if last.Outcome != "success" || last.Provider != "prov-low" {
		t.Fatalf("second trace = %+v, want low to serve", trace.Attempts)
	}
}

func TestChatLedgerAndTokens(t *testing.T) {
	a := errClient(errors.New("down"))
	b := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok", Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 32, TotalTokens: 42}}, nil
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
	// The failed attempt reports no usage: the split is unknown.
	if led.calls[0].usage != (provider.Usage{}) {
		t.Fatalf("failed attempt usage = %+v, want zero Usage", led.calls[0].usage)
	}
	// The successful attempt passes the upstream's prompt/completion
	// split through verbatim — never collapsed to a single total —
	// and records the local prompt estimate alongside it
	// (CHECKLIST 229-230: reconcileUsage always sets
	// LocalEstimatePrompt when a local count exists).
	local, err := tokencount.CountPromptFull("m2", testReq())
	if err != nil {
		t.Fatalf("tokencount: %v", err)
	}
	want := provider.Usage{PromptTokens: 10, CompletionTokens: 32, TotalTokens: 42, LocalEstimatePrompt: local.Tokens}
	if led.calls[1].usage != want {
		t.Fatalf("ledger usage = %+v, want %+v", led.calls[1].usage, want)
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
			return nil, &provider.RateLimitError{RetryAfter: 500 * time.Millisecond, RetryAfterPresent: true}
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
	// A is now on cooldown: the 500ms hint is floored to 10s
	// (CHECKLIST 168), so the second call must skip it even
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
			// A 30ms hint is floored to the 10s floor
			// (CHECKLIST 168): the same-target retry waits
			// 10s, not 30ms — this test takes ~10s by design
			// and proves the floor is actually slept, not
			// just computed.
			return nil, &provider.RateLimitError{RetryAfter: 30 * time.Millisecond, RetryAfterPresent: true}
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
	if elapsed < 9*time.Second {
		t.Fatalf("elapsed = %v, want >= 9s (Retry-After 30ms floored to 10s)", elapsed)
	}
	if elapsed > 60*time.Second {
		t.Fatalf("elapsed = %v, want < 60s (a floored retry must not park the request)", elapsed)
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
	// G11 Area 2: the cross-provider default (3) would bind
	// before MaxAttempts here, so this test opts out of it
	// (MaxProvidersPerRequest 8) to isolate what it measures:
	// MaxAttempts caps total calls.
	cfg.MaxProvidersPerRequest = 8
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

func TestStreamMidFailureNoSwitchAfterCommit(t *testing.T) {
	// CHECKLIST 133 (research-log entries 97 + 111): once the
	// first content byte is sent, the stream NEVER switches
	// candidates — no mid-stream resume. A failure after "Hello
	// brave " ends the stream with an error; B is never dialled.
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
	if err == nil {
		t.Fatalf("stream err = nil, want the upstream failure (partial %q)", out)
	}
	if out != "Hello brave " {
		t.Fatalf("stream = %q, want only the delivered prefix %q", out, "Hello brave ")
	}
	if b.streamCalls() != 0 {
		t.Fatalf("b stream calls = %d, want 0 (no switch after commit)", b.streamCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 {
		t.Fatalf("trace = %+v, want exactly 1 attempt", trace)
	}
	if outcome(trace, 0) != "stream_error" {
		t.Fatalf("outcome = %q, want stream_error", outcome(trace, 0))
	}
}

// TestStreamingPrereadFailoverGate is the permanent gate for
// CHECKLIST 133 (research-log entries 97 + 111): the streaming
// preread buffer. It proves, through the real Engine with fake
// clients: (a) an empty stream fails over in the SAME request with
// no partial body delivered; (b) an error before the first byte
// fails over; (c) content delivered then upstream death ends the
// stream with an error and NO switch; (d) every replay exemption
// is never retried. The buffer bounds (8 events / 5 s) are proven
// too: hitting one commits the stream instead of failing over.
func TestStreamingPrereadFailoverGate(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	newEngine := func(a, b provider.Client) *Engine {
		return NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	}
	runStream := func(t *testing.T, eng *Engine, req provider.ChatRequest) (string, error) {
		t.Helper()
		ch, err := eng.ChatStreamWithChain(context.Background(), chain, req)
		if err != nil {
			t.Fatalf("ChatStreamWithChain: %v", err)
		}
		return collectStream(t, ch)
	}

	t.Run("empty-stream-fails-over", func(t *testing.T) {
		// (a) A ends with [DONE] and no content: the same request
		// succeeds via B, and the client never sees a partial body.
		a := prereadChunks(nil, nil) // no chunks, clean Done
		b := streamClient([]string{"bee"}, nil)
		eng := newEngine(a, b)
		out, err := runStream(t, eng, testReq())
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
		if out != "bee" {
			t.Fatalf("stream = %q, want %q", out, "bee")
		}
		// M4.8 early-retry: a truncating/transport-blip stream
		// is re-opened on the same target up to 3 times (1 + 3
		// under the 5-attempt budget, leaving one attempt for
		// the failover) before the chain moves on.
		if a.streamCalls() != 4 || b.streamCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 4/1", a.streamCalls(), b.streamCalls())
		}
		trace := eng.LastTrace()
		if trace == nil || len(trace.Attempts) != 5 {
			t.Fatalf("trace = %+v, want 5 attempts (4 empty + 1 success)", trace)
		}
		if got := trace.Attempts[0]; got.Outcome != "empty_stream" || got.Reason != "empty_stream" || got.Preread != "empty" {
			t.Fatalf("attempt 0 = %+v, want outcome/reason empty_stream, preread empty", got)
		}
		if outcome(trace, 4) != "success" {
			t.Fatalf("attempt 4 outcome = %q, want success", outcome(trace, 4))
		}
		// B gets the original request unchanged (no partial existed).
		req, _ := b.lastStreamReq()
		if len(req.Messages) != 1 {
			t.Fatalf("b request messages = %+v, want original only", req.Messages)
		}
	})

	t.Run("empty-finish-only-chunk-fails-over", func(t *testing.T) {
		// (a, variant) A sends only a finish_reason chunk, then
		// Done: a finish reason with no content is still empty.
		a := prereadChunks([]provider.StreamChunk{{FinishReason: "stop"}}, nil)
		b := streamClient([]string{"bee"}, nil)
		eng := newEngine(a, b)
		out, err := runStream(t, eng, testReq())
		if err != nil || out != "bee" {
			t.Fatalf("stream = %q err = %v, want %q nil", out, err, "bee")
		}
		// M4.8 early-retry: a truncating/transport-blip stream
		// is re-opened on the same target up to 3 times (1 + 3
		// under the 5-attempt budget, leaving one attempt for
		// the failover) before the chain moves on.
		if a.streamCalls() != 4 || b.streamCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 4/1", a.streamCalls(), b.streamCalls())
		}
		if got := eng.LastTrace().Attempts[0]; got.Outcome != "empty_stream" || got.Preread != "empty" {
			t.Fatalf("attempt 0 = %+v, want empty_stream/empty", got)
		}
	})

	t.Run("error-before-first-byte-fails-over", func(t *testing.T) {
		// (b) A errors before any content: same request, via B.
		a := streamClient(nil, errors.New("boom"))
		b := streamClient([]string{"bee"}, nil)
		eng := newEngine(a, b)
		out, err := runStream(t, eng, testReq())
		if err != nil || out != "bee" {
			t.Fatalf("stream = %q err = %v, want %q nil", out, err, "bee")
		}
		// M4.8 early-retry: a truncating/transport-blip stream
		// is re-opened on the same target up to 3 times (1 + 3
		// under the 5-attempt budget, leaving one attempt for
		// the failover) before the chain moves on.
		if a.streamCalls() != 4 || b.streamCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 4/1", a.streamCalls(), b.streamCalls())
		}
		trace := eng.LastTrace()
		if len(trace.Attempts) != 5 || outcome(trace, 0) != "error" || outcome(trace, 4) != "success" {
			t.Fatalf("trace = %+v, want error then success", trace)
		}
	})

	t.Run("content-then-death-no-switch", func(t *testing.T) {
		// (c) A sends content, then dies: the stream ends with the
		// error and B is never dialled.
		a := streamClient([]string{"Hello "}, errors.New("boom"))
		b := streamClient([]string{"world"}, nil)
		eng := newEngine(a, b)
		out, err := runStream(t, eng, testReq())
		if err == nil {
			t.Fatalf("stream err = nil, want the upstream failure (partial %q)", out)
		}
		if out != "Hello " {
			t.Fatalf("stream = %q, want only the delivered prefix", out)
		}
		if b.streamCalls() != 0 {
			t.Fatalf("b stream calls = %d, want 0 (no switch after commit)", b.streamCalls())
		}
		if trace := eng.LastTrace(); len(trace.Attempts) != 1 || outcome(trace, 0) != "stream_error" {
			t.Fatalf("trace = %+v, want exactly 1 stream_error attempt", trace)
		}
	})

	t.Run("tool-call-delta-is-content", func(t *testing.T) {
		// A tool-call delta is real content: it commits the stream
		// immediately (it must NOT be treated as an empty stream
		// and failed over — the client will execute that call).
		a := prereadChunks([]provider.StreamChunk{
			{ToolCalls: []provider.ChunkToolCall{{Index: 0, ID: "call_1", Name: "get_weather", Arguments: `{"city":"Pune"}`}}},
			{Content: "hi"},
		}, nil)
		b := streamClient([]string{"bee"}, nil)
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		var gotTool bool
		var sb strings.Builder
		resp, err := eng.ChatStreamChunks(context.Background(), testReq(), func(chunk provider.StreamChunk) error {
			if len(chunk.ToolCalls) > 0 {
				gotTool = true
			}
			sb.WriteString(chunk.Content)
			return nil
		})
		if err != nil {
			t.Fatalf("ChatStreamChunks: %v", err)
		}
		if !gotTool {
			t.Fatal("tool-call delta never reached the client")
		}
		if sb.String() != "hi" || resp.Content != "hi" {
			t.Fatalf("content = %q resp = %q, want hi", sb.String(), resp.Content)
		}
		if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "get_weather" {
			t.Fatalf("tool calls = %+v, want the get_weather call", resp.ToolCalls)
		}
		if b.streamCalls() != 0 {
			t.Fatalf("b stream calls = %d, want 0", b.streamCalls())
		}
	})

	t.Run("event-limit-commits", func(t *testing.T) {
		// 8 empty leading events, then content: the buffer holds
		// all 8 and commits on the content — no failover.
		chunks := make([]provider.StreamChunk, 0, 9)
		for i := 0; i < PrereadMaxEvents; i++ {
			chunks = append(chunks, provider.StreamChunk{})
		}
		chunks = append(chunks, provider.StreamChunk{Content: "hi"})
		a := prereadChunks(chunks, nil)
		b := streamClient([]string{"bee"}, nil)
		eng := newEngine(a, b)
		out, err := runStream(t, eng, testReq())
		if err != nil || out != "hi" {
			t.Fatalf("stream = %q err = %v, want %q nil", out, err, "hi")
		}
		if b.streamCalls() != 0 {
			t.Fatalf("b stream calls = %d, want 0 (limit commits, not failover)", b.streamCalls())
		}
		if got := eng.LastTrace().Attempts[0]; got.Preread != "held" || got.PrereadEvents != PrereadMaxEvents {
			t.Fatalf("attempt 0 = %+v, want preread held with %d events", got, PrereadMaxEvents)
		}
	})

	t.Run("wait-limit-commits", func(t *testing.T) {
		// One empty event, then a slow upstream: the 5 s bound
		// commits what was held instead of failing over.
		old := prereadMaxWait
		prereadMaxWait = 40 * time.Millisecond
		defer func() { prereadMaxWait = old }()
		a := &prereadChunkClient{chunkFn: func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
			if err := onChunk(provider.StreamChunk{}); err != nil {
				return nil, err
			}
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if err := onChunk(provider.StreamChunk{Content: "hi"}); err != nil {
				return nil, err
			}
			return &provider.ChatResponse{Content: "hi"}, nil
		}}
		b := streamClient([]string{"bee"}, nil)
		eng := newEngine(a, b)
		out, err := runStream(t, eng, testReq())
		if err != nil || out != "hi" {
			t.Fatalf("stream = %q err = %v, want %q nil", out, err, "hi")
		}
		if b.streamCalls() != 0 {
			t.Fatalf("b stream calls = %d, want 0 (wait bound commits, not failover)", b.streamCalls())
		}
		if got := eng.LastTrace().Attempts[0]; got.Preread != "held" || got.PrereadEvents != 1 {
			t.Fatalf("attempt 0 = %+v, want preread held with 1 event", got)
		}
	})

	t.Run("exemptions-never-retried", func(t *testing.T) {
		// (d) Every replay exemption, on both an empty stream and
		// an error: exactly one attempt on A, B never dialled.
		// NOTE (CHECKLIST 177 slice, minimal fix for a
		// concurrent-slice build break): the "responses" case
		// (fresh /v1/responses create, ExemptResponses) was
		// removed here — the CHECKLIST 169 slice already
		// deleted ExemptResponses from preread.go because a
		// fresh Responses create now fails over like chat, so
		// the old case could not compile and contradicted the
		// landed production code. The 237-238 slice removed
		// the "responses-continuation" case the same way:
		// a branched create is replay-safe too (the gateway
		// holds the transcript), so it fails over like chat.
		cases := []struct {
			name string
			req  provider.ChatRequest
			want string
		}{
			{"embeddings", withEndpoint(testReq(), provider.EndpointEmbeddings), ExemptEmbeddings},
			{"images", withEndpoint(testReq(), provider.EndpointImages), ExemptImages},
			{"rerank", withEndpoint(testReq(), provider.EndpointRerank), ExemptRerank},
			{"warm-up", withWarmUp(testReq()), ExemptWarmUp},
		}
		for _, tc := range cases {
			t.Run(tc.name+"/empty", func(t *testing.T) {
				a := prereadChunks(nil, nil)
				b := streamClient([]string{"bee"}, nil)
				eng := newEngine(a, b)
				out, err := runStream(t, eng, tc.req)
				if err != nil || out != "" {
					t.Fatalf("stream = %q err = %v, want empty success (no failover)", out, err)
				}
				if a.streamCalls() != 1 || b.streamCalls() != 0 {
					t.Fatalf("calls a=%d b=%d, want 1/0", a.streamCalls(), b.streamCalls())
				}
				if got := eng.LastTrace().Attempts[0]; got.Preread != "exempt:"+tc.want {
					t.Fatalf("attempt 0 preread = %q, want exempt:%s", got.Preread, tc.want)
				}
			})
			t.Run(tc.name+"/error", func(t *testing.T) {
				a := streamClient(nil, errors.New("boom"))
				b := streamClient([]string{"bee"}, nil)
				eng := newEngine(a, b)
				_, err := runStream(t, eng, tc.req)
				if err == nil {
					t.Fatal("stream err = nil, want the upstream failure (no failover)")
				}
				if a.streamCalls() != 1 || b.streamCalls() != 0 {
					t.Fatalf("calls a=%d b=%d, want 1/0", a.streamCalls(), b.streamCalls())
				}
				if trace := eng.LastTrace(); len(trace.Attempts) != 1 {
					t.Fatalf("trace = %+v, want exactly 1 attempt", trace)
				}
			})
		}
	})
}

func withEndpoint(req provider.ChatRequest, ep provider.EndpointKind) provider.ChatRequest {
	req.Endpoint = ep
	return req
}

func withPrevResponse(req provider.ChatRequest, id string) provider.ChatRequest {
	req.PreviousResponseID = id
	return req
}

func withWarmUp(req provider.ChatRequest) provider.ChatRequest {
	req.WarmUp = true
	return req
}

// TestChatExemptRequestsSingleAttempt proves the replay exemption
// on the non-streaming path too: an exempt request that fails gets
// exactly one attempt — no fallback, no same-provider retry, no
// hedge — so a stateful request can never be double-applied.
func TestChatExemptRequestsSingleAttempt(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	for _, tc := range []struct {
		name string
		req  provider.ChatRequest
	}{
		{"embeddings", withEndpoint(testReq(), provider.EndpointEmbeddings)},
		{"warm-up", withWarmUp(testReq())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := errClient(errors.New("boom"))
			b := okClient("bee")
			eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
			_, _, err := eng.ChatWithChain(context.Background(), chain, tc.req)
			if err == nil {
				t.Fatal("Chat err = nil, want the upstream failure (no failover)")
			}
			if a.chatCalls() != 1 || b.chatCalls() != 0 {
				t.Fatalf("calls a=%d b=%d, want 1/0", a.chatCalls(), b.chatCalls())
			}
			if trace := eng.LastTrace(); len(trace.Attempts) != 1 {
				t.Fatalf("trace = %+v, want exactly 1 attempt", trace)
			}
		})
	}
}

// TestResponsesFreshCreateFailsOver proves the CHECKLIST 169 split:
// a FRESH /v1/responses create (no previous_response_id) is
// replay-safe pre-commit and fails over like a chat completion —
// an empty stream and an error before the first byte both switch
// to the fallback in the SAME request. A continuation fails over
// the same way since CHECKLIST 237 (see
// TestResponsesContinuationFailsOver). Sensitivity: with the preread
// exemption restored for EndpointResponses, both subtests fail (B
// never dialled).
func TestResponsesFreshCreateFailsOver(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	freshReq := withEndpoint(testReq(), provider.EndpointResponses)
	runStream := func(t *testing.T, eng *Engine, req provider.ChatRequest) (string, error) {
		t.Helper()
		ch, err := eng.ChatStreamWithChain(context.Background(), chain, req)
		if err != nil {
			t.Fatalf("ChatStreamWithChain: %v", err)
		}
		return collectStream(t, ch)
	}
	t.Run("empty-stream", func(t *testing.T) {
		a := prereadChunks(nil, nil)
		b := streamClient([]string{"bee"}, nil)
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		out, err := runStream(t, eng, freshReq)
		if err != nil || out != "bee" {
			t.Fatalf("stream = %q err = %v, want %q nil (failover to B)", out, err, "bee")
		}
		// M4.8 early-retry: a truncating/transport-blip stream
		// is re-opened on the same target up to 3 times (1 + 3
		// under the 5-attempt budget, leaving one attempt for
		// the failover) before the chain moves on.
		if a.streamCalls() != 4 || b.streamCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 4/1", a.streamCalls(), b.streamCalls())
		}
	})
	t.Run("error-before-first-byte", func(t *testing.T) {
		a := streamClient(nil, errors.New("boom"))
		b := streamClient([]string{"bee"}, nil)
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		out, err := runStream(t, eng, freshReq)
		if err != nil || out != "bee" {
			t.Fatalf("stream = %q err = %v, want %q nil (failover to B)", out, err, "bee")
		}
		// M4.8 early-retry: a truncating/transport-blip stream
		// is re-opened on the same target up to 3 times (1 + 3
		// under the 5-attempt budget, leaving one attempt for
		// the failover) before the chain moves on.
		if a.streamCalls() != 4 || b.streamCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 4/1", a.streamCalls(), b.streamCalls())
		}
	})
	t.Run("non-streaming", func(t *testing.T) {
		a := errClient(errors.New("boom"))
		b := okClient("bee")
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		resp, _, err := eng.ChatWithChain(context.Background(), chain, freshReq)
		if err != nil || resp == nil || resp.Content != "bee" {
			t.Fatalf("Chat = %+v err = %v, want bee via failover", resp, err)
		}
		if a.chatCalls() != 1 || b.chatCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 1/1", a.chatCalls(), b.chatCalls())
		}
	})
}

// TestResponsesContinuationFailsOver proves the CHECKLIST 237
// rule: a branched create (previous_response_id set) fails over
// mid-branch like any other request — when the pinned provider
// (the one that served the parent) fails, the SAME branched input
// (the parent's transcript + the new turn) is replayed against
// the next target, and the reply comes from there. The transcript
// is gateway-held, so replaying it cannot double-apply upstream
// state. Sensitivity: with the old replay exemption restored, both
// subtests fail (B never dialled, the request errors instead).
func TestResponsesContinuationFailsOver(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	branchedReq := func() provider.ChatRequest {
		return provider.ChatRequest{
			Endpoint:           provider.EndpointResponses,
			PreviousResponseID: "resp_123",
			PinProviderID:      "a",
			Model:              "m",
			Messages: []provider.Message{
				{Role: "user", Content: "first"},
				{Role: "assistant", Content: "root reply"},
				{Role: "user", Content: "second"},
			},
		}
	}
	wantMsgs := []provider.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "root reply"},
		{Role: "user", Content: "second"},
	}
	checkBranchedInput := func(t *testing.T, got []provider.Message) {
		t.Helper()
		if len(got) != len(wantMsgs) {
			t.Fatalf("failover target messages = %+v, want the branched input %+v", got, wantMsgs)
		}
		for i := range wantMsgs {
			if got[i].Role != wantMsgs[i].Role || got[i].Content != wantMsgs[i].Content {
				t.Fatalf("failover target message %d = %+v, want %+v", i, got[i], wantMsgs[i])
			}
		}
	}

	t.Run("non-streaming", func(t *testing.T) {
		a := errClient(errors.New("boom"))
		b := okClient("bee")
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		resp, _, err := eng.ChatWithChain(context.Background(), chain, branchedReq())
		if err != nil || resp == nil || resp.Content != "bee" {
			t.Fatalf("Chat = %+v err = %v, want bee via failover", resp, err)
		}
		if a.chatCalls() != 1 || b.chatCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 1/1", a.chatCalls(), b.chatCalls())
		}
		checkBranchedInput(t, b.reqs[0].Messages)
		if trace := eng.LastTrace(); len(trace.Attempts) != 2 || trace.Attempts[0].Provider != "a" || trace.Attempts[1].Provider != "b" || trace.Attempts[1].Outcome != "success" {
			t.Fatalf("trace = %+v, want a-fail then b-success", trace)
		}
	})

	t.Run("stream-error-before-first-byte", func(t *testing.T) {
		a := streamClient(nil, errors.New("boom"))
		b := streamClient([]string{"bee"}, nil)
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		ch, err := eng.ChatStreamWithChain(context.Background(), chain, branchedReq())
		if err != nil {
			t.Fatalf("ChatStreamWithChain: %v", err)
		}
		out, err := collectStream(t, ch)
		if err != nil || out != "bee" {
			t.Fatalf("stream = %q err = %v, want %q via failover", out, err, "bee")
		}
		// M4.8 early-retry: a truncating/transport-blip stream
		// is re-opened on the same target up to 3 times (1 + 3
		// under the 5-attempt budget, leaving one attempt for
		// the failover) before the chain moves on.
		if a.streamCalls() != 4 || b.streamCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 4/1", a.streamCalls(), b.streamCalls())
		}
		req, ok := b.lastStreamReq()
		if !ok {
			t.Fatal("B recorded no stream request")
		}
		checkBranchedInput(t, req.Messages)
	})
}

// TestResponsesContinuationPin proves the provider pin
// (CHECKLIST 169, reordered by 237-238): a continuation names the
// provider that served the original response, and the engine tries
// that provider FIRST — even when it is not first in the chain.
// The pin is a preference, not a narrowing: when the pinned
// provider fails, the request fails over to the rest of the chain
// (see TestResponsesContinuationFailsOver), and when the pinned
// provider no longer serves the model, the normal chain applies.
func TestResponsesContinuationPin(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	pinnedReq := func(pin string) provider.ChatRequest {
		req := withPrevResponse(testReq(), "resp_123")
		req.PinProviderID = pin
		return req
	}
	t.Run("pin-wins-over-chain-order", func(t *testing.T) {
		a := errClient(errors.New("boom"))
		b := okClient("bee")
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		resp, _, err := eng.ChatWithChain(context.Background(), chain, pinnedReq("b"))
		if err != nil || resp == nil || resp.Content != "bee" {
			t.Fatalf("Chat = %+v err = %v, want bee from pinned B", resp, err)
		}
		if a.chatCalls() != 0 || b.chatCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 0/1 (A never tried)", a.chatCalls(), b.chatCalls())
		}
		if trace := eng.LastTrace(); len(trace.Attempts) != 1 || trace.Attempts[0].Provider != "b" {
			t.Fatalf("trace = %+v, want exactly 1 attempt on b", trace)
		}
	})
	t.Run("pin-missing-falls-back-to-chain", func(t *testing.T) {
		a := errClient(errors.New("boom"))
		b := okClient("bee")
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		resp, _, err := eng.ChatWithChain(context.Background(), chain, pinnedReq("gone"))
		if err != nil || resp == nil || resp.Content != "bee" {
			t.Fatalf("Chat = %+v err = %v, want bee via failover (unknown pin keeps the full chain)", resp, err)
		}
		if a.chatCalls() != 1 || b.chatCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 1/1", a.chatCalls(), b.chatCalls())
		}
	})
	t.Run("pin-streaming", func(t *testing.T) {
		a := streamClient([]string{"ay"}, nil)
		b := streamClient([]string{"bee"}, nil)
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		ch, err := eng.ChatStreamWithChain(context.Background(), chain, pinnedReq("b"))
		if err != nil {
			t.Fatalf("ChatStreamWithChain: %v", err)
		}
		out, err := collectStream(t, ch)
		if err != nil || out != "bee" {
			t.Fatalf("stream = %q err = %v, want %q from pinned B", out, err, "bee")
		}
		if a.streamCalls() != 0 || b.streamCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 0/1", a.streamCalls(), b.streamCalls())
		}
	})
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

// chunkFakeClient is a provider.Client that streams chunk-aware
// (tool-call deltas + finish reason), like the Anthropic client.
type chunkFakeClient struct {
	fakeClient
	chunks []provider.StreamChunk
	fail   error
}

func (f *chunkFakeClient) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	var full strings.Builder
	var toolCalls []provider.ToolCall
	finish := ""
	for _, c := range f.chunks {
		full.WriteString(c.Content)
		toolCalls = provider.MergeToolCallDeltas(toolCalls, c.ToolCalls)
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		if err := onChunk(c); err != nil {
			return nil, err
		}
	}
	return &provider.ChatResponse{Content: full.String(), ToolCalls: toolCalls, FinishReason: finish}, nil
}

func TestEngineCrossWireFailoverAnthropicToOpenAI(t *testing.T) {
	// The Anthropic client maps a 429 to *provider.RateLimitError;
	// the engine must fail over on it to an OpenAI provider, and
	// vice versa — the wire format of the failing attempt must not
	// matter, only the error type.
	for _, tc := range []struct {
		name  string
		first string
	}{
		{"anthropic-429-then-openai", "anth"},
		{"openai-429-then-anthropic", "oai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failing := errClient(&provider.RateLimitError{})
			winning := okClient("from-winner")
			clients := map[string]provider.Client{tc.first: failing, "other": winning}
			eng := NewEngine(clients, nil, testConfig())
			resp, trace, err := eng.ChatWithChain(context.Background(), []Attempt{
				{ProviderID: tc.first, Model: "m"},
				{ProviderID: "other", Model: "m"},
			}, provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: RoleUser, Content: "hi"}}})
			if err != nil {
				t.Fatalf("ChatWithChain: %v", err)
			}
			if resp.Content != "from-winner" {
				t.Fatalf("Content = %q, want from-winner", resp.Content)
			}
			if len(trace.Attempts) != 2 || trace.Attempts[0].Outcome != "rate_limited" || trace.Attempts[1].Outcome != "success" {
				t.Errorf("trace = %+v, want rate_limited then success", trace.Attempts)
			}
		})
	}
}

func TestEngineCrossWireFailoverGeminiToOpenAI(t *testing.T) {
	// Same shape as the Anthropic cross-wire test, but with a REAL
	// Gemini client as the failing attempt: its 429 (Gemini
	// RESOURCE_EXHAUSTED) must map to *provider.RateLimitError and
	// the engine must fail over to an OpenAI provider — and the
	// reverse direction must work too. This proves the failover
	// decision is driven by the error type, not the wire format.
	for _, tc := range []struct {
		name  string
		first string
	}{
		{"gemini-429-then-openai", "gem"},
		{"openai-429-then-gemini", "oai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clients map[string]provider.Client
			if tc.first == "gem" {
				// The Gemini client fails with a real 429
				// (RESOURCE_EXHAUSTED); the OpenAI fake wins.
				geminiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, ":generateContent") {
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = w.Write([]byte(`{"error":{"code":429,"message":"quota exceeded","status":"RESOURCE_EXHAUSTED"}}`))
						return
					}
					w.WriteHeader(http.StatusNotFound)
				}))
				defer geminiSrv.Close()
				gemClient, err := provider.NewClient(geminiSrv.URL, "gem-key",
					provider.WithAuthHeaders(map[string]string{"x-goog-api-key": "gem-key"}),
					provider.WithWireFormat(provider.WireFormatGemini))
				if err != nil {
					t.Fatalf("NewClient gemini: %v", err)
				}
				clients = map[string]provider.Client{"gem": gemClient, "other": okClient("from-winner")}
			} else {
				// The OpenAI fake fails with a 429; a real Gemini
				// client answers the second attempt.
				geminiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"from-winner"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
				}))
				defer geminiSrv.Close()
				gemClient, err := provider.NewClient(geminiSrv.URL, "gem-key",
					provider.WithAuthHeaders(map[string]string{"x-goog-api-key": "gem-key"}),
					provider.WithWireFormat(provider.WireFormatGemini))
				if err != nil {
					t.Fatalf("NewClient gemini: %v", err)
				}
				clients = map[string]provider.Client{"oai": errClient(&provider.RateLimitError{}), "other": gemClient}
			}
			eng := NewEngine(clients, nil, testConfig())
			resp, trace, err := eng.ChatWithChain(context.Background(), []Attempt{
				{ProviderID: tc.first, Model: "m"},
				{ProviderID: "other", Model: "m"},
			}, provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: RoleUser, Content: "hi"}}})
			if err != nil {
				t.Fatalf("ChatWithChain: %v", err)
			}
			if resp.Content != "from-winner" {
				t.Fatalf("Content = %q, want from-winner", resp.Content)
			}
			if len(trace.Attempts) != 2 || trace.Attempts[0].Outcome != "rate_limited" || trace.Attempts[1].Outcome != "success" {
				t.Errorf("trace = %+v, want rate_limited then success", trace.Attempts)
			}
		})
	}
}

func TestEngineCrossWireFailoverCohereToOpenAI(t *testing.T) {
	// Same shape as the Gemini cross-wire test, but with a REAL
	// Cohere client as the failing attempt: its 429 must map to
	// *provider.RateLimitError and the engine must fail over to an
	// OpenAI provider — and the reverse direction must work too.
	// This proves the failover decision is driven by the error type,
	// not the wire format.
	for _, tc := range []struct {
		name  string
		first string
	}{
		{"cohere-429-then-openai", "coh"},
		{"openai-429-then-cohere", "oai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clients map[string]provider.Client
			if tc.first == "coh" {
				// The Cohere client fails with a real 429; the
				// OpenAI fake wins.
				cohereSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v2/chat" {
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = w.Write([]byte(`{"message":"Too many requests"}`))
						return
					}
					w.WriteHeader(http.StatusNotFound)
				}))
				defer cohereSrv.Close()
				coClient, err := provider.NewClient(cohereSrv.URL, "co-key",
					provider.WithAuthHeaders(map[string]string{"Authorization": "Bearer co-key"}),
					provider.WithWireFormat(provider.WireFormatCohere))
				if err != nil {
					t.Fatalf("NewClient cohere: %v", err)
				}
				clients = map[string]provider.Client{"coh": coClient, "other": okClient("from-winner")}
			} else {
				// The OpenAI fake fails with a 429; a real Cohere
				// client answers the second attempt.
				cohereSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(`{"id":"chat-1","finish_reason":"COMPLETE","message":{"role":"assistant","content":[{"type":"text","text":"from-winner"}]},"usage":{"input_tokens":1,"output_tokens":1}}`))
				}))
				defer cohereSrv.Close()
				coClient, err := provider.NewClient(cohereSrv.URL, "co-key",
					provider.WithAuthHeaders(map[string]string{"Authorization": "Bearer co-key"}),
					provider.WithWireFormat(provider.WireFormatCohere))
				if err != nil {
					t.Fatalf("NewClient cohere: %v", err)
				}
				clients = map[string]provider.Client{"oai": errClient(&provider.RateLimitError{}), "other": coClient}
			}
			eng := NewEngine(clients, nil, testConfig())
			resp, trace, err := eng.ChatWithChain(context.Background(), []Attempt{
				{ProviderID: tc.first, Model: "m"},
				{ProviderID: "other", Model: "m"},
			}, provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: RoleUser, Content: "hi"}}})
			if err != nil {
				t.Fatalf("ChatWithChain: %v", err)
			}
			if resp.Content != "from-winner" {
				t.Fatalf("Content = %q, want from-winner", resp.Content)
			}
			if len(trace.Attempts) != 2 || trace.Attempts[0].Outcome != "rate_limited" || trace.Attempts[1].Outcome != "success" {
				t.Errorf("trace = %+v, want rate_limited then success", trace.Attempts)
			}
		})
	}
}

func TestEngineCrossWireFailoverBedrockToOpenAI(t *testing.T) {
	// Same shape as the Gemini/Cohere cross-wire tests, but with a
	// REAL Bedrock client as the failing attempt: its Throttling
	// 429 must map to *provider.RateLimitError and the engine must
	// fail over to an OpenAI provider — and the reverse direction
	// must work too. This proves the failover decision is driven
	// by the error type, not the wire format.
	for _, tc := range []struct {
		name  string
		first string
	}{
		{"bedrock-429-then-openai", "brk"},
		{"openai-429-then-bedrock", "oai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clients map[string]provider.Client
			if tc.first == "brk" {
				// The Bedrock client fails with a real
				// ThrottlingException 429; the OpenAI fake wins.
				brkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/model/m/converse" {
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = w.Write([]byte(`{"__type":"ThrottlingException","message":"Too many requests"}`))
						return
					}
					w.WriteHeader(http.StatusNotFound)
				}))
				defer brkSrv.Close()
				brkClient, err := provider.NewClient(brkSrv.URL, "AKIDEXAMPLE:SECRET",
					provider.WithWireFormat(provider.WireFormatBedrock))
				if err != nil {
					t.Fatalf("NewClient bedrock: %v", err)
				}
				clients = map[string]provider.Client{"brk": brkClient, "other": okClient("from-winner")}
			} else {
				// The OpenAI fake fails with a 429; a real
				// Bedrock client answers the second attempt.
				brkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(`{"output":{"message":{"role":"assistant","content":[{"text":"from-winner"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2}}`))
				}))
				defer brkSrv.Close()
				brkClient, err := provider.NewClient(brkSrv.URL, "AKIDEXAMPLE:SECRET",
					provider.WithWireFormat(provider.WireFormatBedrock))
				if err != nil {
					t.Fatalf("NewClient bedrock: %v", err)
				}
				clients = map[string]provider.Client{"oai": errClient(&provider.RateLimitError{}), "other": brkClient}
			}
			eng := NewEngine(clients, nil, testConfig())
			resp, trace, err := eng.ChatWithChain(context.Background(), []Attempt{
				{ProviderID: tc.first, Model: "m"},
				{ProviderID: "other", Model: "m"},
			}, provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: RoleUser, Content: "hi"}}})
			if err != nil {
				t.Fatalf("ChatWithChain: %v", err)
			}
			if resp.Content != "from-winner" {
				t.Fatalf("Content = %q, want from-winner", resp.Content)
			}
			if len(trace.Attempts) != 2 || trace.Attempts[0].Outcome != "rate_limited" || trace.Attempts[1].Outcome != "success" {
				t.Errorf("trace = %+v, want rate_limited then success", trace.Attempts)
			}
		})
	}
}

func TestEngineChatStreamChunksToolCalls(t *testing.T) {
	client := &chunkFakeClient{chunks: []provider.StreamChunk{
		{Content: "hi "},
		{ToolCalls: []provider.ChunkToolCall{{Index: 0, ID: "call_1", Name: "get_weather"}}},
		{ToolCalls: []provider.ChunkToolCall{{Index: 0, Arguments: `{"city":"Pune"}`}}},
		{FinishReason: "tool_calls"},
	}}
	eng := NewEngine(map[string]provider.Client{"a": client}, nil, testConfig())
	var got []provider.StreamChunk
	resp, err := eng.ChatStreamChunks(context.Background(), provider.ChatRequest{
		Model: "m", Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}, func(chunk provider.StreamChunk) error {
		got = append(got, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if resp.Content != "hi " {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Arguments != `{"city":"Pune"}` {
		t.Errorf("ToolCalls = %+v", resp.ToolCalls)
	}
	if len(got) != 4 {
		t.Errorf("got %d chunks, want 4 (text, tool-call head, args, finish)", len(got))
	}
}

// ---------------------------------------------------------------------------
// Auto-router ("auto/*" virtual models).
// ---------------------------------------------------------------------------

func newAutoTestEngine(t *testing.T) (*Engine, *fakeClient, *fakeClient) {
	t.Helper()
	ar := autorouter.New(autorouter.Config{
		DefaultModel: "strong-model",
		TierModels: map[autorouter.Tier][]string{
			autorouter.TierCoding:    {"coder-model"},
			autorouter.TierChat:      {"chat-model"},
			autorouter.TierReasoning: {"reasoner-model"},
			autorouter.TierQuickQA:   {"qa-model"},
		},
	})
	coder := &fakeClient{}
	chatter := &fakeClient{}
	clients := map[string]provider.Client{"prov_coder": coder, "prov_chat": chatter}
	// ChainFor serves the requested model from whichever provider
	// owns it; the auto-router decides the model, the chain decides
	// the provider.
	chainFor := func(model string) []Attempt {
		switch model {
		case "coder-model":
			return []Attempt{{ProviderID: "prov_coder", Model: model}}
		case "chat-model", "qa-model", "reasoner-model", "strong-model":
			return []Attempt{{ProviderID: "prov_chat", Model: model}}
		}
		return nil
	}
	eng := NewEngine(clients, nil, Config{
		ChainFor:  chainFor,
		AutoRoute: ar.Resolve,
	})
	return eng, coder, chatter
}

func TestEngineAutoRoutesCodingPromptToCoderModel(t *testing.T) {
	eng, coder, chatter := newAutoTestEngine(t)
	resp, err := eng.Chat(context.Background(), provider.ChatRequest{
		Model:    "auto/coding",
		Messages: []provider.Message{{Role: "user", Content: "Write a Python function to reverse a string"}},
	})
	if err != nil {
		t.Fatalf("Chat(auto/coding) error: %v", err)
	}
	if resp == nil {
		t.Fatal("Chat(auto/coding) returned nil response")
	}
	if coder.chats != 1 {
		t.Errorf("coder client chats = %d; want 1 (coding prompt must route to coder-model)", coder.chats)
	}
	if chatter.chats != 0 {
		t.Errorf("chat client chats = %d; want 0", chatter.chats)
	}
	if got := coder.reqs[0].Model; got != "coder-model" {
		t.Errorf("coder request model = %q; want coder-model", got)
	}
	dec, ok := eng.LastAutoDecision()
	if !ok {
		t.Fatal("LastAutoDecision: no decision after auto-routed request")
	}
	if dec.Tier != autorouter.TierCoding {
		t.Errorf("decision tier = %s; want coding", dec.Tier)
	}
	if dec.Model != "coder-model" {
		t.Errorf("decision model = %q; want coder-model", dec.Model)
	}
	if dec.Fallback {
		t.Errorf("coding prompt unexpectedly fell back: %+v", dec)
	}
}

func TestEngineAutoRoutesChatPromptToChatModel(t *testing.T) {
	eng, coder, chatter := newAutoTestEngine(t)
	_, err := eng.Chat(context.Background(), provider.ChatRequest{
		Model:    "auto",
		Messages: []provider.Message{{Role: "user", Content: "Tell me a funny joke about cats"}},
	})
	if err != nil {
		t.Fatalf("Chat(auto) error: %v", err)
	}
	if coder.chats != 0 || chatter.chats != 1 {
		t.Errorf("chats coder=%d chat=%d; want 0 and 1", coder.chats, chatter.chats)
	}
	dec, ok := eng.LastAutoDecision()
	if !ok || dec.Tier != autorouter.TierChat {
		t.Errorf("decision = %+v, ok=%v; want tier chat", dec, ok)
	}
}

func TestEngineAutoFallbackPromptGoesToStrongModel(t *testing.T) {
	eng, _, chatter := newAutoTestEngine(t)
	_, err := eng.Chat(context.Background(), provider.ChatRequest{
		Model:    "auto",
		Messages: []provider.Message{{Role: "user", Content: "zxqv blorp wumpus flibberty"}},
	})
	if err != nil {
		t.Fatalf("Chat(auto) error: %v", err)
	}
	if chatter.chats != 1 {
		t.Errorf("chat client chats = %d; want 1 (fallback serves via strong-model)", chatter.chats)
	}
	if got := chatter.reqs[0].Model; got != "reasoner-model" {
		t.Errorf("fallback request model = %q; want reasoner-model", got)
	}
	dec, ok := eng.LastAutoDecision()
	if !ok || !dec.Fallback || dec.Tier != autorouter.TierReasoning {
		t.Errorf("decision = %+v, ok=%v; want fallback to reasoning", dec, ok)
	}
}

func TestEngineAutoStreamRoutesToo(t *testing.T) {
	eng, coder, _ := newAutoTestEngine(t)
	_, err := eng.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "auto/coding",
		Messages: []provider.Message{{Role: "user", Content: "Write unit tests for this function"}},
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream(auto/coding) error: %v", err)
	}
	if coder.streams != 1 {
		t.Errorf("coder streams = %d; want 1", coder.streams)
	}
}

func TestEngineAutoUnresolvableModelFailsHonestly(t *testing.T) {
	// No tier models and no default model: Resolve reports ok=false,
	// so the engine must NOT invent a model — the request fails with
	// an empty chain error.
	ar := autorouter.New(autorouter.Config{})
	eng := NewEngine(map[string]provider.Client{"p": &fakeClient{}}, nil, Config{
		ChainFor:  func(string) []Attempt { return nil },
		AutoRoute: ar.Resolve,
	})
	_, err := eng.Chat(context.Background(), provider.ChatRequest{
		Model:    "auto",
		Messages: []provider.Message{{Role: "user", Content: "Write a Python function"}},
	})
	if err == nil {
		t.Fatal("Chat(auto) with no configured model: got nil error; want an honest failure, never an invented model")
	}
	dec, ok := eng.LastAutoDecision()
	if !ok {
		t.Fatal("LastAutoDecision: no decision recorded for the failed auto request")
	}
	if dec.Model != "" {
		t.Errorf("failed decision model = %q; want empty", dec.Model)
	}
}

func TestEnginePlainModelClearsAutoDecision(t *testing.T) {
	eng, _, _ := newAutoTestEngine(t)
	_, err := eng.Chat(context.Background(), provider.ChatRequest{
		Model:    "auto",
		Messages: []provider.Message{{Role: "user", Content: "Tell me a joke"}},
	})
	if err != nil {
		t.Fatalf("Chat(auto) error: %v", err)
	}
	if _, ok := eng.LastAutoDecision(); !ok {
		t.Fatal("expected an auto decision after auto request")
	}
	_, err = eng.Chat(context.Background(), provider.ChatRequest{
		Model:    "chat-model",
		Messages: []provider.Message{{Role: "user", Content: "Tell me a joke"}},
	})
	if err != nil {
		t.Fatalf("Chat(chat-model) error: %v", err)
	}
	if dec, ok := eng.LastAutoDecision(); ok {
		t.Errorf("plain-model request left a stale auto decision: %+v", dec)
	}
}

// TestEngineAddClientRoutesWithoutRestart proves a client registered
// after construction routes immediately: with no clients Chat fails
// honestly, after AddClient the same model reaches the new client —
// the live provider-add path (POST /api/providers) depends on this.
func TestEngineAddClientRoutesWithoutRestart(t *testing.T) {
	fc := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "live"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{}, nil, Config{})
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	if _, err := eng.Chat(context.Background(), req); err == nil {
		t.Fatal("Chat with no clients: expected error, got nil")
	}
	eng.AddClient("p1", fc)
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat after AddClient: %v", err)
	}
	if resp.Content != "live" {
		t.Errorf("Chat content = %q, want live", resp.Content)
	}
	if got := fc.chatCalls(); got != 1 {
		t.Errorf("fake client chats = %d, want 1", got)
	}
	// Empty id / nil client are ignored, never a panic or a wipe.
	eng.AddClient("", fc)
	eng.AddClient("p2", nil)
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat after no-op AddClients: %v", err)
	}
	if got := fc.chatCalls(); got != 2 {
		t.Errorf("fake client chats = %d, want 2", got)
	}
}

// TestEngineAddClientConcurrentWithChat runs AddClient against live
// traffic: the clients map must never race (run with -race).
func TestEngineAddClientConcurrentWithChat(t *testing.T) {
	fc := &fakeClient{}
	eng := NewEngine(map[string]provider.Client{"p0": fc}, nil, Config{})
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			eng.AddClient("p-live", fc)
		}
	}()
	for i := 0; i < 50; i++ {
		_, _ = eng.Chat(context.Background(), req)
	}
	<-done
}

// TestRemoveClientDropsProviderFromChain is the delete counterpart
// of AddClient: a removed provider leaves the client map (so the
// next request's fallback chain never includes it), the other
// provider keeps routing, and removing an unknown or empty id is a
// no-op. In-flight requests are unaffected because Chat snapshots
// its chain at start — here the removal happens before the call, so
// the removed provider's fake sees zero chats.
func TestRemoveClientDropsProviderFromChain(t *testing.T) {
	a := &fakeClient{}
	b := &fakeClient{}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	// Unknown and empty removals are no-ops.
	eng.RemoveClient("no-such-provider")
	eng.RemoveClient("")
	if got := eng.clientIDs(); len(got) != 2 {
		t.Fatalf("clientIDs = %v after no-op removals, want [a b]", got)
	}
	eng.RemoveClient("a")
	if got := eng.clientIDs(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("clientIDs = %v after removing a, want [b]", got)
	}
	if _, ok := eng.clientFor("a"); ok {
		t.Errorf("clientFor(a) still succeeds after RemoveClient")
	}
	// A request now routes through b only.
	resp, err := eng.Chat(context.Background(), provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err != nil || resp == nil {
		t.Fatalf("Chat = %v, %v", resp, err)
	}
	if a.chatCalls() != 0 {
		t.Errorf("removed provider a got %d chats, want 0", a.chatCalls())
	}
	if b.chatCalls() != 1 {
		t.Errorf("remaining provider b got %d chats, want 1", b.chatCalls())
	}
	// Re-adding restores it (the live-add path).
	eng.AddClient("a", a)
	if got := eng.clientIDs(); len(got) != 2 {
		t.Fatalf("clientIDs = %v after re-adding a, want [a b]", got)
	}
}

// TestDeadlineBudgetStopsChain proves the deadline sub-budget
// (research-log entry 147): a combo whose per-combo time budget
// runs out stops cleanly with ErrDeadlineExceeded — it does not
// keep retrying into a worse latency, and the budget bounds the
// WHOLE chain, not each attempt.
func TestDeadlineBudgetStopsChain(t *testing.T) {
	// A client that respects ctx cancellation and would otherwise
	// take 200 ms — far past the 30 ms budget.
	slow := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return &provider.ChatResponse{Content: "too-late"}, nil
		}
	}}
	fast := okClient("from-fast")
	cfg := testConfig()
	cfg.DeadlineBudget = 30 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"slow": slow, "fast": fast}, nil, cfg)

	// One slow attempt: the budget cuts it short.
	start := time.Now()
	_, _, err := eng.ChatWithChain(context.Background(),
		[]Attempt{{ProviderID: "slow", Model: "m"}}, testReq())
	elapsed := time.Since(start)
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("slow attempt with budget: err = %v, want ErrDeadlineExceeded", err)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("slow attempt with budget took %v — the budget did not cut it short", elapsed)
	}

	// A chain [slow, fast]: the budget is per-combo, so after the
	// slow attempt burns it, the fast provider must not rescue
	// the request with a fresh budget — the same request still
	// fails cleanly and quickly.
	slow2 := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return &provider.ChatResponse{Content: "too-late"}, nil
		}
	}}
	fast2 := okClient("from-fast")
	eng2 := NewEngine(map[string]provider.Client{"slow": slow2, "fast": fast2}, nil, cfg)
	start = time.Now()
	resp, _, err := eng2.ChatWithChain(context.Background(),
		[]Attempt{{ProviderID: "slow", Model: "m"}, {ProviderID: "fast", Model: "m"}}, testReq())
	elapsed = time.Since(start)
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("chain with budget: err = %v, want ErrDeadlineExceeded", err)
	}
	if resp != nil {
		t.Fatalf("chain with budget returned a response %q — the fast provider must not rescue a spent budget", resp.Content)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("chain with budget took %v — the budget did not bound the whole chain", elapsed)
	}
	if fast2.chatCalls() != 0 {
		t.Fatalf("fast provider was called %d times after the budget was spent — the chain must stop, not retry", fast2.chatCalls())
	}
}

// TestDeadlineBudgetOffMeansNoCap proves the budget is opt-in:
// with DeadlineBudget 0 (the default) a slow-but-healthy provider
// still completes — the budget never caps a request the operator
// did not ask it to cap. (Caller cancellation is covered by
// TestDeadlineBudgetDoesNotMaskCallerCancellation in
// strategy_test.go.)
func TestDeadlineBudgetOffMeansNoCap(t *testing.T) {
	slow := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
			return &provider.ChatResponse{Content: "slow-but-ok"}, nil
		}
	}}
	cfg := testConfig() // DeadlineBudget 0 = off
	eng := NewEngine(map[string]provider.Client{"slow": slow}, nil, cfg)
	resp, _, err := eng.ChatWithChain(context.Background(),
		[]Attempt{{ProviderID: "slow", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("no budget: err = %v, want nil", err)
	}
	if resp.Content != "slow-but-ok" {
		t.Fatalf("no budget: content = %q, want slow-but-ok", resp.Content)
	}
}

// TestChatWithChainModelNotFoundFallback (CHECKLIST 121; research-log
// entries 151 + 183): a 404 — or a 400 whose body signals the model
// does not exist at that target — falls back to the next target in
// the chain, and the same request succeeds there. The trace carries
// the reason "model_not_found" on the dead attempt.
func TestChatWithChainModelNotFoundFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"dead-model-404", &provider.UpstreamError{StatusCode: 404, Body: `{"error":{"message":"model: gate-model not found","type":"not_found_error"}}`}},
		{"dead-model-400", &provider.UpstreamError{StatusCode: 400, Body: `{"error":{"message":"The model ` + "`gate-model`" + ` does not exist","type":"invalid_request_error","code":"model_not_found"}}`}},
		{"overloaded-529", &provider.UpstreamError{StatusCode: 529, Body: "overloaded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dead := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
				return nil, tc.err
			}}
			fallback := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
				return &provider.ChatResponse{Content: "fallback says hi"}, nil
			}}
			eng := NewEngine(map[string]provider.Client{"dead": dead, "fallback": fallback}, nil, testConfig())
			chain := []Attempt{{ProviderID: "dead", Model: "gate-model"}, {ProviderID: "fallback", Model: "gate-model"}}
			resp, trace, err := eng.ChatWithChain(context.Background(), chain, testReq())
			if err != nil {
				t.Fatalf("ChatWithChain: err = %v, want nil (same request must succeed via fallback)", err)
			}
			if resp.Content != "fallback says hi" {
				t.Fatalf("content = %q, want fallback's content", resp.Content)
			}
			if dead.chatCalls() != 1 || fallback.chatCalls() != 1 {
				t.Fatalf("calls: dead=%d fallback=%d, want 1 each", dead.chatCalls(), fallback.chatCalls())
			}
			if trace == nil || len(trace.Attempts) != 2 {
				t.Fatalf("trace = %+v, want 2 attempts", trace)
			}
			if trace.Attempts[0].Provider != "dead" || trace.Attempts[0].Outcome != "error" {
				t.Fatalf("trace attempt 0 = %+v, want dead/error", trace.Attempts[0])
			}
			if trace.Attempts[1].Provider != "fallback" || trace.Attempts[1].Outcome != "success" {
				t.Fatalf("trace attempt 1 = %+v, want fallback/success", trace.Attempts[1])
			}
		})
	}
}

// TestChatWithChainModelNotFoundTraceReason pins the fallback reason
// the dashboard renders: the dead attempt's trace reason is
// "model_not_found" (not a bare status code).
func TestChatWithChainModelNotFoundTraceReason(t *testing.T) {
	dead := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.UpstreamError{StatusCode: 404, Body: `{"error":{"message":"model not found","type":"not_found_error"}}`}
	}}
	fallback := &fakeClient{}
	eng := NewEngine(map[string]provider.Client{"dead": dead, "fallback": fallback}, nil, testConfig())
	_, trace, err := eng.ChatWithChain(context.Background(),
		[]Attempt{{ProviderID: "dead", Model: "m"}, {ProviderID: "fallback", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatWithChain: err = %v, want nil", err)
	}
	if trace.Attempts[0].Reason != provider.ReasonModelNotFound {
		t.Fatalf("trace reason = %q, want %q", trace.Attempts[0].Reason, provider.ReasonModelNotFound)
	}
	if trace.Attempts[1].Reason != "" {
		t.Fatalf("success attempt reason = %q, want empty", trace.Attempts[1].Reason)
	}
}

// TestChatWithChainMalformedStopsChain (CHECKLIST 121; entry 151):
// a malformed request — 400 with no model-not-found signal, 403,
// 422 — must NOT fall back. The fallback target is never dialled
// (its call count stays 0), the trace shows exactly one attempt
// with reason "request_invalid" (or "forbidden"), and the error is
// the upstream's, not a fallback's.
func TestChatWithChainMalformedStopsChain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantReason string
	}{
		{"malformed-400", &provider.UpstreamError{StatusCode: 400, Body: `{"error":{"message":"temperature must be <= 1","type":"invalid_request_error"}}`}, provider.ReasonRequestInvalid},
		{"forbidden-403", &provider.UpstreamError{StatusCode: 403, Body: `{"error":{"message":"nope"}}`}, provider.ReasonForbidden},
		{"unprocessable-422", &provider.UpstreamError{StatusCode: 422, Body: `{"error":{"message":"bad field"}}`}, provider.ReasonRequestInvalid},
		{"auth-401", &provider.UpstreamError{StatusCode: 401, Body: `{"error":{"message":"bad key"}}`}, provider.ReasonAuthFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
				return nil, tc.err
			}}
			fallback := &fakeClient{}
			eng := NewEngine(map[string]provider.Client{"bad": bad, "fallback": fallback}, nil, testConfig())
			chain := []Attempt{{ProviderID: "bad", Model: "m"}, {ProviderID: "fallback", Model: "m"}}
			_, trace, err := eng.ChatWithChain(context.Background(), chain, testReq())
			if err == nil {
				t.Fatal("ChatWithChain: err = nil, want the upstream error (no fallback for a terminal failure)")
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want the upstream error %v (no fallback must run)", err, tc.err)
			}
			if bad.chatCalls() != 1 {
				t.Fatalf("bad calls = %d, want 1", bad.chatCalls())
			}
			if fallback.chatCalls() != 0 {
				t.Fatalf("fallback calls = %d, want 0 — a malformed request must never be dialled on a second target", fallback.chatCalls())
			}
			if trace == nil || len(trace.Attempts) != 1 {
				t.Fatalf("trace = %+v, want exactly 1 attempt", trace)
			}
			if trace.Attempts[0].Reason != tc.wantReason {
				t.Fatalf("trace reason = %q, want %q", trace.Attempts[0].Reason, tc.wantReason)
			}
		})
	}
}

// TestChatWithChainModelNotFoundDoesNotTripBreaker pins the
// breaker carve-out: the provider answered fine (only the model is
// dead), so five consecutive dead-model failures must not open the
// provider's circuit breaker — the sixth request still tries the
// dead target first instead of skipping it as "skipped_breaker".
func TestChatWithChainModelNotFoundDoesNotTripBreaker(t *testing.T) {
	deadErr := &provider.UpstreamError{StatusCode: 404, Body: `{"error":{"message":"model not found","type":"not_found_error"}}`}
	dead := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, deadErr
	}}
	fallback := &fakeClient{}
	eng := NewEngine(map[string]provider.Client{"dead": dead, "fallback": fallback}, nil, testConfig())
	chain := []Attempt{{ProviderID: "dead", Model: "m"}, {ProviderID: "fallback", Model: "m"}}
	for i := 0; i < 5; i++ {
		if _, _, err := eng.ChatWithChain(context.Background(), chain, testReq()); err != nil {
			t.Fatalf("request %d: err = %v, want nil (fallback serves it)", i, err)
		}
	}
	// Sixth request: the dead target must still be attempted first
	// (breaker threshold is 5 — a tripped breaker would skip it).
	_, trace, err := eng.ChatWithChain(context.Background(), chain, testReq())
	if err != nil {
		t.Fatalf("request 6: err = %v, want nil", err)
	}
	if trace.Attempts[0].Provider != "dead" || trace.Attempts[0].Outcome == "skipped_breaker" {
		t.Fatalf("request 6 trace attempt 0 = %+v, want the dead target tried, not skipped by the breaker", trace.Attempts[0])
	}
}

// TestChatStreamTerminalStopsBeforeCommit (CHECKLIST 121): a
// streaming request whose first attempt fails terminally before any
// content is streamed must stop the chain — the fallback target is
// never started.
func TestChatStreamTerminalStopsBeforeCommit(t *testing.T) {
	bad := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		return nil, &provider.UpstreamError{StatusCode: 400, Body: `{"error":{"message":"temperature must be <= 1","type":"invalid_request_error"}}`}
	}}
	fallback := &fakeClient{}
	eng := NewEngine(map[string]provider.Client{"bad": bad, "fallback": fallback}, nil, testConfig())
	chain := []Attempt{{ProviderID: "bad", Model: "m"}, {ProviderID: "fallback", Model: "m"}}
	ch, err := eng.ChatStreamWithChain(context.Background(), chain, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: err = %v", err)
	}
	var streamErr error
	for ev := range ch {
		if ev.Err != nil {
			streamErr = ev.Err
		}
	}
	if streamErr == nil {
		t.Fatal("stream ended without an error, want the terminal 400")
	}
	if bad.streamCalls() != 1 {
		t.Fatalf("bad stream calls = %d, want 1", bad.streamCalls())
	}
	if fallback.streamCalls() != 0 {
		t.Fatalf("fallback stream calls = %d, want 0 — a terminal stream failure must not start the fallback", fallback.streamCalls())
	}
}

// ---------------------------------------------------------------------------
// Per-key ledger attribution (rolling 5-hour budgets, CHECKLIST 139):
// the engine must stamp every attempt row with the gateway key the
// request arrived under (ChatRequest.CallerID), so the ledger can
// account spend per key. A ledger that implements KeyedLedger gets
// RecordAttemptForKey; a plain Ledger keeps the old unkeyed call.
// ---------------------------------------------------------------------------

type keyedFakeLedger struct {
	fakeLedger
	keyed []keyedLedgerCall
}

type keyedLedgerCall struct {
	keyID string
	call  ledgerCall
}

func (l *keyedFakeLedger) RecordAttemptForKey(keyID, provider, model string, ok bool, latencyMs int64, usage provider.Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keyed = append(l.keyed, keyedLedgerCall{keyID: keyID, call: ledgerCall{provider: provider, model: model, ok: ok, latencyMs: latencyMs, usage: usage}})
}

func TestEngineLedgerRowsCarryCallerKeyID(t *testing.T) {
	led := &keyedFakeLedger{}
	dead := errClient(&provider.RateLimitError{RetryAfter: 0, Body: "slow down"})
	live := okClient("live says hi")
	eng := NewEngine(map[string]provider.Client{"dead": dead, "live": live}, led, testConfig())
	chain := []Attempt{{ProviderID: "dead", Model: "m"}, {ProviderID: "live", Model: "m"}}
	req := testReq()
	req.CallerID = "gk_testkey"
	req.Model = "m"
	resp, _, err := eng.ChatWithChain(context.Background(), chain, req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Content != "live says hi" {
		t.Fatalf("content = %q", resp.Content)
	}
	led.mu.Lock()
	defer led.mu.Unlock()
	// The dead attempt (failed, no usage) and the live attempt
	// (success) must BOTH carry the caller key — a failed
	// fallback attempt that lost its key would be invisible to
	// the budget, and a successful one unbilled to the key.
	if len(led.keyed) != 2 {
		t.Fatalf("keyed ledger calls = %d, want 2 (dead + live attempts)", len(led.keyed))
	}
	if len(led.calls) != 0 {
		t.Fatalf("unkeyed ledger calls = %d, want 0 — the keyed path must win", len(led.calls))
	}
	for i, c := range led.keyed {
		if c.keyID != "gk_testkey" {
			t.Errorf("call %d key = %q, want gk_testkey", i, c.keyID)
		}
	}
	if led.keyed[0].call.provider != "dead" || led.keyed[0].call.ok {
		t.Errorf("call 0 = %+v, want the failed dead attempt", led.keyed[0].call)
	}
	if led.keyed[1].call.provider != "live" || !led.keyed[1].call.ok {
		t.Errorf("call 1 = %+v, want the successful live attempt", led.keyed[1].call)
	}
}

func TestEngineUnkeyedLedgerKeepsOldBehaviour(t *testing.T) {
	// A ledger without KeyedLedger still records, with empty keys.
	led := &fakeLedger{}
	eng := NewEngine(map[string]provider.Client{"live": okClient("hi")}, led, testConfig())
	req := testReq()
	req.CallerID = "gk_testkey"
	req.Model = "m"
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "live", Model: "m"}}, req); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if led.count() != 1 {
		t.Fatalf("ledger calls = %d, want 1", led.count())
	}
}
