package ledger

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestResponseTreeBranching proves the CHECKLIST 237 tree index:
// a root with two children (a branch) and a grandchild, children
// listed in creation order, a node's parent read back, an upsert
// updating a stream's terminal status, and delete removing only
// the deleted node (its children keep naming it as parent).
func TestResponseTreeBranching(t *testing.T) {
	l := openTestLedger(t)

	root := ResponseTreeNode{ID: "resp_root", Model: "m", Provider: "p1", Status: "completed", CreatedAt: 100}
	kid1 := ResponseTreeNode{ID: "resp_kid1", ParentID: "resp_root", Model: "m", Provider: "p1", Status: "completed", CreatedAt: 101}
	kid2 := ResponseTreeNode{ID: "resp_kid2", ParentID: "resp_root", Model: "m", Provider: "p2", Status: "completed", CreatedAt: 102, Stream: true}
	grand := ResponseTreeNode{ID: "resp_grand", ParentID: "resp_kid1", Model: "m", Provider: "p1", Status: "completed", CreatedAt: 103}
	for _, n := range []ResponseTreeNode{root, kid1, kid2, grand} {
		if err := l.RecordResponseTree(n); err != nil {
			t.Fatalf("RecordResponseTree(%s): %v", n.ID, err)
		}
	}

	kids, err := l.ResponseTreeChildren("resp_root")
	if err != nil {
		t.Fatalf("ResponseTreeChildren: %v", err)
	}
	if len(kids) != 2 || kids[0].ID != "resp_kid1" || kids[1].ID != "resp_kid2" {
		t.Fatalf("children of root = %v, want [resp_kid1 resp_kid2] in creation order", kids)
	}
	if !kids[1].Stream {
		t.Fatalf("kid2 stream = false, want true (the stream flag is stored)")
	}
	gkids, err := l.ResponseTreeChildren("resp_kid1")
	if err != nil || len(gkids) != 1 || gkids[0].ID != "resp_grand" {
		t.Fatalf("children of kid1 = %v (err %v), want [resp_grand]", gkids, err)
	}
	leafKids, err := l.ResponseTreeChildren("resp_grand")
	if err != nil || len(leafKids) != 0 {
		t.Fatalf("children of grand = %v (err %v), want none", leafKids, err)
	}

	node, ok, err := l.ResponseTreeNode("resp_kid2")
	if err != nil || !ok || node.ParentID != "resp_root" || node.Provider != "p2" {
		t.Fatalf("node resp_kid2 = %+v ok=%v err=%v, want parent resp_root provider p2", node, ok, err)
	}
	if _, ok, err := l.ResponseTreeNode("resp_nope"); err != nil || ok {
		t.Fatalf("node resp_nope ok=%v err=%v, want a miss", ok, err)
	}

	// Upsert: a stream's in_progress placeholder re-recorded with
	// its terminal status keeps one row, latest status wins.
	if err := l.RecordResponseTree(ResponseTreeNode{ID: "resp_live", Model: "m", Status: "in_progress", CreatedAt: 104, Stream: true}); err != nil {
		t.Fatalf("RecordResponseTree live: %v", err)
	}
	if err := l.RecordResponseTree(ResponseTreeNode{ID: "resp_live", Model: "m", Provider: "p1", Status: "completed", CreatedAt: 104, Stream: true}); err != nil {
		t.Fatalf("RecordResponseTree live terminal: %v", err)
	}
	live, ok, err := l.ResponseTreeNode("resp_live")
	if err != nil || !ok || live.Status != "completed" || live.Provider != "p1" {
		t.Fatalf("node resp_live = %+v ok=%v err=%v, want completed/p1", live, ok, err)
	}

	all, err := l.ResponseTreeAll(0)
	if err != nil || len(all) != 5 {
		t.Fatalf("ResponseTreeAll = %d nodes (err %v), want 5", len(all), err)
	}
	if all[0].ID != "resp_root" || all[len(all)-1].ID != "resp_live" {
		t.Fatalf("ResponseTreeAll order = %v, want oldest first", all)
	}
	one, err := l.ResponseTreeAll(1)
	if err != nil || len(one) != 1 || one[0].ID != "resp_root" {
		t.Fatalf("ResponseTreeAll(1) = %v (err %v), want [resp_root]", one, err)
	}

	// Delete removes only the deleted node: the children keep
	// naming it as parent (the tree endpoint renders them as
	// roots with a missing parent — never silently re-parented).
	if err := l.DeleteResponseTree("resp_kid1"); err != nil {
		t.Fatalf("DeleteResponseTree: %v", err)
	}
	if _, ok, err := l.ResponseTreeNode("resp_kid1"); err != nil || ok {
		t.Fatalf("node resp_kid1 after delete ok=%v err=%v, want a miss", ok, err)
	}
	grand2, ok, err := l.ResponseTreeNode("resp_grand")
	if err != nil || !ok || grand2.ParentID != "resp_kid1" {
		t.Fatalf("node resp_grand after parent delete = %+v ok=%v err=%v, want parent still resp_kid1", grand2, ok, err)
	}
	// Deleting an unknown id is a no-op, never an error.
	if err := l.DeleteResponseTree("resp_nope"); err != nil {
		t.Fatalf("DeleteResponseTree unknown: %v", err)
	}
}

// TestResponseTreeMigratesOldDatabase proves the CHECKLIST 237
// migration: a database created by an older build (no
// response_tree table at all) gains the table, empty, on first
// Open — and tree writes/reads work against it immediately.
func TestResponseTreeMigratesOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	oldSchema := `CREATE TABLE usage_events (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                INTEGER NOT NULL,
  gateway_key_id    TEXT NOT NULL,
  provider          TEXT NOT NULL,
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd          REAL NOT NULL DEFAULT 0,
  ok                INTEGER NOT NULL DEFAULT 1,
  latency_ms        INTEGER NOT NULL DEFAULT 0
);`
	if _, err := db.Exec(oldSchema); err != nil {
		db.Close()
		t.Fatalf("create old schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open old db: %v", err)
	}
	defer l.Close()

	var count int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='response_tree'`).Scan(&count); err != nil {
		t.Fatalf("check response_tree table: %v", err)
	}
	if count != 1 {
		t.Fatalf("response_tree table count = %d, want 1 (migration did not create it)", count)
	}
	// No backfill: the migrated table starts empty.
	all, err := l.ResponseTreeAll(0)
	if err != nil || len(all) != 0 {
		t.Fatalf("ResponseTreeAll on migrated db = %d (err %v), want 0", len(all), err)
	}
	if err := l.RecordResponseTree(ResponseTreeNode{ID: "resp_a", Model: "m", Status: "completed", CreatedAt: 1}); err != nil {
		t.Fatalf("RecordResponseTree on migrated db: %v", err)
	}
	if _, ok, err := l.ResponseTreeNode("resp_a"); err != nil || !ok {
		t.Fatalf("node resp_a on migrated db ok=%v err=%v, want found", ok, err)
	}
}
