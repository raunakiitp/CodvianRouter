package ledger

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "time/tzdata"
)

// M5.9: quota schedules — time-aware quota windows for a quota
// pool (the sector baseline lib/quota/schedules.ts, enforce.ts). A schedule
// answers WHEN a pool may be used (days + start/end minute +
// mode) and HOW MUCH may burn while it is in there (reserves —
// the keep-free floor — and the schedule's own-consumption
// budget). Pure and clock-injectable: every evaluation takes
// `now`, so tests drive the calendar deterministically.

// ScheduleMode is allow (whitelist: the pool is usable only
// inside the window) or block (blackout: the pool is unusable
// inside the window).
const (
	ScheduleModeAllow = "allow"
	ScheduleModeBlock = "block"
)

// ReserveRule is one "keep this much free" rule (the sector baseline
// schedules.ts ReserveRule): the free fraction of the account's
// quota in Window must stay at or above Percent. Providers meter
// several windows at once, so a reserve names the window it
// guards; "any" means whichever window is most consumed decides.
type ReserveRule struct {
	Window  string  `json:"window"`
	Percent float64 `json:"percent"`
}

// QuotaSchedule is one time-aware quota window on a pool.
type QuotaSchedule struct {
	ID     string `json:"id"`
	PoolID string `json:"pool_id"`
	Label  string `json:"label,omitempty"`
	// Days are JS getDay() values, 0 = Sunday .. 6 = Saturday.
	Days []int `json:"days"`
	// StartMinute/EndMinute are minutes from local midnight.
	// EndMinute <= StartMinute wraps past midnight; Days then
	// refers to the day the window STARTS on.
	StartMinute int `json:"start_minute"`
	EndMinute   int `json:"end_minute"`
	// Timezone is the IANA zone the window is evaluated in.
	// Empty (or unknown) means UTC — never the host's local
	// time — so a schedule means the same on every machine.
	Timezone    string        `json:"timezone,omitempty"`
	Mode        string        `json:"mode"`
	Reserves    []ReserveRule `json:"reserves,omitempty"`
	BudgetValue *float64      `json:"budget_value,omitempty"`
	BudgetUnit  string        `json:"budget_unit,omitempty"`
	// BudgetWindow is the rolling window the budget is counted
	// over (5h/hourly/daily/weekly/monthly).
	BudgetWindow string `json:"budget_window,omitempty"`
	Priority     int    `json:"priority"`
	Enabled      bool   `json:"enabled"`
}

// ScheduleVerdict is what a pool's schedules say about now.
type ScheduleVerdict struct {
	// Kind: "unscheduled" (no enabled schedules, or only block
	// rows and none matching — open by default), "active" (an
	// allow window is open), "closed" (no window open).
	Kind string
	// Reason for a closed verdict: "blackout" or "outside-window".
	Reason string
	// Schedule is the matching schedule for active/blackout.
	Schedule *QuotaSchedule
	// RetryAfterSeconds is set on outside-window when a later
	// allow window exists.
	RetryAfterSeconds *int
}

// ScheduleBlock is a schedule enforcement refusal: the gate
// blocks the request and (for the webhook) says why.
type ScheduleBlock struct {
	// Reason: "blackout" | "outside-window" | "reserve" | "budget".
	Reason string
	// ScheduleID is the schedule that blocked ("" when none —
	// outside-window with no matching schedule still names the
	// pool's next window only via RetryAfterSeconds).
	ScheduleID string
	// RetryAfterSeconds, when the block will lift at a known time.
	RetryAfterSeconds *int
	// Used/Limit describe the breached floor or budget, when
	// the block is a reserve or a budget breach (0 otherwise).
	Used  float64
	Limit float64
	// Percent is the reserve floor that was breached (reserve
	// blocks only).
	Percent float64
}

func (b *ScheduleBlock) Error() string {
	switch b.Reason {
	case "blackout":
		return fmt.Sprintf("quota pool schedule %q is in a blackout window", b.ScheduleID)
	case "outside-window":
		return "quota pool is outside its allowed schedule window"
	case "reserve":
		return fmt.Sprintf("quota pool is below the %.2f%% keep-free floor this window protects [schedule-reserve]", b.Percent)
	case "budget":
		return fmt.Sprintf("scheduled %s budget for quota pool is spent for this window [schedule-budget]", "quota")
	default:
		return "quota pool schedule blocked the request"
	}
}

// localDayAndMinute resolves now to a (weekday, minute-of-day)
// pair in the schedule's zone. An empty or unknown zone falls
// back to UTC — never to the host's local time: a schedule must
// mean the same thing on every machine. With a time.Local
// fallback the same config enforced different windows on a UTC
// Docker host and an IST laptop (the two gate failures of
// 2026-11-19 were exactly this: the gate ran with local time
// UTC+5:30, so empty-zone windows were evaluated 5:30 off).
// A bad zone string must never take the quota engine down
// (the sector baseline localDayAndMinute).
func localDayAndMinute(now time.Time, timezone string) (day int, minute int) {
	if timezone != "" {
		if loc, err := time.LoadLocation(timezone); err == nil {
			local := now.In(loc)
			return int(local.Weekday()), local.Hour()*60 + local.Minute()
		}
	}
	utc := now.UTC()
	return int(utc.Weekday()), utc.Hour()*60 + utc.Minute()
}

// isScheduleActive reports whether one schedule's window is open
// at now. A window whose end <= start wraps past midnight; days
// then refers to the day the window STARTS on.
func isScheduleActive(s QuotaSchedule, now time.Time) bool {
	if !s.Enabled || len(s.Days) == 0 {
		return false
	}
	day, minute := localDayAndMinute(now, s.Timezone)
	if s.EndMinute > s.StartMinute {
		return containsDay(s.Days, day) && minute >= s.StartMinute && minute < s.EndMinute
	}
	if minute >= s.StartMinute {
		return containsDay(s.Days, day)
	}
	if minute < s.EndMinute {
		return containsDay(s.Days, (day+6)%7)
	}
	return false
}

func containsDay(days []int, day int) bool {
	for _, d := range days {
		if d == day {
			return true
		}
	}
	return false
}

// secondsUntilNextWindow returns seconds until the earliest
// allow window opens, or nil when none is configured. It scans
// the next 8 local days, which always covers a weekly cycle.
// (the sector baseline secondsUntilNextWindow; DST inside the wait can
// shift the hint by up to an hour — it only feeds Retry-After.)
func secondsUntilNextWindow(schedules []QuotaSchedule, now time.Time) *int {
	var best *int
	for _, s := range schedules {
		if !s.Enabled || s.Mode != ScheduleModeAllow || len(s.Days) == 0 {
			continue
		}
		day, minute := localDayAndMinute(now, s.Timezone)
		for offset := 0; offset <= 7; offset++ {
			if !containsDay(s.Days, (day+offset)%7) {
				continue
			}
			deltaMinutes := offset*1440 + s.StartMinute - minute
			if deltaMinutes <= 0 {
				continue
			}
			if best == nil || deltaMinutes < *best {
				v := deltaMinutes
				best = &v
			}
			break
		}
	}
	if best == nil {
		return nil
	}
	secs := *best * 60
	return &secs
}

// ResolveActiveSchedule decides what a pool's schedules say
// about now. A blackout always wins over an overlapping allow
// window; a pool with only block rows keeps blacklist semantics
// (open by default); as soon as it has one allow row the allow
// rows become the whitelist.
func ResolveActiveSchedule(schedules []QuotaSchedule, now time.Time) ScheduleVerdict {
	var enabled []QuotaSchedule
	for _, s := range schedules {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	if len(enabled) == 0 {
		return ScheduleVerdict{Kind: "unscheduled"}
	}
	var matching []QuotaSchedule
	for _, s := range enabled {
		if isScheduleActive(s, now) {
			matching = append(matching, s)
		}
	}
	sort.Slice(matching, func(a, b int) bool {
		if matching[a].Priority != matching[b].Priority {
			return matching[a].Priority > matching[b].Priority
		}
		return matching[a].ID < matching[b].ID
	})
	for _, s := range matching {
		if s.Mode == ScheduleModeBlock {
			sc := s
			return ScheduleVerdict{Kind: "closed", Reason: "blackout", Schedule: &sc}
		}
	}
	for _, s := range matching {
		if s.Mode == ScheduleModeAllow {
			sc := s
			return ScheduleVerdict{Kind: "active", Schedule: &sc}
		}
	}
	hasAllow := false
	for _, s := range enabled {
		if s.Mode == ScheduleModeAllow {
			hasAllow = true
			break
		}
	}
	if !hasAllow {
		return ScheduleVerdict{Kind: "unscheduled"}
	}
	return ScheduleVerdict{Kind: "closed", Reason: "outside-window", RetryAfterSeconds: secondsUntilNextWindow(enabled, now)}
}

// PoolSaturationByWindow returns the pool's consumption as a
// fraction (0..1) of the effective limit per quota window, over
// the pool's own dimensions. A window the pool does not meter is
// absent. DIVERGENCE from the sector baseline (documented): the sector baseline
// measures the UPSTREAM account's saturation signal (the whole
// account, including other tools sharing the credentials); we
// have no upstream signal, so the fraction is our own pool-wide
// consumption against the dimension's effective limit.
func (l *Ledger) PoolSaturationByWindow(ctx context.Context, pool QuotaPool, now time.Time) (map[string]float64, error) {
	out := map[string]float64{}
	seen := map[string]bool{}
	for _, dim := range pool.Dimensions {
		if seen[dim.Window] {
			continue
		}
		seen[dim.Window] = true
		start, end, ok := poolWindow(dim.Window, now)
		if !ok {
			continue
		}
		used, err := l.poolDimensionUsed(ctx, pool, dim, nil, start, end)
		if err != nil {
			return nil, err
		}
		limit := pool.EffectiveLimit(dim)
		if limit <= 0 {
			continue
		}
		if used > out[dim.Window] {
			out[dim.Window] = used / limit
		}
	}
	return out, nil
}

// FailingReserve returns the first reserve rule the pool can no
// longer honour, or nil when every rule still has room. Rules
// are ANDed and evaluated in order. A window with no usable
// signal is fail-open (treated as satisfied, never as
// exhausted). "any" takes the most-consumed window.
func FailingReserve(s QuotaSchedule, saturation map[string]float64) *ReserveRule {
	var max float64
	haveMax := false
	for _, v := range saturation {
		if !haveMax || v > max {
			max, haveMax = v, true
		}
	}
	for i, rule := range s.Reserves {
		var used float64
		var ok bool
		if rule.Window == "any" {
			used, ok = max, haveMax
		} else {
			used, ok = saturation[rule.Window]
		}
		if !ok {
			continue // fail-open: no signal for this window
		}
		if (1-used)*100 < rule.Percent {
			return &s.Reserves[i]
		}
	}
	return nil
}

// CheckPoolSchedules enforces one pool's schedules at now: the
// window verdict, then the active schedule's keep-free reserves,
// then its own-consumption budget. Nil means the request may
// proceed. A ledger error fails closed (the caller turns it
// into a 503); a missing signal for a reserve fails open inside
// FailingReserve.
func (l *Ledger) CheckPoolSchedules(ctx context.Context, pool QuotaPool, now time.Time) (*ScheduleBlock, error) {
	schedules, err := l.ListQuotaSchedules(ctx, pool.ID)
	if err != nil {
		return nil, err
	}
	verdict := ResolveActiveSchedule(schedules, now)
	switch verdict.Kind {
	case "unscheduled":
		return nil, nil
	case "closed":
		return &ScheduleBlock{Reason: verdict.Reason, ScheduleID: scheduleIDOf(verdict.Schedule), RetryAfterSeconds: verdict.RetryAfterSeconds}, nil
	}
	s := verdict.Schedule
	// Keep-free floor: the active schedule's reserves, measured
	// on the pool's own consumption (see PoolSaturationByWindow).
	if len(s.Reserves) > 0 {
		saturation, err := l.PoolSaturationByWindow(ctx, pool, now)
		if err != nil {
			return nil, err
		}
		if rule := FailingReserve(*s, saturation); rule != nil {
			used := saturation[rule.Window]
			if rule.Window == "any" {
				for _, v := range saturation {
					if v > used {
						used = v
					}
				}
			}
			return &ScheduleBlock{Reason: "reserve", ScheduleID: s.ID, Used: used, Limit: 1, Percent: rule.Percent}, nil
		}
	}
	// Own-consumption budget: the pool's consumption in the
	// budget window against budgetValue. DIVERGENCE from
	// the sector baseline (documented): the sector baseline counts only consumption
	// recorded while the window was open, in a separate bucket
	// namespace; the ledger IS our counter, so we count the
	// pool's consumption in the budget window regardless of
	// when the schedule was open.
	if s.BudgetValue != nil && s.BudgetUnit != "" {
		dim := QuotaDimension{Unit: s.BudgetUnit, Window: s.BudgetWindow}
		start, end, ok := poolWindow(s.BudgetWindow, now)
		if !ok {
			return nil, fmt.Errorf("ledger: unknown schedule budget window %q", s.BudgetWindow)
		}
		used, err := l.poolDimensionUsed(ctx, pool, dim, nil, start, end)
		if err != nil {
			return nil, err
		}
		if used >= *s.BudgetValue {
			return &ScheduleBlock{Reason: "budget", ScheduleID: s.ID, Used: used, Limit: *s.BudgetValue}, nil
		}
	}
	return nil, nil
}

func scheduleIDOf(s *QuotaSchedule) string {
	if s == nil {
		return ""
	}
	return s.ID
}

// ---------------------------------------------------------------------------
// Storage.
// ---------------------------------------------------------------------------

// SetQuotaSchedule upserts one schedule (and its reserve rules,
// replaced wholesale) for a pool. The pool must exist.
func (l *Ledger) SetQuotaSchedule(ctx context.Context, s QuotaSchedule) error {
	if s.ID == "" {
		return fmt.Errorf("ledger: quota schedule id must not be empty")
	}
	if s.PoolID == "" {
		return fmt.Errorf("ledger: quota schedule pool_id must not be empty")
	}
	if _, err := l.GetQuotaPool(ctx, s.PoolID); err != nil {
		return fmt.Errorf("ledger: quota schedule pool: %w", err)
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: schedule tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_schedules (id, pool_id, label, days, start_minute, end_minute, timezone, mode, budget_value, budget_unit, budget_window, priority, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET pool_id=excluded.pool_id, label=excluded.label, days=excluded.days, start_minute=excluded.start_minute, end_minute=excluded.end_minute, timezone=excluded.timezone, mode=excluded.mode, budget_value=excluded.budget_value, budget_unit=excluded.budget_unit, budget_window=excluded.budget_window, priority=excluded.priority, enabled=excluded.enabled`,
		s.ID, s.PoolID, s.Label, daysToString(s.Days), s.StartMinute, s.EndMinute, s.Timezone, s.Mode, s.BudgetValue, s.BudgetUnit, s.BudgetWindow, s.Priority, boolToInt(s.Enabled)); err != nil {
		return fmt.Errorf("ledger: set quota schedule: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM quota_schedule_reserves WHERE schedule_id = ?`, s.ID); err != nil {
		return fmt.Errorf("ledger: schedule reserves delete: %w", err)
	}
	for _, r := range s.Reserves {
		if _, err := tx.ExecContext(ctx, `INSERT INTO quota_schedule_reserves (schedule_id, window, percent) VALUES (?, ?, ?)`, s.ID, r.Window, r.Percent); err != nil {
			return fmt.Errorf("ledger: schedule reserve insert: %w", err)
		}
	}
	return tx.Commit()
}

// GetQuotaSchedule returns one schedule by id.
func (l *Ledger) GetQuotaSchedule(ctx context.Context, id string) (QuotaSchedule, error) {
	row := l.rdb.QueryRowContext(ctx, `
		SELECT id, pool_id, label, days, start_minute, end_minute, timezone, mode, budget_value, budget_unit, budget_window, priority, enabled
		FROM quota_schedules WHERE id = ?`, id)
	s, err := scanQuotaScheduleRow(row)
	if err != nil {
		return QuotaSchedule{}, err
	}
	reserves, err := l.loadScheduleReserves(ctx, s.ID)
	if err != nil {
		return QuotaSchedule{}, err
	}
	s.Reserves = reserves
	return s, nil
}

// ListQuotaSchedules returns a pool's schedules, priority DESC
// then id ASC (the resolution order).
func (l *Ledger) ListQuotaSchedules(ctx context.Context, poolID string) ([]QuotaSchedule, error) {
	rows, err := l.rdb.QueryContext(ctx, `
		SELECT id, pool_id, label, days, start_minute, end_minute, timezone, mode, budget_value, budget_unit, budget_window, priority, enabled
		FROM quota_schedules WHERE pool_id = ? ORDER BY priority DESC, id ASC`, poolID)
	if err != nil {
		return nil, fmt.Errorf("ledger: list quota schedules: %w", err)
	}
	defer rows.Close()
	var out []QuotaSchedule
	for rows.Next() {
		s, err := scanQuotaScheduleRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list quota schedules rows: %w", err)
	}
	for i := range out {
		reserves, err := l.loadScheduleReserves(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Reserves = reserves
	}
	return out, nil
}

// DeleteQuotaSchedule removes one schedule and its reserves.
func (l *Ledger) DeleteQuotaSchedule(ctx context.Context, id string) error {
	if _, err := l.db.ExecContext(ctx, `DELETE FROM quota_schedules WHERE id = ?`, id); err != nil {
		return fmt.Errorf("ledger: delete quota schedule: %w", err)
	}
	return nil
}

func (l *Ledger) loadScheduleReserves(ctx context.Context, scheduleID string) ([]ReserveRule, error) {
	rows, err := l.rdb.QueryContext(ctx, `SELECT window, percent FROM quota_schedule_reserves WHERE schedule_id = ? ORDER BY rowid`, scheduleID)
	if err != nil {
		return nil, fmt.Errorf("ledger: schedule reserves: %w", err)
	}
	defer rows.Close()
	var out []ReserveRule
	for rows.Next() {
		var r ReserveRule
		if err := rows.Scan(&r.Window, &r.Percent); err != nil {
			return nil, fmt.Errorf("ledger: schedule reserves scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type quotaScheduleRowScanner interface {
	Scan(dest ...any) error
}

func scanQuotaScheduleRow(row quotaScheduleRowScanner) (QuotaSchedule, error) {
	var s QuotaSchedule
	var days string
	var budgetValue *float64
	var enabled int
	if err := row.Scan(&s.ID, &s.PoolID, &s.Label, &days, &s.StartMinute, &s.EndMinute, &s.Timezone, &s.Mode, &budgetValue, &s.BudgetUnit, &s.BudgetWindow, &s.Priority, &enabled); err != nil {
		return QuotaSchedule{}, fmt.Errorf("ledger: quota schedule row: %w", err)
	}
	s.Days = stringToDays(days)
	s.BudgetValue = budgetValue
	s.Enabled = enabled != 0
	return s, nil
}

// daysToString stores the weekday set as a comma list ("1,2,3").
func daysToString(days []int) string {
	if len(days) == 0 {
		return ""
	}
	parts := make([]string, len(days))
	for i, d := range days {
		parts[i] = fmt.Sprintf("%d", d)
	}
	return strings.Join(parts, ",")
}

func stringToDays(s string) []int {
	if s == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		var d int
		if _, err := fmt.Sscanf(strings.TrimSpace(part), "%d", &d); err == nil {
			out = append(out, d)
		}
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
