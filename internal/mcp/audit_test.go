package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempAuditLog(t *testing.T) (*AuditLog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp", "audit.jsonl")
	log, err := OpenAuditLog(path)
	if err != nil {
		t.Fatalf("OpenAuditLog(%s): %v", path, err)
	}
	return log, path
}

func TestAuditHashPresentRawInputAbsent(t *testing.T) {
	log, path := tempAuditLog(t)
	secret := "sk-test-secret-VALUE-12345"
	args := map[string]any{"note": "my key is " + secret, "n": float64(1)}
	if err := log.Record(AuditEntry{
		Tool: "router_status", CallerKind: KindLocalOperator, CallerID: "local-operator",
		Decision: AuditAllow, InputHash: HashArguments(args),
		OutputSummary: SummarizeOutput(map[string]any{"providers": 2}),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	// The raw secret must appear NOWHERE in the log file.
	if strings.Contains(string(raw), secret) {
		t.Fatalf("audit log contains raw input secret: %s", raw)
	}
	if strings.Contains(string(raw), "my key is") {
		t.Fatalf("audit log contains raw input text: %s", raw)
	}
	// The hash IS present, and equals SHA-256 of the canonical JSON.
	canonical, _ := json.Marshal(args)
	sum := sha256.Sum256(canonical)
	want := hex.EncodeToString(sum[:])
	if !strings.Contains(string(raw), want) {
		t.Fatalf("audit log lacks input hash %s: %s", want, raw)
	}
	// A hash is not reversible here: the log alone must not let a
	// reader recover the secret without already knowing it.
	entries, err := log.List(0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("List = %d entries, err %v, want 1", len(entries), err)
	}
	if entries[0].InputHash != want {
		t.Errorf("entry InputHash = %s, want %s", entries[0].InputHash, want)
	}
}

func TestAuditOutputTruncationAt200(t *testing.T) {
	log, _ := tempAuditLog(t)
	long := map[string]any{"text": strings.Repeat("x", 500)}
	summary := SummarizeOutput(long)
	if len([]rune(summary)) != MaxOutputSummaryChars {
		t.Fatalf("SummarizeOutput length = %d runes, want %d", len([]rune(summary)), MaxOutputSummaryChars)
	}
	// Short outputs are untouched.
	short := SummarizeOutput(map[string]any{"a": 1})
	if short != `{"a":1}` {
		t.Errorf("short summary = %q, want {\"a\":1}", short)
	}
	// Truncation is by rune: a multi-byte tail is never split.
	unicode := SummarizeOutput(map[string]any{"text": strings.Repeat("é", 500)})
	if len([]rune(unicode)) != MaxOutputSummaryChars {
		t.Fatalf("unicode summary length = %d runes, want %d", len([]rune(unicode)), MaxOutputSummaryChars)
	}
	if !strings.HasPrefix(unicode, `{"text":"`) {
		t.Errorf("unicode summary lost its prefix: %q", unicode)
	}
	if err := log.Record(AuditEntry{
		Tool: "provider_list", CallerKind: KindLocalOperator, CallerID: "local-operator",
		Decision: AuditAllow, InputHash: HashArguments(nil), OutputSummary: summary,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	entries, _ := log.List(0)
	if len(entries) != 1 || len([]rune(entries[0].OutputSummary)) != MaxOutputSummaryChars {
		t.Fatalf("stored entry summary length = %d, want %d", len([]rune(entries[0].OutputSummary)), MaxOutputSummaryChars)
	}
}

func TestAuditDeniedCallsLogged(t *testing.T) {
	log, _ := tempAuditLog(t)
	if err := log.Record(AuditEntry{
		Tool: "router_status", CallerKind: KindGatewayKey, CallerID: "key-7",
		Decision: AuditDeny, Reason: "missing required scope(s): mcp:read:status",
		InputHash: HashArguments(map[string]any{}), ErrorCode: ScopeDeniedCode,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	entries, err := log.List(0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("List = %d entries, err %v, want 1 denied entry", len(entries), err)
	}
	e := entries[0]
	if e.Decision != AuditDeny || e.ErrorCode != ScopeDeniedCode {
		t.Errorf("denied entry = %+v, want deny/-32002", e)
	}
	if e.OutputSummary != "" {
		t.Errorf("denied entry has output summary %q, want empty (no output was produced)", e.OutputSummary)
	}
	if e.CallerID != "key-7" || e.CallerKind != KindGatewayKey {
		t.Errorf("denied entry caller = %s/%s, want gateway-key/key-7", e.CallerKind, e.CallerID)
	}
	if e.Timestamp.IsZero() {
		t.Errorf("denied entry has no timestamp")
	}
}

func TestAuditPersistsAcrossRestart(t *testing.T) {
	log, path := tempAuditLog(t)
	for i := 0; i < 3; i++ {
		if err := log.Record(AuditEntry{
			Tool: "model_list", CallerKind: KindLocalOperator, CallerID: "local-operator",
			Decision: AuditAllow, InputHash: HashArguments(map[string]any{}),
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	// "Restart": open a fresh AuditLog over the same file.
	reopened, err := OpenAuditLog(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	n, err := reopened.Count()
	if err != nil || n != 3 {
		t.Fatalf("reopened Count = %d, err %v, want 3", n, err)
	}
	entries, err := reopened.List(0)
	if err != nil || len(entries) != 3 {
		t.Fatalf("reopened List = %d entries, err %v, want 3", len(entries), err)
	}
	// List is newest-first.
	if !entries[0].Timestamp.After(entries[2].Timestamp) && !entries[0].Timestamp.Equal(entries[2].Timestamp) {
		t.Errorf("List not newest-first: %v then %v", entries[0].Timestamp, entries[2].Timestamp)
	}
	// Limit honoured.
	limited, _ := reopened.List(2)
	if len(limited) != 2 {
		t.Errorf("List(2) = %d entries, want 2", len(limited))
	}
}

func TestAuditListSkipsCorruptLines(t *testing.T) {
	log, path := tempAuditLog(t)
	if err := log.Record(AuditEntry{Tool: "router_status", CallerKind: KindLocalOperator, CallerID: "local-operator", Decision: AuditAllow, InputHash: HashArguments(nil)}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Corrupt the file by hand: one bad line between good ones.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open for corrupt write: %v", err)
	}
	if _, err := f.WriteString("{this is not json\n"); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	_ = f.Close()
	if err := log.Record(AuditEntry{Tool: "model_list", CallerKind: KindLocalOperator, CallerID: "local-operator", Decision: AuditAllow, InputHash: HashArguments(nil)}); err != nil {
		t.Fatalf("Record after corrupt line: %v", err)
	}
	entries, err := log.List(0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("List with a corrupt line = %d entries, err %v, want 2 good entries", len(entries), err)
	}
}

func TestAuditMissingFileIsEmpty(t *testing.T) {
	log, err := OpenAuditLog(filepath.Join(t.TempDir(), "mcp", "audit.jsonl"))
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}
	// The file exists (created by Open) but is empty; and a log
	// whose file was deleted afterwards also reads as empty.
	if err := os.Remove(log.Path()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	entries, err := log.List(0)
	if err != nil || len(entries) != 0 {
		t.Fatalf("List on missing file = %d entries, err %v, want 0/nil", len(entries), err)
	}
	n, err := log.Count()
	if err != nil || n != 0 {
		t.Fatalf("Count on missing file = %d, err %v, want 0/nil", n, err)
	}
}
