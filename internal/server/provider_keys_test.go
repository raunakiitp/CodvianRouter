package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// Provider-key endpoint tests (CHECKLIST 175-176): add / remove /
// re-role one provider's pool keys through the real HTTP stack,
// with the live key test against a key-aware mock upstream.

// keyAwareUpstream is mockUpstream with one twist: it 401s only for
// the bad key, so the add-key live test can refuse a dead key while
// accepting good ones — the way a real upstream behaves.
type keyAwareUpstream struct {
	mockUpstream
}

func (m keyAwareUpstream) handler() http.HandlerFunc {
	base := m.mockUpstream.handler()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" && strings.TrimSpace(r.Header.Get("Authorization")) == "Bearer sk-bad" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`))
			return
		}
		base(w, r)
	}
}

// keyTestServer is a test server with one custom provider (key-1)
// registered against the key-aware mock, plus a hook counter that
// records every key-changed hook call.
type keyTestServer struct {
	srv     *Server
	reg     *provider.Registry
	provID  string
	hookMu  sync.Mutex
	hookIDs []string
}

func newKeyTestServer(t *testing.T) *keyTestServer {
	t.Helper()
	mock := httptest.NewServer(keyAwareUpstream{}.handler())
	t.Cleanup(mock.Close)
	reg := provider.NewRegistry(nil, provider.WithRegistryAllowPrivateDestinations(true)) // memory-only; ephemeral cipher is fine here
	ks := &keyTestServer{reg: reg}
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg),
		WithProviderKeyChangedHook(func(ctx context.Context, p *provider.Provider) error {
			ks.hookMu.Lock()
			defer ks.hookMu.Unlock()
			ks.hookIDs = append(ks.hookIDs, p.ID)
			return nil
		}))
	ks.srv = srv
	rec := doReq(t, srv, "POST", "/api/providers", `{"base_url":"`+mock.URL+`","api_key":"custom-key-1","name":"MockUpstream"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup POST /api/providers: status %d, body %s", rec.Code, rec.Body.String())
	}
	var created addedProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("setup provider is not JSON: %v", err)
	}
	ks.provID = created.ID
	return ks
}

func (ks *keyTestServer) hookCalls() int {
	ks.hookMu.Lock()
	defer ks.hookMu.Unlock()
	return len(ks.hookIDs)
}

func (ks *keyTestServer) keys(t *testing.T) []provider.ProviderKey {
	t.Helper()
	p, ok := ks.reg.Get(ks.provID)
	if !ok {
		t.Fatalf("provider %q vanished from the registry", ks.provID)
	}
	return p.EffectiveKeys()
}

func (ks *keyTestServer) postKey(t *testing.T, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, ks.srv, "POST", "/api/providers/"+ks.provID+"/keys", body, key)
}

func TestAddProviderKey(t *testing.T) {
	ks := newKeyTestServer(t)
	if got := ks.keys(t); len(got) != 1 {
		t.Fatalf("pool = %d keys, want 1 (the legacy key)", len(got))
	}
	rec := ks.postKey(t, `{"api_key":"custom-key-2","role":"preferred"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST keys: status %d, body %s (want 201)", rec.Code, rec.Body.String())
	}
	var added keyMutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatalf("add-key response is not JSON: %v", err)
	}
	if added.ProviderID != ks.provID || added.Key.ID == "" || added.Key.Role != "preferred" {
		t.Fatalf("added = %+v, want provider %q with a preferred key id", added, ks.provID)
	}
	if added.Key.Fingerprint == "" {
		t.Fatalf("added key has no fingerprint: %+v", added.Key)
	}
	if !added.RoutingLive || added.RoutingNote != "" {
		t.Fatalf("routing_live/note = %v/%q, want true/empty (hook wired)", added.RoutingLive, added.RoutingNote)
	}
	if ks.hookCalls() != 1 {
		t.Fatalf("hook calls = %d, want 1", ks.hookCalls())
	}
	// The response must never carry the key itself.
	if strings.Contains(rec.Body.String(), "custom-key-2") {
		t.Fatalf("add-key response leaks the key: %s", rec.Body.String())
	}
	// The pool now holds both keys, legacy first.
	pool := ks.keys(t)
	if len(pool) != 2 || pool[0].Role != "preferred" || pool[1].ID != added.Key.ID {
		t.Fatalf("pool = %+v, want [legacy, %s]", pool, added.Key.ID)
	}
	// And the provider feed shows both keys.
	feed := doReq(t, ks.srv, "GET", "/api/providers", "", "")
	var list apiProvidersResponse
	if err := json.Unmarshal(feed.Body.Bytes(), &list); err != nil {
		t.Fatalf("provider feed is not JSON: %v", err)
	}
	if len(list.Providers) != 1 || len(list.Providers[0].Keys) != 2 {
		t.Fatalf("feed providers = %+v, want 1 provider with 2 keys", list.Providers)
	}
	if list.Providers[0].Keys[1].ID != added.Key.ID || list.Providers[0].Keys[1].Status != "active" {
		t.Fatalf("feed keys = %+v, want the new key active", list.Providers[0].Keys)
	}
	if list.Providers[0].Keys[1].HasSpend {
		t.Fatalf("feed keys = %+v, want has_spend false (the key never served)", list.Providers[0].Keys)
	}
}

func TestAddProviderKeyBadKeyRefused(t *testing.T) {
	ks := newKeyTestServer(t)
	rec := ks.postKey(t, `{"api_key":"sk-bad","role":"preferred"}`, "test-key-123")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST keys with a 401 key: status %d, body %s (want 401)", rec.Code, rec.Body.String())
	}
	if got := ks.keys(t); len(got) != 1 {
		t.Fatalf("pool = %d keys after a refused add, want 1", len(got))
	}
	if ks.hookCalls() != 0 {
		t.Fatalf("hook calls = %d after a refused add, want 0", ks.hookCalls())
	}
}

func TestAddProviderKeyDuplicateRefused(t *testing.T) {
	ks := newKeyTestServer(t)
	rec := ks.postKey(t, `{"api_key":"custom-key-1","role":"preferred"}`, "test-key-123")
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST keys with the legacy credential: status %d, body %s (want 409)", rec.Code, rec.Body.String())
	}
	if got := ks.keys(t); len(got) != 1 {
		t.Fatalf("pool = %d keys after a duplicate add, want 1", len(got))
	}
}

func TestAddProviderKeyAuthRequired(t *testing.T) {
	ks := newKeyTestServer(t)
	for _, key := range []string{"", "wrong-key"} {
		rec := ks.postKey(t, `{"api_key":"custom-key-2"}`, key)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("POST keys with gateway key %q: status %d, want 401", key, rec.Code)
		}
	}
}

func TestAddProviderKeyRoleValidation(t *testing.T) {
	ks := newKeyTestServer(t)
	rec := ks.postKey(t, `{"api_key":"custom-key-2","role":"bogus"}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST keys with role bogus: status %d, want 400", rec.Code)
	}
	// An empty role defaults to preferred.
	rec = ks.postKey(t, `{"api_key":"custom-key-2"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST keys with no role: status %d, body %s (want 201)", rec.Code, rec.Body.String())
	}
}

func TestDeleteProviderKey(t *testing.T) {
	ks := newKeyTestServer(t)
	rec := ks.postKey(t, `{"api_key":"custom-key-2","role":"reserve"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup add key: status %d", rec.Code)
	}
	var added keyMutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatalf("add-key response is not JSON: %v", err)
	}
	before := ks.hookCalls()
	del := doReq(t, ks.srv, "DELETE", "/api/providers/"+ks.provID+"/keys/"+added.Key.ID, "", "test-key-123")
	if del.Code != http.StatusOK {
		t.Fatalf("DELETE key: status %d, body %s (want 200)", del.Code, del.Body.String())
	}
	var removed keyMutationResponse
	if err := json.Unmarshal(del.Body.Bytes(), &removed); err != nil {
		t.Fatalf("delete response is not JSON: %v", err)
	}
	if removed.Key.ID != added.Key.ID || !removed.RoutingLive {
		t.Fatalf("removed = %+v, want the key with routing_live true", removed)
	}
	if ks.hookCalls() != before+1 {
		t.Fatalf("hook calls = %d, want %d", ks.hookCalls(), before+1)
	}
	if got := ks.keys(t); len(got) != 1 {
		t.Fatalf("pool = %d keys after delete, want 1", len(got))
	}
}

func TestDeleteProviderKeyLastRefused(t *testing.T) {
	ks := newKeyTestServer(t)
	only := ks.keys(t)[0]
	rec := doReq(t, ks.srv, "DELETE", "/api/providers/"+ks.provID+"/keys/"+only.ID, "", "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE last key: status %d, body %s (want 400)", rec.Code, rec.Body.String())
	}
	if got := ks.keys(t); len(got) != 1 {
		t.Fatalf("pool = %d keys after a refused delete, want 1", len(got))
	}
}

func TestDeleteProviderKeyUnknown(t *testing.T) {
	ks := newKeyTestServer(t)
	rec := doReq(t, ks.srv, "DELETE", "/api/providers/"+ks.provID+"/keys/key_nope", "", "test-key-123")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown key: status %d, want 404", rec.Code)
	}
	rec = doReq(t, ks.srv, "DELETE", "/api/providers/nope/keys/key_nope", "", "test-key-123")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE key of unknown provider: status %d, want 404", rec.Code)
	}
}

func TestSetProviderKeyRole(t *testing.T) {
	ks := newKeyTestServer(t)
	rec := ks.postKey(t, `{"api_key":"custom-key-2","role":"preferred"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup add key: status %d", rec.Code)
	}
	var added keyMutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatalf("add-key response is not JSON: %v", err)
	}
	role := doReq(t, ks.srv, "POST", "/api/providers/"+ks.provID+"/keys/"+added.Key.ID+"/role", `{"role":"reserve"}`, "test-key-123")
	if role.Code != http.StatusOK {
		t.Fatalf("POST role: status %d, body %s (want 200)", role.Code, role.Body.String())
	}
	var changed keyMutationResponse
	if err := json.Unmarshal(role.Body.Bytes(), &changed); err != nil {
		t.Fatalf("role response is not JSON: %v", err)
	}
	if changed.Key.Role != "reserve" || !changed.RoutingLive {
		t.Fatalf("changed = %+v, want reserve + routing_live", changed)
	}
	pool := ks.keys(t)
	if pool[1].Role != "reserve" {
		t.Fatalf("pool role = %q, want reserve", pool[1].Role)
	}
	// Back to preferred.
	role = doReq(t, ks.srv, "POST", "/api/providers/"+ks.provID+"/keys/"+added.Key.ID+"/role", `{"role":"preferred"}`, "test-key-123")
	if role.Code != http.StatusOK {
		t.Fatalf("POST role back: status %d", role.Code)
	}
	if ks.keys(t)[1].Role != "preferred" {
		t.Fatalf("pool role = %q, want preferred", ks.keys(t)[1].Role)
	}
	// Unknown role / unknown key.
	bad := doReq(t, ks.srv, "POST", "/api/providers/"+ks.provID+"/keys/"+added.Key.ID+"/role", `{"role":"bogus"}`, "test-key-123")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("POST role bogus: status %d, want 400", bad.Code)
	}
	missing := doReq(t, ks.srv, "POST", "/api/providers/"+ks.provID+"/keys/key_nope/role", `{"role":"reserve"}`, "test-key-123")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("POST role of unknown key: status %d, want 404", missing.Code)
	}
}

// TestSetProviderEnabled proves POST /api/providers/{id}/enabled
// (critic fix 2026-11-19 — the provider-card Connect button):
// disabling a configured provider persists, flips the live feed to
// enabled=false, and re-enabling returns it, each time running the
// key-changed hook; unknown provider 404s, a missing or mistyped
// enabled 400s, and a wrong gateway key 401s.
func TestSetProviderEnabled(t *testing.T) {
	ks := newKeyTestServer(t)
	enabledURL := "/api/providers/" + ks.provID + "/enabled"

	setEnabled := func(body, key string) *httptest.ResponseRecorder {
		return doReq(t, ks.srv, "POST", enabledURL, body, key)
	}
	feedEnabled := func(t *testing.T) bool {
		t.Helper()
		rec := doReq(t, ks.srv, "GET", "/api/providers", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/providers: status %d, want 200", rec.Code)
		}
		var feed struct {
			Providers []struct {
				ID      string `json:"id"`
				Enabled bool   `json:"enabled"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
			t.Fatalf("providers feed is not JSON: %v", err)
		}
		for _, p := range feed.Providers {
			if p.ID == ks.provID {
				return p.Enabled
			}
		}
		t.Fatalf("provider %q missing from the feed", ks.provID)
		return false
	}

	hooksBefore := ks.hookCalls()
	if !feedEnabled(t) {
		t.Fatalf("feed enabled = false for a fresh provider, want true")
	}
	// Disable.
	rec := setEnabled(`{"enabled":false}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST enabled=false: status %d, body %s (want 200)", rec.Code, rec.Body.String())
	}
	var dis providerEnabledResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &dis); err != nil {
		t.Fatalf("enabled response is not JSON: %v", err)
	}
	if dis.ProviderID != ks.provID || dis.Enabled || !dis.RoutingLive || dis.RoutingNote != "" {
		t.Fatalf("disabled = %+v, want enabled=false + routing_live", dis)
	}
	if p, ok := ks.reg.Get(ks.provID); !ok || p.Enabled {
		t.Fatalf("registry enabled = true after disable, want false")
	}
	if feedEnabled(t) {
		t.Fatalf("feed enabled = true after disable, want false")
	}
	if ks.hookCalls() != hooksBefore+1 {
		t.Fatalf("hook calls = %d, want %d (one per enable/disable)", ks.hookCalls(), hooksBefore+1)
	}
	// Re-enable.
	rec = setEnabled(`{"enabled":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST enabled=true: status %d, body %s (want 200)", rec.Code, rec.Body.String())
	}
	var en providerEnabledResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &en); err != nil {
		t.Fatalf("enabled response is not JSON: %v", err)
	}
	if !en.Enabled || !en.RoutingLive {
		t.Fatalf("enabled = %+v, want enabled=true + routing_live", en)
	}
	if !feedEnabled(t) {
		t.Fatalf("feed enabled = false after re-enable, want true")
	}
	if ks.hookCalls() != hooksBefore+2 {
		t.Fatalf("hook calls = %d, want %d", ks.hookCalls(), hooksBefore+2)
	}
	// Unknown provider / bad bodies / bad auth.
	unknown := doReq(t, ks.srv, "POST", "/api/providers/nope/enabled", `{"enabled":true}`, "test-key-123")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("POST enabled for unknown provider: status %d, want 404", unknown.Code)
	}
	for _, body := range []string{`{}`, `{"enabled":"yes"}`, `{"enabled":true,"extra":1}`} {
		bad := setEnabled(body, "test-key-123")
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("POST enabled body %s: status %d, want 400", body, bad.Code)
		}
	}
	for _, key := range []string{"", "wrong-key"} {
		unauth := setEnabled(`{"enabled":true}`, key)
		if unauth.Code != http.StatusUnauthorized {
			t.Fatalf("POST enabled with gateway key %q: status %d, want 401", key, unauth.Code)
		}
	}
}

// TestRotateProviderKey proves POST /api/providers/{id}/keys/{keyId}/rotate
// (critic fix 2026-11-19 — the provider-card "paste a replacement key"
// Enter box): rotation replaces the credential in place — same key ID
// and role, new fingerprint, pool size unchanged — and runs the
// key-changed hook; a 401 key is refused with the pool untouched, the
// same credential twice 409s, and unknown key/provider 404, empty
// api_key 400s, a wrong gateway key 401s.
func TestRotateProviderKey(t *testing.T) {
	ks := newKeyTestServer(t)
	only := ks.keys(t)[0]
	rotateURL := "/api/providers/" + ks.provID + "/keys/" + only.ID + "/rotate"

	rotate := func(body, key string) *httptest.ResponseRecorder {
		return doReq(t, ks.srv, "POST", rotateURL, body, key)
	}
	hooksBefore := ks.hookCalls()
	rec := rotate(`{"api_key":"custom-key-2"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST rotate: status %d, body %s (want 200)", rec.Code, rec.Body.String())
	}
	var rot keyMutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rot); err != nil {
		t.Fatalf("rotate response is not JSON: %v", err)
	}
	if rot.ProviderID != ks.provID || rot.Key.ID != only.ID || rot.Key.Role != "preferred" {
		t.Fatalf("rotated = %+v, want the same key id + preferred role", rot)
	}
	if rot.Key.Fingerprint == "" || rot.Key.Fingerprint == only.Fingerprint {
		t.Fatalf("rotated fingerprint = %q, want a new non-empty one (was %q)", rot.Key.Fingerprint, only.Fingerprint)
	}
	if !rot.RoutingLive || rot.RoutingNote != "" {
		t.Fatalf("routing_live/note = %v/%q, want true/empty (hook wired)", rot.RoutingLive, rot.RoutingNote)
	}
	if ks.hookCalls() != hooksBefore+1 {
		t.Fatalf("hook calls = %d, want %d", ks.hookCalls(), hooksBefore+1)
	}
	// The response must never carry the key itself.
	if strings.Contains(rec.Body.String(), "custom-key-2") {
		t.Fatalf("rotate response leaks the key: %s", rec.Body.String())
	}
	// Still one key in the pool — rotation replaces, never adds.
	pool := ks.keys(t)
	if len(pool) != 1 || pool[0].ID != only.ID || pool[0].Fingerprint != rot.Key.Fingerprint {
		t.Fatalf("pool = %+v, want [rotated key]", pool)
	}
	// A second rotation keeps the same identity again.
	rec = rotate(`{"api_key":"custom-key-3"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST rotate again: status %d, body %s (want 200)", rec.Code, rec.Body.String())
	}
	var rot2 keyMutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rot2); err != nil {
		t.Fatalf("second rotate response is not JSON: %v", err)
	}
	if rot2.Key.ID != only.ID {
		t.Fatalf("second rotated id = %q, want %q", rot2.Key.ID, only.ID)
	}
	// Refusals: a 401 key, the same credential twice, unknown key /
	// provider, empty api_key, wrong gateway key.
	bad := rotate(`{"api_key":"sk-bad"}`, "test-key-123")
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("POST rotate with a 401 key: status %d, body %s (want 401)", bad.Code, bad.Body.String())
	}
	if got := ks.keys(t); len(got) != 1 || got[0].Fingerprint != rot2.Key.Fingerprint {
		t.Fatalf("pool changed after a refused rotate: %+v", got)
	}
	dup := rotate(`{"api_key":"custom-key-3"}`, "test-key-123")
	if dup.Code != http.StatusConflict {
		t.Fatalf("POST rotate with the current credential: status %d, want 409", dup.Code)
	}
	missing := doReq(t, ks.srv, "POST", "/api/providers/"+ks.provID+"/keys/key_nope/rotate", `{"api_key":"custom-key-9"}`, "test-key-123")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("POST rotate of unknown key: status %d, want 404", missing.Code)
	}
	noProv := doReq(t, ks.srv, "POST", "/api/providers/nope/keys/key_nope/rotate", `{"api_key":"custom-key-9"}`, "test-key-123")
	if noProv.Code != http.StatusNotFound {
		t.Fatalf("POST rotate of unknown provider: status %d, want 404", noProv.Code)
	}
	empty := rotate(`{"api_key":""}`, "test-key-123")
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("POST rotate with empty api_key: status %d, want 400", empty.Code)
	}
	unauth := rotate(`{"api_key":"custom-key-9"}`, "wrong-key")
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("POST rotate with wrong gateway key: status %d, want 401", unauth.Code)
	}
}

// TestProvidersFeedKeySpendAndCooling proves the GET /api/providers
// feed enriches each key with honest per-key spend (from the
// ledger) and live cooling status (from the engine) — never
// invented, never pooled into one number.
func TestProvidersFeedKeySpendAndCooling(t *testing.T) {
	mock := httptest.NewServer(keyAwareUpstream{}.handler())
	defer mock.Close()
	reg := provider.NewRegistry(nil, provider.WithRegistryAllowPrivateDestinations(true))
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	eng := &cooldownFakeEngine{}
	srv := newTestServer(t, eng, WithProviderRegistry(reg), WithLedger(led))
	rec := doReq(t, srv, "POST", "/api/providers", `{"base_url":"`+mock.URL+`","api_key":"custom-key-1","name":"MockUpstream"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup POST /api/providers: status %d, body %s", rec.Code, rec.Body.String())
	}
	var created addedProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("setup provider is not JSON: %v", err)
	}
	// Second key (reserve), then spend: 3 requests on the legacy
	// key, 1 on the reserve key — each attributed to its own key.
	add := doReq(t, srv, "POST", "/api/providers/"+created.ID+"/keys", `{"api_key":"custom-key-2","role":"reserve"}`, "test-key-123")
	if add.Code != http.StatusCreated {
		t.Fatalf("setup add key: status %d, body %s", add.Code, add.Body.String())
	}
	var added keyMutationResponse
	if err := json.Unmarshal(add.Body.Bytes(), &added); err != nil {
		t.Fatalf("add-key response is not JSON: %v", err)
	}
	p, _ := reg.Get(created.ID)
	firstKeyID := p.EffectiveKeys()[0].ID
	ctx := context.Background()
	prices := ledger.Prices{{Model: "mock-model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002}}
	// Spend is recorded against each key's real ID — this provider
	// was added via the API, so its first key has a fingerprint
	// and a real ID (the "" legacy fallback is covered by
	// TestProvidersFeedLegacyKeyFallback below).
	for i := 0; i < 3; i++ {
		if _, err := led.RecordUsage(ctx, ledger.Usage{
			GatewayKeyID: "gw-1", ProviderKeyID: firstKeyID, Provider: created.ID, Model: "mock-model-a",
			PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true, LatencyMS: 10,
		}, prices); err != nil {
			t.Fatalf("RecordUsage first key: %v", err)
		}
	}
	if _, err := led.RecordUsage(ctx, ledger.Usage{
		GatewayKeyID: "gw-1", ProviderKeyID: added.Key.ID, Provider: created.ID, Model: "mock-model-a",
		PromptTokens: 10, CompletionTokens: 0, SplitKnown: true, OK: true, LatencyMS: 10,
	}, prices); err != nil {
		t.Fatalf("RecordUsage reserve: %v", err)
	}
	// The reserve key is cooling (hard, 5 min) — the engine feed
	// must say so; the legacy key is not.
	eng.cooling = []router.CoolingTarget{{
		ProviderID: created.ID, KeyID: added.Key.ID, Mode: "hard",
		Until: time.Now().Add(5 * time.Minute), RemainingSeconds: 300,
	}}
	feed := doReq(t, srv, "GET", "/api/providers", "", "")
	if feed.Code != http.StatusOK {
		t.Fatalf("GET /api/providers: status %d", feed.Code)
	}
	var list apiProvidersResponse
	if err := json.Unmarshal(feed.Body.Bytes(), &list); err != nil {
		t.Fatalf("provider feed is not JSON: %v", err)
	}
	if len(list.Providers) != 1 || len(list.Providers[0].Keys) != 2 {
		t.Fatalf("feed = %+v, want 1 provider with 2 keys", list.Providers)
	}
	legacy := list.Providers[0].Keys[0]
	if legacy.ID != firstKeyID || legacy.Status != "active" {
		t.Fatalf("legacy key = %+v, want %s active", legacy, firstKeyID)
	}
	if !legacy.HasSpend || legacy.Requests != 3 || legacy.Tokens != 450 {
		t.Fatalf("legacy spend = %+v, want 3 requests / 450 tokens", legacy)
	}
	reserve := list.Providers[0].Keys[1]
	if reserve.ID != added.Key.ID || reserve.Role != "reserve" {
		t.Fatalf("reserve key = %+v, want %s reserve", reserve, added.Key.ID)
	}
	if reserve.Status != "cooling-reserve" || reserve.CoolingUntil == nil || reserve.CoolingMode != "hard" {
		t.Fatalf("reserve status = %+v, want cooling-reserve with until + hard mode", reserve)
	}
	if !reserve.HasSpend || reserve.Requests != 1 || reserve.Tokens != 10 {
		t.Fatalf("reserve spend = %+v, want 1 request / 10 tokens", reserve)
	}
}

// fakeProviderLister is a DashboardProviderLister returning a fixed
// list, so feed-enrichment rules can be tested without a registry.
type fakeProviderLister struct {
	providers []provider.DashboardProvider
}

func (f fakeProviderLister) DashboardProviders() []provider.DashboardProvider {
	return f.providers
}

// TestProvidersFeedLegacyKeyFallback proves the "" cooling/spend
// rows of a pre-pooling provider land on its legacy key — the one
// with no fingerprint — and on no other key. A provider whose
// legacy key was removed after pooling began leaves "" rows
// unattributed (covered by the no-match half: the second provider
// below has two fingerprinted keys, and its "" rows match neither).
func TestProvidersFeedLegacyKeyFallback(t *testing.T) {
	ctx := context.Background()
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	prices := ledger.Prices{{Model: "m"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002}}
	record := func(prov, keyID string, reqs int) {
		t.Helper()
		for i := 0; i < reqs; i++ {
			if _, err := led.RecordUsage(ctx, ledger.Usage{
				GatewayKeyID: "gw-1", ProviderKeyID: keyID, Provider: prov, Model: "m",
				PromptTokens: 100, CompletionTokens: 0, SplitKnown: true, OK: true, LatencyMS: 5,
			}, prices); err != nil {
				t.Fatalf("RecordUsage %s/%s: %v", prov, keyID, err)
			}
		}
	}
	record("prov-legacy", "", 3)      // pre-pooling rows: the legacy key served them
	record("prov-legacy", "key_b", 1) // the pooled reserve key's own row
	record("prov-nolegacy", "", 2)    // pre-pooling rows, legacy key since removed
	eng := &cooldownFakeEngine{cooling: []router.CoolingTarget{{
		ProviderID: "prov-legacy", KeyID: "", Mode: "hard",
		Until: time.Now().Add(5 * time.Minute), RemainingSeconds: 300,
	}}}
	lister := fakeProviderLister{providers: []provider.DashboardProvider{
		{ID: "prov-legacy", Name: "Legacy", Enabled: true, WireFormat: "openai", Verification: "unverified", Models: 1, Keys: []provider.DashboardProviderKey{
			{ID: "key_legacy", Role: "preferred", Fingerprint: "", CreatedAt: time.Now()},
			{ID: "key_b", Role: "reserve", Fingerprint: "abc123", CreatedAt: time.Now()},
		}},
		{ID: "prov-nolegacy", Name: "NoLegacy", Enabled: true, WireFormat: "openai", Verification: "unverified", Models: 1, Keys: []provider.DashboardProviderKey{
			{ID: "key_x", Role: "preferred", Fingerprint: "def456", CreatedAt: time.Now()},
			{ID: "key_y", Role: "preferred", Fingerprint: "789abc", CreatedAt: time.Now()},
		}},
	}}
	srv := newTestServer(t, eng, WithDashboardProviders(lister), WithLedger(led))
	feed := doReq(t, srv, "GET", "/api/providers", "", "")
	if feed.Code != http.StatusOK {
		t.Fatalf("GET /api/providers: status %d", feed.Code)
	}
	var list apiProvidersResponse
	if err := json.Unmarshal(feed.Body.Bytes(), &list); err != nil {
		t.Fatalf("provider feed is not JSON: %v", err)
	}
	if len(list.Providers) != 2 {
		t.Fatalf("feed providers = %d, want 2", len(list.Providers))
	}
	legacy := list.Providers[0].Keys[0]
	if legacy.ID != "key_legacy" || legacy.Status != "cooling" || legacy.CoolingUntil == nil {
		t.Fatalf("legacy key = %+v, want key_legacy cooling with an until", legacy)
	}
	if !legacy.HasSpend || legacy.Requests != 3 || legacy.Tokens != 300 {
		t.Fatalf("legacy spend = %+v, want the 3 unattributed rows", legacy)
	}
	reserve := list.Providers[0].Keys[1]
	if reserve.Status != "reserve" {
		t.Fatalf("reserve key status = %q, want reserve (not cooling — the cooling row was the legacy key's)", reserve.Status)
	}
	if !reserve.HasSpend || reserve.Requests != 1 || reserve.Tokens != 100 {
		t.Fatalf("reserve spend = %+v, want its own 1 row", reserve)
	}
	// No-legacy provider: neither key inherits the "" rows.
	for _, k := range list.Providers[1].Keys {
		if k.HasSpend {
			t.Fatalf("key %s inherited unattributed spend: %+v", k.ID, k)
		}
		if k.Status != "active" {
			t.Fatalf("key %s status = %q, want active", k.ID, k.Status)
		}
	}
}
