// M9.3 — routing/combo tools.
//
// Shape studied from the reference router for reference only (never copied):
// MCP-SERVER.md: list_combos [read:combos],
// get_combo_metrics [read:combos], switch_combo
// [write:combos], create_combo [write:combos],
// simulate_route [read:health, read:combos],
// set_routing_strategy [write:combos],
// best_combo_for_task [read:combos, read:health].
// Our scopes are our own M9.1 set (see the scope choice below).
//
// SCOPE CHOICE (option (b) — the task's option b): the 9-scope set
// had mcp:manage:routing and mcp:manage:cost but NO read scopes
// for routing/cost, so under option (a) a read-only key could never
// use explain_route, simulate_route, list_combos, cost_report or
// check_quota — the read tools would demand a manage grant, which
// also grants mutation. That defeats the point of the scope system
// (M9.1: a key narrowed to exactly one tool). So this slice ADDS
// mcp:read:routing and mcp:read:cost as additive constants in
// scopes.go (constants + AllScopes entries only — no change to
// matching/enforcement logic, which stays the M9.1 wildcard/AND
// rules). Read tools take the read scope; mutating tools take the
// manage scope. The scope set is now 11, not 9.
//
// HONESTY RULES:
//   - Combos ARE the config routing groups (see Source in mcp.go):
//     list/create read and write Routing.Groups, with the
//     ChainStrategies mirror for the combo's strategy. No combo
//     state lives anywhere else.
//   - switch_combos sets the MCP server's session-active combo
//     (Source.ActiveCombo). The serving router has NO single active
//     combo — every group is always addressable — so this never
//     changes what the serving router routes; it selects the
//     default combo list_combos reports and explain/simulate
//     default to. The tool result says so.
//   - explain_route / simulate_route are DRY RUNS: they order
//     providers from the live registry + the effective strategy
//     and price the prompt from the real price book. They make no
//     provider call, and the order shown is the declared
//     (priority) order: strategies that re-rank (cheapest,
//     fastest, ...) do so at request time from live latency /
//     success signals this process does not observe, so the
//     result carries a note saying exactly that instead of
//     pretending the shown order is the request-time order.
//   - best_combo_for_task ranks ONLY on measured ledger metrics
//     (success rate, cost per request, mean latency) and the
//     combo's strategy; a combo with no history ranks on strategy
//     match alone and the result says its metrics are unknown.
//     A cost/latency constraint a combo cannot be checked against
//     (unknown price, no latency) does NOT exclude the combo —
//     it is reported as not-applied for that combo, never silently
//     treated as $0 or 0 ms.
//   - pick_fastest_model ranks ONLY on measured mean latency from
//     recent ledger events. With no measured latency it errors
//     honestly instead of guessing from prices or catalog order.
//     When the latency backend itself is unavailable — the Source
//     reports ModelStatsAvailable() false (the HTTP Source with no
//     ledger wired; the stdio registry Source when no data dir is
//     configured or the ledger cannot be opened) — the error says
//     the backend is unavailable, never "no measured model latency
//     yet": an empty stats list cannot tell "no events yet" from
//     "no backend" (N4). A Source that does not implement the
//     optional interface is treated as available, so such a Source
//     (e.g. the plain test fake) keeps the old message.
//   - Error codes: an invalid input (unknown combo/model/
//     strategy, bad types, out-of-range numbers) is refused with
//     -32602 — the dispatcher maps every handler error to -32602
//     (see mcp.go callTool), and the handler returns the
//     validation message. A backing-store failure (ledger
//     unavailable) is ALSO -32602 by that dispatcher rule, but
//     the message is prefixed "unavailable: " so the caller can
//     tell a bad input from a down store. That collapse is a
//     pre-existing dispatcher wart, not something this slice
//     changes (M9.5 protocol work owns error-code passthrough).
package mcp

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ErrInvalidInput marks a refused input: the caller asked for
// something that does not exist or is not well-formed. Source
// implementations wrap it (fmt.Errorf("%w: ...")) so handlers can
// tell it apart from a backing-store failure with errors.Is.
var ErrInvalidInput = errors.New("mcp: invalid input")

// ComboInfo is one named routing combo: a config routing group
// (members are provider ids, optionally "provider@effort"),
// its strategy (from routing.chain_strategies, "priority" when
// none is configured), whether it is the session-active combo,
// and whether every member can serve today (provider exists,
// enabled, has a key or is a no-auth local).
type ComboInfo struct {
	Name           string   `json:"name"`
	Members        []string `json:"members"`
	Strategy       string   `json:"strategy"`
	Active         bool     `json:"active"`
	Usable         bool     `json:"usable"`
	UnusableReason string   `json:"unusable_reason,omitempty"`
}

// ComboMetrics is the measured ledger history for one combo's
// member providers (all time, from usage_events). SuccessRate is
// nil when the combo has no requests — never 0 by default.
// CostUSD sums cost_usd over priced rows only; CostKnown is false
// when any of the combo's tokens are unpriced (UnpricedTokens /
// UnpricedModels), and then CostUSD is a lower bound, never the
// whole cost. MeanLatencyMS is the mean over recent successful
// events only (LatencyEvents of them, at most the 500 most recent
// events — older events do not affect the mean, while
// Requests/tokens/cost are all-time); LatencyKnown is false when
// there are none.
type ComboMetrics struct {
	Name             string   `json:"name"`
	Requests         int      `json:"requests"`
	OK               int      `json:"ok"`
	Errors           int      `json:"errors"`
	SuccessRate      *float64 `json:"success_rate,omitempty"`
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	CostUSD          float64  `json:"cost_usd"`
	CostKnown        bool     `json:"cost_known"`
	UnpricedTokens   int      `json:"unpriced_tokens"`
	UnpricedModels   []string `json:"unpriced_models,omitempty"`
	MeanLatencyMS    float64  `json:"mean_latency_ms"`
	LatencyKnown     bool     `json:"latency_known"`
	LatencyEvents    int      `json:"latency_events"`
	Window           string   `json:"window"`
}

// StrategyInfo is one effective strategy: its canonical name, the
// one-line rule (from strategy.Describe in production), whether it
// came from the global setting or a per-chain override, and which
// chain ("" for global). Inherited is true when the chain has no
// override and falls back to the global strategy.
type StrategyInfo struct {
	Name      string `json:"name"`
	Rule      string `json:"rule"`
	Scope     string `json:"scope"`
	Chain     string `json:"chain,omitempty"`
	Inherited bool   `json:"inherited"`
}

// RouteAttempt is one target in an explain_route order: the
// provider, the requested model, whether the provider is enabled,
// and whether the (provider, model) price is known — with the
// prompt rate per 1M tokens when it is (nil when unknown, never
// a guessed rate).
type RouteAttempt struct {
	Provider       string   `json:"provider"`
	ProviderName   string   `json:"provider_name"`
	Model          string   `json:"model"`
	Enabled        bool     `json:"enabled"`
	PriceKnown     bool     `json:"price_known"`
	PromptUSDPer1M *float64 `json:"prompt_usd_per_1m,omitempty"`
}

// RouteExplanation is the explain_route result: the model, the
// effective strategy and its rule, the attempt order (declared
// order — see the Note), and the combo the order was restricted
// to, if any.
type RouteExplanation struct {
	Model    string         `json:"model"`
	Strategy string         `json:"strategy"`
	Rule     string         `json:"rule"`
	Combo    string         `json:"combo,omitempty"`
	Attempts []RouteAttempt `json:"attempts"`
	Note     string         `json:"note"`
}

// SimAttempt is one step of a simulated route: the provider, the
// model, what happens when it fails (fail over / stop), and the
// estimated prompt cost for this step when its price is known.
type SimAttempt struct {
	Provider               string   `json:"provider"`
	Model                  string   `json:"model"`
	OnFailure              string   `json:"on_failure"`
	EstimatedPromptCostUSD *float64 `json:"estimated_prompt_cost_usd,omitempty"`
	PriceKnown             bool     `json:"price_known"`
}

// RouteSimulation is the simulate_route result: the model, the
// effective strategy, the fallback tree (one attempt per target,
// in order), the prompt-token count the estimate was priced at,
// and the first-attempt prompt-cost estimate — nil + CostKnown
// false when the first target's price is unknown. Completion cost
// is never estimated: the completion length is unknown before the
// call, so any number would be invented.
type RouteSimulation struct {
	Model                  string       `json:"model"`
	Strategy               string       `json:"strategy"`
	Combo                  string       `json:"combo,omitempty"`
	PromptTokens           int          `json:"prompt_tokens"`
	Attempts               []SimAttempt `json:"attempts"`
	EstimatedPromptCostUSD *float64     `json:"estimated_prompt_cost_usd,omitempty"`
	CostKnown              bool         `json:"cost_known"`
	Note                   string       `json:"note"`
}

// ModelStat is one (provider, model) cell of measured history
// from at most the 500 most recent ledger events: requests,
// successes, mean latency over successful events with a recorded
// latency. With more than 500 events recorded, older events are
// dropped — the counts are the last 500, not all-time.
// SuccessRate is nil with no requests; LatencyKnown is false
// with no such event.
type ModelStat struct {
	Provider      string   `json:"provider"`
	Model         string   `json:"model"`
	Requests      int      `json:"requests"`
	OK            int      `json:"ok"`
	SuccessRate   *float64 `json:"success_rate,omitempty"`
	MeanLatencyMS float64  `json:"mean_latency_ms"`
	LatencyKnown  bool     `json:"latency_known"`
	LatencyEvents int      `json:"latency_events"`
}

// --- argument helpers -------------------------------------------------
//
// All helpers return a message for the -32602 reply on failure.
// JSON numbers arrive as float64; a non-integral float, a string
// where a number belongs, or a wrong-typed value is invalid input,
// never coerced.

func reqStringArg(args map[string]any, key string) (string, string) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", fmt.Sprintf("%s: required", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Sprintf("%s: must be a string", key)
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Sprintf("%s: must not be empty", key)
	}
	return strings.TrimSpace(s), ""
}

func optStringArg(args map[string]any, key string) (string, string) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", ""
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Sprintf("%s: must be a string", key)
	}
	return strings.TrimSpace(s), ""
}

func reqStringListArg(args map[string]any, key string) ([]string, string) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, fmt.Sprintf("%s: required", key)
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Sprintf("%s: must be an array of strings", key)
	}
	if len(arr) == 0 {
		return nil, fmt.Sprintf("%s: must not be empty", key)
	}
	out := make([]string, 0, len(arr))
	for i, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Sprintf("%s[%d]: must be a string", key, i)
		}
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Sprintf("%s[%d]: must not be empty", key, i)
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, ""
}

func optFloatArg(args map[string]any, key string) (float64, bool, string) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false, ""
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false, fmt.Sprintf("%s: must be a number", key)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, fmt.Sprintf("%s: must be a finite number", key)
	}
	return f, true, ""
}

func optIntArg(args map[string]any, key string) (int, bool, string) {
	f, present, errMsg := optFloatArg(args, key)
	if errMsg != "" || !present {
		return 0, present, errMsg
	}
	if f != math.Trunc(f) {
		return 0, false, fmt.Sprintf("%s: must be an integer", key)
	}
	return int(f), true, ""
}

func optBoolArg(args map[string]any, key string) (bool, bool, string) {
	v, ok := args[key]
	if !ok || v == nil {
		return false, false, ""
	}
	b, ok := v.(bool)
	if !ok {
		return false, false, fmt.Sprintf("%s: must be a boolean", key)
	}
	return b, true, ""
}

// handlerErr maps a Source error to the handler's error string:
// an invalid input is returned as-is (-32602, correct); anything
// else is a backing-store failure, prefixed so the caller can
// tell it from a bad input (the dispatcher still answers -32602
// for both — see the package note).
func handlerErr(err error) string {
	if errors.Is(err, ErrInvalidInput) {
		return err.Error()
	}
	return "unavailable: " + err.Error()
}

func schemaProps(props map[string]any, required []string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func numProp(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

func init() {
	// Scopes FIRST, then RegisterTool — RegisterTool panics
	// otherwise (see internal/mcp/mcp.go).
	RegisterToolScopes("list_combos", ScopeReadRouting)
	RegisterTool(Tool{
		Name:        "list_combos",
		Description: "List every configured routing combo (config routing groups) with its members, strategy, and whether it is the session-active combo. Read-only.",
		InputSchema: emptySchema(),
	}, toolListCombos)

	RegisterToolScopes("create_combo", ScopeManageRouting)
	RegisterTool(Tool{
		Name:        "create_combo",
		Description: "Create a routing combo: a named ordered list of providers (members may carry a reasoning effort as \"provider@effort\") with a routing strategy. The combo is written to the config routing groups (and the chain-strategy mirror) and persisted to the config file. Members must be configured providers; the strategy must be a known strategy name or a combo execution mode (fusion, pipeline, jev).",
		InputSchema: schemaProps(map[string]any{
			"name":     strProp("Combo name: non-empty, no \":\" or \"/\" (routing-group name rule)."),
			"members":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Ordered provider ids, optionally \"provider@effort\". At least one."},
			"strategy": strProp("Routing strategy (default \"priority\"). A strategy name, or a combo execution mode: fusion, pipeline, jev."),
		}, []string{"name", "members"}),
	}, toolCreateCombo)

	RegisterToolScopes("switch_combos", ScopeManageRouting)
	RegisterTool(Tool{
		Name:        "switch_combos",
		Description: "Set the session-active combo (the default combo the routing tools report). The serving router has no single active combo — every group is always addressable — so this selects the MCP session's default only; it never changes what the serving router routes. Pass active=false to clear the active selection.",
		InputSchema: schemaProps(map[string]any{
			"name":   strProp("Combo name to activate."),
			"active": map[string]any{"type": "boolean", "description": "True (default) to activate the combo; false to clear the active selection when it names the active combo."},
		}, []string{"name"}),
	}, toolSwitchCombos)

	RegisterToolScopes("get_combo_metrics", ScopeReadRouting)
	RegisterTool(Tool{
		Name:        "get_combo_metrics",
		Description: "Measured ledger history for one combo's member providers (all time): requests, successes, errors, success rate, tokens, cost, and mean latency over recent successful events (at most the 500 most recent events — older events do not affect the mean). Cost is a lower bound when any of the combo's tokens are unpriced (cost_known=false) — unpriced usage is never reported as $0.",
		InputSchema: schemaProps(map[string]any{
			"name": strProp("Combo name."),
		}, []string{"name"}),
	}, toolGetComboMetrics)

	RegisterToolScopes("set_routing_strategy", ScopeManageRouting)
	RegisterTool(Tool{
		Name:        "set_routing_strategy",
		Description: "Set the routing strategy: globally (no chain) or for one chain (a model id, or a combo/group name). The strategy must be a known strategy name. The change is written to the config and persisted to the config file.",
		InputSchema: schemaProps(map[string]any{
			"strategy": strProp("Canonical strategy name (e.g. priority, cheapest, fastest, most-reliable, weighted, lkgp). Aliases are accepted."),
			"chain":    strProp("Chain to set the strategy for (a model id or combo name). Empty or omitted sets the global strategy."),
		}, []string{"strategy"}),
	}, toolSetRoutingStrategy)

	RegisterToolScopes("best_combo_for_task", ScopeReadRouting)
	RegisterTool(Tool{
		Name:        "best_combo_for_task",
		Description: "Recommend a combo for a task, ranked on measured ledger metrics (success rate, cost per request, mean latency) and the task's strategy preference (coding tasks prefer code, fast tasks fastest, cheap tasks cheapest). A combo with no history ranks on strategy match alone and is reported as metrics-unknown. A cost/latency constraint a combo cannot be checked against (unknown price, no latency) does not exclude it — the result reports the constraint as not applied for that combo.",
		InputSchema: schemaProps(map[string]any{
			"task":           strProp("Task description, e.g. \"coding\", \"fast chat\", \"cheapest summarisation\"."),
			"max_cost_usd":   numProp("Optional maximum estimated cost per request in USD (0 or omitted = no cap)."),
			"max_latency_ms": numProp("Optional maximum mean latency in milliseconds (0 or omitted = no cap)."),
		}, []string{"task"}),
	}, toolBestComboForTask)

	RegisterToolScopes("explain_route", ScopeReadRouting)
	RegisterTool(Tool{
		Name:        "explain_route",
		Description: "Explain how a model would be routed: the effective strategy and its rule, and the attempt order over the providers offering the model (declared order). Dry run — no provider is called. Strategies that re-rank do so at request time from live signals this tool does not observe; the result says so. Optionally restrict the order to one combo's members.",
		InputSchema: schemaProps(map[string]any{
			"model": strProp("Model id to explain routing for."),
			"combo": strProp("Optional combo name: restrict the attempt order to that combo's members. When omitted, the session-active combo (set by switch_combos) applies when one is set; otherwise all providers are shown."),
		}, []string{"model"}),
	}, toolExplainRoute)

	RegisterToolScopes("simulate_route", ScopeReadRouting)
	RegisterTool(Tool{
		Name:        "simulate_route",
		Description: "Dry-run a route for a model: the fallback tree (one attempt per target, in order, with what happens when each fails) and the first-attempt prompt-cost estimate from the real price book when prompt_tokens is given. No provider is called. Completion cost is never estimated (the completion length is unknown before the call). Unpriced targets report cost_known=false, never $0.",
		InputSchema: schemaProps(map[string]any{
			"model":         strProp("Model id to simulate routing for."),
			"prompt_tokens": numProp("Optional prompt-token count to price the first attempt at (integer >= 0). Without it no cost is estimated."),
			"combo":         strProp("Optional combo name: restrict the fallback tree to that combo's members. When omitted, the session-active combo (set by switch_combos) applies when one is set; otherwise all providers are shown."),
		}, []string{"model"}),
	}, toolSimulateRoute)

	RegisterToolScopes("pick_fastest_model", ScopeReadRouting)
	RegisterTool(Tool{
		Name:        "pick_fastest_model",
		Description: "Pick the fastest model by MEASURED mean latency over recent successful ledger events. With no measured latency it errors honestly instead of guessing from prices or catalog order; when the latency backend itself is unavailable, the error says the backend is unavailable. Optionally restrict to one provider.",
		InputSchema: schemaProps(map[string]any{
			"provider":     strProp("Optional provider id to restrict the choice to."),
			"min_requests": numProp("Optional minimum measured requests a model needs to be eligible (integer >= 1, default 1)."),
		}, nil),
	}, toolPickFastestModel)
}

// modelStatsAvailable is the optional Source capability behind
// the N4 message: a Source whose measured-latency backend may be
// unwired reports whether it is available at all. ModelStats()
// has no error return — an empty list is the honest "no stats"
// answer — so without this the handler cannot tell "the ledger
// has no events yet" from "there is no ledger behind this
// transport". Both real Sources implement it: the HTTP Source
// (false when the serve process has no ledger wired) and the
// stdio registry Source (false when no data dir is configured
// or the ledger file exists but cannot be opened; true when the
// ledger is missing — no events yet — or opens cleanly). A
// Source that does not implement it is treated as available.
// The plain test fake does not implement it; the
// statsBackendSource test wrapper does, to drive the N4 branch
// with a chosen availability.
type modelStatsAvailable interface {
	ModelStatsAvailable() bool
}

// --- handlers -----------------------------------------------------------

func toolListCombos(s *Server, args map[string]any) (map[string]any, string) {
	combos := s.src.Combos()
	if combos == nil {
		combos = []ComboInfo{}
	}
	active := s.src.ActiveCombo()
	var activePtr any
	if active != "" {
		activePtr = active
	}
	return toolResult(map[string]any{
		"combos":       combos,
		"active_combo": activePtr,
		"count":        len(combos),
	}), ""
}

func toolCreateCombo(s *Server, args map[string]any) (map[string]any, string) {
	name, errMsg := reqStringArg(args, "name")
	if errMsg != "" {
		return nil, errMsg
	}
	members, errMsg := reqStringListArg(args, "members")
	if errMsg != "" {
		return nil, errMsg
	}
	strategy, errMsg := optStringArg(args, "strategy")
	if errMsg != "" {
		return nil, errMsg
	}
	if strategy == "" {
		strategy = "priority"
	}
	if err := s.src.CreateCombo(s.callCtx, name, members, strategy); err != nil {
		return nil, handlerErr(err)
	}
	// Re-read: the Source normalises the strategy (aliases to
	// canonical names), so the reply reports what was stored,
	// not what was asked for.
	for _, c := range s.src.Combos() {
		if c.Name == name {
			return toolResult(map[string]any{"combo": c, "created": true}), ""
		}
	}
	return nil, "unavailable: combo was created but cannot be read back"
}

func toolSwitchCombos(s *Server, args map[string]any) (map[string]any, string) {
	name, errMsg := reqStringArg(args, "name")
	if errMsg != "" {
		return nil, errMsg
	}
	active, _, errMsg := optBoolArg(args, "active")
	if errMsg != "" {
		return nil, errMsg
	}
	if _, present, _ := optBoolArg(args, "active"); !present {
		active = true
	}
	if !active {
		if s.src.ActiveCombo() == name {
			if err := s.src.SetActiveCombo(""); err != nil {
				return nil, handlerErr(err)
			}
			return toolResult(map[string]any{"active_combo": nil, "note": "Active combo cleared. The serving router has no single active combo; this selection is the MCP session's default only."}), ""
		}
		return nil, fmt.Sprintf("%s: combo %q is not the active combo", ErrInvalidInput, name)
	}
	if err := s.src.SetActiveCombo(name); err != nil {
		return nil, handlerErr(err)
	}
	return toolResult(map[string]any{
		"active_combo": name,
		"note":         "Session-active combo only: the serving router has no single active combo (every group is always addressable), so this does not change what the serving router routes.",
	}), ""
}

func toolGetComboMetrics(s *Server, args map[string]any) (map[string]any, string) {
	name, errMsg := reqStringArg(args, "name")
	if errMsg != "" {
		return nil, errMsg
	}
	m, err := s.src.ComboMetrics(name)
	if err != nil {
		return nil, handlerErr(err)
	}
	return toolResult(map[string]any{"metrics": m}), ""
}

func toolSetRoutingStrategy(s *Server, args map[string]any) (map[string]any, string) {
	strategy, errMsg := reqStringArg(args, "strategy")
	if errMsg != "" {
		return nil, errMsg
	}
	chain, errMsg := optStringArg(args, "chain")
	if errMsg != "" {
		return nil, errMsg
	}
	previous := s.src.RoutingStrategy(chain)
	if err := s.src.SetRoutingStrategy(chain, strategy); err != nil {
		return nil, handlerErr(err)
	}
	current := s.src.RoutingStrategy(chain)
	return toolResult(map[string]any{
		"chain":    chain,
		"strategy": current,
		"previous": previous,
	}), ""
}

func toolExplainRoute(s *Server, args map[string]any) (map[string]any, string) {
	model, errMsg := reqStringArg(args, "model")
	if errMsg != "" {
		return nil, errMsg
	}
	combo, errMsg := optStringArg(args, "combo")
	if errMsg != "" {
		return nil, errMsg
	}
	plan, err := s.src.ExplainRoute(model)
	if err != nil {
		return nil, handlerErr(err)
	}
	if combo == "" {
		// No combo named: default to the session-active combo
		// (switch_combos) when one is set — the documented
		// default. "" means no active combo: all providers.
		combo = s.src.ActiveCombo()
	}
	if combo != "" {
		filtered, errMsg := restrictAttemptsToCombo(s, combo, plan.Attempts)
		if errMsg != "" {
			return nil, errMsg
		}
		plan.Combo = combo
		plan.Attempts = filtered
	}
	return toolResult(map[string]any{"route": plan}), ""
}

func toolSimulateRoute(s *Server, args map[string]any) (map[string]any, string) {
	model, errMsg := reqStringArg(args, "model")
	if errMsg != "" {
		return nil, errMsg
	}
	combo, errMsg := optStringArg(args, "combo")
	if errMsg != "" {
		return nil, errMsg
	}
	promptTokens := 0
	if n, present, errMsg := optIntArg(args, "prompt_tokens"); errMsg != "" {
		return nil, errMsg
	} else if present {
		if n < 0 {
			return nil, "prompt_tokens: must be >= 0"
		}
		promptTokens = n
	}
	sim, err := s.src.SimulateRoute(model, promptTokens)
	if err != nil {
		return nil, handlerErr(err)
	}
	if combo == "" {
		// Same session-active-combo default as explain_route.
		combo = s.src.ActiveCombo()
	}
	if combo != "" {
		members, errMsg := comboMembers(s, combo)
		if errMsg != "" {
			return nil, errMsg
		}
		kept := make([]SimAttempt, 0, len(sim.Attempts))
		for _, a := range sim.Attempts {
			if members[a.Provider] {
				kept = append(kept, a)
			}
		}
		if len(kept) == 0 {
			return nil, fmt.Sprintf("%s: combo %q offers no provider for model %q", ErrInvalidInput, combo, model)
		}
		sim.Combo = combo
		sim.Attempts = kept
		// The cost estimate prices the FIRST attempt: recompute
		// it for the restricted tree, or drop it when the first
		// kept target is unpriced.
		sim.EstimatedPromptCostUSD = kept[0].EstimatedPromptCostUSD
		sim.CostKnown = kept[0].PriceKnown
	}
	return toolResult(map[string]any{"simulation": sim}), ""
}

// comboMembers resolves a combo name to its member provider ids
// (effort suffixes stripped). Unknown combo → invalid input.
func comboMembers(s *Server, name string) (map[string]bool, string) {
	for _, c := range s.src.Combos() {
		if c.Name == name {
			out := make(map[string]bool, len(c.Members))
			for _, m := range c.Members {
				if at := strings.LastIndex(m, "@"); at >= 0 {
					m = m[:at]
				}
				out[m] = true
			}
			return out, ""
		}
	}
	return nil, fmt.Sprintf("%s: unknown combo %q", ErrInvalidInput, name)
}

// restrictAttemptsToCombo keeps only the attempts whose provider
// is a member of the named combo. An unknown combo, or a combo
// with no provider offering the model, is invalid input.
func restrictAttemptsToCombo(s *Server, combo string, attempts []RouteAttempt) ([]RouteAttempt, string) {
	members, errMsg := comboMembers(s, combo)
	if errMsg != "" {
		return nil, errMsg
	}
	kept := make([]RouteAttempt, 0, len(attempts))
	for _, a := range attempts {
		if members[a.Provider] {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Sprintf("%s: combo %q offers no provider for this model", ErrInvalidInput, combo)
	}
	return kept, ""
}

// --- best_combo_for_task --------------------------------------------------

// taskPreferredStrategy maps a task description to the strategy it
// prefers, by keyword. "" means no preference (the combo's own
// strategy decides). This is a preference for RANKING only — the
// tool never sets a strategy.
func taskPreferredStrategy(task string) string {
	lower := strings.ToLower(task)
	switch {
	case strings.Contains(lower, "code") || strings.Contains(lower, "coding") ||
		strings.Contains(lower, "debug") || strings.Contains(lower, "program"):
		return "code"
	case strings.Contains(lower, "fast") || strings.Contains(lower, "quick") ||
		strings.Contains(lower, "latency") || strings.Contains(lower, "realtime") ||
		strings.Contains(lower, "real-time"):
		return "fastest"
	case strings.Contains(lower, "cheap") || strings.Contains(lower, "cost") ||
		strings.Contains(lower, "budget") || strings.Contains(lower, "thrift"):
		return "cheapest"
	case strings.Contains(lower, "reliab"):
		return "most-reliable"
	default:
		return ""
	}
}

type bestComboCandidate struct {
	combo          ComboInfo
	metrics        ComboMetrics
	metricsOK      bool
	strategyMatch  bool
	costPerRequest *float64
	reasons        []string
	constraints    map[string]any
}

func toolBestComboForTask(s *Server, args map[string]any) (map[string]any, string) {
	task, errMsg := reqStringArg(args, "task")
	if errMsg != "" {
		return nil, errMsg
	}
	maxCost, costPresent, errMsg := optFloatArg(args, "max_cost_usd")
	if errMsg != "" {
		return nil, errMsg
	}
	// 0 is allowed: the tool schema documents "0 or omitted = no
	// cap" for both caps — 0 is the no-cap sentinel, not a $0 cap.
	if costPresent && maxCost < 0 {
		return nil, "max_cost_usd: must be >= 0"
	}
	maxLatency, latencyPresent, errMsg := optFloatArg(args, "max_latency_ms")
	if errMsg != "" {
		return nil, errMsg
	}
	// 0 is allowed: the tool schema documents "0 or omitted = no
	// cap" for both caps — 0 is the no-cap sentinel, not a 0 ms cap.
	if latencyPresent && maxLatency < 0 {
		return nil, "max_latency_ms: must be >= 0"
	}
	combos := s.src.Combos()
	if len(combos) == 0 {
		return nil, fmt.Sprintf("%s: no combos configured", ErrInvalidInput)
	}
	preferred := taskPreferredStrategy(task)
	candidates := make([]bestComboCandidate, 0, len(combos))
	for _, c := range combos {
		cand := bestComboCandidate{combo: c, constraints: map[string]any{}}
		cand.strategyMatch = preferred != "" && c.Strategy == preferred
		if cand.strategyMatch {
			cand.reasons = append(cand.reasons, "strategy "+c.Strategy+" matches the task preference ("+preferred+")")
		}
		m, err := s.src.ComboMetrics(c.Name)
		if err == nil {
			cand.metrics = m
			cand.metricsOK = true
			if m.Requests > 0 {
				cpr := m.CostUSD / float64(m.Requests)
				if m.CostKnown {
					cand.costPerRequest = &cpr
					cand.reasons = append(cand.reasons, fmt.Sprintf("measured $%.6g per request over %d requests", cpr, m.Requests))
				} else {
					cand.reasons = append(cand.reasons, fmt.Sprintf("measured $%.6g per request over %d requests is a lower bound — %d tokens unpriced", cpr, m.Requests, m.UnpricedTokens))
				}
			}
			if m.SuccessRate != nil {
				cand.reasons = append(cand.reasons, fmt.Sprintf("measured success rate %.1f%% over %d requests", *m.SuccessRate*100, m.Requests))
			}
			if m.LatencyKnown {
				cand.reasons = append(cand.reasons, fmt.Sprintf("measured mean latency %.1f ms over %d recent events", m.MeanLatencyMS, m.LatencyEvents))
			}
		} else {
			cand.reasons = append(cand.reasons, "no metrics available ("+err.Error()+")")
		}
		// Constraints: a combo that cannot be checked against a
		// constraint is NOT excluded — the constraint is
		// reported as not applied for it, never as passed. A cap
		// of 0 is the documented no-cap sentinel ("0 or omitted
		// = no cap" in the tool schema), hence the "> 0" guards.
		if costPresent && maxCost > 0 {
			if cand.costPerRequest != nil {
				if *cand.costPerRequest > maxCost {
					continue
				}
				cand.constraints["max_cost_usd_applied"] = true
			} else {
				cand.constraints["max_cost_usd_applied"] = false
				cand.constraints["max_cost_usd_note"] = "cost per request unknown (no priced history) — constraint not applied, combo not excluded"
			}
		}
		if latencyPresent && maxLatency > 0 {
			if cand.metricsOK && cand.metrics.LatencyKnown {
				if cand.metrics.MeanLatencyMS > maxLatency {
					continue
				}
				cand.constraints["max_latency_ms_applied"] = true
			} else {
				cand.constraints["max_latency_ms_applied"] = false
				cand.constraints["max_latency_ms_note"] = "mean latency unknown (no recent successful events) — constraint not applied, combo not excluded"
			}
		}
		candidates = append(candidates, cand)
	}
	if len(candidates) == 0 {
		return nil, fmt.Sprintf("%s: no combo satisfies the given constraints", ErrInvalidInput)
	}
	// Rank: strategy match first, then measured success rate
	// (unknown last), then measured cost per request (unknown
	// last), then mean latency (unknown last), then name — so
	// the choice is deterministic on identical numbers.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.strategyMatch != b.strategyMatch {
			return a.strategyMatch
		}
		aSR, bSR := -1.0, -1.0
		if a.metricsOK && a.metrics.SuccessRate != nil {
			aSR = *a.metrics.SuccessRate
		}
		if b.metricsOK && b.metrics.SuccessRate != nil {
			bSR = *b.metrics.SuccessRate
		}
		if aSR != bSR {
			return aSR > bSR
		}
		aC, bC := math.Inf(1), math.Inf(1)
		if a.costPerRequest != nil {
			aC = *a.costPerRequest
		}
		if b.costPerRequest != nil {
			bC = *b.costPerRequest
		}
		if aC != bC {
			return aC < bC
		}
		aL, bL := math.Inf(1), math.Inf(1)
		if a.metricsOK && a.metrics.LatencyKnown {
			aL = a.metrics.MeanLatencyMS
		}
		if b.metricsOK && b.metrics.LatencyKnown {
			bL = b.metrics.MeanLatencyMS
		}
		if aL != bL {
			return aL < bL
		}
		return a.combo.Name < b.combo.Name
	})
	best := candidates[0]
	// Copy best.reasons: it aliases candidates[0].reasons, so
	// appending the no-history note below must not write into
	// the candidate's backing array.
	reasons := make([]string, len(best.reasons))
	copy(reasons, best.reasons)
	if !best.metricsOK {
		reasons = append(reasons, "ranked on strategy match alone — no measured history")
	}
	return toolResult(map[string]any{
		"combo":              best.combo,
		"metrics":            best.metrics,
		"metrics_known":      best.metricsOK,
		"preferred_strategy": preferred,
		"reasons":            reasons,
		"constraints":        best.constraints,
		"candidates_seen":    len(candidates),
	}), ""
}

// --- pick_fastest_model ---------------------------------------------------

func toolPickFastestModel(s *Server, args map[string]any) (map[string]any, string) {
	provider, errMsg := optStringArg(args, "provider")
	if errMsg != "" {
		return nil, errMsg
	}
	minRequests := 1
	if n, present, errMsg := optIntArg(args, "min_requests"); errMsg != "" {
		return nil, errMsg
	} else if present {
		if n < 1 {
			return nil, "min_requests: must be >= 1"
		}
		minRequests = n
	}
	stats := s.src.ModelStats()
	var eligible []ModelStat
	for _, st := range stats {
		if provider != "" && st.Provider != provider {
			continue
		}
		if st.Requests < minRequests || !st.LatencyKnown || st.LatencyEvents == 0 {
			continue
		}
		eligible = append(eligible, st)
	}
	if len(eligible) == 0 {
		// N4: when the backend itself is unavailable, say so —
		// "no measured model latency yet" would claim the ledger
		// was consulted and found no events, which over HTTP
		// with no ledger wired is false. The check runs before
		// the provider-specific message: with no backend there
		// is no per-provider latency either.
		if backend, ok := s.src.(modelStatsAvailable); ok && !backend.ModelStatsAvailable() {
			return nil, "unavailable: model latency backend is not available over this transport (ledger unavailable) — pick_fastest_model ranks only on measured ledger latency"
		}
		if provider != "" {
			return nil, fmt.Sprintf("no measured model latency for provider %q (need at least %d successful events with a recorded latency)", provider, minRequests)
		}
		return nil, fmt.Sprintf("no measured model latency yet (need at least %d successful events with a recorded latency)", minRequests)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.MeanLatencyMS != b.MeanLatencyMS {
			return a.MeanLatencyMS < b.MeanLatencyMS
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	fastest := eligible[0]
	return toolResult(map[string]any{
		"model":           fastest.Model,
		"provider":        fastest.Provider,
		"mean_latency_ms": fastest.MeanLatencyMS,
		"requests":        fastest.Requests,
		"latency_events":  fastest.LatencyEvents,
		"success_rate":    fastest.SuccessRate,
		"note":            "Measured mean latency over recent successful ledger events only — no price or catalog guess.",
	}), ""
}
