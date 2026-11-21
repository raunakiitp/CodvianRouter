package degrade

// Phase 2 carry-over closure: an exact-cache replay must report the SAME
// usage shape as the original live response. Before this slice the entry
// stored only prompt/completion/total and the replay dropped the
// provider-reported cached/reasoning detail splits.

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestExactCacheReplayPreservesUsageDetails(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "4",
		Usage: provider.Usage{
			PromptTokens: 12, CompletionTokens: 6, TotalTokens: 18,
			CachedTokens: 7, ReasoningTokens: 3,
		},
		Model: "test-model",
	}}
	se, _ := wrapTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	f.mu.Lock()
	f.chatCalls = 0
	f.mu.Unlock()

	resp, err := se.Chat(ctx, testReq()) // exact-cache hit
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	f.mu.Lock()
	calls := f.chatCalls
	f.mu.Unlock()
	if calls != 0 {
		t.Fatalf("engine calls on hit = %d, want 0 (served from cache)", calls)
	}
	u := resp.Usage
	if u.PromptTokens != 12 || u.CompletionTokens != 6 || u.TotalTokens != 18 {
		t.Errorf("replayed base usage = %+v, want 12/6/18", u)
	}
	if u.CachedTokens != 7 || u.ReasoningTokens != 3 {
		t.Errorf("replayed detail splits = cached %d / reasoning %d, want 7 / 3 (dropped by replay before this slice)", u.CachedTokens, u.ReasoningTokens)
	}
}

func TestExactCacheStoreClampsNegativeUsageDetails(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "4",
		Usage: provider.Usage{
			PromptTokens: 12, CompletionTokens: 6, TotalTokens: 18,
			CachedTokens: -5, ReasoningTokens: -2,
		},
		Model: "test-model",
	}}
	se, _ := wrapTest(f, serveCachePolicy())
	ctx := context.Background()
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	resp, err := se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if resp.Usage.CachedTokens != 0 || resp.Usage.ReasoningTokens != 0 {
		t.Fatalf("negative details stored verbatim: %+v", resp.Usage)
	}
}
