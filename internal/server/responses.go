// POST /v1/responses — the OpenAI Responses API surface (CHECKLIST
// 169-170; competitor-sweep P1 gap: the Responses API is now a
// first-class surface in LiteLLM, Portkey and OmniRoute).
//
// Design: the gateway translates a Responses request into the
// canonical chat request and routes it through the SAME engine as
// POST /v1/chat/completions — same fallback chains, budgets,
// guardrails, ledger and route trace — then translates the chat
// response back into a Responses object. The upstream never sees a
// Responses request, so a Responses conversation works on ANY
// provider the engine can route chat to (including native-wire
// Anthropic/Gemini/Cohere/Bedrock targets), not only on providers
// that implement /v1/responses upstream.
//
// State: the gateway mints its own response ids ("resp_…") and
// stores each served response (transcript + served object) in the
// responses store. The store persists to responses.json in the
// data dir on every mutation (CHECKLIST 315–316) and restores
// from it at startup, so a restart no longer loses transcripts:
// a create naming previous_response_id resumes the restored
// conversation instead of answering 404. A create naming
// previous_response_id resumes that conversation: the stored
// transcript is re-sent with the new input appended. A continuation fails over like a chat
// completion (CHECKLIST 237): the original provider is tried
// first (the pin), and a failure replays the same branched input
// against the next target — the transcript is gateway-held, so no
// upstream-held state exists for a replay to double-apply. The
// response tree (parent → children, branching when two responses
// share one parent) is kept durably in the ledger
// (internal/ledger response_tree table) as well as in the store,
// and GET /v1/responses/{id} reports the child ids.
//
// Deliberately NOT cached, coalesced or stale-served (see
// internal/coalescing.Coalescable and internal/degrade.cacheable):
// a Responses create mints a distinct stateful object per call,
// so sharing a flight or serving a cached answer would hand two
// callers the same response id or record a conversation the
// provider never had.
//
// Supported request subset (anything outside it is ignored, like
// the chat endpoint's lenient decode — see docs/responses-api.md
// for the full list): model, input (string, or an array of message
// items / function_call / function_call_output items), instructions,
// previous_response_id, stream, max_output_tokens, temperature,
// top_p, tools (function tools only), tool_choice, metadata,
// routing (the same per-request contract as chat completions).
// The reasoning/text.format/parallel_tool_calls controls are not
// honoured — they are ignored, never faked as applied.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/responses"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// ---------------------------------------------------------------------------
// Wire shapes (OpenAI Responses API, compatible subset).
// ---------------------------------------------------------------------------

// responsesRequest is the decode shape for POST /v1/responses.
// Input is raw: a string, or an array of items (message,
// function_call, function_call_output). Routing is decoded
// strictly in decodeResponsesRequest (unknown keys are a 400
// naming the key, like the chat contract).
type responsesRequest struct {
	Model              string                   `json:"model"`
	Input              json.RawMessage          `json:"input"`
	Instructions       string                   `json:"instructions,omitempty"`
	PreviousResponseID string                   `json:"previous_response_id,omitempty"`
	Stream             bool                     `json:"stream,omitempty"`
	MaxOutputTokens    int                      `json:"max_output_tokens,omitempty"`
	Temperature        float64                  `json:"temperature,omitempty"`
	TopP               float64                  `json:"top_p,omitempty"`
	Tools              []responsesTool          `json:"tools,omitempty"`
	ToolChoice         json.RawMessage          `json:"tool_choice,omitempty"`
	Metadata           map[string]string        `json:"metadata,omitempty"`
	Routing            *provider.RequestRouting `json:"routing,omitempty"`
}

// responsesTool is one function tool on a Responses request. Any
// other tool type is a 400 naming it (the engine only carries
// function tools).
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// responsesInputItem is one item of an array-form input.
type responsesInputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// function_call items:
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// function_call_output items:
	Output string `json:"output"`
}

// responsesInputPart is one content part inside an input item.
type responsesInputPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// responseOutputText is one output_text part of a message item.
type responseOutputText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

// responseOutputItem is one item of a response's output array: a
// message item (the assistant's text turn) or a function_call
// item (one tool call).
type responseOutputItem struct {
	Type      string               `json:"type"`
	ID        string               `json:"id"`
	Status    string               `json:"status,omitempty"`
	Role      string               `json:"role,omitempty"`
	Content   []responseOutputText `json:"content,omitempty"`
	CallID    string               `json:"call_id,omitempty"`
	Name      string               `json:"name,omitempty"`
	Arguments string               `json:"arguments,omitempty"`
}

// responseUsage is the Responses usage block. CachedTokens and
// ReasoningTokens ride the details sub-objects only when the
// upstream reported them (> 0); 0 there means "not reported",
// never "measured zero".
type responseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	InputDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
	OutputDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
}

// responseIncompleteDetails names why a response stopped short of
// completing (finish_reason length → max_output_tokens, content
// filter → content_filter).
type responseIncompleteDetails struct {
	Reason string `json:"reason"`
}

// responsesResponse is the served Responses object.
// ChildResponseIDs is set only by GET /v1/responses/{id} (from
// the ledger tree, or the in-memory store when no ledger is
// wired): the ids of the responses that branched off this one.
// It is absent on the object as served by a create (no child can
// exist yet) and on the list endpoint, which serves the stored
// objects byte-identical.
type responsesResponse struct {
	ID                 string                     `json:"id"`
	Object             string                     `json:"object"`
	CreatedAt          int64                      `json:"created_at"`
	Status             string                     `json:"status"`
	Model              string                     `json:"model"`
	Output             []responseOutputItem       `json:"output"`
	Usage              *responseUsage             `json:"usage,omitempty"`
	PreviousResponseID *string                    `json:"previous_response_id"`
	Metadata           map[string]string          `json:"metadata,omitempty"`
	IncompleteDetails  *responseIncompleteDetails `json:"incomplete_details,omitempty"`
	Error              *responseErrorBody         `json:"error,omitempty"`
	ChildResponseIDs   []string                   `json:"child_response_ids,omitempty"`
}

// responseErrorBody is the error block of a failed response.
type responseErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// responsesFeedEntry is one row of GET /dashboard/responses:
// identifiers and counts only — never input or output text.
// SessionTokens is the session chain's cumulative input+output
// tokens up to and including this response (CHECKLIST 243:
// per-turn admission counts a turn before the next is admitted).
type responsesFeedEntry struct {
	ID                 string `json:"id"`
	Model              string `json:"model"`
	Status             string `json:"status"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	Provider           string `json:"provider,omitempty"`
	CreatedAt          int64  `json:"created_at"`
	Stream             bool   `json:"stream"`
	SessionTokens      int    `json:"session_tokens,omitempty"`
}

// responsesFeed is the GET /dashboard/responses payload.
// SessionAdmissionRefused counts continuation turns refused by
// the per-turn session-token admission (CHECKLIST 243);
// StrippedToolCalls counts assistant tool calls dropped from a
// fallback request because the user already saw them
// (CHECKLIST 243, LiteLLM #51683). SessionTokenCap is the
// configured per-session token cap (CHECKLIST 263–264): 0
// means counting-only, never refused. Restored (CHECKLIST
// 315–316) is how many stored responses the last startup
// restore loaded from the persistence file — 0 on a fresh
// install or a server with no file wired, never a count of
// records created since this process started.
type responsesFeed struct {
	Responses               []responsesFeedEntry `json:"responses"`
	Stored                  int                  `json:"stored"`
	Created                 uint64               `json:"created"`
	Continuations           uint64               `json:"continuations"`
	Evicted                 uint64               `json:"evicted"`
	Restored                int                  `json:"restored"`
	SessionTokenCap         int                  `json:"session_token_cap"`
	SessionAdmissionRefused uint64               `json:"session_admission_refused"`
	StrippedToolCalls       uint64               `json:"stripped_tool_calls"`
}

// WithResponsesFile wires the Responses persistence file
// (CHECKLIST 315–316): <data-dir>/responses.json, the file the
// store persists to on every mutation and restores from at
// startup (see RestoreResponses). Empty (tests, servers without
// a data dir) means transcripts live in memory only — a
// restart then loses them, and continuations answer 404, the
// pre-315 behaviour. The production server wires the data-dir
// file in openStack.
func WithResponsesFile(path string) Option {
	return func(s *Server) { s.responsesFile = path }
}

// RestoreResponses loads the persisted Responses transcripts
// back into the in-memory store (CHECKLIST 315–316). It runs
// once, at startup, after the server is built (openStack calls
// it right after New): the store is empty at that point, so
// nothing live is ever overwritten. A missing file is a fresh
// install (nothing to restore); a corrupt file is logged and
// the store starts empty rather than failing the boot on a
// file the gateway can live without — the failure is visible
// in the log, never silent. Records older than the TTL, or
// over the cap, are not restored (the store enforces both).
// It is safe to call with no file wired (a no-op) and safe to
// call twice (the second call restores nothing: the ids are
// already live).
func (s *Server) RestoreResponses() {
	if s.responsesFile == "" || s.responses == nil {
		return
	}
	recs, err := responses.LoadFile(s.responsesFile)
	if err != nil {
		s.logger.Printf("responses restore: %v; starting with an empty store", logSafeError(err))
		return
	}
	if n := s.responses.Restore(recs); n > 0 {
		s.responsesRestored = n
		s.logger.Printf("responses restore: %d stored responses restored from %s", n, s.responsesFile)
	}
}

// persistResponses rewrites the persistence file from the live
// store (CHECKLIST 315–316). It runs after every store
// mutation — a create, a stream's terminal store, a delete —
// so the file is always a complete copy of the store. With no
// file wired it is a no-op. A write failure is logged, never
// surfaced to the client whose response was already served:
// the in-memory store stays the continuation source of truth,
// and the next mutation retries the write. Writes are
// serialised, so a concurrent create's older snapshot can
// never win the rename and drop a newer record.
func (s *Server) persistResponses() {
	if s.responsesFile == "" || s.responses == nil {
		return
	}
	s.responsesPersistMu.Lock()
	defer s.responsesPersistMu.Unlock()
	if err := responses.SaveFile(s.responsesFile, s.responses.Snapshot()); err != nil {
		s.logger.Printf("responses persist: %v", logSafeError(err))
	}
}

// responseTreeNode is one node of the GET /dashboard/responses/tree
// payload: a served response's identity in the tree, with its
// branches nested. ParentMissing marks a node whose parent id is
// set but no node with that id is in the tree — the parent was
// deleted, evicted, or recorded before the tree existed — so the
// dashboard renders it as a root instead of dropping it or
// silently re-parenting it. No input or output text, ever.
type responseTreeNode struct {
	ID            string              `json:"id"`
	ParentID      string              `json:"parent_id,omitempty"`
	Model         string              `json:"model"`
	Provider      string              `json:"provider,omitempty"`
	Status        string              `json:"status"`
	CreatedAt     int64               `json:"created_at"`
	Stream        bool                `json:"stream"`
	ParentMissing bool                `json:"parent_missing,omitempty"`
	Children      []*responseTreeNode `json:"children"`
}

// responseTreeFeed is the GET /dashboard/responses/tree payload:
// the whole forest (roots with nested branches), oldest first.
// Total is every node in the tree; Branched is the nodes that have
// a parent. An empty forest (no responses recorded since the tree
// existed) is roots [], total 0 — the dashboard's honest empty
// state — never a synthesised root.
type responseTreeFeed struct {
	Roots    []*responseTreeNode `json:"roots"`
	Total    int                 `json:"total"`
	Branched int                 `json:"branched"`
}

// recordResponseTree writes one tree node to the ledger when one
// is wired (WithLedger). It never fails a request: the in-memory
// store is the continuation source of truth, the ledger is the
// durable tree index — a ledger write failure is logged, not
// surfaced to a client whose response was already served.
func (s *Server) recordResponseTree(rec responses.Record) {
	if s.ledger == nil {
		return
	}
	if err := s.ledger.RecordResponseTree(ledger.ResponseTreeNode{
		ID:        rec.ID,
		ParentID:  rec.PreviousID,
		Model:     rec.Model,
		Provider:  rec.ProviderID,
		Status:    rec.Status,
		CreatedAt: rec.CreatedAt,
		Stream:    rec.Stream,
	}); err != nil {
		s.logger.Printf("responses tree: ledger record %s failed: %v", rec.ID, logSafeError(err))
	}
}

// responseChildren returns the ids of the responses that branched
// off id: from the ledger tree when one is wired (durable — it
// survives a restart), otherwise from the in-memory store. A
// ledger read failure falls back to the store, never to an error:
// the children list is enrichment, not the response itself.
func (s *Server) responseChildren(id string) []string {
	if s.ledger != nil {
		if kids, err := s.ledger.ResponseTreeChildren(id); err == nil {
			out := make([]string, 0, len(kids))
			for _, k := range kids {
				out = append(out, k.ID)
			}
			return out
		}
	}
	if s.responses == nil {
		return nil
	}
	return s.responses.Children(id)
}

// responseTreeNodes returns the flat tree nodes for the dashboard
// tree endpoint: from the ledger when one is wired, otherwise from
// the in-memory store (newest 200, the store's cap). A ledger read
// failure falls back to the store the same way responseChildren
// does. Nodes come back oldest first, which is the order the tree
// builder nests them in.
func (s *Server) responseTreeNodes() []ledger.ResponseTreeNode {
	if s.ledger != nil {
		if nodes, err := s.ledger.ResponseTreeAll(0); err == nil {
			return nodes
		} else {
			s.logger.Printf("responses tree: ledger read failed: %v", logSafeError(err))
		}
	}
	if s.responses == nil {
		return nil
	}
	recent := s.responses.Recent(200)
	nodes := make([]ledger.ResponseTreeNode, 0, len(recent))
	for i := len(recent) - 1; i >= 0; i-- {
		rec := recent[i]
		nodes = append(nodes, ledger.ResponseTreeNode{
			ID:        rec.ID,
			ParentID:  rec.PreviousID,
			Model:     rec.Model,
			Provider:  rec.ProviderID,
			Status:    rec.Status,
			CreatedAt: rec.CreatedAt,
			Stream:    rec.Stream,
		})
	}
	return nodes
}

// buildResponseTreeFeed nests flat nodes into the forest: a node is
// a root when it has no parent, or when its parent is not in the
// node set (deleted, evicted, or pre-tree) — the latter is flagged
// ParentMissing. Children keep creation order (the flat list is
// oldest first). A node that names itself as parent, or a cycle,
// cannot nest: it is rendered as a root, never dropped.
func buildResponseTreeFeed(nodes []ledger.ResponseTreeNode) responseTreeFeed {
	feed := responseTreeFeed{Roots: []*responseTreeNode{}}
	byID := make(map[string]*responseTreeNode, len(nodes))
	for _, n := range nodes {
		node := &responseTreeNode{
			ID:        n.ID,
			ParentID:  n.ParentID,
			Model:     n.Model,
			Provider:  n.Provider,
			Status:    n.Status,
			CreatedAt: n.CreatedAt,
			Stream:    n.Stream,
			Children:  []*responseTreeNode{},
		}
		byID[n.ID] = node
		if n.ParentID != "" {
			feed.Branched++
		}
	}
	feed.Total = len(nodes)
	for _, n := range nodes {
		node := byID[n.ID]
		if n.ParentID == "" || n.ParentID == n.ID {
			if n.ParentID == n.ID {
				node.ParentMissing = true
			}
			feed.Roots = append(feed.Roots, node)
			continue
		}
		if parent, ok := byID[n.ParentID]; ok {
			parent.Children = append(parent.Children, node)
			continue
		}
		node.ParentMissing = true
		feed.Roots = append(feed.Roots, node)
	}
	return feed
}

// ---------------------------------------------------------------------------
// Decode + translate.
// ---------------------------------------------------------------------------

// responsesRoutingDetail turns a strict routing-decode error into
// the one detail worth reporting: the unknown field name when that
// is what failed, otherwise the decoder's own message. (Mirrors
// provider.routingDecodeDetail, which is unexported.)
func responsesRoutingDetail(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, `unknown field "`); i >= 0 {
		rest := msg[i+len(`unknown field "`):]
		if j := strings.Index(rest, `"`); j >= 0 {
			return `unknown field "` + rest[:j] + `"`
		}
	}
	return msg
}

// decodeResponsesRequest decodes + validates a Responses request
// body. It returns the request and the translated chat messages
// (input items only — instructions and any continuation transcript
// are prepended by the caller). The routing contract is decoded
// strictly: an unknown key inside "routing" is a 400 naming the
// key, exactly like the chat contract.
func decodeResponsesRequest(data []byte) (responsesRequest, []provider.Message, string) {
	var req responsesRequest
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&req); err != nil {
		return req, nil, "invalid request body"
	}
	// Strict routing decode (same rule as ChatRequest).
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err == nil {
		if v, ok := fields["routing"]; ok && string(v) != "null" {
			sdec := json.NewDecoder(bytes.NewReader(v))
			sdec.DisallowUnknownFields()
			var strict provider.RequestRouting
			if err := sdec.Decode(&strict); err != nil {
				return req, nil, fmt.Sprintf("invalid routing: %s (known fields: order, only, exclude, sort, max_price, data_collection)", responsesRoutingDetail(err))
			}
			req.Routing = &strict
		}
	}
	if req.Model == "" {
		return req, nil, "model is required"
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		return req, nil, "input is required"
	}
	if req.MaxOutputTokens < 0 {
		return req, nil, "max_output_tokens must not be negative"
	}
	for _, name := range []string{"temperature", "top_p"} {
		var v float64
		if name == "temperature" {
			v = req.Temperature
		} else {
			v = req.TopP
		}
		if v != v || v > 1e308 || v < -1e308 {
			return req, nil, name + " must be a finite number"
		}
	}
	msgs, errMsg := responsesInputToMessages(req.Input)
	if errMsg != "" {
		return req, nil, errMsg
	}
	// Tools: function tools only, each with a name.
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			return req, nil, fmt.Sprintf("unsupported tool type %q (only function tools are supported)", t.Type)
		}
		if t.Name == "" {
			return req, nil, "tools[].name is required"
		}
	}
	// Tool choice: "auto" / "none" / "required", or a function
	// object {type:"function", name:"…"} mapped to the chat shape.
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var asString string
		if err := json.Unmarshal(req.ToolChoice, &asString); err == nil {
			switch asString {
			case "auto", "none", "required":
			default:
				return req, nil, fmt.Sprintf("unsupported tool_choice %q (want auto, none, required, or a function object)", asString)
			}
		} else {
			var obj struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(req.ToolChoice, &obj); err != nil || obj.Type != "function" || obj.Name == "" {
				return req, nil, "unsupported tool_choice (want auto, none, required, or {\"type\":\"function\",\"name\":\"…\"})"
			}
		}
	}
	return req, msgs, ""
}

// responsesInputToMessages translates a Responses input (string or
// array of items) into chat messages.
func responsesInputToMessages(input json.RawMessage) ([]provider.Message, string) {
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "" || trimmed == "null" {
		return nil, "input is required"
	}
	// String form: one user turn.
	var asString string
	if err := json.Unmarshal(input, &asString); err == nil {
		if strings.TrimSpace(asString) == "" {
			return nil, "input is required"
		}
		return []provider.Message{{Role: "user", Content: asString}}, ""
	}
	var items []responsesInputItem
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, "input must be a string or an array of items"
	}
	if len(items) == 0 {
		return nil, "input is required"
	}
	msgs := make([]provider.Message, 0, len(items))
	for i, it := range items {
		switch it.Type {
		case "", "message":
			// A message item: role + content (string or parts).
			role := it.Role
			if role == "" {
				role = "user"
			}
			switch role {
			case "system", "developer", "user", "assistant":
			default:
				return nil, fmt.Sprintf("input[%d].role must be one of system, developer, user, assistant", i)
			}
			text, parts, errMsg := responsesContentText(it.Content)
			if errMsg != "" {
				return nil, fmt.Sprintf("input[%d].content: %s", i, errMsg)
			}
			if strings.TrimSpace(text) == "" && role != "assistant" {
				return nil, fmt.Sprintf("input[%d].content is required", i)
			}
			m := provider.Message{Role: role, Content: text}
			if len(parts) > 0 {
				m.Parts = parts
			}
			msgs = append(msgs, m)
		case "function_call":
			// A prior tool call, replayed as an assistant turn.
			if it.CallID == "" || it.Name == "" {
				return nil, fmt.Sprintf("input[%d]: function_call items need call_id and name", i)
			}
			msgs = append(msgs, provider.Message{
				Role: "assistant",
				ToolCalls: []provider.ToolCall{{
					ID:   it.CallID,
					Type: "function",
					Function: provider.ToolCallFunction{
						Name:      it.Name,
						Arguments: it.Arguments,
					},
				}},
			})
		case "function_call_output":
			// A tool result, replayed as a tool turn.
			if it.CallID == "" {
				return nil, fmt.Sprintf("input[%d]: function_call_output items need call_id", i)
			}
			msgs = append(msgs, provider.Message{
				Role:       "tool",
				Content:    it.Output,
				ToolCallID: it.CallID,
			})
		default:
			return nil, fmt.Sprintf("input[%d]: unsupported item type %q", i, it.Type)
		}
	}
	return msgs, ""
}

// responsesContentText decodes message content (string or an array
// of input_text/output_text parts) into plain text + wire parts.
func responsesContentText(raw json.RawMessage) (string, []provider.ContentPart, string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil, ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil, ""
	}
	var parts []responsesInputPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, "must be a string or an array of text parts"
	}
	var sb strings.Builder
	var wireParts []provider.ContentPart
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			sb.WriteString(p.Text)
			wireParts = append(wireParts, provider.ContentPart{Type: "text", Text: p.Text})
		default:
			return "", nil, fmt.Sprintf("unsupported content part type %q", p.Type)
		}
	}
	return sb.String(), wireParts, ""
}

// responsesChatRequest translates a decoded Responses request into
// the canonical chat request the engine routes. base is the
// continuation transcript (nil for a fresh create); instructions
// becomes the leading system message when set. The endpoint marks
// the surface for the replay exemption; PreviousResponseID carries
// the continuation id ("" for a fresh create); PinProviderID the
// original provider for continuations.
func responsesChatRequest(req responsesRequest, inputMsgs, base []provider.Message, pinProvider string) provider.ChatRequest {
	msgs := make([]provider.Message, 0, len(base)+len(inputMsgs)+1)
	if strings.TrimSpace(req.Instructions) != "" {
		msgs = append(msgs, provider.Message{Role: "system", Content: req.Instructions})
	}
	msgs = append(msgs, base...)
	msgs = append(msgs, inputMsgs...)
	chat := provider.ChatRequest{
		Model:              req.Model,
		Messages:           msgs,
		Stream:             req.Stream,
		MaxTokens:          req.MaxOutputTokens,
		Temperature:        req.Temperature,
		TopP:               req.TopP,
		Routing:            req.Routing,
		Endpoint:           provider.EndpointResponses,
		PreviousResponseID: req.PreviousResponseID,
		PinProviderID:      pinProvider,
	}
	for _, t := range req.Tools {
		td := provider.ToolDefinition{Type: "function"}
		td.Function.Name = t.Name
		td.Function.Description = t.Description
		td.Function.Parameters = t.Parameters
		chat.Tools = append(chat.Tools, td)
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var asString string
		if err := json.Unmarshal(req.ToolChoice, &asString); err == nil {
			chat.ToolChoice = json.RawMessage(`"` + asString + `"`)
		} else {
			var obj struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.ToolChoice, &obj)
			chat.ToolChoice = json.RawMessage(fmt.Sprintf(`{"type":"function","function":{"name":%q}}`, obj.Name))
		}
	}
	return chat
}

// ---------------------------------------------------------------------------
// Response object construction.
// ---------------------------------------------------------------------------

// stripDanglingToolCalls drops, from the engine request, every
// assistant tool call with no matching tool result anywhere in
// the request (CHECKLIST 243, LiteLLM #51683): a call the user
// already saw, delivered on an earlier turn, whose result never
// arrived. Sending it to a fallback target again lets that
// target re-emit the same call — and the client re-execute it —
// so the fallback request excludes it. Only the ENGINE request
// is stripped: the stored transcript keeps the full history (the
// record stays honest), and a later create carrying the result
// re-pairs the call, so nothing is lost. A call with an empty id
// cannot be matched to a result and is kept: it is never dropped
// on a guess. Tool-result messages are never dropped — a result
// whose call was stripped is the client's own record of work
// done, and the provider can ignore an unmatched result more
// safely than the gateway can delete it. The returned count is
// the number of calls stripped (0 when nothing dangled).
func stripDanglingToolCalls(msgs []provider.Message) ([]provider.Message, int) {
	results := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" {
			results[m.ToolCallID] = true
		}
	}
	stripped := 0
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			kept := make([]provider.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				if tc.ID != "" && !results[tc.ID] {
					stripped++
					continue
				}
				kept = append(kept, tc)
			}
			if len(kept) != len(m.ToolCalls) {
				// m is the range copy: assigning the filtered
				// list here never mutates the caller's message.
				m.ToolCalls = kept
			}
		}
		out = append(out, m)
	}
	return out, stripped
}

// countTextTokens counts one text locally, for session-turn
// accounting when the upstream reported no usage: the same
// estimator the prompt gates use (tokencount.Count), so a turn's
// completion count and its prompt count come from one ruler. A
// counting failure returns 0 — the turn proceeds with no
// completion count rather than being refused on a guess.
func countTextTokens(model, text string) int {
	if text == "" {
		return 0
	}
	count, err := tokencount.Count(model, text)
	if err != nil {
		return 0
	}
	return count.Tokens
}

// sessionTurnCompletion counts one turn's completion tokens: the
// provider-reported completion count when the upstream reported
// it (> 0), else the local count of the delivered text. The
// delivered text is authoritative for streams (the drain's
// response carries no usage) and for failed streams (the partial
// text actually sent).
func sessionTurnCompletion(resp *provider.ChatResponse, deliveredText string) int {
	if resp != nil && resp.Usage.CompletionTokens > 0 {
		return resp.Usage.CompletionTokens
	}
	text := deliveredText
	if resp != nil && text == "" {
		text = resp.Content
	}
	model := ""
	if resp != nil {
		model = resp.Model
	}
	return countTextTokens(model, text)
}

// Response object construction.
// ---------------------------------------------------------------------------

// responsesStatus maps a chat finish reason to a Responses status
// + incomplete detail. "stop" / "tool_calls" complete; "length"
// stops short on output tokens; "content_filter" on policy.
func responsesStatus(finishReason string) (string, *responseIncompleteDetails) {
	switch finishReason {
	case "", "stop", "tool_calls":
		return responses.StatusCompleted, nil
	case "length":
		return responses.StatusIncomplete, &responseIncompleteDetails{Reason: "max_output_tokens"}
	case "content_filter":
		return responses.StatusIncomplete, &responseIncompleteDetails{Reason: "content_filter"}
	default:
		return responses.StatusIncomplete, &responseIncompleteDetails{Reason: finishReason}
	}
}

// responsesUsage maps a chat usage block to the Responses usage
// block, carrying the provider-reported cached/reasoning splits
// only when the upstream reported them.
func responsesUsage(u provider.Usage) *responseUsage {
	out := &responseUsage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
	if u.CachedTokens > 0 {
		out.InputDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: u.CachedTokens}
	}
	if u.ReasoningTokens > 0 {
		out.OutputDetails = &struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		}{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

// newFunctionCallID mints an output-item id for a function_call
// item: "fc_" + 24 hex chars.
func newFunctionCallID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fc_" + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))[:24]
	}
	return "fc_" + hex.EncodeToString(b[:])
}

// buildResponsesObject builds the served Responses object from a
// completed chat response. msgID is the message output-item id;
// prev is the previous response id (nil when this is a fresh
// create). The assistant turn it records (for the transcript) is
// returned alongside.
func buildResponsesObject(id, msgID string, req responsesRequest, resp *provider.ChatResponse) (responsesResponse, provider.Message) {
	status, incomplete := responsesStatus(resp.FinishReason)
	model := resp.Model
	if model == "" {
		model = req.Model
	}
	msgItem := responseOutputItem{
		Type:   "message",
		ID:     msgID,
		Status: status,
		Role:   "assistant",
	}
	if resp.Content != "" {
		msgItem.Content = []responseOutputText{{Type: "output_text", Text: resp.Content, Annotations: []any{}}}
	}
	output := []responseOutputItem{msgItem}
	assistant := provider.Message{Role: "assistant", Content: resp.Content}
	for _, tc := range resp.ToolCalls {
		output = append(output, responseOutputItem{
			Type:      "function_call",
			ID:        newFunctionCallID(),
			Status:    status,
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
		assistant.ToolCalls = append(assistant.ToolCalls, tc)
	}
	var prev *string
	if req.PreviousResponseID != "" {
		prev = &req.PreviousResponseID
	}
	obj := responsesResponse{
		ID:                 id,
		Object:             "response",
		CreatedAt:          time.Now().Unix(),
		Status:             status,
		Model:              model,
		Output:             output,
		Usage:              responsesUsage(resp.Usage),
		PreviousResponseID: prev,
		Metadata:           req.Metadata,
		IncompleteDetails:  incomplete,
	}
	return obj, assistant
}

// servingProvider names the provider that served the last finished
// request, from the route trace (the successful attempt). "" when
// the engine exposes no trace or no attempt succeeded — the
// continuation then has no pin and routes normally.
func (s *Server) servingProvider() string {
	tp, ok := s.engine.(traceProvider)
	if !ok || tp == nil {
		return ""
	}
	trace := tp.LastTrace()
	if trace == nil {
		return ""
	}
	for i := len(trace.Attempts) - 1; i >= 0; i-- {
		if trace.Attempts[i].Outcome == "success" {
			return trace.Attempts[i].Provider
		}
	}
	return ""
}

// storeResponsesRecord stores one served response (transcript +
// served object) under id, for retrieve + continuation.
// transcriptBase is the conversation so far (continuation base +
// this turn's input messages, WITHOUT the per-turn instructions
// system message — instructions apply to one response only, so
// they must not accumulate into the stored transcript); the
// assistant turn is appended here. turnPrompt/turnCompletion are
// this turn's counted tokens (see sessionTurnCompletion): the
// record carries them plus the session chain's cumulative totals,
// so the next turn's admission can count before admitting
// (CHECKLIST 243). The stored transcript is the FULL history —
// the engine request's dangling-tool-call strip (see
// handleResponses) never rewrites what is stored.
func (s *Server) storeResponsesRecord(id string, req responsesRequest, transcriptBase []provider.Message, assistant provider.Message, obj responsesResponse, stream bool, turnPrompt, turnCompletion int, keyID string) {
	if s.responses == nil {
		return
	}
	transcript := append(append([]provider.Message(nil), transcriptBase...), assistant)
	transcriptJSON, _ := json.Marshal(transcript)
	objJSON, _ := json.Marshal(obj)
	sessionPrompt, sessionCompletion := turnPrompt, turnCompletion
	if req.PreviousResponseID != "" {
		if parent, ok := s.responses.Get(req.PreviousResponseID); ok {
			sessionPrompt += parent.SessionInputTokens
			sessionCompletion += parent.SessionOutputTokens
		}
	}
	rec := responses.Record{
		ID:                  id,
		Model:               obj.Model,
		ProviderID:          s.servingProvider(),
		Status:              obj.Status,
		CreatedAt:           obj.CreatedAt,
		PreviousID:          req.PreviousResponseID,
		Stream:              stream,
		TranscriptJSON:      transcriptJSON,
		ResponseJSON:        objJSON,
		TurnInputTokens:     turnPrompt,
		TurnOutputTokens:    turnCompletion,
		SessionInputTokens:  sessionPrompt,
		SessionOutputTokens: sessionCompletion,
		KeyID:               keyID,
	}
	s.responses.Put(rec)
	// The tree index (CHECKLIST 237): the same record, durably.
	s.recordResponseTree(rec)
	// The transcript (CHECKLIST 315–316): the same record, in
	// the persistence file — a restart restores it.
	s.persistResponses()
}

// ---------------------------------------------------------------------------
// Handlers.
// ---------------------------------------------------------------------------

// handleResponses serves POST /v1/responses. The pre-engine gates
// mirror handleChatCompletions in the same order — routing-header
// rejection, decode, readiness, guardrails, request budget,
// prompt budget, key budget — so a Responses request can never
// bypass a check a chat request faces. The engine call itself is
// s.engine.Chat directly: the MCP selector loop is a chat
// completions feature and never runs for Responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	handlerStart := time.Now()
	var chat provider.ChatRequest
	engineCalled := false
	responded := false
	// Panic isolation (CHECKLIST 165-166), same backstop as the
	// chat handler: a panic anywhere on this path ends the
	// request with an honest 502, never kills the gateway, and
	// leaves a ledger row when the engine recorded no attempt.
	defer func() {
		if v := recover(); v != nil {
			s.noteServerPanic("server_responses", v)
			if !responded {
				s.writePanicFailure(w)
			}
			if chat.Model != "" && (!engineCalled || s.traceHasNoAttempts()) {
				rowCtx, rowCancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.recordPanicRow(rowCtx, s.callerKeyID(r), chat, handlerStart)
				rowCancel()
			}
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	// Request-scoped attribution (CHECKLIST 317-318): the
	// engine publishes this request's decision + trace into
	// the sink on ctx, so recordMetrics / setAutoRouteHeader
	// below attribute the outcome to this request even when a
	// neighbour finishes first.
	ctx = router.WithAttribution(ctx, router.NewAttribution())

	// The routing contract's canonical spelling is the "routing"
	// request-body field — same rejection as chat completions.
	if r.Header.Get("X-Codvian-Routing") != "" {
		writeError(w, http.StatusBadRequest, "X-Codvian-Routing header is not supported; send the routing contract in the \"routing\" request-body field")
		return
	}

	// Admission before I/O (CHECKLIST 243, LiteLLM #51428): same
	// rule as chat completions — a blocked or over-limit key is
	// refused before the body is read.
	if s.preBodyAdmission(w, r) {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return
	}
	req, inputMsgs, errMsg := decodeResponsesRequest(body)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return
	}
	// Intent tiers (CHECKLIST 277 (c), same rule as chat
	// completions in CHECKLIST 259): a request naming an unmapped
	// intent tier is refused here — before guardrails, budgets and
	// any provider call — with a 400 naming the tier, never the
	// generic empty-chain error the engine would return for the
	// unresolved virtual model. A mapped tier flows through to
	// the engine, which resolves it via AutoRoute.
	if refused := s.checkIntentTier(w, r, req.Model); refused {
		return
	}
	// Security cluster (CHECKLIST 245): a client-supplied Vertex
	// location/project is rejected unless the admin opted in.
	if ov, ok := provider.DetectVertexOverride(body); ok {
		if refused := s.checkClientVertexOverride(w, &ov); refused {
			return
		}
	}
	// Readiness gate: a gateway-provided local capability that is
	// not ready answers 503 with Retry-After here, before any
	// provider is dialled.
	if !s.checkLocalReadiness(w, r, req.Model) {
		return
	}

	// Continuation: resolve the stored response first — an
	// unknown previous_response_id is a 404 (response_not_found),
	// never a fresh conversation silently started in its place.
	// The stored transcript is the conversation base; the pin is
	// the provider that served the original response.
	var base []provider.Message
	pin := ""
	if req.PreviousResponseID != "" {
		rec, ok := s.responses.Get(req.PreviousResponseID)
		if !ok {
			writeError(w, http.StatusNotFound, "response not found: "+req.PreviousResponseID)
			return
		}
		if err := json.Unmarshal(rec.TranscriptJSON, &base); err != nil {
			// A stored transcript that cannot decode is a
			// server-side corruption, never the client's fault
			// to work around: fail honestly, 500.
			writeServerError(w, http.StatusInternalServerError, "stored response transcript is unreadable")
			return
		}
		pin = rec.ProviderID
	}

	chat = responsesChatRequest(req, inputMsgs, base, pin)
	chat.CallerID = s.callerKeyID(r)
	// No replay of delivered tool calls (CHECKLIST 243, LiteLLM
	// #51683): the fallback request excludes assistant tool
	// calls the user already saw with no result attached — a
	// fallback target must never re-emit them. Only the engine
	// request is stripped; the stored transcript (transcriptBase
	// below) keeps the full history.
	strippedMsgs, stripped := stripDanglingToolCalls(chat.Messages)
	chat.Messages = strippedMsgs
	if stripped > 0 {
		s.noteStrippedToolCalls(stripped)
	}
	// Security cluster (CHECKLIST 245): the deployment's pinned
	// parameters outrank the translated request's fields.
	s.applyDeploymentParams(w, &chat)
	// Guardrails run on the translated request BEFORE any
	// provider is dialled, exactly as on chat completions.
	if blocked := s.applyGuardrails(ctx, w, &chat, s.keyGuardrailScope(r)); blocked {
		return
	}
	// Plugin pre-request hooks (CHECKLIST 201), same as on
	// /v1/chat/completions: after guardrails, before any provider
	// is dialled. The ctx below carries any plugin-added outbound
	// headers to the provider client.
	if blocked, newCtx := s.applyPlugins(ctx, w, &chat); blocked {
		return
	} else {
		ctx = newCtx
	}
	// Per-key access schedule (M5.10): a key outside its allowed
	// hours is refused with a 403 here, before any budget is
	// touched.
	if refused := s.checkKeyAccessSchedule(w, r); refused {
		return
	}
	if refused := s.enforceRequestBudget(w, r, &chat); refused {
		return
	}
	if refused := s.enforcePromptBudget(w, r, &chat); refused {
		return
	}
	// M5.8: the budget check commits the prompt's estimated
	// cost; settled by the defer once this handler returns.
	if resID, refused := s.reserveKeyBudget(w, r, &chat); refused {
		return
	} else {
		defer s.settleReservation(resID)
	}
	// Per-key token limits (M5.3): global + model scopes for
	// the requested model, before the traffic check.
	if refused := s.checkKeyTokenLimits(w, r, "", chat.Model); refused {
		return
	}
	// Fair-share quota pools (M5.4): the caller's pools decide
	// before the traffic check.
	if refused := s.checkQuotaPools(w, r); refused {
		return
	}
	// Quota schedules (M5.9): the caller's pools' time windows
	// decide before the traffic check, right after the pool gate.
	if refused := s.checkQuotaSchedules(w, r); refused {
		return
	}
	// Internal budgets (M5.11): the operator's caps on total
	// spend (global / model / pool scopes) decide before the
	// traffic check, right after the schedule gate.
	if refused := s.checkInternalBudgets(w, r, chat.Model); refused {
		return
	}
	if !s.checkTeamBudget(w, r) {
		return
	}
	// Per-turn token admission for long-lived sessions
	// (CHECKLIST 243, LiteLLM #50730): a continuation turn is
	// admitted only after its tokens are counted — the session's
	// tokens so far plus this turn's prompt estimate, against
	// the configured session cap. Runs with the other budget
	// gates, before any provider is dialled, and before the
	// traffic check (a refused turn consumes no traffic slot).
	if refused := s.checkSessionTokenAdmission(w, r, &chat); refused {
		return
	}
	// Per-key traffic limits (CHECKLIST 195): same pre-call gate
	// as chat completions, on the translated request's prompt.
	if refused := s.checkKeyTrafficForChat(w, r, &chat); refused {
		return
	}
	// Per-key throttle delay + max sessions (M5.10): after every
	// refusal gate and before the engine is called; the session
	// slot is held for the whole handler via the defer.
	if !s.throttleKeyDelay(r) {
		return
	}
	sessionKey, admitted := s.tryEnterKeySession(w, r)
	if !admitted {
		return
	}
	defer s.exitKeySession(sessionKey)
	if lf := logFieldsFrom(ctx); lf != nil {
		lf.model = chat.Model
	}

	start := time.Now()
	if s.recorder != nil {
		s.recorder.IncInFlight()
		defer s.recorder.DecInFlight()
	}
	transcriptBase := append(append([]provider.Message(nil), base...), inputMsgs...)

	if req.Stream {
		engineCalled = true
		err := s.handleResponsesStream(w, ctx, req, chat, transcriptBase, start, s.callerKeyID(r))
		responded = true
		if isPanicError(err) {
			s.recordPanicRowIfUnrecorded(r.Context(), s.callerKeyID(r), chat, start)
		}
		s.recordMetrics(ctx, chat, nil, err, start)
		return
	}

	engineCalled = true
	resp, err := s.engine.Chat(ctx, chat)
	if err != nil {
		s.logPanicError(err)
		if isPanicError(err) {
			s.recordPanicRowIfUnrecorded(r.Context(), s.callerKeyID(r), chat, start)
		}
		s.recordMetrics(ctx, chat, resp, err, start)
		s.noteRequestBudgetMidChain(&chat, err)
		// Telemetry headers (M2.1): the trace still names the
		// attempted chain; tokens/cost stay absent.
		meta := s.responseMeta(ctx, chat, resp, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, s.statusForEngineError(err))
		s.handleEngineError(w, err)
		return
	}
	if resp == nil {
		s.recordMetrics(ctx, chat, resp, errNilUpstreamResponse, start)
		meta := s.responseMeta(ctx, chat, resp, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, http.StatusBadGateway)
		s.writeUpstreamFailure(w)
		return
	}
	// Response guardrail, non-streaming only, like chat.
	if blocked := s.applyResponseGuardrails(w, resp, s.keyGuardrailScope(r)); blocked {
		s.recordMetrics(ctx, chat, resp, errGuardrailResponseBlocked, start)
		return
	}
	s.recordMetrics(ctx, chat, resp, nil, start)
	s.checkRequestBudgetOverage(ctx, w, &chat, resp)
	id := responses.NewID()
	obj, assistant := buildResponsesObject(id, responses.NewMsgID(), req, resp)
	s.storeResponsesRecord(id, req, transcriptBase, assistant, obj, false, estimatePromptTokens(&chat), sessionTurnCompletion(resp, ""), s.callerKeyID(r))
	s.setAutoRouteHeader(ctx, w)
	s.setStrategyHeader(w)
	s.setDecisionHeader(w, resp.Model, chat.Model)
	// Telemetry headers (M2.1, choke point): cost/tokens/latency/
	// fallback from the real trace, usage and ledger price map.
	meta := s.responseMeta(ctx, chat, resp, start)
	s.writeResponseHeaders(w, ctx, meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	writeJSON(w, http.StatusOK, obj)
	responded = true
}

// handleGetResponse serves GET /v1/responses/{id}: the stored
// response object, byte-identical to what the create served —
// plus child_response_ids (CHECKLIST 237): the ids of the
// responses that branched off this one, from the ledger tree (or
// the in-memory store when no ledger is wired). The stored object
// itself is never rewritten: the children are enriched at read
// time, so the list endpoint keeps serving the stored bytes
// unchanged. An unknown id (never created, expired, evicted, or
// deleted — or created before transcripts persisted, CHECKLIST
// 315–316) is a 404 — never a synthesised object.
func (s *Server) handleGetResponse(w http.ResponseWriter, r *http.Request) {
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
	var obj responsesResponse
	if err := json.Unmarshal(rec.ResponseJSON, &obj); err != nil {
		// A stored object that cannot decode is a server-side
		// corruption, never the client's fault to work around.
		writeServerError(w, http.StatusInternalServerError, "stored response is unreadable")
		return
	}
	if kids := s.responseChildren(id); len(kids) > 0 {
		obj.ChildResponseIDs = kids
	}
	// Telemetry headers (M2.1): a stored object names no live
	// provider call, so only the model (from the stored object)
	// is reported — no invented cost, tokens or fallback.
	s.writeResponseHeaders(w, r.Context(), ResponseMeta{Model: obj.Model})
	writeJSON(w, http.StatusOK, obj)
}

// responsesList is the GET /v1/responses payload (the OpenAI list
// shape): the stored response objects, newest first, with cursor
// pagination. first_id / last_id are null on an empty page.
type responsesList struct {
	Object  string            `json:"object"`
	Data    []json.RawMessage `json:"data"`
	HasMore bool              `json:"has_more"`
	FirstID *string           `json:"first_id"`
	LastID  *string           `json:"last_id"`
}

// responsesListDefaultLimit is the page size when the client sends
// no limit; responsesListMaxLimit is the clamp for larger asks.
const (
	responsesListDefaultLimit = 20
	responsesListMaxLimit     = 100
)

// handleListResponses serves GET /v1/responses (CHECKLIST 204):
// the stored responses, newest first, paginated with limit (default
// 20, clamped to 100) + an `after` cursor (the id to continue after
// in newest-first order). Data carries the full stored response
// objects, byte-identical to what each create served. An unknown
// cursor is a 400 naming it — never a silent restart from the top;
// a bad limit is a 400; only order=desc is served (anything else is
// a 400 naming the value, never silently reordered).
func (s *Server) handleListResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
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
	if v := r.URL.Query().Get("order"); v != "" && v != "desc" {
		writeError(w, http.StatusBadRequest, "order must be \"desc\" (newest first)")
		return
	}
	offset := 0
	if after := r.URL.Query().Get("after"); after != "" {
		idx, ok := s.responses.IndexOf(after)
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown cursor: "+after)
			return
		}
		offset = idx + 1
	}
	page, total := s.responses.Page(offset, limit)
	out := responsesList{Object: "list", Data: []json.RawMessage{}, HasMore: offset+len(page) < total}
	for _, rec := range page {
		out.Data = append(out.Data, rec.ResponseJSON)
	}
	if len(page) > 0 {
		out.FirstID = &page[0].ID
		out.LastID = &page[len(page)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteResponse serves DELETE /v1/responses/{id} (CHECKLIST
// 204): deletes the stored transcript, so a later create naming
// the id as previous_response_id answers 404 (response_not_found)
// instead of resuming a conversation that no longer exists, and a
// later retrieve answers 404 too. Deleting an id that never existed
// (or already expired, evicted, or deleted) is an honest 404,
// never a silent success. The response is the OpenAI delete shape.
func (s *Server) handleDeleteResponse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "response id is required")
		return
	}
	if s.responses == nil || !s.responses.Delete(id) {
		writeError(w, http.StatusNotFound, "response not found: "+id)
		return
	}
	// The tree index goes with it (CHECKLIST 237): children keep
	// naming the deleted id as their parent and render as roots
	// with a missing parent — never silently re-parented.
	if s.ledger != nil {
		if err := s.ledger.DeleteResponseTree(id); err != nil {
			s.logger.Printf("responses tree: ledger delete %s failed: %v", id, logSafeError(err))
		}
	}
	// The transcript goes with it (CHECKLIST 315–316): the
	// persistence file is rewritten without the record, so a
	// restart never resurrects a deleted response.
	s.persistResponses()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "response", "deleted": true})
}

// handleDashboardResponsesTree serves GET /dashboard/responses/tree
// (CHECKLIST 238): the response forest — roots with their branches
// nested — from the ledger tree when one is wired, otherwise from
// the in-memory store. Identifiers and counts only, never input or
// output text. An empty forest is roots [], total 0: the
// dashboard's honest empty state.
func (s *Server) handleDashboardResponsesTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, buildResponseTreeFeed(s.responseTreeNodes()))
}

// handleDashboardResponses serves GET /dashboard/responses: the
// live Responses feed — recent response ids with model, status,
// previous id (the continuation chain), serving provider and
// creation time, plus store counts. Identifiers and counts only:
// input and output text never leave the store for a feed.
func (s *Server) handleDashboardResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	feed := responsesFeed{Responses: []responsesFeedEntry{}}
	if s.responses != nil {
		st := s.responses.Stats()
		feed.Stored = st.Stored
		feed.Created = st.Created
		feed.Continuations = st.Continuations
		feed.Evicted = st.Evicted
		feed.Restored = s.responsesRestored
		feed.SessionAdmissionRefused = s.fail243SessionAdmissionRefused.Load()
		feed.SessionTokenCap = s.responsesSessionTokenCap
		feed.StrippedToolCalls = s.fail243StrippedToolCalls.Load()
		for _, rec := range s.responses.Recent(50) {
			feed.Responses = append(feed.Responses, responsesFeedEntry{
				ID:                 rec.ID,
				Model:              rec.Model,
				Status:             rec.Status,
				PreviousResponseID: rec.PreviousID,
				Provider:           rec.ProviderID,
				CreatedAt:          rec.CreatedAt,
				Stream:             rec.Stream,
				SessionTokens:      rec.SessionInputTokens + rec.SessionOutputTokens,
			})
		}
	}
	writeJSON(w, http.StatusOK, feed)
}

// ---------------------------------------------------------------------------
// Streaming (SSE, Responses event shapes).
// ---------------------------------------------------------------------------

// handleResponsesStream streams a Responses create as SSE events
// in the OpenAI Responses shape: response.created,
// response.output_item.added, response.output_text.delta,
// response.output_text.done, response.output_item.done,
// response.completed, then data: [DONE]. Tool-call deltas ride
// response.function_call_arguments.delta under a function_call
// output item. The response id is minted up front (response.created
// carries it) and an in_progress placeholder is stored before the
// first chunk, so a retrieve during the stream sees the live
// object; the terminal state overwrites it. A failure before any
// content is a plain JSON error (headers not yet sent); after
// content, an SSE error event + [DONE], never a provider switch.
func (s *Server) handleResponsesStream(w http.ResponseWriter, ctx context.Context, req responsesRequest, chat provider.ChatRequest, transcriptBase []provider.Message, start time.Time, keyID string) (err error) {
	id := responses.NewID()
	msgID := responses.NewMsgID()
	created := time.Now().Unix()
	model := req.Model
	// Telemetry stream-start headers (M2.1): request id, version
	// and the requested model are known before the first event;
	// cost/tokens/fallback ride the final trailer instead.
	s.writeStreamStartHeaders(w, ctx, model)

	// In-progress placeholder: retrieve works during the stream,
	// and a stream that dies mid-way leaves a failed record, not
	// a missing one.
	createdObj := responsesResponse{
		ID:                 id,
		Object:             "response",
		CreatedAt:          created,
		Status:             responses.StatusInProgress,
		Model:              model,
		Output:             []responseOutputItem{},
		PreviousResponseID: nil,
		Metadata:           req.Metadata,
	}
	if req.PreviousResponseID != "" {
		createdObj.PreviousResponseID = &req.PreviousResponseID
	}
	createdJSON, _ := json.Marshal(createdObj)
	if s.responses != nil {
		rec := responses.Record{
			ID:           id,
			Model:        model,
			Status:       responses.StatusInProgress,
			CreatedAt:    created,
			PreviousID:   req.PreviousResponseID,
			Stream:       true,
			ResponseJSON: createdJSON,
		}
		s.responses.Put(rec)
		// The tree index (CHECKLIST 237): the placeholder is a
		// node too — the terminal store re-records the same id
		// with its final status (upsert), so the tree never
		// shows a stream twice.
		s.recordResponseTree(rec)
		// The transcript (CHECKLIST 315–316): the placeholder
		// persists too, so a retrieve during the stream works
		// after a restart as well; the terminal store
		// overwrites it with the full transcript.
		s.persistResponses()
	}

	var sent strings.Builder // full text already sent to the client
	headersSent := false
	flusher, _ := w.(http.Flusher)

	var batch *sseBatcher
	if s.sseBatching.enabled {
		batch = newSSEBatcher(
			func(line string) error { _, err := io.WriteString(w, line); return err },
			func() {
				if flusher != nil {
					flusher.Flush()
				}
			},
			s.sseBatching.maxTokens, s.sseBatching.maxDelay, nil,
		)
		defer batch.Close()
	}
	ensureHeaders := func() {
		if headersSent {
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		headersSent = true
	}
	writeSSE := func(line string) error {
		ensureHeaders()
		if batch != nil {
			return batch.Write(line)
		}
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	writeSSENow := func(line string) error {
		ensureHeaders()
		if batch != nil {
			if err := batch.Flush(); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	writeEvent := func(eventType string, payload any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return writeSSE("event: " + eventType + "\ndata: " + string(b) + "\n\n")
	}
	writeEventNow := func(eventType string, payload any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return writeSSENow("event: " + eventType + "\ndata: " + string(b) + "\n\n")
	}
	// Panic isolation: a panic in the server's own stream code
	// ends the stream with a terminal error frame, never kills
	// the gateway. The in-progress record is marked failed below
	// via finishFailed when the stream ends in error.
	defer func() {
		if v := recover(); v != nil {
			pe := s.noteServerPanic("server_responses_stream", v)
			err = pe
			if !headersSent {
				s.writePanicFailure(w)
				return
			}
			payload, _ := json.Marshal(errorBody{Error: errorDetail{Message: "gateway internal error", Type: "server_error"}})
			_ = writeSSENow("data: " + string(payload) + "\n\n")
			_ = writeSSENow("data: [DONE]\n\n")
		}
	}()

	// finishFailed marks the stored record failed with the partial
	// output delivered so far, so a retrieve shows what actually
	// happened instead of a stale in_progress.
	finishFailed := func(content string, toolCalls []provider.ToolCall) {
		if s.responses == nil {
			return
		}
		resp := &provider.ChatResponse{Content: content, ToolCalls: toolCalls, Model: model}
		obj, assistant := buildResponsesObject(id, msgID, req, resp)
		obj.Status = responses.StatusFailed
		obj.Error = &responseErrorBody{Code: "server_error", Message: "upstream provider error"}
		obj.Usage = nil
		s.storeResponsesRecord(id, req, transcriptBase, assistant, obj, true, estimatePromptTokens(&chat), sessionTurnCompletion(resp, ""), keyID)
	}

	var full strings.Builder
	var toolCalls []provider.ToolCall
	// Per-index function-call tracking for argument deltas: the
	// output-item id minted on first sight, plus the call id and
	// name from that first delta, and the output_index the item
	// was added at (the message item is 0; function calls follow
	// in first-sight order).
	fcItemID := map[int]string{}
	fcCallID := map[int]string{}
	fcName := map[int]string{}
	fcArgs := map[int]*strings.Builder{}
	fcOutputIndex := map[int]int{}
	outputIndex := 0 // message item is 0; function calls follow
	itemAdded := false
	decisionSet := false

	// Stream redaction (B3-26 / :798): the Responses stream
	// calls the engine directly, not handleChatStream, so the
	// response checks never ran on its deltas. Same per-stream
	// redactor as handleChatStream: PEM-only by default, the
	// store-wired ResponseStreamRedactor when response
	// sanitization is on — and in block mode it cuts the
	// stream at the first line carrying a finding (see the
	// cut check after the engine call). One redactor per
	// stream (not safe for concurrent use).
	var respRedactor *guardrails.ResponseStreamRedactor
	var redactor streamChunkRedactor = guardrails.NewStreamRedactor()
	if s.responseSanitization {
		respRedactor = s.guardrails.NewResponseStreamRedactor("")
		redactor = respRedactor
	}

	onChunk := func(chunk provider.StreamChunk) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Decision transparency (CHECKLIST 199): the header goes
		// on the first chunk, before the first event (the
		// response.created event below) commits the headers.
		// Responses requests are never cacheable, so no
		// cache-replayed stream can reach this point.
		if !decisionSet {
			decisionSet = true
			s.setStreamDecisionHeader(w, chat.Model)
		}
		if !itemAdded {
			itemAdded = true
			if err := writeEvent("response.created", map[string]any{
				"type": "response.created", "response": createdObj,
			}); err != nil {
				return err
			}
			if err := writeEvent("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{
					"type": "message", "id": msgID, "status": "in_progress",
					"role": "assistant", "content": []any{},
				},
			}); err != nil {
				return err
			}
		}
		if chunk.Content != "" {
			content := redactor.WriteChunk(chunk.Content)
			// Response sanitization cut (B3-22, block mode):
			// the redactor withheld the first line carrying
			// a finding, so nothing further may be emitted.
			// Returning the sentinel stops the engine loop;
			// the terminal refusal is written by the cut
			// check after the engine call below. Without
			// this return the stream would keep running
			// silently and close with a normal completed
			// event, as if the response had been delivered
			// in full.
			if respRedactor != nil && respRedactor.Blocked() {
				return errResponseGuardrailBlocked
			}
			if content != "" {
				if err := writeEvent("response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "item_id": msgID,
					"output_index": 0, "content_index": 0, "delta": content,
				}); err != nil {
					return err
				}
				full.WriteString(content)
				sent.WriteString(content)
			}
		}
		for _, tc := range chunk.ToolCalls {
			idx := tc.Index
			if _, ok := fcItemID[idx]; !ok {
				outputIndex++
				fcItemID[idx] = newFunctionCallID()
				fcCallID[idx] = tc.ID
				fcName[idx] = tc.Name
				fcArgs[idx] = &strings.Builder{}
				fcOutputIndex[idx] = outputIndex
				if err := writeEvent("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": outputIndex,
					"item": map[string]any{
						"type": "function_call", "id": fcItemID[idx],
						"status": "in_progress", "call_id": tc.ID, "name": tc.Name,
						"arguments": "",
					},
				}); err != nil {
					return err
				}
			}
			if tc.Name != "" {
				fcName[idx] = tc.Name
			}
			if tc.ID != "" {
				fcCallID[idx] = tc.ID
			}
			if tc.Arguments != "" {
				fcArgs[idx].WriteString(tc.Arguments)
				if err := writeEvent("response.function_call_arguments.delta", map[string]any{
					"type": "response.function_call_arguments.delta", "item_id": fcItemID[idx],
					"output_index": fcOutputIndex[idx], "delta": tc.Arguments,
				}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	onDelta := func(delta string) error {
		if delta == "" {
			return nil
		}
		return onChunk(provider.StreamChunk{Content: delta})
	}

	var resp *provider.ChatResponse
	if ce, ok := s.engine.(chunkEngine); ok {
		resp, err = ce.ChatStreamChunks(ctx, chat, onChunk)
	} else {
		resp, err = s.engine.ChatStream(ctx, chat, onDelta)
	}
	s.logPanicError(err)
	// Response sanitization cut (B3-22, block mode): the
	// ResponseStreamRedactor cut the stream at the first line
	// carrying a finding. This check does NOT require err !=
	// nil: an engine that ignores the onChunk error returns
	// nil, and the stream must still end with a refusal,
	// never a normal completed event. The decision uses the
	// redactor's flag, not the error value, so it holds
	// either way. The withheld tail is never emitted (Flush
	// returns "" once cut). Like the chat path, this is a
	// policy refusal, never an upstream failure: the record
	// keeps the partial output actually delivered, and the
	// client gets the 403 / -32001 refusal — as a plain JSON
	// refusal when nothing was sent yet, as an SSE refusal
	// event + [DONE] once content was committed.
	if respRedactor != nil && respRedactor.Blocked() {
		kind := "secret"
		if f := respRedactor.Findings(); len(f) > 0 {
			kind = f[0].Kind
		}
		finishFailed(full.String(), toolCalls)
		s.logger.Printf("guardrail: response sanitization blocked responses stream (kind=%s); stream cut, terminal refusal -32001", kind)
		if sent.Len() == 0 && !headersSent {
			writePolicyRefusal(w, kind, "response")
		} else {
			payload, _ := json.Marshal(errorBody{Error: errorDetail{Message: "guardrail blocked stream: " + kind, Type: "policy_refusal", Code: guardrails.PolicyRefusalCode, Reason: kind}})
			_ = writeSSENow("data: " + string(payload) + "\n\n")
			_ = writeSSENow("data: [DONE]\n\n")
		}
		s.recordTelemetry(ctx, s.responseMeta(ctx, chat, resp, start), http.StatusForbidden)
		return errResponseGuardrailBlocked
	}
	if err != nil {
		if sent.Len() == 0 && !headersSent {
			// Nothing delivered: a plain JSON error is still
			// possible — and the failed record keeps the
			// partial (empty) output for retrieve.
			finishFailed("", nil)
			s.noteRequestBudgetMidChain(&chat, err)
			// Telemetry headers (M2.1): the trace still names
			// the attempted chain; tokens/cost stay absent.
			meta := s.responseMeta(ctx, chat, resp, start)
			s.writeResponseHeaders(w, ctx, meta)
			s.recordTelemetry(ctx, meta, s.statusForEngineError(err))
			s.handleEngineError(w, err)
			return err
		}
		// Committed stream: never switch providers; end with an
		// SSE error event + [DONE], and mark the record failed
		// with the partial output actually delivered. The alert
		// names the deployment that failed the stream
		// (CHECKLIST 243, LiteLLM #51034).
		finishFailed(full.String(), toolCalls)
		errDetail := s.streamErrorDetail()
		s.logger.Printf("responses stream failed after %d bytes sent: deployment=%s %v", sent.Len(), errDetail.Deployment, logSafeError(err))
		payload, _ := json.Marshal(errorBody{Error: errDetail})
		_ = writeSSENow("data: " + string(payload) + "\n\n")
		_ = writeSSENow("data: [DONE]\n\n")
		// Committed-error stream: no trailer (A8 rule) — record
		// it server-side with the engine-error status.
		s.recordTelemetry(ctx, s.responseMeta(ctx, chat, resp, start), s.statusForEngineError(err))
		return err
	}
	if resp == nil {
		finishFailed(full.String(), toolCalls)
		meta := s.responseMeta(ctx, chat, resp, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, http.StatusBadGateway)
		s.writeUpstreamFailure(w)
		return errNilUpstreamResponse
	}
	// Reassemble tool calls from the returned response (the
	// chunk deltas above carried fragments; the response holds
	// the completed calls).
	toolCalls = resp.ToolCalls
	// A chunk engine that never emitted response.created (an
	// empty stream that still "succeeded" — the engine treats an
	// empty stream as a failure, so this is a no-content success
	// only for engines that do not go through the preread path):
	// emit the framing events now so the client still sees a
	// well-formed sequence.
	if !itemAdded {
		itemAdded = true
		if err := writeEvent("response.created", map[string]any{
			"type": "response.created", "response": createdObj,
		}); err != nil {
			return err
		}
		if err := writeEvent("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{
				"type": "message", "id": msgID, "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		}); err != nil {
			return err
		}
	}
	// Stream redaction (M1.7): emit the redactor's withheld
	// tail — an unclosed PEM block as one placeholder — as a
	// final text delta before output_text.done, so no secret
	// reaches the client in any chunking. Withheld text was
	// never sent, so it joins the authoritative streamed
	// text (full) like any other delta.
	if tail := redactor.Flush(); tail != "" {
		if err := writeEvent("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": msgID,
			"output_index": 0, "content_index": 0, "delta": tail,
		}); err != nil {
			return err
		}
		full.WriteString(tail)
		sent.WriteString(tail)
	}
	if err := writeEvent("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": msgID,
		"output_index": 0, "content_index": 0, "text": full.String(),
	}); err != nil {
		return err
	}
	obj, assistant := buildResponsesObject(id, msgID, req, resp)
	// The function_call item ids in the completed object must
	// match the ids announced in response.output_item.added during
	// the stream, or a client tracking items by id sees two
	// different items for one call.
	for idx, itemID := range fcItemID {
		if oi := idx + 1; oi < len(obj.Output) {
			obj.Output[oi].ID = itemID
		}
	}
	// The streamed text is authoritative: a chunk engine's
	// reassembled content and the deltas must agree, but the
	// deltas are what the client actually received — use them.
	if obj.Output != nil && len(obj.Output) > 0 {
		if full.String() != "" {
			obj.Output[0].Content = []responseOutputText{{Type: "output_text", Text: full.String(), Annotations: []any{}}}
		}
	}
	for idx, tc := range resp.ToolCalls {
		itemID, ok := fcItemID[idx]
		if !ok {
			itemID = newFunctionCallID()
		}
		outIdx, ok := fcOutputIndex[idx]
		if !ok {
			outIdx = len(obj.Output) - 1
			if outIdx < 1 {
				outIdx = 1
			}
		}
		if err := writeEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": itemID,
			"output_index": outIdx, "arguments": tc.Function.Arguments,
		}); err != nil {
			return err
		}
		// The matching function_call output item's done event:
		// the item in the completed object, by output position.
		if oi := idx + 1; oi < len(obj.Output) {
			if err := writeEvent("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": outIdx,
				"item": obj.Output[oi],
			}); err != nil {
				return err
			}
		}
	}
	if err := writeEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": obj.Output[0],
	}); err != nil {
		return err
	}
	if err := writeEventNow("response.completed", map[string]any{
		"type": "response.completed", "response": obj,
	}); err != nil {
		return err
	}
	// Telemetry trailer (M2.1): the values that were not known
	// when the stream's headers were sent — cost, tokens,
	// fallback — as a final SSE comment before [DONE], and in
	// the log row.
	meta := s.responseMeta(ctx, chat, resp, start)
	if line := s.streamTrailerString(ctx, meta); line != "" {
		_ = writeSSENow(line)
	}
	s.logResponseMeta(meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	_ = writeSSENow("data: [DONE]\n\n")
	s.storeResponsesRecord(id, req, transcriptBase, assistant, obj, true, estimatePromptTokens(&chat), sessionTurnCompletion(resp, full.String()), keyID)
	return nil
}
