package responses

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPersistRecord(id string) Record {
	return Record{
		ID:                  id,
		Model:               "gpt-4o",
		ProviderID:          "prov_1",
		Status:              StatusCompleted,
		CreatedAt:           time.Now().Unix(),
		PreviousID:          "resp_parent",
		Stream:              false,
		TranscriptJSON:      json.RawMessage(`[{"role":"user","content":"hi"},{"role":"assistant","content":"hello back"}]`),
		ResponseJSON:        json.RawMessage(`{"id":"` + id + `","object":"response","status":"completed"}`),
		TurnInputTokens:     5,
		TurnOutputTokens:    4,
		SessionInputTokens:  12,
		SessionOutputTokens: 9,
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "responses.json")
	recs := []Record{testPersistRecord("resp_aaa"), testPersistRecord("resp_bbb")}
	recs[1].PreviousID = "resp_aaa"
	if err := SaveFile(path, recs); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	// No temp file left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".responses-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d records, want 2", len(loaded))
	}
	for i, want := range recs {
		got := loaded[i]
		if got.ID != want.ID || got.Model != want.Model || got.ProviderID != want.ProviderID ||
			got.Status != want.Status || got.CreatedAt != want.CreatedAt ||
			got.PreviousID != want.PreviousID || got.Stream != want.Stream ||
			got.TurnInputTokens != want.TurnInputTokens || got.TurnOutputTokens != want.TurnOutputTokens ||
			got.SessionInputTokens != want.SessionInputTokens || got.SessionOutputTokens != want.SessionOutputTokens {
			t.Fatalf("loaded[%d] = %+v, want %+v", i, got, want)
		}
		if string(got.TranscriptJSON) != string(want.TranscriptJSON) {
			t.Fatalf("loaded[%d] transcript = %s, want %s", i, got.TranscriptJSON, want.TranscriptJSON)
		}
		if string(got.ResponseJSON) != string(want.ResponseJSON) {
			t.Fatalf("loaded[%d] response = %s, want %s", i, got.ResponseJSON, want.ResponseJSON)
		}
	}
	// Saving again replaces the file (same path, new content),
	// never appends a second copy.
	if err := SaveFile(path, recs[:1]); err != nil {
		t.Fatalf("SaveFile again: %v", err)
	}
	loaded, err = LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile again: %v", err)
	}
	if len(loaded) != 1 || loaded[0].ID != "resp_aaa" {
		t.Fatalf("reloaded = %+v, want only resp_aaa", loaded)
	}
}

func TestLoadMissingFileIsFreshInstall(t *testing.T) {
	loaded, err := LoadFile(filepath.Join(t.TempDir(), "responses.json"))
	if err != nil {
		t.Fatalf("LoadFile missing: %v, want nil error", err)
	}
	if loaded != nil {
		t.Fatalf("loaded = %+v, want nil for a missing file", loaded)
	}
}

func TestLoadCorruptFileErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "responses.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile corrupt: nil error, want an error")
	}
	if err := os.WriteFile(path, []byte(`{"version":99,"records":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile wrong version: nil error, want an error")
	}
}

func TestSaveEmptyPathErrors(t *testing.T) {
	if err := SaveFile("", nil); err == nil {
		t.Fatal("SaveFile empty path: nil error, want an error")
	}
	if _, err := LoadFile(""); err == nil {
		t.Fatal("LoadFile empty path: nil error, want an error")
	}
	// A missing parent directory is an error, never a silent skip.
	if err := SaveFile(filepath.Join(t.TempDir(), "nope", "responses.json"), nil); err == nil {
		t.Fatal("SaveFile missing dir: nil error, want an error")
	}
}

func TestSaveEmptyStoreWritesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.json")
	if err := SaveFile(path, nil); err != nil {
		t.Fatalf("SaveFile nil: %v", err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("loaded = %+v, want empty", loaded)
	}
}

func TestStoreSnapshotOldestFirstFullRecords(t *testing.T) {
	s := NewStore(time.Hour, 10)
	s.Put(testPersistRecord("resp_1"))
	s.Put(testPersistRecord("resp_2"))
	snap := s.Snapshot()
	if len(snap) != 2 || snap[0].ID != "resp_1" || snap[1].ID != "resp_2" {
		t.Fatalf("Snapshot = %+v, want resp_1 then resp_2", snap)
	}
	if string(snap[0].TranscriptJSON) == "" || string(snap[0].ResponseJSON) == "" {
		t.Fatal("Snapshot dropped the transcript or the served object")
	}
	// Mutating a snapshot record never mutates the store.
	snap[0].Model = "changed"
	if got, _ := s.Get("resp_1", ""); got.Model != "gpt-4o" {
		t.Fatalf("snapshot mutation leaked into the store: %q", got.Model)
	}
}

func TestStoreSnapshotSkipsExpired(t *testing.T) {
	s := NewStore(10*time.Millisecond, 10)
	s.Put(testPersistRecord("resp_1"))
	time.Sleep(25 * time.Millisecond)
	if snap := s.Snapshot(); len(snap) != 0 {
		t.Fatalf("Snapshot = %+v, want empty after expiry", snap)
	}
}

func TestStoreRestoreRoundTrip(t *testing.T) {
	src := NewStore(time.Hour, 10)
	rec1 := testPersistRecord("resp_1")
	rec1.PreviousID = "" // a fresh create: no parent
	src.Put(rec1)
	rec2 := testPersistRecord("resp_2")
	rec2.PreviousID = "resp_1"
	src.Put(rec2)

	dst := NewStore(time.Hour, 10)
	if n := dst.Restore(src.Snapshot()); n != 2 {
		t.Fatalf("Restore = %d, want 2", n)
	}
	got, ok := dst.Get("resp_2", "")
	if !ok {
		t.Fatal("restored resp_2 not found")
	}
	if string(got.TranscriptJSON) != string(rec2.TranscriptJSON) {
		t.Fatalf("restored transcript = %s, want %s", got.TranscriptJSON, rec2.TranscriptJSON)
	}
	if got.SessionInputTokens != rec2.SessionInputTokens {
		t.Fatalf("restored session tokens = %d, want %d", got.SessionInputTokens, rec2.SessionInputTokens)
	}
	// Restoring the same records again restores nothing: the
	// live store wins, never overwritten.
	if n := dst.Restore(src.Snapshot()); n != 0 {
		t.Fatalf("second Restore = %d, want 0", n)
	}
	if st := dst.Stats(); st.Created != 2 || st.Continuations != 1 {
		t.Fatalf("Stats = %+v, want created 2 continuations 1", st)
	}
}

func TestStoreRestoreSkipsExpired(t *testing.T) {
	// A record created 3h ago, restored into a store with a 2h
	// TTL, is dropped — a restart never resurrects it.
	old := testPersistRecord("resp_old")
	old.CreatedAt = time.Now().Add(-3 * time.Hour).Unix()
	dst := NewStore(2*time.Hour, 10)
	if n := dst.Restore([]Record{old}); n != 0 {
		t.Fatalf("Restore = %d, want 0 (expired)", n)
	}
	if _, ok := dst.Get("resp_old", ""); ok {
		t.Fatal("expired record restored")
	}
	// A record created 1h ago restores, and its TTL keeps
	// running from the original creation time, not the restart.
	fresh := testPersistRecord("resp_fresh")
	fresh.CreatedAt = time.Now().Add(-time.Hour).Unix()
	if n := dst.Restore([]Record{fresh}); n != 1 {
		t.Fatalf("Restore = %d, want 1", n)
	}
}

func TestStoreRestoreRespectsCap(t *testing.T) {
	var recs []Record
	for i := 0; i < 5; i++ {
		recs = append(recs, testPersistRecord("resp_"+string(rune('a'+i))))
	}
	dst := NewStore(time.Hour, 3)
	if n := dst.Restore(recs); n != 5 {
		// Restore counts attempts, not survivors: all 5 were
		// offered, the cap kept the newest 3.
		t.Fatalf("Restore = %d, want 5 offered", n)
	}
	if dst.Len() != 3 {
		t.Fatalf("Len = %d, want 3 (cap)", dst.Len())
	}
	if _, ok := dst.Get("resp_a", ""); ok {
		t.Fatal("oldest record survived the cap on restore")
	}
	if _, ok := dst.Get("resp_e", ""); !ok {
		t.Fatal("newest record evicted on restore")
	}
}

func TestStoreRestoreZeroCreatedAtCountsAsNow(t *testing.T) {
	rec := testPersistRecord("resp_zero")
	rec.CreatedAt = 0
	dst := NewStore(time.Hour, 10)
	if n := dst.Restore([]Record{rec}); n != 1 {
		t.Fatalf("Restore = %d, want 1", n)
	}
	if _, ok := dst.Get("resp_zero", ""); !ok {
		t.Fatal("zero-CreatedAt record not restored")
	}
}
