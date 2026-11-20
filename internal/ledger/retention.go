// Package ledger — usage_events retention (G16, Area 11
// "Retention + export", retention half).
//
// usage_events grows by one row per attempt and previously had no
// retention at all — only a manual wipe/reset. Retention is now
// configurable with a 7-day default (matching the reference router's 7-day
// fallback in src/lib/usage/callLogRetention.ts — explicit
// override wins, then the dashboard-persisted value, then the
// 7-day fallback — and their startup cleanup), and expired rows
// are pruned at startup by Open.
//
// DIVERGENCE (deliberate, safer than the reference router): the reference router's
// retention deletes call-log rows only. Ours deletes only the
// usage_events rows too — budgets, reservations, teams and the
// response tree are operator state, not log history, and are
// never pruned by retention. A pruned row's spend stays in
// budgets.spent_usd (the running total is not rewritten):
// retention bounds the log, it does not restate history.
package ledger

import (
	"context"
	"fmt"
	"time"
)

// DefaultUsageRetentionDays is the usage_events retention window
// in days when the operator configures none: 7 days, matching
// the reference router's 7-day fallback.
const DefaultUsageRetentionDays = 7

// retentionDays returns the retention window in force: the
// configured UsageRetentionDays when positive, else the default.
// It never returns <= 0.
func (l *Ledger) retentionDays() int {
	if l != nil && l.UsageRetentionDays > 0 {
		return l.UsageRetentionDays
	}
	return DefaultUsageRetentionDays
}

// SetRetentionDays configures the usage_events retention window in
// days. days <= 0 resets to the default (7 days). It does not
// prune by itself: call PruneExpiredUsage afterwards to apply a
// newly configured window (Open already pruned at the default).
func (l *Ledger) SetRetentionDays(days int) {
	if l == nil {
		return
	}
	if days <= 0 {
		l.UsageRetentionDays = DefaultUsageRetentionDays
		return
	}
	l.UsageRetentionDays = days
}

// PruneExpiredUsage deletes usage_events rows older than the
// retention window (rows with ts strictly before now minus the
// window) and returns how many rows were deleted. now is the
// reference instant (tests pass a fixed time; production passes
// time.Now()). It is idempotent and safe to call concurrently
// with recording: the DELETE is one statement, and a row
// recorded mid-prune carries a fresh ts that is never expired.
func (l *Ledger) PruneExpiredUsage(ctx context.Context, now time.Time) (int64, error) {
	if l == nil || l.db == nil {
		return 0, fmt.Errorf("ledger: prune expired usage: ledger not open")
	}
	cutoff := now.Add(-time.Duration(l.retentionDays()) * 24 * time.Hour).UnixMilli()
	res, err := l.db.ExecContext(ctx, `DELETE FROM usage_events WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("ledger: prune expired usage: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ledger: prune expired usage count: %w", err)
	}
	return n, nil
}
