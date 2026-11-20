// Per-request USD budget (CHECKLIST 155-156): the
// client-supplied cap from the X-Codvian-Budget header.
//
// Headers (all on POST /v1/chat/completions):
//
//   - X-Codvian-Budget: <decimal USD> — the max this request may
//     spend. Required to be a finite number > 0; anything else
//     (unparseable, <= 0, NaN/Inf) is a 400, never silently
//     ignored — a client that thinks it set a cap must not be
//     served as if it had not.
//   - X-Codvian-Budget-Strict: true | 1 | strict — strict mode:
//     the engine stops the chain before any attempt whose
//     priced prompt estimate would cross the budget (HTTP 402,
//     the attempt never dialled). Absent/false/0 = non-strict:
//     the chain continues and the overage is recorded honestly.
//     The reference-style alias Budget-Fallback: strict is also
//     accepted (same meaning); X-Codvian-Budget-Strict wins
//     when both are present.
//
// Enforcement points, in order:
//
//  1. Pre-send (enforceRequestBudget, after guardrails, before
//     the stream/non-stream branch): the prompt is counted
//     locally and priced at the model level (the serving
//     provider is not known yet — same price source as item
//     145). If the priced prompt-only estimate alone exceeds
//     the budget, the request is refused with HTTP 402 and zero
//     provider calls, in BOTH modes: the estimate is a lower
//     bound (completion not included), so serving would
//     guarantee an overspend on the prompt alone. An unpriced
//     pair or a counting failure proceeds — an estimate that
//     cannot be trusted never refuses a request.
//  2. Mid-chain (engine budgetGate, strict only): before each
//     fallback attempt, priced at the real (provider, model)
//     pair. The typed error is mapped to 402 by
//     handleEngineError and recorded here as refused_mid_chain.
//  3. Post-hoc (non-strict overage, and strict completions
//     whose completion pushed the priced actual over the
//     budget): the served response's reported usage is priced
//     at the serving pair (trace first, model-level fallback)
//     and, when it exceeds the budget, recorded as an overage.
//     A stream's actual cost cannot be priced (streaming usage
//     is a local-estimate fallback with cost $0), so streams
//     never produce a post-hoc overage — stated, not hidden.
//
// Recording: requestBudgetStore is in-memory only (like the
// coalescing counters and the guardrail store's live view):
// counts since server start + the 50 most recent refusal /
// overage events (model, budget, strict, outcome, estimated /
// spent / actual cost — never prompt text, never a key). A
// restart resets it to the honest empty state; the ledger
// remains the billing record (per-attempt rows with real costs).
package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// Request-budget header names.
const (
	headerRequestBudget       = "X-Codvian-Budget"
	headerRequestBudgetStrict = "X-Codvian-Budget-Strict"
	// headerBudgetFallback is the reference-style alias for the
	// strict flag (the reference router's budget companion header
	// is Budget-Fallback: strict). Accepted, never required.
	headerBudgetFallback = "Budget-Fallback"
)

// parseRequestBudget reads the per-request budget headers. It
// returns (budget, strict, ok): budget nil means "no header —
// no per-request budget". ok is false only when a header is
// present but invalid, in which case err names the problem and
// the caller must answer 400.
func parseRequestBudget(r *http.Request) (budget *float64, strict bool, err error) {
	raw := strings.TrimSpace(r.Header.Get(headerRequestBudget))
	if raw == "" {
		return nil, false, nil
	}
	v, perr := strconv.ParseFloat(raw, 64)
	// ParseFloat accepts "Inf"/"NaN" spellings: a budget must be
	// a finite number, so reject those explicitly.
	if perr != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return nil, false, fmt.Errorf("invalid %s %q: must be a finite USD amount greater than 0", headerRequestBudget, raw)
	}
	budget = &v
	strictRaw := strings.TrimSpace(r.Header.Get(headerRequestBudgetStrict))
	if strictRaw == "" {
		// reference-style alias: Budget-Fallback: strict.
		if strings.EqualFold(strings.TrimSpace(r.Header.Get(headerBudgetFallback)), "strict") {
			strict = true
		}
		return budget, strict, nil
	}
	switch strings.ToLower(strictRaw) {
	case "true", "1", "strict":
		strict = true
	case "false", "0", "non-strict", "lenient":
		strict = false
	default:
		return nil, false, fmt.Errorf("invalid %s %q: must be true, false, 1, 0 or strict", headerRequestBudgetStrict, strictRaw)
	}
	return budget, strict, nil
}

// requestBudgetEvent is one recorded refusal or overage: counts
// and costs only — never prompt text, never a key, never a base
// URL.
type requestBudgetEvent struct {
	At               time.Time `json:"at"`
	Model            string    `json:"model"`
	Provider         string    `json:"provider,omitempty"`
	BudgetUSD        float64   `json:"budget_usd"`
	Strict           bool      `json:"strict"`
	Outcome          string    `json:"outcome"` // refused_pre_send | refused_mid_chain | overage
	EstimatedCostUSD float64   `json:"estimated_cost_usd,omitempty"`
	SpentUSD         float64   `json:"spent_usd,omitempty"`
	ActualCostUSD    *float64  `json:"actual_cost_usd,omitempty"`
	PromptTokens     int       `json:"prompt_tokens,omitempty"`
}

// requestBudgetFeed is the GET /dashboard/request-budgets
// payload: counters since server start + recent events, newest
// first. Empty before any budgeted request — never invented.
type requestBudgetFeed struct {
	RequestsWithBudget uint64               `json:"requests_with_budget"`
	RefusedPreSend     uint64               `json:"refused_pre_send"`
	RefusedMidChain    uint64               `json:"refused_mid_chain"`
	Overages           uint64               `json:"overages"`
	Events             []requestBudgetEvent `json:"events"`
}

// requestBudgetStore is the in-memory recorder for per-request
// budget outcomes. Bounded: the 50 most recent events are kept,
// older ones drop off (counts never drop).
type requestBudgetStore struct {
	mu              sync.Mutex
	withBudget      uint64
	refusedPreSend  uint64
	refusedMidChain uint64
	overages        uint64
	events          []requestBudgetEvent
}

const requestBudgetMaxEvents = 50

func newRequestBudgetStore() *requestBudgetStore {
	return &requestBudgetStore{events: []requestBudgetEvent{}}
}

func (s *requestBudgetStore) noteBudgeted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.withBudget++
}

func (s *requestBudgetStore) record(ev requestBudgetEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Outcome {
	case "refused_pre_send":
		s.refusedPreSend++
	case "refused_mid_chain":
		s.refusedMidChain++
	case "overage":
		s.overages++
	}
	ev.At = time.Now()
	s.events = append([]requestBudgetEvent{ev}, s.events...)
	if len(s.events) > requestBudgetMaxEvents {
		s.events = s.events[:requestBudgetMaxEvents]
	}
}

func (s *requestBudgetStore) snapshot() requestBudgetFeed {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs := make([]requestBudgetEvent, len(s.events))
	copy(evs, s.events)
	return requestBudgetFeed{
		RequestsWithBudget: s.withBudget,
		RefusedPreSend:     s.refusedPreSend,
		RefusedMidChain:    s.refusedMidChain,
		Overages:           s.overages,
		Events:             evs,
	}
}

// handleRequestBudgets serves GET /dashboard/request-budgets
// (Tier 3 MANAGEMENT, auth-exempt): live per-request budget
// refusals and overages from this process — counts since start
// plus recent events. No keys, no base URLs, no message content.
func (s *Server) handleRequestBudgets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.reqBudgets.snapshot())
}

// enforceRequestBudget is the pre-send per-request budget guard
// (CHECKLIST 155): it parses the budget headers (400 on an
// invalid value), attaches the budget to the request for the
// engine's mid-chain gate, and refuses BEFORE any provider is
// dialled when the priced prompt-only estimate alone exceeds
// the budget — 402, zero provider calls, in both modes (the
// estimate is a lower bound). It returns true when the handler
// must refuse the request (the error is already written).
//
//   - No budget header: nothing to enforce, proceed.
//   - Unpriced pair or counting failure: proceed — only the
//     already-exhausted key-budget check (enforcePromptBudget,
//     which runs next) can refuse; an untrustworthy estimate
//     never refuses a request on its own.
func (s *Server) enforceRequestBudget(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest) (refused bool) {
	budget, strict, err := parseRequestBudget(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return true
	}
	if budget == nil {
		return false
	}
	req.BudgetUSD = budget
	req.BudgetStrict = strict
	s.reqBudgets.noteBudgeted()
	price, _ := s.pairPrice("", req.Model)
	if price.InputUSDPer1M == nil {
		return false
	}
	// CountPromptFull (tools + reference framing, CHECKLIST
	// 229-230): the prompt estimate must count what the provider
	// will bill — a message-text-only count undercounts a
	// tool-heavy prompt by up to 80%, so a request the gate
	// thinks fits could still overshoot a fail-closed budget.
	count, err := tokencount.CountPromptFull(req.Model, *req)
	if err != nil {
		s.logger.Printf("request budget: local token count failed (proceeding): %v", err)
		return false
	}
	estimatedCost := float64(count.Tokens) * (*price.InputUSDPer1M / 1e6)
	if estimatedCost <= *budget {
		return false
	}
	s.reqBudgets.record(requestBudgetEvent{
		Model:            req.Model,
		BudgetUSD:        *budget,
		Strict:           strict,
		Outcome:          "refused_pre_send",
		EstimatedCostUSD: estimatedCost,
		PromptTokens:     count.Tokens,
	})
	mode := "non-strict"
	if strict {
		mode = "strict"
	}
	writeError(w, http.StatusPaymentRequired, fmt.Sprintf(
		"per-request budget exceeded: estimated prompt cost $%.6f for ~%d prompt tokens (local estimate, prompt only — completion not included) exceeds the per-request budget $%.6f (%s mode); request refused before any provider was called",
		estimatedCost, count.Tokens, *budget, mode))
	return true
}

// noteRequestBudgetMidChain records a strict-mode mid-chain
// refusal (the engine stopped before a pricier fallback) for
// the dashboard feed. It is a no-op for any other error.
func (s *Server) noteRequestBudgetMidChain(req *provider.ChatRequest, err error) {
	var be *router.RequestBudgetExceededError
	if !errors.As(err, &be) {
		return
	}
	if req.BudgetUSD == nil {
		return
	}
	s.reqBudgets.record(requestBudgetEvent{
		Model:            req.Model,
		Provider:         be.Provider,
		BudgetUSD:        be.BudgetUSD,
		Strict:           true,
		Outcome:          "refused_mid_chain",
		EstimatedCostUSD: be.NextEstimateUSD,
		SpentUSD:         be.SpentUSD,
	})
}

// checkRequestBudgetOverage prices a served non-streaming
// response's reported usage at the serving pair (route trace
// first, model-level fallback) and records an overage when the
// priced actual cost exceeds the request's budget. It also sets
// X-Codvian-Budget-Spent to the priced actual cost when the
// response could be priced at all, so the client can see what
// the request cost against its cap. Unpriced responses record
// nothing and set no header: no price, no verdict. Streaming
// responses never reach this function — their usage is a
// local-estimate fallback (cost $0), so no actual cost can be
// honestly priced for them.
func (s *Server) checkRequestBudgetOverage(ctx context.Context, w http.ResponseWriter, req *provider.ChatRequest, resp *provider.ChatResponse) {
	if req.BudgetUSD == nil || resp == nil {
		return
	}
	// Serving provider from this request's own route trace
	// (CHECKLIST 317-318: the attribution sink on ctx, falling
	// back to the engine-global most-recent trace only when no
	// sink was attached — the ledger, not this header, is the
	// billing record). Fall back to the model-level price when
	// the trace is unavailable.
	providerID := ""
	if trace := s.requestTrace(ctx); trace != nil {
		for _, att := range trace.Attempts {
			if att.Outcome == "success" {
				providerID = att.Provider
			}
		}
	}
	price, _ := s.pairPrice(providerID, req.Model)
	if price.InputUSDPer1M == nil || price.OutputUSDPer1M == nil {
		return
	}
	actual := float64(resp.Usage.PromptTokens)*(*price.InputUSDPer1M/1e6) +
		float64(resp.Usage.CompletionTokens)*(*price.OutputUSDPer1M/1e6)
	if resp.Usage.PromptTokens <= 0 && resp.Usage.CompletionTokens <= 0 {
		return
	}
	w.Header().Set("X-Codvian-Budget-Spent", fmt.Sprintf("$%.6f", actual))
	if actual <= *req.BudgetUSD {
		return
	}
	s.reqBudgets.record(requestBudgetEvent{
		Model:         req.Model,
		Provider:      providerID,
		BudgetUSD:     *req.BudgetUSD,
		Strict:        req.BudgetStrict,
		Outcome:       "overage",
		ActualCostUSD: &actual,
		SpentUSD:      actual,
		PromptTokens:  resp.Usage.PromptTokens,
	})
}

// requestBudgetPrice is the engine's PriceFunc source in tests
// and a shared helper: price one (provider, model) call from a
// PriceBook, per leg (a leg with no configured price
// contributes $0). It mirrors ledger.Prices.Cost except it does
// not require both legs to be known: a prompt-only estimate
// needs only the input leg.
func requestBudgetPrice(book *config.PriceBook, providerID, model string, promptTokens, completionTokens int) float64 {
	if book == nil {
		return 0
	}
	var in, out float64
	if p, ok := book.LookupPair(providerID, model); ok {
		in = priceOrZero(p.InputUSDPer1M)
		out = priceOrZero(p.OutputUSDPer1M)
	}
	if in == 0 || out == 0 {
		if p, ok := book.LookupModel(model); ok {
			if in == 0 {
				in = priceOrZero(p.InputUSDPer1M)
			}
			if out == 0 {
				out = priceOrZero(p.OutputUSDPer1M)
			}
		}
	}
	return float64(promptTokens)*in/1e6 + float64(completionTokens)*out/1e6
}
