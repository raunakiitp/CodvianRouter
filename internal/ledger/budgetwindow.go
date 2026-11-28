// Budget windows (M5.1, the sector baseline costRules.ts getBudgetWindow,
// verified 2026-11-18).
//
// A per-key budget window is a calendar period in UTC whose boundary
// is shifted by the operator's reset time: resetInterval selects the
// period (daily / weekly / monthly) and resetTime ("HH:MM", default
// "00:00") is the UTC time of day the period rolls over.
//
//   - daily: [today resetTime, tomorrow resetTime)
//   - weekly: [Monday resetTime, next Monday resetTime)
//   - monthly: [1st resetTime, next 1st resetTime)
//
// The window containing now is [PeriodStart, NextReset). All math is
// UTC: the ledger stores usage timestamps in UTC milliseconds, so a
// window boundary is directly comparable to a usage_events.ts value
// with no timezone conversion anywhere in the path.
package ledger

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// BudgetResetInterval is one budget reset cadence.
type BudgetResetInterval string

const (
	// BudgetResetDaily rolls the window over every day at resetTime.
	BudgetResetDaily BudgetResetInterval = "daily"
	// BudgetResetWeekly rolls the window over every Monday at resetTime.
	BudgetResetWeekly BudgetResetInterval = "weekly"
	// BudgetResetMonthly rolls the window over on the 1st at resetTime.
	BudgetResetMonthly BudgetResetInterval = "monthly"
)

// NormalizeBudgetResetInterval parses an interval name: "" means the
// default (daily). ok=false names an unknown interval.
func NormalizeBudgetResetInterval(s string) (BudgetResetInterval, bool) {
	switch s {
	case "", string(BudgetResetDaily):
		return BudgetResetDaily, true
	case string(BudgetResetWeekly):
		return BudgetResetWeekly, true
	case string(BudgetResetMonthly):
		return BudgetResetMonthly, true
	default:
		return "", false
	}
}

// NormalizeBudgetResetTime parses an "HH:MM" UTC reset time: "" means
// the default ("00:00"). ok=false names a malformed time (wrong
// shape, hour > 23, minute > 59).
func NormalizeBudgetResetTime(s string) (string, bool) {
	if s == "" {
		return "00:00", true
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return "", false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return "", false
	}
	return s, true
}

// BudgetWindow is the calendar period containing now for one reset
// interval + reset time: [PeriodStart, NextReset), both UTC.
type BudgetWindow struct {
	PeriodStart time.Time
	NextReset   time.Time
}

// BudgetWindowFor returns the window containing now for interval +
// resetTime. resetTime must already be normalized ("" is treated as
// "00:00"); an unknown interval falls back to daily rather than
// failing — callers validate at write time, and the window math
// must never refuse to answer for a stored row.
func BudgetWindowFor(interval BudgetResetInterval, resetTime string, now time.Time) BudgetWindow {
	hours, minutes := budgetResetTimeParts(resetTime)
	now = now.UTC()
	y, mo, d := now.Date()

	switch interval {
	case BudgetResetWeekly:
		// Monday = 0 .. Sunday = 6 (Go: Sunday = 0).
		daysSinceMonday := (int(now.Weekday()) + 6) % 7
		thisWeekReset := time.Date(y, mo, d-daysSinceMonday, hours, minutes, 0, 0, time.UTC)
		if !now.Before(thisWeekReset) {
			return BudgetWindow{
				PeriodStart: thisWeekReset,
				NextReset:   time.Date(y, mo, d-daysSinceMonday+7, hours, minutes, 0, 0, time.UTC),
			}
		}
		return BudgetWindow{
			PeriodStart: time.Date(y, mo, d-daysSinceMonday-7, hours, minutes, 0, 0, time.UTC),
			NextReset:   thisWeekReset,
		}
	case BudgetResetMonthly:
		thisMonthReset := time.Date(y, mo, 1, hours, minutes, 0, 0, time.UTC)
		if !now.Before(thisMonthReset) {
			return BudgetWindow{
				PeriodStart: thisMonthReset,
				NextReset:   time.Date(y, mo+1, 1, hours, minutes, 0, 0, time.UTC),
			}
		}
		return BudgetWindow{
			PeriodStart: time.Date(y, mo-1, 1, hours, minutes, 0, 0, time.UTC),
			NextReset:   thisMonthReset,
		}
	default: // daily (and unknown intervals)
		todayReset := time.Date(y, mo, d, hours, minutes, 0, 0, time.UTC)
		if !now.Before(todayReset) {
			return BudgetWindow{
				PeriodStart: todayReset,
				NextReset:   time.Date(y, mo, d+1, hours, minutes, 0, 0, time.UTC),
			}
		}
		return BudgetWindow{
			PeriodStart: time.Date(y, mo, d-1, hours, minutes, 0, 0, time.UTC),
			NextReset:   todayReset,
		}
	}
}

// budgetResetTimeParts splits a normalized "HH:MM" reset time. A
// malformed value degrades to midnight rather than failing — the
// window math must never refuse to answer for a stored row.
func budgetResetTimeParts(resetTime string) (hours, minutes int) {
	parts := strings.Split(resetTime, ":")
	if len(parts) != 2 {
		return 0, 0
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0
	}
	return h, m
}

// budgetWindowSpent sums priced spend for one key in [start, end)
// from usage_events: the ledger is the counter, exactly like the
// 5-hour rolling window — failed attempts and shared rows already
// record $0, so no extra filter is needed.
func (l *Ledger) budgetWindowSpent(ctx context.Context, keyID string, start, end time.Time) (float64, error) {
	var spent float64
	err := l.rdb.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM usage_events
		WHERE gateway_key_id = ? AND ts >= ? AND ts < ? AND shared = 0`,
		keyID, start.UnixMilli(), end.UnixMilli()).Scan(&spent)
	if err != nil {
		return 0, fmt.Errorf("ledger: budget window spend: %w", err)
	}
	return spent, nil
}
