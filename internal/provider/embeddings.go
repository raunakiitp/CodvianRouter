// Embeddings + rerank passthrough (CHECKLIST 171-172).
//
// The gateway routes POST /v1/embeddings and POST /v1/rerank through
// the same engine as chat completions — chain resolution, budgets,
// guardrails, ledger — but the requests are replay-exempt
// (router.ReplayExemption): exactly one attempt each, never replayed
// onto another target. Embeddings are single-shot vector work and a
// rerank is a stateful scoring of the caller's documents; a replay
// would double-apply upstream work, so the engine picks one capable
// target and that target's answer — success or failure — is final.
//
// Shapes are OpenAI-compatible where OpenAI defines the shape
// (/v1/embeddings) and Cohere/LiteLLM-compatible where it does not
// (OpenAI has no /v1/rerank; LiteLLM and Portkey route rerank with
// the Cohere shape: model, query, documents, top_n,
// return_documents, results with index + relevance_score).
//
// The vectors themselves pass through verbatim (json.RawMessage):
// the gateway never parses, truncates or re-encodes an embedding,
// so a float array stays a float array and a base64 string stays a
// base64 string. Only the usage block is parsed, for the ledger.
package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
)

// Embedder is optionally implemented by clients that can serve
// POST /v1/embeddings. The OpenAI-wire client implements it; the
// native-wire clients (Anthropic, Gemini, Cohere, Bedrock) embed
// *openAIClient and inherit the method, but the engine never calls
// it on them: selection requires an OpenAI wire format (see
// router.supportsEmbeddings), because only an OpenAI-compatible
// surface offers /v1/embeddings. A future slice may add native
// transports (Cohere /v1/embed, Gemini embedContent) — until then
// a native-wire provider is skipped at selection, never dialled
// for an endpoint it does not offer.
type Embedder interface {
	Embed(ctx context.Context, req EmbeddingRequest) (*EmbeddingResponse, error)
}

// Reranker is optionally implemented by clients that can serve
// POST /v1/rerank. Same selection rule as Embedder: OpenAI wire
// only, in this slice.
type Reranker interface {
	Rerank(ctx context.Context, req RerankRequest) (*RerankResponse, error)
}

// EmbeddingRequest is one POST /v1/embeddings request, in the
// OpenAI shape: input is a string, an array of strings, an array of
// token IDs, or an array of token-ID arrays — kept as RawMessage
// and forwarded verbatim, so the gateway never rewrites the
// caller's input. Dimensions and EncodingFormat are the OpenAI
// options ("float" or "base64"); User is forwarded when set.
// CallerID / BudgetUSD / BudgetStrict are router-internal
// (json:"-"): the caller identity from the gateway key, and the
// per-request budget from X-Codvian-Budget — never serialised, so
// they can never leak into a trace, the ledger, or an upstream
// request body.
type EmbeddingRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
	User           string          `json:"user,omitempty"`

	CallerID     string   `json:"-"`
	BudgetUSD    *float64 `json:"-"`
	BudgetStrict bool     `json:"-"`
}

// Validate reports whether req is a well-formed embeddings request:
// a model and a present, non-null, non-empty input. It does not
// validate dimensions or encoding_format against any provider's
// catalogue — an unknown value fails honestly at the upstream,
// never here on a guessed catalogue.
func (r EmbeddingRequest) Validate() error {
	if r.Model == "" {
		return fmt.Errorf("provider: embeddings request model is required")
	}
	if len(r.Input) == 0 || string(r.Input) == "null" {
		return fmt.Errorf("provider: embeddings request input is required")
	}
	var v any
	if err := json.Unmarshal(r.Input, &v); err != nil {
		return fmt.Errorf("provider: embeddings request input is not valid JSON: %w", err)
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return fmt.Errorf("provider: embeddings request input must not be empty")
		}
	case map[string]any:
		if len(t) == 0 {
			return fmt.Errorf("provider: embeddings request input must not be empty")
		}
	case []any:
		if len(t) == 0 {
			return fmt.Errorf("provider: embeddings request input must not be empty")
		}
	default:
		return fmt.Errorf("provider: embeddings request input must be a string, object, or an array")
	}
	return nil
}

// TextInputs decodes the string forms of the input for the
// guardrail scan and the local token estimate: a single string,
// an array of strings, or text fields from multimodal maps.
// Token-ID inputs (an array of ints, or an array of int arrays)
// carry no text, so they return nil — there is nothing to scan,
// and the token estimate counts the IDs directly instead (see EstimateTokens).
func (r EmbeddingRequest) TextInputs() []string {
	var v any
	if err := json.Unmarshal(r.Input, &v); err != nil {
		return nil
	}
	switch t := v.(type) {
	case string:
		return []string{t}
	case map[string]any:
		for _, k := range []string{"text", "prompt", "content", "query"} {
			if s, ok := t[k].(string); ok && s != "" {
				return []string{s}
			}
		}
		return nil
	case []any:
		out := []string{}
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			} else if m, ok := item.(map[string]any); ok {
				for _, k := range []string{"text", "prompt", "content", "query"} {
					if s, ok := m[k].(string); ok && s != "" {
						out = append(out, s)
						break
					}
				}
			}
		}
		if len(out) > 0 {
			return out
		}
		return nil
	}
	return nil
}

// TokenIDCount counts the token IDs in a token-ID input (an array
// of ints, or an array of int arrays): the exact prompt-token count
// for the budget estimate, since the caller already tokenised the
// input. 0 for string inputs (counted by the tokenizer instead)
// and for anything unparseable.
func (r EmbeddingRequest) TokenIDCount() int {
	var v any
	if err := json.Unmarshal(r.Input, &v); err != nil {
		return 0
	}
	arr, ok := v.([]any)
	if !ok {
		return 0
	}
	n := 0
	for _, item := range arr {
		switch t := item.(type) {
		case float64:
			n++
		case []any:
			n += len(t)
		default:
			return 0
		}
	}
	return n
}

// Embedding is one vector in an embeddings response, verbatim:
// Embedding is a JSON array of floats or a base64 string, exactly
// as the upstream returned it.
type Embedding struct {
	Object    string          `json:"object"`
	Embedding json.RawMessage `json:"embedding"`
	Index     int             `json:"index"`
}

// EmbeddingResponse is one POST /v1/embeddings response, in the
// OpenAI shape. Usage is the provider-reported prompt/total
// (completion is 0 by nature — embeddings produce no completion
// tokens); a provider that reports no usage leaves Usage zero,
// and the engine falls back to a labelled local estimate exactly
// like a chat attempt with no reported usage. RateLimit carries
// the upstream rate-limit headers parsed from this response
// (M5.13, the ChatResponse precedent) — nil when the upstream
// sent no rate-limit headers. The router learns from it (pacing
// + quota hint); it is never serialized to callers.
type EmbeddingResponse struct {
	Object string      `json:"object"`
	Data   []Embedding `json:"data"`
	Model  string      `json:"model"`
	Usage  Usage       `json:"usage"`

	RateLimit *adaptive.RateLimitSignals `json:"-"`
}

// RerankRequest is one POST /v1/rerank request, in the
// Cohere/LiteLLM shape: the query plus the documents to score.
// Documents are strings or objects (e.g. {"text": ...}) — kept as
// RawMessage and forwarded verbatim, so the gateway never rewrites
// the caller's documents. TopN caps the returned results;
// ReturnDocuments asks the upstream to echo each scored document
// back in its result. CallerID / BudgetUSD / BudgetStrict are
// router-internal, as on EmbeddingRequest.
type RerankRequest struct {
	Model           string            `json:"model"`
	Query           string            `json:"query"`
	Documents       []json.RawMessage `json:"documents"`
	TopN            *int              `json:"top_n,omitempty"`
	ReturnDocuments *bool             `json:"return_documents,omitempty"`

	CallerID     string   `json:"-"`
	BudgetUSD    *float64 `json:"-"`
	BudgetStrict bool     `json:"-"`
}

// Validate reports whether req is a well-formed rerank request: a
// model, a query, and at least one document.
func (r RerankRequest) Validate() error {
	if r.Model == "" {
		return fmt.Errorf("provider: rerank request model is required")
	}
	if r.Query == "" {
		return fmt.Errorf("provider: rerank request query is required")
	}
	if len(r.Documents) == 0 {
		return fmt.Errorf("provider: rerank request documents must not be empty")
	}
	return nil
}

// TextDocuments decodes the string documents for the guardrail
// scan and the local token estimate. Object documents are skipped
// for the scan (their text lives under provider-specific keys the
// gateway does not parse) — the query is always scanned, and an
// object document's text is never invented from a guessed key.
func (r RerankRequest) TextDocuments() []string {
	out := []string{}
	for _, d := range r.Documents {
		var s string
		if err := json.Unmarshal(d, &s); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// RerankResult is one scored document in a rerank response.
// Document is the upstream's echo of the scored document, verbatim
// when the request asked for it (ReturnDocuments) — absent
// otherwise, never reconstructed.
type RerankResult struct {
	Index          int             `json:"index"`
	RelevanceScore float64         `json:"relevance_score"`
	Document       json.RawMessage `json:"document,omitempty"`
}

// RerankResponse is one POST /v1/rerank response, in the
// Cohere/LiteLLM shape. Usage is present only when the upstream
// reported one (most rerank providers do not — Cohere reports
// billed_units in meta instead, which is not a token usage block
// and is not converted into one); a nil Usage means unknown, never
// zero usage. RateLimit carries the upstream rate-limit headers
// parsed from this response (M5.13, the ChatResponse precedent)
// — nil when the upstream sent no rate-limit headers. The router
// learns from it (pacing + quota hint); it is never serialized
// to callers.
type RerankResponse struct {
	ID      string         `json:"id,omitempty"`
	Model   string         `json:"model"`
	Results []RerankResult `json:"results"`
	Usage   *Usage         `json:"usage,omitempty"`

	RateLimit *adaptive.RateLimitSignals `json:"-"`
}
