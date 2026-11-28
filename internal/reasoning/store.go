package reasoning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Store is the router's provider replay state: the reasoning of
// each served assistant turn, keyed by the conversation transcript
// up to that turn, so the next turn can restore it even when the
// client replays its history without it (the sector-typical hybrid
// memory/SQLite cache, memory half only — see the limits below).
//
// Scope: the key digests the gateway-key ID (CallerID) plus the
// transcript, so one caller's reasoning can never be replayed
// into another caller's conversation, and the same tool-call id
// under a different session cannot select the entry. The store
// holds reasoning text and opaque signatures only — never prompts
// beyond the transcript digest (the transcript itself is hashed,
// not stored), and nothing in it is ever rendered to a client or
// a dashboard feed: Stats are counts only.
//
// Limits, stated honestly: the store is in-memory only, so a
// process restart loses it (a later turn then misses and the hop
// proceeds without restored reasoning — a miss, never a wrong
// replay). TTL is 2h and the cap is 200 entries, the sector-typical
// defaults; eviction is oldest-created first. A lookup miss is
// not an error: the hop simply carries no restored reasoning.
const (
	// DefaultTTL is how long a captured turn stays replayable.
	DefaultTTL = 2 * time.Hour
	// DefaultMaxEntries caps the in-memory store.
	DefaultMaxEntries = 200
)

// Stats is the store's accounting: counts only, no reasoning
// content, no keys, no transcripts. Entries is the live size;
// Captures the turns ever stored; Hits/Misses the lookups that
// found / did not find a turn; Injected the turns actually
// attached to a follow-up request; Evicted the entries dropped
// by the cap (expired entries are deleted lazily on lookup and
// are not counted here).
type Stats struct {
	Entries  int    `json:"entries"`
	Captures uint64 `json:"captures"`
	Hits     uint64 `json:"hits"`
	Misses   uint64 `json:"misses"`
	Injected uint64 `json:"injected"`
	Evicted  uint64 `json:"evicted"`
}

// ToolCallRef is one tool call inside a transcript turn, for key
// purposes: the id, name and arguments as the client replayed
// them. Two turns that differ only in a tool call's arguments
// are different turns.
type ToolCallRef struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Turn is one message of a transcript for key purposes: role,
// visible text, and tool calls. Reasoning state is deliberately
// NOT part of the key — a turn's key must be the same whether
// its reasoning was captured, injected, or stripped by the
// client, or capture and injection could never meet.
type Turn struct {
	Role      string        `json:"role"`
	Text      string        `json:"text,omitempty"`
	ToolCalls []ToolCallRef `json:"tool_calls,omitempty"`
}

// TranscriptKey digests (callerID, turns) into a stable hex key:
// SHA-256 over the canonical JSON. The caller ID scopes the key
// to one gateway key ("" for anonymous callers — their turns are
// still scoped to the empty caller, never to another key).
func TranscriptKey(callerID string, turns []Turn) string {
	if turns == nil {
		turns = []Turn{}
	}
	payload, err := json.Marshal(struct {
		Caller string `json:"caller"`
		Turns  []Turn `json:"turns"`
	}{Caller: callerID, Turns: turns})
	if err != nil {
		// json.Marshal of this shape cannot fail; if it ever
		// did, fall back to a key that can never match a
		// stored entry rather than a colliding one.
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

type storeEntry struct {
	state   State
	created time.Time
}

// Store is the in-memory replay store. Safe for concurrent use.
type Store struct {
	mu         sync.Mutex
	entries    map[string]storeEntry
	order      []string // creation order, oldest first (eviction)
	ttl        time.Duration
	maxEntries int

	captures uint64
	hits     uint64
	misses   uint64
	injected uint64
	evicted  uint64
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

// Put stores the reasoning of one assistant turn under key. A
// repeat Put for the same key replaces the entry (same turn,
// newer capture) without growing the store.
func (s *Store) Put(key string, st State) {
	if s == nil || key == "" || st.Empty() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[key]; !ok {
		s.entries[key] = storeEntry{state: *st.Clone(), created: time.Now()}
		s.order = append(s.order, key)
		s.captures++
		s.evictLocked()
		return
	}
	s.entries[key] = storeEntry{state: *st.Clone(), created: time.Now()}
	s.captures++
}

// Get returns the stored reasoning for key, and whether it was
// found. An expired entry counts as a miss and is deleted. The
// returned state is a clone: mutating it never mutates the store.
func (s *Store) Get(key string) (State, bool) {
	if s == nil || key == "" {
		return State{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		s.misses++
		return State{}, false
	}
	if s.ttl > 0 && time.Since(e.created) > s.ttl {
		delete(s.entries, key)
		s.removeFromOrderLocked(key)
		s.misses++
		return State{}, false
	}
	s.hits++
	return *e.state.Clone(), true
}

// NoteInjected records one successful injection: a stored turn
// actually attached to a follow-up request. It is separate from
// Get's hit count, so the stats can tell "looked up and found"
// apart from "found and used".
func (s *Store) NoteInjected() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.injected++
}

// Stats returns the store's accounting snapshot.
func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Entries:  len(s.entries),
		Captures: s.captures,
		Hits:     s.hits,
		Misses:   s.misses,
		Injected: s.injected,
		Evicted:  s.evicted,
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

func (s *Store) removeFromOrderLocked(key string) {
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}
