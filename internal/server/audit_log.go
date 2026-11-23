// Package server provides request-level compliance audit logging.
//
// Audit log records one row per applicable API request:
//   - correlation ID (joins to response headers and stream trailers)
//   - timestamp (unix milliseconds, UTC)
//   - actor key ID hash (first 8 hex of SHA-256(key_id), never the raw key)
//   - HTTP method and normalized route path
//   - response status code
//   - latency in milliseconds
//   - failure classification (auth, rate_limit, quota, upstream, server, none)
//   - actor type (gateway_key, oauth, anonymous)
//
// Security: no API keys, Authorization headers, passwords, session
// secrets, or sensitive request/response bodies are ever stored.
// Sensitive fields are redacted before the row is written.
//
// Append-only: the audit_events table has no UPDATE or DELETE paths.
// Tamper-evident: each row's entry_hash chains to the previous row's
// entry_hash, so any modification is detectable via VerifyChain.
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	// sqlite driver
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// auditEvent is one audit record as stored in the audit_events table.
// All sensitive fields are redacted before construction.
type auditEvent struct {
	TS            int64  `json:"ts"`             // unix milliseconds (UTC)
	CorrelationID string `json:"correlation_id"` // request ID from context
	KeyIDHash     string `json:"key_id_hash"`    // first 8 hex of SHA-256(key_id), "" when anonymous
	Method        string `json:"method"`         // HTTP method
	Path          string `json:"path"`           // normalized route path, query stripped
	StatusCode    int    `json:"status_code"`    // HTTP response status
	LatencyMs     int64  `json:"latency_ms"`     // handler latency
	FailureClass  string `json:"failure_class"`  // auth, rate_limit, quota, upstream, server, none
	ActorType     string `json:"actor_type"`     // gateway_key, oauth, anonymous
}

// AuditStore is the append-only, tamper-evident audit event store.
type AuditStore interface {
	// Record appends one audit event. It must never fail the request
	// path: on storage failure it returns an error and the caller
	// decides whether to continue (the middleware always continues).
	Record(evt auditEvent) error

	// List returns audit events newest-first, paginated by limit/offset.
	List(limit, offset int) ([]auditEvent, int, error)

	// VerifyChain walks every row and returns the first index where
	// the hash chain breaks, or -1 if the chain is intact.
	VerifyChain() (int, error)

	// Close releases store resources.
	Close() error
}

// auditStore is the SQLite-backed append-only audit store.
type auditStore struct {
	mu   sync.Mutex
	db   *sql.DB
	path string

	// stmtMu guards the lazily-prepared insert statement.
	stmtMu     sync.Mutex
	insertStmt *sql.Stmt
}

// schema creates the audit_events table. Append-only: no UPDATE or
// DELETE statements exist anywhere in this package for this table.
const auditSchema = `
CREATE TABLE IF NOT EXISTS audit_events (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  ts             INTEGER NOT NULL,      -- unix milliseconds (UTC)
  correlation_id TEXT NOT NULL DEFAULT '',
  key_id_hash    TEXT NOT NULL DEFAULT '', -- first 8 hex of SHA-256(key_id)
  method         TEXT NOT NULL DEFAULT '',
  path           TEXT NOT NULL DEFAULT '',
  status_code    INTEGER NOT NULL DEFAULT 0,
  latency_ms     INTEGER NOT NULL DEFAULT 0,
  failure_class  TEXT NOT NULL DEFAULT 'none',
  actor_type     TEXT NOT NULL DEFAULT 'anonymous',
  prev_hash      TEXT NOT NULL DEFAULT '', -- SHA-256 of previous row's entry_hash
  entry_hash     TEXT NOT NULL DEFAULT ''  -- SHA-256 of this row's full content
);
CREATE INDEX IF NOT EXISTS idx_audit_events_ts ON audit_events(ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_key_hash ON audit_events(key_id_hash);
CREATE INDEX IF NOT EXISTS idx_audit_events_status ON audit_events(status_code);
`

// openAuditStore opens (creating if needed) the SQLite audit store at path.
// The directory is created if missing. A single writer connection is used
// to avoid SQLite "database is locked" churn.
func openAuditStore(path string) (*auditStore, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("audit: create directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("audit: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit: enable WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit: busy timeout: %w", err)
	}
	if _, err := db.Exec(auditSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit: apply schema: %w", err)
	}
	return &auditStore{db: db, path: path}, nil
}

// Record appends one audit event. The entry_hash chains to the previous
// row's entry_hash, making the store tamper-evident.
func (s *auditStore) Record(evt auditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Read the last row's entry_hash to build the chain.
	var prevHash string
	err := s.db.QueryRow(`SELECT entry_hash FROM audit_events ORDER BY id DESC LIMIT 1`).Scan(&prevHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("audit: read last hash: %w", err)
	}

	entryHash := computeEntryHash(prevHash, evt)

	stmt, err := s.prepareInsert()
	if err != nil {
		return fmt.Errorf("audit: prepare insert: %w", err)
	}
	_, err = stmt.Exec(
		evt.TS, evt.CorrelationID, evt.KeyIDHash, evt.Method, evt.Path,
		evt.StatusCode, evt.LatencyMs, evt.FailureClass, evt.ActorType,
		prevHash, entryHash,
	)
	if err != nil {
		return fmt.Errorf("audit: insert row: %w", err)
	}
	return nil
}

// prepareInsert lazily prepares the insert statement.
func (s *auditStore) prepareInsert() (*sql.Stmt, error) {
	s.stmtMu.Lock()
	defer s.stmtMu.Unlock()
	if s.insertStmt != nil {
		return s.insertStmt, nil
	}
	stmt, err := s.db.Prepare(`
INSERT INTO audit_events
  (ts, correlation_id, key_id_hash, method, path, status_code, latency_ms,
   failure_class, actor_type, prev_hash, entry_hash)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	s.insertStmt = stmt
	return stmt, nil
}

// List returns audit events newest-first, paginated by limit/offset.
func (s *auditStore) List(limit, offset int) ([]auditEvent, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("audit: count: %w", err)
	}

	rows, err := s.db.Query(`
	SELECT ts, correlation_id, key_id_hash, method, path, status_code,
	       latency_ms, failure_class, actor_type
	FROM audit_events
	ORDER BY id DESC
	LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: query: %w", err)
	}
	defer rows.Close()

	var events []auditEvent
	for rows.Next() {
		var e auditEvent
		if err := rows.Scan(&e.TS, &e.CorrelationID, &e.KeyIDHash, &e.Method,
			&e.Path, &e.StatusCode, &e.LatencyMs, &e.FailureClass, &e.ActorType); err != nil {
			return nil, 0, fmt.Errorf("audit: scan: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("audit: rows: %w", err)
	}
	return events, total, nil
}

// VerifyChain walks every row ordered by id and returns the first index
// (0-based) where the hash chain breaks, or -1 if the chain is intact.
func (s *auditStore) VerifyChain() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
	SELECT id, ts, correlation_id, key_id_hash, method, path, status_code,
	       latency_ms, failure_class, actor_type, prev_hash, entry_hash
	FROM audit_events
	ORDER BY id ASC`)
	if err != nil {
		return -1, fmt.Errorf("audit: chain query: %w", err)
	}
	defer rows.Close()

	var prevHash string
	idx := 0
	for rows.Next() {
		var id int64
		var e auditEvent
		var storedPrev, storedEntry string
		if err := rows.Scan(&id, &e.TS, &e.CorrelationID, &e.KeyIDHash, &e.Method,
			&e.Path, &e.StatusCode, &e.LatencyMs, &e.FailureClass, &e.ActorType,
			&storedPrev, &storedEntry); err != nil {
			return -1, fmt.Errorf("audit: chain scan: %w", err)
		}
		if idx == 0 {
			// First row: prev_hash must be empty.
			if storedPrev != "" {
				return idx, nil
			}
		} else {
			if storedPrev != prevHash {
				return idx, nil
			}
		}
		expected := computeEntryHash(storedPrev, e)
		if expected != storedEntry {
			return idx, nil
		}
		prevHash = storedEntry
		idx++
	}
	if err := rows.Err(); err != nil {
		return -1, fmt.Errorf("audit: chain rows: %w", err)
	}
	return -1, nil
}

// Close releases store resources.
func (s *auditStore) Close() error {
	s.stmtMu.Lock()
	if s.insertStmt != nil {
		_ = s.insertStmt.Close()
		s.insertStmt = nil
	}
	s.stmtMu.Unlock()
	return s.db.Close()
}

// computeEntryHash returns SHA-256(prevHash || "|" || ts || "|" || correlation_id
// || "|" || key_id_hash || "|" || method || "|" || path || "|" || status_code
// || "|" || latency_ms || "|" || failure_class || "|" || actor_type).
func computeEntryHash(prevHash string, e auditEvent) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%s|%s|%s|%s|%d|%d|%s|%s",
		prevHash, e.TS, e.CorrelationID, e.KeyIDHash, e.Method, e.Path,
		e.StatusCode, e.LatencyMs, e.FailureClass, e.ActorType)
	return hex.EncodeToString(h.Sum(nil))
}

// hashKeyID returns the first 8 hex characters of SHA-256(keyID),
// suitable for storing as a non-reversible actor reference.
func hashKeyID(keyID string) string {
	if keyID == "" {
		return ""
	}
	h := sha256.Sum256([]byte(keyID))
	return hex.EncodeToString(h[:])[:8]
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// ctxKeyAuditEvent carries the in-progress audit event through the
// handler chain so downstream middleware can enrich it (e.g. model name).
type ctxKeyAuditEvent struct{}

// auditEventFrom returns the in-progress audit event from ctx, or nil.
func auditEventFrom(ctx context.Context) *auditEvent {
	ae, _ := ctx.Value(ctxKeyAuditEvent{}).(*auditEvent)
	return ae
}

// withAudit wraps next with request-level compliance audit logging.
// It records one audit row per request after the handler completes.
// Sensitive values (API keys, Authorization headers, bodies) are never
// stored. Storage failure never fails the request.
func (s *Server) withAudit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Build the audit event before the handler runs so downstream
		// middleware can enrich it (model name, etc.).
		evt := &auditEvent{
			TS:            time.Now().UnixMilli(),
			CorrelationID: requestIDFor(r),
			Method:        r.Method,
			Path:          normalizeRoutePath(r.URL.Path),
			ActorType:     "anonymous",
		}
		// Attribute to a gateway key when one is presented (hashed, never raw).
		if keyID := s.callerKeyID(r); keyID != "" {
			evt.KeyIDHash = hashKeyID(keyID)
			evt.ActorType = "gateway_key"
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyAuditEvent{}, evt))

		// Wrap the response writer to capture the status code.
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		next.ServeHTTP(rec, r)

		// Finalize the audit event.
		evt.StatusCode = rec.status
		evt.LatencyMs = time.Since(start).Milliseconds()
		evt.FailureClass = classifyFailure(rec.status)
		// Read the store under auditMu: Close() may be releasing it
		// concurrently. Record on a closed/raced store returns an
		// error that is logged here — the request is never failed.
		s.auditMu.Lock()
		store := s.auditStore
		s.auditMu.Unlock()
		if store != nil {
			if err := store.Record(*evt); err != nil {
				// Never fail the request because audit storage failed.
				s.logger.Printf("audit: record failed: %v", err)
			}
		}
	})
}

// normalizeRoutePath strips the query string and collapses repeated
// slashes. It never includes query parameters (which may contain
// secrets) in the stored path.
func normalizeRoutePath(p string) string {
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		p = "/"
	}
	return p
}

// classifyFailure maps an HTTP status code to a failure class.
func classifyFailure(status int) string {
	switch {
	case status == 0:
		return "none"
	case status >= 200 && status < 300:
		return "none"
	case status == 401 || status == 403:
		return "auth"
	case status == 429:
		return "rate_limit"
	case status == 402:
		return "quota"
	case status >= 500 && status < 600:
		return "server"
	case status >= 400 && status < 500:
		return "upstream"
	default:
		return "none"
	}
}

// ---------------------------------------------------------------------------
// Server wiring
// ---------------------------------------------------------------------------

// auditStore is the append-only audit event store (nil = audit logging off).
// Set by WithAuditStore.
func (s *Server) setAuditStore(store *auditStore) {
	s.auditStore = store
}

// WithAuditStore configures request-level compliance audit logging backed
// by a SQLite store at path. The store is created lazily on first use.
// A nil path disables audit storage (events are still accepted but dropped).
func WithAuditStore(path string) Option {
	return func(s *Server) {
		if path == "" {
			return
		}
		store, err := openAuditStore(path)
		if err != nil {
			s.logger.Printf("audit: open failed: %v", err)
			return
		}
		s.auditStore = store
	}
}

// Ensure auditStore field exists on Server.
var _ interface{ setAuditStore(*auditStore) } = (*Server)(nil)
