// Package ledger implements the unified credit/usage ledger for the
// Codvian Router. All usage is recorded locally in SQLite (pure-Go driver
// modernc.org/sqlite, no cgo), so the router can run fully offline.
//
// Schema:
//
//	usage_events(id, ts, gateway_key_id, provider, model,
//	             prompt_tokens, completion_tokens, cost_usd, ok, latency_ms)
//	budgets(key_id, cap_usd, spent_usd, period)
//
// Cost is computed from caller-supplied per-model prices: tokens *
// per-token price. Prices are never invented here — a model with no
// supplied price records cost 0, explicitly, so an unknown price can
// never silently turn into a made-up charge.
package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ModelPrice is the per-token price of one model, in USD.
type ModelPrice struct {
	PromptPerToken     float64 // USD per prompt token
	CompletionPerToken float64 // USD per completion token
}

// Prices maps model name -> per-token prices. A model absent from the
// map has unknown pricing and costs 0 (see Cost).
type Prices map[string]ModelPrice

// Cost returns the USD cost of a call, or 0 when the model's price is
// unknown. Unknown prices are recorded as 0, never guessed.
func (p Prices) Cost(model string, promptTokens, completionTokens int) float64 {
	pr, ok := p[model]
	if !ok {
		return 0
	}
	return float64(promptTokens)*pr.PromptPerToken +
		float64(completionTokens)*pr.CompletionPerToken
}

// Usage is one recorded request against a gateway key.
type Usage struct {
	GatewayKeyID     string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	OK               bool
	LatencyMS        int64
}

// Ledger is the usage/budget store backed by a local SQLite file.
type Ledger struct {
	db   *sql.DB
	path string

	// FailClosed enables "never bill me" mode: once a key's budget is
	// exhausted (spent >= cap, including a cap of 0), CheckBudget
	// refuses the request. When false, an exhausted budget is only a
	// warning signal — requests are still allowed. See budget.go.
	FailClosed bool
}

const schema = `
CREATE TABLE IF NOT EXISTS usage_events (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                INTEGER NOT NULL,  -- unix milliseconds (UTC)
  gateway_key_id    TEXT NOT NULL,
  provider          TEXT NOT NULL,
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd          REAL NOT NULL DEFAULT 0,
  ok                INTEGER NOT NULL DEFAULT 1,
  latency_ms        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_key_ts
  ON usage_events(gateway_key_id, ts);

CREATE TABLE IF NOT EXISTS budgets (
  key_id   TEXT PRIMARY KEY,
  cap_usd  REAL NOT NULL,
  spent_usd REAL NOT NULL DEFAULT 0,
  period   TEXT NOT NULL DEFAULT ''
);
`

// Open opens (creating if needed) the SQLite ledger at path, applies
// the schema, and returns the Ledger. Parent directories are created.
func Open(path string) (*Ledger, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("ledger: create directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("ledger: open sqlite: %w", err)
	}
	// Single writer connection: SQLite (even in WAL mode) serialises
	// writers, and one connection avoids "database is locked" churn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: enable WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: busy timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: apply schema: %w", err)
	}
	return &Ledger{db: db, path: path}, nil
}

// Close closes the underlying database.
func (l *Ledger) Close() error {
	if l.db == nil {
		return nil
	}
	return l.db.Close()
}

// Path returns the SQLite file path backing this ledger.
func (l *Ledger) Path() string { return l.path }

// RecordUsage records one request and returns its computed USD cost.
// The cost comes from prices; an unknown model costs 0 and is recorded
// as 0. When a budget row exists for the key, budgets.spent_usd is
// incremented in the same transaction, so spend and budget stay in sync.
func (l *Ledger) RecordUsage(ctx context.Context, u Usage, prices Prices) (float64, error) {
	cost := prices.Cost(u.Model, u.PromptTokens, u.CompletionTokens)
	ok := 0
	if u.OK {
		ok = 1
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("ledger: begin tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_events
		  (ts, gateway_key_id, provider, model, prompt_tokens,
		   completion_tokens, cost_usd, ok, latency_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), u.GatewayKeyID, u.Provider, u.Model,
		u.PromptTokens, u.CompletionTokens, cost, ok, u.LatencyMS); err != nil {
		return 0, fmt.Errorf("ledger: insert usage event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE budgets SET spent_usd = spent_usd + ? WHERE key_id = ?`,
		cost, u.GatewayKeyID); err != nil {
		return 0, fmt.Errorf("ledger: update budget spend: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("ledger: commit: %w", err)
	}
	return cost, nil
}

// SpendFor returns the total USD recorded for keyID since the given
// time (inclusive). It sums usage_events, the source of truth;
// budgets.spent_usd is a cached running total kept in sync by
// RecordUsage.
func (l *Ledger) SpendFor(ctx context.Context, keyID string, since time.Time) (float64, error) {
	var total sql.NullFloat64
	err := l.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_usd), 0) FROM usage_events
		WHERE gateway_key_id = ? AND ts >= ?`,
		keyID, since.UnixMilli()).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("ledger: spend for key: %w", err)
	}
	return total.Float64, nil
}

// Budget is one key's cap and running spend.
type Budget struct {
	KeyID    string
	CapUSD   float64
	SpentUSD float64
	Period   string
}

// SetBudget creates or updates the budget cap for a key. An existing
// spent_usd total is preserved when the cap changes; period is an
// opaque label (e.g. "monthly", "2026-11") used by callers to decide
// when to reset.
func (l *Ledger) SetBudget(ctx context.Context, keyID string, capUSD float64, period string) error {
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO budgets (key_id, cap_usd, spent_usd, period)
		VALUES (?, ?, 0, ?)
		ON CONFLICT(key_id) DO UPDATE SET cap_usd = excluded.cap_usd,
		                                   period = excluded.period`,
		keyID, capUSD, period)
	if err != nil {
		return fmt.Errorf("ledger: set budget: %w", err)
	}
	return nil
}

// GetBudget returns the budget row for a key, or sql.ErrNoRows when
// the key has no budget (unlimited).
func (l *Ledger) GetBudget(ctx context.Context, keyID string) (Budget, error) {
	var b Budget
	err := l.db.QueryRowContext(ctx, `
		SELECT key_id, cap_usd, spent_usd, period FROM budgets
		WHERE key_id = ?`, keyID).Scan(&b.KeyID, &b.CapUSD, &b.SpentUSD, &b.Period)
	if err != nil {
		return Budget{}, err
	}
	return b, nil
}

// ResetSpent zeroes the running spend for a key (e.g. at a period
// rollover). It does not touch usage_events history.
func (l *Ledger) ResetSpent(ctx context.Context, keyID string) error {
	_, err := l.db.ExecContext(ctx, `
		UPDATE budgets SET spent_usd = 0 WHERE key_id = ?`, keyID)
	if err != nil {
		return fmt.Errorf("ledger: reset spend: %w", err)
	}
	return nil
}

// CheckBudget reports whether keyID may spend more, and how much
// budget remains (cap - spent).
//
//   - No budget row: unlimited. allowed=true, remaining=+Inf.
//   - spent < cap: allowed=true, remaining = cap - spent.
//   - spent >= cap: allowed depends on FailClosed. With FailClosed on
//     ("never bill me"), allowed=false — including a cap of 0, where
//     any spend would exceed the cap. With FailClosed off, the request
//     is still allowed (soft cap) but remaining is <= 0, signalling
//     the caller to warn. Use CheckBudgetOrError (budget.go) for the
//     typed refusal plus the 80% soft warning.
func (l *Ledger) CheckBudget(ctx context.Context, keyID string) (allowed bool, remaining float64, err error) {
	b, err := l.GetBudget(ctx, keyID)
	if err != nil {
		if err == sql.ErrNoRows {
			return true, math.Inf(1), nil
		}
		return false, 0, fmt.Errorf("ledger: check budget: %w", err)
	}
	remaining = b.CapUSD - b.SpentUSD
	if b.SpentUSD >= b.CapUSD {
		if l.FailClosed {
			return false, remaining, nil
		}
		return true, remaining, nil
	}
	return true, remaining, nil
}
