// Effort-move tests (CHECKLIST 256): the scorer may move effort
// before switching model — a same-model effort change within
// EffortMoveTolerance of the best model switch wins, so the cheap
// adjustment is preferred over the expensive switch.
package scoring

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/router"
)

func TestBestEffortMovePrefersEffortOverModelSwitch(t *testing.T) {
	current := Candidate{ProviderID: "p1", Model: "m", Effort: "medium"}
	// The low-effort move scores 0.815 (hand-computed from
	// ModeBalanced: lat 100ms 0.909, success 0.9, cost $5 0.667,
	// quality 0.9, quota 50 0.5). The best model switch scores
	// 0.820 (same, but 80ms latency) — 0.005 better, well inside
	// the 0.05 tolerance, so the effort move wins.
	move := Candidate{ProviderID: "p1", Model: "m", Effort: "low"}
	swtch := Candidate{ProviderID: "p2", Model: "m2"}
	signals := map[Candidate]Signals{
		current: {LatencyMs: 120, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50},
		move:    {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50},
		swtch:   {LatencyMs: 80, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50},
	}
	got, ok := BestEffortMove(current, []Candidate{move, swtch}, signals, ModeBalanced)
	if !ok {
		t.Fatal("BestEffortMove returned false, want the low-effort move (within tolerance of the switch)")
	}
	if got != move {
		t.Errorf("BestEffortMove = %+v, want %+v (prefer changing effort over switching model)", got, move)
	}
}

func TestBestEffortMoveLosesWhenClearlyWorse(t *testing.T) {
	current := Candidate{ProviderID: "p1", Model: "m", Effort: "medium"}
	move := Candidate{ProviderID: "p1", Model: "m", Effort: "low"}
	// A genuinely free, near-instant switch scores 0.9025 —
	// 0.0875 above the move, outside the 0.05 tolerance, so the
	// switch wins and no effort move is returned.
	swtch := Candidate{ProviderID: "p2", Model: "m2"}
	signals := map[Candidate]Signals{
		current: {LatencyMs: 120, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50},
		move:    {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50},
		swtch:   {LatencyMs: 10, SuccessRate: 0.9, CostUSDPer1M: 0, QualityScore: 0.9, QuotaHint: 50},
	}
	if got, ok := BestEffortMove(current, []Candidate{move, swtch}, signals, ModeBalanced); ok {
		t.Errorf("BestEffortMove = %+v, want false (the switch is clearly better)", got)
	}
}

func TestBestEffortMoveStrictImprovementWithNoSwitch(t *testing.T) {
	current := Candidate{ProviderID: "p1", Model: "m", Effort: "medium"}
	move := Candidate{ProviderID: "p1", Model: "m", Effort: "low"}
	better := map[Candidate]Signals{
		current: {LatencyMs: 120, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50},
		move:    {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50},
	}
	if got, ok := BestEffortMove(current, []Candidate{move}, better, ModeBalanced); !ok || got != move {
		t.Errorf("BestEffortMove = %+v, %v, want the low-effort move (strictly better than current, no switch to compare)", got, ok)
	}
	worse := map[Candidate]Signals{
		current: {LatencyMs: 120, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50},
		move:    {LatencyMs: 300, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50},
	}
	if got, ok := BestEffortMove(current, []Candidate{move}, worse, ModeBalanced); ok {
		t.Errorf("BestEffortMove = %+v, want false (the move is worse than the current effort — no churn)", got)
	}
}

func TestBestEffortMoveNoSameModelEffort(t *testing.T) {
	current := Candidate{ProviderID: "p1", Model: "m", Effort: "medium"}
	swtch := Candidate{ProviderID: "p2", Model: "m2"}
	signals := map[Candidate]Signals{
		current: {LatencyMs: 120, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50},
		swtch:   {LatencyMs: 80, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50},
	}
	if got, ok := BestEffortMove(current, []Candidate{swtch}, signals, ModeBalanced); ok {
		t.Errorf("BestEffortMove = %+v, want false (no same-model effort to move to)", got)
	}
}

// TestRankKeysSignalsByEffort: Rank treats (provider, model,
// effort) as the full candidate key — the same pair at two efforts
// with different signals ranks by those signals, and both entries
// survive (no dedup by model).
func TestRankKeysSignalsByEffort(t *testing.T) {
	low := Candidate{ProviderID: "p", Model: "m", Effort: "low"}
	high := Candidate{ProviderID: "p", Model: "m", Effort: "high"}
	signals := map[Candidate]Signals{
		low:  {LatencyMs: 50, SuccessRate: 0.9, CostUSDPer1M: 1, QualityScore: 0.9, QuotaHint: 50},
		high: {LatencyMs: 2000, SuccessRate: 0.9, CostUSDPer1M: 20, QualityScore: 0.9, QuotaHint: 50},
	}
	ranked := Rank([]Candidate{high, low}, signals, ModeCheap)
	if len(ranked) != 2 {
		t.Fatalf("Rank = %v, want both effort entries", ranked)
	}
	if ranked[0] != low || ranked[1] != high {
		t.Errorf("Rank = %v, want [low high] (cheap/fast low effort first)", ranked)
	}
}

// TestHookKeepsEffortAttempts: the strategy hook carries each
// attempt's Effort through — two attempts on the same pair at
// different efforts stay two attempts, ordered by the pair's
// signals, never collapsed into one.
func TestHookKeepsEffortAttempts(t *testing.T) {
	chain := []router.Attempt{
		{ProviderID: "p", Model: "m", Effort: "low"},
		{ProviderID: "p", Model: "m", Effort: "high"},
	}
	hook := HookWithWeights(func(providerID, model string) Signals {
		return Signals{LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.9, QuotaHint: 50}
	}, Modes[ModeBalanced])
	out := hook(chain)
	if len(out) != 2 {
		t.Fatalf("hook output = %v, want both effort attempts", out)
	}
	// Both entries are the same pair at different efforts — the
	// hook must not have dropped one as a duplicate. The pair's
	// signals are identical, so the tie keeps config order.
	seen := map[string]bool{}
	for _, a := range out {
		seen[a.Effort] = true
	}
	if !seen["low"] || !seen["high"] {
		t.Errorf("hook output = %v, want one low and one high entry", out)
	}
}
