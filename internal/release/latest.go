package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// LookupKind classifies why a latest-release lookup did not return a
// tag. Only KindNoRelease means "the repository exists and has no
// published release"; every other kind is a failure to find out.
type LookupKind string

const (
	KindNoRelease   LookupKind = "no_published_release"
	KindNotFound    LookupKind = "repository_not_found" // 404 and the repo itself 404s (deleted, renamed, private)
	KindRateLimited LookupKind = "rate_limited"         // 429, or 403 with X-RateLimit-Remaining: 0
	KindAuth        LookupKind = "auth_failed"          // 401 / other 403
	KindUpstream    LookupKind = "upstream_error"       // 5xx or another unexpected status
	KindTimeout     LookupKind = "timeout"
	KindNetwork     LookupKind = "network_error"
	KindMalformed   LookupKind = "malformed_response"
)

// LookupError is the typed failure of LatestTag.
type LookupError struct {
	Kind       LookupKind
	Status     int           // HTTP status, 0 when none
	RetryAfter time.Duration // set for KindRateLimited when the API says
	Err        error
}

func (e *LookupError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("latest release lookup: %s (HTTP %d): %v", e.Kind, e.Status, e.Err)
	}
	return fmt.Sprintf("latest release lookup: %s: %v", e.Kind, e.Err)
}

func (e *LookupError) Unwrap() error { return e.Err }

// IsNoRelease reports whether err is the confirmed "no published
// release" condition.
func IsNoRelease(err error) bool {
	var le *LookupError
	return errors.As(err, &le) && le.Kind == KindNoRelease
}

// LatestTag returns the v-prefixed tag of repo's latest published
// release from the GitHub API at apiBase.
//
// GitHub answers GET /repos/{repo}/releases/latest with 404 both when
// the repository has no published release and when the repository
// does not exist (or is private to this caller). A 404 is therefore
// confirmed by a second request for the repository itself: the
// repository answering 200 makes it KindNoRelease; a 404 there is
// KindNotFound. Any other outcome of that confirmation is reported as
// its own failure, never as "no release".
func LatestTag(ctx context.Context, client *http.Client, apiBase, repo string) (string, error) {
	base := strings.TrimSuffix(apiBase, "/") + "/repos/" + repo
	status, hdr, body, err := apiGet(ctx, client, base+"/releases/latest")
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusOK:
		var v struct {
			TagName string `json:"tag_name"`
			Draft   bool   `json:"draft"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return "", &LookupError{Kind: KindMalformed, Status: status, Err: err}
		}
		if strings.TrimSpace(v.TagName) == "" {
			return "", &LookupError{Kind: KindMalformed, Status: status, Err: errors.New("release has no tag_name")}
		}
		tag, err := NormalizeTag(v.TagName)
		if err != nil {
			return "", &LookupError{Kind: KindMalformed, Status: status, Err: err}
		}
		return tag, nil
	case status == http.StatusNotFound:
		rs, rh, _, err := apiGet(ctx, client, base)
		if err != nil {
			return "", err
		}
		switch {
		case rs == http.StatusOK:
			return "", &LookupError{Kind: KindNoRelease, Status: http.StatusNotFound, Err: errors.New("the repository has no published release")}
		case rs == http.StatusNotFound:
			return "", &LookupError{Kind: KindNotFound, Status: rs, Err: errors.New("repository not found")}
		default:
			return "", statusError(rs, rh)
		}
	default:
		return "", statusError(status, hdr)
	}
}

func statusError(status int, h http.Header) error {
	switch {
	case status == http.StatusTooManyRequests,
		status == http.StatusForbidden && h.Get("X-RateLimit-Remaining") == "0":
		le := &LookupError{Kind: KindRateLimited, Status: status, Err: errors.New("GitHub API rate limit")}
		if s := h.Get("Retry-After"); s != "" {
			var n int
			if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
				le.RetryAfter = time.Duration(n) * time.Second
			}
		}
		return le
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &LookupError{Kind: KindAuth, Status: status, Err: errors.New("GitHub API refused the request")}
	default:
		return &LookupError{Kind: KindUpstream, Status: status, Err: fmt.Errorf("unexpected HTTP %d", status)}
	}
}

func apiGet(ctx context.Context, client *http.Client, url string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, nil, &LookupError{Kind: KindNetwork, Err: err}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return 0, nil, nil, &LookupError{Kind: KindTimeout, Err: err}
		}
		return 0, nil, nil, &LookupError{Kind: KindNetwork, Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, nil, &LookupError{Kind: KindNetwork, Status: resp.StatusCode, Err: err}
	}
	return resp.StatusCode, resp.Header, body, nil
}
