package feedback

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "feedback.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func register(t *testing.T, s *Store, id, tier, model string) {
	t.Helper()
	if err := s.RegisterDecision(context.Background(), DecisionRef{ID: id, Tier: tier, Model: model, LatencyMicros: 123}); err != nil {
		t.Fatalf("RegisterDecision %s: %v", id, err)
	}
}

// TestSchemaStoresNoContent is the no-content rule as a test: the
// feedback schema must have no column that could hold a prompt,
// an answer, or any message content. It checks both the forbidden
// words and an exact column allowlist, so any future column added
// to either table fails here until the test is deliberately
// updated — a new column that stores content cannot slip in
// unnoticed. It also scans the schema SQL itself, so a forbidden
// identifier cannot hide in a constraint or comment either.
func TestSchemaStoresNoContent(t *testing.T) {
	s := openTestStore(t)
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('feedback_decisions') UNION ALL SELECT name FROM pragma_table_info('feedback_events')`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()
	forbidden := []string{"prompt", "answer", "message", "content", "text", "body", "request", "response", "completion"}
	// Exact allowlist: the only columns the feedback store may
	// ever have. Anything else — including a content column under
	// an innocuous name — fails this test.
	allowed := map[string]bool{
		"decision_id": true, "ts": true, "tier": true, "model": true,
		"decision_latency_us": true, "id": true, "signal": true,
		// tier_scores_json holds raw per-tier cosine scores
		// (numbers only) for the A/B evaluation — metadata, not
		// content: it can never hold a prompt or an answer.
		"tier_scores_json": true,
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !allowed[name] {
			t.Errorf("schema column %q is not in the allowlist — feedback must store metadata only (decision id, tier, model, latency, signal)", name)
		}
		for _, bad := range forbidden {
			if strings.Contains(strings.ToLower(name), bad) {
				t.Errorf("schema column %q looks like it could store message content (forbidden: %q)", name, bad)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	// The schema SQL itself must contain no forbidden identifier.
	// ("text" is excluded here: it is the SQLite column TYPE
	// keyword in the schema text, not a column name — the
	// column-name check above already rejects a column literally
	// named "text".)
	var sqlText string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'feedback_decisions'`).Scan(&sqlText); err != nil {
		t.Fatalf("schema sql: %v", err)
	}
	for _, bad := range []string{"prompt", "answer", "message", "content", "body", "request", "response", "completion"} {
		if strings.Contains(strings.ToLower(sqlText), bad) {
			t.Errorf("feedback_decisions schema mentions %q — no content may ever be stored", bad)
		}
	}
}

func TestParseSignal(t *testing.T) {
	for _, sig := range AllSignals {
		if _, err := ParseSignal(string(sig)); err != nil {
			t.Errorf("ParseSignal(%q): %v", sig, err)
		}
	}
	// Case-insensitive, whitespace-tolerant — but never a guess.
	if got, err := ParseSignal("  Accept "); err != nil || got != SignalAccept {
		t.Errorf("ParseSignal(\"  Accept \") = %q, %v", got, err)
	}
	for _, bad := range []string{"", "like", "upvote", "accepts", "good"} {
		if _, err := ParseSignal(bad); err == nil {
			t.Errorf("ParseSignal(%q) accepted, want rejection", bad)
		}
	}
}

func TestNudgeFor(t *testing.T) {
	// Below the sample floor: exactly zero, inactive — even 9 accepts.
	if n, active := NudgeFor(SignalCounts{Accept: 9}); n != 0 || active {
		t.Errorf("9 accepts: nudge=%v active=%v, want 0/false", n, active)
	}
	// At the floor: 10 accepts -> +0.05 (10 * 0.005), at the cap.
	if n, active := NudgeFor(SignalCounts{Accept: 10}); n != 0.05 || !active {
		t.Errorf("10 accepts: nudge=%v active=%v, want 0.05/true", n, active)
	}
	// 100 accepts still clamp at +0.05.
	if n, _ := NudgeFor(SignalCounts{Accept: 100}); n != MaxNudge {
		t.Errorf("100 accepts: nudge=%v, want cap %v", n, MaxNudge)
	}
	// 10 discards -> -0.05.
	if n, _ := NudgeFor(SignalCounts{Discard: 10}); n != -MaxNudge {
		t.Errorf("10 discards: nudge=%v, want %v", n, -MaxNudge)
	}
	// Mixed below the cap: 6 accepts + 4 discards = rate 0.2 -> +0.01.
	if n, active := NudgeFor(SignalCounts{Accept: 6, Discard: 4}); !approx(n, 0.01) || !active {
		t.Errorf("6 accept + 4 discard: nudge=%v active=%v, want 0.01/true", n, active)
	}
	// Edits are half-weight: 8 accepts + 4 edits = rate (8-2)/12 = 0.5
	// -> +0.025. (Under the old fixed rule this was net 6 * 0.005
	// = +0.03; the calibrated rate, not the volume, now sets the
	// nudge — see CalibratedNudge.)
	if n, _ := NudgeFor(SignalCounts{Accept: 8, Edit: 4}); n != 0.025 {
		t.Errorf("8 accept + 4 edit: nudge=%v, want 0.025", n)
	}
	// Balanced signals calibrate to exactly zero — but active: the
	// tier has enough evidence, and the evidence says "neutral".
	if n, active := NudgeFor(SignalCounts{Accept: 10, Discard: 10}); n != 0 || !active {
		t.Errorf("10 accept + 10 discard: nudge=%v active=%v, want 0/true", n, active)
	}
	// Rate, not volume: 9 accepts + 1 discard (rate 0.8) out-nudges
	// 10 accepts + 10 discards (rate 0), and equals 90 accepts +
	// 10 discards (same rate 0.8) — volume alone must not move it.
	if n, _ := NudgeFor(SignalCounts{Accept: 9, Discard: 1}); !approx(n, 0.04) {
		t.Errorf("9 accept + 1 discard: nudge=%v, want 0.04", n)
	}
	if n, _ := NudgeFor(SignalCounts{Accept: 90, Discard: 10}); !approx(n, 0.04) {
		t.Errorf("90 accept + 10 discard: nudge=%v, want 0.04 (same rate as 9+1)", n)
	}
	// Regenerate counts like a discard.
	if n, _ := NudgeFor(SignalCounts{Regenerate: 10}); n != -MaxNudge {
		t.Errorf("10 regenerates: nudge=%v, want %v", n, -MaxNudge)
	}
}

// approx reports whether two nudge values agree to within 1e-9:
// rate * MaxNudge is not always exactly representable in binary
// floating point (0.2 * 0.05 = 0.010000000000000002), so exact
// equality is the wrong assertion for calibrated nudges.
func approx(got, want float64) bool {
	d := got - want
	return d < 1e-9 && d > -1e-9
}

func TestRecordSignalValidation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	register(t, s, "dec-1", "chat", "model-x")

	// Unknown decision id: rejected, never recorded.
	if _, _, err := s.RecordSignal(ctx, "dec-nope", SignalAccept); !errors.Is(err, ErrUnknownDecision) {
		t.Errorf("unknown decision: err=%v, want ErrUnknownDecision", err)
	}
	// Invalid signal: rejected.
	if _, _, err := s.RecordSignal(ctx, "dec-1", Signal("love")); err == nil {
		t.Error("invalid signal accepted, want rejection")
	}
	// Empty decision id: rejected.
	if _, _, err := s.RecordSignal(ctx, "", SignalAccept); err == nil {
		t.Error("empty decision id accepted, want rejection")
	}
	// Happy path.
	ev, dup, err := s.RecordSignal(ctx, "dec-1", SignalAccept)
	if err != nil || dup {
		t.Fatalf("RecordSignal: ev=%+v dup=%v err=%v", ev, dup, err)
	}
	if ev.Tier != "chat" || ev.Model != "model-x" || ev.Signal != SignalAccept {
		t.Errorf("event = %+v, want tier chat / model model-x / accept", ev)
	}
	// Same signal again: idempotent, same event id, no new row.
	ev2, dup, err := s.RecordSignal(ctx, "dec-1", SignalAccept)
	if err != nil || !dup || ev2.ID != ev.ID {
		t.Errorf("repeat accept: ev=%+v dup=%v err=%v, want duplicate of id %d", ev2, dup, err, ev.ID)
	}
	// A different signal for the same decision: conflict.
	if _, _, err := s.RecordSignal(ctx, "dec-1", SignalDiscard); !errors.Is(err, ErrSignalConflict) {
		t.Errorf("different signal: err=%v, want ErrSignalConflict", err)
	}
	sum, err := s.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Total != 1 {
		t.Errorf("total signals = %d, want 1 (no duplicates, no conflicts stored)", sum.Total)
	}
}

func TestRegisterDecisionValidation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	for _, bad := range []DecisionRef{
		{ID: "", Tier: "chat", Model: "m"},
		{ID: "d", Tier: "nope", Model: "m"},
		{ID: "d", Tier: "chat", Model: "m", LatencyMicros: -1},
		{ID: strings.Repeat("x", 129), Tier: "chat", Model: "m"},
	} {
		if err := s.RegisterDecision(ctx, bad); err == nil {
			t.Errorf("RegisterDecision(%+v) accepted, want rejection", bad)
		}
	}
	// Idempotent re-register keeps the first row.
	register(t, s, "dec-9", "coding", "model-a")
	if err := s.RegisterDecision(ctx, DecisionRef{ID: "dec-9", Tier: "chat", Model: "model-b"}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	ev, _, err := s.RecordSignal(ctx, "dec-9", SignalEdit)
	if err != nil {
		t.Fatalf("RecordSignal: %v", err)
	}
	if ev.Tier != "coding" || ev.Model != "model-a" {
		t.Errorf("event tier/model = %s/%s, want coding/model-a (first registration wins)", ev.Tier, ev.Model)
	}
}

func TestLearnedWeightsDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	// Learning is OFF until SetEnabled(true): signals accumulate,
	// but the weights the router would apply stay exactly zero.
	for i := 0; i < 10; i++ {
		id := "dec-a" + string(rune('0'+i))
		register(t, s, id, "chat", "model-x")
		if _, _, err := s.RecordSignal(ctx, id, SignalAccept); err != nil {
			t.Fatalf("RecordSignal %s: %v", id, err)
		}
	}
	if w := s.LearnedWeights()["chat"]; w.Weight != 0 || w.Samples != 10 {
		t.Errorf("disabled: weight=%v samples=%d, want 0/10", w.Weight, w.Samples)
	}
	s.SetEnabled(true)
	if w := s.LearnedWeights()["chat"]; w.Weight != 0.05 || w.Samples != 10 || w.Accepts != 10 {
		t.Errorf("enabled: weight=%v samples=%d accepts=%d, want 0.05/10/10", w.Weight, w.Samples, w.Accepts)
	}
	// Untouched tiers stay zero with zero samples.
	if w := s.LearnedWeights()["coding"]; w.Weight != 0 || w.Samples != 0 {
		t.Errorf("untouched tier: %+v, want zero", w)
	}
}

func TestSummaryShape(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	sum, err := s.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Total != 0 || len(sum.Tiers) != 4 || len(sum.Recent) != 0 {
		t.Errorf("empty summary = total %d, %d tiers, %d recent; want 0/4/0", sum.Total, len(sum.Tiers), len(sum.Recent))
	}
	if sum.MinSamples != MinSamples || sum.Step != Step || sum.MaxNudge != MaxNudge {
		t.Errorf("rule params = %d/%v/%v, want %d/%v/%v", sum.MinSamples, sum.Step, sum.MaxNudge, MinSamples, Step, MaxNudge)
	}
	// Recent signals are newest-first, metadata only.
	register(t, s, "dec-r1", "quick-qa", "model-q")
	register(t, s, "dec-r2", "quick-qa", "model-q")
	if _, _, err := s.RecordSignal(ctx, "dec-r1", SignalAccept); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordSignal(ctx, "dec-r2", SignalDiscard); err != nil {
		t.Fatal(err)
	}
	sum, err = s.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(sum.Recent) != 2 || sum.Recent[0].DecisionID != "dec-r2" || sum.Recent[1].DecisionID != "dec-r1" {
		t.Errorf("recent = %+v, want newest-first [dec-r2, dec-r1]", sum.Recent)
	}
}

// timed builds n identical timed signals of the given point value
// and age — the pure-function input for CalibratedNudge.
func timed(n int, points float64, age time.Duration) []TimedSignal {
	out := make([]TimedSignal, n)
	for i := range out {
		out[i] = TimedSignal{Points: points, Age: age}
	}
	return out
}

func TestDecayWeight(t *testing.T) {
	if w := DecayWeight(0); w != 1 {
		t.Errorf("age 0: weight=%v, want 1", w)
	}
	if w := DecayWeight(-time.Hour); w != 1 {
		t.Errorf("negative age: weight=%v, want 1 (decay never amplifies)", w)
	}
	if w := DecayWeight(DecayHalfLife); w != 0.5 {
		t.Errorf("one half-life: weight=%v, want 0.5", w)
	}
	if w := DecayWeight(2 * DecayHalfLife); w != 0.25 {
		t.Errorf("two half-lives: weight=%v, want 0.25", w)
	}
	if w := DecayWeight(4 * DecayHalfLife); w < 0.06 || w > 0.065 {
		t.Errorf("four half-lives: weight=%v, want ~0.0625 (effectively forgotten after ~a month)", w)
	}
}

func TestCalibratedNudgeDecay(t *testing.T) {
	// Fresh evidence calibrates normally: 10 fresh accepts -> +0.05.
	if n, active, eff := CalibratedNudge(timed(10, +1, 0)); n != 0.05 || !active || eff != 10 {
		t.Errorf("10 fresh accepts: nudge=%v active=%v eff=%v, want 0.05/true/10", n, active, eff)
	}
	// The SAME 10 accepts, one half-life old, fall below the floor:
	// effective samples 5 < 10, so the nudge switches off entirely.
	// Stale evidence alone must never keep moving routing.
	if n, active, eff := CalibratedNudge(timed(10, +1, DecayHalfLife)); n != 0 || active || eff != 5 {
		t.Errorf("10 accepts one half-life old: nudge=%v active=%v eff=%v, want 0/false/5", n, active, eff)
	}
	// Two half-lives old: effective samples 2.5, still off.
	if _, active, eff := CalibratedNudge(timed(10, +1, 2*DecayHalfLife)); active || eff != 2.5 {
		t.Errorf("10 accepts two half-lives old: active=%v eff=%v, want false/2.5", active, eff)
	}
	// Old signals fade against fresh ones: 10 fresh discards plus
	// 10 accepts from 4 half-lives ago (weight 1/16 each). Without
	// decay the two groups would cancel to rate 0; with decay the
	// fresh discards dominate: eff = 10 + 0.625 = 10.625,
	// net = -10 + 0.625 = -9.375, rate ≈ -0.882, nudge ≈ -0.0441.
	signals := append(timed(10, -1, 0), timed(10, +1, 4*DecayHalfLife)...)
	n, active, eff := CalibratedNudge(signals)
	if !active || eff < 10.62 || eff > 10.63 {
		t.Errorf("fresh discards + month-old accepts: active=%v eff=%v, want true/10.625", active, eff)
	}
	if n > -0.044 || n < -0.0442 {
		t.Errorf("fresh discards + month-old accepts: nudge=%v, want ≈ -0.0441 (old accepts barely offset)", n)
	}
	// Fresh evidence in the same direction is unaffected by old
	// evidence: 10 fresh accepts + 10 accepts 3 half-lives old
	// (weight 1/8) still rates 1.0 -> +0.05, eff 11.25.
	signals = append(timed(10, +1, 0), timed(10, +1, 3*DecayHalfLife)...)
	if n, active, eff := CalibratedNudge(signals); n != 0.05 || !active || eff != 11.25 {
		t.Errorf("fresh + old accepts: nudge=%v active=%v eff=%v, want 0.05/true/11.25", n, active, eff)
	}
}

// TestLearnedWeightsDecay is the store-level decay check: signals
// recorded 14 days ago lose their nudge, and fresh signals after
// that restore it. The store clock is overridden (feedback.Store
// keeps an injectable clock for exactly this) instead of rewriting
// database rows, so the test exercises the real RecordSignal /
// LearnedWeights path.
func TestLearnedWeightsDecay(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	now := t0
	s.now = func() time.Time { return now }

	for i := 0; i < 10; i++ {
		id := "dec-d" + string(rune('0'+i))
		register(t, s, id, "chat", "model-x")
		if _, _, err := s.RecordSignal(ctx, id, SignalAccept); err != nil {
			t.Fatalf("RecordSignal %s: %v", id, err)
		}
	}
	s.SetEnabled(true)
	if w := s.LearnedWeights()["chat"]; w.Weight != 0.05 || w.Samples != 10 {
		t.Fatalf("fresh: weight=%v samples=%d, want 0.05/10", w.Weight, w.Samples)
	}
	// 14 days later (2 half-lives) the same 10 accepts are worth
	// 2.5 effective samples — below the floor, so the nudge is off
	// while the raw sample count is unchanged.
	now = t0.Add(14 * 24 * time.Hour)
	s.invalidateCache() // simulate the 5s weights-cache TTL expiring
	if w := s.LearnedWeights()["chat"]; w.Weight != 0 || w.Samples != 10 {
		t.Errorf("14 days later: weight=%v samples=%d, want 0/10 (decayed below floor)", w.Weight, w.Samples)
	}
	sum, err := s.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	var chatRow *TierSummary
	for i := range sum.Tiers {
		if sum.Tiers[i].Tier == "chat" {
			chatRow = &sum.Tiers[i]
		}
	}
	if chatRow == nil {
		t.Fatalf("summary has no chat tier row: %+v", sum.Tiers)
	}
	if got := chatRow.EffectiveSamples; got != 2.5 {
		t.Errorf("summary effective samples = %v, want 2.5", got)
	}
	// 10 fresh accepts at the new time restore the full nudge:
	// eff = 10 fresh + 2.5 decayed = 12.5, rate 1.0 -> +0.05.
	for i := 0; i < 10; i++ {
		id := "dec-n" + string(rune('0'+i))
		register(t, s, id, "chat", "model-x")
		if _, _, err := s.RecordSignal(ctx, id, SignalAccept); err != nil {
			t.Fatalf("RecordSignal %s: %v", id, err)
		}
	}
	if w := s.LearnedWeights()["chat"]; w.Weight != 0.05 || w.Samples != 20 {
		t.Errorf("after fresh signals: weight=%v samples=%d, want 0.05/20", w.Weight, w.Samples)
	}
}

// TestStoreClearAll proves the feedback half of the DB-wipe scope:
// every decision and every signal is deleted, the summary goes
// back to zero, and a second clear is a clean no-op. The
// learning-enabled flag is untouched (clearing history is not
// toggling learning).
func TestTrainingPairs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// A scored decision with an accept, a scored decision with a
	// discard, and a scoreless decision with an accept: only the
	// two scored ones may train the learned router.
	if err := s.RegisterDecision(ctx, DecisionRef{ID: "dec-1", Tier: "coding", Model: "m1",
		TierScores: map[string]float64{"coding": 0.9, "chat": 0.1, "reasoning": 0.2, "quick-qa": 0.05}}); err != nil {
		t.Fatalf("register dec-1: %v", err)
	}
	if err := s.RegisterDecision(ctx, DecisionRef{ID: "dec-2", Tier: "chat", Model: "m2",
		TierScores: map[string]float64{"coding": 0.1, "chat": 0.8, "reasoning": 0.2, "quick-qa": 0.05}}); err != nil {
		t.Fatalf("register dec-2: %v", err)
	}
	register(t, s, "dec-3", "coding", "m1") // no scores: excluded
	// Recorded in a fixed order, not map order (CHECKLIST
	// 251-252): ranging over a map recorded dec-1/dec-2 in a
	// random order, and TrainingPairs returns outcome (time)
	// order, so pairs[0]/pairs[1] flipped whenever the map
	// order did — the flake that passed in isolation and
	// failed under load. The fixed order plus the (ts, id)
	// tiebreak in TrainingPairs makes the pair order total:
	// dec-1 is always recorded first, so it is always pair 0,
	// even when both signals land in the same millisecond.
	for _, sig := range []struct {
		id  string
		sig Signal
	}{
		{"dec-1", SignalAccept},
		{"dec-2", SignalDiscard},
		{"dec-3", SignalAccept},
	} {
		if _, _, err := s.RecordSignal(ctx, sig.id, sig.sig); err != nil {
			t.Fatalf("signal %s: %v", sig.id, err)
		}
	}
	pairs, err := s.TrainingPairs(ctx)
	if err != nil {
		t.Fatalf("TrainingPairs: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("TrainingPairs = %d pairs, want 2 (scoreless decision excluded, never guessed)", len(pairs))
	}
	if pairs[0].Tier != "coding" || pairs[0].Label != 1 || pairs[0].Scores["coding"] != 0.9 {
		t.Errorf("pair 0 = %+v, want coding/accept/0.9", pairs[0])
	}
	if pairs[1].Tier != "chat" || pairs[1].Label != 0 {
		t.Errorf("pair 1 = %+v, want chat/discard/0", pairs[1])
	}
	if got := LabelForSignal(SignalEdit); got != 0.5 {
		t.Errorf("LabelForSignal(edit) = %v, want 0.5", got)
	}
	if got := LabelForSignal(SignalRegenerate); got != 0 {
		t.Errorf("LabelForSignal(regenerate) = %v, want 0", got)
	}
}

func TestStoreClearAll(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	register(t, s, "dec-1", "chat", "model-x")
	register(t, s, "dec-2", "coding", "model-a")
	if _, _, err := s.RecordSignal(ctx, "dec-1", SignalAccept); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordSignal(ctx, "dec-2", SignalDiscard); err != nil {
		t.Fatal(err)
	}
	s.SetEnabled(true)
	if sum, err := s.Summary(ctx); err != nil || sum.Total != 2 {
		t.Fatalf("summary before clear = %+v (err %v), want Total 2", sum, err)
	}
	decisions, signals, err := s.ClearAll(ctx)
	if err != nil {
		t.Fatalf("ClearAll: %v", err)
	}
	if decisions != 2 || signals != 2 {
		t.Errorf("ClearAll = %d decisions, %d signals; want 2/2", decisions, signals)
	}
	if sum, err := s.Summary(ctx); err != nil {
		t.Fatal(err)
	} else if sum.Total != 0 {
		t.Errorf("summary after clear Total = %d, want 0", sum.Total)
	}
	if !s.Enabled() {
		t.Error("ClearAll turned learning off; want the enabled flag untouched")
	}
	decisions, signals, err = s.ClearAll(ctx)
	if err != nil {
		t.Fatalf("second ClearAll: %v", err)
	}
	if decisions != 0 || signals != 0 {
		t.Errorf("second ClearAll = %d/%d, want 0/0", decisions, signals)
	}
}

// TestCodeOutcomes (item 265): the per-model coding-task tally
// rebuilt from persisted signals — coding tier only, solved credit
// per signal (accept 1, edit 0.5, discard/regenerate 0), empty
// when nothing is recorded.
func TestCodeOutcomes(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if out, err := s.CodeOutcomes(ctx); err != nil {
		t.Fatalf("CodeOutcomes empty store: %v", err)
	} else if len(out) != 0 {
		t.Errorf("CodeOutcomes empty store = %+v, want empty map", out)
	}

	// model-a: 6 accepts + 2 edits + 2 discards on coding decisions
	// (10 outcomes, 7.0 solved credit); model-b: 3 accepts on
	// coding decisions; model-c: 10 accepts on reasoning decisions
	// (not coding — excluded).
	signals := []struct {
		id, tier, model string
		signal          Signal
	}{
		{"a1", "coding", "model-a", SignalAccept}, {"a2", "coding", "model-a", SignalAccept},
		{"a3", "coding", "model-a", SignalAccept}, {"a4", "coding", "model-a", SignalAccept},
		{"a5", "coding", "model-a", SignalAccept}, {"a6", "coding", "model-a", SignalAccept},
		{"a7", "coding", "model-a", SignalEdit}, {"a8", "coding", "model-a", SignalEdit},
		{"a9", "coding", "model-a", SignalDiscard}, {"a10", "coding", "model-a", SignalRegenerate},
		{"b1", "coding", "model-b", SignalAccept}, {"b2", "coding", "model-b", SignalAccept},
		{"b3", "coding", "model-b", SignalAccept},
		{"c1", "reasoning", "model-c", SignalAccept}, {"c2", "reasoning", "model-c", SignalAccept},
	}
	for _, sig := range signals {
		register(t, s, sig.id, sig.tier, sig.model)
		if _, _, err := s.RecordSignal(ctx, sig.id, sig.signal); err != nil {
			t.Fatalf("RecordSignal %s: %v", sig.id, err)
		}
	}
	out, err := s.CodeOutcomes(ctx)
	if err != nil {
		t.Fatalf("CodeOutcomes: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("CodeOutcomes = %+v, want exactly [model-a model-b]", out)
	}
	if a := out["model-a"]; a.Attempts != 10 || a.Solved != 7.0 {
		t.Errorf("model-a = %+v, want 10 attempts / 7.0 solved", a)
	}
	if b := out["model-b"]; b.Attempts != 3 || b.Solved != 3.0 {
		t.Errorf("model-b = %+v, want 3 attempts / 3.0 solved", b)
	}
	if _, ok := out["model-c"]; ok {
		t.Error("model-c (reasoning tier only) must not appear in code outcomes")
	}
	// The measured rate applies at the floor: 10 outcomes at 0.70.
	if p, evidence := out["model-a"].SolveRate(); !evidence || p != 0.7 {
		t.Errorf("model-a SolveRate = %.2f/%v, want 0.70/true", p, evidence)
	}
}

// TestCodeOutcomesSurviveReopen (item 265 — the restart proof at
// the store level): outcomes recorded, the store closed, a fresh
// handle opened over the same DB file, the same tally comes back.
func TestCodeOutcomesSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "feedback.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("dec-%d", i)
		if err := s.RegisterDecision(ctx, DecisionRef{ID: id, Tier: "coding", Model: "model-code", LatencyMicros: 1}); err != nil {
			t.Fatalf("RegisterDecision: %v", err)
		}
		sig := SignalAccept
		if i >= 8 {
			sig = SignalEdit // 8 accepts + 2 edits = 9.0 solved
		}
		if _, _, err := s.RecordSignal(ctx, id, sig); err != nil {
			t.Fatalf("RecordSignal: %v", err)
		}
	}
	before, err := s.CodeOutcomes(ctx)
	if err != nil {
		t.Fatalf("CodeOutcomes before close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Fresh handle over the same file: the restart.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close()
	after, err := s2.CodeOutcomes(ctx)
	if err != nil {
		t.Fatalf("CodeOutcomes after reopen: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("after reopen = %+v, want exactly [model-code]", after)
	}
	if after["model-code"] != before["model-code"] {
		t.Errorf("after reopen = %+v, want %+v (measured rate must survive a restart)", after["model-code"], before["model-code"])
	}
	if o := after["model-code"]; o.Attempts != 10 || o.Solved != 9.0 {
		t.Errorf("after reopen = %+v, want 10 attempts / 9.0 solved", o)
	}
}
