// Package oauth is the gateway's own OAuth 2.0 authorization server
// (PARITY.md:678, B3-07): gateway clients authenticate with an
// authorization-code + PKCE flow and call /v1 routes with a JWT
// access token, instead of a gateway key. Upstream provider OAuth
// (provider login, token minting for Vertex etc.) is a separate
// surface and is NOT this package.
//
// The JWT is HS256 (HMAC-SHA-256) implemented on the standard
// library only — no new dependency — mirroring the stdlib RS256
// JWT the Vertex client already mints (internal/provider/
// vertex_client.go). Access tokens live 1 hour; refresh tokens
// are opaque, single-use-rotated, and live 30 days. The signing
// key is generated once and persisted to <data-dir>/oauth.json
// (0600) so tokens survive restarts; with no data dir the key is
// in-memory only and every token dies with the process (honest,
// documented in NewIssuer).
package oauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrTokenExpired is returned when a token's signature is valid
// but its exp has passed; the caller can name the expiry in the
// 401 instead of reporting a generic invalid token.
var ErrTokenExpired = errors.New("oauth: token expired")

// ErrTokenInvalid is returned for a malformed token, a bad
// signature, a wrong alg, or a wrong issuer/audience.
var ErrTokenInvalid = errors.New("oauth: token invalid")

// IssuerName is the iss/aud value of every gateway JWT.
const IssuerName = "codvian-router"

// AccessTokenTTL is the lifetime of a minted JWT access token.
const AccessTokenTTL = time.Hour

// RefreshTokenTTL is the lifetime of a refresh token.
const RefreshTokenTTL = 30 * 24 * time.Hour

// CodeTTL is the lifetime of an authorization code (single-use).
const CodeTTL = 10 * time.Minute

// base64URLEncode encodes without padding, as RFC 7515 requires.
func base64URLEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func base64URLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// randomToken returns n crypto-random bytes as a base64url string
// (no padding). It is used for codes, refresh tokens and client
// secrets — never for anything that must be memorable.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauth: random source: %w", err)
	}
	return base64URLEncode(b), nil
}

// jwtHeader is the fixed JOSE header of every gateway token.
var jwtHeader = []byte(`{"alg":"HS256","typ":"JWT"}`)

// jwtClaims is the payload of a gateway access token. Scope is a
// space-joined list, as RFC 6749 requires.
type jwtClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	Expires  int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
	ID       string `json:"jti"`
	ClientID string `json:"client_id"`
	Scope    string `json:"scope"`
}

// SignJWT mints an HS256 JWT for clientID with the given scopes.
// The token expires AccessTokenTTL after now.
func SignJWT(key []byte, clientID string, scopes []string, now time.Time) (string, error) {
	jti, err := randomToken(16)
	if err != nil {
		return "", err
	}
	claims := jwtClaims{
		Issuer:   IssuerName,
		Subject:  "oauth-client:" + clientID,
		Audience: IssuerName,
		Expires:  now.Add(AccessTokenTTL).Unix(),
		IssuedAt: now.Unix(),
		ID:       jti,
		ClientID: clientID,
		Scope:    strings.Join(scopes, " "),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("oauth: marshal claims: %w", err)
	}
	unsigned := base64URLEncode(jwtHeader) + "." + base64URLEncode(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(unsigned))
	sig := base64URLEncode(mac.Sum(nil))
	return unsigned + "." + sig, nil
}

// VerifyJWT checks a token's shape, alg, signature, issuer,
// audience and expiry, and returns its claims. A signature-valid
// but expired token returns ErrTokenExpired (wrapping the expiry
// so the caller can name it); anything else malformed returns
// ErrTokenInvalid.
func VerifyJWT(key []byte, token string, now time.Time) (*jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrTokenInvalid
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	rawHeader, err := base64URLDecode(parts[0])
	if err != nil {
		return nil, ErrTokenInvalid
	}
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return nil, ErrTokenInvalid
	}
	if header.Alg != "HS256" {
		return nil, ErrTokenInvalid
	}
	unsigned := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(unsigned))
	want := mac.Sum(nil)
	got, err := base64URLDecode(parts[2])
	if err != nil {
		return nil, ErrTokenInvalid
	}
	if !hmac.Equal(got, want) {
		return nil, ErrTokenInvalid
	}
	rawPayload, err := base64URLDecode(parts[1])
	if err != nil {
		return nil, ErrTokenInvalid
	}
	var claims jwtClaims
	if err := json.Unmarshal(rawPayload, &claims); err != nil {
		return nil, ErrTokenInvalid
	}
	if claims.Issuer != IssuerName || claims.Audience != IssuerName {
		return nil, ErrTokenInvalid
	}
	if claims.Expires == 0 || now.Unix() >= claims.Expires {
		return nil, fmt.Errorf("%w on %s", ErrTokenExpired,
			time.Unix(claims.Expires, 0).UTC().Format(time.RFC3339))
	}
	return &claims, nil
}

// TokenExpiry parses a token's exp claim WITHOUT verifying the
// signature. It exists only so the 401 path can name an expiry
// for a token whose signature already failed elsewhere — callers
// must never treat its result as authenticated.
func TokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64URLDecode(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims jwtClaims
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Expires == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Expires, 0), true
}

// NewCodeVerifier returns a PKCE code verifier: 43 base64url
// characters from 32 random bytes (RFC 7636 §4.1).
func NewCodeVerifier() (string, error) {
	return randomToken(32)
}

// CodeChallengeS256 returns the S256 code challenge for a
// verifier: base64url(SHA-256(verifier)) (RFC 7636 §4.2).
func CodeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64URLEncode(sum[:])
}

// VerifyCodeChallenge reports whether verifier matches challenge
// under method. Only S256 is accepted — "plain" is rejected
// outright, because a plain challenge is no challenge at all on
// a public client.
func VerifyCodeChallenge(challenge, method, verifier string) bool {
	if method != "S256" || challenge == "" || verifier == "" {
		return false
	}
	return hmac.Equal([]byte(CodeChallengeS256(verifier)), []byte(challenge))
}
