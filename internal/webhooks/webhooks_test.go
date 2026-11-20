package webhooks

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The dispatcher delivers asynchronously: the test server
// collects deliveries on a channel and the test waits for them.
func TestDispatcherDeliversSignedPayload(t *testing.T) {
	var mu sync.Mutex
	var raw []byte
	var deliveries int
	var gotSig, gotEvent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		raw = body
		deliveries++
		gotSig = r.Header.Get("X-Codvian-Signature-256")
		gotEvent = r.Header.Get("X-Codvian-Event")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New([]Endpoint{
		{ID: "ops", URL: srv.URL, Events: []string{QuotaExceededEvent}, Secret: "s3cr3t"},
		{ID: "other", URL: srv.URL, Events: []string{"some.other.event"}},
	}, srv.Client(), nil)
	d.Notify(QuotaExceededEvent, map[string]any{
		"key_id": "k1", "pool_id": "pool1", "reason": "schedule-budget",
		"quota": "daily_requests", "used": 91.0, "limit": 90.0, "pct": 101.1,
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		delivered := raw != nil
		mu.Unlock()
		if delivered || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give a wrongly-subscribed second endpoint time to fire too.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if deliveries != 1 {
		t.Fatalf("deliveries = %d, want 1 (only the subscribed endpoint)", deliveries)
	}
	if raw == nil {
		t.Fatal("no delivery arrived")
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("webhook body is not JSON: %v", err)
	}
	if env["event"] != QuotaExceededEvent {
		t.Errorf("event = %v, want %q", env["event"], QuotaExceededEvent)
	}
	data, ok := env["data"].(map[string]any)
	if !ok || data["reason"] != "schedule-budget" || data["pool_id"] != "pool1" {
		t.Errorf("data = %v, want reason schedule-budget / pool1", env["data"])
	}
	if gotEvent != QuotaExceededEvent {
		t.Errorf("X-Codvian-Event = %q, want %q", gotEvent, QuotaExceededEvent)
	}
	// The signature must be HMAC-SHA256 over the exact body.
	mac := hmac.New(sha256.New, []byte("s3cr3t"))
	mac.Write(raw)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); gotSig != want {
		t.Errorf("signature = %q, want %q", gotSig, want)
	}
}

func TestDispatcherNilIsNoop(t *testing.T) {
	var d *Dispatcher
	d.Notify(QuotaExceededEvent, map[string]any{"reason": "budget"})
}

func TestDispatcherDownEndpointDoesNotBlock(t *testing.T) {
	// Zero the backoff so the retry sequence finishes in
	// milliseconds instead of ~7s (production 1s/2s/4s), and
	// wait for the delivery goroutine to finish before the
	// test returns: deliver reads webhookRetryBackoff on
	// every retry, so a goroutine that outlives this test
	// races the next test's noRetryBackoff swap (the race
	// detector fires it in TestDispatcherRetries5xxThen-
	// Succeeds). The terminal "delivery failed after"
	// log line is written just before deliver returns, so
	// once it is seen no further backoff read can happen.
	noRetryBackoff(t)
	finished := make(chan struct{})
	var once sync.Once
	logger := log.New(logFuncWriter(func(p []byte) (int, error) {
		if bytes.Contains(p, []byte("delivery failed after")) {
			once.Do(func() { close(finished) })
		}
		return len(p), nil
	}), "", 0)
	// No server listens on this port: delivery fails, Notify
	// must still return immediately.
	d := New([]Endpoint{{ID: "down", URL: "http://127.0.0.1:1/hook", Events: []string{QuotaExceededEvent}}}, nil, logger)
	done := make(chan struct{})
	go func() {
		d.Notify(QuotaExceededEvent, map[string]any{"reason": "budget"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked on a down endpoint")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery goroutine did not finish its retry sequence")
	}
}

// logFuncWriter adapts a func to io.Writer so a test can
// watch for a log line.
type logFuncWriter func([]byte) (int, error)

func (f logFuncWriter) Write(p []byte) (int, error) { return f(p) }

// noRetryBackoff shrinks the retry delay to zero for the retry
// tests below (the production 1s/2s/4s backoff would make them
// take ~7s each); restored after the test.
func noRetryBackoff(t *testing.T) {
	t.Helper()
	orig := webhookRetryBackoff
	webhookRetryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { webhookRetryBackoff = orig })
}

// waitForAttempts polls until the server has seen n attempts
// (or the deadline passes), then waits a little longer and
// fails if more attempts arrive — a retry loop that does not
// stop would keep counting.
func waitForAttempts(t *testing.T, mu *sync.Mutex, count *int, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := *count
		mu.Unlock()
		if got >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if *count != n {
		t.Fatalf("attempts = %d, want exactly %d", *count, n)
	}
}

func TestDispatcherRetries5xxThenSucceeds(t *testing.T) {
	noRetryBackoff(t)
	var mu sync.Mutex
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := New([]Endpoint{{ID: "flaky", URL: srv.URL, Events: []string{QuotaExceededEvent}}}, srv.Client(), nil)
	d.Notify(QuotaExceededEvent, map[string]any{"reason": "budget"})
	waitForAttempts(t, &mu, &attempts, 3)
}

func TestDispatcherRetriesExhaustedStopsAfterFourAttempts(t *testing.T) {
	noRetryBackoff(t)
	var mu sync.Mutex
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	d := New([]Endpoint{{ID: "down", URL: srv.URL, Events: []string{QuotaExceededEvent}}}, srv.Client(), nil)
	d.Notify(QuotaExceededEvent, map[string]any{"reason": "budget"})
	// 1 first attempt + 3 retries = 4 attempts, then it stops.
	waitForAttempts(t, &mu, &attempts, 4)
}

func TestDispatcherDoesNotRetry4xx(t *testing.T) {
	noRetryBackoff(t)
	var mu sync.Mutex
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	d := New([]Endpoint{{ID: "rejecting", URL: srv.URL, Events: []string{QuotaExceededEvent}}}, srv.Client(), nil)
	d.Notify(QuotaExceededEvent, map[string]any{"reason": "budget"})
	// A 4xx rejects the payload itself: one attempt, no retry.
	waitForAttempts(t, &mu, &attempts, 1)
}

func TestDispatcherRetriesTransportError(t *testing.T) {
	noRetryBackoff(t)
	var mu sync.Mutex
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		// Close the connection without a response: the
		// client sees a transport error, which is retryable.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	}))
	defer srv.Close()
	d := New([]Endpoint{{ID: "resetting", URL: srv.URL, Events: []string{QuotaExceededEvent}}}, srv.Client(), nil)
	d.Notify(QuotaExceededEvent, map[string]any{"reason": "budget"})
	waitForAttempts(t, &mu, &attempts, 4)
}
