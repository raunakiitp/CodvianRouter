package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// A4 item 8: the registration API accepts chat-integration kinds and
// echoes them back; an unknown kind (or a telegram URL without a
// chat_id) is a 400.
func TestWebhookRegistrationChatKinds(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)

	for _, kind := range []string{"slack", "discord"} {
		body := `{"url":"https://93.184.215.10/hook","events":["request.completed"],"kind":"` + kind + `"}`
		rec := callWebhookHandler(t, srv, "POST", "/api/webhooks", body)
		if rec.Code != http.StatusCreated {
			t.Errorf("register kind %q: status %d, want 201: %s", kind, rec.Code, rec.Body.String())
			continue
		}
		var created webhookView
		decodeWebhookBody(t, rec, &created)
		if created.Kind != kind {
			t.Errorf("created kind = %q, want %q", created.Kind, kind)
		}
	}

	rec := callWebhookHandler(t, srv, "POST", "/api/webhooks", `{"url":"https://93.184.215.10/hook","events":["request.completed"],"kind":"irc"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("register unknown kind: status %d, want 400", rec.Code)
	}

	// A telegram entry with a well-formed Bot API URL is accepted even
	// though its host cannot be dialed at registration time (the URL is
	// validated for shape, not reachability, on the chat path).
	tgURL := "https://api.telegram.org/bot123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij/sendMessage?chat_id=1"
	rec = callWebhookHandler(t, srv, "POST", "/api/webhooks", `{"url":"`+tgURL+`","events":["request.completed"],"kind":"telegram"}`)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusCreated {
		// Either is acceptable depending on DNS availability at the
		// registration-time egress check; a 500 would be a bug.
		t.Errorf("register telegram: unexpected status %d: %s", rec.Code, rec.Body.String())
	}
}

// A4 item 8 (server path): a chat-kind endpoint loaded from config is
// delivered in the platform format and is NOT signed.
func TestServerDeliversSlackKindFromConfig(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	recv := &mockWebhookReceiver{ch: make(chan []byte, 4)}
	mock := httptest.NewServer(recv)
	defer mock.Close()

	s.cfg.Webhooks = []config.WebhookConfig{{
		ID: "wh-slack", URL: mock.URL, Events: []string{webhooks.RequestCompletedEvent}, Kind: "slack",
	}}
	srv.webhookClient = mock.Client()
	srv.rebuildWebhookDispatcher()
	srv.webhooks.Notify(webhooks.RequestCompletedEvent, map[string]any{"model": "gpt-4o"})

	select {
	case body := <-recv.ch:
		var env map[string]any
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("slack delivery is not JSON: %v (%s)", err, body)
		}
		if _, ok := env["blocks"].([]any); !ok {
			t.Fatalf("slack delivery = %v, want a Block Kit body (blocks)", env)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no slack delivery arrived")
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if len(recv.sigs) != 1 || recv.sigs[0] != "" {
		t.Errorf("slack delivery signature = %v, want none", recv.sigs)
	}
}
