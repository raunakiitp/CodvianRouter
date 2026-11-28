// Idempotency-Key billing safety (parity tracker Area 11, Idempotency-Key
// billing safety — MISSING: zero hits in internal/ + cmd/).
//
// A client that retries a billable request after a timeout (the
// response was served but the client never saw it) can be charged
// twice today: nothing records that the request already ran. This
// file records it: when a mutating/billable request carries an
// `Idempotency-Key` header, the key → (response, cost) pair is
// stored, and a repeat with the same key returns the ORIGINAL
// response without re-executing or re-billing.
//
// SCOPE: keys are scoped per gateway key — the store's primary key
// is (gateway_key_id, idempotency_key), so a key from gateway-key A
// never replays gateway-key B's result. A request with no
// authenticated gateway key is never replayed (Begin returns
// IdemNone when the gateway key id is "").
//
// TTL: 24 hours by default. the sector baseline has no server-side
// idempotency handling to match — the sampled code only forwards
// an Idempotency-Key to providers from a dashboard controller and
// shows idempotency stats on the cache page; no TTL is documented
// there. 24 h is the Stripe convention for idempotency-key
// retention, chosen here and documented as OUR choice, not an
// the sector baseline match. After expiry the key executes normally again.
//
// SAME KEY, DIFFERENT REQUEST: the claim records a SHA-256 of
// (method, path, raw body). A repeat with the same key but a
// different body is a 409 (IdemConflict) — replaying a different
// request's response would be a correctness bug, not a safety
// feature. The hash deliberately EXCLUDES the query string:
// /v1/chat/completions ignores query parameters, so two query
// variants of the same request would get identical responses
// anyway; a future endpoint that reads the query must include it
// in its own hash input before reusing this store.
//
// IN-FLIGHT: a repeat while the first request is still running
// gets a 409 (IdemInflight), never a second execution. An
// in-flight claim older than inflightStaleAfter (default 10 min —
// above the 120 s non-streaming request timeout, so a live request
// is never stolen) is treated as crashed and may be re-claimed;
// its replacement executes normally.
//
// STORAGE: SQLite in the data dir (<data-dir>/idempotency.db),
// modernc.org/sqlite, the same driver and the same open
// conventions as the ledger (single writer connection, WAL,
// busy_timeout). The store is opened fresh per Begin call from
// s.cfg.DataDir — there is deliberately no cached handle on the
// Server: the struct lives in server.go, which Wave 3 owns, so no
// field could be added here; each Claim/Complete is a single
// transaction, so no cross-call state is needed. A store failure
// fails OPEN (the request executes normally, unprotected, and the
// failure is logged) — the safety store must never block a paid
// request.
//
// WIRING (coordinator / Wave 3 owns server.go — paste the
// verbatim call-site lines from the G17 report into
// handleChatCompletions; this file only provides Begin/Finish /
// Replay): idempotency applies to NON-STREAMING billable requests
// only — a stream cannot be replayed byte-for-byte, so a
// streaming request with an Idempotency-Key proceeds normally and
// the key is ignored.
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

	_ "modernc.org/sqlite"
)

// idempotencyDefaultTTL is how long a completed key replays (24 h,
// the Stripe convention — see the file header).
const idempotencyDefaultTTL = 24 * time.Hour

// idempotencyInflightStaleAfter is how old an in-flight claim must
// be before a repeat may re-claim it (the first attempt is assumed
// crashed, not still running).
const idempotencyInflightStaleAfter = 10 * time.Minute

// idempotencyMaxKeyLen caps the key at 255 bytes (the Stripe
// limit); longer keys are rejected, never truncated (truncation
// could alias two different keys).
const idempotencyMaxKeyLen = 255

// idempotencyMaxBodyBytes caps a stored response body at 4 MiB.
// A larger response is still billed once — the key is completed
// with body_too_large set — but a repeat gets a 409 naming a new
// key instead of a replayed body that would bloat the store.
const idempotencyMaxBodyBytes = 4 << 20

// idempotencyDBFile is the SQLite file name under the data dir.
const idempotencyDBFile = "idempotency.db"

// idempotencySchema is the whole store: one row per
// (gateway key, idempotency key). status is "in-flight" or
// "completed"; only completed rows replay.
const idempotencySchema = `
CREATE TABLE IF NOT EXISTS idempotency_keys (
  gateway_key_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash   TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL DEFAULT 'in-flight',
  status_code    INTEGER NOT NULL DEFAULT 0,
  response_body  BLOB NOT NULL DEFAULT '',
  content_type   TEXT NOT NULL DEFAULT '',
  cost_usd       REAL NOT NULL DEFAULT 0,
  has_cost       INTEGER NOT NULL DEFAULT 0,
  body_too_large INTEGER NOT NULL DEFAULT 0,
  created_unix   INTEGER NOT NULL,
  expires_unix   INTEGER NOT NULL,
  PRIMARY KEY (gateway_key_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_idempotency_expires
  ON idempotency_keys(expires_unix);
`

// IdempotencyState is what Begin decided for one request.
type IdempotencyState int

const (
	// IdemNone: no Idempotency-Key was sent (or no gateway key /
	// no store — fail-open). Execute normally.
	IdemNone IdempotencyState = iota
	// IdemReplay: the key completed before — return Record.
	IdemReplay
	// IdemProceed: the key is newly claimed — execute, then Finish.
	IdemProceed
	// IdemInflight: the first request with this key is still
	// running — answer 409, do not execute.
	IdemInflight
	// IdemConflict: the key was used for a different request
	// (different request hash) — answer 409, do not execute.
	IdemConflict
)

// IdempotencyRecord is one stored key → (response, cost) pair.
type IdempotencyRecord struct {
	GatewayKeyID string
	Key          string
	RequestHash  string
	Status       string // "in-flight" | "completed"
	StatusCode   int
	ResponseBody []byte
	ContentType  string
	CostUSD      float64
	HasCost      bool
	BodyTooLarge bool
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// IdempotencyStore is the SQLite idempotency store. TTL is the
// replay window for completed keys (default 24 h); InflightStale
// is the age after which an in-flight claim may be re-claimed
// (default 10 min). Both are fields, not constants, so tests can
// shorten them. The zero value is NOT usable — always build via
// OpenIdempotencyStore.
type IdempotencyStore struct {
	db            *sql.DB
	path          string
	TTL           time.Duration
	InflightStale time.Duration
}

// OpenIdempotencyStore opens (creating if needed) the SQLite store
// at path and applies the schema. Parent directories are created.
func OpenIdempotencyStore(path string) (*IdempotencyStore, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("idempotency: create directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("idempotency: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("idempotency: enable WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("idempotency: busy timeout: %w", err)
	}
	if _, err := db.Exec(idempotencySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("idempotency: apply schema: %w", err)
	}
	return &IdempotencyStore{db: db, path: path, TTL: idempotencyDefaultTTL, InflightStale: idempotencyInflightStaleAfter}, nil
}

// Close closes the store's database handle.
func (s *IdempotencyStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// ValidateIdempotencyKey rejects a non-empty key that must not be
// stored: over 255 bytes, or carrying control characters (header
// injection shapes). An empty key is NOT an error here — the
// caller treats "no key sent" as IdemNone before calling this.
func ValidateIdempotencyKey(key string) error {
	if len(key) > idempotencyMaxKeyLen {
		return fmt.Errorf("idempotency-key: must be at most %d bytes", idempotencyMaxKeyLen)
	}
	for _, r := range key {
		if r < ' ' || r == 0x7f {
			return errors.New("idempotency-key: must not contain control characters")
		}
	}
	return nil
}

// idempotencyRequestHash is the request identity a key binds to:
// SHA-256 over method, path and the raw body, NUL-separated so no
// concatenation can alias two different requests.
func idempotencyRequestHash(method, path string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(method))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(path))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// IdempotencyClaim is the outcome of one Claim call.
type IdempotencyClaim struct {
	State  IdempotencyState
	Record *IdempotencyRecord
}

// scanIdempotencyRecord reads one row into a record. Columns must
// match the SELECT order in Claim/Lookup.
func scanIdempotencyRecord(row *sql.Row, rec *IdempotencyRecord) error {
	var created, expires int64
	var hasCost, tooLarge int
	err := row.Scan(&rec.GatewayKeyID, &rec.Key, &rec.RequestHash, &rec.Status,
		&rec.StatusCode, &rec.ResponseBody, &rec.ContentType, &rec.CostUSD,
		&hasCost, &tooLarge, &created, &expires)
	if err != nil {
		return err
	}
	rec.HasCost = hasCost != 0
	rec.BodyTooLarge = tooLarge != 0
	rec.CreatedAt = time.Unix(created, 0).UTC()
	rec.ExpiresAt = time.Unix(expires, 0).UTC()
	return nil
}

// idempotencyClaimMu serialises Claim across store instances in
// this process. The store is opened fresh per Begin, so two
// concurrent requests hold separate SQLite connections and a
// deferred transaction alone cannot exclude the read-then-insert
// race (both SELECT no-row, the second INSERT then fails on the
// PK and the caller fail-opens into a double execution). The
// mutex makes the check-and-insert atomic in-process; the PK
// constraint remains as a backstop. Single server process, so
// no cross-process coordination is needed.
var idempotencyClaimMu sync.Mutex

// Claim atomically decides one request's fate for (gatewayKeyID,
// key): replay a completed key, report an in-flight or conflicting
// key, or claim a fresh key for execution. Expired rows are
// deleted first (an expired key executes normally again). The
// check-and-insert is atomic via idempotencyClaimMu (see above),
// so two concurrent repeats of the same key cannot both claim it.
func (s *IdempotencyStore) Claim(ctx context.Context, gatewayKeyID, key, reqHash string) (IdempotencyClaim, error) {
	if s == nil || s.db == nil {
		return IdempotencyClaim{}, errors.New("idempotency: store is closed")
	}
	if err := ValidateIdempotencyKey(key); err != nil {
		return IdempotencyClaim{}, err
	}
	if gatewayKeyID == "" {
		return IdempotencyClaim{}, errors.New("idempotency: gateway key id is required")
	}
	// Serialised across store instances: see idempotencyClaimMu.
	idempotencyClaimMu.Lock()
	defer idempotencyClaimMu.Unlock()
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IdempotencyClaim{}, fmt.Errorf("idempotency: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// An expired key is gone: delete it so the repeat below
	// claims fresh instead of replaying a stale response.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE gateway_key_id = ? AND idempotency_key = ? AND expires_unix <= ?`,
		gatewayKeyID, key, now.Unix()); err != nil {
		return IdempotencyClaim{}, fmt.Errorf("idempotency: expire: %w", err)
	}
	var rec IdempotencyRecord
	err = scanIdempotencyRecord(tx.QueryRowContext(ctx,
		`SELECT gateway_key_id, idempotency_key, request_hash, status, status_code,
		        response_body, content_type, cost_usd, has_cost, body_too_large,
		        created_unix, expires_unix
		   FROM idempotency_keys WHERE gateway_key_id = ? AND idempotency_key = ?`,
		gatewayKeyID, key), &rec)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Fresh key: claim it in-flight, then execute.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys(gateway_key_id, idempotency_key, request_hash, status,
				status_code, response_body, content_type, cost_usd, has_cost, body_too_large,
				created_unix, expires_unix)
			 VALUES(?, ?, ?, 'in-flight', 0, '', '', 0, 0, 0, ?, ?)`,
			gatewayKeyID, key, reqHash, now.Unix(), now.Add(s.TTL).Unix()); err != nil {
			return IdempotencyClaim{}, fmt.Errorf("idempotency: claim: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return IdempotencyClaim{}, fmt.Errorf("idempotency: commit claim: %w", err)
		}
		return IdempotencyClaim{State: IdemProceed}, nil
	case err != nil:
		return IdempotencyClaim{}, fmt.Errorf("idempotency: lookup: %w", err)
	}
	// A key exists. A different request under the same key is a
	// conflict, whatever the stored state — never replay a
	// different request's response.
	if rec.RequestHash != reqHash {
		if err := tx.Commit(); err != nil {
			return IdempotencyClaim{}, fmt.Errorf("idempotency: commit: %w", err)
		}
		return IdempotencyClaim{State: IdemConflict, Record: &rec}, nil
	}
	switch rec.Status {
	case "completed":
		if err := tx.Commit(); err != nil {
			return IdempotencyClaim{}, fmt.Errorf("idempotency: commit: %w", err)
		}
		return IdempotencyClaim{State: IdemReplay, Record: &rec}, nil
	default: // "in-flight"
		// A crashed first attempt leaves its claim behind; after
		// the stale window the repeat may re-claim it and execute.
		if now.Sub(rec.CreatedAt) > s.InflightStale {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM idempotency_keys WHERE gateway_key_id = ? AND idempotency_key = ?`,
				gatewayKeyID, key); err != nil {
				return IdempotencyClaim{}, fmt.Errorf("idempotency: reclaim: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO idempotency_keys(gateway_key_id, idempotency_key, request_hash, status,
					status_code, response_body, content_type, cost_usd, has_cost, body_too_large,
					created_unix, expires_unix)
				 VALUES(?, ?, ?, 'in-flight', 0, '', '', 0, 0, 0, ?, ?)`,
				gatewayKeyID, key, reqHash, now.Unix(), now.Add(s.TTL).Unix()); err != nil {
				return IdempotencyClaim{}, fmt.Errorf("idempotency: reclaim: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return IdempotencyClaim{}, fmt.Errorf("idempotency: commit reclaim: %w", err)
			}
			return IdempotencyClaim{State: IdemProceed}, nil
		}
		if err := tx.Commit(); err != nil {
			return IdempotencyClaim{}, fmt.Errorf("idempotency: commit: %w", err)
		}
		return IdempotencyClaim{State: IdemInflight, Record: &rec}, nil
	}
}

// Lookup returns the stored record for (gatewayKeyID, key), or
// found=false when there is none (or it expired — expired rows
// are deleted, so a repeat executes normally).
func (s *IdempotencyStore) Lookup(ctx context.Context, gatewayKeyID, key string) (*IdempotencyRecord, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, errors.New("idempotency: store is closed")
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE gateway_key_id = ? AND idempotency_key = ? AND expires_unix <= ?`,
		gatewayKeyID, key, now.Unix()); err != nil {
		return nil, false, fmt.Errorf("idempotency: expire: %w", err)
	}
	var rec IdempotencyRecord
	err := scanIdempotencyRecord(s.db.QueryRowContext(ctx,
		`SELECT gateway_key_id, idempotency_key, request_hash, status, status_code,
		        response_body, content_type, cost_usd, has_cost, body_too_large,
		        created_unix, expires_unix
		   FROM idempotency_keys WHERE gateway_key_id = ? AND idempotency_key = ?`,
		gatewayKeyID, key), &rec)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("idempotency: lookup: %w", err)
	default:
		return &rec, true, nil
	}
}

// Complete records the executed response for a claimed key, so a
// repeat replays it. Only an in-flight claim completes: completing
// a key that was never claimed (or whose claim was stolen after
// the stale window) is a caller bug and is reported, not silently
// swallowed — the request itself already ran, so the billing row
// exists either way. Bodies over the cap are not stored (see the
// const); the key still completes with body_too_large set, so the
// repeat is a 409 naming a new key rather than a second billing.
func (s *IdempotencyStore) Complete(ctx context.Context, gatewayKeyID, key string, statusCode int, body []byte, contentType string, costUSD float64, hasCost bool) error {
	if s == nil || s.db == nil {
		return errors.New("idempotency: store is closed")
	}
	tooLarge := 0
	stored := body
	if len(body) > idempotencyMaxBodyBytes {
		tooLarge = 1
		// Empty blob, never nil: the column is NOT NULL, and a
		// nil []byte would bind as SQL NULL and fail the insert.
		stored = []byte{}
	}
	hasCostInt := 0
	if hasCost {
		hasCostInt = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE idempotency_keys
		    SET status = 'completed', status_code = ?, response_body = ?,
		        content_type = ?, cost_usd = ?, has_cost = ?, body_too_large = ?
		  WHERE gateway_key_id = ? AND idempotency_key = ? AND status = 'in-flight'`,
		statusCode, stored, contentType, costUSD, hasCostInt, tooLarge, gatewayKeyID, key)
	if err != nil {
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	if n == 0 {
		return errors.New("idempotency: complete: no in-flight claim for this key (never claimed, already completed, or claim stolen)")
	}
	return nil
}

// Release drops an in-flight claim so a retry executes fresh.
// Call it when a claimed request FAILS before a response was
// served (engine error, nil upstream response, guardrail block
// after the claim): without it the key stays in-flight and a
// client retry inside the 10-minute stale window gets a 409
// instead of the fresh execution a failure deserves. A completed
// row is never touched (status = 'in-flight' in the WHERE), so
// Release cannot delete a replayable response. A no-op when no
// in-flight claim exists.
func (s *IdempotencyStore) Release(ctx context.Context, gatewayKeyID, key string) error {
	if s == nil || s.db == nil {
		return errors.New("idempotency: store is closed")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE gateway_key_id = ? AND idempotency_key = ? AND status = 'in-flight'`,
		gatewayKeyID, key); err != nil {
		return fmt.Errorf("idempotency: release: %w", err)
	}
	return nil
}

// PruneExpired deletes expired rows and returns how many were
// removed. Expiry is also lazy (Claim/Lookup delete the row they
// touch), so this is a hygiene sweep, not a correctness path.
func (s *IdempotencyStore) PruneExpired(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("idempotency: store is closed")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE expires_unix <= ?`, time.Now().UTC().Unix())
	if err != nil {
		return 0, fmt.Errorf("idempotency: prune: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("idempotency: prune: %w", err)
	}
	return int(n), nil
}

// Count returns the number of stored rows (all states). Tests use
// it to prove a replay created no second row; production code
// does not need it.
func (s *IdempotencyStore) Count(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("idempotency: store is closed")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM idempotency_keys`).Scan(&n); err != nil {
		return 0, fmt.Errorf("idempotency: count: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Server check functions (the call-site API — see the G17 report
// for the verbatim handleChatCompletions lines).
// ---------------------------------------------------------------------------

// idempotencyStore opens a fresh store handle from the data dir.
// A fresh handle per call is deliberate (see the file header): the
// Server struct is Wave-3-owned, so no handle can be cached on it.
// The caller closes the returned store when Begin hands it back
// (Finish does that); on every other outcome Begin closes it
// itself before returning.
func (s *Server) idempotencyStore() (*IdempotencyStore, error) {
	if s.cfg == nil || s.cfg.DataDir == "" {
		return nil, errors.New("idempotency: no data directory configured")
	}
	return OpenIdempotencyStore(filepath.Join(s.cfg.DataDir, idempotencyDBFile))
}

// idempotencyBegin is the result of the pre-execution check. Store
// is non-nil only when State is IdemProceed — the caller owns it
// until idempotencyFinish, which closes it. Key echoes the header
// value ("" when State is IdemNone); gatewayKeyID is the scope
// Finish completes under.
type idempotencyBegin struct {
	State        IdempotencyState
	Record       *IdempotencyRecord
	Store        *IdempotencyStore
	Key          string
	gatewayKeyID string
}

// idempotencyBegin runs the pre-execution check for one billable
// request: read the Idempotency-Key header, validate it, and claim
// it. reqHash must be idempotencyRequestHash(method, path, rawBody)
// for the request being served. An absent header, an
// unauthenticated caller (gatewayKeyID ""), or a store failure all
// yield IdemNone with a nil error — the request executes normally,
// unprotected, and the store failure is logged, never fatal. An
// invalid non-empty key yields an error: the call-site answers
// 400. A replay / in-flight / conflict outcome yields no error —
// the call-site answers from Record (replay) or with a 409.
func (s *Server) idempotencyBegin(ctx context.Context, gatewayKeyID string, r *http.Request, reqHash string) (idempotencyBegin, error) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || gatewayKeyID == "" {
		return idempotencyBegin{State: IdemNone}, nil
	}
	if err := ValidateIdempotencyKey(key); err != nil {
		return idempotencyBegin{}, err
	}
	store, err := s.idempotencyStore()
	if err != nil {
		// Fail open (see the file header): never block a paid
		// request on the safety store.
		s.logger.Printf("idempotency: store unavailable, proceeding unprotected: %v", err)
		return idempotencyBegin{State: IdemNone}, nil
	}
	claim, err := store.Claim(ctx, gatewayKeyID, key, reqHash)
	if err != nil {
		_ = store.Close()
		s.logger.Printf("idempotency: claim failed, proceeding unprotected: %v", err)
		return idempotencyBegin{State: IdemNone}, nil
	}
	switch claim.State {
	case IdemProceed:
		return idempotencyBegin{State: IdemProceed, Store: store, Key: key, gatewayKeyID: gatewayKeyID}, nil
	default:
		// Replay / in-flight / conflict: the record is already
		// materialised; the store handle is no longer needed.
		_ = store.Close()
		return idempotencyBegin{State: claim.State, Record: claim.Record, Key: key, gatewayKeyID: gatewayKeyID}, nil
	}
}

// idempotencyFinish records the executed response for a claimed
// key and closes the store. A no-op unless b is an IdemProceed
// begin. A completion failure is logged, never fatal — the
// request already ran and its billing row exists; the only cost
// is that a repeat of this key may execute again.
func (s *Server) idempotencyFinish(ctx context.Context, b idempotencyBegin, statusCode int, body []byte, contentType string, costUSD float64, hasCost bool) {
	if b.Store == nil || b.State != IdemProceed {
		return
	}
	defer func() { _ = b.Store.Close() }()
	if err := b.Store.Complete(ctx, b.RecordGatewayKeyID(), b.Key, statusCode, body, contentType, costUSD, hasCost); err != nil {
		s.logger.Printf("idempotency: complete failed for key %q: %v", b.Key, err)
	}
}

// idempotencyRelease drops a claimed key's in-flight row (see
// IdempotencyStore.Release) and closes the store. A no-op unless
// b is an IdemProceed begin. Call it on every error return after
// Begin — engine error, nil upstream response, guardrail block —
// so a retry executes fresh instead of meeting a 409. A release
// failure is logged, never fatal.
func (s *Server) idempotencyRelease(ctx context.Context, b idempotencyBegin) {
	if b.Store == nil || b.State != IdemProceed {
		return
	}
	defer func() { _ = b.Store.Close() }()
	if err := b.Store.Release(ctx, b.RecordGatewayKeyID(), b.Key); err != nil {
		s.logger.Printf("idempotency: release failed for key %q: %v", b.Key, err)
	}
}

// RecordGatewayKeyID returns the gateway key id a begin was scoped to.
func (b idempotencyBegin) RecordGatewayKeyID() string {
	return b.gatewayKeyID
}

// idempotencyReplay writes a stored response back to the client:
// the original status + body + content-type, plus
// X-Idempotency-Replayed: true so the client can tell a replay
// from a fresh execution, and X-Codvian-Cost-USD from the stored
// cost when the original was priced. A body_too_large record
// cannot be replayed — the call-site must answer 409 naming a
// new key instead (idempotencyReplay returns false then).
func idempotencyReplay(w http.ResponseWriter, rec *IdempotencyRecord) bool {
	if rec == nil || rec.BodyTooLarge {
		return false
	}
	h := w.Header()
	if rec.ContentType != "" {
		h.Set("Content-Type", rec.ContentType)
	}
	h.Set("X-Idempotency-Replayed", "true")
	if rec.HasCost {
		h.Set(headerCodvianCostUSD, fmt.Sprintf("%.10f", rec.CostUSD))
	}
	status := rec.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(rec.ResponseBody)
	return true
}
