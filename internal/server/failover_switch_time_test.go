// TestChatCompletionsFailoverSwitchTime measures the TRUE failover-switch
// time (parity tracker Area 12 row 18, WRONG): the gap between the dead attempt's
// 429 response being written and the fallback attempt's request arriving at
// the fallback mock — router decision + dispatch, not end-to-end latency.
// The old quoted figures ("~53 ms failover / 1–2 ms switch") were
// mislabelled: 52.4–55.8 ms is end-to-end latency with a 50 ms upstream,
// and 1–2 ms is router overhead vs direct, not a switch time.
//
// The mocks run in-process, so both timestamps share one clock; the gap is
// the switch time plus loopback RTT (sub-millisecond). Target: the switch
// promise, failover in 0.1–0.3 s — the test fails above 300 ms.
package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

func TestChatCompletionsFailoverSwitchTime(t *testing.T) {
	var deadRespondedAt, fallbackReceivedAt atomic.Value // time.Time
	var deadHits, fallbackHits atomic.Int32

	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadHits.Add(1)
		deadRespondedAt.Store(time.Now())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited by dead-model","type":"server_error"}}`))
	}))
	defer deadSrv.Close()

	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackReceivedAt.Store(time.Now())
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-fallback","object":"chat.completion","model":"gate-model","choices":[{"message":{"role":"assistant","content":"fallback says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}))
	defer fallbackSrv.Close()

	deadClient, err := provider.NewClient(deadSrv.URL, "dead-key", provider.WithTimeout(2*time.Second), provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient dead: %v", err)
	}
	fallbackClient, err := provider.NewClient(fallbackSrv.URL, "fallback-key", provider.WithTimeout(2*time.Second), provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient fallback: %v", err)
	}
	eng := router.NewEngine(map[string]provider.Client{"dead": deadClient, "fallback": fallbackClient}, &failoverGateLedger{}, router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1, // hedging off: deterministic attempt order
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			if model != "gate-model" {
				return nil
			}
			return []router.Attempt{{ProviderID: "dead", Model: "gate-model"}, {ProviderID: "fallback", Model: "gate-model"}}
		},
	})
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithTimeout(10*time.Second))

	resp := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"gate-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.Code, resp.Body.String())
	}
	if got := deadHits.Load(); got != 1 {
		t.Fatalf("dead mock hit %d times, want 1", got)
	}
	if got := fallbackHits.Load(); got != 1 {
		t.Fatalf("fallback mock hit %d times, want 1", got)
	}
	deadAt, ok := deadRespondedAt.Load().(time.Time)
	if !ok {
		t.Fatal("dead mock never recorded a response time")
	}
	fallbackAt, ok := fallbackReceivedAt.Load().(time.Time)
	if !ok {
		t.Fatal("fallback mock never recorded a receipt time")
	}
	switchTime := fallbackAt.Sub(deadAt)
	t.Logf("failover switch time: %v (dead-429 response -> fallback dispatch, localhost mocks)", switchTime)
	if switchTime < 0 {
		t.Fatalf("switch time %v is negative — clock/timestamp bug, not a measurement", switchTime)
	}
	if switchTime > 300*time.Millisecond {
		t.Fatalf("failover switch time %v, want < 300 ms (the 0.1–0.3 s switch promise)", switchTime)
	}
}
