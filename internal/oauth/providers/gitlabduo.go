package providers

// GitLab Duo OAuth (catalog "gitlab-duo").
//
// Flow: OAuth 2.0 authorization code + PKCE against the GitLab
// instance's documented OAuth endpoints. GitLab documents the
// authorize/token endpoints for every instance (and its own IDE/CLI
// integrations authenticate this way), but publishes NO universal Duo
// client ID — applications are per-instance registrations owned by the
// operator. So the client ID is operator-supplied via
// CODVIAN_OAUTH_GITLAB_DUO_CLIENT_ID, mirroring the dashboard OAuth
// providers' env-based client configuration; no client secret is sent
// (PKCE public client).
//
// Sources:
//   - https://docs.gitlab.com/integration/oauth_provider/ (authorize
//     and token endpoint paths for any instance, instance-wide
//     applications for the GitLab CLI / Duo IDE plugins)
//   - https://docs.gitlab.com/user/gitlab_duo_cli/set_up/ (how Duo
//     tooling authenticates against the instance)

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	gitlabAuthURL  = "https://gitlab.com/oauth/authorize"
	gitlabTokenURL = "https://gitlab.com/oauth/token"
	// gitlabClientIDEnv is the operator's GitLab application ID.
	gitlabClientIDEnv = "CODVIAN_OAUTH_GITLAB_DUO_CLIENT_ID"
)

type gitlabDuoHandler struct {
	baseHandler
}

func (gitlabDuoHandler) Info() Info {
	return Info{
		ID:          "gitlab-duo",
		Name:        "GitLab Duo",
		Alias:       "gitlab-duo",
		Flow:        FlowBrowserSignIn,
		DocsURL:     "https://docs.gitlab.com/integration/oauth_provider/",
		Reason:      "",
		Implemented: true,
	}
}

// gitlabClientID returns the operator-configured GitLab application ID.
func gitlabClientID() string {
	return strings.TrimSpace(os.Getenv(gitlabClientIDEnv))
}

// StartAuthorization builds the instance authorize URL with PKCE.
func (gitlabDuoHandler) StartAuthorization(redirectURI string) (string, string, string, error) {
	if strings.TrimSpace(redirectURI) == "" {
		return "", "", "", fmt.Errorf("providers: gitlab-duo: redirect_uri is required")
	}
	clientID := gitlabClientID()
	if clientID == "" {
		return "", "", "", fmt.Errorf("providers: gitlab-duo: client id not configured (set %s to your GitLab application's ID — GitLab registers applications per instance, see docs.gitlab.com/integration/oauth_provider)", gitlabClientIDEnv)
	}
	verifier, challenge, err := newPKCE()
	if err != nil {
		return "", "", "", err
	}
	state, err := newState()
	if err != nil {
		return "", "", "", err
	}
	authURL := endpoint("gitlab-duo", "auth", gitlabAuthURL)
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return state, authURL + "?" + q.Encode(), verifier, nil
}

// ExchangeCode swaps the authorization code for tokens at the
// instance token endpoint (form-encoded, PKCE, no client secret).
func (gitlabDuoHandler) ExchangeCode(ctx context.Context, cl *http.Client, code, redirectURI, verifier string) (*TokenResult, error) {
	clientID := gitlabClientID()
	if clientID == "" {
		return nil, fmt.Errorf("providers: gitlab-duo: client id not configured (set %s)", gitlabClientIDEnv)
	}
	tokenURL := endpoint("gitlab-duo", "token", gitlabTokenURL)
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)
	var resp oauthTokenResponse
	if err := postForm(ctx, cl, tokenURL, form, &resp, nil); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: gitlab-duo: token response carried no access_token")
	}
	return resp.result(tokenURL), nil
}

// Refresh swaps the refresh token for fresh tokens (GitLab's standard
// refresh grant; tokens expire per the instance's configured TTL).
func (gitlabDuoHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	clientID := gitlabClientID()
	if clientID == "" {
		return nil, fmt.Errorf("providers: gitlab-duo: client id not configured (set %s)", gitlabClientIDEnv)
	}
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoRefresh
	}
	tokenURL := endpoint("gitlab-duo", "token", gitlabTokenURL)
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	var resp oauthTokenResponse
	if err := postForm(ctx, cl, tokenURL, form, &resp, nil); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: gitlab-duo: refresh response carried no access_token")
	}
	return resp.result(tokenURL), nil
}
