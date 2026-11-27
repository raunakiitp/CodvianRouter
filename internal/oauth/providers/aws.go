package providers

// Kiro / Amazon Q OAuth (catalog "kiro", "amazon-q").
//
// Flow: AWS SSO OIDC device authorization — the AWS-documented, fully
// public API used by both the Kiro IDE and the Amazon Q Developer CLI
// for Builder ID sign-in:
//
//	POST /client/register        (public client, CodeWhisperer scopes)
//	POST /device_authorization   (start URL https://view.awsapps.com/start)
//	POST /token                  (device_code grant; refresh_token grant)
//
// The returned access token is the Builder ID credential the Q / Kiro
// runtimes accept; refreshes use CreateToken's refresh_token grant and
// rotate the refresh token.
//
// Sources:
//   - https://docs.aws.amazon.com/singlesignon/latest/OIDCAPIReference/API_CreateToken.html
//     (+ RegisterClient / StartDeviceAuthorization — the API contract)
//   - https://github.com/mikeyobrien/pi-provider-kiro/blob/main/src/oauth.ts
//     (the concrete endpoint values, scopes and Builder ID start URL
//     used by Kiro's own flow)
//   - Amazon Q Developer CLI `q login` Builder ID device flow
//     (https://docs.aws.amazon.com/amazonq/latest/qdeveloper-ug/q-dev-login.html)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

const (
	awsOIDCEndpoint   = "https://oidc.us-east-1.amazonaws.com"
	awsBuilderIDURL   = "https://view.awsapps.com/start"
	awsClientName     = "codvian-router"
	awsDeviceTokenURL = awsOIDCEndpoint + "/token"
)

// awsScopes is the CodeWhisperer scope set both Kiro and Amazon Q
// register their public client with (the Builder ID lineage of the
// CodeWhisperer service).
var awsScopes = []string{
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:conversations",
}

type awsDeviceHandler struct {
	baseHandler
	info Info
}

func kiroInfo() Info {
	return Info{
		ID:          "kiro",
		Name:        "Kiro AI",
		Alias:       "kr",
		Flow:        FlowDeviceCode,
		BaseURL:     "https://runtime.us-east-1.kiro.dev",
		DocsURL:     "https://docs.aws.amazon.com/singlesignon/latest/OIDCAPIReference/API_CreateToken.html",
		Implemented: true,
	}
}

func amazonQInfo() Info {
	return Info{
		ID:          "amazon-q",
		Name:        "Amazon Q",
		Alias:       "aq",
		Flow:        FlowDeviceCode,
		BaseURL:     "https://q.us-east-1.amazonaws.com",
		DocsURL:     "https://docs.aws.amazon.com/amazonq/latest/qdeveloper-ug/q-dev-login.html",
		Implemented: true,
	}
}

func (h awsDeviceHandler) Info() Info { return h.info }

// StartDevice registers a public OIDC client and starts the device
// authorization against the Builder ID start URL. The registered
// clientId/clientSecret ride in the DeviceCode's continuation state
// for the poll.
func (h awsDeviceHandler) StartDevice(ctx context.Context, cl *http.Client) (*DeviceCode, error) {
	registerURL := endpoint(h.info.ID, "register", awsOIDCEndpoint+"/client/register")
	regBody := map[string]any{
		"clientName": awsClientName,
		"clientType": "public",
		"scopes":     awsScopes,
		"grantTypes": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
		"issuerUrl":  awsBuilderIDURL,
	}
	var reg struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := postJSON(ctx, cl, registerURL, regBody, &reg, nil); err != nil {
		return nil, err
	}
	if reg.ClientID == "" || reg.ClientSecret == "" {
		return nil, fmt.Errorf("providers: %s: client registration missing clientId/clientSecret", h.info.ID)
	}

	deviceURL := endpoint(h.info.ID, "deviceauth", awsOIDCEndpoint+"/device_authorization")
	devBody := map[string]any{
		"clientId":     reg.ClientID,
		"clientSecret": reg.ClientSecret,
		"startUrl":     awsBuilderIDURL,
	}
	var dev struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationURI         string `json:"verificationUri"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		ExpiresIn               int    `json:"expiresIn"`
		Interval                int    `json:"interval"`
	}
	if err := postJSON(ctx, cl, deviceURL, devBody, &dev, nil); err != nil {
		return nil, err
	}
	if dev.DeviceCode == "" || dev.UserCode == "" {
		return nil, fmt.Errorf("providers: %s: device authorization missing deviceCode/userCode", h.info.ID)
	}
	return &DeviceCode{
		DeviceCode:              dev.DeviceCode,
		UserCode:                dev.UserCode,
		VerificationURI:         dev.VerificationURI,
		VerificationURIComplete: dev.VerificationURIComplete,
		ExpiresIn:               dev.ExpiresIn,
		Interval:                dev.Interval,
		state: map[string]string{
			"clientId":     reg.ClientID,
			"clientSecret": reg.ClientSecret,
			"deviceCode":   dev.DeviceCode,
		},
	}, nil
}

// PollDevice calls CreateToken once with the device_code grant.
func (h awsDeviceHandler) PollDevice(ctx context.Context, cl *http.Client, dc *DeviceCode) (*TokenResult, error) {
	if dc == nil || dc.state["deviceCode"] == "" {
		return nil, fmt.Errorf("providers: %s: poll called without a device-code offer", h.info.ID)
	}
	tokenURL := endpoint(h.info.ID, "token", awsDeviceTokenURL)
	body := map[string]any{
		"clientId":     dc.state["clientId"],
		"clientSecret": dc.state["clientSecret"],
		"deviceCode":   dc.state["deviceCode"],
		"grantType":    "urn:ietf:params:oauth:grant-type:device_code",
	}
	return h.createToken(ctx, cl, tokenURL, body)
}

// Refresh calls CreateToken with the refresh_token grant (the AWS SSO
// OIDC refresh; the refresh token rotates).
func (h awsDeviceHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoRefresh
	}
	// The registered client credentials are required for CreateToken;
	// they are remembered process-locally when the device flow ran in
	// this process (after a restart the honest refusal points at a
	// re-run of the device flow).
	clientID := refreshTokenState.get(refreshToken, "clientId")
	clientSecret := refreshTokenState.get(refreshToken, "clientSecret")
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("providers: %s: refresh requires the registered OIDC client credentials from the original device flow — reconnect the provider", h.info.ID)
	}
	tokenURL := endpoint(h.info.ID, "token", awsDeviceTokenURL)
	body := map[string]any{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	}
	return h.createToken(ctx, cl, tokenURL, body)
}

// createToken posts a CreateToken request and normalizes the result.
// On success the registered client credentials are remembered against
// the refresh token so a later Refresh can rebuild the request.
func (h awsDeviceHandler) createToken(ctx context.Context, cl *http.Client, tokenURL string, body map[string]any) (*TokenResult, error) {
	var resp struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		TokenType    string `json:"tokenType"`
		ExpiresIn    int    `json:"expiresIn"`
		Error        string `json:"error"`
		ErrorDetail  string `json:"error_description"`
	}
	status, raw, err := doTokenRaw(ctx, cl, tokenURL, http.MethodPost, "application/json", strings.NewReader(mustJSON(body)), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		if code, _, ok := decodeOAuthError(raw); ok && (code == "authorization_pending" || code == "slow_down") {
			return nil, ErrAuthorizationPending
		}
		return nil, fmt.Errorf("providers: %s: create token failed: status %d: %s", h.info.ID, status, truncateBody(raw))
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("providers: %s: decode token response: %w", h.info.ID, err)
	}
	if resp.AccessToken == "" {
		if resp.Error != "" {
			return nil, fmt.Errorf("providers: %s: create token failed: %s (%s)", h.info.ID, resp.Error, resp.ErrorDetail)
		}
		return nil, fmt.Errorf("providers: %s: token response carried no accessToken", h.info.ID)
	}
	if resp.RefreshToken != "" {
		refreshTokenState.put(resp.RefreshToken, dcState(body))
	}
	return &TokenResult{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		TokenType:    resp.TokenType,
		ExpiresIn:    resp.ExpiresIn,
		TokenURL:     tokenURL,
	}, nil
}

// dcState extracts the client credentials from a CreateToken request
// body so they can be re-attached at refresh time.
func dcState(body map[string]any) map[string]string {
	st := map[string]string{}
	if v, ok := body["clientId"].(string); ok {
		st["clientId"] = v
	}
	if v, ok := body["clientSecret"].(string); ok {
		st["clientSecret"] = v
	}
	return st
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// refreshStateStore remembers the registered OIDC client credentials
// per refresh token (CreateToken's refresh grant needs them). This is
// process-local continuation state — the durable credential lives in
// the connection store.
type refreshStateStore struct {
	m sync.Map
}

func (s *refreshStateStore) put(refreshToken string, st map[string]string) {
	if refreshToken == "" || len(st) == 0 {
		return
	}
	s.m.Store(refreshToken, st)
}

func (s *refreshStateStore) get(refreshToken, key string) string {
	v, ok := s.m.Load(refreshToken)
	if !ok {
		return ""
	}
	m, _ := v.(map[string]string)
	return m[key]
}

var refreshTokenState refreshStateStore
