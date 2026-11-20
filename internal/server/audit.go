// Audit feed (M10 B4): GET /dashboard/audit.
//
// The Audit page's feed: guard denials, key lifecycle events,
// management POSTs, and the MCP tool-call audit join. Counts +
// redacted identifiers ONLY — no prompts, no keys, no message
// content, ever.
//
// Sources (all verified in the tree at build time; nothing here is
// invented — a category with no real source stays present, zeroed,
// and says so in its note):
//
//   - Guard denials — REAL, counters only. s.localOnlyDenials
//     (internal/server/guard.go:426, incremented by
//     requireLocalOnly at guard.go:423; surfaced by handleGuard
//     at guard.go:610, Load at :636)
//     plus the guardrail stats from s.guardrails.Stats()
//     (SecurityRefusals labels + Blocked count,
//     internal/guardrails/guardrails.go:1065, :1082;
//     Stats() at :1398). There is NO
//     per-event denial log in the tree — denials are also written
//     as unstructured logger lines only — so this category
//     carries live counts and an always-empty entries list, never
//     a reconstructed event.
//   - Key lifecycle — NO STRUCTURED SOURCE YET, honestly empty.
//     Disable / revoke / rotate write only redacted logger lines
//     (internal/server/keystatus.go:744-746 disable, :825 revoke,
//     :959 rotate: "key disable: key <id> disabled" and friends).
//     No store exists to query, and this feed will NOT reconstruct
//     events by parsing log text. Available is false, total 0,
//     entries [] until a structured key-event store lands.
//   - Management POSTs — NO STRUCTURED SOURCE YET, honestly
//     empty. Dashboard/admin POSTs are served and logged
//     per-handler but never recorded in a queryable store, so
//     there is nothing to list. Available is false, total 0,
//     entries [] until a management-action log lands.
//   - MCP audit join — REAL. s.mcpAudit (*mcp.AuditLog,
//     internal/server/server.go:639-645), wired by WithMCPAuditLog
//     (server.go:980) from serve mode
//     (cmd/codvian-router/main.go:3731, :3927, same JSONL log the
//     stdio MCP server records into). Entries are projected, not
//     passed through: OutputSummary is DROPPED (it is message
//     content, up to 200 runes of a tool result), and every
//     remaining string passes through guardrails.RedactLogLine,
//     so a secret-shaped value smuggled into a stored reason or
//     caller id can never leave through this feed. InputHash is
//     already a SHA-256 hex, never raw arguments.
//
// Auth is NOT enforced here: like internal/server/a2a_status.go,
// the handler assumes the route registration enforces it.
// auditGuardRoute (below) is the exact guard-table row the
// coordinator must land: Tier MANAGEMENT, admin scope. The
// registration line is:
//
//	s.register(mux, "GET", "/dashboard/audit", s.handleDashboardAudit)
//
// Pagination follows the GET /api/mcp/audit convention
// (internal/server/mcp.go handleMCPAudit): limit default 50,
// clamped to 1..200, offset default 0; the MCP join is newest
// first and the limit/offset apply to that join only — the
// denial counters are process-lifetime totals and are never
// paginated.
package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/mcp"
)

// auditGuardRoute is the exact guard-table row for GET
// /dashboard/audit (M10 B4). The coordinator copies this row into
// guardRouteTable in internal/server/guard.go and registers the
// route in internal/server/server.go; the handler itself enforces
// no auth, exactly like the A2A status handler. Tier MANAGEMENT
// with admin scope: the feed is operator-only — a gateway key
// without admin must never read the audit trail.
func auditGuardRoute() GuardRoute {
	return GuardRoute{
		Method:      "GET",
		Path:        "/dashboard/audit",
		Tier:        TierManagement,
		Description: "Audit feed: guard-denial counts (live counters), key-lifecycle events, management POSTs, and the MCP tool-call audit join — counts + redacted identifiers only, no prompts, no keys, no message content. Management auth (admin scope) required.",
		Scopes:      []string{"admin"},
	}
}

// auditEntry is one audit event in the unified shape used by the
// categories that have per-event rows. Today every such category
// is honestly empty (see the file doc), so this type only pins
// the shape the feed will use once a structured event store
// lands for key lifecycle / management POSTs.
type auditEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	ActorID   string    `json:"actor_id,omitempty"`
	TargetID  string    `json:"target_id,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// auditEmptyCategory is a category with no structured event
// source yet: present, zeroed, and honest about why. Available is
// false — the feed does not claim to have data it cannot read.
type auditEmptyCategory struct {
	Available bool         `json:"available"`
	Total     int          `json:"total"`
	Entries   []auditEntry `json:"entries"`
	Note      string       `json:"note"`
}

// auditDenialsCategory is the guard-denials category: live
// counters, no per-event rows (no denial event log exists).
// Total counts route-guard denials only (loopback refusals +
// security refusals by label); GuardrailBlocked is reported
// separately because it measures blocked prompts, not
// route-guard denials, and must never be folded into the total
// silently.
type auditDenialsCategory struct {
	Available bool `json:"available"`
	// Total is LocalOnlyDenials + SecurityRefusalsTotal.
	Total                 uint64            `json:"total"`
	LocalOnlyDenials      uint64            `json:"local_only_denials"`
	GuardrailBlocked      uint64            `json:"guardrail_blocked"`
	SecurityRefusals      map[string]uint64 `json:"security_refusals,omitempty"`
	SecurityRefusalsTotal uint64            `json:"security_refusals_total"`
	Entries               []auditEntry      `json:"entries"`
	Note                  string            `json:"note"`
}

// auditMCPEntry is one MCP tool-call audit row as this feed
// reports it: the stored mcp.AuditEntry projected down to
// identifiers only. OutputSummary is deliberately ABSENT —
// it is message content (a truncated tool result) and this
// feed never carries message content. Every string field is
// passed through guardrails.RedactLogLine at projection time.
type auditMCPEntry struct {
	Timestamp  time.Time         `json:"timestamp"`
	Tool       string            `json:"tool"`
	CallerKind mcp.ClientKind    `json:"caller_kind"`
	CallerID   string            `json:"caller_id"`
	Decision   mcp.AuditDecision `json:"decision"`
	Reason     string            `json:"reason,omitempty"`
	InputHash  string            `json:"input_hash"`
	ErrorCode  int               `json:"error_code,omitempty"`
}

// auditMCPCategory is the MCP audit join: the projected entries,
// newest first, paginated by limit/offset. Cap/Dropped mirror
// GET /api/mcp/audit so a rotation is visible, not silent.
type auditMCPCategory struct {
	Available bool            `json:"available"`
	Total     int             `json:"total"`
	Entries   []auditMCPEntry `json:"entries"`
	Cap       int             `json:"cap"`
	Dropped   int             `json:"dropped"`
	Note      string          `json:"note"`
}

// auditResponse is the GET /dashboard/audit payload.
type auditResponse struct {
	GuardDenials    auditDenialsCategory `json:"guard_denials"`
	KeyLifecycle    auditEmptyCategory   `json:"key_lifecycle"`
	ManagementPosts auditEmptyCategory   `json:"management_posts"`
	MCPAudit        auditMCPCategory     `json:"mcp_audit"`
	Limit           int                  `json:"limit"`
	Offset          int                  `json:"offset"`
}

// handleDashboardAudit serves GET /dashboard/audit (M10 B4).
func (s *Server) handleDashboardAudit(w http.ResponseWriter, r *http.Request) {
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

	// Guard denials: live counters from the guard + guardrails.
	// No per-event denial log exists, so entries stays empty.
	stats := s.guardrails.Stats()
	var securityTotal uint64
	for _, n := range stats.SecurityRefusals {
		securityTotal += n
	}
	localOnly := s.localOnlyDenials.Load()
	denials := auditDenialsCategory{
		Available:             true,
		Total:                 localOnly + securityTotal,
		LocalOnlyDenials:      localOnly,
		GuardrailBlocked:      stats.Blocked,
		SecurityRefusals:      stats.SecurityRefusals,
		SecurityRefusalsTotal: securityTotal,
		Entries:               []auditEntry{},
		Note:                  "Live counters only: no per-event guard-denial log exists in this process (denials are also written as unstructured logger lines), so individual denial rows cannot be listed.",
	}

	// Key lifecycle + management POSTs: no structured source
	// exists yet — present, zeroed, honest (see file doc).
	keyLifecycle := auditEmptyCategory{
		Available: false,
		Total:     0,
		Entries:   []auditEntry{},
		Note:      "No structured key-lifecycle store exists yet: disable/revoke/rotate are written only as redacted logger lines (internal/server/keystatus.go), not into a queryable store. This category stays empty until a structured key-event store lands; events are never reconstructed by parsing log text.",
	}
	managementPosts := auditEmptyCategory{
		Available: false,
		Total:     0,
		Entries:   []auditEntry{},
		Note:      "No central record of management POSTs exists yet: dashboard/admin POSTs are served and logged per-handler but never recorded in a queryable store. This category stays empty until a management-action log lands.",
	}

	// MCP audit join: projected, redacted, newest first.
	mcpCat := auditMCPCategory{
		Available: false,
		Entries:   []auditMCPEntry{},
		Cap:       mcp.MaxAuditEntries,
		Note:      "No MCP audit log is wired into this server (WithMCPAuditLog not set); serve mode wires the stdio MCP server's log.",
	}
	if s.mcpAudit != nil {
		mcpCat.Available = true
		mcpCat.Cap = s.mcpAudit.Cap()
		mcpCat.Dropped = s.mcpAudit.Dropped()
		mcpCat.Note = "MCP tool-call audit joined from the wired log; entries are projected to identifiers only (output summaries dropped, all strings redacted)."
		entries, err := s.mcpAudit.List(0)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "audit log unreadable")
			return
		}
		mcpCat.Total = len(entries)
		if offset < len(entries) {
			end := offset + limit
			if end > len(entries) {
				end = len(entries)
			}
			for _, e := range entries[offset:end] {
				mcpCat.Entries = append(mcpCat.Entries, auditMCPEntry{
					Timestamp:  e.Timestamp,
					Tool:       guardrails.RedactLogLine(e.Tool),
					CallerKind: e.CallerKind,
					CallerID:   guardrails.RedactLogLine(e.CallerID),
					Decision:   e.Decision,
					Reason:     guardrails.RedactLogLine(e.Reason),
					InputHash:  e.InputHash,
					ErrorCode:  e.ErrorCode,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, auditResponse{
		GuardDenials:    denials,
		KeyLifecycle:    keyLifecycle,
		ManagementPosts: managementPosts,
		MCPAudit:        mcpCat,
		Limit:           limit,
		Offset:          offset,
	})
}
