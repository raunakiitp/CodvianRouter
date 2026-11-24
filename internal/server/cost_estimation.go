package server

// Pre-dispatch cost estimation and estimate-vs-actual reconciliation
// (P1.7).
//
// Two rules govern this file:
//
//  1. An estimate is not an invoice. Estimated cost is labelled as an
//     estimate, priced from the price book only, and NEVER produced by
//     calling a provider.
//  2. Nothing is marked "reconciled" merely because token usage exists.
//     A reconciliation is reconciled only when BOTH sides are priced;
//     otherwise it says which side is unknown and why.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// CostEstimateRequest is the request body for POST /api/cost/estimate.
//
// Every field beyond model/messages is optional. provider selects
// provider-specific pricing and tokenizer framing; tools and multimodal
// message parts are counted when present; output_tokens is the
// completion length the caller intends to request (max_tokens is the
// fallback).
type CostEstimateRequest struct {
	Model        string                    `json:"model"`
	Provider     string                    `json:"provider,omitempty"`
	Messages     []provider.Message        `json:"messages"`
	Tools        []provider.ToolDefinition `json:"tools,omitempty"`
	MaxTokens    int                       `json:"max_tokens,omitempty"`
	OutputTokens int                       `json:"output_tokens,omitempty"`
	Temperature  float64                   `json:"temperature,omitempty"`
	Stream       bool                      `json:"stream,omitempty"`
	// Wire optionally names the wire format the request will use
	// ("openai", "anthropic", "gemini"); when empty, generic framing is
	// used and the response says which wire was counted.
	Wire string `json:"wire,omitempty"`
	// BudgetCheck asks for the caller's key budget to be consulted
	// (read-only) against the estimate: within/over, remaining, and
	// whether a soft warning threshold is crossed. It never refuses the
	// estimate and never dispatches.
	BudgetCheck bool `json:"budget_check,omitempty"`
}

// CostEstimateResponse is the response for POST /api/cost/estimate.
type CostEstimateResponse struct {
	Model                     string         `json:"model"`
	Provider                  string         `json:"provider,omitempty"`
	PromptTokens              int            `json:"prompt_tokens"`
	PromptTokensSource        string         `json:"prompt_tokens_source"` // tiktoken_wire | tiktoken | chars_fallback
	TokenCountWire            string         `json:"token_count_wire,omitempty"`
	EstimatedCompletionTokens int            `json:"estimated_completion_tokens"`
	CompletionTokensSource    string         `json:"completion_tokens_source"` // requested | max_tokens | heuristic
	EstimatedCostUSD          float64        `json:"estimated_cost_usd"`
	CostKnown                 bool           `json:"cost_known"`
	PriceSource               string         `json:"price_source"` // pair | model | models.dev | unknown
	Breakdown                 *CostBreakdown `json:"breakdown,omitempty"`
	Budget                    *BudgetCheck   `json:"budget_check,omitempty"`
	Warning                   string         `json:"warning,omitempty"`
	// Estimate flags, stated rather than implied: the cached-token share
	// and cache writes cannot be known before the provider answers.
	CachedShareUnknown bool   `json:"cached_share_unknown"`
	EstimateNote       string `json:"estimate_note"`
}

// CostBreakdown is the priced legs of the estimate. Every rate is
// reported when known; a nil rate means the price book has no value for
// that leg, never zero.
type CostBreakdown struct {
	InputCostUSD       float64  `json:"input_cost_usd"`
	OutputCostUSD      float64  `json:"output_cost_usd,omitempty"`
	CachedInputCostUSD float64  `json:"cached_input_cost_usd,omitempty"`
	CacheWriteCostUSD  float64  `json:"cache_write_cost_usd,omitempty"`
	InputPricePer1M    *float64 `json:"input_price_per_1m,omitempty"`
	OutputPricePer1M   *float64 `json:"output_price_per_1m,omitempty"`
	CachedInputPer1M   *float64 `json:"cached_input_price_per_1m,omitempty"`
	CacheWrite5mPer1M  *float64 `json:"cache_write_5m_price_per_1m,omitempty"`
	CacheWrite1hPer1M  *float64 `json:"cache_write_1h_price_per_1m,omitempty"`
}

// BudgetCheck is the read-only pre-dispatch budget view.
type BudgetCheck struct {
	KeyID        string  `json:"key_id,omitempty"`
	WithinBudget bool    `json:"within_budget"`
	RemainingUSD float64 `json:"remaining_usd"`
	SoftWarn     bool    `json:"soft_warn"`
	Note         string  `json:"note,omitempty"`
}

// estimateCompletionTokens derives the completion leg: the requested
// output length, then max_tokens, then a documented heuristic. The
// source is reported so a caller can see which applied.
func estimateCompletionTokens(req *CostEstimateRequest) (int, string) {
	if req.OutputTokens > 0 {
		return req.OutputTokens, "requested"
	}
	if req.MaxTokens > 0 {
		return req.MaxTokens, "max_tokens"
	}
	return 500, "heuristic"
}

// chatRequestForCount builds the canonical request the token counter
// sees: the same messages and tool definitions that will be sent.
func chatRequestForCount(req *CostEstimateRequest) provider.ChatRequest {
	return provider.ChatRequest{
		Model:     req.Model,
		Messages:  req.Messages,
		Tools:     req.Tools,
		MaxTokens: req.MaxTokens,
		Stream:    req.Stream,
	}
}

// countPromptTokens counts the prompt locally, in order of fidelity:
// wire-framed calibrated tiktoken (counts tools and multimodal parts),
// plain tiktoken, then a char/4 fallback. The source is always reported.
func countPromptTokens(providerID string, req *CostEstimateRequest, wire string) (tokens int, source, usedWire string) {
	chat := chatRequestForCount(req)
	if wire != "" {
		if tc, err := tokencount.CountPromptWireCalibrated(providerID, req.Model, wire, chat); err == nil && tc.Tokens > 0 {
			return tc.Tokens, "tiktoken_wire", wire
		}
	}
	if tc, err := tokencount.CountPrompt(req.Model, req.Messages); err == nil && tc.Tokens > 0 {
		return tc.Tokens, "tiktoken", ""
	}
	totalChars := 0
	for _, m := range req.Messages {
		totalChars += len(m.Content)
	}
	if totalChars <= 0 {
		return 1, "chars_fallback", ""
	}
	return totalChars / 4, "chars_fallback", ""
}

// priceFor resolves the price the estimate is allowed to use: the
// selected provider's pair price first, then the model-level price, then
// the opt-in models.dev-synced pair. A price that is not Known is
// reported as "unknown" — never priced as zero.
func (s *Server) priceFor(providerID, model string) (config.ModelPrice, string) {
	p, src := s.pairPrice(providerID, model)
	if !p.Known() {
		return config.ModelPrice{}, "unknown"
	}
	switch src {
	case "pair", "model", "models.dev":
		return p, src
	default:
		return p, "unknown"
	}
}

// handleCostEstimate serves POST /api/cost/estimate. No provider call is
// made: pricing comes from the price book, token counting is local.
func (s *Server) handleCostEstimate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req CostEstimateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages is required")
		return
	}

	tokens, source, usedWire := countPromptTokens(req.Provider, &req, req.Wire)
	completionTokens, completionSource := estimateCompletionTokens(&req)
	price, priceSource := s.priceFor(req.Provider, req.Model)

	resp := CostEstimateResponse{
		Model:                     req.Model,
		Provider:                  req.Provider,
		PromptTokens:              tokens,
		PromptTokensSource:        source,
		TokenCountWire:            usedWire,
		EstimatedCompletionTokens: completionTokens,
		CompletionTokensSource:    completionSource,
		PriceSource:               priceSource,
		CachedShareUnknown:        true,
		EstimateNote:              "Estimate only, from the price book — not an invoice. The cached-token share and cache writes are unknown before the provider answers.",
	}

	// Price the legs. A nil rate is reported as unknown, never zero.
	bd := &CostBreakdown{
		InputPricePer1M:   price.InputUSDPer1M,
		OutputPricePer1M:  price.OutputUSDPer1M,
		CachedInputPer1M:  price.CachedInputUSDPer1M,
		CacheWrite5mPer1M: price.CacheWrite5mUSDPer1M,
		CacheWrite1hPer1M: price.CacheWrite1hUSDPer1M,
	}
	if price.InputUSDPer1M != nil {
		bd.InputCostUSD = float64(tokens) * (*price.InputUSDPer1M / 1e6)
		resp.CostKnown = true
	}
	if price.OutputUSDPer1M != nil {
		bd.OutputCostUSD = float64(completionTokens) * (*price.OutputUSDPer1M / 1e6)
	}
	// Cached input: the request has not run, so the cached share is
	// unknown. The RATE is reported (so an operator can see the upside);
	// no cached cost is invented.
	resp.Breakdown = bd
	resp.EstimatedCostUSD = bd.InputCostUSD + bd.OutputCostUSD

	switch {
	case !resp.CostKnown:
		resp.Warning = fmt.Sprintf("model %q has no known input pricing (source %q); cost cannot be estimated", req.Model, priceSource)
	case price.OutputUSDPer1M == nil:
		resp.Warning = fmt.Sprintf("model %q has input pricing but no output pricing; the completion leg is unpriced", req.Model)
	}

	if req.BudgetCheck {
		resp.Budget = s.budgetCheckFor(r, resp.EstimatedCostUSD)
	}
	writeJSON(w, http.StatusOK, resp)
}

// budgetCheckFor consults the caller key's budget read-only against an
// estimate. It never refuses anything (this endpoint does not dispatch)
// and reports an unavailable ledger honestly rather than assuming room.
func (s *Server) budgetCheckFor(r *http.Request, estimateUSD float64) *BudgetCheck {
	out := &BudgetCheck{}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		keyID = s.callerKeyIDFromContext(r.Context())
	}
	if keyID == "" {
		out.Note = "no authenticated caller key on this request; budget not consulted"
		return out
	}
	out.KeyID = keyID
	if s.ledger == nil {
		out.Note = "no ledger wired; budget not consulted"
		return out
	}
	remaining, warn, err := s.ledger.CheckBudgetForEstimate(r.Context(), keyID, estimateUSD)
	if err != nil {
		var be *ledger.BudgetExceededError
		if errors.As(err, &be) {
			out.WithinBudget = false
			out.RemainingUSD = be.RemainingUSD
			out.Note = "the estimate would exceed the key budget"
			return out
		}
		out.Note = "budget check failed: " + err.Error()
		return out
	}
	out.WithinBudget = true
	out.RemainingUSD = remaining
	if math.IsInf(remaining, 1) {
		// No cap on this key: the ledger reports +Inf. A JSON body
		// cannot carry that value, and an infinite remaining figure is
		// not useful to an operator — report no cap instead.
		out.RemainingUSD = 0
		out.Note = "no budget cap is set for this key"
		return out
	}
	if math.IsNaN(out.RemainingUSD) {
		out.RemainingUSD = 0
		out.Note = "the ledger reported an unusable remaining figure"
		return out
	}
	if out.RemainingUSD < 0 {
		// The window is already overspent: every additional estimate is
		// over budget, whatever the soft-warning figure says.
		out.WithinBudget = false
		out.SoftWarn = true
		out.Note = "the key has no remaining budget in its current window"
		return out
	}
	out.SoftWarn = warn
	if warn {
		out.Note = "the estimate crosses the soft warning threshold"
	}
	return out
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

// CostReconciliation compares an estimate with a completed request's
// usage. Status is one of:
//
//	reconciled       — both the estimated and actual cost are known
//	estimate_unknown — the estimate could not be priced
//	actual_unknown   — usage exists but cannot be priced (missing price,
//	                   or no reported token split)
//	usage_missing    — no provider-reported usage at all
//
// Variance is reported only in the "reconciled" case: a variance against
// an unknown side would be a fabricated number.
type CostReconciliation struct {
	Model            string    `json:"model"`
	Provider         string    `json:"provider,omitempty"`
	EstimatedCostUSD float64   `json:"estimated_cost_usd"`
	ActualCostUSD    float64   `json:"actual_cost_usd"`
	VarianceUSD      float64   `json:"variance_usd"`
	VariancePct      float64   `json:"variance_pct"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CachedTokens     int       `json:"cached_tokens,omitempty"`
	CacheWriteTokens int       `json:"cache_write_tokens,omitempty"`
	Status           string    `json:"status"`
	Note             string    `json:"note,omitempty"`
	Timestamp        time.Time `json:"timestamp"`
}

// CostReconcileRequest is the body of POST /api/cost/reconcile: the
// estimate that was made, plus the usage the provider reported. Nothing
// here triggers a provider call.
type CostReconcileRequest struct {
	Model            string         `json:"model"`
	Provider         string         `json:"provider,omitempty"`
	EstimatedCostUSD float64        `json:"estimated_cost_usd"`
	EstimateKnown    bool           `json:"estimate_known"`
	Usage            provider.Usage `json:"usage"`
	SplitKnown       bool           `json:"split_known"`
}

// actualCostUSD prices a provider-reported usage block. ok is false when
// the price is unknown or the split is not reported — the caller must not
// read a zero as "free".
func actualCostUSD(price config.ModelPrice, usage provider.Usage, splitKnown bool) (float64, bool) {
	if !splitKnown || (usage.PromptTokens <= 0 && usage.CompletionTokens <= 0) {
		return 0, false
	}
	if price.InputUSDPer1M == nil || price.OutputUSDPer1M == nil {
		return 0, false
	}
	prompt := usage.PromptTokens
	cached := usage.CachedTokens
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt // a cached count can never exceed the prompt
	}
	uncached := prompt - cached

	var cost float64
	if cached > 0 && price.CachedInputUSDPer1M != nil {
		cost += float64(cached) * (*price.CachedInputUSDPer1M / 1e6)
	} else {
		// No cached rate: the cached share prices at the plain input
		// rate. That is the honest fallback (the provider charged
		// something for those tokens), not a guess of zero.
		cost += float64(cached) * (*price.InputUSDPer1M / 1e6)
	}
	cost += float64(uncached) * (*price.InputUSDPer1M / 1e6)

	// Cache writes (Anthropic-style): the tier rate when the tier is
	// known, else the plain input rate.
	cw5, cw1 := usage.CacheWriteTokens5m, usage.CacheWriteTokens1h
	cw := usage.CacheWriteTokens - cw5 - cw1
	if cw5 > 0 && price.CacheWrite5mUSDPer1M != nil {
		cost += float64(cw5) * (*price.CacheWrite5mUSDPer1M / 1e6)
	} else {
		cost += float64(cw5) * (*price.InputUSDPer1M / 1e6)
	}
	if cw1 > 0 && price.CacheWrite1hUSDPer1M != nil {
		cost += float64(cw1) * (*price.CacheWrite1hUSDPer1M / 1e6)
	} else {
		cost += float64(cw1) * (*price.InputUSDPer1M / 1e6)
	}
	if cw > 0 {
		cost += float64(cw) * (*price.InputUSDPer1M / 1e6)
	}

	cost += float64(usage.CompletionTokens) * (*price.OutputUSDPer1M / 1e6)
	return cost, true
}

// reconcile performs the reconciliation. It is the single implementation
// used by the endpoint and by tests.
func (s *Server) reconcile(in CostReconcileRequest) CostReconciliation {
	rec := CostReconciliation{
		Model:            in.Model,
		Provider:         in.Provider,
		EstimatedCostUSD: in.EstimatedCostUSD,
		PromptTokens:     in.Usage.PromptTokens,
		CompletionTokens: in.Usage.CompletionTokens,
		CachedTokens:     in.Usage.CachedTokens,
		CacheWriteTokens: in.Usage.CacheWriteTokens,
		Timestamp:        time.Now(),
	}
	if in.Usage.PromptTokens <= 0 && in.Usage.CompletionTokens <= 0 {
		rec.Status = "usage_missing"
		rec.Note = "the provider reported no usage; there is nothing to reconcile"
		return rec
	}
	price, src := s.priceFor(in.Provider, in.Model)
	actual, ok := actualCostUSD(price, in.Usage, in.SplitKnown)
	if !ok {
		rec.Status = "actual_unknown"
		if src == "unknown" {
			rec.Note = "no price for this model/provider; the actual cost cannot be computed"
		} else {
			rec.Note = "usage was reported without a known token split; the actual cost cannot be computed"
		}
		return rec
	}
	rec.ActualCostUSD = actual
	if !in.EstimateKnown {
		rec.Status = "estimate_unknown"
		rec.Note = "the estimate was not priced; only the actual cost is known"
		return rec
	}
	rec.Status = "reconciled"
	rec.VarianceUSD = rec.ActualCostUSD - rec.EstimatedCostUSD
	if rec.EstimatedCostUSD != 0 {
		rec.VariancePct = rec.VarianceUSD / rec.EstimatedCostUSD * 100
	} else {
		rec.Note = "the estimate is zero; a variance percentage is undefined"
	}
	return rec
}

// handleCostReconcile serves POST /api/cost/reconcile.
func (s *Server) handleCostReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in CostReconcileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if in.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	writeJSON(w, http.StatusOK, s.reconcile(in))
}

// registerCostRoutes wires the P3-C cost estimation API.
func (s *Server) registerCostRoutes(mux *http.ServeMux) {
	s.register(mux, "POST", "/api/cost/estimate", s.handleCostEstimate)
	s.register(mux, "POST", "/api/cost/reconcile", s.handleCostReconcile)
}

// ctxKeyCallerKey carries the authenticated caller's key ID from the
// request middleware to handlers that need it without the request.
type ctxKeyCallerKey struct{}

// callerKeyHolder resolves the caller key lazily and once per request:
// the verifier lookup costs a store read, so it runs at most once and
// only when something actually asks.
type callerKeyHolder struct {
	s    *Server
	r    *http.Request
	once sync.Once
	id   string
}

func (h *callerKeyHolder) keyID() string {
	h.once.Do(func() { h.id = h.s.callerKeyID(h.r) })
	return h.id
}

// callerKeyIDFromContext returns the authenticated key ID carried by the
// request context, or "" when the context was not produced by this
// server's middleware (or the request was unauthenticated). It never
// guesses an identity.
func (s *Server) callerKeyIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if h, ok := ctx.Value(ctxKeyCallerKey{}).(*callerKeyHolder); ok && h != nil {
		return h.keyID()
	}
	return ""
}
