package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestFailoverChaosMatrix asserts the core product promise under chaos:
// 1. A failing provider (dies / 500) fails over immediately.
// 2. A 429 rate limit is absorbed by the router and NEVER reaches the user.
// 3. A slow provider triggers hedging (at 150ms default) so client latency is preserved.
// 4. Mid-stream connection drop fails over gracefully.
// Real switch times are recorded and reported honestly.
func TestFailoverChaosMatrix(t *testing.T) {
	ctx := context.Background()

	// 1. Provider dies (HTTP 500 / connection error)
	t.Run("ProviderDies", func(t *testing.T) {
		deadClient := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return nil, &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "upstream 500 internal server error"}
		}}
		fallbackClient := okClient("fallback-success")

		eng := NewEngine(map[string]provider.Client{
			"dead":     deadClient,
			"fallback": fallbackClient,
		}, nil, Config{HedgeDelay: -1})

		const n = 50
		var switchTimes []time.Duration
		for i := 0; i < n; i++ {
			start := time.Now()
			resp, _, err := eng.ChatWithChain(ctx, chainFor("dead", "fallback"), coolReq("m"))
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("run %d: client received unexpected error: %v", i, err)
			}
			if resp.Content != "fallback-success" {
				t.Fatalf("run %d: served by %q, want fallback-success", i, resp.Content)
			}
			switchTimes = append(switchTimes, elapsed)
		}
		p50 := medianDuration(switchTimes)
		t.Logf("ProviderDies: n=%d errors=0%% switch p50=%v p99=%v", n, p50, p99Duration(switchTimes))
		if p50 > 10*time.Millisecond {
			t.Fatalf("failover switch time %v exceeds 10ms threshold", p50)
		}
	})

	// 2. Provider returns 429 (rate limit) - MUST NEVER reach user
	t.Run("ProviderReturns429", func(t *testing.T) {
		rateLimitedClient := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return nil, &provider.UpstreamError{StatusCode: 429, Retryable: true, Body: `{"error":{"message":"Rate limit exceeded","code":429}}`}
		}}
		fallbackClient := okClient("absorbed-429-fallback")

		eng := NewEngine(map[string]provider.Client{
			"rate-limited": rateLimitedClient,
			"fallback":     fallbackClient,
		}, nil, Config{HedgeDelay: -1})

		const n = 50
		var switchTimes []time.Duration
		for i := 0; i < n; i++ {
			start := time.Now()
			resp, _, err := eng.ChatWithChain(ctx, chainFor("rate-limited", "fallback"), coolReq("m"))
			elapsed := time.Since(start)
			if err != nil {
				var upErr *provider.UpstreamError
				if errors.As(err, &upErr) && upErr.StatusCode == 429 {
					t.Fatalf("CRITICAL RULE VIOLATION: 429 reached the client!")
				}
				t.Fatalf("run %d: client received error: %v", i, err)
			}
			if resp.Content != "absorbed-429-fallback" {
				t.Fatalf("run %d: served by %q, want absorbed-429-fallback", i, resp.Content)
			}
			switchTimes = append(switchTimes, elapsed)
		}
		p50 := medianDuration(switchTimes)
		t.Logf("ProviderReturns429: n=%d errors=0%% 429_leaks=0 switch p50=%v p99=%v", n, p50, p99Duration(switchTimes))
		if p50 > 10*time.Millisecond {
			t.Fatalf("failover switch time %v exceeds 10ms threshold", p50)
		}
	})

	// 3. Provider goes slow (hangs past hedge delay)
	t.Run("ProviderGoesSlowHedge", func(t *testing.T) {
		slowClient := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			select {
			case <-time.After(500 * time.Millisecond):
				return &provider.ChatResponse{Content: "slow-response"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
		fastFallback := okClient("fast-hedge-winner")

		// 150ms hedge delay (product standard)
		eng := NewEngine(map[string]provider.Client{
			"slow": slowClient,
			"fast": fastFallback,
		}, nil, Config{HedgeDelay: 150 * time.Millisecond})

		const n = 10
		var switchTimes []time.Duration
		for i := 0; i < n; i++ {
			start := time.Now()
			resp, _, err := eng.ChatWithChain(ctx, chainFor("slow", "fast"), coolReq("m"))
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("run %d: error: %v", i, err)
			}
			if resp.Content != "fast-hedge-winner" {
				t.Fatalf("run %d: served by %q, want fast-hedge-winner", i, resp.Content)
			}
			switchTimes = append(switchTimes, elapsed)
		}
		p50 := medianDuration(switchTimes)
		t.Logf("ProviderGoesSlowHedge: n=%d hedged p50=%v (expected ~150-180ms, far below 500ms)", n, p50)
		if p50 > 300*time.Millisecond {
			t.Fatalf("hedging failed to prevent slow provider latency: p50 = %v", p50)
		}
	})

	// 4. Provider drops connection mid-stream
	t.Run("ProviderDropsMidStream", func(t *testing.T) {
		flakyStreamClient := &fakeClient{
			streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
				_ = onDelta("partial chunk before drop")
				return nil, errors.New("connection reset by peer")
			},
			chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
				return nil, errors.New("connection reset by peer")
			},
		}
		healthyFallback := okClient("healthy-stream-recovery")

		eng := NewEngine(map[string]provider.Client{
			"flaky":   flakyStreamClient,
			"healthy": healthyFallback,
		}, nil, Config{HedgeDelay: -1})

		resp, _, err := eng.ChatWithChain(ctx, chainFor("flaky", "healthy"), coolReq("m"))
		if err != nil {
			t.Fatalf("failover after stream drop failed: %v", err)
		}
		if resp.Content != "healthy-stream-recovery" {
			t.Fatalf("served by %q, want healthy-stream-recovery", resp.Content)
		}
		t.Logf("ProviderDropsMidStream: successfully failed over to healthy provider (%s)", resp.Content)
	})
}

func BenchmarkFailoverOn500(b *testing.B) {
	ctx := context.Background()
	dead := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "fail"}
	}}
	fallback := okClient("ok")
	eng := NewEngine(map[string]provider.Client{"dead": dead, "fallback": fallback}, nil, Config{HedgeDelay: -1})
	req := coolReq("m")
	chain := chainFor("dead", "fallback")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, err := eng.ChatWithChain(ctx, chain, req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFailoverOn429(b *testing.B) {
	ctx := context.Background()
	rateLimited := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.UpstreamError{StatusCode: 429, Retryable: true, Body: "rate limited"}
	}}
	fallback := okClient("ok")
	eng := NewEngine(map[string]provider.Client{"rateLimited": rateLimited, "fallback": fallback}, nil, Config{HedgeDelay: -1})
	req := coolReq("m")
	chain := chainFor("rateLimited", "fallback")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, err := eng.ChatWithChain(ctx, chain, req)
		if err != nil {
			b.Fatal(err)
		}
	}
}
