// M6.2b — serve-mode write-through: WrapWithSemantic accepts the
// persistent wrappers, the admin flush is durable through the
// wrapper, and savings reporting exists only when a real
// PersistentCache is wired.
//
// Toggle-verification: these tests FAIL against the pre-M6.2b
// code (restored from backup) — WrapWithSemantic takes concrete
// *cache.ExactCache / *cache.LocalSemanticCache there, so passing
// a *cache.PersistentCache (or psc.AsStore()) does not compile,
// and WithSavings does not exist.
package degrade

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// savingsReporter is the structural interface GET /api/cache/stats
// reads through (mirrors internal/server's cacheSavingsReporter).
type m62bSavingsReporter interface {
	SavingsTotals() cache.SavingsTotals
}

// TestM62bTypedNilCacheIsPassthrough is the regression test for
// the interface-typed-nil hazard: a nil *cache.ExactCache /
// *cache.LocalSemanticCache passed to WrapWithSemantic must
// behave exactly like an absent layer (pass-through, no panic),
// not like a present cache with a nil receiver.
func TestM62bTypedNilCacheIsPassthrough(t *testing.T) {
	ctx := context.Background()
	f := &fakeEngine{chatResp: &provider.ChatResponse{Content: "4", Model: "test-model"}}
	var nilExact *cache.ExactCache
	var nilSemantic *cache.LocalSemanticCache
	eng := WrapWithSemantic(f, nilExact, nilSemantic, Policy{Enabled: true, AllowStale: true})
	resp, err := eng.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("Chat with typed-nil caches: %v", err)
	}
	if resp.Content != "4" {
		t.Fatalf("Chat content = %q, want provider answer %q", resp.Content, "4")
	}
	// And the failure path: no stale fallback exists, so the
	// original error comes back unchanged.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	if _, err := eng.Chat(ctx, testReq()); err != errUpstreamDown {
		t.Fatalf("Chat error = %v, want %v", err, errUpstreamDown)
	}
}

// TestM62bDegradeWriteThrough proves serve mode is TRUE
// write-through once the persistent wrappers are passed in: one
// Chat stores the response, and with no Sync anywhere both disk
// tables hold it after a restart. A second engine over the
// reopened caches then serves the same request proactively
// without calling any provider.
func TestM62bDegradeWriteThrough(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pc, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 500)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	psc, err := cache.OpenPersistentSemanticCache(filepath.Join(dir, "sem.db"), cache.LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "4", Model: "test-model",
		Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}}
	policy := Policy{Enabled: true, AllowStale: true, ServeCache: true}
	eng := WrapWithSemantic(f, pc, psc.AsStore(), policy)
	req := testReq()
	req.CallerID = "key-1"
	if _, err := eng.Chat(ctx, req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("chatCalls = %d, want 1 (first request goes to the provider)", f.chatCalls)
	}
	// No Sync anywhere: close both files and reopen.
	if err := pc.Close(); err != nil {
		t.Fatalf("pc Close: %v", err)
	}
	if err := psc.Close(); err != nil {
		t.Fatalf("psc Close: %v", err)
	}
	pc2, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 500)
	if err != nil {
		t.Fatalf("reopen exact: %v", err)
	}
	defer pc2.Close()
	psc2, err := cache.OpenPersistentSemanticCache(filepath.Join(dir, "sem.db"), cache.LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen semantic: %v", err)
	}
	defer psc2.Close()
	if pc2.Size() != 1 {
		t.Fatalf("reopened exact Size = %d, want 1 (Chat stored without any Sync)", pc2.Size())
	}
	if got, ok := psc2.GetTextForModel(ctx, "test-model", "What is 2+2?"); !ok || got.Response.Content != "4" {
		t.Fatalf("reopened semantic GetTextForModel = %+v, %v; want hit with content %q", got, ok, "4")
	}
	// A fresh engine over the reopened caches serves the same
	// request proactively — the provider is never called.
	f2 := &fakeEngine{chatResp: &provider.ChatResponse{Content: "fresh", Model: "test-model"}}
	eng2 := WrapWithSemantic(f2, pc2, psc2.AsStore(), policy)
	resp, err := eng2.Chat(ctx, req)
	if err != nil {
		t.Fatalf("second-engine Chat: %v", err)
	}
	if resp.Content != "4" {
		t.Fatalf("second-engine Chat content = %q, want cached %q", resp.Content, "4")
	}
	if f2.chatCalls != 0 {
		t.Fatalf("second-engine chatCalls = %d, want 0 (proactive cache hit)", f2.chatCalls)
	}
}

// TestM62bDegradeFlushDurable proves the admin flush is durable
// through the wrapper: FlushTenant removes one tenant's disk
// rows at once (the other tenant survives), and FlushAll followed
// by a Sync (the 1-minute ticker) leaves nothing to resurrect.
func TestM62bDegradeFlushDurable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pc, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 500)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "4", Model: "test-model",
		Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}}
	policy := Policy{Enabled: true, AllowStale: true, ServeCache: true}
	se := WrapWithSemantic(f, pc, nil, policy).(*StaleEngine)
	for _, tenant := range []string{"key-1", "key-2"} {
		req := testReq()
		req.CallerID = tenant
		if _, err := se.Chat(ctx, req); err != nil {
			t.Fatalf("Chat(%s): %v", tenant, err)
		}
	}
	if pc.Size() != 2 {
		t.Fatalf("Size = %d, want 2 (one entry per tenant)", pc.Size())
	}
	if n := se.FlushTenant(ctx, "key-1"); n != 1 {
		t.Fatalf("FlushTenant(key-1) = %d, want 1", n)
	}
	// The ticker fires after the flush: it must not resurrect key-1.
	if _, err := pc.Sync(ctx); err != nil {
		t.Fatalf("Sync after FlushTenant: %v", err)
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pc2, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 500)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer pc2.Close()
	if pc2.Size() != 1 {
		t.Fatalf("reopened Size = %d, want 1 (key-1 flushed, key-2 kept)", pc2.Size())
	}
	// key-2's request still hits proactively; key-1's misses.
	f2 := &fakeEngine{chatResp: &provider.ChatResponse{Content: "fresh", Model: "test-model"}}
	se2 := WrapWithSemantic(f2, pc2, nil, policy).(*StaleEngine)
	req2 := testReq()
	req2.CallerID = "key-2"
	if resp, err := se2.Chat(ctx, req2); err != nil || resp.Content != "4" {
		t.Fatalf("key-2 Chat = %+v, %v; want cached %q", resp, err, "4")
	}
	if f2.chatCalls != 0 {
		t.Fatalf("key-2 chatCalls = %d, want 0 (proactive hit)", f2.chatCalls)
	}
	req1 := testReq()
	req1.CallerID = "key-1"
	if resp, err := se2.Chat(ctx, req1); err != nil || resp.Content != "fresh" {
		t.Fatalf("key-1 Chat = %+v, %v; want fresh provider answer (flushed)", resp, err)
	}
	if f2.chatCalls != 1 {
		t.Fatalf("key-1 chatCalls = %d, want 1 (miss after flush)", f2.chatCalls)
	}

	// FlushAll, then a Sync, then a restart: nothing comes back.
	if n := se2.FlushAll(ctx); n != 2 {
		t.Fatalf("FlushAll = %d, want 2", n)
	}
	if _, err := pc2.Sync(ctx); err != nil {
		t.Fatalf("Sync after FlushAll: %v", err)
	}
	if err := pc2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pc3, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 500)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer pc3.Close()
	if pc3.Size() != 0 {
		t.Fatalf("reopened Size = %d, want 0 after FlushAll + Sync", pc3.Size())
	}
}

// TestM62bDegradeSavingsViaDecorator proves the savings wiring:
// with a PersistentCache recorder + prices, a proactive cache
// hit records real tokens + dollars, the decorator reports the
// live totals (which survive a restart), and — the negative
// half — a bare StaleEngine with no recorder does NOT implement
// the savings reporter, so GET /api/cache/stats keeps its
// savings_note in memory-only mode instead of a zero-filled
// savings block.
func TestM62bDegradeSavingsViaDecorator(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pc, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 500)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "4", Model: "test-model",
		Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}}
	policy := Policy{Enabled: true, AllowStale: true, ServeCache: true}
	se := WrapWithSemantic(f, pc, nil, policy).(*StaleEngine)
	se.SetSavingsRecorder(pc)
	se.SetSavingsPrices(func(model string) (float64, float64) { return 0.001, 0.002 })
	eng := WithSavings(se, pc.SavingsTotals)
	req := testReq()
	req.CallerID = "key-1"
	if _, err := eng.Chat(ctx, req); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if _, err := eng.Chat(ctx, req); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("chatCalls = %d, want 1 (second request is a proactive hit)", f.chatCalls)
	}
	// Hand-computed: 1 hit, 100+50 = 150 tokens,
	// 100*0.001 + 50*0.002 = $0.20.
	rep, ok := eng.(m62bSavingsReporter)
	if !ok {
		t.Fatal("WithSavings engine does not implement SavingsTotals; the stats endpoint could not read it")
	}
	tot := rep.SavingsTotals()
	if tot.Hits != 1 || tot.TokensSaved != 150 || math.Abs(tot.CostSavedUSD-0.20) > 1e-9 {
		t.Fatalf("SavingsTotals = %+v, want {hits 1, tokens_saved 150, cost_saved_usd 0.20}", tot)
	}
	// Totals survive a restart (rows live in the same SQLite file).
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pc2, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 500)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer pc2.Close()
	if tot2 := pc2.SavingsTotals(); tot2.Hits != tot.Hits || tot2.TokensSaved != tot.TokensSaved || math.Abs(tot2.CostSavedUSD-tot.CostSavedUSD) > 1e-9 {
		t.Fatalf("reopened SavingsTotals = %+v, want %+v", tot2, tot)
	}

	// Negative half: a bare StaleEngine (memory-only, no
	// recorder) must NOT implement the reporter — otherwise the
	// stats endpoint would emit a zero savings block for a
	// deployment that recorded nothing.
	bare := WrapWithSemantic(f, cache.NewExactCache(100), nil, policy)
	if _, ok := bare.(m62bSavingsReporter); ok {
		t.Fatal("bare StaleEngine implements SavingsTotals; memory-only mode would zero-fill the savings block")
	}
}
