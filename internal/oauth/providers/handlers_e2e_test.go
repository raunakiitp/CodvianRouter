package providers

// Per-handler end-to-end tests against local mock token endpoints.
// Each test proves the handler's REAL documented request shape (grant,
// client id, PKCE verifier / device code, provider-specific second
// steps) and the normalized token result — authorize (or device start)
// → exchange → refresh. The full storage/resolver chain lives in the
// bridge tests (internal/server).

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// e2eCtx is the background context for handler calls.
var e2eCtx = context.Background()

// readBody decodes a request body (form or JSON) into a flat map of
// string fields for assertions (non-string JSON values are skipped).
func readBody(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") || (ct == "" && r.Method != http.MethodGet) {
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Fatalf("decode JSON body: %v", err)
		}
		out := map[string]string{}
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
		return out
	}
	if err := r.ParseForm(); err != nil {
		t.Fatalf("parse form: %v", err)
	}
	m := map[string]string{}
	for k := range r.Form {
		m[k] = r.Form.Get(k)
	}
	return m
}

func writeJSONToken(t *testing.T, w http.ResponseWriter, fields map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(fields); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

// s256 recomputes the RFC 7636 challenge for a verifier.
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// TestClaudeHandlerE2E — PKCE authorize URL, JSON code exchange,
// rotating refresh.
func TestClaudeHandlerE2E(t *testing.T) {
	var exchanged, refreshed int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		switch body["grant_type"] {
		case "authorization_code":
			exchanged++
			if body["client_id"] != claudeClientID {
				t.Errorf("client_id = %q", body["client_id"])
			}
			if body["code"] != "auth-code-claude" {
				t.Errorf("code = %q", body["code"])
			}
			if body["redirect_uri"] != "http://127.0.0.1:54545/callback" {
				t.Errorf("redirect_uri = %q", body["redirect_uri"])
			}
			if len(body["code_verifier"]) < 43 {
				t.Errorf("code_verifier = %q, want an RFC 7636 verifier", body["code_verifier"])
			}
			writeJSONToken(t, w, map[string]any{
				"access_token": "sk-ant-oat-claude-1", "refresh_token": "claude-rot-1",
				"token_type": "bearer", "expires_in": 3600, "scope": "user:inference",
			})
		case "refresh_token":
			refreshed++
			if body["refresh_token"] != "claude-rot-1" {
				t.Errorf("refresh_token = %q, want the stored (rotated) one", body["refresh_token"])
			}
			if body["client_id"] != claudeClientID {
				t.Errorf("client_id = %q", body["client_id"])
			}
			writeJSONToken(t, w, map[string]any{
				"access_token": "sk-ant-oat-claude-2", "refresh_token": "claude-rot-2",
				"token_type": "bearer", "expires_in": 3600,
			})
		default:
			t.Errorf("unexpected grant_type %q", body["grant_type"])
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	SetEndpointsForTest(t, "claude", map[string]string{
		"auth":  srv.URL + "/oauth/authorize",
		"token": srv.URL + "/v1/oauth/token",
	})

	h, err := Get("claude")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	state, authURL, verifier, err := h.StartAuthorization("http://127.0.0.1:54545/callback")
	if err != nil {
		t.Fatalf("StartAuthorization: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("authorize URL: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != claudeClientID || q.Get("response_type") != "code" ||
		q.Get("code") != "true" || q.Get("state") != state ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != s256(verifier) {
		t.Errorf("authorize params = %v (state=%s verifier=%s)", q, state, verifier)
	}
	if !strings.Contains(q.Get("scope"), "user:inference") {
		t.Errorf("scope = %q, want it to request user:inference", q.Get("scope"))
	}

	tr, err := h.ExchangeCode(e2eCtx, srv.Client(), "auth-code-claude", "http://127.0.0.1:54545/callback", verifier)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tr.AccessToken != "sk-ant-oat-claude-1" || tr.RefreshToken != "claude-rot-1" || tr.ExpiresIn != 3600 {
		t.Errorf("exchange result = %+v", tr)
	}
	if tr.TokenURL == "" {
		t.Errorf("exchange result carries no refresh TokenURL")
	}

	tr2, err := h.Refresh(e2eCtx, srv.Client(), tr.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tr2.AccessToken != "sk-ant-oat-claude-2" || tr2.RefreshToken != "claude-rot-2" {
		t.Errorf("refresh result = %+v, want the rotated pair", tr2)
	}
	if exchanged != 1 || refreshed != 1 {
		t.Errorf("mock hits: exchange=%d refresh=%d, want 1/1", exchanged, refreshed)
	}
}

// TestGitHubCopilotHandlerE2E — device flow start, pending→approved
// poll, Copilot API-token exchange, refresh via re-exchange.
func TestGitHubCopilotHandlerE2E(t *testing.T) {
	polls, pending := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		form := readBody(t, r)
		if form["client_id"] != copilotClientID || form["scope"] != "read:user" {
			t.Errorf("device request = %v", form)
		}
		writeJSONToken(t, w, map[string]any{
			"device_code": "dc-copilot-1", "user_code": "ABCD-1234",
			"verification_uri": "https://github.com/login/device", "interval": 5, "expires_in": 899,
		})
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		form := readBody(t, r)
		polls++
		if form["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" || form["device_code"] != "dc-copilot-1" || form["client_id"] != copilotClientID {
			t.Errorf("device poll = %v", form)
		}
		if polls == 1 {
			pending++
			w.WriteHeader(http.StatusForbidden)
			writeJSONToken(t, w, map[string]any{"error": "authorization_pending"})
			return
		}
		writeJSONToken(t, w, map[string]any{"access_token": "gho-copilot-1", "token_type": "bearer"})
	})
	mux.HandleFunc("/copilot_internal/v2/token", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token gho-copilot-1" {
			t.Errorf("Authorization = %q, want the GitHub OAuth token", got)
		}
		if r.Header.Get("Editor-Version") == "" || r.Header.Get("Editor-Plugin-Version") == "" {
			t.Errorf("editor headers missing: %v", r.Header)
		}
		writeJSONToken(t, w, map[string]any{
			"token": "copilot-jwt-1", "expires_in": 1800,
			"endpoints": map[string]string{"api": "https://api.githubcopilot.com"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	SetEndpointsForTest(t, "github-copilot", map[string]string{
		"device":   srv.URL + "/login/device/code",
		"token":    srv.URL + "/login/oauth/access_token",
		"exchange": srv.URL + "/copilot_internal/v2/token",
	})

	h, err := Get("github-copilot")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, _, _, err := h.StartAuthorization("http://127.0.0.1/x"); !errIs(err, ErrNotCodeFlow) {
		t.Errorf("StartAuthorization error = %v, want ErrNotCodeFlow", err)
	}
	dc, err := h.StartDevice(e2eCtx, srv.Client())
	if err != nil {
		t.Fatalf("StartDevice: %v", err)
	}
	if dc.UserCode != "ABCD-1234" || dc.VerificationURI == "" {
		t.Errorf("device offer = %+v", dc)
	}
	if _, err := h.PollDevice(e2eCtx, srv.Client(), dc); !errIs(err, ErrAuthorizationPending) {
		t.Errorf("first poll error = %v, want ErrAuthorizationPending", err)
	}
	tr, err := h.PollDevice(e2eCtx, srv.Client(), dc)
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if tr.AccessToken != "copilot-jwt-1" {
		t.Errorf("access token = %q, want the short-lived Copilot token", tr.AccessToken)
	}
	if tr.RefreshToken != "gho-copilot-1" {
		t.Errorf("refresh credential = %q, want the GitHub OAuth token", tr.RefreshToken)
	}
	if tr.ExpiresIn != 1800 {
		t.Errorf("expires_in = %d, want ~30 min", tr.ExpiresIn)
	}

	tr2, err := h.Refresh(e2eCtx, srv.Client(), tr.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tr2.AccessToken != "copilot-jwt-1" || tr2.RefreshToken != "gho-copilot-1" {
		t.Errorf("refresh result = %+v", tr2)
	}
	if pending != 1 || polls != 2 {
		t.Errorf("mock polls = %d (pending %d), want 2 (1 pending)", polls, pending)
	}
}

// TestClineHandlerE2E covers cline AND clinepass — same WorkOS device
// flow, registration, and rotating refresh.
func TestClineHandlerE2E(t *testing.T) {
	for _, id := range []string{"cline", "clinepass"} {
		t.Run(id, func(t *testing.T) {
			polls := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/user_management/authorize/device", func(w http.ResponseWriter, r *http.Request) {
				form := readBody(t, r)
				if form["client_id"] != clineWorkOSClientID {
					t.Errorf("device request = %v", form)
				}
				writeJSONToken(t, w, map[string]any{
					"device_code": "dc-" + id, "user_code": "CLIN-1234",
					"verification_uri": "https://auth.cline.bot/device", "interval": 5, "expires_in": 300,
				})
			})
			mux.HandleFunc("/user_management/authenticate", func(w http.ResponseWriter, r *http.Request) {
				form := readBody(t, r)
				polls++
				if form["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" || form["client_id"] != clineWorkOSClientID {
					t.Errorf("device poll = %v", form)
				}
				if polls == 1 {
					w.WriteHeader(http.StatusBadRequest)
					writeJSONToken(t, w, map[string]any{"error": "authorization_pending"})
					return
				}
				writeJSONToken(t, w, map[string]any{"access_token": "wk-access-1", "refresh_token": "wk-refresh-1"})
			})
			mux.HandleFunc("/api/v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
				body := readBody(t, r)
				if body["accessToken"] != "wk-access-1" || body["refreshToken"] != "wk-refresh-1" {
					t.Errorf("register body = %v", body)
				}
				writeJSONToken(t, w, map[string]any{
					"success": true,
					"data": map[string]any{
						"accessToken": "workos:account-tok-1", "refreshToken": "account-rot-1",
						"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					},
				})
			})
			mux.HandleFunc("/api/v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
				body := readBody(t, r)
				if body["granttype"] != "refresh_token" || body["refreshtoken"] != "account-rot-1" {
					t.Errorf("refresh body = %v, want Cline's lowercase refresh shape", body)
				}
				writeJSONToken(t, w, map[string]any{
					"success": true,
					"data": map[string]any{
						"accessToken": "workos:account-tok-2", "refreshToken": "account-rot-2",
						"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					},
				})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			SetEndpointsForTest(t, id, map[string]string{
				"device":   srv.URL + "/user_management/authorize/device",
				"poll":     srv.URL + "/user_management/authenticate",
				"register": srv.URL + "/api/v1/auth/register",
				"refresh":  srv.URL + "/api/v1/auth/refresh",
			})

			h, err := Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			dc, err := h.StartDevice(e2eCtx, srv.Client())
			if err != nil {
				t.Fatalf("StartDevice: %v", err)
			}
			if _, err := h.PollDevice(e2eCtx, srv.Client(), dc); !errIs(err, ErrAuthorizationPending) {
				t.Errorf("first poll error = %v, want pending", err)
			}
			tr, err := h.PollDevice(e2eCtx, srv.Client(), dc)
			if err != nil {
				t.Fatalf("second poll: %v", err)
			}
			if tr.AccessToken != "workos:account-tok-1" || tr.RefreshToken != "account-rot-1" {
				t.Errorf("register result = %+v", tr)
			}
			if tr.ExpiresIn < 3500 || tr.ExpiresIn > 3600 {
				t.Errorf("expires_in = %d, want ~1h from expiresAt", tr.ExpiresIn)
			}
			tr2, err := h.Refresh(e2eCtx, srv.Client(), tr.RefreshToken)
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if tr2.AccessToken != "workos:account-tok-2" || tr2.RefreshToken != "account-rot-2" {
				t.Errorf("refresh result = %+v, want the rotated pair", tr2)
			}
		})
	}
}

// TestPendingClassification pins the RFC 8628 pending/slow_down
// classification shared by every device handler.
func TestPendingClassification(t *testing.T) {
	mux := http.NewServeMux()
	slow := 0
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		slow++
		w.WriteHeader(http.StatusBadRequest)
		if slow == 1 {
			writeJSONToken(t, w, map[string]any{"error": "slow_down"})
			return
		}
		writeJSONToken(t, w, map[string]any{"error": "expired_token"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	h, err := Get("github-copilot")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	dc := &DeviceCode{DeviceCode: "x", state: map[string]string{"device_code": "x"}}
	SetEndpointsForTest(t, "github-copilot", map[string]string{"token": srv.URL + "/token"})
	if _, err := h.PollDevice(e2eCtx, srv.Client(), dc); !errIs(err, ErrAuthorizationPending) {
		t.Errorf("slow_down error = %v, want ErrAuthorizationPending", err)
	}
	if _, err := h.PollDevice(e2eCtx, srv.Client(), dc); err == nil || errIs(err, ErrAuthorizationPending) {
		t.Errorf("expired_token error = %v, want terminal", err)
	}
	if _, err := h.PollDevice(e2eCtx, srv.Client(), nil); err == nil {
		t.Errorf("poll without an offer: want error, got nil")
	}
}

func errIs(err error, target error) bool {
	if err == nil {
		return false
	}
	return err == target || strings.Contains(err.Error(), target.Error())
}
