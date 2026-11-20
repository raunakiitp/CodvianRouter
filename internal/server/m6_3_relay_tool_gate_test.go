// M6.3 relay tool-gate hole: forwardVerbatim's async-guardrail
// branch stored a non-streaming response with no cacheable check,
// while the normal branch gates the store on `cacheable` — so a
// tool-carrying /v1/messages request under async guardrails could
// store a tool-less response (ShouldStore inside storeMessagesCache
// only inspects the RESPONSE's tool calls, never the request's).
// These tests prove, through the live handler with a real async
// guardrail run, that a tool-carrying request is NOT stored while a
// tool-free request under the same async run IS stored (the control
// proves the harness can tell stored from not-stored).
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/relay"
)

// m63AsyncRelayServer mirrors messagesAsyncServer but wires the
// relay caches in (and cache serving on), so a store-back is
// observable via lookupMessagesCache.
func m63AsyncRelayServer(t *testing.T, mock *messagesAsyncMock, store *guardrails.Store, exact *cache.ExactCache, semantic *cache.LocalSemanticCache) *Server {
	t.Helper()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	resolver := &fakeRelayResolver{targets: map[string]*relayTarget{
		"claude-sonnet-4-5": {
			ProviderID: "anth", ProviderName: "Anthropic", WireFormat: "anthropic",
			BaseURL: srv.URL, AuthHeaders: map[string]string{"x-api-key": "sk-ant-test"},
			UpstreamModel: "claude-sonnet-4-5", Attempts: 1,
		},
	}}
	return newTestServer(t, &fakeEngine{}, WithRelayResolver(resolver), WithGuardrails(store), WithTimeout(60*time.Second), WithCaches(exact, semantic), WithCacheServing(true))
}

// m63RelayLookup probes the relay cache for raw exactly as the
// handler stored it: canonical request + the caller's key id, so
// the exact key (which includes the tools) matches.
func m63RelayLookup(t *testing.T, srv *Server, raw string) (cache.CachedResponse, string, bool) {
	t.Helper()
	parsed, err := relay.ParseAnthropicDetailed([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	req := anthropicToChatRequest(parsed.Request)
	req.CallerID = testCallerID(t, srv)
	return srv.lookupMessagesCache(context.Background(), req)
}

const m63ToolMessagesRaw = `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"tools":[{"name":"get_weather","description":"Get the weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`

const m63PlainMessagesRaw = `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`

// TestM63AsyncRelayToolRequestNotStored (M6.3): a tool-carrying
// non-streaming /v1/messages request under async guardrails is
// served once but stored nowhere — neither layer — even though the
// upstream reply itself carries no tool calls.
func TestM63AsyncRelayToolRequestNotStored(t *testing.T) {
	classifier := newAsyncClassifier(t, "allow", "")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	close(classifier.release)
	mock := &messagesAsyncMock{mode: "instant-json"}
	srv := m63AsyncRelayServer(t, mock, store, cache.NewExactCache(500), cache.NewLocalSemanticCache())

	// Precondition: the tool-carrying request is not cacheable.
	parsed, err := relay.ParseAnthropicDetailed([]byte(m63ToolMessagesRaw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if relay.Cacheable(parsed.Request) {
		t.Fatal("tool-carrying request parses as cacheable; the test would not exercise the gate")
	}

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", m63ToolMessagesRaw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != anthropicJSONReply {
		t.Fatalf("body differs from the upstream JSON:\n got %q\nwant %q", rec.Body.String(), anthropicJSONReply)
	}
	if mock.hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", mock.hits.Load())
	}
	// The async run really happened (an ALLOW verdict on the
	// messages surface): without it the request would have taken
	// the already-gated normal branch and this test would be
	// vacuous.
	pollMessagesAsyncEvent(t, srv, guardrails.AsyncOutcomeAllowed)
	if hit, layer, found := m63RelayLookup(t, srv, m63ToolMessagesRaw); found {
		t.Fatalf("tool-carrying async relay response was stored (%s layer, content %q); the async branch must gate on cacheable like the normal branch", layer, hit.Content)
	}
}

// TestM63AsyncRelayPlainRequestStored (M6.3 control): the same
// async-guardrail relay stores a tool-free request — the async
// branch still stores cacheable responses, and the harness can
// tell stored from not-stored.
func TestM63AsyncRelayPlainRequestStored(t *testing.T) {
	classifier := newAsyncClassifier(t, "allow", "")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	close(classifier.release)
	mock := &messagesAsyncMock{mode: "instant-json"}
	srv := m63AsyncRelayServer(t, mock, store, cache.NewExactCache(500), cache.NewLocalSemanticCache())

	parsed, err := relay.ParseAnthropicDetailed([]byte(m63PlainMessagesRaw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !relay.Cacheable(parsed.Request) {
		t.Fatal("tool-free request parses as not cacheable; the control would not exercise the store")
	}

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", m63PlainMessagesRaw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != anthropicJSONReply {
		t.Fatalf("body differs from the upstream JSON:\n got %q\nwant %q", rec.Body.String(), anthropicJSONReply)
	}
	if mock.hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", mock.hits.Load())
	}
	pollMessagesAsyncEvent(t, srv, guardrails.AsyncOutcomeAllowed)
	hit, layer, found := m63RelayLookup(t, srv, m63PlainMessagesRaw)
	if !found {
		t.Fatal("tool-free async relay response was not stored; the control must store (async branch still stores cacheable responses)")
	}
	if hit.Content != "Hello from Claude" {
		t.Fatalf("stored cache content = %q (layer %s), want %q", hit.Content, layer, "Hello from Claude")
	}
}
