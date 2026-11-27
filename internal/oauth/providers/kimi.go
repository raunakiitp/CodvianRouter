package providers

// Kimi Code CLI OAuth (catalog "kimi-coding").
//
// Flow: OAuth 2.0 device authorization grant (RFC 8628) against
// auth.kimi.com with Kimi Code's public client — the same device flow
// the official Kimi CLI runs (`kimi login`). Token requests carry the
// X-Msh-* device-information headers the real backend requires.
//
// Source (the official CLI's own design document — endpoints, client
// ID, required headers):
//   - https://github.com/MoonshotAI/kimi-cli/blob/main/klips/klip-14-kimi-code-oauth-login.md
//     (client_id 17e5f671-d194-4dfb-9706-5516cb48c098, POST
//     /api/oauth/device_authorization, POST /api/oauth/token with
//     device_code + refresh_token grants, X-Msh-Platform: kimi_cli and
//     the X-Msh-Device-* header set)

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	kimiOAuthHost  = "https://auth.kimi.com"
	kimiClientID   = "17e5f671-d194-4dfb-9706-5516cb48c098"
	kimiDevicePath = "/api/oauth/device_authorization"
	kimiTokenPath  = "/api/oauth/token"
)

// kimiDeviceHeaders returns the X-Msh-* headers the Kimi token
// endpoints require (platform, version, device identity).
func kimiDeviceHeaders() map[string]string {
	return map[string]string{
		"X-Msh-Platform": "kimi_cli",
		"X-Msh-Version":  "1.0.0-codvian-router",
	}
}

type kimiHandler struct {
	baseHandler
}

func (kimiHandler) Info() Info {
	return Info{
		ID:          "kimi-coding",
		Name:        "Kimi Code CLI",
		Alias:       "kmc",
		Flow:        FlowDeviceCode,
		BaseURL:     "https://api.kimi.com/coding/v1",
		DocsURL:     "https://github.com/MoonshotAI/kimi-cli/blob/main/klips/klip-14-kimi-code-oauth-login.md",
		Implemented: true,
	}
}

// StartDevice begins the device authorization at auth.kimi.com.
func (kimiHandler) StartDevice(ctx context.Context, cl *http.Client) (*DeviceCode, error) {
	deviceURL := endpoint("kimi-coding", "device", kimiOAuthHost+kimiDevicePath)
	form := url.Values{}
	form.Set("client_id", kimiClientID)
	var resp struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := postForm(ctx, cl, deviceURL, form, &resp, kimiDeviceHeaders()); err != nil {
		return nil, err
	}
	if resp.DeviceCode == "" || resp.UserCode == "" {
		return nil, fmt.Errorf("providers: kimi-coding: device response missing device_code/user_code")
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
func (kimiHandler) PollDevice(ctx context.Context, cl *http.Client, dc *DeviceCode) (*TokenResult, error) {
	if dc == nil || dc.state["device_code"] == "" {
		return nil, fmt.Errorf("providers: kimi-coding: poll called without a device-code offer")
	}
	tokenURL := endpoint("kimi-coding", "token", kimiOAuthHost+kimiTokenPath)
	form := url.Values{}
	form.Set("client_id", kimiClientID)
	form.Set("device_code", dc.state["device_code"])
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	status, raw, err := doTokenRaw(ctx, cl, tokenURL, http.MethodPost, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), kimiDeviceHeaders())
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		if code, _, ok := decodeOAuthError(raw); ok && (code == "authorization_pending" || code == "slow_down" || code == "expired_token") {
			if code == "expired_token" {
				return nil, fmt.Errorf("providers: kimi-coding: device code expired — start a new device flow")
			}
			return nil, ErrAuthorizationPending
		}
		return nil, fmt.Errorf("providers: kimi-coding: device poll failed: status %d: %s", status, truncateBody(raw))
	}
	var resp oauthTokenResponse
	if err := decodeBody(raw, &resp); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: kimi-coding: device poll response carried no access_token")
	}
	return resp.result(tokenURL), nil
}

// Refresh swaps the refresh token for fresh tokens (standard
// refresh_token grant with the device headers).
func (kimiHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoRefresh
	}
	tokenURL := endpoint("kimi-coding", "token", kimiOAuthHost+kimiTokenPath)
	form := url.Values{}
	form.Set("client_id", kimiClientID)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")
	var resp oauthTokenResponse
	if err := postForm(ctx, cl, tokenURL, form, &resp, kimiDeviceHeaders()); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("providers: kimi-coding: refresh response carried no access_token")
	}
	return resp.result(tokenURL), nil
}
