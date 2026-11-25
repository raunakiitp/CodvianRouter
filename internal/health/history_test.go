package health

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// historyFileSamples reads the persisted SNAPSHOT back as raw JSON
// for assertions about what is actually in the snapshot file (not
// just in memory). It only sees samples up to the last compaction:
// newer probes live in the journal until then — use
// historyMergedSamples for the full persisted ring.
func historyFileSamples(t *testing.T, dir, providerID string) []ProbeSample {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, HistoryFileName))
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", HistoryFileName, err)
	}
	var hf historyFile
	if err := json.Unmarshal(raw, &hf); err != nil {
		t.Fatalf("Unmarshal persisted history: %v", err)
	}
	if hf.Version != 1 {
		t.Fatalf("persisted version = %d, want 1", hf.Version)
	}
	return hf.Providers[providerID]
}

// historyMergedSamples reads the full persisted ring the way a
// restart sees it: the snapshot plus the journal lines newer than
// it, pruned to the cap. It loads a fresh checker on the same
// data dir (read-only — a load never writes except for a torn
// journal-tail repair) and returns that checker's in-memory ring
// for the provider.
func historyMergedSamples(t *testing.T, dir, providerID string) []ProbeSample {
	t.Helper()
	c := NewWithDataDir(nil, nil, dir)
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := append([]ProbeSample(nil), c.history[providerID]...)
	return cp
}

func TestHistoryPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// "First run": record real probe outcomes, then drop the checker.
	c1 := NewWithDataDir(nil, nil, dir)
	seedHistory(c1, "p1", now.Add(-30*time.Minute), true, 40, 10)
	seedHistory(c1, "p1", now.Add(-5*time.Minute), false, 5, 10)
	// 20 records is under the 128-line compaction interval, so
	// nothing has been snapshotted yet: the samples live in the
	// journal, and the merged persisted ring still holds all 20.
	if got := historyMergedSamples(t, dir, "p1"); len(got) != 20 {
		t.Fatalf("persisted samples = %d, want 20", len(got))
	}
	// The journal file must not be world-readable (same 0600 as
	// the snapshot and the config and vault files). Windows has no
	// Unix permission bits (files report 0666), so the exact mode
	// is only asserted off Windows.
	if info, err := os.Stat(filepath.Join(dir, HistoryJournalFileName)); err != nil {
		t.Fatalf("Stat journal file: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("journal file perms = %o, want 0600", info.Mode().Perm())
	}

	// "Restart": a fresh checker on the same data dir restores them.
	c2 := NewWithDataDir(nil, nil, dir)
	sum := c2.historyAt("p1", now)
	if sum.Samples != 20 {
		t.Fatalf("restored Samples = %d, want 20", sum.Samples)
	}
	if sum.Uptime24h == nil || *sum.Uptime24h != 50 {
		t.Errorf("restored Uptime24h = %v, want 50", sum.Uptime24h)
	}
	if sum.P50Ms == nil || *sum.P50Ms != 40 {
		t.Errorf("restored P50Ms = %v, want 40 (successful probes only)", sum.P50Ms)
	}
	// No leftover temp files from the atomic writes.
	if temps, _ := filepath.Glob(filepath.Join(dir, ".health-history-*.tmp")); len(temps) != 0 {
		t.Errorf("leftover temp files: %v", temps)
	}
}

func TestHistoryMissingFileIsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	c := NewWithDataDir(nil, nil, dir)
	if sum := c.History("p1"); sum.Samples != 0 {
		t.Fatalf("Samples = %d, want 0 for a fresh data dir", sum.Samples)
	}
	// The first probe creates the journal (the snapshot appears
	// only at the first compaction, after 128 lines).
	c.record(Status{ProviderID: "p1", OK: true, LatencyMs: 12, CheckedAt: time.Now().UTC()}, nil)
	if got := historyMergedSamples(t, dir, "p1"); len(got) != 1 {
		t.Fatalf("persisted samples = %d, want 1 after the first probe", len(got))
	}
}

func TestHistoryLoadPrunesCapAndRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// Hand-build an over-cap, partly stale file: 5 samples older
	// than 48 h plus 3,000 recent ones.
	providers := map[string][]ProbeSample{}
	var samples []ProbeSample
	for i := 0; i < 5; i++ {
		samples = append(samples, ProbeSample{At: now.Add(-49 * time.Hour), OK: true, LatencyMs: 1})
	}
	for i := 0; i < 3000; i++ {
		samples = append(samples, ProbeSample{At: now.Add(-time.Duration(3000-i) * time.Second), OK: i%2 == 0, LatencyMs: int64(i % 100)})
	}
	providers["p1"] = samples
	raw, err := json.Marshal(historyFile{Version: 1, Providers: providers})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, HistoryFileName), raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c := NewWithDataDir(nil, nil, dir)
	sum := c.historyAt("p1", now)
	if sum.Samples != MaxHistorySamples {
		t.Fatalf("loaded Samples = %d, want capped at %d", sum.Samples, MaxHistorySamples)
	}
	// The stale 49 h samples must be gone, and the oldest kept
	// sample must be within retention.
	c.mu.Lock()
	kept := append([]ProbeSample(nil), c.history["p1"]...)
	c.mu.Unlock()
	for _, s := range kept {
		if s.At.Before(now.Add(-HistoryRetention)) {
			t.Fatalf("kept a stale sample from %v", s.At)
		}
	}
	if kept[0].At.Before(now.Add(-time.Hour)) {
		t.Errorf("oldest kept sample = %v, want within the last hour (newest 2880 of 3000 recent)", kept[0].At)
	}
}

func TestHistoryPrunesOnWrite(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	c := NewWithDataDir(nil, nil, dir)
	// Timing pin (CHECKLIST 206): before the tiered-persistence
	// fix this loop did 2,980 whole-file fsync rewrites and took
	// 128 s (baseline 2026-11-18, Go 1.27.4) — over the 10 min
	// test timeout in the 189-190 gate. It must now complete in
	// seconds; the 60 s bound leaves wide headroom for a loaded
	// VM while still catching a return to per-record fsync.
	start := time.Now()
	for i := 0; i < MaxHistorySamples+100; i++ {
		c.record(Status{ProviderID: "p1", OK: true, LatencyMs: 1, CheckedAt: now.Add(time.Duration(i) * time.Millisecond)}, nil)
	}
	elapsed := time.Since(start)
	t.Logf("2,980 records persisted in %s (baseline before the fix: 128s)", elapsed)
	if elapsed > 60*time.Second {
		t.Errorf("persisting 2,980 records took %s, want < 60s (per-record fsync regression?)", elapsed)
	}
	if got := historyFileSamples(t, dir, "p1"); len(got) != MaxHistorySamples {
		t.Errorf("persisted samples = %d, want capped at %d", len(got), MaxHistorySamples)
	}
}

// TestHistoryCheapPersistsStayComplete pins the tiered-persistence
// semantics (CHECKLIST 206): a cheap (non-fsync'd) per-record
// persist still writes a COMPLETE valid snapshot — the file after
// every record holds the whole ring, not just the newest sample —
// and the backup still holds the previous snapshot (a strict
// prefix of the primary), so a corrupt primary still loses
// nothing. Prune semantics are unchanged: the cap applies on
// every write, in memory and on disk.
// TestHistoryJournalCompactionKeepsCompleteRing pins the
// append + periodic compaction mechanics (CHECKLIST 206): 300
// records cross the 128-line compaction interval twice, so the
// snapshot holds the 256-sample ring from the second compaction,
// the backup the SAME 256-sample ring (it is a replica, not the
// previous snapshot), and the journal the 44 lines since — and
// the merged persisted ring is all 300, with the snapshot a
// strict prefix of the merged ring. A restart restores all 300:
// nothing is lost between compactions. Prune semantics are
// unchanged: the cap still applies on every compaction, in memory
// and on disk.
func TestHistoryJournalCompactionKeepsCompleteRing(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	c := NewWithDataDir(nil, nil, dir)
	const n = 300 // under the 2,880 cap, over the 128-line compaction interval twice
	for i := 0; i < n; i++ {
		c.record(Status{ProviderID: "p1", OK: true, LatencyMs: 1, CheckedAt: now.Add(time.Duration(i) * time.Millisecond)}, nil)
	}
	primary := historyFileSamples(t, dir, "p1")
	if len(primary) != 256 {
		t.Fatalf("primary samples = %d, want 256 (snapshot from the compaction at line 256)", len(primary))
	}
	backup := historyBackupSamples(t, dir, "p1")
	if len(backup) != 256 {
		t.Fatalf("backup samples = %d, want 256 (replica of the snapshot from the compaction at line 256)", len(backup))
	}
	raw, err := os.ReadFile(filepath.Join(dir, HistoryJournalFileName))
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", HistoryJournalFileName, err)
	}
	journal := 0
	for _, line := range splitJournalLines(raw) {
		if len(line) != 0 {
			journal++
		}
	}
	if journal != 44 {
		t.Fatalf("journal lines = %d, want 44 (lines 257..300, since the last compaction)", journal)
	}
	// Windows has no Unix permission bits (files report 0666), so
	// the exact mode is only asserted off Windows.
	if info, err := os.Stat(filepath.Join(dir, HistoryJournalFileName)); err != nil {
		t.Fatalf("Stat journal: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("journal perms = %o, want 0600", info.Mode().Perm())
	}
	merged := historyMergedSamples(t, dir, "p1")
	if len(merged) != n {
		t.Fatalf("merged samples = %d, want %d (snapshot + journal must hold the whole ring)", len(merged), n)
	}
	for i, s := range backup {
		if !s.At.Equal(primary[i].At) || s.LatencyMs != primary[i].LatencyMs {
			t.Fatalf("backup sample %d = %+v, want primary sample %d = %+v (backup must be a replica of the snapshot)", i, s, i, primary[i])
		}
	}
	for i, s := range primary {
		if !s.At.Equal(merged[i].At) || s.LatencyMs != merged[i].LatencyMs {
			t.Fatalf("primary sample %d = %+v, want merged sample %d = %+v (primary must be a prefix of the merged ring)", i, s, i, merged[i])
		}
	}
	// Restart on the same data dir restores the full ring.
	c2 := NewWithDataDir(nil, nil, dir)
	if sum := c2.History("p1"); sum.Samples != n {
		t.Errorf("restored Samples = %d, want %d", sum.Samples, n)
	}
	// A torn journal tail (a partial line with no trailing
	// newline, as a crash mid-append leaves) is skipped, never
	// fatal — and the load repairs it, so the next append starts
	// on a fresh line and survives the restart after that.
	journalPath := filepath.Join(dir, HistoryJournalFileName)
	f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile journal for append: %v", err)
	}
	if _, err := f.WriteString(`{"seq":9999,"provider":"p1","at":`); err != nil {
		t.Fatalf("WriteString torn tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close journal: %v", err)
	}
	c3 := NewWithDataDir(nil, nil, dir)
	if sum := c3.History("p1"); sum.Samples != n {
		t.Errorf("restored Samples after torn journal tail = %d, want %d", sum.Samples, n)
	}
	c3.record(Status{ProviderID: "p1", OK: true, LatencyMs: 2, CheckedAt: now.Add(time.Duration(n) * time.Millisecond)}, nil)
	c4 := NewWithDataDir(nil, nil, dir)
	if sum := c4.History("p1"); sum.Samples != n+1 {
		t.Errorf("restored Samples after a probe past the repaired tail = %d, want %d", sum.Samples, n+1)
	}
}

func TestHistoryCorruptFileFallsBackToEmpty(t *testing.T) {
	cases := map[string]string{
		"garbage":         `not json at all {{{`,
		"truncated":       `{"version":1,"providers":{"p1":[{"at":"2026-11-17T10:00:00Z"`,
		"wrong type":      `{"version":1,"providers":{"p1":"nope"}}`,
		"bad version":     `{"version":99,"providers":{}}`,
		"bad sample time": `{"version":1,"providers":{"p1":[{"at":"not-a-time","ok":true,"latency_ms":1}]}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, HistoryFileName), []byte(content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			// Must not panic or crash startup: empty history, honestly.
			c := NewWithDataDir(nil, nil, dir)
			sum := c.History("p1")
			if sum.Samples != 0 {
				t.Fatalf("Samples = %d, want 0 after a corrupt file", sum.Samples)
			}
			if sum.Uptime24h != nil || sum.P50Ms != nil {
				t.Errorf("uptime/latency = %v/%v, want nil (no data)", sum.Uptime24h, sum.P50Ms)
			}
			// Recovery: the next probe records in memory and in
			// the journal; the merged persisted ring holds it
			// (the corrupt snapshot stays corrupt on disk until
			// the next compaction rewrites it).
			c.record(Status{ProviderID: "p1", OK: true, LatencyMs: 7, CheckedAt: time.Now().UTC()}, nil)
			if got := historyMergedSamples(t, dir, "p1"); len(got) != 1 || got[0].LatencyMs != 7 {
				t.Fatalf("persisted after recovery = %+v, want 1 sample at 7ms", got)
			}
		})
	}
}

func TestHistoryEmptyDataDirDisablesPersistence(t *testing.T) {
	c := NewWithDataDir(nil, nil, "")
	if c.HistoryPath() != "" {
		t.Fatalf("HistoryPath() = %q, want empty", c.HistoryPath())
	}
	c.record(Status{ProviderID: "p1", OK: true, LatencyMs: 1, CheckedAt: time.Now().UTC()}, nil)
	if sum := c.History("p1"); sum.Samples != 1 {
		t.Errorf("Samples = %d, want 1 in memory without persistence", sum.Samples)
	}
	// New() behaves the same: in-memory only.
	if New(nil, nil).HistoryPath() != "" {
		t.Error("New() checker has a history path, want in-memory only")
	}
}

func TestHistoryPersistsUnderConcurrentChecks(t *testing.T) {
	dir := t.TempDir()
	reg := provider.NewRegistry(nil, provider.WithRegistryAllowPrivateDestinations(true))
	id := addProvider(t, reg, "sk-conc")
	c := NewWithDataDir(reg, fakeFactory(map[string]*fakeClient{"sk-conc": {}}), dir)
	// Two concurrent CheckAll rounds: every probe must land in
	// memory AND on disk, none lost to an interleaved write.
	c.CheckAll(context.Background())
	c.CheckAll(context.Background())
	sum := c.History(id)
	if sum.Samples != 2 {
		t.Fatalf("Samples = %d, want 2 after two checks", sum.Samples)
	}
	if got := historyMergedSamples(t, dir, id); len(got) != 2 {
		t.Fatalf("persisted samples = %d, want 2 (no lost update)", len(got))
	}
	// A restart sees both.
	if sum2 := NewWithDataDir(reg, fakeFactory(map[string]*fakeClient{"sk-conc": {}}), dir).History(id); sum2.Samples != 2 {
		t.Errorf("restored Samples = %d, want 2", sum2.Samples)
	}
}

// historyBackupSamples reads the .bak file back as raw JSON.
func historyBackupSamples(t *testing.T, dir, providerID string) []ProbeSample {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, HistoryFileName+".bak"))
	if err != nil {
		t.Fatalf("ReadFile(%s.bak): %v", HistoryFileName, err)
	}
	var hf historyFile
	if err := json.Unmarshal(raw, &hf); err != nil {
		t.Fatalf("Unmarshal backup history: %v", err)
	}
	return hf.Providers[providerID]
}

func TestHistoryBackupWrittenBeforeOverwrite(t *testing.T) {
	dir := t.TempDir()
	c := NewWithDataDir(nil, nil, dir)
	now := time.Now().UTC()
	// The backup is a REPLICA of the latest snapshot, written at
	// a compaction (not a copy of the previous ring): 130 records
	// cross the first compaction (line 128), so the primary and
	// the backup both hold the same 128-sample snapshot.
	for i := 0; i < 130; i++ {
		c.record(Status{ProviderID: "p1", OK: true, LatencyMs: int64(i + 1), CheckedAt: now.Add(time.Duration(i) * time.Second)}, nil)
	}
	if got := historyBackupSamples(t, dir, "p1"); len(got) != 128 || got[127].LatencyMs != 128 {
		t.Errorf("backup = %d samples (last latency %v), want the 128-sample replica of the first compaction (last latency 128)", len(got), lastLatency(got))
	}
	if got := historyFileSamples(t, dir, "p1"); len(got) != 128 || got[127].LatencyMs != 128 {
		t.Errorf("primary = %d samples (last latency %v), want the same 128-sample snapshot as the backup", len(got), lastLatency(got))
	}
	// 130 more records cross the second compaction (line 256):
	// both copies now hold the 256-sample snapshot.
	for i := 130; i < 260; i++ {
		c.record(Status{ProviderID: "p1", OK: true, LatencyMs: int64(i + 1), CheckedAt: now.Add(time.Duration(i) * time.Second)}, nil)
	}
	if got := historyBackupSamples(t, dir, "p1"); len(got) != 256 || got[255].LatencyMs != 256 {
		t.Errorf("backup = %d samples (last latency %v), want the 256-sample replica (last latency 256)", len(got), lastLatency(got))
	}
	if got := historyFileSamples(t, dir, "p1"); len(got) != 256 || got[255].LatencyMs != 256 {
		t.Errorf("primary = %d samples (last latency %v), want the 256-sample compacted snapshot (last latency 256)", len(got), lastLatency(got))
	}
	if got := historyMergedSamples(t, dir, "p1"); len(got) != 260 {
		t.Errorf("merged samples = %d, want 260", len(got))
	}
	// The backup is 0600 like the primary. Windows has no Unix
	// permission bits (files report 0666), so the exact mode is
	// only asserted off Windows.
	if info, err := os.Stat(filepath.Join(dir, HistoryFileName+".bak")); err != nil {
		t.Fatalf("Stat backup: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("backup perms = %o, want 0600", info.Mode().Perm())
	}
}

func lastLatency(samples []ProbeSample) int64 {
	if len(samples) == 0 {
		return -1
	}
	return samples[len(samples)-1].LatencyMs
}

func TestHistoryCorruptPrimaryRestoresFromBackup(t *testing.T) {
	dir := t.TempDir()
	c1 := NewWithDataDir(nil, nil, dir)
	now := time.Now().UTC()
	// 260 records: snapshot 256, replica backup 256, journal 4 lines.
	for i := 0; i < 260; i++ {
		c1.record(Status{ProviderID: "p1", OK: true, LatencyMs: 40, CheckedAt: now.Add(time.Duration(i) * time.Second)}, nil)
	}
	if got := historyMergedSamples(t, dir, "p1"); len(got) != 260 {
		t.Fatalf("persisted = %d, want 260", len(got))
	}
	// Corrupt the primary out from under the router (disk damage,
	// a bad edit, a crashed write elsewhere).
	if err := os.WriteFile(filepath.Join(dir, HistoryFileName), []byte(`not json at all {{{`), 0o600); err != nil {
		t.Fatalf("WriteFile corrupt: %v", err)
	}
	// Restart: the replica backup (256 samples) plus the journal
	// lines it does not cover (257..260) restore the whole ring
	// — a corrupt primary loses nothing, not even the samples
	// the backup predates (there are none: the replica holds
	// the same snapshot the primary held).
	c2 := NewWithDataDir(nil, nil, dir)
	if sum := c2.History("p1"); sum.Samples != 260 {
		t.Errorf("restored Samples = %d, want 260 from the backup + journal", sum.Samples)
	}
	if sum := c2.History("p1"); sum.P50Ms == nil || *sum.P50Ms != 40 {
		t.Errorf("restored P50Ms = %v, want 40", sum.P50Ms)
	}
	// With the journal deleted too, only the replica is left: the
	// restart restores exactly its 256 samples.
	if err := os.Remove(filepath.Join(dir, HistoryJournalFileName)); err != nil {
		t.Fatalf("Remove journal: %v", err)
	}
	c3 := NewWithDataDir(nil, nil, dir)
	if sum := c3.History("p1"); sum.Samples != 256 {
		t.Errorf("restored Samples without the journal = %d, want 256 from the backup replica alone", sum.Samples)
	}
}

func TestHistoryCorruptPrimaryAndBackupStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, HistoryFileName), []byte(`garbage`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, HistoryFileName+".bak"), []byte(`also garbage`), 0o600); err != nil {
		t.Fatalf("WriteFile backup: %v", err)
	}
	c := NewWithDataDir(nil, nil, dir)
	if sum := c.History("p1"); sum.Samples != 0 {
		t.Errorf("Samples = %d, want 0 when both copies are corrupt", sum.Samples)
	}
}

func TestHistoryCorruptPrimaryHealsBackupOnCompaction(t *testing.T) {
	dir := t.TempDir()
	c1 := NewWithDataDir(nil, nil, dir)
	now := time.Now().UTC()
	// 260 records with distinct latencies: snapshot 256, replica
	// backup 256 (latencies 1..256), journal 4 lines (257..260).
	for i := 0; i < 260; i++ {
		c1.record(Status{ProviderID: "p1", OK: true, LatencyMs: int64(i + 1), CheckedAt: now.Add(time.Duration(i) * time.Second)}, nil)
	}
	if got := historyBackupSamples(t, dir, "p1"); len(got) != 256 || got[255].LatencyMs != 256 {
		t.Fatalf("backup = %d samples before corruption, want the 256-sample replica (latencies 1..256)", len(got))
	}
	if err := os.WriteFile(filepath.Join(dir, HistoryFileName), []byte(`corrupt`), 0o600); err != nil {
		t.Fatalf("WriteFile corrupt: %v", err)
	}
	// Restart restores the full 260 from the replica + journal.
	c2 := NewWithDataDir(nil, nil, dir)
	if sum := c2.History("p1"); sum.Samples != 260 {
		t.Fatalf("restored Samples = %d, want 260", sum.Samples)
	}
	// Record 124 more probes to force the next compaction
	// (journal 4 + 124 = 128 lines). The backup is written from
	// the marshalled ring in memory, never by copying the primary
	// file — so the compaction cannot copy the corrupt primary
	// over the good backup; instead it heals the backup to the
	// current 384-sample ring, identical to the new snapshot.
	for i := 260; i < 384; i++ {
		c2.record(Status{ProviderID: "p1", OK: true, LatencyMs: int64(i + 1), CheckedAt: now.Add(time.Duration(i) * time.Second)}, nil)
	}
	if got := historyBackupSamples(t, dir, "p1"); len(got) != 384 || got[383].LatencyMs != 384 {
		t.Errorf("backup after recovery compaction = %d samples (last latency %v), want the healed 384-sample replica (last latency 384)", len(got), lastLatency(got))
	}
	if got := historyFileSamples(t, dir, "p1"); len(got) != 384 {
		t.Errorf("primary after recovery = %d samples, want 384 (restored 260 + 124 new)", len(got))
	}
	if got := historyMergedSamples(t, dir, "p1"); len(got) != 384 {
		t.Errorf("merged after recovery = %d samples, want 384", len(got))
	}
}

func TestHistoryPruneProvider(t *testing.T) {
	dir := t.TempDir()
	c := NewWithDataDir(nil, nil, dir)
	now := time.Now().UTC()
	seedHistory(c, "p1", now.Add(-time.Minute), true, 40, 3)
	seedHistory(c, "p2", now.Add(-time.Minute), true, 50, 2)
	c.PruneProvider("p1")
	if sum := c.History("p1"); sum.Samples != 0 {
		t.Errorf("p1 Samples = %d after PruneProvider, want 0", sum.Samples)
	}
	if sum := c.History("p2"); sum.Samples != 2 {
		t.Errorf("p2 Samples = %d after pruning p1, want 2 (untouched)", sum.Samples)
	}
	// Persisted: p1 is gone from the file, p2 stays.
	if got := historyFileSamples(t, dir, "p1"); len(got) != 0 {
		t.Errorf("file still has %d p1 samples after prune", len(got))
	}
	if got := historyFileSamples(t, dir, "p2"); len(got) != 2 {
		t.Errorf("file has %d p2 samples after prune, want 2", len(got))
	}
	// Pruning an unknown provider is a no-op.
	c.PruneProvider("no-such-provider")
	if sum := c.History("p2"); sum.Samples != 2 {
		t.Errorf("p2 Samples = %d after pruning an unknown provider, want 2", sum.Samples)
	}
}

func TestHistoryOrphansPrunedAtLoad(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	// A router with two providers records history for both.
	c1 := NewWithDataDir(nil, nil, dir)
	seedHistory(c1, "p1", now.Add(-time.Minute), true, 40, 3)
	seedHistory(c1, "p2", now.Add(-time.Minute), true, 50, 2)
	// p2 is then removed from the config (while the router is
	// down, or by hand-editing the providers file): the registry
	// now knows only p1.
	provPath := filepath.Join(dir, "providers.json")
	cfgs, err := json.Marshal([]provider.ProviderConfig{
		{ID: "p1", Name: "One", BaseURL: "https://one.example/v1", WireFormat: "openai", Enabled: true},
	})
	if err != nil {
		t.Fatalf("Marshal configs: %v", err)
	}
	if err := os.WriteFile(provPath, cfgs, 0o600); err != nil {
		t.Fatalf("WriteFile providers: %v", err)
	}
	reg := provider.NewRegistry(provider.NewJSONFileStore(provPath), provider.WithRegistryAllowPrivateDestinations(true))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry Load: %v", err)
	}
	// Restart with the p1-only registry: p2's history is pruned,
	// p1's survives.
	c2 := NewWithDataDir(reg, nil, dir)
	if sum := c2.History("p2"); sum.Samples != 0 {
		t.Errorf("orphan p2 Samples = %d after load, want 0", sum.Samples)
	}
	if sum := c2.History("p1"); sum.Samples != 3 {
		t.Errorf("p1 Samples = %d after load, want 3 (kept)", sum.Samples)
	}
	if got := historyFileSamples(t, dir, "p2"); len(got) != 0 {
		t.Errorf("file still has %d p2 samples after orphan prune", len(got))
	}
}

func TestHistoryOrphansNotPrunedWithEmptyRegistry(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	c1 := NewWithDataDir(nil, nil, dir)
	seedHistory(c1, "p1", now.Add(-time.Minute), true, 40, 3)
	// A nil registry (fresh install, no config yet) must NOT wipe
	// the recorded history — the guard in PruneOrphans.
	c2 := NewWithDataDir(nil, nil, dir)
	if sum := c2.History("p1"); sum.Samples != 3 {
		t.Errorf("p1 Samples = %d with an empty registry, want 3 (kept)", sum.Samples)
	}
}

// TestCheckerClearHistory proves the probe-history half of the
// DB-wipe scope: every sample leaves the ring, the persisted
// snapshot AND its .bak replica AND the journal are deleted (any
// of the three could otherwise resurrect the history), and a fresh
// checker on the same data dir after the clear restores nothing.
// The current per-provider status is live state, not history, and
// is kept. An in-memory-only checker clears its ring with no files.
func TestCheckerClearHistory(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	c := NewWithDataDir(nil, nil, dir)
	// 300 p1 records cross two compactions, so the snapshot, its
	// replica backup AND the journal all exist before the clear:
	// the clear has to delete all three.
	seedHistory(c, "p1", now.Add(-30*time.Minute), true, 40, 300)
	seedHistory(c, "p2", now.Add(-10*time.Minute), true, 12, 3)
	if _, err := os.Stat(filepath.Join(dir, HistoryFileName+".bak")); err != nil {
		t.Fatalf("Stat backup before clear: %v (want a .bak to exist, so the test proves it is deleted)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, HistoryFileName)); err != nil {
		t.Fatalf("Stat history file before clear: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, HistoryJournalFileName)); err != nil {
		t.Fatalf("Stat journal before clear: %v (want a journal to exist, so the test proves it is deleted)", err)
	}
	samples, filesRemoved, err := c.ClearHistory()
	if err != nil {
		t.Fatalf("ClearHistory: %v", err)
	}
	if samples != 303 {
		t.Errorf("ClearHistory samples = %d, want 303 (300+3)", samples)
	}
	if !filesRemoved {
		t.Error("ClearHistory filesRemoved = false, want true (history file + backup + journal deleted)")
	}
	if sum := c.History("p1"); sum.Samples != 0 {
		t.Errorf("p1 Samples after clear = %d, want 0", sum.Samples)
	}
	if _, err := os.Stat(filepath.Join(dir, HistoryFileName)); !os.IsNotExist(err) {
		t.Errorf("history file still exists after clear (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, HistoryFileName+".bak")); !os.IsNotExist(err) {
		t.Errorf("history backup still exists after clear (err %v) — the old ring would resurrect on restore", err)
	}
	if _, err := os.Stat(filepath.Join(dir, HistoryJournalFileName)); !os.IsNotExist(err) {
		t.Errorf("history journal still exists after clear (err %v) — the old ring would resurrect on restore", err)
	}
	// Restart on the same data dir: nothing comes back.
	c2 := NewWithDataDir(nil, nil, dir)
	if sum := c2.History("p1"); sum.Samples != 0 {
		t.Errorf("p1 Samples after restart = %d, want 0 (wipe must survive restart)", sum.Samples)
	}
	// A probe recorded after the clear is post-wipe traffic and is
	// honestly new — the clear must not break later persistence.
	c.record(Status{ProviderID: "p1", OK: true, LatencyMs: 9, CheckedAt: now}, nil)
	if sum := c.History("p1"); sum.Samples != 1 {
		t.Errorf("p1 Samples after post-wipe probe = %d, want 1", sum.Samples)
	}
	if got := historyMergedSamples(t, dir, "p1"); len(got) != 1 {
		t.Errorf("persisted p1 samples after post-wipe probe = %d, want 1", len(got))
	}
}

func TestCheckerClearHistoryInMemoryOnly(t *testing.T) {
	c := NewWithDataDir(nil, nil, "")
	seedHistory(c, "p1", time.Now().UTC(), true, 10, 4)
	samples, filesRemoved, err := c.ClearHistory()
	if err != nil {
		t.Fatalf("ClearHistory: %v", err)
	}
	if samples != 4 {
		t.Errorf("samples = %d, want 4", samples)
	}
	if filesRemoved {
		t.Error("filesRemoved = true with no data dir, want false")
	}
	if sum := c.History("p1"); sum.Samples != 0 {
		t.Errorf("p1 Samples after clear = %d, want 0", sum.Samples)
	}
}
