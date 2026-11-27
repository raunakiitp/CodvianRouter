package webexec

import (
	"context"
	"errors"
	"fmt"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// This file adapts a web Executor to the provider.Client interface so
// a WebCookie catalog entry can execute through the SAME plumbing the
// key-based providers use (registry ClientFactory → engine). The
// error mapping is the behavior contract:
//
//   - stale/no-longer-valid session state → provider.UpstreamError
//     with StatusCode 401 (the router refuses to retry a dead
//     credential and surfaces 401 upstream semantics);
//   - provider quota 429 → provider.RateLimitError carrying the
//     parsed Retry-After (account fallback / pacing sees the real
//     upstream signal — "429 quota passthrough for account fallback").

// WebExecClient is a provider.Client backed by a web Executor.
type WebExecClient struct {
	ex      Executor
	surface WebSurface
}

// NewWebExecClient builds the adapter for a catalog ID. The executor
// (and therefore the stored session state) comes from the harness.
func NewWebExecClient(catalogID string, store *SessionStore, opts ...Option) (*WebExecClient, error) {
	surface, err := Surface(catalogID)
	if err != nil {
		return nil, err
	}
	ex, err := New(catalogID, store, opts...)
	if err != nil {
		return nil, err
	}
	return &WebExecClient{ex: ex, surface: surface}, nil
}

// mapError converts web-exec classification errors into the provider
// error taxonomy. Anything that is not stale/quota falls through as a
// plain wrapped error (the engine treats unknown errors as generic
// target failures).
func mapError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrStaleSession):
		return &provider.UpstreamError{StatusCode: 401, Body: err.Error()}
	case errors.Is(err, ErrQuotaExceeded):
		e := &provider.RateLimitError{Body: err.Error()}
		var werr *webExecError
		if errors.As(err, &werr) && werr.hasRetry {
			e.RetryAfter = werr.retryAfter
			e.RetryAfterPresent = true
		}
		return e
	default:
		return err
	}
}

// Chat implements provider.Client: one non-streaming chat completion
// over the provider's web surface.
func (c *WebExecClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	prompt := lastUserPrompt(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("webexec: chat request carries no user message to execute on the %s web surface", c.surface.CatalogID)
	}
	res, err := c.ex.Execute(ctx, ExecuteRequest{
		Prompt:        prompt,
		Model:         req.Model,
		TemporaryChat: true, // executed chat never persists history in the operator's account
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &provider.ChatResponse{
		Content: res.Content,
		Model:   res.Model,
	}, nil
}

// ChatStream implements provider.Client: the web surface answer
// arrives whole, so it is emitted as a single delta.
func (c *WebExecClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	resp, err := c.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Content != "" && onDelta != nil {
		if err := onDelta(resp.Content); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// ListModels implements provider.Client: the executor advertises the
// surface's recorded model IDs.
func (c *WebExecClient) ListModels(ctx context.Context) ([]string, error) {
	return append([]string(nil), c.surface.ModelIDs...), nil
}

// TestKey implements provider.Client: for a web surface there is no
// API key — the equivalent liveness check is whether a session state
// is stored and still loadable. A missing or stale state is the honest
// failure.
func (c *WebExecClient) TestKey(ctx context.Context) error {
	if !c.ex.HasSession() {
		return fmt.Errorf("webexec: %s has no stored session state (import/capture one first): %w", c.surface.CatalogID, ErrNoSession)
	}
	return nil
}

// lastUserPrompt pulls the most recent user message as the prompt to
// execute on the web surface (web UIs are single-prompt surfaces).
func lastUserPrompt(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}
