// Package provider — AWS SigV4 request signing for Bedrock.
//
// Bedrock's Converse surface takes no static API key: every request
// is signed with AWS Signature Version 4 from the operator's AWS
// credentials (access key + secret, optional session token). This
// file is a minimal, stdlib-only SigV4 signer (crypto/hmac,
// crypto/sha256): no AWS SDK is vendored, and no credential exchange
// is faked as a static header.
//
// Credential format (the provider key material, sealed in the vault
// like any other key): "AWS_ACCESS_KEY_ID:AWS_SECRET_ACCESS_KEY"
// with an optional third part ":AWS_SESSION_TOKEN" for temporary
// credentials. The region comes from the base URL host
// (bedrock-runtime.{region}.amazonaws.com); for non-AWS hosts
// (mocks, proxies) it defaults to us-east-1, which keeps the
// signature well-formed — a wrong region against real AWS fails
// loudly at the upstream, never silently.
//
// The signer signs exactly the headers it sets (host, x-amz-date,
// x-amz-content-sha256 when present, x-amz-security-token when a
// session token is present). The request body is hashed, never
// logged; the secret key never leaves this file.
package provider

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// awsCredentials are the AWS credentials for one SigV4-signed
// provider: access key, secret, and an optional session token for
// temporary credentials.
type awsCredentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
}

// errBadAWSCredentials is returned when a provider key is not valid
// AWS credential material. It names the exchange so callers that
// refuse exchange auth keep matching on "exchange".
func errBadAWSCredentials(reason string) error {
	return fmt.Errorf("bedrock SigV4 credential exchange required: provider key must be \"AWS_ACCESS_KEY_ID:AWS_SECRET_ACCESS_KEY\" with an optional \":AWS_SESSION_TOKEN\" (%s)", reason)
}

// parseAWSCredentials parses provider key material into AWS
// credentials. The key must be "ACCESS:SECRET" or
// "ACCESS:SECRET:TOKEN" (empty parts are rejected); anything else is
// not AWS credential material at all and the exchange stays refused.
func parseAWSCredentials(key string) (awsCredentials, error) {
	parts := strings.Split(key, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return awsCredentials{}, errBadAWSCredentials("expected 2 or 3 colon-separated parts")
	}
	creds := awsCredentials{
		AccessKey: strings.TrimSpace(parts[0]),
		SecretKey: strings.TrimSpace(parts[1]),
	}
	if len(parts) == 3 {
		creds.SessionToken = strings.TrimSpace(parts[2])
	}
	if creds.AccessKey == "" {
		return awsCredentials{}, errBadAWSCredentials("access key id is empty")
	}
	if creds.SecretKey == "" {
		return awsCredentials{}, errBadAWSCredentials("secret access key is empty")
	}
	if strings.ContainsAny(creds.AccessKey, " \t\n") || strings.ContainsAny(creds.SecretKey, " \t\n") {
		return awsCredentials{}, errBadAWSCredentials("credential parts must not contain whitespace")
	}
	return creds, nil
}

// bedrockRegionRe matches a Bedrock runtime host and captures the
// region: bedrock-runtime.{region}.amazonaws.com (and the .api.aws
// form).
var bedrockRegionRe = regexp.MustCompile(`^bedrock-runtime\.([a-z0-9-]+)\.(amazonaws\.com|api\.aws)$`)

// regionFromBaseURL extracts the AWS region from a Bedrock base URL
// host. Non-AWS hosts (mocks, proxies) have no region to extract and
// fall back to us-east-1 so the signature stays well-formed.
func regionFromBaseURL(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return "us-east-1"
	}
	host := u.Hostname()
	if m := bedrockRegionRe.FindStringSubmatch(host); m != nil {
		return m[1]
	}
	return "us-east-1"
}

// hmacSHA256 returns HMAC-SHA256(key, data).
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// sha256Hex returns the hex SHA-256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// signSigV4 signs req in place with AWS Signature Version 4 for
// service in region, using creds, at time now. It sets X-Amz-Date
// and X-Amz-Security-Token (when a session token is present), and
// Authorization. The canonical request signs exactly the headers
// listed in SignedHeaders: host and x-amz-date always, plus
// x-amz-content-sha256 when the caller pre-set that header (the
// Bedrock client always does — it hashes the body it sends) and
// x-amz-security-token when a session token is present. The body is
// passed separately because req.Body may already be consumed by the
// time signing runs; the caller hashes the same bytes it sends.
//
// The implementation follows the AWS Signature Version 4 signing
// process (docs.aws.amazon.com/general/latest/gr/signature-version-4):
// canonical request -> string to sign -> signing key chain
// (kDate/kRegion/kService/kSigning) -> signature.
func signSigV4(req *http.Request, body []byte, creds awsCredentials, region, service string, now time.Time) error {
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return errBadAWSCredentials("access key id or secret access key is empty")
	}
	if region == "" {
		region = "us-east-1"
	}
	if service == "" {
		service = "bedrock"
	}
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := sha256Hex(body)
	req.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	// Canonical headers, in sorted order: host, x-amz-content-sha256
	// (only when the caller pre-set it), x-amz-date,
	// x-amz-security-token (only with a session token).
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	values := map[string]string{
		"host":                 strings.TrimSpace(host),
		"x-amz-date":           amzDate,
		"x-amz-content-sha256": strings.TrimSpace(req.Header.Get("X-Amz-Content-Sha256")),
		"x-amz-security-token": strings.TrimSpace(creds.SessionToken),
	}
	signedHeaders := []string{"host", "x-amz-date"}
	if values["x-amz-content-sha256"] != "" {
		signedHeaders = append(signedHeaders, "x-amz-content-sha256")
	}
	if values["x-amz-security-token"] != "" {
		signedHeaders = append(signedHeaders, "x-amz-security-token")
	}
	sort.Strings(signedHeaders)
	var canonicalHeaderLines []string
	for _, h := range signedHeaders {
		canonicalHeaderLines = append(canonicalHeaderLines, h+":"+values[h])
	}
	signedHeadersStr := strings.Join(signedHeaders, ";")
	canonicalHeaders := strings.Join(canonicalHeaderLines, "\n") + "\n"

	canonicalURI := canonicalURI(req.URL)
	canonicalQuery := canonicalQueryString(req.URL)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeadersStr,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))

	kDate := hmacSHA256([]byte("AWS4"+creds.SecretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+creds.AccessKey+"/"+scope+
		", SignedHeaders="+signedHeadersStr+", Signature="+signature)
	return nil
}

// canonicalURI returns the canonical URI for SigV4: each path segment
// URI-encoded, "/" separators preserved, an empty path becoming "/".
func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	// EscapedPath already encodes each segment; a raw path with
	// unencoded characters is encoded segment by segment.
	if path == u.Path {
		segments := strings.Split(u.Path, "/")
		for i, s := range segments {
			segments[i] = url.PathEscape(s)
		}
		path = strings.Join(segments, "/")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

// canonicalQueryString returns the canonical query string: parameters
// sorted by name then value, each name and value URI-encoded per
// SigV4 (RFC 3986 unreserved characters left bare).
func canonicalQueryString(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return u.RawQuery
	}
	var keys []string
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		values := q[k]
		sort.Strings(values)
		for _, v := range values {
			parts = append(parts, sigV4Escape(k)+"="+sigV4Escape(v))
		}
	}
	return strings.Join(parts, "&")
}

// sigV4Escape URI-encodes s per SigV4: RFC 3986 unreserved
// (A-Z a-z 0-9 - _ . ~) are left bare, everything else is
// %-encoded uppercase.
func sigV4Escape(s string) string {
	var buf bytes.Buffer
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			buf.WriteByte(c)
		} else {
			fmt.Fprintf(&buf, "%%%02X", c)
		}
	}
	return buf.String()
}
