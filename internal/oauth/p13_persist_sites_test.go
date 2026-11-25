package oauth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// p13FailingPersistDir returns a data dir whose state file has been
// replaced by a directory, so every saveLocked fails deterministically
// (the temp write succeeds, the rename onto the target cannot).
func p13FailingPersistDir(t *testing.T, dir string, iss *Issuer) {
	t.Helper()
	statePath := filepath.Join(dir, "oauth.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove state file: %v", err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatalf("replace state file with a directory: %v", err)
	}
}

func p13SilencePersistLog(t *testing.T) *bool {
	t.Helper()
	orig := persistLogf
	logged := false
	persistLogf = func(string, ...any) { logged = true }
	t.Cleanup(func() { persistLogf = orig })
	return &logged
}

// TestAUD_CODE_012DeleteClientPersistFailureRollsBack: deleting a client
// revokes its tokens, so a failed persist must not claim success — the
// credential would come back alive after a restart.
func TestAUD_CODE_012DeleteClientPersistFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	iss, err := NewIssuer(dir)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	c := registerPublicClient(t, iss)
	code, verifier := authorizeFor(t, iss, c)
	resp, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	logged := p13SilencePersistLog(t)
	p13FailingPersistDir(t, dir, iss)

	if ok := iss.DeleteClient(c.ID); ok {
		t.Fatal("DeleteClient returned true although the deletion could not be persisted")
	}
	if !*logged {
		t.Error("the persist failure was not logged")
	}
	// Fail closed: the client and its refresh token must still be in memory.
	iss.mu.Lock()
	_, clientPresent := iss.clients[c.ID]
	_, tokenPresent := iss.refresh[sha256Hex(resp.RefreshToken)]
	iss.mu.Unlock()
	if !clientPresent {
		t.Error("client removed from memory after a failed persist: a restart would resurrect it")
	}
	if !tokenPresent {
		t.Error("refresh token removed from memory after a failed persist")
	}
}

// TestAUD_CODE_012RefreshRotationPersistFailureFailsClosed: the rotated
// credential must not be handed out when nothing was written, and the
// consumed token must still work on retry.
func TestAUD_CODE_012RefreshRotationPersistFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	iss, err := NewIssuer(dir)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	c := registerPublicClient(t, iss)
	code, verifier := authorizeFor(t, iss, c)
	resp, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	logged := p13SilencePersistLog(t)
	p13FailingPersistDir(t, dir, iss)

	if _, err := iss.RefreshToken(resp.RefreshToken, c.ID, ""); err == nil {
		t.Fatal("RefreshToken returned tokens although the rotation could not be persisted")
	}
	if !*logged {
		t.Error("the persist failure was not logged")
	}
	// The consumed token is available again, so the client can retry once
	// persistence is healthy.
	iss.mu.Lock()
	_, present := iss.refresh[sha256Hex(resp.RefreshToken)]
	iss.mu.Unlock()
	if !present {
		t.Fatal("the consumed refresh token was not restored: the client would be locked out")
	}

	// Heal persistence and retry: the flow must work.
	if err := os.Remove(filepath.Join(dir, "oauth.json")); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if _, err := iss.RefreshToken(resp.RefreshToken, c.ID, ""); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
}

// TestAUD_CODE_012ExpiredRefreshRemovalLogsPersistFailure covers the
// expiry path: the token is refused either way, but the failure to
// record its removal must be visible.
func TestAUD_CODE_012ExpiredRefreshRemovalLogsPersistFailure(t *testing.T) {
	dir := t.TempDir()
	iss, err := NewIssuer(dir)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	c := registerPublicClient(t, iss)
	code, verifier := authorizeFor(t, iss, c)
	resp, err := iss.ExchangeCode(code, c.ID, c.RedirectURIs[0], verifier, "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	// Expire the token in memory, then break persistence.
	iss.mu.Lock()
	rt := iss.refresh[sha256Hex(resp.RefreshToken)]
	rt.ExpiresAt = time.Now().Add(-time.Hour)
	iss.refresh[sha256Hex(resp.RefreshToken)] = rt
	iss.mu.Unlock()

	logged := p13SilencePersistLog(t)
	p13FailingPersistDir(t, dir, iss)

	if _, err := iss.RefreshToken(resp.RefreshToken, c.ID, ""); err == nil {
		t.Fatal("an expired refresh token was accepted")
	}
	if !*logged {
		t.Error("the persist failure of the expired-token removal was swallowed")
	}
}
