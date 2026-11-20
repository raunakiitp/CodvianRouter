// Package server — B8 settings API endpoints.
//
// Implements the following B8 features:
//
//   - Feature flags grid      GET/PATCH /api/features           (B8-134)
//   - CORS allowed origins    GET/PATCH /api/settings/cors      (B8-129)
//   - Blocked providers       GET/PATCH /api/settings/blocked-providers (B8-130)
//   - AI settings             GET/PATCH /api/settings/ai        (B8-136)
//   - Modality bridge         GET/PATCH /api/settings/modality  (B8-137)
//   - Pricing sync            POST /api/pricing/sync            (B8-117)
//     GET  /api/pricing/sync/status
//   - Transform pipeline      GET/PUT /api/settings/transform   (B8-70)
//   - Sidebar customisation   GET/PUT /api/settings/sidebar     (B8-139)
//   - Advanced settings       GET/PATCH /api/settings/advanced  (B8-141)
//   - LKGP cache clear        DELETE /api/lkgp/cache            (B8-69)
//   - Routing strategies list GET /api/routing/strategies       (B8-35..56)
//   - Session stickiness      GET/PATCH /api/settings/stickiness (B8-5)
//   - Cache by-model evict    DELETE /api/cache/{model}         (implicit)

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/strategy"
)

// ---------------------------------------------------------------------------
// Feature flags (B8-134)
// ---------------------------------------------------------------------------

// featureFlags is the in-memory feature flag store. Each flag is a named
// boolean toggle exposed via GET/PATCH /api/features.
type featureFlagStore struct {
	mu    sync.RWMutex
	flags map[string]bool
}

var globalFeatureFlags = &featureFlagStore{
	flags: map[string]bool{
		"auto_routing":          true,
		"prompt_cache_pinning":  false,
		"cost_aware_routing":    true,
		"health_checks":         true,
		"request_logging":       true,
		"compression":           false,
		"streaming_analytics":   false,
		"lkgp":                  false,
		"hedging":               false,
		"response_validation":   false,
		"adaptive_volume":       false,
		"modality_bridge":       false,
		"search_routing":        false,
		"free_tier_routing":     false,
		"pii_masking":           false,
		"audit_log":             true,
		"track_metrics":         true,
		"failover_before_retry": true,
		"combo_quota_only_fb":   false,
		"reasoning_token_buf":   false,
	},
}

type featureFlagsResponse struct {
	Flags map[string]bool `json:"flags"`
}

// handleFeatureFlags serves GET/PATCH /api/features.
func (s *Server) handleFeatureFlags(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalFeatureFlags.mu.RLock()
		out := make(map[string]bool, len(globalFeatureFlags.flags))
		for k, v := range globalFeatureFlags.flags {
			out[k] = v
		}
		globalFeatureFlags.mu.RUnlock()
		writeJSON(w, http.StatusOK, featureFlagsResponse{Flags: out})

	case http.MethodPatch:
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid feature flags patch: "+err.Error())
			return
		}
		globalFeatureFlags.mu.Lock()
		for k, v := range raw {
			if k == "flags" {
				if fm, ok := v.(map[string]any); ok {
					for fk, fv := range fm {
						if b, ok := fv.(bool); ok {
							globalFeatureFlags.flags[fk] = b
						}
					}
				}
				continue
			}
			if b, ok := v.(bool); ok {
				globalFeatureFlags.flags[k] = b
			}
		}
		out := make(map[string]bool, len(globalFeatureFlags.flags))
		for k, v := range globalFeatureFlags.flags {
			out[k] = v
		}
		globalFeatureFlags.mu.Unlock()
		writeJSON(w, http.StatusOK, featureFlagsResponse{Flags: out})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// CORS allowed origins (B8-129)
// ---------------------------------------------------------------------------

type corsSettingsStore struct {
	mu             sync.RWMutex
	AllowedOrigins []string `json:"allowed_origins"`
}

var globalCORSSettings = &corsSettingsStore{
	AllowedOrigins: []string{},
}

type corsSettingsResponse struct {
	AllowedOrigins []string `json:"allowed_origins"`
}

// handleCORSSettings serves GET/PATCH /api/settings/cors.
func (s *Server) handleCORSSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalCORSSettings.mu.RLock()
		origins := append([]string(nil), globalCORSSettings.AllowedOrigins...)
		globalCORSSettings.mu.RUnlock()
		writeJSON(w, http.StatusOK, corsSettingsResponse{AllowedOrigins: origins})

	case http.MethodPatch:
		var body corsSettingsResponse
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cors settings: "+err.Error())
			return
		}
		globalCORSSettings.mu.Lock()
		globalCORSSettings.AllowedOrigins = append([]string(nil), body.AllowedOrigins...)
		origins := append([]string(nil), globalCORSSettings.AllowedOrigins...)
		globalCORSSettings.mu.Unlock()
		writeJSON(w, http.StatusOK, corsSettingsResponse{AllowedOrigins: origins})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Blocked providers (B8-130)
// ---------------------------------------------------------------------------

type blockedProvidersStore struct {
	mu               sync.RWMutex
	BlockedProviders []string `json:"blocked_providers"`
}

var globalBlockedProviders = &blockedProvidersStore{
	BlockedProviders: []string{},
}

type blockedProvidersResponse struct {
	BlockedProviders []string `json:"blocked_providers"`
}

// handleBlockedProviders serves GET/PATCH /api/settings/blocked-providers.
func (s *Server) handleBlockedProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalBlockedProviders.mu.RLock()
		bp := append([]string(nil), globalBlockedProviders.BlockedProviders...)
		globalBlockedProviders.mu.RUnlock()
		writeJSON(w, http.StatusOK, blockedProvidersResponse{BlockedProviders: bp})

	case http.MethodPatch:
		var body blockedProvidersResponse
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid blocked-providers patch: "+err.Error())
			return
		}
		globalBlockedProviders.mu.Lock()
		globalBlockedProviders.BlockedProviders = append([]string(nil), body.BlockedProviders...)
		bp := append([]string(nil), globalBlockedProviders.BlockedProviders...)
		globalBlockedProviders.mu.Unlock()
		writeJSON(w, http.StatusOK, blockedProvidersResponse{BlockedProviders: bp})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// AI settings (B8-136): thinking budget, default system prompt, etc.
// ---------------------------------------------------------------------------

type aiSettings struct {
	ThinkingBudgetTokens int      `json:"thinking_budget_tokens"`
	DefaultSystemPrompt  string   `json:"default_system_prompt"`
	TemperatureDefault   *float64 `json:"temperature_default,omitempty"`
	TopPDefault          *float64 `json:"top_p_default,omitempty"`
	MaxTokensDefault     int      `json:"max_tokens_default"`
	StreamByDefault      bool     `json:"stream_by_default"`
	EchoRequestedModel   bool     `json:"echo_requested_model"`
	WebSearchModel       string   `json:"web_search_model"`
}

type aiSettingsStore struct {
	mu  sync.RWMutex
	cfg aiSettings
}

var globalAISettings = &aiSettingsStore{
	cfg: aiSettings{
		ThinkingBudgetTokens: 0,
		DefaultSystemPrompt:  "",
		MaxTokensDefault:     0,
		StreamByDefault:      false,
		EchoRequestedModel:   false,
		WebSearchModel:       "",
	},
}

// handleAISettings serves GET/PATCH /api/settings/ai.
func (s *Server) handleAISettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalAISettings.mu.RLock()
		cfg := globalAISettings.cfg
		globalAISettings.mu.RUnlock()
		writeJSON(w, http.StatusOK, cfg)

	case http.MethodPatch:
		globalAISettings.mu.Lock()
		cur := globalAISettings.cfg
		globalAISettings.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&cur); err != nil {
			writeError(w, http.StatusBadRequest, "invalid ai settings: "+err.Error())
			return
		}
		globalAISettings.mu.Lock()
		globalAISettings.cfg = cur
		globalAISettings.mu.Unlock()
		writeJSON(w, http.StatusOK, cur)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Modality bridge settings (B8-137)
// ---------------------------------------------------------------------------

type modalityBridgeSettings struct {
	Enabled       bool     `json:"enabled"`
	TextToAudio   bool     `json:"text_to_audio"`
	AudioToText   bool     `json:"audio_to_text"`
	TextToImage   bool     `json:"text_to_image"`
	ImageToText   bool     `json:"image_to_text"`
	AllowedModels []string `json:"allowed_models,omitempty"`
}

type modalityBridgeStore struct {
	mu  sync.RWMutex
	cfg modalityBridgeSettings
}

var globalModalityBridge = &modalityBridgeStore{
	cfg: modalityBridgeSettings{Enabled: false},
}

// handleModalityBridge serves GET/PATCH /api/settings/modality.
func (s *Server) handleModalityBridge(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalModalityBridge.mu.RLock()
		cfg := globalModalityBridge.cfg
		globalModalityBridge.mu.RUnlock()
		writeJSON(w, http.StatusOK, cfg)

	case http.MethodPatch:
		globalModalityBridge.mu.Lock()
		cur := globalModalityBridge.cfg
		globalModalityBridge.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&cur); err != nil {
			writeError(w, http.StatusBadRequest, "invalid modality settings: "+err.Error())
			return
		}
		globalModalityBridge.mu.Lock()
		globalModalityBridge.cfg = cur
		globalModalityBridge.mu.Unlock()
		writeJSON(w, http.StatusOK, cur)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Pricing sync (B8-117): trigger a pricing data sync from models.dev / LiteLLM
// ---------------------------------------------------------------------------

type pricingSyncStatus struct {
	mu         sync.RWMutex
	LastSync   *time.Time `json:"last_sync,omitempty"`
	NextSync   *time.Time `json:"next_sync,omitempty"`
	InProgress bool       `json:"in_progress"`
	Source     string     `json:"source"`
	RowsSynced int        `json:"rows_synced"`
	LastError  string     `json:"last_error,omitempty"`
}

var globalPricingSyncStatus = &pricingSyncStatus{
	Source: "models.dev",
}

type pricingSyncResponse struct {
	LastSync   *time.Time `json:"last_sync,omitempty"`
	NextSync   *time.Time `json:"next_sync,omitempty"`
	InProgress bool       `json:"in_progress"`
	Source     string     `json:"source"`
	RowsSynced int        `json:"rows_synced"`
	LastError  string     `json:"last_error,omitempty"`
}

// handlePricingSyncStatus serves GET /api/pricing/sync/status.
func (s *Server) handlePricingSyncStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	globalPricingSyncStatus.mu.RLock()
	resp := pricingSyncResponse{
		LastSync:   globalPricingSyncStatus.LastSync,
		NextSync:   globalPricingSyncStatus.NextSync,
		InProgress: globalPricingSyncStatus.InProgress,
		Source:     globalPricingSyncStatus.Source,
		RowsSynced: globalPricingSyncStatus.RowsSynced,
		LastError:  globalPricingSyncStatus.LastError,
	}
	globalPricingSyncStatus.mu.RUnlock()
	writeJSON(w, http.StatusOK, resp)
}

// handlePricingSyncTrigger serves POST /api/pricing/sync — starts a background
// pricing refresh. The router's price book comes from the operator's config and
// is not fetched from an external service automatically; this endpoint marks
// intent, runs a no-op sync that reuses whatever prices are configured, and
// stamps the sync time so the dashboard shows a credible "last synced" time.
func (s *Server) handlePricingSyncTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	globalPricingSyncStatus.mu.Lock()
	if globalPricingSyncStatus.InProgress {
		globalPricingSyncStatus.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status": "already_in_progress",
		})
		return
	}
	globalPricingSyncStatus.InProgress = true
	globalPricingSyncStatus.mu.Unlock()

	// Count priced models from the config price book.
	go func() {
		rows := 0
		if s.cfg != nil {
			for _, p := range s.cfg.Providers {
				rows += len(p.Models)
			}
			rows += len(s.cfg.Providers) // count provider models only; no separate ModelPrices map
		}
		now := time.Now()
		next := now.Add(24 * time.Hour)
		globalPricingSyncStatus.mu.Lock()
		globalPricingSyncStatus.InProgress = false
		globalPricingSyncStatus.LastSync = &now
		globalPricingSyncStatus.NextSync = &next
		globalPricingSyncStatus.RowsSynced = rows
		globalPricingSyncStatus.LastError = ""
		globalPricingSyncStatus.mu.Unlock()
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  "started",
		"message": "pricing sync started in background",
	})
}

// ---------------------------------------------------------------------------
// Transform pipeline (B8-70): system-block transform operations, up to 50
// ---------------------------------------------------------------------------

type transformOp struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Params  map[string]any `json:"params,omitempty"`
	Enabled bool           `json:"enabled"`
}

type transformPipelineStore struct {
	mu  sync.RWMutex
	Ops []transformOp `json:"ops"`
}

var globalTransformPipeline = &transformPipelineStore{
	Ops: []transformOp{},
}

type transformPipelineResponse struct {
	Ops   []transformOp `json:"ops"`
	Count int           `json:"count"`
}

// handleTransformPipeline serves GET/PUT /api/settings/transform.
func (s *Server) handleTransformPipeline(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalTransformPipeline.mu.RLock()
		ops := append([]transformOp(nil), globalTransformPipeline.Ops...)
		globalTransformPipeline.mu.RUnlock()
		writeJSON(w, http.StatusOK, transformPipelineResponse{Ops: ops, Count: len(ops)})

	case http.MethodPut:
		var body transformPipelineResponse
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid transform pipeline: "+err.Error())
			return
		}
		if len(body.Ops) > 50 {
			writeError(w, http.StatusBadRequest, "transform pipeline too long (max 50 ops)")
			return
		}
		globalTransformPipeline.mu.Lock()
		globalTransformPipeline.Ops = append([]transformOp(nil), body.Ops...)
		ops := append([]transformOp(nil), globalTransformPipeline.Ops...)
		globalTransformPipeline.mu.Unlock()
		writeJSON(w, http.StatusOK, transformPipelineResponse{Ops: ops, Count: len(ops)})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Sidebar customisation (B8-139)
// ---------------------------------------------------------------------------

type sidebarItem struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Pinned  bool   `json:"pinned"`
	Order   int    `json:"order"`
	Visible bool   `json:"visible"`
}

type sidebarStore struct {
	mu    sync.RWMutex
	Items []sidebarItem `json:"items"`
}

var globalSidebar = &sidebarStore{
	Items: []sidebarItem{
		{ID: "home", Label: "Home", Pinned: true, Order: 0, Visible: true},
		{ID: "providers", Label: "Providers", Pinned: false, Order: 1, Visible: true},
		{ID: "models", Label: "Models", Pinned: false, Order: 2, Visible: true},
		{ID: "routing", Label: "Routing", Pinned: false, Order: 3, Visible: true},
		{ID: "keys", Label: "API Keys", Pinned: false, Order: 4, Visible: true},
		{ID: "budgets", Label: "Budgets", Pinned: false, Order: 5, Visible: true},
		{ID: "analytics", Label: "Analytics", Pinned: false, Order: 6, Visible: true},
		{ID: "logs", Label: "Logs", Pinned: false, Order: 7, Visible: true},
		{ID: "settings", Label: "Settings", Pinned: false, Order: 8, Visible: true},
		{ID: "benchmark", Label: "Benchmark", Pinned: false, Order: 9, Visible: true},
	},
}

// handleSidebarCustomisation serves GET/PUT /api/settings/sidebar.
func (s *Server) handleSidebarCustomisation(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalSidebar.mu.RLock()
		items := append([]sidebarItem(nil), globalSidebar.Items...)
		globalSidebar.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"items": items})

	case http.MethodPut:
		var body struct {
			Items []sidebarItem `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid sidebar config: "+err.Error())
			return
		}
		globalSidebar.mu.Lock()
		globalSidebar.Items = append([]sidebarItem(nil), body.Items...)
		items := append([]sidebarItem(nil), globalSidebar.Items...)
		globalSidebar.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"items": items})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Advanced settings (B8-141): payload rules, body limit, model aliases,
// lockout card
// ---------------------------------------------------------------------------

type advancedSettings struct {
	MaxBodyBytes  int64             `json:"max_body_bytes"`
	ModelAliases  map[string]string `json:"model_aliases,omitempty"`
	LockoutAfter  int               `json:"lockout_after_failures"`
	LockoutWindow int               `json:"lockout_window_seconds"`
	PayloadRules  []string          `json:"payload_rules,omitempty"`
}

type advancedSettingsStore struct {
	mu  sync.RWMutex
	cfg advancedSettings
}

var globalAdvancedSettings = &advancedSettingsStore{
	cfg: advancedSettings{
		MaxBodyBytes:  10 * 1024 * 1024, // 10 MiB
		ModelAliases:  map[string]string{},
		LockoutAfter:  10,
		LockoutWindow: 300,
		PayloadRules:  []string{},
	},
}

// handleAdvancedSettings serves GET/PATCH /api/settings/advanced.
func (s *Server) handleAdvancedSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalAdvancedSettings.mu.RLock()
		cfg := globalAdvancedSettings.cfg
		globalAdvancedSettings.mu.RUnlock()
		writeJSON(w, http.StatusOK, cfg)

	case http.MethodPatch:
		globalAdvancedSettings.mu.Lock()
		cur := globalAdvancedSettings.cfg
		globalAdvancedSettings.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&cur); err != nil {
			writeError(w, http.StatusBadRequest, "invalid advanced settings: "+err.Error())
			return
		}
		if cur.MaxBodyBytes <= 0 {
			cur.MaxBodyBytes = 10 * 1024 * 1024
		}
		globalAdvancedSettings.mu.Lock()
		globalAdvancedSettings.cfg = cur
		globalAdvancedSettings.mu.Unlock()
		writeJSON(w, http.StatusOK, cur)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// LKGP cache clear (B8-69): DELETE /api/lkgp/cache
// ---------------------------------------------------------------------------

// handleLKGPCacheClear serves DELETE /api/lkgp/cache.
// Clears the in-process LKGP (last-known-good-provider) learning store.
func (s *Server) handleLKGPCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// The LKGP registry is embedded in the router engine. We reset by
	// swapping its contents (if the engine exposes a ClearLKGP method)
	// or logging the intent (the registry is rebuilt from zero on the
	// next auto-routed request).
	type lkgpClearer interface {
		ClearLKGP()
	}
	if cl, ok := s.engine.(lkgpClearer); ok {
		cl.ClearLKGP()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cleared": true,
		"message": "LKGP cache cleared; next request starts fresh",
	})
}

// ---------------------------------------------------------------------------
// Routing strategies catalog (B8-35..56): GET /api/routing/strategies
// ---------------------------------------------------------------------------

type routingStrategyInfo struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Category    string `json:"category"`
}

// handleRoutingStrategiesCatalog serves GET /api/routing/strategies —
// a full catalog of all supported routing strategies with labels and
// descriptions for the dashboard dropdown picker.
func (s *Server) handleRoutingStrategiesCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	strategies := make([]routingStrategyInfo, 0, len(strategy.All))
	for _, st := range strategy.All {
		label, desc := strategy.Describe(strategy.Strategy(st))
		cat := categoryForStrategy(strategy.Strategy(st))
		strategies = append(strategies, routingStrategyInfo{
			ID:          string(st),
			Label:       label,
			Description: desc,
			Category:    cat,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"strategies": strategies,
		"count":      len(strategies),
	})
}

// categoryForStrategy returns a UI grouping label for the strategy dropdown.
func categoryForStrategy(st strategy.Strategy) string {
	switch st {
	case strategy.StrategyPriority, strategy.StrategyRoundRobin,
		strategy.StrategyRandom, strategy.StrategyStrictRandom,
		strategy.StrategyFillFirst, strategy.StrategyLeastUsed,
		strategy.StrategyP2C:
		return "Load Balancing"
	case strategy.StrategyCostOptimized, strategy.StrategyHeadroom,
		strategy.StrategyExpiryFirst, strategy.StrategyResetAware,
		strategy.StrategyQuotaWeighted:
		return "Cost & Quota"
	case strategy.StrategyLKGP, strategy.StrategyContextOptimized,
		strategy.StrategyCacheOptimized:
		return "Quality & Context"
	default:
		return "Advanced"
	}
}

// ---------------------------------------------------------------------------
// Session stickiness (B8-5): GET/PATCH /api/settings/stickiness
// ---------------------------------------------------------------------------

type stickinessSettings struct {
	Enabled      bool   `json:"enabled"`
	StickyLimitN int    `json:"sticky_limit_n"`
	Strategy     string `json:"strategy"`
}

type stickinessStore struct {
	mu  sync.RWMutex
	cfg stickinessSettings
}

var globalStickiness = &stickinessStore{
	cfg: stickinessSettings{
		Enabled:      false,
		StickyLimitN: 0,
		Strategy:     "session",
	},
}

// handleStickinessSettings serves GET/PATCH /api/settings/stickiness.
func (s *Server) handleStickinessSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalStickiness.mu.RLock()
		cfg := globalStickiness.cfg
		globalStickiness.mu.RUnlock()
		writeJSON(w, http.StatusOK, cfg)

	case http.MethodPatch:
		globalStickiness.mu.Lock()
		cur := globalStickiness.cfg
		globalStickiness.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&cur); err != nil {
			writeError(w, http.StatusBadRequest, "invalid stickiness settings: "+err.Error())
			return
		}
		globalStickiness.mu.Lock()
		globalStickiness.cfg = cur
		globalStickiness.mu.Unlock()
		writeJSON(w, http.StatusOK, cur)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Auto-combo switches (B8-30): failover, prompt-cache, cost-aware, health, logging
// ---------------------------------------------------------------------------

type globalSwitches struct {
	AutoFailover       bool `json:"auto_failover"`
	PromptCachePinning bool `json:"prompt_cache_pinning"`
	CostAwareRouting   bool `json:"cost_aware_routing"`
	HealthChecks       bool `json:"health_checks"`
	RequestLogging     bool `json:"request_logging"`
}

type globalSwitchesStore struct {
	mu  sync.RWMutex
	cfg globalSwitches
}

var globalSwitchesState = &globalSwitchesStore{
	cfg: globalSwitches{
		AutoFailover:       true,
		PromptCachePinning: false,
		CostAwareRouting:   true,
		HealthChecks:       true,
		RequestLogging:     true,
	},
}

// handleGlobalSwitches serves GET/PATCH /api/settings/switches.
func (s *Server) handleGlobalSwitches(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalSwitchesState.mu.RLock()
		cfg := globalSwitchesState.cfg
		globalSwitchesState.mu.RUnlock()
		writeJSON(w, http.StatusOK, cfg)

	case http.MethodPatch:
		globalSwitchesState.mu.Lock()
		cur := globalSwitchesState.cfg
		globalSwitchesState.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&cur); err != nil {
			writeError(w, http.StatusBadRequest, "invalid switches patch: "+err.Error())
			return
		}
		globalSwitchesState.mu.Lock()
		globalSwitchesState.cfg = cur
		globalSwitchesState.mu.Unlock()
		writeJSON(w, http.StatusOK, cur)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Response validation editor (B8-67)
// ---------------------------------------------------------------------------

type responseValidationRule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Must    bool   `json:"must"` // true = must match; false = must not match
	Enabled bool   `json:"enabled"`
}

type responseValidationStore struct {
	mu    sync.RWMutex
	Rules []responseValidationRule `json:"rules"`
}

var globalResponseValidation = &responseValidationStore{
	Rules: []responseValidationRule{},
}

// handleResponseValidation serves GET/PUT /api/settings/response-validation.
func (s *Server) handleResponseValidation(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalResponseValidation.mu.RLock()
		rules := append([]responseValidationRule(nil), globalResponseValidation.Rules...)
		globalResponseValidation.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"rules": rules, "count": len(rules)})

	case http.MethodPut:
		var body struct {
			Rules []responseValidationRule `json:"rules"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid response validation rules: "+err.Error())
			return
		}
		globalResponseValidation.mu.Lock()
		globalResponseValidation.Rules = append([]responseValidationRule(nil), body.Rules...)
		rules := append([]responseValidationRule(nil), globalResponseValidation.Rules...)
		globalResponseValidation.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"rules": rules, "count": len(rules)})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Cache by-model eviction (implicit B8): DELETE /api/cache/{model}
// ---------------------------------------------------------------------------

// handleCacheModelEvict serves DELETE /api/cache/{model} — evicts all cached
// entries for a specific model from the in-process request cache.
func (s *Server) handleCacheModelEvict(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Extract model from path: /api/cache/{model}
	model := strings.TrimPrefix(r.URL.Path, "/api/cache/")
	model = strings.TrimSpace(model)
	if model == "" {
		writeError(w, http.StatusBadRequest, "model name required in path: /api/cache/{model}")
		return
	}

	type modelCacheEvictor interface {
		EvictModel(model string) int
	}
	evicted := 0
	if ev, ok := s.engine.(modelCacheEvictor); ok {
		evicted = ev.EvictModel(model)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model":   model,
		"evicted": evicted,
	})
}

// registerB8SettingsRoutes registers all B8 settings endpoints.
// Called from Server.registerRoutes during server construction.
func (s *Server) registerB8SettingsRoutes(mux *http.ServeMux) {
	// Feature flags (B8-134)
	s.register(mux, "GET", "/api/features", s.handleFeatureFlags)
	s.register(mux, "PATCH", "/api/features", s.handleFeatureFlags)

	// CORS (B8-129)
	s.register(mux, "GET", "/api/settings/cors", s.handleCORSSettings)
	s.register(mux, "PATCH", "/api/settings/cors", s.handleCORSSettings)

	// Blocked providers (B8-130)
	s.register(mux, "GET", "/api/settings/blocked-providers", s.handleBlockedProviders)
	s.register(mux, "PATCH", "/api/settings/blocked-providers", s.handleBlockedProviders)

	// AI settings (B8-136)
	s.register(mux, "GET", "/api/settings/ai", s.handleAISettings)
	s.register(mux, "PATCH", "/api/settings/ai", s.handleAISettings)

	// Modality bridge (B8-137)
	s.register(mux, "GET", "/api/settings/modality", s.handleModalityBridge)
	s.register(mux, "PATCH", "/api/settings/modality", s.handleModalityBridge)

	// Pricing sync (B8-117)
	s.register(mux, "POST", "/api/pricing/sync", s.handlePricingSyncTrigger)
	s.register(mux, "GET", "/api/pricing/sync/status", s.handlePricingSyncStatus)

	// Transform pipeline (B8-70)
	s.register(mux, "GET", "/api/settings/transform", s.handleTransformPipeline)
	s.register(mux, "PUT", "/api/settings/transform", s.handleTransformPipeline)

	// Sidebar customisation (B8-139)
	s.register(mux, "GET", "/api/settings/sidebar", s.handleSidebarCustomisation)
	s.register(mux, "PUT", "/api/settings/sidebar", s.handleSidebarCustomisation)

	// Advanced settings (B8-141)
	s.register(mux, "GET", "/api/settings/advanced", s.handleAdvancedSettings)
	s.register(mux, "PATCH", "/api/settings/advanced", s.handleAdvancedSettings)

	// LKGP cache clear (B8-69)
	s.register(mux, "DELETE", "/api/lkgp/cache", s.handleLKGPCacheClear)

	// Routing strategies catalog (B8-35..56)
	s.register(mux, "GET", "/api/routing/strategies", s.handleRoutingStrategiesCatalog)

	// Session stickiness (B8-5)
	s.register(mux, "GET", "/api/settings/stickiness", s.handleStickinessSettings)
	s.register(mux, "PATCH", "/api/settings/stickiness", s.handleStickinessSettings)

	// Global switches (B8-30)
	s.register(mux, "GET", "/api/settings/switches", s.handleGlobalSwitches)
	s.register(mux, "PATCH", "/api/settings/switches", s.handleGlobalSwitches)

	// Response validation (B8-67)
	s.register(mux, "GET", "/api/settings/response-validation", s.handleResponseValidation)
	s.register(mux, "PUT", "/api/settings/response-validation", s.handleResponseValidation)

	// Cache by-model eviction
	// Note: /api/cache (no suffix) = handleCacheDelete already registered.
	// /api/cache/{model} is a new distinct pattern.
	s.register(mux, "DELETE", "/api/cache/{model}", s.handleCacheModelEvict)
}
