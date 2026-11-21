package server

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	webFetchMaxRequestBytes  = 1 << 20
	webFetchMaxResponseBytes = 1 << 20
	webFetchTimeout          = 10 * time.Second
	webFetchMaxRedirects     = 5
)

// webFetchClient returns the SSRF-safe client for this server. The
// guard and TLS config are nil in production (real DNS, system
// roots); tests set them for deterministic DNS and a trusted test CA.
func (s *Server) webFetchClient() *http.Client {
	g := s.webFetchEgress
	if g == nil {
		g = defaultEgressGuard()
	}
	var tlsCfg *tls.Config
	if s.webFetchTLS != nil {
		tlsCfg = s.webFetchTLS.Clone()
	}
	return g.client(webFetchTimeout, webFetchMaxRedirects, tlsCfg)
}

// handleWebFetch serves POST /v1/web/fetch: fetches one public
// http(s) URL and returns its text with HTML tags stripped. Every
// connection, including each redirect hop, goes through the egress
// guard (egress_guard.go), which validates the resolved IP and then
// dials that same IP, so DNS rebinding cannot reach a private,
// loopback, link-local or metadata address.
func (s *Server) handleWebFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, webFetchMaxRequestBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	targetURL := strings.TrimSpace(req.URL)
	if targetURL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	u, err := url.Parse(targetURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		writeError(w, http.StatusBadRequest, "invalid URL: must be an absolute http or https URL")
		return
	}
	if u.User != nil {
		writeError(w, http.StatusBadRequest, "invalid URL: credentials in the URL are not allowed")
		return
	}

	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not build request: "+err.Error())
		return
	}
	httpReq.Header.Set("User-Agent", "CodvianRouter/1.0 (WebFetch; +https://github.com/raunakiitp/CodvianRouter)")

	resp, err := s.webFetchClient().Do(httpReq)
	if err != nil {
		if errors.Is(err, errEgressBlocked) {
			writeError(w, http.StatusForbidden, "SSRF blocked: "+err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "failed to fetch URL: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Read one byte past the cap so a truncated page is reported
	// as truncated instead of silently cut.
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxResponseBytes+1))
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed reading response body: "+err.Error())
		return
	}
	truncated := len(respBytes) > webFetchMaxResponseBytes
	if truncated {
		respBytes = respBytes[:webFetchMaxResponseBytes]
	}

	rawText := string(respBytes)
	writeJSON(w, http.StatusOK, map[string]any{
		"url":          targetURL,
		"final_url":    resp.Request.URL.String(),
		"status_code":  resp.StatusCode,
		"title":        extractHTMLTitle(rawText),
		"content":      stripHTMLTags(rawText),
		"content_type": resp.Header.Get("Content-Type"),
		"truncated":    truncated,
	})
}

var (
	titleTagRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	tagStripRe = regexp.MustCompile(`<[^>]+>`)
	spaceRe    = regexp.MustCompile(`\s+`)
)

func extractHTMLTitle(html string) string {
	m := titleTagRe.FindStringSubmatch(html)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func stripHTMLTags(html string) string {
	clean := tagStripRe.ReplaceAllString(html, " ")
	clean = spaceRe.ReplaceAllString(clean, " ")
	return strings.TrimSpace(clean)
}
