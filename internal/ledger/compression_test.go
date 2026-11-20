package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Compression record storage (CHECKLIST 183-184): prompt tokens
// before compression and tokens saved ride the ledger row
// alongside the prompt as sent, so the dashboard can total the
// saving honestly. Saved tokens are local estimates, never
// provider-reported; failed and shared rows carry none, so each
// request is counted once.
func TestCompressionStorage(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()

	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "openai", Model: "gpt-4o",
		PromptTokens: 14, CompletionTokens: 3, TotalTokens: 17,
		PromptBeforeCompression: 25, CompressionSavedTokens: 11,
		SplitKnown: true, OK: true, LatencyMS: 100,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// No compression on this one: honest zeros, not a saving.
	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "openai", Model: "gpt-4o",
		PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12,
		SplitKnown: true, OK: true, LatencyMS: 50,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// A failed attempt's row keeps the counts the caller passed
	// (like its prompt tokens — the attempt happened), but the
	// Summary totals count successful attempts only, so it never
	// inflates the saving. In practice the engine never attaches
	// compression to a failed attempt at all (noteFailure records
	// Usage{}), so this row only exists when a caller passes the
	// fields directly.
	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "openai", Model: "gpt-4o",
		PromptTokens: 20, CompletionTokens: 0, TotalTokens: 20,
		PromptBeforeCompression: 40, CompressionSavedTokens: 20,
		SplitKnown: true, OK: false, LatencyMS: 30,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	events, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("RecentEvents = %d rows, want 3", len(events))
	}
	// Newest first: failed row first, then the plain row, then
	// the compressed row. The failed row keeps its counts
	// verbatim (see above); the plain row has honest zeros.
	if got := events[0]; got.CompressionSavedTokens != 20 || got.PromptBeforeCompressionTokens != 40 {
		t.Errorf("failed row = %+v, want before=40 saved=20 stored verbatim", got)
	}
	if got := events[1]; got.CompressionSavedTokens != 0 || got.PromptBeforeCompressionTokens != 0 {
		t.Errorf("plain row = %+v, want compression zeroed", got)
	}
	comp := events[2]
	if comp.PromptBeforeCompressionTokens != 25 || comp.CompressionSavedTokens != 11 {
		t.Errorf("compressed row = %+v, want before=25 saved=11", comp)
	}

	sum, err := led.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.CompressionRequests != 1 || sum.CompressionTokensSaved != 11 {
		t.Errorf("Summary compression = requests %d saved %d, want 1 / 11",
			sum.CompressionRequests, sum.CompressionTokensSaved)
	}
}

func TestCompressionStorageLocalEstimate(t *testing.T) {
	// Regression (found by the live smoke, not the unit tests):
	// a local-estimate row — the upstream reported no usage, or
	// a successful stream whose usage the engine counted locally
	// — used to lose its compression record, because
	// normalizeUsage only stored it alongside a known split.
	// The prompt count IS stored for these rows, and the saving
	// is measured against that same local count, so the record
	// must be stored too.
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()

	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "openai", Model: "gpt-4o",
		PromptTokens: 14, CompletionTokens: 0, TotalTokens: 14,
		PromptBeforeCompression: 25, CompressionSavedTokens: 11,
		LocalEstimate: true, OK: true, LatencyMS: 100,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// A split-unknown row with no local estimate and no
	// compression still stores honest zeros.
	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "openai", Model: "gpt-4o",
		TotalTokens: 8, OK: true, LatencyMS: 50,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	events, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("rows = %d, want 2", len(events))
	}
	if got := events[1]; !got.LocalEstimate || got.PromptBeforeCompressionTokens != 25 || got.CompressionSavedTokens != 11 {
		t.Errorf("estimate row = %+v, want LocalEstimate=true before=25 saved=11", got)
	}
	if got := events[0]; got.CompressionSavedTokens != 0 || got.PromptBeforeCompressionTokens != 0 {
		t.Errorf("unknown-split row = %+v, want compression zeroed", got)
	}
	sum, err := led.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.CompressionRequests != 1 || sum.CompressionTokensSaved != 11 {
		t.Errorf("Summary compression = requests %d saved %d, want 1 / 11",
			sum.CompressionRequests, sum.CompressionTokensSaved)
	}
}

func TestCompressionStorageClamping(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()

	// A malformed record claiming more saved than the prompt had
	// clamps to the before-count, never a negative prompt.
	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "x", Model: "m",
		PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6,
		PromptBeforeCompression: 10, CompressionSavedTokens: 99,
		SplitKnown: true, OK: true, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// A before-count of 0 with a positive saving is
	// contradictory — the saving is zeroed, not trusted.
	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "x", Model: "m",
		PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6,
		PromptBeforeCompression: 0, CompressionSavedTokens: 5,
		SplitKnown: true, OK: true, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// Negative counts clamp to 0.
	if _, err := led.RecordUsage(ctx, Usage{
		Provider: "x", Model: "m",
		PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6,
		PromptBeforeCompression: -3, CompressionSavedTokens: -2,
		SplitKnown: true, OK: true, LatencyMS: 10,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	events, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("rows = %d, want 3", len(events))
	}
	// Newest first: negative row, zero-before row, clamped row.
	if got := events[0]; got.PromptBeforeCompressionTokens != 0 || got.CompressionSavedTokens != 0 {
		t.Errorf("negative row = %+v, want before=0 saved=0", got)
	}
	if got := events[1]; got.PromptBeforeCompressionTokens != 0 || got.CompressionSavedTokens != 0 {
		t.Errorf("zero-before row = %+v, want before=0 saved=0", got)
	}
	if got := events[2]; got.PromptBeforeCompressionTokens != 10 || got.CompressionSavedTokens != 10 {
		t.Errorf("clamped row = %+v, want before=10 saved=10", got)
	}
}

func TestAsyncWriterCarriesCompression(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	w := NewAsyncWriter(led, nil, AsyncWriterConfig{})
	defer w.Close()
	w.Record(Usage{
		Provider: "openai", Model: "gpt-4o",
		PromptTokens: 14, CompletionTokens: 3, TotalTokens: 17,
		PromptBeforeCompression: 25, CompressionSavedTokens: 11,
		SplitKnown: true, OK: true, LatencyMS: 10,
	})
	w.Flush()
	events, err := led.RecentEvents(context.Background(), 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("rows = %d, want 1", len(events))
	}
	if got := events[0]; got.PromptBeforeCompressionTokens != 25 || got.CompressionSavedTokens != 11 {
		t.Errorf("async row = %+v, want before=25 saved=11", got)
	}
	sum, err := led.Summary(context.Background(), time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.CompressionRequests != 1 || sum.CompressionTokensSaved != 11 {
		t.Errorf("Summary compression = requests %d saved %d, want 1 / 11",
			sum.CompressionRequests, sum.CompressionTokensSaved)
	}
}

// Export -> import round-trips the compression record, and a
// re-import inserts zero rows (dedup covers the new columns).
func TestImportRoundTripsCompression(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	events := []UsageEvent{
		{TS: now, GatewayKeyID: "k", Provider: "openai", Model: "gpt-4o",
			PromptTokens: 14, CompletionTokens: 3, TotalTokens: 17,
			PromptBeforeCompressionTokens: 25, CompressionSavedTokens: 11,
			SplitKnown: true, OK: true, LatencyMS: 10},
		{TS: now, GatewayKeyID: "k", Provider: "openai", Model: "gpt-4o",
			PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12,
			SplitKnown: true, OK: true, LatencyMS: 5},
	}
	imported, dups, err := led.ImportEvents(ctx, events)
	if err != nil {
		t.Fatalf("ImportEvents: %v", err)
	}
	if imported != 2 || dups != 0 {
		t.Fatalf("imported=%d dups=%d, want 2/0", imported, dups)
	}
	imported, dups, err = led.ImportEvents(ctx, events)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if imported != 0 || dups != 2 {
		t.Fatalf("re-import imported=%d dups=%d, want 0/2", imported, dups)
	}
	got, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	bySaved := map[int]UsageEvent{}
	for _, e := range got {
		bySaved[e.CompressionSavedTokens] = e
	}
	if bySaved[11].PromptBeforeCompressionTokens != 25 {
		t.Errorf("compressed row after import = %+v, want before=25 saved=11", bySaved[11])
	}
	if _, ok := bySaved[0]; !ok {
		t.Errorf("plain row after import missing: %v", got)
	}
}

// Import validation rejects compression contradictions: a saving
// larger than the prompt before compression, and negative counts.
func TestImportValidatesCompression(t *testing.T) {
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	bad := []UsageEvent{
		{TS: now, Provider: "x", Model: "m", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			PromptBeforeCompressionTokens: 10, CompressionSavedTokens: 11, SplitKnown: true, OK: true},
		{TS: now, Provider: "x", Model: "m", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			PromptBeforeCompressionTokens: -1, CompressionSavedTokens: 0, SplitKnown: true, OK: true},
		{TS: now, Provider: "x", Model: "m", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			PromptBeforeCompressionTokens: 10, CompressionSavedTokens: -1, SplitKnown: true, OK: true},
		{TS: now, Provider: "x", Model: "m", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			PromptBeforeCompressionTokens: 0, CompressionSavedTokens: 5, SplitKnown: true, OK: true},
	}
	for i, e := range bad {
		if _, _, err := led.ImportEvents(ctx, []UsageEvent{e}); err == nil {
			t.Errorf("event %d (%+v): import accepted, want rejection", i, e)
		}
	}
	got, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("rows after rejected imports = %d, want 0 (never partial)", len(got))
	}
}
