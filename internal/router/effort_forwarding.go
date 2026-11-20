// Intent-tier effort forwarding on non-OpenAI wires (CHECKLIST
// 285-286, Vertex 303-304).
//
// The 269-270 slice carried the tier-resolved effort onto the
// engine attempt, but per-wire forwarding was OpenAI-only: the
// OpenAI wire sends the named reasoning_effort, while Anthropic
// takes a numeric thinking.budget_tokens and Gemini a numeric
// thinkingConfig.thinkingBudget — and the engine invented no
// token budget for an effort level. This slice closes that gap
// with an OPERATOR-DECLARED effort→budget table (config
// `reasoning.effort_budgets`): the engine resolves the attempt's
// effective effort to a budget through the table and hands the
// numeric budget to the Anthropic/Gemini clients, which send it
// on the wire. CHECKLIST 303-304 extends the same table to
// Vertex, which uses the Gemini API shape: its client sends the
// budget as the same thinkingConfig.thinkingBudget, resolved
// from the table's own "vertex" entry (a "gemini" entry is never
// copied onto Vertex). With no table — or no entry for the
// effort — no thinking field is sent, and the trace records the
// gap honestly instead of filling it with a guessed number.
//
// The decision is per attempt, not per request: the same request
// can fail over from an Anthropic target (budget 1024) to a
// Gemini one (budget 2048), and each hop records its own
// forwarding. The label rides Attempt.EffortForwarding into the
// trace (see stampTraceEffortForwarding), and the numeric budget
// rides ChatRequest.ThinkingBudget into the provider client.
package router

import (
	"fmt"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// resolveEffortForwarding resolves one attempt's effective effort
// for one wire into the numeric thinking budget to send (0 =
// none) and the trace label saying what happened. lookup is the
// operator's effort→budget table (nil = no table declared); it is
// consulted only for the Anthropic/Gemini/Vertex wires, which are the
// only ones that take a numeric budget. The label is
// machine-stable — the dashboard renders it verbatim — and always
// names the wire's own field, so a forwarded value can be checked
// against what the upstream actually received.
//
//   - OpenAI / Ollama / unknown wires speak the OpenAI shape, so
//     the named effort goes out as reasoning_effort (the
//     openAIClient always sends it); no budget applies.
//   - Anthropic / Gemini / Vertex need a table entry: without one the
//     effort is not forwarded, and the label says why. Vertex
//     reads the table's own "vertex" entry and sends the budget
//     as thinkingConfig.thinkingBudget, the same field as Gemini.
//   - Cohere and Bedrock Converse have no effort control at all:
//     the label says so, whatever the table holds.
func resolveEffortForwarding(wire, effort string, lookup func(string, string) (int, bool)) (budget int, label string) {
	switch wire {
	case provider.WireFormatAnthropic:
		if lookup == nil {
			return 0, "not-forwarded:no-operator-budget-table"
		}
		budget, ok := lookup(wire, effort)
		if !ok {
			return 0, "not-forwarded:no-operator-budget-table"
		}
		return budget, fmt.Sprintf("forwarded:thinking.budget_tokens=%d", budget)
	case provider.WireFormatGemini, provider.WireFormatVertex:
		if lookup == nil {
			return 0, "not-forwarded:no-operator-budget-table"
		}
		budget, ok := lookup(wire, effort)
		if !ok {
			return 0, "not-forwarded:no-operator-budget-table"
		}
		return budget, fmt.Sprintf("forwarded:thinkingConfig.thinkingBudget=%d", budget)
	case provider.WireFormatCohere, provider.WireFormatBedrock:
		return 0, "not-forwarded:no-effort-control"
	case provider.WireFormatOpenAI, "ollama", provider.WireFormatUnknown:
		// The openAIClient is the fallback implementation for
		// every OpenAI-shaped wire (including "unknown", which
		// NewClient builds as an OpenAI client), and it always
		// sends reasoning_effort when one is set — so the named
		// effort does reach the upstream on these wires.
		return 0, fmt.Sprintf("forwarded:reasoning_effort=%s", effort)
	default:
		// The client reports no wire format (or an unknown one):
		// the engine cannot say what the wire did with the
		// effort, so it says so rather than guessing.
		return 0, "not-forwarded:unknown-wire"
	}
}

// withEffortForwarding resolves the attempt's effective effort
// (the request's ReasoningEffort after reqWithModel / the stream
// loop's inline build — the tier's pinned effort, a group entry's
// effort, or the client's own value, in that precedence) into the
// per-wire forwarding for this attempt. It returns the request
// copy carrying the resolved numeric budget (if any) and the
// attempt copy stamped with the forwarding label for the trace.
// An attempt with no effective effort returns both unchanged:
// there is nothing to forward, and the trace records nothing.
func (e *Engine) withEffortForwarding(req provider.ChatRequest, att Attempt, client provider.Client) (provider.ChatRequest, Attempt) {
	if req.ReasoningEffort == "" {
		return req, att
	}
	wire := ""
	if wf, ok := client.(wireFormatClient); ok {
		wire = wf.WireFormat()
	}
	budget, label := resolveEffortForwarding(wire, req.ReasoningEffort, e.cfg.EffortBudgetFor)
	if budget > 0 {
		req.ThinkingBudget = budget
	}
	att.EffortForwarding = label
	return req, att
}
