package server

import (
	"sync"
	"testing"
	"time"
)

// fakeBatchClock is a deterministic clock for the batcher
// bounds tests: the test advances time by hand, and timers fire
// exactly when their deadline passes — no sleeps, no scheduler
// noise, so the 3-token / 8 ms bounds are asserted exactly.
type fakeBatchClock struct {
	mu     sync.Mutex
	now    time.Duration
	timers []*fakeBatchTimer
}

type fakeBatchTimer struct {
	clock   *fakeBatchClock
	fireAt  time.Duration
	fn      func()
	stopped bool
	fired   bool
}

func (c *fakeBatchClock) after(d time.Duration, fn func()) batchTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeBatchTimer{clock: c, fireAt: c.now + d, fn: fn}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeBatchTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// advance moves the fake clock forward, firing every timer
// whose deadline is reached, in deadline order. A timer armed
// by a firing callback is honoured in the same advance if its
// deadline is also reached.
func (c *fakeBatchClock) advance(d time.Duration) {
	c.mu.Lock()
	target := c.now + d
	for {
		var next *fakeBatchTimer
		for _, t := range c.timers {
			if t.stopped || t.fired {
				continue
			}
			if t.fireAt <= target && (next == nil || t.fireAt < next.fireAt) {
				next = t
			}
		}
		if next == nil {
			break
		}
		c.now = next.fireAt
		next.fired = true
		fn := next.fn
		c.mu.Unlock()
		fn()
		c.mu.Lock()
	}
	c.now = target
	c.mu.Unlock()
}

// batchRecorder records what the batcher wrote and flushed.
type batchRecorder struct {
	mu      sync.Mutex
	lines   []string
	flushes int
}

func (r *batchRecorder) write(line string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	return nil
}

func (r *batchRecorder) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
}

func (r *batchRecorder) delivered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

func newTestBatcher(rec *batchRecorder, clock *fakeBatchClock) *sseBatcher {
	return newSSEBatcher(rec.write, rec.flush, sseBatchMaxTokens, sseBatchMaxDelay, clock.after)
}

// TestSSEBatcherNeverBatchesMoreThanThree: the third token in a
// fast burst flushes immediately — no clock advance needed —
// and a fourth token starts a new batch instead of joining a
// four-event flush.
func TestSSEBatcherNeverBatchesMoreThanThree(t *testing.T) {
	clock := &fakeBatchClock{}
	rec := &batchRecorder{}
	b := newTestBatcher(rec, clock)

	if err := b.Write("data: tok1\n\n"); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if err := b.Write("data: tok2\n\n"); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	if got := rec.delivered(); len(got) != 0 {
		t.Fatalf("2 tokens delivered %d lines before the 3-token bound, want 0 (still batched)", len(got))
	}
	if err := b.Write("data: tok3\n\n"); err != nil {
		t.Fatalf("Write 3: %v", err)
	}
	// The 3rd write must flush synchronously, with the clock
	// still at zero — the count bound, not the timer, fired.
	if got := rec.delivered(); len(got) != 3 {
		t.Fatalf("after 3rd token delivered %d lines, want 3", len(got))
	}
	if rec.flushes != 1 {
		t.Fatalf("flushes = %d, want 1 (one 3-token flush)", rec.flushes)
	}
	// A 4th token must NOT be swept into the first flush, and
	// must not be delivered until the next bound fires.
	if err := b.Write("data: tok4\n\n"); err != nil {
		t.Fatalf("Write 4: %v", err)
	}
	if got := rec.delivered(); len(got) != 3 {
		t.Fatalf("after 4th token delivered %d lines, want 3 (4th still batched)", len(got))
	}
	// The 4th token's own 8 ms timer flushes it alone.
	clock.advance(8 * time.Millisecond)
	if got := rec.delivered(); len(got) != 4 || got[3] != "data: tok4\n\n" {
		t.Fatalf("after 8 ms delivered %v, want 4 lines ending with tok4", got)
	}
	if rec.flushes != 2 {
		t.Fatalf("flushes = %d, want 2", rec.flushes)
	}
}

// TestSSEBatcherNeverHoldsLongerThan8ms: a lone token is
// delivered exactly when its 8 ms timer fires — not at 7 ms,
// and never later than 8 ms after it landed.
func TestSSEBatcherNeverHoldsLongerThan8ms(t *testing.T) {
	clock := &fakeBatchClock{}
	rec := &batchRecorder{}
	b := newTestBatcher(rec, clock)

	if err := b.Write("data: lone\n\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	clock.advance(7 * time.Millisecond)
	if got := rec.delivered(); len(got) != 0 {
		t.Fatalf("delivered %d lines at 7 ms, want 0 (timer not yet fired)", len(got))
	}
	clock.advance(1 * time.Millisecond) // 8 ms total
	if got := rec.delivered(); len(got) != 1 || got[0] != "data: lone\n\n" {
		t.Fatalf("delivered %v at 8 ms, want the lone token", got)
	}
	if rec.flushes != 1 {
		t.Fatalf("flushes = %d, want 1", rec.flushes)
	}
}

// TestSSEBatcherTimerRunsFromFirstToken: the 8 ms window starts
// when the first token of a batch lands, not when the last one
// does — a second token arriving 5 ms in still leaves at 8 ms
// from the first.
func TestSSEBatcherTimerRunsFromFirstToken(t *testing.T) {
	clock := &fakeBatchClock{}
	rec := &batchRecorder{}
	b := newTestBatcher(rec, clock)

	if err := b.Write("data: a\n\n"); err != nil {
		t.Fatalf("Write a: %v", err)
	}
	clock.advance(5 * time.Millisecond)
	if err := b.Write("data: b\n\n"); err != nil {
		t.Fatalf("Write b: %v", err)
	}
	clock.advance(2 * time.Millisecond) // 7 ms from the first token
	if got := rec.delivered(); len(got) != 0 {
		t.Fatalf("delivered %d lines at 7 ms from first token, want 0", len(got))
	}
	clock.advance(1 * time.Millisecond) // 8 ms from the first token
	if got := rec.delivered(); len(got) != 2 {
		t.Fatalf("delivered %d lines at 8 ms from first token, want 2", len(got))
	}
}

// TestSSEBatcherFlushForcesDelivery: an explicit Flush
// delivers a partial batch at once — the path [DONE] and error
// events take so a stream close is never delayed.
func TestSSEBatcherFlushForcesDelivery(t *testing.T) {
	clock := &fakeBatchClock{}
	rec := &batchRecorder{}
	b := newTestBatcher(rec, clock)

	if err := b.Write("data: a\n\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := b.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := rec.delivered(); len(got) != 1 {
		t.Fatalf("delivered %d lines after Flush, want 1", len(got))
	}
	// A flush of an empty batch must not call the underlying
	// Flush again (no spurious flush on the wire).
	flushes := rec.flushes
	if err := b.Flush(); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if rec.flushes != flushes {
		t.Fatalf("empty Flush called the underlying flush (flushes %d -> %d)", flushes, rec.flushes)
	}
}

// TestSSEBatcherCloseFlushesAndPassesThrough: Close delivers
// the pending batch, and a write after Close goes straight
// through instead of being held.
func TestSSEBatcherCloseFlushesAndPassesThrough(t *testing.T) {
	clock := &fakeBatchClock{}
	rec := &batchRecorder{}
	b := newTestBatcher(rec, clock)

	if err := b.Write("data: a\n\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := rec.delivered(); len(got) != 1 {
		t.Fatalf("delivered %d lines after Close, want 1", len(got))
	}
	if err := b.Write("data: late\n\n"); err != nil {
		t.Fatalf("Write after Close: %v", err)
	}
	if got := rec.delivered(); len(got) != 2 || got[1] != "data: late\n\n" {
		t.Fatalf("delivered %v after Close + write, want both lines at once", got)
	}
}

// TestSSEBatcherSingleTokenPassThrough: maxTokens 1 means
// every event flushes immediately — the shape the server uses
// when batching is disabled (CODVIAN_ROUTER_SSE_BATCH=0).
func TestSSEBatcherSingleTokenPassThrough(t *testing.T) {
	clock := &fakeBatchClock{}
	rec := &batchRecorder{}
	b := newSSEBatcher(rec.write, rec.flush, 1, sseBatchMaxDelay, clock.after)

	for _, ln := range []string{"data: a\n\n", "data: b\n\n"} {
		if err := b.Write(ln); err != nil {
			t.Fatalf("Write %q: %v", ln, err)
		}
	}
	if got := rec.delivered(); len(got) != 2 {
		t.Fatalf("delivered %d lines, want 2 immediately", len(got))
	}
	if rec.flushes != 2 {
		t.Fatalf("flushes = %d, want 2 (one per token)", rec.flushes)
	}
}

// TestSSEBatcherRealTimerBound: the production constructor
// (real time.AfterFunc timers) delivers a lone token promptly.
// The bound is 8 ms; the assertion is loose (200 ms) because
// this shared VM's scheduler is noisy — the exact 8 ms bound
// is what the fake-clock tests above prove. The measured delay
// is logged so a slow run is visible, not hidden.
func TestSSEBatcherRealTimerBound(t *testing.T) {
	rec := &batchRecorder{}
	b := newSSEBatcher(rec.write, rec.flush, sseBatchMaxTokens, sseBatchMaxDelay, nil)

	start := time.Now()
	if err := b.Write("data: real\n\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		if len(rec.delivered()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lone token not delivered within 200 ms (real timer)")
		}
		time.Sleep(time.Millisecond)
	}
	elapsed := time.Since(start)
	t.Logf("lone token delivered after %v (bound 8 ms + scheduler noise)", elapsed)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("delivery took %v, want < 200 ms", elapsed)
	}
}
