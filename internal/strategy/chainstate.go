// M3 Phase C — chain-keyed stateful-ordering state.
//
// Stateful orderings (round-robin rotation, strict-random deck) must
// not be shared across chains: a rotation advanced by chain A must
// not move chain B's next leader. This file holds one ChainState per
// chain signature string, so each chain gets its own RoundRobin and
// StrictRandomDeck.
//
// State is in-memory only in v1: it is not persisted across
// restarts. A restart rebuilds rotation/deck state from scratch —
// ordering degrades to the first target of each chain until the
// rotation advances again, which is the same behaviour as a fresh
// process today.
//
// Concurrency: ChainStateStore.GetOrCreate is mutex-guarded, and
// RoundRobin / StrictRandomDeck are each internally mutex-guarded.
// The *rand.Rand passed to Deck.Order is NOT goroutine-safe, so the
// caller must serialise draws through one shared rand (as wiring.go's
// hookState does today via its mu) — the deck does not own the rng.

package strategy

import "sync"

// ChainState is the stateful-ordering state for one chain
// signature: its own round-robin rotation and its own
// strict-random deck. One ChainState per chain; never shared
// between chains.
type ChainState struct {
	RR   *RoundRobin
	Deck *StrictRandomDeck
}

// ChainStateStore holds one ChainState per chain signature string.
// The zero value is not usable; use NewChainStateStore.
type ChainStateStore struct {
	mu      sync.Mutex
	byChain map[string]*ChainState
}

// NewChainStateStore returns an empty chain-state store.
func NewChainStateStore() *ChainStateStore {
	return &ChainStateStore{byChain: make(map[string]*ChainState)}
}

// DefaultChainStates is the process-wide chain-state store. The
// coordinator switches the hook in wiring.go to it (replacing the
// process-wide rr/deck in hookState) so each chain rotates and deals
// from its own state.
var DefaultChainStates = NewChainStateStore()

// GetOrCreate returns the ChainState for chainKey, creating a fresh
// one (rotation at the start, empty deck) on first use. Concurrent
// calls for the same key all return the same instance.
func (s *ChainStateStore) GetOrCreate(chainKey string) *ChainState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.byChain[chainKey]
	if !ok {
		st = &ChainState{RR: &RoundRobin{}, Deck: &StrictRandomDeck{}}
		s.byChain[chainKey] = st
	}
	return st
}
