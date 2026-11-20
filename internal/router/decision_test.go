package router

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

func TestStrategyReason(t *testing.T) {
	for _, tc := range []struct {
		strategy string
		want     string
	}{
		{"cheapest", "cheapest"},
		{"fastest", "latency"},
		{"least-latency", "latency"},
		{"most-reliable", "reliability"},
		{"code", "cost-per-solved-task"},
		{"balanced", "balanced"},
		{"weighted", "weighted"},
		{"", ""},
		{"some-future-strategy", "some-future-strategy"},
	} {
		if got := StrategyReason(tc.strategy); got != tc.want {
			t.Errorf("StrategyReason(%q) = %q, want %q", tc.strategy, got, tc.want)
		}
	}
}

func TestDecisionReason(t *testing.T) {
	trace := func(outcomes ...string) *routetrace.RouteTrace {
		tr := &routetrace.RouteTrace{}
		for _, o := range outcomes {
			tr.Attempts = append(tr.Attempts, routetrace.AttemptTrace{Outcome: o})
		}
		return tr
	}
	for _, tc := range []struct {
		name        string
		strategy    string
		trace       *routetrace.RouteTrace
		contract    *routetrace.RoutingContract
		treeOrdered bool
		want        string
	}{
		// Strategy-derived reasons, no trace/contract/tree.
		{"cheapest strategy", "cheapest", nil, nil, false, "cheapest"},
		{"fastest strategy", "fastest", nil, nil, false, "latency"},
		{"least-latency strategy", "least-latency", nil, nil, false, "latency"},
		{"most-reliable strategy", "most-reliable", nil, nil, false, "reliability"},
		{"balanced strategy", "balanced", nil, nil, false, "balanced"},
		// Fallback counting: every non-success, non-hedged attempt
		// before the winner counts — failures and skips alike.
		{"one failure before winner", "cheapest", trace("error"), nil, false, "fallback-after-1"},
		{"two failures before winner", "balanced", trace("rate_limited", "error"), nil, false, "fallback-after-2"},
		{"skip counts like a failure", "balanced", trace("skipped_breaker", "skipped_zero_quota"), nil, false, "fallback-after-2"},
		{"mixed failure and skip", "balanced", trace("error", "skipped_breaker", "rate_limited"), nil, false, "fallback-after-3"},
		// A hedged loser ran in parallel and lost the race: it
		// is not a fallback step.
		{"hedged loser is not a fallback", "balanced", trace("hedged_cancelled"), nil, false, "balanced"},
		{"hedged loser plus one failure", "balanced", trace("hedged_cancelled", "error"), nil, false, "fallback-after-1"},
		// The winner's own success entry (present on the
		// non-streaming path) is not its own fallback step.
		{"winner success entry ignored", "cheapest", trace("success"), nil, false, "cheapest"},
		{"winner success after a failure", "cheapest", trace("error", "success"), nil, false, "fallback-after-1"},
		// Fallback beats every more specific reason.
		{"fallback beats group", "balanced", trace("error"), &routetrace.RoutingContract{Group: "eu"}, false, "fallback-after-1"},
		{"fallback beats contract", "balanced", trace("error"), &routetrace.RoutingContract{Sort: "cheapest"}, false, "fallback-after-1"},
		{"fallback beats tree", "balanced", trace("error"), nil, true, "fallback-after-1"},
		// Group beats contract; contract beats tree.
		{"group", "balanced", nil, &routetrace.RoutingContract{Group: "eu"}, false, "group"},
		{"group beats contract fields", "balanced", nil, &routetrace.RoutingContract{Group: "eu", Sort: "cheapest"}, false, "group"},
		{"contract order", "balanced", nil, &routetrace.RoutingContract{Order: []string{"a", "b"}}, false, "contract"},
		{"contract only", "balanced", nil, &routetrace.RoutingContract{Only: []string{"a"}}, false, "contract"},
		{"contract exclude", "balanced", nil, &routetrace.RoutingContract{Exclude: []string{"a"}}, false, "contract"},
		{"contract sort", "balanced", nil, &routetrace.RoutingContract{Sort: "fastest"}, false, "contract"},
		{"contract max_price", "balanced", nil, &routetrace.RoutingContract{MaxPrice: &routetrace.RoutingMaxPrice{}}, false, "contract"},
		{"contract data_collection", "balanced", nil, &routetrace.RoutingContract{DataCollection: "none"}, false, "contract"},
		{"contract beats tree", "balanced", nil, &routetrace.RoutingContract{Sort: "cheapest"}, true, "contract"},
		{"tree", "balanced", nil, nil, true, "tree"},
		// An empty contract (no shaping fields) is no contract.
		{"empty contract is no contract", "cheapest", nil, &routetrace.RoutingContract{}, false, "cheapest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decisionReason(tc.strategy, tc.trace, tc.contract, tc.treeOrdered); got != tc.want {
				t.Errorf("decisionReason(%q, ...) = %q, want %q", tc.strategy, got, tc.want)
			}
		})
	}
}
