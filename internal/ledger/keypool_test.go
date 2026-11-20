package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Multi-key pooling ledger tests (CHECKLIST 175): per-key spend is
// attributed to the key that served — never pooled into one number
// for the provider — and a pre-pooling database migrates honestly.

func TestProviderKeySpendAttribution(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}
	record := func(keyID string, ok bool, prompt, completion int) {
		t.Helper()
		if _, err := l.RecordUsage(ctx, Usage{
			GatewayKeyID: "gw-1", ProviderKeyID: keyID, Provider: "p1", Model: "model-a",
			PromptTokens: prompt, CompletionTokens: completion, SplitKnown: true, OK: ok, LatencyMS: 10,
		}, prices); err != nil {
			t.Fatalf("RecordUsage %s: %v", keyID, err)
		}
	}
	// key_1 serves twice (once failed — its row carries tokens,
	// so it still cost them, exactly as SpendFor counts it);
	// key_2 serves once; the legacy "" row is pre-pooling history.
	record("key_1", true, 100, 50) // 0.20
	record("key_1", false, 10, 10) // 0.03
	record("key_2", true, 200, 0)  // 0.20
	record("", true, 50, 50)       // 0.15, unattributed

	totals, err := l.ProviderKeySpendTotals(ctx)
	if err != nil {
		t.Fatalf("ProviderKeySpendTotals: %v", err)
	}
	byKey := map[string]ProviderKeySpend{}
	for _, sp := range totals {
		if sp.Provider != "p1" {
			t.Fatalf("provider = %q, want p1", sp.Provider)
		}
		byKey[sp.ProviderKeyID] = sp
	}
	k1, ok := byKey["key_1"]
	if !ok {
		t.Fatalf("no totals for key_1: %+v", totals)
	}
	if k1.Requests != 2 || k1.Successes != 1 || k1.Tokens != 170 || !almostEqualUSD(k1.CostUSD, 0.23) {
		t.Fatalf("key_1 = %+v, want requests 2, successes 1, tokens 170, cost 0.23", k1)
	}
	if k1.LastUsed == nil {
		t.Fatal("key_1 LastUsed is nil, want the newest row's time")
	}
	k2, ok := byKey["key_2"]
	if !ok {
		t.Fatalf("no totals for key_2: %+v", totals)
	}
	if k2.Requests != 1 || k2.Successes != 1 || k2.Tokens != 200 || !almostEqualUSD(k2.CostUSD, 0.20) {
		t.Fatalf("key_2 = %+v, want requests 1, successes 1, tokens 200, cost 0.20", k2)
	}
	// The unattributed "" group exists but belongs to no key: a
	// dashboard keyed lookup never attributes it to key_1/key_2.
	legacy, ok := byKey[""]
	if !ok {
		t.Fatalf("no totals for the unattributed group: %+v", totals)
	}
	if legacy.Requests != 1 || legacy.Tokens != 100 || !almostEqualUSD(legacy.CostUSD, 0.15) {
		t.Fatalf("unattributed = %+v, want requests 1, tokens 100, cost 0.15", legacy)
	}
	// The per-key costs sum to the provider's total spend — never
	// more, never less (no double-counting, no pooling loss).
	var sum float64
	for _, sp := range totals {
		sum += sp.CostUSD
	}
	if !almostEqualUSD(sum, 0.58) {
		t.Fatalf("total cost = %v, want 0.58", sum)
	}
}

// almostEqualUSD compares USD totals within a cent of a cent:
// per-token prices are float64, so 50*0.001 + 50*0.002 lands at
// 0.15000000000000002, not 0.15 — the difference is representation,
// never a billing error.
func almostEqualUSD(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

func TestProviderKeySpendEmptyLedger(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	totals, err := l.ProviderKeySpendTotals(ctx)
	if err != nil {
		t.Fatalf("ProviderKeySpendTotals: %v", err)
	}
	if len(totals) != 0 {
		t.Fatalf("totals = %+v, want empty (a key with no rows is absent — the dashboard reports 'no usage yet' from the pool list)", totals)
	}
}

// TestProviderKeyColumnMigration opens a database whose
// usage_events table predates the provider_key_id column and
// proves the migration adds it with the honest default (”) — old
// rows stay unattributed, never retroactively assigned to a key —
// and new rows then record their key.
func TestProviderKeyColumnMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	// A pre-pooling database: the schema without provider_key_id.
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := l.db.Exec(`DROP TABLE usage_events`); err != nil {
		t.Fatalf("drop usage_events: %v", err)
	}
	if _, err := l.db.Exec(`
		CREATE TABLE usage_events (
		  id INTEGER PRIMARY KEY AUTOINCREMENT,
		  ts INTEGER NOT NULL,
		  gateway_key_id TEXT NOT NULL,
		  provider TEXT NOT NULL,
		  model TEXT NOT NULL,
		  prompt_tokens INTEGER NOT NULL DEFAULT 0,
		  completion_tokens INTEGER NOT NULL DEFAULT 0,
		  total_tokens INTEGER NOT NULL DEFAULT 0,
		  cached_tokens INTEGER NOT NULL DEFAULT 0,
		  reasoning_tokens INTEGER NOT NULL DEFAULT 0,
		  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
		  prompt_before_compression_tokens INTEGER NOT NULL DEFAULT 0,
		  compression_saved_tokens INTEGER NOT NULL DEFAULT 0,
		  local_estimate INTEGER NOT NULL DEFAULT 0,
		  tokens_split_known INTEGER NOT NULL DEFAULT 1,
		  endpoint TEXT NOT NULL DEFAULT '',
		  price_known INTEGER NOT NULL DEFAULT 0,
		  cost_usd REAL NOT NULL DEFAULT 0,
		  ok INTEGER NOT NULL DEFAULT 1,
		  latency_ms INTEGER NOT NULL DEFAULT 0,
		  shared INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
		t.Fatalf("create old usage_events: %v", err)
	}
	if _, err := l.db.Exec(`INSERT INTO usage_events (ts, gateway_key_id, provider, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, ok) VALUES (?, 'gw-1', 'p1', 'model-a', 10, 10, 20, 0.03, 1)`, time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	l.Close()
	// Reopen: ensureUsageColumns must add provider_key_id.
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	var colDefault string
	if err := l2.db.QueryRow(`SELECT dflt_value FROM pragma_table_info('usage_events') WHERE name = 'provider_key_id'`).Scan(&colDefault); err != nil {
		t.Fatalf("provider_key_id column missing after migration: %v", err)
	}
	if colDefault != "''" {
		t.Fatalf("provider_key_id default = %q, want '' (old rows stay unattributed)", colDefault)
	}
	// The old row is still there, under the empty key.
	totals, err := l2.ProviderKeySpendTotals(ctx)
	if err != nil {
		t.Fatalf("ProviderKeySpendTotals: %v", err)
	}
	if len(totals) != 1 || totals[0].ProviderKeyID != "" || totals[0].Requests != 1 {
		t.Fatalf("totals = %+v, want exactly one unattributed row", totals)
	}
	// And a new row records its key.
	if _, err := l2.RecordUsage(ctx, Usage{
		GatewayKeyID: "gw-1", ProviderKeyID: "key_9", Provider: "p1", Model: "model-a",
		PromptTokens: 5, CompletionTokens: 5, SplitKnown: true, OK: true, LatencyMS: 5,
	}, Prices{}); err != nil {
		t.Fatalf("RecordUsage after migration: %v", err)
	}
	totals, err = l2.ProviderKeySpendTotals(ctx)
	if err != nil {
		t.Fatalf("ProviderKeySpendTotals: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("totals = %+v, want the unattributed row plus key_9", totals)
	}
}
