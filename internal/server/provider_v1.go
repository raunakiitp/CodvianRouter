// Per-provider v1 surfaces (M8.12, PARITY Area 14 §14a "Per-provider"
// row): address ONE provider explicitly instead of the auto-routed
// chain.
//
// Routes (registration is the coordinator's step — see
// RegisterProviderV1Routes below):
//
//   - POST /v1/providers/{provider}/chat/completions — scope chat
//   - GET  /v1/providers/{provider}/models            — scope models
//   - GET  /v1/providers/{provider}/status            — scope models
//
// The chat route is a PIN, not a second routing model: the handler
// injects routing.only=[provider] into the request body and then
// delegates to handleChatCompletions, so the request runs the SAME
// engine attempt machinery as normal routing — failover within the
// pinned provider's key pool, breaker, cooldowns, budgets, guardrails
// and ledger. A pinned request that bypassed the engine would be a
// defect, not a feature. A client-supplied routing contract that
// names a different provider (only/order/exclude naming the pin,
// or a routing group) is a 400 naming the conflict, never silently
// rewritten.
//
// A provider that is known but not usable fails honestly with a 4xx
// naming the gap — unknown provider 404; catalog-known but
// unconfigured, disabled, missing base URL, or keyless 422 — and is
// NEVER silently rerouted to a different provider.
//
// Guard entries + server.go registration (coordinator step —
// guard.go and server.go are outside this slice's owned files):
//
//	{Method: "POST", Path: "/v1/providers/{provider}/chat/completions", Tier: TierManagement, Description: "Chat completions pinned to one provider — gateway-key auth required.", Scopes: []string{"chat"}},
//	{Method: "GET", Path: "/v1/providers/{provider}/models", Tier: TierManagement, Description: "Model list for one provider — gateway-key auth required.", Scopes: []string{"models"}},
//	{Method: "GET", Path: "/v1/providers/{provider}/status", Tier: TierManagement, Description: "Status for one provider (configured, key count, breaker, cooling) — gateway-key auth required.", Scopes: []string{"models"}},
//	s.register(mux, "POST", "/v1/providers/{provider}/chat/completions", s.handleProviderChatCompletions)
//	s.register(mux, "GET", "/v1/providers/{provider}/models", s.handleProviderModels)
//	s.register(mux, "GET", "/v1/providers/{provider}/status", s.handleProviderStatus)
//	s.RegisterProviderV1Routes(mux) // equivalent one-liner, once the
//	// guard entries above exist — register() panics without them.
//
// NOT built here (PARITY row remainder, follow-up slices): pinned
// /embeddings, /images/generations, /voices, /voices/[voiceId] and
// /limits. There is no live per-provider limit source in this repo
// (the only configured limit-like value, quota_hint, is reported on
// /status when set) — a /limits endpoint would have to invent
// limits, which the no-invention rule forbids.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// RegisterProviderV1Routes registers the M8.12 per-provider routes
// on mux. It must be called from New() AFTER the guard-table entries
// listed in the file doc exist — s.register panics on an
// unclassified route, by design, so calling this before the
// coordinator adds those entries fails loudly at startup instead of
// landing an unguarded route.
func (s *Server) RegisterProviderV1Routes(mux *http.ServeMux) {
	s.register(mux, "POST", "/v1/providers/{provider}/chat/completions", s.handleProviderChatCompletions)
	s.register(mux, "GET", "/v1/providers/{provider}/models", s.handleProviderModels)
	s.register(mux, "GET", "/v1/providers/{provider}/status", s.handleProviderStatus)
}

// providerV1Info is the resolved identity of one provider for the
// per-provider surface: where it came from and what is known about
// it. It carries no secrets and no base URL — the status feed
// reports only whether a base URL is configured, never the URL.
type providerV1Info struct {
	ID           string
	Name         string
	Configured   bool // a live registry or config provider exists
	Enabled      bool
	BaseURLKnown bool
	KeyCount     int
	Models       []provider.Model // stored model list, when the source has one
	QuotaHint    float64
	CatalogKnown bool
}

// resolveProviderV1 resolves a provider id from the live registry
// first (providers added/changed at runtime land there), the config
// snapshot second (the startup providers), and the static catalog
// third (known but unconfigured — usable for an honest 422, never
// for routing). Exact match first, then a case-insensitive match:
// provider ids are lowercase kebab-case, and a caller typing
// "OpenAI" names the same provider as "openai". A display Name is
// accepted as a fallback (B2-29, :600): a custom provider is
// addressed by name ("fakeprov"), not by its generated "prov_…"
// id — but only when the input matched no ID, so an existing ID
// prefix resolves exactly as before.
func (s *Server) resolveProviderV1(id string) (providerV1Info, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return providerV1Info{}, false
	}
	if s.providerKeyManager != nil {
		if p, ok := s.providerKeyManager.Get(id); ok && p != nil {
			return providerV1InfoFromProvider(p, true), true
		}
		if lower := strings.ToLower(id); lower != id {
			if p, ok := s.providerKeyManager.Get(lower); ok && p != nil {
				return providerV1InfoFromProvider(p, true), true
			}
		}
	}
	if s.cfg != nil {
		for i := range s.cfg.Providers {
			c := s.cfg.Providers[i]
			if c.ID == id || strings.EqualFold(c.ID, id) {
				return providerV1InfoFromProvider(provider.ProviderFromConfig(c), true), true
			}
		}
	}
	if p, ok := catalog.ByID(id); ok {
		return providerV1InfoFromCatalog(p), true
	}
	if p, ok := catalog.ByAlias(id); ok {
		return providerV1InfoFromCatalog(p), true
	}
	if lower := strings.ToLower(id); lower != id {
		if p, ok := catalog.ByID(lower); ok {
			return providerV1InfoFromCatalog(p), true
		}
		if p, ok := catalog.ByAlias(lower); ok {
			return providerV1InfoFromCatalog(p), true
		}
	}
	// Name fallback (B2-29, :600): the input matched no ID, so
	// try display Names in the same source order (live registry,
	// config snapshot, static catalog), exact first, then
	// case-insensitive. An empty name never matches. A registry
	// hit returns the full provider (its generated ID is what
	// the pin uses), so a name prefix resolves to the same
	// provider an ID prefix would.
	if s.providerKeyManager != nil {
		listed := s.providerKeyManager.List()
		for _, p := range listed {
			if p != nil && strings.TrimSpace(p.Name) != "" && p.Name == id {
				return providerV1InfoFromProvider(p, true), true
			}
		}
		for _, p := range listed {
			if p != nil && strings.TrimSpace(p.Name) != "" && strings.EqualFold(p.Name, id) {
				return providerV1InfoFromProvider(p, true), true
			}
		}
	}
	if s.cfg != nil {
		for i := range s.cfg.Providers {
			c := s.cfg.Providers[i]
			if strings.TrimSpace(c.Name) != "" && c.Name == id {
				return providerV1InfoFromProvider(provider.ProviderFromConfig(c), true), true
			}
		}
		for i := range s.cfg.Providers {
			c := s.cfg.Providers[i]
			if strings.TrimSpace(c.Name) != "" && strings.EqualFold(c.Name, id) {
				return providerV1InfoFromProvider(provider.ProviderFromConfig(c), true), true
			}
		}
	}
	all := catalog.All()
	for _, p := range all {
		if strings.TrimSpace(p.Name) != "" && p.Name == id {
			return providerV1InfoFromCatalog(p), true
		}
	}
	for _, p := range all {
		if strings.TrimSpace(p.Name) != "" && strings.EqualFold(p.Name, id) {
			return providerV1InfoFromCatalog(p), true
		}
	}
	return providerV1Info{}, false
}

func providerV1InfoFromProvider(p *provider.Provider, configured bool) providerV1Info {
	return providerV1Info{
		ID:           p.ID,
		Name:         p.Name,
		Configured:   configured,
		Enabled:      p.Enabled,
		BaseURLKnown: strings.TrimSpace(p.BaseURL) != "",
		KeyCount:     len(p.EffectiveKeys()),
		Models:       p.Models,
		QuotaHint:    p.QuotaHint,
	}
}

func providerV1InfoFromCatalog(p catalog.Provider) providerV1Info {
	return providerV1Info{
		ID:           p.ID,
		Name:         p.Name,
		Configured:   false,
		Enabled:      false,
		BaseURLKnown: len(p.BaseURLs) > 0,
		KeyCount:     0,
		CatalogKnown: true,
	}
}

// providerV1Segment extracts the {provider} segment from a
// /v1/providers/{provider}<suffix> path. It parses the path
// itself (rather than r.PathValue) so the handlers also work when
// invoked directly in tests, without a ServeMux pattern.
func providerV1Segment(r *http.Request, suffix string) (string, bool) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/providers/")
	if rest == r.URL.Path || !strings.HasSuffix(rest, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(rest, suffix)
	id = strings.Trim(id, "/")
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// checkProviderV1Routable enforces the honest-failure rule for the
// chat route: an unknown provider is a 404, a known-but-unusable
// provider a 422 naming the gap. It returns false when it has
// already answered.
func (s *Server) checkProviderV1Routable(w http.ResponseWriter, info providerV1Info, found bool, id string) bool {
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown provider %q", id))
		return false
	}
	if !info.Configured {
		if !info.BaseURLKnown {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("provider %q is known in the catalog but has no base URL and no API key configured — configure it before routing to it", info.ID))
		} else {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("provider %q is known in the catalog but has no API key configured — add a key before routing to it", info.ID))
		}
		return false
	}
	if !info.Enabled {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("provider %q is disabled", info.ID))
		return false
	}
	if !info.BaseURLKnown {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("provider %q has no base URL configured", info.ID))
		return false
	}
	if info.KeyCount == 0 {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("provider %q has no API key configured — add a key before routing to it", info.ID))
		return false
	}
	return true
}

// pinProviderV1Body injects routing.only=[providerID] into the raw
// request body and returns the rewritten body. A client-supplied
// routing contract that conflicts with the pin is a 400 naming the
// conflict: only/order naming any other provider, exclude naming
// the pinned provider, or a routing group (the group would select
// a different chain). Non-conflicting controls (sort, max_price,
// data_collection, exclude of other providers) pass through
// unchanged — they narrow within the pinned provider.
func pinProviderV1Body(raw []byte, providerID string) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("invalid request body")
	}
	routing, _ := body["routing"].(map[string]any)
	if body["routing"] != nil && routing == nil {
		return nil, fmt.Errorf("routing must be an object")
	}
	if routing == nil {
		routing = map[string]any{}
		body["routing"] = routing
	}
	if g, _ := routing["group"].(string); strings.TrimSpace(g) != "" {
		return nil, fmt.Errorf("routing.group %q conflicts with the pinned provider %q: a pinned request already selects its chain", g, providerID)
	}
	nameList := func(field string) ([]string, error) {
		v, ok := routing[field]
		if v == nil {
			return nil, nil
		}
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("routing.%s must be an array of provider names", field)
		}
		out := make([]string, 0, len(list))
		for _, e := range list {
			s, ok := e.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("routing.%s must contain only non-empty provider names", field)
			}
			out = append(out, s)
		}
		return out, nil
	}
	for _, field := range []string{"only", "order"} {
		names, err := nameList(field)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if !strings.EqualFold(n, providerID) {
				return nil, fmt.Errorf("routing.%s names provider %q, which conflicts with the pinned provider %q", field, n, providerID)
			}
		}
	}
	if names, err := nameList("exclude"); err != nil {
		return nil, err
	} else {
		for _, n := range names {
			if strings.EqualFold(n, providerID) {
				return nil, fmt.Errorf("routing.exclude names the pinned provider %q", providerID)
			}
		}
	}
	// The pin is the whole contract: only=[provider]. An order
	// naming exactly the pin is redundant — drop it so the
	// contract the engine sees is unambiguous (order+sort would
	// also be a contract error downstream).
	delete(routing, "order")
	routing["only"] = []string{providerID}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("invalid request body")
	}
	return out, nil
}

// applyPrefixedModelRouting translates a "provider/model" model
// string on the plain chat path into routing.only=[provider]
// (B2-29, PARITY.md Area 2 row :600 — "prefixed provider/model
// never rerouted"). The engine enforces the narrowing for any
// caller that sets routing.only (applyContract narrows the chain
// before the loop, and the loop guard in
// internal/router/failover_policy.go re-checks every attempt),
// but the plain chat path never set it from the model prefix,
// so a prefixed model could still fail over to another provider.
// This closes that gap at the point the model string first
// enters the server: the request is narrowed to the prefixed
// provider before any guardrail, budget or provider call, so
// both the streaming and non-streaming branches (both run from
// handleChatCompletions with the same request) are covered.
//
// Fail-open by design: a model with no "/" (bare model id), an
// empty provider or model part, the "auto/" virtual prefix, or
// a prefix that names no known provider all leave the request
// untouched. "auto/" is engine auto-routing, not a provider —
// the catalog carries a system entry ID "auto"
// (CategorySystem), so the known-provider check below would
// wrongly match it, and narrowing to it would 400 in
// applyContract (no "auto" client exists). A client-supplied
// routing contract that conflicts with the pin is a 400 naming
// the conflict (same rule as pinProviderV1Body): routing.group
// set, routing.only/order naming another provider, or
// routing.exclude naming the pinned provider. Non-conflicting
// controls (sort, max_price, data_collection, exclude of other
// providers) pass through unchanged. An order naming exactly
// the pinned provider is redundant and dropped, so the contract
// the engine sees is unambiguous. The model string itself is
// rewritten to the bare id (B2-29 fix): chain matching compares
// bare model ids (offers() in cmd/codvian-router/main.go), so a
// prefixed model could never be served, and the wire model
// reaches the upstream verbatim — bare-id upstreams would 404
// the prefixed id. The provider is carried by routing.only,
// so nothing downstream needs the prefix.
func (s *Server) applyPrefixedModelRouting(req *provider.ChatRequest) error {
	prov, bare, ok := autoComboSplitPair(req.Model)
	if !ok {
		return nil
	}
	// The "auto/" virtual prefix is engine auto-routing, not a
	// provider (see above): never translate it.
	if strings.EqualFold(prov, "auto") {
		return nil
	}
	info, known := s.resolveProviderV1(prov)
	if !known {
		// Not a known provider (no key, no config entry, no
		// catalog entry): leave the request exactly as sent.
		// The engine fails such a request honestly downstream
		// rather than this layer inventing a narrowing.
		return nil
	}
	pinned := info.ID
	rc := req.Routing
	if rc == nil {
		req.Routing = &provider.RequestRouting{Only: []string{pinned}}
		req.Model = bare
		return nil
	}
	if strings.TrimSpace(rc.Group) != "" {
		return fmt.Errorf("routing.group %q conflicts with the prefixed provider %q: a prefixed model already selects its provider", rc.Group, pinned)
	}
	for _, n := range rc.Only {
		if !strings.EqualFold(strings.TrimSpace(n), pinned) {
			return fmt.Errorf("routing.only names provider %q, which conflicts with the prefixed provider %q", n, pinned)
		}
	}
	for _, n := range rc.Order {
		if !strings.EqualFold(strings.TrimSpace(n), pinned) {
			return fmt.Errorf("routing.order names provider %q, which conflicts with the prefixed provider %q", n, pinned)
		}
	}
	for _, n := range rc.Exclude {
		if strings.EqualFold(strings.TrimSpace(n), pinned) {
			return fmt.Errorf("routing.exclude names the prefixed provider %q", pinned)
		}
	}
	// An order naming exactly the pin is redundant — drop it so
	// the contract the engine sees is unambiguous (order+sort
	// would also be a contract error downstream).
	rc.Order = nil
	rc.Only = []string{pinned}
	req.Model = bare
	return nil
}

// handleProviderChatCompletions serves POST
// /v1/providers/{provider}/chat/completions: resolve + gate the
// provider, pin routing.only to it, and delegate to
// handleChatCompletions — the same handler (and engine) as POST
// /v1/chat/completions, so every gate, guardrail, budget, failover
// and ledger row is identical.
func (s *Server) handleProviderChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, ok := providerV1Segment(r, "/chat/completions")
	if !ok {
		writeError(w, http.StatusNotFound, "unknown provider route")
		return
	}
	info, found := s.resolveProviderV1(id)
	if !s.checkProviderV1Routable(w, info, found, id) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(raw) > maxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	pinned, err := pinProviderV1Body(raw, info.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(pinned))
	r.ContentLength = int64(len(pinned))
	s.handleChatCompletions(w, r)
}

// handleProviderModels serves GET /v1/providers/{provider}/models:
// the provider's models in the GET /v1/models list shape. The live
// registry is the source when wired (its Models are the probed
// lists); otherwise the provider's stored model list. An
// unconfigured provider has no model list anywhere — 422 naming
// that, never an invented list. Unknown provider: 404.
func (s *Server) handleProviderModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, ok := providerV1Segment(r, "/models")
	if !ok {
		writeError(w, http.StatusNotFound, "unknown provider route")
		return
	}
	info, found := s.resolveProviderV1(id)
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown provider %q", id))
		return
	}
	if !info.Configured {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("provider %q is known in the catalog but has no API key configured, so no model list is available for it", info.ID))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	var models []Model
	if s.registry != nil {
		all, err := s.registry.Models(ctx)
		if err != nil {
			writeServerError(w, http.StatusBadGateway, "upstream provider error")
			return
		}
		for _, m := range all {
			if strings.EqualFold(m.ProviderID, info.ID) || strings.EqualFold(m.OwnedBy, info.ID) {
				models = append(models, m)
			}
		}
	} else {
		for _, pm := range info.Models {
			models = append(models, Model{ID: pm.ID, Object: "model", OwnedBy: info.ID, ProviderID: info.ID})
		}
	}
	if models == nil {
		models = []Model{}
	}
	if r.URL.Query().Get("include") != "deprecated" {
		kept := models[:0]
		for _, m := range models {
			if !m.Deprecated {
				kept = append(kept, m)
			}
		}
		for i := len(kept); i < len(models); i++ {
			models[i] = Model{}
		}
		models = kept
	}
	mf, ferr := parseModelFilters(r)
	if ferr != nil {
		writeError(w, http.StatusBadRequest, ferr.Error())
		return
	}
	mf.Provider = info.ID
	models = s.filterV1Models(models, mf)
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

// providerV1Status is the GET /v1/providers/{provider}/status
// payload. Every field is a measured or configured fact: counts
// from the live registry/config, breaker/cooling from the engine
// when it reports them ("unknown" when it does not — never an
// invented state). No base URL, no key material, no prices: an
// unknown price is absent, never $0.
type providerV1Status struct {
	Object           string   `json:"object"`
	Provider         string   `json:"provider"`
	Name             string   `json:"name,omitempty"`
	Configured       bool     `json:"configured"`
	Enabled          bool     `json:"enabled"`
	BaseURLKnown     bool     `json:"base_url_configured"`
	KeyCount         int      `json:"key_count"`
	ModelCount       *int     `json:"model_count,omitempty"`
	ModelsKnown      bool     `json:"models_known"`
	Breaker          string   `json:"breaker"`
	CoolingKeys      int      `json:"cooling_keys"`
	QuotaHint        *float64 `json:"quota_hint,omitempty"`
	UnconfiguredNote string   `json:"note,omitempty"`
}

// handleProviderStatus serves GET /v1/providers/{provider}/status.
// Unlike chat/models it also answers for a catalog-known but
// unconfigured provider (200 with configured:false) — status is a
// report, not a routing request, so the honest report is the
// useful answer. Unknown provider: 404.
func (s *Server) handleProviderStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, ok := providerV1Segment(r, "/status")
	if !ok {
		writeError(w, http.StatusNotFound, "unknown provider route")
		return
	}
	info, found := s.resolveProviderV1(id)
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown provider %q", id))
		return
	}
	st := providerV1Status{
		Object:       "provider.status",
		Provider:     info.ID,
		Name:         info.Name,
		Configured:   info.Configured,
		Enabled:      info.Enabled,
		BaseURLKnown: info.BaseURLKnown,
		KeyCount:     info.KeyCount,
		ModelsKnown:  false,
		Breaker:      "unknown",
	}
	if info.QuotaHint > 0 {
		q := info.QuotaHint
		st.QuotaHint = &q
	}
	if !info.Configured {
		st.UnconfiguredNote = fmt.Sprintf("provider %q is known in the catalog but has no API key configured", info.ID)
	}
	// Breaker state from the engine when it reports one; a
	// provider the engine does not report reads "unknown".
	if rep, ok := s.engine.(breakerReporter); ok && rep != nil {
		for _, bs := range rep.BreakerStates() {
			if strings.EqualFold(bs.ProviderID, info.ID) {
				if bs.Open {
					st.Breaker = "OPEN"
				} else {
					st.Breaker = "CLOSED"
				}
			}
		}
	}
	// Cooling keys from the engine when it reports them.
	if rep, ok := s.engine.(cooldownReporter); ok && rep != nil {
		seen := map[string]bool{}
		for _, t := range rep.CooldownTargets() {
			if strings.EqualFold(t.ProviderID, info.ID) && !seen[t.KeyID] {
				seen[t.KeyID] = true
				st.CoolingKeys++
			}
		}
	}
	// Model count: live registry first, stored list second,
	// unknown when neither source exists.
	if s.registry != nil {
		ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
		all, err := s.registry.Models(ctx)
		cancel()
		if err == nil {
			n := 0
			for _, m := range all {
				if strings.EqualFold(m.ProviderID, info.ID) || strings.EqualFold(m.OwnedBy, info.ID) {
					n++
				}
			}
			st.ModelCount = &n
			st.ModelsKnown = true
		}
	} else if info.Configured {
		n := len(info.Models)
		st.ModelCount = &n
		st.ModelsKnown = true
	}
	writeJSON(w, http.StatusOK, st)
}
