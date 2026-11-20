// Ledger endpoint column tests (CHECKLIST 171-172): the
// endpoint is stored on every usage row, survives migration from
// a pre-column database, and round-trips through export/import
// (including the dedup fingerprint — a chat row and an embeddings
// row for the same model are different rows).
package ledger

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func endpointTestPrices() Prices {
	return Prices{{Model: "emb-model"}: {PromptPerToken: 0.0001, CompletionPerToken: 0}}
}

func TestLedgerEndpointRoundTrip(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := endpointTestPrices()

	// An embeddings row: priced from the book (8 prompt tokens
	// at $0.0001 = $0.0008), never a $0 claim on a priced call.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "emb-model", Endpoint: "embeddings",
		PromptTokens: 8, CompletionTokens: 0, SplitKnown: true, OK: true, LatencyMS: 42,
	}, prices); err != nil {
		t.Fatalf("RecordUsage embeddings: %v", err)
	}
	// A chat row for the same model: endpoint "" (the chat
	// default), recorded alongside.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "emb-model",
		PromptTokens: 10, CompletionTokens: 5, SplitKnown: true, OK: true, LatencyMS: 100,
	}, prices); err != nil {
		t.Fatalf("RecordUsage chat: %v", err)
	}

	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	// Newest first: the chat row, then the embeddings row.
	chat, emb := events[0], events[1]
	if chat.Endpoint != "" {
		t.Errorf("chat endpoint = %q, want \"\" (chat)", chat.Endpoint)
	}
	if emb.Endpoint != "embeddings" {
		t.Errorf("embeddings endpoint = %q, want embeddings", emb.Endpoint)
	}
	if emb.CostUSD != 0.0008 {
		t.Errorf("embeddings cost = %v, want 0.0008 (8 x $0.0001)", emb.CostUSD)
	}
	if emb.PromptTokens != 8 || emb.TotalTokens != 8 {
		t.Errorf("embeddings tokens = %d/%d, want 8/8", emb.PromptTokens, emb.TotalTokens)
	}
}

// An unknown price stays unpriced: the embeddings row records
// its tokens with cost 0 and price_known false — never a $0
// claim. (price_known is not on UsageEvent; the cost-0 +
// token-carrying row is the observable half — the priced half
// is covered by TestLedgerEndpointRoundTrip above.)
func TestLedgerEndpointUnpricedModel(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "unknown-emb", Endpoint: "embeddings",
		PromptTokens: 8, SplitKnown: true, OK: true,
	}, Prices{})
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0 (unknown price is unpriced, not $0-claimed)", cost)
	}
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 1 || events[0].Endpoint != "embeddings" || events[0].PromptTokens != 8 {
		t.Fatalf("events = %+v, want one embeddings row with 8 prompt tokens", events)
	}
}

func TestLedgerEndpointMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old-ledger.db")
	// A pre-endpoint database: the old schema has no endpoint
	// column (and none of the newer columns either).
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	oldSchema := `CREATE TABLE usage_events (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                INTEGER NOT NULL,
  gateway_key_id    TEXT NOT NULL,
  provider          TEXT NOT NULL,
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd          REAL NOT NULL DEFAULT 0,
  ok                INTEGER NOT NULL DEFAULT 1,
  latency_ms        INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE budgets (
  key_id   TEXT PRIMARY KEY,
  cap_usd  REAL NOT NULL,
  spent_usd REAL NOT NULL DEFAULT 0,
  period   TEXT NOT NULL DEFAULT ''
);`
	if _, err := db.Exec(oldSchema); err != nil {
		db.Close()
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO usage_events (ts, gateway_key_id, provider, model, prompt_tokens, completion_tokens, cost_usd, ok, latency_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), "key-1", "p1", "old-model", 10, 5, 0, 1, 50); err != nil {
		db.Close()
		t.Fatalf("insert old row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open old db: %v", err)
	}
	defer l.Close()
	cols := usageColumns(t, l)
	if !cols["endpoint"] {
		t.Fatalf("migrated columns = %v, want the endpoint column", cols)
	}
	// The pre-migration row reads back as chat ("") — chat was
	// the only surface when it was recorded, so "" is the honest
	// value, not a guess.
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 (the pre-migration row)", len(events))
	}
	if events[0].Endpoint != "" || events[0].Model != "old-model" {
		t.Errorf("old row = %+v, want model old-model with endpoint \"\"", events[0])
	}
	// And a new embeddings row records against the migrated DB.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "emb-model", Endpoint: "embeddings",
		PromptTokens: 4, SplitKnown: true, OK: true,
	}, endpointTestPrices()); err != nil {
		t.Fatalf("RecordUsage after migration: %v", err)
	}
}

func TestImportEndpointRoundTripAndDedup(t *testing.T) {
	ctx := context.Background()
	led := importTestLedger(t)
	ts := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

	emb := importTestEvent(ts, "openai", "emb-model", 8, 0, 0, true)
	emb.Endpoint = "embeddings"
	chat := importTestEvent(ts, "openai", "emb-model", 8, 0, 0, true)
	// Same timestamp, provider, model, tokens — but a chat row
	// (endpoint ""): the dedup fingerprint includes the endpoint,
	// so this is a DIFFERENT row, not a duplicate.
	if n, d, err := led.ImportEvents(ctx, []UsageEvent{emb, chat}); err != nil || n != 2 || d != 0 {
		t.Fatalf("import = %d/%d, err %v; want 2/0", n, d, err)
	}
	// Re-importing the same two rows inserts nothing.
	if n, d, err := led.ImportEvents(ctx, []UsageEvent{emb, chat}); err != nil || n != 0 || d != 2 {
		t.Fatalf("re-import = %d/%d, err %v; want 0/2", n, d, err)
	}
	events, err := led.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Endpoint] = true
	}
	if !seen["embeddings"] || !seen[""] {
		t.Errorf("imported endpoints = %v, want both embeddings and chat", seen)
	}
}

func TestParseExportRejectsUnknownEndpoint(t *testing.T) {
	data := `{"format":"codvian-router-db-export/v1","exported_at":"2026-11-18T00:00:00Z","usage":{"events":[{"id":1,"ts":"2026-11-18T00:00:00Z","gateway_key_id":"k1","provider":"openai","model":"m","endpoint":"teleport","prompt_tokens":1,"completion_tokens":0,"total_tokens":1,"split_known":true,"cost_usd":0,"ok":true,"latency_ms":1,"shared":false}],"events_total":1,"events_included":1,"truncated":false}}`
	if _, err := ParseExport([]byte(data)); err == nil {
		t.Error("want ParseExport to reject an unknown endpoint")
	}
}
