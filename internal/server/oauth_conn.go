// Package server — OAuth provider connections (PR #52, Phase 4).
//
// OAuth provider connections let the dashboard operator connect external
// OAuth accounts (GitHub, Google, Microsoft) to Codvian Router.
//
// Security model:
//   - All connection endpoints require a valid dashboard session OR an
//     admin gateway key (same gate as other Tier 3 dashboard mutating
//     endpoints).
//   - OAuth tokens (access + refresh) are encrypted at rest with the
//     per-data-dir Vault (Argon2id-derived key) when a data dir is
//     configured; in-memory only otherwise.
//   - OAuth state parameter is random (32 bytes) and validated at
//     callback, preventing CSRF on the authorization flow.
//   - Tokens are never returned to the client; only connection status
//     (provider, connected, expires_at, scopes) is exposed.
package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// oauthProvider is the static configuration for one supported OAuth
// provider.
type oauthProvider struct {
	ID       string
	Name     string
	AuthURL  string
	TokenURL string
	Scopes   []string
}

// oauthProviders is the registry of supported OAuth providers (3 required
// by PR #52).
var oauthProviders = map[string]oauthProvider{
	"github": {
		ID:       "github",
		Name:     "GitHub",
		AuthURL:  "https://github.com/login/oauth/authorize",
		TokenURL: "https://github.com/login/oauth/access_token",
		Scopes:   []string{"read:user", "user:email"},
	},
	"google": {
		ID:       "google",
		Name:     "Google",
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
		Scopes:   []string{"openid", "email", "profile"},
	},
	"microsoft": {
		ID:       "microsoft",
		Name:     "Microsoft",
		AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
		TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		Scopes:   []string{"User.Read", "email"},
	},
}

// oauthConn is one connected OAuth account. AccessToken/RefreshToken hold
// either the raw token (no vault) or the Vault.Seal() ciphertext (vault).
type oauthConn struct {
	Provider     string    `json:"provider"`
	AccessToken  string    `json:"-"`
	RefreshToken string    `json:"-"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        string    `json:"scope"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// oauthConnStatus is the client-visible view of a connection (no secrets).
type oauthConnStatus struct {
	Provider  string    `json:"provider"`
	Connected bool      `json:"connected"`
	ExpiresAt time.Time `json:"expires_at"`
	Scope     string    `json:"scope"`
	UpdatedAt time.Time `json:"updated_at"`
}

// oauthConnStore holds OAuth connections in memory, encrypted at rest.
type oauthConnStore struct {
	mu    sync.Mutex
	vault *crypto.Vault // nil = in-memory only
	conns map[string]*oauthConn
}

func newOAuthConnStore(vault *crypto.Vault) *oauthConnStore {
	return &oauthConnStore{
		vault: vault,
		conns: make(map[string]*oauthConn),
	}
}

// StartAuth generates a random state and returns the provider
// authorization URL. Caller must bind state to the session for CSRF
// protection at callback time.
func (s *oauthConnStore) StartAuth(providerID, redirectURI string) (state string, authURL string, err error) {
	p, ok := oauthProviders[providerID]
	if !ok {
		return "", "", fmt.Errorf("oauth: unknown provider %q", providerID)
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("oauth: state generation: %w", err)
	}
	state = base64.RawURLEncoding.EncodeToString(b)

	q := url.Values{}
	q.Set("client_id", oauthClientID(providerID))
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(p.Scopes, " "))
	q.Set("state", state)
	if p.ID == "github" {
		q.Set("allow_signup", "false")
	}
	return state, p.AuthURL + "?" + q.Encode(), nil
}

// oauthClientID reads CODVIAN_OAUTH_<PROVIDER>_CLIENT_ID from the env.
func oauthClientID(providerID string) string {
	v, _ := os.LookupEnv("CODVIAN_OAUTH_" + strings.ToUpper(providerID) + "_CLIENT_ID")
	return v
}

// oauthClientSecret reads CODVIAN_OAUTH_<PROVIDER>_CLIENT_SECRET from the env.
func oauthClientSecret(providerID string) string {
	v, _ := os.LookupEnv("CODVIAN_OAUTH_" + strings.ToUpper(providerID) + "_CLIENT_SECRET")
	return v
}

// CompleteAuth exchanges the authorization code for tokens and stores
// the connection. Tokens are sealed with the Vault when configured.
func (s *oauthConnStore) CompleteAuth(providerID, code, redirectURI string) error {
	p, ok := oauthProviders[providerID]
	if !ok {
		return fmt.Errorf("oauth: unknown provider %q", providerID)
	}

	clientID := oauthClientID(providerID)
	clientSecret := oauthClientSecret(providerID)
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf("oauth: client credentials not configured for %q", providerID)
	}

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")

	req, err := http.NewRequest(http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("oauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: token exchange: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oauth: token exchange failed: status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("oauth: decode token response: %w", err)
	}

	accessStore := tokenResp.AccessToken
	refreshStore := tokenResp.RefreshToken
	if s.vault != nil {
		sealed, err := s.vault.Seal([]byte(tokenResp.AccessToken))
		if err != nil {
			return fmt.Errorf("oauth: encrypt access token: %w", err)
		}
		accessStore = sealed
		if tokenResp.RefreshToken != "" {
			sealed, err = s.vault.Seal([]byte(tokenResp.RefreshToken))
			if err != nil {
				return fmt.Errorf("oauth: encrypt refresh token: %w", err)
			}
			refreshStore = sealed
		}
	}

	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	if tokenResp.ExpiresIn == 0 {
		expiresAt = time.Now().Add(24 * time.Hour)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[providerID] = &oauthConn{
		Provider:     providerID,
		AccessToken:  accessStore,
		RefreshToken: refreshStore,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    expiresAt,
		Scope:        tokenResp.Scope,
		UpdatedAt:    time.Now(),
	}
	return nil
}

// ListConnections returns connection status for all providers (no tokens).
func (s *oauthConnStore) ListConnections() []oauthConnStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []oauthConnStatus
	for _, c := range s.conns {
		out = append(out, oauthConnStatus{
			Provider:  c.Provider,
			Connected: true,
			ExpiresAt: c.ExpiresAt,
			Scope:     c.Scope,
			UpdatedAt: c.UpdatedAt,
		})
	}
	return out
}

// RevokeConnection deletes the OAuth connection for a provider.
func (s *oauthConnStore) RevokeConnection(providerID string) error {
	s.mu.Lock()
	_, ok := s.conns[providerID]
	delete(s.conns, providerID)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("oauth: no connection for %q", providerID)
	}
	return nil
}

// GetAccessToken returns the access token for a provider, decrypting via
// the Vault when configured.
func (s *oauthConnStore) GetAccessToken(providerID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.conns[providerID]
	if !ok {
		return "", fmt.Errorf("oauth: no connection for %q", providerID)
	}
	if s.vault != nil {
		plain, err := s.vault.Unseal(c.AccessToken)
		if err != nil {
			return "", fmt.Errorf("oauth: decrypt access token: %w", err)
		}
		return string(plain), nil
	}
	return c.AccessToken, nil
}

// ---------- HTTP handlers ----------

func (s *Server) oauthStoreFor() *oauthConnStore {
	s.dashAuthMu.Lock()
	defer s.dashAuthMu.Unlock()
	if s.oauthStore != nil {
		return s.oauthStore
	}
	var vault *crypto.Vault
	if s.cfg != nil && s.cfg.DataDir != "" {
		v, err := crypto.New(s.cfg.DataDir)
		if err == nil {
			vault = v
		}
	}
	s.oauthStore = newOAuthConnStore(vault)
	return s.oauthStore
}

// OAuthAccessToken returns the stored OAuth access token for a
// catalog provider ID, decrypting via the vault when configured.
// It is the bridge the provider registry's OAuth resolver calls
// (provider.WithOAuthTokenResolver / SetOAuthTokenResolver) so an
// OAuth-connected provider executes with its real OAuth credential.
// Returns an error when no connection exists for the provider.
func (s *Server) OAuthAccessToken(providerID string) (string, error) {
	return s.oauthStoreFor().GetAccessToken(providerID)
}

// handleOAuthConnect serves GET /dashboard/oauth/connect/{provider}:
// redirects the operator to the provider's authorization URL.
func (s *Server) handleOAuthConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.dashSessionValid(r) && !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	providerID := r.PathValue("provider")
	if _, ok := oauthProviders[providerID]; !ok {
		writeError(w, http.StatusNotFound, "unknown oauth provider")
		return
	}
	redirectURI := r.URL.Query().Get("redirect_uri")
	if redirectURI == "" {
		redirectURI = r.URL.Query().Get("redirect") // fallback
	}
	if redirectURI == "" {
		redirectURI = "/dashboard/oauth/connections"
	}
	st := s.oauthStoreFor()
	state, authURL, err := st.StartAuth(providerID, redirectURI)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oauth: "+err.Error())
		return
	}
	// Store state in a short-lived cookie for CSRF validation at callback.
	http.SetCookie(w, &http.Cookie{
		Name:     "codvian_oauth_state_" + providerID,
		Value:    state,
		Path:     "/dashboard/oauth/callback/" + providerID,
		MaxAge:   600, // 10 minutes
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOAuthCallback serves GET /dashboard/oauth/callback/{provider}:
// exchanges the authorization code for tokens and stores the connection.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.dashSessionValid(r) && !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	providerID := r.PathValue("provider")
	if _, ok := oauthProviders[providerID]; !ok {
		writeError(w, http.StatusNotFound, "unknown oauth provider")
		return
	}

	// Validate state (CSRF protection).
	expectedCookie, err := r.Cookie("codvian_oauth_state_" + providerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "oauth: missing state cookie")
		return
	}
	stateParam := r.URL.Query().Get("state")
	if stateParam == "" || stateParam != expectedCookie.Value {
		writeError(w, http.StatusBadRequest, "oauth: state mismatch")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "oauth: missing code")
		return
	}

	redirectURI := "/dashboard/oauth/connections"
	st := s.oauthStoreFor()
	if err := st.CompleteAuth(providerID, code, redirectURI); err != nil {
		writeError(w, http.StatusInternalServerError, "oauth: "+err.Error())
		return
	}
	// Clear the state cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     "codvian_oauth_state_" + providerID,
		Path:     "/dashboard/oauth/callback/" + providerID,
		MaxAge:   -1,
		HttpOnly: true,
	})
	http.Redirect(w, r, redirectURI, http.StatusFound)
}

// handleOAuthConnections serves GET /dashboard/oauth/connections:
// lists connected OAuth accounts (no tokens).
func (s *Server) handleOAuthConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.dashSessionValid(r) && !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	st := s.oauthStoreFor()
	conns := st.ListConnections()
	writeJSON(w, http.StatusOK, map[string]any{"connections": conns})
}

// handleOAuthDisconnect serves POST /dashboard/oauth/disconnect/{provider}:
// revokes and deletes the OAuth connection.
func (s *Server) handleOAuthDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.dashSessionValid(r) && !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	providerID := r.PathValue("provider")
	if _, ok := oauthProviders[providerID]; !ok {
		writeError(w, http.StatusNotFound, "unknown oauth provider")
		return
	}
	st := s.oauthStoreFor()
	if err := st.RevokeConnection(providerID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}
