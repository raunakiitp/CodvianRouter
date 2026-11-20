// W1 close-out tests (M1.4/M1.5 wiring): the scoped cache key, the
// temperature/truncation store gates, per-tenant flush, and savings
// recording must all hold through the serving path (StaleEngine),
// not just inside internal/cache.
package degrade

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// w1FakeSavingsRecorder is a CacheSavingsRecorder stand-in: it keeps
// every RecordHit call so the test can assert on tenant, tokens and $.
type w1FakeSavingsRecorder struct {
	mu   sync.Mutex
	hits []cache.SavingsRecord
}

func (r *w1FakeSavingsRecorder) RecordHit(_ context.Context, tenant, key string, resp cache.CachedResponse, promptPrice, completionPrice float64) (cache.SavingsRecord, error) {
	rec := cache.SavingsRecord{
		Tenant: tenant, Key: key, Model: resp.Model,
		PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens,
		TokensSaved:  resp.Usage.PromptTokens + resp.Usage.CompletionTokens,
		CostSavedUSD: float64(resp.Usage.PromptTokens)*promptPrice + float64(resp.Usage.CompletionTokens)*completionPrice,
	}
	r.mu.Lock()
	r.hits = append(r.hits, rec)
	r.mu.Unlock()
	return rec, nil
}

// TestW1TwoKeysSamePromptNoCrossHit (M1.4): two gateway keys, same
// prompt — the second key must NOT be served the first key's cached
// answer through the serving path, and the first key must still hit
// its own entry.
func TestW1TwoKeysSamePromptNoCrossHit(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	alice := testReq()
	alice.CallerID = "gk-alice"
	bob := testReq()
	bob.CallerID = "gk-bob"
	if _, err := se.Chat(ctx, alice); err != nil {
		t.Fatalf("alice Chat: %v", err)
	}
	calls := f.chatCalls
	if _, err := se.Chat(ctx, bob); err != nil {
		t.Fatalf("bob Chat: %v", err)
	}
	if f.chatCalls != calls+1 {
		t.Fatalf("bob Chat did not reach the provider (calls %d, want %d): bob was served alice's cached answer", f.chatCalls, calls+1)
	}
	if _, err := se.Chat(ctx, alice); err != nil {
		t.Fatalf("alice second Chat: %v", err)
	}
	if f.chatCalls != calls+1 {
		t.Fatalf("alice second Chat reached the provider (calls %d, want %d): alice lost her own cache hit", f.chatCalls, calls+1)
	}
	if _, _, ok := se.LastCacheHit(); !ok {
		t.Fatal("alice second Chat must report a cache hit")
	}
}

// TestW1TemperatureOneNeverStoredOrServed (M1.4): a temperature-1.0
// response is never stored, and a temperature-1.0 request never
// hits a temperature-0 entry.
func TestW1TemperatureOneNeverStoredOrServed(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	hot := testReq()
	hot.Temperature = 1.0
	if _, err := se.Chat(ctx, hot); err != nil {
		t.Fatalf("hot Chat: %v", err)
	}
	// The hot answer must not have been stored: with the provider
	// now failing, the same hot request must fail, not serve stale.
	f.chatErr = errUpstreamDown
	f.chatResp = nil
	if _, err := se.Chat(ctx, hot); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("hot Chat with failing provider: err = %v, want the upstream error (nothing stored)", err)
	}
	// A cold request stores; a hot request for the same prompt
	// must still miss it.
	f.chatErr = nil
	f.chatResp = freshResp()
	cold := testReq()
	if _, err := se.Chat(ctx, cold); err != nil {
		t.Fatalf("cold Chat: %v", err)
	}
	calls := f.chatCalls
	f.chatErr = errUpstreamDown
	f.chatResp = nil
	if _, err := se.Chat(ctx, hot); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("hot Chat after cold store: err = %v, want the upstream error (temperature buckets must not mix)", err)
	}
	if f.chatCalls != calls+1 {
		t.Fatalf("hot Chat did not reach the provider (calls %d, want %d)", f.chatCalls, calls+1)
	}
}

// TestW1TruncatedResponseNotStored (M1.4): finish_reason=length is
// never stored — a later identical request with a failing provider
// must fail, not serve the truncated answer as stale.
func TestW1TruncatedResponseNotStored(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "partial", FinishReason: "length",
		Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
	}}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	f.chatErr = errUpstreamDown
	f.chatResp = nil
	if _, err := se.Chat(ctx, testReq()); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("second Chat: err = %v, want the upstream error (truncated answer must not be stored)", err)
	}
}

// TestW1RecordHitOnOwnCacheHit (M1.5): a proactive own-cache hit
// calls the savings recorder with the tenant, the tokens saved and
// the dollars saved at the caller-supplied prices.
func TestW1RecordHitOnOwnCacheHit(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	rec := &w1FakeSavingsRecorder{}
	se.SetSavingsRecorder(rec)
	se.SetSavingsPrices(func(model string) (float64, float64) {
		if model != "test-model" {
			t.Errorf("price func called for model %q, want test-model", model)
		}
		return 0.001, 0.002
	})
	ctx := context.Background()
	alice := testReq()
	alice.CallerID = "gk-alice"
	if _, err := se.Chat(ctx, alice); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if _, err := se.Chat(ctx, alice); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.hits) != 1 {
		t.Fatalf("RecordHit calls = %d, want 1 (one proactive hit)", len(rec.hits))
	}
	hit := rec.hits[0]
	if hit.Tenant != "gk-alice" {
		t.Errorf("hit tenant = %q, want gk-alice", hit.Tenant)
	}
	if hit.TokensSaved != 6 {
		t.Errorf("tokens saved = %d, want 6 (5 prompt + 1 completion)", hit.TokensSaved)
	}
	// 5 prompt tokens at $0.001 + 1 completion token at $0.002.
	if hit.CostSavedUSD != 0.007 {
		t.Errorf("dollars saved = %v, want 0.007", hit.CostSavedUSD)
	}
}

// TestW1FlushTenantOnlyRemovesThatTenantsEntries (M1.5): flushing
// one tenant removes exactly that tenant's entries; the other
// tenant keeps hitting.
func TestW1FlushTenantOnlyRemovesThatTenantsEntries(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()
	alice := testReq()
	alice.CallerID = "gk-alice"
	bob := testReq()
	bob.CallerID = "gk-bob"
	if _, err := se.Chat(ctx, alice); err != nil {
		t.Fatalf("alice Chat: %v", err)
	}
	if _, err := se.Chat(ctx, bob); err != nil {
		t.Fatalf("bob Chat: %v", err)
	}
	if n := se.FlushTenant(ctx, "gk-alice"); n != 1 {
		t.Fatalf("FlushTenant(alice) = %d, want 1", n)
	}
	// Bob still hits his own entry: no new provider call.
	calls := f.chatCalls
	if _, err := se.Chat(ctx, bob); err != nil {
		t.Fatalf("bob second Chat: %v", err)
	}
	if f.chatCalls != calls {
		t.Fatalf("bob second Chat reached the provider (calls %d, want %d)", f.chatCalls, calls)
	}
	// Alice misses: the provider is called again.
	if _, err := se.Chat(ctx, alice); err != nil {
		t.Fatalf("alice second Chat: %v", err)
	}
	if f.chatCalls != calls+1 {
		t.Fatalf("alice second Chat did not reach the provider (calls %d, want %d)", f.chatCalls, calls+1)
	}
}
