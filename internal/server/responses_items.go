// M8.4 — Responses sub-actions: GET /v1/responses/{id}/input_items.
//
// The OpenAI Responses API has exactly two sub-actions on one stored
// response (verified against the OpenAI API reference, 2026-11-19):
//   - GET  /v1/responses/{response_id}/input_items  (list input items)
//   - POST /v1/responses/{response_id}/cancel       (cancel in-progress)
//
// There is NO output-items sub-action: a response's output array is
// already in GET /v1/responses/{id}. POST /v1/responses/input_tokens
// is a sibling token-count endpoint, not a per-response sub-action,
// and belongs to the token-count slice (parity tracker Area 14 §14a), not this
// one.
//
// Only input_items is built here, because it is the only sub-action
// the existing store already supports: the stored transcript
// (Record.TranscriptJSON) IS the input, minus the assistant turn this
// response produced. Cancel is MISSING by design (see the note at the
// bottom of this file): there is no in-flight cancellation handle in
// the store — no cancel func, no in-flight registry, no "cancelled"
// status — so marking a record cancelled while its stream still runs
// would lie, and the terminal store would overwrite it anyway.
// Building it would need new stored state, which this slice must not
// invent.
//
// Input-items semantics, stated honestly:
//   - The items come from the persisted transcript, never re-fetched
//     from upstream (the gateway holds no upstream Responses state).
//   - The transcript's LAST message is this response's own assistant
//     turn (storeResponsesRecord always appends exactly one); it is
//     output, not input, so it is dropped. Earlier assistant turns
//     (from a continuation base) WERE this response's input and are
//     kept. The per-turn `instructions` system message is NOT in the
//     transcript (it applies to one response only) and so is NOT
//     listed here — a documented gap, not a silent omission.
//   - Item ids are gateway-synthesised positional ids ("item_0",
//     "item_1", …): the transcript stores no upstream item ids, so
//     there is nothing real to reuse. They are stable per
//     (response, position) and serve as the `after` cursor.
//   - A stream still in progress has no transcript yet (the
//     placeholder record stores none; the terminal store writes it
//     when the stream completes), so input_items answers 409 with a
//     retryable message instead of an empty list that would pretend
//     the response had no input.
//   - Pagination mirrors GET /v1/responses: limit default 20,
//     clamped to 100; `after` is an item id, unknown cursors are a
//     400; order defaults to asc (chronological input order) and
//     also accepts desc. A bad limit / order / cursor is a 400.
//   - An unknown response id is an honest 404, like retrieve and
//     delete. A stored transcript that cannot decode is a 500
//     (server-side corruption, never the client's fault).
package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/responses"
)

// responseInputItem is one item of GET /v1/responses/{id}/input_items
// (the OpenAI input-item shape, compatible subset). Status is always
// "completed": a stored transcript message is complete history by
// definition. Content parts carry the input_text / output_text /
// input_image types; function_call / function_call_output items carry
// no content array.
type responseInputItem struct {
	Type      string                     `json:"type"`
	ID        string                     `json:"id"`
	Status    string                     `json:"status"`
	Role      string                     `json:"role,omitempty"`
	Content   []responseInputContentPart `json:"content,omitempty"`
	CallID    string                     `json:"call_id,omitempty"`
	Name      string                     `json:"name,omitempty"`
	Arguments string                     `json:"arguments,omitempty"`
	Output    string                     `json:"output,omitempty"`
}

// responseInputContentPart is one content part of an input message
// item. ImageURL carries the input_image URL.
type responseInputContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// responseInputItemsList is the GET /v1/responses/{id}/input_items
// payload (the OpenAI list shape, same fields as responsesList).
type responseInputItemsList struct {
	Object  string              `json:"object"`
	Data    []responseInputItem `json:"data"`
	HasMore bool                `json:"has_more"`
	FirstID *string             `json:"first_id"`
	LastID  *string             `json:"last_id"`
}

// transcriptToInputItems converts a stored transcript (full history
// including this response's own assistant turn) into input items.
// The last message is dropped when it is the assistant turn this
// response produced (callers pass dropLastAssistant=true for terminal
// records, false when no assistant turn was appended yet). Every
// other message becomes one message item plus, for assistant turns,
// one function_call item per tool call; tool-role messages become
// function_call_output items. Text parts map to input_text
// (non-assistant roles) or output_text (assistant role); image_url
// parts map to input_image; any other stored part type is passed
// through verbatim so data is never silently dropped.
func transcriptToInputItems(transcript []provider.Message, dropLastAssistant bool) []responseInputItem {
	msgs := transcript
	if dropLastAssistant && len(msgs) > 0 && msgs[len(msgs)-1].Role == "assistant" {
		msgs = msgs[:len(msgs)-1]
	}
	items := make([]responseInputItem, 0, len(msgs))
	for i, m := range msgs {
		base := "item_" + strconv.Itoa(i)
		switch m.Role {
		case "tool":
			// A tool result, replayed as a function_call_output.
			items = append(items, responseInputItem{
				Type:   "function_call_output",
				ID:     base,
				Status: "completed",
				CallID: m.ToolCallID,
				Output: m.Content,
			})
		default:
			if parts := messageToInputContent(m); len(parts) > 0 {
				items = append(items, responseInputItem{
					Type:    "message",
					ID:      base,
					Status:  "completed",
					Role:    m.Role,
					Content: parts,
				})
			}
			// Assistant tool calls ride as function_call items
			// after the message item (in call order), mirroring
			// the output array's message-then-calls layout.
			for j, tc := range m.ToolCalls {
				items = append(items, responseInputItem{
					Type:      "function_call",
					ID:        base + "_call_" + strconv.Itoa(j),
					Status:    "completed",
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		}
	}
	return items
}

// messageToInputContent renders one transcript message's content as
// input-item content parts. A message with neither text nor parts
// (an assistant turn carrying only tool calls) yields no parts —
// its calls are emitted as function_call items instead.
func messageToInputContent(m provider.Message) []responseInputContentPart {
	textType := "input_text"
	if m.Role == "assistant" {
		textType = "output_text"
	}
	if len(m.Parts) > 0 {
		parts := make([]responseInputContentPart, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				parts = append(parts, responseInputContentPart{Type: textType, Text: p.Text})
			case "image_url":
				part := responseInputContentPart{Type: "input_image"}
				if p.ImageURL != nil {
					part.ImageURL = p.ImageURL.URL
				}
				parts = append(parts, part)
			default:
				// Unknown stored part type: pass it through
				// verbatim (type + fields) rather than dropping
				// data the client sent. Text stays under "text"
				// when the part recorded one.
				parts = append(parts, responseInputContentPart{Type: p.Type, Text: p.Text})
			}
		}
		return parts
	}
	if strings.TrimSpace(m.Content) == "" {
		return nil
	}
	return []responseInputContentPart{{Type: textType, Text: m.Content}}
}

// handleListResponseInputItems serves GET /v1/responses/{id}/input_items
// (M8.4): the stored input items of one served response, from the
// persisted transcript. Registration (guard.go entry + server.go
// register line) is the coordinator's step — this file owns only the
// handler, so it can be wired exactly like handleGetResponse:
// TierManagement, gateway-key auth, scope "chat" (proposed).
func (s *Server) handleListResponseInputItems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "response id is required")
		return
	}
	rec, ok := s.responses.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "response not found: "+id)
		return
	}
	// A stream still in progress has no transcript yet: the
	// placeholder record stores none, and the terminal store
	// writes it only when the stream completes. 409 (retryable)
	// instead of an empty list that would pretend the response
	// had no input.
	if rec.Status == responses.StatusInProgress || len(rec.TranscriptJSON) == 0 {
		writeError(w, http.StatusConflict, "response is still in progress; input items are stored when it completes")
		return
	}
	var transcript []provider.Message
	if err := json.Unmarshal(rec.TranscriptJSON, &transcript); err != nil {
		// A stored transcript that cannot decode is a server-side
		// corruption, never the client's fault to work around.
		writeServerError(w, http.StatusInternalServerError, "stored response transcript is unreadable")
		return
	}
	// Terminal records always carry their own assistant turn as
	// the last transcript message; it is output, not input.
	items := transcriptToInputItems(transcript, rec.Status != responses.StatusInProgress)

	limit := responsesListDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > responsesListMaxLimit {
			n = responsesListMaxLimit
		}
		limit = n
	}
	order := r.URL.Query().Get("order")
	if order == "" {
		order = "asc"
	}
	if order != "asc" && order != "desc" {
		writeError(w, http.StatusBadRequest, `order must be "asc" or "desc"`)
		return
	}
	if order == "desc" {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	offset := 0
	if after := r.URL.Query().Get("after"); after != "" {
		found := -1
		for i, it := range items {
			if it.ID == after {
				found = i
				break
			}
		}
		if found < 0 {
			writeError(w, http.StatusBadRequest, "unknown cursor: "+after)
			return
		}
		offset = found + 1
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	page := items[offset:end]
	out := responseInputItemsList{Object: "list", Data: page, HasMore: end < len(items)}
	if len(page) > 0 {
		out.Data = page
		out.FirstID = &page[0].ID
		out.LastID = &page[len(page)-1].ID
	} else {
		out.Data = []responseInputItem{}
	}
	// Telemetry headers (M2.1): a stored transcript names no live
	// provider call, so only the model (from the stored record)
	// is reported — no invented cost, tokens or fallback.
	s.writeResponseHeaders(w, r.Context(), ResponseMeta{Model: rec.Model})
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// MISSING by design — POST /v1/responses/{id}/cancel.
//
// NOT built, deliberately. Cancel needs stored state that does not
// exist, and this slice may not invent it:
//   - No in-flight handle: the engine Chat / stream runs on the
//     request context and nothing is stored (no cancel func, no
//     in-flight registry) that could stop it. Marking the record
//     "cancelled" while its stream still delivers would lie, and
//     the terminal store would overwrite the mark with completed
//     or failed anyway.
//   - No "cancelled" status exists in internal/responses (only
//     completed / incomplete / failed / in_progress); a status
//     with no transition owner would be dead state.
//   - Cancel is only meaningful for in_progress responses, and
//     without live in-flight tracking the handler cannot tell a
//     genuinely live stream from a placeholder whose stream
//     already ended but whose terminal store has not landed yet.
// What cancel needs (a future slice, new stored state): an
// in-flight registry mapping response id -> stream cancel func,
// a "cancelled" terminal status the stream drain honours, and a
// rule that a cancelled record's terminal store keeps "cancelled"
// instead of overwriting it. Until then, POST .../cancel must
// stay unregistered rather than answer a fake success.
// ---------------------------------------------------------------------------
