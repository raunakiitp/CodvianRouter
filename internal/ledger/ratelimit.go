package ledger

// Learned upstream rate limits (M5.13): the durable side of the
// router's rate-limit learning. Every successful (or 429) response
// teaches the router the upstream's remaining budget per
// (provider, key); the rows persist it across restarts so the
// startup load can pace the adaptive limiter before the first new
// response arrives.
//
// One row per (provider, key, kind) — kind "requests" or "tokens" —
// holding the latest limit/remaining/reset the upstream reported.
// The write is an upsert of the latest signal: the upstream's
// remaining only moves as requests consume it, so the newest header
// is the current truth. A row is fresh while its window has not
// reset and it is younger than the 5-minute TTL (the same TTL
// the sector-typical header cache uses); stale rows are ignored by the
// read path and pruned lazily.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
)

// LearnedRateLimit is one persisted rate-limit signal.
type LearnedRateLimit struct {
	ProviderID string
	KeyID      string
	Kind       string // "requests" | "tokens"
	Limit      float64
	Remaining  float64
	ResetAt    *time.Time
	UpdatedAt  time.Time
}

// LearnRateLimit upserts the latest signals for (provider, key):
// the requests row when the signal carries request headers, the
// tokens row when it carries token headers. A Retry-After-only
// signal (no limit/remaining) persists nothing — there is no budget
// to learn from it.
func (l *Ledger) LearnRateLimit(ctx context.Context, providerID, keyID string, sig adaptive.RateLimitSignals, now time.Time) error {
	if sig.Empty() {
		return nil
	}
	if sig.HasRequests {
		if err := l.upsertLearnedRateLimit(ctx, providerID, keyID, "requests", sig.RequestsLimit, sig.RequestsRemaining, sig.RequestsResetAt, now); err != nil {
			return err
		}
	}
	if sig.HasTokens {
		if err := l.upsertLearnedRateLimit(ctx, providerID, keyID, "tokens", sig.TokensLimit, sig.TokensRemaining, sig.TokensResetAt, now); err != nil {
			return err
		}
	}
	return nil
}

func (l *Ledger) upsertLearnedRateLimit(ctx context.Context, providerID, keyID, kind string, limit, remaining float64, resetAt *time.Time, now time.Time) error {
	var resetMs sql.NullInt64
	if resetAt != nil {
		resetMs = sql.NullInt64{Int64: resetAt.UnixMilli(), Valid: true}
	}
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO learned_rate_limits
		  (provider_id, provider_key_id, kind, limit_value, remaining, reset_at_ms, updated_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, provider_key_id, kind) DO UPDATE SET
		  limit_value = excluded.limit_value, remaining = excluded.remaining,
		  reset_at_ms = excluded.reset_at_ms, updated_ms = excluded.updated_ms`,
		providerID, keyID, kind, limit, remaining, resetMs, now.UnixMilli())
	if err != nil {
		return fmt.Errorf("ledger: learn rate limit: %w", err)
	}
	return nil
}

// LearnedRateLimits returns every fresh learned signal: rows whose
// window has not reset and which are younger than the 5-minute
// TTL. One signal per (provider, key), requests and tokens combined
// from their rows.
func (l *Ledger) LearnedRateLimits(ctx context.Context, now time.Time) ([]LearnedRateLimit, error) {
	rows, err := l.rdb.QueryContext(ctx, `
		SELECT provider_id, provider_key_id, kind, limit_value, remaining, reset_at_ms, updated_ms
		FROM learned_rate_limits
		WHERE (reset_at_ms IS NULL OR reset_at_ms > ?)
		  AND updated_ms > ?
		ORDER BY provider_id, provider_key_id, kind`,
		now.UnixMilli(), now.Add(-adaptive.RateLimitTTL).UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("ledger: learned rate limits: %w", err)
	}
	defer rows.Close()
	var out []LearnedRateLimit
	for rows.Next() {
		var e LearnedRateLimit
		var resetMs, updatedMs sql.NullInt64
		if err := rows.Scan(&e.ProviderID, &e.KeyID, &e.Kind, &e.Limit, &e.Remaining, &resetMs, &updatedMs); err != nil {
			return nil, fmt.Errorf("ledger: learned rate limits: %w", err)
		}
		if resetMs.Valid {
			t := time.UnixMilli(resetMs.Int64).UTC()
			e.ResetAt = &t
		}
		if updatedMs.Valid {
			e.UpdatedAt = time.UnixMilli(updatedMs.Int64).UTC()
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: learned rate limits: %w", err)
	}
	return out, nil
}

// LearnedSignalsForKey rebuilds the combined signal for one
// (provider, key) from its fresh rows, or (zero, false) when
// nothing fresh is stored.
func (l *Ledger) LearnedSignalsForKey(ctx context.Context, providerID, keyID string, now time.Time) (adaptive.RateLimitSignals, bool, error) {
	rows, err := l.LearnedRateLimits(ctx, now)
	if err != nil {
		return adaptive.RateLimitSignals{}, false, err
	}
	var sig adaptive.RateLimitSignals
	found := false
	for _, e := range rows {
		if e.ProviderID != providerID || e.KeyID != keyID {
			continue
		}
		found = true
		if sig.ObservedAt.IsZero() || e.UpdatedAt.After(sig.ObservedAt) {
			sig.ObservedAt = e.UpdatedAt
		}
		switch e.Kind {
		case "requests":
			sig.HasRequests = true
			sig.RequestsLimit = e.Limit
			sig.RequestsRemaining = e.Remaining
			sig.RequestsResetAt = e.ResetAt
		case "tokens":
			sig.HasTokens = true
			sig.TokensLimit = e.Limit
			sig.TokensRemaining = e.Remaining
			sig.TokensResetAt = e.ResetAt
		}
	}
	return sig, found, nil
}

// PruneLearnedRateLimits deletes stale rows (past their reset, or
// older than the TTL). It is best-effort maintenance, not part of
// any request path.
func (l *Ledger) PruneLearnedRateLimits(ctx context.Context, now time.Time) (int64, error) {
	res, err := l.db.ExecContext(ctx, `
		DELETE FROM learned_rate_limits
		WHERE (reset_at_ms IS NOT NULL AND reset_at_ms <= ?)
		   OR updated_ms <= ?`,
		now.UnixMilli(), now.Add(-adaptive.RateLimitTTL).UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("ledger: prune learned rate limits: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ledger: prune learned rate limits: %w", err)
	}
	return n, nil
}
