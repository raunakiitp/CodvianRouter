// POST /v1/images/generations + POST /v1/images/edits server
// tests (M8.5): validation, verbatim passthrough, the typed
// no-capable-provider 400, the verbatim upstream-error status,
// the 503 when the engine has no images capability, and the
// apitest fixture contracts.
//
// NOTE: the routes are not registered in server.go yet —
// registration is the coordinator's wiring step (see
// internal/server/images.go header). These tests therefore call
// the handlers directly (same package), and exercise the real
// auth gate through requireAuth around the handler.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/plugins"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// imageFakeEngine is a fakeEngine that also serves images, with
// call counters and scripted results. It records the exact
// request the handler handed to the engine, so the tests can
// pin that generations JSON and edits multipart bodies travel
// verbatim.
type imageFakeEngine struct {
	*fakeEngine
	imageCalls int32
	imageResp  *provider.ImageResponse
	imageErr   error
	lastReq    provider.ImageRequest
	trace      *routetrace.RouteTrace
}

func (e *imageFakeEngine) Images(ctx context.Context, req provider.ImageRequest) (*provider.ImageResponse, error) {
	atomic.AddInt32(&e.imageCalls, 1)
	e.lastReq = req
	if e.imageErr != nil {
		return nil, e.imageErr
	}
	if e.imageResp != nil {
		return e.imageResp, nil
	}
	return &provider.ImageResponse{
		StatusCode: 200,
		Body:       json.RawMessage(`{"created":1794000000,"data":[{"b64_json":"AAAABBBB"}],"model":"mock-model","usage":{"input_tokens":10,"output_tokens":15,"total_tokens":25}}`),
		Model:      "mock-model",
		Usage:      provider.Usage{PromptTokens: 10, CompletionTokens: 15, TotalTokens: 25},
	}, nil
}

func (e *imageFakeEngine) LastTrace() *routetrace.RouteTrace { return e.trace }

func doImageReq(t *testing.T, srv *Server, handler http.HandlerFunc, method, path string, body []byte, contentType, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestImagesGenerationsValidation(t *testing.T) {
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	for _, body := range []string{
		`{"prompt":"x"}`,
		`{"model":"m"}`,
		`{"model":"m","prompt":""}`,
		`{"model":"m","prompt":"x","stream":true}`,
		`not json`,
		``,
	} {
		rec := doImageReq(t, srv, srv.handleImagesGenerations, http.MethodPost, "/v1/images/generations", []byte(body), "application/json", "test-key-123")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
	if eng.imageCalls != 0 {
		t.Errorf("engine calls = %d, want 0 (validation before dialling)", eng.imageCalls)
	}
}

func TestImagesEditsValidation(t *testing.T) {
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	// Not multipart at all.
	rec := doImageReq(t, srv, srv.handleImagesEdits, http.MethodPost, "/v1/images/edits", []byte(`{"model":"m"}`), "application/json", "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("json edits body: status = %d, want 400", rec.Code)
	}
	// Multipart without a model field.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "Add a lighthouse")
	_ = mw.Close()
	rec = doImageReq(t, srv, srv.handleImagesEdits, http.MethodPost, "/v1/images/edits", buf.Bytes(), mw.FormDataContentType(), "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("multipart without model: status = %d, want 400", rec.Code)
	}
	if eng.imageCalls != 0 {
		t.Errorf("engine calls = %d, want 0", eng.imageCalls)
	}
}

func TestImagesMethodNotAllowed(t *testing.T) {
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	rec := doImageReq(t, srv, srv.handleImagesGenerations, http.MethodGet, "/v1/images/generations", nil, "", "test-key-123")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// Auth: the real requireAuth gate around the handler refuses a
// missing or wrong gateway key before the engine is ever called,
// and admits the test key.
func TestImagesAuthRequired(t *testing.T) {
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	gated := srv.requireAuth(srv.handleImagesGenerations)
	body := []byte(`{"model":"mock-model","prompt":"A red sailboat"}`)
	for _, key := range []string{"", "wrong-key"} {
		rec := doImageReq(t, srv, gated, http.MethodPost, "/v1/images/generations", body, "application/json", key)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", key, rec.Code)
		}
	}
	if eng.imageCalls != 0 {
		t.Errorf("engine calls = %d, want 0 (auth first)", eng.imageCalls)
	}
	rec := doImageReq(t, srv, gated, http.MethodPost, "/v1/images/generations", body, "application/json", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Errorf("test key: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if eng.imageCalls != 1 {
		t.Errorf("engine calls = %d, want 1", eng.imageCalls)
	}
}

// A server whose engine has no images capability answers 503,
// never a faked image.
func TestImagesEngineWithoutCapabilityIs503(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doImageReq(t, srv, srv.handleImagesGenerations, http.MethodPost, "/v1/images/generations", []byte(`{"model":"m","prompt":"x"}`), "application/json", "test-key-123")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// No capable provider (every target native-wire): an honest 400
// naming the endpoint, never a 502.
func TestImagesNoCapableProviderIs400(t *testing.T) {
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}, imageErr: &router.NoCapableProviderError{Endpoint: provider.EndpointImages, Model: "mock-model"}}
	srv := newTestServer(t, eng)
	rec := doImageReq(t, srv, srv.handleImagesGenerations, http.MethodPost, "/v1/images/generations", []byte(`{"model":"mock-model","prompt":"x"}`), "application/json", "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "images") {
		t.Errorf("body = %q, want it to name the images endpoint", rec.Body.String())
	}
}

// Generations success: the engine's body is returned verbatim,
// the surface is named, and the engine saw the exact JSON the
// client sent.
func TestImagesGenerationsVerbatim(t *testing.T) {
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	body := `{"model":"mock-model","prompt":"A red sailboat","size":"1024x1024"}`
	rec := doImageReq(t, srv, srv.handleImagesGenerations, http.MethodPost, "/v1/images/generations", []byte(body), "application/json", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Endpoint"); got != "images" {
		t.Errorf("X-Codvian-Endpoint = %q, want images", got)
	}
	if !strings.Contains(rec.Body.String(), `"b64_json":"AAAABBBB"`) {
		t.Errorf("body = %s, want the engine's b64_json verbatim", rec.Body.String())
	}
	if string(eng.lastReq.Body) != body {
		t.Errorf("engine body = %s, want verbatim %s", eng.lastReq.Body, body)
	}
	if eng.lastReq.Operation != provider.ImageGenerations || eng.lastReq.Model != "mock-model" || eng.lastReq.PromptText != "A red sailboat" {
		t.Errorf("engine request = %+v, want generations/mock-model with the prompt", eng.lastReq)
	}
}

// Edits success: the raw multipart bytes reach the engine byte
// for byte, boundary and binary part included — never decoded
// and rebuilt.
func TestImagesEditsMultipartVerbatim(t *testing.T) {
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "mock-model")
	_ = mw.WriteField("prompt", "Add a lighthouse")
	part, err := mw.CreateFormFile("image", "in.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	// Fake PNG bytes (header + a few payload bytes, not a real image).
	_, _ = part.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x01, 0x02, 0x03})
	_ = mw.Close()
	raw := buf.Bytes()
	rec := doImageReq(t, srv, srv.handleImagesEdits, http.MethodPost, "/v1/images/edits", raw, mw.FormDataContentType(), "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(eng.lastReq.Body, raw) {
		t.Errorf("engine body differs from the client's raw multipart bytes (%d vs %d bytes)", len(eng.lastReq.Body), len(raw))
	}
	if eng.lastReq.ContentType != mw.FormDataContentType() {
		t.Errorf("engine Content-Type = %q, want %q", eng.lastReq.ContentType, mw.FormDataContentType())
	}
	if eng.lastReq.Operation != provider.ImageEdits || eng.lastReq.Model != "mock-model" || eng.lastReq.PromptText != "Add a lighthouse" {
		t.Errorf("engine request = %+v, want edits/mock-model with the prompt", eng.lastReq)
	}
}

// Upstream errors surface with the upstream's own status and
// error body, verbatim — not a gateway-invented 502.
func TestImagesUpstreamErrorVerbatim(t *testing.T) {
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}, imageErr: &provider.UpstreamError{
		StatusCode: 400,
		Body:       `{"error":{"message":"The image could not be generated: mock policy refusal.","type":"mock_policy_error"}}`,
	}}
	srv := newTestServer(t, eng)
	rec := doImageReq(t, srv, srv.handleImagesGenerations, http.MethodPost, "/v1/images/generations", []byte(`{"model":"mock-model","prompt":"x"}`), "application/json", "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want the upstream's 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "mock policy refusal") {
		t.Errorf("body = %s, want the upstream error body verbatim", rec.Body.String())
	}
}

// A malformed upstream 200 must answer 502, never 200 — the
// reviewer's probe, reproduced: the fake engine returns the
// exact UpstreamError PostImage used to build for an
// unparseable 200 (StatusCode 200, "malformed images
// response: ..."), and the handler must clamp it to 502 so the
// wire and the telemetry (which already logs upstream_5xx)
// agree.
func TestImagesMalformedUpstream200Is502(t *testing.T) {
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}, imageErr: &provider.UpstreamError{
		StatusCode: http.StatusOK,
		Body:       "malformed images response: unexpected end of JSON input",
	}}
	srv := newTestServer(t, eng)
	rec := doImageReq(t, srv, srv.handleImagesGenerations, http.MethodPost, "/v1/images/generations", []byte(`{"model":"mock-model","prompt":"x"}`), "application/json", "test-key-123")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (malformed upstream 200 is an upstream failure, never a 200)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "malformed images response") {
		t.Errorf("body = %q, want the malformed-response error body", rec.Body.String())
	}
}

// A 429 from the engine forwards the upstream's Retry-After
// end to end at server level: 429 + Retry-After header + the
// upstream error body verbatim.
func TestImagesRateLimitRetryAfterForwarded(t *testing.T) {
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}, imageErr: &provider.RateLimitError{
		RetryAfter:        7 * time.Second,
		RetryAfterPresent: true,
		Body:              `{"error":{"message":"rate limited","type":"rate_limit_error"}}`,
	}}
	srv := newTestServer(t, eng)
	rec := doImageReq(t, srv, srv.handleImagesGenerations, http.MethodPost, "/v1/images/generations", []byte(`{"model":"mock-model","prompt":"x"}`), "application/json", "test-key-123")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want \"7\"", got)
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("body = %s, want the upstream error body verbatim", rec.Body.String())
	}
}

// A plugin model rewrite on generations reaches the engine and
// the upstream body as the rewritten model: the engine resolves
// the chain for B, the JSON body names B, and the ledger names
// B — never a mix where the body says B and the engine saw A.
func TestImagesPluginModelRewriteSyncsGenerations(t *testing.T) {
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}}
	reg := plugins.New()
	if err := reg.Register(modelRewritePlugin{}, false); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))
	rec := doImageReq(t, srv, srv.handleImagesGenerations, http.MethodPost, "/v1/images/generations", []byte(`{"model":"mock-model","prompt":"A red sailboat"}`), "application/json", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if eng.lastReq.Model != "rewritten-model" {
		t.Errorf("engine saw model = %q, want %q (the plugin rewrite)", eng.lastReq.Model, "rewritten-model")
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(eng.lastReq.Body, &sent); err != nil {
		t.Fatalf("engine body is not valid JSON: %v", err)
	}
	if sent.Model != "rewritten-model" {
		t.Errorf("engine body model = %q, want %q (body and engine must agree on the rewritten model)", sent.Model, "rewritten-model")
	}
}

// sniffMultipartFields: model/prompt come out of the raw stream;
// file parts are skipped, not loaded as fields.
func TestSniffMultipartFields(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "mock-model")
	_ = mw.WriteField("prompt", "Add a lighthouse")
	part, _ := mw.CreateFormFile("image", "in.png")
	_, _ = part.Write([]byte{0x89, 'P', 'N', 'G'})
	_ = mw.Close()
	model, prompt, err := sniffMultipartFields(buf.Bytes(), mw.FormDataContentType())
	if err != nil {
		t.Fatalf("sniff: %v", err)
	}
	if model != "mock-model" || prompt != "Add a lighthouse" {
		t.Errorf("model=%q prompt=%q, want mock-model / Add a lighthouse", model, prompt)
	}
	if _, _, err := sniffMultipartFields(buf.Bytes(), "multipart/form-data"); err == nil {
		t.Error("missing boundary: want an error, got nil")
	}
}

// --- apitest fixture contracts (m8.5) ---

// Generations 200: the fixture's recorded response is what the
// fake engine returns; apitest.Run pins the handler answers 200
// with the recorded fields.
func TestImagesGenerations200Fixture(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.5", "images-generations-200")
	raw, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded response: %v", err)
	}
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}, imageResp: &provider.ImageResponse{
		StatusCode: f.RecordedResponse.Status,
		Body:       json.RawMessage(raw),
		Model:      f.Model,
		Usage:      provider.Usage{PromptTokens: 10, CompletionTokens: 15, TotalTokens: 25},
	}}
	srv := newTestServer(t, eng)
	apitest.Run(t, http.HandlerFunc(srv.handleImagesGenerations), f)
}

// Generations 400: the fixture's recorded error is what the fake
// engine returns as an UpstreamError; the handler must answer
// with the upstream's status and error body.
func TestImagesGenerations400UpstreamErrorFixture(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.5", "images-generations-400-upstream-error")
	raw, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded response: %v", err)
	}
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}, imageErr: &provider.UpstreamError{
		StatusCode: f.RecordedResponse.Status,
		Body:       string(raw),
	}}
	srv := newTestServer(t, eng)
	apitest.Run(t, http.HandlerFunc(srv.handleImagesGenerations), f)
}

// Edits 200: the harness cannot send multipart, so the test
// builds a real multipart request itself, loads the fixture for
// the recorded response + assertions, and checks with
// CheckResponse directly.
func TestImagesEdits200Fixture(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.5", "images-edits-200")
	raw, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded response: %v", err)
	}
	eng := &imageFakeEngine{fakeEngine: &fakeEngine{}, imageResp: &provider.ImageResponse{
		StatusCode: f.RecordedResponse.Status,
		Body:       json.RawMessage(raw),
		Model:      f.Model,
	}}
	srv := newTestServer(t, eng)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", f.Model)
	_ = mw.WriteField("prompt", "Add a lighthouse")
	part, err := mw.CreateFormFile("image", "in.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	_, _ = part.Write([]byte{0x89, 'P', 'N', 'G'})
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, f.Path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key-123")
	rec := httptest.NewRecorder()
	srv.handleImagesEdits(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if err := apitest.CheckResponse(resp, respBody, f); err != nil {
		t.Fatalf("apitest: fixture %s/%s: %v", f.Slice, f.Name, err)
	}
}

// Toggle proof: the same 200 response must FAIL when the
// fixture expects a different status.
func TestImagesGenerationsFixtureWrongStatusFails(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.5", "images-generations-200")
	f.Assert.Status = 201 // wrong on purpose: the response is 200
	raw, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded response: %v", err)
	}
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	if err := apitest.CheckResponse(resp, raw, f); err == nil {
		t.Fatal("CheckResponse accepted a 200 response against assert.status 201")
	} else if !strings.Contains(err.Error(), "status") {
		t.Fatalf("wrong-status error should name status, got: %v", err)
	}
}
