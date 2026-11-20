// Streaming failover via a bounded pre-commit preread buffer
// (research-log entries 97 + 111; CHECKLIST 133-134).
//
// Entry 97 (CLIProxyAPI): fail BEFORE the 200 commit — a stream
// that ends with zero output must fail before the first byte reaches
// the client, because once 200 is committed the client owns the
// failure. Entry 111 (GPT-Load): hold the leading events of a
// stream up to 8 events / 256 KiB / 5 s; if the stream turns out
// empty (or errors) before real content, the gateway can still
// switch candidates. "Once a streaming response is committed to
// the client, candidates cannot be switched."
//
// The rule this file implements, in one sentence: nothing leaves
// the router until the first real content exists, and after that
// first content byte no candidate switch ever happens.
//
// Bounds (GPT-Load's, verbatim):
//   - at most 8 leading events held,
//   - at most 256 KiB of buffered text,
//   - at most 5 s waiting for the first content.
//
// Whichever bound is hit first commits the stream: the buffered
// events are flushed to the client and the rest forwards live.
// The 256 KiB bound is enforced on the buffered bytes, but with
// today's event shapes it cannot trip on its own: a text delta or
// a tool-call delta IS real content and commits immediately, so the
// only events ever buffered are content-free (empty, usage-only,
// finish-only) and carry ~0 bytes. It is a safety bound for future
// event shapes, not a tripwire this code can demonstrate — the
// event-count and time bounds are the ones with live tests.
//
// "Real content" is a non-empty text delta or a non-empty tool-call
// delta. A finish-reason-only chunk is NOT content: a stream that
// sends only `finish_reason: stop` (or only [DONE]) is an empty
// completion and fails over, never a success with an empty body
// (entry 97: "fail over on empty completions; never fake one").
//
// 200-OK body validation (PARITY.md Area 2 row "200-OK body
// validation predicate → failover"; BURNDOWN Tier 1): the same
// predicate the non-streaming path applies in callChat
// (ValidateResponseBody, response_validation.go) is applied here
// to the text about to be committed — BEFORE the first byte reaches
// the client. A stream whose leading content already fails the
// predicate fails over pre-commit, exactly like a failed attempt:
// the commit point is the last moment a switch is legal.
//
// Only the prefix-sound subset of the predicate runs here. A commit
// sees a PREFIX of the response (the buffered leading events plus
// the committing event's delta), so a check may only fail the text
// when the failure is already certain from the prefix:
//   - top-level JSON error payload / empty choices array — certain
//     only when the prefix parses as a COMPLETE JSON object
//     carrying the member (invalidJSONBodyReason, same package);
//     a prefix that does not parse yet passes, it cannot be judged;
//   - forbidden substrings — certain: a substring present in the
//     prefix is present in the full text.
//
// Required substrings and minimum content length are deliberately
// NOT checked pre-commit: a prefix can never prove something is
// missing or the whole will stay short, so failing on them would
// fail over on healthy streams. Tool-call-carrying events are
// exempt from the JSON check, as in the non-streaming predicate:
// the tool calls ARE content.
// A split error payload — one whose first delta is only a prefix
// of the JSON object — is NOT caught: the first delta does not
// parse yet, so the stream commits, and after commit no switch is
// possible. That is the honest limit of a pre-commit check; the
// common case (an error payload arriving as a single chunk) is
// caught. A failing commit returns streamFailed with the same
// *provider.UpstreamError{StatusCode: 0, Retryable: true} the
// non-streaming path uses, so the caller fails over at once
// (isStreamEarlyRetryable never re-opens an UpstreamError).
//
// Exemptions (entry 111's list, codified in ReplayExemption): some
// requests must never be replayed onto another target, because a
// replay would double-apply stateful upstream work. Those requests
// skip the preread buffer entirely AND skip hedging AND get exactly
// one attempt — the exemption is enforced in runStream and in
// ChatWithChainStrategy, never just documented.
//
// What is deliberately NOT exempt: plain multi-turn chat. Its
// messages are carried by the client, the upstream holds no session
// state for a chat completion, and a pre-commit replay (nothing
// delivered yet) cannot double-apply anything. Tool-using chat is
// likewise NOT exempt pre-commit: no tool call has been delivered
// to the client yet, so the client has executed nothing. (Entry 98's
// rule — never replay tool calls ALREADY DELIVERED — is exactly what
// the commit point enforces: after commit there is no replay at
// all.)
package router

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Preread bounds (research-log entry 111, GPT-Load's numbers).
const (
	// PrereadMaxEvents is the most leading events held before commit.
	PrereadMaxEvents = 8
	// PrereadMaxBytes is the most buffered text held before commit.
	PrereadMaxBytes = 256 << 10
)

// prereadMaxWait is the longest the buffer waits for first content
// before committing what it holds (5 s in production). A variable,
// not a const, so tests can shrink it without waiting 5 s; nothing
// else in the codebase changes it.
var prereadMaxWait = 5 * time.Second

// Replay-exemption reason slugs (stable; the dashboard renders them).
const (
	// ExemptEmbeddings is a /v1/embeddings request.
	ExemptEmbeddings = "embeddings"
	// ExemptImages is a /v1/images request.
	ExemptImages = "images"
	// ExemptRerank is a /v1/rerank request.
	ExemptRerank = "rerank"
	// ExemptSearch is a /v1/search request (M8.20).
	ExemptSearch = "search"
	// ExemptOCR is a /v1/ocr request (M8.20).
	ExemptOCR = "ocr"
	// ExemptVideo is a /v1/videos request (M8.20).
	ExemptVideo = "video"
	// ExemptMusic is a /v1/music request (M8.20).
	ExemptMusic = "music"
	// ExemptWarmUp is a synthetic warm-up request.
	ExemptWarmUp = "warm_up"
)

// ReplayExemption reports whether req must never be replayed onto
// another target — and why. "" means the request is replay-safe
// (plain chat completions, including tool-using ones, pre-commit,
// and ALL /v1/responses creates, fresh or branched: the gateway
// holds the conversation transcript itself and re-sends it as
// plain chat messages, so no upstream-held state exists for a
// replay to double-apply — CHECKLIST 237, which is why a branched
// create fails over mid-branch like a chat completion, replaying
// the same branched input against the next target). A non-empty
// slug means: skip the preread buffer, skip hedging, exactly one
// attempt, ever. The slug is the single codified exemption list
// for the whole router; no other place invents replay rules.
//
// (CHECKLIST 169 used to exempt Responses continuations here —
// the exemption was written when the design assumed upstream-held
// conversation state. The gateway never sends a Responses request
// upstream, so that state never existed; the exemption was removed
// by the 237-238 slice, and continuations now fail over.)
func ReplayExemption(req provider.ChatRequest) string {
	switch req.Endpoint {
	case provider.EndpointEmbeddings:
		return ExemptEmbeddings
	case provider.EndpointImages:
		return ExemptImages
	case provider.EndpointRerank:
		return ExemptRerank
	case provider.EndpointSearch:
		return ExemptSearch
	case provider.EndpointOCR:
		return ExemptOCR
	case provider.EndpointVideo:
		return ExemptVideo
	case provider.EndpointMusic:
		return ExemptMusic
	}
	if req.WarmUp {
		return ExemptWarmUp
	}
	return ""
}

// prereadInfo is what the preread buffer did for one attempt, for
// the route trace. state is "held" (buffered events, then committed
// on first content — 0 events when the first event was already
// content), "empty" (stream ended with no content before commit),
// "exempt:<slug>" (preread skipped), or "" (preread never committed:
// error/cancel before first content, or never ran).
type prereadInfo struct {
	state  string
	events int
	bytes  int
}

// isContentEvent reports whether ev is real content: a non-empty
// text delta or a non-empty tool-call delta. Finish-reason-only,
// usage-only and empty events are not content.
func isContentEvent(ev StreamEvent) bool {
	return ev.Delta != "" || len(ev.ToolCalls) > 0
}

// validatePrereadPrefix applies the prefix-sound subset of the
// 200-OK body validation predicate (response_validation.go) to text
// that is about to be committed to the client. Only checks whose
// failure is already certain from a prefix run here: a top-level
// JSON error payload / empty choices array, judged by
// invalidJSONBodyReason on the text as it stands (a prefix that
// does not parse as complete JSON passes — it cannot be judged
// yet), and the operator-configured forbidden substrings. Required
// substrings and minimum content length are NOT checked: a prefix
// can never prove them missing, so they are not prefix-sound.
// hasToolCalls exempts the JSON check only, exactly as in
// ValidateResponseBody — the tool calls ARE content. A nil error
// means the text may commit. A non-nil error is the same
// *provider.UpstreamError{StatusCode: 0, Retryable: true} the
// non-streaming path returns, so the caller fails over at once.
func validatePrereadPrefix(text string, hasToolCalls bool, cfg ResponseValidationConfig) error {
	if cfg.Disabled {
		return nil
	}
	if !hasToolCalls && text != "" {
		if reason, bad := invalidJSONBodyReason(text); bad {
			return &provider.UpstreamError{
				StatusCode: 0,
				Retryable:  true,
				Body:       "router: 200 response failed body validation: " + reason,
			}
		}
	}
	for _, sub := range cfg.ForbiddenSubstrings {
		if sub != "" && strings.Contains(text, sub) {
			return &provider.UpstreamError{
				StatusCode: 0,
				Retryable:  true,
				Body:       "router: 200 response failed body validation: response contains forbidden substring " + quoteForValidation(sub),
			}
		}
	}
	return nil
}

// forwardStreamPreread is forwardStream with the pre-commit preread
// buffer: leading events are held until the first real content
// arrives (or a bound is hit), and only then is anything sent to
// out. It returns streamEmpty when the stream ends with no content
// before commit (with a *provider.EmptyStreamError), streamFailed
// when it errors before content, streamDone when it completes, and
// streamCtxDone when ctx ends. After the buffer is flushed the rest
// of the stream forwards live via forwardStream — and a failure
// after that point is reported, never switched (the caller stops
// the chain once partial content exists; see runStream).
func (e *Engine) forwardStreamPreread(ctx context.Context, out chan<- StreamEvent, ch <-chan StreamEvent, first *StreamEvent, partial *strings.Builder, onCommit func()) (streamOutcome, error, prereadInfo) {
	var buf []StreamEvent
	bufBytes := 0
	finishReason := ""
	pr := prereadInfo{}

	// commit flushes the buffered events to the client in order,
	// then forwards the rest of ch live. firstEv is an already-read
	// event to emit first (nil when there is none). The preread
	// record always says "held" once the buffer commits — with 0
	// events when the first event was already content — so the
	// trace can tell "preread ran, committed immediately" apart
	// from "preread never ran" (state "").
	commit := func(firstEv *StreamEvent) (streamOutcome, error, prereadInfo) {
		// 200-OK body validation (see the file header): the text
		// about to reach the client is judged BEFORE commit —
		// afterwards no switch is possible. A failing prefix
		// fails over pre-commit, as streamFailed with the same
		// *provider.UpstreamError the non-streaming path uses.
		// onCommit is NOT fired: no provider served anything.
		var commitText strings.Builder
		commitToolCalls := false
		for _, ev := range buf {
			commitText.WriteString(ev.Delta)
			if len(ev.ToolCalls) > 0 {
				commitToolCalls = true
			}
		}
		if firstEv != nil {
			commitText.WriteString(firstEv.Delta)
			if len(firstEv.ToolCalls) > 0 {
				commitToolCalls = true
			}
		}
		if verr := validatePrereadPrefix(commitText.String(), commitToolCalls, e.cfg.ResponseValidation); verr != nil {
			return streamFailed, verr, pr
		}
		// The commit point (CHECKLIST 199): the first buffered
		// event is about to reach the client, so this attempt's
		// provider is the serving provider — publish the routing
		// decision before any byte is sent, so the server's
		// first-chunk read of LastRouting is ordered after it.
		// forwardStream gets nil: the decision is already
		// recorded, and firing twice would be a lie about two
		// commits.
		if onCommit != nil {
			onCommit()
		}
		for _, ev := range buf {
			if ev.Delta != "" {
				partial.WriteString(ev.Delta)
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return streamCtxDone, ctx.Err(), pr
			}
		}
		pr = prereadInfo{state: "held", events: len(buf), bytes: bufBytes}
		buf = nil
		oc, err := e.forwardStream(ctx, out, ch, firstEv, partial, nil)
		return oc, err, pr
	}

	// Seed with the hedge winner's already-consumed first event.
	if first != nil {
		if first.Err != nil {
			return streamFailed, first.Err, pr
		}
		if first.Done {
			pr = prereadInfo{state: "empty", events: len(buf), bytes: bufBytes}
			return streamEmpty, &provider.EmptyStreamError{FinishReason: first.FinishReason}, pr
		}
		if isContentEvent(*first) {
			return commit(first)
		}
		if first.FinishReason != "" {
			finishReason = first.FinishReason
		}
		buf = append(buf, *first)
		bufBytes += len(first.Delta)
	}

	timer := time.NewTimer(prereadMaxWait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return streamCtxDone, ctx.Err(), pr
		case <-timer.C:
			// Bound hit: commit what was held (possibly nothing)
			// and stream the rest live.
			return commit(nil)
		case ev, ok := <-ch:
			if !ok {
				// As in forwardStream: a close after ctx
				// ended is the cancellation unwinding, not
				// an upstream failure (CHECKLIST 153).
				if err := ctx.Err(); err != nil {
					return streamCtxDone, err, pr
				}
				return streamFailed, errors.New("router: upstream closed stream without Done"), pr
			}
			if ev.Err != nil {
				return streamFailed, ev.Err, pr
			}
			if ev.Done {
				pr = prereadInfo{state: "empty", events: len(buf), bytes: bufBytes}
				return streamEmpty, &provider.EmptyStreamError{FinishReason: finishReason}, pr
			}
			if ev.FinishReason != "" {
				finishReason = ev.FinishReason
			}
			if isContentEvent(ev) {
				return commit(&ev)
			}
			// Non-content leading event: hold it, unless a
			// bound is hit first — then commit and stream live.
			if len(buf) >= PrereadMaxEvents || bufBytes+len(ev.Delta) > PrereadMaxBytes {
				return commit(&ev)
			}
			buf = append(buf, ev)
			bufBytes += len(ev.Delta)
		}
	}
}
