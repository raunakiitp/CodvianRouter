// Package provider — Vertex AI native-wire client.
//
// vertexClient speaks Google Vertex AI's Gemini surface
// (POST {location}-aiplatform.googleapis.com/v1/projects/{project}/
// locations/{location}/publishers/google/models/{model}
// :generateContent, and :streamGenerateContent?alt=sse for
// streaming) for providers whose catalog wire format resolves to
// Vertex. The request/response shape is the Gemini generateContent
// shape, converted by internal/wire exactly as the AI Studio
// client does — but the auth, the base URL and the model path are
// Vertex's own, never AI Studio's:
//
//   - Auth is a minted OAuth2 access token, never a static header.
//     The provider key is the operator's service-account JSON
//     (type "service_account", client_email, private_key,
//     token_uri). The client mints an RS256 JWT (RFC 7523,
//     scope https://www.googleapis.com/auth/cloud-platform),
//     exchanges it at token_uri for an access token, and caches
//     the token until shortly before it expires. The cache is
//     shared across client instances for the same service-account
//     key (CHECKLIST 283 (a)): the provider-add key test mints
//     once and the first chat reuses that token instead of
//     minting again. The cache is keyed by a SHA-256 fingerprint
//     of the service-account identity (client_email, token_uri,
//     hash of the private key) — never by the raw key material,
//     and the fingerprint is never logged or persisted. A key that is
//     not a service-account JSON, a key with an unparseable
//     private key, or a failed exchange fails loudly at first
//     use — the client is never built with a faked static
//     Authorization header, and no request is ever sent
//     unauthenticated.
//   - The project and location come from the provider's base URL
//     (.../v1/projects/{project}/locations/{location}), set by
//     the operator at add time with a concrete base-URL override
//     (the catalog records the Vertex base templated). A
//     client-supplied vertex_location / vertex_project override
//     that the admin opted in to (config
//     AllowClientVertexOverride, ChatRequest.ClientVertexOverride)
//     replaces the location / project for that one request —
//     that is the routing effect the opt-in was waiting for —
//     and never anything else: no other request field moves the
//     request to another project.
//
// Error mapping reuses the Gemini mapping (Vertex errors are the
// same google.rpc.Status shape): a 429 / RESOURCE_EXHAUSTED
// becomes *RateLimitError, quota/billing failures become
// *QuotaError, other non-2xx become *UpstreamError.
package provider

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// vertexClient is the Client implementation for Vertex AI
// upstreams. It embeds *openAIClient for the shared HTTP plumbing
// (HTTP client, timeouts); auth is a minted OAuth2 token per
// request, not the embedded client's authHeaders — newRequest is
// deliberately NOT used here, because the token must be minted
// (and refreshed) per request, never stored as a static header.
type vertexClient struct {
	*openAIClient
}

// vertexTokenCacheEntry is one shared minted token: the live
// access token for one service-account key, its expiry, and the
// mint/hit counters the dashboard reports honestly. mu serialises
// minting for this key only, so two clients for the same key share
// one exchange while clients for different keys mint in parallel.
// mintFailures / mintCooldownUntil are the token-refresh breaker
// (M4.7b): after vertexMintBreakerFailures consecutive mint
// failures, minting cools down for vertexMintBreakerCooldown
// instead of hammering the token endpoint on every request.
type vertexTokenCacheEntry struct {
	mu     sync.Mutex
	token  string
	expiry time.Time
	// created bounds entries that never minted a token: a first
	// mint takes seconds, so an entry still token-less after the
	// grace period is stale (the mint failed or the client went
	// away) and its slot may be reclaimed.
	created time.Time
	mints   int64
	hits    int64
	// mintFailures counts consecutive mint failures since the
	// last success; mintCooldownUntil is set when the count
	// reaches vertexMintBreakerFailures.
	mintFailures      int
	mintCooldownUntil time.Time
}

// vertexMintBreakerFailures is how many consecutive mint failures
// trip the token-refresh breaker (5). vertexMintBreakerCooldown is
// how long minting stays cooled down after tripping (30 min).
// Vars, not consts, so tests can shrink them without waiting
// 30 minutes; nothing else in the codebase changes them.
var (
	vertexMintBreakerFailures = 5
	vertexMintBreakerCooldown = 30 * time.Minute
)

// vertexMintCoolingDownError is the fast failure accessToken
// returns while the token-refresh breaker is cooling down: the
// exchange is not even attempted.
type vertexMintCoolingDownError struct {
	Until time.Time
}

func (e *vertexMintCoolingDownError) Error() string {
	return fmt.Sprintf("vertex: token minting cooling down after repeated failures (retry after %s)", e.Until.Format(time.RFC3339))
}

// IsVertexMintCoolingDown reports whether err is a token-refresh
// breaker cooldown refusal.
func IsVertexMintCoolingDown(err error) bool {
	var e *vertexMintCoolingDownError
	return errors.As(err, &e)
}

// VertexMintCoolingDown returns how many service-account keys are
// currently inside a token-mint cooldown, for observability.
func VertexMintCoolingDown() int {
	now := time.Now()
	vertexTokenCacheMu.Lock()
	defer vertexTokenCacheMu.Unlock()
	n := 0
	for _, e := range vertexTokenCache {
		e.mu.Lock()
		cooling := now.Before(e.mintCooldownUntil)
		e.mu.Unlock()
		if cooling {
			n++
		}
	}
	return n
}

// vertexTokenCacheTokenlessGrace is how long an entry that never
// minted a token may hold a map slot. A mint exchange takes
// seconds, so five minutes is generous for an in-flight first
// mint; after that the slot is stale and may be reclaimed, so a
// key whose mint always fails cannot hold a slot forever.
const vertexTokenCacheTokenlessGrace = 5 * time.Minute

// vertexTokenCache is the process-wide minted-token cache, keyed
// by service-account fingerprint (see vertexKeyFingerprint). A
// token is re-minted when it expires, never evicted early — but
// the map slot IS reclaimed once the token is expired: the map is
// bounded (vertexTokenCacheMaxEntries) and inserting past the cap
// first sweeps expired entries, then drops the slot whose token
// expires soonest, so one entry per distinct SA key ever used
// cannot grow the map without bound (CHECKLIST 310). Entries
// that never minted yet (empty token) are reclaimed once stale
// (older than vertexTokenCacheTokenlessGrace); only a first mint
// still inside its grace period is never swept.
var (
	vertexTokenCacheMu sync.Mutex
	vertexTokenCache   = map[string]*vertexTokenCacheEntry{}
	// vertexTokenCacheMaxEntries bounds the shared minted-token
	// cache: at most this many distinct service-account keys hold
	// a map slot. A var (not a const) so tests can lower it
	// without minting a thousand tokens.
	vertexTokenCacheMaxEntries = 1024
)

// vertexKeyFingerprint identifies a service-account key without
// carrying any of its material: SHA-256 over client_email,
// token_uri and the SHA-256 of the private-key PEM. Two clients
// built from the same key JSON (same email, token URI and private
// key) get the same fingerprint; a rotated key gets a new one.
// The fingerprint is a hash, never logged or persisted — it only
// ever appears as this map's key.
func vertexKeyFingerprint(sa *vertexServiceAccount) string {
	keyHash := sha256.Sum256([]byte(sa.PrivateKey))
	sum := sha256.Sum256([]byte(sa.ClientEmail + "\x00" + sa.TokenURI + "\x00" + string(keyHash[:])))
	return fmt.Sprintf("%x", sum[:])
}

// vertexTokenCachePruneExpired deletes every entry whose minted
// token has expired (now at or after expiry), plus every entry
// that never minted a token and is past its grace period, and
// returns how many map slots it reclaimed. Entries inside their
// first-mint grace period are never pruned: a first mint may be
// in flight for that key. The caller must hold
// vertexTokenCacheMu; each entry's own mu is taken to read its
// token/expiry/created — the same lock order as
// VertexTokenCacheStats (global, then entry), so no inversion.
// A holder that already has the entry pointer keeps using it
// after the delete (it mints into the detached entry); the next
// lookup creates a fresh entry, so at most one extra mint can
// follow a prune, never a lost token.
func vertexTokenCachePruneExpired(now time.Time) int {
	pruned := 0
	for fp, e := range vertexTokenCache {
		e.mu.Lock()
		expired := e.token != "" && !now.Before(e.expiry)
		stale := e.token == "" && now.After(e.created.Add(vertexTokenCacheTokenlessGrace))
		e.mu.Unlock()
		if expired || stale {
			delete(vertexTokenCache, fp)
			pruned++
		}
	}
	return pruned
}

// vertexTokenCacheEntryFor returns the shared cache entry for a
// fingerprint, creating it on first use. Creating past the cap
// first sweeps expired (and stale token-less) entries; if the map
// is still over the cap, it drops the evictable slot whose
// deadline is soonest — a live token's expiry, or a stale
// token-less entry's grace deadline (never the fresh entry — it
// has no token yet — and never a first mint still inside its
// grace period), so the map never grows past the cap however many
// distinct keys are ever used. A slot dropped this way holds only
// a live token for a key nobody is using, or a stale failed mint;
// the next request for that key mints again.
func vertexTokenCacheEntryFor(fingerprint string) *vertexTokenCacheEntry {
	vertexTokenCacheMu.Lock()
	defer vertexTokenCacheMu.Unlock()
	if e, ok := vertexTokenCache[fingerprint]; ok {
		return e
	}
	e := &vertexTokenCacheEntry{created: time.Now()}
	vertexTokenCache[fingerprint] = e
	if len(vertexTokenCache) > vertexTokenCacheMaxEntries {
		now := time.Now()
		vertexTokenCachePruneExpired(now)
		for len(vertexTokenCache) > vertexTokenCacheMaxEntries {
			var oldestFP string
			var oldestDeadline time.Time
			for fp, oe := range vertexTokenCache {
				if fp == fingerprint {
					continue
				}
				oe.mu.Lock()
				deadline, evictable := oe.expiry, oe.token != ""
				if !evictable {
					// Token-less: evictable only once stale.
					deadline = oe.created.Add(vertexTokenCacheTokenlessGrace)
					evictable = now.After(deadline)
				}
				oe.mu.Unlock()
				if !evictable {
					continue
				}
				if oldestFP == "" || deadline.Before(oldestDeadline) {
					oldestFP, oldestDeadline = fp, deadline
				}
			}
			if oldestFP == "" {
				// Only live tokens and in-flight first mints
				// are left; the map stays over the cap rather
				// than dropping either.
				break
			}
			delete(vertexTokenCache, oldestFP)
		}
	}
	return e
}

// VertexTokenCacheStats reports the shared minted-token cache's
// honest totals since process start: how many token exchanges
// ran (mints), how many requests reused a cached token (hits),
// and how many distinct service-account keys hold a cache entry
// right now. Keys is the live slot count, not a process total:
// expired entries are reclaimed when the map grows past its cap
// (CHECKLIST 310), so a key nobody uses any more eventually
// leaves the count. Zero mints with zero Vertex providers is the
// honest empty state, not an error.
func VertexTokenCacheStats() (mints, hits int64, keys int) {
	vertexTokenCacheMu.Lock()
	defer vertexTokenCacheMu.Unlock()
	keys = len(vertexTokenCache)
	for _, e := range vertexTokenCache {
		e.mu.Lock()
		mints += e.mints
		hits += e.hits
		e.mu.Unlock()
	}
	return mints, hits, keys
}

// vertexServiceAccount is the parsed service-account JSON the
// operator stored as the provider key.
type vertexServiceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
	ProjectID   string `json:"project_id"`
	Type        string `json:"type"`
}

// vertexDefaultTokenURI is the Google OAuth2 token endpoint, used
// when the service-account JSON carries no token_uri.
const vertexDefaultTokenURI = "https://oauth2.googleapis.com/token"

// vertexTokenScope is the OAuth2 scope the minted token carries:
// full Vertex AI (Cloud Platform) access.
const vertexTokenScope = "https://www.googleapis.com/auth/cloud-platform"

// parseVertexServiceAccount parses the stored provider key into a
// service-account credential. Anything that is not a
// service-account JSON — not JSON at all, the wrong type, a
// missing client_email, an unparseable private key — is an error
// naming the problem, never a faked credential. The private key
// is validated here (PEM + PKCS#8 or PKCS#1 RSA) so a bad key
// fails at parse time, not as a mysterious 401 mid-request.
func parseVertexServiceAccount(key string) (*vertexServiceAccount, *rsa.PrivateKey, error) {
	var sa vertexServiceAccount
	if err := json.Unmarshal([]byte(key), &sa); err != nil {
		return nil, nil, fmt.Errorf("vertex: provider key is not a service-account JSON: %v", err)
	}
	if sa.Type != "" && sa.Type != "service-account" {
		return nil, nil, fmt.Errorf("vertex: provider key is a %q JSON credential, not a service-account JSON", sa.Type)
	}
	if strings.TrimSpace(sa.ClientEmail) == "" {
		return nil, nil, errors.New("vertex: service-account JSON has no client_email")
	}
	if strings.TrimSpace(sa.PrivateKey) == "" {
		return nil, nil, errors.New("vertex: service-account JSON has no private_key")
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, nil, errors.New("vertex: service-account private_key is not PEM-encoded")
	}
	var priv *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		priv, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, nil, errors.New("vertex: service-account private_key is not an RSA key")
		}
	} else if parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		priv = parsed
	} else {
		return nil, nil, fmt.Errorf("vertex: service-account private_key is not a parseable RSA key: %v", err)
	}
	if sa.TokenURI == "" {
		sa.TokenURI = vertexDefaultTokenURI
	}
	return &sa, priv, nil
}

// vertexJWT builds the RS256-signed JWT assertion (RFC 7523) for
// the token exchange: iss = client_email, scope =
// cloud-platform, aud = token_uri, 1h expiry.
func vertexJWT(sa *vertexServiceAccount, priv *rsa.PrivateKey, now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss":   sa.ClientEmail,
		"scope": vertexTokenScope,
		"aud":   sa.TokenURI,
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString
	unsigned := enc(header) + "." + enc(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("vertex: signing token-exchange JWT: %w", err)
	}
	return unsigned + "." + enc(sig), nil
}

// vertexTokenResponse is the token endpoint's answer.
type vertexTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// mintVertexToken exchanges the service-account JWT for an OAuth2
// access token at token_uri (grant type
// urn:ietf:params:oauth:grant-type:jwt-bearer). The returned
// expiry is the token's lifetime minus a 60 s skew, so a token
// is never used in its last minute. Any failure — bad JSON, bad
// key, exchange rejection — fails loudly naming the step.
func (c *vertexClient) mintVertexToken(ctx context.Context) (string, time.Time, error) {
	sa, priv, err := parseVertexServiceAccount(c.apiKey)
	if err != nil {
		return "", time.Time{}, err
	}
	assertion, err := vertexJWT(sa, priv, time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sa.TokenURI, strings.NewReader(form))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("vertex: building token-exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("vertex: token exchange failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("vertex: reading token-exchange response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("vertex: token exchange rejected (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tok vertexTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", time.Time{}, fmt.Errorf("vertex: token-exchange response is not JSON: %w", err)
	}
	if tok.AccessToken == "" {
		return "", time.Time{}, errors.New("vertex: token exchange returned no access_token")
	}
	lifetime := time.Duration(tok.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	// Never use a token in its last minute.
	expiry := time.Now().Add(lifetime - time.Minute)
	if expiry.Before(time.Now().Add(time.Minute)) {
		expiry = time.Now().Add(time.Minute)
	}
	return tok.AccessToken, expiry, nil
}

// accessToken returns a live OAuth2 access token, minting (and
// caching) one when the cached token is missing or expired. The
// cache is shared across client instances for the same
// service-account key (CHECKLIST 283 (a)): the provider-add key
// test and the first chat mint once between them, and concurrent
// requests for the same key share one exchange instead of each
// minting their own. A mint failure is never cached — the next
// request tries again.
func (c *vertexClient) accessToken(ctx context.Context) (string, error) {
	sa, _, err := parseVertexServiceAccount(c.apiKey)
	if err != nil {
		return "", err
	}
	entry := vertexTokenCacheEntryFor(vertexKeyFingerprint(sa))
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.token != "" && time.Now().Before(entry.expiry) {
		entry.hits++
		return entry.token, nil
	}
	// Token-refresh breaker (M4.7b): while minting is cooling
	// down after repeated failures, fail fast without attempting
	// the exchange — a revoked key or a dead token endpoint
	// must not be hammered once per request. A live cached token
	// above still serves during cooldown: only MINTING is gated.
	if now := time.Now(); now.Before(entry.mintCooldownUntil) {
		return "", &vertexMintCoolingDownError{Until: entry.mintCooldownUntil}
	}
	tok, expiry, err := c.mintVertexToken(ctx)
	if err != nil {
		// A mint failure is never cached — but it counts toward
		// the breaker: at vertexMintBreakerFailures consecutive
		// failures minting cools down for
		// vertexMintBreakerCooldown.
		entry.mintFailures++
		if entry.mintFailures >= vertexMintBreakerFailures {
			entry.mintCooldownUntil = time.Now().Add(vertexMintBreakerCooldown)
			entry.mintFailures = 0
		}
		return "", err
	}
	entry.token = tok
	entry.expiry = expiry
	entry.mints++
	entry.mintFailures = 0
	entry.mintCooldownUntil = time.Time{}
	return tok, nil
}

// vertexProjectLocation parses the provider base URL into its
// project and location: the base must be
// .../v1/projects/{project}/locations/{location} (a mock base in
// tests has the same shape on a loopback host). A base without
// the project/location path fails loudly naming the expected
// shape — the client cannot guess which project to bill.
func (c *vertexClient) vertexProjectLocation(req ChatRequest) (project, location string, err error) {
	u, err := url.Parse(strings.TrimRight(c.baseURL, "/"))
	if err != nil {
		return "", "", fmt.Errorf("vertex: base URL %q is not parseable: %w", c.baseURL, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// ["v1", "projects", "{project}", "locations", "{location}"]
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "projects" || parts[3] != "locations" ||
		parts[2] == "" || parts[4] == "" {
		return "", "", fmt.Errorf("vertex: base URL %q must be .../v1/projects/{project}/locations/{location} (the operator's concrete Vertex base, not the catalog's templated one)", c.baseURL)
	}
	project, location = parts[2], parts[4]
	// The opted-in client override (CHECKLIST 245/277): a request
	// carrying vertex_location / vertex_project — allowed only
	// when the admin opted in, enforced by the server before the
	// request reaches here — routes to that location / project
	// for this one request. Anything else about the request
	// leaves the operator's base URL untouched.
	if req.ClientVertexOverride != nil {
		switch req.ClientVertexOverride.Field {
		case "vertex_location", "vertex_ai_location":
			location = req.ClientVertexOverride.Value
		case "vertex_project", "vertex_ai_project":
			project = req.ClientVertexOverride.Value
		}
	}
	if project == "" || location == "" {
		return "", "", errors.New("vertex: project/location override is empty")
	}
	return project, location, nil
}

// vertexAPIRoot is the request's Vertex API root:
// {scheme}://{host}/v1/projects/{project}/locations/{location},
// with the operator's base host kept verbatim (a mock host in
// tests, the real {location}-aiplatform.googleapis.com host in
// production).
func (c *vertexClient) vertexAPIRoot(req ChatRequest) (string, error) {
	project, location, err := c.vertexProjectLocation(req)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(strings.TrimRight(c.baseURL, "/"))
	if err != nil {
		return "", fmt.Errorf("vertex: base URL %q is not parseable: %w", c.baseURL, err)
	}
	return u.Scheme + "://" + u.Host + "/v1/projects/" + project + "/locations/" + location, nil
}

// newVertexRequest builds an authenticated request for the Vertex
// API: a freshly minted (or cached) OAuth2 token as
// Authorization: Bearer <token>. No static header is ever sent:
// the token is minted from the service-account JSON at most once
// an hour, and a mint failure fails the request loudly instead
// of sending it unauthenticated.
func (c *vertexClient) newVertexRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

// generateURL is POST {root}/publishers/google/models/{model}:generateContent.
func (c *vertexClient) generateURL(req ChatRequest) (string, error) {
	root, err := c.vertexAPIRoot(req)
	if err != nil {
		return "", err
	}
	return root + "/publishers/google/models/" + geminiModelName(req.Model) + ":generateContent", nil
}

// streamURL is POST {root}/publishers/google/models/{model}:streamGenerateContent?alt=sse.
func (c *vertexClient) streamURL(req ChatRequest) (string, error) {
	root, err := c.vertexAPIRoot(req)
	if err != nil {
		return "", err
	}
	return root + "/publishers/google/models/" + geminiModelName(req.Model) + ":streamGenerateContent?alt=sse", nil
}

// modelsURL is GET {root}/publishers/google/models (Vertex
// ListModels for the publisher's models).
func (c *vertexClient) modelsURL() (string, error) {
	root, err := c.vertexAPIRoot(ChatRequest{})
	if err != nil {
		return "", err
	}
	return root + "/publishers/google/models", nil
}

func (c *vertexClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	gemReq, err := wire.ConvertGeminiRequest(toWireRequest(req))
	if err != nil {
		return nil, mapGeminiError(err)
	}
	// Thinking budget (CHECKLIST 303-304): Vertex uses the
	// Gemini API shape, so the operator-declared budget rides
	// the same generationConfig.thinkingConfig.thinkingBudget
	// field — applyGeminiThinkingBudget stamps it, and a zero
	// budget sends no thinkingConfig at all (no default
	// invented). The budget arrives on req.ThinkingBudget,
	// resolved by the engine from the table's "vertex" entry.
	applyGeminiThinkingBudget(gemReq, req)
	payload, err := json.Marshal(gemReq)
	if err != nil {
		return nil, fmt.Errorf("encoding vertex request: %w", err)
	}
	u, err := c.generateURL(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := c.newVertexRequest(ctx, http.MethodPost, u, payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	signals := adaptive.ParseRateLimitHeaders(resp.Header, time.Now())
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("reading vertex response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, mapGeminiError(wire.MapGeminiError(resp.StatusCode, resp.Header, body))
	}
	var decoded wire.GeminiResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed vertex response: " + err.Error()}
	}
	converted, err := wire.ConvertGeminiResponse(&decoded)
	if err != nil {
		return nil, mapGeminiError(err)
	}
	if converted.Model == "" {
		converted.Model = req.Model
	}
	out, err := fromWireResponse(converted)
	if err != nil {
		return nil, err
	}
	out.RateLimit = &signals
	return out, nil
}

// ChatStream is the text-only streaming entry point; see ChatStreamChunks.
func (c *vertexClient) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error) {
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	return c.ChatStreamChunks(ctx, req, func(chunk StreamChunk) error {
		if chunk.Content == "" {
			return nil
		}
		return onDelta(chunk.Content)
	})
}

// ChatStreamChunks performs a streaming generateContent request,
// translating the Vertex SSE stream into OpenAI-shaped chunks via
// wire.TranslateGeminiStream (the Vertex streaming shape is the
// Gemini SSE shape). The stream runs under a child context the
// client can cancel, exactly as the Bedrock client does
// (CHECKLIST 277 (a)): a consumer that stops consuming, or a
// cancelled parent context, drops the connection so billed
// generation stops upstream.
func (c *vertexClient) ChatStreamChunks(ctx context.Context, req ChatRequest, onChunk func(StreamChunk) error) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	if onChunk == nil {
		onChunk = func(StreamChunk) error { return nil }
	}
	gemReq, err := wire.ConvertGeminiRequest(toWireRequest(req))
	if err != nil {
		return nil, mapGeminiError(err)
	}
	// Thinking budget on the streaming path too (CHECKLIST
	// 303-304): same field as Chat, same zero-means-absent rule.
	applyGeminiThinkingBudget(gemReq, req)
	payload, err := json.Marshal(gemReq)
	if err != nil {
		return nil, fmt.Errorf("encoding vertex request: %w", err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	u, err := c.streamURL(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := c.newVertexRequest(streamCtx, http.MethodPost, u, payload)
	if err != nil {
		return nil, err
	}
	// No client-level timeout for streaming (context bounds it).
	streamClient := *c.httpClient
	streamClient.Timeout = 0
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	streamSignals := adaptive.ParseRateLimitHeaders(resp.Header, time.Now())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, mapGeminiError(wire.MapGeminiError(resp.StatusCode, resp.Header, body))
	}
	emit := func(wc wire.ChatChunk) error {
		if err := streamCtx.Err(); err != nil {
			return err
		}
		chunk := StreamChunk{}
		if len(wc.Choices) > 0 {
			d := wc.Choices[0].Delta
			chunk.Content = d.Content
			if wc.Choices[0].FinishReason != nil {
				chunk.FinishReason = *wc.Choices[0].FinishReason
			}
			for _, tc := range d.ToolCalls {
				chunk.ToolCalls = append(chunk.ToolCalls, ChunkToolCall{
					Index:     tc.Index,
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		}
		if wc.Usage != nil {
			chunk.Usage = &Usage{
				PromptTokens:     wc.Usage.PromptTokens,
				CompletionTokens: wc.Usage.CompletionTokens,
				TotalTokens:      wc.Usage.TotalTokens,
				CachedTokens:     wc.Usage.CachedTokens,
				ReasoningTokens:  wc.Usage.ReasoningTokens,
			}
		}
		if err := onChunk(chunk); err != nil {
			cancel()
			return err
		}
		return nil
	}
	res, err := wire.TranslateGeminiStream(resp.Body, emit)
	if err != nil {
		return nil, mapGeminiError(err)
	}
	if res.Model == "" {
		res.Model = req.Model
	}
	return &ChatResponse{
		Content:      res.Content,
		Usage:        Usage{PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens, TotalTokens: res.Usage.TotalTokens, CachedTokens: res.Usage.CachedTokens, ReasoningTokens: res.Usage.ReasoningTokens},
		Model:        res.Model,
		ToolCalls:    res.ToolCalls,
		FinishReason: res.FinishReason,
		Reasoning:    res.Reasoning,
		RateLimit:    &streamSignals,
	}, nil
}

// vertexModelsListResponse is the {"models": [{"name":
// "projects/.../publishers/google/models/gemini-..."}]} shape
// Vertex ListModels returns.
type vertexModelsListResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// ListModels returns the model IDs from GET
// {root}/publishers/google/models. The "…/publishers/google/models/"
// prefix is stripped so IDs match what Chat takes.
func (c *vertexClient) ListModels(ctx context.Context) ([]string, error) {
	u, err := c.modelsURL()
	if err != nil {
		return nil, err
	}
	httpReq, err := c.newVertexRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("reading vertex models response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, mapGeminiError(wire.MapGeminiError(resp.StatusCode, resp.Header, body))
	}
	var decoded vertexModelsListResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed vertex models response: " + err.Error()}
	}
	ids := make([]string, 0, len(decoded.Models))
	for _, m := range decoded.Models {
		if m.Name == "" {
			continue
		}
		id := m.Name
		if idx := strings.LastIndex(id, "/models/"); idx >= 0 {
			id = id[idx+len("/models/"):]
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// TestKey verifies the service-account credentials with a
// ListModels call (which mints a token first, so a bad key or a
// rejected exchange fails here, before any chat is routed).
func (c *vertexClient) TestKey(ctx context.Context) error {
	_, err := c.ListModels(ctx)
	return err
}

// Ensure vertexClient satisfies the client contracts at compile time.
var (
	_ Client        = (*vertexClient)(nil)
	_ ChunkStreamer = (*vertexClient)(nil)
)
