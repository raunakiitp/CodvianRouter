package wire

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Streaming: Anthropic SSE -> OpenAI chat-completion chunks.
// ---------------------------------------------------------------------------

// ChunkToolCall is one tool-call delta inside a streaming chunk.
type ChunkToolCall struct {
	Index int    `json:"index"`
	ID    string `json:"id,omitempty"`
	Type  string `json:"type,omitempty"`
	// Function carries the name (first delta for the block) and/or an
	// arguments fragment (input_json_delta partial_json).
	Function ChunkFunction `json:"function,omitempty"`
}

// ChunkFunction is the function payload of a ChunkToolCall.
type ChunkFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ChunkDelta is the delta of one streaming choice.
type ChunkDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []ChunkToolCall `json:"tool_calls,omitempty"`
}

// ChunkChoice is one choice of a streaming chunk.
type ChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// ChatChunk is one OpenAI chat.completion.chunk.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// StreamResult is the reassembled result of a translated stream: the
// full text, the completed tool calls, the finish reason, and usage.
type StreamResult struct {
	ID           string
	Model        string
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	name string
	data string
}

// readSSE parses SSE frames from r, calling fn for each complete event
// (blank line ends an event). Comment lines (":...") are ignored, as
// are lines that are neither "event:" nor "data:". Multiple "data:"
// lines in one event are joined with "\n" per the SSE spec.
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	scanner := bufio.NewScanner(r)
	// Deltas can be large; allow up to 1 MiB per SSE line, matching
	// the OpenAI client's streaming scanner.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var ev sseEvent
	flush := func() error {
		if ev.name == "" && ev.data == "" {
			return nil
		}
		e := ev
		ev = sseEvent{}
		return fn(e)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE comment / keep-alive
		}
		if name, ok := strings.CutPrefix(line, "event:"); ok {
			ev.name = strings.TrimSpace(name)
			continue
		}
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimPrefix(data, " ")
			if ev.data != "" {
				ev.data += "\n"
			}
			ev.data += data
			continue
		}
		// Unknown line: ignored — Anthropic sends nothing else, and a
		// stray line must not kill a stream.
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("wire: error reading SSE stream: %w", err)
	}
	return flush()
}

// anthropicStreamEvent is the JSON envelope of one Anthropic SSE data
// payload. Only the fields each event type carries are populated.
type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	// message_start.
	Message *struct {
		ID    string         `json:"id"`
		Model string         `json:"model"`
		Usage AnthropicUsage `json:"usage"`
	} `json:"message,omitempty"`
	// content_block_start.
	ContentBlock *AnthropicBlock `json:"content_block,omitempty"`
	// content_block_delta.
	Delta *struct {
		Type        string `json:"type"` // "text_delta" | "input_json_delta" | "thinking_delta" | "signature_delta"
		Text        string `json:"text,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
		StopReason  string `json:"stop_reason,omitempty"`
	} `json:"delta,omitempty"`
	// message_delta.
	Usage AnthropicUsage `json:"usage,omitempty"`
	// error events.
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// streamBlock tracks one open content block while streaming.
type streamBlock struct {
	kind     string // "text" | "tool_use" | "ignored"
	toolID   string
	toolName string
	jsonBuf  strings.Builder
}

// TranslateStream reads an Anthropic SSE stream (the body of a
// stream:true /v1/messages response) and emits OpenAI
// chat.completion.chunk values via emit, in order. It returns the
// reassembled result once message_stop arrives.
//
//   - ping events are ignored; thinking_delta / signature_delta are
//     dropped (internal reasoning, no OpenAI equivalent).
//   - text_delta becomes a content delta; input_json_delta fragments
//     become tool-call argument deltas, validated as JSON when the
//     block closes (fragments are not valid JSON on their own).
//   - message_delta supplies the finish reason and output tokens; the
//     final emitted chunk carries finish_reason and usage.
//   - an error event returns the mapped *AnthropicError and never a
//     success result; a stream that ends without message_stop returns
//     an error (truncated), never a partial success.
func TranslateStream(r io.Reader, emit func(ChatChunk) error) (StreamResult, error) {
	if emit == nil {
		emit = func(ChatChunk) error { return nil }
	}
	var res StreamResult
	created := time.Now().Unix()
	blocks := make(map[int]*streamBlock)
	var (
		content    strings.Builder
		sawStart   bool
		sawDelta   bool
		sawStop    bool
		stopReason string
	)
	emitChunk := func(delta ChunkDelta, finishReason *string, usage *Usage) error {
		return emit(ChatChunk{
			ID:      res.ID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   res.Model,
			Choices: []ChunkChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
			Usage:   usage,
		})
	}

	err := readSSE(r, func(ev sseEvent) error {
		if ev.name == "ping" {
			return nil
		}
		data := strings.TrimSpace(ev.data)
		if data == "" {
			return nil
		}
		var parsed anthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			return fmt.Errorf("wire: malformed SSE data: %w", err)
		}
		// The event name normally comes from the "event:" line; fall
		// back to the JSON "type" field when it does not.
		evType := ev.name
		if evType == "" {
			evType = parsed.Type
		}
		switch evType {
		case "message_start":
			sawStart = true
			if parsed.Message != nil {
				res.ID = parsed.Message.ID
				res.Model = parsed.Message.Model
				res.Usage.PromptTokens = parsed.Message.Usage.InputTokens
			}
			// OpenAI streams open with a role-only chunk.
			return emitChunk(ChunkDelta{Role: "assistant"}, nil, nil)
		case "content_block_start":
			if parsed.ContentBlock == nil {
				return fmt.Errorf("wire: content_block_start without content_block")
			}
			kind := parsed.ContentBlock.Type
			blk := &streamBlock{kind: kind}
			switch kind {
			case "text":
				// Nothing to emit yet; deltas carry the text.
			case "tool_use":
				blk.toolID = parsed.ContentBlock.ID
				blk.toolName = parsed.ContentBlock.Name
				if strings.TrimSpace(blk.toolName) == "" {
					return unrep("streamed tool_use block has no name")
				}
				// First delta for the call carries id, type, name.
				if err := emitChunk(ChunkDelta{ToolCalls: []ChunkToolCall{{
					Index: parsed.Index,
					ID:    blk.toolID,
					Type:  "function",
					Function: ChunkFunction{
						Name: blk.toolName,
					},
				}}}, nil, nil); err != nil {
					return err
				}
			case "thinking", "redacted_thinking":
				blk.kind = "ignored"
			default:
				return unrep("streamed content block type %q has no OpenAI equivalent", kind)
			}
			blocks[parsed.Index] = blk
			return nil
		case "content_block_delta":
			blk, ok := blocks[parsed.Index]
			if !ok {
				return fmt.Errorf("wire: content_block_delta for unknown block index %d", parsed.Index)
			}
			if blk.kind == "ignored" || parsed.Delta == nil {
				return nil
			}
			switch parsed.Delta.Type {
			case "text_delta":
				if blk.kind != "text" {
					return fmt.Errorf("wire: text_delta for non-text block index %d", parsed.Index)
				}
				if parsed.Delta.Text == "" {
					return nil
				}
				content.WriteString(parsed.Delta.Text)
				return emitChunk(ChunkDelta{Content: parsed.Delta.Text}, nil, nil)
			case "input_json_delta":
				if blk.kind != "tool_use" {
					return fmt.Errorf("wire: input_json_delta for non-tool_use block index %d", parsed.Index)
				}
				if parsed.Delta.PartialJSON == "" {
					return nil
				}
				blk.jsonBuf.WriteString(parsed.Delta.PartialJSON)
				return emitChunk(ChunkDelta{ToolCalls: []ChunkToolCall{{
					Index: parsed.Index,
					Function: ChunkFunction{
						Arguments: parsed.Delta.PartialJSON,
					},
				}}}, nil, nil)
			case "thinking_delta", "signature_delta":
				return nil // internal reasoning, dropped
			default:
				return fmt.Errorf("wire: unknown delta type %q", parsed.Delta.Type)
			}
		case "content_block_stop":
			blk, ok := blocks[parsed.Index]
			if !ok {
				return fmt.Errorf("wire: content_block_stop for unknown block index %d", parsed.Index)
			}
			delete(blocks, parsed.Index)
			if blk.kind == "tool_use" {
				args := strings.TrimSpace(blk.jsonBuf.String())
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					return fmt.Errorf("wire: streamed tool_use arguments for %q are not valid JSON", blk.toolName)
				}
				res.ToolCalls = append(res.ToolCalls, ToolCall{
					ID:   blk.toolID,
					Type: "function",
					Function: ToolCallFunction{
						Name:      blk.toolName,
						Arguments: args,
					},
				})
			}
			return nil
		case "message_delta":
			sawDelta = true
			if parsed.Delta != nil && parsed.Delta.StopReason != "" {
				stopReason = parsed.Delta.StopReason
			}
			if parsed.Usage.OutputTokens != 0 {
				res.Usage.CompletionTokens = parsed.Usage.OutputTokens
			}
			return nil
		case "message_stop":
			sawStop = true
			return nil
		case "error":
			// Never fake success: surface the mapped error.
			raw, _ := json.Marshal(parsed)
			return MapStreamError(raw)
		default:
			// Unknown event type: ignore — a future Anthropic event
			// must not break streams that carry usable content.
			return nil
		}
	})
	if err != nil {
		return StreamResult{}, err
	}
	if !sawStart {
		return StreamResult{}, errors.New("wire: SSE stream ended without message_start")
	}
	if !sawStop {
		return StreamResult{}, errors.New("wire: SSE stream ended without message_stop (truncated)")
	}
	if !sawDelta {
		// Anthropic always sends message_delta before message_stop;
		// its absence means the stream was cut short.
		return StreamResult{}, errors.New("wire: SSE stream ended without message_delta (truncated)")
	}
	res.Content = content.String()
	res.FinishReason = MapFinishReason(stopReason)
	res.Usage.TotalTokens = res.Usage.PromptTokens + res.Usage.CompletionTokens
	fr := res.FinishReason
	usage := res.Usage
	if err := emitChunk(ChunkDelta{}, &fr, &usage); err != nil {
		return StreamResult{}, err
	}
	return res, nil
}
