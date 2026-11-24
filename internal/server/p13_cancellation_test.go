// AUD-PERF-001 / AUD-SEC-002: a client disconnect must CANCEL the
// upstream call, not leave it running. An upstream that keeps generating
// (and billing for) tokens after the client has gone is both a resource
// leak and a spend leak, and the relayed-spend row must not claim the
// abandoned request was a clean success.
//
// Also the §3.4 interaction check: the relay path is single-attempt, so
// there is no retry to duplicate a charge or to amplify a provider's
// cost after a non-retryable failure.
package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAUD_PERF_001ClientDisconnectCancelsTheUpstreamCall(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCancelled := make(chan struct{})
	var hb atomic.Int64

	mock := &anthropicMock{hook: func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// One real event, then hold the stream open the way a slow
		// provider would, counting heartbeats until the caller goes away.
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-5\"}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(upstreamStarted)
		for {
			select {
			case <-r.Context().Done():
				close(upstreamCancelled)
				return true
			case <-time.After(20 * time.Millisecond):
				hb.Add(1)
				if hb.Load() > 250 { // 5s fail-safe: never hang the suite
					return true
				}
				fmt.Fprint(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
	}}

	srv, led, _ := p13RelaySpend(t, mock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raw := `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"stream please"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer test-key-123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-upstreamStarted:
	case <-done:
		t.Fatalf("the handler returned before reaching the upstream: status %d, upstream calls seen %d, body %s", rec.Code, hb.Load(), rec.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("the upstream was never reached: status %d body %s", rec.Code, rec.Body.String())
	}
	// The client goes away.
	cancel()

	select {
	case <-upstreamCancelled:
	case <-time.After(5 * time.Second):
		t.Fatalf("the upstream call was NOT cancelled after the client disconnected (heartbeats sent: %d): it would keep generating and billing tokens", hb.Load())
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return after the client disconnected")
	}

	// The abandoned request must not be recorded as a clean success, and
	// it must not be recorded as a zero-cost one either.
	rows := p13RelayRows(t, led)
	if len(rows) > 1 {
		t.Fatalf("rows = %d, want at most 1 (no duplicate charge for one request)", len(rows))
	}
	if len(rows) == 1 {
		if rows[0].OK {
			t.Error("a request abandoned by the client was recorded as a successful call")
		}
		if rows[0].CostUSD < 0 {
			t.Errorf("cost = %f, want a non-negative number", rows[0].CostUSD)
		}
	}
}

// TestAUD_REL_001SingleAttemptNoRetryAmplification pins the §3.4 property
// that the relayed path carries no retry loop: a provider failure is
// reported once, so a non-retryable failure (a quota wall, a 4xx) cannot
// be walked into repeatedly and cannot multiply spend.
func TestAUD_REL_001SingleAttemptNoRetryAmplification(t *testing.T) {
	var calls atomic.Int64
	mock := &anthropicMock{status: http.StatusTooManyRequests, hook: func(w http.ResponseWriter, r *http.Request) bool {
		calls.Add(1)
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
		return true
	}}
	srv, led, _ := p13RelaySpend(t, mock)

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13RelayMsgBody, "test-key-123")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 relayed from the upstream (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want the upstream's own 7s propagated", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1: a non-retryable failure must not be retried", got)
	}
	for _, r := range p13RelayRows(t, led) {
		if r.CostUSD > 0 {
			t.Errorf("a failed call was charged %.8f: a provider failure must not create a successful spend entry", r.CostUSD)
		}
	}
}
