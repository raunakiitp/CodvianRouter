// M6.2 — persistent semantic cache (SQLite).
//
// PersistentSemanticCache wraps the in-memory LocalSemanticCache
// with a SQLite file (modernc.org/sqlite, no cgo — the same driver
// the exact layer's PersistentCache and the ledger use). Every Set
// writes through to disk synchronously, so entries survive a
// process restart when persistence is enabled; with no file there
// is no persistence (callers that want memory-only keep using
// LocalSemanticCache directly). Expired rows are pruned on open
// and never reloaded.
//
// One row per (model, tenant, temperature bucket, vector): the
// vector is stored as a little-endian float32 blob alongside the
// entry JSON and its expiry, keyed by the SHA-256 of the vector
// bytes so the same text stored twice upserts instead of
// duplicating. Scope is the model the layer already records per
// entry (CHECKLIST 187) PLUS, since M6.11, the tenant (gateway
// key id, "" = anonymous) and the temperature bucket
// (cache.TemperatureBucket, same 2-decimal bucketing as the exact
// layer) — so a restart never drops the tenant/temperature
// scoping: a reloaded scoped entry serves only its own tenant at
// its own temperature. Rows written before M6.11 carry
// temp_bucket ” (the migration in openSemanticDB copies them
// that way); they reload as LEGACY (unscoped) entries, which a
// scoped lookup never serves — they fail closed, never leak.
// There is still no per-tenant flush here (see
// semantic_flush.go): FlushModel is the layer's flush scope.
//
// Serve-mode note (M6.2b): degrade.WrapWithSemantic and
// server.WithCaches take the SemanticStore interface, so serve
// mode passes this wrapper via AsStore() and every Set writes
// through to disk synchronously. The load/snapshot helpers
// below stay for the memory-only path and for tests: a plain
// LocalSemanticCache still loads from disk at startup and
// snapshots every minute when no persistent wrapper is used.
package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const semanticPersistSchema = `
CREATE TABLE IF NOT EXISTS semantic_entries(
	model TEXT NOT NULL,
	tenant TEXT NOT NULL DEFAULT '',
	temp_bucket TEXT NOT NULL DEFAULT '',
	vec_key TEXT NOT NULL,
	vector BLOB NOT NULL,
	response TEXT NOT NULL,
	expires_unix INTEGER NOT NULL,
	PRIMARY KEY(model, tenant, temp_bucket, vec_key)
);`

// semanticPersistSchemaNew is the M6.11 table definition used by
// the migration rebuild (migrateSemanticSchema): it must stay
// identical to semanticPersistSchema above — the two spellings
// exist because CREATE TABLE IF NOT EXISTS cannot rewrite an
// existing table's primary key, so an old-schema table is
// rebuilt under a temporary name instead.
const semanticPersistSchemaNew = `
CREATE TABLE IF NOT EXISTS semantic_entries_new(
	model TEXT NOT NULL,
	tenant TEXT NOT NULL DEFAULT '',
	temp_bucket TEXT NOT NULL DEFAULT '',
	vec_key TEXT NOT NULL,
	vector BLOB NOT NULL,
	response TEXT NOT NULL,
	expires_unix INTEGER NOT NULL,
	PRIMARY KEY(model, tenant, temp_bucket, vec_key)
);`

// PersistentSemanticCache is a LocalSemanticCache backed by a SQLite file.
type PersistentSemanticCache struct {
	mu     sync.Mutex
	db     *sql.DB
	inner  *LocalSemanticCache
	closed bool
}

// semanticVecKey is the stable row key for a vector: the hex
// SHA-256 of its little-endian float32 bytes. Two identical
// vectors key identically; distinct vectors collide only if
// SHA-256 does.
func semanticVecKey(vec []float32) string {
	sum := sha256.Sum256(encodeSemanticVector(vec))
	return hex.EncodeToString(sum[:])
}

// encodeSemanticVector renders vec as little-endian float32 bytes.
func encodeSemanticVector(vec []float32) []byte {
	b := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

// decodeSemanticVector parses little-endian float32 bytes back
// into a vector. It reports false on a malformed blob instead of
// returning a half-decoded vector.
func decodeSemanticVector(b []byte) ([]float32, bool) {
	if len(b)%4 != 0 {
		return nil, false
	}
	vec := make([]float32, len(b)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return vec, true
}

// openSemanticDB opens (creating it if needed) the SQLite file at
// path with the semantic schema applied. A file whose table was
// created by the pre-M6.11 schema (no tenant/temp_bucket columns,
// PRIMARY KEY (model, vec_key)) is migrated in place by
// migrateSemanticSchema before the handle is returned.
func openSemanticDB(path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("cache: persistent semantic cache path is empty")
	}
	// Open with WAL + busy_timeout on every connection (AUD-REL-004).
	db, err := sql.Open("sqlite", cacheSQLiteDSN(path))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(semanticPersistSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateSemanticSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// migrateSemanticSchema rebuilds a semantic_entries table created
// by the pre-M6.11 schema into the M6.11 schema (tenant +
// temp_bucket columns, PRIMARY KEY (model, tenant, temp_bucket,
// vec_key)). Existing rows are copied as LEGACY entries (tenant
// ”, temp_bucket ”): they recorded no tenant or temperature, so
// after the migration they reload unscoped and a scoped lookup
// never serves them — fail closed, never leak across tenants or
// temperatures. A table already on the new schema (temp_bucket
// column present) is untouched. The rebuild runs in one
// transaction: a failure leaves the old table in place.
func migrateSemanticSchema(db *sql.DB) error {
	var hasTempBucket int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('semantic_entries') WHERE name='temp_bucket'`).Scan(&hasTempBucket); err != nil {
		return err
	}
	if hasTempBucket > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(semanticPersistSchemaNew); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO semantic_entries_new(model, tenant, temp_bucket, vec_key, vector, response, expires_unix)
		SELECT model, '', '', vec_key, vector, response, expires_unix FROM semantic_entries`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE semantic_entries`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE semantic_entries_new RENAME TO semantic_entries`); err != nil {
		return err
	}
	return tx.Commit()
}

// loadSemanticRows prunes expired rows from db and loads every
// unexpired row into c via c.set / c.setScoped (which re-apply
// the M6.1 byte caps and the model scope, so a row that no longer
// fits is dropped, never served). A row with temp_bucket ” is a
// pre-M6.11 LEGACY row: it loads unscoped, so a scoped lookup
// never serves it — fail closed. A row with a recorded bucket
// loads scoped under its recorded tenant and bucket. It returns
// how many rows were loaded.
func loadSemanticRows(db *sql.DB, c *LocalSemanticCache) (int, error) {
	now := time.Now()
	if _, err := db.Exec(`DELETE FROM semantic_entries WHERE expires_unix <= ?`, now.Unix()); err != nil {
		return 0, err
	}
	rows, err := db.Query(`SELECT model, tenant, temp_bucket, vector, response, expires_unix FROM semantic_entries`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	loaded := 0
	for rows.Next() {
		var model, tenant, tempBucket string
		var vecBlob []byte
		var raw string
		var expiresUnix int64
		if err := rows.Scan(&model, &tenant, &tempBucket, &vecBlob, &raw, &expiresUnix); err != nil {
			continue
		}
		vec, ok := decodeSemanticVector(vecBlob)
		if !ok {
			continue
		}
		var entry SemanticEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			continue
		}
		ttl := time.Until(time.Unix(expiresUnix, 0))
		if ttl <= 0 {
			continue
		}
		if tempBucket == "" {
			// Legacy row (pre-M6.11): no tenant/temperature
			// recorded — load unscoped, never served by a
			// scoped lookup.
			c.set(vec, model, entry, ttl)
		} else {
			c.setScoped(vec, model, semanticScope{tenant: tenant, tempBucket: tempBucket, scoped: true}, entry, ttl)
		}
		loaded++
	}
	if err := rows.Err(); err != nil {
		return loaded, err
	}
	return loaded, nil
}

// OpenPersistentSemanticCache opens (creating it if needed) the
// SQLite file at path and loads every unexpired entry into memory.
// Expired rows are deleted, not loaded.
func OpenPersistentSemanticCache(path string, cfg LocalSemanticCacheConfig) (*PersistentSemanticCache, error) {
	db, err := openSemanticDB(path)
	if err != nil {
		return nil, err
	}
	p := &PersistentSemanticCache{db: db, inner: NewLocalSemanticCacheWithConfig(cfg)}
	if _, err := loadSemanticRows(db, p.inner); err != nil {
		_ = db.Close()
		return nil, err
	}
	return p, nil
}

// Inner returns the in-memory cache this wrapper persists. The
// caller must not store through it directly — writes bypassing
// the wrapper never reach disk.
func (p *PersistentSemanticCache) Inner() *LocalSemanticCache {
	return p.inner
}

// Get returns the stored response most similar to embedding (raw
// model-less lookup, exactly like LocalSemanticCache.Get).
func (p *PersistentSemanticCache) Get(ctx context.Context, embedding []float32) (SemanticEntry, bool) {
	return p.inner.Get(ctx, embedding)
}

// GetForModel is Get scoped by model (CHECKLIST 187).
func (p *PersistentSemanticCache) GetForModel(ctx context.Context, model string, embedding []float32) (SemanticEntry, bool) {
	return p.inner.GetForModel(ctx, model, embedding)
}

// GetText returns the cached response for text most similar to
// the given text (raw model-less lookup).
func (p *PersistentSemanticCache) GetText(ctx context.Context, text string) (SemanticEntry, bool) {
	return p.inner.GetText(ctx, text)
}

// GetTextForModel is GetText scoped by model (CHECKLIST 187).
func (p *PersistentSemanticCache) GetTextForModel(ctx context.Context, model, text string) (SemanticEntry, bool) {
	return p.inner.GetTextForModel(ctx, model, text)
}

// GetTextForModelWithThreshold is GetTextForModel with a per-call
// similarity floor (M6.7b): it delegates to the inner
// LocalSemanticCache's method of the same name, so the floor is
// enforced in the cache — floored at DefaultSemanticThreshold
// (0.85), 0 means the configured floor — instead of by a
// post-filter in the caller. The persistent adapter offers it so
// serve mode, which passes the AsStore adapter to the degrade
// wrapper, takes the in-cache path in semanticLookup rather than
// the M6.4 post-filter fallback; the hit/miss decision is
// identical either way (the lookup returns the single best
// entry), only where the floor is enforced changes. No similarity
// matching is re-implemented here: the inner cache owns it.
func (p *PersistentSemanticCache) GetTextForModelWithThreshold(ctx context.Context, model, text string, threshold float32) (SemanticEntry, bool) {
	return p.inner.GetTextForModelWithThreshold(ctx, model, text, threshold)
}

// GetForModelScopedWithThreshold is GetForModelWithThreshold with
// the M6.11 serving scope (M6.11): tenant + TemperatureBucket
// (temperature) must both match the stored entry, and a legacy
// (unscoped) entry is never served. Serving code (degrade,
// relay) must use this, never GetForModel.
func (p *PersistentSemanticCache) GetForModelScopedWithThreshold(ctx context.Context, model string, embedding []float32, tenant string, temperature float64, threshold float32) (SemanticEntry, bool) {
	return p.inner.GetForModelScopedWithThreshold(ctx, model, embedding, tenant, temperature, threshold)
}

// GetTextForModelScopedWithThreshold is the text form of
// GetForModelScopedWithThreshold (M6.11).
func (p *PersistentSemanticCache) GetTextForModelScopedWithThreshold(ctx context.Context, model, text, tenant string, temperature float64, threshold float32) (SemanticEntry, bool) {
	return p.inner.GetTextForModelScopedWithThreshold(ctx, model, text, tenant, temperature, threshold)
}

// SemanticStore is the semantic cache contract shared by the
// in-memory LocalSemanticCache and the persistent
// PersistentSemanticCache (M6.2b write-through): the degrade
// wrapper and the relay read and write through this interface,
// so serve mode can pass the persistent wrapper (via AsStore)
// and every Set reaches disk synchronously instead of waiting
// for the next snapshot tick.
//
// SetTextForModel takes no error here because
// LocalSemanticCache.SetTextForModel returns nothing; the
// persistent wrapper's disk write failure is swallowed by the
// AsStore adapter, matching the degrade wrapper's best-effort
// store rule (a transient disk error never breaks serving —
// the in-memory entry is still stored).
type SemanticStore interface {
	GetTextForModel(ctx context.Context, model, text string) (SemanticEntry, bool)
	SetTextForModel(ctx context.Context, model, text string, entry SemanticEntry, ttl time.Duration)
	Stats() (entries, bytes int)
}

// persistentSemanticStore adapts *PersistentSemanticCache to
// SemanticStore: the only difference from the wrapper's own
// methods is SetTextForModel, whose disk-write error is
// dropped (see SemanticStore).
type persistentSemanticStore struct{ p *PersistentSemanticCache }

func (s persistentSemanticStore) GetTextForModel(ctx context.Context, model, text string) (SemanticEntry, bool) {
	return s.p.GetTextForModel(ctx, model, text)
}

// GetTextForModelWithThreshold is the per-call-threshold lookup
// (M6.7b), delegated to the wrapper: serve mode's degrade wrapper
// type-asserts the store for this method and, when present, pushes
// the request's effective floor into the cache instead of
// post-filtering the hit (see GetTextForModelWithThreshold on
// *PersistentSemanticCache).
func (s persistentSemanticStore) GetTextForModelWithThreshold(ctx context.Context, model, text string, threshold float32) (SemanticEntry, bool) {
	return s.p.GetTextForModelWithThreshold(ctx, model, text, threshold)
}

// GetTextForModelScopedWithThreshold is the M6.11 scoped lookup
// (tenant + temperature bucket, fail-closed on legacy entries),
// delegated to the wrapper: serve mode's degrade wrapper and
// relay type-assert the store for this method and, when present,
// push the request's tenant and temperature into the cache
// instead of serving tenant-/temperature-blind.
func (s persistentSemanticStore) GetTextForModelScopedWithThreshold(ctx context.Context, model, text, tenant string, temperature float64, threshold float32) (SemanticEntry, bool) {
	return s.p.GetTextForModelScopedWithThreshold(ctx, model, text, tenant, temperature, threshold)
}

func (s persistentSemanticStore) SetTextForModel(ctx context.Context, model, text string, entry SemanticEntry, ttl time.Duration) {
	_ = s.p.SetTextForModel(ctx, model, text, entry, ttl)
}

// SetTextForModelScoped is the M6.11 scoped store (tenant +
// temperature bucket), delegated to the wrapper; the disk-write
// error is dropped, matching the adapter's best-effort store
// rule (see SetTextForModel above).
func (s persistentSemanticStore) SetTextForModelScoped(ctx context.Context, model, text, tenant string, temperature float64, entry SemanticEntry, ttl time.Duration) {
	_ = s.p.SetTextForModelScoped(ctx, model, text, tenant, temperature, entry, ttl)
}

func (s persistentSemanticStore) Stats() (entries, bytes int) { return s.p.Stats() }

// AsStore returns p as a SemanticStore, for serve mode: the
// degrade wrapper and the relay store through the returned
// value and every write reaches disk synchronously.
func (p *PersistentSemanticCache) AsStore() SemanticStore {
	return persistentSemanticStore{p: p}
}

// Stats returns the live in-memory entries + bytes, exactly like
// LocalSemanticCache.Stats (the server's cache-stats endpoint
// reads the layers through SemanticStore).
func (p *PersistentSemanticCache) Stats() (entries, bytes int) {
	return p.inner.Stats()
}

// Sync rewrites the semantic_entries table from the live
// in-memory slots in one transaction, on the already-open
// handle — the write-through twin of SnapshotSemanticCache.
// Rows for entries that have expired or been evicted (LRU /
// byte caps) since the last sync are dropped, not resurrected:
// a Set writes its row to disk immediately, but eviction only
// removes the slot from memory, so without a Sync the evicted
// row would be reloaded on the next restart. It returns how
// many entries were written.
//
// SERIALIZATION (M6.2b): the snapshot read AND the rewrite run
// under p.mu, and storeThrough/FlushAll/FlushModel hold p.mu
// for their whole operation (memory + disk). Before this, a
// flush that ran between a snapshot and its commit was undone
// by the commit, resurrecting flushed entries. Lock order is
// always p.mu -> inner.mu: no path holds inner.mu while
// acquiring p.mu, so this cannot deadlock.
func (p *PersistentSemanticCache) Sync(ctx context.Context) (int, error) {
	type snapRow struct {
		model      string
		tenant     string
		tempBucket string
		vec        []float32
		entry      SemanticEntry
		expires    time.Time
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errors.New("cache: persistent semantic cache is closed")
	}
	p.inner.mu.Lock()
	now := time.Now()
	rows := make([]snapRow, 0, p.inner.size)
	for _, e := range p.inner.slots {
		if e == nil || now.After(e.expires) {
			continue
		}
		vec := make([]float32, len(e.vec))
		copy(vec, e.vec)
		rows = append(rows, snapRow{model: e.model, tenant: e.tenant, tempBucket: e.tempBucket, vec: vec, entry: e.entry, expires: e.expires})
	}
	p.inner.mu.Unlock()

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM semantic_entries`); err != nil {
		return 0, err
	}
	for _, r := range rows {
		raw, err := json.Marshal(r.entry)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO semantic_entries(model, tenant, temp_bucket, vec_key, vector, response, expires_unix) VALUES(?,?,?,?,?,?,?)`,
			r.model, r.tenant, r.tempBucket, semanticVecKey(r.vec), encodeSemanticVector(r.vec), string(raw), r.expires.Unix()); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// storeThrough stores entry under vec for ttl in memory and on
// disk. The in-memory store runs first and applies the layer's
// own rules (zero vectors store nothing, oversize entries are
// refused by the M6.1 caps); a row is written only when the
// in-memory store would accept the entry, so disk never holds
// an entry memory would refuse. A DB write failure is returned;
// the in-memory entry is still stored so a transient disk error
// never breaks serving.
//
// SERIALIZATION (M6.2b): the memory store AND the disk write
// run under p.mu (see Sync). Before this, the memory store ran
// outside p.mu, so a FlushAll that cleared memory and deleted
// the disk rows between the two steps was undone by the
// trailing INSERT — the flushed entry resurrected on restart.
func (p *PersistentSemanticCache) storeThrough(ctx context.Context, model string, scope semanticScope, vec []float32, entry SemanticEntry, ttl time.Duration) error {
	if ttl <= 0 || len(vec) == 0 || isZeroVector(vec) {
		return nil
	}
	if semanticEntrySize(vec, entry) > p.inner.maxEntryBytes {
		return nil
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("cache: persistent semantic cache is closed")
	}
	p.inner.setScoped(vec, model, scope, entry, ttl)
	_, err = p.db.ExecContext(ctx,
		`INSERT INTO semantic_entries(model, tenant, temp_bucket, vec_key, vector, response, expires_unix) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(model, tenant, temp_bucket, vec_key) DO UPDATE SET vector=excluded.vector, response=excluded.response, expires_unix=excluded.expires_unix`,
		model, scope.tenant, scope.tempBucket, semanticVecKey(vec), encodeSemanticVector(vec), string(raw), time.Now().Add(ttl).Unix())
	return err
}

// Set stores entry under embedding for the cache's default TTL.
// The entry's model scope is entry.Response.Model.
//
// LEGACY (M6.11): stores an UNSCOPED entry (no tenant /
// temperature recorded); a scoped lookup never serves it.
func (p *PersistentSemanticCache) Set(ctx context.Context, embedding []float32, entry SemanticEntry) error {
	return p.storeThrough(ctx, entry.Response.Model, legacySemanticScope, embedding, entry, p.inner.ttl)
}

// SetForModel is Set with an explicit model scope (CHECKLIST 187).
//
// LEGACY (M6.11): stores an UNSCOPED entry; see Set.
func (p *PersistentSemanticCache) SetForModel(ctx context.Context, model string, embedding []float32, entry SemanticEntry) error {
	return p.storeThrough(ctx, model, legacySemanticScope, embedding, entry, p.inner.ttl)
}

// SetForModelScoped is SetForModel with the M6.11 serving scope:
// the entry is recorded for tenant and
// TemperatureBucket(temperature), and is retrievable only by a
// scoped lookup naming the same model, tenant and temperature
// bucket.
func (p *PersistentSemanticCache) SetForModelScoped(ctx context.Context, model string, embedding []float32, tenant string, temperature float64, entry SemanticEntry) error {
	return p.storeThrough(ctx, model, scopedSemanticScope(tenant, temperature), embedding, entry, p.inner.ttl)
}

// SetText stores entry for text for ttl. The entry's model scope
// is entry.Response.Model.
//
// LEGACY (M6.11): stores an UNSCOPED entry; see Set.
func (p *PersistentSemanticCache) SetText(ctx context.Context, text string, entry SemanticEntry, ttl time.Duration) error {
	return p.storeThrough(ctx, entry.Response.Model, legacySemanticScope, p.inner.embedTextCtx(ctx, text), entry, ttl)
}

// SetTextForModel is SetText with an explicit model scope
// (CHECKLIST 187).
//
// LEGACY (M6.11): stores an UNSCOPED entry; see Set.
func (p *PersistentSemanticCache) SetTextForModel(ctx context.Context, model, text string, entry SemanticEntry, ttl time.Duration) error {
	return p.storeThrough(ctx, model, legacySemanticScope, p.inner.embedTextCtx(ctx, text), entry, ttl)
}

// SetTextForModelScoped is SetTextForModel with the M6.11 serving
// scope: the entry is recorded for tenant and
// TemperatureBucket(temperature), and is retrievable only by a
// scoped lookup naming the same model, tenant and temperature
// bucket. Serving code must use this, never SetTextForModel.
func (p *PersistentSemanticCache) SetTextForModelScoped(ctx context.Context, model, text, tenant string, temperature float64, entry SemanticEntry, ttl time.Duration) error {
	return p.storeThrough(ctx, model, scopedSemanticScope(tenant, temperature), p.inner.embedTextCtx(ctx, text), entry, ttl)
}

// Size returns the number of live entries in memory.
func (p *PersistentSemanticCache) Size() int {
	return p.inner.Size()
}

// FlushAll removes every entry (memory + disk) and returns how
// many live entries were removed. The whole operation holds
// p.mu (see Sync): a concurrent store cannot re-insert a row
// this flush just deleted. Memory is flushed even when closed;
// only the disk delete is skipped then.
func (p *PersistentSemanticCache) FlushAll(ctx context.Context) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.inner.FlushAll()
	if p.closed {
		return n
	}
	_, _ = p.db.ExecContext(ctx, `DELETE FROM semantic_entries`)
	return n
}

// FlushModel removes every entry stored under model (memory +
// disk) and returns how many were removed. Other models are
// untouched. The whole operation holds p.mu (see Sync): a
// concurrent store cannot re-insert a row this flush just
// deleted. Memory is flushed even when closed; only the disk
// delete is skipped then.
func (p *PersistentSemanticCache) FlushModel(ctx context.Context, model string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.inner.FlushModel(model)
	if p.closed {
		return n
	}
	_, _ = p.db.ExecContext(ctx, `DELETE FROM semantic_entries WHERE model = ?`, model)
	return n
}

// Close closes the SQLite file. In-memory entries are already on
// disk (Set writes through), so nothing is lost on close.
func (p *PersistentSemanticCache) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.db.Close()
}

// ---------------------------------------------------------------------------
// Serve-mode load / snapshot helpers (memory-only path).
//
// With persistence, serve mode passes the PersistentSemanticCache
// itself (via AsStore) and stores write through; these helpers
// bridge the same SQLite schema to a plain LocalSemanticCache for
// the memory-only path: LoadSemanticCache populates one at startup
// and SnapshotSemanticCache rewrites the file from its live slots
// (a periodic snapshot — entries stored after the last snapshot
// are lost on a crash, bounded by the snapshot interval).
// ---------------------------------------------------------------------------

// LoadSemanticCache opens the SQLite file at path (creating it if
// needed), prunes expired rows, loads every unexpired entry into
// a fresh LocalSemanticCache, and closes the file. A missing or
// empty file yields an empty cache, not an error.
func LoadSemanticCache(path string, cfg LocalSemanticCacheConfig) (*LocalSemanticCache, error) {
	db, err := openSemanticDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	c := NewLocalSemanticCacheWithConfig(cfg)
	if _, err := loadSemanticRows(db, c); err != nil {
		return nil, err
	}
	return c, nil
}

// SnapshotSemanticCache rewrites the SQLite file at path with the
// live (unexpired) entries of c, in one transaction: rows for
// entries that have expired or been evicted since the last
// snapshot are dropped, not resurrected. It returns how many
// entries were written.
func SnapshotSemanticCache(c *LocalSemanticCache, path string) (int, error) {
	type snapRow struct {
		model      string
		tenant     string
		tempBucket string
		vec        []float32
		entry      SemanticEntry
		expires    time.Time
	}
	c.mu.Lock()
	now := time.Now()
	rows := make([]snapRow, 0, c.size)
	for _, e := range c.slots {
		if e == nil || now.After(e.expires) {
			continue
		}
		vec := make([]float32, len(e.vec))
		copy(vec, e.vec)
		rows = append(rows, snapRow{model: e.model, tenant: e.tenant, tempBucket: e.tempBucket, vec: vec, entry: e.entry, expires: e.expires})
	}
	c.mu.Unlock()

	db, err := openSemanticDB(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM semantic_entries`); err != nil {
		return 0, err
	}
	for _, r := range rows {
		raw, err := json.Marshal(r.entry)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(
			`INSERT INTO semantic_entries(model, tenant, temp_bucket, vec_key, vector, response, expires_unix) VALUES(?,?,?,?,?,?,?)`,
			r.model, r.tenant, r.tempBucket, semanticVecKey(r.vec), encodeSemanticVector(r.vec), string(raw), r.expires.Unix()); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(rows), nil
}
