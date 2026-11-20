// Per-request USD budget (CHECKLIST 155): the client-supplied
// cap from the X-Codvian-Budget header, enforced inside the
// fallback chain.
//
// Semantics, stated exactly so no mode can be misread:
//
//   - The server refuses BEFORE dialling when the priced
//     prompt-only estimate alone exceeds the budget (HTTP 402,
//     zero provider calls). That refusal happens in both modes:
//     the estimate is a lower bound on the request's true cost
//     (the completion is not in it), so serving would guarantee
//     an overspend on the prompt alone.
//   - In STRICT mode the engine additionally stops the chain
//     before any later attempt whose priced prompt estimate
//     would push cumulative spend over the budget: the attempt
//     is never dialled, the trace records it as
//     "skipped_budget", and the request fails with
//     RequestBudgetExceededError (mapped to HTTP 402 by the
//     server). This is the case the pre-send check cannot see:
//     the pre-send estimate is priced at the model level (the
//     serving provider is not known yet), while the engine
//     prices each attempt at its real (provider, model) pair —
//     so a fallback to a pricier provider can be blocked here
//     even though the first target passed pre-send.
//   - In NON-STRICT mode the chain continues past the budget
//     and the server records the overage honestly (priced actual
//     cost vs the budget) in the request-budget feed — never
//     silently.
//
// Honest limits of the mid-chain accumulator: every attempt
// that precedes a retry failed unbilled (failed attempts record
// Usage{} / $0), so cumulative priced spend before attempt N is
// $0 in today's paths and the gate is effectively the next
// attempt's estimate against the remaining budget. The
// accumulator is kept anyway, so a future billed-then-continue
// path (a success that does not end the chain) is covered by
// the same invariant: spent + next estimate vs the budget.
// Streaming successes likewise record a local-estimate usage
// (cost $0 in the ledger), so a stream's true billed cost is
// invisible to the accumulator — the strict gate on streams is
// the per-attempt prompt estimate only, and the server cannot
// price a stream's actual cost post-hoc. The MCP agent loop's
// follow-up calls each carry the same per-request budget and
// are gated individually; the budget does not shrink across
// loop iterations (see the item-155 CHECKLIST note).
package router

import (
	"errors"
	"fmt"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// RequestBudgetExceededError is the typed refusal when a
// per-request budget stops the chain in strict mode: the next
// attempt's priced prompt estimate, added to the spend already
// made by this request, would exceed the client's budget. The
// blocked attempt was never dialled. The server maps it to HTTP
// 402 (see server.handleEngineError); it is NOT an upstream
// failure (no -32603) and NOT a policy refusal (no -32001).
type RequestBudgetExceededError struct {
	// BudgetUSD is the client's per-request cap.
	BudgetUSD float64
	// SpentUSD is the priced cost of attempts already made by
	// this request ($0 in today's paths — see the package note).
	SpentUSD float64
	// NextEstimateUSD is the priced prompt-only estimate of the
	// blocked attempt (completion not included — a lower bound).
	NextEstimateUSD float64
	// Provider / Model name the blocked attempt's target.
	Provider string
	Model    string
}

func (e *RequestBudgetExceededError) Error() string {
	return fmt.Sprintf(
		"per-request budget exceeded: budget $%.6f, spent $%.6f, next attempt %s/%s estimated prompt cost $%.6f (local estimate, prompt only — completion not included) would exceed it; request refused in strict mode before dialling that provider",
		e.BudgetUSD, e.SpentUSD, e.Provider, e.Model, e.NextEstimateUSD)
}

// IsRequestBudgetExceeded reports whether err is (or wraps) a
// RequestBudgetExceededError.
func IsRequestBudgetExceeded(err error) bool {
	var be *RequestBudgetExceededError
	return errors.As(err, &be)
}

// budgetGate checks, before dialling att, whether the priced
// prompt-only estimate of this attempt would push this request's
// cumulative spend over its per-request budget. It returns
// (blocked, estimateUSD): blocked is true only in strict mode
// when spentUSD+estimate exceeds the budget; estimateUSD is the
// priced prompt estimate ($0 when the attempt cannot be priced
// or the request carries no budget).
//
//   - No budget on the request, or no PriceFunc wired: (false, 0)
//     — the gate cannot price, so it never blocks. Unpriced
//     requests proceed; the server's pre-send check already
//     refused the cases it could price.
//   - The local prompt count is 0 or the count failed, or the
//     priced estimate is $0 (unpriced pair): (false, 0) — an
//     estimate of $0 cannot move the decision, and a failed
//     count must never fail a request.
//   - Non-strict mode: (false, estimate) — the caller proceeds
//     and the server records the overage post-hoc from the
//     priced actual cost.
func (e *Engine) budgetGate(req provider.ChatRequest, att Attempt, spentUSD float64) (blocked bool, estimateUSD float64) {
	if req.BudgetUSD == nil || e.cfg.PriceFunc == nil {
		return false, 0
	}
	prompt := e.localPromptCount(att.Model, e.wireFor(att.ProviderID), req)
	if prompt <= 0 {
		return false, 0
	}
	est := e.cfg.PriceFunc(att.ProviderID, att.Model, prompt, 0)
	if est <= 0 {
		return false, 0
	}
	if req.BudgetStrict && spentUSD+est > *req.BudgetUSD {
		return true, est
	}
	return false, est
}

// budgetExceededError builds the typed strict-mode refusal for a
// blocked attempt, and records the skip in the trace so the
// dashboard's route trace shows why the chain stopped.
func (e *Engine) budgetExceededError(req provider.ChatRequest, att Attempt, spentUSD, estimateUSD float64) *RequestBudgetExceededError {
	return &RequestBudgetExceededError{
		BudgetUSD:       *req.BudgetUSD,
		SpentUSD:        spentUSD,
		NextEstimateUSD: estimateUSD,
		Provider:        att.ProviderID,
		Model:           att.Model,
	}
}

// attemptCost prices one attempt's reported usage with PriceFunc:
// $0 when there is no PriceFunc, no response, or no reported
// usage (failed attempts report none — they billed nothing).
// A local-estimate usage (streaming fallback) prices at $0 in
// the ledger for the same reason it is $0 here: the split is
// unknown, so no cost can be honestly priced from it — the
// completion side is missing entirely.
func (e *Engine) attemptCost(att Attempt, resp *provider.ChatResponse) float64 {
	if e.cfg.PriceFunc == nil || resp == nil {
		return 0
	}
	u := resp.Usage
	if u.LocalEstimate {
		return 0
	}
	if u.PromptTokens <= 0 && u.CompletionTokens <= 0 {
		return 0
	}
	return e.cfg.PriceFunc(att.ProviderID, att.Model, u.PromptTokens, u.CompletionTokens)
}
