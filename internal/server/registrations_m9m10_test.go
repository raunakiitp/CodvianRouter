// Registration tests for the M9.9 status route + B4 audit + B5
// webhooks PUT + B6 resilience slice (8 routes, one pass).
//
// These tests prove REGISTRATION + GUARD behaviour only, over the
// live server (ServeHTTP): each route answers with an admin key,
// 401 keyless, 403 for a non-admin key; the status shape; the PUT
// webhook round-trip; the resilience settings read. Handler logic
// itself is already covered by the slice tests (a2a_status_test.go,
// audit_test.go, webhooks_api_test.go, resilience_api_test.go)
// and is NOT re-tested here.
//
// Auth setup: keyBudgetSetup issues a chat-only key (s.raw) and
// adminKeyFor adds a second key with the admin scope, both verified
// through the config verifier — so scope enforcement is real here.
// (newTestServer's static "test-key-123" carries no scope metadata
// and requireScope passes it through, so it cannot prove a 403.)
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// regM9M10Setup builds a live server with an admin key and a
// chat-only (non-admin) key. a2aEnabled controls the A2A flag the
// status route captures at registration time.
func regM9M10Setup(t *testing.T, a2aEnabled bool) (*Server, *keyBudgetSetup, string, string) {
	t.Helper()
	s := newKeyBudgetSetup(t)
	s.cfg.A2AEnabled = a2aEnabled
	adminRaw := adminKeyFor(t, s)
	return s.server(t), s, adminRaw, s.raw
}

// regM9M10Routes is the eight routes this slice registers, with
// the status the admin key must get (200, or the honest non-auth
// status the handler returns for the probe body used here).
var regM9M10Routes = []struct {
	method, path, body string
	adminWant          int
}{
	{"GET", "/api/a2a/status", "", http.StatusOK},
	{"GET", "/dashboard/audit", "", http.StatusOK},
	{"PUT", "/api/webhooks/wh_does-not-exist", `{}`, http.StatusNotFound},
	{"GET", "/api/resilience/settings", "", http.StatusOK},
	{"PATCH", "/api/resilience/settings", `{}`, http.StatusBadRequest},
	// The test engine implements neither breakerResetter nor
	// cooldownReporter, so the honest production answers apply:
	// 501 (see resilience_api.go) — registration is proven by
	// reaching the handler at all, not by a fake reset.
	{"POST", "/api/resilience/breakers/reset", `{"all":true}`, http.StatusNotImplemented},
	{"POST", "/api/resilience/cooldowns", `{"model":"openai/gpt-5","mode":"hard"}`, http.StatusNotImplemented},
	{"DELETE", "/api/resilience/cooldowns/openai/gpt-5", "", http.StatusNotImplemented},
}

func TestRegistrationsM9M10GuardRows(t *testing.T) {
	want := map[string]struct {
		tier   GuardTier
		scopes []string
	}{
		"GET /api/a2a/status":                         {TierManagement, []string{"admin"}},
		"GET /dashboard/audit":                        {TierManagement, []string{"admin"}},
		"PUT /api/webhooks/{id}":                      {TierAlwaysProtected, []string{"admin"}},
		"GET /api/resilience/settings":                {TierManagement, []string{"admin"}},
		"PATCH /api/resilience/settings":              {TierManagement, []string{"admin"}},
		"POST /api/resilience/breakers/reset":         {TierManagement, []string{"admin"}},
		"POST /api/resilience/cooldowns":              {TierManagement, []string{"admin"}},
		"DELETE /api/resilience/cooldowns/{model...}": {TierManagement, []string{"admin"}},
	}
	seen := map[string]bool{}
	for _, r := range RouteTable() {
		key := r.Method + " " + r.Path
		w, ok := want[key]
		if !ok {
			continue
		}
		seen[key] = true
		if r.Tier != w.tier {
			t.Errorf("route %s tier = %s, want %s", key, r.Tier, w.tier)
		}
		if !reflect.DeepEqual(r.Scopes, w.scopes) {
			t.Errorf("route %s scopes = %v, want %v", key, r.Scopes, w.scopes)
		}
		if r.AuthExempt {
			t.Errorf("route %s must not be auth-exempt", key)
		}
		if r.Description == "" {
			t.Errorf("route %s has no description", key)
		}
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("route %s is missing from the guard route table", key)
		}
	}
}

// TestRegistrationsM9M10AuthMatrix proves the guard behaviour of
// all eight routes over the live server: 401 keyless, 403 for a
// valid but non-admin key, and the handler's own (non-auth) status
// for the admin key.
func TestRegistrationsM9M10AuthMatrix(t *testing.T) {
	srv, _, adminRaw, chatRaw := regM9M10Setup(t, true)
	for _, rt := range regM9M10Routes {
		// Keyless: 401.
		if rec := doReq(t, srv, rt.method, rt.path, rt.body, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s keyless: status %d, want 401 (body %q)", rt.method, rt.path, rec.Code, rec.Body.String())
		}
		// Non-admin key: 403 (auth passes, scope fails).
		if rec := doReq(t, srv, rt.method, rt.path, rt.body, chatRaw); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s non-admin: status %d, want 403 (body %q)", rt.method, rt.path, rec.Code, rec.Body.String())
		}
		// Admin key: the handler's own status — never 401/403.
		rec := doReq(t, srv, rt.method, rt.path, rt.body, adminRaw)
		if rec.Code != rt.adminWant {
			t.Errorf("%s %s admin: status %d, want %d (body %q)", rt.method, rt.path, rec.Code, rt.adminWant, rec.Body.String())
		}
	}
}

// TestRegistrationsM9M10StatusEnabled proves the registered status
// route serves the live shape over the server (enabled): the exact
// six skills, the protocol version, all five state keys, and a
// REAL count — one task seeded through the live service reads
// back as one submitted task.
func TestRegistrationsM9M10StatusEnabled(t *testing.T) {
	srv, _, adminRaw, _ := regM9M10Setup(t, true)
	svc, err := srv.a2aService()
	if err != nil {
		t.Fatalf("a2aService: %v", err)
	}
	if _, err := svc.CreateTask("owner-seed", json.RawMessage(`{"role":"user","content":"hi"}`)); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	rec := doReq(t, srv, http.MethodGet, "/api/a2a/status", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/a2a/status admin: status %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var out a2aStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status response is not JSON: %v: %s", err, rec.Body.String())
	}
	if !out.Enabled {
		t.Error("enabled = false, want true on an A2A-enabled server")
	}
	wantSkills := []string{"smart-routing", "quota-management", "provider-discovery",
		"cost-analysis", "health-report", "list-capabilities"}
	if !reflect.DeepEqual(out.Skills, wantSkills) {
		t.Errorf("skills = %v, want %v", out.Skills, wantSkills)
	}
	if out.ProtocolVersion != "2025-03-26" {
		t.Errorf("protocol_version = %q, want 2025-03-26", out.ProtocolVersion)
	}
	if len(out.TasksByState) != 5 {
		t.Errorf("tasks_by_state has %d keys, want 5", len(out.TasksByState))
	}
	if out.TasksByState["submitted"] != 1 {
		t.Errorf("tasks_by_state[submitted] = %d, want 1 (the seeded task)", out.TasksByState["submitted"])
	}
	for _, st := range []string{"working", "completed", "failed", "cancelled"} {
		if out.TasksByState[st] != 0 {
			t.Errorf("tasks_by_state[%s] = %d, want 0", st, out.TasksByState[st])
		}
	}
}

// TestRegistrationsM9M10StatusDisabledNoStore proves the disabled
// contract over the live server: enabled:false with all-zero
// counts — and NO tasks.db is created while A2A is off.
func TestRegistrationsM9M10StatusDisabledNoStore(t *testing.T) {
	srv, s, adminRaw, _ := regM9M10Setup(t, false)
	rec := doReq(t, srv, http.MethodGet, "/api/a2a/status", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/a2a/status admin (disabled): status %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var out a2aStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status response is not JSON: %v: %s", err, rec.Body.String())
	}
	if out.Enabled {
		t.Error("enabled = true, want false on an A2A-disabled server")
	}
	for st, n := range out.TasksByState {
		if n != 0 {
			t.Errorf("tasks_by_state[%s] = %d, want 0 while disabled", st, n)
		}
	}
	if len(out.Skills) != 6 || out.ProtocolVersion != "2025-03-26" {
		t.Errorf("disabled shape: skills %v protocol %q, want 6 skills + 2025-03-26", out.Skills, out.ProtocolVersion)
	}
	dbPath := filepath.Join(s.cfg.DataDir, "a2a", "tasks.db")
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("tasks.db exists at %s after a disabled status call (err %v) — the disabled route must never touch the store", dbPath, err)
	}
	// The opener was never invoked either: a2aService must still
	// refuse while disabled.
	if _, err := srv.a2aService(); err == nil {
		t.Error("a2aService() succeeded on a disabled server — the store must stay closed")
	}
}

// TestRegistrationsM9M10WebhookPutRoundTrip proves PUT
// /api/webhooks/{id} over the live server: create, partial-update
// the URL only (events untouched), read back, unknown id 404.
func TestRegistrationsM9M10WebhookPutRoundTrip(t *testing.T) {
	srv, _, adminRaw, _ := regM9M10Setup(t, false)
	rec := doReq(t, srv, http.MethodPost, "/api/webhooks", `{"url":"https://93.184.215.10/hook","events":["quota.exceeded"]}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create webhook: status %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created webhookView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create response is not JSON: %v: %s", err, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodPut, "/api/webhooks/"+created.ID, `{"url":"https://93.184.215.11/hook2"}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT webhook: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var updated webhookView
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("PUT response is not JSON: %v: %s", err, rec.Body.String())
	}
	if updated.URL != "https://93.184.215.11/hook2" {
		t.Errorf("updated url = %q, want https://93.184.215.11/hook2", updated.URL)
	}
	if len(updated.Events) != 1 || updated.Events[0] != "quota.exceeded" {
		t.Errorf("updated events = %v, want [quota.exceeded] (partial update must keep unprovided fields)", updated.Events)
	}
	if strings.Contains(rec.Body.String(), `"secret"`) {
		t.Errorf("PUT response contains a secret field: %s", rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/api/webhooks/"+created.ID, "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after PUT: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var one webhookView
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
		t.Fatalf("GET response is not JSON: %v: %s", err, rec.Body.String())
	}
	if one.URL != "https://93.184.215.11/hook2" {
		t.Errorf("GET url = %q, want the updated URL persisted in the live server", one.URL)
	}
}

// TestRegistrationsM9M10ResilienceSettingsRead proves GET
// /api/resilience/settings over the live server: the live shape,
// the three unbacked settings listed, and reset_supported=false
// against the fake test engine (which, unlike router.Engine,
// exposes no breaker reset — the dashboard must render reset
// unavailable).
func TestRegistrationsM9M10ResilienceSettingsRead(t *testing.T) {
	srv, _, adminRaw, _ := regM9M10Setup(t, false)
	rec := doReq(t, srv, http.MethodGet, "/api/resilience/settings", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/resilience/settings admin: status %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var out resilienceSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("settings response is not JSON: %v: %s", err, rec.Body.String())
	}
	if out.CooldownMode == "" {
		t.Error("cooldown_mode is empty, want the effective global mode")
	}
	if out.Breakers == nil || out.Cooling == nil || out.Chains == nil {
		t.Errorf("settings feed has nil sections: breakers %v cooling %v chains %v", out.Breakers, out.Cooling, out.Chains)
	}
	if out.ResetSupported {
		t.Error("reset_supported = true, want false: the fake test engine exposes no breaker reset (router.Engine does implement ResetBreaker/ResetAllBreakers — this assertion is about the fake engine only)")
	}
	wantUnsupported := []string{"breaker_threshold", "breaker_open_for_seconds", "cooldown_seconds"}
	if len(out.Unsupported) != len(wantUnsupported) {
		t.Fatalf("unsupported = %d entries, want %d (%v)", len(out.Unsupported), len(wantUnsupported), wantUnsupported)
	}
	for i, f := range wantUnsupported {
		if out.Unsupported[i].Field != f {
			t.Errorf("unsupported[%d].field = %q, want %q", i, out.Unsupported[i].Field, f)
		}
		if out.Unsupported[i].Reason == "" {
			t.Errorf("unsupported[%d] has no reason", i)
		}
	}
}

// TestRegistrationsM9M10StatusUsesOneStore proves the registered
// status route reuses the server's lazily-opened service: the
// store the status feed counts is the SAME store a2aService
// returns (no second store is opened by registration).
func TestRegistrationsM9M10StatusUsesOneStore(t *testing.T) {
	srv, _, adminRaw, _ := regM9M10Setup(t, true)
	svc, err := srv.a2aService()
	if err != nil {
		t.Fatalf("a2aService: %v", err)
	}
	if svc.Store() == nil {
		t.Fatal("Service.Store() = nil, want the live store")
	}
	// Seed through the service, read through the registered route.
	if _, err := svc.CreateTask("owner-one-store", json.RawMessage(`{"role":"user","content":"hi"}`)); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	rec := doReq(t, srv, http.MethodGet, "/api/a2a/status", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/a2a/status: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out a2aStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status response is not JSON: %v: %s", err, rec.Body.String())
	}
	if out.TasksByState["submitted"] != 1 {
		t.Errorf("status counts submitted = %d, want 1 — the route must count the same store a2aService opened", out.TasksByState["submitted"])
	}
	// And the A2A task endpoint's owner for this key is derived
	// from the key id, never from the seed owner — the counts are
	// operator aggregates, so no owner id may leak.
	if strings.Contains(rec.Body.String(), "owner-one-store") {
		t.Errorf("status response leaks the seed owner: %s", rec.Body.String())
	}
}
