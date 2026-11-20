// Audio passthrough client tests (M8.6 merge follow-up): verbatim
// body + status + Content-Type, multipart boundary preservation,
// non-2xx mapping, validation, and the truncation contract —
// a >10 MiB clip passes intact (the old doForEndpoint cap must
// not apply) and a >64 MiB response errors honestly instead of
// delivering a corrupt truncated clip as a 200. The upstream is
// an httptest server throughout — no real provider is called.
package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// audioUpstream records the request it saw and answers with a
// scripted status + body + Content-Type.
type audioUpstream struct {
	t       *testing.T
	status  int
	body    []byte
	ct      string
	method  string
	path    string
	auth    string
	reqCT   string
	rawBody []byte
	hits    int
}

func (u *audioUpstream) handler(w http.ResponseWriter, r *http.Request) {
	u.hits++
	u.method = r.Method
	u.path = r.URL.Path
	u.auth = r.Header.Get("Authorization")
	u.reqCT = r.Header.Get("Content-Type")
	raw, _ := io.ReadAll(r.Body)
	u.rawBody = raw
	if u.ct != "" {
		w.Header().Set("Content-Type", u.ct)
	}
	w.WriteHeader(u.status)
	_, _ = w.Write(u.body)
}

func audioClient(t *testing.T, up *audioUpstream) *openAIClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "test-key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	oc, ok := c.(*openAIClient)
	if !ok {
		t.Fatalf("NewClient returned %T, want *openAIClient", c)
	}
	return oc
}

func TestPostAudioSpeechVerbatim(t *testing.T) {
	clip := []byte("ID3\x04\x00\x00\x00\x00\x00\x00\x00\x00\x00fake-mp3-bytes")
	up := &audioUpstream{status: 200, body: clip, ct: "audio/mpeg"}
	c := audioClient(t, up)
	resp, err := c.PostAudio(context.Background(), AudioRequest{
		Operation: AudioSpeech,
		Model:     "tts-1",
		Body:      []byte(`{"model":"tts-1","input":"hello world","voice":"alloy"}`),
	})
	if err != nil {
		t.Fatalf("PostAudio: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if resp.ContentType != "audio/mpeg" {
		t.Errorf("content-type = %q, want audio/mpeg", resp.ContentType)
	}
	if !bytes.Equal(resp.Body, clip) {
		t.Errorf("body = %q, want the upstream clip verbatim", resp.Body)
	}
	if resp.Model != "tts-1" {
		t.Errorf("model = %q, want tts-1", resp.Model)
	}
	if up.path != "/v1/audio/speech" {
		t.Errorf("upstream path = %q, want /v1/audio/speech", up.path)
	}
	if up.auth != "Bearer test-key" {
		t.Errorf("upstream auth = %q, want Bearer test-key", up.auth)
	}
	if string(up.rawBody) != `{"model":"tts-1","input":"hello world","voice":"alloy"}` {
		t.Errorf("upstream body = %q, want the request JSON verbatim", up.rawBody)
	}
}

func TestPostAudioTranscriptionsMultipartBoundaryPreserved(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "hello.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("fake-audio-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("model", "whisper-1"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	up := &audioUpstream{status: 200, body: []byte(`{"text":"hello world"}`), ct: "application/json"}
	c := audioClient(t, up)
	resp, err := c.PostAudio(context.Background(), AudioRequest{
		Operation:   AudioTranscriptions,
		Model:       "whisper-1",
		Body:        buf.Bytes(),
		ContentType: mw.FormDataContentType(),
	})
	if err != nil {
		t.Fatalf("PostAudio: %v", err)
	}
	if resp.StatusCode != 200 || string(resp.Body) != `{"text":"hello world"}` {
		t.Errorf("response = %d %q, want 200 {\"text\":\"hello world\"}", resp.StatusCode, resp.Body)
	}
	if resp.ContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", resp.ContentType)
	}
	// The multipart boundary the client sent must reach the
	// upstream verbatim, or the upstream cannot parse the parts.
	if up.reqCT != mw.FormDataContentType() {
		t.Errorf("upstream Content-Type = %q, want %q", up.reqCT, mw.FormDataContentType())
	}
	if !bytes.Equal(up.rawBody, buf.Bytes()) {
		t.Errorf("upstream body differs from the sent multipart bytes")
	}
	if up.path != "/v1/audio/transcriptions" {
		t.Errorf("upstream path = %q, want /v1/audio/transcriptions", up.path)
	}
}

func TestPostAudioNon2xxMapsUpstreamError(t *testing.T) {
	up := &audioUpstream{status: 500, body: []byte(`{"error":{"message":"upstream down"}}`), ct: "application/json"}
	c := audioClient(t, up)
	_, err := c.PostAudio(context.Background(), AudioRequest{
		Operation: AudioSpeech,
		Model:     "tts-1",
		Body:      []byte(`{"model":"tts-1","input":"hi"}`),
	})
	if err == nil {
		t.Fatal("PostAudio on a 500 upstream: got nil error")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %T (%v), want *UpstreamError", err, err)
	}
	if ue.StatusCode != 500 {
		t.Errorf("UpstreamError status = %d, want 500", ue.StatusCode)
	}
}

func TestPostAudioRateLimitMapsRateLimitError(t *testing.T) {
	up := &audioUpstream{status: 429, body: []byte(`{"error":{"message":"slow down"}}`), ct: "application/json"}
	c := audioClient(t, up)
	_, err := c.PostAudio(context.Background(), AudioRequest{
		Operation: AudioSpeech,
		Model:     "tts-1",
		Body:      []byte(`{"model":"tts-1","input":"hi"}`),
	})
	if err == nil {
		t.Fatal("PostAudio on a 429 upstream: got nil error")
	}
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("error = %T (%v), want *RateLimitError", err, err)
	}
}

func TestPostAudioLargeClipUnderCapPassesIntact(t *testing.T) {
	// An 11 MiB clip: over the shared 10 MiB doForEndpoint cap
	// that used to truncate silently, under the 64 MiB audio
	// cap. It must pass through byte-for-byte intact.
	clip := bytes.Repeat([]byte{'m'}, 11<<20)
	up := &audioUpstream{status: 200, body: clip, ct: "audio/mpeg"}
	c := audioClient(t, up)
	resp, err := c.PostAudio(context.Background(), AudioRequest{
		Operation: AudioSpeech,
		Model:     "tts-1",
		Body:      []byte(`{"model":"tts-1","input":"a long chapter"}`),
	})
	if err != nil {
		t.Fatalf("PostAudio: %v", err)
	}
	if len(resp.Body) != len(clip) || !bytes.Equal(resp.Body, clip) {
		t.Fatalf("body = %d bytes, want the %d-byte clip intact", len(resp.Body), len(clip))
	}
}

func TestPostAudioOversizeBodyErrorsHonestly(t *testing.T) {
	// A 65 MiB upstream body: over the 64 MiB audio cap. It
	// must error naming the truncation — never a corrupt
	// truncated clip delivered as a 200.
	clip := bytes.Repeat([]byte{'m'}, 65<<20)
	up := &audioUpstream{status: 200, body: clip, ct: "audio/mpeg"}
	c := audioClient(t, up)
	resp, err := c.PostAudio(context.Background(), AudioRequest{
		Operation: AudioSpeech,
		Model:     "tts-1",
		Body:      []byte(`{"model":"tts-1","input":"a very long chapter"}`),
	})
	if err == nil {
		t.Fatalf("PostAudio on a 65 MiB upstream body: got response %d bytes, want an honest error", len(resp.Body))
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want it to name the truncation", err.Error())
	}
}

func TestPostAudioContentLengthOverCapFailsFast(t *testing.T) {
	// The upstream declares 100 MiB in Content-Length but the
	// body is short: the client must refuse before reading,
	// not stream 64 MiB to learn the same.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "104857600")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()
	oc, err := NewClient(srv.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	c := oc.(*openAIClient)
	_, err = c.PostAudio(context.Background(), AudioRequest{
		Operation: AudioSpeech,
		Model:     "tts-1",
		Body:      []byte(`{"model":"tts-1","input":"hi"}`),
	})
	if err == nil {
		t.Fatal("PostAudio with Content-Length 100 MiB: got nil error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want it to name the truncation", err.Error())
	}
}

func TestAudioRequestValidation(t *testing.T) {
	cases := []struct {
		name string
		req  AudioRequest
	}{
		{"unknown operation", AudioRequest{Operation: "sound", Model: "m", Body: []byte(`{}`)}},
		{"empty model", AudioRequest{Operation: AudioSpeech, Body: []byte(`{"input":"hi"}`)}},
		{"empty body", AudioRequest{Operation: AudioSpeech, Model: "tts-1"}},
		{"speech non-JSON", AudioRequest{Operation: AudioSpeech, Model: "tts-1", Body: []byte(`not json`)}},
		{"speech empty input", AudioRequest{Operation: AudioSpeech, Model: "tts-1", Body: []byte(`{"model":"tts-1","input":"  "}`)}},
		{"transcriptions not multipart", AudioRequest{Operation: AudioTranscriptions, Model: "whisper-1", Body: []byte(`audio`), ContentType: "application/json"}},
		{"translations no content-type", AudioRequest{Operation: AudioTranslations, Model: "whisper-1", Body: []byte(`audio`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.req.Validate(); err == nil {
				t.Errorf("Validate(%+v): got nil, want an error", tc.req)
			}
		})
	}
}

func TestPostAudioRejectsInvalidRequestWithoutDialling(t *testing.T) {
	up := &audioUpstream{status: 200, body: []byte(`{}`)}
	c := audioClient(t, up)
	_, err := c.PostAudio(context.Background(), AudioRequest{
		Operation: AudioSpeech,
		Model:     "",
		Body:      []byte(`{"input":"hi"}`),
	})
	if err == nil {
		t.Fatal("PostAudio with no model: got nil error")
	}
	if up.hits != 0 {
		t.Errorf("upstream hits = %d, want 0 (validation precedes any dial)", up.hits)
	}
}
