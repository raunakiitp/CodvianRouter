package config

import "testing"

// A4 item 8: chat-integration kinds are validated. An empty kind is the
// generic webhook; slack/discord accept any http(s) URL; telegram
// requires a parseable Bot API URL carrying a chat_id; an unknown
// non-empty kind is rejected.
func TestValidateWebhookKind(t *testing.T) {
	const tg = "https://api.telegram.org/bot123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij/sendMessage?chat_id=-100"
	cases := []struct {
		name string
		w    WebhookConfig
		ok   bool
	}{
		{"generic empty kind", WebhookConfig{ID: "a", URL: "https://example.com/h"}, true},
		{"slack", WebhookConfig{ID: "a", URL: "https://hooks.slack.com/services/x", Kind: "slack"}, true},
		{"discord", WebhookConfig{ID: "a", URL: "https://discord.com/api/webhooks/x", Kind: "discord"}, true},
		{"telegram valid", WebhookConfig{ID: "a", URL: tg, Kind: "telegram"}, true},
		{"unknown kind", WebhookConfig{ID: "a", URL: "https://example.com/h", Kind: "irc"}, false},
		{"telegram missing chat_id", WebhookConfig{ID: "a", URL: "https://api.telegram.org/bot123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij/sendMessage", Kind: "telegram"}, false},
		{"telegram bad token", WebhookConfig{ID: "a", URL: "https://api.telegram.org/botnope/sendMessage?chat_id=1", Kind: "telegram"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWebhookKind(tc.w)
			if (err == nil) != tc.ok {
				t.Errorf("ValidateWebhookKind() err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// The whole-config Validate must reject an unknown kind too, so a bad
// config-file entry cannot load.
func TestConfigValidateRejectsUnknownWebhookKind(t *testing.T) {
	c := DefaultConfig(t.TempDir())
	c.Webhooks = []WebhookConfig{{ID: "a", URL: "https://example.com/hook", Kind: "carrierpigeon"}}
	if err := c.Validate(); err == nil {
		t.Error("Config.Validate() accepted an unknown webhook kind")
	}
}

// Clone must deep-copy the new Metadata map, so mutating the original
// does not change the clone.
func TestWebhookMetadataDeepCopied(t *testing.T) {
	c := DefaultConfig(t.TempDir())
	c.Webhooks = []WebhookConfig{{ID: "a", URL: "https://api.telegram.org/bot123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij/sendMessage?chat_id=1", Kind: "telegram", Metadata: map[string]string{"bot_token": "v2:sealed"}}}
	clone := c.Clone()
	if clone.Webhooks[0].Metadata["bot_token"] != "v2:sealed" {
		t.Fatalf("clone metadata = %v", clone.Webhooks[0].Metadata)
	}
	clone.Webhooks[0].Metadata["bot_token"] = "tampered"
	if c.Webhooks[0].Metadata["bot_token"] != "v2:sealed" {
		t.Error("clone shares the Metadata map with the original")
	}
}
