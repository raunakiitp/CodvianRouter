package ledger

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// Token-count drift (CHECKLIST 229-230): the local estimate of the
// prompt as sent, recorded alongside the provider-reported split,
// so estimate-vs-reported drift is computable from live ledger data.

func TestRecordUsageStoresLocalEstimatePrompt(t *testing.T) {
	ctx := context.Background()
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	// Split-known row: the estimate rides alongside the reported
	// split, and pricing still uses the reported split only.
	if _, err := l.RecordUsage(ctx, Usage{
		Provider: "openai", Model: "gpt-4o-mini",
		PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110,
		SplitKnown: true, OK: true,
		LocalEstimatePromptTokens: 90,
	}, Prices{{Provider: "openai", Model: "gpt-4o-mini"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// Shared row: the estimate is zeroed like every other token
	// field — the leader's row carries the real usage.
	if _, err := l.RecordUsage(ctx, Usage{
		Provider: "openai", Model: "gpt-4o-mini",
		PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110,
		SplitKnown: true, OK: true, Shared: true,
		LocalEstimatePromptTokens: 90,
	}, nil); err != nil {
		t.Fatalf("RecordUsage shared: %v", err)
	}
	// Local-estimate row (upstream reported no usage): the
	// estimate is stored both as the prompt count and in the
	// drift column, so the two agree.
	if _, err := l.RecordUsage(ctx, Usage{
		Provider: "anthropic", Model: "claude-x",
		PromptTokens: 55, TotalTokens: 55,
		LocalEstimate: true, OK: true,
		LocalEstimatePromptTokens: 55,
	}, nil); err != nil {
		t.Fatalf("RecordUsage estimate: %v", err)
	}
	// A negative estimate is a caller bug: clamped to 0, never stored.
	if _, err := l.RecordUsage(ctx, Usage{
		Provider: "openai", Model: "gpt-4o-mini",
		PromptTokens: 50, CompletionTokens: 5, TotalTokens: 55,
		SplitKnown: true, OK: true,
		LocalEstimatePromptTokens: -7,
	}, nil); err != nil {
		t.Fatalf("RecordUsage negative estimate: %v", err)
	}

	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("RecentEvents = %d rows, want 4", len(events))
	}
	// Newest first: negative-estimate row, estimate row, shared, split-known.
	if got := events[0].LocalEstimatePromptTokens; got != 0 {
		t.Errorf("negative estimate row: LocalEstimatePromptTokens = %d, want 0 (clamped)", got)
	}
	if got := events[1].LocalEstimatePromptTokens; got != 55 {
		t.Errorf("estimate row: LocalEstimatePromptTokens = %d, want 55", got)
	}
	if got := events[2].LocalEstimatePromptTokens; got != 0 {
		t.Errorf("shared row: LocalEstimatePromptTokens = %d, want 0 (zeroed)", got)
	}
	if got := events[3].LocalEstimatePromptTokens; got != 90 {
		t.Errorf("split-known row: LocalEstimatePromptTokens = %d, want 90", got)
	}
	if got := events[3].PromptTokens; got != 100 {
		t.Errorf("split-known row: PromptTokens = %d, want 100 (reported, never overwritten)", got)
	}
}

func TestDriftSummary(t *testing.T) {
	ctx := context.Background()
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	seed := []Usage{
		// openai / gpt-4o-mini: local 90 vs reported 100 (-10%),
		// local 110 vs reported 100 (+10%): mean 0, mean|.| 10, max 10.
		{Provider: "openai", Model: "gpt-4o-mini", PromptTokens: 100, CompletionTokens: 5, SplitKnown: true, OK: true, LocalEstimatePromptTokens: 90},
		{Provider: "openai", Model: "gpt-4o-mini", PromptTokens: 100, CompletionTokens: 5, SplitKnown: true, OK: true, LocalEstimatePromptTokens: 110},
		// anthropic / claude-x: local 80 vs reported 100 (-20%).
		{Provider: "anthropic", Model: "claude-x", PromptTokens: 100, CompletionTokens: 5, SplitKnown: true, OK: true, LocalEstimatePromptTokens: 80},
		// Excluded: no estimate measured (0) — not perfect agreement.
		{Provider: "openai", Model: "gpt-4o-mini", PromptTokens: 100, CompletionTokens: 5, SplitKnown: true, OK: true},
		// Excluded: split unknown (total-only row).
		{Provider: "openai", Model: "gpt-4o-mini", TotalTokens: 100, OK: true, LocalEstimatePromptTokens: 90},
		// Excluded: local-estimate row (no reported split at all).
		{Provider: "anthropic", Model: "claude-x", PromptTokens: 60, TotalTokens: 60, LocalEstimate: true, OK: true, LocalEstimatePromptTokens: 60},
		// Excluded: shared row.
		{Provider: "anthropic", Model: "claude-x", PromptTokens: 100, CompletionTokens: 5, SplitKnown: true, OK: true, Shared: true, LocalEstimatePromptTokens: 50},
	}
	for i, u := range seed {
		if _, err := l.RecordUsage(ctx, u, nil); err != nil {
			t.Fatalf("RecordUsage %d: %v", i, err)
		}
	}

	drift, err := l.DriftSummary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("DriftSummary: %v", err)
	}
	if len(drift) != 2 {
		t.Fatalf("DriftSummary = %d rows, want 2: %+v", len(drift), drift)
	}
	byKey := map[string]DriftRow{}
	for _, d := range drift {
		byKey[d.Provider+"/"+d.Model] = d
	}
	oai := byKey["openai/gpt-4o-mini"]
	if oai.Compared != 2 {
		t.Errorf("openai compared = %d, want 2", oai.Compared)
	}
	if oai.MeanErrPct != 0 {
		t.Errorf("openai mean error = %v, want 0 (-10 and +10 cancel)", oai.MeanErrPct)
	}
	if oai.MeanAbsErrPct != 10 || oai.MaxAbsErrPct != 10 {
		t.Errorf("openai mean|max abs = %v|%v, want 10|10", oai.MeanAbsErrPct, oai.MaxAbsErrPct)
	}
	ant := byKey["anthropic/claude-x"]
	if ant.Compared != 1 || ant.MeanErrPct != -20 || ant.MeanAbsErrPct != 20 || ant.MaxAbsErrPct != 20 {
		t.Errorf("anthropic drift = %+v, want compared 1, all errors -20/20/20", ant)
	}

	// Summary carries the same drift (the dashboard feed reads it).
	sum, err := l.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(sum.Drift) != 2 {
		t.Fatalf("Summary.Drift = %d rows, want 2", len(sum.Drift))
	}
	// An empty window reports no drift, never invented rows.
	empty, err := l.DriftSummary(ctx, time.Now().Add(time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("DriftSummary empty window: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("DriftSummary empty window = %d rows, want 0", len(empty))
	}
}

func TestOpenMigratesLocalEstimatePromptColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old-ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// A database from before the drift column existed: the schema
	// has local_estimate but no local_estimate_prompt_tokens.
	oldSchema := `CREATE TABLE usage_events (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                INTEGER NOT NULL,
  gateway_key_id    TEXT NOT NULL,
  provider          TEXT NOT NULL,
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  local_estimate    INTEGER NOT NULL DEFAULT 0,
  tokens_split_known INTEGER NOT NULL DEFAULT 1,
  cost_usd          REAL NOT NULL DEFAULT 0,
  ok                INTEGER NOT NULL DEFAULT 1,
  latency_ms        INTEGER NOT NULL DEFAULT 0,
  shared            INTEGER NOT NULL DEFAULT 0
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
	if err := db.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open old db: %v", err)
	}
	defer l.Close()
	if cols := usageColumns(t, l); !cols["local_estimate_prompt_tokens"] {
		t.Fatalf("migrated columns = %v, want local_estimate_prompt_tokens", cols)
	}
	// RecordUsage + DriftSummary work against the migrated DB.
	if _, err := l.RecordUsage(ctx, Usage{
		Provider: "openai", Model: "m", PromptTokens: 100, CompletionTokens: 5,
		SplitKnown: true, OK: true, LocalEstimatePromptTokens: 95,
	}, nil); err != nil {
		t.Fatalf("RecordUsage after migration: %v", err)
	}
	drift, err := l.DriftSummary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("DriftSummary after migration: %v", err)
	}
	if len(drift) != 1 || drift[0].Compared != 1 || drift[0].MeanErrPct != -5 {
		t.Fatalf("drift after migration = %+v, want 1 row, compared 1, mean -5", drift)
	}
}
