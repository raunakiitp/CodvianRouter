// Fail-closed budgeting ("never bill me" mode) for the ledger.
//
// When Ledger.FailClosed is on, an exhausted budget is a hard refusal:
// the caller must not call the provider and must return a 402-style
// error to the client. When it is off, the budget is a soft cap —
// requests continue, but the caller should surface a warning.
//
// The 80% soft warning fires regardless of FailClosed: once a key has
// spent >= 80% of its cap, callers should warn the operator/user that
// the budget is nearly gone.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// SoftWarnThreshold is the fraction of the cap at which the caller
// should warn that the budget is nearly exhausted.
const SoftWarnThreshold = 0.80

// BudgetExceededError is the typed refusal returned when a fail-closed
// budget is exhausted. Callers translate it into a 402 Payment Required
// response (with this message) instead of calling any provider, so no
// further spend can happen.
type BudgetExceededError struct {
	KeyID        string
	Window       string // M5.1: the window that refused ("5h", "daily", "weekly", "monthly"); "" = the global cap (pre-M5.1 callers)
	CapUSD       float64
	SpentUSD     float64
	RemainingUSD float64
}

func (e *BudgetExceededError) Error() string {
	if e.Window != "" {
		return fmt.Sprintf(
			"budget exceeded for key %q (%s window): spent $%.4f of $%.4f cap (remaining $%.4f); "+
				"request refused — enable a higher cap or disable fail-closed mode to continue",
			e.KeyID, e.Window, e.SpentUSD, e.CapUSD, e.RemainingUSD)
	}
	return fmt.Sprintf(
		"budget exceeded for key %q: spent $%.4f of $%.4f cap (remaining $%.4f); "+
			"request refused — enable a higher cap or disable fail-closed mode to continue",
		e.KeyID, e.SpentUSD, e.CapUSD, e.RemainingUSD)
}

// IsBudgetExceeded reports whether err is (or wraps) a
// BudgetExceededError.
func IsBudgetExceeded(err error) bool {
	var be *BudgetExceededError
	return errors.As(err, &be)
}

// CheckBudgetOrError is the fail-closed guard to call before routing a
// request. It returns:
//
//	remaining — cap minus spent (+Inf when the key has no budget)
//	warn      — true when spent >= 80% of a positive cap
//	err       — *BudgetExceededError when FailClosed is on and the
//	            budget is exhausted (spent >= cap, including cap == 0);
//	            nil otherwise.
//
// When err is non-nil the caller must refuse the request (HTTP 402)
// and must not call any provider.
// checkBudgetDetailed is the single-read implementation behind
// CheckBudget, CheckBudgetOrError and CheckBudgetForEstimate: it
// reads the budget row ONCE and derives allowed / remaining /
// warn inputs from that one read. The old code read the row twice
// per request — CheckBudgetOrError called CheckBudget (one
// GetBudget) and then GetBudget again for the 80% warning, and
// CheckBudgetForEstimate did the same pair — and the CPU profile
// (2026-11-18 bench, CHECKLIST 249-250) put GetBudget at 4.07 s
// cum (11% of all router CPU), half of it the second read. The
// returned Budget is the row that decided allowed/remaining, so
// callers never re-read it; found=false means no row (unlimited),
// in which case b is the zero Budget. err is wrapped exactly as
// CheckBudget wrapped it, so callers see the same error text.
// spent counts committed spend, not settled spend alone: the
// returned Budget's SpentUSD is the settled spend PLUS the key's
// outstanding reservations (M5.8), so N parallel requests cannot
// all read the pre-spend figure and jointly exceed the cap — the
// reservation is the fix, this read is its visibility (M5.14).
// The read and the reservation sum run under one reserveMu hold,
// so no ReserveSpend insert can interleave between them. Callers
// must never call this while already holding reserveMu.
func (l *Ledger) checkBudgetDetailed(ctx context.Context, keyID string) (allowed bool, remaining float64, b Budget, found bool, err error) {
	l.reserveMu.Lock()
	defer l.reserveMu.Unlock()
	b, err = l.GetBudget(ctx, keyID)
	if err != nil {
		if err == sql.ErrNoRows {
			return true, math.Inf(1), Budget{}, false, nil
		}
		return false, 0, Budget{}, false, fmt.Errorf("ledger: check budget: %w", err)
	}
	reserved, err := l.reservedSpendLocked(ctx, keyID, time.Now())
	if err != nil {
		return false, 0, Budget{}, false, fmt.Errorf("ledger: check budget reserved: %w", err)
	}
	b.SpentUSD += reserved
	remaining = b.CapUSD - b.SpentUSD
	if b.SpentUSD >= b.CapUSD && l.FailClosed {
		return false, remaining, b, true, nil
	}
	return true, remaining, b, true, nil
}

func (l *Ledger) CheckBudgetOrError(ctx context.Context, keyID string) (remaining float64, warn bool, err error) {
	allowed, remaining, b, found, err := l.checkBudgetDetailed(ctx, keyID)
	if err != nil {
		return 0, false, err
	}
	if !allowed {
		return remaining, false, &BudgetExceededError{
			KeyID:        keyID,
			CapUSD:       b.CapUSD,
			SpentUSD:     b.SpentUSD,
			RemainingUSD: remaining,
		}
	}
	if found && b.CapUSD > 0 {
		warn = b.SpentUSD >= SoftWarnThreshold*b.CapUSD
	}
	return remaining, warn, nil
}

// CheckBudgetForEstimate is the pre-send budget guard: it extends
// CheckBudgetOrError with the request's ESTIMATED prompt cost
// (local token count × the pair's input price, computed by the
// caller). It answers the question "would this request, on its
// prompt alone, push the key past its cap?" — the completion is
// not in the estimate, so the check is a lower bound on the
// request's true cost, and the error says so.
//
//   - No budget row in EITHER store: unlimited, (remaining=+Inf,
//     warn=false, nil) whatever the estimate is. The two stores
//     are the legacy per-key cap (SetBudget) and the rolling 5-hour
//     key budget (SetKeyBudget, item 139 — the one the config
//     file's key_budgets block populates at startup). Either store
//     can refuse; the tightest remaining wins. A config-driven cap
//     that only lived in the rolling store used to be invisible
//     here, so a prompt that alone exceeded it was never refused
//     pre-send — that gap is closed by consulting both.
//   - FailClosed off: the request is allowed; warn is true once
//     spent+estimate reaches 80% of a cap (or spent alone
//     already does).
//   - FailClosed on and spent+estimate > cap: a
//     *BudgetExceededError naming the estimated prompt cost, so the
//     caller refuses with 402 BEFORE dialling any provider — no
//     spend can happen for a request that was never sent.
//   - estimatedCostUSD <= 0 (unpriced pair, or a zero estimate):
//     the estimate cannot move the decision, so this degrades to
//     CheckBudgetOrError — refuse only when already exhausted.
//
// The estimate is a local count, not a bill: the check never
// records spend, and a refusal here means "the prompt alone would
// exceed the cap", never "the provider charged this much".
func (l *Ledger) CheckBudgetForEstimate(ctx context.Context, keyID string, estimatedCostUSD float64) (remaining float64, warn bool, err error) {
	if estimatedCostUSD <= 0 {
		return l.CheckBudgetOrError(ctx, keyID)
	}
	// One budget-row read (see checkBudgetDetailed): the old
	// code called CheckBudget and then GetBudget again for the
	// same row — two SQLite round-trips per pre-send check.
	allowed, remaining, b, found, err := l.checkBudgetDetailed(ctx, keyID)
	if err != nil {
		return 0, false, err
	}
	if found {
		if !allowed {
			return remaining, false, &BudgetExceededError{
				KeyID:        keyID,
				CapUSD:       b.CapUSD,
				SpentUSD:     b.SpentUSD,
				RemainingUSD: remaining,
			}
		}
		if b.CapUSD > 0 {
			warn = b.SpentUSD+estimatedCostUSD >= SoftWarnThreshold*b.CapUSD
			if l.FailClosed && b.SpentUSD+estimatedCostUSD > b.CapUSD {
				return remaining, warn, &BudgetExceededError{
					KeyID:        keyID,
					CapUSD:       b.CapUSD,
					SpentUSD:     b.SpentUSD,
					RemainingUSD: remaining,
				}
			}
		}
	}
	// Rolling 5-hour key budget (item 139): same question, window
	// spend instead of the legacy running total. The estimate is
	// priced (the caller had a price to multiply), so the unpriced
	// policy does not apply to it — only the cap does.
	st, serr := l.KeyBudgetStatus(ctx, keyID, time.Now())
	if serr != nil {
		return remaining, warn, fmt.Errorf("ledger: key budget status: %w", serr)
	}
	if st.HasBudget {
		rollingRemaining := st.LimitUSD - st.SpentUSD
		if rollingRemaining < remaining {
			remaining = rollingRemaining
		}
		if st.SpentUSD+estimatedCostUSD >= SoftWarnThreshold*st.LimitUSD {
			warn = true
		}
		if l.FailClosed && st.SpentUSD+estimatedCostUSD > st.LimitUSD {
			return remaining, warn, &BudgetExceededError{
				KeyID:        keyID,
				CapUSD:       st.LimitUSD,
				SpentUSD:     st.SpentUSD,
				RemainingUSD: rollingRemaining,
			}
		}
	}
	return remaining, warn, nil
}
