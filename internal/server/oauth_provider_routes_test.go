package server

// Provider OAuth connect-route tests (A3 W1): the five bridge methods
// exercised through the REAL server routes (newTestServer → ServeHTTP),
// not through the bridge methods directly. Pins:
//
//   - start returns a real authorize URL + verifier; complete exchanges
//     through the store's client and stores the connection; the
//     response NEVER carries the access token;
//   - device start/complete round-trip through the server-side flow
//     cache, answering pending while approval is outstanding;
//   - import stores an allowlisted paste blob and refuses a
//     wrong-provider blob;
//   - typed refusals: unknown provider 404, unimplemented 501 (with
//     the registry's documented reason), wrong-flow 409, import-only
//     on the code route 409;
//   - every mutating route requires a session or gateway key;
//   - the listing route serves the real providers registry.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/oauth/providers"
)

// newProviderRouteServer builds a test server whose connection store
// uses a plain test-injectable client (the same seam the bridge tests
// use — the SSRF-guarded-client behavior is proven separately in
// oauth_conn_guard_test.go and would refuse the httptest loopback
// mock).
func newProviderRouteServer(t *testing.T) (*Server, *oauthConnStore) {
	t.Helper()
	srv := newTestServer(t, &fakeEngine{})
	st := newOAuthConnStore(nil)
	st.client = &http.Client{}
	srv.oauthStore = st
	return srv, st
}

func providerRoutePost(t *testing.T, srv *Server, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, srv, http.MethodPost, path, body, key)
}

// oauthJSONBody marshals a request body for the connect routes.
func oauthJSONBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(b)
}

// deviceFlowMock serves /device and /token for the kimi-coding device
// flow; token answers pending until release() is called.
type deviceFlowMock struct {
	mu       sync.Mutex
	pending  bool
	released chan struct{}
}

func newDeviceFlowMock() *deviceFlowMock {
	return &deviceFlowMock{pending: true, released: make(chan struct{})}
}

func (m *deviceFlowMock) release() {
	m.mu.Lock()
	m.pending = false
	m.mu.Unlock()
	close(m.released)
}

func (m *deviceFlowMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		writeBridgeJSON(t, w, map[string]any{
			"device_code": "dc-kimi", "user_code": "KIMI-CODE",
			"verification_uri": "https://kimi.example/device", "interval": 5, "expires_in": 300,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		pending := m.pending
		m.mu.Unlock()
		if pending {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		writeBridgeJSON(t, w, map[string]any{
			"access_token": "kimi-at-1", "refresh_token": "kimi-rt-1",
			"token_type": "bearer", "expires_in": 3600,
		})
	})
	return httptest.NewServer(mux)
}

// TestProviderOAuthRoutesStartCompleteE2E: the code flow through the
// real routes — start returns the authorize URL/verifier, complete
// exchanges and stores, resolution serves the stored token, and no
// response ever carries a credential.
func TestProviderOAuthRoutesStartCompleteE2E(t *testing.T) {
	srv, st := newProviderRouteServer(t)

	m := &tokenMock{}
	mock := m.mockServer(t, "claude")
	defer mock.Close()
	providers.SetEndpointsForTest(t, "claude", map[string]string{"token": mock.URL + "/token"})

	const key = "test-key-123"

	// No credential: the mutating routes require a session or key.
	rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/claude/start", "{}", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated start: status %d, want 401", rec.Code)
	}

	rec = providerRoutePost(t, srv, "/dashboard/oauth/providers/claude/start", `{"redirect_uri":"http://127.0.0.1:3000/callback"}`, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body.String())
	}
	var start struct {
		Provider     string `json:"provider"`
		Flow         string `json:"flow"`
		State        string `json:"state"`
		AuthorizeURL string `json:"authorize_url"`
		Verifier     string `json:"verifier"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start body: %v", err)
	}
	if start.AuthorizeURL == "" || start.Verifier == "" || start.State == "" {
		t.Fatalf("start response incomplete: %+v", start)
	}

	rec = providerRoutePost(t, srv, "/dashboard/oauth/providers/claude/complete",
		`{"code":"code-claude","redirect_uri":"http://127.0.0.1:3000/callback","verifier":"`+start.Verifier+`"}`, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: status %d, body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "claude-at-1") {
		t.Fatalf("complete response leaked the access token: %s", rec.Body.String())
	}

	tok, err := srv.OAuthProviderAccessToken("claude")
	if err != nil {
		t.Fatalf("resolve after complete: %v", err)
	}
	if tok != "claude-at-1" {
		t.Errorf("stored token = %q, want %q", tok, "claude-at-1")
	}
	if _, ok := st.conns["claude"]; !ok {
		t.Errorf("connection not stored in the connection store")
	}
}

// TestProviderOAuthRoutesDeviceE2E: the device flow through the real
// routes — start caches the flow server-side, complete answers pending
// until approval, then connected with the tokens stored (never
// returned).
func TestProviderOAuthRoutesDeviceE2E(t *testing.T) {
	srv, st := newProviderRouteServer(t)

	mock := newDeviceFlowMock()
	hs := mock.server(t)
	defer hs.Close()
	providers.SetEndpointsForTest(t, "kimi-coding", map[string]string{
		"device": hs.URL + "/device",
		"token":  hs.URL + "/token",
	})

	const key = "test-key-123"

	rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/kimi-coding/device/start", "{}", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("device start: status %d, body %s", rec.Code, rec.Body.String())
	}
	var start struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("device start body: %v", err)
	}
	if start.DeviceCode == "" || start.UserCode == "" {
		t.Fatalf("device start incomplete: %+v", start)
	}

	// Approval outstanding: status pending (NOT an error).
	rec = providerRoutePost(t, srv, "/dashboard/oauth/providers/kimi-coding/device/complete",
		`{"device_code":"`+start.DeviceCode+`"}`, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("device complete (pending): status %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"pending"`) {
		t.Errorf("pending response = %s, want status pending", rec.Body.String())
	}

	// Unknown device code: an honest 400, not a fabricated poll.
	rec = providerRoutePost(t, srv, "/dashboard/oauth/providers/kimi-coding/device/complete",
		`{"device_code":"dc-never-started"}`, key)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown device code: status %d, want 400", rec.Code)
	}

	mock.release()
	rec = providerRoutePost(t, srv, "/dashboard/oauth/providers/kimi-coding/device/complete",
		`{"device_code":"`+start.DeviceCode+`"}`, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("device complete (approved): status %d, body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "kimi-at-1") {
		t.Fatalf("device complete response leaked the access token: %s", rec.Body.String())
	}
	if tok, err := srv.OAuthProviderAccessToken("kimi-coding"); err != nil || tok != "kimi-at-1" {
		t.Errorf("stored token = %q, err %v; want kimi-at-1", tok, err)
	}
	if _, ok := st.conns["kimi-coding"]; !ok {
		t.Errorf("connection not stored in the connection store")
	}
}

// TestProviderOAuthRoutesImportE2E: the allowlisted paste import stores
// a matching blob and refuses a mismatched one; resolution serves the
// imported token through the wired resolver path.
func TestProviderOAuthRoutesImportE2E(t *testing.T) {
	srv, st := newProviderRouteServer(t)
	const key = "test-key-123"

	blob := `{"provider":"antigravity","access_token":"ag-at-1","token_type":"bearer"}`
	rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/antigravity/import",
		oauthJSONBody(t, map[string]string{"blob": blob}), key)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: status %d, body %s", rec.Code, rec.Body.String())
	}
	if tok, err := srv.OAuthProviderAccessToken("antigravity"); err != nil || tok != "ag-at-1" {
		t.Errorf("imported token = %q, err %v; want ag-at-1", tok, err)
	}
	if _, ok := st.conns["antigravity"]; !ok {
		t.Errorf("imported connection not stored")
	}

	// A blob that names another provider is refused — one provider's
	// credential can never be stored under another's route.
	foreign := `{"provider":"agy","access_token":"nope"}`
	rec = providerRoutePost(t, srv, "/dashboard/oauth/providers/antigravity/import",
		oauthJSONBody(t, map[string]string{"blob": foreign}), key)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("foreign blob: status %d, want 400 (ErrImportRefused), body %s", rec.Code, rec.Body.String())
	}
}

// TestProviderOAuthRoutesTypedRefusals: the refusal statuses are typed,
// not guessed.
func TestProviderOAuthRoutesTypedRefusals(t *testing.T) {
	srv, _ := newProviderRouteServer(t)
	const key = "test-key-123"

	// Unknown provider → 404.
	if rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/no-such/start", "{}", key); rec.Code != http.StatusNotFound {
		t.Errorf("unknown provider: status %d, want 404", rec.Code)
	}
	// Honestly-unimplemented → 501 with the registry's documented
	// reason. Picked from the live registry (the unimplemented set
	// shrinks as handlers land).
	var unimpl providers.Info
	for _, in := range providers.All() {
		if !in.Implemented && !in.ImportOnly && in.Reason != "" {
			unimpl = in
			break
		}
	}
	if unimpl.ID != "" {
		rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/"+unimpl.ID+"/start", "{}", key)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("unimplemented provider %q: status %d, want 501", unimpl.ID, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), unimpl.Reason) {
			t.Errorf("501 body %q does not carry the documented reason", rec.Body.String())
		}
	}
	// Device handler on the code route → 409.
	if rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/kimi-coding/start", "{}", key); rec.Code != http.StatusConflict {
		t.Errorf("device handler on code route: status %d, want 409", rec.Code)
	}
	// Code handler on the device route → 409.
	if rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/claude/device/start", "{}", key); rec.Code != http.StatusConflict {
		t.Errorf("code handler on device route: status %d, want 409", rec.Code)
	}
	// Import-only provider on the code route → 409.
	if rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/antigravity/start", "{}", key); rec.Code != http.StatusConflict {
		t.Errorf("import-only on code route: status %d, want 409", rec.Code)
	}
	// Import refused for a non-allowlisted provider (blob never read).
	if rec := providerRoutePost(t, srv, "/dashboard/oauth/providers/claude/import", `{"blob":"{}"}`, key); rec.Code != http.StatusBadRequest {
		t.Errorf("non-allowlisted import: status %d, want 400", rec.Code)
	}
}

// TestProviderOAuthRoutesList: the listing route serves the real
// providers registry — implemented flows and the honestly-unimplemented
// ones with their reason — behind session-or-key auth.
func TestProviderOAuthRoutesList(t *testing.T) {
	srv, _ := newProviderRouteServer(t)

	if rec := doReq(t, srv, http.MethodGet, "/dashboard/oauth/providers", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: status %d, want 401", rec.Code)
	}
	rec := doReq(t, srv, http.MethodGet, "/dashboard/oauth/providers", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Providers []struct {
			ID          string `json:"id"`
			Implemented bool   `json:"implemented"`
			ImportOnly  bool   `json:"import_only"`
			Reason      string `json:"reason"`
		} `json:"providers"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if out.Total != len(providers.All()) || len(out.Providers) != len(providers.All()) {
		t.Fatalf("list total = %d, want %d (every catalog OAuth provider, implemented or not)", out.Total, len(providers.All()))
	}
	seenClaude, seenUnimplementedReason := false, false
	for _, p := range out.Providers {
		if p.ID == "claude" && p.Implemented {
			seenClaude = true
		}
		if !p.Implemented && p.Reason != "" {
			seenUnimplementedReason = true
		}
		if !p.Implemented && p.Reason == "" {
			t.Errorf("%q listed without its documented refusal reason", p.ID)
		}
	}
	if !seenClaude {
		t.Errorf("list missing the implemented claude entry")
	}
	if !seenUnimplementedReason && len(out.Providers) > 0 {
		t.Logf("no honestly-unimplemented OAuth providers remain; reason rendering not exercised")
	}
}
