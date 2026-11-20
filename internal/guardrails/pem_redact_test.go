package guardrails

import (
	"math/rand"
	"strings"
	"testing"
)

// M1.7 acceptance tests (written FIRST, before the fix): the PEM
// private-key rule must redact the whole BEGIN…END block including
// body and footer — not just the header line — and the same
// redaction must apply to MCP tool args (PhaseMCPToolArgs, which
// flows through Store.Check → scan).

// pemBlock builds a synthetic PEM block with nlines body lines of
// exactly 64 chars drawn from alphabet.
func pemBlock(t *testing.T, label, alphabet string, nlines int, rng *rand.Rand) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("-----BEGIN " + label + "-----\n")
	line := make([]byte, 64)
	for i := 0; i < nlines; i++ {
		for j := range line {
			line[j] = alphabet[rng.Intn(len(alphabet))]
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteString("-----END " + label + "-----")
	return b.String()
}

func TestRedactPEMWholeBlock(t *testing.T) {
	upper := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	lower := "abcdefghijklmnopqrstuvwxyz"
	mixed := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

	cases := []struct {
		name    string
		label   string
		alpha   string
		header  string // "" = canonical upper-case header/footer
		unclose bool
	}{
		{"mixed-body RSA", "RSA PRIVATE KEY", mixed, "", false},
		{"upper-only body (verified leak)", "PRIVATE KEY", upper, "", false},
		{"lower-only body (verified leak)", "PRIVATE KEY", lower, "", false},
		{"lowercase header+footer", "private key", mixed, "lower", false},
		{"encrypted private key", "ENCRYPTED PRIVATE KEY", mixed, "", false},
		{"ec private key", "EC PRIVATE KEY", mixed, "", false},
		{"unclosed block redacted to EOF", "PRIVATE KEY", mixed, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(7))
			block := pemBlock(t, tc.label, tc.alpha, 3, rng)
			if tc.header == "lower" {
				block = strings.Replace(block, "-----BEGIN ", "-----begin ", 1)
				block = strings.Replace(block, "-----END ", "-----end ", 1)
			}
			if tc.unclose {
				// Drop the END footer line entirely.
				if i := strings.LastIndex(block, "-----END"); i >= 0 {
					block = block[:i]
				}
			}
			in := "here is a key:\n" + block + "\nand some trailing text"
			got := Redact(in)
			if strings.Contains(got, "MII") || strings.Contains(got, "-----END") || strings.Contains(got, "-----end") {
				t.Fatalf("Redact leaked PEM footer/body: %q", got)
			}
			// No 64-char body line may survive verbatim.
			for _, line := range strings.Split(block, "\n") {
				if len(line) == 64 && strings.Contains(got, line) {
					t.Fatalf("Redact leaked PEM body line %q in %q", line, got)
				}
			}
			if !strings.Contains(got, "[REDACTED:SECRET]") {
				t.Fatalf("Redact did not stamp the placeholder: %q", got)
			}
			// An unclosed block is redacted to end of text by
			// design (no END footer bounds it), so trailing text
			// after it cannot survive; closed blocks must keep it.
			if !tc.unclose && !strings.Contains(got, "and some trailing text") {
				t.Fatalf("Redact ate trailing text: %q", got)
			}
			found := false
			for _, f := range Findings(in) {
				if f.Pattern == "private-key" && f.Kind == KindSecret {
					found = true
					if f.Count != 1 {
						t.Fatalf("private-key finding count = %d, want 1", f.Count)
					}
				}
			}
			if !found {
				t.Fatalf("Findings(%q) has no private-key finding: %+v", in, Findings(in))
			}
		})
	}
}

func TestRedactPEMTwoBlocks(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	b1 := pemBlock(t, "PRIVATE KEY", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", 2, rng)
	b2 := pemBlock(t, "RSA PRIVATE KEY", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", 2, rng)
	in := b1 + "\nmiddle\n" + b2
	got := Redact(in)
	if strings.Count(got, "[REDACTED:SECRET]") != 2 {
		t.Fatalf("want 2 placeholders, got %q", got)
	}
	if !strings.Contains(got, "middle") {
		t.Fatalf("text between blocks lost: %q", got)
	}
	for _, line := range strings.Split(in, "\n") {
		if len(line) == 64 && strings.Contains(got, line) {
			t.Fatalf("leaked body line %q", line)
		}
	}
}

func TestRedactPEMHeaderOnlyStillRedacted(t *testing.T) {
	// The prefilter pin: a bare header (no body, no END) is still a
	// private-key finding — this is the input the old header-only
	// rule matched, and the new whole-block rule must not regress it.
	in := "-----BEGIN PRIVATE KEY-----"
	if got := Redact(in); got != "[REDACTED:SECRET]" {
		t.Fatalf("Redact(%q) = %q, want placeholder", in, got)
	}
}

func TestRedactPEMCertificatePassesThrough(t *testing.T) {
	// Scope pin: a CERTIFICATE block is public by design, not a
	// private key — the whole-block rule is PRIVATE KEY only, so
	// its header and footer must survive. (Its base64 body lines
	// may still be touched by the pre-existing generic-token
	// heuristic for mixed-case runs; that is out of scope here.)
	rng := rand.New(rand.NewSource(13))
	block := pemBlock(t, "CERTIFICATE", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", 2, rng)
	got := Redact(block)
	if !strings.Contains(got, "-----BEGIN CERTIFICATE-----") || !strings.Contains(got, "-----END CERTIFICATE-----") {
		t.Fatalf("certificate header/footer were redacted as a private key: %q", got)
	}
}

// FuzzRedactPEMWholeBlock: body lines drawn from upper-only,
// lower-only and mixed alphabets must never survive Redact, for
// any line count / seed. (This is the acceptance fuzz test from
// the M1.7 row: upper/lower/mixed 64-char lines.)
func FuzzRedactPEMWholeBlock(f *testing.F) {
	f.Add(0, 3)
	f.Add(1, 3)
	f.Add(2, 3)
	f.Add(0, 1)
	f.Add(2, 20)
	f.Fuzz(func(t *testing.T, alphaSel, nlines int) {
		alphabets := []string{
			"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
			"abcdefghijklmnopqrstuvwxyz",
			"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/",
		}
		if alphaSel < 0 || alphaSel >= len(alphabets) {
			t.Skip("alphabet selector out of range")
		}
		if nlines < 0 || nlines > 64 {
			t.Skip("line count out of range")
		}
		rng := rand.New(rand.NewSource(int64(alphaSel*1000 + nlines)))
		block := pemBlock(t, "PRIVATE KEY", alphabets[alphaSel], nlines, rng)
		got := Redact("prefix\n" + block + "\nsuffix")
		for _, line := range strings.Split(block, "\n") {
			if len(line) == 64 && strings.Contains(got, line) {
				t.Fatalf("fuzz: leaked 64-char body line %q (alpha=%d lines=%d): %q", line, alphaSel, nlines, got)
			}
		}
		if strings.Contains(got, "-----END") {
			t.Fatalf("fuzz: leaked END footer: %q", got)
		}
		if nlines > 0 && !strings.Contains(got, "[REDACTED:SECRET]") {
			t.Fatalf("fuzz: no placeholder for %d-line block: %q", nlines, got)
		}
		if !strings.Contains(got, "prefix") || !strings.Contains(got, "suffix") {
			t.Fatalf("fuzz: surrounding text lost: %q", got)
		}
	})
}

func TestToolArgsPEMBlockedAndRedacted(t *testing.T) {
	// Tool-args path: MCP tools/call arguments flow through
	// Store.Check(..., PhaseMCPToolArgs) (internal/mcp/mcp.go).
	// A PEM block smuggled in tool args must be redacted from the
	// recorded text and must block when BlockSecrets is on.
	rng := rand.New(rand.NewSource(17))
	block := pemBlock(t, "PRIVATE KEY", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", 3, rng)
	args := `{"name":"read_file","arguments":{"path":"/tmp/k.pem","content":"` + block + `"}}`
	// Normalise the embedded newlines the way toolArgsText sees
	// them: the block is inline here, so build the args with real
	// newlines instead of JSON escapes.
	args = strings.ReplaceAll(args, "\\n", "\n")
	store := NewStoreWith(Config{BlockSecrets: true})
	res := store.Check(args, PhaseMCPToolArgs)
	if !res.Blocked || res.BlockReason != KindSecret {
		t.Fatalf("tool-args PEM: Blocked=%v reason=%q, want blocked/secret (findings %+v)", res.Blocked, res.BlockReason, res.Findings)
	}
	for _, line := range strings.Split(block, "\n") {
		if len(line) == 64 && strings.Contains(res.Redacted, line) {
			t.Fatalf("tool-args PEM: redacted text leaked body line %q", line)
		}
	}
	if strings.Contains(res.Redacted, "-----END") {
		t.Fatalf("tool-args PEM: redacted text leaked END footer: %q", res.Redacted)
	}
}
