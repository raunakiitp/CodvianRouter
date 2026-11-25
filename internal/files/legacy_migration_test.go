// AUD-SEC-010 legacy migration: file records written BEFORE ownership
// scoping deserialise with an empty OwnerKeyID. They must not be exposed
// to arbitrary authenticated keys, and they must not be silently
// destroyed or stranded either — the migration is explicit, admin-gated
// and auditable.
//
// The fixture below is written by hand in the REAL pre-change format (a
// bare JSON array with no owner field and no version marker), not by the
// current code, so this test exercises the upgrade path an existing
// deployment would take.
package files

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyIndexFixture is the exact v1 on-disk shape: a bare array of
// records, no owner_key_id on the pre-scoping record.
const legacyIndexFixture = `[
  {"id":"file-000000000000000000000001","object":"file","bytes":11,"created_at":1700000000,"filename":"old-upload.txt","purpose":"assistants"},
  {"id":"file-000000000000000000000002","object":"file","bytes":7,"created_at":1700000001,"filename":"newer.txt","purpose":"assistants","owner_key_id":"key-A"}
]`

// writeLegacyStore writes the hand-authored pre-change index and the
// matching content files, exactly as a pre-upgrade deployment would have.
func writeLegacyStore(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	// Content for both records, so their absence/presence is meaningful.
	for _, c := range []struct{ id, body string }{
		{"file-000000000000000000000001", "legacy-body"},
		{"file-000000000000000000000002", "owned-body"},
	} {
		if err := os.WriteFile(filepath.Join(dir, c.id), []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestAUD_SEC_010LegacyUnownedRecordsAreNotVisibleToAnyKey: the upgrade
// must not expose pre-scoping records to any authenticated key — and must
// not treat an empty owner as a wildcard.
func TestAUD_SEC_010LegacyUnownedRecordsAreNotVisibleToAnyKey(t *testing.T) {
	st := New(writeLegacyStore(t, legacyIndexFixture))

	// Key A sees only its own record (not the unowned one).
	listA, err := st.ListFor("key-A")
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 || listA[0].ID != "file-000000000000000000000002" {
		t.Fatalf("key-A list = %+v, want only its own record", listA)
	}

	// Any other key sees nothing — in particular not the unowned record.
	for _, key := range []string{"key-B", "key-a", "KEY-A", "static-1"} {
		got, err := st.ListFor(key)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("key %q sees %d record(s) including unowned legacy data: %+v", key, len(got), got)
		}
		if _, err := st.GetFor("file-000000000000000000000001", key); err == nil {
			t.Fatalf("key %q could GET the unowned legacy record", key)
		}
		if _, _, err := st.OpenFor("file-000000000000000000000001", key); err == nil {
			t.Fatalf("key %q could OPEN the unowned legacy record", key)
		}
		if ok, _ := st.DeleteFor("file-000000000000000000000001", key); ok {
			t.Fatalf("key %q could DELETE the unowned legacy record", key)
		}
	}

	// The admin/forensic view can still see it, which is the whole point:
	// it is private, not lost.
	unowned, err := st.LegacyUnowned()
	if err != nil {
		t.Fatal(err)
	}
	if len(unowned) != 1 || unowned[0].ID != "file-000000000000000000000001" {
		t.Fatalf("LegacyUnowned = %+v, want the pre-scoping record", unowned)
	}
}

// TestAUD_SEC_010LegacyReclaimAssignsExactlyAndIsAuditable.
func TestAUD_SEC_010LegacyReclaimAssignsExactlyAndIsAuditable(t *testing.T) {
	dir := writeLegacyStore(t, legacyIndexFixture)
	st := New(dir)

	reclaimed, remaining, err := st.ReclaimLegacy("key-B", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed != 1 || remaining != 0 {
		t.Fatalf("reclaim = %d reclaimed / %d remaining, want 1 / 0", reclaimed, remaining)
	}

	// The record is now key-B's, and key-A still has only its own.
	got, err := st.GetFor("file-000000000000000000000001", "key-B")
	if err != nil || got.OwnerKeyID != "key-B" {
		t.Fatalf("after reclaim: %+v %v", got, err)
	}
	rc, _, err := st.OpenFor("file-000000000000000000000001", "key-B")
	if err != nil {
		t.Fatalf("the reclaimed record's content is not readable by its new owner: %v", err)
	}
	body, readErr := io.ReadAll(rc)
	if closeErr := rc.Close(); closeErr != nil {
		t.Fatalf("close reclaimed content: %v", closeErr)
	}
	if readErr != nil {
		t.Fatalf("read reclaimed content: %v", readErr)
	}
	if string(body) != "legacy-body" {
		t.Fatalf("reclaimed content = %q, want the original bytes", body)
	}
	if _, err := st.GetFor("file-000000000000000000000001", "key-A"); err == nil {
		t.Fatal("key-A can still read a record reclaimed by key-B")
	}
	listA, _ := st.ListFor("key-A")
	if len(listA) != 1 {
		t.Fatalf("key-A list after reclaim = %+v, want its own single record", listA)
	}

	// The persisted index was upgraded to the versioned envelope, and the
	// owner is on disk — an operator can audit the migration with jq.
	raw, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env indexEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("index is not valid JSON after reclaim: %v", err)
	}
	if env.Version != indexVersion {
		t.Errorf("index version = %d, want %d", env.Version, indexVersion)
	}
	if len(env.Files) != 2 {
		t.Fatalf("index files = %d, want both records preserved", len(env.Files))
	}
	found := false
	for _, f := range env.Files {
		if f.ID == "file-000000000000000000000001" {
			found = true
			if f.OwnerKeyID != "key-B" {
				t.Errorf("persisted owner = %q, want key-B", f.OwnerKeyID)
			}
		}
	}
	if !found {
		t.Fatal("the migrated record disappeared from the index")
	}
}

// TestAUD_SEC_010ReclaimRefusesTheWildcardState: an empty owner (or an
// empty owner applied to nothing) must never be accepted — that is the
// state being repaired.
func TestAUD_SEC_010ReclaimRefusesTheWildcardState(t *testing.T) {
	st := New(writeLegacyStore(t, legacyIndexFixture))
	if _, _, err := st.ReclaimLegacy("", nil); err == nil {
		t.Fatal("ReclaimLegacy accepted an empty owner: unowned would have been re-created")
	}
	if _, _, err := st.ReclaimLegacy("   ", nil); err == nil {
		t.Fatal("ReclaimLegacy accepted a whitespace owner")
	}
	unowned, _ := st.LegacyUnowned()
	if len(unowned) != 1 {
		t.Fatal("a refused reclaim changed the index")
	}
}

// TestAUD_SEC_010ReclaimCannotStealAnotherKeysFile.
func TestAUD_SEC_010ReclaimCannotStealAnotherKeysFile(t *testing.T) {
	st := New(writeLegacyStore(t, legacyIndexFixture))
	// key-A's record is owned; asking to reclaim it for key-C must be a
	// no-op rather than a transfer.
	reclaimed, remaining, err := st.ReclaimLegacy("key-C", []string{"file-000000000000000000000002"})
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed != 0 {
		t.Fatalf("reclaimed = %d, want 0: an owned record must never be reassigned by the migration", reclaimed)
	}
	if remaining != 1 {
		t.Fatalf("remaining = %d, want 1 (the unowned record is untouched)", remaining)
	}
	if got, err := st.GetFor("file-000000000000000000000002", "key-A"); err != nil || got.OwnerKeyID != "key-A" {
		t.Fatalf("key-A's record changed hands: %+v %v", got, err)
	}
	if _, err := st.GetFor("file-000000000000000000000002", "key-C"); err == nil {
		t.Fatal("key-C acquired a record it did not own")
	}
}

// TestAUD_SEC_010LegacyIndexRoundTripsWithoutLoss: reading a v1 index and
// writing it back keeps every record (an upgrade must not drop data), and
// the rewritten index loads as a current one.
func TestAUD_SEC_010LegacyIndexRoundTripsWithoutLoss(t *testing.T) {
	dir := writeLegacyStore(t, legacyIndexFixture)
	st := New(dir)

	// A write of any kind upgrades the format. Reclaim one record and
	// check the OTHER is preserved with its owner and content intact.
	if _, _, err := st.ReclaimLegacy("key-B", nil); err != nil {
		t.Fatal(err)
	}
	reopened := New(dir)
	listA, err := reopened.ListFor("key-A")
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 || listA[0].OwnerKeyID != "key-A" || listA[0].Filename != "newer.txt" {
		t.Fatalf("reopened key-A list = %+v, want the untouched record", listA)
	}
	listB, err := reopened.ListFor("key-B")
	if err != nil || len(listB) != 1 {
		t.Fatalf("reopened key-B list = %+v %v, want the reclaimed record", listB, err)
	}
}

// TestAUD_SEC_010CorruptIndexIsStillAnError: a corrupt index is still an
// error (no silent drop of metadata, which would orphan content).
func TestAUD_SEC_010CorruptIndexIsStillAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := New(dir)
	if _, err := st.ListFor("key-A"); err == nil {
		t.Fatal("a corrupt index was silently treated as empty: content files would be orphaned")
	}
}

// TestAUD_SEC_010CurrentFormatStillLoads pins that the envelope written by
// saveIndexLocked is read back by the same code (no format self-conflict).
func TestAUD_SEC_010CurrentFormatStillLoads(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	created, err := st.CreateOwned("mine.txt", "assistants", "key-A", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	reopened := New(dir)
	got, err := reopened.GetFor(created.ID, "key-A")
	if err != nil || got.ID != created.ID {
		t.Fatalf("current-format index did not load: %+v %v", got, err)
	}
	if _, err := reopened.GetFor(created.ID, "key-B"); err == nil {
		t.Fatal("a current-format record leaked across keys")
	}
	if unowned, err := reopened.LegacyUnowned(); err != nil || len(unowned) != 0 {
		t.Fatalf("LegacyUnowned on a current index = %+v %v, want none", unowned, err)
	}
}
