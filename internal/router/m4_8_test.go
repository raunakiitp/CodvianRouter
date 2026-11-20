// M4.8 — hedge body-size guard, attempt-cap clamp, exhausted
// skip, stream early-retry, wait budgets, preferred failover.
package router

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// bigReq builds a request whose body exceeds the 256 KiB hedge guard.
func bigReq() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: strings.Repeat("x", 300<<10)}},
	}
}

// TestHedgeBodySizeGuard: a >256 KiB body is never hedged — the
// slow first pick serves alone, the second target is never dialled,
// and the trace says disabled-large-body.
func TestHedgeBodySizeGuard(t *testing.T) {
	ctx := context.Background()
	a := delayClient(300*time.Millisecond, "slow")
	b := okClient("fast")
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a", "b"), bigReq())
	if err != nil || resp.Content != "slow" {
		t.Fatalf("chat = %v, %v, want slow + nil (large body: no hedge, first pick serves)", resp, err)
	}
	if got := b.chatCalls(); got != 0 {
		t.Fatalf("provider B called %d times, want 0 (hedge never launched for a >256 KiB body)", got)
	}
	if trace.Hedge != "disabled-large-body" {
		t.Fatalf("trace.Hedge = %q, want disabled-large-body", trace.Hedge)
	}
}

// TestHedgeBodySizeGuardBoundary: a body at exactly 256 KiB still
// hedges; one byte over does not.
func TestHedgeBodySizeGuardBoundary(t *testing.T) {
	if got := requestBodyBytes(provider.ChatRequest{Messages: []provider.Message{{Role: "user", Content: strings.Repeat("x", 256<<10)}}}); got != 256<<10 {
		t.Fatalf("requestBodyBytes at the boundary = %d, want %d", got, 256<<10)
	}
	ctx := context.Background()
	a := delayClient(300*time.Millisecond, "slow")
	b := okClient("fast")
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	atBoundary := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: strings.Repeat("x", 256<<10)}}}
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), atBoundary)
	if err != nil || resp.Content != "fast" {
		t.Fatalf("boundary chat = %v, %v, want fast + nil (exactly 256 KiB still hedges)", resp, err)
	}
	if got := b.chatCalls(); got != 1 {
		t.Fatalf("provider B called %d times at the boundary, want 1 (hedge fired)", got)
	}
}

// TestHedgeBodySizeGuardCountsParts: tool-call arguments and image
// payloads count toward the body — a small text body with a huge
// tool-call argument is still over the guard.
func TestHedgeBodySizeGuardCountsParts(t *testing.T) {
	req := provider.ChatRequest{Messages: []provider.Message{{
		Role:    "user",
		Content: "hi",
		ToolCalls: []provider.ToolCall{{
			ID:       "call_1",
			Function: provider.ToolCallFunction{Name: "f", Arguments: strings.Repeat("y", 300<<10)},
		}},
	}}}
	if got := requestBodyBytes(req); got <= hedgeMaxBodyBytes {
		t.Fatalf("requestBodyBytes with a 300 KiB tool-call argument = %d, want > %d", got, hedgeMaxBodyBytes)
	}
}

// TestHedgeBodySizeGuardCountsTools: tool DEFINITIONS count toward
// the body — a small prompt with a huge tool list is still over the
// guard and never hedges; a tool list under the guard still hedges.
// Toggle: if requestBodyBytes ignored Tools, the over-guard request
// would hedge (provider B called once, "fast" wins) and this fails.
func TestHedgeBodySizeGuardCountsTools(t *testing.T) {
	// Unit level: the byte count itself.
	big := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	tool := provider.ToolDefinition{Type: "function"}
	tool.Function.Name = "f"
	tool.Function.Parameters = json.RawMessage(strings.Repeat("y", 300<<10))
	big.Tools = []provider.ToolDefinition{tool}
	if got := requestBodyBytes(big); got <= hedgeMaxBodyBytes {
		t.Fatalf("requestBodyBytes with a 300 KiB tool definition = %d, want > %d", got, hedgeMaxBodyBytes)
	}

	// End to end, over the guard: never hedged.
	ctx := context.Background()
	a := delayClient(300*time.Millisecond, "slow")
	b := okClient("fast")
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a", "b"), big)
	if err != nil || resp.Content != "slow" {
		t.Fatalf("chat = %v, %v, want slow + nil (huge tool list: no hedge, first pick serves)", resp, err)
	}
	if got := b.chatCalls(); got != 0 {
		t.Fatalf("provider B called %d times, want 0 (hedge never launched for a >256 KiB tool list)", got)
	}
	if trace.Hedge != "disabled-large-body" {
		t.Fatalf("trace.Hedge = %q, want disabled-large-body", trace.Hedge)
	}

	// End to end, under the guard: still hedges.
	small := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	tool2 := provider.ToolDefinition{Type: "function"}
	tool2.Function.Name = "f"
	tool2.Function.Parameters = json.RawMessage(strings.Repeat("y", 100<<10))
	small.Tools = []provider.ToolDefinition{tool2}
	b2 := okClient("fast")
	eng2 := NewEngine(map[string]provider.Client{"a": delayClient(300*time.Millisecond, "slow"), "b": b2}, nil, cfg)
	resp, _, err = eng2.ChatWithChain(ctx, chainFor("a", "b"), small)
	if err != nil || resp.Content != "fast" {
		t.Fatalf("chat = %v, %v, want fast + nil (100 KiB tool list still hedges)", resp, err)
	}
	if got := b2.chatCalls(); got != 1 {
		t.Fatalf("provider B called %d times, want 1 (hedge fired under the guard)", got)
	}
}

// TestMaxAttemptsClamp: withDefaults clamps MaxAttempts to [1, 200]
// — 0 becomes the default 5, 500 becomes 200, an explicit 3 stays 3.
func TestMaxAttemptsClamp(t *testing.T) {
	if got := (Config{}).withDefaults().MaxAttempts; got != 5 {
		t.Fatalf("MaxAttempts 0 -> %d, want 5 (default)", got)
	}
	if got := (Config{MaxAttempts: 500}).withDefaults().MaxAttempts; got != 200 {
		t.Fatalf("MaxAttempts 500 -> %d, want 200 (clamped)", got)
	}
	if got := (Config{MaxAttempts: 3}).withDefaults().MaxAttempts; got != 3 {
		t.Fatalf("MaxAttempts 3 -> %d, want 3 (in range, untouched)", got)
	}
	if got := (Config{MaxAttempts: 200}).withDefaults().MaxAttempts; got != 200 {
		t.Fatalf("MaxAttempts 200 -> %d, want 200 (the cap itself)", got)
	}
}

// TestStreamEarlyRetryReopens: a stream that truncates twice (a
// transport blip, pre-commit) is transparently re-opened on the
// SAME target — the client gets a full answer, the sibling is never
// dialled.
func TestStreamEarlyRetryReopens(t *testing.T) {
	ctx := context.Background()
	calls := 0
	a := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		calls++
		if calls <= 2 {
			return nil, errTransportBlip
		}
		if onDelta != nil {
			if err := onDelta("live"); err != nil {
				return nil, err
			}
		}
		return &provider.ChatResponse{Content: "live"}, nil
	}}
	b := streamClient([]string{"bee"}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		got += ev.Delta
	}
	if got != "live" {
		t.Fatalf("stream content = %q, want %q", got, "live")
	}
	if calls != 3 {
		t.Fatalf("provider A streamed %d times, want 3 (initial + 2 transparent re-opens)", calls)
	}
	if b.streamCalls() != 0 {
		t.Fatalf("sibling streamed %d times, want 0 (no failover — the same target recovered)", b.streamCalls())
	}
}

var errTransportBlip = errBlip()

func errBlip() error { return errorString("read: connection reset by peer") }

type errorString string

func (e errorString) Error() string { return string(e) }

// TestStreamEarlyRetryMaxFour: a stream that always truncates is
// re-opened exactly 4 times (5 attempts on the same target), then
// the chain fails over to the healthy sibling.
func TestStreamEarlyRetryMaxFour(t *testing.T) {
	ctx := context.Background()
	a := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		return nil, errTransportBlip
	}}
	b := streamClient([]string{"bee"}, nil)
	cfg := testConfig()
	cfg.MaxAttempts = 10 // the 5 re-open attempts must fit the budget
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		got += ev.Delta
	}
	if got != "bee" {
		t.Fatalf("stream content = %q, want %q (failover after the re-opens ran out)", got, "bee")
	}
	if a.streamCalls() != 5 {
		t.Fatalf("provider A streamed %d times, want 5 (initial + 4 re-opens, then failover)", a.streamCalls())
	}
}

// TestStreamEarlyRetrySkips429: a 429 at stream start does NOT get
// an early re-open — the 429 path owns it (failover here, since a
// sibling exists).
func TestStreamEarlyRetrySkips429(t *testing.T) {
	ctx := context.Background()
	a := streamClient(nil, &provider.RateLimitError{Body: "slow down"})
	b := streamClient([]string{"bee"}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		got += ev.Delta
	}
	if got != "bee" {
		t.Fatalf("stream content = %q, want %q", got, "bee")
	}
	if a.streamCalls() != 1 {
		t.Fatalf("provider A streamed %d times, want 1 (a 429 never re-opens)", a.streamCalls())
	}
}

// TestStreamEarlyRetryEmptyStream: an empty stream (no content
// before close) is a truncation — re-opened up to 4 times, then
// failover.
func TestStreamEarlyRetryEmptyStream(t *testing.T) {
	ctx := context.Background()
	calls := 0
	a := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		calls++
		if calls <= 2 {
			return &provider.ChatResponse{Content: ""}, nil
		}
		if onDelta != nil {
			if err := onDelta("live"); err != nil {
				return nil, err
			}
		}
		return &provider.ChatResponse{Content: "live"}, nil
	}}
	b := streamClient([]string{"bee"}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		got += ev.Delta
	}
	if got != "live" {
		t.Fatalf("stream content = %q, want %q", got, "live")
	}
	if calls != 3 {
		t.Fatalf("provider A streamed %d times, want 3 (initial + 2 re-opens of the empty stream)", calls)
	}
}

// breaker on the first attempt (threshold 1) — its second key is
// skipped for the rest of THIS request (skipped_breaker), and B
// serves. The exhausted provider is never re-dialled mid-request.
func TestExhaustedProviderSkippedForRestOfRequest(t *testing.T) {
	ctx := context.Background()
	k1 := errClient(&provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"})
	k2 := okClient("from-k2")
	b := okClient("bee")
	clients := map[string]provider.Client{
		ClientKey("a", "key_1"): k1,
		ClientKey("a", "key_2"): k2,
		"b":                     b,
	}
	cfg := testConfig()
	cfg.BreakerThreshold = 1
	eng := NewEngine(clients, nil, cfg)
	chain := []Attempt{
		{ProviderID: "a", Model: "m", KeyID: "key_1"},
		{ProviderID: "a", Model: "m", KeyID: "key_2"},
		{ProviderID: "b", Model: "m"},
	}
	resp, trace, err := eng.ChatWithChain(ctx, chain, testReq())
	if err != nil || resp.Content != "bee" {
		t.Fatalf("chat = %v, %v, want bee + nil (exhausted A skipped, B serves)", resp, err)
	}
	if got := k2.chatCalls(); got != 0 {
		t.Fatalf("exhausted provider's second key called %d times, want 0 (skip for the rest of the request)", got)
	}
	found := false
	for _, att := range trace.Attempts {
		if att.Provider == "a" && att.Outcome == "skipped_breaker" {
			found = true
		}
	}
	if !found {
		t.Fatalf("trace has no skipped_breaker for A (attempts: %+v)", trace.Attempts)
	}
}

// TestWaitBudgetWaitsInsteadOfFailingOver: with wait budgets ON, a
// plain 429 (no hint) is waited out on the SAME target — the
// sibling is never dialled. Toggle: budgets OFF fails over at once.
func TestWaitBudgetWaitsInsteadOfFailingOver(t *testing.T) {
	ctx := context.Background()
	calls := 0
	a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls <= 2 {
			return nil, &provider.RateLimitError{Body: "slow down"}
		}
		return &provider.ChatResponse{Content: "a-ok"}, nil
	}}
	b := okClient("bee")
	cfg := testConfig()
	cfg.WaitBudgets = WaitBudgetConfig{Enabled: true}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "a-ok" {
		t.Fatalf("chat = %v, %v, want a-ok + nil (budget waited the 429 out on A)", resp, err)
	}
	if calls != 3 {
		t.Fatalf("provider A called %d times, want 3 (initial + 2 waited retries)", calls)
	}
	if got := b.chatCalls(); got != 0 {
		t.Fatalf("provider B called %d times, want 0 (no failover while the budget waits)", got)
	}
	// Budgets OFF: the same 429 fails over immediately.
	calls = 0
	engOff := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, _, err = engOff.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("budgets-off chat = %v, %v, want bee + nil (fail over at once)", resp, err)
	}
	if calls != 1 {
		t.Fatalf("budgets-off: provider A called %d times, want 1 (no wait, immediate failover)", calls)
	}
}

// TestWaitBudgetHonoursHint: a 429 with a 7 s hint is waited out
// (floored to 10 s) on the same target instead of failing over.
// Slow by construction (the 10 s floor) — the one test that proves
// the hint path end to end.
func TestWaitBudgetHonoursHint(t *testing.T) {
	ctx := context.Background()
	calls := 0
	a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			return nil, &provider.RateLimitError{RetryAfter: 7 * time.Second, RetryAfterPresent: true, Body: "slow down"}
		}
		return &provider.ChatResponse{Content: "a-ok"}, nil
	}}
	b := okClient("bee")
	cfg := testConfig()
	cfg.WaitBudgets = WaitBudgetConfig{Enabled: true}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	start := time.Now()
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	elapsed := time.Since(start)
	if err != nil || resp.Content != "a-ok" {
		t.Fatalf("chat = %v, %v, want a-ok + nil (hint waited out on A)", resp, err)
	}
	if elapsed < 10*time.Second {
		t.Fatalf("request took %v, want >= 10s (the 7s hint floored to 10s was actually slept)", elapsed)
	}
	if calls != 2 {
		t.Fatalf("provider A called %d times, want 2 (initial + 1 waited retry)", calls)
	}
	if got := b.chatCalls(); got != 0 {
		t.Fatalf("provider B called %d times, want 0", got)
	}
}

// TestWaitBudgetNeverOnQuota: a quota-text 429 is never waited out,
// budgets or not — it fails over immediately.
func TestWaitBudgetNeverOnQuota(t *testing.T) {
	ctx := context.Background()
	a := errClient(&provider.RateLimitError{RetryAfter: 5 * time.Second, RetryAfterPresent: true, Body: "insufficient_quota: you have no credits left"})
	b := okClient("bee")
	cfg := testConfig()
	cfg.WaitBudgets = WaitBudgetConfig{Enabled: true}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	start := time.Now()
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("chat = %v, %v, want bee + nil (quota 429 fails over, never waits)", resp, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("quota request took %v, want < 2s (no wait was slept)", elapsed)
	}
	if got := a.chatCalls(); got != 1 {
		t.Fatalf("provider A called %d times, want 1", got)
	}
}

// TestWaitBudgetWaitUnit: the budget helper's caps directly —
// per-wait cap, retry count, total budget, quota/concurrency
// exclusions, disabled default.
func TestWaitBudgetWaitUnit(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": okClient("x")}, nil, Config{WaitBudgets: WaitBudgetConfig{Enabled: true}})
	plain := &provider.RateLimitError{RetryAfter: 20 * time.Second, RetryAfterPresent: true, Body: "slow down"}
	if w, ok := eng.waitBudgetWait(plain, 20*time.Second, 0, 0); !ok || w != 20*time.Second {
		t.Fatalf("plain 20s hint: wait=%v ok=%v, want 20s/true", w, ok)
	}
	huge := &provider.RateLimitError{RetryAfter: 120 * time.Second, RetryAfterPresent: true, Body: "slow down"}
	if _, ok := eng.waitBudgetWait(huge, 0, 0, 0); ok {
		t.Fatalf("120s hint: budget accepted it, want decline (above the 90s per-wait cap)")
	}
	if _, ok := eng.waitBudgetWait(plain, 20*time.Second, 5, 0); ok {
		t.Fatalf("5 waits used: budget accepted a 6th, want decline (max 5 retries)")
	}
	if _, ok := eng.waitBudgetWait(plain, 20*time.Second, 0, 290*time.Second); ok {
		t.Fatalf("290s already waited: budget accepted 20s more, want decline (300s total)")
	}
	quota := &provider.RateLimitError{RetryAfter: 5 * time.Second, RetryAfterPresent: true, Body: "insufficient_quota: no credits"}
	if _, ok := eng.waitBudgetWait(quota, 5*time.Second, 0, 0); ok {
		t.Fatalf("quota 429: budget accepted it, want decline (never on quota)")
	}
	conc := &provider.RateLimitError{Body: "concurrency limit reached"}
	if _, ok := eng.waitBudgetWait(conc, time.Second, 0, 0); ok {
		t.Fatalf("concurrency 429: budget accepted it, want decline (fail over now)")
	}
	off := NewEngine(map[string]provider.Client{"a": okClient("x")}, nil, Config{})
	if _, ok := off.waitBudgetWait(plain, 20*time.Second, 0, 0); ok {
		t.Fatalf("budgets disabled: helper accepted a wait, want decline (default off)")
	}
}

// TestPreferredFailoverRotatesToSibling: the preferred account's
// stream never starts — after the 200 ms response-start deadline
// the chain rotates to the ONE sibling account, and the client gets
// a full answer in well under a second (not after the 65 s
// preread+watchdog bound).
func TestPreferredFailoverRotatesToSibling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k1 := stallStreamClient("")
	k2 := streamClient([]string{"bee"}, nil)
	clients := map[string]provider.Client{
		ClientKey("a", "key_1"): k1,
		ClientKey("a", "key_2"): k2,
	}
	cfg := testConfig()
	cfg.PreferredFailoverAfter = 200 * time.Millisecond
	eng := NewEngine(clients, nil, cfg)
	chain := []Attempt{
		{ProviderID: "a", Model: "m", KeyID: "key_1"},
		{ProviderID: "a", Model: "m", KeyID: "key_2"},
	}
	start := time.Now()
	ch, err := eng.ChatStreamWithChain(ctx, chain, coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		got += ev.Delta
	}
	elapsed := time.Since(start)
	if got != "bee" {
		t.Fatalf("stream content = %q, want %q (rotation to the sibling)", got, "bee")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("rotation took %v, want < 2s (deadline 200ms, not the 65s stall bound)", elapsed)
	}
	if k1.streamCalls() != 1 {
		t.Fatalf("preferred account streamed %d times, want 1", k1.streamCalls())
	}
	if k2.streamCalls() != 1 {
		t.Fatalf("sibling streamed %d times, want 1", k2.streamCalls())
	}
}

// TestPreferredFailoverOffByDefault: with no deadline configured, a
// stalled preferred account does NOT rotate — the request dies on
// the caller's 1 s deadline with the sibling never tried.
func TestPreferredFailoverOffByDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	k1 := stallStreamClient("")
	k2 := streamClient([]string{"bee"}, nil)
	clients := map[string]provider.Client{
		ClientKey("a", "key_1"): k1,
		ClientKey("a", "key_2"): k2,
	}
	eng := NewEngine(clients, nil, testConfig())
	chain := []Attempt{
		{ProviderID: "a", Model: "m", KeyID: "key_1"},
		{ProviderID: "a", Model: "m", KeyID: "key_2"},
	}
	ch, err := eng.ChatStreamWithChain(ctx, chain, coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var streamErr error
	for ev := range ch {
		if ev.Err != nil {
			streamErr = ev.Err
		}
	}
	if streamErr == nil {
		t.Fatalf("want the caller-deadline error, got a clean stream (a rotation must not have happened)")
	}
	if k2.streamCalls() != 0 {
		t.Fatalf("sibling streamed %d times, want 0 (no rotation when the failover is off)", k2.streamCalls())
	}
}

// TestPreferredFailoverDisarmedByFirstByte: a preferred account that
// answers fast is never rotated — the deadline is disarmed at the
// first byte, and the stream runs past the deadline untouched.
func TestPreferredFailoverDisarmedByFirstByte(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k1 := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if onDelta != nil {
			if err := onDelta("fast"); err != nil {
				return nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
		if onDelta != nil {
			if err := onDelta("slow"); err != nil {
				return nil, err
			}
		}
		return &provider.ChatResponse{Content: "fastslow"}, nil
	}}
	k2 := streamClient([]string{"bee"}, nil)
	clients := map[string]provider.Client{
		ClientKey("a", "key_1"): k1,
		ClientKey("a", "key_2"): k2,
	}
	cfg := testConfig()
	cfg.PreferredFailoverAfter = 200 * time.Millisecond
	eng := NewEngine(clients, nil, cfg)
	chain := []Attempt{
		{ProviderID: "a", Model: "m", KeyID: "key_1"},
		{ProviderID: "a", Model: "m", KeyID: "key_2"},
	}
	ch, err := eng.ChatStreamWithChain(ctx, chain, coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v (the deadline must not kill a live stream)", ev.Err)
		}
		got += ev.Delta
	}
	if got != "fastslow" {
		t.Fatalf("stream content = %q, want %q", got, "fastslow")
	}
	if k2.streamCalls() != 0 {
		t.Fatalf("sibling streamed %d times, want 0 (first byte disarmed the deadline)", k2.streamCalls())
	}
}

// TestPreferredFailoverOnlyOneSibling: the rotation happens once —
// when the sibling ALSO stalls, there is no second rotation to the
// third account; the request dies on the caller's deadline.
func TestPreferredFailoverOnlyOneSibling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	k1 := stallStreamClient("")
	k2 := stallStreamClient("")
	k3 := streamClient([]string{"bee"}, nil)
	clients := map[string]provider.Client{
		ClientKey("a", "key_1"): k1,
		ClientKey("a", "key_2"): k2,
		ClientKey("a", "key_3"): k3,
	}
	cfg := testConfig()
	cfg.PreferredFailoverAfter = 200 * time.Millisecond
	eng := NewEngine(clients, nil, cfg)
	chain := []Attempt{
		{ProviderID: "a", Model: "m", KeyID: "key_1"},
		{ProviderID: "a", Model: "m", KeyID: "key_2"},
		{ProviderID: "a", Model: "m", KeyID: "key_3"},
	}
	ch, err := eng.ChatStreamWithChain(ctx, chain, coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var streamErr error
	for ev := range ch {
		if ev.Err != nil {
			streamErr = ev.Err
		}
	}
	if streamErr == nil {
		t.Fatalf("want an error (the sibling stalled and no second rotation exists), got a clean stream")
	}
	if k3.streamCalls() != 0 {
		t.Fatalf("third account streamed %d times, want 0 (only ONE sibling rotation per request)", k3.streamCalls())
	}
	if k2.streamCalls() != 1 {
		t.Fatalf("sibling streamed %d times, want 1 (the rotation reached it)", k2.streamCalls())
	}
}
