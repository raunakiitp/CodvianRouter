// M9.4 — cache + DB health + memory tools.
//
// Shape studied from the reference router for reference only (never copied):
// cache_stats [read:cache], cache_flush
// [write:cache], db_health_check [read:health,
// write:resilience], memory_save/search/update/delete over their
// Qdrant vector store. Our scopes are our own M9.1 set: the cache
// tools take mcp:manage:cache, the DB tools mcp:manage:ops, the
// memory tools mcp:manage:memory — and the destructive two
// (cache_flush, memory_clear) take mcp:destructive IN ADDITION
// (AND semantics: a caller with only the family scope is denied,
// and a caller with only the destructive scope is denied).
//
// Tenant rule, read this before touching cache_flush: the tenant
// is NEVER caller-supplied. The handler flushes the entries of
// the identity the dispatch path already authenticated
// (s.identity — LocalOperator on stdio, the gateway-key id on a
// remote transport via HandleWithIdentity). There is no "tenant"
// argument: accepting one would let any key flush any other
// tenant's entries, which is exactly the M6 tenant-isolation
// property (P0-2) these tools must not break.
//
// Honest-numbers rule: every number comes from the Source at call
// time. A layer the current process cannot see is reported
// Present=false, never zero-filled — the stdio server has no live
// in-memory cache (that lives in the serve process), so its
// CacheStats reports only the persistent tier it can read from
// disk; see the production Source in
// cmd/codvian-router/doctor_configure_mcp.go.
package mcp

import (
	"errors"
	"log"
	"strings"
	"time"
)

// CacheLayerStats is one cache layer's live counters. Present is
// false when the layer is not wired in this process — Entries and
// Bytes are meaningless then and must not be read as zero.
type CacheLayerStats struct {
	Present bool `json:"present"`
	Entries int  `json:"entries"`
	Bytes   int  `json:"bytes"`
}

// CacheStats is the cache_stats payload: the M6 3-layer cache
// (exact / semantic / persistent), one block per layer.
type CacheStats struct {
	Exact      CacheLayerStats `json:"exact"`
	Semantic   CacheLayerStats `json:"semantic"`
	Persistent CacheLayerStats `json:"persistent"`
	Note       string          `json:"note,omitempty"`
}

// DBFileHealth is one data-dir database file as db_health /
// db_stats report it. Rows counts only tables whose schema is
// known (see the production Source); a file that cannot be opened
// carries Error and Healthy=false on the whole report. SizeBytes
// and ModifiedAt are the live file facts — "last write" is the
// file modification time, never an invented timestamp.
type DBFileHealth struct {
	Name       string           `json:"name"`
	Exists     bool             `json:"exists"`
	SizeBytes  int64            `json:"size_bytes,omitempty"`
	ModifiedAt time.Time        `json:"modified_at,omitempty"`
	Rows       map[string]int64 `json:"rows,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// DBHealth is the db_health payload: Healthy is true only when
// every database file that exists opens cleanly and its known
// tables count without error. A missing optional file is not
// unhealthy — it is reported Exists=false.
type DBHealth struct {
	Healthy bool           `json:"healthy"`
	Files   []DBFileHealth `json:"files"`
	Note    string         `json:"note,omitempty"`
}

// DBStats is the db_stats payload: the same per-file facts as
// db_health, without the verdict — sizes, row counts, last write.
type DBStats struct {
	Files []DBFileHealth `json:"files"`
	Note  string         `json:"note,omitempty"`
}

func stringArgSchema(key, desc string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			key: map[string]any{"type": "string", "description": desc},
		},
		"required": []string{key},
	}
}

func stringArg(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// callerID is the tenant/namespace for the ops tools: the
// identity the dispatch path authenticated. The zero value
// cannot occur (Anonymous carries id "anonymous"), but an empty
// id still fails closed to "anonymous" rather than to "".
func (s *Server) callerID() string {
	if s.identity.ID != "" {
		return s.identity.ID
	}
	return "anonymous"
}

func init() {
	// Scopes FIRST, then RegisterTool — RegisterTool panics
	// otherwise (see internal/mcp/mcp.go).
	RegisterToolScopes("cache_stats", ScopeManageCache)
	RegisterTool(Tool{
		Name:        "cache_stats",
		Description: "Live counts for the router's 3-layer cache (exact / semantic / persistent): entries and bytes per layer. A layer the current process cannot see is reported present=false, never zero-filled.",
		InputSchema: emptySchema(),
	}, toolCacheStats)

	RegisterToolScopes("cache_flush", ScopeManageCache, ScopeDestructive)
	RegisterTool(Tool{
		Name:        "cache_flush",
		Description: "DESTRUCTIVE: evict the caller's own cached entries only. The tenant is the authenticated caller, never an argument — other tenants' entries are untouched. Requires mcp:manage:cache AND mcp:destructive.",
		InputSchema: emptySchema(),
	}, toolCacheFlush)

	RegisterToolScopes("db_health", ScopeManageOps)
	RegisterTool(Tool{
		Name:        "db_health",
		Description: "Health of the router's data-dir databases (ledger, feedback, persistent cache): whether each file exists, opens, and its known tables count cleanly.",
		InputSchema: emptySchema(),
	}, toolDBHealth)

	RegisterToolScopes("db_stats", ScopeManageOps)
	RegisterTool(Tool{
		Name:        "db_stats",
		Description: "Live statistics for the router's data-dir databases: file sizes, row counts per known table, and last-write (file modification) time.",
		InputSchema: emptySchema(),
	}, toolDBStats)

	RegisterToolScopes("memory_add", ScopeManageMemory)
	RegisterTool(Tool{
		Name:        "memory_add",
		Description: "Store a free-text memory under the caller's own namespace in the local file-backed memory store. No embeddings, no external service — the text is stored as written.",
		InputSchema: stringArgSchema("text", "Free-text memory to store (non-empty, at most 10000 characters)."),
	}, toolMemoryAdd)

	RegisterToolScopes("memory_search", ScopeManageMemory)
	RegisterTool(Tool{
		Name:        "memory_search",
		Description: "Search the caller's own stored memories by case-insensitive substring match. Results are ranked by occurrence count, then recency — the returned matches field is the raw occurrence count, not a relevance score.",
		InputSchema: stringArgSchema("query", "Substring to search the caller's memories for (non-empty)."),
	}, toolMemorySearch)

	RegisterToolScopes("memory_clear", ScopeManageMemory, ScopeDestructive)
	RegisterTool(Tool{
		Name:        "memory_clear",
		Description: "DESTRUCTIVE: delete all of the caller's own stored memories. Other callers' memories are untouched. Requires mcp:manage:memory AND mcp:destructive.",
		InputSchema: emptySchema(),
	}, toolMemoryClear)
}

// opsStoreErr maps a Source error to the message a remote MCP
// caller sees (H4 — path leaks). An invalid input is returned
// as-is: it names only the caller's own arguments, never a
// filesystem path. Anything else is a backing-store failure,
// and it is NEVER returned verbatim: os errors embed the full
// local path (*PathError from os.Stat, %q in an open error),
// which would leak the operator's data-dir layout to any
// authenticated remote key in the -32602 reply. The full error
// is logged server-side (stderr, operator only — stdout is the
// MCP protocol) and the caller gets the fixed path-free
// message. Do NOT "simplify" this to handlerErr (tools_routing):
// that helper prefixes the raw error with "unavailable: " and
// would re-introduce the leak.
func opsStoreErr(tool, generic string, err error) string {
	if errors.Is(err, ErrInvalidInput) {
		return err.Error()
	}
	log.Printf("mcp: %s store error: %v", tool, err)
	return generic
}

func toolCacheStats(s *Server, args map[string]any) (map[string]any, string) {
	return toolResult(s.src.CacheStats()), ""
}

func toolCacheFlush(s *Server, args map[string]any) (map[string]any, string) {
	tenant := s.callerID()
	n, err := s.src.CacheFlush(tenant)
	if err != nil {
		return nil, opsStoreErr("cache_flush", "unavailable: persistent cache unavailable", err)
	}
	return toolResult(map[string]any{
		"flushed": n,
		"tenant":  tenant,
		"scope":   "caller-only",
		"note":    "Only entries stored under the caller's own tenant were evicted; other tenants' entries are untouched.",
	}), ""
}

func toolDBHealth(s *Server, args map[string]any) (map[string]any, string) {
	return toolResult(s.src.DBHealth()), ""
}

func toolDBStats(s *Server, args map[string]any) (map[string]any, string) {
	return toolResult(s.src.DBStats()), ""
}

func toolMemoryAdd(s *Server, args map[string]any) (map[string]any, string) {
	text, ok := stringArg(args, "text")
	if !ok || strings.TrimSpace(text) == "" {
		return nil, `memory_add requires a non-empty string argument "text"`
	}
	e, err := s.src.MemoryAdd(s.callerID(), text)
	if err != nil {
		return nil, opsStoreErr("memory_add", "unavailable: memory store unavailable", err)
	}
	return toolResult(map[string]any{
		"id":         e.ID,
		"text":       e.Text,
		"created_at": e.CreatedAt.Format(time.RFC3339),
	}), ""
}

func toolMemorySearch(s *Server, args map[string]any) (map[string]any, string) {
	query, ok := stringArg(args, "query")
	if !ok || strings.TrimSpace(query) == "" {
		return nil, `memory_search requires a non-empty string argument "query"`
	}
	entries, err := s.src.MemorySearch(s.callerID(), query)
	if err != nil {
		return nil, opsStoreErr("memory_search", "unavailable: memory store unavailable", err)
	}
	if entries == nil {
		entries = []MemoryEntry{}
	}
	return toolResult(map[string]any{"entries": entries, "count": len(entries)}), ""
}

func toolMemoryClear(s *Server, args map[string]any) (map[string]any, string) {
	tenant := s.callerID()
	n, err := s.src.MemoryClear(tenant)
	if err != nil {
		return nil, opsStoreErr("memory_clear", "unavailable: memory store unavailable", err)
	}
	return toolResult(map[string]any{
		"cleared": n,
		"caller":  tenant,
		"scope":   "caller-only",
		"note":    "Only the caller's own memories were deleted; other callers' memories are untouched.",
	}), ""
}
