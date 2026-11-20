// Quota-share pools (M3 Phase C, builder C5).
//
// The dedicated selection strategy for quota-share combos: the
// internal "quota-share" strategy (routingStrategies.ts:30-34,
// quotaShareStrategy.ts). Three mechanisms, applied in sequence:
//
//  1. Per-model bucket gating: a member whose quota window is
//     saturated for the requested model is moved to a
//     DEPRIORITIZED tail, not dropped. If EVERY member is
//     saturated, all are eligible again (fail-open: a quota-share
//     pool is never hard-blocked here).
//  2. DRR (Deficit Round Robin): each round adds a quantum
//     proportional to the member's share weight to that member's
//     deficit, then selects the member with the largest deficit,
//     which pays a fixed cost of 1. Subtracting a constant (rather
//     than zeroing) keeps accumulated fractional credit, so the
//     long-run selection frequency converges EXACTLY to the share
//     ratio and the choice stays deterministic.
//  3. P2C (Power of Two Choices) over real in-flight load: between
//     the top two DRR candidates, the one with fewer active
//     in-flight requests wins; ties keep the DRR order.
//
// This package is PURE: all live data (saturation, in-flight
// counts, concurrency caps) arrives pre-resolved in the input
// struct — no DB, no network, no globals. DRR state is explicit:
// the caller owns a QuotaShareState and passes it in, so tests
// (and the engine) can run hand-computed sequences deterministically
// and reset state by constructing a fresh one. All state is
// in-process by design; the engine owns one state per pool.
package strategy

import (
	"fmt"
	"math"
)

// QuotaShareMember is one pool member as the caller knows it.
type QuotaShareMember struct {
	// ID is the member's stable key (execution key in the
	// reference: provider + connection + model). DRR deficits
	// are keyed by it.
	ID string
	// Share is the member's quota-share weight. It follows
	// normalizeWeight (quotaShareStrategy.ts): an explicit 0
	// DISABLES the member relative to weighted siblings (it is
	// never selected while any sibling has a positive share);
	// a missing, non-finite or negative share defaults to 1.
	// When ALL shares are 0 the pool is unweighted and shares
	// evenly instead of pinning the first member.
	Share float64
	// Saturated reports that the member's quota bucket is
	// saturated for the requested model (5h, 7d or per-model
	// 7d:<model> window). Saturated members are deprioritized
	// to the tail, never dropped; all-saturated fails open.
	Saturated bool
	// Inflight is the member's live in-flight request count,
	// the P2C signal. The caller reads it from its own
	// in-flight ledger; this function never mutates it.
	Inflight int
	// MaxConcurrent is the member's per-connection concurrency
	// cap (provider_connections.max_concurrent). <= 0 means "no
	// limit". A member at/over its cap is deprioritized behind
	// members with headroom (fail-open when all are at cap).
	MaxConcurrent int
}

// AtCap reports whether the member is at or over its concurrency
// cap. Members without a positive cap never report at-cap.
func (m QuotaShareMember) AtCap() bool {
	return m.MaxConcurrent > 0 && m.Inflight >= m.MaxConcurrent
}

// QuotaShareState is one pool's DRR deficit state: accumulated
// deficit per member ID (higher = this member is "owed" more
// service). The caller owns it — one state per pool — and a fresh
// state starts every member at zero. It is NOT goroutine-safe:
// the engine must hold its own lock around Select, as it does
// for the other stateful orderings.
type QuotaShareState struct {
	deficits map[string]float64
}

// NewQuotaShareState returns an empty DRR state.
func NewQuotaShareState() *QuotaShareState {
	return &QuotaShareState{deficits: make(map[string]float64)}
}

// normalizeShare mirrors normalizeWeight: explicit 0 stays 0 (the
// operator can disable a member); anything missing, non-finite
// or <= 0 defaults to 1.
func normalizeShare(share float64) float64 {
	if share == 0 {
		return 0
	}
	if share > 0 && !math.IsNaN(share) && !math.IsInf(share, 0) {
		return share
	}
	return 1
}

// QuotaShareResult is one selection: the winner, the full ordered
// dispatch list (winner first, then the remaining eligible
// members, then at-cap members, then saturated members as
// last-resort fallbacks), and whether a member was selected at all.
type QuotaShareResult struct {
	// Winner is the selected member; zero value when no member
	// was selected (empty pool).
	Winner QuotaShareMember
	// Ordered is the full dispatch order: winner first, then the
	// remaining with-room members in DRR order, then at-cap
	// members, then saturated members. Every input member
	// appears exactly once.
	Ordered []QuotaShareMember
	// OK reports whether a winner was selected (false only for
	// an empty pool).
	OK bool
}

// SelectQuotaShare picks the next member of a quota-share pool.
//
// Order of operations (quotaShareStrategy.ts selectQuotaShareTarget):
//  1. saturation gating (fail-open),
//  2. concurrency-cap gating (fail-open),
//  3. DRR ordering over the with-room set (state mutated),
//  4. P2C between the top two DRR candidates.
//
// members must not be empty for OK to be true. state must be
// non-nil (a nil state is an error — the caller must own one per
// pool, so a nil state is a wiring bug, not a default).
func SelectQuotaShare(members []QuotaShareMember, state *QuotaShareState) (QuotaShareResult, error) {
	if state == nil {
		return QuotaShareResult{}, fmt.Errorf("quota-share: nil DRR state (caller must own one QuotaShareState per pool)")
	}
	if len(members) == 0 {
		return QuotaShareResult{}, nil
	}
	// 1) Saturation gating: eligible = not saturated; fail-open
	// when every member is saturated.
	eligible := make([]QuotaShareMember, 0, len(members))
	saturated := make([]QuotaShareMember, 0, len(members))
	for _, m := range members {
		if m.Saturated {
			saturated = append(saturated, m)
		} else {
			eligible = append(eligible, m)
		}
	}
	if len(eligible) == 0 {
		eligible, saturated = members, nil
	}
	// 1b) Concurrency gating: at-cap members demoted behind
	// members with headroom; fail-open when none has room.
	withRoom := make([]QuotaShareMember, 0, len(eligible))
	atCap := make([]QuotaShareMember, 0, len(eligible))
	for _, m := range eligible {
		if m.AtCap() {
			atCap = append(atCap, m)
		} else {
			withRoom = append(withRoom, m)
		}
	}
	if len(withRoom) == 0 {
		withRoom, atCap = eligible, nil
	}
	// 2) DRR ordering over the with-room set.
	ordered := applyDRR(withRoom, state)
	// 3) P2C between the top two: fewer in-flight wins, ties keep
	// the DRR order.
	winner := ordered[0]
	rest := ordered[1:]
	if len(ordered) >= 2 && ordered[1].Inflight < ordered[0].Inflight {
		winner = ordered[1]
		rest = append([]QuotaShareMember{ordered[0]}, ordered[2:]...)
	}
	full := make([]QuotaShareMember, 0, len(members))
	full = append(full, winner)
	full = append(full, rest...)
	full = append(full, atCap...)
	full = append(full, saturated...)
	return QuotaShareResult{Winner: winner, Ordered: full, OK: true}, nil
}

// applyDRR reorders members by deficit round-robin and mutates
// state: every member gains a quantum equal to its normalized
// share of the total (or 1/n when all shares are 0 — an
// unweighted pool shares evenly); the member with the largest
// deficit wins (ties keep input order) and pays a unit cost of 1,
// so leftover fractional credit carries into the next round.
// It returns a NEW slice with the winner first and the rest in
// input order — the same shape as the reference applyDrr.
func applyDRR(members []QuotaShareMember, state *QuotaShareState) []QuotaShareMember {
	if len(members) <= 1 {
		return append([]QuotaShareMember(nil), members...)
	}
	shares := make([]float64, len(members))
	total := 0.0
	for i, m := range members {
		shares[i] = normalizeShare(m.Share)
		total += shares[i]
	}
	unweighted := total <= 0
	denom := total
	if unweighted {
		denom = float64(len(members))
	}
	for i, m := range members {
		quantum := 1.0 / denom
		if !unweighted {
			quantum = shares[i] / denom
		}
		state.deficits[m.ID] += quantum
	}
	winner := 0
	for i := 1; i < len(members); i++ {
		if state.deficits[members[i].ID] > state.deficits[members[winner].ID] {
			winner = i
		}
	}
	state.deficits[members[winner].ID] -= 1
	out := make([]QuotaShareMember, 0, len(members))
	out = append(out, members[winner])
	for i, m := range members {
		if i != winner {
			out = append(out, m)
		}
	}
	return out
}

// DeficitForTest returns the current DRR deficit for a member ID
// (0 when the member has no accumulated deficit). Tests only.
func (s *QuotaShareState) DeficitForTest(id string) float64 {
	if s == nil {
		return 0
	}
	return s.deficits[id]
}
