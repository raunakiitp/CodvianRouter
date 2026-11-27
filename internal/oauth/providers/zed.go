package providers

// Zed hosted-model OAuth (catalog "zed", "zed-hosted").
//
// Flow: Zed's own native-app browser sign-in — the flow the Zed
// editor runs for "Sign in with Zed" against zed.dev. An RSA-2048
// key pair is generated; the browser is opened at
// https://zed.dev/native_app_signin with the loopback port, the
// key's public half (PKCS#1 DER, padded URL-safe base64) and a
// system id; zed.dev sends the browser back to that loopback port
// with the user id and the ACCESS TOKEN ENCRYPTED to the key
// (RSA-OAEP SHA-256, PKCS#1 v1.5 as the fallback). There is NO
// refresh grant: the access token lasts until Zed refuses it, and
// the sign-in is then run again.
//
// The ExchangeCode `code` parameter carries the callback's
// `access_token` value (the encrypted blob); the RSA private key and
// system id ride in the verifier the caller binds to the session.
//
// Source: https://github.com/magpie-community/plugins/tree/main/packages/zed
// (the sign-in as the Zed editor runs it, read from Zed's own source
// github.com/zed-industries/zed: zed.dev/native_app_signin with
// native_app_port + native_app_public_key + system_id, the encrypted
// access_token callback, cloud.zed.dev/client/* as the API base,
// "Zed has no refresh token").

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	zedSignInURL  = "https://zed.dev/native_app_signin"
	zedCloudBase  = "https://cloud.zed.dev"
	zedRSABits    = 2048
	zedSystemBits = 96 // hex-encoded system id length in bytes
)

type zedHandler struct {
	baseHandler
	info Info
}

func zedInfo() Info {
	return Info{
		ID:          "zed",
		Name:        "Zed IDE",
		Alias:       "zd",
		Flow:        FlowBrowserSignIn,
		BaseURL:     zedCloudBase,
		DocsURL:     "https://github.com/magpie-community/plugins/tree/main/packages/zed",
		Reason:      "",
		Implemented: true,
	}
}

func zedHostedInfo() Info {
	return Info{
		ID:      "zed-hosted",
		Name:    "Zed Hosted Models",
		Flow:    FlowBrowserSignIn,
		BaseURL: zedCloudBase,
		DocsURL: "https://github.com/magpie-community/plugins/tree/main/packages/zed",
		// The same Zed-account sign-in: the hosted-models surface is
		// what the access token is good for.
		Reason:      "",
		Implemented: true,
	}
}

func (h zedHandler) Info() Info { return h.info }

// zedVerifierState is the continuation data carried in the verifier
// slot between StartAuthorization and ExchangeCode: the RSA private
// key (PKCS#1 DER, base64) and the system id.
type zedVerifierState struct {
	PrivateKey string `json:"private_key"`
	SystemID   string `json:"system_id"`
}

// StartAuthorization builds the zed.dev/native_app_signin URL: the
// loopback port from the redirect URI, the fresh RSA public key and a
// system id, exactly as the Zed editor opens it.
func (h zedHandler) StartAuthorization(redirectURI string) (string, string, string, error) {
	u, err := url.Parse(strings.TrimSpace(redirectURI))
	if err != nil || u.Port() == "" {
		return "", "", "", fmt.Errorf("providers: zed: redirect_uri must be a loopback callback with a port (zed.dev sends the browser back to it): %q", redirectURI)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 || port > 65535 {
		return "", "", "", fmt.Errorf("providers: zed: redirect_uri port invalid: %q", u.Port())
	}
	key, err := rsa.GenerateKey(rand.Reader, zedRSABits)
	if err != nil {
		return "", "", "", fmt.Errorf("providers: zed: rsa keygen: %w", err)
	}
	// Zed's page takes the public half as PKCS#1 DER, URL-safe base64
	// WITH padding (Rust's URL_SAFE encoding).
	pubDER := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	pub := base64.URLEncoding.EncodeToString(pubDER)
	sysID, err := zedSystemID()
	if err != nil {
		return "", "", "", err
	}
	st, err := json.Marshal(zedVerifierState{
		PrivateKey: base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(key)),
		SystemID:   sysID,
	})
	if err != nil {
		return "", "", "", fmt.Errorf("providers: zed: encode verifier state: %w", err)
	}
	state, err := newState()
	if err != nil {
		return "", "", "", err
	}
	authURL := endpoint(h.info.ID, "auth", zedSignInURL)
	q := url.Values{}
	q.Set("native_app_port", strconv.Itoa(port))
	q.Set("native_app_public_key", pub)
	q.Set("system_id", sysID)
	return state, authURL + "?" + q.Encode(), string(st), nil
}

// ExchangeCode decrypts the callback's encrypted access token with the
// session's RSA private key. Zed's sign-in carries no authorization
// code: the callback itself delivers the credential.
func (h zedHandler) ExchangeCode(ctx context.Context, cl *http.Client, code, redirectURI, verifier string) (*TokenResult, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("providers: zed: the callback's encrypted access_token is required as the code")
	}
	var st zedVerifierState
	if err := json.Unmarshal([]byte(verifier), &st); err != nil || st.PrivateKey == "" {
		return nil, fmt.Errorf("providers: zed: verifier does not carry the sign-in's RSA key (run StartAuthorization first)")
	}
	der, err := base64.StdEncoding.DecodeString(st.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("providers: zed: decode verifier private key: %w", err)
	}
	key, err := x509.ParsePKCS1PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("providers: zed: parse verifier private key: %w", err)
	}
	// The ciphertext arrives URL-safe base64; padding is optional.
	normalized := strings.TrimRight(code, "=")
	normalized = strings.NewReplacer("-", "+", "_", "/").Replace(normalized)
	ct, err := base64.StdEncoding.DecodeString(normalized)
	if err != nil {
		// Unpadded input: decode raw.
		ct, err = base64.RawStdEncoding.DecodeString(normalized)
		if err != nil {
			return nil, fmt.Errorf("providers: zed: decode callback access_token: %w", err)
		}
	}
	token, err := zedDecryptToken(key, ct)
	if err != nil {
		return nil, err
	}
	return &TokenResult{
		AccessToken: token,
		TokenType:   "bearer",
		// No expiry is declared: the token lasts until Zed refuses it.
		ExpiresIn: 0,
		// No refresh grant exists (see Refresh), so no TokenURL.
		TokenURL: "",
	}, nil
}

// Refresh is a documented non-grant: Zed has no refresh token — the
// access token lasts until Zed refuses it and the sign-in is run
// again.
func (zedHandler) Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error) {
	return nil, ErrNoRefresh
}

// zedDecryptToken decrypts the callback token: RSA-OAEP SHA-256, with
// PKCS#1 v1.5 as the older fallback.
func zedDecryptToken(key *rsa.PrivateKey, ct []byte) (string, error) {
	if plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, ct, nil); err == nil {
		return string(bytes.TrimSpace(plain)), nil
	}
	plain, err := rsa.DecryptPKCS1v15(rand.Reader, key, ct)
	if err != nil {
		return "", fmt.Errorf("providers: zed: decrypt callback access_token: %w", err)
	}
	return string(bytes.TrimSpace(plain)), nil
}

// zedSystemID is the random system id the sign-in names (UUID-shaped
// from random bytes).
func zedSystemID() (string, error) {
	b := make([]byte, zedSystemBits)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("providers: zed: system id entropy: %w", err)
	}
	hex := make([]byte, len(b)*2)
	const digits = "0123456789abcdef"
	for i, v := range b {
		hex[i*2] = digits[v>>4]
		hex[i*2+1] = digits[v&0x0f]
	}
	s := string(hex)
	// 8-4-4-4-12 layout for a UUID-shaped id.
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}
