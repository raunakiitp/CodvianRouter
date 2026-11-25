// G13 — persistent reasoning cache (SQLite).
//
// PersistentReasoningCache is the DB tier for the M6.6 reasoning
// replay cache (parity tracker Area 6 row "Reasoning cache (2 h)":
// the reference router reads memory first, DB fallback). It follows the
// existing cache persistence pattern exactly
// (internal/cache/persist.go, semantic_persist.go — same driver,
// modernc.org/sqlite, no cgo; same conventions):
//
//   - Write-through: every Set/SetState reaches disk synchronously
//     (upsert under the persist lock), so entries survive a process
//     restart when persistence is enabled. With no file there is no
//     persistence — callers that want memory-only keep using
//     NewReasoningCache directly.
//   - Reload on start: OpenPersistentReasoningCache prunes expired
//     rows first (they are deleted, never reloaded) and loads every
//     unexpired row into memory, restoring each row's REMAINING
//     TTL — a row with 10 minutes left does not come back with a
//     fresh 2 h. Rows whose state JSON is corrupt, or whose stored
//     size no longer fits the caps, are skipped, never served.
//   - Durable flush: Sync rewrites the table from the live
//     in-memory entries in one transaction (rows for entries that
//     expired or were evicted since the last sync are dropped, not
//     resurrected), and Close closes the file after a final sync
//     by the caller. Delete/FlushAll delete the disk rows too, so
//     a flush cannot be undone by the next restart.
//
// Unlike PersistentCache / PersistentSemanticCache this tier is
// NOT a wrapper type: it returns the same *ReasoningCache with a
// persist handle attached (ReasoningCache.persist), because the
// degrade wrapper holds the cache as a concrete *ReasoningCache
// (degrade.StaleEngine.SetReasoningCache) and a wrapper type
// would need that signature — and every call site — to change.
// A memory-only cache has persist == nil and behaves exactly as
// before; the store funnel (setStateForTenantTTL) is the single
// place that branches on it.
//
// Lock order (see ReasoningCache doc): persist.mu -> c.mu, never
// the reverse. Mutating methods take persist.mu first when a
// tier is attached; reads take c.mu only and never touch disk.
// A DB write failure is swallowed by the store path (the
// in-memory entry is still stored, so a transient disk error
// never breaks serving) and returned by Sync/Close, matching
// the exact layer's best-effort-store rule.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/reasoning"

	_ "modernc.org/sqlite"
)

// reasoningPersist is the SQLite handle attached to a ReasoningCache
// by OpenPersistentReasoningCache. mu serialises every disk write
// against Sync/Close/Delete/FlushAll (see the lock-order rule in
// the ReasoningCache doc); closed is set by Close.
type reasoningPersist struct {
	mu     sync.Mutex
	db     *sql.DB
	closed bool
}

const reasoningPersistSchema = `
CREATE TABLE IF NOT EXISTS reasoning_entries(
	key TEXT PRIMARY KEY,
	tenant TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	expires_unix INTEGER NOT NULL
);`

// OpenPersistentReasoningCache opens (creating it if needed) the
// SQLite file at path and loads every unexpired entry into memory.
// Expired rows are deleted, not loaded. A row whose state JSON is
// corrupt, or whose stored size no longer fits the caps, is
// skipped. It returns the same *ReasoningCache serve mode already
// wires (degrade.SetReasoningCache), with the DB tier attached.
func OpenPersistentReasoningCache(path string, cfg ReasoningCacheConfig) (*ReasoningCache, error) {
	if path == "" {
		return nil, errors.New("cache: persistent reasoning cache path is empty")
	}
	// Open with WAL + busy_timeout on every connection (AUD-REL-004).
	db, err := sql.Open("sqlite", cacheSQLiteDSN(path))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(reasoningPersistSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	c := NewReasoningCacheWithConfig(cfg)
	now := time.Now()
	// Prune expired rows first so they can never be reloaded.
	if _, err := db.Exec(`DELETE FROM reasoning_entries WHERE expires_unix <= ?`, now.Unix()); err != nil {
		_ = db.Close()
		return nil, err
	}
	rows, err := db.Query(`SELECT key, tenant, provider, model, state, expires_unix FROM reasoning_entries`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, tenant, provider, model, raw string
		var expiresUnix int64
		if err := rows.Scan(&key, &tenant, &provider, &model, &raw, &expiresUnix); err != nil {
			continue
		}
		var st reasoning.State
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			continue
		}
		if st.Empty() {
			continue
		}
		ttl := time.Until(time.Unix(expiresUnix, 0))
		if ttl <= 0 {
			continue
		}
		// Restore the row's REMAINING ttl, not a fresh 2 h.
		// The persist tier is not attached yet, so this store
		// is memory-only; the cap check inside the funnel
		// still applies, so an oversize row is dropped.
		c.setStateForTenantTTL(key, tenant, provider, model, st, st.ReasoningText(), ttl)
	}
	if err := rows.Err(); err != nil {
		_ = db.Close()
		return nil, err
	}
	c.persist = &reasoningPersist{db: db}
	return c, nil
}

// persistStore upserts one row. Caller holds persist.mu.
func (c *ReasoningCache) persistStore(key, tenant, provider, model string, st reasoning.State, expires time.Time) error {
	p := c.persist
	if p == nil || p.closed {
		return errors.New("cache: persistent reasoning cache is closed")
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(context.Background(),
		`INSERT INTO reasoning_entries(key, tenant, provider, model, state, expires_unix) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(key) DO UPDATE SET tenant=excluded.tenant, provider=excluded.provider, model=excluded.model, state=excluded.state, expires_unix=excluded.expires_unix`,
		key, tenant, provider, model, string(raw), expires.Unix())
	return err
}

// persistDelete deletes one row. Caller holds persist.mu.
func (c *ReasoningCache) persistDelete(key string) {
	p := c.persist
	if p == nil || p.closed {
		return
	}
	_, _ = p.db.ExecContext(context.Background(), `DELETE FROM reasoning_entries WHERE key = ?`, key)
}

// persistDeleteAll deletes every row. Caller holds persist.mu.
func (c *ReasoningCache) persistDeleteAll() {
	p := c.persist
	if p == nil || p.closed {
		return
	}
	_, _ = p.db.ExecContext(context.Background(), `DELETE FROM reasoning_entries`)
}

// Sync rewrites the reasoning_entries table from the live
// in-memory entries in one transaction, on the already-open
// handle: rows for entries that have expired or been evicted
// since the last sync are dropped, not resurrected. It returns
// how many entries were written. A memory-only cache (no tier
// attached) reports 0, nil — there is nothing to sync.
//
// SERIALIZATION: the snapshot read AND the rewrite run under
// persist.mu (lock order persist.mu -> c.mu, see the
// ReasoningCache doc), and the store/delete/flush paths hold
// persist.mu for their whole operation (memory + disk). A flush
// that runs between a snapshot and its commit therefore cannot
// be undone by the commit — flushed entries stay flushed.
func (c *ReasoningCache) Sync(ctx context.Context) (int, error) {
	p := c.persist
	if p == nil {
		return 0, nil
	}
	type snapRow struct {
		key      string
		tenant   string
		provider string
		model    string
		raw      string
		expires  int64
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errors.New("cache: persistent reasoning cache is closed")
	}
	c.mu.Lock()
	now := time.Now()
	rows := make([]snapRow, 0, len(c.items))
	for key, e := range c.items {
		if now.After(e.expires) {
			continue
		}
		raw, err := json.Marshal(e.state)
		if err != nil {
			continue
		}
		rows = append(rows, snapRow{key: key, tenant: e.tenant, provider: e.provider, model: e.model, raw: string(raw), expires: e.expires.Unix()})
	}
	c.mu.Unlock()

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM reasoning_entries`); err != nil {
		return 0, err
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reasoning_entries(key, tenant, provider, model, state, expires_unix) VALUES(?,?,?,?,?,?)`,
			r.key, r.tenant, r.provider, r.model, r.raw, r.expires); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// Close closes the SQLite file. In-memory entries are already on
// disk (every store writes through), so nothing is lost on close.
// A memory-only cache (no tier attached) is a no-op.
func (c *ReasoningCache) Close() error {
	p := c.persist
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.db.Close()
}

// Persistent reports whether the SQLite tier is attached.
func (c *ReasoningCache) Persistent() bool {
	return c != nil && c.persist != nil
}
