package server

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestVerificationFeed pins the honest-status contract for CHECKLIST
// 241-242: the feed serves every backlog item, the status vocabulary is
// exactly the documented one, proven/unproven counts add up, and no
// unproven item is ever reported with a proven status.
func TestVerificationFeed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithVersion("9.9.9-test"))
	rec := doReq(t, srv, "GET", "/dashboard/verification", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/verification: status %d, want 200", rec.Code)
	}
	var feed verificationFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("verification feed is not JSON: %v", err)
	}
	if len(feed.Items) != len(verificationItems) {
		t.Fatalf("feed serves %d items, table has %d", len(feed.Items), len(verificationItems))
	}
	allowed := map[string]bool{
		"proven-in-vm": true, "needs-ci": true, "needs-first-tag": true,
		"needs-live-browser": true, "needs-second-host": true, "needs-docker": true,
	}
	seen := map[string]bool{}
	proven, unproven := 0, 0
	for _, it := range feed.Items {
		if it.ID == "" || it.Title == "" || it.Status == "" || it.Needs == "" || it.Evidence == "" || it.Doc == "" {
			t.Errorf("item %+v has an empty field", it)
		}
		if seen[it.ID] {
			t.Errorf("duplicate item id %q", it.ID)
		}
		seen[it.ID] = true
		if !allowed[it.Status] {
			t.Errorf("item %s has unknown status %q", it.ID, it.Status)
		}
		if it.Status == "proven-in-vm" {
			proven++
			if it.Needs != "—" {
				t.Errorf("proven item %s must need nothing, needs=%q", it.ID, it.Needs)
			}
		} else {
			unproven++
			if it.Needs == "—" || it.Needs == "" {
				t.Errorf("unproven item %s must name what it needs", it.ID)
			}
		}
		if !strings.HasPrefix(it.ID, "LV-") && !strings.HasPrefix(it.ID, "PV-") {
			t.Errorf("item %s: id must start with LV- or PV-", it.ID)
		}
	}
	if feed.Proven != proven || feed.Unproven != unproven {
		t.Errorf("counts: feed says proven=%d unproven=%d, rows say %d/%d",
			feed.Proven, feed.Unproven, proven, unproven)
	}
	if proven == 0 || unproven == 0 {
		t.Errorf("feed must contain both proven and unproven items, got proven=%d unproven=%d", proven, unproven)
	}
	if feed.Note == "" {
		t.Error("feed note must explain the honesty rule")
	}
	// The backlog's own counts, pinned so a silent edit to the table
	// (e.g. flipping an LV item to proven without evidence) fails here.
	if len(feed.Items) != 18 {
		t.Errorf("feed has %d items, want 18 (6 proven + 12 unproven) — update this test and docs/live-verification-backlog.md together", len(feed.Items))
	}
	if proven != 6 || unproven != 12 {
		t.Errorf("proven=%d unproven=%d, want 6/12 — update this test and docs/live-verification-backlog.md together", proven, unproven)
	}
}

// TestVerificationFeedRejectsNonGET pins the method guard.
func TestVerificationFeedRejectsNonGET(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithVersion("9.9.9-test"))
	rec := doReq(t, srv, http.MethodPost, "/dashboard/verification", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /dashboard/verification: status %d, want 405", rec.Code)
	}
}

// TestVerificationItemsMatchBacklogDoc pins that every feed item id
// appears in docs/live-verification-backlog.md, so the feed and the doc
// cannot drift apart silently.
func TestVerificationItemsMatchBacklogDoc(t *testing.T) {
	docBytes, err := os.ReadFile("../../docs/live-verification-backlog.md")
	if err != nil {
		t.Fatalf("cannot read backlog doc: %v", err)
	}
	doc := string(docBytes)
	for _, it := range verificationItems {
		if !strings.Contains(doc, it.ID) {
			t.Errorf("feed item %s (%s) is missing from docs/live-verification-backlog.md", it.ID, it.Title)
		}
	}
	if !strings.Contains(doc, "docs/ci-proof-multarch.md") {
		t.Error("backlog doc must reference the CI proof spec")
	}
}
