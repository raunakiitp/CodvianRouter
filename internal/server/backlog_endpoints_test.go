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

	"github.com/raunakiitp/codvian-router/internal/release"
)

func TestBacklogEndpoints_MultimodalEmbeddings(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)

	reqBody := `{
		"model": "text-embedding-3-small",
		"input": [
			{"text": "A photo of a sunrise over the mountains", "image_url": "https://example.com/sunrise.jpg"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/multimodal-embeddings", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Model string `json:"model"`
		Data  []struct {
			Index int `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed unmarshaling response: %v", err)
	}
	if len(resp.Data) != 1 || resp.Model != "text-embedding-3-small" {
		t.Errorf("unexpected response shape: %+v", resp)
	}
	if eng.embedCalls != 1 {
		t.Errorf("embedCalls = %d, want 1 (the request must reach the engine)", eng.embedCalls)
	}
}

// TestRestartRunsRestartHookOnly proves POST /admin/restart answers
// 202 and runs the RESTART hook from loopback with a key, and that
// the shutdown hook is not used in its place.
func TestRestartRunsRestartHookOnly(t *testing.T) {
	var restarted, shutdown atomic.Int32
	srv := newTestServer(t, &fakeEngine{},
		WithRestartHook(func() { restarted.Add(1) }),
		WithShutdownHook(func() { shutdown.Add(1) }))

	rec := doReqPeer(t, srv, "POST", "/admin/restart", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body: %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["status"] != "restarting" {
		t.Errorf("status field = %v, want restarting", res["status"])
	}
	deadline := time.Now().Add(2 * time.Second)
	for restarted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if restarted.Load() != 1 {
		t.Errorf("restart hook calls = %d, want 1", restarted.Load())
	}
	if shutdown.Load() != 0 {
		t.Errorf("shutdown hook calls = %d, want 0", shutdown.Load())
	}
}

// TestRestartWithoutHookIs503 proves a server with no restart hook
// refuses honestly instead of shutting down while saying
// "restarting".
func TestRestartWithoutHookIs503(t *testing.T) {
	var shutdown atomic.Int32
	srv := newTestServer(t, &fakeEngine{}, WithShutdownHook(func() { shutdown.Add(1) }))
	rec := doReqPeer(t, srv, "POST", "/admin/restart", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body: %s", rec.Code, rec.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	if shutdown.Load() != 0 {
		t.Errorf("shutdown hook ran %d times for a restart request", shutdown.Load())
	}
}

// TestRestartRemoteRefused proves the LOCAL_ONLY boundary: a
// non-loopback peer is refused 403 with or without a key, and
// spoofed forwarding headers do not help. The hook never runs.
func TestRestartRemoteRefused(t *testing.T) {
	var restarted atomic.Int32
	srv := newTestServer(t, &fakeEngine{}, WithRestartHook(func() { restarted.Add(1) }))
	for _, key := range []string{"test-key-123", ""} {
		rec := doReqPeer(t, srv, "POST", "/admin/restart", "", key, nonLoopbackPeer)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("remote restart (key=%q): status %d, want 403", key, rec.Code)
		}
	}
	req := httptest.NewRequest("POST", "/admin/restart", nil)
	req.RemoteAddr = nonLoopbackPeer
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote restart with spoofed XFF/Host: status %d, want 403", rec.Code)
	}
	// Loopback without a key still needs auth.
	if rec := doReqPeer(t, srv, "POST", "/admin/restart", "", "", loopbackPeer); rec.Code != http.StatusUnauthorized {
		t.Fatalf("loopback restart without key: status %d, want 401", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if restarted.Load() != 0 {
		t.Fatalf("restart hook ran %d times for refused requests", restarted.Load())
	}
}

// TestRemovedFakeRoutesAre404 proves the fabricated endpoints from
// 17c27f4 (SystemOne canned reply, fixed video "drilldown" metrics,
// static voice list) are no longer served.
func TestRemovedFakeRoutesAre404(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	cases := []struct{ method, path, body string }{
		{"POST", "/v1/systemone", `{"query":"x"}`},
		{"GET", "/v1/video-bridge/drilldown?task_id=t1", ""},
		{"POST", "/v1/video-bridge/drilldown", `{"task_id":"t1"}`},
		{"GET", "/v1/audio/voices", ""},
		{"GET", "/v1/voices", ""},
	}
	for _, c := range cases {
		rec := doReqPeer(t, srv, c.method, c.path, c.body, "test-key-123", loopbackPeer)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status %d, want 404 (route removed), body %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	for _, r := range guardRouteTable {
		switch r.Path {
		case "/v1/systemone", "/v1/video-bridge/drilldown", "/v1/audio/voices", "/v1/voices":
			t.Errorf("guard table still lists removed route %s %s", r.Method, r.Path)
		}
	}
}

// TestTextToSpeechVoicePathReachesEngine proves the {voiceId} route
// is real: the path voice is written into the forwarded body.
func TestTextToSpeechVoicePathReachesEngine(t *testing.T) {
	eng := &audioFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)

	rec := doReqPeer(t, srv, "POST", "/v1/audio/text-to-speech/nova", `{"model":"tts-1","input":"hello"}`, "test-key-123", loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&eng.audioCalls) != 1 {
		t.Fatalf("audio engine calls = %d, want 1", eng.audioCalls)
	}
	var fwd map[string]any
	if err := json.Unmarshal(eng.lastReq.Body, &fwd); err != nil {
		t.Fatalf("forwarded body is not JSON: %v", err)
	}
	if fwd["voice"] != "nova" || fwd["input"] != "hello" || fwd["model"] != "tts-1" {
		t.Errorf("forwarded body = %v, want voice=nova with model/input kept", fwd)
	}

	// A conflicting body voice is refused, never silently overridden.
	rec = doReqPeer(t, srv, "POST", "/v1/audio/text-to-speech/nova", `{"model":"tts-1","input":"hi","voice":"onyx"}`, "test-key-123", loopbackPeer)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("conflicting voice: status %d, want 400", rec.Code)
	}
	// A malformed voice id is refused.
	rec = doReqPeer(t, srv, "POST", "/v1/audio/text-to-speech/bad%20voice", `{"model":"tts-1","input":"hi"}`, "test-key-123", loopbackPeer)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad voice id: status %d, want 400", rec.Code)
	}
	if atomic.LoadInt32(&eng.audioCalls) != 1 {
		t.Errorf("refused requests reached the engine: calls = %d, want 1", eng.audioCalls)
	}
}

// TestUnavailableRoutesHonest proves every documented unavailable
// route answers 501 + code feature_unavailable (never 2xx, never a
// stored-and-ignored setting), and that its guard-table description
// says so. Tier 1 entries are still refused 403 from a remote peer.
func TestUnavailableRoutesHonest(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	if len(unavailableRoutes) == 0 {
		t.Fatal("unavailableRoutes is empty")
	}
	tierOf := map[string]GuardTier{}
	descOf := map[string]string{}
	for _, r := range guardRouteTable {
		tierOf[r.Method+" "+r.Path] = r.Tier
		descOf[r.Method+" "+r.Path] = r.Description
	}
	for key := range unavailableRoutes {
		method, path, _ := strings.Cut(key, " ")
		reqPath := strings.NewReplacer("{id}", "x1", "{model}", "m1").Replace(path)
		rec := doReqPeer(t, srv, method, reqPath, `{}`, "test-key-123", loopbackPeer)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s: status %d, want 501, body %s", key, rec.Code, rec.Body.String())
			continue
		}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Available *bool `json:"available"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body not JSON: %v", key, err)
		}
		if body.Error.Code != "feature_unavailable" || body.Error.Message == "" || body.Available == nil || *body.Available {
			t.Errorf("%s: body %s, want code feature_unavailable, a reason and available=false", key, rec.Body.String())
		}
		if !strings.HasPrefix(descOf[key], "Unavailable: answers 501 feature_unavailable") {
			t.Errorf("%s: guard description %q does not say it is unavailable", key, descOf[key])
		}
		if tierOf[key] == TierLocalOnly {
			if rec := doReqPeer(t, srv, method, reqPath, `{}`, "test-key-123", nonLoopbackPeer); rec.Code != http.StatusForbidden {
				t.Errorf("%s from remote: status %d, want 403", key, rec.Code)
			}
		}
	}
}

func TestAdminUpdateCheck(t *testing.T) {
	t.Run("no checker is 503", func(t *testing.T) {
		srv := newTestServer(t, &fakeEngine{})
		if rec := doReqPeer(t, srv, "POST", "/admin/update", "", "test-key-123", loopbackPeer); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", rec.Code)
		}
	})
	t.Run("newer release", func(t *testing.T) {
		var calls atomic.Int32
		srv := newTestServer(t, &fakeEngine{}, WithUpdateCheck("0.1.0", func(ctx context.Context) (string, error) {
			calls.Add(1)
			return "v0.2.0", nil
		}))
		rec := doReqPeer(t, srv, "POST", "/admin/update", "", "test-key-123", loopbackPeer)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		if res["update_available"] != true || res["latest"] != "v0.2.0" || res["current"] != "v0.1.0" || res["installed"] != false {
			t.Errorf("body = %v", res)
		}
		if calls.Load() != 1 {
			t.Errorf("checker calls = %d, want 1", calls.Load())
		}
	})
	t.Run("up to date", func(t *testing.T) {
		srv := newTestServer(t, &fakeEngine{}, WithUpdateCheck("v0.2.0", func(ctx context.Context) (string, error) { return "v0.2.0", nil }))
		rec := doReqPeer(t, srv, "POST", "/admin/update", "", "test-key-123", loopbackPeer)
		var res map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		if rec.Code != http.StatusOK || res["update_available"] != false {
			t.Errorf("status %d body %v, want 200 update_available=false", rec.Code, res)
		}
	})
	t.Run("checker error is 502", func(t *testing.T) {
		srv := newTestServer(t, &fakeEngine{}, WithUpdateCheck("0.1.0", func(ctx context.Context) (string, error) {
			return "", errors.New("GitHub API: HTTP 404")
		}))
		if rec := doReqPeer(t, srv, "POST", "/admin/update", "", "test-key-123", loopbackPeer); rec.Code != http.StatusBadGateway {
			t.Errorf("status %d, want 502", rec.Code)
		}
	})
	check := func(t *testing.T, err error) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		srv := newTestServer(t, &fakeEngine{}, WithUpdateCheck("0.1.0", func(ctx context.Context) (string, error) { return "", err }))
		rec := doReqPeer(t, srv, "POST", "/admin/update", "", "test-key-123", loopbackPeer)
		var res map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		return rec, res
	}
	t.Run("no published release", func(t *testing.T) {
		rec, res := check(t, &release.LookupError{Kind: release.KindNoRelease, Status: 404, Err: errors.New("none")})
		if rec.Code != http.StatusOK || res["latest"] != nil || res["release_available"] != false ||
			res["update_available"] != false || res["status"] != "no_published_release" {
			t.Fatalf("status %d body %v, want 200 with latest=null, release_available=false, update_available=false", rec.Code, res)
		}
		if _, ok := res["release_url"]; ok {
			t.Errorf("no-release answer invented a release_url: %v", res)
		}
	})
	t.Run("failures are not no-release", func(t *testing.T) {
		for _, c := range []struct {
			kind release.LookupKind
			code int
		}{
			{release.KindRateLimited, http.StatusServiceUnavailable},
			{release.KindTimeout, http.StatusGatewayTimeout},
			{release.KindUpstream, http.StatusBadGateway},
			{release.KindMalformed, http.StatusBadGateway},
			{release.KindNetwork, http.StatusBadGateway},
			{release.KindAuth, http.StatusBadGateway},
			{release.KindNotFound, http.StatusBadGateway},
		} {
			rec, res := check(t, &release.LookupError{Kind: c.kind, RetryAfter: 30 * time.Second, Err: errors.New("x")})
			e, _ := res["error"].(map[string]any)
			if rec.Code != c.code || e["code"] != string(c.kind) || res["status"] == "no_published_release" {
				t.Errorf("%s: status %d body %v, want %d code=%s", c.kind, rec.Code, res, c.code, c.kind)
			}
			if c.kind == release.KindRateLimited && rec.Header().Get("Retry-After") != "30" {
				t.Errorf("rate limited: Retry-After = %q, want 30", rec.Header().Get("Retry-After"))
			}
		}
	})
	t.Run("remote refused", func(t *testing.T) {
		srv := newTestServer(t, &fakeEngine{}, WithUpdateCheck("0.1.0", func(ctx context.Context) (string, error) { return "v9", nil }))
		if rec := doReqPeer(t, srv, "POST", "/admin/update", "", "test-key-123", nonLoopbackPeer); rec.Code != http.StatusForbidden {
			t.Errorf("status %d, want 403", rec.Code)
		}
	})
}
