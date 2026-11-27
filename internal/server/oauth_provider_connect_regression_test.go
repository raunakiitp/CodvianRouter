package server

// A3 cross-cutting regressions (stream C): the per-provider OAuth
// handler registry (internal/oauth/providers) and its five connect
// routes must never drift apart silently. PR #117/#119 built the
// handlers and the server bridge; PR #122 wired the production routes.
// These tests fail if:
//
//   - one of the five connect routes loses its HTTP surface (route
//     deleted from the mux — a plain mux 404 has a different body than
//     the connect handler's typed 404, so the dispatch is provable);
//   - an IMPLEMENTED handler in the registry has no production route
//     that can actually start its flow (registry → route coverage, per
//     flow kind: code/browser → /start, device → /device/start,
//     import-only → /import).
//
// Both probes are offline: code-flow start builds URLs locally, device
// start runs against a local mock through providers.SetEndpointsForTest,
// and the import probe stops at the route's own empty-blob validation.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/oauth/providers"
)

// providerConnectRoutes are the five connect surfaces from
// oauth_provider_routes.go. {provider} is substituted per probe.
var providerConnectRoutes = []struct {
	name string
	path string
}{
	{"StartProviderOAuth", "/dashboard/oauth/providers/{provider}/start"},
	{"CompleteProviderOAuth", "/dashboard/oauth/providers/{provider}/complete"},
	{"StartProviderDevice", "/dashboard/oauth/providers/{provider}/device/start"},
	{"CompleteProviderDevice", "/dashboard/oauth/providers/{provider}/device/complete"},
	{"ImportProviderBlob", "/dashboard/oauth/providers/{provider}/import"},
}

// newConnectRegressionServer is newTestServer with the connection
// store's client swapped for a plain test client — the same seam
// oauth_provider_routes_test.go uses — so the device-flow mock (an
// httptest loopback server) is reachable; the SSRF-guarded-client
// behavior is proven separately by oauth_conn_guard_test.go.
func newConnectRegressionServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(t, &fakeEngine{})
	st := newOAuthConnStore(nil)
	st.client = &http.Client{}
	srv.oauthStore = st
	return srv
}

// TestProviderConnectRoutesExistAndDispatch proves each of the five
// connect methods still has an HTTP route that dispatches through the
// connect handler: a probe with an unknown provider reaches the
// handler's typed refusal ("not a catalog OAuth provider", 404) — a
// deleted route would answer the mux's plain 404 page instead.
func TestProviderConnectRoutesExistAndDispatch(t *testing.T) {
	srv := newConnectRegressionServer(t)
	const key = "test-key-123"
	const unknown = "definitely-not-a-catalog-provider"

	for _, rt := range providerConnectRoutes {
		path := strings.Replace(rt.path, "{provider}", unknown, 1)
		rec := doReq(t, srv, http.MethodPost, path, "{}", key)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s route %s: status %d, want the connect handler's typed 404 for an unknown provider (a plain 404 here means the route is gone)", rt.name, path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), "not a catalog OAuth provider") {
			t.Errorf("%s route %s: body %q does not carry the connect handler's refusal — the route no longer dispatches through the provider connect handler", rt.name, path, rec.Body.String())
		}
	}
}

// deviceStartMock serves the superset JSON every device-flow handler's
// StartDevice parses (camelCase and snake_case variants of the RFC 8628
// fields, plus the AWS SSO OIDC client-registration response), so one
// server can stand in for every device provider's documented endpoint.
func deviceStartMock(t *testing.T) *httptest.Server {
	t.Helper()
	writeMockJSON := func(w http.ResponseWriter, v map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	superset := func(deviceCode, userCode string) map[string]any {
		return map[string]any{
			"deviceCode": deviceCode, "device_code": deviceCode,
			"userCode": userCode, "user_code": userCode,
			"verificationUri": "https://example.com/device", "verification_uri": "https://example.com/device",
			"verificationUriComplete":   "https://example.com/device?user_code=" + userCode,
			"verification_uri_complete": "https://example.com/device?user_code=" + userCode,
			"expiresIn":                 600, "expires_in": 600,
			"interval": 5,
			// AWS SSO OIDC client registration (POST /client/register)
			"clientId": "mock-client-1", "client_id": "mock-client-1",
			"clientSecret": "mock-secret-1", "client_secret": "mock-secret-1",
			"clientSecretExpiresAt": 1893456000, "client_secret_expires_at": 1893456000,
		}
	}
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, superset("mock-dc-1", "USER-CODE-1"))
	})
	mux.HandleFunc("/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, superset("mock-dc-1", "USER-CODE-1"))
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, superset("mock-dc-1", "USER-CODE-1"))
	})
	return httptest.NewServer(mux)
}

// TestEveryImplementedOAuthHandlerIsRouteCovered walks the registry's
// implemented list and, per provider, drives the production route that
// matches its flow kind until the flow actually starts (200 with the
// flow payload). A handler whose production route disappeared (or was
// never wired) fails here — the registry and the routes cannot drift.
func TestEveryImplementedOAuthHandlerIsRouteCovered(t *testing.T) {
	srv := newConnectRegressionServer(t)
	const key = "test-key-123"

	// Handlers that need operator configuration before they can build a
	// start request (their documented env knobs). Everything else starts
	// offline against the documented defaults or the endpoint override.
	t.Setenv("CODVIAN_OAUTH_GITLAB_DUO_CLIENT_ID", "coverage-test-client")
	t.Setenv("CODVIAN_OAUTH_CURSOR_CLIENT_ID", "coverage-test-client")
	t.Setenv("CODVIAN_OAUTH_CURSOR_CLIENT_SECRET", "coverage-test-secret")
	t.Setenv("CODVIAN_OAUTH_GHE_HOST", "octocorp.ghe.com")

	// One local mock stands in for every device provider's documented
	// device endpoint.
	mock := deviceStartMock(t)
	defer mock.Close()
	deviceOverride := map[string]string{
		"device":     mock.URL + "/device",
		"deviceauth": mock.URL + "/device_authorization",
		"register":   mock.URL + "/register",
		"poll":       mock.URL + "/device",
		"token":      mock.URL + "/device",
		"exchange":   mock.URL + "/device",
		"refresh":    mock.URL + "/device",
		"auth":       mock.URL + "/device",
	}

	infos := providers.ImplementedInfos()
	if len(infos) == 0 {
		t.Fatal("the OAuth handler registry reports ZERO implemented handlers — the registry lost its handlers or this coverage test must be updated")
	}

	covered := map[string]int{"start": 0, "device/start": 0, "import": 0}
	for _, in := range infos {
		switch in.Flow {
		case providers.FlowPKCE, providers.FlowBrowserSignIn:
			rec := doReq(t, srv, http.MethodPost, "/dashboard/oauth/providers/"+in.ID+"/start",
				`{"redirect_uri":"http://127.0.0.1:3000/callback"}`, key)
			if rec.Code != http.StatusOK {
				t.Errorf("implemented code-flow handler %q: POST .../start = %d, want 200 (body %s)", in.ID, rec.Code, rec.Body.String())
				continue
			}
			var start struct {
				AuthorizeURL string `json:"authorize_url"`
				State        string `json:"state"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil || start.AuthorizeURL == "" || start.State == "" {
				t.Errorf("implemented code-flow handler %q: start response %q has no authorize_url/state", in.ID, rec.Body.String())
				continue
			}
			covered["start"]++

		case providers.FlowDeviceCode:
			providers.SetEndpointsForTest(t, in.ID, deviceOverride)
			rec := doReq(t, srv, http.MethodPost, "/dashboard/oauth/providers/"+in.ID+"/device/start", "{}", key)
			if rec.Code != http.StatusOK {
				t.Errorf("implemented device-flow handler %q: POST .../device/start = %d, want 200 (body %s)", in.ID, rec.Code, rec.Body.String())
				continue
			}
			var start struct {
				DeviceCode string `json:"device_code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil || start.DeviceCode == "" {
				t.Errorf("implemented device-flow handler %q: device/start response %q has no device_code", in.ID, rec.Body.String())
				continue
			}
			covered["device/start"]++

		case providers.FlowPasteImport:
			if !in.ImportOnly {
				t.Errorf("provider %q has flow %q but ImportOnly=false — the registry's own invariants are inconsistent", in.ID, in.Flow)
			}
			// An empty blob stops at the route's own validation, which
			// proves the route serves this import-only provider (an
			// unknown provider would 404; an unimplemented one 501).
			rec := doReq(t, srv, http.MethodPost, "/dashboard/oauth/providers/"+in.ID+"/import", `{"blob":""}`, key)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "blob is required") {
				t.Errorf("import-only handler %q: POST .../import = %d, want 400 blob-required (body %s) — the import route no longer serves this provider", in.ID, rec.Code, rec.Body.String())
				continue
			}
			covered["import"]++

		default:
			t.Errorf("provider %q has unknown flow %q — update TestEveryImplementedOAuthHandlerIsRouteCovered for the new flow kind", in.ID, in.Flow)
		}
	}
	if covered["start"] == 0 || covered["device/start"] == 0 || covered["import"] == 0 {
		t.Fatalf("flow-kind coverage incomplete: %+v — the registry should implement at least one code-flow, one device-flow and one import-only provider", covered)
	}

	// Cross-check: the listing route (GET /dashboard/oauth/providers)
	// serves the real registry, and every implemented handler is on it.
	rec := doReq(t, srv, http.MethodGet, "/dashboard/oauth/providers", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/oauth/providers = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var list struct {
		Providers []struct {
			ID          string `json:"id"`
			Implemented bool   `json:"implemented"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("listing body: %v", err)
	}
	listed := map[string]bool{}
	for _, p := range list.Providers {
		listed[p.ID] = true
	}
	for _, in := range infos {
		if !listed[in.ID] {
			t.Errorf("implemented handler %q is missing from GET /dashboard/oauth/providers — the dashboard connect flow cannot reach it", in.ID)
		}
	}
	if t.Failed() {
		return
	}
	t.Logf("registry→route coverage: %v", fmt.Sprintf("%d start, %d device, %d import", covered["start"], covered["device/start"], covered["import"]))
}
