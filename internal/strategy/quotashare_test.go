package strategy

import (
	"math"
	"testing"
)

// Hand-computed DRR sequences. Quanta are share/total; the winner
// pays a unit cost of 1 and fractional credit carries over.
// Shares 2:1 (quanta 2/3, 1/3):
//
//	round  deficits before pick      winner  deficits after
//	1      A=2/3, B=1/3              A       A=-1/3, B=1/3
//	2      A=1/3, B=2/3              B       A=1/3,  B=-1/3
//	3      A=1,   B=0                A       A=0,    B=0
//	4      A=2/3, B=1/3              A       A=-1/3, B=1/3
//	5      A=1/3, B=2/3              B       A=1/3,  B=-1/3
//	6      A=1,   B=0                A       A=0,    B=0
//
// Sequence A,B,A,A,B,A — 4:2, exactly the 2:1 share ratio.
func TestQuotaShareDRRWeightedSequence(t *testing.T) {
	members := []QuotaShareMember{{ID: "A", Share: 2}, {ID: "B", Share: 1}}
	state := NewQuotaShareState()
	want := []string{"A", "B", "A", "A", "B", "A"}
	for i, w := range want {
		res, err := SelectQuotaShare(members, state)
		if err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
		if !res.OK || res.Winner.ID != w {
			t.Fatalf("round %d: winner = %q, want %q (ordered %v)", i+1, res.Winner.ID, w, idsOf(res.Ordered))
		}
	}
	// After round 6 both deficits are exactly 0 again (see table).
	if d := state.DeficitForTest("A"); math.Abs(d) > 1e-9 {
		t.Fatalf("deficit A after 6 rounds = %v, want 0", d)
	}
	if d := state.DeficitForTest("B"); math.Abs(d) > 1e-9 {
		t.Fatalf("deficit B after 6 rounds = %v, want 0", d)
	}
}

func TestQuotaShareDRREqualSharesAlternate(t *testing.T) {
	members := []QuotaShareMember{{ID: "A", Share: 1}, {ID: "B", Share: 1}}
	state := NewQuotaShareState()
	// Quanta 1/2 each; round 1 ties 1/2-1/2 and input order wins (A),
	// round 2 B leads 0 vs 1, round 3 ties again -> A, ...
	want := []string{"A", "B", "A", "B"}
	for i, w := range want {
		res, err := SelectQuotaShare(members, state)
		if err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
		if res.Winner.ID != w {
			t.Fatalf("round %d: winner = %q, want %q", i+1, res.Winner.ID, w)
		}
	}
}

func TestQuotaShareDRRZeroShareNeverWins(t *testing.T) {
	members := []QuotaShareMember{{ID: "A", Share: 1}, {ID: "B", Share: 0}}
	state := NewQuotaShareState()
	for i := 0; i < 3; i++ {
		res, err := SelectQuotaShare(members, state)
		if err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
		if res.Winner.ID != "A" {
			t.Fatalf("round %d: winner = %q, want A (share 0 never wins while a sibling is weighted)", i+1, res.Winner.ID)
		}
	}
	// B's quantum is 0, so its deficit never moves from 0.
	if d := state.DeficitForTest("B"); d != 0 {
		t.Fatalf("deficit B = %v, want 0 (zero share gains no quantum)", d)
	}
}

func TestQuotaShareDRRAllZeroSharesShareEvenly(t *testing.T) {
	members := []QuotaShareMember{{ID: "A", Share: 0}, {ID: "B", Share: 0}}
	state := NewQuotaShareState()
	// An all-zero pool is unweighted: quanta 1/2 each, so it
	// alternates instead of pinning the first member.
	want := []string{"A", "B", "A", "B"}
	for i, w := range want {
		res, err := SelectQuotaShare(members, state)
		if err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
		if res.Winner.ID != w {
			t.Fatalf("round %d: winner = %q, want %q", i+1, res.Winner.ID, w)
		}
	}
}

func TestQuotaShareDRRThreeWayRatio(t *testing.T) {
	// Shares 3:2:1 (quanta 1/2, 1/3, 1/6). Hand-computed:
	// R1: A=1/2 B=1/3 C=1/6 -> A; A=-1/2
	// R2: A=0 B=2/3 C=1/3 -> B; B=-1/3
	// R3: A=1/2 B=0 C=1/2 -> tie A/C, input order -> A; A=-1/2
	// R4: A=0 B=1/3 C=2/3 -> C; C=-1/3
	// R5: A=1/2 B=2/3 C=-1/6 -> B; B=-1/3
	// R6: A=1 B=0 C=0 -> A; A=0
	// Sequence A,B,A,C,B,A = 3:2:1 over 6 rounds.
	members := []QuotaShareMember{{ID: "A", Share: 3}, {ID: "B", Share: 2}, {ID: "C", Share: 1}}
	state := NewQuotaShareState()
	want := []string{"A", "B", "A", "C", "B", "A"}
	for i, w := range want {
		res, err := SelectQuotaShare(members, state)
		if err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
		if res.Winner.ID != w {
			t.Fatalf("round %d: winner = %q, want %q (ordered %v)", i+1, res.Winner.ID, w, idsOf(res.Ordered))
		}
	}
}

func TestQuotaShareP2CPicksLessLoaded(t *testing.T) {
	// Equal shares: DRR winner of round 1 is A (tie, input
	// order). B has fewer in-flight requests, so P2C picks B.
	members := []QuotaShareMember{{ID: "A", Share: 1, Inflight: 5}, {ID: "B", Share: 1, Inflight: 1}}
	state := NewQuotaShareState()
	res, err := SelectQuotaShare(members, state)
	if err != nil {
		t.Fatalf("SelectQuotaShare: %v", err)
	}
	if res.Winner.ID != "B" {
		t.Fatalf("winner = %q, want B (fewer in-flight: 1 < 5)", res.Winner.ID)
	}
	if got := idsOf(res.Ordered); len(got) != 2 || got[0] != "B" || got[1] != "A" {
		t.Fatalf("ordered = %v, want [B A]", got)
	}
}

func TestQuotaShareP2CTieKeepsDRRWinner(t *testing.T) {
	members := []QuotaShareMember{{ID: "A", Share: 1, Inflight: 2}, {ID: "B", Share: 1, Inflight: 2}}
	state := NewQuotaShareState()
	res, err := SelectQuotaShare(members, state)
	if err != nil {
		t.Fatalf("SelectQuotaShare: %v", err)
	}
	if res.Winner.ID != "A" {
		t.Fatalf("winner = %q, want A (in-flight tie keeps the DRR winner)", res.Winner.ID)
	}
}

func TestQuotaShareSaturatedDeprioritizedNotDropped(t *testing.T) {
	members := []QuotaShareMember{{ID: "A", Share: 1, Saturated: true}, {ID: "B", Share: 1}}
	state := NewQuotaShareState()
	res, err := SelectQuotaShare(members, state)
	if err != nil {
		t.Fatalf("SelectQuotaShare: %v", err)
	}
	if res.Winner.ID != "B" {
		t.Fatalf("winner = %q, want B (A is saturated)", res.Winner.ID)
	}
	if got := idsOf(res.Ordered); len(got) != 2 || got[0] != "B" || got[1] != "A" {
		t.Fatalf("ordered = %v, want [B A] (saturated member stays as last-resort fallback)", got)
	}
}

func TestQuotaShareAllSaturatedFailsOpen(t *testing.T) {
	members := []QuotaShareMember{{ID: "A", Share: 1, Saturated: true}, {ID: "B", Share: 1, Saturated: true}}
	state := NewQuotaShareState()
	res, err := SelectQuotaShare(members, state)
	if err != nil {
		t.Fatalf("SelectQuotaShare: %v", err)
	}
	if !res.OK || res.Winner.ID != "A" {
		t.Fatalf("winner = %q ok=%v, want A/true (all-saturated fails open, never hard-blocks)", res.Winner.ID, res.OK)
	}
}

func TestQuotaShareAtCapDeprioritized(t *testing.T) {
	members := []QuotaShareMember{
		{ID: "A", Share: 1, Inflight: 2, MaxConcurrent: 2},
		{ID: "B", Share: 1, Inflight: 0, MaxConcurrent: 2},
	}
	state := NewQuotaShareState()
	res, err := SelectQuotaShare(members, state)
	if err != nil {
		t.Fatalf("SelectQuotaShare: %v", err)
	}
	if res.Winner.ID != "B" {
		t.Fatalf("winner = %q, want B (A is at its maxConcurrent cap)", res.Winner.ID)
	}
	if got := idsOf(res.Ordered); len(got) != 2 || got[0] != "B" || got[1] != "A" {
		t.Fatalf("ordered = %v, want [B A]", got)
	}
}

func TestQuotaShareAllAtCapFailsOpen(t *testing.T) {
	members := []QuotaShareMember{
		{ID: "A", Share: 1, Inflight: 3, MaxConcurrent: 1},
		{ID: "B", Share: 1, Inflight: 3, MaxConcurrent: 1},
	}
	state := NewQuotaShareState()
	res, err := SelectQuotaShare(members, state)
	if err != nil {
		t.Fatalf("SelectQuotaShare: %v", err)
	}
	if !res.OK {
		t.Fatal("all-at-cap pool: expected a winner (fail-open), got none")
	}
}

func TestQuotaShareEmptyPoolAndNilState(t *testing.T) {
	res, err := SelectQuotaShare(nil, NewQuotaShareState())
	if err != nil {
		t.Fatalf("empty pool: unexpected error %v", err)
	}
	if res.OK {
		t.Fatal("empty pool: expected OK=false, got a winner")
	}
	if _, err := SelectQuotaShare([]QuotaShareMember{{ID: "A", Share: 1}}, nil); err == nil {
		t.Fatal("nil DRR state: expected error, got none")
	}
}

func TestQuotaShareSingleMemberAlwaysWins(t *testing.T) {
	members := []QuotaShareMember{{ID: "only", Share: 0, Saturated: true}}
	state := NewQuotaShareState()
	res, err := SelectQuotaShare(members, state)
	if err != nil {
		t.Fatalf("SelectQuotaShare: %v", err)
	}
	if !res.OK || res.Winner.ID != "only" {
		t.Fatalf("winner = %q ok=%v, want only/true", res.Winner.ID, res.OK)
	}
}

func idsOf(members []QuotaShareMember) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.ID)
	}
	return out
}
