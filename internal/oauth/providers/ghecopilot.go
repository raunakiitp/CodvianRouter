package providers

// GitHub Enterprise Copilot OAuth (catalog "ghe-copilot").
//
// Flow: the same OAuth 2.0 device authorization grant the GitHub.com
// Copilot client uses, pointed at the operator's GitHub Enterprise
// Cloud data-residency host (SUBDOMAIN.ghe.com). The device sign-in
// and token exchange go to https://SUBDOMAIN.ghe.com, and the
// Copilot API-token exchange goes to https://api.SUBDOMAIN.ghe.com —
// the host routing GitHub documents for Copilot on Enterprise Cloud
// with data residency. The public VS Code Copilot client ID is used
// unchanged; GitHub Enterprise Server is NOT supported (Copilot is
// not offered there).
//
// The operator supplies the host via CODVIAN_OAUTH_GHE_HOST (e.g.
// "octocorp.ghe.com"); leaving it unset refuses rather than falling
// back to github.com (that surface is the github-copilot handler).
//
// Sources:
//   - https://docs.litellm.ai/docs/providers/github_copilot (the
//     per-host device flow for GHE Cloud with data residency:
//     sign-in at https://SUBDOMAIN.ghe.com/login/device, user lookup
//     and token exchange at https://api.SUBDOMAIN.ghe.com, model
//     requests at https://copilot-api.SUBDOMAIN.ghe.com; GHES not
//     accepted)
//   - https://docs.github.com/en/enterprise-cloud@latest/copilot/how-tos/copilot-sdk/auth/authenticate
//     (Copilot OAuth authentication on Enterprise Cloud)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// gheHostEnv is the operator's GitHub Enterprise Cloud host
// (SUBDOMAIN.ghe.com).
const gheHostEnv = "CODVIAN_OAUTH_GHE_HOST"

type gheCopilotHandler struct {
	baseHandler
}

func gheCopilotInfo() Info {
	return Info{
		ID:          "ghe-copilot",
		Name:        "GitHub Enterprise Copilot",
		Flow:        FlowDeviceCode,
		BaseURL:     "https://api.githubcopilot.com",
		DocsURL:     "https://docs.litellm.ai/docs/providers/github_copilot",
		Reason:      "",
		Implemented: true,
	}
}

func (gheCopilotHandler) Info() Info { return gheCopilotInfo() }

// gheHost returns the operator-configured Enterprise Cloud host. The
// value is a bare hostname (no scheme, no path); anything else is
// refused so the endpoints can be composed safely.
func gheHost() (string, error) {
	host := strings.TrimSpace(strings.ToLower(os.Getenv(gheHostEnv)))
	if host == "" {
		return "", fmt.Errorf("providers: ghe-copilot: host not configured (set %s to your GitHub Enterprise Cloud data-residency host, e.g. octocorp.ghe.com — Copilot sign-in then runs on that host; github.com is served by the github-copilot provider)", gheHostEnv)
	}
	if strings.ContainsAny(host, "/\\ ") || !strings.Contains(host, ".") ||
		strings.HasPrefix(host, "http") || strings.Contains(host, ":") {
		return "", fmt.Errorf("providers: ghe-copilot: %s must be a bare hostname (got %q)", gheHostEnv, host)
	}
	return host, nil
}

// StartDevice begins the device flow on the operator's GHE host.
func (gheCopilotHandler) StartDevice(ctx context.Context, cl *http.Client) (*DeviceCode, error) {
	host, err := gheHost()
	if err != nil {
		return nil, err
	}
	deviceURL := endpoint("ghe-copilot", "device", "https://"+host+"/login/device/code")
	form := url.Values{}
	form.Set("client_id", copilotClientID)
	form.Set("scope", "read:user")
	var resp struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := postForm(ctx, cl, deviceURL, form, &resp, nil); err != nil {
		return nil, err
	}
	if resp.DeviceCode == "" || resp.UserCode == "" {
		return nil, fmt.Errorf("providers: ghe-copilot: device response missing device_code/user_code")
	}
	return &DeviceCode{
		DeviceCode:      resp.DeviceCode,
		UserCode:        resp.UserCode,
		VerificationURI: resp.VerificationURI,
		ExpiresIn:       resp.ExpiresIn,
		Interval:        resp.Interval,
		state:           map[string]string{"device_code": resp.DeviceCode},
	}, nil
}

// PollDevice polls the host's token endpoint once with the device
// grant; on approval it exchanges the GitHub token for the short-lived
// Copilot API token at api.<host>/copilot_internal/v2/token and keeps
// the GitHub token as the refresh credential (same shape as the
// github.com Copilot flow, on the residency host).
func (gheCopilotHandler) PollDevice(ctx context.Context, cl *http.Client, dc *DeviceCode) (*TokenResult, error) {
	host, err := gheHost()
	if err != nil {
		return nil, err
	}
	if dc == nil || dc.state["device_code"] == "" {
		return nil, fmt.Errorf("providers: ghe-copilot: poll called without a device-code offer")
	}
	tokenURL := endpoint("ghe-copilot", "token", "https://"+host+"/login/oauth/access_token")
	form := url.Values{}
	form.Set("client_id", copilotClientID)
	form.Set("device_code", dc.state["device_code"])
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	status, raw, err := doTokenRaw(ctx, cl, tokenURL, http.MethodPost, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		if code, _, ok := decodeOAuthError(raw); ok && (code == "authorization_pending" || code == "slow_down") {
			return nil, ErrAuthorizationPending
		}
		return nil, fmt.Errorf("providers: ghe-copilot: device poll failed: status %d: %s", status, truncateBody(raw))
	}
	var resp struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("providers: ghe-copilot: decode device poll response: %w", err)
	}
	if resp.AccessToken == "" {
		if resp.Error == "authorization_pending" || resp.Error == "slow_down" {
			return nil, ErrAuthorizationPending
		}
		if resp.Error != "" {
			return nil, fmt.Errorf("providers: ghe-copilot: device authorization failed: %s", resp.Error)
		}
		return nil, fmt.Errorf("providers: ghe-copilot: device poll response carried no access_token")
	}
	tr, err := gheCopilotExchangeToken(ctx, cl, host, resp.AccessToken)
	if err != nil {
		return nil, err
	}
	// The long-lived GitHub token is the refresh credential for every
	// later Copilot token exchange.
	tr.RefreshToken = resp.AccessToken
	return tr, nil
}

// Refresh re-exchanges the stored GitHub token for a fresh Copilot API
// token at api.<host>.
func (gheCopilotHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	host, err := gheHost()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoRefresh
	}
	tr, err := gheCopilotExchangeToken(ctx, cl, host, refreshToken)
	if err != nil {
		return nil, err
	}
	// The GitHub token itself is not rotated by the exchange.
	tr.RefreshToken = refreshToken
	return tr, nil
}

// gheCopilotExchangeToken trades a GitHub OAuth token for the
// short-lived Copilot API token at api.<host>/copilot_internal/v2/token.
func gheCopilotExchangeToken(ctx context.Context, cl *http.Client, host, githubToken string) (*TokenResult, error) {
	exchangeURL := endpoint("ghe-copilot", "exchange", "https://api."+host+"/copilot_internal/v2/token")
	hdr := map[string]string{
		"Authorization":         "token " + githubToken,
		"Accept":                "application/json",
		"Editor-Version":        "vscode/1.85.1",
		"Editor-Plugin-Version": "copilot/1.155.0",
		"User-Agent":            "GithubCopilot/1.155.0",
	}
	status, raw, err := doTokenRaw(ctx, cl, exchangeURL, http.MethodGet, "", nil, hdr)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("providers: ghe-copilot: copilot token exchange failed: status %d: %s", status, truncateBody(raw))
	}
	var resp struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("providers: ghe-copilot: decode copilot token response: %w", err)
	}
	if resp.Token == "" {
		return nil, fmt.Errorf("providers: ghe-copilot: copilot token exchange carried no token")
	}
	expiresIn := resp.ExpiresIn
	if expiresIn == 0 {
		expiresIn = copilotTokenLifetime
	}
	return &TokenResult{
		AccessToken: resp.Token,
		TokenType:   "bearer",
		Scope:       "copilot",
		ExpiresIn:   expiresIn,
		TokenURL:    exchangeURL,
	}, nil
}
