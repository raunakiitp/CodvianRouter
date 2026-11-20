// Models surface (M8.10, Area 14 §14a): single-model detail, catalog
// query, and per-model health.
//
//   - GET /v1/models/{model} — one catalog entry: identity, provider,
//     context window, price when known (never invented), and whether
//     a base URL is configured for its provider (presence only — the
//     URL itself is never returned, like every other feed in this
//     server). Unknown models answer 404; unpriced models report
//     price unknown, never $0.
//   - GET /v1/models/query — the catalog filtered by provider,
//     free-tier-only, and capability. Only provider and
//     free-tier-only filter on data the catalog actually has.
//     Capability has no catalog data at all — the registry records
//     no per-model capability flags (tools/JSON/vision), see
//     provider.DashboardModel — so any capability value answers
//     400 naming the gap, never a silently unfiltered list.
//   - GET /v1/models/health — per-model health from the existing
//     health-probe ring. Probes are PROVIDER-level key tests
//     (health.Checker: one probe per provider, not per model), so
//     each model's status is its provider's last probe result and
//     the response says so (probe_scope "provider"). Unprobed
//     models report unknown with null probe fields — never a
//     fabricated uptime.
//
// Registration (guard table + mux) is the coordinator's job: these
// handlers carry no auth themselves, exactly like every other
// handler — the guard enforces TierManagement + scope "models"
// (the same scope as GET /v1/models) at registration. Proposed
// guard entries are listed in models_detail_test.go.
package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/health"
)

// m810ListModels returns the catalog exactly as GET /v1/models
// lists it (see handleModels): the registry's models, or the
// WithModels fallback when no registry is wired, plus the
// auto-router's virtual models (real request targets), with
// deprecated rows hidden unless includeDeprecated is set. It
// mirrors handleModels' sourcing because that handler cannot be
// shared without editing server.go; if handleModels' sourcing
// changes, this must change with it.
func (s *Server) m810ListModels(ctx context.Context, includeDeprecated bool) ([]Model, error) {
	var models []Model
	if s.registry != nil {
		var err error
		models, err = s.registry.Models(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		for _, id := range s.models {
			models = append(models, Model{ID: id, Object: "model", OwnedBy: "codvian-router"})
		}
	}
	if models == nil {
		models = []Model{}
	}
	if s.autoRouter != nil {
		for _, name := range autorouter.VirtualModels {
			vm := Model{ID: name, Object: "model", OwnedBy: "codvian-router"}
			switch name {
			case "auto/best-coding":
				vm.Capabilities = []string{"code"}
			case "auto/best-vision":
				vm.Capabilities = []string{"vision"}
				vm.Vision = true
			case "auto/best-reasoning":
				vm.Capabilities = []string{"reasoning"}
				vm.Reasoning = true
			}
			models = append(models, vm)
		}
		for _, name := range autorouter.IntentVirtualModelNames() {
			models = append(models, Model{ID: name, Object: "model", OwnedBy: "codvian-router"})
		}
	}
	if !includeDeprecated {
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
	return models, nil
}

// modelPriceDetail is the honest price of one catalog row. Known is
// false when no price exists at any level — the numeric fields are
// then null, never $0 (a $0 price is a real, configured free price
// and reports Known true with Blended 0). Source names where the
// price came from: "pair" / "model" / "models.dev" (operator price
// book, see pairPrice), "provider-reported" (both legs reported by
// the provider), or "unknown".
type modelPriceDetail struct {
	Known           bool     `json:"known"`
	Source          string   `json:"source"`
	InputUSDPer1M   *float64 `json:"input_usd_per_1m"`
	OutputUSDPer1M  *float64 `json:"output_usd_per_1m"`
	BlendedUSDPer1M *float64 `json:"blended_usd_per_1m"`
}

// resolveModelPrice resolves one row's price: the operator price
// book first (pair, then model-level, then models.dev — a
// configured $0 there is genuinely free, not unknown), else the
// provider-reported legs when both are reported (> 0 — 0 means
// unknown on the row shape, never free), else unknown.
func (s *Server) resolveModelPrice(m Model) modelPriceDetail {
	if p, src := s.pairPrice(m.ProviderID, m.ID); src != "" {
		d := modelPriceDetail{Known: true, Source: src, InputUSDPer1M: p.InputUSDPer1M, OutputUSDPer1M: p.OutputUSDPer1M}
		if b, ok := p.BlendedUSDPer1M(); ok {
			d.BlendedUSDPer1M = &b
		}
		return d
	}
	if m.InputPriceUSD > 0 && m.OutputPriceUSD > 0 {
		in, out := m.InputPriceUSD, m.OutputPriceUSD
		blended := (in + out) / 2
		return modelPriceDetail{Known: true, Source: "provider-reported", InputUSDPer1M: &in, OutputUSDPer1M: &out, BlendedUSDPer1M: &blended}
	}
	return modelPriceDetail{Known: false, Source: "unknown"}
}

// modelDetailResponse is one catalog row in full: the GET
// /v1/models row shape plus its resolved price and base-URL
// presence. BaseURLConfigured is nil when presence cannot be
// determined (no relay resolver wired, or the resolver cannot
// resolve this model — e.g. its provider is disabled), and is
// omitted from the JSON then; it is never the URL itself.
type modelDetailResponse struct {
	Model
	Price             modelPriceDetail `json:"price"`
	BaseURLConfigured *bool            `json:"base_url_configured,omitempty"`
}

// m810Detail builds the detail row for one catalog model: the
// shortlist mark (same rule as the list) plus the resolved price
// and base-URL presence. Presence comes from the stored provider
// record (providerRemover.Get — the registry surface DELETE
// already uses; it reads the stored BaseURL, never key material,
// and the value itself is never returned, only the boolean), with
// the relay resolver as a fallback when no remover is wired.
// Presence is nil only when neither seam can resolve the model.
func (s *Server) m810Detail(ctx context.Context, m Model) modelDetailResponse {
	m.Shortlisted = s.isShortlisted(m.ProviderID, m.ID)
	d := modelDetailResponse{Model: m, Price: s.resolveModelPrice(m)}
	if m.ProviderID != "" && s.providerRemover != nil {
		if p, ok := s.providerRemover.Get(m.ProviderID); ok && p != nil {
			present := strings.TrimSpace(p.BaseURL) != ""
			d.BaseURLConfigured = &present
			return d
		}
	}
	if s.relayResolver != nil {
		if t, err := s.relayResolver.ResolveRelayTarget(ctx, m.ID); err == nil && t != nil {
			present := strings.TrimSpace(t.BaseURL) != ""
			d.BaseURLConfigured = &present
		}
	}
	return d
}

// handleModelDetail serves GET /v1/models/{model}: the catalog
// entry for one model. Unknown models answer 404 naming the id,
// never an empty object.
func (s *Server) handleModelDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := r.PathValue("model")
	if id == "" {
		writeError(w, http.StatusBadRequest, "model id is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	models, err := s.m810ListModels(ctx, true)
	if err != nil {
		s.logger.Printf("model detail registry error: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusBadGateway, "upstream provider error")
		return
	}
	for _, m := range models {
		if m.ID == id {
			writeJSON(w, http.StatusOK, s.m810Detail(ctx, m))
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", id))
}

// m810FreeTierOnly reports whether one catalog row is known-free:
// its provider's catalog category is "free", or its resolved
// blended price is known and $0. A row with no known price is
// never free — unknown is not $0 (the same rule the auto-combo
// best-free narrowing and the max_price filter use).
func (s *Server) m810FreeTierOnly(m Model) bool {
	if strings.EqualFold(m.Category, "free") {
		return true
	}
	if b, ok := s.rowBlendedPriceUSDPer1M(m.ProviderID, m.ID, m.InputPriceUSD, m.OutputPriceUSD); ok && b == 0 {
		return true
	}
	return false
}

// isValidCapability reports whether the capability name is one of the supported capabilities.
func isValidCapability(c string) bool {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "tools", "tool", "function_calling",
		"vision", "image", "image_input",
		"audio", "audio_input", "speech",
		"video", "video_input",
		"reasoning", "thinking",
		"json_schema", "json", "structured_output",
		"code", "coding":
		return true
	default:
		return false
	}
}

// catalogHasCapabilityData checks if any model in the catalog records capability flags.
func catalogHasCapabilityData(models []Model) bool {
	for _, m := range models {
		if m.Vision || m.Audio || m.Video || m.Reasoning || (m.Tools != nil) || len(m.Capabilities) > 0 {
			return true
		}
	}
	return false
}

// modelMatchesCapability checks if a model matches the requested capability.
func modelMatchesCapability(m Model, cap string) bool {
	cNorm := strings.ToLower(strings.TrimSpace(cap))
	for _, c := range m.Capabilities {
		if strings.EqualFold(c, cNorm) {
			return true
		}
	}
	switch cNorm {
	case "vision", "image", "image_input":
		return m.Vision
	case "tools", "tool", "function_calling":
		return m.Tools != nil && *m.Tools
	case "audio", "audio_input", "speech":
		return m.Audio
	case "video", "video_input":
		return m.Video
	case "reasoning", "thinking":
		return m.Reasoning
	case "json_schema", "json", "structured_output":
		return m.Tools != nil && *m.Tools
	case "code", "coding":
		id := strings.ToLower(m.ID)
		return strings.Contains(id, "coder") || strings.Contains(id, "code")
	}
	return false
}

// handleModelsQuery serves GET /v1/models/query: the catalog
// filtered by ?provider= (provider id or display name,
// case-insensitive, as on GET /v1/models), ?free-tier-only=true,
// and ?capability= (e.g. tools, vision, audio, video, reasoning, code).
// When the catalog records no per-model capability flags, capability
// queries answer 400 naming the gap.
func (s *Server) handleModelsQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	capParam := strings.TrimSpace(q.Get("capability"))
	if capParam != "" && !isValidCapability(capParam) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid capability %q: valid capabilities are tools, vision, audio, video, json_schema, reasoning, code", capParam))
		return
	}
	freeOnly := false
	switch strings.ToLower(strings.TrimSpace(q.Get("free-tier-only"))) {
	case "true", "1", "yes":
		freeOnly = true
	case "", "false", "0", "no":
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid free-tier-only %q: must be true or false", q.Get("free-tier-only")))
		return
	}
	provider := strings.TrimSpace(q.Get("provider"))
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	models, err := s.m810ListModels(ctx, q.Get("include") == "deprecated")
	if err != nil {
		s.logger.Printf("models query registry error: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusBadGateway, "upstream provider error")
		return
	}

	if capParam != "" && !catalogHasCapabilityData(models) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("capability filter %q is not supported: the catalog records no per-model capability flags (tools/JSON/vision), so no model can be matched by capability", capParam))
		return
	}

	out := make([]modelDetailResponse, 0, len(models))
	for _, m := range models {
		if provider != "" && !strings.EqualFold(m.ProviderID, provider) && !strings.EqualFold(m.OwnedBy, provider) {
			continue
		}
		if freeOnly && !s.m810FreeTierOnly(m) {
			continue
		}
		if capParam != "" && !modelMatchesCapability(m, capParam) {
			continue
		}
		out = append(out, s.m810Detail(ctx, m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// modelHealthRow is one model's health: its provider's last probe
// result from the health-probe ring, plus the ring's recorded
// uptime/latency for that provider where probes ran. Status is
// "up", "down", or "unknown" (never probed, or no checker wired);
// LastProbeAt / LatencyMs / uptimes are null while unknown —
// never a fabricated uptime. Scope is always "provider": the
// probe is a provider key test, not a per-model check.
type modelHealthRow struct {
	ID           string     `json:"id"`
	ProviderID   string     `json:"provider_id"`
	ProviderName string     `json:"provider_name"`
	Status       string     `json:"status"`
	Scope        string     `json:"scope"`
	LastProbeAt  *time.Time `json:"last_probe_at"`
	LatencyMs    *int64     `json:"latency_ms"`
	LastError    string     `json:"last_error,omitempty"`
	Samples      int        `json:"samples"`
	Uptime1h     *float64   `json:"uptime_1h"`
	Uptime24h    *float64   `json:"uptime_24h"`
	P50Ms        *int64     `json:"p50_ms"`
	P95Ms        *int64     `json:"p95_ms"`
}

// handleModelsHealth serves GET /v1/models/health: one row per
// catalog model with its provider's last probe result
// (up/down/last-probe/latency where measured) and the probe ring's
// recorded uptime/latency for that provider. ?provider= narrows
// to one provider (id or display name, as on GET /v1/models). No
// per-model probe exists — the health package's own limitation —
// so the response carries probe_scope "provider" and a note saying
// so; unprobed models report unknown.
func (s *Server) handleModelsHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	models, err := s.m810ListModels(ctx, r.URL.Query().Get("include") == "deprecated")
	if err != nil {
		s.logger.Printf("models health registry error: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusBadGateway, "upstream provider error")
		return
	}
	byProvider := map[string]health.Status{}
	if s.healthStatuses != nil {
		for _, st := range s.healthStatuses.Last() {
			byProvider[st.ProviderID] = st
		}
	}
	var hist historyLister
	if s.healthStatuses != nil {
		hist, _ = s.healthStatuses.(historyLister)
	}
	rows := make([]modelHealthRow, 0, len(models))
	for _, m := range models {
		if provider != "" && !strings.EqualFold(m.ProviderID, provider) && !strings.EqualFold(m.OwnedBy, provider) {
			continue
		}
		row := modelHealthRow{
			ID: m.ID, ProviderID: m.ProviderID, ProviderName: m.OwnedBy,
			Status: "unknown", Scope: "provider",
		}
		if st, ok := byProvider[m.ProviderID]; ok && !st.CheckedAt.IsZero() {
			if st.OK {
				row.Status = "up"
			} else {
				row.Status = "down"
			}
			at := st.CheckedAt
			row.LastProbeAt = &at
			lat := st.LatencyMs
			row.LatencyMs = &lat
			row.LastError = st.LastError
		}
		if hist != nil && m.ProviderID != "" {
			sum := hist.History(m.ProviderID)
			row.Samples = sum.Samples
			row.Uptime1h = sum.Uptime1h
			row.Uptime24h = sum.Uptime24h
			row.P50Ms = sum.P50Ms
			row.P95Ms = sum.P95Ms
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":      "list",
		"data":        rows,
		"probe_scope": "provider",
		"note":        "Health probes are provider-level key tests (one probe per provider, not per model); each model's status is its provider's last probe result. Unprobed models report unknown, never an invented uptime.",
	})
}
