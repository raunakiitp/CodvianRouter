// M7.4 — RTK tool-output filter.
//
// Command-aware, extractive-only compression for terminal / tool
// output (test logs, build output, package-manager noise, shell
// transcripts, Docker output, git output, stack traces, JSONL
// logs). It drops or strips only provably redundant material —
// ANSI escapes, carriage-return progress overwrites, progress
// lines, repeated pass lines, verbose runner chatter, info-level
// JSONL lines — and NEVER rewrites prose, never cuts a line
// mid-string, and never drops a failure or a summary line.
//
// What was studied (the sector baseline reference clone, study only — no
// code copied):
//
//   - docs/compression/RTK_COMPRESSION.md — the 3 intensity
//     levels (minimal/standard/aggressive; head/tail 24/24/16),
//     the "60-90%" upstream savings claim, the 56 built-in
//     filter packs, the stripAnsi -> replace -> drop/include ->
//     head/tail/maxLines pipeline, dedup threshold 3, and the
//     rule that errors / stack traces / test failures are
//     preserved at every level.
//   - the earlier implementation
//     (493 lines) — command-prefix + content-pattern detection,
//     shell-tool-only gating.
//   - the earlier implementation
//     (756 lines) — engine pipeline, smartTruncate head/tail
//     with priority (severity) patterns, engine-wide dedup.
//   - the earlier implementation
//     (244 lines) — stripAnsi, drop/include/collapse patterns,
//     head/tail/maxLines per filter.
//   - the earlier implementation*.json
//     (56 files) — e.g. test-pytest.json, test-go.json,
//     npm-install.json, json-output.json,
//     error-stacktrace.json, generic-output.json.
//
// Honest coverage: this file implements 18 packs, NOT 56.
// RTKPackCount reports the real number, and every pack is
// reachable through DetectRTKPack (proved by the detection
// table test). The remaining 38 the sector baseline packs (aws, gcloud,
// kubectl, gradle, turbo, nx, systemctl, terraform beyond
// plan, and the rest) are NOT implemented; claiming 56 would
// be fake. No savings percentage is claimed here either —
// measured savings per corpus case are logged by the tests,
// not asserted as a headline.
//
// Safety properties (the M7.2 fidelity gate's spirit):
//
//   - Whole lines only. A line is kept verbatim or dropped
//     whole; no line is ever truncated mid-string, so a URL or
//     JSON value is never cut mid-string and no spliced value
//     is ever produced. (Deliberate divergence from the sector-typical
//     truncateLineAt: per-line truncation is NOT implemented.)
//   - Fenced code blocks are kept byte-identical, ANSI and
//     all: ANSI stripping and \r resolution run only outside
//     fences, so a fence block in the output matches an
//     original fence block exactly for CheckLossy.
//   - A single JSON value (pretty-printed blob) passes through
//     untouched apart from outside-fence ANSI stripping —
//     dropping interior lines of one JSON value would cut it
//     mid-structure. JSONL (one JSON object per line) is the
//     only JSON shape whose lines are dropped, and only whole
//     lines, only info/debug/trace level, only at
//     standard/aggressive.
//   - Failures and summaries are ALWAYS kept, at every level,
//     including in the middle of a head/tail truncation.
//   - Idempotent: filtering already-filtered output changes
//     nothing further (proved by test).
//
// Wiring (coordinator's job, NOT done here): this file does not
// touch Apply. The intended wiring is: in Apply, for tool-role
// messages (and only those — never user/assistant prose, never
// part-carrying messages), call FilterRTKMessages with the
// configured level before or as part of the lossy step, then
// run CheckLossy on the result and send the original on any
// doubt, exactly as the lossy path already does. One known
// gate interaction: CheckLossy is strict about JSON fragments,
// so a JSONL corpus whose dropped lines share long substrings
// with kept lines can still trip it in edge cases — the tests
// prove the shipped corpora pass; if a future corpus trips
// the gate, the safe outcome is the original being sent.
package compress

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// RTKLevel is an RTK intensity level, mirroring the sector-typical
// minimal / standard / aggressive. The zero value ("") means
// disabled: FilterRTKText returns its input unchanged.
type RTKLevel string

const (
	// RTKLevelMinimal strips ANSI and collapses progress and
	// duplicate runs, but drops no content lines. Lowest risk.
	RTKLevelMinimal RTKLevel = "minimal"
	// RTKLevelStandard is the default: progress lines, verbose
	// runner chatter, routine pass lines (collapsed) and
	// info-level JSONL lines are dropped; head/tail 24/24.
	RTKLevelStandard RTKLevel = "standard"
	// RTKLevelAggressive drops routine pass lines entirely and
	// truncates harder (head/tail 16/16, max 80 lines). Failures
	// and summaries are still always kept.
	RTKLevelAggressive RTKLevel = "aggressive"
)

// ParseRTKLevel parses a level name (case-insensitive). The
// empty string parses as disabled ("", false is not returned —
// "" is a valid disabled level). Unknown names return false.
func ParseRTKLevel(s string) (RTKLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", true
	case "minimal":
		return RTKLevelMinimal, true
	case "standard":
		return RTKLevelStandard, true
	case "aggressive":
		return RTKLevelAggressive, true
	default:
		return "", false
	}
}

// rtkLevelConfig is the per-level behaviour table.
type rtkLevelConfig struct {
	maxLines     int // head/tail truncation cap (0 = no cap)
	headLines    int
	tailLines    int
	dropProgress bool // drop progress lines (false = collapse runs to last)
	dropPack     bool // apply pack Drop patterns
	dropJSONInfo bool // drop info/debug/trace JSONL lines
	routinePass  int  // 0 keep, 1 collapse runs, 2 drop
}

func (l RTKLevel) config() rtkLevelConfig {
	switch l {
	case RTKLevelMinimal:
		// maxLines 0 = no cap (see the field doc): minimal
		// drops no content lines, so it must never truncate.
		return rtkLevelConfig{maxLines: 0, headLines: 24, tailLines: 24}
	case RTKLevelAggressive:
		return rtkLevelConfig{maxLines: 80, headLines: 16, tailLines: 16, dropProgress: true, dropPack: true, dropJSONInfo: true, routinePass: 2}
	default: // standard, and the default for any other value
		return rtkLevelConfig{maxLines: 120, headLines: 24, tailLines: 24, dropProgress: true, dropPack: true, dropJSONInfo: true, routinePass: 1}
	}
}

// RTKPack is one command-output filter pack: a command-output
// family with its detection signals and its line rules. Keep
// patterns are advisory (failure/summary lines are kept
// regardless — see isFailureLine/isSummaryLine); Drop patterns
// name lines that are redundant once the summary survives;
// Collapse patterns name lines of which only the first of a
// consecutive run is kept (e.g. library stack frames).
type RTKPack struct {
	ID       string
	Category string
	Commands []string // command prefixes, e.g. "go test", "git status"
	Signals  []*regexp.Regexp
	Keep     []*regexp.Regexp
	Drop     []*regexp.Regexp
	Collapse []*regexp.Regexp
}

func re(patterns ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		// (?m) so ^/$ anchor per line: Signals are matched
		// against whole multi-line text, while Keep/Drop/
		// Collapse are matched per line — multiline mode is
		// correct (and harmless) for both.
		out = append(out, regexp.MustCompile("(?m)"+p))
	}
	return out
}

// rtkPacks is the full implemented catalog: 18 packs. Count it
// with RTKPackCount — do not write the number anywhere else.
var rtkPacks = []RTKPack{
	{
		ID: "git-status", Category: "git", Commands: []string{"git status"},
		Signals: re(`^On branch `, `^Changes (not staged|to be committed)`, `^Untracked files:`, `^nothing to commit`),
		Keep:    re(`^On branch `, `^Changes `, `^Untracked files:`, `nothing to commit`, `^\s+(modified|new file|deleted|renamed):`),
		Drop:    re(`^\s*\(use "git `, `^\s*\(use "git add`),
	},
	{
		ID: "git-diff", Category: "git", Commands: []string{"git diff", "git show"},
		Signals: re(`^diff --git `, `^@@\s+-\d+,\d+\s+\+\d+,\d+\s+@@`),
		Keep:    re(`^diff --git `, `^@@ `, `^\+[^+]`, `^-[^-]`),
		Drop:    re(`^index [0-9a-f]+\.\.[0-9a-f]+`),
	},
	{
		ID: "git-log", Category: "git", Commands: []string{"git log"},
		Signals: re(`^commit [0-9a-f]{7,40}$`, `^Author: `),
		Keep:    re(`^commit `, `^Author: `, `^Date: `),
	},
	{
		ID: "git-branch", Category: "git", Commands: []string{"git branch", "git checkout", "git switch"},
		Signals: re(`^\*\s+\S+`, `Switched to (a new )?branch`, `Already on `),
		Keep:    re(`^\*\s+\S+`, `Switched to `, `Already on `),
	},
	{
		ID: "test-pytest", Category: "test", Commands: []string{"pytest", "python -m pytest"},
		Signals: re(`^FAILED `, `^ERROR `, `^E\s+`, `=+ short test summary info =+`, `=+ .*failed`, `Traceback \(most recent call last\)`),
		Keep:    re(`^FAILED `, `^ERROR `, `^E\s+`, `Traceback`, `AssertionError`, `=+ .*failed`, `=+ short test summary info =+`),
		Drop:    re(`^[.FEsx]+$`, `^collected \d+ items`),
	},
	{
		ID: "test-go", Category: "test", Commands: []string{"go test"},
		Signals: re(`^--- FAIL:`, `^(ok|FAIL)\s+\S+`, `^=== RUN`, `^panic:`),
		Keep:    re(`^--- FAIL:`, `^FAIL\s`, `^ok\s+`, `^panic:`, `^\s+\S+_test\.go:\d+`, `^\s*Error`),
		Drop:    re(`^=== RUN`, `^=== PAUSE`, `^=== CONT`),
	},
	{
		ID: "test-jest", Category: "test", Commands: []string{"jest", "vitest", "npm test", "npx jest"},
		Signals: re(`^FAIL `, `^PASS `, `^Test Suites:`, `^Tests:\s+\d+ passed`, `^\s*[✕✓] `),
		Keep:    re(`^FAIL `, `^Test Suites:`, `^Tests:`, `✕`, `●.*›`),
		Drop:    re(`^\s*✓ `),
	},
	{
		ID: "test-cargo", Category: "test", Commands: []string{"cargo test"},
		Signals: re(`^test \S+ \.\.\. (ok|FAILED)`, `^running \d+ tests`, `^test result: `),
		Keep:    re(`FAILED`, `^test result: `, `^failures:`),
		Drop:    re(`^running \d+ tests`),
	},
	{
		ID: "test-phpunit", Category: "test", Commands: []string{"phpunit", "vendor/bin/phpunit"},
		Signals: re(`^FAILURES!`, `^OK \(`, `^Tests: \d+, Assertions`, `^\d+\) `),
		Keep:    re(`^FAILURES!`, `^OK \(`, `^Tests: `),
		Drop:    re(`^\.+$`, `^Time: `),
	},
	{
		ID: "npm-install", Category: "package", Commands: []string{"npm install", "npm add", "npm update", "pnpm install", "pnpm add", "yarn add"},
		Signals: re(`added \d+ packages`, `audited \d+ packages`, `packages are looking for funding`),
		Keep:    re(`added \d+ packages`, `removed \d+ packages`, `changed \d+ packages`, `audited \d+ packages`, `vulnerabilities`, `WARN`, `ERR!`),
		Drop:    re(`^npm notice`, `^npm http fetch`, `^Progress:`),
	},
	{
		ID: "pip-install", Category: "package", Commands: []string{"pip install", "pip3 install", "uv pip install"},
		Signals: re(`Successfully installed`, `Requirement already satisfied`, `^Collecting `),
		Keep:    re(`Successfully installed`, `ERROR`, `WARNING`),
		Drop:    re(`^Collecting `, `^Requirement already satisfied`, `^Installing collected packages`),
	},
	{
		ID: "build-typescript", Category: "build", Commands: []string{"tsc", "eslint", "biome", "prettier"},
		Signals: re(`error TS\d+:`, `\(\d+,\d+\): error`, `^\S+:\d+:\d+\s+error`),
		Keep:    re(`error TS\d+`, `error`, `warning`),
	},
	{
		ID: "docker-ps", Category: "docker", Commands: []string{"docker ps"},
		Signals: re(`^CONTAINER ID\s+IMAGE\s+COMMAND`, `^CONTAINER ID\s`),
		Keep:    re(`^CONTAINER ID\s`),
	},
	{
		ID: "docker-logs", Category: "docker", Commands: []string{"docker logs"},
		Signals: re(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`),
		Keep:    re(`(?i)\b(error|warn|fatal|panic)\b`),
		Drop:    re(`(?i)\bINFO\b`),
	},
	{
		ID: "terraform-plan", Category: "infra", Commands: []string{"terraform plan", "tofu plan"},
		Signals: re(`^Plan: \d+ to add`, `^Terraform will perform`, `will be (created|destroyed|updated)`),
		Keep:    re(`^Plan: `, `will be (created|destroyed|updated)`, `Error:`),
		Drop:    re(`will be read during apply`, `^Terraform used the selected providers`),
	},
	{
		ID: "json-output", Category: "generic", Commands: []string{"jq"},
		Signals: re(`^\s*\{.*"level"\s*:`, `^\s*\{.*"timestamp"\s*:`),
		Keep:    re(`"level"\s*:\s*"(error|warn|fatal)"`, `"error"\s*:`, `"status"\s*:`),
	},
	{
		ID: "error-stacktrace", Category: "generic",
		Signals:  re(`Traceback \(most recent call last\)`, `^\s+at\s+\S+ \(`, `^panic: `, `^Caused by:`, `^[A-Z][A-Za-z]+(Error|Exception):`),
		Keep:     re(`Error:`, `Exception:`, `Traceback`, `^panic:`, `^Caused by:`, `^\s+at `),
		Collapse: re(`^\s+File ".*(site-packages|dist-packages)/`, `^\s+at\s+.*\(node:internal`, `^\s+at\s+.*node_modules/`),
	},
	{
		ID: "generic-output", Category: "generic",
		Signals: re(`(?i)error:`, `(?i)exception:`, `(?i)\bfailed\b`, `^FAIL\b`),
		Keep:    re(`(?i)error`, `(?i)fail`),
	},
}

// RTKPackCount returns the number of implemented packs (18).
// It is the only place the count is stated; tests pin it.
func RTKPackCount() int { return len(rtkPacks) }

// RTKPackIDs returns the implemented pack IDs in priority order.
func RTKPackIDs() []string {
	ids := make([]string, len(rtkPacks))
	for i, p := range rtkPacks {
		ids[i] = p.ID
	}
	return ids
}

// DetectRTKPack classifies text (with an optional shell command
// such as "go test ./...") and returns the best-matching pack
// and its score. A command-prefix match scores 10; each
// matching content signal scores 1. Score 0 means no pack
// matched. Packs are tried in catalog order, so a more specific
// pack listed earlier wins a tie.
func DetectRTKPack(text, command string) (RTKPack, int) {
	cmd := strings.ToLower(strings.TrimSpace(command))
	// First segment only: "npm test && go test" is an npm test.
	if i := strings.Index(cmd, "&&"); i >= 0 {
		cmd = strings.TrimSpace(cmd[:i])
	}
	best := RTKPack{}
	bestScore := 0
	for _, p := range rtkPacks {
		score := 0
		for _, c := range p.Commands {
			if cmd != "" && (cmd == c || strings.HasPrefix(cmd, c+" ") || strings.HasPrefix(cmd, c+"\t")) {
				score += 10
				break
			}
		}
		for _, s := range p.Signals {
			if s.MatchString(text) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = p, score
		}
	}
	return best, bestScore
}

// --- line classifiers (level-independent; failures always win) ---

var (
	ansiRegexp = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[()][0-9A-Z]|\x1b[=>]")

	failurePatterns = re(
		`(?i)\bfail(ed|ure)?\b`, `(?i)\berror\b`, `(?i)\bpanic\b`, `(?i)traceback`,
		`(?i)\bexception\b`, `(?i)assertionerror`, `(?i)\bfatal\b`, `(?i)\bdenied\b`,
		`(?i)could not\b`, `(?i)unable to\b`, `(?i)not found\b`,
		`^--- FAIL:`, `^FAIL\s`, `^FAILED `, `^E\s+`, `[✗✖]`,
		`^Caused by:`, // failure continuation line, kept with its error
		`"level"\s*:\s*"(error|fatal)"`,
		// Exception-type lines with no word boundary before
		// "Error"/"Exception" (ModuleNotFoundError,
		// SyntaxError, ValueError, ...): \berror\b never
		// matches these, so they need their own pattern.
		`^[A-Za-z][A-Za-z0-9_]*(Error|Exception):`,
		`ERR!`, // npm ERR! code E404 (also in npm-install Keep)
		`(?i)segmentation fault`, `(?i)\bsegfault\b`, `(?i)core dumped`,
		`(?i)^\s*killed\b`, // shell "Killed" at line start (OOM-killer / signal 137); mid-sentence "killed" (benign prose) must NOT rescue
		`exit status [1-9]\d*`, `(?i)exited with (code|status) [1-9]`,
	)
	summaryPatterns = re(
		`^\s*(ok|FAIL)\s+\S+`, `=+.*(passed|failed)`, `^Test Suites:`, `^Tests:`,
		`added \d+ packages`, `audited \d+ packages`, `^BUILD (SUCCESS|FAILED)`,
		`^Plan: \d+ to`, `^On branch `, `^test result: `, `^OK \(`, `^FAILURES!`,
		`Successfully installed`, `^Done in `, `^Found \d+ (error|warning)`,
		`^=+ short test summary info =+`,
	)
	progressPatterns = re(
		`^\s*[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏⢄⢂⢀⡠⡐⡈]\s`,
		`\[[=#>-]+\s*[<#>-]*\]\s*\d+(\.\d+)?%`,
		`^\s*Progress:\s*\d`,
		`^\s*\d+/\d+\s*\[`,
		`(?i)^\s*(downloading|pulling fs layer|extracting|waiting|pull complete|pushing|fetching)\b`,
		`(?i)\bdownloading\b.*\d+(\.\d+)?%`,
	)
	routinePassPatterns = re(
		`^--- PASS:`, `^PASS\s+\S+`, `^PASS `, `^\s*✓ `, `^test \S+ \.\.\. ok$`,
		`^=== RUN`, // runner chatter, redundant once --- FAIL names the test
	)
	jsonInfoLevels = map[string]bool{"info": true, "debug": true, "trace": true}
)

// StripANSI removes terminal escape sequences (CSI, OSC, charset
// switches) from s. It is exported for the coordinator and for
// tests; FilterRTKText applies it only outside fenced blocks.
func StripANSI(s string) string { return ansiRegexp.ReplaceAllString(s, "") }

func isFailureLine(line string) bool {
	for _, p := range failurePatterns {
		if p.MatchString(line) {
			return true
		}
	}
	return false
}

func isSummaryLine(line string) bool {
	for _, p := range summaryPatterns {
		if p.MatchString(line) {
			return true
		}
	}
	return false
}

func isProgressLine(line string) bool {
	for _, p := range progressPatterns {
		if p.MatchString(line) {
			return true
		}
	}
	return false
}

func isRoutinePassLine(line string) bool {
	for _, p := range routinePassPatterns {
		if p.MatchString(line) {
			return true
		}
	}
	return false
}

func matchesAny(line string, pats []*regexp.Regexp) bool {
	for _, p := range pats {
		if p.MatchString(line) {
			return true
		}
	}
	return false
}

// isJSONInfoLine reports whether line is a single JSON object
// whose level is info/debug/trace with no error-ish key. Such
// a line is routine log noise: dropping the whole line can
// never cut a JSON value mid-structure.
func isJSONInfoLine(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "{") || !strings.HasSuffix(t, "}") {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(t), &obj); err != nil {
		return false
	}
	for k := range obj {
		lk := strings.ToLower(k)
		if lk == "error" || lk == "errors" || lk == "fail" || lk == "failure" || lk == "exception" {
			return false
		}
	}
	var level string
	if raw, ok := obj["level"]; ok {
		_ = json.Unmarshal(raw, &level)
	}
	return jsonInfoLevels[strings.ToLower(level)]
}

// isJSONL reports whether at least two lines (and at least half
// of the non-blank lines) each parse as a JSON object.
func isJSONL(lines []string) bool {
	jsonLines, nonBlank := 0, 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		nonBlank++
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "{") {
			var obj map[string]json.RawMessage
			if json.Unmarshal([]byte(t), &obj) == nil {
				jsonLines++
			}
		}
	}
	return jsonLines >= 2 && nonBlank > 0 && jsonLines*2 >= nonBlank
}

// IsToolOutput reports whether text looks like command/tool
// output rather than prose: a pack matches, or it carries ANSI
// escapes, progress lines, failure markers, or JSONL shape.
// Ordinary prose (no signals at all) returns false, so clean
// prose passes through FilterRTKText unchanged.
func IsToolOutput(text string) bool {
	if _, score := DetectRTKPack(text, ""); score > 0 {
		return true
	}
	if ansiRegexp.MatchString(text) {
		return true
	}
	for _, ln := range strings.Split(text, "\n") {
		if isProgressLine(ln) || isFailureLine(ln) {
			return true
		}
	}
	lines := strings.Split(text, "\n")
	return isJSONL(lines)
}

// RTKResult describes one FilterRTKText / FilterRTKMessages run.
type RTKResult struct {
	Level        RTKLevel
	PackID       string   // "" when no pack matched
	PackIDs      []string // packs matched across messages (FilterRTKMessages)
	Applied      bool     // output differs from input
	LinesIn      int
	LinesOut     int
	DroppedLines int
	// OmittedLines is set when head/tail truncation dropped
	// middle lines (the output carries a "... (N lines omitted
	// by RTK)" marker line for those).
	OmittedLines int
}

// FilterRTKText filters one tool-output text at the given level.
// command is the shell command that produced the output ("" when
// unknown — detection then relies on content signals only).
// The disabled level ("") returns text unchanged.
func FilterRTKText(text string, level RTKLevel, command string) (string, RTKResult) {
	res := RTKResult{Level: level}
	if level == "" {
		return text, res
	}
	if _, ok := ParseRTKLevel(string(level)); !ok {
		return text, res
	}
	cfg := level.config()

	// Strip ANSI and resolve \r overwrites, outside fenced
	// blocks only, so fence blocks survive byte-identical.
	lines := stripANSIOutsideFences(text)
	res.LinesIn = len(lines)

	// A single JSON value passes through untouched (apart from
	// the ANSI strip above): dropping interior lines would cut
	// it mid-structure.
	trimmed := strings.TrimSpace(strings.Join(lines, "\n"))
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		out := strings.Join(lines, "\n")
		res.LinesOut = len(lines)
		res.Applied = out != text
		return out, res
	}

	pack, score := DetectRTKPack(text, command)
	if score > 0 {
		res.PackID = pack.ID
	}
	jsonl := isJSONL(lines)
	if score == 0 && !jsonl && !IsToolOutput(text) {
		// Not tool output at all: return the ANSI-stripped
		// text (unchanged when there was no ANSI either).
		out := strings.Join(lines, "\n")
		res.LinesOut = len(lines)
		res.Applied = out != text
		return out, res
	}

	kept := filterRTKLines(lines, pack, score > 0, jsonl, cfg)

	// Head/tail truncation with priority: failure and summary
	// lines in the dropped middle are always kept.
	if cfg.maxLines > 0 && len(kept) > cfg.maxLines {
		var omitted int
		kept, omitted = truncateHeadTail(kept, cfg)
		res.OmittedLines = omitted
	}

	out := strings.Join(kept, "\n")
	res.LinesOut = len(kept)
	res.DroppedLines = res.LinesIn - len(kept)
	if res.OmittedLines > 0 {
		// The omission marker line counts in len(kept) but is
		// not an input line: one more input line was dropped
		// than the raw difference shows.
		res.DroppedLines++
	}
	if res.DroppedLines < 0 {
		res.DroppedLines = 0
	}
	res.Applied = out != text
	return out, res
}

// stripANSIOutsideFences splits text into lines, strips ANSI
// escapes and resolves \r progress overwrites (keeping only
// the segment after the last \r — the terminal's final state)
// on lines outside fenced code blocks, and returns the lines.
func stripANSIOutsideFences(text string) []string {
	raw := strings.Split(text, "\n")
	out := make([]string, len(raw))
	inFence := false
	for i, ln := range raw {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			out[i] = ln
			continue
		}
		if inFence {
			out[i] = ln
			continue
		}
		ln = StripANSI(ln)
		if idx := strings.LastIndex(ln, "\r"); idx >= 0 {
			ln = ln[idx+1:]
		}
		out[i] = ln
	}
	return out
}

// filterRTKLines is the per-line pass. Every decision is
// whole-line keep/drop; no line is ever rewritten.
func filterRTKLines(lines []string, pack RTKPack, hasPack, jsonl bool, cfg rtkLevelConfig) []string {
	kept := make([]string, 0, len(lines))
	inFence := false
	prevBlank := false
	prevLine := ""
	prevRun := 0 // run length of prevLine among kept lines
	lastCollapse := -1
	var pendingProgress string
	flushProgress := func() {
		if pendingProgress != "" {
			kept = append(kept, pendingProgress)
			prevLine, prevRun = pendingProgress, 1
			pendingProgress = ""
		}
	}
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			flushProgress()
			inFence = !inFence
			kept = append(kept, ln)
			prevBlank, prevLine, prevRun, lastCollapse = false, ln, 1, -1
			continue
		}
		if inFence {
			kept = append(kept, ln)
			prevBlank, prevLine, prevRun, lastCollapse = false, ln, 1, -1
			continue
		}
		// Collapse runs of blank lines to at most one.
		if t == "" {
			flushProgress()
			if prevBlank {
				continue
			}
			prevBlank = true
			lastCollapse = -1
			// A blank line is not dedup-counted; record it as
			// the previous line so a repeated blank after a
			// non-blank is still collapsed by prevBlank.
			kept = append(kept, ln)
			prevLine, prevRun = ln, 1
			continue
		}
		prevBlank = false

		// Failures and summaries always survive every drop
		// rule below (but still pass through dedup).
		priority := isFailureLine(ln) || isSummaryLine(ln)

		if !priority {
			// Progress lines: collapse runs to the last one
			// at minimal, drop at standard/aggressive.
			if isProgressLine(ln) {
				if cfg.dropProgress {
					continue
				}
				pendingProgress = ln
				continue
			}
			flushProgress()
			// Pack drop rules (standard/aggressive only).
			if cfg.dropPack && hasPack && matchesAny(ln, pack.Drop) {
				continue
			}
			// Info-level JSONL lines (standard/aggressive).
			if cfg.dropJSONInfo && jsonl && isJSONInfoLine(ln) {
				continue
			}
			// Routine pass lines: keep (minimal), collapse
			// runs to the first (standard), drop (aggressive).
			if isRoutinePassLine(ln) {
				switch cfg.routinePass {
				case 2:
					continue
				case 1:
					if prevLine != "" && isRoutinePassLine(prevLine) {
						continue
					}
				}
			}
			// Pack collapse: keep only the first of a
			// consecutive run matching the same pattern.
			if cfg.dropPack && hasPack && len(pack.Collapse) > 0 {
				hit := -1
				for i, p := range pack.Collapse {
					if p.MatchString(ln) {
						hit = i
						break
					}
				}
				if hit >= 0 {
					if hit == lastCollapse {
						continue
					}
					lastCollapse = hit
				} else {
					lastCollapse = -1
				}
			} else {
				lastCollapse = -1
			}
		} else {
			flushProgress()
			lastCollapse = -1
		}

		// Engine-wide dedup (the sector baseline threshold 3): a run of
		// >= 3 identical consecutive kept lines collapses —
		// the first two are kept, the third and later dropped.
		// (Keeping two, not one, is the conservative reading
		// of "collapses a run of >=3": a pair is never noise
		// by itself.)
		if ln == prevLine {
			prevRun++
			if prevRun >= 3 {
				continue
			}
		} else {
			prevRun = 1
		}
		prevLine = ln
		kept = append(kept, ln)
	}
	flushProgress()
	// Drop leading/trailing blank lines left by the pass.
	for len(kept) > 0 && strings.TrimSpace(kept[0]) == "" {
		kept = kept[1:]
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	return kept
}

// truncateHeadTail keeps the first headLines and last tailLines
// lines, plus every failure/summary line from the middle, and
// inserts one omission marker line between the sections. It
// returns the truncated lines and how many middle lines were
// omitted (0 when nothing was omitted).
//
// NOTE: truncation deliberately consults ONLY the
// level-independent failure/summary vocabulary, NOT the matched
// pack's Keep patterns. Wholesale pack-Keep rescue was tried
// and reverted: the error-stacktrace pack Keeps `^\s+at `, so
// every stack frame was rescued, truncation never fired
// (OmittedLines 0, 203 lines out at aggressive) and the pinned
// stack-trace tests failed. The vocabulary is the rescue rule,
// so any failure shape a pack Keeps must also be in
// failurePatterns (e.g. npm-install's `ERR!` lives there too).
func truncateHeadTail(lines []string, cfg rtkLevelConfig) ([]string, int) {
	if len(lines) <= cfg.maxLines {
		return lines, 0
	}
	head, tail := cfg.headLines, cfg.tailLines
	if head+tail >= len(lines) {
		return lines, 0
	}
	middle := lines[head : len(lines)-tail]
	var priority []string
	omitted := 0
	for _, ln := range middle {
		if isFailureLine(ln) || isSummaryLine(ln) {
			priority = append(priority, ln)
		} else {
			omitted++
		}
	}
	if omitted == 0 {
		return lines, 0
	}
	marker := "... (" + itoa(omitted) + " lines omitted by RTK; failures and summaries kept)"
	out := make([]string, 0, head+len(priority)+tail+1)
	out = append(out, lines[:head]...)
	out = append(out, marker)
	out = append(out, priority...)
	out = append(out, lines[len(lines)-tail:]...)
	return out, omitted
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// FilterRTKMessages filters the tool-role, plain-text messages
// in msgs (command hint "" — content detection only) and
// returns the new slice plus the aggregate result. Non-tool
// messages, part-carrying messages, and empty messages are
// returned untouched. The disabled level ("") returns msgs
// unchanged with Applied=false. The input slice is not mutated.
func FilterRTKMessages(msgs []provider.Message, level RTKLevel) ([]provider.Message, RTKResult) {
	res := RTKResult{Level: level}
	if level == "" {
		return msgs, res
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	seenPacks := map[string]bool{}
	for i, m := range out {
		if m.Role != "tool" || len(m.Parts) > 0 || m.Content == "" {
			continue
		}
		filtered, r := FilterRTKText(m.Content, level, "")
		res.LinesIn += r.LinesIn
		res.LinesOut += r.LinesOut
		res.DroppedLines += r.DroppedLines
		res.OmittedLines += r.OmittedLines
		if r.PackID != "" && !seenPacks[r.PackID] {
			seenPacks[r.PackID] = true
			res.PackIDs = append(res.PackIDs, r.PackID)
		}
		if r.Applied {
			out[i].Content = filtered
			res.Applied = true
		}
	}
	if len(res.PackIDs) == 1 {
		res.PackID = res.PackIDs[0]
	}
	return out, res
}
