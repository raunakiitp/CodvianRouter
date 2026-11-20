// Decision transparency on every gateway response (CHECKLIST 199):
// every response the router serves from a provider carries
// X-Codvian-Decision naming the provider + model that served it, the
// strategy that ordered its chain, and the reason that provider was
// chosen (see internal/router/decision.go for the reason vocabulary
// and precedence). The value is a semicolon-separated key=value list
// a client can parse without knowing the router's internals:
//
//	X-Codvian-Decision: provider=prov-a; model=gpt-4o; strategy=cheapest; reason=fallback-after-2
//
// When the serving attempt reached its target by a region-aware
// failover step (CHECKLIST 257-258), the header carries it as a
// trailing key — `; region_step=cross-region` (same model, another
// region), `; region_step=equivalent-model`, or
// `; region_step=self-hosted` — so a client can tell a cross-region
// failover from a plain provider failover without fetching the
// trace. Absent when the first target served: no step to name.
//
// The header is set from the engine's LastRouting record — the same
// record the strategies dashboard feed reads — so the header and the
// dashboard can never disagree about the last request.
//
// When the header is ABSENT, that is itself the honest answer, and
// each case has a documented reason (see docs/decision-header.md):
//
//   - the response was served from a cache (X-Codvian-Cache names the
//     layer instead — no provider was called, so there is no provider
//     to name);
//   - the response was served stale after every provider failed
//     (X-Codvian-Stale names it instead — no provider served THIS
//     response);
//   - the request failed before any provider served it (contract
//     rejection, budget stop, guardrail block, exhausted chain) —
//     there is no serving provider, and inventing one would be a lie;
//   - the request bypassed routing entirely (the /v1/messages
//     verbatim relay, which forwards to one fixed provider —
//     X-Codvian-Relay names it instead);
//   - the serving provider's record could not be matched to this
//     request (model mismatch — the header is withheld rather than
//     guessed).
//
// The header is never set with a guessed or placeholder value: every
// guard below fails closed to "absent".
package server

import (
	"net/http"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// decisionHeaderValue formats one LastRouting record as the
// X-Codvian-Decision value. ok=false means "do not send the header":
// no serving provider, no strategy, or no model was recorded for this
// request. Values are sanitised (any ';', '=' or control character
// becomes '_'): provider IDs and model IDs never contain them, but a
// hostile operator-configured name must not be able to smuggle a
// second key=value pair into the header.
func decisionHeaderValue(info router.RoutingInfo) (string, bool) {
	if info.Provider == "" || info.Strategy == "" || info.Model == "" {
		return "", false
	}
	reason := info.Reason
	if reason == "" {
		// An engine that records a provider without a reason
		// (an older wrapper, or a test fake): the strategy
		// itself is the reason — the documented fallback rule,
		// never an invented narrower claim.
		reason = router.StrategyReason(info.Strategy)
	}
	if reason == "" {
		return "", false
	}
	san := func(v string) string {
		return strings.Map(func(r rune) rune {
			if r == ';' || r == '=' || r < ' ' || r == 0x7f {
				return '_'
			}
			return r
		}, v)
	}
	return "provider=" + san(info.Provider) + "; model=" + san(info.Model) +
		"; strategy=" + san(info.Strategy) + "; reason=" + san(reason) +
		regionStepSuffix(info.RegionStep), true
}

// regionStepSuffix renders the region-aware failover step of the
// serving attempt (CHECKLIST 257-258) as the decision header's
// trailing key: "; region_step=cross-region" (same model, another
// region), "; region_step=equivalent-model" or
// "; region_step=self-hosted". "" when the first target served —
// no failover step to name, and the header stays exactly as it
// was before this slice, so existing header assertions keep
// passing byte-for-byte.
func regionStepSuffix(step string) string {
	switch step {
	case router.StepCrossRegion, router.StepEquivalentModel, router.StepSelfHosted:
		return "; region_step=" + step
	}
	return ""
}

// decisionSuppressed reports whether this request's response was NOT
// served by a provider: a proactive cache hit, or a stale serve after
// every provider failed. Both are reported by the degrade wrapper
// for the most recent call, and both are published before the
// response is delivered — before the server sets its headers on the
// non-streaming path, and before the first chunk on the streaming
// path (CHECKLIST 199's degrade ordering fix). When either is true
// the decision header stays absent: X-Codvian-Cache / X-Codvian-Stale
// already names what served the response, and the engine's
// LastRouting still holds a previous request's record, which must
// never be presented as this request's decision.
func (s *Server) decisionSuppressed() bool {
	if chr, ok := s.engine.(cacheHitReporter); ok {
		if _, _, hit := chr.LastCacheHit(); hit {
			return true
		}
	}
	if sr, ok := s.engine.(staleReporter); ok && sr.LastServedStale() {
		return true
	}
	return false
}

// lastDecision reads the engine's most recent routing record, or
// false when the engine exposes none.
func (s *Server) lastDecision() (router.RoutingInfo, bool) {
	rep, ok := s.engine.(strategyReporter)
	if !ok {
		return router.RoutingInfo{}, false
	}
	return rep.LastRouting()
}

// setDecisionHeader marks a non-streaming response with the routing
// decision that served it. models are the model names this response
// may legitimately name: the model the client asked for and the
// model the upstream response echoes (they differ when the request
// named a virtual model — "auto/cheap" resolves to a concrete model
// the upstream then echoes — or when an upstream echoes a fuller
// model id than requested). The header is sent only when the
// engine's record names one of them: a record naming another model
// belongs to a different (concurrent or earlier) request, and is
// withheld rather than guessed. Call after the response is known to
// be a fresh provider response — the cache/stale guard inside
// handles the served-from-cache cases.
func (s *Server) setDecisionHeader(w http.ResponseWriter, models ...string) {
	if s.decisionSuppressed() {
		return
	}
	info, ok := s.lastDecision()
	if !ok {
		return
	}
	matched := false
	for _, m := range models {
		if m != "" && info.Model == m {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	if v, ok := decisionHeaderValue(info); ok {
		w.Header().Set("X-Codvian-Decision", v)
	}
}

// setStreamDecisionHeader marks a streaming response with the routing
// decision that serves it. It must be called on the first chunk,
// before the first SSE write commits the headers. Unlike the
// non-streaming setter it does NOT require the engine's model to
// match the requested model string: the engine publishes the
// decision at stream commit, immediately before the first event is
// sent (see internal/router runStream's onCommit), so the record
// read here is this request's by channel ordering — and for an
// auto/* request the requested string is the virtual model while the
// record names the resolved concrete model, which is exactly what
// the header should carry. The cache/stale guard still applies: a
// cache-replayed stream has no provider decision to name.
func (s *Server) setStreamDecisionHeader(w http.ResponseWriter, reqModel string) {
	if s.decisionSuppressed() {
		return
	}
	info, ok := s.lastDecision()
	if !ok {
		return
	}
	// A record naming a different concrete model than requested
	// belongs to another request — except for virtual models,
	// where the engine resolves before recording.
	if info.Model != reqModel && !autorouter.IsVirtualModel(reqModel) {
		return
	}
	if v, ok := decisionHeaderValue(info); ok {
		w.Header().Set("X-Codvian-Decision", v)
	}
}
