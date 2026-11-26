package main

// Behavioral tests for the bench-compare regression gate (WS7/B4):
// each exit outcome — pass, regression, skip, invalid input — is
// exercised against real files by building and running the binary.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles the bench-compare binary once per test.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bench-compare.exe")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// writeResult writes one bench.sh-shaped result JSON.
func writeResult(t *testing.T, cpu string, readyHealthz float64, passes map[string][4]float64) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"rig":{"cpu_model":"` + cpu + `"},"ready_ms":{"healthz":`)
	b.WriteString(fnum(readyHealthz))
	b.WriteString(`},"passes":{`)
	first := true
	for name, m := range passes {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString(`"` + name + `":{"direct_latency":{"mean_ms":` + fnum(m[0]) +
			`,"p50_ms":` + fnum(m[1]) + `,"p95_ms":` + fnum(m[2]) + `,"p99_ms":` + fnum(m[3]) + `}}`)
	}
	b.WriteString(`}}`)
	path := filepath.Join(t.TempDir(), "bench.json")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fnum(v float64) string {
	s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

func runGate(t *testing.T, bin string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("run bench-compare: %v\n%s", err, out)
	return -1, ""
}

func TestBenchComparePass(t *testing.T) {
	bin := buildBinary(t)
	base := writeResult(t, "AMD EPYC", 1000, map[string][4]float64{"1": {10, 8, 12, 15}})
	cur := writeResult(t, "AMD EPYC", 1050, map[string][4]float64{"1": {11, 9, 13, 16}}) // +10%, within 25%
	code, out := runGate(t, bin, "--current", cur, "--baseline", base)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (within threshold)\n%s", code, out)
	}
	if !strings.Contains(out, "PASS") {
		t.Fatalf("output = %q, want PASS", out)
	}
}

func TestBenchCompareRegressionFails(t *testing.T) {
	bin := buildBinary(t)
	base := writeResult(t, "AMD EPYC", 1000, map[string][4]float64{"1": {10, 8, 12, 15}})
	cur := writeResult(t, "AMD EPYC", 1000, map[string][4]float64{"1": {15, 12, 18, 22}}) // +50% mean
	code, out := runGate(t, bin, "--current", cur, "--baseline", base)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (regression beyond threshold)\n%s", code, out)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "mean_ms") {
		t.Fatalf("output = %q, want FAIL naming mean_ms", out)
	}
}

func TestBenchCompareMissingBaselineSkips(t *testing.T) {
	bin := buildBinary(t)
	cur := writeResult(t, "AMD EPYC", 1000, map[string][4]float64{"1": {10, 8, 12, 15}})
	code, out := runGate(t, bin, "--current", cur)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (no baseline yet is not a failure)\n%s", code, out)
	}
	if !strings.Contains(out, "SKIP") {
		t.Fatalf("output = %q, want SKIP", out)
	}
}

func TestBenchCompareRigMismatchSkips(t *testing.T) {
	bin := buildBinary(t)
	base := writeResult(t, "AMD EPYC", 1000, map[string][4]float64{"1": {10, 8, 12, 15}})
	cur := writeResult(t, "Intel Xeon", 1000, map[string][4]float64{"1": {50, 40, 60, 70}})
	code, out := runGate(t, bin, "--current", cur, "--baseline", base)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (cross-rig numbers are not comparable)\n%s", code, out)
	}
	if !strings.Contains(out, "SKIP") || !strings.Contains(out, "rig mismatch") {
		t.Fatalf("output = %q, want SKIP rig mismatch", out)
	}
}

func TestBenchCompareInvalidCurrentFails(t *testing.T) {
	bin := buildBinary(t)
	base := writeResult(t, "AMD EPYC", 1000, map[string][4]float64{"1": {10, 8, 12, 15}})
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _ := runGate(t, bin, "--current", bad, "--baseline", base)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (invalid input must never read as no-regression)", code)
	}
}

func TestBenchCompareThresholdFlag(t *testing.T) {
	bin := buildBinary(t)
	base := writeResult(t, "AMD EPYC", 1000, map[string][4]float64{"1": {10, 8, 12, 15}})
	// +30% mean: regression at the default 25%, pass at 50%.
	cur := writeResult(t, "AMD EPYC", 1000, map[string][4]float64{"1": {13, 10, 15, 18}})
	code, _ := runGate(t, bin, "--current", cur, "--baseline", base)
	if code != 1 {
		t.Fatalf("default threshold: exit = %d, want 1", code)
	}
	code, out := runGate(t, bin, "--current", cur, "--baseline", base, "--threshold", "0.5")
	if code != 0 {
		t.Fatalf("threshold 0.5: exit = %d, want 0\n%s", code, out)
	}
}
