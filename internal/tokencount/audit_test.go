package tokencount_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

func TestCorpusValid(t *testing.T) {
	corpus := tokencount.Corpus()
	if len(corpus) != 18 {
		t.Fatalf("Corpus() = %d cases, want 18 (3 per class x 6 classes)", len(corpus))
	}
	seen := map[string]bool{}
	perClass := map[tokencount.CorpusClass]int{}
	for _, c := range corpus {
		if c.ID == "" {
			t.Errorf("case with empty ID in class %s", c.Class)
		}
		if seen[c.ID] {
			t.Errorf("duplicate corpus case ID %q", c.ID)
		}
		seen[c.ID] = true
		perClass[c.Class]++
		if len(c.Messages) == 0 {
			t.Errorf("case %s: no messages", c.ID)
		}
		for _, m := range c.Messages {
			if m.Role == "" {
				t.Errorf("case %s: message with empty role", c.ID)
			}
		}
		for _, td := range c.Tools {
			if td.Type != "function" || td.Function.Name == "" {
				t.Errorf("case %s: malformed tool definition %+v", c.ID, td)
			}
			if len(td.Function.Parameters) > 0 {
				var raw json.RawMessage
				if err := json.Unmarshal(td.Function.Parameters, &raw); err != nil {
					t.Errorf("case %s: tool %s has invalid parameters JSON: %v", c.ID, td.Function.Name, err)
				}
			}
		}
	}
	for _, class := range tokencount.AllClasses {
		if perClass[class] != 3 {
			t.Errorf("class %s: %d cases, want 3", class, perClass[class])
		}
	}
	// The tool-call-heavy class must actually carry tools, and no
	// other class may (a stray tool would silently change what the
	// harness measures for that class).
	for _, c := range corpus {
		if c.Class == tokencount.ClassToolCallHeavy && len(c.Tools) == 0 {
			t.Errorf("case %s: tool-call-heavy case with no tools", c.ID)
		}
		if c.Class != tokencount.ClassToolCallHeavy && len(c.Tools) != 0 {
			t.Errorf("case %s: non-tool class carries tools", c.ID)
		}
	}
	// The long-repetitive cases must actually be long: the class
	// exists to test BPE merge behaviour on repeated text.
	for _, c := range corpus {
		if c.Class != tokencount.ClassLongRepetitive {
			continue
		}
		var n int
		for _, m := range c.Messages {
			n += len(m.Content)
		}
		if n < 2000 {
			t.Errorf("case %s: %d chars, want a long repetitive prompt (>= 2000 chars)", c.ID, n)
		}
	}
}

func TestCorpusCountsDifferByClass(t *testing.T) {
	// Sanity: the local counter sees the corpus as genuinely
	// different prompts — a corpus where every case counted the
	// same would measure nothing. Uses the real BPE counter.
	var prev int
	seen := map[int]bool{}
	for _, c := range tokencount.Corpus() {
		count, err := tokencount.CountPrompt("gpt-4o-mini", c.Messages)
		if err != nil {
			t.Fatalf("case %s: CountPrompt: %v", c.ID, err)
		}
		if count.Tokens <= 0 {
			t.Errorf("case %s: local count %d, want > 0", c.ID, count.Tokens)
		}
		seen[count.Tokens] = true
		_ = prev
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct local counts across 18 cases — corpus is too uniform", len(seen))
	}
	// The tool-conversation case counts its tool-call arguments
	// (CountPrompt covers tool-call arguments, just not tool
	// definitions): the arguments JSON must contribute tokens.
	var conv []provider.Message
	for _, c := range tokencount.Corpus() {
		if c.ID == "tool-conv" {
			conv = c.Messages
		}
	}
	withArgs, err := tokencount.CountPrompt("gpt-4o-mini", conv)
	if err != nil {
		t.Fatal(err)
	}
	stripped := append([]provider.Message{}, conv...)
	for i := range stripped {
		stripped[i].ToolCalls = nil
	}
	withoutArgs, err := tokencount.CountPrompt("gpt-4o-mini", stripped)
	if err != nil {
		t.Fatal(err)
	}
	if withArgs.Tokens <= withoutArgs.Tokens {
		t.Errorf("tool-call arguments add no tokens: with=%d without=%d", withArgs.Tokens, withoutArgs.Tokens)
	}
}

func TestComputeDrift(t *testing.T) {
	samples := []tokencount.Sample{
		// OpenAI / english: local undercounts by 10% twice, overcounts once.
		{Wire: "openai", Class: tokencount.ClassEnglish, CaseID: "a", Local: 90, Reported: 100},
		{Wire: "openai", Class: tokencount.ClassEnglish, CaseID: "b", Local: 180, Reported: 200},
		{Wire: "openai", Class: tokencount.ClassEnglish, CaseID: "c", Local: 110, Reported: 100},
		// Another wire/class must not leak into this cell.
		{Wire: "anthropic", Class: tokencount.ClassEnglish, CaseID: "a", Local: 50, Reported: 100},
		{Wire: "openai", Class: tokencount.ClassCode, CaseID: "a", Local: 50, Reported: 100},
		// Reported 0 is not a comparison (no provider number).
		{Wire: "openai", Class: tokencount.ClassEnglish, CaseID: "d", Local: 10, Reported: 0},
	}
	st := tokencount.ComputeDrift("openai", tokencount.ClassEnglish, samples)
	if st.N != 3 {
		t.Fatalf("N = %d, want 3", st.N)
	}
	// Mean error: (-10 -10 +10) / 3 = -3.333...
	if got, want := st.MeanErrPct, -10.0/3.0; got < want-0.001 || got > want+0.001 {
		t.Errorf("MeanErrPct = %.4f, want %.4f", got, want)
	}
	if got, want := st.MeanAbsPct, 10.0; got != want {
		t.Errorf("MeanAbsPct = %.4f, want %.4f", got, want)
	}
	if got, want := st.MaxAbsPct, 10.0; got != want {
		t.Errorf("MaxAbsPct = %.4f, want %.4f", got, want)
	}
	// Empty cell: N=0, no NaN.
	empty := tokencount.ComputeDrift("bedrock", tokencount.ClassCJK, samples)
	if empty.N != 0 || empty.MeanErrPct != 0 || empty.MeanAbsPct != 0 || empty.MaxAbsPct != 0 {
		t.Errorf("empty cell = %+v, want all zeros", empty)
	}
}

func TestSummarizeAndMarkdownTable(t *testing.T) {
	samples := []tokencount.Sample{
		{Wire: "openai", Class: tokencount.ClassEnglish, CaseID: "a", Local: 90, Reported: 100},
		{Wire: "anthropic", Class: tokencount.ClassCode, CaseID: "a", Local: 95, Reported: 100},
	}
	stats := tokencount.Summarize(samples)
	if len(stats) != 2 {
		t.Fatalf("Summarize = %d cells, want 2", len(stats))
	}
	if stats[0].Wire != "anthropic" || stats[1].Wire != "openai" {
		t.Errorf("cells not wire-ordered: %+v", stats)
	}
	table := tokencount.MarkdownTable(stats)
	if !strings.Contains(table, "| anthropic | code | 1 | -5.00 | 5.00 | 5.00 |") {
		t.Errorf("table missing anthropic row:\n%s", table)
	}
	if !strings.Contains(table, "| openai | english | 1 | -10.00 | 10.00 | 10.00 |") {
		t.Errorf("table missing openai row:\n%s", table)
	}
}

func TestSampleErrPct(t *testing.T) {
	s := tokencount.Sample{Local: 80, Reported: 100}
	if s.ErrPct() != -20 {
		t.Errorf("ErrPct = %v, want -20", s.ErrPct())
	}
	if (tokencount.Sample{Local: 10, Reported: 0}).ErrPct() != 0 {
		t.Errorf("reported 0 must give 0 error, not NaN/Inf")
	}
}
