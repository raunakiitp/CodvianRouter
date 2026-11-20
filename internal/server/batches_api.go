// Batch API handlers (M8.8 — POST/GET/DELETE /v1/batches,
// [id], [id]/cancel, delete-completed).
//
// Route registration and auth live with the coordinator
// (guard.go / server.go New() — NOT this file): every route
// below is TierManagement, gateway-key auth, scope "chat" (see
// BatchesRoutes). Handlers here assume auth already happened;
// they answer 503 only when the server has no config (hence no
// data dir to store batches under) or no engine (a batch with
// nothing to execute it would be fake work).
//
// Shapes follow the OpenAI Batch API: a Batch object is
// {id, object: "batch", endpoint, errors, input_file_id,
// completion_window, status, output_file_id, error_file_id,
// created_at, in_progress_at, expires_at, finalizing_at,
// completed_at, failed_at, expired_at, cancelling_at,
// cancelled_at, request_counts {total, completed, failed},
// metadata, model, usage}. Unknown ids answer 404 — never a
// silent success, and never a 403 that would confirm a foreign
// batch exists. Cancel on a terminal batch answers 409; delete
// on a non-terminal batch answers 409.
//
// INPUT-FILE OWNERSHIP GAP (stated, not hidden): the Files
// store records no owner for an uploaded file (M8.7 design —
// the Files API itself lists every file to any key), so the
// create handler CANNOT check that the input file belongs to
// the caller's key. the reference router denies null-owner / foreign input
// files (GHSA-2jm2-mpx8-6523); here that check is impossible
// without an owner column on files.File, which is a
// coordinator decision for the Files store, not this slice.
// Batch RECORDS are owner-scoped (see batchVisible): a foreign
// batch id answers 404 on every route.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/files"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// batchRoute describes one batch route for the coordinator to
// register in guard.go (TierManagement, gateway-key auth) and
// server.go New(). Scope is the proposal; the coordinator
// assigns the final scope at registration.
type batchRoute struct {
	Method      string
	Path        string
	Scope       string // proposed gateway-key scope
	Description string
}

// BatchesRoutes returns the six M8.8 routes with their proposed
// scopes. All propose "chat": a batch IS a set of chat
// completions (its lines execute through engine.Chat), and the
// closest existing analogues — the Files API (batch inputs)
// and the Responses stored-object API — scope every route
// "chat" as well. No separate "batch" scope exists in this
// codebase; inventing one would be a new auth concept, not a
// route classification. Reads and writes share the scope:
// the guard table has no read/write split for this shape.
func BatchesRoutes() []batchRoute {
	return []batchRoute{
		{Method: "POST", Path: "/v1/batches", Scope: "chat", Description: "Create a batch from an input file id (endpoint must be /v1/chat/completions, completion_window must be 24h); returns the Batch object in status validating."},
		{Method: "GET", Path: "/v1/batches", Scope: "chat", Description: "List the caller's batches, newest first (?limit= default 20, max 100)."},
		{Method: "GET", Path: "/v1/batches/{id}", Scope: "chat", Description: "Retrieve one batch by id; unknown or foreign ids answer 404."},
		{Method: "POST", Path: "/v1/batches/{id}/cancel", Scope: "chat", Description: "Cancel a running batch: cancelling -> cancelled, the worker stops starting new lines; already-terminal batches answer 409."},
		{Method: "DELETE", Path: "/v1/batches/{id}", Scope: "chat", Description: "Delete one terminal batch record; non-terminal batches answer 409, unknown or foreign ids answer 404."},
		{Method: "DELETE", Path: "/v1/batches/delete-completed", Scope: "chat", Description: "Delete all of the caller's terminal (completed/cancelled/failed/expired) batch records; in-progress batches are never deleted."},
	}
}

// batchListResponse is the GET /v1/batches payload: OpenAI's
// {object: "list", data: [...], has_more, first_id, last_id}
// shape. Data is never null — an empty store answers an empty
// array.
type batchListResponse struct {
	Object  string   `json:"object"`
	Data    []*Batch `json:"data"`
	HasMore bool     `json:"has_more"`
	FirstID *string  `json:"first_id"`
	LastID  *string  `json:"last_id"`
}

// batchDeleteResponse is the DELETE /v1/batches/{id} payload.
type batchDeleteResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

// batchDeleteCompletedResponse is the DELETE
// /v1/batches/delete-completed payload: the ids removed.
type batchDeleteCompletedResponse struct {
	Object       string   `json:"object"`
	Data         []string `json:"data"`
	DeletedCount int      `json:"deleted_count"`
}

// batchCreateRequest is the decode shape for POST /v1/batches.
// Metadata is raw so a non-object value is a 400 naming the
// field instead of a silent ignore.
type batchCreateRequest struct {
	InputFileID      string          `json:"input_file_id"`
	Endpoint         string          `json:"endpoint"`
	CompletionWindow string          `json:"completion_window"`
	Metadata         json.RawMessage `json:"metadata"`
}

// handleBatches is the dispatcher for the whole /v1/batches
// surface: POST /v1/batches (create), GET /v1/batches (list),
// GET|DELETE /v1/batches/{id}, POST /v1/batches/{id}/cancel,
// DELETE /v1/batches/delete-completed.
func (s *Server) handleBatches(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/batches")
	rest = strings.TrimPrefix(rest, "/")
	switch {
	case rest == "":
		switch r.Method {
		case http.MethodPost:
			s.handleBatchesCreate(w, r)
		case http.MethodGet:
			s.handleBatchesList(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case rest == "delete-completed":
		if r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleBatchesDeleteCompleted(w, r)
	case strings.HasSuffix(rest, "/cancel"):
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleBatchesCancel(w, r, strings.TrimSuffix(rest, "/cancel"))
	default:
		if strings.Contains(rest, "/") {
			writeError(w, http.StatusNotFound, "batch not found: "+rest)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.handleBatchesGet(w, r, rest)
		case http.MethodDelete:
			s.handleBatchesDelete(w, r, rest)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

// handleBatchesCreate serves POST /v1/batches.
func (s *Server) handleBatchesCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.engine == nil {
		writeServerError(w, http.StatusServiceUnavailable, "batch unavailable: no chat engine configured")
		return
	}
	var req batchCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.InputFileID == "" {
		writeError(w, http.StatusBadRequest, "invalid request: missing \"input_file_id\"")
		return
	}
	if req.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "invalid request: missing \"endpoint\"")
		return
	}
	if req.Endpoint != "/v1/chat/completions" {
		writeError(w, http.StatusBadRequest, "invalid request: endpoint must be \"/v1/chat/completions\"")
		return
	}
	window := req.CompletionWindow
	if window == "" {
		window = "24h" // the only valid value, so the default cannot be wrong
	}
	if window != "24h" {
		writeError(w, http.StatusBadRequest, "invalid request: completion_window must be \"24h\"")
		return
	}
	var metadata map[string]any
	if len(req.Metadata) > 0 {
		if err := json.Unmarshal(req.Metadata, &metadata); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request: \"metadata\" must be an object")
			return
		}
	}
	st, err := s.batchesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	fs, err := s.filesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	// The input file must resolve in the Files store. An
	// invalid id shape and an unknown id both answer 404 — the
	// difference is not observable to the caller.
	f, err := fs.Get(req.InputFileID)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) || errors.Is(err, files.ErrInvalidID) {
			writeError(w, http.StatusNotFound, "input file not found: "+req.InputFileID)
			return
		}
		s.logger.Printf("batches create: files get: %v", err)
		writeServerError(w, http.StatusInternalServerError, "could not read the input file")
		return
	}
	if f.Purpose != "batch" {
		writeError(w, http.StatusBadRequest, "invalid request: input file purpose must be \"batch\" (got "+quoteForError(f.Purpose)+")")
		return
	}
	// G21 — per-key model policy (GAP-TRACK §9, G20 Blocker B1):
	// the gate runs here, at create time, before any batch row
	// exists. The worker runs async with no http.ResponseWriter,
	// so it cannot refuse with a 403 — and a refused batch must
	// create no row and start no engine call.
	if refused := s.enforceBatchModelPolicy(w, r, fs, req.InputFileID); refused {
		return
	}
	b, err := st.createBatch(req.InputFileID, req.Endpoint, window, metadata, s.callerKeyID(r))
	if err != nil {
		s.logger.Printf("batches create: store failed: %v", err)
		writeServerError(w, http.StatusInternalServerError, "could not create the batch")
		return
	}
	// Snapshot BEFORE launching the worker: the worker mutates
	// the live record, so serialising the record itself after
	// the goroutine starts would race it.
	snap := batchCopy(b)
	go s.runBatchWorker(b)
	writeJSON(w, http.StatusOK, snap)
}

// enforceBatchModelPolicy is the G21 create-time model-policy
// gate (GAP-TRACK §9, G20 Blocker B1). It returns true when the
// batch must be refused — the 403/429 already written — and the
// caller must return without creating a batch row.
//
// 1. Models: the input file is read with validateBatchLines
// (batches.go:617), the same parser the worker uses, and the
// distinct models of the VALID lines are collected. A line with
// no model cannot reach the engine: validateBatchLines requires
// a non-empty body.model (batches.go:673-676) and the worker
// executes only validated lines, so no model-less check is
// invented here. A file with no valid lines names no model, so
// it passes this gate and the worker fails it as
// invalid_input_file, exactly as before this slice.
// 2. Policy: for every distinct model, the same
// ModelAllowed/ForcedModel decision the chat path runs —
// checkKeyPolicy (admin_keys_api.go:269), not a duplicated copy
// of the logic — against a copy of the request whose path is
// "/v1/chat/completions", the endpoint every line executes as
// (validateBatchLines requires url "/v1/chat/completions"; the
// worker sets req.Endpoint to EndpointChatCompletions at
// batches.go:810). The IP and endpoint axes therefore answer
// exactly as a chat request for that line would: a key whose
// allowlist names /v1/chat/completions may batch, a key allowed
// only on another endpoint may not spend on chat through a
// batch. The whole batch is refused if ANY line names a model
// the key may not use. A caller with no config key record
// (auth-relaxed mode, a static verifier key, or no key at all)
// has no policy to enforce — the same skip checkKeyTrafficModel
// makes (keytraffic.go:351-359). The key-record lookup runs
// before the file is read, so such callers pay no parse; if the
// file cannot be opened the gate falls through and the worker
// fails the batch as input_file_unreadable, exactly as before
// this slice.
// 3. Traffic (recorded decision): a refusal consumes NO slot —
// the policy loop runs before any admission, as on the chat
// path. An admission consumes EXACTLY ONE slot, via
// checkKeyTraffic (keytraffic.go:345) with 0 prompt tokens: the
// per-line tokens are not countable at create time, the same
// honesty as checkKeyTrafficRequestsOnly (keytraffic.go:416).
// The batch path admitted no traffic at all before this slice
// (no checkKeyTraffic* call in batches_api.go/batches.go; the
// worker's engine.Chat at batches.go:812 bypasses the limiter),
// so one slot per create — one per HTTP request, as chat admits
// one per request — is the faithful chat rule. The worker's N
// line executions consume no further slots (pre-existing, not
// changed here).
//
// No defence-in-depth re-check in the worker: the create-time
// gate is the enforcement point, and the worker trusts the
// stored validated input because the input cannot change under
// it — files are immutable after Create (no update path in
// internal/files/store.go; Delete only removes, and a deleted
// input fails the batch as input_file_unreadable in the worker),
// so the models checked here are the models the worker
// executes.
func (s *Server) enforceBatchModelPolicy(w http.ResponseWriter, r *http.Request, fs *files.Store, inputFileID string) (refused bool) {
	// Key-record lookup first (G21 review Nit 1): callers with no
	// policy to enforce pay no file parse.
	keyID := s.callerKeyID(r)
	if keyID == "" || s.cfg == nil {
		return false
	}
	key, ok := s.cfg.FindGatewayKey(keyID)
	if !ok {
		return false
	}
	rc, _, err := fs.Open(inputFileID)
	if err != nil {
		// G21 review Nit 5: the file may have been deleted
		// between the caller's Get and this Open; fall through
		// and let the worker fail the batch as
		// input_file_unreadable, exactly as before this slice,
		// instead of answering 500 here.
		s.logger.Printf("batches create: input file open for policy check: %v", err)
		return false
	}
	valid, _ := validateBatchLines(rc)
	_ = rc.Close()
	if len(valid) == 0 {
		return false
	}
	seen := map[string]bool{}
	var models []string
	for _, vl := range valid {
		var req provider.ChatRequest
		if err := json.Unmarshal(vl.body, &req); err != nil {
			// Cannot happen — the line was validated — and
			// the worker records such a line as failed
			// without calling the engine, so it names no
			// model to check.
			continue
		}
		if req.Model == "" || seen[req.Model] {
			continue
		}
		seen[req.Model] = true
		models = append(models, req.Model)
	}
	// The batch route's own path (/v1/batches) is not what the
	// lines execute as; check the policy as a chat request.
	chatReq := r.Clone(r.Context())
	if r.URL != nil {
		u := *r.URL
		u.Path = "/v1/chat/completions"
		chatReq.URL = &u
	}
	for _, model := range models {
		if s.checkKeyPolicy(w, chatReq, key, model) {
			return true
		}
	}
	if s.checkKeyTraffic(w, chatReq, 0) {
		return true
	}
	return false
}

// handleBatchesList serves GET /v1/batches: the caller's
// batches, newest first. ?limit= defaults to 20, max 100 — an
// invalid limit is a 400 naming the bound (the reference router
// parseListLimit shape), never a silent clamp or a 500.
func (s *Server) handleBatchesList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit, ok := parseBatchListLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeError(w, http.StatusBadRequest, "'limit' must be an integer between 1 and 100")
		return
	}
	st, err := s.batchesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	data, total := st.list(s.callerKeyID(r), limit)
	resp := batchListResponse{Object: "list", Data: data, HasMore: total > limit}
	if len(data) == 0 {
		resp.Data = []*Batch{}
	} else {
		first, last := data[0].ID, data[len(data)-1].ID
		resp.FirstID, resp.LastID = &first, &last
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseBatchListLimit parses the ?limit= query param for
// GET /v1/batches: default 20, integer 1–100. It reports
// ok=false for anything else (non-integer, out of range).
func parseBatchListLimit(raw string) (limit int, ok bool) {
	if raw == "" {
		return 20, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 100 {
		return 0, false
	}
	return n, true
}

// handleBatchesGet serves GET /v1/batches/{id}.
func (s *Server) handleBatchesGet(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.batchesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	b := st.get(id, s.callerKeyID(r))
	if b == nil {
		writeError(w, http.StatusNotFound, "batch not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// handleBatchesCancel serves POST /v1/batches/{id}/cancel: the
// worker stops starting new lines and the batch lands on
// "cancelled" with its partial results published. Already
// terminal batches answer 409.
func (s *Server) handleBatchesCancel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.batchesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	b, res := st.cancelBatch(id, s.callerKeyID(r))
	switch res {
	case cancelNotFound:
		writeError(w, http.StatusNotFound, "batch not found: "+id)
	case cancelConflict:
		writeError(w, http.StatusConflict, "batch "+id+" is already "+string(b.Status)+"; a terminal batch cannot be cancelled")
	default:
		writeJSON(w, http.StatusOK, b)
	}
}

// handleBatchesDelete serves DELETE /v1/batches/{id}: only
// terminal batches may be deleted (409 while running); the
// batch's output/error files stay in the Files store.
func (s *Server) handleBatchesDelete(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.batchesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	ok, conflict := st.deleteBatch(id, s.callerKeyID(r))
	if conflict {
		writeError(w, http.StatusConflict, "batch "+id+" is not terminal; only a completed, cancelled, failed or expired batch can be deleted")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "batch not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, batchDeleteResponse{ID: id, Object: "batch", Deleted: true})
}

// handleBatchesDeleteCompleted serves DELETE
// /v1/batches/delete-completed: every terminal batch the
// caller may see is removed. In-progress batches are never
// touched, so a concurrent run cannot lose its record.
func (s *Server) handleBatchesDeleteCompleted(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.batchesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	ids := st.deleteCompleted(s.callerKeyID(r))
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, batchDeleteCompletedResponse{Object: "list", Data: ids, DeletedCount: len(ids)})
}
