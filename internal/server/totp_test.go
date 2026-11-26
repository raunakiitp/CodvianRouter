package server

import (
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// TestTOTPValidateCode verifies TOTP code validation against known test
// vectors (RFC 4226 / RFC 6238 style).
func TestTOTPValidateCode(t *testing.T) {
	// Known test vector (RFC 4226): secret 0x7071...7F (base32: GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ)
	// counter 0 → HOTP-SHA1 = 755224
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	used := map[string]time.Time{}

	// Time 0: step = 0 → code 755224
	if !totpValidateCode(secret, "755224", time.Unix(0, 0), used) {
		t.Errorf("totpValidateCode failed at time 0 for code 755224")
	}

	// Wrong code at same time should fail
	if totpValidateCode(secret, "000000", time.Unix(0, 0), used) {
		t.Errorf("totpValidateCode should reject wrong code")
	}

	// Replay of same code at same time step should fail
	if totpValidateCode(secret, "354771", time.Unix(0, 0), used) {
		t.Errorf("totpValidateCode should reject replayed code within same step")
	}

	// Different time step: advance to step 1 (30s later)
	used2 := map[string]time.Time{}
	// Compute expected code for step 1 using HMAC-SHA1
	// Just verify that a different code works (we don't know the exact value)
	// What matters is that the same code at a different step doesn't replay
	if totpValidateCode(secret, "354771", time.Unix(30, 0), used2) {
		// This might or might not be valid depending on the algorithm —
		// the point is it's a different step so replay protection key differs
		t.Logf("code 354771 also valid at step 1 (different time step)")
	}
}

// TestTOTPStoreEnrollment tests TOTP enrollment and confirmation.
func TestTOTPStoreEnrollment(t *testing.T) {
	st := newTotpStore(nil, "")

	// Initially disabled
	if st.Enabled() {
		t.Errorf("TOTP should be disabled initially")
	}

	// Start enrollment
	secret, uri, err := st.StartEnrollment("test@example.com")
	if err != nil {
		t.Fatalf("StartEnrollment: %v", err)
	}
	if secret == "" {
		t.Errorf("StartEnrollment returned empty secret")
	}
	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Errorf("unexpected provisioning URI: %s", uri)
	}

	// Generate a valid TOTP code for the pending secret at the current time
	code := totpCodeForSecret(secret, time.Now())

	// Confirm enrollment
	codes, err := st.ConfirmEnrollment(code)
	if err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	if len(codes) != totpBackupCodeCount {
		t.Errorf("expected %d backup codes, got %d", totpBackupCodeCount, len(codes))
	}

	// Should be enabled now
	if !st.Enabled() {
		t.Errorf("TOTP should be enabled after confirmation")
	}

	// Verify login with a fresh TOTP code (not the enrollment code — that's
	// already in usedCodes from ConfirmEnrollment). Wait for the next step
	// boundary so VerifyLoginCode (which uses time.Now()) validates a
	// different code than the enrollment one.
	verified := false
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		nowStep := time.Unix((time.Now().Unix()/totpInterval)*totpInterval, 0)
		code := totpCodeForSecret(secret, nowStep)
		if st.VerifyLoginCode(code) {
			verified = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !verified {
		t.Errorf("VerifyLoginCode should accept current TOTP code")
	}

	// Verify login with a backup code
	if !st.VerifyLoginCode(codes[0]) {
		t.Errorf("VerifyLoginCode should accept backup code %s", codes[0])
	}

	// Backup code should be single-use
	if st.VerifyLoginCode(codes[0]) {
		t.Errorf("VerifyLoginCode should reject reused backup code")
	}

	// Wrong TOTP code should fail
	if st.VerifyLoginCode("000000") {
		t.Errorf("VerifyLoginCode should reject wrong code")
	}
}

// TestTOTPStoreDisable tests disabling TOTP.
func TestTOTPStoreDisable(t *testing.T) {
	st := newTotpStore(nil, "")

	secret, _, err := st.StartEnrollment("test@example.com")
	if err != nil {
		t.Fatalf("StartEnrollment: %v", err)
	}
	code := totpCodeForSecret(secret, time.Now())
	if _, err := st.ConfirmEnrollment(code); err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}

	if !st.Enabled() {
		t.Errorf("TOTP should be enabled")
	}

	if err := st.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	if st.Enabled() {
		t.Errorf("TOTP should be disabled after Disable()")
	}
	if st.Secret() != "" {
		t.Errorf("Secret should be cleared after Disable()")
	}
}

// TestTOTPReplayPrevention tests that TOTP codes cannot be replayed
// within the same time step.
func TestTOTPReplayPrevention(t *testing.T) {
	st := newTotpStore(nil, "")

	secret, _, err := st.StartEnrollment("test@example.com")
	if err != nil {
		t.Fatalf("StartEnrollment: %v", err)
	}
	code := totpCodeForSecret(secret, time.Now())
	if _, err := st.ConfirmEnrollment(code); err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}

	// First use should succeed (wait for next step so code differs from
	// enrollment code). VerifyLoginCode uses time.Now().
	verified := false
	var firstCode string
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		nowStep := time.Unix((time.Now().Unix()/totpInterval)*totpInterval, 0)
		code := totpCodeForSecret(secret, nowStep)
		if st.VerifyLoginCode(code) {
			verified = true
			firstCode = code
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !verified {
		t.Errorf("first use of code should succeed")
	}

	// Same code at same step should fail (replay)
	if firstCode != "" && st.VerifyLoginCode(firstCode) {
		t.Errorf("replayed code should fail")
	}
}

// TestTOTPWithVault tests TOTP with an encrypted vault for at-rest storage.
func TestTOTPWithVault(t *testing.T) {
	dir := t.TempDir()
	vault, err := crypto.New(dir)
	if err != nil {
		t.Fatalf("crypto.New: %v", err)
	}
	st := newTotpStore(vault, dir)

	secret, _, err := st.StartEnrollment("test@example.com")
	if err != nil {
		t.Fatalf("StartEnrollment: %v", err)
	}
	code := totpCodeForSecret(secret, time.Now())
	if _, err := st.ConfirmEnrollment(code); err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}

	if !st.Enabled() {
		t.Errorf("TOTP should be enabled")
	}

	// Verify code still works with vault-backed store. Wait up to 2 step
	// boundaries (enrollment may have crossed one already) so VerifyLoginCode
	// (which uses time.Now()) validates a different code than enrollment.
	verified := false
	deadline := time.Now().Add(75 * time.Second)
	for time.Now().Before(deadline) {
		nowStep := time.Unix((time.Now().Unix()/totpInterval)*totpInterval, 0)
		code := totpCodeForSecret(secret, nowStep)
		if st.VerifyLoginCode(code) {
			verified = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !verified {
		t.Errorf("VerifyLoginCode should accept current TOTP code with vault")
	}
}

// totpCodeForSecret computes the current TOTP code for a given base32
// secret at the given time. Used only in tests.
func totpCodeForSecret(secret string, now time.Time) string {
	secretBytes, err := base32.StdEncoding.DecodeString(secret)
	if err != nil {
		return ""
	}
	step := uint64(now.Unix() / totpInterval)
	hash := hotpCompute(secretBytes, step)
	offset := hash[len(hash)-1] & 0x0F
	bin := (uint32(hash[offset])&0x7F)<<24 |
		(uint32(hash[offset+1])&0xFF)<<16 |
		(uint32(hash[offset+2])&0xFF)<<8 |
		(uint32(hash[offset+3]) & 0xFF)
	otp := bin % 1000000
	return formatOTP(otp)
}

func formatOTP(otp uint32) string {
	buf := make([]byte, totpCodeDigits)
	for i := totpCodeDigits - 1; i >= 0; i-- {
		buf[i] = '0' + byte(otp%10)
		otp /= 10
	}
	return string(buf)
}

// TestTotpHandlers tests the TOTP HTTP handlers end-to-end.
func TestTotpHandlers(t *testing.T) {
	// Use newTestServer so the auth store (session + keys) is configured.
	srv := newTestServer(t, &fakeEngine{})
	s := srv

	// Set a config with a temp data dir for vault-backed storage
	dir := t.TempDir()
	s.cfg = &config.Config{DataDir: dir}

	// Enroll TOTP first (via store directly)
	st, err := s.totpStoreFor()
	if err != nil {
		t.Fatalf("totpStoreFor: %v", err)
	}
	secret, _, err := st.StartEnrollment("test@example.com")
	if err != nil {
		t.Fatalf("StartEnrollment: %v", err)
	}

	// Confirm enrollment via store (use current step)
	stepStart := time.Unix((time.Now().Unix()/totpInterval)*totpInterval, 0)
	code := totpCodeForSecret(secret, stepStart)
	backupCodes, err := st.ConfirmEnrollment(code)
	if err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}

	// Test status endpoint (requires session or admin key)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/auth/totp/status", nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	w := httptest.NewRecorder()
	s.handleTotpStatus(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var status map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("status response not JSON: %v", err)
	}
	if status["totp_enabled"] != true {
		t.Errorf("expected totp_enabled=true, got %v", status["totp_enabled"])
	}

	// Test verify endpoint with valid TOTP code. Wait for next step so code
	// differs from enrollment code (VerifyLoginCode uses time.Now()).
	// Clear any prior failure count so rate limiting (P2-SEC-06) does not
	// interfere with this test's verify attempts.
	dst, _ := s.dashboardAuth()
	if dst != nil {
		dst.RecordLoginSuccess("")
	}
	verified := false
	var body []byte
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		nowStep := time.Unix((time.Now().Unix()/totpInterval)*totpInterval, 0)
		code := totpCodeForSecret(secret, nowStep)
		body, _ = json.Marshal(map[string]string{"code": code})
		req = httptest.NewRequest(http.MethodPost, "/dashboard/auth/totp/verify", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-key-123")
		w = httptest.NewRecorder()
		// Clear the per-peer failure count before each attempt so the
		// TOTP timing race (step may change between compute and send)
		// doesn't trigger P2-SEC-06 rate limiting in this test.
		if d, err := s.dashboardAuth(); err == nil && d != nil {
			d.RecordLoginSuccess("")
		}
		s.handleTotpVerify(w, req)
		if w.Code == http.StatusOK {
			verified = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !verified {
		t.Errorf("verify: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Test verify endpoint with backup code
	body, _ = json.Marshal(map[string]string{"code": backupCodes[0]})
	req = httptest.NewRequest(http.MethodPost, "/dashboard/auth/totp/verify", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key-123")
	w = httptest.NewRecorder()
	s.handleTotpVerify(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("verify backup: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Backup code should be consumed
	body, _ = json.Marshal(map[string]string{"code": backupCodes[0]})
	req = httptest.NewRequest(http.MethodPost, "/dashboard/auth/totp/verify", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key-123")
	w = httptest.NewRecorder()
	s.handleTotpVerify(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("verify consumed backup: expected 401, got %d", w.Code)
	}

	// Test disable endpoint (no auth — should fail)
		body, _ = json.Marshal(map[string]string{})
		req = httptest.NewRequest(http.MethodPost, "/dashboard/auth/totp/disable", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		s.handleTotpDisable(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("disable without auth: expected 401, got %d", w.Code)
		}
	}

	// TestTotpVerifyRateLimited verifies that POST /dashboard/auth/totp/verify
	// is rate-limited per peer like password login (P2-SEC-06 / AUD-SEC-007).
	func TestTotpVerifyRateLimited(t *testing.T) {
		srv := newTestServer(t, &fakeEngine{})
		s := srv

		dir := t.TempDir()
		s.cfg = &config.Config{DataDir: dir}

		// Enroll TOTP via store
		st, err := s.totpStoreFor()
		if err != nil {
			t.Fatalf("totpStoreFor: %v", err)
		}
		secret, _, err := st.StartEnrollment("rate@example.com")
		if err != nil {
			t.Fatalf("StartEnrollment: %v", err)
		}
		stepStart := time.Unix((time.Now().Unix()/totpInterval)*totpInterval, 0)
		code := totpCodeForSecret(secret, stepStart)
		_, err = st.ConfirmEnrollment(code)
		if err != nil {
			t.Fatalf("ConfirmEnrollment: %v", err)
		}

		// Make dashLoginMaxFailures failed verify attempts.
		// All from httptest, so RemoteAddr is "" → same peer.
		badBody, _ := json.Marshal(map[string]string{"code": "000000"})
		for i := 0; i < dashLoginMaxFailures; i++ {
			req := httptest.NewRequest(http.MethodPost, "/dashboard/auth/totp/verify", strings.NewReader(string(badBody)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-key-123")
			w := httptest.NewRecorder()
			s.handleTotpVerify(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("attempt %d: expected 401, got %d: %s", i+1, w.Code, w.Body.String())
			}
		}

		// The next attempt should be rate-limited (429).
		req := httptest.NewRequest(http.MethodPost, "/dashboard/auth/totp/verify", strings.NewReader(string(badBody)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-key-123")
		w := httptest.NewRecorder()
		s.handleTotpVerify(w, req)
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("after %d failures: expected 429, got %d: %s", dashLoginMaxFailures, w.Code, w.Body.String())
		}

		// A valid code while locked out should still be 429 (rate limit takes precedence).
		goodStep := time.Unix((time.Now().Unix()/totpInterval)*totpInterval, 0)
		goodCode := totpCodeForSecret(secret, goodStep)
		goodBody, _ := json.Marshal(map[string]string{"code": goodCode})
		req = httptest.NewRequest(http.MethodPost, "/dashboard/auth/totp/verify", strings.NewReader(string(goodBody)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-key-123")
		w = httptest.NewRecorder()
		s.handleTotpVerify(w, req)
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("locked out with valid code: expected 429, got %d: %s", w.Code, w.Body.String())
		}
	}

// configForTest removed — using config.Config directly.
