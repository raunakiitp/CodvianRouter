// M6.2 — flush API for the semantic layer.
//
// LocalSemanticCache previously had no way to drop entries: the
// exact layer has FlushAll (backing POST /admin/cache/flush) and
// the persistent exact layer has FlushTenant, but a semantic entry
// could only leave by expiry or LRU eviction. These methods close
// that gap with the smallest correct flush: every dropped slot
// goes through freeSlotLocked, so the dedup map, the inverted
// index postings, the recency order, the size count and the M6.1
// byte accounting all stay consistent, and stale postings are
// reclaimed in bulk by maybeRebuild afterwards.
//
// Scope note: the semantic layer records a MODEL per entry, never
// a tenant (see localSemanticEntry.model, CHECKLIST 187), so
// there is no FlushTenant here — inventing one would need a
// tenant field the layer does not have. FlushModel is the
// layer's real scope: it drops exactly one model's entries.
package cache

// FlushAll removes every entry and returns how many were removed.
func (c *LocalSemanticCache) FlushAll() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	slots := make([]int, 0, c.size)
	for slot, e := range c.slots {
		if e != nil {
			slots = append(slots, slot)
		}
	}
	for _, slot := range slots {
		c.freeSlotLocked(slot)
	}
	c.maybeRebuild()
	return len(slots)
}

// FlushModel removes every entry stored under model (the model
// scope recorded at store time, exact case-sensitive match) and
// returns how many were removed. Entries under other models are
// untouched.
func (c *LocalSemanticCache) FlushModel(model string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	slots := make([]int, 0, c.size)
	for slot, e := range c.slots {
		if e != nil && e.model == model {
			slots = append(slots, slot)
		}
	}
	for _, slot := range slots {
		c.freeSlotLocked(slot)
	}
	c.maybeRebuild()
	return len(slots)
}
