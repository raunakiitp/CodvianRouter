package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/relay"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// Routing contract on /v1/messages (CHECKLIST 211): the canonical
// "routing" field is parsed and forwarded into the engine's
// translate path, so the contract is honoured exactly as on
// /v1/chat/completions — same validation, same fail-closed 400s
// and 402, same trace — while the verbatim path stays contract-free
// by construction (a contract forces translation, so a contracted
// request can never be forwarded untouched).

// messagesContractStack builds the real Server + real router.Engine
// + three real OpenAI provider clients against local mocks, for
// model "m", PLUS an Anthropic mock behind the relay resolver (the
// single-attempt Anthropic target the verbatim path would use).
// Hedging is off so attempt order is deterministic.
func messagesContractStack(t *testing.T, opts ...Option) (*Server, map[string]*contractMock, *anthropicMock, *router.Engine) {
	t.Helper()
	mocks := map[string]*contractMock{}
	clients := map[string]provider.Client{}
	for _, id := range []string{"cheap", "mid", "pricey"} {
		m := newContractMock(t, id)
		mocks[id] = m
		c, err := provider.NewClient(m.server.URL, id+"-key", provider.WithTimeout(2*time.Second))
		if err != nil {
			t.Fatalf("NewClient %s: %v", id, err)
		}
		clients[id] = c
	}
	eng := router.NewEngine(clients, nil, router.Config{
		MaxAttempts:       5,
		HedgeDelay:        -1,
		BackoffBase:       time.Millisecond,
		BackoffMax:        5 * time.Millisecond,
		BreakerThreshold:  5,
		BreakerOpenFor:    time.Minute,
		RetryAfterCap:     30 * time.Second,
		ContractPriceFunc: contractPrices(),
		TrainingSafe:      func(id string) bool { return id == "cheap" },
		ChainFor: func(model string) []router.Attempt {
			if model != "m" {
				return nil
			}
			return []router.Attempt{
				{ProviderID: "cheap", Model: "m"},
				{ProviderID: "mid", Model: "m"},
				{ProviderID: "pricey", Model: "m"},
			}
		},
	})
	anth := &anthropicMock{jsonBody: anthropicJSONReply}
	anthSrv := httptest.NewServer(anth.handler(t))
	t.Cleanup(anthSrv.Close)
	resolver := &fakeRelayResolver{targets: map[string]*relayTarget{
		"m": {
			ProviderID: "anth", ProviderName: "Anthropic", WireFormat: "anthropic",
			BaseURL: anthSrv.URL, AuthHeaders: map[string]string{"x-api-key": "sk-ant-test"},
			UpstreamModel: "m", Attempts: 1,
		},
	}}
	all := append([]Option{WithRelayResolver(resolver), WithMetrics(metrics.NewRecorder()), WithTimeout(10 * time.Second)}, opts...)
	srv := newTestServer(t, eng, all...)
	return srv, mocks, anth, eng
}

// messagesBody renders one /v1/messages request for model "m".
// routing is the raw "routing" value ("" = no routing field).
func messagesBody(t *testing.T, routing string, stream bool) string {
	t.Helper()
	s := ""
	if stream {
		s = `,"stream":true`
	}
	if routing == "" {
		return `{"model":"m","max_tokens":64` + s + `,"messages":[{"role":"user","content":"Hi"}]}`
	}
	return `{"model":"m","max_tokens":64` + s + `,"messages":[{"role":"user","content":"Hi"}],"routing":` + routing + `}`
}

func messagesReq(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, srv, http.MethodPost, "/v1/messages", body, "test-key-123")
}

func TestMessagesContractOrder(t *testing.T) {
	srv, mocks, anth, _ := messagesContractStack(t)
	rec := messagesReq(t, srv, messagesBody(t, `{"order":["pricey","cheap"]}`, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Relay"); !strings.HasPrefix(got, "translated") || !strings.Contains(got, "routing contract present") {
		t.Fatalf("X-Codvian-Relay = %q, want translated with the routing-contract reason", got)
	}
	if !strings.Contains(rec.Body.String(), `"text":"served-by-pricey"`) {
		t.Fatalf("body = %s, want the pricey mock's content (first in order)", rec.Body.String())
	}
	if got := mocks["cheap"].hits.Load(); got != 0 {
		t.Fatalf("cheap hits = %d, want 0", got)
	}
	if got := mocks["mid"].hits.Load(); got != 0 {
		t.Fatalf("mid hits = %d, want 0: not in the order, never tried", got)
	}
	anth.mu.Lock()
	verbatimCalls := len(anth.bodies)
	anth.mu.Unlock()
	if verbatimCalls != 0 {
		t.Fatalf("anthropic mock called %d times, want 0: a contracted request must never go verbatim", verbatimCalls)
	}
	tr := contractTrace(t, srv)
	if tr.Endpoint != "messages" {
		t.Fatalf("trace endpoint = %q, want messages", tr.Endpoint)
	}
	if tr.Contract == nil {
		t.Fatalf("trace has no contract")
	}
	if len(tr.Contract.Order) != 2 || tr.Contract.Order[0] != "pricey" {
		t.Fatalf("trace contract order = %v, want [pricey cheap]", tr.Contract.Order)
	}
	if len(tr.Contract.Excluded) != 1 || tr.Contract.Excluded[0].Provider != "mid" || tr.Contract.Excluded[0].Reason != "not_in_order" {
		t.Fatalf("trace exclusions = %+v, want [{mid not_in_order}]", tr.Contract.Excluded)
	}
	if len(tr.Attempts) != 1 || tr.Attempts[0].Provider != "pricey" || tr.Attempts[0].Outcome != "success" {
		t.Fatalf("trace attempts = %+v, want one pricey/success", tr.Attempts)
	}
}

func TestMessagesContractOnlyAndExclude(t *testing.T) {
	srv, mocks, _, _ := messagesContractStack(t)
	rec := messagesReq(t, srv, messagesBody(t, `{"only":["mid"]}`, false))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"served-by-mid"`) {
		t.Fatalf("only: status = %d, body = %s, want 200 with mid's content", rec.Code, rec.Body.String())
	}
	if got := mocks["cheap"].hits.Load(); got != 0 {
		t.Fatalf("only: cheap hits = %d, want 0", got)
	}
	tr := contractTrace(t, srv)
	if tr.Contract == nil || len(tr.Contract.Excluded) != 2 {
		t.Fatalf("only: trace contract = %+v, want 2 exclusions", tr.Contract)
	}

	rec = messagesReq(t, srv, messagesBody(t, `{"exclude":["cheap"]}`, false))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"served-by-mid"`) {
		t.Fatalf("exclude: status = %d, body = %s, want 200 with mid's content", rec.Code, rec.Body.String())
	}
	if got := mocks["cheap"].hits.Load(); got != 0 {
		t.Fatalf("exclude: cheap hits = %d, want 0", got)
	}
}

func TestMessagesContractSort(t *testing.T) {
	srv, _, _, eng := messagesContractStack(t)
	rec := messagesReq(t, srv, messagesBody(t, `{"sort":"price"}`, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Strategy"); got != "cheapest" {
		t.Fatalf("X-Codvian-Strategy = %q, want cheapest", got)
	}
	tr := contractTrace(t, srv)
	if tr.Contract == nil || tr.Contract.Strategy != "cheapest" {
		t.Fatalf("trace contract = %+v, want strategy cheapest", tr.Contract)
	}
	if info, ok := eng.LastRouting(); !ok || info.Strategy != "cheapest" {
		t.Fatalf("LastRouting = %+v, want strategy cheapest", info)
	}
}

func TestMessagesContractMaxPrice402(t *testing.T) {
	srv, mocks, anth, _ := messagesContractStack(t)
	rec := messagesReq(t, srv, messagesBody(t, `{"max_price":{"input":0.5}}`, false))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "max_price") {
		t.Fatalf("body = %s, want it to name max_price", rec.Body.String())
	}
	for id, m := range mocks {
		if got := m.hits.Load(); got != 0 {
			t.Fatalf("%s hits = %d, want 0: a 402 must dial nobody", id, got)
		}
	}
	anth.mu.Lock()
	verbatimCalls := len(anth.bodies)
	anth.mu.Unlock()
	if verbatimCalls != 0 {
		t.Fatalf("anthropic mock called %d times, want 0", verbatimCalls)
	}
	tr := contractTrace(t, srv)
	if tr.Endpoint != "messages" {
		t.Fatalf("trace endpoint = %q, want messages (a rejected messages contract still names its surface)", tr.Endpoint)
	}
	if tr.Contract == nil || tr.Contract.Error == "" || !strings.Contains(tr.Contract.Error, "max_price") {
		t.Fatalf("trace contract = %+v, want a max_price rejection recorded", tr.Contract)
	}
	if len(tr.Attempts) != 0 {
		t.Fatalf("trace attempts = %d, want 0", len(tr.Attempts))
	}
}

func TestMessagesContractUnknownProvider400(t *testing.T) {
	srv, mocks, _, _ := messagesContractStack(t)
	rec := messagesReq(t, srv, messagesBody(t, `{"only":["ghost"]}`, false))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ghost") {
		t.Fatalf("body = %s, want it to name the unknown provider", rec.Body.String())
	}
	for id, m := range mocks {
		if got := m.hits.Load(); got != 0 {
			t.Fatalf("%s hits = %d, want 0", id, got)
		}
	}
}

func TestMessagesContractUnknownField400(t *testing.T) {
	srv, mocks, anth, _ := messagesContractStack(t)
	rec := messagesReq(t, srv, messagesBody(t, `{"excludes":["cheap"]}`, false))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown field") || !strings.Contains(rec.Body.String(), "excludes") {
		t.Fatalf("body = %s, want it to name the unknown field", rec.Body.String())
	}
	// Fail-closed before ANY forward: neither the engine mocks
	// nor the verbatim Anthropic target was dialled.
	for id, m := range mocks {
		if got := m.hits.Load(); got != 0 {
			t.Fatalf("%s hits = %d, want 0", id, got)
		}
	}
	anth.mu.Lock()
	verbatimCalls := len(anth.bodies)
	anth.mu.Unlock()
	if verbatimCalls != 0 {
		t.Fatalf("anthropic mock called %d times, want 0: a malformed contract must fail before any forward", verbatimCalls)
	}
	// The message is identical to chat completions' for the same
	// mistake — the client gets one spelling to fix, not two.
	chatRec := contractChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"excludes":["cheap"]}}`)
	if chatRec.Code != http.StatusBadRequest {
		t.Fatalf("chat unknown-field: status = %d, want 400", chatRec.Code)
	}
	var msgBody, chatBody struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msgBody); err != nil {
		t.Fatalf("messages error body is not JSON: %v", err)
	}
	if err := json.Unmarshal(chatRec.Body.Bytes(), &chatBody); err != nil {
		t.Fatalf("chat error body is not JSON: %v", err)
	}
	if msgBody.Error.Message != chatBody.Error.Message {
		t.Errorf("messages 400 = %q\nchat 400      = %q\nwant identical validation errors", msgBody.Error.Message, chatBody.Error.Message)
	}
}

func TestMessagesContractHeaderRejected(t *testing.T) {
	srv, mocks, anth, _ := messagesContractStack(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(messagesBody(t, "", false)))
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("X-Codvian-Routing", `{"only":["mid"]}`)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "routing") || !strings.Contains(rec.Body.String(), "request-body field") {
		t.Fatalf("body = %s, want it to point at the routing body field", rec.Body.String())
	}
	for id, m := range mocks {
		if got := m.hits.Load(); got != 0 {
			t.Fatalf("%s hits = %d, want 0: a rejected header must dial nobody", id, got)
		}
	}
	anth.mu.Lock()
	verbatimCalls := len(anth.bodies)
	anth.mu.Unlock()
	if verbatimCalls != 0 {
		t.Fatalf("anthropic mock called %d times, want 0", verbatimCalls)
	}
}

func TestMessagesContractStreamingOnly(t *testing.T) {
	srv, mocks, _, _ := messagesContractStack(t)
	rec := messagesReq(t, srv, messagesBody(t, `{"only":["mid"]}`, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"text":"stream-mid"`) {
		t.Fatalf("body = %s, want the mid mock's streamed content", body)
	}
	if strings.Contains(body, "stream-cheap") || strings.Contains(body, "stream-pricey") {
		t.Fatalf("body = %s, want no content from any other mock", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("body = %s, want the Anthropic envelope to close with message_stop", body)
	}
	if got := mocks["cheap"].hits.Load(); got != 0 {
		t.Fatalf("cheap hits = %d, want 0", got)
	}
	if got := mocks["pricey"].hits.Load(); got != 0 {
		t.Fatalf("pricey hits = %d, want 0", got)
	}
	if got := mocks["mid"].hits.Load(); got != 1 {
		t.Fatalf("mid hits = %d, want 1", got)
	}
	tr := contractTrace(t, srv)
	if tr.Endpoint != "messages" {
		t.Fatalf("trace endpoint = %q, want messages on the stream path too", tr.Endpoint)
	}
}

func TestMessagesContractDataCollectionDeny(t *testing.T) {
	srv, mocks, _, _ := messagesContractStack(t)
	rec := messagesReq(t, srv, messagesBody(t, `{"data_collection":"deny"}`, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"served-by-cheap"`) {
		t.Fatalf("body = %s, want cheap's content (only verified provider)", rec.Body.String())
	}
	if got := mocks["mid"].hits.Load(); got != 0 {
		t.Fatalf("mid hits = %d, want 0", got)
	}
	tr := contractTrace(t, srv)
	if tr.Contract == nil || tr.Contract.DataCollection != "deny" {
		t.Fatalf("trace contract = %+v, want data_collection deny", tr.Contract)
	}
}

// TestMessagesContractNeverServedFromCache proves the messages-side
// cache lookup honours the same rule the engine's cache wrapper
// enforces on chat (internal/degrade): a request carrying a
// non-empty routing contract never consults the cache, because a
// cached entry holds content only, with no record of which provider
// produced it — serving it could hand back an answer from a
// provider the contract forbade.
func TestMessagesContractNeverServedFromCache(t *testing.T) {
	exact := cache.NewExactCache(500)
	semantic := cache.NewLocalSemanticCache()
	srv, mocks, _, _ := messagesContractStack(t, WithCacheServing(true), WithCaches(exact, semantic))
	raw := messagesBody(t, `{"only":["mid"]}`, false)
	parsed, err := relay.ParseAnthropicDetailed([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	req := anthropicToChatRequest(parsed.Request)
	req.CallerID = testCallerID(t, srv)
	key, err := scopedMessagesKey(req)
	if err != nil {
		t.Fatalf("scopedMessagesKey: %v", err)
	}
	exact.Set(context.Background(), key, cache.CachedResponse{
		Content: "Cached hello", Usage: cache.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}, Model: "m",
	}, time.Hour)
	rec := messagesReq(t, srv, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Cache"); got != "" {
		t.Fatalf("X-Codvian-Cache = %q, want empty: the cache must not serve a contracted request", got)
	}
	if strings.Contains(rec.Body.String(), `"text":"Cached hello"`) {
		t.Fatalf("body = %s, want the engine's answer, not the cached one", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"served-by-mid"`) {
		t.Fatalf("body = %s, want mid's live content", rec.Body.String())
	}
	if got := mocks["mid"].hits.Load(); got != 1 {
		t.Fatalf("mid hits = %d, want 1: the request must route live", got)
	}
}

// TestMessagesVerbatimStaysContractFree pins the verbatim half of
// the rule: a request with NO contract still relays verbatim,
// byte-identical — and it publishes no trace contract at all,
// because the engine was never involved.
func TestMessagesVerbatimStaysContractFree(t *testing.T) {
	srv, _, anth, _ := messagesContractStack(t)
	raw := messagesBody(t, "", false)
	rec := messagesReq(t, srv, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Relay"); got != "verbatim" {
		t.Fatalf("X-Codvian-Relay = %q, want verbatim for a contract-free request", got)
	}
	if got := string(anth.lastBody(t)); got != raw {
		t.Fatalf("upstream received:\n%s\nwant byte-identical:\n%s", got, raw)
	}
	tr := contractTrace(t, srv)
	if tr.Contract != nil {
		t.Fatalf("trace contract = %+v, want none: a verbatim request publishes no contract", tr.Contract)
	}
	if len(tr.Attempts) != 0 {
		t.Fatalf("trace attempts = %+v, want none: a verbatim request never reaches the engine", tr.Attempts)
	}
}

// TestMessagesEmptyContractIsNoContract pins the empty-contract
// rule: "routing": {} is the same as no contract — the request
// still relays verbatim instead of being forced through the
// translate path.
func TestMessagesEmptyContractIsNoContract(t *testing.T) {
	srv, _, anth, _ := messagesContractStack(t)
	raw := messagesBody(t, `{}`, false)
	rec := messagesReq(t, srv, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Relay"); got != "verbatim" {
		t.Fatalf("X-Codvian-Relay = %q, want verbatim for an empty contract", got)
	}
	if got := string(anth.lastBody(t)); got != raw {
		t.Fatalf("upstream received:\n%s\nwant byte-identical:\n%s", got, raw)
	}
}

func TestDashboardServesMessagesEndpointLabel(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Endpoint: messages — Anthropic Messages dialect",
		"/v1/messages",
		"verbatim relay never carries one",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing messages-endpoint label %q", want)
		}
	}
	if strings.Contains(body, "Playground") {
		t.Fatal("dashboard must not contain Playground")
	}
}
