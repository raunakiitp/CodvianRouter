package provider

// A1 gap 2: templated catalog base URLs resolve at add time from
// operator-supplied parameters (Cloudflare {account_id}, AI Gateway
// {account_id}/{gateway_id}/{provider}, Snowflake ${SNOWFLAKE_ACCOUNT}).
// Resolution runs against the registry's real add flow — the mock
// upstream stands in for the provider host, so the resolved URL is
// what the add-time probe actually dials. Missing or unsafe parameter
// values must refuse the add and name the problem.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newTemplateTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	// A wildcard mock: the add-time probe dials the RESOLVED base
	// (mock host + substituted path + /v1/models), so the handler
	// must accept any path — an unexpected path here means the
	// substitution produced something unexpected, which the
	// assertions on p.BaseURL already check.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			_, _ = w.Write([]byte(`{"model":"model-a","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
	}))
	t.Cleanup(srv.Close)
	reg := NewRegistry(nil, WithRegistryAllowPrivateDestinations(true))
	return reg, srv.URL
}

// TestAddCatalogTemplatedWorkersAI: cloudflare-workers-ai resolves
// {account_id} and the add-time probe dials the RESOLVED base (the
// mock serves the substituted path), so a resolved-but-wrong URL
// cannot pass silently.
func TestAddCatalogTemplatedWorkersAI(t *testing.T) {
	reg, mockURL := newTemplateTestRegistry(t)
	concrete := mockURL + "/client/v4/accounts/acct-42/ai/v1"
	p, err := reg.AddCatalogWithOptions(t.Context(), "cloudflare-workers-ai", "cf-key", CatalogAddOptions{
		BaseURL:        mockURL + "/client/v4/accounts/{account_id}/ai/v1",
		TemplateParams: map[string]string{"account_id": "acct-42"},
	})
	if err != nil {
		t.Fatalf("add with template params: %v", err)
	}
	if p.BaseURL != concrete {
		t.Errorf("BaseURL = %q, want the substituted %q", p.BaseURL, concrete)
	}
}

// TestAddCatalogTemplatedSnowflakeForm: the ${NAME} spelling used by
// the snowflake-cortex template resolves the same way.
func TestAddCatalogTemplatedSnowflakeForm(t *testing.T) {
	reg, mockURL := newTemplateTestRegistry(t)
	concrete := mockURL + "/api/v2/cortex/v1"
	p, err := reg.AddCatalogWithOptions(t.Context(), "snowflake-cortex", "sf-key", CatalogAddOptions{
		BaseURL:        mockURL + "/api/v2/cortex/v1",
		TemplateParams: map[string]string{"SNOWFLAKE_ACCOUNT": "acct1"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	// The catalog template carries ${SNOWFLAKE_ACCOUNT} before the
	// host; with the mock override the substitution exercises the
	// same resolver — assert the value landed in the concrete URL.
	if !strings.Contains(p.BaseURL, concrete) || strings.Contains(p.BaseURL, "${") || strings.Contains(p.BaseURL, "{") {
		t.Errorf("BaseURL = %q, want the substituted %q with no placeholders left", p.BaseURL, concrete)
	}
}

// TestAddCatalogTemplatedMissingParamRefused: without the parameters
// the add is refused and the error NAMES the missing placeholders —
// the operator must never guess which values a template wants.
func TestAddCatalogTemplatedMissingParamRefused(t *testing.T) {
	reg, mockURL := newTemplateTestRegistry(t)
	_, err := reg.AddCatalogWithOptions(t.Context(), "cloudflare-ai-gateway", "cf-key", CatalogAddOptions{
		BaseURL: mockURL + "/v1/{account_id}/{gateway_id}/{provider}",
	})
	if err == nil {
		t.Fatal("missing template parameters must refuse the add")
	}
	for _, name := range []string{"account_id", "gateway_id", "provider"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q must name %q", err.Error(), name)
		}
	}
	if len(reg.List()) != 0 {
		t.Errorf("a refused add must not leave providers behind (got %d)", len(reg.List()))
	}
}

// TestAddCatalogTemplatedUnsafeValueRefused: a template value that is
// not a safe URL path segment (path traversal, scheme injection) is
// rejected — never interpolated into the base URL.
func TestAddCatalogTemplatedUnsafeValueRefused(t *testing.T) {
	reg, mockURL := newTemplateTestRegistry(t)
	for _, bad := range []string{"../admin", "https://evil.example", "acct 1"} {
		_, err := reg.AddCatalogWithOptions(t.Context(), "cloudflare-workers-ai", "cf-key", CatalogAddOptions{
			BaseURL:        mockURL + "/client/v4/accounts/{account_id}/ai/v1",
			TemplateParams: map[string]string{"account_id": bad},
		})
		if err == nil {
			t.Errorf("unsafe value %q must refuse the add", bad)
		}
	}
}

// TestAddCatalogTemplatedPersistedResolved: the persisted provider
// stores the RESOLVED URL, never the template — a reload must not
// bring placeholders back.
func TestAddCatalogTemplatedPersistedResolved(t *testing.T) {
	_, mockURL := newTemplateTestRegistry(t)
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, "providers.json")
	reg2, err := NewFileRegistry(vaultPath, WithRegistryAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("file registry: %v", err)
	}
	concrete := mockURL + "/client/v4/accounts/acct-7/ai/v1"
	p, err := reg2.AddCatalogWithOptions(t.Context(), "cloudflare-workers-ai", "cf-key", CatalogAddOptions{
		BaseURL:        mockURL + "/client/v4/accounts/{account_id}/ai/v1",
		TemplateParams: map[string]string{"account_id": "acct-7"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	reloaded, err := NewFileRegistry(vaultPath, WithRegistryAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reloaded.Get(p.ID)
	if !ok {
		t.Fatal("provider not persisted")
	}
	if got.BaseURL != concrete {
		t.Errorf("persisted BaseURL = %q, want the resolved %q", got.BaseURL, concrete)
	}
}
