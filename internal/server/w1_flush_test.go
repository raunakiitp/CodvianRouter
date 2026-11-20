// W1 close-out test (M1.5): POST /admin/cache/flush is registered,
// admin-scoped (Tier 2), and flushes through the serving engine's
// per-tenant index: a per-key flush removes exactly that tenant's
// entries and a full flush removes the rest.
package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/degrade"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/server"
)

func w1IssueKey(t *testing.T, cfg *config.Config, name string, scopes []string) (config.GatewayKey, string) {
	t.Helper()
	k, raw, err := config.IssueKey(name, scopes, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(k); err != nil {
		t.Fatal(err)
	}
	return k, raw
}

func TestW1CacheFlushEndpoint(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	_, adminRaw := w1IssueKey(t, cfg, "admin-user", []string{"chat", "models", "admin"})
	_, chatRaw := w1IssueKey(t, cfg, "chat-user", []string{"chat"})
	aliceKey, _ := w1IssueKey(t, cfg, "alice", []string{"chat"})
	bobKey, _ := w1IssueKey(t, cfg, "bob", []string{"chat"})

	exact := cache.NewExactCache(100)
	semantic := cache.NewLocalSemanticCache()
	policy := degrade.DefaultPolicy()
	policy.Enabled = true
	policy.ServeCache = true
	eng := degrade.WrapWithSemantic(&extChunkEngine{
		resp: provider.ChatResponse{
			Content: "4", Model: "test-model", FinishReason: "stop",
			Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
		},
	}, exact, semantic, policy)
	se, ok := eng.(*degrade.StaleEngine)
	if !ok {
		t.Fatalf("WrapWithSemantic returned %T, want *degrade.StaleEngine", eng)
	}
	ctx := context.Background()
	aliceReq := provider.ChatRequest{Model: "test-model", Messages: []provider.Message{{Role: "user", Content: "What is 2+2?"}}, CallerID: aliceKey.ID}
	bobReq := provider.ChatRequest{Model: "test-model", Messages: []provider.Message{{Role: "user", Content: "What is 2+2?"}}, CallerID: bobKey.ID}
	if _, err := se.Chat(ctx, aliceReq); err != nil {
		t.Fatalf("alice Chat: %v", err)
	}
	if _, err := se.Chat(ctx, bobReq); err != nil {
		t.Fatalf("bob Chat: %v", err)
	}

	srv := server.New(eng, server.WithConfig(cfg), server.WithLogger(log.New(io.Discard, "", 0)))
	doPost := func(body, key string) *httptest.ResponseRecorder {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(http.MethodPost, "/admin/cache/flush", rdr)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	// No key: 401.
	if rec := doPost("{}", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("flush without key: status %d, want 401", rec.Code)
	}
	// A valid key without admin scope: 403 naming the scope.
	if rec := doPost("{}", chatRaw); rec.Code != http.StatusForbidden {
		t.Errorf("flush with chat-only key: status %d, want 403", rec.Code)
	}
	// Admin key, per-key flush: exactly alice's entry goes.
	rec := doPost(`{"key_id":"`+aliceKey.ID+`"}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("flush alice: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Flushed int    `json:"flushed"`
		Scope   string `json:"scope"`
		KeyID   string `json:"key_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("flush alice body: %v", err)
	}
	if out.Flushed != 1 || out.Scope != "key" || out.KeyID != aliceKey.ID {
		t.Errorf("flush alice = %+v, want {flushed:1 scope:key key_id:%s}", out, aliceKey.ID)
	}
	// Alice misses now; bob still hits his own entry.
	if _, err := se.Chat(ctx, aliceReq); err != nil {
		t.Fatalf("alice Chat after flush: %v", err)
	}
	if _, _, ok := se.LastCacheHit(); ok {
		t.Error("alice Chat after her flush must not report a cache hit")
	}
	if _, err := se.Chat(ctx, bobReq); err != nil {
		t.Fatalf("bob Chat after alice flush: %v", err)
	}
	if _, _, ok := se.LastCacheHit(); !ok {
		t.Error("bob Chat after alice flush must still report a cache hit")
	}
	// Admin key, full flush: bob's entry goes, plus alice's entry
	// re-stored by the verification Chat above.
	rec = doPost("{}", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("flush all: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("flush all body: %v", err)
	}
	if out.Flushed != 2 || out.Scope != "all" {
		t.Errorf("flush all = %+v, want {flushed:2 scope:all}", out)
	}
	// The exact layer is empty after a full flush — but the
	// semantic layer has no flush API yet (internal/cache
	// follow-up), so bob's next request is still served
	// semantically for the same text. That hit must name the
	// semantic layer, proving it did not come from the flushed
	// exact layer.
	if exact.Size() != 0 {
		t.Errorf("exact cache size after flush-all = %d, want 0", exact.Size())
	}
	if _, err := se.Chat(ctx, bobReq); err != nil {
		t.Fatalf("bob Chat after full flush: %v", err)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "semantic" {
		t.Errorf("bob Chat after full flush: LastCacheHit = (%q, %v), want (semantic, true)", kind, ok)
	}
}
