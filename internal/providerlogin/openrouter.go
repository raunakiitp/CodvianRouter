// Package providerlogin implements provider-documented account login
// flows that produce a credential the provider registry can store and
// test live. Phase 1 ships one flow: OpenRouter OAuth PKCE.
//
// The official OpenRouter flow (https://openrouter.ai/docs/guides/overview/auth/oauth)
// is PKCE-only and returns a USER-CONTROLLED API KEY, not an OAuth
// access/refresh token pair:
//
//  1. The app generates a random code_verifier, derives the S256
//     code_challenge, and sends the user to {base}/auth with the
//     challenge and a callback_url (or no callback_url at all for
//     headless apps — the code is then displayed on screen).
//  2. The user signs in to OpenRouter and authorizes; OpenRouter
//     redirects back with ?code=...&state=... (headless: the user
//     copies the displayed code).
//  3. The app POSTs {code, code_verifier, code_challenge_method} to
//     {base}/api/v1/auth/keys and receives {key}.
//
// The returned key is an ordinary OpenRouter API key: it is stored
// through the provider registry's encrypted key pool, tested live
// against the upstream before it is persisted, and routed exactly
// like a pasted key. There is NO refresh token in this flow — the
// key lives until the user revokes it or its chosen expiry passes,
// so "refresh" here means the operator re-runs the login (or rotates
// the key), never a silent token renewal this package could fake.
//
// Security properties:
//   - The code_verifier NEVER leaves this package: it is not in the
//     authorization URL, not in any response, not in any error, not
//     logged. Only the challenge (a SHA-256 hash) travels to OpenRouter.
//   - Every flow carries a fresh 256-bit state, returned unchanged by
//     OpenRouter, matched server-side and consumed exactly once.
//   - Pending flows expire after 10 minutes (OpenRouter's own code
//     lifetime) and the pending map is bounded.
//   - The exchanged API key is returned to the caller once and never
//     logged or persisted by this package.
package providerlogin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultOpenRouterAuthBase is OpenRouter's production auth host.
const DefaultOpenRouterAuthBase = "https://openrouter.ai"

// OpenRouterPendingTTL is how long a started login flow stays
// redeemable. OpenRouter's authorization codes are single-use and
// expire after 10 minutes; the pending flow matches that lifetime.
const OpenRouterPendingTTL = 10 * time.Minute

// openRouterMaxPending bounds the pending-flow map so abandoned
// logins cannot grow it without limit.
const openRouterMaxPending = 64

// Errors returned by Exchange. Callers map them to honest HTTP
// statuses; none of them carries key material or the verifier.
var (
	// ErrUnknownState means the state was never issued, already
	// consumed, or belongs to a different flow instance.
	ErrUnknownState = errors.New("providerlogin: unknown or already-used login state")
	// ErrLoginExpired means the flow started more than
	// OpenRouterPendingTTL ago.
	ErrLoginExpired = errors.New("providerlogin: login flow expired (authorization codes live 10 minutes)")
)

// ExchangeError is a non-2xx answer from the key-exchange endpoint.
// The body is deliberately not echoed: it can quote request fields.
type ExchangeError struct {
	Status int
}

func (e *ExchangeError) Error() string {
	if e.Status == http.StatusForbidden {
		return "openrouter: key exchange rejected (HTTP 403 — invalid or expired code, or wrong code_verifier)"
	}
	return fmt.Sprintf("openrouter: key exchange rejected (HTTP %d)", e.Status)
}

// CodeChallenge derives the RFC 7636 S256 code_challenge for a
// verifier: BASE64URL(SHA256(verifier)), no padding. Exported for
// the RFC 7636 appendix-B test vector and for callers that build
// their own authorization URL from a verifier they hold.
func CodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// OpenRouterStartOptions are the optional parameters of the
// authorization URL. Empty fields are omitted (OpenRouter's defaults
// apply: no credit limit, no expiry).
type OpenRouterStartOptions struct {
	// CallbackURL is where OpenRouter redirects after authorization.
	// Empty means HEADLESS: the code is displayed on screen and the
	// user pastes it back (SSH sessions, containers, remote servers).
	// http is allowed only for loopback callbacks; hosted callbacks
	// must be https (OpenRouter's own guidance).
	CallbackURL string
	// KeyLabel prefills the label of the API key OpenRouter creates.
	KeyLabel string
	// Limit prefills the key's credit limit in USD: a positive amount
	// below 1,000,000 with at most two decimal places ("25", "12.50").
	Limit string
	// Expiration prefills the key's expiry: one of OpenRouter's
	// documented values (see openRouterExpirations).
	Expiration string
}

// openRouterExpirations is the documented expiration enum.
var openRouterExpirations = map[string]bool{
	"1-hour": true, "1-day": true, "7-days": true, "30-days": true,
	"90-days": true, "180-days": true, "1-year": true, "never": true,
}

// OpenRouterStarted is the public half of a started flow: everything
// the operator needs to authorize, and nothing secret. The verifier
// stays inside the flow.
type OpenRouterStarted struct {
	AuthorizationURL string    `json:"authorization_url"`
	State            string    `json:"state"`
	ExpiresAt        time.Time `json:"expires_at"`
	Headless         bool      `json:"headless"`
}

// OpenRouterFlow drives the PKCE login. The zero value is usable
// (production defaults); tests inject AuthBase, Client, Now and Rand.
// A flow is safe for concurrent use.
type OpenRouterFlow struct {
	// AuthBase overrides the OpenRouter host (tests point it at a
	// local mock). Empty means DefaultOpenRouterAuthBase.
	AuthBase string
	// Client is the HTTP client for the key exchange. Nil means a
	// 30-second-timeout client.
	Client *http.Client
	// Now overrides the clock (tests). Nil means time.Now.
	Now func() time.Time
	// Rand overrides the randomness source (tests). Nil means
	// crypto/rand.Reader.
	Rand io.Reader

	mu      sync.Mutex
	pending map[string]*openRouterPending
}

type openRouterPending struct {
	verifier string
	created  time.Time
	headless bool
}

// NewOpenRouterFlow returns a flow with production defaults.
func NewOpenRouterFlow() *OpenRouterFlow { return &OpenRouterFlow{} }

func (f *OpenRouterFlow) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *OpenRouterFlow) base() string {
	if strings.TrimSpace(f.AuthBase) != "" {
		return strings.TrimRight(strings.TrimSpace(f.AuthBase), "/")
	}
	return DefaultOpenRouterAuthBase
}

func (f *OpenRouterFlow) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (f *OpenRouterFlow) randomB64(n int) (string, error) {
	r := f.Rand
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("providerlogin: generating random material: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// validateCallbackURL enforces OpenRouter's callback rules: an
// absolute http(s) URL; plain http only for loopback hosts.
func validateCallbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return errors.New("providerlogin: callback_url must be an absolute http(s) URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
		return errors.New("providerlogin: http callback_url is only allowed for loopback hosts; use https")
	default:
		return errors.New("providerlogin: callback_url scheme must be http or https")
	}
}

// validateLimit enforces the documented credit-limit shape.
func validateLimit(limit string) error {
	if limit == "" {
		return nil
	}
	// Positive decimal, < 1000000, at most two decimal places.
	dollars, cents, hasCents := strings.Cut(limit, ".")
	if hasCents && (len(cents) == 0 || len(cents) > 2) {
		return errors.New("providerlogin: limit must have at most two decimal places")
	}
	for _, part := range []string{dollars, cents} {
		for _, c := range part {
			if c < '0' || c > '9' {
				return errors.New("providerlogin: limit must be a positive decimal amount")
			}
		}
	}
	if dollars == "" {
		dollars = "0"
	}
	if len(dollars) > 6 {
		return errors.New("providerlogin: limit must be below 1000000")
	}
	// Numeric bound check without float rounding surprises.
	whole := 0
	for _, c := range dollars {
		whole = whole*10 + int(c-'0')
	}
	frac := 0
	for _, c := range cents {
		frac = frac*10 + int(c-'0')
	}
	if whole > 999999 || (whole == 0 && frac == 0) {
		return errors.New("providerlogin: limit must be a positive amount below 1000000")
	}
	return nil
}

// Start begins a login flow: it generates the verifier/challenge and
// state, records the pending flow, and returns the authorization URL
// the operator opens in a browser. The verifier never leaves the flow.
func (f *OpenRouterFlow) Start(opts OpenRouterStartOptions) (OpenRouterStarted, error) {
	if opts.CallbackURL != "" {
		if err := validateCallbackURL(opts.CallbackURL); err != nil {
			return OpenRouterStarted{}, err
		}
	}
	if err := validateLimit(opts.Limit); err != nil {
		return OpenRouterStarted{}, err
	}
	if opts.Expiration != "" && !openRouterExpirations[opts.Expiration] {
		return OpenRouterStarted{}, fmt.Errorf("providerlogin: expiration must be one of 1-hour, 1-day, 7-days, 30-days, 90-days, 180-days, 1-year, never")
	}
	verifier, err := f.randomB64(32)
	if err != nil {
		return OpenRouterStarted{}, err
	}
	state, err := f.randomB64(32)
	if err != nil {
		return OpenRouterStarted{}, err
	}
	q := url.Values{}
	if opts.CallbackURL != "" {
		q.Set("callback_url", opts.CallbackURL)
	}
	q.Set("code_challenge", CodeChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	if opts.KeyLabel != "" {
		q.Set("key_label", opts.KeyLabel)
	}
	if opts.Limit != "" {
		q.Set("limit", opts.Limit)
	}
	if opts.Expiration != "" {
		q.Set("expiration", opts.Expiration)
	}
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending == nil {
		f.pending = make(map[string]*openRouterPending)
	}
	// Prune expired flows first; a full map of live flows is an
	// honest refusal, never an unbounded grow.
	for s, p := range f.pending {
		if now.Sub(p.created) >= OpenRouterPendingTTL {
			delete(f.pending, s)
		}
	}
	if len(f.pending) >= openRouterMaxPending {
		return OpenRouterStarted{}, errors.New("providerlogin: too many pending login flows; finish or wait for one to expire")
	}
	f.pending[state] = &openRouterPending{verifier: verifier, created: now, headless: opts.CallbackURL == ""}
	return OpenRouterStarted{
		AuthorizationURL: f.base() + "/auth?" + q.Encode(),
		State:            state,
		ExpiresAt:        now.Add(OpenRouterPendingTTL),
		Headless:         opts.CallbackURL == "",
	}, nil
}

// openRouterExchangeRequest is the POST body of the key exchange.
type openRouterExchangeRequest struct {
	Code                string `json:"code"`
	CodeVerifier        string `json:"code_verifier"`
	CodeChallengeMethod string `json:"code_challenge_method"`
}

// openRouterExchangeResponse is the exchange answer: the
// user-controlled API key.
type openRouterExchangeResponse struct {
	Key string `json:"key"`
}

// Exchange redeems an authorization code for the user's OpenRouter
// API key. The state must match a live pending flow; the flow is
// consumed exactly once (the code itself is single-use upstream, so
// a failed exchange means the operator starts a new flow). The
// returned key is secret: the caller stores it through the encrypted
// provider registry and never logs it.
func (f *OpenRouterFlow) Exchange(ctx context.Context, state, code string) (string, error) {
	state = strings.TrimSpace(state)
	code = strings.TrimSpace(code)
	if state == "" || code == "" {
		return "", errors.New("providerlogin: state and code are required")
	}
	f.mu.Lock()
	p, ok := f.pending[state]
	if ok {
		delete(f.pending, state) // single-use, even on failure
	}
	f.mu.Unlock()
	if !ok {
		return "", ErrUnknownState
	}
	if f.now().Sub(p.created) >= OpenRouterPendingTTL {
		return "", ErrLoginExpired
	}
	body, err := json.Marshal(openRouterExchangeRequest{
		Code:                code,
		CodeVerifier:        p.verifier,
		CodeChallengeMethod: "S256",
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.base()+"/api/v1/auth/keys", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("providerlogin: building key-exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := f.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("providerlogin: key exchange failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("providerlogin: reading key-exchange response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &ExchangeError{Status: resp.StatusCode}
	}
	var out openRouterExchangeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", errors.New("providerlogin: key-exchange response is not JSON")
	}
	if strings.TrimSpace(out.Key) == "" {
		return "", errors.New("providerlogin: key exchange returned no key")
	}
	return out.Key, nil
}
