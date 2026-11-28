// G17 Area 11 — cost-saved header tests.
//
// The header rides in the single choke-point writer
// (writeResponseHeaders) alongside X-Codvian-Cache-Saved-USD: same
// value, same format, emitted only when the saving was priced.
// A cache MISS never carries it, and a hit whose model has no
// configured price never carries it either — the cost is unknown,
// never a fabricated 0. A memory-only (no persistence) hit is the
// same case at the header level: withCacheSavings prices from the
// ledger map, not from the cache_savings table, so persistence is
// irrelevant here (the table feeds only the aggregate
// GET /api/cache/stats block, whose savings_note covers the
// memory-only case). The fake engine in the end-to-end test below
// IS memory-only — no PersistentCache anywhere — and still
// carries the header when priced, which proves the point.
package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestCostSavedHeaderOnCacheHit(t *testing.T) {
	// Unit-level: the choke-point writer emits the the sector baseline
	// name with the same value as the Codvian name.
	srv := newTestServer(t, &fakeEngine{}, WithVersion("v-test"))
	w := httptest.NewRecorder()
	meta := ResponseMeta{
		Cache: "exact", CacheSavedTokens: 150,
		CacheSavedUSD: 0.00015, HasCacheSavings: true, HasCacheSavedUSD: true,
	}
	srv.writeResponseHeaders(w, nil, meta)
	mustHeader(t, w, "X-Codvian-Cache-Saved-USD", "0.0001500000")
	mustHeader(t, w, "X-Router-Cost-Saved", "0.0001500000")

	// Tokens known but cost unpriced (no price for the model):
	// the token header stays, both USD headers stay absent —
	// never a fabricated 0.
	w = httptest.NewRecorder()
	meta = ResponseMeta{Cache: "exact", CacheSavedTokens: 150, HasCacheSavings: true}
	srv.writeResponseHeaders(w, nil, meta)
	mustHeaderInt(t, w, "X-Codvian-Cache-Saved-Tokens", 150)
	mustHeaderAbsent(t, w, "X-Codvian-Cache-Saved-USD")
	mustHeaderAbsent(t, w, "X-Router-Cost-Saved")

	// A free ($0-priced) model hit emits an explicit 0 — the
	// sector baseline rule that 0 is a valid saved cost.
	w = httptest.NewRecorder()
	meta = ResponseMeta{
		Cache: "exact", CacheSavedTokens: 150,
		CacheSavedUSD: 0, HasCacheSavings: true, HasCacheSavedUSD: true,
	}
	srv.writeResponseHeaders(w, nil, meta)
	mustHeader(t, w, "X-Router-Cost-Saved", "0.0000000000")
}

func TestCostSavedHeaderUsesPairPriceFirst(t *testing.T) {
	// G17 re-review nit 3: withCacheSavings prices pair-first,
	// model fallback — prove it with rates that differ between
	// the two levels. Pair (prov-b, model-b): 10e-6/20e-6;
	// model-level model-b: 1e-6/2e-6. 100 prompt + 50 completion
	// tokens: pair price 100*10e-6 + 50*20e-6 = 0.002; model
	// price would be 0.0002.
	srv := newTestServer(t, &fakeEngine{}, WithVersion("v-test"))
	srv.SetLedgerPrices(ledger.Prices{
		{Provider: "prov-b", Model: "model-b"}: {PromptPerToken: 10e-6, CompletionPerToken: 20e-6},
		{Model: "model-b"}:                     {PromptPerToken: 1e-6, CompletionPerToken: 2e-6},
	})
	meta := srv.withCacheSavings(ResponseMeta{
		Provider: "prov-b", Model: "model-b", HasUsage: true,
		Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	})
	if !meta.HasCacheSavedUSD || math.Abs(meta.CacheSavedUSD-0.002) > 1e-12 {
		t.Fatalf("CacheSavedUSD = %v (has=%v), want 0.002 (pair price, not model-level 0.0002)", meta.CacheSavedUSD, meta.HasCacheSavedUSD)
	}
}

func TestCostSavedHeaderEndToEndCacheHit(t *testing.T) {
	// Memory-only cache hit (fake engine, no persistence): 100
	// prompt + 50 completion tokens at the model-b rates =
	// 100*1e-6 + 50*2e-6 = 0.0002 USD saved.
	eng := &headerTestEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{
			Content: "cached answer", Model: "model-b",
			Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		}},
		cacheHit: true, cacheKind: "exact",
	}
	srv := newTestServer(t, eng, WithVersion("test-1.2.3"))
	srv.SetLedgerPrices(headerPrices())
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mustHeader(t, rec, "X-Codvian-Cache", "exact")
	mustHeader(t, rec, "X-Codvian-Cost-USD", "0.0000000000")
	mustHeader(t, rec, "X-Codvian-Cache-Saved-USD", "0.0002000000")
	mustHeader(t, rec, "X-Router-Cost-Saved", "0.0002000000")
}

func TestCostSavedHeaderAbsentOnCacheMiss(t *testing.T) {
	// A normal (non-cache) response carries no cost-saved header
	// of either name: nothing was saved.
	srv, _ := newHeaderServer(t)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mustHeaderAbsent(t, rec, "X-Codvian-Cache-Saved-USD")
	mustHeaderAbsent(t, rec, "X-Router-Cost-Saved")
	mustHeaderAbsent(t, rec, "X-Codvian-Cache")
}

func TestCostSavedInStreamTrailer(t *testing.T) {
	// The SSE trailer carries the same saving under the
	// the sector baseline name, so a stream reader sees what a header
	// reader sees.
	meta := ResponseMeta{
		Cache: "semantic", CacheSimilarity: 0.923, HasCacheSimilarity: true,
		CacheSavedTokens: 150, CacheSavedUSD: 0.00015,
		HasCacheSavings: true, HasCacheSavedUSD: true,
	}
	b, err := meta.trailerJSON()
	if err != nil {
		t.Fatalf("trailerJSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("trailer JSON: %v: %s", err, b)
	}
	mustTrailerFloat(t, m, "cache_saved_usd", 0.00015)
	mustTrailerFloat(t, m, "cost_saved_usd", 0.00015)
}
