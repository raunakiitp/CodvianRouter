// Media verify-first server tests (M8.20): auth, validation, the
// verbatim request/response relay per endpoint, the engine-error
// mapping, the 503 when the engine has no media capability, and
// one apitest fixture contract test per endpoint.
//
// The handlers are NOT registered in guard.go yet (the coordinator
// wires the routes) — so tests serve each handler directly behind
// the same requireAuth wrapper the route table will use, via
// mediaAuthed. A direct call without that wrapper would bypass
// auth, so no test ever calls a handler bare.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// mediaFakeEngine is a fakeEngine that also serves media
// passthrough, with a call counter, the last request it saw, and
// scripted results. LastTrace returns the canned trace the test
// sets (nil by default). delay sleeps before answering (0 =
// answer at once); blockUntilCtxDone waits for the request
// context to end instead of answering, returning ctx.Err() — the
// slow-upstream stand-in for the timeout tests.
type mediaFakeEngine struct {
	*fakeEngine
	calls             int32
	resp              *provider.MediaPassthroughResponse
	err               error
	lastReq           provider.MediaPassthroughRequest
	trace             *routetrace.RouteTrace
	delay             time.Duration
	blockUntilCtxDone bool
}

func (e *mediaFakeEngine) MediaPassthrough(ctx context.Context, req provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error) {
	atomic.AddInt32(&e.calls, 1)
	e.lastReq = req
	if e.blockUntilCtxDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if e.delay > 0 {
		select {
		case <-time.After(e.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	if e.resp != nil {
		return e.resp, nil
	}
	return &provider.MediaPassthroughResponse{Status: 200, Body: json.RawMessage(`{}`)}, nil
}

func (e *mediaFakeEngine) LastTrace() *routetrace.RouteTrace { return e.trace }

func (e *mediaFakeEngine) mediaCalls() int32 { return atomic.LoadInt32(&e.calls) }

// mediaAuthed serves one media handler behind the same requireAuth
// wrapper the guard route table will use for it.
func mediaAuthed(srv *Server, h http.HandlerFunc) http.Handler {
	return srv.requireAuth(h)
}

func doMediaReq(t *testing.T, handler http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" && method == http.MethodGet {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

const mediaTestKey = "test-key-123"

// mediaHandlers maps a name to the handler + the path + a valid
// body for it, so auth/validation tests can cover all five
// surfaces in one table.
func mediaHandlers(srv *Server) map[string]struct {
	h    http.Handler
	path string
	body string
} {
	return map[string]struct {
		h    http.Handler
		path string
		body string
	}{
		"search":       {mediaAuthed(srv, srv.handleSearch), "/v1/search", `{"query":"hello"}`},
		"ocr":          {mediaAuthed(srv, srv.handleOCR), "/v1/ocr", `{"model":"mistral-ocr-latest","document":{"type":"document_url","document_url":"https://example.com/d.pdf"}}`},
		"video-create": {mediaAuthed(srv, srv.handleVideoGenerations), "/v1/videos/generations", `{"model":"seedance2_5","promptText":"mist"}`},
		"video-task":   {mediaAuthed(srv, srv.handleVideoTask), "/v1/videos/task-1", ""},
		"music":        {mediaAuthed(srv, srv.handleMusic), "/v1/music/generations", `{"model":"music-3.0","prompt":"folk"}`},
	}
}

func TestMediaAuthRequired(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)
	for name, tc := range mediaHandlers(srv) {
		method := http.MethodPost
		if name == "video-task" {
			method = http.MethodGet
		}
		for _, key := range []string{"", "wrong-key"} {
			rec := doMediaReq(t, tc.h, method, tc.path, tc.body, key)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s key %q: status = %d, want 401", name, key, rec.Code)
			}
		}
	}
	if eng.mediaCalls() != 0 {
		t.Errorf("engine calls = %d, want 0 (auth first)", eng.mediaCalls())
	}
}

func TestMediaValidation(t *testing.T) {
	cases := []struct {
		name   string
		which  string
		method string
		path   string
		body   string
	}{
		{"search no query", "search", http.MethodPost, "/v1/search", `{}`},
		{"search empty query", "search", http.MethodPost, "/v1/search", `{"query":""}`},
		{"search six queries", "search", http.MethodPost, "/v1/search", `{"query":["1","2","3","4","5","6"]}`},
		{"search bad json", "search", http.MethodPost, "/v1/search", `{not json`},
		{"ocr no model", "ocr", http.MethodPost, "/v1/ocr", `{"document":{"type":"document_url"}}`},
		{"ocr no document", "ocr", http.MethodPost, "/v1/ocr", `{"model":"mistral-ocr-latest"}`},
		{"ocr document not object", "ocr", http.MethodPost, "/v1/ocr", `{"model":"m","document":"x"}`},
		{"video no model", "video-create", http.MethodPost, "/v1/videos/generations", `{"promptText":"mist"}`},
		{"music no model", "music", http.MethodPost, "/v1/music/generations", `{"prompt":"folk"}`},
		{"video task no id", "video-task", http.MethodGet, "/v1/videos/", ""},
		{"wrong method on search", "search", http.MethodGet, "/v1/search", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}}
			srv := newTestServer(t, eng)
			h := mediaHandlers(srv)[tc.which].h
			rec := doMediaReq(t, h, tc.method, tc.path, tc.body, mediaTestKey)
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 400 (or 405 for the wrong-method case)", rec.Code)
			}
			if tc.name == "wrong method on search" && rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rec.Code)
			}
			if eng.mediaCalls() != 0 {
				t.Errorf("engine calls = %d, want 0 (validation first)", eng.mediaCalls())
			}
		})
	}
}

// mediaHappy checks one happy path end to end: the engine saw the
// pinned provider + provider-native path + verbatim body, and the
// client got the upstream status + body verbatim with the endpoint
// header.
func TestMediaSearchHappyPathVerbatim(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}, resp: &provider.MediaPassthroughResponse{
		Status: 200,
		Body:   json.RawMessage(`{"id":"srch-1","results":[{"title":"T","url":"https://example.com","snippet":"S"}]}`),
	}}
	srv := newTestServer(t, eng)
	h := mediaAuthed(srv, srv.handleSearch)
	rec := doMediaReq(t, h, http.MethodPost, "/v1/search", `{"query":"hello","max_results":3,"future_field":true}`, mediaTestKey)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, `"future_field"`) && !strings.Contains(got, `"id":"srch-1"`) {
		t.Errorf("body = %s, want the upstream body verbatim", got)
	}
	if got := rec.Header().Get("X-Codvian-Endpoint"); got != "search" {
		t.Errorf("X-Codvian-Endpoint = %q, want search", got)
	}
	if eng.mediaCalls() != 1 {
		t.Fatalf("engine calls = %d, want 1", eng.mediaCalls())
	}
	req := eng.lastReq
	if req.ProviderID != "perplexity" || req.Path != "search" || req.Method != http.MethodPost {
		t.Errorf("engine req = %+v, want provider perplexity, path search, POST", req)
	}
	if req.Model != "perplexity" {
		t.Errorf("engine model = %q, want perplexity (search takes no model)", req.Model)
	}
	if !strings.Contains(string(req.Body), `"future_field":true`) {
		t.Errorf("engine body = %s, want the verbatim request body", req.Body)
	}
	if len(req.BookkeepingTexts) != 1 || req.BookkeepingTexts[0] != "hello" {
		t.Errorf("bookkeeping texts = %v, want [hello]", req.BookkeepingTexts)
	}
}

func TestMediaOCRHappyPath(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}, resp: &provider.MediaPassthroughResponse{
		Status: 200,
		Body:   json.RawMessage(`{"pages":[{"index":0,"markdown":"# Hi"}],"model":"mistral-ocr-latest"}`),
	}}
	srv := newTestServer(t, eng)
	rec := doMediaReq(t, mediaAuthed(srv, srv.handleOCR), http.MethodPost, "/v1/ocr", `{"model":"mistral-ocr-latest","document":{"type":"document_url","document_url":"https://example.com/d.pdf"}}`, mediaTestKey)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Endpoint"); got != "ocr" {
		t.Errorf("X-Codvian-Endpoint = %q, want ocr", got)
	}
	if req := eng.lastReq; req.ProviderID != "mistral" || req.Path != "ocr" || req.Model != "mistral-ocr-latest" {
		t.Errorf("engine req = %+v, want provider mistral, path ocr, model mistral-ocr-latest", req)
	}
	if len(eng.lastReq.BookkeepingTexts) != 0 {
		t.Errorf("bookkeeping texts = %v, want none (a document is not text)", eng.lastReq.BookkeepingTexts)
	}
}

// TestMediaOCRRequestBodyCap (F4): an OCR request body between
// the old 10 MiB default and the 50 MiB OCR cap (e.g. an ~11 MB
// inline base64 document) reaches the engine, and a body over
// 50 MiB is refused 400 before any provider is dialled. Bodies
// are built in memory; nothing is written to disk.
func TestMediaOCRRequestBodyCap(t *testing.T) {
	ocrBody := func(padding int) string {
		return `{"model":"mistral-ocr-latest","document":{"type":"document_url","document_url":"https://example.com/d.pdf"},"padding":"` + strings.Repeat("x", padding) + `"}`
	}
	under := ocrBody(11 << 20)
	if len(under) <= 10<<20 || len(under) >= 50<<20 {
		t.Fatalf("under-cap body = %d bytes, want between 10 MiB and 50 MiB", len(under))
	}
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}, resp: &provider.MediaPassthroughResponse{
		Status: 200,
		Body:   json.RawMessage(`{"pages":[],"model":"mistral-ocr-latest"}`),
	}}
	srv := newTestServer(t, eng)
	rec := doMediaReq(t, mediaAuthed(srv, srv.handleOCR), http.MethodPost, "/v1/ocr", under, mediaTestKey)
	if rec.Code != 200 {
		t.Errorf("11 MB OCR body: status = %d, want 200 (Mistral accepts documents up to 50 MB): %s", rec.Code, rec.Body.String())
	}
	if eng.mediaCalls() != 1 {
		t.Errorf("engine calls = %d, want 1", eng.mediaCalls())
	}

	over := ocrBody(51 << 20)
	if len(over) <= 50<<20 {
		t.Fatalf("over-cap body = %d bytes, want over 50 MiB", len(over))
	}
	eng2 := &mediaFakeEngine{fakeEngine: &fakeEngine{}}
	srv2 := newTestServer(t, eng2)
	rec = doMediaReq(t, mediaAuthed(srv2, srv2.handleOCR), http.MethodPost, "/v1/ocr", over, mediaTestKey)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("51 MB OCR body: status = %d, want 400", rec.Code)
	}
	if eng2.mediaCalls() != 0 {
		t.Errorf("engine calls = %d, want 0 (refused before dialling)", eng2.mediaCalls())
	}
}

func TestMediaVideoCreateHappyPathPreserves201(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}, resp: &provider.MediaPassthroughResponse{
		Status: 201,
		Body:   json.RawMessage(`{"id":"task-1","status":"PENDING"}`),
	}}
	srv := newTestServer(t, eng)
	rec := doMediaReq(t, mediaAuthed(srv, srv.handleVideoGenerations), http.MethodPost, "/v1/videos/generations", `{"model":"seedance2_5","promptText":"mist","ratio":"1280:720","duration":5}`, mediaTestKey)
	if rec.Code != 201 {
		t.Fatalf("status = %d, want the upstream 201 preserved: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Endpoint"); got != "video" {
		t.Errorf("X-Codvian-Endpoint = %q, want video", got)
	}
	req := eng.lastReq
	if req.ProviderID != "runwayml" || req.Path != "text_to_video" {
		t.Errorf("engine req = %+v, want provider runwayml, path text_to_video", req)
	}
	if req.ExtraHeaders["X-Runway-Version"] != "2024-11-06" {
		t.Errorf("extra headers = %v, want X-Runway-Version 2024-11-06", req.ExtraHeaders)
	}
}

func TestMediaVideoTaskHappyPath(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}, resp: &provider.MediaPassthroughResponse{
		Status: 200,
		Body:   json.RawMessage(`{"id":"task-1","status":"SUCCEEDED","output":["https://example.com/v.mp4"]}`),
	}}
	srv := newTestServer(t, eng)
	rec := doMediaReq(t, mediaAuthed(srv, srv.handleVideoTask), http.MethodGet, "/v1/videos/task-1", "", mediaTestKey)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	req := eng.lastReq
	if req.ProviderID != "runwayml" || req.Path != "tasks/task-1" || req.Method != http.MethodGet {
		t.Errorf("engine req = %+v, want provider runwayml, path tasks/task-1, GET", req)
	}
	if req.Body != nil {
		t.Errorf("engine body = %s, want nil on GET", req.Body)
	}
	if req.Model != "runwayml" {
		t.Errorf("engine model = %q, want runwayml (a task id names no model)", req.Model)
	}
}

func TestMediaMusicHappyPath(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}, resp: &provider.MediaPassthroughResponse{
		Status: 200,
		Body:   json.RawMessage(`{"data":{"audio":"00ff","status":2},"base_resp":{"status_code":0,"status_msg":"success"}}`),
	}}
	srv := newTestServer(t, eng)
	rec := doMediaReq(t, mediaAuthed(srv, srv.handleMusic), http.MethodPost, "/v1/music/generations", `{"model":"music-3.0","prompt":"folk","lyrics":"la la"}`, mediaTestKey)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Endpoint"); got != "music" {
		t.Errorf("X-Codvian-Endpoint = %q, want music", got)
	}
	req := eng.lastReq
	if req.ProviderID != "minimax" || req.Path != "music_generation" || req.Model != "music-3.0" {
		t.Errorf("engine req = %+v, want provider minimax, path music_generation, model music-3.0", req)
	}
	// The MiniMax 200 envelope is returned verbatim — including a
	// failure envelope, which the client must read from
	// base_resp.status_code, not from the HTTP status.
	if !strings.Contains(rec.Body.String(), `"status_code":0`) {
		t.Errorf("body = %s, want the MiniMax envelope verbatim", rec.Body.String())
	}
}

func TestMediaEngineUnavailable503(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doMediaReq(t, mediaAuthed(srv, srv.handleSearch), http.MethodPost, "/v1/search", `{"query":"hello"}`, mediaTestKey)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (engine has no media capability)", rec.Code)
	}
}

func TestMediaEngineErrors(t *testing.T) {
	ncp := &mediaFakeEngine{fakeEngine: &fakeEngine{}, err: &router.NoCapableProviderError{Endpoint: provider.EndpointSearch, Model: "perplexity"}}
	srv := newTestServer(t, ncp)
	rec := doMediaReq(t, mediaAuthed(srv, srv.handleSearch), http.MethodPost, "/v1/search", `{"query":"hello"}`, mediaTestKey)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no-capable-provider status = %d, want 400", rec.Code)
	}

	boom := &mediaFakeEngine{fakeEngine: &fakeEngine{}, err: errors.New("boom")}
	srv = newTestServer(t, boom)
	rec = doMediaReq(t, mediaAuthed(srv, srv.handleSearch), http.MethodPost, "/v1/search", `{"query":"hello"}`, mediaTestKey)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("generic engine error status = %d, want 502", rec.Code)
	}

	// A foreign engine answering a non-2xx "success" is a 502,
	// never a faked 200.
	badResp := &mediaFakeEngine{fakeEngine: &fakeEngine{}, resp: &provider.MediaPassthroughResponse{Status: 500, Body: json.RawMessage(`{"error":"x"}`)}}
	srv = newTestServer(t, badResp)
	rec = doMediaReq(t, mediaAuthed(srv, srv.handleSearch), http.MethodPost, "/v1/search", `{"query":"hello"}`, mediaTestKey)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("non-2xx engine response status = %d, want 502", rec.Code)
	}
}

// TestMediaVideoTaskUnknownID404 (F2): an upstream 404 for a
// video task id the upstream has never seen answers 404 — the
// client's id does not exist — not 502 "upstream down". The
// client polls by re-GETting; a 404 on any poll means no such
// task, and it must be distinguishable from an outage.
func TestMediaVideoTaskUnknownID404(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}, err: &provider.UpstreamError{StatusCode: http.StatusNotFound, Body: `{"error":"task not found"}`}}
	srv := newTestServer(t, eng)
	rec := doMediaReq(t, mediaAuthed(srv, srv.handleVideoTask), http.MethodGet, "/v1/videos/no-such-task", "", mediaTestKey)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown video task id: %s", rec.Code, rec.Body.String())
	}
	if eng.mediaCalls() != 1 {
		t.Errorf("engine calls = %d, want 1", eng.mediaCalls())
	}
}

// --- apitest fixture contract tests (one per endpoint) ---
//
// Each test loads its M8.20 fixture, builds a fake engine that
// replays the fixture's recorded upstream response (the mock
// upstream, in the harness's sense), and runs the handler behind
// requireAuth through apitest.Run, which asserts the status,
// required fields and JSON paths from the fixture. The handler
// under test must relay the recorded body verbatim for the
// assertions to hold.

func mediaFixtureEngine(t *testing.T, f apitest.Fixture) *mediaFakeEngine {
	t.Helper()
	raw, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded response: %v", err)
	}
	return &mediaFakeEngine{fakeEngine: &fakeEngine{}, resp: &provider.MediaPassthroughResponse{
		Status: f.RecordedResponse.Status,
		Body:   json.RawMessage(raw),
	}}
}

func TestMediaFixtureSearch200(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.20", "search-200")
	srv := newTestServer(t, mediaFixtureEngine(t, f))
	apitest.Run(t, mediaAuthed(srv, srv.handleSearch), f)
}

func TestMediaFixtureOCR200(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.20", "ocr-200")
	srv := newTestServer(t, mediaFixtureEngine(t, f))
	apitest.Run(t, mediaAuthed(srv, srv.handleOCR), f)
}

func TestMediaFixtureVideosGenerations200(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.20", "videos-generations-200")
	srv := newTestServer(t, mediaFixtureEngine(t, f))
	apitest.Run(t, mediaAuthed(srv, srv.handleVideoGenerations), f)
}

func TestMediaFixtureVideosGet200(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.20", "videos-get-200")
	srv := newTestServer(t, mediaFixtureEngine(t, f))
	apitest.Run(t, mediaAuthed(srv, srv.handleVideoTask), f)
}

func TestMediaFixtureMusic200(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.20", "music-200")
	srv := newTestServer(t, mediaFixtureEngine(t, f))
	apitest.Run(t, mediaAuthed(srv, srv.handleMusic), f)
}

// M8.20 F3: per-endpoint media timeouts. The resolver is the
// wiring under test: a music call must get the music timeout, not
// the server-wide 120 s, while every other surface keeps it.

// TestMediaTimeoutResolution proves the precedence table
// exactly, without waiting on any clock: programmatic override >
// routing.media_timeouts_ms config > music default (10 min) >
// server-wide timeout (120 s default, or WithTimeout). If the
// resolver is removed and serveMediaVerify goes back to s.timeout
// for every surface, the music rows fail at once.
func TestMediaTimeoutResolution(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}}
	cfg := func(ms map[string]int64) *config.Config {
		return &config.Config{Routing: &config.RoutingConfig{MediaTimeoutsMs: ms}}
	}
	cases := []struct {
		name string
		opts []Option
		spec mediaSpec
		want time.Duration
	}{
		{"music default is 10m", nil, mediaMusicSpec, 10 * time.Minute},
		{"search default is server 120s", nil, mediaSearchSpec, DefaultTimeout},
		{"ocr default is server 120s", nil, mediaOCRSpec, DefaultTimeout},
		{"video default is server 120s", nil, mediaVideoCreateSpec, DefaultTimeout},
		{"video task default is server 120s", nil, mediaVideoTaskSpec, DefaultTimeout},
		{"WithTimeout moves non-music only", []Option{WithTimeout(30 * time.Second)}, mediaSearchSpec, 30 * time.Second},
		{"WithTimeout leaves music at 10m", []Option{WithTimeout(30 * time.Second)}, mediaMusicSpec, 10 * time.Minute},
		{"config music 45s", []Option{WithConfig(cfg(map[string]int64{"music": 45000}))}, mediaMusicSpec, 45 * time.Second},
		{"config music does not move search", []Option{WithConfig(cfg(map[string]int64{"music": 45000}))}, mediaSearchSpec, DefaultTimeout},
		{"config search 5s", []Option{WithConfig(cfg(map[string]int64{"search": 5000}))}, mediaSearchSpec, 5 * time.Second},
		{"config 0 means default", []Option{WithConfig(cfg(map[string]int64{"music": 0}))}, mediaMusicSpec, 10 * time.Minute},
		{"override beats config", []Option{WithConfig(cfg(map[string]int64{"music": 45000})), WithMediaTimeouts(map[string]time.Duration{"music": 60 * time.Second})}, mediaMusicSpec, 60 * time.Second},
		{"override is per-endpoint", []Option{WithMediaTimeouts(map[string]time.Duration{"music": 50 * time.Millisecond})}, mediaSearchSpec, DefaultTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, eng, tc.opts...)
			if got := srv.mediaTimeout(tc.spec); got != tc.want {
				t.Errorf("mediaTimeout(%s) = %v, want %v", tc.spec.name, got, tc.want)
			}
		})
	}
}

// doMediaReqBounded serves one media request in a goroutine and
// reports whether it finished within limit. A handler stuck on
// the old 120 s bound fails the test in limit, not in 120 s.
func doMediaReqBounded(handler http.Handler, method, path, body, key string, limit time.Duration) (*httptest.ResponseRecorder, bool) {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		var rdr *strings.Reader
		if body == "" && method == http.MethodGet {
			rdr = strings.NewReader("")
		} else {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		done <- rec
	}()
	select {
	case rec := <-done:
		return rec, true
	case <-time.After(limit):
		return nil, false
	}
}

// TestMediaMusicTimeoutUsesMusicTimeout proves the wiring end to
// end with a short music timeout: an upstream that never answers
// is cut off at ~50 ms with a 504 — not at the 120 s server
// default. (The 10-minute default itself is proven exactly, and
// without waiting, by TestMediaTimeoutResolution.)
func TestMediaMusicTimeoutUsesMusicTimeout(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}, blockUntilCtxDone: true}
	srv := newTestServer(t, eng, WithMediaTimeouts(map[string]time.Duration{"music": 50 * time.Millisecond}))
	h := mediaAuthed(srv, srv.handleMusic)
	start := time.Now()
	rec, ok := doMediaReqBounded(h, http.MethodPost, "/v1/music/generations", `{"model":"music-3.0","prompt":"folk"}`, mediaTestKey, 10*time.Second)
	if !ok {
		t.Fatalf("music request did not finish within 10s: the music timeout did not fire (still on the 120s bound?)")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("music request took %v, want it cut off near the 50ms music timeout", elapsed)
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504 (music timeout)", rec.Code)
	}
	if eng.mediaCalls() != 1 {
		t.Errorf("engine calls = %d, want 1 (the call started, then timed out)", eng.mediaCalls())
	}
}

// TestMediaNonMusicTimeoutUnaffectedByMusicTimeout proves the
// timeout is per-endpoint, not global: with the music timeout at
// 50 ms, a search whose upstream answers after 200 ms still
// succeeds (search keeps the 120 s server timeout), while the
// same slow upstream on music is cut off with a 504.
func TestMediaNonMusicTimeoutUnaffectedByMusicTimeout(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}, delay: 200 * time.Millisecond}
	srv := newTestServer(t, eng, WithMediaTimeouts(map[string]time.Duration{"music": 50 * time.Millisecond}))

	rec, ok := doMediaReqBounded(mediaAuthed(srv, srv.handleSearch), http.MethodPost, "/v1/search", `{"query":"hello"}`, mediaTestKey, 10*time.Second)
	if !ok {
		t.Fatalf("search request did not finish within 10s")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("search status = %d, want 200 (search must not inherit the 50ms music timeout): %s", rec.Code, rec.Body.String())
	}

	rec, ok = doMediaReqBounded(mediaAuthed(srv, srv.handleMusic), http.MethodPost, "/v1/music/generations", `{"model":"music-3.0","prompt":"folk"}`, mediaTestKey, 10*time.Second)
	if !ok {
		t.Fatalf("music request did not finish within 10s")
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("music status = %d, want 504 (50ms music timeout)", rec.Code)
	}
}

// TestMediaMusicTimeoutFromConfig proves the config value, not
// just the programmatic override, drives the handler: with
// routing.media_timeouts_ms["music"] = 50, a never-answering
// music upstream is cut off with a 504 in well under 10 s.
func TestMediaMusicTimeoutFromConfig(t *testing.T) {
	eng := &mediaFakeEngine{fakeEngine: &fakeEngine{}, blockUntilCtxDone: true}
	cfg := &config.Config{Routing: &config.RoutingConfig{MediaTimeoutsMs: map[string]int64{"music": 50}}}
	// WithConfig replaces the key verifier with the config-backed
	// one (which holds no gateway keys here), so re-apply the
	// static test key after it; the cfg itself stays wired.
	srv := newTestServer(t, eng, WithConfig(cfg), WithStaticKeys(mediaTestKey))
	rec, ok := doMediaReqBounded(mediaAuthed(srv, srv.handleMusic), http.MethodPost, "/v1/music/generations", `{"model":"music-3.0","prompt":"folk"}`, mediaTestKey, 10*time.Second)
	if !ok {
		t.Fatalf("music request did not finish within 10s: the configured music timeout did not fire")
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504 (configured 50ms music timeout)", rec.Code)
	}
}
