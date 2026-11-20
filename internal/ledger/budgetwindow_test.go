// M5.1 window math: BudgetWindowFor mirrors OmniRoute
// costRules.ts getBudgetWindow (verified 2026-11-18) — daily /
// weekly (Monday) / monthly (1st), all UTC, boundary shifted by
// resetTime.
package ledger

import (
	"testing"
	"time"
)

func TestBudgetWindowFor(t *testing.T) {
	utc := func(y int, mo time.Month, d, h, mi int) time.Time {
		return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
	}
	// Wednesday 2026-11-18 12:00 UTC.
	now := utc(2026, time.November, 18, 12, 0)
	cases := []struct {
		name      string
		interval  BudgetResetInterval
		reset     string
		wantStart time.Time
		wantNext  time.Time
	}{
		{"daily midnight", BudgetResetDaily, "00:00", utc(2026, time.November, 18, 0, 0), utc(2026, time.November, 19, 0, 0)},
		{"daily 09:30 after reset", BudgetResetDaily, "09:30", utc(2026, time.November, 18, 9, 30), utc(2026, time.November, 19, 9, 30)},
		{"weekly monday", BudgetResetWeekly, "00:00", utc(2026, time.November, 16, 0, 0), utc(2026, time.November, 23, 0, 0)},
		{"weekly 09:30", BudgetResetWeekly, "09:30", utc(2026, time.November, 16, 9, 30), utc(2026, time.November, 23, 9, 30)},
		{"monthly 1st", BudgetResetMonthly, "00:00", utc(2026, time.November, 1, 0, 0), utc(2026, time.December, 1, 0, 0)},
		{"monthly 09:30", BudgetResetMonthly, "09:30", utc(2026, time.November, 1, 9, 30), utc(2026, time.December, 1, 9, 30)},
		{"unknown interval falls back to daily", BudgetResetInterval("fortnightly"), "00:00", utc(2026, time.November, 18, 0, 0), utc(2026, time.November, 19, 0, 0)},
		{"malformed reset time falls back to midnight", BudgetResetDaily, "99:99", utc(2026, time.November, 18, 0, 0), utc(2026, time.November, 19, 0, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := BudgetWindowFor(tc.interval, tc.reset, now)
			if !w.PeriodStart.Equal(tc.wantStart) || !w.NextReset.Equal(tc.wantNext) {
				t.Errorf("window = [%v, %v), want [%v, %v)", w.PeriodStart, w.NextReset, tc.wantStart, tc.wantNext)
			}
		})
	}
	// Before today's reset time the window is yesterday's.
	early := utc(2026, time.November, 18, 8, 0)
	w := BudgetWindowFor(BudgetResetDaily, "09:30", early)
	if !w.PeriodStart.Equal(utc(2026, time.November, 17, 9, 30)) || !w.NextReset.Equal(utc(2026, time.November, 18, 9, 30)) {
		t.Errorf("pre-reset window = [%v, %v), want [Nov 17 09:30, Nov 18 09:30)", w.PeriodStart, w.NextReset)
	}
	// Monday before the reset time belongs to last week.
	mondayEarly := utc(2026, time.November, 16, 8, 0)
	w = BudgetWindowFor(BudgetResetWeekly, "09:30", mondayEarly)
	if !w.PeriodStart.Equal(utc(2026, time.November, 9, 9, 30)) || !w.NextReset.Equal(utc(2026, time.November, 16, 9, 30)) {
		t.Errorf("pre-reset weekly window = [%v, %v), want [Nov 9 09:30, Nov 16 09:30)", w.PeriodStart, w.NextReset)
	}
	// December → January month rollover.
	dec := utc(2026, time.December, 15, 12, 0)
	w = BudgetWindowFor(BudgetResetMonthly, "00:00", dec)
	if !w.PeriodStart.Equal(utc(2026, time.December, 1, 0, 0)) || !w.NextReset.Equal(utc(2027, time.January, 1, 0, 0)) {
		t.Errorf("december window = [%v, %v), want [Dec 1, Jan 1)", w.PeriodStart, w.NextReset)
	}
}

func TestNormalizeBudgetResetTime(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "00:00", true},
		{"00:00", "00:00", true},
		{"09:30", "09:30", true},
		{"23:59", "23:59", true},
		{"24:00", "", false},
		{"9:30", "", false},
		{"09:60", "", false},
		{"ab:cd", "", false},
		{"09:30:00", "", false},
	} {
		got, ok := NormalizeBudgetResetTime(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeBudgetResetTime(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"", true}, {"daily", true}, {"weekly", true}, {"monthly", true}, {"fortnightly", false},
	} {
		if _, ok := NormalizeBudgetResetInterval(tc.in); ok != tc.ok {
			t.Errorf("NormalizeBudgetResetInterval(%q) ok = %v, want %v", tc.in, ok, tc.ok)
		}
	}
}
