// Phase D, builder D5: context-relay engine integration.
//
// Why this file exists instead of calling internal/strategy: the
// router package CANNOT import internal/strategy — that would be an
// import cycle (strategy/wiring.go imports router, and router.go's
// Config comment says so explicitly). D4's precedent (cascade.go)
// applies: cross-package data arrives through Config funcs and the
// engine runs its own local mirror of the logic. This file is the
// context-relay mirror of strategy.RelayState (internal/strategy/
// affinity.go): same thresholds, same gating, same <context_handoff>
// text, driven by the engine's attempt loop instead of the strategy
// package's state.
//
// What the mirror covers:
//   - Per-request relay state: attempt outcomes recorded in order,
//     handoff wanted/built/active, expiry, message cap.
//   - The trigger gates of strategy.RecordAttempt: provider
//     allowlist (default ["codex"]), success with quota use in
//     [threshold, exhaustion), and — when RelayOnError is set —
//     failure on an allowlisted provider.
//   - The handoff text (strategy.BuildHandoffText) and the chat
//     injection (leading system message, original messages
//     preserved — strategy.FormatChat).
//   - The summariser seam: RelaySummariser (Config) is the
//     caller-owned summariser call. With none configured — or when
//     it errors — the handoff degrades honestly to the raw attempt
//     transcript, labelled unsummarised, never a faked summary.
//   - Message selection for the summariser: system/developer
//     messages pinned, recent non-system messages up to the cap,
//     oldest trimmed until the history fits the 8000-token budget
//     (strategy.SelectMessagesForSummary, mirrored here as
//     selectRelayMessages — same thresholds, same fallback).
//
// What the mirror does NOT cover (honest divergences from the
// strategy primitive — see also the PARITY row):
//   - No session gate: strategy.RecordAttempt requires a non-empty
//     SessionID. The engine has no session concept of its own —
//     the state is request-scoped by construction, created fresh
//     per request — so the gate is vacuous for the in-request
//     relay. Cross-request relay (a handoff surviving into the
//     NEXT request of a session, which is how the sector-typical
//     success-triggered handoff is consumed) runs on the
//     engine-owned session store below (relaySessions, keyed by
//     the gateway key the request arrived under —
//     req.CallerID): a request that ends with a handoff wanted
//     but no next attempt stores the WANT (source messages +
//     expiry, never a built handoff); the next context-relay
//     request under the same key preloads it, and the handoff is
//     built lazily by that request's own relayAttemptReq — so no
//     summariser call is ever spent on a request that already
//     ended, and a want whose next request never comes costs
//     nothing. Within one request a success is terminal, so a
//     success-triggered handoff has no next attempt to carry it
//     to — the store is what carries it instead. The state
//     records the outcome (visible via LastRelayStatus) and the
//     summariser is only ever called lazily, when another attempt
//     is actually about to be dialled.
//   - Chat format only: strategy.BuildNextAttemptContext also
//     covers FormatResponses (prepend to instructions) and
//     FormatClaude (append to the system array). The engine's
//     attempt path is chat-shaped (provider.ChatRequest carries
//     Messages); the engine never builds a Responses or
//     Claude-native body on this path, so only the leading-system-
//     message injection is mirrored.
//   - Threshold/TTL/max-messages config: the trigger window
//     ([RelayHandoffThreshold, RelayExhaustionThreshold)),
//     the handoff TTL and the summariser message cap are
//     operator-configurable through Config (routing.* in the
//     config file, B2-01 wiring); unset means the sector-typical
//     defaults (0.85 / 0.95 / 5h / 30 messages). The
//     allowlist (Config.RelayHandoffProviders) and the
//     on-error opt-in (Config.RelayOnError) were already
//     configurable.
//   - Quota signal in production: the success trigger needs the
//     account's quota-use fraction, which arrives through
//     Config.RelayQuotaUsedFor. The caller (cmd/codvian-router
//     main.go) supplies it from the engine's learned quota hint
//     (LearnedQuotaHint — measured from real upstream
//     rate-limit headers, the same source :495/:497 wired into
//     signalsFn), converted to the used fraction; an unknown
//     signal is recorded as unknown, never invented. Before
//     that wiring a success alone never triggered a handoff in
//     production; the live trigger was the on-error one
//     (RelayOnError).
//   - No explicit-expiry support: strategy.SetHandoff lets an
//     explicit expiry (e.g. a quota reset time) win over now+TTL.
//     The mirror's setHandoff always uses now+TTL. Moot within
//     one request (5h TTL, request-scoped state), but the
//     divergence is real if the state ever outlives a request.
//   - Injection source: strategy.BuildNextAttemptContext
//     (FormatChat) injects into r.Messages — the capped last-30
//     copy. The mirror's attemptMessages injects into the full
//     original message list instead. Direction is safer (no
//     message loss), but it is a real divergence from the
//     primitive's cap.
//   - Connection identity: strategy carries a ConnectionID (the
//     account/connection the attempt ran on). The engine has no
//     per-account connection concept — Attempt carries ProviderID,
//     Model and KeyID only — so the mirror records no connection
//     id rather than mapping KeyID onto it and pretending it is
//     one.
package router

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// contextRelayStrategy is the effective-strategy name that selects
// context-relay in the attempt loops.
const contextRelayStrategy = "context-relay"

// Relay thresholds and defaults, mirroring strategy.RelayState's
// (affinity.go) — which mirror the sector-typical resolveContextRelayConfig
// / contextHandoff.ts constants.
const (
	// defaultRelayHandoffThreshold is the quota-use fraction at
	// which a successful turn on a handoff provider starts
	// relaying context (HANDOFF_WARNING_THRESHOLD = 0.85).
	defaultRelayHandoffThreshold = 0.85
	// defaultRelayExhaustionThreshold is the quota-use fraction
	// at or above which relaying is pointless — the quota is
	// effectively gone (HANDOFF_EXHAUSTION_THRESHOLD = 0.95).
	defaultRelayExhaustionThreshold = 0.95
	// defaultRelayTTL is how long a generated handoff stays
	// usable (DEFAULT_TTL_MS = 5h).
	defaultRelayTTL = 5 * time.Hour
	// defaultRelayMaxMessages is the cap on source messages fed
	// to the summariser (DEFAULT_MAX_MESSAGES_FOR_SUMMARY = 30).
	defaultRelayMaxMessages = 30
	// maxDegradedTranscriptChars caps the raw transcript carried
	// by a degraded (unsummarised) handoff, so a missing
	// summariser can never blow the next attempt's context window
	// with an unbounded verbatim paste. 4000 chars is a local
	// safety bound, not an the sector baseline constant.
	maxDegradedTranscriptChars = 4000
)

// defaultRelayHandoffProviders mirrors strategy.DefaultHandoffProviders:
// context relay only ever fires for these providers unless the
// operator configures others.
func defaultRelayHandoffProviders() []string { return []string{"codex"} }

// RelayMessage is one message of the request being relayed.
// Mirrors strategy.RelayMessage.
type RelayMessage struct {
	Role    string
	Content string
}

// RelayHandoff is the summarised context that moves from a spent
// attempt to the next target. Mirrors strategy.HandoffSummary, plus
// Summarised: false marks the honest-degradation form — a raw
// transcript carried because no summariser was configured (or the
// configured one errored) — so the next attempt can tell a real
// summary from a verbatim paste.
type RelayHandoff struct {
	Summary        string
	KeyDecisions   []string
	TaskProgress   string
	ActiveEntities []string
	MessageCount   int
	Model          string
	Summarised     bool
}

// RelaySummariser is the caller-owned summariser call: given the
// source messages (the original request's messages, oldest first,
// capped at the configured max, default 30), it returns the handoff
// summary — the the sector baseline generateHandoffAsync step. It is a Config
// func because the router cannot import the strategy package. The
// caller (cmd/codvian-router main.go) supplies the production one:
// the cheapest priced model with a live client, called directly,
// never a stub and never a canned summary. A nil RelaySummariser
// means "no summariser", and the engine degrades honestly (raw
// transcript) instead of faking a summary. The call runs on the
// request path, so it must be cheap and non-blocking relative to
// the attempt it precedes; an error degrades the same way as a
// missing summariser.
type RelaySummariser func(ctx context.Context, messages []RelayMessage) (RelayHandoff, error)

// relayOutcome is what one attempt hands to the relay: whether it
// succeeded, how much quota the account has used (when known), and
// the source messages a handoff summary would be built from.
// Mirrors strategy.AttemptOutcome.
type relayOutcome struct {
	Provider string
	Model    string
	Success  bool
	// PercentUsed is the account's quota use as a fraction in
	// [0, 1]; meaningful only when QuotaKnown is true. Unknown
	// is never invented — see the file header.
	PercentUsed float64
	QuotaKnown  bool
	Messages    []RelayMessage
}

// relayState is the context-relay state carried across attempts in
// one request. Mirrors strategy.RelayState (see the file header for
// the divergences). It is not safe for concurrent use by two request
// goroutines — one request owns one relayState — but it IS safe for
// LastRelayStatus to read while the owning request is still running:
// every field access goes through mu (a RWMutex), and the summariser
// call in maybeBuildHandoff runs outside the lock, so a summariser
// that calls back into LastRelayStatus cannot deadlock. The handoff
// pointer is only ever replaced, never mutated in place, so a
// rendering copied under a read lock stays valid after unlock.
type relayState struct {
	mu sync.RWMutex

	providers    []string
	relayOnError bool
	// handoffThreshold, exhaustionThreshold, ttl and
	// maxMessages are the operator-configured context-relay
	// knobs (Config.RelayHandoffThreshold etc., B2-01
	// wiring), stored raw: zero means "not configured" and
	// the accessors below resolve it to the sector-typical default,
	// so a state built as a bare literal (as the tests do)
	// behaves exactly like a configured one with defaults.
	handoffThreshold    float64
	exhaustionThreshold float64
	ttl                 time.Duration
	maxMessages         int

	attempts     int
	fromProvider string
	fromModel    string
	messages     []RelayMessage

	handoff       *RelayHandoff
	handoffWanted bool
	// wantExpiresAt is when the pending want stops being usable:
	// set when the want is first recorded (now + TTL), carried
	// across the cross-request store so a re-store never
	// refreshes the TTL, cleared when the want is consumed by
	// maybeBuildHandoff. Zero means no want is pending.
	wantExpiresAt time.Time
	expiresAt     time.Time
	// degradeReason names why the active handoff (if any) is a
	// raw transcript instead of a summary: "" when the handoff
	// is summarised (or when no handoff is active).
	degradeReason string
}

// newRelayState returns a relayState for one request, or nil when
// the effective strategy is not context-relay — other strategies
// are untouched (no state, no recording, no injection).
func newRelayState(strategy string, cfg Config) *relayState {
	if strategy != contextRelayStrategy {
		return nil
	}
	return &relayState{
		providers:           cfg.RelayHandoffProviders,
		relayOnError:        cfg.RelayOnError,
		handoffThreshold:    cfg.RelayHandoffThreshold,
		exhaustionThreshold: cfg.RelayExhaustionThreshold,
		ttl:                 cfg.RelayTTL,
		maxMessages:         cfg.RelayMaxMessages,
	}
}

// handoffThresholdValue resolves the trigger threshold: the
// configured value when it is a usable fraction, the sector-typical
// default (0.85) otherwise. Zero means "not configured".
func (r *relayState) handoffThresholdValue() float64 {
	if r.handoffThreshold > 0 && r.handoffThreshold < 1 {
		return r.handoffThreshold
	}
	return defaultRelayHandoffThreshold
}

// exhaustionThresholdValue resolves the exhaustion threshold:
// the configured value when it is a usable fraction above the
// trigger threshold, the sector-typical default (0.95) otherwise.
func (r *relayState) exhaustionThresholdValue() float64 {
	if r.exhaustionThreshold > 0 && r.exhaustionThreshold <= 1 && r.exhaustionThreshold > r.handoffThresholdValue() {
		return r.exhaustionThreshold
	}
	return defaultRelayExhaustionThreshold
}

// ttlValue resolves the handoff TTL: the configured value when
// positive, the sector-typical default (5h) otherwise.
func (r *relayState) ttlValue() time.Duration {
	if r.ttl > 0 {
		return r.ttl
	}
	return defaultRelayTTL
}

// maxMessagesValue resolves the summariser message cap: the
// configured value when positive, the sector-typical default (30)
// otherwise.
func (r *relayState) maxMessagesValue() int {
	if r.maxMessages > 0 {
		return r.maxMessages
	}
	return defaultRelayMaxMessages
}

// effectiveProviders mirrors strategy.RelayState.effectiveProviders:
// the configured allowlist, or the ["codex"] default when empty.
func (r *relayState) effectiveProviders() []string {
	if len(r.providers) > 0 {
		return r.providers
	}
	return defaultRelayHandoffProviders()
}

// recordAttempt records one attempt's outcome and reports whether a
// handoff should now be generated (by the caller's summariser call,
// made lazily by maybeBuildHandoff); a true return also records the
// want on the state, so maybeBuildHandoff finds it without the
// caller re-checking. The gates mirror
// strategy.RelayState.RecordAttempt: the provider must be
// allowlisted, no handoff may already be active, and the attempt
// must be a success with quota use in [threshold, exhaustion) —
// or, with RelayOnError set, a failed attempt on an allowlisted
// provider. A success with an unknown quota signal never triggers:
// the fraction is unknown, not zero, and never guessed. Recording
// never generates the summary itself.
func (r *relayState) recordAttempt(outcome relayOutcome) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	r.fromProvider = outcome.Provider
	r.fromModel = outcome.Model
	if len(outcome.Messages) > 0 {
		// Selection, not a blind last-30 slice: the system prompt
		// is pinned and only the most recent non-system messages
		// are kept, so a long history never drops the task before
		// the summariser sees it. The token-budget trim runs again
		// at handoff-build time; selection is idempotent.
		r.messages = selectRelayMessages(outcome.Messages, r.maxMessagesValue())
	}
	allowed := false
	for _, p := range r.effectiveProviders() {
		if strings.EqualFold(p, outcome.Provider) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	if r.handoff != nil {
		return false // a handoff is already active; do not regenerate
	}
	if !outcome.Success && !r.relayOnError {
		return false
	}
	if outcome.Success {
		if !outcome.QuotaKnown ||
			outcome.PercentUsed < r.handoffThresholdValue() ||
			outcome.PercentUsed >= r.exhaustionThresholdValue() {
			return false
		}
	}
	// The want lives on the state, not just in the return value:
	// maybeBuildHandoff consumes it lazily at the next attempt.
	// The expiry is set once, on the false→true transition, and
	// never refreshed: a want re-stored for the next request
	// keeps its original deadline (see storeRelayWantIfPending).
	if !r.handoffWanted {
		r.wantExpiresAt = time.Now().Add(r.ttlValue())
	}
	r.handoffWanted = true
	return true
}

// hasHandoff reports whether an active (unexpired) handoff is held
// at now. Mirrors strategy.RelayState.HasHandoff.
func (r *relayState) hasHandoff(now time.Time) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hasHandoffLocked(now)
}

// hasHandoffLocked is hasHandoff with mu already held.
func (r *relayState) hasHandoffLocked(now time.Time) bool {
	return r.handoff != nil && !r.expiresAt.IsZero() && now.Before(r.expiresAt)
}

// setHandoff stores a caller-generated handoff summary, expiring
// now + TTL. Mirrors strategy.RelayState.SetHandoff with a zero
// explicit expiry.
func (r *relayState) setHandoff(summary RelayHandoff, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setHandoffLocked(summary, now)
}

// setHandoffLocked is setHandoff with mu already held.
func (r *relayState) setHandoffLocked(summary RelayHandoff, now time.Time) {
	cp := summary
	r.handoff = &cp
	r.expiresAt = now.Add(r.ttlValue())
	if summary.Summarised {
		r.degradeReason = ""
	}
	// An unsummarised handoff keeps whatever degradeReason the
	// degraded-handoff builder recorded before calling setHandoff.
}

// buildHandoffText renders a handoff as the <context_handoff> block
// the next attempt receives (strategy.BuildHandoffText), with XML
// escaping. An unsummarised handoff carries an explicit transfer
// note saying so, so the next attempt never mistakes a verbatim
// transcript for a summary.
func (r *relayState) buildHandoffText() string {
	r.mu.RLock()
	handoff := r.handoff
	r.mu.RUnlock()
	if handoff == nil {
		return ""
	}
	s := *handoff // value copy: the pointer is never mutated in place
	esc := strings.NewReplacer(
		`&`, "&amp;",
		`<`, "&lt;",
		`>`, "&gt;",
		`"`, "&quot;",
		`'`, "&apos;",
	)
	decisions := make([]string, len(s.KeyDecisions))
	for i, d := range s.KeyDecisions {
		decisions[i] = "  - " + esc.Replace(d)
	}
	var b strings.Builder
	b.WriteString("<context_handoff>\n")
	b.WriteString("<transfer_reason>Account quota transfer - continuing from previous session</transfer_reason>\n")
	if !s.Summarised {
		b.WriteString("<transfer_note>Unsummarised transcript - no summariser configured; raw messages carried verbatim, not a summary.</transfer_note>\n")
	}
	b.WriteString("<session_summary>" + esc.Replace(s.Summary) + "</session_summary>\n")
	b.WriteString("<task_progress>" + esc.Replace(s.TaskProgress) + "</task_progress>\n")
	b.WriteString("<key_decisions>\n" + strings.Join(decisions, "\n") + "\n</key_decisions>\n")
	b.WriteString("<active_context>" + esc.Replace(strings.Join(s.ActiveEntities, ", ")) + "</active_context>\n")
	b.WriteString("<messages_processed>" + strconv.Itoa(s.MessageCount) + "</messages_processed>\n")
	b.WriteString("</context_handoff>\n\n")
	b.WriteString("You are continuing a conversation that was transferred from another account due to quota limits.\n")
	b.WriteString("The context above contains a concise summary of the prior work. Continue seamlessly from where the session left off.")
	return b.String()
}

// maxRelayHistoryTokens caps the formatted history fed to the
// summariser (the sector baseline MAX_HISTORY_TOKENS_FOR_SUMMARY = 8000).
// Mirrors strategy.maxRelayHistoryTokens.
const maxRelayHistoryTokens = 8000

// relayEstimateTokens estimates the token count of text for the
// history budget: ~4 chars per token, the same rough heuristic
// the sector-typical estimateTokens uses. A budget guard, not a billing
// count.
func relayEstimateTokens(text string) int {
	return len(text) / 4
}

// formatRelayMessagesForEstimate renders messages the way the
// summariser prompt does ("[n] ROLE:\ncontent"), so the budget
// measures what the summariser actually receives.
func formatRelayMessagesForEstimate(messages []RelayMessage) string {
	var b strings.Builder
	for i, m := range messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		role := m.Role
		if role == "" {
			role = "unknown"
		}
		b.WriteString("[" + strconv.Itoa(i+1) + "] " + strings.ToUpper(role) + ":\n" + content + "\n\n")
	}
	return b.String()
}

// selectRelayMessages mirrors strategy.SelectMessagesForSummary (see
// internal/strategy/affinity.go): system/developer messages pinned,
// recent non-system messages up to the cap, oldest trimmed until the
// formatted history fits the token budget. The router cannot import
// strategy (import cycle), so the logic lives here as a local mirror
// — same thresholds, same selection, same fallback. Selection is
// idempotent: selecting an already-selected set returns the same set.
func selectRelayMessages(messages []RelayMessage, maxMessages int) []RelayMessage {
	if len(messages) == 0 {
		return nil
	}
	if maxMessages <= 0 {
		maxMessages = defaultRelayMaxMessages
	}
	var system, nonSystem []RelayMessage
	for _, m := range messages {
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, m)
		} else {
			nonSystem = append(nonSystem, m)
		}
	}
	recent := nonSystem
	if len(recent) > maxMessages {
		recent = recent[len(recent)-maxMessages:]
	}
	working := append(append([]RelayMessage(nil), system...), recent...)
	minLen := len(system) + 1
	for len(working) > minLen && relayEstimateTokens(formatRelayMessagesForEstimate(working)) > maxRelayHistoryTokens {
		working = append(append([]RelayMessage(nil), system...), working[len(system)+1:]...)
	}
	if relayEstimateTokens(formatRelayMessagesForEstimate(working)) > maxRelayHistoryTokens {
		if len(system) > 0 {
			return append([]RelayMessage(nil), system...)
		}
		return []RelayMessage{nonSystem[len(nonSystem)-1]}
	}
	return working
}

// maybeBuildHandoff builds the pending handoff, if one was requested
// by recordAttempt and none is active. It is LAZY on purpose: it
// runs only when another attempt is actually about to be dialled
// (called from relayAttemptReq), so a handoff is never built for a
// request that has already ended — in particular a success-triggered
// handoff, whose request is terminal, never costs a summariser call.
// With a summariser configured its result is stored with
// Summarised forced true (the code sets it unconditionally — a
// summariser result that reaches storage is, by definition, a
// summarised handoff); with none — or when it errors — the
// handoff degrades to the raw transcript (see degradedHandoff), and the reason is recorded on the state for
// LastRelayStatus. Either way the want is consumed: a failed build
// is not retried on the next attempt.
func (r *relayState) maybeBuildHandoff(ctx context.Context, summariser RelaySummariser) {
	// Claim the want and copy the source messages under the lock,
	// then release it BEFORE calling the summariser: the call can
	// be slow, and a summariser that calls back into
	// LastRelayStatus must not deadlock on mu. The want is
	// consumed at claim time, so a second call while the first is
	// still summarising finds nothing to do.
	r.mu.Lock()
	if !r.handoffWanted || r.hasHandoffLocked(time.Now()) {
		r.mu.Unlock()
		return
	}
	r.handoffWanted = false
	r.wantExpiresAt = time.Time{}
	// Generation-time selection (as in the sector-typical
	// generateHandoffAsync): what the summariser — or the degraded
	// transcript — receives is the selected set, never the raw
	// stored window. Idempotent with the record-time selection.
	msgs := selectRelayMessages(r.messages, r.maxMessagesValue())
	r.mu.Unlock()

	now := time.Now()
	if summariser != nil {
		if summary, err := summariser(ctx, msgs); err == nil {
			summary.Summarised = true
			if summary.MessageCount <= 0 {
				summary.MessageCount = len(msgs)
			}
			r.setHandoff(summary, now)
			return
		} else {
			r.mu.Lock()
			r.degradeReason = "summariser error: " + err.Error()
			r.mu.Unlock()
		}
	} else {
		r.mu.Lock()
		r.degradeReason = "no summariser configured"
		r.mu.Unlock()
	}
	r.setHandoff(r.degradedHandoff(msgs), now)
}

// degradedHandoff builds the honest-degradation handoff from msgs:
// the raw source transcript, verbatim, truncated to
// maxDegradedTranscriptChars and labelled unsummarised (Summarised =
// false, so buildHandoffText adds the transfer note). Nothing is
// summarised, paraphrased or invented — the next attempt sees exactly
// what the user sent.
func (r *relayState) degradedHandoff(msgs []RelayMessage) RelayHandoff {
	var b strings.Builder
	total := 0
	for _, m := range msgs {
		line := m.Role + ": " + m.Content + "\n"
		if total+len(line) > maxDegradedTranscriptChars {
			remaining := maxDegradedTranscriptChars - total
			if remaining > 0 {
				b.WriteString(line[:remaining])
			}
			b.WriteString("\n[transcript truncated]")
			break
		}
		b.WriteString(line)
		total += len(line)
	}
	return RelayHandoff{
		Summary:      strings.TrimRight(b.String(), "\n"),
		MessageCount: len(msgs),
		Summarised:   false,
	}
}

// attemptMessages returns the message list the next attempt should
// be sent: the handoff as a leading system message followed by the
// original messages (strategy.FormatChat injection). With no active
// handoff the original list is returned unchanged and applied is
// false. The returned slice is a fresh copy; the caller's list is
// never mutated.
func (r *relayState) attemptMessages(original []provider.Message) ([]provider.Message, bool) {
	if !r.hasHandoff(time.Now()) {
		return original, false
	}
	out := make([]provider.Message, 0, len(original)+1)
	out = append(out, provider.Message{Role: RoleSystem, Content: r.buildHandoffText()})
	out = append(out, original...)
	return out, true
}

// RelayStatus is the observable snapshot of the most recent
// request's relay state (see LastRelayStatus). HandoffActive says a
// handoff was built and unexpired at snapshot time; Summarised
// false with HandoffActive true means the active handoff is a raw
// transcript (DegradeReason names why).
type RelayStatus struct {
	Attempts          int
	FromProvider      string
	FromModel         string
	HandoffActive     bool
	HandoffSummarised bool
	DegradeReason     string
	Messages          int
}

// relayMessagesFrom converts a request's messages to relay source
// messages (role + content only). Tool calls, reasoning state and
// parts are not carried: the summariser's source is the conversation
// text, and the handoff injection rebuilds a plain chat list.
func relayMessagesFrom(req provider.ChatRequest) []RelayMessage {
	msgs := make([]RelayMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, RelayMessage{Role: m.Role, Content: m.Content})
	}
	return msgs
}

// relaySessionEntry is one cross-request relay want held for a
// session (keyed by the gateway key the request arrived under —
// req.CallerID): the source messages the handoff must summarise,
// where the spent attempt ran, and when the want expires. The
// store holds the WANT, not a built handoff: the summary is built
// lazily by the next request's own relayAttemptReq, so no
// summariser call is ever spent on a request that already ended,
// and a want whose next request never comes costs nothing.
type relaySessionEntry struct {
	messages     []RelayMessage
	fromProvider string
	fromModel    string
	expiresAt    time.Time
}

// maxRelaySessionEntries caps the cross-request relay store: one
// pending want per gateway key, 512 keys at most. Old entries are
// swept on write (expired first, then earliest-expiring).
const maxRelaySessionEntries = 512

// storeRelayWantIfPending stores this request's pending relay want
// for the next request under the same session key — the
// cross-request half of context-relay. It is a no-op for a nil
// relay, an empty session key, a request with no want pending, or
// a request whose handoff was already built in-request: only a
// want that outlived its request crosses into the next one. The
// want keeps its original expiry (a re-store after a request that
// never dialled never refreshes the TTL). Called via defer at the
// end of the attempt loops, so every exit path stores.
func (e *Engine) storeRelayWantIfPending(sessionKey string, r *relayState) {
	if sessionKey == "" || r == nil {
		return
	}
	r.mu.RLock()
	if !r.handoffWanted || r.handoff != nil {
		r.mu.RUnlock()
		return
	}
	entry := relaySessionEntry{
		messages:     append([]RelayMessage(nil), r.messages...),
		fromProvider: r.fromProvider,
		fromModel:    r.fromModel,
		expiresAt:    r.wantExpiresAt,
	}
	r.mu.RUnlock()
	if entry.expiresAt.IsZero() {
		entry.expiresAt = time.Now().Add(r.ttlValue())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.relaySessions == nil {
		e.relaySessions = make(map[string]relaySessionEntry)
	}
	if len(e.relaySessions) >= maxRelaySessionEntries {
		now := time.Now()
		for k, v := range e.relaySessions {
			if !now.Before(v.expiresAt) {
				delete(e.relaySessions, k)
			}
		}
	}
	if len(e.relaySessions) >= maxRelaySessionEntries {
		// Still full: evict the earliest-expiring entry so the
		// newest want is never dropped for a stale one.
		var oldestKey string
		var oldest time.Time
		for k, v := range e.relaySessions {
			if oldestKey == "" || v.expiresAt.Before(oldest) {
				oldestKey, oldest = k, v.expiresAt
			}
		}
		if oldestKey != "" {
			delete(e.relaySessions, oldestKey)
		}
	}
	e.relaySessions[sessionKey] = entry
}

// loadRelayWant returns the stored pending want for a session key,
// or false when none exists or it expired (an expired entry is
// deleted on read). The entry is consumed on load: a want is
// carried into exactly one next request — a request that loads
// it but never dials re-stores it on exit (see
// storeRelayWantIfPending), so the want is never lost by a
// load alone.
func (e *Engine) loadRelayWant(sessionKey string) (relaySessionEntry, bool) {
	if sessionKey == "" {
		return relaySessionEntry{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.relaySessions[sessionKey]
	if !ok {
		return relaySessionEntry{}, false
	}
	delete(e.relaySessions, sessionKey)
	if !time.Now().Before(entry.expiresAt) {
		return relaySessionEntry{}, false
	}
	return entry, true
}

// newRelayFor creates the per-request relay state for an effective
// strategy (nil for anything but context-relay) and publishes it as
// the engine's last-relay record so tests and the dashboard can see
// what the request's relay did even after the request ends. The
// pointer is shared: later mutations by the attempt loop are
// visible through LastRelayStatus. A stored cross-request want for
// the session key is preloaded into the fresh state (source
// messages + want + original expiry), so the next request's first
// attempt builds and injects the handoff its predecessor earned.
func (e *Engine) newRelayFor(strategy, sessionKey string) *relayState {
	relay := newRelayState(strategy, e.cfg)
	if relay == nil {
		return nil
	}
	if entry, ok := e.loadRelayWant(sessionKey); ok {
		relay.mu.Lock()
		relay.messages = append([]RelayMessage(nil), entry.messages...)
		relay.fromProvider = entry.fromProvider
		relay.fromModel = entry.fromModel
		relay.handoffWanted = true
		relay.wantExpiresAt = entry.expiresAt
		relay.mu.Unlock()
	}
	e.mu.Lock()
	e.lastRelay = relay
	e.mu.Unlock()
	return relay
}

// LastRelayStatus returns the relay snapshot of the most recent
// context-relay request, or false when no context-relay request has
// run yet (or the most recent request used another strategy — the
// record is only replaced by a context-relay request, never cleared
// by other strategies, exactly like LastRouting keeps the last
// routing record).
func (e *Engine) LastRelayStatus() (RelayStatus, bool) {
	e.mu.Lock()
	relay := e.lastRelay
	e.mu.Unlock()
	if relay == nil {
		return RelayStatus{}, false
	}
	return relay.snapshot(), true
}

// snapshot returns a consistent RelayStatus under one read lock, so
// a concurrent read while the owning request is still running sees a
// coherent state, never a torn one.
func (r *relayState) snapshot() RelayStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return RelayStatus{
		Attempts:          r.attempts,
		FromProvider:      r.fromProvider,
		FromModel:         r.fromModel,
		HandoffActive:     r.hasHandoffLocked(time.Now()),
		HandoffSummarised: r.handoff != nil && r.handoff.Summarised,
		DegradeReason:     r.degradeReason,
		Messages:          len(r.messages),
	}
}

// relayNote feeds one attempt's outcome into the relay state. It is
// nil-safe (a nil relay is a no-op) so every loop path can call it
// unconditionally. The outcome's messages are the ORIGINAL request's
// messages, never an already-relayed attempt request: feeding a
// relayed request back in would nest the handoff text into the next
// summary's source. Success is res.err == nil after the caller's
// content-filter conversion (a "content_filter" finish is a refusal,
// recorded as a failure). The quota-use fraction comes from
// Config.RelayQuotaUsedFor; nil, or a false report, means unknown —
// never invented. Recording never calls the summariser: the handoff
// is built lazily by relayAttemptReq when the next attempt is about
// to be dialled.
func (e *Engine) relayNote(relay *relayState, req provider.ChatRequest, res attemptResult) {
	if relay == nil {
		return
	}
	if !res.called {
		// Never dialled (unknown provider, limiter-shed): not an
		// outcome. The codebase convention is that called=false
		// never touches the breaker either (router.go:2436), and
		// the stream loop already skips relayNote on those
		// branches — the non-stream loop must match, or a
		// never-dialled allowlisted provider could trigger a
		// handoff want and inflate the Attempts count.
		return
	}
	used, known := 0.0, false
	if e.cfg.RelayQuotaUsedFor != nil {
		used, known = e.cfg.RelayQuotaUsedFor(res.att.ProviderID, res.att.Model)
	}
	relay.recordAttempt(relayOutcome{
		Provider:    res.att.ProviderID,
		Model:       res.att.Model,
		Success:     res.err == nil,
		PercentUsed: used,
		QuotaKnown:  known,
		Messages:    relayMessagesFrom(req),
	})
}

// relayAttemptReq returns the request the next attempt should be
// dialled with: the pending handoff is built first (lazily — only
// when another attempt is actually about to be dialled, so a
// terminal success never costs a summariser call) and injected as
// a leading system message. With no relay, or no active handoff,
// the request is returned unchanged. The input request is never
// mutated: the injected list is a fresh copy.
func (e *Engine) relayAttemptReq(ctx context.Context, relay *relayState, req provider.ChatRequest) provider.ChatRequest {
	if relay == nil {
		return req
	}
	relay.maybeBuildHandoff(ctx, e.cfg.RelaySummariser)
	msgs, applied := relay.attemptMessages(req.Messages)
	if !applied {
		return req
	}
	out := req
	out.Messages = msgs
	return out
}
