// Audio passthrough engine tests (M8.6): the exactly-once
// invariant, native-wire skipping, the no-capable-provider
// honest 400, verbatim body forwarding, and the upstream-path
// mapping. Upstream calls go to a mock client implementing
// provider.AudioPoster — no live provider is called — plus one
// end-to-end test through the REAL *openAIClient against an
// httptest upstream, proving the router adapter selects and
// drives a production client (not just a mock).
package router

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// audioFakeClient is a fakeClient that also serves audio
// passthrough, with a scripted function and a call counter. It
// embeds *fakeClient, so Chat/ChatStream/ListModels/TestKey and
// WireFormat are the fake's; setting the fake's wireFormat to a
// native wire ("anthropic", "gemini", ...) makes the engine skip
// it at selection even though the method exists — exactly like
// the real native clients, which inherit methods from
// *openAIClient but speak no OpenAI wire. The audio method is
// provider.AudioPoster.PostAudio (the router asserts that
// interface, not a router-side one — see
// audio_passthrough.go).
type audioFakeClient struct {
	*fakeClient
	mu      sync.Mutex
	calls   int
	audioFn func(ctx context.Context, req provider.AudioRequest) (*provider.AudioResponse, error)
	lastReq provider.AudioRequest
}

func (c *audioFakeClient) PostAudio(ctx context.Context, req provider.AudioRequest) (*provider.AudioResponse, error) {
	c.mu.Lock()
	c.calls++
	c.lastReq = req
	fn := c.audioFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &provider.AudioResponse{
		StatusCode:  200,
		ContentType: "application/json",
		Body:        []byte(`{"text":"hello from the mock upstream"}`),
		Model:       req.Model,
	}, nil
}

func (c *audioFakeClient) audioCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *audioFakeClient) lastAudioReq() provider.AudioRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastReq
}

func speechReq() AudioRequest {
	return AudioRequest{
		Operation:   AudioSpeech,
		Model:       "mock-tts-model",
		Body:        []byte(`{"model":"mock-tts-model","input":"Hello world","voice":"alloy"}`),
		ContentType: "application/json",
		Text:        "Hello world",
	}
}

func transcriptionReq() AudioRequest {
	// A canned multipart body: the bytes are opaque to the
	// engine — it forwards them, never parses them. The
	// boundary and the file part are mock data, never a real
	// recording.
	return AudioRequest{
		Operation:   AudioTranscriptions,
		Model:       "mock-stt-model",
		Body:        []byte("--mockboundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"mock.wav\"\r\nContent-Type: audio/wav\r\n\r\nMOCKAUDIOBYTES\r\n--mockboundary\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nmock-stt-model\r\n--mockboundary--\r\n"),
		ContentType: "multipart/form-data; boundary=mockboundary",
	}
}

// The upstream path mapping is the contract the provider
// transport slice will post to: one canonical operation, one
// /v1/audio/* path, no invented names.
func TestAudioUpstreamPaths(t *testing.T) {
	for op, want := range map[AudioOperation]string{
		AudioSpeech:         "audio/speech",
		AudioTranscriptions: "audio/transcriptions",
		AudioTranslations:   "audio/translations",
	} {
		got, ok := audioUpstreamPath(op)
		if !ok || got != want {
			t.Errorf("operation %q: path = %q, ok = %v, want %q, true", op, got, ok, want)
		}
	}
	if _, ok := audioUpstreamPath("speech-to-text"); ok {
		t.Error("alias \"speech-to-text\" must not map to an upstream path: it is a server-side alias for transcriptions, not a canonical operation")
	}
	if _, ok := audioUpstreamPath("text-to-speech"); ok {
		t.Error("alias \"text-to-speech\" must not map to an upstream path: it is a server-side alias for speech, not a canonical operation")
	}
}

// Validation: unknown operation, missing model, empty body.
func TestAudioRequestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  AudioRequest
	}{
		{"alias is not an operation", AudioRequest{Operation: "speech-to-text", Model: "m", Body: []byte("x")}},
		{"unknown operation", AudioRequest{Operation: "sound-generation", Model: "m", Body: []byte("x")}},
		{"missing model", AudioRequest{Operation: AudioSpeech, Body: []byte("x")}},
		{"empty body", AudioRequest{Operation: AudioSpeech, Model: "m"}},
	} {
		if err := tc.req.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", tc.name)
		}
	}
	if err := speechReq().Validate(); err != nil {
		t.Errorf("speechReq().Validate() = %v, want nil", err)
	}
}

// Exactly once: the first target's failure ends the request —
// the second target is never dialled, even though it would have
// succeeded. A 429 from the first target is still a failure of
// the one attempt, not a reason to replay.
func TestAudioExactlyOnceFirstFailureNeverCallsSecond(t *testing.T) {
	a := &audioFakeClient{fakeClient: &fakeClient{}, audioFn: func(ctx context.Context, req provider.AudioRequest) (*provider.AudioResponse, error) {
		return nil, &provider.UpstreamError{StatusCode: 429, Body: "rate limited"}
	}}
	b := &audioFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	_, err := eng.AudioPassthrough(context.Background(), speechReq())
	if err == nil {
		t.Fatal("want the first target's error, got nil")
	}
	if a.audioCalls() != 1 {
		t.Errorf("first target calls = %d, want 1", a.audioCalls())
	}
	if b.audioCalls() != 0 {
		t.Errorf("second target calls = %d, want 0 (exactly once — no replay)", b.audioCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || trace.Endpoint != "audio_speech" {
		t.Fatalf("trace endpoint = %v, want audio_speech", trace)
	}
	if len(trace.Attempts) != 1 {
		t.Fatalf("trace attempts = %d, want 1", len(trace.Attempts))
	}
	if trace.Attempts[0].Outcome != "error" {
		t.Errorf("attempt outcome = %q, want error", trace.Attempts[0].Outcome)
	}
	if trace.Attempts[0].Preread != "exempt:audio_speech" {
		t.Errorf("preread = %q, want exempt:audio_speech", trace.Attempts[0].Preread)
	}
}

// Success: the provider's response comes back verbatim — status,
// content type and body bytes untouched — and the multipart body
// is forwarded byte-for-byte, never decoded and rebuilt.
func TestAudioSuccessReturnsProviderResponseVerbatim(t *testing.T) {
	audioBytes := []byte("ID3MOCKAUDIO")
	a := &audioFakeClient{fakeClient: &fakeClient{}, audioFn: func(ctx context.Context, req provider.AudioRequest) (*provider.AudioResponse, error) {
		return &provider.AudioResponse{StatusCode: 200, ContentType: "audio/mpeg", Body: audioBytes, Model: req.Model}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, testConfig())
	resp, err := eng.AudioPassthrough(context.Background(), speechReq())
	if err != nil {
		t.Fatalf("AudioPassthrough() err = %v, want nil", err)
	}
	if string(resp.Body) != string(audioBytes) || resp.ContentType != "audio/mpeg" || resp.StatusCode != 200 {
		t.Errorf("response = %+v, want the mock audio bytes verbatim (200, audio/mpeg)", resp)
	}
	if a.audioCalls() != 1 {
		t.Errorf("audio calls = %d, want 1", a.audioCalls())
	}
	if got := a.lastAudioReq(); string(got.Body) != string(speechReq().Body) || got.ContentType != "application/json" {
		t.Errorf("forwarded body/content-type = %q / %q, want the caller's JSON body verbatim", got.Body, got.ContentType)
	}

	// Transcriptions: the multipart body reaches the client
	// byte-for-byte, boundary and file part included.
	b := &audioFakeClient{fakeClient: &fakeClient{}}
	eng2 := NewEngine(map[string]provider.Client{"b": b}, nil, testConfig())
	req := transcriptionReq()
	resp2, err := eng2.AudioPassthrough(context.Background(), req)
	if err != nil {
		t.Fatalf("transcriptions AudioPassthrough() err = %v, want nil", err)
	}
	if string(resp2.Body) != `{"text":"hello from the mock upstream"}` {
		t.Errorf("transcriptions body = %q, want the mock transcript JSON", resp2.Body)
	}
	if got := b.lastAudioReq(); string(got.Body) != string(req.Body) {
		t.Error("multipart body was not forwarded byte-for-byte: the engine must never decode-and-rebuild it")
	}
	if !strings.Contains(b.lastAudioReq().ContentType, "boundary=mockboundary") {
		t.Errorf("forwarded content-type = %q, want the caller's multipart boundary preserved", b.lastAudioReq().ContentType)
	}
	trace := eng2.LastTrace()
	if trace == nil || trace.Endpoint != "audio_transcriptions" {
		t.Fatalf("trace endpoint = %v, want audio_transcriptions", trace)
	}
}

// Native-wire skipping: a client on a native wire is skipped
// before dialling even though it implements the method — only
// an OpenAI-compatible surface offers /v1/audio/*.
func TestAudioSkipsNativeWireClient(t *testing.T) {
	native := &audioFakeClient{fakeClient: &fakeClient{wireFormat: "anthropic"}}
	openai := &audioFakeClient{fakeClient: &fakeClient{wireFormat: "openai"}}
	eng := NewEngine(map[string]provider.Client{"native": native, "openai": openai}, nil, testConfig())
	_, err := eng.AudioPassthrough(context.Background(), speechReq())
	if err != nil {
		t.Fatalf("AudioPassthrough() err = %v, want nil (openai target serves it)", err)
	}
	if native.audioCalls() != 0 {
		t.Errorf("native-wire client calls = %d, want 0 (skipped, never dialled)", native.audioCalls())
	}
	if openai.audioCalls() != 1 {
		t.Errorf("openai-wire client calls = %d, want 1", openai.audioCalls())
	}
}

// No capable provider: when no client in the chain implements
// provider.AudioPoster at all, the engine says so honestly with
// a NoCapableProviderError (the server answers 400 naming the
// endpoint) — never a 502, never a faked clip or transcript.
func TestAudioNoCapableProviderIsHonest400(t *testing.T) {
	plain := &fakeClient{}
	eng := NewEngine(map[string]provider.Client{"a": plain}, nil, testConfig())
	_, err := eng.AudioPassthrough(context.Background(), speechReq())
	if err == nil {
		t.Fatal("want NoCapableProviderError, got nil")
	}
	var ncp *NoCapableProviderError
	if !errors.As(err, &ncp) {
		t.Fatalf("err = %T %v, want *NoCapableProviderError", err, err)
	}
	if ncp.Endpoint != EndpointAudioSpeech {
		t.Errorf("endpoint = %q, want audio_speech", ncp.Endpoint)
	}
	if plain.chats != 0 {
		t.Errorf("chat calls = %d, want 0 (no provider was dialled)", plain.chats)
	}
}

// Toggle verification: removing the audio capability from the
// only client (plain fakeClient, no PostAudio method)
// turns the success case into the honest 400 above — the
// positive tests fail without the feature, they do not pass
// vacuously. Pinned here by asserting the two engines differ.
func TestAudioToggleCapabilityChangesOutcome(t *testing.T) {
	withAudio := NewEngine(map[string]provider.Client{"a": &audioFakeClient{fakeClient: &fakeClient{}}}, nil, testConfig())
	if _, err := withAudio.AudioPassthrough(context.Background(), speechReq()); err != nil {
		t.Fatalf("with audio capability: err = %v, want nil", err)
	}
	withoutAudio := NewEngine(map[string]provider.Client{"a": &fakeClient{}}, nil, testConfig())
	if _, err := withoutAudio.AudioPassthrough(context.Background(), speechReq()); err == nil {
		t.Fatal("without audio capability: err = nil, want NoCapableProviderError (toggle: the feature must matter)")
	}
}

// Strategy projection: a chat-only combo strategy ("fusion")
// fails closed on audio, naming the strategy and the endpoint
// — it is never silently substituted. The default "priority"
// (and the other non-chat strategies) stand, as the success
// tests above prove under the default config.
func TestAudioChatOnlyStrategyFailsClosed(t *testing.T) {
	cfg := testConfig()
	cfg.Strategy = "fusion"
	a := &audioFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	_, err := eng.AudioPassthrough(context.Background(), speechReq())
	if err == nil {
		t.Fatal("want a projection error for strategy \"fusion\" on audio, got nil")
	}
	if !strings.Contains(err.Error(), "fusion") || !strings.Contains(err.Error(), "audio-speech") {
		t.Errorf("err = %v, want it to name strategy \"fusion\" and endpoint \"audio-speech\"", err)
	}
	if a.audioCalls() != 0 {
		t.Errorf("audio calls = %d, want 0 (projection refuses before any dial)", a.audioCalls())
	}
}

// A non-2xx response from the transport is an upstream error,
// never a delivered response: the client must not hand the
// caller a 4xx/5xx body as if it were audio.
func TestAudioNon2xxIsUpstreamError(t *testing.T) {
	a := &audioFakeClient{fakeClient: &fakeClient{}, audioFn: func(ctx context.Context, req provider.AudioRequest) (*provider.AudioResponse, error) {
		return &provider.AudioResponse{StatusCode: 400, ContentType: "application/json", Body: []byte(`{"error":{"message":"bad voice"}}`)}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, testConfig())
	resp, err := eng.AudioPassthrough(context.Background(), speechReq())
	if resp != nil {
		t.Errorf("resp = %+v, want nil on a non-2xx upstream status", resp)
	}
	if err == nil {
		t.Fatal("want an upstream error, got nil")
	}
	var ue *provider.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 400 {
		t.Errorf("err = %T %v, want *provider.UpstreamError with status 400", err, err)
	}
}

// End-to-end through the REAL provider client: the engine must
// select a production *openAIClient (provider.AudioPoster) and
// drive it to an httptest upstream, returning the clip verbatim.
// This is the test the M8.6 merge gate asked for: a mock-only
// suite cannot prove audio works in production, because the
// router used to select on an interface (AudioPassthroughClient)
// no production client implemented — audio answered an honest
// 400 until the callAudio adapter landed. If the adapter is
// removed, this test fails with NoCapableProviderError.
func TestAudioEndToEndRealClient(t *testing.T) {
	var mu sync.Mutex
	var hits int
	var gotPath, gotAuth, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits++
		gotPath, gotAuth, gotCT, gotBody = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), raw
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/audio/speech") {
			w.Header().Set("Content-Type", "audio/mpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ID3REALCLIP"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"hello from the real upstream"}`))
	}))
	defer srv.Close()
	client, err := provider.NewClient(srv.URL, "test-key", provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	eng := NewEngine(map[string]provider.Client{"a": client}, nil, testConfig())

	// Speech: JSON in, audio bytes out, verbatim.
	resp, err := eng.AudioPassthrough(context.Background(), speechReq())
	if err != nil {
		t.Fatalf("speech AudioPassthrough() err = %v, want nil", err)
	}
	if resp.StatusCode != 200 || resp.ContentType != "audio/mpeg" || string(resp.Body) != "ID3REALCLIP" {
		t.Errorf("speech response = %+v, want the upstream clip verbatim (200, audio/mpeg)", resp)
	}
	mu.Lock()
	if hits != 1 || gotPath != "/v1/audio/speech" || gotAuth != "Bearer test-key" || gotCT != "application/json" || string(gotBody) != string(speechReq().Body) {
		t.Errorf("speech upstream: hits=%d path=%q auth=%q ct=%q body=%q, want 1 /v1/audio/speech Bearer test-key application/json + the speech JSON verbatim", hits, gotPath, gotAuth, gotCT, gotBody)
	}
	mu.Unlock()

	// Transcriptions: multipart in (boundary preserved), JSON out.
	req := transcriptionReq()
	resp2, err := eng.AudioPassthrough(context.Background(), req)
	if err != nil {
		t.Fatalf("transcriptions AudioPassthrough() err = %v, want nil", err)
	}
	if resp2.StatusCode != 200 || string(resp2.Body) != `{"text":"hello from the real upstream"}` {
		t.Errorf("transcriptions response = %+v, want the upstream transcript JSON verbatim", resp2)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 2 || gotPath != "/v1/audio/transcriptions" {
		t.Errorf("transcriptions upstream: hits=%d path=%q, want 2 /v1/audio/transcriptions", hits, gotPath)
	}
	if !strings.Contains(gotCT, "boundary=mockboundary") {
		t.Errorf("transcriptions upstream Content-Type = %q, want the multipart boundary preserved", gotCT)
	}
	if string(gotBody) != string(req.Body) {
		t.Error("transcriptions upstream body differs from the sent multipart bytes (verbatim rule)")
	}
}
