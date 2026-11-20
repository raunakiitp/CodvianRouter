// StaleEngine embeddings/rerank forwarding (CHECKLIST 171-172):
// replay-exempt calls are forwarded to the wrapped engine with
// no cache lookup, no proactive serving and no stale fallback —
// serving a cached or stale vector would hand out another
// request's embedding and hide the upstream call the ledger
// must record.
package degrade

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// embedFakeEngine is the degrade fakeEngine with Embed/Rerank.
type embedFakeEngine struct {
	*fakeEngine
	embedCalls  int
	rerankCalls int
	embedResp   *provider.EmbeddingResponse
	embedErr    error
	rerankResp  *provider.RerankResponse
	rerankErr   error
}

func (f *embedFakeEngine) Embed(_ context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.embedCalls++
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	if f.embedResp != nil {
		return f.embedResp, nil
	}
	return &provider.EmbeddingResponse{
		Model: req.Model,
		Data:  []provider.Embedding{{Object: "embedding", Embedding: json.RawMessage(`[0.5]`), Index: 0}},
		Usage: provider.Usage{PromptTokens: 2, TotalTokens: 2},
	}, nil
}

func (f *embedFakeEngine) Rerank(_ context.Context, req provider.RerankRequest) (*provider.RerankResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rerankCalls++
	if f.rerankErr != nil {
		return nil, f.rerankErr
	}
	if f.rerankResp != nil {
		return f.rerankResp, nil
	}
	return &provider.RerankResponse{
		Model:   req.Model,
		Results: []provider.RerankResult{{Index: 0, RelevanceScore: 0.7}},
	}, nil
}

func TestStaleEngineForwardsEmbeddingsWithoutCache(t *testing.T) {
	f := &embedFakeEngine{fakeEngine: &fakeEngine{chatResp: freshResp()}}
	se, _ := wrapTest(f, DefaultPolicy())
	ctx := context.Background()

	// Two identical embeddings calls: each one reaches the
	// wrapped engine (no cache serving — the vector is fresh
	// work, not a cacheable answer).
	req := provider.EmbeddingRequest{Model: "m", Input: json.RawMessage(`"hello"`)}
	resp1, err := se.Embed(ctx, req)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	resp2, err := se.Embed(ctx, req)
	if err != nil {
		t.Fatalf("Embed (2nd): %v", err)
	}
	if f.embedCalls != 2 {
		t.Errorf("embed calls = %d, want 2 (never served from cache)", f.embedCalls)
	}
	if string(resp1.Data[0].Embedding) != string(resp2.Data[0].Embedding) {
		t.Errorf("vectors differ: %s vs %s", resp1.Data[0].Embedding, resp2.Data[0].Embedding)
	}
	// The wrapper never reports a stale serve for an embeddings
	// call.
	if se.LastServedStale() {
		t.Error("LastServedStale = true after embeddings calls, want false")
	}
}

func TestStaleEngineEmbedFailureHasNoStaleFallback(t *testing.T) {
	f := &embedFakeEngine{fakeEngine: &fakeEngine{chatResp: freshResp()}, embedErr: errors.New("upstream down")}
	policy := DefaultPolicy()
	policy.Enabled = true
	se, _ := wrapTest(f, policy)
	ctx := context.Background()

	// Populate the chat cache first, so a stale fallback would
	// have something to serve if the wrapper tried.
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, err := se.Embed(ctx, provider.EmbeddingRequest{Model: "m", Input: json.RawMessage(`"hello"`)})
	if err == nil {
		t.Fatal("want the upstream error, got nil (a stale fallback must never serve an embeddings call)")
	}
	if se.LastServedStale() {
		t.Error("LastServedStale = true after a failed embeddings call, want false")
	}
}

func TestStaleEngineForwardsRerankWithoutCache(t *testing.T) {
	f := &embedFakeEngine{fakeEngine: &fakeEngine{chatResp: freshResp()}}
	se, _ := wrapTest(f, DefaultPolicy())
	ctx := context.Background()

	req := provider.RerankRequest{Model: "m", Query: "q", Documents: []json.RawMessage{json.RawMessage(`"d"`)}}
	if _, err := se.Rerank(ctx, req); err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if _, err := se.Rerank(ctx, req); err != nil {
		t.Fatalf("Rerank (2nd): %v", err)
	}
	if f.rerankCalls != 2 {
		t.Errorf("rerank calls = %d, want 2 (never served from cache)", f.rerankCalls)
	}
}

func TestStaleEngineEmbedWithoutCapabilityErrors(t *testing.T) {
	// A wrapped engine with no Embed/Rerank answers an honest
	// error, never a faked vector or scoring.
	f := &fakeEngine{chatResp: freshResp()}
	se, _ := wrapTest(f, DefaultPolicy())
	ctx := context.Background()
	if _, err := se.Embed(ctx, provider.EmbeddingRequest{Model: "m", Input: json.RawMessage(`"hello"`)}); err == nil || !strings.Contains(err.Error(), "does not support embeddings") {
		t.Errorf("Embed err = %v, want a 'does not support embeddings' error", err)
	}
	if _, err := se.Rerank(ctx, provider.RerankRequest{Model: "m", Query: "q", Documents: []json.RawMessage{json.RawMessage(`"d"`)}}); err == nil || !strings.Contains(err.Error(), "does not support rerank") {
		t.Errorf("Rerank err = %v, want a 'does not support rerank' error", err)
	}
}
