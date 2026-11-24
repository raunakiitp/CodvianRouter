// M9.8 A2A HTTP layer: POST /a2a, a single JSON-RPC 2.0 endpoint on
// top of the transport-free core in internal/a2a.
//
// Methods (shapes only, studied from the reference router's a2a/route.ts — no
// the reference router code is copied):
//
//   - message/send     — create the task, run it synchronously through
//     the real router, return the task.
//   - message/stream   — same, but the task runs in a goroutine and
//     state transitions stream back as SSE.
//   - tasks/get        — return one task.
//   - tasks/cancel     — cancel a Submitted/Working task.
//   - SendMessage / SendStreamingMessage — the v1.0 aliases the reference router
//     ships (route.ts V1_METHOD_ALIASES). Those two are the whole
//     alias set: route.ts defines no aliases for tasks/get or
//     tasks/cancel, and neither do we.
//
// Two the reference router behaviours are deliberately NOT reproduced (see
// internal/a2a/a2a.go): the endpoint is never keyless — every call
// requires a valid gateway key, the owner is the FULL SHA-256 hex of
// the resolved gateway key id, and a task belonging to another owner
// behaves exactly as if it does not exist (never "forbidden, it
// exists"). And the endpoint is off unless the operator enables it
// (config A2AEnabled, default false): while off it answers 404 and
// never creates <data-dir>/a2a/tasks.db.
//
// Dispatch: the Processor (a2aProcessor) calls s.engine.Chat — the
// same internal dispatch path POST /v1/chat/completions uses after
// its gates, and the same call the batch worker (batches.go) and the
// Responses handler (responses.go) make. There is no second request
// path and no loopback HTTP call. CallerID is the caller's own
// gateway-key id, so routing, failover, ledger and budget attribution
// stay with that key; Endpoint marks the request as a chat
// completion for the replay exemption. The model is whatever the
// caller asked for (params.model / params.metadata.model), or "" —
// the engine then applies its normal routing/default for that key;
// no model is hardcoded here and no response is ever fabricated: a
// router error fails the task with the real error.
//
// Timeout: the processor context is bounded by s.timeout — the
// server's normal per-request timeout (DefaultTimeout 120s,
// overridable with WithTimeout), the same bound /v1/chat/completions
// applies. A processor that exceeds it fails the task with a timeout
// error; the HTTP request never hangs.
//
// Cancel-abort: message/send and message/stream both register
// the processor context's cancel func in s.a2aCancels under the
// task id before starting RunTask (send synchronously, stream in
// a goroutine) and unregister it on return. tasks/cancel (and a
// disconnecting SSE client) aborts the in-flight engine.Chat call
// via that func — but only AFTER the owner-scoped store
// transition has proven ownership and landed Cancelled: the
// cancel-func map is keyed by task id only, so aborting first
// would let a foreign caller abort a victim's router call. With
// Cancelled terminal, RunTask then cannot mark the task Failed
// and the task stays Cancelled.
//
// Pre-call admission: message/send and message/stream run the
// same pre-call gate chain as POST /v1/chat/completions
// (guardrails, plugins, access schedule, request/prompt budgets,
// the ReserveSpend budget reservation, token limits, quota pools
// and schedules, internal and team budgets, traffic limits,
// throttle delay and max sessions — see a2aAdmit) BEFORE the
// task is created and before any provider is called. A refused
// gate answers a JSON-RPC error and no task is created, no
// provider is called and no spend is recorded. The gates
// themselves are the chat handler's own functions, called in the
// same order — the call sequence is repeated, as in responses.go
// and gemini_native.go, because a shared cross-handler helper
// would have to live in server.go, outside this file's scope;
// no gate logic is duplicated here.
//
// Streaming mechanism: the core Service exposes no event feed, so
// the stream polls the task state (Service.GetTask, every 250 ms)
// and emits an SSE event on each observed state change, then a
// final event carrying the terminal task, and closes. A task that
// moves through two states between polls emits one event for the
// latest observed state — the poll interval, not fabrication,
// decides the granularity; the final task event always carries the
// true terminal state. A ": heartbeat" SSE comment is emitted at
// least every 15 s so idle proxies do not drop long streams.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/a2a"
	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// errA2ADisabled is returned by a2aService when the operator has
// not enabled the endpoint (config A2AEnabled, default false).
var errA2ADisabled = errors.New("a2a: endpoint not enabled")

// a2aPruneOptions returns the retention options a2aService wires
// into both the store (OpenWithLimits) and the prune loop, so the
// two can never disagree. The values come from config.A2A when
// set (design §4: retention_days default 7, max_tasks_per_key
// default 1000, prune_enabled default true); a nil A2A block
// means the design defaults. A test override (s.a2aPruneOpts)
// still wins outright, so tests can shorten the sweep interval
// — the interval is NOT a config key (fixed 1h, design §2).
func (s *Server) a2aPruneOptions() A2APruneOptions {
	if s.a2aPruneOpts != nil {
		return *s.a2aPruneOpts
	}
	opts := DefaultA2APruneOptions()
	if s.cfg != nil && s.cfg.A2A != nil {
		opts.RetentionDays = s.cfg.A2A.EffectiveRetentionDays()
		opts.MaxTasksPerKey = s.cfg.A2A.EffectiveMaxTasksPerKey()
		opts.PruneEnabled = s.cfg.A2A.PruneEnabled
	}
	return opts
}

// a2aStopPruneLoop stops the prune loop a2aService started, if
// any, and drops the handle. A no-op when the loop never started,
// and safe to call twice. POST /admin/shutdown (handleShutdown in
// guard.go) calls it — the in-package shutdown path — so the loop
// ends on server shutdown instead of lingering until process exit;
// tests call it too, to stop a loop started with a short interval.
// The wait happens outside a2aMu: Stop blocks until the goroutine
// exits.
func (s *Server) a2aStopPruneLoop() {
	s.a2aMu.Lock()
	loop, cancel := s.a2aPruneLoop, s.a2aPruneCancel
	s.a2aPruneLoop, s.a2aPruneCancel = nil, nil
	s.a2aMu.Unlock()
	if loop != nil {
		loop.Stop()
	}
	if cancel != nil {
		cancel()
	}
}

// a2aService returns the lazily-opened A2A task service. The store
// is opened on the first enabled call only: while the flag is off
// this returns errA2ADisabled before touching the disk, so a
// disabled endpoint never creates <data-dir>/a2a/tasks.db. Reads
// never create the DB file when the flag is off, for the same
// reason — there is no read path that opens the store first.
//
// The store opens with the retention limits (OpenWithLimits) and
// the prune loop starts exactly once, under the same a2aMu hold,
// next to a2aSvc — a second call returns the cached service and
// the cached loop, never a second goroutine.
func (s *Server) a2aService() (*a2a.Service, error) {
	s.a2aMu.Lock()
	defer s.a2aMu.Unlock()
	if s.a2aSvc != nil {
		return s.a2aSvc, nil
	}
	if s.cfg == nil || !s.cfg.A2AEnabled {
		return nil, errA2ADisabled
	}
	if s.cfg.DataDir == "" {
		return nil, errors.New("a2a: enabled but no data directory configured")
	}
	opts := s.a2aPruneOptions()
	store, err := a2a.OpenWithLimits(s.cfg.DataDir, opts.Limits())
	if err != nil {
		return nil, err
	}
	svc, err := a2a.NewService(store)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	s.a2aSvc = svc
	if s.a2aPruneLoop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.a2aPruneCancel = cancel
		s.a2aPruneLoop = StartA2APruneLoop(ctx, store, opts, s.logger)
	}
	return svc, nil
}

// a2aRegisterCancel records the cancel func for an in-flight
// streaming processor, keyed by task id. tasks/cancel (and a
// disconnecting SSE client) uses it to abort the router call.
func (s *Server) a2aRegisterCancel(id string, cancel context.CancelFunc) {
	s.a2aMu.Lock()
	defer s.a2aMu.Unlock()
	if s.a2aCancels == nil {
		s.a2aCancels = map[string]context.CancelFunc{}
	}
	s.a2aCancels[id] = cancel
}

// a2aUnregisterCancel drops the cancel func for a finished
// processor. It is the RunTask goroutine's job, via defer.
func (s *Server) a2aUnregisterCancel(id string) {
	s.a2aMu.Lock()
	defer s.a2aMu.Unlock()
	delete(s.a2aCancels, id)
}

// a2aCancelProcessor aborts the in-flight processor for id, if
// one is registered. A no-op when the task has no running
// processor (already terminal, or finished before any cancel
// arrived). Both message/send and message/stream register their
// processor here, so a synchronous send is abortable too.
func (s *Server) a2aCancelProcessor(id string) {
	s.a2aMu.Lock()
	cancel := s.a2aCancels[id]
	s.a2aMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 envelope.
// ---------------------------------------------------------------------------

// a2aRPCRequest is one JSON-RPC 2.0 request. ID is raw so a string,
// number or null id round-trips verbatim; Params is raw so each
// method parses what it needs (and the task store keeps the params
// exactly as received).
type a2aRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type a2aRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *a2aRPCError    `json:"error,omitempty"`
}

type a2aRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func a2aRPCID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func writeA2AResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, a2aRPCResponse{JSONRPC: "2.0", ID: a2aRPCID(id), Result: result})
}

func writeA2AError(w http.ResponseWriter, id json.RawMessage, code int, msg string, status int) {
	writeJSON(w, status, a2aRPCResponse{JSONRPC: "2.0", ID: a2aRPCID(id), Error: &a2aRPCError{Code: code, Message: msg}})
}

// ---------------------------------------------------------------------------
// Task views.
// ---------------------------------------------------------------------------

// a2aTaskView is the wire form of an a2a.Task: everything except
// Owner. The owner hash is the caller's own identity — revealing
// it buys nothing — so the wire carries no owner field at all.
type a2aTaskView struct {
	ID        string          `json:"id"`
	State     a2a.State       `json:"state"`
	Message   json.RawMessage `json:"message"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func a2aView(t *a2a.Task) a2aTaskView {
	return a2aTaskView{
		ID: t.ID, State: t.State, Message: t.Message,
		Result: t.Result, Error: t.Error,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func a2aIsTerminal(st a2a.State) bool {
	switch st {
	case a2a.StateCompleted, a2a.StateFailed, a2a.StateCancelled:
		return true
	}
	return false
}

// a2aV1Task rebuilds the A2A 1.0 Task shape for a SendMessage
// response (the reference router's buildV1Task shape, reimplemented): the
// reply text lives in task.status.message.parts[].text, the state
// is the TASK_STATE_* enum, and the same text is repeated in
// task.artifacts. The text is the router reply's content field,
// parsed out of the stored raw result — never invented.
func a2aV1Task(t *a2a.Task, contextID string) map[string]any {
	text := ""
	if len(t.Result) > 0 {
		var resp struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(t.Result, &resp); err == nil {
			text = resp.Content
		}
	}
	state := "TASK_STATE_" + strings.ToUpper(string(t.State))
	part := map[string]any{"text": text, "mediaType": "text/plain"}
	task := map[string]any{
		"id": t.ID,
		"status": map[string]any{
			"state": state,
			"message": map[string]any{
				"role":  "ROLE_AGENT",
				"parts": []any{part},
			},
		},
		"artifacts": []any{map[string]any{
			"role":  "ROLE_AGENT",
			"parts": []any{part},
		}},
	}
	if contextID != "" {
		task["contextId"] = contextID
	}
	return task
}

// ---------------------------------------------------------------------------
// Params.
// ---------------------------------------------------------------------------

// a2aRawMessage is one A2A message as the caller sent it: {role,
// content} or {role, parts:[...]} with text parts.
type a2aRawMessage struct {
	Role      string            `json:"role"`
	Content   string            `json:"content"`
	Parts     []json.RawMessage `json:"parts"`
	ContextID string            `json:"contextId"`
}

// a2aMessageText extracts the text of one raw message: the content
// string when present, otherwise the text parts joined with "\n"
// (a part is a bare string, or an object with a content/text
// string). The second return is false when the message carries no
// text at all.
func a2aMessageText(m a2aRawMessage) (string, bool) {
	if strings.TrimSpace(m.Content) != "" {
		return m.Content, true
	}
	var texts []string
	for _, p := range m.Parts {
		var s string
		if err := json.Unmarshal(p, &s); err == nil {
			if strings.TrimSpace(s) != "" {
				texts = append(texts, s)
			}
			continue
		}
		var obj struct {
			Content string `json:"content"`
			Text    string `json:"text"`
		}
		if err := json.Unmarshal(p, &obj); err == nil {
			if obj.Content != "" {
				texts = append(texts, obj.Content)
			} else if obj.Text != "" {
				texts = append(texts, obj.Text)
			}
		}
	}
	if len(texts) == 0 {
		return "", false
	}
	return strings.Join(texts, "\n"), true
}

// a2aSendParams is the params object of message/send and
// message/stream: messages[] or a single message, plus an optional
// model (params.model, or params.metadata.model as the reference router
// accepts) and the v1.0 contextId carried on the message.
type a2aSendParams struct {
	Message  json.RawMessage `json:"message"`
	Messages json.RawMessage `json:"messages"`
	Model    string          `json:"model"`
	Metadata json.RawMessage `json:"metadata"`
}

// a2aExtractMessages parses send/stream params into the chat
// messages the router runs, the requested model ("" when the
// caller named none — the engine then applies its normal routing
// for that key), and the v1.0 contextId when present. It mirrors
// the reference router's toMessageArray shapes (messages[] first, then a
// single message) without copying its code.
func a2aExtractMessages(params json.RawMessage) (msgs []provider.Message, model, contextID string, err error) {
	var p a2aSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, "", "", errors.New("params must be an object with messages[] or message")
	}
	model = p.Model
	if model == "" && len(p.Metadata) > 0 {
		var meta struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(p.Metadata, &meta); err == nil {
			model = meta.Model
		}
	}
	add := func(m a2aRawMessage) {
		text, ok := a2aMessageText(m)
		if !ok {
			return
		}
		role := m.Role
		if strings.TrimSpace(role) == "" {
			role = "user"
		}
		msgs = append(msgs, provider.Message{Role: role, Content: text})
		if contextID == "" {
			contextID = m.ContextID
		}
	}
	if len(p.Messages) > 0 && string(p.Messages) != "null" {
		var arr []a2aRawMessage
		if err := json.Unmarshal(p.Messages, &arr); err != nil {
			return nil, "", "", errors.New("messages must be an array of {role, content}")
		}
		for _, m := range arr {
			add(m)
		}
	}
	if len(msgs) == 0 && len(p.Message) > 0 && string(p.Message) != "null" {
		var m a2aRawMessage
		if err := json.Unmarshal(p.Message, &m); err != nil {
			return nil, "", "", errors.New("message must be an object with content or parts")
		}
		add(m)
	}
	if len(msgs) == 0 {
		return nil, "", "", errors.New("provide messages[] or message.content")
	}
	return msgs, model, contextID, nil
}

// a2aTaskIDParam reads a task id from tasks/get / tasks/cancel
// params: taskId or id, as the reference router accepts both.
func a2aTaskIDParam(params json.RawMessage) (string, error) {
	var p struct {
		TaskID string `json:"taskId"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", errors.New("params must be an object with taskId")
	}
	if p.TaskID != "" {
		return p.TaskID, nil
	}
	if p.ID != "" {
		return p.ID, nil
	}
	return "", errors.New("taskId required")
}

// ---------------------------------------------------------------------------
// Processor: the real router.
// ---------------------------------------------------------------------------

// a2aProcessor builds the a2a.Processor for one gateway key: given
// a task's stored params it runs them through the real router and
// returns the router's raw JSON reply. The caller is responsible
// for the timeout (message/send and message/stream both bound the
// context by s.timeout) and for cancellation (both paths wire
// CancelTask to this context — see the file header).
//
// admitted is the request a2aAdmit already built and gated: when
// non-nil the engine is called with exactly that request — the
// messages as extracted, the model, the per-request budget header
// attached by enforceRequestBudget (so the engine's mid-chain
// budget gate sees it), the compression stamp from
// applyCompression, and the cache controls — rather than a second
// request rebuilt from the stored params, which would silently
// drop all of those. When nil (no admission ran) the processor
// falls back to extracting the messages from the stored params,
// exactly as before.
func (s *Server) a2aProcessor(keyID string, admitted *provider.ChatRequest) a2a.Processor {
	return func(ctx context.Context, owner string, message json.RawMessage) (json.RawMessage, error) {
		var chatReq provider.ChatRequest
		if admitted != nil {
			chatReq = *admitted
		} else {
			msgs, model, _, err := a2aExtractMessages(message)
			if err != nil {
				return nil, err
			}
			chatReq = provider.ChatRequest{
				Model:    model,
				Messages: msgs,
				Endpoint: provider.EndpointChatCompletions,
				CallerID: keyID,
			}
		}
		if s.engine == nil {
			return nil, errors.New("a2a: no routing engine configured")
		}
		resp, err := s.engine.Chat(ctx, chatReq)
		if err != nil {
			return nil, err
		}
		if resp == nil {
			return nil, errors.New("a2a: engine returned no response")
		}
		raw, err := json.Marshal(resp)
		if err != nil {
			return nil, fmt.Errorf("a2a: encode router response: %w", err)
		}
		return raw, nil
	}
}

// ---------------------------------------------------------------------------
// Pre-call admission (M9.8 follow-up): the /v1/chat/completions
// gate chain, run for A2A before any provider is called.
// ---------------------------------------------------------------------------

// a2aAdmissionRefusal is one gate's refusal, translated out of
// the chat handler's HTTP refusal shape into a JSON-RPC error:
// Code is the JSON-RPC error code, Message is the gate's own
// message (never prompt text), and RetryAfter carries the gate's
// Retry-After value when it set one.
type a2aAdmissionRefusal struct {
	Code       int
	Message    string
	RetryAfter string
}

// a2aAdmitHeaders are the non-refusal headers the gates set on
// their (captured) response — fair-share penalisation, budget
// warnings, guardrail flagging, deployment pins — copied onto
// the real response by the caller so they are not silently lost
// when the gates write to a recorder instead of the live writer.
var a2aAdmitHeaderAllow = []string{
	"X-Codvian-FairShare-Penalized",
	"X-Codvian-Budget-Warning",
	"X-Codvian-Guardrail",
	"X-Codvian-Deployment-Override",
}

// a2aAdmit runs the /v1/chat/completions pre-call gate chain for
// one A2A send/stream, in the chat handler's order, before the
// task is created and before any provider is called. It calls
// the chat handler's own gate functions — the gate logic is
// shared, not duplicated; only the call sequence is repeated,
// as responses.go and gemini_native.go already do. A shared
// cross-handler helper would have to live in server.go (and be
// adopted by all three handlers), which is outside this file's
// scope; this helper is the smallest correct call into the
// existing gates.
//
// Each gate writes its refusal to a recorder, never to the live
// response: the refusal is translated into a JSON-RPC error by
// the caller, so a refused A2A call answers -32603 (or the
// gate's own JSON-RPC code, e.g. -32001 for a policy refusal)
// with HTTP 200, matching this endpoint's existing error
// convention — never a half-written HTTP error.
//
// On success it returns the admitted ChatRequest (the exact
// request the processor must send), the plugin-augmented context
// the engine call must use, the budget reservation id and the
// session key — both owned by the caller, which must settle the
// reservation and exit the session once the provider call has
// finished (for a stream, when its goroutine completes). On
// refusal it returns a non-nil refusal and no reservation or
// session is left held: a reservation made by an earlier gate is
// settled here before returning, so a refused request never
// holds headroom.
//
// Gates NOT applied, and why:
//   - checkClientVertexOverride: A2A params carry no
//     vertex_location/project fields, so there is nothing to
//     check (a nil override always passes).
//   - checkSessionTokenAdmission: Responses-session token caps
//     key off PreviousResponseID; A2A has no continuations.
//   - Async guardrails and the response guardrail: the chat
//     handler starts async classifiers concurrently with the
//     provider call and scans the reply before delivery. A2A
//     stores the reply as the task result instead of delivering
//     it on the wire, so there is no delivery point to withhold
//     at; the synchronous request-side guardrails (built-in
//     blocking plus sync external classifiers, inside
//     applyGuardrails) DO run. Async verdicts arriving
//     mid-flight are therefore not enforced for A2A — a known
//     limitation, recorded here, not a silent skip.
func (s *Server) a2aAdmit(ctx context.Context, r *http.Request, keyID string, msgs []provider.Message, model string) (chatReq *provider.ChatRequest, runCtx context.Context, resID, sessionKey string, headers http.Header, refusal *a2aAdmissionRefusal) {
	runCtx = ctx
	chatReq = &provider.ChatRequest{
		Model:         model,
		Messages:      msgs,
		Endpoint:      provider.EndpointChatCompletions,
		CallerID:      keyID,
		CacheControls: parseCacheControls(r),
	}
	headers = http.Header{}
	// refuse translates one gate's captured HTTP refusal into a
	// JSON-RPC error. The gate's own JSON-RPC code survives
	// when it set one (policy refusal -32001); otherwise a
	// 400 maps to -32602 (invalid params) and anything else to
	// -32603. An empty or unparseable body falls back to a
	// generic message naming the gate, never the prompt.
	refuse := func(rec *httptest.ResponseRecorder, gate string) *a2aAdmissionRefusal {
		msg := "request refused by " + gate
		code := -32603
		if rec.Code == http.StatusBadRequest {
			code = -32602
		}
		var body errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err == nil && body.Error.Message != "" {
			msg = body.Error.Message
			if body.Error.Code != 0 {
				code = body.Error.Code
			}
		}
		return &a2aAdmissionRefusal{Code: code, Message: msg, RetryAfter: rec.Header().Get("Retry-After")}
	}
	// collect merges one gate's non-refusal headers into the
	// headers the caller will copy onto the real response.
	collect := func(rec *httptest.ResponseRecorder) {
		for _, h := range a2aAdmitHeaderAllow {
			if v := rec.Header().Get(h); v != "" {
				headers.Set(h, v)
			}
		}
	}
	// fail settles a reservation made by an earlier gate and
	// returns the refusal: a refused request never holds
	// budget headroom or a session slot.
	fail := func(rec *httptest.ResponseRecorder, gate, res string) (*provider.ChatRequest, context.Context, string, string, http.Header, *a2aAdmissionRefusal) {
		s.settleReservation(res)
		return nil, ctx, "", "", headers, refuse(rec, gate)
	}

	// Admission before I/O (CHECKLIST 243): the key-budget cap
	// and the requests-per-minute ceiling, peeked without
	// recording a slot. The full checks still run below.
	rec := httptest.NewRecorder()
	if s.preBodyAdmission(rec, r) {
		return fail(rec, "pre-body admission", "")
	}
	collect(rec)
	// An unmapped intent tier is refused before any provider
	// is dialled, never silently defaulted.
	rec = httptest.NewRecorder()
	if s.checkIntentTier(rec, r, model) {
		return fail(rec, "intent tier", "")
	}
	collect(rec)
	// A gateway-provided local capability that is not ready
	// answers 503 here, before any provider is dialled.
	rec = httptest.NewRecorder()
	if !s.checkLocalReadiness(rec, r, model) {
		return fail(rec, "local readiness", "")
	}
	collect(rec)
	// The deployment's pinned parameters outrank the client's
	// fields; the processor sends the admitted request, so the
	// pins actually apply (they would be lost on a rebuild).
	rec = httptest.NewRecorder()
	s.applyDeploymentParams(rec, chatReq)
	collect(rec)
	// Guardrails run on the full inbound request BEFORE any
	// provider is dialled, under the caller's key scope.
	rec = httptest.NewRecorder()
	if blocked := s.applyGuardrails(ctx, rec, chatReq, s.keyGuardrailScope(r)); blocked {
		return fail(rec, "guardrail", "")
	}
	collect(rec)
	// Plugin pre-request hooks, after guardrails and before
	// budgets: a denial happens before any provider is
	// dialled. The returned ctx carries any plugin-added
	// outbound headers to the provider client.
	rec = httptest.NewRecorder()
	blocked, pluginCtx := s.applyPlugins(ctx, rec, chatReq)
	if blocked {
		return fail(rec, "plugin", "")
	}
	runCtx = pluginCtx
	collect(rec)
	// Lossless-first compression, after guardrails (which saw
	// the original prompt) and before every budget gate, so
	// the pre-send prompt-cost checks price the prompt as it
	// will actually be sent. The registry pre-pass
	// (session-dedup, CCR, Codex) runs first, at the same point.
	s.applyCompressionPrePass(r, chatReq)
	s.applyCompression(chatReq)
	// Per-key access schedule: outside allowed hours is a 403
	// here, before any budget is touched.
	rec = httptest.NewRecorder()
	if refused := s.checkKeyAccessSchedule(rec, r); refused {
		return fail(rec, "access schedule", "")
	}
	collect(rec)
	// Per-request budget (X-Codvian-Budget header), enforced
	// pre-send; the budget rides on chatReq for the engine's
	// mid-chain gate.
	rec = httptest.NewRecorder()
	if refused := s.enforceRequestBudget(rec, r, chatReq); refused {
		return fail(rec, "request budget", "")
	}
	collect(rec)
	// Pre-send prompt budget: the estimated prompt cost alone
	// must not blow a fail-closed budget.
	rec = httptest.NewRecorder()
	if refused := s.enforcePromptBudget(rec, r, chatReq); refused {
		return fail(rec, "prompt budget", "")
	}
	collect(rec)
	// Per-key rolling 5-hour USD budget, WITH the M5.8
	// reservation: the prompt's estimated cost is COMMITTED
	// (ReserveSpend) so parallel A2A calls cannot all pass on
	// the same headroom. resID is settled by the caller once
	// the provider call has finished — or here, by fail(),
	// when a later gate refuses.
	rec = httptest.NewRecorder()
	resID, refused := s.reserveKeyBudget(rec, r, chatReq)
	if refused {
		return fail(rec, "key budget", "")
	}
	collect(rec)
	// Per-key token limits, before the traffic check (a refused
	// request consumes no traffic slot).
	rec = httptest.NewRecorder()
	if refused := s.checkKeyTokenLimits(rec, r, "", model); refused {
		return fail(rec, "key token limits", resID)
	}
	collect(rec)
	// Fair-share quota pools, before the traffic check.
	rec = httptest.NewRecorder()
	if refused := s.checkQuotaPools(rec, r); refused {
		return fail(rec, "quota pools", resID)
	}
	collect(rec)
	// Quota schedules, right after the pool gate.
	rec = httptest.NewRecorder()
	if refused := s.checkQuotaSchedules(rec, r); refused {
		return fail(rec, "quota schedules", resID)
	}
	collect(rec)
	// Internal budgets, after the schedule gate.
	rec = httptest.NewRecorder()
	if refused := s.checkInternalBudgets(rec, r, model); refused {
		return fail(rec, "internal budgets", resID)
	}
	collect(rec)
	// P1.6 org/team policy: the authenticated key's team allowlists
	// (model, strategy) decide before dispatch — an A2A task is a
	// model call like any other dialect, so it is not a bypass.
	rec = httptest.NewRecorder()
	if !s.enforceTeamPolicy(rec, r, model, routingStrategy(chatReq)) {
		return fail(rec, "team policy", resID)
	}
	collect(rec)
	// Team budget: the key's active team, refused when its
	// shared budget is exceeded.
	rec = httptest.NewRecorder()
	if !s.checkTeamBudget(rec, r) {
		return fail(rec, "team budget", resID)
	}
	collect(rec)
	// Per-key traffic limits (RPM/TPM), enforced BEFORE any
	// provider is called, after every budget gate — so a
	// request refused by a budget never consumes a traffic
	// slot, and a request refused here never reaches a
	// provider.
	rec = httptest.NewRecorder()
	if refused := s.checkKeyTrafficForChat(rec, r, chatReq); refused {
		return fail(rec, "key traffic", resID)
	}
	collect(rec)
	// Per-key throttle delay + max sessions, after every
	// refusal gate (a refused request is never delayed and
	// never takes a session slot) and before the engine is
	// called. The session slot is held for the whole provider
	// call by the caller.
	if !s.throttleKeyDelay(r) {
		s.settleReservation(resID)
		return nil, ctx, "", "", headers, &a2aAdmissionRefusal{Code: -32603, Message: "request cancelled while waiting for throttle delay"}
	}
	rec = httptest.NewRecorder()
	sessionKey, admitted := s.tryEnterKeySession(rec, r)
	if !admitted {
		return fail(rec, "key sessions", resID)
	}
	collect(rec)
	return chatReq, runCtx, resID, sessionKey, headers, nil
}

// a2aCopyAdmitHeaders copies the non-refusal headers a2aAdmit
// collected onto the live response. Call before the first write.
func a2aCopyAdmitHeaders(w http.ResponseWriter, headers http.Header) {
	for _, h := range a2aAdmitHeaderAllow {
		if v := headers.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
}

// writeA2AAdmissionRefusal answers a refused gate as a JSON-RPC
// error, carrying the gate's Retry-After when it set one. The
// HTTP status stays 200, matching this endpoint's existing
// JSON-RPC error convention.
func (s *Server) writeA2AAdmissionRefusal(w http.ResponseWriter, id json.RawMessage, refusal *a2aAdmissionRefusal) {
	if refusal.RetryAfter != "" {
		w.Header().Set("Retry-After", refusal.RetryAfter)
	}
	writeA2AError(w, id, refusal.Code, refusal.Message, http.StatusOK)
}

// ---------------------------------------------------------------------------
// Handler.
// ---------------------------------------------------------------------------

// handleA2A serves POST /a2a, the single A2A JSON-RPC 2.0 endpoint.
// Auth runs twice by design: the guard wrapper (Tier MANAGEMENT,
// scope "chat") requires a valid gateway key, and this handler
// checks s.authorized again so the endpoint can never go keyless
// even if auth were relaxed for the tier. The disabled check runs
// before the body is parsed and before any store is opened.
func (s *Server) handleA2A(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	svc, err := s.a2aService()
	if err != nil {
		if errors.Is(err, errA2ADisabled) {
			writeA2AError(w, nil, -32000, "A2A not enabled", http.StatusNotFound)
			return
		}
		// Generic only: the store-open error can carry the
		// data-dir path, which must never reach the client.
		// The detail stays in the server log.
		s.logger.Printf("a2a: store unavailable: %v", err)
		writeServerError(w, http.StatusServiceUnavailable, "a2a: store unavailable")
		return
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		// The verifier cannot name this key: no owner can be
		// derived, so fail closed rather than serve ownerless.
		s.unauthorized(w, r)
		return
	}
	owner := a2a.OwnerIDForKey(keyID)

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req a2aRPCRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeA2AError(w, nil, -32700, "parse error: invalid JSON", http.StatusOK)
		return
	}
	// Trailing bytes after the first JSON value are rejected:
	// a second value (or garbage) after the request object is
	// not part of the request and must not be silently ignored.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		writeA2AError(w, nil, -32700, "parse error: trailing data after JSON value", http.StatusOK)
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeA2AError(w, req.ID, -32600, `invalid request: jsonrpc must be "2.0" and method is required`, http.StatusOK)
		return
	}
	method := req.Method
	isV1 := false
	switch method {
	case "SendMessage":
		method, isV1 = "message/send", true
	case "SendStreamingMessage":
		method, isV1 = "message/stream", true
	}
	switch method {
	case "message/send":
		s.handleA2ASend(w, r, svc, owner, keyID, req, isV1)
	case "message/stream":
		s.handleA2AStream(w, r, svc, owner, keyID, req, isV1)
	case "tasks/get":
		s.handleA2AGet(w, svc, owner, req)
	case "tasks/cancel":
		s.handleA2ACancel(w, svc, owner, req)
	case "skills/list":
		s.handleA2ASkillsList(w, req)
	case "skills/invoke":
		s.handleA2ASkillsInvoke(w, r, keyID, req)
	default:
		writeA2AError(w, req.ID, -32601, "method not found: "+method, http.StatusOK)
	}
}

// ---------------------------------------------------------------------------
// Skills dispatch (Integration-B item A).
// ---------------------------------------------------------------------------

// a2aSkillSource returns the live router state the A2A skills read,
// bound to this request's context: the same serverMCPSource the
// MCP HTTP transport uses (mcp_http.go), so the skills and the MCP
// tools can never disagree about providers, combos, ledger or
// cost state. The bound copy carries the request context (and its
// timeout/cancel), never context.Background().
func (s *Server) a2aSkillSource(ctx context.Context) a2a.SkillSource {
	src := &serverMCPSource{srv: s, st: mcpHTTPStateFor(s)}
	if bound, ok := src.WithContext(ctx).(a2a.SkillSource); ok {
		return bound
	}
	return src
}

// a2aSkillErrorCode maps an InvokeSkill error to its JSON-RPC code:
//   - unknown skill → -32602 (invalid params: the method names a
//     skill that does not exist);
//   - a skill input error (mcp.ErrInvalidInput, or any other
//     client-side failure — bad args, a caller-identity failure
//     such as a gateway key naming another key) → -32602;
//   - a backing-store failure ("unavailable: ...") or a nil
//     source → -32603.
func a2aSkillErrorCode(err error) int {
	if errors.Is(err, mcp.ErrInvalidInput) {
		return -32602
	}
	msg := err.Error()
	if strings.Contains(msg, "unknown skill") {
		return -32602
	}
	if strings.HasPrefix(msg, "unavailable: ") || strings.HasPrefix(msg, "a2a: nil SkillSource") {
		return -32603
	}
	return -32602
}

// handleA2ASkillsList runs skills/list: the six skill descriptors
// from the A2A skill registry, in fixed order. No live state is
// read and no args are needed — params, if any, are ignored.
func (s *Server) handleA2ASkillsList(w http.ResponseWriter, req a2aRPCRequest) {
	writeA2AResult(w, req.ID, map[string]any{"skills": a2a.SkillDescriptors()})
}

// handleA2ASkillsInvoke runs skills/invoke: {"skill": "<name>",
// "args": {...}} ("name" is accepted as an alias for "skill").
// The skill runs against the live SkillSource with the caller on
// the context as a gateway-key caller — CallerGatewayKey with the
// already-authenticated key id — so the key-scoped skills
// (quota-management, cost-analysis) read only that key's ledger
// rows and reject any key_id naming another key. The HTTP path
// NEVER constructs CallerLocalOperator: there is no stdio/local
// A2A transport, and local-operator trust must never be reachable
// over HTTP. A context with no caller would fail closed inside
// resolveSkillKeyID; this path always sets one.
func (s *Server) handleA2ASkillsInvoke(w http.ResponseWriter, r *http.Request, keyID string, req a2aRPCRequest) {
	var p struct {
		Skill string          `json:"skill"`
		Name  string          `json:"name"`
		Args  json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeA2AError(w, req.ID, -32602, "invalid params: "+err.Error(), http.StatusOK)
		return
	}
	name := strings.TrimSpace(p.Skill)
	if name == "" {
		name = strings.TrimSpace(p.Name)
	}
	if name == "" {
		writeA2AError(w, req.ID, -32602, "invalid params: skill required", http.StatusOK)
		return
	}
	ctx := a2a.WithCaller(r.Context(), a2a.Caller{Kind: a2a.CallerGatewayKey, KeyID: keyID})
	raw, err := a2a.InvokeSkill(ctx, s.a2aSkillSource(ctx), name, p.Args)
	if err != nil {
		writeA2AError(w, req.ID, a2aSkillErrorCode(err), err.Error(), http.StatusOK)
		return
	}
	var result any
	if err := json.Unmarshal(raw, &result); err != nil {
		writeA2AError(w, req.ID, -32603, "a2a: encode skill result: "+err.Error(), http.StatusOK)
		return
	}
	writeA2AResult(w, req.ID, result)
}

// handleA2ASend runs message/send (and the SendMessage alias):
// admit the call through the pre-call gate chain, create the
// task in Submitted, then execute it synchronously through the
// real router. A refused gate answers a JSON-RPC error before
// any task is created or provider called. The response always
// returns the task — Completed with the router's raw JSON reply
// as result, or Failed with the router's real error — never a
// JSON-RPC error for a router failure: the task IS the result of
// the call. The one exception is a task cancelled mid-flight:
// RunTask then returns the Cancelled task itself (see
// internal/a2a/service.go), and a -32603 is never raised for
// the lost terminal transition.
func (s *Server) handleA2ASend(w http.ResponseWriter, r *http.Request, svc *a2a.Service, owner, keyID string, req a2aRPCRequest, isV1 bool) {
	msgs, model, contextID, err := a2aExtractMessages(req.Params)
	if err != nil {
		writeA2AError(w, req.ID, -32602, "invalid params: "+err.Error(), http.StatusOK)
		return
	}
	// Bounded by the server's normal per-request timeout (see the
	// file header): a timed-out processor fails the task with
	// context deadline exceeded, never a hung request.
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	chatReq, runCtx, resID, sessionKey, admitHeaders, refusal := s.a2aAdmit(ctx, r, keyID, msgs, model)
	if refusal != nil {
		s.writeA2AAdmissionRefusal(w, req.ID, refusal)
		return
	}
	a2aCopyAdmitHeaders(w, admitHeaders)
	// The reservation is held across the provider call and
	// settled once this handler returns, after the engine has
	// recorded the actual usage row; the session slot is held
	// for the whole call the same way.
	defer s.settleReservation(resID)
	defer s.exitKeySession(sessionKey)
	task, err := svc.CreateTask(owner, req.Params)
	if err != nil {
		if errors.Is(err, a2a.ErrTaskLimitExceeded) {
			// The per-key retention cap: explicit -32603
			// carrying the cap message itself, not the
			// generic create-task failure below.
			writeA2AError(w, req.ID, -32603, err.Error(), http.StatusOK)
			return
		}
		writeA2AError(w, req.ID, -32603, "a2a: create task: "+err.Error(), http.StatusOK)
		return
	}
	// Register the send context's cancel func, exactly like the
	// stream path: tasks/cancel aborts an in-flight send the
	// same way it aborts a stream. Cancelling the timeout
	// context cancels runCtx (derived from it) as well.
	s.a2aRegisterCancel(task.ID, cancel)
	defer s.a2aUnregisterCancel(task.ID)
	task, err = svc.RunTask(runCtx, owner, task.ID, s.a2aProcessor(keyID, chatReq))
	if err != nil {
		if errors.Is(err, a2a.ErrNotFound) {
			writeA2AError(w, req.ID, -32602, "task not found", http.StatusOK)
			return
		}
		if errors.Is(err, a2a.ErrIllegalTransition) {
			// The task was cancelled between creation and
			// the RunTask call (or the service could not
			// absorb the race): return the task itself —
			// Cancelled — not a -32603 naming the lost
			// transition.
			if cur, gerr := svc.GetTask(owner, task.ID); gerr == nil {
				if isV1 {
					writeA2AResult(w, req.ID, map[string]any{"task": a2aV1Task(cur, contextID)})
					return
				}
				writeA2AResult(w, req.ID, map[string]any{"task": a2aView(cur)})
				return
			}
		}
		writeA2AError(w, req.ID, -32603, err.Error(), http.StatusOK)
		return
	}
	if isV1 {
		writeA2AResult(w, req.ID, map[string]any{"task": a2aV1Task(task, contextID)})
		return
	}
	writeA2AResult(w, req.ID, map[string]any{"task": a2aView(task)})
}

// handleA2AStream runs message/stream (and the SendStreamingMessage
// alias): admit the call through the pre-call gate chain, create
// the task, run it in a goroutine — RunTask is synchronous, so the
// handler must not hold on it — and stream observed state
// transitions as SSE until the task reaches a terminal state.
// Cancelling the task aborts the processor via its context (see
// a2aRegisterCancel); a disconnecting client does the same instead
// of leaving orphaned router work running. A refused gate answers
// a JSON-RPC error before any task is created or provider called.
func (s *Server) handleA2AStream(w http.ResponseWriter, r *http.Request, svc *a2a.Service, owner, keyID string, req a2aRPCRequest, isV1 bool) {
	msgs, model, contextID, err := a2aExtractMessages(req.Params)
	if err != nil {
		writeA2AError(w, req.ID, -32602, "invalid params: "+err.Error(), http.StatusOK)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeServerError(w, http.StatusInternalServerError, "a2a: streaming not supported")
		return
	}
	// The processor context is deliberately DETACHED from
	// r.Context(): it is bounded by s.timeout, but a client
	// disconnect must not cancel it directly. The poll loop
	// below owns the disconnect (CancelTask first, then
	// cancel()); had the processor context derived from
	// r.Context(), the transport would cancel it the moment the
	// client went away and the aborted RunTask could commit
	// Working→Failed before CancelTask landed — the exact
	// nondeterministic Failed-on-disconnect this reorder exists
	// to prevent. Admission still runs on this context; the
	// gates that need the request (throttle, ledger reads) use
	// r.Context() themselves.
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), s.timeout)
	defer timeoutCancel()
	chatReq, pluginCtx, resID, sessionKey, admitHeaders, refusal := s.a2aAdmit(timeoutCtx, r, keyID, msgs, model)
	if refusal != nil {
		s.writeA2AAdmissionRefusal(w, req.ID, refusal)
		return
	}
	// The session slot is held for the whole stream: the poll
	// loop below only returns once the task is terminal (or the
	// client/timeout went away), so exiting here covers the
	// full provider call.
	defer s.exitKeySession(sessionKey)
	task, err := svc.CreateTask(owner, req.Params)
	if err != nil {
		// No provider call was made: release the reservation
		// the admission just made.
		s.settleReservation(resID)
		if errors.Is(err, a2a.ErrTaskLimitExceeded) {
			// The per-key retention cap: explicit -32603
			// carrying the cap message itself, not the
			// generic create-task failure below.
			writeA2AError(w, req.ID, -32603, err.Error(), http.StatusOK)
			return
		}
		writeA2AError(w, req.ID, -32603, "a2a: create task: "+err.Error(), http.StatusOK)
		return
	}
	ctx, cancel := context.WithCancel(pluginCtx)
	defer cancel()
	s.a2aRegisterCancel(task.ID, cancel)
	go func() {
		defer s.a2aUnregisterCancel(task.ID)
		defer cancel()
		// The reservation is held across the provider call and
		// settled when RunTask completes, after the engine has
		// recorded the actual usage row — not when the HTTP
		// handler returns, which may be later (polling) or
		// earlier (client disconnect).
		defer s.settleReservation(resID)
		if _, err := svc.RunTask(ctx, owner, task.ID, s.a2aProcessor(keyID, chatReq)); err != nil {
			// RunTask fails here only when the final transition
			// cannot land — the usual case is a task cancelled
			// mid-flight, where Cancelled already won and there
			// is nothing left to record. The store stays the
			// truth; log and move on.
			s.logger.Printf("a2a: stream task RunTask ended: %v", err)
		}
	}()

	a2aCopyAdmitHeaders(w, admitHeaders)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	emit := func(event string, data any) {
		raw, err := json.Marshal(data)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		fl.Flush()
	}
	// The v1.0 stream carries the same state events; the final
	// task event carries the 1.0 Task shape, matching the
	// synchronous SendMessage reply.

	// Poll-based streaming (see the file header): the Service
	// exposes no event feed, so GetTask is polled every 250 ms
	// and an event is emitted on each observed state change. A
	// ": heartbeat" comment goes out at least every 15 s so
	// idle proxies do not drop long streams; the terminal event
	// still arrives within one poll interval of the task ending.
	lastState := a2a.State("")
	lastHeartbeat := time.Now()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			// Owner-scoped CancelTask FIRST — the actor is
			// provably this stream's owner, and landing
			// Cancelled before aborting the processor is
			// what makes the outcome deterministic: had
			// cancel() run first, the aborted RunTask's
			// Working→Failed transition could commit
			// before CancelTask and end the task Failed
			// instead of Cancelled. Only then abort the
			// processor, mirroring handleA2ACancel.
			_, _ = svc.CancelTask(owner, task.ID)
			cancel()
			return
		case <-timeoutCtx.Done():
			// The processor context is bounded by s.timeout: if
			// the engine ignores cancellation the poll loop
			// must still end instead of polling GetTask every
			// 250 ms forever. (The timeout also cancels the
			// processor context, so a well-behaved engine has
			// already returned by now.)
			return
		case <-ticker.C:
			cur, err := svc.GetTask(owner, task.ID)
			if err != nil {
				return
			}
			if cur.State != lastState {
				emit("task-state", map[string]any{
					"task_id":    cur.ID,
					"from_state": string(lastState),
					"to_state":   string(cur.State),
					"at":         cur.UpdatedAt.Format(time.RFC3339Nano),
				})
				lastState = cur.State
			}
			if a2aIsTerminal(cur.State) {
				if isV1 {
					emit("task", map[string]any{"task": a2aV1Task(cur, contextID)})
					return
				}
				emit("task", map[string]any{"task": a2aView(cur)})
				return
			}
			if time.Since(lastHeartbeat) >= 15*time.Second {
				fmt.Fprint(w, ": heartbeat\n\n")
				fl.Flush()
				lastHeartbeat = time.Now()
			}
		}
	}
}

// handleA2AGet runs tasks/get: the task, owner-scoped. A wrong
// owner (or a missing id) answers -32602 "task not found" —
// indistinguishable from absence, never "forbidden, it exists".
func (s *Server) handleA2AGet(w http.ResponseWriter, svc *a2a.Service, owner string, req a2aRPCRequest) {
	id, err := a2aTaskIDParam(req.Params)
	if err != nil {
		writeA2AError(w, req.ID, -32602, "invalid params: "+err.Error(), http.StatusOK)
		return
	}
	task, err := svc.GetTask(owner, id)
	if err != nil {
		if errors.Is(err, a2a.ErrNotFound) {
			writeA2AError(w, req.ID, -32602, "task not found", http.StatusOK)
			return
		}
		writeA2AError(w, req.ID, -32603, err.Error(), http.StatusOK)
		return
	}
	writeA2AResult(w, req.ID, map[string]any{"task": a2aView(task)})
}

// handleA2ACancel runs tasks/cancel: cancel from Submitted or
// Working only. Ownership is proven FIRST via the owner-scoped
// store transition — only then is the in-flight processor (if
// any) aborted via its context, so a foreign caller who guesses a
// task id can never abort the victim's router call (the map of
// cancel funcs is keyed by task id only, so abort-before-check
// let a -32602 "not found" answer carry a real side effect: the
// victim's Working task flipped to Failed). The order is store
// transition first, processor abort second — never the other way
// round — and it covers both send and stream: both register
// their processor context here, so a synchronous send is
// abortable exactly like a stream. A wrong owner answers -32602
// "task not found"; a terminal task answers -32602 naming the
// illegal transition.
func (s *Server) handleA2ACancel(w http.ResponseWriter, svc *a2a.Service, owner string, req a2aRPCRequest) {
	id, err := a2aTaskIDParam(req.Params)
	if err != nil {
		writeA2AError(w, req.ID, -32602, "invalid params: "+err.Error(), http.StatusOK)
		return
	}
	task, err := svc.CancelTask(owner, id)
	if err != nil {
		if errors.Is(err, a2a.ErrNotFound) {
			writeA2AError(w, req.ID, -32602, "task not found", http.StatusOK)
			return
		}
		if errors.Is(err, a2a.ErrIllegalTransition) {
			writeA2AError(w, req.ID, -32602, err.Error(), http.StatusOK)
			return
		}
		writeA2AError(w, req.ID, -32603, err.Error(), http.StatusOK)
		return
	}
	// Ownership proven and the task is now Cancelled (terminal),
	// so aborting the processor cannot flip it to Failed: RunTask
	// can no longer land any transition on it.
	s.a2aCancelProcessor(id)
	writeA2AResult(w, req.ID, map[string]any{"task": a2aView(task)})
}
