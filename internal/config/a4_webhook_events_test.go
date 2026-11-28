package config

import (
	"sort"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// A4 item 3: the config-file validator and the registration API must
// accept exactly the SAME event set. The config side accepts
// KnownWebhookEvents() (an alias of webhooks.KnownEvents), so every
// canonical event must validate here — including the eval events and
// test.ping that only the API accepted before — and an unknown name
// must be rejected.
func TestConfigAcceptsEveryCanonicalWebhookEvent(t *testing.T) {
	for _, ev := range KnownWebhookEvents() {
		c := DefaultConfig(t.TempDir())
		c.Webhooks = []WebhookConfig{{ID: "ops", URL: "https://example.com/hook", Events: []string{ev}}}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() rejected known webhook event %q: %v", ev, err)
		}
	}
	c := DefaultConfig(t.TempDir())
	c.Webhooks = []WebhookConfig{{ID: "ops", URL: "https://example.com/hook", Events: []string{"not.a.real.event"}}}
	if err := c.Validate(); err == nil {
		t.Error("Validate() accepted an unknown webhook event")
	}
}

// A4 item 3: the two named sets (config-file known set and the API
// known set) are IDENTICAL. Both must reference the one canonical
// source (webhooks.KnownEvents), so a divergence is a regression this
// catches.
func TestConfigWebhookEventSetMatchesCanonical(t *testing.T) {
	got := append([]string(nil), KnownWebhookEvents()...)
	want := webhooks.KnownEvents()
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("config known set has %d events, canonical has %d: %v vs %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("config known set[%d] = %q, canonical = %q", i, got[i], want[i])
		}
	}
}
