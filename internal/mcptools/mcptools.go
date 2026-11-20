// Package mcptools is the MCP *client* side of the router: it talks
// to operator-configured MCP servers (external tools) and makes them
// usable from chat completions.
//
// The problem it solves is prompt size. The naive way to expose MCP
// tools to a model is to inject every discovered tool schema into
// every request — with N servers × M tools the prompt grows without
// bound and most of it is never used. This package defaults to
// *selector* mode instead: exactly two meta-tools are injected no
// matter how many servers or tools are configured:
//
//   - mcp_tools_get: list the discovered tools (optionally filtered
//     by server or by a substring), so the model can pick what it
//     needs;
//   - mcp_tools_execute: run one tool by name, with arguments.
//
// A `direct` mode remains available as an opt-in: it injects every
// discovered tool schema, for models/clients that work better with
// the full list in front of them.
//
// The agent loop (RunAgentLoop) is what makes selector mode work
// end to end: when the model answers with a meta-tool call, the
// gateway executes the MCP tool itself and calls the model again
// with the tool result appended — as an in-process provider call
// through the same Chat function the HTTP handler would call, never
// as a second HTTP request back through the gateway. The middleware
// chain (guardrails, ledger, cache, coalescing, auth) is therefore
// entered exactly once per client request, no matter how many tool
// iterations the loop runs. The loop is capped (default 10
// follow-up model calls); hitting the cap is an honest stop — the
// last model response is returned with Capped set, never a faked
// completion.
//
// Tool names on the wire are qualified as <server>__<tool>,
// sanitised to [A-Za-z0-9_-] (the OpenAI function-name alphabet)
// and truncated to 64 characters; collisions get a numeric suffix.
// mcp_tools_execute also accepts a plain tool name when it is
// unique across all configured servers.
//
// This package never touches HTTP, the config file, or the ledger:
// the caller supplies ToolServers (a static in-process one for
// tests, a stdio subprocess one for real MCP servers) and a ChatFunc.
// That is what makes "no HTTP re-entry" a structural property, not
// a convention — there is no HTTP client in this package to re-enter
// with.
package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ModeSelector injects only the two meta-tools. ModeDirect injects
// every discovered tool schema. Anything else resolves to selector.
const (
	ModeSelector = "selector"
	ModeDirect   = "direct"
)

// DefaultMaxIterations is the default cap on follow-up model calls
// in one agent loop. MaxIterationsLimit is the highest value the
// config accepts — the cap is a safety rail against a model that
// keeps calling tools forever, and an unbounded setting would
// defeat it.
const (
	DefaultMaxIterations = 10
	MaxIterationsLimit   = 50
)

// MetaGetTool lists the discovered MCP tools. MetaExecuteTool runs
// one of them. These are the only two tools selector mode ever
// injects, regardless of how many servers or tools are configured.
const (
	MetaGetTool     = "mcp_tools_get"
	MetaExecuteTool = "mcp_tools_execute"
)

// ChatFunc is one in-process model call — the same signature as the
// routing engine's Chat. The agent loop calls it directly; it must
// never be an HTTP round-trip back through the gateway.
type ChatFunc func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error)

// ToolServer is one configured MCP server: a source of tools plus
// an executor for them. The stdio implementation spawns a real MCP
// server subprocess; StaticServer is in-process (tests and local
// stubs). A server that cannot be reached reports the error from
// ListTools/CallTool instead of inventing tools.
type ToolServer interface {
	Name() string
	ListTools(ctx context.Context) ([]mcp.Tool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
}

// DiscoveredTool is one tool found on one server, with the qualified
// name the model actually calls.
type DiscoveredTool struct {
	Server      string         `json:"server"`
	Name        string         `json:"name"`
	Qualified   string         `json:"qualified_name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// ServerStatus is one configured server as the dashboard reports it:
// whether it answered tools/list, how many tools it exposes, and
// the last error when it did not (empty when healthy).
type ServerStatus struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Tools      int    `json:"tools"`
	LastError  string `json:"last_error,omitempty"`
	Discovered bool   `json:"discovered"`
}

// Stats are the live tool-call counters: real calls only, counted
// exactly where they happen (get/execute dispatch and loop runs).
// Denied counts the executions refused by the M9.6 controls
// (allowlist + budget) before any server was contacted.
type Stats struct {
	GetCalls        int64            `json:"get_calls"`
	ExecuteCalls    int64            `json:"execute_calls"`
	DeniedCalls     int64            `json:"denied_calls"`
	AllowlistDenied int64            `json:"allowlist_denied"`
	BudgetDenied    int64            `json:"budget_denied"`
	ExecuteByTool   map[string]int64 `json:"execute_by_tool"`
	LoopRuns        int64            `json:"loop_runs"`
	LoopIterations  int64            `json:"loop_iterations"`
	LoopCapped      int64            `json:"loop_capped"`
}

// LoopResult reports what one RunAgentLoop did: how many follow-up
// model calls it made, how many MCP tool calls were executed, and
// whether it stopped because the iteration cap was hit (Capped) —
// an honest stop, not a completion.
type LoopResult struct {
	Iterations int  `json:"iterations"`
	ToolCalls  int  `json:"tool_calls"`
	Capped     bool `json:"capped"`
}

var nonToolNameChar = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// maxNameSuffixAttempts bounds the numeric-suffix search in
// uniqueQualifiedName. With N colliding tools the search needs at
// most N tries per tool, so 10,000 covers any realistic registry
// with huge headroom; the hash fallback below guarantees
// termination even past it.
const maxNameSuffixAttempts = 10000

// uniqueQualifiedName returns a name of at most 64 characters
// that is not already in used, derived deterministically from
// base: base itself when free, else base truncated to make room
// for a "__<n>" suffix (n = 2, 3, …). Truncating first is what
// makes the suffix survive: the old code appended the suffix and
// then truncated to 64, so for a 64-char base the suffix was cut
// off, the candidate equalled base forever, and Discover looped
// infinitely. The search is bounded: after
// maxNameSuffixAttempts tries it falls back to a deterministic
// hash of (server, tool) — distinct per tool, same on every
// Discover run — tried with a bounded counter, so this function
// always returns.
func uniqueQualifiedName(base, server, tool string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	for n := 2; n <= maxNameSuffixAttempts; n++ {
		suffix := "__" + strconv.Itoa(n)
		keep := 64 - len(suffix)
		cand := base
		if len(cand) > keep {
			cand = cand[:keep]
		}
		cand += suffix
		if !used[cand] {
			return cand
		}
	}
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	for i := 0; i < 1000; i++ {
		suffix := "__" + hex.EncodeToString(sum[:6])
		if i > 0 {
			suffix += "-" + strconv.Itoa(i)
		}
		keep := 64 - len(suffix)
		cand := base
		if len(cand) > keep {
			cand = cand[:keep]
		}
		cand += suffix
		if !used[cand] {
			return cand
		}
	}
	// Unreachable in practice (it would need 1000 prior tools
	// carrying these exact hash suffixes); return the last
	// candidate rather than loop forever.
	suffix := "__" + hex.EncodeToString(sum[:6]) + "-999"
	keep := 64 - len(suffix)
	cand := base
	if len(cand) > keep {
		cand = cand[:keep]
	}
	return cand + suffix
}

// QualifyName builds the wire name for a tool on a server:
// <server>__<tool>, sanitised to the OpenAI function-name alphabet
// and truncated to 64 characters. Two different (server, tool)
// pairs that sanitise to the same string are distinguished by the
// caller (see Registry.Discover, which appends __2, __3, ...).
func QualifyName(server, tool string) string {
	q := nonToolNameChar.ReplaceAllString(server+"__"+tool, "_")
	if len(q) > 64 {
		q = q[:64]
	}
	return strings.Trim(q, "_")
}

// metaToolDef builds one meta-tool definition (OpenAI function
// shape) from a name, description and JSON-schema parameters.
func metaToolDef(name, desc string, params map[string]any) provider.ToolDefinition {
	raw, _ := json.Marshal(params)
	var def provider.ToolDefinition
	def.Type = "function"
	def.Function.Name = name
	def.Function.Description = desc
	def.Function.Parameters = raw
	return def
}

// MetaTools returns the two selector-mode meta-tools, in a stable
// order. The count is the contract: selector mode injects exactly
// these two, no matter how many servers or tools are configured.
func MetaTools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		metaToolDef(MetaGetTool,
			"List the MCP tools available through this router. Returns each tool's qualified name (server__tool), description and input schema. Call this first when you need a capability you do not have as a direct tool, then run the tool you picked with mcp_tools_execute.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server": map[string]any{"type": "string", "description": "Only list tools from this MCP server (exact server name). Omit for all servers."},
					"query":  map[string]any{"type": "string", "description": "Only list tools whose name or description contains this substring (case-insensitive). Omit for all tools."},
				},
			}),
		metaToolDef(MetaExecuteTool,
			"Run one MCP tool by its qualified name (server__tool, as returned by mcp_tools_get; a plain tool name works when it is unique across all servers) with its arguments object. Returns the tool's result as text.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tool":      map[string]any{"type": "string", "description": "Qualified tool name (server__tool) or unique plain tool name."},
					"arguments": map[string]any{"type": "object", "description": "Arguments for the tool, as a JSON object."},
				},
				"required": []string{"tool"},
			}),
	}
}

// Registry holds the configured MCP servers, the mode, and the live
// counters. Discovered tools are snapshotted by Discover (at
// startup and on explicit refresh): the selector/direct injection
// and mcp_tools_get both read the snapshot, so a request never
// blocks on a tools/list round-trip to every server.
type Registry struct {
	servers       []ToolServer
	mode          string
	maxIterations int

	mu         sync.RWMutex
	discovered []DiscoveredTool // snapshot, in server order then tool order
	statuses   []ServerStatus

	getCalls        atomic.Int64
	executeCalls    atomic.Int64
	allowlistDenied atomic.Int64
	budgetDenied    atomic.Int64
	loopRuns        atomic.Int64
	loopIterations  atomic.Int64
	loopCapped      atomic.Int64
	executeByToolMu sync.Mutex
	executeByTool   map[string]int64

	// M9.6 controls (see controls.go). controlsSet=false means
	// legacy New(): no controls engaged, everything allowed.
	// NewWithControls / SetControls engage them (fail-closed).
	controlsMu        sync.RWMutex
	controls          Controls
	controlsSet       bool
	budgetMu          sync.Mutex
	budgetCalls       int
	budgetWindowStart time.Time
}

// New builds a Registry. An unknown or empty mode resolves to
// selector (the default); maxIterations <= 0 resolves to
// DefaultMaxIterations, values above MaxIterationsLimit are clamped
// to it — the cap is a rail, not a dial without a stop.
//
// New engages NO M9.6 controls: every discovered tool may execute
// (the pre-M9.6 behaviour). This is the legacy constructor, kept
// so existing callers (internal/server's agent-loop wiring and
// its tests) keep working. New registries that must be hardened
// use NewWithControls, which is fail-closed from construction —
// see controls.go.
func New(servers []ToolServer, mode string, maxIterations int) *Registry {
	return newRegistry(servers, mode, maxIterations, Controls{}, false)
}

// NewWithControls builds a Registry with M9.6 controls engaged
// from construction: fail-closed — a tool executes only if the
// allowlist admits it (an empty Allow list denies everything),
// and the spend budget (MaxCalls / MaxDuration) caps dispatched
// executions. See controls.go for the exact semantics.
func NewWithControls(servers []ToolServer, mode string, maxIterations int, c Controls) *Registry {
	return newRegistry(servers, mode, maxIterations, c, true)
}

func newRegistry(servers []ToolServer, mode string, maxIterations int, c Controls, controlsSet bool) *Registry {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m != ModeDirect {
		m = ModeSelector
	}
	if maxIterations <= 0 {
		maxIterations = DefaultMaxIterations
	}
	if maxIterations > MaxIterationsLimit {
		maxIterations = MaxIterationsLimit
	}
	return &Registry{
		servers:           servers,
		mode:              m,
		maxIterations:     maxIterations,
		executeByTool:     map[string]int64{},
		controls:          c,
		controlsSet:       controlsSet,
		budgetWindowStart: time.Now(),
	}
}

// Mode returns the configured mode ("selector" or "direct").
func (r *Registry) Mode() string { return r.mode }

// MaxIterations returns the loop cap for this registry.
func (r *Registry) MaxIterations() int { return r.maxIterations }

// ServerCount is the number of configured MCP servers.
func (r *Registry) ServerCount() int { return len(r.servers) }

// Discover asks every configured server for its tools and stores
// the snapshot. A server that fails is recorded as down with its
// error and contributes no tools; the other servers still load —
// one dead server never hides the healthy ones. Discover is safe
// to call again (refresh): the snapshot and statuses are replaced.
func (r *Registry) Discover(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	discovered := []DiscoveredTool{}
	statuses := make([]ServerStatus, 0, len(r.servers))
	used := map[string]bool{}
	for _, srv := range r.servers {
		st := ServerStatus{Name: srv.Name(), Discovered: true}
		tools, err := srv.ListTools(ctx)
		if err != nil {
			st.OK = false
			st.LastError = err.Error()
			statuses = append(statuses, st)
			continue
		}
		st.OK = true
		st.Tools = len(tools)
		for _, t := range tools {
			// Sanitisation collisions get a numeric suffix so no
			// tool is ever silently dropped from the list the
			// model sees. uniqueQualifiedName truncates the base
			// to make room for the suffix and is bounded, so a
			// 64-char collision can never loop forever.
			q := uniqueQualifiedName(QualifyName(srv.Name(), t.Name), srv.Name(), t.Name, used)
			used[q] = true
			discovered = append(discovered, DiscoveredTool{
				Server: srv.Name(), Name: t.Name, Qualified: q,
				Description: t.Description, InputSchema: t.InputSchema,
			})
		}
		statuses = append(statuses, st)
	}
	r.discovered = discovered
	r.statuses = statuses
}

// Discovered returns a copy of the current tool snapshot.
func (r *Registry) Discovered() []DiscoveredTool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DiscoveredTool, len(r.discovered))
	copy(out, r.discovered)
	return out
}

// Statuses returns a copy of the per-server discovery statuses.
func (r *Registry) Statuses() []ServerStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ServerStatus, len(r.statuses))
	copy(out, r.statuses)
	return out
}

// Stats returns the live tool-call counters.
func (r *Registry) Stats() Stats {
	r.executeByToolMu.Lock()
	byTool := make(map[string]int64, len(r.executeByTool))
	for k, v := range r.executeByTool {
		byTool[k] = v
	}
	r.executeByToolMu.Unlock()
	return Stats{
		GetCalls:        r.getCalls.Load(),
		ExecuteCalls:    r.executeCalls.Load(),
		DeniedCalls:     r.allowlistDenied.Load() + r.budgetDenied.Load(),
		AllowlistDenied: r.allowlistDenied.Load(),
		BudgetDenied:    r.budgetDenied.Load(),
		ExecuteByTool:   byTool,
		LoopRuns:        r.loopRuns.Load(),
		LoopIterations:  r.loopIterations.Load(),
		LoopCapped:      r.loopCapped.Load(),
	}
}

// toolDef converts one discovered tool to an OpenAI function tool
// definition under its qualified name.
func (d DiscoveredTool) toolDef() provider.ToolDefinition {
	var def provider.ToolDefinition
	def.Type = "function"
	def.Function.Name = d.Qualified
	desc := d.Description
	if desc == "" {
		desc = "MCP tool " + d.Name + " on server " + d.Server + "."
	}
	def.Function.Description = desc
	if d.InputSchema != nil {
		raw, _ := json.Marshal(d.InputSchema)
		def.Function.Parameters = raw
	} else {
		def.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return def
}

// Inject appends the MCP tools for this registry's mode to req
// (in place) and returns how many were added. Selector mode adds
// exactly the two meta-tools; direct mode adds one definition per
// discovered tool. Tools whose name is already present are never
// duplicated — a client that supplied its own mcp_tools_get keeps
// it. With no servers configured nothing is injected: there is
// nothing to select from, so advertising the meta-tools would be
// a lie.
func (r *Registry) Inject(req *provider.ChatRequest) int {
	if len(r.servers) == 0 {
		return 0
	}
	have := map[string]bool{}
	for _, t := range req.Tools {
		have[t.Function.Name] = true
	}
	added := 0
	add := func(def provider.ToolDefinition) {
		if have[def.Function.Name] {
			return
		}
		have[def.Function.Name] = true
		req.Tools = append(req.Tools, def)
		added++
	}
	if r.mode == ModeDirect {
		for _, d := range r.Discovered() {
			add(d.toolDef())
		}
		return added
	}
	for _, def := range MetaTools() {
		add(def)
	}
	return added
}

// ListForGet implements mcp_tools_get: the discovered tools,
// optionally filtered by exact server name and/or a
// case-insensitive substring of the name or description. It counts
// one get call — this is the real counter the dashboard reports.
func (r *Registry) ListForGet(serverFilter, query string) []DiscoveredTool {
	r.getCalls.Add(1)
	out := []DiscoveredTool{}
	q := strings.ToLower(strings.TrimSpace(query))
	for _, d := range r.Discovered() {
		if serverFilter != "" && d.Server != serverFilter {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(d.Name), q) && !strings.Contains(strings.ToLower(d.Description), q) && !strings.Contains(strings.ToLower(d.Qualified), q) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// resolve maps a tool name as the model gave it to the discovered
// tool: a qualified name matches directly; a plain name matches
// only when exactly one discovered tool carries it, otherwise the
// call is ambiguous and the model is told so (it can disambiguate
// via mcp_tools_get).
func (r *Registry) resolve(name string) (DiscoveredTool, *ToolServer, error) {
	var plain []DiscoveredTool
	for _, d := range r.Discovered() {
		if d.Qualified == name {
			return d, r.serverByName(d.Server), nil
		}
		if d.Name == name {
			plain = append(plain, d)
		}
	}
	if len(plain) == 1 {
		return plain[0], r.serverByName(plain[0].Server), nil
	}
	if len(plain) > 1 {
		names := make([]string, 0, len(plain))
		for _, d := range plain {
			names = append(names, d.Qualified)
		}
		sort.Strings(names)
		return DiscoveredTool{}, nil, fmt.Errorf("tool %q is ambiguous: available as %s — use the qualified name", name, strings.Join(names, ", "))
	}
	return DiscoveredTool{}, nil, fmt.Errorf("unknown tool: %s", name)
}

func (r *Registry) serverByName(name string) *ToolServer {
	for i := range r.servers {
		if r.servers[i].Name() == name {
			return &r.servers[i]
		}
	}
	return nil
}

// Execute runs one discovered tool by qualified (or unique plain)
// name with the given arguments, and returns its result text. It
// counts one execute call per invocation, including failures — the
// dashboard counts attempts, not just successes.
//
// Execute is a thin wrapper over ExecuteDetailed: the M9.6
// controls (allowlist, spend budget) are enforced inside
// ExecuteDetailed, the single choke point every execution path
// funnels through (direct Execute calls, the agent loop's
// dispatchOne, and direct-mode tool calls). A denied call never
// reaches the server.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]any) (string, error) {
	res, err := r.ExecuteDetailed(ctx, name, args)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// ExecuteDetailed is the single execution choke point (M9.6).
// Order of checks — each one refuses BEFORE the server is
// contacted, so a refused call costs nothing and leaks nothing
// to the server:
//
//  1. resolve the name against the discovered snapshot (unknown
//     or ambiguous names fail here, server untouched);
//  2. allowlist: denied tools fail with ErrToolNotAllowed;
//  3. spend budget: an exhausted call-count cap or wall-clock
//     budget fails with ErrBudgetExhausted;
//  4. dispatch to the server (reserving one budget call first).
//
// The cost estimate is honest: it reports usage only when the
// server implements UsageReportingServer and returns usage; a
// plain server yields CostEstimate{UsageKnown:false} — cost
// UNKNOWN, never an invented number. A USD figure appears only
// when the server itself reports one.
func (r *Registry) ExecuteDetailed(ctx context.Context, name string, args map[string]any) (ExecutionResult, error) {
	r.executeCalls.Add(1)
	d, srv, err := r.resolve(name)
	if err != nil {
		return ExecutionResult{}, err
	}
	if srv == nil {
		return ExecutionResult{}, fmt.Errorf("tool %q has no server", name)
	}
	r.controlsMu.RLock()
	c, controlsSet := r.controls, r.controlsSet
	r.controlsMu.RUnlock()
	if !allowedByControls(c, controlsSet, d.Server, d.Name, d.Qualified) {
		r.allowlistDenied.Add(1)
		return ExecutionResult{}, fmt.Errorf("%w: tool %q on server %q is not allowlisted", ErrToolNotAllowed, d.Name, d.Server)
	}
	r.budgetMu.Lock()
	if err := r.checkBudgetLocked(c); err != nil {
		r.budgetMu.Unlock()
		r.budgetDenied.Add(1)
		return ExecutionResult{}, err
	}
	r.budgetMu.Unlock()
	if args == nil {
		args = map[string]any{}
	}
	start := time.Now()
	var out string
	var usage *ToolUsage
	if us, ok := (*srv).(UsageReportingServer); ok {
		out, usage, err = us.CallToolWithUsage(ctx, d.Name, args)
	} else {
		out, err = (*srv).CallTool(ctx, d.Name, args)
	}
	if err != nil {
		return ExecutionResult{}, err
	}
	r.executeByToolMu.Lock()
	r.executeByTool[d.Qualified]++
	r.executeByToolMu.Unlock()
	cost := UnknownCost()
	if usage != nil {
		cost = CostEstimate{
			UsageKnown:   true,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			USDKnown:     usage.HasCost,
			USD:          usage.CostUSD,
		}
	}
	return ExecutionResult{
		Text:      out,
		Cost:      cost,
		Duration:  time.Since(start),
		Server:    d.Server,
		Tool:      d.Name,
		Qualified: d.Qualified,
	}, nil
}

// handlesToolCall reports whether a tool call is one this registry
// can execute: a meta-tool, a qualified discovered tool, or a plain
// name matching at least one discovered tool (an ambiguous plain
// name still counts — dispatch answers it with the ambiguity error
// so the model can disambiguate). A client's own tool (defined by
// the client, not by any MCP server) is NOT handled: the loop
// returns that response to the client untouched, so the normal
// OpenAI client-driven tool flow keeps working.
func (r *Registry) handlesToolCall(name string) bool {
	if name == MetaGetTool || name == MetaExecuteTool {
		return true
	}
	for _, d := range r.Discovered() {
		if d.Qualified == name || d.Name == name {
			return true
		}
	}
	return false
}

// allMCP reports whether every tool call in the response is one
// this registry can execute.
func (r *Registry) allMCP(calls []wire.ToolCall) bool {
	for _, tc := range calls {
		if !r.handlesToolCall(tc.Function.Name) {
			return false
		}
	}
	return true
}

// send back as the tool result. Unknown tools and execution errors
// become an "error: ..." result, never a failed request — the model
// gets to see the failure and decide what to do next.
func (r *Registry) dispatchOne(ctx context.Context, tc wire.ToolCall) string {
	name := tc.Function.Name
	var args map[string]any
	if strings.TrimSpace(tc.Function.Arguments) != "" {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("error: arguments for %s are not a JSON object: %v", name, err)
		}
	}
	switch name {
	case MetaGetTool:
		serverFilter, _ := args["server"].(string)
		query, _ := args["query"].(string)
		tools := r.ListForGet(serverFilter, query)
		raw, _ := json.Marshal(map[string]any{"tools": tools})
		return string(raw)
	case MetaExecuteTool:
		tool, _ := args["tool"].(string)
		if tool == "" {
			return "error: mcp_tools_execute requires a \"tool\" argument"
		}
		var toolArgs map[string]any
		if v, ok := args["arguments"]; ok {
			if m, ok := v.(map[string]any); ok {
				toolArgs = m
			} else {
				return "error: mcp_tools_execute \"arguments\" must be a JSON object"
			}
		}
		out, err := r.Execute(ctx, tool, toolArgs)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return out
	default:
		// Direct-mode tool call: a qualified (or unique plain)
		// discovered tool name. Meta-tool names are handled
		// above, so anything else here is a real tool call.
		out, err := r.Execute(ctx, name, args)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return out
	}
}

// RunAgentLoop runs one chat request with the MCP tools injected,
// driving follow-up model calls in-process until the model answers
// with plain text (or a non-tool finish) or the iteration cap is
// hit. Every follow-up is a direct chat(ctx, req) call — the
// gateway's own engine function — never an HTTP request, so the
// middleware chain is entered exactly once per client request.
//
// The first model call is not an iteration; Iterations counts only
// follow-up calls. When the cap is hit with tool calls still
// pending, the last response is returned with Capped=true: the loop
// stops honestly instead of inventing a final answer or looping
// forever. An error from chat mid-loop aborts the loop and is
// returned (the caller maps it like any other engine error).
func (r *Registry) RunAgentLoop(ctx context.Context, chat ChatFunc, req provider.ChatRequest) (*provider.ChatResponse, LoopResult, error) {
	if chat == nil {
		return nil, LoopResult{}, fmt.Errorf("mcptools: nil ChatFunc — the agent loop has no in-process model call")
	}
	r.loopRuns.Add(1)
	work := req
	r.Inject(&work)
	result := LoopResult{}

	resp, err := chat(ctx, work)
	if err != nil {
		return nil, result, err
	}
	if resp == nil {
		return nil, result, fmt.Errorf("mcptools: model call returned no response")
	}
	for len(resp.ToolCalls) > 0 {
		// A response that calls a tool this registry cannot
		// execute (the client's own tool) is returned untouched:
		// the client drives those calls itself, exactly as
		// before MCP existed.
		if !r.allMCP(resp.ToolCalls) {
			return resp, result, nil
		}
		if result.Iterations >= r.maxIterations {
			result.Capped = true
			r.loopCapped.Add(1)
			return resp, result, nil
		}
		// The assistant turn that made the calls, then one tool
		// result per call — the standard OpenAI tool-result
		// shape, built in-process.
		work.Messages = append(work.Messages, provider.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})
		for i, tc := range resp.ToolCalls {
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", result.Iterations, i)
			}
			text := r.dispatchOne(ctx, tc)
			result.ToolCalls++
			work.Messages = append(work.Messages, provider.Message{
				Role:       "tool",
				Content:    text,
				ToolCallID: id,
			})
		}
		result.Iterations++
		r.loopIterations.Add(1)
		resp, err = chat(ctx, work)
		if err != nil {
			return nil, result, err
		}
		if resp == nil {
			return nil, result, fmt.Errorf("mcptools: model call returned no response")
		}
	}
	return resp, result, nil
}

// Snapshot is the dashboard feed for this registry: mode, cap,
// per-server status, the two selector tools, the full direct tool
// list, and the live counters. Nothing here is invented — with no
// servers configured the lists are empty and the counters zero.
// Controls / ControlsSet / Budget expose the M9.6 hardening state
// (ControlsSet=false means the legacy unhardened registry).
type Snapshot struct {
	Mode          string           `json:"mode"`
	MaxIterations int              `json:"max_iterations"`
	Servers       []ServerStatus   `json:"servers"`
	SelectorTools []string         `json:"selector_tools"`
	DirectTools   []DiscoveredTool `json:"direct_tools"`
	DirectCount   int              `json:"direct_tool_count"`
	Stats         Stats            `json:"tool_calls"`
	Controls      Controls         `json:"controls,omitempty"`
	ControlsSet   bool             `json:"controls_set"`
	Budget        BudgetStatus     `json:"budget"`
}

// Snapshot builds the dashboard view of this registry.
func (r *Registry) Snapshot() Snapshot {
	direct := r.Discovered()
	if direct == nil {
		direct = []DiscoveredTool{}
	}
	servers := r.Statuses()
	if servers == nil {
		servers = []ServerStatus{}
	}
	selector := []string{MetaGetTool, MetaExecuteTool}
	controls, controlsSet := r.Controls()
	return Snapshot{
		Mode:          r.mode,
		MaxIterations: r.maxIterations,
		Servers:       servers,
		SelectorTools: selector,
		DirectTools:   direct,
		DirectCount:   len(direct),
		Stats:         r.Stats(),
		Controls:      controls,
		ControlsSet:   controlsSet,
		Budget:        r.BudgetStatus(),
	}
}
