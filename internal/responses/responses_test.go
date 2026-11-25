package responses

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNewIDShape(t *testing.T) {
	a, b := NewID(), NewID()
	for _, id := range []string{a, b} {
		if !strings.HasPrefix(id, "resp_") || len(id) != 29 {
			t.Fatalf("id = %q, want resp_ + 24 hex chars", id)
		}
	}
	if a == b {
		t.Fatalf("two minted ids collided: %q", a)
	}
	m := NewMsgID()
	if !strings.HasPrefix(m, "msg_") || len(m) != 28 {
		t.Fatalf("msg id = %q, want msg_ + 24 hex chars", m)
	}
}

func TestStorePutGetRoundTrip(t *testing.T) {
	s := NewStore(time.Hour, 10)
	rec := Record{
		ID:             "resp_abc",
		Model:          "gpt-4o",
		ProviderID:     "prov_1",
		Status:         StatusCompleted,
		CreatedAt:      123,
		TranscriptJSON: json.RawMessage(`[{"role":"user","content":"hi"}]`),
		ResponseJSON:   json.RawMessage(`{"id":"resp_abc"}`),
	}
	s.Put(rec)
	got, ok := s.Get("resp_abc", "")
	if !ok {
		t.Fatal("Get after Put: not found")
	}
	if got.Model != "gpt-4o" || got.ProviderID != "prov_1" || got.Status != StatusCompleted {
		t.Fatalf("Get = %+v, want the stored record", got)
	}
	if string(got.TranscriptJSON) != `[{"role":"user","content":"hi"}]` {
		t.Fatalf("transcript = %s, want the stored bytes", got.TranscriptJSON)
	}
	if _, ok := s.Get("resp_missing", ""); ok {
		t.Fatal("Get of an unknown id found a record")
	}
	if st := s.Stats(); st.Stored != 1 || st.Created != 1 || st.Continuations != 0 {
		t.Fatalf("Stats = %+v, want stored 1, created 1, continuations 0", st)
	}
}

func TestStorePutReplacesWithoutGrowing(t *testing.T) {
	s := NewStore(time.Hour, 10)
	s.Put(Record{ID: "resp_1", Model: "m", Status: StatusInProgress})
	s.Put(Record{ID: "resp_1", Model: "m", Status: StatusCompleted})
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (a stream placeholder updated in place)", s.Len())
	}
	if st := s.Stats(); st.Created != 1 {
		t.Fatalf("Created = %d, want 1", st.Created)
	}
	got, ok := s.Get("resp_1", "")
	if !ok || got.Status != StatusCompleted {
		t.Fatalf("Get = %+v ok=%v, want completed", got, ok)
	}
}

func TestStoreContinuationCounted(t *testing.T) {
	s := NewStore(time.Hour, 10)
	s.Put(Record{ID: "resp_1", Model: "m", Status: StatusCompleted})
	s.Put(Record{ID: "resp_2", Model: "m", Status: StatusCompleted, PreviousID: "resp_1"})
	if st := s.Stats(); st.Continuations != 1 {
		t.Fatalf("Continuations = %d, want 1", st.Continuations)
	}
}

func TestStoreExpiry(t *testing.T) {
	s := NewStore(10*time.Millisecond, 10)
	s.Put(Record{ID: "resp_1", Model: "m", Status: StatusCompleted})
	time.Sleep(25 * time.Millisecond)
	if _, ok := s.Get("resp_1", ""); ok {
		t.Fatal("expired record still returned")
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d after expiry, want 0", s.Len())
	}
}

func TestStoreCapEvictsOldest(t *testing.T) {
	s := NewStore(time.Hour, 3)
	for _, id := range []string{"resp_1", "resp_2", "resp_3", "resp_4"} {
		s.Put(Record{ID: id, Model: "m", Status: StatusCompleted})
	}
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3 (cap)", s.Len())
	}
	if _, ok := s.Get("resp_1", ""); ok {
		t.Fatal("oldest record survived the cap")
	}
	if _, ok := s.Get("resp_4", ""); !ok {
		t.Fatal("newest record evicted")
	}
	if st := s.Stats(); st.Evicted != 1 {
		t.Fatalf("Evicted = %d, want 1", st.Evicted)
	}
}

func TestStoreRecentMostRecentFirstNoContent(t *testing.T) {
	s := NewStore(time.Hour, 10)
	s.Put(Record{ID: "resp_1", Model: "m", Status: StatusCompleted, TranscriptJSON: json.RawMessage(`[{"role":"user","content":"secret prompt"}]`), ResponseJSON: json.RawMessage(`{"id":"resp_1","output":"secret answer"}`)})
	s.Put(Record{ID: "resp_2", Model: "m", Status: StatusCompleted, PreviousID: "resp_1"})
	recent := s.Recent(10)
	if len(recent) != 2 || recent[0].ID != "resp_2" || recent[1].ID != "resp_1" {
		t.Fatalf("Recent = %+v, want resp_2 then resp_1", recent)
	}
	for _, rec := range recent {
		if rec.TranscriptJSON != nil || rec.ResponseJSON != nil {
			t.Fatalf("Recent record %s carries content bytes — the feed must never see them", rec.ID)
		}
	}
	if got := s.Recent(1); len(got) != 1 || got[0].ID != "resp_2" {
		t.Fatalf("Recent(1) = %+v, want only resp_2", got)
	}
}

func TestStoreNilSafe(t *testing.T) {
	var s *Store
	s.Put(Record{ID: "resp_1"})
	if _, ok := s.Get("resp_1", ""); ok {
		t.Fatal("nil store returned a record")
	}
	if len(s.Recent(5)) != 0 || s.Len() != 0 || s.Stats().Stored != 0 {
		t.Fatal("nil store reported state")
	}
	if s.Delete("resp_1", "") {
		t.Fatal("nil store deleted something")
	}
	if page, total := s.Page(0, 10, ""); len(page) != 0 || total != 0 {
		t.Fatalf("nil store Page = %d records / total %d, want 0/0", len(page), total)
	}
	if _, ok := s.IndexOf("resp_1"); ok {
		t.Fatal("nil store resolved a cursor")
	}
}

// TestStoreDelete is the store gate for CHECKLIST 204: deleting
// removes the record (Get misses, continuations cannot resume it),
// a second delete of the same id is false (never a silent
// success), and deleting an id that never existed is false.
func TestStoreDelete(t *testing.T) {
	s := NewStore(time.Hour, 10)
	s.Put(Record{ID: "resp_1", Model: "m", Status: StatusCompleted})
	s.Put(Record{ID: "resp_2", Model: "m", Status: StatusCompleted})
	if !s.Delete("resp_1", "") {
		t.Fatal("Delete(resp_1) = false, want true")
	}
	if _, ok := s.Get("resp_1", ""); ok {
		t.Fatal("deleted record still retrievable")
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1 after the delete", s.Len())
	}
	if s.Delete("resp_1", "") {
		t.Fatal("second Delete(resp_1) = true, want false (already gone)")
	}
	if s.Delete("resp_never", "") {
		t.Fatal("Delete(never-existed) = true, want false")
	}
	if s.Delete("", "") {
		t.Fatal("Delete(\"\") = true, want false")
	}
	if _, ok := s.Get("resp_2", ""); !ok {
		t.Fatal("the surviving record is gone too")
	}
}

// TestStorePage is the store gate for CHECKLIST 204: newest first,
// offset/limit paging, total before paging, expired entries
// skipped (and pruned) instead of counted or served, and the page
// carries the stored response JSON (the list endpoint serves the
// full objects — unlike Recent, which strips content).
func TestStorePage(t *testing.T) {
	s := NewStore(time.Hour, 10)
	for i := 1; i <= 3; i++ {
		s.Put(Record{ID: "resp_" + string(rune('0'+i)), Model: "m", Status: StatusCompleted,
			ResponseJSON: json.RawMessage(`{"id":"resp_` + string(rune('0'+i)) + `"}`)})
	}
	page, total := s.Page(0, 2, "")
	if total != 3 || len(page) != 2 || page[0].ID != "resp_3" || page[1].ID != "resp_2" {
		t.Fatalf("Page(0,2) = %+v / total %d, want resp_3,resp_2 / 3", page, total)
	}
	if string(page[0].ResponseJSON) != `{"id":"resp_3"}` {
		t.Fatalf("page record lost its response JSON: %q", page[0].ResponseJSON)
	}
	page, total = s.Page(2, 2, "")
	if total != 3 || len(page) != 1 || page[0].ID != "resp_1" {
		t.Fatalf("Page(2,2) = %+v / total %d, want resp_1 / 3", page, total)
	}
	page, total = s.Page(3, 2, "")
	if total != 3 || len(page) != 0 {
		t.Fatalf("Page(3,2) = %+v / total %d, want empty / 3", page, total)
	}
	page, total = s.Page(-1, 2, "")
	if len(page) != 2 || page[0].ID != "resp_3" {
		t.Fatalf("Page(-1,2) = %+v, want the negative offset clamped to 0", page)
	}
	if page, total := s.Page(0, 0, ""); len(page) != 0 || total != 0 {
		t.Fatalf("Page(0,0) = %d / %d, want 0/0", len(page), total)
	}
	// Expired entries never count and never serve. The TTL is
	// 100 ms (not 1 ms): under full-suite load a goroutine can
	// be descheduled for >1 ms between the Put and the Page, which
	// would expire the fresh entry too and fail the test for no
	// product reason.
	s2 := NewStore(100*time.Millisecond, 10)
	s2.Put(Record{ID: "resp_old", Model: "m", Status: StatusCompleted})
	time.Sleep(150 * time.Millisecond)
	s2.Put(Record{ID: "resp_new", Model: "m", Status: StatusCompleted})
	page, total = s2.Page(0, 10, "")
	if total != 1 || len(page) != 1 || page[0].ID != "resp_new" {
		t.Fatalf("Page with an expired entry = %+v / total %d, want only resp_new / 1", page, total)
	}
	if _, ok := s2.Get("resp_old", ""); ok {
		t.Fatal("expired entry still retrievable after the page pruned it")
	}
}

// TestStoreIndexOf pins the list cursor: newest-first indices, and
// absent/expired ids resolve to false.
func TestStoreIndexOf(t *testing.T) {
	s := NewStore(time.Hour, 10)
	s.Put(Record{ID: "resp_1", Model: "m", Status: StatusCompleted})
	s.Put(Record{ID: "resp_2", Model: "m", Status: StatusCompleted})
	s.Put(Record{ID: "resp_3", Model: "m", Status: StatusCompleted})
	if idx, ok := s.IndexOf("resp_3"); !ok || idx != 0 {
		t.Fatalf("IndexOf(resp_3) = %d/%v, want 0/true", idx, ok)
	}
	if idx, ok := s.IndexOf("resp_1"); !ok || idx != 2 {
		t.Fatalf("IndexOf(resp_1) = %d/%v, want 2/true", idx, ok)
	}
	if _, ok := s.IndexOf("resp_never"); ok {
		t.Fatal("IndexOf(never-existed) = true")
	}
	if _, ok := s.IndexOf(""); ok {
		t.Fatal("IndexOf(\"\") = true")
	}
}

// TestStoreChildren is the CHECKLIST 237 store gate: Children
// returns the direct branches off a response in creation order —
// two children of one parent, a grandchild only under its own
// parent, none for a leaf or an unknown id — and a nil store
// returns none, never panics.
func TestStoreChildren(t *testing.T) {
	s := NewStore(time.Hour, 10)
	s.Put(Record{ID: "resp_root", Model: "m", Status: StatusCompleted})
	s.Put(Record{ID: "resp_kid1", Model: "m", Status: StatusCompleted, PreviousID: "resp_root"})
	s.Put(Record{ID: "resp_kid2", Model: "m", Status: StatusCompleted, PreviousID: "resp_root"})
	s.Put(Record{ID: "resp_grand", Model: "m", Status: StatusCompleted, PreviousID: "resp_kid1"})
	s.Put(Record{ID: "resp_other", Model: "m", Status: StatusCompleted})

	kids := s.Children("resp_root")
	if len(kids) != 2 || kids[0] != "resp_kid1" || kids[1] != "resp_kid2" {
		t.Fatalf("Children(root) = %v, want [resp_kid1 resp_kid2]", kids)
	}
	if g := s.Children("resp_kid1"); len(g) != 1 || g[0] != "resp_grand" {
		t.Fatalf("Children(kid1) = %v, want [resp_grand]", g)
	}
	if len(s.Children("resp_grand")) != 0 || len(s.Children("resp_nope")) != 0 || len(s.Children("")) != 0 {
		t.Fatal("Children of a leaf / unknown / empty id must be empty")
	}
	var nilStore *Store
	if len(nilStore.Children("resp_root")) != 0 {
		t.Fatal("nil store Children must be empty")
	}
}
