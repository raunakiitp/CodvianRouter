package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPluginHeadersAppliedToUpstream proves the pre-request plugin
// header set (carried on the request context by the server) reaches
// the upstream, and that reserved headers can never be overridden
// through that path.
func TestPluginHeadersAppliedToUpstream(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithExtraHeaders(context.Background(), map[string]string{
		"X-Codvian-Plugin": "header-add",
		"Authorization":    "attacker-value",
		"Content-Type":     "text/evil",
	})
	_, err = c.Chat(ctx, ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Codvian-Plugin") != "header-add" {
		t.Fatalf("plugin header missing upstream: %v", got)
	}
	if got.Get("Authorization") != "Bearer "+"test-key" {
		t.Fatalf("Authorization overridden by plugin headers (got len %d)", len(got.Get("Authorization")))
	}
	if got.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type overridden by plugin headers: %q", got.Get("Content-Type"))
	}
}

// TestWithExtraHeadersCopies proves the context helper copies the
// map: mutating the caller's map afterwards cannot change what the
// client sends.
func TestWithExtraHeadersCopies(t *testing.T) {
	m := map[string]string{"X-A": "1"}
	ctx := WithExtraHeaders(context.Background(), m)
	m["X-A"] = "2"
	if got := extraHeadersFrom(ctx); got["X-A"] != "1" {
		t.Fatalf("headers = %v, want copy", got)
	}
	if extraHeadersFrom(context.Background()) != nil {
		t.Fatal("empty context carries headers")
	}
}

// TestHTTPAdapterAppliedAndFailClosed proves the registry-attached
// adapter rewrites the outgoing request for its provider, and that
// an adapter error fails the attempt closed: no request reaches
// the upstream at all.
func TestHTTPAdapterAppliedAndFailClosed(t *testing.T) {
	var saw http.Header
	var hits int
	m := &mockUpstream{
		modelsStatus: 200,
		modelsBody:   `{"data":[{"id":"model-a"}]}`,
		chatStatus:   200,
		chatBody:     `{"model":"model-a","choices":[{"message":{"role":"assistant","content":"hi"}}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			hits++
			saw = r.Header.Clone()
		}
		m.handler(t)(w, r)
	}))
	defer srv.Close()

	reg := NewRegistry(nil)
	if _, err := reg.Add(srv.URL, "k"); err != nil {
		t.Fatalf("add: %v", err)
	}
	// The registry assigns its own ID (derived from the name);
	// resolve it from List.
	providers := reg.List()
	if len(providers) != 1 {
		t.Fatalf("providers = %d", len(providers))
	}
	providerID := providers[0].ID

	reg.SetHTTPAdapter(func(_ context.Context, id string, req *http.Request) (*http.Request, error) {
		if id != providerID {
			t.Errorf("adapter called for wrong provider %q", id)
		}
		clone := req.Clone(context.Background())
		clone.Header.Set("X-Acme-Wire", "v2")
		return clone, nil
	})
	client, err := reg.ClientFor(providerID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
	if saw.Get("X-Acme-Wire") != "v2" {
		t.Fatalf("adapter header missing upstream: %v", saw)
	}

	// Adapter error: fail closed — Chat errors and the upstream is
	// never hit.
	reg.SetHTTPAdapter(func(_ context.Context, _ string, _ *http.Request) (*http.Request, error) {
		return nil, errors.New("cannot adapt this wire")
	})
	client2, err := reg.ClientFor(providerID)
	if err != nil {
		t.Fatal(err)
	}
	hitsBefore := hits
	if _, err := client2.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("adapter error returned success")
	}
	if hits != hitsBefore {
		t.Fatalf("hits grew %d -> %d: unadapted request was sent", hitsBefore, hits)
	}
}
