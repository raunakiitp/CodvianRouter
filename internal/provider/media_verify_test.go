// M8.20 media passthrough client tests: verbatim body + status,
// raw path joining (no /v1 insertion), auth + per-call headers,
// non-2xx mapping, and request validation. The upstream is an
// httptest server throughout — no real provider is called.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mediaUpstream records the request it saw and answers with a
// scripted status + body.
type mediaUpstream struct {
	t       *testing.T
	status  int
	body    string
	method  string
	path    string
	auth    string
	ct      string
	version string
	rawBody string
	hits    int
}

func (u *mediaUpstream) handler(w http.ResponseWriter, r *http.Request) {
	u.hits++
	u.method = r.Method
	u.path = r.URL.Path
	u.auth = r.Header.Get("Authorization")
	u.ct = r.Header.Get("Content-Type")
	u.version = r.Header.Get("X-Runway-Version")
	raw, _ := io.ReadAll(r.Body)
	u.rawBody = string(raw)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(u.status)
	_, _ = w.Write([]byte(u.body))
}

func mediaClient(t *testing.T, up *mediaUpstream) *openAIClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
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

func TestMediaPassthroughPostVerbatimNoV1Insertion(t *testing.T) {
	up := &mediaUpstream{status: 200, body: `{"id":"srch-1","results":[{"title":"T","url":"https://example.com","snippet":"S"}]}`}
	c := mediaClient(t, up)
	resp, err := c.MediaPassthrough(context.Background(), MediaPassthroughRequest{
		ProviderID: "perplexity",
		Model:      "perplexity",
		Endpoint:   EndpointSearch,
		Method:     http.MethodPost,
		Path:       "search",
		Body:       json.RawMessage(`{"query":"hello","max_results":3,"unknown_future_field":true}`),
	})
	if err != nil {
		t.Fatalf("MediaPassthrough: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	if string(resp.Body) != up.body {
		t.Errorf("body = %s, want %s", resp.Body, up.body)
	}
	if up.hits != 1 {
		t.Errorf("upstream hits = %d, want 1", up.hits)
	}
	// The raw join must NOT insert /v1: Perplexity's search path
	// is "search" on a base with no /v1.
	if up.path != "/search" {
		t.Errorf("upstream path = %q, want /search (no /v1 inserted)", up.path)
	}
	if up.method != http.MethodPost {
		t.Errorf("upstream method = %q, want POST", up.method)
	}
	if up.auth != "Bearer test-key" {
		t.Errorf("upstream Authorization = %q, want Bearer test-key", up.auth)
	}
	if !strings.Contains(up.rawBody, `"unknown_future_field":true`) {
		t.Errorf("upstream body = %s, want the verbatim request body", up.rawBody)
	}
}

func TestMediaPassthroughPreservesNon200SuccessStatus(t *testing.T) {
	up := &mediaUpstream{status: 201, body: `{"id":"task-1","status":"PENDING"}`}
	c := mediaClient(t, up)
	resp, err := c.MediaPassthrough(context.Background(), MediaPassthroughRequest{
		ProviderID:   "runwayml",
		Model:        "seedance2_5",
		Endpoint:     EndpointVideo,
		Method:       http.MethodPost,
		Path:         "text_to_video",
		Body:         json.RawMessage(`{"model":"seedance2_5","promptText":"mist"}`),
		ExtraHeaders: map[string]string{"X-Runway-Version": "2024-11-06"},
	})
	if err != nil {
		t.Fatalf("MediaPassthrough: %v", err)
	}
	if resp.Status != 201 {
		t.Errorf("status = %d, want the upstream 201 preserved", resp.Status)
	}
	if up.path != "/text_to_video" {
		t.Errorf("upstream path = %q, want /text_to_video (mock base has no /v1)", up.path)
	}
	if up.version != "2024-11-06" {
		t.Errorf("upstream X-Runway-Version = %q, want 2024-11-06", up.version)
	}
}

func TestMediaPassthroughGetNoBody(t *testing.T) {
	up := &mediaUpstream{status: 200, body: `{"id":"task-1","status":"SUCCEEDED","output":["https://example.com/v.mp4"]}`}
	c := mediaClient(t, up)
	resp, err := c.MediaPassthrough(context.Background(), MediaPassthroughRequest{
		ProviderID: "runwayml",
		Model:      "runwayml",
		Endpoint:   EndpointVideo,
		Method:     http.MethodGet,
		Path:       "tasks/task-1",
	})
	if err != nil {
		t.Fatalf("MediaPassthrough: %v", err)
	}
	if resp.Status != 200 || string(resp.Body) != up.body {
		t.Errorf("resp = %d %s, want 200 %s", resp.Status, resp.Body, up.body)
	}
	if up.method != http.MethodGet {
		t.Errorf("upstream method = %q, want GET", up.method)
	}
	if up.rawBody != "" {
		t.Errorf("upstream body = %q, want empty on GET", up.rawBody)
	}
	if up.ct != "" {
		t.Errorf("upstream Content-Type = %q, want empty on GET", up.ct)
	}
}

func TestMediaPassthroughNon2xxMapsToUpstreamError(t *testing.T) {
	// A 429 maps to *RateLimitError (the same mapping Embed gets);
	// a plain 4xx maps to *UpstreamError with the status preserved.
	up429 := &mediaUpstream{status: 429, body: `{"error":{"message":"slow down"}}`}
	c := mediaClient(t, up429)
	_, err := c.MediaPassthrough(context.Background(), MediaPassthroughRequest{
		ProviderID: "mistral",
		Model:      "mistral-ocr-latest",
		Endpoint:   EndpointOCR,
		Method:     http.MethodPost,
		Path:       "ocr",
		Body:       json.RawMessage(`{"model":"mistral-ocr-latest"}`),
	})
	if err == nil {
		t.Fatal("want an error for a 429 upstream, got nil")
	}
	if _, ok := err.(*RateLimitError); !ok {
		t.Fatalf("429 error = %T, want *RateLimitError", err)
	}
	up400 := &mediaUpstream{status: 400, body: `{"error":{"message":"bad model"}}`}
	c2 := mediaClient(t, up400)
	_, err = c2.MediaPassthrough(context.Background(), MediaPassthroughRequest{
		ProviderID: "mistral",
		Model:      "mistral-ocr-latest",
		Endpoint:   EndpointOCR,
		Method:     http.MethodPost,
		Path:       "ocr",
		Body:       json.RawMessage(`{"model":"mistral-ocr-latest"}`),
	})
	if err == nil {
		t.Fatal("want an error for a 400 upstream, got nil")
	}
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("400 error = %T, want *UpstreamError", err)
	}
	if ue.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", ue.StatusCode)
	}
	if ue.Retryable {
		t.Errorf("Retryable = true, want false for a 400")
	}
}

func TestMediaPassthroughExtraHeadersCannotOverrideAuth(t *testing.T) {
	up := &mediaUpstream{status: 200, body: `{}`}
	c := mediaClient(t, up)
	_, err := c.MediaPassthrough(context.Background(), MediaPassthroughRequest{
		ProviderID: "perplexity",
		Model:      "perplexity",
		Endpoint:   EndpointSearch,
		Method:     http.MethodPost,
		Path:       "search",
		Body:       json.RawMessage(`{"query":"q"}`),
		ExtraHeaders: map[string]string{
			"Authorization": "Bearer planted",
			"Content-Type":  "text/plain",
		},
	})
	if err != nil {
		t.Fatalf("MediaPassthrough: %v", err)
	}
	if up.auth != "Bearer test-key" {
		t.Errorf("upstream Authorization = %q, want the real key (planted header ignored)", up.auth)
	}
	if up.ct != "application/json" {
		t.Errorf("upstream Content-Type = %q, want application/json", up.ct)
	}
}

func TestMediaPassthroughValidate(t *testing.T) {
	c := &openAIClient{baseURL: "https://example.com", apiKey: "k", httpClient: &http.Client{}}
	cases := []struct {
		name string
		req  MediaPassthroughRequest
	}{
		{"no provider", MediaPassthroughRequest{Endpoint: EndpointSearch, Method: http.MethodPost, Path: "/search", Body: json.RawMessage(`{"query":"q"}`)}},
		{"no endpoint", MediaPassthroughRequest{ProviderID: "perplexity", Method: http.MethodPost, Path: "/search", Body: json.RawMessage(`{"query":"q"}`)}},
		{"no path", MediaPassthroughRequest{ProviderID: "perplexity", Endpoint: EndpointSearch, Method: http.MethodPost, Body: json.RawMessage(`{"query":"q"}`)}},
		{"bad method", MediaPassthroughRequest{ProviderID: "perplexity", Endpoint: EndpointSearch, Method: http.MethodDelete, Path: "/search", Body: json.RawMessage(`{"query":"q"}`)}},
		{"POST no body", MediaPassthroughRequest{ProviderID: "perplexity", Endpoint: EndpointSearch, Method: http.MethodPost, Path: "/search"}},
		{"POST null body", MediaPassthroughRequest{ProviderID: "perplexity", Endpoint: EndpointSearch, Method: http.MethodPost, Path: "/search", Body: json.RawMessage(`null`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.MediaPassthrough(context.Background(), tc.req); err == nil {
				t.Errorf("want a validation error, got nil")
			}
		})
	}
}

// TestMediaPassthroughLargeHexBodyUnderCapPassesIntact (F1):
// a realistic 4-minute MiniMax song as hex audio — 240 s at
// 256 kbps is ~7.68 MB binary, ~15.36 MB hex, over the shared
// 10 MiB cap but under the media-local 64 MiB cap — must pass
// through intact, byte for byte. Under the old shared cap this
// body was silently cut mid-JSON and returned as a 200.
func TestMediaPassthroughLargeHexBodyUnderCapPassesIntact(t *testing.T) {
	// 3,840,000 x "00ff" = 15,360,000 hex chars ~= 7.68 MB of
	// audio. Built in memory; nothing is written to disk.
	hexAudio := strings.Repeat("00ff", 3840000)
	body := `{"data":{"audio":"` + hexAudio + `","status":2},"base_resp":{"status_code":0,"status_msg":"success"}}`
	if len(body) <= 10*1024*1024 {
		t.Fatalf("test body = %d bytes, want over the old 10 MiB cap", len(body))
	}
	if len(body) >= mediaMaxResponseBody {
		t.Fatalf("test body = %d bytes, want under the 64 MiB media cap", len(body))
	}
	up := &mediaUpstream{status: 200, body: body}
	c := mediaClient(t, up)
	resp, err := c.MediaPassthrough(context.Background(), MediaPassthroughRequest{
		ProviderID: "minimax",
		Model:      "music-3.0",
		Endpoint:   EndpointMusic,
		Method:     http.MethodPost,
		Path:       "music_generation",
		Body:       json.RawMessage(`{"model":"music-3.0","prompt":"folk"}`),
	})
	if err != nil {
		t.Fatalf("MediaPassthrough: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if string(resp.Body) != body {
		t.Errorf("body length = %d, want the full %d-byte upstream body intact", len(resp.Body), len(body))
	}
}

// TestMediaPassthroughOversizeBodyErrorsHonestly (F1): an
// upstream body over the 64 MiB media cap is refused with an
// error naming the truncation — never returned as a corrupt
// 200 with the tail cut off.
func TestMediaPassthroughOversizeBodyErrorsHonestly(t *testing.T) {
	// 17,500,000 x "00ff" = 70,000,000 hex chars, over the
	// 64 MiB cap. Built in memory; nothing is written to disk.
	hexAudio := strings.Repeat("00ff", 17500000)
	body := `{"data":{"audio":"` + hexAudio + `","status":2}}`
	if len(body) <= mediaMaxResponseBody {
		t.Fatalf("test body = %d bytes, want over the 64 MiB media cap", len(body))
	}
	up := &mediaUpstream{status: 200, body: body}
	c := mediaClient(t, up)
	_, err := c.MediaPassthrough(context.Background(), MediaPassthroughRequest{
		ProviderID: "minimax",
		Model:      "music-3.0",
		Endpoint:   EndpointMusic,
		Method:     http.MethodPost,
		Path:       "music_generation",
		Body:       json.RawMessage(`{"model":"music-3.0","prompt":"folk"}`),
	})
	if err == nil {
		t.Fatal("want an error for an over-cap upstream body, got nil (a truncated 200 would be corrupt JSON)")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error = %q, want it to name the truncation", err.Error())
	}
}

func TestJoinRawPath(t *testing.T) {
	cases := []struct{ base, path, want string }{
		// Perplexity: base has no /v1, path is bare — no /v1 inserted.
		{"https://api.perplexity.ai", "search", "https://api.perplexity.ai/search"},
		{"https://api.perplexity.ai/", "/search", "https://api.perplexity.ai/search"},
		// Mistral / Runway / MiniMax: base already ends in /v1, path
		// is relative to it — no second /v1 inserted.
		{"https://api.mistral.ai/v1", "ocr", "https://api.mistral.ai/v1/ocr"},
		{"https://api.mistral.ai/v1/", "ocr", "https://api.mistral.ai/v1/ocr"},
		{"https://api.dev.runwayml.com/v1", "text_to_video", "https://api.dev.runwayml.com/v1/text_to_video"},
		{"https://api.minimax.io/v1", "music_generation", "https://api.minimax.io/v1/music_generation"},
	}
	for _, tc := range cases {
		if got := joinRawPath(tc.base, tc.path); got != tc.want {
			t.Errorf("joinRawPath(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

// M8.20 F3: MediaPassthrough must not be cut off by the provider
// HTTP client's 120 s timeout when the server allows longer
// (music, 10 minutes by default). The request context — bounded
// by the server's per-endpoint timeout — is the sole bound.
func TestMediaPassthroughIgnoresClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	// A 50 ms client timeout would kill this 200 ms upstream
	// call if MediaPassthrough still used httpClient directly.
	c, err := NewClient(srv.URL, "test-key", WithTimeout(50*time.Millisecond), WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	oc, ok := c.(*openAIClient)
	if !ok {
		t.Fatalf("NewClient returned %T, want *openAIClient", c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := oc.MediaPassthrough(ctx, MediaPassthroughRequest{
		ProviderID: "minimax",
		Model:      "music-3.0",
		Endpoint:   EndpointMusic,
		Method:     http.MethodPost,
		Path:       "music_generation",
		Body:       json.RawMessage(`{"model":"music-3.0","prompt":"folk"}`),
	})
	if err != nil {
		t.Fatalf("MediaPassthrough: %v (want success: the 50ms client timeout must not apply)", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
}

// TestMediaPassthroughStillBoundByContext proves the other half:
// with no client-level timeout, a cancelled/expired context must
// still abort the call — the context is the bound, not "no bound".
func TestMediaPassthroughStillBoundByContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep longer than the client's deadline, but stop
		// early when the client goes away, so server Close
		// in cleanup does not wait out the whole sleep.
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "test-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	oc, ok := c.(*openAIClient)
	if !ok {
		t.Fatalf("NewClient returned %T, want *openAIClient", c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = oc.MediaPassthrough(ctx, MediaPassthroughRequest{
		ProviderID: "minimax",
		Model:      "music-3.0",
		Endpoint:   EndpointMusic,
		Method:     http.MethodPost,
		Path:       "music_generation",
		Body:       json.RawMessage(`{"model":"music-3.0","prompt":"folk"}`),
	})
	if err == nil {
		t.Fatalf("MediaPassthrough succeeded, want a context-deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("MediaPassthrough took %v, want it aborted near the 100ms context deadline", elapsed)
	}
}
