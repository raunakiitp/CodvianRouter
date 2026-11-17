// Package ledger implements the unified credit/usage ledger for the
// Codvian Router. All usage is recorded locally in SQLite (pure-Go driver
// modernc.org/sqlite, no cgo), so the router can run fully offline.
//
// Schema:
//
//	usage_events(id, ts, gateway_key_id, provider, model,
//	             prompt_tokens, completion_tokens, total_tokens,
//	             tokens_split_known, cost_usd, ok, latency_ms,
//	             shared)
//	budgets(key_id, cap_usd, spent_usd, period)
//
// shared marks a request served from a coalesced (single-flight)
// upstream call: 1 when the row is a waiter that shared the leader's
// call, 0 otherwise. A shared row always carries zero tokens and
// zero cost — the leader's row is the only one billed — so summing
// cost_usd or tokens over usage_events never double-counts, and
// filtering shared = 0 gives exactly the real upstream attempts.
//
// Cost is computed from caller-supplied per-model prices: tokens *
// per-token price. Prices are never invented here — a model with no
// supplied price records cost 0, explicitly, so an unknown price can
// never silently turn into a made-up charge. Likewise a usage split
// is never invented: when the caller does not know the
// prompt/completion split (Usage.SplitKnown false), the event records
// prompt_tokens = completion_tokens = 0, the reported total in
// total_tokens, tokens_split_known = 0, and cost 0 — a split is needed
// for honest pricing, so no cost is guessed from a bare total.
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
//
// SplitKnown reports whether PromptTokens / CompletionTokens are the
// real split reported by the upstream. When false, the split is
// unknown: PromptTokens and CompletionTokens must be ignored (they
// are stored as 0) and only TotalTokens is meaningful — and the cost
// is 0, because a split is needed for honest pricing. Callers must
// never set SplitKnown true with a guessed split.
//
// Shared reports whether the request was served from a coalesced
// (single-flight) upstream call instead of making its own: the
// leader's row carries the real tokens and cost, and a shared row
// always records zero tokens and zero cost, so it can never inflate
// spend or token totals. It exists so a served-but-not-billed
// request is still visible in the ledger, distinguishable from a
// real upstream attempt.
type Usage struct {
	GatewayKeyID     string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	SplitKnown       bool
	OK               bool
	LatencyMS        int64
	Shared           bool
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
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  tokens_split_known INTEGER NOT NULL DEFAULT 1,
  cost_usd          REAL NOT NULL DEFAULT 0,
  ok                INTEGER NOT NULL DEFAULT 1,
  latency_ms        INTEGER NOT NULL DEFAULT 0,
  shared            INTEGER NOT NULL DEFAULT 0
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
	// Migrate databases created before total_tokens /
	// tokens_split_known existed: fresh databases already have the
	// columns from the schema above, so this is a no-op for them.
	if err := ensureUsageColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Ledger{db: db, path: path}, nil
}

// ensureUsageColumns adds total_tokens and tokens_split_known to
// usage_events when they are missing (databases created by an older
// build). It is idempotent: each column is added only after a PRAGMA
// table_info check shows it absent, so opening an already-migrated
// database changes nothing.
func ensureUsageColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(usage_events)`)
	if err != nil {
		return fmt.Errorf("ledger: inspect usage_events schema: %w", err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("ledger: inspect usage_events schema: %w", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ledger: inspect usage_events schema: %w", err)
	}
	rows.Close()
	add := map[string]string{
		"total_tokens":       `ALTER TABLE usage_events ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`,
		"tokens_split_known": `ALTER TABLE usage_events ADD COLUMN tokens_split_known INTEGER NOT NULL DEFAULT 1`,
		"shared":             `ALTER TABLE usage_events ADD COLUMN shared INTEGER NOT NULL DEFAULT 0`,
	}
	for col, stmt := range add {
		if present[col] {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("ledger: add column %s: %w", col, err)
		}
	}
	return nil
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
//
// Split handling (honest accounting, never a guessed split):
//   - SplitKnown true: prompt/completion are stored as given;
//     TotalTokens is stored as given, or prompt+completion when it is 0;
//     cost comes from the split prices.
//   - SplitKnown false: prompt and completion are stored as 0, the
//     reported total is stored in total_tokens, tokens_split_known is
//     0, and cost is 0 — a split is needed for honest pricing, so no
//     cost is guessed from a bare total.
//   - Shared true: the row is a waiter served from a coalesced
//     upstream call — tokens, split and cost are all forced to 0
//     (the leader's row carries the real usage), so a shared row can
//     never inflate spend or token totals.
func (l *Ledger) RecordUsage(ctx context.Context, u Usage, prices Prices) (float64, error) {
	var promptTokens, completionTokens, totalTokens, splitKnown int
	var cost float64
	if u.SplitKnown {
		promptTokens = u.PromptTokens
		completionTokens = u.CompletionTokens
		totalTokens = u.TotalTokens
		if totalTokens == 0 {
			totalTokens = promptTokens + completionTokens
		}
		splitKnown = 1
		cost = prices.Cost(u.Model, promptTokens, completionTokens)
	} else {
		totalTokens = u.TotalTokens
		splitKnown = 0
		cost = 0
	}
	ok := 0
	if u.OK {
		ok = 1
	}
	shared := 0
	if u.Shared {
		shared = 1
		// A shared row is served, never billed: zero tokens and
		// zero cost, whatever the caller passed. The leader's row
		// carries the real usage for the one upstream call.
		promptTokens, completionTokens, totalTokens, splitKnown, cost = 0, 0, 0, 0, 0
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("ledger: begin tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_events
		  (ts, gateway_key_id, provider, model, prompt_tokens,
		   completion_tokens, total_tokens, tokens_split_known,
		   cost_usd, ok, latency_ms, shared)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), u.GatewayKeyID, u.Provider, u.Model,
		promptTokens, completionTokens, totalTokens, splitKnown,
		cost, ok, u.LatencyMS, shared); err != nil {
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

// RecordShared records one request served from a coalesced
// (single-flight) upstream call: a row marked shared with zero tokens
// and zero cost (see Usage.Shared). ok is whether the shared result
// was a success; latencyMs is how long the waiter waited for the
// leader's flight. It never moves a budget (cost is always 0).
func (l *Ledger) RecordShared(ctx context.Context, provider, model string, ok bool, latencyMs int64) (float64, error) {
	return l.RecordUsage(ctx, Usage{
		Provider:  provider,
		Model:     model,
		OK:        ok,
		LatencyMS: latencyMs,
		Shared:    true,
	}, nil)
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
