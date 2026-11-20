// M5.1 windowed per-key USD budgets: calendar daily/weekly/
// monthly caps + resetInterval/resetTime selector + unpriced-month
// fail-closed, enforced in CheckKeyBudget and reported in
// KeyBudgetStatus.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fptr(v float64) *float64 { return &v }

// windowedKey is a KeyBudget with a $1,000 5-hour cap (never the
// binding constraint in these tests) and the given window config.
func windowedKey(key string, interval BudgetResetInterval, daily, weekly, monthly *float64) KeyBudget {
	return KeyBudget{
		KeyID:            key,
		LimitUSD:         1000,
		UnpricedPolicy:   UnpricedFailClosed,
		DailyLimitUSD:    daily,
		WeeklyLimitUSD:   weekly,
		MonthlyLimitUSD:  monthly,
		WarningThreshold: 0.8,
		ResetInterval:    interval,
		ResetTime:        "00:00",
	}
}

// TestKeyBudgetWindowedEnforcement: the active window gates the
// request; spend outside the current period does not count.
func TestKeyBudgetWindowedEnforcement(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	// Wednesday 2026-11-18 12:00 UTC.
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	priced := func(string) bool { return true }

	// Daily $5: $4 today passes, $2 more (total $6) refuses.
	daily := windowedKey("k-daily", BudgetResetDaily, fptr(5), nil, nil)
	if err := l.SetKeyBudgetConfig(ctx, daily); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-daily", "p", "m", 4.00, 1, 1, 1, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-daily", []string{"m"}, priced, now); err != nil {
		t.Fatalf("check at $4 of $5 daily: %v", err)
	}
	seedUsageRow(t, l, now.Add(-30*time.Minute), "k-daily", "p", "m", 2.00, 1, 1, 1, 0, 10)
	err := l.CheckKeyBudget(ctx, "k-daily", []string{"m"}, priced, now)
	var be *BudgetExceededError
	if !errors.As(err, &be) || be.Window != "daily" || be.CapUSD != 5 || be.SpentUSD != 6 {
		t.Fatalf("check at $6 of $5 daily = %v, want BudgetExceededError daily $5/$6", err)
	}

	// Yesterday's $100 does not count toward today.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-yday", BudgetResetDaily, fptr(5), nil, nil)); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-26*time.Hour), "k-yday", "p", "m", 100.00, 1, 1, 1, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-yday", []string{"m"}, priced, now); err != nil {
		t.Fatalf("yesterday's $100 must not count toward today: %v", err)
	}

	// Weekly $10: last week's $50 does not count; this week's $11 refuses.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-week", BudgetResetWeekly, nil, fptr(10), nil)); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-8*24*time.Hour), "k-week", "p", "m", 50.00, 1, 1, 1, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-week", []string{"m"}, priced, now); err != nil {
		t.Fatalf("last week's $50 must not count: %v", err)
	}
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-week", "p", "m", 11.00, 1, 1, 1, 0, 10)
	err = l.CheckKeyBudget(ctx, "k-week", []string{"m"}, priced, now)
	if !errors.As(err, &be) || be.Window != "weekly" || be.SpentUSD != 11 {
		t.Fatalf("check at $11 of $10 weekly = %v, want BudgetExceededError weekly", err)
	}

	// Monthly $20: October's $90 does not count; November's $21 refuses.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-month", BudgetResetMonthly, nil, nil, fptr(20))); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC), "k-month", "p", "m", 90.00, 1, 1, 1, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-month", []string{"m"}, priced, now); err != nil {
		t.Fatalf("october's $90 must not count toward november: %v", err)
	}
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-month", "p", "m", 21.00, 1, 1, 1, 0, 10)
	err = l.CheckKeyBudget(ctx, "k-month", []string{"m"}, priced, now)
	if !errors.As(err, &be) || be.Window != "monthly" || be.SpentUSD != 21 {
		t.Fatalf("check at $21 of $20 monthly = %v, want BudgetExceededError monthly", err)
	}
}

// TestKeyBudgetActiveWindowFallback: the active limit follows
// the reference router's getActiveBudgetLimit — monthly with no monthly limit
// falls back to the daily limit; weekly with no weekly limit falls
// back to daily; daily with no daily limit means no calendar cap
// is active FOR DISPLAY. B2: the fallback is display-only —
// enforcement gates on EVERY set window, so a $50 monthly cap
// refuses a $60 month even when the interval is weekly and the
// reported active window is "5h".
func TestKeyBudgetActiveWindowFallback(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	priced := func(string) bool { return true }
	var be *BudgetExceededError

	// Interval monthly, only daily $5 set: the daily limit gates.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-fb1", BudgetResetMonthly, fptr(5), nil, nil)); err != nil {
		t.Fatalf("set: %v", err)
	}
	st, err := l.KeyBudgetStatus(ctx, "k-fb1", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.ActiveWindow != "daily" || st.ActiveLimitUSD == nil || *st.ActiveLimitUSD != 5 {
		t.Fatalf("active = %q/%v, want daily/$5", st.ActiveWindow, st.ActiveLimitUSD)
	}
	// Interval weekly, only monthly $50 set: NO calendar cap is
	// active for display (weekly falls back to daily, which is
	// unset — the monthly limit does not gate a weekly interval
	// in the status view), but B2 enforcement still gates on the
	// set monthly cap: $60 in the month refuses.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-fb2", BudgetResetWeekly, nil, nil, fptr(50))); err != nil {
		t.Fatalf("set: %v", err)
	}
	st, err = l.KeyBudgetStatus(ctx, "k-fb2", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.ActiveWindow != "5h" || st.ActiveLimitUSD != nil {
		t.Fatalf("active = %q/%v, want 5h/nil (monthly does not gate a weekly interval)", st.ActiveWindow, st.ActiveLimitUSD)
	}
	if st.Monthly.SpentUSD != 0 || st.Monthly.LimitUSD == nil || *st.Monthly.LimitUSD != 50 {
		t.Fatalf("monthly status = %+v, want limit $50 reported", st.Monthly)
	}
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-fb2", "p", "m", 60.00, 1, 1, 1, 0, 10)
	err = l.CheckKeyBudget(ctx, "k-fb2", []string{"m"}, priced, now)
	if !errors.As(err, &be) || be.Window != "monthly" || be.CapUSD != 50 || be.SpentUSD != 60 {
		t.Fatalf("check at $60 of $50 monthly (interval weekly) = %v, want BudgetExceededError monthly $50/$60", err)
	}
	if st, err := l.KeyBudgetStatus(ctx, "k-fb2", now); err != nil {
		t.Fatalf("status: %v", err)
	} else if !st.AtLimit || !st.Monthly.AtLimit {
		t.Fatalf("status at limit = %v monthly at limit = %v, want both true", st.AtLimit, st.Monthly.AtLimit)
	}
}

// TestKeyBudgetResetTimeShiftsWindow: with resetTime 09:30, spend
// at 08:00 belongs to yesterday's window and does not count.
func TestKeyBudgetResetTimeShiftsWindow(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	priced := func(string) bool { return true }
	kb := windowedKey("k-rt", BudgetResetDaily, fptr(5), nil, nil)
	kb.ResetTime = "09:30"
	if err := l.SetKeyBudgetConfig(ctx, kb); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, time.Date(2026, time.November, 18, 8, 0, 0, 0, time.UTC), "k-rt", "p", "m", 100.00, 1, 1, 1, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-rt", []string{"m"}, priced, now); err != nil {
		t.Fatalf("08:00 spend belongs to yesterday's 09:30 window: %v", err)
	}
	st, err := l.KeyBudgetStatus(ctx, "k-rt", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Daily.PeriodStart.Equal(time.Date(2026, time.November, 18, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("daily period start = %v, want Nov 18 09:30", st.Daily.PeriodStart)
	}
	if st.Daily.SpentUSD != 0 {
		t.Errorf("daily spent = $%.2f, want $0 (the $100 is in yesterday's window)", st.Daily.SpentUSD)
	}
}

// TestKeyBudgetMonthClosedByUnpriced: a monthly cap + any unpriced
// usage this month closes the month for ALL models under
// fail_closed; count_as_zero opts out; no monthly cap means no
// month to close.
func TestKeyBudgetMonthClosedByUnpriced(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	priced := func(string) bool { return true }

	kb := windowedKey("k-closed", BudgetResetMonthly, nil, nil, fptr(100))
	if err := l.SetKeyBudgetConfig(ctx, kb); err != nil {
		t.Fatalf("set: %v", err)
	}
	// One unpriced row this month (price_known=0), priced spend $1.
	seedUsageRow(t, l, now.Add(-2*time.Hour), "k-closed", "p", "ghost-model", 0, 1, 1, 0, 0, 10)
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-closed", "p", "m", 1.00, 1, 1, 1, 0, 10)
	err := l.CheckKeyBudget(ctx, "k-closed", []string{"m"}, priced, now)
	var ue *UnpricedUsageError
	if !errors.As(err, &ue) || !ue.MonthClosed {
		t.Fatalf("check = %v, want month-closed UnpricedUsageError", err)
	}
	if len(ue.Models) != 1 || ue.Models[0] != "ghost-model" {
		t.Errorf("month-closed models = %v, want [ghost-model]", ue.Models)
	}

	// count_as_zero opts out: same usage, different policy.
	kb2 := windowedKey("k-open", BudgetResetMonthly, nil, nil, fptr(100))
	kb2.UnpricedPolicy = UnpricedCountAsZero
	if err := l.SetKeyBudgetConfig(ctx, kb2); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-2*time.Hour), "k-open", "p", "ghost-model", 0, 1, 1, 0, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-open", []string{"m"}, priced, now); err != nil {
		t.Fatalf("count_as_zero must not close the month: %v", err)
	}

	// No calendar cap at all: unpriced usage this month closes nothing.
	kb3 := windowedKey("k-nocap", BudgetResetDaily, nil, nil, nil)
	if err := l.SetKeyBudgetConfig(ctx, kb3); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-2*time.Hour), "k-nocap", "p", "ghost-model", 0, 1, 1, 0, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-nocap", []string{"m"}, priced, now); err != nil {
		t.Fatalf("no calendar cap must not close any period: %v", err)
	}

	// B2: a daily cap closes the DAY the same way the monthly cap
	// closes the month — unpriced usage today refuses every model
	// until tomorrow, with Window "daily" (not MonthClosed).
	kb5 := windowedKey("k-dayclosed", BudgetResetDaily, fptr(100), nil, nil)
	if err := l.SetKeyBudgetConfig(ctx, kb5); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-2*time.Hour), "k-dayclosed", "p", "ghost-model", 0, 1, 1, 0, 0, 10)
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-dayclosed", "p", "m", 1.00, 1, 1, 1, 0, 10)
	err = l.CheckKeyBudget(ctx, "k-dayclosed", []string{"m"}, priced, now)
	if !errors.As(err, &ue) || ue.MonthClosed || ue.Window != "daily" {
		t.Fatalf("check = %v, want day-closed UnpricedUsageError window=daily", err)
	}
	if len(ue.Models) != 1 || ue.Models[0] != "ghost-model" {
		t.Errorf("day-closed models = %v, want [ghost-model]", ue.Models)
	}
	if !strings.Contains(err.Error(), "daily") {
		t.Errorf("day-closed error = %q, want it to name the daily window", err.Error())
	}

	// B2: a weekly cap closes the WEEK — unpriced usage yesterday
	// (same Mon–Sun week) refuses, with Window "weekly".
	kb6 := windowedKey("k-weekclosed", BudgetResetWeekly, nil, fptr(100), nil)
	if err := l.SetKeyBudgetConfig(ctx, kb6); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-26*time.Hour), "k-weekclosed", "p", "ghost-model", 0, 1, 1, 0, 0, 10)
	err = l.CheckKeyBudget(ctx, "k-weekclosed", []string{"m"}, priced, now)
	if !errors.As(err, &ue) || ue.MonthClosed || ue.Window != "weekly" {
		t.Fatalf("check = %v, want week-closed UnpricedUsageError window=weekly", err)
	}

	// count_as_zero opts out of the daily close too.
	kb7 := windowedKey("k-dayopen", BudgetResetDaily, fptr(100), nil, nil)
	kb7.UnpricedPolicy = UnpricedCountAsZero
	if err := l.SetKeyBudgetConfig(ctx, kb7); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-2*time.Hour), "k-dayopen", "p", "ghost-model", 0, 1, 1, 0, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-dayopen", []string{"m"}, priced, now); err != nil {
		t.Fatalf("count_as_zero must not close the day: %v", err)
	}

	// Last month's unpriced usage does not close this month.
	kb4 := windowedKey("k-lastmo", BudgetResetMonthly, nil, nil, fptr(100))
	if err := l.SetKeyBudgetConfig(ctx, kb4); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC), "k-lastmo", "p", "ghost-model", 0, 1, 1, 0, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-lastmo", []string{"m"}, priced, now); err != nil {
		t.Fatalf("october unpriced usage must not close november: %v", err)
	}
}

// TestKeyBudgetWarningThreshold: status reports WarningReached once
// the active window's spend reaches 80% of its limit.
func TestKeyBudgetWarningThreshold(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-warn", BudgetResetDaily, fptr(10), nil, nil)); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-warn", "p", "m", 7.99, 1, 1, 1, 0, 10)
	st, err := l.KeyBudgetStatus(ctx, "k-warn", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.WarningReached {
		t.Error("warning reached at $7.99 of $10 (want false below 80%)")
	}
	seedUsageRow(t, l, now.Add(-30*time.Minute), "k-warn", "p", "m", 0.01, 1, 1, 1, 0, 10)
	st, err = l.KeyBudgetStatus(ctx, "k-warn", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.WarningReached {
		t.Error("warning not reached at $8.00 of $10 (want true at 80%)")
	}
	if st.ActiveWindow != "daily" || st.Daily.SpentUSD != 8.00 {
		t.Errorf("status = active %q daily $%.2f, want daily $8.00", st.ActiveWindow, st.Daily.SpentUSD)
	}
}

// TestKeyBudgetWarningNonActiveWindow: WarningReached ORs across
// every set window (B2 nit 1), like AtLimit — a non-active window
// at/above the warning threshold warns even when the active window
// is far below it, so the dashboard warns before the gate hard-
// refuses at 100%.
func TestKeyBudgetWarningNonActiveWindow(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	// Wednesday 2026-11-18 12:00 UTC; active window is daily.
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)

	// Monthly $10 at 90% ($9 yesterday — same month, other day),
	// daily $100 untouched: the active (daily) window is at 0%,
	// the non-active monthly window is past the 80% threshold.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-warn-mo", BudgetResetDaily, fptr(100), nil, fptr(10))); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC), "k-warn-mo", "p", "m", 9.00, 1, 1, 1, 0, 10)
	st, err := l.KeyBudgetStatus(ctx, "k-warn-mo", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.ActiveWindow != "daily" {
		t.Errorf("active = %q, want daily (monthly must be the non-active window here)", st.ActiveWindow)
	}
	if st.Daily.SpentUSD != 0 || st.Monthly.SpentUSD != 9.00 {
		t.Errorf("spent = daily $%.2f monthly $%.2f, want $0 / $9.00", st.Daily.SpentUSD, st.Monthly.SpentUSD)
	}
	if !st.WarningReached {
		t.Error("WarningReached = false with monthly at $9 of $10 (90% >= 80% threshold), want true")
	}
	if st.AtLimit {
		t.Error("AtLimit = true at $9 of $10 monthly, want false (warning, not refusal)")
	}

	// Same setup below the threshold: monthly $7 of $10 (70%)
	// must NOT warn — the OR must not fire on spend alone.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-warn-mo-low", BudgetResetDaily, fptr(100), nil, fptr(10))); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC), "k-warn-mo-low", "p", "m", 7.00, 1, 1, 1, 0, 10)
	st, err = l.KeyBudgetStatus(ctx, "k-warn-mo-low", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.WarningReached {
		t.Error("WarningReached = true with monthly at $7 of $10 (70% < 80% threshold), want false")
	}
}

// TestKeyBudgetWarningThresholdZeroNeverWarns: a stored
// WarningThreshold of 0 means "never warn" (see the
// SetKeyBudgetConfig doc), not "warn at $0" — a daily $10 key
// at $9 must NOT warn. Negative control: the same setup with
// threshold 0.8 DOES warn at $9 (90% >= 80%).
func TestKeyBudgetWarningThresholdZeroNeverWarns(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)

	neverWarn := windowedKey("k-warn-zero", BudgetResetDaily, fptr(10), nil, nil)
	neverWarn.WarningThreshold = 0
	if err := l.SetKeyBudgetConfig(ctx, neverWarn); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-warn-zero", "p", "m", 9.00, 1, 1, 1, 0, 10)
	st, err := l.KeyBudgetStatus(ctx, "k-warn-zero", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Daily.SpentUSD != 9.00 {
		t.Fatalf("spent = $%.2f, want $9.00 (test setup broken)", st.Daily.SpentUSD)
	}
	if st.WarningReached {
		t.Error("WarningReached = true with threshold 0 at $9 of $10, want false (0 = never warn)")
	}
	if st.AtLimit {
		t.Error("AtLimit = true at $9 of $10, want false (warning only, not refusal)")
	}

	// Negative control: threshold 0.8, same $9 of $10 — must warn.
	if err := l.SetKeyBudgetConfig(ctx, windowedKey("k-warn-ctrl", BudgetResetDaily, fptr(10), nil, nil)); err != nil {
		t.Fatalf("set control: %v", err)
	}
	seedUsageRow(t, l, now.Add(-1*time.Hour), "k-warn-ctrl", "p", "m", 9.00, 1, 1, 1, 0, 10)
	st, err = l.KeyBudgetStatus(ctx, "k-warn-ctrl", now)
	if err != nil {
		t.Fatalf("status control: %v", err)
	}
	if !st.WarningReached {
		t.Error("WarningReached = false with threshold 0.8 at $9 of $10, want true (negative control)")
	}
}

// TestKeyBudgetResetTimeShiftsWeeklyWindow: with resetTime 09:30,
// spend on Monday 08:00 belongs to the PREVIOUS week (the current
// week starts Monday 09:30), not the current one — the Monday
// anchor and the shift compose the same way the daily shift does.
// Monday 2026-11-16, now Wednesday 2026-11-18 12:00 UTC.
func TestKeyBudgetResetTimeShiftsWeeklyWindow(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	priced := func(string) bool { return true }
	kb := windowedKey("k-rt-week", BudgetResetWeekly, nil, fptr(5), nil)
	kb.ResetTime = "09:30"
	if err := l.SetKeyBudgetConfig(ctx, kb); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, time.Date(2026, time.November, 16, 8, 0, 0, 0, time.UTC), "k-rt-week", "p", "m", 100.00, 1, 1, 1, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-rt-week", []string{"m"}, priced, now); err != nil {
		t.Fatalf("Monday 08:00 spend belongs to last week's 09:30 window: %v", err)
	}
	st, err := l.KeyBudgetStatus(ctx, "k-rt-week", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Weekly.PeriodStart.Equal(time.Date(2026, time.November, 16, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("weekly period start = %v, want Mon Nov 16 09:30", st.Weekly.PeriodStart)
	}
	if st.Weekly.SpentUSD != 0 {
		t.Errorf("weekly spent = $%.2f, want $0 (the $100 is in last week's window)", st.Weekly.SpentUSD)
	}

	// Negative control: the same Monday 08:00 spend with resetTime
	// 00:00 IS in the current week, so the $5 weekly cap refuses.
	kb2 := windowedKey("k-rt-week-0000", BudgetResetWeekly, nil, fptr(5), nil)
	if err := l.SetKeyBudgetConfig(ctx, kb2); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, time.Date(2026, time.November, 16, 8, 0, 0, 0, time.UTC), "k-rt-week-0000", "p", "m", 100.00, 1, 1, 1, 0, 10)
	err = l.CheckKeyBudget(ctx, "k-rt-week-0000", []string{"m"}, priced, now)
	var be *BudgetExceededError
	if !errors.As(err, &be) || be.Window != "weekly" || be.SpentUSD != 100 {
		t.Fatalf("check with resetTime 00:00 = %v, want BudgetExceededError weekly $5/$100 (spend in current week)", err)
	}
}

// TestKeyBudgetResetTimeShiftsMonthlyWindow: with resetTime 09:30,
// spend on Nov 1 08:00 belongs to OCTOBER (the current month
// starts Nov 1 09:30), not November — the 1st-of-month anchor and
// the shift compose the same way the daily shift does.
func TestKeyBudgetResetTimeShiftsMonthlyWindow(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	priced := func(string) bool { return true }
	kb := windowedKey("k-rt-month", BudgetResetMonthly, nil, nil, fptr(5))
	kb.ResetTime = "09:30"
	if err := l.SetKeyBudgetConfig(ctx, kb); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, time.Date(2026, time.November, 1, 8, 0, 0, 0, time.UTC), "k-rt-month", "p", "m", 100.00, 1, 1, 1, 0, 10)
	if err := l.CheckKeyBudget(ctx, "k-rt-month", []string{"m"}, priced, now); err != nil {
		t.Fatalf("Nov 1 08:00 spend belongs to October's 09:30 window: %v", err)
	}
	st, err := l.KeyBudgetStatus(ctx, "k-rt-month", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Monthly.PeriodStart.Equal(time.Date(2026, time.November, 1, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("monthly period start = %v, want Nov 1 09:30", st.Monthly.PeriodStart)
	}
	if st.Monthly.SpentUSD != 0 {
		t.Errorf("monthly spent = $%.2f, want $0 (the $100 is in October's window)", st.Monthly.SpentUSD)
	}

	// Negative control: the same Nov 1 08:00 spend with resetTime
	// 00:00 IS in November, so the $5 monthly cap refuses.
	kb2 := windowedKey("k-rt-month-0000", BudgetResetMonthly, nil, nil, fptr(5))
	if err := l.SetKeyBudgetConfig(ctx, kb2); err != nil {
		t.Fatalf("set: %v", err)
	}
	seedUsageRow(t, l, time.Date(2026, time.November, 1, 8, 0, 0, 0, time.UTC), "k-rt-month-0000", "p", "m", 100.00, 1, 1, 1, 0, 10)
	err = l.CheckKeyBudget(ctx, "k-rt-month-0000", []string{"m"}, priced, now)
	var be *BudgetExceededError
	if !errors.As(err, &be) || be.Window != "monthly" || be.SpentUSD != 100 {
		t.Fatalf("check with resetTime 00:00 = %v, want BudgetExceededError monthly $5/$100 (spend in current month)", err)
	}
}

// TestKeyBudgetConfigRoundTrip: SetKeyBudgetConfig → GetKeyBudget /
// ListKeyBudgets return every field; the 5-hour-only SetKeyBudget
// leaves the window columns untouched.
func TestKeyBudgetConfigRoundTrip(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	kb := windowedKey("k-rt-trip", BudgetResetWeekly, fptr(5), fptr(20), fptr(50))
	kb.ResetTime = "09:30"
	kb.WarningThreshold = 0.5
	if err := l.SetKeyBudgetConfig(ctx, kb); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := l.GetKeyBudget(ctx, "k-rt-trip")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DailyLimitUSD == nil || *got.DailyLimitUSD != 5 ||
		got.WeeklyLimitUSD == nil || *got.WeeklyLimitUSD != 20 ||
		got.MonthlyLimitUSD == nil || *got.MonthlyLimitUSD != 50 ||
		got.WarningThreshold != 0.5 || got.ResetInterval != BudgetResetWeekly || got.ResetTime != "09:30" {
		t.Errorf("round-trip = %+v, want all window fields", got)
	}
	rows, err := l.ListKeyBudgets(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].KeyID != "k-rt-trip" {
		t.Fatalf("list = %v, want 1 row for k-rt-trip", rows)
	}
	// 5-hour-only setter preserves the windows.
	if err := l.SetKeyBudget(ctx, "k-rt-trip", 200, UnpricedFailClosed); err != nil {
		t.Fatalf("set 5h: %v", err)
	}
	got, err = l.GetKeyBudget(ctx, "k-rt-trip")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LimitUSD != 200 || got.DailyLimitUSD == nil || *got.DailyLimitUSD != 5 || got.ResetInterval != BudgetResetWeekly {
		t.Errorf("after 5h-only set = %+v, want limit $200 with windows intact", got)
	}
	// Validation: negative window limit, unknown interval, bad time.
	bad := windowedKey("k-bad", BudgetResetDaily, fptr(-1), nil, nil)
	if err := l.SetKeyBudgetConfig(ctx, bad); err == nil {
		t.Error("negative daily limit must be rejected")
	}
	bad = windowedKey("k-bad", BudgetResetInterval("fortnightly"), nil, nil, nil)
	if err := l.SetKeyBudgetConfig(ctx, bad); err == nil {
		t.Error("unknown reset interval must be rejected")
	}
	bad = windowedKey("k-bad", BudgetResetDaily, nil, nil, nil)
	bad.ResetTime = "25:00"
	if err := l.SetKeyBudgetConfig(ctx, bad); err == nil {
		t.Error("malformed reset time must be rejected")
	}
}

// TestKeyBudgetMigration: a database created by an older build
// (key_budgets with only the 3 original columns) gains the M5.1
// columns on Open, and the old row keeps working with no calendar
// cap.
func TestKeyBudgetMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE key_budgets (
		key_id TEXT PRIMARY KEY, limit_usd REAL NOT NULL,
		unpriced_policy TEXT NOT NULL DEFAULT 'fail_closed')`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO key_budgets (key_id, limit_usd, unpriced_policy) VALUES ('k-legacy', 10, 'fail_closed')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	db.Close()
	l, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated ledger: %v", err)
	}
	defer l.Close()
	got, err := l.GetKeyBudget(context.Background(), "k-legacy")
	if err != nil {
		t.Fatalf("get migrated row: %v", err)
	}
	if got.LimitUSD != 10 || got.DailyLimitUSD != nil || got.ResetInterval != BudgetResetDaily || got.ResetTime != "00:00" || got.WarningThreshold != 0.8 {
		t.Errorf("migrated row = %+v, want limit $10, no calendar caps, daily/00:00/0.8", got)
	}
	// The migrated row accepts a windowed update.
	kb := got
	kb.DailyLimitUSD = fptr(5)
	if err := l.SetKeyBudgetConfig(context.Background(), kb); err != nil {
		t.Fatalf("set on migrated row: %v", err)
	}
}
