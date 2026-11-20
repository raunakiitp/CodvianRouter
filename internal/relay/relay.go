// Package relay implements the byte-for-byte native relay rule
// (research-log entries 83, 145, 179):
//
//   - When a request's provider matches the client's dialect, forward
//     it verbatim — only the top-level "model" field is rewritten.
//     Translation runs ONLY when a feature needs the canonical form
//     (guardrails, cache, failover, forced reasoning).
//   - cache_control and the Anthropic SSE event envelope pass through
//     untouched on the verbatim path: the relay never parses, rebuilds
//     or re-encodes the body, so provider-native fields no translation
//     preserves survive.
//
// The translate path (Anthropic Messages -> the gateway's canonical
// OpenAI-shaped request, and back) lives here too, so the rule's
// other half is testable in one place. What the canonical form cannot
// carry (cache_control blocks, thinking blocks) is dropped — loudly,
// as an error — on the translate path only; that loss is exactly what
// the verbatim path exists to avoid, and the decision function makes
// it visible instead of silent.
//
// Routing contract (CHECKLIST 211): the per-request routing
// contract — the canonical "routing" field (order / only / exclude /
// sort / max_price / data_collection / group, the same shape as on
// /v1/chat/completions, CHECKLIST 157) — is parsed by
// ParseAnthropicDetailed and carried on ParseResult.Routing, so the
// translate path honours it exactly as chat completions does (same
// engine validation, same fail-closed 400s/402, same trace). A
// non-empty contract is a canonical-form need: Features.RoutingPresent
// forces translation, so the verbatim path is contract-free BY
// CONSTRUCTION — a request carrying a contract can never be forwarded
// verbatim, and a contract on a verbatim request is therefore
// impossible, never silently ignored. The verbatim path forwards the
// body untouched, so it cannot apply (or even see) a contract; that
// is by design, documented here and in the server handler, and
// pinned by test.
package relay

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ---------------------------------------------------------------------------
// The decision.
// ---------------------------------------------------------------------------

// Features are the canonical-form needs that force translation. Every
// field is a real signal the server computes per request; a feature
// that is off does not force anything.
type Features struct {
	// GuardrailsBlocking is true when any blocking guardrail toggle
	// is on: the request text must be inspected before any provider
	// is dialled, which needs the canonical form.
	GuardrailsBlocking bool
	// CacheServing is true when the gateway cache is serving this
	// request — a cache hit was found for it. The server consults
	// the cache first and only sets this on a hit; a miss relays
	// verbatim, so the cache never forces a translation it cannot
	// serve. Set it only when the serving cache is actually on.
	CacheServing bool
	// MultiAttempt is true when the model resolves to more than one
	// provider: failover needs the canonical form to retry the same
	// request against the next target.
	MultiAttempt bool
	// ReasoningForced is true when the operator forces reasoning on
	// a request whose native dialect cannot express it. No
	// forced-reasoning policy exists yet, so the server always passes
	// false today; the field exists so the rule is enforced the
	// moment one lands, instead of the verbatim path silently
	// skipping it.
	ReasoningForced bool
	// RoutingPresent is true when the request carries a non-empty
	// per-request routing contract (CHECKLIST 211): the contract
	// is a canonical-form need — only the engine can apply
	// order/only/exclude/sort/max_price/data_collection/group —
	// so a contracted request is always translated, never relayed
	// verbatim. An empty contract (or none) is the same as no
	// contract and does not force anything. This is what keeps
	// the verbatim path contract-free by construction: a contract
	// can never reach it, so it can never be silently ignored
	// there.
	RoutingPresent bool
}

// Decide applies the native-relay rule: verbatim when the client's
// dialect is the provider's native wire AND no feature needs the
// canonical form. The returned reason names the deciding factor, so
// the server can report it (X-Codvian-Relay) and tests can assert on
// it. Verbatim never depends on the model name or the body content.
func Decide(clientDialect, targetWire string, f Features) (verbatim bool, reason string) {
	if clientDialect != targetWire {
		return false, fmt.Sprintf("client dialect %q does not match provider wire %q", clientDialect, targetWire)
	}
	switch {
	case f.GuardrailsBlocking:
		return false, "guardrails blocking is on"
	case f.CacheServing:
		return false, "gateway cache is serving this request"
	case f.MultiAttempt:
		return false, "failover chain has more than one attempt"
	case f.ReasoningForced:
		return false, "reasoning is forced"
	case f.RoutingPresent:
		return false, "routing contract present"
	default:
		return true, "dialect matches provider wire and no feature needs the canonical form"
	}
}

// ---------------------------------------------------------------------------
// Byte-preserving model rewrite.
// ---------------------------------------------------------------------------

// ErrNoModel is returned when the body has no top-level "model" field
// (or it is not a JSON string): there is nothing to rewrite, and
// inventing a model would be guessing.
var ErrNoModel = errors.New("relay: request body has no top-level \"model\" string")

// RewriteModel rewrites ONLY the top-level "model" string of a JSON
// object body, returning new bytes. Everything else — whitespace,
// key order, unknown fields, nested "model" keys, cache_control,
// thinking blocks — is passed through byte-for-byte: the input is
// never decoded into a struct and re-encoded. A body that is not a
// JSON object, or has no top-level string "model", is an error.
func RewriteModel(raw []byte, model string) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, ErrNoModel
	}
	// Validate the whole body first: the splice below only rewrites
	// the model value, so a truncated body would otherwise come back
	// "rewritten" and still malformed.
	if !json.Valid(raw) {
		return nil, fmt.Errorf("relay: request body is not valid JSON")
	}
	s := &scanner{b: raw}
	if err := s.skipWS(); err != nil {
		return nil, err
	}
	if err := s.expect('{'); err != nil {
		return nil, fmt.Errorf("relay: request body is not a JSON object: %w", err)
	}
	for {
		if err := s.skipWS(); err != nil {
			return nil, err
		}
		if s.peek() == '}' {
			// Walked the whole object without a "model" key.
			return nil, ErrNoModel
		}
		keyStart, key, err := s.string()
		if err != nil {
			return nil, err
		}
		_ = keyStart
		if err := s.skipWS(); err != nil {
			return nil, err
		}
		if err := s.expect(':'); err != nil {
			return nil, err
		}
		if err := s.skipWS(); err != nil {
			return nil, err
		}
		if key == "model" {
			if s.peek() != '"' {
				return nil, fmt.Errorf("relay: top-level \"model\" is not a string")
			}
			valStart := s.pos
			if _, _, err := s.string(); err != nil {
				return nil, fmt.Errorf("relay: top-level \"model\" is not a string: %w", err)
			}
			repl, err := json.Marshal(model)
			if err != nil {
				return nil, err
			}
			out := make([]byte, 0, len(raw)-(s.pos-valStart)+len(repl))
			out = append(out, raw[:valStart]...)
			out = append(out, repl...)
			out = append(out, raw[s.pos:]...)
			return out, nil
		}
		if err := s.skipValue(); err != nil {
			return nil, err
		}
		if err := s.skipWS(); err != nil {
			return nil, err
		}
		switch s.peek() {
		case ',':
			s.pos++
		case '}':
			return nil, ErrNoModel
		default:
			return nil, fmt.Errorf("relay: malformed JSON object at offset %d", s.pos)
		}
	}
}

// scanner is a minimal JSON cursor: it walks values without decoding
// them, so RewriteModel can splice one string in place.
type scanner struct {
	b   []byte
	pos int
}

func (s *scanner) peek() byte {
	if s.pos >= len(s.b) {
		return 0
	}
	return s.b[s.pos]
}

func (s *scanner) skipWS() error {
	for s.pos < len(s.b) {
		switch s.b[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return nil
		}
	}
	return io.ErrUnexpectedEOF
}

func (s *scanner) expect(c byte) error {
	if s.pos >= len(s.b) || s.b[s.pos] != c {
		return fmt.Errorf("relay: expected %q at offset %d", c, s.pos)
	}
	s.pos++
	return nil
}

// string parses a JSON string at the cursor. It returns the start
// offset, the decoded value, and whether the value was a string at
// all (false when the value starts with any other token — the caller
// then knows the "model" value is not a string).
func (s *scanner) string() (int, string, error) {
	if s.pos >= len(s.b) || s.b[s.pos] != '"' {
		return s.pos, "", nil
	}
	start := s.pos
	var sb strings.Builder
	s.pos++ // opening quote
	for s.pos < len(s.b) {
		c := s.b[s.pos]
		switch c {
		case '"':
			s.pos++
			return start, sb.String(), nil
		case '\\':
			if s.pos+1 >= len(s.b) {
				return start, "", io.ErrUnexpectedEOF
			}
			esc := s.b[s.pos+1]
			switch esc {
			case '"', '\\', '/':
				sb.WriteByte(esc)
				s.pos += 2
			case 'b':
				sb.WriteByte('\b')
				s.pos += 2
			case 'f':
				sb.WriteByte('\f')
				s.pos += 2
			case 'n':
				sb.WriteByte('\n')
				s.pos += 2
			case 'r':
				sb.WriteByte('\r')
				s.pos += 2
			case 't':
				sb.WriteByte('\t')
				s.pos += 2
			case 'u':
				if s.pos+6 > len(s.b) {
					return start, "", io.ErrUnexpectedEOF
				}
				var r rune
				if _, err := fmt.Sscanf(string(s.b[s.pos+2:s.pos+6]), "%04x", &r); err != nil {
					return start, "", fmt.Errorf("relay: bad unicode escape at offset %d", s.pos)
				}
				sb.WriteRune(r)
				s.pos += 6
			default:
				return start, "", fmt.Errorf("relay: bad escape at offset %d", s.pos)
			}
		default:
			sb.WriteByte(c)
			s.pos++
		}
	}
	return start, "", io.ErrUnexpectedEOF
}

// skipValue skips one JSON value of any shape.
func (s *scanner) skipValue() error {
	if err := s.skipWS(); err != nil {
		return err
	}
	if s.pos >= len(s.b) {
		return io.ErrUnexpectedEOF
	}
	switch c := s.b[s.pos]; {
	case c == '"':
		_, _, err := s.string()
		return err
	case c == '{':
		s.pos++
		if err := s.skipWS(); err != nil {
			return err
		}
		if s.peek() == '}' {
			s.pos++
			return nil
		}
		for {
			if _, _, err := s.string(); err != nil {
				return err
			}
			if err := s.skipWS(); err != nil {
				return err
			}
			if err := s.expect(':'); err != nil {
				return err
			}
			if err := s.skipValue(); err != nil {
				return err
			}
			if err := s.skipWS(); err != nil {
				return err
			}
			switch s.peek() {
			case ',':
				s.pos++
				if err := s.skipWS(); err != nil {
					return err
				}
			case '}':
				s.pos++
				return nil
			default:
				return fmt.Errorf("relay: malformed JSON object at offset %d", s.pos)
			}
		}
	case c == '[':
		s.pos++
		if err := s.skipWS(); err != nil {
			return err
		}
		if s.peek() == ']' {
			s.pos++
			return nil
		}
		for {
			if err := s.skipValue(); err != nil {
				return err
			}
			if err := s.skipWS(); err != nil {
				return err
			}
			switch s.peek() {
			case ',':
				s.pos++
			case ']':
				s.pos++
				return nil
			default:
				return fmt.Errorf("relay: malformed JSON array at offset %d", s.pos)
			}
		}
	case c == 't':
		return s.literal("true")
	case c == 'f':
		return s.literal("false")
	case c == 'n':
		return s.literal("null")
	default:
		// Number: consume [-0-9.eE+] run.
		start := s.pos
		for s.pos < len(s.b) {
			c := s.b[s.pos]
			if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
				s.pos++
				continue
			}
			break
		}
		if s.pos == start {
			return fmt.Errorf("relay: unexpected byte %q at offset %d", c, s.pos)
		}
		return nil
	}
}

func (s *scanner) literal(lit string) error {
	if s.pos+len(lit) > len(s.b) || string(s.b[s.pos:s.pos+len(lit)]) != lit {
		return fmt.Errorf("relay: bad literal at offset %d", s.pos)
	}
	s.pos += len(lit)
	return nil
}

// Cacheable reports whether a canonical request could be served
// from the gateway's exact/semantic cache: no tools, no tool
// choice, no tool-role messages, no tool calls. It mirrors the
// cacheability rule in internal/degrade (cacheable) so the relay
// decision and the cache agree on what "in play" means — a request
// the cache could never serve must not be forced through the
// canonical form on the cache's account.
func Cacheable(req *wire.ChatCompletionRequest) bool {
	if req == nil {
		return false
	}
	if len(req.Tools) > 0 || req.ToolChoice != nil {
		return false
	}
	for _, m := range req.Messages {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			return false
		}
	}
	return true
}

// MessagesURL joins a provider base URL with the Anthropic messages
// path, using the same /v1 rule as the provider client: a base
// already ending in "/v1" is used as-is, otherwise "/v1" is appended.
func MessagesURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

// ---------------------------------------------------------------------------
// Translate path: Anthropic Messages -> canonical OpenAI shape.
// ---------------------------------------------------------------------------

// anthropicRequestJSON is the decode shape for a native /v1/messages
// request: the wire type, nothing else.
//
// NOTE — do NOT add System/Thinking/Routing RawMessage fields here.
// wire.AnthropicRequest implements UnmarshalJSON, so encoding/json
// hands the whole body to it and never populates sibling fields on
// this struct: an earlier version of this struct carried System,
// Thinking and Routing as json.RawMessage and all three silently
// stayed empty (system prompts were dropped, thinking was never
// rejected, routing contracts were lost). The data lives on the
// embedded wire value instead — System/SystemBlocks and Thinking
// come from in.AnthropicRequest — and Routing is decoded separately
// from the raw body in ParseAnthropicDetailed (it is not a wire
// field at all, so the wire decode would drop it silently).
type anthropicRequestJSON struct {
	wire.AnthropicRequest
}

// ParseAnthropic converts a native Anthropic /v1/messages request
// body into the gateway's canonical OpenAI-shaped request, for the
// translate path. It returns an error for anything the canonical
// form cannot carry faithfully — thinking blocks, non-text system
// blocks, unknown content-block types — instead of producing a
// lossy translation. cache_control blocks are also unrepresentable
// in the canonical form: they are dropped here (the verbatim path
// is the one that preserves them), and the drop is recorded in the
// returned request's dropped-cache-control count via the error-free
// path — see ParseResult.
func ParseAnthropic(raw []byte) (*wire.ChatCompletionRequest, error) {
	res, err := ParseAnthropicDetailed(raw)
	if err != nil {
		return nil, err
	}
	return res.Request, nil
}

// ParseResult is ParseAnthropicDetailed's result: the canonical
// request plus the count of cache_control blocks the translation
// dropped (0 on the verbatim path — there is no translation there).
// Routing is the request's per-request routing contract (CHECKLIST
// 211), decoded strictly from the same canonical "routing" field
// chat completions uses — nil when the request carried none. The
// server copies it onto the canonical request, so the translate
// path honours it exactly as chat does; the verbatim path never
// sees it (a non-empty contract forces translation via
// Features.RoutingPresent).
type ParseResult struct {
	Request             *wire.ChatCompletionRequest
	DroppedCacheControl int
	Routing             *provider.RequestRouting
}

// ParseAnthropicDetailed is ParseAnthropic with the loss accounting:
// how many cache_control blocks the canonical form could not carry.
// Callers on the translate path must surface the count (the server
// logs it and reports it in the X-Codvian-Relay reason); hiding it
// would be a silent loss.
func ParseAnthropicDetailed(raw []byte) (*ParseResult, error) {
	// Normalise first: Anthropic accepts a message's content as a
	// plain string as well as a block array, but the wire type only
	// carries the array form. A string is wrapped into a single
	// text block — the same content, no loss — before decoding.
	raw, err := normaliseAnthropicContent(raw)
	if err != nil {
		return nil, fmt.Errorf("relay: invalid anthropic request: %w", err)
	}
	var in anthropicRequestJSON
	// No DisallowUnknownFields: a native request may carry fields the
	// canonical form ignores (metadata, service_tier, ...). Unknown
	// fields are dropped by the translation — which is why the
	// verbatim path exists — but they must not fail the parse.
	// The routing contract is the exception (CHECKLIST 211): it is
	// decoded strictly below, exactly like chat completions, so a
	// misspelled control fails closed with a 400 naming the field
	// instead of being silently dropped.
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("relay: invalid anthropic request: %w", err)
	}
	// Strict routing decode (CHECKLIST 211): the contract's shape
	// is the canonical one — order / only / exclude / sort /
	// max_price / data_collection / group — and an unknown key
	// inside "routing" is a 400 naming the key, never silently
	// ignored. The error string mirrors
	// provider.ChatRequest.UnmarshalJSON's exactly (including its
	// known-fields list), so a client gets the identical message
	// on /v1/messages and on /v1/chat/completions for the same
	// mistake. A malformed contract fails the parse even for a
	// request that would otherwise relay verbatim: fail-closed,
	// before any provider is dialled or any byte forwarded.
	var routing *provider.RequestRouting
	// Routing is decoded from the raw body, not from the embedded
	// wire value: it is not a wire field, so the wire decode drops
	// it — a separate strict decode is the only place it survives.
	var rawRouting struct {
		Routing json.RawMessage `json:"routing"`
	}
	if err := json.Unmarshal(raw, &rawRouting); err != nil {
		return nil, fmt.Errorf("relay: invalid anthropic request: %w", err)
	}
	if len(rawRouting.Routing) > 0 && string(rawRouting.Routing) != "null" {
		sdec := json.NewDecoder(bytes.NewReader(rawRouting.Routing))
		sdec.DisallowUnknownFields()
		var strict provider.RequestRouting
		if err := sdec.Decode(&strict); err != nil {
			return nil, fmt.Errorf("provider: invalid routing: %s (known fields: order, only, exclude, sort, max_price, data_collection)", routingDecodeDetail(err))
		}
		routing = &strict
	}
	// Thinking comes from the embedded wire value (see the struct
	// note above): a non-nil pointer means the client sent a
	// thinking block, which has no canonical equivalent. An
	// explicit null leaves the pointer nil, same as absent.
	if in.AnthropicRequest.Thinking != nil {
		return nil, &wire.UnrepresentableError{Reason: "thinking block has no canonical equivalent; use the verbatim relay (single Anthropic target, no guardrails/cache/failover) to preserve it"}
	}
	if strings.TrimSpace(in.Model) == "" {
		return nil, &wire.UnrepresentableError{Reason: "model is empty"}
	}
	if len(in.Messages) == 0 {
		return nil, &wire.UnrepresentableError{Reason: "messages is empty"}
	}
	out := &wire.ChatCompletionRequest{
		Model:     in.Model,
		Stream:    in.Stream,
		MaxTokens: in.MaxTokens,
		Stop:      append([]string(nil), in.StopSequences...),
	}
	if in.Temperature != nil {
		out.Temperature = in.Temperature
	}
	if in.TopP != nil {
		out.TopP = in.TopP
	}
	dropped := countCacheControl(raw)
	// System comes from the embedded wire value (see the struct
	// note above): the wire UnmarshalJSON already normalised it —
	// string form in System, block form in SystemBlocks with the
	// joined text in System. A non-text system block has no
	// canonical equivalent and fails closed, exactly as before.
	if blocks := in.AnthropicRequest.SystemBlocks; len(blocks) > 0 {
		var texts []string
		for _, b := range blocks {
			if b.Type != "text" {
				return nil, &wire.UnrepresentableError{Reason: "system block type " + b.Type + " has no canonical equivalent"}
			}
			texts = append(texts, b.Text)
		}
		if joined := strings.Join(texts, "\n\n"); joined != "" {
			out.Messages = append(out.Messages, wire.ChatMessage{Role: "system", Content: mustJSON(joined)})
		}
	} else if in.AnthropicRequest.System != "" {
		out.Messages = append(out.Messages, wire.ChatMessage{Role: "system", Content: mustJSON(in.AnthropicRequest.System)})
	}
	// Tools.
	for i, t := range in.Tools {
		if strings.TrimSpace(t.Name) == "" {
			return nil, &wire.UnrepresentableError{Reason: fmt.Sprintf("tools[%d].name is empty", i)}
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, wire.ToolDefinition{
			Type: "function",
			Function: struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			}{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}
	if in.ToolChoice != nil {
		switch in.ToolChoice.Type {
		case "auto":
			out.ToolChoice = "auto"
		case "any":
			out.ToolChoice = "required"
		case "tool":
			if in.ToolChoice.Name == "" {
				return nil, &wire.UnrepresentableError{Reason: "tool_choice of type \"tool\" needs a name"}
			}
			out.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": in.ToolChoice.Name}}
		default:
			return nil, &wire.UnrepresentableError{Reason: "unknown tool_choice type " + in.ToolChoice.Type}
		}
	}
	// Messages. (dropped is the whole-body cache_control count taken
	// up front: the wire decode drops the markers before per-message
	// conversion runs, so per-message counting would double-count.)
	for i, m := range in.Messages {
		_, msg, _, err := convertAnthropicMessage(m, i)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, msg)
	}
	if len(out.Messages) == 0 {
		return nil, &wire.UnrepresentableError{Reason: "no representable messages"}
	}
	return &ParseResult{Request: out, DroppedCacheControl: dropped, Routing: routing}, nil
}

// routingDecodeDetail turns a strict routing-decode error into the
// one detail worth reporting: the unknown field name when that is
// what failed, otherwise the decoder's own message. (Mirrors
// provider.routingDecodeDetail, which is unexported — the message
// must stay identical to chat's, so the mirror is deliberate.)
func routingDecodeDetail(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, `unknown field "`); i >= 0 {
		rest := msg[i+len(`unknown field "`):]
		if j := strings.Index(rest, `"`); j >= 0 {
			return `unknown field "` + rest[:j] + `"`
		}
	}
	return msg
}

// anthropicBlockJSON is the decode shape for one Anthropic content
// block, carrying the cache_control marker the wire type drops.
type anthropicBlockJSON struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	Source       json.RawMessage `json:"source,omitempty"`
}

// convertAnthropicMessage converts one native message. The third
// return is always 0: cache_control markers are counted once for the
// whole body in ParseAnthropicDetailed (the wire decode drops them
// before this runs), so per-message counting would double-count.
func convertAnthropicMessage(m wire.AnthropicMessage, index int) (string, wire.ChatMessage, int, error) {
	dropped := 0
	switch m.Role {
	case "user", "assistant":
	default:
		return "", wire.ChatMessage{}, 0, &wire.UnrepresentableError{Reason: fmt.Sprintf("messages[%d].role %q is not supported", index, m.Role)}
	}
	// Re-decode the blocks from the wire form via JSON.
	rawBlocks, err := json.Marshal(m.Content)
	if err != nil {
		return "", wire.ChatMessage{}, 0, err
	}
	var blocks []anthropicBlockJSON
	if err := json.Unmarshal(rawBlocks, &blocks); err != nil {
		return "", wire.ChatMessage{}, 0, err
	}
	// cache_control never survives wire.AnthropicBlock, so it is not
	// visible here at all — the count is taken from the raw body in
	// ParseAnthropicDetailed instead.
	out := wire.ChatMessage{Role: m.Role}
	var parts []wire.ContentPart
	var toolCalls []wire.ToolCall
	var toolResults []wire.ContentPart
	for i, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, wire.ContentPart{Type: "text", Text: b.Text})
			}
		case "image":
			var src struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type,omitempty"`
				Data      string `json:"data,omitempty"`
				URL       string `json:"url,omitempty"`
			}
			if err := json.Unmarshal(b.Source, &src); err != nil {
				return "", wire.ChatMessage{}, 0, &wire.UnrepresentableError{Reason: fmt.Sprintf("messages[%d].content[%d] image source is malformed", index, i)}
			}
			switch src.Type {
			case "base64":
				parts = append(parts, wire.ContentPart{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: "data:" + src.MediaType + ";base64," + src.Data}})
			case "url":
				parts = append(parts, wire.ContentPart{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: src.URL}})
			default:
				return "", wire.ChatMessage{}, 0, &wire.UnrepresentableError{Reason: fmt.Sprintf("messages[%d].content[%d] image source type %q is not supported", index, i, src.Type)}
			}
		case "tool_use":
			if m.Role != "assistant" {
				return "", wire.ChatMessage{}, 0, &wire.UnrepresentableError{Reason: fmt.Sprintf("messages[%d].content[%d] is a tool_use block in a %s message", index, i, m.Role)}
			}
			args := strings.TrimSpace(string(b.Input))
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return "", wire.ChatMessage{}, 0, &wire.UnrepresentableError{Reason: fmt.Sprintf("messages[%d].content[%d] tool_use input is not valid JSON", index, i)}
			}
			toolCalls = append(toolCalls, wire.ToolCall{ID: b.ID, Type: "function", Function: wire.ToolCallFunction{Name: b.Name, Arguments: args}})
		case "tool_result":
			if m.Role != "user" {
				return "", wire.ChatMessage{}, 0, &wire.UnrepresentableError{Reason: fmt.Sprintf("messages[%d].content[%d] is a tool_result block in a %s message", index, i, m.Role)}
			}
			if len(toolResults) > 0 || len(parts) > 0 || len(toolCalls) > 0 {
				return "", wire.ChatMessage{}, 0, &wire.UnrepresentableError{Reason: fmt.Sprintf("messages[%d] mixes tool_result with other blocks; one tool result per message on the translate path", index)}
			}
			text := blockText(b.Content)
			out.Role = "tool"
			out.ToolCallID = b.ToolUseID
			if b.IsError {
				text = "error: " + text
			}
			out.Content = mustJSON(text)
			return out.Role, out, dropped, nil
		case "thinking", "redacted_thinking":
			return "", wire.ChatMessage{}, 0, &wire.UnrepresentableError{Reason: fmt.Sprintf("messages[%d].content[%d] is a thinking block with no canonical equivalent", index, i)}
		default:
			return "", wire.ChatMessage{}, 0, &wire.UnrepresentableError{Reason: fmt.Sprintf("messages[%d].content[%d].type %q has no canonical equivalent", index, i, b.Type)}
		}
	}
	if len(parts) == 1 && parts[0].Type == "text" && len(toolCalls) == 0 {
		out.Content = mustJSON(parts[0].Text)
	} else if len(parts) > 0 {
		out.Content = mustJSONRaw(parts)
	}
	if len(toolCalls) > 0 {
		out.ToolCalls = toolCalls
	}
	return out.Role, out, dropped, nil
}

// blockText extracts plain text from a tool_result content value
// (string, or an array of text blocks).
func blockText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlockJSON
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return trimmed
}

// normaliseAnthropicContent wraps a message's string-form content
// into a single text block, so the wire decode (array form only)
// accepts what the Anthropic API accepts. Anything else passes
// through unchanged; a body that is not a JSON object is an error.
func normaliseAnthropicContent(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	msgs, ok := doc["messages"].([]any)
	if !ok {
		return raw, nil
	}
	changed := false
	for i, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := mm["content"].(string); ok {
			mm["content"] = []any{map[string]any{"type": "text", "text": s}}
			msgs[i] = mm
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(doc)
}

// countCacheControl counts "cache_control" keys in the raw body. It is
// a plain substring count over JSON string keys, used only to report
// how many markers the translate path dropped — never to decide
// anything, so an over-count on a pathological body is a reporting
// error, not a routing error.
func countCacheControl(raw []byte) int {
	return bytes.Count(raw, []byte(`"cache_control"`))
}

func mustJSON(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}

func mustJSONRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

// ---------------------------------------------------------------------------
// Translate path back: canonical response -> Anthropic Messages.
// ---------------------------------------------------------------------------

// MapFinishReasonToAnthropic maps an OpenAI finish_reason onto an
// Anthropic stop_reason. Unknown reasons end the turn normally.
func MapFinishReasonToAnthropic(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "stop", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// ToAnthropicResponse renders a canonical engine response as a
// native Anthropic /v1/messages JSON body. The id is minted here
// (msg_ + 24 hex chars): the engine response carries no message id,
// and inventing a stable-looking one from the content would be
// worse than a fresh random id.
func ToAnthropicResponse(resp *provider.ChatResponse) ([]byte, error) {
	if resp == nil {
		return nil, fmt.Errorf("relay: nil response")
	}
	id, err := randomID("msg_")
	if err != nil {
		return nil, err
	}
	type usage struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
	}
	type block struct {
		Type  string          `json:"type"`
		Text  string          `json:"text,omitempty"`
		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	}
	content := []block{}
	if resp.Content != "" {
		content = append(content, block{Type: "text", Text: resp.Content})
	}
	for _, tc := range resp.ToolCalls {
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return nil, fmt.Errorf("relay: tool call %q arguments are not valid JSON", tc.Function.Name)
		}
		content = append(content, block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(args)})
	}
	model := resp.Model
	body := struct {
		ID           string  `json:"id"`
		Type         string  `json:"type"`
		Role         string  `json:"role"`
		Content      []block `json:"content"`
		Model        string  `json:"model"`
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence,omitempty"`
		Usage        usage   `json:"usage"`
	}{
		ID: id, Type: "message", Role: "assistant", Content: content, Model: model,
		StopReason: MapFinishReasonToAnthropic(resp.FinishReason),
		Usage:      usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens, CacheReadInputTokens: resp.Usage.CachedTokens},
	}
	return json.Marshal(body)
}

func randomID(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// ---------------------------------------------------------------------------
// Anthropic SSE envelope (translate-path streaming).
//
// The verbatim path never touches the envelope: upstream SSE bytes
// are copied as-is. This emitter is only for the translate path,
// where the gateway itself is the SSE author: it emits the standard
// Anthropic event sequence (message_start, content_block_start,
// content_block_delta, content_block_stop, message_delta, message_stop)
// from the engine's text/tool-call chunks. message_start carries no
// usage block: input tokens are only known once the upstream answer
// finishes, and emitting 0 would be an invented number — the true
// usage arrives in message_delta at the end.
// ---------------------------------------------------------------------------

// AnthropicStreamEmitter writes Anthropic SSE events to w. The
// caller owns flushing (http.Flusher) between events.
type AnthropicStreamEmitter struct {
	w         io.Writer
	messageID string
	model     string
	blockOpen bool
	blockIdx  int
	finished  bool
}

// NewAnthropicStreamEmitter creates an emitter. messageID/model are
// the values message_start reports; an empty messageID mints one.
func NewAnthropicStreamEmitter(w io.Writer, messageID, model string) (*AnthropicStreamEmitter, error) {
	if messageID == "" {
		id, err := randomID("msg_")
		if err != nil {
			return nil, err
		}
		messageID = id
	}
	return &AnthropicStreamEmitter{w: w, messageID: messageID, model: model}, nil
}

// Start emits message_start + the first text content_block_start.
func (e *AnthropicStreamEmitter) Start() error {
	if err := e.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": e.messageID, "type": "message", "role": "assistant",
			"content": []any{}, "model": e.model,
			"stop_reason": nil, "stop_sequence": nil,
		},
	}); err != nil {
		return err
	}
	return e.openTextBlock()
}

func (e *AnthropicStreamEmitter) openTextBlock() error {
	if err := e.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": e.blockIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	}); err != nil {
		return err
	}
	e.blockOpen = true
	return nil
}

// TextDelta emits one text content_block_delta. A no-op when text is empty.
func (e *AnthropicStreamEmitter) TextDelta(text string) error {
	if text == "" || e.finished {
		return nil
	}
	if !e.blockOpen {
		if err := e.openTextBlock(); err != nil {
			return err
		}
	}
	return e.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": e.blockIdx,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

// ToolUseBlock emits a complete tool_use content block (index
// blockIdx+1 onward) as start + a single input_json_delta + stop.
// The translate path only learns tool calls once the engine's stream
// finishes, so the block is emitted whole at the end — never split
// across partial JSON.
func (e *AnthropicStreamEmitter) ToolUseBlock(id, name, inputJSON string) error {
	if e.finished {
		return nil
	}
	if e.blockOpen {
		if err := e.closeBlock(); err != nil {
			return err
		}
	}
	e.blockIdx++
	if err := e.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": e.blockIdx,
		"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
	}); err != nil {
		return err
	}
	if err := e.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": e.blockIdx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": inputJSON},
	}); err != nil {
		return err
	}
	return e.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.blockIdx})
}

func (e *AnthropicStreamEmitter) closeBlock() error {
	if !e.blockOpen {
		return nil
	}
	e.blockOpen = false
	return e.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.blockIdx})
}

// Finish emits content_block_stop (if open), message_delta with the
// true stop reason + usage, and message_stop.
func (e *AnthropicStreamEmitter) Finish(stopReason string, usage provider.Usage) error {
	if e.finished {
		return nil
	}
	e.finished = true
	if err := e.closeBlock(); err != nil {
		return err
	}
	delta := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": usage.PromptTokens, "output_tokens": usage.CompletionTokens},
	}
	if usage.CachedTokens > 0 {
		delta["usage"].(map[string]any)["cache_read_input_tokens"] = usage.CachedTokens
	}
	if err := e.emit("message_delta", delta); err != nil {
		return err
	}
	return e.emit("message_stop", map[string]any{"type": "message_stop"})
}

func (e *AnthropicStreamEmitter) emit(event string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
