package providers

// OpenAI Codex / ChatGPT OAuth (catalog "codex", "chatgpt").
//
// Flow: OAuth 2.1 authorization code + PKCE (S256) against
// auth.openai.com with the Codex CLI's public client — the same
// ChatGPT-account sign-in the official Codex CLI runs (`codex login`).
// The access token is a JWT whose chatgpt_account_id claim selects the
// account; refresh uses the standard refresh_token grant at the same
// token endpoint and may rotate the refresh token.
//
// Sources:
//   - https://github.com/openai/codex (the official CLI, `codex login`:
//     client app_EMoamEEZ73f0CkXaXp7hrann, authorize/token endpoints,
//     PKCE S256, scope openid profile email offline_access)
//   - https://github.com/AgentiLoop/Agent/blob/main/docs/CODEX_OAUTH_RESEARCH.md
//     (authorize parameters incl. codex_cli_simplified_flow, exchange
//     body, refresh grant)

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	openaiClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
	openaiAuthURL    = "https://auth.openai.com/oauth/authorize"
	openaiTokenURL   = "https://auth.openai.com/oauth/token"
	openaiScopes     = "openid profile email offline_access"
	codexRedirectURI = "http://localhost:1455/auth/callback"
)

type openaiPKCEHandler struct {
	baseHandler
	info Info
}

func codexInfo() Info {
	return Info{
		ID:          "codex",
		Name:        "OpenAI Codex",
		Alias:       "cx",
		Flow:        FlowPKCE,
		DocsURL:     "https://github.com/openai/codex",
		Implemented: true,
	}
}

func chatgptInfo() Info {
	return Info{
		ID:      "chatgpt",
		Name:    "ChatGPT",
		Alias:   "chatgpt",
		Flow:    FlowPKCE,
		DocsURL: "https://github.com/openai/codex",
		// The ChatGPT catalog entry is the SAME ChatGPT-account
		// sign-in (Codex CLI client) pointed at the ChatGPT
		// backend instead of the API — one flow, two routing
		// targets.
		BaseURL:     "https://chatgpt.com/backend-api",
		Implemented: true,
	}
}

func (h openaiPKCEHandler) Info() Info { return h.info }

// StartAuthorization builds the auth.openai.com authorize URL with
// PKCE S256 and the Codex CLI's simplified-flow parameters.
func (h openaiPKCEHandler) StartAuthorization(redirectURI string) (string, string, string, error) {
	if strings.TrimSpace(redirectURI) == "" {
		redirectURI = codexRedirectURI
	}
	verifier, challenge, err := newPKCE()
	if err != nil {
		return "", "", "", err
	}
	state, err := newState()
	if err != nil {
		return "", "", "", err
	}
	authURL := endpoint(h.info.ID, "auth", openaiAuthURL)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", openaiClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", openaiScopes)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	return state, authURL + "?" + q.Encode(), verifier, nil
}

// ExchangeCode swaps the authorization code for tokens (form-encoded,
// RFC 6749/7636).
func (h openaiPKCEHandler) ExchangeCode(ctx context.Context, cl *http.Client, code, redirectURI, verifier string) (*TokenResult, error) {
	if strings.TrimSpace(redirectURI) == "" {
		redirectURI = codexRedirectURI
	}
	tokenURL := endpoint(h.info.ID, "token", openaiTokenURL)
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", openaiClientID)
	form.Set("code_verifier", verifier)
	var resp oauthTokenResponse
	if err := postForm(ctx, cl, tokenURL, form, &resp, nil); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: %s: token response carried no access_token", h.info.ID)
	}
	return resp.result(tokenURL), nil
}

// Refresh swaps the refresh token for fresh tokens; the grant may
// rotate the refresh token, which the store persists when present.
func (h openaiPKCEHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoRefresh
	}
	tokenURL := endpoint(h.info.ID, "token", openaiTokenURL)
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", openaiClientID)
	var resp oauthTokenResponse
	if err := postForm(ctx, cl, tokenURL, form, &resp, nil); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: %s: refresh response carried no access_token", h.info.ID)
	}
	return resp.result(tokenURL), nil
}
