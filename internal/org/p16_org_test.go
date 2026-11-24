package org

import (
	"path/filepath"
	"testing"
)

// P1.6 additions: org resolution from a team, org-scoped team listing,
// and denial records written to the append-only audit log and persisted.

func TestOrgOfResolvesTeamToOrg(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddOrg(&Organization{ID: "o1", Name: "Acme"}, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeam(&Team{ID: "t1", OrgID: "o1", Name: "Alpha"}, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if org := s.OrgOf("t1"); org == nil || org.ID != "o1" {
		t.Fatalf("OrgOf(t1) = %+v, want o1", org)
	}
	if org := s.OrgOf("nope"); org != nil {
		t.Fatalf("OrgOf(unknown) = %+v, want nil", org)
	}
	// A team cannot be created under an unknown org: the store refuses,
	// so OrgOf can never report a team whose org does not exist.
	if err := s.AddTeam(&Team{ID: "t2", OrgID: "ghost", Name: "Ghost"}, RoleOwner); err == nil {
		t.Fatal("AddTeam accepted a team under an unknown org")
	}
	if org := s.OrgOf("t2"); org != nil {
		t.Fatalf("OrgOf(t2) = %+v, want nil (team was never created)", org)
	}
}

func TestTeamsForOrgIsScopedAndSorted(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"o1", "o2"} {
		if err := s.AddOrg(&Organization{ID: id, Name: id}, RoleOwner); err != nil {
			t.Fatal(err)
		}
	}
	for _, tm := range []Team{
		{ID: "zeta", OrgID: "o1", Name: "Zeta"},
		{ID: "alpha", OrgID: "o1", Name: "Alpha"},
		{ID: "other", OrgID: "o2", Name: "Other"},
	} {
		tm := tm
		if err := s.AddTeam(&tm, RoleOwner); err != nil {
			t.Fatal(err)
		}
	}
	got := s.TeamsForOrg("o1")
	if len(got) != 2 || got[0].ID != "alpha" || got[1].ID != "zeta" {
		t.Fatalf("TeamsForOrg(o1) = %+v, want alpha then zeta", got)
	}
	if len(s.TeamsForOrg("o2")) != 1 {
		t.Fatal("TeamsForOrg(o2) must not leak o1's teams")
	}
	if len(s.TeamsForOrg("missing")) != 0 {
		t.Fatal("TeamsForOrg(unknown) must be empty")
	}
}

func TestRecordDenialIsAuditedAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "org.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddOrg(&Organization{ID: "o1", Name: "Acme"}, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeam(&Team{ID: "t1", OrgID: "o1", Name: "Alpha"}, RoleOwner); err != nil {
		t.Fatal(err)
	}
	s.RecordDenial("key-1", "t1", "model \"x\" denied by team allowlist")

	entries := s.Audit(10)
	found := false
	for _, e := range entries {
		if e.Action == "request.denied" && e.Actor == "key-1" && e.Target == "t1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit log has no denial entry: %+v", entries)
	}

	// Persistence: a fresh store on the same path sees it.
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted := false
	for _, e := range reopened.Audit(10) {
		if e.Action == "request.denied" {
			persisted = true
		}
	}
	if !persisted {
		t.Fatal("denial entry did not survive reopen")
	}

	// Nothing recorded when there is neither a key nor a team.
	before := len(reopened.Audit(0))
	reopened.RecordDenial("", "", "nothing to attribute")
	if after := len(reopened.Audit(0)); after != before {
		t.Fatalf("RecordDenial with no actor recorded %d entries, want 0", after-before)
	}
}
