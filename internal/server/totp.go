// Package server — TOTP two-factor authentication for dashboard login.
//
// TOTP is an opt-in second factor for the dashboard operator. When enabled,
// login requires both the operator password and a valid TOTP code (or a
// single-use backup code). The TOTP secret is encrypted at rest with the
// same AES-256-GCM Vault used for provider keys; backup codes are hashed
// with Argon2id before storage.
//
// Enrollment is two-step:
//  1. POST /dashboard/auth/totp/enroll — generates a secret, returns an
//     otpauth:// URI (rendered as a QR code by the dashboard shell), and
//     stores the secret in a pending state.
//  2. POST /dashboard/auth/totp/confirm — presents a valid TOTP code to
//     prove possession; the secret is then activated and backup codes
//     are minted.
//
// Login with TOTP enabled:
//  1. POST /dashboard/auth/login with password — returns 202 with
//     totp_required=true (no session cookie yet).
//  2. POST /dashboard/auth/totp/verify with the TOTP code — sets the
//     session cookie on success.
//
// Disabling TOTP requires the operator password plus a current TOTP code
// (or an admin gateway key in place of both).
package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// totpAuthFile holds the encrypted TOTP state for the dashboard operator
// inside the data directory (mode 0600, atomic rename).
const totpAuthFile = "dashboard_totp.json"

// totpIssueWindow is how long a freshly-enrolled but unconfirmed secret
// may be used for confirmation before it expires.
const totpIssueWindow = 10 * time.Minute

// totpCodeDigits is the standard 6-digit TOTP code.
const totpCodeDigits = 6

// totpInterval is the TOTP time step in seconds (standard 30s).
const totpInterval = 30

// totpIssuer is the issuer name embedded in the otpauth:// URI.
const totpIssuer = "Codvian Router"

// totpBackupCodeCount is the number of single-use backup codes minted
// at enrollment or regeneration.
const totpBackupCodeCount = 8

// totpBackupCodeLen is the length of each backup code in characters.
const totpBackupCodeLen = 12

// totpReplayWindow tracks used TOTP codes within one time step to prevent
// replay. A code is valid once per step; the store forgets entries older
// than two steps.
const totpReplayWindow = 2 * totpInterval * time.Second

// totpState is the persisted TOTP state for the operator.
type totpState struct {
	Version      int    `json:"version"`       // 1
	Enabled      bool   `json:"enabled"`       // TOTP required at login
	SecretCipher string `json:"secret_cipher"` // Vault-encrypted TOTP secret (base64), "" when disabled
	// Backup codes are stored as Argon2id hashes; the plaintext is shown
	// to the operator exactly once at enrollment/regeneration.
	BackupCodeHashes []string `json:"backup_code_hashes"`
}

// totpPending is an in-memory, short-lived enrollment pending confirmation.
type totpPending struct {
	secret   string // base32-encoded TOTP secret
	issuedAt time.Time
}

// totpStore is the dashboard operator's TOTP state: encrypted secret,
// enabled flag, backup-code hashes, and in-memory replay protection.
type totpStore struct {
	mu      sync.Mutex
	vault   *crypto.Vault // nil = in-memory only (no data dir / vault unavailable)
	file    string        // "" = in-memory only
	state   totpState
	pending *totpPending // nil = no pending enrollment
	// usedCodes tracks TOTP codes used within the current step to
	// prevent replay. Key = code + step, value = time first seen.
	usedCodes map[string]time.Time
}

// newTotpStore creates a TOTP store. When vault is nil the store works
// in-memory only (no persistence), suitable for tests and headless runs.
func newTotpStore(vault *crypto.Vault, dataDir string) *totpStore {
	st := &totpStore{
		vault:     vault,
		usedCodes: map[string]time.Time{},
	}
	if dataDir != "" && vault != nil {
		st.file = dataDir + "/" + totpAuthFile
		st.load()
	}
	return st
}

// load reads the persisted TOTP state. A missing file is not an error;
// a corrupt file is (fail closed).
func (st *totpStore) load() {
	raw, err := os.ReadFile(st.file)
	if err != nil {
		return // no state yet
	}
	var s totpState
	if err := json.Unmarshal(raw, &s); err != nil {
		// Corrupt TOTP state: fail closed by leaving zero state.
		// The operator must re-enroll.
		return
	}
	if s.Version != 1 {
		return
	}
	st.state = s
}

// save persists the TOTP state atomically. A nil vault means no
// persistence (in-memory only).
func (st *totpStore) save() error {
	if st.vault == nil || st.file == "" {
		return nil
	}
	raw, err := json.Marshal(st.state)
	if err != nil {
		return err
	}
	sealed, err := st.vault.Seal(raw)
	if err != nil {
		return fmt.Errorf("totp: seal state: %w", err)
	}
	tmp := st.file + ".tmp"
	if err := os.WriteFile(tmp, []byte(sealed), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.file)
}

// Enabled reports whether TOTP is currently required for dashboard login.
func (st *totpStore) Enabled() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.state.Enabled
}

// Secret returns the base32-encoded TOTP secret, or "" when none is set.
func (st *totpStore) Secret() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.state.SecretCipher == "" || st.vault == nil {
		return st.state.SecretCipher
	}
	plain, err := st.vault.Unseal(st.state.SecretCipher)
	if err != nil {
		return ""
	}
	return string(plain)
}

// SetSecret stores the base32-encoded TOTP secret, encrypted at rest.
func (st *totpStore) SetSecret(secret string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.vault == nil {
		st.state.SecretCipher = secret
		return nil
	}
	sealed, err := st.vault.Seal([]byte(secret))
	if err != nil {
		return err
	}
	st.state.SecretCipher = sealed
	return st.save()
}

// Disable clears the TOTP state (disables TOTP, drops backup codes).
func (st *totpStore) Disable() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.state.Enabled = false
	st.state.SecretCipher = ""
	st.state.BackupCodeHashes = nil
	st.pending = nil
	return st.save()
}

// SetEnabled toggles TOTP enforcement. Requires a secret to be set.
func (st *totpStore) SetEnabled(enabled bool) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if enabled && st.state.SecretCipher == "" {
		return errors.New("totp: cannot enable without a secret")
	}
	st.state.Enabled = enabled
	return st.save()
}

// StartEnrollment generates a new TOTP secret and returns the otpauth://
// provisioning URI. The secret is held pending until confirmed.
func (st *totpStore) StartEnrollment(accountName string) (string, string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	secret := randomBase32Secret()
	uri := totpProvisioningURI(totpIssuer, accountName, secret)
	st.pending = &totpPending{
		secret:   secret,
		issuedAt: time.Now(),
	}
	return secret, uri, nil
}

// ConfirmEnrollment verifies a TOTP code against the pending secret and,
// on success, activates TOTP and mints backup codes. The pending secret
// is cleared whether or not confirmation succeeds.
func (st *totpStore) ConfirmEnrollment(code string) ([]string, error) {
	st.mu.Lock()
	pending := st.pending
	st.pending = nil // always clear pending on confirm attempt
	if pending == nil {
		st.mu.Unlock()
		return nil, errors.New("totp: no pending enrollment")
	}
	if time.Since(pending.issuedAt) > totpIssueWindow {
		st.mu.Unlock()
		return nil, errors.New("totp: enrollment expired — start again")
	}
	valid := totpValidateCode(pending.secret, code, time.Now(), st.usedCodes)
	st.mu.Unlock()

	if !valid {
		return nil, errors.New("totp: invalid confirmation code")
	}

	backupCodes := randomBackupCodes(totpBackupCodeCount, totpBackupCodeLen)
	hashes := make([]string, len(backupCodes))
	for i, c := range backupCodes {
		h, err := hashBackupCode(c)
		if err != nil {
			return nil, fmt.Errorf("totp: hash backup code: %w", err)
		}
		hashes[i] = h
	}

	st.mu.Lock()
	st.state.BackupCodeHashes = hashes
	st.state.Enabled = true
	st.mu.Unlock()
	// SetSecret acquires its own lock; call it outside the outer lock to
	// avoid self-deadlock.
	if err := st.SetSecret(pending.secret); err != nil {
		return nil, fmt.Errorf("totp: store secret: %w", err)
	}
	st.mu.Lock()
	if err := st.save(); err != nil {
		st.mu.Unlock()
		return nil, fmt.Errorf("totp: save state: %w", err)
	}
	st.mu.Unlock()
	return backupCodes, nil
}

// VerifyLoginCode checks a TOTP code or backup code for login. Returns
// true when the code is valid and (for TOTP codes) not a replay.
func (st *totpStore) VerifyLoginCode(code string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	// Try backup code first (single-use, no time step).
	for i, hash := range st.state.BackupCodeHashes {
		if verifyBackupCode(code, hash) {
			// Consume the backup code: remove its hash.
			st.state.BackupCodeHashes = append(
				st.state.BackupCodeHashes[:i],
				st.state.BackupCodeHashes[i+1:]...,
			)
			st.save() // ignore error — best effort to persist consumption
			return true
		}
	}

	// Try TOTP code (time-based, replay-protected).
	if st.state.SecretCipher != "" {
		secret := st.state.SecretCipher
		if st.vault != nil {
			plain, err := st.vault.Unseal(secret)
			if err != nil {
				return false
			}
			secret = string(plain)
		}
		result := totpValidateCode(secret, code, time.Now(), st.usedCodes)
		if result {
			st.pruneUsedCodes()
			return true
		}
	}
	return false
}

// ChangeSecret replaces the TOTP secret (used when rotating after a
// suspected compromise). Requires TOTP to be currently enabled.
func (st *totpStore) ChangeSecret(newSecret string) error {
	st.mu.Lock()
	enabled := st.state.Enabled
	st.mu.Unlock()
	if !enabled {
		return errors.New("totp: TOTP not enabled")
	}
	return st.SetSecret(newSecret)
}

// pruneUsedCodes removes replay-tracking entries older than the window.
func (st *totpStore) pruneUsedCodes() {
	now := time.Now()
	for k, t := range st.usedCodes {
		if now.After(t.Add(totpReplayWindow)) {
			delete(st.usedCodes, k)
		}
	}
}

// ---------------------------------------------------------------------------
// TOTP helpers
// ---------------------------------------------------------------------------

// randomBase32Secret generates a 160-bit (20-byte) random secret and
// returns it base32-encoded (standard RFC 4648 base32, no padding).
func randomBase32Secret() string {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		panic("totp: crypto/rand failed: " + err.Error())
	}
	return base32.StdEncoding.EncodeToString(buf)
}

// totpProvisioningURI builds an otpauth://totp/ URI for QR code provisioning.
func totpProvisioningURI(issuer, account, secret string) string {
	params := url.Values{}
	params.Set("secret", secret)
	params.Set("issuer", issuer)
	return "otpauth://totp/" + url.PathEscape(account) + "?" + params.Encode()
}

// hotpCompute computes the HMAC-SHA1 hash of the counter using the secret,
// returning the raw hash bytes (RFC 4226).
func hotpCompute(secret []byte, counter uint64) []byte {
	// Counter as 8-byte big-endian.
	var cb [8]byte
	binary.BigEndian.PutUint64(cb[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(cb[:])
	return mac.Sum(nil)
}

// totpValidateCode checks a 6-digit TOTP code against the secret for the
// current time step. usedCodes prevents replay within a step.
func totpValidateCode(secret, code string, now time.Time, usedCodes map[string]time.Time) bool {
	if len(code) != totpCodeDigits {
		return false
	}
	for _, ch := range code {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	secretBytes, err := base32.StdEncoding.DecodeString(secret)
	if err != nil {
		return false
	}
	step := uint64(now.Unix() / totpInterval)
	hash := hotpCompute(secretBytes, step)

	// Dynamic truncation (RFC 4226).
	offset := hash[len(hash)-1] & 0x0F
	bin := (uint32(hash[offset])&0x7F)<<24 |
		(uint32(hash[offset+1])&0xFF)<<16 |
		(uint32(hash[offset+2])&0xFF)<<8 |
		(uint32(hash[offset+3]) & 0xFF)
	otp := bin % 1000000

	expected := fmt.Sprintf("%06d", otp)

	key := code + "-" + fmt.Sprintf("%d", step)
	if _, seen := usedCodes[key]; seen {
		return false // replay within the same step
	}
	if subtleCompare(code, expected) {
		usedCodes[key] = now
		return true
	}
	return false
}

// subtleCompare compares two strings in constant time.
func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// randomBackupCodes generates count random hex backup codes of length
// chars each.
func randomBackupCodes(count, chars int) []string {
	codes := make([]string, count)
	buf := make([]byte, (chars+1)/2)
	for i := range codes {
		if _, err := rand.Read(buf); err != nil {
			panic("totp: crypto/rand failed: " + err.Error())
		}
		codes[i] = hex.EncodeToString(buf)[:chars]
	}
	return codes
}

// hashBackupCode hashes a backup code with Argon2id.
func hashBackupCode(code string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(code), salt, 3, 65536, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		65536, 3, 1,
		base32.StdEncoding.EncodeToString(salt),
		base32.StdEncoding.EncodeToString(hash)), nil
}

// verifyBackupCode checks a plaintext backup code against an Argon2id hash.
func verifyBackupCode(code, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	salt, err := base32.StdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base32.StdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(code), salt, 3, 65536, 1, uint32(len(want)))
	return subtleCompare(string(got), string(want))
}

// ---------------------------------------------------------------------------
// File helpers
//
// readFileLocal, writeFileLocal, renameFileLocal are defined in server.go
// (same package) — totp.go uses them directly.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Server wiring
// ---------------------------------------------------------------------------

// totpAuth and totpAuthErr are declared on the Server struct in server.go.
// totpStoreFor returns this server's lazy TOTP store.
func (s *Server) totpStoreFor() (*totpStore, error) {
	s.dashAuthMu.Lock()
	defer s.dashAuthMu.Unlock()
	if s.totpAuth != nil {
		return s.totpAuth, nil
	}
	if s.totpAuthErr != nil {
		return nil, s.totpAuthErr
	}
	dataDir := ""
	if s.cfg != nil {
		dataDir = s.cfg.DataDir
	}
	var vault *crypto.Vault
	if dataDir != "" {
		v, err := crypto.New(dataDir)
		if err != nil {
			vault = nil
		} else {
			vault = v
		}
	}
	st := newTotpStore(vault, dataDir)
	s.totpAuth = st
	return st, nil
}

// ---------------------------------------------------------------------------
// TOTP HTTP handlers
// ---------------------------------------------------------------------------

// handleTotpStatus serves GET /dashboard/auth/totp/status: reports whether
// TOTP is enrolled/enabled and whether backup codes remain.
func (s *Server) handleTotpStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.dashSessionValid(r) && !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	st, err := s.totpStoreFor()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "totp unavailable: "+err.Error())
		return
	}
	st.mu.Lock()
	enabled := st.state.Enabled
	backupCount := len(st.state.BackupCodeHashes)
	hasSecret := st.state.SecretCipher != ""
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"totp_enabled":  enabled,
		"totp_enrolled": hasSecret,
		"backup_codes":  backupCount,
	})
}

// handleTotpEnroll serves POST /dashboard/auth/totp/enroll: begins TOTP
// enrollment, returning a provisioning URI (QR code) and a pending secret.
// Requires a valid dashboard session or admin gateway key.
func (s *Server) handleTotpEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.dashSessionValid(r) && !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	st, err := s.totpStoreFor()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "totp unavailable: "+err.Error())
		return
	}
	secret, uri, err := st.StartEnrollment("codvian-router")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start TOTP enrollment")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secret":           secret,
		"provisioning_uri": uri,
		"note":             "Scan the URI with an authenticator app, then confirm with POST /dashboard/auth/totp/confirm",
	})
}

// handleTotpConfirm serves POST /dashboard/auth/totp/confirm: verifies a
// TOTP code against the pending enrollment secret and, on success, activates
// TOTP and mints backup codes. Returns the backup codes exactly once.
func (s *Server) handleTotpConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.dashSessionValid(r) && !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	st, err := s.totpStoreFor()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "totp unavailable: "+err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid confirm request: expected {\"code\": string}")
		return
	}
	codes, err := st.ConfirmEnrollment(req.Code)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"totp_enabled": true,
		"backup_codes": codes,
		"note":         "Store these codes securely — each can be used once in place of a TOTP code",
	})
}

// handleTotpVerify serves POST /dashboard/auth/totp/verify: verifies a TOTP
// or backup code for login step 2. On success, sets the session cookie.
// Used when TOTP is enabled and login step 1 returned totp_required.
func (s *Server) handleTotpVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.dashSessionValid(r) && !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	st, err := s.totpStoreFor()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "totp unavailable: "+err.Error())
		return
	}
	if !st.Enabled() {
		writeError(w, http.StatusBadRequest, "TOTP is not enabled")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid verify request: expected {\"code\": string}")
		return
	}
	if !st.VerifyLoginCode(req.Code) {
		writeError(w, http.StatusUnauthorized, "invalid TOTP or backup code")
		return
	}
	// Create a dashboard session (same store as password login).
	dst := s.dashAuthStore()
	if dst == nil {
		writeError(w, http.StatusServiceUnavailable, "dashboard auth unavailable")
		return
	}
	tok, exp, err := dst.NewSession()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start dashboard session")
		return
	}
	setDashSessionCookie(w, tok, exp)
	writeJSON(w, http.StatusOK, map[string]any{
		"verified":   true,
		"expires_at": exp.UTC().Format(time.RFC3339),
	})
}

// handleTotpDisable serves POST /dashboard/auth/totp/disable: disables TOTP.
// Requires password + current TOTP code, or an admin gateway key.
func (s *Server) handleTotpDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.totpStoreFor()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "totp unavailable: "+err.Error())
		return
	}
	hasSession := s.dashSessionValid(r)
	hasAdmin := s.authorized(r) && s.adminScopeOK(r)
	if !hasSession && !hasAdmin {
		s.unauthorized(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Password   string `json:"password,omitempty"`
		TotpCode   string `json:"totp_code,omitempty"`
		BackupCode string `json:"backup_code,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid disable request")
		return
	}
	// Verify identity: password + TOTP/backup code, or admin key alone.
	if hasAdmin {
		// Admin key can disable without password/TOTP.
	} else if hasSession {
		if req.Password == "" || !s.dashAuthStore().VerifyPassword(req.Password) {
			writeError(w, http.StatusForbidden, "current password is required")
			return
		}
		if req.TotpCode == "" && req.BackupCode == "" {
			writeError(w, http.StatusForbidden, "TOTP code or backup code is required")
			return
		}
		if !st.VerifyLoginCode(req.TotpCode) && !st.VerifyLoginCode(req.BackupCode) {
			writeError(w, http.StatusForbidden, "invalid TOTP or backup code")
			return
		}
	} else {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := st.Disable(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not disable TOTP")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"totp_enabled": false,
		"note":         "TOTP disabled — dashboard login no longer requires a second factor",
	})
}

// handleTotpBackupCodes serves POST /dashboard/auth/totp/backup-codes:
// regenerates backup codes. Requires a valid session and current password.
func (s *Server) handleTotpBackupCodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.dashSessionValid(r) && !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	st, err := s.totpStoreFor()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "totp unavailable: "+err.Error())
		return
	}
	if !st.Enabled() {
		writeError(w, http.StatusBadRequest, "TOTP is not enabled")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: expected {\"password\": string}")
		return
	}
	if !s.dashAuthStore().VerifyPassword(req.Password) {
		writeError(w, http.StatusForbidden, "current password is required")
		return
	}
	codes := randomBackupCodes(totpBackupCodeCount, totpBackupCodeLen)
	hashes := make([]string, len(codes))
	for i, c := range codes {
		h, err := hashBackupCode(c)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not hash backup codes")
			return
		}
		hashes[i] = h
	}
	st.mu.Lock()
	st.state.BackupCodeHashes = hashes
	if err := st.save(); err != nil {
		st.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "could not save backup codes")
		return
	}
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"backup_codes": codes,
		"note":         "Store these codes securely — each can be used once",
	})
}

// dashAuthStore returns this server's dashboard auth store (from dashauth.go).
func (s *Server) dashAuthStore() *dashAuthStore {
	st, err := s.dashboardAuth()
	if err != nil {
		return nil
	}
	return st
}
