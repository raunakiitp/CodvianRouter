// SSE delivery batching (CHECKLIST items 135-136).
//
// The server's SSE writer used to Flush() after every single token
// event. That is one write syscall + one TCP segment per token —
// cheap on localhost, wasteful at real token rates, and it makes
// the server pay a flush for tokens that arrive back-to-back from
// the upstream. This batcher coalesces events into fewer flushes
// with a hard hybrid bound, whichever comes first:
//
//   - at most sseBatchMaxTokens (3) events per flush, or
//   - at most sseBatchMaxDelay (8 ms) after the first pending event.
//
// A token is never held longer than 8 ms and never batched with
// more than 2 others. [DONE] and error events never go through
// the batcher at all (see handleChatStream's writeSSENow): a
// stream close is never delayed by up to 8 ms.
//
// The bound is enforced by a timer armed when the first event of
// a batch lands, not by a periodic ticker: a quiet stream pays
// one timer per batch, and a token that arrives alone still
// leaves within 8 ms. The timer is injectable (the after func)
// so the bounds are unit-testable deterministically with a fake
// clock instead of sleeping and hoping the scheduler cooperates.
//
// Batching is a throughput trade, not a latency win, and the
// measurement says so: see scripts/bench-sse.sh and
// docs/sse-delivery-2026-11-18.json. At a 10 ms upstream cadence
// each token waits the full 8 ms timer (its successor is 10 ms
// away, so the 3-token bound never fires first); at a 1 ms
// cadence the 3-token bound fires first and the wait is ~2 ms.
// The batcher exists to cut flushes ~3x under fast streams, not
// to make a slow stream faster — the dashboard shows the measured
// before/after numbers with no target verdict because there is
// no latency target for token delivery yet.
package server

import (
	"sync"
	"time"
)

// Hybrid batching bounds for SSE token delivery.
const (
	// sseBatchMaxTokens is the most events one Flush carries.
	sseBatchMaxTokens = 3
	// sseBatchMaxDelay is the longest a token waits for company.
	sseBatchMaxDelay = 8 * time.Millisecond
)

// batchTimer is the minimal timer surface the batcher needs:
// arm one callback, cancel it if the batch flushes early.
type batchTimer interface {
	Stop() bool
}

// realBatchTimer adapts time.AfterFunc to batchTimer.
func realBatchAfter(d time.Duration, f func()) batchTimer {
	return time.AfterFunc(d, f)
}

// sseBatchConfig is the server's batching switch. Enabled by
// default; CODVIAN_ROUTER_SSE_BATCH=0 disables it (measurement
// only — the before/after comparison in scripts/bench-sse.sh).
type sseBatchConfig struct {
	enabled   bool
	maxTokens int
	maxDelay  time.Duration
}

// sseBatcher coalesces SSE event lines into fewer Flush calls.
// It is safe for concurrent use, though handleChatStream calls
// it from a single goroutine. All writes happen under the mutex
// so events can never interleave or reorder across a timer
// callback and a Write racing each other.
type sseBatcher struct {
	mu        sync.Mutex
	writeLine func(string) error // writes one SSE line, no flush
	flush     func()             // flushes the underlying writer
	maxTokens int
	maxDelay  time.Duration
	after     func(time.Duration, func()) batchTimer
	pending   []string
	timer     batchTimer
	closed    bool
}

// newSSEBatcher builds a batcher. after may be nil, in which
// case time.AfterFunc is used. maxTokens < 1 means 1 (every
// event flushes immediately); maxDelay <= 0 means the timer
// fires at once, which is also effectively immediate.
func newSSEBatcher(writeLine func(string) error, flush func(), maxTokens int, maxDelay time.Duration, after func(time.Duration, func()) batchTimer) *sseBatcher {
	if maxTokens < 1 {
		maxTokens = 1
	}
	if after == nil {
		after = realBatchAfter
	}
	return &sseBatcher{
		writeLine: writeLine,
		flush:     flush,
		maxTokens: maxTokens,
		maxDelay:  maxDelay,
		after:     after,
	}
}

// Write queues one SSE event line. It flushes synchronously
// when the batch reaches maxTokens, and arms the maxDelay timer
// when the batch goes from empty to one pending event.
func (b *sseBatcher) Write(line string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		// Closed means the stream is ending: never hold a late
		// event, write it straight through.
		return b.writeLine(line)
	}
	b.pending = append(b.pending, line)
	if len(b.pending) >= b.maxTokens {
		return b.flushLocked()
	}
	if len(b.pending) == 1 && b.timer == nil {
		b.timer = b.after(b.maxDelay, b.onTimer)
	}
	return nil
}

// Flush delivers every pending event now, whatever the bounds
// say. [DONE] and error events are flushed this way by the
// caller, never through Write.
func (b *sseBatcher) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flushLocked()
}

// Close flushes any pending events and marks the batcher
// closed; later Writes go straight through. It is safe to
// call more than once.
func (b *sseBatcher) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	err := b.flushLocked()
	b.closed = true
	return err
}

// onTimer runs when the maxDelay timer fires: the oldest
// pending event has waited long enough, so the batch goes out
// even if it holds fewer than maxTokens events.
func (b *sseBatcher) onTimer() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.timer = nil // already fired; flushLocked must not Stop it
	_ = b.flushLocked()
}

// flushLocked writes every pending line in order, then flushes
// the underlying writer once. The caller holds b.mu. An empty
// pending list is a no-op — in particular it never calls the
// underlying Flush, so a stale timer firing after an early
// count-flush costs nothing.
func (b *sseBatcher) flushLocked() error {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	if len(b.pending) == 0 {
		return nil
	}
	lines := b.pending
	b.pending = nil
	var err error
	for _, ln := range lines {
		if werr := b.writeLine(ln); werr != nil {
			err = werr
			break
		}
	}
	b.flush()
	return err
}
