package metrics

import (
	"math"
	"regexp"
	"strings"
	"testing"
)

// seriesLine matches an exposition sample with a provider/model/kind
// label set. Nothing outside [a-zA-Z0-9_:] may appear inside a label.
var seriesLine = regexp.MustCompile(`^codvian_router_usage_(tokens_total|cost_usd_total)\{provider="([a-zA-Z0-9_:]*)",model="([a-zA-Z0-9_:]*)",(?:kind|source)="([a-zA-Z0-9_:]*)"\} (.+)$`)

func TestUsageCounters(t *testing.T) {
	r := NewRecorder()
	var b strings.Builder
	r.WriteTo(&b)
	if !strings.Contains(b.String(), "# TYPE codvian_router_usage_tokens_total counter") ||
		!strings.Contains(b.String(), "# TYPE codvian_router_usage_cost_usd_total counter") {
		t.Fatal("empty recorder must still expose HELP/TYPE for usage metrics")
	}
	if strings.Contains(b.String(), "codvian_router_usage_tokens_total{") ||
		strings.Contains(b.String(), "codvian_router_usage_cost_usd_total{") {
		t.Fatal("no data must emit no fake zero-valued series")
	}

	r.IncUsageTokens("p", "m", "prompt", 100)
	r.IncUsageTokens("p", "m", "completion", 50)
	r.IncUsageTokens("p", "m", "cached", 20)
	r.IncUsageTokens("p", "m", "reasoning", 10)
	r.IncUsageTokens("p", "m", "garage", 7)  // unknown kind: fixed vocabulary
	r.IncUsageTokens("p", "m", "prompt", -5) // negative: ignored
	r.IncUsageTokens("", "", "prompt", 3)    // empty labels -> unknown
	r.AddReportedCost("p", "m", 0.012345)    // exact to the micro-USD
	r.AddReportedCost("p", "m", 0.000001)    // accumulates
	r.AddReportedCost("p", "m", math.NaN())  // invalid: ignored
	r.AddReportedCost("p", "m", math.Inf(1)) // invalid: ignored
	r.AddReportedCost("p", "m", -1)          // invalid: ignored
	r.AddReportedCost("p", "m", 0.0000001)   // rounds to zero: ignored

	b.Reset()
	r.WriteTo(&b)
	want := []string{
		`codvian_router_usage_tokens_total{provider="p",model="m",kind="prompt"} 100`,
		`codvian_router_usage_tokens_total{provider="p",model="m",kind="completion"} 50`,
		`codvian_router_usage_tokens_total{provider="p",model="m",kind="cached"} 20`,
		`codvian_router_usage_tokens_total{provider="p",model="m",kind="reasoning"} 10`,
		`codvian_router_usage_tokens_total{provider="p",model="m",kind="other"} 7`,
		`codvian_router_usage_tokens_total{provider="unknown",model="unknown",kind="prompt"} 3`,
		`codvian_router_usage_cost_usd_total{provider="p",model="m",source="reported"} 0.012346`,
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	seen := map[string]bool{}
	for _, line := range lines {
		seen[line] = true
	}
	for _, k := range want {
		if !seen[k] {
			t.Errorf("missing exposition line %q\nFULL:\n%s", k, b.String())
		}
	}
	if strings.Contains(b.String(), "kind=\"garage\"") {
		t.Fatal("unknown kind leaked as a label value")
	}
	if strings.Contains(b.String(), "cost_usd_total{provider=\"p\",model=\"m\",source=\"reported\"} 0.012346") {
		// Exact expected value checked above; this guard makes accidental
		// duplicate series obvious when the test is edited later.
		if n := strings.Count(b.String(), `source="reported"} 0.012346`); n != 1 {
			t.Errorf("got %d reported-cost series, want 1", n)
		}
	}
}

func TestUsageLabelSafety(t *testing.T) {
	// Provider-controlled model names and key IDs must never leak
	// into /metrics: sanitize every value to a fixed charset and a
	// fixed kind vocabulary, with no raw secret or payload content.
	const secret = "sk-live-DO-NOT-LEAK-12345"
	r := NewRecorder()
	r.IncUsageTokens(secret, "model \"prompt here\"\n"+secret, "prompt", 11)
	r.AddReportedCost(secret, "gpt-4o\n"+secret, 0.02)
	var b strings.Builder
	r.WriteTo(&b)
	out := b.String()
	if strings.Contains(out, secret) || strings.Contains(out, "DO-NOT-LEAK") {
		t.Fatalf("secret leaked into exposition:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "codvian_router_usage_") {
			continue
		}
		m := seriesLine.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("malformed/unsafe series line: %q", line)
			continue
		}
		if m[4] != "prompt" && m[4] != "reported" {
			t.Errorf("unexpected kind/source label %q", m[4])
		}
	}
}
