package ledger

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// M5.11: internal scoped budgets — the operator's own caps on
// total spend (the reference router lib/usage/budgetGuard.ts:
// InternalBudgetLimit + evaluateBudget). A budget names a scope
// (global / provider / model / pool), a calendar period (daily /
// weekly / monthly), a limit type (currency / tokens / requests)
// and a value; the evaluation is allow / warn / deny, with the
// warning threshold (default 0.8) reporting "warn" instead of
// "allow".
//
// DIVERGENCE (documented): the reference router defines the type and the
// evaluator but wires no enforcement into the gateway path in
// the current checkout — evaluateBudget has no callers. We
// enforce it: the server gate refuses global/model/pool scopes
// pre-call, the engine skips provider-scoped targets
// per-attempt. The ledger IS the counter (usage_events, ok=1,
// shared=0), the same rule as every other M5 budget.

// InternalBudgetScope names which spend a budget caps.
const (
	InternalBudgetScopeGlobal   = "global"
	InternalBudgetScopeProvider = "provider"
	InternalBudgetScopeModel    = "model"
	InternalBudgetScopePool     = "pool"
)

// InternalBudgetLimitType names what is counted.
const (
	InternalBudgetLimitCurrency = "currency"
	InternalBudgetLimitTokens   = "tokens"
	InternalBudgetLimitRequests = "requests"
)

// InternalBudget is one operator cap on total spend.
type InternalBudget struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`
	// ProviderID / ModelID / PoolID name the scoped entity
	// (required for provider / model / pool scopes, ignored
	// for global).
	ProviderID string `json:"provider_id,omitempty"`
	ModelID    string `json:"model_id,omitempty"`
	PoolID     string `json:"pool_id,omitempty"`
	// Period is a calendar window: daily / weekly / monthly
	// (UTC, via BudgetWindowFor).
	Period string `json:"period"`
	// LimitType is currency (USD) / tokens / requests.
	LimitType string `json:"limit_type"`
	// LimitValue is the cap in the limit type's units.
	LimitValue float64 `json:"limit_value"`
	// WarningThreshold is the used fraction (0-1) at which the
	// evaluation reports warn instead of allow; 0 is stored as
	// the default 0.8.
	WarningThreshold float64 `json:"warning_threshold"`
	Enabled          bool    `json:"enabled"`
}

// InternalBudgetDecision is allow / warn / deny.
type InternalBudgetDecision string

const (
	InternalBudgetAllow InternalBudgetDecision = "allow"
	InternalBudgetWarn  InternalBudgetDecision = "warn"
	InternalBudgetDeny  InternalBudgetDecision = "deny"
)

// InternalBudgetEvaluation is one budget's verdict.
type InternalBudgetEvaluation struct {
	Decision  InternalBudgetDecision
	Budget    *InternalBudget
	Used      float64
	Remaining float64
	Reason    string
}

// Error renders a deny as a refusal message (the server writes
// it as the 402 body).
func (e *InternalBudgetEvaluation) Error() string {
	if e == nil || e.Budget == nil {
		return "internal budget exceeded"
	}
	return fmt.Sprintf("internal %s budget %q exhausted (%s: used %.2f of %.2f)", e.Budget.Scope, e.Budget.ID, e.Budget.LimitType, e.Used, e.Budget.LimitValue)
}

// evaluateInternalBudget is the port of the reference router's
// evaluateBudget: a disabled budget allows; an invalid one
// (non-positive limit) denies; used >= limit denies; used >=
// limit * warningThreshold warns; otherwise allows.
func evaluateInternalBudget(b InternalBudget, used float64) InternalBudgetEvaluation {
	if !b.Enabled {
		return InternalBudgetEvaluation{Decision: InternalBudgetAllow, Budget: &b, Used: 0, Reason: "No enabled internal budget applies."}
	}
	if b.LimitValue <= 0 {
		return InternalBudgetEvaluation{Decision: InternalBudgetDeny, Budget: &b, Used: 0, Remaining: 0, Reason: "Internal budget configuration is invalid."}
	}
	remaining := b.LimitValue - used
	if remaining < 0 {
		remaining = 0
	}
	if used >= b.LimitValue {
		return InternalBudgetEvaluation{Decision: InternalBudgetDeny, Budget: &b, Used: used, Remaining: remaining, Reason: "Internal budget exhausted."}
	}
	threshold := b.WarningThreshold
	if threshold == 0 {
		threshold = 0.8
	}
	if used/b.LimitValue >= threshold {
		return InternalBudgetEvaluation{
			Decision: InternalBudgetWarn, Budget: &b, Used: used, Remaining: remaining,
			Reason: fmt.Sprintf("Internal budget is %d%% consumed.", int(used/b.LimitValue*100)),
		}
	}
	return InternalBudgetEvaluation{Decision: InternalBudgetAllow, Budget: &b, Used: used, Remaining: remaining, Reason: "Internal budget permits the request."}
}

// internalBudgetUsed sums one budget's usage in its period
// window from usage_events (ok=1, shared=0 — our own spend
// only, the ledger-is-the-counter rule). The window is the UTC
// calendar period (midnight reset, like the key-budget windows).
func (l *Ledger) internalBudgetUsed(ctx context.Context, b InternalBudget, now time.Time) (float64, error) {
	window := BudgetWindowFor(budgetPeriodToInterval(b.Period), "00:00", now)
	start, end := window.PeriodStart, window.NextReset
	var selectExpr string
	switch b.LimitType {
	case InternalBudgetLimitCurrency:
		selectExpr = "COALESCE(SUM(cost_usd), 0)"
	case InternalBudgetLimitTokens:
		selectExpr = "COALESCE(SUM(total_tokens), 0)"
	case InternalBudgetLimitRequests:
		selectExpr = "COUNT(*)"
	default:
		return 0, fmt.Errorf("ledger: unknown internal budget limit type %q", b.LimitType)
	}
	query := fmt.Sprintf(`SELECT %s FROM usage_events WHERE ok = 1 AND shared = 0 AND ts >= ? AND ts < ?`, selectExpr)
	args := []any{start.UnixMilli(), end.UnixMilli()}
	switch b.Scope {
	case InternalBudgetScopeProvider:
		query += ` AND provider = ?`
		args = append(args, b.ProviderID)
	case InternalBudgetScopeModel:
		query += ` AND model = ?`
		args = append(args, b.ModelID)
	case InternalBudgetScopePool:
		pool, err := l.GetQuotaPool(ctx, b.PoolID)
		if err != nil {
			return 0, fmt.Errorf("ledger: internal budget pool: %w", err)
		}
		if len(pool.Allocations) == 0 {
			return 0, nil
		}
		placeholders := make([]string, len(pool.Allocations))
		for i, a := range pool.Allocations {
			placeholders[i] = "?"
			args = append(args, a.KeyID)
		}
		query += ` AND gateway_key_id IN (` + strings.Join(placeholders, ",") + `)`
	case InternalBudgetScopeGlobal:
		// No filter: every key's spend counts.
	default:
		return 0, fmt.Errorf("ledger: unknown internal budget scope %q", b.Scope)
	}
	var used float64
	if err := l.db.QueryRowContext(ctx, query, args...).Scan(&used); err != nil {
		return 0, fmt.Errorf("ledger: internal budget usage: %w", err)
	}
	return used, nil
}

// budgetPeriodToInterval maps an internal-budget period to the
// BudgetResetInterval the window helper understands.
func budgetPeriodToInterval(period string) BudgetResetInterval {
	switch period {
	case "daily":
		return BudgetResetDaily
	case "weekly":
		return BudgetResetWeekly
	case "monthly":
		return BudgetResetMonthly
	default:
		return BudgetResetInterval(period)
	}
}

// InternalBudgetUsage returns one budget's current usage in its
// period window plus the window's next reset (for the dashboard
// feed).
func (l *Ledger) InternalBudgetUsage(ctx context.Context, b InternalBudget, now time.Time) (used float64, nextReset time.Time, err error) {
	used, err = l.internalBudgetUsed(ctx, b, now)
	if err != nil {
		return 0, time.Time{}, err
	}
	window := BudgetWindowFor(budgetPeriodToInterval(b.Period), "00:00", now)
	return used, window.NextReset, nil
}

// EvaluateInternalBudgets evaluates every enabled budget that
// applies to this request — global always; provider when it
// names provider; model when it names model; pool when its pool
// is in poolIDs — and returns the most restrictive verdict:
// deny beats warn beats allow; within the same decision the
// smallest remaining wins (tie-break: smaller limit, then id).
// A nil evaluation means no budget applies (all allow).
func (l *Ledger) EvaluateInternalBudgets(ctx context.Context, provider, model string, poolIDs []string, now time.Time) (*InternalBudgetEvaluation, error) {
	budgets, err := l.ListInternalBudgets(ctx)
	if err != nil {
		return nil, err
	}
	poolSet := map[string]bool{}
	for _, id := range poolIDs {
		poolSet[id] = true
	}
	var worst *InternalBudgetEvaluation
	for _, b := range budgets {
		if !b.Enabled {
			continue
		}
		switch b.Scope {
		case InternalBudgetScopeGlobal:
		case InternalBudgetScopeProvider:
			if b.ProviderID != provider {
				continue
			}
		case InternalBudgetScopeModel:
			if b.ModelID != model {
				continue
			}
		case InternalBudgetScopePool:
			if !poolSet[b.PoolID] {
				continue
			}
		default:
			continue
		}
		used, err := l.internalBudgetUsed(ctx, b, now)
		if err != nil {
			return nil, err
		}
		ev := evaluateInternalBudget(b, used)
		if worst == nil || moreRestrictive(ev, *worst) {
			evCopy := ev
			worst = &evCopy
		}
	}
	return worst, nil
}

// CheckInternalBudgetForProvider evaluates only the
// provider-scoped budgets for one provider (the engine's
// per-attempt check). Nil means no provider budget blocks.
func (l *Ledger) CheckInternalBudgetForProvider(ctx context.Context, provider string, now time.Time) (*InternalBudgetEvaluation, error) {
	budgets, err := l.ListInternalBudgets(ctx)
	if err != nil {
		return nil, err
	}
	var worst *InternalBudgetEvaluation
	for _, b := range budgets {
		if !b.Enabled || b.Scope != InternalBudgetScopeProvider || b.ProviderID != provider {
			continue
		}
		used, err := l.internalBudgetUsed(ctx, b, now)
		if err != nil {
			return nil, err
		}
		ev := evaluateInternalBudget(b, used)
		if worst == nil || moreRestrictive(ev, *worst) {
			evCopy := ev
			worst = &evCopy
		}
	}
	return worst, nil
}

// moreRestrictive reports whether a beats b: deny > warn >
// allow; within the same decision, smaller remaining wins,
// then smaller limit, then id order.
func moreRestrictive(a, b InternalBudgetEvaluation) bool {
	rank := func(d InternalBudgetDecision) int {
		switch d {
		case InternalBudgetDeny:
			return 2
		case InternalBudgetWarn:
			return 1
		default:
			return 0
		}
	}
	if rank(a.Decision) != rank(b.Decision) {
		return rank(a.Decision) > rank(b.Decision)
	}
	if a.Remaining != b.Remaining {
		return a.Remaining < b.Remaining
	}
	if a.Budget.LimitValue != b.Budget.LimitValue {
		return a.Budget.LimitValue < b.Budget.LimitValue
	}
	return a.Budget.ID < b.Budget.ID
}

// ---------------------------------------------------------------------------
// Storage.
// ---------------------------------------------------------------------------

// SetInternalBudget upserts one internal budget. A zero warning
// threshold is stored as the default 0.8.
func (l *Ledger) SetInternalBudget(ctx context.Context, b InternalBudget) error {
	if b.ID == "" {
		return fmt.Errorf("ledger: internal budget id must not be empty")
	}
	if b.WarningThreshold == 0 {
		b.WarningThreshold = 0.8
	}
	if _, err := l.db.ExecContext(ctx, `
		INSERT INTO internal_budgets (id, scope, provider_id, model_id, pool_id, period, limit_type, limit_value, warning_threshold, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET scope=excluded.scope, provider_id=excluded.provider_id, model_id=excluded.model_id, pool_id=excluded.pool_id, period=excluded.period, limit_type=excluded.limit_type, limit_value=excluded.limit_value, warning_threshold=excluded.warning_threshold, enabled=excluded.enabled`,
		b.ID, b.Scope, b.ProviderID, b.ModelID, b.PoolID, b.Period, b.LimitType, b.LimitValue, b.WarningThreshold, boolToInt(b.Enabled)); err != nil {
		return fmt.Errorf("ledger: set internal budget: %w", err)
	}
	return nil
}

// GetInternalBudget returns one internal budget by id.
func (l *Ledger) GetInternalBudget(ctx context.Context, id string) (InternalBudget, error) {
	row := l.db.QueryRowContext(ctx, `
		SELECT id, scope, provider_id, model_id, pool_id, period, limit_type, limit_value, warning_threshold, enabled
		FROM internal_budgets WHERE id = ?`, id)
	return scanInternalBudgetRow(row)
}

// ListInternalBudgets returns all internal budgets, id order.
func (l *Ledger) ListInternalBudgets(ctx context.Context) ([]InternalBudget, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT id, scope, provider_id, model_id, pool_id, period, limit_type, limit_value, warning_threshold, enabled
		FROM internal_budgets ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("ledger: list internal budgets: %w", err)
	}
	defer rows.Close()
	var out []InternalBudget
	for rows.Next() {
		b, err := scanInternalBudgetRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteInternalBudget removes one internal budget.
func (l *Ledger) DeleteInternalBudget(ctx context.Context, id string) error {
	if _, err := l.db.ExecContext(ctx, `DELETE FROM internal_budgets WHERE id = ?`, id); err != nil {
		return fmt.Errorf("ledger: delete internal budget: %w", err)
	}
	return nil
}

type internalBudgetRowScanner interface {
	Scan(dest ...any) error
}

func scanInternalBudgetRow(row internalBudgetRowScanner) (InternalBudget, error) {
	var b InternalBudget
	var enabled int
	if err := row.Scan(&b.ID, &b.Scope, &b.ProviderID, &b.ModelID, &b.PoolID, &b.Period, &b.LimitType, &b.LimitValue, &b.WarningThreshold, &enabled); err != nil {
		return InternalBudget{}, fmt.Errorf("ledger: internal budget row: %w", err)
	}
	b.Enabled = enabled != 0
	return b, nil
}
