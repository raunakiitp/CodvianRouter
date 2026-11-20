package plugins_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/plugins"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// flatPrices is a PriceLookup over a fixed model-level price table
// for tests: input $1 / output $2 per 1M tokens for "priced-model",
// nothing for any other model.
func flatPrices(model string) (float64, float64, bool) {
	if model == "priced-model" {
		return 1, 2, true
	}
	return 0, 0, false
}

func costCapReq(model, text string, maxTokens int) *plugins.Request {
	return &plugins.Request{Chat: &provider.ChatRequest{
		Model:     model,
		Messages:  []provider.Message{{Role: "user", Content: text}},
		MaxTokens: maxTokens,
	}}
}

// TestCostCapAllowUnderCap proves a cheap chat request passes
// through unchanged (no denial, no error, no mutation).
func TestCostCapAllowUnderCap(t *testing.T) {
	p, err := plugins.NewCostCapPlugin(0.01, flatPrices)
	if err != nil {
		t.Fatal(err)
	}
	req := costCapReq("priced-model", "hi", 0)
	deny, reason, err := p.(plugins.PreRequestPlugin).PreRequest(context.Background(), req)
	if err != nil || deny {
		t.Fatalf("deny=%v reason=%q err=%v, want allow", deny, reason, err)
	}
	if req.Chat.Model != "priced-model" || len(req.Chat.Messages) != 1 {
		t.Fatalf("request mutated: %+v", req.Chat)
	}
}

// TestCostCapDenyOverCap proves a request whose estimate exceeds
// the cap is denied with a reason naming the estimated cost and
// the cap — and that the registry surfaces it as a 402 with that
// message (the RefusalDetails contract the server writes).
func TestCostCapDenyOverCap(t *testing.T) {
	p, err := plugins.NewCostCapPlugin(0.001, flatPrices)
	if err != nil {
		t.Fatal(err)
	}
	// A long prompt: ~10 tokens a sentence x 200, so the prompt
	// alone costs ~2000 x $1/1M = $0.002, over the $0.001 cap
	// before completion cost is even added.
	long := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200)
	req := costCapReq("priced-model", long, 0)
	deny, reason, err := p.(plugins.PreRequestPlugin).PreRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v, want a denial, not an error", err)
	}
	if !deny {
		t.Fatalf("long request allowed, want denial (cap $0.001)")
	}
	if !strings.Contains(reason, "estimated cost $") || !strings.Contains(reason, "cap $0.001000") {
		t.Fatalf("reason = %q, want estimated cost + cap named", reason)
	}
	if !strings.Contains(reason, "assumed completion tokens") {
		t.Fatalf("reason = %q, want the completion assumption named", reason)
	}
	// The registry must carry the 402 + message to the server.
	reg := plugins.New()
	if err := reg.Register(p, true); err != nil {
		t.Fatal(err)
	}
	out := reg.PreRequest(context.Background(), req)
	if !out.Denied || out.Status != 402 || out.Detail != reason {
		t.Fatalf("outcome = %+v, want Denied with status 402 and the denial reason as detail", out)
	}
}

// TestCostCapMaxTokensRespected proves the estimate uses the
// request's own max_tokens when set: the same prompt is allowed
// with a small max_tokens and denied with a large one, at a cap
// between the two estimates.
func TestCostCapMaxTokensRespected(t *testing.T) {
	// Prompt "hi": ~7 tokens with framing, so prompt cost is
	// ~$0.000007 — negligible. Completion dominates: 10 tokens x
	// $2/1M = $0.00002 vs 10000 tokens x $2/1M = $0.02.
	p, err := plugins.NewCostCapPlugin(0.001, flatPrices)
	if err != nil {
		t.Fatal(err)
	}
	pre := p.(plugins.PreRequestPlugin)
	small := costCapReq("priced-model", "hi", 10)
	if deny, _, err := pre.PreRequest(context.Background(), small); err != nil || deny {
		t.Fatalf("max_tokens=10: deny=%v err=%v, want allow", deny, err)
	}
	large := costCapReq("priced-model", "hi", 10000)
	deny, reason, err := pre.PreRequest(context.Background(), large)
	if err != nil || !deny {
		t.Fatalf("max_tokens=10000: deny=%v err=%v, want denial", deny, err)
	}
	if !strings.Contains(reason, "10000 assumed completion tokens") {
		t.Fatalf("reason = %q, want the request's max_tokens named as the completion count", reason)
	}
}

// TestCostCapUnknownPriceFailsClosed proves a model with no
// complete model-level price is refused (402, naming the missing
// price) instead of being treated as free.
func TestCostCapUnknownPriceFailsClosed(t *testing.T) {
	p, err := plugins.NewCostCapPlugin(0.01, flatPrices)
	if err != nil {
		t.Fatal(err)
	}
	req := costCapReq("unpriced-model", "hi", 0)
	deny, reason, err := p.(plugins.PreRequestPlugin).PreRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v, want a denial naming the missing price", err)
	}
	if !deny || !strings.Contains(reason, "no complete model-level price") || !strings.Contains(reason, "unpriced-model") {
		t.Fatalf("deny=%v reason=%q, want denial naming the missing price", deny, reason)
	}
	reg := plugins.New()
	if err := reg.Register(p, true); err != nil {
		t.Fatal(err)
	}
	out := reg.PreRequest(context.Background(), req)
	if !out.Denied || out.Status != 402 {
		t.Fatalf("outcome = %+v, want Denied with status 402", out)
	}
}

// TestCostCapUnlimitedNeverDenies proves cap 0 (the default) is
// "off": even an enormous request is allowed, and the plugin still
// describes itself as unlimited.
func TestCostCapUnlimitedNeverDenies(t *testing.T) {
	p, err := plugins.NewCostCapPlugin(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Description(), "no cap configured") {
		t.Fatalf("description = %q, want the unlimited state named", p.Description())
	}
	long := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 2000)
	req := costCapReq("unpriced-model", long, 100000)
	deny, _, err := p.(plugins.PreRequestPlugin).PreRequest(context.Background(), req)
	if err != nil || deny {
		t.Fatalf("deny=%v err=%v, want allow with no cap", deny, err)
	}
}

// TestCostCapNonChatSurfacesPassThrough proves embeddings, rerank,
// responses and images requests are untouched: the plugin costs
// chat only, and never invents a chat-shaped estimate for a
// surface with a different cost shape.
func TestCostCapNonChatSurfacesPassThrough(t *testing.T) {
	p, err := plugins.NewCostCapPlugin(0.000001, flatPrices)
	if err != nil {
		t.Fatal(err)
	}
	pre := p.(plugins.PreRequestPlugin)
	for _, ep := range []provider.EndpointKind{
		provider.EndpointEmbeddings, provider.EndpointRerank,
		provider.EndpointResponses, provider.EndpointImages,
	} {
		req := costCapReq("priced-model", strings.Repeat("x ", 5000), 0)
		req.Chat.Endpoint = ep
		deny, _, err := pre.PreRequest(context.Background(), req)
		if err != nil || deny {
			t.Fatalf("endpoint %s: deny=%v err=%v, want pass-through", ep, deny, err)
		}
	}
	// Chat and messages surfaces are still costed.
	for _, ep := range []provider.EndpointKind{"", provider.EndpointChatCompletions, provider.EndpointMessages} {
		req := costCapReq("priced-model", strings.Repeat("x ", 5000), 0)
		req.Chat.Endpoint = ep
		deny, _, err := pre.PreRequest(context.Background(), req)
		if err != nil || !deny {
			t.Fatalf("endpoint %q: deny=%v err=%v, want denial", ep, deny, err)
		}
	}
}

// TestCostCapConstructorValidation proves the guards: a negative
// cap and a positive cap with no price lookup are both startup
// errors, not silent pass-throughs.
func TestCostCapConstructorValidation(t *testing.T) {
	if _, err := plugins.NewCostCapPlugin(-1, flatPrices); err == nil {
		t.Fatal("negative cap accepted")
	}
	if _, err := plugins.NewCostCapPlugin(0.01, nil); err == nil {
		t.Fatal("positive cap with no price lookup accepted")
	}
}

// TestCostCapEstimateMatchesCount proves the estimate is built
// from the real local token count: the denied reason's prompt
// token number equals CountPromptFull for the same request.
func TestCostCapEstimateMatchesCount(t *testing.T) {
	p, err := plugins.NewCostCapPlugin(0.000001, flatPrices)
	if err != nil {
		t.Fatal(err)
	}
	req := costCapReq("priced-model", "hello world, this is a cost-cap test", 0)
	count, err := tokencount.CountPromptFull("priced-model", *req.Chat)
	if err != nil {
		t.Fatal(err)
	}
	_, reason, err := p.(plugins.PreRequestPlugin).PreRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, fmt.Sprintf("%d prompt tokens", count.Tokens)) {
		t.Fatalf("reason = %q, want the real prompt-token count %d in it", reason, count.Tokens)
	}
}

// TestCostCapBuiltinLoadConfigured proves the configured loader
// wires the cap + lookup (the path main uses), while the bare
// loader keeps the unlimited default.
// TestCostCapUSDReportsCap proves the cap accessor the server
// feed reads: the configured cap for a capped plugin, 0 for the
// unlimited default — and Registry.CostCap finds it, while an
// empty registry reports "not registered".
func TestCostCapUSDReportsCap(t *testing.T) {
	p, err := plugins.NewCostCapPlugin(0.005, flatPrices)
	if err != nil {
		t.Fatal(err)
	}
	cc, ok := p.(plugins.CostCapper)
	if !ok {
		t.Fatal("cost-cap plugin does not implement CostCapper")
	}
	if got := cc.CostCapUSD(); got != 0.005 {
		t.Fatalf("CostCapUSD = %v, want 0.005", got)
	}
	reg := plugins.New()
	if _, present := reg.CostCap(); present {
		t.Fatal("empty registry reports a cost cap")
	}
	if err := reg.Register(p, true); err != nil {
		t.Fatal(err)
	}
	if capUSD, present := reg.CostCap(); !present || capUSD != 0.005 {
		t.Fatalf("CostCap = (%v, %v), want (0.005, true)", capUSD, present)
	}

	// Unlimited default: registered, cap 0.
	reg2 := plugins.New()
	unlimited, err := plugins.NewCostCapPlugin(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg2.Register(unlimited, true); err != nil {
		t.Fatal(err)
	}
	if capUSD, present := reg2.CostCap(); !present || capUSD != 0 {
		t.Fatalf("CostCap = (%v, %v), want (0, true) for the unlimited plugin", capUSD, present)
	}
	// A registry with only other plugins reports "not registered".
	reg3 := plugins.New()
	if err := plugins.LoadBuiltins(reg3, []string{plugins.HeaderAddName}); err != nil {
		t.Fatal(err)
	}
	if _, present := reg3.CostCap(); present {
		t.Fatal("registry without cost-cap reports a cost cap")
	}
}

func TestCostCapBuiltinLoadConfigured(t *testing.T) {
	reg := plugins.New()
	if err := plugins.LoadBuiltinsConfigured(reg, []string{plugins.CostCapName},
		plugins.BuiltinOptions{CostCapUSD: 0.001, CostCapPrices: flatPrices}); err != nil {
		t.Fatal(err)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len = %d, want 1", reg.Len())
	}
	long := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200)
	out := reg.PreRequest(context.Background(), costCapReq("priced-model", long, 0))
	if !out.Denied || out.Status != 402 {
		t.Fatalf("outcome = %+v, want Denied 402 from the configured load", out)
	}

	reg2 := plugins.New()
	if err := plugins.LoadBuiltins(reg2, []string{plugins.CostCapName}); err != nil {
		t.Fatal(err)
	}
	out2 := reg2.PreRequest(context.Background(), costCapReq("unpriced-model", long, 0))
	if out2.Denied || out2.Err != nil {
		t.Fatalf("bare-load outcome = %+v, want allow (unlimited default)", out2)
	}
}
