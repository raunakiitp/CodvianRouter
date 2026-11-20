// B2-14 pinning tests (T-B2-524-TEST): live intelligence — Arena ELO
// sync (HF primary since 2026-11-20, wulong mirror fallback), models.dev
// tier derivation, TaskFit priority, scoring-hook consumption contract.
//
// LIVE PROBE 2026-11-20 (VM, curl — recorded here because /tmp probe
// bodies do not survive restarts):
//   - wulong https://api.wulong.dev/arena-ai-leaderboards/v1/leaderboard?name=text
//     → HTTP 404 {"error": "Leaderboard not found: text"}; same for
//     ?name=code, ?name=overall, ?name=coding; no-query → 400 "Missing
//     required parameter: name". The mirror is dead as configured.
//   - HF datasets-server filter (lmarena-ai/leaderboard-dataset,
//     config=text, split=latest, where category='overall' / 'coding')
//     → HTTP 200, 426 overall / 421 coding rows; top rows Gemini 4
//     Carbon (High) 1539.3 / Claude Opus 5.5 (High) 1543.5 — the exact
//     shape fetchHFCategory parses.
//   - models.dev https://models.dev/api.json → HTTP 200, 6,782,977 bytes.
//   - GitHub oolong-tea-2026/arena-ai-leaderboards EXISTS (public) with
//     daily snapshots data/<date>/{text,code}.json, latest 2026-11-19 —
//     NOT used as primary: it needs latest-date discovery + shape
//     verification, while HF is already the same LMArena dataset in the
//     exact shape the parser consumes.
package modelsync

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// b2_524_wulongBoard is a wulong-shaped leaderboard doc for one category.
const b2_524_wulongTextBoard = `{"meta": {"leaderboard": "text", "model_count": 2, "dataset_date": "2026-11-19"}, "models": [
	{"rank": 1, "model": "openai/gpt-5", "vendor": "openai", "score": 1500, "ci": 10, "votes": 6000, "license": ""},
	{"rank": 2, "model": "anthropic/claude-opus-5", "vendor": "anthropic", "score": 1400, "ci": 12, "votes": 3000, "license": ""}
]}`

const b2_524_wulongCodeBoard = `{"meta": {"leaderboard": "code", "model_count": 2, "dataset_date": "2026-11-19"}, "models": [
	{"rank": 1, "model": "openai/gpt-5", "vendor": "openai", "score": 1600, "ci": 9, "votes": 5000, "license": ""},
	{"rank": 2, "model": "deepseek/deepseek-v4", "vendor": "deepseek", "score": 1300, "ci": 15, "votes": 1200, "license": ""}
]}`

// b2_524_hfPage renders one HF datasets-server filter page for a category.
func b2_524_hfPage(category string) string {
	if category == "coding" {
		return `{"num_rows_total": 2, "rows": [
			{"row": {"model_name": "Claude Opus 5.5 (High)", "organization": "anthropic", "license": "", "rating": 1560.0, "rating_lower": 1550.0, "rating_upper": 1570.0, "vote_count": 3995, "rank": 1, "leaderboard_publish_date": "2026-11-19"}},
			{"row": {"model_name": "DeepSeek V4", "organization": "deepseek", "license": "", "rating": 1400.0, "rating_lower": 1390.0, "rating_upper": 1410.0, "vote_count": 1061, "rank": 2, "leaderboard_publish_date": "2026-11-19"}}
		]}`
	}
	return `{"num_rows_total": 2, "rows": [
		{"row": {"model_name": "Gemini 4 Carbon (High)", "organization": "google", "license": "", "rating": 1539.3, "rating_lower": 1530.0, "rating_upper": 1548.0, "vote_count": 4262, "rank": 1, "leaderboard_publish_date": "2026-11-19"}},
		{"row": {"model_name": "Claude Opus 5.5 (High)", "organization": "anthropic", "license": "", "rating": 1500.0, "rating_lower": 1490.0, "rating_upper": 1510.0, "vote_count": 3000, "rank": 2, "leaderboard_publish_date": "2026-11-19"}}
	]}`
}

// b2_524_standins returns a wulong stand-in (404s, like the real dead
// mirror, unless serve is true) and an HF stand-in (serves both
// categories unless dead is true).
func b2_524_standins(t *testing.T, wulongServe *bool, hfDead *bool) (*httptest.Server, *httptest.Server) {
	t.Helper()
	wulong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !*wulongServe {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": "Leaderboard not found: ` + r.URL.Query().Get("name") + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("name") {
		case "text":
			_, _ = w.Write([]byte(b2_524_wulongTextBoard))
		case "code":
			_, _ = w.Write([]byte(b2_524_wulongCodeBoard))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": "Leaderboard not found"}`))
		}
	}))
	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *hfDead {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error": "upstream down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.RawQuery, "coding") {
			_, _ = w.Write([]byte(b2_524_hfPage("coding")))
		} else {
			_, _ = w.Write([]byte(b2_524_hfPage("overall")))
		}
	}))
	t.Cleanup(wulong.Close)
	t.Cleanup(hf.Close)
	return wulong, hf
}

func b2_524_syncer(t *testing.T, wulongURL string, intel *Intelligence) *ArenaSyncer {
	t.Helper()
	return NewArenaSyncer(ArenaSyncerConfig{
		Enabled:  true,
		URL:      wulongURL + "/leaderboard",
		Interval: time.Hour,
	}, intel)
}

func b2_524_ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestB2_524_HFFirst_WulongDead pins the HF-first order: with the
// wulong mirror 404ing (as the live probe proved), the sync succeeds
// from HF and the store holds arena_elo rows. It also proves the
// fallback order (both live → huggingface) and that a fully dead
// sync errors while keeping the last good data.
func TestB2_524_HFFirst_WulongDead(t *testing.T) {
	wulongServe := false
	hfDead := false
	wulong, hf := b2_524_standins(t, &wulongServe, &hfDead)
	oldHFURL := hfFilterURL
	hfFilterURL = hf.URL + "/filter"
	defer func() { hfFilterURL = oldHFURL }()

	intel := NewIntelligence(filepath.Join(t.TempDir(), IntelligenceStateFileName))

	// (a) Both live → HF wins (primary).
	wulongServe = true
	res, err := b2_524_syncer(t, wulong.URL, intel).Sync(b2_524_ctx(t), nil)
	if err != nil {
		t.Fatalf("sync with both sources live: %v", err)
	}
	if res.Source != "huggingface" {
		t.Fatalf("source with both live = %q, want huggingface (HF is primary)", res.Source)
	}
	if score, src, ok := intel.TaskFit("gemini-4-carbon-high", CategoryDefault); !ok || src != SourceArenaELO || score != 0.98 {
		t.Fatalf("TaskFit gemini-4-carbon-high/default = (%v, %q, %v), want (0.98, arena_elo, true)", score, src, ok)
	}

	// (b) Wulong dead (the live-probe state) → still huggingface.
	wulongServe = false
	intel2 := NewIntelligence(filepath.Join(t.TempDir(), IntelligenceStateFileName))
	res, err = b2_524_syncer(t, wulong.URL, intel2).Sync(b2_524_ctx(t), nil)
	if err != nil {
		t.Fatalf("sync with wulong dead: %v", err)
	}
	if res.Source != "huggingface" || res.Entries == 0 {
		t.Fatalf("sync with wulong dead = source %q entries %d, want huggingface + entries", res.Source, res.Entries)
	}
	if score, _, ok := intel2.TaskFit("claude-opus-5.5-high", CategoryCoding); !ok || score != 0.98 {
		t.Fatalf("TaskFit claude-opus-5.5-high/coding = (%v, %v), want (0.98, true)", score, ok)
	}
	if _, _, ok := intel2.TaskFit("deepseek-v4", CategoryDefault); ok {
		t.Fatalf("TaskFit deepseek-v4/default ok, want false (code board does not map to default)")
	}

	// (c) Both dead → error, and the last good data stays.
	hfDead = true
	if _, err := b2_524_syncer(t, wulong.URL, intel2).Sync(b2_524_ctx(t), nil); err == nil {
		t.Fatalf("sync with both sources dead: want error, got nil")
	}
	if score, _, ok := intel2.TaskFit("claude-opus-5.5-high", CategoryCoding); !ok || score != 0.98 {
		t.Fatalf("after failed sync TaskFit = (%v, %v), want (0.98, true) — last good data must stay", score, ok)
	}
}

// TestB2_524_WulongFallback_WhenHFDead pins the fallback: with HF
// down, the wulong mirror serves the sync (source "wulong").
func TestB2_524_WulongFallback_WhenHFDead(t *testing.T) {
	wulongServe := true
	hfDead := true
	wulong, hf := b2_524_standins(t, &wulongServe, &hfDead)
	oldHFURL := hfFilterURL
	hfFilterURL = hf.URL + "/filter"
	defer func() { hfFilterURL = oldHFURL }()

	intel := NewIntelligence(filepath.Join(t.TempDir(), IntelligenceStateFileName))
	res, err := b2_524_syncer(t, wulong.URL, intel).Sync(b2_524_ctx(t), nil)
	if err != nil {
		t.Fatalf("sync with HF dead: %v", err)
	}
	if res.Source != "wulong" {
		t.Fatalf("source with HF dead = %q, want wulong", res.Source)
	}
	if score, src, ok := intel.TaskFit("gpt-5", CategoryDefault); !ok || src != SourceArenaELO || score != 0.98 {
		t.Fatalf("TaskFit gpt-5/default = (%v, %q, %v), want (0.98, arena_elo, true)", score, src, ok)
	}
	if score, _, ok := intel.TaskFit("claude-opus-5", CategoryDefault); !ok || score != 0.4 {
		t.Fatalf("TaskFit claude-opus-5/default = (%v, %v), want (0.4, true) — lowest ELO normalises to 0.4", score, ok)
	}
	if score, _, ok := intel.TaskFit("deepseek-v4", CategoryCoding); !ok || score != 0.4 {
		t.Fatalf("TaskFit deepseek-v4/coding = (%v, %v), want (0.4, true)", score, ok)
	}
}

// TestB2_524_SyncOffTouchesNoNetwork pins OFF-by-default: a disabled
// syncer makes no network call (a client that fails the test on any
// RoundTrip) and stores nothing.
func TestB2_524_SyncOffTouchesNoNetwork(t *testing.T) {
	intel := NewIntelligence(filepath.Join(t.TempDir(), IntelligenceStateFileName))
	syncer := NewArenaSyncer(ArenaSyncerConfig{
		Enabled: false,
		URL:     "http://127.0.0.1:1/leaderboard",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("disabled syncer made a network call")
			return nil, nil
		})},
	}, intel)
	res, err := syncer.Sync(b2_524_ctx(t), nil)
	if err != nil {
		t.Fatalf("disabled sync: %v", err)
	}
	if res.Entries != 0 {
		t.Fatalf("disabled sync entries = %d, want 0", res.Entries)
	}
	if syncer.Status().Enabled {
		t.Fatalf("disabled syncer reports Enabled, want false")
	}
	if _, _, ok := intel.TaskFit("gpt-5", CategoryDefault); ok {
		t.Fatalf("TaskFit after disabled sync ok, want false (no signal without a sync)")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestB2_524_TaskFitPriority pins the lookup order user_override >
// arena_elo > models_dev_tier, the variant hop, expiry filtering and
// invalid-row rejection.
func TestB2_524_TaskFitPriority(t *testing.T) {
	now := time.Now().UTC()
	intel := NewIntelligence("")
	intel.ApplySource(SourceArenaELO, []IntelligenceEntry{
		{Model: "gpt-5", Category: CategoryDefault, Score: 0.98, Confidence: ConfidenceHigh, SyncedAt: now},
	})
	intel.ApplySource(SourceModelsDevTier, []IntelligenceEntry{
		{Model: "gpt-5", Category: CategoryDefault, Score: 0.55, Confidence: ConfidenceMedium, SyncedAt: now},
	})

	if score, src, ok := intel.TaskFit("gpt-5", CategoryDefault); !ok || src != SourceArenaELO || score != 0.98 {
		t.Fatalf("arena vs tier = (%v, %q, %v), want (0.98, arena_elo, true)", score, src, ok)
	}
	// Variant hop: "-free" strips to the base row.
	if score, _, ok := intel.TaskFit("gpt-5-free", CategoryDefault); !ok || score != 0.98 {
		t.Fatalf("variant hop gpt-5-free = (%v, %v), want (0.98, true)", score, ok)
	}
	// Normalisation: "GPT-5" is the same row.
	if score, _, ok := intel.TaskFit("GPT-5", CategoryDefault); !ok || score != 0.98 {
		t.Fatalf("TaskFit GPT-5 = (%v, %v), want (0.98, true)", score, ok)
	}
	// Override wins over arena.
	intel.SetOverride("gpt-5", CategoryDefault, 0.70)
	if score, src, ok := intel.TaskFit("gpt-5", CategoryDefault); !ok || src != SourceUserOverride || score != 0.70 {
		t.Fatalf("override = (%v, %q, %v), want (0.70, user_override, true)", score, src, ok)
	}
	// Delete restores the arena row.
	if !intel.DeleteOverride("gpt-5", CategoryDefault) {
		t.Fatalf("DeleteOverride reported no row")
	}
	if score, src, ok := intel.TaskFit("gpt-5", CategoryDefault); !ok || src != SourceArenaELO || score != 0.98 {
		t.Fatalf("after delete = (%v, %q, %v), want (0.98, arena_elo, true)", score, src, ok)
	}
	// Clearing the arena source falls through to the tier row.
	if n := intel.ClearSource(SourceArenaELO); n == 0 {
		t.Fatalf("ClearSource(arena_elo) removed 0 rows")
	}
	if score, src, ok := intel.TaskFit("gpt-5", CategoryDefault); !ok || src != SourceModelsDevTier || score != 0.55 {
		t.Fatalf("after clear = (%v, %q, %v), want (0.55, models_dev_tier, true)", score, src, ok)
	}
	// Unknown model: no signal, never an invented score.
	if _, _, ok := intel.TaskFit("no-such-model-xyz", CategoryDefault); ok {
		t.Fatalf("unknown model TaskFit ok, want false")
	}
	// Expired arena row is skipped, not served.
	expired := now.Add(-time.Hour)
	intel.ApplySource(SourceArenaELO, []IntelligenceEntry{
		{Model: "gpt-5", Category: CategoryDefault, Score: 0.98, SyncedAt: now, ExpiresAt: &expired},
	})
	if score, src, ok := intel.TaskFit("gpt-5", CategoryDefault); !ok || src != SourceModelsDevTier || score != 0.55 {
		t.Fatalf("expired arena row = (%v, %q, %v), want fall-through to (0.55, models_dev_tier, true)", score, src, ok)
	}
	// Invalid rows are skipped, never stored.
	res := intel.ApplySource(SourceArenaELO, []IntelligenceEntry{
		{Model: "", Category: CategoryDefault, Score: 0.9, SyncedAt: now},
		{Model: "bad-score", Category: CategoryDefault, Score: 1.5, SyncedAt: now},
		{Model: "bad-score", Category: CategoryDefault, Score: math.NaN(), SyncedAt: now},
	})
	if res.SkippedInvalid != 3 || res.Upserted != 0 {
		t.Fatalf("invalid apply = %+v, want SkippedInvalid 3 Upserted 0", res)
	}
}

// TestB2_524_TierDerivation pins DeriveTier, BuildTierEntries and
// RebuildTier (stale rows prune with the rebuild).
func TestB2_524_TierDerivation(t *testing.T) {
	yes := true
	if got := DeriveTier(TierCapability{Reasoning: true}); got != TierPremium {
		t.Fatalf("reasoning tier = %q, want premium", got)
	}
	if got := DeriveTier(TierCapability{ToolCall: true, ContextWindow: 200000}); got != TierStandard {
		t.Fatalf("toolcall+200k tier = %q, want standard", got)
	}
	if got := DeriveTier(TierCapability{ToolCall: true, ContextWindow: 128000}); got != TierStandard {
		t.Fatalf("toolcall+128k tier = %q, want standard (boundary inclusive)", got)
	}
	if got := DeriveTier(TierCapability{ToolCall: true, ContextWindow: 32000}); got != TierFast {
		t.Fatalf("toolcall+32k tier = %q, want fast", got)
	}
	if got := DeriveTier(TierCapability{}); got != TierBudget {
		t.Fatalf("no-capability tier = %q, want budget", got)
	}
	cat := &Catalog{Providers: map[string]CatalogProvider{
		"openai": {ID: "openai", Models: map[string]CatalogModel{
			"gpt-5":       {ID: "gpt-5", ContextWindow: 400000, Reasoning: &yes},
			"gpt-5-mini":  {ID: "gpt-5-mini", ContextWindow: 128000, ToolCall: &yes},
			"cheap-model": {ID: "cheap-model", ContextWindow: 8000},
			"old-model":   {ID: "old-model", ContextWindow: 200000, Reasoning: &yes, Deprecated: true},
		}},
	}}
	now := time.Now().UTC()
	entries := BuildTierEntries(cat, now)
	byModel := map[string]float64{}
	for _, e := range entries {
		if e.Category == CategoryDefault {
			byModel[e.Model] = e.Score
		}
	}
	if byModel["gpt-5"] != TierTaskFit[TierPremium][CategoryDefault] {
		t.Fatalf("gpt-5 tier score = %v, want premium default %v", byModel["gpt-5"], TierTaskFit[TierPremium][CategoryDefault])
	}
	if byModel["gpt-5-mini"] != TierTaskFit[TierStandard][CategoryDefault] {
		t.Fatalf("gpt-5-mini tier score = %v, want standard default %v", byModel["gpt-5-mini"], TierTaskFit[TierStandard][CategoryDefault])
	}
	if byModel["cheap-model"] != TierTaskFit[TierBudget][CategoryDefault] {
		t.Fatalf("cheap-model tier score = %v, want budget default %v", byModel["cheap-model"], TierTaskFit[TierBudget][CategoryDefault])
	}
	if _, ok := byModel["old-model"]; ok {
		t.Fatalf("deprecated old-model got tier rows, want none")
	}

	intel := NewIntelligence("")
	res := intel.RebuildTier(cat)
	if res.Upserted == 0 {
		t.Fatalf("RebuildTier upserted 0 rows")
	}
	if score, src, ok := intel.TaskFit("cheap-model", CategoryDefault); !ok || src != SourceModelsDevTier || score != 0.55 {
		t.Fatalf("cheap-model after rebuild = (%v, %q, %v), want (0.55, models_dev_tier, true)", score, src, ok)
	}
	// Rebuild without cheap-model: its tier rows prune away.
	cat2 := &Catalog{Providers: map[string]CatalogProvider{
		"openai": {ID: "openai", Models: map[string]CatalogModel{
			"gpt-5": {ID: "gpt-5", ContextWindow: 400000, Reasoning: &yes},
		}},
	}}
	intel.RebuildTier(cat2)
	if _, _, ok := intel.TaskFit("cheap-model", CategoryDefault); ok {
		t.Fatalf("cheap-model still resolves after rebuild without it, want pruned")
	}
	if score, _, ok := intel.TaskFit("gpt-5", CategoryDefault); !ok || score != TierTaskFit[TierPremium][CategoryDefault] {
		t.Fatalf("gpt-5 after second rebuild = (%v, %v), want (0.85, true)", score, ok)
	}
}

// TestB2_524_ParseAndTransform pins the leaderboard parse (a bad row
// fails the whole parse), ELO normalisation to [0.4, 0.98], and that
// unknown boards (vision) and empty boards yield nothing.
func TestB2_524_ParseAndTransform(t *testing.T) {
	now := time.Now().UTC()
	board, err := ParseArenaLeaderboard([]byte(b2_524_wulongTextBoard))
	if err != nil {
		t.Fatalf("parse valid board: %v", err)
	}
	if len(board.Models) != 2 || board.Models[0].Model != "openai/gpt-5" {
		t.Fatalf("parsed board = %+v, want 2 models starting openai/gpt-5", board.Models)
	}
	if _, err := ParseArenaLeaderboard([]byte(`{"meta": {}, "models": [{"rank": 1, "model": "", "score": 1500}]}`)); err == nil {
		t.Fatalf("empty model name: want parse error, got nil")
	}
	if _, err := ParseArenaLeaderboard([]byte(`not json`)); err == nil {
		t.Fatalf("invalid JSON: want parse error, got nil")
	}

	text, _ := ParseArenaLeaderboard([]byte(b2_524_wulongTextBoard))
	code, _ := ParseArenaLeaderboard([]byte(b2_524_wulongCodeBoard))
	entries := TransformArenaLeaderboards(map[string]*arenaLeaderboard{"text": text, "code": code}, nil, now)
	// text board: 2 models x 4 categories; code board: 2 models x 1 category.
	if len(entries) != 10 {
		t.Fatalf("transformed entries = %d, want 10", len(entries))
	}
	byKey := map[string]float64{}
	for _, e := range entries {
		byKey[e.Model+"|"+e.Category] = e.Score
		if e.ExpiresAt == nil || !e.ExpiresAt.After(now) {
			t.Fatalf("entry %+v has no future expiry, want now+7d", e)
		}
	}
	if byKey["gpt-5|default"] != 0.98 || byKey["claude-opus-5|default"] != 0.4 {
		t.Fatalf("text normalisation: gpt-5=%v claude-opus-5=%v, want 0.98/0.4", byKey["gpt-5|default"], byKey["claude-opus-5|default"])
	}
	if byKey["gpt-5|coding"] != 0.98 || byKey["deepseek-v4|coding"] != 0.4 {
		t.Fatalf("code normalisation: gpt-5=%v deepseek-v4=%v, want 0.98/0.4", byKey["gpt-5|coding"], byKey["deepseek-v4|coding"])
	}

	// Single-model board: zero ELO range normalises to 0.4.
	solo, _ := ParseArenaLeaderboard([]byte(`{"meta": {}, "models": [{"rank": 1, "model": "solo-model", "score": 1450, "votes": 100}]}`))
	if got := TransformArenaLeaderboards(map[string]*arenaLeaderboard{"text": solo}, nil, now); len(got) != 4 || got[0].Score != 0.4 {
		t.Fatalf("single-model transform = %d entries first score %v, want 4 entries at 0.4", len(got), func() float64 {
			if len(got) > 0 {
				return got[0].Score
			}
			return -1
		}())
	}
	// Vision-only and empty boards yield nothing.
	if got := TransformArenaLeaderboards(map[string]*arenaLeaderboard{"vision": solo}, nil, now); len(got) != 0 {
		t.Fatalf("vision-only transform = %d entries, want 0", len(got))
	}
	empty, _ := ParseArenaLeaderboard([]byte(`{"meta": {}, "models": []}`))
	if got := TransformArenaLeaderboards(map[string]*arenaLeaderboard{"text": empty}, nil, now); len(got) != 0 {
		t.Fatalf("empty-board transform = %d entries, want 0", len(got))
	}
}
