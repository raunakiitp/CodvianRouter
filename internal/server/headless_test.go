package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Headless mode (CHECKLIST 197-198): a headless gateway is a pure
// API process — every /dashboard* route 404s, while /v1/*, /metrics
// and /healthz keep working. The refusal to start headless without
// a gateway key lives in cmd runServe (before the stack is built);
// these tests cover the server half: no dashboard is served.
func TestHeadlessDashboard404(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithHeadless(true))
	for _, path := range []string{"/dashboard", "/dashboard/overview", "/dashboard/mcp", "/dashboard/guard", "/dashboard/headless"} {
		rec := doReq(t, srv, http.MethodGet, path, "", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s headless: status = %d, want 404", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "headless") {
			t.Fatalf("GET %s headless: body %q does not say headless", path, rec.Body.String())
		}
	}
}

func TestHeadlessAPIStillWorks(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithHeadless(true))
	// Liveness stays up in headless mode.
	if rec := doReq(t, srv, http.MethodGet, "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz headless: status = %d, want 200", rec.Code)
	}
	// /v1/models still requires a gateway key (auth is ON in
	// headless mode — the mode never relaxes it).
	if rec := doReq(t, srv, http.MethodGet, "/v1/models", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/models without key headless: status = %d, want 401", rec.Code)
	}
	if rec := doReq(t, srv, http.MethodGet, "/v1/models", "", "test-key-123"); rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models with key headless: status = %d, want 200", rec.Code)
	}
}

func TestHeadlessOffByDefault(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	if srv.Headless() {
		t.Fatal("Headless() = true on a server built without WithHeadless")
	}
	if rec := doReq(t, srv, http.MethodGet, "/dashboard", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard non-headless: status = %d, want 200", rec.Code)
	}
}

func TestHeadlessFeed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/headless", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/headless: status = %d, want 200", rec.Code)
	}
	var resp headlessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET /dashboard/headless: bad JSON: %v", err)
	}
	if resp.Headless {
		t.Fatal("headless = true on a dashboard server (a 200 here can only come from one)")
	}
	if !resp.DashboardEnabled || !resp.WizardAvailable {
		t.Fatalf("feed = %+v, want dashboard_enabled and wizard_available true", resp)
	}
	for _, want := range []string{"serve --headless", "init --wizard", "run <cli> --model"} {
		if !strings.Contains(resp.HeadlessCommand+resp.WizardCommand+resp.RunCommand, want) {
			t.Fatalf("feed commands %+v do not contain %q", resp, want)
		}
	}
	if resp.Note == "" {
		t.Fatal("feed note is empty — the headless limitation must be stated")
	}
}
