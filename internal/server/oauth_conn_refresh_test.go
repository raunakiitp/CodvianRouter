package server

// A3: token refresh + expiry semantics for the OAuth connection store.
// These tests drive the REAL GetAccessToken against a mock token
// endpoint (via the store's injectable client) — no stub resolvers:
//   - a valid token is served untouched (the token endpoint is not
//     called);
//   - an expired token with a refresh token is refreshed (the mock
//     sees grant_type=refresh_token and the stored refresh token),
//     the NEW token is returned and persisted;
//   - an expired token without a refresh token errors instead of
//     serving a stale credential.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// oauthRefreshMock serves the token endpoint and counts requests by
// grant type, so tests can prove exactly what was called.
type oauthRefreshMock struct {
	refreshCalls int
	seenRefresh  []string
	t            *testing.T
}

func (m *oauthRefreshMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			m.t.Errorf("parse form: %v", err)
		}
		switch r.Form.Get("grant_type") {
		case "refresh_token":
			m.refreshCalls++
			m.seenRefresh = append(m.seenRefresh, r.Form.Get("refresh_token"))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"access_token":"refreshed-token-%d","token_type":"bearer","expires_in":3600}`, m.refreshCalls)
		default:
			m.t.Errorf("unexpected grant_type %q", r.Form.Get("grant_type"))
			w.WriteHeader(http.StatusBadRequest)
		}
	}
}

func newRefreshTestStore(t *testing.T, mock *oauthRefreshMock) (*oauthConnStore, string) {
	t.Helper()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	st := newOAuthConnStore(nil)
	st.client = srv.Client()
	st.mu.Lock()
	st.conns["provider-x"] = &oauthConn{
		Provider:     "provider-x",
		AccessToken:  "stale-token",
		RefreshToken: "stored-refresh-token",
		TokenURL:     srv.URL + "/token",
		ExpiresAt:    time.Now().Add(-time.Minute), // expired
	}
	st.mu.Unlock()
	return st, srv.URL
}

func TestGetAccessTokenRefreshesExpiredToken(t *testing.T) {
	t.Setenv("CODVIAN_OAUTH_PROVIDER-X_CLIENT_ID", "cid-123")
	t.Setenv("CODVIAN_OAUTH_PROVIDER-X_CLIENT_SECRET", "csecret-456")
	mock := &oauthRefreshMock{t: t}
	st, _ := newRefreshTestStore(t, mock)

	tok, err := st.GetAccessToken("provider-x")
	if err != nil {
		t.Fatalf("GetAccessToken: %v", err)
	}
	if tok != "refreshed-token-1" {
		t.Fatalf("token = %q, want the refreshed token", tok)
	}
	if mock.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", mock.refreshCalls)
	}
	if len(mock.seenRefresh) != 1 || mock.seenRefresh[0] != "stored-refresh-token" {
		t.Errorf("refresh used %v, want the stored refresh token", mock.seenRefresh)
	}

	st.mu.Lock()
	c := st.conns["provider-x"]
	expiresAt, updated := c.ExpiresAt, c.AccessToken
	st.mu.Unlock()
	if !expiresAt.After(time.Now().Add(30 * time.Minute)) {
		t.Errorf("ExpiresAt = %s, want ~1h in the future after refresh", expiresAt)
	}
	if updated != "refreshed-token-1" {
		t.Errorf("stored access token = %q, want the refreshed token persisted", updated)
	}

	// A fresh token is served WITHOUT hitting the token endpoint again.
	tok2, err := st.GetAccessToken("provider-x")
	if err != nil {
		t.Fatalf("second GetAccessToken: %v", err)
	}
	if tok2 != "refreshed-token-1" || mock.refreshCalls != 1 {
		t.Errorf("second call: token=%q calls=%d, want the same token and no extra refresh", tok2, mock.refreshCalls)
	}
}

func TestGetAccessTokenValidTokenNotRefreshed(t *testing.T) {
	mock := &oauthRefreshMock{t: t}
	st := newOAuthConnStore(nil)
	srvURL := "http://127.0.0.1:1/token" // would fail if called
	st.client = srvURLClient()
	st.mu.Lock()
	st.conns["provider-y"] = &oauthConn{
		Provider:    "provider-y",
		AccessToken: "valid-token",
		TokenURL:    srvURL,
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	st.mu.Unlock()

	tok, err := st.GetAccessToken("provider-y")
	if err != nil {
		t.Fatalf("GetAccessToken: %v", err)
	}
	if tok != "valid-token" {
		t.Fatalf("token = %q, want the stored token", tok)
	}
	if mock.refreshCalls != 0 {
		t.Errorf("token endpoint called %d times for a valid token, want 0", mock.refreshCalls)
	}
}

func TestGetAccessTokenExpiredWithoutRefreshTokenErrors(t *testing.T) {
	mock := &oauthRefreshMock{t: t}
	st, _ := newRefreshTestStore(t, mock)
	st.mu.Lock()
	st.conns["provider-x"].RefreshToken = ""
	st.mu.Unlock()

	_, err := st.GetAccessToken("provider-x")
	if err == nil {
		t.Fatal("expired token with no refresh token must error, not serve a stale credential")
	}
	if !strings.Contains(err.Error(), "cannot be refreshed") {
		t.Errorf("error = %v, want the reconnect guidance", err)
	}
}

// srvURLClient keeps the unused-client wiring explicit without dialing.
func srvURLClient() *http.Client {
	return &http.Client{Transport: failTransport{}}
}

type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("network dial attempted — a valid token must not hit the token endpoint")
}

func TestRefreshClientCredentialsRequired(t *testing.T) {
	// oauthClientID/Secret read the process env; with neither set the
	// refresh must refuse instead of sending an empty client_id.
	if oauthClientID("zz-nonexistent-provider") != "" || oauthClientSecret("zz-nonexistent-provider") != "" {
		t.Skip("provider credentials configured in env — skipping the unset case")
	}
	mock := &oauthRefreshMock{t: t}
	st, _ := newRefreshTestStore(t, mock)
	_, err := st.GetAccessToken("provider-x")
	if err == nil {
		t.Fatal("refresh without client credentials must error")
	}
	if !strings.Contains(err.Error(), "client credentials not configured") {
		t.Errorf("error = %v, want the client-credentials reason", err)
	}
	if mock.refreshCalls != 0 {
		t.Errorf("token endpoint called %d times without credentials, want 0", mock.refreshCalls)
	}
}

func TestRefreshSendsStandardForm(t *testing.T) {
	// Pins the OAuth wire format the refresh uses (grant_type,
	// client_id, client_secret, refresh_token) by capturing the form
	// with client credentials supplied via the documented env vars.
	t.Setenv("CODVIAN_OAUTH_PROVIDER-X_CLIENT_ID", "cid-123")
	t.Setenv("CODVIAN_OAUTH_PROVIDER-X_CLIENT_SECRET", "csecret-456")
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"tok-2","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	st := newOAuthConnStore(nil)
	st.client = srv.Client()
	st.mu.Lock()
	st.conns["provider-x"] = &oauthConn{
		Provider:     "provider-x",
		AccessToken:  "stale",
		RefreshToken: "rt-789",
		TokenURL:     srv.URL,
		ExpiresAt:    time.Now().Add(-time.Second),
	}
	st.mu.Unlock()

	tok, err := st.GetAccessToken("provider-x")
	if err != nil {
		t.Fatalf("GetAccessToken: %v", err)
	}
	if tok != "tok-2" {
		t.Fatalf("token = %q, want the refreshed token", tok)
	}
	if got.Get("grant_type") != "refresh_token" || got.Get("client_id") != "cid-123" ||
		got.Get("client_secret") != "csecret-456" || got.Get("refresh_token") != "rt-789" {
		t.Errorf("refresh form = %v, want the standard OAuth refresh form", got)
	}
}
