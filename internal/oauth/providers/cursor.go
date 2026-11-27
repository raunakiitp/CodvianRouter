package providers

// Cursor OAuth (catalog "cursor").
//
// Flow: OAuth 2.0 authorization code + PKCE (S256) against
// cursor.com — the flow Cursor itself documents for OAuth
// applications. Applications are registered per-team in the Cursor
// dashboard ("OAuth Applications" page; client IDs start with
// `oauth_client_`) and Cursor publishes no universal client
// credentials, so the client ID and secret are operator-supplied via
// CODVIAN_OAUTH_CURSOR_CLIENT_ID / CODVIAN_OAUTH_CURSOR_CLIENT_SECRET
// and sent as HTTP Basic auth on the token endpoint (the documented
// client authentication; body credentials are the alternative — never
// both).
//
// The refresh grant ROTATES the refresh token: every refresh returns
// a new refresh token and retires the old one (invalid_grant on
// reuse), so the store must persist the rotated pair.
//
// Source: https://cursor.com/docs/security/oauth-applications
// (authorize endpoint, required parameters incl. S256 PKCE + state,
// token endpoint https://api2.cursor.sh/oauth/applications/token,
// Basic client auth, rotating refresh, 1-hour access tokens, 2026-11
// retrieval).

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	cursorAuthorizeURL = "https://cursor.com/oauth/authorize"
	cursorTokenURL     = "https://api2.cursor.sh/oauth/applications/token"
	cursorBaseURL      = "https://api.cursor.com"
	cursorScope        = "agent:*"
	// cursorClientIDEnv / cursorClientSecretEnv hold the operator's
	// dashboard-registered OAuth application credentials.
	cursorClientIDEnv     = "CODVIAN_OAUTH_CURSOR_CLIENT_ID"
	cursorClientSecretEnv = "CODVIAN_OAUTH_CURSOR_CLIENT_SECRET"
)

type cursorHandler struct {
	baseHandler
}

func cursorInfo() Info {
	return Info{
		ID:          "cursor",
		Name:        "Cursor IDE",
		Alias:       "cu",
		Flow:        FlowBrowserSignIn,
		BaseURL:     cursorBaseURL,
		DocsURL:     "https://cursor.com/docs/security/oauth-applications",
		Reason:      "",
		Implemented: true,
	}
}

func (cursorHandler) Info() Info { return cursorInfo() }

func cursorCredentials() (id, secret string, err error) {
	id = strings.TrimSpace(os.Getenv(cursorClientIDEnv))
	secret = strings.TrimSpace(os.Getenv(cursorClientSecretEnv))
	if id == "" || secret == "" {
		return "", "", fmt.Errorf("providers: cursor: client credentials not configured (set %s and %s to the OAuth application's ID and secret registered on the Cursor dashboard, see cursor.com/docs/security/oauth-applications)", cursorClientIDEnv, cursorClientSecretEnv)
	}
	return id, secret, nil
}

// cursorBasicAuth is the token endpoint's HTTP Basic client
// authentication (client_id:client_secret).
func cursorBasicAuth(id, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(id+":"+secret))
}

// StartAuthorization builds the cursor.com authorize URL: PKCE S256,
// random state, operator's registered redirect URI.
func (cursorHandler) StartAuthorization(redirectURI string) (string, string, string, error) {
	if strings.TrimSpace(redirectURI) == "" {
		return "", "", "", fmt.Errorf("providers: cursor: redirect_uri is required")
	}
	id, _, err := cursorCredentials()
	if err != nil {
		return "", "", "", err
	}
	verifier, challenge, err := newPKCE()
	if err != nil {
		return "", "", "", err
	}
	state, err := newState()
	if err != nil {
		return "", "", "", err
	}
	authURL := endpoint("cursor", "auth", cursorAuthorizeURL)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", id)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", cursorScope)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return state, authURL + "?" + q.Encode(), verifier, nil
}

// ExchangeCode swaps the authorization code for tokens at
// api2.cursor.sh (form-encoded, PKCE, Basic client auth).
func (cursorHandler) ExchangeCode(ctx context.Context, cl *http.Client, code, redirectURI, verifier string) (*TokenResult, error) {
	id, secret, err := cursorCredentials()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("providers: cursor: code is required")
	}
	tokenURL := endpoint("cursor", "token", cursorTokenURL)
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", redirectURI)
	hdr := map[string]string{"Authorization": cursorBasicAuth(id, secret)}
	var resp oauthTokenResponse
	if err := postForm(ctx, cl, tokenURL, form, &resp, hdr); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: cursor: token response carried no access_token")
	}
	return resp.result(tokenURL), nil
}

// Refresh swaps the refresh token for a fresh (rotated) pair. Cursor
// retires the presented refresh token and issues a new one on every
// refresh — the store must persist the new pair.
func (cursorHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoRefresh
	}
	id, secret, err := cursorCredentials()
	if err != nil {
		return nil, err
	}
	tokenURL := endpoint("cursor", "token", cursorTokenURL)
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	hdr := map[string]string{"Authorization": cursorBasicAuth(id, secret)}
	var resp oauthTokenResponse
	if err := postForm(ctx, cl, tokenURL, form, &resp, hdr); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: cursor: refresh response carried no access_token")
	}
	return resp.result(tokenURL), nil
}
