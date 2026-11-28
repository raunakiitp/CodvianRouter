package config

import (
	"fmt"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/webhooks/integrations"
)

// ValidateWebhookKind rejects an unknown non-empty integration kind and,
// for telegram, requires the endpoint URL to be a parseable Bot API URL
// that carries a chat_id (the token may instead come from sealed
// metadata). An empty kind is the generic signed webhook and always
// passes. Shared by the config-file validator and the registration API
// so a kind accepted at registration also loads at startup.
func ValidateWebhookKind(w WebhookConfig) error {
	kind, isChat := integrations.ParseKind(w.Kind)
	if !isChat {
		if strings.TrimSpace(w.Kind) != "" {
			return fmt.Errorf("config: webhooks[%q].kind %q is not a known integration (known: slack, discord, telegram)", w.ID, w.Kind)
		}
		return nil
	}
	if kind == integrations.KindTelegram {
		if _, _, err := integrations.ParseTelegramURL(w.URL); err != nil {
			return fmt.Errorf("config: webhooks[%q] telegram url: %v", w.ID, err)
		}
	}
	return nil
}
