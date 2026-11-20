package server

import (
	"fmt"
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/compress"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Lossless-first compression on the server path (CHECKLIST 183).
//
// applyCompression runs in handleChatCompletions after the
// guardrails (which must see the original prompt — a blocked
// request never reaches here) and before every budget gate, so
// the pre-send prompt-cost checks price the prompt as it will
// actually be sent. It stamps the result on the request; the
// engine keeps a stamp for the same model and re-decides only
// after an "auto/*" resolve (see router.maybeCompress), so the
// saving is measured once, against the original prompt.
//
// The per-model list comes from the config file's compression
// block (s.cfg.Compression); a nil config or a nil block means
// no model compresses by default. A counting failure never fails
// the request: compress.Apply returns the original messages with
// Applied=false, and the request proceeds uncompressed.

// prefixFreezeEngine is optionally implemented by engines that own
// the process-wide PrefixFreeze hooks (row :939): the router engine
// exports ObservePrefix / PrefixFreezeChecker / AnyCachingProvider
// for the server lane (see router.SetPrefixFreezeHooks, installed
// by main.go). An engine without them (a test fake, or an observer
// not yet installed) simply yields the zero-option decision — the
// same pass as plain compress.Apply.
type prefixFreezeEngine interface {
	ObservePrefix(msgs []provider.Message)
	PrefixFreezeChecker() func([]provider.Message) bool
	AnyCachingProvider() bool
}

// applyCompression compresses req in place (messages + stamp).
// The per-request opt-in rides on req.Compression; the operator's
// per-model list rides on the server config.
//
// Row :939: the decision runs through compress.ApplyWithOptions
// with the cache-aware inputs from the engine — IsCachingProvider
// from AnyCachingProvider, FrozenPrefix from PrefixFreezeChecker
// (nil until main.go installs the observer, when Apply treats it
// as absent) — and observes the system prompt once per request,
// only when this request carries no compression stamp yet, i.e.
// this call is the first compression point for it. The engine's
// maybeCompress uses the same rule: a stamp means an earlier
// compression point already observed it, and observing on every
// call would count one request 2-3 times and trip the freeze
// threshold (3) on the first request.
func (s *Server) applyCompression(req *provider.ChatRequest) {
	var losslessModels []string
	if s.cfg != nil && s.cfg.Compression != nil {
		losslessModels = s.cfg.Compression.LosslessModels
	}
	var opts compress.ApplyOptions
	if pf, ok := s.engine.(prefixFreezeEngine); ok {
		if req.Compressed == nil {
			pf.ObservePrefix(req.Messages)
		}
		opts = compress.ApplyOptions{
			IsCachingProvider: pf.AnyCachingProvider(),
			HasCacheControl:   false,
			FrozenPrefix:      pf.PrefixFreezeChecker(),
		}
	}
	// Row B4-25: the per-request RTK level rides on
	// req.Compression (body field rtk_level, or the
	// X-Codvian-RTK-Level header stashed there by
	// applyCompressionPrePass). An unparseable value means
	// standard — the HTTP layer 400s an unknown body value, and
	// an unknown header value is ignored by the pre-pass.
	if req.Compression != nil && req.Compression.RTKLevel != "" {
		if level, ok := compress.ParseRTKLevel(req.Compression.RTKLevel); ok && level != "" {
			opts.RTKLevel = level
		}
	}
	msgs, res := compress.ApplyWithOptions(req.Model, req.Messages, req.Compression, losslessModels, req.Compressed, opts)
	req.Messages = msgs
	req.Compressed = res
}

// compressionModeName is the X-Codvian-Compression value for a
// stamped result: "lossless", "lossy", or "lossless+lossy" (a
// lossy request always runs the lossless steps first, so a lossy
// application is both). "" when nothing applied — no header is
// set then, so an absent header is an honest "not compressed".
func compressionModeName(res *provider.CompressionResult) string {
	if res == nil || !res.Applied {
		return ""
	}
	if res.LossyApplied {
		return "lossless+lossy"
	}
	return "lossless"
}

// setCompressionHeader reports the applied compression on the
// response, so a lossy step is never silent: the mode, the local
// estimate of tokens saved, and an explicit lossy flag when a
// lossy step ran. The saved count is a local tiktoken estimate,
// never a provider-reported number. Called for non-streaming
// responses before the body is written, and at the top of the
// streaming handler before any SSE header can be sent.
func (s *Server) setCompressionHeader(w http.ResponseWriter, req *provider.ChatRequest) {
	mode := compressionModeName(req.Compressed)
	if mode == "" {
		return
	}
	w.Header().Set("X-Codvian-Compression", mode)
	w.Header().Set("X-Codvian-Compression-Saved-Tokens", fmt.Sprintf("%d", req.Compressed.SavedTokens))
	w.Header().Set("X-Codvian-Compression-Prompt-Tokens", fmt.Sprintf("%d", req.Compressed.AfterTokens))
	if req.Compressed.LossyApplied {
		w.Header().Set("X-Codvian-Compression-Lossy", "true")
	}
}

// compressionSummary is the Overview's compression block: window
// totals from the ledger summary (CHECKLIST 183). Requests counts
// successful attempts that carried a compression saving; Tokens
// Saved is the sum of their local-estimate savings. Both are
// honest zeros before any compressed request — the dashboard
// renders that as "no compressed requests yet", never an invented
// saving.
type compressionSummary struct {
	Requests    int `json:"requests_with_compression"`
	TokensSaved int `json:"tokens_saved"`
}
