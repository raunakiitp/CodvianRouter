package server

import (
	"context"
	"net/http"
	"sort"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/plugins"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ---------------------------------------------------------------------------
// Plugin hooks in the request path (CHECKLIST 201-202).
// ---------------------------------------------------------------------------

// applyPlugins runs the pre-request plugin hooks on one inbound
// request, after the built-in guardrails (a blocked request never
// reaches a plugin) and before compression, budgets, and the
// engine — so a denial or fail-closed refusal happens before any
// provider is dialled. It returns (blocked, ctx): blocked means the
// handler must stop (the refusal is already written); ctx is the
// request context with any plugin-added outbound headers attached,
// and is the context the engine calls must use (headers travel to
// the provider client on it — see provider.WithExtraHeaders).
//
// Outcomes, per the documented per-hook semantics:
//
//   - Deny: 403 with the policy-refusal code -32001, reason
//     "plugin_denied" — the same shape as a guardrail refusal, so
//     no client has to parse message strings. The plugin's own
//     reason is logged (redacted), never echoed to the client:
//     a plugin reason could carry prompt text.
//   - Error: fail closed, same 403 — a guardrail-style plugin
//     that cannot decide must not let the request through. The
//     error is logged redacted.
//   - Panic: fail closed as a gateway panic — 502 with JSON-RPC
//     code -32603, via noteServerPanic/writePanicFailure (the
//     CHECKLIST 165-166 rule: the request fails honestly, the
//     gateway survives, exactly one log line + one count).
//   - Allow: the request continues, with any in-place mutation
//     (headers, model rewrite) applied.
func (s *Server) applyPlugins(ctx context.Context, w http.ResponseWriter, req *provider.ChatRequest) (blocked bool, outCtx context.Context) {
	outCtx = ctx
	if s.plugins == nil || s.plugins.Len() == 0 {
		return false, ctx
	}
	preq := &plugins.Request{Chat: req, Headers: map[string]string{}}
	outcome := s.plugins.PreRequest(ctx, preq)
	if outcome.Panic != nil {
		// The registry already counted the panic; this only
		// logs it once (redacted) and answers 502.
		s.noteServerPanic("plugin_pre_request:"+outcome.Plugin, outcome.Panic)
		s.writePanicFailure(w)
		return true, ctx
	}
	if outcome.Err != nil {
		s.logger.Printf("plugin %s pre-request hook failed (%v); request refused (fail closed)", outcome.Plugin, guardrails.RedactLogLine(outcome.Err.Error()))
		writePolicyRefusal(w, "plugin_denied", "request")
		return true, ctx
	}
	if outcome.Denied {
		s.logger.Printf("plugin %s denied request (reason=%s)", outcome.Plugin, guardrails.Redact(outcome.Reason))
		if outcome.Status != 0 {
			// A plugin that implements RefusalDetails (the
			// cost-cap plugin: 402 naming the estimated cost
			// and the cap) gets its own refusal written
			// verbatim. The detail is built by the plugin from
			// numbers and the model name only — never prompt or
			// response content — so echoing it is safe.
			writeError(w, outcome.Status, outcome.Detail)
			return true, ctx
		}
		writePolicyRefusal(w, "plugin_denied", "request")
		return true, ctx
	}
	if len(preq.Headers) > 0 {
		outCtx = provider.WithExtraHeaders(ctx, preq.Headers)
	}
	return false, outCtx
}

// applyPostResponsePlugins runs the post-response plugin hooks on
// a delivered provider response. Failures are fail-open: each is
// logged (redacted) and counted by the registry, and the response
// is still delivered — a logging plugin must never take down a
// response. The caller passes the request the response answers, so
// a plugin can correlate the two.
func (s *Server) applyPostResponsePlugins(ctx context.Context, req *provider.ChatRequest, resp *provider.ChatResponse) {
	if s.plugins == nil || s.plugins.Len() == 0 || resp == nil {
		return
	}
	preq := &plugins.Request{Chat: req, Headers: map[string]string{}}
	for _, err := range s.plugins.PostResponse(ctx, preq, resp) {
		s.logger.Printf("plugin post-response hook failed (fail open): %v", guardrails.RedactLogLine(err.Error()))
	}
}

// ---------------------------------------------------------------------------
// Plugins dashboard feed (CHECKLIST 202).
// ---------------------------------------------------------------------------

// pluginsFeed is the GET /dashboard/plugins payload: the live
// registry snapshot — one row per registered plugin with its hook
// points and real per-hook call/error/denial/panic counts — plus
// the honest empty state when no plugin is registered. No prompt
// or response content is ever in it: the response-log plugin
// reports shapes only (model, finish reason, content length,
// token counts). CostCap is the cost-cap slice (CHECKLIST
// 319-320): which served models lack a model-level price while a
// cap is on, so the operator can add the price before traffic
// hits the per-request 402.
type pluginsFeed struct {
	Plugins       []plugins.PluginStats `json:"plugins"`
	InProcessOnly bool                  `json:"in_process_only"`
	Note          string                `json:"note"`
	CostCap       costCapFeed           `json:"cost_cap"`
}

// costCapModel is one served model without a complete model-level
// price while a cost cap is on. HasPairPrice means a
// per-(provider, model) pair price exists for the model somewhere
// in the price book (operator-configured or models.dev-synced):
// it is the "you priced this per-provider — add a model-level
// price" signal, never a claim the cap can use the pair price
// (it cannot: the plugin runs before routing).
type costCapModel struct {
	Model        string `json:"model"`
	HasPairPrice bool   `json:"has_pair_price"`
}

// costCapFeed is the cost-cap block of GET /dashboard/plugins
// (CHECKLIST 319-320). Enabled=false means the cost-cap plugin
// is not registered; a registered plugin with no cap reports
// CapUSDPerRequest 0 and an empty list (no model is capped, so
// none is refused for price). ModelsChecked counts the distinct
// served models examined; UnpricedModels is sorted by model ID.
// Note is always a one-line honest state, never an invented
// price: when the model list is unknown the feed says so instead
// of reporting zero unpriced models.
type costCapFeed struct {
	Enabled          bool           `json:"enabled"`
	CapUSDPerRequest float64        `json:"cap_usd_per_request"`
	ModelsChecked    int            `json:"models_checked"`
	UnpricedModels   []costCapModel `json:"unpriced_models"`
	Note             string         `json:"note"`
}

// costCapStatus builds the cost-cap block of the plugins feed.
// Layer choice (CHECKLIST 319): the refusal stays at request
// time — a capped model without a complete model-level price is
// refused 402 naming the model (the plugin's behaviour since
// 239) — not a startup error. The served-model set changes as
// providers are added live, and refusing to start the whole
// gateway because one model lacks a price would take down every
// other model; this feed is the proactive surface instead, so the
// operator sees the missing prices before traffic hits them.
func (s *Server) costCapStatus(ctx context.Context) costCapFeed {
	feed := costCapFeed{UnpricedModels: []costCapModel{}}
	if s.plugins == nil {
		feed.Note = "Cost-cap is not enabled."
		return feed
	}
	capUSD, present := s.plugins.CostCap()
	if !present {
		feed.Note = "Cost-cap is not enabled."
		return feed
	}
	feed.Enabled = true
	feed.CapUSDPerRequest = capUSD
	if capUSD <= 0 {
		feed.Note = "No cap configured — nothing is refused for price."
		return feed
	}
	// Capped models = the models the gateway may serve (the same
	// source GET /v1/models uses: the registry, else the
	// configured model list). Auto-router virtual models are not
	// listed: they resolve to concrete models at routing time,
	// and a model-level price is configured per concrete model.
	var ids []string
	if s.registry != nil {
		list, err := s.registry.Models(ctx)
		if err != nil {
			s.logger.Printf("cost-cap feed: model list unavailable: %v", guardrails.RedactLogLine(err.Error()))
			feed.Note = "Model list unavailable — requests still refused 402 per model."
			return feed
		}
		for _, m := range list {
			ids = append(ids, m.ID)
		}
	} else {
		ids = append(ids, s.models...)
	}
	seen := map[string]bool{}
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	sort.Strings(unique)
	feed.ModelsChecked = len(unique)
	if len(unique) == 0 {
		feed.Note = "No models served — nothing is capped."
		return feed
	}
	// A pair price for the model (any provider, operator or
	// synced): the "add a model-level price" signal. A model is
	// priced only when the MODEL-LEVEL entry is complete — both
	// input and output set (a half-known price is unknown, never
	// guessed from one side, the same rule the plugin uses).
	pairPriced := map[string]bool{}
	if s.priceBook != nil {
		for _, key := range s.priceBook.SortedPairs() {
			pairPriced[key.Model] = true
		}
		for _, key := range s.priceBook.SortedSyncedPairs() {
			pairPriced[key.Model] = true
		}
	}
	for _, id := range unique {
		priced := false
		if s.priceBook != nil {
			if p, ok := s.priceBook.LookupModel(id); ok && p.InputUSDPer1M != nil && p.OutputUSDPer1M != nil {
				priced = true
			}
		}
		if !priced {
			feed.UnpricedModels = append(feed.UnpricedModels, costCapModel{Model: id, HasPairPrice: pairPriced[id]})
		}
	}
	if len(feed.UnpricedModels) == 0 {
		feed.Note = "Every served model has a complete model-level price."
		return feed
	}
	if s.priceBook == nil {
		feed.Note = "No prices configured — every capped request is refused 402."
		return feed
	}
	feed.Note = "Requests naming these models are refused 402."
	return feed
}

// handlePlugins serves GET /dashboard/plugins.
func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	feed := pluginsFeed{
		Plugins:       []plugins.PluginStats{},
		InProcessOnly: true,
		Note:          "Plugins are compiled-in Go code registered from the config file (plugins.enabled) at startup. Dynamic loading is intentionally unsupported: no plugin binary, .so, Wasm, or JS file is ever loaded from config — see docs/plugins.md.",
		CostCap:       s.costCapStatus(r.Context()),
	}
	if s.plugins != nil {
		if snap := s.plugins.Snapshot(); len(snap) > 0 {
			feed.Plugins = snap
		}
	}
	// CHECKLIST 219-220: the pre-request and post-response hooks
	// also run on POST /v1/embeddings and POST /v1/rerank, and
	// their calls are counted in the rows above — the counts are
	// per hook point across all surfaces, not per surface.
	feed.Note += " Pre-request and post-response hooks also run on POST /v1/embeddings and POST /v1/rerank (their calls are counted in the rows above)."
	if len(feed.Plugins) == 0 {
		feed.Note += " No plugins are registered — enable the built-ins (header-add, response-log, cost-cap) in the config file under plugins.enabled, or register a compiled-in plugin at startup."
	}
	writeJSON(w, http.StatusOK, feed)
}
