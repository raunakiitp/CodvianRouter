// Declarative per-model fallback chains — engine store (G11 Area 2,
// PARITY.md Area 2 row "Declarative per-model fallback chains API",
// shared with §14b).
//
// OmniRoute's evidence is src/domain/fallbackPolicy.ts: a named
// chain per model (ordered FallbackEntry{provider, priority,
// enabled}), persisted in SQLite, resolved with already-tried
// providers excluded. Ours is the same shape in our own code: an
// in-memory per-model chain on the engine, consulted through
// FallbackChainFor (which returns Attempts the chain machinery can
// walk) and mutated through Set/Delete/List. The HTTP face lives
// in internal/server/fallback_chains_api.go; the route registration
// is the coordinator's (server.go is Wave 3's).
//
// Backing: the chains live on the engine next to the other runtime
// chain state (runtimeChains), NOT in a second store — the CRUD
// API mutates the same chains the router resolves. Persistence is
// deliberately in-memory only in this slice (like the runtime
// strategy/cooldown overrides): a persist callback wired to the
// config file is the coordinator's follow-up, and List reports
// the live state honestly either way. FallbackChainFor is the
// seam the coordinator wires into chain resolution (chainFor):
// until then the store is reachable and testable, but no request
// consults it yet — recorded as remaining work in the G11 report,
// not claimed as wired.
package router

import (
	"errors"
	"fmt"
	"sort"
)

// FallbackEntry is one step of a declarative per-model fallback
// chain: a provider, in priority order (lower first). Model is
// the model pin for this step ("" = the request-time model, as
// for any routing-group step). Enabled=false steps are stored but
// never served — an operator disables a step without deleting it.
type FallbackEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Priority int    `json:"priority"`
	Enabled  bool   `json:"enabled"`
}

// SetFallbackChain replaces the declarative fallback chain for one
// model (create or update — the same call; there is no separate
// update path, mirroring OmniRoute's registerFallback). Validation
// failures name the field: an empty model, an empty chain, an
// entry with no provider, or a duplicated provider in one chain.
// Entries are stored priority-sorted (stable: equal priorities
// keep their submitted order). Disabled entries are kept, not
// dropped.
func (e *Engine) SetFallbackChain(model string, entries []FallbackEntry) error {
	if model == "" {
		return errors.New("model: empty — a fallback chain needs a model to attach to")
	}
	if len(entries) == 0 {
		return errors.New("entries: empty — a fallback chain needs at least one provider")
	}
	seen := make(map[string]bool, len(entries))
	for _, en := range entries {
		if en.Provider == "" {
			return errors.New("entries: a fallback entry needs a provider")
		}
		if seen[en.Provider] {
			return fmt.Errorf("entries: provider %q appears twice in the chain", en.Provider)
		}
		seen[en.Provider] = true
	}
	stored := make([]FallbackEntry, len(entries))
	copy(stored, entries)
	sort.SliceStable(stored, func(i, j int) bool { return stored[i].Priority < stored[j].Priority })
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fallbackChains[model] = stored
	return nil
}

// ListFallbackChains returns every stored chain, by model. The
// returned slices are copies: mutating them never touches the
// engine's state.
func (e *Engine) ListFallbackChains() map[string][]FallbackEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string][]FallbackEntry, len(e.fallbackChains))
	for model, entries := range e.fallbackChains {
		cp := make([]FallbackEntry, len(entries))
		copy(cp, entries)
		out[model] = cp
	}
	return out
}

// FallbackChain returns the stored chain for one model (a copy),
// and whether one exists at all.
func (e *Engine) FallbackChain(model string) ([]FallbackEntry, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entries, ok := e.fallbackChains[model]
	if !ok {
		return nil, false
	}
	cp := make([]FallbackEntry, len(entries))
	copy(cp, entries)
	return cp, true
}

// DeleteFallbackChain removes the chain for one model. It reports
// whether a chain existed (false = nothing to delete).
func (e *Engine) DeleteFallbackChain(model string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.fallbackChains[model]; !ok {
		return false
	}
	delete(e.fallbackChains, model)
	return true
}

// FallbackChainFor resolves the stored chain for one model into
// Attempts in priority order, skipping disabled entries. An entry
// with no model pin serves the requested model. An unknown model,
// or a chain with no enabled entry, resolves to no attempts —
// never a guessed provider.
func (e *Engine) FallbackChainFor(model string) []Attempt {
	e.mu.Lock()
	defer e.mu.Unlock()
	entries := e.fallbackChains[model]
	out := make([]Attempt, 0, len(entries))
	for _, en := range entries {
		if !en.Enabled {
			continue
		}
		m := en.Model
		if m == "" {
			m = model
		}
		out = append(out, Attempt{ProviderID: en.Provider, Model: m})
	}
	return out
}
