// Package reasoning is the router's provider replay state for
// reasoning (thinking-mode) content.
//
// Problem (OmniRoute docs/routing/REASONING_REPLAY.md, GoModel
// research-log entry 145 as supporting): several thinking-mode
// providers reject a follow-up turn unless the previous assistant
// message carries the original reasoning back — Anthropic requires
// its thinking blocks with their cryptographic signatures, Gemini
// requires thought signatures on the parts they came with, and
// DeepSeek-style OpenAI-compatible providers require
// reasoning_content. Typical clients strip that state from the
// history they replay, so the router must capture it from each
// response and restore it on the next turn — in the wire format
// the next target speaks.
//
// The canonical form is State: one Block per reasoning unit, in
// the order the upstream produced them. Each wire's converter
// translates a State into its own shape (or refuses, honestly):
//
//   - Anthropic: thinking blocks replay verbatim, signature and
//     all — the API rejects unsigned thinking blocks (the signature
//     is a tamper seal over the thinking text). Reasoning text
//     without an Anthropic signature cannot be sent as a thinking
//     block, so it is unrepresentable, never silently dropped.
//   - Gemini: reasoning text becomes a thought part
//     ({"text": ..., "thought": true}); a thought signature rides
//     on the part it came with (a functionCall part, or the thought
//     part itself). An Anthropic signature has no Gemini slot and
//     is dropped with a recorded reason — the text survives.
//   - OpenAI wire: reasoning text becomes the assistant message's
//     reasoning_content — but only for models that accept it
//     (DeepSeek reasoner/R1, QwQ, Qwen/GLM think models, Kimi K,
//     Mimo — the OmniRoute replay list). Any other OpenAI-wire
//     model has no reasoning field, so the hop fails honestly.
//   - Bedrock Converse: reasoningContent blocks replay verbatim
//     (reasoningText with its signature, redactedContent as-is).
//     This fixes the old "reasoningContent dropped by design"
//     gap (CHECKLIST 52): the Converse API supports
//     reasoningContent in requests, and the signature must be
//     included with all previous messages. Reasoning text without
//     a signature cannot be sent, so it is unrepresentable.
//   - Cohere Chat v2: no reasoning replay field exists, so any
//     reasoning state is unrepresentable on a Cohere target.
//
// The policy lives in exactly one place — Check. Wire converters
// call it before emitting, and the router calls it to label each
// attempt's handoff in the route trace, so the label and the
// behaviour can never drift apart (both are pinned by the same
// unit tests).
//
// Two rules the policy enforces everywhere:
//
//  1. Reasoning is NEVER replayed as fresh user content. A
//     thinking block is either translated into the target's own
//     reasoning slot, or the hop fails / records a drop — it is
//     never pasted into a user message.
//  2. A drop is never silent. When the reasoning TEXT survives in
//     another form but an opaque wire-specific signature cannot,
//     the hop proceeds and the trace records exactly what was
//     dropped and why. When the text itself cannot be carried, the
//     hop fails with *UnrepresentableError (the router fails over
//     to the next target; if none can carry it, the client gets an
//     honest 400, never a silently lobotomised answer).
//
// This package imports nothing internal: it is a leaf, so wire,
// provider, router and server can all use it without a cycle. It
// holds no prompt or answer text beyond the reasoning itself, and
// nothing in it is ever rendered to a client or a dashboard feed —
// the store's stats are counts only (see Store).
package reasoning

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Wire formats a reasoning state can be translated into. The names
// match provider.WireFormat* (this package cannot import provider,
// so the strings are duplicated here and pinned by test).
const (
	WireOpenAI    = "openai"
	WireAnthropic = "anthropic"
	WireGemini    = "gemini"
	WireCohere    = "cohere"
	WireBedrock   = "bedrock-converse"
)

// Block kinds. Kind is the origin shape of the reasoning unit; a
// block keeps the fields its origin wire needs to replay it, and
// translators read only the fields their target wire can use.
const (
	// KindThinking is an Anthropic thinking block: Text is the
	// thinking, Signature the cryptographic signature the API
	// issued with it. Without the signature the block cannot be
	// replayed anywhere that requires one.
	KindThinking = "thinking"
	// KindRedactedThinking is an Anthropic redacted_thinking
	// block: Data is the encrypted thinking, verbatim. It can
	// only ever replay to Anthropic; every other wire drops it
	// with a recorded reason.
	KindRedactedThinking = "redacted_thinking"
	// KindThought is plain reasoning text: DeepSeek's
	// reasoning_content, or a Gemini thought part's text. Text
	// is the reasoning; Signature, when set, is the Gemini
	// thought signature that arrived on the same part.
	KindThought = "thought"
	// KindThoughtSignature is a Gemini thought signature that
	// arrived on a function-call part: Signature is the
	// signature, Name the function it belongs to ("" when the
	// signature arrived on a part with no function call).
	KindThoughtSignature = "thought_signature"
	// KindBedrock is a Bedrock Converse reasoningContent block:
	// Data is the raw reasoningContent JSON, verbatim
	// ({"reasoningText": {"text", "signature"}} and/or
	// {"redactedContent": ...}). It replays verbatim to
	// Bedrock only; other wires read the text out of it (and
	// fail honestly when that text has no signature they can
	// use).
	KindBedrock = "bedrock"
)

// Block is one reasoning unit.
type Block struct {
	Kind      string          `json:"kind"`
	Text      string          `json:"text,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Name      string          `json:"name,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// State is the captured reasoning of one assistant turn: the
// blocks in the order the upstream produced them, plus where they
// came from (for the trace and for debugging — never rendered to
// a client).
type State struct {
	Blocks      []Block `json:"blocks"`
	SourceWire  string  `json:"source_wire,omitempty"`
	SourceModel string  `json:"source_model,omitempty"`
}

// Empty reports whether the state carries no reasoning.
func (s *State) Empty() bool {
	return s == nil || len(s.Blocks) == 0
}

// Clone deep-copies the state (block Data is raw JSON — copied, so
// a stored state can never be mutated through a replayed one).
func (s *State) Clone() *State {
	if s == nil {
		return nil
	}
	out := &State{SourceWire: s.SourceWire, SourceModel: s.SourceModel}
	out.Blocks = make([]Block, len(s.Blocks))
	for i, b := range s.Blocks {
		nb := b
		if len(b.Data) > 0 {
			nb.Data = append(json.RawMessage(nil), b.Data...)
		}
		out.Blocks[i] = nb
	}
	return out
}

// Add appends one block.
func (s *State) Add(b Block) {
	s.Blocks = append(s.Blocks, b)
}

// Status is the verdict of Check for one target wire.
type Status string

const (
	// StatusNone is an empty state: there is no reasoning to
	// hand off, so the hop carries no label.
	StatusNone Status = "none"
	// StatusPreserved is a verbatim replay: every block went
	// over in its own shape (same wire, e.g. Anthropic to
	// Anthropic, Bedrock to Bedrock).
	StatusPreserved Status = "preserved"
	// StatusTranslated is a faithful translation: the reasoning
	// text reached the target in its own slot, possibly with
	// opaque signatures dropped (recorded in the detail).
	StatusTranslated Status = "translated"
	// StatusDropped is a hop that proceeded with no reasoning
	// carried at all — every block was opaque metadata the
	// target wire has no slot for (e.g. a lone thought
	// signature to OpenAI). The detail names what was dropped
	// and why; the original stays in the replay store for a
	// later hop that can carry it.
	StatusDropped Status = "dropped"
	// StatusUnrepresentable is a hop that must not run: the
	// reasoning text itself cannot be carried on this wire, so
	// Check returns an *UnrepresentableError instead of a
	// lossy translation.
	StatusUnrepresentable Status = "unrepresentable"
)

// UnrepresentableError is returned when a target wire cannot carry
// a reasoning state at all. It is a hop failure, not a request
// failure: the router fails over to the next target, and only when
// no target can carry the state does the client see an honest 400.
// Detail names the wire and the reason — never any reasoning or
// prompt text.
type UnrepresentableError struct {
	Wire   string
	Detail string
}

func (e *UnrepresentableError) Error() string {
	if e == nil {
		return "reasoning state cannot be represented"
	}
	return "reasoning state cannot be represented on the " + e.Wire + " wire: " + e.Detail
}

// IsUnrepresentable reports whether err is (or wraps) a reasoning
// unrepresentability failure.
func IsUnrepresentable(err error) bool {
	var re *UnrepresentableError
	return errors.As(err, &re)
}

func unrepErr(wire, detail string) error {
	return &UnrepresentableError{Wire: wire, Detail: detail}
}

// Describe renders a Check verdict as the route-trace handoff
// label: the status word, plus the detail when there is one
// ("translated: thinking → gemini thought part; anthropic
// signature dropped (no gemini equivalent)"). "" for StatusNone —
// a hop with no reasoning state carries no label at all.
func Describe(status Status, detail string) string {
	if status == StatusNone {
		return ""
	}
	if detail == "" {
		return string(status)
	}
	return string(status) + ": " + detail
}

// Check decides what happens to s on one hop to (wire, model). It
// is the single source of truth for the translation policy: wire
// converters call it before emitting, and the router calls it to
// label the attempt. err is non-nil exactly when the status is
// StatusUnrepresentable. detail is a short human note naming block
// kinds, wires and counts — never reasoning or prompt text.
func Check(s *State, wire, model string) (Status, string, error) {
	if s == nil || len(s.Blocks) == 0 {
		return StatusNone, "", nil
	}
	switch wire {
	case WireAnthropic:
		return checkAnthropic(s)
	case WireGemini:
		return checkGemini(s)
	case WireOpenAI:
		return checkOpenAI(s, model)
	case WireCohere:
		return StatusUnrepresentable, "", unrepErr(wire, "cohere chat api v2 has no reasoning replay field")
	case WireBedrock:
		return checkBedrock(s)
	default:
		return StatusUnrepresentable, "", unrepErr(wire, "unknown wire format")
	}
}

// verdict accumulates a Check outcome block by block.
type verdict struct {
	keptVerbatim int
	translated   int
	dropped      int
	notes        []string
}

func (v *verdict) note(format string, args ...any) {
	v.notes = append(v.notes, fmt.Sprintf(format, args...))
}

// finish resolves the accumulated outcome: a fatal error wins over
// everything; otherwise translated beats preserved beats dropped.
func (v *verdict) finish() (Status, string) {
	detail := strings.Join(v.notes, "; ")
	switch {
	case v.translated > 0:
		return StatusTranslated, detail
	case v.keptVerbatim > 0:
		return StatusPreserved, detail
	default:
		return StatusDropped, detail
	}
}

// ParseBedrockReasoning parses a KindBedrock block's Data — the
// raw reasoningContent JSON — into its text, signature, and
// whether it carries redacted content only. Wire converters use
// it to read the text out of a Bedrock block when translating to
// another wire; the verbatim Data is what replays to Bedrock.
func ParseBedrockReasoning(data json.RawMessage) (text, signature string, redacted bool) {
	var raw struct {
		ReasoningContent *struct {
			ReasoningText *struct {
				Text      string `json:"text"`
				Signature string `json:"signature"`
			} `json:"reasoningText"`
			RedactedContent json.RawMessage `json:"redactedContent"`
		} `json:"reasoningContent"`
	}
	if len(data) == 0 || json.Unmarshal(data, &raw) != nil || raw.ReasoningContent == nil {
		return "", "", false
	}
	redacted = len(raw.ReasoningContent.RedactedContent) > 0
	if raw.ReasoningContent.ReasoningText != nil {
		text = raw.ReasoningContent.ReasoningText.Text
		signature = raw.ReasoningContent.ReasoningText.Signature
	}
	return text, signature, redacted
}

// checkAnthropic: thinking blocks replay verbatim with their
// signature; redacted_thinking replays verbatim. Reasoning text
// with no Anthropic signature cannot be sent as a thinking block
// — the API rejects unsigned thinking blocks — so it is
// unrepresentable, never silently dropped. A lone thought
// signature is metadata for a function call the message already
// carries, so it drops with a recorded reason.
func checkAnthropic(s *State) (Status, string, error) {
	v := &verdict{}
	thinking, redacted := 0, 0
	for _, b := range s.Blocks {
		switch b.Kind {
		case KindThinking:
			if strings.TrimSpace(b.Signature) == "" {
				return StatusUnrepresentable, "", unrepErr(WireAnthropic, "thinking block without a signature cannot be replayed to anthropic (the api rejects unsigned thinking blocks)")
			}
			v.keptVerbatim++
			thinking++
		case KindRedactedThinking:
			v.keptVerbatim++
			redacted++
		case KindThought:
			return StatusUnrepresentable, "", unrepErr(WireAnthropic, "reasoning text has no anthropic signature (thinking blocks must carry the signature the api issued)")
		case KindThoughtSignature:
			v.dropped++
			v.note("thought signature dropped (no anthropic equivalent; the function call itself is preserved)")
		case KindBedrock:
			brText, _, _ := ParseBedrockReasoning(b.Data)
			if strings.TrimSpace(brText) != "" {
				return StatusUnrepresentable, "", unrepErr(WireAnthropic, "bedrock reasoning text has no anthropic signature")
			}
			v.dropped++
			v.note("bedrock redacted content dropped (no anthropic equivalent)")
		default:
			return StatusUnrepresentable, "", unrepErr(WireAnthropic, fmt.Sprintf("unknown reasoning block kind %q", b.Kind))
		}
	}
	if thinking > 0 {
		v.note("%d thinking block(s) replayed verbatim", thinking)
	}
	if redacted > 0 {
		v.note("%d redacted thinking block(s) replayed verbatim", redacted)
	}
	st, detail := v.finish()
	return st, detail, nil
}

// checkGemini: reasoning text becomes a thought part; a
// Gemini-issued thought signature rides on its part. An Anthropic
// or Bedrock signature has no Gemini slot — the text survives, the
// signature drops with a recorded reason. Redacted thinking is
// opaque and drops.
func checkGemini(s *State) (Status, string, error) {
	v := &verdict{}
	for _, b := range s.Blocks {
		switch b.Kind {
		case KindThinking:
			v.translated++
			v.note("thinking text → gemini thought part; anthropic signature dropped (no gemini equivalent)")
		case KindRedactedThinking:
			v.dropped++
			v.note("redacted thinking dropped (no gemini equivalent; the original is kept in the replay store)")
		case KindThought:
			v.translated++
			if b.Signature != "" && s.SourceWire == WireGemini {
				v.note("thought text → gemini thought part; thought signature preserved")
			} else if b.Signature != "" {
				v.note("thought text → gemini thought part; signature dropped (not a gemini signature)")
			} else {
				v.note("thought text → gemini thought part")
			}
		case KindThoughtSignature:
			v.keptVerbatim++
			v.note("thought signature replayed on its part")
		case KindBedrock:
			brText, _, _ := ParseBedrockReasoning(b.Data)
			if strings.TrimSpace(brText) != "" {
				v.translated++
				v.note("reasoning text → gemini thought part; bedrock signature dropped (no gemini equivalent)")
			} else {
				v.dropped++
				v.note("bedrock redacted content dropped (no gemini equivalent)")
			}
		default:
			return StatusUnrepresentable, "", unrepErr(WireGemini, fmt.Sprintf("unknown reasoning block kind %q", b.Kind))
		}
	}
	st, detail := v.finish()
	return st, detail, nil
}

// checkOpenAI: reasoning text becomes the assistant message's
// reasoning_content — but only for models that accept it (see
// ReplayModel). Any other OpenAI-wire model has no reasoning
// field, so text-bearing state is unrepresentable there. Lone
// signatures / redacted blocks drop with a recorded reason.
func checkOpenAI(s *State, model string) (Status, string, error) {
	replay := ReplayModel(model)
	v := &verdict{}
	for _, b := range s.Blocks {
		switch b.Kind {
		case KindThinking, KindThought:
			if !replay {
				return StatusUnrepresentable, "", unrepErr(WireOpenAI, fmt.Sprintf("model %q does not accept reasoning_content (openai chat completions has no reasoning field for it)", model))
			}
			v.translated++
			v.note("reasoning text → reasoning_content; signature dropped (no openai equivalent)")
		case KindThoughtSignature:
			v.dropped++
			v.note("thought signature dropped (no openai equivalent; the function call itself is preserved)")
		case KindRedactedThinking:
			v.dropped++
			v.note("redacted thinking dropped (no openai equivalent)")
		case KindBedrock:
			brText, _, _ := ParseBedrockReasoning(b.Data)
			if strings.TrimSpace(brText) != "" {
				if !replay {
					return StatusUnrepresentable, "", unrepErr(WireOpenAI, fmt.Sprintf("model %q does not accept reasoning_content (openai chat completions has no reasoning field for it)", model))
				}
				v.translated++
				v.note("reasoning text → reasoning_content; bedrock signature dropped (no openai equivalent)")
			} else {
				v.dropped++
				v.note("bedrock redacted content dropped (no openai equivalent)")
			}
		default:
			return StatusUnrepresentable, "", unrepErr(WireOpenAI, fmt.Sprintf("unknown reasoning block kind %q", b.Kind))
		}
	}
	st, detail := v.finish()
	return st, detail, nil
}

// checkBedrock: reasoningContent blocks replay verbatim — the
// Converse API supports them in requests, signature included.
// Reasoning text without a signature cannot be sent (Converse
// reasoningText must carry the signature the API issued), so it
// is unrepresentable. Lone signatures and redacted thinking drop
// with a recorded reason.
func checkBedrock(s *State) (Status, string, error) {
	v := &verdict{}
	for _, b := range s.Blocks {
		switch b.Kind {
		case KindBedrock:
			v.keptVerbatim++
		case KindThoughtSignature:
			v.dropped++
			v.note("thought signature dropped (no bedrock equivalent; the function call itself is preserved)")
		case KindRedactedThinking:
			v.dropped++
			v.note("redacted thinking dropped (no bedrock equivalent)")
		case KindThinking, KindThought:
			return StatusUnrepresentable, "", unrepErr(WireBedrock, "reasoning text has no bedrock signature (converse reasoningtext must carry the signature the api issued)")
		default:
			return StatusUnrepresentable, "", unrepErr(WireBedrock, fmt.Sprintf("unknown reasoning block kind %q", b.Kind))
		}
	}
	if v.keptVerbatim > 0 {
		v.note("%d reasoningcontent block(s) replayed verbatim", v.keptVerbatim)
	}
	st, detail := v.finish()
	return st, detail, nil
}

// replayModelPatterns is the model list that accepts
// reasoning_content on the OpenAI wire, taken from OmniRoute's
// requiresReasoningReplay model patterns
// (docs/routing/REASONING_REPLAY.md): DeepSeek reasoner/R1,
// DeepSeek V4 Flash/Pro, Kimi K, QwQ, Qwen/GLM think models,
// Mimo. The patterns are unanchored (a "provider/model" id
// still matches) except Mimo, which is anchored like
// OmniRoute's. OpenAI's own reasoning models (o1/o3/o4/gpt-5)
// are deliberately NOT here: chat completions takes no
// reasoning_content for them — their reasoning travels on the
// Responses API, which this router does not replay.
var replayModelPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)deepseek-r1`),
	regexp.MustCompile(`(?i)deepseek-reasoner`),
	regexp.MustCompile(`(?i)deepseek-chat`),
	regexp.MustCompile(`(?i)deepseek[-/]?v4[-.]flash`),
	regexp.MustCompile(`(?i)deepseek[-/]?v4[-.]pro`),
	regexp.MustCompile(`(?i)(deepseek|zen/deepseek)-v4`),
	regexp.MustCompile(`(?i)kimi[-/]k\d`),
	regexp.MustCompile(`(?i)qwq`),
	regexp.MustCompile(`(?i)qwen.*think`),
	regexp.MustCompile(`(?i)glm.*think`),
	regexp.MustCompile(`(?i)^mimo[-.]?v\d`),
}

// ReplayModel reports whether an OpenAI-wire model accepts
// reasoning_content in its assistant history, and so can carry a
// reasoning state across a provider switch.
func ReplayModel(model string) bool {
	for _, re := range replayModelPatterns {
		if re.MatchString(model) {
			return true
		}
	}
	return false
}

// StoredBytes is the byte size of the state AS STORED: every
// block's text PLUS its opaque payload — the Signature (Anthropic
// thinking / Gemini thought signatures) and the raw Data blob
// (redacted_thinking payloads, Bedrock reasoningContent JSON).
// ReasoningText counts text only, so a state whose text is short
// but whose signature is long used to count as nearly free against
// the reasoning cache's 2 MiB budget (G13: signatures/redacted
// payloads were uncounted). StoredBytes is the accounting the
// cache caps use (internal/cache ReasoningCache): the per-entry
// and total caps apply to the bytes actually held, not just the
// text view. No double-counting in practice: a block carries its
// payload in exactly one of Text / Signature / Data — Bedrock
// blocks carry text+signature inside Data with Text empty, and
// thinking blocks carry Text+Signature with Data empty — so
// summing the three fields counts each byte once.
func (s *State) StoredBytes() int {
	if s == nil {
		return 0
	}
	n := 0
	for _, b := range s.Blocks {
		n += len(b.Text) + len(b.Signature) + len(b.Data)
	}
	return n
}

// ReasoningText renders the state's reasoning TEXT as one string
// for wires that carry it as a single field (OpenAI
// reasoning_content): the text-bearing blocks in order, joined
// with a blank line. Signatures and redacted blocks contribute
// nothing — they have no text form on those wires. "" when the
// state carries no text at all.
func (s *State) ReasoningText() string {
	if s == nil {
		return ""
	}
	var parts []string
	for _, b := range s.Blocks {
		switch b.Kind {
		case KindThinking, KindThought:
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		case KindBedrock:
			if brText, _, _ := ParseBedrockReasoning(b.Data); strings.TrimSpace(brText) != "" {
				parts = append(parts, brText)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
