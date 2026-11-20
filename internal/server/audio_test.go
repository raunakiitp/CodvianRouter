// Audio passthrough server tests (M8.6): fixture contract tests
// for all five routes, validation, the typed no-capable-provider
// 400, the 503 when the engine has no audio capability, and
// verbatim multipart forwarding.
//
// NOTE ON ROUTING: the /v1/audio/* routes are NOT registered in
// guard.go yet — the coordinator wires the registration at
// merge (TierManagement, gateway-key auth, proposed scope
// "chat"). These tests therefore invoke the handlers directly
// instead of going through ServeHTTP, and auth enforcement at
// the guard table is NOT exercised here; it is a coordinator
// merge-step check (the scope table-driven test must cover the
// new routes once registered). The handlers themselves assume
// the guard has already authenticated the key, exactly like the
// embeddings handlers do.
package server

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// audioFakeEngine is a fakeEngine that also serves audio
// passthrough, with a call counter and scripted results.
// LastTrace returns the canned trace the test sets.
type audioFakeEngine struct {
	*fakeEngine
	audioCalls int32
	audioResp  *router.AudioResponse
	audioErr   error
	lastReq    router.AudioRequest
	trace      *routetrace.RouteTrace
}

func (e *audioFakeEngine) AudioPassthrough(ctx context.Context, req router.AudioRequest) (*router.AudioResponse, error) {
	atomic.AddInt32(&e.audioCalls, 1)
	e.lastReq = req
	if e.audioErr != nil {
		return nil, e.audioErr
	}
	if e.audioResp != nil {
		return e.audioResp, nil
	}
	switch req.Operation {
	case router.AudioSpeech:
		return &router.AudioResponse{StatusCode: 200, ContentType: "audio/mpeg", Body: []byte("ID3MOCKAUDIO"), Model: req.Model}, nil
	default:
		return &router.AudioResponse{StatusCode: 200, ContentType: "application/json", Body: []byte(`{"text":"Hello from the mock upstream."}`), Model: req.Model}, nil
	}
}

func (e *audioFakeEngine) LastTrace() *routetrace.RouteTrace { return e.trace }

// doAudioReq invokes one audio handler directly (see the package
// note: routes are unregistered until the coordinator wires
// guard.go) with a gateway key, and returns the recorder.
func doAudioReq(t *testing.T, srv *Server, handler func(http.ResponseWriter, *http.Request), method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer test-key-123")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// mockMultipart builds a real multipart/form-data body with a
// `file` part (mock audio bytes, never a real recording) and a
// `model` field, and returns the body plus its Content-Type
// (boundary included).
func mockMultipart(t *testing.T, model string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "mock.wav")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := fw.Write([]byte("MOCKAUDIOBYTES")); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := mw.WriteField("model", model); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// TestAudioSpeechFixtureContract runs the m8.6 speech fixture
// through apitest.Run against the real handler, with the fake
// engine replaying the fixture's recorded response (audio
// bytes). The fixture asserts status only — the test then checks
// the bytes and Content-Type verbatim itself.
func TestAudioSpeechFixtureContract(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.6", "audio-speech-200")
	eng := &audioFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	apitest.Run(t, http.HandlerFunc(srv.handleAudioSpeech), f)
	if eng.audioCalls != 1 {
		t.Errorf("audio calls = %d, want 1", eng.audioCalls)
	}
	// Verbatim delivery, checked outside the harness (the
	// fixture is status-only because audio bytes are not JSON).
	rec := doAudioReq(t, srv, srv.handleAudioSpeech, http.MethodPost, "/v1/audio/speech",
		[]byte(`{"model":"mock-tts-model","input":"Hello world","voice":"alloy"}`), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %.200s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "ID3MOCKAUDIO" {
		t.Errorf("body = %q, want the mock audio bytes verbatim", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", got)
	}
	if got := rec.Header().Get("X-Codvian-Endpoint"); got != "audio_speech" {
		t.Errorf("X-Codvian-Endpoint = %q, want audio_speech", got)
	}
	if got := eng.lastReq; string(got.Body) != `{"model":"mock-tts-model","input":"Hello world","voice":"alloy"}` {
		t.Errorf("forwarded body = %q, want the caller's JSON verbatim", got.Body)
	}
}

// TestAudioTextToSpeechAliasFixtureContract proves the alias
// route serves the same operation: the text-to-speech fixture
// passes through the alias handler with the same recorded
// response.
func TestAudioTextToSpeechAliasFixtureContract(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.6", "audio-text-to-speech-200")
	eng := &audioFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	apitest.Run(t, http.HandlerFunc(srv.handleAudioTextToSpeech), f)
	if eng.audioCalls != 1 {
		t.Errorf("audio calls = %d, want 1", eng.audioCalls)
	}
	if eng.lastReq.Operation != router.AudioSpeech {
		t.Errorf("operation = %q, want speech (alias must map to the canonical operation)", eng.lastReq.Operation)
	}
}

// checkMultipartFixture is the shared contract check for the
// multipart endpoints: build a real multipart request, call the
// handler directly, and verify the response against the loaded
// fixture with apitest.CheckResponse.
func checkMultipartFixture(t *testing.T, fixtureName, path string, pick func(*Server) func(http.ResponseWriter, *http.Request)) {
	t.Helper()
	f := apitest.LoadFixture(t, "m8.6", fixtureName)
	eng := &audioFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	body, contentType := mockMultipart(t, "mock-stt-model")
	rec := doAudioReq(t, srv, pick(srv), http.MethodPost, path, body, contentType)
	resp := rec.Result()
	respBody := rec.Body.Bytes()
	if err := apitest.CheckResponse(resp, respBody, f); err != nil {
		t.Fatalf("fixture %s: %v (body: %.200s)", fixtureName, err, respBody)
	}
	if eng.audioCalls != 1 {
		t.Errorf("audio calls = %d, want 1", eng.audioCalls)
	}
	// The multipart body reached the engine byte-for-byte,
	// boundary and file part included — never decoded and
	// rebuilt.
	if got := eng.lastReq; string(got.Body) != string(body) {
		t.Error("multipart body was not forwarded byte-for-byte to the engine")
	}
	if !strings.Contains(eng.lastReq.ContentType, "multipart/form-data; boundary=") {
		t.Errorf("forwarded content-type = %q, want the caller's multipart boundary preserved", eng.lastReq.ContentType)
	}
}

func TestAudioTranscriptionsFixtureContract(t *testing.T) {
	checkMultipartFixture(t, "audio-transcriptions-200", "/v1/audio/transcriptions",
		func(srv *Server) func(http.ResponseWriter, *http.Request) { return srv.handleAudioTranscriptions })
}

func TestAudioSpeechToTextAliasFixtureContract(t *testing.T) {
	// The alias handler is exercised through a fresh server so
	// the operation mapping is the alias's own, not the
	// canonical handler's.
	f := apitest.LoadFixture(t, "m8.6", "audio-speech-to-text-200")
	eng := &audioFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	body, contentType := mockMultipart(t, "mock-stt-model")
	rec := doAudioReq(t, srv, srv.handleAudioSpeechToText, http.MethodPost, "/v1/audio/speech-to-text", body, contentType)
	if err := apitest.CheckResponse(rec.Result(), rec.Body.Bytes(), f); err != nil {
		t.Fatalf("fixture audio-speech-to-text-200: %v", err)
	}
	if eng.lastReq.Operation != router.AudioTranscriptions {
		t.Errorf("operation = %q, want transcriptions (alias must map to the canonical operation)", eng.lastReq.Operation)
	}
}

func TestAudioTranslationsFixtureContract(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.6", "audio-translations-200")
	eng := &audioFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	body, contentType := mockMultipart(t, "mock-stt-model")
	rec := doAudioReq(t, srv, srv.handleAudioTranslations, http.MethodPost, "/v1/audio/translations", body, contentType)
	if err := apitest.CheckResponse(rec.Result(), rec.Body.Bytes(), f); err != nil {
		t.Fatalf("fixture audio-translations-200: %v", err)
	}
	if eng.lastReq.Operation != router.AudioTranslations {
		t.Errorf("operation = %q, want translations", eng.lastReq.Operation)
	}
}

// Validation: missing model / input / file, and a non-multipart
// transcriptions body are all 400s with zero engine calls.
func TestAudioValidation(t *testing.T) {
	eng := &audioFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	cases := []struct {
		name        string
		handler     func(http.ResponseWriter, *http.Request)
		path        string
		body        []byte
		contentType string
	}{
		{"speech missing model", srv.handleAudioSpeech, "/v1/audio/speech", []byte(`{"input":"hi"}`), "application/json"},
		{"speech missing input", srv.handleAudioSpeech, "/v1/audio/speech", []byte(`{"model":"m"}`), "application/json"},
		{"speech malformed JSON", srv.handleAudioSpeech, "/v1/audio/speech", []byte(`{`), "application/json"},
		{"transcriptions JSON body, not multipart", srv.handleAudioTranscriptions, "/v1/audio/transcriptions", []byte(`{"model":"m"}`), "application/json"},
	}
	for _, tc := range cases {
		rec := doAudioReq(t, srv, tc.handler, http.MethodPost, tc.path, tc.body, tc.contentType)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body: %.200s)", tc.name, rec.Code, rec.Body.String())
		}
	}
	// Transcriptions with a model field but no file part.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", "mock-stt-model"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	rec := doAudioReq(t, srv, srv.handleAudioTranscriptions, http.MethodPost, "/v1/audio/transcriptions", buf.Bytes(), mw.FormDataContentType())
	if rec.Code != http.StatusBadRequest {
		t.Errorf("transcriptions missing file: status = %d, want 400 (body: %.200s)", rec.Code, rec.Body.String())
	}
	if eng.audioCalls != 0 {
		t.Errorf("audio calls = %d, want 0 (validation refuses before any dial)", eng.audioCalls)
	}
	// GET is not allowed on a POST-only route.
	rec = doAudioReq(t, srv, srv.handleAudioSpeech, http.MethodGet, "/v1/audio/speech", nil, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET speech: status = %d, want 405", rec.Code)
	}
}

// No capable provider: the engine's honest 400 names the
// endpoint; nothing is faked.
func TestAudioNoCapableProvider400(t *testing.T) {
	eng := &audioFakeEngine{fakeEngine: &fakeEngine{}, audioErr: &router.NoCapableProviderError{Endpoint: router.EndpointAudioSpeech, Model: "mock-tts-model"}}
	srv := newTestServer(t, eng)
	rec := doAudioReq(t, srv, srv.handleAudioSpeech, http.MethodPost, "/v1/audio/speech",
		[]byte(`{"model":"mock-tts-model","input":"Hello world"}`), "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %.200s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "audio_speech") {
		t.Errorf("body = %.200s, want it to name the audio_speech endpoint", rec.Body.String())
	}
}

// A server whose engine has no audio capability answers 503,
// never a faked clip or transcript.
func TestAudioEngineUnavailable503(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doAudioReq(t, srv, srv.handleAudioSpeech, http.MethodPost, "/v1/audio/speech",
		[]byte(`{"model":"mock-tts-model","input":"Hello world"}`), "application/json")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %.200s)", rec.Code, rec.Body.String())
	}
}

// TestAudioUploadOverCap413: a 26 MiB audio upload is refused
// with 413 before any byte is buffered, and the engine is never
// called. The body streams past the cap (repeatReader from the
// files tests) instead of allocating 26 MiB.
func TestAudioUploadOverCap413(t *testing.T) {
	eng := &audioFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	const boundary = "audio-cap-test-boundary"
	prefix := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"model\"\r\n\r\n" +
		"mock-stt-model\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"big.mp3\"\r\n" +
		"Content-Type: audio/mpeg\r\n\r\n"
	body := io.MultiReader(strings.NewReader(prefix), &repeatReader{b: 'a', left: audioMaxUploadBytes + 1<<20})
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	rec := httptest.NewRecorder()
	srv.handleAudioTranscriptions(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap upload: status %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "25 MiB") {
		t.Fatalf("over-cap upload body %q does not name the 25 MiB cap", rec.Body.String())
	}
	if eng.audioCalls != 0 {
		t.Errorf("audio calls = %d, want 0 (the cap refuses before any dial)", eng.audioCalls)
	}
}

// TestExtractAudioMultipartPrompt: the model, the prompt field
// and the file part are all found — including a prompt that
// comes AFTER the file part, so part order cannot hide it from
// the guardrail scan.
func TestExtractAudioMultipartPrompt(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "mock.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("MOCKAUDIOBYTES")); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("prompt", "a sailor's tale"); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("model", "whisper-1"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	model, prompt, filePresent, err := extractAudioMultipart(buf.Bytes(), mw.FormDataContentType())
	if err != nil {
		t.Fatalf("extractAudioMultipart: %v", err)
	}
	if model != "whisper-1" {
		t.Errorf("model = %q, want whisper-1", model)
	}
	if prompt != "a sailor's tale" {
		t.Errorf("prompt = %q, want \"a sailor's tale\"", prompt)
	}
	if !filePresent {
		t.Error("filePresent = false, want true")
	}
	// No prompt field at all: empty prompt, no error.
	body, ct := mockMultipart(t, "whisper-1")
	_, prompt, _, err = extractAudioMultipart(body, ct)
	if err != nil {
		t.Fatalf("extractAudioMultipart without prompt: %v", err)
	}
	if prompt != "" {
		t.Errorf("prompt = %q, want empty when no prompt part is present", prompt)
	}
}

// TestAudioTranscriptionPromptReachesEngine: a transcription
// prompt rides the engine request as Text (and therefore the
// bookkeeping request the guardrails scan), while the audio
// bytes still travel verbatim.
func TestAudioTranscriptionPromptReachesEngine(t *testing.T) {
	eng := &audioFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "mock.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("MOCKAUDIOBYTES")); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("model", "mock-stt-model"); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("prompt", "a sailor's tale"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	rec := doAudioReq(t, srv, srv.handleAudioTranscriptions, http.MethodPost, "/v1/audio/transcriptions", buf.Bytes(), mw.FormDataContentType())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if eng.audioCalls != 1 {
		t.Fatalf("audio calls = %d, want 1", eng.audioCalls)
	}
	if eng.lastReq.Text != "a sailor's tale" {
		t.Errorf("engine request Text = %q, want the prompt \"a sailor's tale\"", eng.lastReq.Text)
	}
	if string(eng.lastReq.Body) != buf.String() {
		t.Errorf("engine request body differs from the sent multipart bytes (verbatim rule)")
	}
}

// Toggle verification: the fixture's assertions must FAIL when
// the response is wrong — a 503 against the 200 fixture is a
// CheckResponse error, so the positive contract tests above do
// not pass vacuously.
func TestAudioFixtureWrongStatusFails(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.6", "audio-transcriptions-200")
	resp := &http.Response{StatusCode: http.StatusServiceUnavailable}
	if err := apitest.CheckResponse(resp, []byte(`{"error":{"message":"audio is not available"}}`), f); err == nil {
		t.Fatal("CheckResponse with a 503 against the 200 fixture = nil, want an error (toggle: the assertion must matter)")
	}
	// And the transcript text assertion must fail on a wrong
	// transcript, not just on a wrong status.
	resp200 := &http.Response{StatusCode: http.StatusOK}
	if err := apitest.CheckResponse(resp200, []byte(`{"text":"something else entirely"}`), f); err == nil {
		t.Fatal("CheckResponse with a wrong transcript text = nil, want an error (toggle: the JSON-path assertion must matter)")
	}
}
