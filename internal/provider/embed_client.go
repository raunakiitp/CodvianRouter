// OpenAI-wire embeddings + rerank client (CHECKLIST 171-172).
//
// POST {base}/v1/embeddings and POST {base}/v1/rerank on the same
// OpenAI-compatible client that serves chat completions: same auth
// headers (catalog-resolved), same base-URL joining, same error
// mapping — only the endpoint name in the taxonomy differs, so a
// 405/501 is "target lacks the endpoint" here instead of a client
// error (see wire.RetryableStatusForEndpoint).
//
// These methods live on *openAIClient, so the native-wire clients
// (Anthropic, Gemini, Cohere, Bedrock) inherit them by embedding —
// but the engine never calls them there: selection requires an
// OpenAI wire format, because only an OpenAI-compatible surface
// offers these endpoints. Inheriting the method is harmless: an
// accidental call would POST to the native base and fail honestly
// on the upstream 404, exactly once.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
)

// embeddingsURL is POST {base}/v1/embeddings (with the /v1 rule
// from joinAPIPath).
func (c *openAIClient) embeddingsURL() string {
	return joinAPIPath(c.baseURL, "embeddings")
}

// rerankURL is POST {base}/v1/rerank (with the /v1 rule from
// joinAPIPath). OpenAI itself has no /v1/rerank; OpenAI-compatible
// providers that offer reranking (Cohere's compatibility surface,
// Voyage, Jina, mixedbread, LiteLLM-fronted providers) serve it
// here, in the Cohere shape.
func (c *openAIClient) rerankURL() string {
	return joinAPIPath(c.baseURL, "rerank")
}

// embeddingWireRequest is the wire shape sent upstream for one
// embeddings call: the client's fields verbatim (input is already
// RawMessage, so no re-encoding can mangle it).
type embeddingWireRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
	User           string          `json:"user,omitempty"`
}

// embeddingWireResponse is the decode shape for an embeddings
// response: the data rows verbatim plus the usage block. The
// embedding vectors are RawMessage (see Embedding): the gateway
// never parses them.
type embeddingWireResponse struct {
	Object string      `json:"object"`
	Data   []Embedding `json:"data"`
	Model  string      `json:"model"`
	Usage  Usage       `json:"usage"`
}

// Embed performs one non-streaming embeddings call. There is no
// streaming form: embeddings are a single response, and a failed
// call is the engine's exactly-once attempt — this method never
// retries internally.
func (c *openAIClient) Embed(ctx context.Context, req EmbeddingRequest) (*EmbeddingResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(embeddingWireRequest{
		Model:          req.Model,
		Input:          req.Input,
		Dimensions:     req.Dimensions,
		EncodingFormat: req.EncodingFormat,
		User:           req.User,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding embeddings request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.embeddingsURL(), payload)
	if err != nil {
		return nil, err
	}
	body, header, err := c.doForEndpoint(httpReq, "embeddings")
	if err != nil {
		return nil, err
	}
	var decoded embeddingWireResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed embeddings response: " + err.Error()}
	}
	if len(decoded.Data) == 0 {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "upstream returned no embeddings"}
	}
	usage := decoded.Usage
	usage.Endpoint = EndpointEmbeddings
	// Rate-limit headers (M5.13, the chat precedent): the
	// response headers pace the adaptive limiter and teach the
	// measured quota fraction — an upstream that sends no
	// headers teaches nothing (the signal is empty and the
	// router's learn call is a no-op).
	signals := adaptive.ParseRateLimitHeaders(header, time.Now())
	return &EmbeddingResponse{
		Object: decoded.Object,
		Data:   decoded.Data,
		Model:  decoded.Model,
		Usage:  usage,

		RateLimit: &signals,
	}, nil
}

// rerankWireRequest is the wire shape sent upstream for one rerank
// call: the client's fields verbatim (documents are already
// RawMessage).
type rerankWireRequest struct {
	Model           string            `json:"model"`
	Query           string            `json:"query"`
	Documents       []json.RawMessage `json:"documents"`
	TopN            *int              `json:"top_n,omitempty"`
	ReturnDocuments *bool             `json:"return_documents,omitempty"`
}

// rerankWireResponse is the decode shape for a rerank response:
// results verbatim, plus the usage block when the upstream sent
// one (most rerank providers do not — Cohere reports billed_units
// in meta instead, which is not a token usage block and is not
// converted into one). An absent usage stays nil: unknown, never
// zero usage.
type rerankWireResponse struct {
	ID      string         `json:"id,omitempty"`
	Model   string         `json:"model"`
	Results []RerankResult `json:"results"`
	Usage   *Usage         `json:"usage,omitempty"`
}

// Rerank performs one rerank call. There is no streaming form, and
// like Embed it never retries internally: the engine's single
// attempt is the only call.
func (c *openAIClient) Rerank(ctx context.Context, req RerankRequest) (*RerankResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(rerankWireRequest{
		Model:           req.Model,
		Query:           req.Query,
		Documents:       req.Documents,
		TopN:            req.TopN,
		ReturnDocuments: req.ReturnDocuments,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding rerank request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.rerankURL(), payload)
	if err != nil {
		return nil, err
	}
	body, header, err := c.doForEndpoint(httpReq, "rerank")
	if err != nil {
		return nil, err
	}
	var decoded rerankWireResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed rerank response: " + err.Error()}
	}
	if decoded.Results == nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "upstream returned no rerank results"}
	}
	if decoded.Usage != nil {
		u := *decoded.Usage
		u.Endpoint = EndpointRerank
		decoded.Usage = &u
	}
	// Rate-limit headers (M5.13, the chat precedent) — see Embed.
	signals := adaptive.ParseRateLimitHeaders(header, time.Now())
	return &RerankResponse{
		ID:      decoded.ID,
		Model:   decoded.Model,
		Results: decoded.Results,
		Usage:   decoded.Usage,

		RateLimit: &signals,
	}, nil
}
