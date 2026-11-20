// G20 — per-key model policy (ModelAllowed / ForcedModel) on the
// relay (/v1/messages) and native-Gemini (:generateContent) paths.
//
// The gap (GAP-TRACK §9, G15 Nit 8): both paths called
// checkKeyTrafficRequestsOnly, which passes model "", so
// checkKeyPolicy skipped the model axis and a restricted /
// forced-model key could call any model. The fix threads the
// already-known model into the traffic gate (checkKeyTrafficModel):
// head.Model on relay (parsed + validated before the gate) and the
// URL path model on Gemini (known before the body is read). The
// policy runs BEFORE the limiter admits the request, so a model
// refusal consumes no traffic slot and no provider is called —
// proven below with an RPM=1 key: a refusal followed by an
// admitted request still succeeds, and the admitted request
// consumes exactly one slot.
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

func g20MessagesBody(model string) string {
	return `{"model":"` + model + `","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
}

func g20GeminiBody() string {
	return `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`
}

// g20MessagesServer wires the fixture key's server with a verbatim
// relay resolver pointing at the mock upstream for both models, so
// a request that reaches the provider is visible as a mock hit.
func g20MessagesServer(t *testing.T, s *keyBudgetSetup, mock *anthropicMock) *Server {
	t.Helper()
	upstream := httptest.NewServer(mock.handler(t))
	t.Cleanup(upstream.Close)
	resolver := &fakeRelayResolver{targets: map[string]*relayTarget{
		"model-a": {
			ProviderID: "anth", ProviderName: "Anthropic", WireFormat: "anthropic",
			BaseURL: upstream.URL, AuthHeaders: map[string]string{"x-api-key": "sk-ant-test"},
			UpstreamModel: "model-a", Attempts: 1,
		},
		"model-b": {
			ProviderID: "anth", ProviderName: "Anthropic", WireFormat: "anthropic",
			BaseURL: upstream.URL, AuthHeaders: map[string]string{"x-api-key": "sk-ant-test"},
			UpstreamModel: "model-b", Attempts: 1,
		},
	}}
	return s.server(t, WithRelayResolver(resolver))
}

func g20UpstreamCalls(mock *anthropicMock) int {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	return len(mock.bodies)
}

// g20SetRPM caps the fixture key at n requests per minute.
func g20SetRPM(s *keyBudgetSetup, n int) {
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].RequestsPerMinute = n
		}
	}
}

func TestG20RelayModelAllowlistEnforcement(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{ModelAccessMode: "restricted", AllowedModels: []string{"model-a"}})
	g20SetRPM(s, 1)
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv := g20MessagesServer(t, s, mock)

	rec := doReqPeer(t, srv, http.MethodPost, "/v1/messages", g20MessagesBody("model-b"), s.raw, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_model_not_allowed") {
		t.Fatalf("relay with unlisted model: status = %d, want 403 key_model_not_allowed: %s", rec.Code, rec.Body.String())
	}
	if got := g20UpstreamCalls(mock); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 — the refused request must never reach a provider", got)
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0", s.eng.chatCalls.Load())
	}

	// The refusal consumed no traffic slot: with RPM=1 the
	// admitted request below still succeeds.
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/messages", g20MessagesBody("model-a"), s.raw, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("relay with listed model: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := g20UpstreamCalls(mock); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	// ...and the admitted request consumed exactly one slot: a
	// second admitted request is now over the RPM=1 ceiling.
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/messages", g20MessagesBody("model-a"), s.raw, loopbackPeer)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second relay with listed model: status = %d, want 429 (one slot consumed): %s", rec.Code, rec.Body.String())
	}
	if got := g20UpstreamCalls(mock); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 — the over-limit request must never reach a provider", got)
	}
}

func TestG20RelayForcedModelEnforcement(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{ForcedModel: "model-a"})
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv := g20MessagesServer(t, s, mock)

	rec := doReqPeer(t, srv, http.MethodPost, "/v1/messages", g20MessagesBody("model-b"), s.raw, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_forced_model") {
		t.Fatalf("relay with a non-pinned model: status = %d, want 403 key_forced_model: %s", rec.Code, rec.Body.String())
	}
	if got := g20UpstreamCalls(mock); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 — the refused request must never reach a provider", got)
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0", s.eng.chatCalls.Load())
	}
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/messages", g20MessagesBody("model-a"), s.raw, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("relay with the pinned model: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := g20UpstreamCalls(mock); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestG20GeminiModelAllowlistEnforcement(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{ModelAccessMode: "restricted", AllowedModels: []string{"model-a"}})
	g20SetRPM(s, 1)
	srv := s.server(t)

	rec := doReqPeer(t, srv, http.MethodPost, "/v1beta/models/model-b:generateContent", g20GeminiBody(), s.raw, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_model_not_allowed") {
		t.Fatalf("gemini with unlisted model: status = %d, want 403 key_model_not_allowed: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the refused request must never reach a provider", s.eng.chatCalls.Load())
	}

	// The refusal consumed no traffic slot: with RPM=1 the
	// admitted request below still succeeds.
	rec = doReqPeer(t, srv, http.MethodPost, "/v1beta/models/model-a:generateContent", g20GeminiBody(), s.raw, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("gemini with listed model: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1", s.eng.chatCalls.Load())
	}
	// ...and the admitted request consumed exactly one slot.
	rec = doReqPeer(t, srv, http.MethodPost, "/v1beta/models/model-a:generateContent", g20GeminiBody(), s.raw, loopbackPeer)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second gemini with listed model: status = %d, want 429 (one slot consumed): %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1 — the over-limit request must never reach a provider", s.eng.chatCalls.Load())
	}
}

func TestG20GeminiForcedModelEnforcement(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{ForcedModel: "model-a"})
	srv := s.server(t)

	rec := doReqPeer(t, srv, http.MethodPost, "/v1beta/models/model-b:generateContent", g20GeminiBody(), s.raw, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_forced_model") {
		t.Fatalf("gemini with a non-pinned model: status = %d, want 403 key_forced_model: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the refused request must never reach a provider", s.eng.chatCalls.Load())
	}
	rec = doReqPeer(t, srv, http.MethodPost, "/v1beta/models/model-a:generateContent", g20GeminiBody(), s.raw, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("gemini with the pinned model: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1", s.eng.chatCalls.Load())
	}
}

// TestG20A2AModelPolicyAlreadyEnforced pins the builder's claim
// from the other direction: the A2A path already enforces the
// model axis (a2a.go:761 checkKeyTrafficForChat carries
// chatReq.Model, set from the caller's params at a2a.go:392),
// so no G20 change was needed there. This test proves it live:
// a restricted key naming a non-allowed model via /a2a gets a
// JSON-RPC error carrying the policy message and no engine call.
// (The JSON-RPC error carries the policy message only — the
// key_model_not_allowed type string lives on the chat/relay
// 403 body, not in the A2A envelope.)
// Caveat (G20 re-review nit 1): only A2A permits a model-less
// call that skips the model axis — chat, relay and Gemini all
// require a non-empty model before their gates (server.go:4771,
// relay.go:326, gemini_native.go:1082), so a forced-model key
// cannot escape pinning by omitting the model on those paths.
func TestG20A2AModelPolicyAlreadyEnforced(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{ModelAccessMode: "restricted", AllowedModels: []string{"model-a"}})
	s.cfg.A2AEnabled = true
	srv := s.server(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"model":"model-b","messages":[{"role":"user","content":"hello"}]}}`
	rec := doReqPeer(t, srv, http.MethodPost, "/a2a", body, s.raw, loopbackPeer)
	if !strings.Contains(rec.Body.String(), "is not allowed for this API key") {
		t.Fatalf("a2a with unlisted model: status = %d, body = %s, want the model-policy refusal", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the refused request must never reach a provider", s.eng.chatCalls.Load())
	}
}
