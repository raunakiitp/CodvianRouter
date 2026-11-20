package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// contractMock is one mock OpenAI-compatible upstream for the
// routing-contract server tests. It counts hits and answers with
// its own id as content (non-streaming) or as one SSE delta
// (streaming), so the test can tell exactly who served.
type contractMock struct {
	id     string
	hits   atomic.Int32
	server *httptest.Server
}

func newContractMock(t *testing.T, id string) *contractMock {
	t.Helper()
	m := &contractMock{id: id}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"stream-%s\"},\"finish_reason\":null}]}\n\n", id)
			fmt.Fprintf(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprintf(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-%s","object":"chat.completion","model":"m","choices":[{"message":{"role":"assistant","content":"served-by-%s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`, id, id)
	}))
	t.Cleanup(m.server.Close)
	return m
}

func contractPrices() func(string, string) (config.ModelPrice, bool) {
	f := func(v float64) *float64 { return &v }
	return func(providerID, model string) (config.ModelPrice, bool) {
		switch providerID {
		case "cheap":
			return config.ModelPrice{InputUSDPer1M: f(1), OutputUSDPer1M: f(2)}, true
		case "mid":
			return config.ModelPrice{InputUSDPer1M: f(5), OutputUSDPer1M: f(10)}, true
		case "pricey":
			return config.ModelPrice{InputUSDPer1M: f(50), OutputUSDPer1M: f(100)}, true
		}
		return config.ModelPrice{}, false
	}
}

// contractStack builds the real Server + real router.Engine +
// three real provider clients against local mocks, for model "m".
// Hedging is off so attempt order is deterministic.
func contractStack(t *testing.T) (*Server, map[string]*contractMock, *router.Engine) {
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
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithTimeout(10*time.Second))
	return srv, mocks, eng
}

func contractChat(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
}

func contractTrace(t *testing.T, srv *Server) traceResponse {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/dashboard/trace", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/trace status = %d, want 200", rec.Code)
	}
	var tr traceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tr); err != nil {
		t.Fatalf("decode trace: %v (body %s)", err, rec.Body.String())
	}
	return tr
}

func TestServerContractOrder(t *testing.T) {
	srv, mocks, _ := contractStack(t)
	rec := contractChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"order":["pricey","cheap"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served-by-pricey") {
		t.Fatalf("body = %s, want the pricey mock's content (first in order)", rec.Body.String())
	}
	if got := mocks["cheap"].hits.Load(); got != 0 {
		t.Fatalf("cheap hits = %d, want 0", got)
	}
	if got := mocks["mid"].hits.Load(); got != 0 {
		t.Fatalf("mid hits = %d, want 0: not in the order, never tried", got)
	}
	tr := contractTrace(t, srv)
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

func TestServerContractOnlyAndExclude(t *testing.T) {
	srv, mocks, _ := contractStack(t)
	rec := contractChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"only":["mid"]}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "served-by-mid") {
		t.Fatalf("only: status = %d, body = %s, want 200 with mid's content", rec.Code, rec.Body.String())
	}
	if got := mocks["cheap"].hits.Load(); got != 0 {
		t.Fatalf("only: cheap hits = %d, want 0", got)
	}
	tr := contractTrace(t, srv)
	if tr.Contract == nil || len(tr.Contract.Excluded) != 2 {
		t.Fatalf("only: trace contract = %+v, want 2 exclusions", tr.Contract)
	}

	rec = contractChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"exclude":["cheap"]}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "served-by-mid") {
		t.Fatalf("exclude: status = %d, body = %s, want 200 with mid's content", rec.Code, rec.Body.String())
	}
	if got := mocks["cheap"].hits.Load(); got != 0 {
		t.Fatalf("exclude: cheap hits = %d, want 0", got)
	}
}

func TestServerContractSort(t *testing.T) {
	srv, mocks, eng := contractStack(t)
	rec := contractChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"sort":"price"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// sort=price maps to the cheapest strategy; the trace records
	// it, and the response carries it as X-Codvian-Strategy.
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
	_ = mocks
}

func TestServerContractMaxPrice402(t *testing.T) {
	srv, mocks, _ := contractStack(t)
	rec := contractChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"max_price":{"input":0.5}}}`)
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
	tr := contractTrace(t, srv)
	if tr.Contract == nil || tr.Contract.Error == "" || !strings.Contains(tr.Contract.Error, "max_price") {
		t.Fatalf("trace contract = %+v, want a max_price rejection recorded", tr.Contract)
	}
	if len(tr.Attempts) != 0 {
		t.Fatalf("trace attempts = %d, want 0", len(tr.Attempts))
	}
}

func TestServerContractUnknownProvider400(t *testing.T) {
	srv, mocks, _ := contractStack(t)
	rec := contractChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"only":["ghost"]}}`)
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

func TestServerContractHeaderRejected(t *testing.T) {
	srv, mocks, _ := contractStack(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
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
}

func TestServerContractUnknownRoutingField400(t *testing.T) {
	srv, mocks, _ := contractStack(t)
	rec := contractChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"excludes":["cheap"]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown field") || !strings.Contains(rec.Body.String(), "excludes") {
		t.Fatalf("body = %s, want it to name the unknown field", rec.Body.String())
	}
	for id, m := range mocks {
		if got := m.hits.Load(); got != 0 {
			t.Fatalf("%s hits = %d, want 0", id, got)
		}
	}
}

func TestServerContractStreamingOnly(t *testing.T) {
	srv, mocks, _ := contractStack(t)
	rec := contractChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"routing":{"only":["mid"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stream-mid") {
		t.Fatalf("body = %s, want the mid mock's streamed content", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "stream-cheap") || strings.Contains(rec.Body.String(), "stream-pricey") {
		t.Fatalf("body = %s, want no content from any other mock", rec.Body.String())
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
}

func TestServerContractDataCollectionDeny(t *testing.T) {
	srv, mocks, _ := contractStack(t)
	rec := contractChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"data_collection":"deny"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served-by-cheap") {
		t.Fatalf("body = %s, want cheap's content (only verified provider)", rec.Body.String())
	}
	if got := mocks["mid"].hits.Load(); got != 0 {
		t.Fatalf("mid hits = %d, want 0", got)
	}
	tr := contractTrace(t, srv)
	if tr.Contract == nil || tr.Contract.DataCollection != "deny" {
		t.Fatalf("trace contract = %+v, want data_collection deny", tr.Contract)
	}
	for _, x := range tr.Contract.Excluded {
		if x.Reason != "data_collection_deny" {
			t.Fatalf("exclusion %+v: reason want data_collection_deny", x)
		}
	}
}

func TestDashboardServesContractExplainer(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Per-request routing", "max_price", "data_collection", "X-Codvian-Routing"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing contract explainer %q", want)
		}
	}
	if strings.Contains(body, "Playground") {
		t.Fatal("dashboard must not contain Playground")
	}
}

func TestTraceContractJSONShape(t *testing.T) {
	// The feed shape the dashboard JS reads: attempts + the
	// contract block with its exclusions. Asserted here so a
	// rename breaks a test, not the dashboard at runtime.
	tr := traceResponse{
		Attempts: []routetrace.AttemptTrace{{Provider: "a", Model: "m", Outcome: "success"}},
		Contract: &routetrace.RoutingContract{
			Only:     []string{"a"},
			Strategy: "balanced",
			Excluded: []routetrace.RoutingExclusion{{Provider: "b", Reason: "not_in_only"}},
		},
	}
	b, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"attempts"`, `"contract"`, `"only":["a"]`, `"strategy":"balanced"`, `"excluded"`, `"reason":"not_in_only"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("trace JSON %s missing %s", b, want)
		}
	}
}
