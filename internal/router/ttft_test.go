// M4.7a — predictive TTFT breaker (skip a target whose mean
// time-to-first-byte exceeds PredictiveTtftMs; min 5 samples).
package router

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// slowFirstByteStream streams after d — its TTFT is ~d. Its Chat
// path serves the same content immediately (chat has no TTFT).
func slowFirstByteStream(d time.Duration, content string) *fakeClient {
	return &fakeClient{
		chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: content}, nil
		},
		streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(d):
			}
			if onDelta != nil {
				if err := onDelta(content); err != nil {
					return nil, err
				}
			}
			return &provider.ChatResponse{Content: content}, nil
		}}
}

// drainStream drains a stream channel, returning the joined deltas.
func drainStream(t *testing.T, ch <-chan StreamEvent) string {
	t.Helper()
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream error: %v", ev.Err)
		}
		got += ev.Delta
	}
	return got
}

// seedTTFT streams n times from provider a (TTFT ~d each) so the
// predictor has n samples.
func seedTTFT(t *testing.T, eng *Engine, chain []Attempt, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ch, err := eng.ChatStreamWithChain(context.Background(), chain, coolReq("m"))
		if err != nil {
			t.Fatalf("seed stream %d start: %v", i+1, err)
		}
		drainStream(t, ch)
	}
}

// TestPredictiveTTFTSkipsSlowTarget: 5 streams at ~300 ms TTFT from
// A, threshold 100 ms — the next chat request skips A and serves
// from B without dialling A at all.
func TestPredictiveTTFTSkipsSlowTarget(t *testing.T) {
	ctx := context.Background()
	a := slowFirstByteStream(300*time.Millisecond, "slow")
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1, PredictiveTtftMs: 100})
	seedTTFT(t, eng, chainFor("a"), 5)
	if avg, n := eng.PredictiveTTFT("a"); n != 5 || avg < 250 || avg > 450 {
		t.Fatalf("PredictiveTTFT(a) = %.1f ms / %d samples, want ~300 ms / 5", avg, n)
	}
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("chat = %v, %v, want bee + nil (slow A skipped)", resp, err)
	}
	if got := a.chatCalls(); got != 0 {
		t.Fatalf("provider A chat-called %d times, want 0 (predicted slow, never dialled)", got)
	}
	if got := b.chatCalls(); got != 1 {
		t.Fatalf("provider B chat-called %d times, want 1", got)
	}
	found := false
	for _, att := range trace.Attempts {
		if att.Outcome == "skipped_predictive_ttft" && att.Provider == "a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("trace has no skipped_predictive_ttft for A (outcomes: %v)", trace.Attempts)
	}
}

// TestPredictiveTTFTNeedsFiveSamples: 4 slow streams are not enough
// to trust the prediction — A still serves.
func TestPredictiveTTFTNeedsFiveSamples(t *testing.T) {
	ctx := context.Background()
	a := slowFirstByteStream(300*time.Millisecond, "slow")
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1, PredictiveTtftMs: 100})
	seedTTFT(t, eng, chainFor("a"), 4)
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "slow" {
		t.Fatalf("chat = %v, %v, want slow + nil (4 samples: prediction not trusted yet)", resp, err)
	}
	if got := a.chatCalls(); got != 1 {
		t.Fatalf("provider A chat-called %d times, want 1", got)
	}
}

// TestPredictiveTTFTLoneSlowProviderServes: a slow provider with no
// alternative still serves — slow beats failing.
func TestPredictiveTTFTLoneSlowProviderServes(t *testing.T) {
	ctx := context.Background()
	a := slowFirstByteStream(300*time.Millisecond, "slow")
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{HedgeDelay: -1, PredictiveTtftMs: 100})
	seedTTFT(t, eng, chainFor("a"), 5)
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != nil || resp.Content != "slow" {
		t.Fatalf("chat = %v, %v, want slow + nil (lone provider serves despite the prediction)", resp, err)
	}
}

// TestPredictiveTTFTRecovery: a provider that recovers is
// un-skipped — 5 slow samples (300 ms) then 15 fast ones bring
// the 20-sample mean to ~75 ms, below the 100 ms threshold.
func TestPredictiveTTFTRecovery(t *testing.T) {
	ctx := context.Background()
	a := slowFirstByteStream(300*time.Millisecond, "slow")
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1, PredictiveTtftMs: 100})
	seedTTFT(t, eng, chainFor("a"), 5)
	// Fast streams: swap in a fast streamer for the same provider.
	a.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if onDelta != nil {
			if err := onDelta("fast"); err != nil {
				return nil, err
			}
		}
		return &provider.ChatResponse{Content: "fast"}, nil
	}
	seedTTFT(t, eng, chainFor("a"), 15)
	if avg, n := eng.PredictiveTTFT("a"); n != 20 || avg > 100 {
		t.Fatalf("PredictiveTTFT(a) after recovery = %.1f ms / %d samples, want <100 ms / 20", avg, n)
	}
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "slow" {
		t.Fatalf("chat = %v, %v, want slow + nil (recovered provider serves again)", resp, err)
	}
}

// TestPredictiveTTFTOffByDefault: with PredictiveTtftMs 0 a slow
// provider is never skipped, however many slow samples exist.
func TestPredictiveTTFTOffByDefault(t *testing.T) {
	ctx := context.Background()
	a := slowFirstByteStream(300*time.Millisecond, "slow")
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	seedTTFT(t, eng, chainFor("a"), 5)
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "slow" {
		t.Fatalf("chat = %v, %v, want slow + nil (predictor off by default)", resp, err)
	}
}

// TestTTFTStatsRing: the rolling window keeps the newest 20
// samples — the mean follows the recent streams, not ancient ones.
func TestTTFTStatsRing(t *testing.T) {
	s := &ttftStats{}
	for i := 0; i < 20; i++ {
		s.add(300 * time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		s.add(10 * time.Millisecond)
	}
	avg, n := s.avg()
	if n != 20 || avg != 10*time.Millisecond {
		t.Fatalf("ring avg = %v / %d, want 10ms / 20 (old samples aged out)", avg, n)
	}
	s.add(50 * time.Millisecond)
	avg, n = s.avg()
	if n != 20 || avg != 12*time.Millisecond {
		t.Fatalf("ring avg after one more = %v / %d, want 12ms / 20", avg, n)
	}
}
