// M1.5 — persistent exact cache (SQLite) + savings accounting.
//
// PersistentCache wraps the in-memory ExactCache with a SQLite file
// (modernc.org/sqlite, no cgo — the same driver the ledger uses).
// Every Set writes through to disk synchronously, so entries survive
// a process restart when persistence is enabled; with no file there
// is no persistence (callers that want memory-only keep using
// ExactCache directly). Expired rows are pruned on open and never
// reloaded.
//
// Per-key flush is by gateway-key id (tenant): each stored row
// records the tenant it was stored under, so FlushTenant removes
// exactly that tenant's entries and nothing else.
//
// Savings accounting: RecordHit is called by the serving path when
// an own-cache hit is served. It records the tokens that did NOT
// have to be bought again (the cached response's prompt +
// completion tokens) and the dollars saved at caller-supplied
// per-token prices. Prices come from the caller — this package
// never invents a price; with zero prices the dollar saving is $0
// and the token saving is still recorded. Rows live in the same
// SQLite file, so totals survive a restart.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// SavingsRecord is one own-cache hit's saving.
type SavingsRecord struct {
	Tenant           string    `json:"tenant"`
	Model            string    `json:"model"`
	Key              string    `json:"cache_key"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TokensSaved      int       `json:"tokens_saved"`
	CostSavedUSD     float64   `json:"cost_saved_usd"`
	At               time.Time `json:"at"`
}

// SavingsTotals aggregates all recorded own-cache hits.
type SavingsTotals struct {
	Hits         int     `json:"hits"`
	TokensSaved  int     `json:"tokens_saved"`
	CostSavedUSD float64 `json:"cost_saved_usd"`
}

// PersistentCache is an ExactCache backed by a SQLite file.
type PersistentCache struct {
	mu      sync.Mutex
	db      *sql.DB
	exact   *ExactCache
	tenants map[string]map[string]struct{} // tenant -> keys stored under it
	closed  bool
	totals  SavingsTotals
}

const persistentSchema = `
CREATE TABLE IF NOT EXISTS cache_entries(
	key TEXT PRIMARY KEY,
	tenant TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	response TEXT NOT NULL,
	expires_unix INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS cache_savings(
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	tenant TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	cache_key TEXT NOT NULL DEFAULT '',
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	tokens_saved INTEGER NOT NULL DEFAULT 0,
	cost_saved_usd REAL NOT NULL DEFAULT 0,
	created_unix INTEGER NOT NULL
);`

// OpenPersistentCache opens (creating it if needed) the SQLite file
// at path and loads every unexpired entry into memory. Expired rows
// are deleted, not loaded. maxEntries <= 0 selects DefaultMaxEntries.
func OpenPersistentCache(path string, maxEntries int) (*PersistentCache, error) {
	if path == "" {
		return nil, errors.New("cache: persistent cache path is empty")
	}
	// Open with WAL + busy_timeout on every connection (AUD-REL-004):
	// a concurrent write waits for the lock instead of failing with
	// SQLITE_BUSY.
	db, err := sql.Open("sqlite", cacheSQLiteDSN(path))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(persistentSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	p := &PersistentCache{
		db:      db,
		exact:   NewExactCache(maxEntries),
		tenants: make(map[string]map[string]struct{}),
	}
	now := time.Now()
	// Prune expired rows first so they can never be reloaded.
	if _, err := db.Exec(`DELETE FROM cache_entries WHERE expires_unix <= ?`, now.Unix()); err != nil {
		_ = db.Close()
		return nil, err
	}
	rows, err := db.Query(`SELECT key, tenant, model, response, expires_unix FROM cache_entries`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, tenant, model, raw string
		var expiresUnix int64
		if err := rows.Scan(&key, &tenant, &model, &raw, &expiresUnix); err != nil {
			continue
		}
		var resp CachedResponse
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			continue
		}
		ttl := time.Until(time.Unix(expiresUnix, 0))
		if ttl <= 0 {
			continue
		}
		p.exact.Set(context.Background(), key, resp, ttl)
		if p.tenants[tenant] == nil {
			p.tenants[tenant] = make(map[string]struct{})
		}
		p.tenants[tenant][key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Reload savings totals so a restart does not zero the accounting.
	row := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(tokens_saved),0), COALESCE(SUM(cost_saved_usd),0) FROM cache_savings`)
	if err := row.Scan(&p.totals.Hits, &p.totals.TokensSaved, &p.totals.CostSavedUSD); err != nil {
		_ = db.Close()
		return nil, err
	}
	return p, nil
}

// Get returns the cached response on a hit, exactly like ExactCache.Get.
func (p *PersistentCache) Get(ctx context.Context, key string) (CachedResponse, bool) {
	return p.exact.Get(ctx, key)
}

// Set stores resp under key for ttl with no tenant (tenant "").
// Prefer SetForTenant so per-key flush can find the entry.
func (p *PersistentCache) Set(ctx context.Context, key string, resp CachedResponse, ttl time.Duration) {
	_ = p.SetForTenant(ctx, key, "", resp, ttl)
}

// SetForTenant stores resp under key for ttl, recording tenant (the
// gateway key id) so FlushTenant can remove it later. ttl <= 0
// stores nothing, matching ExactCache. A DB write failure is
// returned; the in-memory entry is still stored so a transient disk
// error never breaks serving.
func (p *PersistentCache) SetForTenant(ctx context.Context, key, tenant string, resp CachedResponse, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	p.exact.Set(ctx, key, resp, ttl)
	raw, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("cache: persistent cache is closed")
	}
	expires := time.Now().Add(ttl).Unix()
	_, err = p.db.ExecContext(ctx,
		`INSERT INTO cache_entries(key, tenant, model, response, expires_unix) VALUES(?,?,?,?,?)
		 ON CONFLICT(key) DO UPDATE SET tenant=excluded.tenant, model=excluded.model, response=excluded.response, expires_unix=excluded.expires_unix`,
		key, tenant, resp.Model, string(raw), expires)
	if err != nil {
		return err
	}
	if p.tenants[tenant] == nil {
		p.tenants[tenant] = make(map[string]struct{})
	}
	p.tenants[tenant][key] = struct{}{}
	return nil
}

// Size returns the number of live entries in memory.
func (p *PersistentCache) Size() int {
	return p.exact.Size()
}

// Exact returns the in-memory cache this wrapper persists.
// It stays for callers that need the bare cache (tests, the
// memory-only fallback); serve mode no longer needs it —
// degrade.WrapWithSemantic and server.WithCaches take the
// ExactStore interface, so the *PersistentCache itself is
// passed and every Set writes through to disk (M6.2b).
// Entries stored through the returned cache carry no recorded
// tenant (tenant ""), so only FlushAll — not FlushTenant —
// removes them; use SetForTenant for tenant-attributed writes.
func (p *PersistentCache) Exact() *ExactCache {
	return p.exact
}

// ExactStore is the exact-match cache contract shared by the
// in-memory ExactCache and the persistent PersistentCache
// (M6.2b write-through): the degrade wrapper and the relay read
// and write through this interface, so serve mode can pass the
// persistent wrapper and every Set reaches disk synchronously
// instead of waiting for the next snapshot tick.
//
// FlushAll is deliberately NOT in the interface: ExactCache has
// FlushAll() int while PersistentCache has FlushAll(ctx) int
// (cache.FlushStore). Callers that must flush use the
// flushExactStore helper shape — a type switch on the two
// forms — instead of forcing either type to change a signature
// other code depends on.
type ExactStore interface {
	Get(ctx context.Context, key string) (CachedResponse, bool)
	Set(ctx context.Context, key string, resp CachedResponse, ttl time.Duration)
	Delete(ctx context.Context, key string)
	Stats() (entries, bytes int)
}

// Delete removes key from memory and from disk, and drops it from
// the tenant index, so a per-key delete (StaleEngine.FlushTenant)
// is durable immediately: a restart cannot reload the entry.
// A missing key is a no-op.
func (p *PersistentCache) Delete(ctx context.Context, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exact.Delete(ctx, key)
	for tenant, keys := range p.tenants {
		delete(keys, key)
		if len(keys) == 0 {
			delete(p.tenants, tenant)
		}
	}
	if p.closed {
		return
	}
	_, _ = p.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE key = ?`, key)
}

// Stats returns the live in-memory entries + bytes, exactly like
// ExactCache.Stats (the server's cache-stats endpoint reads the
// layers through ExactStore).
func (p *PersistentCache) Stats() (entries, bytes int) {
	return p.exact.Stats()
}

// Sync snapshots every live in-memory entry to disk in one
// transaction, rewriting the cache_entries table: rows for
// entries that have expired or been evicted since the last sync
// are dropped, not resurrected. Entries stored via SetForTenant
// keep their recorded tenant; entries stored directly through
// Exact() sync with tenant "". It returns how many entries were
// written. Savings rows are untouched.
//
// SERIALIZATION (M6.2b): the snapshot read AND the rewrite run
// under p.mu, and FlushAll/FlushTenant/Delete hold p.mu for
// their whole operation (memory + disk + index). Before this,
// Sync read the snapshot outside p.mu, so a flush that ran
// between the snapshot and the commit was undone by the commit
// — flushed entries resurrected on the next restart. With one
// lock covering both sides, either the flush happens first
// (the snapshot never sees the flushed entries) or the sync
// commits first (the flush then deletes the rewritten rows).
// Lock order is always p.mu -> exact.mu: no path holds exact.mu
// while acquiring p.mu (SetForTenant releases exact.mu inside
// ExactCache.Set before taking p.mu), so this cannot deadlock.
func (p *PersistentCache) Sync(ctx context.Context) (int, error) {
	type snapRow struct {
		key     string
		tenant  string
		model   string
		raw     string
		expires int64
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errors.New("cache: persistent cache is closed")
	}
	p.exact.mu.Lock()
	now := time.Now()
	rows := make([]snapRow, 0, len(p.exact.items))
	for key, e := range p.exact.items {
		if now.After(e.expires) {
			continue
		}
		raw, err := json.Marshal(e.resp)
		if err != nil {
			continue
		}
		rows = append(rows, snapRow{key: key, model: e.resp.Model, raw: string(raw), expires: e.expires.Unix()})
	}
	p.exact.mu.Unlock()

	// Reverse the tenant index: key -> tenant, for rows stored
	// via SetForTenant. Keys stored directly through Exact()
	// have no recorded tenant and sync as "".
	keyTenant := make(map[string]string, len(rows))
	for tenant, keys := range p.tenants {
		for key := range keys {
			keyTenant[key] = tenant
		}
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM cache_entries`); err != nil {
		return 0, err
	}
	for _, r := range rows {
		r.tenant = keyTenant[r.key]
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cache_entries(key, tenant, model, response, expires_unix) VALUES(?,?,?,?,?)`,
			r.key, r.tenant, r.model, r.raw, r.expires); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// FlushAll removes every entry (memory + disk) and returns how many
// live entries were removed. The whole operation holds p.mu (see
// Sync): a concurrent snapshot cannot resurrect entries this
// flush just removed. Memory is flushed even when closed; only
// the disk delete is skipped then.
func (p *PersistentCache) FlushAll(ctx context.Context) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.exact.FlushAll()
	if p.closed {
		return n
	}
	_, _ = p.db.ExecContext(ctx, `DELETE FROM cache_entries`)
	p.tenants = make(map[string]map[string]struct{})
	return n
}

// FlushTenant removes every entry stored under tenant (a gateway key
// id) and returns how many were removed. Other tenants are untouched.
// The whole operation holds p.mu (see Sync): a concurrent snapshot
// cannot resurrect entries this flush just removed. Memory is
// flushed even when closed; only the disk delete is skipped then.
func (p *PersistentCache) FlushTenant(ctx context.Context, tenant string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys, ok := p.tenants[tenant]
	if !ok || len(keys) == 0 {
		// No in-memory record: still delete any disk rows for the
		// tenant (e.g. entries evicted from memory but not yet
		// pruned) and report what the DB removed.
		if p.closed {
			return 0
		}
		res, err := p.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE tenant = ?`, tenant)
		if err != nil {
			return 0
		}
		affected, _ := res.RowsAffected()
		return int(affected)
	}
	n := 0
	for key := range keys {
		p.exact.Delete(ctx, key)
		n++
	}
	if !p.closed {
		_, _ = p.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE tenant = ?`, tenant)
	}
	delete(p.tenants, tenant)
	return n
}

// RecordHit records the saving from serving one own-cache hit: the
// cached response's tokens are tokens the router did not have to
// buy again. promptPricePerToken / completionPricePerToken are the
// caller-supplied per-token USD prices for the model (0 when
// unknown — the dollar saving is then $0 and only tokens are
// recorded). The row is written to SQLite immediately. The returned
// record is also reflected in SavingsTotals.
func (p *PersistentCache) RecordHit(ctx context.Context, tenant, key string, resp CachedResponse, promptPricePerToken, completionPricePerToken float64) (SavingsRecord, error) {
	rec := SavingsRecord{
		Tenant:           tenant,
		Model:            resp.Model,
		Key:              key,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TokensSaved:      resp.Usage.PromptTokens + resp.Usage.CompletionTokens,
		CostSavedUSD:     float64(resp.Usage.PromptTokens)*promptPricePerToken + float64(resp.Usage.CompletionTokens)*completionPricePerToken,
		At:               time.Now(),
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return SavingsRecord{}, errors.New("cache: persistent cache is closed")
	}
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO cache_savings(tenant, model, cache_key, prompt_tokens, completion_tokens, tokens_saved, cost_saved_usd, created_unix) VALUES(?,?,?,?,?,?,?,?)`,
		rec.Tenant, rec.Model, rec.Key, rec.PromptTokens, rec.CompletionTokens, rec.TokensSaved, rec.CostSavedUSD, rec.At.Unix())
	if err != nil {
		return SavingsRecord{}, err
	}
	p.totals.Hits++
	p.totals.TokensSaved += rec.TokensSaved
	p.totals.CostSavedUSD += rec.CostSavedUSD
	return rec, nil
}

// SavingsTotals returns the aggregated savings of every recorded
// own-cache hit, including hits recorded before a restart.
func (p *PersistentCache) SavingsTotals() SavingsTotals {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.totals
}

// SavingsRowCount returns how many savings rows are stored in SQLite.
func (p *PersistentCache) SavingsRowCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.totals.Hits
	}
	var n int
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM cache_savings`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// Close closes the SQLite file. In-memory entries are already on
// disk (Set writes through), so nothing is lost on close.
func (p *PersistentCache) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.db.Close()
}
