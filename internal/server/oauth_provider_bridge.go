// OAuth provider bridge (A3): connects the per-provider OAuth handler
// registry (internal/oauth/providers) to the router's real OAuth
// token machinery.
//
// What this file owns:
//
//   - Storage: tokens produced by a provider handler (authorization
//     exchange, device-flow completion, paste-token import) are
//     stored in the SAME oauthConnStore the dashboard OAuth identity
//     connections use — Vault-sealed at rest when a data dir is
//     configured, plain in-memory otherwise.
//   - Resolution + refresh: OAuthProviderAccessToken serves the
//     stored access token, unsealing via the Vault; a token expiring
//     within the store's refresh skew is REFRESHED through the
//     provider handler's documented refresh grant — issued through
//     the SAME SSRF-guarded egress client the code exchange uses
//     (oauthTokenClientLocked, AUD-SEC-005; never http.DefaultClient)
//     — and the rotated tokens are resealed. A stale credential is
//     never served: no refresh token, no refresh endpoint, or a
//     refused refresh all error honestly.
//   - Resolver wiring: InstallOAuthProviderResolver points the
//     provider registry's OAuth resolver (SetOAuthTokenResolver) at
//     OAuthProviderAccessToken, so a catalog OAuth surface's requests
//     resolve through the real store (expiry + refresh included).
//
// No stub resolvers: every path here ends in the real connection
// store and the real egress client, or in an honest error.
//
// NOTE on the dashboard identity providers: the three identity
// providers in oauth_conn.go (github, google, microsoft) keep their
// own store keys; the catalog OAuth IDs do not collide with them
// (the GitHub Copilot catalog ID is "github-copilot").
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/oauth/providers"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// storeProviderConn replaces the stored connection for a handler's
// provider ID with the tokens from an exchange or device-flow
// completion. The storage shape mirrors oauth_conn.go's CompleteAuth
// (same fields, same Vault sealing, same expiry default).
func (s *oauthConnStore) storeProviderConn(providerID string, tr *providers.TokenResult) error {
	if tr == nil || strings.TrimSpace(tr.AccessToken) == "" {
		return fmt.Errorf("oauth: handler for %q produced no access token", providerID)
	}
	accessStore := tr.AccessToken
	refreshStore := tr.RefreshToken
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vault != nil {
		sealed, err := s.vault.Seal([]byte(tr.AccessToken))
		if err != nil {
			return fmt.Errorf("oauth: encrypt access token: %w", err)
		}
		accessStore = sealed
		if tr.RefreshToken != "" {
			sealed, err = s.vault.Seal([]byte(tr.RefreshToken))
			if err != nil {
				return fmt.Errorf("oauth: encrypt refresh token: %w", err)
			}
			refreshStore = sealed
		} else {
			refreshStore = ""
		}
	}
	expiresAt := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.ExpiresIn == 0 {
		expiresAt = time.Now().Add(24 * time.Hour)
	}
	s.conns[providerID] = &oauthConn{
		Provider:     providerID,
		AccessToken:  accessStore,
		RefreshToken: refreshStore,
		TokenURL:     tr.TokenURL,
		TokenType:    tr.TokenType,
		ExpiresAt:    expiresAt,
		Scope:        tr.Scope,
		UpdatedAt:    time.Now(),
	}
	return nil
}

// storeImportedConn stores an allowlisted paste-token import. The
// connection records NO refresh endpoint (the import carries whatever
// the provider's tooling exported): an expired imported token errors
// honestly instead of guessing an endpoint.
func (s *oauthConnStore) storeImportedConn(providerID string, cred *providers.ImportedCredential) error {
	accessStore := cred.AccessToken
	refreshStore := cred.RefreshToken
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vault != nil {
		sealed, err := s.vault.Seal([]byte(cred.AccessToken))
		if err != nil {
			return fmt.Errorf("oauth: encrypt access token: %w", err)
		}
		accessStore = sealed
		if cred.RefreshToken != "" {
			sealed, err = s.vault.Seal([]byte(cred.RefreshToken))
			if err != nil {
				return fmt.Errorf("oauth: encrypt refresh token: %w", err)
			}
			refreshStore = sealed
		} else {
			refreshStore = ""
		}
	}
	expiresAt := cred.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(24 * time.Hour)
	}
	s.conns[providerID] = &oauthConn{
		Provider:     providerID,
		AccessToken:  accessStore,
		RefreshToken: refreshStore,
		TokenType:    cred.TokenType,
		ExpiresAt:    expiresAt,
		Scope:        cred.Scope,
		UpdatedAt:    time.Now(),
	}
	return nil
}

// resolveProviderToken serves the stored access token for an
// implemented handler, refreshing through the handler's documented
// refresh grant when the token is expiring or expired. Callers get the
// PLAINTEXT token (it is about to be sent upstream); the stored copy
// stays sealed.
func (s *oauthConnStore) resolveProviderToken(h providers.Handler) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := h.Info().ID
	c, ok := s.conns[id]
	if !ok {
		return "", fmt.Errorf("oauth: no connection for %q", id)
	}
	if time.Until(c.ExpiresAt) > refreshSkew {
		return s.decryptAccessToken(id, c)
	}
	// Expiring (or expired): refresh — never serve a stale credential.
	if c.RefreshToken == "" {
		return "", fmt.Errorf("oauth: token for %q expired at %s and cannot be refreshed (no refresh token stored) — reconnect the provider", id, c.ExpiresAt.Format(time.RFC3339))
	}
	if c.TokenURL == "" {
		return "", fmt.Errorf("oauth: token for %q expired at %s and cannot be refreshed (imported credential without a refresh endpoint) — reconnect the provider", id, c.ExpiresAt.Format(time.RFC3339))
	}
	refreshPlain := c.RefreshToken
	if s.vault != nil {
		plain, err := s.vault.Unseal(c.RefreshToken)
		if err != nil {
			return "", fmt.Errorf("oauth: decrypt refresh token: %w", err)
		}
		refreshPlain = string(plain)
	}
	// The refresh call goes through the SSRF-guarded egress client —
	// the LOCKED variant, since s.mu is held (same rule as
	// oauth_conn.go's refreshLocked; AUD-SEC-005).
	tr, err := h.Refresh(context.Background(), s.oauthTokenClientLocked(), refreshPlain)
	if err != nil {
		return "", fmt.Errorf("oauth: refresh token for %q: %w", id, err)
	}
	if tr == nil || strings.TrimSpace(tr.AccessToken) == "" {
		return "", fmt.Errorf("oauth: refresh for %q produced no access token", id)
	}
	accessStore := tr.AccessToken
	if s.vault != nil {
		sealed, serr := s.vault.Seal([]byte(tr.AccessToken))
		if serr != nil {
			return "", fmt.Errorf("oauth: encrypt access token: %w", serr)
		}
		accessStore = sealed
	}
	// OAuth refresh responses may omit the refresh token when the old
	// one remains valid (no rotation) — keep the stored one then.
	if tr.RefreshToken != "" {
		refreshStore := tr.RefreshToken
		if s.vault != nil {
			sealed, serr := s.vault.Seal([]byte(tr.RefreshToken))
			if serr != nil {
				return "", fmt.Errorf("oauth: encrypt refresh token: %w", serr)
			}
			refreshStore = sealed
		}
		c.RefreshToken = refreshStore
	}
	expiresAt := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.ExpiresIn == 0 {
		expiresAt = time.Now().Add(24 * time.Hour)
	}
	c.AccessToken = accessStore
	c.TokenURL = tr.TokenURL
	c.ExpiresAt = expiresAt
	c.UpdatedAt = time.Now()
	if tr.TokenType != "" {
		c.TokenType = tr.TokenType
	}
	if tr.Scope != "" {
		c.Scope = tr.Scope
	}
	return s.decryptAccessToken(id, c)
}

// providerHandler resolves the registry handler for a catalog OAuth
// provider ID (honest refusal for unimplemented providers).
func providerHandler(catalogID string) (providers.Handler, error) {
	return providers.Get(catalogID)
}

// StartProviderOAuth returns the authorize URL for a code-flow
// provider (FlowPKCE / FlowBrowserSignIn). The caller binds the
// returned state to the operator session and exchanges the callback
// code with CompleteProviderOAuth (same redirect URI + verifier).
func (s *Server) StartProviderOAuth(catalogID, redirectURI string) (state, authURL, verifier string, err error) {
	h, err := providerHandler(catalogID)
	if err != nil {
		return "", "", "", err
	}
	return h.StartAuthorization(redirectURI)
}

// CompleteProviderOAuth exchanges an authorization code for tokens and
// stores the connection (Vault-sealed when configured).
func (s *Server) CompleteProviderOAuth(catalogID, code, redirectURI, verifier string) error {
	h, err := providerHandler(catalogID)
	if err != nil {
		return err
	}
	st := s.oauthStoreFor()
	tr, err := h.ExchangeCode(context.Background(), st.oauthTokenClient(), code, redirectURI, verifier)
	if err != nil {
		return err
	}
	return st.storeProviderConn(h.Info().ID, tr)
}

// StartProviderDevice begins a device flow for a device-code provider.
func (s *Server) StartProviderDevice(catalogID string) (*providers.DeviceCode, error) {
	h, err := providerHandler(catalogID)
	if err != nil {
		return nil, err
	}
	st := s.oauthStoreFor()
	return h.StartDevice(context.Background(), st.oauthTokenClient())
}

// CompleteProviderDevice polls the device flow ONCE and stores the
// tokens on approval. ErrAuthorizationPending means "not approved yet
// — poll again after DeviceCode.Interval"; any other error is
// terminal.
func (s *Server) CompleteProviderDevice(catalogID string, dc *providers.DeviceCode) (*providers.TokenResult, error) {
	h, err := providerHandler(catalogID)
	if err != nil {
		return nil, err
	}
	st := s.oauthStoreFor()
	tr, err := h.PollDevice(context.Background(), st.oauthTokenClient(), dc)
	if err != nil {
		return nil, err
	}
	if err := st.storeProviderConn(h.Info().ID, tr); err != nil {
		return nil, err
	}
	return tr, nil
}

// ImportProviderBlob stores an allowlisted paste-token import
// (antigravity, agy only; the blob's provider must match the route).
func (s *Server) ImportProviderBlob(catalogID string, raw []byte) error {
	cred, err := providers.ImportPasteToken(catalogID, raw)
	if err != nil {
		return err
	}
	return s.oauthStoreFor().storeImportedConn(catalogID, cred)
}

// resolveImportedToken serves the stored access token for an
// import-only provider (antigravity, agy): fresh imports are served,
// expired ones error honestly — an imported blob carries no documented
// refresh endpoint, so nothing is invented.
func (s *oauthConnStore) resolveImportedToken(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.conns[id]
	if !ok {
		return "", fmt.Errorf("oauth: no connection for %q", id)
	}
	if time.Until(c.ExpiresAt) > refreshSkew {
		return s.decryptAccessToken(id, c)
	}
	return "", fmt.Errorf("oauth: token for %q expired at %s and cannot be refreshed (imported credential without a refresh endpoint) — reconnect the provider", id, c.ExpiresAt.Format(time.RFC3339))
}

// OAuthProviderAccessToken resolves the stored access token for a
// catalog OAuth provider through its handler — fresh tokens served
// from the store, expiring ones refreshed via the provider's
// documented grant through the SSRF-guarded client. Unimplemented
// providers are refused with the registry's documented reason; the
// import-only providers resolve straight from the store (their
// connection came from the allowlisted paste import); a
// never-connected provider errors like any missing connection.
func (s *Server) OAuthProviderAccessToken(catalogID string) (string, error) {
	h, err := providerHandler(catalogID)
	if err != nil {
		if errors.Is(err, providers.ErrImportOnly) {
			return s.oauthStoreFor().resolveImportedToken(catalogID)
		}
		if errors.Is(err, providers.ErrNotImplemented) {
			return "", fmt.Errorf("oauth providers: %w", err)
		}
		// Not a catalog OAuth provider at all: the dashboard identity
		// providers (github, google, microsoft) share this store under
		// their own IDs. Resolve through the plain store read (vault
		// unseal + expiry-aware refresh included) so the identity flows
		// keep working through the same resolver the catalog OAuth
		// surfaces use — one wiring, both credential kinds.
		return s.oauthStoreFor().GetAccessToken(catalogID)
	}
	return s.oauthStoreFor().resolveProviderToken(h)
}

// InstallOAuthProviderResolver wires the per-provider handler registry
// into the provider registry's OAuth resolver, so a catalog OAuth
// surface's per-request auth (provider.Registry.oauthHeadersFor) runs
// through OAuthProviderAccessToken — real store, real refresh, real
// egress guard.
func InstallOAuthProviderResolver(reg *provider.Registry, srv *Server) {
	reg.SetOAuthTokenResolver(srv.OAuthProviderAccessToken)
}
