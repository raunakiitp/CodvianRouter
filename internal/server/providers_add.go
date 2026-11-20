// Real provider-add endpoint (POST /api/providers) and the live
// catalog feed (GET /api/catalog) for the dashboard's add-provider
// form.
//
// POST /api/providers reuses the exact code path the CLI
// (`provider add` / `provider add-catalog`) uses — Registry.AddContext
// for a fully-custom base URL, Registry.AddCatalogWithOptions for a
// catalog ID (+ optional endpoint/variant/base-URL override) — so the
// key is tested live against the upstream before anything is stored,
// ToS-grey entries are refused, entries with no known base URL are
// refused, and templated base URLs need a concrete override. The
// registry call mutates the live in-memory registry AND persists
// through its Store (in production the config file's Providers list),
// so the new provider routes immediately with no restart, and
// survives one.
//
// The response never carries a secret: no provider API key and no
// base URL (a base URL can carry embedded credentials — the same
// rule the settings export follows). Error statuses are distinct
// per cause: 400 validation, 401 gateway-key auth (guard) or
// upstream-rejected provider key, 403 ToS-grey refused, 404 unknown
// catalog id, 409 duplicate base URL, 502 upstream unreachable /
// key-test failure, 503 no registry wired, 500 persist failure.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ProviderAdder is the registry surface POST /api/providers needs:
// the same two add operations the CLI uses. *provider.Registry
// satisfies it; tests may inject a fake.
type ProviderAdder interface {
	AddCatalogWithOptions(ctx context.Context, catalogID, apiKey string, opts provider.CatalogAddOptions) (*provider.Provider, error)
	AddContext(ctx context.Context, name, baseURL, apiKey string) (*provider.Provider, error)
}

// WithProviderAdder wires the adder used by POST /api/providers
// directly. WithProviderRegistry also wires it from the registry;
// use this option only when the adder must differ from the model
// registry (tests).
func WithProviderAdder(a ProviderAdder) Option {
	return func(s *Server) { s.providerAdder = a }
}

// ProviderAddedHook runs after POST /api/providers has added a
// provider to the live registry and persisted it. Production wires
// it in main.go: it builds the provider's upstream client and
// registers it with the routing engine, so the new provider routes
// immediately with no restart. A hook failure does NOT fail the
// add (the provider is already stored — a retry would only hit a
// duplicate): the handler answers 201 with routing_live=false and
// the note, so the dashboard says honestly that a restart is
// needed to route through it. Nil hook (tests, servers without an
// engine) means routing_live=false with the same honest note.
type ProviderAddedHook func(ctx context.Context, p *provider.Provider) error

// WithProviderAddedHook wires the hook run after a successful
// provider add (see ProviderAddedHook).
func WithProviderAddedHook(h ProviderAddedHook) Option {
	return func(s *Server) { s.providerAddedHook = h }
}

// maxAddProviderBody caps a POST /api/providers request body (64 KiB:
// a catalog id, a base URL and two keys — nothing here is large).
const maxAddProviderBody = 64 << 10

// addProviderRequest is the POST /api/providers body. Exactly one
// mode is used: catalog_id (catalog path; base_url is then only an
// override) or base_url (fully-custom path; name optional).
// Models (CHECKLIST 283 (b)) is accepted, validated (every entry
// a non-empty string), and then explicitly NOT stored: the
// provider's model list always comes from the live key test's
// /v1/models probe, so a client-supplied list can never disagree
// with what the upstream actually serves. The 201 response says
// so in ModelsNote instead of silently dropping the field.
type addProviderRequest struct {
	CatalogID string   `json:"catalog_id"`
	APIKey    string   `json:"api_key"`
	BaseURL   string   `json:"base_url"`
	Endpoint  string   `json:"endpoint"`
	Variant   string   `json:"variant"`
	Name      string   `json:"name"`
	Models    []string `json:"models"`
}

// addedProviderResponse is the 201 payload for a created provider:
// identity and wiring facts only — never the key, never the base URL.
// RoutingLive reports whether the new provider routes immediately
// (the engine took its client); false carries a RoutingNote saying
// a restart is needed instead of silently pretending otherwise.
type addedProviderResponse struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	WireFormat   string   `json:"wire_format"`
	Verification string   `json:"verification"`
	CatalogID    string   `json:"catalog_id,omitempty"`
	AuthEndpoint string   `json:"auth_endpoint,omitempty"`
	AuthVariant  string   `json:"auth_variant,omitempty"`
	Models       []string `json:"models"`
	Enabled      bool     `json:"enabled"`
	HealthStatus string   `json:"health_status"`
	RoutingLive  bool     `json:"routing_live"`
	RoutingNote  string   `json:"routing_note,omitempty"`
	// ModelsNote is set only when the add request carried a
	// `models` field: it says the supplied list was accepted but
	// not stored, because the provider's models come from the
	// upstream /v1/models probe (see addProviderRequest). Absent
	// when no `models` field was sent.
	ModelsNote string `json:"models_note,omitempty"`
}

func addedProviderFrom(p *provider.Provider, routingLive bool, routingNote string) addedProviderResponse {
	models := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	verification := p.Verification
	if verification == "" {
		verification = string(catalog.VerificationUnverified)
	}
	return addedProviderResponse{
		ID: p.ID, Name: p.Name, WireFormat: p.WireFormat,
		Verification: verification, CatalogID: p.CatalogID,
		AuthEndpoint: p.AuthEndpoint, AuthVariant: p.AuthVariant,
		Models: models, Enabled: p.Enabled,
		HealthStatus: "unknown", // never probed yet at add time
		RoutingLive:  routingLive, RoutingNote: routingNote,
	}
}

// handleAddProvider serves POST /api/providers (Tier 2: gateway-key
// auth always required — see guard.go). It validates the request,
// adds the provider through the registry (live key test against the
// upstream, then persist), and returns the created provider.
func (s *Server) handleAddProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.providerAdder == nil {
		writeError(w, http.StatusServiceUnavailable, "provider registry is not configured")
		return
	}
	var req addProviderRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAddProviderBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		// Generic message only: the decode error can quote the
		// request body, which carries the provider API key.
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.CatalogID = strings.TrimSpace(req.CatalogID)
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	req.Variant = strings.TrimSpace(req.Variant)
	req.Name = strings.TrimSpace(req.Name)

	// Exactly one mode: catalog id, or a custom base URL.
	if req.CatalogID != "" && req.BaseURL != "" {
		// Both is allowed: base_url is the catalog override (proxy /
		// mirror / per-resource URL), exactly as the CLI flag does.
	} else if req.CatalogID == "" && req.BaseURL == "" {
		writeError(w, http.StatusBadRequest, "either catalog_id or base_url is required")
		return
	}
	if req.CatalogID != "" && req.Name != "" {
		writeError(w, http.StatusBadRequest, "name is only used for custom providers; a catalog provider takes its name from the catalog entry")
		return
	}
	if req.CatalogID == "" && req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "api_key is required for a custom provider")
		return
	}
	// A supplied `models` list is validated (every entry a
	// non-empty string) and then explicitly not stored — the
	// provider's models come from the key test's /v1/models
	// probe. An empty entry is a client bug, not a reason to
	// silently drop the whole list.
	for _, m := range req.Models {
		if strings.TrimSpace(m) == "" {
			writeError(w, http.StatusBadRequest, "models entries must be non-empty strings")
			return
		}
	}

	var p *provider.Provider
	var err error
	if req.CatalogID != "" {
		p, err = s.providerAdder.AddCatalogWithOptions(r.Context(), req.CatalogID, req.APIKey, provider.CatalogAddOptions{
			Endpoint: req.Endpoint, Variant: req.Variant, BaseURL: req.BaseURL,
		})
	} else {
		p, err = s.providerAdder.AddContext(r.Context(), req.Name, req.BaseURL, req.APIKey)
	}
	if err != nil {
		// Log the failure without secrets: registry errors never
		// carry the key, and the redactor strips anything shaped
		// like one anyway.
		s.logger.Printf("provider add failed: %v", guardrails.RedactLogLine(err.Error()))
		status, msg := classifyAddError(err)
		writeError(w, status, msg)
		return
	}
	// The provider is live in the registry and persisted. Make it
	// route immediately when an engine hook is wired; a hook
	// failure (or no hook) never fails the add — the response says
	// honestly that a restart is needed to route through it.
	routingLive := false
	routingNote := "added and saved, but the live engine has no client for it yet — restart the router to route through it"
	if s.providerAddedHook != nil {
		if hookErr := s.providerAddedHook(r.Context(), p); hookErr != nil {
			s.logger.Printf("provider add: engine hook failed for %s: %v", p.ID, guardrails.RedactLogLine(hookErr.Error()))
		} else {
			routingLive = true
			routingNote = ""
		}
	}
	// Live-added models join the auto-router's tiers only once
	// they route live: before the hook succeeds there is no
	// client for them, so an auto/* decision resolving to one
	// would fail on every request until a restart. Registering
	// here (and only here) makes the new models tier candidates
	// immediately — the ranker then orders them by their real
	// cost + measured latency/success + measured quality, exactly
	// as for config-listed models. A provider with no model list
	// (chat-only gateway) registers nothing. The counterpart is
	// DELETE /api/providers/{id} (see providers_delete.go):
	// removing a provider unregisters its models from tiers
	// (only models no remaining provider serves), so tier
	// registration is no longer append-only.
	if routingLive && s.autoRouter != nil {
		modelIDs := make([]string, 0, len(p.Models))
		for _, m := range p.Models {
			if m.ID != "" {
				modelIDs = append(modelIDs, m.ID)
			}
		}
		s.autoRouter.RegisterTierModels(modelIDs)
	}
	writeJSON(w, http.StatusCreated, func() addedProviderResponse {
		resp := addedProviderFrom(p, routingLive, routingNote)
		if len(req.Models) > 0 {
			resp.ModelsNote = fmt.Sprintf("models field accepted but not stored (%d supplied) — models come from the upstream /v1/models probe", len(req.Models))
		}
		return resp
	}())
}

// classifyAddError maps a registry add failure to a distinct HTTP
// status and an honest message. Messages for 400/403/404 pass the
// registry's own wording through (it names the catalog id or the
// missing base URL the operator typed); 401/409/502 use generic
// wording so no upstream body or base URL is echoed back.
func classifyAddError(err error) (int, string) {
	msg := err.Error()
	switch {
	case errors.Is(err, provider.ErrProviderExists):
		return http.StatusConflict, "a provider with this base URL is already configured"
	case strings.Contains(msg, "unknown catalog provider"):
		// The message names the catalog id the operator typed;
		// redact it first — a pasted key must not bounce back.
		return http.StatusNotFound, guardrails.RedactLogLine(msg)
	case strings.Contains(msg, "ToS-grey"):
		return http.StatusForbidden, guardrails.RedactLogLine(msg)
	}
	var remErr *catalog.RemovedProviderError
	if errors.As(err, &remErr) {
		// A removed provider is a hard refusal, like ToS-grey:
		// distinct 403 with the removal reason, never a generic 500.
		return http.StatusForbidden, guardrails.RedactLogLine(remErr.Error())
	}
	var upErr *provider.UpstreamError
	if errors.As(err, &upErr) {
		switch upErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return http.StatusUnauthorized, fmt.Sprintf("upstream rejected the provider API key (HTTP %d) — check the key and try again", upErr.StatusCode)
		case http.StatusTooManyRequests:
			return http.StatusBadGateway, "upstream rate-limited the key test (HTTP 429) — try again shortly"
		default:
			return http.StatusBadGateway, fmt.Sprintf("upstream error during the key test (HTTP %d)", upErr.StatusCode)
		}
	}
	var rlErr *provider.RateLimitError
	if errors.As(err, &rlErr) {
		return http.StatusBadGateway, "upstream rate-limited the key test (HTTP 429) — try again shortly"
	}
	var qErr *provider.QuotaError
	if errors.As(err, &qErr) {
		return http.StatusBadGateway, "upstream reports exhausted quota for this key"
	}
	switch {
	case strings.Contains(msg, "key test failed"), strings.Contains(msg, "fetching model list"):
		// Network failure, DNS failure, TLS failure, or an upstream
		// that answers but speaks no supported wire format: the
		// key test is the only thing that can tell them apart, and
		// it failed — say so rather than blaming the key.
		return http.StatusBadGateway, "upstream unreachable or key test failed — check the base URL and network, then try again"
	case strings.Contains(msg, "persisting provider"):
		return http.StatusInternalServerError, "provider was verified but could not be saved — check the config file"
	case strings.Contains(msg, "API key is empty"), strings.Contains(msg, "base URL"),
		strings.Contains(msg, "credential exchange"), strings.Contains(msg, "not supported yet"),
		strings.Contains(msg, "templated"):
		return http.StatusBadRequest, msg
	default:
		return http.StatusInternalServerError, "could not add provider"
	}
}

// ---------------------------------------------------------------------------
// Live catalog feed (GET /api/catalog).
// ---------------------------------------------------------------------------

// catalogEntryView is one catalog entry for the dashboard's
// add-provider selector: identity and addability facts only — no
// base URL (it can carry embedded credentials in templated form,
// and the selector never needs it: the server resolves it at add
// time), no key material of any kind. HasFreeTier / Category are
// catalog facts the dashboard groups and filters on (M10-2 Free
// Tier group, category filter): they come straight from the
// catalog entry, never invented. FreeTierNote is deliberately NOT
// serialized: 6 notes embed upstream URLs, and this feed carries
// no URL of any kind (TestCatalogFeed enforces it) — the note is
// display prose the selector never needs. Routable is the computed
// concrete-base flag (catalog.Provider.Routable): true only when at
// least one base URL is a concrete http(s) URL — a templated-only
// entry is not routable as-is, even though HasBaseURL is true.
type catalogEntryView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Alias            string `json:"alias,omitempty"`
	Verification     string `json:"verification"`
	ToSGrey          bool   `json:"tos_grey"`
	HasBaseURL       bool   `json:"has_base_url"`
	TemplatedBaseURL bool   `json:"templated_base_url"`
	Routable         bool   `json:"routable"`
	HasFreeTier      bool   `json:"has_free_tier"`
	Category         string `json:"category"`
}

type catalogResponse struct {
	Providers []catalogEntryView `json:"providers"`
	Total     int                `json:"total"`
	FreeTier  freeTierView       `json:"free_tier"`
}

// freeTierPoolView is one free-tier pool for the catalog feed: the
// pool name, its member count, the docs-verified share, and the
// member provider IDs in catalog order. IDs and counts only — no
// URLs, no FreeTierNote (notes embed upstream URLs, and this feed
// carries no URL of any kind: TestCatalogFeed enforces it).
type freeTierPoolView struct {
	Name         string   `json:"name"`
	Count        int      `json:"count"`
	VerifiedDocs int      `json:"verified_docs"`
	Providers    []string `json:"providers"`
}

// freeTierView is the free-tier block of the catalog feed: the
// honest headline counts from catalog.SummarizeFreeTier, the
// per-category pools from catalog.FreeTierPools, and the display
// ranking from catalog.FreeTierRanking. IDs and counts only —
// never an invented token budget, never a URL.
type freeTierView struct {
	Total        int                `json:"total"`
	VerifiedDocs int                `json:"verified_docs"`
	WithNote     int                `json:"with_note"`
	Pools        int                `json:"pools"`
	PoolList     []freeTierPoolView `json:"pool_list"`
	Ranking      []string           `json:"ranking"`
}

// buildFreeTierView assembles the free-tier block from the live
// catalog — never a hard-coded list.
func buildFreeTierView() freeTierView {
	summary := catalog.SummarizeFreeTier()
	pools := catalog.FreeTierPools()
	poolViews := make([]freeTierPoolView, 0, len(pools))
	for _, p := range pools {
		ids := make([]string, 0, len(p.Providers))
		for _, prov := range p.Providers {
			ids = append(ids, prov.ID)
		}
		poolViews = append(poolViews, freeTierPoolView{
			Name: p.Name, Count: len(p.Providers),
			VerifiedDocs: p.VerifiedDocs, Providers: ids,
		})
	}
	ranked := catalog.FreeTierRanking()
	ranking := make([]string, 0, len(ranked))
	for _, p := range ranked {
		ranking = append(ranking, p.ID)
	}
	return freeTierView{
		Total: summary.Total, VerifiedDocs: summary.VerifiedDocs,
		WithNote: summary.WithNote, Pools: summary.Pools,
		PoolList: poolViews, Ranking: ranking,
	}
}

// handleCatalog serves GET /api/catalog: the full live catalog for
// the dashboard's add-provider selector, straight from the catalog
// package — never a hard-coded list. ToS-grey entries are included
// and flagged, so the selector can refuse them with the reason
// instead of hiding that they exist; the server refuses them again
// at add time. The free_tier block (summary counts, per-category
// pools, display ranking) comes from the same live catalog via
// buildFreeTierView — IDs and counts only, no URLs, no notes.
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	entries := catalog.All()
	out := make([]catalogEntryView, 0, len(entries))
	for _, e := range entries {
		hasBase := false
		templated := false
		for _, u := range e.BaseURLs {
			if strings.TrimSpace(u) == "" {
				continue
			}
			hasBase = true
			if strings.ContainsAny(u, "{}") {
				templated = true
			}
		}
		out = append(out, catalogEntryView{
			ID: e.ID, Name: e.Name, Alias: e.Alias,
			Verification: string(e.Verification), ToSGrey: e.ToSGrey,
			HasBaseURL: hasBase, TemplatedBaseURL: templated,
			Routable:    e.Routable(),
			HasFreeTier: e.HasFreeTier, Category: string(e.Category),
		})
	}
	writeJSON(w, http.StatusOK, catalogResponse{Providers: out, Total: len(out), FreeTier: buildFreeTierView()})
}
