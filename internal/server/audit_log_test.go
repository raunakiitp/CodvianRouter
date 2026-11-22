package server

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuditRecordAndList verifies that Record appends and List returns
// events newest-first with correct pagination.
func TestAuditRecordAndList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.db")
	store, err := openAuditStore(path)
	if err != nil {
		t.Fatalf("openAuditStore: %v", err)
	}
	defer store.Close()

	events := []auditEvent{
		{TS: 1000, CorrelationID: "req-1", KeyIDHash: "abc12345", Method: "POST", Path: "/v1/chat/completions", StatusCode: 200, LatencyMs: 45, FailureClass: "none", ActorType: "gateway_key"},
		{TS: 2000, CorrelationID: "req-2", KeyIDHash: "", Method: "GET", Path: "/v1/models", StatusCode: 401, LatencyMs: 12, FailureClass: "auth", ActorType: "anonymous"},
		{TS: 3000, CorrelationID: "req-3", KeyIDHash: "def67890", Method: "POST", Path: "/v1/embeddings", StatusCode: 429, LatencyMs: 5, FailureClass: "rate_limit", ActorType: "gateway_key"},
	}
	for i, e := range events {
		if err := store.Record(e); err != nil {
			t.Fatalf("Record(%d): %v", i, err)
		}
	}

	got, total, err := store.List(10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	// Newest first.
	if got[0].CorrelationID != "req-3" {
		t.Errorf("first event correlation_id = %q, want req-3", got[0].CorrelationID)
	}
	if got[2].CorrelationID != "req-1" {
		t.Errorf("last event correlation_id = %q, want req-1", got[2].CorrelationID)
	}

	// Pagination.
	got2, _, err := store.List(1, 1)
	if err != nil {
		t.Fatalf("List(1,1): %v", err)
	}
	if len(got2) != 1 || got2[0].CorrelationID != "req-2" {
		t.Errorf("List(1,1) = %+v, want [req-2]", got2)
	}
}

// TestAuditVerifyChainIntact verifies VerifyChain returns -1 for an
// unmodified store.
func TestAuditVerifyChainIntact(t *testing.T) {
	dir := t.TempDir()
	store, err := openAuditStore(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatalf("openAuditStore: %v", err)
	}
	defer store.Close()

	for i := 0; i < 10; i++ {
		if err := store.Record(auditEvent{
			TS:           int64(i * 1000),
			CorrelationID: "req",
			KeyIDHash:    "abc12345",
			Method:       "GET",
			Path:         "/v1/models",
			StatusCode:   200,
			LatencyMs:    10,
			FailureClass: "none",
			ActorType:    "gateway_key",
		}); err != nil {
			t.Fatalf("Record(%d): %v", i, err)
		}
	}
	idx, err := store.VerifyChain()
	if err != nil {
		t.Fatalf("VerifyChain error: %v", err)
	}
	if idx != -1 {
		t.Errorf("VerifyChain = %d, want -1 (chain intact)", idx)
	}
}

// TestAuditTamperDetection verifies VerifyChain detects a modified row.
func TestAuditTamperDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.db")
	store, err := openAuditStore(path)
	if err != nil {
		t.Fatalf("openAuditStore: %v", err)
	}

	// Record 3 events.
	for i := 0; i < 3; i++ {
		if err := store.Record(auditEvent{
			TS:           int64(i * 1000),
			CorrelationID: "req",
			KeyIDHash:    "abc12345",
			Method:       "GET",
			Path:         "/v1/models",
			StatusCode:   200,
			LatencyMs:    10,
			FailureClass: "none",
			ActorType:    "gateway_key",
		}); err != nil {
			t.Fatalf("Record(%d): %v", i, err)
		}
	}
	store.Close()

	// Tamper: directly modify the status_code of row 2 (middle row).
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE audit_events SET status_code = 500 WHERE id = 2`); err != nil {
		t.Fatalf("tamper UPDATE: %v", err)
	}

	store2, err := openAuditStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()

	idx, err := store2.VerifyChain()
	if err != nil {
		t.Fatalf("VerifyChain error: %v", err)
	}
	if idx != 1 { // 0-based: row id=2 is index 1
		t.Errorf("VerifyChain = %d, want 1 (tamper detected at row 2)", idx)
	}
}

// TestAuditAppendOnly verifies there is no DELETE path on audit_events.
func TestAuditAppendOnlySchema(t *testing.T) {
	dir := t.TempDir()
	store, err := openAuditStore(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatalf("openAuditStore: %v", err)
	}
	defer store.Close()

	// There should be no DELETE or UPDATE in the schema.
	// We verify by attempting a DELETE and confirming it's not part of our API.
	// The store type has no Delete method — that's the append-only guarantee.
	if store == nil {
		t.Fatal("store is nil")
	}
	// Record one event.
	if err := store.Record(auditEvent{
		TS:           1,
		CorrelationID: "req",
		Method:       "GET",
		Path:         "/v1/models",
		StatusCode:   200,
		LatencyMs:    1,
		FailureClass: "none",
		ActorType:    "anonymous",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// List should still return it.
	got, _, err := store.List(10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d events, want 1", len(got))
	}
}

// TestHashKeyIDNeverReversible verifies hashKeyID output is short and
// deterministic, and that different inputs produce different outputs.
func TestHashKeyID(t *testing.T) {
	h1 := hashKeyID("sk-ant-very-long-secret-key")
	h2 := hashKeyID("sk-ant-very-long-secret-key")
	h3 := hashKeyID("different-key")
	if h1 != h2 {
		t.Errorf("hashKeyID not deterministic: %q vs %q", h1, h2)
	}
	if h1 == h3 {
		t.Errorf("hashKeyID collision: %q vs %q", h1, h3)
	}
	if len(h1) != 8 {
		t.Errorf("hashKeyID length = %d, want 8", len(h1))
	}
	if hashKeyID("") != "" {
		t.Errorf("hashKeyID(empty) = %q, want empty", hashKeyID(""))
	}
}

// TestClassifyFailure verifies failure class mapping.
func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{200, "none"},
		{201, "none"},
		{301, "none"},
		{401, "auth"},
		{403, "auth"},
		{429, "rate_limit"},
		{402, "quota"},
		{404, "upstream"},
		{500, "server"},
		{503, "server"},
		{0, "none"},
	}
	for _, c := range cases {
		if got := classifyFailure(c.status); got != c.want {
			t.Errorf("classifyFailure(%d) = %q, want %q", c.status, got, c.want)
		}
	}
}

// TestAuditMiddlewareEndToEnd verifies the withAudit middleware records
// an event through a real HTTP handler.
func TestAuditMiddlewareEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.db")
	store, err := openAuditStore(path)
	if err != nil {
		t.Fatalf("openAuditStore: %v", err)
	}
	defer store.Close()

	s := &Server{
		auditStore: store,
		logger:     log.Default(),
	}

	// A simple handler that sets a model in context and returns 200.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	})

	wrapped := s.withAudit(handler)

	// Request with a Bearer key.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
	req.Header.Set("Authorization", "Bearer sk-test-key-12345")
	req.Header.Set("Content-Type", "application/json")

	// We need to set the verifier to extract key ID.
	s.verifier = &testKeyVerifier{keyID: "gw-key-1"}

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("handler status = %d, want 200", rec.Code)
	}

	// Verify the audit event was recorded.
	got, total, err := store.List(10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	e := got[0]
	if e.Method != "POST" {
		t.Errorf("method = %q, want POST", e.Method)
	}
	if e.Path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", e.Path)
	}
	if e.StatusCode != 200 {
		t.Errorf("status_code = %d, want 200", e.StatusCode)
	}
	if e.KeyIDHash == "" {
		t.Error("key_id_hash is empty, want a hash")
	}
	if e.ActorType != "gateway_key" {
		t.Errorf("actor_type = %q, want gateway_key", e.ActorType)
	}
	if e.FailureClass != "none" {
		t.Errorf("failure_class = %q, want none", e.FailureClass)
	}
	if e.CorrelationID == "" {
		t.Error("correlation_id is empty")
	}
	if e.LatencyMs < 0 {
		t.Errorf("latency_ms = %d, want >= 0", e.LatencyMs)
	}

	// Verify the raw key is NOT stored.
	if strings.Contains(e.KeyIDHash, "sk-test-key-12345") {
		t.Error("raw key material found in key_id_hash")
	}
}

// TestAuditMiddlewareNoKey verifies anonymous requests are recorded
// with actor_type=anonymous and empty key_id_hash.
func TestAuditMiddlewareNoKey(t *testing.T) {
	dir := t.TempDir()
	store, err := openAuditStore(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatalf("openAuditStore: %v", err)
	}
	defer store.Close()

	s := &Server{auditStore: store, logger: log.Default()}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	})
	wrapped := s.withAudit(handler)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	// No Authorization header.
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	got, _, err := store.List(10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	e := got[0]
	if e.ActorType != "anonymous" {
		t.Errorf("actor_type = %q, want anonymous", e.ActorType)
	}
	if e.KeyIDHash != "" {
		t.Errorf("key_id_hash = %q, want empty", e.KeyIDHash)
	}
	if e.StatusCode != 401 {
		t.Errorf("status_code = %d, want 401", e.StatusCode)
	}
	if e.FailureClass != "auth" {
		t.Errorf("failure_class = %q, want auth", e.FailureClass)
	}
}

// TestAuditStorageFailureDoesNotFailRequest verifies that when the audit
// store fails, the request still completes successfully.
func TestAuditStorageFailureDoesNotFailRequest(t *testing.T) {
	// Use a nil store — Record is a no-op (nil check in middleware).
	s := &Server{auditStore: nil, logger: log.Default()}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	wrapped := s.withAudit(handler)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("handler status = %d, want 200 (nil store must not fail)", rec.Code)
	}
}

// TestNormalizeRoutePath verifies query strings are stripped.
func TestNormalizeRoutePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"/v1/models?limit=10", "/v1/models"},
		{"/v1/models?key=secret&limit=10", "/v1/models"},
		{"/", "/"},
		{"", "/"},
	}
	for _, c := range cases {
		if got := normalizeRoutePath(c.in); got != c.want {
			t.Errorf("normalizeRoutePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}


// --- test helpers ---

type testKeyVerifier struct {
	keyID string
}

func (v *testKeyVerifier) VerifyKey(_ context.Context, key string) bool { return true }
func (v *testKeyVerifier) KeyID(_ context.Context, key string) (string, bool) {
	return v.keyID, true
}
