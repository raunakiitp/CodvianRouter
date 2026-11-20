// Media verify-first surfaces (M8.20): POST /v1/search, POST /v1/ocr,
// POST /v1/videos/generations, GET /v1/videos/{id}, POST
// /v1/music/generations.
//
// Each surface exists ONLY because a provider already in the
// catalog documents a real API for it (see
// internal/provider/media_verify.go for the per-provider evidence
// and the deliberately-NOT-built list — Sora shut down 2026-09-24,
// Veo 3.1 preview shut down 2026-10-22, Suno/Udio have no public
// API). The handler validates the gateway request, builds a
// provider.MediaPassthroughRequest naming the pinned provider +
// the provider-native upstream path, and hands it to
// Engine.MediaPassthrough — the same single-attempt engine path as
// embeddings (chain, budgets, guardrails, ledger, trace; exactly
// one upstream attempt, never replayed). The request body travels
// VERBATIM to the upstream and the upstream status + body come back
// VERBATIM: the gateway never rebuilds, re-encodes or reinterprets
// either side, so provider-native fields it does not know reach
// the upstream untouched and the upstream validates them.
//
// Shapes (OpenAI-wire-shaped where a standard shape exists; the
// provider-native shape where none does):
//   - /v1/search: Perplexity Search API shape — {query, max_results,
//     search_type, ...}; NO model field (the upstream takes none).
//     The routing/ledger model is the provider id "perplexity".
//   - /v1/ocr: Mistral OCR shape — {model, document, ...}; model
//     required (e.g. "mistral-ocr-latest").
//   - /v1/videos/generations: Runway text-to-video shape — {model,
//     promptText, ratio, duration, ...}; model required. Answers
//     the Runway task object ({id, status, ...}); poll it at
//     GET /v1/videos/{id}, which returns the task ({id, status,
//     output, ...}). promptText is NOT required gateway-side:
//     Runway makes it optional for some models (seedance2_5, Kling
//     3/O3) — the upstream validates per model.
//   - /v1/music/generations: MiniMax shape — {model, prompt, lyrics,
//     ...}; model required (e.g. "music-3.0"). MiniMax reports
//     failures inside a 200 envelope (base_resp.status_code); the
//     envelope is returned verbatim, so the client sees the real
//     error. Access restriction (MiniMax docs, 2026-08-20): the paid
//     music APIs are closed to NEW users — existing paying users
//     only. The gateway routes an entitled key; it cannot conjure
//     access.
//
// Routing: these handlers ARE registered — guard-table rows
// in guard.go:233-238 and route registrations in server.go:
// 1404-1409 (POST /v1/search, POST /v1/ocr, POST
// /v1/videos/generations, GET /v1/videos/{id}, POST
// /v1/music/generations), all TierManagement, scope "chat".
// Gateway-key auth comes from that registration (requireAuth,
// TierManagement, scope "chat", like /v1/embeddings); the
// handlers themselves assume an authorised caller, exactly
// like handleEmbedRerank.
//
// Search caching (G13, parity tracker Area 6 row "Search cache
// (60 s)"): POST /v1/search answers are cached for 60 s in a
// SearchCache[searchCachedResult] (internal/cache/search_cache.go:
// same keying, TTL, caps and coalescing as the reference router's
// searchCache.ts), in front of the single upstream call below —
// a repeated identical search inside the TTL is served without
// dialling Perplexity again, and concurrent identical searches
// share one upstream fetch. Errors are never stored. The cache
// sits AFTER every gate (guardrails, budgets, traffic), so a
// refused request never reads or warms it. Only the search
// surface is cached: OCR/video/music are generative or
// task-scoped, never repeatable lookups. A client that sends
// X-Codvian-Cache-No-Store: true or Cache-Control: no-cache, or
// a server built with WithSearchCacheTTL(0), bypasses the cache
// AND the coalescing (a ttl <= 0 fetch always runs its own
// upstream call). The cached answer is served with
// X-Codvian-Search-Cache: hit; a fresh upstream answer with
// X-Codvian-Search-Cache: miss.
//
// No price is recorded for any of these calls (no price-book entry
// for these models — cost stays unknown, never invented), and no
// cap is invented (Q4): max_results, ratio, duration and the rest
// pass through unvalidated; the upstream enforces its own limits.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// mediaVerifyEngine is the optional engine capability these
// handlers need (router.Engine implements it; degrade.StaleEngine
// forwards it with no caching and no stale fallback — a media call
// is never served from cache). A server whose engine does not
// implement it answers 503 — an honest "this deployment cannot
// serve this surface", never a faked result.
type mediaVerifyEngine interface {
	MediaPassthrough(context.Context, provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error)
}

// searchCachedResult is one cached POST /v1/search answer: the
// upstream status and body, verbatim. Only 2xx answers are ever
// stored (the fetch below returns an error for anything else,
// and GetOrCoalesce never stores a failed fetch).
type searchCachedResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// errSearchBadUpstream marks a non-2xx (or empty) upstream answer
// from a foreign engine inside the search-cache fetch: the fetch
// must fail — not store — so the handler's existing nil/bad
// response branch (502, never a faked 200) runs unchanged.
var errSearchBadUpstream = errors.New("media upstream returned no usable response")

// searchCacheState is one Server's search cache plus its TTL
// override (G13). server.go is owned by Wave 3, so the cache
// cannot live on the Server struct; it lives in the
// package-level registry below, keyed by *Server. One entry per
// Server: test servers are distinct pointers, so tests never
// share entries, and the production server (one per process)
// owns exactly one cache. The registry entry is created lazily
// and never removed — a Server lives for the life of its
// process (or test), so the leak is one small struct per Server.
type searchCacheState struct {
	mu     sync.Mutex
	cache  *cache.SearchCache[searchCachedResult]
	ttl    time.Duration
	ttlSet bool
}

var searchCacheStates sync.Map // *Server -> *searchCacheState

// searchCacheStateFor returns (creating it) this Server's search
// cache state. The default TTL is cache.DefaultSearchTTL (60 s).
func (s *Server) searchCacheStateFor() *searchCacheState {
	if v, ok := searchCacheStates.Load(s); ok {
		return v.(*searchCacheState)
	}
	st := &searchCacheState{ttl: cache.DefaultSearchTTL}
	actual, _ := searchCacheStates.LoadOrStore(s, st)
	return actual.(*searchCacheState)
}

// WithSearchCacheTTL overrides the POST /v1/search cache TTL for
// one Server: a positive value is the entry lifetime, and 0 (or
// a negative value) disables the search cache entirely — every
// search then runs its own upstream call, bypassing the cache
// AND the coalescing (the ttl <= 0 rule of
// SearchCache.GetOrCoalesce). Tests use it to prove the bypass;
// operators use it to turn search caching off. The default
// without this option is 60 s.
func WithSearchCacheTTL(d time.Duration) Option {
	return func(s *Server) {
		st := s.searchCacheStateFor()
		st.mu.Lock()
		defer st.mu.Unlock()
		st.ttl = d
		st.ttlSet = true
		st.cache = nil // rebuilt lazily with the new TTL
	}
}

// searchCache returns this Server's SearchCache, creating it
// lazily. NewSearchCacheWithConfig treats a ttl <= 0 as "use
// the default", so a disabled cache (ttl <= 0) still gets a
// normally-built cache — the bypass happens at call time, by
// passing ttl 0 to GetOrCoalesce (see searchCacheKeyTTL).
func (s *Server) searchCache() *cache.SearchCache[searchCachedResult] {
	st := s.searchCacheStateFor()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cache == nil {
		st.cache = cache.NewSearchCache[searchCachedResult]()
	}
	return st.cache
}

// searchCacheKeyTTL derives the cache key and the effective TTL
// for one search request body. The key is cache.SearchKey over
// the Perplexity parameters (query, provider, search type, max
// results, country, language, filters) — any other top-level
// body field (search_domain_filter, return_images, ...) is
// folded into the filters slot, so two requests that differ in
// ANY field key differently and can never share an answer. A
// body that cannot be parsed keys as "" (GetOrCoalesce then
// fetches directly, never serving a wrong entry — the handler
// already validated the body before this point, so an unparseable
// body here is unreachable in practice). The TTL is 0 — bypass
// cache AND coalescing — when the client asked for no caching
// (X-Codvian-Cache-No-Store: true or Cache-Control: no-cache)
// or the server's TTL override is <= 0; otherwise the server's
// TTL (default 60 s).
func (s *Server) searchCacheKeyTTL(r *http.Request, body []byte) (string, time.Duration) {
	st := s.searchCacheStateFor()
	st.mu.Lock()
	ttl, ttlSet := st.ttl, st.ttlSet
	st.mu.Unlock()
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Codvian-Cache-No-Store")), "true") ||
		strings.Contains(strings.ToLower(r.Header.Get("Cache-Control")), "no-cache") {
		ttl = 0
	} else if ttlSet && ttl <= 0 {
		ttl = 0
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", ttl
	}
	// Query: a string, or an array of up to 5 strings (validated
	// by mediaSearchQueries before this point). Joined with
	// \x1f so a multi-query request keys on the whole set.
	var queries []string
	if raw, ok := fields["query"]; ok {
		var single string
		if err := json.Unmarshal(raw, &single); err == nil {
			queries = []string{single}
		} else {
			_ = json.Unmarshal(raw, &queries)
		}
	}
	var maxResults int
	_ = json.Unmarshal(fields["max_results"], &maxResults)
	var searchType, country, language string
	_ = json.Unmarshal(fields["search_type"], &searchType)
	_ = json.Unmarshal(fields["country"], &country)
	_ = json.Unmarshal(fields["language"], &language)
	var filters any
	if raw, ok := fields["filters"]; ok {
		_ = json.Unmarshal(raw, &filters)
	} else {
		// Any other top-level field is part of the key: fold
		// the remainder into the filters slot.
		rest := make(map[string]json.RawMessage, len(fields))
		for k, v := range fields {
			switch k {
			case "query", "max_results", "search_type", "country", "language", "filters":
			default:
				rest[k] = v
			}
		}
		if len(rest) > 0 {
			filters = rest
		}
	}
	return cache.SearchKey(strings.Join(queries, "\x1f"), "perplexity", searchType, maxResults, country, language, filters), ttl
}

// mediaPassthroughSearch runs the single upstream media call, with
// the G13 search cache in front of the search surface only: every
// other surface calls the engine directly, exactly as before. For
// search, a cache hit (or a waiter that joined an in-flight
// identical fetch) returns the stored answer with hit=true and
// zero upstream calls; a miss runs exactly one upstream fetch and
// stores its 2xx answer. A failed fetch (engine error, or a
// non-2xx/empty answer marked errSearchBadUpstream) stores
// nothing and the error reaches the caller unchanged, so the
// handler's error branches run exactly as they did uncached.
func (s *Server) mediaPassthroughSearch(ctx context.Context, ee mediaVerifyEngine, spec mediaSpec, r *http.Request, body []byte, mediaReq provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error, bool) {
	if spec.name != "search" {
		resp, err := ee.MediaPassthrough(ctx, mediaReq)
		return resp, err, false
	}
	key, ttl := s.searchCacheKeyTTL(r, body)
	cached, hit, err := s.searchCache().GetOrCoalesce(ctx, key, ttl, func(fetchCtx context.Context) (searchCachedResult, error) {
		resp, err := ee.MediaPassthrough(fetchCtx, mediaReq)
		if err != nil {
			return searchCachedResult{}, err
		}
		if resp == nil || resp.Status < 200 || resp.Status >= 300 {
			return searchCachedResult{}, errSearchBadUpstream
		}
		return searchCachedResult{Status: resp.Status, Body: append(json.RawMessage(nil), resp.Body...)}, nil
	})
	if err != nil {
		return nil, err, false
	}
	return &provider.MediaPassthroughResponse{Status: cached.Status, Body: cached.Body}, nil, hit
}

// mediaSpec is one media surface: the pinned provider, the endpoint
// kind, the upstream method, and the parse step that validates the
// gateway request and derives the upstream path, the routing model,
// the bookkeeping texts and any per-call upstream headers. The raw
// body is forwarded verbatim — parse only reads it, never rewrites
// it. For GET surfaces body is nil and parse reads the URL path.
type mediaSpec struct {
	name       string // "search" | "ocr" | "video" | "music", for errors + X-Codvian-Endpoint
	providerID string
	endpoint   provider.EndpointKind
	method     string
	// maxRequestBody caps the gateway request body for this
	// surface; 0 means the 10 MiB default (like chat and
	// embeddings). Only /v1/ocr overrides it (50 MiB — see
	// mediaOCRSpec).
	maxRequestBody int64
	parse          func(r *http.Request, body []byte) (path, model string, texts []string, extra map[string]string, err error)
}

// handleSearch serves POST /v1/search.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	s.serveMediaVerify(w, r, mediaSearchSpec)
}

// handleOCR serves POST /v1/ocr.
func (s *Server) handleOCR(w http.ResponseWriter, r *http.Request) {
	s.serveMediaVerify(w, r, mediaOCRSpec)
}

// handleVideoGenerations serves POST /v1/videos/generations.
func (s *Server) handleVideoGenerations(w http.ResponseWriter, r *http.Request) {
	s.serveMediaVerify(w, r, mediaVideoCreateSpec)
}

// handleVideoTask serves GET /v1/videos/{id} (Runway task retrieve).
func (s *Server) handleVideoTask(w http.ResponseWriter, r *http.Request) {
	s.serveMediaVerify(w, r, mediaVideoTaskSpec)
}

// handleMusic serves POST /v1/music/generations.
func (s *Server) handleMusic(w http.ResponseWriter, r *http.Request) {
	s.serveMediaVerify(w, r, mediaMusicSpec)
}

// mediaSearchSpec: Perplexity Search API. The upstream takes no
// model, so the routing/ledger model is the provider id. Query is
// a string or an array of up to 5 strings (Perplexity's own limit —
// enforced here because the docs state it, not invented).
var mediaSearchSpec = mediaSpec{
	name:       "search",
	providerID: "perplexity",
	endpoint:   provider.EndpointSearch,
	method:     http.MethodPost,
	parse: func(r *http.Request, body []byte) (string, string, []string, map[string]string, error) {
		var req struct {
			Query json.RawMessage `json:"query"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", nil, nil, err
		}
		texts, err := mediaSearchQueries(req.Query)
		if err != nil {
			return "", "", nil, nil, err
		}
		return "search", "perplexity", texts, nil, nil
	},
}

// mediaSearchQueries validates + extracts the Perplexity query:
// one non-empty string, or an array of 1–5 non-empty strings.
func mediaSearchQueries(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errMediaField("query is required")
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if strings.TrimSpace(single) == "" {
			return nil, errMediaField("query must not be empty")
		}
		return []string{single}, nil
	}
	var multi []string
	if err := json.Unmarshal(raw, &multi); err != nil {
		return nil, errMediaField("query must be a string or an array of strings")
	}
	if len(multi) == 0 || len(multi) > 5 {
		return nil, errMediaField("query array must hold 1 to 5 queries")
	}
	for _, q := range multi {
		if strings.TrimSpace(q) == "" {
			return nil, errMediaField("query must not be empty")
		}
	}
	return multi, nil
}

// mediaOCRSpec: Mistral OCR API. Model + document required; the
// document object travels verbatim (URL, base64 or file id — the
// upstream validates the shape per model). No bookkeeping text:
// a document URL / base64 blob is not scannable text.
//
// REQUEST CAP (F4): the body cap for this surface is 50 MiB, not
// the 10 MiB default — Mistral OCR accepts documents up to 50 MB
// / 1000 pages, so a base64 document between 10–50 MB was refused
// 400 by the gateway although the upstream would take it. URL
// documents (the common path) are unaffected — they are a few
// hundred bytes either way; the cap only matters for inline
// base64 documents.
var mediaOCRSpec = mediaSpec{
	name:           "ocr",
	providerID:     "mistral",
	endpoint:       provider.EndpointOCR,
	method:         http.MethodPost,
	maxRequestBody: 50 << 20,
	parse: func(r *http.Request, body []byte) (string, string, []string, map[string]string, error) {
		var req struct {
			Model    string          `json:"model"`
			Document json.RawMessage `json:"document"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", nil, nil, err
		}
		if strings.TrimSpace(req.Model) == "" {
			return "", "", nil, nil, errMediaField("model is required")
		}
		if len(req.Document) == 0 || string(req.Document) == "null" {
			return "", "", nil, nil, errMediaField("document is required")
		}
		var doc map[string]any
		if err := json.Unmarshal(req.Document, &doc); err != nil {
			return "", "", nil, nil, errMediaField("document must be an object")
		}
		return "ocr", req.Model, nil, nil, nil
	},
}

// mediaVideoCreateSpec: Runway text-to-video task create. Model
// required; promptText extracted leniently for the guardrail scan
// (it is optional for some Runway models, so its absence is not a
// gateway error — the upstream validates per model). Runway
// requires X-Runway-Version: 2024-11-06 on every request
// (docs.dev.runwayml.com).
var mediaVideoCreateSpec = mediaSpec{
	name:       "video",
	providerID: "runwayml",
	endpoint:   provider.EndpointVideo,
	method:     http.MethodPost,
	parse: func(r *http.Request, body []byte) (string, string, []string, map[string]string, error) {
		var req struct {
			Model      string `json:"model"`
			PromptText string `json:"promptText"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", nil, nil, err
		}
		if strings.TrimSpace(req.Model) == "" {
			return "", "", nil, nil, errMediaField("model is required")
		}
		var texts []string
		if strings.TrimSpace(req.PromptText) != "" {
			texts = []string{req.PromptText}
		}
		return "text_to_video", req.Model, texts, map[string]string{"X-Runway-Version": "2024-11-06"}, nil
	},
}

// mediaVideoTaskSpec: Runway task retrieve. The id comes from the
// URL path (/v1/videos/{id}); the routing/ledger model is the
// provider id, because a task id alone does not name a model.
var mediaVideoTaskSpec = mediaSpec{
	name:       "video",
	providerID: "runwayml",
	endpoint:   provider.EndpointVideo,
	method:     http.MethodGet,
	parse: func(r *http.Request, body []byte) (string, string, []string, map[string]string, error) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
		if id == "" || strings.Contains(id, "/") {
			return "", "", nil, nil, errMediaField("video id is required in the path")
		}
		return "tasks/" + id, "runwayml", nil, map[string]string{"X-Runway-Version": "2024-11-06"}, nil
	},
}

// mediaMusicSpec: MiniMax Music Generation API. Model required;
// prompt + lyrics extracted leniently for the guardrail scan (the
// per-model rules — lyrics required except instrumental/cover with
// lyrics_optimizer — are the upstream's, not the gateway's).
//
// TIMEOUT (F3, resolved): MiniMax music generation is SYNCHRONOUS —
// one request holds the connection until the song is done, which
// takes tens of seconds to several minutes — so the music surface
// does NOT use the server-wide DefaultTimeout (120s). Its bound is
// mediaTimeout (below): a routing.media_timeouts_ms["music"] config
// value when set, else DefaultMediaMusicTimeout (10 minutes). 10
// minutes is a CHOSEN bound, not a provider-documented one: no
// documented upper bound for the generation time exists in the
// MiniMax docs the code cites, and a generation running past the
// bound is still cut off as a 504 after the paid job ($0.15 per
// generation, MiniMax docs) has run upstream, with no task id to
// recover the finished song — exactly-once forbids a retry, so the
// client pays and gets nothing. The longer bound shrinks that
// window from "any song over 2 minutes" to "any song over 10
// minutes"; it does not close it. Closing it needs an async task
// model, which MiniMax does not offer for music (no task id in its
// API — unlike Runway video, which already polls via GET
// /v1/videos/{id}); building a second task store for a synchronous
// call with nothing to poll was rejected in favour of this
// per-endpoint timeout. The provider HTTP client's own 120 s
// timeout does not re-impose the old bound: MediaPassthrough runs
// with no client-level timeout, so the request context — bounded
// here — is the sole bound (see internal/provider/media_verify.go).
var mediaMusicSpec = mediaSpec{
	name:       "music",
	providerID: "minimax",
	endpoint:   provider.EndpointMusic,
	method:     http.MethodPost,
	parse: func(r *http.Request, body []byte) (string, string, []string, map[string]string, error) {
		var req struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			Lyrics string `json:"lyrics"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", nil, nil, err
		}
		if strings.TrimSpace(req.Model) == "" {
			return "", "", nil, nil, errMediaField("model is required")
		}
		var texts []string
		if strings.TrimSpace(req.Prompt) != "" {
			texts = append(texts, req.Prompt)
		}
		if strings.TrimSpace(req.Lyrics) != "" {
			texts = append(texts, req.Lyrics)
		}
		return "music_generation", req.Model, texts, nil, nil
	},
}

// DefaultMediaMusicTimeout is the request timeout for POST
// /v1/music/generations when no per-endpoint timeout is configured
// (M8.20 F3). Music generation is synchronous and takes tens of
// seconds to several minutes; no provider-documented upper bound
// exists in the MiniMax docs the code cites, so 10 minutes is a
// chosen bound with headroom over "several minutes", not a
// provider promise — the operator overrides it with
// routing.media_timeouts_ms["music"].
const DefaultMediaMusicTimeout = 10 * time.Minute

// mediaTimeout resolves the request timeout for one media surface.
// Precedence, highest first:
//
//  1. WithMediaTimeouts — a positive programmatic override.
//  2. routing.media_timeouts_ms[spec.name] from the loaded config
//     (read via the routing snapshot, so a live config swap can
//     never race the read); 0 or an absent key means unset.
//  3. DefaultMediaMusicTimeout for music (see the const).
//  4. s.timeout — the server-wide timeout (DefaultTimeout 120 s
//     unless WithTimeout overrode it) — for every other surface.
//
// The DefaultTimeout global is never changed: non-music surfaces
// keep exactly the behaviour they had before this slice.
func (s *Server) mediaTimeout(spec mediaSpec) time.Duration {
	if d, ok := s.mediaTimeouts[spec.name]; ok && d > 0 {
		return d
	}
	if rc := s.routingConfig(); rc != nil {
		if ms, ok := rc.MediaTimeoutsMs[spec.name]; ok && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	if spec.name == "music" {
		return DefaultMediaMusicTimeout
	}
	return s.timeout
}

// errMediaField is a validation error for one gateway request field.
func errMediaField(msg string) error {
	return &mediaFieldError{msg: msg}
}

type mediaFieldError struct{ msg string }

func (e *mediaFieldError) Error() string { return "invalid request: " + e.msg }

// mediaStatusForEngineError is statusForEngineError with one
// media correction (F2): an upstream 404 — e.g. GET
// /v1/videos/{id} for a task id Runway has never seen — is the
// caller's unknown id, not an upstream outage, so it answers
// 404, not 502. The intercept applies to ALL five media
// surfaces, not just video-task GET (a search, OCR,
// video-create or music upstream 404 is likewise the caller's
// unknown id/resource, never an outage). classifyStatus maps a
// 404 to DecisionFallback, and statusForEngineError only
// preserves 4xx statuses on DecisionStop, so without this the
// gateway would report "upstream down" for "no such task" and
// the client could not tell the two apart. The GET dials the
// upstream task endpoint on every call — there is no stored
// record and no polling loop; the client polls by re-GETting,
// and a 404 on any poll means the id does not exist upstream.
func (s *Server) mediaStatusForEngineError(err error) int {
	var ue *provider.UpstreamError
	if errors.As(err, &ue) && ue.StatusCode == http.StatusNotFound {
		return http.StatusNotFound
	}
	return s.statusForEngineError(err)
}

// serveMediaVerify is the shared handler for all five media
// surfaces, mirroring handleEmbedRerank: panic isolation, admission
// before I/O, the engine-capability 503, decode + validate, the
// guardrail / plugin / budget gauntlet before dialling, exactly one
// engine call, then the upstream status + body verbatim. The
// per-endpoint differences live in the mediaSpec.
func (s *Server) serveMediaVerify(w http.ResponseWriter, r *http.Request, spec mediaSpec) {
	handlerStart := time.Now()
	if r.Method != spec.method {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Panic isolation (CHECKLIST 165-166), mirroring
	// handleChatCompletions / handleEmbedRerank.
	var bookReq provider.ChatRequest
	engineCalled := false
	defer func() {
		if v := recover(); v != nil {
			s.noteServerPanic("server_"+string(spec.endpoint), v)
			s.writePanicFailure(w)
			if bookReq.Model != "" && (!engineCalled || s.traceHasNoAttempts()) {
				rowCtx, rowCancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.recordPanicRow(rowCtx, s.callerKeyID(r), bookReq, handlerStart)
				rowCancel()
			}
		}
	}()

	// Admission before I/O (CHECKLIST 277 (b)): a blocked or
	// over-limit key is refused BEFORE the request body is read.
	if s.preBodyAdmission(w, r) {
		return
	}

	// The request body is capped at 10 MiB, like chat and
	// embeddings — except /v1/ocr, whose per-spec cap is 50 MiB
	// (see mediaOCRSpec). GET surfaces carry no body.
	var body []byte
	if spec.method == http.MethodPost {
		cap := spec.maxRequestBody
		if cap == 0 {
			cap = 10 << 20
		}
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, cap))
		if err != nil {
			writeError(w, http.StatusBadRequest, "request body too large")
			return
		}

		// Security cluster (CHECKLIST 245): a client-supplied
		// Vertex location/project is rejected unless the admin
		// opted in.
		if ov, ok := provider.DetectVertexOverride(body); ok {
			if refused := s.checkClientVertexOverride(w, &ov); refused {
				return
			}
		}
	}

	// The per-request timeout is per-endpoint (M8.20 F3):
	// mediaTimeout gives music 10 minutes by default (or the
	// configured routing.media_timeouts_ms value) and every other
	// media surface the server-wide timeout (DefaultTimeout 120s
	// unless WithTimeout overrode it). A generation running past
	// its bound is cut off as a 504 by the context below.
	ctx, cancel := context.WithTimeout(r.Context(), s.mediaTimeout(spec))
	defer cancel()

	// A deployment whose engine cannot serve this surface says so
	// up front, before any guardrail or budget is evaluated: 503,
	// not a faked result.
	ee, ok := s.engine.(mediaVerifyEngine)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, spec.name+" is not available on this server")
		return
	}

	// Decode + validate once; the raw body is what travels
	// upstream, verbatim.
	path, model, texts, extra, err := spec.parse(r, body)
	if err != nil {
		if _, isField := err.(*mediaFieldError); isField {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// Readiness gate: a gateway-provided local capability that is
	// not ready answers 503 with Retry-After here, before any
	// provider is dialled.
	if !s.checkLocalReadiness(w, r, model) {
		return
	}

	// The synthetic bookkeeping request drives the shared
	// guardrail / budget / ledger / metrics machinery (see
	// internal/router/media_verify.go): it carries the
	// bookkeeping text as user messages, the endpoint kind, the
	// caller and the budget — never sent upstream.
	bookReq = provider.ChatRequest{
		Model:    model,
		Endpoint: spec.endpoint,
		CallerID: s.callerKeyID(r),
	}
	for _, t := range texts {
		bookReq.Messages = append(bookReq.Messages, provider.Message{Role: router.RoleUser, Content: t})
	}
	// Guardrails run on the bookkeeping text BEFORE any provider
	// is dialled. A blocked request makes zero provider calls.
	if blocked := s.applyGuardrails(ctx, w, &bookReq, s.keyGuardrailScope(r)); blocked {
		return
	}
	// Plugin pre-request hooks (CHECKLIST 219-220): the same
	// pipeline as chat completions, run on the synthetic
	// bookkeeping request after the built-in guardrails and before
	// every budget gate and the engine. A model rewrite by a plugin
	// is recorded on the routing / ledger label ONLY (the `model`
	// sync below) — it is NOT applied to the upstream request:
	// the media body travels verbatim and is never rebuilt from
	// the bookkeeping messages, so the upstream still receives
	// the ORIGINAL model while the ledger records the rewritten
	// one. That split is deliberate (the verbatim-body rule
	// forbids rebuilding the body), but it is a split — a plugin
	// that rewrites the model does not change what the upstream
	// is asked for. A message-content rewrite is likewise NOT
	// applied to the body.
	if blocked, newCtx := s.applyPlugins(ctx, w, &bookReq); blocked {
		return
	} else {
		ctx = newCtx
	}
	// F5: this syncs a plugin model rewrite onto the routing /
	// ledger label only (see the comment above) — the upstream
	// body below still carries the original model verbatim.
	if bookReq.Model != "" && bookReq.Model != model {
		model = bookReq.Model
	}
	// Per-key access schedule (M5.10): a key outside its allowed
	// hours is refused with a 403 here, before any budget is
	// touched.
	if refused := s.checkKeyAccessSchedule(w, r); refused {
		return
	}
	// Per-request budget (X-Codvian-Budget), the prompt budget
	// (estimate from the bookkeeping text) and the key budget: all
	// refused 402/429 BEFORE dialling, with zero provider hits.
	if refused := s.enforceRequestBudget(w, r, &bookReq); refused {
		return
	}
	if refused := s.enforcePromptBudget(w, r, &bookReq); refused {
		return
	}
	// M5.8: the budget check commits the prompt's estimated cost;
	// settled by the defer once this handler returns.
	if resID, refused := s.reserveKeyBudget(w, r, &bookReq); refused {
		return
	} else {
		defer s.settleReservation(resID)
	}
	// Per-key token limits (M5.3): global + model scopes for the
	// requested model, before the traffic check.
	if refused := s.checkKeyTokenLimits(w, r, "", bookReq.Model); refused {
		return
	}
	// Fair-share quota pools (M5.4): the caller's pools decide
	// before the traffic check.
	if refused := s.checkQuotaPools(w, r); refused {
		return
	}
	// Quota schedules (M5.9): the caller's pools' time windows
	// decide before the traffic check, right after the pool gate.
	if refused := s.checkQuotaSchedules(w, r); refused {
		return
	}
	// Internal budgets (M5.11): the operator's caps on total spend
	// (global / model / pool scopes) decide before the traffic
	// check, right after the schedule gate.
	if refused := s.checkInternalBudgets(w, r, bookReq.Model); refused {
		return
	}
	if !s.checkTeamBudget(w, r) {
		return
	}
	// Per-key traffic limits (CHECKLIST 195): same pre-call gate
	// as chat completions.
	if refused := s.checkKeyTrafficForChat(w, r, &bookReq); refused {
		return
	}
	// Per-key throttle delay + max sessions (M5.10): after every
	// refusal gate and before the engine is called; the session
	// slot is held for the whole handler via the defer.
	if !s.throttleKeyDelay(r) {
		return
	}
	sessionKey, admitted := s.tryEnterKeySession(w, r)
	if !admitted {
		return
	}
	defer s.exitKeySession(sessionKey)

	mediaReq := provider.MediaPassthroughRequest{
		ProviderID:       spec.providerID,
		Model:            model,
		Endpoint:         spec.endpoint,
		Method:           spec.method,
		Path:             path,
		Body:             json.RawMessage(body),
		ExtraHeaders:     extra,
		CallerID:         bookReq.CallerID,
		BudgetUSD:        bookReq.BudgetUSD,
		BudgetStrict:     bookReq.BudgetStrict,
		BookkeepingTexts: texts,
	}
	if spec.method == http.MethodGet {
		mediaReq.Body = nil
	}

	start := time.Now()
	engineCalled = true
	// G13: the search surface reads through the 60 s search cache
	// (mediaPassthroughSearch); every other surface calls the
	// engine directly, exactly as before. A non-2xx/empty answer
	// from a foreign engine arrives as errSearchBadUpstream and
	// is converted here to the nil-response case, so the 502
	// branch below runs unchanged.
	resp, err, searchCached := s.mediaPassthroughSearch(ctx, ee, spec, r, body, mediaReq)
	if err != nil && errors.Is(err, errSearchBadUpstream) {
		resp, err = nil, nil
	}
	if err != nil {
		s.recordEmbedMetrics(bookReq, "", err, start)
		meta := s.responseMeta(ctx, bookReq, nil, start)
		s.writeResponseHeaders(w, ctx, meta)
		errStatus := s.mediaStatusForEngineError(err)
		s.recordTelemetry(ctx, meta, errStatus)
		if errStatus == http.StatusNotFound {
			// F2: an upstream 404 (e.g. an unknown video task
			// id) answers 404 here, not the 502
			// handleEmbedEngineError would produce.
			writeError(w, http.StatusNotFound, "upstream resource not found")
			return
		}
		s.handleEmbedEngineError(w, err)
		return
	}
	if resp == nil || resp.Status < 200 || resp.Status >= 300 {
		// Unreachable through the real client (non-2xx becomes an
		// error), but a foreign engine must never produce a
		// success-shaped lie: 502, never a faked 200.
		badRespErr := errors.New("media upstream returned no usable response")
		s.recordEmbedMetrics(bookReq, "", badRespErr, start)
		meta := s.responseMeta(ctx, bookReq, nil, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, http.StatusBadGateway)
		writeServerError(w, http.StatusBadGateway, badRespErr.Error())
		return
	}
	// G13: name a cached search answer so a client (or a log
	// reader) can tell a cache serve from a fresh upstream call.
	// Set here, before any header is written: a hit means zero
	// upstream calls for this request, a miss means exactly one.
	if spec.name == "search" {
		if searchCached {
			w.Header().Set("X-Codvian-Search-Cache", "hit")
		} else {
			w.Header().Set("X-Codvian-Search-Cache", "miss")
		}
	}

	// Plugin post-response hooks (CHECKLIST 219-220): fail-open,
	// on the delivered response only. The response is synthesised
	// into the chat shape the hooks take: model is real; there is
	// no chat content and no token usage, so Content stays empty
	// and the token/cost headers stay absent, never 0.
	pluginResp := &provider.ChatResponse{Model: model, FinishReason: "stop"}
	s.applyPostResponsePlugins(ctx, &bookReq, pluginResp)

	// The response names the surface it came from, so a client (or
	// a log reader) never has to guess whether a 200 was chat,
	// embeddings or media.
	w.Header().Set("X-Codvian-Endpoint", string(spec.endpoint))
	// Decision transparency (CHECKLIST 199): the single attempt
	// that served this call.
	s.setDecisionHeader(w, model, model)
	// Telemetry headers (M2.1, choke point): cost/tokens from the
	// real usage block — a media response carries none, so the
	// headers stay absent, never 0.
	meta := s.responseMeta(ctx, bookReq, pluginResp, start)
	s.writeResponseHeaders(w, ctx, meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	s.recordEmbedMetrics(bookReq, model, nil, start)
	// The upstream body, verbatim: status + bytes exactly as the
	// provider returned them.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// Wiring (guard.go + the route table in server.go): the five
// routes are registered — guard-table rows guard.go:233-238
// and server.go:1404-1409, TierManagement, scope "chat",
// behind requireAuth, exactly like /v1/embeddings:
//
//	{Method: "POST", Path: "/v1/search", ...}              -> handleSearch
//	{Method: "POST", Path: "/v1/ocr", ...}                 -> handleOCR
//	{Method: "POST", Path: "/v1/videos/generations", ...}  -> handleVideoGenerations
//	{Method: "GET",  Path: "/v1/videos/{id}", ...}         -> handleVideoTask
//	{Method: "POST", Path: "/v1/music/generations", ...}   -> handleMusic
//
// The tier assignment landed as proposed (TierManagement +
// scope "chat", the embeddings precedent); the routes are
// live, so ServeHTTP serves them — the pre-wiring 404 state
// in the earlier note no longer applies.
