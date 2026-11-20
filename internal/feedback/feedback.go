// Package feedback is the Phase 2 prototype for routing learned from
// user feedback (research-log entry 153, Aiport): it records, per
// routing decision, only the outcome signal — accept / edit / discard
// / regenerate — and turns accumulated signals into small bounded
// per-tier weight nudges for the auto-router.
//
// PRIVACY RULE — NO CONTENT, EVER. This store records metadata only:
// a decision id, the tier/model chosen, the decision latency, and
// the signal. It NEVER stores prompts, answers, or any message
// content: there is no column for them, no field for them, and no
// code path that accepts them. A decision is registered by the
// server when it serves a real auto-routed /v1 request (tier, model
// and latency only); the dashboard preview classifier never
// registers its decisions, so feedback can only ever attach to a
// decision the router actually served. The test
// TestSchemaStoresNoContent asserts the rule at the schema level.
//
// NUDGE RULE — CALIBRATED HEURISTIC (see CalibratedNudge). This is
// NOT reinforcement learning and must never be described as such:
// it is a bounded, rate-based adjustment of the tier's score from
// accumulated outcome signals, with no model training, no gradient
// and no exploration. Per tier, each signal is worth points
// (accept +1, edit -0.5, discard -1, regenerate -1) and is
// time-decayed (see DecayHalfLife): the nudge is the tier's
// decayed net-points RATE — decayed net points divided by decayed
// (effective) samples — times MaxNudge (0.05), clamped to
// +/-MaxNudge. A tier gets NO nudge at all until it has MinSamples
// (10) EFFECTIVE samples: effective samples are the decay-weighted
// count, always <= the raw count, so the floor is at least as
// strict as the old raw-count floor and a handful of signals — or
// a pile of stale ones — can never flip routing. The nudge is
// added to the tier's raw cosine score inside
// autorouter.Classify (clamped to [0, 1]), so at most it moves a
// genuinely borderline decision across the threshold; it can never
// promote a tier that scored far below it. Learning is behind the
// config flag auto_router.feedback_learned_weights (default OFF):
// with the flag off the store still records signals, but
// LearnedWeights returns all-zero nudges, so routing is exactly
// unchanged. The exact formula, the decay choice and the reasons
// for keeping the floor and the clamp are documented in
// docs/feedback-calibration.md.
//
// A decision has exactly ONE signal: the first POST wins, a repeat
// of the same signal is an idempotent no-op, and a different signal
// for the same decision is a conflict (409). A regenerate is
// reported by the client against the ORIGINAL decision id when the
// user asks for a new answer — the server cannot observe a
// client-side regenerate click, so there is no server-side
// derivation; a client that never posts simply contributes no
// signal. That is an honest limitation, not a silent inference.
package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/learned"

	_ "modernc.org/sqlite"
)

// Signal is one user outcome for a routing decision.
type Signal string

// Signals.
const (
	// SignalAccept means the user kept the response as-is.
	SignalAccept Signal = "accept"
	// SignalEdit means the user kept the response but edited it:
	// usable, but the tier's answer needed fixing (mildly negative).
	SignalEdit Signal = "edit"
	// SignalDiscard means the user discarded the response.
	SignalDiscard Signal = "discard"
	// SignalRegenerate means the user asked for a new answer
	// instead of the one this decision produced.
	SignalRegenerate Signal = "regenerate"
)

// AllSignals lists every valid signal in a stable order.
var AllSignals = []Signal{SignalAccept, SignalEdit, SignalDiscard, SignalRegenerate}

// ParseSignal validates s: "" and anything outside the four signals
// is rejected (never silently mapped to a default).
func ParseSignal(s string) (Signal, error) {
	switch Signal(strings.ToLower(strings.TrimSpace(s))) {
	case SignalAccept:
		return SignalAccept, nil
	case SignalEdit:
		return SignalEdit, nil
	case SignalDiscard:
		return SignalDiscard, nil
	case SignalRegenerate:
		return SignalRegenerate, nil
	default:
		return "", fmt.Errorf("feedback: signal must be one of accept, edit, discard, regenerate (got %q)", s)
	}
}

// IsSignal reports whether s names a valid signal.
func IsSignal(s string) bool {
	_, err := ParseSignal(s)
	return err == nil
}

// ---------------------------------------------------------------------------
// Nudge rule — pure, unit-testable, no I/O.
// ---------------------------------------------------------------------------

// MinSamples is the number of EFFECTIVE (decay-weighted) samples a
// tier needs before ANY nudge applies: a single signal, or a
// handful, must never move routing — and neither may a pile of
// stale signals whose decayed weight has fallen below the floor.
const MinSamples = 10

// Step is retained for API compatibility (Summary.Step) only: the
// old fixed rule (net-points * Step) no longer drives the nudge.
// The calibrated rule (see CalibratedNudge) uses the tier's
// net-points rate times MaxNudge instead. Do not reintroduce Step
// into the nudge computation without updating
// docs/feedback-calibration.md.
const Step = 0.005

// DecayHalfLife is the time-decay half-life for feedback signals:
// a signal's influence halves every 7 days. Rationale (see
// docs/feedback-calibration.md): signals are sparse per tier in
// this prototype, so a 1-day half-life would erase evidence before
// 10 effective samples ever accumulate, while a 30-day half-life
// would let a tier's behaviour from a month ago dominate routing
// long after it changed. At 7 days a signal is at half strength
// after a week, ~6% after a month (4 half-lives) and effectively
// forgotten after ~30 days. Decay is computed at READ time from
// the stored signal timestamp — no background job, no deletion:
// old rows stay in feedback.db (auditable) but contribute ~0
// weight, and a tier whose evidence has all decayed falls back
// below the sample floor and loses its nudge.
const DecayHalfLife = 7 * 24 * time.Hour

// DecayWeight is the influence weight of a signal of the given age:
// 2^(-age / DecayHalfLife). A fresh signal weighs 1, one
// half-life old weighs 0.5, two half-lives 0.25, and so on.
// A non-positive age (clock skew, same-millisecond signal)
// weighs 1 — decay never amplifies a signal.
func DecayWeight(age time.Duration) float64 {
	if age <= 0 {
		return 1
	}
	return math.Pow(0.5, float64(age)/float64(DecayHalfLife))
}

// TimedSignal is one signal for calibration: its point value
// (see SignalPoints) and its age at computation time. It carries
// no content — only the point value derived from the signal enum
// and how old the signal is.
type TimedSignal struct {
	Points float64
	Age    time.Duration
}

// CalibratedNudge computes a tier's score nudge from its timed
// signals. Formula:
//
//	effective samples  N = sum over signals of DecayWeight(age)
//	decayed net points P = sum over signals of points * DecayWeight(age)
//	rate               r = P / N            (in [-1, +1])
//	nudge                  = r * MaxNudge, clamped to +/-MaxNudge
//
// The nudge is 0 and active=false while N < MinSamples. The rate
// form is the calibration: the nudge now reflects HOW GOOD the
// tier has been (its observed accept rate), not how much traffic
// it got — under the old fixed rule, net-points * Step, a tier
// with 10 accepts already saturated the cap and volume alone
// pushed the nudge, so a heavily-used mediocre tier could out-nudge
// a rarely-used excellent one. Here 10/10 accepts still gives
// +0.05 and 10/10 discards -0.05, but 6 accepts + 4 discards gives
// rate 0.2 -> +0.01 (not volume-driven), and 10 accepts + 10
// discards gives rate 0 -> exactly 0 (active, but neutral). The
// clamp is now a safety net (|r| <= 1 already bounds the nudge)
// rather than the main limiter; it is kept so a future formula
// change cannot silently widen the blast radius. It also returns
// N so callers (Summary) can report the effective sample count.
//
// This is a calibrated heuristic, not RL: no training, no
// gradient, no exploration — a bounded rate adjustment.
func CalibratedNudge(signals []TimedSignal) (nudge float64, active bool, effSamples float64) {
	var net float64
	for _, s := range signals {
		w := DecayWeight(s.Age)
		effSamples += w
		net += s.Points * w
	}
	// Floor comparison with a 1e-3 tolerance: decay over the
	// recording window must not keep a tier with 10 genuinely
	// fresh signals below the floor. Each signal's decay weight
	// is 0.5^(age/7d), so a signal recorded ~1 s before the
	// computation is worth ~1 - 1.2e-6, not exactly 1 — and on a
	// loaded machine recording 10 signals can take ~1-2 s, so
	// the 10-signal sum lands at 10 - ~4e-6, not 10 - ~1e-8 as
	// the old 1e-6 tolerance assumed (that tolerance failed
	// TestLearnedWeightsDisabledByDefault under load: eff
	// 9.999996 < 10 - 1e-6). 1e-3 still cannot activate a tier
	// that is meaningfully short of evidence — 9 signals sum to
	// 9.0, far below 10 - 1e-3 — and it covers a recording
	// window of ~60 s (deficit ~7e-4); it only absorbs decay
	// noise, never a missing signal.
	if effSamples+1e-3 < MinSamples {
		return 0, false, effSamples
	}
	nudge = net / effSamples * MaxNudge
	if nudge > MaxNudge {
		nudge = MaxNudge
	} else if nudge < -MaxNudge {
		nudge = -MaxNudge
	}
	return nudge, true, effSamples
}

// MaxNudge bounds the nudge in either direction: a tier's score
// moves by at most 0.05, so only borderline decisions can flip.
const MaxNudge = 0.05

// SignalPoints is the point value of one signal: an accept rewards
// the tier, a discard or regenerate penalises it, an edit is a
// half-penalty (the answer was usable but needed fixing).
var SignalPoints = map[Signal]float64{
	SignalAccept:     +1,
	SignalEdit:       -0.5,
	SignalDiscard:    -1,
	SignalRegenerate: -1,
}

// SignalCounts is the per-signal tally for one tier.
type SignalCounts struct {
	Accept     int `json:"accept"`
	Edit       int `json:"edit"`
	Discard    int `json:"discard"`
	Regenerate int `json:"regenerate"`
}

// Samples is the total number of signals in the tally.
func (c SignalCounts) Samples() int {
	return c.Accept + c.Edit + c.Discard + c.Regenerate
}

// NetPoints is the tally's point sum: accepts minus (edits at half
// weight, discards and regenerates at full weight).
func (c SignalCounts) NetPoints() float64 {
	return float64(c.Accept)*SignalPoints[SignalAccept] +
		float64(c.Edit)*SignalPoints[SignalEdit] +
		float64(c.Discard)*SignalPoints[SignalDiscard] +
		float64(c.Regenerate)*SignalPoints[SignalRegenerate]
}

// NudgeFor computes the tier's score nudge from its tally: 0 and
// active=false below MinSamples; otherwise the calibrated rate
// (see CalibratedNudge) — net-points rate times MaxNudge, clamped
// to +/-MaxNudge. A tally has no timestamps, so this is the
// no-decay special case: every signal is treated as fresh (age 0,
// decay weight 1). The store path always uses CalibratedNudge
// with real ages instead.
func NudgeFor(c SignalCounts) (nudge float64, active bool) {
	signals := make([]TimedSignal, 0, c.Samples())
	add := func(n int, sig Signal) {
		for i := 0; i < n; i++ {
			signals = append(signals, TimedSignal{Points: SignalPoints[sig]})
		}
	}
	add(c.Accept, SignalAccept)
	add(c.Edit, SignalEdit)
	add(c.Discard, SignalDiscard)
	add(c.Regenerate, SignalRegenerate)
	nudge, active, _ = CalibratedNudge(signals)
	return nudge, active
}

// ---------------------------------------------------------------------------
// Store.
// ---------------------------------------------------------------------------

// NO-CONTENT RULE (schema): feedback_decisions carries only a
// decision id, tier, model, decision latency and the raw per-tier
// cosine scores that produced the tier choice; feedback_events
// carries only a decision id and a signal. The scores are numbers
// (metadata, like the latency) — they are what makes the A/B
// evaluation possible without ever storing a prompt: a recorded
// decision's scores say exactly which tier each policy would have
// picked. There is deliberately NO prompt / answer / message /
// content column — a future column addition that stores content
// would fail TestSchemaStoresNoContent.
const schema = `
CREATE TABLE IF NOT EXISTS feedback_decisions (
  decision_id         TEXT PRIMARY KEY,
  ts                  INTEGER NOT NULL,  -- unix milliseconds (UTC)
  tier                TEXT NOT NULL,
  model               TEXT NOT NULL,
  decision_latency_us INTEGER NOT NULL DEFAULT 0,
  tier_scores_json    TEXT NOT NULL DEFAULT ''  -- {"quick-qa":0.42,...} raw cosine scores, metadata only
);
CREATE TABLE IF NOT EXISTS feedback_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  ts          INTEGER NOT NULL,  -- unix milliseconds (UTC)
  decision_id TEXT NOT NULL UNIQUE REFERENCES feedback_decisions(decision_id),
  signal      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_feedback_events_signal
  ON feedback_events(signal);
`

// DecisionRef is the metadata of one served routing decision, as
// the server registers it. It carries no prompt and no answer.
// TierScores is the raw per-tier cosine score map from
// autorouter.Classify (metadata only — numbers, no content): the
// A/B evaluation replays these scores under both policies, so a
// decision recorded without scores can never be evaluated (it is
// excluded, never guessed). Nil/empty is allowed for callers that
// have no scores (old code paths); the server always passes them.
type DecisionRef struct {
	ID            string
	Tier          string
	Model         string
	LatencyMicros int64
	TierScores    map[string]float64
}

// Event is one recorded feedback signal.
type Event struct {
	ID         int64     `json:"id"`
	TS         time.Time `json:"ts"`
	DecisionID string    `json:"decision_id"`
	Tier       string    `json:"tier"`
	Model      string    `json:"model"`
	Signal     Signal    `json:"signal"`
}

// TierSummary is one tier's learned state for the dashboard: its
// signal tally, sample count, effective (decay-weighted) sample
// count, effective nudge (0 when learning is off or the tier is
// below the sample floor), and whether the nudge is currently
// active.
type TierSummary struct {
	Tier             string       `json:"tier"`
	Counts           SignalCounts `json:"signals"`
	Samples          int          `json:"samples"`
	EffectiveSamples float64      `json:"effective_samples"`
	Weight           float64      `json:"weight"`
	Active           bool         `json:"active"`
	MinSamples       int          `json:"min_samples"`
}

// Summary is the GET /dashboard/feedback payload: the learning
// flag, the nudge rule parameters, totals, one row per tier (all
// four tiers always present, zero-filled when untouched), the most
// recent signals (decision id, tier, signal — never content), and
// the A/B evaluation of the heuristic against the no-feedback
// baseline on the recorded traffic (nil when the evaluation
// cannot run — its own Verdict then says why).
type Summary struct {
	Enabled      bool          `json:"enabled"`
	MinSamples   int           `json:"min_samples"`
	Step         float64       `json:"step"`
	MaxNudge     float64       `json:"max_nudge"`
	HalfLifeDays float64       `json:"half_life_days"`
	Total        int           `json:"total_signals"`
	Tiers        []TierSummary `json:"tiers"`
	Recent       []Event       `json:"recent"`
	Eval         *EvalResult   `json:"eval,omitempty"`
}

// ErrUnknownDecision is returned when a signal names a decision id
// the store never registered (e.g. a dashboard preview decision,
// which is classified but never served and so never registered).
var ErrUnknownDecision = errors.New("feedback: unknown decision id")

// ErrSignalConflict is returned when a decision already has a
// different signal recorded: one decision, one outcome.
var ErrSignalConflict = errors.New("feedback: decision already has a different signal recorded")

// Store is the feedback signal store backed by a local SQLite file.
type Store struct {
	db   *sql.DB
	path string

	enabled bool
	mu      sync.Mutex // guards enabled

	// now is the clock used for decision/signal timestamps and for
	// decay ages. It defaults to time.Now; tests override it to
	// place signals at fixed ages and verify decay.
	now func() time.Time

	// weightsCache is the last computed LearnedWeights result, so a
	// Classify on every auto-routed request does not hit SQLite.
	// TTL 5s; RecordSignal invalidates it immediately.
	cacheMu     sync.Mutex
	cachedAt    time.Time
	cached      map[autorouter.Tier]autorouter.TierWeight
	cacheFilled bool
}

// weightsCacheTTL bounds how stale a routing decision's learned
// weights can be: at most 5 s after a new signal lands (and 0 s
// when RecordSignal just invalidated the cache).
const weightsCacheTTL = 5 * time.Second

// Open opens (creating if needed) the SQLite feedback store at
// path, applies the schema, and returns the Store. Parent
// directories are created. Learning starts OFF: the caller sets
// it from config via SetEnabled.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("feedback: create directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("feedback: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("feedback: enable WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("feedback: busy timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("feedback: apply schema: %w", err)
	}
	// Migration: stores created before tier_scores_json existed
	// gain the column (empty = unscored, pre-A/B decisions).
	// CREATE TABLE IF NOT EXISTS never adds a column to an
	// existing table, so this is the only way old feedback.db
	// files pick it up. Idempotent: the column check makes a
	// second open a no-op.
	if err := migrateTierScores(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("feedback: migrate tier scores: %w", err)
	}
	return &Store{db: db, path: path, now: time.Now}, nil
}

// migrateTierScores adds feedback_decisions.tier_scores_json to
// stores created before the A/B evaluation slice (item 115).
// Rows written before the migration keep ” (no scores) and are
// excluded from the evaluation, never guessed.
func migrateTierScores(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('feedback_decisions') WHERE name = 'tier_scores_json'`)
	if err != nil {
		return err
	}
	has := rows.Next()
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE feedback_decisions ADD COLUMN tier_scores_json TEXT NOT NULL DEFAULT ''`)
	return err
}

// ClearAll deletes every recorded decision and every recorded
// signal and invalidates the learned-weights cache, so a later
// Summary/LearnedWeights call sees an empty store. The tables
// (and the database file) stay in place, empty, and the
// learning-enabled flag is untouched — clearing history is not
// turning learning on or off. There is no undo: callers must gate
// this behind the operator-credential boundary (POST
// /admin/db/wipe) and say so in the response. No prompt, answer or
// message content is stored in either table (see the schema
// comment), so there is nothing content-bearing to delete — the
// counts are decision/signal metadata only.
func (s *Store) ClearAll(ctx context.Context) (decisions, signals int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("feedback: clear begin tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM feedback_decisions`)
	if err != nil {
		return 0, 0, fmt.Errorf("feedback: clear decisions: %w", err)
	}
	if decisions, err = res.RowsAffected(); err != nil {
		return 0, 0, fmt.Errorf("feedback: clear decisions count: %w", err)
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM feedback_events`)
	if err != nil {
		return 0, 0, fmt.Errorf("feedback: clear signals: %w", err)
	}
	if signals, err = res.RowsAffected(); err != nil {
		return 0, 0, fmt.Errorf("feedback: clear signals count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("feedback: clear commit: %w", err)
	}
	s.invalidateCache()
	return decisions, signals, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the SQLite file path backing this store.
func (s *Store) Path() string { return s.path }

// SetEnabled turns learned-weight application on or off. Recording
// is unaffected: signals are always stored, only their effect on
// routing is gated — so turning learning on later applies the
// signals accumulated while it was off.
func (s *Store) SetEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = enabled
	s.invalidateCache()
}

// Enabled reports whether learned weights currently apply to routing.
func (s *Store) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

func (s *Store) invalidateCache() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.cacheFilled = false
	s.cached = nil
}

// marshalTierScores encodes a raw per-tier score map as JSON for
// the tier_scores_json column. Empty input encodes as "" (no
// scores). Scores must be finite numbers in [-1, 1] (raw cosine
// similarity); anything else is a programming error and is
// rejected rather than stored. Keys are sorted so the stored JSON
// is deterministic.
func marshalTierScores(scores map[string]float64) (string, error) {
	if len(scores) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(scores))
	for k, v := range scores {
		if !autorouter.IsTier(k) {
			return "", fmt.Errorf("feedback: tier score key %q is not a known tier", k)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < -1 || v > 1 {
			return "", fmt.Errorf("feedback: tier score for %q must be a finite number in [-1, 1] (got %v)", k, v)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]float64, len(scores))
	for _, k := range keys {
		ordered[k] = scores[k]
	}
	raw, err := json.Marshal(ordered)
	if err != nil {
		return "", fmt.Errorf("feedback: encode tier scores: %w", err)
	}
	return string(raw), nil
}

// parseTierScores decodes a tier_scores_json value. "" (no scores)
// and malformed JSON both decode as nil — a corrupt score blob
// excludes the decision from the evaluation instead of failing it.
func parseTierScores(raw string) map[string]float64 {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var scores map[string]float64
	if err := json.Unmarshal([]byte(raw), &scores); err != nil || len(scores) == 0 {
		return nil
	}
	for k, v := range scores {
		if !autorouter.IsTier(k) || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
	}
	return scores
}

// RegisterDecision records that a routing decision was served (so a
// later signal can attach to it). It is idempotent: re-registering
// the same decision id is a no-op that keeps the first row, so a
// decision observed twice (e.g. once for the response header and
// once for the dashboard "last decision" read) can never change
// the stored tier/model. decision_id, tier and model are validated
// (non-empty, bounded length); anything else is a programming
// error, not a signal, and is rejected.
func (s *Store) RegisterDecision(ctx context.Context, d DecisionRef) error {
	if strings.TrimSpace(d.ID) == "" || len(d.ID) > 128 {
		return fmt.Errorf("feedback: decision id must be non-empty and at most 128 characters")
	}
	if strings.TrimSpace(d.Tier) == "" || !autorouter.IsTier(d.Tier) {
		return fmt.Errorf("feedback: tier %q is not a known tier", d.Tier)
	}
	if len(d.Model) > 256 {
		return fmt.Errorf("feedback: model must be at most 256 characters")
	}
	if d.LatencyMicros < 0 {
		return fmt.Errorf("feedback: decision latency must not be negative")
	}
	scoresJSON, err := marshalTierScores(d.TierScores)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO feedback_decisions
		  (decision_id, ts, tier, model, decision_latency_us, tier_scores_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		d.ID, s.now().UnixMilli(), d.Tier, d.Model, d.LatencyMicros, scoresJSON)
	if err != nil {
		return fmt.Errorf("feedback: register decision: %w", err)
	}
	return nil
}

// RecordSignal records signal for decisionID. It validates the
// signal and that the decision was registered (unknown ids are
// rejected — a client cannot invent routing facts by posting an
// arbitrary id with a tier of its choosing; the tier/model always
// come from the registered decision row). One decision, one signal:
// repeating the SAME signal returns the stored event with
// duplicate=true (idempotent retry); a DIFFERENT signal for the
// same decision returns ErrSignalConflict.
func (s *Store) RecordSignal(ctx context.Context, decisionID string, signal Signal) (event Event, duplicate bool, err error) {
	if _, err := ParseSignal(string(signal)); err != nil {
		return Event{}, false, err
	}
	if strings.TrimSpace(decisionID) == "" || len(decisionID) > 128 {
		return Event{}, false, fmt.Errorf("feedback: decision id must be non-empty and at most 128 characters")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, false, fmt.Errorf("feedback: begin tx: %w", err)
	}
	defer tx.Rollback()
	var tier, model string
	err = tx.QueryRowContext(ctx, `
		SELECT tier, model FROM feedback_decisions WHERE decision_id = ?`,
		decisionID).Scan(&tier, &model)
	if err != nil {
		if err == sql.ErrNoRows {
			return Event{}, false, ErrUnknownDecision
		}
		return Event{}, false, fmt.Errorf("feedback: look up decision: %w", err)
	}
	var existingID int64
	var existingTS int64
	var existingSignal string
	err = tx.QueryRowContext(ctx, `
		SELECT id, ts, signal FROM feedback_events WHERE decision_id = ?`,
		decisionID).Scan(&existingID, &existingTS, &existingSignal)
	switch {
	case err == nil:
		if existingSignal == string(signal) {
			// Idempotent retry: the same signal for the same
			// decision is already recorded — return it, record
			// nothing new.
			return Event{
				ID: existingID, TS: time.UnixMilli(existingTS).UTC(),
				DecisionID: decisionID, Tier: tier, Model: model,
				Signal: signal,
			}, true, nil
		}
		return Event{}, false, ErrSignalConflict
	case err != sql.ErrNoRows:
		return Event{}, false, fmt.Errorf("feedback: look up existing signal: %w", err)
	}
	now := s.now().UnixMilli()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO feedback_events (ts, decision_id, signal)
		VALUES (?, ?, ?)`, now, decisionID, string(signal))
	if err != nil {
		return Event{}, false, fmt.Errorf("feedback: insert signal: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Event{}, false, fmt.Errorf("feedback: signal id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, false, fmt.Errorf("feedback: commit: %w", err)
	}
	s.invalidateCache()
	return Event{
		ID: id, TS: time.UnixMilli(now).UTC(),
		DecisionID: decisionID, Tier: tier, Model: model, Signal: signal,
	}, false, nil
}

// LabelForSignal maps a recorded outcome signal to the learned
// router's win label: accept = 1 (kept as-is), edit = 0.5
// (usable but needed fixing), discard / regenerate = 0 (the
// tier's answer was rejected). This is the same point scale as
// SignalPoints, rescaled to [0, 1] so a logistic model can treat
// it as a win probability target.
func LabelForSignal(sig Signal) float64 {
	switch sig {
	case SignalAccept:
		return 1
	case SignalEdit:
		return 0.5
	default: // discard, regenerate
		return 0
	}
}

// TrainingPairs returns every labelled training example the
// learned router can learn from: one per recorded signal, in
// outcome order (oldest first — the learned model splits them
// 70/30 by time itself). Each pair carries only the decision's
// recorded per-tier heuristic scores (metadata numbers, no
// prompts or answers), the tier actually served, and the win
// label of the recorded signal. Decisions recorded without
// scores are excluded, never guessed — a pair without features
// cannot train a score->outcome model. The order is total, not
// just by timestamp: signals recorded in the same millisecond
// (CHECKLIST 251-252) break the tie by insertion id, so two
// runs over the same store always return the same order —
// ORDER BY ts alone left same-millisecond pairs in unspecified
// SQLite order, which is what made TestTrainingPairs flip.
func (s *Store) TrainingPairs(ctx context.Context) ([]learned.Pair, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.tier, d.tier_scores_json, e.signal, e.ts
		FROM feedback_events e
		JOIN feedback_decisions d ON d.decision_id = e.decision_id
		ORDER BY e.ts ASC, e.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("feedback: load training pairs: %w", err)
	}
	defer rows.Close()
	var pairs []learned.Pair
	for rows.Next() {
		var tier, scoresJSON, signal string
		var tsMs int64
		if err := rows.Scan(&tier, &scoresJSON, &signal, &tsMs); err != nil {
			return nil, fmt.Errorf("feedback: load training pairs: %w", err)
		}
		scores := parseTierScores(scoresJSON)
		if len(scores) == 0 {
			continue
		}
		sig := Signal(signal)
		if _, ok := SignalPoints[sig]; !ok {
			continue
		}
		pairs = append(pairs, learned.Pair{
			Scores: scores,
			Tier:   tier,
			Label:  LabelForSignal(sig),
			TS:     time.UnixMilli(tsMs).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("feedback: load training pairs: %w", err)
	}
	return pairs, nil
}

// CodeOutcomes returns the auto/code mode's per-model coding-task
// outcome tally, rebuilt from the persisted signals (CHECKLIST
// 265): one entry per model with a recorded signal on a decision
// served to the coding tier — Attempts the labelled outcome count
// and Solved the summed solved credit (accept 1, edit 0.5,
// discard/regenerate 0, per autorouter.SolveCredit — the mapping
// lives in exactly one place, so the store and the scorer can
// never disagree). Signals on any other tier are excluded (a
// reasoning/chat/quick-qa decision is not a coding task, even when
// it served a code prompt), and a signal outside the four known
// ones is skipped rather than guessed. The map is empty when
// nothing is recorded — an honest empty state, never an invented
// rate. The caller (the server at startup) loads the result into
// the in-memory OutcomeStore, so a model keeps its measured solve
// rate across restarts; the 10-outcome evidence gate still applies
// at scoring time, exactly as before.
func (s *Store) CodeOutcomes(ctx context.Context) (map[string]autorouter.TaskOutcome, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.model, e.signal
		FROM feedback_events e
		JOIN feedback_decisions d ON d.decision_id = e.decision_id
		WHERE d.tier = 'coding'`)
	if err != nil {
		return nil, fmt.Errorf("feedback: load code outcomes: %w", err)
	}
	defer rows.Close()
	out := make(map[string]autorouter.TaskOutcome)
	for rows.Next() {
		var model, signal string
		if err := rows.Scan(&model, &signal); err != nil {
			return nil, fmt.Errorf("feedback: load code outcomes: %w", err)
		}
		credit, ok := autorouter.SolveCredit(signal)
		if !ok || model == "" {
			continue
		}
		o := out[model]
		o.Attempts++
		o.Solved += credit
		out[model] = o
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("feedback: load code outcomes: %w", err)
	}
	return out, nil
}

// tierData is one tier's accumulated evidence: the raw signal
// tally (for display and for the raw sample count) and the timed
// signals the calibrated nudge is computed from (points + age,
// so decay applies). It carries no content.
type tierData struct {
	counts  SignalCounts
	signals []TimedSignal
}

// tierSignalData returns the per-tier evidence (all four tiers
// present, zero-filled) plus the total number of signals. Signal
// ages are measured against the store clock at call time, so decay
// is always computed from the stored timestamps — nothing is ever
// rewritten or deleted as it ages.
func (s *Store) tierSignalData(ctx context.Context) (map[string]tierData, int, error) {
	data := map[string]tierData{}
	for _, t := range autorouter.AllTiers {
		data[string(t)] = tierData{}
	}
	nowMs := s.now().UnixMilli()
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.tier, e.signal, e.ts
		FROM feedback_events e
		JOIN feedback_decisions d ON d.decision_id = e.decision_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("feedback: tally signals: %w", err)
	}
	defer rows.Close()
	total := 0
	for rows.Next() {
		var tier, signal string
		var tsMs int64
		if err := rows.Scan(&tier, &signal, &tsMs); err != nil {
			return nil, 0, fmt.Errorf("feedback: tally signals: %w", err)
		}
		sig := Signal(signal)
		points, ok := SignalPoints[sig]
		if !ok {
			continue
		}
		d := data[tier]
		switch sig {
		case SignalAccept:
			d.counts.Accept++
		case SignalEdit:
			d.counts.Edit++
		case SignalDiscard:
			d.counts.Discard++
		case SignalRegenerate:
			d.counts.Regenerate++
		}
		d.signals = append(d.signals, TimedSignal{
			Points: points,
			Age:    time.Duration(nowMs-tsMs) * time.Millisecond,
		})
		data[tier] = d
		total++
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("feedback: tally signals: %w", err)
	}
	return data, total, nil
}

// LearnedWeights implements autorouter.WeightsProvider: the current
// per-tier score nudge, with the sample count and signal breakdown
// behind it. With learning off every weight is exactly 0 (the
// sample counts are still reported, so the dashboard can show how
// much signal has accumulated while learning is off). Results are
// cached for 5 s; RecordSignal invalidates the cache.
func (s *Store) LearnedWeights() map[autorouter.Tier]autorouter.TierWeight {
	s.cacheMu.Lock()
	if s.cacheFilled && time.Since(s.cachedAt) < weightsCacheTTL {
		out := make(map[autorouter.Tier]autorouter.TierWeight, len(s.cached))
		for k, v := range s.cached {
			out[k] = v
		}
		s.cacheMu.Unlock()
		return out
	}
	s.cacheMu.Unlock()

	enabled := s.Enabled()
	data, _, err := s.tierSignalData(context.Background())
	out := make(map[autorouter.Tier]autorouter.TierWeight, len(autorouter.AllTiers))
	for _, tier := range autorouter.AllTiers {
		w := autorouter.TierWeight{Samples: 0}
		if err == nil {
			d := data[string(tier)]
			w.Samples = d.counts.Samples()
			w.Accepts, w.Edits, w.Discards, w.Regenerations = d.counts.Accept, d.counts.Edit, d.counts.Discard, d.counts.Regenerate
			if enabled {
				w.Weight, _, _ = CalibratedNudge(d.signals)
			}
		}
		out[tier] = w
	}
	s.cacheMu.Lock()
	s.cached = out
	s.cachedAt = time.Now()
	s.cacheFilled = true
	s.cacheMu.Unlock()
	return out
}

// Summary returns the dashboard payload: the learning flag, the
// nudge rule parameters, the total, one row per tier (always all
// four, zero-filled), and the 10 most recent signals.
func (s *Store) Summary(ctx context.Context) (Summary, error) {
	enabled := s.Enabled()
	data, total, err := s.tierSignalData(ctx)
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{
		Enabled:      enabled,
		MinSamples:   MinSamples,
		Step:         Step,
		MaxNudge:     MaxNudge,
		HalfLifeDays: float64(DecayHalfLife) / float64(24*time.Hour),
		Total:        total,
		Tiers:        make([]TierSummary, 0, len(autorouter.AllTiers)),
		Recent:       []Event{},
	}
	for _, tier := range autorouter.AllTiers {
		d := data[string(tier)]
		row := TierSummary{
			Tier:       string(tier),
			Counts:     d.counts,
			Samples:    d.counts.Samples(),
			MinSamples: MinSamples,
		}
		if enabled {
			row.Weight, row.Active, row.EffectiveSamples = CalibratedNudge(d.signals)
		} else {
			// Learning off: the nudge is 0, but the effective
			// sample count is still reported so the dashboard can
			// show how much (decayed) evidence has accumulated.
			_, _, row.EffectiveSamples = CalibratedNudge(d.signals)
		}
		sum.Tiers = append(sum.Tiers, row)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.ts, e.decision_id, e.signal,
		       d.tier, d.model
		FROM feedback_events e
		JOIN feedback_decisions d ON d.decision_id = e.decision_id
		ORDER BY e.id DESC LIMIT 10`)
	if err != nil {
		return Summary{}, fmt.Errorf("feedback: recent signals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		var tsMs int64
		var sig string
		if err := rows.Scan(&e.ID, &tsMs, &e.DecisionID, &sig, &e.Tier, &e.Model); err != nil {
			return Summary{}, fmt.Errorf("feedback: recent signals: %w", err)
		}
		e.TS = time.UnixMilli(tsMs).UTC()
		e.Signal = Signal(sig)
		sum.Recent = append(sum.Recent, e)
	}
	if err := rows.Err(); err != nil {
		return Summary{}, fmt.Errorf("feedback: recent signals: %w", err)
	}
	// A/B evaluation (item 115): the heuristic vs the no-feedback
	// baseline on the recorded traffic, 70/30 time split. An
	// evaluation failure never fails the summary — the dashboard
	// shows no eval instead of no feedback at all.
	if ev, err := s.Evaluate(ctx); err == nil {
		sum.Eval = &ev
	}
	return sum, nil
}
