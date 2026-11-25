// Embeddings + rerank client tests (CHECKLIST 171-172).
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// openAIEmbedClient returns the concrete client behind mustClient,
// so tests can call Embed / Rerank (the Client interface only
// exposes Chat/ChatStream/ListModels/TestKey).
func openAIEmbedClient(t *testing.T, srv *httptest.Server) *openAIClient {
	t.Helper()
	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	oc, ok := c.(*openAIClient)
	if !ok {
		t.Fatalf("NewClient returned %T, want *openAIClient", c)
	}
	return oc
}

func TestEmbedSuccessVectorsVerbatim(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// Float vectors with full precision, plus a second
		// embedding: the response must be returned verbatim,
		// never re-encoded or truncated.
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.123456789012345,-0.5,1e-06],"index":0},{"object":"embedding","embedding":[0.25,0.75],"index":1}],"model":"text-embedding-3-small","usage":{"prompt_tokens":8,"total_tokens":8}}`))
	}))
	defer srv.Close()

	resp, err := openAIEmbedClient(t, srv).Embed(context.Background(), EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: json.RawMessage(`["hello world","second"]`),
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/v1/embeddings" {
		t.Errorf("path = %q, want /v1/embeddings", gotPath)
	}
	// The request body carries the input array verbatim.
	var sent struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("sent body: %v", err)
	}
	if sent.Model != "text-embedding-3-small" || string(sent.Input) != `["hello world","second"]` {
		t.Errorf("sent = %+v", sent)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("data = %d embeddings, want 2", len(resp.Data))
	}
	// Verbatim: the exact float text survives, including 1e-06.
	if string(resp.Data[0].Embedding) != `[0.123456789012345,-0.5,1e-06]` {
		t.Errorf("embedding[0] = %s, want verbatim floats", resp.Data[0].Embedding)
	}
	if resp.Data[1].Index != 1 || string(resp.Data[1].Embedding) != `[0.25,0.75]` {
		t.Errorf("embedding[1] = %+v", resp.Data[1])
	}
	if resp.Usage.PromptTokens != 8 || resp.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Usage.Endpoint != EndpointEmbeddings {
		t.Errorf("usage endpoint = %q, want %q", resp.Usage.Endpoint, EndpointEmbeddings)
	}
	if resp.Model != "text-embedding-3-small" {
		t.Errorf("model = %q", resp.Model)
	}
}

func TestEmbedBase64StaysBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":"QUJDRA==","index":0}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`))
	}))
	defer srv.Close()

	resp, err := openAIEmbedClient(t, srv).Embed(context.Background(), EmbeddingRequest{
		Model: "m", Input: json.RawMessage(`"hi"`), EncodingFormat: "base64",
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if string(resp.Data[0].Embedding) != `"QUJDRA=="` {
		t.Errorf("base64 embedding = %s, want the base64 string untouched", resp.Data[0].Embedding)
	}
}

func TestEmbedTokenIDs(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"m","usage":{"prompt_tokens":3,"total_tokens":3}}`))
	}))
	defer srv.Close()

	_, err := openAIEmbedClient(t, srv).Embed(context.Background(), EmbeddingRequest{
		Model: "m", Input: json.RawMessage(`[101, 2054, 999]`),
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !strings.Contains(string(gotBody), `[101,2054,999]`) {
		t.Errorf("sent body = %s, want the token-id array", gotBody)
	}
}

func TestEmbedValidation(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()
	c := openAIEmbedClient(t, srv)

	for _, tc := range []struct {
		name string
		req  EmbeddingRequest
	}{
		{"empty model", EmbeddingRequest{Input: json.RawMessage(`"x"`)}},
		{"empty input", EmbeddingRequest{Model: "m"}},
		{"empty string", EmbeddingRequest{Model: "m", Input: json.RawMessage(`""`)}},
		{"empty array", EmbeddingRequest{Model: "m", Input: json.RawMessage(`[]`)}},
		{"bad json", EmbeddingRequest{Model: "m", Input: json.RawMessage(`{oops`)}},
		// Dimensions / encoding_format are NOT validated here
		// by design: an unknown value fails honestly at the
		// upstream, never on a guessed catalogue.
	} {
		if _, err := c.Embed(context.Background(), tc.req); err == nil {
			t.Errorf("%s: want an error", tc.name)
		}
	}
	if hits != 0 {
		t.Errorf("upstream hits = %d, want 0 (validation is local)", hits)
	}
}

func TestEmbed405IsRetryableButNotRetried(t *testing.T) {
	// 405/501 on embeddings/rerank means "this target does not
	// offer the endpoint": the wire taxonomy marks it retryable
	// (another target may offer it), but the client itself never
	// retries — the engine owns the exactly-once decision.
	if !wire.RetryableStatusForEndpoint(405, "embeddings") {
		t.Error("405 on embeddings should be retryable per the wire taxonomy")
	}
	if !wire.RetryableStatusForEndpoint(501, "rerank") {
		t.Error("501 on rerank should be retryable per the wire taxonomy")
	}
	if wire.RetryableStatusForEndpoint(405, "chat/completions") {
		t.Error("405 on chat/completions should stay non-retryable")
	}

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"error":{"message":"no such endpoint"}}`))
	}))
	defer srv.Close()

	_, err := openAIEmbedClient(t, srv).Embed(context.Background(), EmbeddingRequest{
		Model: "m", Input: json.RawMessage(`"x"`),
	})
	if err == nil {
		t.Fatal("want an error for a 405")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %T, want *UpstreamError", err)
	}
	if ue.StatusCode != 405 {
		t.Errorf("status = %d, want 405", ue.StatusCode)
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want exactly 1 (no internal retry)", hits)
	}
}

func TestEmbedMalformedResponse(t *testing.T) {
	for _, body := range []string{
		`{"object":"list","data":[],"model":"m"}`,
		`{"object":"list","model":"m"}`,
		`not json at all`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		_, err := openAIEmbedClient(t, srv).Embed(context.Background(), EmbeddingRequest{
			Model: "m", Input: json.RawMessage(`"x"`),
		})
		srv.Close()
		if err == nil {
			t.Errorf("body %q: want an error", body)
		}
	}
}

func TestRerankSuccess(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			t.Errorf("path = %q, want /v1/rerank", r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"rr-1","model":"rerank-english-v3.0","results":[{"index":1,"relevance_score":0.92,"document":{"text":"the second doc"}},{"index":0,"relevance_score":0.31}],"usage":{"prompt_tokens":40,"total_tokens":40}}`))
	}))
	defer srv.Close()

	topN := 2
	retDocs := true
	resp, err := openAIEmbedClient(t, srv).Rerank(context.Background(), RerankRequest{
		Model:           "rerank-english-v3.0",
		Query:           "what is the capital?",
		Documents:       []json.RawMessage{json.RawMessage(`"first doc"`), json.RawMessage(`{"text":"the second doc"}`)},
		TopN:            &topN,
		ReturnDocuments: &retDocs,
	})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	var sent struct {
		Model           string            `json:"model"`
		Query           string            `json:"query"`
		Documents       []json.RawMessage `json:"documents"`
		TopN            int               `json:"top_n"`
		ReturnDocuments bool              `json:"return_documents"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("sent body: %v", err)
	}
	if sent.TopN != 2 || !sent.ReturnDocuments || sent.Query != "what is the capital?" {
		t.Errorf("sent = %+v", sent)
	}
	// A document object passes through verbatim.
	if string(sent.Documents[1]) != `{"text":"the second doc"}` {
		t.Errorf("sent documents[1] = %s", sent.Documents[1])
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Results))
	}
	if resp.Results[0].Index != 1 || resp.Results[0].RelevanceScore != 0.92 {
		t.Errorf("results[0] = %+v", resp.Results[0])
	}
	if string(resp.Results[0].Document) != `{"text":"the second doc"}` {
		t.Errorf("results[0].document = %s, want the document echoed verbatim", resp.Results[0].Document)
	}
	// A result without return_documents has no document field.
	if resp.Results[1].Document != nil {
		t.Errorf("results[1].document = %s, want absent", resp.Results[1].Document)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 40 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Usage.Endpoint != EndpointRerank {
		t.Errorf("usage endpoint = %q, want %q", resp.Usage.Endpoint, EndpointRerank)
	}
}

func TestRerankNoUsage(t *testing.T) {
	// Cohere-style responses carry no token usage (only
	// billed_units in meta): the response honestly carries no
	// usage block, never an invented one.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"rr-2","model":"m","results":[{"index":0,"relevance_score":0.5}],"meta":{"billed_units":{"search_units":1}}}`))
	}))
	defer srv.Close()

	resp, err := openAIEmbedClient(t, srv).Rerank(context.Background(), RerankRequest{
		Model: "m", Query: "q", Documents: []json.RawMessage{json.RawMessage(`"d"`)},
	})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if resp.Usage != nil {
		t.Errorf("usage = %+v, want nil (upstream reported none)", resp.Usage)
	}
	if len(resp.Results) != 1 || resp.Results[0].RelevanceScore != 0.5 {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestRerankValidation(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()
	c := openAIEmbedClient(t, srv)

	for _, tc := range []struct {
		name string
		req  RerankRequest
	}{
		{"empty model", RerankRequest{Query: "q", Documents: []json.RawMessage{json.RawMessage(`"d"`)}}},
		{"empty query", RerankRequest{Model: "m", Documents: []json.RawMessage{json.RawMessage(`"d"`)}}},
		{"no documents", RerankRequest{Model: "m", Query: "q"}},
		// An empty-string document and top_n=0 are NOT
		// validated here by design: the upstream decides.
	} {
		if _, err := c.Rerank(context.Background(), tc.req); err == nil {
			t.Errorf("%s: want an error", tc.name)
		}
	}
	if hits != 0 {
		t.Errorf("upstream hits = %d, want 0 (validation is local)", hits)
	}
}
