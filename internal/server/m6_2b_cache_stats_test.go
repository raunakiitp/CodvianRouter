// M6.2b — GET /api/cache/stats end to end over HTTP, with the
// REAL serving engine types: a StaleEngine wrapped with
// degrade.WithSavings reports live savings, and a bare
// StaleEngine (memory-only, no recorder) keeps the honest
// savings_note.
//
// This file is package server_test (external): the internal
// package server tests cannot import internal/degrade (import
// cycle: degrade imports server), so the end-to-end wiring is
// proven here through the server's public API only.
//
// Toggle-verification: these tests FAIL against the pre-M6.2b
// code (restored from backup) — WithSavings does not exist
// there, and WrapWithSemantic takes concrete cache types, so
// the wired engine cannot be built.
package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/degrade"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/server"
)

// m62bFakeEngine is a minimal server.Engine that always answers
// the same response (or fails, when chatErr is set).
type m62bFakeEngine struct {
	chatResp  *provider.ChatResponse
	chatCalls int
}

func (f *m62bFakeEngine) Chat(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	f.chatCalls++
	return f.chatResp, nil
}

func (f *m62bFakeEngine) ChatStream(_ context.Context, _ provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return f.chatResp, nil
}

// m62bCacheStats is the subset of the GET /api/cache/stats
// payload these tests assert on.
type m62bCacheStats struct {
	Savings *struct {
		Hits         int     `json:"hits"`
		TokensSaved  int     `json:"tokens_saved"`
		CostSavedUSD float64 `json:"cost_saved_usd"`
	} `json:"savings"`
	SavingsNote string `json:"savings_note"`
}

func m62bGetCacheStats(t *testing.T, srv *server.Server) (int, m62bCacheStats) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cache/stats", nil))
	var out m62bCacheStats
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /api/cache/stats: %v (body %s)", err, rec.Body.String())
	}
	return rec.Code, out
}

// TestM62bCacheStatsSavingsWiredEndToEnd drives a real proactive
// cache hit through the production engine shape (StaleEngine
// over a PersistentCache, wrapped with WithSavings) and proves
// GET /api/cache/stats reports the live totals — hand-computed
// values, not the same source echoed back: 1 hit, 150 tokens,
// 100*0.001 + 50*0.002 = $0.20 — with no savings_note.
func TestM62bCacheStatsSavingsWiredEndToEnd(t *testing.T) {
	ctx := context.Background()
	pc, err := cache.OpenPersistentCache(filepath.Join(t.TempDir(), "cache.db"), 500)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	defer pc.Close()
	f := &m62bFakeEngine{chatResp: &provider.ChatResponse{
		Content: "4", Model: "test-model",
		Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}}
	se := degrade.WrapWithSemantic(f, pc, nil, degrade.Policy{Enabled: true, AllowStale: true, ServeCache: true}).(*degrade.StaleEngine)
	se.SetSavingsRecorder(pc)
	se.SetSavingsPrices(func(model string) (float64, float64) { return 0.001, 0.002 })
	eng := degrade.WithSavings(se, pc.SavingsTotals)
	req := provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Content: "What is 2+2?"}},
		CallerID: "key-1",
	}
	if _, err := eng.Chat(ctx, req); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if _, err := eng.Chat(ctx, req); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("chatCalls = %d, want 1 (second request is a proactive hit)", f.chatCalls)
	}
	srv := server.New(eng, server.WithCaches(pc, nil), server.WithLogger(log.New(io.Discard, "", 0)))
	code, out := m62bGetCacheStats(t, srv)
	if code != http.StatusOK {
		t.Fatalf("GET /api/cache/stats status = %d, want 200", code)
	}
	if out.Savings == nil {
		t.Fatal("savings absent, want the live totals for the wired PersistentCache")
	}
	if out.Savings.Hits != 1 || out.Savings.TokensSaved != 150 || math.Abs(out.Savings.CostSavedUSD-0.20) > 1e-9 {
		t.Fatalf("savings = %+v, want {hits 1, tokens_saved 150, cost_saved_usd 0.20}", out.Savings)
	}
	if out.SavingsNote != "" {
		t.Errorf("savings_note = %q, want empty when savings are present", out.SavingsNote)
	}
}

// TestM62bCacheStatsMemoryOnlyKeepsNote proves the other half:
// a bare StaleEngine (memory-only caches, no savings recorder,
// no WithSavings wrapper) served over HTTP keeps the savings
// block omitted and the honest savings_note — never a
// zero-filled savings block for a deployment that recorded
// nothing.
func TestM62bCacheStatsMemoryOnlyKeepsNote(t *testing.T) {
	f := &m62bFakeEngine{chatResp: &provider.ChatResponse{Content: "4", Model: "test-model"}}
	exact := cache.NewExactCache(100)
	semantic := cache.NewLocalSemanticCache()
	eng := degrade.WrapWithSemantic(f, exact, semantic, degrade.Policy{Enabled: true, AllowStale: true, ServeCache: true})
	srv := server.New(eng, server.WithCaches(exact, semantic), server.WithLogger(log.New(io.Discard, "", 0)))
	code, out := m62bGetCacheStats(t, srv)
	if code != http.StatusOK {
		t.Fatalf("GET /api/cache/stats status = %d, want 200", code)
	}
	if out.Savings != nil {
		t.Errorf("savings = %+v, want omitted in memory-only mode (no savings recorder wired)", out.Savings)
	}
	if out.SavingsNote == "" {
		t.Error("savings_note empty, want the honest unavailable note in memory-only mode")
	}
}
