package ledger

import (
	"context"
	"testing"
	"time"
)

func f64ptr(v float64) *float64 { return &v }

func TestTeamCRUD(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := l.SetTeam(ctx, Team{ID: "t1", Name: "Alpha", Description: "eng", MaxBudgetUSD: f64ptr(100), BudgetDuration: "7d"}); err != nil {
		t.Fatal(err)
	}
	got, err := l.GetTeam(ctx, "t1")
	if err != nil || got == nil {
		t.Fatalf("GetTeam = %+v, %v", got, err)
	}
	if got.Name != "Alpha" || got.Status != "active" || got.BudgetDuration != "7d" || *got.MaxBudgetUSD != 100 {
		t.Errorf("team = %+v, want Alpha/active/7d/$100", got)
	}
	if got.BudgetResetAt == nil || got.BudgetResetAt.Before(now) || got.BudgetResetAt.After(now.Add(8*24*time.Hour)) {
		t.Errorf("BudgetResetAt = %v, want now+7d", got.BudgetResetAt)
	}
	// A budgetless team has no window.
	if err := l.SetTeam(ctx, Team{ID: "t2", Name: "Beta"}); err != nil {
		t.Fatal(err)
	}
	got2, _ := l.GetTeam(ctx, "t2")
	if got2.BudgetResetAt != nil || got2.BudgetDuration != "" {
		t.Errorf("budgetless team = %+v, want no window", got2)
	}
	// Update replaces the row; list is in name order.
	if err := l.SetTeam(ctx, Team{ID: "t1", Name: "Alpha", MaxBudgetUSD: f64ptr(250), BudgetDuration: "30d"}); err != nil {
		t.Fatal(err)
	}
	got, _ = l.GetTeam(ctx, "t1")
	if *got.MaxBudgetUSD != 250 || got.BudgetDuration != "30d" {
		t.Errorf("updated team = %+v, want $250/30d", got)
	}
	teams, err := l.ListTeams(ctx)
	if err != nil || len(teams) != 2 || teams[0].Name != "Alpha" || teams[1].Name != "Beta" {
		t.Fatalf("ListTeams = %+v, %v", teams, err)
	}
	// Validation.
	for _, bad := range []Team{
		{ID: "", Name: "x"},
		{ID: "b", Name: ""},
		{ID: "b", Name: "x", MaxBudgetUSD: f64ptr(0), BudgetDuration: "1d"},
		{ID: "b", Name: "x", MaxBudgetUSD: f64ptr(5)},
		{ID: "b", Name: "x", BudgetDuration: "1d"},
		{ID: "b", Name: "x", MaxBudgetUSD: f64ptr(5), BudgetDuration: "year"},
		{ID: "b", Name: "x", Status: "deleted"},
	} {
		if err := l.SetTeam(ctx, bad); err == nil {
			t.Errorf("SetTeam(%+v) = nil, want an error", bad)
		}
	}
	// Delete.
	if err := l.DeleteTeam(ctx, "t2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.GetTeam(ctx, "t2"); got != nil {
		t.Errorf("GetTeam after delete = %+v, want nil", got)
	}
}

func TestTeamMembershipHistory(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := l.SetTeam(ctx, Team{ID: "t1", Name: "Alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetTeam(ctx, Team{ID: "t2", Name: "Beta"}); err != nil {
		t.Fatal(err)
	}
	// Unknown team refuses the assignment.
	if err := l.AssignKeyToTeam(ctx, "k1", "nope", now); err == nil {
		t.Error("assign to unknown team = nil, want an error")
	}
	// Assign both keys to t1.
	if err := l.AssignKeyToTeam(ctx, "k1", "t1", now); err != nil {
		t.Fatal(err)
	}
	if err := l.AssignKeyToTeam(ctx, "k2", "t1", now); err != nil {
		t.Fatal(err)
	}
	// Re-assigning to the same team is a no-op.
	if err := l.AssignKeyToTeam(ctx, "k1", "t1", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	team, err := l.ActiveTeamForKey(ctx, "k1")
	if err != nil || team == nil || team.ID != "t1" {
		t.Fatalf("ActiveTeamForKey(k1) = %+v, %v", team, err)
	}
	members, err := l.TeamMembers(ctx, "t1")
	if err != nil || len(members) != 2 || members[0].KeyID != "k1" || members[1].KeyID != "k2" {
		t.Fatalf("TeamMembers(t1) = %+v, %v", members, err)
	}
	// Reassign k1 to t2: the old binding closes, history keeps the
	// moment of the move.
	later := now.Add(time.Hour)
	if err := l.AssignKeyToTeam(ctx, "k1", "t2", later); err != nil {
		t.Fatal(err)
	}
	if team, _ := l.ActiveTeamForKey(ctx, "k1"); team == nil || team.ID != "t2" {
		t.Fatalf("ActiveTeamForKey(k1) after move = %+v, want t2", team)
	}
	if at, _ := l.TeamIDForKeyAt(ctx, "k1", now.Add(30*time.Minute)); at != "t1" {
		t.Errorf("TeamIDForKeyAt(k1, before move) = %q, want t1", at)
	}
	if at, _ := l.TeamIDForKeyAt(ctx, "k1", later.Add(time.Minute)); at != "t2" {
		t.Errorf("TeamIDForKeyAt(k1, after move) = %q, want t2", at)
	}
	if members, _ := l.TeamMembers(ctx, "t1"); len(members) != 1 || members[0].KeyID != "k2" {
		t.Errorf("TeamMembers(t1) after move = %+v, want only k2", members)
	}
	// Unassign: true once, false the second time.
	if ok, err := l.UnassignKeyFromTeam(ctx, "k2", later); err != nil || !ok {
		t.Fatalf("UnassignKeyFromTeam(k2) = %v, %v", ok, err)
	}
	if ok, _ := l.UnassignKeyFromTeam(ctx, "k2", later); ok {
		t.Error("second unassign = true, want false")
	}
	if team, _ := l.ActiveTeamForKey(ctx, "k2"); team != nil {
		t.Errorf("ActiveTeamForKey(k2) after unassign = %+v, want nil", team)
	}
	// Archived teams refuse new members.
	if err := l.SetTeam(ctx, Team{ID: "t1", Name: "Alpha", Status: "archived"}); err != nil {
		t.Fatal(err)
	}
	if err := l.AssignKeyToTeam(ctx, "k2", "t1", later); err == nil {
		t.Error("assign to archived team = nil, want an error")
	}
}

func TestTeamBudgetWindowAdvance(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// A window that ended 25 hours ago (1d duration) rolls forward
	// by whole days until it is in the future.
	past := now.Add(-25 * time.Hour)
	if err := l.SetTeam(ctx, Team{ID: "t1", Name: "Alpha", MaxBudgetUSD: f64ptr(10), BudgetDuration: "1d", BudgetResetAt: &past}); err != nil {
		t.Fatal(err)
	}
	team, _ := l.GetTeam(ctx, "t1")
	advanced, err := l.AdvanceTeamBudgetWindow(ctx, *team, now)
	if err != nil {
		t.Fatal(err)
	}
	if !advanced.BudgetResetAt.After(now) {
		t.Errorf("advanced reset = %v, want after now", advanced.BudgetResetAt)
	}
	if d := advanced.BudgetResetAt.Sub(past); d < 47*time.Hour || d > 49*time.Hour {
		t.Errorf("advanced reset moved by %v, want ~48h (two whole 1d steps; the stored end is millisecond-truncated)", d)
	}
	// Persisted.
	stored, _ := l.GetTeam(ctx, "t1")
	if !stored.BudgetResetAt.Equal(*advanced.BudgetResetAt) {
		t.Errorf("stored reset = %v, want %v", stored.BudgetResetAt, advanced.BudgetResetAt)
	}
	// An open window does not move.
	team, _ = l.GetTeam(ctx, "t1")
	same, err := l.AdvanceTeamBudgetWindow(ctx, *team, now)
	if err != nil || !same.BudgetResetAt.Equal(*team.BudgetResetAt) {
		t.Errorf("open window moved: %v -> %v, %v", team.BudgetResetAt, same.BudgetResetAt, err)
	}
	// A budgetless team is untouched.
	if err := l.SetTeam(ctx, Team{ID: "t2", Name: "Beta"}); err != nil {
		t.Fatal(err)
	}
	team2, _ := l.GetTeam(ctx, "t2")
	untouched, err := l.AdvanceTeamBudgetWindow(ctx, *team2, now)
	if err != nil || untouched.BudgetResetAt != nil {
		t.Errorf("budgetless advance = %+v, %v", untouched, err)
	}
	// Window bounds: [reset-1d, reset).
	start, end := TeamWindow(*stored)
	if !end.Equal(*stored.BudgetResetAt) || !start.Equal(end.Add(-24*time.Hour)) {
		t.Errorf("TeamWindow = %v..%v, want end=%v start=end-24h", start, end, stored.BudgetResetAt)
	}
}

func TestTeamUsageSlicesAttribution(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := l.SetTeam(ctx, Team{ID: "t1", Name: "Alpha", MaxBudgetUSD: f64ptr(100), BudgetDuration: "1d"}); err != nil {
		t.Fatal(err)
	}
	if err := l.AssignKeyToTeam(ctx, "k1", "t1", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	prices := Prices{PriceKey{Provider: "openai", Model: "m"}: ModelPrice{PromptPerToken: 0.00001, CompletionPerToken: 0.00003}}
	record := func(key string, prompt, completion int) {
		t.Helper()
		if _, err := l.RecordUsage(ctx, Usage{
			GatewayKeyID: key, Provider: "openai", Model: "m",
			PromptTokens: prompt, CompletionTokens: completion, SplitKnown: true, OK: true,
		}, prices); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}
	record("k1", 1000, 1000) // team member: $0.01 + $0.03 = $0.04
	record("k2", 1000, 1000) // no team: must never land in t1
	// A failed row from a member is excluded too.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "k1", Provider: "openai", Model: "m",
		PromptTokens: 1000, CompletionTokens: 1000, SplitKnown: true, OK: false,
	}, prices); err != nil {
		t.Fatal(err)
	}

	start := now.Add(-time.Hour)
	end := now.Add(time.Hour)
	slices, err := l.UsageSlicesForTeam(ctx, "t1", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(slices) != 1 {
		t.Fatalf("team slices = %+v, want 1", slices)
	}
	s := slices[0]
	if s.Rows != 1 || s.PromptTokens != 1000 || s.CompletionTokens != 1000 || s.CostUSD != 0.04 {
		t.Errorf("team slice = %+v, want 1 row / 1000+1000 / $0.04", s)
	}
	if !s.PriceKnown || !s.SplitKnown {
		t.Errorf("team slice priced/split = %v/%v, want true/true", s.PriceKnown, s.SplitKnown)
	}
	// Per-member breakdown: only k1 appears.
	members, err := l.TeamMemberSlices(ctx, "t1", start, end)
	if err != nil || len(members) != 1 || members[0].KeyID != "k1" || members[0].Rows != 1 {
		t.Fatalf("member slices = %+v, %v", members, err)
	}
	// Reassignment moves FUTURE rows only: k1 leaves t1, spends
	// again, and the new row is not in t1's window.
	if _, err := l.UnassignKeyFromTeam(ctx, "k1", now); err != nil {
		t.Fatal(err)
	}
	record("k1", 500, 500)
	slices, _ = l.UsageSlicesForTeam(ctx, "t1", start, end)
	if len(slices) != 1 || slices[0].Rows != 1 {
		t.Errorf("team slices after unassign = %+v, want the original 1 row only", slices)
	}
}
