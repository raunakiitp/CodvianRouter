package webexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"
)

// This file defines the MOCK web surface contract every executor test
// drives (per the spec: tests run against mock web surfaces — never
// against live provider web UIs):
//
//   POST {origin}/api/chat
//     in:  {"messages":[{"role":"user","content":"..."}],"model":"...","temporary":true?}
//     out: {"content":"...","model":"..."}     (HTTP 200)
//     401 {"error":"stale session"}            (missing/unknown session cookie)
//     429 + Retry-After: <secs>                (quota-exhausted session cookie)
//
// The session cookie is mocksid=<token>; the mock accepts exactly one
// "logged-in" token per surface instance, so a stale/foreign token is
// provably refused — the 401-on-stale behavior is exercised for real
// at the HTTP layer.

const (
	mockSessionCookie = "mocksid"
	mockValidToken    = "good-session"
	mockQuotaToken    = "quota-session"
)

// recordedRequest is what the mock web UI saw on the last chat call.
type recordedRequest struct {
	CookieHeader  string
	Prompt        string
	Model         string
	TemporarySet  bool // "temporary" present in the body at all
	TemporaryFlag bool // its value when present
}

// mockWebSurface is a full fake provider web UI.
type mockWebSurface struct {
	mu      sync.Mutex
	server  *httptest.Server
	cookie  string // the accepted session cookie token
	lastReq recordedRequest
	hits    int
}

func newMockWebSurface() *mockWebSurface {
	m := &mockWebSurface{cookie: mockValidToken}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat", m.handleChat)
	mux.HandleFunc("GET /login", m.handleLogin)
	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockWebSurface) handleChat(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hits++

	cookie, err := r.Cookie(mockSessionCookie)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"stale session"}`))
		return
	}
	if cookie.Value == mockQuotaToken {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"quota exhausted"}`))
		return
	}
	if cookie.Value != m.cookie {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"stale session"}`))
		return
	}

	var body struct {
		Messages  []struct{ Content string } `json:"messages"`
		Model     string                     `json:"model"`
		Temporary *bool                      `json:"temporary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	prompt := ""
	if len(body.Messages) > 0 {
		prompt = body.Messages[len(body.Messages)-1].Content
	}
	m.lastReq = recordedRequest{
		CookieHeader:  r.Header.Get("Cookie"),
		Prompt:        prompt,
		Model:         body.Model,
		TemporarySet:  body.Temporary != nil,
		TemporaryFlag: body.Temporary != nil && *body.Temporary,
	}

	content := "mock reply to: " + prompt
	if m.lastReq.TemporaryFlag {
		content += " [temporary]"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"content": content,
		"model":   body.Model,
	})
}

// handleLogin is the mock UI's login endpoint: it sets the session
// cookie, mimicking what a real login establishes in the browser.
func (m *mockWebSurface) handleLogin(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		token = mockValidToken
	}
	http.SetCookie(w, &http.Cookie{Name: mockSessionCookie, Value: token, Path: "/"})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<html><body>logged in</body></html>`))
}

// URL is the mock origin to point an executor at.
func (m *mockWebSurface) URL() string { return m.server.URL }

func (m *mockWebSurface) lastHit() recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastReq
}

func (m *mockWebSurface) hitCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits
}

func (m *mockWebSurface) close() { m.server.Close() }

// sessionStateFor returns a captured SessionState carrying the mock's
// session cookie (what a browser capture would produce after login).
func (m *mockWebSurface) sessionStateFor(token string) *SessionState {
	return &SessionState{
		Cookies: []Cookie{{
			Name:     mockSessionCookie,
			Value:    token,
			Domain:   hostOf(m.server.URL),
			Path:     "/",
			Expires:  float64(time.Now().Add(24 * time.Hour).Unix()),
			Secure:   false,
			SameSite: "Lax",
		}},
		Origins: []OriginStorage{{
			Origin:       m.server.URL,
			LocalStorage: []LocalStorageEntry{{Key: "lastModel", Value: "mock-default"}},
		}},
		CapturedAt: time.Now(),
	}
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
