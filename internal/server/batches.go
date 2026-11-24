// Batch API store + worker (M8.8 — POST/GET/DELETE /v1/batches,
// [id], [id]/cancel, delete-completed; Area 14 §14a "Batches"
// row, parity tracker row 1072).
//
// OpenAI-compatible Batch objects backed by a local disk index
// (<data-dir>/batches/batches.json, atomic temp+rename writes,
// same durability pattern as the Files store's index.json) plus
// in-memory worker state (the per-batch cancel func, which cannot
// survive a restart — see below). Input .jsonl files come from
// the M8.7 Files store; output/error .jsonl files are written
// back through it with the server-generated purposes
// "batch_output" / "batch_error" (files.CreateServerGenerated).
//
// Execution is REAL: every valid input line is executed as a
// chat completion through the same engine handleChatCompletions
// calls (s.engine.Chat) — no shell-out, no fabricated responses.
// Lines run sequentially (correctness over speed); a batch is
// stopped by cancelling the worker context, which the engine
// honours promptly.
//
// HONEST LIMITS (not hidden):
//   - Restart: a batch that is validating / in_progress /
//     finalizing / cancelling when the server restarts is marked
//     failed on load ("server_restart") — its worker is gone and
//     re-running its lines would double-spend. Terminal batches
//     survive intact.
//   - Input-file ownership: the Files store records no owner, so
//     a batch input file cannot be checked against the caller's
//     key (see batches_api.go). Batch RECORDS are owner-scoped
//     (OwnerKeyID, foreign ids answer 404).
//   - Pricing: lines execute through engine.Chat, so the ledger
//     records them at the SYNC rate, exactly like a sync chat
//     call. The 50% batch rate (the reference router costEstimator:
//     batchCost = syncCost * 0.5) is NOT applied to ledger rows:
//     applying it needs a batch flag threaded through
//     provider.ChatRequest → router.Attempt → attemptCost, a
//     hot-path change outside this slice. The Batch object
//     reports real token usage; no discounted cost is invented.
//   - Gates: batch lines bypass the HTTP handler's per-request
//     gates (guardrails, key budgets, traffic limits) — batch
//     create is the admission point, and per-line spend is still
//     recorded in the ledger by the engine. Per-line guardrail
//     scanning is a coordinator decision, not silently assumed.
//   - Output buffering: validated lines are held in memory until
//     executed (the input file is re-read only once, at worker
//     start); output/error lines stream to temp files on disk,
//     so a large batch's RESULTS never sit in RAM.
package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/files"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Batch statuses (OpenAI Batch object).
type batchStatus string

const (
	batchValidating batchStatus = "validating"
	batchInProgress batchStatus = "in_progress"
	batchFinalizing batchStatus = "finalizing"
	batchCompleted  batchStatus = "completed"
	batchFailed     batchStatus = "failed"
	batchExpired    batchStatus = "expired"
	batchCancelling batchStatus = "cancelling"
	batchCancelled  batchStatus = "cancelled"
)

// batchTerminal reports whether status is terminal: no further
// transition (and no worker) is possible.
func batchTerminal(s batchStatus) bool {
	switch s {
	case batchCompleted, batchFailed, batchExpired, batchCancelled:
		return true
	}
	return false
}

// batchRequestCounts is the Batch object's request_counts block.
// Total counts the VALID (executable) input lines; invalid lines
// are reported in errors.data instead and never counted here.
type batchRequestCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// batchLineError is one input-validation failure: 1-based line
// number, a short reason, and the offending field when known.
// At most 50 are reported per batch (the reference router validateJsonl
// shape); duplicate custom_ids report at most the first 10.
type batchLineError struct {
	Line   int    `json:"line_number"`
	Reason string `json:"reason"`
	Field  string `json:"field,omitempty"`
}

// batchErrors is the Batch object's errors block (OpenAI shape:
// code + message, null when none) with the per-line validation
// failures attached as data so a caller can see WHICH lines
// were skipped without a second round-trip.
type batchErrors struct {
	Code    string           `json:"code"`
	Message string           `json:"message"`
	Data    []batchLineError `json:"data,omitempty"`
}

// batchUsage is the Batch object's usage block: real summed
// token counts from the lines that actually executed. No cost
// is reported here — the ledger holds the (sync-rate) spend,
// and inventing a discounted cost would be fake data (see the
// file header).
type batchUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Batch is one batch record, in the OpenAI Batch-object shape.
// Nullable fields are pointers so they serialise as null, never
// as a missing key or a zero that would read as a real time.
// OwnerKeyID, ctx and cancel are internal and never serialised.
type Batch struct {
	ID               string             `json:"id"`
	Object           string             `json:"object"`
	Endpoint         string             `json:"endpoint"`
	Errors           *batchErrors       `json:"errors"`
	InputFileID      string             `json:"input_file_id"`
	CompletionWindow string             `json:"completion_window"`
	Status           batchStatus        `json:"status"`
	OutputFileID     *string            `json:"output_file_id"`
	ErrorFileID      *string            `json:"error_file_id"`
	CreatedAt        int64              `json:"created_at"`
	InProgressAt     *int64             `json:"in_progress_at"`
	ExpiresAt        *int64             `json:"expires_at"`
	FinalizingAt     *int64             `json:"finalizing_at"`
	CompletedAt      *int64             `json:"completed_at"`
	FailedAt         *int64             `json:"failed_at"`
	ExpiredAt        *int64             `json:"expired_at"`
	CancellingAt     *int64             `json:"cancelling_at"`
	CancelledAt      *int64             `json:"cancelled_at"`
	RequestCounts    batchRequestCounts `json:"request_counts"`
	Metadata         map[string]any     `json:"metadata"`
	Model            *string            `json:"model"`
	Usage            *batchUsage        `json:"usage"`

	OwnerKeyID string             `json:"-"`
	ctx        context.Context    `json:"-"`
	cancel     context.CancelFunc `json:"-"`
	// Seq is the store-assigned creation sequence (persisted in
	// the index file, never serialised to API callers): list
	// order is Seq desc, so "newest first" is exact even when
	// two batches share a created_at second. Sorting by
	// (created_at, id) instead was non-deterministic on ties —
	// a same-second pair could list oldest-first.
	Seq int64 `json:"-"`
}

// batchCopy returns a snapshot copy safe to serialise while the
// worker mutates the live record: the struct is copied, and the
// pointer/map fields are copied too, so no concurrent write can
// race the JSON encoder.
func batchCopy(b *Batch) *Batch {
	if b == nil {
		return nil
	}
	c := *b
	if b.OutputFileID != nil {
		v := *b.OutputFileID
		c.OutputFileID = &v
	}
	if b.ErrorFileID != nil {
		v := *b.ErrorFileID
		c.ErrorFileID = &v
	}
	c.InProgressAt = timePtrCopy(b.InProgressAt)
	c.ExpiresAt = timePtrCopy(b.ExpiresAt)
	c.FinalizingAt = timePtrCopy(b.FinalizingAt)
	c.CompletedAt = timePtrCopy(b.CompletedAt)
	c.FailedAt = timePtrCopy(b.FailedAt)
	c.ExpiredAt = timePtrCopy(b.ExpiredAt)
	c.CancellingAt = timePtrCopy(b.CancellingAt)
	c.CancelledAt = timePtrCopy(b.CancelledAt)
	if b.Errors != nil {
		e := *b.Errors
		if len(b.Errors.Data) > 0 {
			e.Data = append([]batchLineError(nil), b.Errors.Data...)
		}
		c.Errors = &e
	}
	if b.Metadata != nil {
		m := make(map[string]any, len(b.Metadata))
		for k, v := range b.Metadata {
			m[k] = v
		}
		c.Metadata = m
	}
	if b.Model != nil {
		v := *b.Model
		c.Model = &v
	}
	if b.Usage != nil {
		u := *b.Usage
		c.Usage = &u
	}
	return &c
}

func timePtrCopy(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// batchVisible reports whether callerID may see batch b.
//
// An IDENTIFIED caller (callerID != "") sees only the batches it owns:
// a batch with no owner is NOT visible to it (AUD-SEC-001 — an empty
// owner must never be a wildcard).
//
// An UNIDENTIFIED caller (callerID == "") sees only UNOWNED batches —
// the auth-relaxed trust domain that created them. It must never see a
// batch owned by a key: before this fix a caller that resolved to ""
// (which a vendor header used to cause) could list, read, cancel and
// delete every other key's batches. A foreign id still answers 404,
// never a 403 that would confirm the batch exists.
func batchVisible(b *Batch, callerID string) bool {
	if callerID == "" {
		return b.OwnerKeyID == ""
	}
	return b.OwnerKeyID == callerID
}

// batchIndexFile is the on-disk shape of batches.json: the
// batch records plus the next creation sequence number.
// batchPersisted carries one record's Seq (the API Batch
// hides it with json:"-"): the outer Seq field wins over the
// embedded Batch's hidden one, so the index round-trips the
// ordering key the API never shows.
type batchIndexFile struct {
	NextSeq int64             `json:"next_seq"`
	Batches []*batchPersisted `json:"batches"`
}

type batchPersisted struct {
	*Batch
	Seq int64 `json:"seq"`
}

// batchStore is the persisted batch index: one JSON file at
// <data-dir>/batches/batches.json, written atomically
// (temp + rename + fsync) on every mutation. All methods are
// safe for concurrent use.
type batchStore struct {
	mu      sync.Mutex
	dir     string
	byID    map[string]*Batch
	nextSeq int64
}

// batchStores caches one store per data dir, like filesStores:
// concurrent requests share the store's mutex instead of each
// opening a fresh index.
var batchStores sync.Map // string -> *batchStore

func (s *Server) batchesStore() (*batchStore, error) {
	if s.cfg == nil || s.cfg.DataDir == "" {
		return nil, errors.New("batches unavailable: no config (no data dir) configured")
	}
	dir := filepath.Join(s.cfg.DataDir, "batches")
	if st, ok := batchStores.Load(dir); ok {
		return st.(*batchStore), nil
	}
	st, err := newBatchStore(dir)
	if err != nil {
		return nil, err
	}
	actual, _ := batchStores.LoadOrStore(dir, st)
	return actual.(*batchStore), nil
}

func newBatchStore(dir string) (*batchStore, error) {
	st := &batchStore{dir: dir, byID: map[string]*Batch{}}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("batches: create store dir: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "batches.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return nil, fmt.Errorf("batches: read index: %w", err)
	}
	var idx batchIndexFile
	if err := json.Unmarshal(raw, &idx); err != nil || idx.Batches == nil {
		// Legacy fallback: a bare array of Batch objects (the
		// shape the first draft of this slice wrote). Sequence
		// numbers are rebuilt from file order (newest first).
		var legacy []*Batch
		if lerr := json.Unmarshal(raw, &legacy); lerr != nil {
			return nil, fmt.Errorf("batches: corrupt index %s: %w", filepath.Join(dir, "batches.json"), err)
		}
		idx.Batches = make([]*batchPersisted, 0, len(legacy))
		for i, b := range legacy {
			idx.Batches = append(idx.Batches, &batchPersisted{Batch: b, Seq: int64(len(legacy) - i)})
		}
		idx.NextSeq = int64(len(legacy)) + 1
	}
	now := time.Now().Unix()
	for _, pb := range idx.Batches {
		b := pb.Batch
		if b == nil || b.ID == "" {
			continue
		}
		b.Seq = pb.Seq
		if b.Seq >= st.nextSeq {
			st.nextSeq = b.Seq + 1
		}
		// A batch that was not terminal when the server stopped
		// has no worker any more: mark it failed honestly
		// instead of leaving it "in_progress" forever or
		// silently re-running lines (which would double-spend).
		// Its ctx/cancel are fresh and already cancelled, so a
		// late cancel call is a harmless no-op.
		if !batchTerminal(b.Status) {
			was := b.Status
			b.Status = batchFailed
			b.FailedAt = &now
			b.Errors = &batchErrors{Code: "server_restart", Message: "batch was " + string(was) + " when the server restarted; it was not re-run"}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // restored batches never run again
		b.ctx, b.cancel = ctx, cancel
		st.byID[b.ID] = b
	}
	if idx.NextSeq > st.nextSeq {
		st.nextSeq = idx.NextSeq
	}
	if st.nextSeq < 1 {
		st.nextSeq = 1
	}
	if err := st.saveLocked(); err != nil {
		return nil, err
	}
	return st, nil
}

// saveLocked writes the index atomically; the caller holds mu.
func (st *batchStore) saveLocked() error {
	list := make([]*Batch, 0, len(st.byID))
	for _, b := range st.byID {
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq > list[j].Seq })
	persisted := make([]*batchPersisted, 0, len(list))
	for _, b := range list {
		persisted = append(persisted, &batchPersisted{Batch: b, Seq: b.Seq})
	}
	raw, err := json.Marshal(batchIndexFile{NextSeq: st.nextSeq, Batches: persisted})
	if err != nil {
		return fmt.Errorf("batches: encode index: %w", err)
	}
	tmp, err := os.CreateTemp(st.dir, "index-*.tmp")
	if err != nil {
		return fmt.Errorf("batches: write index: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("batches: write index: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("batches: fsync index: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("batches: write index: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(st.dir, "batches.json")); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("batches: commit index: %w", err)
	}
	// Directory fsync is skipped on Windows: Go opens directories
	// without FILE_FLAG_BACKUP_SEMANTICS, and FlushFileBuffers
	// rejects such a handle with "Access is denied" (same as the
	// Files store's fsyncDir, S1-FIX-FSYNC). The index file itself
	// is still fsync'd above, which is the durability that
	// matters; the directory entry is best-effort only — the
	// rename above is already the atomicity guarantee.
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(st.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// save persists the index (locks internally).
func (st *batchStore) save() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.saveLocked()
}

// mutate applies fn to the live record under the lock and
// persists. fn returning false means "no change".
func (st *batchStore) mutate(id string, fn func(b *Batch) bool) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	b, ok := st.byID[id]
	if !ok {
		return fmt.Errorf("batches: unknown batch %q", id)
	}
	if !fn(b) {
		return nil
	}
	return st.saveLocked()
}

func newBatchID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("batches: random id: %w", err)
	}
	return "batch-" + hex.EncodeToString(b[:]), nil
}

func newBatchReqID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// createBatch records a new batch in status "validating" and
// returns it. The worker context is created here — not in the
// worker — so a cancel arriving before the worker starts still
// takes effect (the worker checks ctx first).
func (st *batchStore) createBatch(inputFileID, endpoint, window string, metadata map[string]any, owner string) (*Batch, error) {
	id, err := newBatchID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	expires := now + 24*60*60
	ctx, cancel := context.WithCancel(context.Background())
	b := &Batch{
		ID: id, Object: "batch", Endpoint: endpoint,
		InputFileID: inputFileID, CompletionWindow: window,
		Status: batchValidating, CreatedAt: now, ExpiresAt: &expires,
		Metadata: metadata, Usage: &batchUsage{},
		OwnerKeyID: owner, ctx: ctx, cancel: cancel,
	}
	st.mu.Lock()
	b.Seq = st.nextSeq
	st.nextSeq++
	st.byID[id] = b
	err = st.saveLocked()
	st.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return b, nil
}

// get returns a snapshot copy of one batch the caller may see,
// or nil when unknown / foreign (both answer 404).
func (st *batchStore) get(id, callerID string) *Batch {
	st.mu.Lock()
	defer st.mu.Unlock()
	b, ok := st.byID[id]
	if !ok || !batchVisible(b, callerID) {
		return nil
	}
	return batchCopy(b)
}

// list returns snapshot copies of the caller's batches, newest
// first, limited to limit entries, plus the total visible count.
func (st *batchStore) list(callerID string, limit int) ([]*Batch, int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	all := make([]*Batch, 0, len(st.byID))
	for _, b := range st.byID {
		if batchVisible(b, callerID) {
			all = append(all, b)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Seq > all[j].Seq })
	total := len(all)
	if limit < total {
		all = all[:limit]
	}
	out := make([]*Batch, len(all))
	for i, b := range all {
		out[i] = batchCopy(b)
	}
	return out, total
}

// cancelResult names the outcome of a cancel attempt.
type cancelResult int

const (
	cancelOK cancelResult = iota
	cancelNotFound
	cancelConflict // already terminal
)

// cancelBatch moves a batch to "cancelling" and cancels its
// worker context. The worker does the rest: it stops starting
// new lines, writes the partial output/error files, and lands
// the record on "cancelled". A batch already cancelling answers
// OK with its current state (idempotent re-cancel).
func (st *batchStore) cancelBatch(id, callerID string) (*Batch, cancelResult) {
	st.mu.Lock()
	b, ok := st.byID[id]
	if !ok || !batchVisible(b, callerID) {
		st.mu.Unlock()
		return nil, cancelNotFound
	}
	if batchTerminal(b.Status) {
		st.mu.Unlock()
		return batchCopy(b), cancelConflict
	}
	var cancel context.CancelFunc
	if b.Status != batchCancelling {
		now := time.Now().Unix()
		b.Status = batchCancelling
		b.CancellingAt = &now
		cancel = b.cancel
		_ = st.saveLocked()
	}
	out := batchCopy(b)
	st.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return out, cancelOK
}

// deleteBatch removes one TERMINAL batch record. ok=false means
// unknown/foreign (404); conflict=true means still running
// (409). Output/error files in the Files store are NOT deleted:
// they are independent file records the caller can delete via
// /v1/files.
func (st *batchStore) deleteBatch(id, callerID string) (ok, conflict bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	b, exists := st.byID[id]
	if !exists || !batchVisible(b, callerID) {
		return false, false
	}
	if !batchTerminal(b.Status) {
		return false, true
	}
	delete(st.byID, id)
	_ = st.saveLocked()
	return true, false
}

// deleteCompleted removes every TERMINAL batch the caller may
// see and returns their ids.
func (st *batchStore) deleteCompleted(callerID string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var ids []string
	for id, b := range st.byID {
		if batchTerminal(b.Status) && batchVisible(b, callerID) {
			ids = append(ids, id)
			delete(st.byID, id)
		}
	}
	if len(ids) > 0 {
		_ = st.saveLocked()
	}
	sort.Strings(ids)
	return ids
}

// ---------------------------------------------------------------------------
// Input validation.
// ---------------------------------------------------------------------------

// batchValidLine is one executable input line: its custom_id and
// the raw chat-completion request body, kept verbatim so the
// worker executes exactly what was validated.
type batchValidLine struct {
	customID string
	body     json.RawMessage
	lineNo   int
}

// batchMaxLineErrors caps the reported per-line validation
// failures at 50; batchMaxDuplicateIDs caps duplicate-custom_id
// reports at the first 10 (the reference router validateJsonl shape).
const (
	batchMaxLineErrors    = 50
	batchMaxDuplicateIDs  = 10
	batchMaxLineBytes     = 10 << 20 // 10 MiB per JSONL line
	batchProgressPersistN = 25       // persist counts every N executed lines
)

// validateBatchLines parses the input file's JSONL: each line
// must be {"custom_id", "method": "POST",
// "url": "/v1/chat/completions", "body": {chat request}} with a
// non-empty model and at least one message. Blank lines are
// skipped (a trailing newline is not an error). custom_id must
// be present, non-empty, and unique. Errors are 1-based.
func validateBatchLines(r io.Reader) (valid []batchValidLine, errs []batchLineError) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), batchMaxLineBytes)
	seen := map[string]bool{}
	dupReported := 0
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		addErr := func(reason, field string) {
			if len(errs) < batchMaxLineErrors {
				errs = append(errs, batchLineError{Line: lineNo, Reason: reason, Field: field})
			}
		}
		var raw struct {
			CustomID *string         `json:"custom_id"`
			Method   string          `json:"method"`
			URL      string          `json:"url"`
			Body     json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			addErr("line is not valid JSON: "+truncateErr(err.Error(), 120), "")
			continue
		}
		if raw.CustomID == nil || *raw.CustomID == "" {
			addErr("missing or empty \"custom_id\"", "custom_id")
			continue
		}
		if seen[*raw.CustomID] {
			if dupReported < batchMaxDuplicateIDs {
				addErr("duplicate custom_id "+quoteForError(*raw.CustomID), "custom_id")
				dupReported++
			}
			continue
		}
		seen[*raw.CustomID] = true
		if raw.Method != "POST" {
			addErr("method must be \"POST\"", "method")
			continue
		}
		if raw.URL != "/v1/chat/completions" {
			addErr("url must be \"/v1/chat/completions\"", "url")
			continue
		}
		if len(raw.Body) == 0 {
			addErr("missing \"body\"", "body")
			continue
		}
		var req provider.ChatRequest
		if err := json.Unmarshal(raw.Body, &req); err != nil {
			addErr("body is not a valid chat completion request: "+truncateErr(err.Error(), 120), "body")
			continue
		}
		if req.Model == "" {
			addErr("body.model is required", "body.model")
			continue
		}
		if len(req.Messages) == 0 {
			addErr("body.messages must not be empty", "body.messages")
			continue
		}
		valid = append(valid, batchValidLine{customID: *raw.CustomID, body: raw.Body, lineNo: lineNo})
	}
	// A scanner error (e.g. a line over 10 MiB) is a validation
	// failure of the file, not a silent truncation.
	if err := sc.Err(); err != nil {
		if len(errs) < batchMaxLineErrors {
			errs = append(errs, batchLineError{Line: lineNo + 1, Reason: "could not read input file: " + truncateErr(err.Error(), 120)})
		}
	}
	return valid, errs
}

func truncateErr(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// Worker.
// ---------------------------------------------------------------------------

// runBatchWorker executes one batch to a terminal status. It runs
// in its own goroutine; it never panics out (a panic is caught
// and recorded as a failed batch, so one bad batch cannot kill
// the gateway).
func (s *Server) runBatchWorker(b *Batch) {
	defer func() {
		if v := recover(); v != nil {
			s.logger.Printf("batch %s: worker panic: %v", b.ID, v)
			_ = s.batchesFail(b.ID, "internal_error", fmt.Sprintf("batch worker failed: %v", v))
		}
	}()
	st, err := s.batchesStore()
	if err != nil {
		_ = s.batchesFail(b.ID, "internal_error", "batch store unavailable: "+err.Error())
		return
	}
	cancelled := func() bool { return b.ctx.Err() != nil }

	// validating -> in_progress (or straight to cancelled when a
	// cancel landed before the worker started).
	wasCancelled := false
	_ = st.mutate(b.ID, func(bb *Batch) bool {
		if bb.ctx.Err() != nil {
			now := time.Now().Unix()
			bb.Status = batchCancelled
			bb.CancellingAt = &now
			bb.CancelledAt = &now
			wasCancelled = true
			return true
		}
		now := time.Now().Unix()
		bb.Status = batchInProgress
		bb.InProgressAt = &now
		return true
	})
	if wasCancelled {
		return
	}

	// Read + validate the input file.
	fs, err := s.filesStore()
	if err != nil {
		_ = s.batchesFail(b.ID, "internal_error", "files store unavailable: "+err.Error())
		return
	}
	rc, _, err := fs.Open(b.InputFileID)
	if err != nil {
		_ = s.batchesFail(b.ID, "input_file_unreadable", "could not open input file "+b.InputFileID+": "+err.Error())
		return
	}
	valid, lineErrs := validateBatchLines(rc)
	_ = rc.Close()
	if cancelled() {
		s.finishBatchAsCancelled(st, b, "", "", batchRequestCounts{Total: len(valid)}, batchUsage{}, lineErrs)
		return
	}
	if len(valid) == 0 {
		msg := "input file has no valid lines"
		if len(lineErrs) > 0 {
			msg += fmt.Sprintf(" (%d line error(s); first: line %d: %s)", len(lineErrs), lineErrs[0].Line, lineErrs[0].Reason)
		}
		_ = st.mutate(b.ID, func(bb *Batch) bool {
			now := time.Now().Unix()
			bb.Status = batchFailed
			bb.FailedAt = &now
			bb.Errors = &batchErrors{Code: "invalid_input_file", Message: msg, Data: lineErrs}
			return true
		})
		return
	}

	// Execute sequentially; results stream to temp files on disk.
	outTmp, err := os.CreateTemp(st.dir, "batch-out-*.jsonl")
	if err != nil {
		_ = s.batchesFail(b.ID, "internal_error", "could not stage output file: "+err.Error())
		return
	}
	errTmp, err := os.CreateTemp(st.dir, "batch-err-*.jsonl")
	if err != nil {
		_ = outTmp.Close()
		_ = os.Remove(outTmp.Name())
		_ = s.batchesFail(b.ID, "internal_error", "could not stage error file: "+err.Error())
		return
	}
	outW := bufio.NewWriter(outTmp)
	errW := bufio.NewWriter(errTmp)
	counts := batchRequestCounts{Total: len(valid)}
	usage := batchUsage{}
	var model *string
	executed := 0
	for _, vl := range valid {
		if cancelled() {
			break
		}
		var req provider.ChatRequest
		if err := json.Unmarshal(vl.body, &req); err != nil {
			// Cannot happen — the line was validated — but a
			// worker that silently skipped it would lie about
			// counts, so record it as a failed line instead.
			s.writeBatchErrorLine(errW, vl.customID, "invalid_request", "body could not be decoded")
			counts.Failed++
			executed++
			continue
		}
		req.Stream = false
		req.Endpoint = provider.EndpointChatCompletions
		req.CallerID = b.OwnerKeyID
		// P1.6 org/team policy, per item: the batch owner's key is
		// resolved to its team exactly as an interactive request is,
		// so a batch cannot smuggle a model the team is not allowed
		// to call. A denied item is reported as its own error line —
		// the rest of the batch still runs.
		if ref := s.evaluateTeamPolicy(b.ctx, b.OwnerKeyID, req.Model, routingStrategy(&req)); ref != nil {
			s.writeBatchErrorLine(errW, vl.customID, "policy_denied", truncateErr(ref.Message, 200))
			counts.Failed++
			executed++
			continue
		}
		resp, err := s.engine.Chat(b.ctx, req)
		if err != nil {
			if cancelled() {
				break
			}
			s.writeBatchErrorLine(errW, vl.customID, "internal_error", truncateErr(err.Error(), 200))
			counts.Failed++
			executed++
			continue
		}
		if model == nil {
			m := req.Model
			if resp.Model != "" {
				m = resp.Model
			}
			model = &m
		}
		s.writeBatchOutputLine(outW, vl.customID, req, resp)
		counts.Completed++
		usage.PromptTokens += resp.Usage.PromptTokens
		usage.CompletionTokens += resp.Usage.CompletionTokens
		usage.TotalTokens += resp.Usage.TotalTokens
		executed++
		if executed%batchProgressPersistN == 0 {
			c, u, m := counts, usage, model
			_ = st.mutate(b.ID, func(bb *Batch) bool {
				bb.RequestCounts = c
				bb.Usage = &batchUsage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
				if m != nil {
					mv := *m
					bb.Model = &mv
				}
				return true
			})
		}
	}
	_ = outW.Flush()
	_ = errW.Flush()
	_ = outTmp.Close()
	_ = errTmp.Close()
	if cancelled() {
		s.finishBatchAsCancelled(st, b, outTmp.Name(), errTmp.Name(), counts, usage, lineErrs)
		_ = os.Remove(outTmp.Name())
		_ = os.Remove(errTmp.Name())
		return
	}

	// finalizing: publish the output/error files, then land.
	_ = st.mutate(b.ID, func(bb *Batch) bool {
		now := time.Now().Unix()
		bb.Status = batchFinalizing
		bb.FinalizingAt = &now
		bb.RequestCounts = counts
		bb.Usage = &usage
		bb.Model = model
		return true
	})
	var outID, errID *string
	if counts.Completed > 0 {
		if f, ferr := s.storeBatchResultFile(fs, outTmp.Name(), "batch-"+b.ID+"-output.jsonl", "batch_output"); ferr != nil {
			s.logger.Printf("batch %s: output file store failed: %v", b.ID, ferr)
		} else {
			outID = &f.ID
		}
	}
	if counts.Failed > 0 {
		if f, ferr := s.storeBatchResultFile(fs, errTmp.Name(), "batch-"+b.ID+"-error.jsonl", "batch_error"); ferr != nil {
			s.logger.Printf("batch %s: error file store failed: %v", b.ID, ferr)
		} else {
			errID = &f.ID
		}
	}
	_ = os.Remove(outTmp.Name())
	_ = os.Remove(errTmp.Name())
	now := time.Now().Unix()
	_ = st.mutate(b.ID, func(bb *Batch) bool {
		bb.RequestCounts = counts
		bb.Usage = &usage
		bb.Model = model
		bb.OutputFileID = outID
		bb.ErrorFileID = errID
		if len(lineErrs) > 0 {
			bb.Errors = &batchErrors{
				Code:    "validation_failed",
				Message: fmt.Sprintf("%d input line(s) skipped: first error line %d: %s", len(lineErrs), lineErrs[0].Line, lineErrs[0].Reason),
				Data:    lineErrs,
			}
		}
		if bb.ExpiresAt != nil && now > *bb.ExpiresAt {
			bb.Status = batchExpired
			bb.ExpiredAt = &now
		} else {
			bb.Status = batchCompleted
			bb.CompletedAt = &now
		}
		return true
	})
}

// finishBatchAsCancelled lands a cancelled batch: partial
// output/error files are still published (the lines that did
// run produced real results), counts reflect what executed.
func (s *Server) finishBatchAsCancelled(st *batchStore, b *Batch, outPath, errPath string, counts batchRequestCounts, usage batchUsage, lineErrs []batchLineError) {
	var outID, errID *string
	if outPath != "" && counts.Completed > 0 {
		if fs, err := s.filesStore(); err == nil {
			if f, ferr := s.storeBatchResultFile(fs, outPath, "batch-"+b.ID+"-output.jsonl", "batch_output"); ferr == nil {
				outID = &f.ID
			}
		}
	}
	if errPath != "" && counts.Failed > 0 {
		if fs, err := s.filesStore(); err == nil {
			if f, ferr := s.storeBatchResultFile(fs, errPath, "batch-"+b.ID+"-error.jsonl", "batch_error"); ferr == nil {
				errID = &f.ID
			}
		}
	}
	now := time.Now().Unix()
	_ = st.mutate(b.ID, func(bb *Batch) bool {
		bb.Status = batchCancelled
		bb.CancelledAt = &now
		if bb.CancellingAt == nil {
			bb.CancellingAt = &now
		}
		bb.RequestCounts = counts
		bb.Usage = &usage
		bb.OutputFileID = outID
		bb.ErrorFileID = errID
		if len(lineErrs) > 0 {
			bb.Errors = &batchErrors{
				Code:    "validation_failed",
				Message: fmt.Sprintf("%d input line(s) skipped: first error line %d: %s", len(lineErrs), lineErrs[0].Line, lineErrs[0].Reason),
				Data:    lineErrs,
			}
		}
		return true
	})
}

// batchesFail lands a batch on "failed" with an errors block.
func (s *Server) batchesFail(id, code, msg string) error {
	st, err := s.batchesStore()
	if err != nil {
		return err
	}
	return st.mutate(id, func(bb *Batch) bool {
		if batchTerminal(bb.Status) {
			return false
		}
		now := time.Now().Unix()
		bb.Status = batchFailed
		bb.FailedAt = &now
		bb.Errors = &batchErrors{Code: code, Message: msg}
		return true
	})
}

// storeBatchResultFile stores a finished temp .jsonl file in the
// Files store under a server-generated purpose.
func (s *Server) storeBatchResultFile(fs *files.Store, path, filename, purpose string) (*files.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return fs.CreateServerGenerated(filename, purpose, f)
}

// writeBatchOutputLine appends one OpenAI-shaped output JSONL
// line: {"id", "custom_id", "response": {"status_code": 200,
// "request_id", "body": {chat completion}}}.
func (s *Server) writeBatchOutputLine(w *bufio.Writer, customID string, req provider.ChatRequest, resp *provider.ChatResponse) {
	line := map[string]any{
		"id":        newBatchReqID("batch_req_"),
		"custom_id": customID,
		"response": map[string]any{
			"status_code": 200,
			"request_id":  newBatchReqID("req_"),
			"body":        batchChatCompletionBody(req, resp),
		},
	}
	raw, _ := json.Marshal(line)
	_, _ = w.Write(raw)
	_ = w.WriteByte('\n')
}

// writeBatchErrorLine appends one OpenAI-shaped error JSONL line:
// {"id", "custom_id", "error": {"code", "message"}}.
func (s *Server) writeBatchErrorLine(w *bufio.Writer, customID, code, msg string) {
	line := map[string]any{
		"id":        newBatchReqID("batch_req_"),
		"custom_id": customID,
		"error":     map[string]any{"code": code, "message": msg},
	}
	raw, _ := json.Marshal(line)
	_, _ = w.Write(raw)
	_ = w.WriteByte('\n')
}

// batchChatCompletionBody renders a provider.ChatResponse as an
// OpenAI chat-completion object for the output file.
func batchChatCompletionBody(req provider.ChatRequest, resp *provider.ChatResponse) map[string]any {
	model := resp.Model
	if model == "" {
		model = req.Model
	}
	fr := resp.FinishReason
	if fr == "" {
		fr = "stop"
	}
	msg := map[string]any{"role": "assistant", "content": resp.Content}
	if len(resp.ToolCalls) > 0 {
		msg["tool_calls"] = resp.ToolCalls
	}
	return map[string]any{
		"id":      newBatchReqID("chatcmpl-"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": fr,
		}},
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		},
	}
}
