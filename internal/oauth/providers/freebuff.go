package providers

// Freebuff OAuth (catalog "freebuff").
//
// Flow: the Freebuff CLI's device-style login — the provider's own
// published surface (the official CLI is open source at
// github.com/CodebuffAI/freebuff; its login flow is documented
// step-by-step in the freebuff2api write-up below):
//
//	POST {base}/api/auth/cli/code    {"fingerprintId": "..."} ->
//	  {loginUrl, fingerprintHash, expiresAt}
//	GET  {base}/api/auth/cli/status  ?fingerprintId=...&fingerprintHash=...&expiresAt=...
//	  401 "Authentication failed" while the user has not signed in
//	  (retried silently, as the official client does); on approval a
//	  200 with {user:{..., authToken}} — the account's credential.
//
// There is NO refresh grant: the authToken is the browser-session
// credential and one account holds one active session at a time — a
// lapsed token means a new login. So TokenURL is empty and Refresh
// refuses with ErrNoRefresh.
//
// Source: https://github.com/chenjh16/freebuff2api/blob/main/docs/en/02-login-flow.md
// (the official CLI's login flow: endpoints, request/response shapes,
// the 401-means-pending poll semantics, single-active-session
// behavior).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

const freebuffBaseURL = "https://freebuff.com"

type freebuffHandler struct {
	baseHandler
}

func freebuffInfo() Info {
	return Info{
		ID:          "freebuff",
		Name:        "Freebuff",
		Alias:       "freebuff",
		Flow:        FlowDeviceCode,
		BaseURL:     freebuffBaseURL,
		DocsURL:     "https://github.com/chenjh16/freebuff2api/blob/main/docs/en/02-login-flow.md",
		Reason:      "",
		Implemented: true,
	}
}

func (freebuffHandler) Info() Info { return freebuffInfo() }

// StartDevice requests the one-time login code and returns the
// sign-in URL. The fingerprint identity and hash ride in the
// DeviceCode's continuation state for the poll.
func (freebuffHandler) StartDevice(ctx context.Context, cl *http.Client) (*DeviceCode, error) {
	fingerprintID, err := freebuffFingerprint()
	if err != nil {
		return nil, err
	}
	codeURL := endpoint("freebuff", "device", freebuffBaseURL+"/api/auth/cli/code")
	var resp struct {
		LoginURL        string `json:"loginUrl"`
		FingerprintHash string `json:"fingerprintHash"`
		ExpiresAt       int64  `json:"expiresAt"`
	}
	if err := postJSON(ctx, cl, codeURL, map[string]string{"fingerprintId": fingerprintID}, &resp, nil); err != nil {
		return nil, err
	}
	if resp.LoginURL == "" || resp.FingerprintHash == "" {
		return nil, fmt.Errorf("providers: freebuff: login-code response missing loginUrl/fingerprintHash")
	}
	return &DeviceCode{
		// loginUrl is https://freebuff.com/login?auth_code=... — the
		// page the user signs in at.
		VerificationURI: resp.LoginURL,
		ExpiresIn:       int(resp.ExpiresAt),
		state: map[string]string{
			"fingerprintId":   fingerprintID,
			"fingerprintHash": resp.FingerprintHash,
			"expiresAt":       fmt.Sprintf("%d", resp.ExpiresAt),
		},
	}, nil
}

// PollDevice polls the login status ONCE. The documented pending
// signal is the 401 "Authentication failed" (retried silently by the
// official client); approval returns the user record with authToken.
func (freebuffHandler) PollDevice(ctx context.Context, cl *http.Client, dc *DeviceCode) (*TokenResult, error) {
	if dc == nil || dc.state["fingerprintId"] == "" || dc.state["fingerprintHash"] == "" {
		return nil, fmt.Errorf("providers: freebuff: poll called without a login-code offer")
	}
	statusURL := endpoint("freebuff", "token", freebuffBaseURL+"/api/auth/cli/status")
	q := url.Values{}
	q.Set("fingerprintId", dc.state["fingerprintId"])
	q.Set("fingerprintHash", dc.state["fingerprintHash"])
	q.Set("expiresAt", dc.state["expiresAt"])
	status, raw, err := doTokenRaw(ctx, cl, statusURL+"?"+q.Encode(), http.MethodGet, "", nil, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		// Documented pending: the user has not approved yet.
		return nil, ErrAuthorizationPending
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("providers: freebuff: login status failed: status %d: %s", status, truncateBody(raw))
	}
	var resp struct {
		User struct {
			AuthToken string `json:"authToken"`
			ID        string `json:"id"`
			Email     string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("providers: freebuff: decode login status: %w", err)
	}
	if resp.User.AuthToken == "" {
		return nil, fmt.Errorf("providers: freebuff: login status carried no authToken")
	}
	return &TokenResult{
		AccessToken: resp.User.AuthToken,
		TokenType:   "bearer",
		// No refresh grant and no declared expiry: the token lasts
		// until the session is taken over elsewhere.
		ExpiresIn: 0,
		TokenURL:  "",
	}, nil
}

// Refresh refuses: the documented flow has no refresh grant — a lapsed
// authToken means a fresh login.
func (freebuffHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	return nil, ErrNoRefresh
}

// freebuffFingerprint is the persistent install identity the CLI
// logs in with ("codebuff-cli-<random>").
func freebuffFingerprint() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("providers: freebuff: fingerprint entropy: %w", err)
	}
	return "codebuff-cli-" + hex.EncodeToString(b), nil
}
