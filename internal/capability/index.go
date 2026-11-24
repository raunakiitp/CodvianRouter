package capability

import "sync"

// Index is a point-in-time snapshot of the capability matrix with
// O(1) lookup, for callers on a request path (the router's capability
// filter, the API handlers).
//
// Build once at startup (or after a discovery refresh) and refresh
// explicitly — Refresh rebuilds from the same Builder. Reads are
// lock-free-ish (RWMutex) and never touch the network.
type Index struct {
	mu      sync.RWMutex
	builder Builder
	matrix  map[string]ModelCapabilities
}

// NewIndex builds the matrix once.
func NewIndex(b Builder) *Index {
	idx := &Index{builder: b}
	idx.matrix = b.Build()
	return idx
}

// Refresh rebuilds the matrix from the builder's current sources.
// Operators trigger this after a discovery sync; it is the only
// mutation point, so a request either sees the old snapshot or the
// new one, never a half-built map.
func (i *Index) Refresh() {
	if i == nil {
		return
	}
	m := i.builder.Build()
	i.mu.Lock()
	i.matrix = m
	i.mu.Unlock()
}

// Lookup returns one model's capabilities. ok is false when the
// provider/model pair is not in the matrix at all (unknown provider or
// a model no source ever reported) — callers must treat that as
// unknown, never as an empty capability set that happens to pass a
// filter.
func (i *Index) Lookup(providerID, model string) (ModelCapabilities, bool) {
	if i == nil {
		return nil, false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	mc, ok := i.matrix[providerID+"|"+model]
	return mc, ok
}

// Size reports how many provider/model pairs the snapshot holds.
func (i *Index) Size() int {
	if i == nil {
		return 0
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.matrix)
}

// Entries returns a copy of the matrix, for API responses that need
// the whole set at one consistent point in time.
func (i *Index) Entries() map[string]ModelCapabilities {
	if i == nil {
		return nil
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make(map[string]ModelCapabilities, len(i.matrix))
	for k, v := range i.matrix {
		out[k] = v
	}
	return out
}
