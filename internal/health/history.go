// Probe-history persistence: the bounded in-memory probe ring
// (History: last 48 h, at most 2,880 samples per provider) is saved
// to disk in the router's data dir — alongside ledger.db and
// feedback.db — as health-history.json, and reloaded at startup, so
// uptime/latency history survives a restart instead of resetting.
//
// The file holds real probe results only, never invented values: a
// provider with no recorded probes still reports "no data". Writes
// are crash-safe, following the same pattern as config.Save and
// crypto.writeFileAtomic: write a temp file in the same directory
// (0600), fsync, then rename over the destination, so a crash
// mid-write can never leave a half-written file behind. At every
// snapshot write the same bytes are also written to
// health-history.json.bak as a replica, and a corrupt primary is
// restored from that backup at load time — a corrupt file loses
// nothing. A missing file is a fresh install (empty history, no error). Only
// when both copies fail does load fall back to empty history
// honestly — startup never fails because of it — and the failure
// is reported on stderr so the operator sees it. Providers that
// no longer exist in the registry are pruned from the ring at
// load time (and live, on provider delete), so a removed provider
// leaves no stale uptime behind it.
//
// Durability design (CHECKLIST 206): a full rewrite of the
// snapshot on every probe made persistence unusable —
// TestHistoryPrunesOnWrite did 2,980 whole-file rewrites and took
// 128 s (baseline 2026-11-18), and even without the fsyncs a
// rewrite costs ~7 ms on this VM, so 2,980 of them still took
// 67 s. Each recorded probe is therefore APPENDED as one small
// line to health-history.journal (~6 µs), and the full snapshot
// is rewritten only at a periodic compaction (every
// historyCompactEvery appends, and on every prune), when it is
// backed up and fsync'd as before. The snapshot carries the
// journal sequence number it includes (journal_seq), so a load
// replays only the journal lines newer than the snapshot and a
// crash between the snapshot write and the journal truncate can
// neither lose nor duplicate a sample. A power loss can lose at
// most the journal tail not yet flushed by the OS — the snapshot
// itself is fsync'd at every compaction and is always a complete
// valid ring, never torn. Prune semantics are unchanged: the same
// cap/retention pruning runs on every write, in memory and on
// disk, and a prune always compacts immediately, so a deleted
// provider's samples leave the disk at once.
//
// The 48 h / 2,880-sample cap is enforced on load and on write:
// samples older than HistoryRetention are dropped and each
// provider's ring is capped to the newest MaxHistorySamples, so a
// stale or oversized file can never grow memory without bound.
package health

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// HistoryFileName is the probe-history file inside the router's data
// dir (see config.DataDir; the same dir holds ledger.db and
// feedback.db).
const HistoryFileName = "health-history.json"

// HistoryJournalFileName is the append-only probe log next to the
// snapshot: one JSON line per recorded probe (see journalLine).
// The snapshot is rewritten only at a compaction; a restart
// replays the journal lines newer than the snapshot's watermark,
// so no probe is lost between compactions.
const HistoryJournalFileName = "health-history.journal"

// historyFile is the on-disk shape of the probe ring. Version is 1;
// a file with any other non-zero version is treated as corrupt.
// JournalSeq is the watermark: the newest journal sequence number
// included in this snapshot (0 for files written before the
// journal existed). A load replays only journal lines with a
// higher seq, so a crash between the snapshot write and the
// journal truncate can neither lose nor duplicate a sample.
type historyFile struct {
	Version    int                      `json:"version"`
	JournalSeq int64                    `json:"journal_seq,omitempty"`
	Providers  map[string][]ProbeSample `json:"providers"`
}

// NewWithDataDir returns a Checker like New, but with its probe
// history persisted to filepath.Join(dataDir, HistoryFileName):
// the file is loaded at startup (pruned to the 48 h / 2,880-sample
// cap, and pruned of providers no longer in the registry) and
// every recorded probe is appended to the journal next to it
// (see HistoryJournalFileName), with the snapshot rewritten only
// at a periodic compaction and a replica backup kept (see
// saveHistoryFile). With an empty dataDir the checker behaves
// exactly like New (in-memory history only, no file). A missing
// file is a fresh install. A corrupt file falls back to the
// backup copy, and only when both fail does it start with empty
// history — the failure is reported on stderr either way;
// startup never fails because of it.
func NewWithDataDir(reg *provider.Registry, factory provider.ClientFactory, dataDir string) *Checker {
	c := New(reg, factory)
	if dataDir == "" {
		return c
	}
	c.historyPath = filepath.Join(dataDir, HistoryFileName)
	c.journalPath = filepath.Join(dataDir, HistoryJournalFileName)
	if err := c.loadHistoryFile(c.historyPath); err != nil {
		fmt.Fprintf(os.Stderr, "health: cannot load probe history from %q, starting with empty history: %v\n", c.historyPath, err)
	}
	// Orphaned providers pruned: a provider removed while the
	// router was down (or hand-edited out of the config) leaves
	// no stale uptime behind it. A nil/empty registry prunes
	// nothing (see PruneOrphans): a fresh install must not wipe a
	// ring a later provider add would rejoin.
	if reg != nil {
		known := make(map[string]bool, len(reg.List()))
		for _, p := range reg.List() {
			known[p.ID] = true
		}
		c.PruneOrphans(known)
	}
	return c
}

// historyPath returns the persistence path ("" when persistence is
// off). It exists so callers can report where history lives.
func (c *Checker) HistoryPath() string {
	if c == nil {
		return ""
	}
	return c.historyPath
}

// pruneHistory drops samples older than HistoryRetention relative to
// now and caps each provider's ring to the newest MaxHistorySamples.
// It returns a copy; the input is not modified. Providers left with
// no samples are dropped.
func pruneHistory(in map[string][]ProbeSample, now time.Time) map[string][]ProbeSample {
	out := make(map[string][]ProbeSample, len(in))
	cutoff := now.Add(-HistoryRetention)
	for id, samples := range in {
		kept := make([]ProbeSample, 0, len(samples))
		for _, s := range samples {
			if s.At.IsZero() || s.At.Before(cutoff) {
				continue
			}
			kept = append(kept, s)
		}
		if len(kept) > MaxHistorySamples {
			kept = kept[len(kept)-MaxHistorySamples:]
		}
		if len(kept) > 0 {
			out[id] = kept
		}
	}
	return out
}

// historyBackupSuffix is appended to the history path for the
// backup replica saveHistoryFile writes at every compaction (see
// saveHistoryFile): a corrupt primary file then loses nothing —
// the replica holds the same snapshot the primary held.
const historyBackupSuffix = ".bak"

// readHistoryFile reads and decodes one history file. A missing
// file is a fresh install (empty history, nil error).
func readHistoryFile(path string) (historyFile, error) {
	var hf historyFile
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return hf, nil
		}
		return hf, fmt.Errorf("health: cannot read probe history: %w", err)
	}
	if err := json.Unmarshal(raw, &hf); err != nil {
		return hf, fmt.Errorf("health: invalid probe history JSON: %w", err)
	}
	if hf.Version != 0 && hf.Version != 1 {
		return hf, fmt.Errorf("health: unsupported probe history version %d", hf.Version)
	}
	return hf, nil
}

// journalLine is one line of the append-only probe log: the
// sequence number (monotonic per data dir, see loadHistoryFile),
// the provider, and the recorded sample (ProbeSample's JSON tags
// are reused: at / ok / latency_ms).
type journalLine struct {
	Seq      int64  `json:"seq"`
	Provider string `json:"provider"`
	ProbeSample
}

// loadHistoryFile reads path into the checker's in-memory ring,
// pruned to the 48 h / 2,880-sample cap, then replays the journal
// lines newer than the snapshot's watermark (see historyFile).
// A missing file is a fresh install (empty history, nil error). A
// corrupt or unreadable primary falls back to the backup copy
// (path + ".bak", written before every compaction by
// saveHistoryFile): a corrupt file then loses nothing — the
// previous good ring is restored from the backup plus the journal
// lines it does not cover, and the failure is reported on stderr.
// Only when both copies fail AND the journal yields nothing does
// it return an error and leave the checker's history empty — the
// caller decides how to report it; startup never fails because
// of it. The journal is the third copy: when both snapshots are
// corrupt but the journal holds lines, those lines are still
// recovered (watermark 0) and no error is returned. A torn
// journal line (a partial write from a crash mid-append) is
// skipped with a stderr note, never a failed load. The checker's
// journal counters are set from what was loaded, so new appends
// continue the sequence with no reuse and no duplicates.
func (c *Checker) loadHistoryFile(path string) error {
	hf, err := readHistoryFile(path)
	watermark := int64(0)
	snapErr := err
	if err != nil {
		backup := path + historyBackupSuffix
		if _, statErr := os.Stat(backup); statErr == nil {
			if bhf, berr := readHistoryFile(backup); berr == nil {
				fmt.Fprintf(os.Stderr, "health: probe history %q is corrupt (%v); restored from backup %q\n", path, err, backup)
				hf = bhf
				snapErr = nil
			}
		}
	}
	if snapErr == nil {
		watermark = hf.JournalSeq
	}
	journalPath := filepath.Join(filepath.Dir(path), HistoryJournalFileName)
	live, maxSeq, jerr := replayJournal(journalPath, watermark)
	if jerr != nil {
		fmt.Fprintf(os.Stderr, "health: cannot replay probe journal %q: %v\n", journalPath, jerr)
	}
	// A torn tail (a partial line with no trailing newline, from
	// a crash mid-append) is repaired here so the next append
	// starts on a fresh line: the partial line is dropped — it
	// never parsed, so nothing is lost — and later appends can
	// no longer fuse with it into one unparsable line.
	if rerr := repairJournalTail(journalPath); rerr != nil {
		fmt.Fprintf(os.Stderr, "health: cannot repair probe journal %q: %v\n", journalPath, rerr)
	}
	merged := hf.Providers
	if merged == nil {
		merged = make(map[string][]ProbeSample)
	}
	for id, samples := range live {
		merged[id] = append(merged[id], samples...)
	}
	// Counters are set before any early return: a checker whose
	// load failed must still append with a fresh sequence, never
	// reuse seq 0 — a seq-0 line would be skipped by the
	// watermark replay on the next load.
	c.persistMu.Lock()
	c.compactSeq = watermark
	c.journalLines = 0
	for _, samples := range live {
		c.journalLines += len(samples)
	}
	c.nextSeq = watermark + 1
	if maxSeq >= c.nextSeq {
		c.nextSeq = maxSeq + 1
	}
	c.persistMu.Unlock()
	if snapErr != nil && len(live) == 0 {
		return snapErr
	}
	pruned := pruneHistory(merged, time.Now().UTC())
	c.mu.Lock()
	if c.history == nil {
		c.history = make(map[string][]ProbeSample)
	}
	for id, samples := range pruned {
		cp := make([]ProbeSample, len(samples))
		copy(cp, samples)
		c.history[id] = cp
	}
	c.mu.Unlock()
	return nil
}

// replayJournal reads the append-only probe log at path and
// returns the samples with seq > watermark, grouped by provider,
// in file order, plus the highest seq seen in the file (0 when
// the file is missing or holds no parseable line). A missing file
// is a fresh install (nil map, nil error). A line that does not
// parse — a torn tail from a crash mid-append — is skipped with
// a stderr note; it never fails the replay, and lines after it
// are still read (only the tail can be torn, but skipping one
// bad line is safer than dropping the rest of the log).
func replayJournal(path string, watermark int64) (map[string][]ProbeSample, int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("health: cannot read probe journal: %w", err)
	}
	live := make(map[string][]ProbeSample)
	var maxSeq int64
	for _, line := range splitJournalLines(raw) {
		if len(line) == 0 {
			continue
		}
		var jl journalLine
		if err := json.Unmarshal(line, &jl); err != nil {
			fmt.Fprintf(os.Stderr, "health: skipping unparsable probe journal line %q: %v\n", string(line), err)
			continue
		}
		if jl.Seq > maxSeq {
			maxSeq = jl.Seq
		}
		if jl.Seq <= watermark || jl.Provider == "" {
			continue
		}
		live[jl.Provider] = append(live[jl.Provider], jl.ProbeSample)
	}
	return live, maxSeq, nil
}

// splitJournalLines splits raw journal bytes into lines without
// allocating a scanner: a trailing newline leaves no empty last
// line, and a final line without a newline (a partial write) is
// still returned so the caller can skip it as unparsable.
func splitJournalLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			lines = append(lines, raw[start:i])
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}

// repairJournalTail drops a torn tail line from the journal: every
// append writes line + '\n' in one Write call, so a file that does
// not end with '\n' ends in a partial line, which can never parse
// and must not fuse with the next append. The file is truncated
// to just after its last newline (or to empty when it has none).
// A file that already ends with '\n' — or is missing — is left
// alone. Best-effort: the caller reports any error.
func repairJournalTail(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(raw) == 0 || raw[len(raw)-1] == '\n' {
		return nil
	}
	last := -1
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == '\n' {
			last = i
			break
		}
	}
	return os.Truncate(path, int64(last+1))
}

// writeFileAtomicSync writes raw to path atomically (0600): a temp
// file in the same directory, fsync, then rename, following
// config.Save / crypto.writeFileAtomic. A crash mid-write can
// never leave a half-written file behind.
func writeFileAtomicSync(dir, pattern, path string, raw []byte) error {
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("health: cannot create temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("health: cannot write temp file in %q: %w", dir, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("health: cannot set temp file permissions in %q: %w", dir, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("health: cannot sync temp file in %q: %w", dir, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("health: cannot close temp file in %q: %w", dir, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("health: cannot replace %q: %w", path, err)
	}
	return nil
}

// saveHistoryFile writes snapshot to path atomically (see
// writeFileAtomicSync), and writes the SAME bytes to path + ".bak"
// as a replica. The snapshot is pruned to the cap first, so the
// file never holds stale or over-cap samples, and stamped with
// journalSeq (the newest journal line it includes) so a load can
// replay exactly the newer lines. The caller holds c.persistMu so
// concurrent records cannot interleave (and overwrite each other
// with an older snapshot). saveHistoryFile only runs at a
// compaction (see compactHistory), never per probe — that is the
// whole point of the journal.
//
// The backup is a REPLICA of the latest snapshot, not the previous
// ring: a corrupt primary then loses nothing, because the backup
// holds the same watermark and the journal covers everything
// after it (a predecessor backup would leave a gap of up to one
// compaction interval). The backup is written from the marshalled
// bytes in memory, never by copying the primary file — so a
// corrupt primary can never overwrite the good backup, and a
// compaction after a corruption heals the backup instead. A
// backup failure is best-effort (reported on stderr, the save
// proceeds): the primary is the live copy and the next compaction
// retries the replica.
func saveHistoryFile(path string, snapshot map[string][]ProbeSample, journalSeq int64) error {
	pruned := pruneHistory(snapshot, time.Now().UTC())
	raw, err := json.Marshal(historyFile{Version: 1, JournalSeq: journalSeq, Providers: pruned})
	if err != nil {
		return fmt.Errorf("health: cannot encode probe history: %w", err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("health: cannot create directory %q: %w", dir, err)
	}
	if err := writeFileAtomicSync(dir, ".health-history-*.tmp", path, raw); err != nil {
		return err
	}
	if err := writeFileAtomicSync(dir, ".health-history-bak-*.tmp", path+historyBackupSuffix, raw); err != nil {
		fmt.Fprintf(os.Stderr, "health: cannot write probe history backup: %v\n", err)
	}
	return nil
}

// snapshotHistory returns a deep copy of the checker's in-memory
// ring. The caller must not hold c.mu.
func (c *Checker) snapshotHistory() map[string][]ProbeSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string][]ProbeSample, len(c.history))
	for id, samples := range c.history {
		cp := make([]ProbeSample, len(samples))
		copy(cp, samples)
		out[id] = cp
	}
	return out
}

// historyCompactEvery is the number of journal lines between
// snapshot compactions: 2,980 back-to-back records then rewrite
// the full snapshot ~24 times instead of 2,980 (measured
// 2026-11-18: a full rewrite costs ~7 ms on this VM, a journal
// append ~6 µs). A restart replays the journal, so the snapshot
// lag loses nothing.
const historyCompactEvery = 128

// persistHistory appends one recorded probe to the journal and
// compacts the snapshot when the journal has grown past
// historyCompactEvery lines. It is called by record after the
// in-memory update; the caller must hold c.persistMu. A journal
// failure is reported on stderr and never fails the probe: the
// result is already recorded in memory, and the next probe
// retries the append (the failed line is simply missing from the
// journal — the in-memory ring is the truth until the next
// compaction rewrites the snapshot from it). A power loss can
// lose at most the journal tail not yet flushed by the OS.
func (c *Checker) persistHistory(st Status) {
	if c == nil || c.journalPath == "" {
		return
	}
	seq := c.nextSeq
	line, err := json.Marshal(journalLine{
		Seq:      seq,
		Provider: st.ProviderID,
		ProbeSample: ProbeSample{
			At:        st.CheckedAt,
			OK:        st.OK,
			LatencyMs: st.LatencyMs,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "health: cannot encode probe journal line: %v\n", err)
		return
	}
	line = append(line, '\n')
	f, err := os.OpenFile(c.journalPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "health: cannot open probe journal: %v\n", err)
		return
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		fmt.Fprintf(os.Stderr, "health: cannot append to probe journal: %v\n", err)
		return
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "health: cannot close probe journal: %v\n", err)
		return
	}
	c.nextSeq++
	c.journalLines++
	if c.journalLines >= historyCompactEvery {
		c.compactHistory()
	}
}

// compactHistory rewrites the snapshot from the in-memory ring
// (backup + fsync, stamped with the newest journal seq) and then
// truncates the journal, so the next load starts from the fresh
// snapshot. The journal is truncated ONLY after the snapshot
// rename succeeds: a crash between the two leaves stale lines in
// the journal, which the watermark skips on replay. A snapshot
// failure is reported on stderr and the journal is left alone —
// the lines stay live and the next compaction retries. The
// caller must hold c.persistMu. With no persistence configured
// this is a no-op.
func (c *Checker) compactHistory() {
	if c == nil || c.historyPath == "" {
		return
	}
	seq := c.nextSeq - 1
	if err := saveHistoryFile(c.historyPath, c.snapshotHistory(), seq); err != nil {
		fmt.Fprintf(os.Stderr, "health: cannot compact probe history: %v\n", err)
		return
	}
	if err := os.WriteFile(c.journalPath, nil, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "health: cannot truncate probe journal: %v\n", err)
		return
	}
	c.compactSeq = seq
	c.journalLines = 0
}

// PruneProvider drops all history for one provider — its probe ring
// and its last status/error — and persists the pruned ring, so a
// live provider delete (DELETE /api/providers/{id}) leaves no stale
// uptime behind it. It mirrors record's locking (persistMu, then
// mu briefly, then the file write while still holding persistMu),
// so a concurrent probe cannot resurrect the pruned provider or
// overwrite the pruned file with a stale snapshot. A provider with
// no history is a no-op; a persist failure is reported on stderr
// and never fails the caller (the in-memory drop already happened).
func (c *Checker) PruneProvider(providerID string) {
	if c == nil || providerID == "" {
		return
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.mu.Lock()
	delete(c.history, providerID)
	delete(c.last, providerID)
	delete(c.lastErr, providerID)
	c.mu.Unlock()
	c.compactHistory()
}

// ClearHistory drops every recorded probe sample from the ring and
// deletes the persisted history file AND its .bak backup AND the
// journal, so no uptime/latency history survives anywhere on disk.
// The current per-provider status (last probe result) is kept: it
// is live state, not history, and the next probe refreshes it. It
// mirrors record's locking (persistMu, then mu briefly), so a
// concurrent probe cannot re-persist the old ring mid-clear; a
// probe recorded after the clear is post-wipe traffic and is
// honestly new. It deliberately does NOT compact afterwards:
// compacting would recreate the files with an empty ring, and a
// wipe means the files are gone until the next probe recreates
// them. The next probe recreates the journal with fresh samples
// (and the snapshot at the next compaction). A checker built with
// New (no data dir) clears the ring only. There is no undo:
// callers must gate this behind the operator-credential boundary
// (POST /admin/db/wipe).
func (c *Checker) ClearHistory() (samples int, filesRemoved bool, err error) {
	if c == nil {
		return 0, false, nil
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.mu.Lock()
	for _, s := range c.history {
		samples += len(s)
	}
	c.history = make(map[string][]ProbeSample)
	c.mu.Unlock()
	if c.historyPath == "" {
		return samples, false, nil
	}
	c.journalLines = 0
	c.compactSeq = 0
	for _, path := range []string{c.historyPath, c.historyPath + historyBackupSuffix, c.journalPath} {
		if rmErr := os.Remove(path); rmErr != nil {
			if !os.IsNotExist(rmErr) {
				return samples, filesRemoved, fmt.Errorf("health: clear probe history %q: %w", path, rmErr)
			}
		} else {
			filesRemoved = true
		}
	}
	return samples, filesRemoved, nil
}

// PruneOrphans drops history for every provider not in known —
// providers that no longer exist in the registry (removed while
// the router was down, or hand-edited out of the config). It
// persists the pruned ring. Called at load time (see
// NewWithDataDir) and available to callers that reconcile the ring
// with the live registry. With no known set it drops nothing:
// an empty registry at startup (fresh install) must not wipe a
// ring that a later provider add would rejoin.
func (c *Checker) PruneOrphans(known map[string]bool) {
	if c == nil || len(known) == 0 {
		return
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.mu.Lock()
	for id := range c.history {
		if !known[id] {
			delete(c.history, id)
		}
	}
	for id := range c.last {
		if !known[id] {
			delete(c.last, id)
			delete(c.lastErr, id)
		}
	}
	c.mu.Unlock()
	c.compactHistory()
}
