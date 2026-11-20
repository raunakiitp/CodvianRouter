// M9.6 client-side hardening: allowlist + spend controls for the
// MCP client executor.
//
// The gap this closes (parity tracker Area 9, "Spend controls on MCP
// tools", PARTIAL): Execute used to run any tool on any connected
// server with no cap, no estimate and no allowlist. All three
// controls now live in the single execution choke point —
// Registry.ExecuteDetailed, which Registry.Execute and the agent
// loop (dispatchOne) both funnel through — so no caller can bypass
// them by calling the executor directly.
//
// Allowlist (fail-closed, deny overrides allow):
//   - Patterns match "server__tool" (glob "*" matches any run of
//     characters), a plain tool name (any server), or a qualified
//     discovered name. Examples: "calc__echo", "calc__*",
//     "echo", "*".
//   - When controls are engaged, a tool executes only if some
//     Allow pattern matches AND no Deny pattern matches. An
//     empty Allow list denies everything.
//   - The check runs BEFORE dispatch: a denied tool never reaches
//     the server (no tools/call is sent, no subprocess spawned).
//
// Spend controls (per-registry budget — the registry is the
// session for one gateway process; a fresh Registry is a fresh
// budget, ResetBudget restarts the window in place):
//   - MaxCalls: at most that many dispatched tool executions per
//     budget window (0 = unlimited). The N+1th call is refused
//     before dispatch.
//   - MaxDuration: a wall-clock budget for the window, measured
//     from when controls were engaged (or the last ResetBudget).
//     A call that starts after the budget is spent is refused
//     before dispatch. 0 = unlimited.
//   - Both are enforced in the same choke point as the allowlist.
//
// Cost estimate (honest, never invented):
//   - The MCP tools/call wire format carries no usage or price, so
//     a plain ToolServer can never yield a cost: CostEstimate then
//     reports UsageKnown=false and the cost is UNKNOWN.
//   - A server that DOES know its usage implements the optional
//     UsageReportingServer interface; its token counts are then
//     reported verbatim, and a USD figure only when the server
//     itself reports one (HasCost). There is no price table for
//     external MCP tools anywhere in this codebase, so a USD
//     spend cap cannot be enforced from usage data — MaxCalls and
//     MaxDuration are the real, enforceable caps, and a USD cap
//     would need a per-tool price source that does not exist yet.
//     Nothing here invents a dollar number.
//
// Configurability: Controls is JSON-tagged so it can be embedded
// in config.MCPConfig later without a shape change; the runtime
// API is NewWithControls / SetControls / Controls. Legacy New()
// keeps the pre-M9.6 behaviour (no controls engaged, everything
// allowed) so existing callers — internal/server's agent-loop
// wiring and its tests — keep working until the config-file
// plumbing lands; the hardened path (NewWithControls /
// SetControls) is fail-closed from the moment controls are
// engaged. That split is deliberate and is recorded in LOG.md;
// it is NOT a bypass: any registry built through the hardened
// path denies by default.
package mcptools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrToolNotAllowed is returned (and wrapped) when a tool call is
// refused by the allowlist.
var ErrToolNotAllowed = errors.New("mcptools: tool not allowlisted")

// ErrBudgetExhausted is returned (and wrapped) when a tool call is
// refused by the spend budget (call-count cap or wall-clock
// budget).
var ErrBudgetExhausted = errors.New("mcptools: tool budget exhausted")

// Controls configures the M9.6 client-side hardening for one
// Registry. JSON-tagged so config.MCPConfig can embed it verbatim
// later (config-file plumbing is a follow-up outside this slice's
// file scope; the runtime API here is the source of truth).
type Controls struct {
	// Allow lists the tool patterns that may execute, e.g.
	// "calc__echo", "calc__*", "echo" (any server), "*".
	// When controls are engaged, an empty Allow denies
	// everything (fail-closed).
	Allow []string `json:"allow,omitempty"`
	// Deny lists the tool patterns that may never execute.
	// A Deny match wins over any Allow match.
	Deny []string `json:"deny,omitempty"`
	// MaxCalls caps dispatched tool executions per budget
	// window (0 = unlimited).
	MaxCalls int `json:"max_calls,omitempty"`
	// MaxDuration caps the wall-clock budget of the window,
	// from controls engagement / last ResetBudget (0 =
	// unlimited).
	MaxDuration time.Duration `json:"max_duration,omitempty"`
}

// ToolUsage is what a usage-reporting MCP server knows about one
// tool execution. All fields are verbatim from the server —
// nothing is estimated here. CostUSD is meaningful only when
// HasCost is true; a server that knows tokens but no price
// reports HasCost=false and the USD cost stays UNKNOWN.
type ToolUsage struct {
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	HasCost      bool    `json:"has_cost"`
}

// UsageReportingServer is the optional interface a ToolServer can
// implement when it knows the usage of its tool executions (token
// counts, and a USD cost when it has one). The stdio and static
// servers do not implement it — the MCP wire format carries no
// usage — so their executions report cost UNKNOWN.
type UsageReportingServer interface {
	ToolServer
	CallToolWithUsage(ctx context.Context, name string, args map[string]any) (string, *ToolUsage, error)
}

// CostEstimate is the honest cost report for one tool execution.
// UsageKnown is false when the server reported no usage at all:
// then the cost is UNKNOWN and no number is offered. USDKnown is
// true only when the server itself reported a USD cost.
type CostEstimate struct {
	UsageKnown   bool    `json:"usage_known"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	USDKnown     bool    `json:"usd_known"`
	USD          float64 `json:"usd,omitempty"`
}

// UnknownCost is the CostEstimate for an execution with no usage
// data: cost UNKNOWN, never a fabricated number.
func UnknownCost() CostEstimate { return CostEstimate{} }

// ExecutionResult is one ExecuteDetailed call: the tool's text,
// the honest cost estimate, how long the dispatch took, and which
// (server, tool) ran.
type ExecutionResult struct {
	Text      string        `json:"text"`
	Cost      CostEstimate  `json:"cost"`
	Duration  time.Duration `json:"duration_ns"`
	Server    string        `json:"server"`
	Tool      string        `json:"tool"`
	Qualified string        `json:"qualified_name"`
}

// BudgetStatus is the live state of the spend budget.
type BudgetStatus struct {
	CallsUsed   int           `json:"calls_used"`
	MaxCalls    int           `json:"max_calls,omitempty"`
	Elapsed     time.Duration `json:"elapsed_ns"`
	MaxDuration time.Duration `json:"max_duration_ns,omitempty"`
	Exhausted   bool          `json:"exhausted"`
}

// wildcardMatch reports whether s matches pattern, where "*" in
// the pattern matches any (possibly empty) run of characters.
// Anything else matches literally. An invalid use (none — "*" is
// the only special) cannot occur, so this never errors.
func wildcardMatch(pattern, s string) bool {
	// Classic two-pointer glob with backtracking on "*".
	px, sx := 0, 0
	star, starS := -1, 0
	for sx < len(s) {
		if px < len(pattern) && (pattern[px] == s[sx]) {
			px++
			sx++
		} else if px < len(pattern) && pattern[px] == '*' {
			star = px
			starS = sx
			px++
		} else if star >= 0 {
			px = star + 1
			starS++
			sx = starS
		} else {
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// patternMatches reports whether one allow/deny pattern matches a
// (server, tool) pair. The pattern is tried against the qualified
// "server__tool" name, the plain tool name, and the discovered
// qualified name (which may carry a disambiguation suffix).
func patternMatches(pattern, server, tool, qualified string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if wildcardMatch(pattern, server+"__"+tool) {
		return true
	}
	if wildcardMatch(pattern, tool) {
		return true
	}
	if qualified != "" && qualified != server+"__"+tool && wildcardMatch(pattern, qualified) {
		return true
	}
	return false
}

// allowedByControls is the allowlist decision for one tool. Deny
// wins over Allow; with no Allow match the tool is denied
// (fail-closed). controlsSet=false (legacy New(), controls never
// engaged) allows everything — see the package comment.
func allowedByControls(c Controls, controlsSet bool, server, tool, qualified string) bool {
	if !controlsSet {
		return true
	}
	for _, p := range c.Deny {
		if patternMatches(p, server, tool, qualified) {
			return false
		}
	}
	for _, p := range c.Allow {
		if patternMatches(p, server, tool, qualified) {
			return true
		}
	}
	return false
}

// checkBudget reserves one dispatch against the budget, or
// refuses it. The caller must hold r.budgetMu. The window start
// is set when controls are engaged (or ResetBudget); the call
// count is the number of dispatches already reserved in this
// window. A refused call reserves nothing.
func (r *Registry) checkBudgetLocked(c Controls) error {
	if c.MaxCalls > 0 && r.budgetCalls >= c.MaxCalls {
		return fmt.Errorf("%w: %d tool calls already used (max %d)", ErrBudgetExhausted, r.budgetCalls, c.MaxCalls)
	}
	if c.MaxDuration > 0 && time.Since(r.budgetWindowStart) >= c.MaxDuration {
		return fmt.Errorf("%w: wall-clock budget %s spent (elapsed %s)", ErrBudgetExhausted, c.MaxDuration, time.Since(r.budgetWindowStart).Round(time.Millisecond))
	}
	r.budgetCalls++
	return nil
}

// BudgetStatus returns the live state of the spend budget.
func (r *Registry) BudgetStatus() BudgetStatus {
	r.controlsMu.RLock()
	c := r.controls
	r.controlsMu.RUnlock()
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	st := BudgetStatus{
		CallsUsed:   r.budgetCalls,
		MaxCalls:    c.MaxCalls,
		Elapsed:     time.Since(r.budgetWindowStart),
		MaxDuration: c.MaxDuration,
	}
	st.Exhausted = (c.MaxCalls > 0 && r.budgetCalls >= c.MaxCalls) ||
		(c.MaxDuration > 0 && st.Elapsed >= c.MaxDuration)
	return st
}

// ResetBudget restarts the spend-budget window: the call count
// returns to zero and the wall-clock clock restarts now. The
// allowlist is untouched.
func (r *Registry) ResetBudget() {
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	r.budgetCalls = 0
	r.budgetWindowStart = time.Now()
}

// SetControls engages (or replaces) the controls on this registry
// and restarts the budget window. From this moment the registry
// is fail-closed: an empty Allow list denies every tool. It is
// safe to call concurrently with Execute — the next dispatch
// sees the new controls.
func (r *Registry) SetControls(c Controls) {
	allow := append([]string(nil), c.Allow...)
	deny := append([]string(nil), c.Deny...)
	r.controlsMu.Lock()
	r.controls = Controls{Allow: allow, Deny: deny, MaxCalls: c.MaxCalls, MaxDuration: c.MaxDuration}
	r.controlsSet = true
	r.controlsMu.Unlock()
	r.ResetBudget()
}

// Controls returns a copy of the controls in force and whether
// controls have been engaged (false for a legacy New() registry,
// which allows everything until SetControls/NewWithControls).
func (r *Registry) Controls() (Controls, bool) {
	r.controlsMu.RLock()
	defer r.controlsMu.RUnlock()
	c := r.controls
	c.Allow = append([]string(nil), c.Allow...)
	c.Deny = append([]string(nil), c.Deny...)
	return c, r.controlsSet
}
