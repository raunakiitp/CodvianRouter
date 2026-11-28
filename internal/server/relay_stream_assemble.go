// Streamed-response assembly for the verbatim relay (A13 Stream B, H2/N1).
//
// The verbatim Anthropic relay copies upstream SSE byte-for-byte to the
// client. Before this file, a streamed response was therefore never stored:
// storeMessagesCache only ran on the non-streaming branches, so the relay
// learned nothing from streamed traffic and a later identical request could
// never be served from the cache (the write tiers were asymmetric — the
// engine path stored completed streams, the relay path did not).
//
// Assembler resolves that asymmetry WITHOUT buffering the response: it sits
// beside the existing usage sniffer on the write path, observes the same
// bytes, and reconstructs the Anthropic message from the SSE event stream
// (message_start -> content_block_start/delta/stop -> message_delta ->
// message_stop). The client still receives every byte as it arrives; the
// assembled message is used only after the stream completes, and only to
// feed the SAME store path the non-streaming branch uses.
//
// Honest limits (deliberate, and the reason the result is a real
// AnthropicResponse rather than a guess):
//   - only the event types this relay can reconstruct are recorded; an
//     unknown event type marks the assembly incomplete, and an incomplete
//     assembly is NEVER stored (no partial answers in the cache);
//   - the assembled usage comes from the provider's own message_start /
//     message_delta events, never estimated;
//   - a truncated stream (no message_stop) is never stored.
package server

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// streamAssembler reconstructs an Anthropic message from SSE bytes.
//
// It is not safe for concurrent use; the relay owns one per stream.
type streamAssembler struct {
	pend   []byte
	blocks map[int]*streamBlock
	order  []int

	id           string
	model        string
	role         string
	stopReason   string
	usage        wire.AnthropicUsage
	seenStart    bool
	seenStop     bool
	incomplete   bool
	sawToolInput bool
	sawThinking  bool
}

// streamBlock is one content block being assembled. Delta kinds are kept
// separate so a tool_use block's partial JSON is not confused with its text.
type streamBlock struct {
	kind         string // "text" | "tool_use" | "thinking" | "redacted_thinking" | other
	text         strings.Builder
	json         strings.Builder
	thinking     strings.Builder
	thinkingSig  string
	redactedData string
	toolID       string
	toolName     string
}

// newStreamAssembler returns an assembler ready to consume SSE bytes.
func newStreamAssembler() *streamAssembler {
	return &streamAssembler{blocks: make(map[int]*streamBlock)}
}

// maxAssemblerLine bounds one pending SSE line, matching the usage
// sniffer's guard: an oversized line cannot be parsed and marks the
// assembly incomplete rather than growing memory without limit.
const maxAssemblerLine = 1 << 20

// Write consumes relayed bytes. It never fails the relay: an unparseable
// or oversized line simply marks the assembly incomplete.
func (a *streamAssembler) Write(p []byte) {
	a.pend = append(a.pend, p...)
	for {
		i := indexByte(a.pend, '\n')
		if i < 0 {
			if len(a.pend) > maxAssemblerLine {
				a.incomplete = true
				a.pend = a.pend[:0]
			}
			return
		}
		line := a.pend[:i]
		a.pend = a.pend[i+1:]
		a.line(strings.TrimSuffix(string(line), "\r"))
	}
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// line handles one SSE line. Only `data:` payloads carry events; the
// `event:` name line is redundant with the payload's own "type" field.
func (a *streamAssembler) line(ln string) {
	if !strings.HasPrefix(ln, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(ln, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Index   *int   `json:"index"`
		Message *struct {
			ID    string              `json:"id"`
			Model string              `json:"model"`
			Role  string              `json:"role"`
			Usage wire.AnthropicUsage `json:"usage"`
		} `json:"message"`
		ContentBlock *struct {
			Type     string `json:"type"`
			ID       string `json:"id"`
			Name     string `json:"name"`
			Thinking string `json:"thinking"`
			Data     string `json:"data"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage *wire.AnthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		a.incomplete = true
		return
	}
	switch ev.Type {
	case "message_start":
		a.seenStart = true
		if ev.Message != nil {
			a.id = ev.Message.ID
			a.model = ev.Message.Model
			a.role = ev.Message.Role
			a.usage = ev.Message.Usage
		}
	case "content_block_start":
		if ev.Index == nil || ev.ContentBlock == nil {
			a.incomplete = true
			return
		}
		b := &streamBlock{kind: ev.ContentBlock.Type, toolID: ev.ContentBlock.ID, toolName: ev.ContentBlock.Name}
		switch ev.ContentBlock.Type {
		case "thinking":
			a.sawThinking = true
			b.thinking.WriteString(ev.ContentBlock.Thinking)
		case "redacted_thinking":
			a.sawThinking = true
			b.redactedData = ev.ContentBlock.Data
		}
		a.blocks[*ev.Index] = b
		a.order = append(a.order, *ev.Index)
	case "content_block_delta":
		if ev.Index == nil || ev.Delta == nil {
			a.incomplete = true
			return
		}
		b := a.blocks[*ev.Index]
		if b == nil {
			a.incomplete = true
			return
		}
		switch ev.Delta.Type {
		case "text_delta":
			b.text.WriteString(ev.Delta.Text)
		case "input_json_delta":
			a.sawToolInput = true
			b.json.WriteString(ev.Delta.PartialJSON)
		case "thinking_delta":
			b.thinking.WriteString(ev.Delta.Thinking)
		case "signature_delta":
			b.thinkingSig = ev.Delta.Signature
		default:
			// An unknown delta type means the reconstruction
			// cannot be faithful.
			a.incomplete = true
		}
	case "content_block_stop":
		// nothing to do: the block is already in a.blocks
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			a.stopReason = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			// Providers report the final output_tokens here; keep
			// the start values for the fields they omit.
			if ev.Usage.OutputTokens > 0 {
				a.usage.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage.InputTokens > 0 {
				a.usage.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				a.usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				a.usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
			}
		}
	case "message_stop":
		a.seenStop = true
	case "ping", "error":
		// ping carries nothing; an error event means the upstream
		// gave up mid-stream.
		if ev.Type == "error" {
			a.incomplete = true
		}
	default:
		// Unknown event type: the stream carries something this
		// assembler cannot represent, so the assembly is not
		// faithful and must not be cached.
		a.incomplete = true
	}
}

// Complete reports whether the stream ended properly AND every event was
// understood. Only a complete assembly may be stored.
func (a *streamAssembler) Complete() bool {
	return a.seenStart && a.seenStop && !a.incomplete
}

// Response builds the Anthropic response this stream carried. It is only
// meaningful when Complete() is true; callers must check first.
func (a *streamAssembler) Response() wire.AnthropicResponse {
	resp := wire.AnthropicResponse{
		ID:    a.id,
		Type:  "message",
		Role:  a.role,
		Model: a.model,
		Usage: a.usage,
	}
	if resp.Role == "" {
		resp.Role = "assistant"
	}
	if a.stopReason != "" {
		sr := a.stopReason
		resp.StopReason = &sr
	}
	for _, idx := range a.order {
		b := a.blocks[idx]
		if b == nil {
			continue
		}
		switch b.kind {
		case "text":
			resp.Content = append(resp.Content, wire.AnthropicBlock{Type: "text", Text: b.text.String()})
		case "tool_use":
			input := json.RawMessage(b.json.String())
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			resp.Content = append(resp.Content, wire.AnthropicBlock{Type: "tool_use", ID: b.toolID, Name: b.toolName, Input: input})
		case "thinking":
			resp.Content = append(resp.Content, wire.AnthropicBlock{Type: "thinking", Thinking: b.thinking.String(), Signature: b.thinkingSig})
		case "redacted_thinking":
			resp.Content = append(resp.Content, wire.AnthropicBlock{Type: "redacted_thinking", Data: b.redactedData})
		}
	}
	return resp
}

// HasToolInput reports whether the stream carried tool-call arguments —
// exposed so the store path can apply the tool-call refusal gate to a
// STREAMED response exactly as it does to a buffered one.
func (a *streamAssembler) HasToolInput() bool { return a.sawToolInput }

// storeAssembledStreamCache stores a COMPLETED streamed relay response under
// the same exact + semantic keys the non-streaming branch uses, so a streamed
// answer is available to a later identical request (A13-H2/N1: the write tiers
// are now symmetric).
//
// It routes through storeMessagesCache by re-encoding the assembled message
// into the same JSON shape the non-streaming path receives, so EVERY store
// gate applies unchanged and cannot drift: per-request cache controls, the
// finish-reason / temperature / tool-call refusal (cache.ShouldStore), the
// 50-turn semantic bypass, model and tenant scoping, and the size cap.
//
// An incomplete assembly (no message_stop, an unknown event type, a
// mid-stream error, an oversized line) is NEVER stored — a partial answer in
// the cache would be a wrong answer, not a fast one.
func (s *Server) storeAssembledStreamCache(ctx context.Context, req provider.ChatRequest, asm *streamAssembler, cacheable bool, status int) {
	if asm == nil || !cacheable {
		return
	}
	if s.exactCache == nil && s.semanticCache == nil {
		return
	}
	if status < 200 || status >= 300 {
		return
	}
	if !asm.Complete() {
		return
	}
	body, err := json.Marshal(asm.Response())
	if err != nil || len(body) == 0 || len(body) > relayMaxStoreBody {
		return
	}
	// storeMessagesCache applies the remaining gates (cache controls,
	// refusal/temperature/tool-call, 50-turn bypass, scoping). A
	// tool-carrying stream is refused there via the converted message's
	// tool_calls, exactly like a buffered tool-carrying response.
	s.storeMessagesCache(ctx, req, body)
}
