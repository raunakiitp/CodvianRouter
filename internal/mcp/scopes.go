// Package mcp — scope system (M9.1, CHECKLIST 382).
//
// Codvian's MCP scopes are OUR OWN set, defined here — not a copy of
// the reference router's 33 scopes. the reference router's shape was studied for reference
// only (open-sse/mcp-server/scopeEnforcement.ts, server.ts): per-tool
// scope assignment, `*` and trailing-`*` wildcard matching, and a
// ScopeCheck result naming the missing scopes. Two deliberate
// divergences from the reference router, both security decisions:
//
//   - Enforcement is ON by default and there is NO off switch on the
//     server. the reference router's enforcement is OFF by default
//     (featureFlags.ts:127-136) and only forced on for narrow HTTP
//     keys; copying that default would leave every tool callable by
//     any client that can reach the transport. Here a tool call with
//     insufficient scopes is denied before dispatch with the
//     scope-denial code -32002 (ScopeDeniedCode) — distinct from the
//     guardrail policy-refusal -32001 and from -32602 (invalid
//     params) — and the denial is written to the audit log.
//   - There is no `manage` super-scope paradox: the reference router's `manage`
//     scope is required by some tools yet is not itself grantable in
//     the same namespace (MCP-SERVER.md:370-374). Our scopes live in
//     one flat `mcp:` namespace; `*` is the only grant that covers
//     everything, and it is granted only to the local operator
//     (see identity.go) — never to a remote key by default.
//
// Scope set (11 scopes). Read scopes are one per existing tool, so a
// key can be narrowed to exactly one tool. Manage scopes are one per
// future tool family, so M9.3/M9.4 tools can be granted family by
// family instead of all-or-nothing. `mcp:destructive` is a SEPARATE
// grant that a destructive tool must require IN ADDITION to its
// family manage scope (AND semantics: every required scope must be
// satisfied) — a key with `mcp:manage:cache` alone can read cache
// stats but can never flush a cache.
package mcp

import (
	"fmt"
	"strings"
	"sync"
)

// ScopeDeniedCode is the JSON-RPC error code for a tool call refused
// for insufficient scopes. It is distinct from the guardrail
// policy-refusal code (-32001) and from -32602 (invalid params):
// a scope denial means the caller is authenticated/identified but
// not authorised for this tool.
const ScopeDeniedCode = -32002

// Scope names. The `mcp:` prefix keeps this namespace disjoint
// from the HTTP gateway-key scopes (guard.go) — an HTTP scope such
// as "chat" never accidentally satisfies an MCP check, and vice
// versa.
const (
	// Read scopes — one per existing read-only tool.
	ScopeReadStatus    = "mcp:read:status"
	ScopeReadProviders = "mcp:read:providers"
	ScopeReadModels    = "mcp:read:models"
	// Read scopes for the M9.3 tool families (added M9.3 — the
		// 9-scope set had manage scopes for routing/cost but no read
		// scopes for them, so a read-only key could not use
		// explain_route at all; see tools_routing.go for the choice).
		ScopeReadRouting = "mcp:read:routing"
		ScopeReadCost    = "mcp:read:cost"
		// P1.2 ecosystem expansion read scopes.
		ScopeReadCapabilities = "mcp:read:capabilities"
		ScopeReadQuota        = "mcp:read:quota"
		ScopeReadBudget       = "mcp:read:budget"
		ScopeReadCostEst      = "mcp:read:cost_est"
		ScopeReadRoutingPlan  = "mcp:read:routing_plan"
		ScopeReadCompression  = "mcp:read:compression"
		ScopeReadEval         = "mcp:read:eval"
		ScopeReadOrg          = "mcp:read:org"
		// Manage scopes — one per future tool family (M9.3/M9.4).
		// A tool declares the family scope(s) it needs; granting a
		// family scope never grants another family.
		ScopeManageRouting = "mcp:manage:routing"
		ScopeManageCost    = "mcp:manage:cost"
		ScopeManageCache   = "mcp:manage:cache"
		ScopeManageMemory  = "mcp:manage:memory"
		ScopeManageOps     = "mcp:manage:ops"
		// P1.2 ecosystem expansion manage scopes.
		ScopeManageEval   = "mcp:manage:eval"
		ScopeManageOrg    = "mcp:manage:org"
		// ScopeDestructive is the separate grant for destructive tools
		// (cache_flush, memory_clear, DB autoRepair — M9.4). A
		// destructive tool requires its family scope AND this scope;
		// neither alone suffices.
		ScopeDestructive = "mcp:destructive"
	)

// AllScopes returns every scope in the set, in a stable order.
func AllScopes() []string {
	return []string{
		ScopeReadStatus,
		ScopeReadProviders,
		ScopeReadModels,
		ScopeReadRouting,
		ScopeReadCost,
		ScopeReadCapabilities,
		ScopeReadQuota,
		ScopeReadBudget,
		ScopeReadCostEst,
		ScopeReadRoutingPlan,
		ScopeReadCompression,
		ScopeReadEval,
		ScopeReadOrg,
		ScopeManageRouting,
		ScopeManageCost,
		ScopeManageCache,
		ScopeManageMemory,
		ScopeManageOps,
		ScopeManageEval,
		ScopeManageOrg,
		ScopeDestructive,
	}
}

// IsKnownScope reports whether s is one of the defined scopes.
// Wildcards (`*`, `mcp:read:*`) are grants, not scopes, and are
// NOT known scopes — they are matched by ScopeMatches, never
// stored as a tool's requirement.
func IsKnownScope(s string) bool {
	for _, known := range AllScopes() {
		if s == known {
			return true
		}
	}
	return false
}

var (
	toolScopesMu sync.RWMutex
	// toolScopes is the per-tool required-scope assignment. The
	// three read-only tools ship with their read scope; M9.3/M9.4
	// register their tools via RegisterToolScopes (in their own
	// files), so this map is the single assignment table and no
	// tool can be added without declaring its scopes.
	toolScopes = map[string][]string{
		"router_status": {ScopeReadStatus},
		"provider_list": {ScopeReadProviders},
		"model_list":    {ScopeReadModels},
	}
)

// RequiredScopes returns the scopes a tool call requires, and
// whether the tool is known. Unknown tools have no required scopes
// — the dispatcher answers -32602 (unknown tool) for them, and the
// scope check is skipped: an unknown tool is not a scope failure,
// and inventing a requirement for it would mask the real error.
// The returned slice is a copy; mutating it cannot change the
// assignment table.
func RequiredScopes(tool string) ([]string, bool) {
	toolScopesMu.RLock()
	defer toolScopesMu.RUnlock()
	scopes, ok := toolScopes[tool]
	if !ok {
		return nil, false
	}
	out := make([]string, len(scopes))
	copy(out, scopes)
	return out, true
}

// RegisterToolScopes assigns the required scopes for a tool. It
// is the seam M9.3/M9.4 use to declare their tools' scopes from
// their own files. Every scope must be a known scope (see
// IsKnownScope) — registering an unknown scope string is a
// programmer error and panics, so a typo can never silently grant
// or deny a tool. Registering with no scopes panics for the same
// reason: every tool must declare what it needs. Re-registering a
// tool that already has scopes panics too (M9.1 SECURITY REVIEW
// N9): silently replacing a tool's scopes — e.g. re-registering
// router_status with a narrower set — would be a quiet privilege
// change, and a panic keeps it loud at startup, consistent with
// the other programmer-error panics here.
func RegisterToolScopes(tool string, scopes ...string) {
	if tool == "" {
		panic("mcp: RegisterToolScopes with empty tool name")
	}
	if len(scopes) == 0 {
		panic(fmt.Sprintf("mcp: RegisterToolScopes(%q) with no scopes — every tool must declare its required scopes", tool))
	}
	for _, s := range scopes {
		if !IsKnownScope(s) {
			panic(fmt.Sprintf("mcp: RegisterToolScopes(%q): unknown scope %q", tool, s))
		}
	}
	toolScopesMu.Lock()
	defer toolScopesMu.Unlock()
	if _, exists := toolScopes[tool]; exists {
		panic(fmt.Sprintf("mcp: RegisterToolScopes(%q): tool already registered — re-registration would silently replace its scopes", tool))
	}
	out := make([]string, len(scopes))
	copy(out, scopes)
	toolScopes[tool] = out
}

// ScopeMatches reports whether one granted scope covers one
// required scope. Matching rules (same shape as the reference router's
// scopeMatches, reimplemented here):
//   - `*` covers everything.
//   - an exact match covers.
//   - a trailing-`*` grant covers any required scope with that
//     prefix (e.g. `mcp:read:*` covers `mcp:read:providers`).
//   - anything else does not cover: a `*` anywhere but the end is
//     literal (e.g. `mcp:*:status` matches nothing), and a parent
//     scope does NOT cover its children (`mcp:read` does not cover
//     `mcp:read:status` — grants must name the scope or use a
//     trailing wildcard explicitly). An empty grant never matches.
func ScopeMatches(granted, required string) bool {
	if granted == "" || required == "" {
		return false
	}
	if granted == "*" || granted == required {
		return true
	}
	if strings.HasSuffix(granted, "*") {
		return strings.HasPrefix(required, granted[:len(granted)-1])
	}
	return false
}

// ScopeCheckResult is the outcome of CheckScopes.
type ScopeCheckResult struct {
	// Allowed is true only when every required scope is covered
	// by at least one granted scope.
	Allowed bool
	// Required is the tool's required scopes, as checked.
	Required []string
	// Granted is the caller's granted scopes, as checked.
	Granted []string
	// Missing lists the required scopes no grant covers, in
	// required order. Empty when Allowed.
	Missing []string
	// Reason is a precise, human-readable denial reason (empty
	// when Allowed). It names scopes only — never secrets, never
	// argument content — so it is safe to return to the caller
	// and to write to the audit log.
	Reason string
}

// CheckScopes decides whether granted scopes satisfy required
// scopes (AND semantics: ALL required scopes must be covered).
// An empty required list allows (a tool with no requirements —
// none exists today, but the rule is total). An empty granted list
// denies any non-empty requirement. The result is deterministic
// and pure, so it is unit-testable without a server.
func CheckScopes(granted, required []string) ScopeCheckResult {
	res := ScopeCheckResult{
		Required: append([]string(nil), required...),
		Granted:  append([]string(nil), granted...),
	}
	if len(required) == 0 {
		res.Allowed = true
		return res
	}
	for _, req := range required {
		covered := false
		for _, g := range granted {
			if ScopeMatches(g, req) {
				covered = true
				break
			}
		}
		if !covered {
			res.Missing = append(res.Missing, req)
		}
	}
	if len(res.Missing) == 0 {
		res.Allowed = true
		return res
	}
	res.Reason = fmt.Sprintf("missing required scope(s): %s", strings.Join(res.Missing, ", "))
	return res
}
