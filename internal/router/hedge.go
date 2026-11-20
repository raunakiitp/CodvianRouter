// Hedge body-size guard (M4.8, OmniRoute comboConfig.ts:177).
//
// A hedge doubles the upstream traffic of the first attempt: for a
// large request body that doubles the cost and the load for no
// benefit — the second attempt re-sends the same megabytes. Bodies
// over 256 KiB are never hedged, in either loop; the request still
// runs (and still fails over) on a single attempt.
//
// Default (deliberate divergence, B2-26): OmniRoute hedges off by
// default at 500 ms; we hedge ON by default at 150 ms (Config.
// HedgeDelay, router.go) — a faster default that cuts failover
// latency, and the hedge only ever fires on the first attempt of a
// non-explicit-order chain. The 256 KiB guard below is the same
// bound OmniRoute uses.
package router

import (
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// hedgeMaxBodyBytes is the largest request body that may be hedged
// (256 KiB, OmniRoute's bound).
const hedgeMaxBodyBytes = 256 << 10

// requestBodyBytes is the size of the request body that would be
// sent upstream: message text, multi-part text and image payloads,
// tool calls, and the tool/name fields — plus the tool DEFINITIONS
// (name, description, parameters) and the tool choice, which ride
// on every attempt too. A request with a small prompt but a huge
// tool list re-sends those megabytes on the hedge as surely as a
// huge prompt does, so they count toward the guard. It is a byte
// count of the gateway's own request shape — the same bytes every
// attempt re-sends — not an upstream estimate.
func requestBodyBytes(req provider.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += len(m.Content)
		for _, p := range m.Parts {
			n += len(p.Text)
			if p.ImageURL != nil {
				n += len(p.ImageURL.URL)
			}
		}
		for _, tc := range m.ToolCalls {
			n += len(tc.ID) + len(tc.Function.Name) + len(tc.Function.Arguments)
		}
		n += len(m.ToolCallID) + len(m.Name)
	}
	for _, t := range req.Tools {
		n += len(t.Type) + len(t.Function.Name) + len(t.Function.Description) + len(t.Function.Parameters)
	}
	n += len(req.ToolChoice)
	return n
}

// hedgeAllowed reports whether this request may be hedged: hedging
// configured on, and the body within the 256 KiB guard.
func (e *Engine) hedgeAllowed(req provider.ChatRequest) bool {
	return e.cfg.HedgeDelay > 0 && requestBodyBytes(req) <= hedgeMaxBodyBytes
}
