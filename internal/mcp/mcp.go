// Package mcp exposes the live router state as MCP (Model Context
// Protocol) tools over stdio (newline-delimited JSON-RPC 2.0, the
// transport MCP clients use for local servers).
//
// It is deliberately small and honest: 24 tools, all backed by a
// Source that the caller wires to the real provider registry —
// provider_list, model_list and router_status return live data from
// that registry, never canned responses. There is no sampling,
// no chat tool and no route that calls a provider: the three built-in
// tools are a read-only window onto the router, and no tool call can
// leak a key. The routing/cost/ops tools do mutate router state and
// can spend quota/budget — each is gated by its own scope, and the
// destructive ones need a separate grant. Provider entries
// carry no base URL and no API key (a base URL can embed credentials;
// see the settings export, which redacts them for the same reason).
//
// Protocol support is the 2025-03-26 surface every MCP client needs:
// initialize, notifications/initialized, tools/list, tools/call and
// ping. Anything else answers a JSON-RPC error (-32601 method not
// found, -32602 invalid params, -32700 parse error) instead of
// pretending to work. A tools/call whose arguments trip a wired
// guardrail store is refused with the policy-refusal code -32001
// (see WithGuardrail) — distinct from -32602 and from the upstream
// -32603, which cannot occur here because no tool calls a provider.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// ProtocolVersion is the MCP protocol version this server speaks.
const ProtocolVersion = "2025-03-26"

// ServerName is the server_info.name reported on initialize.
const ServerName = "codvian-router"

// ProviderInfo is one provider as an MCP tool reports it: identity
// and wiring facts only — never a base URL, never a key.
type ProviderInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	WireFormat   string   `json:"wire_format"`
	Enabled      bool     `json:"enabled"`
	Models       []string `json:"models"`
	CatalogID    string   `json:"catalog_id,omitempty"`
	Verification string   `json:"verification,omitempty"`
}

// ModelInfo is one model as model_list reports it.
type ModelInfo struct {
	ID           string `json:"id"`
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
}

// StatusInfo is the live router status router_status reports.
// UptimeSeconds is -1 when the caller does not know the start time
// (the stdio server has no serving loop); every other field comes
// straight from the config/registry the Source wraps.
type StatusInfo struct {
	Version          string `json:"version"`
	Listen           string `json:"listen"`
	Providers        int    `json:"providers"`
	EnabledProviders int    `json:"enabled_providers"`
	Models           int    `json:"models"`
	GatewayKeys      int    `json:"gateway_keys"`
	AutoRouter       bool   `json:"auto_router"`
	UptimeSeconds    int64  `json:"uptime_seconds"`
}

// Source is the live router state behind the tools. The production
// implementation (cmd/codvian-router) adapts the provider registry +
// config file; tests use a fake. A nil Source is never used — New
// panics on one, so a server without live state cannot exist.
type Source interface {
	Providers() []ProviderInfo
	Models() []ModelInfo
	// Routing/combo + cost/quota state (M9.3). Combos ARE the
	// config routing groups: list/create read and write
	// Routing.Groups (plus the ChainStrategies mirror for the
	// combo's strategy, exactly as the /v1/combos API does).
	// The active combo is the MCP server's session selection —
	// the serving router has no single active combo (every group
	// is always addressable), so SetActiveCombo only selects the
	// default combo the routing tools report and explain/simulate
	// default to; it never changes what the serving router
	// routes. Strategy reads and writes are the real config
	// values (global, or per-chain). Metrics, model stats, cost
	// and quota read the real ledger and the real price book —
	// never hardcoded rates. A method returns ErrInvalidInput
	// (wrapped) when the caller asked for something that does
	// not exist or is not well-formed, and a plain error when
	// the backing store is unavailable; handlers turn both into
	// a -32602 reply (the dispatcher's only handler-error code),
	// with the message naming which case it is.
	Combos() []ComboInfo
	// CreateCombo takes the caller's context: a cancelled
	// context aborts before any persist (the server path
	// answers its ctx.Err() → 503 guard; the stdio path
	// returns ctx.Err() before mutating), so a client that
	// went away cannot leave a half-created combo behind. A
	// nil ctx means "no caller context" (a direct
	// Source-level call): implementations fall back to
	// context.Background().
	CreateCombo(ctx context.Context, name string, members []string, strategy string) error
	ActiveCombo() string
	SetActiveCombo(name string) error
	ComboMetrics(name string) (ComboMetrics, error)
	// RoutingStrategy returns the effective strategy for a chain
	// ("" = the global strategy). The empty global strategy
	// means "priority" (M1.11).
	RoutingStrategy(chain string) StrategyInfo
	SetRoutingStrategy(chain, strategy string) error
	ExplainRoute(model string) (RouteExplanation, error)
	SimulateRoute(model string, promptTokens int) (RouteSimulation, error)
	ModelStats() []ModelStat
	CostReport(keyID string, days int) (CostReport, error)
	CheckQuota(keyID string) (QuotaReport, error)
	SetBudgetGuard(keyID string, limitUSD float64, unpricedPolicy string, dailyLimitUSD, weeklyLimitUSD, monthlyLimitUSD *float64) error
	PriceKnown(provider, model string) bool
	Status() StatusInfo
	// M9.4 ops tools (internal/mcp/tools_ops.go). CacheStats
	// reports the live M6 3-layer cache counts (a layer the
	// process cannot see is Present=false, never zero-filled);
	// CacheFlush evicts ONLY the named tenant's entries and
	// returns how many were removed. DBHealth/DBStats report
	// the live data-dir databases. The memory methods are
	// namespaced by caller: a caller can only touch their own
	// entries (see internal/mcp/memory_store.go).
	CacheStats() CacheStats
	CacheFlush(tenant string) (int, error)
	DBHealth() DBHealth
	DBStats() DBStats
	MemoryAdd(caller, text string) (MemoryEntry, error)
	MemorySearch(caller, query string) ([]MemoryEntry, error)
	MemoryClear(caller string) (int, error)
	// P1.2 ecosystem expansion data sources.
	CapabilityEntries() []CapabilityEntry
	QuotaIntelligence(filter string) ([]QuotaIntelEntry, error)
	BudgetForecast(keyID string) (BudgetForecastEntry, error)
	CompressionStats() CompressionStatsEntry
	EvalSchedules() ([]EvalScheduleEntry, error)
	OrgReport() OrgReportEntry
}

// CapabilityEntry is one provider/model capability fact with
// provenance: source, verification status, observation time,
// confidence. Never guessed from a model name.
type CapabilityEntry struct {
	ProviderID    string            `json:"provider_id"`
	ModelID       string            `json:"model_id,omitempty"`
	Capability    string            `json:"capability"`   // e.g. "tool_calling", "vision", "audio", "reasoning"
	Supported     *bool             `json:"supported,omitempty"` // nil = unknown
	Native        bool              `json:"native"`        // true = native support, false = emulated
	Source        string            `json:"source"`        // e.g. "catalog", "modelsync", "runtime-tested"
	VerifiedAt    string            `json:"verified_at,omitempty"`
	Confidence    float64           `json:"confidence"`
	Documentation string            `json:"documentation,omitempty"`
}

// QuotaIntelEntry is one quota/budget intelligence row, mirroring
// the /api/quota/intelligence response but MCP-native.
type QuotaIntelEntry struct {
	Kind       string            `json:"kind"`       // pool, schedule, key_budget, internal_budget, free_tier
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Scope      string            `json:"scope"`
	Target     string            `json:"target"`
	Model      string            `json:"model,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	BudgetUSD  float64           `json:"budget_usd"`
	Consumed   float64           `json:"consumed"`
	UsedPct    float64           `json:"used_pct"`
	State      string            `json:"state"`      // healthy, approaching_limit, exhausted, unavailable, unknown
	Source     string            `json:"source"`     // ledger_pool, ledger_budget, catalog_free_tier, etc.
	Confidence float64           `json:"confidence"`
	UpdatedAt  string            `json:"updated_at,omitempty"`
	Filters    map[string]string `json:"filters,omitempty"`
}

// BudgetForecastEntry is burn-rate forecasting for one key.
type BudgetForecastEntry struct {
	KeyID              string  `json:"key_id"`
	BurnRateUSDPerHour float64 `json:"burn_rate_usd_per_hour"`
	SpendUSD24h        float64 `json:"spend_usd_24h"`
	LimitUSD           float64 `json:"limit_usd"`
	LimitKnown         bool    `json:"limit_known"`
	ProjectedExhaustAt string  `json:"projected_exhaust_at,omitempty"`
	InsufficientData   bool    `json:"insufficient_data"`
	UpdatedAt          string  `json:"updated_at,omitempty"`
}

// CompressionStatsEntry is live compression analytics.
type CompressionStatsEntry struct {
	Layers          int     `json:"layers"`
	OriginalTokens  int64   `json:"original_tokens"`
	CompressedTokens int64  `json:"compressed_tokens"`
	SavingsPct      float64 `json:"savings_pct"`
	InflationRate   float64 `json:"inflation_rate"`
	BypassCount     int     `json:"bypass_count"`
	LastUpdated     string  `json:"last_updated,omitempty"`
}

// EvalScheduleEntry is one scheduled evaluation schedule.
type EvalScheduleEntry struct {
	ID          string  `json:"id"`
	Suite       string  `json:"suite"`
	EverySec    int64   `json:"every_sec"`
	Enabled     bool    `json:"enabled"`
	TimeZone    string  `json:"timezone"`
	LastRun     string  `json:"last_run,omitempty"`
	NextRun     string  `json:"next_run,omitempty"`
	UseRealEngine bool  `json:"use_real_engine"`
}

// OrgReportEntry is org/team reporting summary.
type OrgReportEntry struct {
	Orgs     int `json:"orgs"`
	Teams    int `json:"teams"`
	Keys     int `json:"keys"`
	BoundKeys int `json:"bound_keys"`
	TotalBudgetUSD float64 `json:"total_budget_usd"`
	ConsumedUSD   float64 `json:"consumed_usd"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// mcpToolCallTimeout bounds one tools/call: the request context
// is derived with this timeout in handleWithIdentity, so a slow
// or hung backing store (a ledger read, a combo persist) cannot
// hold a tool call — and its transport handler goroutine —
// forever. 30 seconds is generous for local SQLite work and
// still bounds the damage; a caller-supplied deadline shorter
// than this wins (context derivation takes the earlier of the
// two). The timeout also applies to stdio (Serve passes
// context.Background), where a hung call previously blocked the
// single stdio loop with no bound at all.
const mcpToolCallTimeout = 30 * time.Second

// ComboState is the per-session active-combo storage one
// transport session owns. The MCP Source interface has no
// session concept — ActiveCombo/SetActiveCombo read and write
// whatever selection the Source holds — so a remote transport
// carries its per-session state in the request context (see
// ContextWithComboState) and a ContextSource binds it for the
// duration of one call. The stdio Source keeps its own
// process-wide selection (the single local operator); it never
// sees a ComboState, so its behaviour is unchanged.
type ComboState interface {
	ActiveCombo() string
	SetActiveCombo(name string)
}

type comboStateKey struct{}

// ContextWithComboState returns ctx carrying st as the
// per-session active-combo state for one request. A nil state
// leaves ctx unchanged.
func ContextWithComboState(ctx context.Context, st ComboState) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if st == nil {
		return ctx
	}
	return context.WithValue(ctx, comboStateKey{}, st)
}

// ComboStateFromContext returns the per-session combo state
// carried by ctx, or nil when the request carries none (stdio,
// or a direct Source call outside a transport).
func ComboStateFromContext(ctx context.Context) ComboState {
	if ctx == nil {
		return nil
	}
	st, _ := ctx.Value(comboStateKey{}).(ComboState)
	return st
}

// ContextSource is the optional Source capability behind H6: a
// Source that can bind one request's context (and the combo
// state it carries) for the duration of one tools/call, and
// use that context — never context.Background() — for its
// backing-store calls. It is deliberately an OPTIONAL
// interface, not a change to Source: the stdio Source
// (registryMCPSource) does not implement it and keeps its
// existing behaviour, while the HTTP Source binds the
// transport's request context so a client disconnect or the
// tool-call timeout cancels a slow ledger call. A Source that
// does not implement it is used as-is, unbound.
type ContextSource interface {
	WithContext(ctx context.Context) Source
}
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func emptySchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// toolHandler executes one tool. args is the parsed "arguments"
// object (nil when the call carried none). It returns the result
// map and "" on success, or nil plus an error message on failure
// (an unknown tool is handled by the registry lookup in callTool,
// not here).
type toolHandler func(s *Server, args map[string]any) (map[string]any, string)

type toolEntry struct {
	def     Tool
	handler toolHandler
}

var (
	toolRegistryMu sync.RWMutex
	// toolRegistry is the tool definition + handler table. The
	// three read-only tools are built in; M9.3/M9.4/M9.5 register
	// theirs via RegisterTool (from their own files), so no slice
	// edits this file's Tools() list or dispatch switch. A tool
	// MUST declare its scopes first (RegisterToolScopes) —
	// RegisterTool panics otherwise, so a tool can never be
	// reachable without a scope assignment.
	toolRegistry = map[string]toolEntry{
		"router_status": {
			def: Tool{
				Name:        "router_status",
				Description: "Live Codvian Router status: listen address, provider/model counts, gateway-key count, whether the auto-router is configured, and uptime when known.",
				InputSchema: emptySchema(),
			},
			handler: toolRouterStatus,
		},
		"provider_list": {
			def: Tool{
				Name:        "provider_list",
				Description: "List the providers configured in the router (id, name, wire format, enabled, models, catalog id, verification). Never includes base URLs or API keys.",
				InputSchema: emptySchema(),
			},
			handler: toolProviderList,
		},
		"model_list": {
			def: Tool{
				Name:        "model_list",
				Description: "List every model offered by the configured providers (id, provider id, provider name). Virtual auto/* models are listed when the auto-router is configured.",
				InputSchema: emptySchema(),
			},
			handler: toolModelList,
		},
	}
	// builtinToolOrder is the stable order of the three built-in
	// tools in Tools() (M9.0 fixtures pin tools[2] == model_list).
	builtinToolOrder = []string{"router_status", "provider_list", "model_list"}
)

// RegisterTool adds one tool (definition + handler) to the server.
// It is the seam M9.3/M9.4/M9.5 use to add tools from their own
// files. Call RegisterToolScopes FIRST: a tool with no scope
// assignment panics here, so a tool can never be reachable without
// declaring what it needs. Re-registering an existing tool panics
// too (same programmer-error rule as RegisterToolScopes, M9.1 N9).
func RegisterTool(def Tool, handler toolHandler) {
	if def.Name == "" {
		panic("mcp: RegisterTool with empty tool name")
	}
	if handler == nil {
		panic(fmt.Sprintf("mcp: RegisterTool(%q) with nil handler", def.Name))
	}
	if _, known := RequiredScopes(def.Name); !known {
		panic(fmt.Sprintf("mcp: RegisterTool(%q) before RegisterToolScopes — every tool must declare its required scopes first", def.Name))
	}
	toolRegistryMu.Lock()
	defer toolRegistryMu.Unlock()
	if _, exists := toolRegistry[def.Name]; exists {
		panic(fmt.Sprintf("mcp: RegisterTool(%q): tool already registered", def.Name))
	}
	toolRegistry[def.Name] = toolEntry{def: def, handler: handler}
}

// Tools returns the tool definitions this server offers, in a stable
// order. The dashboard's MCP section renders this same list (via
// GET /dashboard/mcp), so the two can never drift apart.
func Tools() []Tool {
	toolRegistryMu.RLock()
	defer toolRegistryMu.RUnlock()
	out := make([]Tool, 0, len(toolRegistry))
	for _, name := range builtinToolOrder {
		if e, ok := toolRegistry[name]; ok {
			out = append(out, e.def)
		}
	}
	var extra []string
	for name := range toolRegistry {
		builtin := false
		for _, b := range builtinToolOrder {
			if b == name {
				builtin = true
				break
			}
		}
		if !builtin {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		out = append(out, toolRegistry[name].def)
	}
	return out
}

func toolRouterStatus(s *Server, args map[string]any) (map[string]any, string) {
	return toolResult(s.src.Status()), ""
}

func toolProviderList(s *Server, args map[string]any) (map[string]any, string) {
	providers := s.src.Providers()
	if providers == nil {
		providers = []ProviderInfo{}
	}
	return toolResult(map[string]any{"providers": providers}), ""
}

func toolModelList(s *Server, args map[string]any) (map[string]any, string) {
	models := s.src.Models()
	if models == nil {
		models = []ModelInfo{}
	}
	return toolResult(map[string]any{"models": models}), ""
}

// Server is a JSON-RPC 2.0 MCP server over one Source.
type Server struct {
	src     Source
	version string
	// guard is the optional guardrail store (see WithGuardrail).
	// Nil means no guardrail check: the pre-existing behaviour.
	guard *guardrails.Store
	// identity is the caller for Handle (see WithClientIdentity).
	// It defaults to Anonymous (deny-all, fail-closed — M9.1
	// SECURITY REVIEW B1 fix): a server built with New() alone
	// denies every tool until the caller explicitly opts into an
	// identity. The stdio transport opts into the local operator
	// explicitly at construction (cmd/codvian-router runMCPTo —
	// same trust as the operator, see identity.go); remote
	// transports (M9.2) must pass a per-request identity via
	// HandleWithIdentity, built from the gateway key. A default
	// of LocalOperator would silently grant any future transport
	// that reuses New() + Handle() the full grant — that was the
	// B1 hole, and a doc comment is not a defence against it.
	identity ClientIdentity
	// callCtx is the request context for this dispatch copy
	// (set by handleWithIdentity, nil on the shared server):
	// handlers that take a ctx (create_combo) pass it to the
	// Source instead of context.Background(), so the caller's
	// cancellation reaches the backing store. It lives on the
	// per-call copy, never the shared server, so concurrent
	// calls cannot see each other's context — the same rule
	// as the per-request identity (see withIdentity).
	callCtx context.Context
	// audit is the optional append-only tool-call log (see
	// WithAuditLog). Nil means no auditing: the pre-existing
	// behaviour. Production stdio wires one (cmd/codvian-router
	// runMCPTo); tests wire one when they assert on the log.
	audit *AuditLog
}

// New builds a Server backed by src. version is reported in
// server_info (the binary version); src must not be nil. The new
// server's caller identity is Anonymous (deny-all, fail-closed —
// M9.1 SECURITY REVIEW B1 fix): Handle denies every tool until
// the caller opts in explicitly with WithClientIdentity (stdio
// does this for the local operator) or per request with
// HandleWithIdentity. Use WithAuditLog to record tool calls.
func New(src Source, version string) *Server {
	if src == nil {
		panic("mcp: nil Source — the MCP server must be backed by live router state")
	}
	return &Server{src: src, version: version, identity: Anonymous()}
}

// WithGuardrail wires a guardrail store into the server and returns
// it, for chaining. The store's blocking toggles are honoured on
// every tools/call, in the tool_args phase: the tool name plus the
// JSON-marshalled arguments are scanned before the tool is
// dispatched, and a blocked call is refused with the Codvian
// policy-refusal code (-32001, guardrails.PolicyRefusalCode) — the
// same distinct code the HTTP path uses, never -32602 (invalid
// params) and never -32603 (an upstream failure, which cannot
// happen here: the tools are read-only and never call a provider).
// The refusal message names the guardrail kind only; argument
// content is never echoed back. With every toggle off (the
// default) a call flows unchanged and only the evaluation is
// counted, under phase "tool_args" in the store.
//
// Only the tool_args (pre-call) phase exists here. There is no
// post_call / tool_output phase: tool outputs are generated from
// the registry (provider names, model lists, counts), never
// user-supplied, so there is nothing to scan — a check there would
// always pass and its counter would be decoration, not a guardrail.
func (s *Server) WithGuardrail(g *guardrails.Store) *Server {
	s.guard = g
	return s
}

// WithClientIdentity sets the caller identity Handle uses for
// scope checks and audit records, and returns the server, for
// chaining. The identity's scopes are copied, so a caller holding
// the same backing slice cannot widen the server's stored grant
// by mutating it afterwards (M9.1 SECURITY REVIEW N3 —
// GatewayKeyClient already copies the same way). This is the seam
// for transports with one caller per server (a stdio server
// always has the local operator, so it calls this explicitly
// with LocalOperator()); per-request identities go through
// HandleWithIdentity instead.
func (s *Server) WithClientIdentity(id ClientIdentity) *Server {
	s.identity = copyIdentity(id)
	return s
}

// copyIdentity returns id with its Scopes slice copied, so the
// result never shares a backing array with the caller's slice.
func copyIdentity(id ClientIdentity) ClientIdentity {
	if id.Scopes == nil {
		return id
	}
	cp := make([]string, len(id.Scopes))
	copy(cp, id.Scopes)
	id.Scopes = cp
	return id
}

// withIdentity returns a shallow copy of the server carrying id as
// its caller identity, sharing src, guard and audit with the
// original. Dispatch goes through the copy (see handleWithIdentity)
// so tool handlers reading s.identity — callerID, the tenant for
// cache_flush and the namespace for the memory tools — see the
// per-request caller, not the server default. The shared server is
// never mutated, so concurrent requests cannot cross identities:
// each in-flight call dispatches through its own copy, and the
// copy is discarded when the call returns. The shared pointers are
// safe to share: src is read-only state, the guardrail store
// serialises its own counters, and the audit log serialises
// under its own mutex.
func (s *Server) withIdentity(id ClientIdentity) *Server {
	cp := *s
	cp.identity = copyIdentity(id)
	return &cp
}

// WithAuditLog wires an append-only audit log into the server and
// returns it, for chaining. Every tools/call (allowed and denied)
// is recorded; a record failure never fails the call (see
// audit.go) and is kept on the log's LastError.
func (s *Server) WithAuditLog(a *AuditLog) *Server {
	s.audit = a
	return s
}

// auditCall records one tools/call outcome when an audit log is
// wired. It is best-effort by design (see audit.go): the error is
// kept on the log, never returned.
func (s *Server) auditCall(id ClientIdentity, tool string, args map[string]any, decision AuditDecision, reason string, output any, errCode int) {
	if s.audit == nil {
		return
	}
	entry := AuditEntry{
		Tool:       tool,
		CallerKind: id.Kind,
		CallerID:   id.ID,
		Decision:   decision,
		Reason:     reason,
		InputHash:  HashArguments(args),
		ErrorCode:  errCode,
	}
	if decision == AuditAllow {
		entry.OutputSummary = SummarizeOutput(output)
	}
	_ = s.audit.Record(entry)
}

// toolArgsText joins a tool call's name and arguments into the text
// the guardrail scans: the name on its own line, then the arguments
// as JSON (marshalling failure falls back to Go formatting — the
// arguments arrived as JSON, so this never happens in practice).
// The returned text is scanned, never returned to the client.
func toolArgsText(name string, args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		raw = []byte(fmt.Sprintf("%v", args))
	}
	return name + "\n" + string(raw)
}

// request is one incoming JSON-RPC message. ID is raw so a string,
// number or null id round-trips verbatim; a missing id marks a
// notification, which gets no response.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func errResponse(id json.RawMessage, code int, msg string) []byte {
	if id == nil {
		id = json.RawMessage("null")
	}
	raw, _ := json.Marshal(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
	return raw
}

func okResponse(id json.RawMessage, result any) []byte {
	if id == nil {
		id = json.RawMessage("null")
	}
	raw, _ := json.Marshal(response{JSONRPC: "2.0", ID: id, Result: result})
	return raw
}

// toolCallParams is the tools/call params shape.
type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// toolResult is one tools/call result: human-readable text plus the
// machine-readable structuredContent (the same payload, twice —
// clients that only read text still see the live data).
func toolResult(payload any) map[string]any {
	text, _ := json.Marshal(payload)
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(text)},
		},
		"structuredContent": payload,
	}
}

// Handle processes one raw JSON-RPC message and returns the response
// bytes, or nil when the message is a notification (no response is
// ever sent for one). ctx bounds the call: handleWithIdentity
// derives a 30-second timeout from it and threads it to the
// Source when the Source implements ContextSource, so a slow
// backing store cannot hang the caller — and a cancelled ctx
// (client disconnect, caller timeout) aborts the in-flight tool
// call. The caller identity is the server's configured identity
// (Anonymous — deny-all — unless WithClientIdentity opted into
// another one; see New).
func (s *Server) Handle(ctx context.Context, raw []byte) []byte {
	return s.handleWithIdentity(ctx, raw, s.identity)
}

// HandleWithIdentity is Handle for a per-request caller identity:
// remote transports (M9.2) build the identity from the gateway key
// that authenticated the request and pass it here, so the scope
// check and the audit record see the real caller, not the server
// default. The server's configured identity is untouched. The
// identity is snapshotted (scopes copied) at call entry, so a
// caller mutating its slice mid-call cannot widen the in-flight
// check — HandleWithIdentity stores nothing, so unlike
// WithClientIdentity it has no persistent aliasing hole (N3
// checked), but the snapshot keeps the decision stable for the
// duration of this one call. Dispatch runs through a shallow copy
// carrying the identity (see withIdentity), so tool handlers
// reading s.identity see this caller too — the identity reaches
// the handlers, not just the scope check and the audit log.
func (s *Server) HandleWithIdentity(ctx context.Context, raw []byte, id ClientIdentity) []byte {
	return s.handleWithIdentity(ctx, raw, copyIdentity(id))
}

func (s *Server) handleWithIdentity(ctx context.Context, raw []byte, id ClientIdentity) []byte {
	if ctx == nil {
		ctx = context.Background()
	}
	// Bound the call (see mcpToolCallTimeout): a hung backing
	// store must not hold the handler goroutine forever, and a
	// caller that went away must cancel the work, not leave it
	// running behind a dead transport. The derived ctx is what
	// reaches the Source (below), never the caller's raw ctx
	// without a bound.
	ctx, cancel := context.WithTimeout(ctx, mcpToolCallTimeout)
	defer cancel()
	// Dispatch runs through a shallow copy carrying the
	// per-request identity (see withIdentity). When the Source
	// implements ContextSource, the copy is bound to this
	// call's context first, so its ledger/registry/combo calls
	// honour cancellation and the timeout; a Source that does
	// not implement it (the stdio Source) is used as-is.
	dispatch := s.withIdentity(id)
	// The copy also carries this call's context, so handlers
	// that take a ctx (create_combo) can pass it to the
	// Source: a Source that does not implement ContextSource
	// (the stdio Source) still sees the caller's cancellation
	// through the CreateCombo ctx parameter.
	dispatch.callCtx = ctx
	if cs, ok := s.src.(ContextSource); ok {
		dispatch.src = cs.WithContext(ctx)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return errResponse(nil, -32700, fmt.Sprintf("parse error: %v", err))
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return errResponse(req.ID, -32600, "invalid request: expected a JSON-RPC 2.0 object with a method")
	}
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		if isNotification {
			return nil
		}
		return okResponse(req.ID, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ServerName, "version": s.version},
		})
	case "notifications/initialized", "notifications/cancelled":
		// Notifications never get a response, even when malformed
		// in every other respect — that is the protocol.
		return nil
	case "ping":
		if isNotification {
			return nil
		}
		return okResponse(req.ID, map[string]any{})
	case "tools/list":
		if isNotification {
			return nil
		}
		return okResponse(req.ID, map[string]any{"tools": s.visibleTools()})
	case "tools/call":
		if isNotification {
			return nil
		}
		var params toolCallParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				// Malformed params are audited too (M9.1 SECURITY
				// REVIEW N1): a tools/call that never parses must
				// not escape the log. The tool name is recorded
				// when it is parseable, "unknown" when it is not;
				// there are no arguments to hash (nil hashes as
				// "null"), and the code is the same -32602 the
				// caller receives.
				tool := "unknown"
				var nameOnly struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(req.Params, &nameOnly); err == nil && nameOnly.Name != "" {
					tool = nameOnly.Name
				}
				s.auditCall(id, tool, nil, AuditDeny, fmt.Sprintf("invalid params: %v", err), nil, -32602)
				return errResponse(req.ID, -32602, fmt.Sprintf("invalid params: %v", err))
			}
		}
		// Visibility, scope check, then guardrail, then dispatch
		// — in that order, all before the tool runs (M9.1). A
		// filtered-out tool is indistinguishable from an unknown
		// tool: the visibility check runs FIRST, so it answers
		// -32602 unknown tool and is audited as a denial — never
		// a scope denial (-32002), which would reveal the tool
		// exists to a caller who lacks its scopes. An unknown
		// tool is invisible too, so it takes the same path and
		// keeps its -32602 answer from dispatch, below.
		// Ordering consequence: an unknown/filtered tool whose arguments would trip the guardrail answers -32602 here, not -32001 — the guardrail never sees a tool the caller cannot see.
		if !s.isToolVisible(params.Name) {
			errMsg := "unknown tool: " + params.Name
			s.auditCall(id, params.Name, params.Arguments, AuditDeny, errMsg, nil, -32602)
			return errResponse(req.ID, -32602, errMsg)
		}
		// Scope check. Scope enforcement is ON by default with no
		// off switch: a known tool the caller lacks scopes for
		// is denied with -32002 (ScopeDeniedCode) and audited
		// as a denial.
		if required, known := RequiredScopes(params.Name); known {
			if chk := CheckScopes(id.Scopes, required); !chk.Allowed {
				s.auditCall(id, params.Name, params.Arguments, AuditDeny, chk.Reason, nil, ScopeDeniedCode)
				return errResponse(req.ID, ScopeDeniedCode, "scope denied: "+chk.Reason)
			}
		}
		// Guardrail, tool_args phase (see WithGuardrail): a blocked
		// call is refused with -32001 before dispatch, so it can
		// never reach a tool — and never be confused with an
		// invalid-params (-32602) or upstream (-32603) failure.
		if s.guard != nil {
			if res := s.guard.Check(toolArgsText(params.Name, params.Arguments), guardrails.PhaseMCPToolArgs); res.Blocked {
				s.auditCall(id, params.Name, params.Arguments, AuditDeny, "guardrail: "+res.BlockReason, nil, guardrails.PolicyRefusalCode)
				return errResponse(req.ID, guardrails.PolicyRefusalCode, "policy refusal: "+res.BlockReason)
			}
		}
		// Dispatch through the copy built at call entry (see
		// above): handlers read the caller from s.identity, so
		// dispatching on the shared server would hand every
		// handler the server default (Anonymous, or
		// LocalOperator on stdio) instead of this request's
		// caller — a remote key's cache_flush would flush the
		// "anonymous" tenant and its memories would land in
		// the wrong namespace. The scope check and audit above
		// already use id directly; only the handler call needs
		// the copy. The copy also carries this call's bound
		// Source, so handlers see the request context (and
		// the session's combo state) through s.src.
		result, errMsg := dispatch.callTool(params.Name, params.Arguments)
		if errMsg != "" {
			s.auditCall(id, params.Name, params.Arguments, AuditDeny, errMsg, nil, -32602)
			return errResponse(req.ID, -32602, errMsg)
		}
		s.auditCall(id, params.Name, params.Arguments, AuditAllow, "", result, 0)
		return okResponse(req.ID, result)
	default:
		if isNotification {
			return nil
		}
		return errResponse(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// callTool dispatches one tool by name against the live Source,
// via the tool registry (see RegisterTool). The second return is
// non-empty only for an unknown tool.
func (s *Server) callTool(name string, args map[string]any) (map[string]any, string) {
	toolRegistryMu.RLock()
	e, ok := toolRegistry[name]
	toolRegistryMu.RUnlock()
	if !ok {
		return nil, fmt.Sprintf("unknown tool: %s", name)
	}
	return e.handler(s, args)
}

// mcpMaxStdioLineBytes caps one stdio JSON-RPC line at 1 MiB —
// the same cap the HTTP transports enforce per message. A line
// over the cap gets a -32700 parse error, not a dead server.
const mcpMaxStdioLineBytes = 1 << 20

// readStdioLine reads one \n-terminated line from r. It returns
// the line (newline included, if present), whether the line ran
// over mcpMaxStdioLineBytes, and any read error. An over-long
// line is consumed in full — the reader is left positioned at
// the next line — so the caller can answer -32700 for it and
// keep serving, which bufio.Scanner cannot do: after
// ErrTooLong a Scanner is unrecoverably stopped, and the old
// Serve loop returned there, killing the whole stdio server
// for one bad client line (H5). A final line without a newline
// is returned as a line, like a Scanner token; io.EOF with no
// pending bytes ends the stream.
func readStdioLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	var buf []byte
	for {
		frag, rerr := r.ReadSlice('\n')
		if len(frag) > 0 && !tooLong {
			if len(buf)+len(frag) > mcpMaxStdioLineBytes {
				// Over the cap: drop the content (it is never
				// echoed back) and discard the rest of the
				// line, so the next read starts clean.
				tooLong = true
				buf = buf[:0]
			} else {
				buf = append(buf, frag...)
			}
		}
		switch rerr {
		case nil:
			return buf, tooLong, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(buf) == 0 && !tooLong {
				return nil, false, io.EOF
			}
			return buf, tooLong, nil
		default:
			return nil, false, rerr
		}
	}
}

// Serve runs the stdio transport: one JSON-RPC message per line on
// r, one response line on w per request. It returns on EOF (the
// client closing stdin is the normal shutdown — MCP stdio servers
// have no other stop signal) or on a write error. Malformed lines
// get a -32700 response, not a crash, and the loop continues —
// including a line over the 1 MiB cap, which answers -32700 for
// that line and keeps serving (H5: the old scanner loop died on
// bufio.ErrTooLong and took the server down with it). Every call
// runs as the server's configured identity — stdio sets
// LocalOperator explicitly at construction (see New), so a bare
// New() server served here would deny every tool. Every call is
// bounded by the tool-call timeout in handleWithIdentity.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, tooLong, err := readStdioLine(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("mcp: reading stdin: %w", err)
		}
		if tooLong {
			resp := errResponse(nil, -32700, "parse error: message exceeds 1 MiB")
			if _, err := w.Write(append(resp, '\n')); err != nil {
				return fmt.Errorf("mcp: writing response: %w", err)
			}
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		resp := s.Handle(context.Background(), line)
		if resp == nil {
			continue
		}
		if _, err := w.Write(append(resp, '\n')); err != nil {
			return fmt.Errorf("mcp: writing response: %w", err)
		}
	}
}
