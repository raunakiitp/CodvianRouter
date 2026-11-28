package server

import (
	"time"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// breakerWebhookEvents adapts router.Engine circuit-breaker transitions
// to the webhook dispatcher ("provider.degraded" on a CLOSED->OPEN
// trip, "provider.recovered" when that OPEN breaker clears). The
// payload deliberately carries only the provider id and the real
// consecutive-failure/trip counts and open duration — never a key, a
// model, a prompt or any provider credential. The engine emits these
// edge-triggered (see router.Engine.SetBreakerEvents), so this adapter
// never invents health signals: steady failures while a breaker is
// already open, and ordinary successes on a healthy provider, send
// nothing. A nil dispatcher is a silent no-op (matches Notify).
type breakerWebhookEvents struct {
	w *webhooks.Dispatcher
}

func (b *breakerWebhookEvents) ProviderDegraded(provider string, consecutive, trips int, openFor time.Duration) {
	if b == nil || b.w == nil {
		return
	}
	b.w.Notify(webhooks.ProviderDegradedEvent, map[string]any{
		"provider":             provider,
		"consecutive_failures": consecutive,
		"trips":                trips,
		"open_for_ms":          openFor.Milliseconds(),
	})
}

func (b *breakerWebhookEvents) ProviderRecovered(provider string) {
	if b == nil || b.w == nil {
		return
	}
	b.w.Notify(webhooks.ProviderRecoveredEvent, map[string]any{"provider": provider})
}

// ProviderResetRedeemed forwards a banked-reset redemption (an operator
// claimed a reset that was banked when the provider's breaker tripped) as
// the "provider.reset_redeemed" webhook event. The payload carries only the
// provider id and when the reset was banked — never a key, model, prompt or
// credential.
func (b *breakerWebhookEvents) ProviderResetRedeemed(provider string, bankedAt time.Time) {
	if b == nil || b.w == nil {
		return
	}
	b.w.Notify(webhooks.ProviderResetRedeemedEvent, map[string]any{
		"provider":  provider,
		"banked_at": bankedAt.UTC().Format(time.RFC3339),
	})
}

// BreakerEventsWire returns a router.BreakerEvents sink that forwards
// the engine's genuine breaker transitions to the server's webhook
// dispatcher. A server without webhooks wired returns a nil-sink
// adapter that emits nothing. main.go calls
// engine.SetBreakerEvents(srv.BreakerEventsWire()) after NewServer.
func (s *Server) BreakerEventsWire() router.BreakerEvents {
	return &breakerWebhookEvents{w: s.webhooks}
}
