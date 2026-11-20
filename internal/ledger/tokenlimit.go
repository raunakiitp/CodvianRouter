// Per-key token limits (M5.3, OmniRoute lib/db/tokenLimits.ts +
// open-sse/services/tokenLimitCounter.ts, verified 2026-11-18).
//
// A token limit caps TOTAL tokens (prompt + completion, as recorded
// in usage_events.total_tokens) inside one calendar reset window,
// scoped to a model, a provider, or globally. The ledger IS the
// counter: window usage is summed from usage_events, so no separate
// counter table exists to drift — the same "ledger is the counter"
// rule as the USD budgets (M5.1) and OmniRoute's seed-from-history.
//
// Pre-request gate (mirror of OmniRoute's checkTokenLimits): a
// limit is breached when the window's used tokens are already at
// or over the limit — the request that would push usage over is
// admitted and the NEXT one is refused. The most-restrictive
// breach wins (smallest remaining, tie-break smaller limit).
// The request's own tokens are not in the ledger yet at gate time;
// the estimate-based TPM axis (keytraffic.go) is the burst guard,
// and the committed-spend reservation (M5.8) closes the parallel-
// request race for these limits too.
package ledger

import (
	"context"
	"fmt"
	"time"
)

// TokenLimitConfig is one token limit as the ledger sees it (the
// config.TokenLimit shape, decoupled so the ledger does not import
// config).
type TokenLimitConfig struct {
	Scope         string
	ScopeValue    string
	Limit         int
	ResetInterval string
	ResetTime     string
	Enabled       bool
}

// TokenLimitBreach is the most-restrictive breached limit for a
// request: the scope that fired, the configured limit, what the
// window already holds, and when the window resets (so the caller
// can set an honest Retry-After).
type TokenLimitBreach struct {
	Scope       string
	ScopeValue  string
	Limit       int
	Used        int
	Remaining   int
	WindowStart time.Time
	NextReset   time.Time
}

func (b *TokenLimitBreach) Error() string {
	scope := "global"
	if b.Scope != "" && b.Scope != "global" {
		scope = b.Scope + " " + b.ScopeValue
	}
	return fmt.Sprintf("key token limit exceeded: %s limit of %d tokens reached (%d used in this window); resets at %s",
		scope, b.Limit, b.Used, b.NextReset.UTC().Format(time.RFC3339))
}

// CheckKeyTokenLimits is the pre-request gate for one key's token
// limits: provider/model name the request's scope ("" = unknown —
// a provider-scoped limit needs a provider, a model-scoped limit
// needs a model; global limits always apply). It returns the
// most-restrictive breach, or nil when every applicable limit
// still has headroom. A ledger error fails closed (the caller
// turns it into a 503 — a limit that cannot be read must not be
// silently waived).
func (l *Ledger) CheckKeyTokenLimits(ctx context.Context, keyID, provider, model string, limits []TokenLimitConfig, now time.Time) (*TokenLimitBreach, error) {
	var worst *TokenLimitBreach
	for _, lim := range limits {
		if !lim.Enabled || lim.Limit <= 0 {
			continue
		}
		applies := false
		switch lim.Scope {
		case "global":
			applies = true
		case "provider":
			applies = provider != "" && provider == lim.ScopeValue
		case "model":
			applies = model != "" && model == lim.ScopeValue
		default:
			continue
		}
		if !applies {
			continue
		}
		interval, ok := NormalizeBudgetResetInterval(lim.ResetInterval)
		if !ok {
			continue
		}
		resetTime, ok := NormalizeBudgetResetTime(lim.ResetTime)
		if !ok {
			continue
		}
		window := BudgetWindowFor(interval, resetTime, now)
		used, err := l.tokenWindowUsed(ctx, keyID, provider, model, lim, window)
		if err != nil {
			return nil, err
		}
		if used < lim.Limit {
			continue
		}
		remaining := lim.Limit - used
		if remaining < 0 {
			remaining = 0
		}
		breach := &TokenLimitBreach{
			Scope:       lim.Scope,
			ScopeValue:  lim.ScopeValue,
			Limit:       lim.Limit,
			Used:        used,
			Remaining:   remaining,
			WindowStart: window.PeriodStart,
			NextReset:   window.NextReset,
		}
		if worst == nil || breach.Remaining < worst.Remaining ||
			(breach.Remaining == worst.Remaining && breach.Limit < worst.Limit) {
			worst = breach
		}
	}
	return worst, nil
}

// tokenWindowUsed sums successful usage (ok = 1: a failed attempt
// consumed no billable tokens) inside the window, filtered by the
// limit's scope — global counts everything, provider counts that
// provider, model counts that model.
func (l *Ledger) tokenWindowUsed(ctx context.Context, keyID, provider, model string, lim TokenLimitConfig, window BudgetWindow) (int, error) {
	query := `SELECT COALESCE(SUM(total_tokens), 0) FROM usage_events
		WHERE gateway_key_id = ? AND ok = 1 AND ts >= ? AND ts < ?`
	args := []any{keyID, window.PeriodStart.UnixMilli(), window.NextReset.UnixMilli()}
	switch lim.Scope {
	case "provider":
		query += ` AND provider = ?`
		args = append(args, provider)
	case "model":
		query += ` AND model = ?`
		args = append(args, model)
	}
	var used int
	if err := l.db.QueryRowContext(ctx, query, args...).Scan(&used); err != nil {
		return 0, fmt.Errorf("ledger: token window usage: %w", err)
	}
	return used, nil
}
