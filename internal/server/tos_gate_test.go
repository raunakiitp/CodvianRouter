package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// A3 item 6 — ToS risk-acknowledgement gate, server side.
//
// The acknowledgement endpoint records WHO (the authenticated admin
// identity the server already resolves), WHICH provider and WHEN,
// persists the record, and the POST lands in the append-only audit
// log. The provider registry's three gate checks read the same store
// through SetToSAcknowledgement (wired in WithProviderRegistry), so
// every test here that flips the gate through the real API exercises
// the real production wiring.

// ackTestServer builds a server with a data dir (persistent
// acknowledgement store) and the real provider registry wired.
func ackTestServer(t *testing.T, opts ...Option) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	// WithConfig first (s.cfg carries the data dir), WithStaticKeys
	// after it (the static verifier wins, and callerKeyID can name
	// "test-key-123" — same order as newA2ATestServer).
	all := append([]Option{WithConfig(cfg), WithStaticKeys("test-key-123")}, opts...)
	return newTestServer(t, &fakeEngine{}, all...), dir
}

// TestRiskAcknowledgementRequiresAuth — Tier 2 always-protected: an
// unauthenticated caller can never open the risk gate.
func TestRiskAcknowledgementRequiresAuth(t *testing.T) {
	srv, _ := ackTestServer(t)
	rec := doReq(t, srv, http.MethodPost, "/api/providers/chatgpt-web/risk-acknowledgement", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated ack: status %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestRiskAcknowledgementValidatesProvider — unknown catalog IDs are
// 404; non-ToS-grey providers are refused (control: the endpoint
// never fabricates a risk that does not exist).
func TestRiskAcknowledgementValidatesProvider(t *testing.T) {
	srv, _ := ackTestServer(t)

	rec := doReq(t, srv, http.MethodPost, "/api/providers/no-such-provider/risk-acknowledgement", "", "test-key-123")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown catalog id: status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}

	rec = doReq(t, srv, http.MethodPost, "/api/providers/groq/risk-acknowledgement", "", "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-grey catalog id: status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not ToS-grey") {
		t.Errorf("non-grey refusal does not say why: %s", rec.Body.String())
	}
}

// TestRiskAcknowledgementRecordsWhoWhenProvider — the endpoint
// records WHO (hashed gateway-key identity), WHICH provider and WHEN,
// and the record survives a restart (persisted in the data dir).
func TestRiskAcknowledgementRecordsWhoWhenProvider(t *testing.T) {
	srv, dir := ackTestServer(t)

	rec := doReq(t, srv, http.MethodPost, "/api/providers/chatgpt-web/risk-acknowledgement", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("ack: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status         string `json:"status"`
		Provider       string `json:"provider"`
		AcknowledgedBy string `json:"acknowledged_by"`
		AcknowledgedAt string `json:"acknowledged_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ack response is not JSON: %v", err)
	}
	if resp.Status != "acknowledged" || resp.Provider != "chatgpt-web" {
		t.Errorf("ack response = %+v, want acknowledged/chatgpt-web", resp)
	}
	if resp.AcknowledgedBy == "" {
		t.Errorf("ack response has no acknowledged_by (want the hashed gateway-key identity)")
	}
	if resp.AcknowledgedBy == "test-key-123" {
		t.Errorf("acknowledged_by leaks the raw key: %q", resp.AcknowledgedBy)
	}
	if resp.AcknowledgedAt == "" {
		t.Errorf("ack response has no acknowledged_at (want WHEN)")
	}

	// The record is persisted in the data dir and a fresh server on
	// the same data dir sees it (restart recovery).
	if _, err := os.Stat(filepath.Join(dir, "tos_acknowledgements.json")); err != nil {
		t.Fatalf("acknowledgement file missing: %v", err)
	}
	srv2, _ := ackTestServer(t, WithConfig(&config.Config{DataDir: dir}))
	if !srv2.ToSAcknowledged("chatgpt-web") {
		t.Fatal("a fresh server on the same data dir does not see the acknowledgement (restart recovery broken)")
	}
	if srv2.ToSAcknowledged("groq") {
		t.Fatal("groq (non-grey) reports acknowledged — store leak")
	}
}

// TestRiskAcknowledgementAuditEntry — the acknowledgement POST is
// written to the append-only audit log: actor key hash set (WHO), the
// path names the provider (WHICH), timestamp present (WHEN).
func TestRiskAcknowledgementAuditEntry(t *testing.T) {
	dir := t.TempDir()
	srv, _ := ackTestServer(t, WithAuditStore(filepath.Join(dir, "audit.db")))

	rec := doReq(t, srv, http.MethodPost, "/api/providers/chatgpt-web/risk-acknowledgement", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("ack: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	events, total, err := srv.auditStore.List(50, 0)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	if total == 0 {
		t.Fatal("audit log is empty after an acknowledgement POST")
	}
	found := false
	for _, e := range events {
		if strings.Contains(e.Path, "/risk-acknowledgement") && strings.Contains(e.Path, "chatgpt-web") {
			found = true
			if e.KeyIDHash == "" {
				t.Errorf("audit row %s has no key_id_hash (WHO missing)", e.Path)
			}
			if e.TS == 0 {
				t.Errorf("audit row %s has no timestamp (WHEN missing)", e.Path)
			}
			if e.Method != http.MethodPost {
				t.Errorf("audit row method = %q, want POST", e.Method)
			}
			if e.ActorType != "gateway_key" {
				t.Errorf("audit row actor_type = %q, want gateway_key", e.ActorType)
			}
		}
	}
	if !found {
		t.Errorf("no audit row names the risk-acknowledgement path for chatgpt-web (rows: %d)", total)
	}
	if broken, err := srv.auditStore.VerifyChain(); err != nil || broken != -1 {
		t.Fatalf("audit chain verify: broken=%d err=%v (want intact chain, -1)", broken, err)
	}
}

// TestRiskAcknowledgementUnblocksAdd — the real pair through the real
// routes and the real store: unacknowledged, POST /api/providers for
// a ToS-grey provider is refused with the plain-words guidance
// message; after the acknowledgement API call, the same add succeeds.
func TestRiskAcknowledgementUnblocksAdd(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	reg := provider.NewRegistry(nil, provider.WithRegistryAllowPrivateDestinations(true))
	srv, _ := ackTestServer(t, WithProviderRegistry(reg))

	addBody := `{"catalog_id":"chatgpt-web","api_key":"session-cookie-value","base_url":"` + mock.URL + `"}`
	rec := postAdd(t, srv, addBody, "test-key-123")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unacknowledged ToS-grey add: status %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"ToS-grey", "terms", "suspended or banned", "acknowledge"} {
		if !strings.Contains(body, want) {
			t.Errorf("add refusal does not state %q: %s", want, body)
		}
	}
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("unacknowledged ToS-grey provider was stored (%d providers), want 0", len(got))
	}

	// Acknowledge through the real API, then the same add proceeds.
	rec = doReq(t, srv, http.MethodPost, "/api/providers/chatgpt-web/risk-acknowledgement", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("ack: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = postAdd(t, srv, addBody, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("acknowledged ToS-grey add: status %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if got := reg.List(); len(got) != 1 {
		t.Errorf("acknowledged ToS-grey provider not stored (%d providers), want 1", len(got))
	}
}

// TestRiskAcknowledgementNonGreyUnaffected — control: a non-ToS-grey
// provider adds exactly as before with no acknowledgement anywhere.
func TestRiskAcknowledgementNonGreyUnaffected(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	reg := provider.NewRegistry(nil, provider.WithRegistryAllowPrivateDestinations(true))
	srv, _ := ackTestServer(t, WithProviderRegistry(reg))

	rec := postAdd(t, srv, `{"catalog_id":"groq","api_key":"sk-groq-test","base_url":"`+mock.URL+`"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Errorf("non-grey add without any acknowledgement: status %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
}
