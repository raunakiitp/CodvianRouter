package provider

import (
	"testing"
	"time"
)

func TestParseRetryAfterBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
		ok   bool
	}{
		{
			name: "gemini retryinfo",
			body: `{"error":{"code":429,"message":"Resource exhausted","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`,
			want: 30 * time.Second, ok: true,
		},
		{
			name: "gemini retryinfo fractional",
			body: `{"error":{"details":[{"retryDelay":"1.5s"}]}}`,
			want: 1500 * time.Millisecond, ok: true,
		},
		{
			name: "retryinfo agrees with structured decode",
			body: `{"error":{"code":429,"message":"slow down","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"45s"}]}}`,
			want: 45 * time.Second, ok: true,
		},
		{
			name: "retry in seconds text",
			body: `{"error":{"message":"Too many requests, retry in 5s"}}`,
			want: 5 * time.Second, ok: true,
		},
		{
			name: "retry after minutes text",
			body: `rate limited, please retry after 2 minutes`,
			want: 2 * time.Minute, ok: true,
		},
		{
			name: "retry after seconds word",
			body: `retry after 10 seconds`,
			want: 10 * time.Second, ok: true,
		},
		{
			name: "resets in hms",
			body: `quota resets in 1h2m3s, try again later`,
			want: time.Hour + 2*time.Minute + 3*time.Second, ok: true,
		},
		{
			name: "reset in seconds",
			body: `limit reset in 90s`,
			want: 90 * time.Second, ok: true,
		},
		{
			name: "iso pt30s",
			body: `retry after PT30S`,
			want: 30 * time.Second, ok: true,
		},
		{
			name: "iso pt1m30s",
			body: `{"error":{"message":"retry after PT1M30S"}}`,
			want: 90 * time.Second, ok: true,
		},
		{
			name: "explicit zero is present",
			body: `retry in 0s`,
			want: 0, ok: true,
		},
		{name: "no hint", body: `{"error":{"message":"slow down"}}`, want: 0, ok: false},
		{name: "empty body", body: ``, want: 0, ok: false},
		{name: "not json no hint", body: `internal error`, want: 0, ok: false},
		{name: "bare P is not a duration", body: `retry after P`, want: 0, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRetryAfterBody(tc.body)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ParseRetryAfterBody(%q) = (%v, %v), want (%v, %v)", tc.body, got, ok, tc.want, tc.ok)
			}
		})
	}
	// The regex path and the structured decode must agree on a real
	// Gemini RetryInfo envelope — if they ever diverge, the regex
	// is matching the wrong field.
	body := `{"error":{"code":429,"message":"Resource exhausted","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"45s"}]}}`
	regexD, regexOK := ParseRetryAfterBody(body)
	jsonD, jsonOK := parseRetryInfoDelayJSON(body)
	if regexD != jsonD || regexOK != jsonOK {
		t.Fatalf("regex (%v,%v) != structured (%v,%v)", regexD, regexOK, jsonD, jsonOK)
	}
}
