// Package router implements the Codvian Router routing engine: fallback
// chains, quota-aware ordering, 429 / Retry-After handling with backoff,
// per-provider circuit breakers, hedged requests and streaming-safe
// failover via a bounded pre-commit preread buffer (no switch after
// the first content byte is sent).
//
// The engine is transport-agnostic: provider access comes through the
// provider.Client interface (internal/provider) and usage accounting
// through the Ledger interface, both injected. There is no SQLite, HTTP
// or other concrete I/O in this package, so it is unit-testable with
// fake clients.
//
// Chat types (Message, ChatRequest, ChatResponse, RateLimitError,
// QuotaError, UpstreamError) come from internal/provider. One mismatch
// remains between the two layers and is bridged here, not hidden:
// provider.Client streams by callback (ChatStream with onDelta), while
// the engine's streaming core (hedging, first-event races, preread)
// consumes a channel of StreamEvents. bridgeStream adapts one to the
// other; the engine never re-implements the upstream client.
//
// The Engine also satisfies the server.Engine contract
// (internal/server): Chat / ChatStream over provider types, with the
// fallback chain resolved per request model (Config.ChainFor, or every
// registered client by default), plus the optional StreamResumer
// contract (ChatStreamFrom) which replays a fresh internal stream and
// skips the runes the client already received. The resumer is an
// explicit entry point only: the engine never invokes it
// automatically after an in-request failure (see the streaming
// section below — no switch after commit).
//
// Design targets (goals, not claims): detect a failed provider in 2-3s,
// switch to the next attempt in 0.1-0.3s, route a request in under 1s.
// Nothing here hardcodes those numbers; the timeouts and delays are
// configurable in Config and the tests measure the behaviour that can
// be measured locally (hedge timing, attempt counts, preread outcomes).
package router

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/capability"
	"github.com/raunakiitp/codvian-router/internal/coalescing"
	"github.com/raunakiitp/codvian-router/internal/compress"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/modality"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/internal/strategytree"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// StreamEvent is one event from a streaming chat response. Exactly one
// of Delta / Done / Err is meaningful per event: Delta carries text,
// Done marks clean completion, Err marks failure. ToolCalls carries
// the tool-call deltas of this event (streaming tool calls) and
// FinishReason the upstream finish reason when this event is the last
// one ("stop", "length", "tool_calls", ...); both are empty on
// plain-text events.
type StreamEvent struct {
	Delta        string
	ToolCalls    []provider.ChunkToolCall
	FinishReason string
	Done         bool
	Err          error
	// Resp is the reassembled response, set on the Done event
	// only: it carries the upstream's reasoning state (see
	// bridgeStream), so the drain can return it and the next
	// turn can restore it. Nil on every other event.
	Resp *provider.ChatResponse
}

// Ledger records one provider attempt for usage accounting. It is
// defined here (not imported) so the engine depends only on this small
// interface; the concrete SQLite ledger lives in another package.
//
// Usage is the full token split reported by the upstream (see
// provider.Usage). The engine passes it through verbatim and never
// invents a split: failed attempts record Usage{} (all zeros),
// because their token usage is unknown to the engine. A
// successful attempt whose upstream reported no usage at all
// records the local tiktoken estimate of its prompt instead
// (provider.Usage.LocalEstimate, ledger local_estimate=1, split
// unknown, cost 0) — see Engine.reconcileUsage — so the row still
// says how big the prompt was without ever claiming the provider
// reported it. Streaming successes record the same local-estimate
// fallback: the engine never sees the upstream's streaming usage.
type Ledger interface {
	RecordAttempt(provider, model string, ok bool, latencyMs int64, usage provider.Usage)
}

// KeyedLedger is optionally implemented by ledgers that can
// attribute an attempt to the gateway key the request arrived
// under. The engine prefers it when the request names a caller
// (ChatRequest.CallerID, set by the server from the verified
// gateway key): per-key accounting — rolling 5-hour budgets —
// needs the key on the row, and the engine is the only place that
// sees every attempt, including failed fallback attempts. A
// ledger that does not implement it keeps the old behaviour (rows
// with an empty key), and an empty CallerID records the same way.
type KeyedLedger interface {
	Ledger
	RecordAttemptForKey(keyID, provider, model string, ok bool, latencyMs int64, usage provider.Usage)
}

// ProviderKeyLedger is optionally implemented by ledgers that can
// attribute an attempt to the provider-side API key that served it
// (CHECKLIST 175, multi-key pooling): each key's usage is recorded
// against that key's ID, never pooled into one number, so per-key
// spend is honest. The engine prefers it over KeyedLedger when
// the attempt names a key; a ledger that does not implement it
// keeps the gateway-key attribution only.
type ProviderKeyLedger interface {
	KeyedLedger
	RecordAttemptWithProviderKey(gatewayKeyID, provider, providerKeyID, model string, ok bool, latencyMs int64, usage provider.Usage)
}

// CancelledLedger is optionally implemented by ledgers that can store
// a cancelled attempt (a hedge loser: started, then cancelled
// because the other hedge attempt won) as a cancelled row instead of
// a failed one (PARITY row B4-15). The engine prefers it for hedge
// losers; a ledger that does not implement it records nothing for
// the loser — the trace still carries the hedged_cancelled entry,
// so the attempt stays visible without inventing a failure.
type CancelledLedger interface {
	RecordCancelledAttempt(keyID, provider, providerKeyID, model string, latencyMs int64)
}

// ---------------------------------------------------------------------------
// Routing types.
// ---------------------------------------------------------------------------

// TokenDivergence is one material disagreement between the local
// tiktoken estimate of a request's prompt tokens and the count the
// upstream reported for the same request. It is a flag, not a
// correction: the engine never overwrites the provider's number
// with the local one (or vice versa) — both are kept, the ledger
// stores the provider's, and the divergence is counted and
// surfaced so the operator can see which side to trust.
type TokenDivergence struct {
	Provider string
	Model    string
	// Local is the local tiktoken estimate; Reported the
	// provider-reported prompt count, for the same request.
	Local    int
	Reported int
	At       time.Time
}

// TokenCountStats is the engine's local-count reconciliation
// accounting, read via Engine.TokenCountStats: how many
// successful attempts carried both a local estimate and a
// provider-reported prompt count (Compared), how many of those
// diverged materially (Diverged), how many counts failed
// outright (CountErrors — a counting failure never fails the
// request, it only skips the comparison), and the most recent
// divergence (Last, with HasLast false before any).
type TokenCountStats struct {
	Compared    uint64
	Diverged    uint64
	CountErrors uint64
	Last        TokenDivergence
	HasLast     bool
}

// tokenDivergenceThreshold: a local estimate and a provider
// report diverge materially when they differ by more than 20
// tokens AND more than 10% of the reported count. Small prompts
// (a 12-token greeting counted as 11) are rounding noise, not a
// signal; a 500-token prompt counted as 200 is.
func tokenDivergenceMaterial(local, reported int) bool {
	if reported <= 0 || local <= 0 {
		return false
	}
	diff := local - reported
	if diff < 0 {
		diff = -diff
	}
	threshold := reported / 10
	if threshold < 20 {
		threshold = 20
	}
	return diff > threshold
}

// Attempt is one step of a fallback chain: a provider and a model on it.
//
// KeyID names the provider-side API key serving this attempt
// (CHECKLIST 175, multi-key pooling): "" means the provider's
// legacy single key, a "key_…" ID names one key of the pool. The
// engine's per-key state — clients, cooldowns — is keyed by
// (ProviderID, KeyID) via ClientKey, so a dead key is cooled down
// without taking the provider's other keys with it. Reserve marks
// a reserve key: it is held back until every preferred key is
// exhausted (see orderAttempts — reserve attempts always sort
// after preferred ones, and are never hedge targets).
//
// Effort is the reasoning effort this attempt runs at (CHECKLIST
// 255): "" means no explicit effort — the client's own
// reasoning_effort passes through, or the provider default
// applies — otherwise one of the provider.ReasoningEffort*
// levels. A routing-group entry is (model, effort), so the same
// model at different efforts is a different attempt: the group
// may hold the same provider+model twice, once per effort, and
// each attempt carries its own effort to the wire (see
// reqWithModel). Two attempts that differ only in effort are
// distinct entries, never deduplicated.
//
// EffortForwarding is the per-wire forwarding label for this
// attempt's effective effort (CHECKLIST 285-286, Vertex 303-304) — see
// resolveEffortForwarding: "forwarded:..." with the field and
// value the upstream received, or "not-forwarded:..." with the
// reason it did not. "" when the attempt ran at no explicit
// effort. Stamped by withEffortForwarding at call time, carried
// into the trace by stampTraceEffortForwarding.
type Attempt struct {
	ProviderID string
	Model      string
	KeyID      string
	Reserve    bool
	Effort     string
	// EffortForwarding is the per-wire forwarding label for this
	// attempt's effective effort (CHECKLIST 285-286, Vertex 303-304): see
	// resolveEffortForwarding for the label forms. "" when the
	// attempt ran at no explicit effort (nothing to forward).
	EffortForwarding string
	// EffortPolicy is the forced-reasoning policy decision for
	// this attempt (CHECKLIST 311-312): "" when no policy
	// matched this attempt's request or model, otherwise
	// "forced:<effort>:policy:<model>" (the policy won) or
	// "client-override:<effort>:policy:<model>" (the client's
	// value won because the policy allows override). Stamped by
	// withForcedEffort / applyRequestForcedEffort, carried into
	// the trace by stampTraceEffortPolicy.
	EffortPolicy string
}

// ClientKey is the engine-internal identity of one (provider, key)
// pair: the provider ID for the legacy single key, otherwise
// provider + NUL + key ID. Clients, hard cooldowns and soft
// cooldowns are all keyed by it, so per-key state can never leak
// into another key of the same provider. The NUL separator cannot
// appear in a provider ID ("prov_…") or a key ID ("key_…").
func ClientKey(providerID, keyID string) string {
	if keyID == "" {
		return providerID
	}
	return providerID + "\x00" + keyID
}

// SplitClientKey is the inverse of ClientKey: the provider ID and
// the key ID ("" for the legacy single key).
func SplitClientKey(key string) (providerID, keyID string) {
	if i := strings.IndexByte(key, '\x00'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}

// ErrNoAvailableAttempt is returned when a chain has no attempt the
// engine is willing to try (empty chain, all providers zero-quota, or
// all skipped by breaker/cooldown).
var ErrNoAvailableAttempt = errors.New("router: no attempts available (all skipped or chain empty)")

// ErrDeadlineExceeded is returned when a request's per-combo deadline
// budget (Config.DeadlineBudget) runs out before any provider
// succeeds. It wraps context.DeadlineExceeded so the server maps it
// to a clean 504 (see server.handleEngineError): the combo stops
// instead of burning more attempts. It is only ever produced by the
// engine's own budget — a caller whose own context expires gets the
// caller's error, not this one.
var ErrDeadlineExceeded = fmt.Errorf("router: deadline budget exceeded: %w", context.DeadlineExceeded)

// Config tunes the engine. Zero values get the documented defaults.
// Set HedgeDelay negative to disable hedging entirely.
// StrategyHookContext carries the per-request facts a strategy
// hook needs that the chain alone cannot express (M6.5-wiring):
// the size of the prompt being routed, whether the request
// carries tools, and its sampling temperature. The hook factory
// (internal/strategy, HookFactoryWithCacheAffinity) closes over
// the live warmth signal itself; this context only carries the
// request side. It is built once per request by
// strategyHookContextFor, from the request as it will actually
// be sent, and threaded through orderAttempts — never stored on
// the Engine (the Engine is shared across concurrent requests).
//
// ContractPinned is true when the client supplied an explicit
// per-request routing order (preserveOrder): the contract wins
// over every strategy, so the hook must never re-rank. In the
// wired path it is always false at hook time — preserveOrder
// short-circuits before any hook runs — but it is carried anyway
// so the cacheability gate can be proved at the hook level too,
// and so a future caller that skips the short-circuit still
// cannot re-rank a pinned request.
// TemperatureSet distinguishes an explicitly-set temperature
// from the zero value: ChatRequest.Temperature is a plain
// float64, so 0 means unset-or-zero, and both are deterministic
// — the gate only closes on a temperature > 0.
// PromptTokens is the local tiktoken count of the message text
// (tokencount.CountPrompt): an estimate — it excludes
// per-message framing and tool schemas — so the gate errs on the
// side of not re-ranking when the count is uncertain. A counting
// failure yields 0, which closes the gate.
type StrategyHookContext struct {
	PromptTokens   int
	HasTools       bool
	Temperature    float64
	TemperatureSet bool
	ContractPinned bool
	// SLA carries the operator's sla-aware objectives for this
	// request (see Config SLATargetP95Ms / SLAMaxErrorRate /
	// SLAMaxCostPer1M / SLAHardConstraints): the hook resolves
	// them through sla.Policy.Resolve, so the zero value here
	// means "defaults" (2000 ms p95, 5% errors, no cost cap,
	// soft constraints) — except when SLAMaxErrorRateSet is
	// true, in which case SLAMaxErrorRate is the operator's
	// explicit value and 0 means zero-tolerance (any error
	// violates); see strategyHookContextFor.
	SLATargetP95Ms     float64
	SLAMaxErrorRate    float64
	SLAMaxCostPer1M    float64
	SLAHardConstraints bool
	// SLAMaxErrorRateSet distinguishes an explicitly
	// configured SLAMaxErrorRate from the zero value: when
	// false, a 0 rate means "unset" (the 0.05 default); when
	// true, 0 is a real zero-tolerance policy. Set by main.go
	// from the config file's routing.sla.max_error_rate
	// pointer (nil = unset); hand-built Configs that assign
	// a positive rate without this flag keep working — a
	// positive rate is used as-is either way.
	SLAMaxErrorRateSet bool
	// Ledger is the ledger accessor for budget-aware routing (B11).
	// It provides access to KeyBudgetStatus and KeyBudgetConfig for
	// the gateway key. When nil, budget-aware routing falls back to
	// balanced order.
	Ledger Ledger
	// ApiKeyId is the gateway key ID for budget-aware routing (B11).
	// When empty, budget-aware routing falls back to balanced order.
	ApiKeyId string
}

type Config struct {
	// DisableAdaptiveLimiter, when true, runs the engine with NO
	// adaptive concurrency limiter (the limiter stays nil, and
	// the adaptive package fails open: every admission is
	// granted). This is a MEASUREMENT switch for the E1 limiter
	// verdict (CHECKLIST 233–234): limiter-off vs limiter-on
	// back-to-back on the same tree, to test whether the
	// Gradient2 limiter is the RPS ceiling. It is NOT a tuning
	// recommendation — with the limiter off, overload queues
	// instead of shedding, so latency under load rises and a
	// slow provider is no longer shed before 429s appear.
	// Default false: the limiter is on.
	DisableAdaptiveLimiter bool
	// MaxAttempts caps the total number of provider calls for one
	// request across the whole chain, including 429 retries and
	// hedged second attempts. Default 5.
	MaxAttempts int
	// G11 Area 2 failover residuals (parity tracker Area 2, 7 PARTIAL
	// rows) — Go-level fields for the knobs the reference router exposes.
	// Config-file wiring (routing.*) is the coordinator's: the
	// verbatim wiring is in the G11 report, and config.go is
	// Wave 3's. Zero values are the documented defaults.
	// ResponseValidation is the 200-OK body validation
	// predicate (row 1): a 200 body that fails it fails over
	// to the next target. Zero value = built-in error-payload
	// predicate, enabled. See response_validation.go.
	ResponseValidation ResponseValidationConfig
	// BreakerStorePath is the JSON file the consecutive-breaker
	// state persists to on every breaker state change (row 2).
	// "" (default) disables persistence entirely. See
	// breaker_store.go.
	BreakerStorePath string
	// DeadlineFailureMax is the readiness-deadline failure
	// sub-budget (row 4): this many per-target deadline
	// failures in one request end the combo with a clean 504.
	// 0 = default 3; negative disables the sub-budget. See
	// deadline_budget.go.
	DeadlineFailureMax int
	// MaxProvidersPerRequest caps the DISTINCT providers
	// attempted in one request (row 5): 0 = default 3.
	// Same-provider retries do not consume it. See
	// failover_policy.go.
	MaxProvidersPerRequest int
	// FailoverMaxRetries caps same-target retries after the
	// first failure, per target (row 6, the reference router maxRetries):
	// 0 = default 3; negative = no same-target retry.
	FailoverMaxRetries int
	// FailoverRetryDelay is the base delay between same-target
	// retries (row 6, the reference router retryDelayMs): 0 = default
	// 200 ms. The decorrelated full-jitter ladder still
	// applies on top of it (transient.go).
	FailoverRetryDelay time.Duration
	// FailoverMaxSetRetries caps same-target retries across
	// the whole set — the whole chain for this request (row 6,
	// the reference router maxSetRetries): 0 = no set cap.
	FailoverMaxSetRetries int
	// FailoverBeforeRetry is the explicit failover-before-retry
	// switch (row 6, the reference router failoverBeforeRetry): when true
	// AND a next target exists in the chain, a retryable
	// transient failure (502/503/504) fails over immediately
	// instead of retrying the same target. Default false: the
	// same target is retried first (up to FailoverMaxRetries,
	// reserve-guarded) — matching the reference router's EFFECTIVE
	// default. Its combo default is true, but the #10217 guard
	// reads only the EXPLICIT flag
	// (failoverBeforeRetryExplicit), so with pure defaults
	// the same target is retried first there too. 429s are
	// unaffected: they already fail over immediately whenever
	// a next target exists.
	FailoverBeforeRetry bool
	// HedgeDelay is how long to wait for the first attempt's first
	// response before starting the next attempt in parallel.
	// Default 150ms; negative disables hedging.
	HedgeDelay time.Duration
	// BackoffBase is the first exponential-backoff delay used when a
	// provider 429s without a Retry-After hint. Default 200ms.
	BackoffBase time.Duration
	// BackoffMax caps exponential backoff. Default 5s.
	BackoffMax time.Duration
	// BreakerThreshold is how many consecutive failures open a
	// provider's circuit breaker. 0 means "use the auth-profile
	// table" (M4.2: OAuth 8, API-key 12, local 2 — the reference router
	// PROVIDER_PROFILES). A non-zero value overrides the table
	// for EVERY provider (the old uniform behaviour, kept for
	// explicit configuration and for tests).
	BreakerThreshold int
	// PredictiveTtftMs is the predictive TTFT breaker threshold
	// (M4.7a): once a provider has at least 5 recorded stream
	// first-byte latencies, a target whose mean TTFT exceeds this
	// is skipped while another target remains in the chain. 0
	// disables the prediction (the default).
	PredictiveTtftMs int
	// TokenLimitCheck, when set, is consulted before each attempt
	// (M5.3): it reports the breached per-key token limit for
	// (keyID, provider, model), or nil when the target still has
	// headroom. A breached target is skipped (trace outcome
	// "skipped_token_limit"); when every target is breached the
	// first breach is the request error. An error fails closed.
	// Nil means no token limits are configured.
	TokenLimitCheck func(ctx context.Context, keyID, provider, model string) (*TokenLimitBreach, error)
	// InternalBudgetCheck, when set, is consulted before each
	// attempt (M5.11): it reports the breached provider-scoped
	// internal budget for one provider, or nil when the provider
	// still has headroom. A breached target is skipped (trace
	// outcome "skipped_internal_budget"); when every target is
	// breached the first breach is the request error. An error
	// fails closed. Nil means no internal budgets are
	// configured. Unlike TokenLimitCheck it applies to anonymous
	// requests too — the cap is on the operator's total spend.
	InternalBudgetCheck func(ctx context.Context, provider string) (*InternalBudgetBreach, error)
	// RateLimitStore, when set, persists learned upstream
	// rate-limit signals per (provider, key) (M5.13). The write
	// runs in a goroutine and its errors are dropped — pacing is
	// the in-memory limiter, persistence is durability only. Nil
	// means signals pace the limiter but are not stored.
	RateLimitStore RateLimitStore
	// PerTargetTimeout bounds one non-streaming attempt's
	// time-to-first-response (M4.7c). 0 means the 120 s default;
	// a negative value disables it. Streaming attempts are
	// bounded instead by preread (5 s) + the throughput
	// watchdog (30 s + 30 s) — see pertarget.go.
	PerTargetTimeout time.Duration
	// WaitBudgets is the opt-in wait-budget configuration
	// (M4.8): when enabled, plain 429s are waited out on the
	// same target (90 s / 5 / 300 s) instead of failing over.
	// Default off (fail-fast). See waitbudget.go.
	WaitBudgets WaitBudgetConfig
	// PreferredFailoverAfter is the opt-in preferred-account
	// response-start deadline (M4.8): when set, the first
	// streaming attempt (the preferred account) that produces
	// no first byte within this long is cancelled and the chain
	// rotates to ONE sibling account. 0 disables it (default).
	// See preferred.go.
	PreferredFailoverAfter time.Duration
	// BreakerOpenFor is how long an open breaker stays open before a
	// half-open probe is allowed. 0 means "use the auth-profile
	// table" (M4.2: OAuth 60s, API-key 30s, local 15s, escalated
	// per trip — see breaker_profile.go). A non-zero value
	// overrides the table for EVERY provider with a FIXED
	// duration: an explicit override never escalates.
	BreakerOpenFor time.Duration
	// AuthProfileFor, if set, reports the auth profile of one
	// provider (M4.2): it selects the breaker thresholds, reset,
	// escalation and failure-window numbers from the profile
	// table. Nil means every provider is API-key (the default).
	// In production it is wired to the registry's auth kind
	// (OAuth / API-key / local). Consulted on the request path
	// only when a breaker decision is made, so it must be cheap
	// and non-blocking.
	AuthProfileFor func(providerID string) AuthProfile
	// ModelLockout is the per-(provider, key, model) lockout (M4.3):
	// a dead model is locked out after a lockout-eligible failure
	// instead of being retried first on every request. Zero value
	// means OFF (the reference router's default): the engine behaves exactly
	// as before. See internal/router/lockout.go for the defaults
	// (base 120 s, max 30 min, max 10 steps, codes
	// 403/404/429/502/503/504).
	ModelLockout LockoutConfig
	// RetryAfterCap caps a provider's Retry-After hint. Default 30s.
	RetryAfterCap time.Duration
	// QuotaHint, if set, reports a provider's remaining quota
	// (higher = more headroom). Providers reporting 0 are skipped.
	// If nil, every provider is treated as having ample quota.
	QuotaHint func(providerID string) int
	// RegionFor, if set, reports the effective serving region
	// for one (provider, model) pair (CHECKLIST 257): the
	// operator-declared region of the model, else of the
	// provider, else "" (unknown — never a default, never
	// guessed). It drives the region-aware failover order
	// (applyRegionTiers) and the trace's per-attempt region.
	// If nil, every region is unknown and no attempt is ever
	// promoted into the cross-region tier.
	RegionFor func(providerID, model string) string
	// ToolCapability, if set, reports one model's tool /
	// function-call capability (CT-1, GAP-TRACK): (capable,
	// known). known is false when the model was never probed
	// and no sync reported it — unknown, never guessed from
	// the model id. It drives the direct-path pre-check (a
	// tool-bearing request never dials a confirmed-incapable
	// model) and the capability-mismatch failover (a 400
	// naming tool support fails over to the next
	// confirmed-capable target). If nil, every model is
	// unknown: the pre-check skips nothing and the failover
	// finds no confirmed-capable target, so behaviour is
	// exactly as before. Consulted on the request path, so
	// it must be cheap and non-blocking.
	ToolCapability func(providerID, model string) (capable, known bool)
	// VisionCapability, if set, reports one model's vision
	// (image-input) capability (B3-24 modality bridges):
	// (capable, known). known is false when no sync ever
	// reported modalities for the model — unknown, never
	// guessed from the model id. It drives the bridge: an
	// image-bearing request never dials a confirmed
	// non-vision model when a vision-capable alternative
	// exists. If nil, every model is unknown and the bridge
	// never fires, so behaviour is exactly as before.
	// Consulted on the request path, so it must be cheap and
	// non-blocking.
	VisionCapability func(providerID, model string) (capable, known bool)
	// VisionPool, if set, returns the pool of models the
	// bridge may reroute to: every model the registry knows
	// to be vision-capable, in registry order (the pool order
	// is the pick ranking). If nil, the bridge never fires.
	VisionPool func() []modality.Candidate
	// ModalityCapability, if set, reports one model's full
	// modality capability record (B3-24 modality bridges):
	// the Vision/Audio/Video flags with their Known markers,
	// as the models.dev sync reported them, plus whether the
	// model is known to the registry at all. It drives the
	// bridge for every modality: an audio/video-bearing
	// request never dials a confirmed-incapable model when a
	// capable alternative exists. Unknown models fail open
	// exactly as vision does. If nil, the bridge falls back
	// to the vision-only VisionCapability/VisionPool hooks;
	// if those are nil too, the bridge never fires.
	// Consulted on the request path, so it must be cheap and
	// non-blocking.
	ModalityCapability func(providerID, model string) (provider.Model, bool)
	// ModalityPool, if set, returns the pool of models the
	// bridge may reroute to: every model the registry knows
	// to be capable of at least one modality (vision, audio
	// or video, each known && capable), in registry order
	// (the pool order is the pick ranking). modality.Decide
	// filters the pool per request — a candidate only wins
	// when it serves every blocking modality and is
	// incapable of none required — so a vision-only
	// candidate never wins an audio-blocked request. If nil,
	// the bridge falls back to the vision-only pool.
	ModalityPool func() []modality.Candidate
	// CapabilityLookup reports one model's unified capability record
	// (P1.5). ok is false when the provider/model pair is unknown,
	// which the filter treats as "no capability is verified" — never
	// as an empty set that passes. Set by CapabilityLookupFunc over
	// a capability.Index. Consulted only when RequireCapabilities is
	// non-empty, so it must stay cheap.
	CapabilityLookup func(providerID, model string) (capability.ModelCapabilities, bool)
	// RequireCapabilities lists capabilities an attempt must hold
	// with verification (Support == supported && Verification ==
	// verified) to be dialled. Empty means no filter — the default
	// path is unchanged. Unknown and merely-declared capabilities
	// never satisfy a requirement: absence of evidence is not
	// evidence of support.
	RequireCapabilities []capability.Capability
	// EffortBudgetFor, if set, returns the operator-declared
	// thinking-token budget for one (wire, effort) pair
	// (CHECKLIST 285-286): the `reasoning.effort_budgets`
	// table from config, consulted only for the Anthropic and
	// Gemini wires. It reports false when there is no table,
	// when the wire takes no numeric budget, or when the
	// effort has no entry — every one of those means "send no
	// thinking field", never a guessed budget. If nil, no
	// budget is ever resolved and every Anthropic/Gemini
	// attempt is recorded as not forwarded for want of a
	// table. Consulted on the request path, so it must be
	// cheap and non-blocking.
	EffortBudgetFor func(wire, effort string) (int, bool)
	// ForcedEffortFor, if set, returns the operator-declared
	// forced-reasoning rule for one model (CHECKLIST 311-312):
	// the `reasoning.forced_effort` rules from config, first
	// exact match then the "*" fallback. It reports false when
	// no rule matches the model. If nil, no forced-effort
	// policy is declared and no request is ever forced — tier,
	// group-entry and client efforts apply as before.
	// Consulted on the request path, so it must be cheap and
	// non-blocking.
	ForcedEffortFor func(model string) (ForcedEffort, bool)
	// SelfHosted, if set, reports whether a provider is
	// self-hosted (CHECKLIST 257): a local open-weight model
	// served from the operator's own machine. It is the last
	// failover tier, after every hosted equivalent. If nil,
	// nothing is self-hosted.
	SelfHosted func(providerID string) bool
	// ChainFor, if set, builds the fallback chain for a request model.
	// It is used by the server-facing Chat / ChatStream / ChatStreamFrom
	// methods, which take no explicit chain. If nil, the chain is every
	// registered client for the model, in sorted provider-ID order.
	// Callers that know which providers offer a model (e.g. main, from
	// the registry) should set this so providers that do not offer the
	// model are never tried.
	ChainFor func(model string) []Attempt
	// OrderHook, if set, is applied as the FINAL reordering step of
	// orderAttempts, after zero-quota filtering and quota-hint sorting.
	// It receives the quota-ordered chain and returns the order to try.
	// This is the frozen extension point for cost/latency/quality
	// scoring (internal/scoring): scorers live in their own package
	// and never edit this file. The hook must be deterministic for a
	// given input, must not drop or duplicate attempts, and must not
	// block. If nil, quota order stands.
	OrderHook func(chain []Attempt) []Attempt
	// AutoRoute, if set, resolves virtual "auto/*" model names to a
	// concrete model before the chain is built (internal/autorouter).
	// It receives the request model and the routing prompt (the last
	// user message's text) and returns the concrete model, the
	// routing decision, and ok=false when the name is unknown or no
	// concrete model can be resolved. When ok is false the model is
	// left unchanged and the engine fails honestly with an empty
	// chain / unknown model, never inventing a model. The decision
	// of the most recent auto-routed request is kept for
	// LastAutoDecision. If nil, "auto/*" models are not resolved
	// and behave like any other (unknown) model name.
	AutoRoute func(model, prompt string) (resolved string, dec autorouter.Decision, ok bool)
	// Strategy is the global routing strategy name (see
	// internal/strategy.All for the full family). Empty means
	// "balanced". It is the fallback when neither the request nor
	// the chain names a strategy. Unknown names are passed to
	// StrategyHook, which ranks them as priority (declared order)
	// — the engine never errors on a strategy name.
	Strategy string
	// ChainStrategy, if set, names the routing strategy for one
	// chain (called with the request model, after any "auto/*" or
	// "auto:<axis>" resolution). "" means "inherit the global
	// strategy". A per-request "auto:<axis>" alias always wins over
	// this (research-log entry 156).
	ChainStrategy func(model string) string
	// StrategyHook, if set, builds the OrderHook for a resolved
	// strategy name: it receives the effective strategy for the
	// request ("balanced", "cheapest", ...) and returns the hook
	// that reorders the chain under it. It is called once per
	// request, on the request path, so it must be cheap and
	// non-blocking. When nil, OrderHook is used for every
	// strategy (the pre-strategy behaviour); when both are nil,
	// quota order stands. Supplied by internal/strategy
	// (HookFactory) in production: the router itself never
	// imports the strategy package (that would be a cycle), so
	// strategy names are opaque strings here.
	StrategyHook func(strategy string) func(chain []Attempt) []Attempt
	// StrategyHookForRequest, when set, builds the OrderHook for
	// one strategy for one request: it receives the effective
	// strategy name and the request's StrategyHookContext and
	// returns the hook that reorders the chain under it. It is
	// preferred over StrategyHook whenever it is set (orderAttempts
	// falls back to StrategyHook, then OrderHook, when it is nil),
	// so a context-free factory keeps working untouched and a
	// per-request factory only has to be installed where the
	// request context is available. Supplied by internal/strategy
	// (HookFactoryWithCacheAffinity) in production. Like
	// StrategyHook it is called once per request, on the request
	// path, so it must be cheap and non-blocking: no per-request
	// goroutines, no network calls, no ledger reads inside the
	// hook itself — any live signal it needs must already be
	// closed over by the factory.
	StrategyHookForRequest func(strategy string, ctx StrategyHookContext) func(chain []Attempt) []Attempt
	// LKGPInvalidator, when set, is called with the chain key
	// (the requested model — the same key the LKGP registry
	// records successes under) and the provider after a hard
	// failure (5xx, timeout, connection error) against that
	// provider, so a dead pinned provider stops being hoisted
	// first on the next request (RT-2 failure-clear). It is
	// called only for the hard-failure class — never for 4xx,
	// 429s, or cancellations — and only when the recorded pin
	// actually names that provider (the hook itself decides
	// that; this package never sees the record). Supplied by
	// internal/strategy in production (the default registry's
	// ClearIfProvider): the router itself never imports the
	// strategy package (that would be a cycle), so the pin
	// store is behind this func-typed field, exactly like
	// StrategyHook. When nil, pins are never cleared on
	// failure and expire only via the LKGP TTL. It must be
	// cheap and non-blocking — an in-memory map delete.
	LKGPInvalidator func(chainKey, provider string)
	// AccountFallbackStrategy is the account-level fallback
	// strategy (M3 Area 1, PARITY row 388): how the ACCOUNTS
	// (keys) of one provider serving one model are ordered
	// within that (provider, model) group when a request falls
	// back between them. It mirrors the reference router's
	// settings.fallbackStrategy, overridable per provider via
	// AccountFallbackStrategies (their
	// providerStrategies[provider].fallbackStrategy), default
	// "fill-first" there and here. Valid values are the
	// AccountFallback* constants in account_fallback.go
	// ("expiry-first" is NOT valid: no per-key quota-expiry
	// source exists anywhere, so it cannot order accounts).
	// An unknown or empty value means "fill-first". It is
	// applied in orderAttempts after the chain strategy hook,
	// and never under an explicit client order (preserveOrder).
	AccountFallbackStrategy string
	// AccountFallbackStrategies overrides
	// AccountFallbackStrategy per provider ID. An unknown or
	// empty value for a provider falls back to
	// AccountFallbackStrategy.
	AccountFallbackStrategies map[string]string
	// SLATargetP95Ms, SLAMaxErrorRate, SLAMaxCostPer1M and
	// SLAHardConstraints are the operator's objectives for the
	// "sla-aware" strategy (M3 Area 1, PARITY row 394; see
	// internal/sla). Defaults when unset: 2000 ms p95, 5%
	// errors, no cost cap, soft constraints. A target <= 0
	// means the 2000 ms default; an error rate <= 0 means the
	// 0.05 default UNLESS SLAMaxErrorRateSet is true, in
	// which case an explicit 0 is zero-tolerance (any error
	// violates) — see SLAMaxErrorRateSet below; a cost cap
	// <= 0 means "no cap" (an explicit 0 cap is the same as
	// unset: the scorer treats 0 as no cap).
	SLATargetP95Ms     float64
	SLAMaxErrorRate    float64
	SLAMaxCostPer1M    float64
	SLAHardConstraints bool
	// SLAMaxErrorRateSet is the presence flag for
	// SLAMaxErrorRate (see StrategyHookContext): true when
	// the operator explicitly configured the error rate, so
	// an explicit 0 survives to the hook as zero-tolerance
	// instead of being rewritten to the 0.05 default. Set
	// by main.go from routing.sla.max_error_rate; a
	// hand-built Config with a positive rate and this flag
	// false still uses the rate as-is.
	SLAMaxErrorRateSet bool
	// NadirEndpoint is the explicitly configured decision
	// endpoint for the "nadir" strategy (M3 Area 1, PARITY
	// row 396): the origin of a Nadir-compatible decision API
	// (e.g. "https://api.getnadir.com"). Empty means
	// UNCONFIGURED: auto/nadir then falls back to priority
	// order with a clear note and NEVER calls any external
	// API — fail-closed, a deliberate divergence from
	// the reference router's fail-open default endpoint. See nadir.go.
	NadirEndpoint string
	// NadirAPIKey is the API key sent as X-API-Key to the
	// decision endpoint, when one is configured. Empty means
	// no key is sent.
	NadirAPIKey string
	// NadirTimeoutMs bounds one decision call in
	// milliseconds. <= 0 means the 2000 ms default; values
	// above 30000 are clamped to 30000.
	NadirTimeoutMs int
	// NadirCooldownMs is the failure-cooldown window for
	// the decision endpoint in milliseconds (G5): after a
	// failed decision call the endpoint is skipped for this
	// long before it is tried again. <= 0 means the 30000
	// ms default (NadirFailureCooldownMs). Set by main.go
	// from routing.nadir.cooldown_ms.
	NadirCooldownMs int
	// NadirTransport, when set, performs the decision POST
	// (URL, JSON body, API key, timeout) and returns the HTTP
	// status and body. It is the test seam: production uses
	// the default net/http transport. Nil means the default.
	NadirTransport NadirTransport
	// QuotaShareFor, if set, reports the operator-configured
	// quota-share weight for one (provider, model) pair (Phase D,
	// D4): the member's share in a "quota-share" pool. It is
	// consulted on the request path, so it must be cheap and
	// non-blocking. The share follows strategy.normalizeShare: an
	// explicit 0 disables the member relative to weighted
	// siblings; a missing, non-finite or negative share defaults
	// to 1; when all shares are 0 the pool shares evenly. If nil,
	// every member shares 1 (an unweighted pool rotates evenly).
	// It is invoked while holding e.mu (see orderQuotaShare in
	// cascade.go), so it must be cheap, non-blocking, and must
	// never call back into the Engine — a re-entrant call (e.g.
	// SetQuotaHint, which also locks e.mu) would deadlock.
	// The router never imports internal/strategy (that would be
	// a cycle), so the DRR itself lives in cascade.go with
	// semantics mirroring strategy.SelectQuotaShare.
	QuotaShareFor func(providerID, model string) float64
	// RelaySummariser, if set, is the caller-owned summariser
	// call for context-relay (Phase D, D5): given the source
	// messages of the request being relayed it returns the
	// handoff summary the next attempt receives. Nil means no
	// summariser is configured — the handoff degrades honestly
	// to the raw transcript, labelled unsummarised, never a
	// faked summary. An error from the summariser degrades the
	// same way. Consulted on the request path, lazily, only
	// when another attempt is actually about to be dialled, so
	// it must be cheap and non-blocking relative to that
	// attempt. The production supplier is main.go (cheapest
	// priced model with a live client, direct provider call).
	// See RelaySummariser in relay.go.
	RelaySummariser RelaySummariser
	// RelayQuotaUsedFor, if set, reports the account's quota use
	// as a fraction in [0, 1] for one (provider, model) pair
	// (Phase D, D5): the signal the context-relay success
	// trigger needs (a successful turn with use in [0.85, 0.95)
	// requests a handoff). It reports false when the use is
	// unknown — unknown is never treated as zero and never
	// guessed. Supplied in production by main.go, from the
	// engine's learned quota hint (LearnedQuotaHint — measured
	// from real upstream rate-limit headers). Consulted on the
	// request path, so it must be cheap and non-blocking.
	RelayQuotaUsedFor func(providerID, model string) (float64, bool)
	// RelayHandoffProviders, if set, is the allowlist of
	// providers context-relay may fire for (Phase D, D5). Empty
	// means the reference router's default: ["codex"] only.
	RelayHandoffProviders []string
	// RelayOnError opts into the universal-handoff "on-error"
	// trigger (Phase D, D5): a failed attempt on an allowlisted
	// provider also requests a handoff, so the next attempt in
	// the chain receives the relayed context instead of starting
	// cold. Off by default — the strategy-relay trigger is a
	// successful turn approaching quota.
	RelayOnError bool
	// RelayHandoffThreshold is the quota-use fraction at which
	// a successful turn on a handoff provider starts relaying
	// context (B2-01, the reference router HANDOFF_WARNING_THRESHOLD =
	// 0.85). 0 means the default (0.85). See relay.go.
	RelayHandoffThreshold float64
	// RelayExhaustionThreshold is the quota-use fraction at or
	// above which relaying is pointless — the quota is
	// effectively gone (B2-01, the reference router
	// HANDOFF_EXHAUSTION_THRESHOLD = 0.95). 0 means the
	// default (0.95). It must be above
	// RelayHandoffThreshold; config.Validate enforces the
	// ordering for the config-file surface.
	RelayExhaustionThreshold float64
	// RelayTTL is how long a generated handoff stays usable
	// (B2-01, the reference router DEFAULT_TTL_MS = 5h). 0 means the
	// default (5h). See relay.go.
	RelayTTL time.Duration
	// RelayMaxMessages caps the source messages fed to the
	// summariser (B2-01, the reference router
	// DEFAULT_MAX_MESSAGES_FOR_SUMMARY = 30). 0 means the
	// default (30). See relay.go.
	RelayMaxMessages int
	// StrategyTree, if set, is the nestable strategy tree for
	// every chain (CHECKLIST 177): a node picks among its
	// children by its strategy, leaves are providers — see
	// internal/strategytree. ChainStrategyTree, if set, names
	// the tree for one chain (called with the request model);
	// it wins over StrategyTree for that model, exactly like
	// ChainStrategy wins over Strategy. A nil tree (both) means
	// no tree: chains order by the flat strategy. Trees are
	// validated at config load (config.Validate), so a tree
	// that reaches the engine is already known-good; the
	// evaluation still guards depth and cycles defensively,
	// because a tree can also be supplied in-process.
	StrategyTree      *strategytree.Node
	ChainStrategyTree func(model string) *strategytree.Node
	// ContractPriceFunc, if set, returns the operator-configured price
	// for a (provider, model) pair — pair price first, then the
	// model-level fallback (the price book's Lookup precedence).
	// It backs the per-request routing.max_price ceiling: a pair
	// fits only when every ceiling leg set has a known price at
	// or under it. Nil means no price is known for any pair, so
	// every max_price check excludes as price_unknown. It is
	// consulted on the request path, so it must be cheap and
	// non-blocking.
	ContractPriceFunc func(providerID, model string) (config.ModelPrice, bool)
	// TrainingSafe, if set, reports whether a provider is
	// verified as not collecting training data from requests
	// (operator-marked no_training_data, or local by
	// construction). It backs routing.data_collection=deny: a
	// provider it does not vouch for is excluded. Nil means no
	// provider can be verified, so "deny" excludes everything
	// and fails closed — the control is never silently allowed.
	// Consulted on the request path; must be cheap and
	// non-blocking.
	TrainingSafe func(providerID string) bool
	// ResolveProvider, if set, resolves a provider name from a
	// per-request routing contract to the registered provider
	// ID it names. Clients address providers by the names they
	// see (the dashboard shows display names; OpenRouter-style
	// slugs are names, not registry IDs), while the engine's
	// chains are keyed by ID — so order/only/exclude entries
	// are resolved here, before validation: an exact ID match,
	// then an exact display-name match, then a
	// case-insensitive display-name match; an ambiguous name
	// (two providers sharing it) is an error, never a guess.
	// Nil means names are already IDs (the unit-test shape):
	// every name is used verbatim. Consulted on the request
	// path; must be cheap and non-blocking.
	ResolveProvider func(name string) (string, error)
	// DeadlineBudget is the per-combo time budget (research-log
	// entry 155): the whole fallback chain for one request —
	// every attempt, every backoff, every hedge — must finish
	// within it. When it runs out the request stops with
	// ErrDeadlineExceeded (mapped to a clean 504 by the server)
	// instead of burning more attempts. 0 (default) disables it:
	// the request is bounded only by the caller's context.
	DeadlineBudget time.Duration
	// StreamMaxLifetime is the hard lifetime bound on a stream
	// (CHECKLIST 255, Magpie v0.1.4406): every held or relayed
	// stream — live, preread-held, Bedrock incremental, drained
	// — is terminated at this age, with a clean terminal event
	// carrying ErrStreamLifetimeExceeded. 0 (default) means the
	// 30-minute default (DefaultStreamMaxLifetime); a negative
	// value disables the cap entirely. See streamlifetime.go for
	// the full rule and its interaction with DeadlineBudget and
	// the server's per-request timeout.
	StreamMaxLifetime time.Duration
	// PriceFunc, if set, prices one (provider, model) call in
	// USD from its token counts: PriceFunc(provider, model,
	// promptTokens, completionTokens). It is the per-request
	// budget's price source (CHECKLIST 155): the engine's
	// pre-attempt gate prices the next attempt's prompt-only
	// estimate with it, and prices completed attempts' reported
	// usage to accumulate cumulative spend. A nil PriceFunc
	// means "nothing is priced": the budget gate cannot price
	// an attempt, so it never blocks one — the server's
	// pre-send check (which uses the price book directly) is
	// the only enforcement, and unpriced requests proceed.
	// Production wires it to the operator-configured price
	// book (pair price first, model-level fallback), per leg:
	// a leg with no configured price contributes $0, never a
	// guessed price.
	PriceFunc func(providerID, model string, promptTokens, completionTokens int) float64
	// CooldownMode is the global cooldown mode for retryable
	// failures (see internal/router/cooldown.go): "hard" (the
	// default — a cooling provider is skipped until its cooldown
	// ends) or "soft-demote" (it moves to the end of the order
	// instead and is still tried once). "" means "hard".
	// Unknown names resolve to "hard".
	CooldownMode string
	// ChainCooldownMode, if set, names the cooldown mode for one
	// chain (called with the request model, after any "auto/*"
	// resolution). "" means "inherit the global mode".
	ChainCooldownMode func(model string) string
	// CompressionLosslessModels names the models whose prompts
	// get the lossless compression steps by default (CHECKLIST
	// 183): the operator-configured per-model opt-in, from
	// config.Compression.LosslessModels. A per-request
	// "compression.lossless": false still opts a single request
	// out, and a per-request opt-in applies to any model; lossy
	// is never configured here — it needs the client's explicit
	// per-call lossy_max_prompt_tokens. Nil/empty means no
	// model compresses by default.
	CompressionLosslessModels []string
	// OnTokenDivergence, if set, is called (synchronously, on the
	// request path — it must be cheap and non-blocking) for every
	// material divergence between the local tiktoken estimate of
	// a request's prompt tokens and the count the upstream
	// reported for the same request (see reconcileUsage). Nil
	// means divergences are only counted in TokenCountStats,
	// never surfaced to a caller.
	OnTokenDivergence func(TokenDivergence)
	// RefusalChainFor, if set, builds the content-policy refusal
	// chain for a request model (CHECKLIST 161): the ordered
	// targets tried — once each, never the refusing target
	// again — when a target refuses the content on policy/safety
	// grounds. It is configured separately from the failure
	// chain (ChainFor): the operator names the targets known to
	// accept that content class. It returns nil when no chain
	// is configured for the model (the default applies: the
	// failure chain's remaining targets, in order, each once);
	// a non-nil return — even an empty one — is a configured
	// chain, and an empty one means "no refusal routing for
	// this model: the first refusal ends the request in an
	// honest refusal at once". The engine filters the returned
	// chain (already-tried targets, targets with no live
	// client, zero-quota targets), so the hook never has to.
	RefusalChainFor func(model string) []Attempt
	// GroupChain, if set, resolves a user-named routing group
	// (CHECKLIST 173) to its fallback chain for a request model:
	// the group's members, in config order, as (provider, model)
	// attempts. It returns exists=false when no group of that
	// name is configured — the engine then fails the request
	// honestly with a 400 naming the group, never silently
	// falling back to the full chain. An existing group that
	// expands to no attempt for the model (no member serves it,
	// or no member is live) returns exists=true with an empty
	// chain, and the engine fails with a 400 naming the group
	// and the model. Members with no live client, disabled
	// members, and members that do not offer the model are
	// skipped by the hook (the same rules as ChainFor), never
	// dialled. Nil means no groups are configured: any group a
	// request names is unknown and fails the same honest 400.
	GroupChain func(group, model string) (chain []Attempt, exists bool)
	// ContextWindow, if set, reports the context window (in
	// tokens) of one (provider, model) pair, and whether the
	// window is known. It feeds the pre-call fit check (see
	// contextfit.go): before dialling, the prompt is counted
	// locally and compared against each candidate's window
	// (minus a response reserve). A known window smaller than
	// the requirement removes that attempt from the chain
	// before any call; an unknown window (ok=false, or a nil
	// hook) never removes an attempt — unknown is not proof
	// the model is too small. In production it is wired to the
	// provider registry (model ContextWindow metadata, 0 =
	// unknown); in tests it is a map lookup.
	ContextWindow func(providerID, model string) (window int, known bool)
	// ContextReserveTokens is the response reserve (in tokens)
	// held back when the caller did not set max_tokens: the
	// prompt must fit in window minus this reserve. 0 means
	// DefaultContextReserveTokens (4096). A request max_tokens,
	// when set, replaces it for that request.
	ContextReserveTokens int
	// ContextFallbackChain, if set, is the dedicated
	// context-window fallback chain: when no model in the
	// primary chain fits the prompt, the engine jumps straight
	// to this chain (operator-configured long-context models)
	// instead of retrying the too-small models. It is called
	// with the requested model and returns attempts in
	// preference order. Nil means no dedicated chain: a prompt
	// that fits nothing fails honestly pre-call with
	// ErrContextTooLarge.
	ContextFallbackChain func(model string) []Attempt
}

func (c Config) withDefaults() Config {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	// Per-request attempt cap (M4.8, the reference router comboPredicates.ts:
	// maxGlobalAttempts clamped [1, 200]): an operator value
	// above 200 is clamped to 200, never silently honoured — an
	// unbounded attempt budget is a runaway-request risk.
	if c.MaxAttempts > 200 {
		c.MaxAttempts = 200
	}
	// G11 Area 2: cross-provider default is 3 distinct providers
	// (row 5); the readiness-deadline sub-budget defaults to 3
	// failures (row 4). FailoverMaxRetries / FailoverRetryDelay
	// keep 0 as "default" and are resolved by their helpers, so
	// 0 stays distinguishable from an explicit value here.
	if c.MaxProvidersPerRequest <= 0 {
		c.MaxProvidersPerRequest = DefaultMaxProvidersPerRequest
	}
	if c.DeadlineFailureMax == 0 {
		c.DeadlineFailureMax = DefaultDeadlineFailureMax
	}
	if c.HedgeDelay == 0 {
		c.HedgeDelay = 150 * time.Millisecond
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 200 * time.Millisecond
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 5 * time.Second
	}
	// BreakerThreshold / BreakerOpenFor are deliberately NOT
	// defaulted here (M4.2): 0 means "use the auth-profile
	// table", and any non-zero value is the operator's explicit
	// uniform override. Defaulting them to 5 / 30s would make
	// "unset" indistinguishable from "override to 5 / 30s".
	if c.RetryAfterCap <= 0 {
		c.RetryAfterCap = 30 * time.Second
	}
	return c
}

// BreakerEvents is optionally implemented by a sink that wants
// edge-triggered provider-health notifications (a server webhook
// adapter satisfies it). Only CLOSED->OPEN and OPEN->CLOSED
// transitions are reported — steady failures while open, and routine
// successes on a healthy provider, never emit events. A nil sink
// disables reporting without changing breaker behavior.
type BreakerEvents interface {
	ProviderDegraded(provider string, consecutiveFailures, trips int, openFor time.Duration)
	ProviderRecovered(provider string)
}

// UsageMetrics is optionally implemented by a metrics sink that wants
// per-attempt token and upstream-reported-cost counters
// (metrics.Recorder satisfies it). Wiring is best-effort: a nil sink
// disables reporting, never routing or ledger accounting.
type UsageMetrics interface {
	IncUsageTokens(provider, model, kind string, tokens int)
	AddReportedCost(provider, model string, usd float64)
}

// CoalescingMetrics is optionally implemented by a metrics sink that
// wants one counter per shared waiter (metrics.Recorder satisfies it).
type CoalescingMetrics interface {
	IncCoalescedShared()
}

// ConcurrencyMetrics is optionally implemented by a metrics sink
// that wants the adaptive limiter's state (metrics.Recorder
// satisfies it): one counter per call shed by the limiter, and the
// current cap per provider (a gauge). Wiring is best-effort: a nil
// sink disables reporting, never routing.
type ConcurrencyMetrics interface {
	IncConcurrencyRejected(provider string)
	SetConcurrencyLimit(provider string, limit float64)
}

// SharedLedger is optionally implemented by ledgers that can record a
// coalesced (shared) request. The row must be marked shared, carry
// zero tokens and zero cost, and never move a budget: the waiter was
// served from the leader's upstream call, which is the only attempt
// that was billed.
type SharedLedger interface {
	RecordShared(provider, model string, ok bool, latencyMs int64)
}

// ---------------------------------------------------------------------------
// Engine.
// ---------------------------------------------------------------------------

type breakerState struct {
	consecutiveFailures int
	openedAt            time.Time
	probeInFlight       bool
	// lastReason is the taxonomy reason slug of the failure that
	// most recently counted toward this breaker ("" when no
	// breaker-counted failure has happened yet). When the breaker
	// is open it names the failure that tripped it — the reason
	// the dashboard shows for the trip.
	lastReason string
	// lastFailureAt is when the most recent breaker-counted
	// failure happened. It drives lazy recovery (parity tracker:524,
	// the reference router RESILIENCE_GUIDE "Lazy recovery"): a DEGRADED
	// breaker with no new failure for a whole decay hold reads
	// CLOSED again, and a failure after that hold counts fresh
	// instead of consecutively. Zero means "no breaker-counted
	// failure yet" — or a state restored from a store file
	// written before this field existed — and is never treated
	// as stale, so old files keep the old behaviour until a
	// new failure stamps the time.
	lastFailureAt time.Time
	// trips counts completed open cycles (M4.2): it drives the
	// backoff escalation — the Nth trip waits the escalated
	// reset for N. A success deletes the state, so recovery
	// resets the escalation to the base reset.
	trips int
	// openFor is the open duration of the CURRENT open window
	// (the escalated reset for this trip, or the operator's
	// BreakerOpenFor override when set). Zero means "not open".
	openFor time.Duration
}

// providerStats is the mutable live record behind ProviderStats.
type providerStats struct {
	attempts    int
	successes   int
	latencyEWMA float64 // EWMA (alpha 0.2) of successful latencies
	// Latency histogram for true p95 (B9): fixed-bucket histogram
	// covering 0-10000 ms with 1 ms resolution up to 1000 ms,
	// then 10 ms resolution up to 10000 ms.
	latencyHist [1100]int // buckets: 0-1000 (1ms each), 1010-10000 (10ms each)
}

// RoutingInfo is the routing outcome of the most recently finished
// (or started) request: which strategy actually ordered its chain,
// for which model, and which provider served it ("" when no provider
// served it — the request failed or is still running). Reason is why
// that provider served it (see decision.go: fallback-after-N,
// group, contract, tree, or the strategy-derived reason); "" until a
// provider is chosen. At is when the request started. Read via
// LastRouting; it is an honest last-request record, never a summary
// or a sample.
type RoutingInfo struct {
	Strategy string
	Model    string
	Provider string
	Reason   string
	// RegionStep names the region-aware failover step of the
	// attempt that served this request (CHECKLIST 257-258):
	// "cross-region", "equivalent-model" or "self-hosted" (see
	// the Step* slugs in failover_region.go). "" when the first
	// target served — no failover step to name. The decision
	// header carries it as region_step, and only when non-empty.
	RegionStep string
	At         time.Time
}

// ProviderStats is the engine's live per-provider attempt record:
// real attempts only (hedge losers and limiter-shed calls are never
// counted), so SuccessRate and LatencyMs are measured, not assumed.
// Samples is Attempts; when it is 0 there is no signal yet and the
// other fields are 0. LatencyP95Ms is the true p95 from the latency
// histogram (B9), or -1 when insufficient samples (<20).
type ProviderStats struct {
	ProviderID   string
	Attempts     int
	Successes    int
	Failures     int
	SuccessRate  float64
	LatencyMs    float64 // EWMA of successful attempt latency (p50 proxy)
	LatencyP95Ms float64 // true p95 from histogram, -1 when unavailable
	Samples      int
}

// Engine routes chat requests across providers.
type Engine struct {
	clients   map[string]provider.Client
	clientsMu sync.RWMutex // guards clients: AddClient writes it while requests read it
	ledger    Ledger
	cfg       Config

	// tokMu guards the local-token-count reconciliation counters
	// (see TokenCountStats): compared/diverged attempts, counting
	// failures, and the most recent material divergence.
	tokMu             sync.Mutex
	tokCompared       uint64
	tokDiverged       uint64
	tokCountErrs      uint64
	lastDivergence    TokenDivergence
	hasLastDivergence bool

	// streamLifetimeKilled counts streams the lifetime cap
	// terminated since this engine started (CHECKLIST 255).
	// Atomic: runStream increments it from the stream goroutine
	// while the dashboard feed reads it. Read via
	// StreamLifetimeExceededCount.
	streamLifetimeKilled atomic.Uint64

	// ledgerRecordAttempts counts ledger RecordAttempt calls
	// since this engine started. Useful for monitoring ledger
	// health without depending on the ledger implementation.
	ledgerRecordAttempts atomic.Uint64

	mu       sync.Mutex
	breakers map[string]*breakerState
	// breakerStoreMu serialises whole breaker-store saves
	// (snapshot + write) so a stale snapshot can never rename
	// over a newer one (G11 Area 2, row 2 — see
	// saveBreakerStore). It is never held together with e.mu
	// by a caller: saves take it first, then e.mu briefly for
	// the snapshot, so no lock ordering can deadlock.
	breakerStoreMu sync.Mutex
	// windowBreakers is the live provider-level failure-window
	// breaker state (M4.2, providerbreaker.go): one entry per
	// provider with recent breaker-counted failures. Guarded by
	// e.mu, like breakers.
	windowBreakers map[string]*windowBreakerState
	// lockouts is the live per-(provider, key, model) lockout table
	// (M4.3). Nil-safe: NewModelLockout always builds one, and its
	// methods are no-ops until ModelLockout.Enabled is true.
	lockouts *ModelLockout

	// faultMu guards faults, the armed chaos-mode fault-injection
	// registry (B2-13, faultinject.go). Nil/empty means no fault
	// is armed: fault injection is OFF by default and can only
	// become active through an explicit ArmFault call
	// (test/operator control) — never from a user request.
	faultMu sync.RWMutex
	faults  []*faultEntry
	// ttft is the live per-provider time-to-first-byte record
	// (M4.7a): the rolling samples behind the predictive TTFT
	// breaker. Guarded by ttftMu.
	ttftMu sync.Mutex
	ttft   map[string]*ttftStats
	// breakerIgnored4xx counts, per provider, the 4xx failures
	// that never tripped the breaker (see isClientErrorFailure):
	// live proof for the dashboard that a bad key or a bad
	// request is not a provider-health signal.
	breakerIgnored4xx map[string]int
	cooldowns         map[string]time.Time
	// softCooldowns is the live soft-demotion state: one entry
	// per provider with a consecutive retryable-failure count
	// (see cooldown.go). A provider is demoted while its entry's
	// until is in the future.
	softCooldowns map[string]*softCooldownState
	// runtimeCooldownMode overrides cfg.CooldownMode when
	// non-empty (set via SetCooldownMode, e.g. from the
	// dashboard); "" means "use the configured mode".
	runtimeCooldownMode string
	// runtimeCooldownChains overrides the per-chain cooldown
	// mode for one model (set via SetChainCooldownMode); an
	// entry's "" value is never stored — clearing deletes it.
	runtimeCooldownChains map[string]string
	// quotaHints holds the remaining-quota fraction (0..100) for a
	// provider, learned from upstream rate-limit headers (see
	// learnRateLimit in ratelimit.go) or set by SetQuotaHint. A
	// learned entry expires at the upstream window reset (capped at
	// the 5-minute signal TTL): without the expiry a provider that
	// reported 0 remaining would be skipped forever, because a
	// skipped provider never answers again to teach a fresh
	// fraction. An entry with a zero expiresAt never expires —
	// that is the SetQuotaHint (operator) form.
	quotaHints map[string]quotaHintEntry
	// keyQuotaHints holds the same measured remaining-quota
	// fraction per (provider, key), keyed by ClientKey — the
	// per-key signal the account-rotation cap check reads
	// (orderAccountAttempts, B2-09 / the reference router #20561). Learned
	// from the same upstream responses as quotaHints (see
	// learnRateLimit in ratelimit.go): the engine knows which
	// key made each request, so the per-key fraction is the
	// same measurement at finer granularity, never a separate
	// invention. Same expiry semantics as quotaHints; restored
	// at startup from the persisted per-key learned signals
	// (RestoreLearnedKeyQuotaHint, called by main).
	keyQuotaHints map[string]quotaHintEntry
	// prefixFreezeObserve / prefixFreezeChecker carry the
	// process-wide PrefixFreeze observer wiring (row :939): the
	// observer lives in internal/degrade, which imports this
	// package, so the engine cannot name its type — main.go owns
	// the *degrade.PrefixFreeze and installs its Observe +
	// FreezeChecker here via SetPrefixFreezeHooks, once at
	// startup before the engine serves. Nil hooks mean no
	// observer is installed: Observe is skipped and
	// FrozenPrefix stays nil, so behaviour is unchanged.
	prefixFreezeObserve func([]provider.Message)
	prefixFreezeChecker func([]provider.Message) bool
	// quotaShare is the live quota-share DRR state (Phase D, D4):
	// one pool per chain signature, guarded by e.mu. See
	// orderQuotaShare (cascade.go).
	quotaShare map[string]*quotaSharePool
	// lastRelay is the context-relay state of the most recent
	// context-relay request (Phase D, D5), guarded by e.mu. It
	// is only ever replaced by a context-relay request, never
	// cleared by other strategies — like lastRouting. Read via
	// LastRelayStatus; see relay.go.
	lastRelay *relayState
	// relaySessions is the cross-request context-relay store
	// (Phase D, D5 — B2-01), guarded by e.mu: one pending relay
	// want per gateway key, keyed by req.CallerID. A request
	// that ends with a handoff wanted but no next attempt stores
	// the want here; the next context-relay request under the
	// same key preloads it, so the handoff is built and injected
	// by that request's first attempt. Entries expire after the
	// relay TTL; the store holds wants, never built handoffs.
	// See storeRelayWantIfPending / loadRelayWant in relay.go.
	relaySessions map[string]relaySessionEntry
	lastTrace     *routetrace.RouteTrace
	// lastAutoDecision is the routing decision of the most recent
	// auto-routed request (nil before any, or after a non-auto
	// request). Read via LastAutoDecision.
	lastAutoDecision *autorouter.Decision
	// lastRouting is the routing record of the most recent request
	// (strategy, model, serving provider). Read via LastRouting.
	lastRouting RoutingInfo
	// lastRoutingOK is false before any request has run.
	lastRoutingOK bool
	// lastContextFit is the fit-check decision of the most
	// recent request that reached the pre-call fit gate (see
	// contextfit.go). Read via LastContextFit; nil before any
	// request reached the gate.
	lastContextFit *ContextFitDecision
	// runtimeGlobal overrides cfg.Strategy when non-empty (set via
	// SetGlobalStrategy, e.g. from the dashboard); "" means "use
	// the configured global strategy".
	runtimeGlobal string
	// runtimeChains overrides the per-chain strategy for one model
	// (set via SetChainStrategy); an entry's "" value is never
	// stored — clearing deletes the entry.
	runtimeChains map[string]string
	// fallbackChains is the declarative per-model fallback chain
	// store (G11 Area 2, row 7): model → priority-sorted entries,
	// mutated by Set/DeleteFallbackChain, resolved by
	// FallbackChainFor. Guarded by e.mu. See fallback_chains.go.
	fallbackChains map[string][]FallbackEntry
	// attemptStats is the live per-provider attempt record behind
	// ProviderStats: real attempts only.
	attemptStats map[string]*providerStats
	// keyAttempts counts real attempts per (provider, key)
	// (ClientKey) behind the account-level "least-used"
	// fallback ordering (M3 Area 1, row 388): real attempts
	// only, like attemptStats. Guarded by e.mu.
	keyAttempts map[string]int
	// accountRR is the per-(provider, model) round-robin start
	// index behind the account-level "round-robin" fallback
	// ordering (row 388). Guarded by e.mu.
	accountRR map[string]int
	// accountDecks is the per-(provider, model) strict-random
	// deck state behind the account-level "strict-random"
	// fallback ordering (row 388). Guarded by e.mu.
	accountDecks map[string]*accountDeck

	// flights coalesces concurrent identical non-streaming
	// requests (internal/coalescing). Streaming never touches it.
	flights *coalescing.Group
	// coalescingMetrics, when set, receives one IncCoalescedShared
	// per waiter served from a shared flight. Nil disables it.
	coalescingMetrics CoalescingMetrics
	// usageMetrics, when set, receives token usage and upstream-
	// reported cost from recordAttempt — the engine's single
	// accounting point for both chat and streaming requests.
	usageMetrics UsageMetrics
	// breakerEvents, when set, receives genuine CLOSED->OPEN and
	// OPEN->CLOSED circuit-breaker transitions (e.g. the server's
	// webhook adapter). Nil disables notifications only.
	breakerEvents BreakerEvents

	// limiter is the per-(provider, key) adaptive concurrency
	// limiter (internal/adaptive, Gradient2). It caps in-flight
	// calls per provider from measured RTT, so a slowing provider
	// is shed BEFORE its queue grows and before 429s appear. It
	// FAILS OPEN: a nil limiter admits everything (the adaptive
	// methods are nil-safe), and admission is only ever refused
	// for a provider whose RTT has actually degraded. Streaming
	// attempts hold a slot for the stream's duration but never
	// feed the RTT estimate (a stream's wall time is not a
	// serving RTT); hedged streaming attempts are not
	// limiter-counted (see runStream).
	limiter *adaptive.Limiter
	// concurrencyMetrics, when set, receives the limiter's cap
	// per provider and one increment per shed call. Nil disables it.
	concurrencyMetrics ConcurrencyMetrics

	randMu sync.Mutex
	rand   *rand.Rand

	// refMu guards the live refusal-chain outcome record
	// (see RefusalChainStats): refusals seen, requests served
	// by a refusal-chain hop, requests that ended in an honest
	// refusal, and the most recent refusing attempt. A refusal
	// never touches attemptStats / breakers / cooldowns, so it
	// has its own record — the health signals stay clean.
	refMu           sync.Mutex
	refRefusals     uint64
	refServed       uint64
	refExhausted    uint64
	refLastProvider string
	refLastModel    string
	refLastAt       time.Time
	// refusalMetrics, when set, receives one increment per
	// refusal, per refusal-chain serve and per exhausted chain.
	// Nil disables reporting.
	refusalMetrics RefusalMetrics

	// fail243Mu guards the CHECKLIST 243 pin-escape record (see
	// Failover243Stats): requests that escaped their pin after a
	// retryable failure on the pinned deployment, and the most
	// recent escape. Counted by the attempt loops (see
	// notePinEscape), never by the trace — a request that never
	// reaches the loop (contract rejection, empty chain) cannot
	// escape a pin it never tried.
	fail243Mu            sync.Mutex
	fail243PinEscapes    uint64
	fail243LastPin       string
	fail243LastEscapedTo string
	fail243LastEscapeAt  time.Time
	// fail243ToolReplays counts tool-call deltas dropped by the
	// resumed-stream no-replay filter (see
	// noteToolReplaysSuppressed).
	fail243ToolReplays uint64

	// fail257Mu guards the CHECKLIST 257 region-aware failover
	// record (see Failover257Stats): requests that stepped to
	// each region tier after a retryable failure, and the most
	// recent step. Counted by the attempt loops (see
	// noteRegionFailoverFromTrace), never by the trace — a
	// request that never reaches the loop cannot step.
	fail257Mu           sync.Mutex
	fail257CrossRegion  uint64
	fail257Equivalent   uint64
	fail257SelfHosted   uint64
	fail257LastStep     string
	fail257LastProvider string
	fail257LastModel    string
	fail257LastAt       time.Time

	// panicMetrics, when set, receives one increment per panic
	// recovered at a request/stream boundary (see panic.go).
	// Nil disables reporting — a panic is still recovered and
	// the request still answered, just not counted.
	panicMetrics PanicMetrics

	// reasoning is the in-memory reasoning-replay store
	// (CHECKLIST 163-164, see reasoning.go): captured reasoning
	// states keyed by conversation transcript, TTL 2h, cap 200.
	// Nil disables capture/replay — every helper in reasoning.go
	// nil-checks it first. (Restored: the field was missing
	// after a concurrent edit to this file landed without it;
	// without it the package does not compile. Initialized in
	// NewEngine with the documented defaults.)
	reasoning *reasoning.Store
}

// NewEngine builds an Engine. clients is keyed by provider ID; the map
// is copied. ledger may be nil (no usage accounting).
func NewEngine(clients map[string]provider.Client, ledger Ledger, cfg Config) *Engine {
	cfg = cfg.withDefaults()
	cp := make(map[string]provider.Client, len(clients))
	for k, v := range clients {
		cp[k] = v
	}
	eng := &Engine{
		clients:               cp,
		ledger:                ledger,
		cfg:                   cfg,
		breakers:              make(map[string]*breakerState),
		windowBreakers:        make(map[string]*windowBreakerState),
		lockouts:              NewModelLockout(cfg.ModelLockout),
		ttft:                  make(map[string]*ttftStats),
		breakerIgnored4xx:     make(map[string]int),
		cooldowns:             make(map[string]time.Time),
		softCooldowns:         make(map[string]*softCooldownState),
		runtimeCooldownChains: make(map[string]string),
		quotaHints:            make(map[string]quotaHintEntry),
		keyQuotaHints:         make(map[string]quotaHintEntry),
		quotaShare:            make(map[string]*quotaSharePool),
		runtimeChains:         make(map[string]string),
		fallbackChains:        make(map[string][]FallbackEntry),
		attemptStats:          make(map[string]*providerStats),
		keyAttempts:           make(map[string]int),
		accountRR:             make(map[string]int),
		accountDecks:          make(map[string]*accountDeck),
		flights:               coalescing.New(),
		limiter:               limiterFor(cfg),
		reasoning:             reasoning.New(),
		rand:                  rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	// G11 Area 2: restore persisted breaker state when a store
	// path is configured (row 2) — a missing/unreadable file
	// starts clean; see breaker_store.go.
	eng.loadBreakerStore()
	return eng
}

// limiterFor builds the engine's adaptive limiter, or nil when
// Config.DisableAdaptiveLimiter is set. A nil limiter is the
// documented fail-open stub: TryAcquire always admits, Record
// no-ops, Snapshot is empty (see internal/adaptive).
func limiterFor(cfg Config) *adaptive.Limiter {
	if cfg.DisableAdaptiveLimiter {
		return nil
	}
	return adaptive.New(adaptive.Config{})
}

// SetCoalescingMetrics wires the metrics sink that counts shared
// waiters (e.g. *metrics.Recorder). A nil value disables reporting.
// Safe to call after NewEngine; the engine reads the field on every
// shared result, so wiring it later still counts later flights.
func (e *Engine) SetCoalescingMetrics(m CoalescingMetrics) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.coalescingMetrics = m
}

// SetUsageMetrics wires the metrics sink for per-attempt token usage and
// upstream-reported cost (e.g. *metrics.Recorder). A nil value disables
// reporting only; the ledger still records every attempt. Safe to call
// after NewEngine — recordAttempt reads the field on every attempt.
func (e *Engine) SetUsageMetrics(m UsageMetrics) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.usageMetrics = m
}

// SetBreakerEvents wires an edge-triggered provider-health sink (e.g.
// the server's webhook adapter). A nil value disables notifications
// only; breakers still open, probe and recover exactly as before.
func (e *Engine) SetBreakerEvents(s BreakerEvents) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.breakerEvents = s
}

// notifyDegradedLocked reports a CLOSED->OPEN (or HALF_OPEN->OPEN)
// transition to the optional sink. Called under e.mu, so it copies
// the numbers and dispatches asynchronously — no sink can re-enter
// the engine and deadlock.
func (e *Engine) notifyDegradedLocked(provider string, st *breakerState, wasOpen, wasProbe bool) {
	if e.breakerEvents == nil || (wasOpen && !wasProbe) {
		return
	}
	sink := e.breakerEvents
	n, trips, openFor := st.consecutiveFailures, st.trips, st.openFor
	go sink.ProviderDegraded(provider, n, trips, openFor)
}

// SetConcurrencyMetrics wires the metrics sink that counts shed
// calls and records the adaptive cap per provider (e.g.
// *metrics.Recorder). A nil value disables reporting. Safe to call
// after NewEngine.
func (e *Engine) SetConcurrencyMetrics(m ConcurrencyMetrics) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.concurrencyMetrics = m
}

// SetPanicMetrics wires the metrics sink that counts recovered
// panics, one increment per panic at the boundary that caught it
// (e.g. *metrics.Recorder; see PanicMetrics in panic.go). A nil
// value disables reporting. Safe to call after NewEngine; the
// engine reads the field on every recovered panic, so wiring it
// later still counts later panics.
func (e *Engine) SetPanicMetrics(m PanicMetrics) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.panicMetrics = m
}

// notePanic counts one recovered panic at the named location in
// the metrics sink, if any. Best-effort: it never fails a request.
func (e *Engine) notePanic(location string) {
	e.mu.Lock()
	m := e.panicMetrics
	e.mu.Unlock()
	if m != nil {
		m.IncPanic(location)
	}
}

// ConcurrencyStats returns a snapshot of the per-(provider, key)
// adaptive limiter: current cap, in-flight count, smoothed RTT vs
// long-term baseline, samples, admitted and shed counts. Providers
// with no traffic have no entry — an empty list before any request
// is the honest empty state, never invented limits.
func (e *Engine) ConcurrencyStats() []adaptive.State {
	return e.limiter.Snapshot()
}

// reportConcurrencyLimit pushes one provider's current cap to the
// metrics sink, if any. Best-effort: it never fails a request.
func (e *Engine) reportConcurrencyLimit(providerID string) {
	e.mu.Lock()
	m := e.concurrencyMetrics
	e.mu.Unlock()
	if m != nil {
		m.SetConcurrencyLimit(providerID, e.limiter.Limit(providerID))
	}
}

// noteConcurrencyRejected counts one call shed by the adaptive
// limiter, in the limiter and (when wired) in metrics.
func (e *Engine) noteConcurrencyRejected(providerID string) {
	e.mu.Lock()
	m := e.concurrencyMetrics
	e.mu.Unlock()
	if m != nil {
		m.IncConcurrencyRejected(providerID)
	}
}

// CoalescingStats returns a snapshot of the single-flight counters
// (real counts only — zero until identical requests actually share a
// flight).
func (e *Engine) CoalescingStats() coalescing.Stats {
	if e.flights == nil {
		return coalescing.Stats{}
	}
	return e.flights.Stats()
}

// noteShared records one waiter served from a shared flight: one
// metrics increment, and — when the ledger supports it — one shared
// ledger row (zero tokens, zero cost, marked shared, so usage
// accounting stays honest: the leader's attempt is the only one
// billed). It is called only for waiters that actually received the
// leader's result. The serving provider/model come from the leader's
// route trace (its last attempt is the winner on success, the last
// tried on failure); with no attempt in the trace (e.g. an empty
// chain) there is nothing to attribute and no row is recorded.
// latency is the waiter's wait time, not provider latency.
func (e *Engine) noteShared(req provider.ChatRequest, res coalescing.Result, waited time.Duration) {
	e.mu.Lock()
	m := e.coalescingMetrics
	e.mu.Unlock()
	if m != nil {
		m.IncCoalescedShared()
	}
	sl, ok := e.ledger.(SharedLedger)
	if !ok || sl == nil {
		return
	}
	if res.Trace == nil || len(res.Trace.Attempts) == 0 {
		return
	}
	last := res.Trace.Attempts[len(res.Trace.Attempts)-1]
	sl.RecordShared(last.Provider, last.Model, res.Err == nil, waited.Milliseconds())
}

// quotaHintEntry is one remaining-quota fraction for a provider
// (quotaHints) or one (provider, key) pair (keyQuotaHints).
// hint is 0..100 (0 = no quota left, the provider is skipped).
// expiresAt is when the learned fraction stops being trustworthy;
// zero means it never expires (the operator-set form).
type quotaHintEntry struct {
	hint      int
	expiresAt time.Time
}

// SetQuotaHint overrides the remaining-quota hint for one provider.
// A hint of 0 means "no quota left": the provider is skipped.
// Overrides take precedence over Config.QuotaHint. An override set
// here never expires; learned fractions (setLearnedQuotaHint) do.
func (e *Engine) SetQuotaHint(providerID string, hint int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.quotaHints[providerID] = quotaHintEntry{hint: hint}
}

// SetPrefixFreezeHooks installs the process-wide PrefixFreeze
// observer wiring (row :939): observe is the observer's Observe
// (one sighting of a request's system prompt), checker is its
// FreezeChecker (reports whether the prompt is frozen). Both come
// from the same *degrade.PrefixFreeze, owned by main.go — this
// package cannot name its type (internal/degrade imports this
// package). Install once at startup, before the engine serves;
// the hooks are read on the request path without a lock. Either
// may be nil: a nil observe means no observation, a nil checker
// means FrozenPrefix stays nil, and behaviour is unchanged
// until the observer is installed.
func (e *Engine) SetPrefixFreezeHooks(observe func([]provider.Message), checker func([]provider.Message) bool) {
	e.prefixFreezeObserve = observe
	e.prefixFreezeChecker = checker
}

// ObservePrefix records one sighting of the system prompt in msgs
// with the installed PrefixFreeze observer. No-op when no observer
// is installed. Exported for the server lane: the server handler's
// applyCompression observes server-routed requests the same way —
// one Observe per request, at the first compression point — so a
// request is never counted twice (see maybeCompress).
func (e *Engine) ObservePrefix(msgs []provider.Message) {
	if e.prefixFreezeObserve != nil {
		e.prefixFreezeObserve(msgs)
	}
}

// PrefixFreezeChecker returns the installed PrefixFreeze predicate
// for compress.ApplyOptions.FrozenPrefix (nil when no observer is
// installed — Apply treats a nil FrozenPrefix as absent).
// Exported for the server lane: applyCompression passes it the
// same way maybeCompress does.
func (e *Engine) PrefixFreezeChecker() func([]provider.Message) bool {
	return e.prefixFreezeChecker
}

// AnyCachingProvider reports whether any provider client configured
// on this engine is known to honour a prompt-cache marker.
// Exported for the server lane: applyCompression supplies the same
// IsCachingProvider to its ApplyWithOptions call.
func (e *Engine) AnyCachingProvider() bool {
	return e.anyCachingProvider()
}

// addTraceAttempt appends one attempt to a trace. It exists because
// RouteTrace.Add takes plain provider/model strings (the trace package
// cannot import router.Attempt), while call sites here already have the
// Attempt in hand. The fallback reason comes from the error taxonomy
// (provider.ClassifyError, chat-completions surface): a failed
// attempt carries its reason slug ("model_not_found",
// "rate_limited", "request_invalid", ...) so the dashboard's Live
// Routing view can say why the chain moved on — or why it stopped.
// Successes and skips carry no reason (err is nil there).
func addTraceAttempt(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error) {
	addTraceAttemptPreread(t, att, latencyMs, outcome, err, prereadInfo{})
}

// traceReason is the dashboard fallback reason for one failed
// attempt. A recovered panic gets its own slug
// (ReasonPanicRecovered) instead of the taxonomy default
// ("transport_error"): the taxonomy cannot know the type (it
// cannot import router), and "connection failed" would be a lie
// about a provider client that panicked. An async guardrail cut
// gets its own slug the same way (guardrails.AsyncBlockReason):
// the taxonomy would read a cancelled stream as a transport
// error, and a policy cut is not a provider failure. Everything
// else goes through provider.ClassifyError as before.
func traceReason(err error) string {
	if err == nil {
		return ""
	}
	var pe *PanicError
	if errors.As(err, &pe) {
		return provider.ReasonPanicRecovered
	}
	var blk *guardrails.AsyncBlockError
	if errors.As(err, &blk) {
		return guardrails.AsyncBlockReason
	}
	return provider.ClassifyError(err, provider.EndpointChatCompletions).Reason
}

// addTraceAttemptPreread is addTraceAttempt plus the preread-buffer
// record for the attempt (pr.state "", 0, 0 when the buffer did not
// run or held nothing).
func addTraceAttemptPreread(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error, pr prereadInfo) {
	addTraceAttemptStream(t, att, latencyMs, outcome, err, pr, "")
}

// addTraceAttemptStream is addTraceAttemptPreread plus the stream
// terminal record: how this attempt's stream ended for the client
// ("done" | "error" | "cancelled", "" when the attempt never
// reached a terminal state for the client — a pre-commit failover
// or a non-streaming attempt; see routetrace.AttemptTrace.Terminal).
func addTraceAttemptStream(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error, pr prereadInfo, terminal string) {
	addTraceAttemptStreamReasoning(t, att, latencyMs, outcome, err, pr, terminal, "")
}

// addTraceAttemptReasoning is addTraceAttempt plus the
// reasoning-handoff label for the attempt ("" when the request
// carried no reasoning state — see AttemptTrace.Reasoning).
func addTraceAttemptReasoning(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error, reasoning string) {
	addTraceAttemptPrereadReasoning(t, att, latencyMs, outcome, err, prereadInfo{}, reasoning)
}

// addTraceAttemptPrereadReasoning is addTraceAttemptReasoning plus
// the preread-buffer record for the attempt.
func addTraceAttemptPrereadReasoning(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error, pr prereadInfo, reasoning string) {
	addTraceAttemptStreamReasoning(t, att, latencyMs, outcome, err, pr, "", reasoning)
}

// addTraceAttemptStreamReasoning is addTraceAttemptStream plus the
// reasoning-handoff label for the attempt.
func addTraceAttemptStreamReasoning(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error, pr prereadInfo, terminal, reasoning string) {
	t.AddWithPrereadTerminalReasoning(att.ProviderID, att.Model, latencyMs, outcome, err, traceReason(err), pr.state, pr.events, pr.bytes, nil, terminal, reasoning)
	stampTraceKey(t, att.KeyID)
	stampTraceEffort(t, att.Effort)
	stampTraceEffortForwarding(t, att.EffortForwarding)
	stampTraceEffortPolicy(t, att.EffortPolicy)
}

// addTraceAttemptRetryReasoning is addTraceAttemptRetry plus the
// reasoning-handoff label for the attempt.
func addTraceAttemptRetryReasoning(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error, retry *routetrace.RetryAfterInfo, reasoning string) {
	addTraceAttemptPrereadRetryReasoning(t, att, latencyMs, outcome, err, prereadInfo{}, retry, reasoning)
}

// addTraceAttemptPrereadRetryReasoning is
// addTraceAttemptRetryReasoning plus the preread-buffer record.
func addTraceAttemptPrereadRetryReasoning(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error, pr prereadInfo, retry *routetrace.RetryAfterInfo, reasoning string) {
	addTraceAttemptPrereadRetryTerminalReasoning(t, att, latencyMs, outcome, err, pr, retry, "", reasoning)
}

// addTraceAttemptPrereadRetryTerminalReasoning is
// addTraceAttemptPrereadRetryReasoning plus the stream terminal
// record.
func addTraceAttemptPrereadRetryTerminalReasoning(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error, pr prereadInfo, retry *routetrace.RetryAfterInfo, terminal, reasoning string) {
	t.AddWithPrereadTerminalReasoning(att.ProviderID, att.Model, latencyMs, outcome, err, traceReason(err), pr.state, pr.events, pr.bytes, retry, terminal, reasoning)
	stampTraceKey(t, att.KeyID)
	stampTraceEffort(t, att.Effort)
	stampTraceEffortForwarding(t, att.EffortForwarding)
	stampTraceEffortPolicy(t, att.EffortPolicy)
}

// stampTraceEffortPolicy records the forced-reasoning policy
// decision for the attempt just added to the trace (CHECKLIST
// 311-312): "forced:<effort>:policy:<model>" when the policy
// won, "client-override:<effort>:policy:<model>" when the
// client's value won under allow_client_override. Both trace
// funnels above reach this helper, so every attempt entry
// carries its policy decision — "" when no policy matched —
// and the dashboard's Live Routing view can show which effort
// was forced and by which policy. It mirrors
// stampTraceEffortForwarding exactly.
func stampTraceEffortPolicy(t *routetrace.RouteTrace, label string) {
	if label == "" || len(t.Attempts) == 0 {
		return
	}
	t.Attempts[len(t.Attempts)-1].EffortPolicy = label
}

// stampTraceEffortForwarding records whether the attempt's
// effort was forwarded to the upstream, and with what value
// (CHECKLIST 285-286). Both trace funnels above reach this
// helper, so every attempt entry carries its forwarding label —
// "" when the attempt ran at no explicit effort — and the
// dashboard's Live Routing view can show per wire whether the
// tier's effort reached the provider. It mirrors
// stampTraceEffort exactly.
func stampTraceEffortForwarding(t *routetrace.RouteTrace, label string) {
	if label == "" || len(t.Attempts) == 0 {
		return
	}
	t.Attempts[len(t.Attempts)-1].EffortForwarding = label
}

// stampTraceEffort records the reasoning effort that served the
// attempt just added to the trace (CHECKLIST 255). Both trace
// funnels above reach this helper, so every attempt entry carries
// its effort — "" when the attempt ran at no explicit effort —
// and the dashboard's Live Routing view can show which effort
// served, or failed, each hop. It mirrors stampTraceKey exactly.
func stampTraceEffort(t *routetrace.RouteTrace, effort string) {
	if effort == "" || len(t.Attempts) == 0 {
		return
	}
	t.Attempts[len(t.Attempts)-1].Effort = effort
}

// stampTraceKey records which provider-side key served the attempt
// just added to the trace (CHECKLIST 175). Both trace funnels
// above reach this helper, so every attempt entry carries its key
// — "" for the legacy single key, a "key_…" ID for a pool key.
func stampTraceKey(t *routetrace.RouteTrace, keyID string) {
	if keyID == "" || len(t.Attempts) == 0 {
		return
	}
	t.Attempts[len(t.Attempts)-1].KeyID = keyID
}

// retryInfoFor builds the trace's Retry-After record for one
// rate-limited attempt from the hint the upstream sent: the raw
// seconds, the abandon/floor/cap/honour decision, and the wait the
// retry path actually used (0 for an abandoned target). Nil when
// the attempt carried no hint — an absent hint is recorded as
// nothing, never as a 0s hint.
func retryInfoFor(retryAfter time.Duration, present bool, cap time.Duration) *routetrace.RetryAfterInfo {
	if !present {
		return nil
	}
	wait, decision, _ := classifyRetryAfter(retryAfter, present, cap)
	return &routetrace.RetryAfterInfo{
		Seconds:     retryAfter.Seconds(),
		Decision:    decision,
		WaitSeconds: wait.Seconds(),
	}
}

// addTraceAttemptRetry is addTraceAttempt for a rate-limited
// attempt: it carries the Retry-After record (see retryInfoFor)
// so the dashboard's Live Routing view can show the hint seen
// and the abandon/floor decision honestly, per attempt.
func addTraceAttemptRetry(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error, retry *routetrace.RetryAfterInfo) {
	addTraceAttemptPrereadRetry(t, att, latencyMs, outcome, err, prereadInfo{}, retry)
}

// addTraceAttemptPrereadRetry is addTraceAttemptPreread plus the
// Retry-After record for the attempt.
func addTraceAttemptPrereadRetry(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error, pr prereadInfo, retry *routetrace.RetryAfterInfo) {
	addTraceAttemptPrereadRetryTerminal(t, att, latencyMs, outcome, err, pr, retry, "")
}

// addTraceAttemptPrereadRetryTerminal is
// addTraceAttemptPrereadRetry plus the stream terminal record
// (see addTraceAttemptStream).
func addTraceAttemptPrereadRetryTerminal(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error, pr prereadInfo, retry *routetrace.RetryAfterInfo, terminal string) {
	addTraceAttemptPrereadRetryTerminalReasoning(t, att, latencyMs, outcome, err, pr, retry, terminal, "")
}

// isTerminalErr reports whether err stops the fallback chain: the
// failure is a property of the request (malformed, forbidden,
// unrepresentable) or the caller, so no other target could serve
// it. See provider.ClassifyError.
func isTerminalErr(err error) bool {
	return provider.ClassifyError(err, provider.EndpointChatCompletions).Decision == provider.DecisionStop
}

// isAuthFailedErr reports whether err is a 401: the key that served
// the attempt is rejected. A 401 is a property of that key, not of
// the request — another key of the same provider may serve it.
func isAuthFailedErr(err error) bool {
	return provider.ClassifyError(err, provider.EndpointChatCompletions).Reason == provider.ReasonAuthFailed
}

// deadKeyCooldown is how long a 401'd pool key cools in hard mode
// (CHECKLIST 175): the key was rejected, so re-probing it on the
// next request would only burn an attempt; 5 minutes is long
// enough to let the operator rotate it (a rotated key is a new key
// ID, so it is never cooled) and short enough that a transient
// auth outage heals by itself. In soft-demote mode the same key
// cools on the doubling ladder instead (see coolDeadKey).
const deadKeyCooldown = 5 * time.Minute

// coolDeadKey cools one dead (401) pool key, per-key (CHECKLIST
// 175): the key is skipped (hard) or demoted (soft) while the
// provider's other keys keep serving. A legacy single-key attempt
// (KeyID "") cools nothing — there is no sibling key to fall back
// to, and cooling it would hide the real 401 behind a "no
// attempts available" on the next request.
func (e *Engine) coolDeadKey(att Attempt, mode string) {
	if att.KeyID == "" {
		return
	}
	key := ClientKey(att.ProviderID, att.KeyID)
	if NormalizeCooldownMode(mode) == CooldownSoftDemote {
		e.startSoftCooldown(key, 0, false)
	} else {
		e.setCooldown(key, deadKeyCooldown)
	}
}

// hasSiblingKeyAttempt reports whether the chain still holds an
// untried attempt on the same provider under a different key,
// starting at index from. The loop only ever moves forward through
// active, so everything from `from` onward is untried in this pass.
func hasSiblingKeyAttempt(active []Attempt, from int, att Attempt) bool {
	if att.KeyID == "" {
		return false
	}
	for i := from; i < len(active); i++ {
		if active[i].ProviderID == att.ProviderID && active[i].KeyID != "" && active[i].KeyID != att.KeyID {
			return true
		}
	}
	return false
}

// isTerminalErrFor is isTerminalErr with the multi-key exception
// (CHECKLIST 175): a 401 is terminal only when no sibling key of
// the same provider remains untried — the dead key's siblings may
// still serve the request. Every other terminal failure stops the
// chain exactly as before.
func isTerminalErrFor(err error, active []Attempt, from int, att Attempt) bool {
	if !isTerminalErr(err) {
		return false
	}
	if isAuthFailedErr(err) && hasSiblingKeyAttempt(active, from, att) {
		return false
	}
	return true
}

// isModelNotFoundErr reports whether err means the model does not
// exist at that target (400/404 with a model-not-found signal).
// The provider itself answered fine — only the model is dead —
// so the failure must not count against the provider's circuit
// breaker (a dead model is not a dead provider; per-model lockout
// is a separate, unbuilt slice).
func isModelNotFoundErr(err error) bool {
	cls := provider.ClassifyError(err, provider.EndpointChatCompletions)
	return cls.Reason == provider.ReasonModelNotFound || cls.Reason == provider.ReasonNotFound
}

// isCapabilityMismatchErr reports whether err is a 400 whose body
// signals the target model does not support tool / function calls
// (CT-1, GAP-TRACK): a capability mismatch, a property of that
// target model — not of the request, which another model may
// serve. Only a 400 counts (the taxonomy already stops the chain
// on any other 400 as request_invalid), and only when the body
// actually names tool/function-call support — a 400 about
// temperature, max_tokens or any other field is not a mismatch
// and stays terminal. The match is a case-insensitive substring
// over the error text (which carries the upstream body, truncated
// at 200 chars by UpstreamError), so a wire error naming the
// same refusal counts the same way.
func isCapabilityMismatchErr(err error) bool {
	if err == nil {
		return false
	}
	if provider.ClassifyError(err, provider.EndpointChatCompletions).Status != 400 {
		return false
	}
	text := strings.ToLower(err.Error())
	tool := strings.Contains(text, "tool")
	fnCall := strings.Contains(text, "function call")
	if !tool && !fnCall {
		return false
	}
	return strings.Contains(text, "not support") ||
		strings.Contains(text, "doesn't support") ||
		strings.Contains(text, "unsupported")
}

// modelToolsCapable reports one attempt target's tool capability
// via Config.ToolCapability: (capable, known). A nil hook means
// every model is unknown — the pre-check skips nothing and the
// mismatch failover finds no confirmed-capable target.
func (e *Engine) modelToolsCapable(providerID, model string) (capable, known bool) {
	if e.cfg.ToolCapability == nil {
		return false, false
	}
	return e.cfg.ToolCapability(providerID, model)
}

// modelVisionCapable reports one attempt target's vision
// capability via Config.VisionCapability: (capable, known). A nil
// hook means every model is unknown — the modality bridge never
// fires.
func (e *Engine) modelVisionCapable(providerID, model string) (capable, known bool) {
	if e.cfg.VisionCapability == nil {
		return false, false
	}
	return e.cfg.VisionCapability(providerID, model)
}

// modelModalityCapable reports one attempt target's full modality
// capability record via Config.ModalityCapability: the model's
// Vision/Audio/Video flags with their Known markers, plus whether
// the registry knows the model at all. A nil hook means unknown —
// the bridge falls back to the vision-only hook.
func (e *Engine) modelModalityCapable(providerID, model string) (provider.Model, bool) {
	if e.cfg.ModalityCapability == nil {
		return provider.Model{}, false
	}
	return e.cfg.ModalityCapability(providerID, model)
}

// capabilityFailoverIndex returns the index of the first attempt
// at or after from whose model is CONFIRMED tool-capable
// (known && capable), or -1 when the chain holds no such target.
// Unknown models do not count: the single capability failover
// goes only to a target known to serve tools, never to another
// guess that may 400 the same way.
func (e *Engine) capabilityFailoverIndex(active []Attempt, from int) int {
	for i := from; i < len(active); i++ {
		if capable, known := e.modelToolsCapable(active[i].ProviderID, active[i].Model); known && capable {
			return i
		}
	}
	return -1
}

// LastTrace returns the RouteTrace of the most recently finished
// request (Chat or ChatStream). It is nil before any request.
func (e *Engine) LastTrace() *routetrace.RouteTrace {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastTrace
}

// LastAutoDecision returns the routing decision of the most recent
// auto-routed request, and whether there is one. It is nil/false
// before any auto-routed request, and after a non-auto request (a
// plain model name clears it, so a stale decision can never be
// mistaken for the current request's). The returned decision is a
// copy; mutating it does not affect the engine.
func (e *Engine) LastAutoDecision() (autorouter.Decision, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastAutoDecision == nil {
		return autorouter.Decision{}, false
	}
	return *e.lastAutoDecision, true
}

func (e *Engine) setLastAutoDecision(dec *autorouter.Decision) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastAutoDecision = dec
}

// resolveAutoAlias parses a per-request "auto:<axis>" model alias
// (research-log entry 156): the axis names the strategy for this
// one request, and the model is rewritten to the matching "auto/*"
// virtual model so resolveAuto can still resolve a concrete model.
// It returns the axis (lowercased, "" when the model is not such an
// alias) and the rewritten request.
//
// The axis -> virtual-model mapping must match
// internal/strategy.AxisVirtualModel (the router cannot import that
// package — it would be a cycle — so the mapping is duplicated here
// and pinned by TestResolveAutoAlias): cheapest -> auto/cheap,
// fastest and least-latency -> auto/fast, most-reliable ->
// auto/reasoning, code -> auto/code, anything else (balanced,
// weighted, or an unknown axis) -> auto. An unknown axis still
// rewrites to "auto": the StrategyHook factory ranks unknown names
// as balanced, so the request never errors on the alias — it just
// gets the balanced strategy for the auto model.
func (e *Engine) resolveAutoAlias(req provider.ChatRequest) (string, provider.ChatRequest) {
	model := strings.TrimSpace(req.Model)
	if len(model) <= 5 || !strings.EqualFold(model[:5], "auto:") {
		return "", req
	}
	axis := strings.ToLower(strings.TrimSpace(model[5:]))
	if axis == "" {
		return "", req
	}
	switch axis {
	case "cheapest", "cheap":
		req.Model = "auto/cheap"
	case "fastest", "fast", "least-latency", "latency":
		req.Model = "auto/fast"
	case "most-reliable", "reliable":
		req.Model = "auto/reasoning"
	case "code":
		req.Model = "auto/code"
	default:
		req.Model = "auto"
	}
	return axis, req
}

// effectiveStrategy resolves the strategy for one request.
// Precedence (highest first): the per-request "auto:<axis>" alias,
// a runtime per-chain override (SetChainStrategy), the configured
// ChainStrategy for the model, a runtime global override
// (SetGlobalStrategy), the configured global Strategy, and finally
// "priority". The returned name is passed verbatim to
// Config.StrategyHook, which ranks unknown names as priority.
func (e *Engine) effectiveStrategy(reqStrategy, model string) string {
	if reqStrategy != "" {
		return reqStrategy
	}
	e.mu.Lock()
	runtimeChain, hasRuntimeChain := e.runtimeChains[model]
	runtimeGlobal := e.runtimeGlobal
	e.mu.Unlock()
	if hasRuntimeChain && runtimeChain != "" {
		return runtimeChain
	}
	if e.cfg.ChainStrategy != nil {
		if s := e.cfg.ChainStrategy(model); s != "" {
			return s
		}
	}
	if runtimeGlobal != "" {
		return runtimeGlobal
	}
	if e.cfg.Strategy != "" {
		return e.cfg.Strategy
	}
	return "priority"
}

// SetGlobalStrategy overrides the global strategy at runtime (e.g.
// from the dashboard). "" clears the override and restores the
// configured Config.Strategy. The value is stored verbatim and
// resolved by effectiveStrategy; unknown names rank as balanced.
func (e *Engine) SetGlobalStrategy(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runtimeGlobal = strings.TrimSpace(s)
}

// SetChainStrategy overrides the strategy for one chain (model) at
// runtime. "" clears the override for that model (it inherits again).
// Unknown strategy names are stored verbatim and rank as balanced.
func (e *Engine) SetChainStrategy(model, strategy string) {
	if model == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if strings.TrimSpace(strategy) == "" {
		delete(e.runtimeChains, model)
		return
	}
	e.runtimeChains[model] = strings.TrimSpace(strategy)
}

// StrategyState returns the runtime strategy overrides: the runtime
// global ("" when unset — the configured global applies) and a copy
// of the runtime per-chain overrides. Configured values (from
// Config) are not included: this is the mutable state only, so the
// dashboard can tell what the operator changed live.
func (e *Engine) StrategyState() (global string, chains map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	chains = make(map[string]string, len(e.runtimeChains))
	for k, v := range e.runtimeChains {
		chains[k] = v
	}
	return e.runtimeGlobal, chains
}

// LastRouting returns the routing record of the most recent request
// (strategy, model, serving provider) and whether any request has
// run yet. The returned value is a copy.
func (e *Engine) LastRouting() (RoutingInfo, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastRouting, e.lastRoutingOK
}

// recordRouting stores the routing record for a request that just
// started (provider "" until a provider serves it).
func (e *Engine) recordRouting(strategy, model string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastRouting = RoutingInfo{Strategy: strategy, Model: model, At: time.Now()}
	e.lastRoutingOK = true
}

// recordRoutingProvider fills in the serving provider of the most
// recent request (called on success). It only touches the record
// when it belongs to the same strategy+model pair, so a concurrent
// request cannot misattribute the winner.
func (e *Engine) recordRoutingProvider(strategy, model, providerID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.lastRoutingOK || e.lastRouting.Strategy != strategy || e.lastRouting.Model != model {
		return
	}
	e.lastRouting.Provider = providerID
}

// noteAttemptStats records one real upstream attempt in the live
// per-provider stats: successes feed the latency EWMA (alpha 0.2),
// failures count against the success rate. It also counts the
// attempt per (provider, key) in keyAttempts — the live record
// behind the account-level "least-used" fallback ordering (M3
// Area 1, row 388). Called only for attempts that actually
// reached a provider (called=true) and were not cancelled by the
// caller or by the engine's own deadline — a cancellation says
// nothing about the provider.
func (e *Engine) noteAttemptStats(providerID, keyID string, ok bool, latencyMs int64) {
	if providerID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.attemptStats[providerID]
	if st == nil {
		st = &providerStats{}
		e.attemptStats[providerID] = st
	}
	st.attempts++
	e.keyAttempts[ClientKey(providerID, keyID)]++
	if !ok {
		return
	}
	st.successes++
	lat := float64(latencyMs)
	if st.successes == 1 {
		st.latencyEWMA = lat
	} else {
		st.latencyEWMA = 0.8*st.latencyEWMA + 0.2*lat
	}
	// Record in latency histogram for true p95 (B9).
	bucket := latencyBucket(latencyMs)
	if bucket >= 0 && bucket < len(st.latencyHist) {
		st.latencyHist[bucket]++
	}
}

// latencyBucket returns the histogram bucket index for a latency in ms.
// Buckets 0-1000: 1 ms each (0-1000 ms).
// Buckets 1001-1099: 10 ms each (1010-10000 ms).
func latencyBucket(latencyMs int64) int {
	if latencyMs <= 1000 {
		return int(latencyMs)
	}
	if latencyMs <= 10000 {
		return 1001 + int((latencyMs-1010)/10)
	}
	return 1099 // cap at max bucket
}

// p95LatencyMs returns the 95th-percentile latency from the histogram,
// or -1 if insufficient samples.
func (st *providerStats) p95LatencyMs() float64 {
	total := 0
	for _, c := range st.latencyHist {
		total += c
	}
	if total < 20 { // need at least 20 samples for meaningful p95
		return -1
	}
	target := int(float64(total) * 0.95)
	sum := 0
	for i, c := range st.latencyHist {
		sum += c
		if sum >= target {
			if i <= 1000 {
				return float64(i)
			}
			return float64(1010 + (i-1001)*10)
		}
	}
	return -1
}

// ProviderStats returns the live attempt record for one provider,
// and whether it has any recorded attempts yet. No attempts means
// no signal — the caller must treat that as unknown, never as
// fast/reliable.
func (e *Engine) ProviderStats(providerID string) (ProviderStats, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.attemptStats[providerID]
	if st == nil || st.attempts == 0 {
		return ProviderStats{ProviderID: providerID}, false
	}
	return ProviderStats{
		ProviderID:   providerID,
		Attempts:     st.attempts,
		Successes:    st.successes,
		Failures:     st.attempts - st.successes,
		SuccessRate:  float64(st.successes) / float64(st.attempts),
		LatencyMs:    st.latencyEWMA,
		LatencyP95Ms: st.p95LatencyMs(),
		Samples:      st.attempts,
	}, true
}

// AllProviderStats returns the live attempt record for every provider
// with at least one recorded attempt, in provider-ID order. Empty
// before any request: an honest empty state, never invented signals.
func (e *Engine) AllProviderStats() []ProviderStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.attemptStats))
	for id, st := range e.attemptStats {
		if st.attempts > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]ProviderStats, 0, len(ids))
	for _, id := range ids {
		st := e.attemptStats[id]
		out = append(out, ProviderStats{
			ProviderID:   id,
			Attempts:     st.attempts,
			Successes:    st.successes,
			Failures:     st.attempts - st.successes,
			SuccessRate:  float64(st.successes) / float64(st.attempts),
			LatencyMs:    st.latencyEWMA,
			LatencyP95Ms: st.p95LatencyMs(),
			Samples:      st.attempts,
		})
	}
	return out
}

// withDeadline wraps ctx in the per-combo deadline budget when one
// is configured (Config.DeadlineBudget > 0). The returned cancel
// must be called when the request finishes. With no budget the
// caller's ctx is returned unchanged with a no-op cancel.
func (e *Engine) withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if e.cfg.DeadlineBudget <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, e.cfg.DeadlineBudget)
}

// lastUserPrompt extracts the routing prompt from a request: the
// last user message's text (content string, plus text parts when
// content is empty). It returns "" when there is no user message.
func lastUserPrompt(req provider.ChatRequest) string {
	msgs := make([]autorouter.PromptMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		var parts []string
		for _, p := range m.Parts {
			if p.Type == "text" {
				parts = append(parts, p.Text)
			}
		}
		msgs = append(msgs, autorouter.PromptMessage{Role: m.Role, Content: m.Content, TextParts: parts})
	}
	return autorouter.ParsePrompt(msgs)
}

// resolveAutoRequest applies the per-request model rewriting for
// one request: the "auto:<axis>" alias first (which also names
// the strategy for this request), then the "auto/*"
// virtual-model resolution. A direct "auto/code" request (no
// alias) implies strategy "code": the mode IS the strategy — its
// model choice optimises cost per solved task — so the decision
// header and the route trace name it and its reasoning
// (cost-per-solved-task) instead of the global default. Other
// virtual models imply no strategy: their strategy stays the
// configured global/chain strategy, exactly as before. The
// chain-order hook for "code" ranks on balanced weights (see
// internal/strategy WeightsFor), so naming the strategy here
// changes only what is reported, never the chain order — the
// cost-per-solved-task choice already happened at model
// resolution.
//
// Variant dispatch (M3 Phase D, D2): when the request names a
// known auto/* id and a live catalog is installed, resolveAuto
// resolves it through autorouter.ResolveAutoVariant and the
// variant's strategy becomes the request's effective strategy
// (alias strategy still wins when one is set). The fourth return
// is the dispatch outcome: Ordered carries a panel / ordered
// model list for multi-model resolutions (the chain-build lines
// use chainForDispatch), and Err carries a fail-closed resolver
// error (auto/subscription rung empty, auto/lkgp unrecorded) —
// the caller must fail the request with Err, never fall back.
//
// The third return is the resolved intent-tier effort
// (CHECKLIST 269-270): "low" / "medium" / "high" when the
// request named an "intent/<tier>" virtual model whose mapping
// pins an effort, "" otherwise (auto/* classification pins no
// effort; an unmapped tier resolves nothing). The caller stamps
// it onto the chain with withResolvedEffort, so the attempt
// carries the tier's effort to the wire and the trace.
func (e *Engine) resolveAutoRequest(ctx context.Context, req provider.ChatRequest) (string, provider.ChatRequest, string, autoDispatch) {
	strategy, req := e.resolveAutoAlias(req)
	if strategy == "" && req.Model == autorouter.CodeVirtualModel {
		strategy = "code"
	}
	req, effort, disp := e.resolveAuto(ctx, req)
	if disp.Strategy != "" && strategy == "" {
		strategy = disp.Strategy
	}
	return strategy, req, effort, disp
}

// withResolvedEffort stamps a resolved intent-tier effort onto
// the attempts of a chain that carry no effort of their own
// (CHECKLIST 269-270): the tier-resolved (model, effort) lands
// on the attempt, so reqWithModel sends the tier's effort to
// the wire and stampTraceEffort records it on the trace. An
// attempt that already carries an effort — a routing-group entry
// (CHECKLIST 255) — keeps its own: the group entry is the
// operator's routing decision for that entry and wins over the
// tier. The input chain is never mutated: the stamped copy is
// returned ("" effort returns the chain untouched).
func withResolvedEffort(chain []Attempt, effort string) []Attempt {
	if effort == "" {
		return chain
	}
	out := make([]Attempt, len(chain))
	copy(out, chain)
	for i := range out {
		if out[i].Effort == "" {
			out[i].Effort = effort
		}
	}
	return out
}

// resolveAuto rewrites an "auto/*" request model to the concrete
// model the auto-router picks for the request's prompt, and records
// the decision for LastAutoDecision. Non-auto models pass through
// unchanged and clear any previous decision. When the hook reports
// ok=false the model is left as "auto/*" so the chain resolution
// fails honestly (empty chain / unknown model) instead of routing
// to an invented model; the decision is still recorded, so the
// caller can see why.
//
// Variant dispatch runs first (M3 Phase D, D2): a known auto/* id
// with a live catalog installed resolves through
// autorouter.ResolveAutoVariant (see dispatchAutoVariant in
// autodispatch.go) — plain "auto", unknown ids, and requests with
// no catalog installed fall through to the AutoRoute path below.
// A variant error (fail-closed) is carried in the third return's
// Err and the model is left unresolved; the caller must fail the
// request with it, never fall back.
//
// The second return is the resolved intent-tier effort
// (CHECKLIST 269-270): when the decision is an intent resolution
// whose mapping pins an effort, the effort is carried onto the
// request itself (req.ReasoningEffort) — the tier is the
// operator's routing decision, so it overrides a client-supplied
// reasoning_effort exactly like a routing-group entry's effort
// does (see reqWithModel). An attempt with no effort of its own
// then passes this value through to the wire, so the tier's
// effort reaches the provider even on a chain the caller
// stamped without it (for example a group chain that replaced
// the stamped one). "" when no effort is pinned: the client's
// own value, or none, passes through untouched.
func (e *Engine) resolveAuto(ctx context.Context, req provider.ChatRequest) (provider.ChatRequest, string, autoDispatch) {
	if dispReq, disp, handled := e.dispatchAutoVariant(ctx, req); handled {
		return dispReq, "", disp
	}
	if e.cfg.AutoRoute == nil || !autorouter.IsVirtualModel(req.Model) {
		e.setLastAutoDecision(nil)
		// Request-scoped attribution (CHECKLIST 317-318): a
		// non-auto request clears its own sink, so a stale
		// decision from an earlier auto request on the same
		// sink can never be mistaken for this request's.
		if att := AttributionFrom(ctx); att != nil {
			att.PublishDecision(nil)
		}
		return req, "", autoDispatch{}
	}
	resolved, dec, ok := e.cfg.AutoRoute(req.Model, lastUserPrompt(req))
	decCopy := dec
	e.setLastAutoDecision(&decCopy)
	// Request-scoped attribution (CHECKLIST 317-318): publish
	// the decision into this request's sink here, from the
	// value just computed — never by re-reading the global
	// LastAutoDecision afterwards, where a neighbour's
	// resolveAuto could already have overwritten it.
	if att := AttributionFrom(ctx); att != nil {
		att.PublishDecision(&decCopy)
	}
	if !ok {
		return req, "", autoDispatch{}
	}
	req.Model = resolved
	if dec.Effort != "" {
		req.ReasoningEffort = string(dec.Effort)
		return req, string(dec.Effort), autoDispatch{}
	}
	return req, "", autoDispatch{}
}

func (e *Engine) setLastTrace(t *routetrace.RouteTrace) {
	// Region annotation (CHECKLIST 257): every published trace
	// carries each attempt's effective region and the
	// region-aware failover step it represents, recomputed from
	// the trace contents — so the trace the dashboard renders
	// and the trace the caller holds name the same steps.
	// Annotated before the lock: RegionFor reads the registry,
	// which has its own lock, and must never run under e.mu.
	e.annotateRegionTrace(t)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastTrace = t
}

// publishTrace publishes t as the most recent trace AND into the
// calling request's attribution sink when one is attached to ctx
// (CHECKLIST 317-318): the sink is how the server attributes
// this request's outcome to its own trace under concurrency,
// where the global LastTrace may already hold a neighbour's.
// Callers that have no ctx (embeddings, rerank) keep calling
// setLastTrace directly — their traces never feed outcome
// attribution, so the global slot is all they need.
func (e *Engine) publishTrace(ctx context.Context, t *routetrace.RouteTrace) {
	e.setLastTrace(t)
	if att := AttributionFrom(ctx); att != nil {
		att.PublishTrace(t)
	}
}

// quotaHint returns the remaining-quota hint for a provider; providers
// without a hint are treated as having ample quota (100).
func (e *Engine) quotaHint(providerID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h, ok := e.quotaHints[providerID]; ok {
		if h.expiresAt.IsZero() || time.Now().Before(h.expiresAt) {
			return h.hint
		}
		// Expired learned fraction: drop it so the provider is
		// eligible again (its window has refilled) and the next
		// response can teach a fresh fraction.
		delete(e.quotaHints, providerID)
	}
	if e.cfg.QuotaHint != nil {
		return e.cfg.QuotaHint(providerID)
	}
	return 100
}

// QuotaHintFunc adapts a provider registry to Config.QuotaHint, so the
// engine reads live quota hints on every request: a
// Registry.SetQuotaHint update takes effect without restarting the
// engine (or the process). Mapping: an unknown provider ID, or a
// provider whose hint is QuotaUnknown (-1), reports 100 (ample quota);
// a hint f in [0, 1] reports int(f*100), so 0 means "no quota left"
// and the engine skips that provider. A nil registry reports 100 for
// every provider.
func QuotaHintFunc(reg *provider.Registry) func(string) int {
	return func(providerID string) int {
		if reg == nil {
			return 100
		}
		p, ok := reg.Get(providerID)
		if !ok {
			return 100
		}
		if p.QuotaHint == provider.QuotaUnknown {
			return 100
		}
		if p.QuotaHint < 0 || p.QuotaHint > 1 {
			return 100
		}
		return int(p.QuotaHint * 100)
	}
}

// toolCapabilityFunc adapts a provider registry to
// Config.ToolCapability (CT-1), mirroring QuotaHintFunc: the
// engine reads live tool flags on every request, so a probe /
// sync / operator edit takes effect without restarting the
// engine (or the process). An unknown provider, an unknown
// model, or a model whose Tools flag is nil reports unknown
// (false, false) — never a guessed capability. A nil registry
// reports unknown for every model.
func toolCapabilityFunc(reg *provider.Registry) func(providerID, model string) (capable, known bool) {
	return func(providerID, model string) (capable, known bool) {
		if reg == nil {
			return false, false
		}
		p, ok := reg.Get(providerID)
		if !ok || p == nil {
			return false, false
		}
		for _, m := range p.Models {
			if m.ID == model {
				return m.ToolsCapable()
			}
		}
		return false, false
	}
}

// VisionCapabilityFunc adapts a provider registry to
// Config.VisionCapability (B3-24 modality bridges), mirroring
// ToolCapabilityFunc: the engine reads live vision flags on
// every request, so a sync / operator edit takes effect without
// restarting the engine (or the process). An unknown provider,
// an unknown model, or a model never reported for vision
// reports unknown (false, false) — never a guessed capability.
// A nil registry reports unknown for every model.
func VisionCapabilityFunc(reg *provider.Registry) func(providerID, model string) (capable, known bool) {
	return func(providerID, model string) (capable, known bool) {
		if reg == nil {
			return false, false
		}
		p, ok := reg.Get(providerID)
		if !ok || p == nil {
			return false, false
		}
		for _, m := range p.Models {
			if m.ID == model {
				return m.Vision, m.VisionKnown
			}
		}
		return false, false
	}
}

// VisionPoolFunc adapts a provider registry to Config.VisionPool
// (B3-24 modality bridges): every model the registry knows to
// be vision-capable (VisionKnown && Vision), in registry order —
// the pool order is the bridge's pick ranking. A nil registry
// yields no candidates, so the bridge never fires.
func VisionPoolFunc(reg *provider.Registry) func() []modality.Candidate {
	return func() []modality.Candidate {
		if reg == nil {
			return nil
		}
		var out []modality.Candidate
		for _, p := range reg.List() {
			for _, m := range p.Models {
				if m.Vision && m.VisionKnown {
					out = append(out, modality.Candidate{ProviderID: p.ID, ModelID: m.ID, Model: m})
				}
			}
		}
		return out
	}
}

// ModalityCapabilityFunc adapts a provider registry to
// Config.ModalityCapability (B3-24 modality bridges), mirroring
// VisionCapabilityFunc but for every modality at once: it returns
// the model's full capability record — Vision/VisionKnown,
// Audio/AudioKnown, Video/VideoKnown — as the registry reports
// it, plus whether the model is known to the registry at all.
// The engine reads live flags on every request, so a sync /
// operator edit takes effect without restarting the engine (or
// the process). An unknown provider, an unknown model, or a model
// the registry does not know reports a zero record + false —
// never a guessed capability. A nil registry reports unknown for
// every model.
func ModalityCapabilityFunc(reg *provider.Registry) func(providerID, model string) (provider.Model, bool) {
	return func(providerID, model string) (provider.Model, bool) {
		if reg == nil {
			return provider.Model{}, false
		}
		p, ok := reg.Get(providerID)
		if !ok || p == nil {
			return provider.Model{}, false
		}
		for _, m := range p.Models {
			if m.ID == model {
				return m, true
			}
		}
		return provider.Model{}, false
	}
}

// ModalityPoolFunc adapts a provider registry to Config.ModalityPool
// (B3-24 modality bridges): every model the registry knows to be
// capable of at least one modality (vision, audio or video, each
// known && capable), in registry order — the pool order is the
// bridge's pick ranking. modality.Decide filters the pool per
// request (a candidate must serve every blocking modality and be
// incapable of none required), so a vision-only candidate never
// wins an audio-blocked request. A nil registry yields no
// candidates, so the bridge never fires.
func ModalityPoolFunc(reg *provider.Registry) func() []modality.Candidate {
	return func() []modality.Candidate {
		if reg == nil {
			return nil
		}
		var out []modality.Candidate
		for _, p := range reg.List() {
			for _, m := range p.Models {
				if (m.Vision && m.VisionKnown) || (m.Audio && m.AudioKnown) || (m.Video && m.VideoKnown) {
					out = append(out, modality.Candidate{ProviderID: p.ID, ModelID: m.ID, Model: m})
				}
			}
		}
		return out
	}
}

// CapabilityLookupFromRegistry adapts a provider registry to
// Config.CapabilityLookup (P1.5), computing each model's capability set
// on demand from the same sources the API reports. A live lookup means
// the routing gate never consults a stale snapshot: a model added or
// synced a second ago is already seen. A nil registry reports
// ok=false for every pair.
func CapabilityLookupFromRegistry(reg *provider.Registry) func(providerID, model string) (capability.ModelCapabilities, bool) {
	b := capability.Builder{Registry: reg}
	return func(providerID, model string) (capability.ModelCapabilities, bool) {
		return b.Model(providerID, model)
	}
}

// CapabilityLookupFunc adapts a capability.Index to
// Config.CapabilityLookup (P1.5). A model missing from the index
// reports ok=false — the router's capability gate treats that as
// "nothing verified" and skips the attempt when a capability is
// required. A nil index reports ok=false for every pair.
func CapabilityLookupFunc(idx *capability.Index) func(providerID, model string) (capability.ModelCapabilities, bool) {
	return func(providerID, model string) (capability.ModelCapabilities, bool) {
		return idx.Lookup(providerID, model)
	}
}

// filterByRequiredCapabilities drops the attempts that do not hold
// every capability in cfg.RequireCapabilities with VERIFIED support
// (P1.5). Unknown is not evidence of support and never qualifies, so a
// model whose capability was never observed is filtered out exactly
// like one confirmed incapable — unlike the CT-1 tool pre-check, which
// fails open by design for plain chat traffic. No requirements (the
// default) returns the chain untouched, so default routing behaviour is
// unchanged. Applied before the dial on both the non-streaming and
// streaming paths, so a filtered-out attempt never costs a provider
// call.
func (e *Engine) filterByRequiredCapabilities(chain []Attempt) (kept []Attempt, skipped []Attempt, reasons []string) {
	if len(e.cfg.RequireCapabilities) == 0 || e.cfg.CapabilityLookup == nil {
		return chain, nil, nil
	}
	kept = make([]Attempt, 0, len(chain))
	for _, att := range chain {
		mc, known := e.cfg.CapabilityLookup(att.ProviderID, att.Model)
		missing := ""
		for _, need := range e.cfg.RequireCapabilities {
			if !known || !mc.HasVerified(need) {
				missing = string(need)
				break
			}
		}
		if missing != "" {
			skipped = append(skipped, att)
			reasons = append(reasons, "skipped_capability_unverified:"+missing)
			continue
		}
		kept = append(kept, att)
	}
	return kept, skipped, reasons
}

// filterZeroQuota splits a chain into the attempts that may serve
// (quota hint > 0) and the zero-quota attempts, which are skipped
// and reported on the trace. Shared by orderAttempts and the
// strategy-tree ordering (orderAttemptsByTree) so both paths
// agree on eligibility.
func (e *Engine) filterZeroQuota(chain []Attempt) (eligible, zeroSkipped []Attempt) {
	for _, att := range chain {
		if e.quotaHint(att.ProviderID) <= 0 {
			zeroSkipped = append(zeroSkipped, att)
			continue
		}
		eligible = append(eligible, att)
	}
	return eligible, zeroSkipped
}

// applyTailPartitions is the FINAL step of every ordering, after
// scoring (flat or tree): providers whose in-flight count is
// already at their adaptive cap go last, then providers inside a
// live soft cooldown go last. Both are stable partitions, never
// drops: a deprioritised provider is still tried when nothing
// else is left. Shared by orderAttempts and orderAttemptsByTree
// so a tree-ordered chain gets exactly the same safety treatment
// as a flat-ordered one.
func (e *Engine) applyTailPartitions(ordered []Attempt, mode string) []Attempt {
	if e.limiter != nil {
		var headroom, saturated []Attempt
		for _, att := range ordered {
			if e.limiter.HasHeadroom(att.ProviderID) {
				headroom = append(headroom, att)
			} else {
				saturated = append(saturated, att)
			}
		}
		ordered = append(headroom, saturated...)
	}
	if NormalizeCooldownMode(mode) == CooldownSoftDemote {
		var fresh, cooling []Attempt
		for _, att := range ordered {
			if _, _, ok := e.softCooling(ClientKey(att.ProviderID, att.KeyID)); ok {
				cooling = append(cooling, att)
			} else {
				fresh = append(fresh, att)
			}
		}
		ordered = append(fresh, cooling...)
	}
	// Reserve keys (CHECKLIST 175) go last — the FINAL step, after
	// scoring, the adaptive partition and soft-demotion: a reserve
	// key is held back until every preferred key is exhausted, so
	// it is only ever reached after all preferred attempts have
	// failed. This is a stable partition, never a drop: a pool of
	// only reserve keys still serves (its keys are simply all
	// last). It runs even for an explicit contract order — the
	// contract names providers, never keys, so it cannot have
	// ordered a reserve key first on purpose. Shared by
	// orderAttempts and orderAttemptsByTree, so a tree-ordered
	// chain gets the same reserve treatment as a flat one.
	{
		var preferred, reserve []Attempt
		for _, att := range ordered {
			if att.Reserve {
				reserve = append(reserve, att)
			} else {
				preferred = append(preferred, att)
			}
		}
		ordered = append(preferred, reserve...)
	}
	return ordered
}

// orderAttempts filters out zero-quota attempts and stably sorts the
// rest by quota hint descending, so the most headroom is tried first
// while the caller's chain order is preserved among equal hints.
// It then reorders by the resolved strategy: Config.StrategyHook
// builds the hook for that strategy name (falling back to
// Config.OrderHook, the pre-strategy behaviour, when no factory is
// set), so each strategy is a real scoring rule over live signals,
// not a label. mode is the resolved cooldown mode for this request
// (see cooldownModeFor): in soft-demote mode, providers inside a
// live soft cooldown move to the end of the order — never dropped,
// still tried once.
//
// preserveOrder is true when the client supplied an explicit
// per-request routing contract order (CHECKLIST 157): the client's
// stated try order is final and no scoring may re-rank it —
// neither the quota-hint sort nor the strategy hook. The safety
// partitions still apply (zero-quota eligibility, adaptive
// saturation, soft-demotion): a provider that cannot serve, is
// saturated, or just failed is still moved last, never dropped.
func (e *Engine) orderAttempts(chain []Attempt, strategy, mode string, preserveOrder bool) (ordered, zeroSkipped []Attempt) {
	// Context-free entry point: no request facts, so the
	// per-request hook (when installed) runs with a zero context
	// and its cacheability gate stays closed — the same behaviour
	// as before M6.5-wiring. Request paths call
	// orderAttemptsWithContext.
	return e.orderAttemptsWithContext(chain, strategy, mode, preserveOrder, StrategyHookContext{})
}

// orderAttemptsWithContext is orderAttempts for one request: the
// same filtering, quota-hint sort and safety partitions, with the
// request's StrategyHookContext threaded to the strategy hook.
// The per-request hook (Config.StrategyHookForRequest) is
// preferred when set; the context-free StrategyHook and then
// OrderHook are the fallbacks, in that order.
func (e *Engine) orderAttemptsWithContext(chain []Attempt, strategy, mode string, preserveOrder bool, hookCtx StrategyHookContext) (ordered, zeroSkipped []Attempt) {
	type scored struct {
		att  Attempt
		hint int
	}
	eligible, zeroSkipped := e.filterZeroQuota(chain)
	items := make([]scored, 0, len(eligible))
	for _, att := range eligible {
		items = append(items, scored{att: att, hint: e.quotaHint(att.ProviderID)})
	}
	if preserveOrder {
		// Explicit client order: keep the chain exactly as the
		// contract left it. No quota-hint sort, no strategy hook.
		for _, it := range items {
			ordered = append(ordered, it.att)
		}
	} else {
		sort.SliceStable(items, func(i, j int) bool { return items[i].hint > items[j].hint })
		for _, it := range items {
			ordered = append(ordered, it.att)
		}
		// Quota-share DRR (Phase D, D4): when the effective
		// strategy is "quota-share" the engine orders with its
		// own DRR over the persistent per-pool state (see
		// orderQuotaShare, cascade.go) instead of the
		// StrategyHook — the router cannot import
		// internal/strategy (cycle), and the hook factory has
		// no quota-share case, so the DRR owns this strategy.
		if strategy == quotaShareStrategy {
			ordered = e.orderQuotaShare(ordered)
		} else if strategy == cascadeStrategy {
			// 4-tier cascade (G19 Area 1): same interception
			// pattern as quota-share — the engine orders with
			// applyCascadeOrder over the live auto-catalog
			// tiers (cascadeTierFn, cascade.go) instead of
			// the StrategyHook.
			ordered = applyCascadeOrder(ordered, e.cascadeTierFn())
		} else if e.cfg.StrategyHookForRequest != nil {
			if hook := e.cfg.StrategyHookForRequest(strategy, hookCtx); hook != nil {
				ordered = hook(ordered)
			}
		} else if e.cfg.StrategyHook != nil {
			if hook := e.cfg.StrategyHook(strategy); hook != nil {
				ordered = hook(ordered)
			}
		} else if e.cfg.OrderHook != nil {
			ordered = e.cfg.OrderHook(ordered)
		}
	}
	// Adaptive-concurrency deprioritisation + soft-demotion (the
	// FINAL step, after scoring): see applyTailPartitions.
	//
	// Region-aware failover order (CHECKLIST 257) runs just
	// before those partitions, and only on engine-ordered
	// chains: after the quota sort and the strategy hook, the
	// chain is stably partitioned into same-model-other-region,
	// same-model-same-region, equivalent-model, self-hosted.
	// An explicit client order (preserveOrder) is final — no
	// scoring and no region tiering may re-rank it; the safety
	// partitions still apply.
	// Account-level fallback (M3 Area 1, PARITY row 388): within
	// each (provider, model) group, order the accounts (keys)
	// under that provider's account fallback strategy. It runs
	// after the chain strategy hook and before the region tiers
	// (both are stable, so the within-group account order
	// survives them); the reserve-last tail partition still runs
	// last. An explicit client order (preserveOrder) is final —
	// no account re-ranking either.
	if !preserveOrder {
		ordered = e.orderAccountAttempts(ordered)
		ordered = e.applyRegionTiers(ordered)
	}
	return e.applyTailPartitions(ordered, mode), zeroSkipped
}

// strategyHookContextFor builds the per-request StrategyHookContext
// for req as it will actually be sent — callers build it just
// before ordering, from the current req (after compression,
// reasoning injection and contract application), so the prompt
// count measures what the providers will receive.
// explicitOrder is the preserveOrder flag for this request (a
// contract order or a routing-group member order).
func (e *Engine) strategyHookContextFor(req provider.ChatRequest, explicitOrder bool) StrategyHookContext {
	ctx := StrategyHookContext{
		HasTools:       len(req.Tools) > 0,
		Temperature:    req.Temperature,
		TemperatureSet: req.Temperature != 0,
		ContractPinned: explicitOrder,
		// SLA objectives for the sla-aware hook (row 394):
		// unset config means the defaults (2000 ms / 5% /
		// no cap / soft). An explicitly configured error
		// rate of 0 (SLAMaxErrorRateSet) reaches the hook
		// as 0 — zero-tolerance — instead of being
		// rewritten to the 0.05 default; only an unset
		// (or non-positive, hand-built) rate maps to 0.05.
		SLATargetP95Ms:     e.cfg.SLATargetP95Ms,
		SLAMaxCostPer1M:    e.cfg.SLAMaxCostPer1M,
		SLAHardConstraints: e.cfg.SLAHardConstraints,
		SLAMaxErrorRateSet: e.cfg.SLAMaxErrorRateSet,
		// Budget-aware routing (B11): provide ledger and caller ID
		// so the budget-aware hook can read KeyBudgetStatus and
		// KeyBudgetConfig for the gateway key.
		Ledger:   e.ledger,
		ApiKeyId: req.CallerID,
	}
	if ctx.SLATargetP95Ms <= 0 {
		ctx.SLATargetP95Ms = 2000
	}
	if e.cfg.SLAMaxErrorRateSet || e.cfg.SLAMaxErrorRate > 0 {
		ctx.SLAMaxErrorRate = e.cfg.SLAMaxErrorRate
	} else {
		ctx.SLAMaxErrorRate = 0.05
	}
	// Message text + tool-call arguments (CountPrompt, not
	// CountPromptWire): the gate measures the cacheable prefix —
	// the stable leading prompt a cache marker would cover — and
	// tool schemas are excluded because a request carrying tools
	// is gated out anyway. A counting failure yields 0 and closes
	// the gate: the count must never fail a request (same rule as
	// the context-fit check).
	if c, err := tokencount.CountPrompt(req.Model, req.Messages); err == nil {
		ctx.PromptTokens = c.Tokens
	} else {
		e.tokMu.Lock()
		e.tokCountErrs++
		e.tokMu.Unlock()
	}
	return ctx
}

// --- circuit breaker ---

// breakerProfile resolves the auth profile of a provider (M4.2):
// the AuthProfileFor hook when set, API-key otherwise.
func (e *Engine) breakerProfile(provider string) BreakerProfile {
	if e.cfg.AuthProfileFor != nil {
		return BreakerProfileFor(e.cfg.AuthProfileFor(provider))
	}
	return BreakerProfileFor(AuthProfileAPIKey)
}

// breakerThresholdFor is the consecutive-failure count that opens
// the provider's breaker: the operator's explicit override when
// set, else the profile table.
func (e *Engine) breakerThresholdFor(provider string) int {
	if e.cfg.BreakerThreshold > 0 {
		return e.cfg.BreakerThreshold
	}
	return e.breakerProfile(provider).OpenThreshold
}

// breakerOpenFor is the open duration for the provider's Nth open
// cycle (N >= 1): the operator's explicit override when set (a
// FIXED duration — an explicit override never escalates), else the
// profile's escalated reset for N.
func (e *Engine) breakerOpenFor(provider string, trips int) time.Duration {
	if e.cfg.BreakerOpenFor > 0 {
		return e.cfg.BreakerOpenFor
	}
	return escalatedResetTimeout(e.breakerProfile(provider), trips)
}

// openForOf is the open duration of a live breaker state: the
// duration stamped at trip time, falling back to the profile reset
// for a state that somehow has none.
func (e *Engine) openForOf(provider string, st *breakerState) time.Duration {
	if st != nil && st.openFor > 0 {
		return st.openFor
	}
	return e.breakerOpenFor(provider, 1)
}

// breakerDecayHold is how long a DEGRADED breaker must go without
// a new breaker-counted failure before lazy recovery decays it
// back to CLOSED (the reference router: the longest OPEN hold for the last
// failure kind — its cooldownByKind value, else resetTimeout, ×
// the max backoff multiplier, at least one cooldown). Ours: the
// escalated reset for the current trip count — the base reset for
// a never-tripped breaker, longer after trips (the operator's
// explicit BreakerOpenFor override wins when set, exactly as for
// the open window itself).
func (e *Engine) breakerDecayHold(provider string, st *breakerState) time.Duration {
	return e.breakerOpenFor(provider, st.trips)
}

// breakerStaleLocked reports whether a breaker has had no new
// breaker-counted failure for a whole decay hold. Caller holds
// e.mu. A zero lastFailureAt means "unknown" (no failure yet, or
// a state restored from a store file written before lastFailureAt
// was recorded) — never treated as stale.
func (e *Engine) breakerStaleLocked(provider string, st *breakerState, now time.Time) bool {
	if st == nil || st.lastFailureAt.IsZero() {
		return false
	}
	return !now.Before(st.lastFailureAt.Add(e.breakerDecayHold(provider, st)))
}

// breakerStateOf reports the provider's breaker state name (M4.2):
// CLOSED below the degrade threshold, DEGRADED at/above it but
// below the open threshold, OPEN while tripped inside its window,
// HALF_OPEN once the window elapsed. Caller holds e.mu.
func (e *Engine) breakerStateOf(provider string, st *breakerState, now time.Time) string {
	prof := e.breakerProfile(provider)
	threshold := e.breakerThresholdFor(provider)
	if st == nil || st.consecutiveFailures < prof.DegradeThreshold {
		return BreakerClosed
	}
	if st.consecutiveFailures < threshold || st.openedAt.IsZero() {
		// Lazy recovery (parity tracker:524, the reference router
		// RESILIENCE_GUIDE "Lazy recovery"): the same reads
		// that report the state decay a stale DEGRADED back
		// to CLOSED — no background timer, the read itself
		// refreshes the state. The count stays until the
		// next failure or a success; only the reported
		// state decays.
		if e.breakerStaleLocked(provider, st, now) {
			return BreakerClosed
		}
		return BreakerDegraded
	}
	if now.Sub(st.openedAt) < e.openForOf(provider, st) {
		return BreakerOpen
	}
	return BreakerHalfOpen
}

// breakerAllow reports whether a provider may be tried now. It also
// claims the single half-open probe slot when a breaker has cooled
// down: at most one probe runs at a time; further callers see the
// breaker as open until the probe resolves. A provider is also
// refused while its failure-window breaker is open (M4.2).
func (e *Engine) breakerAllow(provider string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.windowOpenLocked(provider, time.Now()) {
		return false
	}
	st := e.breakers[provider]
	if st == nil || st.consecutiveFailures < e.breakerThresholdFor(provider) {
		return true
	}
	if st.openedAt.IsZero() {
		return true
	}
	if time.Since(st.openedAt) >= e.openForOf(provider, st) {
		if st.probeInFlight {
			return false
		}
		st.probeInFlight = true
		return true
	}
	return false
}

// BreakerOpenForProvider reports whether the provider's circuit
// breaker is currently OPEN (M3 Area 1, PARITY row 394): the
// exported read of breakerOpen for the signals closure in
// cmd/codvian-router, which fills scoring.Signals.BreakerOpen so
// the sla-aware hook can filter OPEN candidates before scoring.
// False means "not open OR unknown" — never proof of health.
func (e *Engine) BreakerOpenForProvider(provider string) bool {
	return e.breakerOpen(provider)
}

// BreakerDegradedForProvider reports whether the provider's
// circuit breaker is currently DEGRADED (RT-2 follow-up): the
// exported read of breakerStateOf's DEGRADED state for the
// signals closure in cmd/codvian-router, which fills
// scoring.Signals.BreakerDegraded so the LKGP hook can refuse
// to hoist a pin to a slowing/failing provider. DEGRADED means
// consecutive failures at/above the profile's degrade
// threshold but below the open threshold — still serving,
// flagged unhealthy (breakerStateOf, router.go). A provider
// that is OPEN reports false here: OPEN is its own state,
// already excluded by BreakerOpenForProvider, and the hook
// checks OPEN first — no double-counting, no change to the
// OPEN behaviour. HALF_OPEN also reports false. False means
// "not degraded OR unknown" — never proof of health.
func (e *Engine) BreakerDegradedForProvider(provider string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.breakerStateOf(provider, e.breakers[provider], time.Now()) == BreakerDegraded
}

// BreakerClosedForProvider reports whether the provider's breaker
// is positively known CLOSED (G19 Area 1, 16-factor health): the
// provider has a registered client (so the engine tracks a breaker
// for it — BreakerStates reports CLOSED by default for every such
// provider) and neither the consecutive breaker nor the failure
// window is open. False means "not closed OR unknown" (no client,
// or open) and must never be read as "open". The two locks are
// never held at once (breakerOpen takes e.mu, clientIDs takes
// e.clientsMu), matching BreakerStates' own ordering.
func (e *Engine) BreakerClosedForProvider(provider string) bool {
	if e.breakerOpen(provider) {
		return false
	}
	for _, id := range e.clientIDs() {
		if id == provider {
			return true
		}
	}
	return false
}

// breakerOpen is a read-only check used when deciding whether a second
// attempt is even worth hedging to. Unlike breakerAllow it never claims
// the half-open probe slot. True while EITHER the consecutive breaker
// or the failure-window breaker is open (M4.2).
func (e *Engine) breakerOpen(provider string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	if e.windowOpenLocked(provider, now) {
		return true
	}
	st := e.breakers[provider]
	if st == nil || st.consecutiveFailures < e.breakerThresholdFor(provider) {
		return false
	}
	if st.openedAt.IsZero() {
		return false
	}
	return time.Since(st.openedAt) < e.openForOf(provider, st)
}

func (e *Engine) breakerSuccess(provider string) {
	e.mu.Lock()
	st, had := e.breakers[provider]
	wasOpen := had && !st.openedAt.IsZero()
	sink := e.breakerEvents
	// Deleting the state resets the escalation: a provider that
	// recovered earns the base reset on its next trip (M4.2).
	delete(e.breakers, provider)
	e.mu.Unlock()
	// Edge-triggered recovery: only a success that actually clears
	// an OPEN breaker is reported; ordinary healthy successes are
	// silent. Dispatch outside e.mu so a sink cannot re-enter the
	// engine and deadlock.
	if sink != nil && wasOpen {
		go sink.ProviderRecovered(provider)
	}
	// G11 Area 2: persist the cleared state, but only when
	// there was state to clear — a healthy provider's steady
	// successes cost no I/O.
	if had {
		e.saveBreakerStore()
	}
}

// ResetBreaker clears one provider's circuit-breaker state — the
// consecutive-failure count (deleting the entry also resets the
// trip/escalation count, exactly like breakerSuccess) and the
// failure-window record — so the provider is tried again at once.
// It reports false when the engine holds no breaker state for the
// provider at all (unknown provider id, or a provider that never
// recorded a breaker-counted failure). The cumulative
// breakerIgnored4xx count is deliberately NOT cleared: it counts
// every 4xx that never tripped the breaker across the provider's
// lifetime, and a success does not reset it either. Cooldowns
// (hard per-key cooldowns, soft demotions) are a separate
// mechanism and are untouched — this resets breaker state only.
// Together with ResetAllBreakers this satisfies the server's
// breakerResetter interface (internal/server/resilience_api.go:
// POST /api/resilience/breakers/reset).
func (e *Engine) ResetBreaker(provider string) bool {
	e.mu.Lock()
	_, hasConsecutive := e.breakers[provider]
	_, hasWindow := e.windowBreakers[provider]
	if !hasConsecutive && !hasWindow {
		e.mu.Unlock()
		return false
	}
	delete(e.breakers, provider)
	delete(e.windowBreakers, provider)
	e.mu.Unlock()
	// G11 Area 2: persist the cleared state so a restart does
	// not resurrect the reset breaker.
	e.saveBreakerStore()
	return true
}

// ResetAllBreakers clears every known provider's circuit-breaker
// state (consecutive-failure count and failure-window record, as
// ResetBreaker does) and reports the provider ids it actually
// reset, in sorted order. Providers with no breaker state are
// omitted — an empty result means nothing was tripped.
func (e *Engine) ResetAllBreakers() []string {
	e.mu.Lock()
	seen := make(map[string]bool, len(e.breakers)+len(e.windowBreakers))
	for id := range e.breakers {
		seen[id] = true
	}
	for id := range e.windowBreakers {
		seen[id] = true
	}
	reset := make([]string, 0, len(seen))
	for id := range seen {
		delete(e.breakers, id)
		delete(e.windowBreakers, id)
		reset = append(reset, id)
	}
	sort.Strings(reset)
	e.mu.Unlock()
	// G11 Area 2: persist the cleared state (see ResetBreaker).
	if len(reset) > 0 {
		e.saveBreakerStore()
	}
	return reset
}

func (e *Engine) breakerFailure(provider, reason string) {
	e.mu.Lock()
	e.breakerFailureLocked(provider, reason)
	e.mu.Unlock()
	// G11 Area 2: persist the new state (breaker_store.go).
	e.saveBreakerStore()
}

// breakerFailureLocked is the breakerFailure body with the lock
// already held (split out so the wrapper can persist after
// unlocking — disk I/O never runs under e.mu).
func (e *Engine) breakerFailureLocked(provider, reason string) {
	prof := e.breakerProfile(provider)
	st := e.breakers[provider]
	if st == nil {
		st = &breakerState{}
		e.breakers[provider] = st
	}
	if reason != "" {
		st.lastReason = reason
	}
	now := time.Now()
	// The failure-window breaker counts every breaker-counted
	// failure (M4.2): even when the consecutive breaker is
	// already open, the window keeps counting — a flapping
	// provider must not escape the window by tripping early.
	e.windowRecordFailure(provider, prof, now)
	threshold := e.breakerThresholdFor(provider)
	// Lazy recovery (parity tracker:524): a failure that arrives
	// after a whole decay hold with no new failure is NOT
	// consecutive with the old ones — the stale count resets
	// first, so this failure counts as 1, and the escalation
	// ladder restarts from the base reset, exactly as after a
	// success. Only in CLOSED/DEGRADED territory (below the
	// open threshold, no probe in flight): an OPEN/HALF_OPEN
	// breaker keeps its trip count and escalation — the probe
	// path below owns that.
	if !st.probeInFlight && st.consecutiveFailures < threshold && e.breakerStaleLocked(provider, st, now) {
		st.consecutiveFailures = 0
		st.trips = 0
		st.openedAt = time.Time{}
		st.openFor = 0
	}
	st.lastFailureAt = now
	if st.probeInFlight {
		// Half-open probe failed: reopen immediately, as the
		// NEXT trip (escalated — the probe failure is a new
		// open cycle, not the same one).
		wasOpen, wasProbe := !st.openedAt.IsZero(), st.probeInFlight
		st.probeInFlight = false
		st.consecutiveFailures = threshold
		st.trips++
		st.openedAt = time.Now()
		st.openFor = e.breakerOpenFor(provider, st.trips)
		e.notifyDegradedLocked(provider, st, wasOpen, wasProbe)
		return
	}
	st.consecutiveFailures++
	if st.consecutiveFailures >= threshold {
		wasOpen := !st.openedAt.IsZero()
		st.trips++
		st.openedAt = time.Now()
		st.openFor = e.breakerOpenFor(provider, st.trips)
		e.notifyDegradedLocked(provider, st, wasOpen, false)
	}
}

// breakerNeutral releases a claimed half-open probe without counting a
// success or a failure. Used for 429s (throttling, not provider-down),
// for 4xx client errors (a bad key or a bad request is not provider
// unhealthiness — see isClientErrorFailure), and for attempts
// cancelled as hedge losers.
func (e *Engine) breakerNeutral(provider string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.breakers[provider]; st != nil {
		st.probeInFlight = false
	}
}

// isClientErrorFailure reports whether err is a client-side failure
// — a 4xx (bad request, bad key, forbidden, unprocessable) — a
// property of the request or the operator's key, never a signal
// that the provider is down. Such failures must NEVER trip the
// circuit breaker: the breaker trips only on 408/5xx and transport
// failures (the reference router RESILIENCE_GUIDE; LiteLLM's reliability docs
// agree). The verdict comes from the shared taxonomy
// (provider.ClassifyError), never a raw status list, so a
// mid-stream wire error with no HTTP status still classifies by
// reason, and a 400 that signals model-not-found is already
// breaker-neutral via isModelNotFoundErr. A 429 is also neutral
// here, but it never reaches this helper — the rate-limit branch
// in every caller runs first.
func isClientErrorFailure(err error) bool {
	cls := provider.ClassifyError(err, provider.EndpointChatCompletions)
	// 408 is a timeout, not a client error: the provider failed
	// to answer in time, so it still counts toward the breaker
	// exactly as before (the reference router: the breaker trips on 408).
	// 429 is likewise not a client error — it never reaches this
	// helper (the rate-limit branch runs first), but excluding it
	// here keeps the rule true even if a caller changes.
	if cls.Status == 408 || cls.Status == 429 {
		return false
	}
	if cls.Status >= 400 && cls.Status < 500 {
		return true
	}
	switch cls.Reason {
	case provider.ReasonRequestInvalid, provider.ReasonAuthFailed,
		provider.ReasonForbidden, provider.ReasonUnrepresentable:
		return true
	}
	return false
}

// countBreakerIgnored4xx records one 4xx failure that did not trip
// provider's breaker — the live counter behind the dashboard's
// "N client errors ignored by the breaker" proof.
func (e *Engine) countBreakerIgnored4xx(provider string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.breakerIgnored4xx[provider]++
}

// isHardLKGPFailure reports whether err is a hard provider failure
// — the class that clears a last-known-good pin (RT-2): a 5xx
// (including 529 overload), a 408 / per-target timeout, or a
// transport/connection error. The verdict comes from the shared
// taxonomy (provider.ClassifyError), never a raw status list, so
// an untyped connect/read failure classifies as transport and a
// mid-stream wire error classifies by its own mapping. What it
// deliberately excludes, matching the breaker rule: 4xx client
// errors (the request or the key is at fault, not the provider),
// 429s (throttling is not the provider being down), 402 quota
// errors, model-not-found, content refusals, and caller/engine
// cancellations — a bare context.DeadlineExceeded is the
// caller's or the engine's own deadline budget expiring, not a
// provider timeout (a per-target timeout arrives as the distinct
// perTargetTimeoutError instead, and counts).
func isHardLKGPFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if IsPerTargetTimeout(err) {
		return true
	}
	cls := provider.ClassifyError(err, provider.EndpointChatCompletions)
	if cls.Status == 408 || cls.Status >= 500 {
		return true
	}
	switch cls.Reason {
	case provider.ReasonTimeout, provider.ReasonTransportError:
		return true
	}
	return false
}

// invalidateLKGP clears the last-known-good pin for chainKey when
// err is a hard failure against provider (see isHardLKGPFailure).
// A no-op when the chain key is empty (surfaces such as embeddings
// carry no LKGP chain), when no LKGPInvalidator is wired, or when
// the failure is not hard — the hook itself only drops the pin
// when it names this provider, so a failure against one target
// can never clear another target's pin.
func (e *Engine) invalidateLKGP(chainKey, provider string, err error) {
	if chainKey == "" || provider == "" || e.cfg.LKGPInvalidator == nil {
		return
	}
	if !isHardLKGPFailure(err) {
		return
	}
	e.cfg.LKGPInvalidator(chainKey, provider)
}

// BreakerStatus is one provider's live circuit-breaker state, for
// the dashboard feed (GET /dashboard/cooldown). Open is true while
// the breaker is open (tripped and inside its open window);
// ConsecutiveFailures is the live breaker-counted failure count
// (0 while closed with no counted failures); LastReason is the
// taxonomy reason slug of the failure that most recently counted
// toward the breaker — when Open, the reason it tripped ("",
// when no breaker-counted failure has happened yet);
// Ignored4xx is how many 4xx failures this provider served that
// never tripped the breaker. State is the breaker state name
// (CLOSED / DEGRADED / OPEN / HALF_OPEN, M4.2); Profile is the
// auth profile the thresholds came from; OpenFor is the open
// duration of the current open window (the escalated reset, or the
// operator's override); Trips counts completed open cycles (the
// escalation rung); WindowOpen is true while the provider-level
// failure-window breaker is open; WindowFailures counts the
// provider's breaker-counted failures inside its current window.
type BreakerStatus struct {
	ProviderID          string
	Open                bool
	ConsecutiveFailures int
	Threshold           int
	OpenedAt            time.Time
	RemainingSeconds    int64
	LastReason          string
	Ignored4xx          int
	State               string
	Profile             AuthProfile
	OpenFor             time.Duration
	Trips               int
	WindowOpen          bool
	WindowFailures      int
}

// BreakerStates returns every known provider's breaker state in
// provider-ID order — including closed breakers with zero counted
// failures, so the dashboard can show "Closed" honestly instead of
// hiding untouched providers. Empty only when no provider is
// registered at all.
func (e *Engine) BreakerStates() []BreakerStatus {
	now := time.Now()
	// Client IDs first (clientsMu only), then breaker state (mu
	// only): the two locks are never held at once, so no lock
	// ordering can deadlock with AddClient.
	ids := e.clientIDs()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]BreakerStatus, 0, len(ids))
	for _, id := range ids {
		st := e.breakers[id]
		prof := e.breakerProfile(id)
		bs := BreakerStatus{
			ProviderID:     id,
			Threshold:      e.breakerThresholdFor(id),
			Ignored4xx:     e.breakerIgnored4xx[id],
			State:          BreakerClosed,
			Profile:        AuthProfileAPIKey,
			WindowOpen:     e.windowOpenLocked(id, now),
			WindowFailures: e.windowFailuresLocked(id, prof, now),
		}
		if e.cfg.AuthProfileFor != nil {
			bs.Profile = e.cfg.AuthProfileFor(id)
		}
		// A provider whose window breaker is open is OPEN even
		// when its consecutive breaker is not (or does not exist
		// yet — the window counts failures, not state).
		if bs.WindowOpen {
			bs.Open = true
			bs.State = BreakerOpen
		}
		if st == nil {
			out = append(out, bs)
			continue
		}
		bs.ConsecutiveFailures = st.consecutiveFailures
		bs.LastReason = st.lastReason
		bs.Trips = st.trips
		bs.OpenFor = e.openForOf(id, st)
		bs.State = e.breakerStateOf(id, st, now)
		if st.consecutiveFailures >= e.breakerThresholdFor(id) && !st.openedAt.IsZero() {
			bs.OpenedAt = st.openedAt
			remaining := int64(math.Ceil((e.openForOf(id, st) - now.Sub(st.openedAt)).Seconds()))
			if remaining > 0 {
				bs.Open = true
				bs.RemainingSeconds = remaining
			}
		}
		// The window breaker overrides the consecutive state:
		// a provider open by window reads OPEN whatever its
		// consecutive count says.
		if bs.WindowOpen {
			bs.Open = true
			bs.State = BreakerOpen
		}
		out = append(out, bs)
	}
	return out
}

// --- cooldowns ---
//
// The hard-cooldown map is keyed by (provider, key) identity
// (ClientKey, CHECKLIST 175): a multi-key provider cools one key
// at a time, never the whole provider. The parameter is named
// `key` everywhere here — it is a ClientKey, not a bare provider
// ID; the legacy single-key identity of a provider IS its bare
// provider ID, so existing single-key behaviour is unchanged.

func (e *Engine) onCooldown(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	until, ok := e.cooldowns[key]
	return ok && time.Now().Before(until)
}

// onCooldownStaggered is onCooldown with the G11 Area 2
// anti-thundering-herd guard (cooldown_auth.go): a key is
// re-admitted at its nominal end PLUS its deterministic release
// stagger, so keys expiring together spread over half a second
// instead of all at once. Used ONLY by the request attempt loops
// (chat, stream, embeddings) — the herd lives there. Nadir's
// decision cooldown (nadir.go) and the hedge-availability check
// keep the exact onCooldown: their timing contracts (a 150 ms
// sleep past a 100 ms cooldown, a hedge firing the moment a
// target frees) must not move.
func (e *Engine) onCooldownStaggered(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	until, ok := e.cooldowns[key]
	return ok && time.Now().Before(e.effectiveCooldownUntil(key, until))
}

func (e *Engine) setCooldown(key string, d time.Duration) {
	if d <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cooldowns[key] = time.Now().Add(d)
}

func (e *Engine) clearCooldown(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.cooldowns, key)
}

// --- helpers ---

func (e *Engine) hedgeSecondAvailable(att Attempt, mode string) bool {
	// A reserve key is held back until every preferred key is
	// exhausted (CHECKLIST 175) — hedging to it while a preferred
	// key is still in flight would spend the reserve early, so a
	// reserve attempt is never a hedge target.
	if att.Reserve {
		return false
	}
	if NormalizeCooldownMode(mode) == CooldownSoftDemote {
		// A demoted target is still tried — hedging to it is
		// allowed even while it cools (it sits at the end of
		// the order, not out of it).
		return !e.breakerOpen(att.ProviderID)
	}
	return !e.breakerOpen(att.ProviderID) && !e.onCooldown(ClientKey(att.ProviderID, att.KeyID))
}

func (e *Engine) hasClient(providerID string) bool {
	e.clientsMu.RLock()
	defer e.clientsMu.RUnlock()
	// Any key's client counts: a provider with only pooled-key
	// clients (CHECKLIST 175) still has a live client, even
	// though no client is registered under the bare provider ID.
	if _, ok := e.clients[providerID]; ok {
		return true
	}
	prefix := providerID + "\x00"
	for k := range e.clients {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// clientFor returns the upstream client for providerID, or false
// when none is registered: the legacy single-key client (key "").
// All clients-map reads go through here (under clientsMu.RLock) so
// a live AddClient cannot race a request in flight.
func (e *Engine) clientFor(providerID string) (provider.Client, bool) {
	return e.clientForKey(ClientKey(providerID, ""))
}

// clientForKey returns the client registered under one (provider,
// key) identity, or false when none is registered.
func (e *Engine) clientForKey(key string) (provider.Client, bool) {
	e.clientsMu.RLock()
	defer e.clientsMu.RUnlock()
	c, ok := e.clients[key]
	return c, ok
}

// clientForAttempt returns the upstream client for one attempt's
// (provider, key) pair, or false when none is registered. This is
// what callChat uses: a multi-key attempt dials its own key's
// client, never another key's.
func (e *Engine) clientForAttempt(att Attempt) (provider.Client, bool) {
	return e.clientForKey(ClientKey(att.ProviderID, att.KeyID))
}

// clientIDs returns the registered provider IDs in sorted order
// (the deterministic fallback-chain order). The clients map is
// keyed by (provider, key) identities, so the key suffix is
// stripped and each provider is listed once.
func (e *Engine) clientIDs() []string {
	e.clientsMu.RLock()
	defer e.clientsMu.RUnlock()
	seen := make(map[string]bool, len(e.clients))
	ids := make([]string, 0, len(e.clients))
	for key := range e.clients {
		id, _ := SplitClientKey(key)
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// AddClient registers (or replaces) the upstream client for
// providerID at runtime, without a restart: a provider added live
// (POST /api/providers, wired in main.go via the server's
// provider-added hook) routes immediately. It registers the
// legacy single-key client (key ""); for one key of a pool use
// AddClientForKey. Safe for concurrent use with Chat/ChatStream —
// the map write holds clientsMu and every reader goes through
// clientFor/clientIDs.
func (e *Engine) AddClient(providerID string, client provider.Client) {
	e.AddClientForKey(providerID, "", client)
}

// AddClientForKey registers (or replaces) the upstream client for
// one (provider, key) pair at runtime (CHECKLIST 175): a key
// added live (POST /api/providers/{id}/keys, wired in main.go via
// the server's provider-key hook) routes immediately, with no
// restart and without disturbing the provider's other keys.
func (e *Engine) AddClientForKey(providerID, keyID string, client provider.Client) {
	if providerID == "" || client == nil {
		return
	}
	e.clientsMu.Lock()
	defer e.clientsMu.Unlock()
	e.clients[ClientKey(providerID, keyID)] = client
}

// RemoveClient drops the upstream clients for providerID at runtime,
// without a restart: a provider removed live (DELETE
// /api/providers/{id}, wired in main.go via the server's
// provider-removed hook) stops routing immediately — the fallback
// chain is rebuilt from the live client map on every request, so a
// request in flight keeps its own chain snapshot while the next
// request never sees the removed provider. Every key's client goes
// (a provider's keys share its fate). Removing an unknown provider
// is a no-op. Safe for concurrent use with Chat/ChatStream.
func (e *Engine) RemoveClient(providerID string) {
	if providerID == "" {
		return
	}
	e.clientsMu.Lock()
	defer e.clientsMu.Unlock()
	delete(e.clients, providerID)
	for key := range e.clients {
		if id, _ := SplitClientKey(key); id == providerID {
			delete(e.clients, key)
		}
	}
}

// RemoveClientForKey drops the upstream client for one (provider,
// key) pair at runtime (CHECKLIST 175): a key removed live (DELETE
// /api/providers/{id}/keys/{keyId}) stops routing immediately,
// while the provider's other keys keep serving. Removing an
// unknown key is a no-op.
func (e *Engine) RemoveClientForKey(providerID, keyID string) {
	if providerID == "" {
		return
	}
	e.clientsMu.Lock()
	defer e.clientsMu.Unlock()
	delete(e.clients, ClientKey(providerID, keyID))
}

// sleep waits for d or until ctx is done. It reports false if ctx ended.
func (e *Engine) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// rateLimitWait computes how long to wait before retrying the SAME
// target after a 429: a present Retry-After hint is floored at 10s
// and capped at RetryAfterCap — except a hint >= 60s, which
// ABANDONS the target (CHECKLIST 167): the wait is 0, and the
// caller must move on or fail, never sleep-and-retry. An absent
// hint (retryAfter 0 with present false) keeps exponential backoff
// (base * 2^(n-1), capped at BackoffMax) with jitter in [0.5x, 1x].
// n is the consecutive-429 count (1-based). present must come from
// rateLimitHint: a present 0 is a poisoned hint and floors to 10s,
// an absent hint backs off.
func (e *Engine) rateLimitWait(retryAfter time.Duration, present bool, n int) time.Duration {
	if present {
		wait, _, _ := classifyRetryAfter(retryAfter, present, e.cfg.RetryAfterCap)
		return wait
	}
	shift := n - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 10 {
		shift = 10
	}
	d := e.cfg.BackoffBase << uint(shift)
	if d > e.cfg.BackoffMax || d <= 0 {
		d = e.cfg.BackoffMax
	}
	e.randMu.Lock()
	f := 0.5 + e.rand.Float64()*0.5
	e.randMu.Unlock()
	return time.Duration(float64(d) * f)
}

func reqWithModel(req provider.ChatRequest, att Attempt) provider.ChatRequest {
	r := req
	r.Model = att.Model
	// The attempt's effort (CHECKLIST 255) overrides the
	// client's own reasoning_effort for this attempt: a group
	// entry (model, effort) is the operator's routing decision
	// for that entry. An attempt with no effort ("") passes the
	// client's value through untouched.
	if att.Effort != "" {
		r.ReasoningEffort = att.Effort
	}
	return r
}

// attemptResult is the outcome of one provider call.
type attemptResult struct {
	att       Attempt
	resp      *provider.ChatResponse
	err       error
	latencyMs int64
	called    bool // a real client call was made (false: unknown provider)
	// reasoning is the handoff label for this hop (what happened
	// to the restored reasoning state — preserved, translated,
	// dropped with a reason, unrepresentable; "" when the request
	// carried no reasoning state). Computed in callChat from the
	// same reasoning.Check verdict the wire converter enforced.
	reasoning string
}

func (e *Engine) callChat(ctx context.Context, att Attempt, req provider.ChatRequest) (res attemptResult) {
	client, ok := e.clientForAttempt(att)
	if !ok {
		return attemptResult{att: att, err: fmt.Errorf("router: unknown provider %q", att.ProviderID)}
	}
	// The reasoning handoff label for this hop, from the same
	// verdict the wire converter enforces (see handoffLabel):
	// it rides every attemptResult below, so the trace says
	// honestly what happened to the restored reasoning state.
	label := e.handoffLabel(att, req, client)
	// Chaos-mode fault injection (B2-13, faultinject.go): an
	// armed fault fires here, before the limiter slot is taken
	// and before any provider is dialled — the injected failure
	// flows through the normal failure path (noteFailure,
	// breaker, lockout, ledger) exactly like a real upstream
	// failure, except the error names the injection. With no
	// fault armed this is a single read-lock probe and changes
	// nothing.
	if res, injected := e.applyFault(ctx, att, label); injected {
		return res
	}
	// Effort forwarding (CHECKLIST 285-286): resolve the
	// attempt's effective effort into the per-wire forwarding
	// for this hop. reqWithModel runs first so the effective
	// effort — tier pinned, group entry, or the client's own
	// value, in that precedence — is what resolves. The
	// stamped attempt rides every attemptResult below (including
	// the limiter-shed one), so the trace records the
	// forwarding decision even for an attempt that never
	// dialled. The budget rides attReq into the client call.
	attReq := reqWithModel(req, att)
	attReq, att = e.withEffortForwarding(attReq, att, client)
	// Adaptive admission: a provider at its cap sheds this call
	// (ErrLimited, called=false) instead of queueing behind its
	// in-flight work. The limiter fails open — a nil limiter
	// admits — so this can only refuse a provider whose RTT has
	// actually degraded. The slot is held for the upstream call
	// only and released the moment it returns.
	// P2-CAP-002: process-wide admission cap consulted first — fail-open
	// when unset (nil). Bounds aggregate concurrency regardless of how
	// many providers/keys are active.
	if !adaptive.GlobalTryAcquire() {
		e.noteConcurrencyRejected(att.ProviderID)
		return attemptResult{att: att, err: adaptive.ErrLimited, reasoning: label}
	}
	defer adaptive.GlobalRelease()
	if !e.limiter.TryAcquire(att.ProviderID) {
		adaptive.GlobalRelease()
		e.noteConcurrencyRejected(att.ProviderID)
		return attemptResult{att: att, err: adaptive.ErrLimited, reasoning: label}
	}
	defer e.limiter.Release(att.ProviderID)
	start := time.Now()
	// Panic isolation (CHECKLIST 165): a provider client that
	// panics — a bug in the client, or in a wire chunk converter
	// it calls — fails this attempt, it does not kill the gateway.
	// The recovered panic becomes an ordinary attempt error, so
	// the chain fails over to the next target and the attempt is
	// recorded in the ledger by the normal failure path
	// (noteFailure, called=true: the call was attempted). The
	// panic is counted (notePanic) and its value rides the
	// PanicError to the server, which logs it redacted.
	defer func() {
		if v := recover(); v != nil {
			e.notePanic("provider_chat")
			res = attemptResult{att: att, err: NewPanicError("provider_chat", v), latencyMs: time.Since(start).Milliseconds(), called: true, reasoning: label}
		}
	}()
	resp, err := func() (*provider.ChatResponse, error) {
		// Per-target timeout (M4.7c): the attempt's own
		// deadline, a child of the request ctx — the request
		// deadline still wins when it is earlier.
		callCtx, cancel := e.perTargetCtx(ctx)
		defer cancel()
		r, err := client.Chat(callCtx, attReq)
		if err != nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			// The per-target timeout fired while the request
			// itself was still alive: a hung provider, not a
			// cancelled request. The typed error keeps
			// noteFailure's cancel guard from swallowing it
			// (the breaker must count a hung provider).
			return nil, &perTargetTimeoutError{Timeout: e.perTargetTimeout()}
		}
		return r, err
	}()
	if err == nil && resp != nil && isEmptyCompletion(resp) && ReplayExemption(req) == "" {
		// An empty completion (no text, no tool calls) is not a
		// success: the attempt served nothing, so the chain
		// moves on to the next target in the SAME request —
		// the non-streaming half of the preread rule
		// (research-log entries 97 + 111, CHECKLIST 133/153).
		// The 200 has not been committed yet, so failing over
		// here is still before commit. A tool call IS content
		// (the client will execute it), so a tool-carrying
		// reply is never empty. Replay-exempt requests
		// (embeddings/images/rerank, Responses continuations,
		// warm-ups) are never replayed: their empty reply is
		// returned as-is, exactly once.
		err = &provider.EmptyStreamError{FinishReason: resp.FinishReason}
		resp = nil
	}
	if err == nil && resp != nil && ReplayExemption(req) == "" {
		// G11 Area 2: 200-OK body validation predicate — a 200
		// body that fails it fails over to the next target on
		// the same path as a failed attempt (the 200 is not
		// committed yet, so the switch is still legal).
		// Replay-exempt requests are exempt here too: exactly
		// one attempt, ever. See response_validation.go.
		if verr := ValidateResponseBody(resp, e.cfg.ResponseValidation); verr != nil {
			err = verr
			resp = nil
		}
	}
	// Rate-limit learning (M5.13): the response's parsed headers
	// pace the adaptive limiter, and a 429's headers teach the
	// same lesson — the next request to this provider slows down
	// before the limit is hit again.
	if resp != nil {
		e.learnRateLimit(att, resp.RateLimit)
	}
	if err != nil {
		e.learnRateLimitError(att, err)
	}
	return attemptResult{att: att, resp: resp, err: err, latencyMs: time.Since(start).Milliseconds(), called: true, reasoning: label}
}

// isEmptyCompletion reports whether a non-streaming response served
// nothing: no text, no tool calls and no reasoning. A finish reason
// alone is not content — a stream that sends only `finish_reason:
// stop` is an empty completion, and the same holds here. A
// reasoning-only completion is NOT empty: a model that spent its
// whole budget thinking (finish_reason: length) served real output,
// and failing over cannot improve the same budget on another target
// (live OpenRouter verification, 2026-11-21).
func isEmptyCompletion(resp *provider.ChatResponse) bool {
	if resp.Content != "" || len(resp.ToolCalls) > 0 {
		return false
	}
	return resp.Reasoning.Empty()
}

// recordAttempt records one attempt against the gateway key the
// request arrived under (keyID "" = anonymous / no key: the plain
// RecordAttempt path, rows with an empty key). It is the single
// place the engine writes ledger rows, so the keyed and unkeyed
// paths can never drift apart on what a row means.
func (e *Engine) recordAttempt(keyID string, att Attempt, ok bool, latencyMs int64, usage provider.Usage) {
	// Token/cost metrics are independent of ledger availability: a
	// nil ledger must not hide usage from /metrics. The sink reads
	// only provider, model and numeric counters — never a key or
	// any message content. Both chat and streaming calls funnel
	// through here, so each attempt is counted exactly once.
	e.mu.Lock()
	sink := e.usageMetrics
	e.mu.Unlock()
	if sink != nil {
		sink.IncUsageTokens(att.ProviderID, att.Model, "prompt", usage.PromptTokens)
		sink.IncUsageTokens(att.ProviderID, att.Model, "completion", usage.CompletionTokens)
		sink.IncUsageTokens(att.ProviderID, att.Model, "cached", usage.CachedTokens)
		sink.IncUsageTokens(att.ProviderID, att.Model, "reasoning", usage.ReasoningTokens)
		if usage.Cost != nil {
			sink.AddReportedCost(att.ProviderID, att.Model, *usage.Cost)
		}
	}
	if e.ledger == nil {
		return
	}
	e.ledgerRecordAttempts.Add(1)
	if pkl, isKeyed := e.ledger.(ProviderKeyLedger); isKeyed {
		pkl.RecordAttemptWithProviderKey(keyID, att.ProviderID, att.KeyID, att.Model, ok, latencyMs, usage)
		return
	}
	if kl, isKeyed := e.ledger.(KeyedLedger); isKeyed {
		kl.RecordAttemptForKey(keyID, att.ProviderID, att.Model, ok, latencyMs, usage)
		return
	}
	e.ledger.RecordAttempt(att.ProviderID, att.Model, ok, latencyMs, usage)
}

// recordCancelledAttempt records a hedge loser: an attempt that was
// started and then cancelled because the other hedge attempt won.
// A cancelled row is stored (the attempt really happened, with its
// latency) but is NOT a failed attempt — the ledger excludes it
// from failure counts (Summary.Errors, Buckets, LogFailed), so a
// hedge race stops inflating the provider's failure rate. A
// cancelled attempt reports no usage, so the row carries none.
// A ledger that cannot store cancelled rows records nothing —
// the attempt stays visible in the trace's hedged_cancelled
// entry without inventing a failure.
func (e *Engine) recordCancelledAttempt(keyID string, att Attempt, latencyMs int64) {
	if e.ledger == nil {
		return
	}
	if cl, isCancelled := e.ledger.(CancelledLedger); isCancelled {
		cl.RecordCancelledAttempt(keyID, att.ProviderID, att.KeyID, att.Model, latencyMs)
	}
}

// failedUsage returns the usage to record for a failed attempt: the
// usage the upstream reported on the attempt's own response, when
// it still carries one — a content-filter refusal converted to an
// error after a 200, where the provider billed the prompt (and
// reports an exact cost on xAI/OpenRouter). It is recorded
// verbatim, never guessed: callChat nils resp on empty bodies,
// failed body validation and client errors, so Usage{} stays
// correct wherever the upstream reported nothing.
func failedUsage(res attemptResult) provider.Usage {
	if res.resp != nil {
		return res.resp.Usage
	}
	return provider.Usage{}
}

// TokenCountStats returns the engine's local-count reconciliation
// accounting (see TokenCountStats).
func (e *Engine) TokenCountStats() TokenCountStats {
	e.tokMu.Lock()
	defer e.tokMu.Unlock()
	return TokenCountStats{
		Compared:    e.tokCompared,
		Diverged:    e.tokDiverged,
		CountErrors: e.tokCountErrs,
		Last:        e.lastDivergence,
		HasLast:     e.hasLastDivergence,
	}
}

// maybeCompress applies lossless-first compression to req
// (CHECKLIST 183) and stamps the result on it. It is a no-op when
// the request already carries a stamp for this exact model — the
// server handler compresses before the budget gates, and the
// engine must not re-measure the saving against the
// already-compressed prompt. When the model changed since the
// stamp (an "auto/*" virtual model resolving to a concrete one),
// the decision is re-run with the stamp as the prior, so the
// saving stays measured against the original prompt (see
// internal/compress.Apply). Compression never fails a request: a
// counting failure leaves the messages untouched.
//
// G22 (row :868): the decision runs through
// compress.ApplyWithOptions with the cache-aware inputs supplied
// from real signals — IsCachingProvider is true when any provider
// client configured on this engine is known to honour a
// prompt-cache marker (see anyCachingProvider). HasCacheControl
// stays false here: the canonical provider.Message drops native
// cache_control markers at parse, so the engine cannot see one
// (a Message field + wire translation is a provider slice first;
// see internal/degrade/cache_aware.go).
//
// Row :939: the decision also passes FrozenPrefix from the
// process-wide PrefixFreeze observer (installed via
// SetPrefixFreezeHooks by main.go; nil until installed), and
// observes the system prompt once per request — only when this
// request carries no compression stamp yet, i.e. this call is
// the first compression point for it. The server stamps first
// for server-routed requests (observation for those is the
// server handler's job, via ObservePrefix in applyCompression),
// and a stamp means an earlier maybeCompress call in this same
// request already observed it: Chat/ChatStream call
// maybeCompress and then the chain functions call it again, so
// observing on every call would count one request 2-3 times
// and trip the freeze threshold (3) on the first request.
// The server handler's applyCompression (internal/server/
// compression.go) now runs the same pass-through (d2107b9):
// ApplyWithOptions with IsCachingProvider from
// AnyCachingProvider, FrozenPrefix from PrefixFreezeChecker,
// and one ObservePrefix per request.
func (e *Engine) maybeCompress(req provider.ChatRequest) provider.ChatRequest {
	// :939 — observe once per request, at the first compression
	// point (see the doc comment above for why a stamp means
	// "already observed"). No-op when no observer is installed.
	if req.Compressed == nil {
		e.ObservePrefix(req.Messages)
	}
	if req.Compressed != nil && req.Compressed.Model == req.Model {
		return req
	}
	msgs, res := compress.ApplyWithOptions(req.Model, req.Messages, req.Compression, e.cfg.CompressionLosslessModels, req.Compressed, compress.ApplyOptions{
		IsCachingProvider: e.anyCachingProvider(),
		HasCacheControl:   false,
		FrozenPrefix:      e.prefixFreezeChecker,
	})
	req.Messages = msgs
	req.Compressed = res
	return req
}

// anyCachingProvider reports whether any provider client configured
// on this engine is known to honour a prompt-cache marker
// (cache.SupportsPromptCacheMarker — the canonical set; the
// strategy mirror must stay in lockstep with it). The compression
// stamp is decided once and shared across every attempt in the
// fallback chain — there is no single resolved target provider at
// compression time (chainFor tries the model against every
// configured client) — so "any" is the conservative choice: if
// the serving attempt turns out to be a caching provider, the
// cacheable prefix must already be protected. When no configured
// provider caches, the downgrade never fires and behaviour is
// unchanged. The check costs one map lookup per configured
// client, only when compression is decided.
func (e *Engine) anyCachingProvider() bool {
	for _, id := range e.clientIDs() {
		if cache.SupportsPromptCacheMarker(id) {
			return true
		}
	}
	return false
}

// traceCompression converts a stamped compression result into the
// route-trace block, or nil when nothing was applied — an absent
// block is more honest than a zeroed one.
func traceCompression(res *provider.CompressionResult) *routetrace.CompressionInfo {
	if res == nil || !res.Applied {
		return nil
	}
	return &routetrace.CompressionInfo{
		Applied:        true,
		Lossless:       res.Lossless,
		LossyApplied:   res.LossyApplied,
		Steps:          res.Steps,
		BeforeTokens:   res.BeforeTokens,
		AfterTokens:    res.AfterTokens,
		SavedTokens:    res.SavedTokens,
		LossyMaxTokens: res.LossyMaxTokens,
		Source:         res.Source,
		Verified:       res.Verified,
	}
}

// withCompression attaches a request's stamped compression saving
// to a successful attempt's usage, so the ledger row carries the
// prompt before compression alongside the prompt as sent. Failed
// attempts and shared rows never carry it (see
// provider.Usage): a failed attempt billed nothing, and a shared
// row is zeroed like every other token field.
func withCompression(u provider.Usage, req provider.ChatRequest) provider.Usage {
	if req.Compressed == nil || !req.Compressed.Applied {
		return u
	}
	u.PromptBeforeCompression = req.Compressed.BeforeTokens
	u.CompressionSavedTokens = req.Compressed.SavedTokens
	return u
}

// wireFor resolves the wire format of a provider's client, or ""
// when the client does not say (unknown wire — the estimator then
// takes the conservative OpenAI-reference framing; see
// tokencount.CountPromptWire).
//
// The clients map is keyed by (provider, key) identities
// (ClientKey, CHECKLIST 175): a provider with a key pool is
// stored under "provider\x00key", never under the bare provider
// ID. A bare-ID lookup alone misses every such client — at
// startup (main.go registers keyed clients under ClientKey) and
// live-added alike — and silently falls back to the OpenAI
// reference, which on a native wire overcounts the prompt by
// (3n+3) − n tokens. The live smoke caught it (CHECKLIST 229-230):
// a two-message Anthropic request measured a local estimate of 26
// against 18 reported (+44%). So wireFor looks up the bare
// identity first (the legacy no-auth client) and then scans for
// any keyed client of the same provider: the wire format is a
// property of the provider, not the key, so any match is the
// right one.
func (e *Engine) wireFor(providerID string) string {
	e.clientsMu.RLock()
	defer e.clientsMu.RUnlock()
	if c, ok := e.clients[providerID]; ok {
		if wf, ok := c.(wireFormatClient); ok {
			return wf.WireFormat()
		}
		return ""
	}
	for key, c := range e.clients {
		if id, _ := SplitClientKey(key); id == providerID {
			if wf, ok := c.(wireFormatClient); ok {
				return wf.WireFormat()
			}
			return ""
		}
	}
	return ""
}

// localPromptCount counts a request's prompt tokens locally
// (tiktoken BPE, offline) under the serving model's encoding and
// the serving wire's framing (tool definitions + per-message
// framing — see tokencount.CountPromptWire). A counting failure
// returns 0 and is counted, never an error: the count is an
// estimate that must never fail a request.
func (e *Engine) localPromptCount(model, wire string, req provider.ChatRequest) int {
	c, err := tokencount.CountPromptWire(model, wire, req)
	if err != nil {
		e.tokMu.Lock()
		e.tokCountErrs++
		e.tokMu.Unlock()
		return 0
	}
	return c.Tokens
}

// localPromptCountCalibrated is localPromptCount with the M7.9
// runtime calibration applied: the wire count scaled by the
// learned factor for (provider, model, tools) once that bucket
// holds >= 3 observations, unchanged otherwise (cold start is
// byte-identical to the uncalibrated count). Use it at the
// post-routing estimate sites; the pre-routing CountPromptFull
// sites have neither provider nor wire and stay uncalibrated.
// The observation feed must NEVER use this count — see
// reconcileUsage.
func (e *Engine) localPromptCountCalibrated(providerID, model, wire string, req provider.ChatRequest) int {
	c, err := tokencount.CountPromptWireCalibrated(providerID, model, wire, req)
	if err != nil {
		e.tokMu.Lock()
		e.tokCountErrs++
		e.tokMu.Unlock()
		return 0
	}
	return c.Tokens
}

// reconcileUsage reconciles a successful attempt's provider-reported
// usage against the local tiktoken estimate of the same request's
// prompt, and returns the usage to record:
//
//   - The upstream reported nothing at all (all zeros): the local
//     prompt count becomes the fallback, labelled LocalEstimate
//     (ledger stores it with split unknown, cost 0). The ledger
//     never receives a bare zero for a request that really sent a
//     prompt, and the label keeps the estimate from ever being
//     read as a provider-reported split.
//   - The upstream reported a prompt count: the provider's number
//     is kept verbatim — never overwritten by the local count —
//     and a material divergence (see tokenDivergenceMaterial) is
//     counted, remembered, and handed to OnTokenDivergence when
//     set. Silence would hide which side to trust; preferring one
//     silently would be worse.
//
// Failed attempts never reach this function: their usage stays
// Usage{} (unknown), because a failed call billed nothing and an
// estimate would imply it did.
func (e *Engine) reconcileUsage(providerID, model string, req provider.ChatRequest, usage provider.Usage) provider.Usage {
	local := e.localPromptCountCalibrated(providerID, model, e.wireFor(providerID), req)
	// The estimate rides along with the usage either way
	// (CHECKLIST 229): on a reported row it is the second side
	// of the estimate-vs-reported comparison the ledger records;
	// on a no-report row it becomes the fallback count below.
	// The provider's numbers are never overwritten by it.
	if local > 0 {
		usage.LocalEstimatePrompt = local
	}
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
		if local > 0 {
			usage.PromptTokens = local
			usage.TotalTokens = local
			usage.LocalEstimate = true
		}
		return usage
	}
	if usage.PromptTokens <= 0 || local <= 0 {
		return usage
	}
	// M7.9 runtime calibration: feed one observation per
	// compared request into the process-wide calibrator — the
	// UNCALIBRATED local against the reported count. Recording
	// the calibrated local would feed the calibrator its own
	// output, and the EMA would learn the square root of the
	// true ratio instead of the ratio. The second count is a
	// memo hit (same text, same encoding), not a second encode.
	// No observation for local-estimate fallback rows (no
	// reported number — those return above) or for rows whose
	// prompt_tokens double-count re-sent history.
	tokencount.RecordCalibrationObservation(providerID, model, len(req.Tools) > 0, e.localPromptCount(model, e.wireFor(providerID), req), usage.PromptTokens)
	e.tokMu.Lock()
	e.tokCompared++
	material := tokenDivergenceMaterial(local, usage.PromptTokens)
	if material {
		e.tokDiverged++
		e.lastDivergence = TokenDivergence{
			Provider: providerID,
			Model:    model,
			Local:    local,
			Reported: usage.PromptTokens,
			At:       time.Now(),
		}
		e.hasLastDivergence = true
	}
	e.tokMu.Unlock()
	if material && e.cfg.OnTokenDivergence != nil {
		e.cfg.OnTokenDivergence(TokenDivergence{
			Provider: providerID,
			Model:    model,
			Local:    local,
			Reported: usage.PromptTokens,
			At:       time.Now(),
		})
	}
	return usage
}

// streamFallbackUsage is the ledger usage for a successful stream
// whose upstream reported NO usage: the local prompt count is the
// fallback, labelled LocalEstimate — the same rule as
// reconcileUsage's no-report branch. A 0 local count records
// Usage{} (unknown), never a guessed split. When the upstream DID
// report streaming usage (G1: the Done event's reassembled
// response carries it), runStream reconciles that report instead
// and never calls this fallback.
func (e *Engine) streamFallbackUsage(providerID, model, wire string, req provider.ChatRequest) provider.Usage {
	local := e.localPromptCountCalibrated(providerID, model, wire, req)
	if local <= 0 {
		return provider.Usage{}
	}
	return provider.Usage{PromptTokens: local, TotalTokens: local, LocalEstimate: true, LocalEstimatePrompt: local}
}

// noteSuccess records a successful attempt in the trace, ledger and
// circuit breaker — and feeds its RTT into the adaptive limiter, so
// a provider that slows down gets its cap shrunk for the next
// requests. Only successes feed the gradient: a failed call's
// latency is not a serving RTT.
//
// The recorded usage is reconciled against the local token count
// (see reconcileUsage): the provider's numbers are kept verbatim,
// the local count fills in only when the upstream reported nothing,
// and a material divergence is counted and surfaced, never
// silently resolved.
func (e *Engine) noteSuccess(trace *routetrace.RouteTrace, res attemptResult, req provider.ChatRequest) {
	addTraceAttemptReasoning(trace, res.att, res.latencyMs, "success", nil, res.reasoning)
	// Reasoning capture (CHECKLIST 163): store the response's
	// reasoning state under the transcript key, so the next
	// turn — or a failover — can restore it.
	e.captureReasoning(req, res.resp)
	if res.called {
		e.noteAttemptStats(res.att.ProviderID, res.att.KeyID, true, res.latencyMs)
	}
	if e.ledger != nil && res.called {
		usage := provider.Usage{}
		if res.resp != nil {
			usage = res.resp.Usage
		}
		usage = e.reconcileUsage(res.att.ProviderID, res.att.Model, req, usage)
		usage = withCompression(usage, req)
		e.recordAttempt(req.CallerID, res.att, true, res.latencyMs, usage)
	}
	e.breakerSuccess(res.att.ProviderID)
	// A success clears any lockout on the triple (M4.3): a
	// recovered model serves again at once.
	e.lockoutClear(res.att)
	// A success ends the cooldown — hard and soft alike — and
	// resets the soft consecutive-failure count, so the next
	// failure starts again from the base duration.
	e.clearCooldown(ClientKey(res.att.ProviderID, res.att.KeyID))
	e.clearSoftCooldown(ClientKey(res.att.ProviderID, res.att.KeyID))
	e.limiter.Record(res.att.ProviderID, time.Duration(res.latencyMs)*time.Millisecond, true)
	e.reportConcurrencyLimit(res.att.ProviderID)
}

// noteFailure records a failed attempt. It returns whether the error
// was a rate limit, how long to wait before retrying the SAME target
// (0 when the target is abandoned — see below), and whether the
// target is abandoned for this request. 429s do NOT trip the circuit
// breaker (throttling is not the provider being down); they set a
// per-provider cooldown instead.
// 4xx client errors (400/401/403/422) do NOT trip it either — a bad
// key or a bad request is not provider unhealthiness, so five of
// them must not take down a provider's live models; they are counted
// as breaker-ignored 4xx instead. Only 408/5xx and transport
// failures count toward the breaker. Context cancellation and
// unknown providers likewise never touch the breaker. mode is the
// resolved cooldown mode for this request: in soft-demote mode every
// retryable failure (not just 429s) starts a soft cooldown and never
// trips the breaker — tripping it would skip the provider outright,
// which demotion refuses to do.
//
// Abandon (CHECKLIST 167): a Retry-After >= 60s abandons the target
// for this request — wait is 0 and abandoned is true, so the caller
// moves to the next target or fails honestly instead of sleeping out
// the wait and retrying it. The cooldown the target gets is still
// the floored/capped wait (never more than RetryAfterCap), so the
// next request does not hammer it immediately; only the in-request
// retry is abandoned.
//
// chainKey is the LKGP chain key for this request (the requested
// model; "" when the surface carries no LKGP chain, e.g.
// embeddings): a hard failure against the pinned provider clears
// that chain's pin via Config.LKGPInvalidator (RT-2) — see
// invalidateLKGP. An empty chainKey disables the clear.
func (e *Engine) noteFailure(trace *routetrace.RouteTrace, res attemptResult, consecutive429 *int, mode, keyID, chainKey string) (rateLimited bool, wait time.Duration, abandoned bool) {
	// A shed call is not a failure: the provider was never tried
	// (called=false), so it touches neither the ledger nor the
	// breaker — it is only recorded in the trace as skipped, so
	// the operator can see the shed happened.
	if errors.Is(res.err, adaptive.ErrLimited) {
		addTraceAttempt(trace, res.att, 0, "skipped_concurrency_limit", res.err)
		return false, 0, false
	}
	// A real attempt that reached the provider: count it in the
	// live stats (except caller/engine cancellations, which say
	// nothing about the provider). Rate limits count as failures
	// — the provider did not serve the request.
	if res.called && !errors.Is(res.err, context.Canceled) && !errors.Is(res.err, context.DeadlineExceeded) {
		e.noteAttemptStats(res.att.ProviderID, res.att.KeyID, false, res.latencyMs)
	}
	if e.ledger != nil && res.called {
		// Failed attempt: record the usage the upstream
		// reported on the attempt's own response when it
		// still carries one (a refusal it billed for), and
		// Usage{} (unknown) when it reported nothing — never
		// a guessed split. See failedUsage.
		e.recordAttempt(keyID, res.att, false, res.latencyMs, failedUsage(res))
	}
	if errors.Is(res.err, context.Canceled) || errors.Is(res.err, context.DeadlineExceeded) {
		addTraceAttempt(trace, res.att, res.latencyMs, "error", res.err)
		return false, 0, false
	}
	// Model lockout (M4.3): record the failure on the triple
	// before any branch returns — the 429, model-not-found and
	// client-error branches below all return early, and a 404
	// dead model must lock exactly like a 429 does.
	e.lockoutRecord(res.att, res.err)
	if !e.hasClient(res.att.ProviderID) {
		addTraceAttempt(trace, res.att, res.latencyMs, "error", res.err)
		return false, 0, false
	}
	if ra, present, ok := rateLimitHint(res.err); ok {
		// M4.4: a 429 is not always a plain rate limit — the
		// body classifies it. A quota-text 429 is
		// quota_exhausted: the account cannot serve, so the
		// target is abandoned for this request (never a
		// same-target retry) and cools for the honoured hint
		// or 60 s. A concurrency-limit 429 fails over now,
		// cools for exactly 1 s, and never climbs the backoff
		// ladder (consecutive429 is not incremented) and never
		// counts toward the breaker. The classification reads
		// the RateLimitError body — every wire maps its 429
		// message into Body, so all four wires classify
		// uniformly here, never per-wire.
		var rle *provider.RateLimitError
		body := ""
		if errors.As(res.err, &rle) && rle != nil {
			body = rle.Body
		}
		key := ClientKey(res.att.ProviderID, res.att.KeyID)
		soft := NormalizeCooldownMode(mode) == CooldownSoftDemote
		var retryInfo *routetrace.RetryAfterInfo
		switch {
		case wire.IsQuotaExhaustedSignal(body):
			abandoned = true
			wait = 0
			cool := retryAfterHonouredWait(ra, present, e.cfg.RetryAfterCap)
			if cool <= 0 {
				cool = QuotaExhaustedCooldown
			}
			if soft {
				e.startSoftCooldown(key, ra, present)
			} else {
				e.setCooldown(key, cool)
			}
			retryInfo = &routetrace.RetryAfterInfo{Seconds: ra.Seconds(), Decision: RetryDecisionQuotaExhausted, WaitSeconds: 0}
		case wire.IsConcurrencyLimitSignal(body):
			// No backoff climb: consecutive429 untouched, so
			// the next 429 on this target still backs off
			// from the base. Wait is the 1 s cool: it is
			// only slept when this was the last target —
			// with further targets the loop fails over now
			// and never sleeps it.
			wait = ConcurrencyLimitCooldown
			if soft {
				e.setSoftCooldownUntil(key, ConcurrencyLimitCooldown)
			} else {
				e.setCooldown(key, ConcurrencyLimitCooldown)
			}
			retryInfo = &routetrace.RetryAfterInfo{Seconds: ra.Seconds(), Decision: RetryDecisionConcurrencyLimited, WaitSeconds: wait.Seconds()}
		default:
			*consecutive429++
			wait = e.rateLimitWait(ra, present, *consecutive429)
			_, _, abandoned = classifyRetryAfter(ra, present, e.cfg.RetryAfterCap)
			if soft {
				// Soft cooldown from the upstream's Retry-After
				// itself, floored at 10s (see softCooldownDuration):
				// the doubling cap (5 min) is the only other bound.
				e.startSoftCooldown(key, ra, present)
			} else {
				// Hard cooldown from the floored/capped wait — an
				// abandoned target still cools (at most
				// RetryAfterCap), so the next request does not
				// hammer it; only this request's retry is abandoned.
				// With no Retry-After hint the honoured wait is 0:
				// fall back to the per-auth cooldown base
				// (OAuth 5s / API-key 3s / Local 2s) so a plain
				// 429 still cools the target instead of
				// re-admitting it immediately. A present hint
				// still wins — it is floored at 10s, so the base
				// can never shrink a hinted wait (B2-529F).
				cool := retryAfterHonouredWait(ra, present, e.cfg.RetryAfterCap)
				if cool <= 0 {
					cool = e.cooldownBaseFor(res.att.ProviderID)
				}
				e.setCooldown(key, cool)
			}
			retryInfo = retryInfoFor(ra, present, e.cfg.RetryAfterCap)
		}
		e.breakerNeutral(res.att.ProviderID)
		addTraceAttemptRetryReasoning(trace, res.att, res.latencyMs, "rate_limited", res.err, retryInfo, res.reasoning)
		return true, wait, abandoned
	}
	*consecutive429 = 0
	if isReasoningUnrepresentableErr(res.err) {
		// The target wire cannot represent the restored
		// reasoning state — a property of that target, not of
		// the request and not of the provider's health: the
		// breaker stays neutral, no cooldown is set, and the
		// chain continues to a target that can carry the
		// reasoning (see isReasoningUnrepresentableErr). The
		// trace label says so (res.reasoning).
		e.breakerNeutral(res.att.ProviderID)
		addTraceAttemptReasoning(trace, res.att, res.latencyMs, "error", res.err, res.reasoning)
		return false, 0, false
	}
	if NormalizeCooldownMode(mode) == CooldownSoftDemote && isRetryableFailure(res.err) {
		// Demote, don't drop: the target cools (5 s base, or
		// its Retry-After floored at 10s, doubling per
		// consecutive failure) and moves to the end of the next
		// request's order. The breaker is left alone — it would
		// skip the provider, which demotion refuses to do.
		// A hard failure still clears the LKGP pin (RT-2):
		// in soft-demote mode this branch is where 5xx /
		// timeout / transport failures land instead of the
		// breakerFailure fall-through below, and the pin
		// must not keep hoisting a provider that just failed
		// hard. invalidateLKGP itself skips non-hard members
		// of this branch (429s, 402s, 404s).
		ra, present := retryAfterOf(res.err)
		e.startSoftCooldown(ClientKey(res.att.ProviderID, res.att.KeyID), ra, present)
		e.breakerNeutral(res.att.ProviderID)
		e.invalidateLKGP(chainKey, res.att.ProviderID, res.err)
		addTraceAttemptReasoning(trace, res.att, res.latencyMs, "error", res.err, res.reasoning)
		return false, 0, false
	}
	if isModelNotFoundErr(res.err) {
		// The provider answered fine — only the model is dead
		// at this target — so the failure must not count
		// against the provider's circuit breaker: five
		// dead-model requests must not take down a provider's
		// live models. (In soft-demote mode the branch above
		// already handled it: demote + breakerNeutral.)
		e.breakerNeutral(res.att.ProviderID)
		addTraceAttemptReasoning(trace, res.att, res.latencyMs, "error", res.err, res.reasoning)
		return false, 0, false
	}
	if isClientErrorFailure(res.err) {
		// A 4xx is a property of the request or the
		// operator's key (bad request, bad key, forbidden,
		// unprocessable) — never a provider-health signal —
		// so it must never trip the circuit breaker, in
		// either cooldown mode: five bad-key requests must
		// not take down a provider's live models. The
		// failure is counted as breaker-ignored 4xx (live
		// proof in the dashboard feed) and the chain still
		// stops on it — the request itself is terminal, only
		// the breaker is left alone.
		//
		// The one exception is a dead key (401) inside a
		// multi-key pool (CHECKLIST 175): the key is cooled
		// down per-key, so the next request skips (or demotes)
		// that key while the provider's other keys keep
		// serving. Whether this request continues to the next
		// key is decided by the loop's isTerminalErrFor — a
		// 401 is not terminal while a sibling key remains.
		e.coolDeadKey(res.att, mode)
		e.breakerNeutral(res.att.ProviderID)
		e.countBreakerIgnored4xx(res.att.ProviderID)
		addTraceAttemptReasoning(trace, res.att, res.latencyMs, "error", res.err, res.reasoning)
		return false, 0, false
	}
	var ese *provider.EmptyStreamError
	if errors.As(res.err, &ese) && ese.FinishReason == "content_filter" {
		// The provider answered fine and deliberately
		// refused the prompt — that is not provider
		// unhealthiness, so the breaker stays neutral.
		// (The streaming half of the rule does the same in
		// runStream's streamEmpty branch; the failover
		// itself still happens — another target may serve
		// the prompt under a different filter.)
		e.breakerNeutral(res.att.ProviderID)
		addTraceAttemptReasoning(trace, res.att, res.latencyMs, "error", res.err, res.reasoning)
		return false, 0, false
	}
	e.breakerFailure(res.att.ProviderID, provider.ClassifyError(res.err, provider.EndpointChatCompletions).Reason)
	// A hard failure (5xx / timeout / transport — the class
	// that reaches this fall-through) clears the LKGP pin for
	// the chain (RT-2): the next request must not hoist a
	// provider that just failed hard, even though the pin's
	// 15-minute TTL has not lapsed.
	e.invalidateLKGP(chainKey, res.att.ProviderID, res.err)
	addTraceAttemptReasoning(trace, res.att, res.latencyMs, "error", res.err, res.reasoning)
	return false, 0, false
}

// noteHedgedLoser records an attempt that was started as a hedge and
// then cancelled because the other attempt won. The loser is a
// cancelled attempt, not a failure: it is stored as a cancelled
// row (see recordCancelledAttempt), never as an ok=false row.
func (e *Engine) noteHedgedLoser(trace *routetrace.RouteTrace, res attemptResult, keyID string) {
	addTraceAttemptReasoning(trace, res.att, res.latencyMs, "hedged_cancelled", context.Canceled, res.reasoning)
	if e.ledger != nil && res.called {
		e.recordCancelledAttempt(keyID, res.att, res.latencyMs)
	}
	e.breakerNeutral(res.att.ProviderID)
}

// ---------------------------------------------------------------------------
// Non-streaming chat.
// ---------------------------------------------------------------------------

// ChatWithChain routes one request through the fallback chain and returns
// the first successful response, the RouteTrace of every attempt, and nil;
// or a nil response, the trace, and the last error if no attempt
// succeeded. At most Config.MaxAttempts provider calls are made. The
// serving provider is recorded in the trace (RouteTrace.Attempts); the
// response itself carries only the provider's content, usage and model.
//
// The strategy for this request is the configured global strategy
// (or a ChainStrategy override for req.Model); see
// ChatWithChainStrategy for a per-request override.
//
// The server-facing Chat (no chain argument) delegates here after
// resolving the chain for req.Model; see chainFor.
func (e *Engine) ChatWithChain(ctx context.Context, chain []Attempt, req provider.ChatRequest) (*provider.ChatResponse, *routetrace.RouteTrace, error) {
	return e.ChatWithChainStrategy(ctx, chain, req, "")
}

// traceEndpointFor names the API surface a trace belongs to, for
// the dashboard's Live Routing view: "messages" for a translated
// /v1/messages request, "" for everything else the chat paths
// serve. Only messages is stamped here: chat keeps "" (the
// dashboard renders "" as chat, unchanged), the other surfaces
// stamp their own endpoint on their own paths (embeddings /
// rerank) or not at all — a fresh Responses create fails over like
// chat, so stamping it "responses" here would make the dashboard's
// replay-exempt line lie about it. (CHECKLIST 211.)
func traceEndpointFor(req provider.ChatRequest) string {
	if req.Endpoint == provider.EndpointMessages {
		return string(req.Endpoint)
	}
	return ""
}

// ChatWithChainStrategy is ChatWithChain with an explicit per-request
// strategy override (the "auto:<axis>" alias, already parsed by the
// caller): a non-empty strategy wins over the chain override and the
// global strategy. "" means "no per-request override".
func (e *Engine) ChatWithChainStrategy(ctx context.Context, chain []Attempt, req provider.ChatRequest, reqStrategy string) (resp *provider.ChatResponse, trace *routetrace.RouteTrace, err error) {
	return e.chatWithChainStrategy(ctx, chain, req, reqStrategy, false)
}

// failoverRetryAllowed reports whether a same-target transient
// retry may run given whether a next target exists in the chain
// (B2-30, the reference router #10217: failoverBeforeRetryExplicit skips the
// same-target retry when a next target exists, failing over
// immediately instead; with the flag off — the default, matching
// the reference router's effective default — or with no next target, the
// retry runs as before).
func (e *Engine) failoverRetryAllowed(hasNextTarget bool) bool {
	return !e.cfg.FailoverBeforeRetry || !hasNextTarget
}

// chatWithChainStrategy is ChatWithChainStrategy with the
// dispatched-chain flag: dispatchExplicitOrder is true when the
// chain came from dispatchAutoVariant as a multi-model ordered
// list (autoDispatch.OrderedExplicit) — the ladder/panel order
// is final, like a group member order (CHECKLIST 173). The
// public method keeps the old signature (no dispatch) so
// existing callers and tests are untouched.
func (e *Engine) chatWithChainStrategy(ctx context.Context, chain []Attempt, req provider.ChatRequest, reqStrategy string, dispatchExplicitOrder bool) (resp *provider.ChatResponse, trace *routetrace.RouteTrace, err error) {
	// Request-scoped attribution (CHECKLIST 317-318): the
	// finished trace is published into the caller's sink as
	// well as LastTrace, so the caller attributes this
	// request's outcome to its own trace even when a
	// neighbour finishes first. Registered BEFORE the panic
	// defer so it runs AFTER it (LIFO) and sees the
	// panic-published trace too.
	defer func() {
		if att := AttributionFrom(ctx); att != nil {
			att.PublishTrace(trace)
		}
	}()
	// Panic isolation (CHECKLIST 165): a panic in a hook (a
	// scoring/order hook in orderAttempts, a strategy hook) or in
	// the chain machinery becomes an ordinary error, not a dead
	// gateway. Per-attempt provider panics are already caught
	// closer in (callChat); this is the backstop for the machinery
	// around them. The panic is counted (notePanic) and its value
	// rides the PanicError to the server, which logs it redacted.
	// Attempts already recorded keep their trace entries and
	// ledger rows; an attempt that was never reached records
	// nothing (no provider was called for it).
	defer func() {
		if v := recover(); v != nil {
			e.notePanic("engine_chat")
			resp = nil
			err = NewPanicError("engine_chat", v)
			if trace == nil {
				trace = &routetrace.RouteTrace{}
			}
			// Publish the (attempt-less) trace: without this the
			// dashboard trace feed and the server's backstop-row
			// check would read the previous request's trace.
			// Safe: no panic point in this function runs under
			// e.mu (all lock sites here are brief bookkeeping;
			// hooks and provider clients run lock-free).
			e.setLastTrace(trace)
		}
	}()
	trace = &routetrace.RouteTrace{Endpoint: traceEndpointFor(req)}
	// Reasoning continuity (CHECKLIST 163): restore captured
	// reasoning states onto the assistant messages they belong
	// to, for this request only. The copy is local to the engine
	// call — the caller's request is never mutated.
	req = e.injectReasoning(req)
	// Lossless-first compression (CHECKLIST 183): decided here,
	// before the contract, the context-fit check and the budget
	// gate — all three must see the prompt as it will actually
	// be sent. A stamp from the server handler (same model) is
	// kept as-is; a re-decision after an "auto/*" resolve keeps
	// the original before-count. The trace carries the block from
	// here on, so even a rejected contract/group shows it.
	req = e.maybeCompress(req)
	trace.Compression = traceCompression(req.Compressed)
	// User-named routing group (CHECKLIST 173): a
	// group-addressed request routes through the group's members,
	// in order, as its chain — resolved before the contract is
	// applied, so the contract filters the group chain. A rejected
	// group publishes its trace (contract + Error, no attempts)
	// and returns before any provider is dialled, like a rejected
	// contract.
	req, chain, groupName, gerr := e.resolveGroupChain(chain, req)
	if gerr != nil {
		trace.Contract = groupFailureTrace(groupName, gerr).Contract
		e.setLastTrace(trace)
		return nil, trace, gerr
	}
	// Forced-reasoning policy (CHECKLIST 311-312): a group
	// replaces the chain the entry points stamped, so the
	// request-level decision (riding req) is re-stamped here —
	// without this a policy on the requested/resolved model
	// would silently not hold on a group-addressed request.
	chain = applyRequestForcedEffort(chain, req)
	// Per-request routing contract (CHECKLIST 157): applied
	// before any scoring or attempt. A rejected contract returns
	// the trace carrying the contract (with its Error) and no
	// attempts, so the dashboard shows why the request was
	// refused instead of a stale trace from an earlier request.
	chain, contract, sortStrat, cerr := e.applyContract(chain, req)
	// The trace contract names the group even when it arrived
	// as a "group:<name>/<model>" model alias (applyContract
	// only echoes routing.group, which the alias form never
	// sets) — and when the alias is the ONLY contract control,
	// applyContract returns a nil contract, so one is created
	// here: a group-addressed request always carries a contract
	// block naming its group. The routing.group form already
	// matches.
	if groupName != "" {
		if contract == nil {
			contract = &routetrace.RoutingContract{Group: groupName}
		} else {
			contract.Group = groupName
		}
	}
	if cerr != nil {
		trace.Contract = contract
		e.setLastTrace(trace)
		return nil, trace, cerr
	}
	// A group-resolved chain is an explicit operator order, like
	// a contract order (CHECKLIST 173): the group's member order
	// is final — no strategy hook, strategy tree, or quota-hint
	// sort may re-rank it. Without this the balanced hook would
	// reorder the group by live signals (a member with one
	// ledger failure drops below the next member), so the
	// operator's stated order would silently not hold. The
	// safety partitions still apply (see orderAttempts). A
	// dispatched multi-model auto chain is explicit the same
	// way: the thrifty ladder / subscription rung pool / chaos
	// panel order is the resolver's decision (try the
	// plan-included model first — it is free), and the
	// quota-hint sort plus the 16-factor cost-saver hook would
	// otherwise invert it before the first attempt.
	explicitOrder := dispatchExplicitOrder || groupName != "" || (contract != nil && len(contract.Order) > 0)
	// The contract's sort wins over the auto:<axis> alias for
	// this request (precedence: contract sort > alias > chain >
	// global). The trace records the effective strategy.
	if sortStrat != "" {
		reqStrategy = sortStrat
	}
	// Responses-continuation pin (CHECKLIST 169, reordered by
	// 237-238): the provider that served the original response
	// goes first when it still serves this model; the rest of
	// the chain follows, so a failing pin fails over instead of
	// ending the request.
	chain = pinChain(chain, req.PinProviderID)
	strategy := e.effectiveStrategy(reqStrategy, req.Model)
	// Nestable strategy tree (CHECKLIST 177): when a tree
	// orders this request, the strategy the trace and the
	// last-routing record name is the tree root's strategy
	// (or the per-request override that replaced it) — never
	// the flat strategy the tree replaced. treeRoot is "" when
	// no tree orders this request.
	tree, treeRoot, treeOrdered := e.treeRootStrategy(req.Model, reqStrategy, explicitOrder)
	if treeOrdered {
		strategy = treeRoot
	}
	// Hedging vs explicit order (CHECKLIST 191): when the chain
	// order is EXPLICIT — a per-request contract order, a
	// routing-group member order, or a strategy-tree order — the
	// operator or client stated which provider serves first, and
	// the default 150 ms hedge must not silently override it:
	// the hedge races the second attempt, so a first pick slower
	// than the delay loses to the hedge (the 177-178 slice's live
	// trace showed `hedged_cancelled` for the tree's first pick).
	// The rule is NO hedge on explicit-order chains, not a longer
	// delay: any delay is still a race, and the first pick's
	// expected latency is only an EWMA estimate — only disabling
	// the hedge guarantees the stated order holds. The cost is
	// explicit: an explicit-order request loses the hedge's
	// latency insurance and waits out a slow first pick;
	// sequential failover on an actual failure is unchanged. The
	// default (engine-computed) chain keeps the 150 ms hedge
	// exactly as before — a flat cheapest chain still hedges,
	// because the engine computed that order itself; an operator
	// who wants cheapest-first guaranteed uses a contract order,
	// a group, or a tree, and the hedge is off by design.
	hedgeOff := explicitOrder || treeOrdered
	if contract != nil {
		contract.Strategy = strategy
	}
	trace.Contract = contract
	// Endpoint projection (Phase D, D4): a combo strategy the
	// request endpoint does not allow fails closed here, before
	// any ordering or combo dispatch — see
	// projectStrategyForEndpoint (cascade.go). Chat allows every
	// strategy; "" means chat.
	if projErr := checkEndpointStrategy(strategy, req.Endpoint); projErr != nil {
		e.setLastTrace(trace)
		return nil, trace, projErr
	}
	mode := e.cooldownModeFor(req.Model)
	// Replay exemption (preread.go): a stateful request is never
	// replayed — no hedge, exactly one attempt. Decided once per
	// request, from req.
	exempt := ReplayExemption(req)
	e.recordRouting(strategy, req.Model)
	// The per-combo deadline budget bounds the whole chain: every
	// attempt, backoff and hedge runs under bctx, so the budget
	// cannot be outrun by retrying. The caller's own ctx is kept
	// as parent, so a caller cancellation still reports the
	// caller's error, not ErrDeadlineExceeded.
	bctx, cancel := e.withDeadline(ctx)
	defer cancel()
	parentCtx := ctx
	ctx = bctx
	if len(chain) == 0 {
		return nil, trace, errors.New("router: empty fallback chain")
	}
	if err := ctx.Err(); err != nil {
		return nil, trace, e.mapDeadlineErr(err, parentCtx)
	}
	// An explicit contract order is final: no scoring may re-rank
	// it (CHECKLIST 157 — the balanced hook used to re-rank the
	// chain by probe latency after the contract had ordered it).
	// A strategy tree orders the chain instead of the flat
	// strategy hook when one is configured for this model
	// (CHECKLIST 177) — except under an explicit contract order,
	// which skips the tree entirely. A per-request strategy
	// override (reqStrategy: the contract's sort or an
	// auto:<axis> alias) replaces the tree's ROOT strategy
	// only; the tree structure still applies.
	// Per-request hook context (M6.5-wiring): the prompt count,
	// tools and temperature the strategy hook needs, built from
	// the request as it will actually be sent.
	hookCtx := e.strategyHookContextFor(req, explicitOrder)
	var ordered, zeroSkipped []Attempt
	if treeOrdered {
		var err error
		ordered, zeroSkipped, err = e.orderAttemptsByTreeWithContext(chain, tree, reqStrategy, mode, hookCtx)
		if err != nil {
			e.setLastTrace(trace)
			return nil, trace, err
		}
	} else {
		ordered, zeroSkipped = e.orderAttemptsWithContext(chain, strategy, mode, explicitOrder, hookCtx)
	}
	// Pin first (CHECKLIST 243): the ordering above (quota-hint
	// sort, strategy hook) can demote the pin below other
	// targets — re-apply it now, so a Responses continuation
	// tries its original provider first whatever the scoring
	// says. pinFirst never adds or resurrects a target.
	ordered = pinFirst(ordered, req.PinProviderID)
	for _, att := range zeroSkipped {
		addTraceAttempt(trace, att, 0, "skipped_zero_quota", nil)
	}
	// P1.5 capability filter: with operator-required capabilities
	// (routing.require_capabilities), an attempt must hold every one
	// of them with VERIFIED support — unknown and merely-declared
	// never qualify — and the filter runs before any dial, so a
	// dropped attempt costs no provider call. No requirements leaves
	// the chain untouched.
	ordered, capSkipped, capReasons := e.filterByRequiredCapabilities(ordered)
	for i, att := range capSkipped {
		reason := "skipped_capability_unverified"
		if i < len(capReasons) {
			reason = capReasons[i]
		}
		addTraceAttempt(trace, att, 0, reason, nil)
	}
	if len(ordered) == 0 {
		return nil, trace, ErrNoAvailableAttempt
	}
	// Context-window pre-call fit check (CHECKLIST 159): an
	// oversize prompt never dials the too-small model — the
	// check removes too-small attempts from the chain (trace:
	// skipped_context_fit) and jumps to the dedicated
	// long-context fallback chain when nothing in the primary
	// chain fits; when nothing fits anywhere it fails honestly
	// here, before any provider is called.
	// AUD-PERF-004: compression and reasoning restore have run, so
	// this request's messages are final for the request — attach a
	// per-request token-count memo. Every later estimate (this
	// context-fit gate, the per-hop budget gate below, reconcileUsage)
	// then reuses one CountPromptWire per (model, wire) instead of
	// re-joining, re-hashing and re-marshalling the prompt on each
	// hop. A caller that already attached one (a request re-entering
	// the engine) is left as-is.
	if req.TokenCountMemo() == nil {
		req.SetTokenCountMemo(tokencount.NewRequestMemo())
	}
	ordered, err = e.applyContextFit(trace, ordered, req)
	if err != nil {
		e.setLastTrace(trace)
		return nil, trace, err
	}

	// Combo strategies (Phase D, D3): fusion / pipeline / jev /
	// chaos-mode execute over the ordered chain instead of the
	// sequential loop below — see runComboStrategy (combo.go).
	// Replay-exempt requests never fan out (exactly one attempt).
	if isComboStrategy(strategy) && exempt == "" {
		comboResp, comboErr := e.runComboStrategy(ctx, ordered, req, trace, strategy, contract, tree, treeRoot, treeOrdered, mode)
		e.setLastTrace(trace)
		return comboResp, trace, comboErr
	}

	// Context-relay (Phase D, D5): when the effective strategy is
	// context-relay, one relay state owns this request — nil for
	// every other strategy, so they are untouched. The state is
	// created here, after ordering and the combo dispatch, so a
	// request that never reaches the loop (empty chain, contract
	// rejection, combo dispatch) never owns one; see relay.go.
	relay := e.newRelayFor(strategy, req.CallerID)
	// Cross-request relay (B2-01): a handoff wanted but never
	// built in this request is stored for the next request under
	// the same gateway key when this function returns, on every
	// exit path. The store holds the want, never a built
	// handoff — see storeRelayWantIfPending in relay.go.
	defer e.storeRelayWantIfPending(req.CallerID, relay)

	maxAttempts := e.cfg.MaxAttempts
	attemptsUsed := 0
	consecutive429 := 0
	// Wait-budget consumption for this request (M4.8): how many
	// waits have been slept, and their total duration.
	waitsUsed := 0
	var waitedTotal time.Duration
	// Same-host transient retry state (M4.5): the count and the
	// jitter ladder belong to one target — a new target restarts
	// both. transientAtt is the attempt the count belongs to.
	var transientAtt Attempt
	transientCount := 0
	var transientPrev time.Duration
	// G11 Area 2: per-request deadline-failure sub-budget count
	// (deadline_budget.go) and set-wide same-target retry count
	// (failover_policy.go). Both are per-request state, never
	// engine state.
	deadlineFailures := 0
	setRetriesUsed := 0
	// Identical-error early stop state (M4.7d): the streak of
	// consecutive identical model-shape 400s for this request.
	identicalErr := &identicalErrorTracker{}
	// CT-1: capability-mismatch failover state — at most ONE
	// failover per request may be spent on a tool-capability
	// mismatch 400. A second mismatch 400 is terminal: the
	// chain must not walk every target on a request no
	// confirmed-capable target served.
	capabilityFailoverUsed := false
	// Refusal-chain state (CHECKLIST 161): active is the chain
	// being walked — the failure chain until the first content
	// refusal, then the refusal chain. tried is every provider
	// already attempted in this request: a refusing target is
	// never retried, and the refusal chain never re-offers one.
	// refusalExhausted is set when the refusal chain runs dry
	// (no further target to try), so the end of the loop can
	// tell an exhausted chain from a stopped one.
	active := ordered
	refusalMode := false
	refusalExhausted := false
	tried := make(map[string]bool)
	// pinEscapeCounted is the once-per-request guard for the
	// pin-escape counter (see the loop top below).
	pinEscapeCounted := false
	// regionCounted is the once-per-failure guard for the
	// region-aware failover counter (CHECKLIST 257): the trace
	// index of the last failure already counted from, so one
	// failure is never counted twice (see
	// noteRegionFailoverFromTrace).
	regionCounted := -1
	// Hedge decision for this request (CHECKLIST 191): stamped
	// on the trace before the loop, so the dashboard can tell a
	// hedged request from an explicit-order one even when the
	// first attempt answers before the delay and no hedge ever
	// fires.
	e.stampHedgeMode(trace, hedgeOff, exempt, len(active), req)
	// spentUSD is this request's cumulative priced spend (see
	// requestbudget.go): failed attempts bill nothing, so it
	// stays $0 until an attempt reports priced usage — but the
	// accumulator is the invariant the gate checks, not just
	// today's $0.
	spentUSD := 0.0
	var lastErr error
	// firstBreach is the first per-key token-limit breach seen
	// while walking the chain (M5.3): when every target is
	// breached the loop ends with no attempt and no lastErr, so
	// the breach — not the generic no-attempts error — is the
	// request error.
	var firstBreach *TokenLimitBreach
	// firstBudgetBreach is the same for provider-scoped internal
	// budgets (M5.11).
	var firstBudgetBreach *InternalBudgetBreach
	idx := 0
	for idx < len(active) && attemptsUsed < maxAttempts {
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}
		att := active[idx]
		// G11 Area 2: cross-provider attempt cap (default 3
		// distinct providers) and routing.only narrowing (a
		// prefixed provider/model is never rerouted) — see
		// failover_policy.go. `tried` holds the providers
		// already attempted, so a new provider past the budget
		// ends the request with the last real error (the tail
		// maps a nil lastErr to ErrNoAvailableAttempt, but the
		// cap can only trip after attempts happened, so lastErr
		// is set).
		if !providerAttemptAllowed(resolvedOnly(contract), tried, att.ProviderID, e.maxProvidersPerRequest()) {
			break
		}
		// Same-host transient retry state (M4.5): a new target
		// restarts the count and the jitter ladder.
		if att != transientAtt {
			transientAtt = att
			transientCount = 0
			transientPrev = 0
		}
		// Pin escape (CHECKLIST 243): the loop has moved past
		// the pinned provider after trying it — a retryable
		// failure on the pin must escape it, and the escape is
		// counted once per request, here at the loop top,
		// wherever the advance came from (429, 5xx, refusal,
		// hedge, empty stream). A pin that was only skipped
		// (cooldown / breaker) was never tried, so tried[pin]
		// is false and nothing counts: an escape needs a
		// failure on the pin, not a skip.
		if req.PinProviderID != "" && !pinEscapeCounted && tried[req.PinProviderID] && att.ProviderID != req.PinProviderID {
			pinEscapeCounted = true
			e.notePinEscape(req.PinProviderID, att.ProviderID)
		}
		// Hard mode skips a cooling provider outright. Soft
		// mode never skips: the demoted target sits at the end
		// of the order (see orderAttempts) and is still tried
		// once, so a request fails only if every target fails.
		// G11 Area 2: the staggered gate (onCooldownStaggered)
		// re-admits cooled keys spread over half a second.
		if NormalizeCooldownMode(mode) == CooldownHard && e.onCooldownStaggered(ClientKey(att.ProviderID, att.KeyID)) {
			addTraceAttempt(trace, att, 0, "skipped_cooldown", nil)
			idx++
			continue
		}
		if !e.breakerAllow(att.ProviderID) {
			addTraceAttempt(trace, att, 0, "skipped_breaker", nil)
			idx++
			continue
		}
		// Model lockout (M4.3): a locked (provider, key, model)
		// triple is skipped until its cooldown ends — the next
		// target serves instead of burning an attempt on a dead
		// model. Off by default: lockoutLocked is false when the
		// lockout is disabled, so this changes nothing there.
		if _, locked := e.lockoutLocked(att); locked {
			addTraceAttempt(trace, att, 0, "skipped_model_lockout", nil)
			idx++
			continue
		}
		// Predictive TTFT breaker (M4.7a): a target whose mean
		// first-byte latency exceeds PredictiveTtftMs is
		// skipped — but only while another target remains in
		// the chain. A slow provider still serves when it is
		// the only one left: slow beats failing.
		if idx+1 < len(active) && e.ttftSlow(att.ProviderID) {
			addTraceAttempt(trace, att, 0, "skipped_predictive_ttft", nil)
			idx++
			continue
		}
		// Per-key token limits (M5.3): a target whose scope is
		// already at its window cap is skipped — the next target
		// serves instead of burning an attempt on a capped
		// (provider, model). A ledger error fails closed: the
		// chain stops with it rather than waiving a limit that
		// could not be read.
		if breach, err := e.tokenLimitBreach(ctx, req, att); err != nil {
			addTraceAttempt(trace, att, 0, "skipped_token_limit", err)
			e.setLastTrace(trace)
			return nil, trace, err
		} else if breach != nil {
			if firstBreach == nil {
				firstBreach = breach
			}
			addTraceAttempt(trace, att, 0, "skipped_token_limit", nil)
			idx++
			continue
		}
		// Internal budgets, provider scope (M5.11): a target
		// whose provider is over its internal budget is skipped
		// the same way — the next target serves instead of
		// spending on a capped provider. A ledger error fails
		// closed, like the token-limit check above.
		if breach, err := e.internalBudgetBreach(ctx, att); err != nil {
			addTraceAttempt(trace, att, 0, "skipped_internal_budget", err)
			e.setLastTrace(trace)
			return nil, trace, err
		} else if breach != nil {
			if firstBudgetBreach == nil {
				firstBudgetBreach = breach
			}
			addTraceAttempt(trace, att, 0, "skipped_internal_budget", nil)
			idx++
			continue
		}

		// Per-request budget (CHECKLIST 155): in strict mode
		// the chain stops BEFORE an attempt whose priced prompt
		// estimate would push cumulative spend over the budget.
		// The attempt is never dialled; the trace records the
		// skip and the caller gets a typed 402 error. In
		// non-strict mode the gate never blocks — the server
		// records the overage post-hoc instead.
		blocked, est := e.budgetGate(req, att, spentUSD)
		if blocked {
			be := e.budgetExceededError(req, att, spentUSD, est)
			addTraceAttempt(trace, att, 0, "skipped_budget", be)
			e.setLastTrace(trace)
			return nil, trace, be
		}

		// Region-aware failover step (CHECKLIST 257): this
		// attempt is about to be dialled after the cooldown,
		// breaker and budget gates passed — if the trace shows
		// a retryable failure before it, count the step it
		// represents (cross-region, equivalent-model or
		// self-hosted), once per failure. Placed here, not at
		// the loop top, so a target skipped by a gate is never
		// counted as stepped-to.
		e.noteRegionFailoverFromTrace(trace, att, &regionCounted)

		// CT-1 direct-path tool-capability pre-check: a
		// tool-bearing request never dials a CONFIRMED-incapable
		// model — the target is skipped (trace: skipped, no
		// attempt spent, no provider call) and the chain moves
		// on. Unknown models fail open: a model never probed
		// and never synced is tried, never assumed incapable.
		// Non-tool requests skip this gate entirely.
		if len(req.Tools) > 0 {
			if capable, known := e.modelToolsCapable(att.ProviderID, att.Model); known && !capable {
				addTraceAttempt(trace, att, 0, "skipped_no_tool_capability", nil)
				idx++
				continue
			}
		}

		// Context-relay (Phase D, D5): the request this attempt
		// is dialled with — the original request, or the
		// original plus the pending handoff as a leading system
		// message when a previous attempt requested one. The
		// handoff is built lazily here, only when an attempt is
		// actually about to be dialled (see relayAttemptReq):
		// a target skipped by a gate above never costs a
		// summariser call. The budget gate above still prices
		// the original request, not the relayed one — the
		// handoff's tokens are not in its estimate (a known
		// undercount: the handoff's tokens are priced nowhere).
		attemptReq := e.relayAttemptReq(ctx, relay, req)

		// Hedged first attempt: only for the first attempt, only
		// while budget remains, and only if the second attempt is
		// actually available to hedge to. A hedge target the
		// budget gate would block is never launched in parallel:
		// the first attempt runs alone instead (the gate above
		// already cleared it). The hedge gate counts the FIRST
		// attempt's estimate too (spentUSD + est): the first
		// attempt is still in flight when the hedge fires, so its
		// cost is not yet in spentUSD — gating the second attempt
		// against spentUSD alone let a hedged pair whose joint
		// prompt estimate exceeds the budget launch (M5.14).
		// Never on an explicit-order chain (CHECKLIST 191 —
		// hedgeOff): the stated order is final.
		hedgeBlocked := false
		if len(active) > 1 {
			hedgeBlocked, _ = e.budgetGate(req, active[1], spentUSD+est)
		}
		if idx == 0 && attemptsUsed == 0 && exempt == "" && !hedgeOff && e.hedgeAllowed(req) && len(active) > 1 && !hedgeBlocked && e.hedgeSecondAvailable(active[1], mode) {
			r1, r2, launched := e.hedgedChat(ctx, att, active[1], attemptReq)
			attemptsUsed++
			if launched {
				attemptsUsed++
			}
			// A "content_filter" finish is a refusal delivered
			// as a response, not a success: convert it to a
			// refusal error now, so the refusal handling below
			// (never the same target, never a health signal)
			// applies to it exactly like an error refusal.
			if r1.err == nil && isContentFilterFinish(r1.resp) {
				r1.err = contentRefusalError(r1.att)
			}
			if launched && r2 != nil && r2.err == nil && isContentFilterFinish(r2.resp) {
				r2.err = contentRefusalError(r2.att)
			}
			// Cumulative spend for the budget gate (see the
			// note on spentUSD above): hedge results price the
			// same way — a failed hedge bills nothing.
			spentUSD += e.attemptCost(att, r1.resp)
			if launched && r2 != nil {
				spentUSD += e.attemptCost(r2.att, r2.resp)
			}
			// Context-relay (Phase D, D5): feed both hedged
			// outcomes into the relay before the branch below
			// decides the next step — the content-filter
			// conversions above already ran, so a refusal is
			// recorded as a failure, never a success. The
			// hedge loser is NOT recorded when the first
			// finisher succeeded: hedgedChat returns it as a
			// synthetic context.Canceled result, and a
			// cancellation by the race is not an attempt
			// outcome — recording it would invent a failure.
			// A genuine second result (after a first failure)
			// is recorded.
			e.relayNote(relay, req, r1)
			if launched && r2 != nil && r1.err != nil && !errors.Is(r2.err, context.Canceled) {
				e.relayNote(relay, req, *r2)
			}
			if r1.err == nil {
				e.noteSuccess(trace, r1, req)
				if launched && r2 != nil {
					e.noteHedgedLoser(trace, *r2, req.CallerID)
				}
				e.recordDecision(trace, contract, treeOrdered, strategy, req.Model, r1.att.Model, r1.att.ProviderID)
				noteTreeServed(trace, tree, r1.att.ProviderID, r1.att.Model, treeRoot)
				e.setLastTrace(trace)
				return e.finishResponse(r1), trace, nil
			}
			// r1 failed: record it refusal-aware (a refusal
			// touches no health signal — see noteRefusal),
			// then let the hedge result decide the next step.
			r1Refusal := isContentRefusalErr(r1.err)
			if r1Refusal {
				e.noteRefusal(trace, r1, req.CallerID)
			} else {
				_, _, _ = e.noteFailure(trace, r1, &consecutive429, mode, req.CallerID, req.Model)
			}
			tried[r1.att.ProviderID] = true
			lastErr = r1.err
			if !launched {
				// First attempt failed before the hedge fired.
				// A terminal failure (malformed request, and
				// the like) stops the chain here: no other
				// target could serve it, so the fallback target
				// is never dialled for it. A refusal instead
				// switches to the refusal chain: the next
				// target is the failure chain's remaining
				// targets (or the configured refusal chain),
				// each tried once — never the refusing target.
				// A 401 is not terminal while a sibling key of
				// the same provider remains (see
				// isTerminalErrFor): the dead key's siblings
				// may still serve the request.
				if isTerminalErrFor(r1.err, active, 1, r1.att) {
					// CT-1: a capability-mismatch 400 is not
					// terminal for a tool-bearing request —
					// fail over once to a confirmed-capable
					// target instead of stopping the chain.
					if len(req.Tools) > 0 && !capabilityFailoverUsed && isCapabilityMismatchErr(r1.err) {
						if ni := e.capabilityFailoverIndex(active, 1); ni >= 0 {
							capabilityFailoverUsed = true
							idx = ni
							continue
						}
					}
					break
				}
				if r1Refusal {
					refusalMode = true
					if rc := e.refusalChainFor(req.Model, ordered, 1, tried); len(rc) == 0 {
						refusalExhausted = true
						break
					} else {
						active, idx = rc, 0
					}
					continue
				}
				idx = 1
				continue
			}
			if r2 != nil {
				if r2.err == nil {
					e.noteSuccess(trace, *r2, req)
					e.recordDecision(trace, contract, treeOrdered, strategy, req.Model, r2.att.Model, r2.att.ProviderID)
					noteTreeServed(trace, tree, r2.att.ProviderID, r2.att.Model, treeRoot)
					e.setLastTrace(trace)
					return e.finishResponse(*r2), trace, nil
				}
				if isContentRefusalErr(r2.err) {
					// Both hedged targets refused the content:
					// switch to the refusal chain from the next
					// target after the hedge pair.
					e.noteRefusal(trace, *r2, req.CallerID)
					tried[r2.att.ProviderID] = true
					lastErr = r2.err
					refusalMode = true
					if rc := e.refusalChainFor(req.Model, ordered, 2, tried); len(rc) == 0 {
						refusalExhausted = true
						break
					} else {
						active, idx = rc, 0
					}
					continue
				}
				_, _, _ = e.noteFailure(trace, *r2, &consecutive429, mode, req.CallerID, req.Model)
				tried[r2.att.ProviderID] = true
				lastErr = r2.err
			}
			// The first attempt was terminal, and the hedge
			// (already in flight when the verdict arrived) did
			// not save it: the chain stops — a further target
			// cannot fix a request this target refused as
			// invalid. (A hedge that DID succeed returned above:
			// that success proves the refusal was specific to
			// the first target, not to the request.) A first
			// refusal, by contrast, keeps routing: the hedge
			// target failed for another reason, so the refusal
			// chain continues from the next target. A 401 is
			// not terminal while a sibling key remains (see
			// isTerminalErrFor).
			if isTerminalErrFor(r1.err, active, 2, r1.att) {
				// CT-1: a capability-mismatch 400 is not
				// terminal for a tool-bearing request — fail
				// over once to a confirmed-capable target
				// instead of stopping the chain.
				if len(req.Tools) > 0 && !capabilityFailoverUsed && isCapabilityMismatchErr(r1.err) {
					if ni := e.capabilityFailoverIndex(active, 2); ni >= 0 {
						capabilityFailoverUsed = true
						idx = ni
						continue
					}
				}
				break
			}
			if r1Refusal {
				refusalMode = true
				if rc := e.refusalChainFor(req.Model, ordered, 2, tried); len(rc) == 0 {
					refusalExhausted = true
					break
				} else {
					active, idx = rc, 0
				}
				continue
			}
			idx = 2
			continue
		}

		res := e.callChat(ctx, att, attemptReq)
		attemptsUsed++
		tried[att.ProviderID] = true
		// Cumulative spend for the budget gate: a failed attempt
		// reports no usage ($0); a success ends the chain below,
		// so this only ever moves on a future billed-then-continue
		// path — the accumulator is the invariant, not today's $0.
		spentUSD += e.attemptCost(att, res.resp)
		// Context-relay (Phase D, D5): a "content_filter" finish
		// is a refusal, not a success — convert it here, before
		// the outcome is fed to the relay and before the success
		// branch below, so a refusal is never recorded as a
		// success. The conversion used to live inside the success
		// branch; hoisting it changes nothing else.
		if res.err == nil && isContentFilterFinish(res.resp) {
			res.err = contentRefusalError(att)
		}
		// Context-relay (Phase D, D5): feed this attempt's
		// outcome into the relay. On success the request ends
		// below, so the record only lands in LastRelayStatus —
		// the handoff is built lazily and never costs a
		// summariser call for a finished request. On failure the
		// record is what the next attempt's handoff is built
		// from.
		e.relayNote(relay, req, res)
		if res.err == nil {
			e.noteSuccess(trace, res, req)
			if refusalMode {
				e.noteRefusalServed()
			}
			e.recordDecision(trace, contract, treeOrdered, strategy, req.Model, res.att.Model, res.att.ProviderID)
			noteTreeServed(trace, tree, res.att.ProviderID, res.att.Model, treeRoot)
			e.setLastTrace(trace)
			return e.finishResponse(res), trace, nil
		}
		lastErr = res.err
		if isContentRefusalErr(res.err) {
			// Content refusal (CHECKLIST 161): never retry the
			// same target, never a breaker/cooldown/health
			// signal (see noteRefusal) — route to the next
			// target in the refusal chain instead. A refusal
			// on a replay-exempt request still ends here:
			// the request must never be replayed anywhere.
			e.noteRefusal(trace, res, req.CallerID)
			if exempt != "" {
				break
			}
			if refusalMode {
				idx++
				continue
			}
			refusalMode = true
			if rc := e.refusalChainFor(req.Model, ordered, idx+1, tried); len(rc) == 0 {
				refusalExhausted = true
				break
			} else {
				active, idx = rc, 0
			}
			continue
		}
		isRL, wait, abandoned := e.noteFailure(trace, res, &consecutive429, mode, req.CallerID, req.Model)
		if exempt != "" {
			// Replay-unsafe request (embeddings/images/rerank,
			// a Responses-ID continuation, a warm-up): exactly
			// one attempt — no fallback, no same-provider retry.
			// The attempt itself is still recorded above.
			break
		}
		// G11 Area 2: readiness-deadline sub-budget — when
		// the sub-budget is exhausted the combo ends with a
		// clean 504 instead of burning the whole request
		// budget on slow targets. See deadline_budget.go.
		if e.deadlineSubBudgetHit(res.err, &deadlineFailures) {
			lastErr = ErrDeadlineExceeded
			break
		}
		if isRL {
			// Wait budgets (M4.8, opt-in): when enabled, a
			// plain rate-limit 429 is waited out on the SAME
			// target — up to 90 s per wait, 5 waits, 300 s
			// total — instead of failing over immediately.
			// Never on quota/concurrency 429s (the helper
			// declines those); when the budget declines, the
			// normal failover below runs unchanged.
			if w, ok := e.waitBudgetWait(res.err, wait, waitsUsed, waitedTotal); ok && attemptsLeaveOne(attemptsUsed, maxAttempts, idx+1 < len(active)) {
				waitsUsed++
				waitedTotal += w
				if !e.sleep(ctx, w) {
					lastErr = ctx.Err()
					break
				}
				e.clearCooldown(ClientKey(att.ProviderID, att.KeyID))
				continue
			}
			if idx+1 < len(active) && attemptsUsed < maxAttempts {
				// Fail over immediately: waiting out a 429 is
				// slower than switching providers.
				idx++
				continue
			}
			if abandoned {
				// Retry-After >= 60s: this target is abandoned
				// for this request — fail honestly now instead
				// of sleeping out the wait and retrying it.
				// (When a further target existed the branch
				// above already moved on to it: the abandon is
				// per-target, never per-request.)
				break
			}
			if attemptsUsed < maxAttempts {
				// No further attempts: honour the wait, then
				// retry the same provider.
				if !e.sleep(ctx, wait) {
					lastErr = ctx.Err()
					break
				}
				e.clearCooldown(ClientKey(att.ProviderID, att.KeyID))
				continue
			}
			break
		}
		// CT-1: a capability-mismatch 400 on a tool-bearing
		// request is a property of the TARGET model, not of
		// the request — the chain does NOT stop on it. It
		// fails over ONCE, to the next CONFIRMED-capable
		// target in the chain (unknown models do not count:
		// the single failover never spends itself on another
		// guess). No confirmed-capable target, or the
		// failover already spent, and the 400 stays terminal
		// below. The breaker was already left neutral for
		// this 4xx by noteFailure above (isClientErrorFailure).
		if len(req.Tools) > 0 && !capabilityFailoverUsed && isCapabilityMismatchErr(res.err) {
			if ni := e.capabilityFailoverIndex(active, idx+1); ni >= 0 {
				capabilityFailoverUsed = true
				idx = ni
				continue
			}
		}
		if isTerminalErrFor(res.err, active, idx+1, res.att) {
			// Terminal failure (malformed request, forbidden,
			// unrepresentable input — entry 151/183): the
			// failure is a property of the request, so no
			// other target could serve it. The chain stops
			// here; the fallback target is never dialled. A
			// 401 is not terminal while a sibling key of the
			// same provider remains (see isTerminalErrFor).
			break
		}
		// Same-host transient retry (M4.5): a 502/503/504 is
		// retried on the SAME target up to 3 times with
		// decorrelated full-jitter backoff (base 200 ms, cap
		// 2 s) before the chain moves on. The retry re-enters
		// the loop top WITHOUT advancing idx, so the
		// breaker/cooldown/budget gates still apply — a
		// tripped breaker stops the retries (skipped_breaker)
		// instead of being ignored. Replay-exempt requests
		// never retry (exactly one attempt, ever); the hedged
		// branch above has its own handling and never reaches
		// here.
		// G11 Area 2: the transient retry is now bounded by the
		// failover-before-retry knobs (row 6) — per-target
		// FailoverMaxRetries and set-wide FailoverMaxSetRetries
		// — instead of the bare TransientMaxRetries constant.
		// An explicit FailoverBeforeRetry skips the retry when
		// a next target exists (fail over immediately instead).
		// See failover_policy.go.
		if isTransientStatus(res.err) && exempt == "" && e.failoverRetryAllowed(idx+1 < len(active)) && e.transientRetryAllowed(transientCount, setRetriesUsed) && attemptsLeaveOne(attemptsUsed, maxAttempts, idx+1 < len(active)) {
			delay := e.transientDelay(transientPrev)
			transientPrev = delay
			transientCount++
			setRetriesUsed++
			if !e.transientSleep(ctx, delay) {
				lastErr = ctx.Err()
				break
			}
			continue
		}
		// Identical-error early stop (M4.7d): 3 consecutive
		// identical model-shape 400s mean the request itself
		// is wrong for every target — stop the chain instead
		// of burning the remaining attempts on it. lastErr
		// already holds the real provider error.
		if identicalErr.note(res.err) {
			break
		}
		idx++
	}
	// M5.3: every target was skipped on a token limit — the
	// request error names the breach, not the generic
	// no-attempts error.
	if lastErr == nil && firstBreach != nil {
		lastErr = firstBreach
	}
	// M5.11: every target was skipped on a provider internal
	// budget — the same, with the token-limit breach taking
	// precedence when both were seen.
	if lastErr == nil && firstBudgetBreach != nil {
		lastErr = firstBudgetBreach
	}
	if lastErr == nil {
		lastErr = ErrNoAvailableAttempt
	}
	e.setLastTrace(trace)
	if refusalMode && isContentRefusalErr(lastErr) {
		// The request ended while routing content refusals.
		// When the refusal chain ran dry (no further target
		// to try) the answer is an honest refusal — count it
		// and wrap it in ErrContentRefused, which the server
		// answers with 403 / -32001. When the request stopped
		// mid-chain instead (attempt budget, deadline, replay
		// exemption) the chain was not exhausted, so the bare
		// refusal error is returned: the server still answers
		// it as a 403 / -32001, never a 502.
		if refusalExhausted || idx >= len(active) {
			e.noteRefusalExhausted()
			return nil, trace, fmt.Errorf("%w: %w", ErrContentRefused, e.mapDeadlineErr(lastErr, parentCtx))
		}
		return nil, trace, e.mapDeadlineErr(lastErr, parentCtx)
	}
	return nil, trace, e.mapDeadlineErr(lastErr, parentCtx)
}

// mapDeadlineErr rewrites a context-deadline error to
// ErrDeadlineExceeded when the expiry came from the engine's own
// per-combo budget (a budget is configured and the caller's context
// is still alive). A caller cancellation, or an expiry with no
// budget configured, passes through untouched.
func (e *Engine) mapDeadlineErr(err error, parentCtx context.Context) error {
	if err == nil {
		return nil
	}
	if e.cfg.DeadlineBudget > 0 && errors.Is(err, context.DeadlineExceeded) && parentCtx.Err() == nil {
		return ErrDeadlineExceeded
	}
	return err
}

func (e *Engine) finishResponse(res attemptResult) *provider.ChatResponse {
	resp := res.resp
	if resp == nil {
		resp = &provider.ChatResponse{}
	}
	if resp.Model == "" {
		resp.Model = res.att.Model
	}
	return resp
}

// stampHedgeMode records, on the trace, what the engine decided
// about hedging for this request (CHECKLIST 191). hedgeOff is
// true when the chain order is explicit (contract order, group
// order, or strategy-tree order — see the hedgeOff comment in
// ChatWithChainStrategy); exempt names a replay exemption
// ("" when there is none); attempts is the length of the chain
// being walked. The value is a policy decision, stamped before
// the loop: whether the hedge actually fires also depends on
// runtime conditions (the second target being available, the
// budget gate), and the attempts tell that part — a
// "hedged_cancelled" row means it fired and the first attempt
// lost. A single-attempt chain records nothing (""): there is
// nothing to race, and an absent value is more honest than a
// claim about a hedge that could never run.
func (e *Engine) stampHedgeMode(trace *routetrace.RouteTrace, hedgeOff bool, exempt string, attempts int, req provider.ChatRequest) {
	switch {
	case exempt != "":
		trace.Hedge = routetrace.HedgeModeDisabledExempt
	case hedgeOff:
		trace.Hedge = routetrace.HedgeModeDisabledExplicitOrder
	case e.cfg.HedgeDelay > 0 && requestBodyBytes(req) > hedgeMaxBodyBytes:
		// Hedge body-size guard (M4.8): the body is too large
		// to hedge, so the hedge is not armed for this request.
		trace.Hedge = routetrace.HedgeModeDisabledLargeBody
	case e.cfg.HedgeDelay > 0 && attempts > 1:
		trace.Hedge = routetrace.HedgeModeArmed
		trace.HedgeDelayMs = e.cfg.HedgeDelay.Milliseconds()
	}
}

// hedgedChat races the first two attempts. The first attempt starts
// immediately; if it has not returned within HedgeDelay, the second
// starts in parallel and whichever returns first wins; the loser is
// cancelled. If the first finisher errored, the already-running other
// attempt is still given its chance to succeed before giving up, since
// it was already launched. It returns the first result, the second
// result (nil when the hedge never fired), and whether the second
// attempt was launched.
func (e *Engine) hedgedChat(ctx context.Context, first, second Attempt, req provider.ChatRequest) (attemptResult, *attemptResult, bool) {
	start := time.Now()
	resCh := make(chan attemptResult, 2)
	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	go func() { resCh <- e.callChat(ctxA, first, req) }()

	timer := time.NewTimer(e.cfg.HedgeDelay)
	defer timer.Stop()
	select {
	case r := <-resCh:
		return r, nil, false
	case <-timer.C:
	case <-ctx.Done():
		return attemptResult{att: first, err: ctx.Err(), latencyMs: time.Since(start).Milliseconds()}, nil, false
	}

	ctxB, cancelB := context.WithCancel(ctx)
	defer cancelB()
	go func() { resCh <- e.callChat(ctxB, second, req) }()

	r1 := <-resCh
	if r1.err == nil {
		// Winner finished first; the loser is cancelled by the
		// deferred cancels on return.
		loser := second
		if r1.att == second {
			loser = first
		}
		return r1, &attemptResult{att: loser, err: context.Canceled, latencyMs: time.Since(start).Milliseconds(), called: true}, true
	}
	// First finisher errored; wait for the other one.
	select {
	case r2 := <-resCh:
		return r1, &r2, true
	case <-ctx.Done():
		other := second
		if r1.att == second {
			other = first
		}
		return r1, &attemptResult{att: other, err: ctx.Err(), latencyMs: time.Since(start).Milliseconds()}, true
	}
}

// ---------------------------------------------------------------------------
// Streaming chat.
//
// Failover rule (research-log entries 97 + 111; CHECKLIST 133): the
// preread buffer holds the leading events of a stream — up to 8
// events / 256 KiB / 5 s — until the first real content arrives.
// A failure before any content is a plain failover with the
// original request unchanged, and a stream that ends empty fails
// over too: nothing was delivered, so the client never sees a
// partial body. Once the first content byte is sent, the stream
// NEVER switches candidates: a later failure ends the stream with
// an error, and the server turns it into an honest SSE error
// frame. There is no mid-stream resume — a resume IS a switch
// after commit, and the preread rule forbids it. (An earlier
// version of this file resumed with the delivered prefix as an
// assistant prefill; that behaviour was removed by the preread
// slice, deliberately. The explicit-resume entry points
// ChatStreamFrom / ChatStreamFromChunks remain for callers that
// ask for a resume outright, e.g. a client reconnect — they run a
// fresh chain with skipChars trimming, and are NOT invoked
// automatically after an in-request failure.)
//
// Exemptions: requests that must never be replayed (embeddings /
// images / rerank / Responses surfaces, Responses-ID continuations,
// warm-ups — see ReplayExemption in preread.go) skip the buffer
// and the hedge and get exactly one attempt.
// ---------------------------------------------------------------------------

// ChatStreamWithChain routes a streaming request through the fallback
// chain and returns a channel of StreamEvents. The channel yields Deltas,
// ends with a Done event on success, or ends with an Err event if every
// attempt failed; it is then closed. The returned error is non-nil
// only for a bad request (empty chain) or an already-done ctx —
// per-attempt failures are delivered as events, so callers must drain
// the channel. Use Engine.LastTrace after the channel closes to see
// every attempt. Callers must either drain the channel or cancel ctx,
// otherwise a blocked send can stall the engine goroutine.
//
// The strategy for this request is the configured global strategy
// (or a ChainStrategy override for req.Model); see
// ChatStreamWithChainStrategy for a per-request override.
//
// The server-facing ChatStream (no chain argument) delegates here after
// resolving the chain for req.Model; see chainFor.
func (e *Engine) ChatStreamWithChain(ctx context.Context, chain []Attempt, req provider.ChatRequest) (<-chan StreamEvent, error) {
	return e.ChatStreamWithChainStrategy(ctx, chain, req, "")
}

// ChatStreamWithChainStrategy is ChatStreamWithChain with an explicit
// per-request strategy override (the "auto:<axis>" alias, already
// parsed by the caller): a non-empty strategy wins over the chain
// override and the global strategy. "" means "no per-request
// override". The per-combo deadline budget bounds the whole stream
// the same way it bounds ChatWithChainStrategy: on expiry the
// channel ends with an Err event carrying ErrDeadlineExceeded.
// The stream-lifetime cap (Config.StreamMaxLifetime, default 30
// minutes — CHECKLIST 255) bounds it the same way: on expiry the
// upstream call is terminated and the channel ends with an Err
// event carrying ErrStreamLifetimeExceeded.
func (e *Engine) ChatStreamWithChainStrategy(ctx context.Context, chain []Attempt, req provider.ChatRequest, reqStrategy string) (<-chan StreamEvent, error) {
	return e.chatStreamWithChainStrategy(ctx, chain, req, reqStrategy, false)
}

// chatStreamWithChainStrategy is ChatStreamWithChainStrategy with
// the dispatched-chain flag (see chatWithChainStrategy): a
// multi-model auto chain dispatched by dispatchAutoVariant keeps
// its order — no quota-hint sort, strategy hook, or hedge may
// re-rank it. The public method keeps the old signature so
// existing callers and tests are untouched.
func (e *Engine) chatStreamWithChainStrategy(ctx context.Context, chain []Attempt, req provider.ChatRequest, reqStrategy string, dispatchExplicitOrder bool) (<-chan StreamEvent, error) {
	// Lossless-first compression (CHECKLIST 183): before the
	// contract is applied, so the chain machinery sees the prompt
	// as sent. runStream re-checks the stamp after restoring
	// reasoning states; a stamp for this model is kept as-is.
	req = e.maybeCompress(req)
	// User-named routing group (CHECKLIST 173): resolved before
	// the empty-chain check, so a group-addressed request whose
	// default chain is empty still gets the honest group error
	// instead of "empty fallback chain". A rejected group
	// publishes its trace (contract + Error, no attempts) and
	// returns before any provider is dialled, so the server can
	// still answer a plain JSON 400 (no SSE headers sent).
	req, chain, groupName, gerr := e.resolveGroupChain(chain, req)
	if gerr != nil {
		tr := groupFailureTrace(groupName, gerr)
		tr.Endpoint = traceEndpointFor(req)
		tr.Compression = traceCompression(req.Compressed)
		e.publishTrace(ctx, tr)
		return nil, gerr
	}
	// Forced-reasoning policy (CHECKLIST 311-312): see
	// ChatWithChainStrategy — a group replaces the chain the
	// entry points stamped, so the request-level decision is
	// re-stamped here.
	chain = applyRequestForcedEffort(chain, req)
	if len(chain) == 0 {
		// Empty chain on entry (CHECKLIST 251-252): the
		// adjacent hole the 231-232 slice flagged and
		// deliberately left open. No provider can serve this
		// model, so the request is refused before any attempt
		// — but it must still leave a trace, like the group
		// rejection just above and the ctx-cancel return just
		// below: without this LastTrace stays nil on a fresh
		// engine, or stale — the previous request's trace —
		// on a reused one, and the dashboard trace feed
		// misattributes the request. No attempt was made, so
		// the trace honestly carries none; no contract was
		// applied yet, so none is named. Same error as
		// before, same callers.
		e.publishTrace(ctx, &routetrace.RouteTrace{Endpoint: traceEndpointFor(req), Compression: traceCompression(req.Compressed)})
		return nil, errors.New("router: empty fallback chain")
	}
	if err := ctx.Err(); err != nil {
		// A cancelled context on entry (CHECKLIST 231-232): the
		// request was cut before the engine started — an async
		// guardrail BLOCK whose verdict landed before the first
		// attempt, or a client that went away. Publish the
		// (attempt-less) trace anyway, like the group and
		// contract rejections around it: without this LastTrace
		// stays nil on a fresh engine — or stale, the previous
		// request's trace, on a reused one — and the dashboard
		// trace feed misattributes the request. No attempt was
		// made, so the trace honestly carries none; the refusal
		// itself is recorded where it belongs, in the guardrail
		// store (blocked_before_delivery). No contract was
		// applied yet, so none is named.
		e.publishTrace(ctx, &routetrace.RouteTrace{Endpoint: traceEndpointFor(req), Compression: traceCompression(req.Compressed)})
		return nil, err
	}
	// Per-request routing contract (CHECKLIST 157): same
	// application as the non-streaming path — a rejected contract
	// publishes its trace (contract + Error, no attempts) and
	// returns before any provider is dialled, so the server can
	// still answer a plain JSON 400/402 (no SSE headers sent).
	chain, contract, sortStrat, err := e.applyContract(chain, req)
	// The trace contract names the group even when it arrived
	// as a "group:<name>/<model>" model alias — and when the
	// alias is the ONLY contract control, applyContract returns
	// a nil contract, so one is created here (see
	// ChatWithChainStrategy).
	if groupName != "" {
		if contract == nil {
			contract = &routetrace.RoutingContract{Group: groupName}
		} else {
			contract.Group = groupName
		}
	}
	if err != nil {
		e.publishTrace(ctx, &routetrace.RouteTrace{Contract: contract, Endpoint: traceEndpointFor(req), Compression: traceCompression(req.Compressed)})
		return nil, err
	}
	// A group-resolved chain is an explicit operator order (see
	// ChatWithChainStrategy): the group's member order is final
	// — no strategy hook, strategy tree, or quota-hint sort may
	// re-rank it. A dispatched multi-model auto chain is
	// explicit the same way (see chatWithChainStrategy).
	// Passed into runStream, which does the ordering.
	explicitOrder := dispatchExplicitOrder || groupName != "" || (contract != nil && len(contract.Order) > 0)
	if sortStrat != "" {
		reqStrategy = sortStrat
	}
	// Responses-continuation pin (CHECKLIST 169, reordered by
	// 237-238): see the non-streaming path — the original
	// provider goes first when it still serves this model; a
	// failure fails over to the rest of the chain.
	chain = pinChain(chain, req.PinProviderID)
	strategy := e.effectiveStrategy(reqStrategy, req.Model)
	// Nestable strategy tree (CHECKLIST 177): same rule as the
	// non-streaming path — when a tree orders this request the
	// recorded strategy is the tree root's (or the per-request
	// override that replaced it), never the flat strategy the
	// tree replaced. runStream receives the raw reqStrategy
	// separately, so the override replaces the root only.
	if _, treeRoot, treeOrdered := e.treeRootStrategy(req.Model, reqStrategy, explicitOrder); treeOrdered {
		strategy = treeRoot
	}
	if contract != nil {
		contract.Strategy = strategy
	}
	// Endpoint projection (Phase D, D4): same fail-closed rule
	// as the non-streaming path, checked before the stream
	// goroutine starts — see projectStrategyForEndpoint
	// (cascade.go).
	if projErr := checkEndpointStrategy(strategy, req.Endpoint); projErr != nil {
		e.publishTrace(ctx, &routetrace.RouteTrace{Contract: contract, Endpoint: traceEndpointFor(req), Compression: traceCompression(req.Compressed)})
		return nil, projErr
	}
	e.recordRouting(strategy, req.Model)
	bctx, cancel := e.withDeadline(ctx)
	out := make(chan StreamEvent, 16)
	go func() {
		defer cancel()
		e.runStream(ctx, bctx, chain, req, out, strategy, contract, reqStrategy, explicitOrder)
	}()
	return out, nil
}

// bridgeStream adapts a provider.Client's callback-based ChatStream to
// the channel of StreamEvents the engine's streaming core consumes.
// When the client implements provider.ChunkStreamer (both built-in
// clients do), ChatStreamChunks is used so tool-call deltas and the
// finish reason survive the bridge; otherwise it falls back to the
// text-only ChatStream. The channel yields events in order, then a
// Done event on clean completion or an Err event on failure, and is
// then closed. If ctx is cancelled (e.g. a hedged loser is abandoned)
// the bridge stops forwarding and the underlying client call unwinds
// via ctx.
func (e *Engine) bridgeStream(ctx context.Context, att Attempt, client provider.Client, req provider.ChatRequest) <-chan StreamEvent {
	ch := make(chan StreamEvent, 16)
	go func() {
		defer close(ch)
		send := func(ev StreamEvent) error {
			// Check first, then select: the channel is
			// buffered, so a plain select could still deliver
			// a stale delta after the abort whenever the send
			// case happened to win the race — the router test
			// flaked on exactly that ("t1 t2 t3 " after a
			// cancel that followed t2). A delta already
			// enqueued before the abort may still be drained;
			// nothing new is enqueued after it (CHECKLIST 190).
			if err := ctx.Err(); err != nil {
				return err
			}
			select {
			case ch <- ev:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// Panic isolation (CHECKLIST 165): a panic in the
		// provider's stream client — including a wire chunk
		// converter turning a malformed upstream event into a
		// panic — becomes a terminal Err event, never a dead
		// gateway. runStream treats it like any other stream
		// failure: fail over before commit, end the stream with
		// an error after commit. The panic is counted
		// (notePanic) and its value rides the PanicError to
		// the server, which logs it redacted.
		defer func() {
			if v := recover(); v != nil {
				e.notePanic("provider_stream")
				_ = send(StreamEvent{Err: NewPanicError("provider_stream", v)})
			}
		}()
		if cs, ok := client.(provider.ChunkStreamer); ok {
			resp, err := cs.ChatStreamChunks(ctx, req, func(chunk provider.StreamChunk) error {
				return send(StreamEvent{
					Delta:        chunk.Content,
					ToolCalls:    chunk.ToolCalls,
					FinishReason: chunk.FinishReason,
				})
			})
			if err != nil {
				e.learnRateLimitError(att, err)
				_ = send(StreamEvent{Err: err})
				return
			}
			// Reasoning capture (CHECKLIST 163): the
			// reassembled response may carry reasoning state
			// — store it under the transcript key so the next
			// turn can restore it. This is the only
			// stream-capture point: every stream path (live,
			// drained, hedged winner) flows through this
			// bridge, and only a stream that reached Done
			// captures — a failed or empty stream served
			// nothing. The Done event also carries the
			// response, so the drain can return its reasoning.
			e.learnRateLimit(att, resp.RateLimit)
			e.captureReasoning(req, resp)
			_ = send(StreamEvent{Done: true, FinishReason: resp.FinishReason, Resp: resp})
			return
		}
		resp, err := client.ChatStream(ctx, req, func(delta string) error {
			if delta == "" {
				return nil
			}
			return send(StreamEvent{Delta: delta})
		})
		if err != nil {
			e.learnRateLimitError(att, err)
			_ = send(StreamEvent{Err: err})
			return
		}
		e.learnRateLimit(att, resp.RateLimit)
		e.captureReasoning(req, resp)
		// M6.3: carry the finish reason on the Done event itself
		// (mirrors replayComboResult, combo.go:436) so the drain
		// sees "length" from a text-only client, whose ChatStream
		// emits no per-chunk reason.
		_ = send(StreamEvent{Done: true, FinishReason: resp.FinishReason, Resp: resp})
	}()
	return ch
}

func (e *Engine) sendTerminal(out chan<- StreamEvent, err error) {
	if err == nil {
		return
	}
	select {
	case out <- StreamEvent{Err: err}:
	default: // receiver gone; channel close below still signals the end
	}
}

type streamOutcome int

const (
	streamDone streamOutcome = iota
	streamFailed
	streamCtxDone
	// streamEmpty is a stream that ended ([DONE] or close) without
	// ever emitting real content, before commit: the preread buffer
	// reports it so the caller can fail over without the client
	// ever seeing a partial body.
	streamEmpty
)

// forwardStream forwards events from ch to out, appending text deltas
// to partial. first, when non-nil, is an already-consumed first event
// (hedge path) that is emitted before reading ch. It returns when the
// stream ends cleanly (Done), fails (Err event or unexpected close),
// or ctx ends. Tool-call deltas and the finish reason ride along on
// the events untouched; the caller that drains the channel
// (chatStreamDrainChunks) reassembles them. forwardStream is the
// live phase of a stream: it is used directly for exempt requests
// (no preread) and by forwardStreamPreread after the buffer commits.
// onCommit, when non-nil, fires exactly once, immediately before the
// first event is sent on out — the commit point (CHECKLIST 199):
// the stream is now committed to this attempt's provider, so the
// routing decision is final and the server may report it on the
// response. It never fires when the stream fails before its first
// event (an Err first event, or ctx ending first): nothing was
// delivered, so there is no serving provider to name.
func (e *Engine) forwardStream(ctx context.Context, out chan<- StreamEvent, ch <-chan StreamEvent, first *StreamEvent, partial *strings.Builder, onCommit func()) (streamOutcome, error, *provider.ChatResponse) {
	committed := false
	fireCommit := func() {
		if !committed {
			committed = true
			if onCommit != nil {
				onCommit()
			}
		}
	}
	// Throughput watchdog (M4.6): every live byte passes through
	// this loop, so a stalled stream is caught here — aborted
	// before commit (failover) or after commit (terminal error).
	wd := newStreamWatchdog(time.Now())
	timer := time.NewTimer(wd.nextCheck())
	defer timer.Stop()
	if first != nil {
		if first.Err != nil {
			return streamFailed, first.Err, nil
		}
		if first.Delta != "" {
			partial.WriteString(first.Delta)
		}
		wd.noteBytes(watchdogBytes(*first))
		fireCommit()
		select {
		case out <- *first:
		case <-ctx.Done():
			return streamCtxDone, ctx.Err(), nil
		}
		if first.Done {
			return streamDone, nil, first.Resp
		}
	}
	for {
		select {
		case <-ctx.Done():
			return streamCtxDone, ctx.Err(), nil
		case <-timer.C:
			if err := wd.check(time.Now()); err != nil {
				return streamFailed, err, nil
			}
			timer.Reset(wd.nextCheck())
		case ev, ok := <-ch:
			if !ok {
				// The bridge closed the channel. When ctx
				// ended first (a client abort, or the
				// engine's own deadline), the close is the
				// cancellation unwinding — report it as
				// such, not as a spurious "upstream closed
				// stream" failure: the trace reason, the
				// dashboard label and the terminal event
				// must all say cancelled (CHECKLIST 153).
				if err := ctx.Err(); err != nil {
					return streamCtxDone, err, nil
				}
				return streamFailed, errors.New("router: upstream closed stream without Done"), nil
			}
			if ev.Err != nil {
				return streamFailed, ev.Err, nil
			}
			if ev.Delta != "" {
				partial.WriteString(ev.Delta)
			}
			wd.noteBytes(watchdogBytes(ev))
			fireCommit()
			select {
			case out <- ev:
			case <-ctx.Done():
				return streamCtxDone, ctx.Err(), nil
			}
			if ev.Done {
				return streamDone, nil, ev.Resp
			}
		}
	}
}

// noteStreamStartFailure records an attempt whose ChatStream call
// itself failed. It returns the 429 wait (0 when not rate-limited) so
// the caller can honour it before retrying the same provider. mode
// is the resolved cooldown mode for this request (see noteFailure):
// in soft-demote mode a retryable start failure demotes the target
// instead of tripping its breaker.
func (e *Engine) noteStreamStartFailure(trace *routetrace.RouteTrace, res attemptResult, start time.Time, mode, keyID, chainKey string) time.Duration {
	lat := time.Since(start).Milliseconds()
	if !e.hasClient(res.att.ProviderID) {
		addTraceAttempt(trace, res.att, lat, "error", res.err)
		return 0
	}
	if e.ledger != nil {
		// Stream start failure: no usage was reported, so record
		// Usage{} (unknown), never a guessed split.
		e.recordAttempt(keyID, res.att, false, lat, provider.Usage{})
	}
	if errors.Is(res.err, context.Canceled) || errors.Is(res.err, context.DeadlineExceeded) {
		addTraceAttempt(trace, res.att, lat, "error", res.err)
		return 0
	}
	// Model lockout (M4.3): same record as noteFailure — a dead
	// model on a stream start locks the triple too.
	e.lockoutRecord(res.att, res.err)
	if ra, present, ok := rateLimitHint(res.err); ok {
		wait := e.rateLimitWait(ra, present, 1)
		if NormalizeCooldownMode(mode) == CooldownSoftDemote {
			e.startSoftCooldown(ClientKey(res.att.ProviderID, res.att.KeyID), ra, present)
		} else {
			e.setCooldown(ClientKey(res.att.ProviderID, res.att.KeyID), retryAfterHonouredWait(ra, present, e.cfg.RetryAfterCap))
		}
		e.breakerNeutral(res.att.ProviderID)
		addTraceAttemptRetryReasoning(trace, res.att, lat, "rate_limited", res.err, retryInfoFor(ra, present, e.cfg.RetryAfterCap), res.reasoning)
		return wait
	}
	if isReasoningUnrepresentableErr(res.err) {
		// The target wire cannot represent the restored
		// reasoning state — breaker-neutral, no cooldown
		// (see noteFailure for the same carve-out).
		e.breakerNeutral(res.att.ProviderID)
		addTraceAttemptReasoning(trace, res.att, lat, "error", res.err, res.reasoning)
		return 0
	}
	if NormalizeCooldownMode(mode) == CooldownSoftDemote && isRetryableFailure(res.err) {
		ra, present := retryAfterOf(res.err)
		e.startSoftCooldown(ClientKey(res.att.ProviderID, res.att.KeyID), ra, present)
		e.breakerNeutral(res.att.ProviderID)
		// Hard failure in soft-demote mode: clear the LKGP pin
		// here — this branch, not the breakerFailure
		// fall-through below, is where it lands (see
		// noteFailure; invalidateLKGP skips non-hard members).
		e.invalidateLKGP(chainKey, res.att.ProviderID, res.err)
		addTraceAttemptReasoning(trace, res.att, lat, "error", res.err, res.reasoning)
		return 0
	}
	if isModelNotFoundErr(res.err) {
		// The provider answered fine — only the model is dead
		// at this target — so the failure must not count
		// against the provider's circuit breaker (see
		// noteFailure for the same carve-out).
		e.breakerNeutral(res.att.ProviderID)
		addTraceAttemptReasoning(trace, res.att, lat, "error", res.err, res.reasoning)
		return 0
	}
	if isClientErrorFailure(res.err) {
		// A 4xx is a property of the request or the
		// operator's key, never a provider-health signal:
		// it must never trip the circuit breaker (see
		// noteFailure). Counted as breaker-ignored 4xx. A
		// dead pool key (401) is cooled per-key, as in
		// noteFailure.
		e.coolDeadKey(res.att, mode)
		e.breakerNeutral(res.att.ProviderID)
		e.countBreakerIgnored4xx(res.att.ProviderID)
		addTraceAttemptReasoning(trace, res.att, lat, "error", res.err, res.reasoning)
		return 0
	}
	// Hard failure (5xx / timeout / transport): clear the
	// LKGP pin for the chain (RT-2) — see noteFailure.
	e.breakerFailure(res.att.ProviderID, provider.ClassifyError(res.err, provider.EndpointChatCompletions).Reason)
	e.invalidateLKGP(chainKey, res.att.ProviderID, res.err)
	addTraceAttemptReasoning(trace, res.att, lat, "error", res.err, res.reasoning)
	return 0
}

// runStream is the ChatStreamWithChain worker goroutine. parentCtx
// is the caller's context (used only to tell a caller cancellation
// apart from the engine's own deadline-budget expiry); ctx is the
// budget-wrapped context the stream actually runs under.
func (e *Engine) runStream(parentCtx, ctx context.Context, chain []Attempt, req provider.ChatRequest, out chan<- StreamEvent, strategy string, contract *routetrace.RoutingContract, rootOverride string, explicitOrder bool) {
	defer close(out)
	trace := &routetrace.RouteTrace{Contract: contract, Endpoint: traceEndpointFor(req)}
	// Reasoning continuity (CHECKLIST 163): restore captured
	// reasoning states onto the assistant messages they belong
	// to, for this request only — same as the non-streaming path.
	req = e.injectReasoning(req)
	// Lossless-first compression (CHECKLIST 183): a stamp for
	// this model (set by ChatStreamWithChainStrategy above, or
	// by the server handler) is kept; a direct runStream caller
	// compresses here, before any attempt.
	req = e.maybeCompress(req)
	trace.Compression = traceCompression(req.Compressed)
	defer e.publishTrace(parentCtx, trace)
	// Panic isolation (CHECKLIST 165): a panic in the engine's own
	// stream machinery — a scoring/order hook in orderAttempts,
	// the preread buffer, the forward loop — ends this stream with
	// a terminal Err event, never a dead gateway. Per-attempt
	// provider panics are already caught closer in (bridgeStream);
	// this is the backstop for the machinery around them.
	//
	// current/currentStart/currentTraced describe the attempt in
	// flight when the panic hit (nil before the first attempt):
	// when the panic struck before that attempt's bookkeeping ran
	// (no trace entry was added for it yet — len(trace.Attempts)
	// still equals currentTraced), the backstop records it as a
	// failed attempt (trace + ledger, unknown usage) so the panic
	// leaves an honest row, not a hole. When bookkeeping already
	// ran, the row is already there and the backstop adds nothing
	// — never a duplicate. streamSlotHeld is the limiter slot the
	// iteration holds, released here so a panic cannot leak it.
	var current *Attempt
	var currentStart time.Time
	var currentTraced int
	var streamSlotHeld string
	defer func() {
		if v := recover(); v != nil {
			err := NewPanicError("engine_stream", v)
			e.notePanic("engine_stream")
			if streamSlotHeld != "" {
				e.limiter.Release(streamSlotHeld)
			}
			if current != nil && len(trace.Attempts) == currentTraced {
				lat := time.Since(currentStart).Milliseconds()
				addTraceAttempt(trace, *current, lat, "error", err)
				if e.ledger != nil {
					e.recordAttempt(req.CallerID, *current, false, lat, provider.Usage{})
				}
			}
			e.sendTerminal(out, err)
		}
	}()
	mode := e.cooldownModeFor(req.Model)
	// Stream-lifetime cap (CHECKLIST 255): the cap runs from
	// here — the stream's start — and bounds the whole request
	// stream across failover attempts, not one attempt. The
	// lifetime context replaces ctx for every stream operation
	// below (hedge, bridge, forwarders, 429 sleep), so the cap
	// bounds the preread buffer and the Bedrock incremental
	// stream exactly like the live relay: they all select on
	// this context's Done. The deferred cancel stops the timer
	// when the stream ends. parentCtx is untouched: it stays
	// the caller's context for mapDeadlineErr.
	ctx, lifeCancel := e.withStreamLifetime(ctx)
	defer lifeCancel()
	// Replay exemption (preread.go): a stateful request is never
	// replayed — no preread buffer, no hedge, exactly one attempt.
	// The exemption is decided once per request, from req.
	exempt := ReplayExemption(req)

	// An explicit contract order is final (CHECKLIST 157): no
	// scoring may re-rank it — same rule as the non-streaming path.
	// A group-resolved chain is explicit the same way (CHECKLIST
	// 173): the group's member order is final. A strategy tree
	// orders the chain instead of the flat hook when one is
	// configured for this model (CHECKLIST 177), except under an
	// explicit order. rootOverride is
	// the raw per-request override (contract sort / auto:<axis>
	// alias, "" when there is none); it replaces the tree's
	// ROOT strategy only. A tree-ordered chain is explicit too,
	// so it gets no hedge (CHECKLIST 191 — see the hedgeOff
	// comment in ChatWithChainStrategy).
	tree := e.strategyTreeFor(req.Model)
	treeOrdered := tree != nil && !explicitOrder
	hedgeOff := explicitOrder || treeOrdered
	// Per-request hook context (M6.5-wiring): same as the
	// non-streaming path — the prompt count, tools and
	// temperature the strategy hook needs, built from the
	// request as it will actually be sent.
	hookCtx := e.strategyHookContextFor(req, explicitOrder)
	var ordered, zeroSkipped []Attempt
	if treeOrdered {
		var err error
		ordered, zeroSkipped, err = e.orderAttemptsByTreeWithContext(chain, tree, rootOverride, mode, hookCtx)
		if err != nil {
			e.sendTerminal(out, err)
			return
		}
	} else {
		ordered, zeroSkipped = e.orderAttemptsWithContext(chain, strategy, mode, explicitOrder, hookCtx)
	}
	// Pin first (CHECKLIST 243): same re-application as the
	// non-streaming path — the ordering above can demote the
	// pin, so it is restored to the front here.
	ordered = pinFirst(ordered, req.PinProviderID)
	for _, att := range zeroSkipped {
		addTraceAttempt(trace, att, 0, "skipped_zero_quota", nil)
	}
	// P1.5 capability filter (streaming path): identical semantics to
	// the non-streaming filter — required capabilities must be
	// VERIFIED, unknown never qualifies, and the filter runs before
	// any dial so a dropped attempt costs no provider call.
	ordered, capSkipped, capReasons := e.filterByRequiredCapabilities(ordered)
	for i, att := range capSkipped {
		reason := "skipped_capability_unverified"
		if i < len(capReasons) {
			reason = capReasons[i]
		}
		addTraceAttempt(trace, att, 0, reason, nil)
	}
	if len(ordered) == 0 {
		e.sendTerminal(out, ErrNoAvailableAttempt)
		return
	}
	// Context-window pre-call fit check (CHECKLIST 159) — same
	// gate as the non-streaming path (see ChatWithChainStrategy):
	// too-small attempts never reach the dial, an oversize prompt
	// jumps to the dedicated long-context fallback chain, and a
	// prompt that fits nothing fails here with ErrContextTooLarge
	// before any provider is called (sent as the terminal stream
	// error; the server turns it into a 400 while nothing was
	// sent yet).
	var err error
	// AUD-PERF-004: attach the per-request token-count memo here,
	// after runStream's own reasoning restore + compression, so the
	// stream's context-fit gate and per-hop budget gate reuse one
	// CountPromptWire instead of recounting per attempt.
	if req.TokenCountMemo() == nil {
		req.SetTokenCountMemo(tokencount.NewRequestMemo())
	}
	ordered, err = e.applyContextFit(trace, ordered, req)
	if err != nil {
		e.sendTerminal(out, err)
		return
	}

	// Context-relay (Phase D, D5): one relay state owns this
	// request's stream attempts — nil for every other strategy.
	// Created here, after ordering and the context-fit gate, so a
	// request that never reaches the loop never owns one; the
	// combo dispatch below takes combo strategies first. See
	// relay.go.
	relay := e.newRelayFor(strategy, req.CallerID)
	// Cross-request relay (B2-01): a handoff wanted but never
	// built in this request is stored for the next request under
	// the same gateway key when this function returns, on every
	// exit path. The store holds the want, never a built
	// handoff — see storeRelayWantIfPending in relay.go.
	defer e.storeRelayWantIfPending(req.CallerID, relay)

	// Combo strategies (Phase D, D3): run non-streaming over the
	// ordered chain and replay the result verbatim as stream
	// events — see runComboStrategy / replayComboResult (combo.go).
	// The trace is published by the deferred publishTrace. The
	// strategy here is already the tree-root strategy (resolved
	// by the caller), so it doubles as the tree root for the
	// decision record. Replay-exempt requests never fan out.
	if isComboStrategy(strategy) && exempt == "" {
		comboResp, comboErr := e.runComboStrategy(ctx, ordered, req, trace, strategy, contract, tree, strategy, treeOrdered, mode)
		if comboErr != nil {
			e.sendTerminal(out, comboErr)
			return
		}
		e.replayComboResult(ctx, out, comboResp)
		return
	}

	maxAttempts := e.cfg.MaxAttempts
	attemptsUsed := 0
	var lastErr error
	// firstBreach is the first per-key token-limit breach seen
	// while walking the chain (M5.3): when every target is
	// breached the stream ends with no attempt and no lastErr,
	// so the breach — not the generic no-attempts error — is
	// the terminal error.
	var firstBreach *TokenLimitBreach
	// firstBudgetBreach is the same for provider-scoped internal
	// budgets (M5.11).
	var firstBudgetBreach *InternalBudgetBreach
	var partial strings.Builder
	// Same-host transient retry state (M4.5): same rule as the
	// non-streaming loop — the count and the jitter ladder belong
	// to one target.
	var streamTransientAtt Attempt
	streamTransientCount := 0
	var streamTransientPrev time.Duration
	// streamSetRetriesUsed is the set-wide same-target retry
	// count (mirror of the non-streaming loop's
	// setRetriesUsed): request-wide, NEVER reset when the
	// target changes — the FailoverMaxSetRetries cap spans
	// the whole chain for this request.
	streamSetRetriesUsed := 0
	// Identical-error early stop state (M4.7d): same rule as the
	// non-streaming loop.
	streamIdenticalErr := &identicalErrorTracker{}
	// Preferred-account response-start failover state (M4.8):
	// whether the one rotation already happened, and whether
	// the deadline fired for the attempt in flight (set by the
	// timer callback — an atomic, because it fires on another
	// goroutine).
	preferredRotated := false
	var preferredFired atomic.Bool
	// Stream-recovery early-retry state (M4.8): the count of
	// transparent re-opens of one target's stream while the
	// holdback is still uncommitted — a new target restarts it.
	var earlyRetryAtt Attempt
	earlyRetryCount := 0
	// spentUSD is this request's cumulative priced spend (see
	// requestbudget.go). Streaming successes record a
	// local-estimate usage (cost $0 — the split is unknown), so
	// it stays $0 here; the gate is the next attempt's prompt
	// estimate against the remaining budget.
	spentUSD := 0.0
	// Refusal-chain state (CHECKLIST 161) — as in
	// ChatWithChainStrategy: active is the chain being walked
	// (failure chain, then refusal chain), tried is every
	// provider already dialled in this request.
	active := ordered
	refusalMode := false
	refusalExhausted := false
	tried := make(map[string]bool)
	// pinEscapeCounted is the once-per-request guard for the
	// pin-escape counter (see the loop top below).
	pinEscapeCounted := false
	// regionCounted is the once-per-failure guard for the
	// region-aware failover counter (CHECKLIST 257) — same
	// rule as the non-streaming loop.
	regionCounted := -1
	// Hedge decision for this request (CHECKLIST 191) — same
	// stamp as the non-streaming path.
	e.stampHedgeMode(trace, hedgeOff, exempt, len(active), req)
	// idx is declared outside the loop so the tail below can tell
	// whether the refusal chain ran dry (idx past the end) or the
	// request stopped mid-chain.
	idx := 0

attemptLoop:
	for idx < len(active) && attemptsUsed < maxAttempts {
		if err := ctx.Err(); err != nil {
			// A cap that fired between attempts still ends
			// the request with the typed lifetime error, never
			// a bare context.Canceled (see lifetimeErr).
			lastErr = lifetimeErr(ctx)
			break attemptLoop
		}
		att := active[idx]
		// G11 Area 2: cross-provider attempt cap (default 3
		// distinct providers) and routing.only narrowing (a
		// prefixed provider/model is never rerouted) — the same
		// rule as the non-streaming loop (see failover_policy.go).
		// Labeled break: this loop is attemptLoop.
		if !providerAttemptAllowed(resolvedOnly(contract), tried, att.ProviderID, e.maxProvidersPerRequest()) {
			break attemptLoop
		}
		// Same-host transient retry state (M4.5): a new target
		// restarts the count and the jitter ladder.
		if att != streamTransientAtt {
			streamTransientAtt = att
			streamTransientCount = 0
			streamTransientPrev = 0
		}
		// Stream-recovery early-retry state (M4.8): a new
		// target restarts the re-open count too.
		if att != earlyRetryAtt {
			earlyRetryAtt = att
			earlyRetryCount = 0
		}
		// Pin escape (CHECKLIST 243): same rule as the
		// non-streaming loop — the stream loop has moved past
		// the pinned provider after trying it, so the pin was
		// escaped on a retryable failure; counted once per
		// request (see notePinEscape).
		if req.PinProviderID != "" && !pinEscapeCounted && tried[req.PinProviderID] && att.ProviderID != req.PinProviderID {
			pinEscapeCounted = true
			e.notePinEscape(req.PinProviderID, att.ProviderID)
		}
		// Hard mode skips a cooling provider outright; soft mode
		// never skips (see ChatWithChainStrategy). G11 Area 2:
		// the staggered gate (onCooldownStaggered).
		if NormalizeCooldownMode(mode) == CooldownHard && e.onCooldownStaggered(ClientKey(att.ProviderID, att.KeyID)) {
			addTraceAttempt(trace, att, 0, "skipped_cooldown", nil)
			idx++
			continue attemptLoop
		}
		if !e.breakerAllow(att.ProviderID) {
			addTraceAttempt(trace, att, 0, "skipped_breaker", nil)
			idx++
			continue attemptLoop
		}
		// Model lockout (M4.3): same skip as the non-streaming
		// loop — a locked triple is never dialled.
		if _, locked := e.lockoutLocked(att); locked {
			addTraceAttempt(trace, att, 0, "skipped_model_lockout", nil)
			idx++
			continue attemptLoop
		}
		// CT-1 direct-path tool-capability pre-check (streaming):
		// a tool-bearing request never dials a CONFIRMED-incapable
		// model — the target is skipped (trace: skipped, no
		// attempt spent, no provider call) and the chain moves
		// on. Unknown models fail open: a model never probed
		// and never synced is tried, never assumed incapable.
		// Non-tool requests skip this gate entirely.
		if len(req.Tools) > 0 {
			if capable, known := e.modelToolsCapable(att.ProviderID, att.Model); known && !capable {
				addTraceAttempt(trace, att, 0, "skipped_no_tool_capability", nil)
				idx++
				continue attemptLoop
			}
		}
		// Predictive TTFT breaker (M4.7a): same skip as the
		// non-streaming loop — a predicted-slow target is
		// skipped while another target remains.
		if idx+1 < len(active) && e.ttftSlow(att.ProviderID) {
			addTraceAttempt(trace, att, 0, "skipped_predictive_ttft", nil)
			idx++
			continue attemptLoop
		}
		// Per-key token limits (M5.3): same skip as the
		// non-streaming loop — a breached target is never
		// dialled; a ledger error fails closed.
		if breach, err := e.tokenLimitBreach(ctx, req, att); err != nil {
			addTraceAttempt(trace, att, 0, "skipped_token_limit", err)
			e.sendTerminal(out, err)
			return
		} else if breach != nil {
			if firstBreach == nil {
				firstBreach = breach
			}
			addTraceAttempt(trace, att, 0, "skipped_token_limit", nil)
			idx++
			continue attemptLoop
		}
		// Internal budgets, provider scope (M5.11): same skip as
		// the non-streaming loop — a provider over its internal
		// budget is never dialled; a ledger error fails closed.
		if breach, err := e.internalBudgetBreach(ctx, att); err != nil {
			addTraceAttempt(trace, att, 0, "skipped_internal_budget", err)
			e.sendTerminal(out, err)
			return
		} else if breach != nil {
			if firstBudgetBreach == nil {
				firstBudgetBreach = breach
			}
			addTraceAttempt(trace, att, 0, "skipped_internal_budget", nil)
			idx++
			continue attemptLoop
		}

		// Per-request budget (CHECKLIST 155): in strict mode
		// the chain stops BEFORE an attempt whose priced prompt
		// estimate would push cumulative spend over the budget.
		// Same undercount as the non-stream loop: a relayed
		// request's handoff tokens are not in the estimate.
		// Nothing was delivered yet (a failure after commit ends
		// the stream in the streamFailed branch below, so this
		// point is always pre-commit): the typed error goes out
		// as the terminal stream event and the server turns it
		// into a 402 while nothing was sent.
		blocked, est := e.budgetGate(req, att, spentUSD)
		if blocked {
			be := e.budgetExceededError(req, att, spentUSD, est)
			addTraceAttempt(trace, att, 0, "skipped_budget", be)
			e.sendTerminal(out, be)
			return
		}

		// Region-aware failover step (CHECKLIST 257): same
		// rule as the non-streaming loop — this attempt is
		// about to be dialled, so count the step from the
		// last retryable failure in the trace, once per
		// failure.
		e.noteRegionFailoverFromTrace(trace, att, &regionCounted)

		// No mid-stream resume: there is no prefix to continue
		// from, by design. A previous attempt can only have failed
		// before commit (a failure after commit ends the stream —
		// see the streamFailed branch), so partial is always empty
		// here; the request goes out unchanged.
		r := req
		r.Model = att.Model
		// The attempt's effort (CHECKLIST 255): same rule as
		// reqWithModel — a group entry's effort overrides the
		// client's reasoning_effort for this attempt; "" passes
		// the client's value through untouched.
		if att.Effort != "" {
			r.ReasoningEffort = att.Effort
		}
		// Context-relay (Phase D, D5): the request this attempt
		// streams with — the original request, or the original
		// plus the pending handoff as a leading system message
		// when a previous attempt requested one. Built lazily
		// (see relayAttemptReq): a target skipped by a gate
		// above never costs a summariser call. r is rebuilt from
		// req every iteration, so the injection never
		// accumulates. withEffortForwarding below preserves
		// Messages, so the injection survives it.
		r = e.relayAttemptReq(ctx, relay, r)
		start := time.Now()
		// The attempt in flight for the panic backstop above:
		// refreshed to the winner (streamAtt) just before the
		// forward call, once the hedge loser (if any) has had
		// its own bookkeeping.
		cur := att
		current = &cur
		currentStart = start
		currentTraced = len(trace.Attempts)

		var ch <-chan StreamEvent
		var first *StreamEvent
		streamAtt := att
		consumed := 1
		var cancelWinner context.CancelFunc
		streamSlot := false // true when this iteration holds a limiter slot
		// Preferred-account response-start failover (M4.8): the
		// deadline timer for this attempt (nil when not armed)
		// and the attempt ctx it cancels. Declared per
		// iteration: onCommit disarms the timer at first byte,
		// and the post-forward cleanup stops it.
		var preferredTimer *time.Timer
		streamCtx := ctx

		// A hedge target the budget gate would block is never
		// launched in parallel: the first attempt runs alone
		// instead (the gate above already cleared it). The gate
		// counts the first attempt's estimate too (spentUSD +
		// est) — same M5.14 fix as the non-stream loop. Never on
		// an explicit-order chain (CHECKLIST 191 — hedgeOff).
		hedgeBlocked := false
		if len(active) > 1 {
			hedgeBlocked, _ = e.budgetGate(req, active[1], spentUSD+est)
		}
		if idx == 0 && attemptsUsed == 0 && exempt == "" && !hedgeOff && e.hedgeAllowed(req) && len(active) > 1 && !hedgeBlocked && e.hedgeSecondAvailable(active[1], mode) {
			hs := e.hedgedStream(ctx, att, active[1], r)
			consumed = len(hs.started)
			attemptsUsed += consumed
			if hs.err != nil {
				for _, s := range hs.started {
					e.noteStreamStartFailure(trace, attemptResult{att: s, err: hs.err}, start, mode, req.CallerID, req.Model)
				}
				lastErr = hs.err
				idx += consumed
				continue attemptLoop
			}
			for _, s := range hs.started {
				tried[s.ProviderID] = true
			}
			if hs.loser != nil {
				lat := time.Since(start).Milliseconds()
				addTraceAttempt(trace, *hs.loser, lat, "hedged_cancelled", context.Canceled)
				if e.ledger != nil {
					// Cancelled hedge loser: a cancelled row,
					// never a failed one (see
					// recordCancelledAttempt).
					e.recordCancelledAttempt(req.CallerID, *hs.loser, lat)
				}
				e.breakerNeutral(hs.loser.ProviderID)
			}
			streamAtt = hs.winner
			ch = hs.ch
			first = hs.first
			cancelWinner = hs.cancel
		} else {
			client, ok := e.clientForAttempt(att)
			if !ok {
				e.noteStreamStartFailure(trace, attemptResult{att: att, err: fmt.Errorf("router: unknown provider %q", att.ProviderID)}, start, mode, req.CallerID, req.Model)
				attemptsUsed++
				lastErr = fmt.Errorf("router: unknown provider %q", att.ProviderID)
				idx++
				continue attemptLoop
			}
			// Effort forwarding (CHECKLIST 285-286): the same
			// resolution as callChat, for this wire — the
			// stamped attempt replaces streamAtt so every trace
			// entry for this hop (including the limiter-shed
			// one below) carries the forwarding label, and the
			// budget rides r into the stream.
			r, att = e.withEffortForwarding(r, att, client)
			streamAtt = att
			// Adaptive admission, as in callChat: a provider at
			// its cap sheds this stream instead of queueing. The
			// slot is held for the stream's duration (released
			// after forwardStream returns) but the stream's wall
			// time is never fed to the RTT estimate — it is not a
			// serving RTT. Hedged streams (the branch above) are
			// not limiter-counted: the hedge race owns their
			// lifetime and its loser is cancelled, not admitted.
			if !e.limiter.TryAcquire(att.ProviderID) {
				e.noteConcurrencyRejected(att.ProviderID)
				addTraceAttempt(trace, att, 0, "skipped_concurrency_limit", adaptive.ErrLimited)
				attemptsUsed++
				lastErr = adaptive.ErrLimited
				idx++
				continue attemptLoop
			}
			streamSlot = true
			streamSlotHeld = att.ProviderID
			tried[att.ProviderID] = true
			// Preferred-account response-start failover (M4.8):
			// arm the deadline on the FIRST, non-hedged attempt
			// when the immediate next attempt is a sibling of
			// the same provider and no rotation has happened
			// yet this request. The attempt runs on its own
			// child ctx so the deadline cancels only it.
			if idx == 0 && attemptsUsed == 0 && exempt == "" && !preferredRotated && e.cfg.PreferredFailoverAfter > 0 && preferredSiblingNext(active, idx) {
				var cancelAttempt context.CancelFunc
				streamCtx, cancelAttempt = context.WithCancel(ctx)
				preferredTimer = time.AfterFunc(e.cfg.PreferredFailoverAfter, func() {
					preferredFired.Store(true)
					cancelAttempt()
				})
			}
			// bridgeStream never fails synchronously: a provider
			// failure (including a start failure such as a refused
			// connection) arrives as an Err event on the channel,
			// and the streamFailed branch below handles it — with
			// the same 429-retry-same-provider rule when nothing
			// was streamed yet.
			ch = e.bridgeStream(streamCtx, streamAtt, client, r)
			attemptsUsed++
		}

		// Pre-commit preread (preread.go, entries 97 + 111): while
		// nothing has been delivered yet, hold the leading events
		// until the first real content arrives, so an empty or
		// erroring stream can still fail over without the client
		// ever seeing a partial body. Exempt requests run
		// unbuffered (forwardStream) and are recorded as such.
		//
		// Refresh the panic backstop's in-flight attempt to the
		// winner (the hedge loser, if any, already has its own
		// trace + ledger entries, so the length check below is
		// taken after them).
		winner := streamAtt
		current = &winner
		currentTraced = len(trace.Attempts)
		// Decision transparency (CHECKLIST 199): the decision is
		// published the moment this attempt's stream commits — the
		// first content byte is about to reach the client, so this
		// provider IS the serving provider and no failover can
		// change it afterwards. The callback fires inside the
		// forwarders, immediately before the first event is sent
		// on out, so the server's first-chunk read of LastRouting
		// is ordered after it by the channel send/receive. A
		// stream that turns out empty (or errors) before commit
		// never fires it: the next attempt's commit records the
		// fallback instead, with the failed attempt counted in
		// the trace (fallback-after-N).
		onCommit := func() {
			e.recordDecision(trace, contract, treeOrdered, strategy, req.Model, streamAtt.Model, streamAtt.ProviderID)
			// Predictive TTFT (M4.7a): the first byte just
			// reached the client — record how long it took.
			e.recordTTFT(streamAtt.ProviderID, time.Since(start))
			// Preferred-account failover (M4.8): the first byte
			// arrived — disarm the response-start deadline.
			if preferredTimer != nil {
				preferredTimer.Stop()
			}
		}
		var outcome streamOutcome
		var serr error
		var doneResp *provider.ChatResponse
		pr := prereadInfo{}
		if partial.Len() == 0 && exempt == "" {
			outcome, serr, pr, doneResp = e.forwardStreamPreread(ctx, out, ch, first, &partial, onCommit)
		} else {
			if partial.Len() == 0 && exempt != "" {
				pr = prereadInfo{state: "exempt:" + exempt}
			}
			outcome, serr, doneResp = e.forwardStream(ctx, out, ch, first, &partial, onCommit)
		}
		// Preferred-account failover (M4.8): the attempt is
		// over — stop the deadline timer if it is still armed
		// (a stray fire after this point cancels a dead ctx).
		if preferredTimer != nil {
			preferredTimer.Stop()
		}
		if streamSlot {
			e.limiter.Release(streamAtt.ProviderID)
			streamSlot = false
			streamSlotHeld = ""
		}
		if cancelWinner != nil {
			cancelWinner()
		}
		lat := time.Since(start).Milliseconds()
		// The reasoning handoff label for this hop ("" when the
		// request carried no reasoning state): computed once
		// per attempt and reused by every outcome branch below.
		label := e.attemptReasoningLabel(streamAtt, r)
		// Async guardrail cut (CHECKLIST 185): the server
		// cancelled this stream's context because an async
		// guardrail verdict said BLOCK. The cancel can surface
		// as streamCtxDone or as a streamFailed carrying the
		// provider client's cancellation error, so it is checked
		// here — before the outcome switch, on the cancel cause
		// (context.Cause propagates through the deadline
		// wrapper), and only when the stream did not already
		// complete: a stream that reached Done was fully
		// delivered, and the verdict is too late to cut it.
		// The cut is a policy decision, never a provider
		// failure and never a failover trigger: the trace
		// carries the async_guardrail_blocked outcome, the
		// ledger records the attempt as failed with the local
		// prompt estimate only (billed for what was actually
		// served before the cut — the prompt — never for the
		// full response that never arrived, and never with an
		// invented completion split), and no health signal is
		// touched (no attempt stats, no breaker, no cooldown).
		// The chain ends here: lastErr is the block error, so
		// the drain returns it and the server writes the
		// terminal refusal frame.
		if outcome != streamDone {
			var blk *guardrails.AsyncBlockError
			if errors.As(context.Cause(ctx), &blk) {
				addTraceAttemptStreamReasoning(trace, streamAtt, lat, guardrails.AsyncBlockOutcome, blk, pr, "error", label)
				if e.ledger != nil {
					e.recordAttempt(req.CallerID, streamAtt, false, lat, withCompression(e.streamFallbackUsage(streamAtt.ProviderID, streamAtt.Model, e.wireFor(streamAtt.ProviderID), req), req))
				}
				lastErr = blk
				break attemptLoop
			}
		}
		// Stream-lifetime cap (CHECKLIST 255): the cap fired
		// while this attempt was in flight — the upstream call
		// is already terminated (the lifetime context cancelled
		// it), so the stream ends here with the typed lifetime
		// error as the terminal event. No failover: the cap
		// bounds this request's stream, and a fresh attempt
		// would hand the client a fresh lifetime. A stream that
		// already completed (outcome == streamDone) is
		// unaffected — the cap cannot un-complete it. The
		// attempt records a failed ledger row with unknown usage
		// (a cut stream's served tokens are unknown to the
		// engine), no attempt-stats signal and no
		// breaker/cooldown touch: a gateway time bound says
		// nothing about the provider being down. It is checked
		// after the guardrail cut above: a policy block is the
		// more specific verdict when both fired.
		if outcome != streamDone {
			if lt := streamLifetimeCause(ctx); lt != nil {
				e.noteStreamLifetimeExceeded()
				addTraceAttemptStreamReasoning(trace, streamAtt, lat, "error", lt, pr, "error", label)
				if e.ledger != nil {
					e.recordAttempt(req.CallerID, streamAtt, false, lat, provider.Usage{})
				}
				lastErr = lt
				break attemptLoop
			}
		}
		switch outcome {
		case streamDone:
			addTraceAttemptStreamReasoning(trace, streamAtt, lat, "success", nil, pr, "done", label)
			e.noteAttemptStats(streamAtt.ProviderID, streamAtt.KeyID, true, lat)
			if e.ledger != nil {
				// G1 (streaming usage accounting): the Done event
				// carries the upstream's reassembled response, and
				// every wire client aggregates the upstream's
				// streaming usage into it (OpenAI's final usage
				// chunk — requested via stream_options.include_usage
				// — Anthropic message_delta, Gemini usageMetadata,
				// Cohere message-end, Bedrock metadata). When the
				// upstream reported a split, it is reconciled exactly
				// like a non-streaming report (provider numbers kept
				// verbatim, local estimate alongside, divergence
				// counted, reported cost preserved). When it reported
				// nothing, the local prompt count is the fallback,
				// labelled LocalEstimate (streamFallbackUsage) —
				// never a guessed split or a made-up cost. The
				// compression saving rides on the same row
				// (CHECKLIST 183). One row per attempt: the usage is
				// the LAST reported event, never a sum.
				usage := e.streamFallbackUsage(streamAtt.ProviderID, streamAtt.Model, e.wireFor(streamAtt.ProviderID), req)
				if doneResp != nil && (doneResp.Usage.PromptTokens != 0 || doneResp.Usage.CompletionTokens != 0 || doneResp.Usage.TotalTokens != 0) {
					usage = e.reconcileUsage(streamAtt.ProviderID, streamAtt.Model, req, doneResp.Usage)
				}
				e.recordAttempt(req.CallerID, streamAtt, true, lat, withCompression(usage, req))
			}
			e.breakerSuccess(streamAtt.ProviderID)
			// A success clears any lockout on the triple (M4.3).
			e.lockoutClear(streamAtt)
			// A success ends the cooldown — hard and soft alike.
			e.clearCooldown(ClientKey(streamAtt.ProviderID, streamAtt.KeyID))
			e.clearSoftCooldown(ClientKey(streamAtt.ProviderID, streamAtt.KeyID))
			if refusalMode {
				e.noteRefusalServed()
			}
			e.recordDecision(trace, contract, treeOrdered, strategy, req.Model, streamAtt.Model, streamAtt.ProviderID)
			noteTreeServed(trace, e.strategyTreeFor(req.Model), streamAtt.ProviderID, streamAtt.Model, strategy)
			// Context-relay (Phase D, D5): feed the successful
			// stream outcome into the relay. The request ends
			// here, so the record only lands in LastRelayStatus
			// — the handoff is built lazily and never costs a
			// summariser call for a finished request.
			e.relayNote(relay, req, attemptResult{att: streamAtt, latencyMs: lat, called: true})
			return
		case streamCtxDone:
			// Terminal "cancelled" when the caller went away;
			// the engine's own deadline budget expiring is an
			// "error" terminal instead — the stream still ends
			// with a terminal event, but it was the router
			// that ended it, not the client.
			ctxTerminal := "cancelled"
			if errors.Is(serr, context.DeadlineExceeded) {
				ctxTerminal = "error"
			}
			addTraceAttemptStreamReasoning(trace, streamAtt, lat, "error", serr, pr, ctxTerminal, label)
			if e.ledger != nil {
				// Cancelled stream: usage unknown.
				e.recordAttempt(req.CallerID, streamAtt, false, lat, provider.Usage{})
			}
			lastErr = serr
			break attemptLoop
		case streamFailed:
			// Context-relay (Phase D, D5): feed the failed stream
			// outcome into the relay up front, so every exit
			// below (refusal chain, 429 retry, failover, terminal)
			// carries it — a pre-commit failure is what the next
			// attempt's handoff is built from. A post-commit
			// failure ends the request, so the record only lands
			// in LastRelayStatus. A caller cancellation surfacing
			// here is recorded as the failure it was; the
			// streamCtxDone outcome (a clean cancel) is not.
			e.relayNote(relay, req, attemptResult{att: streamAtt, err: serr, latencyMs: lat, called: true})
			// failTerminal is the stream's ending once content
			// was committed ("error": the client gets a
			// terminal error event); before commit the chain
			// fails over and the next attempt owns the
			// terminal, so this attempt records "".
			failTerminal := ""
			if partial.Len() > 0 {
				failTerminal = "error"
			}
			if isContentRefusalErr(serr) {
				// Content refusal (CHECKLIST 161): no health
				// signal — no attempt stats, no breaker, no
				// cooldown (a refusal says nothing about the
				// provider being down). Pre-commit it routes
				// to the refusal chain like any other refusal;
				// post-commit the client already owns the
				// stream, so it ends here with the refusal
				// error instead of switching targets.
				if partial.Len() > 0 {
					addTraceAttemptStreamReasoning(trace, streamAtt, lat, "stream_error", serr, pr, failTerminal, label)
					if e.ledger != nil {
						e.recordAttempt(req.CallerID, streamAtt, false, lat, provider.Usage{})
					}
					e.breakerNeutral(streamAtt.ProviderID)
					e.noteRefusalSeen(streamAtt.ProviderID, streamAtt.Model)
					lastErr = serr
					break attemptLoop
				}
				e.noteRefusalPreread(trace, attemptResult{att: streamAtt, err: serr, latencyMs: lat, called: true}, req.CallerID, pr)
				lastErr = serr
				if exempt != "" {
					break attemptLoop
				}
				if refusalMode {
					idx += consumed
					continue attemptLoop
				}
				refusalMode = true
				if rc := e.refusalChainFor(req.Model, ordered, idx+consumed, tried); len(rc) == 0 {
					refusalExhausted = true
					break attemptLoop
				} else {
					active, idx = rc, 0
				}
				continue attemptLoop
			}
			var retryWait time.Duration
			// Model lockout (M4.3): a failed stream records on
			// the triple exactly like a failed chat call.
			e.lockoutRecord(streamAtt, serr)
			if ra, present, ok := rateLimitHint(serr); ok {
				// rateLimitWait is 0 for an abandoned target
				// (Retry-After >= 60s), so the same-provider
				// retry below (gated on retryWait > 0) never
				// fires for it — the chain moves on or ends.
				retryWait = e.rateLimitWait(ra, present, 1)
				if NormalizeCooldownMode(mode) == CooldownSoftDemote {
					e.startSoftCooldown(ClientKey(streamAtt.ProviderID, streamAtt.KeyID), ra, present)
				} else {
					e.setCooldown(ClientKey(streamAtt.ProviderID, streamAtt.KeyID), retryAfterHonouredWait(ra, present, e.cfg.RetryAfterCap))
				}
				e.breakerNeutral(streamAtt.ProviderID)
				oc := "rate_limited"
				if partial.Len() > 0 {
					oc = "stream_error"
				}
				addTraceAttemptPrereadRetryTerminalReasoning(trace, streamAtt, lat, oc, serr, pr, retryInfoFor(ra, present, e.cfg.RetryAfterCap), failTerminal, label)
			} else {
				if !errors.Is(serr, context.Canceled) && !errors.Is(serr, context.DeadlineExceeded) {
					if isReasoningUnrepresentableErr(serr) {
						// The target wire cannot represent the
						// restored reasoning state —
						// breaker-neutral, no cooldown (see
						// noteFailure for the same carve-out).
						e.breakerNeutral(streamAtt.ProviderID)
					} else if NormalizeCooldownMode(mode) == CooldownSoftDemote && isRetryableFailure(serr) {
						// Demote, don't drop (see noteFailure).
						ra, present := retryAfterOf(serr)
						e.startSoftCooldown(ClientKey(streamAtt.ProviderID, streamAtt.KeyID), ra, present)
						e.breakerNeutral(streamAtt.ProviderID)
						// Hard failure in soft-demote mode: clear
						// the LKGP pin here (RT-2; invalidateLKGP
						// skips non-hard members of this branch).
						e.invalidateLKGP(req.Model, streamAtt.ProviderID, serr)
					} else if isModelNotFoundErr(serr) {
						// The provider answered fine — only the
						// model is dead at this target — so the
						// failure must not count against the
						// provider's circuit breaker.
						e.breakerNeutral(streamAtt.ProviderID)
					} else if isClientErrorFailure(serr) {
						// A 4xx is a property of the request
						// or the operator's key, never a
						// provider-health signal: it must
						// never trip the circuit breaker (see
						// noteFailure). Counted as
						// breaker-ignored 4xx. A dead pool key
						// (401) is cooled per-key, as in
						// noteFailure.
						e.coolDeadKey(streamAtt, mode)
						e.breakerNeutral(streamAtt.ProviderID)
						e.countBreakerIgnored4xx(streamAtt.ProviderID)
					} else {
						e.breakerFailure(streamAtt.ProviderID, provider.ClassifyError(serr, provider.EndpointChatCompletions).Reason)
						// Hard failure (5xx / timeout /
						// transport): clear the LKGP pin for the
						// chain (RT-2) — see noteFailure.
						e.invalidateLKGP(req.Model, streamAtt.ProviderID, serr)
					}
				}
				oc := "error"
				if partial.Len() > 0 {
					oc = "stream_error"
				}
				addTraceAttemptStreamReasoning(trace, streamAtt, lat, oc, serr, pr, failTerminal, label)
			}
			// A real failed stream counts in the live stats
			// (except caller/engine cancellations, which say
			// nothing about the provider).
			if !errors.Is(serr, context.Canceled) && !errors.Is(serr, context.DeadlineExceeded) {
				e.noteAttemptStats(streamAtt.ProviderID, streamAtt.KeyID, false, lat)
			}
			if e.ledger != nil {
				// Failed stream: usage unknown to the engine.
				e.recordAttempt(req.CallerID, streamAtt, false, lat, provider.Usage{})
			}
			lastErr = serr
			if exempt != "" {
				// Replay-unsafe request: exactly one attempt — a
				// failure ends the request, never a failover.
				// (Skips above still move on: a skipped target was
				// never sent the request, so trying the next one
				// still sends it exactly once.)
				break attemptLoop
			}
			// Committed streams never switch (entries 97 + 111):
			// once the first content byte reached the client, the
			// client owns the failure — no resume on another
			// provider, no replay. The stream ends with the
			// error (the server turns it into an honest SSE
			// error frame). This deliberately supersedes the old
			// mid-stream resume: a resume IS a switch after
			// commit, and the preread rule forbids it.
			if partial.Len() > 0 {
				break attemptLoop
			}
			// Preferred-account response-start failover (M4.8):
			// this attempt was cancelled by the response-start
			// deadline, not by the caller — rotate to the ONE
			// sibling (the immediate next attempt, armed above)
			// instead of stopping. A context.Canceled error
			// would otherwise be terminal here and end the
			// chain; the rotation bypasses exactly that check.
			// The attempt is breaker-neutral (the cancellation
			// guard above already skipped the breaker), and the
			// rotation happens only once per request.
			if preferredFired.Load() && !preferredRotated {
				preferredRotated = true
				idx += consumed
				continue attemptLoop
			}
			// Terminal failure before any content was streamed
			// (malformed request, forbidden, unrepresentable —
			// entries 151/183): the chain stops, because no
			// target could serve this request. A 401 is not
			// terminal while a sibling key of the same
			// provider remains (see isTerminalErrFor).
			if partial.Len() == 0 && isTerminalErrFor(serr, active, idx+consumed, streamAtt) {
				break attemptLoop
			}
			// Retry the same provider after a 429 only when
			// nothing was streamed yet, this was a single
			// (non-hedged) attempt, it was the last attempt, and
			// budget remains.
			if partial.Len() == 0 && retryWait > 0 && consumed == 1 && idx+consumed >= len(active) && attemptsUsed < maxAttempts {
				if !e.sleep(ctx, retryWait) {
					lastErr = lifetimeErr(ctx)
					break attemptLoop
				}
				e.clearCooldown(ClientKey(streamAtt.ProviderID, streamAtt.KeyID))
				continue attemptLoop
			}
			// Same-host transient retry (M4.5): a 502/503/504
			// before any content, on a single (non-hedged)
			// attempt, is retried on the SAME target — up to
			// FailoverMaxRetries times (default 3), with
			// decorrelated full-jitter backoff (base
			// FailoverRetryDelay, default 200 ms, cap 2 s),
			// bounded set-wide by FailoverMaxSetRetries —
			// before the chain moves on. This is the mirror
			// of the non-streaming loop's retry, and of the
			// 429 retry above; an explicit FailoverBeforeRetry
			// skips the retry when a next target exists. The
			// loop top re-runs the breaker/cooldown/budget
			// gates, so a tripped breaker stops the retries.
			// Committed streams never retry (partial.Len() > 0
			// breaks above); replay-exempt requests never
			// retry.
			if partial.Len() == 0 && consumed == 1 && exempt == "" && isTransientStatus(serr) && e.failoverRetryAllowed(idx+consumed < len(active)) && e.transientRetryAllowed(streamTransientCount, streamSetRetriesUsed) && attemptsLeaveOne(attemptsUsed, maxAttempts, idx+consumed < len(active)) {
				delay := e.transientDelay(streamTransientPrev)
				streamTransientPrev = delay
				streamTransientCount++
				streamSetRetriesUsed++
				if !e.transientSleep(ctx, delay) {
					lastErr = lifetimeErr(ctx)
					break attemptLoop
				}
				continue attemptLoop
			}
			// Identical-error early stop (M4.7d): same rule as
			// the non-streaming loop — 3 consecutive identical
			// model-shape 400s stop the chain.
			if streamIdenticalErr.note(serr) {
				break attemptLoop
			}
			// Stream-recovery early retry (M4.8): the stream
			// failed before commit, so the client saw nothing
			// — transparently re-open the SAME target's stream
			// up to 4 times before spending a failover. The
			// loop top re-runs the breaker/cooldown/budget
			// gates, and every re-open counts toward
			// MaxAttempts.
			if partial.Len() == 0 && consumed == 1 && exempt == "" && isStreamEarlyRetryable(serr) && earlyRetryCount < earlyRetryMax && attemptsLeaveOne(attemptsUsed, maxAttempts, idx+consumed < len(active)) {
				earlyRetryCount++
				continue attemptLoop
			}
			idx += consumed
			continue attemptLoop
		case streamEmpty:
			// Context-relay (Phase D, D5): the preread verdict —
			// the stream ended with no content before commit, so
			// the provider served nothing. A failed attempt, fed
			// to the relay like any other (a content_filter
			// finish inside it is a refusal, recorded as a
			// failure by the branches below, never a success).
			e.relayNote(relay, req, attemptResult{att: streamAtt, err: serr, latencyMs: lat, called: true})
			var ese *provider.EmptyStreamError
			if errors.As(serr, &ese) && ese.FinishReason == "content_filter" {
				// Deliberately refused (content_filter finish):
				// the provider is healthy, only the prompt was
				// refused — so this is a refusal-chain hop, not
				// a failure-chain one: no attempt stats, no
				// breaker, no cooldown (see noteRefusal).
				e.noteRefusalPreread(trace, attemptResult{att: streamAtt, err: serr, latencyMs: lat, called: true}, req.CallerID, pr)
				lastErr = serr
				if exempt != "" {
					break attemptLoop
				}
				if refusalMode {
					idx += consumed
					continue attemptLoop
				}
				refusalMode = true
				if rc := e.refusalChainFor(req.Model, ordered, idx+consumed, tried); len(rc) == 0 {
					refusalExhausted = true
					break attemptLoop
				} else {
					active, idx = rc, 0
				}
				continue attemptLoop
			}
			// Preread verdict: the stream ended with no content
			// before commit, so the client saw nothing and the
			// chain moves on to the next target. The provider
			// served nothing — a failed attempt.
			addTraceAttemptPrereadReasoning(trace, streamAtt, lat, "empty_stream", serr, pr, label)
			if !errors.Is(serr, context.Canceled) && !errors.Is(serr, context.DeadlineExceeded) {
				e.noteAttemptStats(streamAtt.ProviderID, streamAtt.KeyID, false, lat)
			}
			if e.ledger != nil {
				// Empty stream: usage unknown to the engine.
				e.recordAttempt(req.CallerID, streamAtt, false, lat, provider.Usage{})
			}
			e.breakerFailure(streamAtt.ProviderID, provider.ClassifyError(serr, provider.EndpointChatCompletions).Reason)
			lastErr = serr
			// Stream-recovery early retry (M4.8): an empty
			// stream before commit is a truncation — re-open
			// the same target up to 4 times before failing over.
			if partial.Len() == 0 && consumed == 1 && exempt == "" && isStreamEarlyRetryable(serr) && earlyRetryCount < earlyRetryMax && attemptsLeaveOne(attemptsUsed, maxAttempts, idx+consumed < len(active)) {
				earlyRetryCount++
				continue attemptLoop
			}
			idx += consumed
			continue attemptLoop
		}
	}
	// M5.3: every target was skipped on a token limit — the
	// terminal error names the breach, not the generic
	// no-attempts error.
	if lastErr == nil && firstBreach != nil {
		lastErr = firstBreach
	}
	// M5.11: every target was skipped on a provider internal
	// budget — the same, with the token-limit breach taking
	// precedence when both were seen.
	if lastErr == nil && firstBudgetBreach != nil {
		lastErr = firstBudgetBreach
	}
	if lastErr == nil {
		lastErr = ErrNoAvailableAttempt
	}
	if refusalMode && isContentRefusalErr(lastErr) {
		// Ended while routing content refusals: when the refusal
		// chain ran dry the answer is an honest refusal
		// (ErrContentRefused → 403 / -32001 from the server);
		// a mid-chain stop keeps the bare refusal error, which
		// the server answers the same way.
		if refusalExhausted || idx >= len(active) {
			e.noteRefusalExhausted()
			lastErr = fmt.Errorf("%w: %w", ErrContentRefused, lastErr)
		}
	}
	e.sendTerminal(out, e.mapDeadlineErr(lastErr, parentCtx))
}

// hedgedStream is the streaming analogue of hedgedChat: start the
// first stream, and if no first event arrives within HedgeDelay, start
// the second stream in parallel and take whichever yields its first
// event first; the loser is cancelled. The winner's first event is
// already consumed and returned in first so the caller can forward it
// before reading the rest of the winner's channel.
type hedgedStreamResult struct {
	winner  Attempt
	ch      <-chan StreamEvent
	first   *StreamEvent
	started []Attempt
	loser   *Attempt
	err     error // non-nil when no stream could be started
	cancel  context.CancelFunc
}

func (e *Engine) hedgedStream(ctx context.Context, first, second Attempt, req provider.ChatRequest) hedgedStreamResult {
	c1, ok := e.clientForAttempt(first)
	if !ok {
		return hedgedStreamResult{started: []Attempt{first}, err: fmt.Errorf("router: unknown provider %q", first.ProviderID)}
	}
	ctxA, cancelA := context.WithCancel(ctx)
	// bridgeStream never fails synchronously: a provider failure
	// arrives as an Err event on the channel, so there is no start-fail
	// path here — the winner's first event may itself be an error, and
	// forwardStream turns it into a failover like any other failure.
	// Effort forwarding (CHECKLIST 285-286): the request the
	// first stream sends carries the resolved budget, and the
	// stamped attempt flows into winner/started/loser so the
	// trace entry carries the forwarding label.
	reqA, first := e.withEffortForwarding(reqWithModel(req, first), first, c1)
	chA := e.bridgeStream(ctxA, first, c1, reqA)
	firstA := firstEventChan(chA)
	timer := time.NewTimer(e.cfg.HedgeDelay)
	defer timer.Stop()
	select {
	case ev := <-firstA:
		return hedgedStreamResult{winner: first, ch: chA, first: &ev, started: []Attempt{first}, cancel: cancelA}
	case <-timer.C:
	case <-ctx.Done():
		cancelA()
		return hedgedStreamResult{started: []Attempt{first}, err: ctx.Err()}
	}

	c2, ok := e.clientForAttempt(second)
	if !ok {
		// Second provider is not registered; keep waiting on the first.
		select {
		case ev := <-firstA:
			return hedgedStreamResult{winner: first, ch: chA, first: &ev, started: []Attempt{first}, cancel: cancelA}
		case <-ctx.Done():
			cancelA()
			return hedgedStreamResult{started: []Attempt{first}, err: ctx.Err()}
		}
	}
	ctxB, cancelB := context.WithCancel(ctx)
	// Effort forwarding (CHECKLIST 285-286): as for the first
	// stream above — the second stream's request carries its own
	// wire's resolved budget.
	reqB, second := e.withEffortForwarding(reqWithModel(req, second), second, c2)
	chB := e.bridgeStream(ctxB, second, c2, reqB)
	firstB := firstEventChan(chB)
	select {
	case ev := <-firstA:
		cancelB()
		return hedgedStreamResult{winner: first, ch: chA, first: &ev, started: []Attempt{first, second}, loser: &second, cancel: cancelA}
	case ev := <-firstB:
		cancelA()
		return hedgedStreamResult{winner: second, ch: chB, first: &ev, started: []Attempt{first, second}, loser: &first, cancel: cancelB}
	case <-ctx.Done():
		cancelA()
		cancelB()
		return hedgedStreamResult{started: []Attempt{first, second}, err: ctx.Err()}
	}
}

// firstEventChan returns a channel that yields the first event from ch
// (or a synthetic error if ch closes first). The helper goroutine ends
// when ch yields, closes, or its ctx is cancelled.
func firstEventChan(ch <-chan StreamEvent) <-chan StreamEvent {
	out := make(chan StreamEvent, 1)
	go func() {
		ev, ok := <-ch
		if !ok {
			out <- StreamEvent{Err: errors.New("router: upstream closed stream without first event")}
			return
		}
		out <- ev
	}()
	return out
}

// ---------------------------------------------------------------------------
// Server-facing API (internal/server Engine + StreamResumer contracts).
//
// These methods take no explicit chain: the chain is resolved per
// request model via chainFor. The chain-based ChatWithChain /
// ChatStreamWithChain above remain the core; these are thin adapters
// over them, so all routing behaviour (fallback, 429 handling,
// breakers, hedging, preread) is shared, not duplicated.
// ---------------------------------------------------------------------------

// chainFor resolves the fallback chain for a request model: Config.ChainFor
// when set, otherwise every registered client for the model, in sorted
// provider-ID order (deterministic, if arbitrary).
func (e *Engine) chainFor(model string) []Attempt {
	// P2-ROUTE-001: consult the declarative per-model fallback chain
	// before falling back to config or client IDs. A chain with at
	// least one enabled entry takes precedence; an unknown model or
	// an all-disabled chain falls through to config/client IDs.
	if chain := e.FallbackChainFor(model); len(chain) > 0 {
		return chain
	}
	if e.cfg.ChainFor != nil {
		return e.cfg.ChainFor(model)
	}
	ids := e.clientIDs()
	chain := make([]Attempt, 0, len(ids))
	for _, id := range ids {
		chain = append(chain, Attempt{ProviderID: id, Model: model})
	}
	return chain
}

// pinChain puts req's pinned provider (ChatRequest.PinProviderID
// — a Responses continuation's original provider) FIRST in the
// chain and keeps the rest in chain order after it. The pin is a
// preference, not a narrowing: the original provider is tried
// first, and when it fails the branched request fails over to the
// next target like any other request (CHECKLIST 237) — the
// transcript is gateway-held, so replaying the same branched
// input against the next target cannot double-apply anything.
// When the pinned provider is not in the chain it returns the
// chain unchanged. The pin never adds a provider — it only
// reorders — so it cannot invent a target.
//
// (CHECKLIST 169 used to narrow the chain to the single pinned
// attempt, because continuations were replay-exempt. The 237-238
// slice removed that exemption, so the pin became a reorder.)
func pinChain(chain []Attempt, pin string) []Attempt {
	if pin == "" {
		return chain
	}
	for i, att := range chain {
		if att.ProviderID == pin {
			if i == 0 {
				return chain
			}
			out := make([]Attempt, 0, len(chain))
			out = append(out, att)
			out = append(out, chain[:i]...)
			out = append(out, chain[i+1:]...)
			return out
		}
	}
	return chain
}

// Chat implements the server.Engine contract: it routes req through the
// fallback chain resolved for req.Model and returns the first successful
// response. Use LastTrace to see every attempt. An "auto/*" model is
// resolved to a concrete model first (see resolveAuto).
//
// Single-flight: a coalescable non-streaming request (see
// coalescing.Coalescable) shares one flight with any identical
// request already in flight — the whole retry loop (ChatWithChain)
// runs inside that flight, so waiters share one backoff sequence and
// one upstream call, and a shared failure reaches them all.
// Streaming requests never reach Chat, and tool-using requests
// bypass the group.
func (e *Engine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	// Forced-reasoning policy (CHECKLIST 311-312): the requested
	// model and the client's own effort are captured BEFORE
	// resolveAutoRequest rewrites them, so a policy on the route
	// (e.g. auto/coding) matches and a client value is judged as
	// the client sent it, not as the tier restamped it.
	requestedModel, clientEffort := req.Model, req.ReasoningEffort
	reqStrategy, req, intentEffort, autoDisp := e.resolveAutoRequest(ctx, req)
	if autoDisp.Err != nil {
		// Fail-closed variant error (auto/subscription rung
		// empty, auto/lkgp unrecorded): surface it, never fall
		// back to another model.
		return nil, autoDisp.Err
	}
	// Lossless-first compression (CHECKLIST 183): before the
	// chain is built AND before the coalescing key is computed,
	// so the key is on the prompt as sent — two requests that
	// differ only in compression must not share a flight unless
	// their compressed prompts are identical.
	req = e.maybeCompress(req)
	chain := withResolvedEffort(e.chainForDispatch(req.Model, autoDisp.Ordered), intentEffort)
	// Modality bridges (B3-24): reroute an image-bearing request
	// off a confirmed non-vision head before the effort policy
	// stamps the chain, so the policy sees the bridged chain.
	chain, req = e.maybeBridgeModality(chain, req, autoDisp.Ordered, intentEffort)
	chain, req = e.withForcedEffort(chain, req, requestedModel, clientEffort)
	if !coalescing.Coalescable(req) {
		resp, _, err := e.chatWithChainStrategy(ctx, chain, req, reqStrategy, autoDisp.OrderedExplicit)
		return resp, err
	}
	key, err := coalescing.RequestKeyForTenant(req, req.CallerID)
	if err != nil {
		// Unkeyable request (a NaN/Inf float param cannot be JSON
		// encoded): run it uncoalesced rather than failing it.
		resp, _, err := e.chatWithChainStrategy(ctx, chain, req, reqStrategy, autoDisp.OrderedExplicit)
		return resp, err
	}
	started := time.Now()
	res, shared := e.flights.Do(ctx, key, func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		return e.chatWithChainStrategy(ctx, chain, req, reqStrategy, autoDisp.OrderedExplicit)
	})
	if shared {
		e.noteShared(req, res, time.Since(started))
	}
	// Request-scoped attribution (CHECKLIST 317-318): a waiter
	// shares the leader's flight, so the leader's trace is the
	// waiter's trace too — publish it into this caller's sink.
	// (The leader already published it via the
	// ChatWithChainStrategy defer; re-publishing the same
	// pointer is a no-op.)
	if att := AttributionFrom(ctx); att != nil {
		att.PublishTrace(res.Trace)
	}
	return res.Resp, res.Err
}

// ChatStream implements the server.Engine contract: it routes a streaming
// request through the fallback chain resolved for req.Model, calls
// onDelta once per content fragment (in order), and returns the
// reassembled response. Pre-commit failover happens inside the engine
// (preread buffer: empty/error streams fail over with nothing
// delivered); after the first content byte a failure ends the stream
// with an error — never a switch. Fragments already delivered are
// never re-emitted.
func (e *Engine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	// Forced-reasoning policy (CHECKLIST 311-312): see Chat —
	// the requested model and the client's own effort are
	// captured before resolveAutoRequest rewrites them.
	requestedModel, clientEffort := req.Model, req.ReasoningEffort
	reqStrategy, req, intentEffort, autoDisp := e.resolveAutoRequest(ctx, req)
	if autoDisp.Err != nil {
		return nil, autoDisp.Err
	}
	req = e.maybeCompress(req)
	chain := withResolvedEffort(e.chainForDispatch(req.Model, autoDisp.Ordered), intentEffort)
	// Modality bridges (B3-24): reroute an image-bearing request
	// off a confirmed non-vision head before the effort policy
	// stamps the chain, so the policy sees the bridged chain.
	chain, req = e.maybeBridgeModality(chain, req, autoDisp.Ordered, intentEffort)
	chain, req = e.withForcedEffort(chain, req, requestedModel, clientEffort)
	return e.chatStreamDrain(ctx, chain, req, 0, onDelta, reqStrategy, autoDisp.OrderedExplicit)
}

// ChatStreamFrom implements the server.StreamResumer contract: like
// ChatStream, but the first skipChars runes of the reassembled content
// are not delivered to onDelta (the client already has them). It runs a
// fresh internal stream — the engine's own pre-commit failover still
// applies inside it — and trims the already-sent prefix rune-wise, so
// a resume never duplicates content on the wire even if the new stream
// replays from the start. skipChars <= 0 behaves like ChatStream. The
// returned response always holds the full reassembled content. This is
// an explicit entry point: the server does NOT invoke it automatically
// after an in-request failure (no switch after commit).
func (e *Engine) ChatStreamFrom(ctx context.Context, req provider.ChatRequest, skipChars int, onDelta func(string) error) (*provider.ChatResponse, error) {
	// Forced-reasoning policy (CHECKLIST 311-312): see Chat.
	requestedModel, clientEffort := req.Model, req.ReasoningEffort
	reqStrategy, req, intentEffort, autoDisp := e.resolveAutoRequest(ctx, req)
	if autoDisp.Err != nil {
		return nil, autoDisp.Err
	}
	req = e.maybeCompress(req)
	chain := withResolvedEffort(e.chainForDispatch(req.Model, autoDisp.Ordered), intentEffort)
	// Modality bridges (B3-24): reroute an image-bearing request
	// off a confirmed non-vision head before the effort policy
	// stamps the chain, so the policy sees the bridged chain.
	chain, req = e.maybeBridgeModality(chain, req, autoDisp.Ordered, intentEffort)
	chain, req = e.withForcedEffort(chain, req, requestedModel, clientEffort)
	return e.chatStreamDrain(ctx, chain, req, skipChars, onDelta, reqStrategy, autoDisp.OrderedExplicit)
}

// ChatStreamChunks implements the chunk-aware streaming contract: it
// routes a streaming request through the fallback chain resolved for
// req.Model, calls onChunk once per chunk in order (text delta,
// tool-call deltas, finish reason), and returns the reassembled
// response (content, completed tool calls, finish reason). Pre-commit
// failover happens inside the engine, so chunks already delivered are
// never re-emitted; after the first content byte a failure ends the
// stream with an error — never a switch.
func (e *Engine) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	// Forced-reasoning policy (CHECKLIST 311-312): see Chat.
	requestedModel, clientEffort := req.Model, req.ReasoningEffort
	reqStrategy, req, intentEffort, autoDisp := e.resolveAutoRequest(ctx, req)
	if autoDisp.Err != nil {
		return nil, autoDisp.Err
	}
	req = e.maybeCompress(req)
	chain := withResolvedEffort(e.chainForDispatch(req.Model, autoDisp.Ordered), intentEffort)
	// Modality bridges (B3-24): reroute an image-bearing request
	// off a confirmed non-vision head before the effort policy
	// stamps the chain, so the policy sees the bridged chain.
	chain, req = e.maybeBridgeModality(chain, req, autoDisp.Ordered, intentEffort)
	chain, req = e.withForcedEffort(chain, req, requestedModel, clientEffort)
	return e.chatStreamDrainChunks(ctx, chain, req, 0, onChunk, reqStrategy, autoDisp.OrderedExplicit)
}

// ChatStreamFromChunks is ChatStreamChunks with resume: like
// ChatStreamFrom, but chunk-aware. skipChars counts text runes only;
// tool-call deltas that fall inside the skipped prefix are dropped,
// so a resumed stream may lose a tool call that was mid-flight when
// the previous attempt failed. Explicit entry point only, like
// ChatStreamFrom — never invoked automatically after a failure.
func (e *Engine) ChatStreamFromChunks(ctx context.Context, req provider.ChatRequest, skipChars int, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	// Forced-reasoning policy (CHECKLIST 311-312): see Chat.
	requestedModel, clientEffort := req.Model, req.ReasoningEffort
	reqStrategy, req, intentEffort, autoDisp := e.resolveAutoRequest(ctx, req)
	if autoDisp.Err != nil {
		return nil, autoDisp.Err
	}
	req = e.maybeCompress(req)
	chain := withResolvedEffort(e.chainForDispatch(req.Model, autoDisp.Ordered), intentEffort)
	// Modality bridges (B3-24): reroute an image-bearing request
	// off a confirmed non-vision head before the effort policy
	// stamps the chain, so the policy sees the bridged chain.
	chain, req = e.maybeBridgeModality(chain, req, autoDisp.Ordered, intentEffort)
	chain, req = e.withForcedEffort(chain, req, requestedModel, clientEffort)
	return e.chatStreamDrainChunks(ctx, chain, req, skipChars, onChunk, reqStrategy, autoDisp.OrderedExplicit)
}

// chatStreamDrain drives a chain-based stream and forwards its chunks
// to onChunk, skipping the first skipChars text runes when resuming.
// It returns the reassembled response (full content, completed tool
// calls, finish reason — even when resuming).
func (e *Engine) chatStreamDrainChunks(ctx context.Context, chain []Attempt, req provider.ChatRequest, skipChars int, onChunk func(provider.StreamChunk) error, reqStrategy string, dispatchExplicitOrder bool) (*provider.ChatResponse, error) {
	if onChunk == nil {
		onChunk = func(provider.StreamChunk) error { return nil }
	}
	if skipChars < 0 {
		skipChars = 0
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := e.chatStreamWithChainStrategy(ctx, chain, req, reqStrategy, dispatchExplicitOrder)
	if err != nil {
		return nil, err
	}
	var (
		full         strings.Builder
		toolCalls    []provider.ToolCall
		finishReason string
		// doneResp/doneErr hold the terminal result. The drain
		// does NOT return on the terminal event: runStream
		// publishes the route trace in a defer that runs before
		// it closes the channel, so returning on Done/Err would
		// race LastTrace (and GET /dashboard/trace) for the
		// caller — a successful stream's trace could read as
		// the previous request's, or nil. Drain to close first;
		// runStream sends nothing after the terminal event, so
		// this waits only for its in-memory bookkeeping.
		doneResp *provider.ChatResponse
		doneErr  error
	)
	skipped := 0
	// delivered names the tool calls the client already
	// received on an earlier stream of this request (see
	// DeliveredToolCallIDs): their deltas are suppressed below
	// so a resumed stream never re-delivers them.
	delivered := deliveredToolCallSet(req)
	for {
		select {
		case <-ctx.Done():
			// The stream already terminated: hand back its
			// result instead of the cancellation.
			if doneResp != nil {
				return doneResp, nil
			}
			if doneErr != nil {
				return nil, doneErr
			}
			// Neither terminal arrived yet: runStream is still
			// unwinding — it publishes LastTrace in a defer
			// that runs before it closes the channel, and
			// records the attempt (trace + ledger) before that.
			// Drain to close so the trace and the terminal
			// error are recorded before this returns: the
			// caller reads LastTrace right after (and the
			// server attributes the request from it), and a
			// nil/stale trace here would misattribute the
			// request — an async guardrail cut would read as an
			// "unknown" provider instead of the cut attempt
			// (CHECKLIST 185). The channel always closes after
			// a cancel (every send in runStream watches ctx),
			// so this cannot hang on a well-behaved engine.
			// Content arriving during this drain is discarded
			// — the caller is gone (or the stream was cut) —
			// but a terminal event is kept, so the caller gets
			// the real terminal error instead of a bare
			// cancellation.
			for ev := range ch {
				if ev.Delta != "" {
					full.WriteString(ev.Delta)
				}
				if len(ev.ToolCalls) > 0 {
					toolCalls = provider.MergeToolCallDeltas(toolCalls, ev.ToolCalls)
				}
				if ev.FinishReason != "" {
					finishReason = ev.FinishReason
				}
				if ev.Err != nil {
					doneErr = ev.Err
					continue
				}
				if ev.Done {
					// M6.3: a text-only client's finish reason lives
					// only in Resp (the bridge emits no per-chunk
					// reason), so fall back to it — otherwise a
					// truncated ("length") stream would reassemble
					// with FinishReason "" and pass the store gate.
					if finishReason == "" && ev.Resp != nil {
						finishReason = ev.Resp.FinishReason
					}
					doneResp = &provider.ChatResponse{Content: full.String(), Model: req.Model, ToolCalls: toolCalls, FinishReason: finishReason}
					if ev.Resp != nil {
						doneResp.Reasoning = ev.Resp.Reasoning
						// G1: carry the upstream-reported streaming
						// usage (and any reported cost inside it) to
						// the caller — the server projects it into
						// the client's usage chunk / telemetry.
						doneResp.Usage = ev.Resp.Usage
					}
				}
			}
			if doneResp != nil {
				return doneResp, nil
			}
			if doneErr != nil {
				return nil, doneErr
			}
			return nil, ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				if doneResp != nil {
					return doneResp, nil
				}
				if doneErr != nil {
					return nil, doneErr
				}
				return nil, errors.New("router: stream closed without Done")
			}
			if doneResp != nil || doneErr != nil {
				// Terminal event already received; nothing
				// more can arrive — keep draining to close.
				continue
			}
			if ev.Err != nil {
				doneErr = ev.Err
				continue
			}
			if ev.Delta != "" {
				full.WriteString(ev.Delta)
			}
			if len(ev.ToolCalls) > 0 {
				toolCalls = provider.MergeToolCallDeltas(toolCalls, ev.ToolCalls)
			}
			if ev.FinishReason != "" {
				finishReason = ev.FinishReason
			}
			if ev.Done {
				// M6.3: see the ctx-cancel loop above — the
				// text-only fallback carries the finish reason
				// only in Resp.FinishReason.
				if finishReason == "" && ev.Resp != nil {
					finishReason = ev.Resp.FinishReason
				}
				doneResp = &provider.ChatResponse{Content: full.String(), Model: req.Model, ToolCalls: toolCalls, FinishReason: finishReason}
				if ev.Resp != nil {
					doneResp.Reasoning = ev.Resp.Reasoning
					// G1: upstream-reported streaming usage rides
					// to the caller (see the ctx-cancel loop).
					doneResp.Usage = ev.Resp.Usage
				}
				continue
			}
			if ev.Delta == "" && len(ev.ToolCalls) == 0 && ev.FinishReason == "" {
				continue
			}
			chunk := provider.StreamChunk{
				Content:      ev.Delta,
				ToolCalls:    ev.ToolCalls,
				FinishReason: ev.FinishReason,
			}
			if skipped < skipChars {
				runes := []rune(chunk.Content)
				drop := skipChars - skipped
				if drop > len(runes) {
					drop = len(runes)
				}
				skipped += drop
				chunk.Content = string(runes[drop:])
				if chunk.Content == "" {
					// Still skipping: suppress tool-call deltas
					// too, so a replayed prefix cannot duplicate
					// an argument fragment on the wire. The
					// reassembled response above still holds them.
					chunk.ToolCalls = nil
					chunk.FinishReason = ""
				}
				if chunk.Content == "" && len(chunk.ToolCalls) == 0 {
					continue
				}
			}
			// No replay of delivered tool calls (CHECKLIST
			// 243, LiteLLM #51683): a resumed stream replays
			// the turn from the start, so a tool call the
			// client already received arrives again with the
			// same id — drop those deltas for the whole
			// stream, not just while the text prefix is being
			// skipped (a call's deltas arrive in their own
			// chunks, after the text). A call the client has
			// NOT seen passes through, fragments included.
			// The reassembled response above still holds the
			// dropped calls, so the returned response is
			// complete; only the wire delivery is deduped.
			if len(delivered) > 0 && len(chunk.ToolCalls) > 0 {
				before := len(chunk.ToolCalls)
				var dropped bool
				chunk, dropped = filterDeliveredToolCalls(chunk, delivered)
				if dropped {
					e.noteToolReplaysSuppressed(before - len(chunk.ToolCalls))
				}
				if chunk.Content == "" && len(chunk.ToolCalls) == 0 && chunk.FinishReason == "" {
					continue
				}
			}
			if err := onChunk(chunk); err != nil {
				return nil, err
			}
		}
	}
}

// chatStreamDrain drives a chain-based stream and forwards its text
// deltas to onDelta, skipping the first skipChars runes when resuming.
// It returns the reassembled response (full content, completed tool
// calls, finish reason — even when resuming).
func (e *Engine) chatStreamDrain(ctx context.Context, chain []Attempt, req provider.ChatRequest, skipChars int, onDelta func(string) error, reqStrategy string, dispatchExplicitOrder bool) (*provider.ChatResponse, error) {
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	return e.chatStreamDrainChunks(ctx, chain, req, skipChars, func(chunk provider.StreamChunk) error {
		if chunk.Content == "" {
			return nil
		}
		return onDelta(chunk.Content)
	}, reqStrategy, dispatchExplicitOrder)
}
