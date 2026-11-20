package router

import (
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// LiteLLM v1.111.0-rc.1 failover cluster (CHECKLIST 243-244,
// research-log entries 222-224): the pin-escape rule, the
// no-replay rule for delivered tool calls, the same-model-group
// retry for pre-output stream breaks, and the no-recursion rule
// for fallback config. The attempt loops themselves live in
// router.go; this file holds the small helpers and the counters
// those loops use, so the loop diffs stay minimal.

// pinFirst moves the pinned attempt (ChatRequest.PinProviderID — a
// Responses continuation's original provider) to the FRONT of an
// already-ordered chain, keeping the rest in order. pinChain puts
// the pin first before ordering, but the quota-hint sort and the
// strategy hook/tree can demote it again — so a continuation whose
// original provider has a low quota hint would silently NOT try
// that provider first, and the pin would be a preference in name
// only. Re-applying the pin after ordering makes "tried first"
// hold whatever the scoring says. It never adds a provider and
// never resurrects one the ordering removed (a zero-quota pin was
// already filtered out of ordered by filterZeroQuota, and stays
// out: a pin that cannot serve is skipped honestly, not forced).
// A chain with no pinned attempt is returned unchanged.
func pinFirst(ordered []Attempt, pin string) []Attempt {
	if pin == "" || len(ordered) == 0 {
		return ordered
	}
	for i, att := range ordered {
		if att.ProviderID == pin {
			if i == 0 {
				return ordered
			}
			out := make([]Attempt, 0, len(ordered))
			out = append(out, att)
			out = append(out, ordered[:i]...)
			out = append(out, ordered[i+1:]...)
			return out
		}
	}
	return ordered
}

// Failover243Stats is the engine's live account of the CHECKLIST
// 243 pin-escape rule: how many requests escaped their pin after
// a retryable failure on the pinned deployment, and which escape
// happened most recently. It is counts only — no request content,
// no prompt text.
type Failover243Stats struct {
	PinEscapes    uint64    `json:"pin_escapes"`
	LastPin       string    `json:"last_pin,omitempty"`
	LastEscapedTo string    `json:"last_escaped_to,omitempty"`
	LastEscapeAt  time.Time `json:"last_escape_at,omitempty"`
	// ToolReplaysSuppressed counts tool-call deltas a resumed
	// stream re-emitted for a call the client already received,
	// dropped before delivery (CHECKLIST 243, LiteLLM #51683).
	ToolReplaysSuppressed uint64 `json:"tool_replays_suppressed"`
}

// Failover243Stats returns a snapshot of the engine's live
// pin-escape outcomes (see Failover243Stats).
func (e *Engine) Failover243Stats() Failover243Stats {
	e.fail243Mu.Lock()
	defer e.fail243Mu.Unlock()
	return Failover243Stats{
		PinEscapes:            e.fail243PinEscapes,
		LastPin:               e.fail243LastPin,
		LastEscapedTo:         e.fail243LastEscapedTo,
		LastEscapeAt:          e.fail243LastEscapeAt,
		ToolReplaysSuppressed: e.fail243ToolReplays,
	}
}

// noteToolReplaysSuppressed counts n tool-call deltas dropped by
// the resumed-stream no-replay filter (see
// filterDeliveredToolCalls): replays the client already saw,
// never delivered twice.
func (e *Engine) noteToolReplaysSuppressed(n int) {
	if n <= 0 {
		return
	}
	e.fail243Mu.Lock()
	e.fail243ToolReplays += uint64(n)
	e.fail243Mu.Unlock()
}

// notePinEscape counts one request that escaped its pin: the
// pinned deployment was tried, failed retryably, and the loop has
// moved on to a different provider. The loops call it at most
// once per request (guarded by the caller's counted flag), so a
// request that fails over twice past the pin still counts one
// escape — the pin was escaped once, whatever happened after.
func (e *Engine) notePinEscape(pin, escapedTo string) {
	e.fail243Mu.Lock()
	e.fail243PinEscapes++
	e.fail243LastPin = pin
	e.fail243LastEscapedTo = escapedTo
	e.fail243LastEscapeAt = time.Now()
	e.fail243Mu.Unlock()
}

// filterDeliveredToolCalls drops, from chunk, every tool-call
// delta whose call id the client already received on an earlier
// stream of this request (delivered, by id). Deltas with an empty
// id are never dropped here — there is nothing to match them
// against — and a call the client has NOT seen passes through
// untouched, fragments included: within one stream a call's
// argument deltas share its id, and they must all flow, or the
// reassembled call loses arguments. The caller seeds delivered
// from req.DeliveredToolCallIDs and never adds to it mid-stream:
// a call first delivered by THIS stream is new, not a replay,
// and its later fragments must reach the client. The returned
// chunk shares the caller's slice storage for the kept deltas;
// the boolean reports whether anything was dropped.
func filterDeliveredToolCalls(chunk provider.StreamChunk, delivered map[string]bool) (provider.StreamChunk, bool) {
	if len(delivered) == 0 || len(chunk.ToolCalls) == 0 {
		return chunk, false
	}
	kept := make([]provider.ChunkToolCall, 0, len(chunk.ToolCalls))
	dropped := false
	for _, tc := range chunk.ToolCalls {
		if tc.ID != "" && delivered[tc.ID] {
			dropped = true
			continue
		}
		kept = append(kept, tc)
	}
	if !dropped {
		return chunk, false
	}
	chunk.ToolCalls = kept
	return chunk, true
}

// deliveredToolCallSet builds the lookup set for
// filterDeliveredToolCalls from the request's delivered ids.
// Nil when the request names none, so the drain can skip the
// filter entirely on fresh streams.
func deliveredToolCallSet(req provider.ChatRequest) map[string]bool {
	if len(req.DeliveredToolCallIDs) == 0 {
		return nil
	}
	set := make(map[string]bool, len(req.DeliveredToolCallIDs))
	for _, id := range req.DeliveredToolCallIDs {
		if id != "" {
			set[id] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}
