// Phase D, builder D4: quota-share engine use, 4-tier cascade and
// endpoint-specific combo projection.
//
// Tests drive the public interface (Engine.ChatWithChainStrategy /
// Embed / Rerank, plus the cascade.go helpers) with fake
// provider.Clients (the router_test.go harness). Every test asserts
// something ONLY the D4 wiring produces — DRR rotation carried
// across requests, cascade tier order from injected tier data, a
// fail-closed projection error on a non-chat endpoint — so each
// test FAILS with the wiring removed (verified: this file was run
// before the implementation existed and every test failed).
package router

import (
	"context"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func qsReq() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}
}

func qsChain(providers ...string) []Attempt {
	chain := make([]Attempt, len(providers))
	for i, p := range providers {
		chain[i] = Attempt{ProviderID: p, Model: "m"}
	}
	return chain
}

// TestQuotaShareDRRRotatesAcrossRequests: two members with skewed
// shares (a=2, b=1) served by one engine must rotate A,B,A,A,B,A
// over six sequential dispatches — the hand-computed DRR sequence
// of strategy.SelectQuotaShare for the same shares (see
// internal/strategy/quotashare_test.go TestQuotaShareDRRWeightedSequence).
// The second dispatch picks B only because the first dispatch's
// deficit survived: a stateless re-run (a fresh engine, same chain)
// picks A again, so this test FAILS if the DRR state is recreated
// per request.
func TestQuotaShareDRRRotatesAcrossRequests(t *testing.T) {
	a := okClient("from-a")
	b := okClient("from-b")
	cfg := testConfig()
	cfg.QuotaShareFor = func(providerID, model string) float64 {
		if providerID == "a" {
			return 2
		}
		return 1
	}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	want := []string{"from-a", "from-b", "from-a", "from-a", "from-b", "from-a"}
	for i, w := range want {
		resp, _, err := eng.ChatWithChainStrategy(context.Background(), qsChain("a", "b"), qsReq(), "quota-share")
		if err != nil {
			t.Fatalf("dispatch %d: ChatWithChainStrategy = %v", i+1, err)
		}
		if resp.Content != w {
			t.Fatalf("dispatch %d: served %q, want %q (DRR deficits must persist across requests)", i+1, resp.Content, w)
		}
	}
	// Stateless re-run: a fresh engine with the same chain and the
	// same shares picks A on its first dispatch — the rotation
	// above came from persisted state, not from input order.
	fresh := NewEngine(map[string]provider.Client{"a": okClient("from-a"), "b": okClient("from-b")}, nil, cfg)
	resp, _, err := fresh.ChatWithChainStrategy(context.Background(), qsChain("a", "b"), qsReq(), "quota-share")
	if err != nil {
		t.Fatalf("fresh engine: %v", err)
	}
	if resp.Content != "from-a" {
		t.Fatalf("fresh engine first dispatch = %q, want from-a", resp.Content)
	}
}

// TestQuotaShareEqualSharesAlternate: with no share source
// configured every member defaults to share 1 and DRR alternates
// A,B,A,B. This pins the nil-safe default (no operator shares
// configured yet) and the persistence across requests.
func TestQuotaShareEqualSharesAlternate(t *testing.T) {
	a := okClient("from-a")
	b := okClient("from-b")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	want := []string{"from-a", "from-b", "from-a", "from-b"}
	for i, w := range want {
		resp, _, err := eng.ChatWithChainStrategy(context.Background(), qsChain("a", "b"), qsReq(), "quota-share")
		if err != nil {
			t.Fatalf("dispatch %d: %v", i+1, err)
		}
		if resp.Content != w {
			t.Fatalf("dispatch %d: served %q, want %q", i+1, resp.Content, w)
		}
	}
}

// TestQuotaShareDRRMatchesReferenceSequence: the engine-side DRR
// (cascade.go quotaShareDRR) reproduces SelectQuotaShare's
// hand-computed 2:1 sequence and returns both deficits to exactly
// 0 after six rounds — the same invariant the strategy package's
// own test pins. A DRR that zeroed the winner or dropped the
// fractional carry would fail this.
func TestQuotaShareDRRMatchesReferenceSequence(t *testing.T) {
	chain := qsChain("a", "b")
	deficits := map[string]float64{}
	shareFn := func(providerID, model string) float64 {
		if providerID == "a" {
			return 2
		}
		return 1
	}
	want := []string{"a", "b", "a", "a", "b", "a"}
	for i, w := range want {
		ordered := quotaShareDRR(chain, deficits, shareFn)
		if ordered[0].ProviderID != w {
			t.Fatalf("round %d: winner = %q, want %q", i+1, ordered[0].ProviderID, w)
		}
		if len(ordered) != 2 || ordered[1].ProviderID == w {
			t.Fatalf("round %d: ordered = %v, want the winner first and the other member second", i+1, ordered)
		}
	}
	for id, d := range deficits {
		if d < -1e-9 || d > 1e-9 {
			t.Fatalf("deficit %s after 6 rounds = %v, want 0", id, d)
		}
	}
}

// TestCascadeOrderWithTierSource: with tier data injected, the
// cascade helper orders subscription -> api-key -> cheap -> free,
// keeps original order inside a tier, and puts members with no
// known tier LAST — never dropped, never assigned an invented tier.
func TestCascadeOrderWithTierSource(t *testing.T) {
	tiers := map[string]autorouter.CascadeTier{
		"free-p":    autorouter.CascadeFree,
		"sub-p":     autorouter.CascadeSubscription,
		"cheap-p":   autorouter.CascadeCheap,
		"api-p":     autorouter.CascadeAPIKey,
		"sub2-p":    autorouter.CascadeSubscription,
		"unknown-p": "",
	}
	chain := qsChain("free-p", "sub-p", "cheap-p", "api-p", "sub2-p", "unknown-p")
	ordered := applyCascadeOrder(chain, func(att Attempt) (autorouter.CascadeTier, bool) {
		tier, ok := tiers[att.ProviderID]
		if tier == "" {
			return "", false
		}
		return tier, ok
	})
	want := []string{"sub-p", "sub2-p", "api-p", "cheap-p", "free-p", "unknown-p"}
	if len(ordered) != len(want) {
		t.Fatalf("cascade order = %v, want %v (no member may be dropped)", providerIDs(ordered), want)
	}
	for i, w := range want {
		if ordered[i].ProviderID != w {
			t.Fatalf("cascade order = %v, want %v", providerIDs(ordered), want)
		}
	}
}

func providerIDs(chain []Attempt) []string {
	out := make([]string, len(chain))
	for i, att := range chain {
		out[i] = att.ProviderID
	}
	return out
}

// TestCascadeNoTierDataLeavesOrder: with no tier source at all the
// cascade helper returns the chain untouched. This pins the
// no-billing-data behaviour: an attempt is never assigned a
// billing tier from invented data.
func TestCascadeNoTierDataLeavesOrder(t *testing.T) {
	chain := qsChain("b", "a", "c")
	ordered := applyCascadeOrder(chain, nil)
	for i, att := range ordered {
		if att.ProviderID != chain[i].ProviderID {
			t.Fatalf("no-tier cascade order = %v, want %v (unchanged)", providerIDs(ordered), providerIDs(chain))
		}
	}
}

// TestEndpointProjectionEmbeddingsRejectsFusion: an embeddings
// request under the disallowed combo strategy "fusion" fails
// closed with the projection error and dials no provider.
// ProjectStrategyForEndpoint returns an ERROR for a disallowed
// strategy — it does not silently substitute another strategy —
// so the engine surfaces that error, never a guessed fallback.
func TestEndpointProjectionEmbeddingsRejectsFusion(t *testing.T) {
	a := &embedFakeClient{fakeClient: &fakeClient{}}
	b := &embedFakeClient{fakeClient: &fakeClient{}}
	cfg := testConfig()
	cfg.Strategy = "fusion"
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	_, err := eng.Embed(context.Background(), embedReq())
	if err == nil {
		t.Fatalf("Embed(fusion) = nil, want the projection error: strategy \"fusion\" is not supported for endpoint \"embeddings\"")
	}
	if !strings.Contains(err.Error(), `"fusion"`) || !strings.Contains(err.Error(), "embeddings") {
		t.Fatalf("Embed(fusion) error = %v, want it to name fusion and embeddings", err)
	}
	if a.embedCalls() != 0 || b.embedCalls() != 0 {
		t.Fatalf("embed calls a=%d b=%d, want 0 each (rejected before any dial)", a.embedCalls(), b.embedCalls())
	}
}

// TestEndpointProjectionEmbeddingsAllowsPriority: the same
// embeddings path under an allowed non-chat strategy ("priority")
// executes normally.
func TestEndpointProjectionEmbeddingsAllowsPriority(t *testing.T) {
	a := &embedFakeClient{fakeClient: &fakeClient{}}
	cfg := testConfig()
	cfg.Strategy = "priority"
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	resp, err := eng.Embed(context.Background(), embedReq())
	if err != nil {
		t.Fatalf("Embed(priority) = %v, want nil", err)
	}
	if resp == nil || len(resp.Data) == 0 {
		t.Fatalf("Embed(priority) = %+v, want an embedding response", resp)
	}
	if a.embedCalls() != 1 {
		t.Fatalf("embed calls = %d, want 1", a.embedCalls())
	}
}

// TestEndpointProjectionRerankRejectsPipeline: rerank under the
// disallowed combo strategy "pipeline" fails closed the same way,
// with no provider dialled.
func TestEndpointProjectionRerankRejectsPipeline(t *testing.T) {
	a := &embedFakeClient{fakeClient: &fakeClient{}}
	cfg := testConfig()
	cfg.Strategy = "pipeline"
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	_, err := eng.Rerank(context.Background(), rerankReq())
	if err == nil {
		t.Fatalf("Rerank(pipeline) = nil, want the projection error")
	}
	if !strings.Contains(err.Error(), `"pipeline"`) || !strings.Contains(err.Error(), "rerank") {
		t.Fatalf("Rerank(pipeline) error = %v, want it to name pipeline and rerank", err)
	}
	if a.rerankCalls() != 0 {
		t.Fatalf("rerank calls = %d, want 0 (rejected before any dial)", a.rerankCalls())
	}
}

// TestEndpointProjectionChatFusionNotProjected: a chat request
// (empty endpoint, which means chat) with strategy "fusion" is
// NOT projected — the combo path still fires and the judge picks
// the longer answer from b.
func TestEndpointProjectionChatFusionNotProjected(t *testing.T) {
	a := okClient("short")
	b := okClient("this is a much longer answer that the heuristic judge must pick")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), qsChain("a", "b"), qsReq(), "fusion")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(fusion, chat) = %v, want nil (chat allows every strategy)", err)
	}
	if resp.Content != "this is a much longer answer that the heuristic judge must pick" {
		t.Fatalf("chat fusion winner = %q, want the long answer from b (combo path must still fire)", resp.Content)
	}
}

// TestEndpointProjectionChatPathRejectsFusionOnEmbeddings:
// the chat entry point with an embeddings endpoint kind and
// strategy "fusion" is rejected the same way as the Embed path —
// the projection is decided by the request endpoint, not by
// which entry point the caller used.
func TestEndpointProjectionChatPathRejectsFusionOnEmbeddings(t *testing.T) {
	a := okClient("from-a")
	b := okClient("from-b")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	req := qsReq()
	req.Endpoint = provider.EndpointEmbeddings
	_, _, err := eng.ChatWithChainStrategy(context.Background(), qsChain("a", "b"), req, "fusion")
	if err == nil {
		t.Fatalf("ChatWithChainStrategy(fusion, endpoint=embeddings) = nil, want the projection error")
	}
	if !strings.Contains(err.Error(), `"fusion"`) || !strings.Contains(err.Error(), "embeddings") {
		t.Fatalf("error = %v, want it to name fusion and embeddings", err)
	}
	if a.chatCalls() != 0 || b.chatCalls() != 0 {
		t.Fatalf("chat calls a=%d b=%d, want 0 each (rejected before any dial)", a.chatCalls(), b.chatCalls())
	}
}

// TestEndpointProjectionModePackUntouched: engine mode-pack
// strategies ("balanced") are not combo strategies, so the combo
// endpoint rule has no opinion on them — an embeddings request
// under "balanced" still executes instead of erroring.
func TestEndpointProjectionModePackUntouched(t *testing.T) {
	a := &embedFakeClient{fakeClient: &fakeClient{}}
	cfg := testConfig()
	cfg.Strategy = "balanced"
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	if _, err := eng.Embed(context.Background(), embedReq()); err != nil {
		t.Fatalf("Embed(balanced) = %v, want nil (mode packs are not combo strategies)", err)
	}
	if a.embedCalls() != 1 {
		t.Fatalf("embed calls = %d, want 1", a.embedCalls())
	}
}

// TestEndpointProjectionUnknownKindErrors: an unknown endpoint kind
// fails closed for a combo strategy instead of silently collapsing
// to chat; a non-combo strategy still passes through untouched
// (the projection governs combo strategies only).
func TestEndpointProjectionUnknownKindErrors(t *testing.T) {
	if _, err := projectStrategyForEndpoint("fusion", provider.EndpointKind("bogus")); err == nil {
		t.Fatalf("projectStrategyForEndpoint(fusion, bogus) = nil, want an unknown-endpoint error")
	} else if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("projectStrategyForEndpoint(fusion, bogus) error = %v, want it to name the kind", err)
	}
	if got, err := projectStrategyForEndpoint("balanced", provider.EndpointKind("bogus")); err != nil || got != "balanced" {
		t.Fatalf("projectStrategyForEndpoint(balanced, bogus) = %q, %v, want balanced, nil", got, err)
	}
}
