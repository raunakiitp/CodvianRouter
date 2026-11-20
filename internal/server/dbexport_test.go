package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// dbExportPayload mirrors the export file shape for assertions.
type dbExportPayload struct {
	Format     string    `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	Config     struct {
		Version         int    `json:"version"`
		Listen          string `json:"listen"`
		GatewayKeyCount int    `json:"gateway_key_count"`
		Providers       []struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			WireFormat string   `json:"wire_format"`
			Models     []string `json:"models"`
		} `json:"providers"`
	} `json:"config"`
	Usage struct {
		Summary        ledger.UsageSummary `json:"summary"`
		Events         []ledger.UsageEvent `json:"events"`
		EventsTotal    int                 `json:"events_total"`
		EventsIncluded int                 `json:"events_included"`
		Truncated      bool                `json:"truncated"`
	} `json:"usage"`
	Secrets string `json:"secrets"`
	Note    string `json:"note"`
}

// newDBExportServer builds a server with a config (one provider, one
// gateway key) and a ledger holding two recorded usage events, and
// returns the server plus the raw gateway key for auth.
func newDBExportServer(t *testing.T) (*Server, string, *ledger.Ledger) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	key, raw, err := config.IssueKey("test", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	cfg.Providers = []provider.ProviderConfig{{
		ID: "openai", Name: "OpenAI", WireFormat: "openai",
		Models:  []provider.Model{{ID: "gpt-4o-mini"}},
		Enabled: true,
	}}
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	ctx := context.Background()
	for _, u := range []ledger.Usage{
		{GatewayKeyID: key.ID, Provider: "openai", Model: "gpt-4o-mini", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, SplitKnown: true, OK: true, LatencyMS: 120},
		{GatewayKeyID: key.ID, Provider: "openai", Model: "gpt-4o-mini", PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28, SplitKnown: true, OK: false, LatencyMS: 90},
	} {
		if _, err := led.RecordUsage(ctx, u, ledger.Prices{}); err != nil {
			t.Fatal(err)
		}
	}
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg), WithLedger(led))
	return srv, raw, led
}

// TestDBExportDownloadsRedactedJSON: with key + typed EXPORT the
// route downloads a JSON file with the redacted config and the full
// usage history (both recorded events, summary totals matching).
func TestDBExportDownloadsRedactedJSON(t *testing.T) {
	srv, raw, led := newDBExportServer(t)
	rec := doReqPeer(t, srv, "POST", "/admin/db/export", `{"confirm": "EXPORT"}`, raw, loopbackPeer)
	if rec.Code != 200 {
		t.Fatalf("db export: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("db export Content-Type = %q, want application/json", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "codvian-router-db-export-") || !strings.HasSuffix(cd, `.json"`) {
		t.Errorf("db export Content-Disposition = %q, want an attachment download named codvian-router-db-export-*.json", cd)
	}
	var payload dbExportPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("db export is not JSON: %v", err)
	}
	if payload.Format != "codvian-router-db-export/v1" {
		t.Errorf("db export format = %q, want codvian-router-db-export/v1", payload.Format)
	}
	if !strings.Contains(payload.Secrets, "redacted") {
		t.Errorf("db export secrets note = %q, want it to say redacted", payload.Secrets)
	}
	if payload.Config.GatewayKeyCount != 1 {
		t.Errorf("db export gateway_key_count = %d, want 1", payload.Config.GatewayKeyCount)
	}
	if len(payload.Config.Providers) != 1 || payload.Config.Providers[0].ID != "openai" {
		t.Errorf("db export providers = %+v, want the one configured provider", payload.Config.Providers)
	}
	if payload.Usage.EventsTotal != 2 || payload.Usage.EventsIncluded != 2 || payload.Usage.Truncated {
		t.Errorf("db export usage = total %d included %d truncated %v, want 2/2/false", payload.Usage.EventsTotal, payload.Usage.EventsIncluded, payload.Usage.Truncated)
	}
	if payload.Usage.Summary.Requests != 2 || payload.Usage.Summary.OK != 1 || payload.Usage.Summary.Errors != 1 {
		t.Errorf("db export summary = %+v, want 2 requests (1 ok, 1 error)", payload.Usage.Summary)
	}
	if len(payload.Usage.Events) != 2 || payload.Usage.Events[0].Model != "gpt-4o-mini" {
		t.Errorf("db export events = %+v, want both recorded events", payload.Usage.Events)
	}
	// No secret material anywhere in the file: no base URLs, no
	// keys, no key hashes, no key-shaped fields. (The word "hash"
	// appears only inside the redaction note itself — "gateway-key
	// hashes ... are never exported" — which is the disclosure, not
	// a hash value; the check below looks for a hash field/value.)
	body := rec.Body.String()
	for _, leaked := range []string{"base_url", "api_key", "api_key_encrypted", "sha256", `"hash":`, `"key":`, "crk_"} {
		if strings.Contains(strings.ToLower(body), leaked) {
			t.Errorf("db export contains %q: %s", leaked, body)
		}
	}
	// The export is read-only: the ledger still holds both events.
	events, err := led.RecentEvents(context.Background(), 10, ledger.LogAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Errorf("ledger holds %d events after export, want 2 (export must never reset history)", len(events))
	}
}

// TestDBExportCredentialBoundary: a key alone is not enough (no
// phrase, or the wrong phrase, is a 400), no key at all is a 401
// even when Tier 3 auth is relaxed, and without a config the route
// answers 503 instead of an empty export.
func TestDBExportCredentialBoundary(t *testing.T) {
	srv, raw, _ := newDBExportServer(t)

	// No key: 401.
	rec := doReqPeer(t, srv, "POST", "/admin/db/export", `{"confirm": "EXPORT"}`, "", loopbackPeer)
	if rec.Code != 401 {
		t.Errorf("db export without key: status %d, want 401", rec.Code)
	}
	// Key but no confirm body: 400.
	rec = doReqPeer(t, srv, "POST", "/admin/db/export", ``, raw, loopbackPeer)
	if rec.Code != 400 {
		t.Errorf("db export key without confirm: status %d, want 400", rec.Code)
	}
	// Key + wrong phrase: 400.
	rec = doReqPeer(t, srv, "POST", "/admin/db/export", `{"confirm": "export"}`, raw, loopbackPeer)
	if rec.Code != 400 {
		t.Errorf("db export wrong phrase: status %d, want 400", rec.Code)
	}
	// Key + right phrase from a non-loopback peer: Tier 2 is not
	// loopback-gated, so it still works (the loopback rule is Tier 1).
	rec = doReqPeer(t, srv, "POST", "/admin/db/export", `{"confirm": "EXPORT"}`, raw, nonLoopbackPeer)
	if rec.Code != 200 {
		t.Errorf("db export non-loopback + key + phrase: status %d, want 200 (Tier 2 is auth-gated, not loopback-gated)", rec.Code)
	}

	// Relaxed Tier 3 auth must not open this Tier 2 route.
	cfg := config.DefaultConfig(t.TempDir())
	key, _, err := config.IssueKey("test", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	relaxed := newTestServer(t, &fakeEngine{}, WithAuthRelaxed(true), WithConfig(cfg))
	rec = doReqPeer(t, relaxed, "POST", "/admin/db/export", `{"confirm": "EXPORT"}`, "", loopbackPeer)
	if rec.Code != 401 {
		t.Errorf("db export relaxed auth without key: status %d, want 401", rec.Code)
	}

	// No config loaded: 503, not an empty export.
	bare := newTestServer(t, &fakeEngine{})
	rec = doReqPeer(t, bare, "POST", "/admin/db/export", `{"confirm": "EXPORT"}`, "test-key-123", loopbackPeer)
	if rec.Code != 503 {
		t.Errorf("db export without config: status %d, want 503", rec.Code)
	}
}

// TestDBExportWithoutLedger: with a config but no ledger wired, the
// export still downloads — with an honestly empty usage section.
func TestDBExportWithoutLedger(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	key, raw, err := config.IssueKey("test", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))
	rec := doReqPeer(t, srv, "POST", "/admin/db/export", `{"confirm": "EXPORT"}`, raw, loopbackPeer)
	if rec.Code != 200 {
		t.Fatalf("db export without ledger: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var payload dbExportPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("db export is not JSON: %v", err)
	}
	if payload.Usage.EventsTotal != 0 || payload.Usage.EventsIncluded != 0 {
		t.Errorf("db export without ledger: usage = %+v, want empty", payload.Usage)
	}
	if payload.Usage.Summary.ByKey == nil {
		t.Error("db export without ledger: summary.by_key decoded as nil, want empty non-nil slice")
	}
	if !strings.Contains(rec.Body.String(), `"by_key": []`) {
		t.Errorf("db export without ledger: summary does not render \"by_key\": []: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"by_key":null`) {
		t.Errorf("db export without ledger: summary renders by_key as null: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no ledger configured") {
		t.Errorf("db export without ledger does not say so: %s", rec.Body.String())
	}
}
