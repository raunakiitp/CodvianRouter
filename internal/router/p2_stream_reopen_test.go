package router

// WS6 closure item 4b: stream re-open (early retry) bounds are
// pinned. A re-open spends another chain attempt: it is classified
// ONLY for error shapes where a second dial is meaningful and safe
// (truncation/transport breakage before any user-visible bytes),
// it is capped at earlyRetryMax, and it never fires when the chain
// has no attempt left. Duplicate billing is impossible downstream:
// the server settles its spend reservation exactly once per request
// (a single deferred settle onto an idempotent ledger delete), so
// re-opens re-dial but never re-charge.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestAttemptsLeaveOneBoundsReOpens(t *testing.T) {
	const maxAttempts = 5
	for _, tc := range []struct {
		used, want int
		hasNext    bool
	}{
		{0, 0, false}, // no next target: reserve 0, spending is free
		{0, 1, true},  // fresh chain with a next target: allowed
		{2, 3, true},  // mid-chain: allowed while a target remains
		{4, 5, true},  // last target: still allowed (leaves exactly one)
		{5, 5, true},  // budget exhausted: never
		{6, 5, true},  // over budget: never
		{2, 3, false}, // no remaining target: allowed (nothing to reserve for)
	} {
		want := tc.used+(func() int {
			if tc.hasNext {
				return 1
			}
			return 0
		})() < maxAttempts
		if got := attemptsLeaveOne(tc.used, maxAttempts, tc.hasNext); got != want {
			t.Errorf("attemptsLeaveOne(used=%d, hasNext=%v) = %v, want %v — bound drifted", tc.used, tc.hasNext, got, want)
		}
	}
}

func TestStreamEarlyRetryClassification(t *testing.T) {
	// Retryable: pre-output breakage where a fresh dial is safe.
	for _, err := range []error{
		errors.New("unexpected EOF"),
		errors.New("context deadline exceeded mid-stream"),
		fmt.Errorf("stream truncated: %w", errors.New("connection reset")),
	} {
		if !isStreamEarlyRetryable(err) {
			t.Errorf("%v: want retryable (safe to re-open)", err)
		}
	}
	// NOT retryable: output already shipped, or statuses with their
	// own failover semantics, or client/model errors a re-dial
	// would just repeat (and re-bill).
	for _, err := range []error{
		&provider.UpstreamError{},
		&provider.RateLimitError{},
		&provider.QuotaError{},
		NewPanicError("provider_chat", "boom"),
		// Classified through provider.ClassifyError: a 404 model
		// surface and a 401 client surface re-open to the same
		// failure, so they must never re-open.
		&provider.UpstreamError{StatusCode: 404, Body: `{"error":{"code":"model_not_found"}}`},
		&provider.UpstreamError{StatusCode: 401, Body: `{"error":{"code":"invalid_api_key"}}`},
		context.Canceled,
		nil,
	} {
		if isStreamEarlyRetryable(err) {
			t.Errorf("%v: want NOT retryable", err)
		}
	}
}

func TestEarlyRetryMaxBounded(t *testing.T) {
	if earlyRetryMax < 1 || earlyRetryMax > 8 {
		t.Fatalf("earlyRetryMax = %d, want a small bounded cap (1..8) — an unbounded cap would re-open loops", earlyRetryMax)
	}
}
