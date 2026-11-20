package ledger

import (
	"context"
	"testing"
	"time"
)

// M5.9: quota schedules — allow/block windows, keep-free
// reserves, own-consumption budgets.
//
// 2026-11-18 is a Wednesday. All times below are UTC (the
// schedules use timezone "" = server local, which is UTC in
// this environment).
func wednesday(t *testing.T, h, m int) time.Time {
	t.Helper()
	return time.Date(2026, 11, 18, h, m, 0, 0, time.UTC)
}

func scheduleTestPool(t *testing.T, l *Ledger) QuotaPool {
	t.Helper()
	pool := testPool()
	pool.Dimensions = []QuotaDimension{
		{Unit: "requests", Window: "daily", Limit: 100},
		{Unit: "tokens", Window: "daily", Limit: 10000},
	}
	if err := l.SetQuotaPool(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestResolveActiveSchedule(t *testing.T) {
	allow := QuotaSchedule{ID: "allow", PoolID: "pool1", Days: []int{3}, StartMinute: 9 * 60, EndMinute: 17 * 60, Mode: ScheduleModeAllow, Priority: 1, Enabled: true}
	block := QuotaSchedule{ID: "block", PoolID: "pool1", Days: []int{3}, StartMinute: 12 * 60, EndMinute: 13 * 60, Mode: ScheduleModeBlock, Priority: 2, Enabled: true}
	// No schedules: unscheduled (open by default).
	if v := ResolveActiveSchedule(nil, wednesday(t, 10, 0)); v.Kind != "unscheduled" {
		t.Errorf("no schedules: %+v, want unscheduled", v)
	}
	// Inside the allow window: active.
	if v := ResolveActiveSchedule([]QuotaSchedule{allow}, wednesday(t, 10, 0)); v.Kind != "active" || v.Schedule.ID != "allow" {
		t.Errorf("10:00 in 09:00-17:00 allow: %+v, want active/allow", v)
	}
	// Outside: closed outside-window, with a retry hint to 09:00 tomorrow (Thu is not in days — next Wed 09:00).
	if v := ResolveActiveSchedule([]QuotaSchedule{allow}, wednesday(t, 18, 0)); v.Kind != "closed" || v.Reason != "outside-window" {
		t.Errorf("18:00 outside allow: %+v, want closed/outside-window", v)
	} else if v.RetryAfterSeconds == nil {
		t.Error("outside-window without a retry hint")
	}
	// Only block rows, none matching: unscheduled (blacklist semantics).
	if v := ResolveActiveSchedule([]QuotaSchedule{block}, wednesday(t, 10, 0)); v.Kind != "unscheduled" {
		t.Errorf("10:00 outside block: %+v, want unscheduled", v)
	}
	// Inside the block: closed blackout — and the blackout wins
	// over the overlapping allow window (higher priority too).
	if v := ResolveActiveSchedule([]QuotaSchedule{allow, block}, wednesday(t, 12, 30)); v.Kind != "closed" || v.Reason != "blackout" || v.Schedule.ID != "block" {
		t.Errorf("12:30 in block + allow: %+v, want closed/blackout/block", v)
	}
	// Disabled schedules are invisible.
	disabled := allow
	disabled.Enabled = false
	if v := ResolveActiveSchedule([]QuotaSchedule{disabled}, wednesday(t, 10, 0)); v.Kind != "unscheduled" {
		t.Errorf("disabled allow: %+v, want unscheduled", v)
	}
}

func TestIsScheduleActiveOvernight(t *testing.T) {
	// Fri 22:00–02:00: open Fri night and Sat small hours.
	s := QuotaSchedule{Days: []int{5}, StartMinute: 22 * 60, EndMinute: 2 * 60, Mode: ScheduleModeAllow, Enabled: true}
	fri := time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC) // Friday
	sat := time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)  // Saturday
	satDay := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	if !isScheduleActive(s, fri) {
		t.Error("Fri 23:00 in Fri 22:00-02:00: want active")
	}
	if !isScheduleActive(s, sat) {
		t.Error("Sat 01:00 in Fri 22:00-02:00: want active (day the window starts on)")
	}
	if isScheduleActive(s, satDay) {
		t.Error("Sat 12:00 in Fri 22:00-02:00: want inactive")
	}
	// Timezone: 09:00-17:00 America/New_York on Wed = 14:00-22:00 UTC.
	ny := QuotaSchedule{Days: []int{3}, StartMinute: 9 * 60, EndMinute: 17 * 60, Timezone: "America/New_York", Mode: ScheduleModeAllow, Enabled: true}
	if !isScheduleActive(ny, wednesday(t, 15, 0)) {
		t.Error("15:00 UTC = 10:00 New York: want active")
	}
	if isScheduleActive(ny, wednesday(t, 13, 0)) {
		t.Error("13:00 UTC = 08:00 New York: want inactive")
	}
	// Unknown zone falls back to server local (UTC here), never down.
	bad := QuotaSchedule{Days: []int{3}, StartMinute: 9 * 60, EndMinute: 17 * 60, Timezone: "No/Such_Zone", Mode: ScheduleModeAllow, Enabled: true}
	if !isScheduleActive(bad, wednesday(t, 10, 0)) {
		t.Error("unknown zone 10:00 UTC: want active (local fallback)")
	}
}

func TestCheckPoolSchedulesReserveAndBudget(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	pool := scheduleTestPool(t, l)
	now := wednesday(t, 10, 0)
	// Active allow window with a 20% keep-free reserve on the
	// daily window and a 90-request budget.
	budget := 90.0
	sched := QuotaSchedule{
		ID: "sched1", PoolID: "pool1", Days: []int{3}, StartMinute: 9 * 60, EndMinute: 17 * 60,
		Mode: ScheduleModeAllow, Priority: 1, Enabled: true,
		Reserves:     []ReserveRule{{Window: "daily", Percent: 20}},
		BudgetValue:  &budget,
		BudgetUnit:   "requests",
		BudgetWindow: "daily",
	}
	if err := l.SetQuotaSchedule(ctx, sched); err != nil {
		t.Fatal(err)
	}
	if block, err := l.CheckPoolSchedules(ctx, pool, now); err != nil || block != nil {
		t.Fatalf("empty pool in window: block=%v err=%v, want pass", block, err)
	}
	// 79/100 daily requests: free fraction 21% >= 20% floor: pass.
	seedRequests(t, l, "k1", now.Add(-time.Hour), 79)
	if block, err := l.CheckPoolSchedules(ctx, pool, now); err != nil || block != nil {
		t.Fatalf("79/100 (21%% free): block=%v err=%v, want pass", block, err)
	}
	// 81/100: free 19% < 20% floor: reserve block. (The 90-request
	// budget is not spent yet, so the reserve fires first.)
	seedRequests(t, l, "k1", now.Add(-30*time.Minute), 2)
	block, err := l.CheckPoolSchedules(ctx, pool, now)
	if err != nil || block == nil || block.Reason != "reserve" || block.ScheduleID != "sched1" {
		t.Fatalf("81/100 (19%% free): block=%v err=%v, want reserve/sched1", block, err)
	}
	if block.Percent != 20 {
		t.Errorf("reserve percent = %v, want 20", block.Percent)
	}
	// Without the reserve, the budget alone blocks at 90.
	sched.Reserves = nil
	if err := l.SetQuotaSchedule(ctx, sched); err != nil {
		t.Fatal(err)
	}
	seedRequests(t, l, "k1", now.Add(-10*time.Minute), 10)
	block, err = l.CheckPoolSchedules(ctx, pool, now)
	if err != nil || block == nil || block.Reason != "budget" {
		t.Fatalf("91 requests vs 90 budget: block=%v err=%v, want budget", block, err)
	}
	if block.Used != 91 || block.Limit != 90 {
		t.Errorf("budget block used/limit = %v/%v, want 91/90", block.Used, block.Limit)
	}
	// Outside the window the allow schedule closes the pool.
	block, err = l.CheckPoolSchedules(ctx, pool, wednesday(t, 18, 0))
	if err != nil || block == nil || block.Reason != "outside-window" {
		t.Fatalf("18:00 outside window: block=%v err=%v, want outside-window", block, err)
	}
	if block.RetryAfterSeconds == nil || *block.RetryAfterSeconds <= 0 {
		t.Errorf("outside-window retryAfter = %v, want positive seconds", block.RetryAfterSeconds)
	}
}

func TestQuotaScheduleCRUD(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	scheduleTestPool(t, l)
	s := QuotaSchedule{ID: "s1", PoolID: "pool1", Label: "work hours", Days: []int{1, 2, 3, 4, 5}, StartMinute: 540, EndMinute: 1020, Mode: ScheduleModeAllow, Priority: 3, Enabled: true, Reserves: []ReserveRule{{Window: "daily", Percent: 10}, {Window: "any", Percent: 5}}}
	if err := l.SetQuotaSchedule(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := l.GetQuotaSchedule(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "work hours" || len(got.Days) != 5 || len(got.Reserves) != 2 || got.Reserves[1].Window != "any" {
		t.Errorf("round-trip = %+v, want label/days/reserves preserved", got)
	}
	// Wholesale replace of reserves on upsert.
	s.Reserves = []ReserveRule{{Window: "weekly", Percent: 25}}
	if err := l.SetQuotaSchedule(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, _ = l.GetQuotaSchedule(ctx, "s1")
	if len(got.Reserves) != 1 || got.Reserves[0].Window != "weekly" {
		t.Errorf("upsert reserves = %+v, want only weekly/25", got.Reserves)
	}
	// Unknown pool: refused.
	if err := l.SetQuotaSchedule(ctx, QuotaSchedule{ID: "bad", PoolID: "nope", Mode: ScheduleModeAllow, Enabled: true}); err == nil {
		t.Error("schedule for an unknown pool: want an error")
	}
	list, err := l.ListQuotaSchedules(ctx, "pool1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d rows err %v, want 1", len(list), err)
	}
	if err := l.DeleteQuotaSchedule(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	list, _ = l.ListQuotaSchedules(ctx, "pool1")
	if len(list) != 0 {
		t.Errorf("list after delete = %d, want 0", len(list))
	}
}
