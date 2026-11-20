package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func getMemstats(t *testing.T, srv *Server, query string) *http.Response {
	t.Helper()
	// doReq goes through the mux, so the guard-tier lookup for the
	// new route runs exactly as in production.
	rec := doReq(t, srv, http.MethodGet, "/dashboard/memstats"+query, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	return rec.Result()
}

// TestDashboardMemstatsLive checks the JSON snapshot reports live,
// self-consistent values from this test process: heap sys covers heap
// alloc, RSS is available on Linux, and the in-flight field honestly
// reports unavailable when the server has no recorder.
func TestDashboardMemstatsLive(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	resp := getMemstats(t, srv, "")
	defer resp.Body.Close()
	var res memstatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.GoVersion == "" {
		t.Error("go_version empty")
	}
	if res.HeapAllocMiB <= 0 {
		t.Errorf("heap_alloc_mib = %v, want > 0 (this process has a heap)", res.HeapAllocMiB)
	}
	if res.HeapSysMiB < res.HeapAllocMiB {
		t.Errorf("heap_sys_mib %v < heap_alloc_mib %v", res.HeapSysMiB, res.HeapAllocMiB)
	}
	if res.HeapInuseMiB < res.HeapAllocMiB {
		t.Errorf("heap_inuse_mib %v < heap_alloc_mib %v", res.HeapInuseMiB, res.HeapAllocMiB)
	}
	if res.Goroutines < 1 {
		t.Errorf("goroutines = %d, want >= 1", res.Goroutines)
	}
	// RSS comes from /proc/self/status, which does not exist
	// on Windows (readRSSKiB is Linux-only by design and the
	// feed honestly reports unavailable there), so the RSS
	// assertions are Linux-only — the heap/goroutine checks
	// above still validate the live snapshot on every OS.
	if runtime.GOOS != "windows" {
		if !res.RSSAvailable || res.RSSMiB <= 0 {
			t.Errorf("rss = %v mib available=%v, want a real Linux VmRSS", res.RSSMiB, res.RSSAvailable)
		}
		if res.RSSMiB < res.HeapAllocMiB {
			t.Errorf("rss_mib %v < heap_alloc_mib %v: RSS must cover the heap", res.RSSMiB, res.HeapAllocMiB)
		}
	}
	if res.InFlightAvailable {
		t.Error("in_flight_available = true, want false: this test server has no recorder")
	}
	if res.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds = %v, want >= 0", res.UptimeSeconds)
	}
	// GOGC is an environment read, never a debug.SetGCPercent
	// call: SetGCPercent(-1) DISABLES the GC in Go 1.27 (it does
	// not read the current value), and an earlier version of this
	// feed made exactly that mistake — every poll turned the GC
	// off, the heap grew to 1.1 GiB across two load waves with
	// NumGC frozen at 1, and the whole profiling run had to be
	// discarded (docs/memory-profile-2026-11-18.md). The string
	// type pins the env-read shape: reverting to the int +
	// SetGCPercent form fails this assertion's compile.
	if res.GOGC != "default" {
		t.Errorf("gogc = %q, want \"default\" (GOGC unset in tests)", res.GOGC)
	}
}

// TestDashboardMemstatsWithRecorder checks the in-flight count comes
// from the wired recorder: IncInFlight before the request shows up
// in the feed, DecInFlight removes it again.
func TestDashboardMemstatsWithRecorder(t *testing.T) {
	rec := metrics.NewRecorder()
	rec.IncInFlight()
	rec.IncInFlight()
	srv := newTestServer(t, &fakeEngine{}, WithMetrics(rec))
	resp := getMemstats(t, srv, "")
	defer resp.Body.Close()
	var res memstatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.InFlightAvailable || res.InFlight != 2 {
		t.Errorf("in_flight = %d available=%v, want 2/true", res.InFlight, res.InFlightAvailable)
	}
	rec.DecInFlight()
	rec.DecInFlight()
	resp2 := getMemstats(t, srv, "")
	defer resp2.Body.Close()
	var res2 memstatsResponse
	if err := json.NewDecoder(resp2.Body).Decode(&res2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res2.InFlight != 0 {
		t.Errorf("in_flight after DecInFlight x2 = %d, want 0", res2.InFlight)
	}
}

// TestDashboardMemstatsHeapProfile checks ?profile=heap serves a
// non-trivial binary pprof profile (gzip magic, real size) with the
// documented content type.
func TestDashboardMemstatsHeapProfile(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/memstats?profile=heap", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	body := rec.Body.Bytes()
	if len(body) < 1024 {
		t.Fatalf("heap profile = %d bytes, want a real profile (>1 KiB)", len(body))
	}
	if body[0] != 0x1f || body[1] != 0x8b {
		t.Errorf("heap profile does not start with the gzip magic: %x", body[:2])
	}
}

// TestDashboardMemstatsGoroutineProfile is the same shape check for
// ?profile=goroutine.
func TestDashboardMemstatsGoroutineProfile(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/memstats?profile=goroutine", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()
	if len(body) < 512 {
		t.Fatalf("goroutine profile = %d bytes, want a real profile", len(body))
	}
	if body[0] != 0x1f || body[1] != 0x8b {
		t.Errorf("goroutine profile does not start with the gzip magic: %x", body[:2])
	}
}

// TestDashboardMemstatsBadProfile checks an unknown profile value is
// a 400, not a heap dump by another name.
func TestDashboardMemstatsBadProfile(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/memstats?profile=cpu", "", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for ?profile=cpu: %s", rec.Code, rec.Body.String())
	}
}

// TestDashboardMemstatsBoundedAcrossWaves is the regression test for
// CHECKLIST 235: two waves of real chat completions through the test
// server, GC forced and heap read back through the memstats feed
// itself after each wave. A leak or unbounded per-request retention
// shows up as wave-2 settled heap far above wave 1; legitimate
// in-flight buffering drains between waves, so the bound is generous
// (1.5x + 10 MiB) and the test only fails on real growth. The 64 KiB
// response body per request churns ~32 MiB per wave, so the GC must
// also run between waves — NumGC advancing proves the collector is
// alive, the exact failure mode of the SetGCPercent(-1) trap this
// feed once had (every poll disabled GC; see the GOGC comment in
// memstats.go and docs/memory-profile-2026-11-18.md).
//
// Scope: the server layer with a fake engine — the full-engine
// two-wave check on the real binary is the profiling run recorded
// in docs/memory-profile-2026-11-18.md, not this test.
func TestDashboardMemstatsBoundedAcrossWaves(t *testing.T) {
	rec := metrics.NewRecorder()
	eng := &fakeEngine{chatResp: provider.ChatResponse{Content: strings.Repeat("x", 64*1024), Model: "model-a", FinishReason: "stop"}}
	srv := newTestServer(t, eng, WithModels("model-a"), WithMetrics(rec))

	driveWave := func(wave int) {
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for i := 0; i < 63; i++ {
					body := fmt.Sprintf(`{"model":"model-a","messages":[{"role":"user","content":"wave %d worker %d request %d"}]}`, wave, worker, i)
					r := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
					if r.Code != http.StatusOK {
						t.Errorf("wave %d: chat status = %d, want 200: %s", wave, r.Code, r.Body.String())
						return
					}
				}
			}(w)
		}
		wg.Wait()
	}

	settled := func() memstatsResponse {
		runtime.GC()
		runtime.GC()
		resp := getMemstats(t, srv, "")
		defer resp.Body.Close()
		var res memstatsResponse
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			t.Fatalf("decode feed: %v", err)
		}
		return res
	}

	driveWave(1)
	w1 := settled()
	driveWave(2)
	w2 := settled()

	if w2.InFlight != 0 {
		t.Errorf("in_flight after wave 2 settled = %d, want 0 (requests drained)", w2.InFlight)
	}
	if w2.NumGC <= w1.NumGC {
		t.Errorf("num_gc wave1=%d wave2=%d: GC did not run across wave 2 — the collector may be disabled", w1.NumGC, w2.NumGC)
	}
	limit := w1.HeapAllocMiB*1.5 + 10
	if w2.HeapAllocMiB > limit {
		t.Errorf("settled heap wave1=%.1f MiB wave2=%.1f MiB, want wave2 <= %.1f MiB (1.5x + 10): heap grows wave over wave — leak or unbounded retention", w1.HeapAllocMiB, w2.HeapAllocMiB, limit)
	}
	t.Logf("settled heap: wave1=%.1f MiB (num_gc=%d) wave2=%.1f MiB (num_gc=%d), in_flight=%d", w1.HeapAllocMiB, w1.NumGC, w2.HeapAllocMiB, w2.NumGC, w2.InFlight)
}

// TestDashboardMemstatsMethodNotAllowed checks POST is refused.
func TestDashboardMemstatsMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodPost, "/dashboard/memstats", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 for POST: %s", rec.Code, rec.Body.String())
	}
}
