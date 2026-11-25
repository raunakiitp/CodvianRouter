package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// Multi-key pooling (CHECKLIST 175): a provider may hold several API
// keys; the router tries preferred keys first, reserve keys only
// when every preferred key is exhausted, and a dead key is cooled
// down per-key. These tests cover the registry side: pool
// construction, legacy synthesis, add/remove/role, persistence
// round-trip, and per-key clients.

func testKeyRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	dir := t.TempDir()
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-keypool!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg, err := NewFileRegistry(filepath.Join(dir, "providers.json"),
		WithCipher(NewVaultCipher(vault)),
		WithRegistryAllowPrivateDestinations(true)) // tests add local mock servers (SSRF bypass is test-only)
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	return reg, dir
}

func TestAddPopulatesPool(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()
	reg, _ := testKeyRegistry(t)

	p, err := reg.Add(srv.URL, "sk-first")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(p.Keys) != 1 {
		t.Fatalf("Keys = %d, want 1", len(p.Keys))
	}
	k := p.Keys[0]
	if k.Role != KeyRolePreferred {
		t.Errorf("added key role = %q, want preferred", k.Role)
	}
	if k.ID == "" || !strings.HasPrefix(k.ID, "key_") {
		t.Errorf("added key ID = %q, want key_…", k.ID)
	}
	if k.Fingerprint != KeyFingerprint("sk-first") {
		t.Errorf("fingerprint = %q, want %q", k.Fingerprint, KeyFingerprint("sk-first"))
	}
	if k.Encrypted == "" || k.Encrypted == "sk-first" {
		t.Error("key must be stored encrypted, never plaintext")
	}
	// The primary key is the added key.
	if pk, ok := p.PrimaryKey(); !ok || pk.ID != k.ID {
		t.Errorf("PrimaryKey = %+v, want the added key", pk)
	}
	if got := p.PreferredKeys(); len(got) != 1 || got[0].ID != k.ID {
		t.Errorf("PreferredKeys = %v, want [added]", got)
	}
	if got := p.ReserveKeys(); len(got) != 0 {
		t.Errorf("ReserveKeys = %v, want []", got)
	}
}

func TestLegacyProviderSynthesizesPool(t *testing.T) {
	// A provider stored before pools existed (Keys empty) still
	// routes through one preferred key synthesized from the legacy
	// ciphertext — stable across reloads, never the plaintext.
	srv := openAIMock(t)
	defer srv.Close()
	reg, dir := testKeyRegistry(t)

	p, err := reg.Add(srv.URL, "sk-legacy")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Strip the pool back to the pre-pool shape on disk, then reload.
	stored, err := reg.store.Load()
	if err != nil {
		t.Fatalf("store Load: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored providers = %d, want 1", len(stored))
	}
	stored[0].Keys = nil
	if err := reg.store.Save(stored); err != nil {
		t.Fatalf("store Save: %v", err)
	}
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-keypool!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg2, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	p2, ok := reg2.Get(p.ID)
	if !ok {
		t.Fatalf("reloaded provider missing")
	}
	keys := p2.EffectiveKeys()
	if len(keys) != 1 {
		t.Fatalf("EffectiveKeys = %d, want 1", len(keys))
	}
	if keys[0].Role != KeyRolePreferred {
		t.Errorf("legacy key role = %q, want preferred", keys[0].Role)
	}
	if keys[0].Encrypted != p2.APIKeyEncrypted {
		t.Error("legacy key must seal the same ciphertext as APIKeyEncrypted")
	}
	// Stable across a second reload.
	reg3, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("reload 2: %v", err)
	}
	p3, _ := reg3.Get(p.ID)
	if got := p3.EffectiveKeys()[0].ID; got != keys[0].ID {
		t.Errorf("legacy key ID changed across reloads: %q vs %q", got, keys[0].ID)
	}
}

func TestAddKeySecondAndReserve(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()
	reg, _ := testKeyRegistry(t)
	ctx := context.Background()

	p, err := reg.Add(srv.URL, "sk-first")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	second, err := reg.AddKey(ctx, p.ID, "sk-second", "preferred")
	if err != nil {
		t.Fatalf("AddKey preferred: %v", err)
	}
	reserve, err := reg.AddKey(ctx, p.ID, "sk-reserve", "reserve")
	if err != nil {
		t.Fatalf("AddKey reserve: %v", err)
	}
	got, ok := reg.Get(p.ID)
	if !ok {
		t.Fatalf("provider missing after AddKey")
	}
	if len(got.Keys) != 3 {
		t.Fatalf("Keys = %d, want 3 (first + second + reserve)", len(got.Keys))
	}
	if got.Keys[1].ID != second.ID || got.Keys[2].ID != reserve.ID {
		t.Errorf("pool order = %v, want [first second reserve]", got.Keys)
	}
	if pref := got.PreferredKeys(); len(pref) != 2 {
		t.Errorf("PreferredKeys = %d, want 2", len(pref))
	}
	if res := got.ReserveKeys(); len(res) != 1 || res[0].ID != reserve.ID {
		t.Errorf("ReserveKeys = %v, want [reserve]", res)
	}
	// The primary is still the first key.
	if pk, _ := got.PrimaryKey(); pk.ID != p.Keys[0].ID {
		t.Errorf("PrimaryKey = %q, want first key %q", pk.ID, p.Keys[0].ID)
	}
	// Per-key clients build and use their own key.
	for _, k := range []ProviderKey{got.Keys[0], second, reserve} {
		c, err := reg.ClientForKey(p.ID, k.ID)
		if err != nil {
			t.Fatalf("ClientForKey %s: %v", k.ID, err)
		}
		if err := c.TestKey(ctx); err != nil {
			t.Fatalf("TestKey via %s: %v", k.ID, err)
		}
	}
}

func TestAddKeyRejects(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()
	reg, _ := testKeyRegistry(t)
	ctx := context.Background()

	p, err := reg.Add(srv.URL, "sk-first")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := reg.AddKey(ctx, p.ID, "", "preferred"); err == nil {
		t.Error("AddKey with an empty key must fail")
	}
	if _, err := reg.AddKey(ctx, p.ID, "sk-other", "bogus"); err == nil {
		t.Error("AddKey with an unknown role must fail")
	}
	if _, err := reg.AddKey(ctx, p.ID, "sk-first", "preferred"); err == nil {
		t.Error("AddKey with the same credential twice must fail")
	}
	if _, err := reg.AddKey(ctx, "prov_nope", "sk-other", "preferred"); err == nil {
		t.Error("AddKey for an unknown provider must fail")
	}
	// A key the upstream rejects (401) is refused by AddKey too:
	// this mock 401s only when the presented key is "sk-bad".
	keyed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"model":"model-a","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer keyed.Close()
	pb, err := reg.Add(keyed.URL, "sk-good")
	if err != nil {
		t.Fatalf("Add good provider: %v", err)
	}
	if _, err := reg.AddKey(ctx, pb.ID, "sk-bad", "preferred"); err == nil {
		t.Error("AddKey with a 401 key must fail")
	}
	if _, err := reg.AddKey(ctx, pb.ID, "sk-also-good", "preferred"); err != nil {
		t.Errorf("AddKey with a good key must pass: %v", err)
	}
}

func TestRotateKeyInPlace(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()
	reg, dir := testKeyRegistry(t)
	ctx := context.Background()

	p, err := reg.Add(srv.URL, "sk-first")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	first := p.Keys[0]
	second, err := reg.AddKey(ctx, p.ID, "sk-second", "reserve")
	if err != nil {
		t.Fatalf("AddKey reserve: %v", err)
	}
	rotated, err := reg.RotateKey(ctx, p.ID, first.ID, "sk-rotated")
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	// Identity is preserved: same ID, same role — only the
	// credential (and its fingerprint) changes.
	if rotated.ID != first.ID {
		t.Errorf("rotated ID = %q, want %q (rotation keeps identity)", rotated.ID, first.ID)
	}
	if rotated.Role != KeyRolePreferred {
		t.Errorf("rotated role = %q, want preferred", rotated.Role)
	}
	if rotated.Fingerprint != KeyFingerprint("sk-rotated") {
		t.Errorf("rotated fingerprint = %q, want %q", rotated.Fingerprint, KeyFingerprint("sk-rotated"))
	}
	if rotated.CreatedAt.Before(first.CreatedAt) {
		t.Errorf("rotated CreatedAt = %v, before the original %v", rotated.CreatedAt, first.CreatedAt)
	}
	if rotated.Encrypted == "" || rotated.Encrypted == "sk-rotated" {
		t.Error("rotated key must be stored encrypted, never plaintext")
	}
	got, ok := reg.Get(p.ID)
	if !ok {
		t.Fatalf("provider missing after RotateKey")
	}
	if len(got.Keys) != 2 {
		t.Fatalf("Keys = %d, want 2 (rotate replaces, never adds)", len(got.Keys))
	}
	if got.Keys[0].ID != first.ID || got.Keys[0].Fingerprint != KeyFingerprint("sk-rotated") {
		t.Errorf("pool[0] = %+v, want the rotated first key", got.Keys[0])
	}
	if got.Keys[1].ID != second.ID || got.Keys[1].Fingerprint != KeyFingerprint("sk-second") {
		t.Errorf("pool[1] = %+v, want the untouched second key", got.Keys[1])
	}
	// The primary is still the first key, now with the new credential.
	if pk, _ := got.PrimaryKey(); pk.ID != first.ID {
		t.Errorf("PrimaryKey = %q, want first key %q", pk.ID, first.ID)
	}
	// The rotated key actually routes: a client built for it passes
	// the live key test against the mock.
	c, err := reg.ClientForKey(p.ID, first.ID)
	if err != nil {
		t.Fatalf("ClientForKey rotated: %v", err)
	}
	if err := c.TestKey(ctx); err != nil {
		t.Fatalf("TestKey via rotated key: %v", err)
	}
	// Persistence round-trip: reload and the rotation survives,
	// with the old credential gone.
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-keypool!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg2, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	p2, ok := reg2.Get(p.ID)
	if !ok {
		t.Fatalf("reloaded provider missing")
	}
	if pk, _ := p2.PrimaryKey(); pk.Fingerprint != KeyFingerprint("sk-rotated") {
		t.Errorf("reloaded primary fingerprint = %q, want the rotated one", pk.Fingerprint)
	}
	// Refusals: same credential twice (its own, or the other key's),
	// unknown key, unknown provider, empty key.
	if _, err := reg.RotateKey(ctx, p.ID, first.ID, "sk-rotated"); err == nil {
		t.Error("RotateKey to the same credential must fail")
	}
	if _, err := reg.RotateKey(ctx, p.ID, first.ID, "sk-second"); err == nil {
		t.Error("RotateKey to another key's credential must fail")
	}
	if _, err := reg.RotateKey(ctx, p.ID, "key_nope", "sk-other"); err == nil {
		t.Error("RotateKey for an unknown key must fail")
	}
	if _, err := reg.RotateKey(ctx, "prov_nope", first.ID, "sk-other"); err == nil {
		t.Error("RotateKey for an unknown provider must fail")
	}
	if _, err := reg.RotateKey(ctx, p.ID, first.ID, ""); err == nil {
		t.Error("RotateKey with an empty key must fail")
	}
}

func TestRotateKeyRefusesBadKey(t *testing.T) {
	// A key the upstream rejects (401) is refused by RotateKey too,
	// and the pool keeps the original credential: the rotation
	// never partially applies.
	keyed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"model":"model-a","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer keyed.Close()
	reg, _ := testKeyRegistry(t)
	ctx := context.Background()

	p, err := reg.Add(keyed.URL, "sk-good")
	if err != nil {
		t.Fatalf("Add good provider: %v", err)
	}
	if _, err := reg.RotateKey(ctx, p.ID, p.Keys[0].ID, "sk-bad"); err == nil {
		t.Error("RotateKey with a 401 key must fail")
	}
	got, ok := reg.Get(p.ID)
	if !ok {
		t.Fatalf("provider missing after refused rotate")
	}
	if pk, _ := got.PrimaryKey(); pk.Fingerprint != KeyFingerprint("sk-good") {
		t.Errorf("primary fingerprint after refused rotate = %q, want the original sk-good", pk.Fingerprint)
	}
	if _, err := reg.RotateKey(ctx, p.ID, p.Keys[0].ID, "sk-also-good"); err != nil {
		t.Errorf("RotateKey with a good key must pass: %v", err)
	}
}

func TestRemoveKeyAndRole(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()
	reg, _ := testKeyRegistry(t)
	ctx := context.Background()

	p, err := reg.Add(srv.URL, "sk-first")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	second, err := reg.AddKey(ctx, p.ID, "sk-second", "reserve")
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	// The last key cannot be removed.
	if err := reg.RemoveKey(p.ID, second.ID); err != nil {
		t.Fatalf("RemoveKey second: %v", err)
	}
	got, _ := reg.Get(p.ID)
	if len(got.Keys) != 1 {
		t.Fatalf("Keys after remove = %d, want 1", len(got.Keys))
	}
	if err := reg.RemoveKey(p.ID, got.Keys[0].ID); err == nil {
		t.Error("removing the last key must fail")
	}
	if err := reg.RemoveKey(p.ID, "key_nope"); err == nil {
		t.Error("removing an unknown key must fail")
	}
	// Role change: preferred -> reserve and back.
	third, err := reg.AddKey(ctx, p.ID, "sk-third", "preferred")
	if err != nil {
		t.Fatalf("AddKey third: %v", err)
	}
	if err := reg.SetKeyRole(p.ID, third.ID, "reserve"); err != nil {
		t.Fatalf("SetKeyRole: %v", err)
	}
	got, _ = reg.Get(p.ID)
	if res := got.ReserveKeys(); len(res) != 1 || res[0].ID != third.ID {
		t.Errorf("ReserveKeys after role change = %v, want [third]", res)
	}
	if err := reg.SetKeyRole(p.ID, third.ID, "bogus"); err == nil {
		t.Error("SetKeyRole with an unknown role must fail")
	}
	if err := reg.SetKeyRole(p.ID, "key_nope", "reserve"); err == nil {
		t.Error("SetKeyRole for an unknown key must fail")
	}
}

func TestKeyPoolRoundTrip(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()
	reg, dir := testKeyRegistry(t)
	ctx := context.Background()

	p, err := reg.Add(srv.URL, "sk-first")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := reg.AddKey(ctx, p.ID, "sk-second", "reserve"); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	// Display pool: no ciphertext anywhere.
	pool, err := reg.KeyPool(p.ID)
	if err != nil {
		t.Fatalf("KeyPool: %v", err)
	}
	if len(pool) != 2 {
		t.Fatalf("KeyPool = %d, want 2", len(pool))
	}
	for _, k := range pool {
		if k.Encrypted != "" {
			t.Errorf("KeyPool key %s leaks ciphertext", k.ID)
		}
		if k.ID == "" || k.Fingerprint == "" {
			t.Errorf("KeyPool key missing id/fingerprint: %+v", k)
		}
	}
	if pool[1].Role != KeyRoleReserve {
		t.Errorf("KeyPool[1] role = %q, want reserve", pool[1].Role)
	}
	// Reload from disk: the pool survives.
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-keypool!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg2, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	p2, ok := reg2.Get(p.ID)
	if !ok {
		t.Fatalf("reloaded provider missing")
	}
	if len(p2.Keys) != 2 || p2.Keys[1].Role != KeyRoleReserve {
		t.Errorf("reloaded pool = %+v, want 2 keys with [1] reserve", p2.Keys)
	}
}

func TestClientForUsesPrimaryKey(t *testing.T) {
	// ClientFor must serve through the first preferred key even
	// after a reserve key is added — and after the first key is
	// demoted to reserve, through the next preferred key.
	srv := openAIMock(t)
	defer srv.Close()
	reg, _ := testKeyRegistry(t)
	ctx := context.Background()

	p, err := reg.Add(srv.URL, "sk-first")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	second, err := reg.AddKey(ctx, p.ID, "sk-second", "preferred")
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if _, err := reg.AddKey(ctx, p.ID, "sk-reserve", "reserve"); err != nil {
		t.Fatalf("AddKey reserve: %v", err)
	}
	// Demote the first key to reserve: the primary becomes second.
	if err := reg.SetKeyRole(p.ID, p.Keys[0].ID, "reserve"); err != nil {
		t.Fatalf("SetKeyRole: %v", err)
	}
	got, _ := reg.Get(p.ID)
	pk, ok := got.PrimaryKey()
	if !ok || pk.ID != second.ID {
		t.Errorf("PrimaryKey = %+v, want second key %q", pk, second.ID)
	}
	c, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if err := c.TestKey(ctx); err != nil {
		t.Fatalf("TestKey via primary: %v", err)
	}
}
