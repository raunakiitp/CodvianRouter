package server

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// A4 item 5: the server's live dispatcher is queue-backed once a config
// with a data dir is wired, so deliveries it fires can be redelivered
// after a restart and are recorded in history.
func TestServerWebhookDispatcherQueueWired(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	s.cfg.WebhooksDurableQueue = true // durability is opt-in (webhooks_durable_queue)
	s.cfg.Webhooks = []config.WebhookConfig{{
		ID: "ep", URL: "https://93.184.215.10/hook", Events: []string{webhooks.QuotaExceededEvent},
	}}
	srv.rebuildWebhookDispatcher()
	if srv.webhooks == nil {
		t.Fatal("dispatcher not rebuilt")
	}
	if srv.webhooks.Queue() == nil {
		t.Fatal("dispatcher has no durable delivery queue wired")
	}
}
