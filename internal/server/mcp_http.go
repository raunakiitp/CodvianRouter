// MCP remote transports (M9.2): SSE + Streamable HTTP.
//
// The stdio transport (internal/mcp Serve) has one caller — the
// local operator — and opts into the full grant explicitly. These
// remote transports are reachable over the network by callers who
// do NOT have local filesystem access, so every request is
// authenticated as a gateway key and authorised in the disjoint
// mcp: scope namespace (M9.1), never in the HTTP scope namespace:
//
//   - Auth: gateway-key Bearer auth (the guard table requires it;
//     the handlers additionally refuse keyless requests outright,
//     so auth-relaxed mode can never open an MCP route keyless).
//   - Identity: per request, GatewayKeyClient(keyID, validated
//     mcp: scopes ONLY). A bare "*" scope and any non-mcp: scope
//     (admin, chat, models, ...) is REJECTED with 403 at the
//     transport — the HTTP guard treats "admin" as implying every
//     scope, so passing HTTP scopes through would escalate a
//     chat/admin key into MCP tools (M9.1 SECURITY REVIEW N4).
//     The identity is passed to HandleWithIdentity on every call;
//     the server default (Anonymous, deny-all) is never used.
//   - Rate limiting: a per-key token bucket (burst 30, refill
//     1/s = 60/min sustained) and a per-IP token bucket (burst
//     120, refill 5/s = 300/min sustained), both in-memory. A
//     trip answers 429 with Retry-After and type mcp_rate_limit.
//   - Sessions: crypto/rand ids, one owner key each (a foreign
//     key's session answers 404, indistinguishable from unknown),
//     capped at 64 per transport and 16 per key per transport,
//     idle-evicted after 5 minutes (swept lazily, at most once a
//     minute), removed on disconnect (SSE) or DELETE (streamable).
//   - Toggles: the enable flags live in the M9.7 state on the
//     Server (s.mcpSSEEnabled / s.mcpStreamEnabled, flipped by
//     POST /api/mcp/status) — this slice reads that state, it
//     does not keep a second copy. Both default OFF (fail-closed,
//     pinned by the M9.7 tests); a disabled transport answers
//     503 on its routes. The flags are in-memory only: a restart
//     restores the default (off).
//
// Transports (shape studied from the reference router's httpTransport.ts,
// reimplemented here — no code copied):
//
//   - SSE: GET /api/mcp/sse opens the event stream (first event
//     is `endpoint` with the POST URL carrying the session id);
//     POST /api/mcp/sse?session_id=<id> sends one JSON-RPC
//     message and answers 202, the response arriving on the
//     stream as a `message` event. A POST before GET is a 404:
//     the session does not exist until GET opens it.
//   - Streamable HTTP: POST /api/mcp/stream carries one JSON-RPC
//     message; an initialize with no Mcp-Session-Id opens a
//     session (the id returns in the Mcp-Session-Id response
//     header), later messages address it by that header, a
//     non-initialize without an id is a 400, and an unknown id
//     is a 404 (an initialize with a stale id opens a fresh
//     session, like the reference router's auto-recovery). A POST that is a
//     request answers 200 application/json directly; a
//     notification answers 202 with no body. GET /api/mcp/stream
//     opens the server→client SSE stream for a session (the
//     session survives the stream closing). DELETE
//     /api/mcp/stream/{id} terminates a session (204; unknown
//     or foreign ids are 404).
//
// The MCP server behind the transports is built lazily from the
// live serve-mode state (serverMCPSource, below): providers,
// models and status from the registry/config, combos and the
// effective strategy from the routing config, cache stats/flush
// from the live caches, DB health/stats from the data-dir files,
// memory from the file-backed store. The M9.3 ledger-backed
// methods (ComboMetrics, ModelStats, CostReport, CheckQuota,
// SetBudgetGuard) read and write the serve process's own live
// ledger (s.ledger) — the same handle the analytics and budget
// endpoints use — and ExplainRoute/SimulateRoute dry-run from the
// live provider feed and price book; SetRoutingStrategy persists
// through the same routingPersist the dashboard strategy endpoint
// uses, persist-first; CreateCombo creates through
// ComboStore.Create — the same validate + mirror + persist path
// POST /v1/combos runs, so the MCP tool and the REST handler
// share one implementation. The stdio
// Source (registryMCPSource in cmd/codvian-router) implements
// all of these against its own ledger/config handles — the
// "backend does not exist yet in either process" claim this
// comment used to carry is stale and was wrong even when the
// stubs were written; the gap was the serve-mode wiring, not
// the backend.
// Batches (a JSON array body) are not supported: one JSON-RPC
// message per POST, like stdio's one message per line.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/strategy"
)

// MCP transport limits (M9.2). All in-memory: a restart resets
// them, and the numbers are stated here, not hidden in code.
const (
	// mcpMaxMessageBytes caps one JSON-RPC message body: the same
	// 1 MiB the stdio scanner allows per line.
	mcpMaxMessageBytes = 1 << 20
	// mcpSessionIdleTimeout is how long a session with no traffic
	// and no open stream lives before the lazy sweep evicts it
	// (the reference router's default is the same five minutes).
	mcpSessionIdleTimeout = 5 * time.Minute
	// mcpSessionSweepInterval is how often the lazy sweep may run
	// (checked on session creation and status reads).
	mcpSessionSweepInterval = 60 * time.Second
	// mcpMaxSessionsPerTransport caps live sessions per transport;
	// mcpMaxSessionsPerKey caps them per gateway key, so one key
	// cannot hog the table. A create past either cap is a 503.
	mcpMaxSessionsPerTransport = 64
	mcpMaxSessionsPerKey       = 16
	// mcpMaxStreamsPerSession caps concurrent GET (server→client)
	// streams per Streamable session at 1 — one stream per
	// session, like SSE's single stream. Without it a session's
	// GET streams were unbounded and the idle sweep skips any
	// session with an open stream, so one key could hold a
	// goroutine + connection per GET indefinitely (H2). With
	// the cap the bound is 1 stream per session, 16 sessions
	// per key, 64 sessions per transport — a second concurrent
	// GET on a session answers 409 until the first stream
	// closes, and the session cap still bounds the total.
	mcpMaxStreamsPerSession = 1
	// mcpSSEOutboxSize bounds queued responses per session: a
	// client that never reads its GET stream cannot grow the
	// server without bound (a full outbox drops the oldest
	// response and logs it).
	mcpSSEOutboxSize = 128
	// mcpRateKeyBurst / mcpRateKeyRefillPerSec: per-key token
	// bucket — burst 30, refill 1/s (60/min sustained). An MCP
	// client session is initialize + tools/list + occasional
	// calls; 60/min is generous for a human-driven agent and
	// tight for a runaway loop, which is the point.
	mcpRateKeyBurst        = 30.0
	mcpRateKeyRefillPerSec = 1.0
	// mcpRateIPBurst / mcpRateIPRefillPerSec: per-IP token bucket
	// — burst 120, refill 5/s (300/min sustained). A second layer
	// behind the per-key bucket, so many keys behind one NAT do
	// not trip each other.
	mcpRateIPBurst        = 120.0
	mcpRateIPRefillPerSec = 5.0
	// mcpHeartbeatInterval is the SSE keep-alive cadence on open
	// streams (a ": ping" comment, never a protocol message).
	mcpHeartbeatInterval = 30 * time.Second
	// mcpUnknownSessionCode is the JSON-RPC error code for an
	// unknown/foreign session id on the Streamable transport
	// (the reference router uses the same -32000).
	mcpUnknownSessionCode = -32000
)

// mcpTransportKind is one remote transport.
type mcpTransportKind string

const (
	mcpTransportSSE        mcpTransportKind = "sse"
	mcpTransportStreamable mcpTransportKind = "streamable"
)

// mcpHTTPSession is one live MCP session on one transport. It
// belongs to exactly one gateway key (keyID); no session state
// ever leaks across keys — a foreign key's session answers 404,
// indistinguishable from unknown. lastActivity and streams are
// guarded by the state's mutex, never touched directly; the
// active combo has its own mutex (comboMu), so a tool call
// reading it never contends with the session table.
type mcpHTTPSession struct {
	id           string
	keyID        string
	transport    mcpTransportKind
	createdAt    time.Time
	lastActivity time.Time
	// out queues JSON-RPC responses for the session's GET stream.
	out chan []byte
	// done closes when the session is terminated.
	done chan struct{}
	// streams counts open GET streams (streamable only — an SSE
	// session has exactly its one GET stream, and disconnecting
	// it terminates the session). The idle sweep never evicts a
	// session with an open stream.
	streams    int
	terminated bool
	// comboMu guards activeCombo.
	comboMu sync.Mutex
	// activeCombo is THIS session's active combo (H1): the
	// default combo this session's routing tools report and
	// explain/simulate default to. It never changes what the
	// serving router routes, and it never leaks to another
	// session — two keys switching combos see only their own
	// selection, even on the same server.
	activeCombo string
}

// ActiveCombo returns this session's active combo ("" = none).
// It implements mcp.ComboState: the transport binds the session
// into the tool call's request context, and the bound Source
// reads the selection from here.
func (sess *mcpHTTPSession) ActiveCombo() string {
	sess.comboMu.Lock()
	defer sess.comboMu.Unlock()
	return sess.activeCombo
}

// SetActiveCombo stores this session's active combo ("" clears
// it). It implements mcp.ComboState.
func (sess *mcpHTTPSession) SetActiveCombo(name string) {
	sess.comboMu.Lock()
	sess.activeCombo = name
	sess.comboMu.Unlock()
}

// mcpTokenBucket is one token bucket: capacity burst, refill
// tokens per second, last refill time.
type mcpTokenBucket struct {
	tokens float64
	last   time.Time
}

// mcpRateLimiter is the transport rate limiter: one token bucket
// per gateway key and one per client IP, in-memory. now is the
// clock the limiter refills by — time.Now in production, frozen
// by TestMCPHTTPRateLimitIPBucket so the exact-burst assertion
// cannot be shifted by refill mid-loop on a slow (-race) run.
type mcpRateLimiter struct {
	mu   sync.Mutex
	keys map[string]*mcpTokenBucket
	ips  map[string]*mcpTokenBucket
	now  func() time.Time
}

func newMCPRateLimiter() *mcpRateLimiter {
	return &mcpRateLimiter{keys: map[string]*mcpTokenBucket{}, ips: map[string]*mcpTokenBucket{}, now: time.Now}
}

// nowTime returns the limiter's clock (time.Now unless a test froze it).
func (l *mcpRateLimiter) nowTime() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// allowLocked spends one token from the bucket for id (creating
// it full), refilling by elapsed time first. Caller must hold mu.
func (l *mcpRateLimiter) allowLocked(buckets map[string]*mcpTokenBucket, id string, burst, refill float64, now time.Time) bool {
	if len(buckets) > 4096 {
		// Bound the table: drop buckets idle over 10 minutes.
		for k, b := range buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(buckets, k)
			}
		}
	}
	b, ok := buckets[id]
	if !ok {
		b = &mcpTokenBucket{tokens: burst, last: now}
		buckets[id] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(burst, b.tokens+elapsed*refill)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// allowKey spends one token from the per-key bucket.
func (l *mcpRateLimiter) allowKey(keyID string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allowLocked(l.keys, keyID, mcpRateKeyBurst, mcpRateKeyRefillPerSec, now)
}

// allowIP spends one token from the per-IP bucket.
func (l *mcpRateLimiter) allowIP(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allowLocked(l.ips, ip, mcpRateIPBurst, mcpRateIPRefillPerSec, now)
}

// mcpHTTPState is the per-Server MCP transport state: live
// sessions, the rate limiter, the lazily built MCP server, and
// the session-active combo. It lives in the package-level map
// below (keyed by server pointer) because server.go is
// route-wiring-only for this slice — no Server field is added.
// One entry per Server that ever serves an MCP route; entries are
// small (a few maps) and the production process has exactly one
// server.
type mcpHTTPState struct {
	mu sync.Mutex
	// srv is the server this state belongs to (the map key).
	srv *Server
	// sessions by transport.
	sseSessions    map[string]*mcpHTTPSession
	streamSessions map[string]*mcpHTTPSession
	rate           *mcpRateLimiter
	// mcpSrv is the lazily built MCP server (source: live
	// serve-mode state). Nil until the first transport request.
	mcpSrv *mcp.Server
	// activeCombo is the process-wide fallback selection, used
	// only when a Source call carries no session (a direct
	// source-level call, never a transport tool call: the
	// transports bind the session's own combo into the request
	// context, and the bound Source reads that instead — see
	// serverMCPSource.WithContext). It never changes what the
	// serving router routes. "" = none selected.
	activeCombo string
	lastSweep   time.Time
}

// mcpHTTPStates is the per-Server transport state registry.
var mcpHTTPStates sync.Map // *Server -> *mcpHTTPState

// mcpHTTPStateFor returns the transport state for s, creating it
// on first use.
func mcpHTTPStateFor(s *Server) *mcpHTTPState {
	if v, ok := mcpHTTPStates.Load(s); ok {
		return v.(*mcpHTTPState)
	}
	st := &mcpHTTPState{
		srv:            s,
		sseSessions:    map[string]*mcpHTTPSession{},
		streamSessions: map[string]*mcpHTTPSession{},
		rate:           newMCPRateLimiter(),
	}
	actual, _ := mcpHTTPStates.LoadOrStore(s, st)
	return actual.(*mcpHTTPState)
}

// transportEnabled reads the M9.7 operator toggle for kind from
// the Server (the single source of truth — this slice keeps no
// second copy). Both default OFF (fail-closed).
func (st *mcpHTTPState) transportEnabled(kind mcpTransportKind) bool {
	s := st.srv
	s.mcpTransportMu.Lock()
	defer s.mcpTransportMu.Unlock()
	if kind == mcpTransportSSE {
		return s.mcpSSEEnabled
	}
	return s.mcpStreamEnabled
}

// sessionsLocked returns the session map for kind. Caller must
// hold st.mu.
func (st *mcpHTTPState) sessionsLocked(kind mcpTransportKind) map[string]*mcpHTTPSession {
	if kind == mcpTransportSSE {
		return st.sseSessions
	}
	return st.streamSessions
}

// sweepIfDueLocked evicts idle sessions when the sweep is due
// (at most once a minute). Caller must hold st.mu.
func (st *mcpHTTPState) sweepIfDueLocked(now time.Time) {
	if now.Sub(st.lastSweep) < mcpSessionSweepInterval {
		return
	}
	st.lastSweep = now
	for _, sessions := range []map[string]*mcpHTTPSession{st.sseSessions, st.streamSessions} {
		for id, sess := range sessions {
			if sess.streams > 0 {
				continue // an open stream is live traffic
			}
			if now.Sub(sess.lastActivity) > mcpSessionIdleTimeout {
				st.terminateLocked(sessions, id)
			}
		}
	}
}

// terminateLocked removes a session and closes its done channel.
// Caller must hold st.mu. Idempotent.
func (st *mcpHTTPState) terminateLocked(sessions map[string]*mcpHTTPSession, id string) {
	sess, ok := sessions[id]
	if !ok {
		return
	}
	delete(sessions, id)
	if !sess.terminated {
		sess.terminated = true
		close(sess.done)
	}
}

// newSessionID mints a session id from crypto/rand: 128 bits,
// hex-encoded. Never sequential, never derived from the key.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// newSessionLocked creates a session for keyID on kind, enforcing
// both caps (per transport, per key). Caller must hold st.mu; the
// sweep runs first so stale sessions free their slots.
func (st *mcpHTTPState) newSessionLocked(kind mcpTransportKind, keyID string) (*mcpHTTPSession, error) {
	now := time.Now()
	st.sweepIfDueLocked(now)
	sessions := st.sessionsLocked(kind)
	if len(sessions) >= mcpMaxSessionsPerTransport {
		return nil, fmt.Errorf("mcp: %s session limit reached (%d sessions)", kind, mcpMaxSessionsPerTransport)
	}
	perKey := 0
	for _, sess := range sessions {
		if sess.keyID == keyID {
			perKey++
		}
	}
	if perKey >= mcpMaxSessionsPerKey {
		return nil, fmt.Errorf("mcp: per-key %s session limit reached (%d sessions for this key)", kind, mcpMaxSessionsPerKey)
	}
	id, err := newSessionID()
	if err != nil {
		return nil, fmt.Errorf("mcp: cannot mint session id: %w", err)
	}
	sess := &mcpHTTPSession{
		id:           id,
		keyID:        keyID,
		transport:    kind,
		createdAt:    now,
		lastActivity: now,
		out:          make(chan []byte, mcpSSEOutboxSize),
		done:         make(chan struct{}),
	}
	sessions[id] = sess
	return sess, nil
}

// sessionLocked returns the session for kind+id, or nil. Caller
// must hold st.mu.
func (st *mcpHTTPState) sessionLocked(kind mcpTransportKind, id string) *mcpHTTPSession {
	return st.sessionsLocked(kind)[id]
}

// terminate terminates a session by kind+id. Idempotent.
func (st *mcpHTTPState) terminate(kind mcpTransportKind, id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.terminateLocked(st.sessionsLocked(kind), id)
}

// touch records activity on a session.
func (st *mcpHTTPState) touch(sess *mcpHTTPSession) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !sess.terminated {
		sess.lastActivity = time.Now()
	}
}

// counts returns the live session counts per transport, sweeping
// idle sessions first so evicted ones are not counted.
func (st *mcpHTTPState) counts() (sse, streamable int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepIfDueLocked(time.Now())
	return len(st.sseSessions), len(st.streamSessions)
}

// mcpServer returns the MCP server for this state, building it
// lazily from the live serve-mode state on first use. The
// server's configured identity is left at the Anonymous default
// (deny-all): every transport request passes its own per-request
// identity to HandleWithIdentity, never Handle.
func (st *mcpHTTPState) mcpServer() *mcp.Server {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.mcpSrv != nil {
		return st.mcpSrv
	}
	s := st.srv
	src := &serverMCPSource{srv: s, st: st}
	ms := mcp.New(src, mcpServerVersion(s))
	if s.guardrails != nil {
		ms.WithGuardrail(s.guardrails)
	}
	// Audit every HTTP tool call into the same log the dashboard
	// reads (GET /api/mcp/audit): the wired log when one is set,
	// else the data-dir log the stdio server uses. A log that
	// cannot be opened warns and the server still runs unaudited
	// — the same availability-over-audit decision stdio makes.
	// No DataDir means no audit path.
	if s.mcpAudit != nil {
		ms.WithAuditLog(s.mcpAudit)
	} else if s.cfg != nil && s.cfg.DataDir != "" {
		if auditLog, err := mcp.OpenAuditLog(filepath.Join(s.cfg.DataDir, "mcp", "audit.jsonl")); err == nil {
			ms.WithAuditLog(auditLog)
		} else {
			s.logger.Printf("mcp: HTTP audit log unavailable (%v); tool calls will not be audited", err)
		}
	}
	st.mcpSrv = ms
	return ms
}

// mcpServerVersion is the version the MCP server reports in
// server_info: the server's release version, "0.1.0" when none
// is configured (the same fallback GET /openapi.json uses).
func mcpServerVersion(s *Server) string {
	if s.version != "" {
		return s.version
	}
	return "0.1.0"
}

// ---------------------------------------------------------------------------
// Auth + identity (the security-critical part).
// ---------------------------------------------------------------------------

// validMCPGrant reports whether scope is a grant the transport may
// honour: a known mcp: scope, or a trailing-* wildcard whose prefix
// covers at least one known scope (mcp:read:*, mcp:manage:*,
// mcp:*). A bare "*" is NEVER valid here — it is the
// local-operator-only grant (see mcp.LocalOperator) and must never
// reach a network identity. (Careful: "*" ends in "*", so the
// wildcard branch below would compute an empty prefix, which
// strings.HasPrefix matches against everything — the bare "*"
// is rejected explicitly first.) A mid-string "*" (mcp:*:status)
// is literal per mcp.ScopeMatches and matches nothing, so it is
// rejected too, rather than carried as a useless grant.
func validMCPGrant(scope string) bool {
	if scope == "" || scope == "*" {
		return false
	}
	if mcp.IsKnownScope(scope) {
		return true
	}
	if strings.HasSuffix(scope, "*") {
		prefix := scope[:len(scope)-1]
		for _, known := range mcp.AllScopes() {
			if strings.HasPrefix(known, prefix) {
				return true
			}
		}
	}
	return false
}

// mcpCallerKeyID returns the stable per-key owner id for the
// request: the gateway-key id when the verifier names it, else a
// derived "key-"+sha256[:12] id (the same shape
// StaticKeyVerifier.KeyID uses). "" when no Bearer token is
// present. The id is never the key itself, never logged, and only
// used as the session owner, the rate-limit key, and the audit
// caller id.
func (s *Server) mcpCallerKeyID(r *http.Request) string {
	raw := bearerKey(r)
	if raw == "" {
		return ""
	}
	if id := s.callerKeyID(r); id != "" {
		return id
	}
	sum := sha256.Sum256([]byte(raw))
	return "key-" + hex.EncodeToString(sum[:])[:12]
}

// mcpRequireKey refuses keyless requests with 401 and returns the
// caller's key id. The guard table already requires auth on these
// routes, but auth-relaxed mode would pass a keyless request
// through — MCP must never be keyless (the MUST NEVER list), so
// the handlers enforce key presence themselves.
func (s *Server) mcpRequireKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	keyID := s.mcpCallerKeyID(r)
	if keyID == "" {
		s.unauthorized(w, r)
		return "", false
	}
	return keyID, true
}

// mcpIdentityForRequest builds the per-request MCP identity from
// the authenticated gateway key: GatewayKeyClient(keyID,
// validated mcp: scopes ONLY). The request is REJECTED with 403
// when the key carries any scope that is not a valid MCP grant —
// a bare "*" or any HTTP-namespace scope (admin, chat, models).
// The HTTP guard treats "admin" as implying every scope, so
// passing HTTP scopes through would escalate a chat/admin key
// into MCP tools (M9.1 SECURITY REVIEW N4). An empty scope list
// is allowed: the identity holds no scopes and every tool call
// is denied -32002 by the MCP layer. A verifier that cannot name
// scopes (static keys in tests) yields no scopes, so its keys
// can initialise and list tools but call none — fail-closed.
func (s *Server) mcpIdentityForRequest(w http.ResponseWriter, r *http.Request, keyID string) (mcp.ClientIdentity, bool) {
	var scopes []string
	if sv, ok := s.verifier.(ScopeVerifier); ok {
		if got, ok2 := sv.KeyScopes(r.Context(), bearerKey(r)); ok2 {
			scopes = got
		}
	}
	for _, sc := range scopes {
		if !validMCPGrant(sc) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("gateway key carries scope %q, which is not a valid MCP scope: the MCP transport accepts only mcp: scopes", sc))
			return mcp.ClientIdentity{}, false
		}
	}
	return mcp.GatewayKeyClient(keyID, scopes), true
}

// mcpClientIP returns the peer IP for the per-IP rate bucket:
// the real TCP peer (RemoteAddr), never X-Forwarded-For.
func mcpClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return strings.TrimSpace(host)
}

// mcpCheckRateLimit applies the transport rate limit: per-IP
// bucket first, then per-key. A trip writes 429 with Retry-After
// and type mcp_rate_limit and returns false.
func (s *Server) mcpCheckRateLimit(w http.ResponseWriter, r *http.Request, keyID string) bool {
	st := mcpHTTPStateFor(s)
	now := st.rate.nowTime()
	if !st.rate.allowIP(mcpClientIP(r), now) {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Message: "MCP transport per-IP rate limit exceeded (120 requests burst, 300/min sustained); retry after 1 second(s)", Type: "mcp_rate_limit"}})
		return false
	}
	if keyID != "" && !st.rate.allowKey(keyID, now) {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Message: "MCP transport per-key rate limit exceeded (30 requests burst, 60/min sustained); retry after 1 second(s)", Type: "mcp_rate_limit"}})
		return false
	}
	return true
}

// mcpTransportGate is the shared front of every transport route:
// key required (401), transport enabled (503), identity valid
// (403), rate limit (429) — in that order, so a disabled
// transport always answers 503 and a bad-scope key always
// answers 403 regardless of limiter state. DELETE skips the
// toggle (a disabled transport's sessions must still be
// terminable) — pass checkToggle=false for it.
func (s *Server) mcpTransportGate(w http.ResponseWriter, r *http.Request, kind mcpTransportKind, checkToggle bool) (string, mcp.ClientIdentity, bool) {
	keyID, ok := s.mcpRequireKey(w, r)
	if !ok {
		return "", mcp.ClientIdentity{}, false
	}
	st := mcpHTTPStateFor(s)
	if checkToggle && !st.transportEnabled(kind) {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("MCP %s transport is disabled (enable it with POST /api/mcp/status)", kind))
		return "", mcp.ClientIdentity{}, false
	}
	ident, ok := s.mcpIdentityForRequest(w, r, keyID)
	if !ok {
		return "", mcp.ClientIdentity{}, false
	}
	if !s.mcpCheckRateLimit(w, r, keyID) {
		return "", mcp.ClientIdentity{}, false
	}
	return keyID, ident, true
}

// readMCPBody reads one JSON-RPC message body, capped at
// mcpMaxMessageBytes. Over-cap is a 413.
func readMCPBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxMessageBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read request body")
		return nil, false
	}
	if len(body) > mcpMaxMessageBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "MCP message exceeds 1 MiB")
		return nil, false
	}
	return body, true
}

// isInitializeMessage reports whether a JSON-RPC body is a
// single initialize request object. Batches (a JSON array body)
// are NOT supported — one JSON-RPC message per POST — so an
// array never opens a session, even when one of its members is
// an initialize: the session is only created once the message
// parses as a single object, so a malformed batch can never
// leave an orphan session behind.
func isInitializeMessage(body []byte) bool {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return false
	}
	var obj struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	return obj.Method == "initialize"
}

// writeMCPStreamableError writes a JSON-RPC error envelope for a
// transport-level failure on the Streamable transport (unknown
// session, missing session id): HTTP status + code -32000, id
// null — the shape spec-compliant clients parse.
func writeMCPStreamableError(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": code, "message": msg},
		"id":      nil,
	})
}

// ---------------------------------------------------------------------------
// SSE transport: GET /api/mcp/sse + POST /api/mcp/sse.
// ---------------------------------------------------------------------------

// handleMCPSSE dispatches the SSE transport by method.
func (s *Server) handleMCPSSE(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleMCPSSEStream(w, r)
	case http.MethodPost:
		s.handleMCPSSEPost(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleMCPSSEStream serves GET /api/mcp/sse: it creates a
// session, opens the server→client event stream, and streams
// queued responses until the client disconnects (which terminates
// the session — a reconnecting client GETs again for a fresh
// session, the same reset-on-initialize behaviour the reference router's SSE
// singleton has). The first event is `endpoint` with the POST URL
// carrying the session id; responses follow as `message` events.
func (s *Server) handleMCPSSEStream(w http.ResponseWriter, r *http.Request) {
	keyID, _, ok := s.mcpTransportGate(w, r, mcpTransportSSE, true)
	if !ok {
		return
	}
	st := mcpHTTPStateFor(s)
	st.mu.Lock()
	sess, err := st.newSessionLocked(mcpTransportSSE, keyID)
	if err != nil {
		st.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	sess.streams++
	st.mu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		st.mu.Lock()
		sess.streams--
		st.mu.Unlock()
		st.terminate(mcpTransportSSE, sess.id)
		writeError(w, http.StatusInternalServerError, "MCP SSE transport requires a streaming response")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "event: endpoint\ndata: /api/mcp/sse?session_id=%s\n\n", sess.id)
	flusher.Flush()

	heartbeat := time.NewTicker(mcpHeartbeatInterval)
	defer func() {
		heartbeat.Stop()
		st.mu.Lock()
		sess.streams--
		st.mu.Unlock()
		// Disconnect ends the SSE session: its only response
		// channel was this stream.
		st.terminate(mcpTransportSSE, sess.id)
	}()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sess.done:
			return
		case msg, ok := <-sess.out:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			flusher.Flush()
			st.touch(sess)
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// handleMCPSSEPost serves POST /api/mcp/sse: one JSON-RPC
// message for a session opened by GET. The response (when the
// message is not a notification) is queued onto the session's
// GET stream; the POST itself answers 202. A POST before GET is
// a 404: the session does not exist until GET opens it.
func (s *Server) handleMCPSSEPost(w http.ResponseWriter, r *http.Request) {
	keyID, ident, ok := s.mcpTransportGate(w, r, mcpTransportSSE, true)
	if !ok {
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "missing session_id query parameter — GET /api/mcp/sse first to open a stream")
		return
	}
	st := mcpHTTPStateFor(s)
	st.mu.Lock()
	sess := st.sessionLocked(mcpTransportSSE, sessionID)
	st.mu.Unlock()
	if sess == nil {
		writeError(w, http.StatusNotFound, "unknown MCP SSE session — GET /api/mcp/sse first to open a stream")
		return
	}
	if sess.keyID != keyID {
		// Foreign session: same 404, no leak.
		writeError(w, http.StatusNotFound, "unknown MCP SSE session — GET /api/mcp/sse first to open a stream")
		return
	}
	body, ok := readMCPBody(w, r)
	if !ok {
		return
	}
	// Bind this session's combo state into the request context
	// (H1): the MCP server binds the session into the Source
	// for this call, so switch_combos / explain_route see this
	// session's selection and no other session's.
	reqCtx := mcp.ContextWithComboState(r.Context(), sess)
	resp := st.mcpServer().HandleWithIdentity(reqCtx, body, ident)
	st.touch(sess)
	if resp != nil {
		select {
		case sess.out <- resp:
		default:
			// The client is not reading its stream: drop the
			// response rather than blocking the POST handler
			// (and the client) behind a dead stream. Logged
			// so a dropped response is visible, not silent.
			s.logger.Printf("mcp: SSE session %s outbox full, dropping a response for key %s", sess.id, sess.keyID)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// ---------------------------------------------------------------------------
// Streamable HTTP transport: POST/GET /api/mcp/stream,
// DELETE /api/mcp/stream/{id}.
// ---------------------------------------------------------------------------

// handleMCPStream dispatches the Streamable transport by method.
func (s *Server) handleMCPStream(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleMCPStreamPost(w, r)
	case http.MethodGet:
		s.handleMCPStreamGet(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleMCPStreamPost serves POST /api/mcp/stream: one JSON-RPC
// message. An initialize with no Mcp-Session-Id opens a session
// (the id returns in the Mcp-Session-Id response header); later
// messages address it by that header. An unknown id is a 404 —
// except an initialize with a stale id, which opens a fresh
// session (auto-recovery after a restart or idle eviction). A
// request answers 200 application/json directly; a notification
// answers 202 with no body.
func (s *Server) handleMCPStreamPost(w http.ResponseWriter, r *http.Request) {
	keyID, ident, ok := s.mcpTransportGate(w, r, mcpTransportStreamable, true)
	if !ok {
		return
	}
	body, ok := readMCPBody(w, r)
	if !ok {
		return
	}
	st := mcpHTTPStateFor(s)
	sessionID := r.Header.Get("Mcp-Session-Id")
	var sess *mcpHTTPSession
	if sessionID != "" {
		st.mu.Lock()
		sess = st.sessionLocked(mcpTransportStreamable, sessionID)
		st.mu.Unlock()
		if sess != nil && sess.keyID != keyID {
			// Foreign session: treat as unknown (no leak).
			sess = nil
		}
		if sess == nil {
			if !isInitializeMessage(body) {
				writeMCPStreamableError(w, http.StatusNotFound, mcpUnknownSessionCode, "unknown session: no session with this Mcp-Session-Id (initialize to open one)")
				return
			}
			st.mu.Lock()
			var err error
			sess, err = st.newSessionLocked(mcpTransportStreamable, keyID)
			st.mu.Unlock()
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			sessionID = sess.id
		}
	} else {
		if !isInitializeMessage(body) {
			writeMCPStreamableError(w, http.StatusBadRequest, mcpUnknownSessionCode, "Mcp-Session-Id header is required (initialize to open a session)")
			return
		}
		st.mu.Lock()
		var err error
		sess, err = st.newSessionLocked(mcpTransportStreamable, keyID)
		st.mu.Unlock()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		sessionID = sess.id
	}
	// Bind this session's combo state into the request context
	// (H1), as the SSE POST does above: switch_combos /
	// explain_route see this session's selection only.
	resp := st.mcpServer().HandleWithIdentity(mcp.ContextWithComboState(r.Context(), sess), body, ident)
	st.touch(sess)
	w.Header().Set("Mcp-Session-Id", sessionID)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

// handleMCPStreamGet serves GET /api/mcp/stream: the
// server→client SSE stream for a session. The stream carries
// queued messages (none are server-initiated today, so it mostly
// idles on heartbeats) and closing it does NOT terminate the
// session — the session lives until DELETE or idle eviction.
func (s *Server) handleMCPStreamGet(w http.ResponseWriter, r *http.Request) {
	keyID, _, ok := s.mcpTransportGate(w, r, mcpTransportStreamable, true)
	if !ok {
		return
	}
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "Mcp-Session-Id header is required")
		return
	}
	st := mcpHTTPStateFor(s)
	st.mu.Lock()
	sess := st.sessionLocked(mcpTransportStreamable, sessionID)
	if sess != nil && sess.keyID != keyID {
		sess = nil // foreign session: same 404, no leak
	}
	if sess == nil {
		st.mu.Unlock()
		writeError(w, http.StatusNotFound, "unknown MCP session")
		return
	}
	// One GET stream per session (H2): a second concurrent GET
	// is refused with 409 instead of holding another goroutine
	// + connection for a stream the sweep will never evict.
	// The client closes the first stream (disconnecting it)
	// and retries; the count drops in the deferred decrement
	// below, so the refusal lifts as soon as the stream ends.
	if sess.streams >= mcpMaxStreamsPerSession {
		st.mu.Unlock()
		writeError(w, http.StatusConflict, "MCP session already has an open server-to-client stream (one stream per session); close the existing stream before opening another")
		return
	}
	sess.streams++
	st.mu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		st.mu.Lock()
		sess.streams--
		st.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "MCP Streamable transport requires a streaming response")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(mcpHeartbeatInterval)
	defer func() {
		heartbeat.Stop()
		st.mu.Lock()
		sess.streams--
		st.mu.Unlock()
		st.touch(sess)
	}()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sess.done:
			return
		case msg, ok := <-sess.out:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			flusher.Flush()
			st.touch(sess)
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// handleMCPStreamDelete serves DELETE /api/mcp/stream/{id}: it
// terminates one session (204). Unknown or foreign ids are 404.
// The transport toggle is NOT consulted here: a disabled
// transport's sessions must still be terminable.
func (s *Server) handleMCPStreamDelete(w http.ResponseWriter, r *http.Request) {
	keyID, _, ok := s.mcpTransportGate(w, r, mcpTransportStreamable, false)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}
	st := mcpHTTPStateFor(s)
	st.mu.Lock()
	sess := st.sessionLocked(mcpTransportStreamable, id)
	if sess != nil && sess.keyID != keyID {
		sess = nil // foreign session: same 404, no leak
	}
	if sess == nil {
		st.mu.Unlock()
		writeError(w, http.StatusNotFound, "unknown MCP session")
		return
	}
	st.terminateLocked(st.streamSessions, id)
	st.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Serve-mode Source: the live state behind the MCP tools.
// ---------------------------------------------------------------------------

// serverMCPSource adapts the serve-mode HTTP server to the MCP
// Source interface: every tool call reads live server state, never
// canned responses. The ledger-backed methods (ComboMetrics,
// ModelStats, CostReport, CheckQuota, SetBudgetGuard) use the
// serve process's own live ledger (s.ledger — the same handle the
// analytics and budget endpoints use), so they answer from real
// usage the moment a ledger is wired; with no ledger they fail
// honestly (ModelStats, which has no error return, answers an
// empty list and reports ModelStatsAvailable() false, so the
// pick_fastest_model handler can say the backend is unavailable
// instead of "no measured latency yet"). ExplainRoute and
// SimulateRoute dry-run from the live provider feed (registry
// order) and the price book. SetRoutingStrategy persists through
// the same routingPersist the dashboard strategy endpoint uses,
// persist-first, then applies to the engine when it reports
// strategies. CreateCombo creates through ComboStore.Create
// (see its comment). KEY-ID RULE (M9.2 N3): the key_id arriving at CostReport /
// CheckQuota / SetBudgetGuard has already been forced to the
// authenticated caller identity by the cost handlers
// (internal/mcp/tools_cost.go resolveCostKeyID) — a gateway key
// can only ever act on its own key. Never call these methods
// with a caller-supplied key id from a new path without the
// same check.
type serverMCPSource struct {
	srv *Server
	st  *mcpHTTPState
	// ctx is the request context bound by WithContext for one
	// tools/call (H6): the transport's request context, already
	// carrying the MCP tool-call timeout from
	// handleWithIdentity. Every backing-store call below uses
	// reqCtx(), never context.Background(), so a client
	// disconnect or the timeout cancels a slow ledger,
	// registry, combo-persist or cache call. Nil on the
	// unbound Source the mcpServer() builder creates — a direct
	// source-level call with no request — where reqCtx falls
	// back to context.Background(), the old behaviour.
	ctx context.Context
	// combo is the per-session active-combo state bound by
	// WithContext from the request context (H1): the session
	// that made this call. Nil when the call carries no
	// session, where ActiveCombo/SetActiveCombo fall back to
	// the process-wide st.activeCombo.
	combo mcp.ComboState
}

// WithContext implements mcp.ContextSource: it returns a copy of
// this Source bound to one request's context (and the session
// combo state that context carries), leaving the shared Source
// untouched — concurrent tool calls on different sessions each
// get their own bound copy, so no request can see another's
// context or combo selection. The bound context is also the
// fallback CreateCombo uses when a direct Source-level call
// passes a nil ctx (CreateCombo review nit 1: the interface now
// carries the caller's ctx as a parameter, so the ctx.Err() →
// 503 guard fires on both paths instead of only the HTTP one).
func (x *serverMCPSource) WithContext(ctx context.Context) mcp.Source {
	if ctx == nil {
		ctx = context.Background()
	}
	cp := *x
	cp.ctx = ctx
	cp.combo = mcp.ComboStateFromContext(ctx)
	return &cp
}

// reqCtx returns the bound request context, or
// context.Background() when this Source is unbound.
func (x *serverMCPSource) reqCtx() context.Context {
	if x.ctx != nil {
		return x.ctx
	}
	return context.Background()
}

// ledgerOrUnavailable returns the serve process's live ledger, or
// an honest unavailable error when none is wired. Reads never
// open or create a ledger file: a missing ledger means "no usage
// recorded yet", and the tool says so.
func (x *serverMCPSource) ledgerOrUnavailable(method string) (*ledger.Ledger, error) {
	if x.srv.ledger == nil {
		return nil, errMCPNotAvailable(method, "no ledger wired in this serve process")
	}
	return x.srv.ledger, nil
}

// promptUSDPer1M returns the prompt rate per 1M tokens for one
// (provider, model) pair from the live ledger price map (the same
// map the ledger charges with — production serve mode sets it at
// startup from the price book + registry), falling back to the
// operator price book when the map was never wired. Unknown is
// nil — never a guessed rate, never $0.
func (x *serverMCPSource) promptUSDPer1M(provider, model string) (*float64, bool) {
	if prices := x.srv.getLedgerPrices(); prices != nil && prices.Known(provider, model) {
		rate := prices.Cost(provider, model, 1000000, 0)
		return &rate, true
	}
	if x.srv.priceBook != nil {
		if p, ok := x.srv.priceBook.Lookup(provider, model); ok && p.InputUSDPer1M != nil {
			rate := *p.InputUSDPer1M
			return &rate, true
		}
	}
	return nil, false
}

// errMCPNotAvailable is the plain error the unimplemented Source
// methods return: the caller asked for something real, the
// backing store is unavailable — not an invalid input.
func errMCPNotAvailable(method, reason string) error {
	return fmt.Errorf("mcp: %s is not available over the MCP HTTP transport yet — %s", method, reason)
}

func (x *serverMCPSource) Providers() []mcp.ProviderInfo {
	s := x.srv
	if s.dashboardProviders == nil {
		return []mcp.ProviderInfo{}
	}
	// Model ids per provider, from the live registry (the
	// dashboard lister only carries a count, not ids).
	byProvider := map[string][]string{}
	if s.registry != nil {
		if models, err := s.registry.Models(x.reqCtx()); err == nil {
			for _, m := range models {
				pid := m.ProviderID
				if pid == "" {
					pid = m.OwnedBy
				}
				byProvider[pid] = append(byProvider[pid], m.ID)
			}
		}
	}
	out := []mcp.ProviderInfo{}
	for _, p := range s.dashboardProviders.DashboardProviders() {
		models := byProvider[p.ID]
		if models == nil {
			models = []string{}
		}
		out = append(out, mcp.ProviderInfo{
			ID: p.ID, Name: p.Name, WireFormat: p.WireFormat,
			Enabled: p.Enabled, Models: models,
			CatalogID: p.CatalogID, Verification: p.Verification,
		})
	}
	return out
}

func (x *serverMCPSource) Models() []mcp.ModelInfo {
	s := x.srv
	seen := map[string]bool{}
	out := []mcp.ModelInfo{}
	if s.registry != nil {
		if models, err := s.registry.Models(x.reqCtx()); err == nil {
			for _, m := range models {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				out = append(out, mcp.ModelInfo{ID: m.ID, ProviderID: m.ProviderID, ProviderName: m.OwnedBy})
			}
		}
	} else {
		// No registry: the WithModels list has ids only, no
		// provider — reported with empty provider fields,
		// never a guessed one.
		for _, id := range s.models {
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, mcp.ModelInfo{ID: id})
		}
	}
	// Virtual auto/* models, when the auto-router can resolve
	// them — the same rule the stdio Source uses.
	if s.autoRouter != nil {
		for _, v := range autorouter.VirtualModels {
			if seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, mcp.ModelInfo{ID: v, ProviderID: "auto-router", ProviderName: "auto-router"})
		}
	}
	return out
}

func (x *serverMCPSource) Status() mcp.StatusInfo {
	s := x.srv
	st := mcp.StatusInfo{Version: mcpServerVersion(s), UptimeSeconds: int64(time.Since(s.startedAt).Seconds())}
	st.Listen = s.listenAddr
	if st.Listen == "" {
		st.Listen = "unknown"
	}
	if s.cfg != nil {
		st.GatewayKeys = len(s.cfg.GatewayKeys)
	}
	st.AutoRouter = s.autoRouter != nil
	providers := x.Providers()
	st.Providers = len(providers)
	for _, p := range providers {
		if p.Enabled {
			st.EnabledProviders++
		}
		st.Models += len(p.Models)
	}
	return st
}

// comboProviderByID finds a configured provider by id, for the
// usability check (same rule as /v1/combos: provider exists,
// enabled, has a key or is a no-auth local).
func (x *serverMCPSource) comboProviderByID(id string) (config.ProviderConfig, bool) {
	if x.srv.cfg == nil {
		return config.ProviderConfig{}, false
	}
	for _, p := range x.srv.cfg.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return config.ProviderConfig{}, false
}

// Combos lists the config routing groups as MCP combos, with the
// strategy from routing.chain_strategies ("priority" when none is
// configured) and usability per member (same comboStepFacts rule
// as /v1/combos). Active is the session-active combo.
func (x *serverMCPSource) Combos() []mcp.ComboInfo {
	s := x.srv
	if s.cfg == nil || s.cfg.Routing == nil {
		return []mcp.ComboInfo{}
	}
	global := s.cfg.Routing.GlobalStrategy
	if global == "" {
		global = "priority"
	}
	// Active is this caller's session-active combo (H1): the
	// bound session's selection when this Source is bound to a
	// request, else the process-wide fallback.
	active := x.ActiveCombo()
	names := make([]string, 0, len(s.cfg.Routing.Groups))
	for name := range s.cfg.Routing.Groups {
		names = append(names, name)
	}
	sort.Strings(names)
	out := []mcp.ComboInfo{}
	for _, name := range names {
		members := append([]string(nil), s.cfg.Routing.Groups[name]...)
		if members == nil {
			members = []string{}
		}
		strategyName := global
		if st, ok := s.cfg.Routing.ChainStrategies[name]; ok && st != "" {
			strategyName = st
		}
		usable := true
		reason := ""
		if len(members) == 0 {
			usable = false
			reason = "combo has no members"
		}
		for _, m := range members {
			// Members are "provider[@effort]".
			pid := m
			if at := strings.LastIndex(pid, "@"); at >= 0 {
				pid = pid[:at]
			}
			p, ok := x.comboProviderByID(pid)
			if !ok {
				usable = false
				reason = fmt.Sprintf("unknown provider %q", pid)
				break
			}
			_, stepUsable, stepReason := comboStepFacts(p)
			if !stepUsable {
				usable = false
				reason = fmt.Sprintf("provider %q: %s", pid, stepReason)
				break
			}
		}
		out = append(out, mcp.ComboInfo{
			Name: name, Members: members, Strategy: strategyName,
			Active: name == active, Usable: usable, UnusableReason: reason,
		})
	}
	return out
}

// CreateCombo creates a combo through ComboStore.Create —
// the same validate + mirror + persist-with-rollback path POST
// /v1/combos runs, so the MCP tool and the REST handler share
// one implementation and can never drift. Members are
// "provider[/model][@effort]" strings, exactly the chain
// entries the REST API accepts. Combos carry no per-key owner:
// like the HTTP handler, the create is scoped by the caller's
// mcp:manage:routing grant (enforced by the tool dispatcher
// before this runs), not by a key id — the M9.2 key-id rule
// covers the cost handlers only. Validation failures and
// duplicate names are invalid input; a persist failure is a
// backing-store failure. ctx is the caller's context: it is
// passed straight to ComboStore.Create, whose ctx.Err() → 503
// guard aborts a cancelled call before any validation or
// persist. A nil ctx (a direct Source-level call) falls back
// to the bound request context, or Background when unbound.
func (x *serverMCPSource) CreateCombo(ctx context.Context, name string, members []string, strategy string) error {
	// The caller's ctx wins; nil falls back to reqCtx()
	// (see the doc comment above).
	if ctx == nil {
		ctx = x.reqCtx()
	}
	chain := make([]json.RawMessage, 0, len(members))
	for _, m := range members {
		raw, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("%w: members: cannot encode member %q", mcp.ErrInvalidInput, m)
		}
		chain = append(chain, raw)
	}
	_, err := x.srv.combosStore().Create(ctx, ComboCreateRequest{Name: name, Chain: chain, Strategy: strategy})
	if err != nil {
		var ce *comboError
		if errors.As(err, &ce) && ce.status != http.StatusInternalServerError {
			return fmt.Errorf("%w: %s", mcp.ErrInvalidInput, ce.Error())
		}
		return err
	}
	return nil
}

// ActiveCombo returns the session-active combo ("" = none):
// the bound session's selection when this Source is bound to a
// request (H1), else the process-wide fallback.
func (x *serverMCPSource) ActiveCombo() string {
	if x.combo != nil {
		return x.combo.ActiveCombo()
	}
	x.st.mu.Lock()
	defer x.st.mu.Unlock()
	return x.st.activeCombo
}

// SetActiveCombo selects the default combo routing tools report.
// It never changes what the serving router routes. Unknown names
// are invalid input. "" clears the selection — the same rule
// the stdio Source follows, and the value switch_combos passes
// to clear (active:false). The selection lands on the bound
// session when this Source is bound to a request (H1), so one
// key's switch never moves another session's default; the
// unbound Source writes the process-wide fallback instead.
func (x *serverMCPSource) SetActiveCombo(name string) error {
	if name == "" {
		x.storeActiveCombo("")
		return nil
	}
	for _, c := range x.Combos() {
		if c.Name == name {
			x.storeActiveCombo(name)
			return nil
		}
	}
	return fmt.Errorf("%w: unknown combo %q", mcp.ErrInvalidInput, name)
}

// storeActiveCombo writes the selection to the bound session
// when there is one, else to the process-wide fallback.
func (x *serverMCPSource) storeActiveCombo(name string) {
	if x.combo != nil {
		x.combo.SetActiveCombo(name)
		return
	}
	x.st.mu.Lock()
	x.st.activeCombo = name
	x.st.mu.Unlock()
}

// ComboMetrics aggregates the live ledger's all-time summary
// over one combo's member providers — the same aggregation the
// stdio Source runs. Cost is priced rows only; any token on an
// unpriced (provider, model) pair marks the cost UNKNOWN
// (CostKnown false) with the tokens and models listed. Mean
// latency comes from the most recent successful events only — a
// combo with no such events reports LatencyKnown false, never a
// 0 ms mean. Unknown combo is invalid input; no ledger is an
// honest unavailable error.
func (x *serverMCPSource) ComboMetrics(name string) (mcp.ComboMetrics, error) {
	var zero mcp.ComboMetrics
	if x.srv.cfg == nil {
		return zero, errMCPNotAvailable("combo_metrics", "no config loaded")
	}
	groups := x.srv.cfg.GroupsSnapshot()
	members, ok := groups[name]
	if !ok {
		return zero, fmt.Errorf("%w: unknown combo %q", mcp.ErrInvalidInput, name)
	}
	led, err := x.ledgerOrUnavailable("combo_metrics")
	if err != nil {
		return zero, err
	}
	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		provID, _, err := config.ParseGroupMember(m)
		if err != nil {
			continue
		}
		memberSet[provID] = true
	}
	ctx := x.reqCtx()
	sum, err := led.Summary(ctx, time.Time{}, time.Time{})
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger summary: %w", err)
	}
	out := mcp.ComboMetrics{Name: name, CostKnown: true, Window: "all_time"}
	for _, p := range sum.ByProvider {
		if !memberSet[p.Provider] {
			continue
		}
		out.Requests += p.Requests
		out.OK += p.OK
		out.Errors += p.Errors
		out.PromptTokens += p.PromptTokens
		out.CompletionTokens += p.CompletionTokens
		out.TotalTokens += p.TotalTokens
		out.CostUSD += p.CostUSD
	}
	unpriced := make(map[string]bool)
	for _, m := range sum.ByModel {
		if !memberSet[m.Provider] {
			continue
		}
		if !x.PriceKnown(m.Provider, m.Model) && m.TotalTokens > 0 {
			out.CostKnown = false
			out.UnpricedTokens += m.TotalTokens
			unpriced[m.Provider+"/"+m.Model] = true
		}
	}
	for model := range unpriced {
		out.UnpricedModels = append(out.UnpricedModels, model)
	}
	sort.Strings(out.UnpricedModels)
	if out.Requests > 0 {
		rate := float64(out.OK) / float64(out.Requests)
		out.SuccessRate = &rate
	}
	events, err := led.RecentEvents(ctx, 500, ledger.LogAll)
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger recent events: %w", err)
	}
	var latSum int64
	for _, ev := range events {
		if !memberSet[ev.Provider] || !ev.OK || ev.LatencyMS <= 0 {
			continue
		}
		latSum += ev.LatencyMS
		out.LatencyEvents++
	}
	if out.LatencyEvents > 0 {
		out.MeanLatencyMS = float64(latSum) / float64(out.LatencyEvents)
		out.LatencyKnown = true
	}
	return out, nil
}

// ComboMetricsForKey implements a2a.KeyScopedComboMetricsSource:
// one combo's measured ledger history for ONE gateway key only —
// the per-key metrics skillSmartRouting serves a gateway-key
// caller, so that caller never sees another key's usage. It
// returns ONLY this key's rows for this combo's member
// providers, never ledger-wide numbers and never another key's
// numbers. Unknown combo and empty key id are invalid input;
// no ledger is an honest unavailable error. Validation order
// (documented, not ComboMetrics' order): empty key id first,
// then config, then unknown combo, then the ledger —
// ComboMetrics has no key id and checks config first.
//
// HOW IT IS SOURCED (and why it is sampled, not all-time): the
// ledger has NO per-key per-provider aggregate. Summary() gives
// ByKey (per-key totals across ALL providers — no provider
// split) and ByProvider/ByModel (ledger-wide — no key split),
// and UsageSlices(key) is per-key per-model but successful,
// non-shared rows ONLY, so it carries no failures and therefore
// no error count and no success rate. The only ledger read
// returning per-row gateway_key_id values is RecentEvents, so
// this method filters the 500 most recent ledger events
// (ledger.LogAll) to ev.GatewayKeyID == keyID and a member
// provider, exactly the sampling precedent the per-key
// CostReport in this file already follows. Every figure —
// requests, ok, errors, tokens, cost, success rate, mean
// latency — comes from that one sample, so the figures are
// mutually consistent within it.
//
// HONEST LIMITS (read before treating the result as all-time):
//   - Window is "recent_500_events", never "all_time": usage
//     older than the 500 most recent ledger-wide events is
//     invisible, and a busy other key can push this key's rows
//     out of the sample entirely — a key with older combo
//     history then reads as no history (Requests 0), not as a
//     measured zero. An all-time per-key combo query needs a
//     new ledger aggregate (a GROUP BY gateway_key_id,
//     provider over usage_events); that is a ledger change,
//     outside this slice.
//   - Errors count ok=0, non-cancelled rows only, and the
//     success rate is OK/Requests over the sample — the same
//     counting rules ComboMetrics applies to the ledger-wide
//     Summary (a cancelled hedge loser is a request, never a
//     failure). Cost is the row's stored cost_usd summed
//     unconditionally — the same rule as ledger-wide
//     ComboMetrics — so a token-bearing failure DOES
//     contribute its stored cost (the Isolation test: key-2's
//     two failed p2/c rows add 2 x $0.0005 to its $0.026
//     total); a priced failure with no reported cost stores 0
//     (see normalizeUsage), so nothing is ever invented.
//   - Unpriced detection uses the LIVE price book
//     (PriceKnown), the same rule ComboMetrics uses: a model
//     priced now but unpriced when the row was recorded reads
//     as priced, and vice versa. A non-member provider's rows
//     are excluded BEFORE the unpriced check, so another
//     combo's unpriced usage can never flip CostKnown here.
//   - Latency is the mean over sampled successful events with
//     a recorded latency (> 0 ms) — the same rule as
//     ComboMetrics, which also reads RecentEvents(500).
//     LatencyKnown is false when the sample holds none.
func (x *serverMCPSource) ComboMetricsForKey(keyID, name string) (mcp.ComboMetrics, error) {
	var zero mcp.ComboMetrics
	// Normalise before comparing: a padded key id (" key-1 ")
	// must match that key's rows, not silently return zero
	// metrics for a key that does exist.
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return zero, fmt.Errorf("%w: key_id must not be empty", mcp.ErrInvalidInput)
	}
	if x.srv.cfg == nil {
		return zero, errMCPNotAvailable("combo_metrics_for_key", "no config loaded")
	}
	groups := x.srv.cfg.GroupsSnapshot()
	members, ok := groups[name]
	if !ok {
		return zero, fmt.Errorf("%w: unknown combo %q", mcp.ErrInvalidInput, name)
	}
	led, err := x.ledgerOrUnavailable("combo_metrics_for_key")
	if err != nil {
		return zero, err
	}
	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		provID, _, err := config.ParseGroupMember(m)
		if err != nil {
			continue
		}
		memberSet[provID] = true
	}
	ctx := x.reqCtx()
	events, err := led.RecentEvents(ctx, 500, ledger.LogAll)
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger recent events: %w", err)
	}
	out := mcp.ComboMetrics{Name: name, CostKnown: true, Window: "recent_500_events"}
	// Total tokens per (provider, model) in the sample, for
	// the unpriced check below (same rule as ComboMetrics).
	tokensByModel := make(map[ledger.PriceKey]int)
	var latSum int64
	for _, ev := range events {
		if ev.GatewayKeyID != keyID || !memberSet[ev.Provider] {
			continue
		}
		out.Requests++
		if ev.OK {
			out.OK++
		} else if !ev.Cancelled {
			out.Errors++
		}
		out.PromptTokens += ev.PromptTokens
		out.CompletionTokens += ev.CompletionTokens
		out.TotalTokens += ev.TotalTokens
		out.CostUSD += ev.CostUSD
		k := ledger.PriceKey{Provider: ev.Provider, Model: ev.Model}
		tokensByModel[k] += ev.TotalTokens
		if ev.OK && ev.LatencyMS > 0 {
			latSum += ev.LatencyMS
			out.LatencyEvents++
		}
	}
	unpriced := make(map[string]bool)
	for k, tokens := range tokensByModel {
		if tokens > 0 && !x.PriceKnown(k.Provider, k.Model) {
			out.CostKnown = false
			out.UnpricedTokens += tokens
			unpriced[k.Provider+"/"+k.Model] = true
		}
	}
	for model := range unpriced {
		out.UnpricedModels = append(out.UnpricedModels, model)
	}
	sort.Strings(out.UnpricedModels)
	if out.Requests > 0 {
		rate := float64(out.OK) / float64(out.Requests)
		out.SuccessRate = &rate
	}
	if out.LatencyEvents > 0 {
		out.MeanLatencyMS = float64(latSum) / float64(out.LatencyEvents)
		out.LatencyKnown = true
	}
	return out, nil
}

// RoutingStrategy returns the effective strategy for a chain
// ("" = the global strategy) from the live routing config: the
// empty global strategy means "priority" (M1.11), and a chain
// with no override inherits the global.
func (x *serverMCPSource) RoutingStrategy(chain string) mcp.StrategyInfo {
	s := x.srv
	global := "priority"
	if s.cfg != nil && s.cfg.Routing != nil && s.cfg.Routing.GlobalStrategy != "" {
		global = s.cfg.Routing.GlobalStrategy
	}
	rule := func(name string) string {
		_, r := strategy.Describe(strategy.Strategy(name))
		return r
	}
	if chain == "" {
		return mcp.StrategyInfo{Name: global, Rule: rule(global), Scope: "global"}
	}
	if s.cfg != nil && s.cfg.Routing != nil {
		if st, ok := s.cfg.Routing.ChainStrategies[chain]; ok && st != "" {
			return mcp.StrategyInfo{Name: st, Rule: rule(st), Scope: "chain", Chain: chain}
		}
	}
	return mcp.StrategyInfo{Name: global, Rule: rule(global), Scope: "global", Chain: chain, Inherited: true}
}

// SetRoutingStrategy writes the global strategy (chain == "") or
// one chain's strategy through the same routingPersist the
// dashboard strategy endpoint uses — persist FIRST, so a failed
// save leaves routing unchanged — then applies it to the engine
// when the engine reports strategies (best-effort, like the
// dashboard handler; a test engine that does not report them
// leaves the persisted value to take effect on restart). With no
// persist wired the call fails instead of applying runtime-only:
// a runtime-only change would not show in the tool's own
// read-back (RoutingStrategy reads the config), so success would
// lie. Unknown strategy names are invalid input. An empty strategy
// name is rejected too (strategy.Parse("") fails) — unlike
// POST /dashboard/strategies, which accepts an empty global and
// persists it as priority.
func (x *serverMCPSource) SetRoutingStrategy(chain, strategyName string) error {
	st, ok := strategy.Parse(strategyName)
	if !ok {
		return fmt.Errorf("%w: unknown strategy %q", mcp.ErrInvalidInput, strategyName)
	}
	canon := string(st)
	if x.srv.routingPersist == nil {
		return errMCPNotAvailable("set_routing_strategy", "no routing persist wired — a strategy change would apply at runtime only and the tool's read-back would still show the old value; use POST /dashboard/strategies")
	}
	var update RoutingUpdate
	if chain == "" {
		update.Global = &canon
	} else {
		update.Chains = map[string]string{chain: canon}
	}
	if err := x.srv.routingPersist(update); err != nil {
		return fmt.Errorf("unavailable: could not save routing update: %w", err)
	}
	if rep, ok := x.srv.engine.(strategyReporter); ok {
		if chain == "" {
			rep.SetGlobalStrategy(canon)
		} else {
			rep.SetChainStrategy(chain, canon)
		}
	}
	return nil
}

// routeTargets returns the providers offering a model, in
// declared (registry) order — Providers() follows the registry
// List order — with their live price facts. A virtual auto/*
// model has no provider chain: the auto-router resolves it, so
// there is no attempt order to explain.
func (x *serverMCPSource) routeTargets(model string) ([]mcp.RouteAttempt, error) {
	if strings.HasPrefix(model, "auto/") {
		return nil, fmt.Errorf("%w: model %q is a virtual auto-router model; its routing is resolved by the auto-router, not a provider chain", mcp.ErrInvalidInput, model)
	}
	var attempts []mcp.RouteAttempt
	for _, p := range x.Providers() {
		offers := false
		for _, m := range p.Models {
			if m == model {
				offers = true
				break
			}
		}
		if !offers {
			continue
		}
		att := mcp.RouteAttempt{Provider: p.ID, ProviderName: p.Name, Model: model, Enabled: p.Enabled}
		if rate, known := x.promptUSDPer1M(p.ID, model); known {
			att.PriceKnown = true
			att.PromptUSDPer1M = rate
		}
		attempts = append(attempts, att)
	}
	if len(attempts) == 0 {
		return nil, fmt.Errorf("%w: unknown model %q (no configured provider offers it)", mcp.ErrInvalidInput, model)
	}
	return attempts, nil
}

// ExplainRoute explains how a model would be routed: the
// effective strategy (chain = the model id) and the attempt order
// in declared (priority) order. The order is the DECLARED order:
// a re-ranking strategy re-orders at request time from live
// signals, and the note says so.
func (x *serverMCPSource) ExplainRoute(model string) (mcp.RouteExplanation, error) {
	var zero mcp.RouteExplanation
	attempts, err := x.routeTargets(model)
	if err != nil {
		return zero, err
	}
	info := x.RoutingStrategy(model)
	note := "Order shown is the declared (priority) order over the providers offering this model."
	if info.Name != "priority" {
		note += " Strategy " + info.Name + " re-ranks this order at request time from live latency, success, cost and quota signals, which this dry run does not observe — the first attempt above is not necessarily the first attempt a live request makes."
	} else {
		note += " Priority keeps declared order: no re-ranking."
	}
	return mcp.RouteExplanation{
		Model: model, Strategy: info.Name, Rule: info.Rule,
		Attempts: attempts, Note: note,
	}, nil
}

// SimulateRoute dry-runs a route: the fallback tree (one attempt
// per target, in declared order) plus the first-attempt prompt
// cost from the real price book when promptTokens > 0.
// Completion cost is never estimated — the completion length is
// unknown before the call.
func (x *serverMCPSource) SimulateRoute(model string, promptTokens int) (mcp.RouteSimulation, error) {
	var zero mcp.RouteSimulation
	if promptTokens < 0 {
		return zero, fmt.Errorf("%w: prompt_tokens must be >= 0", mcp.ErrInvalidInput)
	}
	attempts, err := x.routeTargets(model)
	if err != nil {
		return zero, err
	}
	info := x.RoutingStrategy(model)
	sim := mcp.RouteSimulation{
		Model: model, Strategy: info.Name,
		PromptTokens: promptTokens, CostKnown: false,
		Note: "Dry run — no provider was called. The tree is the declared order; a re-ranking strategy re-orders it at request time from live signals. Completion cost is never estimated (completion length is unknown before the call).",
	}
	for i, att := range attempts {
		sa := mcp.SimAttempt{Provider: att.Provider, Model: att.Model, PriceKnown: att.PriceKnown}
		if i == len(attempts)-1 {
			sa.OnFailure = "fail the request (no further targets)"
		} else {
			sa.OnFailure = "fail over to the next target on a retryable failure (429, 502, 503, 504, 5xx, network error); stop the chain on 400/401/403"
		}
		if att.PriceKnown && att.PromptUSDPer1M != nil && promptTokens > 0 {
			cost := float64(promptTokens) * (*att.PromptUSDPer1M) / 1000000
			sa.EstimatedPromptCostUSD = &cost
		}
		sim.Attempts = append(sim.Attempts, sa)
	}
	if len(sim.Attempts) > 0 && sim.Attempts[0].PriceKnown && sim.Attempts[0].EstimatedPromptCostUSD != nil {
		sim.EstimatedPromptCostUSD = sim.Attempts[0].EstimatedPromptCostUSD
		sim.CostKnown = true
	} else if promptTokens == 0 {
		sim.Note += " No cost estimated: pass prompt_tokens to price the first attempt."
	} else {
		sim.Note += " First target's price is unknown, so no cost is estimated (unpriced, never $0)."
	}
	return sim, nil
}

// ModelStatsAvailable reports whether the measured-latency backend
// behind ModelStats is available: true when the serve process has
// a ledger wired. The pick_fastest_model handler uses it (via the
// mcp package's optional interface) to say "backend unavailable"
// instead of "no measured latency yet" when there is no ledger
// at all (N4).
func (x *serverMCPSource) ModelStatsAvailable() bool {
	return x.srv.ledger != nil
}

// ModelStats aggregates the most recent ledger events (the
// ledger's RecentEvents cap) per (provider, model): requests,
// successes, and mean latency over successful events with a
// recorded latency. No events → no rows; no latency is ever
// guessed from prices or catalog order. No ledger (or a ledger
// read failure) → no rows, and ModelStatsAvailable() false says
// why — this method has no error return, so the empty list is
// the honest "no stats" answer either way.
func (x *serverMCPSource) ModelStats() []mcp.ModelStat {
	led, err := x.ledgerOrUnavailable("model_stats")
	if err != nil {
		return []mcp.ModelStat{}
	}
	events, err := led.RecentEvents(x.reqCtx(), 500, ledger.LogAll)
	if err != nil {
		return []mcp.ModelStat{}
	}
	type agg struct {
		requests, ok int
		latSum       int64
		latEvents    int
	}
	byKey := make(map[ledger.PriceKey]*agg)
	order := []ledger.PriceKey{}
	for _, ev := range events {
		k := ledger.PriceKey{Provider: ev.Provider, Model: ev.Model}
		a, exists := byKey[k]
		if !exists {
			a = &agg{}
			byKey[k] = a
			order = append(order, k)
		}
		a.requests++
		if ev.OK {
			a.ok++
			if ev.LatencyMS > 0 {
				a.latSum += ev.LatencyMS
				a.latEvents++
			}
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].Provider != order[j].Provider {
			return order[i].Provider < order[j].Provider
		}
		return order[i].Model < order[j].Model
	})
	out := make([]mcp.ModelStat, 0, len(order))
	for _, k := range order {
		a := byKey[k]
		st := mcp.ModelStat{Provider: k.Provider, Model: k.Model, Requests: a.requests, OK: a.ok, LatencyEvents: a.latEvents}
		if a.requests > 0 {
			rate := float64(a.ok) / float64(a.requests)
			st.SuccessRate = &rate
		}
		if a.latEvents > 0 {
			st.MeanLatencyMS = float64(a.latSum) / float64(a.latEvents)
			st.LatencyKnown = true
		}
		out = append(out, st)
	}
	return out
}

// CostReport reports cost from the live ledger and price book.
// Without a key id it is the authoritative all-window summary.
// With a key id the spend is the authoritative full-window sum
// for that key (SpendFor), while the request/token/by-model
// counts are sampled from the ledger's recent events — the
// ledger has no per-key aggregate query — and the report says
// so. Unpriced usage is UNKNOWN (CostKnown false), never $0.
// The key id arriving here was forced to the authenticated
// caller by the cost handlers (see the serverMCPSource comment);
// this method itself cannot tell whose a key id is.
func (x *serverMCPSource) CostReport(keyID string, days int) (mcp.CostReport, error) {
	var zero mcp.CostReport
	if days < 0 || days > 365 {
		return zero, fmt.Errorf("%w: days must be between 0 (all time) and 365", mcp.ErrInvalidInput)
	}
	led, err := x.ledgerOrUnavailable("cost_report")
	if err != nil {
		return zero, err
	}
	ctx := x.reqCtx()
	now := time.Now().UTC()
	since := time.Time{}
	if days > 0 {
		since = now.AddDate(0, 0, -days)
	}
	rep := mcp.CostReport{KeyID: keyID, Days: days, WindowStart: since, WindowEnd: now, CostKnown: true, ByModel: []mcp.ModelCost{}}
	priceKnown := func(provider, model string) bool {
		return x.PriceKnown(provider, model)
	}
	if keyID == "" {
		sum, err := led.Summary(ctx, since, time.Time{})
		if err != nil {
			return zero, fmt.Errorf("unavailable: ledger summary: %w", err)
		}
		rep.Requests, rep.OK, rep.Errors = sum.Requests, sum.OK, sum.Errors
		rep.PromptTokens, rep.CompletionTokens, rep.TotalTokens = sum.PromptTokens, sum.CompletionTokens, sum.TotalTokens
		rep.CostUSD = sum.CostUSD
		unpriced := make(map[string]bool)
		for _, m := range sum.ByModel {
			mc := mcp.ModelCost{
				Provider: m.Provider, Model: m.Model, Requests: m.Requests,
				PromptTokens: m.PromptTokens, CompletionTokens: m.CompletionTokens,
				TotalTokens: m.TotalTokens, CostUSD: m.CostUSD,
				PriceKnown: priceKnown(m.Provider, m.Model),
			}
			if !mc.PriceKnown && m.TotalTokens > 0 {
				mc.UnpricedTokens = m.TotalTokens
				rep.CostKnown = false
				rep.UnpricedTokens += m.TotalTokens
				unpriced[m.Provider+"/"+m.Model] = true
			}
			rep.ByModel = append(rep.ByModel, mc)
		}
		for model := range unpriced {
			rep.UnpricedModels = append(rep.UnpricedModels, model)
		}
		sort.Strings(rep.UnpricedModels)
		if !rep.CostKnown {
			rep.Note = "cost_usd is a lower bound: some usage in the window is unpriced (see unpriced_tokens / unpriced_models), never $0."
		}
		return rep, nil
	}
	// Per-key: authoritative spend + sampled counts.
	spent, err := led.SpendFor(ctx, keyID, since)
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger spend: %w", err)
	}
	rep.CostUSD = spent
	events, err := led.RecentEvents(ctx, 500, ledger.LogAll)
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger recent events: %w", err)
	}
	rep.Sampled = true
	byModel := make(map[ledger.PriceKey]*mcp.ModelCost)
	modelOrder := []ledger.PriceKey{}
	unpriced := make(map[string]bool)
	for _, ev := range events {
		if ev.GatewayKeyID != keyID || ev.TS.Before(since) {
			continue
		}
		rep.Requests++
		if ev.OK {
			rep.OK++
		} else {
			rep.Errors++
		}
		rep.PromptTokens += ev.PromptTokens
		rep.CompletionTokens += ev.CompletionTokens
		rep.TotalTokens += ev.TotalTokens
		k := ledger.PriceKey{Provider: ev.Provider, Model: ev.Model}
		mc, exists := byModel[k]
		if !exists {
			mc = &mcp.ModelCost{Provider: k.Provider, Model: k.Model, PriceKnown: priceKnown(k.Provider, k.Model)}
			byModel[k] = mc
			modelOrder = append(modelOrder, k)
		}
		mc.Requests++
		mc.PromptTokens += ev.PromptTokens
		mc.CompletionTokens += ev.CompletionTokens
		mc.TotalTokens += ev.TotalTokens
		mc.CostUSD += ev.CostUSD
		if !mc.PriceKnown && ev.TotalTokens > 0 {
			mc.UnpricedTokens += ev.TotalTokens
			rep.UnpricedTokens += ev.TotalTokens
			unpriced[k.Provider+"/"+k.Model] = true
		}
	}
	sort.Slice(modelOrder, func(i, j int) bool {
		if modelOrder[i].Provider != modelOrder[j].Provider {
			return modelOrder[i].Provider < modelOrder[j].Provider
		}
		return modelOrder[i].Model < modelOrder[j].Model
	})
	for _, k := range modelOrder {
		rep.ByModel = append(rep.ByModel, *byModel[k])
	}
	for model := range unpriced {
		rep.UnpricedModels = append(rep.UnpricedModels, model)
	}
	sort.Strings(rep.UnpricedModels)
	if len(unpriced) > 0 {
		rep.CostKnown = false
	}
	rep.Note = "cost_usd is the full-window spend for this key; requests, tokens and by_model are sampled from the most recent 500 ledger events (the ledger has no per-key aggregate query)."
	if !rep.CostKnown {
		rep.Note += " cost_usd is a lower bound: some sampled usage is unpriced, never $0; cost_known reflects the sampled events only — unpriced usage outside the sample would not be seen."
	}
	return rep, nil
}

// CheckQuota reports one key's budget position from the
// ledger's KeyBudgetStatus: the rolling 5-hour window plus the
// calendar windows. A key with no budget row is unlimited
// (HasBudget false, RemainingUSD nil) — never an invented cap.
// The key id arriving here was forced to the authenticated
// caller by the cost handlers (see the serverMCPSource comment).
func (x *serverMCPSource) CheckQuota(keyID string) (mcp.QuotaReport, error) {
	var zero mcp.QuotaReport
	if strings.TrimSpace(keyID) == "" {
		return zero, fmt.Errorf("%w: key_id must not be empty", mcp.ErrInvalidInput)
	}
	led, err := x.ledgerOrUnavailable("check_quota")
	if err != nil {
		return zero, err
	}
	st, err := led.KeyBudgetStatus(x.reqCtx(), keyID, time.Now())
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger key budget status: %w", err)
	}
	toWindow := func(w ledger.KeyBudgetWindowStatus) mcp.QuotaWindow {
		return mcp.QuotaWindow{LimitUSD: w.LimitUSD, SpentUSD: w.SpentUSD, RemainingUSD: w.RemainingUSD, AtLimit: w.AtLimit}
	}
	return mcp.QuotaReport{
		KeyID: keyID, HasBudget: st.HasBudget, LimitUSD: st.LimitUSD,
		SpentUSD: st.SpentUSD, RemainingUSD: st.RemainingUSD, AtLimit: st.AtLimit,
		UnpricedPolicy: string(st.UnpricedPolicy), ActiveWindow: st.ActiveWindow,
		ActiveLimitUSD: st.ActiveLimitUSD, WarningReached: st.WarningReached,
		UnpricedModels: append([]string(nil), st.UnpricedModels...),
		Daily:          toWindow(st.Daily), Weekly: toWindow(st.Weekly), Monthly: toWindow(st.Monthly),
	}, nil
}

// SetBudgetGuard writes a real key budget into the live ledger:
// the rolling 5-hour cap (required) plus optional calendar caps.
// With no calendar cap it uses SetKeyBudget (rolling only);
// with one it uses SetKeyBudgetConfig, preserving the row's
// existing reset interval, reset time and warning threshold (or
// the ledger defaults when the key has no row yet). The key id
// arriving here was forced to the authenticated caller by the
// cost handlers (see the serverMCPSource comment) — a remote key
// can only ever guard its own budget through this path.
func (x *serverMCPSource) SetBudgetGuard(keyID string, limitUSD float64, unpricedPolicy string, dailyLimitUSD, weeklyLimitUSD, monthlyLimitUSD *float64) error {
	if strings.TrimSpace(keyID) == "" {
		return fmt.Errorf("%w: key_id must not be empty", mcp.ErrInvalidInput)
	}
	if limitUSD < 0 {
		return fmt.Errorf("%w: limit_usd must be >= 0", mcp.ErrInvalidInput)
	}
	policy, ok := ledger.NormalizeUnpricedPolicy(unpricedPolicy)
	if !ok {
		return fmt.Errorf("%w: unknown unpriced_policy %q (want fail_closed or count_as_zero)", mcp.ErrInvalidInput, unpricedPolicy)
	}
	for name, v := range map[string]*float64{"daily": dailyLimitUSD, "weekly": weeklyLimitUSD, "monthly": monthlyLimitUSD} {
		if v != nil && *v < 0 {
			return fmt.Errorf("%w: %s_limit_usd must be >= 0", mcp.ErrInvalidInput, name)
		}
	}
	led, err := x.ledgerOrUnavailable("set_budget_guard")
	if err != nil {
		return err
	}
	ctx := x.reqCtx()
	if dailyLimitUSD == nil && weeklyLimitUSD == nil && monthlyLimitUSD == nil {
		if err := led.SetKeyBudget(ctx, keyID, limitUSD, policy); err != nil {
			return fmt.Errorf("unavailable: ledger set budget: %w", err)
		}
		return nil
	}
	existing, err := led.GetKeyBudget(ctx, keyID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("unavailable: ledger read budget: %w", err)
	}
	row := ledger.KeyBudget{
		KeyID: keyID, LimitUSD: limitUSD, UnpricedPolicy: policy,
		DailyLimitUSD: dailyLimitUSD, WeeklyLimitUSD: weeklyLimitUSD, MonthlyLimitUSD: monthlyLimitUSD,
		WarningThreshold: 0.8, ResetInterval: ledger.BudgetResetDaily, ResetTime: "00:00",
	}
	if err == nil {
		row.WarningThreshold = existing.WarningThreshold
		row.ResetInterval = existing.ResetInterval
		row.ResetTime = existing.ResetTime
	}
	if err := led.SetKeyBudgetConfig(ctx, row); err != nil {
		return fmt.Errorf("unavailable: ledger set budget: %w", err)
	}
	return nil
}

// PriceKnown reports whether a price is known for
// (provider, model): the operator price book first, then the
// ledger's per-token prices.
func (x *serverMCPSource) PriceKnown(providerName, model string) bool {
	s := x.srv
	if s.priceBook != nil && s.priceBook.OperatorHasPrice(providerName, model) {
		return true
	}
	return s.getLedgerPrices().Known(providerName, model)
}

// CacheStats reports the live in-memory cache layers. The
// persistent tier is the same store as the exact layer in this
// process, so it is reported Present=false with a note rather
// than double-counted — the stdio server's disk read reports
// the on-disk rows.
func (x *serverMCPSource) CacheStats() mcp.CacheStats {
	s := x.srv
	st := mcp.CacheStats{
		Note: "persistent tier is the same store as the exact layer in this process; its on-disk rows are reported by the stdio server's disk read, not double-counted here",
	}
	if s.exactCache != nil {
		entries, bytes := s.exactCache.Stats()
		st.Exact = mcp.CacheLayerStats{Present: true, Entries: entries, Bytes: bytes}
	}
	if s.semanticCache != nil {
		entries, bytes := s.semanticCache.Stats()
		st.Semantic = mcp.CacheLayerStats{Present: true, Entries: entries, Bytes: bytes}
	}
	return st
}

// CacheFlush evicts the named tenant's entries through the same
// flush store the admin endpoint uses: real per-tenant eviction
// when the engine keeps a per-tenant index, 0 when it does not
// (the store records no tenant per entry — the honest count,
// never a flush-all disguised as a per-key flush).
func (x *serverMCPSource) CacheFlush(tenant string) (int, error) {
	return x.srv.cacheFlushStore().FlushTenant(x.reqCtx(), tenant), nil
}

// mcpDBFileSpec is one database file the inspector knows how to
// count: name for display, path on disk, and the tables whose
// schema is known (an unknown table is never counted by guessing
// its name — a missing table is an error on that file, not a 0).
type mcpDBFileSpec struct {
	name   string
	path   string
	tables []string
}

// inspectDBs inspects the data-dir database files: existence,
// size, last write, and live row counts for known tables. It
// mirrors the stdio Source's disk read (same files, same
// tables); a missing file is a fact (created on first use), not
// an error. Files open read-only (mode=ro, one connection), so
// inspection never creates or checkpoints a database.
func (x *serverMCPSource) inspectDBs() []mcp.DBFileHealth {
	s := x.srv
	if s.cfg == nil || s.cfg.DataDir == "" {
		return nil
	}
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(s.cfg.DataDir, p)
	}
	specs := []mcpDBFileSpec{
		{name: "ledger.db", path: filepath.Join(s.cfg.DataDir, "ledger.db"), tables: []string{"usage_events"}},
		{name: "feedback.db", path: filepath.Join(s.cfg.DataDir, "feedback.db"), tables: []string{"feedback_decisions", "feedback_events"}},
	}
	if s.cfg.Cache != nil {
		if p := resolve(s.cfg.Cache.ExactPath); p != "" {
			specs = append(specs, mcpDBFileSpec{name: filepath.Base(p), path: p, tables: []string{"cache_entries", "cache_savings"}})
		}
		if p := resolve(s.cfg.Cache.SemanticPath); p != "" {
			specs = append(specs, mcpDBFileSpec{name: filepath.Base(p), path: p, tables: []string{"semantic_entries"}})
		}
	}
	out := make([]mcp.DBFileHealth, 0, len(specs))
	for _, spec := range specs {
		f := mcp.DBFileHealth{Name: spec.name}
		info, err := os.Stat(spec.path)
		if err != nil {
			out = append(out, f)
			continue
		}
		f.Exists = true
		f.SizeBytes = info.Size()
		f.ModifiedAt = info.ModTime().UTC()
		ctx, cancel := context.WithTimeout(x.reqCtx(), 5*time.Second)
		db, err := sql.Open("sqlite", "file:"+spec.path+"?mode=ro")
		if err != nil {
			// H4: the open error embeds the DSN, i.e. the
			// full data-dir path — it is logged server-side
			// (where the path belongs) and the caller only
			// learns the database is unavailable.
			s.logger.Printf("mcp: inspectDBs %s: cannot open database: %v", spec.name, err)
			f.Error = "database unavailable"
			cancel()
			out = append(out, f)
			continue
		}
		db.SetMaxOpenConns(1)
		rows := make(map[string]int64, len(spec.tables))
		for _, table := range spec.tables {
			n, err := countSQLiteRows(ctx, db, table)
			if err != nil {
				// Same rule as the open error: log the
				// detail (which may name the file) and
				// report only that this table's count is
				// unavailable — never the driver's message.
				s.logger.Printf("mcp: inspectDBs %s: cannot count table %s: %v", spec.name, table, err)
				f.Error = fmt.Sprintf("count %s: unavailable", table)
				break
			}
			rows[table] = n
		}
		_ = db.Close()
		cancel()
		if len(rows) > 0 {
			f.Rows = rows
		}
		out = append(out, f)
	}
	return out
}

// countSQLiteRows counts one table. The driver ("sqlite") is
// registered by internal/ledger's blank import, which this
// package links through the Server's ledger field.
func countSQLiteRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var n int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DBHealth reports whether every data-dir database that exists
// opens cleanly and its known tables count without error.
func (x *serverMCPSource) DBHealth() mcp.DBHealth {
	files := x.inspectDBs()
	h := mcp.DBHealth{Healthy: true, Files: files}
	if x.srv.cfg == nil || x.srv.cfg.DataDir == "" {
		h.Note = "no data directory configured — no database files to check"
		return h
	}
	for _, f := range files {
		if f.Exists && f.Error != "" {
			h.Healthy = false
		}
	}
	return h
}

// DBStats reports the live per-file facts: sizes, row counts per
// known table, and last write (file modification time).
func (x *serverMCPSource) DBStats() mcp.DBStats {
	st := mcp.DBStats{Files: x.inspectDBs(), Note: "row counts are live counts from the SQLite files at call time; last write is the file modification time; sizes are the main database file only, excluding -wal/-shm sidecars"}
	if x.srv.cfg == nil || x.srv.cfg.DataDir == "" {
		st.Note = "no data directory configured — no database files to report"
	}
	return st
}

// memoryStore is the M9.4 file-backed memory store: one JSON line
// per entry under <data-dir>/mcp/memory.jsonl, next to the audit
// log. No DataDir means no memory store — the tools fail closed
// with an error, never with an in-memory store that would forget
// everything on exit and pretend otherwise.
func (x *serverMCPSource) memoryStore() (*mcp.MemoryStore, error) {
	if x.srv.cfg == nil || x.srv.cfg.DataDir == "" {
		return nil, errors.New("mcp: memory store unavailable — no data directory configured")
	}
	return mcp.NewMemoryStore(filepath.Join(x.srv.cfg.DataDir, "mcp", "memory.jsonl")), nil
}

func (x *serverMCPSource) MemoryAdd(caller, text string) (mcp.MemoryEntry, error) {
	st, err := x.memoryStore()
	if err != nil {
		return mcp.MemoryEntry{}, err
	}
	return st.Add(caller, text)
}

func (x *serverMCPSource) MemorySearch(caller, query string) ([]mcp.MemoryEntry, error) {
	st, err := x.memoryStore()
	if err != nil {
		return nil, err
	}
	return st.Search(caller, query)
}

func (x *serverMCPSource) MemoryClear(caller string) (int, error) {
	st, err := x.memoryStore()
	if err != nil {
		return 0, err
	}
	return st.Clear(caller)
}

// P1.2 ecosystem expansion — data source methods on serverMCPSource.

func (x *serverMCPSource) CapabilityEntries() []mcp.CapabilityEntry {
	// Capability metadata comes from the catalog + modelsync, exposed
	// via the registry. Read per-provider/model capability facts with
	// provenance. Empty when no registry is wired (never fabricate).
	if x.srv.registry == nil {
		return []mcp.CapabilityEntry{}
	}
	models, err := x.srv.registry.Models(x.reqCtx())
	if err != nil {
		return []mcp.CapabilityEntry{}
	}
	out := []mcp.CapabilityEntry{}
	seen := map[string]bool{}
	for _, m := range models {
		key := m.ProviderID + "/" + m.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		// Registry model info carries id + provider only; per-model
		// capability flags live in the catalog, which this source
		// does not read directly. Report unknown-with-provenance so
		// callers never mistake absence for unsupported.
		out = append(out, mcp.CapabilityEntry{
			ProviderID: m.ProviderID,
			ModelID:    m.ID,
			Capability: "tool_calling",
			Supported:  nil,
			Native:     false,
			Source:     "registry",
			Confidence: 0.0,
		})
	}
	return out
}

func (x *serverMCPSource) QuotaIntelligence(filter string) ([]mcp.QuotaIntelEntry, error) {
	// Quota intelligence mirrors /api/quota/intelligence: pools,
	// schedules, key budgets, internal budgets, free-tier providers.
	// Read from the live ledger + catalog. Filter is echoed back
	// but not applied here (the MCP tool handler applies it).
	if x.srv.ledger == nil {
		return nil, errMCPNotAvailable("QuotaIntelligence", "no ledger wired")
	}
	out := []mcp.QuotaIntelEntry{}
	now := time.Now().Format(time.RFC3339)
	// Key budgets from the ledger.
	if budgets, err := x.srv.ledger.ListKeyBudgets(x.reqCtx()); err == nil {
		for _, b := range budgets {
			st, err := x.srv.ledger.KeyBudgetStatus(x.reqCtx(), b.KeyID, time.Now())
			if err != nil {
				continue
			}
			state := "healthy"
			if st.AtLimit {
				state = "exhausted"
			} else if st.WarningReached {
				state = "approaching_limit"
			}
			out = append(out, mcp.QuotaIntelEntry{
				Kind:        "key_budget",
				ID:          b.KeyID,
				Name:        b.KeyID,
				Scope:       "key",
				Target:      b.KeyID,
				BudgetUSD:   st.LimitUSD,
				Consumed:    st.SpentUSD,
				UsedPct:     0,
				State:       state,
				Source:      "ledger_budget",
				Confidence:  0.95,
				UpdatedAt:   now,
				Filters:     map[string]string{"key_id": b.KeyID},
			})
			if st.LimitUSD > 0 {
				out[len(out)-1].UsedPct = st.SpentUSD / st.LimitUSD * 100
			}
		}
	}
	return out, nil
}

func (x *serverMCPSource) BudgetForecast(keyID string) (mcp.BudgetForecastEntry, error) {
	// Burn-rate forecast from the server's usage/budget/forecast logic
	// (Workstream B12): real ledger spend + stored budget limit.
	if x.srv.ledger == nil {
		return mcp.BudgetForecastEntry{}, errMCPNotAvailable("BudgetForecast", "no ledger wired")
	}
	fc, err := x.srv.usageBudgetForecast(x.reqCtx(), keyID, time.Now())
	if err != nil {
		return mcp.BudgetForecastEntry{}, err
	}
	entry := mcp.BudgetForecastEntry{
		KeyID:          keyID,
		SpendUSD24h:    fc.PeriodSpendUSD,
		LimitKnown:     fc.ActiveLimitUSD != nil,
		UpdatedAt:      time.Now().Format(time.RFC3339),
	}
	if fc.ActiveLimitUSD != nil {
		entry.LimitUSD = *fc.ActiveLimitUSD
	}
	if fc.BurnRate1hUSDh > 0 {
		entry.BurnRateUSDPerHour = fc.BurnRate1hUSDh
	} else if fc.BurnRate24hUSDh > 0 {
		entry.BurnRateUSDPerHour = fc.BurnRate24hUSDh / 24
	}
	if fc.ProjectedExhaustion != nil {
		entry.ProjectedExhaustAt = fc.ProjectedExhaustion.Format(time.RFC3339)
	}
	entry.InsufficientData = fc.Status == "insufficient_data"
	return entry, nil
}

func (x *serverMCPSource) CompressionStats() mcp.CompressionStatsEntry {
	// Compression analytics from the live M6 cache layers.
	// No compress field on Server in this build — return zero stats
	// honestly rather than fabricating numbers.
	stats := mcp.CompressionStatsEntry{Layers: 0, LastUpdated: time.Now().Format(time.RFC3339)}
	return stats
}

func (x *serverMCPSource) EvalSchedules() ([]mcp.EvalScheduleEntry, error) {
	st, err := x.srv.ensureEvalStore()
	if err != nil {
		return nil, err
	}
	schs := st.List()
	out := make([]mcp.EvalScheduleEntry, 0, len(schs))
	for _, sch := range schs {
		out = append(out, mcp.EvalScheduleEntry{
			ID:            sch.ID,
			Suite:         sch.Suite,
			EverySec:      int64(sch.Every.Seconds()),
			Enabled:       sch.Enabled,
			TimeZone:      sch.TimeZone,
			LastRun:       sch.LastRun.Format(time.RFC3339),
			UseRealEngine: sch.UseRealEngine,
		})
	}
	return out, nil
}

func (x *serverMCPSource) OrgReport() mcp.OrgReportEntry {
	// Org/team reporting from the org store + ledger.
	st, err := x.srv.ensureOrgStore()
	if err != nil {
		return mcp.OrgReportEntry{}
	}
	orgs := st.Orgs()
	teams := st.Teams()
	keys := 0
	boundKeys := 0
	for _, t := range teams {
		// Team.Roles maps member identity -> role; count members as bound keys.
		boundKeys += len(t.Roles)
		for _, role := range t.Roles {
			if role != "" {
				keys++
			}
		}
	}
	totalBudget := 0.0
	for _, t := range teams {
		totalBudget += t.BudgetUSD
	}
	return mcp.OrgReportEntry{
		Orgs:           len(orgs),
		Teams:          len(teams),
		Keys:           keys,
		BoundKeys:      boundKeys,
		TotalBudgetUSD: totalBudget,
		UpdatedAt:      time.Now().Format(time.RFC3339),
	}
}
