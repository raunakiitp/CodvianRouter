package plugins

import (
	"context"
	"fmt"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// ---------------------------------------------------------------------------
// Cost-cap plugin (CHECKLIST 239-240): the first real built-in
// adapter plugin — a genuine guardrail through the plugin system,
// not a stub and not a demo of the mechanism.
//
// It refuses a chat request BEFORE any provider is dialled when the
// request's estimated cost exceeds a configured per-request cap, so
// an over-budget request costs exactly $0 and makes zero provider
// calls. The refusal is HTTP 402 naming the estimated cost and the
// cap (see RefusalDetails), the same status the per-request budget
// header uses for the same reason.
//
// The estimate is honest about what it is:
//
//   - Prompt tokens: the local tiktoken estimate
//     (tokencount.CountPromptFull — message text + tool definitions
//     + OpenAI-rule framing, so it overcounts a native-wire request
//     by a few tokens: the safe direction for a fail-closed gate).
//     The estimate is labelled an estimate wherever it is shown:
//     the provider's own count is what is eventually billed, and
//     the local count can drift from it (see docs/token-count-audit.md).
//   - Completion tokens: the request's own max_tokens when set,
//     else DefaultAssumedCompletionTokens. A request that does not
//     say how much it may generate is costed as if it generated
//     that many tokens — the assumption is a documented constant,
//     not a measurement, and the denial names it.
//   - Prices: the MODEL-LEVEL price for the model (input and
//     output USD per 1M tokens). The plugin runs before routing,
//     so it cannot know which provider will serve the request and
//     cannot use a per-(provider, model) pair price; the
//     model-level price is the price that applies to every
//     provider serving the model. A pair price for the model —
//     however many providers carry one — never satisfies the
//     cap: an operator who priced only pairs must add a
//     model-level price for the model (CHECKLIST 319; pinned by
//     TestPluginsCostCapPairPriceOnlyStillRefused in the server
//     tests, which builds the lookup from a real price book the
//     way main does). A model with no complete
//     model-level price (either side missing) is UNKNOWN, never
//     free: the request is refused 402 naming the missing price,
//     exactly like the key-budget fail-closed rule for unpriced
//     models. Configure the price in the config "prices" block.
//
// Scope: chat surfaces only (POST /v1/chat/completions and POST
// /v1/messages). Embeddings, rerank, images and Responses requests
// carry a different cost shape (no completion, or a different
// price unit) and pass through untouched — the plugin never
// invents a chat-shaped estimate for them. A request the plugin
// cannot cost (nil request, token-count failure) fails closed
// with an error, never allowed through uncosted.
//
// Cap: configured per plugin via plugins.cost_cap_usd_per_request
// in the config file. 0 (or absent) means unlimited: the plugin
// still registers, still counts its calls in the feed, and never
// denies — a cap of 0 is "off", not "free".
// ---------------------------------------------------------------------------

// CostCapName is the built-in cost-cap plugin's name.
const CostCapName = "cost-cap"

// DefaultAssumedCompletionTokens is the completion-token count a
// request is costed at when it sets no max_tokens. It is a
// documented assumption, not a measurement: the denial message
// names it ("assumed completion tokens") so nobody mistakes the
// estimate for a bill.
const DefaultAssumedCompletionTokens = 1024

// PriceLookup returns the model-level price for a model: input and
// output USD per 1M tokens. ok=false means the model has no
// complete model-level price (unknown, never free). Either price
// may legitimately be 0 (a genuinely free direction); only the
// pair's presence is what ok reports.
type PriceLookup func(model string) (inputUSDPer1M, outputUSDPer1M float64, ok bool)

type costCapPlugin struct {
	capUSD float64
	lookup PriceLookup
}

// NewCostCapPlugin builds the cost-cap plugin. A negative cap is a
// construction error (a cap is a non-negative amount of money). A
// positive cap with no price lookup is also an error: with prices
// unknowable every request would fail closed as unpriced, so the
// misconfiguration must fail at startup, not one denied request
// at a time. An unlimited cap (0) needs no lookup.
func NewCostCapPlugin(capUSD float64, lookup PriceLookup) (Plugin, error) {
	if capUSD < 0 {
		return nil, fmt.Errorf("plugins: cost-cap needs a non-negative cap, got %v", capUSD)
	}
	if capUSD > 0 && lookup == nil {
		return nil, fmt.Errorf("plugins: cost-cap with a cap needs a price lookup")
	}
	return &costCapPlugin{capUSD: capUSD, lookup: lookup}, nil
}

func (p *costCapPlugin) Name() string { return CostCapName }

// CostCapUSD reports the configured per-request cap in USD (0 =
// unlimited). The server feed uses it to say which cap the
// unpriced-model list applies to — the dashboard never parses the
// cap out of the description string.
func (p *costCapPlugin) CostCapUSD() float64 { return p.capUSD }

func (p *costCapPlugin) Description() string {
	if p.capUSD <= 0 {
		return "Refuses chat requests whose estimated cost exceeds the per-request cap — no cap configured, so it counts calls and never denies"
	}
	return fmt.Sprintf("Refuses chat requests whose estimated cost exceeds $%.6f per request (local tiktoken prompt count + assumed completion tokens, model-level prices)", p.capUSD)
}

func (p *costCapPlugin) Hooks() []HookKind { return []HookKind{HookPreRequest} }

// RefusalStatus is 402: the request is refused for cost, the same
// status the per-request budget header uses.
func (p *costCapPlugin) RefusalStatus() int { return 402 }

// RefusalMessage returns the denial reason verbatim. The reason is
// built by PreRequest from numbers and the model name only — never
// prompt or response content — so it is safe to echo to the client.
func (p *costCapPlugin) RefusalMessage(reason string) string { return reason }

// PreRequest estimates the request's cost and denies it when the
// estimate exceeds the cap. It never mutates the request.
func (p *costCapPlugin) PreRequest(_ context.Context, req *Request) (bool, string, error) {
	if req == nil || req.Chat == nil {
		return false, "", fmt.Errorf("plugins: cost-cap received a nil request")
	}
	// Chat surfaces only: any other endpoint has a different cost
	// shape and passes through untouched (see the package doc).
	ep := req.Chat.Endpoint
	if ep != "" && ep != provider.EndpointChatCompletions && ep != provider.EndpointMessages {
		return false, "", nil
	}
	// Unlimited cap: count the call, never deny.
	if p.capUSD <= 0 {
		return false, "", nil
	}
	model := req.Chat.Model
	count, err := tokencount.CountPromptFull(model, *req.Chat)
	if err != nil {
		return false, "", fmt.Errorf("plugins: cost-cap cannot count prompt tokens for model %q: %w", model, err)
	}
	inputPer1M, outputPer1M, ok := p.lookup(model)
	if !ok {
		// A denial, not an error: the registry answers errors
		// with the generic 403, but this refusal has something
		// specific and safe to say — the model is unpriced, so
		// the cost cannot be verified against the cap.
		return true, fmt.Sprintf("cost_cap: no complete model-level price for model %q (input and output USD per 1M tokens both required) — cannot verify cost against cap $%.6f; add a model-level price in the config \"prices\" block", model, p.capUSD), nil
	}
	completionTokens := req.Chat.MaxTokens
	if completionTokens <= 0 {
		completionTokens = DefaultAssumedCompletionTokens
	}
	estimated := float64(count.Tokens)*inputPer1M/1e6 + float64(completionTokens)*outputPer1M/1e6
	if estimated <= p.capUSD {
		return false, "", nil
	}
	return true, fmt.Sprintf("cost_cap: estimated cost $%.6f exceeds per-request cap $%.6f (model %q, %d prompt tokens + %d assumed completion tokens)", estimated, p.capUSD, model, count.Tokens, completionTokens), nil
}
