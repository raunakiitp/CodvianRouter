package server

// Bridge end-to-end tests (A3): the per-provider OAuth handlers driven
// through the REAL bridge — authorize/device start → code exchange or
// device completion through the store's client → Vault-sealed (when
// configured) storage → expired token REFRESHED (not served) → request
// headers built through the real provider registry resolver
// (SetOAuthTokenResolver → oauthHeadersFor). No stub resolvers.
//
// The providers-package tests pin each handler's documented request
// shape; these tests pin the storage/refresh/bridge chain.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/crypto"
	"github.com/raunakiitp/codvian-router/internal/oauth/providers"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// newBridgeServer returns a Server wired with an empty connection
// store (no vault — use newBridgeVaultServer for the sealed variant).
// The store's client is the test-injectable plain client (the same
// seam oauth_conn.go's tests use) so the mocks stay reachable; the
// SSRF-guarded-client behavior is proven separately by
// TestBridgeExchangeCannotReachPrivateHosts with a nil client.
func newBridgeServer(t *testing.T) (*Server, *oauthConnStore) {
	t.Helper()
	srv := &Server{}
	st := newOAuthConnStore(nil)
	st.client = &http.Client{}
	srv.oauthStore = st
	return srv, st
}

// newBridgeServerGuarded is newBridgeServer with the store client
// left nil, so every handler call goes through the SSRF-guarded
// egress client (what production does).
func newBridgeServerGuarded(t *testing.T) (*Server, *oauthConnStore) {
	t.Helper()
	srv := &Server{}
	st := newOAuthConnStore(nil)
	srv.oauthStore = st
	return srv, st
}

func newBridgeVaultServer(t *testing.T) (*Server, *oauthConnStore) {
	t.Helper()
	v, err := crypto.NewFromSecret([]byte("bridge-test-vault-secret"), t.TempDir())
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	srv := &Server{}
	st := newOAuthConnStore(v)
	st.client = &http.Client{}
	srv.oauthStore = st
	return srv, st
}

func expireBridgeConn(t *testing.T, st *oauthConnStore, id string) {
	t.Helper()
	st.mu.Lock()
	if c, ok := st.conns[id]; ok {
		c.ExpiresAt = time.Now().Add(-time.Minute)
	}
	st.mu.Unlock()
}

func setConnExpiry(t *testing.T, st *oauthConnStore, id string, at time.Time) {
	t.Helper()
	st.mu.Lock()
	if c, ok := st.conns[id]; ok {
		c.ExpiresAt = at
	}
	st.mu.Unlock()
}

// tokenMock serves a /token endpoint (form AND JSON bodies) logging
// grant types, answering provider-prefixed token pairs.
type tokenMock struct {
	grants     []string
	refreshes  []string
	exchanges  int
	copilotJWT int
}

func (m *tokenMock) mockServer(t *testing.T, id string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		writeBridgeJSON(t, w, map[string]any{
			"device_code": "dc-" + id, "user_code": "USER-" + strings.ToUpper(id),
			"verification_uri": "https://example.com/device", "interval": 5, "expires_in": 300,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		body := readBridgeBody(t, r)
		m.grants = append(m.grants, body["grant_type"])
		m.refreshes = append(m.refreshes, body["refresh_token"])
		w.Header().Set("Content-Type", "application/json")
		switch body["grant_type"] {
		case "authorization_code", "urn:ietf:params:oauth:grant-type:device_code", "device_code", "":
			fmt.Fprintf(w, `{"access_token":"%s-at-1","refresh_token":"%s-rt-1","token_type":"bearer","expires_in":3600,"scope":"bridge"}`, id, id)
		case "refresh_token":
			fmt.Fprintf(w, `{"access_token":"%s-at-2","refresh_token":"%s-rt-2","token_type":"bearer","expires_in":3600,"scope":"bridge"}`, id, id)
		default:
			t.Errorf("unexpected grant_type %q", body["grant_type"])
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	return httptest.NewServer(mux)
}

// readBridgeBody decodes a form or JSON request body into a flat map.
func readBridgeBody(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Fatalf("decode JSON body: %v", err)
		}
		out := map[string]string{}
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
		return out
	}
	if err := r.ParseForm(); err != nil {
		t.Fatalf("parse form: %v", err)
	}
	out := map[string]string{}
	for k := range r.Form {
		out[k] = r.Form.Get(k)
	}
	return out
}

// copilotMock serves the GitHub device flow + Copilot token exchange.
func copilotMock(t *testing.T, m *tokenMock) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		writeBridgeJSON(t, w, map[string]any{
			"device_code": "dc-bridge", "user_code": "BRDG-1234",
			"verification_uri": "https://github.com/login/device", "interval": 5, "expires_in": 899,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		body := readBridgeBody(t, r)
		m.grants = append(m.grants, body["grant_type"])
		writeBridgeJSON(t, w, map[string]any{"access_token": "gho-bridge-1", "token_type": "bearer"})
	})
	mux.HandleFunc("/exchange", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token gho-bridge-1" {
			t.Errorf("exchange Authorization = %q", got)
		}
		m.exchanges++
		m.copilotJWT++
		writeBridgeJSON(t, w, map[string]any{
			"token":      fmt.Sprintf("copilot-jwt-%d", m.copilotJWT),
			"expires_in": 1800,
		})
	})
	return httptest.NewServer(mux)
}

// clineMock serves the WorkOS device flow + Cline register/refresh.
func clineMock(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		writeBridgeJSON(t, w, map[string]any{
			"device_code": "dc-cline", "user_code": "CLIN-BRDG",
			"verification_uri": "https://auth.cline.bot/device", "interval": 5, "expires_in": 300,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeBridgeJSON(t, w, map[string]any{"access_token": "wk-access-1", "refresh_token": "wk-refresh-1"})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		writeBridgeJSON(t, w, map[string]any{
			"success": true,
			"data": map[string]any{
				"accessToken": "workos:cl-at-1", "refreshToken": "cl-rt-1",
				"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
		})
	})
	mux.HandleFunc("/refresh", func(w http.ResponseWriter, r *http.Request) {
		body := readBridgeBody(t, r)
		if body["granttype"] != "refresh_token" || body["refreshtoken"] != "cl-rt-1" {
			t.Errorf("refresh body = %v", body)
		}
		writeBridgeJSON(t, w, map[string]any{
			"success": true,
			"data": map[string]any{
				"accessToken": "workos:cl-at-2", "refreshToken": "cl-rt-2",
				"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
		})
	})
	return httptest.NewServer(mux)
}

func writeBridgeJSON(t *testing.T, w http.ResponseWriter, fields map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(fields); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

// TestBridgeCodeFlowProvidersE2E: authorize URL → code exchange →
// stored → fresh served → expired REFRESHED (not served).
func TestBridgeCodeFlowProvidersE2E(t *testing.T) {
	for _, id := range []string{"claude", "codex", "chatgpt", "gitlab-duo"} {
		t.Run(id, func(t *testing.T) {
			if id == "gitlab-duo" {
				t.Setenv("CODVIAN_OAUTH_GITLAB_DUO_CLIENT_ID", "bridge-gitlab-app")
			}
			m := &tokenMock{}
			mock := m.mockServer(t, id)
			defer mock.Close()
			providers.SetEndpointsForTest(t, id, map[string]string{"token": mock.URL + "/token"})

			srv, st := newBridgeServer(t)
			_, authURL, verifier, err := srv.StartProviderOAuth(id, "http://127.0.0.1:3000/callback")
			if err != nil {
				t.Fatalf("StartProviderOAuth: %v", err)
			}
			if authURL == "" || verifier == "" {
				t.Fatalf("authorize URL/verifier empty")
			}
			if err := srv.CompleteProviderOAuth(id, "code-"+id, "http://127.0.0.1:3000/callback", verifier); err != nil {
				t.Fatalf("CompleteProviderOAuth: %v", err)
			}
			tok, err := srv.OAuthProviderAccessToken(id)
			if err != nil {
				t.Fatalf("resolve fresh: %v", err)
			}
			if tok != id+"-at-1" {
				t.Errorf("fresh token = %q, want %q", tok, id+"-at-1")
			}
			// Expire the stored token: the next resolve REFRESHES.
			expireBridgeConn(t, st, id)
			tok2, err := srv.OAuthProviderAccessToken(id)
			if err != nil {
				t.Fatalf("resolve after expiry: %v", err)
			}
			if tok2 != id+"-at-2" {
				t.Errorf("refreshed token = %q, want %q (never served stale)", tok2, id+"-at-2")
			}
			found := false
			for _, g := range m.grants {
				if g == "refresh_token" {
					found = true
				}
			}
			if !found {
				t.Errorf("mock never saw a refresh_token grant (grants=%v)", m.grants)
			}
		})
	}
}

// TestBridgeDeviceFlowProvidersE2E: device start → pending → completion
// → stored → fresh served → expired REFRESHED.
func TestBridgeDeviceFlowProvidersE2E(t *testing.T) {
	type devFixture struct {
		setup   func(t *testing.T) *httptest.Server
		pending bool // expect the first completion to report pending
	}
	fixtures := map[string]devFixture{
		"github-copilot": {setup: func(t *testing.T) *httptest.Server { return copilotMock(t, &tokenMock{}) }, pending: false},
		"cline":          {setup: func(t *testing.T) *httptest.Server { return clineMock(t) }, pending: false},
		"clinepass":      {setup: func(t *testing.T) *httptest.Server { return clineMock(t) }, pending: false},
		"kimi-coding": {setup: func(t *testing.T) *httptest.Server {
			m := &tokenMock{}
			return m.mockServer(t, "kimi-coding")
		}, pending: false}, // tokenMock also serves /device for the device start
	}
	for _, id := range []string{"github-copilot", "cline", "clinepass", "kimi-coding"} {
		t.Run(id, func(t *testing.T) {
			fx := fixtures[id]
			mock := fx.setup(t)
			defer mock.Close()
			switch id {
			case "github-copilot":
				providers.SetEndpointsForTest(t, id, map[string]string{
					"device": mock.URL + "/device", "token": mock.URL + "/token", "exchange": mock.URL + "/exchange",
				})
			case "cline", "clinepass":
				providers.SetEndpointsForTest(t, id, map[string]string{
					"device": mock.URL + "/device", "poll": mock.URL + "/token",
					"register": mock.URL + "/register", "refresh": mock.URL + "/refresh",
				})
			case "kimi-coding":
				providers.SetEndpointsForTest(t, id, map[string]string{
					"device": mock.URL + "/device", "token": mock.URL + "/token",
				})
			}

			srv, st := newBridgeServer(t)
			dc, err := srv.StartProviderDevice(id)
			if err != nil {
				t.Fatalf("StartProviderDevice: %v", err)
			}
			if dc.UserCode == "" || dc.VerificationURI == "" {
				t.Fatalf("device offer incomplete: %+v", dc)
			}
			tr, err := srv.CompleteProviderDevice(id, dc)
			if err != nil {
				t.Fatalf("CompleteProviderDevice: %v", err)
			}
			if tr == nil || tr.AccessToken == "" {
				t.Fatalf("completion produced no token")
			}
			tok, err := srv.OAuthProviderAccessToken(id)
			if err != nil {
				t.Fatalf("resolve fresh: %v", err)
			}
			if tok != tr.AccessToken {
				t.Errorf("resolved token = %q, want the completed %q", tok, tr.AccessToken)
			}
			// Expire: the next resolve must REFRESH through the
			// handler's documented grant.
			expireBridgeConn(t, st, id)
			tok2, err := srv.OAuthProviderAccessToken(id)
			if err != nil {
				t.Fatalf("resolve after expiry: %v", err)
			}
			if tok2 == tok {
				t.Errorf("served the SAME stale token after expiry")
			}
		})
	}
}

// TestBridgeVaultSealedStorage proves the stored copy is sealed (and
// the served token is plaintext) when a vault is configured.
func TestBridgeVaultSealedStorage(t *testing.T) {
	m := &tokenMock{}
	mock := m.mockServer(t, "claude")
	defer mock.Close()
	providers.SetEndpointsForTest(t, "claude", map[string]string{"token": mock.URL + "/token"})

	srv, st := newBridgeVaultServer(t)
	_, _, verifier, err := srv.StartProviderOAuth("claude", "http://127.0.0.1:3000/callback")
	if err != nil {
		t.Fatalf("StartProviderOAuth: %v", err)
	}
	if err := srv.CompleteProviderOAuth("claude", "code-vault", "http://127.0.0.1:3000/callback", verifier); err != nil {
		t.Fatalf("CompleteProviderOAuth: %v", err)
	}
	st.mu.Lock()
	stored := st.conns["claude"]
	sealedAccess := stored.AccessToken
	sealedRefresh := stored.RefreshToken
	st.mu.Unlock()
	if sealedAccess == "" || strings.Contains(sealedAccess, "claude-at-1") {
		t.Errorf("stored access token is not sealed: %q", sealedAccess)
	}
	if sealedRefresh == "" || strings.Contains(sealedRefresh, "claude-rt-1") {
		t.Errorf("stored refresh token is not sealed: %q", sealedRefresh)
	}
	tok, err := srv.OAuthProviderAccessToken("claude")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tok != "claude-at-1" {
		t.Errorf("resolved plaintext token = %q", tok)
	}
	// Refresh under the vault: rotated pair resealed, plaintext served.
	expireBridgeConn(t, st, "claude")
	tok2, err := srv.OAuthProviderAccessToken("claude")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok2 != "claude-at-2" {
		t.Errorf("refreshed token = %q", tok2)
	}
	st.mu.Lock()
	stillSealed := st.conns["claude"].AccessToken
	st.mu.Unlock()
	if strings.Contains(stillSealed, "claude-at-2") {
		t.Errorf("rotated token stored unsealed: %q", stillSealed)
	}
}

// stubUpstreamClient satisfies provider.Client for the capture factory.
type stubUpstreamClient struct{}

func (stubUpstreamClient) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("stub client")
}

func (stubUpstreamClient) ChatStream(context.Context, provider.ChatRequest, func(string) error) (*provider.ChatResponse, error) {
	return nil, errors.New("stub client")
}

func (stubUpstreamClient) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}

func (stubUpstreamClient) TestKey(context.Context) error { return nil }

// mapStore is a provider.Store the test seeds directly: the ONLY
// exported way to land a catalog-linked provider in the registry
// (AddCatalog refuses OAuth surfaces at add time by design; config
// loads carry catalog_id).
type mapStore struct{ configs []provider.ProviderConfig }

func (s *mapStore) Load() ([]provider.ProviderConfig, error) { return s.configs, nil }
func (s *mapStore) Save(cs []provider.ProviderConfig) error  { s.configs = cs; return nil }

// TestBridgeHeadersThroughRealRegistry proves the full routing chain
// through the REAL provider registry, post-ToS-gate:
//
//	claude OAuth flow (bridge) → Vault-shaped store → ToS risk
//	acknowledgement seeded via the gate's store API → registry config
//	load of the catalog-linked OAuth surface → ClientFor resolves
//	Authorization headers through THIS bridge's resolver (the real
//	SetOAuthTokenResolver path) → an expired token is REFRESHED and a
//	cold-cache registry sees the NEW token in the Bearer header.
//
// claude is ToS-grey, so the test seeds the acknowledgement exactly the
// way production does (the gate store API from tos_gate.go). The
// add-path refusal for OAuth surfaces (AddCatalogWithOptions treats
// them like un-implemented exchanges) is a shared-file follow-up —
// see the PR body.
func TestBridgeHeadersThroughRealRegistry(t *testing.T) {
	m := &tokenMock{}
	mock := m.mockServer(t, "claude")
	defer mock.Close()
	providers.SetEndpointsForTest(t, "claude", map[string]string{"token": mock.URL + "/token"})

	srv, st := newBridgeServer(t)
	_, _, verifier, err := srv.StartProviderOAuth("claude", "http://127.0.0.1:3000/callback")
	if err != nil {
		t.Fatalf("StartProviderOAuth: %v", err)
	}
	if err := srv.CompleteProviderOAuth("claude", "code-bridge", "http://127.0.0.1:3000/callback", verifier); err != nil {
		t.Fatalf("CompleteProviderOAuth: %v", err)
	}

	// Seed the ToS risk acknowledgement through the gate's store API
	// (claude is ToS-grey — the gate refuses add/load/request without it).
	if _, err := srv.tosGateFor().Acknowledge("claude", "bridge-test-actor"); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}

	var captured map[string]string
	vault, err := crypto.NewFromSecret([]byte("bridge-registry-vault-secret"), t.TempDir())
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	newReg := func() *provider.Registry {
		sealed, serr := vault.Seal([]byte("ignored-static-key"))
		if serr != nil {
			t.Fatalf("seal: %v", serr)
		}
		store := &mapStore{configs: []provider.ProviderConfig{{
			ID:              "p-claude",
			Name:            "Claude Code",
			BaseURL:         "https://api.anthropic.com",
			APIKeyEncrypted: sealed,
			Enabled:         true,
			CatalogID:       "claude",
			CreatedAt:       time.Now().UTC(),
		}}}
		reg := provider.NewRegistry(store,
			provider.WithCipher(provider.NewVaultCipher(vault)),
			provider.WithClientFactory(func(baseURL, apiKey string, authHeaders map[string]string, wireFormat string) (provider.Client, error) {
				captured = authHeaders
				return stubUpstreamClient{}, nil
			}),
			provider.WithRegistryAllowPrivateDestinations(true))
		reg.SetToSAcknowledgement(srv.ToSAcknowledged)
		if err := reg.Load(); err != nil {
			t.Fatalf("registry Load: %v", err)
		}
		InstallOAuthProviderResolver(reg, srv)
		return reg
	}
	reg := newReg()
	if _, err := reg.ClientFor("p-claude"); err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if got := captured["Authorization"]; got != "Bearer claude-at-1" {
		t.Errorf("headers Authorization = %q, want Bearer claude-at-1", got)
	}
	if got := captured["anthropic-beta"]; got != "oauth-2025-04-20" {
		t.Errorf("headers anthropic-beta = %q, want the catalog's oauth beta header", got)
	}

	// Expire the stored token: a cold-cache registry must see the
	// REFRESHED token in the headers — never the stale one.
	expireBridgeConn(t, st, "claude")
	reg2 := newReg()
	if _, err := reg2.ClientFor("p-claude"); err != nil {
		t.Fatalf("ClientFor 2: %v", err)
	}
	if got := captured["Authorization"]; got != "Bearer claude-at-2" {
		t.Errorf("refreshed headers Authorization = %q, want Bearer claude-at-2 (expired token must be refreshed, not served)", got)
	}
}

// TestBridgeExchangeCannotReachPrivateHosts proves the code exchange
// goes through the SSRF-guarded egress client: a handler endpoint
// pointing at loopback or link-local is BLOCKED, never dialed.
func TestBridgeExchangeCannotReachPrivateHosts(t *testing.T) {
	srv, _ := newBridgeServerGuarded(t)
	// st.client intentionally nil: oauthTokenClient() returns the
	// guarded egress client (audit AUD-SEC-005 pattern).

	t.Run("loopback", func(t *testing.T) {
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("token endpoint MUST NOT be reached through the guarded client")
		}))
		defer mock.Close()
		providers.SetEndpointsForTest(t, "claude", map[string]string{"token": mock.URL + "/token"})
		err := srv.CompleteProviderOAuth("claude", "code-x", "http://127.0.0.1:54545/callback", "verifier-x")
		if err == nil {
			t.Fatal("loopback exchange succeeded — the SSRF guard did not engage")
		}
		if !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "blocked") {
			t.Errorf("error = %v, want an egress-guard loopback refusal", err)
		}
	})
	t.Run("metadata-link-local", func(t *testing.T) {
		providers.SetEndpointsForTest(t, "claude", map[string]string{"token": "http://169.254.169.254/latest/meta-data"})
		err := srv.CompleteProviderOAuth("claude", "code-x", "http://127.0.0.1:54545/callback", "verifier-x")
		if err == nil {
			t.Fatal("link-local exchange succeeded — the SSRF guard did not engage")
		}
		if !strings.Contains(err.Error(), "blocked") && !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "link") {
			t.Errorf("error = %v, want an egress-guard refusal", err)
		}
	})
}

// TestBridgePasteImportE2E: allowlisted blob import → stored → served
// → expired (no refresh endpoint) refused honestly; non-allowlisted
// providers and mismatched blobs refused.
func TestBridgePasteImportE2E(t *testing.T) {
	srv, st := newBridgeServer(t)
	blob := fmt.Sprintf(`{"provider":"antigravity","access_token":"agy-at-1","refresh_token":"agy-rt-1","expires_at":%q}`,
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	if err := srv.ImportProviderBlob("antigravity", []byte(blob)); err != nil {
		t.Fatalf("ImportProviderBlob: %v", err)
	}
	tok, err := srv.OAuthProviderAccessToken("antigravity")
	if err != nil {
		t.Fatalf("resolve imported: %v", err)
	}
	if tok != "agy-at-1" {
		t.Errorf("imported token = %q", tok)
	}

	// Expired with NO refresh endpoint: honest error, no invention.
	setConnExpiry(t, st, "antigravity", time.Now().Add(-time.Minute))
	if _, err := srv.OAuthProviderAccessToken("antigravity"); err == nil ||
		!strings.Contains(err.Error(), "cannot be refreshed") {
		t.Errorf("expired import error = %v, want an honest cannot-be-refreshed refusal", err)
	}

	// Non-allowlisted route: refused.
	if err := srv.ImportProviderBlob("cursor", []byte(`{"provider":"cursor","access_token":"t"}`)); err == nil {
		t.Errorf("import for cursor succeeded — allowlist broken")
	}
	// Blob provider mismatch: refused.
	if err := srv.ImportProviderBlob("agy", []byte(`{"provider":"antigravity","access_token":"t"}`)); err == nil {
		t.Errorf("mismatched blob succeeded — provider-match broken")
	}
}

// TestBridgeRefusals: unimplemented providers are refused with their
// documented reason; unknown/never-connected providers error like any
// missing connection; import-only providers point at the import path.
func TestBridgeRefusals(t *testing.T) {
	srv, _ := newBridgeServer(t)
	_, err := srv.OAuthProviderAccessToken("cursor")
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("cursor resolution error = %v, want the honest unimplemented refusal", err)
	}
	if _, err := srv.OAuthProviderAccessToken("claude"); err == nil || !strings.Contains(err.Error(), "no connection") {
		t.Errorf("unconnected claude error = %v, want a no-connection error", err)
	}
	_, err = srv.OAuthProviderAccessToken("antigravity")
	if err == nil || !strings.Contains(err.Error(), "no connection") {
		t.Errorf("unconnected antigravity error = %v, want a no-connection error (connect via the paste import)", err)
	}
	_, _, _, err = srv.StartProviderOAuth("antigravity", "http://127.0.0.1/cb")
	if err == nil || !strings.Contains(err.Error(), "import") {
		t.Errorf("antigravity StartProviderOAuth error = %v, want the import-only pointer", err)
	}
	_, err = srv.StartProviderDevice("claude")
	if err == nil || !strings.Contains(err.Error(), "device") {
		t.Errorf("claude device start error = %v, want ErrNotDeviceFlow", err)
	}

	_, _, _, err = srv.StartProviderOAuth("github-copilot", "http://127.0.0.1/cb")
	if err == nil || !strings.Contains(err.Error(), "authorization URL") {
		t.Errorf("github-copilot StartProviderOAuth error = %v, want ErrNotCodeFlow", err)
	}
}
