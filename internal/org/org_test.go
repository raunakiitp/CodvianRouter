// P3-A: org/team/RBAC/team-key isolation tests.
package org

import (
	"path/filepath"
	"testing"
)

func TestOrgTeamRbacAndIsolation(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "org.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Bootstrap an org (owner).
	if err := st.AddOrg(&Organization{ID: "acme", Name: "Acme"}, RoleOwner); err != nil {
		t.Fatal(err)
	}
	// A viewer cannot create a team.
	if err := st.AddTeam(&Team{ID: "t1", OrgID: "acme", Name: "Core"}, RoleViewer); err == nil {
		t.Fatal("viewer should not be able to create a team")
	}
	// Owner can.
	if err := st.AddTeam(&Team{ID: "t1", OrgID: "acme", Name: "Core", BudgetUSD: 50, AllowedModels: []string{"gpt-5-mini"}}, RoleOwner); err != nil {
		t.Fatal(err)
	}
	// Assign a member role, then confirm role lookup.
	if err := st.SetRole("t1", "alice", RoleAdmin, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if got := st.RoleOf("t1", "alice"); got != RoleAdmin {
		t.Fatalf("roleOf alice = %s, want admin", got)
	}
	// Member (non-manage) cannot assign roles.
	if err := st.SetRole("t1", "bob", RoleMember, RoleMember); err == nil {
		t.Fatal("member should not assign roles")
	}
	// Team-key isolation: bind a key and enforce allowlists.
	if err := st.BindKey("key-abc", "t1", RoleOwner); err != nil {
		t.Fatal(err)
	}
	team := st.TeamForKey("key-abc")
	if team == nil || team.ID != "t1" {
		t.Fatal("team-for-key binding not found")
	}
	if !st.AllowModel("t1", "gpt-5-mini") {
		t.Fatal("team should allow its listed model")
	}
	if st.AllowModel("t1", "claude-4") {
		t.Fatal("team must deny a model outside its allowlist (isolation)")
	}
	if !st.AllowStrategy("t1", "auto/cheap") {
		t.Fatal("team with no strategy allowlist must allow all")
	}
	// Budget: under and over.
	if within, budget, _ := st.SpendWithinBudget("t1", 20); !within || budget != 50 {
		t.Fatal("budget check under limit must pass")
	}
	if within, _, _ := st.SpendWithinBudget("t1", 90); within {
		t.Fatal("budget check over limit must fail")
	}
	// Unbind.
	if err := st.UnbindKey("key-abc", RoleOwner); err != nil {
		t.Fatal(err)
	}
	if st.TeamForKey("key-abc") != nil {
		t.Fatal("key still bound after unbind")
	}
	// Audit log recorded.
	audit := st.Audit(0)
	if len(audit) == 0 {
		t.Fatal("audit log empty")
	}
	actions := map[string]bool{}
	for _, e := range audit {
		actions[e.Action] = true
	}
	for _, want := range []string{"org.create", "team.create", "team.role.set", "key.bind", "key.unbind"} {
		if !actions[want] {
			t.Errorf("audit missing action %q", want)
		}
	}
}

func TestOrgStoreRestartRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "org2.json")
	st, _ := Open(path)
	_ = st.AddOrg(&Organization{ID: "o1", Name: "One"}, RoleOwner)
	_ = st.AddTeam(&Team{ID: "t1", OrgID: "o1", Name: "T"}, RoleOwner)
	// Reopen (restart recovery).
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	teams := st2.Teams()
	if len(teams) != 1 || teams[0].ID != "t1" {
		t.Fatalf("teams did not survive reopen: %+v", teams)
	}
	if len(st2.Audit(0)) == 0 {
		t.Fatal("audit did not survive reopen")
	}
}
