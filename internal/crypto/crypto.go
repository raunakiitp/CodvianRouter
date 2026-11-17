// Package crypto encrypts provider API keys at rest with AES-256-GCM.
//
// METHOD (honest summary — see README.md in this directory for the full
// write-up): the encryption key is SHA-256 of a 32-byte random master
// secret stored in the router's data directory (master.key, mode 0600).
// There is no passphrase and no Argon2id KDF in this build: the
// golang.org/x/crypto dependency was deliberately avoided to keep the
// module stdlib-only, so the secret's safety rests on local file
// permissions and the router being localhost-only. Seal/Unseal are methods
// on Vault, which owns the derived key and the master-secret file.
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
)

// masterKeyFile is the file inside the data directory holding the random
// master secret.
const masterKeyFile = "master.key"

// masterSecretLen is the length of the random master secret in bytes.
const masterSecretLen = 32

// sealPrefix marks the sealed format version so the format can evolve
// without silently mis-decrypting old values.
const sealPrefix = "v1:"

// Vault encrypts and decrypts small secrets (provider API keys) for
// storage. A Vault is bound to one data directory and one master secret;
// values sealed by one Vault cannot be unsealed by another Vault created
// from a different data directory.
type Vault struct {
	key     [32]byte
	dataDir string
}

// New loads the master secret from dataDir (creating the directory and a
// fresh random secret when needed) and returns a Vault ready to Seal and
// Unseal. The master secret file is created with 0600 permissions; an
// existing file is left untouched apart from a best-effort permission
// tightening to 0600.
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
	return NewFromSecret(secret, dataDir)
}

// NewFromSecret builds a Vault from an explicit master secret (at least 16
// bytes). The encryption key is SHA-256(secret). This exists mainly for
// tests and for callers that manage the secret themselves; prefer New in
// production code. dataDir is recorded for diagnostics only.
func NewFromSecret(secret []byte, dataDir string) (*Vault, error) {
	if len(secret) < 16 {
		return nil, fmt.Errorf("crypto: master secret must be at least 16 bytes (got %d)", len(secret))
	}
	v := &Vault{dataDir: dataDir}
	v.key = sha256.Sum256(secret)
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
	tmp, err := os.CreateTemp(dataDir, ".master.key-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("crypto: cannot create temp master secret: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(secret); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("crypto: cannot write temp master secret: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("crypto: cannot set master secret permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("crypto: cannot sync temp master secret: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("crypto: cannot close temp master secret: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, fmt.Errorf("crypto: cannot store master secret: %w", err)
	}
	return secret, nil
}

// Seal encrypts plaintext with AES-256-GCM under a fresh random nonce and
// returns "v1:" + base64(nonce || ciphertext). Each call produces a
// different output even for the same plaintext.
func (v *Vault) Seal(plaintext []byte) (string, error) {
	if v == nil {
		return "", errors.New("crypto: nil vault")
	}
	block, err := aes.NewCipher(v.key[:])
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
	return sealPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Unseal reverses Seal. It fails on a wrong prefix, bad base64, a
// truncated value, or any tampering (GCM authentication failure) — and on
// values sealed under a different master secret.
func (v *Vault) Unseal(s string) ([]byte, error) {
	if v == nil {
		return nil, errors.New("crypto: nil vault")
	}
	if len(s) <= len(sealPrefix) || s[:len(sealPrefix)] != sealPrefix {
		return nil, errors.New("crypto: sealed value has an unknown format (want \"v1:\" prefix)")
	}
	raw, err := base64.StdEncoding.DecodeString(s[len(sealPrefix):])
	if err != nil {
		return nil, fmt.Errorf("crypto: sealed value is not valid base64: %w", err)
	}
	block, err := aes.NewCipher(v.key[:])
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

// DataDir returns the data directory this Vault was created for.
func (v *Vault) DataDir() string {
	if v == nil {
		return ""
	}
	return v.dataDir
}
