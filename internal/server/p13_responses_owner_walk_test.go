// P13: internal Responses chain walks are TENANT-SCOPED. The store's
// Get(id, owner) enforces exact owner equality (the Responses
// cross-tenant isolation fix), and a production record is stored with
// Owner = the caller's key id. An internal walk that looks records up
// under owner "" therefore finds NOTHING on a real (owner-carrying)
// record: the session list silently under-counts, and the session
// detail / input-tokens endpoints 404. These tests seed records in the
// PRODUCTION shape (both KeyID and Owner set to the caller's id) and
// prove the walks still resolve the whole chain.
package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/responses"
)

// seedOwnedSession stores a two-turn Responses session (root + child)
// owned by keyID — the shape storeResponsesRecord writes.
func seedOwnedSession(t *testing.T, srv *Server, keyID string) {
	t.Helper()
	put := func(rec responses.Record) {
		raw, err := json.Marshal([]provider.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		})
		if err != nil {
			t.Fatal(err)
		}
		rec.TranscriptJSON = raw
		rec.Status = responses.StatusCompleted
		rec.KeyID = keyID
		rec.Owner = keyID // production shape: the owner is the caller id
		srv.responses.Put(rec)
	}
	put(responses.Record{ID: "resp_root", Model: "claude-sonnet-4-5", TurnInputTokens: 10, TurnOutputTokens: 5})
	put(responses.Record{ID: "resp_child", Model: "claude-sonnet-4-5", PreviousID: "resp_root", TurnInputTokens: 20, TurnOutputTokens: 7})
}

// TestResponsesOwnerScopedSessionListCountsWholeChain: the list must
// count every turn of the session, not just the root.
func TestResponsesOwnerScopedSessionListCountsWholeChain(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	keyID := staticKeyID("test-key-123")
	seedOwnedSession(t, srv, keyID)

	rec := doReq(t, srv, http.MethodGet, "/v1/me/sessions", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out meSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(out.Data) != 1 {
		t.Fatalf("rows = %d, want 1 (the seeded session)", len(out.Data))
	}
	row := out.Data[0]
	if row.Source != "responses" {
		t.Fatalf("row source = %q, want responses", row.Source)
	}
	if row.Turns != 2 {
		t.Errorf("turns = %d, want 2: the walk must follow the chain on an owner-carrying record", row.Turns)
	}
	if row.InputTokens != 30 || row.OutputTokens != 12 {
		t.Errorf("tokens = %d/%d, want 30/12 (root + child)", row.InputTokens, row.OutputTokens)
	}
}

// TestResponsesOwnerScopedSessionDetailResolvesChain: the detail
// endpoint must resolve the root from a MID-CHAIN turn id.
func TestResponsesOwnerScopedSessionDetailResolvesChain(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	keyID := staticKeyID("test-key-123")
	seedOwnedSession(t, srv, keyID)

	rec := doReq(t, srv, http.MethodGet, "/v1/me/sessions/resp_child", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the PreviousID walk must resolve the root on owner-carrying records (%s)", rec.Code, rec.Body.String())
	}
	var out meSessionDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if out.Turns != 2 {
		t.Errorf("turns = %d, want 2 (whole chain)", out.Turns)
	}
	if len(out.TurnsDetail) != 2 {
		t.Errorf("turns_detail = %d, want 2", len(out.TurnsDetail))
	}
}

// TestResponsesOwnerScopedInputTokensResolves: the input-tokens
// endpoint must find an owner-carrying stored response.
func TestResponsesOwnerScopedInputTokensResolves(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	keyID := staticKeyID("test-key-123")
	seedOwnedSession(t, srv, keyID)

	rec := doReq(t, srv, http.MethodGet, "/v1/responses/input_tokens?response_id=resp_root", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (owner-carrying record must resolve): %s", rec.Code, rec.Body.String())
	}
}

// TestResponsesOwnerScopedWalksRejectOtherTenant: the fix must NOT
// widen access — another key's record stays invisible.
func TestResponsesOwnerScopedWalksRejectOtherTenant(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	seedOwnedSession(t, srv, "static-someone-else")

	rec := doReq(t, srv, http.MethodGet, "/v1/me/sessions/resp_root", "", "test-key-123")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other-tenant session status = %d, want 404", rec.Code)
	}
	// And it never appears in the caller's list.
	list := doReq(t, srv, http.MethodGet, "/v1/me/sessions", "", "test-key-123")
	var out meSessionsResponse
	if err := json.Unmarshal(list.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 0 {
		t.Fatalf("rows = %d, want 0 (another key's session must not be listed)", len(out.Data))
	}
}
