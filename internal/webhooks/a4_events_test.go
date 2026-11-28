package webhooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A4 item 1/4: every event name in the canonical set must actually be
// deliverable to a subscriber. This subscribes one endpoint to the
// whole KnownEvents() set, fires each event, and asserts every one
// arrives — a subscribable-but-never-deliverable event is a fake
// capability, so this fails the moment a name is added to the set
// without a delivery path (or removed from the dispatcher's matching).
func TestKnownEventsAllDeliver(t *testing.T) {
	events := KnownEvents()
	if len(events) < 11 {
		t.Fatalf("KnownEvents() has %d entries, want at least 11 (quota/budget/provider/request/eval/test)", len(events))
	}
	var mu sync.Mutex
	got := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.Header.Get("X-Codvian-Event")]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New([]Endpoint{{ID: "all", URL: srv.URL, Events: events}}, srv.Client(), nil)
	for _, ev := range events {
		d.Notify(ev, map[string]any{"event_under_test": ev})
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		var missing []string
		for _, ev := range events {
			if got[ev] == 0 {
				missing = append(missing, ev)
			}
		}
		mu.Unlock()
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("events never delivered to their subscriber: %v (delivered: %v)", missing, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A4 item 4: the test-send path must deliver to the named endpoint
// even when that endpoint does not subscribe to the event, and must
// report an unknown endpoint as not delivered.
func TestNotifyEndpointIgnoresSubscription(t *testing.T) {
	var mu sync.Mutex
	var delivered []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delivered = append(delivered, r.Header.Get("X-Codvian-Event"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The endpoint subscribes to quota.exceeded only.
	d := New([]Endpoint{{ID: "ep1", URL: srv.URL, Events: []string{QuotaExceededEvent}}}, srv.Client(), nil)
	if ok := d.NotifyEndpoint("ep1", TestPingEvent, map[string]any{"test": true}); !ok {
		t.Fatal("NotifyEndpoint(ep1) = false, want true")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(delivered)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test.ping was not delivered to the endpoint despite an explicit test-send")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	if delivered[0] != TestPingEvent {
		t.Errorf("delivered event = %q, want %q", delivered[0], TestPingEvent)
	}
	mu.Unlock()

	if ok := d.NotifyEndpoint("does-not-exist", TestPingEvent, nil); ok {
		t.Error("NotifyEndpoint(unknown) = true, want false")
	}
	var nilD *Dispatcher
	if ok := nilD.NotifyEndpoint("x", TestPingEvent, nil); ok {
		t.Error("nil dispatcher NotifyEndpoint = true, want false")
	}
}

// A4 item 4: the delivered test.ping envelope carries the example
// data the test-send endpoint supplies, under the standard
// {event,timestamp,data} shape.
func TestNotifyEndpointPayloadShape(t *testing.T) {
	var mu sync.Mutex
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		mu.Lock()
		raw = buf
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := New([]Endpoint{{ID: "ep1", URL: srv.URL, Events: []string{TestPingEvent}}}, srv.Client(), nil)
	d.NotifyEndpoint("ep1", TestPingEvent, map[string]any{"test": true, "webhook_id": "wh_x"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		have := raw != nil && len(raw) > 0
		mu.Unlock()
		if have {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no test.ping delivery arrived")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("delivered body is not JSON: %v (%s)", err, raw)
	}
	if env["event"] != TestPingEvent {
		t.Errorf("event = %v, want %q", env["event"], TestPingEvent)
	}
	data, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want an object", env["data"])
	}
	if data["test"] != true || data["webhook_id"] != "wh_x" {
		t.Errorf("data = %v, want {test:true, webhook_id:wh_x}", data)
	}
}
