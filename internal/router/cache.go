// In-engine proactive cache consultation (A13 Stream A, gap H1).
//
// Before this file, caching was a property of the serve-mode ASSEMBLY, not of
// the engine: the degrade wrapper (internal/degrade.StaleEngine) and the
// server relay consulted the caches, while a bare router.Engine consults
// nothing. Any caller that constructed an Engine directly therefore got no
// caching at all.
//
// This file gives the Engine the same capability, through the SAME cache
// package and the SAME gates the wrapper applies, so there is one rule set:
//
//   - the exact layer first (cache.ScopedKey over tenant + model + the full
//     generation surface), then
//   - the semantic layer (provider.SemanticWindowText, last 3 non-system
//     turns), with the 50-turn bypass.
//
// Gates, all identical to the wrapper's lookup:
//   - ServeCache off  -> no lookup and no store (the feature is opt-in);
//   - CacheControls.BypassCache() -> no lookup and no store;
//   - routing contract present -> no lookup and no store (a contracted
//     request must route, never be served from cache);
//   - temperature not cacheable -> no lookup and no store;
//   - tool-carrying / tool-role / Responses-endpoint requests -> not
//     cacheable at all (see ChatRequest.cacheableForEngine);
//   - lossy-compressed requests never consult the semantic layer;
//   - conversations past provider.SemanticMaxTurns never consult it either.
//
// Store gates additionally use cache.ShouldStore (finish_reason "length",
// tool calls, temperature), so a truncated or tool-carrying answer can never
// enter the cache from this path.
//
// The Engine stays the cache OWNER here; the degrade wrapper keeps its
// stale-fallback role and its own consultation for the legacy assembly. A
// hit served by the engine is reported through LastCacheHit and never dials a
// provider, so it is free by construction.
package router

import (
	"context"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// DefaultEngineStoreTTL is how long an engine-stored entry lives: the same
// one hour the wrapper and relay use, so the three write paths cannot drift.
const DefaultEngineStoreTTL = time.Hour

// engineCacheable reports whether this request may use the cache at all.
// It mirrors degrade.cacheable, restated here because the router must not
// import the wrapper (that would invert the dependency: the wrapper wraps the
// engine).
func engineCacheable(req provider.ChatRequest) bool {
	if req.Endpoint == provider.EndpointResponses || req.PreviousResponseID != "" {
		return false
	}
	if len(req.Tools) > 0 || len(req.ToolChoice) > 0 {
		return false
	}
	for _, m := range req.Messages {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			return false
		}
	}
	return true
}

// engineHasRoutingContract reports whether the request carries a non-empty
// per-request routing contract. A contracted request must actually route.
func engineHasRoutingContract(req provider.ChatRequest) bool {
	return req.Routing != nil && !req.Routing.Empty()
}

// engineCacheGates holds the shared pre-lookup gates, so the exact and
// semantic branches cannot drift apart.
func (e *Engine) engineCacheGates(req provider.ChatRequest) bool {
	if !e.cfg.ServeCache {
		return false
	}
	if e.exactCacheRef() == nil && e.semanticCacheRef() == nil {
		return false
	}
	if !engineCacheable(req) {
		return false
	}
	if req.CacheControls.BypassCache() {
		return false
	}
	if engineHasRoutingContract(req) {
		return false
	}
	if !cache.CacheableTemperature(req.Temperature, e.cfg.AllowNonZeroCacheTemp) {
		return false
	}
	return true
}

// engineScopedKey derives the exact-layer key for req: cache.ScopedKey over
// the tenant (gateway key id), the requested model and the generation
// surface. A key error means the request cannot be cached at all.
func engineScopedKey(req provider.ChatRequest) (string, error) {
	if cc := req.CacheControls; cc != nil && cc.KeyOverride != "" {
		// A key override replaces the whole key, tenant+model scoped,
		// exactly as the wrapper and relay do, so all three paths
		// agree on what an override means.
		return cache.ScopedKey(cache.KeyInput{
			Tenant:   req.CallerID,
			Model:    req.Model,
			Messages: []any{"codvian-cache-key-override", cc.KeyOverride},
		})
	}
	return cache.ScopedKey(cache.KeyInput{
		Tenant:          req.CallerID,
		Model:           req.Model,
		Messages:        req.Messages,
		Tools:           req.Tools,
		ToolChoice:      req.ToolChoice,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		MaxTokens:       req.MaxTokens,
		Stop:            req.Stop,
		ReasoningEffort: req.ReasoningEffort,
		Compression:     req.Compression,
	})
}

// engineCacheLookup consults the exact layer and then the semantic layer.
// It returns the served response, the layer name ("exact" / "semantic") and
// true on a hit. layer is "" and ok is false on a miss or on any failed gate.
func (e *Engine) engineCacheLookup(ctx context.Context, req provider.ChatRequest) (cache.CachedResponse, string, bool) {
	if !e.engineCacheGates(req) {
		return cache.CachedResponse{}, "", false
	}
	exact := e.exactCacheRef()
	if exact != nil && req.CacheControls.UseExact() {
		if key, err := engineScopedKey(req); err == nil {
			if hit, found := exact.Get(ctx, key); found {
				return hit, "exact", true
			}
			e.noteCacheMiss("exact")
		}
	}
	semantic := e.semanticCacheRef()
	if semantic != nil && req.CacheControls.UseSemantic() &&
		!engineLossyCompressed(req) &&
		!provider.SemanticTurnsExceeded(req.Messages) {
		if text := provider.SemanticWindowText(req.Messages); text != "" {
			if hit, found := e.engineSemanticLookup(ctx, req, text); found {
				return hit.Response, "semantic", true
			}
			e.noteCacheMiss("semantic")
		}
	}
	return cache.CachedResponse{}, "", false
}

// engineLossyCompressed reports whether lossy compression was applied: such a
// prompt is a different question, so it never matches a stored paraphrase.
func engineLossyCompressed(req provider.ChatRequest) bool {
	if req.Compressed != nil && req.Compressed.LossyApplied {
		return true
	}
	return req.Compression != nil && req.Compression.LossyMaxPromptTokens > 0
}

// engineCacheStore stores a completed response under BOTH layers, applying
// cache.ShouldStore so a truncated, hot or tool-carrying answer never enters
// the cache.
func (e *Engine) engineCacheStore(ctx context.Context, req provider.ChatRequest, resp *provider.ChatResponse) {
	if resp == nil {
		return
	}
	if !e.cfg.ServeCache {
		return
	}
	if e.exactCacheRef() == nil && e.semanticCacheRef() == nil {
		return
	}
	if !engineCacheable(req) {
		return
	}
	if !req.CacheControls.AllowStore() {
		return
	}
	if engineHasRoutingContract(req) {
		return
	}
	if !cache.ShouldStore(resp.FinishReason, req.Temperature, e.cfg.AllowNonZeroCacheTemp, len(resp.ToolCalls) > 0) {
		return
	}
	entry := cache.CachedResponse{
		Content: resp.Content,
		Model:   resp.Model,
		Usage: cache.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			CachedTokens:     resp.Usage.CachedTokens,
			ReasoningTokens:  resp.Usage.ReasoningTokens,
		},
	}
	if entry.Model == "" {
		entry.Model = req.Model
	}
	ttl := DefaultEngineStoreTTL
	if t := req.CacheControls.EffectiveTTL(); t > 0 {
		ttl = t
	}
	if exact := e.exactCacheRef(); exact != nil && req.CacheControls.UseExact() {
		if key, err := engineScopedKey(req); err == nil {
			exact.Set(ctx, key, entry, ttl)
		}
	}
	if semantic := e.semanticCacheRef(); semantic != nil && req.CacheControls.UseSemantic() &&
		!provider.SemanticTurnsExceeded(req.Messages) && !engineLossyCompressed(req) {
		if text := provider.SemanticWindowText(req.Messages); text != "" {
			e.engineSemanticStore(ctx, req, text, entry, ttl)
		}
	}
}

// CachedResponseFor returns the response to serve for a cache hit, so callers
// (and tests) build it the same way: content, model and zero cost.
func CachedResponseFor(hit cache.CachedResponse, req provider.ChatRequest) *provider.ChatResponse {
	model := hit.Model
	if model == "" {
		model = req.Model
	}
	return &provider.ChatResponse{
		Content:      hit.Content,
		Model:        model,
		FinishReason: "stop",
		Usage: provider.Usage{
			PromptTokens:     hit.Usage.PromptTokens,
			CompletionTokens: hit.Usage.CompletionTokens,
			TotalTokens:      hit.Usage.TotalTokens,
			CachedTokens:     hit.Usage.CachedTokens,
			ReasoningTokens:  hit.Usage.ReasoningTokens,
		},
	}
}

// --- semantic layer, engine side (A13 Stream A, H1) ---

// engineSemanticThresholdFloor is the floor the engine applies. It is the
// same 0.85 the cache enforces as its own minimum, stated once here so the
// engine cannot silently request a looser floor than the store allows.
const engineSemanticThresholdFloor float32 = 0.85

// semanticThresholded is the optional per-call-floor lookup the semantic
// stores offer (the in-memory layer and the persistent adapter both do).
type semanticThresholded interface {
	GetTextForModelWithThreshold(ctx context.Context, model, text string, threshold float32) (cache.SemanticEntry, bool)
}

// semanticScopedThresholded is the tenant + temperature scoped variant:
// the entry must have been stored by the same tenant at the same
// temperature bucket. The engine prefers it whenever the store offers it,
// so a paraphrase from another tenant can never be served.
type semanticScopedThresholded interface {
	GetTextForModelScopedWithThreshold(ctx context.Context, model, text, tenant string, temperature float64, threshold float32) (cache.SemanticEntry, bool)
}

// semanticScopedStorer is the scoped store form: the entry records the
// tenant and temperature bucket that produced it.
type semanticScopedStorer interface {
	SetTextForModelScoped(ctx context.Context, model, text, tenant string, temperature float64, entry cache.SemanticEntry, ttl time.Duration)
}

// engineSemanticLookup is the engine's semantic lookup with the full
// scoping the wrapper uses: tenant + temperature bucket when the store
// supports it (fail-closed — a legacy unscoped entry is never served), else
// the plain string lookup with the floor applied.
func (e *Engine) engineSemanticLookup(ctx context.Context, req provider.ChatRequest, text string) (cache.SemanticEntry, bool) {
	semantic := e.semanticCacheRef()
	if semantic == nil {
		return cache.SemanticEntry{}, false
	}
	if scoped, ok := semantic.(semanticScopedThresholded); ok {
		return scoped.GetTextForModelScopedWithThreshold(ctx, req.Model, text, req.CallerID, req.Temperature, engineSemanticThresholdFloor)
	}
	if th, ok := semantic.(semanticThresholded); ok {
		return th.GetTextForModelWithThreshold(ctx, req.Model, text, engineSemanticThresholdFloor)
	}
	entry, found := semantic.GetTextForModel(ctx, req.Model, text)
	if !found {
		return cache.SemanticEntry{}, false
	}
	// The store offered no floor parameter: enforce the floor here, so a
	// lax third-party store cannot widen what the engine serves.
	if entry.Similarity < engineSemanticThresholdFloor {
		return cache.SemanticEntry{}, false
	}
	return entry, true
}

// engineSemanticStore records one semantic entry, scoped to the tenant and
// temperature bucket when the store supports it.
func (e *Engine) engineSemanticStore(ctx context.Context, req provider.ChatRequest, text string, entry cache.CachedResponse, ttl time.Duration) {
	semantic := e.semanticCacheRef()
	if semantic == nil {
		return
	}
	if scoped, ok := semantic.(semanticScopedStorer); ok {
		scoped.SetTextForModelScoped(ctx, req.Model, text, req.CallerID, req.Temperature, cache.SemanticEntry{Response: entry}, ttl)
		return
	}
	semantic.SetTextForModel(ctx, req.Model, text, cache.SemanticEntry{Response: entry}, ttl)
}

// noteCacheMiss records a miss for the layer, when a metrics sink is wired.
func (e *Engine) noteCacheMiss(layer string) {
	if e.cfg.CacheMetrics == nil {
		return
	}
	e.cfg.CacheMetrics.IncCacheMiss(layer)
}

// noteCacheHit records a hit for the layer, when a metrics sink is wired.
func (e *Engine) noteCacheHit(layer string) {
	if e.cfg.CacheMetrics == nil {
		return
	}
	e.cfg.CacheMetrics.IncCacheHit(layer)
}

// exactCacheRef / semanticCacheRef read the configured layers under the
// engine lock, so a runtime wiring change cannot race a request.
func (e *Engine) exactCacheRef() cache.ExactStore {
	e.mu.Lock()
	c := e.exactCache
	e.mu.Unlock()
	return c
}

// semanticCacheRef returns the configured semantic layer.
func (e *Engine) semanticCacheRef() cache.SemanticStore {
	e.mu.Lock()
	c := e.semanticCache
	e.mu.Unlock()
	return c
}
