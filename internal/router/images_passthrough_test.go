// Images passthrough engine tests (M8.5): the exactly-once
// invariant, native-wire skipping, verbatim multipart
// forwarding, and validation. The upstream is always a fake
// client — no live provider is ever called.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// imageFakeClient is a fakeClient that also serves images, with
// a scripted function and call counters. It embeds *fakeClient,
// so Chat/ChatStream/ListModels/TestKey and WireFormat are the
// fake's; setting the fake's wireFormat to a native wire
// ("anthropic", "gemini", ...) makes the engine skip it at
// selection even though the method exists — exactly like the
// real native clients, which inherit PostImage from
// *openAIClient but speak no OpenAI wire.
type imageFakeClient struct {
	*fakeClient
	mu       sync.Mutex
	calls    int
	imageFn  func(ctx context.Context, req provider.ImageRequest) (*provider.ImageResponse, error)
	lastReq  provider.ImageRequest
	lastBody []byte
}

func (c *imageFakeClient) PostImage(ctx context.Context, req provider.ImageRequest) (*provider.ImageResponse, error) {
	c.mu.Lock()
	c.calls++
	c.lastReq = req
	c.lastBody = append([]byte(nil), req.Body...)
	fn := c.imageFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &provider.ImageResponse{
		StatusCode: 200,
		Body:       json.RawMessage(`{"created":1794000000,"data":[{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="}],"model":"mock-model","usage":{"input_tokens":10,"output_tokens":15,"total_tokens":25}}`),
		Model:      "mock-model",
		Usage:      provider.Usage{PromptTokens: 10, CompletionTokens: 15, TotalTokens: 25},
	}, nil
}

func (c *imageFakeClient) imageCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func genReq() provider.ImageRequest {
	return provider.ImageRequest{
		Operation:   provider.ImageGenerations,
		Model:       "mock-model",
		Body:        []byte(`{"model":"mock-model","prompt":"A red sailboat on calm water","size":"1024x1024"}`),
		ContentType: "application/json",
		PromptText:  "A red sailboat on calm water",
	}
}

func editReq(body []byte, contentType string) provider.ImageRequest {
	return provider.ImageRequest{
		Operation:   provider.ImageEdits,
		Model:       "mock-model",
		Body:        body,
		ContentType: contentType,
		PromptText:  "Add a lighthouse",
	}
}

// Exactly once: the first target's failure ends the request —
// the second target is never dialled, even though it would have
// succeeded. A 429 from the first target is still a failure of
// the one attempt, not a reason to replay.
func TestImagesExactlyOnceFirstFailureNeverCallsSecond(t *testing.T) {
	a := &imageFakeClient{fakeClient: &fakeClient{}, imageFn: func(ctx context.Context, req provider.ImageRequest) (*provider.ImageResponse, error) {
		return nil, &provider.UpstreamError{StatusCode: 429, Body: "rate limited"}
	}}
	b := &imageFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	_, err := eng.Images(context.Background(), genReq())
	if err == nil {
		t.Fatal("want the first target's error, got nil")
	}
	if a.imageCalls() != 1 {
		t.Errorf("first target calls = %d, want 1", a.imageCalls())
	}
	if b.imageCalls() != 0 {
		t.Errorf("second target calls = %d, want 0 (exactly once — no replay)", b.imageCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || trace.Endpoint != "images" {
		t.Fatalf("trace endpoint = %v, want images", trace)
	}
	if len(trace.Attempts) != 1 {
		t.Fatalf("trace attempts = %d, want 1", len(trace.Attempts))
	}
	if trace.Attempts[0].Outcome != "error" {
		t.Errorf("attempt outcome = %q, want error", trace.Attempts[0].Outcome)
	}
	if trace.Attempts[0].Preread != "exempt:images" {
		t.Errorf("preread = %q, want exempt:images", trace.Attempts[0].Preread)
	}
}

// Success returns the upstream body verbatim, byte for byte,
// with the parsed model and usage alongside.
func TestImagesGenerationsVerbatimSuccess(t *testing.T) {
	a := &imageFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, testConfig())
	resp, err := eng.Images(context.Background(), genReq())
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if resp == nil {
		t.Fatal("Images returned nil response")
	}
	wantBody := `{"created":1794000000,"data":[{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="}],"model":"mock-model","usage":{"input_tokens":10,"output_tokens":15,"total_tokens":25}}`
	if string(resp.Body) != wantBody {
		t.Errorf("response body = %s, want verbatim %s", resp.Body, wantBody)
	}
	if resp.Model != "mock-model" {
		t.Errorf("model = %q, want mock-model", resp.Model)
	}
	if resp.Usage.TotalTokens != 25 || resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 15 {
		t.Errorf("usage = %+v, want 10/15/25", resp.Usage)
	}
	if a.imageCalls() != 1 {
		t.Errorf("calls = %d, want 1", a.imageCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || trace.Endpoint != "images" || len(trace.Attempts) != 1 || trace.Attempts[0].Preread != "exempt:images" {
		t.Errorf("trace = %+v, want one exempt:images attempt", trace)
	}
}

// Edits: the multipart body and its Content-Type (boundary
// included) reach the upstream byte for byte — forwarded, not
// decoded and rebuilt.
func TestImagesEditsMultipartForwardedVerbatim(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("------MockBoundary\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"model\"\r\n\r\nmock-model\r\n")
	buf.WriteString("------MockBoundary\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"prompt\"\r\n\r\nAdd a lighthouse\r\n")
	buf.WriteString("------MockBoundary\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"image\"; filename=\"in.png\"\r\nContent-Type: image/png\r\n\r\n\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\r\n")
	buf.WriteString("------MockBoundary--\r\n")
	raw := buf.Bytes()
	a := &imageFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, testConfig())
	_, err := eng.Images(context.Background(), editReq(raw, "multipart/form-data; boundary=----MockBoundary"))
	if err != nil {
		t.Fatalf("Images edits: %v", err)
	}
	a.mu.Lock()
	gotBody, gotCT, gotOp := a.lastBody, a.lastReq.ContentType, a.lastReq.Operation
	a.mu.Unlock()
	if !bytes.Equal(gotBody, raw) {
		t.Errorf("upstream body differs from the client's raw multipart bytes (%d bytes vs %d)", len(gotBody), len(raw))
	}
	if gotCT != "multipart/form-data; boundary=----MockBoundary" {
		t.Errorf("upstream Content-Type = %q, want the client's multipart Content-Type verbatim", gotCT)
	}
	if gotOp != provider.ImageEdits {
		t.Errorf("operation = %q, want edits", gotOp)
	}
}

// Native wire: a client that implements PostImage but speaks
// Anthropic is skipped before dialling — never mistranslated —
// and the OpenAI-wire target behind it serves the request.
func TestImagesNativeWireSkippedOpenAIWireServes(t *testing.T) {
	native := &imageFakeClient{fakeClient: &fakeClient{wireFormat: "anthropic"}}
	openai := &imageFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"native": native, "openai": openai}, nil, testConfig())
	resp, err := eng.Images(context.Background(), genReq())
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if resp == nil || resp.Model != "mock-model" {
		t.Fatalf("response = %+v, want mock-model", resp)
	}
	if native.imageCalls() != 0 {
		t.Errorf("native-wire target calls = %d, want 0 (skipped, never dialled)", native.imageCalls())
	}
	if openai.imageCalls() != 1 {
		t.Errorf("openai-wire target calls = %d, want 1", openai.imageCalls())
	}
}

// All-native chain: no target can serve the endpoint, so the
// engine says so honestly instead of failing over onto a
// mistranslation.
func TestImagesAllNativeWireIsNoCapableProvider(t *testing.T) {
	native := &imageFakeClient{fakeClient: &fakeClient{wireFormat: "gemini"}}
	eng := NewEngine(map[string]provider.Client{"native": native}, nil, testConfig())
	_, err := eng.Images(context.Background(), genReq())
	if err == nil {
		t.Fatal("want NoCapableProviderError, got nil")
	}
	var ncp *NoCapableProviderError
	if !errors.As(err, &ncp) {
		t.Fatalf("error = %T %v, want *NoCapableProviderError", err, err)
	}
	if ncp.Endpoint != provider.EndpointImages {
		t.Errorf("endpoint = %q, want images", ncp.Endpoint)
	}
	if native.imageCalls() != 0 {
		t.Errorf("native-wire target calls = %d, want 0", native.imageCalls())
	}
	if !strings.Contains(err.Error(), "images") {
		t.Errorf("error %q does not name the endpoint", err.Error())
	}
}

// Validation: a bad request never reaches a provider — zero
// calls, whatever is wrong with it.
func TestImagesValidationNeverDials(t *testing.T) {
	a := &imageFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, testConfig())
	cases := map[string]provider.ImageRequest{
		"missing model":       {Operation: provider.ImageGenerations, Model: "", Body: []byte(`{"model":"","prompt":"x"}`), PromptText: "x"},
		"missing prompt":      {Operation: provider.ImageGenerations, Model: "m", Body: []byte(`{"model":"m","prompt":""}`), PromptText: ""},
		"streaming":           {Operation: provider.ImageGenerations, Model: "m", Body: []byte(`{"model":"m","prompt":"x","stream":true}`), PromptText: "x"},
		"bad json":            {Operation: provider.ImageGenerations, Model: "m", Body: []byte(`not json`), PromptText: "x"},
		"unknown operation":   {Operation: provider.ImageOperation("upscale"), Model: "m", Body: []byte(`{"model":"m","prompt":"x"}`), PromptText: "x"},
		"edits not multipart": {Operation: provider.ImageEdits, Model: "m", Body: []byte(`{"model":"m"}`), ContentType: "application/json", PromptText: "x"},
		"empty body":          {Operation: provider.ImageGenerations, Model: "m", Body: nil, PromptText: "x"},
	}
	for name, req := range cases {
		if _, err := eng.Images(context.Background(), req); err == nil {
			t.Errorf("%s: want a validation error, got nil", name)
		}
	}
	if a.imageCalls() != 0 {
		t.Errorf("provider calls = %d, want 0 (validation before dialling)", a.imageCalls())
	}
}
