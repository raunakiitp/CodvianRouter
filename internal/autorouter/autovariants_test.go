package autorouter

import (
	"strings"
	"testing"
)

// fixedCatalog is the shared fixture for every auto-variant test.
// Each row pins the signals one decision reads, so the expected
// winner is hand-computed, not re-derived from the code under test:
//
//	cc/claude-opus        coding, premium, subscription (hard-stop), 60% left, $15, p95 800ms, quality 0.90, headroom 0.60
//	glm/glm-free          chat, free, keyless, 80% left, $0, p95 400ms, quality 0.50, headroom 0.80
//	deepseek/deepseek-chat chat, cheap, metered, 50% left, $0.14, p95 300ms, quality 0.60, headroom 0.50
//	openai/gpt-5          vision+reasoning, premium, metered, 90% left, $5, p95 1200ms, quality 0.95, headroom 0.90
//	cc/claude-haiku-sub   chat, cheap, subscription (hard-stop), 1% left (exhausted), $1, headroom 0.01
//	zhipu/glm-free-2      chat, free, metered, 40% left, $0, p95 500ms, quality 0.40, headroom 0.40
//	xai/grok              reasoning, premium, unknown billing, no quota reading, $3, p95 900ms, quality 0.85, headroom n/a
func fixedCatalog() []CatalogModel {
	return []CatalogModel{
		{Provider: "cc", Model: "cc/claude-opus", Category: "coding", Reasoning: true, Tier: "premium", Billing: "subscription", OverageHardStop: true, PriceUSDPerMTok: 15, HasPrice: true, LatencyMs: 800, HasLatency: true, QuotaHeadroom: 0.60, HasQuota: true, RemainingPercent: 60, HasRemaining: true, Health: 1, HasHealth: true, Quality: 0.90, HasQuality: true},
		{Provider: "glm", Model: "glm/glm-free", Category: "chat", Tier: "free", Billing: "keyless", PriceUSDPerMTok: 0, HasPrice: true, LatencyMs: 400, HasLatency: true, QuotaHeadroom: 0.80, HasQuota: true, RemainingPercent: 80, HasRemaining: true, Health: 1, HasHealth: true, Quality: 0.50, HasQuality: true},
		{Provider: "deepseek", Model: "deepseek/deepseek-chat", Category: "chat", Tier: "cheap", Billing: "metered", PriceUSDPerMTok: 0.14, HasPrice: true, LatencyMs: 300, HasLatency: true, QuotaHeadroom: 0.50, HasQuota: true, RemainingPercent: 50, HasRemaining: true, Health: 1, HasHealth: true, Quality: 0.60, HasQuality: true},
		{Provider: "openai", Model: "openai/gpt-5", Category: "vision", Vision: true, Reasoning: true, Tier: "premium", Billing: "metered", PriceUSDPerMTok: 5, HasPrice: true, LatencyMs: 1200, HasLatency: true, QuotaHeadroom: 0.90, HasQuota: true, RemainingPercent: 90, HasRemaining: true, Health: 0.5, HasHealth: true, Quality: 0.95, HasQuality: true},
		{Provider: "cc", Model: "cc/claude-haiku-sub", Category: "chat", Tier: "cheap", Billing: "subscription", OverageHardStop: true, PriceUSDPerMTok: 1, HasPrice: true, LatencyMs: 600, HasLatency: true, QuotaHeadroom: 0.01, HasQuota: true, RemainingPercent: 1, HasRemaining: true, Health: 1, HasHealth: true, Quality: 0.70, HasQuality: true},
		{Provider: "zhipu", Model: "zhipu/glm-free-2", Category: "chat", Tier: "free", Billing: "metered", PriceUSDPerMTok: 0, HasPrice: true, LatencyMs: 500, HasLatency: true, QuotaHeadroom: 0.40, HasQuota: true, RemainingPercent: 40, HasRemaining: true, Health: 1, HasHealth: true, Quality: 0.40, HasQuality: true},
		{Provider: "xai", Model: "xai/grok", Category: "reasoning", Reasoning: true, Tier: "premium", Billing: "unknown", PriceUSDPerMTok: 3, HasPrice: true, LatencyMs: 900, HasLatency: true, Quality: 0.85, HasQuality: true},
	}
}

func resolve(t *testing.T, id string, catalog []CatalogModel, lastGood string) VariantResolution {
	t.Helper()
	res, err := ResolveAutoVariant(id, catalog, lastGood)
	if err != nil {
		t.Fatalf("ResolveAutoVariant(%q) error: %v", id, err)
	}
	return res
}

func TestAutoVariantOfflineMostHeadroomFirst(t *testing.T) {
	res := resolve(t, "auto/offline", fixedCatalog(), "")
	if res.Strategy != "offline-friendly" {
		t.Fatalf("strategy = %q, want offline-friendly", res.Strategy)
	}
	// Headrooms: opus .60, glm .80, deepseek .50, gpt-5 .90, haiku .01, zhipu .40, grok none -> 1.00 (no fetcher).
	// grok has no quota fetcher, so it counts as FULL headroom (1.0) per the reference default of 100.
	if res.Model != "xai/grok" {
		t.Fatalf("model = %q, want xai/grok (no quota fetcher counts as full headroom)", res.Model)
	}
}

func TestAutoVariantOfflineUnreadableScoresZero(t *testing.T) {
	cat := []CatalogModel{
		{Provider: "a", Model: "a/m1", QuotaUnreadable: true, HasQuota: true, QuotaHeadroom: 0.9},
		{Provider: "b", Model: "b/m2", QuotaHeadroom: 0.10, HasQuota: true},
	}
	res := resolve(t, "auto/offline", cat, "")
	if res.Model != "b/m2" {
		t.Fatalf("model = %q, want b/m2 (unreadable quota scores 0, below the 0.10 reading)", res.Model)
	}
}

func TestAutoVariantLKGPExplicit(t *testing.T) {
	res := resolve(t, "auto/lkgp", fixedCatalog(), "deepseek/deepseek-chat")
	if res.Strategy != "lkgp" || res.Model != "deepseek/deepseek-chat" {
		t.Fatalf("got strategy=%q model=%q, want lkgp/deepseek/deepseek-chat", res.Strategy, res.Model)
	}
}

func TestAutoVariantLKGPNoRecordErrors(t *testing.T) {
	if _, err := ResolveAutoVariant("auto/lkgp", fixedCatalog(), ""); err == nil {
		t.Fatal("auto/lkgp with no recorded model: expected error, got none")
	}
}

func TestAutoVariantLKGPNotInCatalogErrors(t *testing.T) {
	if _, err := ResolveAutoVariant("auto/lkgp", fixedCatalog(), "gone/model"); err == nil {
		t.Fatal("auto/lkgp with a recorded model outside the catalog: expected error, got none")
	}
}

func TestAutoVariantChaosPanelOfFiveOnePerProvider(t *testing.T) {
	res := resolve(t, "auto/chaos", fixedCatalog(), "")
	if res.Strategy != "chaos-mode" {
		t.Fatalf("strategy = %q, want chaos-mode", res.Strategy)
	}
	if res.Model != "" {
		t.Fatalf("model = %q, want empty: a chaos decision is a panel, not a single model", res.Model)
	}
	// One model per provider, catalog order, max 5: cc, glm, deepseek, openai, zhipu.
	// xai is the 6th provider and is cut by the panel cap.
	want := []string{"cc/claude-opus", "glm/glm-free", "deepseek/deepseek-chat", "openai/gpt-5", "zhipu/glm-free-2"}
	if len(res.Panel) != len(want) {
		t.Fatalf("panel = %v, want %v", res.Panel, want)
	}
	for i, m := range want {
		if res.Panel[i] != m {
			t.Fatalf("panel = %v, want %v", res.Panel, want)
		}
	}
}

func TestAutoVariantChaosBestAlias(t *testing.T) {
	res := resolve(t, "auto/best-chaos", fixedCatalog(), "")
	if len(res.Panel) != 5 || res.Strategy != "chaos-mode" {
		t.Fatalf("auto/best-chaos: panel=%v strategy=%q, want 5-member chaos-mode panel", res.Panel, res.Strategy)
	}
}

func TestAutoVariantSubscriptionFailClosed(t *testing.T) {
	res := resolve(t, "auto/subscription", fixedCatalog(), "")
	// Subscription rung: opus (60% left, hard-stop) only — haiku-sub is
	// subscription too but at 1% remaining, below the 2% exit cutoff.
	if res.Model != "cc/claude-opus" {
		t.Fatalf("model = %q, want cc/claude-opus (only verified subscription candidate)", res.Model)
	}
	if res.Strategy != "balanced" {
		t.Fatalf("strategy = %q, want balanced (subscription carries no weight bias)", res.Strategy)
	}
}

func TestAutoVariantSubscriptionEmptyPoolErrorsNeverBillable(t *testing.T) {
	meteredOnly := []CatalogModel{
		{Provider: "deepseek", Model: "deepseek/deepseek-chat", Tier: "cheap", Billing: "metered", RemainingPercent: 50, HasRemaining: true},
		{Provider: "glm", Model: "glm/glm-free", Tier: "free", Billing: "keyless", RemainingPercent: 80, HasRemaining: true},
	}
	res, err := ResolveAutoVariant("auto/subscription", meteredOnly, "")
	if err == nil {
		t.Fatalf("auto/subscription with no subscription target: expected error, got model %q", res.Model)
	}
	if !strings.Contains(err.Error(), "no billable fallback") && !strings.Contains(err.Error(), "failing closed") {
		t.Fatalf("error %q does not state the fail-closed promise", err.Error())
	}
}

func TestAutoVariantSubscriptionExcludesUnverifiedAndSoftOverage(t *testing.T) {
	cat := []CatalogModel{
		// Subscription billing but overage meters to paid: excluded.
		{Provider: "a", Model: "a/soft", Billing: "subscription", OverageHardStop: false, RemainingPercent: 90, HasRemaining: true},
		// Subscription billing, hard-stop, but no quota reading: excluded.
		{Provider: "b", Model: "b/unverified", Billing: "subscription", OverageHardStop: true},
	}
	if _, err := ResolveAutoVariant("auto/subscription", cat, ""); err == nil {
		t.Fatal("auto/subscription with only unverifiable/soft-overage candidates: expected error, got none")
	}
}

func TestAutoVariantThriftyRungOrder(t *testing.T) {
	res := resolve(t, "auto/thrifty", fixedCatalog(), "")
	// Rungs: subscription opus (60) -> keyless glm (80) -> free zhipu (40,
	// metered free) -> cheap deepseek (50) -> premium gpt-5 (90), grok.
	// haiku-sub is subscription at 1%: exhausted, gated out.
	want := []string{"cc/claude-opus", "glm/glm-free", "zhipu/glm-free-2", "deepseek/deepseek-chat", "openai/gpt-5", "xai/grok"}
	if len(res.Ordered) != len(want) {
		t.Fatalf("ordered = %v, want %v", res.Ordered, want)
	}
	for i, m := range want {
		if res.Ordered[i] != m {
			t.Fatalf("ordered = %v, want %v", res.Ordered, want)
		}
	}
	if res.Model != "cc/claude-opus" {
		t.Fatalf("model = %q, want cc/claude-opus (first surviving rung)", res.Model)
	}
}

func TestAutoVariantThriftyAdmitsUnknownQuota(t *testing.T) {
	cat := []CatalogModel{
		{Provider: "a", Model: "a/sub-unknown", Billing: "subscription", OverageHardStop: true},
		{Provider: "b", Model: "b/cheap", Tier: "cheap", Billing: "metered", RemainingPercent: 50, HasRemaining: true},
	}
	res := resolve(t, "auto/thrifty", cat, "")
	// Unlike subscription, thrifty ADMITs the plan connection with no
	// usable quota reading (trying it costs nothing).
	if res.Model != "a/sub-unknown" || len(res.Ordered) != 2 {
		t.Fatalf("model=%q ordered=%v, want a/sub-unknown first of 2", res.Model, res.Ordered)
	}
}

func TestAutoVariantBestFamily(t *testing.T) {
	cases := []struct {
		id, wantModel, wantStrategy string
	}{
		// best-coding: coding adds NO pool filter in the reference
		// (buildAutoCandidateFilter) — quality-first over the full
		// pool picks the highest quality signal, gpt-5 (0.95).
		{"auto/best-coding", "openai/gpt-5", "quality-first"},
		// best-reasoning: reasoning-capable are opus (.90), gpt-5 (.95), grok (.85) -> gpt-5.
		{"auto/best-reasoning", "openai/gpt-5", "quality-first"},
		// best-fast: whole pool, lowest p95 -> deepseek 300ms.
		{"auto/best-fast", "deepseek/deepseek-chat", "ship-fast"},
		// best-vision: vision category (not a flat variant) -> gpt-5 only, balanced weights.
		{"auto/best-vision", "openai/gpt-5", "balanced"},
		// best-chat: chat adds no pool filter; balanced -> catalog order -> opus.
		{"auto/best-chat", "cc/claude-opus", "balanced"},
		// best-coding-fast: coding adds no filter; ship-fast over the full pool -> deepseek.
		{"auto/best-coding-fast", "deepseek/deepseek-chat", "ship-fast"},
		// best-free: free-tier pool (glm $0, zhipu $0), cheapest tie -> catalog order -> glm.
		{"auto/best-free", "glm/glm-free", "cost-saver"},
	}
	for _, c := range cases {
		res := resolve(t, c.id, fixedCatalog(), "")
		if res.Model != c.wantModel || res.Strategy != c.wantStrategy {
			t.Errorf("%s: got model=%q strategy=%q, want model=%q strategy=%q (notes: %s)", c.id, res.Model, res.Strategy, c.wantModel, c.wantStrategy, res.Notes)
		}
	}
}

func TestAutoVariantProFamily(t *testing.T) {
	cases := []struct {
		id, wantModel, wantStrategy string
	}{
		// pro-coding: coding adds no pool filter; premium filter
		// (opus, gpt-5, grok) + quality-first -> gpt-5.
		{"auto/pro-coding", "openai/gpt-5", "quality-first"},
		// pro-reasoning: reasoning + premium (opus .90, gpt-5 .95, grok .85) -> gpt-5.
		{"auto/pro-reasoning", "openai/gpt-5", "quality-first"},
		// pro-vision: vision + premium -> gpt-5 alone, balanced weights.
		{"auto/pro-vision", "openai/gpt-5", "balanced"},
		// pro-chat: chat adds no filter; premium filter matches, balanced -> opus.
		{"auto/pro-chat", "cc/claude-opus", "balanced"},
		// pro-fast: premium pool (opus 800ms, gpt-5 1200ms, grok 900ms) -> opus.
		{"auto/pro-fast", "cc/claude-opus", "ship-fast"},
	}
	for _, c := range cases {
		res := resolve(t, c.id, fixedCatalog(), "")
		if res.Model != c.wantModel || res.Strategy != c.wantStrategy {
			t.Errorf("%s: got model=%q strategy=%q, want model=%q strategy=%q (notes: %s)", c.id, res.Model, res.Strategy, c.wantModel, c.wantStrategy, res.Notes)
		}
	}
	// pro-chat matched the premium filter (no fail-open happened).
	res := resolve(t, "auto/pro-chat", fixedCatalog(), "")
	if !strings.Contains(res.Notes, "premium-tier") {
		t.Fatalf("auto/pro-chat notes %q do not record the premium-tier filter", res.Notes)
	}
	// The fail-open path is covered by auto/multimodal:free below.
}

func TestAutoVariantCategoryTierComposition(t *testing.T) {
	cases := []struct {
		id, wantModel, wantStrategy string
	}{
		{"auto/coding:fast", "deepseek/deepseek-chat", "ship-fast"}, // coding adds no filter; fastest overall
		{"auto/coding:cheap", "glm/glm-free", "cost-saver"},         // coding adds no filter; cheapest overall ($0 tie -> catalog order)
		{"auto/coding:floor", "glm/glm-free", "cost-saver"},         // floor is the cheap alias
		{"auto/reasoning:pro", "openai/gpt-5", "quality-first"},
		{"auto/vision", "openai/gpt-5", "balanced"},               // no tier -> balanced weights
		{"auto/multimodal:free", "openai/gpt-5", "quality-first"}, // vision+free matches nothing -> fail-open full pool -> gpt-5 by quality
		{"auto/coding:subscription", "cc/claude-opus", "balanced"},
		{"auto/reasoning", "cc/claude-opus", "balanced"},              // no tier -> balanced weights; reasoning pool in catalog order
		{"auto/chat:reliable", "cc/claude-opus", "reliability-first"}, // chat adds no filter; all health 1.0 except gpt-5 (0.5) -> catalog order
	}
	for _, c := range cases {
		res := resolve(t, c.id, fixedCatalog(), "")
		if res.Model != c.wantModel || res.Strategy != c.wantStrategy {
			t.Errorf("%s: got model=%q strategy=%q, want model=%q strategy=%q (notes: %s)", c.id, res.Model, res.Strategy, c.wantModel, c.wantStrategy, res.Notes)
		}
	}
}

func TestAutoVariantChatThriftyComposition(t *testing.T) {
	res := resolve(t, "auto/chat:thrifty", fixedCatalog(), "")
	// Chat adds no pool filter, so this is thrifty over the full
	// catalog: subscription opus -> keyless glm -> free zhipu ->
	// cheap deepseek -> premium gpt-5, grok. haiku-sub is
	// subscription at 1%: exhausted, gated out.
	want := []string{"cc/claude-opus", "glm/glm-free", "zhipu/glm-free-2", "deepseek/deepseek-chat", "openai/gpt-5", "xai/grok"}
	if len(res.Ordered) != len(want) {
		t.Fatalf("ordered = %v, want %v", res.Ordered, want)
	}
	for i, m := range want {
		if res.Ordered[i] != m {
			t.Fatalf("ordered = %v, want %v", res.Ordered, want)
		}
	}
}

func TestAutoVariantFailOpenFilterSaysSo(t *testing.T) {
	res := resolve(t, "auto/multimodal:free", fixedCatalog(), "")
	if !strings.Contains(res.Notes, "fail-open") {
		t.Fatalf("notes %q do not record the fail-open filter (no vision model is free-tier)", res.Notes)
	}
}

func TestAutoVariantCodeNarrowsToCodingReasoning(t *testing.T) {
	res := resolve(t, "auto/code", fixedCatalog(), "")
	// Pool narrows to coding/reasoning (opus, gpt-5, grok); the
	// code strategy ranks by cost per solved task, whose documented
	// neutral default without outcome evidence is price order:
	// grok $3 < gpt-5 $5 < opus $15.
	if res.Strategy != "code" || res.Model != "xai/grok" {
		t.Fatalf("got strategy=%q model=%q, want code/xai/grok (notes: %s)", res.Strategy, res.Model, res.Notes)
	}
}

func TestAutoVariantCodeEmptyNarrowingErrors(t *testing.T) {
	cat := []CatalogModel{
		{Provider: "a", Model: "a/chat-only", Category: "chat"},
	}
	if _, err := ResolveAutoVariant("auto/code", cat, ""); err == nil {
		t.Fatal("auto/code with no coding/reasoning model: expected error, got none")
	}
}

func TestAutoVariantPlainAutoNeedsPrompt(t *testing.T) {
	res := resolve(t, "auto", fixedCatalog(), "")
	if res.Strategy != "balanced" {
		t.Fatalf("strategy = %q, want balanced", res.Strategy)
	}
	if res.Model != "" {
		t.Fatalf("model = %q, want empty: plain auto names no model without a prompt classification", res.Model)
	}
}

func TestAutoVariantCheapUnknownPriceSortsLast(t *testing.T) {
	cat := []CatalogModel{
		{Provider: "a", Model: "a/unpriced-first"},
		{Provider: "b", Model: "b/priced", PriceUSDPerMTok: 2, HasPrice: true},
	}
	res := resolve(t, "auto/cheap", cat, "")
	if res.Model != "b/priced" {
		t.Fatalf("model = %q, want b/priced (unknown price never cheapest)", res.Model)
	}
	cat2 := []CatalogModel{
		{Provider: "a", Model: "a/first"},
		{Provider: "b", Model: "b/second"},
	}
	res2 := resolve(t, "auto/cheap", cat2, "")
	if res2.Model != "a/first" || !strings.Contains(res2.Notes, "no prices known") {
		t.Fatalf("model=%q notes=%q, want a/first with a no-prices note", res2.Model, res2.Notes)
	}
}

func TestAutoVariantSmartNotesExploration(t *testing.T) {
	res := resolve(t, "auto/smart", fixedCatalog(), "")
	if res.Strategy != "quality-first" {
		t.Fatalf("strategy = %q, want quality-first", res.Strategy)
	}
	if !strings.Contains(res.Notes, "10% exploration") {
		t.Fatalf("notes %q do not record the 10%% exploration rate", res.Notes)
	}
}

func TestAutoVariantUnknownErrors(t *testing.T) {
	for _, id := range []string{
		"auto/bogus",
		"auto/best-bogus",
		"auto/pro-bogus",
		"auto/coding:bogus",
		"auto/bogus:pro",
		"auto/coding:fast:extra",
		"auto/",
		"gpt-5",
		"auto",
		"",
	} {
		if id == "auto" || id == "" {
			continue // plain auto is valid (tested above); "" is tested separately
		}
		if _, err := ResolveAutoVariant(id, fixedCatalog(), ""); err == nil {
			t.Errorf("ResolveAutoVariant(%q): expected error, got none", id)
		}
	}
	if _, err := ResolveAutoVariant("", fixedCatalog(), ""); err == nil {
		t.Error("ResolveAutoVariant(\"\"): expected error, got none")
	}
	if _, err := ResolveAutoVariant("auto/fast", nil, ""); err == nil {
		t.Error("ResolveAutoVariant with an empty catalog: expected error, got none")
	}
}

func TestAutoVariantIsKnownAutoID(t *testing.T) {
	known := []string{"auto", "auto/fast", "auto/offline", "auto/lkgp", "auto/chaos", "auto/subscription", "auto/thrifty", "auto/best-coding", "auto/pro-vision", "auto/coding:pro", "auto/multimodal:free", "auto/chat:thrifty"}
	for _, id := range known {
		if !IsKnownAutoID(id) {
			t.Errorf("IsKnownAutoID(%q) = false, want true", id)
		}
	}
	unknown := []string{"", "auto/", "auto/bogus", "auto/coding:bogus", "gpt-5", "intent/fast"}
	for _, id := range unknown {
		if IsKnownAutoID(id) {
			t.Errorf("IsKnownAutoID(%q) = true, want false", id)
		}
	}
}

func TestCascadeOrderSubscriptionAPIKeyCheapFree(t *testing.T) {
	steps := OrderCascade(fixedCatalog())
	// Subscription: opus (haiku-sub gated out at 1%, but the tier survives
	// via opus). API Key: metered deepseek, gpt-5, zhipu in catalog order.
	// Cheap: no member (cheap-tier models are subscription/metered, which
	// place higher) — omitted, not invented. Free: glm (keyless, free-tier).
	wantTiers := []CascadeTier{CascadeSubscription, CascadeAPIKey, CascadeFree}
	if len(steps) != len(wantTiers) {
		t.Fatalf("tiers = %v, want %v", tiersOf(steps), wantTiers)
	}
	for i, tier := range wantTiers {
		if steps[i].Tier != tier {
			t.Fatalf("tiers = %v, want %v", tiersOf(steps), wantTiers)
		}
	}
	if got := steps[0].Models; len(got) != 1 || got[0] != "cc/claude-opus" {
		t.Fatalf("subscription step = %v, want [cc/claude-opus]", got)
	}
	wantAPIKey := []string{"deepseek/deepseek-chat", "openai/gpt-5", "zhipu/glm-free-2"}
	if len(steps[1].Models) != len(wantAPIKey) {
		t.Fatalf("api-key step = %v, want %v", steps[1].Models, wantAPIKey)
	}
	for i, m := range wantAPIKey {
		if steps[1].Models[i] != m {
			t.Fatalf("api-key step = %v, want %v", steps[1].Models, wantAPIKey)
		}
	}
	if got := steps[2].Models; len(got) != 1 || got[0] != "glm/glm-free" {
		t.Fatalf("free step = %v, want [glm/glm-free]", got)
	}
}

func TestCascadeExhaustedTierReportedNotDropped(t *testing.T) {
	cat := []CatalogModel{
		{Provider: "cc", Model: "cc/sub-spent", Billing: "subscription", OverageHardStop: true, RemainingPercent: 0, HasRemaining: true},
		{Provider: "d", Model: "d/cheap", Tier: "cheap", Billing: "keyless", RemainingPercent: 50, HasRemaining: true},
	}
	steps := OrderCascade(cat)
	if len(steps) != 2 {
		t.Fatalf("steps = %v, want 2 (subscription exhausted + cheap)", tiersOf(steps))
	}
	if steps[0].Tier != CascadeSubscription || !steps[0].Exhausted || len(steps[0].Models) != 0 {
		t.Fatalf("subscription step = %+v, want exhausted with no models", steps[0])
	}
	if steps[1].Tier != CascadeCheap || len(steps[1].Models) != 1 {
		t.Fatalf("cheap step = %+v, want [d/cheap]", steps[1])
	}
}

func TestCascadeOrderConstant(t *testing.T) {
	want := []CascadeTier{CascadeSubscription, CascadeAPIKey, CascadeCheap, CascadeFree}
	if len(CascadeOrder) != len(want) {
		t.Fatalf("CascadeOrder = %v, want %v", CascadeOrder, want)
	}
	for i, tier := range want {
		if CascadeOrder[i] != tier {
			t.Fatalf("CascadeOrder = %v, want %v", CascadeOrder, want)
		}
	}
}

func TestEndpointStrategies(t *testing.T) {
	chat, err := StrategiesForEndpoint(EndpointChat)
	if err != nil {
		t.Fatalf("StrategiesForEndpoint(chat): %v", err)
	}
	for _, want := range []string{"priority", "auto", "fusion", "jev", "lkgp", "p2c"} {
		found := false
		for _, s := range chat {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("chat strategies %v lack %q", chat, want)
		}
	}
	nonChat, err := StrategiesForEndpoint(EndpointEmbeddings)
	if err != nil {
		t.Fatalf("StrategiesForEndpoint(embeddings): %v", err)
	}
	if len(nonChat) != len(NonChatStrategies) {
		t.Fatalf("embeddings strategies = %v, want exactly %v", nonChat, NonChatStrategies)
	}
	for _, ep := range []Endpoint{EndpointSearch, EndpointRerank, EndpointImages, EndpointVideos, EndpointMusic, EndpointOCR, EndpointAudioSpeech} {
		if _, err := StrategiesForEndpoint(ep); err != nil {
			t.Errorf("StrategiesForEndpoint(%q): %v", ep, err)
		}
	}
	if _, err := StrategiesForEndpoint("telepathy"); err == nil {
		t.Error("StrategiesForEndpoint(unknown): expected error, got none")
	}
}

func TestEndpointStrategyProjection(t *testing.T) {
	// Chat accepts anything (the chat plan is canonical).
	if got, err := ProjectStrategyForEndpoint("fusion", EndpointChat); err != nil || got != "fusion" {
		t.Fatalf("fusion on chat: got=%q err=%v, want fusion/nil", got, err)
	}
	// Non-chat accepts only the non-chat set.
	if got, err := ProjectStrategyForEndpoint("p2c", EndpointRerank); err != nil || got != "p2c" {
		t.Fatalf("p2c on rerank: got=%q err=%v, want p2c/nil", got, err)
	}
	for _, bad := range []string{"auto", "fusion", "jev", "lkgp", "cost-optimized", "headroom"} {
		if _, err := ProjectStrategyForEndpoint(bad, EndpointEmbeddings); err == nil {
			t.Errorf("strategy %q on embeddings: expected rejection, got none", bad)
		}
	}
	if _, err := ProjectStrategyForEndpoint("", EndpointChat); err == nil {
		t.Error("empty strategy: expected error, got none")
	}
	if _, err := ProjectStrategyForEndpoint("priority", "telepathy"); err == nil {
		t.Error("unknown endpoint: expected error, got none")
	}
}

func tiersOf(steps []CascadeStep) []CascadeTier {
	out := make([]CascadeTier, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Tier)
	}
	return out
}

// TestCascadeTierFor (G19 Area 1): the engine-side classifier
// places every catalog model the projection leaves unplaced —
// unknown billing defaults to metered (api-key) per the ladder's
// "uncurated is not free" rule — except keyless, which has no
// cascade rung and reports false.
func TestCascadeTierFor(t *testing.T) {
	cases := []struct {
		m    CatalogModel
		want CascadeTier
		ok   bool
	}{
		{CatalogModel{Billing: "subscription"}, CascadeSubscription, true},
		{CatalogModel{Billing: "metered"}, CascadeAPIKey, true},
		{CatalogModel{Billing: "metered", Tier: "free"}, CascadeAPIKey, true},
		{CatalogModel{Tier: "cheap"}, CascadeCheap, true},
		{CatalogModel{Tier: "free"}, CascadeFree, true},
		{CatalogModel{}, CascadeAPIKey, true}, // unknown billing: metered default
		{CatalogModel{Billing: "keyless"}, "", false},
		{CatalogModel{Billing: "keyless", Tier: "free"}, CascadeFree, true},
	}
	for _, tc := range cases {
		got, ok := CascadeTierFor(tc.m)
		if got != tc.want || ok != tc.ok {
			t.Errorf("CascadeTierFor(%+v) = (%q, %v), want (%q, %v)", tc.m, got, ok, tc.want, tc.ok)
		}
	}
}

// TestModelHasCapability (G19 Area 1): operator-declared
// capabilities match case-insensitively, in bare or
// provider-prefixed key form; an undeclared model is false.
func TestModelHasCapability(t *testing.T) {
	caps := map[string][]string{
		"gpt-4o":           {"Vision", "tools"},
		"anthropic/claude": {"vision"},
	}
	if !ModelHasCapability(caps, "gpt-4o", "vision") {
		t.Error("bare model with Vision capability: want true")
	}
	if !ModelHasCapability(caps, "openai/gpt-4o", "VISION") {
		t.Error("prefixed lookup against bare key: want true")
	}
	if !ModelHasCapability(caps, "claude", "vision") {
		t.Error("bare lookup against prefixed key: want true")
	}
	if ModelHasCapability(caps, "llama-3", "vision") {
		t.Error("undeclared model: want false")
	}
	if ModelHasCapability(caps, "gpt-4o", "audio") {
		t.Error("undeclared capability: want false")
	}
	if ModelHasCapability(nil, "gpt-4o", "vision") {
		t.Error("nil map: want false")
	}
}
