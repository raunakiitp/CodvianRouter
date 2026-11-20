// Package mcp — M9.5: tool_search + heartbeat tools, protocol 2025-03-26,
// and server-side tool-cardinality controls.
//
// # tool_search (scope mcp:read:status)
//
// Searches the registered tool list (Tools()) by case-insensitive
// substring over name and description, and returns matching tools
// with their descriptions and required scopes. An empty query
// returns the full (visible) list; no match returns an empty list,
// not an error.
//
// Scope reasoning (for the M9.1 reviewer): tool_search is a
// read-only introspection over the tool catalog. It discloses
// nothing tools/list does not already disclose to any caller —
// tool names and descriptions — plus each tool's required scopes,
// which are authorization metadata, not secrets (a scope denial
// already names the missing scopes, see scopes.go). It mutates
// nothing, calls no provider, and can never spend quota. A
// dedicated narrower scope would fragment the 9-scope set for a
// read that reveals nothing beyond tools/list, so it takes the
// status-read scope, the same scope as router_status. It does NOT
// filter results by the caller's scopes, for the same reason
// tools/list does not: listing is not calling, and the scope
// check still gates every tools/call.
//
// # heartbeat (scope mcp:read:status)
//
// A liveness snapshot, not a background writer: uptime (from
// Source.Status()), announced tool count, protocol version, and
// audit-log state (enabled/disabled). It uses only Source.Status()
// + Tools() — no new Source methods. (OmniRoute's heartbeat is a
// 5 s background writer to runtime/mcp-heartbeat.json; ours is a
// read-only tool because this server has no serving loop to tick
// from — the stdio transport is request-driven. A file-writing
// heartbeat belongs with a daemon transport, M9.2, not here.)
//
// # Protocol 2025-03-26 — what changed for OUR surface
//
// Sources: the MCP specification, revision 2025-03-26
// (modelcontextprotocol.io/specification/2025-03-26), and the
// revision history at dorokhovich.com/blog/mcp-revision-history
// (which tracks the spec's own changelog). The 2025-03-26
// revision adds: (1) Streamable HTTP, replacing HTTP+SSE
// (deprecated from this revision); (2) an OAuth 2.1
// authorization framework (PKCE mandatory); (3) JSON-RPC
// batching; (4) tool annotations (readOnlyHint,
// destructiveHint, idempotentHint, openWorldHint); (5) audio
// content and an argument-completion capability. What each
// means for the surface this server implements — initialize,
// capabilities, tools/list, tools/call, ping, notifications —
// is:
//
//   - initialize / capabilities / tools/list / tools/call /
//     ping / notifications: wire shapes UNCHANGED. The
//     initialize response (protocolVersion + capabilities +
//     serverInfo), the tools/list entries, and the tools/call
//     result envelope are the same under 2025-03-26 as under
//     2024-11-05. The bump is the version string itself.
//   - Streamable HTTP: a TRANSPORT change, not a method change.
//     This server is stdio-only; the Streamable HTTP endpoint
//     is M9.2's slice (SSE + Streamable HTTP). Nothing to
//     implement here — the bump aligns the version the future
//     transport will speak.
//   - OAuth 2.1: NOT APPLICABLE. Our remote auth is gateway
//     keys (M9.2); there is no OAuth flow to add, and no
//     client-registration surface exists in this package.
//   - JSON-RPC batching: DELIBERATELY NOT IMPLEMENTED. It was
//     added in 2025-03-26 and REMOVED again in 2025-06-18
//     ("batching, before it was three months old" —
//     dorokhovich). Implementing a feature the next revision
//     deletes would be wrong, and our stdio transport is
//     one-message-per-line, so a JSON array batch has no
//     framing to arrive in.
//   - Tool annotations: APPLICABLE IN PRINCIPLE, NOT
//     IMPLEMENTED HERE. All our tools are read-only today, so
//     the annotation would be readOnlyHint=true on every entry
//     (and destructiveHint=true on M9.4's destructive tools).
//     Carrying it needs an `annotations` field on the Tool
//     struct in mcp.go, which is outside this slice's files —
//     flagged as a coordinator follow-up, not silently skipped.
//   - Audio content / completion: NOT APPLICABLE. We ship no
//     sampling, resources, or prompts surface, and no audio
//     content anywhere.
//
// # Cardinality controls — design choice
//
// A per-server filter, set with the Server options
// WithToolFilter(allow, deny []string) and WithMaxTools(n int).
// The filter state lives in a package-level map keyed by *Server
// AND by the server's Source (this file owns both maps; the
// Server struct in mcp.go gains no field, per this slice's file
// ownership). The dual key is forced by dispatch: tools/call
// runs handlers on s.withIdentity(id), a shallow copy of the
// server, so a handler's *Server pointer never matches the
// pointer WithToolFilter was called on — but the copy shares
// the Source, so the Source key is how the copy finds the
// filter. See the sourceFilters comment for the full reasoning,
// the shared-Source edge, and the struct-field follow-up.
// Slices are copied on set, so a caller mutating its slice
// afterwards cannot widen the filter — the same defensive-copy
// discipline as M9.1's identity (N3). Servers are long-lived
// singletons in practice (one stdio server per process); the
// maps hold one small entry per server and are never pruned —
// acceptable for that lifetime, and noted here so a future
// per-request-server transport (M9.2) knows to revisit it.
//
// Semantics (documented, deterministic, Tools() order preserved):
//
//   - No option set: everything visible (the default —
//     announcing the full manifest, as today).
//   - Deny wins over allow (OmniRoute's reduceToolManifest rule
//     3, toolCardinality.ts): a tool in both lists is invisible.
//   - Allow non-empty: ONLY the listed tools are visible
//     (exclusive allow-list mode). This differs deliberately
//     from OmniRoute, where allowTools is additive (allow-listed
//     tools PLUS scope-matched tools): our scope system already
//     authorizes per call, and cardinality here is announcement
//     control — an operator who sets an allow-list means "announce
//     exactly these". Unknown names in either list are ignored
//     silently (a deny of a tool that does not exist is a no-op).
//   - MaxTools <= 0: no cap (OmniRoute's own guard: a negative
//     max is "no cap", never a silent tail-drop). MaxTools > 0:
//     keep the first MaxTools in Tools() order (builtins first,
//     then extras sorted by name). This differs from OmniRoute's
//     cap, which prioritises allow-listed entries first: with an
//     exclusive allow-list the set is already restricted, so
//     Tools() order is the deterministic order and needs no
//     second sort.
//   - A filtered-out tool is INVISIBLE, never merely denied:
//     tools/list omits it, tool_search never returns it, the
//     heartbeat tool count excludes it, and tools/call answers
//     -32602 "unknown tool" — NOT -32002, because it is not a
//     scope failure, and not -32001 either. Indistinguishability
//     is the point: a filtered tool must be indistinguishable
//     from a tool that was never registered, so the visibility
//     check MUST run BEFORE the scope check in tools/call —
//     otherwise a caller without the scope would get -32002 and
//     learn the tool exists.
//
// WIRING (landed in the M9 merge wave): handleWithIdentity in
// mcp.go calls s.visibleTools() for tools/list and checks
// s.isToolVisible(params.Name) BEFORE the RequiredScopes check
// in tools/call, answering -32602 unknown tool for a
// filtered-out tool. The seam lives here — visibleTools() and
// isToolVisible() — so the filter is enforced identically in
// tools/list, tools/call, tool_search, and heartbeat.
package mcp

import (
	"reflect"
	"strings"
	"sync"
)

func init() {
	// Scopes FIRST, then RegisterTool — RegisterTool panics
	// otherwise (M9.1 rule: no tool reachable without a scope
	// assignment). Both tools are read-only status reads; see
	// the scope reasoning in the package doc above.
	RegisterToolScopes("tool_search", ScopeReadStatus)
	RegisterTool(Tool{
		Name:        "tool_search",
		Description: "Search the registered MCP tool catalog by name or description substring; returns matching tools with their descriptions and required scopes. An empty query returns the full catalog.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Case-insensitive substring to match against tool names and descriptions. Empty or absent returns the full catalog.",
				},
			},
		},
	}, toolSearch)

	RegisterToolScopes("heartbeat", ScopeReadStatus)
	RegisterTool(Tool{
		Name:        "heartbeat",
		Description: "Live liveness snapshot: server uptime, announced tool count, protocol version, and audit-log state (enabled/disabled). Read-only; calls no provider.",
		InputSchema: emptySchema(),
	}, toolHeartbeat)
}

// toolSearchHit is one tool_search result entry: the tool's name,
// description, and required scopes. Scopes is always an array
// (never null) — a tool with no scope assignment (impossible via
// RegisterTool, defensive only) reports an empty list.
type toolSearchHit struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Scopes      []string `json:"scopes"`
}

// searchTools returns the tools in tools whose name or description
// contains query as a case-insensitive substring. An empty query
// (after trimming spaces) returns the full list, in input order.
// No match returns an empty (non-nil) list, not an error. Matching
// is plain substring only — the query is never compiled as a
// regexp (Anti-ReDoS; same rule as OmniRoute's searchTools, which
// uses indexOf loops for the same reason). tools is not modified.
func searchTools(tools []Tool, query string) []toolSearchHit {
	q := strings.ToLower(strings.TrimSpace(query))
	out := []toolSearchHit{}
	for _, t := range tools {
		if q != "" &&
			!strings.Contains(strings.ToLower(t.Name), q) &&
			!strings.Contains(strings.ToLower(t.Description), q) {
			continue
		}
		scopes, known := RequiredScopes(t.Name)
		if !known {
			scopes = []string{}
		}
		out = append(out, toolSearchHit{
			Name:        t.Name,
			Description: t.Description,
			Scopes:      scopes,
		})
	}
	return out
}

// toolSearch is the tool_search handler: args["query"] (optional,
// must be a string when present) over the server's visible tools
// (cardinality-filtered — a filtered-out tool is never returned).
// A non-string query is an invalid-params error (-32602 via the
// dispatcher's handler-error path).
func toolSearch(s *Server, args map[string]any) (map[string]any, string) {
	query := ""
	if args != nil {
		if raw, ok := args["query"]; ok {
			q, ok := raw.(string)
			if !ok {
				return nil, "tool_search: query must be a string"
			}
			query = q
		}
	}
	hits := searchTools(s.visibleTools(), query)
	return toolResult(map[string]any{
		"query": query,
		"count": len(hits),
		"tools": hits,
	}), ""
}

// toolHeartbeat is the heartbeat handler: uptime from
// Source.Status(), the announced (cardinality-filtered) tool
// count, the protocol version, and the audit-log state. No new
// Source methods; a nil audit log reports "disabled", never an
// error — heartbeat must not fail because auditing is off.
func toolHeartbeat(s *Server, args map[string]any) (map[string]any, string) {
	auditState := "disabled"
	if s.audit != nil {
		auditState = "enabled"
	}
	return toolResult(map[string]any{
		"uptime_seconds":   s.src.Status().UptimeSeconds,
		"tool_count":       len(s.visibleTools()),
		"protocol_version": ProtocolVersion,
		"audit_log":        auditState,
	}), ""
}

// --- Cardinality controls -------------------------------------
//
// ToolFilter is one server's tool-announcement filter. Allow and
// Deny hold tool names; MaxTools caps the announced count
// (<= 0 means no cap). The zero value filters nothing.

type ToolFilter struct {
	Allow    []string
	Deny     []string
	MaxTools int
}

var (
	serverFiltersMu sync.RWMutex
	// serverFilters is the per-server filter state, keyed by the
	// server pointer. It lives here (not on the Server struct)
	// because mcp.go is owned to the ProtocolVersion line by
	// this slice. Entries are created lazily by the With*
	// options and never pruned: servers are long-lived
	// singletons in practice, so one small entry per server is
	// the honest cost — a future per-request-server transport
	// (M9.2) must revisit this before creating servers per
	// request.
	serverFilters = map[*Server]*ToolFilter{}
	// sourceFilters is the SAME filter state keyed by the
	// server's Source. It exists because of a real dispatch
	// fact found while testing this slice (M9.4): tools/call
	// dispatches handlers on s.withIdentity(id) — a SHALLOW
	// COPY of the server (mcp.go: `cp := *s`), so a handler
	// runs on a *Server pointer that was never passed to
	// WithToolFilter and has no entry in serverFilters. The
	// copy shares exactly the field values — src, version,
	// guard, audit — so the filter must ALSO be reachable
	// through one of them. Source is the only always-present
	// shared value (audit/guard are nil unless wired), so the
	// With* options write the filter under both keys, and
	// toolFilter() prefers the exact server entry and falls
	// back to the Source entry. Consequences, all documented:
	//   - Two servers over DIFFERENT Sources stay independent
	//     everywhere (the normal case: every New() in the
	//     tree gets its own Source value).
	//   - Two servers over the SAME Source share the filter
	//     on the handler path (last writer wins); their direct
	//     visibleTools()/isToolVisible() calls still use their
	//     own server entry. Servers over one Source serve one
	//     backend, so one announcement policy for them is the
	//     sane default — but a per-transport filter over a
	//     shared Source needs the struct-field home below.
	//   - A Source whose dynamic type is uncomparable cannot
	//     be a map key: the Source entry is then skipped (no
	//     panic — see sourceFilterKey), the server entry still
	//     works for direct calls, and the handler path on a
	//     withIdentity copy degrades to unfiltered. Every
	//     Source in the tree today is a pointer or an empty
	//     struct, both comparable.
	// The cleaner long-term home is a field on the Server
	// struct (which withIdentity copies for free) — a
	// coordinator-owned mcp.go change, flagged here, not
	// smuggled in through this slice's files.
	sourceFilters = map[Source]*ToolFilter{}
)

// setServerFilter stores f for server s under both keys (the
// server pointer and its Source — see the sourceFilters comment
// for why both exist). The caller must hold serverFiltersMu for
// writing. Both slices are copied, so the caller cannot widen
// the filter by mutating its slice afterwards (M9.1 N3
// discipline).
func setServerFilter(s *Server, f *ToolFilter) {
	serverFilters[s] = f
	if key, ok := sourceFilterKey(s.src); ok {
		sourceFilters[key] = f
	}
}

// sourceFilterKey reports whether src can be a map key (its
// dynamic type is comparable) and returns it as a Source key.
// A nil or uncomparable Source has no key: the filter is then
// server-keyed only (see the sourceFilters comment).
func sourceFilterKey(src Source) (Source, bool) {
	if src == nil {
		return nil, false
	}
	if !reflect.TypeOf(src).Comparable() {
		return nil, false
	}
	return src, true
}

// WithToolFilter sets this server's allow/deny tool lists and
// returns the server, for chaining. An allow-list is exclusive
// (only listed tools are announced); a deny-list removes the
// listed tools; deny wins over allow. Either list may be nil or
// empty. Both slices are copied, so the caller cannot widen the
// filter by mutating its slice afterwards (M9.1 N3 discipline).
// The filter does not change scope enforcement: a visible tool
// still requires its scopes on tools/call, and a filtered-out
// tool answers -32602 unknown tool rather than a scope denial.
func (s *Server) WithToolFilter(allow, deny []string) *Server {
	serverFiltersMu.Lock()
	defer serverFiltersMu.Unlock()
	f := serverFilters[s]
	if f == nil {
		f = &ToolFilter{}
	}
	f.Allow = append([]string(nil), allow...)
	f.Deny = append([]string(nil), deny...)
	setServerFilter(s, f)
	return s
}

// WithMaxTools caps the number of tools this server announces
// (tools/list, tool_search, heartbeat count). n <= 0 means no
// cap. The cap keeps the first n tools in Tools() order. It
// composes with WithToolFilter: allow/deny apply first, then
// the cap. Returns the server, for chaining.
func (s *Server) WithMaxTools(n int) *Server {
	serverFiltersMu.Lock()
	defer serverFiltersMu.Unlock()
	f := serverFilters[s]
	if f == nil {
		f = &ToolFilter{}
	}
	f.MaxTools = n
	setServerFilter(s, f)
	return s
}

// toolFilter returns a copy of this server's filter (nil when no
// option was ever set — no filtering). The copy keeps callers
// from mutating the stored lists. Lookup prefers the exact
// server entry; a withIdentity copy (which shares the Source
// but not the pointer — see the sourceFilters comment) falls
// back to the Source entry.
func (s *Server) toolFilter() *ToolFilter {
	serverFiltersMu.RLock()
	defer serverFiltersMu.RUnlock()
	f := serverFilters[s]
	if f == nil {
		if key, ok := sourceFilterKey(s.src); ok {
			f = sourceFilters[key]
		}
	}
	if f == nil {
		return nil
	}
	cp := &ToolFilter{MaxTools: f.MaxTools}
	cp.Allow = append([]string(nil), f.Allow...)
	cp.Deny = append([]string(nil), f.Deny...)
	return cp
}

// applyToolFilter returns the visible subset of tools under f
// (nil f: everything, unchanged order). Pure and deterministic,
// so it is unit-testable without a server: deny wins over
// allow, an exclusive allow-list keeps only listed names,
// unknown names are ignored, and MaxTools > 0 keeps the first
// MaxTools in input order. The input slice is not modified.
func applyToolFilter(tools []Tool, f *ToolFilter) []Tool {
	if f == nil {
		return tools
	}
	deny := make(map[string]bool, len(f.Deny))
	for _, name := range f.Deny {
		deny[name] = true
	}
	allow := make(map[string]bool, len(f.Allow))
	for _, name := range f.Allow {
		allow[name] = true
	}
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if deny[t.Name] {
			continue
		}
		if len(allow) > 0 && !allow[t.Name] {
			continue
		}
		out = append(out, t)
	}
	if f.MaxTools > 0 && len(out) > f.MaxTools {
		out = out[:f.MaxTools]
	}
	return out
}

// visibleTools returns the tools this server announces: Tools()
// under this server's filter, in Tools() order. This is what
// the tools/list case in mcp.go calls, so a filtered-out tool
// is omitted there too.
func (s *Server) visibleTools() []Tool {
	return applyToolFilter(Tools(), s.toolFilter())
}

// isToolVisible reports whether name is announced by this server
// under its filter. It consults visibleTools() — the same list
// tools/list announces — so the two can never disagree. This is
// what the tools/call case in mcp.go checks BEFORE the scope
// check: a filtered-out tool must answer -32602 unknown tool,
// indistinguishable from a tool that was never registered.
func (s *Server) isToolVisible(name string) bool {
	for _, t := range s.visibleTools() {
		if t.Name == name {
			return true
		}
	}
	return false
}
