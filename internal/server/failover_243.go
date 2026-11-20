package server

// LiteLLM v1.111.0-rc.1 failover cluster, server half (CHECKLIST
// 243-244, research-log entries 222-224): admission before I/O
// (g), per-turn token admission for Responses sessions (f), the
// no-replay strip's counters (b), and naming the deployment in
// stream error alerts (h). The engine half (pin escape, resume
// no-replay, same-group retry, no-recursion) lives in
// internal/router/failover_243.go.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// WithResponsesSessionTokenCap sets the per-session token cap for
// Responses continuations (CHECKLIST 243, LiteLLM #50730). A
// session whose counted tokens plus the next turn's prompt
// estimate would exceed it refuses the turn with 429 before any
// provider is dialled. 0 (the default) means no cap: turns are
// counted and reported, never refused. A negative value is
// treated as 0.
func WithResponsesSessionTokenCap(n int) Option {
	return func(s *Server) {
		if n < 0 {
			n = 0
		}
		s.responsesSessionTokenCap = n
	}
}

// ---------------------------------------------------------------------------
// Counters (CHECKLIST 244: real counts for the dashboard feeds).
// ---------------------------------------------------------------------------

func (s *Server) noteEarlyRefusal() { s.fail243EarlyRefusals.Add(1) }
func (s *Server) noteStrippedToolCalls(n int) {
	if n > 0 {
		s.fail243StrippedToolCalls.Add(uint64(n))
	}
}
func (s *Server) noteSessionAdmissionRefused() { s.fail243SessionAdmissionRefused.Add(1) }
func (s *Server) noteStreamErrorNamed()        { s.fail243StreamErrorsNamed.Add(1) }

// ---------------------------------------------------------------------------
// (g) Admission before I/O: refuse blocked and over-limit keys
// before reading the request body.
// ---------------------------------------------------------------------------

// preBodyAdmission refuses a request before its body is read
// when the caller's key is already over a limit — the key-budget
// cap (model unknown: the cap check only, no unpriced check) or
// the requests-per-minute ceiling (peeked, never recorded). It
// writes the refusal (402 / 429, the same shapes the post-body
// gates write) and returns true when the handler must stop. It
// is a no-op without a verifiable key (auth-relaxed mode: there
// is no key to limit), without a ledger (no budget to check), and
// without config (no traffic limits to peek). The body-dependent
// checks still run after decode — the token axis of the traffic
// limiter needs the prompt, and the unpriced-model check needs
// the model — so a request admitted here is re-checked in full
// below; admission here only ever refuses early, never admits
// finally. "Blocked" keys need no separate check: an unknown,
// revoked, or expired key never reaches a handler — requireAuth
// refuses it (401, naming expiry) before the handler runs, and
// therefore before any body is read.
func (s *Server) preBodyAdmission(w http.ResponseWriter, r *http.Request) bool {
	if s.callerKeyID(r) == "" {
		return false
	}
	if s.checkKeyBudgetForModel(w, r, "") {
		s.noteEarlyRefusal()
		return true
	}
	if s.checkKeyTrafficPeek(w, r) {
		s.noteEarlyRefusal()
		return true
	}
	return false
}

// checkKeyTrafficPeek is the pre-body half of checkKeyTraffic: it
// refuses (429 + Retry-After, the same shape) when the caller's
// key is already at a request-count ceiling — requests per minute,
// requests per day, or any generic rule — WITHOUT recording a
// slot — the post-body check still runs after decode and records
// the one slot for the request. The token axis is not peeked:
// without the prompt there is nothing to count against it, and a
// 0-token peek would wave through a key that is over its token
// ceiling — so the token axis stays post-body-only, honestly.
// Skipped entirely when there is no verifiable key, no config, or
// the key is not in the config (a static verifier key has no
// operator-set limits).
func (s *Server) checkKeyTrafficPeek(w http.ResponseWriter, r *http.Request) bool {
	keyID := s.callerKeyID(r)
	if keyID == "" || s.cfg == nil {
		return false
	}
	key, ok := s.cfg.FindGatewayKey(keyID)
	if !ok {
		return false
	}
	limiter := s.trafficLimiter()
	refused, retryAfter, refusal := limiter.wouldRefuse(keyID, trafficLimitsFor(key), time.Now())
	if !refused {
		return false
	}
	seconds := int(retryAfter / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Message: trafficRefusalMessage(key, refusal, seconds), Type: "key_rate_limit"}})
	return true
}

// ---------------------------------------------------------------------------
// (f) Per-turn token admission for long-lived Responses sessions.
// ---------------------------------------------------------------------------

// checkSessionTokenAdmission refuses a Responses continuation
// turn whose tokens would exceed the configured session cap: the
// session's counted tokens so far (from the stored parent
// record's cumulative totals) plus this turn's prompt estimate.
// It writes a 429 naming the cap and the counted total, counts
// the refusal, and returns true when the handler must stop. A
// fresh create (no previous_response_id) starts a session and is
// never refused here; with no cap configured (0) or no responses
// store, every turn is admitted — counting still happens at
// store time, so the dashboard shows the session totals either
// way. The parent record is known to exist: handleResponses
// already 404'd an unknown previous_response_id before the
// gates run.
func (s *Server) checkSessionTokenAdmission(w http.ResponseWriter, r *http.Request, chat *provider.ChatRequest) bool {
	if s.responsesSessionTokenCap <= 0 || s.responses == nil || chat == nil {
		return false
	}
	if chat.PreviousResponseID == "" {
		return false
	}
	parent, ok := s.responses.Get(chat.PreviousResponseID)
	if !ok {
		return false
	}
	used := parent.SessionInputTokens + parent.SessionOutputTokens
	turnPrompt := estimatePromptTokens(chat)
	cap := s.responsesSessionTokenCap
	if used+turnPrompt <= cap {
		return false
	}
	s.noteSessionAdmissionRefused()
	msg := fmt.Sprintf("responses session token limit exceeded: session has used %d tokens and this turn needs about %d more, over the %d-token session cap", used, turnPrompt, cap)
	writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Message: msg, Type: "session_token_limit"}})
	return true
}

// ---------------------------------------------------------------------------
// (h) Name the deployment in stream error alerts.
// ---------------------------------------------------------------------------

// deploymentDisplayName resolves a trace/stats provider ID to the
// operator-configured display name when the provider lister knows
// it, so alerts and dashboard cards name the deployment the way
// the operator configured it ("flaky") instead of the registry ID
// ("prov_…") the engine's chain carries. Unknown IDs, a missing
// lister, or an empty name fall back to the ID unchanged — the
// alert never loses its deployment, it only gains a readable one.
func (s *Server) deploymentDisplayName(id string) string {
	if id == "" || s.dashboardProviders == nil {
		return id
	}
	for _, p := range s.dashboardProviders.DashboardProviders() {
		if p.ID == id && p.Name != "" {
			return p.Name
		}
	}
	return id
}

// lastStreamDeployment names the provider of the last attempt in
// the engine's route trace — the deployment that served (and
// failed) the stream. "" when the engine exposes no trace or no
// attempt ran: the alert then carries no deployment rather than
// a guessed one.
func (s *Server) lastStreamDeployment() string {
	tp, ok := s.engine.(traceProvider)
	if !ok || tp == nil {
		return ""
	}
	trace := tp.LastTrace()
	if trace == nil || len(trace.Attempts) == 0 {
		return ""
	}
	return s.deploymentDisplayName(trace.Attempts[len(trace.Attempts)-1].Provider)
}

// streamErrorDetail builds the SSE error payload for a failed
// stream (CHECKLIST 243, LiteLLM #51034): the generic upstream
// error plus the deployment that failed it, so the alert is
// actionable — an alert that does not name the deployment cannot
// be triaged. The deployment is counted when present. The
// provider name is an operator-configured identifier, never
// prompt or response text.
func (s *Server) streamErrorDetail() errorDetail {
	d := errorDetail{Message: "upstream provider error", Type: "server_error"}
	if dep := s.lastStreamDeployment(); dep != "" {
		d.Deployment = dep
		s.noteStreamErrorNamed()
	}
	return d
}
