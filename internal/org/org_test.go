// P3-A: org/team/RBAC/team-key isolation tests.
package org

import (
	"os"
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

// TestOrgStorePersistIsAtomicRename pins the same write discipline as
// the eval store: a temp file is RENAMED over the target, never written
// in place. An in-place rewrite could tear the org/team snapshot on a
// crash (AUD-REL-003, mirrored). A hard link to the committed file is
// the observable: an atomic rename leaves the link's bytes unchanged,
// an in-place os.WriteFile changes them.
func TestOrgStorePersistIsAtomicRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "org.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddOrg(&Organization{ID: "o1", Name: "One"}, RoleOwner); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "org.link")
	if err := os.Link(path, link); err != nil {
		t.Fatalf("hard-link the committed store: %v", err)
	}
	before, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read link: %v", err)
	}
	if err := st.AddTeam(&Team{ID: "t1", OrgID: "o1", Name: "T"}, RoleOwner); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read link after second persist: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("org store persist rewrote the file in place; expected an atomic temp-file + rename")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "org.json" && e.Name() != "org.link" {
			t.Fatalf("unexpected leftover file after persist: %q", e.Name())
		}
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after atomic persist: %v", err)
	}
	if len(st2.Teams()) != 1 {
		t.Fatalf("teams = %d, want 1 after atomic persist", len(st2.Teams()))
	}
}

// TestOrgRecordDenialLogsPersistFailure pins the no-silent-drop rule: a
// denial whose audit record cannot be persisted must be surfaced, not
// swallowed.
func TestOrgRecordDenialLogsPersistFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "org.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddOrg(&Organization{ID: "o1", Name: "One"}, RoleOwner); err != nil {
		t.Fatal(err)
	}
	orig := orgLogf
	logged := false
	orgLogf = func(string, ...any) { logged = true }
	t.Cleanup(func() { orgLogf = orig })
	// Sabotage: replace the store file with a directory so persist fails.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	st.RecordDenial("key-1", "t1", "model denied by team allowlist")
	if !logged {
		t.Fatal("RecordDenial swallowed a persist failure instead of logging it")
	}
}
