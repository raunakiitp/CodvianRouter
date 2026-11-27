package providers

// Cline / ClinePass OAuth (catalog "cline", "clinepass").
//
// Flow: WorkOS device authorization (AuthKit CLI auth) with Cline's
// production WorkOS client, then a registration step that trades the
// WorkOS token pair for Cline's own account token pair at
// api.cline.bot. The access token is a short-lived (~1 hour) JWT
// prefixed `workos:`; refreshes ROTATE the refresh token and the
// rotated pair must be persisted.
//
// Sources:
//   - https://workos.com/docs/authkit/cli-auth (device authorization,
//     poll and error semantics)
//   - https://pi.dev/packages/pi-clinepass (token refresh endpoint and
//     response shape: POST /api/v1/auth/refresh, { data: { accessToken,
//     refreshToken, expiresAt } }, `workos:` prefix)
//   - https://docs.cline.bot/getting-started/authorizing-with-cline
//     (Cline + ClinePass sign-in via the same account auth)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	clineWorkOSClientID  = "client_01K3A541FN8TA3EPPHTD2325AR"
	clineWorkOSDeviceURL = "https://api.workos.com/user_management/authorize/device"
	clineWorkOSAuthURL   = "https://api.workos.com/user_management/authenticate"
	clineAPIBase         = "https://api.cline.bot"
	clineRegisterPath    = "/api/v1/auth/register"
	clineRefreshPath     = "/api/v1/auth/refresh"
)

// clineTokenPair is Cline's own token response shape
// ({"success":true,"data":{"accessToken":...,"refreshToken":...,"expiresAt":...}}).
type clineTokenPair struct {
	Success bool `json:"success"`
	Data    struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    string `json:"expiresAt"`
	} `json:"data"`
}

func (p clineTokenPair) result(refreshURL string) (*TokenResult, error) {
	if !p.Success || p.Data.AccessToken == "" || p.Data.RefreshToken == "" {
		return nil, fmt.Errorf("providers: cline: token response missing accessToken/refreshToken")
	}
	tr := &TokenResult{
		AccessToken:  p.Data.AccessToken,
		RefreshToken: p.Data.RefreshToken,
		TokenType:    "bearer",
		TokenURL:     refreshURL,
	}
	if p.Data.ExpiresAt != "" {
		if ts, err := time.Parse(time.RFC3339, p.Data.ExpiresAt); err == nil {
			tr.ExpiresIn = max(1, int(time.Until(ts).Seconds()))
		}
	}
	return tr, nil
}

type clineHandler struct {
	baseHandler
	info Info
}

func clineInfo() Info {
	return Info{
		ID:          "cline",
		Name:        "Cline",
		Alias:       "cl",
		Flow:        FlowDeviceCode,
		BaseURL:     clineAPIBase + "/api/v1",
		DocsURL:     "https://docs.cline.bot/getting-started/authorizing-with-cline",
		Implemented: true,
	}
}

func clinePassInfo() Info {
	return Info{
		ID:          "clinepass",
		Name:        "ClinePass",
		Alias:       "cp",
		Flow:        FlowDeviceCode,
		BaseURL:     clineAPIBase + "/api/v1",
		DocsURL:     "https://docs.cline.bot/getting-started/authorizing-with-cline",
		Implemented: true,
	}
}

func (h clineHandler) Info() Info { return h.info }

// StartDevice begins the WorkOS device authorization for Cline.
func (h clineHandler) StartDevice(ctx context.Context, cl *http.Client) (*DeviceCode, error) {
	deviceURL := endpoint(h.info.ID, "device", clineWorkOSDeviceURL)
	form := url.Values{}
	form.Set("client_id", clineWorkOSClientID)
	var resp struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	if err := postForm(ctx, cl, deviceURL, form, &resp, nil); err != nil {
		return nil, err
	}
	if resp.DeviceCode == "" || resp.UserCode == "" {
		if resp.Error != "" {
			return nil, fmt.Errorf("providers: %s: device authorization failed: %s (%s)", h.info.ID, resp.Error, resp.ErrorDescription)
		}
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

// PollDevice polls WorkOS once with the device grant; on approval it
// registers the WorkOS token pair with Cline and returns Cline's own
// token pair.
func (h clineHandler) PollDevice(ctx context.Context, cl *http.Client, dc *DeviceCode) (*TokenResult, error) {
	if dc == nil || dc.state["device_code"] == "" {
		return nil, fmt.Errorf("providers: %s: poll called without a device-code offer", h.info.ID)
	}
	authURL := endpoint(h.info.ID, "poll", clineWorkOSAuthURL)
	form := url.Values{}
	form.Set("client_id", clineWorkOSClientID)
	form.Set("device_code", dc.state["device_code"])
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	status, raw, err := doTokenRaw(ctx, cl, authURL, http.MethodPost, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		if code, _, ok := decodeOAuthError(raw); ok && (code == "authorization_pending" || code == "slow_down") {
			return nil, ErrAuthorizationPending
		}
		return nil, fmt.Errorf("providers: %s: device poll failed: status %d: %s", h.info.ID, status, truncateBody(raw))
	}
	var resp oauthTokenResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("providers: %s: decode device poll response: %w", h.info.ID, err)
	}
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		return nil, fmt.Errorf("providers: %s: device poll response missing access_token/refresh_token", h.info.ID)
	}
	return h.register(ctx, cl, resp.AccessToken, resp.RefreshToken)
}

// Refresh swaps the stored Cline refresh token for a fresh (rotated)
// pair at Cline's refresh endpoint.
func (h clineHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoRefresh
	}
	refreshURL := endpoint(h.info.ID, "refresh", clineAPIBase+clineRefreshPath)
	// Cline's refresh endpoint takes lowercase JSON fields without
	// underscores ({"granttype","refreshtoken"}) — verified live by
	// the ClinePass integration write-ups.
	body := map[string]string{"granttype": "refresh_token", "refreshtoken": refreshToken}
	var pair clineTokenPair
	if err := postJSON(ctx, cl, refreshURL, body, &pair, nil); err != nil {
		return nil, err
	}
	return pair.result(refreshURL)
}

// register trades the WorkOS token pair for Cline's own token pair.
func (h clineHandler) register(ctx context.Context, cl *http.Client, workosAccess, workosRefresh string) (*TokenResult, error) {
	registerURL := endpoint(h.info.ID, "register", clineAPIBase+clineRegisterPath)
	body := map[string]string{"accessToken": workosAccess, "refreshToken": workosRefresh}
	var pair clineTokenPair
	if err := postJSON(ctx, cl, registerURL, body, &pair, nil); err != nil {
		return nil, err
	}
	return pair.result(endpoint(h.info.ID, "refresh", clineAPIBase+clineRefreshPath))
}
