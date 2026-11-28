// Nadir decision strategy (M3 Area 1, PARITY row 396) —
// FAIL-CLOSED.
//
// the reference router's nadir strategy (open-sse/services/autoCombo/
// nadirStrategy.ts, studied, not copied) asks an external Nadir
// decision API which model the prompt needs, with a hardcoded
// default endpoint (https://api.getnadir.com) and fail-open
// behaviour: any error, timeout or missing configuration falls
// back to the rules strategy — and an unconfigured combo still
// calls the default endpoint, sending the user's prompt to a
// third party the operator never chose.
//
// This implementation is deliberately BETTER, and the divergence
// is intentional:
//   - There is NO default endpoint. The strategy requires an
//     explicitly configured decision endpoint
//     (Config.NadirEndpoint); when unconfigured it falls back
//     to priority order with a clear note and NEVER calls any
//     external API — no prompt ever leaves the box without the
//     operator's explicit choice.
//   - The decision call is bounded (default 2000 ms, max
//     30000 ms, one POST per request) and sends only the last
//     user turn (capped at 16000 chars) plus the pool's model
//     ids — no system prompt, no history, no tools.
//   - A failed call, a non-2xx status, a malformed response, or
//     a selected model outside the pool falls back to priority
//     order with a note naming the reason. The request is still
//     served; only the decision is refused, never the request.
//   - After such a failure the endpoint is cooled down for
//     30 s (the reference router's NADIR_FAILURE_COOLDOWN_MS, same number,
//     own code): requests in the window fall back to priority
//     WITHOUT another call, so an unreachable endpoint costs
//     one timeout instead of one per request. The cooldown is
//     per decision endpoint, held in the engine's existing
//     hard-cooldown map — no new engine state.
//   - The transport is injectable (Config.NadirTransport) so
//     tests can prove the unconfigured path makes zero external
//     calls.
//
// The router package has no logger; the fallback is recorded
// where routing decisions are recorded — the auto-decision
// (LastAutoDecision / attribution sink) and the resolution
// Notes — so the dashboard and traces show exactly why priority
// order applied instead of a Nadir decision.
//
// The durable decision record — why this is fail-CLOSED and why an
// external fail-open decisions API is refused — is
// docs/routing-decisions.md (D-A2-9b).
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Nadir decision-call constants (the reference router's documented values,
// same numbers, own code): default timeout 2000 ms, max 30000 ms,
// at most 100 menu items, prompt capped at 16000 chars.
const (
	// NadirDefaultTimeoutMs bounds one decision call when the
	// operator configures no timeout.
	NadirDefaultTimeoutMs = 2000
	// NadirMaxTimeoutMs caps the configured decision timeout.
	NadirMaxTimeoutMs = 30000
	// NadirMaxPromptChars caps the prompt text sent to the
	// decision endpoint.
	NadirMaxPromptChars = 16000
	// NadirMaxMenuItems caps the model ids sent in the menu.
	NadirMaxMenuItems = 100
	// NadirFailureCooldownMs is the default failure cooldown
	// in milliseconds (the reference router's NADIR_FAILURE_COOLDOWN_MS,
	// same number, own code): after a failed decision call
	// the endpoint is skipped for this long.
	NadirFailureCooldownMs = 30000
)

// nadirCooldownKey is the hard-cooldown-map key for one
// decision endpoint (per base URL, like the reference router's per-URL
// cooldown): the engine's existing cooldown map (e.cooldowns,
// guarded by e.mu) holds it, so no new engine state is needed.

func nadirCooldownKey(baseURL string) string {
	return "nadir:" + baseURL
}

// NadirTransport performs one decision POST: url is the full
// /v1/bucket URL, body is the JSON request, apiKey is the
// configured key ("" when none — no header is sent), timeout
// bounds the call. It returns the HTTP status and response body.
type NadirTransport func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (statusCode int, respBody []byte, err error)

// nadirHTTPTransport is the process-wide transport the default Nadir
// decision POST shares, so bursty decision calls to one operator
// endpoint reuse warm connections instead of re-dialing TCP+TLS once
// http.DefaultTransport's MaxIdleConnsPerHost=2 is exceeded
// (AUD-PERF-003). The Nadir endpoint is an operator-configured URL, not
// a catalog provider, so it cannot address the provider pool manager
// (Registry.PooledTransport); it is sized by the SAME Little's Law
// defaults the pools use — provider.PoolSizeForRate over the provider
// pool's expected λ × W — so the reuse behaviour matches a provider
// pool of the same shape.
var nadirHTTPTransport = func() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	size := provider.PoolSizeForRate(
		provider.DefaultPoolExpectedRPS,
		time.Duration(provider.DefaultPoolExpectedLatencyMs)*time.Millisecond,
		provider.DefaultPoolMinConns,
		provider.DefaultPoolMaxConns,
	)
	tr.MaxIdleConnsPerHost = size
	tr.MaxIdleConns = size * 4
	if tr.MaxIdleConns < 100 {
		tr.MaxIdleConns = 100
	}
	tr.IdleConnTimeout = 90 * time.Second
	return tr
}()

// defaultNadirTransport is the production NadirTransport: one
// POST with a client timeout, Content-Type application/json, and
// X-API-Key only when a key is configured. The response body is
// capped at 1 MiB — a decision response is small, and an
// unbounded read would let a hostile endpoint exhaust memory. The
// client uses the shared warm transport (nadirHTTPTransport) so a
// burst of decisions reuses connections (AUD-PERF-003).
func defaultNadirTransport(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
	client := &http.Client{Timeout: timeout, Transport: nadirHTTPTransport}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// normalizeNadirBaseURL validates an operator-supplied decision
// endpoint origin: trims whitespace and trailing slashes, strips
// a trailing /v1 (a pasted OpenAI-compatible base would otherwise
// double up to /v1/v1/bucket), and requires an absolute http(s)
// URL. It reports false for "" and for anything that is not one.
func normalizeNadirBaseURL(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	value = strings.TrimRight(value, "/")
	value = strings.TrimSuffix(value, "/v1")
	value = strings.TrimRight(value, "/")
	if value == "" {
		return "", false
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	return value, true
}

// nadirTimeout clamps the configured decision timeout: <= 0
// means the default, above the max means the max.
func (e *Engine) nadirTimeout() time.Duration {
	ms := e.cfg.NadirTimeoutMs
	if ms <= 0 {
		return NadirDefaultTimeoutMs * time.Millisecond
	}
	if ms > NadirMaxTimeoutMs {
		return NadirMaxTimeoutMs * time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}

// nadirCooldown returns the live failure-cooldown duration
// (G5): the operator-configured NadirCooldownMs, or the
// 30000 ms default (NadirFailureCooldownMs) when unset
// (<= 0). Unlike the timeout there is no upper clamp — an
// operator may cool a flaky endpoint down longer.
func (e *Engine) nadirCooldown() time.Duration {
	ms := e.cfg.NadirCooldownMs
	if ms <= 0 {
		return NadirFailureCooldownMs * time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}

// nadirFallback is the priority-order fallback for one nadir
// failure: the first catalog model leads, the rest follow in
// catalog order, and reason names why no decision was applied.
func nadirFallback(catalog []autorouter.CatalogModel, reason string) (model string, ordered []string, notes string) {
	ordered = make([]string, 0, len(catalog))
	for _, m := range catalog {
		ordered = append(ordered, m.Model)
	}
	if len(ordered) > 0 {
		model = ordered[0]
	}
	notes = fmt.Sprintf("nadir: %s; fell back to priority order (no decision applied)", reason)
	return model, ordered, notes
}

// nadirSelect chooses the serving model for an auto/nadir
// request over catalog, using the configured decision endpoint
// when one exists. It returns the model to serve, the ordered
// fallback pool (the selected model first, the rest in catalog
// order), and notes explaining the decision. It NEVER calls an
// external API without an explicitly configured endpoint.
func (e *Engine) nadirSelect(ctx context.Context, catalog []autorouter.CatalogModel, prompt string) (model string, ordered []string, notes string) {
	baseURL, ok := normalizeNadirBaseURL(e.cfg.NadirEndpoint)
	if !ok {
		if strings.TrimSpace(e.cfg.NadirEndpoint) == "" {
			return nadirFallback(catalog, "unconfigured: no decision endpoint set (fail-closed — no external call made)")
		}
		return nadirFallback(catalog, fmt.Sprintf("invalid decision endpoint %q", e.cfg.NadirEndpoint))
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nadirFallback(catalog, "request carries no user text")
	}
	if len(prompt) > NadirMaxPromptChars {
		prompt = prompt[:NadirMaxPromptChars]
	}
	// Failure cooldown: a failed decision call cools this
	// endpoint down, so requests in the window fall back to
	// priority WITHOUT another call (no call was made on the
	// paths above, so they never arm or consult the cooldown).
	key := nadirCooldownKey(baseURL)
	if e.onCooldown(key) {
		return nadirFallback(catalog, "decision endpoint in failure cooldown — skipped the decision call")
	}
	// fail records a decision failure: the endpoint is cooled
	// down for e.nadirCooldown() (the configured window,
	// 30000 ms by default) and the request falls back to
	// priority order. Every post-call failure arms it — a
	// transport error, a non-2xx status, a malformed response,
	// or an unusable selection all mean the endpoint cannot
	// decide right now, and retrying it on every request
	// would cost a full timeout each time.
	fail := func(reason string) (string, []string, string) {
		e.setCooldown(key, e.nadirCooldown())
		return nadirFallback(catalog, reason)
	}
	menu := make([]string, 0, len(catalog))
	seen := make(map[string]struct{}, len(catalog))
	for _, m := range catalog {
		if len(menu) >= NadirMaxMenuItems {
			break
		}
		if _, dup := seen[m.Model]; dup {
			continue
		}
		seen[m.Model] = struct{}{}
		menu = append(menu, m.Model)
	}
	body, err := json.Marshal(map[string]any{
		"prompt": prompt,
		"menu":   menu,
		"source": "codvian-router",
	})
	if err != nil {
		return nadirFallback(catalog, fmt.Sprintf("decision request could not be encoded: %v", err))
	}
	transport := e.cfg.NadirTransport
	if transport == nil {
		transport = defaultNadirTransport
	}
	timeout := e.nadirTimeout()
	status, respBody, err := transport(ctx, baseURL+"/v1/bucket", body, e.cfg.NadirAPIKey, timeout)
	if err != nil {
		return fail(fmt.Sprintf("decision call failed: %v", err))
	}
	if status < 200 || status >= 300 {
		return fail(fmt.Sprintf("decision endpoint returned HTTP %d", status))
	}
	var data struct {
		SelectedModel string  `json:"selected_model"`
		Bucket        string  `json:"bucket"`
		Confidence    float64 `json:"confidence"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return fail(fmt.Sprintf("decision response malformed: %v", err))
	}
	selected := strings.TrimSpace(data.SelectedModel)
	if selected == "" {
		return fail("decision response carries no selected_model")
	}
	if _, ok := seen[selected]; !ok {
		return fail(fmt.Sprintf("decision selected model %q, which is not in the pool", selected))
	}
	ordered = []string{selected}
	for _, m := range menu {
		if m != selected {
			ordered = append(ordered, m)
		}
	}
	notes = fmt.Sprintf("nadir selected %s (bucket=%s confidence=%.2f); connection choice by fallback order",
		selected, strings.TrimSpace(data.Bucket), data.Confidence)
	return selected, ordered, notes
}
