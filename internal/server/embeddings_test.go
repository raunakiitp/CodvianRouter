// POST /v1/embeddings + POST /v1/rerank server tests (CHECKLIST
// 171-172): auth, validation, the guardrail block with zero
// provider calls, the pre-send budget refusal, the typed
// no-capable-provider 400, the 503 when the engine has no
// embeddings capability, and the trace endpoint passthrough.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// embedFakeEngine is a fakeEngine that also serves embeddings and
// rerank, with call counters and scripted results. LastTrace
// returns the canned trace the test sets, so the trace-endpoint
// passthrough can be exercised without a real engine.
type embedFakeEngine struct {
	*fakeEngine
	embedCalls  int32
	rerankCalls int32
	embedResp   *provider.EmbeddingResponse
	rerankResp  *provider.RerankResponse
	embedErr    error
	rerankErr   error
	trace       *routetrace.RouteTrace
	// lastEmbedModel / lastRerankModel record the model the
	// handler actually handed to the engine (CHECKLIST 219-220:
	// pins that a plugin model rewrite reaches the real request).
	lastEmbedModel  string
	lastRerankModel string
}

func (e *embedFakeEngine) Embed(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	atomic.AddInt32(&e.embedCalls, 1)
	e.lastEmbedModel = req.Model
	if e.embedErr != nil {
		return nil, e.embedErr
	}
	if e.embedResp != nil {
		return e.embedResp, nil
	}
	return &provider.EmbeddingResponse{
		Model: req.Model,
		Data:  []provider.Embedding{{Object: "embedding", Embedding: json.RawMessage(`[0.1,0.2,0.3]`), Index: 0}},
		Usage: provider.Usage{PromptTokens: 4, TotalTokens: 4},
	}, nil
}

func (e *embedFakeEngine) Rerank(ctx context.Context, req provider.RerankRequest) (*provider.RerankResponse, error) {
	atomic.AddInt32(&e.rerankCalls, 1)
	e.lastRerankModel = req.Model
	if e.rerankErr != nil {
		return nil, e.rerankErr
	}
	if e.rerankResp != nil {
		return e.rerankResp, nil
	}
	return &provider.RerankResponse{
		Model:   req.Model,
		Results: []provider.RerankResult{{Index: 1, RelevanceScore: 0.92}, {Index: 0, RelevanceScore: 0.31}},
	}, nil
}

func (e *embedFakeEngine) LastTrace() *routetrace.RouteTrace { return e.trace }

func doEmbedReq(t *testing.T, srv *Server, path, body, key string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestEmbeddingsAuthRequired(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	body := `{"model":"emb-model","input":"hello"}`
	for _, key := range []string{"", "wrong-key"} {
		rec := doEmbedReq(t, srv, "/v1/embeddings", body, key, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", key, rec.Code)
		}
	}
	if eng.embedCalls != 0 {
		t.Errorf("embed calls = %d, want 0 (auth first)", eng.embedCalls)
	}
}

func TestRerankAuthRequired(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	rec := doEmbedReq(t, srv, "/v1/rerank", `{"model":"m","query":"q","documents":["d"]}`, "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if eng.rerankCalls != 0 {
		t.Errorf("rerank calls = %d, want 0", eng.rerankCalls)
	}
}

func TestEmbeddingsValidation(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	for _, body := range []string{
		`{"input":"hello"}`,
		`{"model":"m"}`,
		`{"model":"m","input":""}`,
		`{"model":"m","input":[]}`,
		`not json`,
	} {
		rec := doEmbedReq(t, srv, "/v1/embeddings", body, "test-key-123", nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
	if eng.embedCalls != 0 {
		t.Errorf("embed calls = %d, want 0 (validation is local)", eng.embedCalls)
	}
}

func TestRerankValidation(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	for _, body := range []string{
		`{"query":"q","documents":["d"]}`,
		`{"model":"m","documents":["d"]}`,
		`{"model":"m","query":"q"}`,
		`{"model":"m","query":"q","documents":[]}`,
	} {
		rec := doEmbedReq(t, srv, "/v1/rerank", body, "test-key-123", nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
	if eng.rerankCalls != 0 {
		t.Errorf("rerank calls = %d, want 0", eng.rerankCalls)
	}
}

func TestEmbeddingsSuccess(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	rec := doEmbedReq(t, srv, "/v1/embeddings", `{"model":"emb-model","input":"hello world"}`, "test-key-123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Endpoint"); got != "embeddings" {
		t.Errorf("X-Codvian-Endpoint = %q, want embeddings", got)
	}
	var resp provider.EmbeddingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	if len(resp.Data) != 1 || string(resp.Data[0].Embedding) != `[0.1,0.2,0.3]` {
		t.Errorf("data = %+v, want the verbatim vector", resp.Data)
	}
	if resp.Usage.PromptTokens != 4 || resp.Usage.TotalTokens != 4 {
		t.Errorf("usage = %+v, want 4/4", resp.Usage)
	}
	if eng.embedCalls != 1 {
		t.Errorf("embed calls = %d, want exactly 1", eng.embedCalls)
	}
}

func TestRerankSuccess(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	rec := doEmbedReq(t, srv, "/v1/rerank", `{"model":"rerank-model","query":"capital?","documents":["Paris is the capital","London is big"],"top_n":2,"return_documents":true}`, "test-key-123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Endpoint"); got != "rerank" {
		t.Errorf("X-Codvian-Endpoint = %q, want rerank", got)
	}
	var resp provider.RerankResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	if len(resp.Results) != 2 || resp.Results[0].Index != 1 || resp.Results[0].RelevanceScore != 0.92 {
		t.Errorf("results = %+v, want the scored pair", resp.Results)
	}
	// No upstream usage in the canned response: the usage block
	// is absent, never invented.
	if resp.Usage != nil {
		t.Errorf("usage = %+v, want nil", resp.Usage)
	}
	if eng.rerankCalls != 1 {
		t.Errorf("rerank calls = %d, want exactly 1", eng.rerankCalls)
	}
}

// An engine without Embed/Rerank answers 503 — an honest
// "this deployment cannot serve this surface", never a faked
// vector or scoring.
func TestEmbeddingsEngineWithoutCapability503(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doEmbedReq(t, srv, "/v1/embeddings", `{"model":"m","input":"hello"}`, "test-key-123", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	rec = doEmbedReq(t, srv, "/v1/rerank", `{"model":"m","query":"q","documents":["d"]}`, "test-key-123", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("rerank status = %d, want 503", rec.Code)
	}
}

// No capable provider is a property of the request + the
// operator's chain, not a provider failure: an honest 400
// naming the endpoint, never a 502.
func TestEmbeddingsNoCapableProvider400(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}, embedErr: &router.NoCapableProviderError{Endpoint: provider.EndpointEmbeddings, Model: "emb-model"}}
	srv := newTestServer(t, eng)
	rec := doEmbedReq(t, srv, "/v1/embeddings", `{"model":"emb-model","input":"hello"}`, "test-key-123", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "embeddings") {
		t.Errorf("body = %s, want it to name the endpoint", rec.Body.String())
	}
}

// An upstream 500 on the single attempt is a provider failure:
// 502 with the upstream-failure code, and the engine was called
// exactly once.
func TestEmbeddingsUpstreamFailure502(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}, embedErr: &provider.UpstreamError{StatusCode: 500, Body: "boom"}}
	srv := newTestServer(t, eng)
	rec := doEmbedReq(t, srv, "/v1/embeddings", `{"model":"m","input":"hello"}`, "test-key-123", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "-32603") {
		t.Errorf("body = %s, want the upstream-failure code -32603", rec.Body.String())
	}
	if eng.embedCalls != 1 {
		t.Errorf("embed calls = %d, want exactly 1", eng.embedCalls)
	}
}

// A guardrail block fires on the input text BEFORE any provider
// is dialled: 403 with the policy-refusal code, zero engine calls.
func TestEmbeddingsGuardrailBlocksWithZeroProviderCalls(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	// Enable the secret rule, like the chat guardrail test.
	rec := doReq(t, srv, http.MethodPost, "/dashboard/guardrails", `{"block_pii":false,"block_secrets":true,"block_injection":false}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle POST status = %d", rec.Code)
	}
	// The input carries a secret-shaped value (a test fixture,
	// not a real credential).
	rec = doEmbedReq(t, srv, "/v1/embeddings", `{"model":"m","input":"my api_key=abcdefgh12345678"}`, "test-key-123", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "-32001") {
		t.Errorf("body = %s, want the policy-refusal code -32001", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("body = %s, want it to name the secret kind", rec.Body.String())
	}
	if eng.embedCalls != 0 {
		t.Errorf("embed calls = %d, want 0 (blocked pre-send)", eng.embedCalls)
	}
}

// A strict per-request budget the prompt estimate exceeds is
// refused 402 BEFORE dialling, with zero engine calls.
func TestEmbeddingsBudgetRefusedPreSend(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng, WithPriceBook(testPriceBook()))
	// model-a costs $2.50/1M input; a long input prices well
	// above the $0.00001 budget.
	longInput := strings.Repeat("hello world ", 100)
	body, _ := json.Marshal(map[string]any{"model": "model-a", "input": longInput})
	rec := doEmbedReq(t, srv, "/v1/embeddings", string(body), "test-key-123", map[string]string{
		"X-Codvian-Budget":        "0.00001",
		"X-Codvian-Budget-Strict": "true",
	})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (body %s)", rec.Code, rec.Body.String())
	}
	if eng.embedCalls != 0 {
		t.Errorf("embed calls = %d, want 0 (refused pre-send)", eng.embedCalls)
	}
}

// The trace endpoint rides through GET /dashboard/trace: after
// an embeddings call the trace names the surface, so the
// dashboard never mistakes it for a chat trace.
func TestTraceEndpointPassthrough(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}, trace: &routetrace.RouteTrace{
		Endpoint: "embeddings",
		Attempts: []routetrace.AttemptTrace{{Provider: "prov-a", Model: "emb-model", Outcome: "success", Preread: "exempt:embeddings"}},
	}}
	srv := newTestServer(t, eng)
	rec := doEmbedReq(t, srv, "/v1/embeddings", `{"model":"emb-model","input":"hello"}`, "test-key-123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("embeddings status = %d", rec.Code)
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/trace", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("trace status = %d", rec.Code)
	}
	var trace struct {
		Endpoint string                    `json:"endpoint"`
		Attempts []routetrace.AttemptTrace `json:"attempts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trace); err != nil {
		t.Fatalf("trace is not JSON: %v (%s)", err, rec.Body.String())
	}
	if trace.Endpoint != "embeddings" {
		t.Errorf("trace endpoint = %q, want embeddings", trace.Endpoint)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Preread != "exempt:embeddings" {
		t.Errorf("trace attempts = %+v, want one exempt:embeddings attempt", trace.Attempts)
	}
}
