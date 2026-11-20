// GET /api/logs (M8.16, management promotion B).
//
// The recent SERVER log lines — the existing redacted log stream
// (s.logger), not the ledger usage events that GET /dashboard/logs
// serves. Every line the server logger writes already passes through
// the redactWriter (see redact_log.go: redaction is always on, no off
// switch), and the capture writer below redacts each line a second
// time before storing it, so this endpoint serves the REDACTED log
// only. It can never serve an unredacted line: there is no code path
// that stores a line without redacting it.
//
// Wiring: the server logger writes to os.Stderr and keeps no history
// by itself, so history exists only when a capture ring is attached
// with WithAPILogCapture — wired in main.go:3763
// (server.WithAPILogCapture(server.NewAPILogRing(1000))) and
// registered as GET /api/logs in server.go:1347 (guard-table row
// guard.go:167, TierManagement, scope "admin"). Without a ring
// the endpoint answers captured:false with an empty list — the
// honest empty state, never invented lines. The ring is kept in a
// package-level registry keyed by *Server because the Server struct
// (server.go) is owned by the coordinator and this slice owns new
// files only; a server with no ring simply has no entry.
//
// Pagination mirrors GET /dashboard/logs: limit defaults to 50, an
// unparseable limit falls back to the default. Lines are newest
// first. since filters by capture time (RFC3339 or Unix seconds);
// an unparseable since is a 400 — a filter that cannot be honoured
// must not silently unfilter (the same principle as the model
// filters). The ring itself is bounded (default 1000 lines) so a
// long-lived server cannot grow it without bound; the bound is a
// memory bound, documented here, not a data claim.
//
// Tier/scope are wired at registration: TierManagement, scope
// "admin" (guard.go:167, server.go:1347).
package server

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// defaultAPILogLimit mirrors GET /dashboard/logs (server.go
// handleLogs): 50 lines when the caller passes no limit.
const defaultAPILogLimit = 50

// maxAPILogLimit caps one response so a log endpoint cannot be used
// to dump an unbounded body. It is a response-size bound, not a data
// claim; the ring bound below is the matching memory bound.
const maxAPILogLimit = 500

// defaultAPILogRingCap is the ring size when NewAPILogRing is given
// no (or a non-positive) capacity.
const defaultAPILogRingCap = 1000

// apiLogEntry is one captured server log line: the line text (already
// redacted — see the file doc) and when it was captured. Capture time
// is what `since` filters on; the log prefix's own timestamp is part
// of the line text and is never parsed.
type apiLogEntry struct {
	At   time.Time `json:"at"`
	Line string    `json:"line"`
}

// APILogRing is the bounded, most-recent-first store for captured
// server log lines. It is safe for concurrent use.
type APILogRing struct {
	mu      sync.Mutex
	cap     int
	entries []apiLogEntry
}

// NewAPILogRing returns a ring holding at most cap lines (default
// 1000 when cap <= 0). Older lines drop off the front as new ones
// arrive.
func NewAPILogRing(cap int) *APILogRing {
	if cap <= 0 {
		cap = defaultAPILogRingCap
	}
	return &APILogRing{cap: cap}
}

// append adds one (already redacted) line to the ring.
func (r *APILogRing) append(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, apiLogEntry{At: time.Now().UTC(), Line: line})
	if len(r.entries) > r.cap {
		r.entries = append([]apiLogEntry(nil), r.entries[len(r.entries)-r.cap:]...)
	}
}

// lines returns a copy of the ring, oldest first.
func (r *APILogRing) lines() []apiLogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]apiLogEntry(nil), r.entries...)
}

// len returns how many lines the ring currently holds.
func (r *APILogRing) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// apiLogWriter is the io.Writer that feeds a ring: it splits the
// stream into newline-terminated lines (a partial line is buffered
// until its newline arrives, mirroring redactWriter) and redacts
// every complete line with guardrails.RedactLogLine before storing
// it. It never stores an unredacted line.
type apiLogWriter struct {
	mu   sync.Mutex
	ring *APILogRing
	buf  []byte
}

func newAPILogWriter(ring *APILogRing) *apiLogWriter {
	return &apiLogWriter{ring: ring}
}

func (w *apiLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i+1])
		w.buf = append([]byte(nil), w.buf[i+1:]...)
		w.ring.append(guardrails.RedactLogLine(line))
	}
	return len(p), nil
}

// apiLogRingOf returns the capture ring attached to s, or nil.
// The ring lives on the Server (server.go) so it dies with the
// server; a package-level pointer-keyed registry leaked stale
// rings across test servers (see WithAPILogCapture).
func (s *Server) apiLogRingOf() *APILogRing {
	if s == nil {
		return nil
	}
	return s.apiLogRing
}

// WithAPILogCapture attaches ring as this server's log capture: the
// server logger's output is tee'd to the ring (in addition to its
// existing destination), so every line the logger writes from then
// on is captured, redacted. Lines logged before attachment are not
// available — the endpoint says so via captured:false when no ring
// is attached, and via total_captured otherwise. A nil ring is a
// no-op. Attaching twice keeps the first ring (re-teeing would
// capture every line twice).
func WithAPILogCapture(ring *APILogRing) Option {
	return func(s *Server) {
		if ring == nil || s == nil || s.logger == nil {
			return
		}
		if s.apiLogRing != nil {
			return
		}
		s.apiLogRing = ring
		s.logger.SetOutput(io.MultiWriter(s.logger.Writer(), newAPILogWriter(ring)))
	}
}

// apiLogLineJSON is one line in the GET /api/logs payload.
type apiLogLineJSON struct {
	At   time.Time `json:"at"`
	Line string    `json:"line"`
}

// apiLogsResponse is the GET /api/logs payload. Captured is false
// when no ring is attached (no history exists); Lines is then empty
// and TotalCaptured is 0. Redaction is always "always-on": the
// endpoint serves the redacted log only and there is no off switch.
type apiLogsResponse struct {
	Captured      bool             `json:"captured"`
	Redaction     string           `json:"redaction"`
	Source        string           `json:"source"`
	Lines         []apiLogLineJSON `json:"lines"`
	TotalCaptured int              `json:"total_captured"`
	Limit         int              `json:"limit"`
	Since         string           `json:"since,omitempty"`
}

// parseAPILogSince parses the `since` query value: RFC3339 first,
// then Unix seconds. An empty value means "no lower bound".
func parseAPILogSince(raw string) (time.Time, bool, error) {
	if raw == "" {
		return time.Time{}, false, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true, nil
	}
	if secs, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), true, nil
	}
	return time.Time{}, false, errInvalidSince
}

var errInvalidSince = &apiLogError{"invalid since: expected RFC3339 or Unix seconds"}

type apiLogError struct{ msg string }

func (e *apiLogError) Error() string { return e.msg }

// handleAPILogs serves GET /api/logs?limit=50&since=<rfc3339|unix>.
// Lines are newest first. No log lines are ever invented: with no
// capture attached the answer is an empty list with captured:false.
func (s *Server) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	limit := defaultAPILogLimit
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > maxAPILogLimit {
		limit = maxAPILogLimit
	}
	since, hasSince, err := parseAPILogSince(q.Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := apiLogsResponse{
		Captured:  false,
		Redaction: "always-on",
		Source:    "server logger",
		Lines:     []apiLogLineJSON{},
		Limit:     limit,
	}
	if hasSince {
		resp.Since = since.Format(time.RFC3339)
	}
	ring := s.apiLogRingOf()
	if ring == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Captured = true
	resp.TotalCaptured = ring.len()
	entries := ring.lines()
	// Newest first, honouring `since`, then the limit.
	out := []apiLogLineJSON{}
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		if hasSince && entries[i].At.Before(since) {
			continue
		}
		out = append(out, apiLogLineJSON{At: entries[i].At, Line: entries[i].Line})
	}
	resp.Lines = out
	writeJSON(w, http.StatusOK, resp)
}
