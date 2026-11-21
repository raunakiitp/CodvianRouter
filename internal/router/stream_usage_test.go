package router

// G1 (streaming usage accounting) regression tests: a successful
// stream whose upstream reported usage records the PROVIDER's split
// in the ledger (reconciled exactly like a non-streaming report),
// not a local estimate; a stream with no report keeps the labelled
// local-estimate fallback; the drain carries the reported usage to
// the caller. One row per attempt — never a sum, never a duplicate.

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

func TestStreamSuccessRecordsReportedUsage(t *testing.T) {
	led := &fakeLedger{}
	req := provider.ChatRequest{Model: "gpt-4", Messages: []provider.Message{{Role: "user", Content: "hello world"}}}
	local, err := tokencount.CountPromptFull("gpt-4", req)
	if err != nil {
		t.Fatalf("tokencount: %v", err)
	}
	cost := 0.0021
	a := &fakeClient{streamFn: func(ctx context.Context, r provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if err := onDelta("hi"); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{
			Content: "hi", FinishReason: "stop",
			Usage: provider.Usage{
				PromptTokens: local.Tokens, CompletionTokens: 5, TotalTokens: local.Tokens + 5,
				CachedTokens: 2, ReasoningTokens: 3, Cost: &cost,
			},
		}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"p1": a}, led, testConfig())
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4"}}, req)
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	out, err := collectStream(t, ch)
	if err != nil || out != "hi" {
		t.Fatalf("stream = %q err = %v, want hi/nil", out, err)
	}
	if led.count() != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1 (no double-count)", led.count())
	}
	row := led.calls[0]
	if !row.ok {
		t.Fatalf("row ok = false, want true")
	}
	u := row.usage
	if u.LocalEstimate {
		t.Errorf("LocalEstimate = true, want false (upstream reported a split)")
	}
	if u.PromptTokens != local.Tokens || u.CompletionTokens != 5 || u.TotalTokens != local.Tokens+5 {
		t.Errorf("usage = %+v, want the provider's split kept verbatim", u)
	}
	if u.CachedTokens != 2 || u.ReasoningTokens != 3 {
		t.Errorf("usage details = %+v, want cached 2 / reasoning 3", u)
	}
	if u.Cost == nil || *u.Cost != cost {
		t.Errorf("Cost = %v, want the upstream-reported %v preserved", u.Cost, cost)
	}
	if u.LocalEstimatePrompt != local.Tokens {
		t.Errorf("LocalEstimatePrompt = %d, want %d (estimate rides alongside the report)", u.LocalEstimatePrompt, local.Tokens)
	}
	if stats := eng.TokenCountStats(); stats.Compared != 1 {
		t.Errorf("TokenCountStats = %+v, want 1 compared (stream reports reconcile like non-stream)", stats)
	}
}

func TestStreamSuccessNoUsageFallsBackToEstimate(t *testing.T) {
	led := &fakeLedger{}
	req := provider.ChatRequest{Model: "gpt-4", Messages: []provider.Message{{Role: "user", Content: "hello world"}}}
	local, err := tokencount.CountPromptFull("gpt-4", req)
	if err != nil {
		t.Fatalf("tokencount: %v", err)
	}
	a := &fakeClient{streamFn: func(ctx context.Context, r provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if err := onDelta("hi"); err != nil {
			return nil, err
		}
		// Upstream reported NO usage (older compat providers).
		return &provider.ChatResponse{Content: "hi", FinishReason: "stop"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"p1": a}, led, testConfig())
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4"}}, req)
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	if _, err := collectStream(t, ch); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if led.count() != 1 {
		t.Fatalf("ledger rows = %d, want 1", led.count())
	}
	u := led.calls[0].usage
	if !u.LocalEstimate {
		t.Fatalf("LocalEstimate = false, want true (no upstream report): %+v", u)
	}
	if u.PromptTokens != local.Tokens || u.TotalTokens != local.Tokens || u.CompletionTokens != 0 {
		t.Errorf("usage = %+v, want prompt=total=%d completion=0", u, local.Tokens)
	}
}

func TestStreamDrainCarriesReportedUsage(t *testing.T) {
	led := &fakeLedger{}
	req := provider.ChatRequest{Model: "gpt-4", Messages: []provider.Message{{Role: "user", Content: "hello world"}}}
	a := &fakeClient{streamFn: func(ctx context.Context, r provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if err := onDelta("hi"); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{
			Content: "hi", FinishReason: "stop",
			Usage: provider.Usage{PromptTokens: 12, CompletionTokens: 6, TotalTokens: 18},
		}, nil
	}}
	cfg := testConfig()
	cfg.ChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: "p1", Model: model}}
	}
	eng := NewEngine(map[string]provider.Client{"p1": a}, led, cfg)
	resp, err := eng.ChatStreamChunks(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 6 || resp.Usage.TotalTokens != 18 {
		t.Fatalf("drain usage = %+v, want the upstream report carried to the caller", resp.Usage)
	}
	if resp.Content != "hi" || resp.FinishReason != "stop" {
		t.Fatalf("resp = %+v, want content hi / finish stop", resp)
	}
}
