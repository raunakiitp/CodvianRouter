package oauth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestIssuer(t *testing.T) *Issuer {
	t.Helper()
	iss, err := NewIssuer("")
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss
}

func registerPublicClient(t *testing.T, iss *Issuer) *Client {
	t.Helper()
	c, secret, err := iss.RegisterClient("test-client", []string{"http://localhost:8080/callback"}, []string{"chat", "models"}, true)
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	if secret != "" {
		t.Fatalf("public client returned secret %q, want empty", secret)
	}
	return c
}

// authorizeFor runs the authorize step for a public client and
// returns the single-use code plus the verifier that redeems it.
func authorizeFor(t *testing.T, iss *Issuer, c *Client) (code, verifier string) {
	t.Helper()
	verifier, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier: %v", err)
	}
	code, err = iss.Authorize(c.ID, c.RedirectURIs[0], nil, CodeChallengeS256(verifier), "S256", "key-admin", []string{"admin"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	return code, verifier
}

func TestIssuerAuthorizeExchangeVerifyRoundTrip(t *testing.T) {
	iss := newTestIssuer(t)
	c := registerPublicClient(t, iss)
	code, verifier := authorizeFor(t, iss, c)

	resp, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if resp.TokenType != "Bearer" {
		t.Errorf("TokenType = %q, want Bearer", resp.TokenType)
	}
	if resp.ExpiresIn != int(AccessTokenTTL/time.Second) {
		t.Errorf("ExpiresIn = %d, want %d", resp.ExpiresIn, int(AccessTokenTTL/time.Second))
	}
	if resp.AccessToken == "" {
		t.Error("AccessToken is empty")
	}
	if resp.RefreshToken == "" {
		t.Error("RefreshToken is empty")
	}
	if resp.Scope != "chat models" {
		t.Errorf("Scope = %q, want \"chat models\"", resp.Scope)
	}

	claims, err := iss.VerifyAccessToken(resp.AccessToken)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if claims.ClientID != c.ID {
		t.Errorf("ClientID = %q, want %q", claims.ClientID, c.ID)
	}
	if len(claims.Scopes) != 2 || claims.Scopes[0] != "chat" || claims.Scopes[1] != "models" {
		t.Errorf("Scopes = %v, want [chat models]", claims.Scopes)
	}
	if claims.JTI == "" {
		t.Error("JTI is empty")
	}
	if time.Until(claims.ExpiresAt) > AccessTokenTTL || time.Until(claims.ExpiresAt) <= 0 {
		t.Errorf("ExpiresAt = %v, want ~1h in the future", claims.ExpiresAt)
	}

	scopes, ok := iss.TokenScopes(resp.AccessToken)
	if !ok {
		t.Fatal("TokenScopes: ok = false, want true")
	}
	if len(scopes) != 2 {
		t.Errorf("TokenScopes = %v, want 2 scopes", scopes)
	}
	if _, ok := iss.TokenScopes("garbage-token"); ok {
		t.Error("TokenScopes(garbage): ok = true, want false")
	}
}

func TestIssuerExchangeWrongVerifierRejected(t *testing.T) {
	iss := newTestIssuer(t)
	c := registerPublicClient(t, iss)
	code, _ := authorizeFor(t, iss, c)

	wrong, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier: %v", err)
	}
	if _, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], wrong, ""); err == nil {
		t.Fatal("ExchangeCode(wrong verifier): err = nil, want an error")
	}
	// The code is consumed whether the exchange succeeds or not:
	// the correct verifier cannot redeem it afterwards.
	if _, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], wrong, ""); err == nil {
		t.Fatal("ExchangeCode(reused code): err = nil, want an error")
	}
}

func TestIssuerCodeSingleUse(t *testing.T) {
	iss := newTestIssuer(t)
	c := registerPublicClient(t, iss)
	code, verifier := authorizeFor(t, iss, c)

	if _, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, ""); err != nil {
		t.Fatalf("ExchangeCode first use: %v", err)
	}
	if _, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, ""); err == nil {
		t.Fatal("ExchangeCode second use: err = nil, want an error")
	}
}

func TestIssuerRefreshRotates(t *testing.T) {
	iss := newTestIssuer(t)
	c := registerPublicClient(t, iss)
	code, verifier := authorizeFor(t, iss, c)
	resp, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}

	refreshed, err := iss.RefreshToken(resp.RefreshToken, c.ID, "")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if refreshed.AccessToken == "" || refreshed.RefreshToken == "" {
		t.Fatal("RefreshToken returned an empty token")
	}
	if refreshed.RefreshToken == resp.RefreshToken {
		t.Error("refresh token was not rotated")
	}
	if refreshed.AccessToken == resp.AccessToken {
		t.Error("access token was not re-minted")
	}
	if _, err := iss.VerifyAccessToken(refreshed.AccessToken); err != nil {
		t.Fatalf("VerifyAccessToken(refreshed): %v", err)
	}
	// The presented refresh token is consumed either way: replay fails.
	if _, err := iss.RefreshToken(resp.RefreshToken, c.ID, ""); err == nil {
		t.Fatal("RefreshToken(replayed old token): err = nil, want an error")
	}
	// The rotated token works.
	if _, err := iss.RefreshToken(refreshed.RefreshToken, c.ID, ""); err != nil {
		t.Fatalf("RefreshToken(rotated token): %v", err)
	}
}

func TestIssuerRevokeAccessToken(t *testing.T) {
	iss := newTestIssuer(t)
	c := registerPublicClient(t, iss)
	code, verifier := authorizeFor(t, iss, c)
	resp, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if !iss.Revoke(resp.AccessToken) {
		t.Fatal("Revoke(access token): returned false, want true")
	}
	if _, err := iss.VerifyAccessToken(resp.AccessToken); err == nil {
		t.Fatal("VerifyAccessToken after revoke: err = nil, want an error")
	}
	if iss.Revoke("garbage-token") {
		t.Error("Revoke(garbage): returned true, want false")
	}
	// The refresh token is unaffected by revoking the access token.
	if _, err := iss.RefreshToken(resp.RefreshToken, c.ID, ""); err != nil {
		t.Fatalf("RefreshToken after access revoke: %v", err)
	}
}

func TestIssuerRevokeRefreshToken(t *testing.T) {
	iss := newTestIssuer(t)
	c := registerPublicClient(t, iss)
	code, verifier := authorizeFor(t, iss, c)
	resp, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if !iss.Revoke(resp.RefreshToken) {
		t.Fatal("Revoke(refresh token): returned false, want true")
	}
	if _, err := iss.RefreshToken(resp.RefreshToken, c.ID, ""); err == nil {
		t.Fatal("RefreshToken after revoke: err = nil, want an error")
	}
}

func TestIssuerAuthorizeValidation(t *testing.T) {
	iss := newTestIssuer(t)
	c := registerPublicClient(t, iss)
	verifier, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier: %v", err)
	}
	challenge := CodeChallengeS256(verifier)
	good := func() (string, string, []string, string, string, []string) {
		return c.ID, c.RedirectURIs[0], nil, challenge, "S256", []string{"admin"}
	}
	cases := []struct {
		name   string
		mutate func(clientID, redirect string, scopes []string, ch, method string, opScopes []string) (string, string, []string, string, string, []string)
	}{
		{"unknown client", func(clientID, redirect string, scopes []string, ch, method string, opScopes []string) (string, string, []string, string, string, []string) {
			return "oc_nope", redirect, scopes, ch, method, opScopes
		}},
		{"unregistered redirect", func(clientID, redirect string, scopes []string, ch, method string, opScopes []string) (string, string, []string, string, string, []string) {
			return clientID, "http://localhost:9999/evil", scopes, ch, method, opScopes
		}},
		{"method plain", func(clientID, redirect string, scopes []string, ch, method string, opScopes []string) (string, string, []string, string, string, []string) {
			return clientID, redirect, scopes, ch, "plain", opScopes
		}},
		{"challenge too short", func(clientID, redirect string, scopes []string, ch, method string, opScopes []string) (string, string, []string, string, string, []string) {
			return clientID, redirect, scopes, "short", method, opScopes
		}},
		{"scope exceeds client scopes", func(clientID, redirect string, scopes []string, ch, method string, opScopes []string) (string, string, []string, string, string, []string) {
			return clientID, redirect, []string{"admin"}, ch, method, opScopes
		}},
		{"unknown scope", func(clientID, redirect string, scopes []string, ch, method string, opScopes []string) (string, string, []string, string, string, []string) {
			return clientID, redirect, []string{"teleport"}, ch, method, opScopes
		}},
		{"operator scopes nil", func(clientID, redirect string, scopes []string, ch, method string, opScopes []string) (string, string, []string, string, string, []string) {
			return clientID, redirect, scopes, ch, method, nil
		}},
		{"operator cannot grant scope", func(clientID, redirect string, scopes []string, ch, method string, opScopes []string) (string, string, []string, string, string, []string) {
			return clientID, redirect, []string{"models"}, ch, method, []string{"chat"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clientID, redirect, scopes, ch, method, opScopes := good()
			clientID, redirect, scopes, ch, method, opScopes = tc.mutate(clientID, redirect, scopes, ch, method, opScopes)
			if _, err := iss.Authorize(clientID, redirect, scopes, ch, method, "key-admin", opScopes); err == nil {
				t.Fatal("Authorize: err = nil, want an error")
			}
		})
	}
}

func TestIssuerConfidentialClientSecret(t *testing.T) {
	iss := newTestIssuer(t)
	c, secret, err := iss.RegisterClient("confidential", []string{"https://app.example.com/cb"}, []string{"chat"}, false)
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	if secret == "" {
		t.Fatal("confidential client returned an empty secret")
	}
	if c.SecretHash == "" {
		t.Fatal("confidential client has an empty SecretHash")
	}
	if c.SecretHash == secret {
		t.Error("SecretHash stores the raw secret")
	}
	if got := sha256Hex(secret); got != c.SecretHash {
		t.Error("SecretHash is not the SHA-256 of the secret")
	}

	// Wrong secret: exchange fails.
	code, verifier := authorizeFor(t, iss, c)
	if _, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "wrong-secret"); err == nil {
		t.Fatal("ExchangeCode(wrong secret): err = nil, want an error")
	}
	// Correct secret: exchange succeeds.
	code, verifier = authorizeFor(t, iss, c)
	if _, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, secret); err != nil {
		t.Fatalf("ExchangeCode(correct secret): %v", err)
	}
	// Empty secret for a confidential client: fails.
	code, verifier = authorizeFor(t, iss, c)
	if _, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, ""); err == nil {
		t.Fatal("ExchangeCode(empty secret): err = nil, want an error")
	}
}

func TestIssuerRegisterClientValidation(t *testing.T) {
	iss := newTestIssuer(t)
	good := []string{"http://localhost:8080/callback"}
	cases := []struct {
		name     string
		client   string
		redirect []string
		scopes   []string
		public   bool
	}{
		{"empty name", "", good, []string{"chat"}, true},
		{"no redirect URIs", "x", nil, []string{"chat"}, true},
		{"relative redirect", "x", []string{"/callback"}, []string{"chat"}, true},
		{"non-http scheme", "x", []string{"ftp://example.com/cb"}, []string{"chat"}, true},
		{"plain http non-loopback", "x", []string{"http://example.com/cb"}, []string{"chat"}, true},
		{"unknown scope", "x", good, []string{"teleport"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := iss.RegisterClient(tc.client, tc.redirect, tc.scopes, tc.public); err == nil {
				t.Fatal("RegisterClient: err = nil, want an error")
			}
		})
	}
	// No scopes means the default scopes, never an admin grant.
	c, _, err := iss.RegisterClient("defaults", good, nil, true)
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	if len(c.Scopes) != len(DefaultScopes) {
		t.Fatalf("Scopes = %v, want %v", c.Scopes, DefaultScopes)
	}
	// Duplicate scopes are de-duplicated and sorted.
	c, _, err = iss.RegisterClient("dedup", good, []string{"models", "chat", "chat"}, true)
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	if len(c.Scopes) != 2 || c.Scopes[0] != "chat" || c.Scopes[1] != "models" {
		t.Errorf("Scopes = %v, want [chat models]", c.Scopes)
	}
}

func TestIssuerDeleteClient(t *testing.T) {
	iss := newTestIssuer(t)
	c := registerPublicClient(t, iss)
	if got := len(iss.ListClients()); got != 1 {
		t.Fatalf("ListClients = %d clients, want 1", got)
	}
	if !iss.DeleteClient(c.ID) {
		t.Fatal("DeleteClient: returned false, want true")
	}
	if got := len(iss.ListClients()); got != 0 {
		t.Fatalf("ListClients after delete = %d, want 0", got)
	}
	if iss.DeleteClient(c.ID) {
		t.Error("DeleteClient twice: returned true, want false")
	}
	verifier, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier: %v", err)
	}
	if _, err := iss.Authorize(c.ID, c.RedirectURIs[0], nil, CodeChallengeS256(verifier), "S256", "key-admin", []string{"admin"}); err == nil {
		t.Fatal("Authorize(deleted client): err = nil, want an error")
	}
	// Client views carry no secret material.
	c2, _, err := iss.RegisterClient("view-check", []string{"http://localhost:8080/callback"}, []string{"chat"}, false)
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	views := iss.ListClients()
	if len(views) != 1 || views[0].ID != c2.ID {
		t.Fatalf("ListClients = %v, want the one registered client", views)
	}
	if views[0].Name != "view-check" || views[0].Public {
		t.Errorf("ListClients view = %+v, want name view-check, public false", views[0])
	}
}

func TestIssuerPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	iss, err := NewIssuer(dir)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	c := registerPublicClient(t, iss)
	code, verifier := authorizeFor(t, iss, c)
	resp, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "oauth.json")); err != nil {
		t.Fatalf("oauth.json not written: %v", err)
	}

	// A fresh Issuer over the same directory reloads the key and
	// the client: tokens minted before the restart still verify.
	iss2, err := NewIssuer(dir)
	if err != nil {
		t.Fatalf("NewIssuer (reload): %v", err)
	}
	clients := iss2.ListClients()
	if len(clients) != 1 || clients[0].ID != c.ID {
		t.Fatalf("ListClients after reload = %v, want the registered client", clients)
	}
	if _, err := iss2.VerifyAccessToken(resp.AccessToken); err != nil {
		t.Fatalf("VerifyAccessToken after restart: %v", err)
	}
	// And the reloaded issuer can mint new tokens for the client.
	code, verifier = authorizeFor(t, iss2, c)
	if _, err := iss2.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, ""); err != nil {
		t.Fatalf("ExchangeCode after restart: %v", err)
	}
}

func TestIssuerTokenExpired(t *testing.T) {
	iss := newTestIssuer(t)
	c := registerPublicClient(t, iss)
	// A token minted with a past issue time is already expired.
	expired, err := SignJWT(iss.key, c.ID, []string{"chat"}, time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	exp, ok := iss.TokenExpired(expired)
	if !ok {
		t.Fatal("TokenExpired(expired token): ok = false, want true")
	}
	if exp.IsZero() {
		t.Error("TokenExpired returned a zero expiry")
	}
	// A fresh token is not expired.
	code, verifier := authorizeFor(t, iss, c)
	resp, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if _, ok := iss.TokenExpired(resp.AccessToken); ok {
		t.Error("TokenExpired(fresh token): ok = true, want false")
	}
	if _, ok := iss.TokenExpired("garbage-token"); ok {
		t.Error("TokenExpired(garbage): ok = true, want false")
	}
}

// TestIssuerRevokeRefreshPersistFailureFailsClosed is the regression
// for AUD-CODE-012: a refresh-token revocation that cannot be
// persisted must fail closed — Revoke returns false, the error is
// logged, and the in-memory state must NOT drop the token (otherwise
// memory would claim a revocation that a restart silently undoes).
func TestIssuerRevokeRefreshPersistFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	iss, err := NewIssuer(dir)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	c := registerPublicClient(t, iss)
	code, verifier := authorizeFor(t, iss, c)
	resp, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}

	// Capture and silence the persistence-failure log for this test.
	orig := persistLogf
	logged := false
	persistLogf = func(string, ...any) { logged = true }
	t.Cleanup(func() { persistLogf = orig })

	// Force every subsequent saveLocked to FAIL deterministically:
	// replace the state FILE with a directory, so the temp write
	// succeeds but the rename onto the target cannot.
	statePath := filepath.Join(dir, "oauth.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove state file: %v", err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatalf("replace state file with a directory: %v", err)
	}

	if ok := iss.Revoke(resp.RefreshToken); ok {
		t.Fatal("Revoke returned true although the revocation could not be persisted")
	}
	if !logged {
		t.Fatal("persist failure was not logged")
	}
	// Fail closed: memory must NOT claim a revocation a restart would undo.
	iss.mu.Lock()
	_, stillPresent := iss.refresh[sha256Hex(resp.RefreshToken)]
	iss.mu.Unlock()
	if !stillPresent {
		t.Fatal("refresh token dropped from memory although the revocation was not persisted")
	}
}

// TestIssuerRevokeRefreshTokenPersistsAcrossRestart pins the happy-path
// durability invariant around the same finding: once Revoke reports
// success, the revocation survives a restart.
func TestIssuerRevokeRefreshTokenPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	iss, err := NewIssuer(dir)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	c := registerPublicClient(t, iss)
	code, verifier := authorizeFor(t, iss, c)
	resp, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if !iss.Revoke(resp.RefreshToken) {
		t.Fatal("Revoke(refresh token): returned false, want true")
	}
	iss2, err := NewIssuer(dir)
	if err != nil {
		t.Fatalf("NewIssuer (reload): %v", err)
	}
	if _, err := iss2.RefreshToken(resp.RefreshToken, c.ID, ""); err == nil {
		t.Fatal("revoked refresh token came back to life after a restart")
	}
}
