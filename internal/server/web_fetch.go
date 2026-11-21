package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// handleWebFetch serves POST /v1/web/fetch.
// Fetches web content from an external URL with SSRF protection, stripping HTML and returning clean content.
// Exa AI is strictly excluded per project constraints.
func (s *Server) handleWebFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large")
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
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "invalid URL scheme: must be http or https")
		return
	}

	// SSRF protection: reject private, loopback, multicast, or link-local hosts
	host := u.Hostname()
	if isPrivateHost(host) {
		writeError(w, http.StatusForbidden, "SSRF blocked: private or loopback addresses are not permitted")
		return
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			if isPrivateHost(req.URL.Hostname()) {
				return fmt.Errorf("SSRF blocked redirect to private host")
			}
			return nil
		},
	}

	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not build request: "+err.Error())
		return
	}
	httpReq.Header.Set("User-Agent", "CodvianRouter/1.0 (WebFetch; +https://github.com/raunakiitp/CodvianRouter)")

	resp, err := client.Do(httpReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch URL: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(http.MaxBytesReader(w, resp.Body, 1<<20))
	if err != nil && err != io.EOF {
		writeError(w, http.StatusBadGateway, "failed reading response body: "+err.Error())
		return
	}

	rawText := string(respBytes)
	title := extractHTMLTitle(rawText)
	cleanContent := stripHTMLTags(rawText)

	writeJSON(w, http.StatusOK, map[string]any{
		"url":          targetURL,
		"status_code":  resp.StatusCode,
		"title":        title,
		"content":      cleanContent,
		"content_type": resp.Header.Get("Content-Type"),
	})
}

func isPrivateHost(host string) bool {
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
	}
	ips, err := net.LookupIP(host)
	if err == nil {
		for _, resolved := range ips {
			if resolved.IsLoopback() || resolved.IsPrivate() || resolved.IsLinkLocalUnicast() || resolved.IsLinkLocalMulticast() || resolved.IsUnspecified() {
				return true
			}
		}
	}
	return false
}

var (
	titleTagRe = regexp.MustCompile(`(?i)<title[^>]*>(.*?)</title>`)
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
