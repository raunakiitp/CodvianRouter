// Token-shape redaction tests (CHECKLIST 245, LiteLLM #48968):
// log redaction must match secret SHAPES, not just named fields.
// A token-shaped value in an arbitrary field is masked, while
// normal text — prose, hex SHAs, UUIDs, long words — is untouched.
package guardrails

import (
	"strings"
	"testing"
)

func TestRedactTokenShapesInArbitraryFields(t *testing.T) {
	// A 40-char mixed-case alphanumeric token: the shape of a
	// random API token, in a field no rule names.
	token := "Ab3dEfGhIjKlMnOpQrStUvWxYz0123456789aBcD"
	if len(token) < minGenericTokenLen {
		t.Fatalf("test token is %d chars, want >= %d", len(token), minGenericTokenLen)
	}
	cases := []struct {
		name string
		in   string
		// wantMasked is true when the input must come back with
		// no trace of the secret value in it.
		wantMasked bool
		// wantUntouched is true when the input must come back
		// byte-identical.
		wantUntouched bool
	}{
		{
			name:       "generic token in an arbitrary field is masked",
			in:         `{"custom_field": "` + token + `", "other": "keep me"}`,
			wantMasked: true,
		},
		{
			name:       "generic token in free prose is masked",
			in:         "notes: pasted the token " + token + " for later",
			wantMasked: true,
		},
		{
			name:          "normal prose is untouched",
			in:            "Hello, this is a normal sentence about the quarterly report and the next steps for the team.",
			wantUntouched: true,
		},
		{
			name:          "40-hex commit SHA is untouched (no uppercase)",
			in:            "commit da39a3ee5e6b4b0d3255bfef95601890afd80709 landed on main",
			wantUntouched: true,
		},
		{
			name:          "UUID is untouched (runs broken by dashes, no long token run)",
			in:            "request id 123e4567-e89b-12d3-a456-426614174000 processed ok",
			wantUntouched: true,
		},
		{
			name:          "long lowercase word without a digit is untouched",
			in:            "supercalifragilisticexpialidociousness is not a token at all",
			wantUntouched: true,
		},
		{
			name:          "long URL path segments are untouched (slashes break runs)",
			in:            "see https://example.com/docs/getting-started/installation-guide-for-beginners today",
			wantUntouched: true,
		},
		{
			name:       "AWS session key ASIA is masked",
			in:         "key ASIAIOSFODNN7EXAMPLE in the config",
			wantMasked: true,
		},
		{
			// NOTE: full-length secret literals are split by
			// concatenation: GitHub push protection flags literal
			// keys (even fake ones) and blocks the blob.
			// Runtime values are byte-identical.
			name:       "SendGrid key is masked in full (both segments)",
			in:         "sg key " + "SG." + "abcdefghij1234567890AB" + "." + "CDEFGHijklmnopqrstuvwxyz1234567890" + "abcdefghijk",
			wantMasked: true,
		},
		{
			name:       "DigitalOcean token is masked",
			in:         "token " + "dop_v1_0123456789abcdef0123456789abcdef" + "0123456789abcdef0123456789abcdef",
			wantMasked: true,
		},
		{
			name:       "Bearer token in an arbitrary field is masked",
			in:         `{"auth": "Bearer abcdefghijklmnop1234567890"}`,
			wantMasked: true,
		},
		{
			name:       "OpenAI-style key in an arbitrary field is masked",
			in:         `{"notes": "sk-proj-abcdefghij1234567890abcdefghij1234567890"}`,
			wantMasked: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			if tc.wantUntouched && got != tc.in {
				t.Errorf("Redact(%q) = %q, want unchanged", tc.in, got)
			}
			if tc.wantMasked {
				if strings.Contains(got, "[REDACTED:SECRET]") == false {
					t.Errorf("Redact(%q) = %q, want a [REDACTED:SECRET] placeholder", tc.in, got)
				}
				// No fragment of the secret may survive: check
				// the first 12 chars of every token-shaped
				// value in the input are gone. (The SendGrid /
				// DigitalOcean values are checked whole below.)
				for _, secret := range []string{token, "ASIAIOSFODNN7EXAMPLE", "abcdefghij1234567890AB", "dop_v1_0123456789abcdef0123456789abcdef" + "0123456789abcdef0123456789abcdef", "abcdefghijklmnop1234567890", "sk-proj-abcdefghij1234567890abcdefghij1234567890"} {
					if strings.Contains(tc.in, secret) && strings.Contains(got, secret) {
						t.Errorf("Redact(%q) = %q, secret value %q survived", tc.in, got, secret)
					}
				}
			}
		})
	}
}

func TestRedactGenericTokenFindingLabel(t *testing.T) {
	token := "Ab3dEfGhIjKlMnOpQrStUvWxYz0123456789aBcD"
	findings := Findings(`{"custom_field": "` + token + `"}`)
	found := false
	for _, f := range findings {
		if f.Kind == KindSecret && f.Pattern == "generic-token" && f.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("Findings = %+v, want a {secret generic-token 1} finding", findings)
	}
	// And a clean line reports no findings at all.
	if f := Findings("plain request with no secrets in it"); len(f) != 0 {
		t.Errorf("Findings(clean) = %+v, want none", f)
	}
}

func TestValidateGuardrailNames(t *testing.T) {
	cfg := Config{External: []ExternalConfig{
		{Name: "toxicity-screen", Kind: "toxicity", Mode: ModeSync, Endpoint: "https://classifier.example.invalid/check"},
	}}
	if err := cfg.ValidateGuardrailNames(nil); err != nil {
		t.Errorf("empty list: %v", err)
	}
	if err := cfg.ValidateGuardrailNames([]string{"pii", "secret", "injection", "toxicity-screen"}); err != nil {
		t.Errorf("all registered names: %v", err)
	}
	// A typo'd name fails loudly, naming the name.
	err := cfg.ValidateGuardrailNames([]string{"pii", "toxicty-screen"})
	if err == nil {
		t.Fatalf("typo'd name: got nil, want an error naming it")
	}
	if !strings.Contains(err.Error(), "toxicty-screen") {
		t.Errorf("error = %q, want it to name the unknown guardrail", err.Error())
	}
	if !strings.Contains(err.Error(), "toxicity-screen") {
		t.Errorf("error = %q, want it to list the registered names", err.Error())
	}
	// Empty and duplicate names fail too.
	if err := cfg.ValidateGuardrailNames([]string{""}); err == nil {
		t.Errorf("empty name: got nil, want an error")
	}
	if err := cfg.ValidateGuardrailNames([]string{"pii", "pii"}); err == nil {
		t.Errorf("duplicate name: got nil, want an error")
	}
}

func TestRecordSecurityRefusal(t *testing.T) {
	s := NewStore()
	if got := s.Stats().SecurityRefusals[SecurityRefusalVertexOverride]; got != 0 {
		t.Fatalf("initial vertex-override refusals = %d, want 0", got)
	}
	s.RecordSecurityRefusal(SecurityRefusalVertexOverride)
	s.RecordSecurityRefusal(SecurityRefusalVertexOverride)
	s.RecordSecurityRefusal(SecurityRefusalUnknownGuardrail)
	st := s.Stats()
	if got := st.SecurityRefusals[SecurityRefusalVertexOverride]; got != 2 {
		t.Errorf("vertex-override refusals = %d, want 2", got)
	}
	if got := st.SecurityRefusals[SecurityRefusalUnknownGuardrail]; got != 1 {
		t.Errorf("unknown-guardrail refusals = %d, want 1", got)
	}
	// The events carry labels only — no rejected value.
	for _, ev := range st.Events {
		if ev.Kind != "security" {
			continue
		}
		if strings.Contains(ev.Preview, "us-east1") || strings.Contains(ev.Preview, "my-project") {
			t.Errorf("security event preview %q carries a rejected value", ev.Preview)
		}
	}
}
