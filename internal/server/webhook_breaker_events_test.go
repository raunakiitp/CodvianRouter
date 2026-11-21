package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// T7: the breaker->webhook adapter ships the two provider-health events
// to a real local receiver (like main.go wires), the payload carries
// only the provider id and the real counts — the signing secret never
// appears anywhere in the body or headers beyond the HMAC signature —
// and an unsubscribed endpoint receives nothing. A server with no
// webhooks wired returns an adapter that is a silent no-op.
func TestBreakerWebhookEventsDeliverToLocalReceiver(t *testing.T) {
	const secret = "top-s3cr3t"
	var mu sync.Mutex
	var degradedBody, recoveredBody []byte
	var events []string
	gotDegradedEvent, gotRecoveredEvent := "", ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		events = append(events, r.Header.Get("X-Codvian-Event"))
		switch r.Header.Get("X-Codvian-Event") {
		case webhooks.ProviderDegradedEvent:
			degradedBody = body
			gotDegradedEvent = r.Header.Get("X-Codvian-Event")
		case webhooks.ProviderRecoveredEvent:
			recoveredBody = body
			gotRecoveredEvent = r.Header.Get("X-Codvian-Event")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := webhooks.New([]webhooks.Endpoint{
		{ID: "ops", URL: srv.URL, Events: []string{webhooks.ProviderDegradedEvent, webhooks.ProviderRecoveredEvent}, Secret: secret},
		{ID: "other", URL: srv.URL, Events: []string{webhooks.QuotaExceededEvent}},
	}, srv.Client(), nil)
	adapter := &breakerWebhookEvents{w: d}

	adapter.ProviderDegraded("anthropic", 7, 2, 9000*time.Millisecond)
	adapter.ProviderRecovered("anthropic")

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		done := len(events) >= 2
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let any wrongly-subscribed endpoint fire too
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("events = %v, want exactly the 2 subscribed provider-health events (unsubscribed quota endpoint silent)", events)
	}
	if gotDegradedEvent != webhooks.ProviderDegradedEvent || gotRecoveredEvent != webhooks.ProviderRecoveredEvent {
		t.Fatalf("got %q / %q, want %q / %q", gotDegradedEvent, gotRecoveredEvent, webhooks.ProviderDegradedEvent, webhooks.ProviderRecoveredEvent)
	}
	if degradedBody == nil || recoveredBody == nil {
		t.Fatal("a provider-health delivery did not arrive")
	}
	// Payload safety: the secret must never appear in either body, and
	// only the provider id plus real counts ride the degraded payload.
	for name, body := range map[string][]byte{"degraded": degradedBody, "recovered": recoveredBody} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("webhook signing secret leaked into %s payload: %s", name, body)
		}
	}
	var degraded map[string]any
	if err := json.Unmarshal(degradedBody, &degraded); err != nil {
		t.Fatalf("degraded body not JSON: %v", err)
	}
	data := degraded["data"].(map[string]any)
	if data["provider"] != "anthropic" || data["consecutive_failures"] != float64(7) ||
		data["trips"] != float64(2) || data["open_for_ms"] != float64(9000) {
		t.Errorf("degraded data = %v, want provider anthropic / 7 / 2 / 9000", data)
	}
	for k := range data {
		switch k {
		case "provider", "consecutive_failures", "trips", "open_for_ms":
		default:
			t.Errorf("degraded payload carries unexpected field %q", k)
		}
	}
}

func TestBreakerWebhookEventsNilDispatcherIsNoop(t *testing.T) {
	// A server created without webhooks returns a nil-dispatcher
	// adapter; both calls must not panic and must deliver nothing.
	a := (&breakerWebhookEvents{w: nil})
	a.ProviderDegraded("p", 1, 1, time.Second)
	a.ProviderRecovered("p")
}
