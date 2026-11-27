package webexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/egress"
)

// HTTPSessionExecutor is the DEFAULT Executor implementation: it drives
// the provider's web UI at the HTTP level — replaying the stored
// session state's cookies (and localStorage-derived headers where a
// surface contract defines them) against the surface's chat endpoint.
// It never spins up a browser, never proxies traffic (no MITM), and
// never mutates what the operator handed over.
//
// Endpoint resolution, per surface:
//   - mock surfaces (mockContract=true, tests only): POST
//     {baseURL}/api/chat — the fixed mock-surface contract.
//   - documented live surfaces: POST {origin}{ChatEndpoint}.
//   - anything else: refuse with ErrEndpointUndocumented. No invented
//     URLs, ever.
type HTTPSessionExecutor struct {
	surface WebSurface
	store   *SessionStore

	// baseURL overrides the surface's WebOrigin — tests point it at an
	// httptest mock; production leaves it empty and uses the real origin.
	baseURL string
	// mockContract permits driving the mock-surface contract endpoint.
	// Production wiring never sets it: a live provider with no
	// documented endpoint is refused, not guessed.
	mockContract bool
	// client is the HTTP client. Production uses the SSRF-guarded
	// egress client (same rule as the OAuth token exchange — AUD-SEC-005);
	// tests inject a plain client for httptest loopback targets.
	client *http.Client
	// timeout bounds one execution.
	timeout time.Duration
	// disabled turns the whole executor path off: Execute refuses with
	// ErrExecutorDisabled and nothing silently falls back.
	disabled bool
}

// Option configures an executor (tests and the server bridge).
type Option func(*HTTPSessionExecutor)

// WithBaseURL points the executor at a specific origin instead of the
// surface's real WebOrigin (tests / self-hosted mock surfaces).
func WithBaseURL(u string) Option {
	return func(e *HTTPSessionExecutor) { e.baseURL = u }
}

// WithMockContract permits the mock-surface chat contract. Tests only.
func WithMockContract() Option {
	return func(e *HTTPSessionExecutor) { e.mockContract = true }
}

// WithHTTPClient overrides the HTTP client (tests, mock surfaces).
func WithHTTPClient(c *http.Client) Option {
	return func(e *HTTPSessionExecutor) { e.client = c }
}

// WithEnabled turns the executor path on/off. Off is LOUD: Execute
// returns ErrExecutorDisabled.
func WithEnabled(enabled bool) Option {
	return func(e *HTTPSessionExecutor) { e.disabled = !enabled }
}

func (e *HTTPSessionExecutor) httpClient() *http.Client {
	if e.client != nil {
		return e.client
	}
	// Production: the SSRF-guarded egress client, no redirects
	// followed (a redirect must not move the session cookies to a
	// different host).
	return egress.Default().Client(defaultExecutorTimeout, 0, nil)
}

// CatalogID implements Executor.
func (e *HTTPSessionExecutor) CatalogID() string { return e.surface.CatalogID }

// HasSession implements Executor.
func (e *HTTPSessionExecutor) HasSession() bool { return e.store.Has(e.surface.CatalogID) }

// Execute implements Executor. Order of refusals is deliberate: the
// disabled gate first (an off executor must never touch the network),
// then session availability, then endpoint honesty.
func (e *HTTPSessionExecutor) Execute(ctx context.Context, req ExecuteRequest) (*ExecuteResult, error) {
	if e.disabled {
		return nil, ErrExecutorDisabled
	}
	state, err := e.store.Load(e.surface.CatalogID)
	if err != nil {
		return nil, err // ErrNoSession / ErrStaleSession already classified
	}

	endpoint, err := e.endpoint()
	if err != nil {
		return nil, err
	}

	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("webexec: empty prompt")
	}
	model := req.Model
	if model == "" && len(e.surface.ModelIDs) > 0 {
		model = e.surface.ModelIDs[0]
	}

	body := map[string]any{
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"model":    model,
	}
	// Temporary chat (no history) is requested only where the surface
	// supports it; the mock contract carries the flag verbatim.
	temporary := req.TemporaryChat && e.surface.TemporaryChat
	if e.surface.TemporaryChat {
		body["temporary"] = temporary
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("webexec: build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("webexec: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Origin", originOf(endpoint))
	httpReq.Header.Set("Referer", originOf(endpoint)+"/")
	// Replay the operator's captured cookies for this origin, verbatim.
	if cookieHeader := cookieHeaderFor(state, e.baseURL); cookieHeader != "" {
		httpReq.Header.Set("Cookie", cookieHeader)
	}

	client := e.httpClient()
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("webexec: %s surface unreachable: %w", e.surface.CatalogID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// The stored state no longer authenticates. Loud 401
		// semantics; the operator must re-capture. Never retried.
		return nil, fmt.Errorf("webexec: %s rejected the stored session (HTTP %d): %w", e.surface.CatalogID, resp.StatusCode, ErrStaleSession)
	case resp.StatusCode == http.StatusTooManyRequests:
		// Quota: pass the upstream signal through with Retry-After
		// preserved, so account fallback/pacing sees the real thing.
		werr := &webExecError{err: ErrQuotaExceeded, statusCode: http.StatusTooManyRequests}
		if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
			werr.retryAfter = ra
			werr.hasRetry = true
		}
		werr.err = fmt.Errorf("webexec: %s quota exhausted (HTTP 429): %w", e.surface.CatalogID, ErrQuotaExceeded)
		return nil, werr
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("webexec: %s answered HTTP %d: %s", e.surface.CatalogID, resp.StatusCode, truncateBody(raw))
	}

	var out struct {
		Content string `json:"content"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("webexec: %s answered a non-JSON chat body: %w", e.surface.CatalogID, err)
	}
	return &ExecuteResult{
		Content:       out.Content,
		Surface:       e.surface.CatalogID,
		Model:         out.Model,
		StatusCode:    resp.StatusCode,
		TemporaryChat: temporary,
	}, nil
}

// endpoint resolves where Execute POSTs. Honest resolution only:
// mock contract in mock mode, documented endpoint on the real origin,
// refusal otherwise.
func (e *HTTPSessionExecutor) endpoint() (string, error) {
	if e.mockContract {
		base := e.baseURL
		if base == "" {
			base = e.surface.WebOrigin
		}
		return strings.TrimSuffix(base, "/") + mockContractChatPath, nil
	}
	if e.surface.EndpointDocumented && e.surface.ChatEndpoint != "" {
		return strings.TrimSuffix(e.surface.WebOrigin, "/") + e.surface.ChatEndpoint, nil
	}
	note := e.surface.EndpointNote
	if note == "" {
		note = "no documented endpoint recorded"
	}
	return "", fmt.Errorf("webexec: %s: %w: %s", e.surface.CatalogID, ErrEndpointUndocumented, note)
}

// cookieHeaderFor renders the stored cookies as a Cookie header. When
// baseURL is set (mock), all cookies replay; otherwise only cookies
// whose domain plausibly matches the target origin's host replay.
func cookieHeaderFor(state *SessionState, baseURL string) string {
	host := ""
	if baseURL != "" {
		if u, err := url.Parse(baseURL); err == nil {
			host = u.Hostname()
		}
	}
	parts := make([]string, 0, len(state.Cookies))
	for _, c := range state.Cookies {
		if host != "" && c.Domain != "" &&
			!strings.EqualFold(c.Domain, host) &&
			!(strings.HasPrefix(c.Domain, ".") && strings.HasSuffix(strings.TrimPrefix(c.Domain, "."), host)) {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// originOf extracts scheme://host from an endpoint URL.
func originOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// parseRetryAfter parses the Retry-After header (delta-seconds form;
// HTTP-date is ignored — web surfaces use seconds).
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// truncateBody bounds an error body like the provider clients do.
func truncateBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// New builds the executor for one catalog ID over its session store.
func New(catalogID string, store *SessionStore, opts ...Option) (Executor, error) {
	surface, err := Surface(catalogID)
	if err != nil {
		return nil, err
	}
	e := &HTTPSessionExecutor{
		surface: surface,
		store:   store,
		timeout: defaultExecutorTimeout,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}
