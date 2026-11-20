// Images client tests (M8.5): the verbatim passthrough against
// a mock upstream — no live provider is ever called.
package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mockImagesUpstream(t *testing.T, handler http.HandlerFunc) (*httptest.Server, Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "mock-key", WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return srv, c
}

// Generations: the JSON body is POSTed to /v1/images/generations
// verbatim, and the response body comes back verbatim with the
// model and image-token usage parsed out of it.
func TestPostImageGenerationsVerbatim(t *testing.T) {
	var gotPath, gotCT string
	var gotBody []byte
	_, c := mockImagesUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotCT = r.URL.Path, r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		if got := r.Header.Get("Authorization"); got != "Bearer mock-key" {
			t.Errorf("Authorization = %q, want Bearer mock-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1794000000,"data":[{"b64_json":"AAAABBBB"}],"model":"mock-model","usage":{"input_tokens":10,"output_tokens":15,"total_tokens":25}}`))
	})
	poster, ok := c.(ImagePoster)
	if !ok {
		t.Fatal("client does not implement ImagePoster")
	}
	reqBody := []byte(`{"model":"mock-model","prompt":"A red sailboat","size":"1024x1024"}`)
	resp, err := poster.PostImage(context.Background(), ImageRequest{
		Operation: ImageGenerations, Model: "mock-model", Body: reqBody,
		ContentType: "application/json", PromptText: "A red sailboat",
	})
	if err != nil {
		t.Fatalf("PostImage: %v", err)
	}
	if gotPath != "/v1/images/generations" {
		t.Errorf("upstream path = %q, want /v1/images/generations", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("upstream Content-Type = %q, want application/json", gotCT)
	}
	if string(gotBody) != string(reqBody) {
		t.Errorf("upstream body = %s, want verbatim %s", gotBody, reqBody)
	}
	want := `{"created":1794000000,"data":[{"b64_json":"AAAABBBB"}],"model":"mock-model","usage":{"input_tokens":10,"output_tokens":15,"total_tokens":25}}`
	if string(resp.Body) != want {
		t.Errorf("response body = %s, want verbatim %s", resp.Body, want)
	}
	if resp.Model != "mock-model" {
		t.Errorf("model = %q, want mock-model", resp.Model)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 15 || resp.Usage.TotalTokens != 25 {
		t.Errorf("usage = %+v, want input 10 / output 15 / total 25", resp.Usage)
	}
	if resp.Usage.Endpoint != EndpointImages {
		t.Errorf("usage endpoint = %q, want images", resp.Usage.Endpoint)
	}
}

// Edits: the multipart body and its boundary survive verbatim,
// and a response with no usage block yields zero usage —
// unknown, never an estimate.
func TestPostImageEditsMultipartVerbatim(t *testing.T) {
	var gotPath, gotCT string
	var gotBody []byte
	_, c := mockImagesUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotCT = r.URL.Path, r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1794000000,"data":[{"b64_json":"CCCC"}],"model":"mock-model"}`))
	})
	poster := c.(ImagePoster)
	raw := []byte("------MockBoundary\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nmock-model\r\n------MockBoundary\r\nContent-Disposition: form-data; name=\"image\"; filename=\"in.png\"\r\nContent-Type: image/png\r\n\r\n\x89PNG\r\n------MockBoundary--\r\n")
	resp, err := poster.PostImage(context.Background(), ImageRequest{
		Operation: ImageEdits, Model: "mock-model", Body: raw,
		ContentType: "multipart/form-data; boundary=----MockBoundary", PromptText: "Add a lighthouse",
	})
	if err != nil {
		t.Fatalf("PostImage: %v", err)
	}
	if gotPath != "/v1/images/edits" {
		t.Errorf("upstream path = %q, want /v1/images/edits", gotPath)
	}
	if gotCT != "multipart/form-data; boundary=----MockBoundary" {
		t.Errorf("upstream Content-Type = %q, want the multipart Content-Type verbatim", gotCT)
	}
	if string(gotBody) != string(raw) {
		t.Errorf("upstream body differs from the raw multipart bytes (%d vs %d bytes)", len(gotBody), len(raw))
	}
	if resp.Usage != (Usage{Endpoint: EndpointImages}) {
		t.Errorf("usage = %+v, want zero usage with endpoint images (no block reported)", resp.Usage)
	}
}

// A malformed upstream 200 is an upstream failure, never a
// success: PostImage returns a 502 UpstreamError (the realistic
// trigger is a large b64_json response truncated at the 10 MiB
// maxResponseBody cap failing json.Unmarshal), so the server
// answers 502 instead of echoing a 200 with a plain-text error
// body.
func TestPostImageMalformedUpstream200Is502(t *testing.T) {
	_, c := mockImagesUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Truncated mid-value, exactly what a body cut at the
		// response cap looks like.
		_, _ = w.Write([]byte(`{"created":1794000000,"data":[{"b64_json":"AAA`))
	})
	poster := c.(ImagePoster)
	_, err := poster.PostImage(context.Background(), ImageRequest{
		Operation: ImageGenerations, Model: "mock-model",
		Body: []byte(`{"model":"mock-model","prompt":"x"}`), PromptText: "x",
	})
	if err == nil {
		t.Fatal("want a malformed-response error, got nil")
	}
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("error = %T %v, want *UpstreamError", err, err)
	}
	if ue.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (malformed upstream 200 is an upstream failure, never a 200)", ue.StatusCode)
	}
	if !strings.Contains(ue.Body, "malformed images response") {
		t.Errorf("body = %q, want it to name the malformed images response", ue.Body)
	}
	if strings.Contains(ue.Error(), "HTTP 200") {
		t.Errorf("Error() = %q, want no 'HTTP 200' (the status is 502 now)", ue.Error())
	}
}

// An upstream 4xx comes back as the typed error carrying the
// upstream's status and body, so the server can answer with
// them verbatim instead of a gateway-invented 502.
func TestPostImageUpstreamErrorCarriesStatusAndBody(t *testing.T) {
	_, c := mockImagesUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad prompt","type":"invalid_request_error"}}`))
	})
	poster := c.(ImagePoster)
	_, err := poster.PostImage(context.Background(), ImageRequest{
		Operation: ImageGenerations, Model: "mock-model",
		Body: []byte(`{"model":"mock-model","prompt":"x"}`), PromptText: "x",
	})
	if err == nil {
		t.Fatal("want an upstream error, got nil")
	}
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("error = %T %v, want *UpstreamError", err, err)
	}
	if ue.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", ue.StatusCode)
	}
	if !strings.Contains(ue.Body, "bad prompt") {
		t.Errorf("body = %q, want it to carry the upstream error", ue.Body)
	}
}

// Validation at the transport: streaming, a missing prompt and
// a non-multipart edits body never reach the wire.
func TestImageRequestValidate(t *testing.T) {
	cases := map[string]ImageRequest{
		"upscale is not an endpoint": {Operation: ImageOperation("upscale"), Model: "m", Body: []byte(`{"model":"m","prompt":"x"}`)},
		"missing model":              {Operation: ImageGenerations, Model: "", Body: []byte(`{"model":"","prompt":"x"}`)},
		"missing prompt":             {Operation: ImageGenerations, Model: "m", Body: []byte(`{"model":"m","prompt":""}`)},
		"streaming":                  {Operation: ImageGenerations, Model: "m", Body: []byte(`{"model":"m","prompt":"x","stream":true}`)},
		"bad json":                   {Operation: ImageGenerations, Model: "m", Body: []byte(`{`)},
		"edits not multipart":        {Operation: ImageEdits, Model: "m", Body: []byte(`x`), ContentType: "application/json"},
		"empty body":                 {Operation: ImageGenerations, Model: "m", Body: nil},
	}
	for name, req := range cases {
		if err := req.Validate(); err == nil {
			t.Errorf("%s: want a validation error, got nil", name)
		}
	}
	if err := (ImageRequest{Operation: ImageGenerations, Model: "m", Body: []byte(`{"model":"m","prompt":"x"}`)}).Validate(); err != nil {
		t.Errorf("valid generations request: %v", err)
	}
	if err := (ImageRequest{Operation: ImageEdits, Model: "m", Body: []byte(`x`), ContentType: "multipart/form-data; boundary=b"}).Validate(); err != nil {
		t.Errorf("valid edits request: %v", err)
	}
}

// Usage mapping: prompt_tokens/completion_tokens spellings from
// an OpenAI-compatible provider map the same way as
// input_tokens/output_tokens.
func TestImageUsagePromptTokensSpelling(t *testing.T) {
	var decoded imageWireResponse
	if err := json.Unmarshal([]byte(`{"model":"m","usage":{"prompt_tokens":7,"completion_tokens":9,"total_tokens":16}}`), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	u := decoded.usage()
	if u.PromptTokens != 7 || u.CompletionTokens != 9 || u.TotalTokens != 16 {
		t.Errorf("usage = %+v, want 7/9/16", u)
	}
}
