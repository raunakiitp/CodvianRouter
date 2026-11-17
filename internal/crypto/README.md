# At-rest encryption — method

Provider API keys are encrypted at rest with **AES-256-GCM** (Go standard
library, `crypto/aes` + `crypto/cipher`). The encryption key is derived
with **Argon2id** from `golang.org/x/crypto` (pinned v0.24.0, compatible
with the `go 1.22` directive in `go.mod`).

## Key derivation

### No-passphrase mode (default)

1. On first use, the router creates a **32-byte random master secret**
   (`crypto/rand`) and stores it at `<data_dir>/master.key` with mode
   **0600**, written atomically (temp file + rename). Created by
   `crypto.New(dataDir)` / `loadOrCreateMasterSecret`.
2. On first use, it also creates a **16-byte random Argon2id salt** at
   `<data_dir>/kdf.salt` with mode **0600**, written atomically. The salt
   is created once and **reused afterwards** — a second `New()` on the
   same directory reads the same salt and derives the same key.
   `loadOrCreateSalt` refuses a salt file of the wrong length.
3. The AES-256 key is **`Argon2id(master secret, salt)`** with
   **time=1, memory=19456 KiB (19 MiB), threads=1, keyLen=32**.
   The parameters are deliberately modest: the input is a random
   32-byte secret, not a weak human password, so a single pass over
   19 MiB is ample while keeping Seal/Unseal fast on small machines.
4. `Seal` encrypts with a fresh random 12-byte nonce per call and
   returns **`"v2:" + base64(nonce || ciphertext)`**. `Unseal`
   reverses it and fails on any tampering (GCM authentication) or on a
   value sealed under a different master secret.

### Passphrase mode

`NewWithPassphrase(dataDir, passphrase)` derives the key as
**`Argon2id(passphrase, salt)`** with the same parameters and the same
`<data_dir>/kdf.salt` file. No `master.key` is created or read.
`NewFromPassphrase(passphrase, salt, dataDir)` is the test /
caller-managed-salt equivalent (salt must be exactly 16 bytes).

A passphrase vault has **no v1 key**, so `Unseal` of a `"v1:"` value
fails with an explicit error stating the value was sealed under a
non-passphrase vault. A passphrase vault can only unseal values sealed
by a vault with the same passphrase and salt; a wrong passphrase fails
the GCM authentication check.

### Caller-managed path (`NewFromSecret`)

`NewFromSecret(secret, dataDir)` — used by the provider registry and by
tests — derives the v2 key with Argon2id using a **deterministic salt,
`SHA-256(secret)[:16]`**, so two vaults built from the same secret
derive the same key without any salt file. It keeps the legacy v1 key
`SHA-256(secret)` for unsealing old values. Prefer `New` in production
code.

## Backward compatibility (v1 values)

Before this change the key was `SHA-256(master secret)` and `Seal`
emitted `"v1:"` values. That derivation is kept **only for unsealing**:

- `Unseal` accepts both `"v2:"` (Argon2id key) and `"v1:"`
  (`SHA-256(secret)` key) values in no-passphrase mode.
- `Seal` **never** emits `"v1:"` values.
- `Vault.ReSeal(old)` unseals a `"v1:"` or `"v2:"` value and re-seals
  it as a fresh `"v2:"` value — the migration primitive for stored
  values. Re-sealing a `"v2:"` value is a no-op in format (it still
  returns a new, different `"v2:"` value because of the fresh nonce).

## Rotation / migration — read this before touching master.key

**Rotating or replacing `master.key` orphans previously sealed values.**
The v2 key is derived from the secret, so a new secret derives a
different key and every old sealed value fails `Unseal` with a
decryption error. `ReSeal` only works **while the old secret is still
present**: the migration sequence is (1) load the vault with the old
secret, (2) `ReSeal` every stored value, (3) only then replace
`master.key` (and `kdf.salt`, if desired). There is no way to recover a
sealed value after its secret is gone — the sealed form alone is not
enough. The same applies to passphrase mode: changing the passphrase
orphans values sealed under the old one.

Back up `master.key` **and** `kdf.salt` together with the config, or
provider keys will need to be re-entered after a data-dir restore.
Losing `kdf.salt` alone is equally fatal: the salt is required to
re-derive the key, even though the salt itself is not secret.

## What this does NOT do (honest limitations)

- **The master secret is still a local file.** Security of the secret
  rests on local file permissions (0600), the data directory being
  0700, and the router being localhost-only. Anyone who can read
  `<data_dir>/master.key` (same user, root, or a backup that copies it)
  can decrypt every stored provider key. Argon2id slows an attacker
  who has the *sealed values* but not the secret; it does not help
  once the secret file itself is readable. That is the accepted Phase 1
  trade-off for a local-only tool.
- **No automatic rotation.** There is no scheduled or prompted key
  rotation; rotation is a manual operation using `ReSeal` as described
  above.
- **No passphrase by default.** The default `New` path uses the random
  master secret, not a passphrase. Passphrase mode exists
  (`NewWithPassphrase`) but nothing prompts for or manages a
  passphrase yet — that is a future UX decision, and a lost passphrase
  is unrecoverable by design.
- **Sealed values are not authenticated to a provider.** `Unseal` only
  proves the value was sealed by this Vault; the provider package must
  still validate the key against the upstream API.
- **The deterministic salt in `NewFromSecret` is weaker than a random
  salt** (an attacker who knows the secret can compute the salt, but
  they already have the secret). It exists so caller-managed vaults
  stay stateless; file-backed vaults always use the random `kdf.salt`.
