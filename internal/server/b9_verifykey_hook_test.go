// B9-G21 — VerifyKey hook for bare reads (GAP-TRACK §9, G21 remainder (b)).
//
// The hole: bare reads such as GET /v1/models (and the non-spend
// batch routes) go through the auth funnel (VerifyKey passes) but
// never reach checkKeyTrafficModel, so a key's IP / endpoint policy
// was never enforced on them: an IP-restricted key could list
// models from a blocked IP, and an endpoint-allowlisted key could
// hit read routes outside its allowlist. The fix hooks the IP /
// endpoint policy into the funnel as a wrapper between scope and
// handler (requireKeyPolicy, server.go), so every authenticated
// request is policy-checked, spend or read — with scope still
// refusing first, and POST /v1/batches checked as the chat
// request its lines execute as (G21 convention).
package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// b9AddModelsScope gives the fixture key the "models" scope, so the
// only gate in play on GET /v1/models is the policy hook — without
// it the scope gate would refuse first and prove nothing about the
// hook.
func b9AddModelsScope(t *testing.T, s *keyBudgetSetup) {
	t.Helper()
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "models"}
		}
	}
}

func TestB9BareReadIPPolicyEnforced(t *testing.T) {
	s, _ := keyStatusSetup(t)
	b9AddModelsScope(t, s)
	setFixturePolicy(s, config.KeyPolicy{IPAllowlist: []string{"10.0.0.1"}})
	srv := s.server(t)
	rec := doReqPeer(t, srv, http.MethodGet, "/v1/models", "", s.raw, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_ip_not_allowed") {
		t.Fatalf("IP-restricted key listing models from a blocked IP: status = %d, want 403 key_ip_not_allowed: %s", rec.Code, rec.Body.String())
	}
}

func TestB9BareReadEndpointPolicyEnforced(t *testing.T) {
	s, _ := keyStatusSetup(t)
	b9AddModelsScope(t, s)
	setFixturePolicy(s, config.KeyPolicy{AllowedEndpoints: []string{"/v1/chat/completions"}})
	srv := s.server(t)
	rec := doReqPeer(t, srv, http.MethodGet, "/v1/models", "", s.raw, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_endpoint_not_allowed") {
		t.Fatalf("endpoint-allowlisted key listing models: status = %d, want 403 key_endpoint_not_allowed: %s", rec.Code, rec.Body.String())
	}
}

func TestB9BareReadNoPolicyPasses(t *testing.T) {
	s, _ := keyStatusSetup(t)
	b9AddModelsScope(t, s)
	srv := s.server(t)
	rec := doReqPeer(t, srv, http.MethodGet, "/v1/models", "", s.raw, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("policy-free key listing models: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestB9BareReadAllowedIPPasses(t *testing.T) {
	s, _ := keyStatusSetup(t)
	b9AddModelsScope(t, s)
	setFixturePolicy(s, config.KeyPolicy{IPAllowlist: []string{"127.0.0.1"}})
	srv := s.server(t)
	rec := doReqPeer(t, srv, http.MethodGet, "/v1/models", "", s.raw, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("key allowlisted for this IP listing models: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestB9BareReadBatchListIPPolicyEnforced(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{IPAllowlist: []string{"10.0.0.1"}})
	srv := s.server(t)
	rec := doReqPeer(t, srv, http.MethodGet, "/v1/batches", "", s.raw, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_ip_not_allowed") {
		t.Fatalf("IP-restricted key listing batches from a blocked IP: status = %d, want 403 key_ip_not_allowed: %s", rec.Code, rec.Body.String())
	}
}

func TestB9BareReadSpendPathStillGated(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{IPAllowlist: []string{"10.0.0.1"}})
	srv := s.server(t)
	rec := doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("model-a", false), s.raw, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_ip_not_allowed") {
		t.Fatalf("IP-restricted key chatting from a blocked IP: status = %d, want 403 key_ip_not_allowed: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the refused chat must never reach the engine", s.eng.chatCalls.Load())
	}
	// The same key from its allowed IP still routes.
	setFixturePolicy(s, config.KeyPolicy{IPAllowlist: []string{"127.0.0.1"}})
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("model-a", false), s.raw, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("key from its allowed IP chatting: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
