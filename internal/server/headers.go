// M2.1/M2.2 — per-response telemetry headers + correlation ids
// (Wave 1 Phase B, A8).
//
// One choke point: every inference response (chat, embeddings,
// rerank, responses, messages — streaming and non-streaming,
// errors included) carries its telemetry through
// writeResponseHeaders, built by responseMeta from the request's
// own route trace, the response usage, and the real ledger price
// map. Nothing here is invented:
//
//   - Cost comes from ledger.Prices (the same map the ledger
//     prices rows with — see internal/ledger Prices.CostWithCached,
//     M1.6). When no price is configured for the (provider, model)
//     pair, the cost header is ABSENT — never a fabricated 0.
//   - Tokens come from the upstream-reported usage block on the
//     response. When the upstream reported no usage (rerank without
//     a usage block, an error before any attempt), the token
//     headers are ABSENT — never 0.
//   - Provider/model/fallback/latency come from the request's own
//     route trace (attribution sink first, engine-global last
//     trace as fallback — the same read recordMetrics uses).
//     Upstream latency is the sum of the attempts' real LatencyMs;
//     router overhead is total minus upstream, emitted only when
//     both are known and the difference is non-negative.
//   - A cache hit bills nothing: Cost-USD is the KNOWN 0, and the
//     savings are the cached response's own tokens priced from the
//     ledger map — the same formula as
//     cache.PersistentCache.RecordHit.
//
// Streaming: only what is known at stream start rides in the
// headers (request id, version, model — writeStreamStartHeaders);
// everything else rides in a final SSE comment trailer
// (": codvian-meta {...}") written before the terminal [DONE] /
// message_stop, and in one log row (logResponseMeta), because
// headers can no longer be sent once the stream is committed.
//
// Correlation (M2.2): withLogging assigns the id — the sanitized
// inbound X-Request-Id when the client sent one, else a fresh
// "req-<32 hex>" — stores it on the request context, echoes it as
// X-Codvian-Request-Id (and X-Request-Id) on every response
// including auth/validation errors, and logs it on the access-log
// row. The id is never derived from a key or any secret.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Response-header names (M2.1 contract — tests pin the literals).
const (
	headerCodvianRequestID        = "X-Codvian-Request-Id"
	headerCodvianVersion          = "X-Codvian-Version"
	headerCodvianProvider         = "X-Codvian-Provider"
	headerCodvianModel            = "X-Codvian-Model"
	headerCodvianCostUSD          = "X-Codvian-Cost-USD"
	headerCodvianTokensIn         = "X-Codvian-Tokens-In"
	headerCodvianTokensOut        = "X-Codvian-Tokens-Out"
	headerCodvianTokensCached     = "X-Codvian-Tokens-Cached"
	headerCodvianLatencyMs        = "X-Codvian-Latency-Ms"
	headerCodvianUpstreamMs       = "X-Codvian-Upstream-Latency-Ms"
	headerCodvianOverheadUs       = "X-Codvian-Router-Overhead-Us"
	headerCodvianTokensPerSec     = "X-Codvian-Tokens-Per-Second"
	headerCodvianFallbackAttempts = "X-Codvian-Fallback-Attempts"
	headerCodvianFallbackPath     = "X-Codvian-Fallback-Path"
	headerCodvianCache            = "X-Codvian-Cache"
	headerCodvianCacheSimilarity  = "X-Codvian-Cache-Similarity"
	headerCodvianCacheSavedTokens = "X-Codvian-Cache-Saved-Tokens"
	headerCodvianCacheSavedUSD    = "X-Codvian-Cache-Saved-USD"
	// headerOmniRouteCostSaved is the OmniRoute name for the same
	// figure (PARITY Area 11, cost-saved reporting): the USD cost
	// a cache hit saved, priced from the real ledger price map —
	// the same value as X-Codvian-Cache-Saved-USD, byte for byte.
	// Emitted only when the saving was priced (HasCacheSavedUSD):
	// on a cache miss, or when no price is configured for the
	// model, it stays absent — never a fabricated 0. A genuinely
	// free ($0-priced) model hit emits "0.0000000000", matching
	// OmniRoute's rule that 0 is a valid saved cost
	// (omnirouteResponseMeta.ts: costSavedUsd "pass 0 to
	// explicitly mark a free-model HIT that saved nothing").
	headerOmniRouteCostSaved = "X-OmniRoute-Cost-Saved"
)

// ---------------------------------------------------------------------------
// Correlation id (M2.2).
// ---------------------------------------------------------------------------

type ctxKeyRequestID struct{}

// newRequestID mints a fresh correlation id. crypto/rand only; the
// time-based fallback keeps the id unique even if the OS RNG ever
// fails, and never blocks a request on entropy.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(b[:])
}

// sanitizeRequestID accepts an inbound X-Request-Id only when it is
// 1–128 chars of [A-Za-z0-9._-]: anything else (control characters,
// whitespace, header-injection shapes, absurd length) is rejected
// and the caller mints a fresh id instead of echoing garbage.
func sanitizeRequestID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 128 {
		return ""
	}
	for _, r := range v {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' {
			continue
		}
		return ""
	}
	return v
}

// requestIDFor returns the correlation id for one inbound request:
// the sanitized inbound X-Request-Id when present, else a fresh id.
func requestIDFor(r *http.Request) string {
	if id := sanitizeRequestID(r.Header.Get("X-Request-Id")); id != "" {
		return id
	}
	return newRequestID()
}

// requestIDFromContext reads the correlation id the logging
// middleware stored on the request context. "" when none was
// stored (or ctx is nil) — never a panic, never a guess.
func requestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return id
}

// ---------------------------------------------------------------------------
// ResponseMeta — everything the choke point can report about one
// finished response. Zero value = nothing known: every field whose
// value is genuinely unknown stays ABSENT from the wire, never 0.
// ---------------------------------------------------------------------------

type ResponseMeta struct {
	RequestID string
	Provider  string
	Model     string

	// Usage is the upstream-reported token split; HasUsage means
	// it came from a real report (LocalEstimate included — the
	// router's own count when the upstream reported nothing — the
	// ledger records the same split, so the header and the ledger
	// can never disagree about which tokens were served).
	Usage    provider.Usage
	HasUsage bool

	// CostUSD is the USD cost priced from the real ledger price
	// map; HasCost means it was priced (a cache hit is a KNOWN $0
	// — no provider was called — not an unknown price).
	CostUSD float64
	HasCost bool

	// Started is when the handler started; Latency overrides it
	// when the caller measured the span itself. Either way the
	// header carries the real elapsed time, never a constant.
	Started time.Time
	Latency time.Duration

	// UpstreamLatency is the sum of the route-trace attempts'
	// real LatencyMs; HasUpstreamLatency means at least one
	// attempt reported a positive latency.
	UpstreamLatency    time.Duration
	HasUpstreamLatency bool

	// FallbackAttempts is len(attempts)-1 from the request's own
	// trace (0 = first target served); HasFallback means a trace
	// with at least one attempt was available at all.
	FallbackAttempts int
	HasFallback      bool
	// FallbackPath is "provider/model -> provider/model" over the
	// attempts, present only when at least one fallback happened.
	FallbackPath string

	// Cache is "exact" or "semantic" on a proactive cache hit.
	Cache string
	// CacheSimilarity is the semantic cosine similarity (3 dp on
	// the wire); HasCacheSimilarity means a semantic hit reported
	// one. Exact hits carry no similarity — it stays absent.
	CacheSimilarity    float32
	HasCacheSimilarity bool
	// CacheSavedTokens is the cached response's own token count
	// (prompt + completion) — the tokens the hit saved re-spending.
	// CacheSavedUSD prices them from the ledger map; when no price
	// is configured the USD figure stays absent while the token
	// count is still reported.
	CacheSavedTokens int
	CacheSavedUSD    float64
	HasCacheSavings  bool
	HasCacheSavedUSD bool
}

// responseMeta builds the telemetry meta for one finished inference
// call: provider/model/fallback/upstream-latency from the request's
// own route trace, tokens from the response usage, cost from the
// real ledger price map. resp may be nil (error before any response
// — the trace fields are still reported when known).
func (s *Server) responseMeta(ctx context.Context, req provider.ChatRequest, resp *provider.ChatResponse, started time.Time) ResponseMeta {
	meta := ResponseMeta{Started: started, RequestID: requestIDFromContext(ctx)}
	if resp != nil {
		// A zero usage block means the upstream reported no
		// usage at all (rerank without a usage block): the
		// token/cost headers stay ABSENT rather than 0. A real
		// report always carries a nonzero token count on at
		// least one axis.
		if resp.Usage != (provider.Usage{}) {
			meta.HasUsage = true
			meta.Usage = resp.Usage
		}
		if resp.Model != "" {
			meta.Model = resp.Model
		}
	}
	if meta.Model == "" {
		meta.Model = req.Model
	}
	if ctx != nil {
		if trace := s.requestTrace(ctx); trace != nil && len(trace.Attempts) > 0 {
			last := trace.Attempts[len(trace.Attempts)-1]
			meta.Provider = last.Provider
			if meta.Model == "" {
				meta.Model = last.Model
			}
			meta.HasFallback = true
			meta.FallbackAttempts = len(trace.Attempts) - 1
			if len(trace.Attempts) > 1 {
				parts := make([]string, len(trace.Attempts))
				for i, a := range trace.Attempts {
					parts[i] = a.Provider + "/" + a.Model
				}
				meta.FallbackPath = strings.Join(parts, " -> ")
			}
			var upstreamMs int64
			for _, a := range trace.Attempts {
				upstreamMs += a.LatencyMs
			}
			if upstreamMs > 0 {
				meta.HasUpstreamLatency = true
				meta.UpstreamLatency = time.Duration(upstreamMs) * time.Millisecond
			}
		}
	}
	// Cache hit: no provider was called, so this request cost the
	// KNOWN $0, and the savings are the cached response's own
	// tokens priced from the ledger map (model-level rate: a cached
	// entry names no provider). Same formula as
	// cache.PersistentCache.RecordHit.
	if chr, ok := s.engine.(cacheHitReporter); ok {
		if kind, sim, hit := chr.LastCacheHit(); hit {
			meta.Cache = kind
			if kind == "semantic" {
				meta.HasCacheSimilarity = true
				meta.CacheSimilarity = sim
			}
			meta.CostUSD = 0
			meta.HasCost = true
			meta = s.withCacheSavings(meta)
			return meta
		}
	}
	if meta.HasUsage {
		if prices := s.getLedgerPrices(); prices.Known(meta.Provider, meta.Model) {
			meta.CostUSD = prices.CostWithCached(meta.Provider, meta.Model, meta.Usage.PromptTokens, meta.Usage.CompletionTokens, meta.Usage.CachedTokens)
			meta.HasCost = true
		}
	}
	return meta
}

// verbatimMeta builds the telemetry meta for a verbatim relay:
// the relay target and the model it served are known; tokens and
// cost are not (the body is forwarded byte-for-byte and the ledger
// records no row for a relayed request — the standing relay gap),
// so they stay absent rather than guessed.
func (s *Server) verbatimMeta(target *relayTarget, head messagesHeader, start time.Time) ResponseMeta {
	meta := ResponseMeta{Started: start}
	if target != nil {
		meta.Provider = target.ProviderID
		meta.Model = target.UpstreamModel
	}
	if meta.Model == "" {
		meta.Model = head.Model
	}
	return meta
}

// withCacheSavings fills the savings fields of a meta for a cache
// hit from the response usage, priced from the real ledger price
// map — pair-first with model-level fallback (the pair rate when
// the trace names a provider with a pair price, else the
// model-level rate; the same token×rate formula as
// cache.PersistentCache.RecordHit, which prices model-level only,
// so the two figures can differ when a pair price exists). When no
// price is configured the USD figure stays absent while the token
// count is still reported.
func (s *Server) withCacheSavings(meta ResponseMeta) ResponseMeta {
	if !meta.HasUsage {
		return meta
	}
	if saved := meta.Usage.PromptTokens + meta.Usage.CompletionTokens; saved > 0 {
		meta.CacheSavedTokens = saved
		meta.HasCacheSavings = true
		if prices := s.getLedgerPrices(); prices.Known(meta.Provider, meta.Model) {
			meta.CacheSavedUSD = prices.Cost(meta.Provider, meta.Model, meta.Usage.PromptTokens, meta.Usage.CompletionTokens)
			meta.HasCacheSavedUSD = true
		}
	}
	return meta
}

// latencyOf resolves the total latency of a meta: the explicit
// Latency when set, else the elapsed time since Started. 0 when
// neither is available — the caller must treat 0 as "unknown".
func (meta ResponseMeta) latencyOf() time.Duration {
	if meta.Latency > 0 {
		return meta.Latency
	}
	if !meta.Started.IsZero() {
		return time.Since(meta.Started)
	}
	return 0
}

// tokensPerSecond is completion tokens per second of generation:
// over the upstream latency when the trace reported one (the time
// the provider spent generating), else over the total latency. 0
// when there is nothing to divide (no completion tokens, or no
// measured duration) — the caller omits the header instead of
// emitting a fabricated rate.
func (meta ResponseMeta) tokensPerSecond() float64 {
	if !meta.HasUsage || meta.Usage.CompletionTokens <= 0 {
		return 0
	}
	d := meta.UpstreamLatency
	if !meta.HasUpstreamLatency || d <= 0 {
		d = meta.latencyOf()
	}
	if d <= 0 {
		return 0
	}
	return float64(meta.Usage.CompletionTokens) / d.Seconds()
}

// writeResponseHeaders is the single choke point (M2.1): it writes
// every per-response telemetry header that is KNOWN for this
// response, and nothing else. Call it immediately before the
// response body is written (WriteHeader / writeJSON / the first SSE
// write) — headers set after the body starts are silently dropped
// by net/http. Decision/Budget headers keep their existing setters
// (setDecisionHeader, setAutoRouteHeader, setStrategyHeader,
// checkRequestBudgetOverage) at the same call sites; this writer
// covers the telemetry set only, so the two never drift apart.
func (s *Server) writeResponseHeaders(w http.ResponseWriter, ctx context.Context, meta ResponseMeta) {
	h := w.Header()
	if id := meta.RequestID; id != "" {
		h.Set(headerCodvianRequestID, id)
	} else if id := requestIDFromContext(ctx); id != "" {
		h.Set(headerCodvianRequestID, id)
	}
	if s.version != "" {
		h.Set(headerCodvianVersion, s.version)
	}
	if meta.Provider != "" {
		h.Set(headerCodvianProvider, meta.Provider)
	}
	if meta.Model != "" {
		h.Set(headerCodvianModel, meta.Model)
	}
	if meta.HasCost {
		h.Set(headerCodvianCostUSD, strconv.FormatFloat(meta.CostUSD, 'f', 10, 64))
	}
	if meta.HasUsage {
		h.Set(headerCodvianTokensIn, strconv.Itoa(meta.Usage.PromptTokens))
		h.Set(headerCodvianTokensOut, strconv.Itoa(meta.Usage.CompletionTokens))
		h.Set(headerCodvianTokensCached, strconv.Itoa(meta.Usage.CachedTokens))
	}
	latency := meta.latencyOf()
	// Emit the latency header whenever timing exists (an
	// explicit Latency or a Started timestamp), even when the
	// measured span truncates to 0 ms: a sub-millisecond
	// request still took measurable time, and a missing header
	// reads as a bug to the client. On Windows the clock can
	// return identical timestamps for a short span, so
	// latencyOf() may be exactly 0 — that is a 0 ms
	// measurement, not an unknown one. When neither timing
	// source exists the header stays absent (0 would be a
	// fabricated measurement, not a truncated one).
	if meta.Latency > 0 || !meta.Started.IsZero() {
		h.Set(headerCodvianLatencyMs, strconv.FormatInt(latency.Milliseconds(), 10))
	}
	if meta.HasUpstreamLatency {
		h.Set(headerCodvianUpstreamMs, strconv.FormatInt(meta.UpstreamLatency.Milliseconds(), 10))
		if overhead := latency - meta.UpstreamLatency; overhead >= 0 {
			h.Set(headerCodvianOverheadUs, strconv.FormatInt(overhead.Microseconds(), 10))
		}
	}
	if tps := meta.tokensPerSecond(); tps > 0 {
		h.Set(headerCodvianTokensPerSec, strconv.FormatFloat(tps, 'f', 2, 64))
	}
	if meta.HasFallback {
		h.Set(headerCodvianFallbackAttempts, strconv.Itoa(meta.FallbackAttempts))
		if meta.FallbackPath != "" {
			h.Set(headerCodvianFallbackPath, meta.FallbackPath)
		}
	}
	if meta.Cache != "" {
		h.Set(headerCodvianCache, meta.Cache)
		if meta.HasCacheSimilarity {
			h.Set(headerCodvianCacheSimilarity, fmt.Sprintf("%.3f", meta.CacheSimilarity))
		}
	}
	if meta.HasCacheSavings {
		h.Set(headerCodvianCacheSavedTokens, strconv.Itoa(meta.CacheSavedTokens))
	}
	if meta.HasCacheSavedUSD {
		h.Set(headerCodvianCacheSavedUSD, strconv.FormatFloat(meta.CacheSavedUSD, 'f', 10, 64))
		// OmniRoute name for the same priced saving (see the
		// const above): same value, same 10-dp format, so the
		// two headers can never disagree. A memory-only hit
		// carries it too — withCacheSavings prices from the
		// ledger map, not from the cache_savings table, so
		// persistence is irrelevant to this header (the table
		// feeds only the aggregate GET /api/cache/stats block,
		// whose savings_note covers the memory-only case).
		h.Set(headerOmniRouteCostSaved, strconv.FormatFloat(meta.CacheSavedUSD, 'f', 10, 64))
	}
}

// writeStreamStartHeaders sets the stream headers that are already
// known before the first event: request id, version, model. The
// logging middleware already set id + version on the underlying
// writer; this re-asserts them on the writer the stream handler
// holds (the statusRecorder delegates Header() to the same map, so
// the set is idempotent) and adds the model, which the middleware
// cannot know. Call before the first SSE write commits the headers.
func (s *Server) writeStreamStartHeaders(w http.ResponseWriter, ctx context.Context, model string) {
	h := w.Header()
	if id := requestIDFromContext(ctx); id != "" {
		h.Set(headerCodvianRequestID, id)
	}
	if s.version != "" {
		h.Set(headerCodvianVersion, s.version)
	}
	if model != "" {
		h.Set(headerCodvianModel, model)
	}
}

// trailerJSON renders the meta as the JSON object carried by the
// final SSE comment. Only known values are present — the same
// absent-means-unknown rule as the headers, so a client parsing
// the trailer never mistakes a missing measurement for 0.
func (meta ResponseMeta) trailerJSON() ([]byte, error) {
	m := map[string]any{}
	if meta.RequestID != "" {
		m["request_id"] = meta.RequestID
	}
	if meta.Provider != "" {
		m["provider"] = meta.Provider
	}
	if meta.Model != "" {
		m["model"] = meta.Model
	}
	if meta.HasCost {
		m["cost_usd"] = meta.CostUSD
	}
	if meta.HasUsage {
		m["tokens_in"] = meta.Usage.PromptTokens
		m["tokens_out"] = meta.Usage.CompletionTokens
		m["tokens_cached"] = meta.Usage.CachedTokens
	}
	if latency := meta.latencyOf(); latency > 0 {
		m["latency_ms"] = latency.Milliseconds()
		if meta.HasUpstreamLatency {
			m["upstream_latency_ms"] = meta.UpstreamLatency.Milliseconds()
			if overhead := latency - meta.UpstreamLatency; overhead >= 0 {
				m["router_overhead_us"] = overhead.Microseconds()
			}
		}
	} else if meta.HasUpstreamLatency {
		m["upstream_latency_ms"] = meta.UpstreamLatency.Milliseconds()
	}
	if tps := meta.tokensPerSecond(); tps > 0 {
		m["tokens_per_second"] = tps
	}
	if meta.HasFallback {
		m["fallback_attempts"] = meta.FallbackAttempts
		if meta.FallbackPath != "" {
			m["fallback_path"] = meta.FallbackPath
		}
	}
	if meta.Cache != "" {
		m["cache"] = meta.Cache
		if meta.HasCacheSimilarity {
			m["cache_similarity"] = meta.CacheSimilarity
		}
	}
	if meta.HasCacheSavings {
		m["cache_saved_tokens"] = meta.CacheSavedTokens
	}
	if meta.HasCacheSavedUSD {
		m["cache_saved_usd"] = meta.CacheSavedUSD
		// Same figure under the OmniRoute name, so a stream
		// trailer reader sees the same saving as a header
		// reader (cache hits are non-streaming in practice,
		// but the trailer is the same meta).
		m["cost_saved_usd"] = meta.CacheSavedUSD
	}
	return json.Marshal(m)
}

// streamTrailerString renders the final SSE comment line for a
// stream, or "" when it cannot be rendered. Streams whose writer
// flushes a batcher first (the OpenAI paths' writeSSENow) pass
// this line through that writer, so the trailer cannot overtake
// buffered token events.
func (s *Server) streamTrailerString(ctx context.Context, meta ResponseMeta) string {
	if meta.RequestID == "" {
		meta.RequestID = requestIDFromContext(ctx)
	}
	b, err := meta.trailerJSON()
	if err != nil {
		return ""
	}
	return ": codvian-meta " + string(b) + "\n\n"
}

// writeStreamTrailer writes the final SSE comment carrying the
// values that were not known when the stream's headers were sent.
// It is a comment (": …"), so every SSE client ignores it unless
// it looks for it. Call after the last content event and before
// the terminal [DONE] / message_stop — and through the same writer
// the stream used (writeSSENow on the OpenAI paths, so a batcher
// holding token events flushes first and the trailer cannot
// overtake them). The caller owns flushing.
func (s *Server) writeStreamTrailer(w io.Writer, ctx context.Context, meta ResponseMeta) {
	if line := s.streamTrailerString(ctx, meta); line != "" {
		_, _ = io.WriteString(w, line)
	}
}

// logResponseMeta logs one finished inference response as a single
// row carrying the trailer values (M2.1: the stream trailer values
// also land in the log row, since headers can no longer carry them
// after commit). Only known values are logged — an unknown cost is
// absent from the row, never 0. Never logs prompt or response
// content, keys, or key ids.
func (s *Server) logResponseMeta(meta ResponseMeta) {
	parts := []string{}
	if meta.RequestID != "" {
		parts = append(parts, "request_id="+meta.RequestID)
	}
	if meta.Provider != "" {
		parts = append(parts, "provider="+meta.Provider)
	}
	if meta.Model != "" {
		parts = append(parts, "model="+meta.Model)
	}
	if meta.HasCost {
		parts = append(parts, fmt.Sprintf("cost_usd=%.10f", meta.CostUSD))
	}
	if meta.HasUsage {
		parts = append(parts, fmt.Sprintf("tokens_in=%d tokens_out=%d tokens_cached=%d",
			meta.Usage.PromptTokens, meta.Usage.CompletionTokens, meta.Usage.CachedTokens))
	}
	if latency := meta.latencyOf(); latency > 0 {
		parts = append(parts, fmt.Sprintf("latency_ms=%d", latency.Milliseconds()))
	}
	if meta.HasUpstreamLatency {
		parts = append(parts, fmt.Sprintf("upstream_latency_ms=%d", meta.UpstreamLatency.Milliseconds()))
	}
	if tps := meta.tokensPerSecond(); tps > 0 {
		parts = append(parts, fmt.Sprintf("tokens_per_second=%.2f", tps))
	}
	if meta.HasFallback {
		parts = append(parts, fmt.Sprintf("fallback_attempts=%d", meta.FallbackAttempts))
		if meta.FallbackPath != "" {
			parts = append(parts, "fallback_path="+meta.FallbackPath)
		}
	}
	if meta.Cache != "" {
		parts = append(parts, "cache="+meta.Cache)
		if meta.HasCacheSavings {
			parts = append(parts, fmt.Sprintf("cache_saved_tokens=%d", meta.CacheSavedTokens))
		}
		if meta.HasCacheSavedUSD {
			parts = append(parts, fmt.Sprintf("cache_saved_usd=%.10f", meta.CacheSavedUSD))
		}
	}
	if len(parts) == 0 {
		return
	}
	s.logger.Printf("response %s", strings.Join(parts, " "))
}
