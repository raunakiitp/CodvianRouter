# At-rest encryption — method

Provider API keys are encrypted at rest with **AES-256-GCM** (Go standard
library, `crypto/aes` + `crypto/cipher`). There are **no external
dependencies**: `go.mod` is stdlib-only.

## Key derivation (what this build actually does)

1. On first use, the router creates a **32-byte random master secret**
   (`crypto/rand`) and stores it at `<data_dir>/master.key` with mode
   **0600**, written atomically (temp file + rename). The file is created
   by `crypto.New(dataDir)` / `loadOrCreateMasterSecret`.
2. The AES-256 key is **`SHA-256(master secret)`** — a single hash, not a
   password-based KDF.
3. `Seal` encrypts with a fresh random 12-byte nonce per call and returns
   `"v1:" + base64(nonce || ciphertext)`. `Unseal` reverses it and fails on
   any tampering (GCM authentication) or on a value sealed under a
   different master secret.

## What this does NOT do (honest limitations)

- **No Argon2id.** The original plan was to try `golang.org/x/crypto`
  Argon2id and fall back if `go get` failed. The fallback was chosen
  deliberately instead: keeping the module stdlib-only avoids a network
  dependency at build time and keeps the single-binary story simple.
  Argon2id would only add value with a human passphrase anyway — there is
  no passphrase in this design, the secret is a random machine-local file,
  so a KDF would not strengthen the threat model.
- **No passphrase.** Security of the master secret rests on local file
  permissions (0600), the data directory being 0700, and the router being
  localhost-only. Anyone who can read `<data_dir>/master.key` (same user,
  root, or a backup that copies it) can decrypt every stored provider key.
  That is the accepted Phase 1 trade-off for a local-only tool; it is the
  same trade-off as storing the keys in plaintext with tighter perms,
  except the keys are not sitting in the config file in the clear.
- **No key rotation yet.** Rotating the master secret orphans previously
  sealed values (they fail `Unseal`); a rotation/re-seal migration is a
  future phase. Back up `master.key` together with the config, or provider
  keys will need to be re-entered after a data-dir restore.
- **Sealed values are not authenticated to a provider.** `Unseal` only
  proves the value was sealed by this Vault; the provider package must
  still validate the key against the upstream API.

## Upgrade path

If a passphrase-based design is wanted later: add
`golang.org/x/crypto/argon2`, derive the key with Argon2id(passphrase,
salt) instead of SHA-256(secret), store the salt next to the ciphertext,
and bump the sealed format prefix from `v1:` to `v2:` (old `v1:` values
keep unsealing via the current path). The `Seal`/`Unseal` API and the
`ProviderConfig.APIKeyEncrypted` field do not need to change shape.
