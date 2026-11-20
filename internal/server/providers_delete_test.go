package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// addTwoMockProviders adds two live providers backed by the mock
// upstream and returns their ids (in add order).
func addTwoMockProviders(t *testing.T, srv *Server) (string, string) {
	t.Helper()
	up1 := httptest.NewServer(mockUpstream{}.handler())
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(mockUpstream{}.handler())
	t.Cleanup(up2.Close)
	rec1 := postAdd(t, srv, `{"base_url": "`+up1.URL+`", "api_key": "sk-test-1", "name": "Mock One"}`, "test-key-123")
	rec2 := postAdd(t, srv, `{"base_url": "`+up2.URL+`", "api_key": "sk-test-2", "name": "Mock Two"}`, "test-key-123")
	var a, b struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &a); err != nil || a.ID == "" {
		t.Fatalf("add one: body = %s, err = %v", rec1.Body.String(), err)
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &b); err != nil || b.ID == "" {
		t.Fatalf("add two: body = %s, err = %v", rec2.Body.String(), err)
	}
	if a.ID == b.ID {
		t.Fatalf("both mock providers got the same id %q", a.ID)
	}
	return a.ID, b.ID
}

func deleteProvider(t *testing.T, srv *Server, id, phrase, key string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, srv, http.MethodDelete, "/api/providers/"+id, `{"confirm":"`+phrase+`"}`, key)
}

func TestDeleteProviderLiveRemoved(t *testing.T) {
	srv, reg := newAddTestServer(t)
	var hooked *provider.Provider
	srv.providerRemovedHook = func(ctx context.Context, p *provider.Provider) error {
		hooked = p
		return nil
	}
	id1, id2 := addTwoMockProviders(t, srv)
	rec := deleteProvider(t, srv, id1, "DELETE "+id1, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res removedProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal response: %v (%s)", err, rec.Body.String())
	}
	if res.ID != id1 || res.Name != "Mock One" || !res.LiveRemoved || res.Note != "" {
		t.Errorf("response = %+v, want id %q name Mock One live_removed true empty note", res, id1)
	}
	if len(res.Models) == 0 {
		t.Errorf("response models = %v, want the provider's models listed", res.Models)
	}
	// The response never carries a secret.
	if strings.Contains(rec.Body.String(), "sk-test-1") || strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Errorf("response leaks a key or base URL: %s", rec.Body.String())
	}
	if hooked == nil || hooked.ID != id1 {
		t.Errorf("hook got %+v, want the removed provider %q", hooked, id1)
	}
	// Registry and feed agree: the provider is gone, the other stays.
	if _, ok := reg.Get(id1); ok {
		t.Errorf("registry still has %q after delete", id1)
	}
	if _, ok := reg.Get(id2); !ok {
		t.Errorf("registry lost %q — delete must remove only the named provider", id2)
	}
	feed := doReq(t, srv, http.MethodGet, "/api/providers", "", "")
	if strings.Contains(feed.Body.String(), id1) {
		t.Errorf("provider feed still lists %q after delete: %s", id1, feed.Body.String())
	}
	if !strings.Contains(feed.Body.String(), id2) {
		t.Errorf("provider feed lost %q after deleting %q: %s", id2, id1, feed.Body.String())
	}
}

func TestDeleteProviderNoHookHonest(t *testing.T) {
	srv, _ := newAddTestServer(t)
	id1, _ := addTwoMockProviders(t, srv)
	rec := deleteProvider(t, srv, id1, "DELETE "+id1, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res removedProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// No engine hook wired: the delete still succeeded (registry +
	// config), but the response says honestly that the live engine
	// may still route through it until restart.
	if res.LiveRemoved || res.Note == "" {
		t.Errorf("response = %+v, want live_removed false with a restart note", res)
	}
}

func TestDeleteProviderHookFailureStillDeletes(t *testing.T) {
	srv, reg := newAddTestServer(t)
	srv.providerRemovedHook = func(ctx context.Context, p *provider.Provider) error {
		return errors.New("engine exploded")
	}
	id1, _ := addTwoMockProviders(t, srv)
	rec := deleteProvider(t, srv, id1, "DELETE "+id1, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when the hook fails: %s", rec.Code, rec.Body.String())
	}
	var res removedProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if res.LiveRemoved || res.Note == "" {
		t.Errorf("response = %+v, want live_removed false with a restart note after a hook failure", res)
	}
	if _, ok := reg.Get(id1); ok {
		t.Errorf("registry still has %q after delete — a hook failure must not undo the delete", id1)
	}
}

func TestDeleteProviderWrongPhrase(t *testing.T) {
	srv, reg := newAddTestServer(t)
	id1, _ := addTwoMockProviders(t, srv)
	for _, phrase := range []string{"", "DELETE", "DELETE " + id1 + " ", "delete " + id1, "REVOKE ALL"} {
		rec := deleteProvider(t, srv, id1, phrase, "test-key-123")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("phrase %q: status = %d, want 400: %s", phrase, rec.Code, rec.Body.String())
		}
	}
	if _, ok := reg.Get(id1); !ok {
		t.Errorf("provider %q was deleted by a wrong phrase", id1)
	}
}

func TestDeleteProviderUnknown(t *testing.T) {
	srv, _ := newAddTestServer(t)
	rec := deleteProvider(t, srv, "no-such-provider", "DELETE no-such-provider", "test-key-123")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteProviderNoAuth(t *testing.T) {
	srv, _ := newAddTestServer(t)
	id1, _ := addTwoMockProviders(t, srv)
	rec := deleteProvider(t, srv, id1, "DELETE "+id1, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteProviderNoRegistry(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := deleteProvider(t, srv, "anything", "DELETE anything", "test-key-123")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}
