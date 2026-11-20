package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestResponsesBranching is the CHECKLIST 237-238 end-to-end gate:
// a root response branched twice — both children keep the parent's
// history (parent input + parent reply + their own turn), the
// parent retrieve names both children, the tree endpoint shows
// root → [child1, child2], an unknown parent 404s naming the id,
// and deleting a node prunes it from the tree (its children render
// as roots with a missing parent, never re-parented).
func TestResponsesBranching(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { led.Close() })

	eng := &captureEngine{resp: provider.ChatResponse{
		Content: "root reply", Model: "m",
		Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithLedger(led), WithTimeout(10*time.Second))

	// Root create.
	rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"first"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("root create: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	root := decodeResponsesBody(t, rec)
	if got := eng.lastReq().Messages; len(got) != 1 || got[0].Content != "first" {
		t.Fatalf("root engine messages = %+v, want [user first]", got)
	}

	// Branch 1 off the root.
	eng.resp = provider.ChatResponse{Content: "child1 reply", Model: "m",
		Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}
	rec = doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"second","previous_response_id":"`+root.ID+`"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("branch 1: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	child1 := decodeResponsesBody(t, rec)
	if child1.PreviousResponseID == nil || *child1.PreviousResponseID != root.ID {
		t.Fatalf("branch 1 previous_response_id = %v, want %s", child1.PreviousResponseID, root.ID)
	}
	if got := eng.lastReq().Messages; len(got) != 3 ||
		got[0].Content != "first" || got[1].Content != "root reply" || got[2].Content != "second" {
		t.Fatalf("branch 1 engine messages = %+v, want [user first, assistant root reply, user second]", got)
	}

	// Branch 2 off the same root: its own transcript copy, still
	// carrying the parent's history — not child1's.
	eng.resp = provider.ChatResponse{Content: "child2 reply", Model: "m",
		Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}
	rec = doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"third","previous_response_id":"`+root.ID+`"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("branch 2: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	child2 := decodeResponsesBody(t, rec)
	if child2.ID == child1.ID {
		t.Fatal("branch 2 id == branch 1 id: two branches must mint distinct ids")
	}
	if got := eng.lastReq().Messages; len(got) != 3 ||
		got[0].Content != "first" || got[1].Content != "root reply" || got[2].Content != "third" {
		t.Fatalf("branch 2 engine messages = %+v, want [user first, assistant root reply, user third]", got)
	}
	if got := eng.lastReq().PreviousResponseID; got != root.ID {
		t.Fatalf("branch 2 engine PreviousResponseID = %q, want %s", got, root.ID)
	}

	// Unknown parent: honest 404 naming the id, never a silent
	// fresh start.
	rec = doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"x","previous_response_id":"resp_nope"}`, "test-key-123")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown parent: status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}

	// Parent retrieve names both children.
	rec = doReq(t, srv, http.MethodGet, "/v1/responses/"+root.ID, "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("retrieve root: status = %d, want 200", rec.Code)
	}
	rootGot := decodeResponsesBody(t, rec)
	if len(rootGot.ChildResponseIDs) != 2 || rootGot.ChildResponseIDs[0] != child1.ID || rootGot.ChildResponseIDs[1] != child2.ID {
		t.Fatalf("root child_response_ids = %v, want [%s %s]", rootGot.ChildResponseIDs, child1.ID, child2.ID)
	}
	// A leaf retrieve carries no child_response_ids field at all.
	rec = doReq(t, srv, http.MethodGet, "/v1/responses/"+child1.ID, "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("retrieve child1: status = %d, want 200", rec.Code)
	}
	child1Got := decodeResponsesBody(t, rec)
	if len(child1Got.ChildResponseIDs) != 0 {
		t.Fatalf("child1 child_response_ids = %v, want none", child1Got.ChildResponseIDs)
	}

	// Tree endpoint: root → [child1, child2].
	tree := getResponseTree(t, srv)
	if tree.Total != 3 || tree.Branched != 2 {
		t.Fatalf("tree total/branched = %d/%d, want 3/2", tree.Total, tree.Branched)
	}
	if len(tree.Roots) != 1 || tree.Roots[0].ID != root.ID {
		t.Fatalf("tree roots = %v, want [root]", tree.Roots)
	}
	if kids := tree.Roots[0].Children; len(kids) != 2 || kids[0].ID != child1.ID || kids[1].ID != child2.ID {
		t.Fatalf("tree root children = %v, want [child1 child2]", kids)
	}

	// The ledger holds the tree durably: the nodes survive even
	// though the store is the continuation source of truth.
	if _, ok, err := led.ResponseTreeNode(root.ID); err != nil || !ok {
		t.Fatalf("ledger node root ok=%v err=%v, want found", ok, err)
	}
	if kids, err := led.ResponseTreeChildren(root.ID); err != nil || len(kids) != 2 {
		t.Fatalf("ledger children of root = %v (err %v), want 2", kids, err)
	}

	// Delete child1: pruned from the tree, root keeps child2.
	rec = doReq(t, srv, http.MethodDelete, "/v1/responses/"+child1.ID, "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete child1: status = %d, want 200", rec.Code)
	}
	if _, ok, err := led.ResponseTreeNode(child1.ID); err != nil || ok {
		t.Fatalf("ledger node child1 after delete ok=%v err=%v, want a miss", ok, err)
	}
	tree = getResponseTree(t, srv)
	if tree.Total != 2 || len(tree.Roots) != 1 || len(tree.Roots[0].Children) != 1 || tree.Roots[0].Children[0].ID != child2.ID {
		t.Fatalf("tree after child1 delete = %+v, want root → [child2]", tree)
	}

	// Delete the root: child2 renders as a root with a missing
	// parent, its parent_id still naming the deleted root — never
	// silently re-parented, never dropped.
	rec = doReq(t, srv, http.MethodDelete, "/v1/responses/"+root.ID, "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete root: status = %d, want 200", rec.Code)
	}
	tree = getResponseTree(t, srv)
	if tree.Total != 1 || len(tree.Roots) != 1 || tree.Roots[0].ID != child2.ID {
		t.Fatalf("tree after root delete = %+v, want [child2] as root", tree)
	}
	if !tree.Roots[0].ParentMissing || tree.Roots[0].ParentID != root.ID {
		t.Fatalf("orphaned child2 = %+v, want parent_missing with parent_id %s", tree.Roots[0], root.ID)
	}
}

// TestResponsesTreeEmptyAndStoreFallback proves the honest empty
// state and the no-ledger fallback: a fresh server's tree endpoint
// is roots [] with total 0, and without a ledger the tree is built
// from the in-memory store and still shows root → child.
func TestResponsesTreeEmptyAndStoreFallback(t *testing.T) {
	eng := &captureEngine{resp: provider.ChatResponse{
		Content: "hi", Model: "m",
		Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()))

	tree := getResponseTree(t, srv)
	if tree.Total != 0 || tree.Branched != 0 || len(tree.Roots) != 0 {
		t.Fatalf("empty tree = %+v, want roots [] total 0 branched 0", tree)
	}

	rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"first"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("root create: status = %d, want 200", rec.Code)
	}
	root := decodeResponsesBody(t, rec)
	rec = doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"second","previous_response_id":"`+root.ID+`"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("branch: status = %d, want 200", rec.Code)
	}
	child := decodeResponsesBody(t, rec)

	tree = getResponseTree(t, srv)
	if tree.Total != 2 || tree.Branched != 1 || len(tree.Roots) != 1 || tree.Roots[0].ID != root.ID {
		t.Fatalf("store-fallback tree = %+v, want root → [child]", tree)
	}
	if kids := tree.Roots[0].Children; len(kids) != 1 || kids[0].ID != child.ID {
		t.Fatalf("store-fallback root children = %v, want [%s]", kids, child.ID)
	}
	// Retrieve names the child even without a ledger.
	rec = doReq(t, srv, http.MethodGet, "/v1/responses/"+root.ID, "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("retrieve root: status = %d, want 200", rec.Code)
	}
	if kids := decodeResponsesBody(t, rec).ChildResponseIDs; len(kids) != 1 || kids[0] != child.ID {
		t.Fatalf("root child_response_ids = %v, want [%s]", kids, child.ID)
	}
}

// TestBuildResponseTreeFeed is the unit gate for the tree builder:
// missing parents become flagged roots, an empty input is an empty
// (non-nil) forest, and a self-parenting node is a root, never
// dropped or recursed into.
func TestBuildResponseTreeFeed(t *testing.T) {
	feed := buildResponseTreeFeed(nil)
	if feed.Total != 0 || feed.Branched != 0 || feed.Roots == nil || len(feed.Roots) != 0 {
		t.Fatalf("empty feed = %+v, want roots [] total 0", feed)
	}
	feed = buildResponseTreeFeed([]ledger.ResponseTreeNode{
		{ID: "resp_orphan", ParentID: "resp_gone", Model: "m", Status: "completed", CreatedAt: 1},
		{ID: "resp_self", ParentID: "resp_self", Model: "m", Status: "completed", CreatedAt: 2},
		{ID: "resp_root", Model: "m", Status: "completed", CreatedAt: 3},
		{ID: "resp_kid", ParentID: "resp_root", Model: "m", Status: "completed", CreatedAt: 4},
	})
	if feed.Total != 4 || feed.Branched != 3 {
		t.Fatalf("feed total/branched = %d/%d, want 4/3", feed.Total, feed.Branched)
	}
	if len(feed.Roots) != 3 {
		t.Fatalf("roots = %d, want 3 (orphan + self-parent + root)", len(feed.Roots))
	}
	if !feed.Roots[0].ParentMissing || feed.Roots[0].ID != "resp_orphan" {
		t.Fatalf("root 0 = %+v, want the orphan flagged parent_missing", feed.Roots[0])
	}
	if !feed.Roots[1].ParentMissing || feed.Roots[1].ID != "resp_self" {
		t.Fatalf("root 1 = %+v, want the self-parent flagged parent_missing", feed.Roots[1])
	}
	if len(feed.Roots[2].Children) != 1 || feed.Roots[2].Children[0].ID != "resp_kid" {
		t.Fatalf("root children = %v, want [resp_kid]", feed.Roots[2].Children)
	}
}

func getResponseTree(t *testing.T, srv *Server) responseTreeFeed {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/dashboard/responses/tree", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/responses/tree: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var feed responseTreeFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode tree feed: %v (body %s)", err, rec.Body.String())
	}
	return feed
}
