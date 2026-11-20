// M3 Phase C — process-wide LKGP registry.
//
// The registry wraps a single LKGPStore keyed by the requested
// model (the chain's first attempt model — see chainModel in
// wiring.go) and is the one place the engine records successes and
// the hook reads them from. Recording is in-memory only: RecordSuccessFor takes the store's mutex and
// returns, with no I/O, so it is cheap and non-blocking on the
// request path. Persistence is explicit — the coordinator/server
// lifecycle calls Store().Save / Store().Load — never per request.

package strategy

import (
	"time"

	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// LKGPRegistry is the process-wide last-known-good registry: one
// LKGPStore shared by every chain the process serves, keyed by the
// requested model. The store itself is mutex-guarded, so the registry
// needs no lock of its own; it is never replaced after construction.
type LKGPRegistry struct {
	store *LKGPStore
}

// NewLKGPRegistry returns a registry backed by a fresh, empty store.
func NewLKGPRegistry() *LKGPRegistry {
	return &LKGPRegistry{store: NewLKGPStore()}
}

// DefaultLKGPRegistry is the process-wide registry. The coordinator
// switches the hook in wiring.go to this instance (replacing
// hookState.lkgp) and the engine calls RecordSuccessFor on it after
// each successful attempt.
var DefaultLKGPRegistry = NewLKGPRegistry()

// Store returns the registry's underlying store, so the coordinator
// can call Save/Load on it from the server lifecycle and the hook
// can call OrderByLKGP on it. It is the same store RecordSuccessFor
// and LookupFor use — never a copy.
func (r *LKGPRegistry) Store() *LKGPStore {
	return r.store
}

// RecordSuccessFor records provider/model (and the key/account id,
// when known) as the last-known-good target for chainKey. Cheap and
// non-blocking: in-memory only, no I/O. Empty chainKey or provider
// is ignored, mirroring LKGPStore.RecordSuccess.
func (r *LKGPRegistry) RecordSuccessFor(chainKey, provider, model, keyID string) {
	r.store.RecordSuccess(chainKey, provider, model, keyID)
}

// LatestRecord returns the most recently recorded last-known-good
// record across ALL chain keys, by each record's UpdatedAt — the
// timestamp LKGPStore.RecordSuccess already stamps on every
// record, so no new field and no store redesign was needed.
// Expired records (older than LKGPRecordTTL) are skipped, and
// records with an empty model are skipped (a success that named
// no serving model is not a last-good). (LKGPRecord{}, false)
// when nothing usable was ever recorded. Ties on UpdatedAt keep
// the first record seen (map order); production records carry
// distinct wall-clock times, so a tie only arises for records
// restored together from disk. Safe on a nil registry/store.
func (r *LKGPRegistry) LatestRecord() (LKGPRecord, bool) {
	if r == nil || r.store == nil {
		return LKGPRecord{}, false
	}
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	now := time.Now()
	var best LKGPRecord
	found := false
	for _, rec := range r.store.byChain {
		if rec.Model == "" || !lkgpRecordFresh(rec, now) {
			continue
		}
		if !found || rec.UpdatedAt.After(best.UpdatedAt) {
			best, found = rec, true
		}
	}
	if !found {
		return LKGPRecord{}, false
	}
	return best, true
}

// LatestHealthyRecord returns the most recently recorded
// last-known-good record whose (provider, model) is NOT breaker-OPEN
// or breaker-DEGRADED per signals — the session-affinity record for
// the 16-factor ranking (B7 RT-2 residual). A DEGRADED last-good
// must not hoist above healthy targets via the session-affinity
// boost, so a degraded newest pin falls back to the most recent
// healthy record; with no healthy record it reports missing.
// Expired and model-less records are skipped exactly as in
// LatestRecord. A nil signals func carries no breaker info, so the
// pin stands — the same contract as the LKGP strategy hook with
// nil signals. Safe on a nil registry/store.
func (r *LKGPRegistry) LatestHealthyRecord(signals scoring.SignalsFunc) (LKGPRecord, bool) {
	if r == nil || r.store == nil {
		return LKGPRecord{}, false
	}
	r.store.mu.RLock()
	defer r.store.mu.RUnlock()
	now := time.Now()
	var best LKGPRecord
	found := false
	for _, rec := range r.store.byChain {
		if rec.Model == "" || !lkgpRecordFresh(rec, now) {
			continue
		}
		if signals != nil {
			if sig := signals(rec.Provider, rec.Model); sig.BreakerOpen || sig.BreakerDegraded {
				continue
			}
		}
		if !found || rec.UpdatedAt.After(best.UpdatedAt) {
			best, found = rec, true
		}
	}
	if !found {
		return LKGPRecord{}, false
	}
	return best, true
}

// Latest returns the most recently recorded last-known-good model
// across ALL chain keys (see LatestRecord). ("", false) when
// nothing usable was ever recorded, so the auto/lkgp supplier can
// return "" and the resolver errors honestly instead of inventing
// a model.
func (r *LKGPRegistry) Latest() (string, bool) {
	rec, ok := r.LatestRecord()
	if !ok {
		return "", false
	}
	return rec.Model, true
}

// LookupFor returns the recorded LKGP entry for chainKey, if any.
func (r *LKGPRegistry) LookupFor(chainKey string) (LKGPRecord, bool) {
	return r.store.Lookup(chainKey)
}

// OrderByLKGPFor returns targets with the last-known-good target for
// chainKey first (sticky), the rest in input order — the same
// semantics as LKGPStore.OrderByLKGP, over the shared store.
func (r *LKGPRegistry) OrderByLKGPFor(chainKey string, targets []AffinityTarget) []AffinityTarget {
	return r.store.OrderByLKGP(chainKey, targets)
}
