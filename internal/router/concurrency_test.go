package router

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// trackingClient is a fakeClient with a switchable delay and a live
// max-concurrency observation, so tests can prove the limiter caps
// real in-flight work — not just a counter.
type trackingClient struct {
	*fakeClient
	delay atomic.Int64 // nanoseconds
	cur   atomic.Int32
	max   atomic.Int32
}

func newTrackingClient(content string) *trackingClient {
	tc := &trackingClient{fakeClient: &fakeClient{}}
	tc.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		cur := tc.cur.Add(1)
		for {
			m := tc.max.Load()
			if cur <= m || tc.max.CompareAndSwap(m, cur) {
				break
			}
		}
		defer tc.cur.Add(-1)
		d := time.Duration(tc.delay.Load())
		if d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &provider.ChatResponse{Content: content}, nil
	}
	return tc
}

func (c *trackingClient) setDelay(d time.Duration) { c.delay.Store(int64(d)) }
func (c *trackingClient) maxConcurrent() int32     { return c.max.Load() }

func chatReq(prompt string) provider.ChatRequest {
	return provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: prompt}}}
}

func concurrencyEngine(t *testing.T, slow, fast *trackingClient) *Engine {
	t.Helper()
	eng := NewEngine(map[string]provider.Client{"slow": slow, "fast": fast}, nil, Config{
		HedgeDelay: -1, // hedging off: the limiter, not the hedge, must do the shedding
		ChainFor: func(model string) []Attempt {
			return []Attempt{{ProviderID: "slow", Model: model}, {ProviderID: "fast", Model: model}}
		},
	})
	return eng
}

func TestEngineSlowProviderCapShrinksAndSheds(t *testing.T) {
	slow := newTrackingClient("slow-ok")
	fast := newTrackingClient("fast-ok")
	eng := concurrencyEngine(t, slow, fast)
	ctx := context.Background()

	// Phase 1 — warm baseline: 5 fast (5ms) calls to the slow provider.
	slow.setDelay(5 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if _, err := eng.Chat(ctx, chatReq(fmt.Sprintf("warm-%d", i))); err != nil {
			t.Fatalf("warm call %d: %v", i, err)
		}
	}
	// Phase 2 — the provider slows to 150ms: 6 sequential calls.
	// The gradient must shrink its cap toward the floor.
	slow.setDelay(150 * time.Millisecond)
	for i := 0; i < 6; i++ {
		if _, err := eng.Chat(ctx, chatReq(fmt.Sprintf("slow-%d", i))); err != nil {
			t.Fatalf("slow call %d: %v", i, err)
		}
	}
	var slowLimit float64
	for _, st := range eng.ConcurrencyStats() {
		if st.Provider == "slow" {
			slowLimit = st.Limit
		}
	}
	if slowLimit == 0 {
		t.Fatal("no limiter state for slow provider after 11 calls")
	}
	if slowLimit > 6 {
		t.Fatalf("slow provider limit = %.2f after 6x150ms samples, want <= 6 (near floor 2)", slowLimit)
	}
	t.Logf("slow provider limit after slowdown: %.2f", slowLimit)

	// Phase 3 — burst: 10 concurrent distinct requests (distinct
	// prompts, so single-flight coalescing cannot share them).
	// The slow provider must serve at most its cap; the rest must
	// shed to the fast provider.
	slowBefore, fastBefore := slow.chatCalls(), fast.chatCalls()
	// A start barrier: all ten goroutines begin together, so the
	// burst is a real contention window (every request orders
	// against the same in-flight state) instead of ten launches
	// smeared across goroutine-scheduling jitter — a straggler
	// launched after the first slow calls finished could
	// otherwise acquire a freed slow slot and skew the split.
	// The assertions below are unchanged: the limiter, not the
	// launch order, must decide who serves what (CHECKLIST 190).
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = eng.Chat(ctx, chatReq(fmt.Sprintf("burst-%d", i)))
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("burst request %d: %v", i, err)
		}
	}
	slowDelta, fastDelta := slow.chatCalls()-slowBefore, fast.chatCalls()-fastBefore
	t.Logf("burst of 10: slow served %d, fast served %d, slow max concurrent %d, slow limit %.2f",
		slowDelta, fastDelta, slow.maxConcurrent(), slowLimit)
	if slowDelta > 3 {
		t.Fatalf("slow provider served %d of the burst, want <= 3 (its cap is %.2f)", slowDelta, slowLimit)
	}
	if fastDelta < 7 {
		t.Fatalf("fast provider served %d of the burst, want >= 7 (load must shed to it)", fastDelta)
	}
	if mc := slow.maxConcurrent(); mc > 3 {
		t.Fatalf("slow provider max concurrent = %d, want <= 3", mc)
	}
	// Every burst request got an answer from exactly one provider:
	// no request was dropped by the limiter when a healthy
	// provider was available.
	if slowDelta+fastDelta != 10 {
		t.Fatalf("burst served %d requests, want 10", slowDelta+fastDelta)
	}
}

func TestEngineSaturatedProviderDeprioritised(t *testing.T) {
	slow := newTrackingClient("slow-ok")
	fast := newTrackingClient("fast-ok")
	eng := concurrencyEngine(t, slow, fast)
	// Shrink the slow cap to the floor with slow samples — after a
	// fast baseline is established first (without one, 500ms IS
	// the baseline and the gradient is 1, correctly) — then hold
	// both floor slots: orderAttempts must put the fast provider
	// first while slow is saturated.
	for i := 0; i < 5; i++ {
		eng.limiter.Record("slow", 5*time.Millisecond, true)
	}
	for i := 0; i < 10; i++ {
		eng.limiter.Record("slow", 500*time.Millisecond, true)
	}
	if got := eng.limiter.Limit("slow"); got > 2.5 {
		t.Fatalf("limit after slow samples = %.2f, want ~floor 2", got)
	}
	if !eng.limiter.TryAcquire("slow") || !eng.limiter.TryAcquire("slow") {
		t.Fatal("could not hold both floor slots")
	}
	defer eng.limiter.Release("slow")
	defer eng.limiter.Release("slow")
	ordered, _ := eng.orderAttempts([]Attempt{{ProviderID: "slow", Model: "m"}, {ProviderID: "fast", Model: "m"}}, "balanced", "hard", false)
	if len(ordered) != 2 || ordered[0].ProviderID != "fast" {
		t.Fatalf("saturated provider not deprioritised: order = %+v", ordered)
	}
}

func TestEngineAllProvidersAtCapFailsOpenHonestly(t *testing.T) {
	slow := newTrackingClient("slow-ok")
	eng := NewEngine(map[string]provider.Client{"slow": slow}, nil, Config{
		HedgeDelay: -1,
		ChainFor: func(model string) []Attempt {
			return []Attempt{{ProviderID: "slow", Model: model}}
		},
	})
	// Shrink the cap to the floor and hold both slots: the next
	// request has nowhere to shed to, so it must fail with the
	// limiter error — and the trace must say so (skipped, not an
	// upstream failure), with no breaker trip and no ledger row
	// for a call that never happened. (Fast samples first: without
	// a low baseline, 500ms is the baseline and the cap never
	// shrinks.)
	for i := 0; i < 5; i++ {
		eng.limiter.Record("slow", 5*time.Millisecond, true)
	}
	for i := 0; i < 10; i++ {
		eng.limiter.Record("slow", 500*time.Millisecond, true)
	}
	if !eng.limiter.TryAcquire("slow") || !eng.limiter.TryAcquire("slow") {
		t.Fatal("could not hold both floor slots")
	}
	defer eng.limiter.Release("slow")
	defer eng.limiter.Release("slow")
	_, err := eng.Chat(context.Background(), chatReq("only"))
	if !errors.Is(err, adaptive.ErrLimited) {
		t.Fatalf("Chat with every provider at cap = %v, want adaptive.ErrLimited", err)
	}
	trace := eng.LastTrace()
	found := false
	for _, att := range trace.Attempts {
		if att.Outcome == "skipped_concurrency_limit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("trace has no skipped_concurrency_limit attempt: %+v", trace.Attempts)
	}
	if slow.chatCalls() != 0 {
		t.Fatalf("slow provider was called %d times while at cap, want 0", slow.chatCalls())
	}
	// Fail-open: a nil limiter admits everything.
	var nilLimiter *adaptive.Limiter
	if !nilLimiter.TryAcquire("slow") {
		t.Fatal("nil limiter must admit (fail open)")
	}
}

func TestEngineLimiterStatsEmptyBeforeTraffic(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": okClient("x")}, nil, Config{})
	if got := eng.ConcurrencyStats(); len(got) != 0 {
		t.Fatalf("ConcurrencyStats before traffic = %+v, want empty", got)
	}
}
