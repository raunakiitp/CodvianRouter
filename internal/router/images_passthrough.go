// Images passthrough (M8.5).
//
// POST /v1/images/generations and POST /v1/images/edits are
// routed through the SAME engine as POST /v1/chat/completions —
// chain resolution, budgets, guardrails, ledger, route trace —
// but each is replay-exempt (router.ReplayExemption, preread.go):
// exactly ONE upstream attempt, never replayed onto another
// target. An images call is single-shot generation work paid
// for per image; replaying it onto another target after a
// failure would double-apply upstream work (and double-bill),
// so the first target that is actually called owns the outcome,
// success or failure. What does NOT count as an attempt: a
// target skipped before dialling (zero quota, breaker open,
// cooling, limiter shed, or no images capability on that
// client) — no upstream call was made, so moving to the next
// capable target is selection, not a replay. A shed target is
// skipped the same way the chat chain skips one.
//
// OpenAI-compatible wire ONLY: the routed provider must speak
// the OpenAI wire. A native-wire provider (Anthropic, Gemini,
// Cohere, Bedrock) is skipped at selection — never dialled —
// and when no target in the chain can serve the endpoint the
// engine answers with NoCapableProviderError (an honest 400
// naming the limitation, never a mistranslation of the request
// onto a native image surface that does not exist in this
// slice). The provider clients inherit PostImage from
// *openAIClient, so the interface check alone would select
// them — the wire check is what keeps them out, exactly as on
// embeddings/rerank.
//
// The engine never sees the image bytes: the handler builds a
// synthetic provider.ChatRequest carrying the prompt text (for
// the shared guardrail / budget / ledger machinery) and hands
// the real request to Engine.Images. The synthetic request is
// never sent upstream. The request body travels verbatim
// (provider.ImageRequest.Body) and the response body is
// returned verbatim (provider.ImageResponse.Body); only the
// model and usage block are parsed. An edits body is raw
// multipart/form-data and is forwarded, not decoded-and-rebuilt.
//
// Deliberately NOT cached, coalesced or stale-served: an image
// generation is a stateful single-shot call — serving a cached
// image would hand out another request's image and hide the
// upstream call the ledger must record.
//
// Endpoint names verified against the OpenAI Images API
// (2026-11-19): generations and edits are the only two live
// endpoints. Variations is retired; /v1/images/upscale does not
// exist in the OpenAI API and is not built here.
//
// What this slice does NOT do (honest gaps, see the M8.5
// report): streaming generations (stream:true is refused 400
// by the server before the engine is called); per-image cost —
// the ledger records whatever usage the upstream reported and
// cost stays $0/unknown unless the price book carries the
// image model (no price is ever invented); the local token
// estimate covers the prompt text only, never the image bytes.
package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// supportsImages reports whether client can serve an images
// call in this slice: it implements provider.ImagePoster AND
// speaks the OpenAI wire. Native-wire clients inherit the
// PostImage method by embedding *openAIClient, so the interface
// check alone would select them — and the call would fail on
// the native base's 404. The wire check is what keeps them out
// of the selection: only an OpenAI-compatible surface offers
// /v1/images/*. A client that does not expose its wire format
// is treated as OpenAI wire (the Client contract is
// OpenAI-compatible by definition); a future native transport
// (Gemini generateImages, Bedrock image models) changes this
// check, not the call sites.
func supportsImages(client provider.Client) bool {
	if _, ok := client.(provider.ImagePoster); !ok {
		return false
	}
	if wf, ok := client.(wireFormatClient); ok {
		format := wf.WireFormat()
		return format == "" || format == provider.WireFormatOpenAI
	}
	return true
}

// callImage makes the single upstream images call for one
// attempt, mirroring callEmbed: adaptive-limiter admission,
// panic isolation (a panicking client fails this attempt, never
// the gateway), and a synthetic ChatResponse carrying the
// reported usage so noteSuccess can record it. The response
// itself is returned separately — the caller needs the image
// payload verbatim, not the synthetic chat shape.
func (e *Engine) callImage(ctx context.Context, att Attempt, req provider.ImageRequest) (img *provider.ImageResponse, res attemptResult) {
	client, ok := e.clientForAttempt(att)
	if !ok {
		return nil, attemptResult{att: att, err: fmt.Errorf("router: unknown provider %q", att.ProviderID)}
	}
	poster, ok := client.(provider.ImagePoster)
	if !ok {
		return nil, attemptResult{att: att, err: fmt.Errorf("router: provider %q does not support images", att.ProviderID)}
	}
	if !e.limiter.TryAcquire(att.ProviderID) {
		e.noteConcurrencyRejected(att.ProviderID)
		return nil, attemptResult{att: att, err: adaptive.ErrLimited}
	}
	defer e.limiter.Release(att.ProviderID)
	start := time.Now()
	defer func() {
		if v := recover(); v != nil {
			e.notePanic("provider_images")
			img = nil
			res = attemptResult{att: att, err: NewPanicError("provider_images", v), latencyMs: time.Since(start).Milliseconds(), called: true}
		}
	}()
	resp, err := poster.PostImage(ctx, reqWithModelImage(req, att))
	if err != nil {
		// Rate-limit learning (M5.13, the callChat precedent):
		// a 429's parsed headers pace the limiter and teach
		// the measured quota fraction (the error already
		// carries the signals — see learnRateLimitError).
		e.learnRateLimitError(att, err)
		return nil, attemptResult{att: att, err: err, latencyMs: time.Since(start).Milliseconds(), called: true}
	}
	if resp == nil {
		return nil, attemptResult{att: att, err: errors.New("router: images provider returned no response"), latencyMs: time.Since(start).Milliseconds(), called: true}
	}
	usage := resp.Usage
	usage.Endpoint = provider.EndpointImages
	// Rate-limit learning (M5.13, the callChat precedent):
	// the response's parsed headers pace the adaptive limiter
	// and record the measured remaining fraction as the
	// engine's quota hint. The synthetic ChatResponse carries
	// the signal so noteSuccess's ledger path sees the same
	// shape chat attempts carry.
	e.learnRateLimit(att, resp.RateLimit)
	return resp, attemptResult{
		att:       att,
		resp:      &provider.ChatResponse{Usage: usage, Model: resp.Model, RateLimit: resp.RateLimit},
		latencyMs: time.Since(start).Milliseconds(),
		called:    true,
	}
}

// reqWithModelImage returns req addressed to att's model (the
// chain resolves the model per provider; the request itself
// names the model the caller asked for). The body is untouched:
// retargeting the model never rewrites the payload.
func reqWithModelImage(req provider.ImageRequest, att Attempt) provider.ImageRequest {
	req.Model = att.Model
	return req
}

// Images routes one images request: exactly one upstream
// attempt (see the package doc). The chain is resolved for
// req.Model exactly like Chat resolves its chain. The prompt
// text the server extracted rides the bookkeeping request so
// the shared machinery (reconcileUsage, budgetGate,
// recordAttempt) works unchanged; the image bytes themselves
// are never counted — the local estimate covers the prompt
// text only, and the ledger records whatever usage the
// upstream reported (or unknown when it reported none).
func (e *Engine) Images(ctx context.Context, req provider.ImageRequest) (*provider.ImageResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	bookReq := bookkeepingRequest(req.Model, []string{req.PromptText}, provider.EndpointImages, req.CallerID, req.BudgetUSD, req.BudgetStrict)
	chain := e.chainFor(req.Model)
	var img *provider.ImageResponse
	_, _, err := e.singleAttempt(ctx, chain, provider.EndpointImages, bookReq, supportsImages,
		func(ctx context.Context, att Attempt) attemptResult {
			var res attemptResult
			img, res = e.callImage(ctx, att, req)
			return res
		})
	if err != nil {
		return nil, err
	}
	return img, nil
}
