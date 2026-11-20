package main

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The latency run's validity rests on the prompt generator: 1,000
// sequential requests must be 1,000 distinct prompts, or the
// router's exact/semantic cache would answer repeats instead of
// the upstream and the "added latency" number would measure the
// cache, not the gateway.
func TestPromptsDistinct(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		p := prompt(i)
		if seen[p] {
			t.Fatalf("prompt(%d) repeats an earlier prompt: %q", i, p)
		}
		seen[p] = true
	}
	if len(topics)*len(frames) != 1000 {
		t.Fatalf("topics x frames = %d, want 1000 unique prompts", len(topics)*len(frames))
	}
}

// Warmup prompts must never equal a measured prompt: a warmup hit
// would pre-populate the cache for the measured request with the
// same content (the first rig run showed exactly 25 cache_hits,
// one per warmup request, before this was fixed).
func TestWarmupPromptsDistinctFromMeasured(t *testing.T) {
	measured := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		measured[prompt(i)] = true
	}
	for i := 0; i < 100; i++ {
		if w := warmupPrompt(i); measured[w] {
			t.Fatalf("warmupPrompt(%d) equals a measured prompt: %q", i, w)
		}
	}
}

func TestPercentile(t *testing.T) {
	lat := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(lat, 50); got != 5 {
		t.Fatalf("p50 = %v, want 5", got)
	}
	if got := percentile(lat, 95); got != 10 {
		t.Fatalf("p95 = %v, want 10", got)
	}
	if got := percentile(lat, 99); got != 10 {
		t.Fatalf("p99 = %v, want 10", got)
	}
	if got := percentile(nil, 50); !math.IsNaN(got) {
		t.Fatalf("p50 of empty = %v, want NaN", got)
	}
}

// --- pollution gate and provenance ---

func TestParseLoadavg(t *testing.T) {
	la, err := parseLoadavg("0.42 0.51 0.60 1/234 12345\n")
	if err != nil {
		t.Fatalf("parseLoadavg: %v", err)
	}
	if la != [3]float64{0.42, 0.51, 0.60} {
		t.Fatalf("loadavg = %v, want [0.42 0.51 0.60]", la)
	}
	if _, err := parseLoadavg("0.42 0.51"); err == nil {
		t.Fatal("short loadavg accepted, want error")
	}
	if _, err := parseLoadavg("a b c"); err == nil {
		t.Fatal("non-numeric loadavg accepted, want error")
	}
}

func TestPollutionReasons(t *testing.T) {
	// Quiet machine: no reasons.
	if r := pollutionReasons([3]float64{2.99, 1.0, 1.0}, nil); len(r) != 0 {
		t.Fatalf("quiet machine flagged: %v", r)
	}
	// Threshold is >= 3.0: exactly 3.0 refuses.
	if r := pollutionReasons([3]float64{3.0, 1.0, 1.0}, nil); len(r) != 1 {
		t.Fatalf("loadavg 3.0 gave %d reasons, want 1: %v", len(r), r)
	}
	// One reason per competing process.
	if r := pollutionReasons([3]float64{0.1, 0.1, 0.1}, []string{"pid 1: go test ./..."}); len(r) != 1 {
		t.Fatalf("one competing process gave %d reasons, want 1: %v", len(r), r)
	}
	if r := pollutionReasons([3]float64{5.0, 5.0, 5.0}, []string{"pid 1: go test ./...", "pid 2: go build ./..."}); len(r) != 3 {
		t.Fatalf("hot machine + 2 processes gave %d reasons, want 3: %v", len(r), r)
	}
}

// writeFakeProc creates a fake /proc/<pid>/cmdline in dir.
func writeFakeProc(t *testing.T, dir, pid, cmdline string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, pid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pid, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanGoProcs(t *testing.T) {
	dir := t.TempDir()
	// A competing `go test` must be flagged.
	writeFakeProc(t, dir, "424242", "go\x00test\x00./...\x00")
	// A `go build` must be flagged.
	writeFakeProc(t, dir, "424243", "/home/hatch/go/bin/go\x00build\x00-o\x00/tmp/x\x00./cmd/codvian-router\x00")
	// An unrelated process must not be flagged.
	writeFakeProc(t, dir, "515151", "bash\x00-c\x00sleep 60\x00")
	// Our own test binary (a *.test process) must never be flagged.
	self := os.Getpid()
	writeFakeProc(t, dir, strconv.Itoa(self), "codvian-bench.test\x00-test.run=TestScanGoProcs\x00")

	found, err := scanGoProcs(dir, self, map[int]bool{self: true})
	if err != nil {
		t.Fatalf("scanGoProcs: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d competing processes, want 2: %v", len(found), found)
	}
	joined := strings.Join(found, "\n")
	if !strings.Contains(joined, "go test") || !strings.Contains(joined, "go build") {
		t.Fatalf("found processes do not include both go test and go build: %v", found)
	}
}

func TestCollectProvenance(t *testing.T) {
	p := collectProvenance()
	if p.GoVersion != runtime.Version() {
		t.Fatalf("go version = %q, want %q", p.GoVersion, runtime.Version())
	}
	if p.Command == "" {
		t.Fatal("empty command line in provenance")
	}
	// HEAD is a 40-char SHA (embedded VCS info or git fallback), or
	// "unknown" when neither is available — never empty, never guessed.
	if p.GitHead == "" {
		t.Fatal("empty git head in provenance")
	}
	if p.GitHead != "unknown" && len(p.GitHead) != 40 {
		t.Fatalf("git head = %q, want a 40-char SHA or \"unknown\"", p.GitHead)
	}
}
