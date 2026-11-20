// Per-key guardrail scoping (CHECKLIST 277 (d)): a key with a
// non-empty guardrails list runs only its listed guardrails.
package guardrails

import (
	"context"
	"testing"
)

// scopedSecretText is a fake test-fixture secret shape (an
// OpenAI-style key made of repeated characters — never a real
// credential) plus a PII email, so one text exercises two
// guardrails at once.
const scopedSecretText = "email me at a@b.com with key sk-proj-xxxxxxxxxxxxxxxxxxxx"

func TestCheckScopedRunsOnlyListedGuardrails(t *testing.T) {
	store := NewStoreWith(Config{BlockPII: true, BlockSecrets: true, BlockInjection: true})
	// Scoped to secrets only: the secret blocks, the PII
	// finding is ignored entirely — not counted, not recorded.
	res := store.CheckScoped(scopedSecretText, PhaseRequest, []string{KindSecret})
	if !res.Blocked || res.BlockReason != KindSecret {
		t.Fatalf("scoped [secret]: blocked=%v reason=%q, want blocked/secret", res.Blocked, res.BlockReason)
	}
	stats := store.Stats()
	if stats.ByKind[KindPII] != 0 {
		t.Fatalf("ByKind[pii] = %d, want 0: the unlisted guardrail must not run", stats.ByKind[KindPII])
	}
	if stats.ByKind[KindSecret] == 0 {
		t.Fatalf("ByKind[secret] = 0, want > 0: the listed guardrail must run")
	}
	for _, ev := range stats.Events {
		if ev.Kind == KindPII {
			t.Fatalf("event for unlisted kind pii recorded: %+v", ev)
		}
	}
	// Scoped to PII only: the PII blocks, the secret is ignored.
	res = store.CheckScoped(scopedSecretText, PhaseRequest, []string{KindPII})
	if !res.Blocked || res.BlockReason != KindPII {
		t.Fatalf("scoped [pii]: blocked=%v reason=%q, want blocked/pii", res.Blocked, res.BlockReason)
	}
	// An empty (non-nil) list runs nothing: neither finding
	// blocks, and nothing is counted for the unlisted kinds.
	before := store.Stats()
	res = store.CheckScoped(scopedSecretText, PhaseRequest, []string{})
	if res.Blocked {
		t.Fatalf("scoped []: blocked=%v, want false (no guardrail listed)", res.Blocked)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("scoped []: findings = %+v, want none", res.Findings)
	}
	after := store.Stats()
	if after.ByKind[KindSecret] != before.ByKind[KindSecret] || after.ByKind[KindPII] != before.ByKind[KindPII] {
		t.Fatalf("scoped [] changed ByKind: before=%v after=%v", before.ByKind, after.ByKind)
	}
	// Nil scope is the global behaviour: identical to Check.
	res = store.CheckScoped(scopedSecretText, PhaseRequest, nil)
	if !res.Blocked {
		t.Fatalf("scoped nil: blocked=%v, want true (global behaviour)", res.Blocked)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("scoped nil: findings = %d, want 2 (pii + secret)", len(res.Findings))
	}
}

// stubScopedChecker is an AsyncChecker that always returns the
// configured verdict, for the scoped external-guardrail tests.
type stubScopedChecker struct {
	blocked bool
	reason  string
	calls   int
}

func (c *stubScopedChecker) Check(ctx context.Context, text string) (AsyncVerdict, error) {
	c.calls++
	return AsyncVerdict{Blocked: c.blocked, Reason: c.reason}, nil
}

func TestCheckExternalSyncScopedRunsOnlyListed(t *testing.T) {
	store := NewStoreWith(Config{External: []ExternalConfig{
		{Name: "tox-a", Kind: "toxicity", Mode: ModeSync, Endpoint: "http://127.0.0.1:9/x"},
		{Name: "tox-b", Kind: "toxicity", Mode: ModeSync, Endpoint: "http://127.0.0.1:9/y"},
	}})
	blocker := &stubScopedChecker{blocked: true, reason: "toxicity"}
	allowed := &stubScopedChecker{}
	if err := store.RegisterChecker("tox-a", blocker); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterChecker("tox-b", allowed); err != nil {
		t.Fatal(err)
	}
	// Scoped to tox-b only: tox-a (the blocker) never runs, so
	// nothing blocks — and its checker is never called.
	results := store.CheckExternalSyncScoped(context.Background(), "hello", []string{"tox-b"})
	if len(results) != 1 || results[0].Name != "tox-b" {
		t.Fatalf("scoped results = %+v, want only tox-b", results)
	}
	if results[0].Blocked {
		t.Fatalf("tox-b blocked, want allowed")
	}
	if blocker.calls != 0 {
		t.Fatalf("tox-a checker calls = %d, want 0: the unlisted guardrail must not run", blocker.calls)
	}
	if allowed.calls != 1 {
		t.Fatalf("tox-b checker calls = %d, want 1", allowed.calls)
	}
	// Nil scope runs both: tox-a blocks.
	results = store.CheckExternalSyncScoped(context.Background(), "hello", nil)
	if len(results) != 2 {
		t.Fatalf("unscoped results = %d, want 2", len(results))
	}
	blocked := false
	for _, r := range results {
		blocked = blocked || r.Blocked
	}
	if !blocked {
		t.Fatalf("unscoped: nothing blocked, want tox-a to block")
	}
}

func TestStartAsyncOnScopedRunsOnlyListed(t *testing.T) {
	store := NewStoreWith(Config{External: []ExternalConfig{
		{Name: "slow-a", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/x"},
		{Name: "slow-b", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/y"},
	}})
	a := &stubScopedChecker{}
	b := &stubScopedChecker{}
	if err := store.RegisterChecker("slow-a", a); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterChecker("slow-b", b); err != nil {
		t.Fatal(err)
	}
	run := store.StartAsyncOnScoped("hello", false, "chat", []string{"slow-b"})
	if run == nil {
		t.Fatal("scoped async run is nil, want a run for slow-b")
	}
	run.Wait()
	run.Finish()
	if a.calls != 0 {
		t.Fatalf("slow-a checker calls = %d, want 0", a.calls)
	}
	if b.calls != 1 {
		t.Fatalf("slow-b checker calls = %d, want 1", b.calls)
	}
	// An empty (non-nil) list starts no run at all.
	if run := store.StartAsyncOnScoped("hello", false, "chat", []string{}); run != nil {
		t.Fatalf("scoped [] async run = %+v, want nil (no guardrail listed)", run)
	}
}
