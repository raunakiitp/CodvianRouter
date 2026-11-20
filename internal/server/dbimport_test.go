package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// dbImportResponse mirrors the import response for assertions.
type dbImportResponseTest struct {
	Imported          int     `json:"imported"`
	Duplicates        int     `json:"duplicates_skipped"`
	EventsInFile      int     `json:"events_in_file"`
	SourceEventsTotal int     `json:"source_events_total"`
	SourceTruncated   bool    `json:"source_truncated"`
	TotalRequests     int     `json:"total_requests"`
	TotalCostUSD      float64 `json:"total_cost_usd"`
	Note              string  `json:"note"`
}

// doDBImport builds a multipart upload (fields: file + confirm)
// and POSTs it to /admin/db/import, like the dashboard file picker
// does. fileContent "" omits the file field; confirm "" omits the
// confirm field.
func doDBImport(t *testing.T, srv *Server, fileContent, fileName, confirm, key string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if fileContent != "" || fileName != "" {
		fw, err := mw.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(fileContent)); err != nil {
			t.Fatal(err)
		}
	}
	if confirm != "" {
		if err := mw.WriteField("confirm", confirm); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/admin/db/import", &buf)
	req.RemoteAddr = loopbackPeer
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// newDBImportFixture builds a config (one gateway key), the raw key
// for auth, and a ledger holding two recorded usage events — the
// same fixture newDBExportServer builds, but with the pieces
// exposed so tests can rewire the server (relaxed auth, no ledger).
func newDBImportFixture(t *testing.T) (*config.Config, string, *ledger.Ledger) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	key, raw, err := config.IssueKey("test", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	ctx := context.Background()
	for _, u := range []ledger.Usage{
		{GatewayKeyID: key.ID, Provider: "openai", Model: "gpt-4o-mini", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, SplitKnown: true, OK: true, LatencyMS: 120},
		{GatewayKeyID: key.ID, Provider: "openai", Model: "gpt-4o-mini", PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28, SplitKnown: true, OK: false, LatencyMS: 90},
	} {
		if _, err := led.RecordUsage(ctx, u, ledger.Prices{}); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, raw, led
}

// export route's own output), so the import tests exercise the
// exact format the export route writes — not a hand-built copy.
func exportFromServer(t *testing.T, srv *Server, key string) string {
	t.Helper()
	rec := doReqPeer(t, srv, "POST", "/admin/db/export", `{"confirm": "EXPORT"}`, key, loopbackPeer)
	if rec.Code != 200 {
		t.Fatalf("export for import test: status %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func decodeImport(t *testing.T, rec *httptest.ResponseRecorder) dbImportResponseTest {
	t.Helper()
	var resp dbImportResponseTest
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("import response is not JSON: %v: %s", err, rec.Body.String())
	}
	return resp
}

// TestDBImportMergesExport: a real export from router A imports
// into router B — totals match, the pre-existing history in B is
// preserved, and re-importing the same file inserts zero rows.
func TestDBImportMergesExport(t *testing.T) {
	ctx := context.Background()
	srvA, rawA, _ := newDBExportServer(t)
	file := exportFromServer(t, srvA, rawA)

	srvB, rawB, ledB := newDBExportServer(t)
	// B already holds its own 2 events; the file from A holds 2
	// more (same provider/model, different timestamps/tokens).
	rec := doDBImport(t, srvB, file, "export.json", "IMPORT", rawB)
	if rec.Code != 200 {
		t.Fatalf("db import: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := decodeImport(t, rec)
	if resp.Imported != 2 || resp.Duplicates != 0 || resp.EventsInFile != 2 {
		t.Errorf("db import = %+v, want imported 2, duplicates 0, in-file 2", resp)
	}
	if resp.TotalRequests != 4 {
		t.Errorf("db import total_requests = %d, want 4 (B's 2 preserved + A's 2 merged)", resp.TotalRequests)
	}
	if !strings.Contains(resp.Note, "preserved") {
		t.Errorf("db import note = %q, want it to say history was preserved", resp.Note)
	}
	events, err := ledB.RecentEvents(ctx, 10, ledger.LogAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("ledger B holds %d events after import, want 4", len(events))
	}

	// Re-import the same file: zero new rows, both skipped, totals
	// unchanged — spend history can never double-count.
	rec = doDBImport(t, srvB, file, "export.json", "IMPORT", rawB)
	if rec.Code != 200 {
		t.Fatalf("db re-import: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp = decodeImport(t, rec)
	if resp.Imported != 0 || resp.Duplicates != 2 || resp.TotalRequests != 4 {
		t.Errorf("db re-import = %+v, want imported 0, duplicates 2, total 4", resp)
	}
}

// TestDBImportCredentialBoundary: a key alone is not enough (no
// phrase, or the wrong phrase, is a 400), no key at all is a 401
// even when Tier 3 auth is relaxed, and a missing file is a 400.
func TestDBImportCredentialBoundary(t *testing.T) {
	srv, raw, _ := newDBExportServer(t)
	file := exportFromServer(t, srv, raw)

	// No key: 401.
	rec := doDBImport(t, srv, file, "export.json", "IMPORT", "")
	if rec.Code != 401 {
		t.Errorf("db import without key: status %d, want 401", rec.Code)
	}
	// Wrong key: 401.
	rec = doDBImport(t, srv, file, "export.json", "IMPORT", "wrong-key")
	if rec.Code != 401 {
		t.Errorf("db import wrong key: status %d, want 401", rec.Code)
	}
	// Key but no confirm field: 400.
	rec = doDBImport(t, srv, file, "export.json", "", raw)
	if rec.Code != 400 {
		t.Errorf("db import key without confirm: status %d, want 400", rec.Code)
	}
	// Key + wrong phrase: 400.
	rec = doDBImport(t, srv, file, "export.json", "EXPORT", raw)
	if rec.Code != 400 {
		t.Errorf("db import wrong phrase: status %d, want 400", rec.Code)
	}
	// Key + phrase but no file: 400.
	rec = doDBImport(t, srv, "", "", "IMPORT", raw)
	if rec.Code != 400 {
		t.Errorf("db import without file: status %d, want 400", rec.Code)
	}
	// Tier 2 is not loopback-gated, so a non-loopback peer with a
	// key + phrase still works (the loopback rule is Tier 1).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "export.json")
	_, _ = fw.Write([]byte(file))
	_ = mw.WriteField("confirm", "IMPORT")
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/admin/db/import", &buf)
	req.RemoteAddr = nonLoopbackPeer
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+raw)
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Errorf("db import non-loopback + key + phrase: status %d, want 200 (Tier 2 is auth-gated, not loopback-gated)", rec2.Code)
	}
}

// TestDBImportRelaxedAuthStillRequiresKey: relaxing Tier 3 auth
// must not open this Tier 2 route.
func TestDBImportRelaxedAuthStillRequiresKey(t *testing.T) {
	srv, raw, _ := newDBExportServer(t)
	file := exportFromServer(t, srv, raw)
	cfg, _, led := newDBImportFixture(t)
	relaxed := newTestServer(t, &fakeEngine{}, WithAuthRelaxed(true), WithConfig(cfg), WithLedger(led))
	rec := doDBImport(t, relaxed, file, "export.json", "IMPORT", "")
	if rec.Code != 401 {
		t.Errorf("db import relaxed auth without key: status %d, want 401", rec.Code)
	}
}

// TestDBImportRejectsCorruptFiles: bad JSON, a wrong format
// version, a corrupt event, and a truncated-total mismatch are all
// 400s — and the ledger holds zero imported rows afterwards, so an
// import is never partial.
func TestDBImportRejectsCorruptFiles(t *testing.T) {
	ctx := context.Background()
	srv, raw, led := newDBExportServer(t)
	cases := []struct {
		name string
		file string
	}{
		{"not json", `{not json`},
		{"wrong version", `{"format":"codvian-router-db-export/v2","usage":{"events":[],"events_total":0}}`},
		{"missing format", `{"usage":{"events":[],"events_total":0}}`},
		{"corrupt event", `{"format":"codvian-router-db-export/v1","usage":{"events":[{"ts":"2026-11-18T00:00:00Z","provider":"p","model":"m","prompt_tokens":-1,"completion_tokens":0,"total_tokens":0,"split_known":true,"cost_usd":0,"ok":true,"latency_ms":1}],"events_total":1}}`},
		{"one bad among good", `{"format":"codvian-router-db-export/v1","usage":{"events":[{"ts":"2026-11-18T00:00:00Z","provider":"p","model":"m","prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"split_known":true,"cost_usd":0,"ok":true,"latency_ms":1},{"ts":"2026-11-18T00:01:00Z","provider":"p","model":"m","prompt_tokens":-1,"completion_tokens":0,"total_tokens":0,"split_known":true,"cost_usd":0,"ok":true,"latency_ms":1}],"events_total":2}}`},
	}
	for _, c := range cases {
		rec := doDBImport(t, srv, c.file, "export.json", "IMPORT", raw)
		if rec.Code != 400 {
			t.Errorf("%s: status %d, want 400: %s", c.name, rec.Code, rec.Body.String())
		}
	}
	// B's own 2 events are still the only rows — no partial import.
	events, err := led.RecentEvents(ctx, 10, ledger.LogAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Errorf("ledger holds %d events after rejected imports, want 2 (rejections import nothing)", len(events))
	}
}

// TestDBImportWithoutLedger: with a config but no ledger wired, the
// route answers 503 instead of pretending to merge.
func TestDBImportWithoutLedger(t *testing.T) {
	cfg, raw, _ := newDBImportFixture(t)
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))
	rec := doDBImport(t, srv, `{"format":"codvian-router-db-export/v1","usage":{"events":[],"events_total":0}}`, "export.json", "IMPORT", raw)
	if rec.Code != 503 {
		t.Errorf("db import without ledger: status %d, want 503", rec.Code)
	}
}

// TestDBImportMethodNotAllowed: GET on the route is a 405.
func TestDBImportMethodNotAllowed(t *testing.T) {
	srv, raw, _ := newDBExportServer(t)
	req := httptest.NewRequest("GET", "/admin/db/import", nil)
	req.RemoteAddr = loopbackPeer
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /admin/db/import: status %d, want 405", rec.Code)
	}
}
