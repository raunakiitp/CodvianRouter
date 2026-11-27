package providers

// Trae OAuth (catalog "trae").
//
// Flow: Trae's browser authorization page — the sign-in the Trae IDE
// runs. The authorization page (www.trae.ai/authorization for the
// international realm, www.trae.cn/authorization for CN) is opened
// with the IDE's client id and the loopback callback; the page sends
// the browser back to `http://127.0.0.1:<port>/authorize` carrying
// the account's token pair (`userJwt`) or, from its older page, a
// refresh token alone. A bare refresh token is exchanged for the
// token pair at the realm auth host's
// /cloudide/api/v3/trae/oauth/ExchangeToken — which is also the
// documented renewal: Trae issues a NEW refresh token each time and
// spends the old one, so the rotated pair must be persisted.
//
// The realms are separate services (trae.cn and trae.ai accounts are
// not interchangeable); the catalog carries one entry, so the realm
// is operator-selected via CODVIAN_OAUTH_TRAE_REALM ("global",
// default, or "cn").
//
// The ExchangeCode `code` parameter carries the callback payload:
// either the `userJwt` JSON blob or a bare `refreshToken` value.
//
// Source: https://github.com/keenlin/magpie-plugins/tree/main/packages/trae
// (@magpie-community/opencode-trae-auth, which documents the IDE's
// own sign-in: authorization pages, IDE client ono9krqynydwx5, the
// 127.0.0.1/authorize callback shape, and the ExchangeToken renewal
// with its rotating refresh token).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	traeClientIDEnv = "CODVIAN_OAUTH_TRAE_REALM"
	traeClientID    = "ono9krqynydwx5" // the Trae IDE client both realms sign in with
)

// traeRealm is one deployment's sign-in hosts.
type traeRealm struct {
	name    string
	authURL string // the authorization page
	authAPI string // the token/renewal host
}

func traeRealms() (global, cn traeRealm) {
	return traeRealm{
			name:    "Trae Global",
			authURL: "https://www.trae.ai/authorization",
			authAPI: "https://growsg-normal.trae.ai",
		},
		traeRealm{
			name:    "Trae CN",
			authURL: "https://www.trae.cn/authorization",
			authAPI: "https://api.trae.cn",
		}
}

// traeRealmFor returns the operator-selected realm (default global).
func traeRealmFor() (traeRealm, error) {
	global, cn := traeRealms()
	switch strings.TrimSpace(strings.ToLower(os.Getenv(traeClientIDEnv))) {
	case "", "global", "intl", "trae.ai":
		return global, nil
	case "cn", "trae.cn":
		return cn, nil
	default:
		return traeRealm{}, fmt.Errorf("providers: trae: %s must be \"global\" or \"cn\" (the realms are separate services)", traeClientIDEnv)
	}
}

type traeHandler struct {
	baseHandler
}

func traeInfo() Info {
	return Info{
		ID:          "trae",
		Name:        "Trae",
		Alias:       "tr",
		Flow:        FlowBrowserSignIn,
		BaseURL:     "https://growsg-normal.trae.ai",
		DocsURL:     "https://github.com/keenlin/magpie-plugins/tree/main/packages/trae",
		Reason:      "",
		Implemented: true,
	}
}

func (traeHandler) Info() Info { return traeInfo() }

// StartAuthorization builds the realm's authorization-page URL with
// the IDE's client, the loopback callback and the device identity the
// page expects.
func (traeHandler) StartAuthorization(redirectURI string) (string, string, string, error) {
	realm, err := traeRealmFor()
	if err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(redirectURI) == "" {
		return "", "", "", fmt.Errorf("providers: trae: redirect_uri is required (the authorization page sends the browser back to http://127.0.0.1:<port>/authorize)")
	}
	deviceID, err := traeDeviceID(realm)
	if err != nil {
		return "", "", "", err
	}
	machineID, err := traeMachineID()
	if err != nil {
		return "", "", "", err
	}
	trace, err := traeMachineID() // UUID-shaped trace id; 32 hex is fine
	if err != nil {
		return "", "", "", err
	}
	state, err := newState()
	if err != nil {
		return "", "", "", err
	}
	authURL := endpoint("trae", "auth", realm.authURL)
	q := url.Values{}
	q.Set("login_version", "1")
	q.Set("auth_from", "solo")
	q.Set("login_channel", "native_ide")
	q.Set("auth_type", "local")
	q.Set("client_id", traeClientID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", trace)
	q.Set("auth_callback_url", redirectURI)
	q.Set("machine_id", machineID)
	q.Set("device_id", deviceID)
	q.Set("x_device_id", deviceID)
	q.Set("x_machine_id", machineID)
	q.Set("hide_saas_login", "false")
	return state, authURL + "?" + q.Encode(), "", nil
}

// traeTokenPair is the callback's userJwt / ExchangeToken result shape.
type traeTokenPair struct {
	Token         string `json:"Token"`
	RefreshToken  string `json:"RefreshToken"`
	TokenExpireAt any    `json:"TokenExpireAt"`
}

func (p traeTokenPair) result(tokenURL string) (*TokenResult, error) {
	if p.Token == "" {
		return nil, fmt.Errorf("providers: trae: token response carried no Token")
	}
	tr := &TokenResult{
		AccessToken:  p.Token,
		RefreshToken: p.RefreshToken,
		TokenType:    "bearer",
		TokenURL:     tokenURL,
	}
	switch v := p.TokenExpireAt.(type) {
	case float64:
		if v > 0 {
			tr.ExpiresIn = max(1, int(time.Until(time.Unix(int64(v), 0)).Seconds()))
		}
	case string:
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			tr.ExpiresIn = max(1, int(time.Until(ts).Seconds()))
		}
	}
	return tr, nil
}

// ExchangeCode normalizes the callback payload into a token pair. A
// bare refresh token (the older callback shape) is exchanged for the
// pair; a userJwt JSON blob is taken as delivered.
func (traeHandler) ExchangeCode(ctx context.Context, cl *http.Client, code, redirectURI, verifier string) (*TokenResult, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("providers: trae: the callback's userJwt or refreshToken is required as the code")
	}
	var pair traeTokenPair
	if err := json.Unmarshal([]byte(code), &pair); err == nil && pair.Token != "" {
		// The page delivered the pair; no network call needed.
		return pair.result("")
	}
	// Bare refresh token (or a JSON blob without a token): exchange it
	// for the pair at the realm's auth host.
	return traeExchangeToken(ctx, cl, strings.TrimSpace(code))
}

// Refresh swaps the stored refresh token for a fresh (rotated) pair at
// the realm's ExchangeToken endpoint.
func (traeHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ErrNoRefresh
	}
	return traeExchangeToken(ctx, cl, refreshToken)
}

// traeExchangeToken POSTs the realm's /cloudide/api/v3/trae/oauth/
// ExchangeToken: the documented renewal that issues a NEW refresh
// token each time and spends the old one.
func traeExchangeToken(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	realm, err := traeRealmFor()
	if err != nil {
		return nil, err
	}
	tokenURL := endpoint("trae", "token", realm.authAPI+"/cloudide/api/v3/trae/oauth/ExchangeToken")
	body := map[string]string{
		"ClientID":     traeClientID,
		"RefreshToken": refreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	var resp struct {
		Result *traeTokenPair `json:"Result"`
		// Some deployments answer unwrapped.
		traeTokenPair
	}
	if err := postJSON(ctx, cl, tokenURL, body, &resp, nil); err != nil {
		return nil, err
	}
	pair := resp.Result
	if pair == nil {
		pair = &resp.traeTokenPair
	}
	return pair.result(tokenURL)
}

// traeDeviceID generates the device id the authorization page expects:
// 19 digits for the international realm, 16 for CN.
func traeDeviceID(realm traeRealm) (string, error) {
	n := 19
	if realm.name == "Trae CN" {
		n = 16
	}
	s := make([]byte, n)
	for i := range s {
		d, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", fmt.Errorf("providers: trae: device id entropy: %w", err)
		}
		s[i] = byte('0' + d.Int64())
	}
	return string(s), nil
}

// traeMachineID is the 32-hex machine identity the sign-in names.
func traeMachineID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("providers: trae: machine id entropy: %w", err)
	}
	const digits = "0123456789abcdef"
	out := make([]byte, 32)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0f]
	}
	return string(out), nil
}
