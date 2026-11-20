package server

// Dashboard operator login (PARITY.md:669, B3-04): the dashboard
// had no login, session, or password of any kind — protection was
// loopback-bind only. This file adds a genuine operator login:
//
//   - One operator password, stored ONLY as an Argon2id hash in
//     <data-dir>/dashboard_password.json (0600, atomic rename).
//     There is NO default password — OmniRoute's CHANGEME default
//     is on the must-never-copy list, so a fresh install has no
//     password at all until the operator sets one.
//   - Set the first password with POST /dashboard/auth/setup,
//     which needs an admin gateway key (the operator proves who
//     they are with the key they already hold; the key is read
//     from the sidebar Operator key field, never stored).
//   - Log in with POST /dashboard/auth/login; success sets an
//     HttpOnly session cookie (12h, server-side token, in-memory
//     only — a restart ends every session, the operator logs in
//     again). Failed logins are throttled per peer: 5 failures
//     lock the peer out for 60s.
//   - Change with POST /dashboard/auth/change (valid session +
//     current password, or an admin gateway key in place of the
//     session); every session is invalidated on change.
//   - Log out with POST /dashboard/auth/logout.
//
// Enforcement lives in register() (guard.go): once a password is
// set, every AuthExempt GET /dashboard/* feed requires a valid
// session cookie OR a valid gateway key — requireLogin. With no
// password set the feeds stay open exactly as today, so existing
// behaviour, existing tests, and the loopback-only protection are
// untouched. GET /dashboard itself stays open: it serves the
// static shell, whose login overlay appears when a feed 401s.
// The auth endpoints themselves are the login surface, so the
// Tier 3 wrapper cannot guard them (it would 401 every login
// attempt before the password could be checked) — register()
// applies scope + key-policy to them and each handler enforces
// its own credential.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// dashAuthFile is the file inside the data directory holding the
// Argon2id hash of the operator password (0600). The password
// itself is never stored, never logged, never returned.
const dashAuthFile = "dashboard_password.json"

// dashSessionCookie is the session cookie set by a successful
// login. HttpOnly, Path=/, SameSite=Lax; the value is a random
// token whose validity lives server-side only.
const dashSessionCookie = "codvian_dash_session"

// dashSessionTTL is how long one login lasts. Sessions are
// in-memory only, so a restart ends them all.
const dashSessionTTL = 12 * time.Hour

// dashPasswordMinLen / dashPasswordMaxLen bound the operator
// password. Length is the control — no complexity rules, no
// blocklists; 12 characters is the floor.
const (
	dashPasswordMinLen = 12
	dashPasswordMaxLen = 256
)

// dashLoginMaxFailures / dashLoginLockout are the brute-force
// throttle: this many failed logins from one peer lock that peer
// out for this long.
const (
	dashLoginMaxFailures = 5
	dashLoginLockout     = 60 * time.Second
)

// Argon2id parameters for the operator password. Stronger than
// the provider-key KDF (internal/crypto): the secret here is a
// human password, not a random 32-byte value.
const (
	dashArgonTime    uint32 = 3
	dashArgonMemory  uint32 = 65536 // KiB (64 MiB)
	dashArgonThreads uint8  = 1
	dashArgonSaltLen        = 16
	dashArgonKeyLen  uint32 = 32
)

// dashSessionBytes is the entropy of one session token: 32
// crypto/rand bytes, hex-encoded.
const dashSessionBytes = 32

// dashAuthStore is the dashboard password + session store: one
// Argon2id hash persisted to the data dir, sessions and login
// throttle in memory. All methods are safe for concurrent use.
type dashAuthStore struct {
	mu       sync.Mutex
	file     string // "" = in-memory only (no data dir)
	hash     string // "" = no password set
	sessions map[string]time.Time
	// failures counts consecutive failed logins per peer IP;
	// lockedUntil is when a locked-out peer may try again.
	failures    map[string]int
	lockedUntil map[string]time.Time
}

type dashAuthState struct {
	Version int    `json:"version"`
	Hash    string `json:"hash"`
}

// newDashAuthStore loads the persisted password hash, or starts
// empty when the file does not exist. A corrupt file is an error
// — a broken auth store must never silently open the dashboard.
func newDashAuthStore(dataDir string) (*dashAuthStore, error) {
	st := &dashAuthStore{
		sessions:    map[string]time.Time{},
		failures:    map[string]int{},
		lockedUntil: map[string]time.Time{},
	}
	if dataDir == "" {
		return st, nil
	}
	st.file = filepath.Join(dataDir, dashAuthFile)
	raw, err := os.ReadFile(st.file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return nil, fmt.Errorf("dashboard password store unreadable: %w", err)
	}
	var state dashAuthState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("dashboard password store corrupt: %w", err)
	}
	if state.Version != 1 || state.Hash == "" {
		return nil, errors.New("dashboard password store corrupt: unknown version or empty hash")
	}
	if _, _, err := parseDashPasswordHash(state.Hash); err != nil {
		return nil, fmt.Errorf("dashboard password store corrupt: %w", err)
	}
	st.hash = state.Hash
	return st, nil
}

// PasswordSet reports whether an operator password exists.
func (st *dashAuthStore) PasswordSet() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.hash != ""
}

// SetPassword stores the Argon2id hash of pw, persisting it
// (when a file is configured) before it takes effect in memory.
func (st *dashAuthStore) SetPassword(pw string) error {
	hash, err := hashDashPassword(pw)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.file != "" {
		raw, err := json.Marshal(dashAuthState{Version: 1, Hash: hash})
		if err != nil {
			return err
		}
		tmp := st.file + ".tmp"
		if err := os.WriteFile(tmp, raw, 0600); err != nil {
			return err
		}
		if err := os.Rename(tmp, st.file); err != nil {
			return err
		}
		if err := os.Chmod(st.file, 0600); err != nil {
			return err
		}
	}
	st.hash = hash
	return nil
}

// VerifyPassword checks pw against the stored hash. No password
// set never verifies — an empty password is not a password.
func (st *dashAuthStore) VerifyPassword(pw string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.hash == "" {
		return false
	}
	ok, err := verifyDashPasswordHash(pw, st.hash)
	return err == nil && ok
}

// NewSession mints one session token valid for dashSessionTTL.
func (st *dashAuthStore) NewSession() (string, time.Time, error) {
	b := make([]byte, dashSessionBytes)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	tok := hex.EncodeToString(b)
	exp := time.Now().Add(dashSessionTTL)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sessions[tok] = exp
	return tok, exp, nil
}

// ValidSession reports whether tok is a live session (expired
// tokens are dropped on sight).
func (st *dashAuthStore) ValidSession(tok string) bool {
	if tok == "" {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	exp, ok := st.sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(st.sessions, tok)
		return false
	}
	return true
}

// InvalidateSession ends one session (logout).
func (st *dashAuthStore) InvalidateSession(tok string) {
	if tok == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.sessions, tok)
}

// InvalidateAllSessions ends every session (password change).
func (st *dashAuthStore) InvalidateAllSessions() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sessions = map[string]time.Time{}
}

// LoginAllowed reports whether peer may attempt a login now.
func (st *dashAuthStore) LoginAllowed(peer string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	until, locked := st.lockedUntil[peer]
	return !locked || time.Now().After(until)
}

// RecordLoginFailure counts one failed login; at the threshold
// the peer is locked out for dashLoginLockout. A success clears
// the count (see RecordLoginSuccess).
func (st *dashAuthStore) RecordLoginFailure(peer string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.failures[peer]++
	if st.failures[peer] >= dashLoginMaxFailures {
		st.lockedUntil[peer] = time.Now().Add(dashLoginLockout)
		st.failures[peer] = 0
	}
}

// RecordLoginSuccess clears a peer's failure count.
func (st *dashAuthStore) RecordLoginSuccess(peer string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.failures, peer)
	delete(st.lockedUntil, peer)
}

// hashDashPassword returns the encoded Argon2id hash of pw.
func hashDashPassword(pw string) (string, error) {
	salt := make([]byte, dashArgonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(pw), salt, dashArgonTime, dashArgonMemory, dashArgonThreads, dashArgonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		dashArgonMemory, dashArgonTime, dashArgonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash)), nil
}

// parseDashPasswordHash splits an encoded hash into its salt and
// key. Anything malformed is an error — a hash that cannot be
// parsed can never verify.
func parseDashPasswordHash(encoded string) (salt, hash []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return nil, nil, errors.New("unsupported password hash format")
	}
	var mem, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return nil, nil, errors.New("unsupported password hash parameters")
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, errors.New("bad password hash salt")
	}
	hash, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, errors.New("bad password hash value")
	}
	return salt, hash, nil
}

// verifyDashPasswordHash recomputes the hash for pw and compares
// it in constant time.
func verifyDashPasswordHash(pw, encoded string) (bool, error) {
	salt, want, err := parseDashPasswordHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, dashArgonTime, dashArgonMemory, dashArgonThreads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// validateDashPassword enforces the length bounds. It returns the
// user-facing message for a bad password, or "" when it is fine.
func validateDashPassword(pw string) string {
	if len(pw) < dashPasswordMinLen {
		return fmt.Sprintf("password must be at least %d characters", dashPasswordMinLen)
	}
	if len(pw) > dashPasswordMaxLen {
		return fmt.Sprintf("password must be at most %d characters", dashPasswordMaxLen)
	}
	return ""
}

// dashLoginPeer returns the peer IP for login throttling (the
// RemoteAddr host without its port; the whole value when it does
// not parse as host:port).
func dashLoginPeer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// Server wiring (lazy store, session check, feed gate, admin check).
// ---------------------------------------------------------------------------

// dashboardAuth returns this server's dashboard password/session
// store, creating it on first use from cfg.DataDir (in-memory
// only when there is no config or no data dir). A corrupt
// persisted state fails here and the auth endpoints answer 503 —
// a broken store must never silently open the dashboard.
func (s *Server) dashboardAuth() (*dashAuthStore, error) {
	s.dashAuthMu.Lock()
	defer s.dashAuthMu.Unlock()
	if s.dashAuth != nil {
		return s.dashAuth, nil
	}
	if s.dashAuthErr != nil {
		return nil, s.dashAuthErr
	}
	dataDir := ""
	if s.cfg != nil {
		dataDir = s.cfg.DataDir
	}
	st, err := newDashAuthStore(dataDir)
	if err != nil {
		s.dashAuthErr = err
		return nil, err
	}
	s.dashAuth = st
	return st, nil
}

// dashSessionValid reports whether the request carries a live
// dashboard session cookie. A store error is not a session —
// the feed gate 503s on store errors before calling this.
func (s *Server) dashSessionValid(r *http.Request) bool {
	st, err := s.dashboardAuth()
	if err != nil || st == nil {
		return false
	}
	c, err := r.Cookie(dashSessionCookie)
	if err != nil {
		return false
	}
	return st.ValidSession(c.Value)
}

// requireDashboardSession is the B3-04 feed gate (wired in
// register() for AuthExempt GET /dashboard/* routes): once an
// operator password is set, a feed needs a valid session cookie
// or a valid gateway key; with no password set it passes through
// exactly as today.
func (s *Server) requireDashboardSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := s.dashboardAuth()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "dashboard auth unavailable: "+err.Error())
			return
		}
		if st == nil || !st.PasswordSet() {
			next(w, r)
			return
		}
		if s.dashSessionValid(r) || s.authorized(r) {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "dashboard login required: POST /dashboard/auth/login with the operator password (or send an admin gateway key)")
	}
}

// adminScopeOK reports whether the request carries a gateway key
// (or gateway JWT) with the admin scope. No bearer, an unknown
// key, or a verifier that cannot name scopes all fail closed —
// setup and key-based change must prove admin, not merely
// present something key-shaped.
func (s *Server) adminScopeOK(r *http.Request) bool {
	if !s.authorized(r) {
		return false
	}
	raw := bearerKey(r)
	if raw == "" {
		return false
	}
	var got []string
	hasScopes := false
	if sv, ok := s.verifier.(ScopeVerifier); ok {
		got, hasScopes = sv.KeyScopes(r.Context(), raw)
	}
	if !hasScopes {
		if scopes, ok := s.oauthTokenScopes(raw); ok {
			got, hasScopes = scopes, true
		}
	}
	if !hasScopes {
		return false
	}
	return config.GatewayKey{Scopes: got}.HasScope("admin")
}

// setDashSessionCookie writes the session cookie for a fresh
// login; clearDashSessionCookie expires it (logout / password
// change).
func setDashSessionCookie(w http.ResponseWriter, tok string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     dashSessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(time.Until(exp).Seconds()),
		Expires:  exp,
	})
}

func clearDashSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     dashSessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// ---------------------------------------------------------------------------
// Handlers.
// ---------------------------------------------------------------------------

type dashAuthPasswordRequest struct {
	Password string `json:"password"`
}

type dashAuthChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleDashAuthSetup serves POST /dashboard/auth/setup: sets the
// dashboard operator password for the first time. Only while none
// is set (409 after that — use change), and only with an admin
// gateway key: the operator proves identity with the key they
// already hold. The password is stored only as an Argon2id hash.
func (s *Server) handleDashAuthSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.dashboardAuth()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "dashboard auth unavailable: "+err.Error())
		return
	}
	if st.PasswordSet() {
		writeError(w, http.StatusConflict, "dashboard password is already set: use POST /dashboard/auth/change to change it")
		return
	}
	if !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	if !s.adminScopeOK(r) {
		writeError(w, http.StatusForbidden, "requires scope admin: this action needs an admin gateway key")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req dashAuthPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid dashboard password setup: expected {\"password\": string}")
		return
	}
	if msg := validateDashPassword(req.Password); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err := st.SetPassword(req.Password); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not store dashboard password")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"password_set": true,
		"note":         "Dashboard password set. Every dashboard feed now requires a login (or an admin gateway key).",
	})
}

// handleDashAuthLogin serves POST /dashboard/auth/login: checks
// the operator password and, on success, sets the session cookie.
// A missing or wrong password is 401; a throttled peer is 429.
func (s *Server) handleDashAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.dashboardAuth()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "dashboard auth unavailable: "+err.Error())
		return
	}
	peer := dashLoginPeer(r)
	if !st.LoginAllowed(peer) {
		writeError(w, http.StatusTooManyRequests, "too many failed logins — try again in a minute")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req dashAuthPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid dashboard login: expected {\"password\": string}")
		return
	}
	if req.Password == "" || !st.VerifyPassword(req.Password) {
		st.RecordLoginFailure(peer)
		writeError(w, http.StatusUnauthorized, "wrong dashboard password")
		return
	}
	st.RecordLoginSuccess(peer)
	tok, exp, err := st.NewSession()
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not start dashboard session")
		return
	}
	setDashSessionCookie(w, tok, exp)
	writeJSON(w, http.StatusOK, map[string]any{
		"logged_in":  true,
		"expires_at": exp.UTC().Format(time.RFC3339),
	})
}

// handleDashAuthLogout serves POST /dashboard/auth/logout:
// invalidates the session cookie (or answers 401 when the caller
// has neither a session nor a gateway key — there is nothing to
// end). Always clears the cookie on success.
func (s *Server) handleDashAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.dashboardAuth()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "dashboard auth unavailable: "+err.Error())
		return
	}
	hadSession := s.dashSessionValid(r)
	if !hadSession && !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	if c, cerr := r.Cookie(dashSessionCookie); cerr == nil {
		st.InvalidateSession(c.Value)
	}
	clearDashSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"logged_in": false})
}

// handleDashAuthChange serves POST /dashboard/auth/change:
// replaces the operator password. The caller proves they are the
// operator with either a valid session (then the current password
// is required and must match) or an admin gateway key (then the
// current password is optional, but must match when given). Every
// session is invalidated, so the operator logs in again with the
// new password.
func (s *Server) handleDashAuthChange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.dashboardAuth()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "dashboard auth unavailable: "+err.Error())
		return
	}
	hasSession := s.dashSessionValid(r)
	hasAdmin := s.authorized(r) && s.adminScopeOK(r)
	if !hasSession && !hasAdmin {
		s.unauthorized(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req dashAuthChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid dashboard password change: expected {\"current_password\": string, \"new_password\": string}")
		return
	}
	if hasSession {
		if req.CurrentPassword == "" || !st.VerifyPassword(req.CurrentPassword) {
			writeError(w, http.StatusForbidden, "current password is incorrect")
			return
		}
	} else if req.CurrentPassword != "" && !st.VerifyPassword(req.CurrentPassword) {
		writeError(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	if msg := validateDashPassword(req.NewPassword); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err := st.SetPassword(req.NewPassword); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not store dashboard password")
		return
	}
	st.InvalidateAllSessions()
	clearDashSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"password_set": true,
		"logged_in":    false,
		"note":         "Dashboard password changed. All sessions ended — log in again with the new password.",
	})
}
