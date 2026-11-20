// Breaker persistence (G11 Area 2, parity tracker Area 2 row "Breaker
// states CLOSED/DEGRADED/OPEN/HALF_OPEN, persisted, lazy recovery").
//
// the reference router persists its breaker states; ours was in-memory only, so
// a restart forgot every trip and a flapping provider got a clean
// slate. This file is the MINIMAL durable store the gap inventory
// scoped: the consecutive-breaker state (consecutive failures, trip
// count, opened-at, open duration, last reason) per provider,
// persisted as one JSON file in the data dir on every breaker state
// change and reloaded at engine start. No daemon, no migration, no
// new dependency.
//
// Scope notes (honest limits, not oversights):
//   - The store is per PROVIDER, because the breaker itself is
//     per-provider (e.breakers is keyed by provider ID). The task
//     brief's "(provider,key)" framing does not match the breaker
//     implementation — per-key state is the cooldown's job
//     (cooldown.go), not the breaker's.
//   - The provider-level failure-WINDOW breaker
//     (providerbreaker.go) is NOT persisted: it is a time window
//     of recent failures, and a restart legitimately starts a
//     fresh window — persisting timestamps would only re-arm a
//     window whose failures predate the restart.
//   - The half-open probe-in-flight flag is NOT persisted: it is
//     transient by definition. A restored OPEN breaker whose open
//     window has already elapsed is probe-eligible on the next
//     request (breakerAllow claims the probe), which IS the lazy
//     recovery the reference router describes — a restart never wedges a
//     breaker open, and never grants a free pass either.
//   - Persistence is opt-in: Config.BreakerStorePath == "" means
//     no file, no writes, exactly the old behaviour. The write
//     runs synchronously after the state change (atomic
//     tmp+rename, so a crash mid-write never leaves a torn file),
//     and a write failure is dropped — durability only, never a
//     request failure (the RateLimitStore pattern, M5.13).
//   - A success only writes when the provider actually had
//     breaker state to clear, so a healthy provider's steady
//     stream of successes costs no I/O at all.
package router

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// breakerStoreVersion is the on-disk schema version. A file with
// any other version is ignored (treated as absent) rather than
// misread — the store is durability only, and a version skew must
// never fail engine start.
const breakerStoreVersion = 1

// breakerStoreRecord is one provider's durable breaker state.
type breakerStoreRecord struct {
	ConsecutiveFailures int           `json:"consecutive_failures"`
	Trips               int           `json:"trips"`
	OpenedAt            time.Time     `json:"opened_at"`
	OpenFor             time.Duration `json:"open_for_ns"`
	LastReason          string        `json:"last_reason,omitempty"`
	// LastFailureAt is when the most recent breaker-counted
	// failure happened (parity tracker:524 lazy recovery). It is
	// persisted so a restart preserves the decay clock: a
	// provider that failed recently stays DEGRADED after a
	// restart, and one whose last failure predates the decay
	// hold still decays on read. Absent (zero) in files written
	// before this field existed — read as "unknown", never
	// treated as stale.
	LastFailureAt time.Time `json:"last_failure_at,omitempty"`
}

// breakerStoreFile is the whole on-disk document.
type breakerStoreFile struct {
	Version  int                           `json:"version"`
	Breakers map[string]breakerStoreRecord `json:"breakers"`
}

// saveBreakerStore snapshots the live breaker table and writes it
// to Config.BreakerStorePath. No-op when no path is configured.
// The whole save — snapshot AND write — runs under
// breakerStoreMu, so two concurrent saves cannot interleave: the
// snapshot order is the write order, and the file always ends
// holding the newest snapshot, never a stale one renamed over a
// newer one (the lost-update race
// TestBreakerConcurrentResetAndFailurePersistsLatest guards).
// The snapshot itself is taken under e.mu and the file write
// happens after e.mu is released, so disk I/O never blocks the
// request path's mutex. Errors are dropped (durability only).
func (e *Engine) saveBreakerStore() {
	path := e.cfg.BreakerStorePath
	if path == "" {
		return
	}
	e.breakerStoreMu.Lock()
	defer e.breakerStoreMu.Unlock()
	e.mu.Lock()
	doc := breakerStoreFile{
		Version:  breakerStoreVersion,
		Breakers: make(map[string]breakerStoreRecord, len(e.breakers)),
	}
	for id, st := range e.breakers {
		if st == nil {
			continue
		}
		doc.Breakers[id] = breakerStoreRecord{
			ConsecutiveFailures: st.consecutiveFailures,
			Trips:               st.trips,
			OpenedAt:            st.openedAt,
			OpenFor:             st.openFor,
			LastReason:          st.lastReason,
			LastFailureAt:       st.lastFailureAt,
		}
	}
	e.mu.Unlock()
	raw, err := json.Marshal(doc)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = renameBreakerStoreFile(tmp, path)
}

// renameBreakerStoreFile moves tmp over path, retrying briefly: on
// Windows the rename can fail transiently with "Access is denied"
// when a scanner (Defender/indexer) briefly holds the existing file
// open, even though the temp file is closed and nothing else
// in-process holds the destination (TestBreakerStaleDegraded-
// PersistsAcrossReload hit this: one of five failure saves lost
// its rename, so the reload saw 4 failures instead of 5). The
// retry is harmless on Unix, where the first rename succeeds.
// Mirrors config.Save's renameFile (T3-PRICING) and the tray PID
// file's renamePIDFile. Errors stay dropped (durability only).
func renameBreakerStoreFile(tmp, path string) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// loadBreakerStore restores breaker state from
// Config.BreakerStorePath into the live table. Called once from
// NewEngine. A missing, unreadable, or wrong-version file is
// treated as "no prior state" — the engine starts clean rather
// than failing to start over a durability file.
func (e *Engine) loadBreakerStore() {
	path := e.cfg.BreakerStorePath
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc breakerStoreFile
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Version != breakerStoreVersion {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, rec := range doc.Breakers {
		if id == "" {
			continue
		}
		e.breakers[id] = &breakerState{
			consecutiveFailures: rec.ConsecutiveFailures,
			trips:               rec.Trips,
			openedAt:            rec.OpenedAt,
			openFor:             rec.OpenFor,
			lastReason:          rec.LastReason,
			lastFailureAt:       rec.LastFailureAt,
		}
	}
}
