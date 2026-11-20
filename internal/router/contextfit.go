// Context-window pre-call fit check + dedicated context-window
// fallback chain (CHECKLIST 159).
//
// Before dialling any provider, the engine counts the prompt
// locally (tiktoken BPE, offline — see internal/tokencount) and
// compares it against each candidate model's context window, minus
// a reserve for the response. An oversize prompt NEVER dials the
// too-small model: too-small attempts are removed from the chain
// before the first call (recorded in the route trace as
// "skipped_context_fit"), so no retry is ever burned on a model
// that cannot hold the prompt.
//
// The reserve: the completion the model must still be able to
// emit. It is the request's own max_tokens when the caller set one
// (the caller asked for that much room, so the check honours it),
// otherwise DefaultContextReserveTokens (4096). Required tokens =
// local prompt estimate + reserve. The local count is an estimate
// (see tokencount: encoding from the model name, no tool schemas
// or per-message framing), so the check is conservative in exactly
// one direction: it uses the same estimate for every candidate,
// and a model whose window is unknown is treated as fitting —
// an unknown window is not proof the model is too small, so it is
// tried, and the decision records it as unknown rather than
// inventing a window.
//
// When no model in the primary chain fits, the engine jumps
// straight to the dedicated context-window fallback chain
// (Config.ContextFallbackChain — operator-configured long-context
// models, e.g. a 1M-token model): the first fitting fallback
// model serves the request. When no configured model fits
// anywhere, the request fails honestly BEFORE any provider is
// dialled, with ErrContextTooLarge naming the required tokens
// against the largest available window — zero provider calls, no
// ledger row, no retry.
//
// The decision of the most recent request is kept for
// Engine.LastContextFit, so the dashboard can show the fit per
// model for the last request (or the honest failure) without
// re-counting. A counting failure never fails the request: the
// check is skipped, the full chain is tried, and the decision
// records Checked=false with the reason — the same rule as the
// pre-send budget gate (CHECKLIST 145).
package router

import (
	"fmt"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// DefaultContextReserveTokens is the response reserve used when
// the caller did not set max_tokens: 4096 tokens held back for the
// completion, so a prompt that exactly fills the window is still
// refused (the model would have nowhere to write its answer).
// A caller that sets max_tokens replaces this reserve with its own
// number — larger or smaller — because the caller named the room
// it wants.
const DefaultContextReserveTokens = 4096

// ContextFitEntry is one candidate model's fit verdict for a
// request: the (provider, model) pair, the window the registry
// reported for it (0 when unknown), and whether the prompt fits.
// Known=false means the registry has no window for this pair —
// the attempt was tried anyway (an unknown window is not proof
// the model is too small) and the dashboard must show "unknown",
// never a guessed number.
type ContextFitEntry struct {
	ProviderID    string `json:"provider"`
	Model         string `json:"model"`
	ContextWindow int    `json:"context_window"`
	Known         bool   `json:"window_known"`
	Fits          bool   `json:"fits"`
}

// ContextFitDecision is the fit-check record of the most recent
// request (or the most recent request that reached the check — a
// request refused earlier by guardrails or a budget gate never
// reaches it, and the previous decision stands). Read via
// Engine.LastContextFit; the returned value is a copy.
type ContextFitDecision struct {
	// Model is the requested (concrete, post auto-resolution)
	// model name.
	Model string `json:"model"`
	// PromptTokens is the local tiktoken estimate of the prompt
	// (0 when the count failed — see Checked).
	PromptTokens int `json:"prompt_tokens"`
	// Encoding is the tiktoken encoding used for the count;
	// EncodingFallback is true when the model matched no known
	// encoding and cl100k_base was assumed.
	Encoding         string `json:"encoding,omitempty"`
	EncodingFallback bool   `json:"encoding_fallback,omitempty"`
	// ReserveTokens is the response reserve actually applied
	// (max_tokens when set, else the configured/default reserve).
	ReserveTokens int `json:"reserve_tokens"`
	// RequiredTokens is PromptTokens + ReserveTokens: the room
	// the request needs inside one context window.
	RequiredTokens int `json:"required_tokens"`
	// Checked is false when the local count failed and the check
	// was skipped (the full chain was tried); CheckSkippedReason
	// names why. A skipped check never fails the request.
	Checked            bool   `json:"checked"`
	CheckSkippedReason string `json:"check_skipped_reason,omitempty"`
	// ChainUsed is "primary" (a model of the requested chain
	// served it), "context_fallback" (the dedicated long-context
	// chain served it), or "none" (no configured model fits).
	ChainUsed string `json:"chain_used"`
	// LargestWindow is the largest KNOWN window among all
	// evaluated attempts (primary + fallback), 0 when no
	// evaluated window was known.
	LargestWindow int `json:"largest_window"`
	// Entries covers every evaluated attempt, primary first
	// then fallback, in chain order.
	Entries []ContextFitEntry `json:"entries"`
	// Error is the no-fit message ("" on success or on a
	// skipped check).
	Error string `json:"error,omitempty"`
	// At is when the check ran.
	At time.Time `json:"at"`
}

// ErrContextTooLarge is the honest pre-call failure when no
// configured model fits the prompt: the required tokens against
// the largest available window. The server maps it to a 400 with
// this message (a property of the request + the operator's
// configuration, not an upstream failure): no provider was
// called, nothing was billed, nothing was retried.
type ErrContextTooLarge struct {
	Model         string
	PromptTokens  int
	ReserveTokens int
	Required      int
	LargestWindow int
}

func (e *ErrContextTooLarge) Error() string {
	if e.LargestWindow > 0 {
		return fmt.Sprintf("router: prompt needs ~%d tokens (%d prompt tokens, local estimate, + %d reserved for the response) but the largest available context window is %d tokens for model %q — shorten the prompt or add a longer-context model; no provider was called",
			e.Required, e.PromptTokens, e.ReserveTokens, e.LargestWindow, e.Model)
	}
	return fmt.Sprintf("router: prompt needs ~%d tokens (%d prompt tokens, local estimate, + %d reserved for the response) for model %q but no configured model reports a context window — add a model with a known context window, or a longer-context fallback model; no provider was called",
		e.Required, e.PromptTokens, e.ReserveTokens, e.Model)
}

// contextReserve returns the response reserve for one request:
// the caller's max_tokens when set, else the configured reserve
// (Config.ContextReserveTokens), else the default.
func (e *Engine) contextReserve(req provider.ChatRequest) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	if e.cfg.ContextReserveTokens > 0 {
		return e.cfg.ContextReserveTokens
	}
	return DefaultContextReserveTokens
}

// contextWindowOf resolves one attempt's context window: the
// Config.ContextWindow hook when set, else unknown. A nil hook
// means every window is unknown — the check then passes every
// attempt (unknown is not proof of too-small) and the decision
// records that, so behaviour without a registry is unchanged.
func (e *Engine) contextWindowOf(att Attempt) (window int, known bool) {
	if e.cfg.ContextWindow == nil {
		return 0, false
	}
	return e.cfg.ContextWindow(att.ProviderID, att.Model)
}

// evaluateContextFit evaluates one chain against the required
// tokens and returns the fitting attempts, the per-attempt
// entries, and the largest known window in this chain.
func (e *Engine) evaluateContextFit(chain []Attempt, required int) (fits []Attempt, entries []ContextFitEntry, largest int) {
	for _, att := range chain {
		window, known := e.contextWindowOf(att)
		if known && window > largest {
			largest = window
		}
		entry := ContextFitEntry{
			ProviderID:    att.ProviderID,
			Model:         att.Model,
			ContextWindow: window,
			Known:         known,
		}
		// Unknown window fits (see the package doc): only a
		// KNOWN window smaller than the requirement refuses.
		entry.Fits = !known || required <= window
		entries = append(entries, entry)
		if entry.Fits {
			fits = append(fits, att)
		}
	}
	return fits, entries, largest
}

// applyContextFit is the pre-call fit gate, run once per request
// (non-streaming in ChatWithChainStrategy, streaming in
// runStream) after the chain is ordered and before the first
// dial. It returns the attempts the engine may actually try —
// the fitting subset of the primary chain, or the fitting subset
// of the dedicated context-window fallback chain when nothing in
// the primary chain fits — and records the decision for
// LastContextFit. Too-small attempts are recorded in the trace
// as "skipped_context_fit" (never dialled, never retried) and
// removed from the returned chain, so the loop that follows can
// never burn a retry on them. A nil error with an empty returned
// chain never happens: the only empty return is the no-fit
// failure, which returns ErrContextTooLarge.
func (e *Engine) applyContextFit(trace *routetrace.RouteTrace, chain []Attempt, req provider.ChatRequest) ([]Attempt, error) {
	dec := ContextFitDecision{Model: req.Model, At: time.Now(), ChainUsed: "primary"}
	// CountPromptFull (tools + reference framing): the tools
	// consume context too, so a message-text-only count would
	// admit attempts that then fail upstream with a
	// context-length error (CHECKLIST 229-230).
	count, err := tokencount.CountPromptFull(req.Model, req)
	if err != nil {
		// Counting failure never fails the request (same rule
		// as the pre-send budget gate): skip the check, try
		// the full chain, and say so in the decision.
		e.tokMu.Lock()
		e.tokCountErrs++
		e.tokMu.Unlock()
		dec.Checked = false
		dec.CheckSkippedReason = "local token count failed — fit check skipped"
		e.setLastContextFit(dec)
		return chain, nil
	}
	dec.Checked = true
	dec.PromptTokens = count.Tokens
	dec.Encoding = count.Encoding
	dec.EncodingFallback = count.Fallback
	dec.ReserveTokens = e.contextReserve(req)
	dec.RequiredTokens = count.Tokens + dec.ReserveTokens

	fits, entries, largest := e.evaluateContextFit(chain, dec.RequiredTokens)
	dec.Entries = entries
	dec.LargestWindow = largest
	if len(fits) > 0 {
		for _, en := range entries {
			if !en.Fits {
				trace.Add(en.ProviderID, en.Model, 0, "skipped_context_fit", nil)
			}
		}
		e.setLastContextFit(dec)
		return fits, nil
	}
	// Nothing in the primary chain fits: jump straight to the
	// dedicated context-window fallback chain (never retry the
	// too-small models — they are already out of the chain).
	if e.cfg.ContextFallbackChain != nil {
		fbChain := e.cfg.ContextFallbackChain(req.Model)
		fbFits, fbEntries, fbLargest := e.evaluateContextFit(fbChain, dec.RequiredTokens)
		dec.Entries = append(dec.Entries, fbEntries...)
		if fbLargest > dec.LargestWindow {
			dec.LargestWindow = fbLargest
		}
		if len(fbFits) > 0 {
			for _, en := range dec.Entries {
				if !en.Fits {
					trace.Add(en.ProviderID, en.Model, 0, "skipped_context_fit", nil)
				}
			}
			dec.ChainUsed = "context_fallback"
			e.setLastContextFit(dec)
			return fbFits, nil
		}
	}
	// No configured model fits anywhere: fail honestly pre-call.
	for _, en := range dec.Entries {
		if !en.Fits {
			trace.Add(en.ProviderID, en.Model, 0, "skipped_context_fit", nil)
		}
	}
	dec.ChainUsed = "none"
	noFit := &ErrContextTooLarge{
		Model:         req.Model,
		PromptTokens:  count.Tokens,
		ReserveTokens: dec.ReserveTokens,
		Required:      dec.RequiredTokens,
		LargestWindow: dec.LargestWindow,
	}
	dec.Error = noFit.Error()
	e.setLastContextFit(dec)
	return nil, noFit
}

// LastContextFit returns the fit-check decision of the most
// recent request that reached the check, and whether there is
// one. It is false before any request reached the check. The
// returned decision is a copy; mutating it does not affect the
// engine.
func (e *Engine) LastContextFit() (ContextFitDecision, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastContextFit == nil {
		return ContextFitDecision{}, false
	}
	dec := *e.lastContextFit
	dec.Entries = append([]ContextFitEntry(nil), e.lastContextFit.Entries...)
	return dec, true
}

func (e *Engine) setLastContextFit(dec ContextFitDecision) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := dec
	cp.Entries = append([]ContextFitEntry(nil), dec.Entries...)
	e.lastContextFit = &cp
}
