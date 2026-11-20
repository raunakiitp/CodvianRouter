package modelsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fptr(v float64) *float64 { return &v }

// testCatalogJSON is a small models.dev-shaped document: one full
// model, one window-only model, one price-only model, one
// deprecated model, one with an invalid (negative) price, one
// with tiered prices (base tier only must be used), and one with
// a cache-only cost (no billable leg — no price).
const testCatalogJSON = `{
  "openai": {"id": "openai", "name": "OpenAI", "models": {
    "gpt-4o": {"id": "gpt-4o", "limit": {"context": 128000}, "cost": {"input": 2.5, "output": 10, "cache_read": 1.25}},
    "gpt-4o-mini": {"id": "gpt-4o-mini", "limit": {"context": 128000}, "cost": {"input": 0.15, "output": 0.6}},
    "o1": {"id": "o1", "limit": {"context": 200000}},
    "gpt-4-turbo": {"id": "gpt-4-turbo", "cost": {"input": 10, "output": 30}},
    "gpt-3.5-turbo-0301": {"id": "gpt-3.5-turbo-0301", "status": "deprecated", "limit": {"context": 16385}, "cost": {"input": 1.5, "output": 2}},
    "gpt-4-bad": {"id": "gpt-4-bad", "limit": {"context": 8192}, "cost": {"input": -5, "output": 15}},
    "gpt-5-tiered": {"id": "gpt-5-tiered", "limit": {"context": 400000}, "cost": {"input": 1.25, "output": 10, "cache_read": 0.125, "context_over_200k": {"input": 2.5, "output": 20}}},
    "cache-only": {"id": "cache-only", "limit": {"context": 64000}, "cost": {"cache_read": 0.5}}
  }},
  "anthropic": {"id": "anthropic", "name": "Anthropic", "models": {
    "claude-sonnet-4-5": {"id": "claude-sonnet-4-5", "limit": {"context": 200000}, "cost": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}}
  }}
}`

func TestParseCatalog(t *testing.T) {
	cat, err := ParseCatalog([]byte(testCatalogJSON))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if len(cat.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(cat.Providers))
	}
	openai := cat.Providers["openai"]
	if len(openai.Models) != 8 {
		t.Fatalf("openai models = %d, want 8", len(openai.Models))
	}
	full := openai.Models["gpt-4o"]
	if full.ContextWindow != 128000 || full.InputUSDPer1M == nil || *full.InputUSDPer1M != 2.5 || full.OutputUSDPer1M == nil || *full.OutputUSDPer1M != 10 || full.CachedInputUSDPer1M == nil || *full.CachedInputUSDPer1M != 1.25 {
		t.Errorf("gpt-4o = %+v, want window 128000 + 2.5/10/1.25", full)
	}
	// Tiered prices: the base tier is used, the over-200k tier ignored.
	tiered := openai.Models["gpt-5-tiered"]
	if tiered.InputUSDPer1M == nil || *tiered.InputUSDPer1M != 1.25 || tiered.OutputUSDPer1M == nil || *tiered.OutputUSDPer1M != 10 {
		t.Errorf("gpt-5-tiered price = %v/%v, want base 1.25/10 (tiered 2.5/20 ignored)", tiered.InputUSDPer1M, tiered.OutputUSDPer1M)
	}
	if openai.Models["gpt-3.5-turbo-0301"].Deprecated != true {
		t.Error("deprecated model not marked deprecated")
	}
	if openai.Models["gpt-4-bad"].CostInvalid != true {
		t.Error("negative-cost model not marked CostInvalid")
	}
	if openai.Models["gpt-4-bad"].ContextWindow != 8192 {
		t.Error("invalid-price model lost its window: the window is independent data and must still sync")
	}
	if openai.Models["cache-only"].hasPrice() {
		t.Error("cache-only model reports a price: a cached leg alone must not make a price")
	}
	if _, err := ParseCatalog([]byte(`{not json`)); err == nil {
		t.Error("malformed JSON parsed without error")
	}
}

func TestPlanOperatorWins(t *testing.T) {
	cat, err := ParseCatalog([]byte(testCatalogJSON))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	locals := []LocalProvider{
		{ID: "openai", CatalogID: "openai", Models: []LocalModel{
			{ID: "gpt-4o"},                         // no operator price, no window: both sync
			{ID: "gpt-4o-mini"},                    // operator pair price: price skipped, window syncs
			{ID: "o1", ContextWindow: 200000},      // operator window: window skipped (no price data anyway)
			{ID: "gpt-4-turbo"},                    // operator model-level price: pair skipped
			{ID: "gpt-3.5-turbo-0301"},             // deprecated: skipped entirely
			{ID: "gpt-4-bad"},                      // invalid price: price skipped, window syncs
			{ID: "not-in-catalog"},                 // unknown model: skipped
			{ID: "gpt-5-tiered", ContextWindow: 1}, // operator window (wrong value): skipped, never overwritten
		}},
		{ID: "my-anthropic", CatalogID: "anthropic", Models: []LocalModel{
			{ID: "claude-sonnet-4-5"}, // matched via catalog_id, not registry id
		}},
		{ID: "unmatched", Models: []LocalModel{{ID: "gpt-4o"}}}, // no models.dev provider: nothing
	}
	operatorHasPrice := func(provider, model string) bool {
		return (provider == "openai" && model == "gpt-4o-mini") ||
			model == "gpt-4-turbo" // model-level fallback covers every provider
	}
	plan := Plan(cat, locals, operatorHasPrice, nil)
	byPair := map[string]Update{}
	for _, u := range plan.Updates {
		byPair[u.Provider+"/"+u.Model] = u
	}
	// gpt-4o: price + window both apply.
	if u, ok := byPair["openai/gpt-4o"]; !ok || u.Price == nil || *u.Price.InputUSDPer1M != 2.5 || u.ContextWindow != 128000 {
		t.Errorf("gpt-4o update = %+v, want price 2.5 + window 128000", u)
	}
	// gpt-4o-mini: operator pair price wins (no price), window still syncs.
	if u, ok := byPair["openai/gpt-4o-mini"]; !ok || u.Price != nil || u.ContextWindow != 128000 || !u.PriceSkippedOperator {
		t.Errorf("gpt-4o-mini update = %+v, want no price (operator wins) + window 128000", u)
	}
	// o1: operator window wins; no price data at all, so no update row.
	if _, ok := byPair["openai/o1"]; ok {
		t.Error("o1 has an update row: its window is operator-set and models.dev has no price for it")
	}
	// gpt-4-turbo: operator model-level price wins over the synced
	// pair, and models.dev reports no window — so there is no
	// update row at all (the skip is counted, not applied).
	if _, ok := byPair["openai/gpt-4-turbo"]; ok {
		t.Error("gpt-4-turbo has an update row: its price is operator-set and models.dev reports no window")
	}
	// Deprecated / unknown / unmatched: no rows at all.
	for _, key := range []string{"openai/gpt-3.5-turbo-0301", "openai/not-in-catalog", "unmatched/gpt-4o"} {
		if _, ok := byPair[key]; ok {
			t.Errorf("%s has an update row, want none", key)
		}
	}
	// gpt-4-bad: invalid price skipped, window still applies.
	if u, ok := byPair["openai/gpt-4-bad"]; !ok || u.Price != nil || u.ContextWindow != 8192 {
		t.Errorf("gpt-4-bad update = %+v, want no price (invalid) + window 8192", u)
	}
	// gpt-5-tiered: operator window (1) is never overwritten.
	if u, ok := byPair["openai/gpt-5-tiered"]; !ok || u.Price == nil || u.ContextWindow != 0 || !u.WindowSkippedOperator {
		t.Errorf("gpt-5-tiered update = %+v, want price only (operator window wins)", u)
	}
	// Catalog-ID match: registry id "my-anthropic" matched anthropic.
	if u, ok := byPair["my-anthropic/claude-sonnet-4-5"]; !ok || u.Price == nil || *u.Price.InputUSDPer1M != 3 || u.ContextWindow != 200000 {
		t.Errorf("claude update = %+v, want price 3 + window 200000 via catalog_id match", u)
	}
	if plan.Counts.MatchedPairs != 8 {
		t.Errorf("matched = %d, want 8 (gpt-4o, mini, o1, turbo, deprecated, bad, tiered, claude)", plan.Counts.MatchedPairs)
	}
	if plan.Counts.SkippedDeprecated != 1 || plan.Counts.SkippedUnknownModel != 1 || plan.Counts.SkippedInvalidPrice != 1 {
		t.Errorf("counts = deprecated %d unknown %d invalid %d, want 1/1/1", plan.Counts.SkippedDeprecated, plan.Counts.SkippedUnknownModel, plan.Counts.SkippedInvalidPrice)
	}
	if plan.Counts.SkippedOperatorPrice != 2 || plan.Counts.SkippedOperatorWindow != 2 {
		t.Errorf("counts = skipped-price %d skipped-window %d, want 2/2", plan.Counts.SkippedOperatorPrice, plan.Counts.SkippedOperatorWindow)
	}
}

func TestPlanSyncedWindowRefresh(t *testing.T) {
	cat, err := ParseCatalog([]byte(testCatalogJSON))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	locals := []LocalProvider{{ID: "openai", Models: []LocalModel{{ID: "gpt-4o", ContextWindow: 128000}}}}
	// The current window IS the one the sync last applied: refresh.
	syncedWindow := func(provider, model string) (int, bool) { return 128000, true }
	plan := Plan(cat, locals, func(string, string) bool { return false }, syncedWindow)
	if len(plan.Updates) != 1 || plan.Updates[0].ContextWindow != 128000 {
		t.Errorf("synced window not refreshed: %+v", plan.Updates)
	}
	// The current window differs from the synced one: the operator
	// changed it — never overwrite.
	syncedWindow = func(provider, model string) (int, bool) { return 64000, true }
	plan = Plan(cat, locals, func(string, string) bool { return false }, syncedWindow)
	if len(plan.Updates) != 1 || plan.Updates[0].ContextWindow != 0 || !plan.Updates[0].WindowSkippedOperator {
		t.Errorf("operator-changed window not protected: %+v", plan.Updates)
	}
}

func TestSyncerDisabledMakesNoRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(testCatalogJSON))
	}))
	defer srv.Close()
	s := New(SyncerConfig{Enabled: false, URL: srv.URL}, "")
	counts, err := s.Sync(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("disabled Sync: %v", err)
	}
	if counts != (Counts{}) {
		t.Errorf("disabled Sync counts = %+v, want zero", counts)
	}
	if calls.Load() != 0 {
		t.Errorf("disabled sync made %d HTTP requests, want 0", calls.Load())
	}
	if st := s.Status(); st.Enabled {
		t.Error("disabled syncer reports enabled")
	}
}

func TestSyncerFailureKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, StateFileName)
	// First sync succeeds against the stand-in.
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testCatalogJSON))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, statePath)
	locals := []LocalProvider{{ID: "openai", CatalogID: "openai", Models: []LocalModel{{ID: "gpt-4o"}}}}
	applied := 0
	apply := func(plan PlanResult) error {
		applied += len(plan.Updates)
		return nil
	}
	counts, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply)
	if err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if counts.PricesApplied != 1 || counts.WindowsApplied != 1 || applied != 1 {
		t.Fatalf("first sync counts = %+v applied %d, want 1 price + 1 window", counts, applied)
	}
	if len(s.SyncedPrices()) != 1 {
		t.Fatalf("synced prices = %d, want 1", len(s.SyncedPrices()))
	}
	// Second sync: the stand-in now fails. The error is recorded,
	// the last good entries stay, and apply is never called.
	standin.Close()
	// Point the syncer at a dead URL (same syncer, new URL would
	// need a new Syncer — instead close+recreate is covered by
	// the closed server: the client still dials the dead port).
	counts, err = s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply)
	if err == nil {
		t.Fatal("second Sync against a dead server succeeded, want an error")
	}
	if applied != 1 {
		t.Errorf("failed sync called apply (%d applies total), want exactly 1 (the first sync)", applied)
	}
	st := s.Status()
	if st.LastError == "" {
		t.Error("failed sync recorded no error")
	}
	if st.LastSuccessAt == nil {
		t.Error("failed sync lost the last success time")
	}
	if len(st.Entries) != 1 {
		t.Errorf("failed sync entries = %d, want 1 (last good kept)", len(st.Entries))
	}
	if len(s.SyncedPrices()) != 1 {
		t.Error("failed sync dropped the last good price from the book layer")
	}
	// The failure persisted: a fresh syncer loading the same state
	// file sees the error and the last good entry.
	s2 := New(SyncerConfig{Enabled: true, URL: standin.URL}, statePath)
	if err := s2.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	st2 := s2.Status()
	if st2.LastError == "" || len(st2.Entries) != 1 {
		t.Errorf("reloaded state = error %q entries %d, want the recorded error + 1 entry", st2.LastError, len(st2.Entries))
	}
}

func TestSyncerStateRoundTripAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, StateFileName)
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testCatalogJSON))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, statePath)
	locals := []LocalProvider{{ID: "openai", Models: []LocalModel{{ID: "gpt-4o"}, {ID: "o1"}}}}
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, nil); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// o1 has a window but no price: the entry exists with a window only.
	if w, ok := s.SyncedWindow("openai", "o1"); !ok || w != 200000 {
		t.Errorf("SyncedWindow(o1) = %d/%v, want 200000/true", w, ok)
	}
	// State file on disk, 0600, valid JSON.
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("state JSON: %v", err)
	}
	if sf.Version != 1 || len(sf.Entries) != 2 {
		t.Errorf("state file = version %d entries %d, want 1/2", sf.Version, len(sf.Entries))
	}
	// Windows has no Unix permission bits (files report 0666), so
	// the exact mode is only asserted off Windows.
	if info, err := os.Stat(statePath); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Errorf("state file mode = %v, want 0600", info.Mode())
	}
	// A fresh syncer loads it back.
	s2 := New(SyncerConfig{Enabled: true, URL: standin.URL}, statePath)
	if err := s2.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(s2.SyncedPrices()) != 1 { // only gpt-4o has a price
		t.Errorf("reloaded synced prices = %d, want 1", len(s2.SyncedPrices()))
	}
	// A corrupt state file errors and leaves the syncer empty —
	// startup must never fail on it (the caller logs + carries on).
	if err := os.WriteFile(statePath, []byte(`{corrupt`), 0o600); err != nil {
		t.Fatalf("corrupt state: %v", err)
	}
	s3 := New(SyncerConfig{Enabled: true, URL: standin.URL}, statePath)
	if err := s3.LoadState(); err == nil {
		t.Error("corrupt state file loaded without error")
	}
	if len(s3.SyncedPrices()) != 0 {
		t.Error("corrupt state left entries behind")
	}
	// A missing state file is "never synced", not an error.
	s4 := New(SyncerConfig{Enabled: true, URL: standin.URL}, filepath.Join(dir, "nope.json"))
	if err := s4.LoadState(); err != nil {
		t.Errorf("missing state file: %v", err)
	}
}

func TestSyncerDropsOperatorPricedPair(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, StateFileName)
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testCatalogJSON))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, statePath)
	locals := []LocalProvider{{ID: "openai", Models: []LocalModel{{ID: "gpt-4o"}}}}
	noOperator := func(string, string) bool { return false }
	if _, err := s.Sync(context.Background(), locals, noOperator, nil); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if len(s.SyncedPrices()) != 1 {
		t.Fatalf("synced prices = %d, want 1", len(s.SyncedPrices()))
	}
	// The operator prices the pair afterwards: the next sync must
	// drop the synced price (PlanResult.DropPrices) and the caller
	// applies the drop — simulate the apply by checking the plan
	// the syncer hands over.
	operatorNow := func(provider, model string) bool { return provider == "openai" && model == "gpt-4o" }
	var gotPlan PlanResult
	apply := func(plan PlanResult) error { gotPlan = plan; return nil }
	if _, err := s.Sync(context.Background(), locals, operatorNow, apply); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if len(gotPlan.DropPrices) != 1 || gotPlan.DropPrices[0].Model != "gpt-4o" {
		t.Errorf("DropPrices = %+v, want [openai/gpt-4o]", gotPlan.DropPrices)
	}
	if gotPlan.Counts.DroppedStale != 1 {
		t.Errorf("DroppedStale = %d, want 1", gotPlan.Counts.DroppedStale)
	}
	// After the drop folds into the state, the price is gone.
	if len(s.SyncedPrices()) != 0 {
		t.Errorf("synced prices after drop = %d, want 0", len(s.SyncedPrices()))
	}
}

func TestSyncerApplyFailureKeepsState(t *testing.T) {
	dir := t.TempDir()
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testCatalogJSON))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, filepath.Join(dir, StateFileName))
	locals := []LocalProvider{{ID: "openai", Models: []LocalModel{{ID: "gpt-4o"}}}}
	apply := func(plan PlanResult) error { return errTestApply }
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err == nil {
		t.Fatal("Sync with a failing apply succeeded, want an error")
	}
	if len(s.SyncedPrices()) != 0 {
		t.Error("failed apply left synced prices behind — the state must not advance past the apply")
	}
	if st := s.Status(); st.LastError == "" || !strings.Contains(st.LastError, "apply failed") {
		t.Errorf("LastError = %q, want an apply-failure record", st.LastError)
	}
}

var errTestApply = errors.New("test apply failure")

// Regression test for the first live smoke (CHECKLIST 179-180):
// the real apply callback (modelsDevApplyPlan) reads the syncer's
// current prices via SyncedPrices() while merging the plan. Sync
// used to hold s.mu across the whole fetch+apply, so that
// re-entrant lock deadlocked the first background sync forever —
// and every dashboard Status() read behind it. Sync must return
// (the apply runs unlocked), and the state must hold the plan.
func TestSyncerApplyCanReadState(t *testing.T) {
	dir := t.TempDir()
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testCatalogJSON))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, filepath.Join(dir, StateFileName))
	locals := []LocalProvider{{ID: "openai", Models: []LocalModel{{ID: "gpt-4o"}}}}
	apply := func(plan PlanResult) error {
		// Exactly what modelsDevApplyPlan does: read the
		// syncer's current prices, then merge the plan in.
		_ = s.SyncedPrices()
		_ = s.Status()
		if w, ok := s.SyncedWindow("openai", "gpt-4o"); ok {
			t.Errorf("SyncedWindow before any sync = %d, want none", w)
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sync with a state-reading apply failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sync did not return within 10s — the apply deadlocked on the syncer lock")
	}
	if len(s.SyncedPrices()) == 0 {
		t.Error("state-reading apply left no synced prices behind")
	}
}

func TestFetchNon200IsAnError(t *testing.T) {
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, "")
	if _, err := s.Sync(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("HTTP 503 catalog fetched without error")
	}
}

// bigCatalogJSON builds a models.dev-shaped document with n
// providers, each carrying m priced models with windows (input
// $1/1M, output $2/1M, window 128000). bigLocals mirrors it: every
// provider/model pair local, so a sync against it applies n*m
// prices.
func bigCatalogJSON(n, m int) string {
	doc := make(map[string]any, n)
	for i := 0; i < n; i++ {
		pid := fmt.Sprintf("p%d", i)
		models := make(map[string]any, m)
		for j := 0; j < m; j++ {
			mid := fmt.Sprintf("m%d", j)
			models[mid] = map[string]any{
				"id":    mid,
				"limit": map[string]any{"context": 128000},
				"cost":  map[string]any{"input": 1.0, "output": 2.0},
			}
		}
		doc[pid] = map[string]any{"id": pid, "name": pid, "models": models}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err) // test helper: marshaling plain maps cannot fail
	}
	return string(raw)
}

func bigLocals(n, m int) []LocalProvider {
	locals := make([]LocalProvider, 0, n)
	for i := 0; i < n; i++ {
		models := make([]LocalModel, 0, m)
		for j := 0; j < m; j++ {
			models = append(models, LocalModel{ID: fmt.Sprintf("m%d", j)})
		}
		locals = append(locals, LocalProvider{ID: fmt.Sprintf("p%d", i), Models: models})
	}
	return locals
}

// CT-5: a 0-provider fetch (empty document, or providers with no
// models at all) is a FAILED sync, never a successful one: it
// errors, records the error, never calls apply, and leaves no
// success time behind.
func TestSyncerEmptyCatalogFails(t *testing.T) {
	body := "{}"
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, "")
	applied := 0
	apply := func(plan PlanResult) error { applied++; return nil }
	locals := []LocalProvider{{ID: "openai", Models: []LocalModel{{ID: "gpt-4o"}}}}

	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err == nil {
		t.Fatal("0-provider fetch succeeded, want an error")
	} else if !strings.Contains(err.Error(), "0 providers") {
		t.Errorf("0-provider error = %q, want it to name the empty fetch", err)
	}
	if applied != 0 {
		t.Errorf("empty fetch called apply %d times, want 0", applied)
	}
	st := s.Status()
	if st.LastError == "" {
		t.Error("empty fetch recorded no error")
	}
	if st.LastSuccessAt != nil {
		t.Error("empty fetch recorded a success time")
	}
	if len(st.Entries) != 0 {
		t.Errorf("empty fetch left %d entries, want 0", len(st.Entries))
	}

	// Providers present but zero models is the same empty fetch.
	body = `{"openai": {"id": "openai", "name": "OpenAI", "models": {}}}`
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err == nil {
		t.Fatal("0-model fetch succeeded, want an error")
	} else if !strings.Contains(err.Error(), "0 models") {
		t.Errorf("0-model error = %q, want it to name the empty fetch", err)
	}
	if applied != 0 {
		t.Errorf("0-model fetch called apply %d times, want 0", applied)
	}
}

// CT-5: a fetch that shrinks the provider count past the guard
// (10 -> 5 providers, a 50% drop against the 20% threshold) is a
// FAILED sync: apply is never called, the last good entries and
// success time survive, the failure is recorded — and persisted,
// so a fresh syncer loading the state file sees the same.
func TestSyncerShrinkingCatalogKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, StateFileName)
	body := bigCatalogJSON(10, 2)
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, statePath)
	locals := bigLocals(10, 2)
	applied := 0
	apply := func(plan PlanResult) error { applied++; return nil }
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}
	if applied != 1 || len(s.SyncedPrices()) != 20 {
		t.Fatalf("seed sync applied %d times with %d prices, want 1/20", applied, len(s.SyncedPrices()))
	}
	lastSuccess := *s.Status().LastSuccessAt

	// The feed now returns half the catalog: a truncated fetch.
	body = bigCatalogJSON(5, 2)
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err == nil {
		t.Fatal("shrinking fetch succeeded, want an error")
	} else if !strings.Contains(err.Error(), "shrank") {
		t.Errorf("shrinking-fetch error = %q, want it to name the shrink", err)
	}
	if applied != 1 {
		t.Errorf("shrinking fetch called apply (%d applies total), want exactly 1 (the seed sync)", applied)
	}
	st := s.Status()
	if st.LastError == "" || !strings.Contains(st.LastError, "shrank") {
		t.Errorf("LastError = %q, want the recorded shrink failure", st.LastError)
	}
	if st.LastSuccessAt == nil || !st.LastSuccessAt.Equal(lastSuccess) {
		t.Errorf("shrinking fetch moved LastSuccessAt to %v, want it kept at %v", st.LastSuccessAt, lastSuccess)
	}
	if len(st.Entries) != 20 || len(s.SyncedPrices()) != 20 {
		t.Errorf("shrinking fetch left %d entries / %d prices, want 20/20 (last good kept)", len(st.Entries), len(s.SyncedPrices()))
	}
	// The failure persisted: a fresh syncer loading the same
	// state file sees the error and the last good entries.
	s2 := New(SyncerConfig{Enabled: true, URL: standin.URL}, statePath)
	if err := s2.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	st2 := s2.Status()
	if st2.LastError == "" || len(st2.Entries) != 20 {
		t.Errorf("reloaded state = error %q entries %d, want the recorded error + 20 entries", st2.LastError, len(st2.Entries))
	}
}

// CT-5: the same guard on the model leg — same 10 providers, but
// half the models (20 -> 10) — is refused the same way.
func TestSyncerModelShrinkKeepsLastGood(t *testing.T) {
	body := bigCatalogJSON(10, 2)
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, "")
	locals := bigLocals(10, 2)
	applied := 0
	apply := func(plan PlanResult) error { applied++; return nil }
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}
	body = bigCatalogJSON(10, 1)
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err == nil {
		t.Fatal("model-shrinking fetch succeeded, want an error")
	} else if !strings.Contains(err.Error(), "shrank") {
		t.Errorf("model-shrinking error = %q, want it to name the shrink", err)
	}
	if applied != 1 {
		t.Errorf("model-shrinking fetch called apply (%d applies total), want exactly 1 (the seed sync)", applied)
	}
	if len(s.SyncedPrices()) != 20 {
		t.Errorf("model-shrinking fetch left %d prices, want 20 (last good kept)", len(s.SyncedPrices()))
	}
}

// CT-5: a normal fetch still applies — including a modest,
// within-threshold shrink (10 -> 9 providers is 10%, under the
// 20% guard) carrying a changed price, which must land.
func TestSyncerNormalFetchStillApplies(t *testing.T) {
	body := bigCatalogJSON(10, 2)
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, "")
	locals := bigLocals(10, 2)
	applied := 0
	apply := func(plan PlanResult) error { applied++; return nil }
	noOperator := func(string, string) bool { return false }
	if _, err := s.Sync(context.Background(), locals, noOperator, apply); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}
	// Same catalog, one price changed: applies cleanly.
	doc := map[string]any{}
	if err := json.Unmarshal([]byte(bigCatalogJSON(10, 2)), &doc); err != nil {
		t.Fatalf("rebuild catalog: %v", err)
	}
	p0 := doc["p0"].(map[string]any)["models"].(map[string]any)
	p0["m0"] = map[string]any{
		"id":    "m0",
		"limit": map[string]any{"context": 128000},
		"cost":  map[string]any{"input": 5.0, "output": 20.0},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	body = string(raw)
	if _, err := s.Sync(context.Background(), locals, noOperator, apply); err != nil {
		t.Fatalf("same-size Sync: %v", err)
	}
	if applied != 2 {
		t.Fatalf("same-size sync applied %d times, want 2", applied)
	}
	e := s.Status().Entries[EntryKey("p0", "m0")]
	if e.Price == nil || e.Price.InputUSDPer1M == nil || *e.Price.InputUSDPer1M != 5.0 {
		t.Errorf("p0/m0 price after re-sync = %+v, want input $5/1M applied", e.Price)
	}
	// A 10% provider shrink (10 -> 9) is inside the guard and
	// still applies.
	body = bigCatalogJSON(9, 2)
	if _, err := s.Sync(context.Background(), locals, noOperator, apply); err != nil {
		t.Fatalf("10%%-shrink Sync: %v", err)
	}
	if applied != 3 {
		t.Fatalf("10%%-shrink sync applied %d times, want 3", applied)
	}
	if st := s.Status(); st.LastError != "" {
		t.Errorf("10%%-shrink sync recorded error %q, want none", st.LastError)
	}
}

// CT-5: the minShrinkBaseline exemption — a shrink from a
// baseline below 5 providers is ALLOWED, even a 50% one (4 -> 2).
// The shrunken catalog keeps the same model count (2 providers x
// 4 models = 8 models, as before), so only the provider leg is
// under test: the model leg cannot trigger and mask the
// exemption. Without the exemption the 50% provider drop would
// be refused.
func TestSyncerSmallBaselineShrinkAllowed(t *testing.T) {
	body := bigCatalogJSON(4, 2)
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, "")
	locals := bigLocals(4, 2)
	applied := 0
	apply := func(plan PlanResult) error { applied++; return nil }
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}
	if applied != 1 || len(s.SyncedPrices()) != 8 {
		t.Fatalf("seed sync applied %d times with %d prices, want 1/8", applied, len(s.SyncedPrices()))
	}
	// 4 -> 2 providers (50% drop) with the model count held at 8.
	body = bigCatalogJSON(2, 4)
	counts, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply)
	if err != nil {
		t.Fatalf("4->2 provider shrink Sync: %v (want it allowed: baseline 4 < minShrinkBaseline 5)", err)
	}
	if applied != 2 {
		t.Fatalf("4->2 shrink sync applied %d times, want 2", applied)
	}
	if counts.FetchedProviders != 2 || counts.FetchedModels != 8 {
		t.Errorf("shrink sync counts = %d providers / %d models, want 2/8", counts.FetchedProviders, counts.FetchedModels)
	}
	if st := s.Status(); st.LastError != "" {
		t.Errorf("4->2 shrink sync recorded error %q, want none", st.LastError)
	}
	if len(s.SyncedPrices()) != 8 {
		t.Errorf("4->2 shrink sync left %d prices, want 8 (last good kept)", len(s.SyncedPrices()))
	}
}

// CT-5: the exact-20% boundary — 10 -> 8 providers is exactly
// 20%, and the guard is strict (<), so it must PASS. Both legs
// sit exactly on the boundary here (8 providers, 16 models
// against a 10-provider / 20-model baseline).
func TestSyncerExactTwentyPercentShrinkAllowed(t *testing.T) {
	body := bigCatalogJSON(10, 2)
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, "")
	locals := bigLocals(10, 2)
	applied := 0
	apply := func(plan PlanResult) error { applied++; return nil }
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}
	body = bigCatalogJSON(8, 2)
	counts, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply)
	if err != nil {
		t.Fatalf("10->8 (exactly 20%%) shrink Sync: %v (want it allowed: guard is strict <)", err)
	}
	if applied != 2 {
		t.Fatalf("10->8 shrink sync applied %d times, want 2", applied)
	}
	if counts.FetchedProviders != 8 || counts.FetchedModels != 16 {
		t.Errorf("shrink sync counts = %d providers / %d models, want 8/16", counts.FetchedProviders, counts.FetchedModels)
	}
	if st := s.Status(); st.LastError != "" {
		t.Errorf("10->8 shrink sync recorded error %q, want none", st.LastError)
	}
}

// CT-5: just beyond the boundary — 10 -> 7 providers is a 30%
// drop, past the 20% guard, so it must be REFUSED (same seed as
// the exact-boundary test, fresh syncer so the baseline is still
// 10, not the 8 the allowed shrink would leave behind).
func TestSyncerJustBeyondTwentyPercentShrinkRefused(t *testing.T) {
	body := bigCatalogJSON(10, 2)
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, "")
	locals := bigLocals(10, 2)
	applied := 0
	apply := func(plan PlanResult) error { applied++; return nil }
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}
	lastSuccess := *s.Status().LastSuccessAt
	body = bigCatalogJSON(7, 2)
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err == nil {
		t.Fatal("10->7 (30%%) shrink succeeded, want an error")
	} else if !strings.Contains(err.Error(), "shrank") {
		t.Errorf("10->7 shrink error = %q, want it to name the shrink", err)
	}
	if applied != 1 {
		t.Errorf("10->7 shrink fetch called apply (%d applies total), want exactly 1 (the seed sync)", applied)
	}
	st := s.Status()
	if st.LastError == "" || !strings.Contains(st.LastError, "shrank") {
		t.Errorf("LastError = %q, want the recorded shrink failure", st.LastError)
	}
	if st.LastSuccessAt == nil || !st.LastSuccessAt.Equal(lastSuccess) {
		t.Errorf("10->7 shrink fetch moved LastSuccessAt to %v, want it kept at %v", st.LastSuccessAt, lastSuccess)
	}
	if len(st.Entries) != 20 || len(s.SyncedPrices()) != 20 {
		t.Errorf("10->7 shrink fetch left %d entries / %d prices, want 20/20 (last good kept)", len(st.Entries), len(s.SyncedPrices()))
	}
}

// CT-5: the restored baseline is actually used — after a refused
// shrinking sync, a fresh syncer loading the persisted state
// still refuses a SECOND shrinking sync against the same feed.
// This proves LoadState() restores the last-good Counts baseline
// (10 providers / 20 models), not just the entries: a syncer
// that forgot the baseline would see no previous catalog and
// let the truncated fetch through.
func TestSyncerShrinkStillRefusedAfterReload(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, StateFileName)
	body := bigCatalogJSON(10, 2)
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, URL: standin.URL}, statePath)
	locals := bigLocals(10, 2)
	applied := 0
	apply := func(plan PlanResult) error { applied++; return nil }
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}
	if applied != 1 || len(s.SyncedPrices()) != 20 {
		t.Fatalf("seed sync applied %d times with %d prices, want 1/20", applied, len(s.SyncedPrices()))
	}
	lastSuccess := *s.Status().LastSuccessAt

	// First shrinking fetch (10 -> 5) is refused, as before.
	body = bigCatalogJSON(5, 2)
	if _, err := s.Sync(context.Background(), locals, func(string, string) bool { return false }, apply); err == nil {
		t.Fatal("first shrinking fetch succeeded, want an error")
	} else if !strings.Contains(err.Error(), "shrank") {
		t.Errorf("first shrinking-fetch error = %q, want it to name the shrink", err)
	}
	if applied != 1 {
		t.Errorf("first shrinking fetch called apply (%d applies total), want exactly 1 (the seed sync)", applied)
	}

	// Reload into a fresh syncer: the last-good baseline must
	// come back with the entries.
	s2 := New(SyncerConfig{Enabled: true, URL: standin.URL}, statePath)
	if err := s2.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	st2 := s2.Status()
	if st2.Counts.FetchedProviders != 10 || st2.Counts.FetchedModels != 20 {
		t.Fatalf("reloaded baseline = %d providers / %d models, want 10/20 (last good)", st2.Counts.FetchedProviders, st2.Counts.FetchedModels)
	}
	if st2.LastError == "" || len(st2.Entries) != 20 {
		t.Fatalf("reloaded state = error %q entries %d, want the recorded error + 20 entries", st2.LastError, len(st2.Entries))
	}
	if st2.LastSuccessAt == nil || !st2.LastSuccessAt.Equal(lastSuccess) {
		t.Fatalf("reloaded LastSuccessAt = %v, want %v", st2.LastSuccessAt, lastSuccess)
	}

	// Second shrinking sync, same truncated feed, on the
	// reloaded syncer: still refused, apply never called.
	applied2 := 0
	apply2 := func(plan PlanResult) error { applied2++; return nil }
	if _, err := s2.Sync(context.Background(), locals, func(string, string) bool { return false }, apply2); err == nil {
		t.Fatal("second shrinking fetch after reload succeeded, want an error (restored baseline must still guard)")
	} else if !strings.Contains(err.Error(), "shrank") {
		t.Errorf("second shrinking-fetch error = %q, want it to name the shrink", err)
	}
	if applied2 != 0 {
		t.Errorf("second shrinking fetch called apply %d times, want 0", applied2)
	}
	st2 = s2.Status()
	if st2.LastError == "" || !strings.Contains(st2.LastError, "shrank") {
		t.Errorf("LastError after second refusal = %q, want the recorded shrink failure", st2.LastError)
	}
	if st2.LastSuccessAt == nil || !st2.LastSuccessAt.Equal(lastSuccess) {
		t.Errorf("second refusal moved LastSuccessAt to %v, want it kept at %v", st2.LastSuccessAt, lastSuccess)
	}
	if len(st2.Entries) != 20 || len(s2.SyncedPrices()) != 20 {
		t.Errorf("second refusal left %d entries / %d prices, want 20/20 (last good kept)", len(st2.Entries), len(s2.SyncedPrices()))
	}
}
