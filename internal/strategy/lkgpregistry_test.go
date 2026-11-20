package strategy

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// TestLKGPPersistenceRoundTrip records successes for two chains,
// saves to disk, loads into a fresh store, and checks every field
// survives — including that a saved file is mode 0600 and leaves no
// temp file behind.
func TestLKGPPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lkgp.json")

	src := NewLKGPStore()
	src.RecordSuccess("chain-a", "openai", "gpt-5", "key-1")
	src.RecordSuccess("chain-b", "anthropic", "claude-opus", "")
	if err := src.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat saved file: %v", err)
	}
	// Windows has no Unix permission bits (files report 0666), so
	// the exact mode is only asserted off Windows.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("saved file mode = %o, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir holds %d entries after Save, want 1 (no temp file left): %v", len(entries), entries)
	}

	dst := NewLKGPStore()
	if err := dst.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec, ok := dst.Lookup("chain-a")
	if !ok {
		t.Fatal("chain-a record missing after Load")
	}
	if rec.Provider != "openai" || rec.Model != "gpt-5" || rec.ConnectionID != "key-1" {
		t.Fatalf("chain-a record = %+v, want provider openai / model gpt-5 / key key-1", rec)
	}
	if rec.UpdatedAt.IsZero() || time.Since(rec.UpdatedAt) > time.Hour {
		t.Fatalf("chain-a UpdatedAt = %v, want a recent non-zero time", rec.UpdatedAt)
	}
	rec, ok = dst.Lookup("chain-b")
	if !ok {
		t.Fatal("chain-b record missing after Load")
	}
	if rec.Provider != "anthropic" || rec.Model != "claude-opus" || rec.ConnectionID != "" {
		t.Fatalf("chain-b record = %+v, want provider anthropic / model claude-opus / empty key", rec)
	}
}

// TestLKGPSaveLoadKeepsOrderingBehaviour proves persistence does not
// change ordering semantics: a loaded store still sticks the
// last-good target first, and a store with no record keeps input order.
func TestLKGPSaveLoadKeepsOrderingBehaviour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lkgp.json")
	src := NewLKGPStore()
	src.RecordSuccess("chain-a", "openai", "gpt-5", "")
	if err := src.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	dst := NewLKGPStore()
	if err := dst.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	targets := []AffinityTarget{
		{Provider: "anthropic", Model: "claude-opus"},
		{Provider: "openai", Model: "gpt-5"},
		{Provider: "google", Model: "gemini"},
	}
	ordered := dst.OrderByLKGP("chain-a", targets)
	if len(ordered) != 3 || ordered[0].Provider != "openai" {
		t.Fatalf("OrderByLKGP after Load = %+v, want openai first", ordered)
	}
	// No record for this chain: input order untouched.
	ordered = dst.OrderByLKGP("chain-unknown", targets)
	if ordered[0].Provider != "anthropic" || ordered[1].Provider != "openai" {
		t.Fatalf("OrderByLKGP unknown chain = %+v, want input order", ordered)
	}
}

// TestLKGPLoadMissingFileStartsEmpty: a missing file is tolerated —
// Load returns an error (never crashes) and the store starts empty.
func TestLKGPLoadMissingFileStartsEmpty(t *testing.T) {
	s := NewLKGPStore()
	s.RecordSuccess("chain-a", "openai", "gpt-5", "")
	if err := s.Load(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatal("Load of missing file returned nil error, want an error")
	}
	if _, ok := s.Lookup("chain-a"); ok {
		t.Fatal("store still holds chain-a after failed Load, want empty")
	}
}

// TestLKGPLoadCorruptFileStartsEmpty: a corrupt file is tolerated —
// Load returns an error and the store starts empty, inventing no records.
func TestLKGPLoadCorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lkgp.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s := NewLKGPStore()
	s.RecordSuccess("chain-a", "openai", "gpt-5", "")
	if err := s.Load(path); err == nil {
		t.Fatal("Load of corrupt file returned nil error, want an error")
	}
	if _, ok := s.Lookup("chain-a"); ok {
		t.Fatal("store still holds chain-a after corrupt Load, want empty")
	}
}

// TestLKGPLoadEmptyFileStartsEmpty: an empty (0-byte) file is corrupt
// JSON, so it behaves like the corrupt case.
func TestLKGPLoadEmptyFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lkgp.json")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s := NewLKGPStore()
	if err := s.Load(path); err == nil {
		t.Fatal("Load of empty file returned nil error, want an error")
	}
	if _, ok := s.Lookup("anything"); ok {
		t.Fatal("store non-empty after empty-file Load, want empty")
	}
}

// TestRegistryRecordLookup records through the registry and reads
// back through LookupFor; chains are isolated from each other.
func TestRegistryRecordLookup(t *testing.T) {
	r := NewLKGPRegistry()
	r.RecordSuccessFor("chain-a", "openai", "gpt-5", "key-1")
	r.RecordSuccessFor("chain-b", "anthropic", "claude-opus", "")

	rec, ok := r.LookupFor("chain-a")
	if !ok {
		t.Fatal("chain-a missing from registry")
	}
	if rec.Provider != "openai" || rec.Model != "gpt-5" || rec.ConnectionID != "key-1" {
		t.Fatalf("chain-a = %+v, want openai/gpt-5/key-1", rec)
	}
	rec, ok = r.LookupFor("chain-b")
	if !ok || rec.Provider != "anthropic" {
		t.Fatalf("chain-b = %+v ok=%v, want anthropic", rec, ok)
	}
	if _, ok := r.LookupFor("chain-unknown"); ok {
		t.Fatal("unknown chain returned a record, want none")
	}
}

// TestRegistryIgnoresEmptyKeyOrProvider: empty chain key or provider
// records nothing, mirroring LKGPStore.RecordSuccess.
func TestRegistryIgnoresEmptyKeyOrProvider(t *testing.T) {
	r := NewLKGPRegistry()
	r.RecordSuccessFor("", "openai", "gpt-5", "")
	r.RecordSuccessFor("chain-a", "", "gpt-5", "")
	if _, ok := r.LookupFor(""); ok {
		t.Fatal("empty chain key recorded, want ignored")
	}
	if _, ok := r.LookupFor("chain-a"); ok {
		t.Fatal("empty provider recorded, want ignored")
	}
}

// TestRegistryStoreIsShared: records made through the registry are
// visible through Store(), and vice versa — the registry wraps one
// store, so the coordinator's Save/Load sees the same records.
func TestRegistryStoreIsShared(t *testing.T) {
	r := NewLKGPRegistry()
	if r.Store() == nil {
		t.Fatal("Store() returned nil")
	}
	r.RecordSuccessFor("chain-a", "openai", "gpt-5", "")
	if _, ok := r.Store().Lookup("chain-a"); !ok {
		t.Fatal("record via registry not visible via Store()")
	}
	r.Store().RecordSuccess("chain-b", "google", "gemini", "")
	if _, ok := r.LookupFor("chain-b"); !ok {
		t.Fatal("record via Store() not visible via LookupFor")
	}
}

// TestRegistryLatestReturnsMostRecent (M3 Phase D, D2X): Latest()
// returns the most recently updated model across ALL chain keys,
// by each record's UpdatedAt (set by LKGPStore.RecordSuccess —
// no new field). Re-recording an older chain makes IT the
// latest. A short sleep between records guarantees distinct
// UpdatedAt values (time.Now() resolution, not a behaviour the
// test may assume).
func TestRegistryLatestReturnsMostRecent(t *testing.T) {
	r := NewLKGPRegistry()
	r.RecordSuccessFor("chain-a", "provA", "coder-best", "")
	time.Sleep(2 * time.Millisecond)
	r.RecordSuccessFor("chain-b", "provB", "chat-fast", "")
	model, ok := r.Latest()
	if !ok || model != "chat-fast" {
		t.Fatalf("Latest() = %q, %v; want chat-fast, true (chain-b recorded last)", model, ok)
	}
	// Re-recording chain-a makes it the most recent again.
	time.Sleep(2 * time.Millisecond)
	r.RecordSuccessFor("chain-a", "provA", "coder-cheap", "")
	model, ok = r.Latest()
	if !ok || model != "coder-cheap" {
		t.Fatalf("Latest() after re-record = %q, %v; want coder-cheap, true (chain-a re-recorded last)", model, ok)
	}
}

// TestRegistryLatestEmpty: an empty registry reports ("", false) —
// never an invented model.
func TestRegistryLatestEmpty(t *testing.T) {
	r := NewLKGPRegistry()
	if model, ok := r.Latest(); ok || model != "" {
		t.Fatalf("Latest() on empty registry = %q, %v; want \"\", false", model, ok)
	}
}

// TestRegistryLatestSkipsEmptyModel: a record with no model (a
// success that named no serving model) never becomes the global
// last-good — Latest() reports ("", false) when it is the only
// record, and skips it when a real record exists.
func TestRegistryLatestSkipsEmptyModel(t *testing.T) {
	r := NewLKGPRegistry()
	r.RecordSuccessFor("chain-a", "provA", "", "")
	if model, ok := r.Latest(); ok || model != "" {
		t.Fatalf("Latest() with only an empty-model record = %q, %v; want \"\", false", model, ok)
	}
	time.Sleep(2 * time.Millisecond)
	r.RecordSuccessFor("chain-b", "provB", "chat-fast", "")
	if model, ok := r.Latest(); !ok || model != "chat-fast" {
		t.Fatalf("Latest() = %q, %v; want chat-fast, true (empty-model record skipped)", model, ok)
	}
}

// TestRegistryConcurrentRecordLookup hammers the registry from many
// goroutines; -race must stay clean and every chain must end up with
// a record.
func TestRegistryConcurrentRecordLookup(t *testing.T) {
	r := NewLKGPRegistry()
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				chain := "chain-a"
				if i%2 == 0 {
					chain = "chain-b"
				}
				r.RecordSuccessFor(chain, "provider", "model", "")
				if _, ok := r.LookupFor(chain); !ok {
					t.Errorf("goroutine %d: %s missing after record", g, chain)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if _, ok := r.LookupFor("chain-a"); !ok {
		t.Fatal("chain-a missing after concurrent records")
	}
	if _, ok := r.LookupFor("chain-b"); !ok {
		t.Fatal("chain-b missing after concurrent records")
	}
}

// TestLatestHealthyRecordSkipsDegraded (B7 RT-2 residual): the
// session-affinity record feeding the 16-factor ranking must never
// be a DEGRADED (or OPEN) pair — a degrading last-known-good must
// not hoist above healthy targets via the session-affinity boost
// (main.go rankModels closure). LatestHealthyRecord falls back to
// the most recent HEALTHY record; with no healthy record it
// reports missing.
func TestLatestHealthyRecordSkipsDegraded(t *testing.T) {
	r := NewLKGPRegistry()
	// healthy-prov recorded first (older), slow-prov last (newest).
	r.RecordSuccessFor("chain-a", "healthy-prov", "model-a", "")
	time.Sleep(2 * time.Millisecond) // distinct UpdatedAt stamps
	r.RecordSuccessFor("chain-b", "slow-prov", "model-a", "")
	// Sanity: the unguarded LatestRecord is the degraded pin.
	if rec, ok := r.LatestRecord(); !ok || rec.Provider != "slow-prov" {
		t.Fatalf("LatestRecord = %+v, %v, want slow-prov (the newest pin)", rec, ok)
	}
	degraded := func(provider, model string) scoring.Signals {
		return scoring.Signals{BreakerDegraded: provider == "slow-prov"}
	}
	// DEGRADED newest pin: fall back to the older healthy record.
	if rec, ok := r.LatestHealthyRecord(degraded); !ok || rec.Provider != "healthy-prov" {
		t.Fatalf("LatestHealthyRecord(DEGRADED slow-prov) = %+v, %v, want healthy-prov", rec, ok)
	}
	// OPEN newest pin: same fallback.
	open := func(provider, model string) scoring.Signals {
		return scoring.Signals{BreakerOpen: provider == "slow-prov"}
	}
	if rec, ok := r.LatestHealthyRecord(open); !ok || rec.Provider != "healthy-prov" {
		t.Fatalf("LatestHealthyRecord(OPEN slow-prov) = %+v, %v, want healthy-prov", rec, ok)
	}
	// Both degraded: no healthy record at all.
	allBad := func(provider, model string) scoring.Signals {
		return scoring.Signals{BreakerDegraded: true}
	}
	if rec, ok := r.LatestHealthyRecord(allBad); ok {
		t.Fatalf("LatestHealthyRecord(all DEGRADED) = %+v, want missing", rec)
	}
	// Healthy signals: the newest pin stands.
	healthy := func(provider, model string) scoring.Signals { return scoring.Signals{} }
	if rec, ok := r.LatestHealthyRecord(healthy); !ok || rec.Provider != "slow-prov" {
		t.Fatalf("LatestHealthyRecord(healthy) = %+v, %v, want slow-prov", rec, ok)
	}
	// Nil signals: no breaker info, so the pin stands (same contract
	// as the LKGP strategy hook with a nil signals func).
	if rec, ok := r.LatestHealthyRecord(nil); !ok || rec.Provider != "slow-prov" {
		t.Fatalf("LatestHealthyRecord(nil) = %+v, %v, want slow-prov", rec, ok)
	}
}
