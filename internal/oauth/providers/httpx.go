package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxTokenBody caps a token-endpoint response (tokens and endpoint
// metadata — never large).
const maxTokenBody = 64 << 10

// requestTimeout bounds every token-endpoint call made from handlers.
const requestTimeout = 30 * time.Second

// postJSON sends a JSON body and decodes the JSON response into out.
func postJSON(ctx context.Context, cl *http.Client, endpoint string, body any, out any, hdr map[string]string) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("providers: encode request: %w", err)
	}
	return doTokenRequest(ctx, cl, endpoint, http.MethodPost, "application/json", strings.NewReader(string(buf)), out, hdr)
}

// postForm sends a form-encoded body and decodes the JSON response.
func postForm(ctx context.Context, cl *http.Client, endpoint string, form url.Values, out any, hdr map[string]string) error {
	return doTokenRequest(ctx, cl, endpoint, http.MethodPost, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), out, hdr)
}

func doTokenRequest(ctx context.Context, cl *http.Client, endpoint, method, contentType string, body io.Reader, out any, hdr map[string]string) error {
	status, raw, err := doTokenRaw(ctx, cl, endpoint, method, contentType, body, hdr)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("providers: %s: status %d: %s", endpoint, status, truncateBody(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("providers: %s: decode response: %w", endpoint, err)
	}
	return nil
}

// doTokenRaw performs a token-endpoint request and returns the raw
// body. A non-200 status IS returned as data (status, raw, nil) — RFC
// 8628 polls report "authorization pending" over a 400 — except for
// transport-level failures.
func doTokenRaw(ctx context.Context, cl *http.Client, endpoint, method, contentType string, body io.Reader, hdr map[string]string) (status int, raw []byte, err error) {
	if cl == nil {
		return 0, nil, fmt.Errorf("providers: nil HTTP client (the caller must pass the SSRF-guarded egress client)")
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, nil, fmt.Errorf("providers: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("providers: %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	r, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("providers: %s: read response: %w", endpoint, err)
	}
	return resp.StatusCode, r, nil
}

func truncateBody(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// oauthTokenResponse is the standard RFC 6749 token response shape.
type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (r oauthTokenResponse) result(tokenURL string) *TokenResult {
	return &TokenResult{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		TokenType:    r.TokenType,
		Scope:        r.Scope,
		ExpiresIn:    r.ExpiresIn,
		TokenURL:     tokenURL,
	}
}

// decodeBody unmarshals a raw token-endpoint body into out.
func decodeBody(raw []byte, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("providers: decode response: %w", err)
	}
	return nil
}

// errField decodes a standard OAuth error payload shape
// ({"error": "...", "error_description": "..."}).
type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// decodeOAuthError extracts the error code from a non-200 body; ok is
// false when the body is not the standard shape.
func decodeOAuthError(body []byte) (code, desc string, ok bool) {
	var e oauthError
	if err := json.Unmarshal(body, &e); err != nil || e.Error == "" {
		return "", "", false
	}
	return e.Error, e.ErrorDescription, true
}
