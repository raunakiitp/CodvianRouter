package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/webexec"
)

// testConfigWithDataDir builds a minimal config pointed at a temp data
// dir so the bridge's lazy vault opens for real.
func testConfigWithDataDir(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{DataDir: t.TempDir()}
	return cfg
}

// TestWebExecBridgeSessionStoreVaultSealed proves the bridge store
// round-trips a session state through the per-data-dir Vault: save on
// one store built from the same data dir, load on a fresh Server —
// same-dir vaults share the key, so the operator's session survives a
// restart encrypted at rest.
func TestWebExecBridgeSessionStoreVaultSealed(t *testing.T) {
	cfg := testConfigWithDataDir(t)
	srv := New(nil, WithConfig(cfg))

	state := &webexec.SessionState{
		Cookies: []webexec.Cookie{{Name: "sessionKey", Value: "bridge-secret-value", Domain: "claude.ai", Path: "/"}},
	}
	if err := srv.WebExecImportSession("claude-web", state); err != nil {
		t.Fatalf("import: %v", err)
	}
	if !srv.WebExecSessionStore().Has("claude-web") {
		t.Fatal("session not stored")
	}

	// A second Server on the SAME data dir must load the same state.
	srv2 := New(nil, WithConfig(cfg))
	loaded, err := srv2.WebExecSessionStore().Load("claude-web")
	if err != nil {
		t.Fatalf("load on second server: %v", err)
	}
	if loaded.Cookies[0].Value != "bridge-secret-value" {
		t.Fatalf("round trip lost the cookie value: %+v", loaded.Cookies)
	}

	// A different data dir must NOT be able to load it (vault bound).
	other := New(nil, WithConfig(testConfigWithDataDir(t)))
	if _, err := other.WebExecSessionStore().Load("claude-web"); err == nil {
		t.Fatal("another data dir must not decrypt the session")
	}
}

// TestWebExecBridgeMemoryOnlyWithoutDataDir proves the no-data-dir
// path: in-memory store, works, no persistence expectation.
func TestWebExecBridgeMemoryOnlyWithoutDataDir(t *testing.T) {
	srv := New(nil)
	if err := srv.WebExecImportSession("grok-web", &webexec.SessionState{
		Cookies: []webexec.Cookie{{Name: "s", Value: "v", Domain: "grok.com"}},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := srv.WebExecSessionStore().Load("grok-web"); err != nil {
		t.Fatalf("load: %v", err)
	}
}

// TestWebExecBridgeRefusesUnknownIDs proves the import path only
// serves the 37 WebCookie IDs.
func TestWebExecBridgeRefusesUnknownIDs(t *testing.T) {
	srv := New(nil, WithConfig(testConfigWithDataDir(t)))
	err := srv.WebExecImportSession("totally-not-a-web-cookie-id", &webexec.SessionState{
		Cookies: []webexec.Cookie{{Name: "s", Value: "v"}},
	})
	if !errors.Is(err, webexec.ErrUnknownSurface) {
		t.Fatalf("want ErrUnknownSurface, got: %v", err)
	}
}

// TestWebExecBridgeExecutorRefusesLiveUndocumented is the production
// honesty gate through the SERVER wiring: an executor from the bridge
// pointed at real origins must refuse every undocumented surface —
// no invented URLs — even with a session stored.
func TestWebExecBridgeExecutorRefusesLiveUndocumented(t *testing.T) {
	srv := New(nil, WithConfig(testConfigWithDataDir(t)))
	for _, id := range webexec.CatalogIDsWithWebExec() {
		if err := srv.WebExecImportSession(id, &webexec.SessionState{
			Cookies: []webexec.Cookie{{Name: "s", Value: "v", Domain: "example.com"}},
		}); err != nil {
			t.Fatalf("%s: import: %v", id, err)
		}
		ex, err := srv.WebExecExecutor(id)
		if err != nil {
			t.Fatalf("%s: executor: %v", id, err)
		}
		_, err = ex.Execute(context.Background(), webexec.ExecuteRequest{Prompt: "hi"})
		if !errors.Is(err, webexec.ErrEndpointUndocumented) {
			t.Errorf("%s: live executor must refuse (ErrEndpointUndocumented), got: %v", id, err)
		}
	}
}

// TestWebExecClientFactoryRoutesWebCookie proves the factory wrapper:
// a WebCookie web origin resolves to a webexec-backed client; other
// origins keep the base factory.
func TestWebExecClientFactoryRoutesWebCookie(t *testing.T) {
	srv := New(nil)
	baseCalled := false
	base := func(baseURL, apiKey string, authHeaders map[string]string, wireFormat string) (provider.Client, error) {
		baseCalled = true
		return nil, errors.New("base factory used")
	}

	factory := WebExecClientFactory(base, srv.WebExecSessionStore())

	// grok.com is grok-web's origin → webexec client (never the base).
	client, err := factory("https://grok.com", "ignored-key", nil, "")
	if err != nil {
		t.Fatalf("web-cookie origin: %v", err)
	}
	if baseCalled {
		t.Fatal("base factory must not be used for a web-cookie origin")
	}
	if client == nil {
		t.Fatal("nil client")
	}
	// The webexec client's liveness check is session presence.
	if err := client.TestKey(context.Background()); !errors.Is(err, webexec.ErrNoSession) {
		t.Fatalf("no session yet → ErrNoSession, got: %v", err)
	}

	// A non-web origin goes to the base factory.
	_, err = factory("https://api.openai.com", "sk-x", nil, "")
	if !baseCalled {
		t.Fatal("base factory must handle non-web origins")
	}
	if err == nil {
		t.Fatal("base factory error should surface")
	}
}

// TestWebExecClientFactoryOriginWithPath proves the origin match for
// the m365 surface (its WebOrigin carries a path component).
func TestWebExecClientFactoryOriginWithPath(t *testing.T) {
	srv := New(nil)
	factory := WebExecClientFactory(nil, srv.WebExecSessionStore())
	client, err := factory("https://m365.cloud.microsoft/chat", "", nil, "")
	if err != nil {
		t.Fatalf("m365 origin with path: %v", err)
	}
	if client == nil {
		t.Fatal("nil client")
	}
}

// TestWebExecBridgeFactoryEndToEnd drives the FULL wiring against a
// mock web surface through the server bridge: import session → factory
// → webexec client → Chat with the cookie replayed. (The mock runs on
// loopback; the executor is created with explicit mock options via the
// webexec package — the bridge factory itself needs no origin override
// because NewWebExecClient takes options only through webexec.New, so
// this test exercises the executor directly with the bridge store.)
func TestWebExecBridgeFactoryEndToEnd(t *testing.T) {
	srv := New(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("mocksid")
		if err != nil || cookie.Value != "e2e-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"stale"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"bridge e2e ok","model":"bridge-test"}`))
	})
	surface := httptest.NewServer(mux)
	defer surface.Close()

	store := srv.WebExecSessionStore()
	if err := store.Save("copilot-web", &webexec.SessionState{
		Cookies: []webexec.Cookie{{Name: "mocksid", Value: "e2e-token", Domain: hostOf(surface.URL), Path: "/"}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Executor pointed at the mock surface over the SERVER's store.
	ex, err := webexec.New("copilot-web", store,
		webexec.WithBaseURL(surface.URL), webexec.WithMockContract(), webexec.WithHTTPClient(surface.Client()))
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	res, err := ex.Execute(context.Background(), webexec.ExecuteRequest{Prompt: "through the bridge"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Content != "bridge e2e ok" {
		t.Fatalf("content = %q", res.Content)
	}
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
