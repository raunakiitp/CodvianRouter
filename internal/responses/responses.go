// Package responses is the gateway's store for OpenAI Responses API
// objects (CHECKLIST 169-170): one record per POST /v1/responses
// create, keyed by the gateway-minted response id, so a later
// create naming previous_response_id resumes the same conversation
// and GET /v1/responses/{id} can retrieve the served object.
//
// The store is deliberately a leaf package (stdlib only, no internal
// imports): the record holds the conversation transcript and the
// served response object as opaque JSON, so the store never needs
// to know the chat types. The server translates between the
// Responses shape and the canonical chat request and owns all
// meaning; this package only stores bytes under an id.
//
// Limits, stated honestly (same contract as the reasoning replay
// store, internal/reasoning): the store is in-memory while the
// process lives, and its contents persist across a restart in
// responses.json in the data dir (see persist.go) — a restart
// restores the live records, so a later continuation works
// instead of answering 404 (response_not_found). TTL is 2h and
// the cap is 200 entries; eviction is oldest-created first, and
// a restart never restores a record the TTL or the cap would
// already have dropped. Nothing in the store is ever rendered
// to a dashboard feed except identifiers and counts (id, model,
// status, previous id, provider) — never input or output text.
package responses

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

const (
	// DefaultTTL is how long a stored response stays continuable.
	DefaultTTL = 2 * time.Hour
	// DefaultMaxEntries caps the in-memory store.
	DefaultMaxEntries = 200
)

// Statuses of a stored response. "in_progress" is the live state
// of a stream whose terminal event has not arrived yet; a stream
// that ends in error is stored "failed" (its partial output is
// kept in the response object, so a retrieve shows what was
// actually delivered, not a success).
const (
	StatusCompleted  = "completed"
	StatusIncomplete = "incomplete"
	StatusFailed     = "failed"
	StatusInProgress = "in_progress"
)

// Record is one served Responses object. TranscriptJSON is the
// conversation transcript as a JSON array of chat messages (the
// full history the next continuation re-sends to the engine,
// including the assistant turn this response produced).
// ResponseJSON is the exact response object as served to the
// client, so GET /v1/responses/{id} returns byte-identical
// content. ProviderID is the provider that served the response
// ("" when unknown — e.g. a cache-served turn, which this slice
// excludes from the cache, so in practice always set for live
// traffic; a continuation pins to it best-effort, see
// internal/router pinChain).
type Record struct {
	ID             string          `json:"id"`
	Model          string          `json:"model"`
	ProviderID     string          `json:"provider"`
	Status         string          `json:"status"`
	CreatedAt      int64           `json:"created_at"`
	PreviousID     string          `json:"previous_response_id,omitempty"`
	Stream         bool            `json:"stream"`
	TranscriptJSON json.RawMessage `json:"-"`
	ResponseJSON   json.RawMessage `json:"-"`
	Owner          string          `json:"-"`
	// Session token accounting (CHECKLIST 243, LiteLLM #50730):
	// per-turn admission for long-lived sessions counts a turn's
	// tokens before the next turn is admitted. Turn* is this
	// response's own turn (prompt + completion as counted by the
	// server — provider-reported usage when the upstream reported
	// it, the local estimate otherwise); Session* is the
	// cumulative total for the whole session chain up to and
	// including this response (this turn plus every ancestor
	// reachable through PreviousID). Counts only, never text.
	TurnInputTokens     int `json:"turn_input_tokens,omitempty"`
	TurnOutputTokens    int `json:"turn_output_tokens,omitempty"`
	SessionInputTokens  int `json:"session_input_tokens,omitempty"`
	SessionOutputTokens int `json:"session_output_tokens,omitempty"`
	// KeyID is the gateway key that opened this response (M5.6):
	// the owner for GET /v1/me/sessions, which lists only the
	// caller's own sessions. "" on records stored before M5.6
	// (pre-migration rows) — those are never listed for any
	// caller, rather than attributed to the wrong key.
	KeyID string `json:"key_id,omitempty"`
}

// Stats is the store's accounting: counts only, no content, no
// transcripts. Stored is the live size; Created the responses
// ever stored; Continuations the creates that named a
// previous_response_id; Evicted the entries dropped by the cap
// (expired entries are deleted lazily on lookup and are not
// counted here).
type Stats struct {
	Stored        int    `json:"stored"`
	Created       uint64 `json:"created"`
	Continuations uint64 `json:"continuations"`
	Evicted       uint64 `json:"evicted"`
}

// NewID mints a gateway response id: "resp_" + 24 hex chars
// (12 random bytes). The gateway mints its own ids rather than
// passing an upstream id through: the upstream never sees a
// Responses request (the gateway translates to chat completions
// internally), so there is no upstream id to reuse, and a
// gateway-minted id is stable across providers for the same
// conversation.
func NewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "resp_" + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))[:24]
	}
	return "resp_" + hex.EncodeToString(b[:])
}

// NewMsgID mints an output-item id: "msg_" + 24 hex chars.
func NewMsgID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "msg_" + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))[:24]
	}
	return "msg_" + hex.EncodeToString(b[:])
}

type storeEntry struct {
	rec     Record
	created time.Time
	owner   string
}

// Store is the in-memory response store. Safe for concurrent use.
type Store struct {
	mu         sync.Mutex
	entries    map[string]storeEntry
	order      []string // creation order, oldest first (eviction)
	ttl        time.Duration
	maxEntries int

	created       uint64
	continuations uint64
	evicted       uint64
}

// New returns a Store with the default TTL (2h) and cap (200).
func New() *Store {
	return NewStore(DefaultTTL, DefaultMaxEntries)
}

// NewStore returns a Store with an explicit TTL and cap (0 cap
// means unbounded; a non-positive TTL disables expiry). Tests use
// it to shrink both without waiting out the defaults.
func NewStore(ttl time.Duration, maxEntries int) *Store {
	return &Store{
		entries:    make(map[string]storeEntry),
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

// Put stores one response record. A repeat Put for the same id
// replaces the entry (a stream's in_progress placeholder updated
// to its terminal state) without growing the store or counting a
// second creation.
func (s *Store) Put(rec Record) {
	if s == nil || rec.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putWithCreatedLocked(rec, time.Now())
}

// putWithCreatedLocked is Put with an explicit creation time.
// The caller holds s.mu. A repeat Put replaces the entry in
// place (same id, new record, fresh creation time) without
// growing the store or counting a second creation.
func (s *Store) putWithCreatedLocked(rec Record, created time.Time) {
	if _, ok := s.entries[rec.ID]; !ok {
		s.entries[rec.ID] = storeEntry{rec: rec, created: created, owner: rec.Owner}
		s.order = append(s.order, rec.ID)
		s.created++
		if rec.PreviousID != "" {
			s.continuations++
		}
		s.evictLocked()
		return
	}
	// Replace in place. The owner MUST be carried onto the
	// replacement: a stream stores an in_progress placeholder and
	// then overwrites it with its terminal state (same id), and if
	// the replace dropped the owner every streamed response would
	// end up unowned — unfetchable by its own tenant and visible
	// to the owner-less bucket. An empty owner on the replacement
	// (a caller that did not set one) keeps the previous owner
	// rather than silently orphaning the record.
	owner := rec.Owner
	if owner == "" {
		owner = s.entries[rec.ID].owner
	}
	s.entries[rec.ID] = storeEntry{rec: rec, created: created, owner: owner}
}

// Snapshot returns the live records, oldest first, as full
// copies (transcript + served object included — this is the
// persistence snapshot, not a feed). Expired entries are
// skipped and pruned as they are found, so a snapshot never
// persists a record the store would already have dropped.
func (s *Store) Snapshot() []Record {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.entries))
	var expired []string
	for _, id := range s.order {
		e, ok := s.entries[id]
		if !ok {
			continue
		}
		if s.ttl > 0 && time.Since(e.created) > s.ttl {
			expired = append(expired, id)
			continue
		}
		out = append(out, e.rec)
	}
	for _, id := range expired {
		delete(s.entries, id)
		s.removeFromOrderLocked(id)
	}
	return out
}

// Restore loads records persisted by an earlier process (see
// LoadFile) back into the store, oldest first, and returns how
// many were restored. A record older than the TTL is skipped —
// a restart never resurrects a response the live store would
// already have dropped — and the cap still applies (oldest
// first), so restoring more records than the cap holds keeps
// only the newest. A record whose id is already live is
// skipped, never overwritten: the live store wins. A zero
// CreatedAt (a record written without one) counts as now, so
// it is restored rather than dropped on a missing timestamp.
// The creation time of each restored record is its original
// one, so the TTL keeps running from when the response was
// actually served, not from the restart.
func (s *Store) Restore(recs []Record) int {
	if s == nil || len(recs) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	restored := 0
	now := time.Now()
	for _, rec := range recs {
		if rec.ID == "" {
			continue
		}
		if _, ok := s.entries[rec.ID]; ok {
			continue
		}
		created := now
		if rec.CreatedAt != 0 {
			created = time.Unix(rec.CreatedAt, 0)
		}
		if s.ttl > 0 && now.Sub(created) > s.ttl {
			continue
		}
		s.putWithCreatedLocked(rec, created)
		restored++
	}
	return restored
}

// Get returns the stored record for id, and whether it was found.
// An expired entry counts as a miss and is deleted. The returned
// record is a copy: mutating it never mutates the store.
func (s *Store) Get(id string, owner string) (Record, bool) {
	if s == nil || id == "" {
		return Record{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return Record{}, false
	}
	if e.owner != owner {
		return Record{}, false
	}
	if s.ttl > 0 && time.Since(e.created) > s.ttl {
		delete(s.entries, id)
		s.removeFromOrderLocked(id)
		return Record{}, false
	}
	return e.rec, true
}

// Delete removes the stored record for id. It reports whether
// anything was there to delete: deleting an id that never existed
// (or already expired, or already evicted) is false, never a
// silent success — the handler turns it into an honest 404. A
// deleted transcript can never be continued afterwards: the next
// create naming it as previous_response_id answers 404.
func (s *Store) Delete(id string, owner string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return false
	}
	if e.owner != owner {
		return false
	}
	delete(s.entries, id)
	s.removeFromOrderLocked(id)
	return true
}

// Page returns one page of stored records, newest first, plus the
// total live count (before paging). offset skips that many newest
// records; limit caps the page (0 or negative means no page).
// Expired entries are skipped and pruned as they are found, so
// they are never counted in total and never served. Records carry
// their stored response JSON (the list endpoint serves the full
// objects); transcripts are never marshalled (json:"-").
func (s *Store) Page(offset, limit int, owner string) ([]Record, int) {
	if s == nil || limit <= 0 {
		return nil, 0
	}
	if offset < 0 {
		offset = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Collect live records newest first. Expired ids are gathered
	// first and pruned after the scan — pruning inside the scan
	// would shift the order slice under the loop.
	order := append([]string(nil), s.order...)
	live := make([]Record, 0, len(s.entries))
	var expired []string
	for i := len(order) - 1; i >= 0; i-- {
		e, ok := s.entries[order[i]]
		if !ok {
			continue
		}
		if s.ttl > 0 && time.Since(e.created) > s.ttl {
			expired = append(expired, order[i])
			continue
		}
		if e.owner != owner {
			continue
		}
		live = append(live, e.rec)
	}
	for _, id := range expired {
		delete(s.entries, id)
		s.removeFromOrderLocked(id)
	}
	total := len(live)
	if offset >= total {
		return nil, total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return live[offset:end], total
}

// IndexOf returns the newest-first index of id (0 = newest), and
// whether it is live. An expired entry counts as absent and is
// pruned. The list handler uses it to resolve an `after` cursor
// without scanning the whole store twice.
func (s *Store) IndexOf(id string) (int, bool) {
	if s == nil || id == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order := append([]string(nil), s.order...)
	var expired []string
	idx := 0
	for i := len(order) - 1; i >= 0; i-- {
		e, ok := s.entries[order[i]]
		if !ok {
			continue
		}
		if s.ttl > 0 && time.Since(e.created) > s.ttl {
			expired = append(expired, order[i])
			continue
		}
		if order[i] == id {
			for _, eid := range expired {
				delete(s.entries, eid)
				s.removeFromOrderLocked(eid)
			}
			return idx, true
		}
		idx++
	}
	for _, eid := range expired {
		delete(s.entries, eid)
		s.removeFromOrderLocked(eid)
	}
	return 0, false
}

// Recent returns up to n records, most recent first. It never
// returns transcript or response JSON: the dashboard feed shows
// identifiers and counts only.
func (s *Store) Recent(n int) []Record {
	if s == nil || n <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, n)
	for i := len(s.order) - 1; i >= 0 && len(out) < n; i-- {
		e, ok := s.entries[s.order[i]]
		if !ok {
			continue
		}
		if s.ttl > 0 && time.Since(e.created) > s.ttl {
			continue
		}
		rec := e.rec
		rec.TranscriptJSON = nil
		rec.ResponseJSON = nil
		out = append(out, rec)
	}
	return out
}

// Children returns the ids of the stored responses that name id
// as their previous_response_id — the direct branches off that
// response — in creation order. It is the in-memory fallback for
// GET /v1/responses/{id}'s child_response_ids and for the tree
// endpoint when no ledger is wired (the ledger is the durable
// tree index; the store is authoritative only while the process
// lives). Expired entries are skipped, never returned.
func (s *Store) Children(id string) []string {
	if s == nil || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, k := range s.order {
		e, ok := s.entries[k]
		if !ok {
			continue
		}
		if s.ttl > 0 && time.Since(e.created) > s.ttl {
			continue
		}
		if e.rec.PreviousID == id {
			out = append(out, k)
		}
	}
	return out
}

// Stats returns the store's accounting snapshot.
func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Stored:        len(s.entries),
		Created:       s.created,
		Continuations: s.continuations,
		Evicted:       s.evicted,
	}
}

// Len returns the live entry count.
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// evictLocked drops the oldest entries while the store is over
// its cap. The caller holds s.mu.
func (s *Store) evictLocked() {
	if s.maxEntries <= 0 {
		return
	}
	for len(s.entries) > s.maxEntries && len(s.order) > 0 {
		oldest := s.order[0]
		s.order = s.order[1:]
		if _, ok := s.entries[oldest]; ok {
			delete(s.entries, oldest)
			s.evicted++
		}
	}
}

func (s *Store) removeFromOrderLocked(id string) {
	for i, k := range s.order {
		if k == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}
