package catalog

// A1: zero live entries are silently unroutable. Every entry All()
// returns either is routable or carries an honest, non-empty,
// category-specific reason (surfaced verbatim through /api/catalog's
// unroutable_reason field).

import (
	"strings"
	"testing"
)

func TestEveryUnroutableEntryHasAReason(t *testing.T) {
	reasoned := 0
	for _, p := range All() {
		reason := p.UnroutableReason()
		if p.Routable() {
			if reason != "" {
				t.Errorf("%s: routable but carries an unroutable reason %q", p.ID, reason)
			}
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s (%s): unroutable with NO reason — every live entry must say why it cannot be dialed", p.ID, p.Category)
			continue
		}
		if !strings.HasSuffix(reason, ".") && !strings.HasSuffix(reason, ")") {
			t.Errorf("%s: reason must read as a complete sentence (got %q)", p.ID, reason)
		}
		reasoned++
	}
	if reasoned == 0 {
		t.Fatal("no unroutable entries found — the catalog shape changed; update this test deliberately")
	}
}

func TestUnroutableReasonIsCategorySpecific(t *testing.T) {
	for _, p := range All() {
		if p.Routable() || p.Retired {
			continue
		}
		reason := p.UnroutableReason()
		switch p.Category {
		case CategoryWebCookie:
			if !strings.Contains(reason, "browser session state") {
				t.Errorf("%s: web-cookie reason must name browser session state (got %q)", p.ID, reason)
			}
		case CategoryOAuth:
			if !strings.Contains(reason, "login") && !strings.Contains(reason, "OAuth") {
				t.Errorf("%s: oauth reason must name the login/OAuth mechanics (got %q)", p.ID, reason)
			}
		case CategoryNoAuth, CategoryLocal:
			if !strings.Contains(reason, "not documented yet") {
				t.Errorf("%s: reason must say the URL is not documented yet (got %q)", p.ID, reason)
			}
		}
	}
}

func TestUnroutableReasonTemplatedURL(t *testing.T) {
	p := Provider{ID: "tmpl", Category: CategoryAPIKey, BaseURLs: []string{"https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1"}}
	want := "template that must be completed"
	if got := p.UnroutableReason(); !strings.Contains(got, want) {
		t.Errorf("templated reason = %q, want it to name the completion requirement", got)
	}
}
