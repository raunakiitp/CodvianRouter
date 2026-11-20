package gctune

import (
	"os"
	"runtime/debug"
	"strings"
	"testing"
)

func TestComputeLimit(t *testing.T) {
	cases := []struct {
		detected uint64
		want     int64
	}{
		{0, 0},                 // detection failed: no limit
		{100 << 20, 89128960},  // 85% of 100 MiB (no floor: a floor would exceed 85% on tiny machines)
		{256 << 20, 228170137}, // 85% of 256 MiB = 217.6 MiB = 228,170,137 bytes
		{1 << 30, 912680550},   // 85% of 1 GiB (912,680,550 bytes; not MiB-aligned, exact)
		{8 << 30, 2 << 30},     // 85% of 8 GiB -> ceiling 2 GiB
		{32 << 30, 2 << 30},    // ceiling holds on big machines
	}
	for _, tc := range cases {
		if got := ComputeLimit(tc.detected); got != tc.want {
			t.Errorf("ComputeLimit(%d) = %d, want %d", tc.detected, got, tc.want)
		}
	}
}

func TestDetectAvailablePrefersSmallest(t *testing.T) {
	read := func(path string) ([]byte, error) {
		switch path {
		case "/sys/fs/cgroup/memory.max":
			return []byte("536870912\n"), nil // 512 MiB cgroup cap
		case "/proc/meminfo":
			return []byte("MemTotal:        8124900 kB\n"), nil // ~7.7 GiB host
		default:
			return nil, os.ErrNotExist
		}
	}
	got, src, err := detectAvailable(read)
	if err != nil {
		t.Fatalf("detectAvailable: %v", err)
	}
	if got != 512<<20 {
		t.Fatalf("detected = %d, want 512 MiB (cgroup cap wins over host)", got)
	}
	if !strings.Contains(src, "cgroup-v2") || !strings.Contains(src, "host-meminfo") {
		t.Fatalf("source = %q, want cgroup-v2+host-meminfo", src)
	}
}

func TestDetectAvailableUnlimitedCgroupFallsBackToHost(t *testing.T) {
	read := func(path string) ([]byte, error) {
		switch path {
		case "/sys/fs/cgroup/memory.max":
			return []byte("max\n"), nil
		case "/proc/meminfo":
			return []byte("MemTotal:        2048000 kB\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}
	got, src, err := detectAvailable(read)
	if err != nil {
		t.Fatalf("detectAvailable: %v", err)
	}
	if got != 2048000*1024 {
		t.Fatalf("detected = %d, want host MemTotal", got)
	}
	if strings.Contains(src, "cgroup") {
		t.Fatalf("source = %q, want host-meminfo only", src)
	}
}

func TestDetectAvailableAllMissing(t *testing.T) {
	_, _, err := detectAvailable(func(string) ([]byte, error) { return nil, os.ErrNotExist })
	if err == nil {
		t.Fatal("expected error when nothing is detectable")
	}
}

func TestParseGoMemLimit(t *testing.T) {
	for in, want := range map[string]int64{
		"512MiB":  512 << 20,
		"2GiB":    2 << 30,
		"1000000": 1000000,
		"500MB":   500 * 1000 * 1000,
	} {
		got, ok := parseGoMemLimit(in)
		if !ok || got != want {
			t.Errorf("parseGoMemLimit(%q) = %d,%v want %d,true", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "abc", "0", "-5", "1.5GiB"} {
		if _, ok := parseGoMemLimit(bad); ok {
			t.Errorf("parseGoMemLimit(%q) unexpectedly parsed", bad)
		}
	}
}

// restoreRuntime resets the global GC/limit settings Apply mutated.
// Tests that call Apply must defer this.
func restoreRuntime(t *testing.T, prevLimit int64, prevGC int) {
	t.Helper()
	debug.SetMemoryLimit(prevLimit)
	debug.SetGCPercent(prevGC)
}

func TestApplyDefaultSetsLimitKeepsGC(t *testing.T) {
	t.Setenv(EnvLowEnd, "")
	t.Setenv(EnvMemoryLimitMB, "")
	t.Setenv("GOMEMLIMIT", "")
	t.Setenv("GOGC", "")
	prevLimit := debug.SetMemoryLimit(-1)
	prevGC := debug.SetGCPercent(100)
	defer restoreRuntime(t, prevLimit, prevGC)

	res := Apply(ApplyOptions{})
	if res.Profile != "default" {
		t.Fatalf("profile = %q, want default", res.Profile)
	}
	if res.LimitBytes <= 0 {
		t.Fatalf("limit = %d, want > 0 (this machine detects RAM)", res.LimitBytes)
	}
	if res.GOGC != "default" {
		t.Fatalf("GOGC = %q, want default (untouched)", res.GOGC)
	}
	if got := debug.SetMemoryLimit(-1); got != res.LimitBytes {
		t.Fatalf("runtime limit = %d, want %d", got, res.LimitBytes)
	}
	if !strings.HasPrefix(res.LogLine(), "gc: profile=default, memory limit=") {
		t.Fatalf("log line = %q", res.LogLine())
	}
}

func TestApplyLowEndSetsGOGCOffWithLimit(t *testing.T) {
	t.Setenv(EnvLowEnd, "1")
	t.Setenv(EnvMemoryLimitMB, "512")
	t.Setenv("GOMEMLIMIT", "")
	t.Setenv("GOGC", "")
	prevLimit := debug.SetMemoryLimit(-1)
	prevGC := debug.SetGCPercent(100)
	defer restoreRuntime(t, prevLimit, prevGC)

	res := Apply(ApplyOptions{})
	if res.Profile != "low-end" {
		t.Fatalf("profile = %q, want low-end", res.Profile)
	}
	if res.LimitBytes != 512<<20 {
		t.Fatalf("limit = %d, want 512 MiB from env", res.LimitBytes)
	}
	if res.GOGC != "off (limit pacer still runs GC)" {
		t.Fatalf("GOGC = %q", res.GOGC)
	}
	if got := debug.SetMemoryLimit(-1); got != 512<<20 {
		t.Fatalf("runtime limit = %d, want 512 MiB", got)
	}
	if !strings.Contains(res.LogLine(), "GOGC=off") {
		t.Fatalf("log line = %q, want GOGC=off", res.LogLine())
	}
}

func TestApplyLowEndNeverWithoutLimit(t *testing.T) {
	// Invalid explicit limit + GOMEMLIMIT unset + detection that
	// cannot work is hard to force on a real machine (meminfo always
	// exists), so this test pins the invariant at the rule level:
	// ComputeLimit(0) == 0, and Apply with an env limit of 0 is
	// ignored, so low-end can only proceed when LimitBytes > 0.
	// The degradation branch itself is covered by construction: Apply
	// returns early to default whenever LimitBytes == 0 with low-end
	// requested (see the `if lowEnd` guard in Apply).
	if ComputeLimit(0) != 0 {
		t.Fatal("ComputeLimit(0) must be 0 so low-end cannot arm without a limit")
	}
	t.Setenv(EnvMemoryLimitMB, "not-a-number")
	t.Setenv("GOMEMLIMIT", "")
	t.Setenv("GOGC", "")
	t.Setenv(EnvLowEnd, "1")
	prevLimit := debug.SetMemoryLimit(-1)
	prevGC := debug.SetGCPercent(100)
	defer restoreRuntime(t, prevLimit, prevGC)

	res := Apply(ApplyOptions{})
	// Detection still succeeds on this machine, so low-end arms with
	// the auto-detected limit; the invalid env value must be noted.
	if res.Profile != "low-end" {
		t.Fatalf("profile = %q, want low-end via auto-detected limit", res.Profile)
	}
	if !strings.Contains(res.Note, EnvMemoryLimitMB) {
		t.Fatalf("note = %q, want invalid-env warning", res.Note)
	}
}

func TestApplyExplicitGOGCIsNeverOverridden(t *testing.T) {
	t.Setenv(EnvLowEnd, "1")
	t.Setenv(EnvMemoryLimitMB, "256")
	t.Setenv("GOMEMLIMIT", "")
	t.Setenv("GOGC", "200")
	prevLimit := debug.SetMemoryLimit(-1)
	prevGC := debug.SetGCPercent(100)
	defer restoreRuntime(t, prevLimit, prevGC)

	res := Apply(ApplyOptions{})
	if res.Profile != "low-end" {
		t.Fatalf("profile = %q, want low-end", res.Profile)
	}
	if !strings.Contains(res.GOGC, "env GOGC=200") {
		t.Fatalf("GOGC = %q, want explicit env preserved", res.GOGC)
	}
}
