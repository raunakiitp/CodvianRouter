package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/compress/evalharness"
)

func runBenchCompression(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runCompressionOut(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// TestCompressionSubcommandJSON: the benchmark is executable without
// `go test`, emits parseable machine-readable JSON naming its model,
// and covers exactly the standard corpus.
func TestCompressionSubcommandJSON(t *testing.T) {
	code, out, errOut := runBenchCompression(t, "-json", "-repeats", "1")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	var report struct {
		Model string `json:"model"`
		Cases []struct {
			Name     string `json:"name"`
			Category string `json:"category"`
		} `json:"cases"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("JSON did not parse: %v\n%s", err, out)
	}
	if report.Model != evalharness.DefaultModel {
		t.Fatalf("model %q, want %q", report.Model, evalharness.DefaultModel)
	}
	if len(report.Cases) != len(evalharness.Corpus()) {
		t.Fatalf("reported %d cases, corpus has %d", len(report.Cases), len(evalharness.Corpus()))
	}
	for i, c := range report.Cases {
		if c.Name == "" || c.Category == "" {
			t.Fatalf("case %d has empty name/category: %+v", i, c)
		}
	}
}

// TestCompressionSubcommandText: the human table names both engine
// totals and a pass/fail verdict per row.
func TestCompressionSubcommandText(t *testing.T) {
	code, out, errOut := runBenchCompression(t, "-repeats", "1")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	for _, want := range []string{"TOTAL (lossless)", "TOTAL (lossy)", "PASS"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text report missing %q:\n%s", want, out)
		}
	}
}

// TestCompressionSubcommandLongCorpus: the long-context corpus runs
// through the same subcommand and both of its cases meet their
// thresholds (the middle-turn case has no prose floor by design).
func TestCompressionSubcommandLongCorpus(t *testing.T) {
	code, out, errOut := runBenchCompression(t, "-corpus", "long", "-repeats", "1")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s\n%s", code, errOut, out)
	}
	if !strings.Contains(out, "long-context-middle-turn") || !strings.Contains(out, "long-context-final-turn") {
		t.Fatalf("long corpus cases missing from report:\n%s", out)
	}
}

// TestCompressionSubcommandBadInput: unknown corpus and stray
// arguments are usage errors (exit 2), never silent success.
func TestCompressionSubcommandBadInput(t *testing.T) {
	if code, _, _ := runBenchCompression(t, "-corpus", "nope"); code != 2 {
		t.Fatalf("unknown corpus exit %d, want 2", code)
	}
	if code, _, _ := runBenchCompression(t, "stray"); code != 2 {
		t.Fatalf("stray argument exit %d, want 2", code)
	}
}
