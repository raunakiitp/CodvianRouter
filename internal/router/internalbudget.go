// Internal scoped budgets, engine side (M5.11).
//
// The server pre-call gate (internal/server/internalbudgets.go
// checkInternalBudgets) covers the global / model / pool scopes
// for the model the request NAMES — but the provider is only
// known per attempt, once the engine resolves the chain. So the
// engine re-checks the provider scope before each attempt,
// through Config.InternalBudgetCheck (wired in main.go to the
// ledger's CheckInternalBudgetForProvider): a target whose
// provider is over its internal budget is skipped with trace
// outcome "skipped_internal_budget" and the next target serves;
// when every target is breached the first breach is returned as
// the request error, which the server maps to 402 (see
// handleEngineError). A check error (the ledger cannot be read)
// fails closed: the chain stops with that error — a limit that
// cannot be read must not be silently waived. A nil
// InternalBudgetCheck means no internal budgets are configured
// and the check is skipped.
//
// Unlike the per-key token limits, this check applies to
// ANONYMOUS requests too: the cap is on the operator's total
// spend, not on one key's — the operator's budget does not care
// who called.
//
// The hedged second attempt bypasses this check, the same way it
// bypasses the breaker/cooldown/lockout/token-limit loop-top
// skips today (hedgedChat/hedgedStream dial the second target
// directly): a hedged request may still be served by a breached
// provider when the hedge fires. The ledger still counts its
// spend, so the NEXT request is refused.
package router

import (
	"context"
	"fmt"
)

// InternalBudgetBreach is one breached provider-scoped internal
// budget, as the engine reports it.
type InternalBudgetBreach struct {
	BudgetID  string
	Provider  string
	LimitType string
	Limit     float64
	Used      float64
	Remaining float64
	Period    string
}

func (b *InternalBudgetBreach) Error() string {
	return fmt.Sprintf("internal %s budget %q exhausted for provider %s (%s: used %.2f of %.2f in this %s window)",
		"provider", b.BudgetID, b.Provider, b.LimitType, b.Used, b.Limit, b.Period)
}

// internalBudgetBreach consults Config.InternalBudgetCheck for
// one attempt's provider. Nil check means no breach.
func (e *Engine) internalBudgetBreach(ctx context.Context, att Attempt) (*InternalBudgetBreach, error) {
	if e.cfg.InternalBudgetCheck == nil {
		return nil, nil
	}
	return e.cfg.InternalBudgetCheck(ctx, att.ProviderID)
}
