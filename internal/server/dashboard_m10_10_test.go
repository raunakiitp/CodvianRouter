// M10-10 dashboard Cache page: hit/miss + tokens saved from the
// live GET /api/cache/stats feed, per-layer state, flush via DELETE
// /api/cache with confirmation, and the honest savings note
// (memory-only vs persisted). Cache-write tier economics are shown
// as the 1.25×–2.5× BAND the dashboard currently estimates —
// labelled an estimate, with the Anthropic 5m 1.25× / 1h 2× tiers
// noted as the backend's per-tier accounting, not as
// dashboard-measured savings. No tokens-saved figure is ever
// invented: the page shows "—" plus the feed's savings_note when
// no savings recorder is wired.
//
// Feed shapes (from cache_stats.go / cache_mgmt.go, not assumed):
// stats = {exact?, semantic?, totals?, savings?, savings_note?,
// coalescing?} where each layer is {entries, bytes, hits, misses,
// hit_ratio?}; savings = {hits, tokens_saved, cost_saved_usd};
// DELETE returns {evicted{exact,semantic,reasoning},
// entries_before, flushed, total_evicted, notes?}.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/coalescing"
	"github.com/raunakiitp/codvian-router/internal/metrics"
)

// cachePageSection returns the served HTML of <section id="view-cache">.
func cachePageSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="view-cache"`)
	if start < 0 {
		t.Fatalf("dashboard has no view-cache section")
	}
	rest := body[start:]
	end := strings.Index(rest, "</section>")
	if end < 0 {
		t.Fatalf("view-cache section is not closed")
	}
	return rest[:end]
}

// cachePageInlineScript returns the served dashboard's single inline
// <script> block.
func cachePageInlineScript(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<script>")
	if start < 0 {
		t.Fatalf("dashboard has no inline script")
	}
	rest := body[start+len("<script>"):]
	end := strings.Index(rest, "</script>")
	if end < 0 {
		t.Fatalf("inline script is not closed")
	}
	return rest[:end]
}

func TestDashboardM10_10CacheRenders(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	section := cachePageSection(t, body)
	script := cachePageInlineScript(t, body)

	for _, want := range []string{
		"<h1>Cache</h1>",
		"Gateway cache: exact + semantic layers",
		// KPI row: hit rate + hits + misses (live ids).
		`id="cachePageHitRate"`, `id="cachePageHitNote"`,
		`id="cachePageHits"`, `id="cachePageMisses"`,
		// Per-layer table + empty state + flush result line.
		`id="cachePageLayersBody"`, `id="cachePageMeta"`,
		`id="cachePageEmpty"`, "No cache entries yet",
		`id="cachePageFlushResult"`,
		// Flush + refresh controls.
		`id="cachePageFlushBtn"`, "Flush cache",
		`id="cachePageRefreshBtn"`, "Refresh",
		// Savings: values + the honesty note (never a number here).
		`id="cachePageSavingsTokens"`, `id="cachePageSavingsCost"`,
		`id="cachePageSavingsNote"`,
		// Tier economics: labelled estimate band + Anthropic tiers
		// as backend per-tier accounting, not measured savings.
		`id="cachePageTierNote"`,
		"1.25×–2.5×",
		"a band the dashboard estimates, not a measured saving",
		"Anthropic 5-minute writes at 1.25× and 1-hour writes at 2×",
		"per-tier accounting in the backend, not savings measured by this dashboard",
		// Single-flight coalescing line from the same feed.
		`id="cachePageCoalescing"`,
	} {
		if !strings.Contains(section, want) {
			t.Errorf("dashboard Cache page missing %q", want)
		}
	}

	// The section reads the live /api feed directly (no
	// /dashboard/cache feed exists) and flushes through the shared
	// adminHeaders() helper — no second auth path.
	for _, want := range []string{
		`fetch("/api/cache/stats")`,
		`fetch("/api/cache", {method: "DELETE"`,
		"adminHeaders()",
		"loadCachePage",
		"flushCachePage",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("dashboard Cache JS missing %q", want)
		}
	}

	// Flush is confirmed before it fires.
	if !strings.Contains(script, `window.confirm("Flush the whole gateway cache?`) {
		t.Error("dashboard Cache flush has no confirmation dialog")
	}

	// Savings values are "—" in the static HTML: no fabricated
	// tokens-saved number ships in the page.
	for _, id := range []string{`id="cachePageSavingsTokens">—<`, `id="cachePageSavingsCost">—<`} {
		if !strings.Contains(section, id) {
			t.Errorf("dashboard Cache savings %q is not the honest static —", id)
		}
	}

	// No fake-data markers, no Playground.
	for _, banned := range []string{"Lorem", "Sample data", "sample data", "Playground"} {
		if strings.Contains(section, banned) {
			t.Errorf("Cache section contains %q", banned)
		}
	}
}

// Regression test for the M10-10 independent-review FAIL: the Cache
// section was nested inside the Settings section (view-settings opened
// at web/index.html:2457, view-cache opened inside it, Cache closed,
// then Settings closed — no </section> between the two opens). With
// CSS .view{display:none} / .view.on{display:block}, navigating to
// Cache added .on to view-cache while its ancestor view-settings stayed
// display:none, so the Cache page was invisible. The slice's own
// cachePageSection helper cannot catch this: it slices to the first
// </section> after the Cache open. This test parses the section stack
// instead, so nesting fails it.
func TestDashboardM10_10CacheNotNestedInSettings(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Section-stack parse over the whole served page: track the open
	// <section class="view" id="..."> stack and record view-cache's
	// parent and the position where view-settings closes.
	var stack []string
	cacheParent := "<not found>"
	cacheDepth := -1
	settingsClose := -1
	rest := body
	offset := 0
	for len(rest) > 0 {
		openIdx := strings.Index(rest, "<section")
		closeIdx := strings.Index(rest, "</section>")
		if openIdx < 0 && closeIdx < 0 {
			break
		}
		if closeIdx >= 0 && (openIdx < 0 || closeIdx < openIdx) {
			if len(stack) > 0 {
				if stack[len(stack)-1] == "view-settings" {
					settingsClose = offset + closeIdx
				}
				stack = stack[:len(stack)-1]
			}
			rest = rest[closeIdx+len("</section>"):]
			offset += closeIdx + len("</section>")
			continue
		}
		// An open tag: extract its id="..." (if any) and push it.
		tagEnd := strings.Index(rest[openIdx:], ">")
		if tagEnd < 0 {
			t.Fatal("unclosed <section tag in dashboard HTML")
		}
		tag := rest[openIdx : openIdx+tagEnd]
		id := ""
		if i := strings.Index(tag, `id="`); i >= 0 {
			tag = tag[i+len(`id="`):]
			if j := strings.Index(tag, `"`); j >= 0 {
				id = tag[:j]
			}
		}
		stack = append(stack, id)
		if id == "view-cache" {
			cacheDepth = len(stack) - 1
			if len(stack) >= 2 {
				cacheParent = stack[len(stack)-2]
			} else {
				cacheParent = ""
			}
		}
		rest = rest[openIdx+tagEnd+1:]
		offset += openIdx + tagEnd + 1
	}
	if cacheDepth < 0 {
		t.Fatal("dashboard has no view-cache section")
	}
	if cacheDepth != 0 {
		t.Errorf("view-cache is nested at section depth %d (parent %q), want depth 0 (sibling of view-settings, direct child of the views container)", cacheDepth, cacheParent)
	}
	if cacheParent != "" {
		t.Errorf("view-cache parent section = %q, want none (must not be nested inside view-settings or any other .view)", cacheParent)
	}
	if settingsClose < 0 {
		t.Error("view-settings section is never closed")
	} else if cacheOpen := strings.Index(body, `id="view-cache"`); settingsClose >= cacheOpen {
		t.Errorf("view-settings closes at byte %d, at/after view-cache opens at byte %d: Cache is nested inside Settings", settingsClose, cacheOpen)
	}
}

// The stats the page reads equal GET /api/cache/stats: seeded live
// caches + recorder, read back through the real handler (reuses
// the M6.8 seeded stack — 2 exact entries, 1 semantic entry, 3
// exact hits / 1 exact miss, 2 semantic hits / 0 semantic misses).
func TestDashboardM10_10StatsEqualFeed(t *testing.T) {
	eng := &cacheStatsCoalescingEngine{coalescing: coalescing.Stats{Requests: 5, Flights: 2, Shared: 3, UpstreamAvoided: 3, QuotaSaved: 3}}
	_, _, get := cacheStatsStack(t, eng)
	code, out := get(t)
	if code != http.StatusOK {
		t.Fatalf("GET /api/cache/stats status = %d, want 200", code)
	}
	if out.Exact == nil || out.Exact.Entries != 2 || out.Exact.Hits != 3 || out.Exact.Misses != 1 {
		t.Errorf("exact layer = %+v, want entries 2 / hits 3 / misses 1", out.Exact)
	}
	if out.Semantic == nil || out.Semantic.Entries != 1 || out.Semantic.Hits != 2 || out.Semantic.Misses != 0 {
		t.Errorf("semantic layer = %+v, want entries 1 / hits 2 / misses 0", out.Semantic)
	}
	if out.Totals == nil || out.Totals.Entries != 3 || out.Totals.Hits != 5 || out.Totals.Misses != 1 {
		t.Errorf("totals = %+v, want entries 3 / hits 5 / misses 1", out.Totals)
	}
	if out.Totals == nil || out.Totals.HitRatio == nil || *out.Totals.HitRatio != 5.0/6.0 {
		t.Errorf("totals hit_ratio = %v, want 5/6", out.Totals)
	}
	if out.Coalescing == nil || out.Coalescing.Requests != 5 || out.Coalescing.Shared != 3 {
		t.Errorf("coalescing = %+v, want requests 5 / shared 3", out.Coalescing)
	}
	// The page renders exactly these numbers: the JS reads the
	// totals block for the KPI row and the layer blocks for the
	// table (asserted structurally — the values themselves come
	// from this same feed, never from a second source).
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	script := cachePageInlineScript(t, rec.Body.String())
	for _, want := range []string{"d.totals", "cachePageLayerRows(d)", "d.coalescing"} {
		if !strings.Contains(script, want) {
			t.Errorf("Cache JS does not read the stats feed via %q", want)
		}
	}
}

// Flush round-trip against the live server: stats before, DELETE
// without a key refused, DELETE with the admin key evicts the
// seeded entries per layer, stats after show empty layers, and a
// second DELETE evicts nothing.
func TestDashboardM10_10FlushRoundTrip(t *testing.T) {
	ctx := context.Background()
	exact := cache.NewExactCache(500)
	exact.Set(ctx, "k1", cache.CachedResponse{Content: "one", Model: "m", Usage: cache.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, time.Hour)
	exact.Set(ctx, "k2", cache.CachedResponse{Content: "two", Model: "m", Usage: cache.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}}, time.Hour)
	semantic := cache.NewLocalSemanticCache()
	semantic.SetTextForModel(ctx, "m", "hello world this is a test", cache.SemanticEntry{Response: cache.CachedResponse{Content: "sem", Model: "m", Usage: cache.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}}}, time.Hour)
	mrec := metrics.NewRecorder()
	mrec.IncCacheHit("exact")
	mrec.IncCacheHit("exact")
	mrec.IncCacheHit("exact")
	mrec.IncCacheMiss("exact")
	mrec.IncCacheHit("semantic")
	mrec.IncCacheHit("semantic")

	setup := newKeyBudgetSetup(t)
	for i := range setup.cfg.GatewayKeys {
		if setup.cfg.GatewayKeys[i].ID == setup.id {
			setup.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	srv := setup.server(t, WithCaches(exact, semantic), WithMetrics(mrec))

	getStats := func(t *testing.T) cacheStatsResponse {
		t.Helper()
		rec := doReq(t, srv, http.MethodGet, "/api/cache/stats", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/cache/stats status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var out cacheStatsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode cache stats: %v", err)
		}
		return out
	}

	before := getStats(t)
	if before.Exact == nil || before.Exact.Entries != 2 {
		t.Fatalf("stats before flush: exact = %+v, want entries 2", before.Exact)
	}
	if before.Semantic == nil || before.Semantic.Entries != 1 {
		t.Fatalf("stats before flush: semantic = %+v, want entries 1", before.Semantic)
	}

	// DELETE is admin-scoped: no key → 401.
	rec := doReq(t, srv, http.MethodDelete, "/api/cache", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("DELETE /api/cache without a key status = %d, want 401", rec.Code)
	}

	rec = doReq(t, srv, http.MethodDelete, "/api/cache", "", setup.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/cache with admin key status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var del cacheDeleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &del); err != nil {
		t.Fatalf("decode cache delete: %v", err)
	}
	if del.Evicted["exact"] != 2 || del.Evicted["semantic"] != 1 {
		t.Errorf("DELETE evicted = %v, want exact 2 / semantic 1", del.Evicted)
	}
	if del.TotalEvicted != 3 {
		t.Errorf("DELETE total_evicted = %d, want 3", del.TotalEvicted)
	}
	if !del.Flushed["exact"] || !del.Flushed["semantic"] {
		t.Errorf("DELETE flushed = %v, want exact+semantic true", del.Flushed)
	}
	if del.EntriesBefore["exact"] != 2 || del.EntriesBefore["semantic"] != 1 {
		t.Errorf("DELETE entries_before = %v, want exact 2 / semantic 1", del.EntriesBefore)
	}

	after := getStats(t)
	if after.Exact == nil || after.Exact.Entries != 0 {
		t.Errorf("stats after flush: exact = %+v, want entries 0", after.Exact)
	}
	if after.Semantic == nil || after.Semantic.Entries != 0 {
		t.Errorf("stats after flush: semantic = %+v, want entries 0", after.Semantic)
	}
	// Hit/miss counters are since server start: the flush empties
	// entries, it does not rewrite history.
	if after.Totals == nil || after.Totals.Hits != 5 || after.Totals.Misses != 1 {
		t.Errorf("stats after flush: totals = %+v, want hits 5 / misses 1 (counters survive the flush)", after.Totals)
	}

	// Second flush evicts nothing: empty layers, honest zeros.
	rec = doReq(t, srv, http.MethodDelete, "/api/cache", "", setup.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("second DELETE status = %d, want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &del); err != nil {
		t.Fatalf("decode second cache delete: %v", err)
	}
	if del.TotalEvicted != 0 {
		t.Errorf("second DELETE total_evicted = %d, want 0", del.TotalEvicted)
	}
}

// Memory-only state: with a plain engine (no savings recorder) the
// feed omits the savings block and carries savings_note — and the
// page shows that note, never a fabricated tokens-saved number.
func TestDashboardM10_10MemoryOnlySavingsNote(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/api/cache/stats", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/cache/stats status = %d, want 200", rec.Code)
	}
	var out cacheStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode cache stats: %v", err)
	}
	if out.Savings != nil {
		t.Errorf("memory-only stats carry a savings block %+v, want it omitted", out.Savings)
	}
	if out.SavingsNote == "" || !strings.Contains(out.SavingsNote, "no savings recorder") {
		t.Errorf("memory-only savings_note = %q, want it to say no savings recorder is wired", out.SavingsNote)
	}

	body := doReq(t, srv, http.MethodGet, "/dashboard", "", "").Body.String()
	script := cachePageInlineScript(t, body)
	// The JS renders savings only when the block exists, and falls
	// back to the feed's own note otherwise.
	for _, want := range []string{
		"if(d.savings){",
		"d.savings_note",
		"Savings are not measured here — no savings recorder is wired (memory-only cache).",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("Cache JS missing memory-only honesty guard %q", want)
		}
	}
	// And it never fabricates a number: no literal tokens-saved
	// figure in the Cache JS.
	if strings.Contains(script, "tokens_saved = ") || strings.Contains(script, "tokensSaved = 0") {
		t.Error("Cache JS fabricates a tokens-saved figure")
	}
}

// The Cache pure helpers run under node, against the exact code
// served in the dashboard: node --check on the whole extracted
// inline script, then the pure block (hit rate, layer rows) driven
// with fixture data.
func TestDashboardM10_10PureHelpersUnderNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not on PATH — cannot run the inline-script check (install node or set NODE_PATH)")
	}
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	script := cachePageInlineScript(t, rec.Body.String())
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "inline.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("node", "--check", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("node --check on the extracted inline script failed: %v: %s", err, out)
	}
	start := strings.Index(script, "/* M10-10-PURE-START */")
	end := strings.Index(script, "/* M10-10-PURE-END */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("pure helper block markers (M10-10-PURE-START/END) not found in the inline script")
	}
	pure := script[start:end]
	driver := pure + `
var out = {
  rateSome: cachePageHitRate(3, 1),
  rateNone: cachePageHitRate(0, 0),
  rateAllMiss: cachePageHitRate(0, 4),
  rowsFull: cachePageLayerRows({exact: {entries: 2, bytes: 100, hits: 3, misses: 1, hit_ratio: 0.75}, semantic: {entries: 1, bytes: 50, hits: 2, misses: 0, hit_ratio: 1}, totals: {entries: 3, bytes: 150, hits: 5, misses: 1, hit_ratio: 5/6}}),
  rowsNoRatio: cachePageLayerRows({exact: {entries: 0, bytes: 0, hits: 0, misses: 0}}),
  rowsEmpty: cachePageLayerRows({})
};
console.log(JSON.stringify(out));
`
	driverPath := filepath.Join(dir, "driver.js")
	if err := os.WriteFile(driverPath, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("node", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v: %s", err, raw)
	}
	var got struct {
		RateSome    float64  `json:"rateSome"`
		RateNone    *float64 `json:"rateNone"`
		RateAllMiss float64  `json:"rateAllMiss"`
		RowsFull    []struct {
			Key      string   `json:"key"`
			Label    string   `json:"label"`
			Entries  int      `json:"entries"`
			Bytes    int      `json:"bytes"`
			Hits     int      `json:"hits"`
			Misses   int      `json:"misses"`
			HitRatio *float64 `json:"hitRatio"`
		} `json:"rowsFull"`
		RowsNoRatio []struct {
			HitRatio *float64 `json:"hitRatio"`
		} `json:"rowsNoRatio"`
		RowsEmpty []struct{} `json:"rowsEmpty"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode node driver output: %v (raw %s)", err, raw)
	}
	if got.RateSome != 0.75 {
		t.Errorf("cachePageHitRate(3,1) = %v, want 0.75", got.RateSome)
	}
	if got.RateNone != nil {
		t.Errorf("cachePageHitRate(0,0) = %v, want null (no lookups is not 0%%)", *got.RateNone)
	}
	if got.RateAllMiss != 0 {
		t.Errorf("cachePageHitRate(0,4) = %v, want 0", got.RateAllMiss)
	}
	if len(got.RowsFull) != 3 {
		t.Fatalf("cachePageLayerRows(full) = %d rows, want 3 (exact, semantic, totals)", len(got.RowsFull))
	}
	if got.RowsFull[0].Key != "exact" || got.RowsFull[0].Entries != 2 || got.RowsFull[0].Hits != 3 || got.RowsFull[0].HitRatio == nil || *got.RowsFull[0].HitRatio != 0.75 {
		t.Errorf("exact row = %+v, want entries 2 / hits 3 / hitRatio 0.75", got.RowsFull[0])
	}
	if got.RowsFull[1].Key != "semantic" || got.RowsFull[1].Entries != 1 {
		t.Errorf("semantic row = %+v, want entries 1", got.RowsFull[1])
	}
	if got.RowsFull[2].Key != "totals" || got.RowsFull[2].Entries != 3 || got.RowsFull[2].HitRatio == nil || *got.RowsFull[2].HitRatio != 5.0/6.0 {
		t.Errorf("totals row = %+v, want entries 3 / hitRatio 5/6", got.RowsFull[2])
	}
	if len(got.RowsNoRatio) != 1 || got.RowsNoRatio[0].HitRatio != nil {
		t.Errorf("layer with no lookups rows = %+v, want 1 row with null hitRatio", got.RowsNoRatio)
	}
	if len(got.RowsEmpty) != 0 {
		t.Errorf("cachePageLayerRows({}) = %d rows, want 0 (absent layers are absent, never zero-filled)", len(got.RowsEmpty))
	}
}
