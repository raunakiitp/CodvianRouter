// AUD-SEC-010 regression tests: the file store enforces ownership.
//
// Before the fix, files.File had no owner and every accessor was
// unscoped, so any gateway key could list, read, download and delete
// every other key's uploaded files (and reference them as batch input).
package files

import (
	"io"
	"strings"
	"testing"
)

func p13CreateOwned(t *testing.T, st *Store, name, owner string) *File {
	t.Helper()
	f, err := st.CreateOwned(name, "batch", owner, strings.NewReader("content-of-"+owner+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAUD_SEC_010StoreOwnerScoping(t *testing.T) {
	st := New(t.TempDir())
	ownedA := p13CreateOwned(t, st, "a.jsonl", "key-a")
	ownedB := p13CreateOwned(t, st, "b.jsonl", "key-b")
	unowned := p13CreateOwned(t, st, "u.jsonl", "")

	// The owner sees its own file; a foreign key does not (reported as
	// not found, never as "forbidden", so existence is not confirmed).
	if f, err := st.GetFor(ownedA.ID, "key-a"); err != nil || f.ID != ownedA.ID {
		t.Fatalf("owner GetFor: %v %v", f, err)
	}
	if _, err := st.GetFor(ownedA.ID, "key-b"); err == nil {
		t.Fatal("foreign key must not GetFor another key's file")
	}
	// Empty identity must not reach an owned file.
	if _, err := st.GetFor(ownedA.ID, ""); err == nil {
		t.Fatal("empty identity must not GetFor an owned file")
	}
	// Download path.
	if rc, _, err := st.OpenFor(ownedA.ID, "key-a"); err != nil {
		t.Fatalf("owner OpenFor: %v", err)
	} else {
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	if _, _, err := st.OpenFor(ownedA.ID, "key-b"); err == nil {
		t.Fatal("foreign key must not OpenFor another key's file")
	}

	// Listing is owner-scoped both ways.
	listA, err := st.ListFor("key-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 || listA[0].ID != ownedA.ID {
		t.Fatalf("ListFor(key-a) = %+v, want only key-a's file", listA)
	}
	listB, _ := st.ListFor("key-b")
	if len(listB) != 1 || listB[0].ID != ownedB.ID {
		t.Fatalf("ListFor(key-b) = %+v, want only key-b's file", listB)
	}
	listEmpty, _ := st.ListFor("")
	if len(listEmpty) != 1 || listEmpty[0].ID != unowned.ID {
		t.Fatalf("ListFor(\"\") = %+v, want only the unowned file", listEmpty)
	}

	// A foreign delete must not remove the file and must report false.
	if deleted, err := st.DeleteFor(ownedA.ID, "key-b"); err != nil || deleted {
		t.Fatalf("foreign DeleteFor = %v %v, want false, nil", deleted, err)
	}
	if _, err := st.GetFor(ownedA.ID, "key-a"); err != nil {
		t.Fatal("a foreign delete must not remove the owner's file")
	}
	if deleted, err := st.DeleteFor(ownedA.ID, "key-a"); err != nil || !deleted {
		t.Fatalf("owner DeleteFor = %v %v, want true, nil", deleted, err)
	}
	// An empty identity must not delete an owned file either.
	if deleted, _ := st.DeleteFor(ownedB.ID, ""); deleted {
		t.Fatal("empty identity must not delete an owned file")
	}
}

func TestAUD_SEC_010LegacyUnownedFilePolicy(t *testing.T) {
	st := New(t.TempDir())
	unowned := p13CreateOwned(t, st, "legacy.jsonl", "")

	// The documented legacy policy: an unowned file is visible to an
	// unowned (auth-relaxed) caller and to no identified key.
	if _, err := st.GetFor(unowned.ID, ""); err != nil {
		t.Fatalf("unowned caller must see an unowned file: %v", err)
	}
	if _, err := st.GetFor(unowned.ID, "key-a"); err == nil {
		t.Fatal("an identified key must not see an unowned file (empty owner is not a wildcard)")
	}
	// The compat wrapper is the unowned caller.
	if _, err := st.Get(unowned.ID); err != nil {
		t.Fatalf("Get (unowned caller) must see an unowned file: %v", err)
	}
}

func TestAUD_SEC_010OwnerSurvivesIndexRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	f := p13CreateOwned(t, st, "persisted.jsonl", "key-a")

	// A fresh store over the same directory (restart) keeps ownership.
	reopened := New(dir)
	got, err := reopened.GetFor(f.ID, "key-a")
	if err != nil {
		t.Fatalf("owner lost across restart: %v", err)
	}
	if got.OwnerKeyID != "key-a" {
		t.Fatalf("reloaded owner = %q, want key-a", got.OwnerKeyID)
	}
	if _, err := reopened.GetFor(f.ID, "key-b"); err == nil {
		t.Fatal("ownership not enforced after restart")
	}
}
