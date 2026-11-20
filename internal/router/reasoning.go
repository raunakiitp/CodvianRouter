// Reasoning continuity across failover (CHECKLIST 163-164).
//
// The engine keeps an in-memory replay store (internal/reasoning)
// of captured reasoning states, keyed by the conversation
// transcript: when an upstream returns thinking blocks (Anthropic),
// thought parts/signatures (Gemini), reasoningContent (Bedrock) or
// reasoning_content (OpenAI wire), the state is captured here; on
// the next turn — or after a failover mid-conversation — the
// stored state is injected back into the assistant message it
// belongs to, in the target wire's own shape. The wire converters
// (internal/wire) do the translation and refuse honestly when the
// target cannot represent the state; the router only labels the
// handoff for the trace and keeps the store.
//
// Two rules, enforced by construction, not by convention:
//
//  1. Reasoning is NEVER replayed as fresh user content: injection
//     only ever attaches a state to the assistant message whose
//     transcript produced it. A state that cannot be represented
//     on the target fails that hop (the chain moves on), it is
//     never rewritten into a user message.
//  2. A drop is never silent: reasoning.Check is the single source
//     of truth for what survives a hop, and the trace label
//     (AttemptTrace.Reasoning) says what happened — preserved,
//     translated, dropped with a reason, or unrepresentable —
//     using the same verdict the converter enforced, so the label
//     and the behaviour cannot drift.
//
// The store is in-memory only (TTL 2h, cap 200): a restart loses
// it, and the next turn then simply misses — never wrongly
// replays. Replay-exempt requests (embeddings, images, rerank,
// Responses, continuations, warm-ups) never touch the store:
// their reasoning state is neither captured nor injected.
package router

import (
	"errors"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// wireFormatClient is the optional interface a client exposes to
// say which wire it speaks. Every built-in client implements it
// (openAIClient.WireFormat, promoted through the native clients).
// A client that does not expose its wire format gets no label:
// guessing "openai" would mislabel a native-wire hop as
// unrepresentable, and a wrong label is worse than none.
type wireFormatClient interface {
	WireFormat() string
}

// ReasoningStats returns the replay store's counters (entries,
// captures, hits, misses, replays, evictions) — counts only,
// never content. Safe to call on an engine whose store is nil
// (zero stats).
func (e *Engine) ReasoningStats() reasoning.Stats {
	if e == nil || e.reasoning == nil {
		return reasoning.Stats{}
	}
	return e.reasoning.Stats()
}

// messageTurns converts provider messages to transcript turns for
// the replay key. Only role, text and tool-call identity feed the
// key — never the reasoning state itself, so a capture and the
// injection that must find it compute the same key.
func messageTurns(messages []provider.Message) []reasoning.Turn {
	turns := make([]reasoning.Turn, 0, len(messages))
	for _, m := range messages {
		turn := reasoning.Turn{Role: m.Role, Text: m.Content}
		for _, tc := range m.ToolCalls {
			turn.ToolCalls = append(turn.ToolCalls, reasoning.ToolCallRef{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		turns = append(turns, turn)
	}
	return turns
}

// injectReasoning restores captured reasoning states onto the
// assistant messages they belong to, for this request only. It
// returns a copy of req with the states attached (the caller's
// request is never mutated) — or req unchanged when there is
// nothing to restore. A message that already carries a state
// (a reasoning-aware client supplied one) is never overwritten.
// Replay-exempt requests are never injected.
func (e *Engine) injectReasoning(req provider.ChatRequest) provider.ChatRequest {
	if e == nil || e.reasoning == nil || ReplayExemption(req) != "" {
		return req
	}
	msgs := append([]provider.Message(nil), req.Messages...)
	changed := false
	for i := range msgs {
		if msgs[i].Role != RoleAssistant {
			continue
		}
		if msgs[i].Reasoning != nil && !msgs[i].Reasoning.Empty() {
			continue
		}
		key := reasoning.TranscriptKey(req.CallerID, messageTurns(msgs[:i+1]))
		if st, ok := e.reasoning.Get(key); ok {
			msgs[i].Reasoning = st.Clone()
			e.reasoning.NoteInjected()
			changed = true
		}
	}
	if !changed {
		return req
	}
	req.Messages = msgs
	return req
}

// captureReasoning stores a response's reasoning state under the
// transcript key the next turn will compute: the request's turns
// plus this response as the assistant turn. Replay-exempt
// requests are never captured; an empty state is never stored.
func (e *Engine) captureReasoning(req provider.ChatRequest, resp *provider.ChatResponse) {
	if e == nil || e.reasoning == nil || resp == nil || resp.Reasoning == nil || resp.Reasoning.Empty() || ReplayExemption(req) != "" {
		return
	}
	turns := messageTurns(req.Messages)
	turn := reasoning.Turn{Role: RoleAssistant, Text: resp.Content}
	for _, tc := range resp.ToolCalls {
		turn.ToolCalls = append(turn.ToolCalls, reasoning.ToolCallRef{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	turns = append(turns, turn)
	e.reasoning.Put(reasoning.TranscriptKey(req.CallerID, turns), *resp.Reasoning)
}

// handoffLabel computes the trace label for one hop: what happened
// to the restored reasoning state when it was (or would be) sent
// to this target, in this target's wire format. "" when the
// request carries no reasoning state. The verdict comes from
// reasoning.Check — the same check the wire converter enforces —
// so the label and the behaviour cannot drift. The label names
// only block kinds, wires and counts, never reasoning or prompt
// text.
func (e *Engine) handoffLabel(att Attempt, req provider.ChatRequest, client provider.Client) string {
	wf, ok := client.(wireFormatClient)
	if !ok {
		return ""
	}
	wireFmt := wf.WireFormat()
	if wireFmt == "" {
		return ""
	}
	var labels []string
	for _, m := range req.Messages {
		if m.Reasoning == nil || m.Reasoning.Empty() {
			continue
		}
		status, detail, err := reasoning.Check(m.Reasoning, wireFmt, att.Model)
		if err != nil {
			// Unrepresentable: Check returns no detail of
			// its own — the reason rides the error, and the
			// label should carry it (wire/model names only,
			// never reasoning or prompt text).
			var runrep *reasoning.UnrepresentableError
			if errors.As(err, &runrep) {
				detail = runrep.Detail
			}
		}
		if label := reasoning.Describe(status, detail); label != "" {
			labels = append(labels, label)
		}
	}
	return strings.Join(labels, "; ")
}

// attemptReasoningLabel is handoffLabel for an attempt whose
// client is looked up from the engine (streaming path: the
// attempt's client is not already in hand). "" when the provider
// has no live client.
func (e *Engine) attemptReasoningLabel(att Attempt, req provider.ChatRequest) string {
	client, ok := e.clientForAttempt(att)
	if !ok {
		return ""
	}
	return e.handoffLabel(att, req, client)
}

// isReasoningUnrepresentableErr reports whether err is a reasoning
// state the target wire cannot represent (*reasoning.
// UnrepresentableError, possibly wrapped by a wire converter).
// Such a failure is a property of that target, not of the request
// and not of the provider's health: the chain must move on to a
// target that can carry the reasoning, with no breaker, cooldown
// or health signal — the carve-out in noteFailure and
// noteStreamStartFailure.
func isReasoningUnrepresentableErr(err error) bool {
	var runrep *reasoning.UnrepresentableError
	return errors.As(err, &runrep)
}
