package server

// Production wiring proof (A3 W1): the provider registry's OAuth
// resolver must be the HANDLER-BASED bridge (InstallOAuthProviderResolver →
// OAuthProviderAccessToken), not the plain store read the A2 wiring used.
//
// These tests prove three things:
//
//  1. before the install, no resolver is wired — an unwired registry
//     refuses OAuth resolution honestly ("no OAuth token resolver is
//     wired"), so removing the wiring breaks resolution loudly;
//  2. after the install, the wired resolver serves a catalog OAuth
//     provider's REAL stored token (fresh and refreshed) through the
//     per-provider handler chain — store, expiry, refresh;
//  3. the dashboard identity providers (github, google, microsoft)
//     keep resolving through the same wiring: their store IDs are not
//     catalog OAuth provider IDs, so the bridge falls back to the
//     plain store read instead of refusing them.

import (
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/oauth/providers"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func newWiringTestRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	reg, err := provider.NewFileRegistry(t.TempDir() + "/providers.json")
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	return reg
}

// TestOAuthProviderResolverWired is the wiring proof: registry resolves
// through the handler bridge after InstallOAuthProviderResolver, and
// refuses loudly when the wiring is absent.
func TestOAuthProviderResolverWired(t *testing.T) {
	srv, st := newBridgeServer(t)
	reg := newWiringTestRegistry(t)

	// Before the install: no resolver — the registry would refuse every
	// OAuth surface with the honest "no resolver wired" error. This is
	// exactly the state the wiring removal leaves behind.
	if reg.OAuthTokenResolver() != nil {
		t.Fatalf("fresh registry already has an OAuth resolver wired")
	}

	InstallOAuthProviderResolver(reg, srv)
	fn := reg.OAuthTokenResolver()
	if fn == nil {
		t.Fatalf("InstallOAuthProviderResolver did not wire the registry's OAuth resolver")
	}

	// Catalog OAuth provider: connect through the real bridge (mock
	// token endpoint), then resolve through the WIRED resolver.
	m := &tokenMock{}
	mock := m.mockServer(t, "claude")
	defer mock.Close()
	providers.SetEndpointsForTest(t, "claude", map[string]string{"token": mock.URL + "/token"})

	if _, _, verifier, err := srv.StartProviderOAuth("claude", "http://127.0.0.1:3000/callback"); err != nil {
		t.Fatalf("StartProviderOAuth: %v", err)
	} else if err := srv.CompleteProviderOAuth("claude", "code-claude", "http://127.0.0.1:3000/callback", verifier); err != nil {
		t.Fatalf("CompleteProviderOAuth: %v", err)
	}

	tok, err := fn("claude")
	if err != nil {
		t.Fatalf("wired resolver (claude): %v", err)
	}
	if tok != "claude-at-1" {
		t.Errorf("wired resolver token = %q, want %q (the handler-stored one)", tok, "claude-at-1")
	}

	// Expire the stored token: the next resolve REFRESHES through the
	// handler's documented refresh grant — the plain A2 store read
	// cannot do this for a handler provider.
	expireBridgeConn(t, st, "claude")
	tok2, err := fn("claude")
	if err != nil {
		t.Fatalf("wired resolver after expiry: %v", err)
	}
	if tok2 != "claude-at-2" {
		t.Errorf("refreshed token = %q, want %q", tok2, "claude-at-2")
	}
}

// TestOAuthProviderResolverIdentityFallback: the three dashboard
// identity providers keep resolving through the installed resolver —
// the bridge falls back to the plain store read for IDs that are not
// catalog OAuth providers.
func TestOAuthProviderResolverIdentityFallback(t *testing.T) {
	srv, st := newBridgeServer(t)
	reg := newWiringTestRegistry(t)
	InstallOAuthProviderResolver(reg, srv)
	fn := reg.OAuthTokenResolver()
	if fn == nil {
		t.Fatalf("resolver not wired")
	}

	for _, id := range []string{"github", "google", "microsoft"} {
		if _, err := fn(id); err == nil {
			t.Errorf("resolver for %q: expected honest error with no connection stored, got nil", id)
		} else if !strings.Contains(err.Error(), "no connection") {
			t.Errorf("resolver for %q: error %q does not name the missing connection", id, err.Error())
		}
	}

	// Store an identity-provider connection the way oauth_conn.go's
	// CompleteAuth does, then resolve: the fallback serves it.
	st.mu.Lock()
	st.conns["github"] = &oauthConn{
		Provider:    "github",
		AccessToken: "gh-plain-token",
		ExpiresAt:   time.Now().Add(time.Hour),
		UpdatedAt:   time.Now(),
	}
	st.mu.Unlock()

	tok, err := fn("github")
	if err != nil {
		t.Fatalf("identity fallback (github): %v", err)
	}
	if tok != "gh-plain-token" {
		t.Errorf("identity fallback token = %q, want gh-plain-token", tok)
	}
}

// TestOAuthProviderResolverRefusesUnimplemented: an honestly-
// unimplemented catalog OAuth provider is refused with its documented
// reason — the wiring never falls back to the plain store for a
// provider the registry refuses (a stale key must not sneak through).
func TestOAuthProviderResolverRefusesUnimplemented(t *testing.T) {
	srv, st := newBridgeServer(t)
	reg := newWiringTestRegistry(t)
	InstallOAuthProviderResolver(reg, srv)
	fn := reg.OAuthTokenResolver()

	// Seed a plain store connection under the catalog ID anyway: the
	// refusal must hold even when a connection exists, because the
	// connection could only have come from an unimplemented path.
	st.mu.Lock()
	st.conns["cursor"] = &oauthConn{
		Provider:    "cursor",
		AccessToken: "sneaky",
		ExpiresAt:   time.Now().Add(time.Hour),
		UpdatedAt:   time.Now(),
	}
	st.mu.Unlock()

	_, err := fn("cursor")
	if err == nil {
		t.Fatalf("unimplemented provider resolved despite the registry refusal")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error %q does not name the honest refusal", err.Error())
	}
}
