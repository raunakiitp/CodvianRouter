// B2 AUTO-FIX group test gaps (T-B2-AUTOFIX, 2026-11-20).
//
// V-B217-GROUP verified B2-17..B2-24 (1 flip :544, 7 stay PARTIAL) and
// named the exact missing tests for :534 (chaos dispatch-order) and
// :536 (quota-hinted thrifty) — both now covered by T-B2-ROUTER
// (commit 90e7ab5, NOT duplicated here). The remaining rows stay
// PARTIAL for want of dispatch-level proof of behaviour that is
// already live in the engine; these tests pin it:
//
//   - :535 subscription — no positive dispatch test existed (only
//     the empty-rung fail-closed test); the usable rung was never
//     shown serving through Engine.Chat.
//   - :538 best-* — only fail-open dispatch was pinned (smoke test);
//     the positive path (a vision/reasoning-capable model present)
//     was never shown serving through Engine.Chat.
//   - :539 pro-* — only fail-open dispatch was pinned
//     (TestAutoProCodingFailOpenPinned); the positive premium-filter
//     path was never shown serving through Engine.Chat.
//   - :542 cascade — the helper was tested with injected tiers, but
//     the live interception (orderAttemptsWithContext /
//     rankAttemptsWithContext with strategy "cascade" over the live
//     auto-catalog tiers) had no dispatch-level test.
//   - :543 endpoint combos — projection was tested for embeddings,
//     rerank and chat only; the six media/audio kinds (images,
//     search, video, music, OCR, audio) had no projection test.
//
// Every test drives the real engine (Engine.Chat /
// ChatWithChainStrategy / orderAttemptsWithContext /
// rankAttemptsWithContext) over fake provider clients and the live
// auto catalog — no mocks of the routing logic. Each test FAILS if
// the dispatch seam is removed (the AutoRoute stub serves
// "fallback-coder", and the fail-closed tests assert the resolver's
// own error text).
//
// What these tests do NOT close (production gaps, reported BLOCKED
// by this lane): :535 still has no live billing/subscription data
// source (by design); :538 still has no vision data / quality
// threshold in production; :539 still has no premium
// classification source in production (main.go only sets Tier
// "free"); :543 still has no audio provider client, so no audio
// call can succeed end to end.
package router

import (
	"context"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestB2SubscriptionServesRungZeroThroughChat (:535): with a usable
// subscription rung, auto/subscription serves the subscription model
// through Engine.Chat — rung 0 first, exactly one provider call, and
// never a billable fallback.
func TestB2SubscriptionServesRungZeroThroughChat(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	_, err := autoChat(t, eng, "auto/subscription", "What is two plus two")
	if err != nil {
		t.Fatalf("Chat(auto/subscription) error: %v", err)
	}
	if got := servedModel(t, fakes); got != "sub-model" {
		t.Fatalf("served model = %q, want sub-model (the usable subscription rung)", got)
	}
	if n := totalChats(fakes); n != 1 {
		t.Fatalf("upstream chats = %d, want 1 (only the subscription provider may be called)", n)
	}
	routing, ok := eng.LastRouting()
	if !ok {
		t.Fatal("LastRouting: no routing recorded")
	}
	if routing.Strategy != "balanced" {
		t.Errorf("routing strategy = %q, want balanced (subscription carries no weight bias)", routing.Strategy)
	}
	dec, ok := eng.LastAutoDecision()
	if !ok {
		t.Fatal("LastAutoDecision: no decision recorded for the subscription request")
	}
	if dec.Model != "sub-model" {
		t.Errorf("decision model = %q, want sub-model", dec.Model)
	}
	if strings.Contains(dec.Reason, "fail-open") {
		t.Errorf("decision reason = %q, must NOT record a fail-open (a usable subscription rung exists)", dec.Reason)
	}
}

// TestB2ProCodingServesPremiumPickThroughChat (:539): with premium
// tiers present in the catalog, auto/pro-coding serves the premium
// quality-first pick through Engine.Chat (the positive path — the
// fail-open path is pinned separately by
// TestAutoProCodingFailOpenPinned), and auto/pro-fast serves the
// fastest premium model. Both decisions must record the
// premium-tier filter.
func TestB2ProCodingServesPremiumPickThroughChat(t *testing.T) {
	cases := []struct {
		id, wantModel, wantStrategy string
	}{
		{"auto/pro-coding", "coder-best", "quality-first"},
		{"auto/pro-fast", "chat-fast", "ship-fast"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			eng, fakes := newAutoVariantTestEngine(t)
			installAutoCatalog(t, autoVariantCatalog())
			_, err := autoChat(t, eng, tc.id, "Write a Python function to reverse a string")
			if err != nil {
				t.Fatalf("Chat(%s) error: %v", tc.id, err)
			}
			if got := servedModel(t, fakes); got != tc.wantModel {
				t.Fatalf("served model = %q, want %q (premium-filter pick)", got, tc.wantModel)
			}
			routing, ok := eng.LastRouting()
			if !ok {
				t.Fatal("LastRouting: no routing recorded")
			}
			if routing.Strategy != tc.wantStrategy {
				t.Errorf("routing strategy = %q, want %q", routing.Strategy, tc.wantStrategy)
			}
			dec, ok := eng.LastAutoDecision()
			if !ok {
				t.Fatal("LastAutoDecision: no decision recorded")
			}
			if dec.Model != tc.wantModel {
				t.Errorf("decision model = %q, want %q", dec.Model, tc.wantModel)
			}
			if !strings.Contains(dec.Reason, "premium-tier") {
				t.Errorf("decision reason = %q, want it to record the premium-tier filter", dec.Reason)
			}
			if strings.Contains(dec.Reason, "fail-open") {
				t.Errorf("decision reason = %q, must NOT record a fail-open (premium models exist)", dec.Reason)
			}
		})
	}
}

// TestB2BestVisionAndReasoningServeCapableModelThroughChat (:538):
// with a vision-capable (resp. reasoning-capable) model in the
// catalog, auto/best-vision (resp. auto/best-reasoning) serves that
// model through Engine.Chat — the positive path the smoke test
// never covers (it only pins the fail-open with no capable model).
func TestB2BestVisionAndReasoningServeCapableModelThroughChat(t *testing.T) {
	cases := []struct {
		id, wantModel, wantStrategy string
		setCapable                  func(*autorouter.CatalogModel)
	}{
		{"auto/best-vision", "coder-best", "balanced", func(m *autorouter.CatalogModel) { m.Vision = true }},
		{"auto/best-reasoning", "coder-best", "quality-first", func(m *autorouter.CatalogModel) { m.Reasoning = true }},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			cat := autoVariantCatalog()
			for i := range cat {
				if cat[i].Model == "coder-best" {
					tc.setCapable(&cat[i])
				}
			}
			eng, fakes := newAutoVariantTestEngine(t)
			installAutoCatalog(t, cat)
			_, err := autoChat(t, eng, tc.id, "Write a Python function to reverse a string")
			if err != nil {
				t.Fatalf("Chat(%s) error: %v", tc.id, err)
			}
			if got := servedModel(t, fakes); got != tc.wantModel {
				t.Fatalf("served model = %q, want %q (the capable model)", got, tc.wantModel)
			}
			routing, ok := eng.LastRouting()
			if !ok {
				t.Fatal("LastRouting: no routing recorded")
			}
			if routing.Strategy != tc.wantStrategy {
				t.Errorf("routing strategy = %q, want %q", routing.Strategy, tc.wantStrategy)
			}
			dec, ok := eng.LastAutoDecision()
			if !ok {
				t.Fatal("LastAutoDecision: no decision recorded")
			}
			if dec.Model != tc.wantModel {
				t.Errorf("decision model = %q, want %q", dec.Model, tc.wantModel)
			}
			if strings.Contains(dec.Reason, "fail-open") {
				t.Errorf("decision reason = %q, must NOT record a fail-open (a capable model exists)", dec.Reason)
			}
		})
	}
}

// b2CascadeCatalog is a live auto catalog with exactly one member
// per cascade tier plus one keyless model (no cascade rung — it must
// sort last without an invented tier).
func b2CascadeCatalog() []autorouter.CatalogModel {
	return []autorouter.CatalogModel{
		{Provider: "provS", Model: "sub-model", Billing: "subscription", OverageHardStop: true, RemainingPercent: 80, HasRemaining: true},
		{Provider: "provA", Model: "api-model", Billing: "metered"},
		{Provider: "provC", Model: "cheap-model", Tier: "cheap"},
		{Provider: "provF", Model: "free-model", Tier: "free"},
		{Provider: "provK", Model: "keyless-model", Billing: "keyless"},
	}
}

func b2CascadeChain() []Attempt {
	return []Attempt{
		{ProviderID: "provC", Model: "cheap-model"},
		{ProviderID: "provF", Model: "free-model"},
		{ProviderID: "provK", Model: "keyless-model"},
		{ProviderID: "provA", Model: "api-model"},
		{ProviderID: "provS", Model: "sub-model"},
	}
}

// TestB2CascadeOrderAppliesAtDispatch (:542): the 4-tier cascade
// (Subscription → API Key → Cheap → Free, unplaced last) is applied
// by the live dispatch ordering — both the flat seam
// (orderAttemptsWithContext, strategy "cascade") and the tree seam
// (rankAttemptsWithContext) — over the live auto-catalog tiers, and
// a real ChatWithChainStrategy dispatch serves the subscription
// tier first. The control proves the reordering is the cascade
// interception: with strategy "balanced" the same chain keeps its
// input order (all quota hints are the 100 default, so the
// quota-hint sort is stable).
func TestB2CascadeOrderAppliesAtDispatch(t *testing.T) {
	want := []string{"provS", "provA", "provC", "provF", "provK"}
	eng := NewEngine(nil, nil, testConfig())
	installAutoCatalog(t, b2CascadeCatalog())

	ordered, zeroSkipped := eng.orderAttemptsWithContext(b2CascadeChain(), "cascade", "hard", false, StrategyHookContext{})
	if len(zeroSkipped) != 0 {
		t.Fatalf("flat cascade: zeroSkipped = %v, want none (no quota data)", providerIDs(zeroSkipped))
	}
	if got := providerIDs(ordered); len(got) != len(want) {
		t.Fatalf("flat cascade order = %v, want %v", got, want)
	} else {
		for i, w := range want {
			if got[i] != w {
				t.Fatalf("flat cascade order = %v, want %v", got, want)
			}
		}
	}

	ranked := eng.rankAttemptsWithContext("cascade", b2CascadeChain(), StrategyHookContext{})
	if got := providerIDs(ranked); len(got) != len(want) {
		t.Fatalf("tree cascade order = %v, want %v", got, want)
	} else {
		for i, w := range want {
			if got[i] != w {
				t.Fatalf("tree cascade order = %v, want %v", got, want)
			}
		}
	}

	control, _ := eng.orderAttemptsWithContext(b2CascadeChain(), "balanced", "hard", false, StrategyHookContext{})
	if got := providerIDs(control); len(got) != 5 || got[0] != "provC" || got[4] != "provS" {
		t.Fatalf("balanced control order = %v, want the input order [provC provF provK provA provS] (no cascade interception)", got)
	}

	// End to end: a real dispatch with strategy "cascade" serves the
	// subscription tier first.
	clients := map[string]provider.Client{
		"provS": okClient("from-sub"),
		"provA": okClient("from-api"),
		"provC": okClient("from-cheap"),
		"provF": okClient("from-free"),
		"provK": okClient("from-keyless"),
	}
	eng2 := NewEngine(clients, nil, testConfig())
	resp, _, err := eng2.ChatWithChainStrategy(context.Background(), b2CascadeChain(), qsReq(), "cascade")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(cascade) error: %v", err)
	}
	if resp.Content != "from-sub" {
		t.Fatalf("cascade dispatch served %q, want from-sub (subscription tier first)", resp.Content)
	}
}

// TestB2EndpointProjectionRejectsFusionOnMediaKinds (:543): the
// endpoint projection rejects a combo strategy ("fusion") on every
// non-chat endpoint kind — images, search, video, music, OCR and
// audio — at the live chat entry point, fail-closed with zero
// provider calls. The control proves the rejection is the
// projection: the same request under "priority" (a NonChatStrategy)
// serves normally.
func TestB2EndpointProjectionRejectsFusionOnMediaKinds(t *testing.T) {
	kinds := []struct {
		name string
		kind provider.EndpointKind
	}{
		{"images", provider.EndpointImages},
		{"search", provider.EndpointSearch},
		{"video", provider.EndpointVideo},
		{"music", provider.EndpointMusic},
		{"ocr", provider.EndpointOCR},
		{"audio-speech", EndpointAudioSpeech},
	}
	for _, kc := range kinds {
		t.Run(kc.name, func(t *testing.T) {
			a := okClient("from-a")
			b := okClient("from-b")
			eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
			req := qsReq()
			req.Endpoint = kc.kind
			_, _, err := eng.ChatWithChainStrategy(context.Background(), qsChain("a", "b"), req, "fusion")
			if err == nil {
				t.Fatalf("ChatWithChainStrategy(fusion, endpoint=%s) = nil, want the projection error", kc.name)
			}
			if !strings.Contains(err.Error(), `"fusion"`) {
				t.Fatalf("projection error = %v, want it to name fusion", err)
			}
			if a.chatCalls() != 0 || b.chatCalls() != 0 {
				t.Fatalf("chat calls a=%d b=%d, want 0 each (rejected before any dial)", a.chatCalls(), b.chatCalls())
			}
			// Control: the same endpoint under "priority" serves.
			resp, _, err := eng.ChatWithChainStrategy(context.Background(), qsChain("a", "b"), req, "priority")
			if err != nil {
				t.Fatalf("ChatWithChainStrategy(priority, endpoint=%s) = %v, want nil (priority stands on media endpoints)", kc.name, err)
			}
			if resp.Content != "from-a" {
				t.Fatalf("priority control served %q, want from-a", resp.Content)
			}
		})
	}
}
