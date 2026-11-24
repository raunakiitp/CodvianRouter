package oauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// KnownScopes is the full scope vocabulary an OAuth client may
// be granted: exactly the scopes gateway keys carry ("admin"
// implies every scope, as on gateway keys).
var KnownScopes = []string{"admin", "chat", "models"}

// DefaultScopes is what an authorize request with no scope gets:
// a usable key, never an admin grant.
var DefaultScopes = []string{"chat", "models"}

func validScopes(scopes []string) ([]string, error) {
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		known := false
		for _, k := range KnownScopes {
			if s == k {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("oauth: unknown scope %q", s)
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// subsetOf reports whether every scope in want is in have (an
// "admin" in have covers every want — the same hierarchy as
// gateway keys).
func subsetOf(want, have []string) bool {
	for _, w := range want {
		ok := false
		for _, h := range have {
			if h == w || h == "admin" {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Client is one registered OAuth client. SecretHash is the
// SHA-256 of the client secret; the raw secret is shown once at
// registration and never stored. Public clients (PKCE-only) have
// an empty SecretHash and authenticate by code_challenge alone.
type Client struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	SecretHash   string    `json:"secret_hash,omitempty"`
	RedirectURIs []string  `json:"redirect_uris"`
	Scopes       []string  `json:"scopes"`
	Public       bool      `json:"public"`
	CreatedAt    time.Time `json:"created_at"`
}

// ClientView is the public shape of a client: no secret material.
// The identifier serializes as "client_id" (RFC 7591 §3.2), matching
// the client_id the authorize/token endpoints consume — a create
// response that said "id" forced every caller to rename it by hand.
type ClientView struct {
	ID           string    `json:"client_id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	Scopes       []string  `json:"scopes"`
	Public       bool      `json:"public"`
	CreatedAt    time.Time `json:"created_at"`
}

func (c *Client) View() ClientView {
	return ClientView{
		ID:           c.ID,
		Name:         c.Name,
		RedirectURIs: append([]string(nil), c.RedirectURIs...),
		Scopes:       append([]string(nil), c.Scopes...),
		Public:       c.Public,
		CreatedAt:    c.CreatedAt,
	}
}

// authCode is a single-use authorization code. Only the hash is
// stored; the raw code is handed to the client once (in the
// authorize redirect) and never logged.
type authCode struct {
	Hash        string
	ClientID    string
	RedirectURI string
	Scopes      []string
	Challenge   string
	Method      string
	ApprovedBy  string // gateway-key ID of the approving operator
	ExpiresAt   time.Time
}

// refreshToken is one stored refresh token (hash only).
type refreshToken struct {
	Hash      string    `json:"hash"`
	ClientID  string    `json:"client_id"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AccessClaims is the verified identity of a JWT access token:
// the client it was issued to and the scopes it carries.
type AccessClaims struct {
	ClientID  string
	Scopes    []string
	ExpiresAt time.Time
	JTI       string
}

// TokenResponse is the RFC 6749 §5.1 token response body.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
}

type persistedState struct {
	SigningKey    string          `json:"signing_key"`
	Clients       []*Client       `json:"clients"`
	RefreshTokens []*refreshToken `json:"refresh_tokens"`
}

// Issuer is the gateway's OAuth 2.0 authorization server: client
// registry, authorization codes, JWT mint/verify, refresh-token
// rotation and revocation. All state is mutex-guarded; codes live
// in memory only (10-minute TTL), while the signing key, clients
// and refresh tokens persist to <data-dir>/oauth.json (0600)
// whenever a data dir is configured. With no data dir everything
// is in-memory and tokens die with the process.
type Issuer struct {
	mu      sync.Mutex
	file    string // "" = in-memory only
	key     []byte
	clients map[string]*Client
	codes   map[string]*authCode
	refresh map[string]*refreshToken // keyed by hash
	revoked map[string]time.Time     // revoked JWT jti -> expiry
}

// NewIssuer loads the OAuth state from <dataDir>/oauth.json, or
// creates it (fresh 32-byte signing key) when the file is absent.
// A dataDir of "" means in-memory only: a fresh key is generated
// and nothing is ever written to disk.
func NewIssuer(dataDir string) (*Issuer, error) {
	iss := &Issuer{
		clients: make(map[string]*Client),
		codes:   make(map[string]*authCode),
		refresh: make(map[string]*refreshToken),
		revoked: make(map[string]time.Time),
	}
	if dataDir != "" {
		iss.file = filepath.Join(dataDir, "oauth.json")
		raw, err := os.ReadFile(iss.file)
		if err == nil {
			var st persistedState
			if err := json.Unmarshal(raw, &st); err != nil {
				return nil, fmt.Errorf("oauth: corrupt state file %s: %w", iss.file, err)
			}
			key, err := base64.StdEncoding.DecodeString(st.SigningKey)
			if err != nil || len(key) != 32 {
				return nil, fmt.Errorf("oauth: corrupt signing key in %s", iss.file)
			}
			iss.key = key
			for _, c := range st.Clients {
				iss.clients[c.ID] = c
			}
			for _, t := range st.RefreshTokens {
				if time.Now().Before(t.ExpiresAt) {
					iss.refresh[t.Hash] = t
				}
			}
			return iss, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("oauth: read state file %s: %w", iss.file, err)
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("oauth: random source: %w", err)
	}
	iss.key = key
	if iss.file != "" {
		if err := iss.saveLocked(); err != nil {
			return nil, err
		}
	}
	return iss, nil
}

// saveLocked writes the persisted state atomically (temp file +
// rename, 0600). The caller must hold iss.mu.
func (iss *Issuer) saveLocked() error {
	if iss.file == "" {
		return nil
	}
	st := persistedState{
		SigningKey: base64.StdEncoding.EncodeToString(iss.key),
	}
	for _, c := range iss.clients {
		st.Clients = append(st.Clients, c)
	}
	sort.Slice(st.Clients, func(i, j int) bool { return st.Clients[i].ID < st.Clients[j].ID })
	for _, t := range iss.refresh {
		st.RefreshTokens = append(st.RefreshTokens, t)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("oauth: marshal state: %w", err)
	}
	tmp := iss.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("oauth: write state: %w", err)
	}
	if err := os.Rename(tmp, iss.file); err != nil {
		return fmt.Errorf("oauth: persist state: %w", err)
	}
	return nil
}

// validRedirectURI enforces the redirect rule: an absolute URL;
// http only for loopback hosts, https everywhere else. A
// relative or non-http(s) URI can never be registered, so a code
// can never be delivered to an attacker-controlled scheme.
func validRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("oauth: redirect_uri %q must be an absolute http(s) URL", raw)
	}
	if u.Scheme == "http" {
		host := strings.ToLower(u.Hostname())
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			return fmt.Errorf("oauth: redirect_uri %q: plain http is allowed only for loopback hosts", raw)
		}
	}
	return nil
}

// RegisterClient creates an OAuth client. For a confidential
// client it returns the raw secret (shown once — the caller must
// return it to the operator and never log it); for a public
// (PKCE-only) client the secret is "".
func (iss *Issuer) RegisterClient(name string, redirectURIs, scopes []string, public bool) (*Client, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, "", errors.New("oauth: client name is required")
	}
	if len(redirectURIs) == 0 {
		return nil, "", errors.New("oauth: at least one redirect_uri is required")
	}
	for _, r := range redirectURIs {
		if err := validRedirectURI(r); err != nil {
			return nil, "", err
		}
	}
	scopes, err := validScopes(scopes)
	if err != nil {
		return nil, "", err
	}
	if len(scopes) == 0 {
		scopes = append([]string(nil), DefaultScopes...)
	}
	id, err := randomToken(18)
	if err != nil {
		return nil, "", err
	}
	client := &Client{
		ID:           "oc_" + id,
		Name:         name,
		RedirectURIs: append([]string(nil), redirectURIs...),
		Scopes:       scopes,
		Public:       public,
		CreatedAt:    time.Now(),
	}
	secret := ""
	if !public {
		secret, err = randomToken(32)
		if err != nil {
			return nil, "", err
		}
		client.SecretHash = sha256Hex(secret)
	}
	iss.mu.Lock()
	defer iss.mu.Unlock()
	iss.clients[client.ID] = client
	if err := iss.saveLocked(); err != nil {
		delete(iss.clients, client.ID)
		return nil, "", err
	}
	return client, secret, nil
}

// ListClients returns every registered client without secrets.
func (iss *Issuer) ListClients() []ClientView {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	out := make([]ClientView, 0, len(iss.clients))
	for _, c := range iss.clients {
		out = append(out, c.View())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DeleteClient removes a client and revokes its outstanding codes
// and refresh tokens. It reports whether the client existed.
func (iss *Issuer) DeleteClient(id string) bool {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	client, ok := iss.clients[id]
	if !ok {
		return false
	}
	// AUD-CODE-012 (same class, remaining site): deleting a client
	// revokes its tokens, so a persist failure must NOT silently claim
	// success — the tokens would come back after a restart. Capture what
	// is removed and put it back if the write fails.
	var delCodes []string
	for code, c := range iss.codes {
		if c.ClientID == id {
			delCodes = append(delCodes, code)
		}
	}
	var delRefresh []string
	for hash, t := range iss.refresh {
		if t.ClientID == id {
			delRefresh = append(delRefresh, hash)
		}
	}
	savedCodes := make(map[string]*authCode, len(delCodes))
	for _, code := range delCodes {
		savedCodes[code] = iss.codes[code]
		delete(iss.codes, code)
	}
	savedRefresh := make(map[string]*refreshToken, len(delRefresh))
	for _, hash := range delRefresh {
		savedRefresh[hash] = iss.refresh[hash]
		delete(iss.refresh, hash)
	}
	delete(iss.clients, id)
	if err := iss.saveLocked(); err != nil {
		for code, c := range savedCodes {
			iss.codes[code] = c
		}
		for hash, t := range savedRefresh {
			iss.refresh[hash] = t
		}
		iss.clients[id] = client
		persistLogf("oauth: persist client deletion failed, rolled back: %v", err)
		return false
	}
	return true
}

// Authorize validates an authorization request and issues a
// single-use code. An empty scope means the client's full
// registered scopes. The operator (approvedBy) is the gateway-key
// ID that approved the grant — recorded, never returned. The
// granted scopes must be a subset of operatorScopes: a key that
// cannot itself call admin routes cannot mint a token that can.
// A nil operatorScopes means the operator's scopes could not be
// determined, and the grant is refused.
func (iss *Issuer) Authorize(clientID, redirectURI string, scopes []string, challenge, method, approvedBy string, operatorScopes []string) (string, error) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	client, ok := iss.clients[clientID]
	if !ok {
		return "", errors.New("oauth: unknown client_id")
	}
	match := false
	for _, r := range client.RedirectURIs {
		if r == redirectURI {
			match = true
			break
		}
	}
	if !match {
		return "", errors.New("oauth: redirect_uri does not match the registered redirect_uris")
	}
	if method != "S256" {
		return "", errors.New("oauth: code_challenge_method must be S256")
	}
	if challenge == "" || len(challenge) < 43 || len(challenge) > 128 {
		return "", errors.New("oauth: code_challenge is required (43-128 characters)")
	}
	if len(scopes) == 0 {
		scopes = append([]string(nil), client.Scopes...)
	} else {
		var err error
		scopes, err = validScopes(scopes)
		if err != nil {
			return "", err
		}
		if !subsetOf(scopes, client.Scopes) {
			return "", errors.New("oauth: requested scope exceeds the client's registered scopes")
		}
	}
	if operatorScopes == nil {
		return "", errors.New("oauth: operator scopes unknown; authorize requires a gateway key with named scopes")
	}
	if !subsetOf(scopes, operatorScopes) {
		return "", errors.New("oauth: requested scope exceeds the operator key's scopes")
	}
	code, err := randomToken(32)
	if err != nil {
		return "", err
	}
	iss.codes[sha256Hex(code)] = &authCode{
		Hash:        sha256Hex(code),
		ClientID:    clientID,
		RedirectURI: redirectURI,
		Scopes:      scopes,
		Challenge:   challenge,
		Method:      method,
		ApprovedBy:  approvedBy,
		ExpiresAt:   time.Now().Add(CodeTTL),
	}
	return code, nil
}

// authenticateClient checks a confidential client's secret
// (constant-time hash compare). Public clients skip this step —
// PKCE is their authentication.
func (iss *Issuer) authenticateClient(client *Client, secret string) bool {
	if client.Public {
		return true
	}
	if secret == "" || client.SecretHash == "" {
		return false
	}
	return hmac.Equal([]byte(sha256Hex(secret)), []byte(client.SecretHash))
}

// ExchangeCode redeems a single-use authorization code for an
// access token + refresh token. The code is consumed whether the
// exchange succeeds or not — a code is never tried twice.
func (iss *Issuer) ExchangeCode(code, clientID, redirectURI, verifier, secret string) (*TokenResponse, error) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	hash := sha256Hex(code)
	ac, ok := iss.codes[hash]
	delete(iss.codes, hash)
	if !ok {
		return nil, errors.New("oauth: invalid or already-used code")
	}
	if time.Now().After(ac.ExpiresAt) {
		return nil, errors.New("oauth: code expired")
	}
	if ac.ClientID != clientID || ac.RedirectURI != redirectURI {
		return nil, errors.New("oauth: code does not match this client or redirect_uri")
	}
	if !VerifyCodeChallenge(ac.Challenge, ac.Method, verifier) {
		return nil, errors.New("oauth: code_verifier does not match the code_challenge")
	}
	client, ok := iss.clients[clientID]
	if !ok {
		return nil, errors.New("oauth: unknown client_id")
	}
	if !iss.authenticateClient(client, secret) {
		return nil, errors.New("oauth: invalid client credentials")
	}
	return iss.mintLocked(client, ac.Scopes)
}

// RefreshToken redeems a refresh token for a new access token and
// a ROTATED refresh token; the presented refresh token is
// consumed either way, so a stolen one cannot be replayed.
func (iss *Issuer) RefreshToken(refresh, clientID, secret string) (*TokenResponse, error) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	hash := sha256Hex(refresh)
	rt, ok := iss.refresh[hash]
	delete(iss.refresh, hash)
	if !ok {
		return nil, errors.New("oauth: invalid refresh_token")
	}
	if time.Now().After(rt.ExpiresAt) {
		if err := iss.saveLocked(); err != nil {
			// The token is expired and refused either way, but a failed
			// write must be visible (AUD-CODE-012 class).
			persistLogf("oauth: persist expired refresh-token removal failed: %v", err)
		}
		return nil, errors.New("oauth: refresh_token expired")
	}
	if rt.ClientID != clientID {
		return nil, errors.New("oauth: refresh_token does not belong to this client")
	}
	client, ok := iss.clients[clientID]
	if !ok {
		return nil, errors.New("oauth: unknown client_id")
	}
	if !iss.authenticateClient(client, secret) {
		return nil, errors.New("oauth: invalid client credentials")
	}
	resp, err := iss.mintLocked(client, rt.Scopes)
	if err != nil {
		// AUD-CODE-012 (same class, remaining site): the old token was
		// already consumed above but the rotation was not persisted, so
		// the client would be locked out with nothing to show for it.
		// Put it back and report the failure.
		iss.refresh[hash] = rt
		persistLogf("oauth: refresh token mint failed, consumed token restored: %v", err)
		return nil, err
	}
	if err := iss.saveLocked(); err != nil {
		// AUD-CODE-012 (same class, remaining site): the old token was
		// already consumed and a new one minted, but nothing was written.
		// Fail closed: put the consumed token back so the client can
		// retry, drop the un-persisted mint, and report the error instead
		// of handing out a credential a restart would invalidate.
		iss.refresh[hash] = rt
		persistLogf("oauth: persist refresh-token rotation failed: %v", err)
		return nil, fmt.Errorf("oauth: refresh token rotation could not be persisted: %w", err)
	}
	return resp, nil
}

// mintLocked mints an access JWT + refresh token for a client.
// The caller must hold iss.mu.
func (iss *Issuer) mintLocked(client *Client, scopes []string) (*TokenResponse, error) {
	now := time.Now()
	access, err := SignJWT(iss.key, client.ID, scopes, now)
	if err != nil {
		return nil, err
	}
	refresh, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	iss.refresh[sha256Hex(refresh)] = &refreshToken{
		Hash:      sha256Hex(refresh),
		ClientID:  client.ID,
		Scopes:    append([]string(nil), scopes...),
		ExpiresAt: now.Add(RefreshTokenTTL),
	}
	if err := iss.saveLocked(); err != nil {
		return nil, err
	}
	return &TokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int(AccessTokenTTL / time.Second),
		RefreshToken: refresh,
		Scope:        strings.Join(scopes, " "),
	}, nil
}

// persistLogf reports a revocation whose persist step failed but
// whose error has no return channel (Revoke returns only a bool).
// A package variable so tests can capture it; production defaults
// to the standard logger.
var persistLogf = log.Printf

// Revoke revokes a token: a refresh token is deleted, a JWT has
// its jti added to the denylist until its expiry. It reports
// whether anything was actually revoked (the RFC 7009 endpoint
// answers 200 either way). A refresh-token revocation is only
// reported as successful once it is durable: if the state file
// cannot be written the deletion is rolled back, false is
// returned and the error is logged, so a revocation that a restart
// would silently undo is never claimed as done.
func (iss *Issuer) Revoke(token string) bool {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	hash := sha256Hex(token)
	if rt, ok := iss.refresh[hash]; ok {
		delete(iss.refresh, hash)
		if err := iss.saveLocked(); err != nil {
			// The deletion did not reach disk. Fail closed and do NOT
			// leave memory claiming a revocation a restart would undo:
			// roll the deletion back, log the error, and return false
			// so the caller learns the revocation did not take effect.
			iss.refresh[hash] = rt
			persistLogf("oauth: persist refresh-token revocation failed: %v", err)
			return false
		}
		return true
	}
	claims, err := VerifyJWT(iss.key, token, time.Now())
	if err == nil {
		iss.revoked[claims.ID] = time.Unix(claims.Expires, 0)
		return true
	}
	return false
}

// VerifyAccessToken verifies a JWT access token and returns its
// claims. A revoked jti is rejected even before expiry.
func (iss *Issuer) VerifyAccessToken(token string) (*AccessClaims, error) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	claims, err := VerifyJWT(iss.key, token, time.Now())
	if err != nil {
		return nil, err
	}
	if exp, ok := iss.revoked[claims.ID]; ok {
		if time.Now().Before(exp) {
			return nil, ErrTokenInvalid
		}
		delete(iss.revoked, claims.ID)
	}
	return &AccessClaims{
		ClientID:  claims.ClientID,
		Scopes:    strings.Fields(claims.Scope),
		ExpiresAt: time.Unix(claims.Expires, 0),
		JTI:       claims.ID,
	}, nil
}

// TokenScopes returns the scopes a JWT access token carries, for
// the scope wrapper. It reports false for anything that is not a
// currently-valid gateway JWT.
func (iss *Issuer) TokenScopes(token string) ([]string, bool) {
	claims, err := iss.VerifyAccessToken(token)
	if err != nil {
		return nil, false
	}
	return claims.Scopes, true
}

// TokenExpired reports whether token is a signature-valid JWT
// whose expiry has passed, and the expiry time. It backs the
// 401 message ("oauth token expired on …").
func (iss *Issuer) TokenExpired(token string) (time.Time, bool) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	if _, err := VerifyJWT(iss.key, token, time.Now()); err == nil {
		return time.Time{}, false
	} else if !errors.Is(err, ErrTokenExpired) {
		return time.Time{}, false
	}
	exp, ok := TokenExpiry(token)
	if !ok {
		return time.Time{}, false
	}
	return exp, true
}
