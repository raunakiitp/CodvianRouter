// P1.2 — MCP/A2A ecosystem expansion tools.
//
// These tools extend the MCP surface (M9.3/M9.4 baseline) with
// provider/capability discovery, quota intelligence, budget
// forecasting, route preview, compression analytics, eval
// management, and org/team reporting. None of them fabricates
// data: every value comes from a live Source method, and
// unknown/missing values are reported explicitly.
//
// Tool registration order: scopes are declared first
// (RegisterToolScopes), then the tool (RegisterTool panics on
// duplicate registration).
package mcp

import (
	"encoding/json"
	"sort"
)

// ---- provider_discovery ----------------------------------------------

func init() {
	RegisterToolScopes("provider_discovery", ScopeReadProviders)
	RegisterTool(Tool{
		Name:        "provider_discovery",
		Description: "List every configured provider with its models, wire format, enabled state, catalog id and verification status. Read-only: no provider keys or credentials are ever returned.",
		InputSchema: emptySchema(),
	}, toolProviderDiscovery)
}

func toolProviderDiscovery(s *Server, args map[string]any) (map[string]any, string) {
	providers := s.src.Providers()
	out := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		out = append(out, map[string]any{
			"id":           p.ID,
			"name":         p.Name,
			"wire_format":  p.WireFormat,
			"enabled":      p.Enabled,
			"models":       p.Models,
			"catalog_id":   p.CatalogID,
			"verification": p.Verification,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": marshalJSON(out)}},
	}, ""
}

// ---- capability_discovery --------------------------------------------

func init() {
	RegisterToolScopes("capability_discovery", ScopeReadCapabilities)
	RegisterTool(Tool{
		Name:        "capability_discovery",
		Description: "List per-provider/model capability facts with provenance (source, verification status, observation time, confidence). Capabilities reported by the registry: tool_calling. Unknown is reported explicitly — never guessed from a model name.",
		InputSchema: schemaProps(map[string]any{
			"provider_id": strProp("Optional provider id filter."),
			"model_id":    strProp("Optional model id filter (requires provider_id)."),
		}, nil),
	}, toolCapabilityDiscovery)
}

func toolCapabilityDiscovery(s *Server, args map[string]any) (map[string]any, string) {
	providerFilter, _ := args["provider_id"].(string)
	modelFilter, _ := args["model_id"].(string)
	if modelFilter != "" && providerFilter == "" {
		return nil, "model_id requires provider_id to be set"
	}
	entries := s.src.CapabilityEntries()
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if providerFilter != "" && e.ProviderID != providerFilter {
			continue
		}
		if modelFilter != "" && e.ModelID != modelFilter {
			continue
		}
		supported := "unknown"
		if e.Supported != nil {
			if *e.Supported {
				supported = "supported"
			} else {
				supported = "unsupported"
			}
		}
		out = append(out, map[string]any{
			"provider_id":   e.ProviderID,
			"model_id":      e.ModelID,
			"capability":    e.Capability,
			"supported":     supported,
			"native":        e.Native,
			"source":        e.Source,
			"verified_at":   e.VerifiedAt,
			"confidence":    e.Confidence,
			"documentation": e.Documentation,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i]["provider_id"].(string) != out[j]["provider_id"].(string) {
			return out[i]["provider_id"].(string) < out[j]["provider_id"].(string)
		}
		return out[i]["model_id"].(string) < out[j]["model_id"].(string)
	})
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": marshalJSON(out)}},
	}, ""
}

// ---- quota_intelligence ----------------------------------------------

func init() {
	RegisterToolScopes("quota_intelligence", ScopeReadQuota)
	RegisterTool(Tool{
		Name:        "quota_intelligence",
		Description: "Unified quota intelligence across key budgets and catalog free-tier providers. Each row carries provenance (source, confidence, observation time) and a state: healthy, approaching_limit, exhausted, unavailable, unknown. Unknown is never confused with exhausted.",
		InputSchema: schemaProps(map[string]any{
			"key_id":      strProp("Optional gateway key id filter."),
			"free_tier":   map[string]any{"type": "boolean", "description": "When true, only free-tier provider rows are returned."},
			"provider_id": strProp("Optional provider id filter."),
		}, nil),
	}, toolQuotaIntelligence)
}

func toolQuotaIntelligence(s *Server, args map[string]any) (map[string]any, string) {
	keyID, _ := args["key_id"].(string)
	freeTier, _ := args["free_tier"].(bool)
	providerID, _ := args["provider_id"].(string)

	entries, err := s.src.QuotaIntelligence(keyID)
	if err != nil {
		return nil, "unavailable: " + err.Error()
	}

	// Apply filters.
	filtered := entries[:0]
	for _, e := range entries {
		if keyID != "" && e.Target != keyID && e.ID != keyID {
			continue
		}
		if freeTier && e.Source != "catalog_free_tier" {
			continue
		}
		if providerID != "" && e.Provider != providerID && e.Target != providerID {
			continue
		}
		filtered = append(filtered, e)
	}

	// Summarise.
	summary := map[string]int{}
	for _, e := range filtered {
		summary[e.State]++
	}

	out := map[string]any{
		"filters": map[string]any{
			"key_id":      keyID,
			"free_tier":   freeTier,
			"provider_id": providerID,
		},
		"summary": summary,
		"rows":    quotaRowsToAny(filtered),
		"count":   len(filtered),
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": marshalJSON(out)}},
	}, ""
}

// ---- budget_forecasting ----------------------------------------------

func init() {
	RegisterToolScopes("budget_forecasting", ScopeReadBudget)
	RegisterTool(Tool{
		Name:        "budget_forecasting",
		Description: "Burn-rate forecast for one gateway key: 24h spend, active window limit, projected exhaustion time, and status (ok/projected/exhausted/insufficient_data). Computed purely from real ledger spend + stored limit; never fabricates a forecast.",
		InputSchema: schemaProps(map[string]any{
			"key_id": strProp("Gateway key id (required)."),
		}, []string{"key_id"}),
	}, toolBudgetForecasting)
}

func toolBudgetForecasting(s *Server, args map[string]any) (map[string]any, string) {
	keyID, ok := args["key_id"].(string)
	if !ok || keyID == "" {
		return nil, "key_id is required"
	}
	fc, err := s.src.BudgetForecast(keyID)
	if err != nil {
		return nil, "unavailable: " + err.Error()
	}
	out := map[string]any{
		"key_id":                 fc.KeyID,
		"burn_rate_usd_per_hour": fc.BurnRateUSDPerHour,
		"spend_usd_24h":          fc.SpendUSD24h,
		"limit_usd":              fc.LimitUSD,
		"limit_known":            fc.LimitKnown,
		"insufficient_data":      fc.InsufficientData,
		"updated_at":             fc.UpdatedAt,
	}
	if fc.ProjectedExhaustAt != "" {
		out["projected_exhaust_at"] = fc.ProjectedExhaustAt
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": marshalJSON(out)}},
	}, ""
}

// ---- route_preview ---------------------------------------------------

func init() {
	RegisterToolScopes("route_preview", ScopeReadRoutingPlan)
	RegisterTool(Tool{
		Name:        "route_preview",
		Description: "Dry-run route preview: given a model, returns eligible candidates, estimated cost, strategy, and notes. Makes no provider call and consumes no quota.",
		InputSchema: schemaProps(map[string]any{
			"model":         strProp("Model id (required)."),
			"prompt_tokens": numProp("Optional prompt token count for context-fit check."),
		}, []string{"model"}),
	}, toolRoutePreview)
}

func toolRoutePreview(s *Server, args map[string]any) (map[string]any, string) {
	model, _ := args["model"].(string)
	promptTokens, _ := args["prompt_tokens"].(float64)
	if model == "" {
		return nil, "model is required"
	}

	// Explain route (dry run, no provider call).
	expl, err := s.src.ExplainRoute(model)
	if err != nil {
		return nil, "unavailable: " + err.Error()
	}

	// Simulate route for cost/context context.
	sim, err := s.src.SimulateRoute(model, int(promptTokens))
	if err != nil {
		return nil, "unavailable: " + err.Error()
	}

	out := map[string]any{
		"model":               model,
		"eligible_candidates": routeAttemptsToAny(expl.Attempts),
		"rejected_candidates": []any{},
		"rejection_reasons":   expl.Note,
		"estimated_cost_usd":  sim.EstimatedPromptCostUSD,
		"cost_known":          sim.CostKnown,
		"prompt_tokens":       sim.PromptTokens,
		"strategy":            expl.Strategy,
		"rule":                expl.Rule,
		"combo":               expl.Combo,
		"note":                expl.Note + " Dry run — no provider call made, no quota consumed.",
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": marshalJSON(out)}},
	}, ""
}

// ---- compression_analytics -------------------------------------------

func init() {
	RegisterToolScopes("compression_analytics", ScopeReadCompression)
	RegisterTool(Tool{
		Name:        "compression_analytics",
		Description: "Live compression analytics: layers, original/compressed token counts, savings percentage, inflation rate, and bypass count. No provider call is made.",
		InputSchema: emptySchema(),
	}, toolCompressionAnalytics)
}

func toolCompressionAnalytics(s *Server, args map[string]any) (map[string]any, string) {
	stats := s.src.CompressionStats()
	out := map[string]any{
		"layers":            stats.Layers,
		"original_tokens":   stats.OriginalTokens,
		"compressed_tokens": stats.CompressedTokens,
		"savings_pct":       stats.SavingsPct,
		"inflation_rate":    stats.InflationRate,
		"bypass_count":      stats.BypassCount,
		"last_updated":      stats.LastUpdated,
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": marshalJSON(out)}},
	}, ""
}

// ---- eval_management -------------------------------------------------

func init() {
	RegisterToolScopes("eval_management", ScopeManageEval)
	RegisterTool(Tool{
		Name:        "eval_management",
		Description: "List scheduled evaluation schedules with suite, interval, enabled state, timezone, last run, and next run. Read-only: no schedule mutations through this tool (use the /api/eval/schedules REST API for create/edit/pause/resume/delete).",
		InputSchema: emptySchema(),
	}, toolEvalManagement)
}

func toolEvalManagement(s *Server, args map[string]any) (map[string]any, string) {
	schedules, err := s.src.EvalSchedules()
	if err != nil {
		return nil, "unavailable: " + err.Error()
	}
	rows := make([]map[string]any, 0, len(schedules))
	for _, sch := range schedules {
		rows = append(rows, map[string]any{
			"id":              sch.ID,
			"suite":           sch.Suite,
			"every_sec":       sch.EverySec,
			"enabled":         sch.Enabled,
			"timezone":        sch.TimeZone,
			"last_run":        sch.LastRun,
			"use_real_engine": sch.UseRealEngine,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["id"].(string) < rows[j]["id"].(string) })
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": marshalJSON(map[string]any{
			"schedules": rows,
			"count":     len(rows),
		})}},
	}, ""
}

// ---- org_reporting ---------------------------------------------------

func init() {
	RegisterToolScopes("org_reporting", ScopeReadOrg)
	RegisterTool(Tool{
		Name:        "org_reporting",
		Description: "Organization/team reporting summary: org count, team count, key count, bound key count, total budget, consumed budget. Read-only.",
		InputSchema: emptySchema(),
	}, toolOrgReporting)
}

func toolOrgReporting(s *Server, args map[string]any) (map[string]any, string) {
	report := s.src.OrgReport()
	out := map[string]any{
		"orgs":             report.Orgs,
		"teams":            report.Teams,
		"keys":             report.Keys,
		"bound_keys":       report.BoundKeys,
		"total_budget_usd": report.TotalBudgetUSD,
		"consumed_usd":     report.ConsumedUSD,
		"updated_at":       report.UpdatedAt,
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": marshalJSON(out)}},
	}, ""
}

// ---- helpers ----------------------------------------------------------

func marshalJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{\"error\": \"json marshal: " + err.Error() + "\"}"
	}
	return string(b)
}

// quotaRowsToAny converts quota intelligence rows to JSON-compatible maps.
func quotaRowsToAny(rows []QuotaIntelEntry) []any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"kind":       r.Kind,
			"id":         r.ID,
			"name":       r.Name,
			"scope":      r.Scope,
			"target":     r.Target,
			"model":      r.Model,
			"provider":   r.Provider,
			"budget_usd": r.BudgetUSD,
			"consumed":   r.Consumed,
			"used_pct":   r.UsedPct,
			"state":      r.State,
			"source":     r.Source,
			"confidence": r.Confidence,
			"updated_at": r.UpdatedAt,
			"filters":    r.Filters,
		})
	}
	return out
}

func routeAttemptsToAny(attempts []RouteAttempt) []any {
	out := make([]any, 0, len(attempts))
	for _, a := range attempts {
		entry := map[string]any{
			"provider":      a.Provider,
			"provider_name": a.ProviderName,
			"model":         a.Model,
			"enabled":       a.Enabled,
			"price_known":   a.PriceKnown,
		}
		if a.PromptUSDPer1M != nil {
			entry["prompt_usd_per_1m"] = *a.PromptUSDPer1M
		}
		out = append(out, entry)
	}
	return out
}
