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
	CapUSD       float64
	SpentUSD     float64
	RemainingUSD float64
}

func (e *BudgetExceededError) Error() string {
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
func (l *Ledger) CheckBudgetOrError(ctx context.Context, keyID string) (remaining float64, warn bool, err error) {
	allowed, remaining, err := l.CheckBudget(ctx, keyID)
	if err != nil {
		return 0, false, err
	}
	if !allowed {
		// Fetch the numbers for the error message. CheckBudget already
		// established the row exists, so ErrNoRows here is unexpected
		// but handled by falling back to the remaining value.
		b, berr := l.GetBudget(ctx, keyID)
		if berr != nil && berr != sql.ErrNoRows {
			return remaining, false, fmt.Errorf("ledger: budget details: %w", berr)
		}
		return remaining, false, &BudgetExceededError{
			KeyID:        keyID,
			CapUSD:       b.CapUSD,
			SpentUSD:     b.SpentUSD,
			RemainingUSD: remaining,
		}
	}
	if b, berr := l.GetBudget(ctx, keyID); berr == nil && b.CapUSD > 0 {
		warn = b.SpentUSD >= SoftWarnThreshold*b.CapUSD
	}
	return remaining, warn, nil
}
