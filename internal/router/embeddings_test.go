// Embeddings + rerank engine tests (CHECKLIST 171-172): the
// exactly-once invariant, native-wire skipping, the strict
// budget gate, and honest ledger rows + trace labels.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// embedFakeClient is a fakeClient that also serves embeddings and
// rerank, with per-endpoint scripted functions and call counters.
// It embeds *fakeClient, so Chat/ChatStream/ListModels/TestKey
// and WireFormat are the fake's; setting the fake's wireFormat to
// a native wire ("anthropic", "gemini", ...) makes the engine
// skip it at selection even though the methods exist — exactly
// like the real native clients, which inherit Embed/Rerank from
// *openAIClient but speak no OpenAI wire.
type embedFakeClient struct {
	*fakeClient
	embeds   int
	reranks  int
	embedFn  func(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error)
	rerankFn func(ctx context.Context, req provider.RerankRequest) (*provider.RerankResponse, error)
}

func (c *embedFakeClient) Embed(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	c.mu.Lock()
	c.embeds++
	fn := c.embedFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &provider.EmbeddingResponse{
		Model: req.Model,
		Data:  []provider.Embedding{{Object: "embedding", Embedding: json.RawMessage(`[0.1,0.2]`), Index: 0}},
		Usage: provider.Usage{PromptTokens: 3, TotalTokens: 3},
	}, nil
}

func (c *embedFakeClient) Rerank(ctx context.Context, req provider.RerankRequest) (*provider.RerankResponse, error) {
	c.mu.Lock()
	c.reranks++
	fn := c.rerankFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &provider.RerankResponse{
		Model:   req.Model,
		Results: []provider.RerankResult{{Index: 0, RelevanceScore: 0.9}},
	}, nil
}

func (c *embedFakeClient) embedCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.embeds
}

func (c *embedFakeClient) rerankCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reranks
}

func embedReq() provider.EmbeddingRequest {
	return provider.EmbeddingRequest{Model: "emb-model", Input: json.RawMessage(`"hello world"`)}
}

func rerankReq() provider.RerankRequest {
	return provider.RerankRequest{
		Model:     "rerank-model",
		Query:     "hello world",
		Documents: []json.RawMessage{json.RawMessage(`"doc one"`), json.RawMessage(`"doc two"`)},
	}
}

// Exactly once: the first target's failure ends the request —
// the second target is never dialled, even though it would have
// succeeded. A 429 from the first target is still a failure of
// the one attempt, not a reason to replay.
func TestEmbedExactlyOnceFirstFailureNeverCallsSecond(t *testing.T) {
	a := &embedFakeClient{fakeClient: &fakeClient{}, embedFn: func(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
		return nil, &provider.UpstreamError{StatusCode: 429, Body: "rate limited"}
	}}
	b := &embedFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	_, err := eng.Embed(context.Background(), embedReq())
	if err == nil {
		t.Fatal("want the first target's error, got nil")
	}
	if a.embedCalls() != 1 {
		t.Errorf("first target calls = %d, want 1", a.embedCalls())
	}
	if b.embedCalls() != 0 {
		t.Errorf("second target calls = %d, want 0 (exactly once — no replay)", b.embedCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || trace.Endpoint != "embeddings" {
		t.Fatalf("trace endpoint = %v, want embeddings", trace)
	}
	if len(trace.Attempts) != 1 {
		t.Fatalf("trace attempts = %d, want 1", len(trace.Attempts))
	}
	if trace.Attempts[0].Outcome != "error" {
		t.Errorf("attempt outcome = %q, want error", trace.Attempts[0].Outcome)
	}
	if trace.Attempts[0].Preread != "exempt:embeddings" {
		t.Errorf("preread = %q, want exempt:embeddings", trace.Attempts[0].Preread)
	}
}

// Exactly once, rerank: same invariant on the other surface.
func TestRerankExactlyOnceFirstFailureNeverCallsSecond(t *testing.T) {
	a := &embedFakeClient{fakeClient: &fakeClient{}, rerankFn: func(ctx context.Context, req provider.RerankRequest) (*provider.RerankResponse, error) {
		return nil, errors.New("upstream 500")
	}}
	b := &embedFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	_, err := eng.Rerank(context.Background(), rerankReq())
	if err == nil {
		t.Fatal("want the first target's error, got nil")
	}
	if a.rerankCalls() != 1 || b.rerankCalls() != 0 {
		t.Errorf("rerank calls = first %d, second %d, want 1 and 0", a.rerankCalls(), b.rerankCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || trace.Endpoint != "rerank" {
		t.Fatalf("trace endpoint = %v, want rerank", trace)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Preread != "exempt:rerank" {
		t.Errorf("trace attempts = %+v, want one exempt:rerank attempt", trace.Attempts)
	}
}

// Native-wire selection: a target speaking a native wire is
// skipped BEFORE dialling (it has no embeddings transport in this
// slice), and the next capable target serves the request. The
// skip is in the trace with its reason.
func TestEmbedNativeWireSkippedAtSelection(t *testing.T) {
	native := &embedFakeClient{fakeClient: &fakeClient{wireFormat: "anthropic"}}
	openai := &embedFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a-native": native, "b-openai": openai}, nil, testConfig())
	resp, err := eng.Embed(context.Background(), embedReq())
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if resp == nil || len(resp.Data) != 1 {
		t.Fatalf("resp = %+v, want one embedding", resp)
	}
	if native.embedCalls() != 0 {
		t.Errorf("native-wire calls = %d, want 0 (skipped before dialling)", native.embedCalls())
	}
	if openai.embedCalls() != 1 {
		t.Errorf("openai-wire calls = %d, want 1", openai.embedCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %d, want 2 (skip + serve)", len(trace.Attempts))
	}
	if trace.Attempts[0].Outcome != "skipped_unsupported" {
		t.Errorf("trace[0].Outcome = %q, want skipped_unsupported", trace.Attempts[0].Outcome)
	}
	if trace.Attempts[1].Outcome != "success" || trace.Attempts[1].Preread != "exempt:embeddings" {
		t.Errorf("trace[1] = %+v, want success + exempt:embeddings", trace.Attempts[1])
	}
}

// A target with no client at all (ChainFor naming an
// unregistered provider) is skipped the same way, and the next
// capable target serves.
func TestEmbedSkippedNoClientThenServes(t *testing.T) {
	openai := &embedFakeClient{fakeClient: &fakeClient{}}
	cfg := testConfig()
	cfg.ChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: "ghost", Model: model}, {ProviderID: "b-openai", Model: model}}
	}
	eng := NewEngine(map[string]provider.Client{"b-openai": openai}, nil, cfg)
	resp, err := eng.Embed(context.Background(), embedReq())
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if resp == nil {
		t.Fatal("want a response from the second target")
	}
	if openai.embedCalls() != 1 {
		t.Errorf("openai-wire calls = %d, want 1", openai.embedCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 2 || trace.Attempts[0].Outcome != "skipped_no_client" {
		t.Errorf("trace attempts = %+v, want skipped_no_client then success", trace.Attempts)
	}
}

// No capable provider: every target speaks a native wire, so the
// request fails honestly with the typed error — never a 502, and
// no provider is dialled.
func TestEmbedNoCapableProvider(t *testing.T) {
	native := &embedFakeClient{fakeClient: &fakeClient{wireFormat: "anthropic"}}
	eng := NewEngine(map[string]provider.Client{"a": native}, nil, testConfig())
	_, err := eng.Embed(context.Background(), embedReq())
	if err == nil {
		t.Fatal("want a NoCapableProviderError, got nil")
	}
	var ncp *NoCapableProviderError
	if !errors.As(err, &ncp) {
		t.Fatalf("err = %T (%v), want *NoCapableProviderError", err, err)
	}
	if ncp.Endpoint != provider.EndpointEmbeddings {
		t.Errorf("endpoint = %q, want embeddings", ncp.Endpoint)
	}
	if native.embedCalls() != 0 {
		t.Errorf("native-wire calls = %d, want 0", native.embedCalls())
	}
	// The same shape on rerank.
	_, err = eng.Rerank(context.Background(), rerankReq())
	var ncp2 *NoCapableProviderError
	if !errors.As(err, &ncp2) || ncp2.Endpoint != provider.EndpointRerank {
		t.Errorf("rerank err = %v, want NoCapableProviderError for rerank", err)
	}
}

// A client that does not implement Embed at all (a plain
// fakeClient) is skipped the same way as a native-wire one.
func TestEmbedClientWithoutEmbedderSkipped(t *testing.T) {
	plain := &fakeClient{}
	openai := &embedFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": plain, "b": openai}, nil, testConfig())
	_, err := eng.Embed(context.Background(), embedReq())
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if openai.embedCalls() != 1 {
		t.Errorf("openai-wire calls = %d, want 1", openai.embedCalls())
	}
}

// Strict per-request budget: the priced prompt estimate exceeds
// the budget, so the chain stops BEFORE dialling — zero provider
// calls, a typed 402 error, and a skipped_budget trace entry.
func TestEmbedStrictBudgetStopsBeforeDialling(t *testing.T) {
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	a := &embedFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	budget := 0.00001 // "hello world" on model "cheap" estimates $0.00003
	req := embedReq()
	req.Model = "cheap"
	req.BudgetUSD = &budget
	req.BudgetStrict = true
	_, err := eng.Embed(context.Background(), req)
	if err == nil || !IsRequestBudgetExceeded(err) {
		t.Fatalf("err = %v, want a RequestBudgetExceededError", err)
	}
	if a.embedCalls() != 0 {
		t.Errorf("embed calls = %d, want 0 (blocked pre-send)", a.embedCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 || trace.Attempts[0].Outcome != "skipped_budget" {
		t.Errorf("trace attempts = %+v, want one skipped_budget", trace.Attempts)
	}
}

// Ledger + trace honesty: a successful embeddings call records
// one ledger row with the endpoint and the provider-reported
// usage, and the trace names the endpoint.
func TestEmbedLedgerRowAndTrace(t *testing.T) {
	led := &fakeLedger{}
	a := &embedFakeClient{fakeClient: &fakeClient{}, embedFn: func(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
		return &provider.EmbeddingResponse{
			Model: req.Model,
			Data:  []provider.Embedding{{Object: "embedding", Embedding: json.RawMessage(`[0.1]`), Index: 0}},
			Usage: provider.Usage{PromptTokens: 8, TotalTokens: 8},
		}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"a": a}, led, testConfig())
	_, err := eng.Embed(context.Background(), embedReq())
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if led.count() != 1 {
		t.Fatalf("ledger rows = %d, want 1", led.count())
	}
	row := led.calls[0]
	if !row.ok {
		t.Error("ledger row ok = false, want true")
	}
	if row.usage.Endpoint != provider.EndpointEmbeddings {
		t.Errorf("ledger endpoint = %q, want embeddings", row.usage.Endpoint)
	}
	if row.usage.PromptTokens != 8 || row.usage.TotalTokens != 8 {
		t.Errorf("ledger usage = %+v, want the provider-reported 8/8", row.usage)
	}
	if row.usage.LocalEstimate {
		t.Error("ledger row is marked a local estimate, want provider-reported")
	}
	trace := eng.LastTrace()
	if trace == nil || trace.Endpoint != "embeddings" {
		t.Errorf("trace endpoint = %v, want embeddings", trace)
	}
}

// Keyed ledger path (the production path — the server's ledger
// adapter is a KeyedLedger): the row carries the caller's key ID
// and the endpoint, so per-key spend attributes an embeddings
// call to the right key and the right surface.
func TestEmbedKeyedLedgerRowCarriesEndpointAndCaller(t *testing.T) {
	led := &keyedFakeLedger{}
	a := &embedFakeClient{fakeClient: &fakeClient{}, embedFn: func(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
		return &provider.EmbeddingResponse{
			Model: req.Model,
			Data:  []provider.Embedding{{Object: "embedding", Embedding: json.RawMessage(`[0.1]`), Index: 0}},
			Usage: provider.Usage{PromptTokens: 8, TotalTokens: 8},
		}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"a": a}, led, testConfig())
	req := embedReq()
	req.CallerID = "gk_testkey"
	_, err := eng.Embed(context.Background(), req)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(led.keyed) != 1 {
		t.Fatalf("keyed ledger rows = %d, want 1", len(led.keyed))
	}
	call := led.keyed[0]
	if call.keyID != "gk_testkey" {
		t.Errorf("key id = %q, want gk_testkey", call.keyID)
	}
	if call.call.usage.Endpoint != provider.EndpointEmbeddings {
		t.Errorf("ledger endpoint = %q, want embeddings", call.call.usage.Endpoint)
	}
	if call.call.usage.PromptTokens != 8 {
		t.Errorf("ledger prompt tokens = %d, want 8", call.call.usage.PromptTokens)
	}
	if led.count() != 0 {
		t.Errorf("unkeyed ledger rows = %d, want 0 (keyed path takes precedence)", led.count())
	}
}

// Local-estimate fallback: the upstream reports no usage, so the
// ledger row carries the local tiktoken estimate, labelled as an
// estimate — never presented as provider-reported.
func TestEmbedLocalEstimateFallback(t *testing.T) {
	led := &fakeLedger{}
	a := &embedFakeClient{fakeClient: &fakeClient{}, embedFn: func(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
		return &provider.EmbeddingResponse{
			Model: req.Model,
			Data:  []provider.Embedding{{Object: "embedding", Embedding: json.RawMessage(`[0.1]`), Index: 0}},
			Usage: provider.Usage{},
		}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"a": a}, led, testConfig())
	_, err := eng.Embed(context.Background(), embedReq())
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if led.count() != 1 {
		t.Fatalf("ledger rows = %d, want 1", led.count())
	}
	row := led.calls[0]
	if !row.usage.LocalEstimate {
		t.Errorf("ledger usage = %+v, want a labelled local estimate", row.usage)
	}
	if row.usage.PromptTokens <= 0 {
		t.Errorf("ledger prompt tokens = %d, want the estimate (>0)", row.usage.PromptTokens)
	}
	if row.usage.Endpoint != provider.EndpointEmbeddings {
		t.Errorf("ledger endpoint = %q, want embeddings", row.usage.Endpoint)
	}
}

// Rerank success: results pass through, the trace names the
// endpoint, and the ledger row carries the local estimate when
// the upstream reports no usage (the usual rerank case).
func TestRerankSuccessLedgerAndTrace(t *testing.T) {
	led := &fakeLedger{}
	a := &embedFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a}, led, testConfig())
	resp, err := eng.Rerank(context.Background(), rerankReq())
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].RelevanceScore != 0.9 {
		t.Fatalf("results = %+v", resp.Results)
	}
	if led.count() != 1 {
		t.Fatalf("ledger rows = %d, want 1", led.count())
	}
	row := led.calls[0]
	if row.usage.Endpoint != provider.EndpointRerank {
		t.Errorf("ledger endpoint = %q, want rerank", row.usage.Endpoint)
	}
	if !row.usage.LocalEstimate {
		t.Errorf("ledger usage = %+v, want a labelled local estimate (no upstream usage)", row.usage)
	}
	trace := eng.LastTrace()
	if trace == nil || trace.Endpoint != "rerank" {
		t.Errorf("trace endpoint = %v, want rerank", trace)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Preread != "exempt:rerank" {
		t.Errorf("trace attempts = %+v, want one exempt:rerank attempt", trace.Attempts)
	}
}
