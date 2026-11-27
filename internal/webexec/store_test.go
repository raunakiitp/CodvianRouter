package webexec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/crypto"
)

func testState(value string) *SessionState {
	return &SessionState{
		Cookies:    []Cookie{{Name: mockSessionCookie, Value: value, Domain: "example.com", Path: "/"}},
		CapturedAt: time.Now(),
	}
}

func TestSessionStoreRoundTripWithoutVault(t *testing.T) {
	store := NewSessionStore(nil)
	if err := store.Save("chatgpt-web", testState("secret-cookie-value")); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.Load("chatgpt-web")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Cookies) != 1 || got.Cookies[0].Value != "secret-cookie-value" {
		t.Fatalf("round trip lost the cookie: %+v", got.Cookies)
	}
}

func TestSessionStoreSealsWithVault(t *testing.T) {
	dir := t.TempDir()
	vault, err := crypto.New(dir)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	store := NewSessionStore(vault)
	if err := store.Save("claude-web", testState("sessionKey-plain-value")); err != nil {
		t.Fatalf("save: %v", err)
	}

	// At rest, the stored blob must be ciphertext: the plaintext
	// cookie value and name must not appear anywhere in the stored
	// data (this is the oauth_conn.go sealing convention).
	store.mu.Lock()
	st := store.sessions["claude-web"]
	store.mu.Unlock()
	if st == nil {
		t.Fatal("no stored entry")
	}
	if strings.Contains(st.Data, "sessionKey-plain-value") || strings.Contains(st.Data, mockSessionCookie) {
		t.Fatalf("stored blob carries plaintext session material: %s", st.Data)
	}

	// Round trip through the same vault.
	got, err := store.Load("claude-web")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Cookies[0].Value != "sessionKey-plain-value" {
		t.Fatalf("decrypt mismatch: %+v", got.Cookies)
	}
}

func TestSessionStoreVaultIsDataDirBound(t *testing.T) {
	dir := t.TempDir()
	vault, err := crypto.New(dir)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	store := NewSessionStore(vault)
	if err := store.Save("poe-web", testState("p-b-cookie")); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A DIFFERENT data dir's vault cannot unseal the blob: loading
	// through a foreign vault is stale-state (401) semantics, never a
	// garbage read.
	other, err := crypto.New(t.TempDir())
	if err != nil {
		t.Fatalf("other vault: %v", err)
	}
	foreign := NewSessionStore(other)
	// Simulate the same stored blob loaded by a foreign vault by
	// copying the ciphertext entry.
	foreign.mu.Lock()
	foreign.sessions["poe-web"] = foreignEntryFrom(store.sessions["poe-web"])
	foreign.mu.Unlock()
	_, err = foreign.Load("poe-web")
	if !errors.Is(err, ErrStaleSession) {
		t.Fatalf("foreign vault load should classify as stale session, got: %v", err)
	}
}

func foreignEntryFrom(st *storedSession) *storedSession {
	cp := *st
	return &cp
}

func TestSessionStoreRefusesUnknownCatalogID(t *testing.T) {
	store := NewSessionStore(nil)
	if err := store.Save("some-api-provider", testState("x")); !errors.Is(err, ErrUnknownSurface) {
		t.Fatalf("unknown catalog ID must be refused: %v", err)
	}
}

func TestSessionStoreRefusesEmptyState(t *testing.T) {
	store := NewSessionStore(nil)
	if err := store.Save("kimi-web", &SessionState{}); err == nil {
		t.Fatal("a state with zero cookies must be refused (silent later 401)")
	}
}

func TestSessionStoreMissingLoadIsErrNoSession(t *testing.T) {
	store := NewSessionStore(nil)
	_, err := store.Load("deepseek-web")
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing load must be ErrNoSession: %v", err)
	}
}

func TestSessionStoreOldStateIsStale(t *testing.T) {
	store := NewSessionStore(nil)
	state := testState("old")
	state.CapturedAt = time.Now().Add(-31 * 24 * time.Hour)
	if err := store.Save("grok-web", state); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, err := store.Load("grok-web")
	if !errors.Is(err, ErrStaleSession) {
		t.Fatalf("31-day-old state must classify stale: %v", err)
	}
}

func TestSessionStoreCorruptBlobIsStale(t *testing.T) {
	store := NewSessionStore(nil)
	store.mu.Lock()
	store.sessions["gemini-web"] = &storedSession{CatalogID: "gemini-web", Data: "not-json-at-all", CapturedAt: time.Now()}
	store.mu.Unlock()
	_, err := store.Load("gemini-web")
	if !errors.Is(err, ErrStaleSession) {
		t.Fatalf("corrupt blob must classify stale: %v", err)
	}
}

func TestSessionStoreDeleteAndList(t *testing.T) {
	store := NewSessionStore(nil)
	if err := store.Save("lmarena", testState("a")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.Save("syntx", testState("b")); err != nil {
		t.Fatalf("save: %v", err)
	}
	list := store.List()
	if len(list) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(list))
	}
	// Status carries NO session material.
	for _, s := range list {
		if s.CookieNum != 1 {
			t.Fatalf("%s: cookie count %d, want 1", s.CatalogID, s.CookieNum)
		}
	}
	if err := store.Delete("lmarena"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if store.Has("lmarena") {
		t.Fatal("delete did not remove the session")
	}
	if err := store.Delete("lmarena"); err == nil {
		t.Fatal("double delete must error")
	}
}

func TestSessionStateRedactedJSON(t *testing.T) {
	state := testState("super-secret-value")
	red := state.RedactedJSON()
	if strings.Contains(red, "super-secret-value") {
		t.Fatalf("redacted JSON leaks the cookie value: %s", red)
	}
	if !strings.Contains(red, `"name":"mocksid"`) {
		t.Fatalf("redacted JSON should keep the cookie name: %s", red)
	}
}

func TestSessionStorePersistsSealedBlob(t *testing.T) {
	dir := t.TempDir()
	vault, err := crypto.New(dir)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	store := NewSessionStore(vault)
	if err := store.Save("venice-web", testState("persist-me-secret")); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The on-disk blob is ciphertext: no plaintext session material.
	files, err := filepath.Glob(filepath.Join(dir, "webexec-sessions", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected exactly one persisted blob, got %v (%v)", files, err)
	}
	blob, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if strings.Contains(string(blob), "persist-me-secret") || strings.Contains(string(blob), mockSessionCookie) {
		t.Fatalf("persisted blob carries plaintext session material: %s", blob)
	}

	// A FRESH store on the SAME data dir (router restart) loads it.
	reborn := NewSessionStore(vault)
	reborn.mu.Lock()
	memEmpty := len(reborn.sessions) == 0
	reborn.mu.Unlock()
	if !memEmpty {
		t.Fatal("fresh store must start with an empty memory map")
	}
	got, err := reborn.Load("venice-web")
	if err != nil {
		t.Fatalf("load after restart: %v", err)
	}
	if got.Cookies[0].Value != "persist-me-secret" {
		t.Fatalf("restart round trip lost the value: %+v", got.Cookies)
	}

	// A fresh store on a DIFFERENT data dir cannot decrypt it.
	otherVault, err := crypto.New(t.TempDir())
	if err != nil {
		t.Fatalf("other vault: %v", err)
	}
	foreign := NewSessionStore(otherVault)
	// Point the foreign store at the same blob directory to prove the
	// KEY binding (not the path) is what protects the blob.
	foreign.dir = filepath.Join(dir, "webexec-sessions")
	if _, err := foreign.Load("venice-web"); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("foreign vault must classify as stale, got: %v", err)
	}

	// Delete removes memory AND the persisted blob.
	if err := reborn.Delete("venice-web"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	remaining, _ := filepath.Glob(filepath.Join(dir, "webexec-sessions", "*.json"))
	if len(remaining) != 0 {
		t.Fatalf("delete left persisted blobs: %v", remaining)
	}
}

func TestSessionStoreNoVaultWritesNothing(t *testing.T) {
	store := NewSessionStore(nil)
	if err := store.Save("t3-web", testState("mem-only")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if store.dir != "" {
		t.Fatal("no-vault store must be memory-only")
	}
}
