package cache

// Acceptance tests for M1.4 (tenant-scoped cache key + temperature
// gating + truncated-response guard) and M1.5 (persistence, flush,
// savings). Written FIRST per the master prompt: they must FAIL
// before the implementation lands.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func m1Msgs() []Message {
	return []Message{{Role: "user", Content: "hello"}}
}

func m1BaseInput() KeyInput {
	return KeyInput{
		Tenant:      "key-aaa",
		Model:       "gpt-4o",
		Messages:    m1Msgs(),
		Temperature: 0,
	}
}

// M1.4: two gateway keys, same prompt -> different keys, no cross-hit.
func TestM14ScopedKeyTenantIsolation(t *testing.T) {
	a := m1BaseInput()
	b := m1BaseInput()
	b.Tenant = "key-bbb"
	ka, err := ScopedKey(a)
	if err != nil {
		t.Fatalf("ScopedKey a: %v", err)
	}
	kb, err := ScopedKey(b)
	if err != nil {
		t.Fatalf("ScopedKey b: %v", err)
	}
	if ka == kb {
		t.Fatalf("same prompt under two gateway keys produced the same cache key %q", ka)
	}
	if len(ka) != 64 || len(kb) != 64 {
		t.Fatalf("key lengths = %d/%d, want 64 hex chars (SHA-256)", len(ka), len(kb))
	}
	// And through the cache itself: store under key A, lookup under key B misses.
	c := NewExactCache(10)
	c.Set(context.Background(), ka, CachedResponse{Content: "secret-a", Model: "gpt-4o"}, time.Hour)
	if _, found := c.Get(context.Background(), kb); found {
		t.Fatalf("cross-tenant hit: key-bbb read key-aaa's cached answer")
	}
	if got, found := c.Get(context.Background(), ka); !found || got.Content != "secret-a" {
		t.Fatalf("same-tenant lookup missed: found=%v got=%q", found, got.Content)
	}
}

// M1.4: temp 1.0 vs 0 -> separate entries; temp 0.7 vs 0.71 separate buckets.
func TestM14ScopedKeyTemperatureBucket(t *testing.T) {
	base := m1BaseInput()
	k0, _ := ScopedKey(base)
	hot := base
	hot.Temperature = 1.0
	k1, _ := ScopedKey(hot)
	if k0 == k1 {
		t.Fatalf("temperature 0 and 1.0 produced the same cache key")
	}
	warmA := base
	warmA.Temperature = 0.7
	warmB := base
	warmB.Temperature = 0.71
	ka, _ := ScopedKey(warmA)
	kb, _ := ScopedKey(warmB)
	if ka == kb {
		t.Fatalf("temperatures 0.70 and 0.71 produced the same cache key")
	}
	if got := TemperatureBucket(0.704); got != "0.70" {
		t.Fatalf("TemperatureBucket(0.704) = %q, want %q", got, "0.70")
	}
}

// M1.4: tools / response_format / top_p / max_tokens are part of the key.
func TestM14ScopedKeyIncludesGenParams(t *testing.T) {
	base := m1BaseInput()
	k0, _ := ScopedKey(base)
	withTools := base
	withTools.Tools = []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}}
	kt, _ := ScopedKey(withTools)
	if kt == k0 {
		t.Fatalf("request with tools produced the same key as the tool-less request")
	}
	withFormat := base
	withFormat.ResponseFormat = map[string]any{"type": "json_object"}
	kf, _ := ScopedKey(withFormat)
	if kf == k0 {
		t.Fatalf("request with response_format produced the same key as without")
	}
	withTopP := base
	withTopP.TopP = 0.9
	kp, _ := ScopedKey(withTopP)
	if kp == k0 {
		t.Fatalf("request with top_p=0.9 produced the same key as top_p unset")
	}
	withMax := base
	withMax.MaxTokens = 100
	km, _ := ScopedKey(withMax)
	if km == k0 {
		t.Fatalf("request with max_tokens=100 produced the same key as unset")
	}
	// Determinism: same input twice -> same key.
	k0b, _ := ScopedKey(m1BaseInput())
	if k0b != k0 {
		t.Fatalf("ScopedKey not deterministic: %q vs %q", k0, k0b)
	}
}

// M1.4: never cache temperature > 0 unless explicitly allowed per-key.
func TestM14TemperatureGating(t *testing.T) {
	if !CacheableTemperature(0, false) {
		t.Fatalf("temperature 0 must be cacheable without any allowance")
	}
	if CacheableTemperature(1.0, false) {
		t.Fatalf("temperature 1.0 cached without explicit per-key allowance")
	}
	if CacheableTemperature(0.5, false) {
		t.Fatalf("temperature 0.5 cached without explicit per-key allowance")
	}
	if !CacheableTemperature(1.0, true) {
		t.Fatalf("temperature 1.0 must be cacheable when explicitly allowed per-key")
	}
	if !CacheableTemperature(0, true) {
		t.Fatalf("temperature 0 must stay cacheable when allowance is on")
	}
}

// M1.4: never store a truncated (finish_reason=length) answer; never
// store tool-carrying answers; temp gate applies at store time too.
func TestM14ShouldStoreRejectsTruncated(t *testing.T) {
	if ShouldStore("length", 0, false, false) {
		t.Fatalf("finish_reason=length response must never be stored")
	}
	if !ShouldStore("stop", 0, false, false) {
		t.Fatalf("finish_reason=stop at temp 0 must be storable")
	}
	if !ShouldStore("", 0, false, false) {
		t.Fatalf("empty finish_reason at temp 0 must be storable")
	}
	if ShouldStore("stop", 1.0, false, false) {
		t.Fatalf("temp 1.0 response must not be stored without per-key allowance")
	}
	if !ShouldStore("stop", 1.0, true, false) {
		t.Fatalf("temp 1.0 response must be storable with per-key allowance")
	}
	if ShouldStore("stop", 0, false, true) {
		t.Fatalf("tool-carrying response must never be stored")
	}
}

// M1.4: X-Codvian-Cache correctness — hit names the layer, miss is MISS.
func TestM14CacheHeaderValues(t *testing.T) {
	if got := CacheHeader("exact", true); got != "exact" {
		t.Fatalf("CacheHeader(exact, hit) = %q, want %q", got, "exact")
	}
	if got := CacheHeader("semantic", true); got != "semantic" {
		t.Fatalf("CacheHeader(semantic, hit) = %q, want %q", got, "semantic")
	}
	if got := CacheHeader("exact", false); got != "MISS" {
		t.Fatalf("CacheHeader(exact, miss) = %q, want %q", got, "MISS")
	}
	if got := CacheHeader("", false); got != "MISS" {
		t.Fatalf("CacheHeader(\"\", miss) = %q, want %q", got, "MISS")
	}
}

// M1.5: restart keeps entries when persistence is enabled.
func TestM15PersistenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	ctx := context.Background()

	p1, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	key, err := ScopedKey(m1BaseInput())
	if err != nil {
		t.Fatalf("ScopedKey: %v", err)
	}
	p1.SetForTenant(ctx, key, "key-aaa", CachedResponse{Content: "persisted answer", Model: "gpt-4o", Usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, time.Hour)
	if err := p1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a restart: reopen the same file.
	p2, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("reopen OpenPersistentCache: %v", err)
	}
	defer p2.Close()
	got, found := p2.Get(ctx, key)
	if !found {
		t.Fatalf("entry lost across restart with persistence enabled")
	}
	if got.Content != "persisted answer" {
		t.Fatalf("reloaded content = %q, want %q", got.Content, "persisted answer")
	}
	// Expired entries must NOT survive: store one with a 1ms TTL, wait, reopen.
	p2.SetForTenant(ctx, "short-key", "key-aaa", CachedResponse{Content: "short-lived"}, time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if err := p2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p3, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	defer p3.Close()
	if _, found := p3.Get(ctx, "short-key"); found {
		t.Fatalf("expired entry survived restart")
	}
	if _, found := p3.Get(ctx, key); !found {
		t.Fatalf("live entry lost on second restart")
	}
}

// M1.5: flush works — all, and per-key only.
func TestM15FlushAllAndPerKey(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenPersistentCache(filepath.Join(dir, "cache.db"), 100)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	defer p.Close()
	ctx := context.Background()
	p.SetForTenant(ctx, "k-a1", "key-aaa", CachedResponse{Content: "a1"}, time.Hour)
	p.SetForTenant(ctx, "k-a2", "key-aaa", CachedResponse{Content: "a2"}, time.Hour)
	p.SetForTenant(ctx, "k-b1", "key-bbb", CachedResponse{Content: "b1"}, time.Hour)

	if n := p.FlushTenant(ctx, "key-aaa"); n != 2 {
		t.Fatalf("FlushTenant(key-aaa) = %d, want 2", n)
	}
	if _, found := p.Get(ctx, "k-a1"); found {
		t.Fatalf("key-aaa entry survived per-key flush")
	}
	if _, found := p.Get(ctx, "k-b1"); !found {
		t.Fatalf("key-bbb entry wrongly removed by key-aaa flush")
	}
	if n := p.FlushAll(ctx); n != 1 {
		t.Fatalf("FlushAll = %d, want 1", n)
	}
	if _, found := p.Get(ctx, "k-b1"); found {
		t.Fatalf("entry survived FlushAll")
	}
	if p.Size() != 0 {
		t.Fatalf("Size after FlushAll = %d, want 0", p.Size())
	}
}

// M1.5: POST /admin/cache/flush handler (route wiring is the
// coordinator's job; the handler itself lives here).
func TestM15FlushHandler(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenPersistentCache(filepath.Join(dir, "cache.db"), 100)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	defer p.Close()
	ctx := context.Background()
	p.SetForTenant(ctx, "k-a1", "key-aaa", CachedResponse{Content: "a1"}, time.Hour)
	p.SetForTenant(ctx, "k-b1", "key-bbb", CachedResponse{Content: "b1"}, time.Hour)

	h := NewFlushHandler(p)

	// Per-key flush via JSON body.
	req := httptest.NewRequest(http.MethodPost, "/admin/cache/flush", strings.NewReader(`{"key_id":"key-aaa"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("per-key flush status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"flushed":1`) {
		t.Fatalf("per-key flush body = %q, want flushed:1", rec.Body.String())
	}
	if _, found := p.Get(ctx, "k-b1"); !found {
		t.Fatalf("other tenant's entry removed by per-key flush")
	}

	// Flush-all with an empty body.
	req = httptest.NewRequest(http.MethodPost, "/admin/cache/flush", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("flush-all status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"flushed":1`) {
		t.Fatalf("flush-all body = %q, want flushed:1", rec.Body.String())
	}

	// Wrong method rejected.
	req = httptest.NewRequest(http.MethodGet, "/admin/cache/flush", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET flush status = %d, want 405", rec.Code)
	}
}

// M1.5: savings row written on an own-cache hit (tokens + $ saved).
func TestM15SavingsRecordedOnHit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	p, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	ctx := context.Background()
	key, _ := ScopedKey(m1BaseInput())
	p.SetForTenant(ctx, key, "key-aaa", CachedResponse{
		Content: "answer", Model: "gpt-4o",
		Usage: Usage{PromptTokens: 1000, CompletionTokens: 50, TotalTokens: 1050},
	}, time.Hour)
	// Simulate a hit: the caller looked the entry up and now records
	// the saving at known per-token prices ($3/1M in, $15/1M out).
	hit, found := p.Get(ctx, key)
	if !found {
		t.Fatalf("expected hit before recording savings")
	}
	rec, err := p.RecordHit(ctx, "key-aaa", key, hit, 0.000003, 0.000015)
	if err != nil {
		t.Fatalf("RecordHit: %v", err)
	}
	if rec.TokensSaved != 1050 {
		t.Fatalf("TokensSaved = %d, want 1050", rec.TokensSaved)
	}
	// 1000*3e-6 + 50*15e-6 = 0.003 + 0.00075 = 0.00375
	if rec.CostSavedUSD < 0.00374 || rec.CostSavedUSD > 0.00376 {
		t.Fatalf("CostSavedUSD = %v, want 0.00375", rec.CostSavedUSD)
	}
	tot := p.SavingsTotals()
	if tot.Hits != 1 || tot.TokensSaved != 1050 {
		t.Fatalf("totals = %+v, want 1 hit / 1050 tokens", tot)
	}
	if tot.CostSavedUSD < 0.00374 || tot.CostSavedUSD > 0.00376 {
		t.Fatalf("total CostSavedUSD = %v, want 0.00375", tot.CostSavedUSD)
	}
	if n := p.SavingsRowCount(); n != 1 {
		t.Fatalf("SavingsRowCount = %d, want 1 row written on hit", n)
	}
	// Savings survive a restart too.
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p2, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer p2.Close()
	tot2 := p2.SavingsTotals()
	if tot2.Hits != 1 || tot2.TokensSaved != 1050 {
		t.Fatalf("savings totals after restart = %+v, want 1 hit / 1050 tokens", tot2)
	}
}
