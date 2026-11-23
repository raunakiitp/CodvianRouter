// Route-guard tiers for Codvian Router's admin/management routes.
//
// Every management route is classified into one of three static tiers,
// evaluated before any other auth branch runs (the same shape as
// the reference router's route-guard tiers in its ROUTE_GUARD_TIERS doc — the
// pattern was studied there, the code here is original):
//
//   - Tier 1 LOCAL_ONLY — routes that spawn processes, execute runtime
//     code, or change the system. Reachable ONLY from loopback, checked
//     BEFORE auth runs, by real TCP peer address (RemoteAddr) — never
//     by Host header or X-Forwarded-For. A leaked credential over a
//     non-loopback connection must still not reach a spawn.
//   - Tier 2 ALWAYS_PROTECTED — destructive routes. They always require
//     gateway-key auth, even if auth were ever relaxed for Tier 3.
//   - Tier 3 MANAGEMENT — default: auth required unless explicitly off
//     (the dashboard feeds are explicitly off: they carry no secrets).
//
// Two deliberate differences from the reference router, both stricter:
//   - No manage-scope bypass: there is no carve-out that lets a
//     non-loopback caller with a privileged key reach a LOCAL_ONLY
//     route. Strict loopback, no exceptions.
//   - Loopback alone is not enough for Tier 1: after the loopback
//     check passes, a valid gateway key is still required (except
//     bootstrap, where the one-shot stdout token stands in for the
//     key that does not exist yet).
//
// The classification lives in guardRouteTable, the single source of
// truth. Server.register looks every route up in the table and applies
// the wrappers for its tier; registering a route with no table entry
// panics at startup, so a new route cannot silently land unclassified.
// TestRouteTableSpawnCapableAreLocalOnly walks the table and fails if
// any spawn-capable route is not LOCAL_ONLY. The spawnCapablePatterns
// regex companion (below) covers the dynamic-segment case the table
// cannot: any path under a spawn-capable prefix must be LOCAL_ONLY,
// whatever its table entry says.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// GuardTier is one of the three route-guard tiers.
type GuardTier string

const (
	// TierLocalOnly is Tier 1: loopback-only, checked before auth.
	TierLocalOnly GuardTier = "LOCAL_ONLY"
	// TierAlwaysProtected is Tier 2: gateway-key auth always required.
	TierAlwaysProtected GuardTier = "ALWAYS_PROTECTED"
	// TierManagement is Tier 3: auth required unless explicitly off.
	TierManagement GuardTier = "MANAGEMENT"
)

// GuardRoute is one classified route: its method + path, its tier,
// whether it can spawn processes / execute runtime code, a short
// human-readable reason, and (Tier 3 only) whether it is explicitly
// exempt from auth. Scopes names the gateway-key scopes a caller
// must carry for the route (M1.1): empty means any authenticated
// key suffices (self-introspection, /metrics) or the route is
// exempt; otherwise the key must carry at least one listed scope,
// and scope "admin" implies every scope. The scope table drives
// enforcement in register(): a key that authenticates but lacks
// the route's scope gets 403.
type GuardRoute struct {
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Tier         GuardTier `json:"tier"`
	SpawnCapable bool      `json:"spawn_capable"`
	Description  string    `json:"description"`
	AuthExempt   bool      `json:"auth_exempt"`
	Scopes       []string  `json:"scopes,omitempty"`
	// Bootstrap marks a loopback-only route that takes no gateway
	// key: the fresh-install first-key setup (claims with the
	// one-shot bootstrap token printed at first boot) and the
	// provider-login browser callback (a top-level redirect cannot
	// carry a key; the single-use PKCE state is its credential).
	Bootstrap bool `json:"bootstrap,omitempty"`
}

// guardRouteTable is the authoritative classification of every route
// the server registers. Add a route here first, then register it;
// register() panics if a route has no entry.
var guardRouteTable = []GuardRoute{
	{Method: "GET", Path: "/healthz", Tier: TierManagement, Description: "Liveness probe — no data, no auth.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard", Tier: TierManagement, Description: "Static dashboard UI — no live data of its own.", AuthExempt: true},
	// B3-04 dashboard operator login: the login surface itself.
	// These four routes are classified TierManagement with the
	// admin scope (the scope wrapper enforces it on any key that
	// IS presented), but register() does NOT apply the Tier 3
	// wrapper to them — that wrapper would 401 every login
	// attempt before the password could be checked. Each handler
	// enforces its own credential instead (setup: admin key;
	// login: password + throttle; change: session or admin key;
	// logout: session or key). They are deliberately NOT
	// AuthExempt: the table-driven scope test pins that every
	// non-exempt route 401s without a credential, and each of
	// these does (setup/change: no key or session; login: no or
	// wrong password; logout: no session and no key).
	{Method: "POST", Path: "/dashboard/auth/setup", Tier: TierManagement, Description: "Sets the dashboard operator password for the first time (only while none is set); admin gateway key required. The password is stored only as an Argon2id hash in the data dir; no default password exists.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/dashboard/auth/login", Tier: TierManagement, Description: "Operator login with the dashboard password; on success sets an HttpOnly session cookie (12h). Failed attempts are throttled per peer (5 failures → 60s lockout).", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/dashboard/auth/logout", Tier: TierManagement, Description: "Invalidates the dashboard session cookie; 401 when the caller has neither a session nor a gateway key.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/dashboard/auth/change", Tier: TierManagement, Description: "Changes the dashboard operator password; requires a valid session plus the current password, or an admin gateway key. All sessions are invalidated.", Scopes: []string{"admin"}},
	// PR #51 TOTP endpoints: session + key protected; enroll/confirm/disable/backup-codes mutate state, status is read-only.
	{Method: "GET", Path: "/dashboard/auth/totp/status", Tier: TierManagement, Description: "Reports whether TOTP is enrolled/enabled and how many backup codes remain.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/dashboard/auth/totp/enroll", Tier: TierManagement, Description: "Begins TOTP enrollment: generates a secret and returns an otpauth:// provisioning URI (QR code). Requires a valid session or admin gateway key.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/dashboard/auth/totp/confirm", Tier: TierManagement, Description: "Confirms TOTP enrollment with a valid 6-digit code; activates TOTP and mints single-use backup codes (shown once).", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/dashboard/auth/totp/verify", Tier: TierManagement, Description: "Verifies a TOTP or backup code for login step 2; on success sets the session cookie.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/dashboard/auth/totp/disable", Tier: TierManagement, Description: "Disables TOTP. Requires a valid session plus current password and TOTP/backup code, or an admin gateway key.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/dashboard/auth/totp/backup-codes", Tier: TierManagement, Description: "Regenerates single-use backup codes. Requires a valid session and current password.", Scopes: []string{"admin"}},
	// PR #52 OAuth provider connections: session + key protected; all mutate state.
	{Method: "GET", Path: "/dashboard/oauth/connect/{provider}", Tier: TierManagement, Description: "Starts an OAuth authorization flow for the given provider (github, google, microsoft). Redirects to the provider's authorization URL. Requires CODVIAN_OAUTH_<PROVIDER>_CLIENT_ID/SECRET env vars.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/oauth/callback/{provider}", Tier: TierManagement, Description: "OAuth callback: exchanges the authorization code for tokens and stores the encrypted connection. Requires a valid state cookie (CSRF protection).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/oauth/connections", Tier: TierManagement, Description: "Lists connected OAuth accounts (provider, connected, expires_at, scopes — no tokens).", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/dashboard/oauth/disconnect/{provider}", Tier: TierManagement, Description: "Revokes and deletes the OAuth connection for the given provider.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/providers", Tier: TierManagement, Description: "Live provider facts (verification, wire, health) — no keys, no base URLs.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/overview", Tier: TierManagement, Description: "Live overview KPIs + traffic buckets from the ledger/recorder — counts and totals only, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/models", Tier: TierManagement, Description: "Live model catalog from the registry + per-model request counts — no keys, no base URLs.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/catalog-audit", Tier: TierManagement, Description: "Last committed catalog freshness audit vs live models.dev (audit date + drift counts) — public catalog facts only, no keys, no operator-configured providers.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/release-verify", Tier: TierManagement, Description: "Last recorded `release verify` run (verified tag, archive/SBOM counts, bundle status) — the committed snapshot proof only, never a live re-run or an invented pass.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/models/refresh", Tier: TierManagement, Description: "Re-probes every provider's model list live and returns one result per provider (ok / failed / skipped, with model counts and reasons); a failed provider keeps its last known list. Local-operator dashboard action on the localhost-bound server — carries no secrets.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/logs", Tier: TierManagement, Description: "Recent request attempts from the ledger (provider, model, tokens, cost, latency, outcome) — no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/usage", Tier: TierManagement, Description: "Live spend/token roll-up from the ledger — totals only, no keys, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/keys", Tier: TierManagement, Description: "Gateway-key facts from the config (name, scopes, budget, created) — masked values only, never a raw key or hash.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/auto-route", Tier: TierManagement, Description: "Live auto-router classification for a typed prompt — no provider is called.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/auto-route/last", Tier: TierManagement, Description: "Last real auto/* routing decision, or an honest empty state.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/feedback", Tier: TierManagement, Description: "Feedback-learned routing weights: per-tier signal tallies + nudges + recent signals — counts only, no prompts, no answers, no message content.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/feedback", Tier: TierManagement, Description: "Records one accept/edit/discard/regenerate signal against a served routing decision id (decision id + signal only; no content). Local-operator dashboard action on the localhost-bound server.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/learned", Tier: TierManagement, Description: "Learned auto-router status: off / learning / active, evidence count, held-out calibration error + log-loss, per-tier sample counts — counts only, no prompts, no answers, no message content, never a fake accuracy.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/intent-tiers", Tier: TierManagement, Description: "Intent-tier mappings (fast / balanced / best) to their operator-defined (model, effort) pairs — model names and effort levels only, no keys, no base URLs, no message content.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/intent-tiers", Tier: TierManagement, Description: "Sets one intent tier's (model, effort) mapping at runtime (tier, model and effort names only). Local-operator dashboard action on the localhost-bound server — carries no secrets; applies at runtime only, never to the config file.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/suggestions", Tier: TierManagement, Description: "Metadata-only learning suggestions: pending routing suggestions with their real evidence counts, per-(task, model) latency/success stats and the applied-change log — counts and model names only, no prompts, no answers, no message content.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/suggestions", Tier: TierManagement, Description: "Approves (applies) or reverts one routing suggestion by id — an explicit operator action recorded in the applied-change log. Local-operator dashboard action on the localhost-bound server — carries no secrets; applies at runtime only, never to the config file.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/code-outcomes", Tier: TierManagement, Description: "Auto Code mode evidence: per-model recorded coding-task outcomes (attempts, solved credit, measured solve rate or the documented neutral default) — counts only, no prompts, no answers, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/coalescing", Tier: TierManagement, Description: "Single-flight counters only — no keys, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/reasoning", Tier: TierManagement, Description: "Reasoning replay counters (captures, restores, hits, misses, evictions) — counts only, never reasoning content, no keys, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/responses", Tier: TierManagement, Description: "Live Responses feed: recent response ids with model, status, previous id (the continuation chain), serving provider and creation time, plus store counts — identifiers and counts only, never input or output text.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/responses/tree", Tier: TierManagement, Description: "Responses tree: roots with their branches nested (parent → children, from the ledger tree, or the in-memory store when no ledger is wired) — identifiers and counts only, never input or output text; an empty forest is roots [] with total 0.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/concurrency", Tier: TierManagement, Description: "Adaptive limiter state + TFO probe — no keys, no base URLs.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/pools", Tier: TierManagement, Description: "Warm HTTP/2 pool state per (provider, key) + Little's Law sizing — counters and key fingerprints only, no keys, no base URLs.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/guard", Tier: TierManagement, Description: "Guard-tier table + live guard status — no keys, no secrets.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/audit", Tier: TierManagement, Description: "Audit feed: guard-denial counts (live counters), key-lifecycle events, management POSTs, and the MCP tool-call audit join — counts + redacted identifiers only, no prompts, no keys, no message content. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/mcp", Tier: TierManagement, Description: "MCP tool list + live provider/model counts — no keys, no base URLs.", AuthExempt: true},
	{Method: "GET", Path: "/api/mcp/audit", Tier: TierManagement, Description: "MCP tool-call audit (inputs SHA-256-hashed, outputs truncated to 200 chars — no raw arguments), paginated, newest first. Entries are redacted as stored, so the feed cannot leak a secret.", AuthExempt: true},
	{Method: "GET", Path: "/api/mcp/status", Tier: TierManagement, Description: "MCP remote-transport toggles + live session counts — flags and counts only, no keys, no message content.", AuthExempt: true},
	{Method: "POST", Path: "/api/mcp/status", Tier: TierManagement, Description: "Enables/disables one MCP remote transport (sse/streamable). Local-operator dashboard action on the localhost-bound server — carries no secrets; the transports themselves are served by M9.2.", Scopes: []string{"admin"}},
	// M9.2 MCP remote transports (SSE + Streamable HTTP): Tier
	// MANAGEMENT, no guard-table scope — deliberately scope-free.
	// No HTTP-namespace scope fits: "admin" would lock out narrow
	// MCP-only keys (defeating the transports' purpose, and the
	// guard treats "admin" as implying every scope), while
	// "chat"/"models" would wrongly demand HTTP scopes of an
	// MCP-only key. The guard-table scope governs the TRANSPORT;
	// authorisation inside the transport uses the disjoint mcp:
	// scope system (M9.1), enforced per request by the handlers
	// themselves: a key with no valid mcp: scope is refused 403,
	// and every tool call is denied -32002 outside its scopes.
	// Auth is still required (not AuthExempt) — a keyless request
	// never reaches a tool. These five routes are in the
	// scopeFree allowlist of TestGuardTableScopesComplete.
	{Method: "GET", Path: "/api/mcp/sse", Tier: TierManagement, Description: "MCP SSE transport: opens the server-to-client event stream (first event is the endpoint carrying the session id). Gateway-key auth; the mcp: scopes on the key govern tool calls inside."},
	{Method: "POST", Path: "/api/mcp/sse", Tier: TierManagement, Description: "MCP SSE transport: sends one JSON-RPC message for a session opened by GET; answers 202, the response arriving on the stream. Gateway-key auth; the mcp: scopes on the key govern tool calls inside."},
	{Method: "POST", Path: "/api/mcp/stream", Tier: TierManagement, Description: "MCP Streamable HTTP transport: one JSON-RPC message per POST; an initialize opens a session (Mcp-Session-Id header). Gateway-key auth; the mcp: scopes on the key govern tool calls inside."},
	{Method: "GET", Path: "/api/mcp/stream", Tier: TierManagement, Description: "MCP Streamable HTTP transport: opens the server-to-client stream for a session (closing it does not terminate the session). Gateway-key auth; the mcp: scopes on the key govern tool calls inside."},
	{Method: "DELETE", Path: "/api/mcp/stream/{id}", Tier: TierManagement, Description: "MCP Streamable HTTP transport: terminates one session (204; unknown or foreign ids are 404). Gateway-key auth; the mcp: scopes on the key govern tool calls inside."},
	{Method: "GET", Path: "/dashboard/guardrails", Tier: TierManagement, Description: "Live guardrail state (blocking config, redaction counts, recent redacted events) — no prompts, no keys.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/guardrails", Tier: TierManagement, Description: "Replaces the guardrail blocking config (toggles only; log redaction is always on and not toggleable). Local-operator dashboard action on the localhost-bound server — carries no secrets.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/plugins", Tier: TierManagement, Description: "Live plugin registry: registered plugins, hook points, per-hook call/error/denial/panic counts, recent response shapes — counts only, no prompts, no keys, no response content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/verification", Tier: TierManagement, Description: "Honest verification status: what is proven in this VM vs what still needs CI on the first tag, a live browser, a second host, or a real Docker host — statuses only, no keys, no secrets.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/strategies", Tier: TierManagement, Description: "Live routing strategies (global strategy, per-chain overrides, deadline budget, last live strategy) — no keys, no base URLs, no message content.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/strategies", Tier: TierManagement, Description: "Replaces the global routing strategy and/or per-chain strategy overrides (strategy names only). Local-operator dashboard action on the localhost-bound server — carries no secrets.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/cooldown", Tier: TierManagement, Description: "Live cooldown mode (global + per-chain overrides) + currently cooling/demoted targets with remaining seconds — mode names and counters only, no keys, no base URLs, no message content.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/cooldown", Tier: TierManagement, Description: "Replaces the global cooldown mode and/or per-chain cooldown-mode overrides (mode names only: hard / soft-demote). Local-operator dashboard action on the localhost-bound server — carries no secrets.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/groups", Tier: TierManagement, Description: "Operator-defined routing groups (routing.groups) with live member health — group names, member provider IDs, enabled flags, breaker state and real attempt counters only, no keys, no base URLs, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/stream-lifetime", Tier: TierManagement, Description: "Stream-lifetime cap the engine enforces (effective cap + how many streams it has terminated since this process started) — a duration and a count only, no keys, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/key-budgets", Tier: TierManagement, Description: "Per-key rolling 5-hour USD budgets from the ledger (spent/limit, window, unknown spend) — key ids and names only, never a raw key or hash.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/key-budgets", Tier: TierManagement, Description: "Sets or deletes one key's rolling 5-hour USD budget (key id, limit, unpriced policy only). Local-operator dashboard action on the localhost-bound server — carries no secrets.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/quota-pools", Tier: TierManagement, Description: "Fair-share quota pools from the ledger (dimensions with live pool-wide consumption, member allocations) — pool ids, key ids and counts only, never a raw key or hash.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/quota-pools", Tier: TierManagement, Description: "Creates, replaces or deletes one fair-share quota pool (dimensions, allocations, saturation threshold). Local-operator dashboard action on the localhost-bound server — carries no secrets.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/quota-schedules", Tier: TierManagement, Description: "Quota schedules from the ledger (time windows, keep-free reserves, per-window budgets, live verdict) — pool and schedule ids only, never a raw key or hash.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/quota-schedules", Tier: TierManagement, Description: "Creates, replaces or deletes one quota schedule (allow/block window, reserves, budget). Local-operator dashboard action on the localhost-bound server — carries no secrets.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/internal-budgets", Tier: TierManagement, Description: "Internal scoped budgets from the ledger (scope, period, limit type, live usage and decision) — budget ids only, never a raw key or hash.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/internal-budgets", Tier: TierManagement, Description: "Creates, replaces or deletes one internal budget (scope, period, limit type, value, warning threshold). Local-operator dashboard action on the localhost-bound server — carries no secrets.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/teams", Tier: TierManagement, Description: "Team cost centers from the ledger (members, budget window, list + metered cost) — team and key ids only, never a raw key or hash.", AuthExempt: true},
	{Method: "POST", Path: "/dashboard/teams", Tier: TierManagement, Description: "Creates, replaces or deletes one team cost center (name, budget, duration, status). Local-operator dashboard action on the localhost-bound server — carries no secrets.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/dashboard/teams/{id}/members", Tier: TierManagement, Description: "Assigns a gateway key to a team, or unassigns it. Local-operator dashboard action on the localhost-bound server — carries no secrets.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/request-budgets", Tier: TierManagement, Description: "Per-request USD budget outcomes since server start (refusals pre-send / mid-chain, overages, recent events) — counts and costs only, no keys, no base URLs, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/trace", Tier: TierManagement, Description: "Route trace of the most recently finished request: every attempt in order with outcome + error-taxonomy fallback reason (model_not_found, rate_limited, request_invalid, ...) — redacted error strings only, no keys, no base URLs, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/context-fit", Tier: TierManagement, Description: "Context-window fit-check decision of the most recently finished request that reached the pre-call fit gate: local prompt estimate, response reserve, required tokens, per-model fit verdicts, and which chain served it (primary / context_fallback / none) — token counts and model names only, no keys, no base URLs, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/prices", Tier: TierManagement, Description: "Operator-configured per-(provider, model) prices (USD per 1M tokens) from the config file and/or local price file — unknown prices reported as unknown, never $0.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/benchmark", Tier: TierManagement, Description: "Last measured Geekflare-style bench run from the data dir's benchmark.json (scripts/bench-geekflare.sh output) with per-metric target verdicts — measured values only; recorded=false when no run exists.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/benchmark/history", Tier: TierManagement, Description: "Every recorded Geekflare-style bench run (benchmark-*.json in CODVIAN_BENCH_HISTORY_DIR, oldest first) with kind, clean flag, verdict and passes — recorded files only, never invented; unreadable or corrupt files are listed as skipped, not shown as data.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/memstats", Tier: TierManagement, Description: "Live memory of this router process (Go heap from runtime.MemStats, RSS, goroutines, in-flight requests, GC counters) + ?profile=heap / ?profile=goroutine binary pprof snapshots — live values only, nothing estimated.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/headless", Tier: TierManagement, Description: "Headless-mode status + the exact terminal commands for serve --headless, init --wizard and run <cli> --model — no keys, no secrets.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/readiness", Tier: TierManagement, Description: "Live local-capability readiness: every gateway-provided local capability with its ready state and the Retry-After a client should honour while it is not ready — capability names and states only, no keys, no base URLs.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/streaming", Tier: TierManagement, Description: "Last measured SSE token-delivery run from the data dir's streaming.json (scripts/bench-sse.sh output): median/p99 before (batching off) and after (batching on) at each mock cadence — measured values only, no target verdict; recorded=false when no run exists.", AuthExempt: true},
	{Method: "GET", Path: "/openapi.json", Tier: TierManagement, Description: "OpenAPI spec for the management surface, generated live from this route table. Every operation carries x-loopback-only / x-always-protected / x-spawn-capable extensions matching its tier — the consistency check is TestOpenAPIConsistency.", AuthExempt: true},
	{Method: "GET", Path: "/api/providers", Tier: TierManagement, Description: "Live provider feed — no keys, no base URLs.", AuthExempt: true},
	{Method: "POST", Path: "/api/providers", Tier: TierAlwaysProtected, Description: "Adds a provider: tests the supplied key live against the upstream, encrypts and persists it, and adds it to the live registry with no restart. Auth always required — an unauthenticated add could redirect future LLM traffic to an attacker-chosen base URL.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/providers/{id}", Tier: TierAlwaysProtected, Description: "Removes a provider: deletes it from the live registry and the config file, drops its live engine client, unregisters its models from the auto-router tiers (only models no remaining provider serves), and prunes its probe history. Auth always required, plus the typed confirmation phrase DELETE <id> in the JSON body — an unauthenticated delete could take a provider offline.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/providers/{id}/keys", Tier: TierAlwaysProtected, Description: "Adds an API key to a provider's key pool: tests the key live against the upstream, then encrypts and persists it. Auth always required — it plants a credential.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/providers/{id}/keys/{keyId}", Tier: TierAlwaysProtected, Description: "Removes one API key from a provider's key pool (the last key cannot be removed — remove the provider instead). Auth always required — it drops a credential.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/providers/{id}/keys/{keyId}/role", Tier: TierAlwaysProtected, Description: "Changes one provider key's role (preferred/reserve). Auth always required — it changes which credential routes.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/providers/{id}/keys/{keyId}/rotate", Tier: TierAlwaysProtected, Description: "Rotates one provider key's credential in place: tests the replacement key live against the upstream, then replaces the stored ciphertext keeping the key's ID and role. Auth always required — it plants a credential.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/providers/{id}/enabled", Tier: TierAlwaysProtected, Description: "Enables or disables a provider: a disabled provider stops routing (the dashboard shows it as Not connected) and re-enabling returns it to the pool with no restart. Auth always required — it changes what routes.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/catalog", Tier: TierManagement, Description: "Live provider catalog for the add-provider selector (id, name, verification, ToS-grey flag, base-URL availability) — catalog facts only, no keys, no base URLs.", AuthExempt: true},
	{Method: "GET", Path: "/api/usage/budget", Tier: TierManagement, Description: "Cost summary for one gateway key (apiKeyId query param): calendar limits, warning threshold, reset cadence, last/next reset, spend today / this month / this budget period, the active limit and the allow/warn/deny budget check, plus the rolling 5h cap. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/usage/budget", Tier: TierManagement, Description: "Set the calendar budget for one gateway key: daily/weekly/monthly limits, warning threshold, reset interval/time. Absent fields keep their stored values; the 5h cap and unpriced policy are untouched. Ledger-first, then config persist with rollback. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/usage/budget/forecast", Tier: TierManagement, Description: "Burn-rate forecast for one gateway key (apiKeyId query param, B12): active window limit, period spend, 1h/24h burn rates and request counts from the ledger, and a projected exhaustion time. Computed only from real ledger spend + the stored limit; reports insufficient_data/exhausted instead of inventing a number when data is missing. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	// M8.17 analytics (read-only over stored ledger rows for one
	// calendar window ?window=day|week|month, default day; all
	// aggregate spend across all gateway keys — operator-only).
	{Method: "GET", Path: "/api/analytics/summary", Tier: TierManagement, Description: "Usage analytics summary for one calendar window: request/token/latency totals, priced spend + cost_status. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/by-provider", Tier: TierManagement, Description: "Usage analytics roll-up by provider for one calendar window, with priced cost and unpriced-request count. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/by-model", Tier: TierManagement, Description: "Usage analytics roll-up by model for one calendar window, with priced cost and unpriced-request count. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/costs", Tier: TierManagement, Description: "Priced spend + unpriced remainder for one calendar window; deliberately no total-cost field. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/compression", Tier: TierManagement, Description: "Compression savings roll-up by provider for one calendar window (prompt tokens before compression, tokens saved, saving rate). Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/diversity", Tier: TierManagement, Description: "Traffic diversity for one calendar window: provider/model spread with Herfindahl concentration index. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/auto-routing", Tier: TierManagement, Description: "Auto-routed traffic for one calendar window, grouped by variant, with serving providers. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/combo-health", Tier: TierManagement, Description: "Per-combo health for one calendar window: step liveness from the live provider config plus matching usage requests (upper bound — a row served through a combo is indistinguishable from one served directly). Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/fallback/chains", Tier: TierManagement, Description: "Fallback chains: every stored (model → provider priority list) chain, or one chain with ?model=. Provider and model names plus priorities only — no keys, no message content. Gateway-key auth required.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/fallback/chains", Tier: TierManagement, Description: "Creates or updates one fallback chain (model + ordered provider entries). Management auth (admin scope) required — it rewrites routing state.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/fallback/chains", Tier: TierManagement, Description: "Deletes one fallback chain by ?model=; 404 when the model has no chain. Management auth (admin scope) required — it rewrites routing state.", Scopes: []string{"admin"}},
	// M8.16 resilience / logs / guardrails-test (read-only or
	// probe; nothing destructive).
	{Method: "GET", Path: "/api/resilience", Tier: TierManagement, Description: "Live resilience feed: breaker states + cooldowns per provider (unknown when unprobed); hedge counts honestly unknown. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	// M10 B6 resilience settings read/patch + breaker reset +
	// per-model cooldowns.
	{Method: "GET", Path: "/api/resilience/settings", Tier: TierManagement, Description: "Live resilience settings: effective cooldown mode (global + per-model overrides), cooling targets and breaker states from the engine, plus the list of settings with no runtime backing. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/resilience/settings", Tier: TierManagement, Description: "Updates resilience settings: cooldown mode (global + per-model overrides) is applied live and persisted; breaker_threshold, breaker_open_for_seconds and cooldown_seconds are validated for range first (400 on a bad range) and then answer 501 naming the field — they have no runtime backing. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/resilience/breakers/reset", Tier: TierManagement, Description: "Resets one provider's circuit breaker, or all breakers with {\"all\": true}; unknown provider is 404. A real reset against the production engine — router.Engine implements ResetBreaker/ResetAllBreakers (internal/router/router.go) — 501 only when the engine lacks the breaker-reset interface. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/resilience/cooldowns", Tier: TierManagement, Description: "Sets one model's cooldown mode (hard / soft-demote); an empty mode is 400 — clearing an override is DELETE /api/resilience/cooldowns/{model...}. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/resilience/cooldowns/{model...}", Tier: TierManagement, Description: "Clears one model's cooldown-mode override so it inherits the global mode again; idempotent — a model with no override still answers 200. The model id uses the multi-segment wildcard because model ids contain a slash. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/logs", Tier: TierManagement, Description: "Recent server log lines, double-redacted, newest first (?limit, default 50, max 500); captured:false with an empty list until log capture is wired. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/guardrails/test", Tier: TierManagement, Description: "Dry-run probe of the local guardrails against sample text: findings + would-block verdict; records nothing, sends nothing upstream. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	// M8.18 teams + quota-pools (operator state, no credentials,
	// cannot redirect traffic — Tier 3, not Tier 2).
	{Method: "GET", Path: "/api/teams", Tier: TierManagement, Description: "List team cost centers with live budget/member/consumption views. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/teams", Tier: TierManagement, Description: "Create a team cost center (name, budget, duration, members). Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/teams/{id}", Tier: TierManagement, Description: "One team cost center by id; unknown ids answer 404. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/teams/{id}", Tier: TierManagement, Description: "Update one team cost center (budget, members, status). Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/teams/{id}", Tier: TierManagement, Description: "Delete one team cost center. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/quota-pools", Tier: TierManagement, Description: "List fair-share quota pools with live pool-wide consumption. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/quota-pools", Tier: TierManagement, Description: "Create a fair-share quota pool (dimensions, allocations, saturation threshold). Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/quota-pools/{id}", Tier: TierManagement, Description: "One fair-share quota pool by id; unknown ids answer 404. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/quota-pools/{id}", Tier: TierManagement, Description: "Update one fair-share quota pool (dimensions, allocations). Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/quota-pools/{id}", Tier: TierManagement, Description: "Delete one fair-share quota pool. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/quota/intelligence", Tier: TierManagement, Description: "Unified quota intelligence across pools, schedules, key budgets, internal budgets, and catalog free-tier providers. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	// M8.19 webhooks (mutations are Tier 2: they point the server
	// at an operator-chosen URL / drop an alert channel).
	{Method: "POST", Path: "/api/webhooks", Tier: TierAlwaysProtected, Description: "Registers a webhook URL + event list (quota.exceeded, budget.warning): validates the URL (http/https only, no private/loopback targets), mints a wh_ id, persists to the config file and rebuilds the live dispatcher. Auth always required — it points the server at an operator-chosen URL and stores a signing secret.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/webhooks", Tier: TierManagement, Description: "Lists webhook registrations (id, url, events, has_secret only — secrets never returned). Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/webhooks/{id}", Tier: TierManagement, Description: "One webhook registration by id (secret never returned); unknown id is 404. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/webhooks/{id}", Tier: TierAlwaysProtected, Description: "Removes a webhook registration, persists the config file and rebuilds the live dispatcher; unknown id is 404. Auth always required — it silently drops an operator alert channel.", Scopes: []string{"admin"}},
	{Method: "PUT", Path: "/api/webhooks/{id}", Tier: TierAlwaysProtected, Description: "Updates a webhook registration (partial update: url, events and secret are each optional, only provided fields change; the URL is re-validated when changed). Persists to the config file and rebuilds the live dispatcher; unknown id is 404. Auth always required — it can re-point an operator alert channel.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/usage/om-usage", Tier: TierManagement, Description: "Terminal-friendly usage command for the calling gateway key (@@om-usage): spend today / this week / this month, the active limit and next reset — plain text by default, JSON with ?format=json. Only for keys the operator enabled allow_usage_command on (403 otherwise). Gateway-key auth required."},
	{Method: "GET", Path: "/api/cache/stats", Tier: TierManagement, Description: "Live cache analytics — per-layer entries/bytes/hits/misses/hit-ratio for the exact + semantic caches, totals, single-flight coalescing counters, and aggregated savings (tokens + USD) when a savings recorder is wired; savings omitted with a note when not. Counts only, no keys, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/api/telemetry/model-latency-stats", Tier: TierManagement, Description: "Per-(provider, model) measured latency percentiles p50/p95/p99 from recorded request samples, with sample counts; optional ?provider= / ?model= exact-match filters. Read-only counts and latencies, no keys, no message content.", AuthExempt: true},
	// M8.15 pricing (read) + cache management (delete).
	{Method: "GET", Path: "/api/pricing", Tier: TierManagement, Description: "Operator price book: per-(provider, model) rates with their source (ledger, book, synced, model fallback). Read-only, rates and sources only, no secrets.", AuthExempt: true},
	{Method: "PATCH", Path: "/api/pricing", Tier: TierAlwaysProtected, Description: "Replaces price-book rates for (provider, model) pairs (all-or-nothing, validated, persisted to the config file with rollback). Auth always required — it changes what every request is charged.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/cache", Tier: TierAlwaysProtected, Description: "Flushes every cache layer (exact, semantic, reasoning) and reports per-layer evicted counts. Auth always required — it discards cached answers.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/metrics", Tier: TierManagement, Description: "Prometheus exposition — gateway-key auth required."},
	{Method: "GET", Path: "/v1/models", Tier: TierManagement, Description: "Model list — gateway-key auth required.", Scopes: []string{"models"}},
	// M8.10 model detail / query / health (scope "models", same
	// as GET /v1/models). The {model...} wildcard matches the
	// remaining segments, so virtual models with "/" in their id
	// (auto/cheap) are reachable; the literal query/health
	// patterns still win over the wildcard.
	{Method: "GET", Path: "/v1/models/query", Tier: TierManagement, Description: "Model list filtered by provider and free-tier-only; capability filter answers 400 — the catalog records no per-model capability flags. Gateway-key auth required.", Scopes: []string{"models"}},
	{Method: "GET", Path: "/v1/models/health", Tier: TierManagement, Description: "Per-model health from the health-probe ring — each model's status is its provider's last probe result; unprobed models report unknown. Gateway-key auth required.", Scopes: []string{"models"}},
	{Method: "GET", Path: "/v1/models/{model...}", Tier: TierManagement, Description: "Single model detail — catalog entry for one model (price, base-URL presence, free-tier flag); unknown models answer 404. Gateway-key auth required.", Scopes: []string{"models"}},
	// M8.12 per-provider routes (pin = routing.only + delegate
	// to handleChatCompletions; models/status are reports).
	{Method: "POST", Path: "/v1/providers/{provider}/chat/completions", Tier: TierManagement, Description: "Chat completions pinned to one provider — gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/providers/{provider}/models", Tier: TierManagement, Description: "Model list for one provider — gateway-key auth required.", Scopes: []string{"models"}},
	{Method: "GET", Path: "/v1/providers/{provider}/status", Tier: TierManagement, Description: "Status for one provider (configured, key count, breaker, cooling) — gateway-key auth required.", Scopes: []string{"models"}},
	// M8.13 Gemini-native surface (list + generate/stream via
	// the {rest...} action route).
	{Method: "GET", Path: "/v1beta/models", Tier: TierManagement, Description: "Gemini-native model list — gateway-key auth required.", Scopes: []string{"models"}},
	{Method: "POST", Path: "/v1beta/models/{rest...}", Tier: TierManagement, Description: "Gemini-native generateContent / streamGenerateContent — gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/chat/completions", Tier: TierManagement, Description: "Chat completions — gateway-key auth required.", Scopes: []string{"chat"}},
	// C2: WebSocket streaming endpoint — /v1/ws upgrades to WebSocket
	// and streams chat completions over WebSocket frames. Gateway-key
	// auth required with chat scope.
	{Method: "GET", Path: "/v1/ws", Tier: TierManagement, Description: "WebSocket chat streaming endpoint — upgrades to WebSocket and streams chat completions over WS frames. Gateway-key auth required.", Scopes: []string{"chat"}},
	// M9.8 A2A JSON-RPC endpoint (message/send, message/stream,
	// tasks/get, tasks/cancel + v1.0 SendMessage /
	// SendStreamingMessage aliases): gateway-key auth required,
	// owner-scoped to the caller's key; disabled unless config
	// a2a_enabled is true. Scope "chat": an A2A task IS a chat
	// completion executed through the real router.
	{Method: "POST", Path: "/a2a", Tier: TierManagement, Description: "A2A JSON-RPC endpoint (message/send, message/stream, tasks/get, tasks/cancel + v1.0 SendMessage/SendStreamingMessage aliases) — gateway-key auth required, owner-scoped to the caller's key; disabled unless config a2a_enabled is true.", Scopes: []string{"chat"}},
	// M9.9 A2A operator status: the dashboard's status feed.
	// Tier MANAGEMENT, admin scope — the counts are operator-only
	// aggregates, so a gateway key without admin must never read
	// them. The handler never opens the store while disabled
	// (see A2AStatusHandler in a2a_status.go), so a disabled
	// endpoint creates no tasks.db.
	{Method: "GET", Path: "/api/a2a/status", Tier: TierManagement, Description: "A2A operator status: enabled flag, live per-state task counts (operator-only aggregates across all owners), the six skill ids and the protocol version. Management auth (admin scope) required; while A2A is disabled it answers enabled:false with all-zero counts and never touches the store.", Scopes: []string{"admin"}},
	// M9.9 A2A agent card: GET /.well-known/agent.json is PUBLIC
	// (AuthExempt, no scopes) by design, not by omission. An
	// agent card is a discovery document: the A2A spec serves it
	// unauthenticated so a client agent can learn the agent's
	// identity, endpoint URL and auth requirements before it can
	// possibly present a credential ("no authentication is
	// required to fetch the card itself, even if the agent's A2A
	// endpoint requires authentication"); gating it would break
	// discovery for exactly the clients it exists to inform.
	// the reference router's card route is the same shape — a bare GET that
	// builds and returns the card with no auth check in the
	// route (src/app/.well-known/agent.json/route.ts). There is
	// nothing to protect here anyway: the card carries only the
	// static document (name, description, url, version, modes,
	// capabilities, the six skill descriptors) — no task data,
	// no keys, no provider details — and the task endpoint
	// itself (POST /a2a) stays gateway-key authed. One
	// deliberate divergence from the reference router: our card's url is
	// built from the server's configured listen address (see
	// agentCardBaseURL in server.go), never from the request —
	// the reference router derives its base URL from the request, which
	// trusts Host headers a caller can forge.
	{Method: "GET", Path: "/.well-known/agent.json", Tier: TierManagement, Description: "A2A agent card (discovery document for the POST /a2a task interface): name, description, url, version, input/output modes, capabilities and the six skill descriptors — public, no auth; no task data, no keys, no provider details.", AuthExempt: true},
	{Method: "POST", Path: "/v1/embeddings", Tier: TierManagement, Description: "Embeddings — OpenAI-compatible, routed through the same engine as chat completions (budgets, guardrails, ledger, route trace) but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/multimodal-embeddings", Tier: TierManagement, Description: "Multimodal embeddings (text + image/media inputs) — routed through the engine, replay-exempt. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/rerank", Tier: TierManagement, Description: "Rerank — Cohere/LiteLLM shape (model, query, documents, top_n, return_documents), routed through the same engine as chat completions but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.", Scopes: []string{"chat"}},
	// M8.2 legacy completions + routing introspection.
	{Method: "POST", Path: "/v1/completions", Tier: TierManagement, Description: "Legacy completions — gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/relay/chat/completions", Tier: TierManagement, Description: "Chat completions explicitly routed with context-relay strategy. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/classify", Tier: TierManagement, Description: "Routing classification — gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/explain/routing", Tier: TierManagement, Description: "Routing explanation — gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/segment", Tier: TierManagement, Description: "Prompt segmentation + classification — gateway-key auth required.", Scopes: []string{"chat"}},
	// M8.5 images (generations + edits only; variations is retired
	// and /v1/images/upscale is not an OpenAI endpoint).
	{Method: "POST", Path: "/v1/images/generations", Tier: TierManagement, Description: "Images generations — OpenAI-compatible, routed through the same engine as chat completions (budgets, guardrails, ledger, route trace) but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/images/edits", Tier: TierManagement, Description: "Images edits — OpenAI-compatible multipart/form-data, forwarded verbatim (never decoded and rebuilt), routed through the same engine as chat completions but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.", Scopes: []string{"chat"}},
	// M8.6 audio (canonical ops + the reference router alias names).
	{Method: "POST", Path: "/v1/audio/speech", Tier: TierManagement, Description: "Audio speech (text-to-speech) — OpenAI-compatible, routed through the same engine as chat completions but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/audio/text-to-speech", Tier: TierManagement, Description: "Alias of POST /v1/audio/speech — not an OpenAI endpoint; gateway route name, same handler and operation. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/audio/text-to-speech/{voiceId}", Tier: TierManagement, Description: "Text-to-speech with the voice id in the URL path: the id is written into the JSON body's voice field (a conflicting body voice is refused 400), then the request follows POST /v1/audio/speech. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/audio/transcriptions", Tier: TierManagement, Description: "Audio transcriptions (speech-to-text) — OpenAI-compatible multipart upload, replay-exempt: exactly one upstream attempt. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/audio/speech-to-text", Tier: TierManagement, Description: "Alias of POST /v1/audio/transcriptions — not an OpenAI endpoint; gateway route name, same handler and operation. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/audio/translations", Tier: TierManagement, Description: "Audio translations (translate audio to English) — OpenAI-compatible multipart upload, replay-exempt: exactly one upstream attempt. Gateway-key auth required.", Scopes: []string{"chat"}},
	// M8.9 moderations (local guardrails only; scope "chat" per
	// reviewer verdict — "models" is read-only listings only).
	{Method: "POST", Path: "/v1/moderations", Tier: TierManagement, Description: "Moderations — OpenAI-shaped, backed ONLY by the router's local guardrails (PII / secret / injection patterns); no upstream call, no input stored or echoed. The 13 OpenAI categories are not judged (all false, score 0); the real verdict is in results[].local_guardrails and results[].flagged reflects it. Gateway-key auth required.", Scopes: []string{"chat"}},
	// M8.20 media verify (search / OCR / video / music).
	{Method: "POST", Path: "/v1/search", Tier: TierManagement, Description: "Search — Perplexity Search API passthrough, verbatim, replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/web/fetch", Tier: TierManagement, Description: "Fetches and extracts clean text from a web URL with SSRF protection. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/ocr", Tier: TierManagement, Description: "OCR — Mistral OCR API passthrough, verbatim, replay-exempt: exactly one upstream attempt. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/videos/generations", Tier: TierManagement, Description: "Video generations — Runway text-to-video passthrough, returns a task id; replay-exempt: exactly one upstream attempt. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/videos/{id}", Tier: TierManagement, Description: "Video task status — Runway task poll by id; unknown ids answer 404. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/music/generations", Tier: TierManagement, Description: "Music generations — MiniMax music generation passthrough, synchronous; replay-exempt: exactly one upstream attempt. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/messages", Tier: TierManagement, Description: "Anthropic Messages dialect — byte-for-byte native relay to a Messages provider (only the model field rewritten; cache_control and the SSE envelope pass through untouched), translate path through the engine when a feature needs the canonical form. Gateway-key auth required.", Scopes: []string{"chat"}},
	// M8.3 token counting (local counting only, no upstream call).
	{Method: "POST", Path: "/v1/messages/count_tokens", Tier: TierManagement, Description: "Token count for a Messages request body — counted locally, never sent upstream. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/responses", Tier: TierManagement, Description: "OpenAI Responses API — translated to the canonical chat request and routed through the same engine as chat completions (failover, budgets, guardrails, ledger); creates fail over pre-commit, including branched continuations, which replay the same branched input against the next target (the original provider is tried first). Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/responses/{id}", Tier: TierManagement, Description: "Retrieve one served Responses object by gateway response id — the stored object, byte-identical to what the create served, plus child_response_ids (the branches off it, from the ledger tree); unknown ids answer 404. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/responses", Tier: TierManagement, Description: "List served Responses objects, newest first, paginated (limit + after cursor) — the full stored objects, byte-identical to what each create served; an unknown cursor is a 400. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "DELETE", Path: "/v1/responses/{id}", Tier: TierManagement, Description: "Deletes one stored response (transcript + served object) so continuations of it answer 404; deleting an unknown id is an honest 404, never a silent success. Gateway-key auth required.", Scopes: []string{"chat"}},
	// M8.4 responses sub-action + M8.3 token counting (stored).
	{Method: "GET", Path: "/v1/responses/{id}/input_items", Tier: TierManagement, Description: "Input items of one stored Responses object, paginated (limit + after cursor, asc/desc); unknown ids answer 404. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/responses/input_tokens", Tier: TierManagement, Description: "Token count for a stored response's input — counted locally, never sent upstream. Gateway-key auth required.", Scopes: []string{"chat"}},
	// M8.7 files (single dispatcher handleFiles; 405 for wrong
	// methods on a shared path).
	{Method: "POST", Path: "/v1/files", Tier: TierManagement, Description: "Upload a file (multipart form-data: file + purpose) to the local disk store; returns the OpenAI-shaped file object.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/files", Tier: TierManagement, Description: "List stored files, newest first.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/files/{id}", Tier: TierManagement, Description: "Retrieve one stored file's metadata by id; unknown ids answer 404.", Scopes: []string{"chat"}},
	{Method: "DELETE", Path: "/v1/files/{id}", Tier: TierManagement, Description: "Delete one stored file (content + index entry); unknown ids answer 404, never a silent success.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/files/{id}/content", Tier: TierManagement, Description: "Download one stored file's bytes; unknown ids answer 404.", Scopes: []string{"chat"}},
	// M8.8 batches (single dispatcher handleBatches; 405 for wrong
	// methods on a shared path). Scope "chat" throughout: a batch
	// IS a set of chat completions executed through engine.Chat,
	// and the Files/Responses analogues scope every route "chat".
	{Method: "POST", Path: "/v1/batches", Tier: TierManagement, Description: "Create a batch from an input file id (endpoint must be /v1/chat/completions, completion_window must be 24h); returns the Batch object in status validating.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/batches", Tier: TierManagement, Description: "List the caller's batches, newest first (?limit= default 20, max 100).", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/batches/{id}", Tier: TierManagement, Description: "Retrieve one batch by id; unknown or foreign ids answer 404.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/batches/{id}/cancel", Tier: TierManagement, Description: "Cancel a running batch: cancelling -> cancelled, the worker stops starting new lines; already-terminal batches answer 409.", Scopes: []string{"chat"}},
	{Method: "DELETE", Path: "/v1/batches/{id}", Tier: TierManagement, Description: "Delete one terminal batch record; non-terminal batches answer 409, unknown or foreign ids answer 404.", Scopes: []string{"chat"}},
	{Method: "DELETE", Path: "/v1/batches/delete-completed", Tier: TierManagement, Description: "Delete all of the caller's terminal (completed/cancelled/failed/expired) batch records; in-progress batches are never deleted.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/me/status", Tier: TierManagement, Description: "Authenticated gateway-key introspection: the caller's key id/fingerprint, created/expiry, spend this period, budget remaining, traffic limits and live usage — never the key value itself. Gateway-key auth required; an expired key answers 401 naming the expiry."},
	{Method: "GET", Path: "/v1/me/sessions", Tier: TierManagement, Description: "List agent sessions opened by the calling API key: Responses chains (one row per session root, no previous_response_id, with turns, cumulative tokens and the list-price cost of its requests) plus ledger agent sessions (one row per coding-agent session, with request/error counts, token split and record-time cost). unpricedCount counts turns/requests with no known catalog price. Every row is filtered to the calling key — other keys' sessions are never listed. Gateway-key auth required."},
	// M8.11 session detail + pre-flight quota check.
	{Method: "GET", Path: "/v1/me/sessions/{id}", Tier: TierManagement, Description: "One agent session of the calling API key: a Responses session (the session root's turns, cumulative tokens and list-price cost, plus the per-turn detail) or a ledger agent session (request/error counts, token split, record-time cost, no per-turn detail); unknown ids, other keys' sessions and keyless records answer 404. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "POST", Path: "/v1/quotas/check", Tier: TierManagement, Description: "Pre-flight quota/budget check for one (provider, model, estimated tokens) request: allowed plus the budget, token-limit and price numbers the verdict was judged against, from the same ledger machinery that enforces live requests. Gateway-key auth required.", Scopes: []string{"chat"}},
	{Method: "GET", Path: "/v1/auto-combo/{channel}/candidates", Tier: TierManagement, Description: "Auto-combo candidate pool for one auto/* channel: live tier models with per-key connections and breaker/cooldown state — a read-only transparency listing, never a dispatch preview. Gateway-key auth required.", Scopes: []string{"models"}},
	// M8.14 combos / chains CRUD (one shared ComboStore; GETs are
	// read-only listings, mutations rewrite routing state — admin
	// scope on mutations per reviewer verdict).
	{Method: "GET", Path: "/v1/combos", Tier: TierManagement, Description: "List named routing combos (chains): one row per combo with steps, strategy and usability. Gateway-key auth required.", Scopes: []string{"models"}},
	{Method: "POST", Path: "/v1/combos", Tier: TierManagement, Description: "Create a named routing combo (ordered provider/model steps + strategy), mirrored into the routing groups so it routes immediately. Management auth (admin scope) required — it rewrites routing state.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/v1/combos/{id}", Tier: TierManagement, Description: "One named routing combo by id; unknown ids answer 404. Gateway-key auth required.", Scopes: []string{"models"}},
	{Method: "PATCH", Path: "/v1/combos/{id}", Tier: TierManagement, Description: "Update one named routing combo (chain, strategy, rename). Management auth (admin scope) required — it rewrites routing state.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/v1/combos/{id}", Tier: TierManagement, Description: "Delete one named routing combo (record + group mirror + strategy mirror). Management auth (admin scope) required — it rewrites routing state.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/v1/combos/import", Tier: TierManagement, Description: "Imports named routing combos from an export file (one at a time); imported combos are mirrored into routing groups like created ones. Management auth (admin scope) required — it rewrites routing state.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/v1/combos/export", Tier: TierManagement, Description: "Exports all named routing combos in the importable shape. Gateway-key auth required.", Scopes: []string{"models"}},
	{Method: "GET", Path: "/v1/combos/{id}/export", Tier: TierManagement, Description: "Exports one named routing combo in the importable shape; unknown ids answer 404. Gateway-key auth required.", Scopes: []string{"models"}},
	{Method: "POST", Path: "/v1/combos/{id}/duplicate", Tier: TierManagement, Description: "Duplicates one named routing combo under a new id (disabled copies stay unmirrored). Management auth (admin scope) required — it rewrites routing state.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/v1/combos/{id}/test-run", Tier: TierManagement, Description: "Simulates one combo run step by step (provider key presence per step); never calls upstream, never spends quota. Gateway-key auth required.", Scopes: []string{"models"}},
	{Method: "POST", Path: "/v1/combos/{id}/enable", Tier: TierManagement, Description: "Enables one named routing combo and re-mirrors its stored chain + strategy. Management auth (admin scope) required — it rewrites routing state.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/v1/combos/{id}/disable", Tier: TierManagement, Description: "Disables one named routing combo (unmirrors it from routing). Management auth (admin scope) required — it rewrites routing state.", Scopes: []string{"admin"}},

	// Tier 1 — LOCAL_ONLY (spawn-capable / system-changing).
	{Method: "POST", Path: "/admin/shutdown", Tier: TierLocalOnly, SpawnCapable: true, Description: "Terminates the server process.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/restart", Tier: TierLocalOnly, SpawnCapable: true, Description: "Graceful restart: answers 202, stops the HTTP server, flushes caches, then starts a replacement process (under the tray supervisor: exits with the restart code and the supervisor relaunches the gateway; unsupervised: re-launches the same binary with the same arguments). 503 when the process has no restart hook.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/service/install", Tier: TierLocalOnly, SpawnCapable: true, Description: "Unavailable: answers 501 feature_unavailable, does nothing (system service installation is not supported in this build). Use instead: `codvian-router restart` starts a supervised background gateway.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/plugin/load", Tier: TierLocalOnly, SpawnCapable: true, Description: "Unavailable: answers 501 feature_unavailable, does nothing (dynamic plugin loading is unsupported by design: plugins are compiled-in and enabled from config at startup (docs/plugins.md)).", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/update", Tier: TierLocalOnly, SpawnCapable: true, Description: "Update check: asks the GitHub releases API for the latest release and answers {current, latest, release_available, update_available, release_url}. When the repository exists but has no published release it answers 200 with latest null, release_available false and status no_published_release (no tag or URL invented). Failures carry their kind in error.code: 503 rate_limited (with Retry-After), 504 timeout, 502 for network, auth, upstream 5xx, malformed response or repository not found. It never downloads or installs anything; installing is `codvian-router update` (signature + checksum verified). 503 when no update checker is wired.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/tunnel/start", Tier: TierLocalOnly, SpawnCapable: true, Description: "Starts the local TCP tunnel: forwards a loopback port to the gateway's listen address (real TCP proxy, loopback-only, stdlib only).", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/tunnel/stop", Tier: TierLocalOnly, SpawnCapable: true, Description: "Stops the running local TCP tunnel and reports the bytes it forwarded.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/admin/tunnel/status", Tier: TierLocalOnly, SpawnCapable: true, Description: "Live tunnel status (running/stopped, ports, bytes forwarded) — loopback-only like the other tunnel routes.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/provider/login", Tier: TierLocalOnly, SpawnCapable: true, Description: "Starts a provider account login flow (Phase 1: OpenRouter OAuth PKCE) and returns {provider, mode, authorization_url, state, expires_at}. The operator opens the authorization URL in a browser; the exchanged API key is live-tested and stored encrypted through the provider registry (into the existing OpenRouter key pool when one is configured). Loopback-only + admin gateway key. The code_verifier never leaves the server; no secret is logged or returned.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/admin/provider/login/callback", Tier: TierLocalOnly, SpawnCapable: true, Description: "Browser redirect target of the provider login flow: exchanges the one-time authorization code (bound to the PKCE state issued by POST /admin/provider/login) for the user's OpenRouter API key and stores it through the registry. Loopback-only WITHOUT a gateway key — a top-level browser navigation cannot carry one; the single-use 256-bit state plus PKCE is the credential, and off-host requests are refused 403 before anything else. Answers a small HTML result page; no code, state, verifier or key is logged or rendered.", Bootstrap: true},
	{Method: "POST", Path: "/admin/bootstrap", Tier: TierLocalOnly, SpawnCapable: true, Description: "Fresh-install first-key setup — only while no gateway key exists; claims with the one-shot bootstrap token printed to stdout at first boot (loopback-only).", Bootstrap: true},

	// Tier 2 — ALWAYS_PROTECTED (destructive; auth always required).
	{Method: "POST", Path: "/admin/settings/export", Tier: TierAlwaysProtected, Description: "Exports the full settings snapshot. Secrets are redacted and never exported — provider keys and gateway-key hashes stay on disk. Requires gateway-key auth AND the typed confirmation phrase EXPORT SETTINGS (the operator-credential second check).", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/db/export", Tier: TierAlwaysProtected, Description: "Exports the config + ledger usage history as a downloadable JSON file. All secrets redacted (no provider keys, no gateway-key hashes, no base URLs); usage history is read-only, never reset. Requires gateway-key auth AND the typed confirmation phrase EXPORT.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/db/wipe/arm", Tier: TierAlwaysProtected, Description: "Issues a single-use DB-wipe confirmation token (5-minute TTL, stored hashed, never logged). Requires gateway-key auth AND the typed confirmation phrase WIPE DATABASE — the token is the operator-credential second check for the wipe.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/db/wipe", Tier: TierAlwaysProtected, Description: "Irreversibly wipes usage history: ledger usage events, budget running spend, feedback decisions/signals, probe history (ring + file + backup). Providers, provider keys, gateway keys, prices and all other config are preserved. Requires gateway-key auth AND the typed phrase WIPE DATABASE AND the single-use confirmation token from /admin/db/wipe/arm — a key alone, or a key plus phrase, cannot wipe.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/db/import", Tier: TierAlwaysProtected, Description: "Imports a DB export file (codvian-router-db-export/v1) and merges its usage history into the ledger — never resets or wipes existing history; duplicate events are skipped, never double-counted; the file's config section is ignored. Requires gateway-key auth AND the typed confirmation phrase IMPORT.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/keys/revoke-all", Tier: TierAlwaysProtected, Description: "Revokes every gateway key (including the caller's) and persists the empty key list; bootstrap becomes pending so a new first key can be issued loopback-only. Requires the typed confirmation phrase REVOKE ALL.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/keys/{id}", Tier: TierAlwaysProtected, Description: "Updates one gateway key's expiry and per-key traffic limits (requests-per-minute, tokens-per-minute) and persists the change. Auth always required — it changes which credential can authenticate and how much traffic it may send.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/keys/{id}/rotate", Tier: TierAlwaysProtected, Description: "Regenerates one gateway key: the old key is revoked at rotation and a new key is issued in its place (same name/scopes/budget/limits unless overridden), returned exactly once and never logged. Auth always required — it revokes a credential and mints a new one.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/keys", Tier: TierAlwaysProtected, Description: "Mints one new gateway key via config.IssueKey and persists it; the raw key is returned once in the response and never logged. Auth always required — it creates a credential.", Scopes: []string{"admin"}},
	// B3-07 OAuth 2.0 gateway auth (oauth_api.go): the gateway is
	// its own authorization server — clients register here, get a
	// code at /oauth/authorize (PKCE S256), and redeem it for a
	// JWT at /oauth/token.
	{Method: "POST", Path: "/oauth/clients", Tier: TierAlwaysProtected, Description: "Registers one OAuth 2.0 client (PKCE public or confidential) for gateway auth; a confidential client's secret is returned once in the response and never logged. Auth always required — it creates a credential.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/oauth/clients", Tier: TierAlwaysProtected, Description: "Lists registered OAuth clients (no secrets). Auth always required.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/oauth/clients/{id}", Tier: TierAlwaysProtected, Description: "Deletes one OAuth client and revokes its outstanding codes and refresh tokens. Auth always required — it destroys a credential.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/oauth/authorize", Tier: TierAlwaysProtected, Description: "OAuth 2.0 authorization endpoint (code + PKCE S256): the operator's gateway key approves the grant; the granted scopes must be a subset of the operator key's scopes. Issues a single-use code by 302 redirect to the client's registered redirect_uri. Auth always required — it mints a credential.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/keys/import", Tier: TierAlwaysProtected, Description: "Stores an externally-issued key definition under its hash; the raw key is never logged and never returned. Auth always required — it installs a credential.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/admin/keys/{id}", Tier: TierAlwaysProtected, Description: "Permanently revokes one gateway key: removes it from the config (persisted) so it stops authenticating at once; the reversible kill-switch is POST /admin/keys/{id} with {\"disabled\":true}. Auth always required — it destroys a credential, plus the typed confirmation phrase DELETE <id>.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/config/reset", Tier: TierAlwaysProtected, Description: "Resets the config to defaults (providers, gateway keys, budget, routing, guardrails, auto-router) and persists it; listen address, data dir and unknown fields are preserved, usage history is not wiped. Requires the typed confirmation phrase RESET CONFIG.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/cache/flush", Tier: TierAlwaysProtected, Description: "Flushes the gateway exact-match cache: an empty body flushes every entry; {\"key_id\"} flushes only that gateway key's entries. Auth always required — it discards cached answers.", Scopes: []string{"admin"}},

	// Tier 3 — MANAGEMENT (auth required unless explicitly off).
	{Method: "GET", Path: "/admin/status", Tier: TierManagement, Description: "Live server status (uptime, provider count, guard state) — gateway-key auth required.", Scopes: []string{"admin"}},
	// B3-07 OAuth token endpoints: public by design (client auth
	// is the PKCE verifier and, for confidential clients, the
	// client secret) — exactly like /healthz, they are AuthExempt.
	{Method: "POST", Path: "/oauth/token", Tier: TierManagement, Description: "OAuth 2.0 token endpoint: authorization_code (PKCE) and refresh_token grants; issues HS256 JWT access tokens (1h) + rotating refresh tokens (30d). No gateway key — client authentication is the code_verifier and the client secret.", AuthExempt: true},
	{Method: "POST", Path: "/oauth/revoke", Tier: TierManagement, Description: "OAuth 2.0 revocation endpoint (RFC 7009): revokes a refresh token or a JWT access token; always answers 200, even for a token that was never issued. No gateway key.", AuthExempt: true},
	{Method: "GET", Path: "/admin/key-templates", Tier: TierManagement, Description: "Built-in gateway-key templates (names, scopes, budgets); secret-free by construction — a template holds no key material. Gateway-key auth required.", Scopes: []string{"admin"}},

	// B8 Dashboard Part 2 routes:
	// b8_settings_api.go
	{Method: "GET", Path: "/api/features", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (these feature flags were not wired to the features they name). Use instead: the config file (each feature's own setting).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/features", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (these feature flags were not wired to the features they name). Use instead: the config file (each feature's own setting).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/cors", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (CORS origin settings are not implemented: the stored list was never applied).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/settings/cors", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (CORS origin settings are not implemented: the stored list was never applied).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/blocked-providers", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (the stored blocked-provider list was never applied). Use instead: POST /api/providers/{id}/enabled (disable a provider).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/settings/blocked-providers", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (the stored blocked-provider list was never applied). Use instead: POST /api/providers/{id}/enabled (disable a provider).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/ai", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (these AI defaults were never applied to requests).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/settings/ai", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (these AI defaults were never applied to requests).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/modality", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (modality bridge settings are not implemented).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/settings/modality", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (modality bridge settings are not implemented).", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/pricing/sync", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (this route reported a sync without fetching any prices). Use instead: PATCH /api/pricing (set rates) or GET /api/pricing (current book).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/pricing/sync/status", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (no pricing sync runs, so there is no status). Use instead: GET /api/pricing.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/transform", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (the transform pipeline is not implemented: stored operations were never applied).", Scopes: []string{"admin"}},
	{Method: "PUT", Path: "/api/settings/transform", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (the transform pipeline is not implemented: stored operations were never applied).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/sidebar", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (sidebar customisation is not implemented: the dashboard never read it).", Scopes: []string{"admin"}},
	{Method: "PUT", Path: "/api/settings/sidebar", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (sidebar customisation is not implemented: the dashboard never read it).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/advanced", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (these advanced settings (body cap, lockout, aliases) were never applied).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/settings/advanced", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (these advanced settings (body cap, lockout, aliases) were never applied).", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/lkgp/cache", Tier: TierAlwaysProtected, Description: "Clears LKGP learned routing cache (B8-69).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/routing/strategies", Tier: TierManagement, Description: "Lists all supported routing strategies (B8-35..56).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/stickiness", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (session stickiness settings are not implemented: stored values were never applied).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/settings/stickiness", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (session stickiness settings are not implemented: stored values were never applied).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/switches", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (these global switches were not wired to anything).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/settings/switches", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (these global switches were not wired to anything).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/response-validation", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (response validation rules are not implemented: stored rules were never evaluated).", Scopes: []string{"admin"}},
	{Method: "PUT", Path: "/api/settings/response-validation", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (response validation rules are not implemented: stored rules were never evaluated).", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/cache/{model}", Tier: TierAlwaysProtected, Description: "Evicts cache entries for a specific model.", Scopes: []string{"admin"}},

	// b8_key_extended.go
	{Method: "POST", Path: "/admin/keys/{id}/disable", Tier: TierAlwaysProtected, Description: "Disables a single gateway key (B8-10).", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/keys/{id}/enable", Tier: TierAlwaysProtected, Description: "Enables a single gateway key (B8-10).", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/keys/{id}/revoke", Tier: TierAlwaysProtected, Description: "Revokes a single gateway key (B8-10).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/admin/keys/{id}/budget", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (this route stored limits in memory and nothing enforced them). Use instead: POST /api/usage/budget (calendar budgets) or POST /dashboard/key-budgets (rolling 5-hour budget); both are enforced.", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/admin/keys/{id}/budget-warn", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (this route stored a threshold in memory and nothing read it). Use instead: POST /api/usage/budget (warning threshold field).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/admin/keys/{id}/budget-reset", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (this route stored a reset window in memory and nothing read it). Use instead: POST /api/usage/budget (reset interval field).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/admin/budget-templates", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (budget templates are not implemented). Use instead: POST /api/usage/budget (calendar budgets) or POST /dashboard/key-budgets (rolling 5-hour budget); both are enforced.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/budget-templates", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (budget templates are not implemented). Use instead: POST /api/usage/budget (calendar budgets) or POST /dashboard/key-budgets (rolling 5-hour budget); both are enforced.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/budget-templates/{id}/apply", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (budget templates are not implemented). Use instead: POST /api/usage/budget (calendar budgets) or POST /dashboard/key-budgets (rolling 5-hour budget); both are enforced.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/keys/{id}/devices", Tier: TierManagement, Description: "Device tracking per key (B8-87).", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/keys/{id}/lease", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (exclusive key leases are not implemented: nothing enforced the stored flag).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/admin/management-tokens", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (management tokens are not implemented: issued tokens never authenticated). Use instead: POST /admin/keys with the admin scope.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/management-tokens", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (management tokens are not implemented: issued tokens never authenticated). Use instead: POST /admin/keys with the admin scope.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/admin/management-tokens/{id}", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (management tokens are not implemented). Use instead: POST /admin/keys with the admin scope.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/admin/management-tokens/{id}", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (management tokens are not implemented). Use instead: DELETE /admin/keys/{id}.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/admin/key-pools", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (key fair-share pools are not implemented: stored pools never affected routing). Use instead: GET /api/quota-pools.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/admin/key-pools", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (key fair-share pools are not implemented). Use instead: POST /api/quota-pools.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/admin/key-pools/{id}", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (key fair-share pools are not implemented). Use instead: GET /api/quota-pools.", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/admin/key-pools/{id}", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (key fair-share pools are not implemented). Use instead: POST /api/quota-pools.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/admin/key-pools/{id}", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (key fair-share pools are not implemented). Use instead: POST /api/quota-pools.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/keys/{id}/routing", Tier: TierManagement, Description: "Per-key routing dashboard page (B8-94).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/dashboard/keys/{id}/access", Tier: TierManagement, Description: "Per-key access configuration page (B8-95).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/ip-filter", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (a global IP filter is not implemented: the stored lists filtered nothing). Use instead: per-key ip_allowlist / ip_blocklist on the gateway key (enforced at auth).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/settings/ip-filter", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (a global IP filter is not implemented: the stored lists filtered nothing). Use instead: per-key ip_allowlist / ip_blocklist on the gateway key (enforced at auth).", Scopes: []string{"admin"}},

	// b8_analytics_extended.go
	{Method: "GET", Path: "/api/analytics/cost-explorer", Tier: TierManagement, Description: "Cost explorer analytics (B8-19).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/spend-projection", Tier: TierManagement, Description: "Monthly spend projection (B8-18,103).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/budget-table", Tier: TierManagement, Description: "Key budget tracking table (B8-105).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/cost-by-key", Tier: TierManagement, Description: "Spend grouped by gateway key (B8-113).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/cost-by-account", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (spend is not partitioned by provider account in the ledger). Use instead: GET /api/analytics/cost-by-key.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/weekly-pattern", Tier: TierManagement, Description: "Weekly usage patterns (B8-115).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/heatmap", Tier: TierManagement, Description: "Activity heatmap (B8-115).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/utilization", Tier: TierManagement, Description: "Provider quota utilization (B8-118).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/search", Tier: TierManagement, Description: "Search analytics (B8-121).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/free-rankings", Tier: TierManagement, Description: "Free model rankings (B8-108).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/scoring-inspector", Tier: TierManagement, Description: "16-factor scoring breakdown (B8-78).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/analytics/candidates", Tier: TierManagement, Description: "Candidate targets for auto-combo (B8-82).", Scopes: []string{"admin"}},

	// b8_logs_extended.go
	{Method: "GET", Path: "/api/logs/proxy", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (no proxy log is recorded). Use instead: GET /dashboard/logs (ledger-backed request log).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/logs/console", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (no console feed is recorded here). Use instead: GET /api/logs (captured server log lines).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/logs/timeline/{id}", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (per-request step timelines are not recorded). Use instead: GET /dashboard/trace (route trace of the last request).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/logs/activity", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (no activity feed is recorded). Use instead: GET /dashboard/logs.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/logs/conversations", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (conversation transcripts are not recorded here). Use instead: GET /v1/responses (stored Responses API conversations).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/logs/compression", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (no per-request compression log is recorded). Use instead: GET /api/analytics/compression.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/logs/audit", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (this audit feed was never written). Use instead: GET /dashboard/audit.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/logs/export", Tier: TierManagement, Description: "Export logs JSON (B8-126).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/logs/destinations", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (log export destinations are not implemented: nothing shipped logs to them). Use instead: GET /api/logs/export.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/logs/destinations", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (log export destinations are not implemented). Use instead: GET /api/logs/export.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/logs/destinations/{id}", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (log export destinations are not implemented). Use instead: GET /api/logs/export.", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/logs/destinations/{id}", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (log export destinations are not implemented). Use instead: GET /api/logs/export.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/logs/destinations/{id}", Tier: TierAlwaysProtected, Description: "Unavailable: answers 501 feature_unavailable, does nothing (log export destinations are not implemented). Use instead: GET /api/logs/export.", Scopes: []string{"admin"}},

	// b8_combos_extended.go
	{Method: "GET", Path: "/api/combos/{id}/capabilities", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (capability-specific combo behaviour is not implemented).", Scopes: []string{"admin"}},
	{Method: "PUT", Path: "/api/combos/{id}/capabilities", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (capability-specific combo behaviour is not implemented).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/combo-tuning", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (combo tuning settings are not implemented: stored values never reached the router).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/settings/combo-tuning", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (combo tuning settings are not implemented: stored values never reached the router).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/scoring-modes", Tier: TierManagement, Description: "Scoring factors and mode packs (B8-77).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/settings/account-fallback", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (account fallback strategy selection is not implemented).", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/settings/account-fallback", Tier: TierManagement, Description: "Unavailable: answers 501 feature_unavailable, does nothing (account fallback strategy selection is not implemented).", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/combos/{id}/studio-cascade", Tier: TierManagement, Description: "Live cascade view for combo studio (B8-83).", Scopes: []string{"admin"}},
	// P3-B scheduled evaluations (eval_schedules_api.go): mutations
	// rewrite scheduling state, so admin scope; reads are MANAGEMENT.
	{Method: "GET", Path: "/api/eval/schedules", Tier: TierManagement, Description: "Lists recurring evaluation schedules (id, suite, interval, enabled, regression threshold). Gateway-key auth required.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/eval/schedules", Tier: TierAlwaysProtected, Description: "Creates a recurring evaluation schedule. Management auth (admin scope) required — it starts periodic evaluation work.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/eval/schedules/{id}", Tier: TierManagement, Description: "One evaluation schedule by id; unknown ids answer 404. Gateway-key auth required.", Scopes: []string{"admin"}},
	{Method: "PATCH", Path: "/api/eval/schedules/{id}", Tier: TierAlwaysProtected, Description: "Partial update of one schedule (interval, enabled, regression threshold, concurrency). Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "DELETE", Path: "/api/eval/schedules/{id}", Tier: TierAlwaysProtected, Description: "Deletes one schedule (its history is preserved); unknown ids answer 404. Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/eval/schedules/{id}/history", Tier: TierManagement, Description: "Run history for one schedule, newest first (?limit=, default 100). Gateway-key auth required.", Scopes: []string{"admin"}},
	// P3-A org/team/RBAC (org_api.go): mutations rewrite tenant state,
	// so admin scope; reads are MANAGEMENT.
	{Method: "GET", Path: "/api/org", Tier: TierManagement, Description: "Lists organizations and teams (id, name, budget, allowed models/strategies, roles). Gateway-key auth required.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/org", Tier: TierAlwaysProtected, Description: "Creates (bootstraps) an organization. Management auth (admin scope) required — it creates a tenant.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/org/teams", Tier: TierAlwaysProtected, Description: "Creates a team under an org. Management auth (admin scope) required — it changes tenant isolation.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/org/teams/{id}/bind", Tier: TierAlwaysProtected, Description: "Binds a gateway key to a team (team-key isolation + team budget). Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "POST", Path: "/api/org/teams/{id}/unbind", Tier: TierAlwaysProtected, Description: "Removes a key's team binding (isolation off). Management auth (admin scope) required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/org/audit", Tier: TierManagement, Description: "Append-only admin audit log, newest first (?limit=, default 200). Gateway-key auth required.", Scopes: []string{"admin"}},
	// P3-A org/team analytics
	{Method: "GET", Path: "/api/org/analytics", Tier: TierManagement, Description: "Organization-level usage analytics across all teams (requests, tokens, costs, budget status). Gateway-key auth required.", Scopes: []string{"admin"}},
	{Method: "GET", Path: "/api/org/teams/{id}/analytics", Tier: TierManagement, Description: "Team-level usage analytics for a specific team (budget status, member breakdown). Gateway-key auth required.", Scopes: []string{"admin"}},
	// P3-C Cost Estimation & Reconciliation
	{Method: "POST", Path: "/api/cost/estimate", Tier: TierManagement, Description: "Estimate the cost of a chat completion request (prompt tokens × model prices). Gateway-key auth required.", Scopes: []string{"admin"}},
}

// RouteTable returns a copy of the guard classification table: every
// route the server registers, with its tier. The gate test walks this
// table and fails if any spawn-capable route is not LOCAL_ONLY.
func RouteTable() []GuardRoute {
	out := make([]GuardRoute, len(guardRouteTable))
	copy(out, guardRouteTable)
	return out
}

// findGuardRoute looks a (method, path) pair up in the table.
func findGuardRoute(method, path string) (GuardRoute, bool) {
	for _, r := range guardRouteTable {
		if r.Method == method && r.Path == path {
			return r, true
		}
	}
	return GuardRoute{}, false
}

// isLoopbackPeer reports whether the request arrived over a real
// loopback TCP connection, judged from RemoteAddr — the actual socket
// peer — only. The Host header and X-Forwarded-For are never
// consulted: either can be forged by a caller, and trusting one
// would re-open exactly the remote-code-execution class this tier
// closes. An unparseable peer fails closed (not loopback).
func isLoopbackPeer(r *http.Request) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port (e.g. a bare IP in tests): try the whole string.
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// requireLocalOnly is the Tier 1 guard: it runs BEFORE auth. A
// non-loopback peer is refused with 403 LOCAL_ONLY even when it
// presents a valid gateway key — a leaked credential over a tunnel
// must still not reach a spawn. The refusal is logged as the guard
// working as designed, not as an error.
func (s *Server) requireLocalOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackPeer(r) {
			s.localOnlyDenials.Add(1)
			s.logger.Printf("route guard: LOCAL_ONLY route %s %s refused for non-loopback peer %s (guard working as designed)", r.Method, r.URL.Path, r.RemoteAddr)
			writeError(w, http.StatusForbidden, "LOCAL_ONLY: this route is reachable only from loopback")
			return
		}
		next(w, r)
	}
}

// requireAuthAlways is the Tier 2 guard: gateway-key auth is required
// unconditionally. Unlike Tier 3 it does not honour authRelaxed —
// relaxing login must never open a destructive route.
func (s *Server) requireAuthAlways(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			s.unauthorized(w, r)
			return
		}
		next(w, r)
	}
}

// requireAuthUnlessRelaxed is the Tier 3 guard: gateway-key auth is
// required unless auth was explicitly relaxed (WithAuthRelaxed).
func (s *Server) requireAuthUnlessRelaxed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authRelaxed {
			next(w, r)
			return
		}
		if !s.authorized(r) {
			s.unauthorized(w, r)
			return
		}
		next(w, r)
	}
}

// spawnCapablePatterns is the regex companion to guardRouteTable
// (the reference router's SPAWN_CAPABLE_PATTERNS, research-log entry 143):
// the path shapes that can spawn a process, execute code, or open
// a login/tunnel flow. The table classifies exact routes; the
// patterns classify whole PREFIXES, so a future route with a
// dynamic segment under one of them — /admin/plugin/load/{id},
// /admin/service/remove/{name} — is still caught even though its
// exact path never appeared in the table. A route whose path
// matches a pattern MUST be TierLocalOnly: register() panics
// otherwise, exactly like an unclassified route. The patterns and
// the table are also checked against each other both ways in
// TestSpawnCapablePatternsMatchTable, so neither can drift from
// the other unnoticed. Patterns are matched against the registered
// path template (with {param} segments), not the request URL.
var spawnCapablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^/admin/shutdown$`),
	regexp.MustCompile(`^/admin/restart$`),
	regexp.MustCompile(`^/admin/service(/.*)?$`),
	regexp.MustCompile(`^/admin/plugin(/.*)?$`),
	regexp.MustCompile(`^/admin/update$`),
	regexp.MustCompile(`^/admin/tunnel(/.*)?$`),
	regexp.MustCompile(`^/admin/provider/login(/.*)?$`),
	regexp.MustCompile(`^/admin/bootstrap$`),
}

// SpawnCapablePatterns returns the spawn-capable path patterns as
// strings, for the guard status feed (GET /dashboard/guard) and
// the OpenAPI spec.
func SpawnCapablePatterns() []string {
	out := make([]string, 0, len(spawnCapablePatterns))
	for _, re := range spawnCapablePatterns {
		out = append(out, re.String())
	}
	return out
}

// matchesSpawnCapablePattern reports whether a registered path
// template falls under a spawn-capable prefix.
func matchesSpawnCapablePattern(path string) bool {
	for _, re := range spawnCapablePatterns {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// register wires one route into mux with the wrappers its tier
// requires. The route must be classified in guardRouteTable; an
// unclassified route panics at startup instead of landing unguarded.
// A route under a spawn-capable prefix that is NOT classified
// TierLocalOnly panics too — the pattern, not the table entry,
// has the last word on spawn-capable paths.
func (s *Server) register(mux *http.ServeMux, method, path string, handler http.HandlerFunc) {
	route, ok := findGuardRoute(method, path)
	if !ok {
		panic("server: route " + method + " " + path + " has no guard-tier classification (add it to guardRouteTable first)")
	}
	if matchesSpawnCapablePattern(path) && route.Tier != TierLocalOnly {
		panic("server: route " + method + " " + path + " matches a spawn-capable pattern but is classified " + string(route.Tier) + ", not LOCAL_ONLY")
	}
	h := handler
	// Headless mode (CHECKLIST 197-198): a headless gateway is a
	// pure API process — every /dashboard* route answers 404
	// instead of serving UI or feeds. The replacement happens
	// here, before the tier wrappers, so the 404 is the same for
	// every dashboard route and no dashboard handler can run.
	if s.headless && strings.HasPrefix(path, "/dashboard") {
		h = func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "dashboard disabled in headless mode — this server serves the API only; run without --headless for the dashboard")
		}
	}
	// Scope enforcement (M1.1) wraps the handler first, so it runs
	// inside auth: unauthenticated requests still answer 401 from
	// the auth wrapper, and only an authenticated key that lacks
	// the route's scope answers 403. A route with no scopes is
	// unchanged (requireScope returns the handler as-is).
	// The per-key policy hook (G21/B9, requireKeyPolicy) wraps the
	// handler inside scope: auth, then scope, then IP / endpoint
	// policy, then the handler — so a scope refusal is never
	// masked by a policy refusal, and every authenticated route
	// (spend and bare read alike) is policy-checked.
	scoped := s.requireScope(s.requireKeyPolicy(h), route.Scopes)
	// B3-04 dashboard auth endpoints (POST /dashboard/auth/*):
	// the login surface authenticates by password / session, not
	// by gateway key, so the tier wrappers below cannot guard
	// them — requireAuthUnlessRelaxed would 401 every login
	// attempt before the handler could check the password, and
	// requireAuthAlways would demand a key for logout. Scope +
	// key-policy still apply (a presented key must be
	// admin-scoped; keyless requests pass through to the handler,
	// which enforces its own credential). The table still
	// classifies them, so no route lands unclassified.
	if strings.HasPrefix(path, "/dashboard/auth/") {
		mux.HandleFunc(method+" "+path, scoped)
		return
	}
	switch route.Tier {
	case TierLocalOnly:
		// Loopback first, then auth — except bootstrap, which takes
		// no key because no key exists yet.
		if route.Bootstrap {
			h = s.requireLocalOnly(h)
		} else {
			h = s.requireLocalOnly(s.requireAuth(scoped))
		}
	case TierAlwaysProtected:
		h = s.requireAuthAlways(scoped)
	case TierManagement:
		if !route.AuthExempt {
			h = s.requireAuthUnlessRelaxed(scoped)
		} else if strings.HasPrefix(path, "/dashboard/") {
			// B3-04 feed gate: once an operator password is set,
			// the read-only dashboard feeds require a session (or
			// a gateway key) — requireLogin. With no password set
			// the feeds stay open exactly as today. GET /dashboard
			// itself is the bare path, not the prefix, so the
			// static shell (with its login overlay) stays open.
			h = s.requireDashboardSession(scoped)
		}
	}
	// The method is part of the mux pattern: two routes may share a
	// path with different methods (GET + POST /dashboard/guardrails).
	// Registering the bare path would make the second registration
	// panic on a pattern conflict, even though the guard table
	// classifies them as distinct routes.
	mux.HandleFunc(method+" "+path, h)
}

// ---------------------------------------------------------------------------
// Guard status feed (GET /dashboard/guard).
// ---------------------------------------------------------------------------

// guardTierView is one tier as the dashboard renders it.
type guardTierView struct {
	Tier        GuardTier    `json:"tier"`
	Description string       `json:"description"`
	Routes      []GuardRoute `json:"routes"`
}

// guardStatus is the live guard state reported by GET /dashboard/guard.
type guardStatus struct {
	LoopbackOnlyEnforced bool   `json:"loopback_only_enforced"`
	BootstrapPending     bool   `json:"bootstrap_pending"`
	AuthRelaxed          bool   `json:"auth_relaxed"`
	LocalOnlyDenials     uint64 `json:"local_only_denials"`
	// SecurityRefusals counts security refusals since this
	// process started, by reason label (CHECKLIST 246): a
	// client Vertex override rejected, a write naming an
	// unknown guardrail. Real counts — zero until one happens.
	SecurityRefusals map[string]uint64 `json:"security_refusals,omitempty"`
	Listen           string            `json:"listen"`
}

// guardResponse is the GET /dashboard/guard payload: the three tiers
// with the routes in each, the spawn-capable path patterns, plus the
// live guard status. It carries no secrets — only route paths, tiers
// and counters.
type guardResponse struct {
	Tiers                []guardTierView `json:"tiers"`
	SpawnCapablePatterns []string        `json:"spawn_capable_patterns"`
	Status               guardStatus     `json:"status"`
}

var guardTierDescriptions = map[GuardTier]string{
	TierLocalOnly:       "Routes that spawn processes, execute runtime code, or change the system. Loopback-only, checked before auth — a leaked key over a non-loopback connection still cannot reach a spawn.",
	TierAlwaysProtected: "Destructive routes. Gateway-key auth is always required, even if auth were ever relaxed for Tier 3.",
	TierManagement:      "Default tier. Auth required unless explicitly off (the dashboard feeds are explicitly off — they carry no secrets).",
}

// handleGuard serves GET /dashboard/guard: the guard-tier table plus
// live status (loopback enforcement, bootstrap state, denial count).
// It is intentionally NOT auth-gated, like the other dashboard feeds:
// the payload carries no secrets.
func (s *Server) handleGuard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tiers := []guardTierView{
		{Tier: TierLocalOnly, Description: guardTierDescriptions[TierLocalOnly], Routes: []GuardRoute{}},
		{Tier: TierAlwaysProtected, Description: guardTierDescriptions[TierAlwaysProtected], Routes: []GuardRoute{}},
		{Tier: TierManagement, Description: guardTierDescriptions[TierManagement], Routes: []GuardRoute{}},
	}
	byTier := map[GuardTier]*[]GuardRoute{
		TierLocalOnly:       &tiers[0].Routes,
		TierAlwaysProtected: &tiers[1].Routes,
		TierManagement:      &tiers[2].Routes,
	}
	for _, route := range guardRouteTable {
		*byTier[route.Tier] = append(*byTier[route.Tier], route)
	}
	listen := s.listenAddr
	if listen == "" {
		listen = "unknown"
	}
	writeJSON(w, http.StatusOK, guardResponse{Tiers: tiers, SpawnCapablePatterns: SpawnCapablePatterns(), Status: guardStatus{
		LoopbackOnlyEnforced: true,
		BootstrapPending:     s.bootstrapPending.Load(),
		AuthRelaxed:          s.authRelaxed,
		LocalOnlyDenials:     s.localOnlyDenials.Load(),
		SecurityRefusals:     s.guardrails.Stats().SecurityRefusals,
		Listen:               listen,
	}})
}

// ---------------------------------------------------------------------------
// Admin routes.
// ---------------------------------------------------------------------------

// BootstrapIssuer issues the first gateway key during fresh-install
// bootstrap. It returns the raw key (shown to the operator exactly
// once); the stored form is only ever a hash (see config.IssueKey /
// AddGatewayKey). The production issuer (main.go) persists the key to
// the config file.
type BootstrapIssuer func(ctx context.Context) (rawKey string, err error)

// bootstrapTokenBytes is the entropy of a one-shot bootstrap token:
// 32 crypto/rand bytes, hex-encoded (64 chars) for stdout + JSON.
const bootstrapTokenBytes = 32

// newBootstrapToken mints a fresh one-shot bootstrap token from
// crypto/rand. It lives in memory only: never persisted, never
// logged, never returned in any API response — the operator reads
// it once from the stdout line serve mode prints at first boot.
func newBootstrapToken() (string, error) {
	b := make([]byte, bootstrapTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// BootstrapToken returns the current one-shot bootstrap token, or
// "" when bootstrap is complete (the token is spent) or no token
// was issued. main.go prints it to stdout at first boot; nothing
// else reads it, and it is never written to a log or a response.
func (s *Server) BootstrapToken() string {
	s.bootstrapMu.Lock()
	defer s.bootstrapMu.Unlock()
	if !s.bootstrapPending.Load() {
		return ""
	}
	return s.bootstrapToken
}

// resetBootstrapToken (re)issues the one-shot token while bootstrap
// is pending — a fresh token at startup, and a fresh one whenever
// bootstrap flips back to pending (revoke-all / config reset), so
// the old token is dead and the first key can be claimed again.
// When bootstrap is complete it clears the token instead.
func (s *Server) resetBootstrapToken() {
	s.bootstrapMu.Lock()
	defer s.bootstrapMu.Unlock()
	if !s.bootstrapPending.Load() {
		s.bootstrapToken = ""
		return
	}
	if tok, err := newBootstrapToken(); err == nil {
		s.bootstrapToken = tok
	} else {
		// crypto/rand essentially never fails; without a token
		// the claim answers 503 (bootstrap stays pending, so a
		// restart can still issue one).
		s.bootstrapToken = ""
	}
}

// handleBootstrap serves POST /admin/bootstrap: fresh-install first-key
// setup. It is loopback-only by real TCP peer (the Tier 1 wrapper
// enforces that before this handler runs) and only while no gateway
// key exists yet. The claim needs the one-shot token printed to
// stdout at first boot — loopback alone is not enough, because any
// local process could otherwise claim the first admin key. A second
// call answers 409; with no issuer wired it answers 503. On success
// the key is returned once, the token is spent, and bootstrap flips
// to complete. A wrong token never consumes the real one.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.bootstrapPending.Load() {
		writeError(w, http.StatusConflict, "bootstrap already complete: a gateway key already exists")
		return
	}
	if s.bootstrapIssuer == nil {
		writeError(w, http.StatusServiceUnavailable, "bootstrap issuer is not configured")
		return
	}
	// The check, the issue and the consume run under one lock so
	// two concurrent claims cannot both mint a key.
	s.bootstrapMu.Lock()
	defer s.bootstrapMu.Unlock()
	if !s.bootstrapPending.Load() {
		writeError(w, http.StatusConflict, "bootstrap already complete: a gateway key already exists")
		return
	}
	if s.bootstrapToken == "" {
		writeError(w, http.StatusServiceUnavailable, "bootstrap token unavailable: restart the router to issue one")
		return
	}
	got := bootstrapTokenFromRequest(r)
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.bootstrapToken)) != 1 {
		writeError(w, http.StatusForbidden, "bootstrap token required: use the single-use token printed to stdout at first boot")
		return
	}
	raw, err := s.bootstrapIssuer(r.Context())
	if err != nil {
		s.logger.Printf("route guard: bootstrap key issue failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not issue gateway key")
		return
	}
	s.bootstrapToken = ""
	s.bootstrapPending.Store(false)
	writeJSON(w, http.StatusCreated, map[string]any{
		"gateway_key": raw,
		"note":        "Save this key now — it is shown once and cannot be recovered. Use it as: Authorization: Bearer <key>.",
	})
}

// bootstrapTokenFromRequest reads the one-shot token from the
// X-Bootstrap-Token header, or the bootstrap_token field of the
// JSON body. An unreadable or empty body is a missing token, not
// an error — the caller gets the 403, not a 400.
func bootstrapTokenFromRequest(r *http.Request) string {
	if tok := strings.TrimSpace(r.Header.Get("X-Bootstrap-Token")); tok != "" {
		return tok
	}
	if r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		return ""
	}
	var req struct {
		BootstrapToken string `json:"bootstrap_token"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	return strings.TrimSpace(req.BootstrapToken)
}

// handleShutdown serves POST /admin/shutdown (Tier 1): it answers 202
// and then invokes the shutdown hook (wired by main.go to gracefully
// stop the HTTP server). The hook runs after the response is written,
// from its own goroutine, so the 202 always reaches the caller.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.shutdownHook == nil {
		writeError(w, http.StatusServiceUnavailable, "shutdown hook is not configured")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting_down"})
	// Stop the A2A retention prune loop (M9.8, Integration-B
	// nit N2): it is the one background goroutine this server
	// starts lazily (see a2aService), and POST /admin/shutdown
	// is the in-package shutdown path — the loop must end here,
	// not linger until process exit. A no-op when A2A never
	// started. This runs before the hook so the loop is already
	// stopped when the HTTP server stops; Stop blocks only
	// until the current prune pass finishes.
	s.a2aStopPruneLoop()
	go s.shutdownHook()
}

// settingsExportProvider is one provider row in the settings export:
// identity and wiring facts only — never a base URL (it can carry
// embedded credentials) and never a key.
type settingsExportProvider struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	WireFormat   string   `json:"wire_format"`
	CatalogID    string   `json:"catalog_id,omitempty"`
	Verification string   `json:"verification,omitempty"`
	Models       []string `json:"models"`
	Enabled      bool     `json:"enabled"`
}

// settingsExportConfirm is the exact phrase the caller must type
// (and send in the JSON body) to export settings. This is the
// operator-credential second check (the reference router's credential-export
// boundary, research-log entry 143): the export redacts secrets
// AND still demands a typed confirmation, so a stolen gateway key
// alone cannot silently exfiltrate the full config — and so a
// future field added to the export can never leak without the
// confirm gate already in front of it. The phrase is deliberately
// distinct from the DB export's "EXPORT", so a body captured for
// one export cannot authorise the other.
const settingsExportConfirm = "EXPORT SETTINGS"

// handleSettingsExport serves POST /admin/settings/export (Tier 2):
// the full settings snapshot with secrets redacted. Provider keys
// and gateway-key hashes are never exported — the response says so
// explicitly instead of silently omitting them. The typed
// confirmation phrase is checked first, before config availability
// (mirroring the DB export). With no config loaded it answers 503
// (honest "unavailable", not an empty export).
func (s *Server) handleSettingsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.readDangerConfirm(w, r, settingsExportConfirm); !ok {
		return
	}
	cfg := s.cfg
	if cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "settings export unavailable: no config loaded")
		return
	}
	providers := []settingsExportProvider{}
	for _, p := range cfg.Providers {
		models := []string{}
		for _, m := range p.Models {
			models = append(models, m.ID)
		}
		providers = append(providers, settingsExportProvider{
			ID: p.ID, Name: p.Name, WireFormat: p.WireFormat,
			CatalogID: p.CatalogID, Verification: p.Verification,
			Models: models, Enabled: p.Enabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":           cfg.Version,
		"listen":            cfg.Listen,
		"budget_usd":        cfg.BudgetUSD,
		"fail_closed":       cfg.FailClosed,
		"telemetry_enabled": cfg.TelemetryEnabled,
		"gateway_key_count": len(cfg.GatewayKeys),
		"providers":         providers,
		"secrets":           "redacted — provider keys and gateway-key hashes are never exported",
		"exported_at":       time.Now().UTC().Format(time.RFC3339),
	})
}

// handleAdminStatus serves GET /admin/status (Tier 3): live server
// status — uptime, provider count, guard state. No keys, no secrets.
func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	providers := 0
	if s.dashboardProviders != nil {
		providers = len(s.dashboardProviders.DashboardProviders())
	}
	listen := s.listenAddr
	if listen == "" {
		listen = "unknown"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "ok",
		"uptime_seconds":     int64(time.Since(s.startedAt).Seconds()),
		"providers":          providers,
		"auth_relaxed":       s.authRelaxed,
		"bootstrap_pending":  s.bootstrapPending.Load(),
		"local_only_denials": s.localOnlyDenials.Load(),
		"listen":             listen,
	})
}
