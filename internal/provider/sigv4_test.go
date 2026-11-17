package provider

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSignSigV4DocsExample verifies the signer against the AWS
// Signature Version 4 documentation example: GET
// https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08
// with AKIDEXAMPLE / wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY at
// 20150830T123600Z. The expected signature below was verified
// against botocore 1.44.4's SigV4Auth for the identical request
// (docs.aws.amazon.com/general/latest/gr/sig-v4-examples.html).
func TestSignSigV4DocsExample(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	creds := awsCredentials{
		AccessKey: "AKIDEXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	if err := signSigV4(req, nil, creds, "us-east-1", "iam", now); err != nil {
		t.Fatalf("signSigV4: %v", err)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=host;x-amz-date, Signature=b2e4af44cfad96d9ffa3c5653674a927b9b0995c33de22e1f843745ce37c1d5e"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n%q\nwant\n%q", got, want)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
}

func TestSignSigV4BedrockShape(t *testing.T) {
	// A Bedrock Converse request: the signature must cover the
	// payload hash and session token headers, and must change when
	// the body changes.
	creds := awsCredentials{
		AccessKey:    "AKIDEXAMPLE",
		SecretKey:    "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		SessionToken: "session-token-abc",
	}
	body := []byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`)
	newReq := func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-haiku-20240307-v1:0/converse", strings.NewReader(string(body)))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Amz-Content-Sha256", sha256Hex(body))
		return req
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	req := newReq()
	if err := signSigV4(req, body, creds, "us-east-1", "bedrock", now); err != nil {
		t.Fatalf("signSigV4: %v", err)
	}
	auth := req.Header.Get("Authorization")
	// Exact signature, verified against an independent pure-Python
	// SigV4 reference implementation for the identical inputs.
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261117/us-east-1/bedrock/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=27df3e315ec503e463c7cdea7f45c8598656ef6053e288b42555456171c497c2"
	if auth != want {
		t.Errorf("Authorization =\n%q\nwant\n%q", auth, want)
	}
	sig := auth[strings.LastIndex(auth, "Signature=")+len("Signature="):]
	if len(sig) != 64 {
		t.Errorf("signature length = %d, want 64 hex chars", len(sig))
	}
	if req.Header.Get("X-Amz-Security-Token") != "session-token-abc" {
		t.Errorf("X-Amz-Security-Token = %q", req.Header.Get("X-Amz-Security-Token"))
	}
	// Same request, different body: different signature.
	req2 := newReq()
	body2 := []byte(`{"messages":[{"role":"user","content":[{"text":"bye"}]}]}`)
	req2.Header.Set("X-Amz-Content-Sha256", sha256Hex(body2))
	if err := signSigV4(req2, body2, creds, "us-east-1", "bedrock", now); err != nil {
		t.Fatalf("signSigV4: %v", err)
	}
	if req2.Header.Get("Authorization") == auth {
		t.Error("signature did not change when the body changed")
	}
	// Same request, same inputs: deterministic.
	req3 := newReq()
	if err := signSigV4(req3, body, creds, "us-east-1", "bedrock", now); err != nil {
		t.Fatalf("signSigV4: %v", err)
	}
	if req3.Header.Get("Authorization") != auth {
		t.Error("signature is not deterministic for identical inputs")
	}
}

func TestParseAWSCredentials(t *testing.T) {
	cases := []struct {
		key       string
		wantErr   bool
		wantToken string
	}{
		{"AKID:SECRET", false, ""},
		{"AKID:SECRET:TOKEN123", false, "TOKEN123"},
		{"  AKID  :  SECRET  ", false, ""},
		{"k", true, ""},
		{"aws-key", true, ""},
		{"", true, ""},
		{":SECRET", true, ""},
		{"AKID:", true, ""},
		{"a:b:c:d", true, ""},
		{"AK ID:SECRET", true, ""},
	}
	for _, tc := range cases {
		creds, err := parseAWSCredentials(tc.key)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseAWSCredentials(%q): expected error, got %+v", tc.key, creds)
			} else if !strings.Contains(err.Error(), "exchange") {
				t.Errorf("parseAWSCredentials(%q): error %q does not mention exchange", tc.key, err.Error())
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAWSCredentials(%q): %v", tc.key, err)
			continue
		}
		if creds.SessionToken != tc.wantToken {
			t.Errorf("parseAWSCredentials(%q): token = %q, want %q", tc.key, creds.SessionToken, tc.wantToken)
		}
	}
}

func TestRegionFromBaseURL(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://bedrock-runtime.us-east-1.amazonaws.com", "us-east-1"},
		{"https://bedrock-runtime.eu-west-1.amazonaws.com/", "eu-west-1"},
		{"https://bedrock-runtime.ap-south-1.api.aws", "ap-south-1"},
		{"http://127.0.0.1:12345", "us-east-1"},
		{"https://example.com/proxy", "us-east-1"},
		{"", "us-east-1"},
	}
	for _, tc := range cases {
		if got := regionFromBaseURL(tc.base); got != tc.want {
			t.Errorf("regionFromBaseURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}
