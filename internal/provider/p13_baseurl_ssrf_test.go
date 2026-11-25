// Package provider tests the 7A provider base-URL SSRF policy:
// a provider base URL is an operator-supplied destination the
// gateway dials (and live-tests with the provider key), so it must
// not be able to point at loopback, private space, link-local or
// cloud-metadata space, and it must not carry embedded userinfo.
//
// The policy is split in two by design:
//   - ValidateBaseURL decides what is decidable without the
//     network: embedded userinfo and a LITERAL non-public IP.
//   - the egress guard decides the rest at CONNECTION time: a
//     hostname that resolves to a private address (DNS rebinding)
//     and any redirect hop. Both go through the transport's only
//     dial path.
//
// This file covers the validation half directly. The
// connection-time half — literal/hostname classification, DNS
// rebinding, mixed answers, dial-to-validated-literal-only, and
// the redirect policy — is covered by internal/egress's tests
// (egress_test.go), which exercise the guard's own policy and
// dialer directly.
package provider

import (
	"strings"
	"testing"
)

func TestValidateBaseURLRejectsUserinfo(t *testing.T) {
	cases := []string{
		"https://user:pass@api.example.com/v1",
		"https://user@api.example.com/v1",
		"http://user:secret@api.example.com",
	}
	for _, raw := range cases {
		if _, err := ValidateBaseURL(raw, false); err == nil {
			t.Errorf("ValidateBaseURL(%q) accepted embedded userinfo; want rejection", raw)
		} else if !strings.Contains(err.Error(), "userinfo") {
			t.Errorf("ValidateBaseURL(%q) error = %v; want it to name the userinfo problem", raw, err)
		}
	}
}

func TestValidateBaseURLRejectsNonPublicLiterals(t *testing.T) {
	// IPv4 loopback, "this network", RFC1918 (three ranges),
	// link-local (incl. the cloud metadata endpoint), and the
	// IPv6 loopback / unique-local / link-local equivalents.
	cases := []string{
		"http://127.0.0.1:8080/v1",
		"http://127.0.0.1/v1",
		"http://0.0.0.0:9000/v1",
		"http://10.0.0.5/v1",
		"http://172.16.3.9/v1",
		"http://192.168.1.10/v1",
		"http://169.254.169.254/latest/meta-data",
		"http://169.254.1.1/v1",
		"http://[::1]:8080/v1",
		"http://[::]/v1",
		"http://[fc00::1]/v1",
		"http://[fe80::1]/v1",
		"http://[::ffff:127.0.0.1]/v1", // IPv4-mapped loopback
	}
	for _, raw := range cases {
		if _, err := ValidateBaseURL(raw, false); err == nil {
			t.Errorf("ValidateBaseURL(%q) accepted a non-public destination; want rejection", raw)
		}
	}
}

func TestValidateBaseURLAcceptsPublicDestinations(t *testing.T) {
	// A legitimate public provider endpoint must keep working -
	// the policy refuses non-public space, not the public internet.
	cases := []string{
		"https://api.openai.com/v1",
		"https://api.anthropic.com/v1",
		"https://generativelanguage.googleapis.com/v1beta",
		"http://93.184.216.34:8080/v1", // a public literal
	}
	for _, raw := range cases {
		got, err := ValidateBaseURL(raw, false)
		if err != nil {
			t.Errorf("ValidateBaseURL(%q) = %v; want it accepted (public destination)", raw, err)
			continue
		}
		if !strings.HasPrefix(got, "http") {
			t.Errorf("ValidateBaseURL(%q) = %q; want a normalized http(s) URL", raw, got)
		}
	}
}

func TestValidateBaseURLStillRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"ftp://api.example.com/v1",
		"file:///etc/passwd",
		"not-a-url",
		"https://",
	}
	for _, raw := range cases {
		if _, err := ValidateBaseURL(raw, false); err == nil {
			t.Errorf("ValidateBaseURL(%q) accepted a malformed base URL; want rejection", raw)
		}
	}
}
