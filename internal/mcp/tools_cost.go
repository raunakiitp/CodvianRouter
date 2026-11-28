// M9.3 — cost/quota tools (cost_report, check_quota,
// set_budget_guard).
//
// COST HONESTY (the rule this whole file exists to enforce):
// every dollar figure comes from the REAL ledger and the REAL
// price book, never a hardcoded rate. Usage whose price is
// unknown is reported as UNKNOWN — CostKnown false, with the
// unpriced tokens and models listed — never as $0. (Reporting
// unpriced usage as $0 is the the sector baseline analytics failure
// parity tracker records for their usage reporting; the ledger's own
// honesty rules say the same thing — "unpriced ≠ $0" — in
// internal/ledger/keybudget.go.) A cost figure over a window
// that mixes priced and unpriced usage is a LOWER BOUND, and
// the result says so.
//
// Scopes: cost_report and check_quota take mcp:read:cost (added
// M9.3 — see tools_routing.go for the scope choice);
// set_budget_guard takes mcp:manage:cost.
//
// CALLER-ONLY KEY RULE (M9.2 security review N3, enforced by
// resolveCostKeyID below — the handlers never pass a
// caller-supplied key_id to the Source): the local operator
// (stdio, same trust as the router process) may name any key, or
// omit key_id on cost_report for all keys. Any OTHER caller — a
// remote gateway key over the HTTP/SSE transports, whose identity
// is the authenticated key id — acts ONLY on its own key: a
// supplied key_id naming a different key is REJECTED (-32602),
// never silently remapped, and an omitted key_id defaults to the
// caller's own id. Without this, a remote mcp:manage:cost key
// could read or set ANY key's quota/budget the moment these tools
// wire to the ledger. The Source methods take a key id as a plain
// argument and cannot tell whose it is, so the enforcement lives
// here, in the handlers — never call them with a caller-supplied
// key id from a new path without the same check.
//
// set_budget_guard writes a real key budget into the ledger
// (the rolling 5-hour cap, plus optional calendar daily/weekly/
// monthly caps). The unpriced policy is fail_closed by default:
// a capped key refuses requests to unpriced models, because the
// router cannot prove the spend fits the budget. The guard
// applies to spend from the moment it is set; existing usage
// history is untouched.
package mcp

import (
	"fmt"
	"time"
)

// ModelCost is one (provider, model) cell of a cost report:
// requests and tokens in the window, the priced cost, and
// whether the pair's price is known. UnpricedTokens counts the
// tokens in the cell whose price is unknown — they contribute
// nothing to CostUSD, which is a lower bound when they are > 0.
type ModelCost struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	Requests         int     `json:"requests"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	PriceKnown       bool    `json:"price_known"`
	UnpricedTokens   int     `json:"unpriced_tokens"`
}

// CostReport is the cost_report result. KeyID "" means all keys.
// Days 0 means all time. CostUSD is the priced cost in the
// window; CostKnown is false when any of the window's tokens are
// unpriced, and then CostUSD is a lower bound, never the whole
// cost. Sampled is true when the per-key counts come from the
// ledger's recent-events sample (the ledger has no per-key
// aggregate query — see the production Source); the authoritative
// per-key spend is still the full-window sum, and the Note says
// which half is sampled.
type CostReport struct {
	KeyID            string      `json:"key_id,omitempty"`
	Days             int         `json:"days"`
	WindowStart      time.Time   `json:"window_start"`
	WindowEnd        time.Time   `json:"window_end"`
	Requests         int         `json:"requests"`
	OK               int         `json:"ok"`
	Errors           int         `json:"errors"`
	PromptTokens     int         `json:"prompt_tokens"`
	CompletionTokens int         `json:"completion_tokens"`
	TotalTokens      int         `json:"total_tokens"`
	CostUSD          float64     `json:"cost_usd"`
	CostKnown        bool        `json:"cost_known"`
	UnpricedTokens   int         `json:"unpriced_tokens"`
	UnpricedModels   []string    `json:"unpriced_models,omitempty"`
	ByModel          []ModelCost `json:"by_model"`
	Sampled          bool        `json:"sampled"`
	Note             string      `json:"note,omitempty"`
}

// QuotaWindow is one calendar window's position in a quota
// report: the limit (nil = unset), the priced spend in the
// current period, the remaining budget (nil when unset), and
// whether the window is at its limit.
type QuotaWindow struct {
	LimitUSD     *float64 `json:"limit_usd,omitempty"`
	SpentUSD     float64  `json:"spent_usd"`
	RemainingUSD *float64 `json:"remaining_usd,omitempty"`
	AtLimit      bool     `json:"at_limit"`
}

// QuotaReport is the check_quota result: the key's rolling
// 5-hour budget position plus its calendar windows, from the
// ledger's KeyBudgetStatus. A key with no budget row reports
// HasBudget false and RemainingUSD nil — unlimited, never an
// invented cap. UnpricedModels lists the models the key used in
// the window whose price is unknown (fail_closed refuses them
// under a cap; see the ledger's UnpricedPolicy).
type QuotaReport struct {
	KeyID          string      `json:"key_id"`
	HasBudget      bool        `json:"has_budget"`
	LimitUSD       float64     `json:"limit_usd"`
	SpentUSD       float64     `json:"spent_usd"`
	RemainingUSD   *float64    `json:"remaining_usd,omitempty"`
	AtLimit        bool        `json:"at_limit"`
	UnpricedPolicy string      `json:"unpriced_policy"`
	ActiveWindow   string      `json:"active_window"`
	ActiveLimitUSD *float64    `json:"active_limit_usd,omitempty"`
	WarningReached bool        `json:"warning_reached"`
	UnpricedModels []string    `json:"unpriced_models,omitempty"`
	Daily          QuotaWindow `json:"daily"`
	Weekly         QuotaWindow `json:"weekly"`
	Monthly        QuotaWindow `json:"monthly"`
}

func init() {
	// Scopes FIRST, then RegisterTool — RegisterTool panics
	// otherwise (see internal/mcp/mcp.go).
	RegisterToolScopes("cost_report", ScopeReadCost)
	RegisterTool(Tool{
		Name:        "cost_report",
		Description: "Cost report from the real ledger and price book: requests, tokens, and priced cost over a window, per model. Usage whose price is unknown is reported as UNKNOWN (cost_known=false, unpriced tokens/models listed), never as $0. The local operator may omit key_id for all keys or name any key; a gateway-key caller may only report on its own key id (omit it, or pass your own id — another key's id is rejected).",
		InputSchema: schemaProps(map[string]any{
			"key_id": strProp("Optional gateway key id to report on. Omit for all keys (local operator only); a gateway-key caller reports on its own key only."),
			"days":   numProp("Optional window in days (integer 1-365). 0 or omitted = all time."),
		}, nil),
	}, toolCostReport)

	RegisterToolScopes("check_quota", ScopeReadCost)
	RegisterTool(Tool{
		Name:        "check_quota",
		Description: "Quota position for one gateway key from the real ledger: the rolling 5-hour budget (limit, priced spend, remaining, at-limit) plus the daily/weekly/monthly calendar windows and the unpriced-usage policy. A key with no budget is unlimited (has_budget=false, no invented cap). A gateway-key caller may only check its own key id (omit it, or pass your own id — another key's id is rejected).",
		InputSchema: schemaProps(map[string]any{
			"key_id": strProp("Gateway key id to check. The local operator must supply it; a gateway-key caller may omit it (its own key is used) but may not name another key."),
		}, nil),
	}, toolCheckQuota)

	RegisterToolScopes("set_budget_guard", ScopeManageCost)
	RegisterTool(Tool{
		Name:        "set_budget_guard",
		Description: "Set a budget guard for one gateway key in the real ledger: a rolling 5-hour USD cap (limit_usd, required, >= 0), optional calendar daily/weekly/monthly USD caps, and the unpriced-usage policy (fail_closed by default — a capped key refuses unpriced models; count_as_zero lets them through counting $0). The guard applies to spend from the moment it is set; existing usage history is untouched. A gateway-key caller may only guard its own key id (omit it, or pass your own id — another key's id is rejected).",
		InputSchema: schemaProps(map[string]any{
			"key_id":            strProp("Gateway key id to guard. The local operator must supply it; a gateway-key caller may omit it (its own key is used) but may not name another key."),
			"limit_usd":         numProp("Rolling 5-hour USD cap (>= 0; 0 = no spend allowed)."),
			"unpriced_policy":   strProp("Unpriced-usage policy: \"fail_closed\" (default) or \"count_as_zero\"."),
			"daily_limit_usd":   numProp("Optional calendar daily USD cap (>= 0)."),
			"weekly_limit_usd":  numProp("Optional calendar weekly USD cap (>= 0)."),
			"monthly_limit_usd": numProp("Optional calendar monthly USD cap (>= 0)."),
		}, []string{"limit_usd"}),
	}, toolSetBudgetGuard)
}

// resolveCostKeyID resolves the key a cost tool acts on — the
// M9.2 N3 enforcement point (see the file header). The local
// operator's supplied key_id is honoured as-is ("" included:
// cost_report over all keys). Any other caller's key is the
// authenticated identity (s.callerID — the gateway key id the
// transport resolved, never the argument): a supplied key_id
// that names a different key is rejected with -32602, and an
// omitted one defaults to the caller's own id. The rejection is
// loud on purpose: silently remapping "key-2" to "key-1" would
// let a caller believe it had read or set another key's budget.
// Callers that must supply a key (check_quota, set_budget_guard)
// still reject an empty result with "key_id: required" — for the
// operator, whose "" means "no key named"; a non-operator caller
// always has a non-empty identity id, so it never hits that.
func resolveCostKeyID(s *Server, args map[string]any) (string, string) {
	supplied, errMsg := optStringArg(args, "key_id")
	if errMsg != "" {
		return "", errMsg
	}
	if s.identity.Kind == KindLocalOperator {
		return supplied, ""
	}
	own := s.callerID()
	if supplied != "" && supplied != own {
		return "", fmt.Sprintf("%s: key_id %q is not the authenticated caller %q — a gateway key may only read or set its own quota/budget", ErrInvalidInput, supplied, own)
	}
	return own, ""
}

func toolCostReport(s *Server, args map[string]any) (map[string]any, string) {
	keyID, errMsg := resolveCostKeyID(s, args)
	if errMsg != "" {
		return nil, errMsg
	}
	days := 0
	if n, present, errMsg := optIntArg(args, "days"); errMsg != "" {
		return nil, errMsg
	} else if present {
		if n < 0 || n > 365 {
			return nil, "days: must be between 0 (all time) and 365"
		}
		days = n
	}
	rep, err := s.src.CostReport(keyID, days)
	if err != nil {
		return nil, handlerErr(err)
	}
	return toolResult(map[string]any{"cost_report": rep}), ""
}

func toolCheckQuota(s *Server, args map[string]any) (map[string]any, string) {
	keyID, errMsg := resolveCostKeyID(s, args)
	if errMsg != "" {
		return nil, errMsg
	}
	if keyID == "" {
		return nil, "key_id: required"
	}
	rep, err := s.src.CheckQuota(keyID)
	if err != nil {
		return nil, handlerErr(err)
	}
	return toolResult(map[string]any{"quota": rep}), ""
}

func toolSetBudgetGuard(s *Server, args map[string]any) (map[string]any, string) {
	keyID, errMsg := resolveCostKeyID(s, args)
	if errMsg != "" {
		return nil, errMsg
	}
	if keyID == "" {
		return nil, "key_id: required"
	}
	limit, present, errMsg := optFloatArg(args, "limit_usd")
	if errMsg != "" {
		return nil, errMsg
	}
	if !present {
		return nil, "limit_usd: required"
	}
	if limit < 0 {
		return nil, "limit_usd: must be >= 0"
	}
	policy, errMsg := optStringArg(args, "unpriced_policy")
	if errMsg != "" {
		return nil, errMsg
	}
	if policy == "" {
		policy = "fail_closed"
	}
	if policy != "fail_closed" && policy != "count_as_zero" {
		return nil, fmt.Sprintf("%s: unknown unpriced_policy %q (want fail_closed or count_as_zero)", ErrInvalidInput, policy)
	}
	var daily, weekly, monthly *float64
	for _, spec := range []struct {
		key string
		dst **float64
	}{
		{"daily_limit_usd", &daily},
		{"weekly_limit_usd", &weekly},
		{"monthly_limit_usd", &monthly},
	} {
		v, present, errMsg := optFloatArg(args, spec.key)
		if errMsg != "" {
			return nil, errMsg
		}
		if present {
			if v < 0 {
				return nil, fmt.Sprintf("%s: must be >= 0", spec.key)
			}
			*spec.dst = &v
		}
	}
	if err := s.src.SetBudgetGuard(keyID, limit, policy, daily, weekly, monthly); err != nil {
		return nil, handlerErr(err)
	}
	// Re-read: the reply reports the stored guard, not the
	// request echo — a read-back that disagrees is a bug, and
	// the caller sees the ledger's answer.
	rep, err := s.src.CheckQuota(keyID)
	if err != nil {
		return nil, handlerErr(err)
	}
	return toolResult(map[string]any{"quota": rep, "guard_set": true}), ""
}
