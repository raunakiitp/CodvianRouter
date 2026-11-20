// Semantic window + bypass cap shared by the engine (internal/degrade)
// and the relay (internal/server).
//
// M6.7 + M6.7b gave both paths the same 3-turn semantic window, but as
// a DUPLICATE: degrade.semanticText/messageText and
// relay.relaySemanticText/relayMessageText computed the same text from
// the same messages. This file is the single canonical home (M6.7c):
// internal/provider is cycle-free for both callers (provider imports
// neither degrade nor server, and neither degrade nor server is in
// provider's transitive deps), so both now delegate here. The window
// text is pinned byte-identically by TestM67SemanticTextWindow
// (internal/degrade), TestM67bRelaySemanticWindow (internal/server)
// and TestSemanticWindowText below — if any literal changes, this
// helper is wrong, not the tests.
package provider

import "strings"

// SemanticWindowTurns is how many of the most recent conversation
// turns the semantic cache matches on (M6.7): the last 3 turns.
// Formerly degrade.SemanticScopeTurns (kept as an alias of this)
// and server.relaySemanticScopeTurns (deleted; the relay delegates
// here directly).
const SemanticWindowTurns = 3

// SemanticMaxTurns is the maximum conversation length the semantic
// cache serves at all (M6.7c): a conversation longer than this
// bypasses semantic caching entirely — no lookup, no store. It is
// the reference router's conversationHistoryThreshold (default 50).
const SemanticMaxTurns = 50

// SemanticMessageText is the visible text of one message: Content,
// or its text parts joined with a space when sent as an array.
// "" when the message carries no text.
func SemanticMessageText(m Message) string {
	if m.Content != "" {
		return m.Content
	}
	var sb strings.Builder
	for _, p := range m.Parts {
		if p.Type == "text" && p.Text != "" {
			sb.WriteString(p.Text)
			sb.WriteString(" ")
		}
	}
	return strings.TrimSpace(sb.String())
}

// SemanticWindowText is the text the semantic cache matches on: the
// last SemanticWindowTurns turns excluding system/developer messages,
// each rendered "role: text" (a missing role reads as "user", as in
// the reference router's normalizer) and joined with "\n", in conversation
// order. Messages inside the window with no text contribute no line
// but still occupy their window slot. "" when none of the last 3
// turns carries text.
//
// Lookup AND store both call this one function, so the window is
// identical at store and lookup by construction: a stored entry can
// only ever be found by a lookup whose last 3 turns agree with the
// stored ones, and history older than the window can never break (or
// make) a match.
//
// The system/developer exclusion is unconditional (there is no
// per-operator toggle yet): a system line is per-client boilerplate,
// not the question being asked, and with the hashed bag-of-words
// vectors a short system line ("Be brief.") is a large share of a
// short prompt's tokens, so including it would break same-question
// matches whenever the caller's system prompt differs. Tool messages
// never reach here — cacheable() rejects any request carrying one —
// so the window never spans a tool call.
func SemanticWindowText(msgs []Message) string {
	kept := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			continue
		}
		kept = append(kept, m)
	}
	if len(kept) > SemanticWindowTurns {
		kept = kept[len(kept)-SemanticWindowTurns:]
	}
	var lines []string
	for _, m := range kept {
		if text := SemanticMessageText(m); text != "" {
			role := m.Role
			if role == "" {
				role = "user"
			}
			lines = append(lines, role+": "+text)
		}
	}
	return strings.Join(lines, "\n")
}

// SemanticTurnsExceeded reports whether a conversation is too long
// for the semantic cache: more than SemanticMaxTurns messages. Both
// the engine and the relay skip the semantic layer — lookup AND
// store — when this is true, so a long conversation never gets a
// semantic hit and never stores a semantic entry. The exact layer
// is unaffected: its key is the messages as sent, so a long
// conversation can still hit exactly.
//
// CAP SEMANTICS (verified against the the reference router reference clone,
// study only, the local reference clone):
//
//   - WHAT IS COUNTED: the RAW request messages — every message in
//     the request, system/developer included, before any exclusion.
//     the reference router's guard is `conv.length > threshold` where conv is
//     `params.body.messages ?? params.body.input`, the unfiltered
//     array (services/cache/semanticCacheManager.ts:364; the conv
//     assignment is at :313); the system exclusion and the 3-turn
//     window are applied only afterwards, in
//     normalizeConversationForEmbedding. So a conversation of 1
//     system + 50 user/assistant messages (51 raw) bypasses, while
//     1 system + 49 (50 raw) does not.
//   - WHERE THE BYPASS APPLIES IN THE REFERENCE ROUTER: LOOKUP ONLY, and only
//     the semantic layer. The guard sits in the semantic lookup
//     (semanticCacheManager.ts:364), AFTER the exact-hash layer
//     (same file, ~line 330), so a long conversation still hits
//     exactly; the store path (same file, ~line 433, and
//     handlers/chatCore/semanticCacheStore.ts) has NO threshold
//     check and stores long conversations. This router DELIBERATELY
//     goes further than the reference router on the store side (per the M6.7c
//     brief): a >50-turn conversation never stores a semantic entry
//     either, so no entry can exist that a later lookup would have
//     to bypass around. The threshold itself and the lookup
//     behaviour match the reference router exactly.
//   - BOUNDARY: strictly greater than — 50 turns are served, 51
//     bypass (config/semanticCacheConfig.ts:45,73:
//     conversationHistoryThreshold: 50, "Maximum message turns in
//     conversation allowed before bypassing caching").
func SemanticTurnsExceeded(msgs []Message) bool {
	return len(msgs) > SemanticMaxTurns
}
