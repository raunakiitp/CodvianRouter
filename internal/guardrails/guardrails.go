// Package guardrails is the plugin-style guardrail stage for Codvian
// Router: deterministic PII redaction, secret redaction, and
// prompt-injection pattern detection.
//
// Design (from research-log entries 30, 100, 150, 152 and the
// Respan/GoModel notes — the findings, not their code):
//
//   - a 1.15.2 gateway release leaked prompts AND provider keys to an open log
//     stream when not run with NODE_ENV=production, a setting its own
//     quickstart never set (entry 150). LiteLLM's spend-log token
//     redaction is OFF by default and must be opted into (entry 152).
//     Codvian Router does the opposite: log redaction is the DEFAULT,
//     not an env var or a setting the operator has to discover. Redact
//     (the pure function) always redacts, whatever the toggles say.
//   - Blocking a request is OPT-IN per guardrail (BlockPII /
//     BlockSecrets / BlockInjection, all default off). With every
//     toggle off a request still flows to the provider unchanged; only
//     the log/spend copies are redacted, and the event is counted.
//   - Async-by-default, sync-deny as the opt-in (research-log entry 30):
//     the check is a local regex/substring scan (~microseconds, no LLM
//     call, no network), so it never slows the fast path; a blocked
//     request is refused before any provider is dialled, so a denial
//     can never leak into a fallback chain (entry 26: guardrail
//     denials must be excluded from fallback triggers — here the
//     request never reaches the router at all).
//   - Streaming-safe by construction: the check runs on the full
//     inbound request before streaming starts, never on a stream in
//     flight (research-log entry 30: a streamed body already left cannot
//     be blocked).
//   - Plugin-style: a guardrail is one entry in the pattern table
//     below (kind + label + matcher). Adding a new pattern is adding
//     one table row; the Store, the server wiring and the dashboard
//     never change shape for it.
//
// What is recorded: counts and the most recent events only. An event
// carries the redacted preview (never the matched value), the pattern
// label (e.g. "email", never the address), the source and whether the
// request was blocked. No prompt text, no key material, no matched
// secret is ever stored — the store itself cannot leak what it
// redacted.
//
// This package is a leaf: stdlib only, no internal imports, so the
// server, the ledger (spend logs) and the router can all use it
// without an import cycle.
package guardrails

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Kinds of guardrail finding. The kind is the label used in metrics,
// dashboard counts and log lines — never the matched value.
const (
	KindPII       = "pii"
	KindSecret    = "secret"
	KindInjection = "injection"
)

// Error codes that let a client tell a policy refusal apart from an
// upstream failure without parsing message strings (research-log
// entry 179). Both are JSON-RPC-style numeric codes, carried in the
// error object's "code" field:
//
//   - PolicyRefusalCode (-32001) is the Codvian policy-refusal code:
//     a guardrail blocked the request, the provider's response, or
//     an MCP tool call. It always arrives with HTTP 403 (or, for
//     MCP stdio, as the JSON-RPC error code of the tools/call
//     response) and a machine-readable "reason" naming the guardrail
//     kind (pii/secret/injection).
//   - UpstreamFailureCode (-32603, the JSON-RPC "internal error"
//     code) marks a failure that belongs to the upstream provider:
//     the chain is exhausted and the gateway answers HTTP 502.
//     A terminal upstream rejection (malformed/forbidden request)
//     keeps the upstream's own 4xx status instead and carries no
//     JSON-RPC code at all — its status IS its code.
//
// No other error in the router uses either code, so -32001 can never
// be confused with an upstream failure and -32603 can never be
// confused with a policy refusal.
const (
	PolicyRefusalCode   = -32001
	UpstreamFailureCode = -32603
)

// Guardrail phases: where an evaluation happened. The phase is the
// "source" a Check call records, and the label the metrics and the
// dashboard break evaluations down by. Only phases that really run
// exist here — no phase is invented for a path that has no check:
//
//   - PhaseRequest: the inbound chat request, checked before any
//     provider is dialled (request guardrail).
//   - PhaseResponse: a completed non-streaming provider response,
//     checked before it is handed to the client (response guardrail).
//     Streaming responses have no phase: their content is delivered
//     incrementally and cannot be recalled, so they are not
//     response-checked (see applyResponseGuardrails).
//   - PhaseMCPToolArgs: the arguments of an MCP tools/call, checked
//     before the tool is dispatched (MCP tool-call guardrail).
//     Tool outputs have no phase: they are generated from the
//     registry (provider names, model lists, counts), never
//     user-supplied, so there is nothing to scan.
const (
	PhaseRequest     = "request"
	PhaseResponse    = "response"
	PhaseMCPToolArgs = "tool_args"
)

// Guardrail evaluation actions: the outcome of one Check call.
// "allowed" means no pattern matched; "redacted" means a pattern
// matched and was redacted but the call was not blocked (blocking
// off for that kind); "blocked" means the call was refused under
// the store's blocking config; "warned" means a pattern matched
// under a warn-mode response check — the content was delivered
// unchanged and only counted and logged.
const (
	ActionAllowed  = "allowed"
	ActionRedacted = "redacted"
	ActionBlocked  = "blocked"
	ActionWarned   = "warned"
)

// Config is the operator's blocking policy. All three default to
// false: blocking is opt-in. Log redaction does not consult Config
// at all — it is always on (see Redact).
//
// External lists the operator-configured external guardrails
// (CHECKLIST 185): named checks backed by a remote classifier
// endpoint, each in sync or async mode. The blocking kinds
// (pii/secret/injection) must stay sync — see Validate: an async
// entry with a blocking kind is a config error, because a secret
// must never reach the provider while a verdict is in flight.
type Config struct {
	// BlockPII refuses a request whose text contains PII (email,
	// phone, card/Aadhaar-shaped numbers).
	BlockPII bool `json:"block_pii"`
	// BlockSecrets refuses a request whose text contains a
	// secret/key-shaped value.
	BlockSecrets bool `json:"block_secrets"`
	// BlockInjection refuses a request matching a prompt-injection
	// pattern.
	BlockInjection bool `json:"block_injection"`
	// DisabledTypes is the per-type toggle selection (G18 Area 7):
	// built-in pattern labels (see ListTypes) whose findings are
	// dropped from Check results — never counted, never blocked
	// on, never recorded as events. A disabled type's matches are
	// still stripped from log/redacted text: a toggle gates
	// detection and blocking, never log hygiene. Empty or absent
	// means every type is enabled. An unknown label is a config
	// error (see Validate) — a typo must fail loudly at load,
	// never silently leave the operator believing a type is off.
	DisabledTypes []string `json:"disabled_types,omitempty"`
	// External is the per-guardrail async/sync config for slow
	// external classifiers. Empty means no external guardrail.
	External []ExternalConfig `json:"external,omitempty"`
}

// BuiltinGuardrailNames are the guardrail names that always
// exist, with no registration needed: the three blocking kinds
// of the built-in stage.
func BuiltinGuardrailNames() []string {
	return []string{KindPII, KindSecret, KindInjection}
}

// RegisteredGuardrailNames returns every guardrail name a write
// may reference: the built-in kinds plus the configured external
// guardrails, in a stable order. A name not in this list is not
// registered — a write naming it must fail, never silently leave
// the writer believing a guardrail is in place.
func (c Config) RegisteredGuardrailNames() []string {
	names := BuiltinGuardrailNames()
	for _, e := range c.External {
		if e.Name != "" {
			names = append(names, e.Name)
		}
	}
	return names
}

// ValidateGuardrailNames rejects a list of guardrail names that
// references a name no guardrail is registered under (CHECKLIST
// 245, LiteLLM #51080): a typo'd name fails loudly at write time,
// naming the name and the registered names, instead of silently
// leaving a key unguarded. An empty name, a duplicate and an
// unknown name are all errors. An empty list is valid: it names
// no guardrail.
func (c Config) ValidateGuardrailNames(names []string) error {
	registered := map[string]bool{}
	for _, n := range c.RegisteredGuardrailNames() {
		registered[n] = true
	}
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" {
			return fmt.Errorf("guardrails: guardrail name must not be empty (registered: %s)", strings.Join(c.RegisteredGuardrailNames(), ", "))
		}
		if seen[n] {
			return fmt.Errorf("guardrails: guardrail %q is named twice", n)
		}
		seen[n] = true
		if !registered[n] {
			return fmt.Errorf("guardrails: unknown guardrail %q (registered: %s)", n, strings.Join(c.RegisteredGuardrailNames(), ", "))
		}
	}
	return nil
}

// Finding is one redacted match: its kind and the pattern label that
// matched (e.g. "email", "api-key", "ignore-instructions"). Count is
// how many matches of that pattern were found in the text.
type Finding struct {
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	Count   int    `json:"count"`
}

// Result is the outcome of checking one text.
type Result struct {
	// Redacted is the text with every PII/secret match replaced by a
	// [REDACTED:<label>] placeholder and every injection match
	// replaced by [REDACTED:INJECTION]. Safe to log, store or show.
	Redacted string `json:"-"`
	// Findings lists the patterns that matched, by kind + label.
	Findings []Finding `json:"findings"`
	// Blocked reports whether the request must be refused under the
	// store's Config. BlockReason names the kind that blocked it.
	Blocked     bool   `json:"blocked"`
	BlockReason string `json:"block_reason,omitempty"`
	// InjectionVerdict is the severity-scored verdict for the
	// injection findings in this result (nil when there are none):
	// level none/log/warn/block with max/total severity, derived
	// from the findings actually present. Advisory — blocking
	// still follows BlockInjection, which blocks on ANY injection
	// match. The dashboard and the guardrails test endpoint read
	// this instead of re-scoring.
	InjectionVerdict *InjectionVerdict `json:"injection_verdict,omitempty"`
}

// Has reports whether any finding of the given kind is present.
func (r Result) Has(kind string) bool {
	for _, f := range r.Findings {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Pattern table.
// ---------------------------------------------------------------------------

// redactRule is one deterministic pattern: what it matches, what kind
// it is, the label shown in findings/events, and the placeholder the
// match is replaced with. The placeholder never contains the match.
//
// Hot-path prefilter (pprof, 2026-11-17 bench: scan was 16.6% of all
// router CPU — 34+ regexes run on every request): any / all / digit
// are the cheap preconditions for the regex to possibly match, checked
// against the fold-lowercased text before the regex runs. any lists
// lowercase substrings of which at least one must appear; all lists
// lowercase substrings all of which must appear; digit means the
// text must contain an ASCII digit. A precondition is SOUND only
// when every possible match of the regex contains it — each entry
// below was derived from a literal fragment of its pattern (see the
// table comments) — so skipping a regex can never hide a match; it
// only skips work that could not have matched. A text with no trigger
// at all (the common clean prompt) runs zero regexes.
//
// Digit-shape gate (pprof, 2026-11-18 bench: the three digit rules
// — phone, card, aadhaar — ran their backtracking regexes on EVERY
// access-log line, because the line always contains a digit from
// "/v1/" + the timestamp + status + latency; ~20 µs per RedactLogLine
// call, ~8% of router CPU): digit=true alone only proves a digit
// exists, which is nearly always true. The shape field below names
// the digit-run pattern every match of that rule must contain (see
// digitShapes), computed once per scan in a single O(n) pass with
// no allocations. The gate is sound for the same reason the needle
// prefilter is: a match's digit runs and the gaps between them are
// fixed by the pattern, and redaction replacements can neither
// create a digit run nor shrink a gap below the shortest
// placeholder (15 chars), so a gap of ≤2 in the progressively
// redacted text is the same gap in the original text the gate was
// computed on. A gate that passes only runs the regex — it never
// suppresses a match.
type redactRule struct {
	kind        string
	pattern     string
	placeholder string
	re          *regexp.Regexp
	any         []string
	all         []string
	digit       bool
}

// digitShape* name the digit-run patterns in digitShapes: every
// match of the rule must exhibit the named shape.
const (
	// digitShapePhone: a phone match is either "+91"/"+1" followed
	// by its digit groups, a run of ≥10 digits ([6-9] followed by
	// 9 digits), three runs of ≥3/≥3/≥4 digits with gaps ≤2
	// (the 3-3-4 forms, separators 1–2 chars), or two runs of
	// ≥3/≥7 digits with gap ≤2 (the 3-3-4 forms where the last two
	// groups touch, e.g. "(123) 4567890").
	digitShapePhone = 1
	// digitShapeCard: a card match is four groups of 4 digits with
	// 0–1 separator chars between groups, or a 15-digit Amex
	// (34/37, 4-6-5 groups): either one run of ≥15 digits, or
	// two consecutive runs of ≥4 digits with gap ≤1.
	digitShapeCard = 2
	// digitShapeAadhaar: an Aadhaar match is three runs of exactly
	// 4 digits separated by exactly one whitespace char each:
	// three consecutive runs of ≥4 digits with gaps ≤1.
	digitShapeAadhaar = 3
)

// digitShapes is the digit-run summary of one text, computed once
// per scan by scanDigitShapes: whether any digit exists, whether
// the "+91"/"+1" country prefixes occur, and whether the text
// contains the run patterns a phone / card / Aadhaar match needs.
// longTokenRun is the generic-token shape gate (CHECKLIST 245):
// whether the text contains a run of >= minGenericTokenLen
// [A-Za-z0-9_-] chars — the precondition for the generic-token
// rule, which has no literal needle to prefilter on.
type digitShapes struct {
	hasDigit     bool
	hasPlus91    bool
	hasPlus1     bool
	phone        bool
	card         bool
	aadhaar      bool
	longTokenRun bool
}

// minGenericTokenLen is the shortest run the generic-token shape
// rule masks: 32 chars of [A-Za-z0-9_-] containing a digit, a
// lowercase and an uppercase letter (see redactGenericTokens).
// Shorter runs are ordinary words, IDs and hashes — masking them
// would hide useful log context for no security gain.
const minGenericTokenLen = 32

// scanDigitShapes summarises a text's maximal ASCII digit runs in
// one pass with no allocations: runs are maximal runs of [0-9];
// gap is the number of non-digit bytes between two consecutive runs.
// It reports whether the text could contain a phone, card or
// Aadhaar match under the shapes in the digitShape* constants.
// Gaps are counted in bytes: the separators in those patterns are
// ASCII (space, tab, newline, dash, dot, paren), so a byte gap is
// the separator length the regex would see.
func scanDigitShapes(text string) digitShapes {
	var out digitShapes
	// Rolling window of the last three runs: their lengths and the
	// gaps before runs 2 and 3 (gap[i] = bytes between run i and
	// run i+1, for the runs currently in the window).
	var lens [3]int
	var gaps [2]int
	n := 0 // runs seen so far (window holds the last min(n,3))
	runLen := 0
	gapLen := 0   // non-digit bytes since the last run ended
	tokenRun := 0 // current run of [A-Za-z0-9_-] chars (generic-token gate)
	inRun := false
	flush := func() {
		if n > 0 {
			gaps[0], gaps[1] = gaps[1], gapLen
			lens[0], lens[1], lens[2] = lens[1], lens[2], runLen
		} else {
			lens[2] = runLen
		}
		n++
		out.hasDigit = true
		// Evaluate the shapes against the window (runs are
		// maximal, so a longer run can only help a ≥ test).
		// ≥15, not ≥16: a bare 15-digit Amex run must reach the
		// card rule — the regex + Luhn validator still decide
		// whether it is a card, so a shorter run costs only one
		// regex run, never a false redaction.
		if runLen >= 15 {
			out.card = true
		}
		if runLen >= 10 {
			out.phone = true
		}
		if n >= 2 {
			a, b := lens[1], lens[2]
			g := gaps[1]
			if a >= 4 && b >= 4 && g <= 1 {
				out.card = true
			}
			if a >= 3 && b >= 7 && g <= 2 {
				out.phone = true
			}
		}
		if n >= 3 {
			a, b, c := lens[0], lens[1], lens[2]
			g1, g2 := gaps[0], gaps[1]
			if a >= 4 && b >= 4 && c >= 4 && g1 <= 1 && g2 <= 1 {
				out.aadhaar = true
			}
			if a >= 3 && b >= 3 && c >= 4 && g1 <= 2 && g2 <= 2 {
				out.phone = true
			}
		}
		runLen = 0
		gapLen = 0
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		// Generic-token shape gate: track runs of token chars
		// ([A-Za-z0-9_-]). A run of >= minGenericTokenLen is the
		// precondition for the generic-token rule. This rides on
		// the same single pass as the digit shapes, so the gate
		// costs nothing extra. A token run always contains a
		// digit (the rule requires one), so the gate is only
		// consulted when the digit pass ran at all.
		if isTokenChar(c) {
			tokenRun++
			if tokenRun >= minGenericTokenLen {
				out.longTokenRun = true
			}
		} else {
			tokenRun = 0
		}
		if c >= '0' && c <= '9' {
			if !inRun {
				inRun = true
				// A run starts: gapLen already holds the
				// non-digit bytes since the previous run
				// ended — that IS the gap, so it is left
				// alone here and consumed by flush().
			}
			runLen++
			continue
		}
		if inRun {
			inRun = false
			flush()
		}
		// Track the "+91" / "+1" prefixes: the '+' byte, then
		// '9','1' or '1'. (Checked here, not with Contains, so
		// the whole summary stays one pass.)
		if c == '+' && i+2 < len(text) && text[i+1] == '9' && text[i+2] == '1' {
			out.hasPlus91 = true
			out.phone = true
		}
		if c == '+' && i+1 < len(text) && text[i+1] == '1' {
			out.hasPlus1 = true
			out.phone = true
		}
		if n > 0 {
			gapLen++
		}
	}
	if inRun {
		flush()
	}
	return out
}

// isTokenChar reports whether c can appear in a token-shaped
// value: ASCII letters, digits, underscore or hyphen. Dots,
// slashes, colons and brackets break a run — a URL or a dotted
// identifier never forms one long token run.
func isTokenChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-'
}

// mustCompile compiles re or panics: every pattern here is a literal
// in this file, so a bad one is a programmer error caught by the
// first test run, not a runtime surprise.
func mustCompile(re string) *regexp.Regexp {
	return regexp.MustCompile(re)
}

var redactRules = []redactRule{
	// Secrets first: a labelled secret value can itself look like an
	// email/phone, and the secret label must win.
	// Prefilter needles: each is a literal fragment every match of
	// the pattern contains (fold-lowercased; e.g. the AKIA pattern
	// is uppercase-only, so its needle is the folded "akia").
	{KindSecret, "api-key", "[REDACTED:SECRET]", mustCompile(`(?i)(?:api[_-]?key|apikey|secret|access[_-]?token|auth[_-]?token|client[_-]?secret|aws[_-]?secret[_-]?access[_-]?key)\s*[:=]\s*["']?[^\s"']+`), []string{"api", "secret", "token"}, nil, false},
	{KindSecret, "password", "[REDACTED:SECRET]", mustCompile(`(?i)(?:password|passwd|pwd)\s*[:=]\s*["']?[^\s"']+`), []string{"password", "passwd", "pwd"}, nil, false},
	{KindSecret, "bearer-token", "[REDACTED:SECRET]", mustCompile(`(?i)Bearer\s+[A-Za-z0-9_\-.]{16,}`), []string{"bearer"}, nil, false},
	{KindSecret, "auth-header", "[REDACTED:SECRET]", mustCompile(`(?i)authorization\s*[:=]\s*["']?[^\s"']+(?:\s+[^\s"']+)?`), []string{"authorization"}, nil, false},
	{KindSecret, "openai-key", "[REDACTED:SECRET]", mustCompile(`sk-(?:proj-|ant-|svcacct-)?[A-Za-z0-9_\-]{16,}`), []string{"sk-"}, nil, false},
	{KindSecret, "anthropic-key", "[REDACTED:SECRET]", mustCompile(`sk-ant-[A-Za-z0-9_\-]{8,}`), []string{"sk-ant-"}, nil, false},
	{KindSecret, "groq-key", "[REDACTED:SECRET]", mustCompile(`gsk_[A-Za-z0-9]{20,}`), []string{"gsk_"}, nil, false},
	{KindSecret, "google-api-key", "[REDACTED:SECRET]", mustCompile(`AIza[0-9A-Za-z_\-]{35}`), []string{"aiza"}, nil, false},
	{KindSecret, "xai-key", "[REDACTED:SECRET]", mustCompile(`xai-[A-Za-z0-9]{16,}`), []string{"xai-"}, nil, false},
	{KindSecret, "huggingface-token", "[REDACTED:SECRET]", mustCompile(`hf_[A-Za-z0-9]{16,}`), []string{"hf_"}, nil, false},
	{KindSecret, "gitlab-token", "[REDACTED:SECRET]", mustCompile(`glpat-[A-Za-z0-9_\-]{16,}`), []string{"glpat-"}, nil, false},
	{KindSecret, "live-key", "[REDACTED:SECRET]", mustCompile(`(?i)(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`), []string{"_live_", "_test_"}, nil, false},
	{KindSecret, "aws-access-key", "[REDACTED:SECRET]", mustCompile(`AKIA[0-9A-Z]{16}`), []string{"akia"}, nil, false},
	{KindSecret, "aws-session-key", "[REDACTED:SECRET]", mustCompile(`ASIA[0-9A-Z]{16}`), []string{"asia"}, nil, false},
	{KindSecret, "sendgrid-key", "[REDACTED:SECRET]", mustCompile(`SG\.[A-Za-z0-9_\-]{22}\.[A-Za-z0-9_\-]{43}`), []string{"sg."}, nil, false},
	{KindSecret, "digitalocean-token", "[REDACTED:SECRET]", mustCompile(`dop_v1_[a-f0-9]{64}`), []string{"dop_v1_"}, nil, false},
	{KindSecret, "github-token", "[REDACTED:SECRET]", mustCompile(`(?:ghp_|gho_|ghu_|ghs_|ghr_|github_pat_)[A-Za-z0-9_]{16,}`), []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}, nil, false},
	{KindSecret, "slack-token", "[REDACTED:SECRET]", mustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`), []string{"xox"}, nil, false},
	{KindSecret, "jwt", "[REDACTED:SECRET]", mustCompile(`\beyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\b`), []string{"eyj"}, nil, false},
	// G18 Area 7 — provider secrets missing from the old table.
	// Each pattern is written from the provider's own published
	// token shape (prefix + length + alphabet), not copied from
	// any other router. A Twilio account SID (AC + 32 hex) is
	// deliberately NOT in this table: it is an identifier, not
	// a secret — only the SK API key is redacted.
	{KindSecret, "replicate-key", "[REDACTED:SECRET]", mustCompile(`\br8_[A-Za-z0-9]{20,}`), []string{"r8_"}, nil, false},
	{KindSecret, "linear-key", "[REDACTED:SECRET]", mustCompile(`\blin_api_[A-Za-z0-9]{32,}`), []string{"lin_api_"}, nil, false},
	{KindSecret, "notion-token", "[REDACTED:SECRET]", mustCompile(`\bsecret_[A-Za-z0-9]{43,}|\bntn_[A-Za-z0-9]{20,}`), []string{"secret_", "ntn_"}, nil, false},
	{KindSecret, "npm-token", "[REDACTED:SECRET]", mustCompile(`\bnpm_[A-Za-z0-9]{36,}`), []string{"npm_"}, nil, false},
	{KindSecret, "postman-key", "[REDACTED:SECRET]", mustCompile(`(?i)\bPMAK-[A-Za-z0-9\-]{24,}`), []string{"pmak-"}, nil, false},
	// discord-token carries no prefilter needles on purpose: the
	// shape has no fixed literal fragment (first char M or N,
	// then variable-length base64url segments) — a "." needle
	// would match nearly every input and skip nothing, so the
	// regex runs on every scan for this rule only.
	{KindSecret, "discord-token", "[REDACTED:SECRET]", mustCompile(`\b[MN][A-Za-z0-9]{23,}\.[A-Za-z0-9_\-]{6}\.[A-Za-z0-9_\-]{27,}\b`), nil, nil, false},
	{KindSecret, "square-key", "[REDACTED:SECRET]", mustCompile(`\bsq0(?:atp|csp)-[A-Za-z0-9_\-]{16,}`), []string{"sq0"}, nil, false},
	{KindSecret, "twilio-key", "[REDACTED:SECRET]", mustCompile(`\bSK[0-9a-fA-F]{32}`), []string{"sk"}, nil, false},
	{KindSecret, "mailgun-key", "[REDACTED:SECRET]", mustCompile(`\bkey-[0-9a-zA-Z]{32}`), []string{"key-"}, nil, false},
	{KindSecret, "connection-string", "[REDACTED:SECRET]", mustCompile(`(?i)\b(?:mongodb(?:\+srv)?|postgres(?:ql)?|mysql|mariadb|rediss?|amqps?)://[^\s"']+`), []string{"mongodb", "postgres", "mysql", "mariadb", "redis", "amqp"}, nil, false},
	// generic-sk-key sits after openai-key/anthropic-key on
	// purpose: the specific rules label their tokens first, and
	// this one catches only the sk- shapes they do not cover
	// (shorter than 16 chars after the prefix). The old table
	// already covered every sk- token of 16+ chars through the
	// openai-key rule; this rule closes the short-token gap.
	{KindSecret, "generic-sk-key", "[REDACTED:SECRET]", mustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`), []string{"sk-"}, nil, false},
	// PII.
	// G18 Area 7 — new PII types. Rules whose regex alone would
	// over-match carry a validator in patternValidators
	// (pii_types.go): the regex finds candidates, the validator
	// decides. Rules are ordered so the most specific shape wins:
	// IP before phone (digit runs inside an address), IBAN
	// before EU VAT (a VAT-shaped prefix of an IBAN has no word
	// boundary to stop on, so the IBAN must be taken first),
	// PESEL before Steuer-ID (both are 11 digits; the PESEL
	// checksum rejects a Steuer-ID and vice versa, so order
	// only matters for the rare number passing both — and both
	// sit AFTER the phone rule, because a +1 phone number
	// without separators is an 11-digit run that can pass the
	// Steuer checksum by chance), ZA VAT before ZA tax-ref
	// (both are 10 digits, only VAT starts with 4). A dotted
	// quad such as a version number ("1.2.3.4") is
	// indistinguishable from an IPv4 address and matches as
	// one — that ambiguity is inherent to the format, not a
	// bug in the pattern.
	{KindPII, "ipv4", "[REDACTED:IP]", mustCompile(`\b(?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\b`), nil, nil, true},
	{KindPII, "ipv6", "[REDACTED:IP]", mustCompile(`[0-9A-Fa-f:]+`), []string{":"}, nil, false},
	{KindPII, "ssn", "[REDACTED:SSN]", mustCompile(`\b\d{3}-\d{2}-\d{4}\b`), nil, nil, true},
	{KindPII, "cpf", "[REDACTED:CPF]", mustCompile(`\b\d{3}\.?\d{3}\.?\d{3}-?\d{2}\b`), nil, nil, true},
	{KindPII, "cnpj", "[REDACTED:CNPJ]", mustCompile(`\b\d{2}\.?\d{3}\.?\d{3}/?\d{4}-?\d{2}\b`), nil, nil, true},
	{KindPII, "iban", "[REDACTED:IBAN]", mustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{0,3})?\b`), nil, nil, true},
	{KindPII, "eu-vat", "[REDACTED:EU_VAT]", mustCompile(`\b(?:AT|BE|BG|CY|CZ|DE|DK|EE|ES|FI|FR|GR|HR|HU|IE|IT|LT|LU|LV|MT|NL|PL|PT|RO|SE|SI|SK)[A-Z0-9]{8,12}\b`), nil, nil, true},
	{KindPII, "es-dni-nie", "[REDACTED:ES_ID]", mustCompile(`\b\d{8}[A-Z]\b|\b[XYZ]\d{7}[A-Z]\b`), nil, nil, true},
	{KindPII, "it-codice-fiscale", "[REDACTED:IT_CF]", mustCompile(`\b[A-Z]{6}\d{2}[A-Z]\d{2}[A-Z]\d{3}[A-Z]\b`), nil, nil, true},
	{KindPII, "nl-bsn", "[REDACTED:BSN]", mustCompile(`\b\d{9}\b`), nil, nil, true},
	{KindPII, "se-personnummer", "[REDACTED:SE_PNR]", mustCompile(`\b\d{6}[-+]?\d{4}\b|\b\d{8}[-]?\d{4}\b`), nil, nil, true},
	{KindPII, "za-id", "[REDACTED:ZA_ID]", mustCompile(`\b\d{13}\b`), nil, nil, true},
	{KindPII, "za-phone", "[REDACTED:PHONE]", mustCompile(`\+27[\s\-]?\d{2}[\s\-]?\d{3}[\s\-]?\d{4}|\b0[1-8]\d[\s\-]?\d{3}[\s\-]?\d{4}\b`), nil, nil, true},
	{KindPII, "za-vat", "[REDACTED:ZA_VAT]", mustCompile(`\b4\d{9}\b`), nil, nil, true},
	// phone-intl extends the phone rule below with the country
	// forms it never covered: BR (+55), UK (+44), DE (+49), FR
	// (+33) and AU (+61). The country code is required — a bare
	// local BR/UK number is ambiguous with other digit runs and
	// is not claimed. ZA international (+27) lives in za-phone
	// above, so it is not repeated here.
	{KindPII, "phone-intl", "[REDACTED:PHONE]", mustCompile(`\+55[\s\-]?\(?\d{2}\)?[\s\-]?\d{4,5}[\s\-]?\d{4}|\+44[\s\-]?7\d{3}[\s\-]?\d{6}|\+49[\s\-]?1\d{2}[\s\-]?\d{3,4}[\s\-]?\d{4}|\+33[\s\-]?\d[\s\-]?\d{2}[\s\-]?\d{2}[\s\-]?\d{2}[\s\-]?\d{2}|\+61[\s\-]?4[\s\-]?\d{4}[\s\-]?\d{4}`), []string{"+55", "+44", "+49", "+33", "+61"}, nil, true},
	{KindPII, "email", "[REDACTED:EMAIL]", mustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), []string{"@"}, nil, false},
	{KindPII, "phone", "[REDACTED:PHONE]", mustCompile(`\+91[\s\-]?\d{5}[\s\-]?\d{5}|\+1[\s\-]?\(?\d{3}\)?[\s\-]?\d{3}[\s\-]?\d{4}|\(\d{3}\)\s?\d{3}[\s\-.]?\d{4}|\b\d{3}[\s\-.]\d{3}[\s\-.]\d{4}\b|\b[6-9]\d{9}\b`), nil, nil, true},
	// The bare-digit ID rules sit AFTER the phone rule on
	// purpose: a +1 phone number without separators
	// ("+14155550132") is an 11-digit run, and an 11-digit run
	// can pass the Steuer-ID checksum by chance — the phone
	// rule must take it first. The phone rule cannot match a
	// bare 11-digit run at all (it needs a country code, a
	// separator form or a 10-digit [6-9] run), so a genuine
	// PESEL/Steuer-ID still reaches these rules untouched;
	// and a 10-digit ZA tax ref cannot be a [6-9] phone
	// number, so its validator and this ordering agree.
	{KindPII, "pl-pesel", "[REDACTED:PESEL]", mustCompile(`\b\d{11}\b`), nil, nil, true},
	{KindPII, "de-steuer-id", "[REDACTED:DE_TAX_ID]", mustCompile(`\b\d{11}\b`), nil, nil, true},
	{KindPII, "za-tax-ref", "[REDACTED:ZA_TAX_REF]", mustCompile(`\b\d{10}\b`), nil, nil, true},
	// Amex alternative: 15 digits starting 34/37, in 4-6-5
	// groups ("3782 822463 10005") or bare ("378282246310005").
	// validCardLuhn already accepts 13–19 digits, so the old
	// 16-digit-only extraction was the only thing keeping Amex
	// out of the validator.
	{KindPII, "card", "[REDACTED:CARD]", mustCompile(`\b(?:\d{4}[\s\-]?\d{4}[\s\-]?\d{4}[\s\-]?\d{4}|3[47]\d{2}[\s\-]?\d{6}[\s\-]?\d{5})\b`), nil, nil, true},
	{KindPII, "aadhaar", "[REDACTED:AADHAAR]", mustCompile(`\b\d{4}\s\d{4}\s\d{4}\b`), nil, nil, true},
}

// injectionRule is one prompt-injection pattern: a deterministic
// case-insensitive phrase/regex. This is a pattern list, not an LLM
// judge — it catches the common instruction-override phrasings and
// says so; it does not claim to catch every jailbreak.
type injectionRule struct {
	pattern string
	re      *regexp.Regexp
	any     []string
	all     []string
}

var injectionRules = []injectionRule{
	// Prefilter needles: same soundness rule as redactRules — every
	// match contains the listed fragment(s). "dan-mode" has two
	// alternatives; either contains "dan" (DAN mode) or "anything"
	// (do anything now), so any=["dan", "anything"] covers both —
	// "do" alone was tried first and fires on every "How does…"
	// prompt, which defeats the prefilter.
	{"ignore-instructions", mustCompile(`(?i)ignore\s+(all\s+)?(previous|prior|above)\s+instructions`), []string{"ignore"}, nil},
	{"disregard-instructions", mustCompile(`(?i)disregard\s+(all\s+)?(previous|prior|above|your)\s+instructions`), []string{"disregard"}, nil},
	{"forget-instructions", mustCompile(`(?i)forget\s+(all\s+)?(previous|prior)\s+instructions`), []string{"forget"}, nil},
	{"you-are-now", mustCompile(`(?i)you\s+are\s+now\b`), nil, []string{"you", "are", "now"}},
	{"reveal-system-prompt", mustCompile(`(?i)reveal\s+(your\s+)?(system\s+prompt|instructions|prompt)`), []string{"reveal"}, nil},
	{"print-system-prompt", mustCompile(`(?i)print\s+(your\s+)?(system\s+prompt|instructions)`), []string{"print"}, nil},
	{"override-instructions", mustCompile(`(?i)override\s+(your\s+)?(instructions|safety|guardrails)`), []string{"override"}, nil},
	{"bypass-safety", mustCompile(`(?i)bypass\s+(your\s+)?(safety|filters?|guardrails|restrictions)`), []string{"bypass"}, nil},
	{"jailbreak", mustCompile(`(?i)jailbreak`), []string{"jailbreak"}, nil},
	{"dan-mode", mustCompile(`(?i)\bDAN\s+mode\b|do\s+anything\s+now`), []string{"dan", "anything"}, nil},
	{"new-instructions", mustCompile(`(?i)new\s+instructions\s*:`), nil, []string{"new", "instructions"}},
}

// ---------------------------------------------------------------------------
// Pure redaction — always on, no config, no recording.
// ---------------------------------------------------------------------------

// Redact returns text with every PII/secret match replaced by its
// placeholder and every injection match replaced by
// [REDACTED:INJECTION]. It never consults any toggle: log redaction
// is the default, not a setting the operator has to discover. A text
// with no matches is returned unchanged.
func Redact(text string) string {
	redacted, _ := scan(text)
	return redacted
}

// Findings returns the patterns that match text, without rewriting
// it. The findings carry labels only — never the matched values.
func Findings(text string) []Finding {
	_, findings := scan(text)
	return findings
}

// scan applies every rule in table order (secrets before PII, so a
// labelled secret wins over an email-shaped value inside it) and
// returns the redacted text plus the findings. Injection matches are
// replaced, not just flagged, so the redacted text is safe to log
// even when the injection phrase itself is the sensitive part.
// foldLower lowercases text for prefilter needle checks. It is NOT
// strings.ToLower: Go's (?i) regexes match by Unicode simple fold,
// so a keyword spelled with a fold-equivalent rune (long-s "ſ" for
// "s", Kelvin sign "K" for "k") still matches the regex — and a
// plain ToLower would miss the needle and wrongly skip the rule.
// foldLower maps every rune to the ASCII lowercase letter of its
// simple-fold orbit when one exists (so "ſecret" folds to "secret"),
// and lowercases ASCII as usual; anything else passes through
// unchanged. Needles are ASCII, so a folded needle can only match
// where the regex could also match — the prefilter stays sound.
func foldLower(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteByte(byte(r + ('a' - 'A')))
		case r < 0x80:
			b.WriteRune(r)
		default:
			rep := r
			for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
				if (f >= 'a' && f <= 'z') || (f >= 'A' && f <= 'Z') {
					rep = f
					break
				}
			}
			if rep >= 'A' && rep <= 'Z' {
				rep += 'a' - 'A'
			}
			b.WriteRune(rep)
		}
	}
	return b.String()
}

// digitShapeFor maps a digit rule's pattern label to the digit-run
// shape every match of that rule must exhibit (0 = no digit gate).
// The shape lives here, derived from the label, rather than as a
// struct field so the table's positional literals stay untouched.
// It runs once per rule at init (see the prefilter index below),
// never per scan.
func digitShapeFor(pattern string) int {
	switch pattern {
	case "phone":
		return digitShapePhone
	case "card":
		return digitShapeCard
	case "aadhaar":
		return digitShapeAadhaar
	}
	return 0
}

// ---------------------------------------------------------------------------
// Single-pass needle prefilter (CHECKLIST 263–264, follow-up to
// the 249–250 profile: guardrails.scan 0.97 s cum, 2.6% of router
// CPU, two thirds of it the ~46 strings.Contains passes — one
// full-text scan per needle). All needles of all rules are
// indexed once at init by first byte; one pass over the folded
// text records which needles occur (a bitset), and each rule's
// prefilter becomes bit tests. The result is exactly
// strings.Contains per needle: a needle matches at position i
// iff folded[i:i+len(needle)] == needle, and the first-byte
// dispatch only ever skips positions whose byte cannot start
// that needle — so a rule is skipped exactly when the old loop
// would have skipped it, and the regexes still decide every
// match. Non-ASCII bytes can never start a needle (every
// needle is ASCII, so its first byte is < 0x80, while UTF-8
// continuation bytes are >= 0x80).
// ---------------------------------------------------------------------------

// maxPrefilterNeedles bounds the needle bitset (two uint64s).
// The tables hold 66 needles today (65 distinct — "sk-" is listed
// twice, once per rule — so 66 bits are consumed and 62 remain);
// exceeding the bound is a programmer error caught at init, not a
// silent miss.
const maxPrefilterNeedles = 128

// needleRef is one needle in the first-byte dispatch table.
type needleRef struct {
	word string // the needle itself
	bit  int    // its bit in the found bitset (0..127)
}

// prefilterRule is one rule's prefilter data: which needles must
// all be present (allMask), which need at least one present
// (anyMask; all-zero means no any-requirement), the digit gate
// and the digit-shape gate (precomputed at init — the old code
// ran the digitShapeFor switch per rule per scan).
type prefilterRule struct {
	anyMask, allMask [2]uint64
	digit            bool
	shape            int
}

// needleByFirst dispatches scan positions to candidate needles
// by first byte; prefilterRules is one entry per rule, redact
// rules first then injection rules, in table order.
var needleByFirst [256][]needleRef
var prefilterRules []prefilterRule

func init() {
	total := 0
	add := func(any, all []string, digit bool, shape int) {
		var r prefilterRule
		r.digit, r.shape = digit, shape
		for _, w := range any {
			if w == "" {
				panic("guardrails: empty needle in a rule's any list — an empty needle matches everywhere, so say so explicitly")
			}
			if total >= maxPrefilterNeedles {
				panic("guardrails: too many prefilter needles — widen the found bitset")
			}
			bit := total
			total++
			r.anyMask[bit>>6] |= 1 << uint(bit&63)
			needleByFirst[w[0]] = append(needleByFirst[w[0]], needleRef{word: w, bit: bit})
		}
		for _, w := range all {
			if w == "" {
				panic("guardrails: empty needle in a rule's all list — an empty needle matches everywhere, so say so explicitly")
			}
			if total >= maxPrefilterNeedles {
				panic("guardrails: too many prefilter needles — widen the found bitset")
			}
			bit := total
			total++
			r.allMask[bit>>6] |= 1 << uint(bit&63)
			needleByFirst[w[0]] = append(needleByFirst[w[0]], needleRef{word: w, bit: bit})
		}
		prefilterRules = append(prefilterRules, r)
	}
	for _, r := range redactRules {
		add(r.any, r.all, r.digit, digitShapeFor(r.pattern))
	}
	for _, r := range injectionRules {
		add(r.any, r.all, false, 0)
	}
}

// matches reports whether a rule's regex could possibly match:
// every all-needle present, at least one any-needle present (when
// the list is non-empty), and a digit present when required. The
// needles are checked against the folded ORIGINAL text; the regex
// itself still runs on the progressively redacted text, so a rule
// whose trigger was already redacted away by an earlier rule just
// spends one wasted regex run — correct, only less skipping in
// multi-match texts. Identical semantics to the old per-needle
// Contains loop — the bitset IS the Contains results, collected
// in one pass.
func (r prefilterRule) matches(found [2]uint64, shapes digitShapes) bool {
	if r.digit && !shapes.hasDigit {
		return false
	}
	// Digit-shape gate (see digitShapes): a digit rule's regex
	// runs only when the text contains the digit-run pattern
	// every match of that rule must exhibit.
	if r.digit && r.shape != 0 {
		switch r.shape {
		case digitShapePhone:
			if !shapes.phone {
				return false
			}
		case digitShapeCard:
			if !shapes.card {
				return false
			}
		case digitShapeAadhaar:
			if !shapes.aadhaar {
				return false
			}
		}
	}
	if found[0]&r.allMask[0] != r.allMask[0] || found[1]&r.allMask[1] != r.allMask[1] {
		return false
	}
	if r.anyMask == [2]uint64{0, 0} {
		return true
	}
	return found[0]&r.anyMask[0] != 0 || found[1]&r.anyMask[1] != 0
}

// needlePresence runs the single-pass needle scan over folded
// and returns the bitset of needles that occur in it.
func needlePresence(folded string) [2]uint64 {
	var found [2]uint64
	for i := 0; i < len(folded); i++ {
		for _, c := range needleByFirst[folded[i]] {
			if found[c.bit>>6]&(1<<uint(c.bit&63)) != 0 {
				continue // already found
			}
			w := c.word
			if i+len(w) <= len(folded) && folded[i:i+len(w)] == w {
				found[c.bit>>6] |= 1 << uint(c.bit&63)
			}
		}
	}
	return found
}

func scan(text string) (string, []Finding) {
	out, findings, _ := scanVault(text, nil)
	return out, findings
}

// scanVault is scan with an optional reversible vault: with a
// non-nil vault, PII-kind matches are minted into tokens instead
// of dead placeholders (see RedactWithVault); secrets and
// injections are never minted. It returns the rewritten text,
// the findings, and how many tokens were minted.
func scanVault(text string, vault *Vault) (string, []Finding, int) {
	if text == "" {
		return "", nil, 0
	}
	// One folded copy + one digit-shape pass + one needle pass
	// serve every rule's prefilter; a clean prompt then runs zero
	// regexes. The shape pass runs only when the text contains a
	// digit at all, so a digit-less prompt pays nothing for it
	// (it replaces the old IndexAny check: hasDigit comes from
	// the shape pass itself).
	folded := foldLower(text)
	var shapes digitShapes
	if strings.IndexAny(text, "0123456789") >= 0 {
		shapes = scanDigitShapes(text)
	}
	found := needlePresence(folded)
	var findings []Finding
	minted := 0
	out := text
	// Whole-block PEM redaction (M1.7): a private-key block is
	// redacted as one span — header, base64 body and END footer —
	// before the other rules run, so no body line can leak through
	// the generic-token heuristic (which only masks mixed-case
	// runs) and an unclosed block is redacted to end of text.
	// The table's old header-only private-key rule is gone (see
	// pem.go); the "private-key" finding is recorded here instead.
	// Gated on the literal header so clean texts pay nothing.
	if strings.Contains(folded, "-----begin") {
		if pemRedacted, n := redactPEMBlocks(text); n > 0 {
			out = pemRedacted
			findings = append(findings, Finding{Kind: KindSecret, Pattern: "private-key", Count: n})
		}
	}
	for i, r := range redactRules {
		if !prefilterRules[i].matches(found, shapes) {
			continue
		}
		// Validated rules (pii_types.go): the regex finds
		// candidates and only the ones passing the checksum /
		// format validator are replaced — a 16-digit run that
		// fails Luhn is an order number, not a card, and must
		// survive untouched.
		if v := patternValidators[r.pattern]; v != nil {
			var n int
			if vault != nil && r.kind == KindPII {
				var m int
				out, n, m = replaceValidatedVault(out, r.re, v, func(match string) (string, bool) {
					return vault.tokenFor(r.pattern, match, r.placeholder)
				})
				minted += m
			} else {
				out, n = replaceValidated(out, r.re, v, r.placeholder)
			}
			if n == 0 {
				continue
			}
			findings = append(findings, Finding{Kind: r.kind, Pattern: r.pattern, Count: n})
			continue
		}
		matches := r.re.FindAllStringIndex(out, -1)
		if len(matches) == 0 {
			continue
		}
		findings = append(findings, Finding{Kind: r.kind, Pattern: r.pattern, Count: len(matches)})
		if vault != nil && r.kind == KindPII {
			out = r.re.ReplaceAllStringFunc(out, func(match string) string {
				tok, ok := vault.tokenFor(r.pattern, match, r.placeholder)
				if ok {
					minted++
				}
				return tok
			})
		} else {
			out = r.re.ReplaceAllString(out, r.placeholder)
		}
	}
	// Generic-token shape rule (CHECKLIST 245, LiteLLM #48968):
	// log redaction must match secret SHAPES, not just named
	// fields — a token-shaped value in an arbitrary field (a
	// pasted key in "notes", a token in an unknown JSON key)
	// is masked even though no rule names that field. It runs
	// after the named secret rules, so a labelled secret is
	// already its placeholder and can never be re-matched here;
	// and it is gated on shapes.longTokenRun (computed on the
	// original text), so a clean line pays no scan at all. The
	// gate is sound: redaction placeholders contain no token
	// run longer than 8 chars ("REDACTED"), so a long run in
	// the progressively redacted text was already a long run
	// in the original text the gate was computed on.
	if shapes.longTokenRun {
		if n := redactGenericTokens(&out); n > 0 {
			findings = append(findings, Finding{Kind: KindSecret, Pattern: "generic-token", Count: n})
		}
	}
	for i, r := range injectionRules {
		if !prefilterRules[len(redactRules)+i].matches(found, shapes) {
			continue
		}
		// Injection scan bound (G18 Area 7): only the first
		// InjectionScanLimit bytes are scanned for injection
		// patterns. The bound is the CPU guard — an unbounded
		// scan of a megabyte prompt is unbounded regex work on
		// the hot path. The deliberate divergence from the old
		// behaviour is that an injection phrase past 16 KB is
		// now unchecked (same bound the reference router uses); PII and
		// secret redaction above still scan the full text, so
		// a key or an address past 16 KB is still redacted.
		head, tail := injectionHead(out)
		matches := r.re.FindAllStringIndex(head, -1)
		if len(matches) == 0 {
			continue
		}
		findings = append(findings, Finding{Kind: KindInjection, Pattern: r.pattern, Count: len(matches)})
		out = r.re.ReplaceAllString(head, "[REDACTED:INJECTION]") + tail
	}
	return out, findings, minted
}

// replaceValidated replaces only the regex matches that pass
// valid, returning the rewritten string and how many matches
// were replaced. Matches that fail validation are left byte
// for byte as they were.
func replaceValidated(s string, re *regexp.Regexp, valid func(string) bool, placeholder string) (string, int) {
	locs := re.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s, 0
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	n := 0
	for _, loc := range locs {
		if !valid(s[loc[0]:loc[1]]) {
			continue
		}
		b.WriteString(s[last:loc[0]])
		b.WriteString(placeholder)
		last = loc[1]
		n++
	}
	if n == 0 {
		return s, 0
	}
	b.WriteString(s[last:])
	return b.String(), n
}

// replaceValidatedVault is replaceValidated with a per-match
// replacer: only matches passing valid are rewritten, and the
// replacement comes from replace, which reports whether the
// replacement is a vault token (true) or a fallback placeholder
// (false). It returns the rewritten string, how many matches
// were replaced, and how many of those are vault tokens.
func replaceValidatedVault(s string, re *regexp.Regexp, valid func(string) bool, replace func(string) (string, bool)) (string, int, int) {
	locs := re.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s, 0, 0
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	n := 0
	minted := 0
	for _, loc := range locs {
		if !valid(s[loc[0]:loc[1]]) {
			continue
		}
		rep, ok := replace(s[loc[0]:loc[1]])
		b.WriteString(s[last:loc[0]])
		b.WriteString(rep)
		last = loc[1]
		n++
		if ok {
			minted++
		}
	}
	if n == 0 {
		return s, 0, 0
	}
	b.WriteString(s[last:])
	return b.String(), n, minted
}

// redactGenericTokens masks token-shaped runs in *s: maximal runs
// of [A-Za-z0-9_-] of at least minGenericTokenLen chars that mix
// all three classes — a digit, a lowercase letter and an uppercase
// letter. The mix is what separates a random token from the long
// strings that legitimately appear in logs: a 40-hex commit SHA
// and a 32-hex UUID have digits and lowercase only (no uppercase)
// and are left alone; a long English word has no digit; a dotted
// version or a URL path never forms one run. A run that fails the
// mix is left untouched, never partially masked. It returns the
// number of runs masked. The shape (length + mix) is the rule —
// there is no allowlist of field names, so a token in any field
// is masked the same way.
func redactGenericTokens(s *string) int {
	text := *s
	n := len(text)
	var b strings.Builder
	b.Grow(n)
	masked := 0
	i := 0
	for i < n {
		if !isTokenChar(text[i]) {
			b.WriteByte(text[i])
			i++
			continue
		}
		j := i
		hasDigit, hasLower, hasUpper := false, false, false
		for j < n && isTokenChar(text[j]) {
			c := text[j]
			switch {
			case c >= '0' && c <= '9':
				hasDigit = true
			case c >= 'a' && c <= 'z':
				hasLower = true
			case c >= 'A' && c <= 'Z':
				hasUpper = true
			}
			j++
		}
		if j-i >= minGenericTokenLen && hasDigit && hasLower && hasUpper {
			b.WriteString("[REDACTED:SECRET]")
			masked++
		} else {
			b.WriteString(text[i:j])
		}
		i = j
	}
	if masked == 0 {
		return 0
	}
	*s = b.String()
	return masked
}

// ---------------------------------------------------------------------------
// Store — runtime config, counters, recent events.
// ---------------------------------------------------------------------------

// Event is one recorded redaction: when it happened, what kind of
// pattern matched, the pattern label, where the text came from, and
// whether the request was blocked. Preview is the redacted text
// truncated to 120 runes — it can contain placeholders but never a
// matched value, so it is safe to show on the dashboard and to log.
type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Pattern string    `json:"pattern"`
	Source  string    `json:"source"`
	Blocked bool      `json:"blocked"`
	Preview string    `json:"preview"`
}

// EvalCount is one (phase, action) evaluation total: how many Check
// calls in that phase ended with that action. One row per Check call
// (not per finding), so summing a phase's rows equals the number of
// evaluations in that phase; the per-kind split lives in ByKind.
type EvalCount struct {
	Phase  string `json:"phase"`
	Action string `json:"action"`
	Count  uint64 `json:"count"`
}

// Stats is the live guardrail state served to the dashboard: the
// current blocking config, lifetime counters (real counts from this
// process, zero until something happens), per-kind counts, and the
// most recent events. Zero values are honest — an empty event list
// means nothing has been redacted yet, not that the feed is broken.
// Async is the per-guardrail async/sync state (CHECKLIST 185):
// every configured external guardrail with its mode and live
// verdict counts, plus the recent async verdicts — empty until an
// external guardrail runs.
// SecurityRefusals counts security refusals since this process
// started, by reason label (see the SecurityRefusal* constants):
// a client override rejected, an unknown guardrail name on a
// write. The keys are labels only — never a value, a field
// value, or a key id. Zero / absent means no such refusal has
// happened yet, not that the feed is broken.
type Stats struct {
	Config   Config            `json:"config"`
	Checked  uint64            `json:"checked"`
	Redacted uint64            `json:"redacted"`
	Blocked  uint64            `json:"blocked"`
	ByKind   map[string]uint64 `json:"by_kind"`
	// ByPattern counts detections per pattern label (e.g.
	// "email", "ssn", "openai-key", "custom:foo") — the
	// per-type/per-pattern totals the dashboard reads (G18
	// Area 7). ByKind is the coarser per-kind roll-up of the
	// same detections. Both are always non-nil.
	ByPattern map[string]uint64 `json:"by_pattern"`
	// Types lists every built-in pattern label with its
	// per-type toggle state (see ListTypes). The dashboard
	// renders its per-type toggles from this, not from a
	// hard-coded list.
	Types            []TypeState          `json:"types"`
	Events           []Event              `json:"events"`
	Evaluations      []EvalCount          `json:"evaluations"`
	Async            []AsyncGuardrailView `json:"async"`
	AsyncEvents      []AsyncEvent         `json:"async_events"`
	SecurityRefusals map[string]uint64    `json:"security_refusals,omitempty"`
}

// maxEvents is how many recent events the store keeps (newest last).
const maxEvents = 50

// maxPreviewRunes caps an event preview.
const maxPreviewRunes = 120

// evalKey is one (phase, action) evaluation bucket.
type evalKey struct {
	phase  string
	action string
}

// Store holds the runtime blocking config plus the counters and
// recent events. The zero value is usable (blocking all off); use
// NewStore for the conventional constructor.
type Store struct {
	mu       sync.Mutex
	cfg      Config
	checked  uint64
	redacted uint64
	blocked  uint64
	byKind   map[string]uint64
	// byPattern counts detections per pattern label (the
	// per-type/per-pattern totals, G18 Area 7). It is the
	// same detections ByKind rolls up, split by label.
	byPattern map[string]uint64
	// disabledTypes is the per-type toggle state (G18 Area
	// 7): a pattern label present here is disabled — its
	// findings are dropped from Check results (so it can
	// never block) while log redaction still strips it.
	// Absent means enabled. See ListTypes/SetTypeEnabled.
	disabledTypes map[string]bool
	// custom is the operator's custom regex patterns (G18
	// Area 7), lazily created. It has its own mutex; see
	// customStore().
	custom *CustomPatternStore
	// respMode is the store's response-guardrail mode
	// (redact/warn/block, default redact). It is a Store
	// setting, not part of Config, so changing it never
	// touches the request blocking toggles.
	respMode ResponseGuardMode
	events   []Event
	// evals counts Check calls by (phase, action) — one entry per
	// call, so a phase's rows sum to its evaluation count.
	evals map[evalKey]uint64
	// checkers are the AsyncCheckers registered by name for the
	// configured external guardrails (main.go registers one
	// HTTPClassifier per entry; tests register fakes).
	checkers map[string]AsyncChecker
	// asyncStats counts async/sync external verdicts by guardrail
	// name and outcome (see AsyncGuardrailView).
	asyncStats map[string]*asyncGuardrailStats
	// asyncEvents is the recent async verdicts (newest last).
	asyncEvents []AsyncEvent
	// verdictHook, when set, is invoked for every async verdict
	// the store records (phase "async"). The server sets it to
	// feed /metrics; the store never requires it.
	verdictHook VerdictHook
	// securityRefusals counts security refusals since this
	// process started, by reason label (see the
	// SecurityRefusal* constants). Recorded by
	// RecordSecurityRefusal; read by Stats.
	securityRefusals map[string]uint64
}

// Security refusal reasons (CHECKLIST 245-246): the labels the
// dashboard counts for refusals that are security policy, not
// content guardrails. Each is a label only — never a value.
const (
	// SecurityRefusalVertexOverride is a client-supplied Vertex
	// location/project rejected because the admin did not opt
	// in to client overrides.
	SecurityRefusalVertexOverride = "vertex-override"
	// SecurityRefusalUnknownGuardrail is a write that named a
	// guardrail no guardrail is registered under.
	SecurityRefusalUnknownGuardrail = "unknown-guardrail"
)

// securityRefusalPreview is the fixed, value-free preview an
// event carries for a security refusal: the reason in words, with
// no field value, location, project, key id or guardrail value in
// it. The event proves the refusal happened without recording
// anything the client sent.
func securityRefusalPreview(reason string) string {
	switch reason {
	case SecurityRefusalVertexOverride:
		return "client-supplied Vertex location/project rejected (admin opt-in off)"
	case SecurityRefusalUnknownGuardrail:
		return "write named an unregistered guardrail"
	default:
		return "security refusal: " + reason
	}
}

// RecordSecurityRefusal counts one security refusal under reason
// and appends a value-free audit event for it, so the dashboard
// can show real counts (zero until one happens) and the refusal
// is visible in the recent-events feed. It never records the
// rejected value: only the reason label.
func (s *Store) RecordSecurityRefusal(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.securityRefusals == nil {
		s.securityRefusals = map[string]uint64{}
	}
	s.securityRefusals[reason]++
	s.events = append(s.events, Event{
		At:      time.Now().UTC(),
		Kind:    "security",
		Pattern: reason,
		Source:  "request-validation",
		Blocked: true,
		Preview: securityRefusalPreview(reason),
	})
	if len(s.events) > maxEvents {
		s.events = append([]Event(nil), s.events[len(s.events)-maxEvents:]...)
	}
}

// NewStore returns a Store with blocking off (the default) and log
// redaction always on.
func NewStore() *Store {
	return &Store{byKind: map[string]uint64{}, byPattern: map[string]uint64{}, disabledTypes: map[string]bool{}, evals: map[evalKey]uint64{}, checkers: map[string]AsyncChecker{}, asyncStats: map[string]*asyncGuardrailStats{}, respMode: ResponseModeRedact}
}

// NewStoreWith returns a Store seeded with cfg (e.g. from the config
// file at startup).
// cloneExternalConfig deep-copies the external entries: Config
// values cross the store boundary by value, but the External
// slice header would otherwise share its backing array — and
// json.Decode into a returned config reuses slice capacity,
// silently rewriting the store's own entries in place (CHECKLIST
// 185: a rejected POST must never mutate the store).
func cloneExternalConfig(in []ExternalConfig) []ExternalConfig {
	if in == nil {
		return nil
	}
	out := make([]ExternalConfig, len(in))
	copy(out, in)
	return out
}

func NewStoreWith(cfg Config) *Store {
	s := NewStore()
	cfg.External = cloneExternalConfig(cfg.External)
	cfg.DisabledTypes = cloneStrings(cfg.DisabledTypes)
	s.cfg = cfg
	s.disabledTypes = disabledTypesMap(cfg.DisabledTypes)
	return s
}

// cloneStrings deep-copies a string list: Config values cross the
// store boundary by value, but a slice header would otherwise
// share its backing array (same CHECKLIST 185 concern as
// cloneExternalConfig).
func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// disabledTypesMap builds the per-type toggle state from a config
// list: every named label is disabled, everything else enabled.
func disabledTypesMap(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// disabledTypeNames lists the currently disabled built-in pattern
// labels, sorted, for Config round-trips.
func (s *Store) disabledTypeNames() []string {
	names := make([]string, 0, len(s.disabledTypes))
	for n := range s.disabledTypes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Config returns the current blocking config. DisabledTypes
// reflects the live per-type toggle state (including runtime
// SetTypeEnabled calls), so a Config() → SetConfig(cfg) round-trip
// never clobbers toggles the dashboard or an operator set.
func (s *Store) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	cfg.External = cloneExternalConfig(cfg.External)
	cfg.DisabledTypes = s.disabledTypeNames()
	return cfg
}

// SetConfig replaces the blocking config, including the per-type
// toggle selection: the store's disabled set becomes exactly the
// names in cfg.DisabledTypes. It never touches log redaction,
// which has no off switch.
func (s *Store) SetConfig(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg.External = cloneExternalConfig(cfg.External)
	cfg.DisabledTypes = cloneStrings(cfg.DisabledTypes)
	s.cfg = cfg
	s.disabledTypes = disabledTypesMap(cfg.DisabledTypes)
}

// Check scans text, records the outcome, and reports whether the
// request must be blocked under the current config. source names
// where the text came from — it is the guardrail phase
// (PhaseRequest, PhaseResponse or PhaseMCPToolArgs) and the label
// the evaluation is counted under. The returned Redacted text is
// safe to log. A blocked result means the caller must refuse the
// request before dialling any provider.
func (s *Store) Check(text, source string) Result {
	return s.checkFiltered(text, source, nil)
}

// CheckScoped is Check running only the guardrails named in
// allowed (CHECKLIST 277 (d), per-key guardrail scoping): a
// gateway key with a non-empty guardrails list runs only its
// listed guardrails, never the global set. allowed == nil means
// every guardrail runs (identical to Check). A non-nil allowed
// list — even an empty one — runs only the named guardrails:
// built-in kinds (pii, secret, injection) by kind name. Findings
// for guardrails outside the list are ignored entirely — not
// blocked on, not counted in byKind, not recorded as events — so
// a scoped check's counters describe only what actually ran for
// that key. External guardrails are not part of this scan; see
// CheckExternalSyncScoped. Log redaction (Result.Redacted) still
// strips every pattern regardless of scope: redaction is log
// hygiene, always on, not a guardrail run.
func (s *Store) CheckScoped(text, source string, allowed []string) Result {
	return s.checkFiltered(text, source, allowed)
}

// injectionSeverityFor returns the 0–10 severity of one injection
// finding's pattern label: built-ins from injectionSeverity,
// "custom:<label>" from the store's custom injection patterns
// (0 means the default of 5, matching Store.ScoreInjection).
// ok=false when the label is unknown — a finding whose severity
// is unknown scores nothing rather than inventing a number.
func (s *Store) injectionSeverityFor(pattern string) (int, bool) {
	if sev, ok := injectionSeverity[pattern]; ok {
		return sev, true
	}
	if !strings.HasPrefix(pattern, "custom:") {
		return 0, false
	}
	label := strings.TrimPrefix(pattern, "custom:")
	s.mu.Lock()
	custom := s.custom
	s.mu.Unlock()
	if custom == nil {
		return 0, false
	}
	custom.mu.Lock()
	defer custom.mu.Unlock()
	for _, p := range custom.patterns {
		if p.Kind == KindInjection && p.Label == label {
			sev := p.Severity
			if sev == 0 {
				sev = 5
			}
			return sev, true
		}
	}
	return 0, false
}

// injectionVerdictFor derives the severity verdict from the
// injection findings already in the result — the same thresholds
// ScoreInjection uses (block max>=8 or total>=12, warn max>=5 or
// total>=8, else log). Deriving it from the findings (rather than
// re-scanning) means the verdict never names a pattern the result
// does not contain: scope filtering and per-type toggles have
// already removed their findings by the time this runs.
func (s *Store) injectionVerdictFor(findings []Finding) *InjectionVerdict {
	var matched []struct {
		pattern  string
		severity int
	}
	for _, f := range findings {
		if f.Kind != KindInjection {
			continue
		}
		sev, ok := s.injectionSeverityFor(f.Pattern)
		if !ok {
			continue
		}
		matched = append(matched, struct {
			pattern  string
			severity int
		}{f.Pattern, sev})
	}
	if len(matched) == 0 {
		return nil
	}
	v := scoreInjectionVerdict(matched)
	return &v
}

// checkFiltered is the shared implementation behind Check and
// CheckScoped: allowed == nil runs everything; a non-nil list
// filters the findings to the named guardrails first, and the
// rest of the logic (blocking, counting, events) sees only the
// filtered findings.
func (s *Store) checkFiltered(text, source string, allowed []string) Result {
	redacted, findings := scan(text)
	if allowed != nil {
		keep := map[string]bool{}
		for _, name := range allowed {
			keep[name] = true
		}
		filtered := findings[:0]
		for _, f := range findings {
			if keep[f.Kind] {
				filtered = append(filtered, f)
			}
		}
		findings = filtered
	}
	// Per-type toggles (G18 Area 7): findings for a disabled
	// type are dropped before blocking, counting and events —
	// a disabled type can never block. The Redacted text is
	// NOT un-redacted: log redaction still strips every
	// pattern regardless of scope or toggles, exactly as
	// CheckScoped already does for scope.
	findings = s.filterDisabledTypes(findings)
	// Custom patterns (G18 Area 7): matched against the
	// redacted text, appended as findings and redacted in
	// place. A custom pattern can therefore block under the
	// kind toggle of its own kind.
	redacted, findings = s.applyCustomPatterns(redacted, findings)
	res := Result{Redacted: redacted, Findings: findings}
	// Injection severity (B3-23): when injection findings survive
	// filtering, attach the scored verdict to the result, so the
	// severity scoring lives in the real request path instead of
	// only in the standalone ScoreInjection function. ScannedBytes
	// is how much of the input the injection scan covered: the
	// first InjectionScanLimit bytes (the bound scan() applies).
	if res.Has(KindInjection) {
		if v := s.injectionVerdictFor(findings); v != nil {
			v.ScannedBytes = min(len(text), InjectionScanLimit)
			res.InjectionVerdict = v
		}
	}
	if len(findings) == 0 {
		s.mu.Lock()
		s.checked++
		s.recordEvalLocked(source, ActionAllowed)
		s.mu.Unlock()
		return res
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.BlockPII && res.Has(KindPII) {
		res.Blocked, res.BlockReason = true, KindPII
	} else if s.cfg.BlockSecrets && res.Has(KindSecret) {
		res.Blocked, res.BlockReason = true, KindSecret
	} else if s.cfg.BlockInjection && res.Has(KindInjection) {
		res.Blocked, res.BlockReason = true, KindInjection
	}
	action := ActionRedacted
	if res.Blocked {
		action = ActionBlocked
	}
	s.recordFindingsLocked(source, findings, action, true, truncateRunes(redacted, maxPreviewRunes))
	return res
}

// recordFindingsLocked counts one check that produced findings:
// the checked/redacted totals, the per-kind and per-pattern
// totals, the blocked total when action is blocked, one
// (phase, action) evaluation, and one audit event per finding
// kind. The caller holds s.mu. preview is the redacted text
// already truncated to maxPreviewRunes — it carries
// placeholders, never a matched value. redactedCount reports
// whether anything was actually redacted by this check (a
// warn-mode check counts its findings but not a redaction).
func (s *Store) recordFindingsLocked(source string, findings []Finding, action string, redactedCount bool, preview string) {
	if s.byKind == nil {
		s.byKind = map[string]uint64{}
	}
	if s.byPattern == nil {
		s.byPattern = map[string]uint64{}
	}
	s.checked++
	if redactedCount {
		s.redacted++
	}
	for _, f := range findings {
		s.byKind[f.Kind] += uint64(f.Count)
		s.byPattern[f.Pattern] += uint64(f.Count)
	}
	if action == ActionBlocked {
		s.blocked++
	}
	s.recordEvalLocked(source, action)
	// One event per finding entry (kind + pattern), not per match:
	// the event list is a readable audit trail, not a per-token dump.
	for _, f := range findings {
		s.events = append(s.events, Event{
			At:      time.Now().UTC(),
			Kind:    f.Kind,
			Pattern: f.Pattern,
			Source:  source,
			Blocked: action == ActionBlocked,
			Preview: preview,
		})
	}
	if len(s.events) > maxEvents {
		s.events = append([]Event(nil), s.events[len(s.events)-maxEvents:]...)
	}
}

// recordEvalLocked counts one evaluation in the (phase, action)
// bucket. The caller holds s.mu. A zero-value Store (nil evals map)
// is handled: the map is created on first use.
func (s *Store) recordEvalLocked(phase, action string) {
	if s.evals == nil {
		s.evals = map[evalKey]uint64{}
	}
	s.evals[evalKey{phase: phase, action: action}]++
}

// Stats returns a snapshot of the live state: config, counters,
// per-kind counts, per-(phase, action) evaluation counts and recent
// events (newest last). ByKind is always non-nil and Events always
// non-nil, so the dashboard renders an honest empty state instead
// of null.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	byKind := map[string]uint64{}
	for k, v := range s.byKind {
		byKind[k] = v
	}
	byPattern := map[string]uint64{}
	for k, v := range s.byPattern {
		byPattern[k] = v
	}
	events := append([]Event(nil), s.events...)
	if events == nil {
		events = []Event{}
	}
	evals := make([]EvalCount, 0, len(s.evals))
	for k, v := range s.evals {
		evals = append(evals, EvalCount{Phase: k.phase, Action: k.action, Count: v})
	}
	sort.Slice(evals, func(i, j int) bool {
		if evals[i].Phase != evals[j].Phase {
			return evals[i].Phase < evals[j].Phase
		}
		return evals[i].Action < evals[j].Action
	})
	securityRefusals := map[string]uint64{}
	for k, v := range s.securityRefusals {
		securityRefusals[k] = v
	}
	return Stats{Config: s.cfg, Checked: s.checked, Redacted: s.redacted, Blocked: s.blocked, ByKind: byKind, ByPattern: byPattern, Types: s.listTypesLocked(), Events: events, Evaluations: evals, Async: s.asyncViews(), AsyncEvents: s.asyncEventsSnapshot(), SecurityRefusals: securityRefusals}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// RedactLogLine is the helper for log call sites that must print an
// error or a value that could carry prompt/key material: it redacts
// the string first, so no log line can ever carry a prompt or a key
// even if the caller forgot the policy. It is Redact under a name
// that reads correctly at a log call site.
func RedactLogLine(s string) string {
	return Redact(s)
}

// SanitizeSpendString redacts a single identifier/value before it is
// written to the spend ledger (usage_events). The ledger stores
// provider/model/key IDs and token counts — never prompt text — but
// a misconfigured identifier carrying a key-shaped value would
// otherwise land in the spend log verbatim; this is the LiteLLM
// entry-152 fix: spend-log redaction ON by default, not opt-in.
func SanitizeSpendString(s string) string {
	return Redact(strings.TrimSpace(s))
}
