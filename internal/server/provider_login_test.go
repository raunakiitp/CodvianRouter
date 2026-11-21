package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/crypto"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/providerlogin"
)

// loginAuthMock stands in for OpenRouter's auth host: /auth is the
// page the operator would open (never fetched by the router), and
// /api/v1/auth/keys exchanges a code for a fixed API key.
type loginAuthMock struct {
	srv       *httptest.Server
	calls     atomic.Int32
	status    int
	key       string
	lastVerif string
}

func newLoginAuthMock(t *testing.T) *loginAuthMock {
	t.Helper()
	m := &loginAuthMock{status: 200, key: "sk-or-v1-login-mock-key"}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/keys" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		m.calls.Add(1)
		var body struct {
			Code         string `json:"code"`
			CodeVerifier string `json:"code_verifier"`
			Method       string `json:"code_challenge_method"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		m.lastVerif = body.CodeVerifier
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(m.status)
		if m.status != 200 {
			_, _ = w.Write([]byte(`{"error":"invalid code"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"key":%q}`, m.key)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func loginFlowFor(m *loginAuthMock) *providerlogin.OpenRouterFlow {
	return &providerlogin.OpenRouterFlow{AuthBase: m.srv.URL}
}

// loginFakeAdder records what the callback stored, without any
// upstream: the guard/flow semantics tests do not need a registry.
type loginFakeAdder struct {
	mu         sync.Mutex
	gotKey     string
	gotCatalog string
	addErr     error
}

func (a *loginFakeAdder) AddCatalogWithOptions(_ context.Context, catalogID, apiKey string, _ provider.CatalogAddOptions) (*provider.Provider, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gotKey, a.gotCatalog = apiKey, catalogID
	if a.addErr != nil {
		return nil, a.addErr
	}
	return &provider.Provider{
		ID: "prov_login", Name: "OpenRouter", CatalogID: catalogID,
		Models: []provider.Model{{ID: "model-a"}, {ID: "model-b"}},
	}, nil
}

func (a *loginFakeAdder) AddContext(_ context.Context, _, _, _ string) (*provider.Provider, error) {
	return nil, errors.New("loginFakeAdder: custom add not used by provider login")
}

func startLogin(t *testing.T, srv *Server, body string) map[string]any {
	t.Helper()
	rec := doReqPeer(t, srv, "POST", "/admin/provider/login", body, "test-key-123", loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("start login: status %d, body %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("start login body not JSON: %v", err)
	}
	return out
}

func TestProviderLoginStartGuards(t *testing.T) {
	m := newLoginAuthMock(t)
	srv := newTestServer(t, &fakeEngine{}, WithProviderLoginFlow(loginFlowFor(m)))
	body := `{"provider":"openrouter","mode":"headless"}`

	// Off-host: 403 before auth, even with a valid key.
	if rec := doReqPeer(t, srv, "POST", "/admin/provider/login", body, "test-key-123", nonLoopbackPeer); rec.Code != http.StatusForbidden {
		t.Errorf("remote + key: status %d, want 403", rec.Code)
	}
	// On-host without a key: 401.
	if rec := doReqPeer(t, srv, "POST", "/admin/provider/login", body, "", loopbackPeer); rec.Code != http.StatusUnauthorized {
		t.Errorf("loopback no key: status %d, want 401", rec.Code)
	}
	out := startLogin(t, srv, body)
	if out["provider"] != "openrouter" || out["mode"] != "headless" {
		t.Errorf("start response = %v", out)
	}
	authURL, _ := out["authorization_url"].(string)
	if !strings.HasPrefix(authURL, m.srv.URL+"/auth?") {
		t.Errorf("authorization_url = %q, want the configured auth host", authURL)
	}
	if out["state"] == "" {
		t.Error("start response has no state")
	}
	// The verifier never leaves the server.
	if m.lastVerif != "" {
		t.Error("start already hit the exchange endpoint")
	}
	if strings.Contains(rec2body(t, srv, body), "code_verifier") {
		t.Error("start response mentions code_verifier")
	}
}

func rec2body(t *testing.T, srv *Server, body string) string {
	t.Helper()
	rec := doReqPeer(t, srv, "POST", "/admin/provider/login", body, "test-key-123", loopbackPeer)
	return rec.Body.String()
}

func TestProviderLoginStartValidation(t *testing.T) {
	m := newLoginAuthMock(t)
	srv := newTestServer(t, &fakeEngine{}, WithProviderLoginFlow(loginFlowFor(m)))
	bad := []string{
		``,
		`{}`,
		`{"provider":"claude"}`,
		`{"provider":"openrouter","mode":"magic"}`,
		`{"provider":"openrouter","limit":"abc"}`,
		`{"provider":"openrouter","expiration":"2-days"}`,
		`{"provider":"openrouter","unknown_field":1}`,
	}
	for _, body := range bad {
		rec := doReqPeer(t, srv, "POST", "/admin/provider/login", body, "test-key-123", loopbackPeer)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want 400", body, rec.Code)
		}
	}
}

func TestProviderLoginStartModes(t *testing.T) {
	m := newLoginAuthMock(t)
	t.Run("default callback with listen addr", func(t *testing.T) {
		srv := newTestServer(t, &fakeEngine{}, WithProviderLoginFlow(loginFlowFor(m)), WithListenAddr("127.0.0.1:20128"))
		out := startLogin(t, srv, `{"provider":"openrouter"}`)
		if out["mode"] != "callback" || out["callback_url"] != "http://127.0.0.1:20128/admin/provider/login/callback" {
			t.Errorf("response = %v", out)
		}
		u, _ := url.Parse(out["authorization_url"].(string))
		if u.Query().Get("callback_url") != "http://127.0.0.1:20128/admin/provider/login/callback" {
			t.Errorf("authorization URL callback = %q", u.Query().Get("callback_url"))
		}
	})
	t.Run("default headless without listen addr", func(t *testing.T) {
		srv := newTestServer(t, &fakeEngine{}, WithProviderLoginFlow(loginFlowFor(m)))
		out := startLogin(t, srv, `{"provider":"openrouter"}`)
		if out["mode"] != "headless" {
			t.Errorf("mode = %v, want headless", out["mode"])
		}
		if _, ok := out["callback_url"]; ok {
			t.Error("headless response carries callback_url")
		}
	})
	t.Run("explicit callback without listen addr is 400", func(t *testing.T) {
		srv := newTestServer(t, &fakeEngine{}, WithProviderLoginFlow(loginFlowFor(m)))
		rec := doReqPeer(t, srv, "POST", "/admin/provider/login", `{"provider":"openrouter","mode":"callback"}`, "test-key-123", loopbackPeer)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", rec.Code)
		}
	})
	t.Run("explicit headless with listen addr", func(t *testing.T) {
		srv := newTestServer(t, &fakeEngine{}, WithProviderLoginFlow(loginFlowFor(m)), WithListenAddr("127.0.0.1:20128"))
		out := startLogin(t, srv, `{"provider":"openrouter","mode":"headless","key_label":"Codvian Router"}`)
		if out["mode"] != "headless" {
			t.Errorf("mode = %v", out["mode"])
		}
		u, _ := url.Parse(out["authorization_url"].(string))
		if u.Query().Has("callback_url") || u.Query().Get("key_label") != "Codvian Router" {
			t.Errorf("authorization URL = %q", out["authorization_url"])
		}
	})
}

func TestProviderLoginCallbackGuards(t *testing.T) {
	m := newLoginAuthMock(t)
	srv := newTestServer(t, &fakeEngine{}, WithProviderLoginFlow(loginFlowFor(m)))
	// Off-host: 403 even with a perfectly formed query.
	rec := doReqPeer(t, srv, "GET", "/admin/provider/login/callback?code=c&state=s", "", "test-key-123", nonLoopbackPeer)
	if rec.Code != http.StatusForbidden {
		t.Errorf("remote callback: status %d, want 403", rec.Code)
	}
	// On-host WITHOUT a gateway key the handler runs (browser
	// redirect): missing params is a 400 page, not a 401.
	rec = doReqPeer(t, srv, "GET", "/admin/provider/login/callback", "", "", loopbackPeer)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("keyless loopback callback: status %d, want 400 (handler reached)", rec.Code)
	}
	if m.calls.Load() != 0 {
		t.Error("guarded requests reached the exchange endpoint")
	}
}

func TestProviderLoginCallbackHappyPath(t *testing.T) {
	m := newLoginAuthMock(t)
	adder := &loginFakeAdder{}
	var hookCalls atomic.Int32
	srv := newTestServer(t, &fakeEngine{},
		WithProviderLoginFlow(loginFlowFor(m)),
		WithProviderAdder(adder),
		WithProviderAddedHook(func(_ context.Context, _ *provider.Provider) error { hookCalls.Add(1); return nil }),
	)
	out := startLogin(t, srv, `{"provider":"openrouter","mode":"headless"}`)
	state := out["state"].(string)

	rec := doReqPeer(t, srv, "GET", "/admin/provider/login/callback?code=auth-code-1&state="+url.QueryEscape(state), "", "", loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback: status %d, body %s", rec.Code, rec.Body.String())
	}
	page := rec.Body.String()
	if !strings.Contains(page, "OpenRouter connected") || !strings.Contains(page, "prov_login") || !strings.Contains(page, "2 discovered models") {
		t.Errorf("success page = %q", page)
	}
	if strings.Contains(page, m.key) || strings.Contains(page, "auth-code-1") || strings.Contains(page, state) {
		t.Error("callback page leaks the key, code or state")
	}
	if adder.gotKey != m.key || adder.gotCatalog != "openrouter" {
		t.Errorf("stored credential = %q / %q, want the exchanged key via the openrouter catalog path", adder.gotKey, adder.gotCatalog)
	}
	if hookCalls.Load() != 1 {
		t.Errorf("routing hook calls = %d, want 1", hookCalls.Load())
	}
	if m.lastVerif == "" {
		t.Error("exchange did not carry the PKCE verifier")
	}
	// The state is single-use: a replay is refused.
	rec = doReqPeer(t, srv, "GET", "/admin/provider/login/callback?code=auth-code-1&state="+url.QueryEscape(state), "", "", loopbackPeer)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("replayed callback: status %d, want 400", rec.Code)
	}
}

func TestProviderLoginCallbackBadFlows(t *testing.T) {
	m := newLoginAuthMock(t)
	adder := &loginFakeAdder{}
	now := time.Unix(1700000000, 0)
	flow := loginFlowFor(m)
	flow.Now = func() time.Time { return now }
	srv := newTestServer(t, &fakeEngine{}, WithProviderLoginFlow(flow), WithProviderAdder(adder))

	t.Run("unknown state", func(t *testing.T) {
		rec := doReqPeer(t, srv, "GET", "/admin/provider/login/callback?code=c&state=never-issued", "", "", loopbackPeer)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Unknown or already-used") {
			t.Errorf("status %d body %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("expired flow", func(t *testing.T) {
		out := startLogin(t, srv, `{"provider":"openrouter","mode":"headless"}`)
		now = now.Add(providerlogin.OpenRouterPendingTTL + time.Second)
		rec := doReqPeer(t, srv, "GET", "/admin/provider/login/callback?code=c&state="+url.QueryEscape(out["state"].(string)), "", "", loopbackPeer)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "expired") {
			t.Errorf("status %d body %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("exchange refused", func(t *testing.T) {
		now = time.Unix(1700000000, 0)
		m.status = 403
		defer func() { m.status = 200 }()
		out := startLogin(t, srv, `{"provider":"openrouter","mode":"headless"}`)
		rec := doReqPeer(t, srv, "GET", "/admin/provider/login/callback?code=c&state="+url.QueryEscape(out["state"].(string)), "", "", loopbackPeer)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("status %d, want 502", rec.Code)
		}
		if adder.gotKey != "" {
			t.Error("a refused exchange still stored a credential")
		}
	})
	t.Run("declined authorization", func(t *testing.T) {
		before := m.calls.Load()
		rec := doReqPeer(t, srv, "GET", "/admin/provider/login/callback?error=access_denied", "", "", loopbackPeer)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "declined") {
			t.Errorf("status %d body %q", rec.Code, rec.Body.String())
		}
		if m.calls.Load() != before {
			t.Error("declined authorization still hit the exchange endpoint")
		}
	})
	t.Run("store failure is honest", func(t *testing.T) {
		m.status = 200
		adder.addErr = errors.New("upstream rejected the key")
		defer func() { adder.addErr = nil }()
		out := startLogin(t, srv, `{"provider":"openrouter","mode":"headless"}`)
		rec := doReqPeer(t, srv, "GET", "/admin/provider/login/callback?code=c&state="+url.QueryEscape(out["state"].(string)), "", "", loopbackPeer)
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Could not store") {
			t.Errorf("status %d body %q", rec.Code, rec.Body.String())
		}
	})
}

func TestProviderLoginCallbackNoAdder(t *testing.T) {
	m := newLoginAuthMock(t)
	srv := newTestServer(t, &fakeEngine{}, WithProviderLoginFlow(loginFlowFor(m)))
	out := startLogin(t, srv, `{"provider":"openrouter","mode":"headless"}`)
	rec := doReqPeer(t, srv, "GET", "/admin/provider/login/callback?code=c&state="+url.QueryEscape(out["state"].(string)), "", "", loopbackPeer)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503 (no registry wired)", rec.Code)
	}
}

// loginUpstream is a minimal OpenAI-compatible upstream for the
// real-registry callback test: model list + chat, recording auth.
type loginUpstream struct {
	mu       sync.Mutex
	lastAuth string
}

func (u *loginUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.lastAuth = r.Header.Get("Authorization")
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			_, _ = w.Write([]byte(`{"model":"model-a","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// TestProviderLoginCallbackRealRegistry is the genuine end-to-end
// path: mock auth host + real provider registry + mock upstream.
// The exchanged key is live-tested, stored encrypted, models are
// discovered; a second login joins the same provider's key pool as
// another account (multi-account rotation), never a duplicate.
func TestProviderLoginCallbackRealRegistry(t *testing.T) {
	m := newLoginAuthMock(t)
	up := &loginUpstream{}
	upSrv := httptest.NewServer(up.handler())
	defer upSrv.Close()

	dir := t.TempDir()
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-login!!"), dir)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	reg, err := provider.NewFileRegistry(filepath.Join(dir, "providers.json"), provider.WithCipher(provider.NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	// The catalog's OpenRouter base is the production host, so the
	// test seeds one OpenRouter provider against the mock upstream
	// (base-URL override, exactly as an operator would for a proxy)
	// and then logs in twice: both exchanged keys must join that
	// provider's encrypted pool as accounts, never duplicate it.
	seed, err := reg.AddCatalogWithOptions(context.Background(), "openrouter", "sk-or-v1-seed-key",
		provider.CatalogAddOptions{BaseURL: upSrv.URL})
	if err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	srv := newTestServer(t, &fakeEngine{},
		WithProviderLoginFlow(loginFlowFor(m)),
		WithProviderAdder(reg),
	)
	for i := 0; i < 2; i++ {
		m.key = fmt.Sprintf("sk-or-v1-account-%d", i)
		out := startLogin(t, srv, `{"provider":"openrouter","mode":"headless"}`)
		rec := doReqPeer(t, srv, "GET", "/admin/provider/login/callback?code=c&state="+url.QueryEscape(out["state"].(string)), "", "", loopbackPeer)
		if rec.Code != http.StatusOK {
			t.Fatalf("login %d: status %d body %s", i, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "key pool") {
			t.Errorf("login %d page does not name the account pool: %q", i, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), m.key) {
			t.Errorf("login %d page leaks the key", i)
		}
	}
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("providers = %d, want 1 (accounts join the pool, not duplicate providers)", len(list))
	}
	p := list[0]
	if p.ID != seed.ID || len(p.Keys) != 3 {
		t.Errorf("pool = provider %s with %d keys, want the seed provider with 3 accounts", p.ID, len(p.Keys))
	}
	for _, k := range p.Keys {
		if strings.Contains(k.Encrypted, "sk-or-v1-") {
			t.Error("pool key is not encrypted")
		}
	}
	// The upstream saw the exchanged keys live (add-time key test).
	up.mu.Lock()
	auth := up.lastAuth
	up.mu.Unlock()
	if auth != "Bearer sk-or-v1-account-1" {
		t.Errorf("last upstream auth = %q, want the second account's key (live-tested)", auth)
	}
	// Persistence: a fresh registry over the same file restores
	// the pool (encrypted credentials survive restart).
	reg2, err := provider.NewFileRegistry(filepath.Join(dir, "providers.json"), provider.WithCipher(provider.NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if err := reg2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg2.List(); len(got) != 1 || len(got[0].Keys) != 3 {
		t.Errorf("restored pool = %d providers / %d keys, want 1 / 3", len(got), len(got[0].Keys))
	}
}
