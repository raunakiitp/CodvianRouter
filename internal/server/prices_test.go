package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/modelsync"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func pricePtr(v float64) *float64 { return &v }

// testPriceBook is a price book with one fully priced model, one
// half-known model (input only — blended unknown), and one free
// model. The half-known model proves a one-sided price is never
// guessed from the known side.
func testPriceBook() *config.PriceBook {
	return &config.PriceBook{
		Entries: map[string]config.ModelPrice{
			"model-a": {
				InputUSDPer1M:       pricePtr(2.5),
				OutputUSDPer1M:      pricePtr(10),
				CachedInputUSDPer1M: pricePtr(0.25),
			},
			"model-half": {InputUSDPer1M: pricePtr(5)},
			"model-free": {InputUSDPer1M: pricePtr(0), OutputUSDPer1M: pricePtr(0)},
		},
		Sources: map[string]string{
			"model-a": "config", "model-half": "price-file", "model-free": "config",
		},
		HasFile:    true,
		FileModels: 1,
	}
}

func TestPricesEndpoint(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithPriceBook(testPriceBook()))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/prices", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp pricesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.HasFile || resp.FileModels != 1 {
		t.Errorf("has_price_file=%v file_models=%d (want true, 1)", resp.HasFile, resp.FileModels)
	}
	byModel := map[string]priceRow{}
	for _, row := range resp.Prices {
		byModel[row.Model] = row
	}
	if len(resp.Prices) != 3 {
		t.Fatalf("prices = %d rows (want 3)", len(resp.Prices))
	}
	a := byModel["model-a"]
	if a.InputUSDPer1M == nil || *a.InputUSDPer1M != 2.5 || a.OutputUSDPer1M == nil || *a.OutputUSDPer1M != 10 {
		t.Errorf("model-a prices = %+v (want input 2.5, output 10)", a)
	}
	if a.BlendedUSDPer1M == nil || *a.BlendedUSDPer1M != 6.25 {
		t.Errorf("model-a blended = %v (want 6.25)", a.BlendedUSDPer1M)
	}
	if a.CachedInputUSDPer1M == nil || *a.CachedInputUSDPer1M != 0.25 {
		t.Errorf("model-a cached = %v (want 0.25)", a.CachedInputUSDPer1M)
	}
	if a.Source != "config" {
		t.Errorf("model-a source = %q (want config)", a.Source)
	}
	// Half-known: input present, blended absent — never guessed.
	half := byModel["model-half"]
	if half.BlendedUSDPer1M != nil {
		t.Errorf("model-half blended = %v (want absent: only input is known)", *half.BlendedUSDPer1M)
	}
	if half.Source != "price-file" {
		t.Errorf("model-half source = %q (want price-file)", half.Source)
	}
	// Free model: blended 0 present, distinct from unknown.
	free := byModel["model-free"]
	if free.BlendedUSDPer1M == nil || *free.BlendedUSDPer1M != 0 {
		t.Errorf("model-free blended = %v (want 0, present)", free.BlendedUSDPer1M)
	}
	if rec := doReq(t, srv, http.MethodPost, "/dashboard/prices", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d (want 405)", rec.Code)
	}
}

func TestPricesEndpointEmptyBook(t *testing.T) {
	// No price book wired: honest empty state, not an error.
	srv := newTestServer(t, &fakeEngine{})
	var resp pricesResponse
	getJSON(t, srv, "/dashboard/prices", &resp)
	if len(resp.Prices) != 0 || resp.HasFile {
		t.Errorf("empty prices = %+v (want no rows, no file)", resp)
	}
}

func TestStrategiesFeedCostAndQuality(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	// Real attempts: model-a served twice OK by openai, model-b
	// once failed by anthropic. Quality is measured from these.
	ctx := context.Background()
	for _, u := range []ledger.Usage{
		{Provider: "openai", Model: "model-a", TotalTokens: 10, SplitKnown: false, OK: true, LatencyMS: 100},
		{Provider: "openai", Model: "model-a", TotalTokens: 10, SplitKnown: false, OK: true, LatencyMS: 120},
		{Provider: "anthropic", Model: "model-b", TotalTokens: 10, SplitKnown: false, OK: false, LatencyMS: 5000},
	} {
		if _, err := led.RecordUsage(ctx, u, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	reg := &fakeModelRegistry{models: []provider.DashboardModel{
		{ID: "model-a", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true},
		{ID: "model-b", ProviderID: "anthropic", ProviderName: "Anthropic", Enabled: true},
		{ID: "model-c", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true},
	}}
	srv := newTestServer(t, &strategyFakeEngine{}, WithLedger(led), WithRegistry(reg), WithPriceBook(testPriceBook()))
	feed := getStrategies(t, srv)
	byModel := map[string]chainStrategy{}
	for _, c := range feed.Chains {
		byModel[c.Model] = c
	}
	if len(feed.Chains) != 3 {
		t.Fatalf("chains = %v (want model-a, model-b, model-c)", feed.Chains)
	}
	// model-a: priced ($6.25 blended) and 2/2 OK.
	a := byModel["model-a"]
	if a.CostUSDPer1M == nil || *a.CostUSDPer1M != 6.25 {
		t.Errorf("model-a cost = %v (want 6.25)", a.CostUSDPer1M)
	}
	if a.Quality == nil || *a.Quality != 1 || a.QualitySamples != 2 {
		t.Errorf("model-a quality = %v over %d samples (want 1.0 over 2)", a.Quality, a.QualitySamples)
	}
	// model-b: unpriced (nil cost, never $0) and 0/1 OK.
	b := byModel["model-b"]
	if b.CostUSDPer1M != nil {
		t.Errorf("model-b cost = %v (want nil: price unknown, never $0)", *b.CostUSDPer1M)
	}
	if b.Quality == nil || *b.Quality != 0 || b.QualitySamples != 1 {
		t.Errorf("model-b quality = %v over %d samples (want 0.0 over 1: every attempt failed)", b.Quality, b.QualitySamples)
	}
	// model-c: unpriced and no attempts — both unknown.
	c := byModel["model-c"]
	if c.CostUSDPer1M != nil {
		t.Errorf("model-c cost = %v (want nil)", *c.CostUSDPer1M)
	}
	if c.Quality != nil || c.QualitySamples != 0 {
		t.Errorf("model-c quality = %v over %d (want nil over 0: no attempts yet)", c.Quality, c.QualitySamples)
	}
	if feed.PricedModels != 1 {
		t.Errorf("priced_models = %d (want 1: only model-a)", feed.PricedModels)
	}
	if feed.PriceSource != "config + price-file" {
		t.Errorf("price_source = %q (want config + price-file)", feed.PriceSource)
	}
	// Without a ledger the quality is unknown everywhere, but the
	// cost column still works.
	srv2 := newTestServer(t, &strategyFakeEngine{}, WithRegistry(reg), WithPriceBook(testPriceBook()))
	feed2 := getStrategies(t, srv2)
	for _, ch := range feed2.Chains {
		if ch.Quality != nil || ch.QualitySamples != 0 {
			t.Errorf("no-ledger chain %s quality = %v over %d (want nil over 0)", ch.Model, ch.Quality, ch.QualitySamples)
		}
	}
}

func TestAutoRouteEnrichedCostAndQuality(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	// qa-model served 3 times OK by openai: quality 1.0 over 3.
	for i := 0; i < 3; i++ {
		if _, err := led.RecordUsage(ctx, ledger.Usage{Provider: "openai", Model: "qa-model", TotalTokens: 5, SplitKnown: false, OK: true, LatencyMS: 90}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	reg := &fakeModelRegistry{models: []provider.DashboardModel{
		{ID: "qa-model", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true},
		{ID: "chat-model", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true},
	}}
	r := autorouter.New(autorouter.Config{
		DefaultModel: "strong-model",
		TierModels: map[autorouter.Tier][]string{
			autorouter.TierQuickQA: {"qa-model"},
			autorouter.TierChat:    {"chat-model"},
		},
	})
	book := &config.PriceBook{
		Entries: map[string]config.ModelPrice{
			"qa-model": {InputUSDPer1M: pricePtr(1), OutputUSDPer1M: pricePtr(3)},
		},
		Sources: map[string]string{"qa-model": "config"},
	}
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(r), WithLedger(led), WithRegistry(reg), WithPriceBook(book))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto%2Fcheap&prompt="+url.QueryEscape("What is two plus two"), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var enriched autoRouteEnriched
	if err := json.Unmarshal(rec.Body.Bytes(), &enriched); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if enriched.Model != "qa-model" {
		t.Fatalf("model = %q (want qa-model)", enriched.Model)
	}
	if enriched.ModelCostUSDPer1M == nil || *enriched.ModelCostUSDPer1M != 2 {
		t.Errorf("model cost = %v (want 2.0 blended)", enriched.ModelCostUSDPer1M)
	}
	if !enriched.ModelPriceKnown {
		t.Error("model_price_known = false (want true: qa-model is priced)")
	}
	if enriched.ModelQuality == nil || *enriched.ModelQuality != 1 || enriched.ModelQualitySamples != 3 {
		t.Errorf("model quality = %v over %d (want 1.0 over 3)", enriched.ModelQuality, enriched.ModelQualitySamples)
	}
	// The unpriced chat-model: cost unknown, quality unknown (no
	// attempts for it), both honest.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto%2Ffast&prompt="+url.QueryEscape("Tell me a funny joke"), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var chat autoRouteEnriched
	if err := json.Unmarshal(rec.Body.Bytes(), &chat); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if chat.Model != "chat-model" {
		t.Fatalf("chat model = %q (want chat-model)", chat.Model)
	}
	if chat.ModelCostUSDPer1M != nil || chat.ModelPriceKnown {
		t.Errorf("chat model cost = %v known=%v (want nil/false: price unknown)", chat.ModelCostUSDPer1M, chat.ModelPriceKnown)
	}
	if chat.ModelQuality != nil || chat.ModelQualitySamples != 0 {
		t.Errorf("chat model quality = %v over %d (want nil/0: no attempts yet)", chat.ModelQuality, chat.ModelQualitySamples)
	}
}

func TestModelQualityIsMeasuredNotClaimed(t *testing.T) {
	// The quality number must equal successes/attempts from the
	// ledger, not a round or claimed value: 2 ok + 1 failed = 2/3.
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	ctx := context.Background()
	for _, ok := range []bool{true, true, false} {
		if _, err := led.RecordUsage(ctx, ledger.Usage{Provider: "openai", Model: "model-a", TotalTokens: 5, SplitKnown: false, OK: ok, LatencyMS: 100}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	reg := &fakeModelRegistry{models: []provider.DashboardModel{
		{ID: "model-a", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true},
	}}
	srv := newTestServer(t, &strategyFakeEngine{}, WithLedger(led), WithRegistry(reg))
	feed := getStrategies(t, srv)
	if len(feed.Chains) != 1 {
		t.Fatalf("chains = %v (want model-a)", feed.Chains)
	}
	q := feed.Chains[0].Quality
	if q == nil || math.Abs(*q-2.0/3.0) > 1e-9 {
		t.Errorf("quality = %v (want 2/3 = 0.6667: measured, not claimed)", q)
	}
	if feed.Chains[0].QualitySamples != 3 {
		t.Errorf("quality_samples = %d (want 3)", feed.Chains[0].QualitySamples)
	}
}

func TestPricesEndpointPairPrices(t *testing.T) {
	// Same model at two providers with different pair prices, plus
	// one model-level fallback row: the endpoint must list the
	// pair rows (with provider) separately from the fallback row
	// (model_level, no provider) — never merged into one price.
	book := &config.PriceBook{
		Pairs: map[config.PriceKey]config.ModelPrice{
			{Provider: "openai", Model: "model-a"}:    {InputUSDPer1M: pricePtr(2.5), OutputUSDPer1M: pricePtr(10)},
			{Provider: "anthropic", Model: "model-a"}: {InputUSDPer1M: pricePtr(30), OutputUSDPer1M: pricePtr(60)},
		},
		PairSources: map[config.PriceKey]string{
			{Provider: "openai", Model: "model-a"}:    "config",
			{Provider: "anthropic", Model: "model-a"}: "price-file",
		},
		Entries: map[string]config.ModelPrice{
			"model-b": {InputUSDPer1M: pricePtr(1), OutputUSDPer1M: pricePtr(3)},
		},
		Sources: map[string]string{"model-b": "config"},
		HasFile: true,
	}
	srv := newTestServer(t, &fakeEngine{}, WithPriceBook(book))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/prices", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp pricesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Prices) != 3 {
		t.Fatalf("prices = %d rows (want 3: two pairs + one fallback), got %+v", len(resp.Prices), resp.Prices)
	}
	byPair := map[string]priceRow{}
	var fallback *priceRow
	for i, row := range resp.Prices {
		if row.ModelLevel {
			fallback = &resp.Prices[i]
			continue
		}
		byPair[row.Provider+"/"+row.Model] = row
	}
	cheap := byPair["openai/model-a"]
	if cheap.BlendedUSDPer1M == nil || *cheap.BlendedUSDPer1M != 6.25 || cheap.Source != "config" {
		t.Errorf("openai/model-a = %+v (want blended 6.25, source config)", cheap)
	}
	expensive := byPair["anthropic/model-a"]
	if expensive.BlendedUSDPer1M == nil || *expensive.BlendedUSDPer1M != 45 || expensive.Source != "price-file" {
		t.Errorf("anthropic/model-a = %+v (want blended 45, source price-file)", expensive)
	}
	if fallback == nil || fallback.Model != "model-b" || fallback.Provider != "" {
		t.Errorf("fallback row = %+v (want model-b, no provider, model_level)", fallback)
	} else if fallback.BlendedUSDPer1M == nil || *fallback.BlendedUSDPer1M != 2 {
		t.Errorf("fallback blended = %v (want 2)", fallback.BlendedUSDPer1M)
	}
	// The strategies feed's per-model cost is the CHEAPEST pair
	// for the model ($6.25 for model-a), never the expensive one.
	reg := &fakeModelRegistry{models: []provider.DashboardModel{
		{ID: "model-a", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true},
		{ID: "model-a", ProviderID: "anthropic", ProviderName: "Anthropic", Enabled: true},
		{ID: "model-b", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true},
	}}
	srv2 := newTestServer(t, &strategyFakeEngine{}, WithRegistry(reg), WithPriceBook(book))
	feed := getStrategies(t, srv2)
	byModel := map[string]chainStrategy{}
	for _, c := range feed.Chains {
		byModel[c.Model] = c
	}
	if a := byModel["model-a"]; a.CostUSDPer1M == nil || *a.CostUSDPer1M != 6.25 {
		t.Errorf("strategies model-a cost = %v (want 6.25: cheapest pair)", a.CostUSDPer1M)
	}
	if b := byModel["model-b"]; b.CostUSDPer1M == nil || *b.CostUSDPer1M != 2 {
		t.Errorf("strategies model-b cost = %v (want 2: model-level fallback)", b.CostUSDPer1M)
	}
	if feed.PricedModels != 2 {
		t.Errorf("priced_models = %d (want 2)", feed.PricedModels)
	}
}

// TestDashboardModelsPairPrices proves the Models feed shows the
// TRUE per-(provider, model) price: the pair price for the one
// pair that has it, the model-level fallback for the pair that
// does not, the provider-reported price where the operator
// configured nothing, and unknown where nothing exists — with
// the source named per row, never the cheapest pair standing in
// for this pair.
func TestDashboardModelsPairPrices(t *testing.T) {
	reg := &fakeModelRegistry{models: []provider.DashboardModel{
		{ID: "model-a", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true, InputPriceUSD: 1, OutputPriceUSD: 4},
		{ID: "model-a", ProviderID: "anthropic", ProviderName: "Anthropic", Enabled: true, InputPriceUSD: 100, OutputPriceUSD: 200},
		{ID: "model-b", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true, InputPriceUSD: 5, OutputPriceUSD: 5},
		{ID: "model-c", ProviderID: "deepseek", ProviderName: "DeepSeek", Enabled: true},
	}}
	book := &config.PriceBook{
		Pairs: map[config.PriceKey]config.ModelPrice{
			{Provider: "openai", Model: "model-a"}: {InputUSDPer1M: pricePtr(2), OutputUSDPer1M: pricePtr(10), CachedInputUSDPer1M: pricePtr(0.25)},
		},
		Entries: map[string]config.ModelPrice{
			"model-a": {InputUSDPer1M: pricePtr(50), OutputUSDPer1M: pricePtr(60)},
		},
	}
	srv := newTestServer(t, &fakeEngine{}, WithRegistry(reg), WithPriceBook(book))
	var resp dashboardModelsResponse
	getJSON(t, srv, "/dashboard/models", &resp)
	if len(resp.Models) != 4 {
		t.Fatalf("models = %d, want 4", len(resp.Models))
	}
	byPair := map[string]dashboardModel{}
	for _, m := range resp.Models {
		byPair[m.ProviderID+"/"+m.ID] = m
	}
	// Pair price wins over the provider-reported price AND over
	// the model-level fallback.
	aOpenAI := byPair["openai/model-a"]
	if aOpenAI.InputPriceUSD != 2 || aOpenAI.OutputPriceUSD != 10 || aOpenAI.CachedInputPriceUSD != 0.25 || aOpenAI.PriceSource != "pair" {
		t.Errorf("openai/model-a = in %.2f out %.2f cached %.2f source %q, want 2/10/0.25/pair", aOpenAI.InputPriceUSD, aOpenAI.OutputPriceUSD, aOpenAI.CachedInputPriceUSD, aOpenAI.PriceSource)
	}
	// No pair price for anthropic/model-a: the model-level
	// fallback applies — NOT openai's cheaper pair price.
	aAnthropic := byPair["anthropic/model-a"]
	if aAnthropic.InputPriceUSD != 50 || aAnthropic.OutputPriceUSD != 60 || aAnthropic.PriceSource != "model" {
		t.Errorf("anthropic/model-a = in %.2f out %.2f source %q, want 50/60/model", aAnthropic.InputPriceUSD, aAnthropic.OutputPriceUSD, aAnthropic.PriceSource)
	}
	// No operator price at all: the provider-reported price.
	bOpenAI := byPair["openai/model-b"]
	if bOpenAI.InputPriceUSD != 5 || bOpenAI.OutputPriceUSD != 5 || bOpenAI.PriceSource != "provider" {
		t.Errorf("openai/model-b = in %.2f out %.2f source %q, want 5/5/provider", bOpenAI.InputPriceUSD, bOpenAI.OutputPriceUSD, bOpenAI.PriceSource)
	}
	// Nothing anywhere: unknown, prices 0.
	cDeepseek := byPair["deepseek/model-c"]
	if cDeepseek.InputPriceUSD != 0 || cDeepseek.OutputPriceUSD != 0 || cDeepseek.PriceSource != "unknown" {
		t.Errorf("deepseek/model-c = in %.2f out %.2f source %q, want 0/0/unknown", cDeepseek.InputPriceUSD, cDeepseek.OutputPriceUSD, cDeepseek.PriceSource)
	}
}

// TestAutoRouteEnrichedPairPrices proves the auto-router explain
// view lists the true price at EVERY provider offering the
// resolved model — cheapest first and marked, unknown last —
// instead of only the cheapest pair.
func TestAutoRouteEnrichedPairPrices(t *testing.T) {
	reg := &fakeModelRegistry{models: []provider.DashboardModel{
		{ID: "model-a", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true, InputPriceUSD: 1, OutputPriceUSD: 4},
		{ID: "model-a", ProviderID: "anthropic", ProviderName: "Anthropic", Enabled: true},
		{ID: "model-a", ProviderID: "deepseek", ProviderName: "DeepSeek", Enabled: true},
		{ID: "model-a", ProviderID: "cheap-half", ProviderName: "CheapHalf", Enabled: true},
	}}
	book := &config.PriceBook{
		Pairs: map[config.PriceKey]config.ModelPrice{
			{Provider: "openai", Model: "model-a"}:     {InputUSDPer1M: pricePtr(2), OutputUSDPer1M: pricePtr(10)},
			{Provider: "anthropic", Model: "model-a"}:  {InputUSDPer1M: pricePtr(50), OutputUSDPer1M: pricePtr(60)},
			{Provider: "cheap-half", Model: "model-a"}: {InputUSDPer1M: pricePtr(0.5)},
		},
	}
	srv := newTestServer(t, &fakeEngine{}, WithRegistry(reg), WithPriceBook(book))
	dec := autorouter.Decision{Tier: autorouter.TierReasoning, Model: "model-a"}
	enriched := srv.enrichDecision(httptest.NewRequest(http.MethodGet, "/dashboard/auto-route", nil), dec)
	if len(enriched.ModelPrices) != 4 {
		t.Fatalf("model prices = %d, want 4 (one per provider offering model-a)", len(enriched.ModelPrices))
	}
	// Cheapest blended first and marked: openai (6), then
	// anthropic (55); the half-known pair has no blended price
	// and the unknown pair sorts last.
	p := enriched.ModelPrices
	if p[0].Provider != "openai" || !p[0].Cheapest || p[0].BlendedUSDPer1M == nil || *p[0].BlendedUSDPer1M != 6 || p[0].Source != "pair" {
		t.Errorf("first pair = %+v, want openai cheapest blended 6 source pair", p[0])
	}
	if p[1].Provider != "anthropic" || p[1].Cheapest || p[1].BlendedUSDPer1M == nil || *p[1].BlendedUSDPer1M != 55 {
		t.Errorf("second pair = %+v, want anthropic blended 55, not cheapest", p[1])
	}
	if p[2].Provider != "cheap-half" || !p[2].Known || p[2].BlendedUSDPer1M != nil || p[2].InputUSDPer1M != 0.5 {
		t.Errorf("third pair = %+v, want cheap-half known (input 0.5) with no blended price", p[2])
	}
	if p[3].Provider != "deepseek" || p[3].Known || p[3].Source != "unknown" {
		t.Errorf("last pair = %+v, want deepseek unknown, sorted last", p[3])
	}
	// The single cheapest cost is unchanged for compat.
	if enriched.ModelCostUSDPer1M == nil || *enriched.ModelCostUSDPer1M != 6 || !enriched.ModelPriceKnown {
		t.Errorf("cheapest cost = %v known=%v, want 6 / true", enriched.ModelCostUSDPer1M, enriched.ModelPriceKnown)
	}
}

// TestDashboardModelsModelsDevProvenance (CHECKLIST 179-180): the
// Models feed names the provenance of every price and context
// window honestly — operator vs models.dev + sync time — and
// carries the sync status block. A synced price is never presented
// as an operator-set one, and a window the operator set reports
// "operator" even when a sync entry exists for another pair.
func TestDashboardModelsModelsDevProvenance(t *testing.T) {
	reg := &fakeModelRegistry{models: []provider.DashboardModel{
		{ID: "gpt-4o", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true, ContextWindow: 128000},
		{ID: "gpt-4o-mini", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true, ContextWindow: 128000},
		{ID: "claude", ProviderID: "anthropic", ProviderName: "Anthropic", Enabled: true, ContextWindow: 200000},
	}}
	book := &config.PriceBook{
		Entries: map[string]config.ModelPrice{
			"claude": {InputUSDPer1M: pricePtr(3), OutputUSDPer1M: pricePtr(15)},
		},
	}
	now := time.Now().UTC().Truncate(time.Second)
	book.SetSyncedPrices(map[config.PriceKey]config.SyncedPrice{
		{Provider: "openai", Model: "gpt-4o-mini"}: {Price: config.ModelPrice{InputUSDPer1M: pricePtr(0.15), OutputUSDPer1M: pricePtr(0.6)}, SyncedAt: now},
	})
	status := modelsync.Status{
		Enabled: true, URL: "https://models.dev/api.json",
		LastSuccessAt: &now,
		Entries: map[string]modelsync.StateEntry{
			modelsync.EntryKey("openai", "gpt-4o"):      {Provider: "openai", Model: "gpt-4o", ContextWindow: 128000, SyncedAt: now},
			modelsync.EntryKey("openai", "gpt-4o-mini"): {Provider: "openai", Model: "gpt-4o-mini", Price: &config.ModelPrice{InputUSDPer1M: pricePtr(0.15), OutputUSDPer1M: pricePtr(0.6)}, ContextWindow: 128000, SyncedAt: now},
		},
	}
	srv := newTestServer(t, &fakeEngine{}, WithRegistry(reg), WithPriceBook(book),
		WithModelsDevStatus(func() modelsync.Status { return status }))
	var resp dashboardModelsResponse
	getJSON(t, srv, "/dashboard/models", &resp)
	if len(resp.Models) != 3 {
		t.Fatalf("models = %d, want 3", len(resp.Models))
	}
	byPair := map[string]dashboardModel{}
	for _, m := range resp.Models {
		byPair[m.ProviderID+"/"+m.ID] = m
	}
	// gpt-4o: no price anywhere (unknown), window synced from models.dev.
	fourO := byPair["openai/gpt-4o"]
	if fourO.PriceSource != "unknown" {
		t.Errorf("gpt-4o price_source = %q, want unknown", fourO.PriceSource)
	}
	if fourO.ContextWindowSource != "models.dev" || fourO.ContextWindowSyncedAt == nil || !fourO.ContextWindowSyncedAt.Equal(now) {
		t.Errorf("gpt-4o window provenance = %q at %v, want models.dev at %v", fourO.ContextWindowSource, fourO.ContextWindowSyncedAt, now)
	}
	// gpt-4o-mini: synced price with its sync time, synced window.
	mini := byPair["openai/gpt-4o-mini"]
	if mini.PriceSource != "models.dev" || mini.InputPriceUSD != 0.15 || mini.OutputPriceUSD != 0.6 {
		t.Errorf("gpt-4o-mini = in %.2f out %.2f source %q, want 0.15/0.6/models.dev", mini.InputPriceUSD, mini.OutputPriceUSD, mini.PriceSource)
	}
	if mini.PriceSyncedAt == nil || !mini.PriceSyncedAt.Equal(now) {
		t.Errorf("gpt-4o-mini price_synced_at = %v, want %v", mini.PriceSyncedAt, now)
	}
	if mini.ContextWindowSource != "models.dev" {
		t.Errorf("gpt-4o-mini window source = %q, want models.dev", mini.ContextWindowSource)
	}
	// claude: operator model-level price + operator window (no sync entry).
	claude := byPair["anthropic/claude"]
	if claude.PriceSource != "model" || claude.InputPriceUSD != 3 {
		t.Errorf("claude = in %.2f source %q, want 3/model", claude.InputPriceUSD, claude.PriceSource)
	}
	if claude.ContextWindowSource != "operator" || claude.ContextWindowSyncedAt != nil {
		t.Errorf("claude window provenance = %q at %v, want operator/nil", claude.ContextWindowSource, claude.ContextWindowSyncedAt)
	}
	// Status block: enabled, last success, not stale.
	if resp.ModelsDev == nil || !resp.ModelsDev.Enabled || resp.ModelsDev.LastSuccessAt == nil || resp.ModelsDev.Stale {
		t.Errorf("models_dev block = %+v, want enabled + success + not stale", resp.ModelsDev)
	}
	if resp.ModelsDev.PricesSynced != 1 || resp.ModelsDev.WindowsSynced != 2 {
		t.Errorf("models_dev counts = %d prices / %d windows, want 1/2", resp.ModelsDev.PricesSynced, resp.ModelsDev.WindowsSynced)
	}
}

// TestPricesEndpointModelsDev (CHECKLIST 179-180): the prices feed
// lists synced rows with source "models.dev" + sync time, and the
// status block reports a failed last attempt as stale (last good
// data, stale since the last success) — never silent.
func TestPricesEndpointModelsDev(t *testing.T) {
	book := &config.PriceBook{
		Pairs: map[config.PriceKey]config.ModelPrice{
			{Provider: "openai", Model: "gpt-4o"}: {InputUSDPer1M: pricePtr(2), OutputUSDPer1M: pricePtr(10)},
		},
		PairSources: map[config.PriceKey]string{{Provider: "openai", Model: "gpt-4o"}: "config"},
	}
	now := time.Now().UTC().Truncate(time.Second)
	attempt := now.Add(time.Minute)
	book.SetSyncedPrices(map[config.PriceKey]config.SyncedPrice{
		{Provider: "openai", Model: "gpt-4o-mini"}: {Price: config.ModelPrice{InputUSDPer1M: pricePtr(0.15), OutputUSDPer1M: pricePtr(0.6)}, SyncedAt: now},
	})
	status := modelsync.Status{
		Enabled: true, URL: "https://models.dev/api.json",
		LastAttemptAt: &attempt, LastSuccessAt: &now, LastError: "dial tcp: connection refused",
		Entries: map[string]modelsync.StateEntry{
			modelsync.EntryKey("openai", "gpt-4o-mini"): {Provider: "openai", Model: "gpt-4o-mini", Price: &config.ModelPrice{InputUSDPer1M: pricePtr(0.15), OutputUSDPer1M: pricePtr(0.6)}, SyncedAt: now},
		},
	}
	srv := newTestServer(t, &fakeEngine{}, WithPriceBook(book),
		WithModelsDevStatus(func() modelsync.Status { return status }))
	var resp pricesResponse
	getJSON(t, srv, "/dashboard/prices", &resp)
	byPair := map[string]priceRow{}
	for _, row := range resp.Prices {
		byPair[row.Provider+"/"+row.Model] = row
	}
	op := byPair["openai/gpt-4o"]
	if op.Source != "config" || op.SyncedAt != nil {
		t.Errorf("operator row = source %q synced_at %v, want config/nil", op.Source, op.SyncedAt)
	}
	synced := byPair["openai/gpt-4o-mini"]
	if synced.Source != "models.dev" || synced.SyncedAt == nil || !synced.SyncedAt.Equal(now) {
		t.Errorf("synced row = source %q synced_at %v, want models.dev/%v", synced.Source, synced.SyncedAt, now)
	}
	if synced.InputUSDPer1M == nil || *synced.InputUSDPer1M != 0.15 {
		t.Errorf("synced row input = %v, want 0.15", synced.InputUSDPer1M)
	}
	if resp.ModelsDev == nil || !resp.ModelsDev.Stale || resp.ModelsDev.LastError == "" {
		t.Errorf("models_dev block = %+v, want stale with the recorded error", resp.ModelsDev)
	}
}

// TestPricesEndpointNoModelsDev: with no sync wired, the feeds omit
// the models_dev block entirely — "never configured", not "unknown".
func TestPricesEndpointNoModelsDev(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithPriceBook(testPriceBook()))
	var resp pricesResponse
	getJSON(t, srv, "/dashboard/prices", &resp)
	if resp.ModelsDev != nil {
		t.Errorf("models_dev block present with no sync wired: %+v", resp.ModelsDev)
	}
}
