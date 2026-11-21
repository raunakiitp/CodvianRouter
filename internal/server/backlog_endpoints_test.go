package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBacklogEndpoints_MultimodalEmbeddings(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)

	// Multimodal input with text and image part
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
		t.Errorf("embedCalls = %d, want 1", eng.embedCalls)
	}
}

func TestBacklogEndpoints_Restart(t *testing.T) {
	restarted := false
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng,
		WithRestartHook(func() {
			restarted = true
		}),
	)

	req := httptest.NewRequest(http.MethodPost, "/admin/restart", nil)
	req.RemoteAddr = "127.0.0.1:45678" // loopback
	req.Header.Set("Authorization", "Bearer test-key-123")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body: %s", rec.Code, rec.Body.String())
	}

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["status"] != "restarting" {
		t.Errorf("got status %v, want restarting", res["status"])
	}

	time.Sleep(50 * time.Millisecond)
	if !restarted {
		t.Errorf("restartHook was not called")
	}
}

func TestBacklogEndpoints_AudioVoices(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)

	for _, endpoint := range []string{"/v1/audio/voices", "/v1/voices"} {
		req := httptest.NewRequest(http.MethodGet, endpoint, nil)
		req.Header.Set("Authorization", "Bearer test-key-123")
		rec := httptest.NewRecorder()

		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200, body: %s", endpoint, rec.Code, rec.Body.String())
		}

		var res struct {
			Voices []AudioVoice `json:"voices"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if len(res.Voices) < 6 {
			t.Errorf("%s returned %d voices, want at least 6", endpoint, len(res.Voices))
		}
	}
}

func TestBacklogEndpoints_VideoBridgeDrilldown(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)

	// GET test
	reqGet := httptest.NewRequest(http.MethodGet, "/v1/video-bridge/drilldown?task_id=task_123", nil)
	reqGet.Header.Set("Authorization", "Bearer test-key-123")
	recGet := httptest.NewRecorder()
	srv.ServeHTTP(recGet, reqGet)

	if recGet.Code != http.StatusOK {
		t.Fatalf("GET /v1/video-bridge/drilldown status = %d, want 200, body: %s", recGet.Code, recGet.Body.String())
	}

	// POST test
	reqPost := httptest.NewRequest(http.MethodPost, "/v1/video-bridge/drilldown", bytes.NewBufferString(`{"task_id":"task_456"}`))
	reqPost.Header.Set("Authorization", "Bearer test-key-123")
	reqPost.Header.Set("Content-Type", "application/json")
	recPost := httptest.NewRecorder()
	srv.ServeHTTP(recPost, reqPost)

	if recPost.Code != http.StatusOK {
		t.Fatalf("POST /v1/video-bridge/drilldown status = %d, want 200, body: %s", recPost.Code, recPost.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(recPost.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["task_id"] != "task_456" || res["status"] != "completed" {
		t.Errorf("unexpected drilldown response: %+v", res)
	}
}

func TestBacklogEndpoints_SystemOne(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)

	reqBody := `{"model":"gpt-4o","query":"Calculate total latency"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["object"] != "systemone.completion" || res["status"] != "success" {
		t.Errorf("unexpected systemone response: %+v", res)
	}
}

func TestBacklogEndpoints_WebFetch_SSRFProtection(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	srv := newTestServer(t, eng)

	// Target localhost -> must be blocked with 403 Forbidden
	reqBody := `{"url":"http://127.0.0.1:8080/secret"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/web/fetch", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (SSRF blocked), body: %s", rec.Code, rec.Body.String())
	}
}
