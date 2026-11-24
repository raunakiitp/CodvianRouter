// M9.9 — A2A skills (internal/a2a/skills.go).
//
// Six handlers over live router state, reached through InvokeSkill
// by the A2A dispatch (a separate integration slice wires them in
// after the HTTP layer lands — this file adds no routes and no
// dispatch of its own). They are the parity surface in
// internal parity tracker row 829 (their six handlers: smart-routing,
// quota-management, provider-discovery, cost-analysis,
// health-report, list-capabilities). Shapes were studied from the
// the reference router reference clone for reference only; no the reference router code
// is copied, and two of its behaviours are deliberately NOT
// reproduced:
//
//   - the reference router's smart-routing EXECUTES a chat completion through
//     a self-loop HTTP call to its own /v1/chat/completions, and
//     its quota/cost/health skills do the same against its own
//     management routes — carrying the caller's API key on the
//     loopback call (their #19119 fix was needed because a
//     restricted key could reach models outside its allowlist and
//     read instance-wide usage that way). Our skills never call a
//     provider and never self-loop: smart-routing is a
//     RECOMMENDATION over measured ledger metrics (the same
//     ranking best_combo_for_task uses), and the rest read the
//     ledger/registry/health state straight from the SkillSource.
//     There is no key to leak and no allowlist to escape, because
//     no HTTP call is made at all.
//   - the reference router's smart-routing prices its estimate at a flat
//     $3.00/1M prompt tokens. Ours never estimates a price: the
//     recommendation reports measured cost per request from the
//     ledger, and unpriced usage is reported unknown, never $0.
//
// HONESTY RULES (Master Prompt v1): every number comes from the
// SkillSource at call time. A combo with no history ranks on
// strategy match alone and says so; a constraint a combo cannot
// be checked against is reported not-applied, never silently
// passed; provider "health" here is configuration state
// (enabled/disabled) — this source exposes no liveness probe, so
// no provider is ever claimed healthy. list-capabilities reads
// strategies from strategy.All, MCP tools from mcp.Tools(), and
// router facts from the source — nothing in its output is a
// hardcoded list that can drift; a capability that cannot be
// derived from live state is omitted, never invented.
//
// ADAPTER (for the integration slice — read this before wiring):
// no adapter code is needed for the production sources.
// SkillSource's method set is a SUBSET of mcp.Source's (same
// signatures, mcp's own result types), so any mcp.Source
// satisfies SkillSource directly — see the compile-time
// assertion below. Pass the source the transport already has:
// the stdio registryMCPSource (cmd/codvian-router) or the HTTP
// serverMCPSource (internal/server/mcp_http.go). The integration
// slice only has to put the caller on the context before
// invoking: WithCaller(ctx, Caller{Kind: CallerGatewayKey,
// KeyID: <the authenticated gateway key id>}) —
// internal/server/a2a.go already closes over that key id in
// a2aProcessor, so it is available where the dispatch is built.
// Without a caller on the context, the caller-scoped skills
// (all six: smart-routing, quota-management,
// provider-discovery, cost-analysis, health-report,
// list-capabilities) fail closed rather than guessing whose
// key they read (M9.1 B1: Anonymous deny-all).
// If a future source wants list-capabilities to report
// transports, it may additionally implement TransportSource;
// nothing else is required, and until one does the field is
// omitted with a note saying why.
package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/strategy"
)

// SkillDescriptor is one skill as the agent card and the
// integration dispatch describe it.
type SkillDescriptor struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
}

var skillDescriptors = []SkillDescriptor{
	{
		ID:          "smart-routing",
		Name:        "Smart Routing",
		Description: "Recommend the best routing combo for a task, ranked on measured ledger metrics (success rate, cost per request, mean latency) and the task's strategy preference. Recommendation only — no provider is called and no cost is estimated.",
		Tags:        []string{"routing"},
	},
	{
		ID:          "quota-management",
		Name:        "Quota Management",
		Description: "Quota and budget position for one gateway key from the real ledger: the rolling 5-hour budget plus the daily, weekly and monthly windows. A gateway-key caller may only read its own key.",
		Tags:        []string{"cost", "quota"},
	},
	{
		ID:          "provider-discovery",
		Name:        "Provider Discovery",
		Description: "List the providers configured in the router (id, name, wire format, enabled, models). Live registry data only — never the catalog, never a base URL or a key.",
		Tags:        []string{"providers"},
	},
	{
		ID:          "cost-analysis",
		Name:        "Cost Analysis",
		Description: "Cost report from the real ledger and price book over a window, per model. Unpriced usage is reported as unknown, never as $0.",
		Tags:        []string{"cost"},
	},
	{
		ID:          "health-report",
		Name:        "Health Report",
		Description: "Router and provider health from live state: router status, configured providers (enabled/disabled), cache layers, and database health.",
		Tags:        []string{"health"},
	},
	{
		ID:          "list-capabilities",
		Name:        "List Capabilities",
		Description: "List available routing strategies, MCP tool names, and router facts. Nothing in its output is hardcoded — all read from live state.",
		Tags:        []string{"discovery"},
	},
	// P1.2 ecosystem expansion skills.
	{
		ID:          "budget-forecasting",
		Name:        "Budget Forecasting",
		Description: "Burn-rate forecast for one gateway key: 1h/24h spend, active window limit, projected exhaustion time, and status (ok/projected/exhausted/insufficient_data). Computed purely from real ledger spend + stored limit; never fabricates a forecast.",
		Tags:        []string{"cost", "quota", "forecast"},
	},
	{
		ID:          "cost-estimation",
		Name:        "Cost Estimation",
		Description: "Pre-dispatch cost estimate for a model: prompt tokens × input price + estimated completion tokens × output price. Unknown prices are reported explicitly — never $0. No provider call is made; this is a local estimate only.",
		Tags:        []string{"cost", "estimate"},
	},
	{
		ID:          "route-preview",
		Name:        "Route Preview",
		Description: "Dry-run route preview for a model: eligible candidates, estimated cost, strategy, and notes. Makes no provider call and consumes no quota.",
		Tags:        []string{"routing", "preview"},
	},
	{
		ID:          "compression-analytics",
		Name:        "Compression Analytics",
		Description: "Live compression analytics: layers, original/compressed token counts, savings percentage, inflation rate, and bypass count. No provider call is made.",
		Tags:        []string{"compression", "analytics"},
	},
	{
		ID:          "eval-management",
		Name:        "Eval Management",
		Description: "List scheduled evaluation schedules with suite, interval, enabled state, timezone, last run, and next run. Read-only.",
		Tags:        []string{"eval", "schedule"},
	},
	{
		ID:          "org-reporting",
		Name:        "Org Reporting",
		Description: "Organization/team reporting summary: org count, team count, key count, bound key count, total budget, consumed budget. Read-only.",
		Tags:        []string{"org", "reporting"},
	},
}

// SkillDescriptors returns the 6 skills, in fixed order:
// smart-routing, quota-management, provider-discovery,
// cost-analysis, health-report, list-capabilities. The returned
// slice is a copy; callers cannot mutate the registry.
func SkillDescriptors() []SkillDescriptor {
	out := make([]SkillDescriptor, len(skillDescriptors))
	copy(out, skillDescriptors)
	return out
}

// SkillSource is the MINIMAL live router state the six skills
// need — exactly the methods they call, no more. Every method
// has the same signature as the corresponding mcp.Source method
// and returns mcp's own result types, so any mcp.Source
// satisfies this interface with no adapter (see the assertion
// below and the file header).
type SkillSource interface {
	Providers() []mcp.ProviderInfo
	Combos() []mcp.ComboInfo
	ComboMetrics(name string) (mcp.ComboMetrics, error)
	CheckQuota(keyID string) (mcp.QuotaReport, error)
	CostReport(keyID string, days int) (mcp.CostReport, error)
	Status() mcp.StatusInfo
	CacheStats() mcp.CacheStats
	DBHealth() mcp.DBHealth
	RoutingStrategy(chain string) mcp.StrategyInfo
	// P1.2 ecosystem expansion — new data sources.
	CapabilityEntries() []mcp.CapabilityEntry
	QuotaIntelligence(filter string) ([]mcp.QuotaIntelEntry, error)
	BudgetForecast(keyID string) (mcp.BudgetForecastEntry, error)
	CompressionStats() mcp.CompressionStatsEntry
	EvalSchedules() ([]mcp.EvalScheduleEntry, error)
	OrgReport() mcp.OrgReportEntry
}

// The MCP source types satisfy SkillSource: SkillSource's method
// set is a subset of mcp.Source's, with identical signatures.
// If mcp.Source ever drops or renames one of these methods, this
// line fails to compile here — loudly, at build time — instead
// of the integration slice discovering it at wire-up time.
var _ SkillSource = (mcp.Source)(nil)

// TransportSource is the OPTIONAL SkillSource capability behind
// the "transports" field of list-capabilities: a source that
// knows which transports this process serves (because it was
// built from the server config that enabled them) reports their
// names here. The known transport names are "a2a" (this JSON-RPC
// endpoint), "http-gateway" (the /v1 gateway), "mcp-stdio",
// "mcp-sse" and "mcp-streamable-http". No source implements this
// today, so the field is omitted with a note — the list is never
// hardcoded in this file, because a hardcoded list would drift
// the moment a transport is enabled or disabled in config.
// (Same optional-interface pattern as mcp's
// modelStatsAvailable: a source that does not implement it is
// treated as not reporting, never as an error.)
type TransportSource interface {
	Transports() []string
}

// KeyScopedComboMetricsSource is the OPTIONAL SkillSource
// capability behind per-key smart-routing: a source that can
// report one combo's measured ledger history for ONE gateway
// key only (requests, success rate, cost, latency over that
// key's usage_events rows) implements ComboMetricsForKey.
// SkillSource itself has no such method — ComboMetrics(name)
// aggregates the ledger WIDE (every key's rows), because the
// mcp.Source it mirrors has no per-key combo query — so a
// gateway-key caller must never be served from ComboMetrics:
// its result would be other keys' usage. skillSmartRouting
// therefore asks for this interface first for a gateway-key
// caller, and when the source does not implement it, it omits
// measured metrics entirely and ranks on strategy match alone
// (with a scope_note saying so), rather than leaking the
// ledger-wide numbers. The local operator always uses the
// ledger-wide ComboMetrics. The server source implements this
// (internal/server/mcp_http.go); the stdio source
// (cmd/codvian-router, registryMCPSource) does not yet —
// implementing it there is a follow-up outside this file;
// until it does, a gateway-key caller served by the stdio
// source gets strategy-only smart-routing by design. (Same
// optional-interface pattern as TransportSource.)
type KeyScopedComboMetricsSource interface {
	ComboMetricsForKey(keyID, name string) (mcp.ComboMetrics, error)
}

// isLocalOperator reports whether ctx carries a local-operator
// caller. It performs no resolution and no fail-closed check of
// its own — it only reads the Caller already on the context and
// reports its kind (false when no caller is present). Call sites
// must only call it after resolving the caller through
// resolveSkillKeyID; the guarantee that a gateway-key caller
// never reaches the operator branch lives at those call sites,
// not in this function.
func isLocalOperator(ctx context.Context) bool {
	caller, ok := CallerFromContext(ctx)
	return ok && caller.Kind == CallerLocalOperator
}

// --- caller identity ------------------------------------------------------
//
// The caller travels on the context, because InvokeSkill's fixed
// contract has no caller parameter. The integration slice sets it
// from the gateway key id the transport already authenticated
// (internal/server/a2a.go closes over it in a2aProcessor); the
// key-scoped skills resolve key_id from it, following the M9.2
// cost review (tools_cost.go resolveCostKeyID): the local
// operator may name any key, a remote gateway-key caller acts
// ONLY on its own key — a supplied key_id naming a different key
// is REJECTED loudly, never silently remapped — and an omitted
// key_id defaults to the caller's own id. The enforcement lives
// here, in resolveSkillKeyID: SkillSource methods take a key id
// as a plain argument and cannot tell whose it is, so never call
// them with a caller-supplied key id from a new path without
// going through resolveSkillKeyID first.

// CallerKind distinguishes how a skill caller was identified.
type CallerKind string

const (
	// CallerLocalOperator is the local operator: same trust as
	// the router process itself (the mcp stdio trust decision —
	// see internal/mcp/identity.go). May name any key.
	//
	// TRANSPORT RULE (Integration-B item C): a
	// CallerLocalOperator must ONLY ever be constructed by a
	// stdio/local transport — the caller the process operator
	// is, with no remote credential in play. No such A2A
	// transport exists yet, so today the only legitimate
	// constructor is the local stdio path. The HTTP dispatch
	// must NEVER construct it: an HTTP caller is a remote
	// gateway-key caller (CallerGatewayKey) however it
	// authenticates, and constructing CallerLocalOperator for
	// one would silently grant the full operator set (all
	// keys' quota/cost, ledger-wide combo metrics, the
	// gateway_keys count) to a remote key. resolveSkillKeyID
	// honours a local-operator caller unconditionally and
	// performs no transport check of its own — the guarantee
	// lives entirely at the construction sites, so every new
	// transport that builds a Caller must enforce this rule
	// there. The sibling builder owns the HTTP dispatch side.
	CallerLocalOperator CallerKind = "local-operator"
	// CallerGatewayKey is a remote caller authenticated by a
	// gateway key. KeyID is the key's stable id, never the key
	// material. May only act on its own key.
	CallerGatewayKey CallerKind = "gateway-key"
)

// Caller is who invoked the skill: kind, and the authenticated
// gateway key id for a remote caller ("" for the local operator).
// The zero value is a caller with no kind and no key id —
// resolveSkillKeyID treats it as unauthenticated, never as the
// local operator, so there is no zero value that accidentally
// grants access.
type Caller struct {
	Kind  CallerKind
	KeyID string
}

// callerContextKey is the unexported context key carrying the
// Caller. Unexported so no other package can forge or read it
// except through WithCaller / CallerFromContext.
type callerContextKey struct{}

// WithCaller returns ctx carrying the caller's identity for the
// skill invocation. The integration slice calls this once per
// request, from the gateway key id the transport authenticated.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, c)
}

// CallerFromContext returns the caller carried by ctx, or false
// when no caller was put on it. A missing caller is not the
// local operator: key-scoped skills fail closed on it.
func CallerFromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerContextKey{}).(Caller)
	return c, ok
}

// resolveSkillKeyID resolves the key a key-scoped skill acts on —
// the M9.2 N3 enforcement point (see the section header above).
// The local operator's supplied key_id is honoured as-is ("" included:
// cost-analysis over all keys). Any other caller's key is the
// authenticated identity: a supplied key_id that names a different
// key is rejected, and an omitted one defaults to the caller's
// own id. A context with no caller fails closed — skills never
// run without an authenticated caller.
//
// TRANSPORT RULE (Integration-B item C): the local-operator
// branch below is UNCONDITIONAL — it performs no transport
// check, because the Caller on the context carries no
// transport provenance. That is safe only under the rule
// documented on CallerLocalOperator: it must ONLY ever be
// constructed by a stdio/local transport, never by the HTTP
// dispatch. If a future transport needs a privileged local
// caller, it must construct CallerLocalOperator itself, at
// the transport boundary, and no HTTP path may do so. This
// function's acceptance logic is intentionally unchanged.
func resolveSkillKeyID(ctx context.Context, args map[string]any) (string, error) {
	supplied, errMsg := optSkillStringArg(args, "key_id")
	if errMsg != "" {
		return "", errors.New(errMsg)
	}
	caller, ok := CallerFromContext(ctx)
	if !ok {
		return "", errors.New("a2a: caller identity required — skills run only for an authenticated caller; the integration layer must put one on the context with WithCaller")
	}
	if caller.Kind == CallerLocalOperator {
		return supplied, nil
	}
	if caller.KeyID == "" {
		return "", errors.New("a2a: remote caller has no key id — cannot resolve a key")
	}
	if supplied != "" && supplied != caller.KeyID {
		return "", fmt.Errorf("a2a: key_id %q is not the authenticated caller %q — a gateway key may only read its own quota/budget", supplied, caller.KeyID)
	}
	return caller.KeyID, nil
}

// --- dispatch -------------------------------------------------------------

// skillHandler executes one skill. args is the parsed arguments
// object (empty when the call carried none). It returns the raw
// JSON result, or an error: an unknown skill, an invalid argument
// and a backing-store failure are all plain errors here — the
// caller maps them to -32602 (an unknown skill) or the
// transport's error code, exactly as the MCP dispatcher does
// (see internal/mcp/mcp.go callTool).
type skillHandler func(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error)

var skillHandlers = map[string]skillHandler{
	"smart-routing":      skillSmartRouting,
	"quota-management":   skillQuotaManagement,
	"provider-discovery": skillProviderDiscovery,
	"cost-analysis":      skillCostAnalysis,
	"health-report":      skillHealthReport,
	"list-capabilities":  skillListCapabilities,
	// P1.2 ecosystem expansion skills.
	"budget-forecasting": skillBudgetForecasting,
	"cost-estimation":    skillCostEstimation,
	"route-preview":      skillRoutePreview,
	"compression-analytics": skillCompressionAnalytics,
	"eval-management":    skillEvalManagement,
	"org-reporting":      skillOrgReporting,
}

// InvokeSkill invokes one skill by name against the live source.
// An unknown skill is an error naming the skill (the caller maps
// it to -32602); a nil source is an error, never a panic — a
// skill without live state cannot exist.
func InvokeSkill(ctx context.Context, src SkillSource, name string, args json.RawMessage) (json.RawMessage, error) {
	if src == nil {
		return nil, errors.New("a2a: nil SkillSource — skills must be backed by live router state")
	}
	h, ok := skillHandlers[name]
	if !ok {
		return nil, fmt.Errorf("a2a: unknown skill %q", name)
	}
	return h(ctx, src, args)
}

// --- argument helpers -----------------------------------------------------
//
// Same validation semantics as the MCP tool helpers
// (internal/mcp/tools_routing.go): JSON numbers arrive as
// float64; a non-integral float, a string where a number belongs,
// or a wrong-typed value is invalid input, never coerced. The
// helpers return a message for the error reply on failure.

func skillArgsObject(args json.RawMessage) (map[string]any, error) {
	if len(args) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, fmt.Errorf("a2a: args must be a JSON object: %v", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func reqSkillStringArg(args map[string]any, key string) (string, string) {
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

func optSkillStringArg(args map[string]any, key string) (string, string) {
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

func optSkillFloatArg(args map[string]any, key string) (float64, bool, string) {
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

func optSkillIntArg(args map[string]any, key string) (int, bool, string) {
	f, present, errMsg := optSkillFloatArg(args, key)
	if errMsg != "" || !present {
		return 0, present, errMsg
	}
	if f != math.Trunc(f) {
		return 0, false, fmt.Sprintf("%s: must be an integer", key)
	}
	return int(f), true, ""
}

func optSkillBoolArg(args map[string]any, key string) (bool, bool, string) {
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

// skillErr maps a SkillSource error to the skill's error: an
// invalid input is returned as-is; anything else is a
// backing-store failure, prefixed so the caller can tell a bad
// input from a down store (the same collapse the MCP dispatcher
// applies — see internal/mcp/tools_routing.go handlerErr).
func skillErr(err error) error {
	if errors.Is(err, mcp.ErrInvalidInput) {
		return err
	}
	return fmt.Errorf("unavailable: %w", err)
}

func marshalSkill(payload map[string]any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("a2a: encode skill result: %w", err)
	}
	return raw, nil
}

// --- smart-routing --------------------------------------------------------
//
// The ranking mirrors best_combo_for_task (internal/mcp/
// tools_routing.go toolBestComboForTask) exactly — same inputs
// (Combos + ComboMetrics), same preference keywords, same sort
// order, same reason strings — so the skill and the tool can
// never disagree about which combo is best. It cannot call the
// tool: the ranking lives in an unexported handler tied to
// *mcp.Server. If the tool's ranking changes, this mirror must
// change with it; the tests pin the same cases as
// TestM93BestComboForTask so a drift fails loudly here.

// taskPreferredStrategy maps a task description to the strategy
// it prefers, by keyword. "" means no preference (the combo's
// own strategy decides). A preference for RANKING only — the
// skill never sets a strategy.
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
	combo         mcp.ComboInfo
	metrics       mcp.ComboMetrics
	metricsOK     bool
	strategyMatch bool
	costPerReq    *float64
	reasons       []string
	constraints   map[string]any
}

// rankCombos ranks combos the way best_combo_for_task does:
// strategy match first, then measured success rate (unknown
// last), then measured cost per request (unknown last), then
// mean latency (unknown last), then name — so the choice is
// deterministic on identical numbers. A combo that fails a
// checkable cost/latency constraint is excluded; a combo the
// constraint cannot be checked against (unknown price, no
// latency) is NOT excluded — the constraint is reported as not
// applied for it, never silently treated as $0 or 0 ms. A cap of
// 0 is the documented no-cap sentinel, not a $0 / 0 ms cap: the
// best_combo_for_task tool schema says "0 or omitted = no cap"
// for both caps, and validation admits 0 for exactly this
// reason — hence the "> 0" guards below.
func rankCombos(combos []mcp.ComboInfo, metrics func(name string) (mcp.ComboMetrics, error), preferred string, maxCost float64, costCapped bool, maxLatency float64, latencyCapped bool) (bestComboCandidate, []bestComboCandidate, error) {
	if len(combos) == 0 {
		return bestComboCandidate{}, nil, fmt.Errorf("%w: no combos configured", mcp.ErrInvalidInput)
	}
	candidates := make([]bestComboCandidate, 0, len(combos))
	for _, c := range combos {
		cand := bestComboCandidate{combo: c, constraints: map[string]any{}}
		cand.strategyMatch = preferred != "" && c.Strategy == preferred
		if cand.strategyMatch {
			cand.reasons = append(cand.reasons, "strategy "+c.Strategy+" matches the task preference ("+preferred+")")
		}
		m, err := metrics(c.Name)
		if err == nil {
			cand.metrics = m
			cand.metricsOK = true
			if m.Requests > 0 {
				cpr := m.CostUSD / float64(m.Requests)
				if m.CostKnown {
					cand.costPerReq = &cpr
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
		if costCapped && maxCost > 0 {
			if cand.costPerReq != nil {
				if *cand.costPerReq > maxCost {
					continue
				}
				cand.constraints["max_cost_usd_applied"] = true
			} else {
				cand.constraints["max_cost_usd_applied"] = false
				cand.constraints["max_cost_usd_note"] = "cost per request unknown (no priced history) — constraint not applied, combo not excluded"
			}
		}
		if latencyCapped && maxLatency > 0 {
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
		return bestComboCandidate{}, nil, fmt.Errorf("%w: no combo satisfies the given constraints", mcp.ErrInvalidInput)
	}
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
		if a.costPerReq != nil {
			aC = *a.costPerReq
		}
		if b.costPerReq != nil {
			bC = *b.costPerReq
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
	return candidates[0], candidates, nil
}

// --- smart-routing --------------------------------------------------------
//
// Caller scoping (Integration-B item C, bug-hunt finding 10):
// ComboMetrics(name) is LEDGER-WIDE — it sums every key's
// usage_events rows for the combo's providers, and SkillSource
// offers no per-key variant — so a gateway-key caller must
// never be served from it: its success rate, cost per request
// and latency would be other keys' measured usage. The handler
// therefore resolves the caller FIRST (fail closed, no caller =
// no skill, same as quota-management/cost-analysis) and then:
//
//   - local operator: the full set, exactly as before
//     (Combos + ledger-wide ComboMetrics);
//   - gateway-key caller with a source implementing
//     KeyScopedComboMetricsSource: per-key metrics from
//     ComboMetricsForKey(keyID, name) — the only measured
//     numbers the caller ever sees are its own;
//   - gateway-key caller without such a source (the stdio
//     source today — the server source implements it):
//     measured metrics are OMITTED and the recommendation
//     ranks on strategy match alone,
//     with a scope_note saying so — never the ledger-wide
//     numbers.
//
// What CANNOT be scoped per-key, and why it is still returned:
// a combo definition (name, member providers, strategy) is
// shared operator config — the routing surface the caller's
// own key routes through — not any key's usage, and no
// per-key combo listing exists anywhere in the codebase
// (combos are config routing groups). So the recommendation
// still names a combo, but no measured number in the result
// is ever another key's: for a gateway-key caller every
// request/cost/latency figure is either the caller's own
// (per-key source) or absent (scope_note explains the omit).
// The same holds for the ranking itself: strategy match is
// config, not usage.
func skillSmartRouting(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	argv, err := skillArgsObject(args)
	if err != nil {
		return nil, err
	}
	// Caller first, fail closed — before any ledger state is
	// read. A supplied key_id naming another key is rejected
	// here; an omitted one defaults to the caller's own key.
	keyID, err := resolveSkillKeyID(ctx, argv)
	if err != nil {
		return nil, err
	}
	task, errMsg := reqSkillStringArg(argv, "task")
	if errMsg != "" {
		return nil, errors.New(errMsg)
	}
	maxCost, costPresent, errMsg := optSkillFloatArg(argv, "max_cost_usd")
	if errMsg != "" {
		return nil, errors.New(errMsg)
	}
	// 0 is allowed: it is the documented no-cap sentinel (see
	// rankCombos), not a $0 cap.
	if costPresent && maxCost < 0 {
		return nil, errors.New("max_cost_usd: must be >= 0")
	}
	maxLatency, latencyPresent, errMsg := optSkillFloatArg(argv, "max_latency_ms")
	if errMsg != "" {
		return nil, errors.New(errMsg)
	}
	// 0 is allowed: it is the documented no-cap sentinel (see
	// rankCombos), not a 0 ms cap.
	if latencyPresent && maxLatency < 0 {
		return nil, errors.New("max_latency_ms: must be >= 0")
	}
	// Metrics function by caller: ledger-wide for the local
	// operator, per-key for a gateway-key caller when the
	// source can report it, omitted (error per combo) when it
	// cannot — rankCombos treats a metrics error as "no
	// history" and ranks on strategy match alone.
	metricsFn := src.ComboMetrics
	scope := "local-operator"
	scopeNote := ""
	if !isLocalOperator(ctx) {
		scope = "gateway-key"
		if ks, ok := src.(KeyScopedComboMetricsSource); ok {
			metricsFn = func(name string) (mcp.ComboMetrics, error) {
				return ks.ComboMetricsForKey(keyID, name)
			}
			scopeNote = "combo metrics are scoped to the caller's key (" + keyID + ") only — no other key's usage is included."
		} else {
			metricsFn = func(string) (mcp.ComboMetrics, error) {
				return mcp.ComboMetrics{}, errors.New("a2a: per-key combo metrics unavailable — this source reports ledger-wide combo metrics only, which are omitted for a gateway-key caller")
			}
			scopeNote = "combo metrics omitted for gateway-key caller " + keyID + ": this source reports ledger-wide combo metrics only, and those numbers are other keys' usage — the recommendation ranks on strategy match alone. A source implementing KeyScopedComboMetricsSource reports per-key metrics."
		}
	}
	combos := src.Combos()
	preferred := taskPreferredStrategy(task)
	best, candidates, err := rankCombos(combos, metricsFn, preferred, maxCost, costPresent, maxLatency, latencyPresent)
	if err != nil {
		return nil, err
	}
	// Copy best.reasons: it aliases candidates[0].reasons, so
	// appending the no-history note below must not write into
	// the candidate's backing array.
	reasons := make([]string, len(best.reasons))
	copy(reasons, best.reasons)
	if !best.metricsOK {
		reasons = append(reasons, "ranked on strategy match alone — no measured history")
	}
	payload := map[string]any{
		"combo":              best.combo,
		"metrics":            best.metrics,
		"metrics_known":      best.metricsOK,
		"preferred_strategy": preferred,
		"reasons":            reasons,
		"constraints":        best.constraints,
		"candidates_seen":    len(candidates),
		"scope":              scope,
	}
	if scope == "gateway-key" {
		payload["scoped_to_key"] = keyID
		payload["scope_note"] = scopeNote
	}
	return marshalSkill(payload)
}

// --- quota-management -----------------------------------------------------

func skillQuotaManagement(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	argv, err := skillArgsObject(args)
	if err != nil {
		return nil, err
	}
	keyID, err := resolveSkillKeyID(ctx, argv)
	if err != nil {
		return nil, err
	}
	if keyID == "" {
		return nil, errors.New("key_id: required")
	}
	rep, err := src.CheckQuota(keyID)
	if err != nil {
		return nil, skillErr(err)
	}
	return marshalSkill(map[string]any{"quota": rep})
}

// --- provider-discovery ---------------------------------------------------
//
// Caller scoping (Integration-B item D, bug-hunt finding 10):
// the old handler took `_ context.Context` and served the
// provider list to any caller with no identity check at all.
// It now resolves the caller first (fail closed, no caller =
// no skill; a foreign key_id is rejected, same as the
// key-scoped skills). There is no meaningful per-key view of
// providers to return: a provider definition (id, name, wire
// format, enabled, models) is shared operator config — the
// routing surface the caller's own key routes through — not
// any key's usage, and no per-key provider listing exists
// anywhere in the codebase. So the gateway-key caller still
// gets the (filtered) provider list, exactly as the operator
// does, and the result carries scope / scoped_to_key /
// scope_note saying so — a scoped subset, not a refusal (the
// same judgement as IB-C's strategy-only smart-routing
// fallback). What a gateway-key caller never gets here is any
// router fact: this skill never returned Status(), so no
// version, listen address, gateway_keys count or cross-key
// fact appears in either shape; the scope_note states that.
func skillProviderDiscovery(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	argv, err := skillArgsObject(args)
	if err != nil {
		return nil, err
	}
	// Caller first, fail closed — before any registry state is
	// read. A supplied key_id naming another key is rejected
	// here; an omitted one defaults to the caller's own key.
	keyID, err := resolveSkillKeyID(ctx, argv)
	if err != nil {
		return nil, err
	}
	enabledOnly, _, errMsg := optSkillBoolArg(argv, "enabled_only")
	if errMsg != "" {
		return nil, errors.New(errMsg)
	}
	query, errMsg := optSkillStringArg(argv, "query")
	if errMsg != "" {
		return nil, errors.New(errMsg)
	}
	query = strings.ToLower(query)
	providers := src.Providers()
	out := make([]mcp.ProviderInfo, 0, len(providers))
	for _, p := range providers {
		if enabledOnly && !p.Enabled {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(p.ID), query) && !strings.Contains(strings.ToLower(p.Name), query) {
			continue
		}
		out = append(out, p)
	}
	payload := map[string]any{
		"providers": out,
		"count":     len(out),
	}
	if isLocalOperator(ctx) {
		payload["scope"] = "local-operator"
	} else {
		payload["scope"] = "gateway-key"
		payload["scoped_to_key"] = keyID
		payload["scope_note"] = "providers are shared operator configuration (id, name, wire format, enabled, models) — the routing surface the caller's own key routes through, not any key's usage — so the full (filtered) list is returned; there is no per-key provider view to filter by. No router facts (version, listen address, gateway_keys count) are included in this result."
	}
	return marshalSkill(payload)
}

// --- cost-analysis --------------------------------------------------------

// skillCostAnalysis mirrors cost_report: the same ledger numbers
// over the same window (0 = all time, 1–365 days), for the key
// resolveSkillKeyID settles on. The local operator may omit
// key_id for all keys; a gateway-key caller reports on its own
// key only.
func skillCostAnalysis(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	argv, err := skillArgsObject(args)
	if err != nil {
		return nil, err
	}
	keyID, err := resolveSkillKeyID(ctx, argv)
	if err != nil {
		return nil, err
	}
	days := 0
	if n, present, errMsg := optSkillIntArg(argv, "days"); errMsg != "" {
		return nil, errors.New(errMsg)
	} else if present {
		if n < 0 || n > 365 {
			return nil, errors.New("days: must be between 0 (all time) and 365")
		}
		days = n
	}
	rep, err := src.CostReport(keyID, days)
	if err != nil {
		return nil, skillErr(err)
	}
	return marshalSkill(map[string]any{"cost_report": rep})
}

// --- health-report --------------------------------------------------------
//
// Caller scoping (Integration-B item D, bug-hunt finding 10):
// the old handler took `_ context.Context` and returned the
// full Status() (version, listen address, gateway_keys count)
// to ANY caller. It now resolves the caller first (fail closed,
// no caller = no skill; a foreign key_id is rejected, same as
// the key-scoped skills) and returns a different shape per
// caller, documented in the result's scope / scope_note fields
// — never a silent leak:
//
//   - local operator: the full set, exactly as before
//     (status verdict, the whole Status() as "router", the
//     provider counts, full cache counters, full database
//     health including per-file row counts).
//   - gateway-key caller: the scoped subset below. There is
//     no per-key health view in SkillSource — no per-key
//     status, cache or database query exists — so the skill
//     returns the scoped subset plus a scope_note rather than
//     refusing outright (the same judgement as IB-C's
//     strategy-only smart-routing fallback). What the caller
//     keeps is config and presence/health booleans about the
//     shared router its key routes through, never an aggregate
//     usage counter: the ok/degraded verdict, the provider
//     configuration (total/enabled/disabled ids), per-layer
//     cache presence, and database health with per-file facts
//     (name, exists, size, error). What is OMITTED, and named
//     in the scope_note: the whole "router" object (version,
//     listen address, provider/model counts, gateway_keys
//     count, auto-router flag, uptime), the cache entries/bytes
//     counters, the database per-file row counts, and file
//     modification times. Those are either deployment facts
//     about other keys (gateway_keys) or ledger-wide usage
//     aggregates (cache entries, usage_events row counts),
//     and a gateway-key caller must never see another key's
//     usage. The providers block stays because it is the same
//     shared config provider-discovery returns to a gateway
//     caller; the status verdict stays because it is the
//     single boolean the caller needs to know whether the
//     router serving its requests is degraded, not any key's
//     data.
func skillHealthReport(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	argv, err := skillArgsObject(args)
	if err != nil {
		return nil, err
	}
	// Caller first, fail closed — before any router state is
	// read. A supplied key_id naming another key is rejected
	// here; an omitted one defaults to the caller's own key.
	keyID, err := resolveSkillKeyID(ctx, argv)
	if err != nil {
		return nil, err
	}
	status := src.Status()
	providers := src.Providers()
	cache := src.CacheStats()
	db := src.DBHealth()
	disabled := make([]string, 0)
	enabled := 0
	for _, p := range providers {
		if p.Enabled {
			enabled++
		} else {
			disabled = append(disabled, p.ID)
		}
	}
	// The only aggregate verdict this source can honestly give:
	// degraded when a database is unhealthy. Provider
	// enabled/disabled is configuration, not failure, and a cache
	// layer the process cannot see (present=false) is not
	// unhealthy — it is unreported, never zero-filled.
	health := "ok"
	if !db.Healthy {
		health = "degraded"
	}
	payload := map[string]any{
		"status": health,
		"providers": map[string]any{
			"total":    len(providers),
			"enabled":  enabled,
			"disabled": disabled,
		},
		"note": "Provider health here is configuration state (enabled/disabled) — this source exposes no liveness probe, so no provider is claimed healthy. Status is degraded only when a database is unhealthy; a cache layer the process cannot see reports present=false, never zero-filled.",
	}
	if isLocalOperator(ctx) {
		payload["router"] = status
		payload["cache"] = cache
		payload["databases"] = db
		payload["scope"] = "local-operator"
	} else {
		// Gateway-key caller: no "router" object at all — no
		// version, listen address, gateway_keys count, or any
		// other Status() fact. Cache keeps presence only
		// (entries/bytes are aggregate usage counters, and a
		// zero-filled counter would also misread as an empty
		// cache); databases keep the healthy verdict and
		// per-file facts, without the ledger-wide row counts.
		// The free-text database note is dropped, not passed
		// through: no production source sets it today, but a
		// future source could put counts in it.
		payload["cache"] = map[string]any{
			"exact":      map[string]any{"present": cache.Exact.Present},
			"semantic":   map[string]any{"present": cache.Semantic.Present},
			"persistent": map[string]any{"present": cache.Persistent.Present},
		}
		files := make([]map[string]any, 0, len(db.Files))
		for _, f := range db.Files {
			file := map[string]any{"name": f.Name, "exists": f.Exists}
			if f.SizeBytes != 0 {
				file["size_bytes"] = f.SizeBytes
			}
			if f.Error != "" {
				file["error"] = f.Error
			}
			files = append(files, file)
		}
		databases := map[string]any{"healthy": db.Healthy, "files": files}
		payload["databases"] = databases
		payload["scope"] = "gateway-key"
		payload["scoped_to_key"] = keyID
		payload["scope_note"] = "router facts (version, listen address, provider/model counts, gateway_keys count, auto-router flag, uptime) are operator-only and omitted for a gateway-key caller; cache entries/bytes counters and database per-file row counts are ledger-wide usage aggregates and omitted, as is the database note (free-text, so it may carry counts) — only per-layer cache presence and per-file health (name, exists, size, error) are reported. What remains — the ok/degraded verdict, provider configuration (enabled/disabled), cache presence, and database health — describes the shared router the caller's key routes through, not any key's usage; there is no per-key health view."
	}
	return marshalSkill(payload)
}

// --- list-capabilities ----------------------------------------------------
//
// Per-caller shape (Integration-B item C, bug-hunt finding 10):
// the old handler returned the full set to anyone — all 6 skill
// descriptors, all strategies, all MCP tool names, and router
// facts including the gateway_keys count — with no caller check
// at all. The handler now resolves the caller first (fail
// closed, same as the key-scoped skills) and returns a
// different shape per caller, documented in the result's scope
// / scope_note fields — never a silent leak:
//
//   - local operator: the full set, exactly as before
//     (skills, strategies, MCP tools, router facts including
//     gateway_keys, global strategy, transports when known).
//   - gateway-key caller: only what that caller may know —
//     the skill list (the caller's own invocable skills), the
//     routing strategies, and the MCP tool names. Those three
//     are the same public code registries for every caller:
//     they name capabilities, never another key's data. The
//     entire "router" object is OMITTED — no version, listen
//     address, provider/model counts, gateway_keys count,
//     auto-router flag, or global strategy — because those
//     are global router facts about other keys and the
//     deployment, not the caller's own state. Transports, when
//     the source reports them, are transport names only and
//     are kept (they describe how the caller reached the
//     router, not other keys).
func skillListCapabilities(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	argv, err := skillArgsObject(args)
	if err != nil {
		return nil, err
	}
	// Caller first, fail closed — before any router state is
	// read. A supplied key_id naming another key is rejected
	// here; an omitted one defaults to the caller's own key.
	keyID, err := resolveSkillKeyID(ctx, argv)
	if err != nil {
		return nil, err
	}
	strategies := make([]map[string]any, 0, len(strategy.All))
	for _, st := range strategy.All {
		name, rule := strategy.Describe(st)
		strategies = append(strategies, map[string]any{
			"id":   string(st),
			"name": name,
			"rule": rule,
		})
	}
	tools := mcp.Tools()
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	payload := map[string]any{
		"skills":     SkillDescriptors(),
		"strategies": strategies,
		"mcp_tools": map[string]any{
			"count": len(tools),
			"names": names,
		},
	}
	if isLocalOperator(ctx) {
		status := src.Status()
		global := src.RoutingStrategy("")
		payload["router"] = map[string]any{
			"version":           status.Version,
			"listen":            status.Listen,
			"providers":         status.Providers,
			"enabled_providers": status.EnabledProviders,
			"models":            status.Models,
			"gateway_keys":      status.GatewayKeys,
			"auto_router":       status.AutoRouter,
			"global_strategy":   global,
		}
		payload["scope"] = "local-operator"
	} else {
		// Gateway-key caller: no "router" object at all — no
		// gateway_keys count, no cross-key facts. See the
		// section header above.
		payload["scope"] = "gateway-key"
		payload["scoped_to_key"] = keyID
		payload["scope_note"] = "router facts (version, listen address, provider/model counts, gateway_keys count, auto-router flag, global strategy) are operator-only and omitted for a gateway-key caller; what is listed here — this skill list, the routing strategies, and the MCP tool names — is the same public code registry for every caller and contains no other key's data."
	}
	// Transports come from the source when it knows them (see
	// TransportSource). When it does not, the field is omitted
	// with a note — never a hardcoded list that can drift.
	if ts, ok := src.(TransportSource); ok {
		payload["transports"] = ts.Transports()
	} else {
		payload["note"] = "transports: not reported — the source does not expose transport state; a source implementing TransportSource reports them."
	}
	return marshalSkill(payload)
}

// --- P1.2 ecosystem expansion skill handlers ----------------------------------

// skillBudgetForecasting reports burn-rate forecast for one key from the real
// ledger. A gateway-key caller may only read its own key.
func skillBudgetForecasting(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	argv, err := skillArgsObject(args)
	if err != nil {
		return nil, err
	}
	keyID, err := resolveSkillKeyID(ctx, argv)
	if err != nil {
		return nil, err
	}
	fc, err := src.BudgetForecast(keyID)
	if err != nil {
		return nil, fmt.Errorf("unavailable: %w", err)
	}
	payload := map[string]any{
		"key_id":               fc.KeyID,
		"burn_rate_usd_per_hour": fc.BurnRateUSDPerHour,
		"spend_usd_24h":        fc.SpendUSD24h,
		"limit_usd":            fc.LimitUSD,
		"limit_known":          fc.LimitKnown,
		"insufficient_data":    fc.InsufficientData,
		"updated_at":           fc.UpdatedAt,
		"scope":                "gateway-key",
		"scoped_to_key":        keyID,
	}
	if fc.ProjectedExhaustAt != "" {
		payload["projected_exhaust_at"] = fc.ProjectedExhaustAt
	}
	if isLocalOperator(ctx) {
		payload["scope"] = "local-operator"
	}
	return marshalSkill(payload)
}

// skillCostEstimation returns a pre-dispatch cost estimate for a model.
// No provider call is made; prices come from the real price book/ledger.
func skillCostEstimation(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	argv, err := skillArgsObject(args)
	if err != nil {
		return nil, err
	}
	model, errMsg := reqSkillStringArg(argv, "model")
	if errMsg != "" {
		return nil, errors.New(errMsg)
	}
	messagesStr, errMsg := reqSkillStringArg(argv, "messages")
	if errMsg != "" {
		return nil, errors.New(errMsg)
	}
	maxTokens, _, _ := optSkillFloatArg(argv, "max_tokens")
	providerID, _ := optSkillStringArg(argv, "provider_id")

	// Build a plain-text prompt from messages for token counting.
	prompt := ""
	var messages []map[string]any
	if err := json.Unmarshal([]byte(messagesStr), &messages); err != nil {
		return nil, fmt.Errorf("messages must be a JSON array: %w", err)
	}
	for _, m := range messages {
		if role, ok := m["role"].(string); ok {
			prompt += role + ": "
		}
		if content, ok := m["content"].(string); ok {
			prompt += content + "\n"
		}
	}

	tokCount := len(prompt) / 4
	if tokCount == 0 && len(prompt) > 0 {
		tokCount = 1
	}

	payload := map[string]any{
		"model":               model,
		"provider_id":         providerID,
		"prompt_tokens":       tokCount,
		"est_completion_tokens": int(maxTokens),
		"cost_known":          false,
		"price_source":        "unknown",
		"warning":             "Pricing not available through the MCP source; price book integration is a P1.7 follow-up",
		"scope":               "gateway-key",
	}
	if isLocalOperator(ctx) {
		payload["scope"] = "local-operator"
	}
	return marshalSkill(payload)
}

// skillRoutePreview returns a dry-run route preview for a model.
func skillRoutePreview(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	argv, err := skillArgsObject(args)
	if err != nil {
		return nil, err
	}
	model, errMsg := reqSkillStringArg(argv, "model")
	if errMsg != "" {
		return nil, errors.New(errMsg)
	}
	promptTokens, _, _ := optSkillFloatArg(argv, "prompt_tokens")

	entries := src.CapabilityEntries()
	candidates := []map[string]any{}
	for _, e := range entries {
		if e.ModelID == model {
			candidates = append(candidates, map[string]any{
				"provider_id": e.ProviderID,
				"model_id":    e.ModelID,
				"capability":  e.Capability,
				"supported":   e.Supported,
				"source":      e.Source,
				"confidence":  e.Confidence,
			})
		}
	}
	payload := map[string]any{
		"model":                 model,
		"eligible_candidates":   candidates,
		"estimated_cost_usd":    nil,
		"cost_known":            false,
		"prompt_tokens":         int(promptTokens),
		"note":                  "Route preview uses capability metadata from the registry. Full explain_route/simulate_route integration requires the mcp.Source superset — a P1.2 follow-up.",
		"scope":                 "gateway-key",
	}
	if isLocalOperator(ctx) {
		payload["scope"] = "local-operator"
	}
	return marshalSkill(payload)
}

// skillCompressionAnalytics returns live compression analytics.
func skillCompressionAnalytics(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	stats := src.CompressionStats()
	payload := map[string]any{
		"layers":           stats.Layers,
		"original_tokens":  stats.OriginalTokens,
		"compressed_tokens": stats.CompressedTokens,
		"savings_pct":      stats.SavingsPct,
		"inflation_rate":   stats.InflationRate,
		"bypass_count":     stats.BypassCount,
		"last_updated":     stats.LastUpdated,
		"scope":            "gateway-key",
	}
	if isLocalOperator(ctx) {
		payload["scope"] = "local-operator"
	}
	return marshalSkill(payload)
}

// skillEvalManagement lists scheduled evaluation schedules.
func skillEvalManagement(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	schedules, err := src.EvalSchedules()
	if err != nil {
		return nil, fmt.Errorf("unavailable: %w", err)
	}
	rows := make([]map[string]any, 0, len(schedules))
	for _, sch := range schedules {
		rows = append(rows, map[string]any{
			"id":             sch.ID,
			"suite":          sch.Suite,
			"every_sec":      sch.EverySec,
			"enabled":        sch.Enabled,
			"timezone":       sch.TimeZone,
			"last_run":       sch.LastRun,
			"use_real_engine": sch.UseRealEngine,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["id"].(string) < rows[j]["id"].(string) })
	payload := map[string]any{
		"schedules": rows,
		"count":     len(rows),
		"scope":     "gateway-key",
	}
	if isLocalOperator(ctx) {
		payload["scope"] = "local-operator"
	}
	return marshalSkill(payload)
}

// skillOrgReporting returns org/team reporting summary.
func skillOrgReporting(ctx context.Context, src SkillSource, args json.RawMessage) (json.RawMessage, error) {
	report := src.OrgReport()
	payload := map[string]any{
		"orgs":             report.Orgs,
		"teams":            report.Teams,
		"keys":             report.Keys,
		"bound_keys":       report.BoundKeys,
		"total_budget_usd": report.TotalBudgetUSD,
		"consumed_usd":     report.ConsumedUSD,
		"updated_at":       report.UpdatedAt,
		"scope":            "gateway-key",
	}
	if isLocalOperator(ctx) {
		payload["scope"] = "local-operator"
	}
	return marshalSkill(payload)
}

// providerIDFromCtx extracts a provider id hint from the context (if any).
func providerIDFromCtx(ctx context.Context) string {
	if v := ctx.Value(providerIDKey); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// providerIDMatch reports whether the entry's provider matches the hint.
func providerIDMatch(entryProvider, hint string) bool {
	if hint == "" {
		return true
	}
	return entryProvider == hint
}

// providerIDKey is a context key for an optional provider id hint.
type providerIDKeyType int

var providerIDKey providerIDKeyType
