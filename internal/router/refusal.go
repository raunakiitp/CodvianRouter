// Content-policy refusal chain (CHECKLIST 161-162; competitor
// sweep gap — LiteLLM + Portkey: guardrail denials leak into
// fallback chains and burn retries).
//
// An upstream content-policy / safety refusal is a verdict about
// the *content*, not about the target: the target is healthy, the
// request shape is fine, only this content is refused here. So a
// refusal is routed on a SEPARATE chain from the failure chain:
//
//   - the refusing target is never retried for this request (its
//     verdict is already given; re-asking burns a call);
//   - the refusal touches no health signal: no circuit-breaker
//     count, no cooldown, no per-provider success-rate record
//     (a refusal says nothing about the provider being down);
//   - the next target comes from the refusal chain: the
//     operator-configured per-model chain (Config.RefusalChainFor)
//     when one is set, otherwise the failure chain's remaining
//     targets, in order, each once;
//   - when the refusal chain runs dry the request ends in an
//     honest refusal — ErrContentRefused, which the server answers
//     with HTTP 403 and the policy-refusal code -32001 (reason
//     "upstream_content_refusal") — never a fake success and
//     never a 502.
//
// A refusal is recognised by the error taxonomy
// (provider.ClassifyError: Classification.ContentRefusal) and by
// a "content_filter" finish on a response or an empty stream.
// Refusals that arrive after the first content byte was already
// delivered cannot switch targets (no switch after commit); they
// end the stream with the refusal error instead.
package router

import (
	"errors"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// ErrContentRefused is returned when every target in the refusal
// chain refused the content on policy/safety grounds. It wraps the
// last refusal error. The server maps it (and any bare content
// refusal error) to HTTP 403 with the policy-refusal code -32001
// and reason "upstream_content_refusal" — never a 502, never a
// fake success.
var ErrContentRefused = errors.New("router: content refused by every target in the refusal chain")

// RefusalChainStats is the engine's live record of refusal-chain
// outcomes, read via Engine.RefusalChainStats and served on the
// dashboard's guardrail feed. All counts are since this process
// started; zero before any refusal is the honest empty state.
// Refusals counts every upstream content refusal seen (one per
// refusing attempt); ServedViaRefusal counts requests that were
// served by a refusal-chain hop after an earlier target refused;
// Exhausted counts requests that ended in an honest refusal
// because the refusal chain ran dry. Last* names the most recent
// refusing attempt (empty before any refusal).
type RefusalChainStats struct {
	Refusals         uint64    `json:"refusals"`
	ServedViaRefusal uint64    `json:"served_via_refusal"`
	Exhausted        uint64    `json:"exhausted"`
	LastProvider     string    `json:"last_provider,omitempty"`
	LastModel        string    `json:"last_model,omitempty"`
	LastAt           time.Time `json:"last_at,omitempty"`
}

// RefusalMetrics is optionally implemented by a metrics sink that
// wants refusal-chain counters (metrics.Recorder satisfies it).
// Wiring is best-effort: a nil sink disables reporting, never
// routing.
type RefusalMetrics interface {
	IncContentRefusal()
	IncRefusalChainServed()
	IncRefusalChainExhausted()
}

// SetRefusalMetrics wires the metrics sink that counts content
// refusals and refusal-chain outcomes (e.g. *metrics.Recorder). A
// nil value disables reporting. Safe to call after NewEngine.
func (e *Engine) SetRefusalMetrics(m RefusalMetrics) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refusalMetrics = m
}

// RefusalChainStats returns a snapshot of the engine's live
// refusal-chain outcomes (see RefusalChainStats).
func (e *Engine) RefusalChainStats() RefusalChainStats {
	e.refMu.Lock()
	defer e.refMu.Unlock()
	return RefusalChainStats{
		Refusals:         e.refRefusals,
		ServedViaRefusal: e.refServed,
		Exhausted:        e.refExhausted,
		LastProvider:     e.refLastProvider,
		LastModel:        e.refLastModel,
		LastAt:           e.refLastAt,
	}
}

// noteRefusalSeen counts one upstream content refusal, records
// which target refused it last, and reports it to the metrics
// sink when one is wired. It is the only counter a refusal
// touches: no attempt stats, no breaker, no cooldown.
func (e *Engine) noteRefusalSeen(providerID, model string) {
	e.refMu.Lock()
	e.refRefusals++
	e.refLastProvider = providerID
	e.refLastModel = model
	e.refLastAt = time.Now()
	e.refMu.Unlock()
	e.mu.Lock()
	m := e.refusalMetrics
	e.mu.Unlock()
	if m != nil {
		m.IncContentRefusal()
	}
}

// noteRefusalServed counts one request served by a refusal-chain
// hop (a target after the first refusal served it).
func (e *Engine) noteRefusalServed() {
	e.refMu.Lock()
	e.refServed++
	e.refMu.Unlock()
	e.mu.Lock()
	m := e.refusalMetrics
	e.mu.Unlock()
	if m != nil {
		m.IncRefusalChainServed()
	}
}

// noteRefusalExhausted counts one request that ended in an honest
// refusal because the refusal chain ran dry.
func (e *Engine) noteRefusalExhausted() {
	e.refMu.Lock()
	e.refExhausted++
	e.refMu.Unlock()
	e.mu.Lock()
	m := e.refusalMetrics
	e.mu.Unlock()
	if m != nil {
		m.IncRefusalChainExhausted()
	}
}

// isContentRefusalErr reports whether err is an upstream
// content-policy / safety refusal: the taxonomy flags it
// (Classification.ContentRefusal — a 400/403/422 whose body
// signals a content refusal, a typed ContentRefusalError, or a
// mid-stream wire error with that signal), or it is an empty
// stream that ended on a "content_filter" finish. Any other
// error — including a plain 400/403 with no content signal —
// is not a refusal.
func isContentRefusalErr(err error) bool {
	if err == nil {
		return false
	}
	if provider.ClassifyError(err, provider.EndpointChatCompletions).ContentRefusal {
		return true
	}
	var ese *provider.EmptyStreamError
	if errors.As(err, &ese) && ese.FinishReason == "content_filter" {
		return true
	}
	return false
}

// isContentFilterFinish reports whether a completed response is a
// content refusal delivered as a finish reason instead of an
// error: the provider stopped generation on its content filter
// (Gemini blocked prompts, OpenAI content_filter finishes).
// Delivering such a reply as a success would be a fake partial
// success — the provider refused to complete — so the engine
// treats it as a refusal attempt and routes on.
func isContentFilterFinish(resp *provider.ChatResponse) bool {
	return resp != nil && resp.FinishReason == "content_filter"
}

// contentRefusalError builds the refusal error for a
// content_filter finish: a typed ContentRefusalError, so the
// taxonomy flags it without guessing from a message string.
func contentRefusalError(att Attempt) error {
	return &provider.ContentRefusalError{Message: "finish_reason content_filter from " + att.ProviderID}
}

// noteRefusal records one refusing attempt: the trace outcome is
// "content_refused" (with the taxonomy reason "content_refused"),
// the ledger row is a failed attempt with unknown usage (the
// attempt happened; nothing was served), and the refusal is
// counted in the refusal-chain stats. Deliberately absent: any
// attempt-stat record, any breaker touch, any cooldown — a
// refusal is not a health failure. A half-open probe claim is
// released without counting success or failure (the provider
// answered, so the probe is over; the verdict says nothing
// about its health either way).
func (e *Engine) noteRefusal(trace *routetrace.RouteTrace, res attemptResult, keyID string) {
	e.noteRefusalPreread(trace, res, keyID, prereadInfo{})
}

// noteRefusalPreread is noteRefusal plus the preread-buffer record
// for a streaming attempt (pr.state "", 0, 0 when the buffer did
// not run or held nothing).
func (e *Engine) noteRefusalPreread(trace *routetrace.RouteTrace, res attemptResult, keyID string, pr prereadInfo) {
	addTraceAttemptPrereadReasoning(trace, res.att, res.latencyMs, "content_refused", res.err, pr, res.reasoning)
	if e.ledger != nil && res.called {
		// A refusal the upstream billed for is recorded with
		// the usage its response reported (failedUsage), not
		// as a zero-token row — the prompt tokens it charged
		// for, and its exact cost when it reported one.
		e.recordAttempt(keyID, res.att, false, res.latencyMs, failedUsage(res))
	}
	e.breakerNeutral(res.att.ProviderID)
	e.noteRefusalSeen(res.att.ProviderID, res.att.Model)
}

// refusalChainFor builds the refusal chain for one request once
// the first content refusal has been seen. tried is the set of
// provider IDs already attempted in this request: they are never
// in the chain again. ordered/from is the failure chain and the
// position after the refusing attempt, for the default.
//
// When the operator configured a per-model refusal chain
// (Config.RefusalChainFor returns non-nil for this model) it is
// used verbatim, minus tried targets, targets with no live client
// and zero-quota targets — the operator names the targets known
// to accept that content class. A configured empty chain means
// "no refusal routing for this model": the result is empty and
// the request ends in an honest refusal at once.
//
// Otherwise the default applies: the failure chain's remaining
// attempts from position `from`, in order, each once — also minus
// tried targets (the hedge may already have tried one ahead),
// targets with no live client and zero-quota targets.
func (e *Engine) refusalChainFor(model string, ordered []Attempt, from int, tried map[string]bool) []Attempt {
	var configured []Attempt
	if e.cfg.RefusalChainFor != nil {
		configured = e.cfg.RefusalChainFor(model)
	}
	var raw []Attempt
	if configured != nil {
		raw = configured
	} else {
		if from < 0 {
			from = 0
		}
		if from > len(ordered) {
			from = len(ordered)
		}
		raw = ordered[from:]
	}
	var out []Attempt
	for _, att := range raw {
		if tried[att.ProviderID] {
			continue
		}
		if !e.hasClient(att.ProviderID) {
			continue
		}
		if e.quotaHint(att.ProviderID) <= 0 {
			continue
		}
		out = append(out, att)
	}
	return out
}
