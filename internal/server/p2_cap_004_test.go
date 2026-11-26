package server

// P2-CAP-004: /dashboard/memstats pprof snapshots (?profile=heap,
// ?profile=goroutine) require session when operator password is set;
// JSON counters feed remains AuthExempt.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestP2_CAP_004_MemstatsProfileAuth verifies that ?profile=* endpoints
// require a session when an operator password is set, while the JSON
// counters feed remains open.
func TestP2_CAP_004_MemstatsProfileAuth(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})

	// Test 1: No password set -> profile endpoints stay open
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dashboard/memstats", srv.handleDashboardMemstats)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/memstats?profile=heap", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("profile=heap without password: status = 401, want non-401 (auth-relaxed)")
	}

	// Test 2: JSON counters feed (no profile) always open
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/dashboard/memstats", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("memstats without profile: status = 401, want non-401 (always open)")
	}

	// Test 3: With password set and no session -> profile endpoint returns 401
	dst, err := srv.dashboardAuth()
	if err != nil {
		t.Fatalf("dashboardAuth: %v", err)
	}
	if err := dst.SetPassword("operator-secret"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/dashboard/memstats?profile=heap", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("profile=heap with password, no session: status = %d, want 401", rec.Code)
	}

	// Test 4: With password set and session -> profile endpoint works
	// Create a session cookie
	tok, _, err := dst.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/dashboard/memstats?profile=heap", nil)
	req.Header.Set("Cookie", "codvian_dash_session="+tok)
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("profile=heap with password + session: status = 401, want non-401 (auth passed)")
	}

	// Test 5: With password set and gateway key -> profile endpoint works
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/dashboard/memstats?profile=heap", nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("profile=heap with password + gateway key: status = 401, want non-401 (auth passed)")
	}
}
