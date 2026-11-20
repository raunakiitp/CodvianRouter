package evalharness

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/compress"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// TestPrintMeasuredSavingsTable is the harness's single documented
// entry point: it runs both engines over the fixed corpus and
// prints the measured table. The table it prints is the source for
// docs/compression-savings-2026-11-18.md — copy it verbatim, never
// retype numbers.
//
//	go test ./internal/compress/evalharness/ -run TestPrintMeasuredSavingsTable -v -count=1
func TestPrintMeasuredSavingsTable(t *testing.T) {
	results, err := RunCorpus(DefaultModel)
	if err != nil {
		t.Fatalf("harness run failed: %v", err)
	}
	t.Logf("\n%s", RenderTable(results))
}

func TestCorpusCoversRequiredCategories(t *testing.T) {
	cases := Corpus()
	if len(cases) < 12 {
		t.Fatalf("corpus has %d cases, need at least 12", len(cases))
	}
	need := map[string]bool{
		"code": false, "json": false, "prose": false,
		"tool-output": false, "system-heavy": false, "mixed": false,
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if c.Name == "" {
			t.Fatal("corpus case with empty name")
		}
		if seen[c.Name] {
			t.Fatalf("duplicate corpus case name %q", c.Name)
		}
		seen[c.Name] = true
		if len(c.Messages) == 0 {
			t.Fatalf("case %s has no messages", c.Name)
		}
		if c.LossyBudget <= 0 {
			t.Fatalf("case %s has no positive LossyBudget", c.Name)
		}
		need[c.Category] = true
	}
	for cat, ok := range need {
		if !ok {
			t.Fatalf("corpus covers no %q case", cat)
		}
	}
}

// TestNoOpCaseReportsZeroSaved (honesty test (a)): a corpus case
// whose compression is a no-op reports exactly 0 saved — not
// negative, not an error. The case is clean on purpose: one
// user message, no duplicate system block, no redundant
// whitespace, under the lossy budget.
func TestNoOpCaseReportsZeroSaved(t *testing.T) {
	var noop *Case
	for _, c := range Corpus() {
		if c.Name == "noop-clean-prose" {
			cc := c
			noop = &cc
		}
	}
	if noop == nil {
		t.Fatal("corpus has no noop-clean-prose case")
	}
	results, err := Run(DefaultModel, []Case{*noop})
	if err != nil {
		t.Fatalf("harness run failed on the no-op case: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (lossless+lossy), got %d", len(results))
	}
	for _, r := range results {
		if r.SavedTokens != 0 {
			t.Fatalf("engine %s on no-op case saved %d tokens, want 0", r.Engine, r.SavedTokens)
		}
		if r.OutputTokens != r.InputTokens {
			t.Fatalf("engine %s on no-op case: output %d != input %d", r.Engine, r.OutputTokens, r.InputTokens)
		}
		if r.SavedPct != 0 {
			t.Fatalf("engine %s on no-op case: saved%% %v, want 0", r.Engine, r.SavedPct)
		}
	}
}

// TestArithmeticIdentity (honesty test (b)): saved + output ==
// input on every case and every engine, enforced by the test, not
// just by construction.
func TestArithmeticIdentity(t *testing.T) {
	results, err := RunCorpus(DefaultModel)
	if err != nil {
		t.Fatalf("harness run failed: %v", err)
	}
	for _, r := range results {
		if r.SavedTokens+r.OutputTokens != r.InputTokens {
			t.Fatalf("case %s engine %s: saved(%d)+output(%d) != input(%d)",
				r.CaseName, r.Engine, r.SavedTokens, r.OutputTokens, r.InputTokens)
		}
		if r.SavedTokens < 0 {
			t.Fatalf("case %s engine %s: negative savings %d", r.CaseName, r.Engine, r.SavedTokens)
		}
		if r.InputTokens <= 0 {
			t.Fatalf("case %s engine %s: non-positive input tokens %d", r.CaseName, r.Engine, r.InputTokens)
		}
	}
}

// TestHarnessFailsOnTamperedNumbers (honesty test (c)): if any
// case's reported numbers don't match a re-count, verification
// fails loudly. Tamper each field in turn and demand an error.
func TestHarnessFailsOnTamperedNumbers(t *testing.T) {
	cases := Corpus()
	original := cases[0].Messages
	lossless := true
	compressed, _ := compress.Apply(DefaultModel, original,
		&provider.CompressionRequest{Lossless: &lossless}, nil, nil)
	good, err := Measure(DefaultModel, cases[0], EngineLossless, original, compressed)
	if err != nil {
		t.Fatalf("Measure failed on an untampered case: %v", err)
	}
	if err := VerifyResult(DefaultModel, cases[0], good, original, compressed); err != nil {
		t.Fatalf("VerifyResult rejected honest numbers: %v", err)
	}
	tampered := good
	tampered.SavedTokens++
	if err := VerifyResult(DefaultModel, cases[0], tampered, original, compressed); err == nil {
		t.Fatal("VerifyResult accepted a tampered SavedTokens (+1) — the harness must fail loudly")
	}
	tampered = good
	tampered.OutputTokens++
	if err := VerifyResult(DefaultModel, cases[0], tampered, original, compressed); err == nil {
		t.Fatal("VerifyResult accepted a tampered OutputTokens (+1)")
	}
	tampered = good
	tampered.InputTokens--
	if err := VerifyResult(DefaultModel, cases[0], tampered, original, compressed); err == nil {
		t.Fatal("VerifyResult accepted a tampered InputTokens (-1)")
	}
	// A Result whose saved+output no longer equals input must fail
	// even if it somehow matched a recount field-by-field: the
	// identity check is independent.
	broken := good
	broken.SavedTokens = good.SavedTokens + 1
	broken.OutputTokens = good.OutputTokens // identity now broken
	if broken.SavedTokens+broken.OutputTokens == broken.InputTokens {
		t.Fatal("test setup error: tampered row still satisfies the identity")
	}
	if err := VerifyResult(DefaultModel, cases[0], broken, original, compressed); err == nil {
		t.Fatal("VerifyResult accepted a row breaking saved+output==input")
	}
}

// TestDeterminism (honesty test (d)): two runs produce
// byte-identical tables.
func TestDeterminism(t *testing.T) {
	first, err := RunCorpus(DefaultModel)
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	second, err := RunCorpus(DefaultModel)
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	a, b := RenderTable(first), RenderTable(second)
	if a != b {
		t.Fatalf("two runs produced different tables:\n--- first ---\n%s\n--- second ---\n%s", a, b)
	}
	if !strings.Contains(a, "TOTAL (lossless)") || !strings.Contains(a, "TOTAL (lossy)") {
		t.Fatalf("table is missing a TOTAL row:\n%s", a)
	}
}

// TestLosslessOutputsVerified pins that every lossless result in
// the corpus passes VerifyLossless — the harness refuses to
// report a lossless case it cannot vouch for.
func TestLosslessOutputsVerified(t *testing.T) {
	results, err := RunCorpus(DefaultModel)
	if err != nil {
		t.Fatalf("harness run failed: %v", err)
	}
	for _, r := range results {
		if r.Engine == EngineLossless && !r.LosslessVerified {
			t.Fatalf("case %s: lossless output not verified", r.CaseName)
		}
		if r.Engine == EngineLossy && r.LosslessVerified {
			t.Fatalf("case %s: lossy output must never claim LosslessVerified", r.CaseName)
		}
	}
}

// TestGoldenInputTokens pins the corpus: the exact BPE input count
// of every case is recorded here from a real harness run. If a
// corpus literal is edited — deliberately or by accident — the
// count moves and this test fails, so the measured table can never
// silently drift from the corpus it claims to measure.
//
// TOGGLE EVIDENCE: change any number below (e.g. go-code-fenced
// 0 → 1) and this test FAILS; restore it and it passes.
func TestGoldenInputTokens(t *testing.T) {
	for _, c := range Corpus() {
		got, err := tokencount.CountPrompt(DefaultModel, c.Messages)
		if err != nil {
			t.Fatalf("case %s: counting failed: %v", c.Name, err)
		}
		want, ok := goldenInputTokens[c.Name]
		if !ok {
			t.Fatalf("case %s: no golden input-token count recorded — add it from a real run", c.Name)
		}
		if got.Tokens != want {
			t.Fatalf("case %s: input tokens %d, golden %d — corpus literal changed or counting changed; re-measure before updating the golden",
				c.Name, got.Tokens, want)
		}
	}
}
