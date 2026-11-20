// Async batched ledger writes.
//
// Hot-path note (pprof, 2026-11-17 bench: ledgerAdapter.RecordAttempt
// was 17.7% of all router CPU): every request paid a synchronous
// SQLite transaction on the hot path — BeginTx + INSERT + budget
// UPDATE + COMMIT, each COMMIT an fsync (WAL + synchronous=FULL)
// plus a fresh SQL parse per statement in the pure-Go driver. Under
// 32 concurrent clients that serialises every request behind one
// writer connection and inflates the RTT the adaptive limiter sees,
// which then sheds load as 502s.
//
// AsyncWriter moves the write off the hot path: Record enqueues and
// returns (a channel send, ~100 ns), and one background goroutine
// commits rows in batches — one transaction per batch of up to
// BatchSize rows or one FlushInterval, with one multi-row INSERT
// per 32-row chunk (see multiRowInsertChunk) and one fsync per
// batch, not per row. The synchronous RecordUsage stays the
// durable API for direct callers and tests; this writer is what
// the request path uses (via the main.go adapter).
//
// Honest tradeoffs, stated plainly:
//   - Durability window: rows sit in memory for at most
//     FlushInterval (default 50 ms) before commit. A crash or
//     kill -9 inside that window loses those rows; a graceful
//     shutdown (SIGINT/SIGTERM, /admin/shutdown) flushes first
//     via Close, so nothing is lost on a normal stop. The ledger
//     is a usage/billing record, so this window is a real cost —
//     it is the price of the throughput, and it is bounded and
//     documented here, not hidden.
//   - Backpressure: the queue is bounded (default 8,192 rows). A
//     full queue does NOT drop rows: Record falls back to a
//     synchronous RecordUsage for that row (slow, but the row is
//     kept) and counts it in SyncFallbacks. The fallback is the
//     safety valve, not the normal path.
//   - Batch failure: if a batch commit fails, the rows are counted
//     in FailedRows, the failure is logged to stderr, and the
//     writer keeps going — a poisoned batch must not wedge the
//     writer and take the request path down with it. Failed rows
//     are lost; the count is exposed via Stats so the loss is
//     visible, never silent.
//   - Read lag: dashboard feeds read SQLite, so they lag the live
//     request count by at most one flush interval. Counts are
//     never invented to hide the lag.
package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncWriterConfig tunes an AsyncWriter. Zero values select the
// defaults: queue 8,192 rows, batch 256 rows, flush every 50 ms.
type AsyncWriterConfig struct {
	// QueueCap bounds the in-memory queue. A full queue falls back
	// to synchronous writes (never drops).
	QueueCap int
	// BatchSize is the most rows committed in one transaction.
	BatchSize int
	// FlushInterval is the longest a queued row waits for a commit.
	FlushInterval time.Duration
}

// AsyncStats is the writer's honest accounting: rows still queued,
// rows written synchronously because the queue was full, rows lost
// to failed batch commits, and batches committed so far.
type AsyncStats struct {
	Queued        int
	SyncFallbacks uint64
	FailedRows    uint64
	Batches       uint64
	CommittedRows uint64
}

// AsyncWriter batches ledger rows off the request path. Use
// NewAsyncWriter, Record/RecordShared on the hot path, Flush in
// tests, and Close on shutdown (it flushes first).
type AsyncWriter struct {
	l      *Ledger
	prices Prices
	// pricesMu guards prices: the map is normally fixed at
	// construction, but the opt-in models.dev sync (CHECKLIST
	// 179) replaces it when a periodic re-sync changes a price.
	// Reads snapshot the map reference under RLock, so a row is
	// always priced with one whole map, never a half-swapped one.
	pricesMu sync.RWMutex

	ch      chan Usage
	flushCh chan chan struct{}
	stopCh  chan struct{}
	doneCh  chan struct{}

	batchSize     int
	flushInterval time.Duration

	syncFallbacks uint64
	failedRows    uint64
	batches       uint64
	committedRows uint64
}

// SetPrices replaces the price map the writer charges rows with
// (see pricesMu). It is used by the opt-in models.dev sync when a
// periodic re-sync changes a price; a nil map means "no prices
// known" and charges $0, exactly as construction with nil would.
// Rows already queued keep the map they were priced with only if
// they were priced before the swap — in practice the swap happens
// between batches, and a row is priced at commit time, so a row
// committed after the swap is charged at the new price.
func (w *AsyncWriter) SetPrices(p Prices) {
	if w == nil {
		return
	}
	w.pricesMu.Lock()
	w.prices = p
	w.pricesMu.Unlock()
}

// pricesSnapshot returns the price map in force, for one pricing
// decision. The map itself is never mutated in place — SetPrices
// replaces the reference — so the snapshot stays valid for the
// whole call.
func (w *AsyncWriter) pricesSnapshot() Prices {
	w.pricesMu.RLock()
	defer w.pricesMu.RUnlock()
	return w.prices
}

// flatRateAsZero returns the ledger's FlatRateAsZero opt-in for
// one pricing decision (see Ledger.FlatRateAsZero). A nil ledger
// means the opt-in is off.
func (w *AsyncWriter) flatRateAsZero() bool {
	if w == nil || w.l == nil {
		return false
	}
	return w.l.FlatRateAsZero
}

// NewAsyncWriter starts the background batch writer for l. A nil l
// returns a writer whose Record calls are no-ops (the engine's
// nil-ledger case). Prices start as the construction map — the same
// prices the synchronous adapter would have used per call — and
// may be replaced later with SetPrices (the opt-in models.dev
// sync does so when a periodic re-sync changes a price).
func NewAsyncWriter(l *Ledger, prices Prices, cfg AsyncWriterConfig) *AsyncWriter {
	w := &AsyncWriter{l: l, prices: prices}
	if cfg.QueueCap <= 0 {
		cfg.QueueCap = 8192
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 256
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 50 * time.Millisecond
	}
	w.batchSize = cfg.BatchSize
	w.flushInterval = cfg.FlushInterval
	if l == nil {
		return w
	}
	w.ch = make(chan Usage, cfg.QueueCap)
	w.flushCh = make(chan chan struct{})
	w.stopCh = make(chan struct{})
	w.doneCh = make(chan struct{})
	go w.run()
	return w
}

// Record enqueues one usage row. It never blocks the caller: when
// the queue is full it writes the row synchronously instead (and
// counts the fallback), so no row is ever dropped to protect
// latency. A nil ledger is a no-op.
func (w *AsyncWriter) Record(u Usage) {
	if w == nil || w.l == nil {
		return
	}
	select {
	case w.ch <- u:
	default:
		atomic.AddUint64(&w.syncFallbacks, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := w.l.RecordUsage(ctx, u, w.pricesSnapshot()); err != nil {
			fmt.Fprintf(os.Stderr, "ledger: async sync-fallback record failed: %v\n", err)
			atomic.AddUint64(&w.failedRows, 1)
		}
	}
}

// RecordShared enqueues one coalesced-waiter row (zero tokens, zero
// cost, marked shared — see Usage.Shared).
func (w *AsyncWriter) RecordShared(provider, model string, ok bool, latencyMs int64) {
	w.Record(Usage{Provider: provider, Model: model, OK: ok, LatencyMS: latencyMs, Shared: true})
}

// Flush blocks until every row enqueued before the call is
// committed. Rows enqueued concurrently with Flush may or may not
// be included — callers that need a hard boundary must stop
// producing first (tests do; shutdown does via Close).
func (w *AsyncWriter) Flush() {
	if w == nil || w.l == nil {
		return
	}
	ack := make(chan struct{})
	select {
	case w.flushCh <- ack:
		<-ack
	case <-w.doneCh:
	}
}

// Close flushes all queued rows and stops the writer. It is safe to
// call more than once; the second call returns immediately.
func (w *AsyncWriter) Close() {
	if w == nil || w.l == nil {
		return
	}
	select {
	case <-w.doneCh:
		return
	default:
	}
	w.Flush()
	close(w.stopCh)
	<-w.doneCh
}

// Stats returns the writer's counters (see AsyncStats).
func (w *AsyncWriter) Stats() AsyncStats {
	if w == nil {
		return AsyncStats{}
	}
	queued := 0
	if w.ch != nil {
		queued = len(w.ch)
	}
	return AsyncStats{
		Queued:        queued,
		SyncFallbacks: atomic.LoadUint64(&w.syncFallbacks),
		FailedRows:    atomic.LoadUint64(&w.failedRows),
		Batches:       atomic.LoadUint64(&w.batches),
		CommittedRows: atomic.LoadUint64(&w.committedRows),
	}
}

func (w *AsyncWriter) run() {
	defer close(w.doneCh)
	var batch []Usage
	timer := time.NewTimer(w.flushInterval)
	defer timer.Stop()
	commit := func() {
		if len(batch) == 0 {
			return
		}
		w.commitBatch(batch)
		batch = batch[:0]
	}
	for {
		select {
		case u := <-w.ch:
			batch = append(batch, u)
			if len(batch) >= w.batchSize {
				commit()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.flushInterval)
			}
		case ack := <-w.flushCh:
			// Drain anything already queued so the flush covers
			// every row enqueued before the Flush call.
			draining := true
			for draining {
				select {
				case u := <-w.ch:
					batch = append(batch, u)
				default:
					draining = false
				}
			}
			commit()
			close(ack)
		case <-timer.C:
			commit()
			timer.Reset(w.flushInterval)
		case <-w.stopCh:
			// Final drain: everything queued up to the stop.
			draining := true
			for draining {
				select {
				case u := <-w.ch:
					batch = append(batch, u)
				default:
					draining = false
				}
			}
			commit()
			return
		}
	}
}

// multiRowInsertChunk is how many usage rows one INSERT statement
// carries in commitBatch. The modernc.org/sqlite driver re-parses
// its SQL on every Exec (prepareV2 inside exec, the statement
// finalized after each call), so a prepared statement is NOT
// reused: the old per-row INSERT re-parsed the 22-column
// statement once per row, and the cpu2 profile (2026-11-18) put
// insert.ExecContext at 2.09 s of commitBatch's 2.62 s cum. One
// statement per 32 rows cuts INSERT parses per 256-row batch from
// 256 to 8. 32 rows x 34 columns = 1088 placeholders, under
// SQLite's 32,766-variable modern limit, so the chunk size is safe
// on any build.
const multiRowInsertChunk = 32

// commitBatch writes one batch in a single transaction with one
// multi-row INSERT per 32-row chunk (one SQL parse per chunk, not
// per row — the driver re-parses on every Exec, see
// multiRowInsertChunk) and one aggregated budget UPDATE per
// gateway key, with one fsync per batch, not per row. A failure
// is counted and logged, never retried in a way that could
// double-record: a failed COMMIT may have partially applied, so
// re-running the batch could duplicate rows. A failed chunk
// counts exactly its rows as failed and excludes their spend
// from the budget update; the blast radius grows from 1 row to
// at most one 32-row chunk, and usage_events has no UNIQUE
// constraint a row can violate, so a chunk failure in practice
// means a DB-level error that would have failed every row
// anyway.
func (w *AsyncWriter) commitBatch(batch []Usage) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := w.l.db.BeginTx(ctx, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledger: async batch begin failed (%d rows lost): %v\n", len(batch), err)
		atomic.AddUint64(&w.failedRows, uint64(len(batch)))
		return
	}
	defer tx.Rollback()
	update, err := tx.PrepareContext(ctx, `
		UPDATE budgets SET spent_usd = spent_usd + ? WHERE key_id = ?`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledger: async batch prepare update failed (%d rows lost): %v\n", len(batch), err)
		atomic.AddUint64(&w.failedRows, uint64(len(batch)))
		return
	}
	defer update.Close()
	now := time.Now().UnixMilli()
	// Budget spend is aggregated per gateway key and applied with
	// one UPDATE per key after the inserts (pprof, 2026-11-18
	// bench: the writer goroutine was ~6% of router CPU, half of
	// it one UPDATE per row — and the bench's rows all share one
	// key, so a 75-row batch paid 75 updates for one number).
	// The accounting is unchanged: a row moves the budget only
	// when its INSERT chunk succeeded, and only when its key's
	// UPDATE succeeded; a failed key UPDATE counts that key's rows
	// as failed, exactly as a failed per-row UPDATE did.
	type keySpend struct {
		key   string
		total float64
		rows  int
	}
	var keys []string
	spend := make(map[string]*keySpend)
	// Normalize the whole batch first: a chunk either lands whole
	// or fails whole, so spend is aggregated only for rows in
	// chunks that succeeded.
	rows := make([]normalizedRow, 0, len(batch))
	for _, u := range batch {
		rows = append(rows, normalizeUsage(u, w.pricesSnapshot(), w.flatRateAsZero()))
	}
	for start := 0; start < len(rows); start += multiRowInsertChunk {
		end := start + multiRowInsertChunk
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		if err := insertChunk(ctx, tx, now, chunk); err != nil {
			fmt.Fprintf(os.Stderr, "ledger: async batch insert failed (%d rows in chunk lost): %v\n", len(chunk), err)
			atomic.AddUint64(&w.failedRows, uint64(len(chunk)))
			continue
		}
		for _, r := range chunk {
			ks, ok := spend[r.gatewayKeyID]
			if !ok {
				ks = &keySpend{key: r.gatewayKeyID}
				spend[r.gatewayKeyID] = ks
				keys = append(keys, r.gatewayKeyID)
			}
			ks.total += r.cost
			ks.rows++
		}
		// AK-3: fold each inserted row into the agent-session
		// rollup in the SAME transaction as the usage row, so
		// usage and session counters move together or neither
		// does — exactly as sync RecordUsage does. An upsert
		// error can only be a DB-level error (the function has
		// no per-row validation), so the whole batch is counted
		// failed and the transaction rolls back; retrying a
		// partial batch could double-record.
		for _, r := range chunk {
			if err := upsertAgentSessionTx(ctx, tx, r, time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "ledger: async batch agent-session upsert failed (%d rows lost): %v\n", len(batch), err)
				atomic.AddUint64(&w.failedRows, uint64(len(batch)))
				return
			}
		}
	}
	for _, key := range keys {
		ks := spend[key]
		if _, err := update.ExecContext(ctx, ks.total, ks.key); err != nil {
			fmt.Fprintf(os.Stderr, "ledger: async batch budget update failed: %v\n", err)
			atomic.AddUint64(&w.failedRows, uint64(ks.rows))
			continue
		}
		atomic.AddUint64(&w.committedRows, uint64(ks.rows))
	}
	if err := tx.Commit(); err != nil {
		fmt.Fprintf(os.Stderr, "ledger: async batch commit failed (%d rows may be lost): %v\n", len(batch), err)
		atomic.AddUint64(&w.failedRows, uint64(len(batch)))
		return
	}
	atomic.AddUint64(&w.batches, 1)
}

// insertChunk writes one chunk of normalized rows with a single
// multi-row INSERT — one SQL parse for the whole chunk instead of
// one per row (see multiRowInsertChunk). The statement is built
// per chunk because the row count varies (the last chunk of a
// batch is usually partial); the column list and the per-row
// placeholder tuple are fixed.
func insertChunk(ctx context.Context, tx *sql.Tx, now int64, chunk []normalizedRow) error {
	var b strings.Builder
	b.WriteString(`INSERT INTO usage_events
	  (ts, gateway_key_id, provider_key_id, provider, model, endpoint, prompt_tokens,
	   completion_tokens, total_tokens, cached_tokens,
	   reasoning_tokens, cache_write_tokens,
	   cache_write_tokens_5m, cache_write_tokens_1h,
	   prompt_before_compression_tokens, compression_saved_tokens, cache_tokens_saved,
	   local_estimate, local_estimate_prompt_tokens,
	   tokens_split_known, price_known,
	   cost_usd, ok, latency_ms, shared,
	   reported_cost_usd, reported_cost_known, cost_delta_usd,
	   service_tier, cancelled, session_id, project_id, client_id, auto_variant)
	VALUES `)
	args := make([]any, 0, len(chunk)*34)
	for i, r := range chunk {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		args = append(args, now, r.gatewayKeyID, r.providerKeyID, r.provider, r.model, r.endpoint,
			r.promptTokens, r.completionTokens, r.totalTokens, r.cachedTokens,
			r.reasoningTokens, r.cacheWriteTokens,
			r.cacheWriteTokens5m, r.cacheWriteTokens1h,
			r.promptBeforeCompression, r.compressionSavedTokens, r.cacheTokensSaved, r.localEstimate, r.localEstimatePrompt, r.splitKnown,
			r.priceKnown,
			r.cost, r.ok, r.latencyMs, r.shared,
			r.reportedCost, r.reportedKnown, r.costDelta,
			r.serviceTier, r.cancelled, r.sessionID, r.projectID, r.clientID, r.autoVariant)
	}
	_, err := tx.ExecContext(ctx, b.String(), args...)
	return err
}
