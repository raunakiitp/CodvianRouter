package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// A4 item 3: the registration API's known-event set and the
// config-file's known-event set are IDENTICAL (both delegate to
// webhooks.KnownEvents). This fails the moment either side re-hardcodes
// a shorter list.
func TestWebhookKnownEventsMatchesConfigSet(t *testing.T) {
	api := append([]string(nil), webhookKnownEvents...)
	cfg := append([]string(nil), config.KnownWebhookEvents()...)
	if len(api) < 11 {
		t.Fatalf("API known-event set has %d entries, want at least 11 (incl. the 3 eval events and test.ping)", len(api))
	}
	sort.Strings(api)
	sort.Strings(cfg)
	if strings.Join(api, ",") != strings.Join(cfg, ",") {
		t.Fatalf("API known set %v != config known set %v", api, cfg)
	}
}

// A4 item 1: every event in the known set — including eval.completed,
// eval.run.failed and eval.regression — must be accepted by the
// registration API. Before the fix these three were a 400 ("not a
// known webhook event"), so the Notify calls in the eval scheduler
// could never reach a subscriber.
func TestWebhookRegistrationAcceptsEveryKnownEvent(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	for _, ev := range webhookKnownEvents {
		body := `{"url":"https://93.184.215.10/hook","events":["` + ev + `"]}`
		rec := callWebhookHandler(t, srv, "POST", "/api/webhooks", body)
		if rec.Code != http.StatusCreated {
			t.Errorf("register known event %q: status %d, want 201: %s", ev, rec.Code, rec.Body.String())
		}
	}
}

// A4 item 1 + required test: the three eval events are subscribable
// through the registration API by their LITERAL names. Before the fix
// each was a 400 ("not a known webhook event"), so the scheduler's
// Notify calls could never reach a subscriber. The names are spelled
// out here (not iterated from the known set) so reverting the known
// set to a shorter list fails this test.
func TestEvalEventsAreSubscribableViaAPI(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	for _, ev := range []string{webhooks.EvalCompletedEvent, webhooks.EvalRunFailedEvent, webhooks.EvalRegressionEvent} {
		body := `{"url":"https://93.184.215.10/hook","events":["` + ev + `"]}`
		rec := callWebhookHandler(t, srv, "POST", "/api/webhooks", body)
		if rec.Code != http.StatusCreated {
			t.Errorf("register eval event %q: status %d, want 201: %s", ev, rec.Code, rec.Body.String())
		}
	}
}

// waitDeliveries drains up to n deliveries (or until the deadline),
// returning the parsed {event, data} envelopes received.
func waitDeliveries(t *testing.T, recv *mockWebhookReceiver, n int, within time.Duration) []map[string]any {
	t.Helper()
	var out []map[string]any
	deadline := time.After(within)
	for len(out) < n {
		select {
		case body := <-recv.ch:
			var env map[string]any
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("delivered body is not JSON: %v (%s)", err, body)
			}
			out = append(out, env)
		case <-deadline:
			return out
		}
	}
	return out
}

// A4 required test: an emit test PER event — one endpoint subscribed
// to the whole canonical set receives every one of the gateway's
// events through the live dispatcher.
func TestEveryKnownEventDeliversToSubscriber(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	recv := &mockWebhookReceiver{ch: make(chan []byte, 64)}
	mock := httptest.NewServer(recv)
	defer mock.Close()

	events := append([]string(nil), webhookKnownEvents...)
	s.cfg.Webhooks = []config.WebhookConfig{{ID: "wh-all", URL: mock.URL, Events: events}}
	srv.webhookClient = mock.Client()
	srv.rebuildWebhookDispatcher()

	for _, ev := range events {
		srv.webhooks.Notify(ev, map[string]any{"event_under_test": ev})
	}

	got := map[string]bool{}
	for _, env := range waitDeliveries(t, recv, len(events), 8*time.Second) {
		if ev, _ := env["event"].(string); ev != "" {
			got[ev] = true
		}
	}
	var missing []string
	for _, ev := range events {
		if !got[ev] {
			missing = append(missing, ev)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("events never delivered to their subscriber: %v", missing)
	}
}

// A4 item 4 + required test: the test-send endpoint emits test.ping
// with example data, to the named endpoint — even though that endpoint
// does not subscribe to test.ping. Before the endpoint existed there
// was no way to emit test.ping at all.
func TestTestSendEmitsTestPing(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	recv := &mockWebhookReceiver{ch: make(chan []byte, 4)}
	mock := httptest.NewServer(recv)
	defer mock.Close()

	s.cfg.Webhooks = []config.WebhookConfig{{ID: "wh-test", URL: mock.URL, Events: []string{webhooks.QuotaExceededEvent}}}
	srv.webhookClient = mock.Client()
	srv.rebuildWebhookDispatcher()

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/wh-test/test", nil)
	rec := httptest.NewRecorder()
	srv.handleWebhookTest(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("test-send status = %d, want 202: %s", rec.Code, rec.Body.String())
	}

	got := waitDeliveries(t, recv, 1, 5*time.Second)
	if len(got) != 1 {
		t.Fatalf("test-send delivered %d events, want 1", len(got))
	}
	if got[0]["event"] != webhooks.TestPingEvent {
		t.Errorf("delivered event = %v, want %q", got[0]["event"], webhooks.TestPingEvent)
	}
	data, ok := got[0]["data"].(map[string]any)
	if !ok {
		t.Fatalf("delivered data = %v, want an object", got[0]["data"])
	}
	if data["test"] != true || data["webhook_id"] != "wh-test" {
		t.Errorf("example data = %v, want {test:true, webhook_id:wh-test}", data)
	}

	// Unknown id is 404.
	req = httptest.NewRequest(http.MethodPost, "/api/webhooks/nope/test", nil)
	rec = httptest.NewRecorder()
	srv.handleWebhookTest(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("test-send unknown id status = %d, want 404", rec.Code)
	}
}

// A4 required test: the eval events are subscribable END-TO-END — the
// scheduler's notify wire reaches a real subscriber. Both halves are
// proven: registration accepts the names (TestWebhookRegistration-
// AcceptsEveryKnownEvent) and the wire (s.evalNotify, the exact
// function startEvalScheduler installs) delivers them.
func TestEvalEventsReachSubscriber(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	recv := &mockWebhookReceiver{ch: make(chan []byte, 8)}
	mock := httptest.NewServer(recv)
	defer mock.Close()

	evalEvents := []string{webhooks.EvalCompletedEvent, webhooks.EvalRunFailedEvent, webhooks.EvalRegressionEvent}
	s.cfg.Webhooks = []config.WebhookConfig{{ID: "wh-eval", URL: mock.URL, Events: evalEvents}}
	srv.webhookClient = mock.Client()
	srv.rebuildWebhookDispatcher()

	for _, ev := range evalEvents {
		srv.evalNotify(ev, "sch-1", "run-7")
	}

	seen := map[string]bool{}
	for _, env := range waitDeliveries(t, recv, len(evalEvents), 5*time.Second) {
		ev, _ := env["event"].(string)
		data, _ := env["data"].(map[string]any)
		if data["schedule_id"] != "sch-1" || data["run_id"] != "run-7" {
			t.Errorf("event %q data = %v, want schedule_id sch-1 / run_id run-7", ev, data)
		}
		seen[ev] = true
	}
	for _, ev := range evalEvents {
		if !seen[ev] {
			t.Errorf("eval event %q never reached its subscriber", ev)
		}
	}
}
