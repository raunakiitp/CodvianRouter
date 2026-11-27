package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/webexec"
)

// This file proves the ADD-TIME web-cookie flow end to end, now that
// the ToS risk-acknowledgement gate (A3 item 6) is live on main:
//
//   unacknowledged  → the registry refuses the add (ToSGateError, "add" layer)
//   acknowledged    → the add reaches the webexec WebExecClientFactory,
//                     which resolves the provider to a webexec-backed
//                     client whose liveness check IS session presence
//   no session yet  → the add fails honestly (the executor's TestKey
//                     is ErrNoSession — the operator must import a
//                     captured session first)
//   session stored  → the add SUCCEEDS and the provider's client
//                     executes chat against the web surface over the
//                     stored state: 200 on a live session, upstream
//                     401 on a stale one, RateLimitError(429) on quota.
//
// The registry's ClientFactory signature carries no catalog ID, so the
// add carries a DISPATCH base URL (<origin>/webexec/<catalog-id>, see
// webexec.DispatchURLFor) via CatalogAddOptions.BaseURL — the factory
// parses the ID back out and routes to the right per-ID executor. The
// mock web surface runs on loopback, which is exactly the only place
// the mock-surface contract is allowed to run.

// addTimeMockSurface is the mock provider web UI for the add-time
// flows (same contract as the harness tests' mock).
type addTimeMockSurface struct {
	mu     sync.Mutex
	server *httptest.Server
	token  string
	hits   int
}

func newAddTimeMockSurface() *addTimeMockSurface {
	m := &addTimeMockSurface{token: "addtime-good"}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.hits++
		cookie, err := r.Cookie("mocksid")
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"no session"}`))
			return
		}
		if cookie.Value == "addtime-quota" {
			w.Header().Set("Retry-After", "9")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"quota"}`))
			return
		}
		if cookie.Value != m.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"stale"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"addtime reply","model":"addtime"}`))
	}))
	return m
}

func (m *addTimeMockSurface) URL() string { return m.server.URL }

func (m *addTimeMockSurface) hitCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits
}

// newAddTimeRegistry builds a registry wired with the bridge factory,
// the server's web session store, and an ack function the test flips.
func newAddTimeRegistry(t *testing.T, srv *Server, ack func(id string) bool) *provider.Registry {
	t.Helper()
	return provider.NewRegistry(provider.NewJSONFileStore(filepath.Join(t.TempDir(), "providers.json")),
		provider.WithClientFactory(WebExecClientFactory(nil, srv.WebExecSessionStore())),
		provider.WithToSAcknowledgement(ack),
		provider.WithRegistryAllowPrivateDestinations(true), // loopback mock
	)
}

func addTimeState(token string) *webexec.SessionState {
	return &webexec.SessionState{
		Cookies: []webexec.Cookie{{Name: "mocksid", Value: token, Domain: "127.0.0.1", Path: "/"}},
	}
}

func TestAddTimeWebCookieUnacknowledgedRefused(t *testing.T) {
	srv := New(nil)
	surface := newAddTimeMockSurface()
	defer surface.server.Close()
	reg := newAddTimeRegistry(t, srv, func(string) bool { return false })

	dispatch, err := webexec.DispatchURLFor(surface.URL(), "grok-web")
	if err != nil {
		t.Fatalf("dispatch url: %v", err)
	}
	_, err = reg.AddCatalogWithOptions(context.Background(), "grok-web", "p-b=cookie",
		provider.CatalogAddOptions{BaseURL: dispatch})
	var gate *provider.ToSGateError
	if !errors.As(err, &gate) {
		t.Fatalf("unacknowledged add must fail with ToSGateError, got: %v (%T)", err, err)
	}
	if gate.Layer != "add" {
		t.Fatalf("refusal layer = %q, want add", gate.Layer)
	}
	if surface.hitCount() != 0 {
		t.Fatalf("unacknowledged add must not touch the surface")
	}
}

func TestAddTimeWebCookieAckedWithoutSessionFailsHonest(t *testing.T) {
	srv := New(nil)
	surface := newAddTimeMockSurface()
	defer surface.server.Close()
	reg := newAddTimeRegistry(t, srv, func(string) bool { return true })

	dispatch, err := webexec.DispatchURLFor(surface.URL(), "grok-web")
	if err != nil {
		t.Fatalf("dispatch url: %v", err)
	}
	_, err = reg.AddCatalogWithOptions(context.Background(), "grok-web", "p-b=cookie",
		provider.CatalogAddOptions{BaseURL: dispatch})
	if err == nil {
		t.Fatal("add without a stored session must fail (executor TestKey = ErrNoSession)")
	}
	if !errors.Is(err, webexec.ErrNoSession) {
		t.Fatalf("add failure must carry ErrNoSession, got: %v", err)
	}
	if surface.hitCount() != 0 {
		t.Fatalf("no-session add must not touch the surface")
	}
}

func TestAddTimeWebCookieAckedWithSessionExecutes(t *testing.T) {
	srv := New(nil)
	surface := newAddTimeMockSurface()
	defer surface.server.Close()
	reg := newAddTimeRegistry(t, srv, func(string) bool { return true })

	dispatch, err := webexec.DispatchURLFor(surface.URL(), "grok-web")
	if err != nil {
		t.Fatalf("dispatch url: %v", err)
	}

	// Import the operator's captured session through the bridge.
	if err := srv.WebExecImportSession("grok-web", addTimeState("addtime-good")); err != nil {
		t.Fatalf("import: %v", err)
	}

	p, err := reg.AddCatalogWithOptions(context.Background(), "grok-web", "p-b=cookie",
		provider.CatalogAddOptions{BaseURL: dispatch})
	if err != nil {
		t.Fatalf("acknowledged add with session: %v", err)
	}
	if p.CatalogID != "grok-web" {
		t.Fatalf("added provider CatalogID = %q", p.CatalogID)
	}

	// Chat through the registry's client: it must be the webexec
	// client driving the mock web surface with the stored session.
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("client for added provider: %v", err)
	}
	resp, err := client.Chat(context.Background(), provider.ChatRequest{
		Model:    "grok-web",
		Messages: []provider.Message{{Role: "user", Content: "add-time hello"}},
	})
	if err != nil {
		t.Fatalf("chat through the added provider: %v", err)
	}
	if resp.Content != "addtime reply" {
		t.Fatalf("content = %q", resp.Content)
	}
	if surface.hitCount() != 1 {
		t.Fatalf("surface hits = %d, want 1", surface.hitCount())
	}

	// STALE state: overwrite the session with a token the surface
	// rejects → the next chat is a loud upstream 401.
	if err := srv.WebExecImportSession("grok-web", addTimeState("expired-token")); err != nil {
		t.Fatalf("re-import stale: %v", err)
	}
	_, cerr := client.Chat(context.Background(), provider.ChatRequest{
		Model:    "grok-web",
		Messages: []provider.Message{{Role: "user", Content: "again"}},
	})
	var ue *provider.UpstreamError
	if !errors.As(cerr, &ue) || ue.StatusCode != 401 {
		t.Fatalf("stale session must surface as UpstreamError 401, got: %v (%T)", cerr, cerr)
	}

	// QUOTA: a session the surface answers 429 → RateLimitError with
	// Retry-After preserved (account fallback sees the real signal).
	if err := srv.WebExecImportSession("grok-web", addTimeState("addtime-quota")); err != nil {
		t.Fatalf("re-import quota: %v", err)
	}
	_, qerr := client.Chat(context.Background(), provider.ChatRequest{
		Model:    "grok-web",
		Messages: []provider.Message{{Role: "user", Content: "again"}},
	})
	var rle *provider.RateLimitError
	if !errors.As(qerr, &rle) {
		t.Fatalf("quota must surface as RateLimitError, got: %v (%T)", qerr, qerr)
	}
	if !rle.RetryAfterPresent || rle.RetryAfter != 9*time.Second {
		t.Fatalf("Retry-After must pass through as 9s, got present=%v ra=%v", rle.RetryAfterPresent, rle.RetryAfter)
	}
}

// TestAddTimeDispatchURLRoundTrip pins the dispatch-key contract used
// above: render → parse → same ID; non-dispatch URLs parse back empty.
func TestAddTimeDispatchURLRoundTrip(t *testing.T) {
	for _, id := range webexec.CatalogIDsWithWebExec() {
		u, err := webexec.DispatchURLFor("https://example.test:8443", id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		got, ok := webexec.SurfaceIDForDispatchURL(u)
		if !ok || got != id {
			t.Fatalf("%s: round trip gave (%q,%v)", id, got, ok)
		}
	}
	if _, ok := webexec.SurfaceIDForDispatchURL("https://api.openai.com/v1"); ok {
		t.Fatal("a non-dispatch URL must not parse as a dispatch URL")
	}
	if _, ok := webexec.SurfaceIDForDispatchURL("https://example.test/webexec/not-a-catalog-id"); ok {
		t.Fatal("a dispatch URL with an unknown ID must not parse")
	}
	if _, err := webexec.DispatchURLFor("https://example.test", "not-a-catalog-id"); err == nil {
		t.Fatal("DispatchURLFor must refuse unknown catalog IDs")
	}
	if !webexec.IsLoopbackOrigin("http://127.0.0.1:1/x") || webexec.IsLoopbackOrigin("https://grok.com") {
		t.Fatal("loopback detection wrong")
	}
}

// TestAddTimeAll37DispatchRoutes proves EVERY one of the 37 IDs can be
// carried through an add via its dispatch URL: the factory resolves
// each to a webexec client (with no session stored → honest
// ErrNoSession liveness check, never a fabricated provider).
func TestAddTimeAll37DispatchRoutes(t *testing.T) {
	srv := New(nil)
	surface := newAddTimeMockSurface()
	defer surface.server.Close()
	factory := WebExecClientFactory(nil, srv.WebExecSessionStore())
	for _, id := range webexec.CatalogIDsWithWebExec() {
		dispatch, err := webexec.DispatchURLFor(surface.URL(), id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		client, err := factory(dispatch, "cookie", map[string]string{"Cookie": "mocksid=x"}, "unknown")
		if err != nil {
			t.Fatalf("%s: factory: %v", id, err)
		}
		err = client.TestKey(context.Background())
		if !errors.Is(err, webexec.ErrNoSession) {
			t.Fatalf("%s: TestKey without session must be ErrNoSession, got: %v", id, err)
		}
	}
}
