package oauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var testSigningKey = []byte("test-signing-key-32-bytes-long!!")

func signTestToken(t *testing.T, key []byte, clientID string, scopes []string, now time.Time) string {
	t.Helper()
	tok, err := SignJWT(key, clientID, scopes, now)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	return tok
}

// craftToken signs an arbitrary claims payload with key, so tests
// can build tokens SignJWT would never mint (wrong alg, wrong
// issuer) and prove VerifyJWT rejects them.
func craftToken(t *testing.T, key []byte, header string, claims jwtClaims) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	unsigned := base64URLEncode([]byte(header)) + "." + base64URLEncode(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(unsigned))
	return unsigned + "." + base64URLEncode(mac.Sum(nil))
}

func TestSignJWTVerifyRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tok := signTestToken(t, testSigningKey, "oc_test123", []string{"chat", "models"}, now)
	if parts := strings.Split(tok, "."); len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	claims, err := VerifyJWT(testSigningKey, tok, now)
	if err != nil {
		t.Fatalf("VerifyJWT: %v", err)
	}
	if claims.ClientID != "oc_test123" {
		t.Errorf("ClientID = %q, want oc_test123", claims.ClientID)
	}
	if claims.Issuer != IssuerName || claims.Audience != IssuerName {
		t.Errorf("iss/aud = %q/%q, want %q", claims.Issuer, claims.Audience, IssuerName)
	}
	if claims.Subject != "oauth-client:oc_test123" {
		t.Errorf("sub = %q, want oauth-client:oc_test123", claims.Subject)
	}
	if claims.Scope != "chat models" {
		t.Errorf("scope = %q, want \"chat models\"", claims.Scope)
	}
	if claims.Expires != now.Add(AccessTokenTTL).Unix() {
		t.Errorf("exp = %d, want %d", claims.Expires, now.Add(AccessTokenTTL).Unix())
	}
	if claims.IssuedAt != now.Unix() {
		t.Errorf("iat = %d, want %d", claims.IssuedAt, now.Unix())
	}
	if claims.ID == "" {
		t.Error("jti is empty")
	}
	// Still valid 59 minutes later.
	if _, err := VerifyJWT(testSigningKey, tok, now.Add(59*time.Minute)); err != nil {
		t.Errorf("VerifyJWT 59m later: %v", err)
	}
}

func TestVerifyJWTExpiredRejected(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tok := signTestToken(t, testSigningKey, "oc_test123", []string{"chat"}, now)
	// Two hours later: past the 1-hour TTL.
	if _, err := VerifyJWT(testSigningKey, tok, now.Add(2*time.Hour)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("VerifyJWT 2h later: err = %v, want ErrTokenExpired", err)
	}
	// Exactly at expiry: now >= exp, so expired.
	if _, err := VerifyJWT(testSigningKey, tok, now.Add(AccessTokenTTL)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("VerifyJWT at expiry: err = %v, want ErrTokenExpired", err)
	}
	// One second before expiry: still valid.
	if _, err := VerifyJWT(testSigningKey, tok, now.Add(AccessTokenTTL-time.Second)); err != nil {
		t.Errorf("VerifyJWT 1s before expiry: %v", err)
	}
}

func TestVerifyJWTWrongKeyRejected(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tok := signTestToken(t, testSigningKey, "oc_test123", []string{"chat"}, now)
	otherKey := []byte("a-different-signing-key-32-bytes!")
	if _, err := VerifyJWT(otherKey, tok, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("VerifyJWT wrong key: err = %v, want ErrTokenInvalid", err)
	}
}

func TestVerifyJWTTamperedPayloadRejected(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tok := signTestToken(t, testSigningKey, "oc_test123", []string{"chat"}, now)
	parts := strings.Split(tok, ".")
	raw, err := base64URLDecode(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	m["client_id"] = "oc_attacker"
	tampered, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	bad := parts[0] + "." + base64URLEncode(tampered) + "." + parts[2]
	if _, err := VerifyJWT(testSigningKey, bad, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("VerifyJWT tampered payload: err = %v, want ErrTokenInvalid", err)
	}
}

func TestVerifyJWTTamperedSignatureRejected(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tok := signTestToken(t, testSigningKey, "oc_test123", []string{"chat"}, now)
	parts := strings.Split(tok, ".")
	// Change the FIRST signature character: it carries 6 full
	// bits, so any different base64url char changes the decoded
	// bytes (the last char carries only 4 bits and could alias).
	sig := parts[2]
	replacement := byte('A')
	if sig[0] == 'A' {
		replacement = 'B'
	}
	bad := parts[0] + "." + parts[1] + "." + string(replacement) + sig[1:]
	if _, err := VerifyJWT(testSigningKey, bad, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("VerifyJWT tampered signature: err = %v, want ErrTokenInvalid", err)
	}
}

func TestVerifyJWTWrongAlgRejected(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	claims := jwtClaims{
		Issuer:   IssuerName,
		Subject:  "oauth-client:oc_test123",
		Audience: IssuerName,
		Expires:  now.Add(time.Hour).Unix(),
		IssuedAt: now.Unix(),
		ID:       "jti-none",
		ClientID: "oc_test123",
		Scope:    "chat",
	}
	// alg "none" with a correctly-computed signature: the alg
	// check must reject it before the signature is even trusted.
	bad := craftToken(t, testSigningKey, `{"alg":"none","typ":"JWT"}`, claims)
	if _, err := VerifyJWT(testSigningKey, bad, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("VerifyJWT alg=none: err = %v, want ErrTokenInvalid", err)
	}
	// alg HS512: also rejected.
	bad = craftToken(t, testSigningKey, `{"alg":"HS512","typ":"JWT"}`, claims)
	if _, err := VerifyJWT(testSigningKey, bad, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("VerifyJWT alg=HS512: err = %v, want ErrTokenInvalid", err)
	}
}

func TestVerifyJWTWrongIssuerRejected(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	claims := jwtClaims{
		Issuer:   "evil-issuer",
		Subject:  "oauth-client:oc_test123",
		Audience: "evil-issuer",
		Expires:  now.Add(time.Hour).Unix(),
		IssuedAt: now.Unix(),
		ID:       "jti-evil",
		ClientID: "oc_test123",
		Scope:    "chat",
	}
	bad := craftToken(t, testSigningKey, `{"alg":"HS256","typ":"JWT"}`, claims)
	if _, err := VerifyJWT(testSigningKey, bad, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("VerifyJWT wrong issuer: err = %v, want ErrTokenInvalid", err)
	}
}

func TestVerifyJWTMalformedRejected(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	key := testSigningKey
	// A token with a valid signature over a non-JSON payload:
	// the payload unmarshal must fail after the signature passes.
	unsigned := base64URLEncode(jwtHeader) + "." + base64URLEncode([]byte("not json"))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(unsigned))
	signedNonJSON := unsigned + "." + base64URLEncode(mac.Sum(nil))
	// A token with a valid signature over an undecodable payload.
	unsignedBadB64 := base64URLEncode(jwtHeader) + ".!!!"
	mac = hmac.New(sha256.New, key)
	mac.Write([]byte(unsignedBadB64))
	signedBadB64 := unsignedBadB64 + "." + base64URLEncode(mac.Sum(nil))
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"one part", "abc"},
		{"two parts", "abc.def"},
		{"four parts", "a.b.c.d"},
		{"bad header base64", "!!!.e30.c2ln"},
		{"non-JSON payload, valid signature", signedNonJSON},
		{"bad payload base64, valid signature", signedBadB64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := VerifyJWT(key, tc.token, now); !errors.Is(err, ErrTokenInvalid) {
				t.Fatalf("VerifyJWT(%q): err = %v, want ErrTokenInvalid", tc.token, err)
			}
		})
	}
}

func TestTokenExpiry(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tok := signTestToken(t, testSigningKey, "oc_test123", []string{"chat"}, now)
	exp, ok := TokenExpiry(tok)
	if !ok {
		t.Fatal("TokenExpiry: ok = false, want true")
	}
	if !exp.Equal(time.Unix(now.Add(AccessTokenTTL).Unix(), 0)) {
		t.Errorf("TokenExpiry = %v, want %v", exp, now.Add(AccessTokenTTL))
	}
	// No signature check: expiry is readable even for a token
	// signed with the wrong key.
	other := signTestToken(t, []byte("a-different-signing-key-32-bytes!"), "oc_test123", []string{"chat"}, now)
	if _, ok := TokenExpiry(other); !ok {
		t.Error("TokenExpiry on wrong-key token: ok = false, want true")
	}
	for _, bad := range []string{"", "a.b", "a.!!!.c"} {
		if _, ok := TokenExpiry(bad); ok {
			t.Errorf("TokenExpiry(%q): ok = true, want false", bad)
		}
	}
}

func TestNewCodeVerifier(t *testing.T) {
	v1, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier: %v", err)
	}
	// RFC 7636 §4.1: 43 characters from 32 random bytes.
	if len(v1) != 43 {
		t.Fatalf("verifier length = %d, want 43", len(v1))
	}
	for _, c := range v1 {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			t.Fatalf("verifier contains non-base64url character %q", c)
		}
	}
	v2, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier: %v", err)
	}
	if v1 == v2 {
		t.Error("two verifiers are identical; random source is not random")
	}
}

func TestCodeChallengeS256RFC7636Vector(t *testing.T) {
	// RFC 7636 Appendix B test vector.
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	want := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := CodeChallengeS256(verifier); got != want {
		t.Fatalf("CodeChallengeS256 = %q, want %q", got, want)
	}
	// Challenge is deterministic and differs from the verifier.
	if got := CodeChallengeS256(verifier); got == verifier {
		t.Error("challenge equals verifier")
	}
}

func TestVerifyCodeChallenge(t *testing.T) {
	verifier, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier: %v", err)
	}
	challenge := CodeChallengeS256(verifier)
	if !VerifyCodeChallenge(challenge, "S256", verifier) {
		t.Error("VerifyCodeChallenge(correct verifier) = false, want true")
	}
	wrong, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier: %v", err)
	}
	if VerifyCodeChallenge(challenge, "S256", wrong) {
		t.Error("VerifyCodeChallenge(wrong verifier) = true, want false")
	}
	// "plain" is rejected outright, even when the challenge is
	// the verifier itself.
	if VerifyCodeChallenge(verifier, "plain", verifier) {
		t.Error("VerifyCodeChallenge(method=plain) = true, want false")
	}
	if VerifyCodeChallenge(challenge, "S256", "") {
		t.Error("VerifyCodeChallenge(empty verifier) = true, want false")
	}
	if VerifyCodeChallenge("", "S256", verifier) {
		t.Error("VerifyCodeChallenge(empty challenge) = true, want false")
	}
	if VerifyCodeChallenge(challenge, "", verifier) {
		t.Error("VerifyCodeChallenge(empty method) = true, want false")
	}
}

// TestCodeChallengeS256MatchesStdlib pins the challenge to a
// direct SHA-256 computation, so a future refactor cannot
// silently change the derivation.
func TestCodeChallengeS256MatchesStdlib(t *testing.T) {
	verifier := "test-verifier-1234567890abcdefghij"
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := CodeChallengeS256(verifier); got != want {
		t.Fatalf("CodeChallengeS256 = %q, want %q", got, want)
	}
}
