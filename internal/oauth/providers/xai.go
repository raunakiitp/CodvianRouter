package providers

// xAI Grok OAuth (catalog "xai-oauth", "grok-cli").
//
// Flow: OAuth 2.0 device authorization grant (RFC 8628) against
// xAI's OAuth issuer auth.x.ai with the public Grok CLI client — the
// same sign-in the official Grok Build CLI runs (`grok login
// --device-auth`). The issuer's endpoints are the documented
// /oauth2/device/code and /oauth2/token paths; the standard
// refresh_token grant renews the session at the token endpoint.
//
// Sources:
//   - https://github.com/xai-org/grok-build (the official CLI's own
//     published surface: issuer https://auth.x.ai, device endpoint
//     POST {issuer}/oauth2/device/code with client_id+scope+referrer,
//     token endpoint POST {issuer}/oauth2/token, device + refresh
//     grants, OIDC scopes incl. grok-cli:access and api:access)
//   - https://github.com/router-for-me/CLIProxyAPI/blob/main/internal/auth/xai/types.go
//     (the public Grok CLI OAuth client ID b1a00492-073a-47ea-816f-4c329264a828
//     and the exact scope set)

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	xaiIssuer         = "https://auth.x.ai"
	xaiClientID       = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiScope          = "openid profile email offline_access grok-cli:access api:access"
	xaiDevicePath     = "/oauth2/device/code"
	xaiTokenPath      = "/oauth2/token"
	xaiDeviceReferrer = "codvian-router"
)

type xaiDeviceHandler struct {
	baseHandler
	info Info
}

func xaiOAuthInfo() Info {
	return Info{
		ID:          "xai-oauth",
		Name:        "xAI OAuth (Grok)",
		Alias:       "xao",
		Flow:        FlowDeviceCode,
		BaseURL:     "https://api.x.ai/v1",
		DocsURL:     "https://github.com/xai-org/grok-build",
		Implemented: true,
	}
}

func grokCLIInfo() Info {
	return Info{
		ID:      "grok-cli",
		Name:    "Grok Build",
		Alias:   "gc",
		Flow:    FlowDeviceCode,
		BaseURL: "https://cli-chat-proxy.grok.com/v1",
		// The official CLI's published surface: endpoints, grants and
		// scopes in the login crate; device mode documented as
		// `grok login --device-auth`.
		DocsURL:     "https://github.com/xai-org/grok-build",
		Implemented: true,
	}
}

func (h xaiDeviceHandler) Info() Info { return h.info }

// StartDevice begins the device authorization at auth.x.ai.
func (h xaiDeviceHandler) StartDevice(ctx context.Context, cl *http.Client) (*DeviceCode, error) {
	deviceURL := endpoint(h.info.ID, "device", xaiIssuer+xaiDevicePath)
	form := url.Values{}
	form.Set("client_id", xaiClientID)
	form.Set("scope", xaiScope)
	form.Set("referrer", xaiDeviceReferrer)
	var resp struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := postForm(ctx, cl, deviceURL, form, &resp, nil); err != nil {
		return nil, err
	}
	if resp.DeviceCode == "" || resp.UserCode == "" {
		return nil, fmt.Errorf("providers: %s: device response missing device_code/user_code", h.info.ID)
	}
	return &DeviceCode{
		DeviceCode:              resp.DeviceCode,
		UserCode:                resp.UserCode,
		VerificationURI:         resp.VerificationURI,
		VerificationURIComplete: resp.VerificationURIComplete,
		ExpiresIn:               resp.ExpiresIn,
		Interval:                resp.Interval,
		state:                   map[string]string{"device_code": resp.DeviceCode},
	}, nil
}

// PollDevice polls the token endpoint once with the device grant.
func (h xaiDeviceHandler) PollDevice(ctx context.Context, cl *http.Client, dc *DeviceCode) (*TokenResult, error) {
	if dc == nil || dc.state["device_code"] == "" {
		return nil, fmt.Errorf("providers: %s: poll called without a device-code offer", h.info.ID)
	}
	tokenURL := endpoint(h.info.ID, "token", xaiIssuer+xaiTokenPath)
	form := url.Values{}
	form.Set("client_id", xaiClientID)
	form.Set("device_code", dc.state["device_code"])
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	status, raw, err := doTokenRaw(ctx, cl, tokenURL, http.MethodPost, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		if code, _, ok := decodeOAuthError(raw); ok && (code == "authorization_pending" || code == "slow_down" || code == "expired_token") {
			if code == "expired_token" {
				return nil, fmt.Errorf("providers: %s: device code expired — start a new device flow", h.info.ID)
			}
			return nil, ErrAuthorizationPending
		}
		return nil, fmt.Errorf("providers: %s: device poll failed: status %d: %s", h.info.ID, status, truncateBody(raw))
	}
	var resp oauthTokenResponse
	if err := decodeBody(raw, &resp); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: %s: device poll response carried no access_token", h.info.ID)
	}
	return resp.result(tokenURL), nil
}

// Refresh swaps the refresh token for fresh tokens (standard
// refresh_token grant at the issuer's token endpoint).
func (h xaiDeviceHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoRefresh
	}
	tokenURL := endpoint(h.info.ID, "token", xaiIssuer+xaiTokenPath)
	form := url.Values{}
	form.Set("client_id", xaiClientID)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")
	var resp oauthTokenResponse
	if err := postForm(ctx, cl, tokenURL, form, &resp, nil); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: %s: refresh response carried no access_token", h.info.ID)
	}
	return resp.result(tokenURL), nil
}
