// M6.2 — persistent semantic cache (Redis).
//
// RedisSemanticCache wraps the in-memory LocalSemanticCache with
// a Redis server (github.com/redis/go-redis/v9) instead of a
// SQLite file: it is the Redis arm of the B2-32 persistence
// tiers, the twin of PersistentSemanticCache in
// semantic_persist.go. Every Set writes through to Redis
// synchronously, so entries survive a process restart when
// Redis persistence is configured; with no Redis address there
// is no Redis tier (callers that want memory-only keep using
// LocalSemanticCache directly, and the SQLite tier stays the
// default when Redis is not configured). Expired rows are pruned
// on open and never reloaded.
//
// One Redis hash per (model, tenant, temperature bucket,
// vector): the vector is stored as a little-endian float32 blob
// alongside the entry JSON and its expiry, keyed by the SHA-256
// of the (model, tenant, temp_bucket, vec_key) tuple so the same
// text stored twice upserts instead of duplicating. The key
// itself is opaque (a hex hash under the configured prefix);
// the hash fields carry model, tenant, temp_bucket, vec_key,
// vector, response and expires_unix, so load / sync / flush can
// recover everything from the hash itself and no key parsing
// can ever collide on a model name containing ":" or "/".
// Scope is the model the layer already records per entry
// (CHECKLIST 187) PLUS, since M6.11, the tenant (gateway key
// id, "" = anonymous) and the temperature bucket
// (cache.TemperatureBucket, same 2-decimal bucketing as the
// exact layer) — so a restart never drops the tenant /
// temperature scoping: a reloaded scoped entry serves only its
// own tenant at its own temperature. Rows with temp_bucket ""
// reload as LEGACY (unscoped) entries, which a scoped lookup
// never serves — they fail closed, never leak. There is still
// no per-tenant flush here (see semantic_flush.go): FlushModel
// is the layer's flush scope.
//
// Each hash also carries a Redis TTL equal to the entry's TTL,
// so Redis prunes expired entries on its own; the stored
// expires_unix is still checked on load, so a key that lost
// its TTL can never resurrect an expired entry.
//
// Similarity matching stays in memory, exactly like the SQLite
// tier: the inner LocalSemanticCache owns it, Redis only
// persists. This is NOT Redis vector search (no RediSearch /
// vector-set commands): it needs only a plain Redis 7.0+
// server, no modules, and the client never invents a score —
// a hit is the inner cache's cosine match or nothing.
//
// Serve-mode note (M6.2b): degrade.WrapWithSemantic and
// server.WithCaches take the SemanticStore interface, so serve
// mode passes this wrapper via AsStore() and every Set writes
// through to Redis synchronously. A Redis that cannot be
// reached at open fails the open (the caller falls back to
// memory-only with a warning, like a bad SQLite path — a bad
// cache backend never fails startup); a Redis that dies later
// degrades to memory-only serving, because storeThrough keeps
// the in-memory entry and only reports the write error.
package cache

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultRedisSemanticPrefix is the key prefix for the Redis
// semantic tier when the operator configures no prefix. All
// entry keys live under it, so a flush or a sync can scope its
// SCAN to exactly this tier's keys and never touch anything
// else the operator stores in the same Redis.
const DefaultRedisSemanticPrefix = "codvian:semantic:"

// RedisOptions carries the Redis connection settings for the
// semantic persistence tier (cache.redis_addr / redis_password /
// redis_db / redis_tls in config). Addr is host:port.
type RedisOptions struct {
	// Addr is the Redis server address, host:port. Empty is
	// rejected by OpenRedisSemanticCache.
	Addr string
	// Password is the AUTH password. Empty means no AUTH.
	Password string
	// DB is the Redis database index. 0 is the default.
	DB int
	// TLS enables TLS to the server (system roots, TLS 1.2+,
	// ServerName taken from Addr's host).
	TLS bool
}

// redisSemanticFields are the hash fields of one stored entry.
const (
	redisFieldModel       = "model"
	redisFieldTenant      = "tenant"
	redisFieldTempBucket  = "temp_bucket"
	redisFieldVecKey      = "vec_key"
	redisFieldVector      = "vector"
	redisFieldResponse    = "response"
	redisFieldExpiresUnix = "expires_unix"
)

// RedisSemanticCache is a LocalSemanticCache backed by a Redis
// server.
type RedisSemanticCache struct {
	mu     sync.Mutex
	client *redis.Client
	prefix string
	inner  *LocalSemanticCache
	closed bool
}

// EmbeddingStats returns the live embedding counters of the inner
// in-memory cache (Stream D observability); it satisfies
// EmbeddingStatsReporter.
func (r *RedisSemanticCache) EmbeddingStats() EmbeddingStats { return r.inner.EmbeddingStats() }

// redisSemanticKey is the stable Redis key for one entry: the
// prefix plus the hex SHA-256 of the (model, tenant,
// temp_bucket, vec_key) tuple. Two identical tuples key
// identically; distinct tuples collide only if SHA-256 does.
func redisSemanticKey(prefix, model, tenant, tempBucket, vecKey string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(model))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(tenant))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(tempBucket))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(vecKey))
	return prefix + hex.EncodeToString(h.Sum(nil))
}

// newRedisClient builds the go-redis client for opts. A bad
// Addr (no host) is rejected here, before any dial, so the
// caller gets a plain error instead of a hung Ping.
func newRedisClient(opts RedisOptions) (*redis.Client, error) {
	addr := strings.TrimSpace(opts.Addr)
	if addr == "" {
		return nil, errors.New("cache: redis address is empty")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, errors.New("cache: redis address must be host:port")
	}
	if host == "" {
		return nil, errors.New("cache: redis address must be host:port")
	}
	ropts := &redis.Options{
		Addr:         addr,
		Password:     opts.Password,
		DB:           opts.DB,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}
	if opts.TLS {
		ropts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	}
	return redis.NewClient(ropts), nil
}

// OpenRedisSemanticCache dials Redis (Ping, 5 s) and loads every
// unexpired entry into memory. Expired rows are deleted, not
// loaded. prefix "" selects DefaultRedisSemanticPrefix. A Redis
// that cannot be reached is an error: the caller keeps serving
// memory-only.
func OpenRedisSemanticCache(opts RedisOptions, prefix string, cfg LocalSemanticCacheConfig) (*RedisSemanticCache, error) {
	client, err := newRedisClient(opts)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(pctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	if prefix == "" {
		prefix = DefaultRedisSemanticPrefix
	}
	p := &RedisSemanticCache{client: client, prefix: prefix, inner: NewLocalSemanticCacheWithConfig(cfg)}
	if err := p.load(context.Background()); err != nil {
		_ = client.Close()
		return nil, err
	}
	return p, nil
}

// load scans every key under the prefix, deletes expired rows,
// and loads every unexpired row into the inner cache via set /
// setScoped (which re-apply the M6.1 byte caps and the model
// scope, so a row that no longer fits is dropped, never
// served). A row with temp_bucket "" is a LEGACY row: it loads
// unscoped, so a scoped lookup never serves it — fail closed.
func (p *RedisSemanticCache) load(ctx context.Context) error {
	now := time.Now()
	var cursor uint64
	for {
		keys, next, err := p.client.Scan(ctx, cursor, p.prefix+"*", 100).Result()
		if err != nil {
			return err
		}
		for _, key := range keys {
			fields, err := p.client.HGetAll(ctx, key).Result()
			if err != nil {
				continue
			}
			if len(fields) == 0 {
				continue
			}
			expiresUnix, err := strconv.ParseInt(fields[redisFieldExpiresUnix], 10, 64)
			if err != nil {
				// No readable expiry: the row cannot be
				// trusted to be live — drop it.
				_, _ = p.client.Del(ctx, key).Result()
				continue
			}
			expires := time.Unix(expiresUnix, 0)
			if !now.Before(expires) {
				_, _ = p.client.Del(ctx, key).Result()
				continue
			}
			vec, ok := decodeSemanticVector([]byte(fields[redisFieldVector]))
			if !ok {
				continue
			}
			var entry SemanticEntry
			if err := json.Unmarshal([]byte(fields[redisFieldResponse]), &entry); err != nil {
				continue
			}
			ttl := time.Until(expires)
			if ttl <= 0 {
				continue
			}
			if fields[redisFieldTempBucket] == "" {
				p.inner.set(vec, fields[redisFieldModel], entry, ttl)
			} else {
				p.inner.setScoped(vec, fields[redisFieldModel], semanticScope{tenant: fields[redisFieldTenant], tempBucket: fields[redisFieldTempBucket], scoped: true}, entry, ttl)
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

// Inner returns the in-memory cache this wrapper persists. The
// caller must not store through it directly — writes bypassing
// the wrapper never reach Redis.
func (p *RedisSemanticCache) Inner() *LocalSemanticCache {
	return p.inner
}

// Get returns the stored response most similar to embedding (raw
// model-less lookup, exactly like LocalSemanticCache.Get).
func (p *RedisSemanticCache) Get(ctx context.Context, embedding []float32) (SemanticEntry, bool) {
	return p.inner.Get(ctx, embedding)
}

// GetForModel is Get scoped by model (CHECKLIST 187).
func (p *RedisSemanticCache) GetForModel(ctx context.Context, model string, embedding []float32) (SemanticEntry, bool) {
	return p.inner.GetForModel(ctx, model, embedding)
}

// GetText returns the cached response for text most similar to
// the given text (raw model-less lookup).
func (p *RedisSemanticCache) GetText(ctx context.Context, text string) (SemanticEntry, bool) {
	return p.inner.GetText(ctx, text)
}

// GetTextForModel is GetText scoped by model (CHECKLIST 187).
func (p *RedisSemanticCache) GetTextForModel(ctx context.Context, model, text string) (SemanticEntry, bool) {
	return p.inner.GetTextForModel(ctx, model, text)
}

// GetTextForModelWithThreshold is GetTextForModel with a per-call
// similarity floor (M6.7b): it delegates to the inner
// LocalSemanticCache's method of the same name, so the floor is
// enforced in the cache — floored at DefaultSemanticThreshold
// (0.85), 0 means the configured floor — instead of by a
// post-filter in the caller. No similarity matching is
// re-implemented here: the inner cache owns it.
func (p *RedisSemanticCache) GetTextForModelWithThreshold(ctx context.Context, model, text string, threshold float32) (SemanticEntry, bool) {
	return p.inner.GetTextForModelWithThreshold(ctx, model, text, threshold)
}

// GetForModelScopedWithThreshold is GetForModelWithThreshold with
// the M6.11 serving scope (M6.11): tenant + TemperatureBucket
// (temperature) must both match the stored entry, and a legacy
// (unscoped) entry is never served. Serving code (degrade,
// relay) must use this, never GetForModel.
func (p *RedisSemanticCache) GetForModelScopedWithThreshold(ctx context.Context, model string, embedding []float32, tenant string, temperature float64, threshold float32) (SemanticEntry, bool) {
	return p.inner.GetForModelScopedWithThreshold(ctx, model, embedding, tenant, temperature, threshold)
}

// GetTextForModelScopedWithThreshold is the text form of
// GetForModelScopedWithThreshold (M6.11).
func (p *RedisSemanticCache) GetTextForModelScopedWithThreshold(ctx context.Context, model, text, tenant string, temperature float64, threshold float32) (SemanticEntry, bool) {
	return p.inner.GetTextForModelScopedWithThreshold(ctx, model, text, tenant, temperature, threshold)
}

// SemanticStore adapter (see AsStore): the only difference from
// the wrapper's own methods is SetTextForModel, whose Redis
// write error is dropped (see SemanticStore).
type redisSemanticStore struct{ p *RedisSemanticCache }

func (s redisSemanticStore) GetTextForModel(ctx context.Context, model, text string) (SemanticEntry, bool) {
	return s.p.GetTextForModel(ctx, model, text)
}

// GetTextForModelWithThreshold is the per-call-threshold lookup
// (M6.7b), delegated to the wrapper: serve mode's degrade wrapper
// type-asserts the store for this method and, when present, pushes
// the request's effective floor into the cache instead of
// post-filtering the hit (see GetTextForModelWithThreshold on
// *RedisSemanticCache).
func (s redisSemanticStore) GetTextForModelWithThreshold(ctx context.Context, model, text string, threshold float32) (SemanticEntry, bool) {
	return s.p.GetTextForModelWithThreshold(ctx, model, text, threshold)
}

// GetTextForModelScopedWithThreshold is the M6.11 scoped lookup
// (tenant + temperature bucket, fail-closed on legacy entries),
// delegated to the wrapper: serve mode's degrade wrapper and
// relay type-assert the store for this method and, when present,
// push the request's tenant and temperature into the cache
// instead of serving tenant-/temperature-blind.
func (s redisSemanticStore) GetTextForModelScopedWithThreshold(ctx context.Context, model, text, tenant string, temperature float64, threshold float32) (SemanticEntry, bool) {
	return s.p.GetTextForModelScopedWithThreshold(ctx, model, text, tenant, temperature, threshold)
}

func (s redisSemanticStore) SetTextForModel(ctx context.Context, model, text string, entry SemanticEntry, ttl time.Duration) {
	_ = s.p.SetTextForModel(ctx, model, text, entry, ttl)
}

// SetTextForModelScoped is the M6.11 scoped store (tenant +
// temperature bucket), delegated to the wrapper; the Redis-write
// error is dropped, matching the adapter's best-effort store
// rule (see SetTextForModel above).
func (s redisSemanticStore) SetTextForModelScoped(ctx context.Context, model, text, tenant string, temperature float64, entry SemanticEntry, ttl time.Duration) {
	_ = s.p.SetTextForModelScoped(ctx, model, text, tenant, temperature, entry, ttl)
}

func (s redisSemanticStore) Stats() (entries, bytes int) { return s.p.Stats() }

// EmbeddingStats forwards the inner cache's live embedding counters (see
// persistentSemanticStore.EmbeddingStats).
func (s redisSemanticStore) EmbeddingStats() EmbeddingStats { return s.p.EmbeddingStats() }

// AsStore returns p as a SemanticStore, for serve mode: the
// degrade wrapper and the relay store through the returned
// value and every write reaches Redis synchronously.
func (p *RedisSemanticCache) AsStore() SemanticStore {
	return redisSemanticStore{p: p}
}

// Stats returns the live in-memory entries + bytes, exactly like
// LocalSemanticCache.Stats (the server's cache-stats endpoint
// reads the layers through SemanticStore).
func (p *RedisSemanticCache) Stats() (entries, bytes int) {
	return p.inner.Stats()
}

// Sync rewrites the tier from the live in-memory slots: every
// key under the prefix is deleted, then one hash per live entry
// is written with a TTL equal to its remaining life. Rows for
// entries that have expired or been evicted (LRU / byte caps)
// since the last sync are dropped, not resurrected: a Set writes
// its row immediately, but eviction only removes the slot from
// memory, so without a Sync the evicted row would be reloaded
// on the next restart. It returns how many entries were
// written.
//
// SERIALIZATION (M6.2b): the snapshot read AND the rewrite run
// under p.mu, and storeThrough/FlushAll/FlushModel hold p.mu
// for their whole operation (memory + Redis). Before this, a
// flush that ran between a snapshot and its commit was undone
// by the commit, resurrecting flushed entries. Lock order is
// always p.mu -> inner.mu: no path holds inner.mu while
// acquiring p.mu, so this cannot deadlock.
func (p *RedisSemanticCache) Sync(ctx context.Context) (int, error) {
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
		return 0, errors.New("cache: redis semantic cache is closed")
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

	if err := p.deleteAll(ctx); err != nil {
		return 0, err
	}
	for _, r := range rows {
		raw, err := json.Marshal(r.entry)
		if err != nil {
			return 0, err
		}
		if err := p.writeRow(ctx, r.model, r.tenant, r.tempBucket, r.vec, string(raw), r.expires); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// deleteAll removes every key under the prefix, in SCAN
// batches. A SCAN error aborts the delete (the caller reports
// it) rather than leaving a half-rewritten tier behind.
func (p *RedisSemanticCache) deleteAll(ctx context.Context) error {
	var cursor uint64
	for {
		keys, next, err := p.client.Scan(ctx, cursor, p.prefix+"*", 100).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := p.client.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

// writeRow stores one entry as a hash under its tuple key and
// sets the key's TTL to the entry's remaining life, so Redis
// prunes it when it expires. A write error is returned; the
// in-memory entry is already stored by the caller.
func (p *RedisSemanticCache) writeRow(ctx context.Context, model, tenant, tempBucket string, vec []float32, raw string, expires time.Time) error {
	key := redisSemanticKey(p.prefix, model, tenant, tempBucket, semanticVecKey(vec))
	ttl := time.Until(expires)
	if ttl <= 0 {
		return nil
	}
	_, err := p.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, key,
			redisFieldModel, model,
			redisFieldTenant, tenant,
			redisFieldTempBucket, tempBucket,
			redisFieldVecKey, semanticVecKey(vec),
			redisFieldVector, encodeSemanticVector(vec),
			redisFieldResponse, raw,
			redisFieldExpiresUnix, strconv.FormatInt(expires.Unix(), 10),
		)
		pipe.Expire(ctx, key, ttl)
		return nil
	})
	return err
}

// storeThrough stores entry under vec for ttl in memory and in
// Redis. The in-memory store runs first and applies the layer's
// own rules (zero vectors store nothing, oversize entries are
// refused by the M6.1 caps); a row is written only when the
// in-memory store would accept the entry, so Redis never holds
// an entry memory would refuse. A Redis write failure is
// returned; the in-memory entry is still stored so a transient
// Redis error never breaks serving.
//
// SERIALIZATION (M6.2b): the memory store AND the Redis write
// run under p.mu (see Sync). Before this, the memory store ran
// outside p.mu, so a FlushAll that cleared memory and deleted
// the Redis rows between the two steps was undone by the
// trailing write — the flushed entry resurrected on restart.
func (p *RedisSemanticCache) storeThrough(ctx context.Context, model string, scope semanticScope, vec []float32, entry SemanticEntry, ttl time.Duration) error {
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
		return errors.New("cache: redis semantic cache is closed")
	}
	p.inner.setScoped(vec, model, scope, entry, ttl)
	return p.writeRow(ctx, model, scope.tenant, scope.tempBucket, vec, string(raw), time.Now().Add(ttl))
}

// Set stores entry under embedding for the cache's default TTL.
// The entry's model scope is entry.Response.Model.
//
// LEGACY (M6.11): stores an UNSCOPED entry (no tenant /
// temperature recorded); a scoped lookup never serves it.
func (p *RedisSemanticCache) Set(ctx context.Context, embedding []float32, entry SemanticEntry) error {
	return p.storeThrough(ctx, entry.Response.Model, legacySemanticScope, embedding, entry, p.inner.ttl)
}

// SetForModel is Set with an explicit model scope (CHECKLIST 187).
//
// LEGACY (M6.11): stores an UNSCOPED entry; see Set.
func (p *RedisSemanticCache) SetForModel(ctx context.Context, model string, embedding []float32, entry SemanticEntry) error {
	return p.storeThrough(ctx, model, legacySemanticScope, embedding, entry, p.inner.ttl)
}

// SetForModelScoped is SetForModel with the M6.11 serving scope:
// the entry is recorded for tenant and
// TemperatureBucket(temperature), and is retrievable only by a
// scoped lookup naming the same model, tenant and temperature
// bucket.
func (p *RedisSemanticCache) SetForModelScoped(ctx context.Context, model string, embedding []float32, tenant string, temperature float64, entry SemanticEntry) error {
	return p.storeThrough(ctx, model, scopedSemanticScope(tenant, temperature), embedding, entry, p.inner.ttl)
}

// SetText stores entry for text for ttl. The entry's model scope
// is entry.Response.Model.
//
// LEGACY (M6.11): stores an UNSCOPED entry; see Set.
func (p *RedisSemanticCache) SetText(ctx context.Context, text string, entry SemanticEntry, ttl time.Duration) error {
	return p.storeThrough(ctx, entry.Response.Model, legacySemanticScope, p.inner.embedTextCtx(ctx, text), entry, ttl)
}

// SetTextForModel is SetText with an explicit model scope
// (CHECKLIST 187).
//
// LEGACY (M6.11): stores an UNSCOPED entry; see Set.
func (p *RedisSemanticCache) SetTextForModel(ctx context.Context, model, text string, entry SemanticEntry, ttl time.Duration) error {
	return p.storeThrough(ctx, model, legacySemanticScope, p.inner.embedTextCtx(ctx, text), entry, ttl)
}

// SetTextForModelScoped is SetTextForModel with the M6.11 serving
// scope: the entry is recorded for tenant and
// TemperatureBucket(temperature), and is retrievable only by a
// scoped lookup naming the same model, tenant and temperature
// bucket. Serving code must use this, never SetTextForModel.
func (p *RedisSemanticCache) SetTextForModelScoped(ctx context.Context, model, text, tenant string, temperature float64, entry SemanticEntry, ttl time.Duration) error {
	return p.storeThrough(ctx, model, scopedSemanticScope(tenant, temperature), p.inner.embedTextCtx(ctx, text), entry, ttl)
}

// Size returns the number of live entries in memory.
func (p *RedisSemanticCache) Size() int {
	return p.inner.Size()
}

// FlushAll removes every entry (memory + Redis) and returns how
// many live entries were removed. The whole operation holds
// p.mu (see Sync): a concurrent store cannot re-insert a row
// this flush just deleted. Memory is flushed even when closed;
// only the Redis delete is skipped then.
func (p *RedisSemanticCache) FlushAll(ctx context.Context) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.inner.FlushAll()
	if p.closed {
		return n
	}
	_ = p.deleteAll(ctx)
	return n
}

// FlushModel removes every entry stored under model (memory +
// Redis) and returns how many were removed. Other models are
// untouched. The whole operation holds p.mu (see Sync): a
// concurrent store cannot re-insert a row this flush just
// deleted. Memory is flushed even when closed; only the Redis
// delete is skipped then.
func (p *RedisSemanticCache) FlushModel(ctx context.Context, model string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.inner.FlushModel(model)
	if p.closed {
		return n
	}
	// The keys are opaque hashes, so the model match is on the
	// hash field, not the key: scan the tier, delete the rows
	// naming this model.
	var cursor uint64
	for {
		keys, next, err := p.client.Scan(ctx, cursor, p.prefix+"*", 100).Result()
		if err != nil {
			return n
		}
		var doomed []string
		for _, key := range keys {
			m, err := p.client.HGet(ctx, key, redisFieldModel).Result()
			if err != nil {
				continue
			}
			if m == model {
				doomed = append(doomed, key)
			}
		}
		if len(doomed) > 0 {
			_ = p.client.Del(ctx, doomed...).Err()
		}
		cursor = next
		if cursor == 0 {
			return n
		}
	}
}

// Close closes the Redis client. In-memory entries are already
// in Redis (Set writes through), so nothing is lost on close.
func (p *RedisSemanticCache) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.client.Close()
}
