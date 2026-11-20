package server

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// G15 Area 4 — key creation from templates, key import, per-key
// IP / model / endpoint policy enforcement, device tracking.
//
// Row 1 note: POST /admin/keys' handler (handleKeyCreate) already
// existed (B1, keystatus.go) with handler-level tests; what was
// missing was the template + policy surface on it and the route
// registration (server.go + guard table, coordinator-owned).
// These tests cover the new surface. Row 6 note: GET
// /v1/me/sessions already exists (mesessions.go:51, registered
// server.go:1579, guard.go:335) — TestMeSessionsRouteExists pins
// that, and the M5.6 tests TestMeSessions /
// TestMeSessionsRequiresKey / TestMeSessionsEmpty cover it.

func doKeyImport(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/keys/import", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleKeyImport(rec, req)
	return rec
}

func doKeyTemplates(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/key-templates", nil)
	rec := httptest.NewRecorder()
	srv.handleKeyTemplates(rec, req)
	return rec
}

// setFixturePolicy replaces the fixture key's policy in the setup
// config (the same direct-config pattern TestMeStatusShapeNoLeak
// uses for expiry/limits).
func setFixturePolicy(s *keyBudgetSetup, p config.KeyPolicy) {
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].ApplyPolicy(p)
		}
	}
}

func TestKeyCreateWithTemplateReadOnly(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	_ = adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	// Explicit chat scope: without it the scope gate (not the
	// endpoint gate) refuses chat first, which would prove
	// nothing about the endpoint allowlist. The pure template
	// key (models-only) is refused by scope alone.
	rec := doKeyCreate(t, srv, `{"name":"ro-key","template":"read-only","scopes":["chat","models"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with template: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v: %s", err, rec.Body.String())
	}
	if created.Key.RequestsPerMinute != 60 {
		t.Errorf("template RPM = %d, want 60", created.Key.RequestsPerMinute)
	}
	if len(created.Policy.AllowedEndpoints) == 0 {
		t.Error("read-only template must set an endpoint allowlist in the create response policy")
	}
	// The read-only key cannot spend: chat is refused at the
	// endpoint gate, before any provider is called.
	callsBefore := s.eng.chatCalls.Load()
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), created.GatewayKey, loopbackPeer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("chat with read-only key: status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_endpoint_not_allowed") {
		t.Errorf("403 body %q must name key_endpoint_not_allowed", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != callsBefore {
		t.Errorf("engine Chat calls = %d, want %d — the refused request must never reach a provider", s.eng.chatCalls.Load(), callsBefore)
	}
	// The pure template key (models scope only) is refused even
	// earlier, by the scope gate — layered defence, not a gap.
	rec = doKeyCreate(t, srv, `{"name":"ro-pure","template":"read-only"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pure read-only key: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var pure keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pure); err != nil {
		t.Fatalf("decode pure create response: %v: %s", err, rec.Body.String())
	}
	if len(pure.Key.Scopes) != 1 || pure.Key.Scopes[0] != "models" {
		t.Errorf("pure template scopes = %v, want [models]", pure.Key.Scopes)
	}
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), pure.GatewayKey, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(strings.ToLower(rec.Body.String()), "scope") {
		t.Fatalf("chat with models-only key: status = %d, want 403 naming the scope: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyCreateWithTemplateOverride(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	_ = adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	rec := doKeyCreate(t, srv, `{"name":"std-key","template":"standard","ip_allowlist":["10.0.0.0/8"],"budget_usd":7}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with template+override: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v: %s", err, rec.Body.String())
	}
	if created.Key.BudgetUSD != 7 {
		t.Errorf("explicit budget = %v, want 7 (override beats template)", created.Key.BudgetUSD)
	}
	if created.Key.RequestsPerMinute != 60 {
		t.Errorf("template RPM = %d, want 60 (kept from template)", created.Key.RequestsPerMinute)
	}
	if len(created.Policy.IPAllowlist) != 1 || created.Policy.IPAllowlist[0] != "10.0.0.0/8" {
		t.Errorf("explicit ip_allowlist = %v, want [10.0.0.0/8]", created.Policy.IPAllowlist)
	}
	// The raw key appears exactly once in the whole response.
	if n := strings.Count(rec.Body.String(), created.GatewayKey); n != 1 {
		t.Fatalf("raw key appears %d times in the create response, want exactly 1", n)
	}
}

func TestKeyCreateTemplateValidation(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	_ = adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	for _, body := range []string{
		`{"name":"x","template":"no-such-template"}`,
		`{"name":"x","ip_allowlist":["not-an-ip"]}`,
		`{"name":"x","ip_allowlist":["10.0.0.0/33"]}`,
		`{"name":"x","model_access_mode":"sometimes"}`,
		`{"name":"x","guardrails":["no-such-guardrail"]}`,
	} {
		rec := doKeyCreate(t, srv, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("create %s: status = %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
	}
	if len(s.cfg.GatewayKeys) != 2 { // fixture + admin only
		t.Errorf("failed creates must not store keys: %d keys", len(s.cfg.GatewayKeys))
	}
}

func TestKeyTemplatesList(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	rec := doKeyTemplates(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("templates list: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Templates      []keyTemplate       `json:"templates"`
		NotImplemented []keyPolicyOmission `json:"not_implemented_policy_fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode templates: %v: %s", err, rec.Body.String())
	}
	if len(out.Templates) != 3 {
		t.Fatalf("templates = %d, want 3 (read-only, standard, admin)", len(out.Templates))
	}
	// G15 re-review nit 7: the omission list is surfaced, not
	// just a code comment.
	fields := map[string]bool{}
	for _, o := range out.NotImplemented {
		fields[o.Field] = true
	}
	for _, want := range []string{"machineId / proxyId", "allowCcDiscoveryAliases"} {
		if !fields[want] {
			t.Errorf("templates response omits %q from not_implemented_policy_fields", want)
		}
	}
	names := map[string]bool{}
	for _, tmpl := range out.Templates {
		names[tmpl.Name] = true
	}
	for _, want := range []string{"read-only", "standard", "admin"} {
		if !names[want] {
			t.Errorf("templates missing %q", want)
		}
	}
	if strings.Contains(rec.Body.String(), "crk_") {
		t.Error("templates response must never contain key material")
	}
}

func TestKeyImportRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	_ = adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath), WithLogger(log.New(&strings.Builder{}, "", 0)))
	// An externally-issued key: mint the raw value, discard the
	// record — the import is the only way it enters this server.
	_, raw, err := config.IssueKey("external", []string{"chat", "models"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"name":"imported","key":"` + raw + `","scopes":["chat","models"],"policy":{"ip_allowlist":["10.0.0.0/8"]}}`
	rec := doKeyImport(t, srv, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var imported keyImportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &imported); err != nil {
		t.Fatalf("decode import response: %v: %s", err, rec.Body.String())
	}
	if !imported.Created {
		t.Error("first import must report created=true")
	}
	if want := "sha256:" + config.HashKey(raw)[:12]; imported.Fingerprint != want {
		t.Errorf("fingerprint = %q, want %q", imported.Fingerprint, want)
	}
	// The raw key is NEVER echoed back: the caller supplied it.
	if strings.Contains(rec.Body.String(), raw) {
		t.Fatal("import response echoes the raw key")
	}
	if len(imported.Policy.IPAllowlist) != 1 || imported.Policy.IPAllowlist[0] != "10.0.0.0/8" {
		t.Errorf("imported policy = %+v, want the ip_allowlist", imported.Policy)
	}
	// The imported key authenticates — from an allowlisted peer.
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), raw, "10.1.2.3:1234")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat with imported key: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Idempotent retry: same id + same key → 200, created=false.
	retry := `{"id":"` + imported.KeyID + `","name":"imported","key":"` + raw + `"}`
	rec = doKeyImport(t, srv, retry)
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent re-import: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// G15 re-review nit 11: same id + same key but a different
	// name or scopes is a 409, not a silent "nothing changed".
	// (Separate recorder: `rec` still holds the 200 retry
	// response decoded below.)
	for _, clash := range []string{
		`{"id":"` + imported.KeyID + `","name":"renamed","key":"` + raw + `"}`,
		`{"id":"` + imported.KeyID + `","name":"imported","key":"` + raw + `","scopes":["chat"]}`,
		`{"id":"` + imported.KeyID + `","name":"imported","key":"` + raw + `","policy":{"ip_allowlist":["192.168.0.0/16"]}}`,
	} {
		clashRec := doKeyImport(t, srv, clash)
		if clashRec.Code != http.StatusConflict {
			t.Fatalf("disagreeing re-import: status = %d, want 409: %s", clashRec.Code, clashRec.Body.String())
		}
	}
	var retryOut keyImportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &retryOut); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if retryOut.Created || retryOut.KeyID != imported.KeyID {
		t.Errorf("retry = created=%v id=%q, want created=false id=%q", retryOut.Created, retryOut.KeyID, imported.KeyID)
	}
	// Duplicate credential under another id → 409.
	rec = doKeyImport(t, srv, `{"name":"dup","key":"`+raw+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate-hash import: status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	// Duplicate id with a different key → 409.
	_, otherRaw, err := config.IssueKey("other", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rec = doKeyImport(t, srv, `{"id":"`+imported.KeyID+`","name":"clash","key":"`+otherRaw+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate-id import: status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	// Malformed / missing key → 400.
	for _, bad := range []string{
		`{"name":"x","key":"not-a-key"}`,
		`{"name":"x","key":"crk_short"}`,
		`{"name":"x"}`,
		`{"name":"","key":"` + raw + `"}`,
		`{"name":"x","key":"` + raw + `","policy":{"ip_allowlist":["bogus"]}}`,
	} {
		rec = doKeyImport(t, srv, bad)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("import %s: status = %d, want 400: %s", bad, rec.Code, rec.Body.String())
		}
	}
}

func TestKeyIPAllowlistEnforcement(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{IPAllowlist: []string{"10.0.0.0/8"}})
	srv := s.server(t)
	callsBefore := s.eng.chatCalls.Load()
	rec := doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw, "192.168.1.5:54321")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("chat from non-allowlisted IP: status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_ip_not_allowed") {
		t.Errorf("403 body %q must name key_ip_not_allowed", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != callsBefore {
		t.Errorf("engine Chat calls = %d, want %d — the refused request must never reach a provider", s.eng.chatCalls.Load(), callsBefore)
	}
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw, "10.1.2.3:54321")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat from allowlisted IP: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyIPBlocklistEnforcement(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{IPBlocklist: []string{"192.168.1.5"}})
	srv := s.server(t)
	rec := doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw, "192.168.1.5:54321")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_ip_not_allowed") {
		t.Fatalf("chat from blocklisted IP: status = %d, want 403 key_ip_not_allowed: %s", rec.Code, rec.Body.String())
	}
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw, "10.1.2.3:54321")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat from non-blocklisted IP: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// A key with no lists behaves as before (no 403 anywhere).
	s2, _ := keyStatusSetup(t)
	srv2 := s2.server(t)
	rec = doReqPeer(t, srv2, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), s2.raw, "192.168.1.5:54321")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat with a list-free key: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyModelAllowlistEnforcement(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{ModelAccessMode: "restricted", AllowedModels: []string{"model-a"}})
	srv := s.server(t)
	callsBefore := s.eng.chatCalls.Load()
	rec := doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("model-b", false), s.raw, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_model_not_allowed") {
		t.Fatalf("chat with unlisted model: status = %d, want 403 key_model_not_allowed: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != callsBefore {
		t.Errorf("engine Chat calls = %d, want %d", s.eng.chatCalls.Load(), callsBefore)
	}
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("model-a", false), s.raw, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat with listed model: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyForcedModelEnforcement(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{ForcedModel: "model-a"})
	srv := s.server(t)
	rec := doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("model-b", false), s.raw, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_forced_model") {
		t.Fatalf("chat with a non-pinned model: status = %d, want 403 key_forced_model: %s", rec.Code, rec.Body.String())
	}
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("model-a", false), s.raw, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat with the pinned model: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyEndpointAllowlistEnforcement(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setFixturePolicy(s, config.KeyPolicy{AllowedEndpoints: []string{"/v1/models"}})
	srv := s.server(t)
	rec := doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw, loopbackPeer)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_endpoint_not_allowed") {
		t.Fatalf("chat on a non-allowed endpoint: status = %d, want 403 key_endpoint_not_allowed: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyDevicesRecorded(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	// Two distinct peers → two devices.
	for _, peer := range []string{"10.0.0.1:1111", "10.0.0.2:2222"} {
		rec := doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw, peer)
		if rec.Code != http.StatusOK {
			t.Fatalf("chat from %s: status = %d, want 200: %s", peer, rec.Code, rec.Body.String())
		}
	}
	// Same peer again → same device, second request.
	rec := doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw, "10.0.0.1:1111")
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat chat: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/v1/me/status", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("me/status: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var st meStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode me/status: %v: %s", err, rec.Body.String())
	}
	if len(st.Devices) != 2 {
		t.Fatalf("devices = %d, want 2 distinct fingerprints", len(st.Devices))
	}
	total := 0
	for _, d := range st.Devices {
		total += d.Requests
		if d.Fingerprint == "" || !strings.HasPrefix(d.Fingerprint, "dev_") {
			t.Errorf("device fingerprint = %q, want dev_ + hex", d.Fingerprint)
		}
		if d.FirstSeen.IsZero() || d.LastSeen.IsZero() || d.LastSeen.Before(d.FirstSeen) {
			t.Errorf("device %s first/last seen = %v/%v, want last >= first, both set", d.Fingerprint, d.FirstSeen, d.LastSeen)
		}
		if strings.Contains(rec.Body.String(), "10.0.0.1") {
			t.Error("me/status must never expose a raw client IP")
		}
	}
	if total != 3 {
		t.Errorf("device request total = %d, want 3 (2 + 1 repeat)", total)
	}
	// The repeat peer's device counted twice, the other once.
	counts := map[int]int{}
	for _, d := range st.Devices {
		counts[d.Requests]++
	}
	if counts[2] != 1 || counts[1] != 1 {
		t.Errorf("device request counts = %v, want one device with 2 and one with 1", counts)
	}
}

func TestKeyDeviceFingerprintDistinguishesUA(t *testing.T) {
	tr := newKeyDeviceTracker()
	now := time.Now()
	tr.record("gk_x", deviceFingerprint("10.0.0.1", "agent-a"), now)
	tr.record("gk_x", deviceFingerprint("10.0.0.1", "agent-b"), now)
	tr.record("gk_x", deviceFingerprint("10.0.0.1", "agent-a"), now.Add(time.Second))
	if got := len(tr.devices("gk_x")); got != 2 {
		t.Fatalf("devices = %d, want 2 (same IP, different User-Agents)", got)
	}
	devs := tr.devices("gk_x")
	if len(devs) != 2 {
		t.Fatalf("devices list = %d, want 2", len(devs))
	}
	for _, d := range devs {
		if strings.Contains(d.Fingerprint, "10.0.0.1") || strings.Contains(d.Fingerprint, "agent-") {
			t.Errorf("fingerprint %q must not contain the raw IP or User-Agent", d.Fingerprint)
		}
	}
	// The fingerprint is a one-way hash: it never equals either input.
	if got := deviceFingerprint("10.0.0.1", "agent-a"); got == "10.0.0.1" || got == "agent-a" {
		t.Errorf("fingerprint %q must be a hash, not an input", got)
	}
	if len(tr.devices("gk_other")) != 0 {
		t.Error("devices must never leak across keys")
	}
}

func TestKeyUpdatePolicyFields(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	rec := doReq(t, srv, http.MethodPost, "/admin/keys/"+s.id, `{"ip_allowlist":["10.0.0.0/8"]}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("update policy: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var upd keyUpdateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &upd); err != nil {
		t.Fatalf("decode update response: %v: %s", err, rec.Body.String())
	}
	if len(upd.Policy.IPAllowlist) != 1 || upd.Policy.IPAllowlist[0] != "10.0.0.0/8" {
		t.Errorf("update policy = %+v, want the ip_allowlist", upd.Policy)
	}
	// Enforced on the very next request.
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw, "192.168.1.5:54321")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("chat after allowlist update: status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	// A bad update is a 400 and leaves the stored policy alone.
	rec = doReq(t, srv, http.MethodPost, "/admin/keys/"+s.id, `{"ip_allowlist":["bogus"]}`, adminRaw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad policy update: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	key, _ := s.cfg.FindGatewayKey(s.id)
	if len(key.IPAllowlist) != 1 || key.IPAllowlist[0] != "10.0.0.0/8" {
		t.Errorf("stored allowlist after failed update = %v, want [10.0.0.0/8] unchanged", key.IPAllowlist)
	}
	// A template on update is a 400, not a silent no-op.
	rec = doReq(t, srv, http.MethodPost, "/admin/keys/"+s.id, `{"template":"standard"}`, adminRaw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("template on update: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// Clearing the list restores open access.
	rec = doReq(t, srv, http.MethodPost, "/admin/keys/"+s.id, `{"ip_allowlist":[]}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear allowlist: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw, "192.168.1.5:54321")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat after clearing allowlist: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyRotateInheritsPolicy(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	setFixturePolicy(s, config.KeyPolicy{
		IPAllowlist:     []string{"10.0.0.0/8"},
		ModelAccessMode: "restricted", AllowedModels: []string{"model-a"},
	})
	srv := s.server(t, s.persistOpt(cfgPath))
	rec := doReq(t, srv, http.MethodPost, "/admin/keys/"+s.id+"/rotate", `{}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("rotate: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var rot keyRotateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rot); err != nil {
		t.Fatalf("decode rotate response: %v: %s", err, rec.Body.String())
	}
	key, ok := s.cfg.FindGatewayKey(rot.KeyID)
	if !ok {
		t.Fatalf("rotated key %s not in config", rot.KeyID)
	}
	if len(key.IPAllowlist) != 1 || key.IPAllowlist[0] != "10.0.0.0/8" {
		t.Errorf("rotated key allowlist = %v, want [10.0.0.0/8] inherited", key.IPAllowlist)
	}
	if key.ModelAccessMode != "restricted" || len(key.AllowedModels) != 1 || key.AllowedModels[0] != "model-a" {
		t.Errorf("rotated key model policy = %q %v, want restricted [model-a]", key.ModelAccessMode, key.AllowedModels)
	}
	// Enforced under the new credential: non-allowlisted peer → 403.
	rec = doReqPeer(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), rot.GatewayKey, "192.168.1.5:54321")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("chat with rotated key from blocked IP: status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// TestMeSessionsRouteExists (row 6 — VERIFY, not build): GET
// /v1/me/sessions exists and serves the caller's sessions.
// Evidence: handler internal/server/mesessions.go:51,
// registration internal/server/server.go:1579, guard table
// internal/server/guard.go:335. The behaviour itself is covered
// by TestMeSessions / TestMeSessionsRequiresKey /
// TestMeSessionsEmpty (mesessions_test.go).
func TestMeSessionsRouteExists(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	found := false
	for _, route := range RouteTable() {
		if route.Method == http.MethodGet && route.Path == "/v1/me/sessions" {
			found = true
		}
	}
	if !found {
		t.Fatal("GET /v1/me/sessions is missing from the guard route table")
	}
	rec := doReq(t, srv, http.MethodGet, "/v1/me/sessions", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/me/sessions: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out meSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode me/sessions: %v: %s", err, rec.Body.String())
	}
	if out.Object != "list" || out.Data == nil {
		t.Errorf("me/sessions response = %+v, want an empty list object", out)
	}
}
