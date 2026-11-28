// Fair-share quota pools (M5.4, the sector baseline lib/quota/fairShare.ts
// + dimensions.ts, verified 2026-11-18).
//
// A pool shares one quota across its member gateway keys. Each
// dimension (unit x window) has a global pool limit; each member
// key has a weight (0-100, its share of the pool), an optional
// absolute cap, and a policy (hard | soft | burst). The decision
// is the sector-typical decideFairShare, ported faithfully:
//
//   - No dimensions → allow.
//   - Unknown policy → hard (fail-safe: never silently allow).
//   - Absolute cap (capValue in the dimension's unit) → block
//     "cap-absolute", always, in either mode.
//   - Pool global limit reached → block "global-saturated", all
//     policies (burst included — no room means no room).
//   - Strict mode (globalUsedPercent >= saturationThreshold,
//     default 0.5): hard blocks at the fair share (weight% of the
//     limit); soft allows but marks the request penalized; burst
//     allows while global headroom exists.
//   - Generous mode (below the threshold): borrowing is allowed —
//     hard allows up to the global limit, soft allows + penalized
//     past its fair share, burst allows.
//
// The ledger IS the counter: consumption is summed from
// usage_events inside the dimension's window (successful rows
// only, shared rows excluded — the same rules as the USD budgets
// and token limits), so no counter table exists to drift.
// the sector baseline keeps separate quota_consumption buckets with a
// sliding-window decay (prev x (1 - elapsed/window) + curr); ours
// sums the window directly — a stricter, simpler count with no
// decay estimate.
//
// Units: requests (COUNT), tokens (SUM total_tokens), usd
// (SUM cost_usd). The "percent" unit has no meaning without an
// external quota to take a percentage of, so config validation
// rejects it. Windows: 5h (rolling), hourly (calendar hour),
// daily / weekly / monthly (calendar, via BudgetWindowFor).
package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// QuotaDimension is one (unit x window) axis of a pool: the
// global pool limit for that unit inside that window.
type QuotaDimension struct {
	Unit   string  `json:"unit"`
	Window string  `json:"window"`
	Limit  float64 `json:"limit"`
}

// PoolAllocation is one member key's share of a pool.
type PoolAllocation struct {
	KeyID    string   `json:"key_id"`
	Weight   float64  `json:"weight"`
	CapValue *float64 `json:"cap_value,omitempty"`
	CapUnit  string   `json:"cap_unit,omitempty"`
	Policy   string   `json:"policy"`
}

// QuotaPool is one fair-share pool: its dimensions, its member
// allocations, and the saturation threshold that flips generous
// mode into strict mode (0 = the 0.5 default). AccountCount is
// the number of upstream accounts backing the pool (M5.5,
// the sector baseline enforce.ts:199 — the dimension limit is a PER-ACCOUNT
// limit, and the pool's effective limit is limit × accounts:
// N accounts contribute N × L capacity to the shared bucket,
// while consumption stays pool-keyed). 0 = 1 (a single account —
// the M5.4 behaviour, where the dimension limit already was the
// pool limit).
type QuotaPool struct {
	ID                  string           `json:"id"`
	Name                string           `json:"name"`
	SaturationThreshold float64          `json:"saturation_threshold"`
	AccountCount        int              `json:"account_count"`
	Dimensions          []QuotaDimension `json:"dimensions"`
	Allocations         []PoolAllocation `json:"allocations"`
}

// EffectiveAccountCount is the account multiplier, never below 1.
func (p QuotaPool) EffectiveAccountCount() int {
	if p.AccountCount < 1 {
		return 1
	}
	return p.AccountCount
}

// EffectiveLimit is the pool's real ceiling for one dimension:
// the per-account limit times the number of backing accounts.
func (p QuotaPool) EffectiveLimit(dim QuotaDimension) float64 {
	return dim.Limit * float64(p.EffectiveAccountCount())
}

// FairShareBlock is a refused pool decision: the pool, the reason
// (the sector-typical kind/reason names), the effective limit that was
// hit (per-account limit × accounts), and when the blocking window
// resets (so the caller can set an honest Retry-After; for the
// rolling 5h window the bound is now + 5h — usage ages out
// continuously inside it).
type FairShareBlock struct {
	PoolID         string
	Reason         string // "fair-share" | "cap-absolute" | "global-saturated"
	EffectiveLimit float64
	NextReset      time.Time
}

func (b *FairShareBlock) Error() string {
	return fmt.Sprintf("quota pool %s: request blocked (%s); window resets at %s",
		b.PoolID, b.Reason, b.NextReset.UTC().Format(time.RFC3339))
}

// normalizePoolPolicy is the sector-typical fail-safe: any policy outside
// hard | soft | burst is treated as hard (most restrictive), so a
// corrupted row can never silently bypass enforcement.
func normalizePoolPolicy(policy string) string {
	switch policy {
	case "hard", "soft", "burst":
		return policy
	default:
		return "hard"
	}
}

// poolWindow returns the [start, end) window containing now for a
// dimension window name. 5h is rolling; hourly is the calendar
// hour; daily/weekly/monthly are the M5.1 calendar windows.
func poolWindow(window string, now time.Time) (start, end time.Time, ok bool) {
	switch window {
	case "5h":
		return now.Add(-5 * time.Hour), now, true
	case "hourly":
		start = now.UTC().Truncate(time.Hour)
		return start, start.Add(time.Hour), true
	case "daily", "weekly", "monthly":
		interval, good := NormalizeBudgetResetInterval(window)
		if !good {
			return time.Time{}, time.Time{}, false
		}
		w := BudgetWindowFor(interval, "00:00", now)
		return w.PeriodStart, w.NextReset, true
	default:
		return time.Time{}, time.Time{}, false
	}
}

// poolDimensionUsed sums one dimension's consumption for the
// given keys (nil = all keys in the pool) inside [start, end):
// requests counts rows, tokens sums total_tokens, usd sums
// cost_usd. Successful rows only (ok = 1), shared rows excluded.
func (l *Ledger) poolDimensionUsed(ctx context.Context, pool QuotaPool, dim QuotaDimension, keyIDs []string, start, end time.Time) (float64, error) {
	var selectExpr string
	switch dim.Unit {
	case "requests":
		selectExpr = "COUNT(*)"
	case "tokens":
		selectExpr = "COALESCE(SUM(total_tokens), 0)"
	case "usd":
		selectExpr = "COALESCE(SUM(cost_usd), 0)"
	default:
		return 0, fmt.Errorf("ledger: unknown pool dimension unit %q", dim.Unit)
	}
	query := fmt.Sprintf(`SELECT %s FROM usage_events
		WHERE ok = 1 AND shared = 0 AND ts >= ? AND ts < ?`, selectExpr)
	args := []any{start.UnixMilli(), end.UnixMilli()}
	if len(keyIDs) > 0 {
		placeholders := make([]string, len(keyIDs))
		for i, id := range keyIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += ` AND gateway_key_id IN (` + strings.Join(placeholders, ",") + `)`
	}
	var used float64
	if err := l.rdb.QueryRowContext(ctx, query, args...).Scan(&used); err != nil {
		return 0, fmt.Errorf("ledger: pool dimension usage: %w", err)
	}
	return used, nil
}

// poolMemberKeys lists the key IDs holding an allocation in a pool.
func poolMemberKeys(pool QuotaPool) []string {
	keys := make([]string, 0, len(pool.Allocations))
	for _, a := range pool.Allocations {
		keys = append(keys, a.KeyID)
	}
	return keys
}

// CheckPoolFairShare is the pre-request gate for one key in one
// pool (the sector-typical decideFairShare): it returns a block when the
// key must be refused, and penalized=true when a soft-policy key
// is allowed past its fair share. A nil block + penalized=false
// is a clean allow. A ledger error fails closed.
func (l *Ledger) CheckPoolFairShare(ctx context.Context, keyID string, pool QuotaPool, now time.Time) (*FairShareBlock, bool, error) {
	var alloc *PoolAllocation
	for i := range pool.Allocations {
		if pool.Allocations[i].KeyID == keyID {
			alloc = &pool.Allocations[i]
			break
		}
	}
	if alloc == nil {
		return nil, false, nil
	}
	if len(pool.Dimensions) == 0 {
		return nil, false, nil
	}
	threshold := pool.SaturationThreshold
	if threshold == 0 {
		threshold = 0.5
	}
	policy := normalizePoolPolicy(alloc.Policy)
	members := poolMemberKeys(pool)
	penalized := false
	for _, dim := range pool.Dimensions {
		start, end, ok := poolWindow(dim.Window, now)
		if !ok {
			continue
		}
		// The block's reset instant: the window end for
		// calendar windows; for the rolling 5h window usage
		// ages out continuously, so the bound is now + 5h.
		reset := end
		if dim.Window == "5h" {
			reset = now.Add(5 * time.Hour)
		}
		// M5.5: the dimension limit is per-account — the pool's
		// real ceiling is limit × backing accounts.
		effectiveLimit := pool.EffectiveLimit(dim)
		consumed, err := l.poolDimensionUsed(ctx, pool, dim, []string{keyID}, start, end)
		if err != nil {
			return nil, false, err
		}
		total, err := l.poolDimensionUsed(ctx, pool, dim, members, start, end)
		if err != nil {
			return nil, false, err
		}
		fairShare := alloc.Weight / 100 * effectiveLimit

		// Absolute cap: always enforced, either mode.
		if alloc.CapValue != nil && alloc.CapUnit == dim.Unit && consumed >= *alloc.CapValue {
			return &FairShareBlock{PoolID: pool.ID, Reason: "cap-absolute", EffectiveLimit: effectiveLimit, NextReset: reset}, false, nil
		}
		// Global ceiling: no room means no room, any policy.
		if total >= effectiveLimit {
			return &FairShareBlock{PoolID: pool.ID, Reason: "global-saturated", EffectiveLimit: effectiveLimit, NextReset: reset}, false, nil
		}
		var usedPercent float64
		if effectiveLimit > 0 {
			usedPercent = total / effectiveLimit
		}
		if usedPercent >= threshold {
			// Strict mode.
			switch policy {
			case "hard":
				if consumed >= fairShare {
					return &FairShareBlock{PoolID: pool.ID, Reason: "fair-share", EffectiveLimit: effectiveLimit, NextReset: reset}, false, nil
				}
			case "soft":
				if consumed >= fairShare {
					penalized = true
				}
			case "burst":
				// Allow while global headroom exists (checked above).
			}
		} else {
			// Generous mode: borrowing allowed.
			switch policy {
			case "hard":
				if consumed >= effectiveLimit {
					return &FairShareBlock{PoolID: pool.ID, Reason: "global-saturated", EffectiveLimit: effectiveLimit, NextReset: reset}, false, nil
				}
			case "soft":
				if consumed >= fairShare {
					penalized = true
				}
			case "burst":
			}
		}
	}
	return nil, penalized, nil
}

// PoolKeyDimensionUsage reads one dimension's consumption in its
// current window for one key and for the pool as a whole, plus the
// window's reset instant — the numbers me/status reports per
// account quota (M5.5). A ledger error fails the read.
func (l *Ledger) PoolKeyDimensionUsage(ctx context.Context, pool QuotaPool, dim QuotaDimension, keyID string, now time.Time) (usedByKey, usedTotal float64, nextReset time.Time, err error) {
	start, end, ok := poolWindow(dim.Window, now)
	if !ok {
		return 0, 0, time.Time{}, fmt.Errorf("ledger: unknown pool window %q", dim.Window)
	}
	if keyID != "" {
		usedByKey, err = l.poolDimensionUsed(ctx, pool, dim, []string{keyID}, start, end)
		if err != nil {
			return 0, 0, time.Time{}, err
		}
	}
	usedTotal, err = l.poolDimensionUsed(ctx, pool, dim, poolMemberKeys(pool), start, end)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	if dim.Window == "5h" {
		return usedByKey, usedTotal, now.Add(5 * time.Hour), nil
	}
	return usedByKey, usedTotal, end, nil
}

// PoolDimensionStatus reads one dimension's live pool-wide
// consumption in its current window plus the window's reset
// instant, for the dashboard feed. A ledger error fails the read.
func (l *Ledger) PoolDimensionStatus(ctx context.Context, pool QuotaPool, dim QuotaDimension, now time.Time) (used float64, nextReset time.Time, err error) {
	_, used, nextReset, err = l.PoolKeyDimensionUsage(ctx, pool, dim, "", now)
	return used, nextReset, err
}

// SetQuotaPool upserts a pool with its dimensions and allocations
// in one transaction (dimensions/allocations are replaced wholesale
// — the config + dashboard write the full pool, never a delta).
func (l *Ledger) SetQuotaPool(ctx context.Context, pool QuotaPool) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: set quota pool: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO quota_pools (id, name, saturation_threshold, account_count) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name = excluded.name, saturation_threshold = excluded.saturation_threshold, account_count = excluded.account_count`,
		pool.ID, pool.Name, pool.SaturationThreshold, pool.EffectiveAccountCount()); err != nil {
		return fmt.Errorf("ledger: set quota pool: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM quota_pool_dimensions WHERE pool_id = ?`, pool.ID); err != nil {
		return fmt.Errorf("ledger: set quota pool dimensions: %w", err)
	}
	for _, d := range pool.Dimensions {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO quota_pool_dimensions (pool_id, unit, window, limit_value) VALUES (?, ?, ?, ?)`,
			pool.ID, d.Unit, d.Window, d.Limit); err != nil {
			return fmt.Errorf("ledger: set quota pool dimension: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM quota_pool_allocations WHERE pool_id = ?`, pool.ID); err != nil {
		return fmt.Errorf("ledger: set quota pool allocations: %w", err)
	}
	for _, a := range pool.Allocations {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO quota_pool_allocations (pool_id, key_id, weight, cap_value, cap_unit, policy) VALUES (?, ?, ?, ?, ?, ?)`,
			pool.ID, a.KeyID, a.Weight, a.CapValue, a.CapUnit, a.Policy); err != nil {
			return fmt.Errorf("ledger: set quota pool allocation: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: set quota pool: %w", err)
	}
	return nil
}

// GetQuotaPool reads one pool with its dimensions and allocations.
// sql.ErrNoRows when the pool does not exist.
func (l *Ledger) GetQuotaPool(ctx context.Context, id string) (QuotaPool, error) {
	var pool QuotaPool
	err := l.rdb.QueryRowContext(ctx,
		`SELECT id, name, saturation_threshold, account_count FROM quota_pools WHERE id = ?`, id).
		Scan(&pool.ID, &pool.Name, &pool.SaturationThreshold, &pool.AccountCount)
	if err != nil {
		return QuotaPool{}, err
	}
	if err := l.loadPoolChildren(ctx, &pool); err != nil {
		return QuotaPool{}, err
	}
	return pool, nil
}

// ListQuotaPools reads every pool with dimensions and allocations,
// ordered by id.
func (l *Ledger) ListQuotaPools(ctx context.Context) ([]QuotaPool, error) {
	rows, err := l.rdb.QueryContext(ctx,
		`SELECT id, name, saturation_threshold, account_count FROM quota_pools ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("ledger: list quota pools: %w", err)
	}
	defer rows.Close()
	pools := []QuotaPool{}
	for rows.Next() {
		var pool QuotaPool
		if err := rows.Scan(&pool.ID, &pool.Name, &pool.SaturationThreshold, &pool.AccountCount); err != nil {
			return nil, fmt.Errorf("ledger: list quota pools: %w", err)
		}
		pools = append(pools, pool)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list quota pools: %w", err)
	}
	for i := range pools {
		if err := l.loadPoolChildren(ctx, &pools[i]); err != nil {
			return nil, err
		}
	}
	return pools, nil
}

// PoolsForKey lists the pools in which keyID holds an allocation.
func (l *Ledger) PoolsForKey(ctx context.Context, keyID string) ([]QuotaPool, error) {
	rows, err := l.rdb.QueryContext(ctx,
		`SELECT DISTINCT p.id, p.name, p.saturation_threshold, p.account_count FROM quota_pools p
		 JOIN quota_pool_allocations a ON a.pool_id = p.id
		 WHERE a.key_id = ? ORDER BY p.id`, keyID)
	if err != nil {
		return nil, fmt.Errorf("ledger: pools for key: %w", err)
	}
	defer rows.Close()
	pools := []QuotaPool{}
	for rows.Next() {
		var pool QuotaPool
		if err := rows.Scan(&pool.ID, &pool.Name, &pool.SaturationThreshold, &pool.AccountCount); err != nil {
			return nil, fmt.Errorf("ledger: pools for key: %w", err)
		}
		pools = append(pools, pool)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: pools for key: %w", err)
	}
	for i := range pools {
		if err := l.loadPoolChildren(ctx, &pools[i]); err != nil {
			return nil, err
		}
	}
	return pools, nil
}

// DeleteQuotaPool removes a pool and its dimensions/allocations.
func (l *Ledger) DeleteQuotaPool(ctx context.Context, id string) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: delete quota pool: %w", err)
	}
	defer tx.Rollback()
	for _, table := range []string{"quota_pool_dimensions", "quota_pool_allocations", "quota_pools"} {
		col := "pool_id"
		if table == "quota_pools" {
			col = "id"
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s = ?`, table, col), id); err != nil {
			return fmt.Errorf("ledger: delete quota pool: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: delete quota pool: %w", err)
	}
	return nil
}

// loadPoolChildren fills a pool's dimensions and allocations.
func (l *Ledger) loadPoolChildren(ctx context.Context, pool *QuotaPool) error {
	drows, err := l.rdb.QueryContext(ctx,
		`SELECT unit, window, limit_value FROM quota_pool_dimensions WHERE pool_id = ? ORDER BY unit, window`, pool.ID)
	if err != nil {
		return fmt.Errorf("ledger: pool dimensions: %w", err)
	}
	pool.Dimensions = []QuotaDimension{}
	for drows.Next() {
		var d QuotaDimension
		if err := drows.Scan(&d.Unit, &d.Window, &d.Limit); err != nil {
			drows.Close()
			return fmt.Errorf("ledger: pool dimensions: %w", err)
		}
		pool.Dimensions = append(pool.Dimensions, d)
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return fmt.Errorf("ledger: pool dimensions: %w", err)
	}
	arows, err := l.rdb.QueryContext(ctx,
		`SELECT key_id, weight, cap_value, cap_unit, policy FROM quota_pool_allocations WHERE pool_id = ? ORDER BY key_id`, pool.ID)
	if err != nil {
		return fmt.Errorf("ledger: pool allocations: %w", err)
	}
	pool.Allocations = []PoolAllocation{}
	for arows.Next() {
		var a PoolAllocation
		var capValue sql.NullFloat64
		var capUnit sql.NullString
		if err := arows.Scan(&a.KeyID, &a.Weight, &capValue, &capUnit, &a.Policy); err != nil {
			arows.Close()
			return fmt.Errorf("ledger: pool allocations: %w", err)
		}
		if capValue.Valid {
			a.CapValue = &capValue.Float64
		}
		if capUnit.Valid {
			a.CapUnit = capUnit.String
		}
		pool.Allocations = append(pool.Allocations, a)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return fmt.Errorf("ledger: pool allocations: %w", err)
	}
	return nil
}
