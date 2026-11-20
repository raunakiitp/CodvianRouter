// Per-key rolling 5-hour USD budgets.
//
// A key budget caps what ONE gateway key may spend in the last 5
// hours — a rolling window, not a calendar day, so spend ages out
// continuously instead of resetting at midnight (the OmniRoute
// v3.8.54 pattern). The window is computed from usage_events, the
// source of truth: only priced spend counts toward the cap, and
// budgets live in the ledger database, so they survive a restart
// exactly like usage history.
//
// Honesty rules (research-log entries 83 + 136: "unpriced ≠ $0"):
//   - Priced spend is the only spend that counts. A row whose cost
//     is unknown — the upstream never reported a usable split
//     (streaming / total-only attempts), or the (provider, model)
//     pair has no configured price — is UNKNOWN spend: it is never
//     counted as $0, and never silently counted as spent. It is
//     reported separately (UnknownEvents / UnknownTokens /
//     UnpricedModels) so the operator can see the gap.
//   - Failed attempts and coalesced-waiter (shared) rows cost $0:
//     a failed attempt bought no tokens, and a shared row is served
//     from the leader's call — the leader's row is the only one
//     billed. Fallback attempts therefore consume NO quota.
//   - The unpriced-usage policy decides what happens when a request
//     would USE an unpriced model under a capped key:
//     fail_closed (default) refuses it with a 400 naming the
//     unpriced models — the router cannot prove the spend fits the
//     budget, so it does not guess; count_as_zero lets it through
//     counting $0 while still enforcing priced spend against the
//     cap. Priced spend over the cap is always a 402 (via the
//     existing BudgetExceededError); the 400 is used only when the
//     block is caused by unpriced usage alone.
//
// B2 (M10): the same fail-closed gate extends to the calendar
// windows — daily / weekly / monthly caps are enforced ALL AT
// ONCE, each against its own UTC period's spend (daily [today
// resetTime, tomorrow resetTime), weekly [Monday resetTime, next
// Monday resetTime), monthly [1st resetTime, next 1st resetTime),
// resetTime default "00:00"; see BudgetWindowFor), and unpriced
// usage already inside a set window's current period closes
// that period for ALL models until it ends. A set window is
// never exempted by ResetInterval — that selector only picks
// which window the status reports as "active".
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// RollingWindow is the per-key budget window: the last 5 hours,
// rolling. Spend older than this does not count toward the cap.
const RollingWindow = 5 * time.Hour

// UnpricedPolicy is what a capped key does with usage whose price
// is unknown.
type UnpricedPolicy string

const (
	// UnpricedFailClosed refuses any request to an unpriced model
	// under a capped key (the default): the spend cannot be
	// proven to fit the budget, so it is blocked, not guessed.
	UnpricedFailClosed UnpricedPolicy = "fail_closed"
	// UnpricedCountAsZero lets unpriced usage through counting
	// $0, while priced spend is still enforced against the cap.
	UnpricedCountAsZero UnpricedPolicy = "count_as_zero"
)

// NormalizeUnpricedPolicy parses a policy name: "" means the
// default (fail_closed). ok=false names an unknown policy.
func NormalizeUnpricedPolicy(s string) (UnpricedPolicy, bool) {
	switch s {
	case "", string(UnpricedFailClosed):
		return UnpricedFailClosed, true
	case string(UnpricedCountAsZero):
		return UnpricedCountAsZero, true
	default:
		return "", false
	}
}

// KeyBudget is one key's rolling 5-hour USD cap plus its
// unpriced-usage policy.
//
// M5.1: calendar-window caps (daily / weekly / monthly, nil =
// unset) + the active-window selector. B2: ALL THREE set windows
// are enforced, each against its own calendar period's spend —
// ResetInterval only selects which window the status reports as
// "active" (ActiveWindow / WarningReached); it never exempts a
// set cap. A request is refused (402) when the 5-hour rolling
// spend OR any set window's spend reaches that window's cap.
// The other windows' spend is still reported in KeyBudgetStatus,
// so the operator sees all three. The 5-hour rolling cap is
// enforced ALWAYS, alongside the windows — it predates M5.1 and
// is never weakened by it. WarningThreshold (0–1, default 0.8)
// is the fraction of any set window's limit at which the key is
// reported as warning (status only; enforcement is still a hard
// deny at the limit) — WarningReached is true once ANY set
// window reaches its threshold, not just the active one.
//
// Boundary rule (B2): every calendar window is a UTC period
// shifted by the row's ResetTime ("HH:MM", default "00:00") —
// daily [today resetTime, tomorrow resetTime), weekly [Monday
// resetTime, next Monday resetTime), monthly [1st resetTime,
// next 1st resetTime). See BudgetWindowFor (budgetwindow.go):
// all math is UTC because usage_events.ts is UTC milliseconds,
// so a boundary compares directly against a usage timestamp
// with no timezone conversion anywhere in the path.
type KeyBudget struct {
	KeyID            string              `json:"key_id"`
	LimitUSD         float64             `json:"limit_usd"`
	UnpricedPolicy   UnpricedPolicy      `json:"unpriced_policy"`
	DailyLimitUSD    *float64            `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD   *float64            `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD  *float64            `json:"monthly_limit_usd,omitempty"`
	WarningThreshold float64             `json:"warning_threshold"`
	ResetInterval    BudgetResetInterval `json:"reset_interval"`
	ResetTime        string              `json:"reset_time"`
}

// KeyBudgetWindowStatus is one calendar window's position: the
// limit (nil = unset), priced spend in the current period, the
// remaining budget (nil when unset), the period bounds, and
// whether the window is at its limit.
type KeyBudgetWindowStatus struct {
	LimitUSD     *float64  `json:"limit_usd,omitempty"`
	SpentUSD     float64   `json:"spent_usd"`
	RemainingUSD *float64  `json:"remaining_usd,omitempty"`
	PeriodStart  time.Time `json:"period_start"`
	NextReset    time.Time `json:"next_reset"`
	AtLimit      bool      `json:"at_limit"`
}

// UnpricedUsageError is the typed refusal returned when a capped
// key's request would use unpriced models under the fail_closed
// policy. Callers translate it into a 400 naming the unpriced
// models — never a 402, which is reserved for priced spend over
// the cap — so the operator can tell "blocked because the price is
// unknown" apart from "blocked because the budget is spent".
//
// M5.1: MonthClosed names the monthly variant — a model without a
// price was used earlier in the current calendar month, so the
// monthly cap cannot be enforced and the month is closed for ALL
// models until it ends (OmniRoute keyQuota.ts). LimitUSD is then
// the monthly cap and SpentUSD the month's priced spend.
//
// B2: the same closed-period rule applies to the daily and weekly
// windows — Window names the closed window ("daily" / "weekly" /
// "monthly") and MonthClosed stays true only for the monthly one,
// so existing monthly callers keep their exact behaviour.
type UnpricedUsageError struct {
	KeyID       string
	Models      []string
	LimitUSD    float64
	SpentUSD    float64
	MonthClosed bool
	Window      string
}

func (e *UnpricedUsageError) Error() string {
	if e.MonthClosed {
		return fmt.Sprintf(
			"unpriced usage blocked for key %q: model(s) %v were used without a configured price earlier this month, "+
				"so spend cannot be proven against the $%.4f monthly budget "+
				"(priced spend so far $%.4f); the month stays closed until it ends — configure prices for the models or set the "+
				"key's unpriced policy to count_as_zero",
			e.KeyID, e.Models, e.LimitUSD, e.SpentUSD)
	}
	if e.Window == "daily" || e.Window == "weekly" {
		return fmt.Sprintf(
			"unpriced usage blocked for key %q: model(s) %v were used without a configured price earlier this %s, "+
				"so spend cannot be proven against the $%.4f %s budget "+
				"(priced spend so far $%.4f); the %s stays closed until it ends — configure prices for the models or set the "+
				"key's unpriced policy to count_as_zero",
			e.KeyID, e.Models, e.Window, e.LimitUSD, e.Window, e.SpentUSD, e.Window)
	}
	return fmt.Sprintf(
		"unpriced usage blocked for key %q: model(s) %v have no configured price, "+
			"so spend cannot be proven against the $%.4f rolling 5-hour budget "+
			"(priced spend so far $%.4f); configure a price for the model or set the "+
			"key's unpriced policy to count_as_zero",
		e.KeyID, e.Models, e.LimitUSD, e.SpentUSD)
}

// IsUnpricedUsage reports whether err is (or wraps) an
// UnpricedUsageError.
func IsUnpricedUsage(err error) bool {
	var ue *UnpricedUsageError
	return errors.As(err, &ue)
}

// KeyBudgetStatus is one key's rolling-window position, as the
// dashboard shows it. SpentUSD is priced spend only — a lower
// bound, never exact billing when unknown usage exists. Unknown
// spend is reported separately and never folded into SpentUSD.
// RemainingUSD is nil when the key has no budget (unlimited).
// NextRolloff is when the oldest priced spend in the window ages
// out (nil when the window holds no priced spend): the rolling
// window has no calendar reset, so this is the honest "reset
// time" for the current spend.
//
// M5.1: Daily / Weekly / Monthly report the calendar windows'
// spend in their current periods (spend is always computed, even
// when the limit is unset — the operator sees what a cap would
// catch). ActiveWindow names the window the enforcement gate uses
// ("5h" when no calendar limit is active); ActiveLimitUSD is its
// limit (nil = unlimited); WarningReached is true once ANY set
// window's spend reaches WarningThreshold of that window's limit
// (not just the active window's — see keyBudgetStatusWithBudget).
type KeyBudgetStatus struct {
	KeyID          string                `json:"key_id"`
	HasBudget      bool                  `json:"has_budget"`
	LimitUSD       float64               `json:"limit_usd"`
	UnpricedPolicy UnpricedPolicy        `json:"unpriced_policy"`
	SpentUSD       float64               `json:"spent_usd"`
	RemainingUSD   *float64              `json:"remaining_usd"`
	AtLimit        bool                  `json:"at_limit"`
	WindowStart    time.Time             `json:"window_start"`
	WindowEnd      time.Time             `json:"window_end"`
	NextRolloff    *time.Time            `json:"next_rolloff"`
	UnknownEvents  int                   `json:"unknown_events"`
	UnknownTokens  int                   `json:"unknown_tokens"`
	UnpricedModels []string              `json:"unpriced_models"`
	Daily          KeyBudgetWindowStatus `json:"daily"`
	Weekly         KeyBudgetWindowStatus `json:"weekly"`
	Monthly        KeyBudgetWindowStatus `json:"monthly"`
	ResetInterval  BudgetResetInterval   `json:"reset_interval"`
	ResetTime      string                `json:"reset_time"`
	ActiveWindow   string                `json:"active_window"`
	ActiveLimitUSD *float64              `json:"active_limit_usd,omitempty"`
	WarningReached bool                  `json:"warning_reached"`
}

// keyBudgetMemoKey is the context key for the per-request
// key-budget memo (CHECKLIST 279-280).
type keyBudgetMemoKey struct{}

// keyBudgetMemoEntry is one memoised key-budget read: a row that
// was found, or the absence of one (found=false, i.e. the read
// returned sql.ErrNoRows). Real database errors are never cached:
// the next read retries the database instead of memoising a
// failure.
type keyBudgetMemoEntry struct {
	b     KeyBudget
	found bool
}

// keyBudgetMemo is the per-request cache for GetKeyBudget reads.
// A chat request hits the key-budget guard twice — pre-body
// admission (checkKeyBudgetForModel, model unknown) and the
// post-decode check (checkKeyBudget) — and the 273-274 CPU profile
// put those two reads at 11.54% cum, the biggest named router
// cost. One memo per request turns the second read into a map
// lookup. The memo lives only as long as the request: a budget
// set or deleted after the request started is visible to the
// next request, never to this one — the same decision the
// request already made on its first read, so nothing is
// re-decided mid-request. It is keyed by key ID, so a request
// can never see another key's row. The mutex is for callers
// that share one request context across goroutines; the normal
// path is sequential and uncontended.
type keyBudgetMemo struct {
	mu   sync.Mutex
	rows map[string]keyBudgetMemoEntry
}

func (m *keyBudgetMemo) get(keyID string) (keyBudgetMemoEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.rows[keyID]
	return e, ok
}

func (m *keyBudgetMemo) put(keyID string, e keyBudgetMemoEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]keyBudgetMemoEntry{}
	}
	m.rows[keyID] = e
}

func (m *keyBudgetMemo) invalidate(keyID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, keyID)
}

// WithKeyBudgetMemo returns ctx carrying an empty per-request
// key-budget memo for GetKeyBudget. The server attaches exactly
// one memo per HTTP request (in withLogging, the outermost
// wrapper), so every guard on that request shares one database
// read per key. Never attach a memo to a long-lived context (a
// background job, a server-wide context): the memo is a
// request-lifetime snapshot, and a long-lived one would serve
// stale budget rows to later requests.
func WithKeyBudgetMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, keyBudgetMemoKey{}, &keyBudgetMemo{})
}

func keyBudgetMemoFrom(ctx context.Context) *keyBudgetMemo {
	m, _ := ctx.Value(keyBudgetMemoKey{}).(*keyBudgetMemo)
	return m
}

// invalidateKeyBudgetMemo drops keyID from the memo on ctx, if
// any — a write in the same request (SetKeyBudget /
// DeleteKeyBudget from the dashboard) must not leave a stale
// row behind for a later read in that same request.
func invalidateKeyBudgetMemo(ctx context.Context, keyID string) {
	if m := keyBudgetMemoFrom(ctx); m != nil {
		m.invalidate(keyID)
	}
}

// scanKeyBudgetRow scans one key_budgets row in the canonical
// SELECT order (key_id, limit_usd, unpriced_policy,
// daily_limit_usd, weekly_limit_usd, monthly_limit_usd,
// warning_threshold, reset_interval, reset_time). NULL calendar
// limits stay nil (unset). A stored row this build wrote is always
// valid; a hand-edited database with an unknown policy, interval,
// or reset time is an error, never silently reinterpreted.
func scanKeyBudgetRow(scan func(dest ...any) error) (KeyBudget, error) {
	var b KeyBudget
	var policy, interval, resetTime string
	var daily, weekly, monthly sql.NullFloat64
	if err := scan(&b.KeyID, &b.LimitUSD, &policy,
		&daily, &weekly, &monthly, &b.WarningThreshold,
		&interval, &resetTime); err != nil {
		return KeyBudget{}, err
	}
	p, ok := NormalizeUnpricedPolicy(policy)
	if !ok {
		return KeyBudget{}, fmt.Errorf("ledger: key budget %q has unknown unpriced policy %q", b.KeyID, policy)
	}
	b.UnpricedPolicy = p
	iv, ok := NormalizeBudgetResetInterval(interval)
	if !ok {
		return KeyBudget{}, fmt.Errorf("ledger: key budget %q has unknown reset interval %q", b.KeyID, interval)
	}
	b.ResetInterval = iv
	rt, ok := NormalizeBudgetResetTime(resetTime)
	if !ok {
		return KeyBudget{}, fmt.Errorf("ledger: key budget %q has malformed reset time %q", b.KeyID, resetTime)
	}
	b.ResetTime = rt
	if daily.Valid {
		v := daily.Float64
		b.DailyLimitUSD = &v
	}
	if weekly.Valid {
		v := weekly.Float64
		b.WeeklyLimitUSD = &v
	}
	if monthly.Valid {
		v := monthly.Float64
		b.MonthlyLimitUSD = &v
	}
	return b, nil
}

// SetKeyBudgetConfig creates or replaces the FULL budget row for a
// key: the 5-hour rolling cap plus the calendar-window caps and the
// active-window selector. Limits must be positive (nil = unset,
// unlimited for that window); a limit of 0 is rejected, not stored
// as "no spend allowed" — a $0 calendar cap would refuse every
// request while looking like a configured budget, and the operator
// clears a window by deleting the budget, not by zeroing it. (The
// 5-hour LimitUSD keeps its older rule: 0 = no spend allowed,
// because that field is a plain float64 — an omitted value decodes
// as 0 — and existing budgets rely on it.) WarningThreshold is
// clamped to [0, 1] (0 = never warn, 1 = warn only at the limit);
// an unknown interval or malformed reset time is an error. Unlike
// SetKeyBudget, this writes every column — callers that only mean
// to change the rolling cap must use SetKeyBudget, which leaves
// the window columns untouched.
func (l *Ledger) SetKeyBudgetConfig(ctx context.Context, b KeyBudget) error {
	if b.KeyID == "" {
		return errors.New("ledger: key budget key_id must not be empty")
	}
	if b.LimitUSD < 0 {
		return fmt.Errorf("ledger: key budget limit must not be negative (got %v)", b.LimitUSD)
	}
	for name, v := range map[string]*float64{"daily": b.DailyLimitUSD, "weekly": b.WeeklyLimitUSD, "monthly": b.MonthlyLimitUSD} {
		if v != nil && *v <= 0 {
			return fmt.Errorf("ledger: key budget %s limit must be positive (got %v)", name, *v)
		}
	}
	p, ok := NormalizeUnpricedPolicy(string(b.UnpricedPolicy))
	if !ok {
		return fmt.Errorf("ledger: unknown unpriced policy %q (want fail_closed or count_as_zero)", b.UnpricedPolicy)
	}
	iv, ok := NormalizeBudgetResetInterval(string(b.ResetInterval))
	if !ok {
		return fmt.Errorf("ledger: unknown reset interval %q (want daily, weekly or monthly)", b.ResetInterval)
	}
	rt, ok := NormalizeBudgetResetTime(b.ResetTime)
	if !ok {
		return fmt.Errorf("ledger: malformed reset time %q (want HH:MM)", b.ResetTime)
	}
	warn := b.WarningThreshold
	switch {
	case warn < 0:
		warn = 0
	case warn > 1:
		warn = 1
	}
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO key_budgets (key_id, limit_usd, unpriced_policy,
		                         daily_limit_usd, weekly_limit_usd, monthly_limit_usd,
		                         warning_threshold, reset_interval, reset_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_id) DO UPDATE SET limit_usd = excluded.limit_usd,
		                                   unpriced_policy = excluded.unpriced_policy,
		                                   daily_limit_usd = excluded.daily_limit_usd,
		                                   weekly_limit_usd = excluded.weekly_limit_usd,
		                                   monthly_limit_usd = excluded.monthly_limit_usd,
		                                   warning_threshold = excluded.warning_threshold,
		                                   reset_interval = excluded.reset_interval,
		                                   reset_time = excluded.reset_time`,
		b.KeyID, b.LimitUSD, string(p),
		nullableFloat(b.DailyLimitUSD), nullableFloat(b.WeeklyLimitUSD), nullableFloat(b.MonthlyLimitUSD),
		warn, string(iv), rt)
	if err != nil {
		return fmt.Errorf("ledger: set key budget config: %w", err)
	}
	invalidateKeyBudgetMemo(ctx, b.KeyID)
	return nil
}

// nullableFloat converts a *float64 limit to a driver value: nil
// stays NULL (unset), a value stays a value.
func nullableFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// SetKeyBudget creates or replaces the rolling 5-hour budget for a
// key. limitUSD must not be negative (0 = no spend allowed under
// enforcement, matching the global budget's cap-0 rule); policy
// "" means the default fail_closed. The row lives in the ledger
// database, so it survives a restart. Existing usage history is
// untouched — a budget applies to spend from the moment it is set,
// and the window only ever looks back 5 hours.
func (l *Ledger) SetKeyBudget(ctx context.Context, keyID string, limitUSD float64, policy UnpricedPolicy) error {
	if keyID == "" {
		return errors.New("ledger: key budget key_id must not be empty")
	}
	if limitUSD < 0 {
		return fmt.Errorf("ledger: key budget limit must not be negative (got %v)", limitUSD)
	}
	p, ok := NormalizeUnpricedPolicy(string(policy))
	if !ok {
		return fmt.Errorf("ledger: unknown unpriced policy %q (want fail_closed or count_as_zero)", policy)
	}
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO key_budgets (key_id, limit_usd, unpriced_policy)
		VALUES (?, ?, ?)
		ON CONFLICT(key_id) DO UPDATE SET limit_usd = excluded.limit_usd,
		                                   unpriced_policy = excluded.unpriced_policy`,
		keyID, limitUSD, string(p))
	if err != nil {
		return fmt.Errorf("ledger: set key budget: %w", err)
	}
	invalidateKeyBudgetMemo(ctx, keyID)
	return nil
}

// DeleteKeyBudget removes a key's rolling budget (the key becomes
// unlimited). Usage history is untouched.
func (l *Ledger) DeleteKeyBudget(ctx context.Context, keyID string) error {
	if _, err := l.db.ExecContext(ctx, `DELETE FROM key_budgets WHERE key_id = ?`, keyID); err != nil {
		return fmt.Errorf("ledger: delete key budget: %w", err)
	}
	invalidateKeyBudgetMemo(ctx, keyID)
	return nil
}

// GetKeyBudget returns the budget row for a key, or sql.ErrNoRows
// when the key has no budget (unlimited). Like GetBudget it runs
// on the prepared statement (see preparedGetKeyBudget): the
// per-request key-budget guard calls this on every request.
//
// When ctx carries a per-request memo (WithKeyBudgetMemo), the
// row — or its absence — is read from the database once per
// request per key and served from the memo after that, so the
// pre-body admission check and the post-decode check share one
// read (CHECKLIST 279-280: the 273-274 profile put the two reads
// at 11.54% cum). Without a memo the behaviour is unchanged: a
// fresh database read every call.
func (l *Ledger) GetKeyBudget(ctx context.Context, keyID string) (KeyBudget, error) {
	memo := keyBudgetMemoFrom(ctx)
	if memo != nil {
		if e, ok := memo.get(keyID); ok {
			if !e.found {
				return KeyBudget{}, sql.ErrNoRows
			}
			return e.b, nil
		}
	}
	var out KeyBudget
	var err error
	if stmt, ok := l.preparedGetKeyBudget(ctx); ok {
		out, err = scanKeyBudgetRow(func(dest ...any) error {
			return stmt.QueryRowContext(ctx, keyID).Scan(dest...)
		})
	} else {
		out, err = scanKeyBudgetRow(func(dest ...any) error {
			return l.db.QueryRowContext(ctx, `
				SELECT key_id, limit_usd, unpriced_policy,
				       daily_limit_usd, weekly_limit_usd, monthly_limit_usd,
				       warning_threshold, reset_interval, reset_time
				FROM key_budgets
				WHERE key_id = ?`, keyID).Scan(dest...)
		})
	}
	if memo != nil {
		switch {
		case err == nil:
			memo.put(keyID, keyBudgetMemoEntry{b: out, found: true})
		case errors.Is(err, sql.ErrNoRows):
			memo.put(keyID, keyBudgetMemoEntry{found: false})
		}
	}
	return out, err
}

// ListKeyBudgets returns every key budget, ordered by key ID.
func (l *Ledger) ListKeyBudgets(ctx context.Context) ([]KeyBudget, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT key_id, limit_usd, unpriced_policy,
		       daily_limit_usd, weekly_limit_usd, monthly_limit_usd,
		       warning_threshold, reset_interval, reset_time
		FROM key_budgets
		ORDER BY key_id`)
	if err != nil {
		return nil, fmt.Errorf("ledger: list key budgets: %w", err)
	}
	defer rows.Close()
	out := []KeyBudget{}
	for rows.Next() {
		b, err := scanKeyBudgetRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("ledger: list key budgets: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list key budgets: %w", err)
	}
	return out, nil
}

// KeyBudgetStatus reports one key's rolling-window position at
// now. A key with no budget row still gets its window spend and
// unknown-usage counts (HasBudget false, RemainingUSD nil) — the
// operator can see what a budget would cap before setting one.
func (l *Ledger) KeyBudgetStatus(ctx context.Context, keyID string, now time.Time) (KeyBudgetStatus, error) {
	b, err := l.GetKeyBudget(ctx, keyID)
	if err != nil && err != sql.ErrNoRows {
		return newKeyBudgetStatus(keyID, now), fmt.Errorf("ledger: key budget status: %w", err)
	}
	return l.keyBudgetStatusWithBudget(ctx, keyID, now, b, err == nil)
}

// newKeyBudgetStatus is the initial status every KeyBudgetStatus
// path starts from (unlimited until a budget row says otherwise).
func newKeyBudgetStatus(keyID string, now time.Time) KeyBudgetStatus {
	day := BudgetWindowFor(BudgetResetDaily, "00:00", now)
	week := BudgetWindowFor(BudgetResetWeekly, "00:00", now)
	month := BudgetWindowFor(BudgetResetMonthly, "00:00", now)
	return KeyBudgetStatus{
		KeyID:          keyID,
		UnpricedPolicy: UnpricedFailClosed,
		WindowStart:    now.Add(-RollingWindow).UTC(),
		WindowEnd:      now.UTC(),
		UnpricedModels: []string{},
		Daily:          KeyBudgetWindowStatus{PeriodStart: day.PeriodStart, NextReset: day.NextReset},
		Weekly:         KeyBudgetWindowStatus{PeriodStart: week.PeriodStart, NextReset: week.NextReset},
		Monthly:        KeyBudgetWindowStatus{PeriodStart: month.PeriodStart, NextReset: month.NextReset},
		ResetInterval:  BudgetResetDaily,
		ResetTime:      "00:00",
		ActiveWindow:   "5h",
	}
}

// activeKeyBudgetLimit selects the calendar window the STATUS
// reports as active (M5.1, OmniRoute costRules.ts
// getActiveBudgetLimit): monthly → the monthly limit, falling
// back to the daily limit when unset; weekly → the weekly limit,
// falling back to daily; daily (and anything else) → the daily
// limit. A nil limit means no calendar cap is active for display.
// B2: this selector is DISPLAY ONLY — enforcement checks every
// set window (enforceKeyBudgetCaps), so a monthly cap gates even
// when the interval is weekly, and vice versa.
func activeKeyBudgetLimit(b KeyBudget) (string, *float64) {
	switch b.ResetInterval {
	case BudgetResetMonthly:
		if b.MonthlyLimitUSD != nil {
			return "monthly", b.MonthlyLimitUSD
		}
		return "daily", b.DailyLimitUSD
	case BudgetResetWeekly:
		if b.WeeklyLimitUSD != nil {
			return "weekly", b.WeeklyLimitUSD
		}
		return "daily", b.DailyLimitUSD
	default:
		return "daily", b.DailyLimitUSD
	}
}

// keyBudgetWindowStatus fills one calendar window's status: the
// period bounds from the row's reset time, priced spend in the
// period from the ledger, and the remaining/at-limit verdict when
// the limit is set.
func (l *Ledger) keyBudgetWindowStatus(ctx context.Context, keyID string, interval BudgetResetInterval, resetTime string, limit *float64, now time.Time) (KeyBudgetWindowStatus, error) {
	w := BudgetWindowFor(interval, resetTime, now)
	st := KeyBudgetWindowStatus{LimitUSD: limit, PeriodStart: w.PeriodStart, NextReset: w.NextReset}
	spent, err := l.budgetWindowSpent(ctx, keyID, w.PeriodStart, w.NextReset)
	if err != nil {
		return st, err
	}
	st.SpentUSD = spent
	if limit != nil {
		remaining := *limit - spent
		st.RemainingUSD = &remaining
		st.AtLimit = spent >= *limit
	}
	return st, nil
}

// addReservedToWindow folds committed (reserved) spend into one
// calendar window's status: the spend, and the remaining/at-limit
// derived from it. The window's period already contains now, so
// a reservation made now belongs to it.
func addReservedToWindow(w *KeyBudgetWindowStatus, reserved float64) {
	w.SpentUSD += reserved
	if w.LimitUSD != nil {
		remaining := *w.LimitUSD - w.SpentUSD
		w.RemainingUSD = &remaining
		w.AtLimit = w.SpentUSD >= *w.LimitUSD
	}
}

// keyBudgetStatusWithBudget is KeyBudgetStatus given the row the
// caller already read: b is the budget row, found=false means
// the key has no row (unlimited). CheckKeyBudget passes the row
// it just read, so a guarded request reads the key_budgets row
// once, not twice (the old code read it in CheckKeyBudget and
// again inside KeyBudgetStatus).
// keyBudgetWindowWarns reports whether one set window has reached
// its warning threshold; threshold 0 means "never warn" (a 0
// threshold would otherwise make spent >= 0 always true).
func keyBudgetWindowWarns(b KeyBudget, limit *float64, spent float64) bool {
	return limit != nil && b.WarningThreshold > 0 && spent >= b.WarningThreshold**limit
}

func (l *Ledger) keyBudgetStatusWithBudget(ctx context.Context, keyID string, now time.Time, b KeyBudget, found bool) (KeyBudgetStatus, error) {
	st := newKeyBudgetStatus(keyID, now)
	if found {
		st.HasBudget = true
		st.LimitUSD = b.LimitUSD
		st.UnpricedPolicy = b.UnpricedPolicy
	}
	sinceMs := st.WindowStart.UnixMilli()
	// Priced spend in the window: successful or failed, the cost
	// column already holds the priced cost (failed attempts and
	// shared rows record 0 — fallback attempts consume no quota).
	if err := l.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_usd), 0) FROM usage_events
		WHERE gateway_key_id = ? AND ts >= ? AND shared = 0`,
		keyID, sinceMs).Scan(&st.SpentUSD); err != nil {
		return st, fmt.Errorf("ledger: key budget status spend: %w", err)
	}
	// M5.8: committed spend counts like settled spend — the
	// gate and the reporting both see what is already promised
	// to in-flight requests, not just what has landed. The
	// reservation sum is added to the 5h spend here (before the
	// AtLimit computation below) and to each calendar window
	// after it is computed (a reservation made now sits in the
	// current period of every window). Callers that need the
	// check+insert atomicity (CheckKeyBudget, ReserveSpend)
	// hold reserveMu around this call; reporting callers read a
	// point-in-time view.
	reserved, err := l.reservedSpendLocked(ctx, keyID, now)
	if err != nil {
		return st, err
	}
	st.SpentUSD += reserved
	if st.HasBudget {
		remaining := st.LimitUSD - st.SpentUSD
		st.RemainingUSD = &remaining
		st.AtLimit = st.SpentUSD >= st.LimitUSD
	}
	// M5.1 calendar windows: spend is computed for all three
	// windows whether or not a limit is set, so the operator sees
	// what a cap would catch. The reset time comes from the row;
	// a key with no row reports the default daily/00:00 windows.
	st.ResetInterval = b.ResetInterval
	st.ResetTime = b.ResetTime
	if st.ResetInterval == "" {
		st.ResetInterval = BudgetResetDaily
	}
	if st.ResetTime == "" {
		st.ResetTime = "00:00"
	}
	if st.Daily, err = l.keyBudgetWindowStatus(ctx, keyID, BudgetResetDaily, st.ResetTime, b.DailyLimitUSD, now); err != nil {
		return st, fmt.Errorf("ledger: key budget status daily window: %w", err)
	}
	addReservedToWindow(&st.Daily, reserved)
	if st.Weekly, err = l.keyBudgetWindowStatus(ctx, keyID, BudgetResetWeekly, st.ResetTime, b.WeeklyLimitUSD, now); err != nil {
		return st, fmt.Errorf("ledger: key budget status weekly window: %w", err)
	}
	addReservedToWindow(&st.Weekly, reserved)
	if st.Monthly, err = l.keyBudgetWindowStatus(ctx, keyID, BudgetResetMonthly, st.ResetTime, b.MonthlyLimitUSD, now); err != nil {
		return st, fmt.Errorf("ledger: key budget status monthly window: %w", err)
	}
	addReservedToWindow(&st.Monthly, reserved)
	// The active window is the one the STATUS reports as active
	// (display only). When no calendar limit is active the 5-hour
	// rolling cap is the active window (its name stays "5h" so
	// callers can tell). B2: enforcement no longer follows this
	// selector — every set window gates — so AtLimit must be true
	// when ANY set window is at its cap, not just the active one,
	// or the dashboard would show a key as under budget while the
	// gate refuses it. WarningReached follows the same rule (see
	// below): it is no longer active-window-only either.
	if name, limit := activeKeyBudgetLimit(b); limit != nil {
		st.ActiveWindow = name
		st.ActiveLimitUSD = limit
		switch name {
		case "daily":
			st.AtLimit = st.AtLimit || st.Daily.AtLimit
		case "weekly":
			st.AtLimit = st.AtLimit || st.Weekly.AtLimit
		case "monthly":
			st.AtLimit = st.AtLimit || st.Monthly.AtLimit
		}
	} else {
		st.ActiveWindow = "5h"
	}
	// WarningReached ORs across EVERY set window, the same way
	// AtLimit does below: a non-active window at 95% must warn,
	// or the key would hard-refuse at 100% with no warning first.
	// When no calendar window is set the OR is empty (false),
	// which keeps the old active-window behaviour — the 5h window
	// has no warning threshold — and when no limit is set at all
	// there is nothing to warn about.
	st.WarningReached = keyBudgetWindowWarns(b, b.DailyLimitUSD, st.Daily.SpentUSD) ||
		keyBudgetWindowWarns(b, b.WeeklyLimitUSD, st.Weekly.SpentUSD) ||
		keyBudgetWindowWarns(b, b.MonthlyLimitUSD, st.Monthly.SpentUSD)
	st.AtLimit = st.AtLimit || st.Daily.AtLimit || st.Weekly.AtLimit || st.Monthly.AtLimit
	// Unknown spend in the window: successful attempts whose cost
	// is unknown — the upstream never reported a usable split, or
	// the (provider, model) pair has no configured price. Failed
	// attempts are excluded (ok = 0): a failed attempt bought no
	// tokens, so its $0 is known, not unknown. Shared rows are
	// excluded: the leader's row carries the call.
	if err := l.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(total_tokens), 0) FROM usage_events
		WHERE gateway_key_id = ? AND ts >= ? AND shared = 0 AND ok = 1
		  AND (tokens_split_known = 0 OR price_known = 0)`,
		keyID, sinceMs).Scan(&st.UnknownEvents, &st.UnknownTokens); err != nil {
		return st, fmt.Errorf("ledger: key budget status unknown spend: %w", err)
	}
	// Which models the unknown spend came from with a known split
	// but no price — the models an operator must price (or switch
	// to count_as_zero) to make the spend knowable. Split-unknown
	// rows name no model price gap (their usage was never
	// reported), so they are counted above, not listed here.
	rows, err := l.db.QueryContext(ctx, `
		SELECT DISTINCT model FROM usage_events
		WHERE gateway_key_id = ? AND ts >= ? AND shared = 0 AND ok = 1
		  AND tokens_split_known = 1 AND price_known = 0
		ORDER BY model`,
		keyID, sinceMs)
	if err != nil {
		return st, fmt.Errorf("ledger: key budget status unpriced models: %w", err)
	}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			rows.Close()
			return st, fmt.Errorf("ledger: key budget status unpriced models: %w", err)
		}
		st.UnpricedModels = append(st.UnpricedModels, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return st, fmt.Errorf("ledger: key budget status unpriced models: %w", err)
	}
	rows.Close()
	// Next roll-off: the oldest priced spend in the window ages
	// out 5 hours after it was recorded.
	var oldestMs sql.NullInt64
	if err := l.db.QueryRowContext(ctx, `
		SELECT MIN(ts) FROM usage_events
		WHERE gateway_key_id = ? AND ts >= ? AND shared = 0 AND cost_usd > 0`,
		keyID, sinceMs).Scan(&oldestMs); err != nil {
		return st, fmt.Errorf("ledger: key budget status rolloff: %w", err)
	}
	if oldestMs.Valid {
		t := time.UnixMilli(oldestMs.Int64).Add(RollingWindow).UTC()
		st.NextRolloff = &t
	}
	sort.Strings(st.UnpricedModels)
	return st, nil
}

// CheckKeyBudget is the fail-closed guard to call BEFORE routing a
// request: a key at its limit is refused before any provider is
// called, so no further spend can happen. models are the concrete
// models this request would use (already resolved — for auto/*
// the caller resolves the virtual model first); priced reports
// whether a model has a known price (nil = every model is treated
// as unpriced). B2: the gate refuses when the 5-hour rolling cap
// OR any set daily / weekly / monthly cap is reached — each
// window against its own UTC calendar period's spend (see
// enforceKeyBudgetCaps) — and a set window whose current period
// already holds unpriced usage closes that period for ALL models
// under fail_closed (see enforceKeyBudgetUnpriced). It returns:
//
//   - nil: the request may proceed.
//   - *BudgetExceededError: priced spend in any set window (the
//     rolling 5h cap or a set daily / weekly / monthly window) is
//     at its cap — the caller must refuse with 402 naming that window.
//   - *UnpricedUsageError: priced spend fits, but the request
//     would use unpriced models under the fail_closed policy — the
//     caller must refuse with 400 naming those models. This error
//     is returned only when the block is caused by unpriced usage
//     alone: an over-cap key gets the 402, not the 400.
//
// A key with no budget row is unlimited (nil). Unknown spend
// already in the window never blocks by itself: it is reported in
// KeyBudgetStatus for the operator to see, but it is neither
// counted as $0 nor silently counted as spent.
func (l *Ledger) CheckKeyBudget(ctx context.Context, keyID string, models []string, priced func(string) bool, now time.Time) error {
	b, err := l.GetKeyBudget(ctx, keyID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("ledger: check key budget: %w", err)
	}
	// M5.8: the status read holds reserveMu, so it cannot
	// interleave with a ReserveSpend check+insert — the gate
	// sees committed + settled spend as one consistent snapshot.
	l.reserveMu.Lock()
	defer l.reserveMu.Unlock()
	// The row is already read: pass it into the status instead of
	// reading key_budgets a second time (CHECKLIST 279-280).
	st, err := l.keyBudgetStatusWithBudget(ctx, keyID, now, b, true)
	if err != nil {
		return err
	}
	if err := enforceKeyBudgetCaps(st, b, 0); err != nil {
		return err
	}
	return enforceKeyBudgetUnpriced(ctx, l, keyID, models, priced, now, b, st)
}

// enforceKeyBudgetCaps enforces the 5h cap and EVERY set calendar
// window against a status (which already counts settled +
// reserved spend), plus an extra committed amount: 0 for a plain
// check, the new estimate for a ReserveSpend call. B2: each
// window gates on its OWN period's spend — daily [today
// resetTime, tomorrow resetTime), weekly [Monday resetTime, next
// Monday resetTime), monthly [1st resetTime, next 1st resetTime),
// all UTC (BudgetWindowFor) — so a request is refused with a 402
// naming the first breached window (5h first, then daily,
// weekly, monthly) as soon as ANY cap is reached. A window with
// no limit set never gates. The extra estimate counts against
// every window at once: the request's cost lands in all three
// periods, so it must fit all three.
func enforceKeyBudgetCaps(st KeyBudgetStatus, b KeyBudget, extra float64) error {
	if st.SpentUSD+extra >= b.LimitUSD {
		return &BudgetExceededError{
			KeyID:        st.KeyID,
			Window:       "5h",
			CapUSD:       b.LimitUSD,
			SpentUSD:     st.SpentUSD + extra,
			RemainingUSD: b.LimitUSD - (st.SpentUSD + extra),
		}
	}
	// B2: every set calendar window gates, not just the one
	// ResetInterval names as "active" — the status already
	// computed each window's spend for its own UTC period, so
	// this is a per-window comparison, never a shared total.
	for _, w := range []struct {
		name  string
		st    KeyBudgetWindowStatus
		limit *float64
	}{
		{"daily", st.Daily, b.DailyLimitUSD},
		{"weekly", st.Weekly, b.WeeklyLimitUSD},
		{"monthly", st.Monthly, b.MonthlyLimitUSD},
	} {
		if w.limit == nil {
			continue
		}
		if w.st.SpentUSD+extra >= *w.limit {
			return &BudgetExceededError{
				KeyID:        st.KeyID,
				Window:       w.name,
				CapUSD:       *w.limit,
				SpentUSD:     w.st.SpentUSD + extra,
				RemainingUSD: *w.limit - (w.st.SpentUSD + extra),
			}
		}
	}
	return nil
}

// enforceKeyBudgetUnpriced is the closed-period + per-request
// unpriced part of CheckKeyBudget, shared with ReserveSpend:
// the closed-period check (any unpriced usage in the current
// period of a SET calendar window closes that window for ALL
// models under fail_closed) and the per-request check (the
// request's own models, refused when unpriced under fail_closed).
func enforceKeyBudgetUnpriced(ctx context.Context, l *Ledger, keyID string, models []string, priced func(string) bool, now time.Time, b KeyBudget, st KeyBudgetStatus) error {
	_ = now
	// B2 closed-period fail-closed (M5.1 did this for the month
	// only, OmniRoute keyQuota.ts isMonthClosedByUnpricedUsage;
	// B2 extends it to every set window): when a daily, weekly,
	// or monthly cap is set, any unpriced usage in that window's
	// current UTC period closes the period for ALL models under
	// fail_closed — the spend cannot be proven against the cap,
	// so the key is refused until the period ends. count_as_zero
	// opts out (the operator accepted $0 for unpriced usage).
	// The ledger IS the counter: no separate table, the
	// usage_events rows decide. Monthly is checked first so the
	// existing month-closed error (MonthClosed=true) keeps its
	// exact shape.
	if b.UnpricedPolicy == UnpricedFailClosed {
		for _, w := range []struct {
			name  string
			limit *float64
			st    KeyBudgetWindowStatus
		}{
			{"monthly", b.MonthlyLimitUSD, st.Monthly},
			{"weekly", b.WeeklyLimitUSD, st.Weekly},
			{"daily", b.DailyLimitUSD, st.Daily},
		} {
			if w.limit == nil {
				continue
			}
			unpriced, err := l.keyBudgetWindowUnpricedModels(ctx, keyID, w.st.PeriodStart, w.st.NextReset)
			if err != nil {
				return err
			}
			if len(unpriced) > 0 {
				return &UnpricedUsageError{
					KeyID:       keyID,
					Models:      unpriced,
					LimitUSD:    *w.limit,
					SpentUSD:    w.st.SpentUSD,
					MonthClosed: w.name == "monthly",
					Window:      w.name,
				}
			}
		}
	}
	if b.UnpricedPolicy == UnpricedFailClosed {
		var unpriced []string
		seen := map[string]bool{}
		for _, m := range models {
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			if priced == nil || !priced(m) {
				unpriced = append(unpriced, m)
			}
		}
		if len(unpriced) > 0 {
			sort.Strings(unpriced)
			return &UnpricedUsageError{
				KeyID:    keyID,
				Models:   unpriced,
				LimitUSD: b.LimitUSD,
				SpentUSD: st.SpentUSD,
			}
		}
	}
	return nil
}

// keyBudgetWindowUnpricedModels lists the distinct models with
// unpriced usage in [start, end): successful, non-shared attempts
// whose price was unknown at record time. Empty means the period
// is not closed by unpriced usage. (M5.1 named this
// keyBudgetMonthUnpricedModels and called it for the month only;
// B2 calls it for every set window's current period.)
func (l *Ledger) keyBudgetWindowUnpricedModels(ctx context.Context, keyID string, start, end time.Time) ([]string, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT DISTINCT model FROM usage_events
		WHERE gateway_key_id = ? AND ts >= ? AND ts < ? AND shared = 0 AND ok = 1
		  AND price_known = 0
		ORDER BY model`,
		keyID, start.UnixMilli(), end.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("ledger: key budget window unpriced models: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("ledger: key budget window unpriced models: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: key budget window unpriced models: %w", err)
	}
	return out, nil
}
