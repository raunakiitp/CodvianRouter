// Live-mux registration tests for the 14 orphaned routes wired by
// the registrations gap fix (audit 87a0056f). The handler-level
// tests prove the handlers work; these tests prove the routes are
// actually reachable over HTTP: every request goes through
// srv.Handler() — the real mux built by New(), so register() +
// guard auth/scope apply exactly as in production. A missing
// register() line or guard row shows up here as 404 (or a
// startup panic), not as a green handler test.
//
// Coverage per route: admin → success status (201/200); no key →
// 401; wrong scope → 403. B3 test-run additionally asserts the
// no-spend guarantee in-band through the live route.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// liveReq sends one request through the server's real mux
// (srv.Handler()), i.e. through register()/guard, not straight at
// a handler function.
func liveReq(t *testing.T, srv *Server, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr strings.Reader
	if body != "" {
		rdr = *strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, &rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// liveKey issues a gateway key with the given scopes into cfg and
// returns its raw value.
func liveKey(t *testing.T, cfg *config.Config, name string, scopes []string) string {
	t.Helper()
	key, raw, err := config.IssueKey(name, scopes, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestRegistrationsKeyCreateLive: POST /admin/keys through the mux.
func TestRegistrationsKeyCreateLive(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))

	// No key → 401 (Tier 2 always-protected).
	rec := liveReq(t, srv, http.MethodPost, "/admin/keys", `{"name":"live-created","scopes":["chat"]}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	// Chat-only key → 403 (admin scope required).
	rec = liveReq(t, srv, http.MethodPost, "/admin/keys", `{"name":"live-created","scopes":["chat"]}`, s.raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("chat-only key: status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	// Admin key → 201, raw key returned once, never the hash.
	rec = liveReq(t, srv, http.MethodPost, "/admin/keys", `{"name":"live-created","scopes":["chat"]}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin key: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v: %s", err, rec.Body.String())
	}
	if created.KeyID == "" || created.GatewayKey == "" || !strings.HasPrefix(created.GatewayKey, "crk_") {
		t.Fatalf("create response = %+v, want a key_id and a crk_ raw key", created)
	}
	if !created.PersistedToConfig {
		t.Error("create response persisted_to_config = false, want true (persist wired)")
	}
}

// TestRegistrationsKeyImportAndTemplatesLive: POST
// /admin/keys/import + GET /admin/key-templates through the mux.
func TestRegistrationsKeyImportAndTemplatesLive(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))

	_, raw, err := config.IssueKey("external-live", []string{"chat", "models"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	importBody := `{"name":"imported-live","key":"` + raw + `","scopes":["chat","models"]}`

	// Import: no key → 401; chat-only → 403; admin → 201.
	rec := liveReq(t, srv, http.MethodPost, "/admin/keys/import", importBody, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("import no key: status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodPost, "/admin/keys/import", importBody, s.raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("import chat-only key: status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodPost, "/admin/keys/import", importBody, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import admin key: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), raw) {
		t.Fatal("import response echoes the raw key")
	}

	// Templates: no key → 401; chat-only → 403; admin → 200.
	rec = liveReq(t, srv, http.MethodGet, "/admin/key-templates", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("templates no key: status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodGet, "/admin/key-templates", "", s.raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("templates chat-only key: status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodGet, "/admin/key-templates", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("templates admin key: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var tpl struct {
		Templates []keyTemplate `json:"templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tpl); err != nil {
		t.Fatalf("decode templates: %v: %s", err, rec.Body.String())
	}
	if len(tpl.Templates) == 0 {
		t.Fatal("templates list is empty, want the built-in templates")
	}
}

// TestRegistrationsTelemetryLive: GET
// /api/telemetry/model-latency-stats through the mux. The guard
// row is AuthExempt, so no key is needed; a nil collector still
// answers 503, never an empty list.
func TestRegistrationsTelemetryLive(t *testing.T) {
	srv, _ := latencyStatsStack(t)
	rec := liveReq(t, srv, http.MethodGet, "/api/telemetry/model-latency-stats", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("no key: status = %d, want 200 (AuthExempt): %s", rec.Code, rec.Body.String())
	}
	var out modelLatencyStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode latency stats: %v: %s", err, rec.Body.String())
	}
	if out.Count != 2 || len(out.Stats) != 2 {
		t.Fatalf("count/stats = %d/%d, want 2/2", out.Count, len(out.Stats))
	}

	off := newTestServer(t, &fakeEngine{}, WithTelemetryCollector(nil))
	rec = liveReq(t, off, http.MethodGet, "/api/telemetry/model-latency-stats", "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil collector: status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

// TestRegistrationsFallbackChainsLive: GET/POST/DELETE
// /api/fallback/chains through the mux, against a real
// *router.Engine store.
func TestRegistrationsFallbackChainsLive(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	chatRaw := liveKey(t, cfg, "chat", []string{"chat"})
	adminRaw := liveKey(t, cfg, "admin", []string{"chat", "models", "admin"})
	eng := router.NewEngine(nil, nil, router.Config{})
	srv := newTestServer(t, eng, WithConfig(cfg))

	// No key → 401; chat-only key → 403 (admin scope).
	rec := liveReq(t, srv, http.MethodGet, "/api/fallback/chains", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET no key: status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodGet, "/api/fallback/chains", "", chatRaw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET chat-only key: status = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// Admin: empty list → 200 with 0 chains.
	rec = liveReq(t, srv, http.MethodGet, "/api/fallback/chains", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET empty: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var list fallbackChainsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Chains) != 0 {
		t.Fatalf("GET empty = %+v, %v, want 0 chains", list, err)
	}

	// POST create → 201; GET ?model= → 200; DELETE → 200;
	// DELETE again → 404.
	rec = liveReq(t, srv, http.MethodPost, "/api/fallback/chains", `{"model": "gpt-4o", "entries": [{"provider": "b", "priority": 1}, {"provider": "a", "priority": 0}]}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST create: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodGet, "/api/fallback/chains?model=gpt-4o", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET one: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodDelete, "/api/fallback/chains?model=gpt-4o", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodDelete, "/api/fallback/chains?model=gpt-4o", "", adminRaw)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing: status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestRegistrationsComboB3Live: all 7 B3 combos verbs through the
// mux, over a config with the mock providers the combo fixtures
// use. Export/test-run take the models scope; the mutations take
// admin.
func TestRegistrationsComboB3Live(t *testing.T) {
	cfg := comboTestConfig()
	modelsRaw := liveKey(t, cfg, "models", []string{"chat", "models"})
	adminRaw := liveKey(t, cfg, "admin", []string{"chat", "models", "admin"})
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))

	// No key → 401 on a read route; models key (no admin) → 403
	// on a mutation route.
	rec := liveReq(t, srv, http.MethodGet, "/v1/combos/export", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("export no key: status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodPost, "/v1/combos/x/duplicate", `{"name":"copy"}`, modelsRaw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("duplicate models-only key: status = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// Seed one combo through the already-registered CRUD route.
	rec = liveReq(t, srv, http.MethodPost, "/v1/combos", `{"name":"mock-src","chain":["mock-upstream/mock-model@low","mock-local"],"strategy":"cheapest"}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed mock-src: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	// And a 3-step combo for the test-run simulation.
	rec = liveReq(t, srv, http.MethodPost, "/v1/combos", `{"name":"sim","chain":["mock-upstream/mock-model","mock-keyless","mock-local"],"strategy":"cheapest"}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed sim: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	// GET /v1/combos/export (models scope) → 200, both combos.
	rec = liveReq(t, srv, http.MethodGet, "/v1/combos/export", "", modelsRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("export all: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var all struct {
		Combos []ComboExport `json:"combos"`
		Count  int           `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode export all: %v: %s", err, rec.Body.String())
	}
	if all.Count != 2 || len(all.Combos) != 2 {
		t.Fatalf("export all count = %d, want 2", all.Count)
	}

	// GET /v1/combos/{id}/export → 200; unknown id → 404.
	rec = liveReq(t, srv, http.MethodGet, "/v1/combos/mock-src/export", "", modelsRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("export one: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var one ComboExport
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
		t.Fatalf("decode export one: %v: %s", err, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodGet, "/v1/combos/no-such/export", "", modelsRaw)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("export unknown: status = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	// POST /v1/combos/{id}/duplicate (admin) → 201.
	rec = liveReq(t, srv, http.MethodPost, "/v1/combos/mock-src/duplicate", `{"name":"mock-copy"}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("duplicate: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	// POST /v1/combos/{id}/test-run (models scope) → 200, and the
	// no-spend guarantee holds through the live route: nothing
	// was called, no quota moved.
	rec = liveReq(t, srv, http.MethodPost, "/v1/combos/sim/test-run", "", modelsRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("test-run: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var run ComboTestRun
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode test-run: %v: %s", err, rec.Body.String())
	}
	if run.WouldCallUpstream || run.UpstreamCalls != 0 || run.QuotaSpent {
		t.Fatalf("test-run: would_call_upstream=%v upstream_calls=%d quota_spent=%v — a simulation must never spend", run.WouldCallUpstream, run.UpstreamCalls, run.QuotaSpent)
	}
	if !run.WouldServe || len(run.Attempts) != 3 {
		t.Fatalf("test-run: would_serve=%v attempts=%d, want true/3", run.WouldServe, len(run.Attempts))
	}

	// POST /v1/combos/{id}/disable then /enable (admin) → 200.
	rec = liveReq(t, srv, http.MethodPost, "/v1/combos/mock-src/disable", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodPost, "/v1/combos/mock-src/enable", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// POST /v1/combos/import: export mock-src, delete it, then
	// import the exported definition → 201; a second import of
	// the same name → 409.
	exportBody := liveReq(t, srv, http.MethodGet, "/v1/combos/mock-src/export", "", modelsRaw).Body.String()
	rec = liveReq(t, srv, http.MethodDelete, "/v1/combos/mock-src", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete mock-src before import: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodPost, "/v1/combos/import", exportBody, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	rec = liveReq(t, srv, http.MethodPost, "/v1/combos/import", exportBody, adminRaw)
	if rec.Code != http.StatusConflict {
		t.Fatalf("import collision: status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}
