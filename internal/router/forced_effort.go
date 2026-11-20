// Forced-reasoning policy (CHECKLIST 311-312).
//
// An operator-declared policy forces a reasoning effort on a
// model or route — e.g. always-high for a coding tier — whatever
// the client asked for. The rules live in config
// (`reasoning.forced_effort`, see internal/config/forced_effort.go)
// and reach the engine as Config.ForcedEffortFor.
//
// Precedence — policy > tier > group entry > client, unless the
// policy allows override:
//
//   - A policy matched on the requested model (as the client
//     named it, e.g. "auto/coding") or the resolved model
//     applies to EVERY attempt of the request, including failover
//     hops: the whole request is that route.
//   - A policy matching one attempt's own model applies to that
//     attempt only, when no request-level policy matched.
//   - A matched policy beats the intent-tier effort and a
//     routing-group entry's effort on that attempt.
//   - A client-supplied reasoning_effort wins ONLY when the
//     matched policy sets allow_client_override; then the client
//     value is the effective effort (it beats the policy, the
//     tier and the group entry). With no client value, an
//     override-allowed policy still forces its effort.
//
// The decision is per request, computed once at the server-facing
// entry points (Chat/ChatStream*), where the requested model and
// the client's own effort are still visible: withForcedEffort.
// The request-level decision rides the request (req.EffortPolicy
// + the effective effort on req.ReasoningEffort) so the
// chain-level functions can re-apply it after the chain is
// rebuilt: a group-resolved chain replaces the chain the entry
// points stamped, so ChatWithChainStrategy and
// ChatStreamWithChainStrategy re-stamp it with
// applyRequestForcedEffort. A policy naming one group member only
// does not single that member out inside a group chain — for a
// group route, declare the policy on the requested or resolved
// model (or the group alias). Direct ChatWithChainStrategy
// callers (tests only, in production) bypass the entry points
// and get no policy application — the same boundary the
// intent-tier effort has (CHECKLIST 269-270).
//
// The trace records which won per attempt (Attempt.EffortPolicy,
// stamped into the trace by stampTraceEffortPolicy): the label
// is machine-stable — "forced:<effort>:policy:<model>" or
// "client-override:<effort>:policy:<model>" — and the dashboard
// renders it verbatim.
package router

import (
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ForcedEffort is the engine-side view of one operator-declared
// forced-reasoning rule (CHECKLIST 311-312): the model the rule
// matched, the effort it forces, and whether a client value may
// override it. Adapted from config.ForcedEffortPolicy by the
// wiring in cmd/codvian-router (main), so the engine never
// imports the config package.
type ForcedEffort struct {
	Model               string
	Effort              string
	AllowClientOverride bool
}

// forcedEffortForModel returns the rule matching one model, or
// false when no rule is declared for it (or no rules are
// configured at all). The lookup must be cheap and non-blocking:
// it runs on the request path.
func (e *Engine) forcedEffortForModel(model string) (ForcedEffort, bool) {
	if e.cfg.ForcedEffortFor == nil || model == "" {
		return ForcedEffort{}, false
	}
	return e.cfg.ForcedEffortFor(model)
}

// resolveForcedWinner decides the effective effort for one
// matched policy: the client's value wins only when the policy
// allows override AND the client sent one; otherwise the policy
// wins. It returns the effective effort and the trace label
// naming the winner. Both values are normalized, so "Low" from a
// client and "High" from a rule mean the same entries as "low"
// and "high".
func resolveForcedWinner(pol ForcedEffort, clientEffort string) (effort, label string) {
	clientEffort = provider.NormalizeReasoningEffort(clientEffort)
	if pol.AllowClientOverride && clientEffort != "" {
		return clientEffort, "client-override:" + clientEffort + ":policy:" + pol.Model
	}
	eff := provider.NormalizeReasoningEffort(pol.Effort)
	return eff, "forced:" + eff + ":policy:" + pol.Model
}

// withForcedEffort applies the forced-reasoning policy to one
// request, at the server-facing entry points. requestedModel is
// the model as the client named it (before auto-resolution);
// clientEffort is the client's own reasoning_effort before any
// tier stamping. The request-level policy — matched on the
// requested model first, then the resolved model — stamps every
// attempt of the request and rides the returned request
// (ReasoningEffort = effective effort, EffortPolicy = the label),
// so the chain functions can re-apply it after a group rebuild.
// When no request-level policy matched, a policy matching one
// attempt's own model stamps that attempt only. The input chain
// is never mutated: the stamped copy is returned. A request with
// no matching policy returns its chain and request unchanged.
func (e *Engine) withForcedEffort(chain []Attempt, req provider.ChatRequest, requestedModel, clientEffort string) ([]Attempt, provider.ChatRequest) {
	// The request's EffortPolicy is engine-stamped, never
	// client-supplied: any value on the incoming request is
	// dropped before a decision is made, so a forged label can
	// never reach the trace (the field is json:"-" too, so it
	// cannot arrive over HTTP at all — this is the second lock).
	// Cleared before the no-rules return below, so the drop
	// holds even when no policy is configured.
	req.EffortPolicy = ""
	if e.cfg.ForcedEffortFor == nil {
		return chain, req
	}
	reqPol, reqOK := e.forcedEffortForModel(requestedModel)
	if !reqOK {
		reqPol, reqOK = e.forcedEffortForModel(req.Model)
	}
	var reqEffort, reqLabel string
	if reqOK {
		reqEffort, reqLabel = resolveForcedWinner(reqPol, clientEffort)
	}
	out := make([]Attempt, len(chain))
	copy(out, chain)
	for i := range out {
		if reqOK {
			out[i].Effort = reqEffort
			out[i].EffortPolicy = reqLabel
			continue
		}
		if pol, ok := e.forcedEffortForModel(out[i].Model); ok {
			eff, label := resolveForcedWinner(pol, clientEffort)
			out[i].Effort = eff
			out[i].EffortPolicy = label
		}
	}
	if reqOK {
		req.ReasoningEffort = reqEffort
		req.EffortPolicy = reqLabel
	}
	return out, req
}

// applyRequestForcedEffort re-stamps the request-level
// forced-reasoning decision onto a chain built after the entry
// points (CHECKLIST 311-312): a group-resolved chain replaces
// the chain the entry points stamped, so without this a policy
// matched on the requested or resolved model would silently not
// hold on a group-addressed request. The decision rides the
// request (req.EffortPolicy, set by withForcedEffort); the
// effective effort rides req.ReasoningEffort. The input chain is
// never mutated: the stamped copy is returned. A request with no
// request-level decision returns its chain unchanged.
func applyRequestForcedEffort(chain []Attempt, req provider.ChatRequest) []Attempt {
	if req.EffortPolicy == "" {
		return chain
	}
	out := make([]Attempt, len(chain))
	copy(out, chain)
	for i := range out {
		out[i].Effort = req.ReasoningEffort
		out[i].EffortPolicy = req.EffortPolicy
	}
	return out
}
