// Capability metadata endpoints (P1.5): GET /api/capabilities and
// GET /api/providers/{id}/capabilities. The tests pin the honesty
// rules — every fact carries support + provenance, unknown is never
// presented as supported, and an unconfigured deployment answers 503
// instead of inventing a matrix.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// p15CapabilityRegistry builds a registry with one hand-added provider
// (no catalog ID, so the matrix contains exactly these models) holding:
//
//	vision-known   — Vision + VisionKnown (sync verified)
//	vision-declared— Vision only (operator-set, unverified)
//	tools-on       — Tools pointer true
//	plain          — nothing recorded
func p15CapabilityRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	yes := true
	cfgs := []provider.ProviderConfig{{
		ID: "prov-cap", Name: "Capability Provider", BaseURL: "https://example.com/cap",
		Models: []provider.Model{
			{ID: "vision-known", Vision: true, VisionKnown: true, ContextWindow: 128000},
			{ID: "vision-declared", Vision: true},
			{ID: "tools-on", Tools: &yes},
			{ID: "plain"},
		},
		Enabled: true,
	}}
	raw, err := json.Marshal(cfgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reg := provider.NewRegistry(provider.NewJSONFileStore(path))
	if err := reg.Load(); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestCapabilitiesEndpointRequiresWiring(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	for _, path := range []string{"/api/capabilities", "/api/providers/prov-cap/capabilities"} {
		rec := doReq(t, srv, http.MethodGet, path, "", "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503 without WithCapabilityRegistry", path, rec.Code)
		}
	}
}

func TestCapabilitiesMatrixReportsProvenance(t *testing.T) {
	reg := p15CapabilityRegistry(t)
	srv := newTestServer(t, &fakeEngine{}, WithCapabilityRegistry(reg))
	rec := doReq(t, srv, http.MethodGet, "/api/capabilities", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Object string `json:"object"`
		Count  int    `json:"count"`
		Models []struct {
			Provider     string `json:"provider"`
			Model        string `json:"model"`
			Capabilities []struct {
				Capability   string `json:"capability"`
				Support      string `json:"support"`
				Source       string `json:"source"`
				Verification string `json:"verification"`
			} `json:"capabilities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Object != "capabilities.matrix" {
		t.Fatalf("object = %q", body.Object)
	}
	if body.Count != len(body.Models) || body.Count < 4 {
		t.Fatalf("count = %d, models = %d", body.Count, len(body.Models))
	}

	find := func(model, cap string) (support, source, verification string, ok bool) {
		for _, m := range body.Models {
			if m.Model != model {
				continue
			}
			for _, c := range m.Capabilities {
				if c.Capability == cap {
					return c.Support, c.Source, c.Verification, true
				}
			}
		}
		return "", "", "", false
	}

	// Vision with a sync report: supported by models.dev, verified.
	if s, src, v, ok := find("vision-known", "vision"); !ok || s != "supported" || v != "verified" || src != "models_dev_sync" {
		t.Fatalf("vision-known/vision = %q %q %q (found=%v), want supported models_dev_sync verified", s, src, v, ok)
	}
	// Vision declared by the operator only: supported but NOT verified.
	if s, src, v, ok := find("vision-declared", "vision"); !ok || s != "supported" || v != "unverified" || src != "operator_config" {
		t.Fatalf("vision-declared/vision = %q %q %q (found=%v), want supported operator_config unverified", s, src, v, ok)
	}
	// Tool calling with a set flag: verified.
	if s, _, v, ok := find("tools-on", "tool_calling"); !ok || s != "supported" || v != "verified" {
		t.Fatalf("tools-on/tool_calling = %q %q (found=%v), want supported verified", s, v, ok)
	}
	// A model with nothing recorded: tool calling is explicitly unknown.
	if s, _, v, ok := find("plain", "tool_calling"); !ok || s != "unknown" || v == "verified" {
		t.Fatalf("plain/tool_calling = %q %q (found=%v), want unknown and not verified", s, v, ok)
	}
	// No entry may present unknown support as verified.
	for _, m := range body.Models {
		for _, c := range m.Capabilities {
			if c.Support == "unknown" && c.Verification == "verified" {
				t.Fatalf("%s/%s: unknown support marked verified", m.Model, c.Capability)
			}
		}
	}
}

func TestProviderCapabilitiesEndpoint(t *testing.T) {
	reg := p15CapabilityRegistry(t)
	srv := newTestServer(t, &fakeEngine{}, WithCapabilityRegistry(reg))

	rec := doReq(t, srv, http.MethodGet, "/api/providers/prov-cap/capabilities", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Object   string `json:"object"`
		Provider string `json:"provider"`
		Count    int    `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Object != "provider.capabilities" || body.Provider != "prov-cap" {
		t.Fatalf("object/provider = %q/%q", body.Object, body.Provider)
	}
	if body.Count != 4 {
		t.Fatalf("count = %d, want 4", body.Count)
	}

	rec = doReq(t, srv, http.MethodGet, "/api/providers/nope/capabilities", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown provider status = %d, want 404", rec.Code)
	}
}

// TestCapabilityRoutesGuardPolicy pins the classification of the three
// P1.5 routes: the GETs are exempt read-only MANAGEMENT feeds, and the
// refresh POST is MANAGEMENT with the admin scope (exempt routes must be
// read-only GETs, so a mutating rebuild is never exempt).
func TestCapabilityRoutesGuardPolicy(t *testing.T) {
	want := map[string]struct {
		tier   GuardTier
		exempt bool
		scopes []string
	}{
		"GET /api/capabilities":                {TierManagement, true, nil},
		"GET /api/providers/{id}/capabilities": {TierManagement, true, nil},
		"POST /api/capabilities/refresh":       {TierManagement, false, []string{"admin"}},
	}
	seen := map[string]bool{}
	for _, r := range RouteTable() {
		key := r.Method + " " + r.Path
		exp, ok := want[key]
		if !ok {
			continue
		}
		seen[key] = true
		if r.Tier != exp.tier {
			t.Errorf("%s tier = %v, want %v", key, r.Tier, exp.tier)
		}
		if r.AuthExempt != exp.exempt {
			t.Errorf("%s AuthExempt = %v, want %v", key, r.AuthExempt, exp.exempt)
		}
		if len(r.Scopes) != len(exp.scopes) {
			t.Errorf("%s scopes = %v, want %v", key, r.Scopes, exp.scopes)
			continue
		}
		for i := range exp.scopes {
			if r.Scopes[i] != exp.scopes[i] {
				t.Errorf("%s scopes = %v, want %v", key, r.Scopes, exp.scopes)
			}
		}
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("route %s is missing from the guard table", key)
		}
	}
}

// TestCapabilitiesRefreshEndpoint: the rebuild is authenticated (it is a
// mutating management route, not an exempt read-only GET) and reports
// the rebuilt size without inventing capabilities.
func TestCapabilitiesRefreshEndpoint(t *testing.T) {
	reg := p15CapabilityRegistry(t)
	srv := newTestServer(t, &fakeEngine{}, WithCapabilityRegistry(reg))

	// Unauthenticated: refused; the endpoint is not exempt.
	rec := doReq(t, srv, http.MethodPost, "/api/capabilities/refresh", "", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("unauthenticated refresh returned 200, want a refusal")
	}

	rec = doReq(t, srv, http.MethodPost, "/api/capabilities/refresh", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Object string `json:"object"`
		Count  int    `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Object != "capabilities.refresh" {
		t.Fatalf("object = %q", body.Object)
	}
	if body.Count < 4 {
		t.Fatalf("count = %d, want the four registry models at least", body.Count)
	}

	// The matrix is still served after the refresh.
	rec = doReq(t, srv, http.MethodGet, "/api/capabilities", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("matrix after refresh = %d, want 200", rec.Code)
	}
}
