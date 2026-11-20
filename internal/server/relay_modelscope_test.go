package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/metrics"
)

// Model scoping (CHECKLIST 187-188) — the bug this slice fixes,
// found live by the 163-164 smoke: the semantic vector is
// text-only, so a cached claude-sonnet response was served for a
// gpt-4o request in 0ms. The /v1/messages relay cache and the
// dashboard Overview split are covered here; the engine path is
// covered in internal/degrade, the cache itself in internal/cache.

// TestMessagesSemanticCacheNeverServesCrossModel is the 163-164
// repro through the relay: a claude-sonnet-4-5 response relayed
// verbatim is stored (exact + semantic, scoped to the request
// model); an identical-text /v1/messages request naming gpt-4o
// must NOT be served that cached response — it takes the translate
// path to the engine instead.
func TestMessagesSemanticCacheNeverServesCrossModel(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	exact := cache.NewExactCache(500)
	semantic := cache.NewLocalSemanticCache()
	eng := &fakeEngine{chatErr: errors.New("engine dialled for gpt-4o")}
	srv := newMessagesServer(t, eng, mock, WithCacheServing(true), WithCaches(exact, semantic))

	text := "Describe the monsoon season in Mumbai."
	rawClaude := `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"` + text + `"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", rawClaude, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("claude status = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"Hello from Claude"`) {
		t.Fatalf("claude body = %s, want Hello from Claude", rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Relay"); got != "verbatim" {
		t.Fatalf("claude X-Codvian-Relay = %q, want verbatim", got)
	}

	// Identical text, other model: the exact key already differs,
	// so only the semantic layer could wrongly hit. The request
	// must take the translate path (no gpt-4o relay target) and
	// reach the engine — which fails loudly here, proving the
	// request went live instead of being served claude's answer.
	rawGPT := `{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"` + text + `"}]}`
	rec = doReq(t, srv, http.MethodPost, "/v1/messages", rawGPT, "test-key-123")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("gpt-4o status = %d, body %s, want 502 (engine dialled, no cache hit)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Hello from Claude") {
		t.Fatalf("gpt-4o body served the cached claude response: %s", rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Cache"); got != "" {
		t.Fatalf("gpt-4o X-Codvian-Cache = %q, want absent (no cache hit)", got)
	}
	mock.mu.Lock()
	nMock := len(mock.bodies)
	mock.mu.Unlock()
	if nMock != 1 {
		t.Fatalf("upstream mock called %d times, want 1 (claude relay only)", nMock)
	}
}

// TestMessagesSemanticCacheSameModelStillHits proves the relay
// layer still works after scoping: the same model, different
// history but the same last user text, is served semantically in
// the Anthropic shape without calling any provider.
func TestMessagesSemanticCacheSameModelStillHits(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	exact := cache.NewExactCache(500)
	semantic := cache.NewLocalSemanticCache()
	srv := newMessagesServer(t, &fakeEngine{}, mock, WithCacheServing(true), WithCaches(exact, semantic))

	text := "Describe the monsoon season in Mumbai."
	raw1 := `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"` + text + `"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw1, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	// Different history (system message added), same last user
	// text: exact misses, semantic must hit for the same model.
	raw2 := `{"model":"claude-sonnet-4-5","max_tokens":64,"system":"Be brief.","messages":[{"role":"user","content":"` + text + `"}]}`
	rec = doReq(t, srv, http.MethodPost, "/v1/messages", raw2, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Cache"); got != "semantic" {
		t.Fatalf("X-Codvian-Cache = %q, want semantic", got)
	}
	if !strings.Contains(rec.Body.String(), `"text":"Hello from Claude"`) {
		t.Fatalf("semantic-hit body = %s, want Hello from Claude", rec.Body.String())
	}
	mock.mu.Lock()
	nMock := len(mock.bodies)
	mock.mu.Unlock()
	if nMock != 1 {
		t.Fatalf("upstream mock called %d times, want 1 (no provider call on a hit)", nMock)
	}
}

// TestOverviewCacheSplitByLayer proves GET /dashboard/overview
// reports the gateway cache hits/misses split by layer, live from
// the recorder — the numbers the dashboard renders next to the
// hit rate (CHECKLIST 188).
func TestOverviewCacheSplitByLayer(t *testing.T) {
	recorder := metrics.NewRecorder()
	srv := newTestServer(t, &fakeEngine{}, WithMetrics(recorder))
	recorder.IncCacheHit("exact")
	recorder.IncCacheHit("exact")
	recorder.IncCacheHit("semantic")
	recorder.IncCacheMiss("exact")
	recorder.IncCacheMiss("semantic")
	recorder.IncCacheMiss("semantic")

	rec := doReq(t, srv, http.MethodGet, "/dashboard/overview?range=24h", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status = %d, want 200", rec.Code)
	}
	var out struct {
		CacheHits           uint64 `json:"cache_hits"`
		CacheMisses         uint64 `json:"cache_misses"`
		CacheHitsExact      uint64 `json:"cache_hits_exact"`
		CacheHitsSemantic   uint64 `json:"cache_hits_semantic"`
		CacheMissesExact    uint64 `json:"cache_misses_exact"`
		CacheMissesSemantic uint64 `json:"cache_misses_semantic"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if out.CacheHits != 3 || out.CacheMisses != 3 {
		t.Fatalf("totals = %d/%d, want 3/3", out.CacheHits, out.CacheMisses)
	}
	if out.CacheHitsExact != 2 || out.CacheHitsSemantic != 1 {
		t.Fatalf("hits split = exact %d / semantic %d, want 2/1", out.CacheHitsExact, out.CacheHitsSemantic)
	}
	if out.CacheMissesExact != 1 || out.CacheMissesSemantic != 2 {
		t.Fatalf("misses split = exact %d / semantic %d, want 1/2", out.CacheMissesExact, out.CacheMissesSemantic)
	}
}

// TestOverviewCacheSplitZeroBeforeTraffic: with no cache lookups
// the split reads honest zeros, never an invented split.
func TestOverviewCacheSplitZeroBeforeTraffic(t *testing.T) {
	recorder := metrics.NewRecorder()
	srv := newTestServer(t, &fakeEngine{}, WithMetrics(recorder))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/overview?range=24h", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status = %d, want 200", rec.Code)
	}
	var out struct {
		CacheHitsExact      uint64 `json:"cache_hits_exact"`
		CacheHitsSemantic   uint64 `json:"cache_hits_semantic"`
		CacheMissesExact    uint64 `json:"cache_misses_exact"`
		CacheMissesSemantic uint64 `json:"cache_misses_semantic"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if out.CacheHitsExact != 0 || out.CacheHitsSemantic != 0 || out.CacheMissesExact != 0 || out.CacheMissesSemantic != 0 {
		t.Fatalf("split before traffic = %+v, want all zero", out)
	}
}
