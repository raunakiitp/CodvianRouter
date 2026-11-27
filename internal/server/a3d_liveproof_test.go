package server

// A3 Stream D live-proof companion (in-process):
//
// The live binary proves the START surface end to end (real authorize
// URL from the claude handler, real device code from auth.kimi.com,
// the old identity login). A real provider login cannot be completed
// here, and production endpoints are deliberately NOT overridable
// (providers.SetEndpointsForTest is test-only), so the code exchange,
// device completion, Vault-sealed storage and the handler-based
// refresh-on-request path are proven IN-PROCESS through the REAL
// server routes (newTestServer → ServeHTTP → the /dashboard/oauth/
// providers/... handlers), with the handlers pointed at local mock
// OAuth servers through the documented test-only override.
//
//   - PKCE: start → complete over the real routes, tokens stored
//     Vault-sealed (ciphertext never the plaintext), served plaintext;
//   - device: start → pending → approved → complete, same storage;
//   - refresh: an expired stored token is refreshed through the
//     handler's documented grant on a real mock OAuth server when a
//     gateway request builds the provider's Authorization header —
//     the expired token is never served.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/crypto"
	"github.com/raunakiitp/codvian-router/internal/oauth/providers"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// newA3DVaultRouteServer is newTestServer (the REAL route mux) with a
// Vault-sealed connection store and a plain test-injectable client, so
// the mock OAuth servers stay reachable.
func newA3DVaultRouteServer(t *testing.T) (*Server, *oauthConnStore, *crypto.Vault) {
	t.Helper()
	vault, err := crypto.NewFromSecret([]byte("a3d-live-proof-vault-secret"), t.TempDir())
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{})
	st := newOAuthConnStore(vault)
	st.client = &http.Client{}
	srv.oauthStore = st
	return srv, st, vault
}

// TestA3DLiveProofPKCESealedViaRealRoutes: the Claude PKCE code flow
// through the real routes, with the exchange hitting a local mock OAuth
// server; the connection lands in the Vault-sealed store.
func TestA3DLiveProofPKCESealedViaRealRoutes(t *testing.T) {
	srv, st, _ := newA3DVaultRouteServer(t)

	m := &tokenMock{}
	mock := m.mockServer(t, "claude")
	defer mock.Close()
	providers.SetEndpointsForTest(t, "claude", map[string]string{"token": mock.URL + "/token"})

	const key = "test-key-123"

	rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/claude/start",
		`{"redirect_uri":"http://127.0.0.1:3000/callback"}`, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: status %d body %s", rec.Code, rec.Body.String())
	}
	var start struct {
		AuthorizeURL string `json:"authorize_url"`
		Verifier     string `json:"verifier"`
		State        string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start body: %v", err)
	}
	if !strings.HasPrefix(start.AuthorizeURL, "https://claude.ai/oauth/authorize?") {
		t.Fatalf("authorize_url = %q, want the documented claude.ai authorize URL", start.AuthorizeURL)
	}
	t.Logf("PKCE start issued authorize_url=%s verifier=<redacted len=%d> state=<redacted len=%d>",
		start.AuthorizeURL[:60]+"...", len(start.Verifier), len(start.State))

	rec = providerRoutePost(t, srv, "/dashboard/oauth/providers/claude/complete",
		`{"code":"code-claude","redirect_uri":"http://127.0.0.1:3000/callback","verifier":"`+start.Verifier+`"}`, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: status %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "claude-at-1") {
		t.Fatalf("complete response leaked the access token: %s", rec.Body.String())
	}

	// The stored copy is sealed; the served copy is plaintext.
	st.mu.Lock()
	c := st.conns["claude"]
	sealedAccess, sealedRefresh := c.AccessToken, c.RefreshToken
	st.mu.Unlock()
	if c == nil {
		t.Fatal("claude connection not stored")
	}
	if sealedAccess == "" || strings.Contains(sealedAccess, "claude-at-1") {
		t.Errorf("stored access token is not sealed: %q", sealedAccess)
	}
	if sealedRefresh == "" || strings.Contains(sealedRefresh, "claude-rt-1") {
		t.Errorf("stored refresh token is not sealed: %q", sealedRefresh)
	}
	t.Logf("stored (sealed) access token: %s... (%d bytes, plaintext NOT present)",
		sealedAccess[:8], len(sealedAccess))

	tok, err := srv.OAuthProviderAccessToken("claude")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tok != "claude-at-1" {
		t.Fatalf("served token = %q, want claude-at-1", tok)
	}

	// Expired: the next resolve REFRESHES through the handler's grant on
	// the mock — never serves the stale token.
	expireBridgeConn(t, st, "claude")
	tok2, err := srv.OAuthProviderAccessToken("claude")
	if err != nil {
		t.Fatalf("resolve after expiry: %v", err)
	}
	if tok2 != "claude-at-2" {
		t.Fatalf("after expiry served %q, want the refreshed claude-at-2", tok2)
	}
	sawRefresh := false
	for _, g := range m.grants {
		if g == "refresh_token" {
			sawRefresh = true
		}
	}
	if !sawRefresh {
		t.Errorf("mock OAuth server never saw a refresh_token grant (grants=%v)", m.grants)
	}
	t.Logf("mock OAuth server observed grants=%v; expired token was refreshed, not served", m.grants)
}

// TestA3DLiveProofDeviceSealedViaRealRoutes: the Kimi Code CLI device
// flow through the real routes, completed against a local mock OAuth
// server, tokens stored Vault-sealed.
func TestA3DLiveProofDeviceSealedViaRealRoutes(t *testing.T) {
	srv, st, _ := newA3DVaultRouteServer(t)

	mock := newDeviceFlowMock()
	hs := mock.server(t)
	defer hs.Close()
	providers.SetEndpointsForTest(t, "kimi-coding", map[string]string{
		"device": hs.URL + "/device",
		"token":  hs.URL + "/token",
	})

	const key = "test-key-123"
	rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/kimi-coding/device/start", "{}", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("device start: status %d body %s", rec.Code, rec.Body.String())
	}
	var start struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("device start body: %v", err)
	}
	if start.DeviceCode == "" || start.UserCode == "" {
		t.Fatalf("device start incomplete: %+v", start)
	}
	t.Logf("device start issued user_code=%s device_code=<redacted len=%d>", start.UserCode, len(start.DeviceCode))

	rec = providerRoutePost(t, srv, "/dashboard/oauth/providers/kimi-coding/device/complete",
		`{"device_code":"`+start.DeviceCode+`"}`, key)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pending"`) {
		t.Fatalf("device complete (pending): status %d body %s", rec.Code, rec.Body.String())
	}

	mock.release()
	rec = providerRoutePost(t, srv, "/dashboard/oauth/providers/kimi-coding/device/complete",
		`{"device_code":"`+start.DeviceCode+`"}`, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("device complete (approved): status %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "kimi-at-1") {
		t.Fatalf("device complete leaked the access token: %s", rec.Body.String())
	}

	st.mu.Lock()
	c := st.conns["kimi-coding"]
	sealed := ""
	if c != nil {
		sealed = c.AccessToken
	}
	st.mu.Unlock()
	if c == nil {
		t.Fatal("kimi-coding connection not stored")
	}
	if sealed == "" || strings.Contains(sealed, "kimi-at-1") {
		t.Errorf("stored kimi access token is not sealed: %q", sealed)
	}
	tok, err := srv.OAuthProviderAccessToken("kimi-coding")
	if err != nil || tok != "kimi-at-1" {
		t.Fatalf("served token = %q err=%v, want kimi-at-1", tok, err)
	}
	t.Logf("device flow stored sealed token (%d bytes) and serves plaintext kimi-at-1", len(sealed))
}

// TestA3DLiveProofGatewayRequestRefreshesExpiredToken: after a real
// route-completed connection, the gateway's provider-auth path (the
// registry resolver installed in production by
// InstallOAuthProviderResolver) builds the Authorization header for the
// provider. With an EXPIRED stored token the header carries the token
// refreshed through the handler's grant — the mock OAuth server records
// the refresh call, and the expired token is never used.
func TestA3DLiveProofGatewayRequestRefreshesExpiredToken(t *testing.T) {
	srv, st, vault := newA3DVaultRouteServer(t)

	m := &tokenMock{}
	mock := m.mockServer(t, "claude")
	defer mock.Close()
	providers.SetEndpointsForTest(t, "claude", map[string]string{"token": mock.URL + "/token"})

	// Complete the connection through the REAL route.
	const key = "test-key-123"
	rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/claude/start",
		`{"redirect_uri":"http://127.0.0.1:3000/callback"}`, key)
	var start struct {
		Verifier string `json:"verifier"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start body: %v", err)
	}
	rec = providerRoutePost(t, srv, "/dashboard/oauth/providers/claude/complete",
		`{"code":"code-claude","redirect_uri":"http://127.0.0.1:3000/callback","verifier":"`+start.Verifier+`"}`, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: status %d body %s", rec.Code, rec.Body.String())
	}

	if _, err := srv.tosGateFor().Acknowledge("claude", "a3d-live-proof"); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}

	var captured map[string]string
	newReg := func() *provider.Registry {
		sealed, serr := vault.Seal([]byte("ignored-static-key"))
		if serr != nil {
			t.Fatalf("seal: %v", serr)
		}
		store := &mapStore{configs: []provider.ProviderConfig{{
			ID: "p-claude", Name: "Claude Code", BaseURL: "https://api.anthropic.com",
			APIKeyEncrypted: sealed, Enabled: true, CatalogID: "claude", CreatedAt: time.Now().UTC(),
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

	// Fresh token: the gateway header carries the stored token.
	reg := newReg()
	if _, err := reg.ClientFor("p-claude"); err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if got := captured["Authorization"]; got != "Bearer claude-at-1" {
		t.Fatalf("fresh gateway Authorization = %q, want Bearer claude-at-1", got)
	}
	t.Logf("fresh gateway request built Authorization=%q", captured["Authorization"])

	// Expire the stored token: the next gateway request MUST refresh.
	expireBridgeConn(t, st, "claude")
	reg2 := newReg()
	if _, err := reg2.ClientFor("p-claude"); err != nil {
		t.Fatalf("ClientFor after expiry: %v", err)
	}
	if got := captured["Authorization"]; got != "Bearer claude-at-2" {
		t.Fatalf("expired gateway Authorization = %q, want the refreshed Bearer claude-at-2 (never the stale token)", got)
	}
	sawRefresh := false
	for _, g := range m.grants {
		if g == "refresh_token" {
			sawRefresh = true
		}
	}
	if !sawRefresh {
		t.Errorf("mock OAuth server never saw a refresh_token grant (grants=%v)", m.grants)
	}
	t.Logf("expired gateway request built Authorization=%q; mock OAuth grants=%v", captured["Authorization"], m.grants)
	_ = context.Background()
}
