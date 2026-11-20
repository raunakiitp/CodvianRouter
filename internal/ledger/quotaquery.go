// Quota telemetry for one (provider, model) pair, read from the
// ledger's usage_events rows (M3 Phase C data plumbing).
//
// What exists and what does not — read before trusting a number:
//
//   - Spend per pair per window is REAL: usage_events carries
//     provider, model, ts and cost_usd, so priced spend in the
//     rolling 5-hour and 7-day windows is a direct SUM, with the
//     same honesty rules as the per-key budget (keybudget.go):
//     shared rows are excluded (the leader's row is the billed
//     one), failed attempts already record cost $0, and unpriced
//     rows record cost $0 — so the sum is a lower bound on true
//     spend when unpriced usage exists, never an invented cost.
//   - A LIMIT per (provider, model) does NOT exist anywhere in
//     the system. The only limits are per gateway key
//     (key_budgets: rolling 5-hour USD cap; budgets: a cap with
//     an opaque period label) — and a gateway key's spend spans
//     providers, so its limit cannot be attributed to one pair.
//     Utilisation is therefore UnknownUtilisation (-1) for every
//     pair until a per-pair limit source lands. The spend itself
//     is still reported, so a future limit can divide it without
//     re-reading history. Never invent a limit.
//   - ResetAt is the pair's next roll-off: the oldest priced
//     spend in the rolling 5-hour window ages out 5 hours after
//     it was recorded — the same rule KeyBudgetStatus uses for
//     NextRolloff, scoped to the pair instead of the key. The
//     rolling window has no calendar reset, so this is the honest
//     "reset time" for the current spend. Zero when the window
//     holds no priced spend.
//   - Quota EXPIRY does not exist anywhere in the system: no key
//     expiry, no quota-grant expiry, no subscription expiry is
//     recorded in the ledger. There is no ExpiresAt source to
//     read; callers leave it zero (unknown).
//   - Attempt counts do NOT come from here: the engine's live
//     attempt record (router.Engine.ProviderStats) is the source
//     signalsFn already uses for SuccessRate, and the attempt
//     count is filled from that same record in cmd — counting
//     ledger rows here would double-count nothing but would
//     disagree with the live record (restarts, in-memory-only
//     attempts), so this file deliberately has no attempt query.
package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

// WeeklyWindow is the 7-day window Util7d is measured over. The
// 5-hour window is RollingWindow (keybudget.go).
const WeeklyWindow = 7 * 24 * time.Hour

// UnknownUtilisation is the utilisation value when no limit is
// known for the pair: unknown, never 0 (0 would claim the pair
// is completely unspent) and never an invented denominator.
const UnknownUtilisation = -1.0

// PairQuota is the measured quota telemetry for one (provider,
// model) pair. Spent5h / Spent7d are priced spend in the rolling
// 5-hour / 7-day windows (lower bounds when unpriced usage
// exists — see the package docs). Util5h / Util7d are
// UnknownUtilisation (-1): the system holds no per-pair limit.
// ResetAt is when the oldest priced spend in the 5-hour window
// ages out; zero when the window holds no priced spend.
type PairQuota struct {
	Provider string
	Model    string
	Spent5h  float64
	Spent7d  float64
	Util5h   float64
	Util7d   float64
	ResetAt  time.Time
}

// Utilisation returns spent/limit clamped to [0, 1]. A missing,
// zero, negative, NaN or infinite limit returns
// UnknownUtilisation (-1): no limit means no utilisation can be
// computed, and a limit must never be invented to make the
// division work. Spend over the cap clamps to 1.
func Utilisation(spent, limit float64) float64 {
	if limit <= 0 || math.IsNaN(limit) || math.IsInf(limit, 0) {
		return UnknownUtilisation
	}
	if spent <= 0 || math.IsNaN(spent) {
		return 0
	}
	u := spent / limit
	if u > 1 {
		return 1
	}
	return u
}

// SpendForProviderModel returns the priced spend recorded for one
// (provider, model) pair since `since` (inclusive). Shared rows
// are excluded — the leader's row is the billed one — and failed
// or unpriced attempts already record cost $0, so they contribute
// nothing. A pair with no rows spends $0.
func (l *Ledger) SpendForProviderModel(ctx context.Context, provider, model string, since time.Time) (float64, error) {
	var total sql.NullFloat64
	err := l.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_usd), 0) FROM usage_events
		WHERE provider = ? AND model = ? AND ts >= ? AND shared = 0`,
		provider, model, since.UnixMilli()).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("ledger: spend for provider model: %w", err)
	}
	return total.Float64, nil
}

// PairQuota reads one pair's quota telemetry in a single query:
// priced spend in the rolling 5-hour and 7-day windows, and the
// timestamp of the oldest priced spend in the 5-hour window (for
// the roll-off). Util5h and Util7d are always
// UnknownUtilisation: no per-(provider, model) limit exists in
// the system to divide the spend by (see the package docs), so
// the spend is reported as-is and the utilisation stays unknown
// rather than invented. ResetAt is zero when the 5-hour window
// holds no priced spend.
func (l *Ledger) PairQuota(ctx context.Context, provider, model string, now time.Time) (PairQuota, error) {
	pq := PairQuota{
		Provider: provider,
		Model:    model,
		Util5h:   UnknownUtilisation,
		Util7d:   UnknownUtilisation,
	}
	fiveHStart := now.Add(-RollingWindow).UnixMilli()
	sevenDStart := now.Add(-WeeklyWindow).UnixMilli()
	var oldestMs sql.NullInt64
	err := l.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN ts >= ? THEN cost_usd ELSE 0 END), 0),
		  COALESCE(SUM(cost_usd), 0),
		  MIN(CASE WHEN ts >= ? AND cost_usd > 0 THEN ts END)
		FROM usage_events
		WHERE provider = ? AND model = ? AND ts >= ? AND shared = 0`,
		fiveHStart, fiveHStart, provider, model, sevenDStart).Scan(
		&pq.Spent5h, &pq.Spent7d, &oldestMs)
	if err != nil {
		return pq, fmt.Errorf("ledger: pair quota: %w", err)
	}
	if oldestMs.Valid {
		pq.ResetAt = time.UnixMilli(oldestMs.Int64).Add(RollingWindow).UTC()
	}
	return pq, nil
}
