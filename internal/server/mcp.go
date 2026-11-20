// MCP + CLI state feed (Slice 5): GET /dashboard/mcp.
//
// The dashboard's "MCP & CLI" section renders this feed: the MCP
// tools the stdio server offers (the same mcp.Tools() list the
// server itself serves, so the two can never drift apart), how to
// start it, and live provider/model counts from the registry. Like
// the other dashboard feeds it is unauthenticated by design: the
// payload carries no secrets — tool names and descriptions, counts,
// and the listen address only.
//
// CHECKLIST 141 (MCP selector mode) extends the feed with the
// selector state: the mode (selector/direct/off), the loop cap,
// the configured MCP servers (name, health, tool count, last
// error), the two selector meta-tools vs the full direct tool
// list, and the live tool-call counters from the registry. With no
// MCP registry wired the mode is "off" and the lists are empty —
// an honest empty state, not an error.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/mcptools"
)

// mcpToolFeed is one MCP tool as the dashboard feed reports it:
// the same definition the stdio server offers (via mcp.Tools())
// plus the scopes a caller needs to invoke it (via
// mcp.RequiredScopes, M9.1). A tool with no scope assignment can
// never exist — RegisterTool panics without one — so Scopes is
// never empty here; an empty list would mean the feed and the
// scope table drifted apart, and the dashboard would show it as
// such instead of inventing a scope.
type mcpToolFeed struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Scopes      []string       `json:"scopes"`
}

// mcpTransportState is one remote transport as GET/POST
// /api/mcp/status reports it (the M9.2 shape — see
// handleMCPStatus): enabled is the operator toggle, sessions is
// the live session count from the transport state
// (mcpHTTPState.counts in mcp_http.go), swept for idle sessions
// when due (at most once per 60 s) — never a placeholder.
type mcpTransportState struct {
	Enabled  bool `json:"enabled"`
	Sessions int  `json:"sessions"`
}

// mcpStatusResponse is the GET/POST /api/mcp/status payload,
// exactly {"sse":{"enabled":bool,"sessions":n},
// "streamable":{"enabled":bool,"sessions":n}}.
type mcpStatusResponse struct {
	SSE        mcpTransportState `json:"sse"`
	Streamable mcpTransportState `json:"streamable"`
}

// mcpAuditResponse is the GET /api/mcp/audit payload: paginated
// audit entries, newest first, exactly as stored — inputs are
// SHA-256 hashes and outputs truncated to 200 runes, so no raw
// argument ever leaves the server through this feed. Available
// is false when no audit log is wired (honest empty state).
// Cap is the rotation cap (M9.7) and Dropped counts entries
// rotation removed in this process's lifetime, so a rotation is
// visible in the dashboard instead of silently dropping history.
type mcpAuditResponse struct {
	Available bool             `json:"available"`
	Entries   []mcp.AuditEntry `json:"entries"`
	Total     int              `json:"total"`
	Limit     int              `json:"limit"`
	Offset    int              `json:"offset"`
	Cap       int              `json:"cap"`
	Dropped   int              `json:"dropped"`
}

// mcpServerFeed is one configured MCP server as the feed reports
// it: health and tool count only — never a command line (a command
// can embed a path or argument the operator may not want published
// on the dashboard feed) and never an error body beyond the
// server's own short failure message.
type mcpServerFeed struct {
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	Tools     int    `json:"tools"`
	LastError string `json:"last_error,omitempty"`
}

// mcpToolCallsFeed carries the live tool-call counters: real calls
// only, counted where they happen in the registry.
type mcpToolCallsFeed struct {
	GetCalls       int64            `json:"get_calls"`
	ExecuteCalls   int64            `json:"execute_calls"`
	ExecuteByTool  map[string]int64 `json:"execute_by_tool"`
	LoopRuns       int64            `json:"loop_runs"`
	LoopIterations int64            `json:"loop_iterations"`
	LoopCapped     int64            `json:"loop_capped"`
}

// mcpResponse is the GET /dashboard/mcp payload.
type mcpResponse struct {
	Transport string        `json:"transport"`
	Command   string        `json:"command"`
	Tools     []mcpToolFeed `json:"tools"`
	Providers int           `json:"providers"`
	Models    int           `json:"models"`
	Listen    string        `json:"listen"`
	// Ready is false when no registry is wired (the feed still
	// answers, with zero counts — an honest empty state, not an
	// error, so the dashboard section can say so).
	Ready bool `json:"ready"`

	// Selector mode (CHECKLIST 141). Mode is "selector" or
	// "direct" when a registry is wired, "off" when none is.
	// MaxIterations is the loop cap (0 when off). Servers lists
	// the configured MCP servers; SelectorTools is always the two
	// meta-tools; DirectTools is every discovered tool with its
	// qualified name; ToolCalls are the live counters.
	Mode          string                    `json:"mode"`
	MaxIterations int                       `json:"max_iterations"`
	Servers       []mcpServerFeed           `json:"servers"`
	SelectorTools []string                  `json:"selector_tools"`
	DirectTools   []mcptools.DiscoveredTool `json:"direct_tools"`
	DirectCount   int                       `json:"direct_tool_count"`
	ToolCalls     mcpToolCallsFeed          `json:"tool_calls"`
}

// handleMCP serves GET /dashboard/mcp: the MCP tool list + live
// counts. Providers come from the dashboard provider lister, models
// from the model registry (falling back to the WithModels list when
// no registry is configured — the same fallback GET /v1/models
// uses).
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	providers := 0
	if s.dashboardProviders != nil {
		providers = len(s.dashboardProviders.DashboardProviders())
	}
	models := 0
	ready := false
	if s.registry != nil {
		if list, err := s.registry.Models(context.Background()); err == nil {
			models = len(list)
			ready = true
		}
	} else if len(s.models) > 0 {
		models = len(s.models)
		ready = true
	}
	if s.dashboardProviders != nil {
		ready = true
	}
	listen := s.listenAddr
	if listen == "" {
		listen = "unknown"
	}
	resp := mcpResponse{
		Transport: "stdio",
		Command:   "codvian-router mcp",
		Tools:     mcpToolsFeed(),
		Providers: providers,
		Models:    models,
		Listen:    listen,
		Ready:     ready,
		Mode:      "off",
		Servers:   []mcpServerFeed{},
		SelectorTools: []string{
			mcptools.MetaGetTool,
			mcptools.MetaExecuteTool,
		},
		DirectTools: []mcptools.DiscoveredTool{},
	}
	if s.mcpTools != nil {
		snap := s.mcpTools.Snapshot()
		resp.Mode = snap.Mode
		resp.MaxIterations = snap.MaxIterations
		resp.DirectTools = snap.DirectTools
		resp.DirectCount = snap.DirectCount
		for _, st := range snap.Servers {
			resp.Servers = append(resp.Servers, mcpServerFeed{
				Name: st.Name, OK: st.OK, Tools: st.Tools, LastError: st.LastError,
			})
		}
		resp.ToolCalls = mcpToolCallsFeed{
			GetCalls:       snap.Stats.GetCalls,
			ExecuteCalls:   snap.Stats.ExecuteCalls,
			ExecuteByTool:  snap.Stats.ExecuteByTool,
			LoopRuns:       snap.Stats.LoopRuns,
			LoopIterations: snap.Stats.LoopIterations,
			LoopCapped:     snap.Stats.LoopCapped,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// mcpToolsFeed builds the dashboard tool list from the same
// mcp.Tools() the stdio server offers, each entry carrying its
// required scopes from the M9.1 scope table — so the dashboard,
// the server, and the scope table can never drift apart. Tools
// registered later by M9.3/M9.4/M9.5 appear here automatically
// (Tools() + RequiredScopes are the single source of truth).
func mcpToolsFeed() []mcpToolFeed {
	tools := mcp.Tools()
	out := make([]mcpToolFeed, 0, len(tools))
	for _, t := range tools {
		scopes, _ := mcp.RequiredScopes(t.Name)
		if scopes == nil {
			scopes = []string{}
		}
		out = append(out, mcpToolFeed{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Scopes:      scopes,
		})
	}
	return out
}

// mcpTransportStatus snapshots the remote-transport toggle state
// (M9.7 owns the toggles) with the LIVE session counts from the
// transport state: mcpHTTPStateFor(s).counts() reads the M9.2
// session tables, sweeping idle sessions when due, so the status
// endpoint reports real sessions, not a hardcoded 0.
func (s *Server) mcpTransportStatus() mcpStatusResponse {
	s.mcpTransportMu.Lock()
	sseEnabled, streamEnabled := s.mcpSSEEnabled, s.mcpStreamEnabled
	s.mcpTransportMu.Unlock()
	sse, streamable := mcpHTTPStateFor(s).counts()
	return mcpStatusResponse{
		SSE:        mcpTransportState{Enabled: sseEnabled, Sessions: sse},
		Streamable: mcpTransportState{Enabled: streamEnabled, Sessions: streamable},
	}
}

// handleMCPStatus serves GET + POST /api/mcp/status (M9.7
// dashboard toggles, M9.2 shape):
//
//	GET  → {"sse":{"enabled":bool,"sessions":n},
//	         "streamable":{"enabled":bool,"sessions":n}}
//	POST {"transport":"sse"|"streamable","enabled":bool}
//	     → the same status shape after flipping the toggle.
//
// The toggle records the operator's intent only. The transports
// themselves are M9.2's: enabling a transport serves M9.2's routes
// (initialize, tools/list, tools/call over SSE / Streamable HTTP),
// and a disabled transport answers 503 on those routes (M9.2
// enforces that from this same state). Unknown transports and
// malformed bodies are 400, never a silent no-op.
func (s *Server) handleMCPStatus(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.mcpTransportStatus())
	case http.MethodPost:
		var req struct {
			Transport string `json:"transport"`
			Enabled   bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		s.mcpTransportMu.Lock()
		switch req.Transport {
		case "sse":
			s.mcpSSEEnabled = req.Enabled
		case "streamable":
			s.mcpStreamEnabled = req.Enabled
		default:
			s.mcpTransportMu.Unlock()
			writeError(w, http.StatusBadRequest, "unknown transport: want sse or streamable")
			return
		}
		s.mcpTransportMu.Unlock()
		writeJSON(w, http.StatusOK, s.mcpTransportStatus())
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleMCPAudit serves GET /api/mcp/audit (M9.7): a paginated
// view of the MCP tool-call audit log, newest first. Query params:
// limit (default 50, clamped to 1..200) and offset (default 0).
// Entries are returned exactly as stored — input_hash (never raw
// arguments) and output_summary truncated to 200 runes — so this
// feed cannot leak a secret even if an operator pastes it into a
// bug report. With no audit log wired the feed answers
// available:false with an empty list and total 0, never an error.
func (s *Server) handleMCPAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	resp := mcpAuditResponse{
		Available: false,
		Entries:   []mcp.AuditEntry{},
		Limit:     limit,
		Offset:    offset,
		Cap:       mcp.MaxAuditEntries,
	}
	if s.mcpAudit == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Available = true
	resp.Cap = s.mcpAudit.Cap()
	resp.Dropped = s.mcpAudit.Dropped()
	entries, err := s.mcpAudit.List(0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "audit log unreadable")
		return
	}
	resp.Total = len(entries)
	if offset < len(entries) {
		end := offset + limit
		if end > len(entries) {
			end = len(entries)
		}
		resp.Entries = entries[offset:end]
	}
	writeJSON(w, http.StatusOK, resp)
}
