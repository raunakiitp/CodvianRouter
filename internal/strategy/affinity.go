// Package strategy — affinity / stickiness orderings (M3).
//
// This file holds the four pure affinity orderings. They take all of their
// inputs as arguments and never read the network, disk, or provider state,
// so the coordinator can wire them into the registry and persistence later
// without changing their semantics.
//
// OmniRoute semantics studied (read-only reference clone
// ~/workspace/omniroute-reference; no code copied):
//
//   - lkgp: services/combo/applyStrategyOrdering.ts orderByLkgp (lines
//     68-122) + src/lib/db/settings/lkgp.ts (LKGPRecord {provider,
//     connectionId?}, persisted per combo name+id). The last-known-good
//     target is moved to the front; the rest keep their order; a missing
//     record, a record that matches nothing, or a lookup failure all
//     leave the input order untouched. Match precedence: provider +
//     connectionId, then provider alone, then "provider/" model prefix.
//     OmniRoute persists the record in SQLite; here the store is
//     in-memory and mutex-guarded, and the coordinator owns persistence.
//   - context-optimized: services/combo/comboStructure.ts
//     sortTargetsByContextSize (lines 1050-1076) over sortModelsByContextSize
//     (lines 463-469): largest context window first, unknown window
//     treated as 0, and if NO target has a known window the input order
//     is returned unchanged.
//   - cache-optimized: services/combo/applyStrategyOrdering.ts
//     orderByCacheAffinity (lines 206-217) over
//     promptCacheAffinity.ts applyPromptCacheAffinity (lines 332+):
//     targets are ranked by a cache-affinity score for the current prompt
//     prefix, highest first. OmniRoute computes that score itself from
//     the prompt prefix; this function takes the per-target warmth
//     (0..1) as input and never invents it.
//   - context-relay: services/combo/strategyRegistry.ts:143
//     ("context-relay": { ordering: "declared", contextRelay: true } —
//     ordering stays declared, relay is orthogonal) +
//     executeTargetAttempt.ts:755-793 (after a successful turn on a
//     handoff provider, quota use in [0.85, 0.95) triggers an async
//     handoff) + services/contextHandoff.ts maybeGenerateHandoff (line
//     474), buildHandoffSystemMessage (line 516), injectHandoffIntoBody
//     (line 563), resolveContextRelayConfig (line 174: default threshold
//     0.85, exhaustion 0.95, providers ["codex"], TTL 5h, max 30
//     messages). The handoff itself is a summariser-model call producing
//     {summary, keyDecisions, taskProgress, activeEntities}; that call
//     lives with the caller — this file only carries the relay state
//     across attempts and builds the next attempt's context from it.
//     Injection mirrors OmniRoute: chat/completions gets a leading
//     system message, Responses-API bodies get the block prepended to
//     instructions, Claude-format bodies get the block appended to the
//     top-level system array with the existing prompt preserved first.

package strategy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// AffinityTarget is the minimal identity of one chain target: enough to match an
// LKGP record and to order by affinity. The coordinator maps
// router.Attempt onto AffinityTarget; this package never imports the engine.
type AffinityTarget struct {
	Provider     string
	Model        string
	ConnectionID string
}

// ---------------------------------------------------------------------------
// lkgp — last-known-good-provider
// ---------------------------------------------------------------------------

// LKGPRecord is one recorded success: the (provider, model) — and, when
// known, the account (connection) — that last succeeded for a chain key.
type LKGPRecord struct {
	Provider     string
	Model        string
	ConnectionID string
	UpdatedAt    time.Time
}

// LKGPStore is an in-memory, mutex-guarded record of the last successful
// (provider, model) per chain key (chain key = combo/chain name; OmniRoute
// keys by combo name + combo id, see lkgp.ts). The zero value is not
// usable; use NewLKGPStore. Persistence is the coordinator's job.
type LKGPStore struct {
	mu      sync.RWMutex
	byChain map[string]LKGPRecord
}

// NewLKGPStore returns an empty LKGP store.
func NewLKGPStore() *LKGPStore {
	return &LKGPStore{byChain: make(map[string]LKGPRecord)}
}

// RecordSuccess records provider/model (and connection, when known) as the
// last-known-good target for chainKey. Calls with an empty chainKey or an
// empty provider are ignored: there is nothing meaningful to stick to.
func (s *LKGPStore) RecordSuccess(chainKey, provider, model, connectionID string) {
	if chainKey == "" || provider == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byChain[chainKey] = LKGPRecord{
		Provider:     provider,
		Model:        model,
		ConnectionID: connectionID,
		UpdatedAt:    time.Now(),
	}
}

// LKGPRecordTTL is how long a last-known-good pin stays sticky
// (G19 Area 1, gap-track RT-2): 15 minutes. A pin older than
// this is stale — the provider that was good a day ago says
// nothing about the provider that is good now — so Lookup
// reports it missing, Latest skips it, and Load drops it on
// restore (a stale pin must not survive a restart). A record
// with a zero UpdatedAt (hand-built, or written before
// timestamping existed) cannot prove its age and is treated as
// fresh — only a stamped record can expire. The TTL is ours,
// not OmniRoute's: their LKGP persists per combo with no expiry
// named in lkgp.ts — expiring pins is a deliberate divergence,
// because a pin that never expires is a pin that can stick
// routing to a provider that died hours ago.
const LKGPRecordTTL = 15 * time.Minute

// lkgpRecordFresh reports whether rec is still within its TTL
// at now. Zero UpdatedAt is fresh (see LKGPRecordTTL).
func lkgpRecordFresh(rec LKGPRecord, now time.Time) bool {
	if rec.UpdatedAt.IsZero() {
		return true
	}
	return !now.After(rec.UpdatedAt.Add(LKGPRecordTTL))
}

// Lookup returns the recorded LKGP entry for chainKey, if any.
// An expired pin (older than LKGPRecordTTL) reports missing:
// stickiness ends when the evidence goes stale, it is never
// served from an old record.
func (s *LKGPStore) Lookup(chainKey string) (LKGPRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.byChain[chainKey]
	if !ok || !lkgpRecordFresh(rec, time.Now()) {
		return LKGPRecord{}, false
	}
	return rec, true
}

// Clear forgets the recorded LKGP entry for chainKey.
func (s *LKGPStore) Clear(chainKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byChain, chainKey)
}

// ClearIfProvider forgets the recorded LKGP entry for chainKey,
// but only when the recorded provider is the one named: a failure
// against provider B must never drop provider A's pin. It reports
// whether a record was cleared. (RT-2 failure-clear: the router
// engine calls this — via the LKGPInvalidator hook — after a hard
// failure against the pinned provider, so the next request stops
// hoisting a dead target until the 15-minute TTL would have
// expired it anyway.)
func (s *LKGPStore) ClearIfProvider(chainKey, provider string) bool {
	if chainKey == "" || provider == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byChain[chainKey]
	if !ok || rec.Provider != provider {
		return false
	}
	delete(s.byChain, chainKey)
	return true
}

// ---------------------------------------------------------------------------
// LKGP persistence (M3 Phase C)
// ---------------------------------------------------------------------------

// lkgpFile is the on-disk form of an LKGPStore: chain key →
// last-good record. The store holds provider/model names, not
// secrets, but the file is still written mode 0600.
type lkgpFile struct {
	Records map[string]LKGPRecord `json:"records"`
}

// Save writes the store as JSON to path atomically: the bytes go to a
// temp file in the same directory, which is then renamed over path,
// so a crash mid-write never leaves a half-written file behind.
// The file is created mode 0600. Save does no I/O beyond this one
// write; callers invoke it explicitly (server lifecycle), never per
// request.
func (s *LKGPStore) Save(path string) error {
	if s == nil {
		return errors.New("strategy: Save on nil LKGPStore")
	}
	s.mu.RLock()
	data, err := json.MarshalIndent(lkgpFile{Records: s.byChain}, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".lkgp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Load restores the store from the JSON file written by Save. A
// missing or corrupt file is tolerated: the store is reset to empty
// (no records are invented) and the error is returned to the caller,
// which decides whether to proceed with an empty store. Expired
// records (older than LKGPRecordTTL) are dropped on restore — a
// stale pin must not survive a restart (G19 Area 1, RT-2).
// Load never logs and never panics.
func (s *LKGPStore) Load(path string) error {
	if s == nil {
		return errors.New("strategy: Load on nil LKGPStore")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		s.reset()
		return err
	}
	var f lkgpFile
	if err := json.Unmarshal(data, &f); err != nil {
		s.reset()
		return err
	}
	if f.Records == nil {
		f.Records = make(map[string]LKGPRecord)
	}
	now := time.Now()
	for key, rec := range f.Records {
		if !lkgpRecordFresh(rec, now) {
			delete(f.Records, key)
		}
	}
	s.mu.Lock()
	s.byChain = f.Records
	s.mu.Unlock()
	return nil
}

// reset empties the store (used when a Load fails, so the store
// starts empty rather than keeping stale records).
func (s *LKGPStore) reset() {
	s.mu.Lock()
	s.byChain = make(map[string]LKGPRecord)
	s.mu.Unlock()
}

// OrderByLKGP returns targets with the last-known-good target for chainKey
// first (sticky), the rest in input order. With no record, or a record
// that matches nothing, the input order is returned unchanged. The result
// is a new slice and never contains a target that was not in the input.
// The input slice is not mutated.
func (s *LKGPStore) OrderByLKGP(chainKey string, targets []AffinityTarget) []AffinityTarget {
	out := append([]AffinityTarget(nil), targets...)
	if len(out) == 0 {
		return out
	}
	rec, ok := s.Lookup(chainKey)
	if !ok {
		return out
	}
	idx := lkgpIndex(rec, out)
	if idx <= 0 {
		// -1: no match; 0: already first. Either way, order is unchanged.
		return out
	}
	lkgp := out[idx]
	copy(out[1:idx+1], out[0:idx])
	out[0] = lkgp
	return out
}

// lkgpIndex finds the LKGP target in targets, mirroring OmniRoute's match
// precedence: (1) provider + connectionId, (2) provider + exact model,
// (3) provider alone, (4) model string starting with "provider/".
// Returns -1 when nothing matches.
func lkgpIndex(rec LKGPRecord, targets []AffinityTarget) int {
	if rec.ConnectionID != "" {
		for i, t := range targets {
			if t.Provider == rec.Provider && t.ConnectionID == rec.ConnectionID {
				return i
			}
		}
	}
	if rec.Model != "" {
		for i, t := range targets {
			if t.Provider == rec.Provider && t.Model == rec.Model {
				return i
			}
		}
	}
	for i, t := range targets {
		if t.Provider == rec.Provider {
			return i
		}
	}
	if rec.Provider != "" {
		prefix := rec.Provider + "/"
		for i, t := range targets {
			if strings.HasPrefix(t.Model, prefix) {
				return i
			}
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// context-optimized — largest context window first
// ---------------------------------------------------------------------------

// ContextTarget is one chain target plus its context-window size in
// tokens. A window of 0 means "unknown" (OmniRoute's `?? 0`).
type ContextTarget struct {
	AffinityTarget
	ContextWindow int
}

// OrderByContextOptimized returns targets ordered by largest context
// window first. Ties keep input order (stable sort). Unknown windows (<=0)
// sort after every known window. When NO target has a known window the
// input order is returned unchanged, mirroring OmniRoute's
// sortTargetsByContextSize early return. The input slice is not mutated.
func OrderByContextOptimized(targets []ContextTarget) []ContextTarget {
	out := append([]ContextTarget(nil), targets...)
	anyKnown := false
	for _, t := range out {
		if t.ContextWindow > 0 {
			anyKnown = true
			break
		}
	}
	if !anyKnown {
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ContextWindow > out[j].ContextWindow
	})
	return out
}

// ---------------------------------------------------------------------------
// cache-optimized — highest cache warmth first
// ---------------------------------------------------------------------------

// CacheTarget is one chain target plus its cache-warmth score (0..1) for
// the current prompt prefix, as measured by the caller (e.g. from the
// prompt-cache layer). This function never invents warmth values.
type CacheTarget struct {
	AffinityTarget
	Warmth float64
}

// OrderByCacheOptimized returns targets ordered by cache warmth, highest
// first. Ties keep input order (stable sort). Warmth outside 0..1 is
// clamped to the range rather than rejected. The input slice is not
// mutated.
func OrderByCacheOptimized(targets []CacheTarget) []CacheTarget {
	out := append([]CacheTarget(nil), targets...)
	clamp := func(w float64) float64 {
		switch {
		case w < 0:
			return 0
		case w > 1:
			return 1
		default:
			return w
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return clamp(out[i].Warmth) > clamp(out[j].Warmth)
	})
	return out
}

// ---------------------------------------------------------------------------
// context-relay — carry context from a failed/exhausted attempt to the next
// ---------------------------------------------------------------------------

// Relay thresholds and defaults, matching OmniRoute's
// resolveContextRelayConfig / contextHandoff.ts constants.
const (
	// DefaultHandoffThreshold is the quota-use fraction at which a
	// successful turn on a handoff provider starts relaying context
	// (HANDOFF_WARNING_THRESHOLD = 0.85).
	DefaultHandoffThreshold = 0.85
	// DefaultHandoffExhaustionThreshold is the quota-use fraction at or
	// above which relaying is pointless — the quota is effectively gone
	// (HANDOFF_EXHAUSTION_THRESHOLD = 0.95).
	DefaultHandoffExhaustionThreshold = 0.95
	// DefaultHandoffTTL is how long a generated handoff stays usable
	// (DEFAULT_TTL_MS = 5h).
	DefaultHandoffTTL = 5 * time.Hour
	// DefaultHandoffMaxMessages is the cap on source messages fed to the
	// summariser (DEFAULT_MAX_MESSAGES_FOR_SUMMARY = 30).
	DefaultHandoffMaxMessages = 30
)

// DefaultHandoffProviders is OmniRoute's default handoff-provider list:
// context relay only ever fires for these providers unless the operator
// configures others.
func DefaultHandoffProviders() []string { return []string{"codex"} }

// RelayMessage is one message of the request being relayed.
type RelayMessage struct {
	Role    string
	Content string
}

// AttemptOutcome is what one attempt hands to the relay: whether it
// succeeded, how much quota the account has used, and the source messages
// a handoff summary would be built from.
type AttemptOutcome struct {
	Provider     string
	Model        string
	ConnectionID string
	Success      bool
	// PercentUsed is the account's quota use as a fraction in [0, 1].
	PercentUsed float64
	// QuotaResetAt, when known, is the earliest quota-reset time; the
	// handoff expires then at the latest.
	QuotaResetAt time.Time
	Messages     []RelayMessage
}

// HandoffSummary is the summarised context that moves from a spent
// attempt to the next target. It is produced by a summariser-model call
// owned by the caller (OmniRoute: generateHandoffAsync with
// HANDOFF_PROMPT_TEMPLATE) — this package never fabricates one.
type HandoffSummary struct {
	Summary        string
	KeyDecisions   []string
	TaskProgress   string
	ActiveEntities []string
	MessageCount   int
	Model          string
}

// RelayFormat selects how a handoff is injected into the next attempt's
// request body (mirrors injectHandoffIntoBody).
type RelayFormat int

const (
	// FormatChat is a chat/completions body: the handoff becomes a
	// leading system message.
	FormatChat RelayFormat = iota
	// FormatResponses is a Responses-API body (input/instructions): the
	// handoff is prepended to instructions.
	FormatResponses
	// FormatClaude is a Claude-native body: the handoff is appended to
	// the top-level system array, after any existing system prompt.
	FormatClaude
)

// AttemptContext is the next attempt's request context after relay.
type AttemptContext struct {
	// Messages is the request's message list (chat format: handoff
	// system message first, then the original messages).
	Messages []RelayMessage
	// Instructions is the Responses-API instructions text (handoff
	// prepended when applied).
	Instructions string
	// SystemBlocks is the Claude-format top-level system array
	// (existing blocks first, handoff appended when applied).
	SystemBlocks []string
	// HandoffApplied reports whether a handoff was injected.
	HandoffApplied bool
}

// RelayState is the context-relay state carried across attempts in one
// request: which session/chain is being relayed, what the previous
// attempt did, and the handoff (if one has been generated) to inject
// into the next attempt. It is not safe for concurrent use; one request
// owns one RelayState.
type RelayState struct {
	SessionID string
	ComboName string

	// HandoffProviders is the allowlist of providers relay may fire for.
	// Empty means DefaultHandoffProviders.
	HandoffProviders []string
	// HandoffThreshold is the quota-use fraction that starts relaying;
	// 0 means DefaultHandoffThreshold.
	HandoffThreshold float64
	// ExhaustionThreshold is the quota-use fraction at/above which
	// relaying stops; 0 means DefaultHandoffExhaustionThreshold.
	ExhaustionThreshold float64
	// TTL is how long a handoff stays usable; 0 means DefaultHandoffTTL.
	TTL time.Duration
	// MaxMessages caps the source messages kept for summarisation;
	// 0 means DefaultHandoffMaxMessages.
	MaxMessages int
	// RelayOnError opts into the universal-handoff "on-error" trigger:
	// a failed attempt also requests a handoff. Off by default — the
	// strategy-relay trigger is a successful turn approaching quota.
	RelayOnError bool

	// FromProvider/FromModel/FromConnectionID name the attempt the
	// relay state came from. Attempts counts recorded attempts.
	FromProvider     string
	FromModel        string
	FromConnectionID string
	Attempts         int

	// Messages is the original request's messages, carried so the next
	// attempt can rebuild its context.
	Messages []RelayMessage

	handoff   *HandoffSummary
	expiresAt time.Time
}

// NewRelayState returns a RelayState for one request's session and chain.
func NewRelayState(sessionID, comboName string) *RelayState {
	return &RelayState{SessionID: sessionID, ComboName: comboName}
}

func (r *RelayState) effectiveProviders() []string {
	if len(r.HandoffProviders) > 0 {
		return r.HandoffProviders
	}
	return DefaultHandoffProviders()
}

func (r *RelayState) effectiveThreshold() float64 {
	if r.HandoffThreshold > 0 {
		return r.HandoffThreshold
	}
	return DefaultHandoffThreshold
}

func (r *RelayState) effectiveExhaustion() float64 {
	if r.ExhaustionThreshold > 0 {
		return r.ExhaustionThreshold
	}
	return DefaultHandoffExhaustionThreshold
}

func (r *RelayState) effectiveTTL() time.Duration {
	if r.TTL > 0 {
		return r.TTL
	}
	return DefaultHandoffTTL
}

func (r *RelayState) effectiveMaxMessages() int {
	if r.MaxMessages > 0 {
		return r.MaxMessages
	}
	return DefaultHandoffMaxMessages
}

// RecordAttempt records one attempt's outcome and reports whether a
// handoff should now be generated (by the caller's summariser call).
// The gates mirror OmniRoute's maybeGenerateHandoff: a session must exist,
// the provider must be allowlisted, no handoff may already be active, and
// the attempt must be a success with quota use in
// [threshold, exhaustion). With RelayOnError set, a failed attempt on an
// allowlisted provider also requests a handoff (the universal-handoff
// "on-error" trigger). RecordAttempt never generates the summary itself.
func (r *RelayState) RecordAttempt(outcome AttemptOutcome) bool {
	r.Attempts++
	r.FromProvider = outcome.Provider
	r.FromModel = outcome.Model
	r.FromConnectionID = outcome.ConnectionID
	if len(outcome.Messages) > 0 {
		msgs := append([]RelayMessage(nil), outcome.Messages...)
		if max := r.effectiveMaxMessages(); len(msgs) > max {
			msgs = msgs[len(msgs)-max:]
		}
		r.Messages = msgs
	}
	if r.SessionID == "" {
		return false
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
	if !outcome.Success && !r.RelayOnError {
		return false
	}
	if outcome.Success {
		if outcome.PercentUsed < r.effectiveThreshold() ||
			outcome.PercentUsed >= r.effectiveExhaustion() {
			return false
		}
	}
	return true
}

// SetHandoff stores a caller-generated handoff summary. expiresAt is the
// handoff's expiry; a zero expiresAt means now + TTL, and an explicit
// expiry earlier than that (e.g. a quota reset time) wins. now is the
// current time, passed in so tests can control expiry.
func (r *RelayState) SetHandoff(summary HandoffSummary, expiresAt, now time.Time) {
	cp := summary
	r.handoff = &cp
	switch {
	case !expiresAt.IsZero():
		r.expiresAt = expiresAt
	default:
		r.expiresAt = now.Add(r.effectiveTTL())
	}
}

// HasHandoff reports whether an active (unexpired) handoff is held at now.
func (r *RelayState) HasHandoff(now time.Time) bool {
	return r.handoff != nil && !r.expiresAt.IsZero() && now.Before(r.expiresAt)
}

// BuildHandoffText renders a handoff summary as the <context_handoff>
// block OmniRoute injects (buildHandoffSystemMessage), with XML escaping.
func BuildHandoffText(s HandoffSummary) string {
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
	b.WriteString("<session_summary>" + esc.Replace(s.Summary) + "</session_summary>\n")
	b.WriteString("<task_progress>" + esc.Replace(s.TaskProgress) + "</task_progress>\n")
	b.WriteString("<key_decisions>\n" + strings.Join(decisions, "\n") + "\n</key_decisions>\n")
	b.WriteString("<active_context>" + esc.Replace(strings.Join(s.ActiveEntities, ", ")) + "</active_context>\n")
	b.WriteString("<messages_processed>" + itoa(s.MessageCount) + "</messages_processed>\n")
	b.WriteString("</context_handoff>\n\n")
	b.WriteString("You are continuing a conversation that was transferred from another account due to quota limits.\n")
	b.WriteString("The context above contains a concise summary of the prior work. Continue seamlessly from where the session left off.")
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// BuildNextAttemptContext builds the next attempt's request context from
// the relay state at time now. With no active handoff it returns the
// original context unchanged. With an active handoff it injects the
// handoff block per format: leading system message (FormatChat),
// prepended to instructions (FormatResponses), or appended to the
// top-level system array after existing blocks (FormatClaude).
// existingInstructions and existingSystem are the next attempt's own
// values; they are preserved, never dropped.
func (r *RelayState) BuildNextAttemptContext(now time.Time, format RelayFormat, existingInstructions string, existingSystem []string) AttemptContext {
	ctx := AttemptContext{
		Messages:     append([]RelayMessage(nil), r.Messages...),
		Instructions: existingInstructions,
		SystemBlocks: append([]string(nil), existingSystem...),
	}
	if !r.HasHandoff(now) {
		return ctx
	}
	text := BuildHandoffText(*r.handoff)
	ctx.HandoffApplied = true
	switch format {
	case FormatResponses:
		if strings.TrimSpace(ctx.Instructions) == "" {
			ctx.Instructions = text
		} else {
			ctx.Instructions = text + "\n\n" + ctx.Instructions
		}
	case FormatClaude:
		ctx.SystemBlocks = append(ctx.SystemBlocks, text)
	default: // FormatChat
		handoffMsg := RelayMessage{Role: "system", Content: text}
		ctx.Messages = append([]RelayMessage{handoffMsg}, ctx.Messages...)
	}
	return ctx
}

// maxRelayHistoryTokens caps the formatted history fed to the
// summariser (OmniRoute MAX_HISTORY_TOKENS_FOR_SUMMARY = 8000): the
// selection trims oldest non-system messages until the history fits.
const maxRelayHistoryTokens = 8000

// relayEstimateTokens estimates the token count of text for the
// history budget: ~4 chars per token, the same rough heuristic
// OmniRoute's estimateTokens uses. It is a budget guard, not a
// billing count — the exact number only decides which old messages
// are trimmed.
func relayEstimateTokens(text string) int {
	return len(text) / 4
}

// formatRelayMessagesForEstimate renders messages the way the
// summariser prompt does ("[n] ROLE:\ncontent"), so the budget
// measures what the summariser actually receives. Empty messages
// contribute nothing to the estimate (as in OmniRoute's
// formatMessagesForPrompt).
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
		b.WriteString("[" + itoa(i+1) + "] " + strings.ToUpper(role) + ":\n" + content + "\n\n")
	}
	return b.String()
}

// SelectMessagesForSummary chooses the messages a handoff summary is
// built from, mirroring OmniRoute's selectMessagesForSummary
// (contextHandoff.ts, "standard" relay mode): system/developer
// messages are always pinned (they carry the task), then the most
// recent non-system messages up to maxMessages; when the formatted
// history exceeds maxRelayHistoryTokens the oldest non-system
// messages are dropped until it fits. At least the system messages
// (or, with none, the single most recent message) are always kept,
// so selection never returns an empty list for a non-empty input.
// maxMessages <= 0 means DefaultHandoffMaxMessages. The returned
// slice is a fresh copy; the input is never mutated.
func SelectMessagesForSummary(messages []RelayMessage, maxMessages int) []RelayMessage {
	if len(messages) == 0 {
		return nil
	}
	if maxMessages <= 0 {
		maxMessages = DefaultHandoffMaxMessages
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
	// Trim oldest non-system messages until the history fits the
	// budget; the system messages plus one non-system message are
	// the floor (OmniRoute's minWorkingLength).
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
