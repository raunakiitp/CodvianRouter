// DB export import — merging a codvian-router-db-export/v1 file into
// the ledger.
//
// This is the one import path, shared by the server route (POST
// /admin/db/import, internal/server/dbimport.go) and the CLI
// (`codvian-router db import`, cmd/codvian-router/main.go), so the
// two entry points can never drift apart on what a valid export is
// or what merging means.
//
// Merge rule (research-log entry 154): an import MERGES usage
// history into the existing ledger — it never resets or wipes it.
// Existing rows are untouched; only events the ledger does not
// already hold are added. Nothing in the config section of the
// export is applied: providers, keys, prices and settings stay
// exactly as they are — the import carries usage history only.
//
// Dedup: an event is a duplicate when a row with the identical
// content already exists — same timestamp, gateway key, provider,
// model, token counts, split flag, cost, outcome, latency and
// shared flag. Re-importing the same export therefore inserts
// zero rows, and spend/token totals can never double-count.
// The honest edge: two genuinely distinct real events that agree
// on every one of those fields (same millisecond, same tokens,
// same cost, same latency) are indistinguishable from a re-import,
// so the second is skipped. That collision needs identical
// metadata down to the millisecond and the cent; the alternative —
// keying dedup on the source row id — would be wrong, because row
// ids are local to the source database and two different routers
// both have an id 5.
//
// Atomicity: the whole file is validated BEFORE any row is written,
// and the insert runs in one transaction. A corrupt file — bad
// JSON, wrong format version, one bad event among good ones — is
// rejected with an honest error and zero rows are imported. An
// import is never partial and never silent about it.
//
// Budgets: each newly inserted row's cost is added to its key's
// budgets.spent_usd in the same transaction (only when a budget
// row for that key exists — the same rule RecordUsage follows),
// so the cached spend stays in sync with usage_events. Skipped
// duplicates move no budget, so a re-import can never inflate
// spend.
package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// ExportFormat is the format string of a DB export file
// (internal/server/dbexport.go writes it). The format string IS
// the schema version: a file whose format differs — including a
// future v2 — is rejected, never guessed at.
const ExportFormat = "codvian-router-db-export/v1"

// ExportUsageFile is the usage-history section of an export file,
// as written by the DB export route. Only the fields the import
// needs are modelled; the summary is informational (the import
// re-derives totals from the ledger afterwards) and the config
// section of the file is ignored entirely.
type ExportUsageFile struct {
	Events      []UsageEvent `json:"events"`
	EventsTotal int          `json:"events_total"`
	Truncated   bool         `json:"truncated"`
}

// ExportFile is the whole export file as the import reads it. The
// config section is deliberately absent: importing a config would
// overwrite providers, keys and prices, and this route merges
// usage history only.
type ExportFile struct {
	Format     string          `json:"format"`
	ExportedAt time.Time       `json:"exported_at"`
	Usage      ExportUsageFile `json:"usage"`
}

// dbImportMaxEvents caps one import file: the export route caps
// its own files at 500 events, so a file far above that is either
// hand-built or from a newer writer — either way it is refused
// rather than silently truncated. 10,000 leaves ample headroom
// for a hand-merged file while bounding one request's work.
const dbImportMaxEvents = 10000

// ParseExport validates an export file's bytes and returns the
// parsed file, or an honest error naming the problem. It validates
// the format version, the event-count consistency, and every event
// — so a caller that gets a file back knows the merge itself
// cannot fail on validation.
func ParseExport(data []byte) (*ExportFile, error) {
	var f ExportFile
	// No DisallowUnknownFields: a real export file carries a
	// config section, a secrets note and a note that the import
	// deliberately ignores (usage history only) — unknown fields
	// must be skipped, not rejected.
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("ledger: import: not a valid DB export file: invalid JSON: %v", err)
	}
	if f.Format == "" {
		return nil, fmt.Errorf("ledger: import: not a Codvian Router DB export (missing \"format\")")
	}
	if f.Format != ExportFormat {
		return nil, fmt.Errorf("ledger: import: unsupported export format version %q (want %q)", f.Format, ExportFormat)
	}
	if len(f.Usage.Events) > dbImportMaxEvents {
		return nil, fmt.Errorf("ledger: import: %d events in file exceeds the %d-event import cap", len(f.Usage.Events), dbImportMaxEvents)
	}
	if len(f.Usage.Events) > f.Usage.EventsTotal {
		return nil, fmt.Errorf("ledger: import: corrupt export: %d events in file but events_total is %d", len(f.Usage.Events), f.Usage.EventsTotal)
	}
	for i, e := range f.Usage.Events {
		if err := validateImportEvent(e); err != nil {
			return nil, fmt.Errorf("ledger: import: event %d is corrupt: %w", i, err)
		}
	}
	return &f, nil
}

// validateImportEvent rejects one event that cannot honestly be
// stored: empty provider/model, negative counts, a negative or
// non-finite cost, a negative latency, a missing timestamp, or a
// split flag that contradicts the stored counts.
func validateImportEvent(e UsageEvent) error {
	if strings.TrimSpace(e.Provider) == "" {
		return fmt.Errorf("empty provider")
	}
	if strings.TrimSpace(e.Model) == "" {
		return fmt.Errorf("empty model")
	}
	if e.PromptTokens < 0 || e.CompletionTokens < 0 || e.TotalTokens < 0 {
		return fmt.Errorf("negative token count (prompt %d, completion %d, total %d)", e.PromptTokens, e.CompletionTokens, e.TotalTokens)
	}
	if e.CachedTokens < 0 || e.ReasoningTokens < 0 || e.CacheWriteTokens < 0 {
		return fmt.Errorf("negative token count (cached %d, reasoning %d, cache-write %d)", e.CachedTokens, e.ReasoningTokens, e.CacheWriteTokens)
	}
	// Cache-write tiers (M6.9): never negative, and when either
	// tier is set they must sum to the stored total — the total
	// is the tiers' sum, not a third independent number. Both
	// tiers 0 with a positive total is the legacy unsplit shape:
	// tier unknown, honestly allowed.
	if e.CacheWriteTokens5m < 0 || e.CacheWriteTokens1h < 0 {
		return fmt.Errorf("negative cache-write tier count (5m %d, 1h %d)", e.CacheWriteTokens5m, e.CacheWriteTokens1h)
	}
	if e.CacheWriteTokens5m+e.CacheWriteTokens1h > 0 && e.CacheWriteTokens5m+e.CacheWriteTokens1h != e.CacheWriteTokens {
		return fmt.Errorf("cache-write tiers %d + %d do not sum to cache-write total %d", e.CacheWriteTokens5m, e.CacheWriteTokens1h, e.CacheWriteTokens)
	}
	if e.PromptBeforeCompressionTokens < 0 || e.CompressionSavedTokens < 0 {
		return fmt.Errorf("negative compression count (prompt-before %d, saved %d)", e.PromptBeforeCompressionTokens, e.CompressionSavedTokens)
	}
	// A saving can never exceed the prompt it was saved from, and
	// a before-count of 0 with a positive saving is contradictory.
	if e.CompressionSavedTokens > e.PromptBeforeCompressionTokens {
		return fmt.Errorf("compression saved tokens %d exceed prompt-before-compression tokens %d", e.CompressionSavedTokens, e.PromptBeforeCompressionTokens)
	}
	// Cache-saved tokens (B4-16): never negative, and a saving
	// larger than the call's own token counts is contradictory —
	// the saving can never exceed the call it avoided. Same bound
	// as normalizeUsage clamps to, enforced here as a rejection;
	// a positive saving on a row with no token counts at all is
	// likewise contradictory.
	if e.CacheTokensSaved < 0 {
		return fmt.Errorf("negative cache-saved count %d", e.CacheTokensSaved)
	}
	if e.SplitKnown && e.CacheTokensSaved > e.PromptTokens+e.CompletionTokens {
		return fmt.Errorf("cache-saved tokens %d exceed prompt+completion tokens %d", e.CacheTokensSaved, e.PromptTokens+e.CompletionTokens)
	}
	if !e.SplitKnown && e.TotalTokens == 0 && e.CacheTokensSaved > 0 {
		return fmt.Errorf("cache-saved tokens %d with no token counts", e.CacheTokensSaved)
	}
	if e.SplitKnown {
		// Subset counts can never exceed their parent count.
		if e.CachedTokens > e.PromptTokens {
			return fmt.Errorf("cached tokens %d exceed prompt tokens %d", e.CachedTokens, e.PromptTokens)
		}
		if e.ReasoningTokens > e.CompletionTokens {
			return fmt.Errorf("reasoning tokens %d exceed completion tokens %d", e.ReasoningTokens, e.CompletionTokens)
		}
		if e.CacheWriteTokens > e.PromptTokens {
			return fmt.Errorf("cache-write tokens %d exceed prompt tokens %d", e.CacheWriteTokens, e.PromptTokens)
		}
		// The tiers are subsets of the prompt too — and, with
		// cached reads, the tiered subsets together can never
		// exceed the prompt.
		if e.CacheWriteTokens5m+e.CacheWriteTokens1h+e.CachedTokens > e.PromptTokens {
			return fmt.Errorf("cached + cache-write tier tokens %d exceed prompt tokens %d", e.CachedTokens+e.CacheWriteTokens5m+e.CacheWriteTokens1h, e.PromptTokens)
		}
	}
	if e.CostUSD < 0 || math.IsNaN(e.CostUSD) || math.IsInf(e.CostUSD, 0) {
		return fmt.Errorf("invalid cost %v", e.CostUSD)
	}
	if e.LatencyMS < 0 {
		return fmt.Errorf("negative latency %d ms", e.LatencyMS)
	}
	if e.TS.IsZero() {
		return fmt.Errorf("missing timestamp")
	}
	if !e.SplitKnown && (e.PromptTokens != 0 || e.CompletionTokens != 0) && !e.LocalEstimate {
		return fmt.Errorf("split unknown but prompt/completion counts are non-zero (%d/%d)", e.PromptTokens, e.CompletionTokens)
	}
	if e.LocalEstimate {
		// A local-estimate row is the one split-unknown shape
		// allowed to carry a prompt count: the estimate itself.
		// It must not also claim a known split, a completion
		// count, a cost, or any provider-reported subset count.
		if e.SplitKnown {
			return fmt.Errorf("local estimate with a known split")
		}
		if e.CompletionTokens != 0 {
			return fmt.Errorf("local estimate with completion tokens %d", e.CompletionTokens)
		}
		if e.CostUSD != 0 {
			return fmt.Errorf("local estimate with a cost %v", e.CostUSD)
		}
		if e.CachedTokens != 0 || e.ReasoningTokens != 0 || e.CacheWriteTokens != 0 || e.CacheWriteTokens5m != 0 || e.CacheWriteTokens1h != 0 {
			return fmt.Errorf("local estimate with provider-reported subset counts")
		}
	}
	// Endpoint is "" (chat completions, including exports from
	// before the field existed) or one of the known API
	// surfaces. An unknown value is corrupt: the dashboard
	// renders the field verbatim, so a made-up surface would
	// be presented as real.
	switch e.Endpoint {
	case "", "chat_completions", "embeddings", "images", "rerank", "responses":
	default:
		return fmt.Errorf("unknown endpoint %q", e.Endpoint)
	}
	return nil
}

// ImportEvents merges events into the ledger, skipping exact
// duplicates (see the package doc). It returns how many rows were
// inserted and how many were skipped as duplicates. The whole
// merge is one transaction: any failure rolls everything back, so
// the ledger is never left half-imported. Events are validated
// again here (ParseExport already validated them) so a direct
// caller cannot bypass validation.
func (l *Ledger) ImportEvents(ctx context.Context, events []UsageEvent) (imported, duplicates int, err error) {
	for i, e := range events {
		if err := validateImportEvent(e); err != nil {
			return 0, 0, fmt.Errorf("ledger: import: event %d is corrupt: %w", i, err)
		}
	}
	// importMu serialises imports in-process: the dedup check and
	// the insert must not interleave with another import, or both
	// could decide the same event is new and insert it twice.
	// (RecordUsage paths do not take this lock — SQLite's single
	// writer connection serialises them, and they never dedup.)
	l.importMu.Lock()
	defer l.importMu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("ledger: import: begin tx: %w", err)
	}
	defer tx.Rollback()
	// Prepared once for the whole file: one parse, one plan.
	// The dedup check keys on the unsplit cache_write_tokens
	// total, not the tiers: the tiers refine the total, and two
	// exports of the same event carry the same total — a tier
	// re-split of an identical event is the same event, not a
	// second one. (The insert below still stores the tiers.)
	dup, err := tx.PrepareContext(ctx, `
		SELECT COUNT(*) FROM usage_events
		WHERE ts = ? AND gateway_key_id = ? AND provider = ? AND model = ?
		  AND endpoint = ?
		  AND prompt_tokens = ? AND completion_tokens = ? AND total_tokens = ?
		  AND cached_tokens = ? AND reasoning_tokens = ?
		  AND cache_write_tokens = ? AND local_estimate = ?
		  AND prompt_before_compression_tokens = ? AND compression_saved_tokens = ?
		  AND cache_tokens_saved = ?
		  AND auto_variant = ?
		  AND tokens_split_known = ? AND cost_usd = ? AND ok = ?
		  AND latency_ms = ? AND shared = ?`)
	if err != nil {
		return 0, 0, fmt.Errorf("ledger: import: prepare dedup check: %w", err)
	}
	defer dup.Close()
	ins, err := tx.PrepareContext(ctx, `
			INSERT INTO usage_events
			  (ts, gateway_key_id, provider, model, endpoint, prompt_tokens,
			   completion_tokens, total_tokens, cached_tokens,
			   reasoning_tokens, cache_write_tokens,
			   cache_write_tokens_5m, cache_write_tokens_1h, local_estimate,
			   prompt_before_compression_tokens, compression_saved_tokens, cache_tokens_saved,
			   tokens_split_known,
			   cost_usd, ok, latency_ms, shared, auto_variant,
			   reported_cost_usd, reported_cost_known, cost_delta_usd, cancelled)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, 0, fmt.Errorf("ledger: import: prepare insert: %w", err)
	}
	defer ins.Close()
	upd, err := tx.PrepareContext(ctx, `
		UPDATE budgets SET spent_usd = spent_usd + ? WHERE key_id = ?`)
	if err != nil {
		return 0, 0, fmt.Errorf("ledger: import: prepare budget update: %w", err)
	}
	defer upd.Close()
	for _, e := range events {
		splitKnown, ok, shared, localEstimate := 0, 0, 0, 0
		if e.SplitKnown {
			splitKnown = 1
		}
		if e.OK {
			ok = 1
		}
		if e.Shared {
			shared = 1
		}
		if e.LocalEstimate {
			localEstimate = 1
		}
		cancelled := 0
		if e.Cancelled {
			cancelled = 1
		}
		// Reported-cost reconciliation, mirroring normalizeUsage
		// (recordUsage): a valid upstream-reported cost wins over
		// the hand-computed number, and the delta (reported −
		// computed) is stored with it. An invalid report (nil,
		// negative, NaN, Inf) is ignored — never stored as billing.
		costValue, reportedCost, reportedKnown, costDelta := e.CostUSD, 0.0, 0, 0.0
		if rep := e.ReportedCostUSD; rep != nil && !math.IsNaN(*rep) && !math.IsInf(*rep, 0) && *rep >= 0 {
			reportedCost = *rep
			reportedKnown = 1
			costDelta = *rep - e.CostUSD
			costValue = *rep
		}
		total := e.TotalTokens
		if e.SplitKnown && total == 0 {
			// Same rule as normalizeUsage: a known split with no
			// total stores prompt+completion.
			total = e.PromptTokens + e.CompletionTokens
		}
		if e.LocalEstimate && total == 0 {
			// Same rule as normalizeUsage: a local-estimate row
			// stores the estimate as its total.
			total = e.PromptTokens
		}
		var n int
		if err := dup.QueryRowContext(ctx, e.TS.UnixMilli(), e.GatewayKeyID, e.Provider, e.Model, e.Endpoint,
			e.PromptTokens, e.CompletionTokens, total,
			e.CachedTokens, e.ReasoningTokens, e.CacheWriteTokens, localEstimate,
			e.PromptBeforeCompressionTokens, e.CompressionSavedTokens, e.CacheTokensSaved,
			e.AutoVariant,
			splitKnown,
			e.CostUSD, ok, e.LatencyMS, shared).Scan(&n); err != nil {
			return 0, 0, fmt.Errorf("ledger: import: dedup check: %w", err)
		}
		if n > 0 {
			duplicates++
			continue
		}
		if _, err := ins.ExecContext(ctx, e.TS.UnixMilli(), e.GatewayKeyID, e.Provider, e.Model, e.Endpoint,
			e.PromptTokens, e.CompletionTokens, total,
			e.CachedTokens, e.ReasoningTokens, e.CacheWriteTokens,
			e.CacheWriteTokens5m, e.CacheWriteTokens1h, localEstimate,
			e.PromptBeforeCompressionTokens, e.CompressionSavedTokens, e.CacheTokensSaved,
			splitKnown,
			costValue, ok, e.LatencyMS, shared, e.AutoVariant,
			reportedCost, reportedKnown, costDelta, cancelled); err != nil {
			return 0, 0, fmt.Errorf("ledger: import: insert: %w", err)
		}
		// Keep the cached budget spend in sync, exactly like
		// RecordUsage does — but only for rows actually inserted.
		if _, err := upd.ExecContext(ctx, costValue, e.GatewayKeyID); err != nil {
			return 0, 0, fmt.Errorf("ledger: import: budget update: %w", err)
		}
		imported++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("ledger: import: commit: %w", err)
	}
	return imported, duplicates, nil
}
