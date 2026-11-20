// Catalog filters, shortlist and live refresh (CHECKLIST 200).
//
// Filters (GET /v1/models and GET /dashboard/models share them):
//
//   - provider: a provider registry ID or display name (case-insensitive).
//     On /v1/models each model ID is listed once, under the first
//     provider offering it in registry order, so the filter matches
//     that listed provider; the dashboard feed lists per-(provider,
//     model) pairs and matches exactly.
//   - category: the catalog category of the model's provider
//     (api-key, free, local, no-auth, oauth, ...; "custom" for a
//     hand-added provider) — exact, lowercased.
//   - max_price: USD per 1M tokens, blended ((input+output)/2). A row
//     passes when its cheapest known blended price is <= the value.
//     A row with no known blended price is EXCLUDED, never assumed
//     cheap — the same rule the per-request routing.max_price
//     contract uses for price_unknown.
//   - min_context: minimum context window in tokens. A row with an
//     unknown window (0) is EXCLUDED, never assumed large.
//   - shortlist: shortlist=true lists only operator-pinned models.
//
// A malformed max_price / min_context answers 400 naming the
// parameter, never a silently unfiltered list.
//
// Shortlist (config `shortlist`): entries pin models to the top of
// both lists, in config order, marked shortlisted. An entry pins any
// row whose model ID equals the entry, and any row whose
// "providerID/modelID" equals the entry — so "gpt-4o" pins the model
// wherever it is offered, and "prov-a/gpt-4o" pins only that pair.
// An entry that is both (a model ID containing "/" offered by a
// provider whose ID is its prefix) pins both — the two readings are
// indistinguishable, and pinning both is the honest outcome.
//
// Refresh (POST /dashboard/models/refresh): re-probes every
// provider's model list live via provider.Registry.RefreshModels and
// returns one result per provider (ok / failed / skipped, with model
// counts and reasons). A failed provider keeps its last known list —
// a refresh never empties the catalog. Newly discovered models of a
// refreshed provider become auto-router tier candidates, exactly as
// live-added providers' models do (CHECKLIST 97).
package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// modelFilter is the parsed catalog filter set for one list request.
type modelFilter struct {
	Provider      string
	Category      string
	Capability    string
	MaxPrice      *float64
	MinContext    *int
	ShortlistOnly bool
}

// parseModelFilters parses the shared catalog query parameters.
// Unknown parameters are ignored; a malformed numeric filter is an
// error naming the parameter.
func parseModelFilters(r *http.Request) (modelFilter, error) {
	var f modelFilter
	q := r.URL.Query()
	f.Provider = strings.TrimSpace(q.Get("provider"))
	f.Category = strings.ToLower(strings.TrimSpace(q.Get("category")))
	f.Capability = strings.TrimSpace(q.Get("capability"))
	if v := strings.TrimSpace(q.Get("max_price")); v != "" {
		p, err := strconv.ParseFloat(v, 64)
		if err != nil || p < 0 {
			return f, fmt.Errorf("invalid max_price %q: must be a non-negative number (USD per 1M tokens, blended)", v)
		}
		f.MaxPrice = &p
	}
	if v := strings.TrimSpace(q.Get("min_context")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return f, fmt.Errorf("invalid min_context %q: must be a non-negative integer (tokens)", v)
		}
		f.MinContext = &n
	}
	switch strings.ToLower(strings.TrimSpace(q.Get("shortlist"))) {
	case "true", "1", "yes":
		f.ShortlistOnly = true
	case "", "false", "0", "no":
	default:
		return f, fmt.Errorf("invalid shortlist %q: must be true or false", q.Get("shortlist"))
	}
	return f, nil
}

// isShortlisted reports whether one catalog row is pinned by the
// operator's shortlist (see the package doc for the matching rule).
// With no shortlist configured nothing is pinned.
func (s *Server) isShortlisted(providerID, modelID string) bool {
	for _, e := range s.shortlist {
		if e == modelID {
			return true
		}
		if i := strings.Index(e, "/"); i > 0 && e[:i] == providerID && e[i+1:] == modelID {
			return true
		}
	}
	return false
}

// WithShortlist sets the operator's curated model shortlist (config
// `shortlist`): model IDs, or "providerID/modelID" pairs. A copy is
// kept, so later edits to the caller's slice cannot move the pins.
func WithShortlist(entries []string) Option {
	return func(s *Server) {
		s.shortlist = append([]string(nil), entries...)
	}
}

// rowBlendedPriceUSDPer1M resolves the blended price (USD per 1M
// tokens, (input+output)/2) of one catalog row for the max_price
// filter: the operator price book's resolved price for the
// (provider, model) pair first (pair, then model-level, then synced
// — see pairPrice), which handles a genuinely-free ($0) price
// correctly; else the row's own provider-reported legs when both are
// reported (> 0 — 0 means unknown on the row shape, never free).
// Unknown when neither is known: the caller excludes the row, never
// assumes it cheap.
func (s *Server) rowBlendedPriceUSDPer1M(providerID, modelID string, reportedIn, reportedOut float64) (float64, bool) {
	if p, src := s.pairPrice(providerID, modelID); src != "" {
		if b, ok := p.BlendedUSDPer1M(); ok {
			return b, true
		}
	}
	if reportedIn > 0 && reportedOut > 0 {
		return (reportedIn + reportedOut) / 2, true
	}
	return 0, false
}

// filterV1Models applies the parsed filters to a /v1/models list:
// provider, category, max_price, min_context, shortlist. It also
// marks shortlisted rows and sorts them first (stable — registry
// order otherwise).
func (s *Server) filterV1Models(models []Model, f modelFilter) []Model {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if f.Provider != "" && !strings.EqualFold(m.ProviderID, f.Provider) && !strings.EqualFold(m.OwnedBy, f.Provider) {
			continue
		}
		if f.Category != "" && strings.ToLower(m.Category) != f.Category {
			continue
		}
		if f.MaxPrice != nil {
			if b, ok := s.rowBlendedPriceUSDPer1M(m.ProviderID, m.ID, m.InputPriceUSD, m.OutputPriceUSD); !ok || b > *f.MaxPrice {
				continue
			}
		}
		if f.MinContext != nil {
			if m.ContextWindow <= 0 || m.ContextWindow < *f.MinContext {
				continue
			}
		}
		if f.Capability != "" && !modelMatchesCapability(m, f.Capability) {
			continue
		}
		m.Shortlisted = s.isShortlisted(m.ProviderID, m.ID)
		if f.ShortlistOnly && !m.Shortlisted {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Shortlisted && !out[j].Shortlisted
	})
	return out
}

func dashboardModelMatchesCapability(m dashboardModel, cap string) bool {
	cNorm := strings.ToLower(strings.TrimSpace(cap))
	switch cNorm {
	case "tools", "tool", "function_calling", "json_schema", "json", "structured_output":
		return m.Tools != nil && *m.Tools
	case "code", "coding":
		id := strings.ToLower(m.ID)
		return strings.Contains(id, "coder") || strings.Contains(id, "code")
	case "reasoning", "thinking":
		id := strings.ToLower(m.ID)
		return strings.Contains(id, "reasoner") || strings.Contains(id, "o1") || strings.Contains(id, "o3") || strings.Contains(id, "r1")
	case "vision", "image", "image_input":
		id := strings.ToLower(m.ID)
		return strings.Contains(id, "vision") || strings.Contains(id, "vl") || strings.Contains(id, "4o")
	default:
		return false
	}
}

// filterDashboardModels applies the same filters to dashboard feed
// rows (per-(provider, model) pairs). It marks shortlisted rows and
// sorts them first (stable — registry order otherwise).
func (s *Server) filterDashboardModels(models []dashboardModel, f modelFilter) []dashboardModel {
	out := make([]dashboardModel, 0, len(models))
	for _, m := range models {
		if f.Provider != "" && !strings.EqualFold(m.ProviderID, f.Provider) && !strings.EqualFold(m.ProviderName, f.Provider) {
			continue
		}
		if f.Category != "" && strings.ToLower(m.Category) != f.Category {
			continue
		}
		if f.MaxPrice != nil {
			if b, ok := s.rowBlendedPriceUSDPer1M(m.ProviderID, m.ID, m.InputPriceUSD, m.OutputPriceUSD); !ok || b > *f.MaxPrice {
				continue
			}
		}
		if f.MinContext != nil {
			if m.ContextWindow <= 0 || m.ContextWindow < *f.MinContext {
				continue
			}
		}
		if f.Capability != "" && !dashboardModelMatchesCapability(m, f.Capability) {
			continue
		}
		m.Shortlisted = s.isShortlisted(m.ProviderID, m.ID)
		if f.ShortlistOnly && !m.Shortlisted {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Shortlisted && !out[j].Shortlisted
	})
	return out
}

// modelRefresher re-probes provider model lists live. *provider.Registry
// satisfies it (see provider.Registry.RefreshModels).
type modelRefresher interface {
	RefreshModels(ctx context.Context) ([]provider.ModelRefreshResult, error)
}

// WithModelRefresher wires the live model-list refresher used by
// POST /dashboard/models/refresh. It is separate from
// WithProviderRegistry so tests can feed the refresh endpoint
// without a full registry.
func WithModelRefresher(r modelRefresher) Option {
	return func(s *Server) { s.modelRefresher = r }
}

// modelsRefreshResponse is the POST /dashboard/models/refresh
// payload: one honest result per provider, plus totals. A provider
// that failed keeps its last known list — its result says so, with
// the reason, instead of the catalog silently shrinking.
type modelsRefreshResponse struct {
	RefreshedAt time.Time                     `json:"refreshed_at"`
	Providers   []provider.ModelRefreshResult `json:"providers"`
	Total       int                           `json:"total"`
	OK          int                           `json:"ok"`
	Failed      int                           `json:"failed"`
	Skipped     int                           `json:"skipped"`
}

// handleModelsRefresh serves POST /dashboard/models/refresh: re-probe
// every provider's model list live and report per-provider success
// or failure. A persist failure after the probes answers 500 with
// the per-provider results and the persist error — never a success
// for a refresh that was not saved. With no refresher wired it
// answers 503, never a faked refresh.
func (s *Server) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.modelRefresher == nil {
		writeError(w, http.StatusServiceUnavailable, "model refresh is not available on this server")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	results, err := s.modelRefresher.RefreshModels(ctx)
	if results == nil {
		results = []provider.ModelRefreshResult{}
	}
	// Newly discovered models become auto-router tier candidates,
	// exactly as live-added providers' models do (CHECKLIST 97):
	// membership is candidacy only — the ranker still decides by
	// real cost/latency/quality signals, and no capability class
	// is guessed from a model ID. Only ok providers register:
	// a failed refresh kept the old list, so there is nothing new.
	if s.autoRouter != nil {
		for _, res := range results {
			if res.Status == provider.ModelRefreshOK && len(res.Models) > 0 {
				s.autoRouter.RegisterTierModels(res.Models)
			}
		}
	}
	resp := modelsRefreshResponse{RefreshedAt: time.Now().UTC(), Providers: results, Total: len(results)}
	for _, res := range results {
		switch res.Status {
		case provider.ModelRefreshOK:
			resp.OK++
		case provider.ModelRefreshFailed:
			resp.Failed++
		case provider.ModelRefreshSkipped:
			resp.Skipped++
		}
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   map[string]any{"message": err.Error()},
			"refresh": resp,
		})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
