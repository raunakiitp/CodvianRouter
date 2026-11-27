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

// TestOpenAIHandlersE2E covers codex AND chatgpt — same auth.openai.com
// PKCE flow.
func TestOpenAIHandlersE2E(t *testing.T) {
	for _, id := range []string{"codex", "chatgpt"} {
		t.Run(id, func(t *testing.T) {
			var exchanged, refreshed int
			mux := http.NewServeMux()
			mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
				form := readBody(t, r)
				switch form["grant_type"] {
				case "authorization_code":
					exchanged++
					if form["client_id"] != openaiClientID || form["code"] != "codex-code-1" {
						t.Errorf("exchange form = %v", form)
					}
					if len(form["code_verifier"]) < 43 {
						t.Errorf("code_verifier = %q", form["code_verifier"])
					}
					if form["redirect_uri"] != "http://localhost:1455/auth/callback" {
						t.Errorf("redirect_uri = %q, want the documented localhost callback", form["redirect_uri"])
					}
					writeJSONToken(t, w, map[string]any{
						"access_token": "codex-at-1", "refresh_token": "codex-rt-1",
						"token_type": "bearer", "expires_in": 3600, "scope": openaiScopes,
					})
				case "refresh_token":
					refreshed++
					if form["client_id"] != openaiClientID || form["refresh_token"] != "codex-rt-1" {
						t.Errorf("refresh form = %v", form)
					}
					writeJSONToken(t, w, map[string]any{
						"access_token": "codex-at-2", "refresh_token": "codex-rt-2",
						"token_type": "bearer", "expires_in": 3600,
					})
				default:
					t.Errorf("unexpected grant_type %q", form["grant_type"])
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			SetEndpointsForTest(t, id, map[string]string{
				"auth":  srv.URL + "/oauth/authorize",
				"token": srv.URL + "/oauth/token",
			})

			h, err := Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			state, authURL, verifier, err := h.StartAuthorization("")
			if err != nil {
				t.Fatalf("StartAuthorization: %v", err)
			}
			u, _ := url.Parse(authURL)
			q := u.Query()
			if q.Get("client_id") != openaiClientID || q.Get("response_type") != "code" ||
				q.Get("state") != state || q.Get("code_challenge_method") != "S256" ||
				q.Get("code_challenge") != s256(verifier) ||
				q.Get("codex_cli_simplified_flow") != "true" {
				t.Errorf("authorize params = %v", q)
			}
			tr, err := h.ExchangeCode(e2eCtx, srv.Client(), "codex-code-1", "", verifier)
			if err != nil {
				t.Fatalf("ExchangeCode: %v", err)
			}
			if tr.AccessToken != "codex-at-1" || tr.ExpiresIn != 3600 {
				t.Errorf("exchange result = %+v", tr)
			}
			tr2, err := h.Refresh(e2eCtx, srv.Client(), tr.RefreshToken)
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if tr2.AccessToken != "codex-at-2" {
				t.Errorf("refresh result = %+v", tr2)
			}
			if exchanged != 1 || refreshed != 1 {
				t.Errorf("mock hits: exchange=%d refresh=%d", exchanged, refreshed)
			}
		})
	}
}

// TestKimiHandlerE2E — device flow with the required X-Msh-* headers.
func TestKimiHandlerE2E(t *testing.T) {
	var polls int
	mux := http.NewServeMux()
	checkHeaders := func(r *http.Request) {
		if r.Header.Get("X-Msh-Platform") != "kimi_cli" {
			t.Errorf("X-Msh-Platform = %q, want kimi_cli (the backend requires the device headers)", r.Header.Get("X-Msh-Platform"))
		}
	}
	mux.HandleFunc("/api/oauth/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		checkHeaders(r)
		form := readBody(t, r)
		if form["client_id"] != kimiClientID {
			t.Errorf("device request = %v", form)
		}
		writeJSONToken(t, w, map[string]any{
			"device_code": "dc-kimi-1", "user_code": "KIMI-1234",
			"verification_uri": "https://auth.kimi.com/device", "interval": 5, "expires_in": 300,
		})
	})
	mux.HandleFunc("/api/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		checkHeaders(r)
		form := readBody(t, r)
		switch form["grant_type"] {
		case "urn:ietf:params:oauth:grant-type:device_code":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				writeJSONToken(t, w, map[string]any{"error": "authorization_pending"})
				return
			}
			if form["client_id"] != kimiClientID || form["device_code"] != "dc-kimi-1" {
				t.Errorf("device poll = %v", form)
			}
			writeJSONToken(t, w, map[string]any{
				"access_token": "kimi-at-1", "refresh_token": "kimi-rt-1",
				"token_type": "bearer", "expires_in": 3600,
			})
		case "refresh_token":
			if form["client_id"] != kimiClientID || form["refresh_token"] != "kimi-rt-1" {
				t.Errorf("refresh form = %v", form)
			}
			writeJSONToken(t, w, map[string]any{
				"access_token": "kimi-at-2", "refresh_token": "kimi-rt-2",
				"token_type": "bearer", "expires_in": 3600,
			})
		default:
			t.Errorf("unexpected grant_type %q", form["grant_type"])
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	SetEndpointsForTest(t, "kimi-coding", map[string]string{
		"device": srv.URL + "/api/oauth/device_authorization",
		"token":  srv.URL + "/api/oauth/token",
	})

	h, err := Get("kimi-coding")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	dc, err := h.StartDevice(e2eCtx, srv.Client())
	if err != nil {
		t.Fatalf("StartDevice: %v", err)
	}
	if dc.UserCode != "KIMI-1234" {
		t.Errorf("device offer = %+v", dc)
	}
	if _, err := h.PollDevice(e2eCtx, srv.Client(), dc); !errIs(err, ErrAuthorizationPending) {
		t.Errorf("first poll error = %v, want pending", err)
	}
	tr, err := h.PollDevice(e2eCtx, srv.Client(), dc)
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if tr.AccessToken != "kimi-at-1" {
		t.Errorf("poll result = %+v", tr)
	}
	tr2, err := h.Refresh(e2eCtx, srv.Client(), tr.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tr2.AccessToken != "kimi-at-2" {
		t.Errorf("refresh result = %+v", tr2)
	}
}

// TestGitLabDuoHandlerE2E — operator-configured client, PKCE
// authorize URL, exchange and refresh at the documented endpoints.
func TestGitLabDuoHandlerE2E(t *testing.T) {
	t.Setenv(gitlabClientIDEnv, "gitlab-app-42")
	var exchanged, refreshed int
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		form := readBody(t, r)
		switch form["grant_type"] {
		case "authorization_code":
			exchanged++
			if form["client_id"] != "gitlab-app-42" || form["code"] != "gl-code-1" {
				t.Errorf("exchange form = %v", form)
			}
			if len(form["code_verifier"]) < 43 {
				t.Errorf("code_verifier = %q", form["code_verifier"])
			}
			writeJSONToken(t, w, map[string]any{
				"access_token": "gl-at-1", "refresh_token": "gl-rt-1",
				"token_type": "bearer", "expires_in": 7200, "scope": "api",
			})
		case "refresh_token":
			refreshed++
			if form["refresh_token"] != "gl-rt-1" || form["client_id"] != "gitlab-app-42" {
				t.Errorf("refresh form = %v", form)
			}
			writeJSONToken(t, w, map[string]any{
				"access_token": "gl-at-2", "refresh_token": "gl-rt-2",
				"token_type": "bearer", "expires_in": 7200,
			})
		default:
			t.Errorf("unexpected grant_type %q", form["grant_type"])
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	SetEndpointsForTest(t, "gitlab-duo", map[string]string{
		"auth":  srv.URL + "/oauth/authorize",
		"token": srv.URL + "/oauth/token",
	})

	h, err := Get("gitlab-duo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	state, authURL, verifier, err := h.StartAuthorization("http://127.0.0.1:3000/callback")
	if err != nil {
		t.Fatalf("StartAuthorization: %v", err)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	if q.Get("client_id") != "gitlab-app-42" || q.Get("response_type") != "code" ||
		q.Get("state") != state || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") != s256(verifier) {
		t.Errorf("authorize params = %v", q)
	}
	tr, err := h.ExchangeCode(e2eCtx, srv.Client(), "gl-code-1", "http://127.0.0.1:3000/callback", verifier)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tr.AccessToken != "gl-at-1" {
		t.Errorf("exchange result = %+v", tr)
	}
	tr2, err := h.Refresh(e2eCtx, srv.Client(), tr.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tr2.AccessToken != "gl-at-2" {
		t.Errorf("refresh result = %+v", tr2)
	}
	if exchanged != 1 || refreshed != 1 {
		t.Errorf("mock hits: exchange=%d refresh=%d", exchanged, refreshed)
	}
}

// TestGitLabDuoUnconfiguredClientRefused — no invented client: without
// the operator's application ID the flow refuses.
func TestGitLabDuoUnconfiguredClientRefused(t *testing.T) {
	t.Setenv(gitlabClientIDEnv, "")
	h, err := Get("gitlab-duo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, _, _, err := h.StartAuthorization("http://127.0.0.1/cb"); err == nil ||
		!strings.Contains(err.Error(), gitlabClientIDEnv) {
		t.Errorf("StartAuthorization error = %v, want it to name %s", err, gitlabClientIDEnv)
	}
	if _, err := h.ExchangeCode(e2eCtx, nil, "c", "u", "v"); err == nil {
		t.Errorf("ExchangeCode without configured client: want refusal, got nil")
	}
}

// TestAWSHandlersE2E covers kiro AND amazon-q — the AWS SSO OIDC
// device flow (register -> device_authorization -> CreateToken) with
// refresh via the refresh_token grant.
func TestAWSHandlersE2E(t *testing.T) {
	for _, id := range []string{"kiro", "amazon-q"} {
		t.Run(id, func(t *testing.T) {
			var registered bool
			mux := http.NewServeMux()
			mux.HandleFunc("/client/register", func(w http.ResponseWriter, r *http.Request) {
				body := readBody(t, r)
				if body["clientName"] == "" || body["clientType"] != "public" {
					t.Errorf("register body = %v", body)
				}
				registered = true
				writeJSONToken(t, w, map[string]any{
					"clientId": "oidc-client-1", "clientSecret": "oidc-secret-1",
					"clientSecretExpiresAt": time.Now().Add(90 * 24 * time.Hour).Unix(),
				})
			})
			mux.HandleFunc("/device_authorization", func(w http.ResponseWriter, r *http.Request) {
				if !registered {
					t.Errorf("device_authorization called before client registration")
				}
				body := readBody(t, r)
				if body["clientId"] != "oidc-client-1" || body["clientSecret"] != "oidc-secret-1" || body["startUrl"] != awsBuilderIDURL {
					t.Errorf("device body = %v", body)
				}
				writeJSONToken(t, w, map[string]any{
					"deviceCode": "aws-dc-1", "userCode": "AWSX-1234",
					"verificationUri":         "https://view.awsapps.com/start#/device",
					"verificationUriComplete": "https://view.awsapps.com/start#/device?user_code=AWSX-1234",
					"expiresIn":               600, "interval": 5,
				})
			})
			mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				body := readBody(t, r)
				if body["clientId"] != "oidc-client-1" || body["clientSecret"] != "oidc-secret-1" {
					t.Errorf("token body client = %v", body)
				}
				switch body["grantType"] {
				case "urn:ietf:params:oauth:grant-type:device_code":
					if body["deviceCode"] != "aws-dc-1" {
						t.Errorf("deviceCode = %q", body["deviceCode"])
					}
					writeJSONToken(t, w, map[string]any{
						"accessToken": "aws-at-1", "refreshToken": "aws-rt-1",
						"tokenType": "bearer", "expiresIn": 3600,
					})
				case "refresh_token":
					if body["refreshToken"] != "aws-rt-1" {
						t.Errorf("refreshToken = %q", body["refreshToken"])
					}
					writeJSONToken(t, w, map[string]any{
						"accessToken": "aws-at-2", "refreshToken": "aws-rt-2",
						"tokenType": "bearer", "expiresIn": 3600,
					})
				default:
					t.Errorf("unexpected grantType %q", body["grantType"])
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			SetEndpointsForTest(t, id, map[string]string{
				"register":   srv.URL + "/client/register",
				"deviceauth": srv.URL + "/device_authorization",
				"token":      srv.URL + "/token",
			})

			h, err := Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			dc, err := h.StartDevice(e2eCtx, srv.Client())
			if err != nil {
				t.Fatalf("StartDevice: %v", err)
			}
			if dc.UserCode != "AWSX-1234" {
				t.Errorf("device offer = %+v", dc)
			}
			tr, err := h.PollDevice(e2eCtx, srv.Client(), dc)
			if err != nil {
				t.Fatalf("PollDevice: %v", err)
			}
			if tr.AccessToken != "aws-at-1" || tr.RefreshToken != "aws-rt-1" {
				t.Errorf("poll result = %+v", tr)
			}
			tr2, err := h.Refresh(e2eCtx, srv.Client(), tr.RefreshToken)
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if tr2.AccessToken != "aws-at-2" {
				t.Errorf("refresh result = %+v", tr2)
			}
		})
	}
}

// TestPendingClassification pins the RFC 8628 pending/slow_down classification
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

// TestCursorHandlerE2E — PKCE authorize URL with the operator's
// dashboard-registered client, Basic-authenticated code exchange and
// rotating refresh at the documented token endpoint.
func TestCursorHandlerE2E(t *testing.T) {
	t.Setenv(cursorClientIDEnv, "oauth_client_cursor-1")
	t.Setenv(cursorClientSecretEnv, "cursor-secret-1")
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth_client_cursor-1:cursor-secret-1"))
	var exchanged, refreshed int
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/applications/token", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantBasic {
			t.Errorf("Authorization = %q, want HTTP Basic client auth", got)
		}
		body := readBody(t, r)
		switch body["grant_type"] {
		case "authorization_code":
			exchanged++
			if body["code"] != "cursor-code-1" {
				t.Errorf("code = %q", body["code"])
			}
			if body["redirect_uri"] != "http://127.0.0.1:4545/callback" {
				t.Errorf("redirect_uri = %q", body["redirect_uri"])
			}
			if len(body["code_verifier"]) < 43 {
				t.Errorf("code_verifier = %q, want an RFC 7636 verifier", body["code_verifier"])
			}
			if body["client_id"] != "" || body["client_secret"] != "" {
				t.Errorf("client credentials must ride the Basic header, not the body: %v", body)
			}
			writeJSONToken(t, w, map[string]any{
				"access_token": "cursor-at-1", "refresh_token": "cursor-rt-1",
				"token_type": "Bearer", "expires_in": 3600, "scope": cursorScope,
			})
		case "refresh_token":
			refreshed++
			if body["refresh_token"] != "cursor-rt-1" {
				t.Errorf("refresh_token = %q, want the stored one", body["refresh_token"])
			}
			writeJSONToken(t, w, map[string]any{
				"access_token": "cursor-at-2", "refresh_token": "cursor-rt-2",
				"token_type": "Bearer", "expires_in": 3600,
			})
		default:
			t.Errorf("unexpected grant_type %q", body["grant_type"])
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	SetEndpointsForTest(t, "cursor", map[string]string{
		"auth":  srv.URL + "/oauth/authorize",
		"token": srv.URL + "/oauth/applications/token",
	})

	h, err := Get("cursor")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	state, authURL, verifier, err := h.StartAuthorization("http://127.0.0.1:4545/callback")
	if err != nil {
		t.Fatalf("StartAuthorization: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("authorize URL: %v", err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "oauth_client_cursor-1" ||
		q.Get("redirect_uri") != "http://127.0.0.1:4545/callback" ||
		q.Get("state") != state || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") != s256(verifier) || q.Get("scope") != cursorScope {
		t.Errorf("authorize params = %v (state=%s verifier=%s)", q, state, verifier)
	}

	tr, err := h.ExchangeCode(e2eCtx, srv.Client(), "cursor-code-1", "http://127.0.0.1:4545/callback", verifier)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tr.AccessToken != "cursor-at-1" || tr.RefreshToken != "cursor-rt-1" || tr.ExpiresIn != 3600 {
		t.Errorf("exchange result = %+v", tr)
	}
	if tr.TokenURL == "" {
		t.Errorf("exchange result carries no refresh TokenURL")
	}

	tr2, err := h.Refresh(e2eCtx, srv.Client(), tr.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// Cursor's documented rotation: a new refresh token every refresh.
	if tr2.AccessToken != "cursor-at-2" || tr2.RefreshToken != "cursor-rt-2" {
		t.Errorf("refresh result = %+v, want the rotated pair", tr2)
	}
	if exchanged != 1 || refreshed != 1 {
		t.Errorf("mock hits: exchange=%d refresh=%d, want 1/1", exchanged, refreshed)
	}
}

// TestCursorUnconfiguredClientRefused — no invented client: without the
// operator's dashboard application credentials the flow refuses.
func TestCursorUnconfiguredClientRefused(t *testing.T) {
	t.Setenv(cursorClientIDEnv, "")
	t.Setenv(cursorClientSecretEnv, "")
	h, err := Get("cursor")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, _, _, err := h.StartAuthorization("http://127.0.0.1/cb"); err == nil ||
		!strings.Contains(err.Error(), cursorClientIDEnv) {
		t.Errorf("StartAuthorization error = %v, want it to name %s", err, cursorClientIDEnv)
	}
	if _, err := h.ExchangeCode(e2eCtx, nil, "c", "u", "v"); err == nil {
		t.Errorf("ExchangeCode without configured client: want refusal, got nil")
	}
	if _, err := h.Refresh(e2eCtx, nil, "r"); err == nil {
		t.Errorf("Refresh without configured client: want refusal, got nil")
	}
}

// TestGHECopilotHandlerE2E — the Enterprise Cloud data-residency device
// flow: device start and poll on the operator's SUBDOMAIN.ghe.com host,
// Copilot API-token exchange at api.<host>, refresh via re-exchange.
func TestGHECopilotHandlerE2E(t *testing.T) {
	t.Setenv(gheHostEnv, "octocorp.ghe.com")
	polls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		form := readBody(t, r)
		if form["client_id"] != copilotClientID || form["scope"] != "read:user" {
			t.Errorf("device request = %v", form)
		}
		writeJSONToken(t, w, map[string]any{
			"device_code": "dc-ghe-1", "user_code": "GHEX-1234",
			"verification_uri": "https://octocorp.ghe.com/login/device", "interval": 5, "expires_in": 899,
		})
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		form := readBody(t, r)
		polls++
		if form["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" ||
			form["device_code"] != "dc-ghe-1" || form["client_id"] != copilotClientID {
			t.Errorf("device poll = %v", form)
		}
		if polls == 1 {
			w.WriteHeader(http.StatusForbidden)
			writeJSONToken(t, w, map[string]any{"error": "authorization_pending"})
			return
		}
		writeJSONToken(t, w, map[string]any{"access_token": "gho-ghe-1", "token_type": "bearer"})
	})
	mux.HandleFunc("/copilot_internal/v2/token", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token gho-ghe-1" {
			t.Errorf("Authorization = %q, want the GitHub OAuth token", got)
		}
		if r.Header.Get("Editor-Version") == "" || r.Header.Get("Editor-Plugin-Version") == "" {
			t.Errorf("editor headers missing: %v", r.Header)
		}
		writeJSONToken(t, w, map[string]any{"token": "ghe-copilot-jwt-1", "expires_in": 1800})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	SetEndpointsForTest(t, "ghe-copilot", map[string]string{
		"device":   srv.URL + "/login/device/code",
		"token":    srv.URL + "/login/oauth/access_token",
		"exchange": srv.URL + "/copilot_internal/v2/token",
	})

	h, err := Get("ghe-copilot")
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
	if dc.UserCode != "GHEX-1234" || dc.VerificationURI == "" {
		t.Errorf("device offer = %+v", dc)
	}
	if _, err := h.PollDevice(e2eCtx, srv.Client(), dc); !errIs(err, ErrAuthorizationPending) {
		t.Errorf("first poll error = %v, want ErrAuthorizationPending", err)
	}
	tr, err := h.PollDevice(e2eCtx, srv.Client(), dc)
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if tr.AccessToken != "ghe-copilot-jwt-1" || tr.RefreshToken != "gho-ghe-1" || tr.ExpiresIn != 1800 {
		t.Errorf("poll result = %+v", tr)
	}
	tr2, err := h.Refresh(e2eCtx, srv.Client(), tr.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tr2.AccessToken != "ghe-copilot-jwt-1" || tr2.RefreshToken != "gho-ghe-1" {
		t.Errorf("refresh result = %+v, want the same GitHub credential kept", tr2)
	}
	if polls != 2 {
		t.Errorf("mock polls = %d, want 2", polls)
	}
}

// TestGHECopilotUnconfiguredHostRefused — no invented host: without the
// operator's Enterprise Cloud host the flow refuses instead of falling
// back to github.com.
func TestGHECopilotUnconfiguredHostRefused(t *testing.T) {
	t.Setenv(gheHostEnv, "")
	h, err := Get("ghe-copilot")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := h.StartDevice(e2eCtx, nil); err == nil || !strings.Contains(err.Error(), gheHostEnv) {
		t.Errorf("StartDevice error = %v, want it to name %s", err, gheHostEnv)
	}
	if _, err := h.PollDevice(e2eCtx, nil, &DeviceCode{state: map[string]string{"device_code": "x"}}); err == nil {
		t.Errorf("PollDevice without configured host: want refusal, got nil")
	}
	if _, err := h.Refresh(e2eCtx, nil, "r"); err == nil {
		t.Errorf("Refresh without configured host: want refusal, got nil")
	}
}

func errIs(err error, target error) bool {
	if err == nil {
		return false
	}
	return err == target || strings.Contains(err.Error(), target.Error())
}
