package compress

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Fixed corpora: realistic command outputs. Every behaviour
// test asserts the FILTERED output satisfies the property AND
// (in TestToggleEvidence) that the UNFILTERED input does not —
// so each test fails with the filter disabled/bypassed and
// passes with it.

const npmLog = "\x1b[32madded 3 packages\x1b[0m\n" +
	"packages are looking for funding\n" +
	"Progress: 45%\n" +
	"[####------] 50%\n" +
	"npm notice created a lockfile as package-lock.json\n" +
	"npm http fetch GET 200 https://registry.npmjs.org/react\n" +
	"audited 3 packages in 1s\n" +
	"\n" +
	"found 0 vulnerabilities\n"

const pytestLog = "collected 3 items\n" +
	"\n" +
	"....F....\n" +
	"FAILED tests/test_a.py::test_a - AssertionError\n" +
	"Traceback (most recent call last):\n" +
	"  File \"tests/test_a.py\", line 12, in test_a\n" +
	"    assert 1 == 2\n" +
	"E assert 1 == 2\n" +
	"================ 1 failed, 2 passed ================\n"

const goLog = "=== RUN TestA\n" +
	"=== RUN TestB\n" +
	"=== RUN TestC\n" +
	"--- PASS: TestA (0.00s)\n" +
	"--- PASS: TestC (0.00s)\n" +
	"--- FAIL: TestB (0.00s)\n" +
	"    b_test.go:12: boom\n" +
	"FAIL\t./pkg\t0.1s\n" +
	"ok  \t./other\t0.02s\n"

const jestLog = "PASS src/a.test.ts\n" +
	"PASS src/b.test.ts\n" +
	"FAIL src/c.test.ts\n" +
	"  ✕ renders the widget\n" +
	"  ✓ renders the header\n" +
	"  ✓ renders the footer\n" +
	"Test Suites: 1 failed, 2 passed, 3 total\n" +
	"Tests:       1 failed, 4 passed, 5 total\n"

func jsonlLog() string {
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		sb.WriteString(`{"level":"info","msg":"starting server ` + itoa(i) + `","ts":"2026-11-19T03:00:0` + itoa(i) + `Z"}` + "\n")
	}
	sb.WriteString(`{"level":"error","msg":"db connection failed","code":"ECONNREFUSED","ts":"2026-11-19T03:05:00Z"}` + "\n")
	sb.WriteString(`{"level":"warn","msg":"retrying connection","ts":"2026-11-19T03:06:00Z"}` + "\n")
	return sb.String()
}

func stackTraceLog() string {
	var sb strings.Builder
	sb.WriteString("node:internal/modules/cjs/loader: running main\n")
	// Distinct frames (dedup cannot collapse them), with the
	// failure in the MIDDLE so only priority-aware head/tail
	// truncation can save it.
	for i := 0; i < 100; i++ {
		sb.WriteString("    at fn" + itoa(i) + " (/repo/src/a" + itoa(i) + ".ts:" + itoa(i+1) + ":1)\n")
	}
	sb.WriteString("Error: boom\n")
	sb.WriteString("Caused by: bad input\n")
	for i := 100; i < 200; i++ {
		sb.WriteString("    at fn" + itoa(i) + " (/repo/src/a" + itoa(i) + ".ts:" + itoa(i+1) + ":1)\n")
	}
	return sb.String()
}

const ansiProgressLog = "\x1b[36m⠋ Installing packages...\x1b[0m\n" +
	"\x1b[36m⠙ Installing packages...\x1b[0m\n" +
	"Downloading foo 10%\rDownloading foo 90%\n" +
	"[====>   ] 50%\n" +
	"added 3 packages\n"

const cleanOutput = "Build succeeded.\nAll 42 tests passed.\n"

const proseText = "The quick brown fox jumps over the lazy dog. " +
	"This is a normal user message with no command output at all, " +
	"just prose that must pass through the filter untouched."

const singleJSONBlob = "{\n  \"status\": \"ok\",\n  \"items\": [1, 2, 3],\n  \"message\": \"all good\"\n}\n"

const pipLog = "Collecting https://files.example.com/pkg-alpha-1.0.tar.gz\n" +
	"Requirement already satisfied: six in /usr/lib/python3\n" +
	"Downloading six-1.0-py3-none-any.whl (10 kB)\n" +
	"Successfully installed beta - docs at https://example.com/docs/beta\n"

// mustFilter fails the test unless out satisfies want and the
// raw input does not (toggle evidence, inline per case).
func assertFiltered(t *testing.T, name, in, out string, want []string, notWant []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("%s: filtered output missing %q\noutput:\n%s", name, w, out)
		}
	}
	for _, w := range notWant {
		if strings.Contains(out, w) {
			t.Errorf("%s: filtered output still contains %q\noutput:\n%s", name, w, out)
		}
	}
	// Toggle: the raw input must NOT already satisfy the
	// expectation, or the test would pass with no filter.
	satisfies := true
	for _, w := range want {
		if !strings.Contains(in, w) {
			satisfies = false
		}
	}
	for _, w := range notWant {
		if strings.Contains(in, w) {
			satisfies = false
		}
	}
	if satisfies {
		t.Errorf("%s: raw input already satisfies expectations — test would pass with the filter bypassed", name)
	}
}

func TestRTKPackCount(t *testing.T) {
	// Honest count: 18 implemented packs, not OmniRoute's 56.
	if got := RTKPackCount(); got != 18 {
		t.Fatalf("RTKPackCount() = %d, want 18 (update the count AND the report if this changes)", got)
	}
	ids := RTKPackIDs()
	if len(ids) != 18 {
		t.Fatalf("RTKPackIDs() len = %d, want 18", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			t.Fatalf("RTKPackIDs() has empty or duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestDetectRTKPack(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		command string
		want    string
	}{
		{"git-status cmd", "On branch main\nnothing to commit", "git status", "git-status"},
		{"git-status content", "On branch main\nUntracked files:\n  (use \"git add\" to include)", "", "git-status"},
		{"git-diff", "diff --git a/x.go b/x.go\n@@ -1,2 +1,2 @@\n-old\n+new", "", "git-diff"},
		{"git-log", "commit abcdef1234567\nAuthor: Raunak <r@example.com>", "git log", "git-log"},
		{"git-branch", "* main\n  feature-x", "git branch", "git-branch"},
		{"pytest", pytestLog, "pytest", "test-pytest"},
		{"go test", goLog, "go test ./...", "test-go"},
		{"jest", jestLog, "npm test", "test-jest"},
		{"cargo", "running 3 tests\ntest a ... ok\ntest result: ok. 3 passed", "cargo test", "test-cargo"},
		{"phpunit", "FAILURES!\nTests: 3, Assertions: 3, Failures: 1.", "phpunit", "test-phpunit"},
		{"npm-install", npmLog, "npm install", "npm-install"},
		{"pip-install", pipLog, "pip install beta", "pip-install"},
		{"typescript", "src/a.ts(1,2): error TS2322: Type 'x' is not assignable", "tsc --noEmit", "build-typescript"},
		{"docker-ps", "CONTAINER ID   IMAGE   COMMAND   CREATED", "docker ps", "docker-ps"},
		{"docker-logs", "2026-11-19T03:00:00Z server started", "docker logs web", "docker-logs"},
		{"terraform", "Plan: 2 to add, 0 to change, 0 to destroy.", "terraform plan", "terraform-plan"},
		{"jsonl", jsonlLog(), "", "json-output"},
		{"stacktrace", stackTraceLog(), "", "error-stacktrace"},
		{"generic error", "Error: something broke badly", "", "generic-output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pack, score := DetectRTKPack(tc.text, tc.command)
			if score == 0 {
				t.Fatalf("no pack detected for %q (command %q)", tc.name, tc.command)
			}
			if pack.ID != tc.want {
				t.Errorf("pack = %q, want %q (score %d)", pack.ID, tc.want, score)
			}
		})
	}
	t.Run("prose matches nothing", func(t *testing.T) {
		if _, score := DetectRTKPack(proseText, ""); score != 0 {
			t.Errorf("prose detected as a pack (score %d)", score)
		}
		if _, score := DetectRTKPack(cleanOutput, ""); score != 0 {
			t.Errorf("clean output detected as a pack (score %d)", score)
		}
	})
}

func TestParseRTKLevel(t *testing.T) {
	for in, want := range map[string]RTKLevel{
		"": "", "minimal": RTKLevelMinimal, "Minimal": RTKLevelMinimal,
		"standard": RTKLevelStandard, "STANDARD": RTKLevelStandard,
		"aggressive": RTKLevelAggressive,
	} {
		got, ok := ParseRTKLevel(in)
		if !ok || got != want {
			t.Errorf("ParseRTKLevel(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	if _, ok := ParseRTKLevel("ultra"); ok {
		t.Errorf("ParseRTKLevel(ultra) unexpectedly accepted")
	}
}

func TestStripANSI(t *testing.T) {
	in := "\x1b[31mred\x1b[0m and \x1b[1;32mbold green\x1b[0m"
	if got := StripANSI(in); got != "red and bold green" {
		t.Errorf("StripANSI = %q", got)
	}
	if got := StripANSI("plain text"); got != "plain text" {
		t.Errorf("StripANSI changed plain text: %q", got)
	}
}

func TestFilterPytest(t *testing.T) {
	out, res := FilterRTKText(pytestLog, RTKLevelStandard, "pytest")
	if !res.Applied || res.PackID != "test-pytest" {
		t.Fatalf("Applied=%v PackID=%q", res.Applied, res.PackID)
	}
	assertFiltered(t, "pytest", pytestLog, out,
		[]string{"FAILED tests/test_a.py::test_a", "Traceback (most recent call last):",
			"E assert 1 == 2", "1 failed, 2 passed"},
		[]string{"....F....", "collected 3 items"})
}

func TestFilterGoTestLevels(t *testing.T) {
	std, res := FilterRTKText(goLog, RTKLevelStandard, "go test ./...")
	if !res.Applied || res.PackID != "test-go" {
		t.Fatalf("standard: Applied=%v PackID=%q", res.Applied, res.PackID)
	}
	assertFiltered(t, "go/standard", goLog, std,
		[]string{"--- FAIL: TestB", "b_test.go:12: boom", "FAIL\t./pkg", "ok  \t./other"},
		[]string{"=== RUN"})
	// Standard collapses the two --- PASS lines to one.
	if n := strings.Count(std, "--- PASS:"); n != 1 {
		t.Errorf("standard: --- PASS lines = %d, want 1 (collapsed run)", n)
	}

	agg, _ := FilterRTKText(goLog, RTKLevelAggressive, "go test ./...")
	assertFiltered(t, "go/aggressive", goLog, agg,
		[]string{"--- FAIL: TestB", "FAIL\t./pkg"},
		[]string{"--- PASS:", "=== RUN"})

	min, _ := FilterRTKText(goLog, RTKLevelMinimal, "go test ./...")
	// Minimal drops no content lines: both PASS lines survive.
	if n := strings.Count(min, "--- PASS:"); n != 2 {
		t.Errorf("minimal: --- PASS lines = %d, want 2 (nothing dropped)", n)
	}
	if strings.Contains(min, "=== RUN") == false {
		t.Errorf("minimal: === RUN lines dropped, want kept")
	}
}

func TestFilterJest(t *testing.T) {
	out, res := FilterRTKText(jestLog, RTKLevelStandard, "npm test")
	if res.PackID != "test-jest" {
		t.Fatalf("PackID=%q, want test-jest", res.PackID)
	}
	assertFiltered(t, "jest", jestLog, out,
		[]string{"FAIL src/c.test.ts", "✕ renders the widget", "PASS src/a.test.ts",
			"Test Suites: 1 failed, 2 passed, 3 total", "Tests:       1 failed, 4 passed, 5 total"},
		[]string{"PASS src/b.test.ts", "✓ renders the header"})
	// Standard collapses the consecutive PASS run to its first line.
	if n := strings.Count(out, "PASS src/"); n != 1 {
		t.Errorf("standard: PASS lines = %d, want 1 (collapsed run)", n)
	}
}

func TestFilterNPMInstall(t *testing.T) {
	out, res := FilterRTKText(npmLog, RTKLevelStandard, "npm install")
	if res.PackID != "npm-install" {
		t.Fatalf("PackID=%q, want npm-install", res.PackID)
	}
	assertFiltered(t, "npm", npmLog, out,
		[]string{"added 3 packages", "audited 3 packages", "found 0 vulnerabilities"},
		[]string{"\x1b[", "Progress: 45%", "[####------]", "npm notice", "npm http fetch"})
}

func TestFilterANSIProgress(t *testing.T) {
	std, res := FilterRTKText(ansiProgressLog, RTKLevelStandard, "")
	if !res.Applied {
		t.Fatal("standard: ANSI/progress log not changed")
	}
	assertFiltered(t, "ansi-progress", ansiProgressLog, std,
		[]string{"added 3 packages"},
		[]string{"\x1b[", "⠋", "⠙", "Downloading foo", "[====>   ]"})
	// The \r overwrite keeps only the final segment, and at
	// minimal the progress run collapses to its LAST line
	// (the terminal's final state), not to the spinner.
	min, _ := FilterRTKText(ansiProgressLog, RTKLevelMinimal, "")
	if n := strings.Count(min, "Installing packages..."); n != 0 {
		t.Errorf("minimal: spinner lines = %d, want 0 (superseded by later progress)", n)
	}
	if !strings.Contains(min, "[====>   ] 50%") {
		t.Errorf("minimal: last progress state missing:\n%s", min)
	}
	if strings.Contains(min, "Downloading foo 10%") {
		t.Errorf("minimal: superseded \\r segment (10%%) survived:\n%s", min)
	}
	if strings.Contains(min, "\x1b[") {
		t.Errorf("minimal: ANSI escapes survive:\n%s", min)
	}
}

func TestFilterCarriageReturn(t *testing.T) {
	// A \r overwrite resolves to the final segment only, and
	// at minimal that final segment is the progress run's last
	// line, so it survives.
	in := "Downloading foo 10%\rDownloading foo 90%\nadded 3 packages\n"
	out, _ := FilterRTKText(in, RTKLevelMinimal, "")
	if strings.Contains(out, "10%") {
		t.Errorf("superseded \\r segment (10%%) survived:\n%s", out)
	}
	if !strings.Contains(out, "Downloading foo 90%") {
		t.Errorf("final \\r segment (90%%) missing:\n%s", out)
	}
}

func TestFilterJSONL(t *testing.T) {
	in := jsonlLog()
	out, res := FilterRTKText(in, RTKLevelStandard, "")
	if res.PackID != "json-output" {
		t.Fatalf("PackID=%q, want json-output", res.PackID)
	}
	assertFiltered(t, "jsonl", in, out,
		[]string{`"level":"error"`, "db connection failed", `"level":"warn"`, "retrying connection"},
		[]string{`"level":"info"`, "starting server"})
	// Minimal keeps the info lines: only ANSI/dedup apply.
	min, _ := FilterRTKText(in, RTKLevelMinimal, "")
	if n := strings.Count(min, `"level":"info"`); n != 10 {
		t.Errorf("minimal: info lines = %d, want 10", n)
	}
}

func TestFilterSingleJSONBlobUnchanged(t *testing.T) {
	out, res := FilterRTKText(singleJSONBlob, RTKLevelAggressive, "jq .")
	if res.Applied || out != singleJSONBlob {
		t.Errorf("single JSON blob was rewritten:\n%s", out)
	}
}

func TestFilterStackTraceFailureAtEnd(t *testing.T) {
	in := stackTraceLog()
	out, res := FilterRTKText(in, RTKLevelAggressive, "")
	if !res.Applied {
		t.Fatal("aggressive: 200-frame trace not changed")
	}
	if res.OmittedLines == 0 {
		t.Errorf("aggressive: expected head/tail truncation (OmittedLines > 0)")
	}
	assertFiltered(t, "stacktrace", in, out,
		[]string{"Error: boom", "Caused by: bad input", "lines omitted by RTK"},
		[]string{})
	// The failure is at the very end and must survive even
	// though it sits in the truncated tail region's middle.
	if !strings.Contains(out, "Error: boom") {
		t.Errorf("failure at end was lost:\n%s", out)
	}
	// Head/tail 16/16 at aggressive: output stays small.
	if n := len(strings.Split(out, "\n")); n > 80 {
		t.Errorf("aggressive trace output = %d lines, want <= 80", n)
	}
}

func TestFilterGitStatus(t *testing.T) {
	in := "On branch main\nYour branch is up to date.\n\nChanges not staged for commit:\n  (use \"git add <file>...\" to update what will be committed)\n  (use \"git restore <file>...\" to discard changes)\n\n\tmodified:   main.go\n\nUntracked files:\n  (use \"git add <file>...\" to include in what will be committed)\n\n\tnotes.txt\n\nno changes added to commit (use \"git add\" and/or \"git commit -a\")\n"
	out, res := FilterRTKText(in, RTKLevelStandard, "git status")
	if res.PackID != "git-status" {
		t.Fatalf("PackID=%q, want git-status", res.PackID)
	}
	assertFiltered(t, "git-status", in, out,
		[]string{"On branch main", "modified:   main.go", "Untracked files:"},
		[]string{`(use "git add <file>`, `(use "git restore <file>`})
}

func TestFilterPipKeepsURLIntact(t *testing.T) {
	out, res := FilterRTKText(pipLog, RTKLevelStandard, "pip install beta")
	if res.PackID != "pip-install" {
		t.Fatalf("PackID=%q, want pip-install", res.PackID)
	}
	assertFiltered(t, "pip", pipLog, out,
		[]string{"Successfully installed beta", "https://example.com/docs/beta"},
		[]string{"Collecting https://files.example.com", "Requirement already satisfied"})
}

func TestFilterCleanAndProseUnchanged(t *testing.T) {
	for name, in := range map[string]string{"clean": cleanOutput, "prose": proseText} {
		out, res := FilterRTKText(in, RTKLevelAggressive, "")
		if res.Applied || out != in {
			t.Errorf("%s: clean input changed:\n%s", name, out)
		}
		if IsToolOutput(in) {
			t.Errorf("%s: IsToolOutput true for clean input", name)
		}
	}
}

func TestFilterDisabledLevelIsBypass(t *testing.T) {
	// Toggle evidence: the disabled level is the bypass — it
	// must return the input byte-identical, while the same
	// input under standard is filtered.
	raw, res := FilterRTKText(pytestLog, "", "pytest")
	if res.Applied || raw != pytestLog {
		t.Errorf("disabled level changed input")
	}
	std, _ := FilterRTKText(pytestLog, RTKLevelStandard, "pytest")
	if std == pytestLog {
		t.Errorf("standard level left pytest log unchanged")
	}
	if _, ok := ParseRTKLevel("bogus"); ok {
		t.Errorf("bogus level parsed as valid")
	}
	bogus, res := FilterRTKText(pytestLog, RTKLevel("bogus"), "pytest")
	if res.Applied || bogus != pytestLog {
		t.Errorf("bogus level changed input")
	}
}

func TestFilterFencedBlockPreserved(t *testing.T) {
	in := "Some tool output:\n```go\n\x1b[31mred code\x1b[0m\nline2\n```\nFAIL something\n"
	out, _ := FilterRTKText(in, RTKLevelAggressive, "")
	// The fence block survives byte-identical (ANSI inside a
	// fence is NOT stripped, so CheckLossy sees the same block).
	if !strings.Contains(out, "\x1b[31mred code\x1b[0m") {
		t.Errorf("fenced block was altered:\n%s", out)
	}
	if !strings.Contains(out, "FAIL something") {
		t.Errorf("failure line lost:\n%s", out)
	}
}

func TestFilterIdempotent(t *testing.T) {
	for name, in := range map[string]string{
		"pytest": pytestLog, "go": goLog, "npm": npmLog,
		"jsonl": jsonlLog(), "trace": stackTraceLog(),
	} {
		once, _ := FilterRTKText(in, RTKLevelStandard, "")
		twice, res := FilterRTKText(once, RTKLevelStandard, "")
		if res.Applied || twice != once {
			t.Errorf("%s: second pass changed output (not idempotent):\n%s", name, twice)
		}
	}
}

func TestFilterMessages(t *testing.T) {
	toolMsg := provider.Message{Role: "tool", Content: pytestLog}
	userMsg := provider.Message{Role: "user", Content: pytestLog}
	partMsg := provider.Message{
		Role: "tool", Content: pytestLog,
		Parts: []provider.ContentPart{{Type: "text", Text: pytestLog}},
	}
	msgs := []provider.Message{userMsg, toolMsg, partMsg}
	out, res := FilterRTKMessages(msgs, RTKLevelStandard)
	if !res.Applied {
		t.Fatal("FilterRTKMessages reported not applied")
	}
	if out[0].Content != pytestLog {
		t.Errorf("user message was filtered (must be untouched)")
	}
	if out[1].Content == pytestLog || !strings.Contains(out[1].Content, "FAILED tests/test_a.py") {
		t.Errorf("tool message not filtered correctly:\n%s", out[1].Content)
	}
	if out[2].Content != pytestLog {
		t.Errorf("part-carrying tool message was rewritten (must be kept byte-identical)")
	}
	if len(res.PackIDs) != 1 || res.PackIDs[0] != "test-pytest" {
		t.Errorf("PackIDs = %v, want [test-pytest]", res.PackIDs)
	}
	// Disabled level: bypass, everything unchanged.
	same, res := FilterRTKMessages(msgs, "")
	if res.Applied || len(same) != len(msgs) {
		t.Errorf("disabled FilterRTKMessages applied changes")
	}
	for i := range msgs {
		if same[i].Content != msgs[i].Content {
			t.Errorf("disabled level changed message %d", i)
		}
	}
	// Input slice is not mutated.
	if msgs[1].Content != pytestLog {
		t.Errorf("FilterRTKMessages mutated its input slice")
	}
}

func TestToggleEvidence(t *testing.T) {
	// Explicit toggle: every behaviour below FAILS on the raw
	// input and PASSES on the filtered output. If the filter
	// were bypassed (identity function), this test fails.
	cases := []struct {
		name    string
		in      string
		command string
		check   func(out string) bool
	}{
		{"ansi gone", ansiProgressLog, "", func(o string) bool { return !strings.Contains(o, "\x1b[") }},
		{"progress gone", npmLog, "npm install", func(o string) bool { return !strings.Contains(o, "Progress:") }},
		{"dots gone", pytestLog, "pytest", func(o string) bool { return !strings.Contains(o, "....F....") }},
		{"run lines gone", goLog, "go test", func(o string) bool { return !strings.Contains(o, "=== RUN") }},
		{"info json gone", jsonlLog(), "", func(o string) bool { return !strings.Contains(o, `"level":"info"`) }},
		{"failure kept", pytestLog, "pytest", func(o string) bool {
			return strings.Contains(o, "FAILED tests/test_a.py") && !strings.Contains(o, "....F....")
		}},
	}
	for _, tc := range cases {
		if tc.check(tc.in) {
			t.Errorf("%s: raw input already passes the check — no toggle", tc.name)
		}
		out, _ := FilterRTKText(tc.in, RTKLevelStandard, tc.command)
		if !tc.check(out) {
			t.Errorf("%s: filtered output fails the check:\n%s", tc.name, out)
		}
	}
}

func TestRTKOutputPassesFidelityGate(t *testing.T) {
	// Every corpus case, filtered at standard and aggressive,
	// must pass CheckLossy against the original: no fence, JSON
	// value or URL cut mid-string, no spliced unit.
	corpora := map[string]string{
		"pytest": pytestLog, "go": goLog, "jest": jestLog,
		"npm": npmLog, "pip": pipLog, "jsonl": jsonlLog(),
		"trace": stackTraceLog(), "ansi-progress": ansiProgressLog,
		"clean": cleanOutput, "single-json": singleJSONBlob,
	}
	for name, in := range corpora {
		for _, level := range []RTKLevel{RTKLevelStandard, RTKLevelAggressive} {
			out, _ := FilterRTKText(in, level, "")
			orig := []provider.Message{{Role: "tool", Content: in}}
			comp := []provider.Message{{Role: "tool", Content: out}}
			if ok, reasons := CheckLossy(orig, comp); !ok {
				t.Errorf("%s/%s: CheckLossy failed: %v\noutput:\n%s", name, level, reasons, out)
			}
		}
	}
}

func TestRTKMeasuredSavings(t *testing.T) {
	// Measured savings only — no headline claim. Each noisy
	// corpus must shrink materially at standard; the clean
	// corpus must not shrink at all.
	cases := []struct {
		name string
		in   string
		min  float64 // minimum fractional char saving
		max  float64 // maximum (sanity: never claims > 95%)
	}{
		{"pytest", pytestLog, 0.05, 0.95},
		{"go", goLog, 0.20, 0.95},
		{"npm", npmLog, 0.20, 0.95},
		{"jsonl", jsonlLog(), 0.50, 0.95},
		{"trace", stackTraceLog(), 0.50, 0.99},
	}
	for _, tc := range cases {
		out, res := FilterRTKText(tc.in, RTKLevelStandard, "")
		saved := 1 - float64(len(out))/float64(len(tc.in))
		t.Logf("%s: %d -> %d chars (%.1f%% saved, pack %s, lines %d->%d)",
			tc.name, len(tc.in), len(out), saved*100, res.PackID, res.LinesIn, res.LinesOut)
		if saved < tc.min || saved > tc.max {
			t.Errorf("%s: saving %.1f%% outside [%.0f%%, %.0f%%]", tc.name, saved*100, tc.min*100, tc.max*100)
		}
	}
	out, _ := FilterRTKText(cleanOutput, RTKLevelStandard, "")
	if out != cleanOutput {
		t.Errorf("clean output shrank/changed")
	}
}

// M7.4-fix regression tests (re-review defects): failure lines
// lost mid-truncation, minimal truncating, DroppedLines off by
// one. Each test was written FIRST and shown FAILING against
// the pre-fix code before any fix landed.

// TestRTKFailureModuleNotFoundErrorKeptMidTruncation is repro 1:
// "ModuleNotFoundError: No module named 'requests'" as the
// terminal line of a 200-frame Python traceback at aggressive.
// Pre-fix it is lost: failurePatterns has no word boundary
// before "Error" in "ModuleNotFoundError", so isFailureLine is
// false and truncateHeadTail drops it.
func TestRTKFailureModuleNotFoundErrorKeptMidTruncation(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("Traceback (most recent call last):\n")
	for i := 0; i < 100; i++ {
		sb.WriteString("  File \"/app/module_" + itoa(i) + ".py\", line " + itoa(i+1) + ", in run_test\n")
	}
	sb.WriteString("ModuleNotFoundError: No module named 'requests'\n")
	for i := 100; i < 200; i++ {
		sb.WriteString("  File \"/app/module_" + itoa(i) + ".py\", line " + itoa(i+1) + ", in run_test\n")
	}
	in := sb.String()
	out, res := FilterRTKText(in, RTKLevelAggressive, "")
	if res.OmittedLines == 0 {
		t.Fatalf("expected head/tail truncation (OmittedLines > 0), got none; lines in=%d out=%d", res.LinesIn, res.LinesOut)
	}
	if !strings.Contains(out, "ModuleNotFoundError: No module named 'requests'") {
		t.Errorf("mid-truncation failure line lost at aggressive (203->%d lines, %d omitted):\n%s", res.LinesOut, res.OmittedLines, out)
	}
}

// TestRTKFailureNpmErrKeptMidTruncation is repro 2: "npm ERR!
// code E404" in the MIDDLE of a 205-line log at standard.
// Pre-fix it is lost: "ERR!" is not in failurePatterns, so
// isFailureLine is false and truncateHeadTail drops it — even
// though the matched npm-install pack's own Keep lists ERR!
// (the fix puts ERR! in the failure vocabulary, which is the
// rescue rule truncation consults).
func TestRTKFailureNpmErrKeptMidTruncation(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 102; i++ {
		sb.WriteString("installing dependency pkg" + itoa(i) + " version 1.2." + itoa(i) + "\n")
	}
	sb.WriteString("npm ERR! code E404\n")
	sb.WriteString("npm ERR! 404 Not Found - GET https://registry.npmjs.org/pkg-missing - Not found\n")
	for i := 102; i < 204; i++ {
		sb.WriteString("installing dependency pkg" + itoa(i) + " version 1.2." + itoa(i) + "\n")
	}
	in := sb.String()
	out, res := FilterRTKText(in, RTKLevelStandard, "npm install")
	if res.PackID != "npm-install" {
		t.Fatalf("PackID=%q, want npm-install (repro needs the pack matched)", res.PackID)
	}
	if res.OmittedLines == 0 {
		t.Fatalf("expected head/tail truncation (OmittedLines > 0), got none; lines in=%d out=%d", res.LinesIn, res.LinesOut)
	}
	if !strings.Contains(out, "npm ERR! code E404") {
		t.Errorf("mid-truncation failure line lost at standard (206->%d lines, %d omitted):\n%s", res.LinesOut, res.OmittedLines, out)
	}
}

// TestRTKFailureSegfaultKeptMidTruncation is repro 3:
// "Segmentation fault (core dumped)" in the MIDDLE of a
// 200-line log at aggressive. Pre-fix it is lost: the phrase
// is not in failurePatterns at all. The leading progress line
// is what marks the log as tool output (it is dropped at
// aggressive, the failure must still survive).
func TestRTKFailureSegfaultKeptMidTruncation(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[##########] 100%\n")
	for i := 0; i < 100; i++ {
		sb.WriteString("processing data chunk " + itoa(i) + " from input stream\n")
	}
	sb.WriteString("Segmentation fault (core dumped)\n")
	for i := 100; i < 199; i++ {
		sb.WriteString("processing data chunk " + itoa(i) + " from input stream\n")
	}
	in := sb.String()
	out, res := FilterRTKText(in, RTKLevelAggressive, "")
	if res.OmittedLines == 0 {
		t.Fatalf("expected head/tail truncation (OmittedLines > 0), got none; lines in=%d out=%d", res.LinesIn, res.LinesOut)
	}
	if !strings.Contains(out, "Segmentation fault (core dumped)") {
		t.Errorf("mid-truncation failure line lost at aggressive (200->%d lines, %d omitted):\n%s", res.LinesOut, res.OmittedLines, out)
	}
}

// TestRTKMinimalDropsNothing: minimal's doc comment promises it
// "drops no content lines", but pre-fix maxLines: 200 truncated
// a 251-line distinct-lines input to ~49 lines. Minimal must
// be truly uncapped: no truncation, every content line kept.
func TestRTKMinimalDropsNothing(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 251; i++ {
		sb.WriteString("build output line " + itoa(i) + ": module-" + itoa(i) + " compiled\n")
	}
	in := sb.String()
	out, res := FilterRTKText(in, RTKLevelMinimal, "npm install")
	if res.OmittedLines != 0 {
		t.Errorf("minimal: OmittedLines = %d, want 0 (minimal must not truncate)", res.OmittedLines)
	}
	// 251 content lines; the trailing blank from the final
	// newline is dropped by the line pass, so LinesOut is 251.
	if res.LinesOut != 251 {
		t.Errorf("minimal: LinesOut = %d, want 251 (nothing dropped)", res.LinesOut)
	}
	for i := 0; i < 251; i++ {
		want := "build output line " + itoa(i) + ": module-" + itoa(i) + " compiled"
		if !strings.Contains(out, want) {
			t.Errorf("minimal: content line %d lost:\n%s", i, out)
			break
		}
	}
}

// TestRTKDroppedLinesCountsMarker: DroppedLines undercounted by
// exactly 1 whenever a truncation marker was inserted (the
// marker line counts as kept). Input has no trailing newline so
// LinesIn is exact and no other line is dropped: every dropped
// line is an omitted middle line, so DroppedLines must equal
// OmittedLines.
func TestRTKDroppedLinesCountsMarker(t *testing.T) {
	in := strings.TrimSuffix(stackTraceLog(), "\n")
	_, res := FilterRTKText(in, RTKLevelAggressive, "")
	if res.LinesIn != 203 {
		t.Fatalf("LinesIn = %d, want 203 (test setup)", res.LinesIn)
	}
	if res.OmittedLines != 169 {
		t.Fatalf("OmittedLines = %d, want 169 (test setup: 203 kept, head/tail 16/16, 2 priority lines rescued)", res.OmittedLines)
	}
	// Every output line except the marker is one input line, so
	// dropped = in - (out - 1).
	want := res.LinesIn - res.LinesOut + 1
	if res.DroppedLines != want {
		t.Errorf("DroppedLines = %d, want %d (LinesIn %d - LinesOut %d + 1 marker line)", res.DroppedLines, want, res.LinesIn, res.LinesOut)
	}
	if res.DroppedLines != res.OmittedLines {
		t.Errorf("DroppedLines = %d, want OmittedLines = %d (no other drops in this corpus)", res.DroppedLines, res.OmittedLines)
	}
}

// TestRTKKilledLineStartOnly: the "killed" failure pattern is
// line-start anchored — a bare shell "Killed" line (OOM-killer /
// signal 137) is rescued mid-truncation, but benign prose that
// merely contains the word ("the process was killed by the user
// request") is not. Re-review nit: the unanchored (?i)\bkilled\b
// over-rescued benign lines (retention, not loss).
func TestRTKKilledLineStartOnly(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[##########] 100%\n")
	for i := 0; i < 100; i++ {
		sb.WriteString("processing data chunk " + itoa(i) + " from input stream\n")
	}
	sb.WriteString("Killed\n")
	sb.WriteString("the process was killed by the user request\n")
	for i := 100; i < 199; i++ {
		sb.WriteString("processing data chunk " + itoa(i) + " from input stream\n")
	}
	in := sb.String()
	out, res := FilterRTKText(in, RTKLevelAggressive, "")
	if res.OmittedLines == 0 {
		t.Fatalf("expected head/tail truncation (OmittedLines > 0), got none; lines in=%d out=%d", res.LinesIn, res.LinesOut)
	}
	if !strings.Contains(out, "Killed\n") {
		t.Errorf("bare Killed line lost mid-truncation (200->%d lines, %d omitted):\n%s", res.LinesOut, res.OmittedLines, out)
	}
	if strings.Contains(out, "the process was killed by the user request") {
		t.Errorf("benign mid-sentence killed line rescued (over-rescue):\n%s", out)
	}
}
