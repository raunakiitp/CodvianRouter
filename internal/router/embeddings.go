// Embeddings + rerank passthrough (CHECKLIST 171-172).
//
// Embed and Rerank route one request through the engine — chain
// resolution, strategy ordering, quota/breaker/cooldown filtering,
// the per-request budget gate, the adaptive limiter, ledger and
// trace — but with exactly ONE upstream attempt, ever. That is the
// replay exemption (ReplayExemption, preread.go): an embeddings
// call is single-shot vector work and a rerank scores the caller's
// documents once; replaying either onto another target after a
// failure would double-apply upstream work, so the first target
// that is actually called owns the outcome — success or failure —
// and the request ends there. What does NOT count as an attempt:
// a target skipped before dialling (zero quota, breaker open,
// cooling, limiter shed, or no embeddings/rerank capability on
// that client) — no upstream call was made, so moving to the next
// capable target is selection, not a replay. A shed target is
// skipped the same way the chat chain skips one.
//
// The bookkeeping request is a synthetic ChatRequest carrying the
// input text as user messages, the endpoint kind, the caller ID
// and the per-request budget. It is never sent upstream; it exists
// so the shared machinery — reconcileUsage (local-estimate
// fallback when the upstream reports no usage), budgetGate (the
// strict per-request budget) and recordAttempt (ledger rows) —
// works unchanged for these endpoints. A token-ID embeddings input
// carries no text, so its bookkeeping request has no messages:
// the local estimate and the strict gate cannot price it, and the
// ledger records whatever the upstream reported (or unknown).
// Rerank results echo the caller's own documents, so no response
// guardrail runs on them — scanning an echo would block the user
// on their own text (the request side is still guardrailed by the
// server before dialling).
package router

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// NoCapableProviderError is returned when no target in the
// fallback chain can serve the endpoint at all: every provider
// that offers the model speaks a native wire with no
// embeddings/rerank transport in this slice. That is a property of
// the request + the operator's configuration, not an upstream
// failure — the server answers it with an honest 400 naming the
// endpoint, never a 502, and no upstream-failure counter moves.
type NoCapableProviderError struct {
	Endpoint provider.EndpointKind
	Model    string
}

func (e *NoCapableProviderError) Error() string {
	return fmt.Sprintf("router: no provider in the fallback chain supports %s for model %q", e.Endpoint, e.Model)
}

// EmbeddingContextTooLarge is the pre-call failure when the input to
// an embeddings/rerank request exceeds every KNOWN context window in
// the chain (P1.8). Embedding surfaces have no completion leg, so the
// requirement is exactly the input tokens — no response reserve. A
// model whose window is unknown is never used to refuse (unknown is
// not proof the model is too small), matching the chat context-fit
// rule. The server maps this to a 400; no provider was called.
type EmbeddingContextTooLarge struct {
	Model         string
	PromptTokens  int
	Endpoint      provider.EndpointKind
	LargestWindow int
}

func (e *EmbeddingContextTooLarge) Error() string {
	if e.LargestWindow > 0 {
		return fmt.Sprintf("router: %s input needs ~%d tokens for model %q but the largest known context window is %d tokens — shorten the input or add a longer-context model; no provider was called",
			e.Endpoint, e.PromptTokens, e.Model, e.LargestWindow)
	}
	return fmt.Sprintf("router: %s input needs ~%d tokens for model %q but no configured model reports a context window — add a model with a known context window; no provider was called",
		e.Endpoint, e.PromptTokens, e.Model)
}

// filterEmbeddingFit drops chain attempts whose KNOWN context window is
// smaller than the input (embeddings/rerank have no completion leg, so
// there is no reserve). Attempts with an unknown window are kept —
// unknown is not proof the model is too small, exactly as in the chat
// context-fit gate. It returns the fitting attempts and the largest
// KNOWN window among the evaluated attempts, and whether any window was
// known at all. When no window is known, the whole chain is returned
// unchanged (the check cannot refuse on unknown windows).
func (e *Engine) filterEmbeddingFit(chain []Attempt, required int) (fits []Attempt, largest int, anyKnown bool) {
	for _, att := range chain {
		window, known := e.contextWindowOf(att)
		if known && window > largest {
			largest = window
		}
		if known {
			anyKnown = true
		}
		if known && required > window {
			continue // too small: drop
		}
		fits = append(fits, att)
	}
	return fits, largest, anyKnown
}

// embeddingInputTokens locally counts the concatenated input texts for
// an embeddings/rerank request. A counting failure (unknown model /
// encoder) returns an error and the caller skips the fit check, exactly
// as the chat gate does — a counting problem never fails a request.
func embeddingInputTokens(model string, texts []string) (int, error) {
	total := 0
	for _, text := range texts {
		if text == "" {
			continue
		}
		count, err := tokencount.Count(model, text)
		if err != nil {
			return 0, err
		}
		total += count.Tokens
	}
	return total, nil
}

// applyEmbeddingFit is the pre-dispatch context gate for the
// embeddings/rerank surfaces. It counts the input locally and refuses
// before any provider call when the input exceeds every KNOWN window.
func (e *Engine) applyEmbeddingFit(chain []Attempt, model string, texts []string, endpoint provider.EndpointKind) ([]Attempt, error) {
	required, err := embeddingInputTokens(model, texts)
	if err != nil {
		// Counting failure: skip the check, try the whole chain.
		return chain, nil
	}
	fits, largest, anyKnown := e.filterEmbeddingFit(chain, required)
	if len(fits) > 0 {
		return fits, nil
	}
	if !anyKnown {
		// No window was known anywhere: refuse nothing (unknown is not
		// proof of too-small), exactly like the chat gate.
		return chain, nil
	}
	return nil, &EmbeddingContextTooLarge{
		Model:         model,
		PromptTokens:  required,
		Endpoint:      endpoint,
		LargestWindow: largest,
	}
}

// supportsEmbeddings reports whether client can serve an
// embeddings call in this slice: it implements provider.Embedder
// AND speaks the OpenAI wire. Native-wire clients inherit the
// Embed method by embedding *openAIClient, so the interface check
// alone would select them — and the call would fail on the native
// base's 404. The wire check is what keeps them out of the
// selection: only an OpenAI-compatible surface offers
// /v1/embeddings. A client that does not expose its wire format
// is treated as OpenAI wire (the Client contract is
// OpenAI-compatible by definition); a future native transport
// (Cohere /v1/embed, Gemini embedContent) changes this check,
// not the call sites.
func supportsEmbeddings(client provider.Client) bool {
	if _, ok := client.(provider.Embedder); !ok {
		return false
	}
	if wf, ok := client.(wireFormatClient); ok {
		format := wf.WireFormat()
		return provider.WireFormatServesPassthrough(format)
	}
	return true
}

// supportsRerank is supportsEmbeddings for the rerank endpoint:
// an OpenAI-wire client implementing provider.Reranker.
func supportsRerank(client provider.Client) bool {
	if _, ok := client.(provider.Reranker); !ok {
		return false
	}
	if wf, ok := client.(wireFormatClient); ok {
		format := wf.WireFormat()
		return provider.WireFormatServesPassthrough(format)
	}
	return true
}

// bookkeepingRequest builds the synthetic ChatRequest the shared
// machinery (reconcileUsage, budgetGate, recordAttempt) runs on
// for one embeddings/rerank request: the input text as user
// messages, the endpoint kind (so the replay exemption and the
// ledger endpoint fire), the caller ID and the per-request budget.
// It is never sent upstream.
func bookkeepingRequest(model string, texts []string, endpoint provider.EndpointKind, callerID string, budgetUSD *float64, budgetStrict bool) provider.ChatRequest {
	msgs := make([]provider.Message, 0, len(texts))
	for _, t := range texts {
		if t == "" {
			continue
		}
		msgs = append(msgs, provider.Message{Role: RoleUser, Content: t})
	}
	return provider.ChatRequest{
		Model:        model,
		Messages:     msgs,
		Endpoint:     endpoint,
		CallerID:     callerID,
		BudgetUSD:    budgetUSD,
		BudgetStrict: budgetStrict,
	}
}

// reqWithModelEmb returns req addressed to att's model (the chain
// resolves the model per provider; the request itself names the
// model the caller asked for).
func reqWithModelEmb(req provider.EmbeddingRequest, att Attempt) provider.EmbeddingRequest {
	req.Model = att.Model
	return req
}

// reqWithModelRerank is reqWithModelEmb for rerank requests.
func reqWithModelRerank(req provider.RerankRequest, att Attempt) provider.RerankRequest {
	req.Model = att.Model
	return req
}

// callEmbed makes the single upstream embeddings call for one
// attempt, mirroring callChat: adaptive-limiter admission, panic
// isolation (a panicking client fails this attempt, never the
// gateway), and a synthetic ChatResponse carrying the reported
// usage so noteSuccess can record it. The response itself is
// returned separately — the caller needs the vectors, not the
// synthetic chat shape.
func (e *Engine) callEmbed(ctx context.Context, att Attempt, req provider.EmbeddingRequest) (emb *provider.EmbeddingResponse, res attemptResult) {
	client, ok := e.clientForAttempt(att)
	if !ok {
		return nil, attemptResult{att: att, err: fmt.Errorf("router: unknown provider %q", att.ProviderID)}
	}
	embedder, ok := client.(provider.Embedder)
	if !ok {
		return nil, attemptResult{att: att, err: fmt.Errorf("router: provider %q does not support embeddings", att.ProviderID)}
	}
	if !e.limiter.TryAcquire(att.ProviderID) {
		e.noteConcurrencyRejected(att.ProviderID)
		return nil, attemptResult{att: att, err: adaptive.ErrLimited}
	}
	defer e.limiter.Release(att.ProviderID)
	start := time.Now()
	defer func() {
		if v := recover(); v != nil {
			e.notePanic("provider_embeddings")
			emb = nil
			res = attemptResult{att: att, err: NewPanicError("provider_embeddings", v), latencyMs: time.Since(start).Milliseconds(), called: true}
		}
	}()
	resp, err := embedder.Embed(ctx, reqWithModelEmb(req, att))
	if err != nil {
		// Rate-limit learning (M5.13, the callChat precedent):
		// a 429's parsed headers pace the limiter and teach
		// the measured quota fraction (the error already
		// carries the signals — see learnRateLimitError).
		e.learnRateLimitError(att, err)
		return nil, attemptResult{att: att, err: err, latencyMs: time.Since(start).Milliseconds(), called: true}
	}
	if resp == nil {
		return nil, attemptResult{att: att, err: errors.New("router: embeddings provider returned no response"), latencyMs: time.Since(start).Milliseconds(), called: true}
	}
	usage := resp.Usage
	usage.Endpoint = provider.EndpointEmbeddings
	// Rate-limit learning (M5.13, the callChat precedent):
	// the response's parsed headers pace the adaptive limiter
	// and record the measured remaining fraction as the
	// engine's quota hint. The synthetic ChatResponse carries
	// the signal so noteSuccess's ledger path sees the same
	// shape chat attempts carry.
	e.learnRateLimit(att, resp.RateLimit)
	return resp, attemptResult{
		att:       att,
		resp:      &provider.ChatResponse{Usage: usage, Model: resp.Model, RateLimit: resp.RateLimit},
		latencyMs: time.Since(start).Milliseconds(),
		called:    true,
	}
}

// callRerank is callEmbed for rerank calls.
func (e *Engine) callRerank(ctx context.Context, att Attempt, req provider.RerankRequest) (rer *provider.RerankResponse, res attemptResult) {
	client, ok := e.clientForAttempt(att)
	if !ok {
		return nil, attemptResult{att: att, err: fmt.Errorf("router: unknown provider %q", att.ProviderID)}
	}
	reranker, ok := client.(provider.Reranker)
	if !ok {
		return nil, attemptResult{att: att, err: fmt.Errorf("router: provider %q does not support rerank", att.ProviderID)}
	}
	if !e.limiter.TryAcquire(att.ProviderID) {
		e.noteConcurrencyRejected(att.ProviderID)
		return nil, attemptResult{att: att, err: adaptive.ErrLimited}
	}
	defer e.limiter.Release(att.ProviderID)
	start := time.Now()
	defer func() {
		if v := recover(); v != nil {
			e.notePanic("provider_rerank")
			rer = nil
			res = attemptResult{att: att, err: NewPanicError("provider_rerank", v), latencyMs: time.Since(start).Milliseconds(), called: true}
		}
	}()
	resp, err := reranker.Rerank(ctx, reqWithModelRerank(req, att))
	if err != nil {
		// Rate-limit learning (M5.13, the callChat precedent):
		// a 429's parsed headers pace the limiter and teach
		// the measured quota fraction (the error already
		// carries the signals — see learnRateLimitError).
		e.learnRateLimitError(att, err)
		return nil, attemptResult{att: att, err: err, latencyMs: time.Since(start).Milliseconds(), called: true}
	}
	if resp == nil {
		return nil, attemptResult{att: att, err: errors.New("router: rerank provider returned no response"), latencyMs: time.Since(start).Milliseconds(), called: true}
	}
	usage := provider.Usage{Endpoint: provider.EndpointRerank}
	if resp.Usage != nil {
		usage = *resp.Usage
		usage.Endpoint = provider.EndpointRerank
	}
	// Rate-limit learning (M5.13, the callChat precedent) —
	// see callEmbed.
	e.learnRateLimit(att, resp.RateLimit)
	return resp, attemptResult{
		att:       att,
		resp:      &provider.ChatResponse{Usage: usage, Model: resp.Model, RateLimit: resp.RateLimit},
		latencyMs: time.Since(start).Milliseconds(),
		called:    true,
	}
}

// markExempt stamps the trace entry the note* call just appended
// for the single called attempt with the exemption label, so the
// dashboard renders "exempt (embeddings) — never replayed" for it
// exactly like the streaming path's preread record. Skipped
// attempts (breaker, cooldown, shed, unsupported) keep whatever
// outcome the note call gave them — nothing was attempted there.
func markExempt(trace *routetrace.RouteTrace, endpoint provider.EndpointKind) {
	if trace == nil || len(trace.Attempts) == 0 {
		return
	}
	trace.Attempts[len(trace.Attempts)-1].Preread = "exempt:" + string(endpoint)
}

// singleAttempt runs one embeddings/rerank request through the
// ordered chain and returns the attempt result of the single
// called attempt (or an error when nothing was called). call is
// invoked once, for the first attempt that passes every pre-dial
// filter; its result — success or failure — ends the request.
// endpoint names the surface for the trace and the error taxonomy;
// bookReq is the bookkeeping request for the shared machinery.
// The caller supplies the per-attempt call; this function owns the
// chain walk, the budget gate, the trace publishing and the
// exactly-once invariant.
func (e *Engine) singleAttempt(
	ctx context.Context,
	chain []Attempt,
	endpoint provider.EndpointKind,
	bookReq provider.ChatRequest,
	supports func(provider.Client) bool,
	call func(context.Context, Attempt) attemptResult,
	publishTrace bool,
) (res attemptResult, trace *routetrace.RouteTrace, err error) {
	trace = &routetrace.RouteTrace{Endpoint: string(endpoint)}
	// Panic isolation (CHECKLIST 165): a panic in a hook (a
	// scoring/order hook in orderAttempts) or in the chain
	// machinery becomes an ordinary error, not a dead gateway.
	// Per-attempt provider panics are already caught closer in
	// (callEmbed / callRerank); this is the backstop for the
	// machinery around them. The trace is published even on
	// this path, so the dashboard never reads a stale trace.
	defer func() {
		if v := recover(); v != nil {
			e.notePanic("engine_" + string(endpoint))
			res = attemptResult{}
			err = NewPanicError("engine_"+string(endpoint), v)
			if trace == nil {
				trace = &routetrace.RouteTrace{Endpoint: string(endpoint)}
			}
			if publishTrace {
				e.setLastTrace(trace)
			}
		}
	}()
	strategy := e.effectiveStrategy("", bookReq.Model)
	mode := e.cooldownModeFor(bookReq.Model)
	// Endpoint projection (Phase D, D4): the embeddings/rerank
	// path orders through the same effective strategy, so a
	// disallowed combo strategy fails closed here, before any
	// ordering or dial — see projectStrategyForEndpoint
	// (cascade.go).
	if projErr := checkEndpointStrategy(strategy, endpoint); projErr != nil {
		if publishTrace {
			e.setLastTrace(trace)
		}
		return attemptResult{}, trace, projErr
	}
	e.recordRouting(strategy, bookReq.Model)
	bctx, cancel := e.withDeadline(ctx)
	defer cancel()
	parentCtx := ctx
	ctx = bctx
	if len(chain) == 0 {
		if publishTrace {
			e.setLastTrace(trace)
		}
		return attemptResult{}, trace, errors.New("router: empty fallback chain")
	}
	if err := ctx.Err(); err != nil {
		if publishTrace {
			e.setLastTrace(trace)
		}
		return attemptResult{}, trace, e.mapDeadlineErr(err, parentCtx)
	}
	ordered, zeroSkipped := e.orderAttempts(chain, strategy, mode, false)
	for _, att := range zeroSkipped {
		addTraceAttempt(trace, att, 0, "skipped_zero_quota", nil)
	}
	if len(ordered) == 0 {
		if publishTrace {
			e.setLastTrace(trace)
		}
		return attemptResult{}, trace, ErrNoAvailableAttempt
	}
	sawCapable := false
	for _, att := range ordered {
		if err := ctx.Err(); err != nil {
			e.setLastTrace(trace)
			return attemptResult{}, trace, e.mapDeadlineErr(err, parentCtx)
		}
		// A cooling provider is skipped in hard mode (soft mode
		// never skips: the demoted target sits at the end of the
		// order and is still tried once). No call was made, so
		// moving on is selection, not a replay. G11 Area 2: the
		// staggered gate (onCooldownStaggered).
		if NormalizeCooldownMode(mode) == CooldownHard && e.onCooldownStaggered(ClientKey(att.ProviderID, att.KeyID)) {
			addTraceAttempt(trace, att, 0, "skipped_cooldown", nil)
			continue
		}
		if !e.breakerAllow(att.ProviderID) {
			addTraceAttempt(trace, att, 0, "skipped_breaker", nil)
			continue
		}
		client, ok := e.clientForAttempt(att)
		if !ok {
			addTraceAttempt(trace, att, 0, "skipped_no_client", nil)
			continue
		}
		if !supports(client) {
			// The target cannot serve this endpoint (a native
			// wire with no embeddings/rerank transport): skip
			// it before dialling, so a capable target later in
			// the chain can serve the request. The skip is in
			// the trace, so the operator can see why the
			// target was passed over.
			addTraceAttempt(trace, att, 0, "skipped_unsupported", nil)
			continue
		}
		sawCapable = true
		// Per-request budget (CHECKLIST 155): in strict mode the
		// chain stops BEFORE an attempt whose priced prompt
		// estimate would push cumulative spend over the budget.
		// The attempt is never dialled; the trace records the
		// skip and the caller gets a typed 402 error.
		if blocked, est := e.budgetGate(bookReq, att, 0); blocked {
			be := e.budgetExceededError(bookReq, att, 0, est)
			addTraceAttempt(trace, att, 0, "skipped_budget", be)
			if publishTrace {
				e.setLastTrace(trace)
			}
			return attemptResult{}, trace, be
		}
		res := call(ctx, att)
		if errors.Is(res.err, adaptive.ErrLimited) {
			// Shed by the limiter: the provider was never
			// tried (called=false), so the attempt is not
			// spent — move to the next capable target. The
			// skip is recorded by noteFailure below; the
			// loop continues instead of ending here.
			// "" chain key: embeddings carry no LKGP chain,
			// so a failure here never clears a chat pin.
			consecutive429 := 0
			_, _, _ = e.noteFailure(trace, res, &consecutive429, mode, bookReq.CallerID, "")
			continue
		}
		// The attempt was called (or panicked inside the
		// client, which counts as called): this is the one
		// attempt, whatever its outcome. Record it and stop.
		if res.err == nil {
			e.noteSuccess(trace, res, bookReq)
			// Decision transparency (CHECKLIST 199): a
			// single-attempt surface has no contract and no
			// tree, so the reason is the strategy (or
			// fallback-after-N when earlier targets were
			// skipped before dialling — the trace counts
			// them the same way as chat).
			e.recordDecision(trace, nil, false, strategy, bookReq.Model, res.att.Model, res.att.ProviderID)
		} else {
			consecutive429 := 0
			_, _, _ = e.noteFailure(trace, res, &consecutive429, mode, bookReq.CallerID, "")
		}
		markExempt(trace, endpoint)
		if publishTrace {
			e.setLastTrace(trace)
		}
		return res, trace, res.err
	}
	// Nothing was called: every target was skipped before
	// dialling. When no target could serve the endpoint at all,
	// say so honestly (a 400, not a 502); when capable targets
	// existed but were all skipped (breaker, cooldown, shed),
	// the chain had nothing available.
	if publishTrace {
		e.setLastTrace(trace)
	}
	if !sawCapable {
		return attemptResult{}, trace, &NoCapableProviderError{Endpoint: endpoint, Model: bookReq.Model}
	}
	return attemptResult{}, trace, ErrNoAvailableAttempt
}

// Embed routes one embeddings request: exactly one upstream
// attempt (see the package doc). The chain is resolved for
// req.Model exactly like Chat resolves its chain.
func (e *Engine) Embed(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	bookReq := bookkeepingRequest(req.Model, req.TextInputs(), provider.EndpointEmbeddings, req.CallerID, req.BudgetUSD, req.BudgetStrict)
	chain := e.chainFor(req.Model)
	// P1.8 context gate: refuse before any provider call when the input
	// exceeds every known window (embeddings have no completion leg).
	chain, fitErr := e.applyEmbeddingFit(chain, req.Model, req.TextInputs(), provider.EndpointEmbeddings)
	if fitErr != nil {
		return nil, fitErr
	}
	var emb *provider.EmbeddingResponse
	_, _, err := e.singleAttempt(ctx, chain, provider.EndpointEmbeddings, bookReq, supportsEmbeddings,
		func(ctx context.Context, att Attempt) attemptResult {
			var res attemptResult
			emb, res = e.callEmbed(ctx, att, req)
			return res
		}, true)
	if err != nil {
		return nil, err
	}
	return emb, nil
}

// EmbedInternal routes one embeddings request without publishing
// the attempt trace to the engine's global LastTrace slot. Used by
// internal callers (e.g., semantic cache neural embedder) that must
// not clobber the client-visible route trace.
func (e *Engine) EmbedInternal(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	bookReq := bookkeepingRequest(req.Model, req.TextInputs(), provider.EndpointEmbeddings, req.CallerID, req.BudgetUSD, req.BudgetStrict)
	chain := e.chainFor(req.Model)
	// P1.8 context gate: refuse before any provider call when the input
	// exceeds every known window (embeddings have no completion leg).
	chain, fitErr := e.applyEmbeddingFit(chain, req.Model, req.TextInputs(), provider.EndpointEmbeddings)
	if fitErr != nil {
		return nil, fitErr
	}
	var emb *provider.EmbeddingResponse
	_, _, err := e.singleAttempt(ctx, chain, provider.EndpointEmbeddings, bookReq, supportsEmbeddings,
		func(ctx context.Context, att Attempt) attemptResult {
			var res attemptResult
			emb, res = e.callEmbed(ctx, att, req)
			return res
		}, false)
	if err != nil {
		return nil, err
	}
	return emb, nil
}

// Rerank routes one rerank request: exactly one upstream attempt
// (see the package doc). The chain is resolved for req.Model
// exactly like Chat resolves its chain.
func (e *Engine) Rerank(ctx context.Context, req provider.RerankRequest) (*provider.RerankResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	texts := append([]string{req.Query}, req.TextDocuments()...)
	bookReq := bookkeepingRequest(req.Model, texts, provider.EndpointRerank, req.CallerID, req.BudgetUSD, req.BudgetStrict)
	chain := e.chainFor(req.Model)
	// P1.8 context gate: refuse before any provider call when the input
	// exceeds every known window.
	chain, fitErr := e.applyEmbeddingFit(chain, req.Model, texts, provider.EndpointRerank)
	if fitErr != nil {
		return nil, fitErr
	}
	var rer *provider.RerankResponse
	_, _, err := e.singleAttempt(ctx, chain, provider.EndpointRerank, bookReq, supportsRerank,
		func(ctx context.Context, att Attempt) attemptResult {
			var res attemptResult
			rer, res = e.callRerank(ctx, att, req)
			return res
		}, true)
	if err != nil {
		return nil, err
	}
	return rer, nil
}

// EmbeddingCapability reports, without dialling any provider, whether
// the engine can serve an embeddings call for model: providers is the
// sorted list of provider IDs with at least one registered
// embedding-capable client, and chainCapable says whether at least one
// attempt in the model's resolved chain is embedding-capable (a chain
// that excludes every capable provider cannot serve the call, so the
// caller would only ever get the fail-open fallback). Serve mode uses
// this for its single semantic-cache startup line, so the log can say
// honestly whether neural embeddings are wired or not.
func (e *Engine) EmbeddingCapability(model string) (providers []string, chainCapable bool) {
	e.clientsMu.RLock()
	seen := make(map[string]bool, len(e.clients))
	for key, c := range e.clients {
		if c == nil || !supportsEmbeddings(c) {
			continue
		}
		id, _ := SplitClientKey(key)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		providers = append(providers, id)
	}
	e.clientsMu.RUnlock()
	sort.Strings(providers)
	for _, att := range e.chainFor(model) {
		if client, ok := e.clientForAttempt(att); ok && supportsEmbeddings(client) {
			chainCapable = true
			break
		}
	}
	return providers, chainCapable
}
