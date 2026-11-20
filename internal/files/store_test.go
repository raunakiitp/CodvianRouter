package files

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustCreate(t *testing.T, st *Store, name, purpose, content string) *File {
	t.Helper()
	f, err := st.Create(name, purpose, strings.NewReader(content))
	if err != nil {
		t.Fatalf("Create(%q, %q): %v", name, purpose, err)
	}
	return f
}

// TestStoreCreateGetListDelete is the round trip: create stores
// bytes + metadata, Get/List return them, Open returns the exact
// bytes, Delete removes both content and index entry.
func TestStoreCreateGetListDelete(t *testing.T) {
	st := New(t.TempDir())
	f := mustCreate(t, st, "data.jsonl", "batch", "{\"a\":1}\n")

	if f.Object != "file" || f.Status != "processed" {
		t.Fatalf("object=%q status=%q, want file/processed", f.Object, f.Status)
	}
	if f.Bytes != 8 || f.Filename != "data.jsonl" || f.Purpose != "batch" {
		t.Fatalf("metadata mismatch: %+v", f)
	}
	if f.CreatedAt <= 0 {
		t.Fatalf("created_at %d, want > 0", f.CreatedAt)
	}
	if !ValidID(f.ID) {
		t.Fatalf("id %q is not a valid store id", f.ID)
	}

	got, err := st.Get(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *f {
		t.Fatalf("Get = %+v, want %+v", got, f)
	}

	rc, opened, err := st.Open(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{\"a\":1}\n" || opened.ID != f.ID {
		t.Fatalf("Open = %q, want exact uploaded bytes", raw)
	}

	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != f.ID {
		t.Fatalf("List = %v, want one entry %s", list, f.ID)
	}

	deleted, err := st.Delete(f.ID)
	if err != nil || !deleted {
		t.Fatalf("Delete = %v, %v, want true, nil", deleted, err)
	}
	if _, err := st.Get(f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	if _, _, err := st.Open(f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after delete = %v, want ErrNotFound", err)
	}
	list, err = st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("List after delete = %d entries, want 0", len(list))
	}

	// Deleting again is a clean "nothing deleted", not an error.
	deleted, err = st.Delete(f.ID)
	if err != nil || deleted {
		t.Fatalf("second Delete = %v, %v, want false, nil", deleted, err)
	}
}

// TestStoreSanitizeFilename: traversal names collapse to a bare
// base name; empty/dot names are rejected, never guessed.
func TestStoreSanitizeFilename(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"../../etc/passwd", "passwd"},
		{"..\\..\\windows\\secret.txt", "secret.txt"},
		{"a/b/c.txt", "c.txt"},
		{"/abs/path.jsonl", "path.jsonl"},
		{" plain.txt ", "plain.txt"},
		{"normal.jsonl", "normal.jsonl"},
	}
	for _, c := range cases {
		got, err := SanitizeFilename(c.in)
		if err != nil {
			t.Fatalf("SanitizeFilename(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("SanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Fatalf("sanitised %q still contains a separator", got)
		}
	}
	for _, bad := range []string{"", "   ", ".", "..", "../..", `..\..\`, "/"} {
		if got, err := SanitizeFilename(bad); err == nil {
			t.Fatalf("SanitizeFilename(%q) = %q, want error", bad, got)
		}
	}

	// End to end: a traversal filename is stored under its base
	// name and its content is reachable only via the id.
	st := New(t.TempDir())
	f := mustCreate(t, st, "../../evil.txt", "user_data", "x")
	if f.Filename != "evil.txt" {
		t.Fatalf("stored filename = %q, want evil.txt", f.Filename)
	}
}

// TestStoreInvalidPurpose: only the six verified OpenAI upload
// purposes are accepted; the output-only purposes and anything
// invented are rejected.
func TestStoreInvalidPurpose(t *testing.T) {
	st := New(t.TempDir())
	for _, p := range ValidPurposes {
		if _, err := st.Create("ok.txt", p, strings.NewReader("x")); err != nil {
			t.Fatalf("Create purpose %q: %v", p, err)
		}
	}
	for _, bad := range []string{"", "assistants_output", "batch_output", "fine-tune-results", "training", "chat"} {
		if _, err := st.Create("ok.txt", bad, strings.NewReader("x")); !errors.Is(err, ErrInvalidPurpose) {
			t.Fatalf("Create purpose %q: err = %v, want ErrInvalidPurpose", bad, err)
		}
	}
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != len(ValidPurposes) {
		t.Fatalf("List = %d entries, want %d (only valid uploads stored)", len(list), len(ValidPurposes))
	}
}

// TestStoreInvalidID: ids that are not store-issued never reach a
// path lookup — traversal strings are ErrInvalidID, not a file.
func TestStoreInvalidID(t *testing.T) {
	st := New(t.TempDir())
	for _, id := range []string{"", "../index.json", "../../etc/passwd", "file-zzz", "file-0000000000000000000001.txt", "file-000000000000000000000001/.."} {
		if _, err := st.Get(id); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("Get(%q): err = %v, want ErrInvalidID", id, err)
		}
		if _, err := st.Delete(id); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("Delete(%q): err = %v, want ErrInvalidID", id, err)
		}
		if _, _, err := st.Open(id); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("Open(%q): err = %v, want ErrInvalidID", id, err)
		}
	}
	// A well-formed but unknown id is NotFound, not InvalidID.
	if _, err := st.Get("file-000000000000000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(unknown well-formed id): err = %v, want ErrNotFound", err)
	}
}

// TestStorePersistence: a second Store over the same directory
// sees the first store's files — the JSON index is the state, not
// process memory.
func TestStorePersistence(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	f := mustCreate(t, a, "keep.jsonl", "fine-tune", "persist me")
	b := New(dir)
	got, err := b.Get(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Filename != "keep.jsonl" || got.Purpose != "fine-tune" {
		t.Fatalf("reopened Get = %+v", got)
	}
	rc, _, err := b.Open(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "persist me" {
		t.Fatalf("reopened content = %q", raw)
	}
}

// TestStoreListNewestFirst: list order is newest first.
func TestStoreListNewestFirst(t *testing.T) {
	st := New(t.TempDir())
	first := mustCreate(t, st, "a.txt", "batch", "a")
	second := mustCreate(t, st, "b.txt", "batch", "b")
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("List = %d, want 2", len(list))
	}
	// Same-second uploads tie-break by id desc; either way both
	// are present and the older one is never first when the
	// timestamps differ. Assert the set, and newest-first when
	// created_at differs.
	seen := map[string]bool{list[0].ID: true, list[1].ID: true}
	if !seen[first.ID] || !seen[second.ID] {
		t.Fatalf("List ids = %v, want %s and %s", []string{list[0].ID, list[1].ID}, first.ID, second.ID)
	}
	for i := 1; i < len(list); i++ {
		if list[i].CreatedAt > list[i-1].CreatedAt {
			t.Fatalf("List not newest-first at %d: %+v", i, list)
		}
	}
}

// TestStoreSweepStaleUploadTemps: the orphan sweeper removes
// exactly the stale staging temps and nothing else. A temp
// unmodified for over the threshold (a crash orphan) is removed;
// a fresh temp (a live upload, still being written) is kept; a
// real stored file, the index, and same-age files that do not
// match the upload-*.tmp staging pattern are kept; a stale
// DIRECTORY matching the pattern is kept (only files are swept).
// A missing store dir holds no orphans and is not an error.
func TestStoreSweepStaleUploadTemps(t *testing.T) {
	root := t.TempDir()
	st := New(root)
	seed := mustCreate(t, st, "seed.txt", "batch", "hello files")

	plant := func(name, content string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(root, name), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	stale := now.Add(-2 * time.Hour)
	plant("upload-stale123.tmp", "orphan", stale) // must go
	plant("upload-fresh456.tmp", "live", now)     // must stay
	plant("upload-keep.txt", "wrong suffix", stale)
	plant("other-789.tmp", "wrong prefix", stale)
	if err := os.Mkdir(filepath.Join(root, "upload-dir.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "upload-dir.tmp"), stale, stale); err != nil {
		t.Fatal(err)
	}

	removed, err := st.SweepStaleUploadTemps(time.Hour)
	if err != nil {
		t.Fatalf("SweepStaleUploadTemps: %v", err)
	}
	if removed != 1 {
		t.Fatalf("SweepStaleUploadTemps removed %d, want 1 (the stale staging temp only)", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "upload-stale123.tmp")); !os.IsNotExist(err) {
		t.Fatalf("stale staging temp still exists after sweep (err=%v)", err)
	}
	for _, keep := range []string{"upload-fresh456.tmp", "upload-keep.txt", "other-789.tmp", "upload-dir.tmp", "index.json", seed.ID} {
		if _, err := os.Stat(filepath.Join(root, keep)); err != nil {
			t.Fatalf("sweep removed or lost %q, want it kept: %v", keep, err)
		}
	}
	if got, err := st.Get(seed.ID); err != nil || got == nil {
		t.Fatalf("seeded file unreachable after sweep: %v", err)
	}

	// Second sweep is a no-op: nothing stale remains.
	if removed, err := st.SweepStaleUploadTemps(time.Hour); err != nil || removed != 0 {
		t.Fatalf("second sweep: removed=%d err=%v, want 0/nil", removed, err)
	}

	// A missing store dir holds no orphans and is not an error.
	missing := New(filepath.Join(t.TempDir(), "no-such-dir"))
	if removed, err := missing.SweepStaleUploadTemps(time.Hour); err != nil || removed != 0 {
		t.Fatalf("sweep of missing dir: removed=%d err=%v, want 0/nil", removed, err)
	}
}

// TestStoreSweepStaleIndexTemps: the orphan sweeper also removes
// stale index-*.tmp temps — the residue saveIndexLocked leaves
// behind when a process crashes mid-index-write (after CreateTemp
// but before Rename), which no deferred cleanup ever runs for.
// Mirrors TestStoreSweepStaleUploadTemps: a 2h-old index temp is
// removed; a fresh index temp, the real index.json, and same-age
// decoys are kept; a stale DIRECTORY matching the pattern is kept
// (only files are swept); a stale upload temp in the same sweep
// proves both orphan classes are swept together. A missing store
// dir holds no orphans and is not an error.
func TestStoreSweepStaleIndexTemps(t *testing.T) {
	root := t.TempDir()
	st := New(root)
	seed := mustCreate(t, st, "seed.txt", "batch", "hello files")

	plant := func(name, content string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(root, name), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	stale := now.Add(-2 * time.Hour)
	plant("index-stale123.tmp", "orphan index write", stale) // must go
	plant("index-fresh456.tmp", "live index write", now)     // must stay
	plant("index-keep.txt", "wrong suffix", stale)
	plant("other-789.tmp", "wrong prefix", stale)
	plant("upload-stale999.tmp", "orphan upload", stale) // must go too
	if err := os.Mkdir(filepath.Join(root, "index-dir.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "index-dir.tmp"), stale, stale); err != nil {
		t.Fatal(err)
	}

	removed, err := st.SweepStaleUploadTemps(time.Hour)
	if err != nil {
		t.Fatalf("SweepStaleUploadTemps: %v", err)
	}
	if removed != 2 {
		t.Fatalf("SweepStaleUploadTemps removed %d, want 2 (stale index temp + stale upload temp)", removed)
	}
	for _, gone := range []string{"index-stale123.tmp", "upload-stale999.tmp"} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("stale temp %q still exists after sweep (err=%v)", gone, err)
		}
	}
	for _, keep := range []string{"index-fresh456.tmp", "index-keep.txt", "other-789.tmp", "index-dir.tmp", "index.json", seed.ID} {
		if _, err := os.Stat(filepath.Join(root, keep)); err != nil {
			t.Fatalf("sweep removed or lost %q, want it kept: %v", keep, err)
		}
	}
	if got, err := st.Get(seed.ID); err != nil || got == nil {
		t.Fatalf("seeded file unreachable after sweep: %v", err)
	}

	// Second sweep is a no-op: nothing stale remains.
	if removed, err := st.SweepStaleUploadTemps(time.Hour); err != nil || removed != 0 {
		t.Fatalf("second sweep: removed=%d err=%v, want 0/nil", removed, err)
	}

	// A missing store dir holds no orphans and is not an error.
	missing := New(filepath.Join(t.TempDir(), "no-such-dir"))
	if removed, err := missing.SweepStaleUploadTemps(time.Hour); err != nil || removed != 0 {
		t.Fatalf("sweep of missing dir: removed=%d err=%v, want 0/nil", removed, err)
	}
}

// TestStoreConcurrentCreates: parallel uploads each get a distinct
// id and all land in the index — no lost entries.
func TestStoreConcurrentCreates(t *testing.T) {
	st := New(t.TempDir())
	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.Create("f.txt", "batch", strings.NewReader("x"))
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n {
		t.Fatalf("List = %d entries after %d concurrent creates, want %d", len(list), n, n)
	}
	ids := map[string]bool{}
	for _, f := range list {
		if ids[f.ID] {
			t.Fatalf("duplicate id %s", f.ID)
		}
		ids[f.ID] = true
	}
}
