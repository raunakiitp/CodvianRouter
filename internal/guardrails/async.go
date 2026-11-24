// Async guardrail mode (CHECKLIST 185-186, competitor-sweep P1 gap;
// Some gateways run guardrails async so a slow guardrail never blocks
// the first token).
//
// The built-in guardrails (PII / secrets / injection, the local
// regex scan) are synchronous by construction: the scan takes
// microseconds, and a secret must never reach the provider — so
// those kinds can NEVER run async. Async mode is for SLOW external
// guardrails, e.g. a remote toxicity / policy classifier the
// operator points the router at: the check runs concurrently with
// the provider call instead of blocking it.
//
// Semantics, by surface:
//
//   - Streaming chat: the response streams immediately. The async
//     check runs in parallel; if its verdict later says BLOCK, the
//     stream is cut — the engine context is cancelled with an
//     AsyncBlockError, the client gets the terminal refusal frame
//     (the same 403 / -32001 policy-refusal shape as the sync
//     path, as an SSE error event + [DONE], or a plain 403 JSON
//     when nothing was sent yet), and the ledger records the
//     attempt as failed with the local prompt estimate only
//     (streamFallbackUsage) — billed for what was actually served
//     before the cut, never for the full response that never
//     arrived, and never with an invented completion split.
//   - Non-streaming chat: the check runs concurrently with the
//     provider call; a BLOCK that lands before the response is
//     written withholds it with 403 / -32001 (like the response
//     guardrail); a BLOCK that lands after delivery is recorded
//     as blocked_after_delivery — too late to act, and said so.
//   - Sync mode for an external guardrail is the opt-in
//     alternative: the check runs before any provider is dialled
//     and a BLOCK refuses the request outright, exactly like the
//     built-in blocking toggles. A slow classifier in sync mode
//     delays the request by its own timeout — the operator's
//     explicit choice.
//
// A checker error (endpoint down, timeout, malformed verdict)
// fails OPEN: the request continues, and the error is recorded
// honestly (outcome "error" in the store + dashboard) instead of
// turning a classifier outage into a gateway outage. The verdict
// itself is never faked: an unknown verdict string is an error,
// not an allow.
//
// This package stays a leaf (stdlib only): the HTTP classifier is
// a plain net/http client, and the run/store machinery has no
// internal imports, so the server, the router (which reads the
// cancel cause) and the config can all use it without a cycle.
package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// containsName reports whether name is in the allowed list
// (CHECKLIST 277 (d), per-key guardrail scoping).
func containsName(allowed []string, name string) bool {
	for _, n := range allowed {
		if n == name {
			return true
		}
	}
	return false
}

// Guardrail modes: how one guardrail runs. Sync is the default —
// the check completes before the request proceeds. Async runs
// concurrently with the provider call and can only cut a stream
// (or withhold a not-yet-delivered response) when its verdict
// lands.
type Mode string

const (
	// ModeSync runs the check before any provider is dialled; a
	// BLOCK refuses the request outright.
	ModeSync Mode = "sync"
	// ModeAsync runs the check concurrently with the provider
	// call; a BLOCK cuts the stream (or withholds a response
	// that has not been delivered yet).
	ModeAsync Mode = "async"
)

// ExternalConfig is one operator-configured external guardrail: a
// named check backed by a remote classifier endpoint. Kind is the
// label the verdict is recorded under (e.g. "toxicity",
// "classifier"); it must NOT be one of the blocking kinds
// (pii/secret/injection) when Mode is async — see Validate.
// TimeoutMs caps one check (0 = DefaultClassifierTimeout);
// negative is rejected by Validate.
type ExternalConfig struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Mode      Mode   `json:"mode"`
	Endpoint  string `json:"endpoint"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

// DefaultClassifierTimeout caps one external classifier check when
// the entry sets no timeout. A classifier slower than this is
// recorded as an error, never waited on forever.
const DefaultClassifierTimeout = 5 * time.Second

// Validate rejects an external-guardrail config that cannot run
// honestly. The one rule that matters: async + a blocking kind is
// an error, not a warning — PII, secrets and prompt injection
// must stay synchronous, because a secret that reaches the
// provider while the verdict is still in flight cannot be
// un-sent. Names must be unique (they key the dashboard counts
// and the checker registry); the mode must be named; the endpoint
// must be an http(s) URL.
func (c Config) Validate() error {
	seen := map[string]bool{}
	for i, e := range c.External {
		where := fmt.Sprintf("guardrails: external[%d]", i)
		if e.Name == "" {
			return fmt.Errorf("%s: name must not be empty", where)
		}
		if seen[e.Name] {
			return fmt.Errorf("guardrails: external guardrail %q is defined twice", e.Name)
		}
		seen[e.Name] = true
		if e.Kind == "" {
			return fmt.Errorf("guardrails: external guardrail %q: kind must not be empty", e.Name)
		}
		if e.Mode != ModeSync && e.Mode != ModeAsync {
			return fmt.Errorf("guardrails: external guardrail %q: mode must be %q or %q (got %q)", e.Name, ModeSync, ModeAsync, e.Mode)
		}
		if (e.Kind == KindPII || e.Kind == KindSecret || e.Kind == KindInjection) && e.Mode == ModeAsync {
			return fmt.Errorf("guardrails: external guardrail %q: kind %q is a blocking type and must stay synchronous — async mode would let a secret or PII reach the provider before the verdict lands", e.Name, e.Kind)
		}
		if e.Endpoint == "" {
			return fmt.Errorf("guardrails: external guardrail %q: endpoint must not be empty", e.Name)
		}
		u, err := url.Parse(e.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("guardrails: external guardrail %q: endpoint %q is not an http(s) URL", e.Name, e.Endpoint)
		}
		if e.TimeoutMs < 0 {
			return fmt.Errorf("guardrails: external guardrail %q: timeout_ms must not be negative (got %d)", e.Name, e.TimeoutMs)
		}
	}
	// Per-type toggles (G18 Area 7): every disabled type must be a
	// known built-in pattern label (see ListTypes), named once.
	// A typo'd label fails loudly at load, naming the label,
	// instead of silently leaving the operator believing a type
	// is off while it still runs.
	seenTypes := map[string]bool{}
	for _, name := range c.DisabledTypes {
		if name == "" {
			return fmt.Errorf("guardrails: disabled_types must not contain an empty type name")
		}
		if seenTypes[name] {
			return fmt.Errorf("guardrails: disabled type %q is listed twice", name)
		}
		seenTypes[name] = true
		if !knownBuiltinType(name) {
			return fmt.Errorf("guardrails: unknown PII type %q in disabled_types (see ListTypes)", name)
		}
	}
	return nil
}

// classifierTimeout is the per-check timeout for one external
// entry: its own TimeoutMs, or the default when unset.
func classifierTimeout(e ExternalConfig) time.Duration {
	if e.TimeoutMs > 0 {
		return time.Duration(e.TimeoutMs) * time.Millisecond
	}
	return DefaultClassifierTimeout
}

// ---------------------------------------------------------------------------
// Checkers: the slow external check itself.
// ---------------------------------------------------------------------------

// AsyncVerdict is one external classifier's answer: allow the
// request, or block it. Reason names the kind when blocked (the
// entry's Kind when the classifier sends none).
type AsyncVerdict struct {
	Blocked bool
	Reason  string
}

// AsyncChecker is a slow guardrail check: given the request text,
// answer allow or block. Implementations must honour ctx (the run
// cancels slow checks at their timeout) and must never return
// prompt text in an error.
type AsyncChecker interface {
	Check(ctx context.Context, text string) (AsyncVerdict, error)
}

// ErrNoChecker is the error when an external guardrail is
// configured but no checker was registered for its name (a
// startup wiring gap, not a verdict): the check fails open and
// the error is recorded, so the gap is visible instead of silent.
var ErrNoChecker = errors.New("guardrails: no checker registered for external guardrail")

// HTTPClassifier is an AsyncChecker backed by an external HTTP
// classifier service (the "slow guardrail" of the async mode).
// Protocol (documented in docs/async-guardrails.md — there is no
// standard classifier protocol, so this one is deliberately
// minimal): POST {"text": "<request text>"} to the endpoint;
// expect HTTP 200 with {"verdict": "allow"|"block", "reason":
// "<kind label, optional>"}. Any other status, a malformed body
// or an unknown verdict string is a checker error (fail open,
// recorded) — never silently treated as allow, and never as a
// block either. The endpoint is operator-configured; no auth
// header is sent (put the classifier behind the operator's own
// proxy when it needs one).
type HTTPClassifier struct {
	Endpoint string
	Timeout  time.Duration
	client   *http.Client
}

// NewHTTPClassifier returns a classifier for endpoint, timing out
// each check after timeout (DefaultClassifierTimeout when <= 0).
func NewHTTPClassifier(endpoint string, timeout time.Duration) *HTTPClassifier {
	if timeout <= 0 {
		timeout = DefaultClassifierTimeout
	}
	return &HTTPClassifier{Endpoint: endpoint, Timeout: timeout, client: &http.Client{}}
}

// Check posts the text to the classifier and parses its verdict.
// The response body is capped (a classifier that echoes the text
// back must not make the gateway hold it) and an error body is
// never included in the returned error — only the status.
func (c *HTTPClassifier) Check(ctx context.Context, text string) (AsyncVerdict, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return AsyncVerdict{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return AsyncVerdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return AsyncVerdict{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return AsyncVerdict{}, fmt.Errorf("classifier: unexpected status %d", resp.StatusCode)
	}
	var out struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return AsyncVerdict{}, fmt.Errorf("classifier: malformed verdict: %w", err)
	}
	switch out.Verdict {
	case "allow":
		return AsyncVerdict{Blocked: false}, nil
	case "block":
		return AsyncVerdict{Blocked: true, Reason: out.Reason}, nil
	default:
		return AsyncVerdict{}, fmt.Errorf("classifier: unknown verdict %q", out.Verdict)
	}
}

// ---------------------------------------------------------------------------
// The cancel cause: how the server tells the engine a stream was
// cut by an async BLOCK verdict.
// ---------------------------------------------------------------------------

// AsyncBlockReason is the trace/dashboard reason slug for an
// attempt cut by an async guardrail BLOCK verdict. It is the same
// string as the trace outcome (AsyncBlockOutcome): a policy cut
// is not a provider failure, so it must never read as one.
const AsyncBlockReason = "async_guardrail_blocked"

// AsyncBlockOutcome is the route-trace outcome for an attempt cut
// by an async guardrail BLOCK verdict.
const AsyncBlockOutcome = "async_guardrail_blocked"

// AsyncBlockError is the context-cancel cause the server uses to
// cut a stream when an async guardrail verdict says BLOCK, and
// the error the engine returns for that attempt. Guardrail names
// the entry that blocked; Kind is the verdict's kind label — the
// same label the terminal refusal frame carries as its reason, so
// the trace, the ledger row and the client's frame all agree.
type AsyncBlockError struct {
	Guardrail string
	Kind      string
}

// Error returns a fixed string carrying only the kind label —
// never the request or response text.
func (e *AsyncBlockError) Error() string {
	return "async guardrail blocked stream: " + e.Kind
}

// ---------------------------------------------------------------------------
// Async outcomes: what a verdict did, from the response's point of
// view. The outcome is a TIMING label, not a claim about which
// guardrail caused a refusal: "blocked_before_delivery" means the
// BLOCK verdict arrived before the response was delivered, whether
// or not another guardrail also refused it.
// ---------------------------------------------------------------------------

const (
	// AsyncOutcomeAllowed: the verdict was allow.
	AsyncOutcomeAllowed = "allowed"
	// AsyncOutcomeBlockedAfterStart: a BLOCK verdict cut a stream
	// that was already in flight (the provider call had started).
	AsyncOutcomeBlockedAfterStart = "blocked_after_start"
	// AsyncOutcomeBlockedBeforeDelivery: a BLOCK verdict arrived
	// before a non-streaming response was delivered, so the
	// response was withheld.
	AsyncOutcomeBlockedBeforeDelivery = "blocked_before_delivery"
	// AsyncOutcomeBlockedAfterDelivery: a BLOCK verdict arrived
	// after the response was already delivered (or the request
	// had already ended) — too late to act, recorded only.
	AsyncOutcomeBlockedAfterDelivery = "blocked_after_delivery"
	// AsyncOutcomeError: the checker errored or timed out — no
	// verdict, the request continued, the error is recorded.
	AsyncOutcomeError = "error"
)

// AsyncResult is one async guardrail's verdict as the run recorded
// it. Delivered reports whether the response had started delivery
// when the verdict landed (first SSE byte sent, or the
// non-streaming response written). Surface names the request path
// the verdict ran on ("chat" / "messages"); "" when unrecorded.
type AsyncResult struct {
	Name      string
	Kind      string
	Blocked   bool
	Reason    string
	Err       error
	Latency   time.Duration
	Delivered bool
	Surface   string
}

// AsyncGuardrailView is one external guardrail as the dashboard
// renders it: its configured mode (sync/async) plus its live
// verdict counts since this process started. Zeroes are honest —
// a guardrail with no verdicts yet has not run, not a broken feed.
type AsyncGuardrailView struct {
	Name                  string `json:"name"`
	Kind                  string `json:"kind"`
	Mode                  string `json:"mode"`
	Endpoint              string `json:"endpoint"`
	HasChecker            bool   `json:"has_checker"`
	Allowed               uint64 `json:"allowed"`
	BlockedAfterStart     uint64 `json:"blocked_after_start"`
	BlockedBeforeDelivery uint64 `json:"blocked_before_delivery"`
	BlockedAfterDelivery  uint64 `json:"blocked_after_delivery"`
	Errors                uint64 `json:"errors"`
}

// AsyncEvent is one recorded async verdict: when it landed, which
// guardrail gave it, and what it did. It carries no prompt or
// response text — only the guardrail name, the kind label and the
// outcome. Surface names the request path the verdict ran on
// ("chat" for /v1/chat/completions, "messages" for the /v1/messages
// relay); "" means the surface was not recorded (sync-mode external
// checks, which do not run through an AsyncRun).
type AsyncEvent struct {
	At        time.Time `json:"at"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Outcome   string    `json:"outcome"`
	LatencyMs int64     `json:"latency_ms"`
	Surface   string    `json:"surface,omitempty"`
}

// maxAsyncEvents is how many recent async verdicts the store keeps
// (newest last), mirroring maxEvents for the sync redaction feed.
const maxAsyncEvents = 50

// asyncGuardrailStats is the per-guardrail verdict counters behind
// one AsyncGuardrailView.
type asyncGuardrailStats struct {
	allowed               uint64
	blockedAfterStart     uint64
	blockedBeforeDelivery uint64
	blockedAfterDelivery  uint64
	errors                uint64
}

// VerdictHook is the optional callback the store invokes for every
// async verdict it records (phase "async", outcome as recorded).
// The server sets it once to feed /metrics; the store never
// requires it. It runs in the verdict goroutine — it must not
// block and must not call back into the store.
type VerdictHook func(phase string, res AsyncResult, outcome string)

// ---------------------------------------------------------------------------
// Store: checker registry, async runs, verdict recording.
// ---------------------------------------------------------------------------

// RegisterChecker attaches a checker to a configured external
// guardrail by name. It errors when the name is not a configured
// external guardrail or a checker is already registered — a
// wiring gap must fail loudly at startup, never silently leave a
// guardrail that can never run. main.go registers one
// HTTPClassifier per configured entry; tests register fakes.
func (s *Store) RegisterChecker(name string, checker AsyncChecker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, e := range s.cfg.External {
		if e.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("guardrails: no external guardrail named %q is configured", name)
	}
	if s.checkers == nil {
		s.checkers = map[string]AsyncChecker{}
	}
	if _, dup := s.checkers[name]; dup {
		return fmt.Errorf("guardrails: checker for %q is already registered", name)
	}
	s.checkers[name] = checker
	return nil
}

// Checker returns the checker registered for name, if any.
func (s *Store) Checker(name string) (AsyncChecker, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.checkers[name]
	return c, ok
}

// SetVerdictHook sets the callback invoked for every async verdict
// the store records (nil clears it).
func (s *Store) SetVerdictHook(hook VerdictHook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verdictHook = hook
}

// recordAsyncVerdict counts one verdict under its outcome, appends
// it to the recent-events list, and invokes the verdict hook. The
// hook runs after the lock is released, so a slow hook can never
// stall the store.
func (s *Store) recordAsyncVerdict(res AsyncResult, outcome string) {
	s.mu.Lock()
	if s.asyncStats == nil {
		s.asyncStats = map[string]*asyncGuardrailStats{}
	}
	st := s.asyncStats[res.Name]
	if st == nil {
		st = &asyncGuardrailStats{}
		s.asyncStats[res.Name] = st
	}
	switch outcome {
	case AsyncOutcomeAllowed:
		st.allowed++
	case AsyncOutcomeBlockedAfterStart:
		st.blockedAfterStart++
	case AsyncOutcomeBlockedBeforeDelivery:
		st.blockedBeforeDelivery++
	case AsyncOutcomeBlockedAfterDelivery:
		st.blockedAfterDelivery++
	case AsyncOutcomeError:
		st.errors++
	}
	s.asyncEvents = append(s.asyncEvents, AsyncEvent{
		At:        time.Now().UTC(),
		Name:      res.Name,
		Kind:      res.Kind,
		Outcome:   outcome,
		LatencyMs: res.Latency.Milliseconds(),
		Surface:   res.Surface,
	})
	if len(s.asyncEvents) > maxAsyncEvents {
		s.asyncEvents = append([]AsyncEvent(nil), s.asyncEvents[len(s.asyncEvents)-maxAsyncEvents:]...)
	}
	hook := s.verdictHook
	s.mu.Unlock()
	if hook != nil {
		hook("async", res, outcome)
	}
}

// asyncViews builds the dashboard's per-guardrail async state:
// every configured external entry (sync and async alike — the
// mode column is the point), with its live verdict counts.
// Views are in config order, so the dashboard order is stable.
func (s *Store) asyncViews() []AsyncGuardrailView {
	views := make([]AsyncGuardrailView, 0, len(s.cfg.External))
	for _, e := range s.cfg.External {
		v := AsyncGuardrailView{Name: e.Name, Kind: e.Kind, Mode: string(e.Mode), Endpoint: e.Endpoint}
		if _, ok := s.checkers[e.Name]; ok {
			v.HasChecker = true
		}
		if st := s.asyncStats[e.Name]; st != nil {
			v.Allowed = st.allowed
			v.BlockedAfterStart = st.blockedAfterStart
			v.BlockedBeforeDelivery = st.blockedBeforeDelivery
			v.BlockedAfterDelivery = st.blockedAfterDelivery
			v.Errors = st.errors
		}
		views = append(views, v)
	}
	if views == nil {
		views = []AsyncGuardrailView{}
	}
	return views
}

// asyncEventsSnapshot returns the recent async verdicts (newest
// last), never nil, so the dashboard renders an honest empty state
// instead of null.
func (s *Store) asyncEventsSnapshot() []AsyncEvent {
	events := append([]AsyncEvent(nil), s.asyncEvents...)
	if events == nil {
		events = []AsyncEvent{}
	}
	return events
}

// ---------------------------------------------------------------------------
// AsyncRun: one request's concurrent async checks.
// ---------------------------------------------------------------------------

// AsyncRun is the concurrent async check for one request: one
// goroutine per async-mode external guardrail, each running its
// checker against the request text while the provider call runs.
// The run is created by StartAsync just before the engine call;
// the server marks delivery as it happens (MarkDelivered) and
// closes the run when the handler returns (Finish), so a verdict
// that lands after the request ended is recorded as too late
// instead of claiming an action that never happened. A nil *AsyncRun
// means no async guardrail is configured — the server treats it as
// "nothing to wait on".
type AsyncRun struct {
	store     *Store
	streaming bool
	surface   string // request path: "chat" or "messages"
	delivered atomic.Bool
	finished  atomic.Bool
	done      chan struct{}
	blockedCh chan AsyncResult
	mu        sync.Mutex
	results   map[string]AsyncResult
	entries   []ExternalConfig
	wg        sync.WaitGroup
}

// StartAsync starts the async-mode external checks for one request
// text, concurrently with the provider call the server is about to
// make. streaming tells the run whether a BLOCK verdict cuts a
// stream (blocked_after_start) or withholds a not-yet-delivered
// response (blocked_before_delivery). It returns nil when no
// async-mode guardrail is configured — no goroutines, no overhead,
// and the caller needs no special case beyond the nil check. The
// checkers run on background contexts (their own timeouts bound
// them), so a slow verdict still lands in the store even after the
// response was delivered — recorded as too late, never lost.
func (s *Store) StartAsync(text string, streaming bool) *AsyncRun {
	return s.StartAsyncOn(text, streaming, "chat")
}

// StartAsyncScoped is StartAsync running only the async external
// guardrails named in allowed (CHECKLIST 277 (d), per-key
// guardrail scoping): allowed == nil runs every async guardrail;
// a non-nil list runs only the named ones.
func (s *Store) StartAsyncScoped(text string, streaming bool, allowed []string) *AsyncRun {
	return s.StartAsyncOnScoped(text, streaming, "chat", allowed)
}

// StartAsyncOn is StartAsync with the request path named: surface
// is recorded on every verdict the run records, so the dashboard
// can show which surface a verdict ran on ("chat" for
// /v1/chat/completions, "messages" for the /v1/messages relay).
// An empty surface records as "chat".
func (s *Store) StartAsyncOn(text string, streaming bool, surface string) *AsyncRun {
	return s.StartAsyncOnScoped(text, streaming, surface, nil)
}

// StartAsyncOnScoped is StartAsyncOn running only the async
// external guardrails named in allowed (CHECKLIST 277 (d)):
// allowed == nil runs every async guardrail; a non-nil list runs
// only the named ones. A skipped guardrail starts no goroutine
// and records no verdict.
func (s *Store) StartAsyncOnScoped(text string, streaming bool, surface string, allowed []string) *AsyncRun {
	if surface == "" {
		surface = "chat"
	}
	s.mu.Lock()
	var entries []ExternalConfig
	for _, e := range s.cfg.External {
		if e.Mode == ModeAsync {
			if allowed == nil || containsName(allowed, e.Name) {
				entries = append(entries, e)
			}
		}
	}
	s.mu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	run := &AsyncRun{
		store:     s,
		streaming: streaming,
		surface:   surface,
		done:      make(chan struct{}),
		blockedCh: make(chan AsyncResult, 1),
		results:   map[string]AsyncResult{},
		entries:   entries,
	}
	run.wg.Add(len(entries))
	for _, e := range entries {
		go run.checkOne(e, text)
	}
	go func() {
		run.wg.Wait()
		close(run.done)
		close(run.blockedCh)
	}()
	return run
}

// checkOne runs one async guardrail's checker and records its
// verdict. A missing checker is an error verdict, not a silent
// skip — the dashboard shows the guardrail as broken (errors > 0,
// has_checker false) instead of pretending it ran.
func (r *AsyncRun) checkOne(e ExternalConfig, text string) {
	defer r.wg.Done()
	start := time.Now()
	checker, ok := r.store.Checker(e.Name)
	if !ok {
		res := AsyncResult{Name: e.Name, Kind: e.Kind, Err: ErrNoChecker, Latency: time.Since(start), Surface: r.surface}
		r.setResult(res)
		r.store.recordAsyncVerdict(res, AsyncOutcomeError)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), classifierTimeout(e))
	defer cancel()
	verdict, err := checker.Check(ctx, text)
	lat := time.Since(start)
	res := AsyncResult{Name: e.Name, Kind: e.Kind, Latency: lat, Delivered: r.delivered.Load(), Surface: r.surface}
	var outcome string
	switch {
	case err != nil:
		res.Err = err
		outcome = AsyncOutcomeError
	case verdict.Blocked:
		res.Blocked = true
		res.Reason = verdict.Reason
		if res.Reason == "" {
			res.Reason = e.Kind
		}
		outcome = r.blockOutcome()
	default:
		outcome = AsyncOutcomeAllowed
	}
	r.setResult(res)
	// Record BEFORE waking the cut goroutine: the stats (and the
	// /metrics verdict hook) must be visible before any observer
	// can act on the block. Recording after the blockedCh send
	// let the server refuse and return while this goroutine was
	// still descheduled, so a reader acting on the refusal (the
	// handler, the dashboard, a test reading Stats() right after
	// the response) could observe pre-block stats — all zeros
	// despite a recorded 403 (full-gate load, 2026-11-18). The
	// hook is a fast in-memory counter increment, so recording
	// first cannot stall the wake-up.
	r.store.recordAsyncVerdict(res, outcome)
	if res.Blocked {
		// First BLOCK only: the channel wakes the server's cut
		// goroutine. A second BLOCK is still recorded above —
		// the channel is a wake-up, not the record.
		select {
		case r.blockedCh <- res:
		default:
		}
	}
}

// blockOutcome is the outcome for a BLOCK verdict landing now: a
// stream in flight is cut (blocked_after_start); a non-streaming
// response not yet delivered is withheld
// (blocked_before_delivery); anything landing after the run was
// finished, or after delivery, is too late
// (blocked_after_delivery).
// blockOutcome is the timing label for a BLOCK verdict. It
// describes what the CLIENT experienced, matching the server's
// two framing paths exactly: a verdict that lands after bytes
// reached the client cut a live stream (blocked_after_start for
// a stream, blocked_after_delivery for a non-streaming response
// already written); a verdict that lands before anything was
// delivered withheld the response (blocked_before_delivery —
// the client saw the plain 403, never a stream); a verdict that
// lands after the handler returned is too late to act on
// (blocked_after_delivery). Keying off the request's streaming
// flag instead of delivery would mislabel the before-commit
// 403 as a stream cut — a stream the client never saw.
func (r *AsyncRun) blockOutcome() string {
	if r.finished.Load() {
		return AsyncOutcomeBlockedAfterDelivery
	}
	if r.delivered.Load() {
		if r.streaming {
			return AsyncOutcomeBlockedAfterStart
		}
		return AsyncOutcomeBlockedAfterDelivery
	}
	return AsyncOutcomeBlockedBeforeDelivery
}

func (r *AsyncRun) setResult(res AsyncResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results[res.Name] = res
}

// MarkDelivered records that the response has started reaching
// the client (first SSE byte, or the non-streaming response
// written). A BLOCK verdict landing after this can no longer
// withhold a non-streaming response — it is too late.
func (r *AsyncRun) MarkDelivered() {
	if r == nil {
		return
	}
	r.delivered.Store(true)
}

// Finish closes the run: the handler has returned, so any verdict
// still to land is recorded as too late (blocked_after_delivery)
// instead of claiming a cut or a withhold that never happened.
func (r *AsyncRun) Finish() {
	if r == nil {
		return
	}
	r.finished.Store(true)
}

// Blocked is the channel the server's cut goroutine waits on: it
// receives the first BLOCK verdict (if any) and is closed when all
// verdicts have landed. A receive with ok=false means the run
// finished with no block — no cut.
func (r *AsyncRun) Blocked() <-chan AsyncResult {
	if r == nil {
		return nil
	}
	return r.blockedCh
}

// BlockVerdict reports whether any BLOCK verdict has landed so
// far, and the first one in config order (deterministic when
// several guardrails block). The server calls it after the engine
// call returns, to decide the terminal refusal frame (stream) or
// the withhold (non-streaming).
func (r *AsyncRun) BlockVerdict() (AsyncResult, bool) {
	if r == nil {
		return AsyncResult{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if res, ok := r.results[e.Name]; ok && res.Blocked {
			return res, true
		}
	}
	return AsyncResult{}, false
}

// Wait blocks until every async verdict has landed (or the run
// was created with no entries — a nil run has no Wait; callers
// nil-check first). Tests use it to assert verdict timing without
// sleeping on a fixed delay.
func (r *AsyncRun) Wait() {
	if r == nil {
		return
	}
	<-r.done
}

// Done is the channel closed when every async verdict has landed.
func (r *AsyncRun) Done() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.done
}

// ---------------------------------------------------------------------------
// Sync external checks: an external guardrail in sync mode runs
// before any provider is dialled, like the built-in blocking
// toggles.
// ---------------------------------------------------------------------------

// ExternalCheckResult is one sync-mode external check's outcome.
type ExternalCheckResult struct {
	Name    string
	Kind    string
	Blocked bool
	Reason  string
	Err     error
	Latency time.Duration
}

// CheckExternalSync runs every sync-mode external guardrail
// against text, in config order, each bounded by its own timeout.
// A BLOCK verdict means the request must be refused before any
// provider is dialled; a checker error fails OPEN (the request
// continues) and is recorded as an error verdict, so a classifier
// outage can never take the gateway down with it. Results are
// recorded in the store (allowed / blocked_before_delivery /
// error) so the dashboard shows sync-mode guardrails with the
// same live counts as async ones; they are NOT fed to the
// /metrics evaluation counter here — the request-phase evaluation
// is counted once by the caller's Check, and counting each
// external check again would break that invariant.
func (s *Store) CheckExternalSync(ctx context.Context, text string) []ExternalCheckResult {
	return s.CheckExternalSyncScoped(ctx, text, nil)
}

// CheckExternalSyncScoped is CheckExternalSync running only the
// external guardrails named in allowed (CHECKLIST 277 (d),
// per-key guardrail scoping): allowed == nil runs every sync
// external guardrail; a non-nil list runs only the named ones. A
// skipped guardrail records no verdict — it did not run for this
// key, so the feed must not claim it allowed the request.
func (s *Store) CheckExternalSyncScoped(ctx context.Context, text string, allowed []string) []ExternalCheckResult {
	s.mu.Lock()
	var entries []ExternalConfig
	for _, e := range s.cfg.External {
		if e.Mode == ModeSync {
			if allowed == nil || containsName(allowed, e.Name) {
				entries = append(entries, e)
			}
		}
	}
	s.mu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	out := make([]ExternalCheckResult, 0, len(entries))
	for _, e := range entries {
		start := time.Now()
		res := ExternalCheckResult{Name: e.Name, Kind: e.Kind}
		checker, ok := s.Checker(e.Name)
		if !ok {
			res.Err = ErrNoChecker
			res.Latency = time.Since(start)
			out = append(out, res)
			s.recordAsyncVerdict(AsyncResult{Name: e.Name, Kind: e.Kind, Err: res.Err, Latency: res.Latency}, AsyncOutcomeError)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, classifierTimeout(e))
		verdict, err := checker.Check(cctx, text)
		cancel()
		res.Latency = time.Since(start)
		switch {
		case err != nil:
			res.Err = err
			out = append(out, res)
			s.recordAsyncVerdict(AsyncResult{Name: e.Name, Kind: e.Kind, Err: err, Latency: res.Latency}, AsyncOutcomeError)
		case verdict.Blocked:
			res.Blocked = true
			res.Reason = verdict.Reason
			if res.Reason == "" {
				res.Reason = e.Kind
			}
			out = append(out, res)
			s.recordAsyncVerdict(AsyncResult{Name: e.Name, Kind: e.Kind, Blocked: true, Reason: res.Reason, Latency: res.Latency}, AsyncOutcomeBlockedBeforeDelivery)
		default:
			out = append(out, res)
			s.recordAsyncVerdict(AsyncResult{Name: e.Name, Kind: e.Kind, Latency: res.Latency}, AsyncOutcomeAllowed)
		}
	}
	return out
}
