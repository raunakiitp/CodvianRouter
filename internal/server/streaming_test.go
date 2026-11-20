package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func getStreaming(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, srv, http.MethodGet, "/dashboard/streaming", "", "")
}

// TestDashboardStreamingSample serves a small recorded run and
// checks the feed reports those exact numbers — median/p99
// before and after at each cadence, batching bounds, and the
// TCP_NODELAY verification flag. No value in the response is
// invented: every number traces to the file.
func TestDashboardStreamingSample(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithStreamingFile("testdata/streaming-sample.json"))
	rec := getStreaming(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res streamingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !res.Recorded {
		t.Fatalf("recorded = false, want true for a recorded run file")
	}
	if res.Date != "2026-11-18T02:00:00+05:30" {
		t.Errorf("date = %q, want the run's date", res.Date)
	}
	if res.RigCPUs != 4 || res.RigGoVersion == "" {
		t.Errorf("rig = %+v, want the run's rig facts", res)
	}
	if res.Batching.MaxTokens != 3 || res.Batching.MaxDelayMs != 8 {
		t.Errorf("batching = %+v, want 3 tokens / 8 ms", res.Batching)
	}
	if !res.TCPNoDelay.Verified {
		t.Errorf("tcp_nodelay.verified = false, want true for the recorded run")
	}
	if len(res.Scenarios) != 2 {
		t.Fatalf("scenarios = %d, want 2", len(res.Scenarios))
	}
	sc := res.Scenarios[0]
	if sc.CadenceMs != 10 || sc.Tokens != 200 {
		t.Errorf("scenario 0 = cadence %v tokens %d, want 10 ms / 200", sc.CadenceMs, sc.Tokens)
	}
	if sc.Before.MedianMs != 0.31 || sc.Before.P99Ms != 1.12 || sc.Before.Received != 200 {
		t.Errorf("scenario 0 before = %+v, want median 0.31 / p99 1.12 / 200 received", sc.Before)
	}
	if sc.After.MedianMs != 8.05 || sc.After.P99Ms != 8.41 || sc.After.Received != 200 {
		t.Errorf("scenario 0 after = %+v, want median 8.05 / p99 8.41 / 200 received", sc.After)
	}
	sc = res.Scenarios[1]
	if sc.CadenceMs != 1 || sc.Tokens != 200 {
		t.Errorf("scenario 1 = cadence %v tokens %d, want 1 ms / 200", sc.CadenceMs, sc.Tokens)
	}
	if sc.Before.MedianMs != 0.28 || sc.After.MedianMs != 1.02 {
		t.Errorf("scenario 1 medians = before %v after %v, want 0.28 / 1.02", sc.Before.MedianMs, sc.After.MedianMs)
	}
}

// TestDashboardStreamingRealRun serves the real measured
// 2026-11-17 SSE run (copied verbatim into testdata) and checks
// the feed reports those exact numbers. The 10 ms cadence shows
// the batching trade honestly: median 0.357 ms off vs 8.536 ms
// on (the 8 ms timer fires first at that cadence), while the
// p99 tightens (25.548 ms off vs 9.943 ms on — the off tail is
// shared-VM scheduler noise, loadavg 13.29 at run start). No
// value is scored MET/MISSED: there is no latency target for
// token delivery.
func TestDashboardStreamingRealRun(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithStreamingFile("testdata/streaming-2026-11-17.json"))
	rec := getStreaming(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res streamingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !res.Recorded {
		t.Fatalf("recorded = false, want true for a real streaming file")
	}
	if res.Date != "2026-11-17T20:50:00+00:00" {
		t.Errorf("date = %q, want the run's date", res.Date)
	}
	if res.Batching.MaxTokens != 3 || res.Batching.MaxDelayMs != 8 {
		t.Errorf("batching = %+v, want 3 tokens / 8 ms", res.Batching)
	}
	if !res.TCPNoDelay.Verified {
		t.Errorf("tcp_nodelay.verified = false, want true")
	}
	if len(res.Scenarios) != 2 {
		t.Fatalf("scenarios = %d, want 2", len(res.Scenarios))
	}
	sc := res.Scenarios[0]
	if sc.CadenceMs != 10 || sc.Tokens != 200 {
		t.Errorf("scenario 0 = cadence %v tokens %d, want 10 ms / 200", sc.CadenceMs, sc.Tokens)
	}
	if sc.Before.MedianMs != 0.357 || sc.Before.P99Ms != 25.548 || sc.Before.Received != 200 {
		t.Errorf("scenario 0 before = %+v, want median 0.357 / p99 25.548 / 200 received", sc.Before)
	}
	if sc.After.MedianMs != 8.536 || sc.After.P99Ms != 9.943 || sc.After.Received != 200 {
		t.Errorf("scenario 0 after = %+v, want median 8.536 / p99 9.943 / 200 received", sc.After)
	}
	sc = res.Scenarios[1]
	if sc.CadenceMs != 1 || sc.Tokens != 200 {
		t.Errorf("scenario 1 = cadence %v tokens %d, want 1 ms / 200", sc.CadenceMs, sc.Tokens)
	}
	if sc.Before.MedianMs != 0.211 || sc.Before.P99Ms != 3.16 {
		t.Errorf("scenario 1 before = %+v, want median 0.211 / p99 3.16", sc.Before)
	}
	if sc.After.MedianMs != 1.302 || sc.After.P99Ms != 2.853 {
		t.Errorf("scenario 1 after = %+v, want median 1.302 / p99 2.853", sc.After)
	}
}

func TestDashboardStreamingNoRun(t *testing.T) {
	// No file configured at all.
	srv := newTestServer(t, &fakeEngine{})
	rec := getStreaming(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var res streamingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if res.Recorded {
		t.Errorf("recorded = true, want false with no file configured")
	}
	if res.Note == "" {
		t.Errorf("note empty, want the how-to-record note")
	}

	// File configured but absent on disk: same honest empty state.
	srv = newTestServer(t, &fakeEngine{}, WithStreamingFile(filepath.Join(t.TempDir(), "streaming.json")))
	rec = getStreaming(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if res.Recorded {
		t.Errorf("recorded = true, want false with no file on disk")
	}
}

func TestDashboardStreamingCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "streaming.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithStreamingFile(path))
	rec := getStreaming(t, srv)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a corrupt file: %s", rec.Code, rec.Body.String())
	}
}
