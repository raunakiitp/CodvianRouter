package providers

// GitHub Copilot OAuth (catalog "github-copilot", alias "github").
//
// Flow: GitHub's OAuth 2.0 device authorization grant (RFC 8628) with
// the public VS Code Copilot client, followed by a second exchange:
// the long-lived GitHub OAuth token is traded at
// api.github.com/copilot_internal/v2/token for the SHORT-LIVED
// Copilot API token (~30 min) that api.githubcopilot.com actually
// accepts. Refresh = re-running the Copilot token exchange with the
// stored GitHub token (no refresh_token grant involved).
//
// Sources:
//   - https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps#device-flow
//     (device grant mechanics)
//   - https://github.com/byrash/github-copilot-api/blob/main/github-copilot-api-curl-guide.md
//     (client ID Iv1.b507a08c87ecfe98, device/token endpoints,
//     copilot_internal/v2/token exchange with editor headers,
//     endpoints.api = https://api.githubcopilot.com)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	copilotClientID       = "Iv1.b507a08c87ecfe98"
	copilotDeviceURL      = "https://github.com/login/device/code"
	copilotGitHubTokenURL = "https://github.com/login/oauth/access_token"
	copilotExchangeURL    = "https://api.github.com/copilot_internal/v2/token"
	copilotBaseURL        = "https://api.githubcopilot.com"
	// copilotTokenLifetime is the documented Copilot API-token
	// lifetime used when the exchange response omits expires_in.
	copilotTokenLifetime = 1800 // seconds (~30 min)
)

type copilotHandler struct {
	baseHandler
}

func (copilotHandler) Info() Info {
	return Info{
		ID:          "github-copilot",
		Name:        "GitHub Copilot",
		Alias:       "gh",
		Flow:        FlowDeviceCode,
		BaseURL:     copilotBaseURL,
		DocsURL:     "https://github.com/byrash/github-copilot-api/blob/main/github-copilot-api-curl-guide.md",
		Implemented: true,
	}
}

// StartDevice begins the device flow at github.com/login/device/code.
func (copilotHandler) StartDevice(ctx context.Context, cl *http.Client) (*DeviceCode, error) {
	deviceURL := endpoint("github-copilot", "device", copilotDeviceURL)
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
		return nil, fmt.Errorf("providers: github-copilot: device response missing device_code/user_code")
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

// PollDevice polls github.com/login/oauth/access_token once with the
// device grant; on success it immediately exchanges the GitHub token
// for the Copilot API token and returns that as the access token,
// keeping the GitHub token as the refresh credential.
func (copilotHandler) PollDevice(ctx context.Context, cl *http.Client, dc *DeviceCode) (*TokenResult, error) {
	if dc == nil || dc.state["device_code"] == "" {
		return nil, fmt.Errorf("providers: github-copilot: poll called without a device-code offer")
	}
	tokenURL := endpoint("github-copilot", "token", copilotGitHubTokenURL)
	form := url.Values{}
	form.Set("client_id", copilotClientID)
	form.Set("device_code", dc.state["device_code"])
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	status, raw, err := doTokenRaw(ctx, cl, tokenURL, http.MethodPost, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		// RFC 8628: pending arrives as a non-200 with error body.
		if code, _, ok := decodeOAuthError(raw); ok && (code == "authorization_pending" || code == "slow_down") {
			return nil, ErrAuthorizationPending
		}
		return nil, fmt.Errorf("providers: github-copilot: device poll failed: status %d: %s", status, truncateBody(raw))
	}
	var resp struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("providers: github-copilot: decode device poll response: %w", err)
	}
	if resp.AccessToken == "" {
		if resp.Error == "authorization_pending" || resp.Error == "slow_down" {
			return nil, ErrAuthorizationPending
		}
		if resp.Error != "" {
			return nil, fmt.Errorf("providers: github-copilot: device authorization failed: %s", resp.Error)
		}
		return nil, fmt.Errorf("providers: github-copilot: device poll response carried no access_token")
	}
	tr, err := copilotExchangeToken(ctx, cl, resp.AccessToken)
	if err != nil {
		return nil, err
	}
	// Keep the long-lived GitHub token: it is the refresh credential
	// for every later Copilot token exchange.
	tr.RefreshToken = resp.AccessToken
	return tr, nil
}

// Refresh re-exchanges the stored long-lived GitHub token for a fresh
// Copilot API token.
func (copilotHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoRefresh
	}
	tr, err := copilotExchangeToken(ctx, cl, refreshToken)
	if err != nil {
		return nil, err
	}
	// The GitHub token itself is not rotated by the exchange — keep
	// it as the refresh credential.
	tr.RefreshToken = refreshToken
	return tr, nil
}

// copilotExchangeToken trades a GitHub OAuth token for the short-lived
// Copilot API token at api.github.com/copilot_internal/v2/token.
func copilotExchangeToken(ctx context.Context, cl *http.Client, githubToken string) (*TokenResult, error) {
	exchangeURL := endpoint("github-copilot", "exchange", copilotExchangeURL)
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
		return nil, fmt.Errorf("providers: github-copilot: copilot token exchange failed: status %d: %s", status, truncateBody(raw))
	}
	var resp struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
		Endpoints struct {
			API string `json:"api"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("providers: github-copilot: decode copilot token response: %w", err)
	}
	if resp.Token == "" {
		return nil, fmt.Errorf("providers: github-copilot: copilot token exchange carried no token")
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
