package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// B8 Combo Extended functionality (B8-3, B8-58, B8-61..66, B8-77, B8-79, B8-83)

// ---------------------------------------------------------------------------
// Capability-specific behaviour (B8-3): GET/PUT /api/combos/{id}/capabilities
// ---------------------------------------------------------------------------

type ComboCapabilityConfig struct {
	Capability string `json:"capability"` // chat, search, embeddings, images, video, music, ocr, audio
	Strategy   string `json:"strategy,omitempty"`
	Model      string `json:"model,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Enabled    bool   `json:"enabled"`
}

type comboCapabilitiesStore struct {
	mu           sync.RWMutex
	capabilities map[string]map[string]ComboCapabilityConfig // comboID -> capability -> config
}

var globalComboCapabilities = &comboCapabilitiesStore{
	capabilities: map[string]map[string]ComboCapabilityConfig{},
}

func (s *Server) handleComboCapabilities(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/combos/")
	id = strings.TrimSuffix(id, "/capabilities")
	if id == "" {
		writeError(w, http.StatusBadRequest, "combo id required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		globalComboCapabilities.mu.RLock()
		defer globalComboCapabilities.mu.RUnlock()
		caps := globalComboCapabilities.capabilities[id]
		if caps == nil {
			caps = map[string]ComboCapabilityConfig{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"combo_id":     id,
			"capabilities": caps,
		})
	case http.MethodPut:
		var req struct {
			Capabilities []ComboCapabilityConfig `json:"capabilities"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		globalComboCapabilities.mu.Lock()
		defer globalComboCapabilities.mu.Unlock()
		if globalComboCapabilities.capabilities[id] == nil {
			globalComboCapabilities.capabilities[id] = map[string]ComboCapabilityConfig{}
		}
		for _, c := range req.Capabilities {
			globalComboCapabilities.capabilities[id][c.Capability] = c
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"combo_id":     id,
			"capabilities": globalComboCapabilities.capabilities[id],
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Combo tuning settings (B8-58, B8-61..66): GET/PATCH /api/settings/combo-tuning
// ---------------------------------------------------------------------------

type ComboTuningSettings struct {
	MaxConcurrency       int            `json:"max_concurrency"`        // B8-58
	QueueTimeoutMs       int            `json:"queue_timeout_ms"`       // B8-58
	QueueDepth           int            `json:"queue_depth"`            // B8-58
	MaxNestingDepth      int            `json:"max_nesting_depth"`      // B8-61
	ProviderOverrides    map[string]int `json:"provider_overrides"`     // B8-62: provider -> max retries
	FailoverBeforeRetry  bool           `json:"failover_before_retry"`  // B8-63
	MaxSetRetries        int            `json:"max_set_retries"`        // B8-64
	SetRetryDelayMs      int            `json:"set_retry_delay_ms"`     // B8-64
	QuotaOnlyFallback    bool           `json:"quota_only_fallback"`    // B8-65
	ReasoningTokenBuffer bool           `json:"reasoning_token_buffer"` // B8-66
}

type comboTuningStore struct {
	mu       sync.RWMutex
	settings ComboTuningSettings
}

var globalComboTuning = &comboTuningStore{
	settings: ComboTuningSettings{
		MaxConcurrency:       50,
		QueueTimeoutMs:       30000,
		QueueDepth:           100,
		MaxNestingDepth:      5,
		ProviderOverrides:    map[string]int{},
		FailoverBeforeRetry:  true,
		MaxSetRetries:        3,
		SetRetryDelayMs:      500,
		QuotaOnlyFallback:    false,
		ReasoningTokenBuffer: true,
	},
}

func (s *Server) handleComboTuning(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalComboTuning.mu.RLock()
		defer globalComboTuning.mu.RUnlock()
		writeJSON(w, http.StatusOK, globalComboTuning.settings)
	case http.MethodPatch:
		var req ComboTuningSettings
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		globalComboTuning.mu.Lock()
		defer globalComboTuning.mu.Unlock()
		if req.MaxConcurrency > 0 {
			globalComboTuning.settings.MaxConcurrency = req.MaxConcurrency
		}
		if req.QueueTimeoutMs > 0 {
			globalComboTuning.settings.QueueTimeoutMs = req.QueueTimeoutMs
		}
		if req.QueueDepth > 0 {
			globalComboTuning.settings.QueueDepth = req.QueueDepth
		}
		if req.MaxNestingDepth > 0 {
			globalComboTuning.settings.MaxNestingDepth = req.MaxNestingDepth
		}
		if req.ProviderOverrides != nil {
			globalComboTuning.settings.ProviderOverrides = req.ProviderOverrides
		}
		globalComboTuning.settings.FailoverBeforeRetry = req.FailoverBeforeRetry
		if req.MaxSetRetries > 0 {
			globalComboTuning.settings.MaxSetRetries = req.MaxSetRetries
		}
		if req.SetRetryDelayMs > 0 {
			globalComboTuning.settings.SetRetryDelayMs = req.SetRetryDelayMs
		}
		globalComboTuning.settings.QuotaOnlyFallback = req.QuotaOnlyFallback
		globalComboTuning.settings.ReasoningTokenBuffer = req.ReasoningTokenBuffer

		writeJSON(w, http.StatusOK, globalComboTuning.settings)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// 16-factor scoring + 6 mode packs (B8-77): GET /api/settings/scoring-modes
// ---------------------------------------------------------------------------

type ScoringFactorInfo struct {
	Name        string  `json:"name"`
	Weight      float64 `json:"default_weight"`
	Description string  `json:"description"`
}

type ScoringModePack struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Weights     map[string]float64 `json:"weights"`
}

var allScoringFactors = []ScoringFactorInfo{
	{Name: "latency_p95", Weight: 0.15, Description: "95th percentile latency history"},
	{Name: "error_rate", Weight: 0.15, Description: "Recent error and timeout rate"},
	{Name: "quota_headroom", Weight: 0.12, Description: "Remaining quota before throttle or reset"},
	{Name: "cost_rank", Weight: 0.10, Description: "Price per million tokens relative to pool"},
	{Name: "context_fit", Weight: 0.08, Description: "Closeness of context window to request size"},
	{Name: "cache_affinity", Weight: 0.08, Description: "Prompt cache hit likelihood"},
	{Name: "throughput", Weight: 0.06, Description: "Tokens per second streaming performance"},
	{Name: "health_score", Weight: 0.06, Description: "Active heartbeat probe status"},
	{Name: "retry_count", Weight: 0.04, Description: "Recent retry volume"},
	{Name: "token_velocity", Weight: 0.04, Description: "Rolling consumption velocity"},
	{Name: "concurrency_load", Weight: 0.03, Description: "In-flight calls relative to limit"},
	{Name: "circuit_breaker_state", Weight: 0.03, Description: "Open/half-open breaker penalty"},
	{Name: "priority_tier", Weight: 0.02, Description: "Operator preferred provider flag"},
	{Name: "model_version_freshness", Weight: 0.01, Description: "Latest revision preference"},
	{Name: "streaming_jitter", Weight: 0.01, Description: "Inter-chunk delivery variance"},
	{Name: "time_to_first_token", Weight: 0.01, Description: "Initial chunk latency"},
}

var allScoringModePacks = []ScoringModePack{
	{
		ID:          "balanced",
		Name:        "Balanced",
		Description: "Standard multi-factor distribution balancing speed, quality, and cost",
		Weights: map[string]float64{
			"latency_p95": 0.15, "error_rate": 0.15, "quota_headroom": 0.12, "cost_rank": 0.10,
			"context_fit": 0.08, "cache_affinity": 0.08, "throughput": 0.06, "health_score": 0.06,
		},
	},
	{
		ID:          "cost_first",
		Name:        "Cost Minimizer",
		Description: "Prioritizes cheapest model and cache affinity over latency",
		Weights: map[string]float64{
			"cost_rank": 0.35, "cache_affinity": 0.20, "quota_headroom": 0.15, "error_rate": 0.15, "latency_p95": 0.15,
		},
	},
	{
		ID:          "speed_first",
		Name:        "Speed Demon",
		Description: "Prioritizes lowest TTFT, p95 latency and throughput",
		Weights: map[string]float64{
			"latency_p95": 0.30, "time_to_first_token": 0.25, "throughput": 0.20, "error_rate": 0.15, "streaming_jitter": 0.10,
		},
	},
	{
		ID:          "quality_first",
		Name:        "Quality First",
		Description: "Prioritizes model tier and context fit for complex reasoning",
		Weights: map[string]float64{
			"context_fit": 0.30, "priority_tier": 0.25, "error_rate": 0.20, "health_score": 0.15, "cache_affinity": 0.10,
		},
	},
	{
		ID:          "quota_saver",
		Name:        "Quota Preserver",
		Description: "Prevents account exhaustion by spreading load across resets",
		Weights: map[string]float64{
			"quota_headroom": 0.40, "token_velocity": 0.25, "concurrency_load": 0.20, "error_rate": 0.15,
		},
	},
	{
		ID:          "custom",
		Name:        "Custom Operator Weights",
		Description: "User-defined weights specified per factor",
		Weights:     map[string]float64{},
	},
}

func (s *Server) handleScoringModes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"factors":    allScoringFactors,
		"mode_packs": allScoringModePacks,
	})
}

// ---------------------------------------------------------------------------
// Account fallback strategies (9 choices) (B8-79): GET/PATCH /api/settings/account-fallback
// ---------------------------------------------------------------------------

var validAccountFallbackStrategies = []string{
	"next_in_pool",
	"cheapest_reserve",
	"fastest_reserve",
	"round_robin_reserve",
	"spillover_all",
	"fail_immediate",
	"escalate_tier",
	"cooldown_bypass",
	"operator_alert",
}

type accountFallbackStore struct {
	mu       sync.RWMutex
	strategy string
}

var globalAccountFallback = &accountFallbackStore{strategy: "next_in_pool"}

func (s *Server) handleAccountFallback(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		globalAccountFallback.mu.RLock()
		defer globalAccountFallback.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"strategy":          globalAccountFallback.strategy,
			"valid_strategies": validAccountFallbackStrategies,
		})
	case http.MethodPatch:
		var req struct {
			Strategy string `json:"strategy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		valid := false
		for _, s := range validAccountFallbackStrategies {
			if s == req.Strategy {
				valid = true
				break
			}
		}
		if !valid {
			writeError(w, http.StatusBadRequest, "unknown strategy: "+req.Strategy)
			return
		}
		globalAccountFallback.mu.Lock()
		globalAccountFallback.strategy = req.Strategy
		globalAccountFallback.mu.Unlock()

		writeJSON(w, http.StatusOK, map[string]any{
			"strategy": req.Strategy,
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Combo Studio live cascade view (B8-83): GET /api/combos/{id}/studio-cascade
// ---------------------------------------------------------------------------

func (s *Server) handleComboStudioCascade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/combos/")
	id = strings.TrimSuffix(id, "/studio-cascade")
	if id == "" {
		writeError(w, http.StatusBadRequest, "combo id required")
		return
	}

	// Build cascade step view from registered combo
	steps := []map[string]any{}
	store := s.combosStore()
	store.mu.RLock()
	if rec, ok := store.records[id]; ok {
		for i, st := range rec.chain {
			steps = append(steps, map[string]any{
				"step":     i + 1,
				"provider": st.provider,
				"model":    st.model,
				"effort":   st.effort,
				"status":   "active",
				"healthy":  true,
			})
		}
	}
	store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"combo_id": id,
		"steps":    steps,
		"count":    len(steps),
	})
}

// registerB8CombosRoutes registers extended combo routes.
func (s *Server) registerB8CombosRoutes(mux *http.ServeMux) {
	s.register(mux, "GET", "/api/combos/{id}/capabilities", s.handleComboCapabilities)
	s.register(mux, "PUT", "/api/combos/{id}/capabilities", s.handleComboCapabilities)
	s.register(mux, "GET", "/api/settings/combo-tuning", s.handleComboTuning)
	s.register(mux, "PATCH", "/api/settings/combo-tuning", s.handleComboTuning)
	s.register(mux, "GET", "/api/settings/scoring-modes", s.handleScoringModes)
	s.register(mux, "GET", "/api/settings/account-fallback", s.handleAccountFallback)
	s.register(mux, "PATCH", "/api/settings/account-fallback", s.handleAccountFallback)
	s.register(mux, "GET", "/api/combos/{id}/studio-cascade", s.handleComboStudioCascade)
}
