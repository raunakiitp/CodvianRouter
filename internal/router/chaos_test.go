// M4.1 — chaos harness gate tests (M4 exit gate, measured).
//
//  1. Single-provider outage with >= 3 healthy fallbacks: 0%
//     client-visible error rate, and added failover latency <=
//     detection time + 2 ms.
//  2. Hedge cuts p99 >= 50% on a slow-provider scenario.
//  3. Mid-run kill: a provider dying halfway through the run still
//     yields 0 client-visible errors.
//
// All providers are fake clients (no network) wrapped in chaos
// injectors; the Engine under test is the real one, so the gate
// exercises the real request path.
package router

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/chaos"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// medianDuration returns the median of d (d is sorted in place).
func medianDuration(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)/2]
}

// p99Duration returns the 99th percentile of d (d is sorted in place).
func p99Duration(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	idx := int(float64(len(d))*0.99) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(d) {
		idx = len(d) - 1
	}
	return d[idx]
}

// TestChaosSingleProviderOutage is the M4 exit gate, part 1: one
// provider dead (500 on every call) in front of three healthy
// fallbacks — 100 requests, 0 client-visible errors, and the added
// failover latency no more than the detection time + 2 ms.
func TestChaosSingleProviderOutage(t *testing.T) {
	ctx := context.Background()
	const n = 100

	// Detection time: how long the dead target takes to report its
	// failure, measured directly (50 lone calls, median).
	dead := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	}}
	var detect []time.Duration
	for i := 0; i < 50; i++ {
		start := time.Now()
		_, _ = dead.Chat(ctx, provider.ChatRequest{Model: "m"})
		detect = append(detect, time.Since(start))
	}
	detection := medianDuration(detect)

	// Baseline: a lone healthy provider, same request, same engine
	// shape (hedge off — pure sequential failover is measured).
	healthy := func() map[string]provider.Client {
		return map[string]provider.Client{"b": okClient("bee")}
	}
	baseEng := NewEngine(healthy(), nil, Config{HedgeDelay: -1})
	var base []time.Duration
	for i := 0; i < n; i++ {
		start := time.Now()
		resp, _, err := baseEng.ChatWithChain(ctx, chainFor("b"), coolReq("m"))
		if err != nil || resp.Content != "bee" {
			t.Fatalf("baseline req %d = %v, %v, want bee + nil", i, resp, err)
		}
		base = append(base, time.Since(start))
	}
	baseP50 := medianDuration(base)

	// Failover: dead first, three healthy fallbacks.
	deadInj := chaos.NewInjector(&fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	}}, chaos.Fault{Kill: true})
	eng := NewEngine(map[string]provider.Client{
		"dead": deadInj,
		"b":    okClient("bee"),
		"c":    okClient("cee"),
		"d":    okClient("dee"),
	}, nil, Config{HedgeDelay: -1})
	var lat []time.Duration
	served := map[string]int{}
	for i := 0; i < n; i++ {
		start := time.Now()
		resp, _, err := eng.ChatWithChain(ctx, chainFor("dead", "b", "c", "d"), coolReq("m"))
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("failover req %d: client-visible error %v (want 0%% errors)", i, err)
		}
		lat = append(lat, elapsed)
		served[resp.Content]++
	}
	failP50 := medianDuration(lat)
	added := failP50 - baseP50
	bound := detection + 2*time.Millisecond
	t.Logf("chaos outage gate: n=%d errors=0 baseline p50=%v failover p50=%v added=%v detection p50=%v bound=%v served=%v",
		n, baseP50, failP50, added, detection, bound, served)
	if added > bound {
		t.Fatalf("added failover latency %v > detection (%v) + 2ms = %v", added, detection, bound)
	}
	if served["bee"] == 0 {
		t.Fatalf("served = %v, want the first healthy fallback to serve", served)
	}
}

// TestChaosHedgeCutsP99 is the M4 exit gate, part 2: a 500 ms slow
// first provider with a fast second — hedging at 150 ms must cut
// p99 by >= 50% versus no hedge.
func TestChaosHedgeCutsP99(t *testing.T) {
	ctx := context.Background()
	const n = 20
	slowFn := func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		select {
		case <-time.After(500 * time.Millisecond):
			return &provider.ChatResponse{Content: "slow"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	run := func(hedge time.Duration) []time.Duration {
		eng := NewEngine(map[string]provider.Client{
			"slow": &fakeClient{chatFn: slowFn},
			"fast": okClient("fast"),
		}, nil, Config{HedgeDelay: hedge})
		var lat []time.Duration
		for i := 0; i < n; i++ {
			start := time.Now()
			resp, _, err := eng.ChatWithChain(ctx, chainFor("slow", "fast"), coolReq("m"))
			if err != nil {
				t.Fatalf("hedge run (delay %v) req %d: %v", hedge, i, err)
			}
			if resp.Content != "fast" && hedge > 0 {
				t.Fatalf("hedged req %d served by %q, want the fast hedge winner", i, resp.Content)
			}
			lat = append(lat, time.Since(start))
		}
		return lat
	}
	unhedged := p99Duration(run(-1))
	hedged := p99Duration(run(150 * time.Millisecond))
	t.Logf("chaos hedge gate: n=%d unhedged p99=%v hedged p99=%v cut=%.1f%%", n, unhedged, hedged, 100*float64(unhedged-hedged)/float64(unhedged))
	if hedged > unhedged/2 {
		t.Fatalf("hedged p99 %v > 50%% of unhedged p99 %v (want >= 50%% cut)", hedged, unhedged)
	}
}

// TestChaosMidRunKill: the provider dies halfway through the run
// (SetFault mid-run) — every request still succeeds via the
// fallbacks, 0 client-visible errors.
func TestChaosMidRunKill(t *testing.T) {
	ctx := context.Background()
	inj := chaos.NewInjector(okClient("aye"), chaos.Fault{})
	eng := NewEngine(map[string]provider.Client{
		"a": inj,
		"b": okClient("bee"),
		// Explicit threshold 5: the profile default for an API-key
		// provider is 12 (M4.2), which this 20-request run would
		// never reach — the test's point is the breaker opening
		// after 5 killed calls, so the threshold is pinned here.
	}, nil, Config{HedgeDelay: -1, BreakerThreshold: 5})
	for i := 0; i < 20; i++ {
		if i == 10 {
			inj.SetFault(chaos.Fault{Kill: true})
		}
		resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
		if err != nil {
			t.Fatalf("req %d: client-visible error %v (want 0%% errors)", i, err)
		}
		want := "aye"
		if i >= 10 {
			want = "bee"
		}
		if resp.Content != want {
			t.Fatalf("req %d served by %q, want %q", i, resp.Content, want)
		}
	}
	// 10 healthy calls, then 5 killed calls — the 5th trips the
	// breaker, so the last 5 requests skip the dead target
	// entirely (no call at all) instead of hammering it.
	if calls, failures := inj.Stats(); calls != 15 || failures != 5 {
		t.Fatalf("injector stats = (%d calls, %d failures), want (15, 5)", calls, failures)
	}
	if st := breakerStatesByID(eng)["a"]; !st.Open {
		t.Fatalf("breaker = %+v, want OPEN after 5 killed calls", st)
	}
}
