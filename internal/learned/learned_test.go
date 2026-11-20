package learned

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// separablePairs builds n labelled outcomes where the win label is
// a deterministic function of the served tier's own heuristic
// score: own score 0.9 -> win, own score 0.3 -> loss. Tiers
// round-robin and the two patterns interleave, so any time split
// sees both patterns in train and test. This is synthetic test
// data for the learning machinery only — it proves the model can
// learn a score->outcome mapping and calibrate it; it is never
// presented as real traffic.
func separablePairs(n int) []Pair {
	base := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	pairs := make([]Pair, 0, n)
	for i := 0; i < n; i++ {
		tier := Tiers[i%len(Tiers)]
		// The win/loss pattern follows the ROUND (i/4), not the
		// tier: every tier sees both wins and losses, so the
		// label is a function of the scores, never of the tier
		// itself. (An earlier draft keyed the label to i%2,
		// which made coding always win and chat always lose —
		// the model then "learned" the tier, not the scores.)
		win := (i/len(Tiers))%2 == 0
		own := 0.3
		label := 0.0
		if win {
			own = 0.9
			label = 1.0
		}
		scores := map[string]float64{"coding": 0.1, "chat": 0.1, "reasoning": 0.1, "quick-qa": 0.1}
		scores[tier] = own
		pairs = append(pairs, Pair{
			Scores: scores,
			Tier:   tier,
			Label:  label,
			TS:     base.Add(time.Duration(i) * time.Minute),
		})
	}
	return pairs
}

func TestTrainNoEvidenceStaysOff(t *testing.T) {
	for _, pairs := range [][]Pair{nil, {}} {
		m := Train(pairs)
		if m.Status != StatusOff {
			t.Errorf("no evidence: status = %q, want %q", m.Status, StatusOff)
		}
		if m.Active() {
			t.Error("no evidence: model must not be active")
		}
		if p := m.WinProb(map[string]float64{"coding": 0.9}, "coding"); p != 0.5 {
			t.Errorf("no evidence: WinProb = %v, want 0.5 (unknown, never a confident guess)", p)
		}
		if c := Contribution(1.0); c != MaxLearned {
			t.Errorf("Contribution(1) = %v, want %v", c, MaxLearned)
		}
	}
}

func TestTrainBelowEvidenceFloorStaysLearning(t *testing.T) {
	// 20 outcomes: separable, but below the 50-evidence floor.
	m := Train(separablePairs(20))
	if m.Status != StatusLearning {
		t.Errorf("20 outcomes: status = %q, want %q", m.Status, StatusLearning)
	}
	if m.Active() {
		t.Error("20 outcomes: model must not be active")
	}
	if p := m.WinProb(map[string]float64{"coding": 0.9}, "coding"); p != 0.5 {
		t.Errorf("below floor: WinProb = %v, want 0.5", p)
	}
}

func TestTrainSeparableActivatesWithHonestCalibration(t *testing.T) {
	m := Train(separablePairs(100))
	if m.Status != StatusActive {
		t.Fatalf("100 separable outcomes: status = %q (note: %s), ECE %.3f logloss %.3f base %.3f, want %q",
			m.Status, m.GateNote, m.ECE, m.LogLoss, m.BaseLogLoss, StatusActive)
	}
	if !m.Active() {
		t.Fatal("model must be active")
	}
	if m.ECE > MaxECE {
		t.Errorf("ECE = %.3f, want <= %.2f", m.ECE, MaxECE)
	}
	if m.LogLoss >= m.BaseLogLoss {
		t.Errorf("log-loss %.3f must beat base-rate %.3f", m.LogLoss, m.BaseLogLoss)
	}
	// A high own-score prompt for coding should win; a low one lose.
	high := map[string]float64{"coding": 0.9, "chat": 0.1, "reasoning": 0.1, "quick-qa": 0.1}
	low := map[string]float64{"coding": 0.3, "chat": 0.1, "reasoning": 0.1, "quick-qa": 0.1}
	if p := m.WinProb(high, "coding"); p < 0.7 {
		t.Errorf("high-score coding WinProb = %.3f, want >= 0.7", p)
	}
	if p := m.WinProb(low, "coding"); p > 0.3 {
		t.Errorf("low-score coding WinProb = %.3f, want <= 0.3", p)
	}
	// An untrained tier (none here — all four trained) still
	// returns a probability; an unknown tier name returns 0.5.
	if p := m.WinProb(high, "no-such-tier"); p != 0.5 {
		t.Errorf("unknown tier WinProb = %v, want 0.5", p)
	}
}

func TestTrainIgnoresBadPairs(t *testing.T) {
	pairs := separablePairs(100)
	// Poison the input: unknown tier, empty scores, out-of-range
	// labels. None may train the model or change the outcome.
	pairs = append(pairs,
		Pair{Scores: map[string]float64{"coding": 0.9}, Tier: "nope", Label: 1, TS: time.Now()},
		Pair{Scores: map[string]float64{}, Tier: "coding", Label: 1, TS: time.Now()},
		Pair{Scores: map[string]float64{"coding": 0.9}, Tier: "coding", Label: 2, TS: time.Now()},
		Pair{Scores: map[string]float64{"coding": 0.9}, Tier: "coding", Label: -1, TS: time.Now()},
	)
	m := Train(pairs)
	if m.Evidence != 100 {
		t.Errorf("Evidence = %d, want 100 (bad pairs excluded, never guessed)", m.Evidence)
	}
	if m.Status != StatusActive {
		t.Errorf("status = %q, want active after excluding bad pairs", m.Status)
	}
}

func TestTrainUncalibratedStaysLearning(t *testing.T) {
	// Labels that carry NO signal about the scores: the model
	// cannot beat the base rate, so it must stay in learning even
	// with plenty of evidence. (Labels alternate by round, not by
	// score, and every score is the same constant 0.5 — the only
	// thing the model can fit is the overall win rate, which is
	// exactly the base-rate predictor it must beat.)
	base := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	pairs := make([]Pair, 0, 100)
	for i := 0; i < 100; i++ {
		tier := Tiers[i%len(Tiers)]
		scores := map[string]float64{"coding": 0.5, "chat": 0.5, "reasoning": 0.5, "quick-qa": 0.5}
		label := 0.0
		if (i/len(Tiers))%2 == 0 {
			label = 1.0
		}
		pairs = append(pairs, Pair{Scores: scores, Tier: tier, Label: label, TS: base.Add(time.Duration(i) * time.Minute)})
	}
	m := Train(pairs)
	if m.Active() {
		t.Errorf("random labels: model must not activate (status %q, logloss %.3f vs base %.3f)", m.Status, m.LogLoss, m.BaseLogLoss)
	}
	if m.Status != StatusLearning {
		t.Errorf("random labels: status = %q, want %q", m.Status, StatusLearning)
	}
}

func TestTrainImbalancedTierLearnsLossShape(t *testing.T) {
	// Regression test for the live-smoke finding: a tier with
	// 20 wins at high scores and only 2 losses at medium scores
	// must still learn that medium scores lose — without class
	// weighting the wins swamp the losses and the model predicts
	// "win" for the medium score, failing the held-out ECE gate
	// (observed ECE 0.174 in the smoke). The other tiers get
	// balanced separable data so the global gate can pass.
	base := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	scoresFor := func(tier string, own float64) map[string]float64 {
		s := map[string]float64{"coding": 0.1, "chat": 0.1, "reasoning": 0.1, "quick-qa": 0.1}
		s[tier] = own
		return s
	}
	var pairs []Pair
	add := func(tier string, own, label float64, ts time.Time) {
		pairs = append(pairs, Pair{Scores: scoresFor(tier, own), Tier: tier, Label: label, TS: ts})
	}
	// Coding: 20 wins at 0.9, 2 losses at 0.55 — the imbalanced
	// tier. Other tiers: 13 wins at 0.9 + 12 losses at 0.3 each
	// (balanced). Interleave by time so the 70/30 split sees
	// both classes throughout.
	minute := 0
	next := func() time.Time { ts := base.Add(time.Duration(minute) * time.Minute); minute++; return ts }
	for i := 0; i < 20; i++ {
		add("coding", 0.9, 1, next())
		if i < 2 {
			add("coding", 0.55, 0, next())
		}
		for _, tier := range []string{"chat", "reasoning", "quick-qa"} {
			add(tier, 0.9, 1, next())
			add(tier, 0.3, 0, next())
		}
	}
	m := Train(pairs)
	if m.Status != StatusActive {
		t.Fatalf("imbalanced data: status = %q (note: %s), ECE %.3f, want active", m.Status, m.GateNote, m.ECE)
	}
	if p := m.WinProb(scoresFor("coding", 0.9), "coding"); p < 0.7 {
		t.Errorf("high-score coding WinProb = %.3f, want >= 0.7", p)
	}
	if p := m.WinProb(scoresFor("coding", 0.55), "coding"); p > 0.5 {
		t.Errorf("medium-score coding WinProb = %.3f, want <= 0.5 (the losses live here)", p)
	}
}

func TestContributionIsBounded(t *testing.T) {
	cases := []struct {
		p    float64
		want float64
	}{
		{0.5, 0},
		{1.0, MaxLearned},
		{0.0, -MaxLearned},
		{0.75, MaxLearned / 2},
		{0.25, -MaxLearned / 2},
		{2.0, MaxLearned}, // out-of-range input still clamps
		{-1.0, -MaxLearned},
	}
	for _, c := range cases {
		if got := Contribution(c.p); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("Contribution(%v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestRefresherFlagGatesRoutingButNotReporting(t *testing.T) {
	ctx := context.Background()
	src := func(context.Context) ([]Pair, error) { return separablePairs(100), nil }
	r := NewRefresher(src, false)
	if _, active := r.Predict(ctx, map[string]float64{"coding": 0.9}); active {
		t.Error("flag off: Predict must report inactive so routing is unchanged")
	}
	sum := r.Summary(ctx)
	if sum.Status != StatusLearning {
		// The model is evidence-ready, but the dashboard must
		// never show "active" while the flag is off.
		t.Errorf("flag off: summary status = %q, want %q (ready-but-off)", sum.Status, StatusLearning)
	}
	if sum.Evidence != 100 {
		t.Errorf("flag off: summary evidence = %d, want 100 (reporting is honest either way)", sum.Evidence)
	}
	r.SetEnabled(true)
	probs, active := r.Predict(ctx, map[string]float64{"coding": 0.9, "chat": 0.1, "reasoning": 0.1, "quick-qa": 0.1})
	if !active {
		t.Error("flag on + active model: Predict must report active")
	}
	if probs["coding"] < 0.7 {
		t.Errorf("flag on: coding win prob = %.3f, want >= 0.7", probs["coding"])
	}
	if sum := r.Summary(ctx); sum.Status != StatusActive {
		t.Errorf("flag on: summary status = %q, want %q", sum.Status, StatusActive)
	}
}

func TestRefresherKeepsOldModelOnSourceError(t *testing.T) {
	ctx := context.Background()
	calls := 0
	src := func(context.Context) ([]Pair, error) {
		calls++
		if calls == 1 {
			return separablePairs(100), nil
		}
		return nil, errors.New("store unavailable")
	}
	r := NewRefresher(src, true)
	if _, active := r.Predict(ctx, nil); !active {
		t.Fatal("first load: model must be active")
	}
	r.Invalidate()
	m := r.Current(ctx)
	if !m.Active() {
		t.Error("source error: previous model must be kept, not dropped to off")
	}
	if sum := r.Summary(ctx); sum.LastErr == "" {
		t.Error("source error: summary must report the load error honestly")
	}
}

func TestRefresherNoEvidenceIsOff(t *testing.T) {
	ctx := context.Background()
	r := NewRefresher(func(context.Context) ([]Pair, error) { return nil, nil }, true)
	if _, active := r.Predict(ctx, nil); active {
		t.Error("no evidence: Predict must be inactive")
	}
	if sum := r.Summary(ctx); sum.Status != StatusOff {
		t.Errorf("no evidence: summary status = %q, want %q", sum.Status, StatusOff)
	}
}
