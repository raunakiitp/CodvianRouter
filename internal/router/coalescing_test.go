package router

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// sharedLedger is a fakeLedger that also records shared (coalesced)
// rows, so tests can prove waiters are logged as shared — zero
// tokens, zero cost — instead of as billed attempts.
type sharedLedger struct {
	fakeLedger
	mu     sync.Mutex
	shared []sharedCall
}

type sharedCall struct {
	provider  string
	model     string
	ok        bool
	latencyMs int64
}

func (l *sharedLedger) RecordShared(provider, model string, ok bool, latencyMs int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.shared = append(l.shared, sharedCall{provider: provider, model: model, ok: ok, latencyMs: latencyMs})
}

func (l *sharedLedger) sharedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.shared)
}

type fakeCoalescingMetrics struct {
	mu sync.Mutex
	n  int
}

func (m *fakeCoalescingMetrics) IncCoalescedShared() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
}

func (m *fakeCoalescingMetrics) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}

func coalesceReq(prompt string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: prompt}},
	}
}

// TestEngineCoalescesConcurrentIdentical is the core single-flight
// proof at the engine level: 10 concurrent identical non-streaming
// requests produce exactly 1 upstream call, all 10 get the result,
// 9 waiters are counted as shared (stats, metrics sink and ledger),
// and the ledger holds exactly 1 billed attempt plus 9 shared rows.
func TestEngineCoalescesConcurrentIdentical(t *testing.T) {
	led := &sharedLedger{}
	met := &fakeCoalescingMetrics{}
	fc := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		time.Sleep(200 * time.Millisecond)
		return &provider.ChatResponse{Content: "shared answer", Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"p1": fc}, led, Config{})
	eng.SetCoalescingMetrics(met)

	const n = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	resps := make([]*provider.ChatResponse, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resps[i], errs[i] = eng.Chat(context.Background(), coalesceReq("hello"))
		}(i)
	}
	close(start)
	wg.Wait()

	if got := fc.chatCalls(); got != 1 {
		t.Fatalf("upstream chat calls = %d, want exactly 1", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d got error: %v", i, errs[i])
		}
		if resps[i] == nil || resps[i].Content != "shared answer" {
			t.Fatalf("caller %d got wrong response: %+v", i, resps[i])
		}
	}
	stats := eng.CoalescingStats()
	if stats.Requests != n || stats.Flights != 1 || stats.Shared != n-1 {
		t.Fatalf("stats = %+v, want requests=%d flights=1 shared=%d", stats, n, n-1)
	}
	if stats.UpstreamAvoided != n-1 || stats.QuotaSaved != n-1 {
		t.Fatalf("stats = %+v, want upstream_avoided/quota_saved=%d", stats, n-1)
	}
	if got := met.count(); got != n-1 {
		t.Fatalf("metrics shared count = %d, want %d", got, n-1)
	}
	if got := led.count(); got != 1 {
		t.Fatalf("ledger billed attempts = %d, want 1 (the leader)", got)
	}
	if got := led.sharedCount(); got != n-1 {
		t.Fatalf("ledger shared rows = %d, want %d", got, n-1)
	}
	led.mu.Lock()
	for _, sc := range led.shared {
		if sc.provider != "p1" || sc.model != "m" || !sc.ok {
			t.Fatalf("shared row = %+v, want provider p1 model m ok=true", sc)
		}
	}
	led.mu.Unlock()
	// The shared result is the leader's trace: one attempt, success.
	if tr := eng.LastTrace(); tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Outcome != "success" {
		t.Fatalf("last trace = %+v, want 1 success attempt", tr)
	}
}

// TestEngineCoalescesSharedFailure: one upstream error reaches all
// waiters, the upstream is called once, and the waiters are recorded
// as shared rows with ok=false.
func TestEngineCoalescesSharedFailure(t *testing.T) {
	led := &sharedLedger{}
	want := errors.New("upstream down")
	fc := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		time.Sleep(150 * time.Millisecond)
		return nil, want
	}}
	eng := NewEngine(map[string]provider.Client{"p1": fc}, led, Config{MaxAttempts: 1})

	const n = 5
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = eng.Chat(context.Background(), coalesceReq("boom"))
		}(i)
	}
	close(start)
	wg.Wait()

	if got := fc.chatCalls(); got != 1 {
		t.Fatalf("upstream chat calls = %d, want exactly 1", got)
	}
	for i, err := range errs {
		if !errors.Is(err, want) {
			t.Fatalf("caller %d got error %v, want %v", i, err, want)
		}
	}
	if got := eng.CoalescingStats().Shared; got != n-1 {
		t.Fatalf("shared = %d, want %d", got, n-1)
	}
	if got := led.sharedCount(); got != n-1 {
		t.Fatalf("ledger shared rows = %d, want %d", got, n-1)
	}
	led.mu.Lock()
	for _, sc := range led.shared {
		if sc.ok {
			t.Fatalf("shared row = %+v, want ok=false for a shared failure", sc)
		}
	}
	led.mu.Unlock()
}

// TestEngineCoalescingSharesOneBackoffSequence: the leader 429s once
// and then succeeds; 5 concurrent identical requests together make
// exactly 2 upstream calls (one 429 + one success) — the waiters
// share the leader's single backoff sequence instead of each
// retrying on their own.
func TestEngineCoalescingSharesOneBackoffSequence(t *testing.T) {
	var calls int32
	fc := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, &provider.RateLimitError{}
		}
		return &provider.ChatResponse{Content: "after retry"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"p1": fc}, nil, Config{})

	const n = 5
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := eng.Chat(context.Background(), coalesceReq("retry me"))
			if err == nil && (resp == nil || resp.Content != "after retry") {
				errs[i] = errors.New("wrong response")
			} else {
				errs[i] = err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d got error: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream chat calls = %d, want 2 (one 429 + one success)", got)
	}
	if got := eng.CoalescingStats().Shared; got != n-1 {
		t.Fatalf("shared = %d, want %d", got, n-1)
	}
}

func TestEngineDifferentRequestsNotCoalesced(t *testing.T) {
	fc := &fakeClient{}
	eng := NewEngine(map[string]provider.Client{"p1": fc}, nil, Config{})
	var wg sync.WaitGroup
	for _, prompt := range []string{"one", "two"} {
		wg.Add(1)
		go func(prompt string) {
			defer wg.Done()
			if _, err := eng.Chat(context.Background(), coalesceReq(prompt)); err != nil {
				t.Errorf("Chat(%q): %v", prompt, err)
			}
		}(prompt)
	}
	wg.Wait()
	if got := fc.chatCalls(); got != 2 {
		t.Fatalf("upstream chat calls = %d, want 2", got)
	}
	if got := eng.CoalescingStats().Shared; got != 0 {
		t.Fatalf("shared = %d, want 0", got)
	}
}

func TestEngineToolRequestsNotCoalesced(t *testing.T) {
	fc := &fakeClient{}
	eng := NewEngine(map[string]provider.Client{"p1": fc}, nil, Config{})
	req := coalesceReq("call a tool")
	req.Tools = []provider.ToolDefinition{{Type: "function"}}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := eng.Chat(context.Background(), req); err != nil {
				t.Errorf("Chat: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := fc.chatCalls(); got != 2 {
		t.Fatalf("upstream chat calls = %d, want 2 (tool requests never coalesce)", got)
	}
	if got := eng.CoalescingStats().Requests; got != 0 {
		t.Fatalf("coalescing requests = %d, want 0 (tool requests never enter the group)", got)
	}
}

// TestEngineStreamingNotCoalesced: two concurrent identical streams
// each get their own upstream stream call.
func TestEngineStreamingNotCoalesced(t *testing.T) {
	fc := &fakeClient{}
	eng := NewEngine(map[string]provider.Client{"p1": fc}, nil, Config{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := eng.ChatStream(context.Background(), coalesceReq("stream me"), func(string) error { return nil }); err != nil {
				t.Errorf("ChatStream: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := fc.streamCalls(); got != 2 {
		t.Fatalf("upstream stream calls = %d, want 2", got)
	}
	if got := eng.CoalescingStats().Requests; got != 0 {
		t.Fatalf("coalescing requests = %d, want 0 (streams never enter the group)", got)
	}
}

// TestEngineCoalescingWaiterContextCancel: a waiter whose own
// context ends leaves with its context error and is not counted as
// shared; the leader still completes and the upstream is called once.
func TestEngineCoalescingWaiterContextCancel(t *testing.T) {
	fc := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		time.Sleep(400 * time.Millisecond)
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"p1": fc}, nil, Config{})

	leaderDone := make(chan error, 1)
	go func() {
		_, err := eng.Chat(context.Background(), coalesceReq("slow"))
		leaderDone <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the leader register
	wctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := eng.Chat(wctx, coalesceReq("slow"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter got %v, want context deadline", err)
	}
	select {
	case err := <-leaderDone:
		if err != nil {
			t.Fatalf("leader got error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("leader never finished")
	}
	if got := fc.chatCalls(); got != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", got)
	}
	if stats := eng.CoalescingStats(); stats.Shared != 0 || stats.Requests != 2 {
		t.Fatalf("stats = %+v, want shared=0 requests=2", stats)
	}
}

// TestEngineCoalescingUnkeyableRequestRunsUncoalesced: a NaN
// temperature cannot be JSON-encoded for the key, so the request
// runs (and succeeds) without coalescing instead of failing.
func TestEngineCoalescingUnkeyableRequestRunsUncoalesced(t *testing.T) {
	fc := &fakeClient{}
	eng := NewEngine(map[string]provider.Client{"p1": fc}, nil, Config{})
	req := coalesceReq("nan temp")
	req.Temperature = math.NaN() // json.Marshal fails on NaN
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("resp = %+v, want ok", resp)
	}
	if got := eng.CoalescingStats().Requests; got != 0 {
		t.Fatalf("coalescing requests = %d, want 0", got)
	}
}
