package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/web"
)

// TestAutoRouteDecisionPriceDisplay (CHECKLIST 213-214) pins the
// auto-route decision price display end to end, feed -> served
// page, so the x1e6 bug cannot regress silently:
//
//   - the feed carries the per-1M price as-is: a $2/1M input price
//     arrives as input_usd_per_1m = 2 (not 2,000,000, not 0.000002);
//   - the served dashboard renders those fields with moneyPerM,
//     the as-is per-1M formatter shared with the Models table —
//     the page contains no "* 1e6" / "* 1000000" price
//     multiplication anywhere, and the old fmtP helper is gone.
//
// moneyPerM's own arithmetic is pinned at runtime by the live
// smoke (node evaluates the extracted function: 2 -> "$2.00",
// 0.0025 -> "$0.0025", 10 -> "$10.00"); Go cannot execute the
// page's JS, so this test pins the wiring the smoke executes.
func TestAutoRouteDecisionPriceDisplay(t *testing.T) {
	reg := &fakeModelRegistry{models: []provider.DashboardModel{
		{ID: "model-a", ProviderID: "openai", ProviderName: "OpenAI", Enabled: true, InputPriceUSD: 1, OutputPriceUSD: 4},
		{ID: "model-a", ProviderID: "anthropic", ProviderName: "Anthropic", Enabled: true},
		{ID: "model-a", ProviderID: "tiny", ProviderName: "Tiny", Enabled: true},
	}}
	book := &config.PriceBook{
		Pairs: map[config.PriceKey]config.ModelPrice{
			{Provider: "openai", Model: "model-a"}: {InputUSDPer1M: pricePtr(2), OutputUSDPer1M: pricePtr(10)},
			{Provider: "tiny", Model: "model-a"}:   {InputUSDPer1M: pricePtr(0.0025), OutputUSDPer1M: pricePtr(0.01)},
		},
	}
	srv := newTestServer(t, &fakeEngine{}, WithRegistry(reg), WithPriceBook(book))
	dec := autorouter.Decision{Tier: autorouter.TierReasoning, Model: "model-a"}
	enriched := srv.enrichDecision(httptest.NewRequest(http.MethodGet, "/dashboard/auto-route", nil), dec)
	if len(enriched.ModelPrices) != 3 {
		t.Fatalf("model prices = %d, want 3 (one per provider offering model-a)", len(enriched.ModelPrices))
	}
	byProvider := map[string]modelPairPrice{}
	for _, p := range enriched.ModelPrices {
		byProvider[p.Provider] = p
	}
	// Feed: $2/1M in, $10/1M out — the exact values the page must
	// display, unscaled.
	openai := byProvider["openai"]
	if openai.InputUSDPer1M != 2 || openai.OutputUSDPer1M != 10 {
		t.Errorf("openai/model-a feed = in %.6f out %.6f, want in 2 out 10 (per-1M, unscaled)", openai.InputUSDPer1M, openai.OutputUSDPer1M)
	}
	if openai.BlendedUSDPer1M == nil || *openai.BlendedUSDPer1M != 6 {
		t.Errorf("openai/model-a blended = %v, want 6 (mean of 2 and 10)", openai.BlendedUSDPer1M)
	}
	// The sub-cent pair is genuinely cheapest (blended 0.00625),
	// so the single cheapest cost is its blend, not openai's 6.
	if enriched.ModelCostUSDPer1M == nil || *enriched.ModelCostUSDPer1M != 0.00625 {
		t.Errorf("cheapest blended cost = %v, want 0.00625 (tiny pair blend)", *enriched.ModelCostUSDPer1M)
	}
	if !byProvider["tiny"].Cheapest {
		t.Errorf("tiny pair Cheapest = false, want true (blended 0.00625 is lowest)")
	}
	// Sub-cent feed value survives unscaled too — moneyPerM keeps
	// 4 decimals so it must never read $0.00.
	tiny := byProvider["tiny"]
	if tiny.InputUSDPer1M != 0.0025 || tiny.OutputUSDPer1M != 0.01 {
		t.Errorf("tiny/model-a feed = in %.6f out %.6f, want in 0.0025 out 0.01", tiny.InputUSDPer1M, tiny.OutputUSDPer1M)
	}

	// Served page: the decision's per-provider lines use moneyPerM.
	page := string(web.IndexHTML)
	if !strings.Contains(page, "moneyPerM(p.input_usd_per_1m)") || !strings.Contains(page, "moneyPerM(p.output_usd_per_1m)") {
		t.Errorf("dashboard does not render the decision pair prices with moneyPerM (per-1M as-is formatter)")
	}
	// No x1e6 multiplication anywhere in the served page: the
	// only "1e6" mentions left are comments describing the old
	// bug, never an operator. A reintroduced "* 1e6" (or
	// "* 1000000") on any price fails this test.
	if strings.Contains(page, "* 1e6") || strings.Contains(page, "*1e6") || strings.Contains(page, "* 1000000") || strings.Contains(page, "*1000000") {
		t.Errorf("dashboard still multiplies a price by 1e6 — a $2/1M price would display as $2,000,000")
	}
	if strings.Contains(page, "fmtP(") {
		t.Errorf("dashboard still contains the old fmtP helper that multiplied per-1M prices by 1e6")
	}
	// moneyPerM itself must not scale: its body (up to its
	// closing brace) contains no multiplication at all.
	start := strings.Index(page, "function moneyPerM(v){")
	if start < 0 {
		t.Fatalf("dashboard has no moneyPerM formatter")
	}
	rest := page[start:]
	end := strings.Index(rest, "\n  }\n")
	if end < 0 {
		t.Fatalf("cannot find the end of moneyPerM in the dashboard")
	}
	body := rest[:end]
	// Strip the /* */ comment inside moneyPerM before checking
	// for multiplication — the comment delimiters themselves
	// contain "*".
	if i := strings.Index(body, "/*"); i >= 0 {
		if j := strings.Index(body[i:], "*/"); j >= 0 {
			body = body[:i] + body[i+j+2:]
		}
	}
	if strings.Contains(body, "*") {
		t.Errorf("moneyPerM body scales its input: %q", body)
	}
	if !strings.Contains(body, "v.toFixed(2)") || !strings.Contains(body, "v.toFixed(4)") {
		t.Errorf("moneyPerM body lost its 2/4-decimal formatting: %q", body)
	}

	// The served page is what the binary actually serves: fetch
	// it through the handler, not just the embedded bytes.
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "moneyPerM(p.input_usd_per_1m)") {
		t.Errorf("GET /dashboard body does not render the decision pair prices with moneyPerM")
	}
	if strings.Contains(rec.Body.String(), "* 1e6") {
		t.Errorf("GET /dashboard body still multiplies a price by 1e6")
	}
}
