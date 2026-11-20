// M4.5 — same-host transient retry (502/503/504, max 3, decorrelated
// full-jitter, base 200 ms / cap 2 s).
package router

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// flaky502Client fails with a 502 the first n Chat calls, then serves.
func flaky502Client(n int, content string) *fakeClient {
	calls := 0
	return &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls <= n {
			return nil, &provider.UpstreamError{StatusCode: 502, Retryable: true, Body: "bad gateway"}
		}
		return &provider.ChatResponse{Content: content}, nil
	}}
}

// TestTransientRetryRecovers: a 502 twice, then success — the same
// host serves, the sibling is never dialled, and the trace shows
// three attempts on the same provider. Toggle: without the retry
// the first 502 fails over to the sibling at once.
func TestTransientRetryRecovers(t *testing.T) {
	ctx := context.Background()
	a := flaky502Client(2, "a-ok")
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "a-ok" {
		t.Fatalf("transient recovery = %v, %v, want a-ok + nil", resp, err)
	}
	if got := a.chatCalls(); got != 3 {
		t.Fatalf("provider A called %d times, want 3 (initial + 2 same-host retries)", got)
	}
	if got := b.chatCalls(); got != 0 {
		t.Fatalf("provider B called %d times, want 0 (no failover — the same host recovered)", got)
	}
	if len(trace.Attempts) != 3 {
		t.Fatalf("trace attempts = %d, want 3 (every retry is a recorded attempt)", len(trace.Attempts))
	}
}

// TestTransientRetryMaxThree: a provider that always 502s is retried
// exactly 3 times (4 calls total), then the chain fails over to the
// healthy sibling.
func TestTransientRetryMaxThree(t *testing.T) {
	ctx := context.Background()
	a := errClient(&provider.UpstreamError{StatusCode: 503, Retryable: true, Body: "unavailable"})
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("max-three failover = %v, %v, want bee + nil", resp, err)
	}
	if got := a.chatCalls(); got != 4 {
		t.Fatalf("provider A called %d times, want 4 (initial + 3 retries, then failover)", got)
	}
}

// TestTransientRetryOnly502_503_504: a 500 is NOT transient — it
// fails over at once with no same-host retry, exactly as before.
func TestTransientRetryOnly502_503_504(t *testing.T) {
	ctx := context.Background()
	for _, status := range []int{500, 529, 408} {
		a := errClient(&provider.UpstreamError{StatusCode: status, Retryable: true, Body: "boom"})
		b := okClient("bee")
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
		resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
		if err != nil || resp.Content != "bee" {
			t.Fatalf("status %d: = %v, %v, want bee + nil", status, resp, err)
		}
		if got := a.chatCalls(); got != 1 {
			t.Fatalf("status %d: provider A called %d times, want 1 (not transient, no same-host retry)", status, got)
		}
	}
	// 504 IS transient.
	a := flaky504Client(1, "a-ok")
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "a-ok" {
		t.Fatalf("504 recovery = %v, %v, want a-ok + nil", resp, err)
	}
	if got := a.chatCalls(); got != 2 {
		t.Fatalf("504: provider A called %d times, want 2 (initial + 1 retry)", got)
	}
}

func flaky504Client(n int, content string) *fakeClient {
	calls := 0
	return &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls <= n {
			return nil, &provider.UpstreamError{StatusCode: 504, Retryable: true, Body: "gateway timeout"}
		}
		return &provider.ChatResponse{Content: content}, nil
	}}
}

// TestTransientDelayBounds: the decorrelated jitter always lands in
// [200 ms, 2 s], grows along the ladder, and varies (not a fixed
// sleep, or a retry storm would synchronise).
func TestTransientDelayBounds(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": okClient("x")}, nil, Config{HedgeDelay: -1})
	var prev time.Duration
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		d := eng.transientDelay(prev)
		if d < TransientBackoffBase || d > TransientBackoffCap {
			t.Fatalf("transientDelay(%v) = %v, want within [200ms, 2s]", prev, d)
		}
		seen[d] = true
		prev = d
	}
	if len(seen) < 10 {
		t.Fatalf("only %d distinct delays in 50 draws, want jitter (not a fixed sleep)", len(seen))
	}
	// First retry from a cold ladder draws from [200ms, 200ms]:
	// the floor is the base itself.
	if d := eng.transientDelay(0); d != TransientBackoffBase {
		t.Fatalf("transientDelay(0) = %v, want exactly the 200ms base", d)
	}
}

// TestTransientRetryStream: a stream that 503s twice at start, then
// streams fine — the same host serves, pre-commit, with no failover.
func TestTransientRetryStream(t *testing.T) {
	ctx := context.Background()
	calls := 0
	a := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		calls++
		if calls <= 2 {
			return nil, &provider.UpstreamError{StatusCode: 503, Retryable: true, Body: "unavailable"}
		}
		if onDelta != nil {
			if err := onDelta("live"); err != nil {
				return nil, err
			}
		}
		return &provider.ChatResponse{Content: "live"}, nil
	}}
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
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
		t.Fatalf("stream provider called %d times, want 3 (initial + 2 same-host retries)", calls)
	}
	if b.streamCalls() != 0 {
		t.Fatalf("sibling streamed %d times, want 0 (no failover)", b.streamCalls())
	}
}
