// Memory feed (GET /dashboard/memstats) + live pprof snapshots.
//
// The dashboard's Benchmark view shows a "Live memory (this process)"
// card fed by this endpoint: current Go heap figures from
// runtime.MemStats, process RSS from /proc/self/status, live goroutine
// count, in-flight requests from the metrics recorder, and GC counters.
// Every value is read live from this process at request time; nothing
// is estimated and nothing is carried over from a recorded bench run.
// The recorded run's idle/peak RSS still comes from GET
// /dashboard/benchmark — this feed is the live counterpart, so the
// operator can watch memory while load is running instead of only
// after the run.
//
// ?profile=heap serves a binary pprof heap profile of this process at
// request time (runtime/pprof.WriteHeapProfile — note: taking a heap
// profile forces a GC first, so the snapshot is post-GC by
// construction); ?profile=goroutine serves the binary goroutine
// profile. Both are operator diagnostics on the localhost-bound
// management surface, same tier as /dashboard/benchmark; an unknown
// profile value is a 400. The profiles exist so a memory question can
// be answered against the running router instead of a guess —
// CHECKLIST 235-236 profiled the attempt-3 peak-RSS signal (98.8 MiB
// under load vs 38.4 MiB idle) through exactly this endpoint, see
// docs/memory-profile-2026-11-18.md.
package server

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"
)

// memstatsResponse is the JSON shape of GET /dashboard/memstats.
// Sizes are MiB (1024*1024 bytes); counts are since process start.
type memstatsResponse struct {
	GoVersion string `json:"go_version"`
	// UptimeSeconds is seconds since this server process started.
	UptimeSeconds float64 `json:"uptime_seconds"`
	// HeapAllocMiB is live heap objects (MemStats.HeapAlloc).
	HeapAllocMiB float64 `json:"heap_alloc_mib"`
	// HeapSysMiB is bytes obtained from the OS for the heap
	// (MemStats.HeapSys) — the heap's high-water footprint, not
	// live objects; it only shrinks when the runtime returns pages.
	HeapSysMiB float64 `json:"heap_sys_mib"`
	// HeapInuseMiB is heap spans in use (MemStats.HeapInuse).
	HeapInuseMiB float64 `json:"heap_inuse_mib"`
	// HeapIdleMiB is idle heap spans (MemStats.HeapIdle); part of
	// this may be returned to the OS (HeapReleasedMiB).
	HeapIdleMiB     float64 `json:"heap_idle_mib"`
	HeapReleasedMiB float64 `json:"heap_released_mib"`
	HeapObjects     uint64  `json:"heap_objects"`
	StackInuseMiB   float64 `json:"stack_inuse_mib"`
	NumGC           uint32  `json:"num_gc"`
	GCCPUFraction   float64 `json:"gc_cpu_fraction"`
	// GCPauseTotalMs is the cumulative GC pause time since start.
	GCPauseTotalMs float64 `json:"gc_pause_total_ms"`
	// GCPauseLastMs is the most recent GC pause (0 before any GC).
	GCPauseLastMs float64 `json:"gc_pause_last_ms"`
	Goroutines    int     `json:"goroutines"`
	// RSSMiB is VmRSS from /proc/self/status; RSSAvailable is false
	// on non-Linux, where /proc/self/status does not exist.
	RSSMiB       float64 `json:"rss_mib"`
	RSSAvailable bool    `json:"rss_available"`
	// InFlight is the recorder's live in-flight request count
	// (IncInFlight/DecInFlight around every relayed request).
	// InFlightAvailable is false when the server runs without a
	// recorder (metrics disabled).
	InFlight          int64  `json:"in_flight"`
	InFlightAvailable bool   `json:"in_flight_available"`
	RequestsTotal     uint64 `json:"requests_total"`
	// GOGC is the GOGC environment value at process start ("default"
	// when unset, i.e. 100). It is read from the environment, NOT
	// from debug.SetGCPercent(-1): in Go, a negative argument to
	// SetGCPercent DISABLES garbage collection (gcPercent = -1),
	// it does not read the current value — an earlier version of
	// this feed made exactly that mistake and turned the GC off on
	// every poll (see docs/memory-profile-2026-11-18.md). The
	// runtime value is only ever changed from the startup value by
	// the low-end profile (internal/gctune), whose gc: startup line
	// reports it. MemoryLimitMiB below IS a safe read:
	// SetMemoryLimit(-1) is documented to return the current limit
	// without changing it.
	GOGC           string  `json:"gogc"`
	MemoryLimitMiB float64 `json:"memory_limit_mib"`
}

func mib(bytes uint64) float64 { return float64(bytes) / (1024 * 1024) }

// gogcEnv reports the GOGC environment value at process start:
// "default" when unset (the runtime default, 100), otherwise the
// raw value ("off", "50", ...). See the GOGC field comment for why
// this is an env read and never a SetGCPercent call.
func gogcEnv() string {
	if v := os.Getenv("GOGC"); v != "" {
		return v
	}
	return "default"
}

// readRSSKiB parses VmRSS from /proc/self/status. Linux-only; the
// error tells the caller the value is unavailable instead of a
// guessed zero.
func readRSSKiB() (int64, error) {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("malformed VmRSS line %q", line)
		}
		return strconv.ParseInt(fields[1], 10, 64)
	}
	return 0, fmt.Errorf("VmRSS not found in /proc/self/status")
}

// handleDashboardMemstats serves GET /dashboard/memstats (and the
// ?profile=heap / ?profile=goroutine pprof snapshots).
// P2-CAP-004: the JSON counters feed remains AuthExempt, but the binary
// pprof snapshots (?profile=heap/*) expose process heap/stack data and
// require a session (or gateway key) when an operator password is set.
func (s *Server) handleDashboardMemstats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	switch profile := r.URL.Query().Get("profile"); profile {
	case "":
		// JSON snapshot below — AuthExempt, no session required.
	case "heap", "goroutine":
		// Binary pprof profile: requires auth when a password is set,
		// because it exposes heap/stack data that could aid an attacker.
		dst, derr := s.dashboardAuth()
		if derr != nil {
			writeError(w, http.StatusServiceUnavailable, "dashboard auth unavailable: "+derr.Error())
			return
		}
		if dst == nil || !dst.PasswordSet() {
			// No password configured: profile endpoints stay open,
			// matching the auth-relaxed deployment model.
		} else if !s.dashSessionValid(r) && !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "dashboard login required for profile endpoint: POST /dashboard/auth/login with the operator password (or send an admin gateway key)")
			return
		}
		s.serveMemProfile(w, r, profile)
		return
	default:
		writeError(w, http.StatusBadRequest, "unknown profile: want heap or goroutine")
		return
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	res := memstatsResponse{
		GoVersion:       runtime.Version(),
		UptimeSeconds:   time.Since(s.startedAt).Seconds(),
		HeapAllocMiB:    mib(ms.HeapAlloc),
		HeapSysMiB:      mib(ms.HeapSys),
		HeapInuseMiB:    mib(ms.HeapInuse),
		HeapIdleMiB:     mib(ms.HeapIdle),
		HeapReleasedMiB: mib(ms.HeapReleased),
		HeapObjects:     ms.HeapObjects,
		StackInuseMiB:   mib(ms.StackInuse),
		NumGC:           ms.NumGC,
		GCCPUFraction:   ms.GCCPUFraction,
		GCPauseTotalMs:  float64(ms.PauseTotalNs) / 1e6,
		Goroutines:      runtime.NumGoroutine(),
		GOGC:            gogcEnv(),
	}
	if ms.NumGC > 0 {
		// PauseNs is a ring of the last 256 pauses; the newest
		// entry sits just before NumGC's slot.
		res.GCPauseLastMs = float64(ms.PauseNs[(ms.NumGC+255)%256]) / 1e6
	}
	if limit := debug.SetMemoryLimit(-1); limit > 0 {
		res.MemoryLimitMiB = mib(uint64(limit))
	}
	if rssKiB, err := readRSSKiB(); err == nil {
		res.RSSMiB = float64(rssKiB) / 1024
		res.RSSAvailable = true
	}
	if s.recorder != nil {
		snap := s.recorder.Snapshot()
		res.InFlight = snap.InFlight
		res.InFlightAvailable = true
		res.RequestsTotal = snap.Requests
	}
	writeJSON(w, http.StatusOK, res)
}

// serveMemProfile writes one binary pprof profile to the response.
// ?profile=heap runs a GC first (WriteHeapProfile's contract) — the
// snapshot is post-GC, so compare it against the JSON feed's
// HeapAlloc taken at the same moment, not against a pre-GC number.
func (s *Server) serveMemProfile(w http.ResponseWriter, r *http.Request, profile string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%d.prof"`, profile, time.Now().Unix()))
	switch profile {
	case "heap":
		// WriteHeapProfile writes the profile itself; a failure
		// mid-stream is logged — the client sees a truncated
		// profile, never a swapped-in error body.
		if err := pprof.WriteHeapProfile(w); err != nil {
			s.logger.Printf("memstats: heap profile write failed: %v", err)
		}
	case "goroutine":
		// debug=0: binary pprof format, readable by go tool pprof.
		if err := pprof.Lookup("goroutine").WriteTo(w, 0); err != nil {
			s.logger.Printf("memstats: goroutine profile write failed: %v", err)
		}
	}
}
