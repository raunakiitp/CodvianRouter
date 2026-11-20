// Package gctune applies Codvian Router's Go GC / memory-limit tuning at
// binary startup (research-log entry 144).
//
// Go 1.27 ships the Green Tea collector by default. The tuning pair that
// matters on a memory-constrained machine is GOGC=off +
// runtime.SetMemoryLimit (GOMEMLIMIT): GOGC alone is a ratio, not a budget —
// it has no idea where the cgroup wall is — while the limit pacer still runs
// GC near the ceiling even with GC "off" (see runtime/debug.SetGCPercent:
// "A negative percentage effectively disables garbage collection, unless the
// memory limit is reached", and SetMemoryLimit: "This limit will be
// respected even if GOGC=off").
//
// The memory-limit rule (single rule, both profiles):
//
//		detected = min(cgroup memory limit if set and finite, host MemTotal)
//		limit    = 85% of detected, capped at 2 GiB (no floor)
//
//	  - 15% headroom: the limit covers Go heap only; stacks, mmap, cgo and
//	    kernel buffers sit outside it (research log: set GOMEMLIMIT 5–15%
//	    below the container/machine budget).
//	  - 2 GiB ceiling: this is a local gateway whose working set is tens of
//	    MB; a larger heap limit only delays GC and grows RSS, it never helps
//	    latency here.
//	  - No floor: a floor would push the limit above 85% of detected RAM
//	    on a tiny machine (e.g. a 128 MiB floor on a 128 MiB box is 100%
//	    of RAM) — the exact failure the headroom exists to prevent. On a
//	    machine under ~256 MiB the limit is simply 85% of what exists;
//	    such a machine is below the router's practical minimum anyway.
//
// Profiles:
//
//   - default: set the memory limit, leave the GC percentage exactly as the
//     process started with (Go's normal behaviour, GOGC=100 unless the user
//     set GOGC). Never touches GC pacing.
//   - low-end (flag --low-end or CODVIAN_ROUTER_LOWEND=1): limit + GOGC=off
//     (debug.SetGCPercent(-1)). GOGC=off is NEVER applied without a memory
//     limit: when no limit can be determined, low-end silently degrades to
//     the default profile and says so in the log line.
//
// Explicit configuration wins over auto-detection, in this precedence:
// CODVIAN_ROUTER_MEMORY_LIMIT_MB > GOMEMLIMIT (already honoured by the Go
// runtime at startup; re-applied here so the log line is honest) >
// auto-detected. An explicitly set GOGC env var is never overridden: the
// user's choice wins and the log line says so.
//
// GOMAXPROCS is deliberately untouched: Go 1.25+ is container-aware by
// default and this package must not fight it.
package gctune

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Environment variables.
const (
	// EnvLowEnd enables the low-end profile: GOGC=off + memory limit.
	EnvLowEnd = "CODVIAN_ROUTER_LOWEND"
	// EnvMemoryLimitMB sets the memory limit explicitly, in MiB.
	// It wins over auto-detection and over GOMEMLIMIT.
	EnvMemoryLimitMB = "CODVIAN_ROUTER_MEMORY_LIMIT_MB"
)

// Memory-limit rule constants (see package doc).
const (
	// HeadroomPct is the share of detected RAM left outside the limit.
	HeadroomPct = 15
	// MaxLimitBytes is the ceiling of the computed limit.
	MaxLimitBytes = 2 << 30
)

// MiB is one mebibyte, exported for tests and the log line.
const MiB = 1 << 20

// ApplyOptions controls Apply.
type ApplyOptions struct {
	// LowEndFlag is the --low-end CLI flag. The CODVIAN_ROUTER_LOWEND
	// env var enables low-end too; either one is enough.
	LowEndFlag bool
}

// Result describes what Apply did. LogLine renders it as the single
// startup log line the binary prints.
type Result struct {
	// Profile is "default" or "low-end". It is "default" also when
	// low-end was requested but no memory limit could be determined
	// (GOGC=off without a limit is never applied).
	Profile string
	// LowEndRequested is true when the flag or env asked for low-end,
	// even if it degraded to default for lack of a limit.
	LowEndRequested bool
	// LimitBytes is the memory limit applied, or 0 when none could be
	// determined and none was applied.
	LimitBytes int64
	// DetectedBytes is the RAM auto-detection found, or 0 when the
	// limit came from explicit config instead.
	DetectedBytes uint64
	// LimitSource names where the limit came from: "auto-detected
	// (<source>)", "env CODVIAN_ROUTER_MEMORY_LIMIT_MB",
	// "env GOMEMLIMIT", or "none (detection failed)".
	LimitSource string
	// GOGC describes the GC percentage state after Apply: "default",
	// "off", or "env GOGC=<value> (left as-is)".
	GOGC string
	// Note carries a degradation or warning, empty when nothing to say.
	Note string
}

// LogLine renders the startup line, e.g.:
//
//	gc: profile=low-end, memory limit=2048 MiB (85% of 8124 MiB detected RAM, capped at 2048 MiB; source=host-meminfo), GOGC=off (limit pacer still runs GC)
func (r Result) LogLine() string {
	limit := "none"
	if r.LimitBytes > 0 {
		limit = fmt.Sprintf("%d MiB", r.LimitBytes/MiB)
	}
	detected := ""
	if r.DetectedBytes > 0 {
		detected = fmt.Sprintf(" (85%% of %d MiB detected RAM, capped at %d MiB; source=%s)",
			r.DetectedBytes/MiB, MaxLimitBytes/MiB, r.LimitSource)
	} else if r.LimitSource != "" {
		detected = fmt.Sprintf(" (source=%s)", r.LimitSource)
	}
	line := fmt.Sprintf("gc: profile=%s, memory limit=%s%s, GOGC=%s",
		r.Profile, limit, detected, r.GOGC)
	if r.Note != "" {
		line += " — " + r.Note
	}
	return line
}

// ComputeLimit applies the rule to detected bytes: 85% of detected,
// capped at MaxLimitBytes. A detected value of 0 (detection failed)
// yields 0, meaning "no limit".
func ComputeLimit(detected uint64) int64 {
	if detected == 0 {
		return 0
	}
	limit := int64(detected * uint64(100-HeadroomPct) / 100)
	if limit > MaxLimitBytes {
		limit = MaxLimitBytes
	}
	return limit
}

// DetectAvailableBytes returns the RAM the process may use: the cgroup
// memory limit when one is set and finite, otherwise host MemTotal,
// taking the smaller of the two when both are known. The returned source
// is one of "cgroup-v2", "cgroup-v1", "host-meminfo", or a combination
// joined with "+". On Windows there are no cgroup or /proc files, so
// when the Linux paths yield nothing the fallback is total physical
// RAM via GlobalMemoryStatusEx (source "host-phys").
func DetectAvailableBytes() (uint64, string, error) {
	if v, src, err := detectAvailable(os.ReadFile); err == nil {
		return v, src, nil
	} else if hv, ok := hostRAMBytes(); ok {
		return hv, "host-phys", nil
	} else {
		return 0, "", err
	}
}

// detectAvailable is DetectAvailableBytes over an injectable reader, so
// tests can feed fake cgroup/meminfo files.
func detectAvailable(read func(string) ([]byte, error)) (uint64, string, error) {
	var (
		best    uint64
		sources []string
	)
	// cgroup v2: /sys/fs/cgroup/memory.max, "max" means no limit.
	if b, err := read("/sys/fs/cgroup/memory.max"); err == nil {
		if v, ok := parseCgroupV2Limit(strings.TrimSpace(string(b))); ok {
			best = v
			sources = append(sources, "cgroup-v2")
		}
	}
	// cgroup v1: /sys/fs/cgroup/memory/memory.limit_in_bytes; the
	// kernel reports a huge sentinel (~2^63) when unlimited.
	if b, err := read("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil && v > 0 && v < 1<<62 {
			if best == 0 || v < best {
				best = v
			}
			sources = append(sources, "cgroup-v1")
		}
	}
	// Host RAM from /proc/meminfo (MemTotal, in KiB).
	if b, err := read("/proc/meminfo"); err == nil {
		if v, ok := parseMemTotal(string(b)); ok {
			if best == 0 || v < best {
				best = v
			}
			sources = append(sources, "host-meminfo")
		}
	}
	if best == 0 {
		return 0, "", fmt.Errorf("gctune: could not detect available memory (no cgroup limit, no /proc/meminfo)")
	}
	return best, strings.Join(sources, "+"), nil
}

// parseCgroupV2Limit parses a memory.max value: "max" means unlimited
// (ok=false), otherwise a byte count.
func parseCgroupV2Limit(s string) (uint64, bool) {
	if s == "max" || s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil || v == 0 {
		return 0, false
	}
	return v, true
}

// parseMemTotal extracts MemTotal (kB) from /proc/meminfo text.
func parseMemTotal(meminfo string) (uint64, bool) {
	for _, line := range strings.Split(meminfo, "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || v == 0 {
			return 0, false
		}
		return v * 1024, true // meminfo reports KiB
	}
	return 0, false
}

// parseGoMemLimit parses a GOMEMLIMIT-style value: plain bytes, or a
// number with a KiB/MiB/GiB/TiB (or KB/MB/GB/TB) suffix, as Go accepts.
func parseGoMemLimit(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	mult := int64(1)
	for _, suf := range []struct {
		suffix string
		mult   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
		{"B", 1},
	} {
		if strings.HasSuffix(s, suf.suffix) {
			mult = suf.mult
			s = strings.TrimSpace(strings.TrimSuffix(s, suf.suffix))
			break
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v * mult, true
}

// parseLowEndBool parses CODVIAN_ROUTER_LOWEND: 1/true/yes/on enable it.
func parseLowEndBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Apply detects/applies the memory limit and, for the low-end profile,
// GOGC=off — never the latter without the former. It is safe to call
// once at startup; it mutates global runtime settings.
func Apply(opts ApplyOptions) Result {
	lowEnd := opts.LowEndFlag || parseLowEndBool(os.Getenv(EnvLowEnd))
	res := Result{LowEndRequested: lowEnd, GOGC: "default"}

	// 1. Explicit limit wins: CODVIAN_ROUTER_MEMORY_LIMIT_MB.
	if v := strings.TrimSpace(os.Getenv(EnvMemoryLimitMB)); v != "" {
		if mb, err := strconv.ParseInt(v, 10, 64); err == nil && mb > 0 {
			res.LimitBytes = mb * MiB
			res.LimitSource = "env " + EnvMemoryLimitMB
		} else {
			res.Note = "ignoring invalid " + EnvMemoryLimitMB + "=" + strconv.Quote(v)
		}
	}
	// 2. GOMEMLIMIT, already honoured by the runtime at startup.
	if res.LimitBytes == 0 {
		if v := strings.TrimSpace(os.Getenv("GOMEMLIMIT")); v != "" {
			if limit, ok := parseGoMemLimit(v); ok {
				res.LimitBytes = limit
				res.LimitSource = "env GOMEMLIMIT"
			} else {
				if res.Note != "" {
					res.Note += "; "
				}
				res.Note += "ignoring unparsable GOMEMLIMIT=" + strconv.Quote(v)
			}
		}
	}
	// 3. Auto-detect.
	if res.LimitBytes == 0 {
		detected, src, err := DetectAvailableBytes()
		if err == nil {
			res.DetectedBytes = detected
			res.LimitBytes = ComputeLimit(detected)
			res.LimitSource = "auto-detected (" + src + ")"
		} else {
			res.LimitSource = "none (detection failed)"
			if res.Note != "" {
				res.Note += "; "
			}
			res.Note += "memory detection failed: " + err.Error()
		}
	}

	if res.LimitBytes > 0 {
		debug.SetMemoryLimit(res.LimitBytes)
	}

	// Low-end only with a limit in place; an explicit GOGC env var is
	// never overridden.
	if lowEnd && res.LimitBytes > 0 {
		if v := strings.TrimSpace(os.Getenv("GOGC")); v != "" {
			res.Profile = "low-end"
			res.GOGC = "env GOGC=" + v + " (left as-is)"
			if res.Note != "" {
				res.Note += "; "
			}
			res.Note += "GOGC env set — low-end did not force GOGC=off"
		} else {
			debug.SetGCPercent(-1) // GOGC=off; the limit pacer still runs GC
			res.Profile = "low-end"
			res.GOGC = "off (limit pacer still runs GC)"
		}
		return res
	}
	if lowEnd {
		// No limit: GOGC=off would be unsafe, degrade to default.
		res.Profile = "default"
		if res.Note != "" {
			res.Note += "; "
		}
		res.Note += "low-end requested but no memory limit could be set — GOGC=off NOT applied"
		return res
	}
	res.Profile = "default"
	return res
}
