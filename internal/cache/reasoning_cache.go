// Reasoning replay cache (M6.6, PARITY.md Area 6 row "Reasoning cache
// (2 h)"): the cache-package half of OmniRoute's reasoningCache.ts.
//
// WHAT OMNIROUTE CACHES (open-sse/services/reasoningCache.ts, read-only
// study — no code copied): reasoning_content strings captured from
// thinking-mode responses (DeepSeek V4, Kimi K2, Qwen-Thinking, GLM,
// ...), re-injected on later turns when the client omits them, so the
// upstream does not 400 with "reasoning_content in the thinking mode
// must be passed back". Two key forms:
//
//   - tool-call key: one tool_call id, scoped by the request owner
//     (api-key id + session). Client ids like `call_0` are reused
//     across callers, so the bare id never selects an entry.
//   - conversation key: SHA-256 of the scope + the canonicalised
//     assistant-message transcript up to that turn, used when the
//     assistant message carries no tool calls.
//
// TTL is 2 h (TTL_MS = 2*60*60*1000), the memory tier holds at most
// 200 entries (MAX_MEMORY_ENTRIES) with the oldest evicted first,
// and one entry is at most 10,000 chars (longer reasoning is
// TRUNCATED, not refused). Reads are memory first, DB fallback,
// miss. There is no temperature gate and no per-entry refusal:
// replay is a protocol requirement, not a determinism cache.
//
// WHAT THIS TYPE IS — AND IS NOT:
//   - It is the memory tier with an optional SQLite tier
//     (G13, reasoning_persist.go): OpenPersistentReasoningCache
//     returns this same type with a DB handle attached, so a
//     restart reloads entries instead of losing them. A
//     memory-only cache (NewReasoningCache) still has no
//     fallback — a miss is a miss, and a restart loses the
//     entries (a later turn then misses and the hop proceeds
//     without restored reasoning, never with a wrong replay).
//     That matches the stated limit of the sibling
//     internal/reasoning.Store, which already implements the
//     transcript-keyed replay store (2 h TTL, 200 entries,
//     tenant-scoped) that the router's Engine consults
//     (internal/router/reasoning.go: injectReasoning /
//     captureReasoning). This type ADDS what that store does not
//     have: the tool-call-id keying (the primary OmniRoute path —
//     a multi-turn tool conversation replays by call id, not by
//     re-digesting the whole transcript), the M6.1 byte caps, and
//     a cache-package home so the degrade wrapper can consult it
//     on the serving path above the router. The two stores overlap
//     deliberately and agree: both scope by tenant, both expire
//     at 2 h, both refuse empty reasoning.
//   - It stores the FULL reasoning state (reasoning.State:
//     thinking blocks with their signatures, Bedrock
//     reasoningContent, redacted blocks), not text only. Get
//     returns the text view (ReasoningText()); GetState returns
//     the cloned full state. A hop that needs a signature must
//     therefore NOT fall back to a text-only replay: the degrade
//     layer injects the stored state verbatim, so what the
//     router's own store (internal/reasoning.Store) would have
//     injected for the same turn is exactly what this cache
//     injects (M6.6b shadowing fix — a text-only inject used to
//     pre-empt the router's signed state and fail Anthropic /
//     Bedrock hops). The byte caps bound the state's STORED
//     size (reasoning.State.StoredBytes: text + signatures +
//     redacted/Data payloads, G13) — opaque payloads ride with
//     the entry AND count against the 2 MiB total, so a
//     signature-heavy entry cannot hide behind a short text
//     view. Stats.Bytes counts stored bytes.
//   - DB tier (G13, reasoning_persist.go): OpenPersistent-
//     ReasoningCache wraps this same type with a SQLite file —
//     every store writes through to disk synchronously, a
//     restart reloads unexpired entries (expired rows are
//     pruned on open, never reloaded), and a final Sync +
//     Close on clean shutdown drops evicted rows. The
//     in-memory type is unchanged: a memory-only cache is
//     still just NewReasoningCache(), and the persistent form
//     is the same *ReasoningCache with a persist handle
//     attached, so the degrade wrapper needs no new type.
//   - Caps follow the M6.1 pattern (cache.go: DefaultMaxEntryBytes
//     256 KiB per entry, DefaultMaxTotalBytes 2 MiB total), NOT
//     OmniRoute's 10,000-char truncation: an entry over the
//     per-entry cap is REFUSED, never silently truncated —
//     truncating reasoning text could hand the upstream a
//     half-thought that fails its own validation. The entry cap
//     (256 KiB) is far above OmniRoute's 10 KiB truncation point,
//     so in practice strictly more reasoning is kept, whole.
//     Eviction is oldest-created first (OmniRoute's evictOldest),
//     not LRU: a replay entry's value does not grow with reuse
//     the way a content-cache hit's does.
//   - Tenant scoping is in the key (SHA-256 of tenant + id /
//     tenant + transcript digest), exactly like ScopedKey: two
//     tenants with the same tool-call id never share an entry,
//     and the tenant never appears in cleartext in the key.
//     Temperature gating is NOT in this type — it is applied at
//     the degrade store site via cache.ShouldStore's temperature
//     rule (see degrade.storeReasoning), because only the caller
//     knows the request temperature and the per-key allowance.
//     The tool-call exclusion inside ShouldStore is deliberately
//     NOT applied there: reasoning replay exists precisely for
//     tool-call turns, and storing reasoning text cannot drop
//     tool calls (the client replays them in the transcript).
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// DefaultReasoningTTL is the lifetime of a reasoning entry: 2 h,
// OmniRoute's reasoningCache.ts TTL_MS.
const DefaultReasoningTTL = 2 * time.Hour

// DefaultReasoningMaxEntries caps the reasoning cache: 200,
// OmniRoute's MAX_MEMORY_ENTRIES.
const DefaultReasoningMaxEntries = 200

// DefaultReasoningMaxEntryBytes is the per-entry cap (M6.1): an
// entry whose reasoning text exceeds 256 KiB is refused, never
// truncated (see the package doc).
const DefaultReasoningMaxEntryBytes = DefaultMaxEntryBytes

// DefaultReasoningMaxTotalBytes is the total memory cap (M6.1):
// 2 MiB across all entries.
const DefaultReasoningMaxTotalBytes = DefaultMaxTotalBytes

// ReasoningTurn is one message of a transcript for conversation
// keying: role, visible text, and tool calls. Reasoning state is
// deliberately NOT part of the key — a turn's key must be the same
// whether its reasoning was captured, injected, or stripped by
// the client, or capture and injection could never meet. This
// mirrors reasoning.Turn without importing internal/reasoning, so
// this package stays a leaf.
type ReasoningTurn struct {
	Role      string              `json:"role"`
	Text      string              `json:"text,omitempty"`
	ToolCalls []ReasoningToolCall `json:"tool_calls,omitempty"`
}

// ReasoningToolCall is one tool call inside a ReasoningTurn, for
// key purposes: the id, name and arguments as the client replayed
// them.
type ReasoningToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolCallKey derives the tool-call cache key for one tool_call
// id under one tenant: "tool-call:" + SHA-256 hex of
// tenant + "\x1f" + toolCallID. The tenant is the gateway key id
// ("" for anonymous callers — still hashed, so an anonymous
// request never collides with a named tenant). Clients reuse ids
// such as `call_0`, so the bare id must never be the key. An
// empty toolCallID yields "" (no key — never store or look up
// under it).
func ToolCallKey(tenant, toolCallID string) string {
	if toolCallID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(tenant + "\x1f" + toolCallID))
	return "tool-call:" + hex.EncodeToString(sum[:])
}

// ConversationKey derives the conversation cache key for the
// transcript turns up to (and including) one assistant turn,
// under one tenant: "conversation:" + SHA-256 hex of tenant +
// "\x1f" + canonical JSON of the turns. It returns "" when the
// tenant is empty AND turns is empty — callers must pass the
// scope and at least one turn, as OmniRoute's
// buildAssistantMessageCacheKey requires (an empty history can
// never match the read side). The tenant alone is not enough to
// refuse: an anonymous caller ("") with a real transcript keys
// fine, scoped to the empty tenant.
func ConversationKey(tenant string, turns []ReasoningTurn) string {
	if len(turns) == 0 {
		return ""
	}
	if turns == nil {
		turns = []ReasoningTurn{}
	}
	payload, err := json.Marshal(struct {
		Tenant string          `json:"tenant"`
		Turns  []ReasoningTurn `json:"turns"`
	}{Tenant: tenant, Turns: turns})
	if err != nil {
		// json.Marshal of this shape cannot fail; if it ever
		// did, return a key that can never match a stored
		// entry rather than a colliding one.
		return ""
	}
	sum := sha256.Sum256(payload)
	return "conversation:" + hex.EncodeToString(sum[:])
}

// ReasoningStats is the cache's accounting: counts only, no
// reasoning content, no keys, no transcripts. Entries/Bytes are
// live; Hits/Misses count lookups; Evicted counts entries dropped
// by the caps (expired entries deleted lazily on lookup are not
// counted here). All counters start at zero and move only when
// real entries flow through the cache — never zero-filled.
type ReasoningStats struct {
	Entries  int            `json:"entries"`
	Bytes    int            `json:"bytes"`
	Hits     uint64         `json:"hits"`
	Misses   uint64         `json:"misses"`
	Evicted  uint64         `json:"evicted"`
	ByModel  map[string]int `json:"by_model,omitempty"`
	ByTenant map[string]int `json:"by_tenant,omitempty"`
}

// ReasoningCacheConfig tunes a ReasoningCache. Zero values select
// the defaults (2 h TTL, 200 entries, 256 KiB per entry, 2 MiB
// total).
type ReasoningCacheConfig struct {
	TTL           time.Duration
	MaxEntries    int
	MaxEntryBytes int
	MaxTotalBytes int
}

type reasoningEntry struct {
	reasoning string // state.ReasoningText(), cached for the text view
	size      int    // st.StoredBytes(): text + signatures + Data payloads (G13)
	state     reasoning.State
	provider  string
	model     string
	tenant    string // recorded for stats only; the KEY already scopes by tenant
	expires   time.Time
	created   time.Time
}

// ReasoningCache is a concurrency-safe in-memory reasoning replay
// cache: raw reasoning text keyed by tool-call id or conversation
// digest, tenant-scoped, 2 h TTL, oldest-first eviction under the
// M6.1 byte caps. See the package doc for what it does and does
// not do, and for how it relates to internal/reasoning.Store.
//
// persist, when non-nil, is the SQLite write-through tier
// (reasoning_persist.go): stores also upsert a row, deletes also
// delete the row, and Close/Sync flush the tier. Lock order is
// ALWAYS persist.mu -> c.mu, never the reverse: mutating methods
// take persist.mu first (when present) and then c.mu, so a Sync
// that snapshots under both locks cannot deadlock against a
// store. Get paths take c.mu only — reads never touch disk.
type ReasoningCache struct {
	mu            sync.Mutex
	ttl           time.Duration
	maxEntries    int
	maxEntryBytes int
	maxTotalBytes int
	totalBytes    int
	items         map[string]*reasoningEntry
	order         []string // creation order, oldest first (eviction)
	hits          uint64
	misses        uint64
	evicted       uint64
	persist       *reasoningPersist
}

// NewReasoningCache returns a ReasoningCache with the defaults:
// 2 h TTL, 200 entries, 256 KiB per entry, 2 MiB total.
func NewReasoningCache() *ReasoningCache {
	return NewReasoningCacheWithConfig(ReasoningCacheConfig{})
}

// NewReasoningCacheWithConfig returns a ReasoningCache tuned by
// cfg; zero fields select the defaults.
func NewReasoningCacheWithConfig(cfg ReasoningCacheConfig) *ReasoningCache {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultReasoningTTL
	}
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultReasoningMaxEntries
	}
	maxEntryBytes := cfg.MaxEntryBytes
	if maxEntryBytes <= 0 {
		maxEntryBytes = DefaultReasoningMaxEntryBytes
	}
	maxTotalBytes := cfg.MaxTotalBytes
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultReasoningMaxTotalBytes
	}
	return &ReasoningCache{
		ttl:           ttl,
		maxEntries:    maxEntries,
		maxEntryBytes: maxEntryBytes,
		maxTotalBytes: maxTotalBytes,
		items:         make(map[string]*reasoningEntry, maxEntries),
		order:         make([]string, 0, maxEntries),
	}
}

// Get returns the cached reasoning text for key, and whether it
// was found. An expired entry counts as a miss and is deleted.
// An empty key always misses. The returned text is a copy of the
// stored string (strings are immutable, so no clone is needed).
// A stored state with no text at all (e.g. a redacted-only block)
// reports ("", true): the entry exists, it just has no text view
// — use GetState for the full state.
func (c *ReasoningCache) Get(ctx context.Context, key string) (string, bool) {
	if key == "" {
		c.mu.Lock()
		c.misses++
		c.mu.Unlock()
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		c.misses++
		return "", false
	}
	if time.Now().After(e.expires) {
		c.removeLocked(key)
		c.misses++
		return "", false
	}
	c.hits++
	return e.reasoning, true
}

// GetState returns the cached FULL reasoning state for key, and
// whether it was found — the state the degrade layer injects
// verbatim (M6.6b). Same hit/miss/expiry accounting as Get. The
// returned state is a clone: mutating it never mutates the
// store. An entry stored via text-only Set yields the
// synthesised thought state (one KindThought block).
func (c *ReasoningCache) GetState(ctx context.Context, key string) (reasoning.State, bool) {
	if key == "" {
		c.mu.Lock()
		c.misses++
		c.mu.Unlock()
		return reasoning.State{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		c.misses++
		return reasoning.State{}, false
	}
	if time.Now().After(e.expires) {
		c.removeLocked(key)
		c.misses++
		return reasoning.State{}, false
	}
	c.hits++
	return *e.state.Clone(), true
}

// Set stores reasoning text under key for the cache's TTL. It
// stores nothing when the key or the reasoning is empty, or when
// the text exceeds the per-entry byte cap (refused, never
// truncated — see the package doc). provider and model are
// recorded on the entry for stats only ("" when unknown — the
// degrade layer does not know the serving provider; see its
// doc). The stored state is the synthesised text-only form: one
// KindThought block carrying the text, no signature. A repeat
// Set for the same key replaces the entry in place (refreshed
// expiry, recency unchanged — creation order decides eviction,
// so a refresh must not promote an entry). Eviction is
// oldest-created first, while the entry count or the total bytes
// would exceed the caps.
func (c *ReasoningCache) Set(ctx context.Context, key, provider, model, text string) {
	c.SetForTenant(ctx, key, "", provider, model, text)
}

// SetForTenant is Set with the tenant recorded for stats. The
// tenant must match the tenant the key was derived with — it is
// never used for lookup, only for the ByTenant stat — so a
// mismatched value can only mislabel stats, never serve an entry
// across tenants (the key itself carries the scope).
func (c *ReasoningCache) SetForTenant(ctx context.Context, key, tenant, provider, model, text string) {
	if key == "" || text == "" {
		return
	}
	if len(text) > c.maxEntryBytes {
		return
	}
	st := reasoning.State{
		Blocks:      []reasoning.Block{{Kind: reasoning.KindThought, Text: text}},
		SourceModel: model,
	}
	c.setStateForTenant(key, tenant, provider, model, st, text)
}

// SetState stores a FULL reasoning state under key for the
// cache's TTL (M6.6b): thinking blocks with their signatures,
// Bedrock reasoningContent and redacted blocks survive verbatim,
// so the degrade layer can inject exactly what the router's own
// store would have injected for the same turn. It stores
// nothing when the key is empty or the state is empty. The
// per-entry cap applies to the state's STORED size
// (st.StoredBytes(), G13) — never truncated, refused when over;
// a state with no text at all (redacted-only, or a lone thought
// signature) is stored regardless of text length, because there
// is no text to cap — but its signature/Data bytes still count
// toward the cap and the total. provider and model are
// recorded for stats only, as in Set. A repeat SetState for the
// same key replaces the entry in place, like Set.
func (c *ReasoningCache) SetState(ctx context.Context, key, provider, model string, st reasoning.State) {
	c.SetStateForTenant(ctx, key, "", provider, model, st)
}

// SetStateForTenant is SetState with the tenant recorded for
// stats (same tenant rule as SetForTenant).
func (c *ReasoningCache) SetStateForTenant(ctx context.Context, key, tenant, provider, model string, st reasoning.State) {
	if key == "" || st.Empty() {
		return
	}
	// The per-entry cap applies to the STORED size (G13), not
	// the text length: a state with a short text view but a
	// long signature / redacted payload is refused when its
	// stored bytes exceed the cap, never truncated.
	if st.StoredBytes() > c.maxEntryBytes {
		return
	}
	text := st.ReasoningText()
	c.setStateForTenant(key, tenant, provider, model, st, text)
}

// setStateForTenant stores a clone of st under key for the
// cache's TTL, accounting st.StoredBytes() bytes (G13: text +
// signatures + Data payloads). A repeat store for the same key
// replaces the entry in place (refreshed expiry, creation order
// unchanged); otherwise the oldest entries are evicted first
// while the caps would be exceeded. Caller supplies
// text = st.ReasoningText(). When the SQLite tier is attached
// the row is written through under the persist lock (see the
// ReasoningCache doc for the lock order).
func (c *ReasoningCache) setStateForTenant(key, tenant, provider, model string, st reasoning.State, text string) {
	c.setStateForTenantTTL(key, tenant, provider, model, st, text, c.ttl)
}

// setStateForTenantTTL is setStateForTenant with an explicit TTL:
// the load path (reasoning_persist.go) restores the row's
// REMAINING TTL instead of granting a fresh 2 h — a row with
// 10 minutes left must not come back with 2 h.
func (c *ReasoningCache) setStateForTenantTTL(key, tenant, provider, model string, st reasoning.State, text string, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	size := st.StoredBytes()
	if c.persist != nil {
		c.persist.mu.Lock()
		defer c.persist.mu.Unlock()
		expires := c.setStateLocked(key, tenant, provider, model, st, text, size, ttl)
		if !expires.IsZero() {
			_ = c.persistStore(key, tenant, provider, model, st, expires)
		}
		return
	}
	c.setStateLocked(key, tenant, provider, model, st, text, size, ttl)
}

// setStateLocked stores a clone of st under key. Caller holds
// c.mu, or holds persist.mu (which is always taken before c.mu
// — see the ReasoningCache doc). It returns the entry's expiry
// (zero when the entry was refused by the caps — callers with a
// persist tier must not write a row for a refused entry).
func (c *ReasoningCache) setStateLocked(key, tenant, provider, model string, st reasoning.State, text string, size int, ttl time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if size > c.maxEntryBytes {
		return time.Time{}
	}
	expires := now.Add(ttl)
	if old, ok := c.items[key]; ok {
		c.totalBytes -= old.size
		c.items[key] = &reasoningEntry{
			reasoning: text, size: size, state: *st.Clone(), provider: provider, model: model,
			tenant: tenant, expires: expires, created: old.created,
		}
		c.totalBytes += size
		return expires
	}
	for len(c.items) >= c.maxEntries || c.totalBytes+size > c.maxTotalBytes {
		if !c.evictOldestLocked() {
			break
		}
	}
	c.items[key] = &reasoningEntry{
		reasoning: text, size: size, state: *st.Clone(), provider: provider, model: model,
		tenant: tenant, expires: expires, created: now,
	}
	c.order = append(c.order, key)
	c.totalBytes += size
	return expires
}

// Delete removes key if present. With the SQLite tier attached
// the disk row is deleted too, so a restart cannot reload the
// entry (lock order persist.mu -> c.mu, see the ReasoningCache
// doc).
func (c *ReasoningCache) Delete(ctx context.Context, key string) {
	if key == "" {
		return
	}
	if c.persist != nil {
		c.persist.mu.Lock()
		defer c.persist.mu.Unlock()
		c.mu.Lock()
		c.removeLocked(key)
		c.mu.Unlock()
		c.persistDelete(key)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(key)
}

// FlushAll removes every entry and returns how many were removed.
// With the SQLite tier attached the disk rows are deleted too,
// so a flush cannot be undone by the next restart (lock order
// persist.mu -> c.mu).
func (c *ReasoningCache) FlushAll() int {
	if c.persist != nil {
		c.persist.mu.Lock()
		defer c.persist.mu.Unlock()
		c.mu.Lock()
		n := c.flushAllLocked()
		c.mu.Unlock()
		c.persistDeleteAll()
		return n
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flushAllLocked()
}

// flushAllLocked removes every entry. Caller holds c.mu.
func (c *ReasoningCache) flushAllLocked() int {
	n := len(c.items)
	c.items = make(map[string]*reasoningEntry, c.maxEntries)
	c.order = c.order[:0]
	c.totalBytes = 0
	return n
}

// Stats returns the live accounting snapshot. ByModel/ByTenant
// count live entries per recorded model / tenant; entries stored
// with "" model or tenant count under "".
func (c *ReasoningCache) Stats() ReasoningStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := ReasoningStats{
		Entries:  len(c.items),
		Bytes:    c.totalBytes,
		Hits:     c.hits,
		Misses:   c.misses,
		Evicted:  c.evicted,
		ByModel:  make(map[string]int, len(c.items)),
		ByTenant: make(map[string]int, len(c.items)),
	}
	for _, e := range c.items {
		st.ByModel[e.model]++
		st.ByTenant[e.tenant]++
	}
	return st
}

// Size returns the number of entries currently stored (including
// any not-yet-expired entries that have passed their TTL but not
// been accessed since).
func (c *ReasoningCache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// evictOldestLocked drops the oldest-created entry. Caller holds
// mu. It returns false when there is nothing to evict.
func (c *ReasoningCache) evictOldestLocked() bool {
	for len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if e, ok := c.items[oldest]; ok {
			c.totalBytes -= e.size
			delete(c.items, oldest)
			c.evicted++
			return true
		}
	}
	return false
}

// removeLocked deletes key's entry and its creation-order
// position, accounting its bytes out. Caller holds mu.
func (c *ReasoningCache) removeLocked(key string) {
	e, ok := c.items[key]
	if !ok {
		return
	}
	c.totalBytes -= e.size
	delete(c.items, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}
