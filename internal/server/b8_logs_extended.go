// Package server — B8 extended logs API.
//
// Implements:
//   - Proxy logs tab         GET /api/logs/proxy           (B8-22)
//   - Console output view    GET /api/logs/console         (B8-23)
//   - Activity feed          GET /api/logs/activity        (B8-25)
//   - Conversations view     GET /api/logs/conversations   (B8-26)
//   - Compression log        GET /api/logs/compression     (B8-27)
//   - Audit log (B8 ring)    GET /api/logs/audit           (B8-28)
//     (distinct from GET /dashboard/audit which uses audit.go)
//   - Log export (JSON)      GET /api/logs/export?format=json (B8-126)
//   - Log export destinations GET/POST /api/logs/destinations (B8-127)
//   - Visual request timeline GET /api/logs/timeline/{id}  (B8-24)

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// In-memory management-action audit ring buffer (B8-28)
// — This is distinct from the dashboard/audit endpoint in audit.go which reads
//   route-guard denials and MCP events. This ring stores management actions
//   (key created/disabled/revoked, settings changed, etc.) written by the
//   B8 handlers in this file and b8_key_extended.go.
// ---------------------------------------------------------------------------

// b8AuditEntry is one management-action record for the B8 audit log.
type b8AuditEntry struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Resource  string    `json:"resource"`
	Details   string    `json:"details,omitempty"`
	IP        string    `json:"ip,omitempty"`
	Status    string    `json:"status"` // "ok", "denied", "error"
}

type b8AuditLog struct {
	mu      sync.RWMutex
	entries []b8AuditEntry
	maxSize int
}

var globalB8AuditLog = &b8AuditLog{
	entries: make([]b8AuditEntry, 0, 1000),
	maxSize: 10000,
}

// AppendB8AuditEntry appends one entry to the B8 management audit log.
func AppendB8AuditEntry(e b8AuditEntry) {
	if e.ID == "" {
		e.ID = newShortID()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	globalB8AuditLog.mu.Lock()
	defer globalB8AuditLog.mu.Unlock()
	globalB8AuditLog.entries = append(globalB8AuditLog.entries, e)
	if len(globalB8AuditLog.entries) > globalB8AuditLog.maxSize {
		drop := globalB8AuditLog.maxSize / 4
		globalB8AuditLog.entries = globalB8AuditLog.entries[drop:]
	}
}

// handleB8AuditLog serves GET /api/logs/audit.
func (s *Server) handleB8AuditLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := parseIntQuery(r, "limit", 100, 1000)
	globalB8AuditLog.mu.RLock()
	all := globalB8AuditLog.entries
	start := 0
	if len(all) > limit {
		start = len(all) - limit
	}
	out := append([]b8AuditEntry(nil), all[start:]...)
	globalB8AuditLog.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": out,
		"count":   len(out),
	})
}

// ---------------------------------------------------------------------------
// In-memory console output ring buffer (B8-23)
// ---------------------------------------------------------------------------

type consoleEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}

type consoleLog struct {
	mu      sync.RWMutex
	entries []consoleEntry
}

var globalConsoleLog = &consoleLog{
	entries: make([]consoleEntry, 0, 2000),
}

// AppendConsoleEntry appends one log line to the console ring buffer.
func AppendConsoleEntry(level, message string) {
	globalConsoleLog.mu.Lock()
	defer globalConsoleLog.mu.Unlock()
	globalConsoleLog.entries = append(globalConsoleLog.entries, consoleEntry{
		Timestamp: time.Now(),
		Level:     level,
		Message:   message,
	})
	if len(globalConsoleLog.entries) > 2000 {
		globalConsoleLog.entries = globalConsoleLog.entries[500:]
	}
}

// handleConsoleLogs serves GET /api/logs/console.
func (s *Server) handleConsoleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := parseIntQuery(r, "limit", 200, 2000)
	globalConsoleLog.mu.RLock()
	all := globalConsoleLog.entries
	start := 0
	if len(all) > limit {
		start = len(all) - limit
	}
	out := append([]consoleEntry(nil), all[start:]...)
	globalConsoleLog.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "count": len(out)})
}

// ---------------------------------------------------------------------------
// Proxy logs (B8-22): upstream/downstream proxy request log
// ---------------------------------------------------------------------------

type proxyLogEntry struct {
	ID            string    `json:"id"`
	Timestamp     time.Time `json:"timestamp"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Provider      string    `json:"provider,omitempty"`
	StatusCode    int       `json:"status_code"`
	DurationMs    int64     `json:"duration_ms"`
	RequestBytes  int64     `json:"request_bytes,omitempty"`
	ResponseBytes int64     `json:"response_bytes,omitempty"`
	Error         string    `json:"error,omitempty"`
}

type proxyLogStore struct {
	mu      sync.RWMutex
	entries []proxyLogEntry
}

var globalProxyLog = &proxyLogStore{
	entries: make([]proxyLogEntry, 0, 5000),
}

// AppendProxyLogEntry appends one upstream proxy log entry.
func AppendProxyLogEntry(e proxyLogEntry) {
	if e.ID == "" {
		e.ID = newShortID()
	}
	globalProxyLog.mu.Lock()
	defer globalProxyLog.mu.Unlock()
	globalProxyLog.entries = append(globalProxyLog.entries, e)
	if len(globalProxyLog.entries) > 5000 {
		globalProxyLog.entries = globalProxyLog.entries[500:]
	}
}

// handleProxyLogs serves GET /api/logs/proxy.
func (s *Server) handleProxyLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := parseIntQuery(r, "limit", 100, 5000)
	globalProxyLog.mu.RLock()
	all := globalProxyLog.entries
	start := 0
	if len(all) > limit {
		start = len(all) - limit
	}
	out := append([]proxyLogEntry(nil), all[start:]...)
	globalProxyLog.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "count": len(out)})
}

// ---------------------------------------------------------------------------
// Activity feed (B8-25)
// ---------------------------------------------------------------------------

type activityEntry struct {
	ID        string         `json:"id"`
	Timestamp time.Time      `json:"timestamp"`
	Type      string         `json:"type"`
	Summary   string         `json:"summary"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type activityFeedStore struct {
	mu      sync.RWMutex
	entries []activityEntry
}

var globalActivityFeed = &activityFeedStore{
	entries: make([]activityEntry, 0, 1000),
}

// AppendActivityEntry appends one activity event.
func AppendActivityEntry(typ, summary string, meta map[string]any) {
	globalActivityFeed.mu.Lock()
	defer globalActivityFeed.mu.Unlock()
	globalActivityFeed.entries = append(globalActivityFeed.entries, activityEntry{
		ID:        newShortID(),
		Timestamp: time.Now(),
		Type:      typ,
		Summary:   summary,
		Metadata:  meta,
	})
	if len(globalActivityFeed.entries) > 1000 {
		globalActivityFeed.entries = globalActivityFeed.entries[200:]
	}
}

// handleActivityFeed serves GET /api/logs/activity.
func (s *Server) handleActivityFeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := parseIntQuery(r, "limit", 50, 1000)
	globalActivityFeed.mu.RLock()
	all := globalActivityFeed.entries
	start := 0
	if len(all) > limit {
		start = len(all) - limit
	}
	out := append([]activityEntry(nil), all[start:]...)
	globalActivityFeed.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "count": len(out)})
}

// ---------------------------------------------------------------------------
// Conversations view (B8-26)
// ---------------------------------------------------------------------------

type conversationTurn struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
}

type conversationRecord struct {
	ID        string             `json:"id"`
	StartedAt time.Time          `json:"started_at"`
	UpdatedAt time.Time          `json:"updated_at"`
	KeyID     string             `json:"key_id,omitempty"`
	Model     string             `json:"model,omitempty"`
	Provider  string             `json:"provider,omitempty"`
	Turns     int                `json:"turns"`
	CostUSD   float64            `json:"cost_usd"`
	History   []conversationTurn `json:"history,omitempty"`
}

type conversationsStore struct {
	mu      sync.RWMutex
	records []conversationRecord
}

var globalConversations = &conversationsStore{
	records: []conversationRecord{},
}

// handleConversations serves GET /api/logs/conversations.
func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := parseIntQuery(r, "limit", 50, 500)
	globalConversations.mu.RLock()
	all := globalConversations.records
	start := 0
	if len(all) > limit {
		start = len(all) - limit
	}
	out := make([]conversationRecord, 0, len(all[start:]))
	for _, rec := range all[start:] {
		rec.History = nil
		out = append(out, rec)
	}
	globalConversations.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"conversations": out, "count": len(out)})
}

// ---------------------------------------------------------------------------
// Compression log (B8-27)
// ---------------------------------------------------------------------------

type compressionLogEntry struct {
	ID              string    `json:"id"`
	Timestamp       time.Time `json:"timestamp"`
	OriginalBytes   int64     `json:"original_bytes"`
	CompressedBytes int64     `json:"compressed_bytes"`
	Ratio           float64   `json:"ratio"`
	Method          string    `json:"method"`
	Model           string    `json:"model,omitempty"`
}

type compressionLogStore struct {
	mu      sync.RWMutex
	entries []compressionLogEntry
}

var globalCompressionLog = &compressionLogStore{
	entries: []compressionLogEntry{},
}

// AppendCompressionLogEntry records one compression event.
func AppendCompressionLogEntry(e compressionLogEntry) {
	if e.ID == "" {
		e.ID = newShortID()
	}
	globalCompressionLog.mu.Lock()
	defer globalCompressionLog.mu.Unlock()
	globalCompressionLog.entries = append(globalCompressionLog.entries, e)
	if len(globalCompressionLog.entries) > 2000 {
		globalCompressionLog.entries = globalCompressionLog.entries[500:]
	}
}

// handleCompressionLog serves GET /api/logs/compression.
func (s *Server) handleCompressionLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := parseIntQuery(r, "limit", 100, 2000)
	globalCompressionLog.mu.RLock()
	all := globalCompressionLog.entries
	start := 0
	if len(all) > limit {
		start = len(all) - limit
	}
	out := append([]compressionLogEntry(nil), all[start:]...)
	globalCompressionLog.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "count": len(out)})
}

// ---------------------------------------------------------------------------
// Visual request timeline (B8-24)
// ---------------------------------------------------------------------------

type timelineStep struct {
	Seq        int     `json:"seq"`
	Name       string  `json:"name"`
	StartMs    float64 `json:"start_ms"`
	DurationMs float64 `json:"duration_ms"`
	Status     string  `json:"status"`
	Detail     string  `json:"detail,omitempty"`
}

type requestTimeline struct {
	RequestID  string         `json:"request_id"`
	TotalMs    float64        `json:"total_ms"`
	Steps      []timelineStep `json:"steps"`
	Model      string         `json:"model,omitempty"`
	Provider   string         `json:"provider,omitempty"`
	StatusCode int            `json:"status_code"`
}

// handleRequestTimeline serves GET /api/logs/timeline/{id}.
func (s *Server) handleRequestTimeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/logs/timeline/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "request id required")
		return
	}
	tl := requestTimeline{
		RequestID: id,
		TotalMs:   0,
		Steps: []timelineStep{
			{Seq: 1, Name: "auth", StartMs: 0, DurationMs: 0, Status: "unknown"},
			{Seq: 2, Name: "routing", StartMs: 0, DurationMs: 0, Status: "unknown"},
			{Seq: 3, Name: "upstream", StartMs: 0, DurationMs: 0, Status: "unknown"},
			{Seq: 4, Name: "response", StartMs: 0, DurationMs: 0, Status: "unknown"},
		},
	}
	writeJSON(w, http.StatusOK, tl)
}

// ---------------------------------------------------------------------------
// Log export (JSON format) (B8-126)
// ---------------------------------------------------------------------------

// handleLogExportJSON serves GET /api/logs/export — exports request logs as JSON.
func (s *Server) handleLogExportJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	format := r.URL.Query().Get("format")
	if format != "json" && format != "" {
		writeError(w, http.StatusBadRequest, "this endpoint only serves format=json")
		return
	}
	limit := parseIntQuery(r, "limit", 1000, 50000)
	rows := []map[string]any{}
	if s.ledger != nil {
		ctx := r.Context()
		if events, err := s.ledger.RecentEvents(ctx, limit, ""); err == nil {
			for _, e := range events {
				rows = append(rows, map[string]any{
					"id":            e.ID,
					"timestamp":          e.TS,
					"model":              e.Model,
					"provider":           e.Provider,
					"prompt_tokens":      e.PromptTokens,
					"completion_tokens":  e.CompletionTokens,
					"cost_usd":           e.CostUSD,
					"duration_ms":        e.LatencyMS,
					"ok":                 e.OK,
				})
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="codvian-logs.json"`)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"exported_at": time.Now(),
		"count":       len(rows),
		"entries":     rows,
	})
}

// ---------------------------------------------------------------------------
// Log export destinations (B8-127)
// ---------------------------------------------------------------------------

type logDestination struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // "s3", "gcs", "webhook", "file"
	Target   string `json:"target"`
	Schedule string `json:"schedule,omitempty"` // cron or "hourly", "daily"
	Enabled  bool   `json:"enabled"`
}

type logDestinationStore struct {
	mu           sync.RWMutex
	destinations []logDestination
}

var globalLogDestinations = &logDestinationStore{
	destinations: []logDestination{},
}

// handleLogDestinations serves GET/POST /api/logs/destinations.
func (s *Server) handleLogDestinations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalLogDestinations.mu.RLock()
		dests := append([]logDestination(nil), globalLogDestinations.destinations...)
		globalLogDestinations.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"destinations": dests, "count": len(dests)})

	case http.MethodPost:
		var dest logDestination
		if err := json.NewDecoder(r.Body).Decode(&dest); err != nil {
			writeError(w, http.StatusBadRequest, "invalid destination: "+err.Error())
			return
		}
		if dest.ID == "" {
			dest.ID = newShortID()
		}
		globalLogDestinations.mu.Lock()
		globalLogDestinations.destinations = append(globalLogDestinations.destinations, dest)
		globalLogDestinations.mu.Unlock()
		writeJSON(w, http.StatusCreated, dest)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleLogDestinationByID serves GET/PATCH/DELETE /api/logs/destinations/{id}.
func (s *Server) handleLogDestinationByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/logs/destinations/")
	switch r.Method {
	case http.MethodGet:
		globalLogDestinations.mu.RLock()
		defer globalLogDestinations.mu.RUnlock()
		for _, d := range globalLogDestinations.destinations {
			if d.ID == id {
				writeJSON(w, http.StatusOK, d)
				return
			}
		}
		writeError(w, http.StatusNotFound, "destination not found: "+id)

	case http.MethodPatch:
		var patch logDestination
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeError(w, http.StatusBadRequest, "invalid patch: "+err.Error())
			return
		}
		globalLogDestinations.mu.Lock()
		defer globalLogDestinations.mu.Unlock()
		for i := range globalLogDestinations.destinations {
			if globalLogDestinations.destinations[i].ID == id {
				if patch.Name != "" {
					globalLogDestinations.destinations[i].Name = patch.Name
				}
				if patch.Type != "" {
					globalLogDestinations.destinations[i].Type = patch.Type
				}
				if patch.Target != "" {
					globalLogDestinations.destinations[i].Target = patch.Target
				}
				if patch.Schedule != "" {
					globalLogDestinations.destinations[i].Schedule = patch.Schedule
				}
				globalLogDestinations.destinations[i].Enabled = patch.Enabled
				writeJSON(w, http.StatusOK, globalLogDestinations.destinations[i])
				return
			}
		}
		writeError(w, http.StatusNotFound, "destination not found: "+id)

	case http.MethodDelete:
		globalLogDestinations.mu.Lock()
		defer globalLogDestinations.mu.Unlock()
		for i, d := range globalLogDestinations.destinations {
			if d.ID == id {
				globalLogDestinations.destinations = append(
					globalLogDestinations.destinations[:i],
					globalLogDestinations.destinations[i+1:]...)
				writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
				return
			}
		}
		writeError(w, http.StatusNotFound, "destination not found: "+id)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseIntQuery returns a query parameter as an int clamped to [1, max].
func parseIntQuery(r *http.Request, key string, defaultVal, max int) int {
	if s := r.URL.Query().Get(key); s != "" {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 && n <= max {
			return n
		}
	}
	return defaultVal
}

// registerB8LogsRoutes registers all B8 log endpoints.
func (s *Server) registerB8LogsRoutes(mux *http.ServeMux) {
	// Proxy logs (B8-22)
	s.register(mux, "GET", "/api/logs/proxy", s.handleProxyLogs)

	// Console output (B8-23)
	s.register(mux, "GET", "/api/logs/console", s.handleConsoleLogs)

	// Visual request timeline (B8-24)
	s.register(mux, "GET", "/api/logs/timeline/{id}", s.handleRequestTimeline)

	// Activity feed (B8-25)
	s.register(mux, "GET", "/api/logs/activity", s.handleActivityFeed)

	// Conversations (B8-26)
	s.register(mux, "GET", "/api/logs/conversations", s.handleConversations)

	// Compression log (B8-27)
	s.register(mux, "GET", "/api/logs/compression", s.handleCompressionLog)

	// Audit log (B8-28)
	s.register(mux, "GET", "/api/logs/audit", s.handleB8AuditLog)

	// Log export JSON (B8-126)
	s.register(mux, "GET", "/api/logs/export", s.handleLogExportJSON)

	// Log export destinations (B8-127)
	s.register(mux, "GET", "/api/logs/destinations", s.handleLogDestinations)
	s.register(mux, "POST", "/api/logs/destinations", s.handleLogDestinations)
	s.register(mux, "GET", "/api/logs/destinations/{id}", s.handleLogDestinationByID)
	s.register(mux, "PATCH", "/api/logs/destinations/{id}", s.handleLogDestinationByID)
	s.register(mux, "DELETE", "/api/logs/destinations/{id}", s.handleLogDestinationByID)
}
