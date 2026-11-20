package ledger

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// Cache-write tier split (M6.9): Anthropic's cache_creation_input_tokens
// splits into ephemeral_5m / ephemeral_1h tiers, priced at 1.25x / 2x
// the input price. These tests pin the exact arithmetic, the legacy
// unsplit path, the migration, and the window aggregates.

// anthropicTierPrices is the golden-test price book: Claude Sonnet
// list prices in USD per token — $3/1M input, $15/1M output,
// $0.30/1M cached reads — with NO explicit tier rates configured,
// so the 1.25x / 2x documented multiples are what prices the tiers.
func anthropicTierPrices() Prices {
	return Prices{
		{Provider: "anthropic", Model: "claude-sonnet-4-5"}: {
			PromptPerToken:       3e-6,
			CompletionPerToken:   15e-6,
			CachedPromptPerToken: 0.30e-6,
			CachedKnown:          true,
		},
	}
}

func TestCostWithCacheTiersGolden(t *testing.T) {
	prices := anthropicTierPrices()
	// Prompt 1000 = 600 regular + 100 cached reads + 100 5m writes
	// + 200 1h writes; completion 100.
	//   600 x $3.00 = $0.001800
	//   100 x $0.30 = $0.000030 (cached reads)
	//   100 x $3.75 = $0.000375 (5m writes at 1.25x input)
	//   200 x $6.00 = $0.001200 (1h writes at 2x input)
	//   100 x $15.00 = $0.001500 (completion)
	//   total = $0.004905 — exact, not a range.
	got := prices.CostWithCacheTiers("anthropic", "claude-sonnet-4-5", 1000, 100, 100, 100, 200)
	want := 0.004905
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("CostWithCacheTiers = %v, want %v", got, want)
	}
	// Pure-tier case, no reads or completion: prompt 1000 with
	// 100 5m + 200 1h writes.
	//   700 x $3.00 + 100 x $3.75 + 200 x $6.00 = $0.003675.
	got = prices.CostWithCacheTiers("anthropic", "claude-sonnet-4-5", 1000, 0, 0, 100, 200)
	if want := 0.003675; math.Abs(got-want) > 1e-12 {
		t.Fatalf("CostWithCacheTiers pure-tier = %v, want %v", got, want)
	}
}

func TestCostWithCacheTiersExplicitRateWins(t *testing.T) {
	prices := Prices{
		{Provider: "anthropic", Model: "claude-sonnet-4-5"}: {
			PromptPerToken:       3e-6,
			CompletionPerToken:   15e-6,
			CacheWrite5mPerToken: 5e-6,
			CacheWrite5mKnown:    true,
			CacheWrite1hPerToken: 9e-6,
			CacheWrite1hKnown:    true,
		},
	}
	// 700 regular x $3 + 100 x $5 (explicit 5m) + 200 x $9 (explicit 1h).
	got := prices.CostWithCacheTiers("anthropic", "claude-sonnet-4-5", 1000, 0, 0, 100, 200)
	want := (700*3e-6 + 100*5e-6 + 200*9e-6)
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("CostWithCacheTiers explicit = %v, want %v", got, want)
	}
}

func TestCostWithCacheTiersUnknownProviderFallsBackToInput(t *testing.T) {
	// No Anthropic gate, no explicit tier rate: the tiers price at
	// the plain input rate — exactly the legacy CostWithCached
	// number, never a guessed multiple.
	prices := Prices{
		{Provider: "deepseek", Model: "deepseek-chat"}: {PromptPerToken: 3e-6, CompletionPerToken: 15e-6},
		// OpenRouter serving Claude is NOT the Anthropic direct
		// tariff either: same 1x fallback.
		{Provider: "openrouter", Model: "claude-sonnet-4-5"}: {PromptPerToken: 3e-6, CompletionPerToken: 15e-6},
	}
	for _, tc := range []struct{ provider, model string }{
		{"deepseek", "deepseek-chat"},
		{"openrouter", "claude-sonnet-4-5"},
	} {
		got := prices.CostWithCacheTiers(tc.provider, tc.model, 1000, 0, 0, 100, 200)
		want := prices.CostWithCached(tc.provider, tc.model, 1000, 0, 0)
		if math.Abs(got-want) > 1e-12 {
			t.Fatalf("%s/%s CostWithCacheTiers = %v, want legacy %v", tc.provider, tc.model, got, want)
		}
		if got != 1000*3e-6 {
			t.Fatalf("%s/%s CostWithCacheTiers = %v, want 1000 x $3/1M = %v", tc.provider, tc.model, got, 1000*3e-6)
		}
	}
}

func TestCostWithCachedUnchanged(t *testing.T) {
	// The legacy entry point is byte-identical arithmetic:
	// CostWithCached == CostWithCacheTiers with both tiers 0.
	prices := anthropicTierPrices()
	got := prices.CostWithCached("anthropic", "claude-sonnet-4-5", 1000, 50, 100)
	// 900 x $3 + 100 x $0.30 + 50 x $15 = $0.003480.
	want := 900*3e-6 + 100*0.30e-6 + 50*15e-6
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("CostWithCached = %v, want %v", got, want)
	}
	if got != prices.CostWithCacheTiers("anthropic", "claude-sonnet-4-5", 1000, 50, 100, 0, 0) {
		t.Fatalf("CostWithCached != CostWithCacheTiers(..., 0, 0)")
	}
}

func TestCostWithCacheTiersClamping(t *testing.T) {
	prices := anthropicTierPrices()
	// Tiers claiming more than the prompt are clamped in turn:
	// cached 100, then 5m clamped to 900, then 1h to 0.
	got := prices.CostWithCacheTiers("anthropic", "claude-sonnet-4-5", 1000, 0, 100, 2000, 500)
	want := 900*3.75e-6 + 100*0.30e-6 // 900 5m writes + 100 reads, 0 regular
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("clamped CostWithCacheTiers = %v, want %v", got, want)
	}
	// Negative tiers clamp to 0.
	got = prices.CostWithCacheTiers("anthropic", "claude-sonnet-4-5", 1000, 0, 0, -5, -7)
	if got != 1000*3e-6 {
		t.Fatalf("negative-tier CostWithCacheTiers = %v, want %v", got, 1000*3e-6)
	}
	// Unknown price still costs 0.
	if got := prices.CostWithCacheTiers("anthropic", "no-such-model", 1000, 0, 0, 100, 200); got != 0 {
		t.Fatalf("unknown model cost = %v, want 0", got)
	}
}

func TestCacheWriteTierRecordAndSummary(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := anthropicTierPrices()

	// Tiered row: prompt 1000, 100 5m + 200 1h writes. The caller
	// also passes a stale/wrong total (999): the tiers win and
	// the stored total is their sum, 300.
	cost, err := l.RecordUsage(ctx, Usage{
		Provider: "anthropic", Model: "claude-sonnet-4-5",
		PromptTokens: 1000, CompletionTokens: 0, TotalTokens: 1000,
		CacheWriteTokens: 999, CacheWriteTokens5m: 100, CacheWriteTokens1h: 200,
		SplitKnown: true, OK: true, LatencyMS: 10,
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage tiered: %v", err)
	}
	if want := 0.003675; math.Abs(cost-want) > 1e-12 {
		t.Fatalf("tiered cost = %v, want %v", cost, want)
	}
	// Legacy row: unsplit total only — tier unknown, priced at 1x.
	if _, err := l.RecordUsage(ctx, Usage{
		Provider: "anthropic", Model: "claude-sonnet-4-5",
		PromptTokens: 500, CompletionTokens: 0, TotalTokens: 500,
		CacheWriteTokens: 500,
		SplitKnown:       true, OK: true, LatencyMS: 10,
	}, prices); err != nil {
		t.Fatalf("RecordUsage legacy: %v", err)
	}
	// Shared row: tiers zeroed like every other token field.
	if _, err := l.RecordUsage(ctx, Usage{
		Provider: "anthropic", Model: "claude-sonnet-4-5",
		PromptTokens: 1000, CacheWriteTokens5m: 100, CacheWriteTokens1h: 200,
		SplitKnown: true, OK: true, Shared: true,
	}, prices); err != nil {
		t.Fatalf("RecordUsage shared: %v", err)
	}

	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("RecentEvents = %d rows, want 3", len(events))
	}
	// Newest first: shared, legacy, tiered.
	if sh := events[0]; sh.CacheWriteTokens != 0 || sh.CacheWriteTokens5m != 0 || sh.CacheWriteTokens1h != 0 {
		t.Errorf("shared row tiers = %d/%d/%d, want 0/0/0", sh.CacheWriteTokens, sh.CacheWriteTokens5m, sh.CacheWriteTokens1h)
	}
	if leg := events[1]; leg.CacheWriteTokens != 500 || leg.CacheWriteTokens5m != 0 || leg.CacheWriteTokens1h != 0 {
		t.Errorf("legacy row tiers = %d/%d/%d, want 500/0/0", leg.CacheWriteTokens, leg.CacheWriteTokens5m, leg.CacheWriteTokens1h)
	}
	if tiered := events[2]; tiered.CacheWriteTokens != 300 || tiered.CacheWriteTokens5m != 100 || tiered.CacheWriteTokens1h != 200 {
		t.Errorf("tiered row = %d/%d/%d, want 300/100/200", tiered.CacheWriteTokens, tiered.CacheWriteTokens5m, tiered.CacheWriteTokens1h)
	}

	sum, err := l.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.CacheWriteTokens != 800 || sum.CacheWriteTokens5m != 100 || sum.CacheWriteTokens1h != 200 {
		t.Errorf("summary tiers = %d/%d/%d, want 800/100/200", sum.CacheWriteTokens, sum.CacheWriteTokens5m, sum.CacheWriteTokens1h)
	}
	if want := 0.003675 + 500*3e-6; math.Abs(sum.CostUSD-want) > 1e-12 {
		t.Errorf("summary cost = %v, want %v", sum.CostUSD, want)
	}
	if len(sum.ByProvider) != 1 {
		t.Fatalf("ByProvider = %d rows, want 1", len(sum.ByProvider))
	}
	if p := sum.ByProvider[0]; p.CacheWriteTokens != 800 || p.CacheWriteTokens5m != 100 || p.CacheWriteTokens1h != 200 {
		t.Errorf("provider tiers = %d/%d/%d, want 800/100/200", p.CacheWriteTokens, p.CacheWriteTokens5m, p.CacheWriteTokens1h)
	}
	if len(sum.ByModel) != 1 {
		t.Fatalf("ByModel = %d rows, want 1", len(sum.ByModel))
	}
	if m := sum.ByModel[0]; m.CacheWriteTokens != 800 || m.CacheWriteTokens5m != 100 || m.CacheWriteTokens1h != 200 {
		t.Errorf("model tiers = %d/%d/%d, want 800/100/200", m.CacheWriteTokens, m.CacheWriteTokens5m, m.CacheWriteTokens1h)
	}
}

func TestCacheWriteTierMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old-ledger.db")
	// A database as an older binary left it: usage_events WITH
	// the unsplit cache_write_tokens column but WITHOUT the two
	// tier columns, plus one legacy row (total 500, tier unknown).
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	oldSchema := `CREATE TABLE usage_events (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                INTEGER NOT NULL,
  gateway_key_id    TEXT NOT NULL,
  provider_key_id   TEXT NOT NULL DEFAULT '',
  provider          TEXT NOT NULL,
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  prompt_before_compression_tokens INTEGER NOT NULL DEFAULT 0,
  compression_saved_tokens INTEGER NOT NULL DEFAULT 0,
  local_estimate    INTEGER NOT NULL DEFAULT 0,
  local_estimate_prompt_tokens INTEGER NOT NULL DEFAULT 0,
  tokens_split_known INTEGER NOT NULL DEFAULT 1,
  endpoint          TEXT NOT NULL DEFAULT '',
  price_known       INTEGER NOT NULL DEFAULT 0,
  cost_usd          REAL NOT NULL DEFAULT 0,
  ok                INTEGER NOT NULL DEFAULT 1,
  latency_ms        INTEGER NOT NULL DEFAULT 0,
  shared            INTEGER NOT NULL DEFAULT 0,
  team_id           TEXT NOT NULL DEFAULT ''
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
	if _, err := db.Exec(`INSERT INTO usage_events
		(ts, gateway_key_id, provider, model, prompt_tokens, completion_tokens,
		 total_tokens, cache_write_tokens, tokens_split_known, cost_usd, ok)
		VALUES (?, 'gw-1', 'anthropic', 'claude-sonnet-4-5', 500, 0, 500, 500, 1, 0.0015, 1)`,
		time.Now().UnixMilli()); err != nil {
		db.Close()
		t.Fatalf("insert legacy row: %v", err)
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
	if !cols["cache_write_tokens_5m"] || !cols["cache_write_tokens_1h"] {
		t.Fatalf("migrated columns = %v, want cache_write_tokens_5m + cache_write_tokens_1h", cols)
	}
	// The legacy row still reads: total preserved, tiers 0.
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("RecentEvents = %d rows, want 1", len(events))
	}
	if e := events[0]; e.CacheWriteTokens != 500 || e.CacheWriteTokens5m != 0 || e.CacheWriteTokens1h != 0 {
		t.Fatalf("legacy row = %d/%d/%d, want 500/0/0", e.CacheWriteTokens, e.CacheWriteTokens5m, e.CacheWriteTokens1h)
	}
	// A tiered row records fine against the migrated database,
	// and the window total is the legacy total plus the tiers.
	if _, err := l.RecordUsage(ctx, Usage{
		Provider: "anthropic", Model: "claude-sonnet-4-5",
		PromptTokens: 1000, CompletionTokens: 0, TotalTokens: 1000,
		CacheWriteTokens5m: 100, CacheWriteTokens1h: 200,
		SplitKnown: true, OK: true,
	}, anthropicTierPrices()); err != nil {
		t.Fatalf("RecordUsage tiered on migrated db: %v", err)
	}
	sum, err := l.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.CacheWriteTokens != 800 || sum.CacheWriteTokens5m != 100 || sum.CacheWriteTokens1h != 200 {
		t.Fatalf("migrated summary tiers = %d/%d/%d, want 800/100/200", sum.CacheWriteTokens, sum.CacheWriteTokens5m, sum.CacheWriteTokens1h)
	}
}

func TestCacheWriteTierImport(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	now := time.Now()
	// A tiered event round-trips through import.
	imported, dups, err := l.ImportEvents(ctx, []UsageEvent{{
		TS: now, GatewayKeyID: "gw-1", Provider: "anthropic", Model: "claude-sonnet-4-5",
		PromptTokens: 1000, CompletionTokens: 0, TotalTokens: 1000,
		CacheWriteTokens: 300, CacheWriteTokens5m: 100, CacheWriteTokens1h: 200,
		SplitKnown: true, OK: true, CostUSD: 0.003675,
	}})
	if err != nil {
		t.Fatalf("ImportEvents tiered: %v", err)
	}
	if imported != 1 || dups != 0 {
		t.Fatalf("ImportEvents = %d imported %d dups, want 1/0", imported, dups)
	}
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 1 || events[0].CacheWriteTokens5m != 100 || events[0].CacheWriteTokens1h != 200 {
		t.Fatalf("imported row = %+v, want tiers 100/200", events[0])
	}
	// Re-importing the same event dedups on the total, not the tiers.
	if _, dups, err := l.ImportEvents(ctx, []UsageEvent{{
		TS: now, GatewayKeyID: "gw-1", Provider: "anthropic", Model: "claude-sonnet-4-5",
		PromptTokens: 1000, CompletionTokens: 0, TotalTokens: 1000,
		CacheWriteTokens: 300, CacheWriteTokens5m: 100, CacheWriteTokens1h: 200,
		SplitKnown: true, OK: true, CostUSD: 0.003675,
	}}); err != nil || dups != 1 {
		t.Fatalf("re-import = dups %d err %v, want 1 dup", dups, err)
	}
	// Tiers that do not sum to the total are corrupt.
	if _, _, err := l.ImportEvents(ctx, []UsageEvent{{
		TS: now, Provider: "anthropic", Model: "claude-sonnet-4-5",
		PromptTokens: 1000, TotalTokens: 1000,
		CacheWriteTokens: 300, CacheWriteTokens5m: 100, CacheWriteTokens1h: 100,
		SplitKnown: true, OK: true,
	}}); err == nil {
		t.Fatalf("ImportEvents with tiers 100+100 != total 300: want error, got nil")
	}
	// Negative tiers are corrupt.
	if _, _, err := l.ImportEvents(ctx, []UsageEvent{{
		TS: now, Provider: "anthropic", Model: "claude-sonnet-4-5",
		PromptTokens: 1000, TotalTokens: 1000,
		CacheWriteTokens: 300, CacheWriteTokens5m: -1,
		SplitKnown: true, OK: true,
	}}); err == nil {
		t.Fatalf("ImportEvents with negative 5m tier: want error, got nil")
	}
}
