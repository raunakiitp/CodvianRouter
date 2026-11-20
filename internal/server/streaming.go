// Streaming-delivery feed (GET /dashboard/streaming).
//
// The dashboard's Benchmark section shows the last measured SSE
// token-delivery run — the numbers recorded by
// scripts/bench-sse.sh, copied to the router's data dir as
// streaming.json (CODVIAN_SSE_OUT). Every value comes from that
// file; nothing is estimated.
//
// There is deliberately NO target verdict here, unlike the
// Geekflare benchmark feed: no latency target exists for token
// delivery yet, so the feed reports the measured median/p99
// before (hybrid batching off) and after (batching on) at each
// mock cadence, and the dashboard renders them as measurements,
// not as MET/MISSED. Inventing a target to pass would violate
// the no-fake-data rule.
//
// No run recorded yet: an honest empty state (recorded=false +
// a note saying how to record one), never invented numbers. A
// corrupt file is a 500 — the operator should re-run the bench,
// not be shown stale or guessed values.
package server

import (
	"encoding/json"
	"net/http"
	"os"
)

// WithStreamingFile wires the SSE-delivery run file GET
// /dashboard/streaming serves (the router's data dir
// streaming.json, written by scripts/bench-sse.sh via
// CODVIAN_SSE_OUT). Empty path (tests, servers without a data
// dir) means the endpoint always answers recorded=false.
func WithStreamingFile(path string) Option {
	return func(s *Server) { s.streamingFile = path }
}

// WithSSEBatching enables or disables hybrid SSE token
// batching (flush at most every 8 ms or every 3 tokens, items
// 135-136). The default is enabled; disable it only to measure
// the before/after difference (scripts/bench-sse.sh sets
// CODVIAN_ROUTER_SSE_BATCH=0 for the "before" run).
func WithSSEBatching(enabled bool) Option {
	return func(s *Server) {
		s.sseBatching.enabled = enabled
		s.sseBatching.maxTokens = sseBatchMaxTokens
		s.sseBatching.maxDelay = sseBatchMaxDelay
	}
}

// streamingFile is the on-disk shape written by
// scripts/bench-sse.sh. Only the fields the feed reports are
// decoded.
type streamingFile struct {
	Date string `json:"date"`
	Rig  struct {
		Date     string `json:"date"`
		OS       string `json:"os"`
		CPUs     int    `json:"cpus"`
		CPUModel string `json:"cpu_model"`
		GoVer    string `json:"go_version"`
		Note     string `json:"note"`
	} `json:"rig"`
	Batching struct {
		MaxTokens  int     `json:"max_tokens"`
		MaxDelayMs float64 `json:"max_delay_ms"`
	} `json:"batching"`
	TCPNoDelay struct {
		Verified bool   `json:"verified"`
		Note     string `json:"note"`
	} `json:"tcp_nodelay"`
	Scenarios []struct {
		CadenceMs float64 `json:"cadence_ms"`
		Tokens    int     `json:"tokens"`
		Before    struct {
			MedianMs float64 `json:"median_ms"`
			P99Ms    float64 `json:"p99_ms"`
			MeanMs   float64 `json:"mean_ms"`
			MaxMs    float64 `json:"max_ms"`
			Received int     `json:"received"`
		} `json:"before"`
		After struct {
			MedianMs float64 `json:"median_ms"`
			P99Ms    float64 `json:"p99_ms"`
			MeanMs   float64 `json:"mean_ms"`
			MaxMs    float64 `json:"max_ms"`
			Received int     `json:"received"`
		} `json:"after"`
	} `json:"scenarios"`
	Note string `json:"note"`
}

type streamingStats struct {
	MedianMs float64 `json:"median_ms"`
	P99Ms    float64 `json:"p99_ms"`
	MeanMs   float64 `json:"mean_ms"`
	MaxMs    float64 `json:"max_ms"`
	Received int     `json:"received"`
}

type streamingScenario struct {
	CadenceMs float64        `json:"cadence_ms"`
	Tokens    int            `json:"tokens"`
	Before    streamingStats `json:"before"`
	After     streamingStats `json:"after"`
}

type streamingBatching struct {
	MaxTokens  int     `json:"max_tokens"`
	MaxDelayMs float64 `json:"max_delay_ms"`
}

type streamingTCPNoDelay struct {
	Verified bool   `json:"verified"`
	Note     string `json:"note,omitempty"`
}

type streamingResponse struct {
	Recorded     bool                `json:"recorded"`
	Note         string              `json:"note,omitempty"`
	Date         string              `json:"date,omitempty"`
	RigOS        string              `json:"rig_os,omitempty"`
	RigCPUs      int                 `json:"rig_cpus,omitempty"`
	RigCPUModel  string              `json:"rig_cpu_model,omitempty"`
	RigGoVersion string              `json:"rig_go_version,omitempty"`
	RigNote      string              `json:"rig_note,omitempty"`
	Batching     streamingBatching   `json:"batching,omitempty"`
	TCPNoDelay   streamingTCPNoDelay `json:"tcp_nodelay,omitempty"`
	Scenarios    []streamingScenario `json:"scenarios,omitempty"`
}

// handleDashboardStreaming serves GET /dashboard/streaming.
func (s *Server) handleDashboardStreaming(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	noRun := func() {
		writeJSON(w, http.StatusOK, streamingResponse{
			Recorded: false,
			Note:     "No streaming run recorded yet — run scripts/bench-sse.sh (with CODVIAN_SSE_OUT=<data-dir>/streaming.json) to record one.",
		})
	}
	if s.streamingFile == "" {
		noRun()
		return
	}
	raw, err := os.ReadFile(s.streamingFile)
	if err != nil {
		if os.IsNotExist(err) {
			noRun()
			return
		}
		s.logger.Printf("streaming feed: cannot read %s: %v", s.streamingFile, err)
		writeError(w, http.StatusInternalServerError, "streaming file unreadable")
		return
	}
	var sf streamingFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		s.logger.Printf("streaming feed: corrupt %s: %v", s.streamingFile, err)
		writeError(w, http.StatusInternalServerError, "streaming file is corrupt — re-run scripts/bench-sse.sh")
		return
	}
	scenarios := make([]streamingScenario, 0, len(sf.Scenarios))
	for _, sc := range sf.Scenarios {
		scenarios = append(scenarios, streamingScenario{
			CadenceMs: sc.CadenceMs,
			Tokens:    sc.Tokens,
			Before: streamingStats{
				MedianMs: sc.Before.MedianMs, P99Ms: sc.Before.P99Ms,
				MeanMs: sc.Before.MeanMs, MaxMs: sc.Before.MaxMs, Received: sc.Before.Received,
			},
			After: streamingStats{
				MedianMs: sc.After.MedianMs, P99Ms: sc.After.P99Ms,
				MeanMs: sc.After.MeanMs, MaxMs: sc.After.MaxMs, Received: sc.After.Received,
			},
		})
	}
	writeJSON(w, http.StatusOK, streamingResponse{
		Recorded:     true,
		Date:         sf.Date,
		RigOS:        sf.Rig.OS,
		RigCPUs:      sf.Rig.CPUs,
		RigCPUModel:  sf.Rig.CPUModel,
		RigGoVersion: sf.Rig.GoVer,
		RigNote:      sf.Rig.Note,
		Batching:     streamingBatching{MaxTokens: sf.Batching.MaxTokens, MaxDelayMs: sf.Batching.MaxDelayMs},
		TCPNoDelay:   streamingTCPNoDelay{Verified: sf.TCPNoDelay.Verified, Note: sf.TCPNoDelay.Note},
		Scenarios:    scenarios,
		Note:         sf.Note,
	})
}
