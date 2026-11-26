package server

// P2-IDENT-002: exempt feeds outside /dashboard/ now require a dashboard
// session (or gateway key) when an operator password is set, matching the
// B3-04 feed gate already applied to /dashboard/* routes.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestP2_IDENT_002_SessionGatedFeedsOutsideDashboard verifies that
// AuthExempt routes outside /dashboard/ (e.g. /api/providers,
// /api/capabilities, /api/mcp/audit) require a session when a password
// is set, while truly public endpoints (/healthz, /.well-known/agent.json,
// /oauth/token) remain open.
func TestP2_IDENT_002_SessionGatedFeedsOutsideDashboard(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})

	// Simulate an operator password being set.
	dst, err := srv.dashboardAuth()
	if err != nil {
		t.Fatalf("dashboardAuth: %v", err)
	}
	if err := dst.SetPassword("operator-secret"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// Test cases: (path, method, expectSessionRequired)
	cases := []struct {
		path    string
		method  string
		want401 bool // true = session required, false = open
	}{
		{"/api/providers", http.MethodGet, true},
		{"/api/capabilities", http.MethodGet, true},
		{"/api/mcp/audit", http.MethodGet, true},
		{"/api/mcp/status", http.MethodGet, true},
		{"/api/cache/stats", http.MethodGet, true},
		{"/api/telemetry/model-latency-stats", http.MethodGet, true},
		{"/api/pricing", http.MethodGet, true},
		{"/api/catalog", http.MethodGet, true},
		// Truly public — no session required:
		{"/healthz", http.MethodGet, false},
		{"/.well-known/agent.json", http.MethodGet, false},
		{"/oauth/token", http.MethodPost, false},
		{"/openapi.json", http.MethodGet, false},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)

			if tc.want401 {
				if w.Code != http.StatusUnauthorized {
					t.Errorf("%s %s: status = %d, want 401 (session required)", tc.method, tc.path, w.Code)
				}
			} else {
				if w.Code == http.StatusUnauthorized {
					t.Errorf("%s %s: status = %d, want non-401 (public endpoint)", tc.method, tc.path, w.Code)
				}
			}
		})
	}
}
