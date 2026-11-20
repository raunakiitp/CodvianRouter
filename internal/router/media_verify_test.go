// Media verify-first engine tests (M8.20): the exactly-once
// invariant on the provider-pinned chain, per-key failover inside
// one provider, the typed no-capable-provider error, and the
// verbatim status + body.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// mediaFakeClient is a fakeClient that also serves media
// passthrough, with a scripted function and a call counter. It
// embeds *fakeClient, so Chat/ChatStream/ListModels/TestKey and
// WireFormat are the fake's.
type mediaFakeClient struct {
	*fakeClient
	calls   int
	mediaFn func(ctx context.Context, req provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error)
}

func (c *mediaFakeClient) MediaPassthrough(ctx context.Context, req provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error) {
	c.mu.Lock()
	c.calls++
	fn := c.mediaFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &provider.MediaPassthroughResponse{Status: 200, Body: json.RawMessage(`{"ok":true}`)}, nil
}

func (c *mediaFakeClient) mediaCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func searchReq() provider.MediaPassthroughRequest {
	return provider.MediaPassthroughRequest{
		ProviderID:       "perplexity",
		Model:            "perplexity",
		Endpoint:         provider.EndpointSearch,
		Method:           "POST",
		Path:             "search",
		Body:             json.RawMessage(`{"query":"hello"}`),
		BookkeepingTexts: []string{"hello"},
	}
}

// Happy path: the upstream status + body come back verbatim, the
// trace names the endpoint, and the single attempt is exempt.
func TestMediaPassthroughSearchVerbatim(t *testing.T) {
	fc := &mediaFakeClient{fakeClient: &fakeClient{}, mediaFn: func(ctx context.Context, req provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error) {
		if req.Path != "search" {
			t.Errorf("upstream path = %q, want search", req.Path)
		}
		if req.ProviderID != "perplexity" {
			t.Errorf("provider = %q, want perplexity", req.ProviderID)
		}
		return &provider.MediaPassthroughResponse{Status: 200, Body: json.RawMessage(`{"id":"srch-1","results":[]}`)}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"perplexity": fc}, nil, testConfig())
	resp, err := eng.MediaPassthrough(context.Background(), searchReq())
	if err != nil {
		t.Fatalf("MediaPassthrough: %v", err)
	}
	if resp.Status != 200 || string(resp.Body) != `{"id":"srch-1","results":[]}` {
		t.Fatalf("resp = %d %s, want 200 {\"id\":\"srch-1\",\"results\":[]}", resp.Status, resp.Body)
	}
	if fc.mediaCalls() != 1 {
		t.Errorf("calls = %d, want 1", fc.mediaCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || trace.Endpoint != "search" {
		t.Fatalf("trace endpoint = %v, want search", trace)
	}
	if len(trace.Attempts) != 1 {
		t.Fatalf("trace attempts = %d, want 1", len(trace.Attempts))
	}
	if trace.Attempts[0].Preread != "exempt:search" {
		t.Errorf("preread = %q, want exempt:search", trace.Attempts[0].Preread)
	}
	if trace.Attempts[0].Provider != "perplexity" {
		t.Errorf("trace provider = %q, want perplexity", trace.Attempts[0].Provider)
	}
}

// Exactly once: the first key's failure ends the request — the
// second key of the SAME provider is never dialled, even though it
// would have succeeded. A video/music generation is a paid job:
// a replay would double-charge.
func TestMediaPassthroughExactlyOnceFirstFailureNeverCallsSecondKey(t *testing.T) {
	a := &mediaFakeClient{fakeClient: &fakeClient{}, mediaFn: func(ctx context.Context, req provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error) {
		return nil, &provider.UpstreamError{StatusCode: 429, Body: "rate limited"}
	}}
	b := &mediaFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{
		"runwayml":         a,
		"runwayml\x00key2": b,
	}, nil, testConfig())
	_, err := eng.MediaPassthrough(context.Background(), provider.MediaPassthroughRequest{
		ProviderID: "runwayml",
		Model:      "seedance2_5",
		Endpoint:   provider.EndpointVideo,
		Method:     "POST",
		Path:       "text_to_video",
		Body:       json.RawMessage(`{"model":"seedance2_5","promptText":"mist"}`),
	})
	if err == nil {
		t.Fatal("want the first key's error, got nil")
	}
	if a.mediaCalls() != 1 {
		t.Errorf("first key calls = %d, want 1", a.mediaCalls())
	}
	if b.mediaCalls() != 0 {
		t.Errorf("second key calls = %d, want 0 (exactly once — no replay)", b.mediaCalls())
	}
}

// Per-key failover before dialling still works: a first key
// SKIPPED before dialling (here: no media capability on that
// client) is selection, not a replay — the capable second key
// serves the request.
func TestMediaPassthroughSkippedKeyFallsThroughToCapableKey(t *testing.T) {
	plain := &fakeClient{} // no MediaPassthrough method
	capable := &mediaFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{
		"minimax":         plain,
		"minimax\x00key2": capable,
	}, nil, testConfig())
	resp, err := eng.MediaPassthrough(context.Background(), provider.MediaPassthroughRequest{
		ProviderID: "minimax",
		Model:      "music-3.0",
		Endpoint:   provider.EndpointMusic,
		Method:     "POST",
		Path:       "music_generation",
		Body:       json.RawMessage(`{"model":"music-3.0"}`),
	})
	if err != nil {
		t.Fatalf("MediaPassthrough: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	if capable.mediaCalls() != 1 {
		t.Errorf("capable key calls = %d, want 1", capable.mediaCalls())
	}
}

// No registered client for the pinned provider: the typed
// no-capable-provider error, not a 502-shaped plain error.
func TestMediaPassthroughNoProviderIsTyped(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"perplexity": &mediaFakeClient{fakeClient: &fakeClient{}}}, nil, testConfig())
	_, err := eng.MediaPassthrough(context.Background(), provider.MediaPassthroughRequest{
		ProviderID: "mistral",
		Model:      "mistral-ocr-latest",
		Endpoint:   provider.EndpointOCR,
		Method:     "POST",
		Path:       "ocr",
		Body:       json.RawMessage(`{"model":"mistral-ocr-latest"}`),
	})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	var ncp *NoCapableProviderError
	if !errors.As(err, &ncp) {
		t.Fatalf("error = %T (%v), want *NoCapableProviderError", err, err)
	}
	if ncp.Endpoint != provider.EndpointOCR {
		t.Errorf("endpoint = %q, want ocr", ncp.Endpoint)
	}
}

// A chain whose only client cannot serve media at all: skipped
// before dialling, then the same typed error.
func TestMediaPassthroughNoCapableClientIsTyped(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"perplexity": &fakeClient{}}, nil, testConfig())
	_, err := eng.MediaPassthrough(context.Background(), searchReq())
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	var ncp *NoCapableProviderError
	if !errors.As(err, &ncp) {
		t.Fatalf("error = %T (%v), want *NoCapableProviderError", err, err)
	}
}

// GET passthrough (video task retrieve): method + path reach the
// client unchanged, and a non-200 2xx status is preserved.
func TestMediaPassthroughGetPreservesStatus(t *testing.T) {
	fc := &mediaFakeClient{fakeClient: &fakeClient{}, mediaFn: func(ctx context.Context, req provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error) {
		if req.Method != "GET" {
			t.Errorf("method = %q, want GET", req.Method)
		}
		if req.Path != "tasks/task-1" {
			t.Errorf("path = %q, want tasks/task-1", req.Path)
		}
		return &provider.MediaPassthroughResponse{Status: 200, Body: json.RawMessage(`{"id":"task-1","status":"SUCCEEDED","output":["https://example.com/v.mp4"]}`)}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"runwayml": fc}, nil, testConfig())
	resp, err := eng.MediaPassthrough(context.Background(), provider.MediaPassthroughRequest{
		ProviderID: "runwayml",
		Model:      "runwayml",
		Endpoint:   provider.EndpointVideo,
		Method:     "GET",
		Path:       "tasks/task-1",
	})
	if err != nil {
		t.Fatalf("MediaPassthrough: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
}

// Validation happens before any chain work.
func TestMediaPassthroughValidation(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"perplexity": &mediaFakeClient{fakeClient: &fakeClient{}}}, nil, testConfig())
	if _, err := eng.MediaPassthrough(context.Background(), provider.MediaPassthroughRequest{}); err == nil {
		t.Errorf("want a validation error, got nil")
	}
}
