// Audit rotation tests (M9.7): the log is capped at MaxAuditEntries
// (10,000 by default); when a Record would exceed the cap the
// OLDEST entries rotate out first and the newest are never lost.
package mcp

import (
	"fmt"
	"testing"
)

func TestAuditRotationDropsOldestKeepsNewest(t *testing.T) {
	log, _ := tempAuditLog(t)
	// Shrink the cap for the test (same package, so the field is
	// reachable): 8 records against a cap of 5 must drop exactly
	// the 3 oldest and keep the 5 newest, in order.
	log.maxEntries = 5
	for i := 1; i <= 8; i++ {
		tool := fmt.Sprintf("tool-%d", i)
		if err := log.Record(AuditEntry{
			Tool: tool, CallerKind: KindLocalOperator, CallerID: "local-operator",
			Decision: AuditAllow, InputHash: HashArguments(map[string]any{"n": i}),
			OutputSummary: SummarizeOutput(map[string]any{"n": i}),
		}); err != nil {
			t.Fatalf("Record %s: %v", tool, err)
		}
	}
	if got := log.Cap(); got != 5 {
		t.Errorf("Cap() = %d, want 5 (test cap)", got)
	}
	if got := log.Dropped(); got != 3 {
		t.Errorf("Dropped() = %d, want 3 (the 3 oldest entries)", got)
	}
	n, err := log.Count()
	if err != nil || n != 5 {
		t.Fatalf("Count() = %d, err %v, want 5", n, err)
	}
	entries, err := log.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("List = %d entries, want 5", len(entries))
	}
	// Newest first: tool-8 down to tool-4. tool-1..tool-3 are gone.
	for i, want := range []string{"tool-8", "tool-7", "tool-6", "tool-5", "tool-4"} {
		if entries[i].Tool != want {
			t.Errorf("entries[%d].Tool = %q, want %q", i, entries[i].Tool, want)
		}
	}
	for _, e := range entries {
		if e.Tool == "tool-1" || e.Tool == "tool-2" || e.Tool == "tool-3" {
			t.Errorf("rotated-out entry %q is still in the log", e.Tool)
		}
	}
}

func TestAuditRotationSurvivesReopen(t *testing.T) {
	log, path := tempAuditLog(t)
	log.maxEntries = 3
	for i := 1; i <= 5; i++ {
		if err := log.Record(AuditEntry{
			Tool: fmt.Sprintf("tool-%d", i), CallerKind: KindLocalOperator,
			CallerID: "local-operator", Decision: AuditAllow,
			InputHash: HashArguments(map[string]any{"n": i}),
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if n, _ := log.Count(); n != 3 {
		t.Fatalf("Count() = %d, want 3 before reopen", n)
	}
	// Reopen: the kept entries are still there, and the cap is
	// back to the default (the test cap was in-memory only).
	reopened, err := OpenAuditLog(path)
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}
	if n, err := reopened.Count(); err != nil || n != 3 {
		t.Fatalf("reopened Count() = %d, err %v, want 3", n, err)
	}
	if got := reopened.Cap(); got != MaxAuditEntries {
		t.Errorf("reopened Cap() = %d, want default %d", got, MaxAuditEntries)
	}
	// Dropped() is a process-lifetime counter: a fresh open has
	// seen no rotation of its own, so it reports 0 even though
	// entries were rotated away before the reopen.
	if got := reopened.Dropped(); got != 0 {
		t.Errorf("reopened Dropped() = %d, want 0 (process-lifetime counter)", got)
	}
}

func TestAuditDefaultCap(t *testing.T) {
	log, _ := tempAuditLog(t)
	if got := log.Cap(); got != MaxAuditEntries {
		t.Errorf("Cap() = %d, want MaxAuditEntries %d", got, MaxAuditEntries)
	}
	if got := log.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d on a fresh log, want 0", got)
	}
}
