package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// G16 Area 11 ledger cost cluster tests.
//
// Row 1 — cost computation, all legs: the cached-input leg already
// existed (TestPricesCostWithCached); these tests prove the new
// per-endpoint price dimension on top of it.
func TestG16PerEndpointPricing(t *testing.T) {
	emb := 0.0000001 // $0.10 / 1M
	chatIn := 0.0000025
	chatOut := 0.00001
	prices := Prices{
		{Provider: "openai", Model: "m"}: {
			PromptPerToken:     chatIn,
			CompletionPerToken: chatOut,
			EndpointPrices: map[string]EndpointPrice{
				"embeddings": {PromptPerToken: &emb},
			},
		},
	}
	// Same model, chat endpoint: full chat legs.
	chat := prices.CostWithOptions("openai", "m", CostOptions{Endpoint: ""}, 1000, 100, 0, 0, 0)
	if chat != 1000*chatIn+100*chatOut {
		t.Fatalf("chat cost = %v, want %v", chat, 1000*chatIn+100*chatOut)
	}
	// Same model, embeddings endpoint: the override input leg,
	// completion leg falls back to the base leg (not overridden).
	embCost := prices.CostWithOptions("openai", "m", CostOptions{Endpoint: "embeddings"}, 1000, 0, 0, 0, 0)
	if embCost != 1000*emb {
		t.Fatalf("embeddings cost = %v, want %v", embCost, 1000*emb)
	}
	// An endpoint with no override prices at the base legs.
	resp := prices.CostWithOptions("openai", "m", CostOptions{Endpoint: "responses"}, 1000, 100, 0, 0, 0)
	if resp != chat {
		t.Fatalf("responses cost = %v, want chat cost %v", resp, chat)
	}
	// Cached-input leg still prices at the cached rate inside the
	// endpoint dimension: cached tokens at the cached rate, the
	// rest at the prompt rate.
	cachedRate := 0.00000125
	prices[(PriceKey{Provider: "openai", Model: "m"})] = ModelPrice{
		PromptPerToken:       chatIn,
		CompletionPerToken:   chatOut,
		CachedPromptPerToken: cachedRate,
		CachedKnown:          true,
	}
	got := prices.CostWithCached("openai", "m", 1000, 0, 400)
	want := 600*chatIn + 400*cachedRate
	if got != want {
		t.Fatalf("cached cost = %v, want %v", got, want)
	}
}

// Row 2 — gateway-reported cost precedence / reconciliation.
func TestG16ReportedCostWinsAndDeltaRecorded(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}
	reported := 0.05 // upstream billed $0.05; hand-computed would be 0.20
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true,
		Cost: &reported,
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if cost != reported {
		t.Fatalf("cost = %v, want reported %v", cost, reported)
	}
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	e := events[0]
	if e.ReportedCostUSD == nil || *e.ReportedCostUSD != reported {
		t.Fatalf("reported_cost = %v, want %v", e.ReportedCostUSD, reported)
	}
	// Delta = reported (0.05) − computed (0.20) = −0.15 (within
	// float64 rounding).
	if e.CostDeltaUSD < -0.150000000001 || e.CostDeltaUSD > -0.149999999999 {
		t.Fatalf("cost delta = %v, want -0.15", e.CostDeltaUSD)
	}
	if e.CostUSD != reported {
		t.Fatalf("cost_usd = %v, want %v", e.CostUSD, reported)
	}
}

func TestG16ReportedCostAbsentFallsBackToComputed(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true,
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if cost != 0.20 {
		t.Fatalf("cost = %v, want computed 0.20", cost)
	}
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if events[0].ReportedCostUSD != nil {
		t.Fatalf("reported_cost = %v, want nil", events[0].ReportedCostUSD)
	}
	if events[0].CostDeltaUSD != 0 {
		t.Fatalf("cost delta = %v, want 0", events[0].CostDeltaUSD)
	}
}

func TestG16InvalidReportedCostIgnored(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}
	neg := -1.0
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true,
		Cost: &neg,
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if cost != 0.20 {
		t.Fatalf("cost = %v, want computed 0.20 (negative reported cost ignored)", cost)
	}
}

// Row 3 — flat-rate / subscription $0 treatment.
func TestG16FlatRateAsZero(t *testing.T) {
	ctx := context.Background()
	prices := Prices{
		{Provider: "claude", Model: "claude-sonnet-4-5"}:    {PromptPerToken: 0.001, CompletionPerToken: 0.002},
		{Provider: "anthropic", Model: "claude-sonnet-4-5"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}
	u := func(provider string) Usage {
		return Usage{
			GatewayKeyID: "key-1", Provider: provider, Model: "claude-sonnet-4-5",
			PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true,
		}
	}
	// Opt-in off: flat-rate provider prices like any other.
	l := openTestLedger(t)
	if cost, err := l.RecordUsage(ctx, u("claude"), prices); err != nil || cost != 0.20 {
		t.Fatalf("opt-in off: cost = %v, err = %v, want 0.20", cost, err)
	}
	// Opt-in on: the flat-rate (Claude Code plan) provider costs $0.
	l.FlatRateAsZero = true
	if cost, err := l.RecordUsage(ctx, u("claude"), prices); err != nil || cost != 0 {
		t.Fatalf("opt-in on: cost = %v, err = %v, want 0", cost, err)
	}
	// Opt-in on: a non-flat-rate provider is unaffected.
	if cost, err := l.RecordUsage(ctx, u("anthropic"), prices); err != nil || cost != 0.20 {
		t.Fatalf("opt-in on, metered provider: cost = %v, err = %v, want 0.20", cost, err)
	}
	// The flat-rate $0 row round-trips through RecentEvents with
	// cost 0 (priced-as-$0, not unknown — price_known is set in
	// normalizeUsage; the event feed carries the $0).
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	var flatZero bool
	for _, e := range events {
		if e.Provider == "claude" && e.CostUSD == 0 {
			flatZero = true
		}
	}
	if !flatZero {
		t.Fatalf("no $0 claude row in events: %+v", events)
	}
}

func TestG16IsFlatRateProvider(t *testing.T) {
	flat := []string{"claude", "cc", "glm", "glm-cn", "kimi-coding", "kimi-coding-apikey",
		"minimax", "muse-code", "xiaomi-mimo", "bailian-coding-plan", "qwen-cloud-token-plan",
		"opencode-go", "chatgpt-web", "chatgpt-web-codex", "grok-web", "gemini-web",
		"perplexity-web", "claude-web", "  Claude  ",
		// G16 re-review catch: six WEB_COOKIE_PROVIDERS keys the
		// first pass missed, plus web-cookie aliases.
		"adobe-firefly", "hyperagent", "maxai", "syntx", "twinmind", "uc",
		"cnl", "copilot", "cpl", "cw", "db", "firefly", "ha", "in-ai",
		"lma", "m365copilot", "mx", "ntw", "nw", "pql", "stx", "t3chat",
		"tasw", "tcw", "tm", "ucn", "ven", "ybw", "zmf", "zw"}
	for _, id := range flat {
		if !IsFlatRateProvider(id) {
			t.Errorf("IsFlatRateProvider(%q) = false, want true", id)
		}
	}
	// the reference router's deliberate exclusions: metered providers that
	// share a family name with a flat-rate plan stay metered.
	// "poe" is also excluded on purpose: it is the id of the
	// metered Poe API provider as well as the alias of "poe-web".
	metered := []string{"", "codex", "cx", "byteplus", "minimax-cn", "glm-thinking", "anthropic", "openai", "poe"}
	for _, id := range metered {
		if IsFlatRateProvider(id) {
			t.Errorf("IsFlatRateProvider(%q) = true, want false", id)
		}
	}
}

// Row 4 — hedge loser not counted as a failure; billed refusal
// carries its reported cost.
func TestG16HedgeLoserNotCountedAsFailure(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}
	// One success, one hedge loser (cancelled), one genuine failure.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 10, CompletionTokens: 10, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("RecordUsage ok: %v", err)
	}
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		OK: false, LatencyMS: 5,
	}.AsCancelled(), prices); err != nil {
		t.Fatalf("RecordUsage cancelled: %v", err)
	}
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		OK: false, LatencyMS: 5,
	}, prices); err != nil {
		t.Fatalf("RecordUsage failed: %v", err)
	}
	sum, err := l.Summary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Requests != 3 || sum.OK != 1 || sum.Errors != 1 || sum.Cancelled != 1 {
		t.Fatalf("summary = requests %d ok %d errors %d cancelled %d, want 3/1/1/1",
			sum.Requests, sum.OK, sum.Errors, sum.Cancelled)
	}
	failed, err := l.RecentEvents(ctx, 10, LogFailed)
	if err != nil {
		t.Fatalf("RecentEvents failed: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("failed events = %d, want 1 (hedge loser excluded)", len(failed))
	}
	all, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents all: %v", err)
	}
	var cancelled int
	for _, e := range all {
		if e.Cancelled {
			cancelled++
		}
	}
	if cancelled != 1 {
		t.Fatalf("cancelled events = %d, want 1", cancelled)
	}
}

func TestG16BilledRefusalCarriesReportedCost(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}
	// A refusal the upstream billed for: ok=false, no usable
	// split, but a reported exact cost. The cost stands.
	reported := 0.004
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		TotalTokens: 40, OK: false, Cost: &reported,
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if cost != reported {
		t.Fatalf("cost = %v, want reported %v", cost, reported)
	}
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if events[0].CostDeltaUSD != reported {
		t.Fatalf("cost delta = %v, want %v (no computed cost to reconcile against)", events[0].CostDeltaUSD, reported)
	}
}

// Row 5 — retention / TTL on usage_events.
func TestG16RetentionPrunesExpired(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	now := time.Now()
	seedUsageRow(t, l, now.Add(-8*24*time.Hour), "key-1", "p1", "old-model", 1.0, 1, 1, 1, 0, 100)
	seedUsageRow(t, l, now.Add(-time.Hour), "key-1", "p1", "new-model", 2.0, 1, 1, 1, 0, 200)
	n, err := l.PruneExpiredUsage(ctx, now)
	if err != nil {
		t.Fatalf("PruneExpiredUsage: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1 (7-day default)", n)
	}
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 1 || events[0].Model != "new-model" {
		t.Fatalf("events = %+v, want only new-model", events)
	}
}

func TestG16RetentionWindowHonoured(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	now := time.Now()
	seedUsageRow(t, l, now.Add(-8*24*time.Hour), "key-1", "p1", "old-model", 1.0, 1, 1, 1, 0, 100)
	l.SetRetentionDays(30)
	n, err := l.PruneExpiredUsage(ctx, now)
	if err != nil {
		t.Fatalf("PruneExpiredUsage: %v", err)
	}
	if n != 0 {
		t.Fatalf("pruned = %d, want 0 (30-day window keeps an 8-day-old row)", n)
	}
	l.SetRetentionDays(0) // resets to the 7-day default
	n, err = l.PruneExpiredUsage(ctx, now)
	if err != nil {
		t.Fatalf("PruneExpiredUsage: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned after reset = %d, want 1", n)
	}
}

func TestG16StartupPrune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seedUsageRow(t, l, time.Now().Add(-8*24*time.Hour), "key-1", "p1", "old-model", 1.0, 1, 1, 1, 0, 100)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Reopen: startup cleanup prunes the expired row.
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	events, err := l2.RecentEvents(context.Background(), 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events after reopen = %d, want 0 (expired row pruned at startup)", len(events))
	}
}

// Row 6 — batch / flex service-tier pricing.
func TestG16ServiceTierPricing(t *testing.T) {
	prices := Prices{
		{Model: "m"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}
	base := prices.CostWithOptions("p", "m", CostOptions{}, 100, 50, 0, 0, 0)
	if base != 0.20 {
		t.Fatalf("standard cost = %v, want 0.20", base)
	}
	for _, tier := range []string{"flex", "batch", "Flex", " BATCH "} {
		got := prices.CostWithOptions("p", "m", CostOptions{ServiceTier: tier}, 100, 50, 0, 0, 0)
		if got != 0.10 {
			t.Fatalf("tier %q cost = %v, want 0.10 (0.5x)", tier, got)
		}
	}
	for _, tier := range []string{"", "standard", "priority", "auto", "unknown-tier"} {
		got := prices.CostWithOptions("p", "m", CostOptions{ServiceTier: tier}, 100, 50, 0, 0, 0)
		if got != 0.20 {
			t.Fatalf("tier %q cost = %v, want 0.20 (1x)", tier, got)
		}
	}
	// The tier threads from Usage through RecordUsage.
	ctx := context.Background()
	l := openTestLedger(t)
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p", Model: "m",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true,
		ServiceTier: "flex",
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if cost != 0.10 {
		t.Fatalf("flex RecordUsage cost = %v, want 0.10", cost)
	}
	events, err := l.RecentEvents(ctx, 10, LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if events[0].ServiceTier != "flex" {
		t.Fatalf("stored service_tier = %q, want flex", events[0].ServiceTier)
	}
	// A reported exact cost is never tier-discounted.
	reported := 0.30
	cost, err = l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p", Model: "m",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true,
		ServiceTier: "flex", Cost: &reported,
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage reported: %v", err)
	}
	if cost != reported {
		t.Fatalf("reported+flex cost = %v, want reported %v untouched", cost, reported)
	}
}

// Row 7 — session / project / client attribution, ledger half.
func TestG16AttributionRoundTripAndFilter(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Model: "m"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}
	rec := func(session, project, client string) {
		u := Usage{
			GatewayKeyID: "key-1", Provider: "p", Model: "m",
			PromptTokens: 10, CompletionTokens: 5, SplitKnown: true, OK: true,
		}.WithAttribution(session, project, client)
		if _, err := l.RecordUsage(ctx, u, prices); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}
	rec("sess-1", "proj-a", "client-x")
	rec("sess-1", "proj-a", "client-y")
	rec("sess-2", "proj-b", "client-x")
	bySession, err := l.RecentEventsByAttribution(ctx, AttributionFilter{SessionID: "sess-1"}, 10)
	if err != nil {
		t.Fatalf("RecentEventsByAttribution session: %v", err)
	}
	if len(bySession) != 2 {
		t.Fatalf("session sess-1 events = %d, want 2", len(bySession))
	}
	for _, e := range bySession {
		if e.SessionID != "sess-1" || e.ProjectID != "proj-a" {
			t.Fatalf("event attribution = %q/%q/%q, want sess-1/proj-a/*", e.SessionID, e.ProjectID, e.ClientID)
		}
	}
	byClient, err := l.RecentEventsByAttribution(ctx, AttributionFilter{ClientID: "client-x"}, 10)
	if err != nil {
		t.Fatalf("RecentEventsByAttribution client: %v", err)
	}
	if len(byClient) != 2 {
		t.Fatalf("client client-x events = %d, want 2", len(byClient))
	}
	byProject, err := l.RecentEventsByAttribution(ctx, AttributionFilter{ProjectID: "proj-b"}, 10)
	if err != nil {
		t.Fatalf("RecentEventsByAttribution project: %v", err)
	}
	if len(byProject) != 1 || byProject[0].SessionID != "sess-2" {
		t.Fatalf("project proj-b events = %+v, want 1 with sess-2", byProject)
	}
	byAll, err := l.RecentEventsByAttribution(ctx, AttributionFilter{SessionID: "sess-1", ProjectID: "proj-a", ClientID: "client-y"}, 10)
	if err != nil {
		t.Fatalf("RecentEventsByAttribution all: %v", err)
	}
	if len(byAll) != 1 {
		t.Fatalf("combined filter events = %d, want 1", len(byAll))
	}
}
