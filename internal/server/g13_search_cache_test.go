// G13 — POST /v1/search cache wiring tests (PARITY.md Area 6 row
// "Search cache (60 s)"): the SearchCache is wired into the live
// search path (internal/server/media_verify.go), so a repeated
// identical search inside the 60 s TTL is served without a second
// upstream call, and a ttl <= 0 server bypasses the cache AND the
// coalescing. The fake engine counts upstream calls, so the cache
// is proven by the call count, not by trusting a header.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func g13SearchServer(t *testing.T, eng *mediaFakeEngine, opts ...Option) (*Server, http.Handler) {
	t.Helper()
	srv := newTestServer(t, eng, opts...)
	return srv, mediaAuthed(srv, srv.handleSearch)
}

func TestG13SearchCacheHitSecondCallFree(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}}
	eng.resp = &provider.MediaPassthroughResponse{
		Status: 200,
		Body:   json.RawMessage(`{"results":[{"title":"Codvian Router","url":"https://example.com"}]}`),
	}
	_, h := g13SearchServer(t, eng)
	body := `{"query":"codvian router","max_results":5}`

	rec1 := doMediaReq(t, h, http.MethodPost, "/v1/search", body, mediaTestKey)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first search: status = %d, want 200\nbody: %s", rec1.Code, rec1.Body.String())
	}
	if got := rec1.Header().Get("X-Codvian-Search-Cache"); got != "miss" {
		t.Errorf("first search X-Codvian-Search-Cache = %q, want miss", got)
	}
	rec2 := doMediaReq(t, h, http.MethodPost, "/v1/search", body, mediaTestKey)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second search: status = %d, want 200\nbody: %s", rec2.Code, rec2.Body.String())
	}
	if got := rec2.Header().Get("X-Codvian-Search-Cache"); got != "hit" {
		t.Errorf("second search X-Codvian-Search-Cache = %q, want hit", got)
	}
	if rec2.Body.String() != rec1.Body.String() {
		t.Errorf("cached body differs from the first answer:\nfirst:  %s\nsecond: %s", rec1.Body.String(), rec2.Body.String())
	}
	if got := eng.mediaCalls(); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (second identical search served from cache)", got)
	}
}

func TestG13SearchCacheDifferentQueryMisses(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}}
	eng.resp = &provider.MediaPassthroughResponse{Status: 200, Body: json.RawMessage(`{"results":[]}`)}
	_, h := g13SearchServer(t, eng)

	for _, q := range []string{`{"query":"alpha"}`, `{"query":"beta"}`, `{"query":"alpha","max_results":3}`} {
		rec := doMediaReq(t, h, http.MethodPost, "/v1/search", q, mediaTestKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("search %s: status = %d, want 200", q, rec.Code)
		}
		if got := rec.Header().Get("X-Codvian-Search-Cache"); got != "miss" {
			t.Errorf("search %s: X-Codvian-Search-Cache = %q, want miss", q, got)
		}
	}
	if got := eng.mediaCalls(); got != 3 {
		t.Errorf("upstream calls = %d, want 3 (different queries / params never share an entry)", got)
	}
}

func TestG13SearchCacheTTLBypass(t *testing.T) {
	// WithSearchCacheTTL(0): ttl <= 0 bypasses the cache AND the
	// coalescing, so two identical searches each run their own
	// upstream call.
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}}
	eng.resp = &provider.MediaPassthroughResponse{Status: 200, Body: json.RawMessage(`{"results":[]}`)}
	_, h := g13SearchServer(t, eng, WithSearchCacheTTL(0))
	body := `{"query":"codvian router"}`

	for i := 0; i < 2; i++ {
		rec := doMediaReq(t, h, http.MethodPost, "/v1/search", body, mediaTestKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("search %d: status = %d, want 200", i, rec.Code)
		}
		if got := rec.Header().Get("X-Codvian-Search-Cache"); got != "miss" {
			t.Errorf("search %d: X-Codvian-Search-Cache = %q, want miss (bypass)", i, got)
		}
	}
	if got := eng.mediaCalls(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (ttl 0 bypasses cache and coalescing)", got)
	}
}

func TestG13SearchCacheNoStoreHeaderBypasses(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}}
	eng.resp = &provider.MediaPassthroughResponse{Status: 200, Body: json.RawMessage(`{"results":[]}`)}
	_, h := g13SearchServer(t, eng)
	body := `{"query":"codvian router"}`

	// First call warms the cache (miss, 1 upstream call).
	rec1 := doMediaReq(t, h, http.MethodPost, "/v1/search", body, mediaTestKey)
	if rec1.Code != http.StatusOK || rec1.Header().Get("X-Codvian-Search-Cache") != "miss" {
		t.Fatalf("first search: status = %d cache = %q, want 200/miss", rec1.Code, rec1.Header().Get("X-Codvian-Search-Cache"))
	}
	// A no-store call must NOT be served from the cache: build
	// the request directly so the header can be set (doMediaReq
	// has no header hook).
	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mediaTestKey)
	req.Header.Set("X-Codvian-Cache-No-Store", "true")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("no-store search: status = %d, want 200", rec2.Code)
	}
	if got := rec2.Header().Get("X-Codvian-Search-Cache"); got != "miss" {
		t.Errorf("no-store search X-Codvian-Search-Cache = %q, want miss", got)
	}
	if got := eng.mediaCalls(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (no-store call ran its own fetch)", got)
	}
}

func TestG13SearchCacheErrorNotStored(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}}
	eng.err = errors.New("upstream down")
	_, h := g13SearchServer(t, eng)
	body := `{"query":"codvian router"}`

	rec1 := doMediaReq(t, h, http.MethodPost, "/v1/search", body, mediaTestKey)
	if rec1.Code == http.StatusOK {
		t.Fatalf("failing search: status = %d, want non-200", rec1.Code)
	}
	// The failure must not be cached: a healthy retry dials the
	// upstream again and succeeds.
	eng.err = nil
	eng.resp = &provider.MediaPassthroughResponse{Status: 200, Body: json.RawMessage(`{"results":[]}`)}
	rec2 := doMediaReq(t, h, http.MethodPost, "/v1/search", body, mediaTestKey)
	if rec2.Code != http.StatusOK {
		t.Fatalf("retry search: status = %d, want 200", rec2.Code)
	}
	if got := rec2.Header().Get("X-Codvian-Search-Cache"); got != "miss" {
		t.Errorf("retry search X-Codvian-Search-Cache = %q, want miss (error was not stored)", got)
	}
	if got := eng.mediaCalls(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (failed fetch + retry)", got)
	}
}
