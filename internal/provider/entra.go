// Package provider — Microsoft Entra ID client-credentials token
// source for Azure OpenAI's /openai/v1 surface (catalog exchange kind
// "entra-id").
//
// The operator stores an Entra app registration's client credentials
// as the provider key, in JSON:
//
//	{
//	  "tenant_id":     "<directory tenant ID or domain>",
//	  "client_id":     "<application (client) ID>",
//	  "client_secret": "<client secret value>",
//	  "token_uri":     "optional override of the token endpoint",
//	  "scope":         "optional override of the requested scope"
//	}
//
// The client mints an OAuth2 access token with the client-credentials
// grant (RFC 6749 §4.4) at
// https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token for
// scope https://cognitiveservices.azure.com/.default, and sends it as
// Authorization: Bearer *** The token is cached in process memory
// until shortly before it expires, then re-minted — client-credentials
// grants issue NO refresh token, so expiry-aware re-minting IS the
// refresh behavior; nothing here pretends otherwise.
//
// The credential JSON itself is stored encrypted by the provider
// registry exactly like any API key (AES-256-GCM vault seal); the
// minted access token is never persisted, never logged, and never
// returned to a caller outside the auth resolution. The cache is
// keyed by a SHA-256 fingerprint of the credential identity — never
// by raw secret material — and is bounded like the Vertex token cache.
//
// token_uri / scope overrides exist for sovereign clouds and test
// mocks. A token_uri override must be https, or http only on a
// loopback host (the same trust model as the operator-supplied base
// URL: the operator points their own credential at their own
// endpoint). A scope override must be an https URL ending /.default.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/catalog"
)

// entraDefaultScope is the Azure AI / Cognitive Services resource
// scope Azure OpenAI's /openai/v1 surface accepts.
const entraDefaultScope = "https://cognitiveservices.azure.com/.default"

// entraCredentials is the parsed client-credential JSON the operator
// stored as the provider key.
type entraCredentials struct {
	TenantID     string `json:"tenant_id"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	TokenURI     string `json:"token_uri"`
	Scope        string `json:"scope"`
}

// errEntraNotApplicable marks "this resolution is not an Entra
// exchange, or the key is not Entra credential material" — the caller
// keeps the catalog's original exchange refusal in that case (the
// same semantics as sigv4Headers / vertexHeaders).
var errEntraNotApplicable = errors.New("entra: not applicable")

// parseEntraCredentials validates the stored key. Anything that is
// not a complete client-credential JSON is an error naming the
// problem, never a faked credential.
func parseEntraCredentials(key string) (*entraCredentials, error) {
	var c entraCredentials
	if err := json.Unmarshal([]byte(key), &c); err != nil {
		return nil, fmt.Errorf("entra: provider key is not a client-credential JSON: %v", err)
	}
	c.TenantID = strings.TrimSpace(c.TenantID)
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.ClientSecret = strings.TrimSpace(c.ClientSecret)
	if c.TenantID == "" {
		return nil, errors.New("entra: credential JSON has no tenant_id")
	}
	// The tenant becomes a URL path segment: refuse anything that
	// could escape it.
	if strings.ContainsAny(c.TenantID, "/\\?#% \t\r\n") || strings.Contains(c.TenantID, "..") {
		return nil, errors.New("entra: tenant_id must be a tenant GUID or domain (no path characters)")
	}
	if c.ClientID == "" {
		return nil, errors.New("entra: credential JSON has no client_id")
	}
	if c.ClientSecret == "" {
		return nil, errors.New("entra: credential JSON has no client_secret")
	}
	if c.TokenURI == "" {
		c.TokenURI = "https://login.microsoftonline.com/" + c.TenantID + "/oauth2/v2.0/token"
	} else {
		u, err := url.Parse(c.TokenURI)
		if err != nil || !u.IsAbs() || u.Host == "" {
			return nil, errors.New("entra: token_uri must be an absolute URL")
		}
		switch u.Scheme {
		case "https":
		case "http":
			h := u.Hostname()
			if h != "localhost" && h != "127.0.0.1" && h != "::1" {
				return nil, errors.New("entra: http token_uri is only allowed on loopback hosts; use https")
			}
		default:
			return nil, errors.New("entra: token_uri scheme must be http or https")
		}
	}
	if c.Scope == "" {
		c.Scope = entraDefaultScope
	} else if !strings.HasPrefix(c.Scope, "https://") || !strings.HasSuffix(c.Scope, "/.default") {
		return nil, errors.New("entra: scope override must be an https resource scope ending /.default")
	}
	return &c, nil
}

// entraKeyFingerprint identifies a client credential without carrying
// any of its material: SHA-256 over tenant, client_id, the SHA-256 of
// the secret, token_uri and scope. A rotated secret gets a new
// fingerprint. The fingerprint is a hash, never logged or persisted.
func entraKeyFingerprint(c *entraCredentials) string {
	secretHash := sha256.Sum256([]byte(c.ClientSecret))
	sum := sha256.Sum256([]byte(c.TenantID + "\x00" + c.ClientID + "\x00" + string(secretHash[:]) + "\x00" + c.TokenURI + "\x00" + c.Scope))
	return fmt.Sprintf("%x", sum[:])
}

// entraTokenCacheEntry is one shared minted token (same shape as the
// Vertex cache entry): mu serialises minting for this credential only.
type entraTokenCacheEntry struct {
	mu                sync.Mutex
	token             string
	expiry            time.Time
	created           time.Time
	mints             int64
	hits              int64
	mintFailures      int
	mintCooldownUntil time.Time
}

// Entra token-refresh breaker: after entraMintBreakerFailures
// consecutive mint failures, minting cools down for
// entraMintBreakerCooldown instead of hammering the token endpoint on
// every request. Vars so tests can shrink them.
var (
	entraMintBreakerFailures = 5
	entraMintBreakerCooldown = 30 * time.Minute
)

const (
	entraTokenCacheMaxEntries     = 1024
	entraTokenCacheTokenlessGrace = 5 * time.Minute
)

var (
	entraTokenCacheMu sync.Mutex
	entraTokenCache   = map[string]*entraTokenCacheEntry{}
)

// entraTokenCacheEntryFor returns the shared entry for a fingerprint,
// creating it on first use and bounding the map like the Vertex cache:
// past the cap, expired (and stale token-less) entries are swept
// first, then the evictable slot whose deadline is soonest is dropped.
func entraTokenCacheEntryFor(fingerprint string) *entraTokenCacheEntry {
	entraTokenCacheMu.Lock()
	defer entraTokenCacheMu.Unlock()
	if e, ok := entraTokenCache[fingerprint]; ok {
		return e
	}
	e := &entraTokenCacheEntry{created: time.Now()}
	entraTokenCache[fingerprint] = e
	if len(entraTokenCache) > entraTokenCacheMaxEntries {
		now := time.Now()
		for fp, oe := range entraTokenCache {
			oe.mu.Lock()
			expired := oe.token != "" && !now.Before(oe.expiry)
			stale := oe.token == "" && now.After(oe.created.Add(entraTokenCacheTokenlessGrace))
			oe.mu.Unlock()
			if expired || stale {
				delete(entraTokenCache, fp)
			}
		}
		for len(entraTokenCache) > entraTokenCacheMaxEntries {
			var oldestFP string
			var oldestDeadline time.Time
			for fp, oe := range entraTokenCache {
				if fp == fingerprint {
					continue
				}
				oe.mu.Lock()
				deadline, evictable := oe.expiry, oe.token != ""
				if !evictable {
					deadline = oe.created.Add(entraTokenCacheTokenlessGrace)
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
				break
			}
			delete(entraTokenCache, oldestFP)
		}
	}
	return e
}

// EntraTokenCacheStats reports the shared minted-token cache's honest
// totals since process start (mints, hits, live credential slots).
func EntraTokenCacheStats() (mints, hits int64, keys int) {
	entraTokenCacheMu.Lock()
	defer entraTokenCacheMu.Unlock()
	keys = len(entraTokenCache)
	for _, e := range entraTokenCache {
		e.mu.Lock()
		mints += e.mints
		hits += e.hits
		e.mu.Unlock()
	}
	return mints, hits, keys
}

// entraTokenResponse is the token endpoint's answer.
type entraTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// mintEntraToken performs the client-credentials grant. The returned
// expiry is the token's lifetime minus a 60 s skew, so a token is
// never used in its last minute.
func mintEntraToken(ctx context.Context, c *entraCredentials, hc *http.Client) (string, time.Time, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"scope":         {c.Scope},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURI, strings.NewReader(form))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("entra: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("entra: token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("entra: reading token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The error body can quote the request (including the
		// secret in a malformed-request echo): report the status
		// and the AAD error code only, never the raw body.
		code := ""
		var aadErr struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if json.Unmarshal(body, &aadErr) == nil && aadErr.Error != "" {
			code = " (" + aadErr.Error + ")"
		}
		return "", time.Time{}, fmt.Errorf("entra: token request rejected (HTTP %d)%s", resp.StatusCode, code)
	}
	var tok entraTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", time.Time{}, errors.New("entra: token response is not JSON")
	}
	if tok.AccessToken == "" {
		return "", time.Time{}, errors.New("entra: token response returned no access_token")
	}
	lifetime := time.Duration(tok.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	expiry := time.Now().Add(lifetime - time.Minute)
	if expiry.Before(time.Now().Add(time.Minute)) {
		expiry = time.Now().Add(time.Minute)
	}
	return tok.AccessToken, expiry, nil
}

// entraAccessToken returns a live access token, minting (and caching)
// one when the cached token is missing or expired. A mint failure is
// never cached; consecutive failures trip the breaker.
func entraAccessToken(ctx context.Context, c *entraCredentials, hc *http.Client) (string, error) {
	entry := entraTokenCacheEntryFor(entraKeyFingerprint(c))
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.token != "" && time.Now().Before(entry.expiry) {
		entry.hits++
		return entry.token, nil
	}
	if now := time.Now(); now.Before(entry.mintCooldownUntil) {
		return "", fmt.Errorf("entra: token minting cooling down after repeated failures (retry after %s)", entry.mintCooldownUntil.Format(time.RFC3339))
	}
	tok, expiry, err := mintEntraToken(ctx, c, hc)
	if err != nil {
		entry.mintFailures++
		if entry.mintFailures >= entraMintBreakerFailures {
			entry.mintCooldownUntil = time.Now().Add(entraMintBreakerCooldown)
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

// entraHTTPClient is the token-endpoint client (bounded timeout; the
// request context bounds it further).
func entraHTTPClient() *http.Client { return &http.Client{Timeout: 20 * time.Second} }

// entraIDHeaders is the registry hook for the Entra exchange, the
// third credential exchange the router performs itself (after Bedrock
// SigV4 and Vertex service-account JSON). When the resolution failed
// only because the surface needs an entra-id exchange and the key
// parses as Entra client credentials, it mints (or reuses) a token
// and returns the Bearer header set, the resolution's wire, and the
// token's expiry — the auth cache re-resolves once the expiry passes,
// so a long-lived provider never serves an expired token.
//
// errEntraNotApplicable means "not an Entra exchange / key is not
// Entra material": the caller keeps the catalog's exchange refusal.
// Any other error is a real mint failure and is surfaced honestly.
func entraIDHeaders(res catalog.AuthResolution, key string) (map[string]string, string, time.Time, error) {
	if res.Exchange == nil || res.Exchange.Kind != "entra-id" {
		return nil, "", time.Time{}, errEntraNotApplicable
	}
	c, err := parseEntraCredentials(key)
	if err != nil {
		return nil, "", time.Time{}, errEntraNotApplicable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tok, err := entraAccessToken(ctx, c, entraHTTPClient())
	if err != nil {
		return nil, "", time.Time{}, err
	}
	return map[string]string{"Authorization": "Bearer " + tok}, string(res.Wire), tokenExpiryFromCache(c), nil
}

// tokenExpiryFromCache reads the expiry the mint just stored (the
// cache entry is the source of truth; a hit returns the same expiry).
func tokenExpiryFromCache(c *entraCredentials) time.Time {
	entraTokenCacheMu.Lock()
	e, ok := entraTokenCache[entraKeyFingerprint(c)]
	entraTokenCacheMu.Unlock()
	if !ok {
		return time.Time{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.expiry
}
