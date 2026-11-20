package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// staticKeyID is the caller ID the static test verifier derives
// for a key ("static-" + first 12 hex of its SHA-256) — the ID
// the ledger budget must be set under.
func staticKeyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "static-" + hex.EncodeToString(sum[:])[:12]
}

func floatPtr(v float64) *float64 { return &v }

// moneyNear reports whether a *float64 economics leg is present
// and within a cent-fraction of want: USD amounts are products of
// per-token prices, so exact float equality is the wrong test.
func moneyNear(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %v", name, want)
		return
	}
	diff := *got - want
	if diff < 0 {
		diff = -diff
	}
	if diff > 1e-9 {
		t.Errorf("%s = %v, want %v", name, *got, want)
	}
}

// Pre-send budget enforcement (CHECKLIST 145): the server counts
// the prompt locally and refuses with 402 BEFORE dialling any
// provider when the estimated prompt cost alone would blow a
// fail-closed budget — and the provider is never called.
func TestPreSendBudgetEnforcement(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	led.FailClosed = true
	keyID := staticKeyID("test-key-123")
	ctx := context.Background()
	// Model-level input price: $10 per 1M input tokens.
	// "hello world" is 3 local tokens (2 words + newline) — the
	// estimated prompt cost is $0.00003.
	book := config.NewPriceBook()
	book.Entries["gpt-4"] = config.ModelPrice{InputUSDPer1M: floatPtr(10), OutputUSDPer1M: floatPtr(30)}
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "gpt-4"}}}
	srv := newTestServer(t, eng, WithLedger(led), WithPriceBook(book))

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello world"}]}`
	// Cap $0.00001: the $0.00003 estimate alone exceeds it.
	if err := led.SetBudget(ctx, keyID, 0.00001, ""); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "estimated prompt cost") || !strings.Contains(rec.Body.String(), "before any provider was called") {
		t.Errorf("402 body = %q, want the estimate named as an estimate and the pre-send refusal", rec.Body.String())
	}
	if eng.chatCalls.Load() != 0 {
		t.Errorf("engine Chat called %d times, want 0 (refused before dialling)", eng.chatCalls.Load())
	}
	// Cap $1: the same request proceeds and reaches the engine.
	if err := led.SetBudget(ctx, keyID, 1.00, ""); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if eng.chatCalls.Load() != 1 {
		t.Errorf("engine Chat called %d times, want 1", eng.chatCalls.Load())
	}
}

// The same pre-send refusal, but with the ROLLING key budget
// (config key_budgets, item 139) instead of the legacy cap: the
// config-driven cap is what operators actually set, and it must
// refuse a prompt that alone exceeds it — this is the regression
// test for the gap the live smoke found (a rolling-only cap was
// invisible to CheckBudgetForEstimate, so the long prompt sailed
// through with a 200).
func TestPreSendBudgetEnforcementRollingKeyBudget(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	led.FailClosed = true
	keyID := staticKeyID("test-key-123")
	ctx := context.Background()
	if err := led.SetKeyBudget(ctx, keyID, 0.0001, "fail_closed"); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}
	book := config.NewPriceBook()
	book.Entries["gpt-4"] = config.ModelPrice{InputUSDPer1M: floatPtr(10), OutputUSDPer1M: floatPtr(30)}
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "gpt-4"}}}
	srv := newTestServer(t, eng, WithLedger(led), WithPriceBook(book))
	// ~200 local tokens ("The quick brown fox..." x20) at $10/1M
	// = ~$0.002 estimated prompt cost vs a $0.0001 rolling cap.
	long := ""
	for i := 0; i < 20; i++ {
		long += "The quick brown fox jumps over the lazy dog. "
	}
	body := `{"model":"gpt-4","messages":[{"role":"user","content":"` + long + `"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "estimated prompt cost") {
		t.Errorf("402 body = %q, want the estimate named as an estimate", rec.Body.String())
	}
	if eng.chatCalls.Load() != 0 {
		t.Errorf("engine Chat called %d times, want 0 (refused before dialling)", eng.chatCalls.Load())
	}
}

// An unpriced pair never refuses on an estimate: the estimate
// cannot be trusted, so only the already-exhausted check applies
// — and an unexhausted budget proceeds.
func TestPreSendBudgetUnpricedProceeds(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	led.FailClosed = true
	keyID := staticKeyID("test-key-123")
	ctx := context.Background()
	if err := led.SetBudget(ctx, keyID, 0.00001, ""); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	// No price book at all: no input price exists for the model.
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "gpt-4"}}}
	srv := newTestServer(t, eng, WithLedger(led))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-4","messages":[{"role":"user","content":"hello world"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unpriced pair must not refuse on a guess; body %q)", rec.Code, rec.Body.String())
	}
	if eng.chatCalls.Load() != 1 {
		t.Errorf("engine Chat called %d times, want 1", eng.chatCalls.Load())
	}
}

// Streaming requests get the same pre-send check: a 402 must
// arrive before any stream is opened.
func TestPreSendBudgetEnforcementStreaming(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	led.FailClosed = true
	keyID := staticKeyID("test-key-123")
	if err := led.SetBudget(context.Background(), keyID, 0.00001, ""); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	book := config.NewPriceBook()
	book.Entries["gpt-4"] = config.ModelPrice{InputUSDPer1M: floatPtr(10), OutputUSDPer1M: floatPtr(30)}
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "gpt-4"}}}
	srv := newTestServer(t, eng, WithLedger(led), WithPriceBook(book))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hello world"}]}`, "test-key-123")
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (body %q)", rec.Code, rec.Body.String())
	}
	if eng.streamCalls.Load() != 0 {
		t.Errorf("engine ChatStream called %d times, want 0", eng.streamCalls.Load())
	}
}

// Prompt-cache economics (CHECKLIST 146): the Overview cache
// block prices cached reads at the configured cached-input price,
// the same tokens at full input price, and cache writes at the
// 1.25–2.5x band — all from real (provider, model) prices, with
// null (never $0) where unpriced.
func TestOverviewCacheEconomics(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	prices := ledger.Prices{
		{Model: "gpt-4o"}: {PromptPerToken: 0.0000025, CompletionPerToken: 0.00001},
	}
	seed := []ledger.Usage{
		// Priced combo: 1000 prompt, 800 cached, 500 written.
		{Provider: "openai", Model: "gpt-4o", PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100,
			CachedTokens: 800, CacheWriteTokens: 500, SplitKnown: true, OK: true, LatencyMS: 100},
		// Unpriced combo: reported cached tokens, no price anywhere.
		{Provider: "anthropic", Model: "claude-x", PromptTokens: 500, CompletionTokens: 50, TotalTokens: 550,
			CachedTokens: 400, CacheWriteTokens: 300, SplitKnown: true, OK: true, LatencyMS: 100},
	}
	for _, u := range seed {
		if _, err := led.RecordUsage(ctx, u, prices); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}
	book := config.NewPriceBook()
	// Pair price for openai/gpt-4o: $2.50/1M input, $1.00/1M cached.
	book.Pairs[config.PriceKey{Provider: "openai", Model: "gpt-4o"}] = config.ModelPrice{
		InputUSDPer1M: floatPtr(2.50), OutputUSDPer1M: floatPtr(10),
		CachedInputUSDPer1M: floatPtr(1.00),
	}
	srv := newTestServer(t, &fakeEngine{}, WithLedger(led), WithPriceBook(book))
	var d overviewResponse
	getJSON(t, srv, "/dashboard/overview?range=24h", &d)

	if d.Cache.CacheWriteTokens != 800 {
		t.Errorf("cache.cache_write_tokens = %d, want 800 (500 + 300)", d.Cache.CacheWriteTokens)
	}
	if len(d.Cache.Combos) != 2 {
		t.Fatalf("combos = %d, want 2", len(d.Cache.Combos))
	}
	var priced, unpriced *cacheCombo
	for i := range d.Cache.Combos {
		if d.Cache.Combos[i].Model == "gpt-4o" {
			priced = &d.Cache.Combos[i]
		}
		if d.Cache.Combos[i].Model == "claude-x" {
			unpriced = &d.Cache.Combos[i]
		}
	}
	if priced == nil || unpriced == nil {
		t.Fatalf("combos = %+v, want gpt-4o + claude-x", d.Cache.Combos)
	}
	// Priced combo: read cost 800 × $1.00/1M = $0.0008; full cost
	// 800 × $2.50/1M = $0.002; write range 500 × $2.50/1M ×
	// [1.25, 2.5] = [$0.0015625, $0.003125].
	moneyNear(t, "gpt-4o cached_read_cost", priced.CachedReadCostUSD, 0.0008)
	moneyNear(t, "gpt-4o full_input_cost", priced.FullInputCostUSD, 0.002)
	moneyNear(t, "gpt-4o write low", priced.CacheWriteCostLowUSD, 0.0015625)
	moneyNear(t, "gpt-4o write high", priced.CacheWriteCostHighUSD, 0.003125)
	// Unpriced combo: every economics leg is null, never $0.
	if unpriced.CachedReadCostUSD != nil || unpriced.FullInputCostUSD != nil ||
		unpriced.CacheWriteCostLowUSD != nil || unpriced.CacheWriteCostHighUSD != nil {
		t.Errorf("claude-x economics = %+v, want all null (unpriced)", *unpriced)
	}
	// Window totals sum only the priced combo.
	moneyNear(t, "window read cost", d.Cache.CachedReadCostUSD, 0.0008)
	moneyNear(t, "window write low", d.Cache.CacheWriteCostLowUSD, 0.0015625)
	// Token-type totals on the Overview itself.
	if d.ReasoningTokens != 0 {
		t.Errorf("reasoning_tokens = %d, want 0 (none reported)", d.ReasoningTokens)
	}
	if d.CacheWriteTokens != 800 {
		t.Errorf("cache_write_tokens = %d, want 800", d.CacheWriteTokens)
	}
}

// Overview token-type totals (CHECKLIST 145): reasoning tokens
// and local-estimate tokens are reported separately from the
// prompt/completion split.
func TestOverviewTokenTypeTotals(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	seed := []ledger.Usage{
		{Provider: "openai", Model: "gpt-4o", PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
			ReasoningTokens: 40, SplitKnown: true, OK: true},
		{Provider: "x", Model: "m", PromptTokens: 42, LocalEstimate: true, OK: true},
	}
	for _, u := range seed {
		if _, err := led.RecordUsage(ctx, u, nil); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}
	srv := newTestServer(t, &fakeEngine{}, WithLedger(led))
	var d overviewResponse
	getJSON(t, srv, "/dashboard/overview?range=24h", &d)
	if d.ReasoningTokens != 40 {
		t.Errorf("reasoning_tokens = %d, want 40", d.ReasoningTokens)
	}
	if d.LocalEstimateTokens != 42 || d.RequestsWithLocalEstimate != 1 {
		t.Errorf("local estimate = %d tokens / %d requests, want 42/1",
			d.LocalEstimateTokens, d.RequestsWithLocalEstimate)
	}
	// The estimate row is also visible in the log feed, labelled.
	var logs struct {
		Events []ledger.UsageEvent `json:"events"`
	}
	getJSON(t, srv, "/dashboard/logs?limit=10&filter=all", &logs)
	if len(logs.Events) != 2 {
		t.Fatalf("log events = %d, want 2", len(logs.Events))
	}
	var est *ledger.UsageEvent
	for i := range logs.Events {
		if logs.Events[i].LocalEstimate {
			est = &logs.Events[i]
		}
	}
	if est == nil {
		t.Fatalf("no local_estimate row in the log feed: %+v", logs.Events)
	}
	if est.PromptTokens != 42 || est.ReasoningTokens != 0 {
		t.Errorf("estimate row = %+v, want prompt=42 reasoning=0", *est)
	}
	var js []byte
	js, _ = json.Marshal(logs)
	if !strings.Contains(string(js), `"local_estimate":true`) {
		t.Errorf("log feed JSON lacks local_estimate:true: %s", js)
	}
}
