// Per-key traffic limits (CHECKLIST 195): operator-set
// requests-per-minute + tokens-per-minute per gateway key, enforced
// pre-call with an honest 429 + Retry-After when exceeded.
//
// The limiter is in-memory and per key: a sliding 60-second window
// of admitted requests, each entry carrying the request's locally
// estimated prompt tokens. It is deliberately separate from the
// ledger: the ledger records attempts asynchronously (and only
// after a response exists), so a ledger-backed check would let a
// burst through before the first row landed. The trade-off is
// honest and stated wherever the numbers surface: counters live
// since server start and reset on restart, and TPM counts the
// locally estimated PROMPT tokens at admission only — completion
// tokens are unknown before the call, so they are never counted.
// A request refused by an earlier gate (guardrails, budgets)
// never reaches the limiter and never consumes a slot; a request
// admitted here counts even if the upstream later fails (it used
// a slot).
//
// A 429 from this limiter is distinct from an upstream 429: it
// carries the Retry-After header (seconds until the oldest entry
// in the window ages out), the error type "key_rate_limit", and a
// message naming the limit and the key's configured value — and it
// is raised before any provider is dialled, so zero provider
// calls happen for it.
package server

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// trafficWindow is the rolling window for per-key traffic limits.
const trafficWindow = 60 * time.Second

// trafficEntry is one admitted request in a key's window: when it
// was admitted, and how many locally estimated prompt tokens it
// carried (0 when the prompt could not be counted — relay and
// embeddings/rerank paths — so those requests consume a request
// slot but no token budget).
type trafficEntry struct {
	at     time.Time
	tokens int
}

// keyTrafficLimiter holds one sliding window per gateway key.
// The zero value is unusable: use newKeyTrafficLimiter.
type keyTrafficLimiter struct {
	mu      sync.Mutex
	windows map[string][]trafficEntry
}

func newKeyTrafficLimiter() *keyTrafficLimiter {
	return &keyTrafficLimiter{windows: map[string][]trafficEntry{}}
}

// trafficLimits is one key's full traffic-limit set (M5.2): the
// 60-second RPM/TPM axes, the UTC-calendar-day RPD axis, and the
// generic sliding-window rules. Zero on any axis means unlimited.
type trafficLimits struct {
	RPM   int
	TPM   int
	RPD   int
	Rules []config.RateLimitRule
}

// trafficLimitsFor reads a gateway key's limits into the limiter's
// shape.
func trafficLimitsFor(key config.GatewayKey) trafficLimits {
	return trafficLimits{
		RPM:   key.RequestsPerMinute,
		TPM:   key.TokensPerMinute,
		RPD:   key.RequestsPerDay,
		Rules: key.RateLimits,
	}
}

// trafficRefusal names the limit a refusal came from. RuleLimit /
// RuleWindowSeconds are set only for rate_limit_rule refusals, so
// the 429 message can name the rule that fired.
type trafficRefusal struct {
	Limit             string
	RuleLimit         int
	RuleWindowSeconds int
}

// maxWindowLocked is the longest window any active limit needs:
// entries older than this can never affect a future decision, so
// pruning drops them. 60 s is the baseline (RPM/TPM/usage); RPD
// needs 24 h; a generic rule needs its own window.
func (l *keyTrafficLimiter) maxWindowLocked(lim trafficLimits) time.Duration {
	max := trafficWindow
	if lim.RPD > 0 && 24*time.Hour > max {
		max = 24 * time.Hour
	}
	for _, r := range lim.Rules {
		if w := time.Duration(r.WindowSeconds) * time.Second; w > max {
			max = w
		}
	}
	return max
}

// pruneLocked drops entries older than window from one key's slice.
// Caller must hold mu.
func (l *keyTrafficLimiter) pruneLocked(keyID string, now time.Time, window time.Duration) {
	entries := l.windows[keyID]
	cutoff := now.Add(-window)
	keep := 0
	for keep < len(entries) && !entries[keep].at.After(cutoff) {
		keep++
	}
	if keep > 0 {
		l.windows[keyID] = append([]trafficEntry(nil), entries[keep:]...)
	}
}

// retryAfterForWindowLocked is the honest wait until the oldest
// entry in a window of this size ages out, rounded up to whole
// seconds, at least 1. Caller must hold mu and the window must be
// non-empty (an empty window means the caller checked wrong).
func (l *keyTrafficLimiter) retryAfterForWindowLocked(keyID string, now time.Time, window time.Duration) time.Duration {
	entries := l.windows[keyID]
	if len(entries) == 0 {
		return time.Second
	}
	wait := entries[0].at.Add(window).Sub(now)
	if wait < time.Second {
		return time.Second
	}
	return time.Duration((wait + time.Second - 1) / time.Second * time.Second)
}

// countSinceLocked counts entries at or after start. Caller must
// hold mu.
func (l *keyTrafficLimiter) countSinceLocked(keyID string, start time.Time) int {
	n := 0
	for _, e := range l.windows[keyID] {
		if !e.at.Before(start) {
			n++
		}
	}
	return n
}

// countInWindowLocked counts entries inside a sliding window of
// this size ending at now. Caller must hold mu.
func (l *keyTrafficLimiter) countInWindowLocked(keyID string, now time.Time, window time.Duration) int {
	return l.countSinceLocked(keyID, now.Add(-window))
}

// tokensInWindowLocked sums prompt tokens inside a sliding window
// of this size ending at now. Caller must hold mu.
func (l *keyTrafficLimiter) tokensInWindowLocked(keyID string, now time.Time, window time.Duration) int {
	used := 0
	cutoff := now.Add(-window)
	for _, e := range l.windows[keyID] {
		if e.at.After(cutoff) {
			used += e.tokens
		}
	}
	return used
}

// checkLimitsLocked is the shared refusal logic for tryAdmit (which
// records) and wouldRefuse (which only peeks): RPM, then (when
// countTokens) TPM, then RPD, then the generic rules, in that
// order — the first hit wins and names itself. countTokens=false
// skips the token axis (the peek has no prompt to count — the
// token axis stays post-body-only, honestly).
func (l *keyTrafficLimiter) checkLimitsLocked(keyID string, tokens int, lim trafficLimits, now time.Time, countTokens bool) (refused bool, retryAfter time.Duration, refusal trafficRefusal) {
	entries := l.windows[keyID]
	if lim.RPM > 0 && len(entries) >= lim.RPM {
		return true, l.retryAfterForWindowLocked(keyID, now, trafficWindow), trafficRefusal{Limit: "requests_per_minute"}
	}
	if countTokens && lim.TPM > 0 {
		if l.tokensInWindowLocked(keyID, now, trafficWindow)+tokens > lim.TPM {
			return true, l.retryAfterForWindowLocked(keyID, now, trafficWindow), trafficRefusal{Limit: "tokens_per_minute"}
		}
	}
	if lim.RPD > 0 {
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		if l.countSinceLocked(keyID, dayStart) >= lim.RPD {
			nextMidnight := dayStart.Add(24 * time.Hour)
			wait := nextMidnight.Sub(now)
			if wait < time.Second {
				wait = time.Second
			}
			return true, time.Duration((wait + time.Second - 1) / time.Second * time.Second), trafficRefusal{Limit: "requests_per_day"}
		}
	}
	for _, r := range lim.Rules {
		w := time.Duration(r.WindowSeconds) * time.Second
		if l.countInWindowLocked(keyID, now, w) >= r.Limit {
			return true, l.retryAfterForWindowLocked(keyID, now, w), trafficRefusal{Limit: "rate_limit_rule", RuleLimit: r.Limit, RuleWindowSeconds: r.WindowSeconds}
		}
	}
	return false, 0, trafficRefusal{}
}

// tryAdmit checks keyID against its limits and, when admitted,
// records the request in the window. Limits of 0 mean unlimited
// on that axis — the request is still recorded, so
// /v1/me/status can report live usage for an unlimited key too.
// It returns ok=false with the retry-after and the refusal (which
// names the limit) when a limit is hit; the caller writes the 429.
func (l *keyTrafficLimiter) tryAdmit(keyID string, tokens int, lim trafficLimits, now time.Time) (ok bool, retryAfter time.Duration, refusal trafficRefusal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(keyID, now, l.maxWindowLocked(lim))
	if refused, wait, r := l.checkLimitsLocked(keyID, tokens, lim, now, true); refused {
		return false, wait, r
	}
	l.windows[keyID] = append(l.windows[keyID], trafficEntry{at: now, tokens: tokens})
	return true, 0, trafficRefusal{}
}

// wouldRefuse is the read-only half of tryAdmit (CHECKLIST
// 243): it reports whether keyID is already at a limit — the
// requests-per-minute ceiling, the requests-per-day ceiling, or
// any generic rule — WITHOUT recording a slot. The pre-body
// admission peek uses it so the post-body check still records the
// one slot for the request. The token axis is not peeked: without
// the prompt there is nothing to count against it, and a 0-token
// peek would wave through a key that is over its token ceiling —
// so the token axis stays post-body-only, honestly.
func (l *keyTrafficLimiter) wouldRefuse(keyID string, lim trafficLimits, now time.Time) (refused bool, retryAfter time.Duration, refusal trafficRefusal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(keyID, now, l.maxWindowLocked(lim))
	return l.checkLimitsLocked(keyID, 0, lim, now, false)
}

// trueUp corrects the newest entry for keyID whose recorded token
// count equals estimate to actual — the post-response correction
// of the TPM admission estimate (M5.3). It returns false when no
// matching entry is still in the window (aged out, or the request
// was never admitted through this limiter). With concurrent
// same-estimate requests each true-up corrects a different entry
// (newest first), so the window sum stays right whichever entry
// a given response corrects — the limiter only ever sums.
func (l *keyTrafficLimiter) trueUp(keyID string, estimate, actual int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(keyID, now, trafficWindow)
	entries := l.windows[keyID]
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].tokens == estimate {
			if actual < 0 {
				actual = 0
			}
			entries[i].tokens = actual
			return true
		}
	}
	return false
}

// usage reports one key's live window usage: admitted requests and
// prompt tokens in the last 60 seconds. Zero when the key has sent
// nothing (or the server restarted — counters are in-memory).
func (l *keyTrafficLimiter) usage(keyID string, now time.Time) (requests, tokens int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(keyID, now, trafficWindow)
	for _, e := range l.windows[keyID] {
		requests++
		tokens += e.tokens
	}
	return requests, tokens
}

// usageToday reports one key's admitted requests since UTC
// midnight (the RPD window), for /v1/me/status.
func (l *keyTrafficLimiter) usageToday(keyID string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(keyID, now, 24*time.Hour)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return l.countSinceLocked(keyID, dayStart)
}

// estimatePromptTokens counts a chat request's prompt locally with
// the same estimator the budget gates use
// (tokencount.CountPromptFull: message text + tool definitions +
// reference framing, CHECKLIST 229-230 — a message-text-only count
// undercounts a tool-heavy prompt by up to 80%). A counting
// failure returns 0: the request proceeds and consumes a request
// slot but no token budget — it is never refused on a guess.
func estimatePromptTokens(req *provider.ChatRequest) int {
	if req == nil {
		return 0
	}
	count, err := tokencount.CountPromptFull(req.Model, *req)
	if err != nil {
		return 0
	}
	return count.Tokens
}

// trafficRefusalMessage renders the 429 message for a refusal: it
// names the limit and the key's configured value.
func trafficRefusalMessage(key config.GatewayKey, refusal trafficRefusal, seconds int) string {
	switch refusal.Limit {
	case "requests_per_minute":
		return fmt.Sprintf("gateway key traffic limit exceeded: requests per minute limit of %d reached; retry after %d second(s)", key.RequestsPerMinute, seconds)
	case "tokens_per_minute":
		return fmt.Sprintf("gateway key traffic limit exceeded: tokens per minute limit of %d reached; retry after %d second(s)", key.TokensPerMinute, seconds)
	case "requests_per_day":
		return fmt.Sprintf("gateway key traffic limit exceeded: requests per day limit of %d reached; retry after %d second(s)", key.RequestsPerDay, seconds)
	case "rate_limit_rule":
		return fmt.Sprintf("gateway key traffic limit exceeded: rate limit rule (max %d requests per %d second(s)) reached; retry after %d second(s)", refusal.RuleLimit, refusal.RuleWindowSeconds, seconds)
	default:
		return fmt.Sprintf("gateway key traffic limit exceeded; retry after %d second(s)", seconds)
	}
}

// checkKeyTraffic enforces the caller's per-key traffic limits
// (CHECKLIST 195) for a request carrying promptTokens locally
// estimated prompt tokens. It writes the 429 + Retry-After and
// returns true when the request must stop here, before any
// provider is called. It is skipped entirely when the request
// arrived without a verifiable key (auth-relaxed mode: there is
// no key to limit), when no config is loaded (no record to read
// limits from), or when the key is not in the config (a static
// verifier key has no operator-set limits).
//
// G15 Area 4: this is also the per-key policy + device choke
// point. checkKeyTrafficModel runs the IP / endpoint / model
// policy (checkKeyPolicy) BEFORE the limiter admits the request,
// so a refused request consumes no traffic slot — and records
// the caller's device (keydevices.go) only once the request is
// admitted. Coverage: every /v1 path that calls this gate (chat,
// embeddings, audio, images, responses, relay, A2A). Bare reads
// that never reach the gate (e.g. GET /v1/models) are policy
// checked in the auth funnel itself — keyPolicyAllows (server.go,
// G21/B9 VerifyKey hook) enforces the IP / endpoint policy on
// every authenticated request — so this gate's IP / endpoint check
// is the second, model-aware pass for spend paths.
func (s *Server) checkKeyTraffic(w http.ResponseWriter, r *http.Request, promptTokens int) bool {
	return s.checkKeyTrafficModel(w, r, promptTokens, "")
}

// checkKeyTrafficModel is checkKeyTraffic for a request naming a
// model: the model / forced-model policy is enforced alongside
// the IP and endpoint policy, before admission.
func (s *Server) checkKeyTrafficModel(w http.ResponseWriter, r *http.Request, promptTokens int, model string) bool {
	keyID := s.callerKeyID(r)
	if keyID == "" || s.cfg == nil {
		return false
	}
	key, ok := s.cfg.FindGatewayKey(keyID)
	if !ok {
		return false
	}
	if s.checkKeyPolicy(w, r, key, model) {
		return true
	}
	limiter := s.trafficLimiter()
	admitted, retryAfter, refusal := limiter.tryAdmit(keyID, promptTokens, trafficLimitsFor(key), time.Now())
	if admitted {
		s.recordKeyDevice(r)
		return false
	}
	seconds := int(retryAfter / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	// The error type names the limiter, not the upstream: a client
	// can tell this 429 (raised pre-call, zero provider calls)
	// from an upstream provider's 429 without parsing the string.
	writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Message: trafficRefusalMessage(key, refusal, seconds), Type: "key_rate_limit"}})
	return true
}

// trueUpKeyTraffic corrects this request's TPM slot from the
// locally estimated prompt tokens to the provider-reported
// actuals (M5.3). Called only on a served non-streaming response:
// without provider-reported usage there is nothing to correct to
// (the estimate stands, honestly), and a failed request consumed
// its estimated prompt — the estimate is the best record of it.
// In MCP loop mode the correction uses the FINAL call's reported
// prompt tokens, not the loop total; the windowed token limits
// (ledger-based) are the authoritative budget there.
func (s *Server) trueUpKeyTraffic(r *http.Request, req *provider.ChatRequest, resp *provider.ChatResponse) {
	keyID := s.callerKeyID(r)
	if keyID == "" || resp == nil || resp.Usage.PromptTokens <= 0 {
		return
	}
	s.trafficLimiter().trueUp(keyID, estimatePromptTokens(req), resp.Usage.PromptTokens, time.Now())
}

// checkKeyTrafficForChat is checkKeyTraffic for a chat request:
// the prompt tokens are estimated from the request itself, and
// the named model is policy-checked (allowlist / denylist /
// forced-model) before admission.
func (s *Server) checkKeyTrafficForChat(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest) bool {
	model := ""
	if req != nil {
		model = req.Model
	}
	return s.checkKeyTrafficModel(w, r, estimatePromptTokens(req), model)
}

// checkKeyTrafficRequestsOnly is checkKeyTraffic for a path whose
// prompt tokens are not countable before the call (the native
// relay: the body is forwarded verbatim, so no local estimate
// exists). The request slot is still enforced; the token axis is
// not — the relay's spend is invisible to the ledger the same way
// (see the standing relay gap in relay.go).
func (s *Server) checkKeyTrafficRequestsOnly(w http.ResponseWriter, r *http.Request) bool {
	return s.checkKeyTraffic(w, r, 0)
}

// trafficLimiter returns the server's limiter, never nil. New sets
// it; the locked backstop covers a hand-built Server (tests) so a
// nil limiter can never panic a request path.
func (s *Server) trafficLimiter() *keyTrafficLimiter {
	if s.traffic != nil {
		return s.traffic
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.traffic == nil {
		s.traffic = newKeyTrafficLimiter()
	}
	return s.traffic
}

// keyTrafficUsage reports one key's live window usage for
// /v1/me/status: admitted requests and prompt tokens in the last
// 60 seconds. Zero when nothing was sent (or after a restart).
func (s *Server) keyTrafficUsage(keyID string) (requests, tokens int) {
	return s.trafficLimiter().usage(keyID, time.Now())
}
