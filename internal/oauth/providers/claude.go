package providers

// Claude Code OAuth (catalog "claude").
//
// Flow: OAuth 2.0 authorization code + PKCE (S256), public client —
// no client secret. The access token starts with sk-ant-oat and is the
// credential the Anthropic API accepts on the subscription surface
// (with the catalog's anthropic-beta: oauth-2025-04-20 header). The
// refresh grant ROTATES the refresh token: the old one stops working,
// so the store must persist the rotated pair.
//
// Sources (endpoints verified against these):
//   - https://github.com/nesyeklaf/opencode-claude-subscription/blob/main/docs/anthropic-subscription-auth.md
//     (client ID, authorize/token endpoints, JSON exchange body,
//     rotating refresh, observed 2026-09-28)
//   - https://gist.github.com/cedws/3a24b2c7569bb610e24aa90dd217d9f2
//     (authorisation request parameters, code=true paste flow)
//   - https://code.claude.com/docs/en/authentication (setup-token
//     long-lived alternative — not needed by this handler)

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	claudeClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeScopes    = "org:create_api_key user:profile user:inference"
	claudeTokenPath = "https://platform.claude.com/v1/oauth/token"
)

type claudeHandler struct {
	baseHandler
}

func (claudeHandler) Info() Info {
	return Info{
		ID:          "claude",
		Name:        "Claude Code",
		Alias:       "cc",
		Flow:        FlowPKCE,
		BaseURL:     "https://api.anthropic.com",
		DocsURL:     "https://github.com/nesyeklaf/opencode-claude-subscription/blob/main/docs/anthropic-subscription-auth.md",
		Implemented: true,
	}
}

// StartAuthorization builds the claude.ai authorize URL: PKCE S256,
// code=true (paste-friendly flow), random state.
func (claudeHandler) StartAuthorization(redirectURI string) (string, string, string, error) {
	if strings.TrimSpace(redirectURI) == "" {
		return "", "", "", fmt.Errorf("providers: claude: redirect_uri is required")
	}
	verifier, challenge, err := newPKCE()
	if err != nil {
		return "", "", "", err
	}
	state, err := newState()
	if err != nil {
		return "", "", "", err
	}
	authURL := endpoint("claude", "auth", "https://claude.ai/oauth/authorize")
	q := url.Values{}
	q.Set("client_id", claudeClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("code", "true")
	q.Set("scope", claudeScopes)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return state, authURL + "?" + q.Encode(), verifier, nil
}

// ExchangeCode swaps the authorization code for tokens at
// platform.claude.com/v1/oauth/token with a JSON body (Claude's OAuth
// token endpoint takes JSON, not form encoding).
func (claudeHandler) ExchangeCode(ctx context.Context, cl *http.Client, code, redirectURI, verifier string) (*TokenResult, error) {
	tokenURL := endpoint("claude", "token", claudeTokenPath)
	body := map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirectURI,
		"client_id":     claudeClientID,
		"code_verifier": verifier,
	}
	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := postJSON(ctx, cl, tokenURL, body, &resp, nil); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: claude: token response carried no access_token")
	}
	return &TokenResult{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		TokenType:    resp.TokenType,
		Scope:        resp.Scope,
		ExpiresIn:    resp.ExpiresIn,
		TokenURL:     tokenURL,
	}, nil
}

// Refresh swaps the refresh token for a fresh (rotated) pair. The
// response's refresh token REPLACES the stored one — keeping the old
// one would brick the connection.
func (claudeHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoRefresh
	}
	tokenURL := endpoint("claude", "token", claudeTokenPath)
	body := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     claudeClientID,
		"scope":         claudeScopes,
	}
	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := postJSON(ctx, cl, tokenURL, body, &resp, nil); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: claude: refresh response carried no access_token")
	}
	if resp.RefreshToken == "" {
		// Rotation is documented behavior: a refresh that returns
		// no refresh token cannot be honored safely.
		return nil, fmt.Errorf("providers: claude: refresh response did not rotate the refresh token")
	}
	return &TokenResult{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		TokenType:    resp.TokenType,
		Scope:        resp.Scope,
		ExpiresIn:    resp.ExpiresIn,
		TokenURL:     tokenURL,
	}, nil
}
