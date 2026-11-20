// Native relay + readiness gate (CHECKLIST items 143-144,
// research-log entries 83/145/179).
//
// POST /v1/messages speaks the Anthropic Messages dialect. When the
// request's provider matches that dialect, the body is forwarded
// verbatim — only the top-level "model" field is rewritten — so
// cache_control and the Anthropic SSE event envelope pass through
// untouched. Translation runs only when a feature needs the canonical
// form (guardrails blocking, gateway cache serving, a multi-provider
// failover chain, forced reasoning); the translate path converts
// Anthropic -> canonical -> engine -> Anthropic, and reports what it
// dropped (cache_control markers) instead of hiding the loss.
//
// A gateway-provided local capability that is not ready answers 503
// with Retry-After until its assets are ready — on both /v1/messages
// and /v1/chat/completions — instead of failing silently or
// pretending the local runtime served the request.
//
// Async guardrails on this path (CHECKLIST 203): the verbatim relay
// runs the same async-mode external classifiers as /v1/chat/completions
// (internal/guardrails/async.go), started after the pre-call gates
// and the cache lookup so a cache hit never spends a classifier call.
// A BLOCK verdict cuts the relayed stream with the Anthropic-protocol
// terminal error event — see anthropicPolicyRefusalEvent for the exact
// frame: an `event: error` event, no [DONE] (Anthropic streams never
// carry [DONE]), no message_stop — or withholds a non-streaming
// response (or a stream whose upstream call had not committed yet)
// with the plain 403 / -32001 JSON refusal. The translate path cuts
// the same way through the engine's cancel cause, exactly like the
// chat-completions stream cut.
//
// Routing contract on this path (CHECKLIST 211): the canonical
// "routing" field is parsed by relay.ParseAnthropicDetailed and
// forwarded into the engine's translate path, so the contract is
// honoured on /v1/messages exactly as on /v1/chat/completions —
// same validation, same fail-closed 400s/402, same trace. A
// non-empty contract forces translation (relay.Features.
// RoutingPresent): the VERBATIM path stays contract-free BY DESIGN
// — it forwards the body untouched, so it cannot apply a contract,
// and a request carrying one never reaches it. A contract on a
// verbatim request is therefore impossible, never silently ignored;
// that is pinned by test. A malformed contract fails closed with a
// 400 naming the field before any provider is dialled, even for a
// request that would otherwise relay verbatim. The X-Codvian-Routing
// header form is rejected with a 400, exactly like on chat
// completions. A contracted request never consults the gateway
// cache (a cached entry holds content only, with no record of which
// provider produced it — serving it could hand back an answer from
// a provider the contract forbade), the same rule the engine's
// cache wrapper enforces on chat.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/readiness"
	"github.com/raunakiitp/codvian-router/internal/relay"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// relayTarget is the server-side view of provider.RelayTarget: the
// single provider serving a model, its native wire, its base URL and
// exactly the auth headers the catalog resolves for it.
type relayTarget struct {
	ProviderID    string
	ProviderName  string
	WireFormat    string
	BaseURL       string
	AuthHeaders   map[string]string
	UpstreamModel string
	Attempts      int
	IsLocal       bool
}

// relayResolver resolves the relay target for one model ID. The
// provider registry implements it (see providerRegistryAdapter);
// tests supply a fake.
type relayResolver interface {
	ResolveRelayTarget(ctx context.Context, model string) (*relayTarget, error)
}

// ResolveRelayTarget adapts *provider.Registry to relayResolver.
func (a providerRegistryAdapter) ResolveRelayTarget(ctx context.Context, model string) (*relayTarget, error) {
	if a.reg == nil {
		return nil, fmt.Errorf("no provider registry configured")
	}
	t, err := a.reg.RelayTargetForModel(model)
	if err != nil {
		return nil, err
	}
	return &relayTarget{
		ProviderID: t.ProviderID, ProviderName: t.ProviderName,
		WireFormat: t.WireFormat, BaseURL: t.BaseURL,
		AuthHeaders: t.AuthHeaders, UpstreamModel: t.UpstreamModel,
		Attempts: t.Attempts, IsLocal: t.IsLocal,
	}, nil
}

// WithRelayResolver sets the resolver POST /v1/messages (and the
// readiness gate) use to find a model's provider. WithProviderRegistry
// wires the live registry automatically; this option is for tests or
// for a resolver that is not the registry.
func WithRelayResolver(r relayResolver) Option {
	return func(s *Server) {
		if r != nil {
			s.relayResolver = r
		}
	}
}

// WithReadinessGate wires the local-capability readiness gate. With
// no gate, no model is readiness-gated (the gateway provides no
// local capability to be unready about) and /dashboard/readiness
// reports an empty list.
func WithReadinessGate(g *readiness.Gate) Option {
	return func(s *Server) { s.readinessGate = g }
}

// WithCacheServing tells the server whether the engine in front of
// it serves requests proactively from the gateway cache (production
// serve mode wraps the engine with ServeCache on). The relay decision
// needs it: a request that could be cache-served must go through
// the canonical form. It defaults to false; main.go sets it true in
// serve mode, matching the degrade policy it builds.
func WithCacheServing(enabled bool) Option {
	return func(s *Server) { s.cacheServing = enabled }
}

// WithCaches wires the gateway caches into the native relay (the
// same instances the engine wraps, so a key stored by either dialect
// hits for the other). The relay consults them in canonical form
// before deciding: a hit is served in the Anthropic shape, a miss
// relays verbatim — so the cache never forces a translation it
// cannot serve. Verbatim non-streaming responses are stored back
// under the same keys. Nil caches mean the relay never serves or
// stores entries; the engine path is unaffected.
//
// The caches are interfaces (cache.ExactStore / cache.SemanticStore,
// M6.2b), so serve mode passes the persistent wrappers and the
// relay's store path writes through to disk synchronously, exactly
// like the engine path. Pass a nil interface, never a typed nil
// pointer, for an absent layer.
func WithCaches(exact cache.ExactStore, semantic cache.SemanticStore) Option {
	// A typed nil — (*cache.ExactCache)(nil) etc. — inside the
	// interface is non-nil, so the relay's nil checks would pass
	// and the first cache call would panic on the nil receiver.
	// Normalize the shipped implementations to a nil interface
	// here (see degrade.WrapWithSemantic for the same guard).
	switch c := exact.(type) {
	case *cache.ExactCache:
		if c == nil {
			exact = nil
		}
	case *cache.PersistentCache:
		if c == nil {
			exact = nil
		}
	}
	switch c := semantic.(type) {
	case *cache.LocalSemanticCache:
		if c == nil {
			semantic = nil
		}
	}
	return func(s *Server) { s.exactCache = exact; s.semanticCache = semantic }
}

// readinessResponse is the GET /dashboard/readiness payload: every
// registered local capability with its live ready state and the
// Retry-After a client should honour while it is not ready. Empty
// (non-nil) when no local capability is registered.
type readinessResponse struct {
	Capabilities []readiness.Capability `json:"capabilities"`
}

// handleReadiness serves GET /dashboard/readiness (Tier 3,
// auth-exempt like the other dashboard feeds: capability names and
// ready states only, no keys, no base URLs, no message content).
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	caps := []readiness.Capability{}
	if s.readinessGate != nil {
		caps = s.readinessGate.Statuses(r.Context())
	}
	writeJSON(w, http.StatusOK, readinessResponse{Capabilities: caps})
}

// localCapabilityName names the readiness capability for one relay
// target: "local:<provider-id>".
func localCapabilityName(target *relayTarget) string {
	return "local:" + target.ProviderID
}

// checkLocalReadiness answers whether a request for model may
// proceed. It returns false (having already written the 503) only
// when the model resolves to a gateway-provided local capability
// that is registered in the readiness gate and is not ready yet.
// Everything else proceeds: unknown models (the engine reports them
// honestly), non-local providers, and local providers with no
// registered capability (not gateway-provided, so there is nothing
// to be unready about) are never gated.
func (s *Server) checkLocalReadiness(w http.ResponseWriter, r *http.Request, model string) bool {
	if s.readinessGate == nil || s.relayResolver == nil || model == "" {
		return true
	}
	target, err := s.relayResolver.ResolveRelayTarget(r.Context(), model)
	if err != nil || target == nil || !target.IsLocal {
		return true
	}
	st := s.readinessGate.Status(r.Context(), localCapabilityName(target))
	if !st.Known || st.Ready {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(st.RetryAfterSeconds))
	writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: errorDetail{
		Message: fmt.Sprintf("local capability %s is not ready yet; its assets are still loading", target.ProviderID),
		Type:    "capability_not_ready",
		Reason:  "capability_not_ready",
	}})
	return false
}

// messagesHeader is the minimal decode of a /v1/messages request:
// only the fields the relay decision needs before the body is
// forwarded verbatim or parsed for translation.
type messagesHeader struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// handleMessages serves POST /v1/messages (Anthropic Messages
// dialect, gateway-key auth like /v1/chat/completions).
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Panic isolation (CHECKLIST 165-166): a panic anywhere on
	// the relay path — the parse, the relay decision, the verbatim
	// forward, the translate path — must never kill the gateway.
	// The stream half has its own recover inside
	// handleMessagesStream (headers + message_start are already
	// sent there); this is the backstop for everything else, and
	// answers an honest 502. A panic after a response was already
	// committed can only produce a superfluous WriteHeader
	// (logged once by net/http) — the stream half owns the
	// committed case. No ledger row is recorded here: a
	// verbatim-forwarded request's spend is never recorded in the
	// ledger even on success (standing relay gap), and a
	// translated request's attempts are recorded by the engine —
	// inventing a row here would double-count one and break the
	// other's rule. The panic is still counted + logged.
	defer func() {
		if v := recover(); v != nil {
			s.noteServerPanic("server_messages", v)
			s.writePanicFailure(w)
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	// Per-request routing contract (CHECKLIST 211): the canonical
	// spelling is the "routing" request-body field, exactly as on
	// /v1/chat/completions. A client that sends the contract as an
	// X-Codvian-Routing header instead gets a 400 naming the right
	// spelling — the header form is rejected outright, never
	// silently ignored.
	if r.Header.Get("X-Codvian-Routing") != "" {
		writeError(w, http.StatusBadRequest, "X-Codvian-Routing header is not supported; send the routing contract in the \"routing\" request-body field")
		return
	}

	// Admission before I/O (CHECKLIST 277 (b), extending 243 (g)
	// beyond chat + Responses): a blocked or over-limit key is
	// refused BEFORE the request body is read — the budget cap and
	// the requests-per-minute ceiling need only the key, never the
	// body. A key admitted here is still re-checked in full after
	// decode (the token axis and the unpriced-model check need the
	// body); this only ever refuses early.
	if s.preBodyAdmission(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return
	}
	// Security cluster (CHECKLIST 245): a client-supplied Vertex
	// location/project is rejected unless the admin opted in —
	// before the relay decision or any provider call.
	if ov, ok := provider.DetectVertexOverride(raw); ok {
		if refused := s.checkClientVertexOverride(w, &ov); refused {
			return
		}
	}
	var head messagesHeader
	if err := json.Unmarshal(raw, &head); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(head.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	// Intent tiers (CHECKLIST 277 (c), same rule as chat
	// completions in CHECKLIST 259): a request naming an unmapped
	// intent tier is refused here — before guardrails, budgets and
	// any provider call — with a 400 naming the tier, never the
	// generic empty-chain error the engine would return for the
	// unresolved virtual model. A mapped tier flows through to
	// the translate path, where the engine resolves it via
	// AutoRoute (a verbatim relay is impossible for intent/*:
	// no provider serves the virtual model).
	if refused := s.checkIntentTier(w, r, head.Model); refused {
		return
	}
	// Per-key access schedule (M5.10): a key outside its allowed
	// hours is refused with a 403 here, before any budget is
	// touched.
	if refused := s.checkKeyAccessSchedule(w, r); refused {
		return
	}
	// Per-key rolling 5-hour USD budget (CHECKLIST 139-140):
	// a capped key is refused here, before the relay decision
	// or any provider call. The unpriced-model half of the check
	// needs the resolved concrete model, which the relay does
	// not know yet for "auto/*" (it resolves inside the engine),
	// so a virtual model gets the cap check only, never a
	// guessed model. Note the standing relay gap: a
	// verbatim-forwarded request's spend is never recorded in
	// the ledger, so its cost does not count toward the window —
	// the cap still refuses a key the ledger shows as over, but
	// relay spend itself is invisible to it.
	if refused := s.checkKeyBudgetForModel(w, r, head.Model); refused {
		return
	}
	// Per-key token limits (M5.3): global + model scopes for
	// the requested model, before the traffic check.
	if refused := s.checkKeyTokenLimits(w, r, "", head.Model); refused {
		return
	}
	// Fair-share quota pools (M5.4): the caller's pools decide
	// before the traffic check.
	if refused := s.checkQuotaPools(w, r); refused {
		return
	}
	// Quota schedules (M5.9): the caller's pools' time windows
	// decide before the traffic check, right after the pool gate.
	if refused := s.checkQuotaSchedules(w, r); refused {
		return
	}
	// Internal budgets (M5.11): the operator's caps on total
	// spend (global / model / pool scopes) decide before the
	// traffic check, right after the schedule gate.
	if refused := s.checkInternalBudgets(w, r, head.Model); refused {
		return
	}
	if !s.checkTeamBudget(w, r) {
		return
	}
	// Per-key traffic limits (CHECKLIST 195): requests-per-minute
	// only — the relay forwards the body verbatim, so no local
	// prompt-token estimate exists and the tokens axis does not
	// apply on this path. G20: the model IS known here — head.Model
	// was parsed and validated above — so the gate runs as
	// checkKeyTrafficModel, not checkKeyTrafficRequestsOnly: the
	// ModelAllowed / ForcedModel policy (checkKeyPolicy) runs
	// BEFORE the limiter admits the request, so a model refusal
	// consumes no traffic slot and no provider is ever called.
	// A second post-admission check would be wrong: the slot
	// would already be spent. Same 403 key_forced_model /
	// key_model_not_allowed semantics as the chat path.
	if refused := s.checkKeyTrafficModel(w, r, 0, head.Model); refused {
		return
	}
	// Per-key throttle delay + max sessions (M5.10): after every
	// refusal gate and before the engine is called; the session
	// slot is held for the whole handler via the defer.
	if !s.throttleKeyDelay(r) {
		return
	}
	sessionKey, admitted := s.tryEnterKeySession(w, r)
	if !admitted {
		return
	}
	defer s.exitKeySession(sessionKey)
	if !s.checkLocalReadiness(w, r, head.Model) {
		return
	}

	// Resolve the target. A miss is not an error here: the request
	// falls through to the translate path, whose engine reports the
	// unknown model honestly (the relay never invents a target).
	var target *relayTarget
	if s.relayResolver != nil {
		if t, err := s.relayResolver.ResolveRelayTarget(ctx, head.Model); err == nil {
			target = t
		}
	}
	targetWire := ""
	if target != nil {
		targetWire = target.WireFormat
	}

	// The decision needs the canonical form, so the request is
	// parsed up front — but only the decision reads the parse: the
	// verbatim path still forwards the raw bytes, never the
	// re-encoded parse. An unparseable body cannot be relayed (its
	// cacheability is unknowable) and falls through to the translate
	// path, which reports the parse error honestly.
	parsed, err := relay.ParseAnthropicDetailed(raw)
	if err != nil {
		// The routing contract's own decode error already names
		// the offending field (see relay.ParseAnthropicDetailed);
		// surface it verbatim, exactly as handleChatCompletions
		// does, so the client can fix the spelling. A malformed
		// contract fails closed here even for a request that
		// would otherwise relay verbatim — before any provider
		// is dialled and before any byte is forwarded.
		if strings.Contains(err.Error(), "invalid routing") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var unrep *wire.UnrepresentableError
		if errors.As(err, &unrep) {
			writeError(w, http.StatusBadRequest, "request cannot be translated to the canonical form: "+unrep.Reason)
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return
	}
	req := anthropicToChatRequest(parsed.Request)
	// Per-request routing contract (CHECKLIST 211): forwarded
	// into the engine's translate path, so the contract is
	// honoured on /v1/messages exactly as on /v1/chat/completions
	// — the engine validates it (unknown provider → 400 naming
	// it, empty result → 400, price ceiling → 402) and records
	// the applied contract on the route trace. Endpoint names
	// the surface for the trace, so the dashboard can tell a
	// messages trace from a chat trace.
	req.Routing = parsed.Routing
	req.Endpoint = provider.EndpointMessages
	// Caller identity for prompt-cache affinity, exactly as on
	// /v1/chat/completions: the gateway-key ID, never the key.
	req.CallerID = s.callerKeyID(r)
	// Per-request cache controls (M6.4), same headers as chat
	// completions: bypass / no-store / type / key / threshold /
	// ttl, parsed fail-open. The relay's own lookup/store below
	// honour them; the translate path goes through the engine,
	// whose degrade wrapper honours the same field.
	req.CacheControls = parseCacheControls(r)

	// Guardrails first, exactly as on /v1/chat/completions: a
	// blocking toggle means the prompt must be inspected before
	// anything is served — including a cached answer. The translate
	// path applies the block; the cache is consulted only when
	// guardrails are off.
	contractPresent := parsed.Routing != nil && !parsed.Routing.Empty()
	features := relay.Features{
		GuardrailsBlocking: s.guardrailsBlocking(),
		MultiAttempt:       target != nil && target.Attempts > 1,
		// A non-empty contract is a canonical-form need: only
		// the engine can apply it, so the request is always
		// translated, never relayed verbatim. This is what
		// keeps the verbatim path contract-free by
		// construction — a contract can never reach it.
		RoutingPresent: contractPresent,
	}

	// Cache next, in canonical form (lookup-then-relay): a hit is
	// served in the Anthropic shape without calling any provider; a
	// miss leaves the cache with nothing to serve, so it no longer
	// forces a translation — the request relays verbatim. Only
	// cacheable requests are consulted (non-streaming, no tools),
	// exactly the set the engine would serve from cache. A request
	// carrying a non-empty routing contract never consults the
	// cache (CHECKLIST 211): a cached entry holds content only,
	// with no record of which provider produced it, so serving it
	// could hand back an answer from a provider the contract
	// forbade — the same rule the engine's cache wrapper enforces
	// on chat (internal/degrade).
	start := time.Now()
	cacheable := relay.Cacheable(parsed.Request)
	if !features.GuardrailsBlocking && s.cacheServing && !head.Stream && cacheable && !contractPresent {
		if hit, layer, found := s.lookupMessagesCache(ctx, req); found {
			s.serveMessagesCache(ctx, w, req, hit, layer, start)
			return
		}
	}
	verbatim, reason := relay.Decide("anthropic", targetWire, features)
	// Async guardrail mode (CHECKLIST 203): slow external
	// guardrails run concurrently with the provider call, on this
	// path exactly as on /v1/chat/completions — started here, after
	// every pre-call gate (sync guardrails, budgets, readiness) and
	// after the cache lookup, so a cache hit never spends a
	// classifier call, and closed by the deferred Finish when the
	// handler returns, so a verdict landing after the request ended
	// is recorded as too late instead of claiming an action that
	// never happened. A nil run means no async guardrail is
	// configured: no goroutines, no overhead. The run's surface is
	// "messages", so the dashboard's async-verdict feed shows which
	// verdicts ran on this path.
	var asyncRun *guardrails.AsyncRun
	if s.guardrails != nil {
		asyncRun = s.guardrails.StartAsyncOnScoped(requestText(&req), head.Stream, "messages", s.keyGuardrailScope(r))
	}
	if asyncRun != nil {
		defer asyncRun.Finish()
	}
	if verbatim && target != nil {
		s.forwardVerbatim(w, ctx, raw, head, target, reason, req, cacheable, start, asyncRun)
		return
	}
	if target == nil {
		reason = "no provider serves this model; routing through the engine"
	}
	s.handleMessagesTranslated(w, ctx, parsed, req, head, reason, start, asyncRun, s.keyGuardrailScope(r))
}

// lookupMessagesCache consults the exact-match cache first, then the
// semantic cache, for one canonical request — the same order and the
// same keys the engine uses, so a response stored by either dialect
// hits for the other. Misses are reported per layer consulted, like
// the engine's lookup. A nil cache layer is skipped, never faked.
// MODEL SCOPING (CHECKLIST 187): both layers are scoped by the
// model the request named — the exact key includes it, and the
// semantic lookup uses semanticLookupMessages (M6.11 scoped:
// GetTextForModelScopedWithThreshold) — so a cached claude-sonnet
// response is never served for a gpt-4o /v1/messages request,
// however similar the text. The provider is deliberately not part
// of the scope: the same model id on another provider shares the
// entry.
// TENANT SCOPING + TEMPERATURE GATING (M1.4, M6.11): the exact key is the
// scoped key (tenant = gateway key id, temperature bucket), and a
// temperature>0 request never consults the cache. The semantic
// lookup is scoped the same way since M6.11
// (semanticLookupMessages): the store records the tenant and
// temperature bucket on every scoped entry and serves only the
// requesting tenant at the requesting temperature, so a
// cross-tenant paraphrase and a cross-temperature paraphrase
// both miss — including entries the engine stored, since both
// dialects share the one store. The provenance gate
// (semanticLookupBlocked) is kept as defence in depth for
// stores without the scoped methods.
func (s *Server) lookupMessagesCache(ctx context.Context, req provider.ChatRequest) (cache.CachedResponse, string, bool) {
	// PER-REQUEST CACHE CONTROLS (M6.4): a bypass request never
	// consults the relay cache either — same rule as the
	// engine's lookup on the chat path.
	if req.CacheControls.BypassCache() {
		return cache.CachedResponse{}, "", false
	}
	if !cache.CacheableTemperature(req.Temperature, false) {
		return cache.CachedResponse{}, "", false
	}
	if s.exactCache != nil && req.CacheControls.UseExact() {
		if key, err := scopedMessagesKey(req); err == nil {
			if hit, found := s.exactCache.Get(ctx, key); found {
				return hit, "exact", true
			}
			if s.recorder != nil {
				s.recorder.IncCacheMiss("exact")
			}
		}
	}
	// 50-TURN BYPASS (M6.7c): a conversation longer than
	// provider.SemanticMaxTurns (50) never consults the semantic
	// layer — OmniRoute bypasses semantic caching past its
	// conversationHistoryThreshold, and the exact layer above is
	// unaffected (its key is the messages as sent).
	if s.semanticCache != nil && req.CacheControls.UseSemantic() && !provider.SemanticTurnsExceeded(req.Messages) {
		if text := relaySemanticText(req); text != "" {
			if s.semanticLookupBlocked(req, text) {
				if s.recorder != nil {
					s.recorder.IncCacheMiss("semantic")
				}
			} else if entry, found := s.semanticLookupMessages(ctx, req, text); found {
				return entry.Response, "semantic", true
			} else if s.recorder != nil {
				s.recorder.IncCacheMiss("semantic")
			}
		}
	}
	return cache.CachedResponse{}, "", false
}

// scopedMessagesKey derives the M1.4 cache key for a messages-path
// request: cache.ScopedKey with tenant = gateway key id
// (req.CallerID, set by handleMessages before any lookup) and the
// request's generation params. Nil slices are normalised to
// untyped nil first: a typed-nil slice marshals as JSON null inside
// the key payload, while an absent field is omitted — the same
// request would otherwise key differently depending on whether its
// decoder left the field nil or unset (same normalisation as
// degrade.scopedKey).
func scopedMessagesKey(req provider.ChatRequest) (string, error) {
	// M6.4 KEY OVERRIDE: identical to degrade.scopedKey — the
	// same hash (tenant + model + override) on both paths, so an
	// entry stored by one dialect hits for the other.
	if cc := req.CacheControls; cc != nil && cc.KeyOverride != "" {
		sum := sha256.Sum256([]byte("codvian-cache-key-override\x00" + req.CallerID + "\x00" + req.Model + "\x00" + cc.KeyOverride))
		return hex.EncodeToString(sum[:]), nil
	}
	var tools any
	if req.Tools != nil {
		tools = req.Tools
	}
	var toolChoice any
	if req.ToolChoice != nil {
		toolChoice = req.ToolChoice
	}
	var messages any
	if req.Messages != nil {
		messages = req.Messages
	}
	var stop any
	if req.Stop != nil {
		stop = req.Stop
	}
	return cache.ScopedKey(cache.KeyInput{
		Tenant:          req.CallerID,
		Model:           req.Model,
		Messages:        messages,
		Tools:           tools,
		ToolChoice:      toolChoice,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		MaxTokens:       req.MaxTokens,
		Stop:            stop,
		ReasoningEffort: req.ReasoningEffort,
	})
}

// cacheStoreRecorder is the structural interface the serving engine
// offers when it keeps cache provenance (degrade.StaleEngine does):
// NoteCacheStore indexes an exact-cache key under its tenant so a
// per-tenant flush can remove it, and NoteSemanticStore records
// which tenant stored a semantic entry for (model, text).
// SemanticLookupBlocked reports whether a semantic lookup for
// (tenant, model, text) must be skipped because the exact text is
// owned only by another tenant. The interfaces are structural so
// this package does not import internal/degrade (which imports
// this package); an engine that offers neither is simply
// unindexed and ungated, exactly as before.
type cacheStoreRecorder interface {
	NoteCacheStore(tenant, key string)
	NoteSemanticStore(tenant, model, text string)
	SemanticLookupBlocked(tenant, model, text string) bool
}

// semanticLookupBlocked asks the serving engine whether the
// semantic lookup for req's semantic window text must be skipped
// (M1.4 provenance gate). False when the engine offers no gate.
// Since M6.11 the scoped store enforces tenant isolation itself,
// so this gate is redundant for scoped stores but harmless, and
// still protects stores without the scoped methods.
func (s *Server) semanticLookupBlocked(req provider.ChatRequest, text string) bool {
	if rec, ok := s.engine.(cacheStoreRecorder); ok {
		return rec.SemanticLookupBlocked(req.CallerID, req.Model, text)
	}
	return false
}

// semanticScopedStore is the optional M6.11 tenant/temperature-
// scoped extension of cache.SemanticStore: a store that offers
// these methods serves a semantic entry only to a lookup naming
// the same tenant (gateway key id, "" = anonymous) and the same
// temperature bucket (the exact layer's 2-decimal bucketing,
// applied inside the cache) — and never serves a legacy
// (unscoped) entry. The in-memory LocalSemanticCache offers
// them, and so does the persistent adapter
// (cache.PersistentSemanticCache.AsStore). A store that does not
// offer them takes the legacy lookup/store in
// semanticLookupMessages / semanticStoreMessages, where only the
// provenance gate (semanticLookupBlocked) stands between
// tenants. The interface is structural so this package does not
// import internal/degrade (which imports this package); the
// same structural-assertion precedent as cacheStoreRecorder
// below and degrade.semanticThresholdStore (M6.7).
type semanticScopedStore interface {
	GetTextForModelScopedWithThreshold(ctx context.Context, model, text, tenant string, temperature float64, threshold float32) (cache.SemanticEntry, bool)
	SetTextForModelScoped(ctx context.Context, model, text, tenant string, temperature float64, entry cache.SemanticEntry, ttl time.Duration)
}

// semanticLookupMessages is the relay's semantic-layer lookup for
// one canonical request (M6.11): when the store offers the scoped
// method, the request's tenant (req.CallerID, set by
// handleMessages before any lookup) and temperature go into the
// cache with the lookup, so a cross-tenant paraphrase and a
// cross-temperature paraphrase both miss inside the layer —
// including an entry the engine stored for another tenant or at
// another temperature, since both dialects share the one store.
// The per-request threshold floor is pushed in with it (0 means
// the cache's configured floor, never below 0.85). A store
// without the scoped method takes the legacy lookup with the
// same post-filter the relay applied before M6.11.
func (s *Server) semanticLookupMessages(ctx context.Context, req provider.ChatRequest, text string) (cache.SemanticEntry, bool) {
	var thr float32
	if f, ok := req.CacheControls.ThresholdFloor(); ok {
		thr = f
	}
	if scoped, ok := s.semanticCache.(semanticScopedStore); ok {
		return scoped.GetTextForModelScopedWithThreshold(ctx, req.Model, text, req.CallerID, req.Temperature, thr)
	}
	hit, found := s.semanticCache.GetTextForModel(ctx, req.Model, text)
	if !found || hit.Similarity < thr {
		return cache.SemanticEntry{}, false
	}
	return hit, true
}

// semanticStoreMessages stores one semantic entry for req
// (M6.11): when the store offers the scoped method, the
// request's tenant and temperature go into the store with the
// entry, so a later scoped lookup — from either dialect — can
// enforce them. Otherwise the legacy SetTextForModel path stores
// an unscoped entry, which a scoped lookup would never serve.
func (s *Server) semanticStoreMessages(ctx context.Context, req provider.ChatRequest, text string, entry cache.SemanticEntry, ttl time.Duration) {
	if scoped, ok := s.semanticCache.(semanticScopedStore); ok {
		scoped.SetTextForModelScoped(ctx, req.Model, text, req.CallerID, req.Temperature, entry, ttl)
		return
	}
	s.semanticCache.SetTextForModel(ctx, req.Model, text, entry, ttl)
}

// noteRelayCacheStore records a relay-stored entry with the
// serving engine's provenance index, when it offers one, so a
// per-tenant flush and the semantic gate cover entries stored by
// this dialect too.
func (s *Server) noteRelayCacheStore(req provider.ChatRequest, key, text string) {
	rec, ok := s.engine.(cacheStoreRecorder)
	if !ok {
		return
	}
	if key != "" {
		rec.NoteCacheStore(req.CallerID, key)
	}
	if text != "" {
		rec.NoteSemanticStore(req.CallerID, req.Model, text)
	}
}

// relayMessageText is the visible text of one message: Content,
// or its text parts joined when sent as an array. "" when the
// message carries no text. Thin wrapper over the shared
// provider.SemanticMessageText (M6.7c) — the duplicate that lived
// here (mirroring degrade.messageText) is gone; kept as a wrapper
// so existing callers keep compiling unchanged.
func relayMessageText(m provider.Message) string {
	return provider.SemanticMessageText(m)
}

// relaySemanticText is the text the relay's semantic cache
// matches on (M6.7b): the last SemanticWindowTurns turns
// excluding system/developer messages, each rendered "role:
// text" (a missing role reads as "user") and joined with "\n",
// in conversation order. Messages inside the window with no
// text contribute no line but still occupy their window slot.
// "" when none of the last 3 turns carries text.
//
// Thin wrapper over the shared provider.SemanticWindowText
// (M6.7c). The duplication is gone: before M6.7c this was a
// deliberate duplicate of degrade.semanticText because
// internal/server cannot import internal/degrade (import cycle:
// degrade imports server for the Engine interface — see the
// cacheStoreRecorder comment above). The shared home is now
// internal/provider, which neither package imports in a cycle,
// so the relay and the engine can never drift apart again. The
// two functions are pinned to the same window text for the same
// messages by TestM67bRelaySemanticWindow, and cross-dialect
// sharing is proven end to end by TestM67bRelayEngineSemanticSharing.
//
// Lookup AND store both call this one function, so the window
// is identical at store and lookup by construction, exactly as
// on the engine path. The 50-turn bypass (M6.7c,
// provider.SemanticMaxTurns) is NOT applied here — this stays a
// pure window function; the cap is enforced at the call sites,
// lookupMessagesCache and storeMessagesCache.
func relaySemanticText(req provider.ChatRequest) string {
	return provider.SemanticWindowText(req.Messages)
}

// serveMessagesCache renders a cached response in the Anthropic shape
// (X-Codvian-Cache names the layer, X-Codvian-Relay names the hit).
// No provider is called and no ledger row is written — a cache hit
// bills nobody, exactly as on the chat-completions path.
func (s *Server) serveMessagesCache(ctx context.Context, w http.ResponseWriter, req provider.ChatRequest, hit cache.CachedResponse, layer string, start time.Time) {
	resp := &provider.ChatResponse{
		Content: hit.Content, Model: hit.Model, FinishReason: "stop",
		Usage: provider.Usage{PromptTokens: hit.Usage.PromptTokens, CompletionTokens: hit.Usage.CompletionTokens, TotalTokens: hit.Usage.TotalTokens},
	}
	if resp.Model == "" {
		resp.Model = req.Model
	}
	if s.recorder != nil {
		s.recorder.IncCacheHit(layer)
	}
	s.recordMetrics(ctx, req, resp, nil, start)
	body, err := relay.ToAnthropicResponse(resp)
	if err != nil {
		writeServerError(w, http.StatusBadGateway, "upstream provider error")
		return
	}
	w.Header().Set("X-Codvian-Cache", layer)
	w.Header().Set("X-Codvian-Relay", "cache-hit;layer="+layer)
	// Telemetry headers (M2.1, choke point): this request cost the
	// known $0 (no provider was called) and saved the cached
	// response's own tokens, priced from the ledger map.
	meta := s.responseMeta(ctx, req, resp, start)
	meta.Cache = layer
	meta = s.withCacheSavings(meta)
	s.writeResponseHeaders(w, ctx, meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// relayMaxStoreBody caps a verbatim response buffered for cache
// storage (10 MiB, the same cap as the provider client's
// maxResponseBody). A larger body still relays; it is simply not
// stored.
const relayMaxStoreBody = 10 << 20

// storeMessagesCache stores a successful verbatim non-streaming
// response under the same exact + semantic keys the engine uses, so
// later requests in either dialect can hit it. Best-effort and
// silent on failure: a response the canonical form cannot carry
// (e.g. a thinking block) is relayed but not stored, never failed.
// MODEL SCOPING (CHECKLIST 187): the semantic entry is stored
// under the model the REQUEST named (semanticStoreMessages with
// req.Model), not the upstream model id the response carries —
// the scope is the requested model, so only a later request
// naming that same model can ever be served it. TENANT +
// TEMPERATURE SCOPING (M6.11): the same store call records the
// requesting tenant (req.CallerID) and the temperature bucket,
// so only a later request from the same tenant at the same
// temperature bucket can ever be served the entry.
func (s *Server) storeMessagesCache(ctx context.Context, req provider.ChatRequest, upstreamBody []byte) {
	if s.exactCache == nil && s.semanticCache == nil {
		return
	}
	// PER-REQUEST CACHE CONTROLS (M6.4): a bypass or no-store
	// request never stores into the shared cache from the relay
	// path either.
	if !req.CacheControls.AllowStore() {
		return
	}
	var aresp wire.AnthropicResponse
	if err := json.Unmarshal(upstreamBody, &aresp); err != nil {
		return
	}
	conv, err := wire.ConvertResponse(&aresp)
	if err != nil || conv == nil || len(conv.Choices) == 0 {
		return
	}
	var content string
	if len(conv.Choices[0].Message.Content) > 0 {
		_ = json.Unmarshal(conv.Choices[0].Message.Content, &content)
	}
	entry := cache.CachedResponse{
		Content: content,
		Usage:   cache.Usage{PromptTokens: conv.Usage.PromptTokens, CompletionTokens: conv.Usage.CompletionTokens, TotalTokens: conv.Usage.TotalTokens},
		Model:   conv.Model,
	}
	if entry.Model == "" {
		entry.Model = req.Model
	}
	// 1h TTL, matching degrade.DefaultStoreTTL — or the
	// per-request TTL (M6.4), capped at that 1h max.
	ttl := time.Hour
	if t := req.CacheControls.EffectiveTTL(); t > 0 {
		ttl = t
	}
	// STORE GATES (M1.4): the entry is stored only when the
	// response is complete (finish_reason "length" is truncated),
	// the temperature is cacheable, and it carries no tool calls —
	// the same cache.ShouldStore gate the engine's store applies,
	// so neither dialect can smuggle a truncated, hot or
	// tool-carrying answer into the shared cache. The exact key
	// is the scoped key (tenant + model + params), and the store
	// is recorded with the engine's provenance index when it
	// offers one, so per-tenant flush and the semantic gate cover
	// relay-stored entries too.
	storable := cache.ShouldStore(conv.Choices[0].FinishReason, req.Temperature, false, len(conv.Choices[0].Message.ToolCalls) > 0)
	if !storable {
		return
	}
	if s.exactCache != nil && req.CacheControls.UseExact() {
		if key, err := scopedMessagesKey(req); err == nil {
			s.exactCache.Set(ctx, key, entry, ttl)
			s.noteRelayCacheStore(req, key, "")
		}
	}
	// 50-TURN BYPASS (M6.7c): a conversation longer than
	// provider.SemanticMaxTurns (50) never stores a semantic entry
	// either — deliberately stricter than OmniRoute, whose store
	// path has no threshold check (see
	// provider.SemanticTurnsExceeded), so no entry can exist that
	// a later lookup would have to bypass around. The exact layer
	// above is unaffected.
	if s.semanticCache != nil && req.CacheControls.UseSemantic() && !provider.SemanticTurnsExceeded(req.Messages) {
		if text := relaySemanticText(req); text != "" {
			s.semanticStoreMessages(ctx, req, text, cache.SemanticEntry{Response: entry}, ttl)
			s.noteRelayCacheStore(req, "", text)
		}
	}
}

// guardrailsBlocking reports whether any blocking guardrail toggle
// is on: with one on, the request text must be inspected before any
// provider is dialled, which needs the canonical form.
func (s *Server) guardrailsBlocking() bool {
	if s.guardrails == nil {
		return false
	}
	cfg := s.guardrails.Config()
	return cfg.BlockPII || cfg.BlockSecrets || cfg.BlockInjection
}

// asyncBlocked reports the first BLOCK verdict on the run, if any.
// Nil-safe: a nil run (no async guardrail configured) never blocks.
func asyncBlocked(run *guardrails.AsyncRun) (guardrails.AsyncResult, bool) {
	if run == nil {
		return guardrails.AsyncResult{}, false
	}
	return run.BlockVerdict()
}

// anthropicPolicyRefusalEvent renders the terminal frame that ends a
// cut /v1/messages stream (CHECKLIST 203).
//
// The exact bytes, for the record — this is the Anthropic-protocol
// terminal error event, the same shape the translate path already
// used for a stream error:
//
//	event: error
//	data: {"type":"error","error":{"type":"api_error","message":"guardrail blocked stream: <kind>"}}
//
// No [DONE] follows it and no message_stop precedes it: Anthropic
// streams never carry [DONE] (that is the OpenAI envelope —
// inventing one here would break Anthropic clients waiting for
// message_stop), and a message_stop would claim the message
// completed normally. The error event alone is terminal. The kind
// label is the only detail — no prompt or response text is ever
// echoed. Anthropic defines no policy-refusal error type or code,
// so the frame keeps this path's existing api_error type and carries
// the kind in the message; the gateway's own -32001 code lives on
// the plain-JSON 403 refusal (before commit), not in this event.
// anthropicStreamLifetimeEvent renders the terminal error event
// for a relayed stream killed at the lifetime cap (CHECKLIST
// 255): the same "event: error" shape as the generic upstream
// failure, but the message names the cap — it is
// gateway-generated (the cap that fired), so unlike an upstream
// body it cannot echo prompt or response text.
func anthropicStreamLifetimeEvent(err error) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": err.Error(),
		},
	})
	return []byte("event: error\ndata: " + string(payload) + "\n\n")
}

func anthropicPolicyRefusalEvent(kind string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": "guardrail blocked stream: " + kind,
		},
	})
	return []byte("event: error\ndata: " + string(payload) + "\n\n")
}

// writeVerbatimHeaders copies the upstream response headers the
// relay passes through (status aside): Content-Type, Retry-After,
// Anthropic / x- headers, plus the relay markers.
func writeVerbatimHeaders(w http.ResponseWriter, resp *http.Response, reason string) {
	h := w.Header()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		h.Set("Content-Type", ct)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		h.Set("Retry-After", ra)
	}
	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "anthropic-") || strings.HasPrefix(lk, "x-") {
			for _, v := range vs {
				h.Add(k, v)
			}
		}
	}
	h.Set("X-Codvian-Relay", "verbatim")
	h.Set("X-Codvian-Relay-Reason", reason)
}

// forwardVerbatim POSTs the raw body to the provider's /v1/messages
// endpoint (only the model field rewritten, when the upstream model
// differs) and copies the response back byte-for-byte: status,
// Content-Type, Anthropic headers, and the body — streaming SSE
// events included, with no re-encoding. An upstream error is copied
// the same way: a single-attempt relay has no failover, so the
// client sees the upstream's own status and body.
//
// asyncRun is the concurrent async guardrail check for this request
// (CHECKLIST 203; nil when no async guardrail is configured). When
// present, the upstream call runs under a child context the verdict
// can cancel, and a BLOCK verdict:
//
//   - streaming: cuts the relayed stream — the copy loop ends when
//     the cancelled upstream call closes, and the stream terminates
//     with anthropicPolicyRefusalEvent (an `event: error` event,
//     no [DONE], no message_stop). The upstream's own error events,
//     if any arrived before the cut, pass through untouched.
//   - non-streaming: withholds the response — the body is buffered
//     (capped at relayMaxStoreBody) before anything is written, so
//     a BLOCK that landed during the upstream call still answers
//     the plain 403 / -32001 JSON refusal with nothing leaked.
//   - before the upstream call returns at all: the same plain 403,
//     because no headers were committed yet.
//
// A cut is a policy refusal, never an upstream failure: no
// upstream-failure count, no failover, and the metrics error is the
// async-block sentinel. The verbatim path keeps its standing rule
// of recording no ledger row, cut or not — a relayed request's spend
// is never recorded in the ledger.
func (s *Server) forwardVerbatim(w http.ResponseWriter, ctx context.Context, raw []byte, head messagesHeader, target *relayTarget, reason string, req provider.ChatRequest, cacheable bool, start time.Time, asyncRun *guardrails.AsyncRun) {
	body := raw
	if target.UpstreamModel != "" && target.UpstreamModel != head.Model {
		rewritten, err := relay.RewriteModel(raw, target.UpstreamModel)
		if err != nil {
			writeError(w, http.StatusBadRequest, "request body has no top-level model to rewrite")
			return
		}
		body = rewritten
	}
	// The upstream call runs under a child context the async
	// verdict can cancel, so a BLOCK stops the upstream call
	// instead of letting it run to completion behind a cut stream.
	// The cut goroutine exits when the run finishes with no block,
	// or when the upstream call ends first.
	upCtx, upCancel := context.WithCancelCause(ctx)
	defer upCancel(nil)
	if asyncRun != nil {
		go func() {
			select {
			case res, ok := <-asyncRun.Blocked():
				if !ok {
					return // run finished with no block: no cut
				}
				upCancel(&guardrails.AsyncBlockError{Guardrail: res.Name, Kind: res.Kind})
			case <-upCtx.Done():
			}
		}()
	}
	// Stream-lifetime cap (CHECKLIST 255): a relayed stream gets
	// the same max lifetime as an engine stream — the timer
	// cancels the upstream call with the typed lifetime error
	// when it fires, so a stalled upstream cannot hold this
	// relay open forever. It runs only for streaming responses:
	// a non-streaming relay is one response body, bounded by the
	// http.Client timeout below. The cap is the engine's
	// effective cap (0 = no engine cap reported — the timeout
	// bound remains). The first cancel wins, so a guardrail cut
	// that fires first keeps its cause.
	if head.Stream {
		if cap := s.streamLifetimeCap(); cap > 0 {
			timer := time.AfterFunc(cap, func() { upCancel(router.NewStreamLifetimeError(cap)) })
			defer timer.Stop()
		}
	}
	upstream, err := http.NewRequestWithContext(upCtx, http.MethodPost, relay.MessagesURL(target.BaseURL), bytes.NewReader(body))
	if err != nil {
		writeServerError(w, http.StatusBadGateway, "upstream provider error")
		return
	}
	for k, v := range target.AuthHeaders {
		upstream.Header.Set(k, v)
	}
	upstream.Header.Set("Content-Type", "application/json")
	if head.Stream {
		upstream.Header.Set("Accept", "text/event-stream")
	} else {
		upstream.Header.Set("Accept", "application/json")
	}
	client := &http.Client{Timeout: s.timeout}
	resp, err := client.Do(upstream)
	if err != nil {
		// A BLOCK verdict that cancelled the upstream call is a
		// policy refusal, not an upstream failure: nothing was
		// committed yet, so the plain 403 JSON is still possible.
		if res, blocked := asyncBlocked(asyncRun); blocked {
			s.recordMetrics(ctx, provider.ChatRequest{Model: head.Model, Stream: head.Stream}, nil, errAsyncGuardrailBlocked, start)
			// No telemetry headers on a withheld relay: record
			// it server-side as a policy refusal.
			s.recordTelemetry(ctx, s.verbatimMeta(target, head, start), http.StatusForbidden)
			w.Header().Set("X-Codvian-Guardrail", "blocked")
			writePolicyRefusal(w, res.Kind, "request")
			return
		}
		s.recordMetrics(ctx, provider.ChatRequest{Model: head.Model, Stream: head.Stream}, nil, err, start)
		// The upstream call itself failed before any byte was
		// committed: record it server-side as a 502.
		s.recordTelemetry(ctx, s.verbatimMeta(target, head, start), http.StatusBadGateway)
		writeServerError(w, http.StatusBadGateway, "upstream provider error")
		return
	}
	defer resp.Body.Close()
	// Non-streaming with an async run: buffer the body (capped)
	// before writing anything, so a BLOCK that landed during the
	// upstream call can still withhold the response. Without an
	// async run the buffered path below is unchanged (buffered
	// only when cacheable, for storage).
	if !head.Stream && asyncRun != nil {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, relayMaxStoreBody+1))
		if res, blocked := asyncRun.BlockVerdict(); blocked {
			s.recordMetrics(ctx, provider.ChatRequest{Model: head.Model, Stream: head.Stream}, nil, errAsyncGuardrailBlocked, start)
			w.Header().Set("X-Codvian-Guardrail", "blocked")
			writePolicyRefusal(w, res.Kind, "request")
			return
		}
		s.recordMetrics(ctx, provider.ChatRequest{Model: head.Model, Stream: head.Stream}, nil, nil, start)
		writeVerbatimHeaders(w, resp, reason)
		// Telemetry headers (M2.1): the relay target and model
		// are known; tokens and cost are not (the body is
		// forwarded verbatim and the ledger records no row for
		// it — the standing relay gap), so they stay absent.
		meta := s.verbatimMeta(target, head, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, resp.StatusCode)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(buf)
		// M6.3: gate the store on the same cacheable condition as
		// the normal (non-async) branch below — a tool-carrying
		// request must not be stored just because an async
		// guardrail run was active. ShouldStore inside
		// storeMessagesCache still refuses "length" here.
		if cacheable && (s.exactCache != nil || s.semanticCache != nil) && resp.StatusCode >= 200 && resp.StatusCode < 300 && len(buf) <= relayMaxStoreBody {
			s.storeMessagesCache(ctx, req, buf)
		} else if len(buf) > relayMaxStoreBody {
			// Oversized: the buffered prefix is already written;
			// the remainder follows verbatim.
			_, _ = io.Copy(w, resp.Body)
		}
		return
	}
	s.recordMetrics(ctx, provider.ChatRequest{Model: head.Model, Stream: head.Stream}, nil, nil, start)
	writeVerbatimHeaders(w, resp, reason)
	// Telemetry headers (M2.1): as on the buffered branch above.
	meta := s.verbatimMeta(target, head, start)
	s.writeResponseHeaders(w, ctx, meta)
	s.recordTelemetry(ctx, meta, resp.StatusCode)
	w.WriteHeader(resp.StatusCode)
	// Non-streaming cacheable responses are buffered (capped) so the
	// response can be stored under the same cache keys the engine
	// uses — the cache learns from native traffic instead of being
	// bypassed by it. Streams and uncacheable responses copy
	// through chunk by chunk, as before.
	if !head.Stream && cacheable && (s.exactCache != nil || s.semanticCache != nil) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, relayMaxStoreBody+1))
		_, _ = w.Write(body)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && len(body) <= relayMaxStoreBody {
			s.storeMessagesCache(ctx, req, body)
		}
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		// Copy in chunks and flush after each, so a streaming SSE
		// response reaches the client event by event instead of
		// only when the upstream finishes.
		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				// The first byte is out — a BLOCK verdict landing
				// after this point can no longer withhold the
				// response, only cut the stream.
				asyncRun.MarkDelivered()
				flusher.Flush()
			}
			if err != nil {
				break
			}
		}
		// Async cut (CHECKLIST 203): the upstream call ended
		// because the verdict cancelled it (or the stream ended on
		// its own). If a BLOCK landed, terminate with the
		// Anthropic terminal error event — never [DONE], never
		// message_stop. On a normal end (no block) nothing more
		// is written: the upstream's own message_stop already
		// terminated the stream.
		if res, blocked := asyncBlocked(asyncRun); blocked {
			_, _ = w.Write(anthropicPolicyRefusalEvent(res.Kind))
			flusher.Flush()
			s.logger.Printf("guardrail: async %s blocked messages relay stream (kind=%s); stream cut, terminal refusal event", res.Name, res.Kind)
			return
		}
		// Stream-lifetime cap (CHECKLIST 255): the relay was
		// killed at the cap — terminate with the Anthropic
		// terminal error event naming it, never [DONE], never
		// message_stop. The upstream call is already terminated
		// (the cap cancelled it); the client must not wait on a
		// terminator that never comes.
		if lt := streamLifetimeCause(upCtx); lt != nil {
			_, _ = w.Write(anthropicStreamLifetimeEvent(lt))
			flusher.Flush()
			s.logger.Printf("relay: stream lifetime exceeded; stream cut, terminal error event")
		}
		return
	}
	_, _ = io.Copy(w, resp.Body)
	if res, blocked := asyncBlocked(asyncRun); blocked {
		// No flusher (not a live SSE stream): the terminal frame
		// is still written — the client reads it as the last
		// event either way.
		_, _ = w.Write(anthropicPolicyRefusalEvent(res.Kind))
	}
	// Stream-lifetime cap (CHECKLIST 255): same terminal event
	// as the flusher path above, for a relay without a flusher.
	if lt := streamLifetimeCause(upCtx); lt != nil {
		_, _ = w.Write(anthropicStreamLifetimeEvent(lt))
	}
}

// handleMessagesTranslated routes a /v1/messages request through
// the canonical engine path (guardrails, cache, failover all apply)
// and renders the result back in the Anthropic shape.
//
// asyncRun is the concurrent async guardrail check for this request
// (CHECKLIST 203; nil when no async guardrail is configured): a
// BLOCK verdict withholds a non-streaming response with 403 /
// -32001 before delivery, exactly like the chat-completions
// withhold, and cuts a stream through the engine's cancel cause,
// exactly like the chat-completions stream cut.
func (s *Server) handleMessagesTranslated(w http.ResponseWriter, ctx context.Context, parsed *relay.ParseResult, req provider.ChatRequest, head messagesHeader, reason string, start time.Time, asyncRun *guardrails.AsyncRun, allowed []string) {
	if lf := logFieldsFrom(ctx); lf != nil {
		lf.model = req.Model
	}
	// Security cluster (CHECKLIST 245): the deployment's pinned
	// parameters outrank the translated request's fields. (The
	// verbatim relay path forwards the client body unchanged by
	// design, so no pin applies there — see
	// docs/security-cluster-245-246.md.)
	s.applyDeploymentParams(w, &req)
	// Guardrails run on the canonical request before any provider
	// is dialled, exactly as on /v1/chat/completions. A block is a
	// 400 with a kind label only — the prompt is never echoed.
	if blocked := s.applyGuardrails(ctx, w, &req, allowed); blocked {
		return
	}
	// Plugin pre-request hooks (CHECKLIST 201), same as on
	// /v1/chat/completions: after guardrails, before any provider
	// is dialled. The ctx below carries any plugin-added outbound
	// headers to the provider client.
	if blocked, newCtx := s.applyPlugins(ctx, w, &req); blocked {
		return
	} else {
		ctx = newCtx
	}
	// Request-scoped attribution (CHECKLIST 317-318): the
	// engine publishes this request's decision + trace into
	// the sink on ctx, so recordMetrics / setAutoRouteHeader
	// below attribute the outcome to this request even when a
	// neighbour finishes first.
	ctx = router.WithAttribution(ctx, router.NewAttribution())
	if parsed.DroppedCacheControl > 0 {
		// The translate path dropped provider-native cache_control
		// markers the canonical form cannot carry. Log the count
		// (never the content) and report it in the relay header,
		// so the loss is visible instead of silent.
		s.logger.Printf("relay: translated /v1/messages for model %s dropped %d cache_control blocks", req.Model, parsed.DroppedCacheControl)
	}
	relayHeader := "translated;reason=" + reason
	if parsed.DroppedCacheControl > 0 {
		relayHeader += fmt.Sprintf(";dropped_cache_control=%d", parsed.DroppedCacheControl)
	}
	if s.recorder != nil {
		s.recorder.IncInFlight()
		defer s.recorder.DecInFlight()
	}
	if head.Stream {
		err := s.handleMessagesStream(w, ctx, req, relayHeader, asyncRun, start)
		// A panic the engine recovered (provider/hook): log its
		// redacted value once, here, and leave a ledger row
		// when the engine recorded no attempt.
		s.logPanicError(err)
		if isPanicError(err) {
			s.recordPanicRowIfUnrecorded(ctx, req.CallerID, req, start)
		}
		s.recordMetrics(ctx, req, nil, err, start)
		return
	}
	resp, err := s.engine.Chat(ctx, req)
	s.logPanicError(err)
	if isPanicError(err) {
		s.recordPanicRowIfUnrecorded(ctx, req.CallerID, req, start)
	}
	// Async guardrail verdict (CHECKLIST 203): a BLOCK that landed
	// while the provider call was in flight withholds the response
	// with 403 / -32001, exactly like the response guardrail below
	// and like the chat-completions withhold. The attempt stays in
	// the ledger as the engine recorded it (the provider was called
	// and billed) — what is withheld is only the delivery. Checked
	// before the single recordMetrics below, so a withheld response
	// is counted once, as the async-block error, never as a success.
	if err == nil && resp != nil {
		if res, blocked := asyncBlocked(asyncRun); blocked {
			s.recordMetrics(ctx, req, resp, errAsyncGuardrailBlocked, start)
			w.Header().Set("X-Codvian-Guardrail", "blocked")
			writePolicyRefusal(w, res.Kind, "request")
			return
		}
	}
	// Response guardrail (B3-22): the provider's own reply is
	// scanned the same way as the request, before it is rendered
	// into the Anthropic shape — a blocked response is refused
	// with 403 / -32001 and its content never leaves the router,
	// and in redact mode the reply is rewritten in place, so the
	// redaction flows into the translated body below. Checked
	// before the single recordMetrics below, so a withheld
	// response is counted once, as the guardrail-block error,
	// never as a success. The stream half cannot be checked
	// this way — it is sanitized incrementally by the
	// ResponseStreamRedactor in handleMessagesStream when
	// sanitization is on.
	if err == nil && resp != nil {
		if blocked := s.applyResponseGuardrails(w, resp, allowed); blocked {
			s.recordMetrics(ctx, req, resp, errGuardrailResponseBlocked, start)
			s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), http.StatusForbidden)
			return
		}
	}
	s.recordMetrics(ctx, req, resp, err, start)
	if err != nil {
		// Telemetry headers (M2.1): the trace still names the
		// attempted chain; tokens/cost stay absent.
		meta := s.responseMeta(ctx, req, resp, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, s.statusForEngineError(err))
		s.handleEngineError(w, err)
		return
	}
	if resp == nil {
		meta := s.responseMeta(ctx, req, resp, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, http.StatusBadGateway)
		writeServerError(w, http.StatusBadGateway, "upstream provider error")
		return
	}
	body, err := relay.ToAnthropicResponse(resp)
	if err != nil {
		writeServerError(w, http.StatusBadGateway, "upstream provider error")
		return
	}
	s.setStaleHeader(w)
	s.setCacheHeader(w)
	s.setAutoRouteHeader(ctx, w)
	s.setStrategyHeader(w)
	// Decision transparency (CHECKLIST 199): the translated path
	// routes through the engine, so it carries the decision
	// header like a chat completion. (The verbatim path bypasses
	// the engine and never sets it — X-Codvian-Relay names that
	// target instead — and the relay stream commits its headers
	// before routing, so it cannot carry it either; both are
	// documented in docs/decision-header.md.)
	s.setDecisionHeader(w, resp.Model, req.Model)
	// Telemetry headers (M2.1, choke point): cost/tokens/latency/
	// fallback from the real trace, usage and ledger price map.
	meta := s.responseMeta(ctx, req, resp, start)
	s.writeResponseHeaders(w, ctx, meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	w.Header().Set("X-Codvian-Relay", relayHeader)
	w.Header().Set("Content-Type", "application/json")
	// Async guardrail mode (CHECKLIST 203): the response is
	// delivered now — a BLOCK verdict landing after this point is
	// too late to withhold it, and the run records it as
	// blocked_after_delivery.
	asyncRun.MarkDelivered()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleMessagesStream streams an engine answer back as Anthropic
// SSE events. Text deltas flow as they arrive; tool calls (known
// only once the engine finishes) are emitted whole at the end, then
// message_delta carries the true usage and stop reason.
//
// asyncRun is the concurrent async guardrail check for this request
// (CHECKLIST 203; nil when no async guardrail is configured): the
// stream runs under a child context the async verdict can cancel,
// and a BLOCK verdict cuts it with the Anthropic terminal error
// event (anthropicPolicyRefusalEvent — no [DONE], no message_stop),
// exactly like the chat-completions stream cut, instead of an
// upstream error.
func (s *Server) handleMessagesStream(w http.ResponseWriter, ctx context.Context, req provider.ChatRequest, relayHeader string, asyncRun *guardrails.AsyncRun, start time.Time) (err error) {
	// Async guardrail mode (CHECKLIST 203): the stream runs under
	// a child context the async verdict can cancel. When a BLOCK
	// verdict lands, the cut goroutine cancels with the block as
	// the cause; the engine records the cut (trace +
	// prompt-estimate ledger row, no health signal) and the
	// handler below writes the terminal refusal event. The parent
	// timeout still applies — cancelling the parent cancels this
	// child too. The deferred cancel is a no-op once the stream
	// ends (the context is abandoned either way).
	streamCtx, streamCancel := context.WithCancelCause(ctx)
	defer streamCancel(nil)
	if asyncRun != nil {
		go func() {
			select {
			case res, ok := <-asyncRun.Blocked():
				if !ok {
					return // run finished with no block: no cut
				}
				streamCancel(&guardrails.AsyncBlockError{Guardrail: res.Name, Kind: res.Kind})
			case <-streamCtx.Done():
			}
		}()
	}
	em, err := relay.NewAnthropicStreamEmitter(w, "", req.Model)
	if err != nil {
		writeServerError(w, http.StatusBadGateway, "upstream provider error")
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Codvian-Relay", relayHeader)
	// Telemetry stream-start headers (M2.1): request id, version
	// and the requested model are known before the first event;
	// cost/tokens/fallback ride the final trailer instead.
	s.writeStreamStartHeaders(w, ctx, req.Model)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	// Panic isolation (CHECKLIST 165-166), relay-stream half: a
	// panic after the headers (and message_start) are sent — in
	// the engine call, or in the emitter callback writing deltas
	// — must end this stream with an Anthropic error event, never
	// kill the gateway. Panics before the headers are caught by
	// the outer recover in handleMessages (502 — the headers
	// have not been sent yet there). The panic is counted +
	// logged redacted (noteServerPanic); logPanicError in
	// handleMessagesTranslated skips server-side locations, so
	// this line is the only log for it.
	defer func() {
		if v := recover(); v != nil {
			pe := s.noteServerPanic("server_messages_stream", v)
			err = pe
			_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", `{"type":"error","error":{"type":"api_error","message":"gateway internal error"}}`)
			flush()
		}
	}()
	if err := em.Start(); err != nil {
		return err
	}
	flush()
	// Async guardrail mode (CHECKLIST 203): message_start is the
	// first byte out — a BLOCK verdict landing after this point can
	// no longer withhold the response, only cut the stream.
	asyncRun.MarkDelivered()
	var resp *provider.ChatResponse
	// Stream redaction (M1.7 + B3-22): PEM private-key blocks split
	// across SSE chunks are withheld by the redactor and replaced
	// with one placeholder; Flush() below emits the withheld tail
	// before the tool-use blocks and message_delta. One redactor
	// per stream (not safe for concurrent use). With response
	// sanitization opted in, the redactor is the store-wired
	// ResponseStreamRedactor instead: it runs the same PEM
	// redactor first, then redacts PII/secrets line by line in
	// the store's mode — and in block mode it cuts the stream at
	// the first line carrying a finding (see the cut check in
	// the callbacks and after the engine call). With
	// sanitization off, the PEM-only redactor is exactly as
	// before. Same shape as handleChatStream (server.go).
	var respRedactor *guardrails.ResponseStreamRedactor
	var redactor streamChunkRedactor = guardrails.NewStreamRedactor()
	if s.responseSanitization {
		respRedactor = s.guardrails.NewResponseStreamRedactor("")
		redactor = respRedactor
	}
	// cutByResponseSanitization ends a stream cut by the response
	// sanitization stage in block mode (B3-22): the Anthropic
	// terminal error event, a 403 telemetry record, and the
	// sentinel — the same shape as the async cut below. A policy
	// refusal, never an upstream failure: no upstream-failure
	// count, no failover. Called from two sites: inside the
	// error block (the engine propagated the callback
	// sentinel) and after it (the engine ignored the callback
	// error and returned nil) — the decision uses the
	// redactor's flag, not the error value, so it holds
	// either way.
	cutByResponseSanitization := func() error {
		kind := "secret"
		if f := respRedactor.Findings(); len(f) > 0 {
			kind = f[0].Kind
		}
		_, _ = w.Write(anthropicPolicyRefusalEvent(kind))
		flush()
		s.logger.Printf("guardrail: response sanitization blocked messages stream (kind=%s); stream cut, terminal refusal event", kind)
		s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), http.StatusForbidden)
		return errResponseGuardrailBlocked
	}
	if ce, ok := s.engine.(chunkEngine); ok {
		resp, err = ce.ChatStreamChunks(streamCtx, req, func(chunk provider.StreamChunk) error {
			if chunk.Content != "" {
				if safe := redactor.WriteChunk(chunk.Content); safe != "" {
					if err := em.TextDelta(safe); err != nil {
						return err
					}
					flush()
				}
			}
			// Response sanitization cut (B3-22, block mode): the
			// redactor withheld the first line carrying a finding,
			// so nothing further may be emitted. Returning the
			// sentinel stops the engine loop; the terminal refusal
			// event is written by cutByResponseSanitization after
			// the engine call below. Without this return the stream
			// would keep running silently and close with a normal
			// message_stop, as if the response had been delivered
			// in full.
			if respRedactor != nil && respRedactor.Blocked() {
				return errResponseGuardrailBlocked
			}
			return nil
		})
	} else {
		resp, err = s.engine.ChatStream(streamCtx, req, func(delta string) error {
			if safe := redactor.WriteChunk(delta); safe != "" {
				if err := em.TextDelta(safe); err != nil {
					return err
				}
				flush()
			}
			// Response sanitization cut (B3-22, block mode): see
			// the chunks callback above.
			if respRedactor != nil && respRedactor.Blocked() {
				return errResponseGuardrailBlocked
			}
			return nil
		})
	}
	if err != nil {
		// Async guardrail cut (CHECKLIST 203): a BLOCK verdict
		// landed while the stream was in flight, and the cut
		// goroutine cancelled the stream context with it. The
		// engine already recorded the cut attempt (trace +
		// prompt-estimate ledger row, no health signal); the
		// client gets the Anthropic terminal error event — no
		// [DONE] (Anthropic streams never carry one), no
		// message_stop (the message did not complete). This is a
		// policy refusal, never an upstream failure: no
		// upstream-failure count, no failover, and the returned
		// sentinel (errAsyncGuardrailBlocked) records the request
		// as an error in metrics without implicating any provider.
		// Only a failed stream can have been cut: a stream that
		// completed (err == nil) delivered its full response, and
		// a BLOCK that landed after that is recorded by the run as
		// blocked_after_delivery — too late to act, and said so.
		// The headers (and message_start) are already sent: the
		// stream can only end with an error event, never a fresh
		// status code — so there is no plain-403 branch here, unlike
		// the verbatim path where the upstream call can fail before
		// any byte is committed.
		if res, blocked := asyncBlocked(asyncRun); blocked {
			_, _ = w.Write(anthropicPolicyRefusalEvent(res.Kind))
			flush()
			s.logger.Printf("guardrail: async %s blocked messages stream after start (kind=%s); stream cut, terminal refusal event", res.Name, res.Kind)
			// No trailer on a cut stream — record it
			// server-side as a policy refusal.
			s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), http.StatusForbidden)
			return errAsyncGuardrailBlocked
		}
		// Response sanitization cut (B3-22, block mode): the
		// ResponseStreamRedactor cut the stream at the first line
		// carrying a finding. Checked here — before the generic
		// error path — so a cut is never mislabelled as an
		// upstream provider error. The async cut above keeps
		// precedence: when both fired, the async verdict wins.
		if respRedactor != nil && respRedactor.Blocked() {
			return cutByResponseSanitization()
		}
		// The headers (and message_start) are already sent: the
		// stream can only end with an error event, never a fresh
		// status code. Map the failure the same way the chat
		// stream does — a plain error event carrying the generic
		// message, no upstream detail.
		// Stream redaction (M1.7): emit the redactor's withheld
		// tail before the error event, or a trailing partial
		// header held for the next chunk is lost (same rule as
		// the chat stream's error path). Withheld text was never
		// sent unredacted, so this cannot leak.
		if tail := redactor.Flush(); tail != "" {
			if err := em.TextDelta(tail); err != nil {
				return err
			}
			flush()
		}
		// Stream-lifetime cap (CHECKLIST 255): the error event
		// names the cap, not a generic upstream failure — the
		// message is gateway-generated, so unlike an upstream
		// body it cannot echo prompt or response text. The
		// engine cancelled its own lifetime context, not this
		// handler's streamCtx, so the check is on the drained
		// error itself, not on the handler context's cause.
		if errors.Is(err, router.ErrStreamLifetimeExceeded) {
			_, _ = w.Write(anthropicStreamLifetimeEvent(err))
			flush()
			// No trailer on a capped stream — record it
			// server-side as a timeout.
			s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), http.StatusGatewayTimeout)
			return err
		}
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", `{"type":"error","error":{"type":"api_error","message":"upstream provider error"}}`)
		flush()
		// Committed-error stream: no trailer (A8 rule) — record
		// it server-side with the engine-error status.
		s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), s.statusForEngineError(err))
		return err
	}
	// Response sanitization cut (B3-22, block mode): the engine
	// ignored the callback error and returned nil, so the error
	// block above never ran — but the redactor still cut the
	// stream, and it must end with a refusal event, never a
	// normal message_stop. Same check as handleChatStream.
	if respRedactor != nil && respRedactor.Blocked() {
		return cutByResponseSanitization()
	}
	if resp == nil {
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", `{"type":"error","error":{"type":"api_error","message":"upstream provider error"}}`)
		flush()
		s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), http.StatusBadGateway)
		return fmt.Errorf("relay: nil engine response on stream")
	}
	// Stream redaction (M1.7): emit the redactor's withheld tail
	// — an unclosed PEM block as one placeholder — as a final
	// text delta before the tool-use blocks and message_delta.
	if tail := redactor.Flush(); tail != "" {
		if err := em.TextDelta(tail); err != nil {
			return err
		}
		flush()
	}
	for _, tc := range resp.ToolCalls {
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if err := em.ToolUseBlock(tc.ID, tc.Function.Name, args); err != nil {
			return err
		}
		flush()
	}
	stopReason := relay.MapFinishReasonToAnthropic(resp.FinishReason)
	if err := em.Finish(stopReason, resp.Usage); err != nil {
		return err
	}
	flush()
	// Telemetry trailer (M2.1): the values that were not known
	// when the stream's headers were sent — cost, tokens,
	// fallback — as a final SSE comment after message_stop
	// (comments are legal anywhere in an SSE stream and Anthropic
	// clients ignore them), and in the log row.
	meta := s.responseMeta(ctx, req, resp, start)
	s.writeStreamTrailer(w, ctx, meta)
	flush()
	s.logResponseMeta(meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	return nil
}

// anthropicToChatRequest converts a canonical (already-translated)
// Anthropic request into the gateway's provider.ChatRequest. Content
// arrives as a JSON string (plain text) or an array of parts (text /
// image_url); tool calls, tool results and tool definitions map
// 1:1 — provider aliases wire's types, so no field is re-typed.
func anthropicToChatRequest(wreq *wire.ChatCompletionRequest) provider.ChatRequest {
	req := provider.ChatRequest{
		Model:     wreq.Model,
		Stream:    wreq.Stream,
		MaxTokens: wreq.MaxTokens,
		Stop:      append([]string(nil), wreq.Stop...),
		Tools:     append([]provider.ToolDefinition(nil), wreq.Tools...),
	}
	if wreq.Temperature != nil {
		req.Temperature = *wreq.Temperature
	}
	if wreq.TopP != nil {
		req.TopP = *wreq.TopP
	}
	if wreq.ToolChoice != nil {
		if b, err := json.Marshal(wreq.ToolChoice); err == nil {
			req.ToolChoice = b
		}
	}
	for _, m := range wreq.Messages {
		pm := provider.Message{
			Role:       m.Role,
			ToolCalls:  append([]provider.ToolCall(nil), m.ToolCalls...),
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		raw := strings.TrimSpace(string(m.Content))
		switch {
		case raw == "" || raw == "null":
			// No content (assistant message carrying only tool calls).
		case strings.HasPrefix(raw, `"`):
			var s string
			if err := json.Unmarshal(m.Content, &s); err == nil {
				pm.Content = s
			}
		case strings.HasPrefix(raw, "["):
			var parts []provider.ContentPart
			if err := json.Unmarshal(m.Content, &parts); err == nil {
				pm.Parts = parts
			}
		}
		req.Messages = append(req.Messages, pm)
	}
	return req
}
