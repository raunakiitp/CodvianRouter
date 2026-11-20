// M4.6 — throughput watchdog on stalled streams (30 s warmup /
// 30 s window / 4 B/s in production; shrunk here).
package router

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// shrinkWatchdog sets the watchdog durations for one test and
// restores them after.
func shrinkWatchdog(t *testing.T, warmup, window time.Duration) {
	t.Helper()
	oldWarmup, oldWindow := watchdogWarmup, watchdogWindow
	watchdogWarmup, watchdogWindow = warmup, window
	t.Cleanup(func() { watchdogWarmup, watchdogWindow = oldWarmup, oldWindow })
}

// stallStreamClient streams content (or nothing) once, then blocks
// until ctx ends — a provider that stops delivering bytes.
func stallStreamClient(content string) *fakeClient {
	return &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if content != "" && onDelta != nil {
			if err := onDelta(content); err != nil {
				return nil, err
			}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
}

// TestWatchdogAbortsStalledStream: a stream that delivers "hello"
// then stalls is aborted by the watchdog well inside a second
// (not after the 30-minute stream lifetime), and the client gets an
// error event naming the stall — not a hang.
func TestWatchdogAbortsStalledStream(t *testing.T) {
	shrinkWatchdog(t, 100*time.Millisecond, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := stallStreamClient("hello")
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{HedgeDelay: -1})
	start := time.Now()
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var got strings.Builder
	var streamErr error
	for ev := range ch {
		if ev.Err != nil {
			streamErr = ev.Err
			break
		}
		got.WriteString(ev.Delta)
	}
	elapsed := time.Since(start)
	if got.String() != "hello" {
		t.Fatalf("delivered = %q, want %q (the stall happened after commit)", got.String(), "hello")
	}
	if streamErr == nil {
		t.Fatalf("no stream error, want the watchdog abort (a stalled stream must not hang)")
	}
	if !IsThroughputStalled(streamErr) {
		t.Fatalf("stream error = %v, want a throughput-stall abort", streamErr)
	}
	// Warmup 100 ms + window 200 ms (+ preread): the abort must
	// land in well under a second — the 30-minute stream lifetime
	// is not what ends a stalled stream.
	if elapsed > 2*time.Second {
		t.Fatalf("watchdog abort took %v, want < 2s (warmup 100ms + window 200ms)", elapsed)
	}
}

// TestWatchdogFailsOverPreCommit: a stream that delivers nothing
// and stalls is aborted before commit, so the chain fails over to
// the healthy sibling — the client gets a full answer, not a hang.
func TestWatchdogFailsOverPreCommit(t *testing.T) {
	shrinkWatchdog(t, 100*time.Millisecond, 200*time.Millisecond)
	oldPreread := prereadMaxWait
	prereadMaxWait = 100 * time.Millisecond
	t.Cleanup(func() { prereadMaxWait = oldPreread })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := stallStreamClient("")
	// okClient has no streamFn (its default stream delta is "ok"),
	// so the healthy sibling is a streaming fake.
	b := streamClient([]string{"bee"}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	start := time.Now()
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var got strings.Builder
	var streamErr error
	for ev := range ch {
		if ev.Err != nil {
			streamErr = ev.Err
			break
		}
		got.WriteString(ev.Delta)
	}
	elapsed := time.Since(start)
	if streamErr != nil {
		t.Fatalf("stream error = %v, want none (pre-commit stall must fail over)", streamErr)
	}
	if got.String() != "bee" {
		t.Fatalf("delivered = %q, want %q (failover to the healthy sibling)", got.String(), "bee")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("pre-commit failover took %v, want < 2s", elapsed)
	}
}

// TestWatchdogHealthyStreamUnaffected: a slow-but-alive stream —
// 10 bytes every 100 ms, 5 B/s... no: 10 bytes / 100 ms is 100 B/s,
// far above the 4 B/s floor — completes normally with no abort.
// The toggle is the floor: the same stream at 1 byte per 500 ms
// (2 B/s) would be aborted.
func TestWatchdogHealthyStreamUnaffected(t *testing.T) {
	shrinkWatchdog(t, 100*time.Millisecond, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		for i := 0; i < 5; i++ {
			if onDelta != nil {
				if err := onDelta("0123456789"); err != nil {
					return nil, err
				}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		return &provider.ChatResponse{Content: "01234567890123456789012345678901234567890123456789"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{HedgeDelay: -1})
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var got strings.Builder
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("healthy stream errored: %v (want no watchdog abort at 100 B/s)", ev.Err)
		}
		got.WriteString(ev.Delta)
	}
	if got.Len() != 50 {
		t.Fatalf("delivered %d bytes, want 50", got.Len())
	}
}

// TestWatchdogTrickleBelowFloor: 1 byte every 500 ms is 2 B/s —
// below the 4 B/s floor — so the watchdog aborts even though bytes
// are still arriving. A trickle is a stall.
func TestWatchdogTrickleBelowFloor(t *testing.T) {
	shrinkWatchdog(t, 100*time.Millisecond, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		for {
			if onDelta != nil {
				if err := onDelta("x"); err != nil {
					return nil, err
				}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{HedgeDelay: -1})
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var streamErr error
	for ev := range ch {
		if ev.Err != nil {
			streamErr = ev.Err
			break
		}
	}
	if streamErr == nil {
		t.Fatalf("no stream error, want the watchdog abort (2 B/s trickle is below the 4 B/s floor)")
	}
	if !IsThroughputStalled(streamErr) {
		t.Fatalf("stream error = %v, want a throughput-stall abort", streamErr)
	}
}

// TestWatchdogBytes: empty events count 0 (heartbeats are not
// throughput); tool-call payload counts.
func TestWatchdogBytes(t *testing.T) {
	if got := watchdogBytes(StreamEvent{}); got != 0 {
		t.Fatalf("empty event = %d bytes, want 0", got)
	}
	if got := watchdogBytes(StreamEvent{Delta: "hello"}); got != 5 {
		t.Fatalf("delta event = %d bytes, want 5", got)
	}
	ev := StreamEvent{ToolCalls: []provider.ChunkToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"Oslo"}`}}}
	if got := watchdogBytes(ev); got != len("call_1")+len("get_weather")+len(`{"city":"Oslo"}`) {
		t.Fatalf("tool-call event = %d bytes, want id+name+arguments", got)
	}
}

// TestWatchdogCheckUnit: the window math directly — warmup is grace
// (bytes in warmup are not credit), a healthy window restarts, a
// starved window aborts.
func TestWatchdogCheckUnit(t *testing.T) {
	now := time.Now()
	w := newStreamWatchdog(now)
	w.noteBytes(1000) // warmup bytes: grace, not credit
	if err := w.check(now.Add(watchdogWarmup)); err != nil {
		t.Fatalf("warmup-end check = %v, want nil (warmup is grace)", err)
	}
	if !w.warmed || w.bytes != 0 {
		t.Fatalf("after warmup: warmed=%v bytes=%d, want warmed=true bytes=0", w.warmed, w.bytes)
	}
	w.noteBytes(int(watchdogMinBytesPerSec) * int(watchdogWindow.Seconds()))
	if err := w.check(now.Add(watchdogWarmup).Add(watchdogWindow)); err != nil {
		t.Fatalf("healthy-window check = %v, want nil (exactly at the floor is alive)", err)
	}
	if err := w.check(now.Add(watchdogWarmup).Add(2 * watchdogWindow)); !IsThroughputStalled(err) {
		t.Fatalf("starved-window check = %v, want throughputStalledError", err)
	}
}
