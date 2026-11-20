// Media verify-first passthrough (M8.20).
//
// MediaPassthrough routes one media request through the engine —
// chain resolution, strategy ordering, quota/breaker/cooldown
// filtering, the per-request budget gate, the adaptive limiter,
// ledger and trace — but with exactly ONE upstream attempt, ever.
// That is the replay exemption (ReplayExemption, preread.go),
// reused from embeddings via singleAttempt: a video/music
// generation starts a paid job upstream, and a search/OCR call is
// single-shot work; replaying any of them onto another target
// after a failure would double-apply upstream work, so the first
// target actually dialled owns the outcome — success or failure —
// and the request ends there. What does NOT count as an attempt: a
// target skipped before dialling (zero quota, breaker open,
// cooling, limiter shed, or no media-passthrough capability on
// that client) — no upstream call was made, so moving to the next
// target is selection, not a replay.
//
// The chain is PROVIDER-PINNED, not model-resolved: each media
// surface has exactly one verified provider (search → perplexity,
// OCR → mistral, video → runwayml, music → minimax), so the chain
// is every registered client key for that provider id, in sorted
// key order. Failover across providers is meaningless here — each
// upstream speaks a different native shape — but failover across
// KEYS of the same provider is real, and the pinned chain keeps
// it. A model that names another provider can never be smuggled
// in: the handler pins the provider, and the chain only ever
// contains it.
//
// The bookkeeping request is a synthetic ChatRequest carrying the
// bookkeeping text as user messages, the endpoint kind, the caller
// ID and the per-request budget. It is never sent upstream; it
// exists so the shared machinery — reconcileUsage (local-estimate
// fallback when the upstream reports no usage), budgetGate (the
// strict per-request budget) and recordAttempt (ledger rows) —
// works unchanged for these endpoints. Media responses carry no
// token usage, so the ledger row records the attempt with a zero
// usage block on the endpoint (the same shape rerank records when
// the upstream reports no usage): tokens/cost stay unknown, never
// an invented number. The local estimate may fill the prompt-token
// count from the bookkeeping text, flagged LocalEstimate — a token
// count, never a price: none of these models has a price-book
// entry, so cost stays unknown.
//
// The four new endpoint kinds are known to endpointForProjection
// (internal/router/cascade.go, M8.20 mapping): they project onto
// the autorouter's already-defined search / ocr / videos / music
// surfaces, so the default "priority" strategy — a NonChatStrategy
// — stands on a media call. A combo strategy outside
// NonChatStrategies still fails the call closed, exactly like
// embeddings/rerank.
package router

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// supportsMediaPassthrough reports whether client can serve a media
// passthrough call: it implements provider.MediaPassthroughClient.
// No wire-format check (unlike embeddings): the chain is
// provider-pinned to a single OpenAI-wire provider, and the path is
// provider-native, so any client implementing the method can serve
// it. A native-wire client that somehow held the pinned id would
// POST its native base + the media path and fail honestly on the
// upstream 404, exactly once.
func supportsMediaPassthrough(client provider.Client) bool {
	_, ok := client.(provider.MediaPassthroughClient)
	return ok
}

// chainForProvider builds the fallback chain for one media call:
// every registered client key for providerID, in sorted key order,
// each carrying model. The provider id is the pin — no other
// provider can enter the chain.
func (e *Engine) chainForProvider(providerID, model string) []Attempt {
	e.clientsMu.RLock()
	defer e.clientsMu.RUnlock()
	keys := make([]string, 0)
	for key := range e.clients {
		id, _ := SplitClientKey(key)
		if id == providerID {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	chain := make([]Attempt, 0, len(keys))
	for _, key := range keys {
		_, keyID := SplitClientKey(key)
		chain = append(chain, Attempt{ProviderID: providerID, KeyID: keyID, Model: model})
	}
	return chain
}

// reqWithModelMedia returns req addressed to att's model (the chain
// resolves the model per attempt; the request itself names the
// model the caller asked for — or the provider id for the
// model-less surfaces).
func reqWithModelMedia(req provider.MediaPassthroughRequest, att Attempt) provider.MediaPassthroughRequest {
	req.Model = att.Model
	return req
}

// callMedia makes the single upstream media call for one attempt,
// mirroring callEmbed: adaptive-limiter admission, panic isolation
// (a panicking client fails this attempt, never the gateway), and a
// synthetic ChatResponse carrying a zero usage block on the
// endpoint so noteSuccess can record the attempt. The response
// itself is returned separately — the caller needs the upstream
// status + body verbatim, not the synthetic chat shape.
func (e *Engine) callMedia(ctx context.Context, att Attempt, req provider.MediaPassthroughRequest) (out *provider.MediaPassthroughResponse, res attemptResult) {
	client, ok := e.clientForAttempt(att)
	if !ok {
		return nil, attemptResult{att: att, err: fmt.Errorf("router: unknown provider %q", att.ProviderID)}
	}
	mp, ok := client.(provider.MediaPassthroughClient)
	if !ok {
		return nil, attemptResult{att: att, err: fmt.Errorf("router: provider %q does not support media passthrough", att.ProviderID)}
	}
	if !e.limiter.TryAcquire(att.ProviderID) {
		e.noteConcurrencyRejected(att.ProviderID)
		return nil, attemptResult{att: att, err: adaptive.ErrLimited}
	}
	defer e.limiter.Release(att.ProviderID)
	start := time.Now()
	defer func() {
		if v := recover(); v != nil {
			e.notePanic("provider_media")
			out = nil
			res = attemptResult{att: att, err: NewPanicError("provider_media", v), latencyMs: time.Since(start).Milliseconds(), called: true}
		}
	}()
	resp, err := mp.MediaPassthrough(ctx, reqWithModelMedia(req, att))
	if err != nil {
		return nil, attemptResult{att: att, err: err, latencyMs: time.Since(start).Milliseconds(), called: true}
	}
	if resp == nil {
		return nil, attemptResult{att: att, err: errors.New("router: media provider returned no response"), latencyMs: time.Since(start).Milliseconds(), called: true}
	}
	usage := provider.Usage{Endpoint: req.Endpoint}
	return resp, attemptResult{
		att:       att,
		resp:      &provider.ChatResponse{Usage: usage, Model: att.Model},
		latencyMs: time.Since(start).Milliseconds(),
		called:    true,
	}
}

// MediaPassthrough routes one media request: exactly one upstream
// attempt (see the package doc). The chain is provider-pinned to
// req.ProviderID; the model is only the routing / ledger label.
func (e *Engine) MediaPassthrough(ctx context.Context, req provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	bookReq := bookkeepingRequest(req.Model, req.BookkeepingTexts, req.Endpoint, req.CallerID, req.BudgetUSD, req.BudgetStrict)
	chain := e.chainForProvider(req.ProviderID, req.Model)
	// No registered client for the pinned provider: that is a
	// property of the request + the operator's configuration, not
	// an upstream failure — the typed error answers an honest 400
	// at the server, never a 502, and no upstream-failure counter
	// moves. (singleAttempt would otherwise report the empty chain
	// as a plain error.)
	if len(chain) == 0 {
		return nil, &NoCapableProviderError{Endpoint: req.Endpoint, Model: req.Model}
	}
	var out *provider.MediaPassthroughResponse
	_, _, err := e.singleAttempt(ctx, chain, req.Endpoint, bookReq, supportsMediaPassthrough,
		func(ctx context.Context, att Attempt) attemptResult {
			var res attemptResult
			out, res = e.callMedia(ctx, att, req)
			return res
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}
