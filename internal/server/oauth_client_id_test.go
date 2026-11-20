package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestOAuthCreateClientReturnsClientID pins the :678 follow-up fix:
// POST /oauth/clients (and GET /oauth/clients) must name the client
// "client_id" (RFC 7591 §3.2), not "id" — the authorize/token
// endpoints already consume "client_id", so a create response that
// said "id" forced every caller to rename the value by hand.
func TestOAuthCreateClientReturnsClientID(t *testing.T) {
	srv, adminKey, _ := newDashAuthServer(t, "")
	rec := doReq(t, srv, http.MethodPost, "/oauth/clients",
		`{"name":"critic-client-id","redirect_uris":["http://127.0.0.1:9/cb"],"public":true}`, adminKey)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create body is not JSON: %v", err)
	}
	id, ok := created["client_id"].(string)
	if !ok || id == "" {
		t.Fatalf("create response has no client_id string: %s", rec.Body.String())
	}
	if !strings.HasPrefix(id, "oc_") {
		t.Fatalf("client_id = %q, want oc_ prefix", id)
	}
	if _, bad := created["id"]; bad {
		t.Fatalf("create response still carries legacy \"id\" key: %s", rec.Body.String())
	}

	rec = doReq(t, srv, http.MethodGet, "/oauth/clients", "", adminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var listed struct {
		Clients []map[string]any `json:"clients"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list body is not JSON: %v", err)
	}
	found := false
	for _, c := range listed.Clients {
		if _, bad := c["id"]; bad {
			t.Fatalf("list entry still carries legacy \"id\" key: %v", c)
		}
		if c["client_id"] == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("list has no entry with client_id %q: %s", id, rec.Body.String())
	}
}
