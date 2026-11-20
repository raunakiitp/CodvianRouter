package ledger

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseExportValid(t *testing.T) {
	data := `{"format":"codvian-router-db-export/v1","exported_at":"2026-11-18T00:00:00Z","usage":{"events":[{"id":1,"ts":"2026-11-18T00:00:00Z","gateway_key_id":"k1","provider":"openai","model":"gpt-4o-mini","prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"split_known":true,"cost_usd":0.0001,"ok":true,"latency_ms":120,"shared":false}],"events_total":1,"events_included":1,"truncated":false}}`
	f, err := ParseExport([]byte(data))
	if err != nil {
		t.Fatalf("ParseExport: %v", err)
	}
	if len(f.Usage.Events) != 1 || f.Usage.Events[0].Provider != "openai" {
		t.Fatalf("ParseExport events = %+v, want the one openai event", f.Usage.Events)
	}
	if f.Usage.Events[0].TS.IsZero() {
		t.Errorf("ParseExport dropped the event timestamp")
	}
}

func TestParseExportRejects(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"not json", `{oops`, "invalid JSON"},
		{"missing format", `{"usage":{"events":[],"events_total":0}}`, "missing \"format\""},
		{"wrong version", `{"format":"codvian-router-db-export/v2","usage":{"events":[],"events_total":0}}`, "unsupported export format version"},
		{"other format", `{"format":"something-else","usage":{"events":[],"events_total":0}}`, "unsupported export format version"},
		{"events exceed total", `{"format":"codvian-router-db-export/v1","usage":{"events":[{"ts":"2026-11-18T00:00:00Z","provider":"p","model":"m","prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"split_known":true,"cost_usd":0,"ok":true,"latency_ms":1}],"events_total":0}}`, "events_total is 0"},
		{"negative tokens", `{"format":"codvian-router-db-export/v1","usage":{"events":[{"ts":"2026-11-18T00:00:00Z","provider":"p","model":"m","prompt_tokens":-1,"completion_tokens":0,"total_tokens":0,"split_known":true,"cost_usd":0,"ok":true,"latency_ms":1}],"events_total":1}}`, "negative token count"},
		{"negative cost", `{"format":"codvian-router-db-export/v1","usage":{"events":[{"ts":"2026-11-18T00:00:00Z","provider":"p","model":"m","prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"split_known":true,"cost_usd":-0.5,"ok":true,"latency_ms":1}],"events_total":1}}`, "invalid cost"},
		{"missing timestamp", `{"format":"codvian-router-db-export/v1","usage":{"events":[{"provider":"p","model":"m","prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"split_known":true,"cost_usd":0,"ok":true,"latency_ms":1}],"events_total":1}}`, "missing timestamp"},
		{"empty provider", `{"format":"codvian-router-db-export/v1","usage":{"events":[{"ts":"2026-11-18T00:00:00Z","provider":"","model":"m","prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"split_known":true,"cost_usd":0,"ok":true,"latency_ms":1}],"events_total":1}}`, "empty provider"},
		{"split contradicts counts", `{"format":"codvian-router-db-export/v1","usage":{"events":[{"ts":"2026-11-18T00:00:00Z","provider":"p","model":"m","prompt_tokens":5,"completion_tokens":0,"total_tokens":5,"split_known":false,"cost_usd":0,"ok":true,"latency_ms":1}],"events_total":1}}`, "split unknown"},
	}
	for _, c := range cases {
		if _, err := ParseExport([]byte(c.data)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
}

func importTestLedger(t *testing.T) *Ledger {
	t.Helper()
	led, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	return led
}

func importTestEvent(ts time.Time, provider, model string, prompt, completion int, cost float64, ok bool) UsageEvent {
	return UsageEvent{
		TS: ts, GatewayKeyID: "k1", Provider: provider, Model: model,
		PromptTokens: prompt, CompletionTokens: completion,
		TotalTokens: prompt + completion, SplitKnown: true,
		CostUSD: cost, OK: ok, LatencyMS: 42,
	}
}

// TestImportEventsMergesPreservingHistory: existing rows are
// untouched, new events are added with their original timestamps,
// and the summary totals are the union.
func TestImportEventsMergesPreservingHistory(t *testing.T) {
	ctx := context.Background()
	led := importTestLedger(t)
	if _, err := led.RecordUsage(ctx, Usage{GatewayKeyID: "k1", Provider: "openai", Model: "gpt-4o-mini", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, SplitKnown: true, OK: true, LatencyMS: 100}, Prices{}); err != nil {
		t.Fatal(err)
	}
	incoming := []UsageEvent{
		importTestEvent(time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), "openai", "gpt-4o-mini", 20, 8, 0, true),
		importTestEvent(time.Date(2026, 11, 18, 9, 1, 0, 0, time.UTC), "anthropic", "claude-sonnet-4-5", 30, 12, 0, false),
	}
	imported, dups, err := led.ImportEvents(ctx, incoming)
	if err != nil {
		t.Fatalf("ImportEvents: %v", err)
	}
	if imported != 2 || dups != 0 {
		t.Fatalf("ImportEvents = %d imported, %d dups; want 2/0", imported, dups)
	}
	sum, err := led.Summary(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 3 || sum.OK != 2 || sum.Errors != 1 {
		t.Errorf("summary after merge = %+v, want 3 requests (2 ok, 1 error)", sum)
	}
	if sum.TotalTokens != 15+28+42 {
		t.Errorf("total tokens after merge = %d, want %d", sum.TotalTokens, 15+28+42)
	}
	events, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Provider == "anthropic" && e.TS.Equal(time.Date(2026, 11, 18, 9, 1, 0, 0, time.UTC)) {
			found = true
		}
	}
	if !found {
		t.Errorf("imported event does not keep its original timestamp: %+v", events)
	}
}

// TestImportEventsSkipsDuplicates: importing the same file twice
// inserts zero rows the second time, and totals never double-count.
func TestImportEventsSkipsDuplicates(t *testing.T) {
	ctx := context.Background()
	led := importTestLedger(t)
	events := []UsageEvent{
		importTestEvent(time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), "openai", "gpt-4o-mini", 20, 8, 0, true),
		importTestEvent(time.Date(2026, 11, 18, 9, 1, 0, 0, time.UTC), "openai", "gpt-4o-mini", 5, 5, 0, true),
	}
	if n, d, err := led.ImportEvents(ctx, events); err != nil || n != 2 || d != 0 {
		t.Fatalf("first import = %d/%d, err %v; want 2/0", n, d, err)
	}
	if n, d, err := led.ImportEvents(ctx, events); err != nil || n != 0 || d != 2 {
		t.Fatalf("second import = %d/%d, err %v; want 0/2 (nothing double-counted)", n, d, err)
	}
	sum, err := led.Summary(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 2 || sum.TotalTokens != 28+10 {
		t.Errorf("summary after re-import = %+v, want 2 requests / 38 tokens", sum)
	}
}

// TestImportEventsAllOrNothing: one corrupt event among good ones
// rejects the whole file — zero rows are imported.
func TestImportEventsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	led := importTestLedger(t)
	good := importTestEvent(time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), "openai", "gpt-4o-mini", 20, 8, 0, true)
	bad := importTestEvent(time.Date(2026, 11, 18, 9, 1, 0, 0, time.UTC), "openai", "gpt-4o-mini", -3, 0, 0, true)
	if _, _, err := led.ImportEvents(ctx, []UsageEvent{good, bad}); err == nil {
		t.Fatal("ImportEvents accepted a file with a corrupt event")
	}
	sum, err := led.Summary(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 0 {
		t.Errorf("ledger holds %d request(s) after a rejected import, want 0 (never partial)", sum.Requests)
	}
}

// TestImportEventsBudgetSync: an imported cost moves the key's
// cached budget spend, but only for rows actually inserted — a
// re-import moves nothing.
func TestImportEventsBudgetSync(t *testing.T) {
	ctx := context.Background()
	led := importTestLedger(t)
	if err := led.SetBudget(ctx, "k1", 10, "test"); err != nil {
		t.Fatal(err)
	}
	// Prices are not consulted on import: the cost in the file is
	// the cost recorded (it was computed by the source router from
	// its own prices). A $0.01 event must move the budget $0.01.
	ev := importTestEvent(time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), "openai", "gpt-4o-mini", 100, 50, 0.01, true)
	if n, _, err := led.ImportEvents(ctx, []UsageEvent{ev}); err != nil || n != 1 {
		t.Fatalf("import = %d, err %v; want 1", n, err)
	}
	b, err := led.GetBudget(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if b.SpentUSD != 0.01 {
		t.Errorf("budget spent after import = %v, want 0.01", b.SpentUSD)
	}
	if n, d, err := led.ImportEvents(ctx, []UsageEvent{ev}); err != nil || n != 0 || d != 1 {
		t.Fatalf("re-import = %d/%d, err %v; want 0/1", n, d, err)
	}
	b, err = led.GetBudget(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if b.SpentUSD != 0.01 {
		t.Errorf("budget spent after re-import = %v, want still 0.01 (duplicates move no budget)", b.SpentUSD)
	}
}

// TestImportEventsSharedRow: a shared (coalesced-waiter) event
// imports with its shared flag intact and zero cost, like a
// locally recorded shared row.
func TestImportEventsSharedRow(t *testing.T) {
	ctx := context.Background()
	led := importTestLedger(t)
	ev := importTestEvent(time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), "openai", "gpt-4o-mini", 0, 0, 0, true)
	ev.TotalTokens = 0
	ev.SplitKnown = false
	ev.Shared = true
	if n, _, err := led.ImportEvents(ctx, []UsageEvent{ev}); err != nil || n != 1 {
		t.Fatalf("import = %d, err %v; want 1", n, err)
	}
	events, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].Shared || events[0].CostUSD != 0 {
		t.Errorf("imported shared row = %+v, want shared=true cost=0", events)
	}
}
