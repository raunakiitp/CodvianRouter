// Package crypto encrypts provider API keys at rest with AES-256-GCM.
//
// METHOD (honest summary — see README.md in this directory for the full
// write-up): the AES-256 key is derived with Argon2id from either a
// 32-byte random master secret stored in the router's data directory
// (master.key, mode 0600) or, in passphrase mode, a user passphrase.
// The Argon2id salt is a 16-byte random value stored in kdf.salt
// (mode 0600). Sealed values carry a version prefix: "v1:" values use
// the legacy SHA-256(secret) key and are kept readable for backward
// compatibility, while Seal always emits "v2:" values under the Argon2id
// key. Seal/Unseal are methods on Vault, which owns the derived keys
// and the secret/salt files.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
)

// masterKeyFile is the file inside the data directory holding the random
// master secret.
const masterKeyFile = "master.key"

// masterSecretLen is the length of the random master secret in bytes.
const masterSecretLen = 32

// kdfSaltFile is the file inside the data directory holding the random
// Argon2id salt.
const kdfSaltFile = "kdf.salt"

// kdfSaltLen is the length of the Argon2id salt in bytes.
const kdfSaltLen = 16

// Argon2id parameters for key derivation. These are deliberately modest
// (19 MiB, one pass, one thread) so a single Seal/Unseal stays fast on
// small machines while still being far stronger than a bare hash; the
// secret being derived is a random 32-byte value, not a weak password.
const (
	argonTime    uint32 = 1
	argonMemory  uint32 = 19456 // KiB (19 MiB)
	argonThreads uint8  = 1
	argonKeyLen  uint32 = 32
)

// sealPrefixV1 marks values sealed with the legacy key, SHA-256(secret).
// Unseal still accepts them; Seal never emits them.
const sealPrefixV1 = "v1:"

// sealPrefixV2 marks values sealed with the Argon2id-derived key.
// Seal always emits this format.
const sealPrefixV2 = "v2:"

// Vault encrypts and decrypts small secrets (provider API keys) for
// storage. A Vault is bound to one data directory and one master secret
// (or passphrase); values sealed by one Vault cannot be unsealed by
// another Vault created from a different data directory or passphrase.
type Vault struct {
	// key is the v2 (Argon2id) encryption key used by Seal.
	key [32]byte
	// v1Key is the legacy SHA-256(secret) key, kept only so old "v1:"
	// values can still be unsealed. It is zero and hasV1 is false in
	// passphrase mode, where no v1 values can exist.
	v1Key   [32]byte
	hasV1   bool
	dataDir string
}

// New loads the master secret from dataDir (creating the directory and a
// fresh random secret when needed), loads or creates the Argon2id salt
// (kdf.salt), and returns a Vault ready to Seal and Unseal. The secret
// and salt files are created with 0600 permissions; existing files are
// left untouched apart from a best-effort permission tightening to 0600.
// Seal emits "v2:" values; Unseal accepts both "v2:" and legacy "v1:"
// values.
func New(dataDir string) (*Vault, error) {
	if dataDir == "" {
		return nil, errors.New("crypto: data directory must not be empty")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("crypto: cannot create data directory %q: %w", dataDir, err)
	}
	secret, err := loadOrCreateMasterSecret(dataDir)
	if err != nil {
		return nil, err
	}
	salt, err := loadOrCreateSalt(dataDir)
	if err != nil {
		return nil, err
	}
	return newVaultFromSecret(secret, salt, dataDir)
}

// NewFromSecret builds a Vault from an explicit master secret (at least 16
// bytes). The v2 encryption key is Argon2id(secret, salt) where the salt
// is deterministically derived as SHA-256(secret)[:16] — this path is for
// tests and for callers that manage the secret (and therefore the salt)
// themselves, so no salt file is read or written. The legacy v1 key
// SHA-256(secret) is kept so old "v1:" values still unseal. Prefer New
// in production code. dataDir is recorded for diagnostics only.
func NewFromSecret(secret []byte, dataDir string) (*Vault, error) {
	if len(secret) < 16 {
		return nil, fmt.Errorf("crypto: master secret must be at least 16 bytes (got %d)", len(secret))
	}
	saltSum := sha256.Sum256(secret)
	return newVaultFromSecret(secret, saltSum[:kdfSaltLen], dataDir)
}

// newVaultFromSecret derives the v2 key with Argon2id and the v1 key with
// SHA-256 from the given secret and salt. The caller must already have
// validated the secret length.
func newVaultFromSecret(secret, salt []byte, dataDir string) (*Vault, error) {
	if len(salt) != kdfSaltLen {
		return nil, fmt.Errorf("crypto: Argon2id salt must be %d bytes (got %d)", kdfSaltLen, len(salt))
	}
	v := &Vault{dataDir: dataDir, hasV1: true}
	copy(v.key[:], argon2.IDKey(secret, salt, argonTime, argonMemory, argonThreads, argonKeyLen))
	v.v1Key = sha256.Sum256(secret)
	return v, nil
}

// NewWithPassphrase builds a Vault from a user passphrase instead of a
// random master secret. The key is Argon2id(passphrase, salt), where the
// salt is the 16-byte random value in <dataDir>/kdf.salt (created on
// first use, reused afterwards). No master.key is created or read, and
// there is no v1 key: Unseal of a "v1:" value fails with a clear error.
// A passphrase vault can only unseal values sealed by a vault with the
// same passphrase and salt.
func NewWithPassphrase(dataDir, passphrase string) (*Vault, error) {
	if dataDir == "" {
		return nil, errors.New("crypto: data directory must not be empty")
	}
	if passphrase == "" {
		return nil, errors.New("crypto: passphrase must not be empty")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("crypto: cannot create data directory %q: %w", dataDir, err)
	}
	salt, err := loadOrCreateSalt(dataDir)
	if err != nil {
		return nil, err
	}
	return NewFromPassphrase(passphrase, salt, dataDir)
}

// NewFromPassphrase builds a passphrase Vault from an explicit salt
// (exactly 16 bytes). This exists mainly for tests and for callers that
// manage the salt themselves; prefer NewWithPassphrase in production
// code. The key is Argon2id(passphrase, salt); there is no v1 key, so
// Unseal of a "v1:" value fails. dataDir is recorded for diagnostics
// only.
func NewFromPassphrase(passphrase string, salt []byte, dataDir string) (*Vault, error) {
	if passphrase == "" {
		return nil, errors.New("crypto: passphrase must not be empty")
	}
	if len(salt) != kdfSaltLen {
		return nil, fmt.Errorf("crypto: Argon2id salt must be %d bytes (got %d)", kdfSaltLen, len(salt))
	}
	v := &Vault{dataDir: dataDir, hasV1: false}
	copy(v.key[:], argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemory, argonThreads, argonKeyLen))
	return v, nil
}

// loadOrCreateMasterSecret reads dataDir/master.key, creating it with a
// fresh 32-byte random secret (0600, written atomically) when absent.
func loadOrCreateMasterSecret(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, masterKeyFile)
	secret, err := os.ReadFile(path)
	if err == nil {
		if len(secret) < 16 {
			return nil, fmt.Errorf("crypto: master secret in %q is too short (%d bytes)", path, len(secret))
		}
		// Best-effort: make sure the secret is not world-readable.
		_ = os.Chmod(path, 0o600)
		return secret, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("crypto: cannot read master secret %q: %w", path, err)
	}
	secret = make([]byte, masterSecretLen)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return nil, fmt.Errorf("crypto: cannot generate master secret: %w", err)
	}
	if err := writeFileAtomic(dataDir, masterKeyFile, secret); err != nil {
		return nil, fmt.Errorf("crypto: cannot store master secret: %w", err)
	}
	return secret, nil
}

// loadOrCreateSalt reads dataDir/kdf.salt, creating it with a fresh
// 16-byte random salt (0600, written atomically) when absent. The salt
// is not secret, but it is kept at 0600 like the master secret so the
// data directory has uniform permissions.
func loadOrCreateSalt(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, kdfSaltFile)
	salt, err := os.ReadFile(path)
	if err == nil {
		if len(salt) != kdfSaltLen {
			return nil, fmt.Errorf("crypto: Argon2id salt in %q has wrong length (%d bytes, want %d)", path, len(salt), kdfSaltLen)
		}
		_ = os.Chmod(path, 0o600)
		return salt, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("crypto: cannot read Argon2id salt %q: %w", path, err)
	}
	salt = make([]byte, kdfSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("crypto: cannot generate Argon2id salt: %w", err)
	}
	if err := writeFileAtomic(dataDir, kdfSaltFile, salt); err != nil {
		return nil, fmt.Errorf("crypto: cannot store Argon2id salt: %w", err)
	}
	return salt, nil
}

// writeFileAtomic writes data to filepath.Join(dir, name) with mode 0600
// via a temp file in the same directory followed by a rename, so a crash
// mid-write never leaves a partial file at the destination.
func writeFileAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+"-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot create temp file for %q: %w", name, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write temp file for %q: %w", name, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot set permissions for %q: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot sync temp file for %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot close temp file for %q: %w", name, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("cannot store %q: %w", name, err)
	}
	return nil
}

// Seal encrypts plaintext with AES-256-GCM under a fresh random nonce and
// returns "v2:" + base64(nonce || ciphertext). Each call produces a
// different output even for the same plaintext.
func (v *Vault) Seal(plaintext []byte) (string, error) {
	if v == nil {
		return "", errors.New("crypto: nil vault")
	}
	return sealWithKey(v.key, sealPrefixV2, plaintext)
}

// sealWithKey encrypts plaintext under key and returns
// prefix + base64(nonce || ciphertext).
func sealWithKey(key [32]byte, prefix string, plaintext []byte) (string, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("crypto: cannot create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: cannot create GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: cannot generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Unseal reverses Seal. It accepts both "v2:" values (Argon2id key) and
// legacy "v1:" values (SHA-256 key). It fails on a wrong prefix, bad
// base64, a truncated value, or any tampering (GCM authentication
// failure) — and on values sealed under a different master secret or
// passphrase. A passphrase vault has no v1 key, so unsealing a "v1:"
// value with one fails with an explicit error.
func (v *Vault) Unseal(s string) ([]byte, error) {
	if v == nil {
		return nil, errors.New("crypto: nil vault")
	}
	switch {
	case len(s) > len(sealPrefixV2) && s[:len(sealPrefixV2)] == sealPrefixV2:
		return unsealWithKey(v.key, sealPrefixV2, s)
	case len(s) > len(sealPrefixV1) && s[:len(sealPrefixV1)] == sealPrefixV1:
		if !v.hasV1 {
			return nil, errors.New("crypto: sealed value was sealed under a non-passphrase vault (format \"v1:\") and cannot be unsealed by a passphrase vault")
		}
		return unsealWithKey(v.v1Key, sealPrefixV1, s)
	default:
		return nil, errors.New("crypto: sealed value has an unknown format (want \"v1:\" or \"v2:\" prefix)")
	}
}

// unsealWithKey decrypts a prefix + base64(nonce || ciphertext) value
// under key.
func unsealWithKey(key [32]byte, prefix, s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(s[len(prefix):])
	if err != nil {
		return nil, fmt.Errorf("crypto: sealed value is not valid base64: %w", err)
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("crypto: cannot create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: cannot create GCM: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("crypto: sealed value is too short")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: cannot decrypt sealed value (wrong key or tampered data): %w", err)
	}
	return plain, nil
}

// ReSeal unseals old (a "v1:" or "v2:" value) and re-seals the plaintext
// as a fresh "v2:" value. Use it to migrate stored values to the v2
// format after upgrading. It only works while the secret that sealed
// old is still present: if master.key has already been replaced or the
// passphrase changed, the Unseal step fails and the value is orphaned —
// there is no way to recover it from the sealed form alone.
func (v *Vault) ReSeal(old string) (string, error) {
	if v == nil {
		return "", errors.New("crypto: nil vault")
	}
	plain, err := v.Unseal(old)
	if err != nil {
		return "", err
	}
	return v.Seal(plain)
}

// DataDir returns the data directory this Vault was created for.
func (v *Vault) DataDir() string {
	if v == nil {
		return ""
	}
	return v.dataDir
}
