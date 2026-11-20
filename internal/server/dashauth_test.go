// Dashboard operator login tests (PARITY.md:669, B3-04).
//
// The feature is built (commit 8f572dc): Argon2id password in
// <data-dir>/dashboard_password.json (0600), setup/login/change/
// logout handlers, HttpOnly 12h session, 5-fail 60s throttle,
// feeds gated once a password is set. These tests exercise the
// real handlers through ServeHTTP — no mocks of the auth logic
// itself. Each test builds its own server (fresh in-memory store,
// fresh throttle state).

package server

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
)

const dashTestPassword = "correct-horse-battery-staple-1"
const dashTestPassword2 = "another-horse-battery-staple-2"
const dashTestPassword3 = "third-horse-battery-staple-3"

// newDashAuthServer builds a test server whose gateway keys are
// real config keys: one admin key and one non-admin key. WithConfig
// installs the config verifier, so adminScopeOK can name scopes.
func newDashAuthServer(t *testing.T, dataDir string) (*Server, string, string) {
	t.Helper()
	cfg := &config.Config{DataDir: dataDir}
	admin, adminRaw, err := config.IssueKey("dashauth-admin", []string{"admin"}, 0)
	if err != nil {
		t.Fatalf("config.IssueKey admin: %v", err)
	}
	if err := cfg.AddGatewayKey(admin); err != nil {
		t.Fatalf("cfg.AddGatewayKey admin: %v", err)
	}
	user, userRaw, err := config.IssueKey("dashauth-user", []string{"models"}, 0)
	if err != nil {
		t.Fatalf("config.IssueKey user: %v", err)
	}
	if err := cfg.AddGatewayKey(user); err != nil {
		t.Fatalf("cfg.AddGatewayKey user: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))
	return srv, adminRaw, userRaw
}

// dashReq issues one request against the real handler chain.
// peer overrides RemoteAddr ("" keeps httptest's 192.0.2.1:1234);
// the login throttle keys off the peer, so tests that need a
// second peer pass one explicitly.
func dashReq(t *testing.T, srv *Server, method, path, body, key, peer string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if peer != "" {
		req.RemoteAddr = peer
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func dashJSONBody(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(raw)
}

func dashBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response body is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return m
}

// dashSessionCookie returns the session cookie set by a response,
// failing the test when there is none.
func dashSessionCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == dashSessionCookie {
			return c
		}
	}
	t.Fatalf("no %q cookie in response (Set-Cookie: %q)", dashSessionCookie, rec.Header().Get("Set-Cookie"))
	return nil
}

// dashSetupPassword sets the first dashboard password; it must
// answer 201 with password_set true.
func dashSetupPassword(t *testing.T, srv *Server, adminKey, pw string) {
	t.Helper()
	rec := dashReq(t, srv, http.MethodPost, "/dashboard/auth/setup",
		dashJSONBody(t, map[string]string{"password": pw}), adminKey, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	if got := dashBody(t, rec)["password_set"]; got != true {
		t.Fatalf("setup password_set = %v, want true", got)
	}
}

// dashLoginOK logs in with pw; it must answer 200 and set the
// session cookie, which is returned.
func dashLoginOK(t *testing.T, srv *Server, pw string) *http.Cookie {
	t.Helper()
	rec := dashReq(t, srv, http.MethodPost, "/dashboard/auth/login",
		dashJSONBody(t, map[string]string{"password": pw}), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := dashBody(t, rec)["logged_in"]; got != true {
		t.Fatalf("login logged_in = %v, want true", got)
	}
	return dashSessionCookieFrom(t, rec)
}

func TestDashAuthSetupFirstPassword(t *testing.T) {
	srv, adminKey, userKey := newDashAuthServer(t, "")

	// No key at all: 401.
	rec := dashReq(t, srv, http.MethodPost, "/dashboard/auth/setup",
		dashJSONBody(t, map[string]string{"password": dashTestPassword}), "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("setup without key status = %d, want 401", rec.Code)
	}
	// A non-admin key authenticates but may not set the password: 403.
	rec = dashReq(t, srv, http.MethodPost, "/dashboard/auth/setup",
		dashJSONBody(t, map[string]string{"password": dashTestPassword}), userKey, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("setup with non-admin key status = %d, want 403", rec.Code)
	}
	// An admin key with a too-short password: 400, and no password
	// is set (a later valid setup must still succeed).
	rec = dashReq(t, srv, http.MethodPost, "/dashboard/auth/setup",
		dashJSONBody(t, map[string]string{"password": "short"}), adminKey, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("setup with short password status = %d, want 400", rec.Code)
	}
	// First valid setup: 201.
	dashSetupPassword(t, srv, adminKey, dashTestPassword)
	// A second setup is a conflict — the password exists now.
	rec = dashReq(t, srv, http.MethodPost, "/dashboard/auth/setup",
		dashJSONBody(t, map[string]string{"password": dashTestPassword2}), adminKey, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second setup status = %d, want 409", rec.Code)
	}
}

func TestDashAuthSetupPersistsArgon2idHash0600(t *testing.T) {
	dir := t.TempDir()
	srv, adminKey, _ := newDashAuthServer(t, dir)
	dashSetupPassword(t, srv, adminKey, dashTestPassword)

	// The password file exists, holds an Argon2id hash (never the
	// plaintext), and is 0600 where the OS can represent it.
	raw, err := os.ReadFile(filepath.Join(dir, dashAuthFile))
	if err != nil {
		t.Fatalf("dashboard password file unreadable: %v", err)
	}
	if strings.Contains(string(raw), dashTestPassword) {
		t.Fatal("dashboard password file contains the plaintext password")
	}
	var state dashAuthState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("dashboard password file is not JSON: %v", err)
	}
	if state.Version != 1 || !strings.HasPrefix(state.Hash, "$argon2id$v=19$") {
		t.Fatalf("dashboard password hash = %q, want an Argon2id v19 hash", state.Hash)
	}
	if runtime.GOOS != "windows" {
		// Windows cannot represent Unix permission bits (Go
		// reports 0666 there); the 0600 check only runs where
		// the mode is meaningful.
		if fi, err := os.Stat(filepath.Join(dir, dashAuthFile)); err != nil {
			t.Fatalf("stat dashboard password file: %v", err)
		} else if fi.Mode().Perm() != 0600 {
			t.Fatalf("dashboard password file mode = %o, want 0600", fi.Mode().Perm())
		}
	}

	// A fresh server on the same data dir reloads the hash from
	// disk: the password still logs in.
	srv2, _, _ := newDashAuthServer(t, dir)
	dashLoginOK(t, srv2, dashTestPassword)
}

func TestDashAuthLoginSuccessWrongPasswordAndCookie(t *testing.T) {
	srv, adminKey, _ := newDashAuthServer(t, "")
	dashSetupPassword(t, srv, adminKey, dashTestPassword)

	// Empty and wrong passwords are 401 and set no cookie.
	for _, pw := range []string{"", "wrong-password-here"} {
		rec := dashReq(t, srv, http.MethodPost, "/dashboard/auth/login",
			dashJSONBody(t, map[string]string{"password": pw}), "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("login with %q status = %d, want 401", pw, rec.Code)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == dashSessionCookie {
				t.Fatalf("failed login set a session cookie")
			}
		}
	}

	// The right password logs in and sets the session cookie.
	rec := dashReq(t, srv, http.MethodPost, "/dashboard/auth/login",
		dashJSONBody(t, map[string]string{"password": dashTestPassword}), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	body := dashBody(t, rec)
	if body["logged_in"] != true {
		t.Fatalf("login logged_in = %v, want true", body["logged_in"])
	}
	expStr, _ := body["expires_at"].(string)
	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil {
		t.Fatalf("login expires_at = %q, want RFC3339: %v", expStr, err)
	}
	if d := time.Until(exp); d < 11*time.Hour+50*time.Minute || d > 12*time.Hour+10*time.Minute {
		t.Fatalf("login expires_at is %v from now, want ~12h", d)
	}
	c := dashSessionCookieFrom(t, rec)
	if len(c.Value) != 64 {
		t.Fatalf("session cookie value length = %d, want 64 hex chars", len(c.Value))
	}
	if _, err := hex.DecodeString(c.Value); err != nil {
		t.Fatalf("session cookie value is not hex: %v", err)
	}
	if !c.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	if c.Path != "/" {
		t.Fatalf("session cookie Path = %q, want /", c.Path)
	}
	if c.MaxAge < 43100 || c.MaxAge > 43200 {
		t.Fatalf("session cookie MaxAge = %d, want ~43200 (12h)", c.MaxAge)
	}
	if d := time.Until(c.Expires); d < 11*time.Hour+50*time.Minute || d > 12*time.Hour+10*time.Minute {
		t.Fatalf("session cookie Expires is %v from now, want ~12h", d)
	}

	// The cookie from a successful login opens a gated feed.
	rec = dashReq(t, srv, http.MethodGet, "/dashboard/overview", "", "", "", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed with session cookie status = %d, want 200", rec.Code)
	}
}

func TestDashAuthLoginThrottleLocksOutAfterFiveFailures(t *testing.T) {
	srv, adminKey, _ := newDashAuthServer(t, "")
	dashSetupPassword(t, srv, adminKey, dashTestPassword)

	// Five wrong passwords: 401 each.
	for i := 0; i < 5; i++ {
		rec := dashReq(t, srv, http.MethodPost, "/dashboard/auth/login",
			dashJSONBody(t, map[string]string{"password": "wrong-password-here"}), "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong login %d status = %d, want 401", i+1, rec.Code)
		}
	}
	// The sixth attempt — even with the CORRECT password — is
	// throttled: 429.
	rec := dashReq(t, srv, http.MethodPost, "/dashboard/auth/login",
		dashJSONBody(t, map[string]string{"password": dashTestPassword}), "", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th login (correct password) status = %d, want 429", rec.Code)
	}
	// A seventh wrong attempt is still throttled.
	rec = dashReq(t, srv, http.MethodPost, "/dashboard/auth/login",
		dashJSONBody(t, map[string]string{"password": "wrong-password-here"}), "", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("7th login status = %d, want 429", rec.Code)
	}
	// The lockout is per peer: a different peer logs in fine.
	rec = dashReq(t, srv, http.MethodPost, "/dashboard/auth/login",
		dashJSONBody(t, map[string]string{"password": dashTestPassword}), "", "198.51.100.9:1234")
	if rec.Code != http.StatusOK {
		t.Fatalf("login from another peer status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestDashAuthLogoutInvalidatesSession(t *testing.T) {
	srv, adminKey, _ := newDashAuthServer(t, "")
	dashSetupPassword(t, srv, adminKey, dashTestPassword)
	cookie := dashLoginOK(t, srv, dashTestPassword)

	// Logout with neither a session nor a key: 401, nothing to end.
	rec := dashReq(t, srv, http.MethodPost, "/dashboard/auth/logout", "", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("logout without session status = %d, want 401", rec.Code)
	}
	// Logout with the session: 200, logged out, cookie cleared.
	rec = dashReq(t, srv, http.MethodPost, "/dashboard/auth/logout", "", "", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := dashBody(t, rec)["logged_in"]; got != false {
		t.Fatalf("logout logged_in = %v, want false", got)
	}
	cleared := dashSessionCookieFrom(t, rec)
	if cleared.MaxAge >= 0 {
		t.Fatalf("logout cookie MaxAge = %d, want negative (cleared)", cleared.MaxAge)
	}
	// The old session is dead: the gated feed 401s with it.
	rec = dashReq(t, srv, http.MethodGet, "/dashboard/overview", "", "", "", cookie)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("feed with logged-out cookie status = %d, want 401", rec.Code)
	}
	// Logging in again mints a fresh working session.
	cookie2 := dashLoginOK(t, srv, dashTestPassword)
	rec = dashReq(t, srv, http.MethodGet, "/dashboard/overview", "", "", "", cookie2)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed with fresh cookie status = %d, want 200", rec.Code)
	}
}

func TestDashAuthChangePassword(t *testing.T) {
	srv, adminKey, _ := newDashAuthServer(t, "")
	dashSetupPassword(t, srv, adminKey, dashTestPassword)
	cookie := dashLoginOK(t, srv, dashTestPassword)

	change := func(key string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		return dashReq(t, srv, http.MethodPost, "/dashboard/auth/change",
			dashJSONBody(t, map[string]string{
				"current_password": dashTestPassword,
				"new_password":     dashTestPassword2,
			}), key, "", cookies...)
	}
	// A session with the wrong current password: 403.
	rec := dashReq(t, srv, http.MethodPost, "/dashboard/auth/change",
		dashJSONBody(t, map[string]string{
			"current_password": "wrong-password-here",
			"new_password":     dashTestPassword2,
		}), "", "", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("change with wrong current status = %d, want 403", rec.Code)
	}
	// A session with a too-short new password: 400.
	rec = dashReq(t, srv, http.MethodPost, "/dashboard/auth/change",
		dashJSONBody(t, map[string]string{
			"current_password": dashTestPassword,
			"new_password":     "short",
		}), "", "", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("change with short new password status = %d, want 400", rec.Code)
	}
	// A session with the right current password: 200, logged out.
	rec = change("", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("change status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := dashBody(t, rec)["logged_in"]; got != false {
		t.Fatalf("change logged_in = %v, want false", got)
	}
	// Every session ended: the old cookie 401s on a gated feed.
	rec = dashReq(t, srv, http.MethodGet, "/dashboard/overview", "", "", "", cookie)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("feed with pre-change cookie status = %d, want 401", rec.Code)
	}
	// The old password is dead; the new one logs in.
	rec = dashReq(t, srv, http.MethodPost, "/dashboard/auth/login",
		dashJSONBody(t, map[string]string{"password": dashTestPassword}), "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login with old password status = %d, want 401", rec.Code)
	}
	dashLoginOK(t, srv, dashTestPassword2)

	// An admin gateway key changes the password with no session
	// and no current password.
	rec = dashReq(t, srv, http.MethodPost, "/dashboard/auth/change",
		dashJSONBody(t, map[string]string{"new_password": dashTestPassword3}), adminKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("key-based change status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	rec = dashReq(t, srv, http.MethodPost, "/dashboard/auth/login",
		dashJSONBody(t, map[string]string{"password": dashTestPassword2}), "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login with previous password status = %d, want 401", rec.Code)
	}
	dashLoginOK(t, srv, dashTestPassword3)
}

func TestDashAuthFeedsGatedOncePasswordSet(t *testing.T) {
	srv, adminKey, userKey := newDashAuthServer(t, "")

	// No password set: the feeds are open, exactly as before.
	rec := dashReq(t, srv, http.MethodGet, "/dashboard/overview", "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("feed without password set status = %d, want 200", rec.Code)
	}
	rec = dashReq(t, srv, http.MethodGet, "/dashboard", "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard shell without password set status = %d, want 200", rec.Code)
	}

	dashSetupPassword(t, srv, adminKey, dashTestPassword)

	// Password set: the feed 401s with no credential.
	rec = dashReq(t, srv, http.MethodGet, "/dashboard/overview", "", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("feed without credential status = %d, want 401", rec.Code)
	}
	// A valid gateway key (even non-admin) opens the feed.
	rec = dashReq(t, srv, http.MethodGet, "/dashboard/overview", "", userKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("feed with gateway key status = %d, want 200", rec.Code)
	}
	// A valid session cookie opens the feed.
	cookie := dashLoginOK(t, srv, dashTestPassword)
	rec = dashReq(t, srv, http.MethodGet, "/dashboard/overview", "", "", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed with session cookie status = %d, want 200", rec.Code)
	}
	// The static shell itself stays open — it serves the login
	// overlay when a feed 401s.
	rec = dashReq(t, srv, http.MethodGet, "/dashboard", "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard shell with password set status = %d, want 200", rec.Code)
	}
}

func TestDashAuthLoginWithoutPasswordSet(t *testing.T) {
	srv, _, _ := newDashAuthServer(t, "")
	// No password exists: no password can verify, login is 401.
	rec := dashReq(t, srv, http.MethodPost, "/dashboard/auth/login",
		dashJSONBody(t, map[string]string{"password": dashTestPassword}), "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login without password set status = %d, want 401", rec.Code)
	}
}

func TestDashAuthSessionExpires(t *testing.T) {
	st, err := newDashAuthStore("")
	if err != nil {
		t.Fatalf("newDashAuthStore: %v", err)
	}
	if err := st.SetPassword(dashTestPassword); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	tok, exp, err := st.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !st.ValidSession(tok) {
		t.Fatal("fresh session must be valid")
	}
	if d := time.Until(exp); d < 11*time.Hour+50*time.Minute || d > 12*time.Hour+10*time.Minute {
		t.Fatalf("session expiry is %v from now, want ~12h", d)
	}
	// An expired session is dropped on sight.
	st.mu.Lock()
	st.sessions[tok] = time.Now().Add(-time.Minute)
	st.mu.Unlock()
	if st.ValidSession(tok) {
		t.Fatal("expired session must be invalid")
	}
}
