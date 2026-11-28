// Package reasoning is the router's provider replay state for
// reasoning (thinking-mode) content.
//
// Problem (the sector baseline docs/routing/REASONING_REPLAY.md, GoModel
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
//     Mimo — the the sector baseline replay list). Any other OpenAI-wire
//     model has no reasoning field, so the hop fails honestly.
//   - Bedrock Converse: reasoningContent blocks replay verbatim
//     (reasoningText with its signature, redactedContent as-is).
//     This fixes the old "reasoningContent dropped by design"
//     gap (CHECKLIST 52): the Converse API supports
//     reasoningContent in requests, and the signature must be
//     included with all previous messages. Reasoning text without
//     a signature cannot be sent, so it is unrepresentable.
//   - Cohere Chat v2: reasoning rides the assistant message's
//     unsigned thinking content block ({"type":"thinking",
//     "thinking": ...}). The old stance — "no reasoning replay
//     field exists, so any state is unrepresentable" — is a
//     recorded divergence: Cohere's own v2 docs (reference/chat,
//     docs/reasoning; read 2026-11-27) document thinking blocks
//     in the assistant message content of chat requests, with no
//     signature field, so the block replays unsigned or not at
//     all. Replay is model-gated exactly like the OpenAI wire:
//     only Cohere's reasoning models (the command-a-reasoning
//     family, hybrid models with thinking on by default) accept
//     thinking blocks; any other Cohere model has no reasoning
//     slot, so text-bearing state stays unrepresentable there.
//   - Vertex AI: speaks the Gemini generateContent shape (the
//     provider client converts Vertex requests through the Gemini
//     converters), so it follows the Gemini strategy exactly
//     instead of the old blanket "unknown wire" refusal.
//   - Unknown wires: an unrecognized wire format rides the
//     OpenAI-compatible passthrough client, so its verdict is the
//     OpenAI-compatible one: the reasoning text is carried
//     opaquely in reasoning_content — a pass-through field the
//     router attaches verbatim and never interprets, and the
//     target either surfaces or ignores — for models documented
//     to accept it. Unrepresentable is reserved for the provable
//     drop: a model with no reasoning field on the passthrough
//     (not on the replay list) fails the hop with the reason
//     stated, instead of the old blanket "unknown wire format"
//     refusal that mislabeled hops which actually carried the
//     text.
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
	// WireVertex is Vertex AI's Gemini generateContent surface
	// (provider.WireFormatVertex): the same request/response shape
	// as WireGemini, served from Vertex AI. The reasoning strategy
	// is the Gemini one — the provider client converts Vertex
	// requests through the Gemini converters, so a thought part
	// replayed to Vertex behaves exactly as it does on Gemini.
	WireVertex = "vertex"
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
	case WireVertex:
		// Vertex speaks the Gemini shape (vertex requests are
		// converted through the Gemini converters), so its
		// verdict is Gemini's — never the unknown-wire refusal.
		return checkGemini(s)
	case WireOpenAI:
		return checkOpenAI(s, model)
	case WireCohere:
		return checkCohere(s, model)
	case WireBedrock:
		return checkBedrock(s)
	default:
		return checkUnknown(s, wire, model)
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

// cohereReplayModelPatterns is the model list that accepts
// reasoning on the Cohere wire, from Cohere's own docs
// (docs.cohere.com — docs/reasoning and docs/command-a-reasoning,
// read 2026-11-27): the command-a-reasoning family, Cohere's
// hybrid reasoning models, whose thinking content blocks ride the
// Chat API v2 assistant message with thinking enabled by default.
// The pattern is unanchored so a dated model id
// ("command-a-reasoning-08-2025") matches.
var cohereReplayModelPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)command-a-reasoning`),
}

// CohereReplayModel reports whether a Cohere-wire model accepts
// thinking content blocks in its assistant history, and so can
// carry a reasoning state across a provider switch.
func CohereReplayModel(model string) bool {
	for _, re := range cohereReplayModelPatterns {
		if re.MatchString(model) {
			return true
		}
	}
	return false
}

// checkCohere: reasoning text becomes the assistant message's
// thinking content block — the documented Cohere Chat API v2
// shape ({"type":"thinking","thinking": ...}). The block schema
// carries no signature, so any foreign signature drops with a
// recorded reason and nothing is fabricated; the block replays
// unsigned or not at all. Replay is model-gated like the OpenAI
// wire: only Cohere's reasoning models accept thinking blocks —
// any other Cohere model has no reasoning slot, so text-bearing
// state is unrepresentable there.
//
// Divergence from the old stance, recorded deliberately: every
// state used to be refused here ("cohere chat api v2 has no
// reasoning replay field"), which the v2 docs disprove — the
// assistant-message content schema lists the thinking block
// alongside text, so a text-bearing state CAN be carried, and the
// blanket refusal mislabeled hops to reasoning models that could
// actually take it.
func checkCohere(s *State, model string) (Status, string, error) {
	replay := CohereReplayModel(model)
	v := &verdict{}
	for _, b := range s.Blocks {
		switch b.Kind {
		case KindThinking, KindThought:
			if !replay {
				return StatusUnrepresentable, "", unrepErr(WireCohere, fmt.Sprintf("model %q does not accept thinking content blocks (cohere chat api v2 carries reasoning only on its reasoning models)", model))
			}
			v.translated++
			v.note("reasoning text → cohere thinking content block; signature dropped (cohere thinking blocks are unsigned)")
		case KindThoughtSignature:
			v.dropped++
			v.note("thought signature dropped (no cohere equivalent; the function call itself is preserved)")
		case KindRedactedThinking:
			v.dropped++
			v.note("redacted thinking dropped (no cohere equivalent)")
		case KindBedrock:
			brText, _, _ := ParseBedrockReasoning(b.Data)
			if strings.TrimSpace(brText) != "" {
				if !replay {
					return StatusUnrepresentable, "", unrepErr(WireCohere, fmt.Sprintf("model %q does not accept thinking content blocks (cohere chat api v2 carries reasoning only on its reasoning models)", model))
				}
				v.translated++
				v.note("reasoning text → cohere thinking content block; bedrock signature dropped (no cohere equivalent)")
			} else {
				v.dropped++
				v.note("bedrock redacted content dropped (no cohere equivalent)")
			}
		default:
			return StatusUnrepresentable, "", unrepErr(WireCohere, fmt.Sprintf("unknown reasoning block kind %q", b.Kind))
		}
	}
	st, detail := v.finish()
	return st, detail, nil
}

// checkUnknown: an unrecognized wire format rides the router's
// OpenAI-compatible passthrough client (the client builder speaks
// "everything else" as OpenAI-compatible chat completions), so its
// verdict is the OpenAI-compatible one, worded for the unknown
// wire: the reasoning text is carried opaquely in the assistant
// message's reasoning_content — a pass-through field the router
// attaches verbatim and never interprets, and the target either
// surfaces or ignores. The model gate is the same replay list the
// OpenAI wire applies. Unrepresentable is reserved for the
// provable drop — a model with no reasoning field on the
// passthrough fails the hop with the reason stated — never the
// old blanket "unknown wire format" refusal, which mislabeled
// hops whose passthrough actually carried the text (a
// replay-capable model behind an unrecognized wire format always
// did). Opaque metadata (signatures, redacted payloads) drops
// with a recorded reason.
func checkUnknown(s *State, wire, model string) (Status, string, error) {
	replay := ReplayModel(model)
	v := &verdict{}
	for _, b := range s.Blocks {
		switch b.Kind {
		case KindThinking, KindThought:
			if !replay {
				return StatusUnrepresentable, "", unrepErr(wire, fmt.Sprintf("model %q does not accept reasoning_content (the %q wire rides the openai-compatible passthrough, which has no reasoning field for it)", model, wire))
			}
			v.translated++
			v.note("reasoning text → reasoning_content pass-through (opaque; the target surfaces or ignores it)")
		case KindThoughtSignature:
			v.dropped++
			v.note("thought signature dropped (no passthrough equivalent; the function call itself is preserved)")
		case KindRedactedThinking:
			v.dropped++
			v.note("redacted thinking dropped (no passthrough equivalent)")
		case KindBedrock:
			brText, _, _ := ParseBedrockReasoning(b.Data)
			if strings.TrimSpace(brText) != "" {
				if !replay {
					return StatusUnrepresentable, "", unrepErr(wire, fmt.Sprintf("model %q does not accept reasoning_content (the %q wire rides the openai-compatible passthrough, which has no reasoning field for it)", model, wire))
				}
				v.translated++
				v.note("reasoning text → reasoning_content pass-through (opaque; the target surfaces or ignores it)")
			} else {
				v.dropped++
				v.note("bedrock redacted content dropped (no passthrough equivalent)")
			}
		default:
			return StatusUnrepresentable, "", unrepErr(wire, fmt.Sprintf("unknown reasoning block kind %q", b.Kind))
		}
	}
	st, detail := v.finish()
	return st, detail, nil
}

// replayModelPatterns is the model list that accepts
// reasoning_content on the OpenAI wire, taken from the sector-typical
// requiresReasoningReplay model patterns
// (docs/routing/REASONING_REPLAY.md): DeepSeek reasoner/R1,
// DeepSeek V4 Flash/Pro, Kimi K, QwQ, Qwen/GLM think models,
// Mimo. The patterns are unanchored (a "provider/model" id
// still matches) except Mimo, which is anchored like
// the sector-typical. OpenAI's own reasoning models (o1/o3/o4/gpt-5)
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

// CohereThinkingBlock is one documented Cohere Chat API v2
// thinking content block — the replay shape for a Cohere
// reasoning model's assistant message. The v2 chat reference
// gives the block exactly two fields and no signature: an
// unsigned block is the only thinking block the API accepts in a
// request.
type CohereThinkingBlock struct {
	Type     string `json:"type"` // always "thinking"
	Thinking string `json:"thinking"`
}

// CohereThinkingBlocks renders the state's text-bearing blocks in
// order as documented Cohere v2 thinking content blocks — the
// emission form the cohere wire converter prepends to the
// assistant message's content list when it carries a restored
// state (Check already proved the target model accepts it).
// Signatures, thought signatures and redacted payloads have no
// Cohere form and are skipped here exactly as Check records them
// dropped. nil when the state carries no text.
func (s *State) CohereThinkingBlocks() []CohereThinkingBlock {
	if s == nil {
		return nil
	}
	var out []CohereThinkingBlock
	for _, b := range s.Blocks {
		switch b.Kind {
		case KindThinking, KindThought:
			if strings.TrimSpace(b.Text) != "" {
				out = append(out, CohereThinkingBlock{Type: "thinking", Thinking: b.Text})
			}
		case KindBedrock:
			if brText, _, _ := ParseBedrockReasoning(b.Data); strings.TrimSpace(brText) != "" {
				out = append(out, CohereThinkingBlock{Type: "thinking", Thinking: brText})
			}
		}
	}
	return out
}

// PlaceholderText is the documented reasoning placeholder: a
// stable, content-free sentinel in the same bracketed-token form
// the redaction placeholders use ([REDACTED:<label>] /
// [VAULT:<kind>:<id>]) — it marks WHERE reasoning was, never
// carries any. It rides the target's own reasoning slot
// (reasoning_content, a thinking content block), never user
// content, and tells the downstream model honestly that the
// original reasoning for this turn was not retained.
const PlaceholderText = "[REASONING:OMITTED:TOOL_CALL_TURN]"

// ToolCallPlaceholder returns the documented placeholder reasoning
// state for an assistant turn that made tool calls and whose
// original reasoning was not retained (the client stripped it, or
// the replay store missed after a restart), plus whether THIS
// (wire, model) should receive it. Injection is deliberately
// narrow: the target must admit an unsigned placeholder in its
// reasoning slot for this model — the same gate Check applies to
// any state, so an admitted placeholder never fails the hop:
//
//   - OpenAI wire and unknown (passthrough) wires: replay-capable
//     models only — reasoning_content takes any text.
//   - Cohere: its reasoning models only — the documented v2
//     thinking content block carries no signature, so an unsigned
//     placeholder is representable.
//   - Anthropic: never — the API rejects unsigned thinking blocks,
//     so a fabricated placeholder would be unrepresentable anyway;
//     the hop proceeds bare (a store miss is a miss, never a wrong
//     replay).
//   - Gemini / Vertex: never — continuity there is the per-part
//     thought signature, which cannot be fabricated; a
//     signature-less thought part does not satisfy it.
//   - Bedrock: never — converse reasoningText must carry the
//     signature the API issued.
//
// ok is false (with a nil state) when this target has no
// placeholder slot: the caller sends the turn without reasoning,
// exactly as before this existed.
func ToolCallPlaceholder(wire, model string) (*State, bool) {
	var slot bool
	switch wire {
	case WireOpenAI:
		slot = ReplayModel(model)
	case WireCohere:
		slot = CohereReplayModel(model)
	case WireAnthropic, WireGemini, WireVertex, WireBedrock:
		return nil, false
	default:
		// Unknown wire: the OpenAI-compatible passthrough slot.
		slot = ReplayModel(model)
	}
	if !slot {
		return nil, false
	}
	return &State{Blocks: []Block{{Kind: KindThought, Text: PlaceholderText}}}, true
}

// ReasoningOnlyDecision is the turn-level verdict for a response
// whose payload is (or may be) reasoning only.
type ReasoningOnlyDecision struct {
	// IsReasoningOnly: the response carried no visible text and no
	// tool calls, but real reasoning output.
	IsReasoningOnly bool
	// KeepTurn: serve the response as an assistant turn instead of
	// treating it as an empty completion (which fails over). A
	// reasoning-only turn is kept: the model spent its budget
	// thinking, and failing over cannot improve the same budget on
	// another target — the live-verified empty-completion policy
	// already exempts reasoning-only completions from the empty
	// set, and this is the documented source of that decision.
	KeepTurn bool
	// VisibleContent is what the client sees: "" for a
	// reasoning-only turn — reasoning is never rendered as content
	// (it travels on the response's reasoning state and in the
	// replay store only). For a normal turn it is the text itself.
	VisibleContent string
	// Capturable: the reasoning state carries text worth storing
	// for next-turn replay. A state of opaque metadata only
	// (signatures, redacted payloads) has nothing to replay.
	Capturable bool
}

// ReasoningOnlyTurn decides how to serve a response that contains
// ONLY reasoning content — no text, no tool calls (toolCalls is
// the response's tool-call count). The documented behavior: the
// turn is KEPT and served as an empty assistant turn with the
// reasoning attached (captured for the next turn's replay), never
// failed over and never rendered to the client; a response with
// no text-bearing reasoning at all remains a plain empty
// completion.
func ReasoningOnlyTurn(content string, toolCalls int, s *State) ReasoningOnlyDecision {
	if strings.TrimSpace(content) != "" || toolCalls > 0 {
		return ReasoningOnlyDecision{
			IsReasoningOnly: false,
			KeepTurn:        true,
			VisibleContent:  content,
			Capturable:      !s.Empty() && strings.TrimSpace(s.ReasoningText()) != "",
		}
	}
	if !s.Empty() && strings.TrimSpace(s.ReasoningText()) != "" {
		return ReasoningOnlyDecision{
			IsReasoningOnly: true,
			KeepTurn:        true,
			VisibleContent:  "",
			Capturable:      true,
		}
	}
	// No text-bearing reasoning: an empty completion, not a
	// reasoning-only turn.
	return ReasoningOnlyDecision{}
}
