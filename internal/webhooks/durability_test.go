package webhooks

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A4 item 5 (required durability test): a delivery that fails is
// persisted, and after a simulated restart (a fresh dispatcher + queue
// loaded from the same file) it is redelivered and recorded delivered.
func TestQueueDurabilityAcrossRestart(t *testing.T) {
	noRetryBackoff(t)
	qpath := filepath.Join(t.TempDir(), "webhooks-queue.json")

	// First "process": the endpoint always fails (500).
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	q1, err := OpenQueue(qpath)
	if err != nil {
		t.Fatal(err)
	}
	d1 := New([]Endpoint{{ID: "ep", URL: fail.URL, Events: []string{QuotaExceededEvent}}}, fail.Client(), nil)
	d1.SetQueue(q1)
	d1.Notify(QuotaExceededEvent, map[string]any{"reason": "budget"})

	// Wait until the failed delivery is recorded as pending (attempted).
	deadline := time.Now().Add(5 * time.Second)
	for {
		p := q1.Pending()
		if len(p) == 1 && p[0].Attempts >= 1 && p[0].Status == DeliveryFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery was not recorded as a failed pending item: %+v", q1.Pending())
		}
		time.Sleep(10 * time.Millisecond)
	}
	fail.Close()

	// "Restart": reopen the queue from disk and a new dispatcher whose
	// endpoint now answers 200.
	q2, err := OpenQueue(qpath)
	if err != nil {
		t.Fatal(err)
	}
	if len(q2.Pending()) != 1 {
		t.Fatalf("pending queue not recovered after restart: %+v", q2.Pending())
	}
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	d2 := New([]Endpoint{{ID: "ep", URL: ok.URL, Events: []string{QuotaExceededEvent}}}, ok.Client(), nil)
	d2.SetQueue(q2)
	if n := d2.RedeliverPending(); n != 1 {
		t.Fatalf("RedeliverPending attempted %d, want 1", n)
	}
	if got := q2.Pending(); len(got) != 0 {
		t.Fatalf("delivery still pending after successful redelivery: %+v", got)
	}
	hist := q2.History(10)
	found := false
	for _, h := range hist {
		if h.Status == DeliveryDelivered && h.Event == QuotaExceededEvent {
			found = true
		}
	}
	if !found {
		t.Fatalf("history has no delivered record after redelivery: %+v", hist)
	}
}

// A4 item 5 (required): 10 consecutive failed deliveries auto-disable
// the webhook, and a disabled endpoint stops receiving attempts.
func TestAutoDisableAfterTenFailures(t *testing.T) {
	noRetryBackoff(t)
	var hits int32
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fail.Close()

	d := New([]Endpoint{{ID: "ep", URL: fail.URL, Events: []string{QuotaExceededEvent}}}, fail.Client(), nil)
	d.SetQueue(mustQueue(t, ""))
	d.SetAutoDisableThreshold(10)

	for i := 0; i < 10; i++ {
		d.Notify(QuotaExceededEvent, map[string]any{"i": i})
	}
	deadline := time.Now().Add(8 * time.Second)
	for !d.Disabled("ep") {
		if time.Now().After(deadline) {
			t.Fatalf("endpoint not auto-disabled after 10 failures (history %d)", len(d.Queue().History(0)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The history carries the failures.
	if h := d.Queue().History(0); len(h) < 10 {
		t.Errorf("delivery history has %d records, want >= 10", len(h))
	}

	before := atomic.LoadInt32(&hits)
	d.Notify(QuotaExceededEvent, map[string]any{"after": true})
	time.Sleep(300 * time.Millisecond)
	if after := atomic.LoadInt32(&hits); after != before {
		t.Errorf("auto-disabled endpoint still received %d request(s)", after-before)
	}
}

// A4 item 5: a "*" subscription receives every event.
func TestWildcardSubscription(t *testing.T) {
	var got int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&got, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := New([]Endpoint{{ID: "all", URL: srv.URL, Events: []string{"*"}}}, srv.Client(), nil)
	d.Notify(RequestCompletedEvent, map[string]any{"model": "m"})
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&got) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("wildcard subscription received no delivery")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Queue persistence round-trips pending items + history.
func TestQueuePersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.json")
	q, err := OpenQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	d := &Delivery{EndpointID: "ep", Event: QuotaExceededEvent, URL: "http://x", Body: []byte("body")}
	if err := q.Enqueue(d); err != nil {
		t.Fatal(err)
	}
	q2, err := OpenQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(q2.Pending()) != 1 || q2.Pending()[0].Event != QuotaExceededEvent {
		t.Fatalf("pending did not survive reopen: %+v", q2.Pending())
	}
	if err := q2.Complete(q2.Pending()[0].ID, 200, 5, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(q2.Pending()) != 0 {
		t.Fatalf("delivered item still pending: %+v", q2.Pending())
	}
	if h := q2.History(5); len(h) != 1 || h[0].Status != DeliveryDelivered {
		t.Fatalf("history = %+v, want one delivered record", h)
	}
}

func mustQueue(t *testing.T, path string) *Queue {
	t.Helper()
	q, err := OpenQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	return q
}
