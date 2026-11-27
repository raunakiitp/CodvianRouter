package webexec

import (
	"context"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/catalog"
)

// TestSurfaceRegistryCoversCatalogIDs pins the surface registry to the
// catalog: every catalog WebCookie ID has a surface descriptor, no
// extra IDs exist, and the count is exactly 37 (the spec's set — never
// fewer, and an added WebCookie entry must come with its executor).
func TestSurfaceRegistryCoversCatalogIDs(t *testing.T) {
	var webCookieIDs []string
	for _, p := range catalog.All() {
		if p.Category == catalog.CategoryWebCookie {
			webCookieIDs = append(webCookieIDs, p.ID)
		}
	}
	if len(webCookieIDs) != 37 {
		t.Fatalf("catalog has %d WebCookie entries, want 37 — the surface registry must follow the catalog", len(webCookieIDs))
	}
	got := map[string]bool{}
	for _, id := range CatalogIDsWithWebExec() {
		got[id] = true
		if _, ok := surfaceByID(id); !ok {
			t.Fatalf("surface %s listed but not registered", id)
		}
	}
	if len(got) != 37 {
		t.Fatalf("registry has %d surfaces, want 37", len(got))
	}
	for _, id := range webCookieIDs {
		if !got[id] {
			t.Errorf("catalog WebCookie ID %q has NO webexec surface — never leave an ID behind", id)
		}
	}
	for id := range got {
		if _, ok := catalog.ByID(id); !ok {
			t.Errorf("registry surface %q is not a catalog ID", id)
		}
	}
}

// TestSurfaceRegistryWebOriginsMatchCatalog pins each surface's
// WebOrigin to the catalog entry's Website (no invented hosts).
func TestSurfaceRegistryWebOriginsMatchCatalog(t *testing.T) {
	for _, id := range CatalogIDsWithWebExec() {
		s := surfaces[id]
		entry, ok := catalog.ByID(id)
		if !ok {
			t.Fatalf("%s: not in catalog", id)
		}
		if s.WebOrigin != entry.Website {
			t.Errorf("%s: WebOrigin %q != catalog Website %q", id, s.WebOrigin, entry.Website)
		}
		if s.Name == "" {
			t.Errorf("%s: missing Name", id)
		}
		// Honesty rule: a documented endpoint claim must carry a
		// source; undocumented ones must carry a TODO note.
		if s.EndpointDocumented && s.EndpointSource == "" {
			t.Errorf("%s: claims EndpointDocumented without a source", id)
		}
		if !s.EndpointDocumented && s.ChatEndpoint != "" && s.EndpointNote == "" {
			t.Errorf("%s: records a community endpoint without an explanatory note", id)
		}
	}
}

// TestSurfaceRegistryNeverInventsLiveURLs is the honesty gate: an
// executor pointed at a REAL provider origin must refuse to execute
// unless the endpoint is officially documented — for every one of the
// 37 surfaces. No invented URLs, ever.
func TestSurfaceRegistryNeverInventsLiveURLs(t *testing.T) {
	for _, id := range CatalogIDsWithWebExec() {
		store := NewSessionStore(nil)
		if err := store.Save(id, testState("whatever")); err != nil {
			t.Fatalf("%s: save: %v", id, err)
		}
		ex, err := New(id, store) // production wiring: real origin, no mock
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		_, err = ex.Execute(context.Background(), ExecuteRequest{Prompt: "hi"})
		if !errors.Is(err, ErrEndpointUndocumented) {
			t.Errorf("%s: live execution must refuse with ErrEndpointUndocumented until the endpoint is officially documented, got: %v", id, err)
		}
	}
}

// TestCaptureSeamWithoutBrowser is the documented-limitation proof:
// with no browser factory registered, capture refuses with
// ErrNoBrowser — it never fakes a session.
func TestCaptureSeamWithoutBrowser(t *testing.T) {
	_, err := CaptureSessionState(context.Background(), "no-such-driver", "chatgpt-web", nil)
	if !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("unregistered driver must answer ErrNoBrowser: %v", err)
	}
}

// mockDriver is the fake headless browser used to prove the
// BrowserSessionFactory seam is pluggable end to end.
type mockDriver struct{ surface *mockWebSurface }

func (m mockDriver) ImportStorageState(_ context.Context, origin string) (*SessionState, error) {
	state := m.surface.sessionStateFor(mockValidToken)
	if origin != "" && !sameOrigin(origin, state.Origins[0].Origin) {
		state.Origins[0].Origin = origin
	}
	return state, nil
}
func (m mockDriver) Close() error { return nil }

func sameOrigin(a, b string) bool { return a == b }

// TestCaptureSeamRegistersDriver registers a fake driver, captures a
// session through the seam, and executes through the resulting state.
func TestCaptureSeamRegistersDriver(t *testing.T) {
	surface := newMockWebSurface()
	defer surface.close()
	RegisterBrowserFactory("mockdriver-test", func(_ context.Context) (BrowserSession, error) {
		return mockDriver{surface: surface}, nil
	})

	state, err := CaptureSessionState(context.Background(), "mockdriver-test", "chatgpt-web", nil)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("captured state invalid: %v", err)
	}

	store := NewSessionStore(nil)
	if err := store.Save("chatgpt-web", state); err != nil {
		t.Fatalf("save: %v", err)
	}
	ex, err := New("chatgpt-web", store, WithBaseURL(surface.URL()), WithMockContract(), WithHTTPClient(surface.server.Client()))
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	res, err := ex.Execute(context.Background(), ExecuteRequest{Prompt: "captured?"})
	if err != nil {
		t.Fatalf("execute after capture: %v", err)
	}
	if res.Content == "" {
		t.Fatal("empty content")
	}
}
