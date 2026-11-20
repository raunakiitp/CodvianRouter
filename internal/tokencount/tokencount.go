// Package tokencount counts tokens locally, offline, with tiktoken
// BPE encodings.
//
// The BPE rank tables (cl100k_base, o200k_base) are vendored under
// bpe/ and embedded with go:embed, and tiktoken-go is pointed at an
// embedded loader — so counting never touches the network at
// runtime, even on a machine that has never downloaded anything.
// The files were fetched once at build time from OpenAI's public
// encodings bucket (openaipublic.blob.core.windows.net/encodings/);
// their SHA-256 hashes are recorded in bpe/SHA256SUMS for an
// operator who wants to verify them against the published files.
//
// A local count is an ESTIMATE, never a provider-reported number:
//   - the encoding is chosen from the model name, and unknown models
//     fall back to cl100k_base (labelled Fallback);
//   - non-OpenAI providers tokenize differently, so the count is
//     approximate for them even when the encoding is right;
//   - CountPrompt covers message text only (content + text parts +
//     tool-call arguments). Tool schemas, system-prompt wrappers and
//     per-message framing overhead the upstream adds are not counted
//     there — use CountPromptWire (per-wire framing + tool
//     definitions) or CountPromptFull (the wire-agnostic reference)
//     wherever the count feeds a budget or fit decision (see
//     docs/token-count-audit.md for the measured drift of each).
//
// Callers must label local counts as estimates wherever they are
// stored or shown (see ledger.Usage.LocalEstimate), and must never
// present one as the provider's own count. The two legitimate uses
// are pre-send budget enforcement (refuse before dialling when the
// estimated prompt cost alone would blow a fail-closed budget) and
// the fallback count when the upstream reports no usage at all.
package tokencount

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	tiktoken "github.com/pkoukk/tiktoken-go"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

//go:embed bpe/cl100k_base.tiktoken bpe/o200k_base.tiktoken
var bpeFS embed.FS

// bpeFileNames maps the tiktoken-go blob URL for an encoding to the
// vendored file name, so the embedded loader can answer the exact
// URL tiktoken-go asks for.
var bpeFileNames = map[string]string{
	"https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken": "bpe/cl100k_base.tiktoken",
	"https://openaipublic.blob.core.windows.net/encodings/o200k_base.tiktoken":  "bpe/o200k_base.tiktoken",
}

// embeddedLoader serves BPE rank tables from the embedded FS. Any
// URL it does not know is an error, never a network fetch: counting
// stays offline by construction.
type embeddedLoader struct{}

func (embeddedLoader) LoadTiktokenBpe(url string) (map[string]int, error) {
	name, ok := bpeFileNames[url]
	if !ok {
		return nil, fmt.Errorf("tokencount: no vendored BPE table for %q (offline loader)", url)
	}
	f, err := bpeFS.Open(name)
	if err != nil {
		return nil, fmt.Errorf("tokencount: open vendored BPE table: %w", err)
	}
	defer f.Close()
	contents, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("tokencount: read vendored BPE table: %w", err)
	}
	ranks := make(map[string]int)
	for _, line := range strings.Split(string(contents), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) != 2 {
			return nil, fmt.Errorf("tokencount: malformed BPE line in %s", name)
		}
		token, err := base64.StdEncoding.DecodeString(parts[0])
		if err != nil {
			return nil, fmt.Errorf("tokencount: malformed BPE token in %s: %w", name, err)
		}
		rank, err := strconv.Atoi(strings.TrimRight(parts[1], "\r"))
		if err != nil {
			return nil, fmt.Errorf("tokencount: malformed BPE rank in %s: %w", name, err)
		}
		ranks[string(token)] = rank
	}
	return ranks, nil
}

var installOnce sync.Once

// encoderCache holds one built *tiktoken.Tiktoken per encoding
// name, for the life of the process. Only two encodings exist
// (cl100k_base, o200k_base) and both are immutable, so the cache
// is bounded at two entries.
//
// Why this exists: tiktoken-go v0.1.8 has no cache of its own —
// GetEncoding calls NewCoreBPE on every call, which compiles two
// regexp2 patterns, inverts the 100,256-entry encoder map and
// sort.Slice's 100,256 token byte-slices (~73 ms per build on this
// VM; benchmark round 2, phase 1 profiled it at 86.9% of all CPU
// on the request path). Caching the built encoder turns every
// Count after the first into a map lookup plus EncodeOrdinary.
//
// Why sharing one encoder across goroutines is safe (verified
// against the vendored dependency source, not assumed):
//   - EncodeOrdinary -> CoreBPE.encodeOrdinaryNative only reads
//     bp.encoder (a map that is read-only after NewCoreBPE) and
//     calls FindStringMatch on bp.tlRegex; bytePairEncode works
//     purely on locals plus read-only map lookups (bpe.go).
//   - regexp2 v1.10.0 documents "A Regexp is safe for concurrent
//     use by multiple goroutines" (regexp.go:25) and implements
//     it with a mutex-guarded pool of per-match runners
//     (getRunner/putRunner under muRun, runner.go:1585-1612), so
//     concurrent matches on the shared tlRegex each get their
//     own runner and share no mutable match state.
//   - Tiktoken.EncodeOrdinary touches no other encoder state.
//
// So no per-encoder mutex is needed around EncodeOrdinary; the
// mutex below guards only the cache map itself, and is never held
// during an encode.
var (
	encodersMu sync.Mutex
	encoders   = map[string]*tiktoken.Tiktoken{}
)

// getEncoder returns the cached built encoder for encoding,
// building (and caching) it on first use. A build failure is
// returned uncached, so a later call retries — a failed build is
// a packaging defect, not a state to remember.
func getEncoder(encoding string) (*tiktoken.Tiktoken, error) {
	installLoader()
	encodersMu.Lock()
	defer encodersMu.Unlock()
	if enc, ok := encoders[encoding]; ok {
		return enc, nil
	}
	enc, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return nil, err
	}
	encoders[encoding] = enc
	return enc, nil
}

// installLoader points tiktoken-go at the embedded loader exactly
// once. tiktoken-go's loader is process-global mutable state, so
// every caller in this binary shares the offline loader — that is
// the point: no code path in the router may trigger a download.
func installLoader() {
	installOnce.Do(func() {
		tiktoken.SetBpeLoader(embeddedLoader{})
	})
}

// TokenCount is one local token count.
type TokenCount struct {
	// Tokens is the local BPE token count of the counted text.
	Tokens int
	// Encoding is the tiktoken encoding used ("cl100k_base" or
	// "o200k_base").
	Encoding string
	// Fallback is true when the model name matched no known
	// encoding and cl100k_base was assumed. A fallback count is a
	// rougher estimate than a matched one, and must be labelled
	// as such wherever it is shown.
	Fallback bool
	// Estimated is true when Tokens is NOT an exact BPE count
	// but a chars/4 estimate: the input exceeded
	// maxExactCountBytes, so exact BPE work was capped (see
	// Count). An estimated count must be labelled as such
	// wherever it is shown, and must never feed a fail-closed
	// budget as if it were exact.
	Estimated bool
}

// maxExactCountBytes caps exact BPE work (M1.9): inputs longer
// than this are counted as len(text)/4 (Estimated=true) instead
// of being BPE-encoded. 4 MiB covers every realistic prompt
// (the 1 MiB timing test below must still be exact) while
// bounding the worst-case encode; see Count.
const maxExactCountBytes = 4 * 1024 * 1024

// chunkThresholdBytes / chunkSizeBytes / maxPieceBytes bound
// the O(n^2) merge in the vendored tiktoken-go bytePairMerge
// (it rescans the whole piece for the minimum rank on every
// merge — measured 6.5 s for a single 64 KiB regex piece of
// repeated 'a', and a 1 MiB unchunked run of random letters
// never finished in 180 s, i.e. such an input is infeasible
// unchunked). Inputs longer than chunkThresholdBytes are
// counted in ~chunkSizeBytes chunks (see splitCountChunks),
// each chunk memoized separately so repeated chunks encode
// once. The merge cost is governed by the longest regex piece
// it can see, so maxPieceBytes caps every run the pre-tokenizer
// could emit as one piece (a long letter / punctuation /
// whitespace run) at 128 bytes: total work is then O(n *
// maxPieceBytes) — linear-ish in the input — at the cost of a
// small, measured boundary drift (a split can cost a token or
// two where a merge would have crossed the boundary; see
// TestCountChunkedMatchesUnchunked, drift budget 0.5%,
// measured 0.000% on prose). A heap based merge inside the
// vendored library would remove the drift entirely, but the
// merge loop lives in tiktoken-go, not here, and reimplementing
// its pre-tokenizer in this package would risk exactness drift
// against the vendored tables; that is recorded as leftover
// work, not done silently.
const chunkThresholdBytes = 64 * 1024
const chunkSizeBytes = 1024
const maxPieceBytes = 128

// encodingForModel resolves the tiktoken encoding for a model name:
// tiktoken-go's own model/prefix table first, then the known
// o200k-era prefixes (gpt-4o, o1/o3/o4, gpt-5), then cl100k_base
// as the fallback. Unknown models (including non-OpenAI ones and
// virtual models such as auto/*) always resolve — never an error —
// because the caller needs a count, not a refusal; Fallback says
// how much to trust it. The table is consulted directly (no BPE
// load) so resolution itself stays cheap.
//
// Only the two vendored encodings (cl100k_base, o200k_base) are
// ever returned: tiktoken-go's table also resolves legacy models
// to p50k_base, r50k_base, p50k_edit and gpt2, none of which is
// vendored, and returning one of those made Count ERROR —
// breaking the "never an error" promise above (M1.9, PARITY
// Area 10 "Per-model encoding selection" was WRONG for exactly
// this). A legacy model now resolves to cl100k_base with
// Fallback=true: a rougher estimate, but a count.
func encodingForModel(model string) (encoding string, fallback bool) {
	if name, ok := tiktoken.MODEL_TO_ENCODING[model]; ok {
		return vendoredOrFallback(name)
	}
	for prefix, name := range tiktoken.MODEL_PREFIX_TO_ENCODING {
		if strings.HasPrefix(model, prefix) {
			return vendoredOrFallback(name)
		}
	}
	lower := strings.ToLower(model)
	for _, p := range []string{"gpt-4o", "o1-", "o1", "o3-", "o3", "o4-", "o4", "gpt-5", "chatgpt-4o"} {
		if strings.HasPrefix(lower, p) {
			return tiktoken.MODEL_O200K_BASE, false
		}
	}
	return tiktoken.MODEL_CL100K_BASE, true
}

// vendoredOrFallback returns name unchanged when it is one of the
// two vendored encodings, and cl100k_base with fallback=true
// otherwise (legacy p50k/r50k/gpt2 encodings — see
// encodingForModel).
func vendoredOrFallback(name string) (string, bool) {
	if name == tiktoken.MODEL_CL100K_BASE || name == tiktoken.MODEL_O200K_BASE {
		return name, false
	}
	return tiktoken.MODEL_CL100K_BASE, true
}

// Count returns the local token count of text under the encoding
// for model. It never fails on an unknown or legacy model (see
// encodingForModel); it fails only when the vendored BPE table
// cannot be loaded, which is a build/packaging defect, not a
// property of the text.
//
// Two size guards (M1.9, parity tracker Area 10 "Large-input bound /
// DoS safety" — was MISSING):
//   - longer than maxExactCountBytes: no BPE at all, a chars/4
//     estimate with Estimated=true (the the reference router behaviour for
//     over-cap input, and fail-safe here: callers treat a Count
//     error as "proceed", so erroring on a huge prompt would
//     silently undercount a budget gate — an estimate in the
//     safe direction for a gate that reads Estimated is better
//     than a silent pass-through);
//   - longer than chunkThresholdBytes: counted in chunkSizeBytes
//     pieces (see the constants above) so the vendored O(n^2)
//     merge can never see a long regex piece.
//
// Hot path (CPU profile, 2026-11-18 bench, CHECKLIST 249-250):
// the SAME prompt is encoded three times per request (per-key
// traffic estimate, context-fit gate, reconcileUsage); the memo
// below collapses those to one BPE encode per distinct prompt
// (a hit is a SHA-256 + map lookup, ~0.5 µs, instead of a
// tiktoken regex split + byte-pair merge, ~75 µs). The memo is
// a pure-function cache — the count of (encoding, text) never
// changes — so a hit returns exactly what a fresh encode
// would, and only successful counts are stored (a BPE-load
// failure is never cached). Bounded at maxMemoEntries; when
// full the map is cleared wholesale (counts are recomputed,
// never stale).
func Count(model, text string) (TokenCount, error) {
	encoding, fallback := encodingForModel(model)
	if len(text) > maxExactCountBytes {
		return TokenCount{
			Tokens:    len(text) / 4,
			Encoding:  encoding,
			Fallback:  fallback,
			Estimated: true,
		}, nil
	}
	sum := sha256.Sum256([]byte(text))
	if hit, ok := memoLookup(encoding, fallback, sum); ok {
		memoHits.Add(1)
		return hit, nil
	}
	memoMisses.Add(1)
	var n int
	var err error
	if len(text) > chunkThresholdBytes {
		n, err = countChunked(encoding, fallback, text)
	} else {
		// The whole-text memo lookup above already missed, so
		// encode without looking it up a second time (M7.9).
		n, err = countExactUncached(encoding, fallback, text, sum)
	}
	if err != nil {
		return TokenCount{}, err
	}
	got := TokenCount{
		Tokens:   n,
		Encoding: encoding,
		Fallback: fallback,
	}
	memoStore(encoding, fallback, sum, got)
	return got, nil
}

// countExact is one BPE encode of text (which must fit in a
// single chunk — callers bound the length), memoized per
// (encoding, fallback, SHA-256). EncodeOrdinary, not Encode: a
// prompt that literally contains a special-token string (e.g.
// "<|endoftext|>") must count as ordinary text — Encode would
// panic on it as disallowed.
func countExact(encoding string, fallback bool, text string) (int, error) {
	sum := sha256.Sum256([]byte(text))
	if hit, ok := memoLookup(encoding, fallback, sum); ok {
		return hit.Tokens, nil
	}
	return countExactUncached(encoding, fallback, text, sum)
}

// countExactUncached encodes text and stores the result under
// sum, without a memo lookup: for callers (Count) that already
// looked the whole text up and missed. The old path hashed the
// text and took the memo lock twice per cold count (once in
// Count, once here) — the M7.9 memo audit found the duplicate
// SHA-256 + lock on every cold encode and this removes it; the
// chunked path still goes through countExact, whose per-piece
// lookup is not a duplicate (each piece is keyed separately).
func countExactUncached(encoding string, fallback bool, text string, sum [32]byte) (int, error) {
	enc, err := getEncoder(encoding)
	if err != nil {
		return 0, fmt.Errorf("tokencount: load %s encoding: %w", encoding, err)
	}
	n := len(enc.EncodeOrdinary(text))
	memoStore(encoding, fallback, sum, TokenCount{Tokens: n, Encoding: encoding, Fallback: fallback})
	return n, nil
}

// countChunked counts text by splitting it into chunkSizeBytes
// pieces and summing the per-piece exact counts. Repeated
// pieces hit the per-piece memo in countExact, so a 1 MiB
// input of repeated text encodes each distinct piece once.
func countChunked(encoding string, fallback bool, text string) (int, error) {
	total := 0
	for _, chunk := range splitCountChunks(text) {
		n, err := countExact(encoding, fallback, chunk)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// splitCountChunks splits text into chunks of at most
// ~chunkSizeBytes for counting, in two steps:
//
//  1. Segments: a segment is a whitespace run plus the
//     following non-whitespace run — i.e. the leading space
//     stays glued to its word, so a regex piece of the form
//     " word" (usually a single BPE token) is never split
//     across a chunk boundary. Any segment longer than
//     maxPieceBytes (a word, punctuation run or whitespace run
//     longer than that) is hard-split into <= maxPieceBytes
//     pieces at UTF-8 rune boundaries, so no single regex
//     piece the merge can see exceeds maxPieceBytes.
//  2. Packing: segments are greedily packed into chunks of at
//     most chunkSizeBytes; a chunk boundary always falls
//     between segments (before a whitespace run), so no piece
//     crosses it.
//
// The only drift vs a single unchunked encode comes from the
// hard splits in step 1, which only happen for runs over 256
// bytes — ordinary prose drifts by zero (see
// TestCountChunkedMatchesUnchunked).
func splitCountChunks(text string) []string {
	var chunks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
	}
	add := func(seg string) {
		if cur.Len() > 0 && cur.Len()+len(seg) > chunkSizeBytes {
			flush()
		}
		cur.WriteString(seg)
	}
	for i := 0; i < len(text); {
		start := i
		for i < len(text) && isChunkSpace(text[i]) {
			i++
		}
		for i < len(text) && !isChunkSpace(text[i]) {
			i++
		}
		seg := text[start:i]
		if len(seg) <= maxPieceBytes {
			add(seg)
			continue
		}
		for _, piece := range splitUTF8(seg, maxPieceBytes) {
			add(piece)
		}
	}
	flush()
	return chunks
}

// isChunkSpace reports whether b is ASCII whitespace, the only
// separator splitCountChunks splits on. Non-ASCII spaces stay
// inside a word run (the BPE regex treats them as part of a
// piece, and splitting one off would only add drift).
func isChunkSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// splitUTF8 splits s into pieces of at most max bytes, never
// cutting in the middle of a multi-byte UTF-8 rune (the cut
// backs off over continuation bytes). Invalid UTF-8 starting
// with continuation bytes falls back to a plain byte split.
func splitUTF8(s string, max int) []string {
	var out []string
	for len(s) > max {
		end := max
		for end > 0 && s[end]&0xC0 == 0x80 {
			end--
		}
		if end == 0 {
			end = max
		}
		out = append(out, s[:end])
		s = s[end:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}

// maxMemoEntries bounds the Count memo: 4,096 distinct prompts at
// ~100 bytes of key + value each is well under 1 MiB. The bench
// cycles 1,000 prompts, so the whole working set fits; a larger
// real-world set simply clears and rebuilds (see memoStore).
const maxMemoEntries = 4096

// memoKey is the Count memo key: the encoding (two prompts that
// differ only by encoding count differently — cl100k vs o200k
// disagree on CJK) plus the fallback flag plus the SHA-256 of the
// text (a 256-bit hash of the exact bytes; storing the text itself
// would duplicate every prompt in memory). The fallback flag MUST
// be part of the key: TokenCount.Fallback depends on the model
// name, not the encoding — "gpt-4" and "unknown-model-xyz" both
// resolve to cl100k_base but report Fallback false and true, and
// keying on encoding alone returns the wrong flag for the second
// model (caught by TestCountKnownTexts).
type memoKey struct {
	encoding string
	fallback bool
	sum      [32]byte
}

var (
	memoMu  sync.Mutex
	memoMap = make(map[memoKey]TokenCount, maxMemoEntries)
	// memoHits / memoMisses count Count-level memo outcomes
	// (M7.9 audit instrumentation): a hit is a Count call
	// answered from the memo, a miss is one that had to BPE-encode
	// (chunked inputs count once here — the per-piece lookups
	// inside countChunked are not Count-level outcomes). Read via
	// Load; the audit test snapshots deltas around a workload.
	memoHits   atomic.Int64
	memoMisses atomic.Int64
)

// memoLookup returns the cached count for (encoding, fallback, sum).
func memoLookup(encoding string, fallback bool, sum [32]byte) (TokenCount, bool) {
	memoMu.Lock()
	defer memoMu.Unlock()
	got, ok := memoMap[memoKey{encoding: encoding, fallback: fallback, sum: sum}]
	return got, ok
}

// memoStore records count for (encoding, fallback, sum). When the map is
// full it is cleared first: counts are a deterministic pure
// function, so dropping them only costs a re-encode, never a
// wrong answer.
func memoStore(encoding string, fallback bool, sum [32]byte, count TokenCount) {
	memoMu.Lock()
	defer memoMu.Unlock()
	if len(memoMap) >= maxMemoEntries {
		clear(memoMap)
	}
	memoMap[memoKey{encoding: encoding, fallback: fallback, sum: sum}] = count
}

// CountPromptWire counts a request's prompt tokens the way the
// provider for wire will bill them (CHECKLIST 229-230): message
// text (as CountPrompt) PLUS the tool definitions PLUS the wire's
// per-message framing. The plain CountPrompt undercounts every
// wire that carries tools or framing — the audit measured -78% to
// -83% mean error on tool-call-heavy prompts on all five wires,
// and -6% to -19% on OpenAI prompts of every class, before this
// function existed (docs/token-count-audit.md).
//
//   - Tool definitions: every wire sends them and every provider
//     bills them as input tokens, so they are counted as the BPE
//     count of the canonical JSON of the tools array — the same
//     JSON the wire clients serialise. Wire-specific reshaping
//     (Anthropic input_schema, Gemini functionDeclarations) moves
//     the count by a few tokens per tool; the audit re-measures
//     the residual per wire instead of pretending it is zero.
//   - Framing: OpenAI's documented cookbook rule — 3 tokens per
//     message plus 3 for the reply priming — for the OpenAI wire
//     (and for an unknown wire, "", the conservative reference:
//     a pre-send gate that does not know the wire yet must not
//     undercount). Native wires (Anthropic, Gemini, Cohere,
//     Bedrock) carry one role-label token per message instead —
//     measured by the audit at +1 token per message on every
//     non-tool class, with no per-message constant beyond the
//     role label.
//
// The result is still an ESTIMATE (the provider's own tokenizer
// may split the same text differently — vocabulary drift the audit
// cannot measure), but it counts everything the wire sends, so the
// systematic undercount is gone: the audit's post-fix residual is
// a few tokens per request, inside the router's materiality floor
// (20 tokens / 10%). Callers that need the wire-agnostic
// reference (pre-send budget gates, which run before routing)
// use CountPromptFull.
func CountPromptWire(model, wire string, req provider.ChatRequest) (TokenCount, error) {
	base, err := CountPrompt(model, req.Messages)
	if err != nil {
		return TokenCount{}, err
	}
	total := base.Tokens
	estimated := base.Estimated
	if len(req.Tools) > 0 {
		toolsJSON, err := toolsJSONForWire(wire, req)
		if err != nil {
			return TokenCount{}, err
		}
		tools, err := Count(model, toolsJSON)
		if err != nil {
			return TokenCount{}, err
		}
		total += tools.Tokens
		estimated = estimated || tools.Estimated
	}
	n := len(req.Messages)
	switch wire {
	case provider.WireFormatOpenAI, "":
		// OpenAI cookbook framing (see the doc above); "" takes
		// the same rule as the conservative reference.
		total += 3*n + 3
	default:
		// Native wires: one role-label token per message.
		total += n
	}
	return TokenCount{Tokens: total, Encoding: base.Encoding, Fallback: base.Fallback, Estimated: estimated}, nil
}

// CountPromptFull is the wire-agnostic reference count: the
// OpenAI-rule framing (3 per message + 3 priming) plus tool
// definitions, for callers that estimate BEFORE the request is
// routed and so cannot know the wire — the pre-send budget gates
// and the context-window fit check. It overcounts a native-wire
// request by (3n+3) - n tokens (a few tokens: the safe direction
// for a fail-closed gate), and never undercounts one the way the
// old message-text-only count did.
func CountPromptFull(model string, req provider.ChatRequest) (TokenCount, error) {
	return CountPromptWire(model, provider.WireFormatOpenAI, req)
}

// toolsJSONForWire renders a request's tool definitions as the
// JSON the wire will carry, so the count matches what the
// provider receives. Most wires carry the OpenAI shape verbatim
// (Anthropic/Gemini/Cohere reshape the keys — input_schema,
// functionDeclarations — moving the count by a few tokens per
// tool, which the audit measures as a small safe-direction
// residual and does not chase). Bedrock is the exception that
// matters: Converse wraps every tool in toolSpec +
// inputSchema.json and adds toolChoice {"auto":{}}, which the
// OpenAI shape misses by ~9 tokens a request — a systematic
// undercount on tool-heavy Bedrock traffic, so Bedrock gets its
// reshaped JSON here. The shape mirrors
// wire.convertBedrockTools (name, description, parameters as the
// input-schema JSON — {"type":"object"} when the tool declares
// no parameters — plus the auto toolChoice the converter adds
// when no choice is given); the audit test fails if the two drift
// apart. A reshaping failure falls back to the OpenAI shape — a
// counting detail must never fail a request.
func toolsJSONForWire(wire string, req provider.ChatRequest) (string, error) {
	if wire != provider.WireFormatBedrock {
		raw, err := json.Marshal(req.Tools)
		if err != nil {
			return "", fmt.Errorf("tokencount: marshal tools: %w", err)
		}
		return string(raw), nil
	}
	type bedrockToolSpec struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	type bedrockTool struct {
		ToolSpec bedrockToolSpec `json:"toolSpec"`
	}
	type bedrockToolConfig struct {
		Tools      []bedrockTool  `json:"tools"`
		ToolChoice map[string]any `json:"toolChoice,omitempty"`
	}
	cfg := bedrockToolConfig{Tools: make([]bedrockTool, 0, len(req.Tools))}
	for _, t := range req.Tools {
		schema := map[string]any{"type": "object"}
		if len(t.Function.Parameters) > 0 {
			var decoded map[string]any
			if err := json.Unmarshal(t.Function.Parameters, &decoded); err != nil {
				// Unparseable parameters: fall back to the
				// OpenAI shape for the whole request (the wire
				// client will refuse it anyway — the estimate
				// must not be the thing that fails first).
				raw, err := json.Marshal(req.Tools)
				if err != nil {
					return "", fmt.Errorf("tokencount: marshal tools: %w", err)
				}
				return string(raw), nil
			}
			schema = decoded
		}
		cfg.Tools = append(cfg.Tools, bedrockTool{ToolSpec: bedrockToolSpec{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: map[string]any{"json": schema},
		}})
	}
	// No explicit tool_choice in the request means "auto" — the
	// converter adds toolChoice {"auto":{}}. An explicit "none"
	// omits toolChoice while the tools stay declared, so the
	// estimator omits it too; any other choice counts the auto
	// shape (the common case, and the larger count — the safe
	// direction).
	if !isToolChoiceNone(req.ToolChoice) {
		cfg.ToolChoice = map[string]any{"auto": map[string]any{}}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("tokencount: marshal bedrock tools: %w", err)
	}
	return string(raw), nil
}

// isToolChoiceNone reports whether a request's tool_choice selects
// "none" — the JSON string "none" or an object with type "none".
// Anything else (absent, "auto", "required", a named function)
// does not suppress toolChoice.
func isToolChoiceNone(toolChoice json.RawMessage) bool {
	if len(toolChoice) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(toolChoice, &s); err == nil {
		return s == "none"
	}
	var obj struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(toolChoice, &obj); err == nil {
		return obj.Type == "none"
	}
	return false
}

// CountPrompt counts the prompt tokens of a chat request: every
// message's content, text parts and tool-call arguments, joined in
// order. Roles, tool schemas and per-message framing are not
// counted (see the package doc) — the result is an estimate of
// the prompt side only, never of the completion. A base64 data
// URI inside a text string is deleted before counting (M7.9);
// see messageSegments.
//
// Array-form messages count ONCE (M1.9): provider.Message
// UnmarshalJSON sets BOTH Content (the concatenation of the
// text parts) and Parts for array content, so adding both
// counted array-form messages ~2x. messageSegments below takes
// the text parts when Parts is set and Content only otherwise;
// Parts is authoritative for array form.
//
// The joined length is measured before the string is built: an
// oversize prompt returns the chars/4 estimate (Estimated=true)
// without ever allocating the joined text.
func CountPrompt(model string, messages []provider.Message) (TokenCount, error) {
	encoding, fallback := encodingForModel(model)
	total := 0
	for _, m := range messages {
		messageSegments(m, func(s string) { total += len(s) + 1 })
	}
	if total > maxExactCountBytes {
		return TokenCount{
			Tokens:    total / 4,
			Encoding:  encoding,
			Fallback:  fallback,
			Estimated: true,
		}, nil
	}
	var sb strings.Builder
	sb.Grow(total)
	for _, m := range messages {
		messageSegments(m, func(s string) {
			sb.WriteString(s)
			sb.WriteByte('\n')
		})
	}
	return Count(model, sb.String())
}

// dataURIRe matches a base64 data URI embedded in text:
// data:<mime>[;params];base64,<base64 payload>. The payload is a
// contiguous run of base64 characters (standard or URL-safe
// alphabet, with padding); the match stops at the first character
// that cannot be base64, so trailing punctuation or prose after
// the URI is kept. Non-base64 data: URIs (data:text/plain,hello)
// have no ";base64," marker and are NOT matched — they count in
// full as ordinary text. The scheme/marker match is
// case-insensitive; the payload class already covers both cases.
// The "[^,\s]*" span covers the MIME type plus any ";param=value"
// segments before ";base64" (e.g. ";charset=utf-8"); it cannot
// cross a comma or whitespace, so it cannot swallow neighbouring
// text. A data URI is deleted outright (replaced with nothing):
// providers bill pasted base64 as ~0 input tokens for the image
// itself, and the surrounding text is then counted exactly.
//
// One ambiguity the regex alone cannot resolve: a payload glued
// directly to following letters ("…Jggg==suffix") — the greedy
// payload class swallows the letters, since they are valid
// base64. stripDataURIs repairs that case below: real base64
// padding ("=") only ever ends a payload, so text after the
// padding is restored. An UNPADDED payload glued to following
// text stays ambiguous and is swallowed whole — undecidable
// without a delimiter, and recorded as a known limitation.
var dataURIRe = regexp.MustCompile(`(?i)data:[^,\s]*;base64,[A-Za-z0-9+/=_-]*`)

// stripDataURIs deletes every base64 data URI in s, leaving the
// surrounding text untouched. It is a CountPrompt-level rule:
// raw Count still counts a URI in full (see
// TestCountStillCountsDataURIInFull).
func stripDataURIs(s string) string {
	// Fast path: lower-case once so every casing of the scheme
	// ("data:", "DATA:", "dAtA:", ...) reaches the (?i) regex.
	// (M7.9 review: the old three-spelling check let mixed-case
	// "dAtA:" bypass the strip entirely.)
	if !strings.Contains(strings.ToLower(s), "data:") {
		return s
	}
	return dataURIRe.ReplaceAllStringFunc(s, func(m string) string {
		// The first comma in the match is the payload
		// separator (the MIME span cannot contain one).
		comma := strings.IndexByte(m, ',')
		if comma < 0 {
			return ""
		}
		payload := m[comma+1:]
		if eq := strings.IndexByte(payload, '='); eq >= 0 {
			// Padding ends the payload: restore anything
			// the greedy payload class swallowed after it.
			end := eq
			for end < len(payload) && payload[end] == '=' {
				end++
			}
			return payload[end:]
		}
		return ""
	})
}

// messageSegments calls f for each countable text segment of m,
// in order, each followed by a '\n' separator in the joined
// text (the separator is the caller's job — see CountPrompt).
// A message's segments are its text parts when Parts is set
// (array form), else its Content (string form), plus every
// tool-call's arguments. Image parts carry no countable text
// and are skipped, as before.
//
// M7.9: a base64 data URI pasted INSIDE a text string is deleted
// from the segment before counting (stripDataURIs) — a pasted
// image blob is not text and must not cost a budget gate
// thousands of tokens. A segment that strips to empty contributes
// nothing (not even its '\n' separator), so a message that is
// only a data URI counts 0. Tool-call arguments are NOT stripped
// by design: they are JSON the provider bills as text, and a
// "data:…;base64" value there is payload — stripping it would
// hide real tokens.
func messageSegments(m provider.Message, f func(string)) {
	if len(m.Parts) > 0 {
		for _, p := range m.Parts {
			if p.Type == "text" && p.Text != "" {
				if s := stripDataURIs(p.Text); s != "" {
					f(s)
				}
			}
		}
	} else if m.Content != "" {
		if s := stripDataURIs(m.Content); s != "" {
			f(s)
		}
	}
	for _, tc := range m.ToolCalls {
		if tc.Function.Arguments != "" {
			f(tc.Function.Arguments)
		}
	}
}
