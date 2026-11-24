// AUD-SEC-001 regression tests.
//
// Before the fix, auth accepted X-Api-Key / X-Goog-Api-Key but callerKeyID
// parsed only Authorization: Bearer, so a vendor-header caller resolved to
// "no identity" — escaping per-key policy/budgets/limits and (via
// batchVisible's empty-owner wildcard) reaching other keys' batches.
//
// These four tests pin the fix:
//  1. every accepted credential header resolves to the caller's key ID;
//  2. per-key policy is enforced identically under each header;
//  3. batches are invisible across keys, including via a vendor header;
//  4. an empty owner is never a wildcard to an identified caller.
package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// p13Server builds a server over cfg with a quiet logger.
func p13Server(t *testing.T, cfg *config.Config, eng Engine) *Server {
	t.Helper()
	srv := New(eng, WithConfig(cfg), WithLogger(log.New(io.Discard, "", 0)))
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// p13VendorKey adds one gateway key to cfg and returns (id, raw secret).
func p13VendorKey(t *testing.T, cfg *config.Config, name string, scopes []string, allowedEndpoints []string) (string, string) {
	t.Helper()
	k, raw, err := config.IssueKey(name, scopes, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(allowedEndpoints) > 0 {
		k.AllowedEndpoints = allowedEndpoints
	}
	if err := cfg.AddGatewayKey(k); err != nil {
		t.Fatal(err)
	}
	return k.ID, raw
}

// p13Req builds a request carrying exactly one credential header.
func p13Req(method, path, header, value, body string) *http.Request {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if header != "" {
		req.Header.Set(header, value)
	}
	return req
}

// TestAUD_SEC_001_VendorHeaderIdentityResolution: callerKeyID returns the
// real key ID for Bearer, X-Api-Key and X-Goog-Api-Key — and "" when no
// credential is presented.
func TestAUD_SEC_001_VendorHeaderIdentityResolution(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	keyID, raw := p13VendorKey(t, cfg, "identity-key", []string{"chat", "models"}, nil)
	srv := p13Server(t, cfg, okBatchEngine())

	for _, tc := range []struct{ header, value string }{
		{"Authorization", "Bearer " + raw},
		{"X-Api-Key", raw},
		{"X-Goog-Api-Key", raw},
	} {
		req := p13Req(http.MethodGet, "/v1/models", tc.header, tc.value, "")
		if got := srv.callerKeyID(req); got != keyID {
			t.Errorf("%s: callerKeyID = %q, want the key ID %q", tc.header, got, keyID)
		}
		if !srv.authorized(req) {
			t.Errorf("%s: authorized = false, want true", tc.header)
		}
	}

	// No credential: no identity, and not authorized.
	req := p13Req(http.MethodGet, "/v1/models", "", "", "")
	if got := srv.callerKeyID(req); got != "" {
		t.Errorf("no credential: callerKeyID = %q, want empty", got)
	}
	if srv.authorized(req) {
		t.Error("no credential: authorized = true, want false")
	}

	// A non-Bearer Authorization scheme is not a credential.
	req = p13Req(http.MethodGet, "/v1/models", "Authorization", "Basic "+raw, "")
	if got := srv.callerKeyID(req); got != "" {
		t.Errorf("Basic scheme: callerKeyID = %q, want empty", got)
	}
}

// TestAUD_SEC_001_PerKeyPolicyEnforcedViaVendorHeader: a key restricted to
// one endpoint is refused elsewhere, whether it authenticates with Bearer
// or with a vendor header (before the fix the vendor form was unpoliced).
// The allowed case proves the credential authenticates; the denied case
// proves the per-key restriction still applies to it.
func TestAUD_SEC_001_PerKeyPolicyEnforcedViaVendorHeader(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	_, raw := p13VendorKey(t, cfg, "restricted-key", []string{"chat", "models"}, []string{"/v1/models"})
	srv := p13Server(t, cfg, okBatchEngine())

	for _, tc := range []struct{ header, value string }{
		{"Authorization", "Bearer " + raw},
		{"X-Api-Key", raw},
		{"X-Goog-Api-Key", raw},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, p13Req(http.MethodGet, "/v1/models", tc.header, tc.value, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s on the allowed endpoint: status %d, want 200 (%s)", tc.header, rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		srv.ServeHTTP(rec, p13Req(http.MethodGet, "/v1/me/status", tc.header, tc.value, ""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s on a denied endpoint: status %d, want 403 (%s)", tc.header, rec.Code, rec.Body.String())
		}
	}
}

// TestAUD_SEC_001_CrossKeyBatchIsolation: key B (authenticating with a
// vendor header) can neither list, read, cancel nor delete key A's batch.
func TestAUD_SEC_001_CrossKeyBatchIsolation(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	srv := p13Server(t, cfg, okBatchEngine())
	_, rawA := p13VendorKey(t, cfg, "key-a", []string{"chat"}, nil)
	_, rawB := p13VendorKey(t, cfg, "key-b", []string{"chat"}, nil)

	// Key A seeds an input file and creates a batch.
	in := seedBatchInput(t, srv, batchJSONL(t, "req-a"))
	createBody, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, p13Req(http.MethodPost, "/v1/batches", "Authorization", "Bearer "+rawA, string(createBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("key A create: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("key A create: no batch id")
	}

	// Key B lists batches: A's batch must not appear.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, p13Req(http.MethodGet, "/v1/batches", "X-Api-Key", rawB, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("key B list: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	for _, b := range list.Data {
		if b.ID == created.ID {
			t.Fatalf("cross-key leak: key B's list contains key A's batch %s", created.ID)
		}
	}

	// Key B cannot read, cancel or delete A's batch (404, never 403).
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/batches/" + created.ID},
		{http.MethodPost, "/v1/batches/" + created.ID + "/cancel"},
		{http.MethodDelete, "/v1/batches/" + created.ID},
	} {
		rec = httptest.NewRecorder()
		srv.ServeHTTP(rec, p13Req(tc.method, tc.path, "X-Api-Key", rawB, ""))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("key B %s %s: status %d, want 404", tc.method, tc.path, rec.Code)
		}
	}

	// Key A still sees its own batch (the fix must not over-restrict).
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, p13Req(http.MethodGet, "/v1/batches/"+created.ID, "X-Api-Key", rawA, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("key A get own batch: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestAUD_SEC_001_EmptyOwnerFailClosed: an empty owner is not a wildcard.
func TestAUD_SEC_001_EmptyOwnerFailClosed(t *testing.T) {
	owned := &Batch{OwnerKeyID: "key-123"}
	unowned := &Batch{OwnerKeyID: ""}

	if !batchVisible(owned, "key-123") {
		t.Error("owner must see its own batch")
	}
	if batchVisible(owned, "key-456") {
		t.Error("a foreign key must not see an owned batch")
	}
	if batchVisible(owned, "") {
		t.Error("an unidentified caller must not see an owned batch")
	}
	if !batchVisible(unowned, "") {
		t.Error("an unidentified caller sees unowned batches (auth-relaxed trust domain)")
	}
	if batchVisible(unowned, "key-123") {
		t.Error("an identified caller must not see an unowned batch (empty owner is not a wildcard)")
	}
}
