package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testRepo = "owner/repo"

// ghServer fakes the two GitHub endpoints LatestTag may call.
func ghServer(t *testing.T, latest, repo http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+testRepo+"/releases/latest", latest)
	if repo != nil {
		mux.HandleFunc("/repos/"+testRepo, repo)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
}

func kindOf(t *testing.T, err error) LookupKind {
	t.Helper()
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v (%T), want *LookupError", err, err)
	}
	return le.Kind
}

func TestLatestTagValidRelease(t *testing.T) {
	srv := ghServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.3"}`))
	}, nil)
	tag, err := LatestTag(context.Background(), srv.Client(), srv.URL, testRepo)
	if err != nil || tag != "v1.2.3" {
		t.Fatalf("tag=%q err=%v, want v1.2.3", tag, err)
	}
}

func TestLatestTagNoPublishedRelease(t *testing.T) {
	var repoCalls int
	srv := ghServer(t, status(http.StatusNotFound), func(w http.ResponseWriter, r *http.Request) {
		repoCalls++
		_, _ = w.Write([]byte(`{"full_name":"owner/repo"}`))
	})
	tag, err := LatestTag(context.Background(), srv.Client(), srv.URL, testRepo)
	if tag != "" || !IsNoRelease(err) {
		t.Fatalf("tag=%q err=%v, want no tag and KindNoRelease", tag, err)
	}
	if repoCalls != 1 {
		t.Fatalf("repository confirmation calls = %d, want 1", repoCalls)
	}
}

// A 404 for both the release and the repository is NOT "no release":
// the repository itself is missing or private.
func TestLatestTagRepositoryMissing(t *testing.T) {
	srv := ghServer(t, status(http.StatusNotFound), status(http.StatusNotFound))
	_, err := LatestTag(context.Background(), srv.Client(), srv.URL, testRepo)
	if IsNoRelease(err) || kindOf(t, err) != KindNotFound {
		t.Fatalf("err = %v, want KindNotFound", err)
	}
}

func TestLatestTagRateLimited(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"429": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		},
		"403 primary limit": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
		},
	} {
		srv := ghServer(t, h, nil)
		_, err := LatestTag(context.Background(), srv.Client(), srv.URL, testRepo)
		if IsNoRelease(err) || kindOf(t, err) != KindRateLimited {
			t.Fatalf("%s: err = %v, want KindRateLimited", name, err)
		}
		var le *LookupError
		errors.As(err, &le)
		if name == "429" && le.RetryAfter != 30*time.Second {
			t.Errorf("RetryAfter = %v, want 30s", le.RetryAfter)
		}
	}
}

func TestLatestTagAuthAndUpstreamErrors(t *testing.T) {
	for code, want := range map[int]LookupKind{
		http.StatusUnauthorized:        KindAuth,
		http.StatusForbidden:           KindAuth,
		http.StatusInternalServerError: KindUpstream,
		http.StatusBadGateway:          KindUpstream,
		http.StatusServiceUnavailable:  KindUpstream,
	} {
		srv := ghServer(t, status(code), nil)
		_, err := LatestTag(context.Background(), srv.Client(), srv.URL, testRepo)
		if IsNoRelease(err) || kindOf(t, err) != want {
			t.Fatalf("HTTP %d: err = %v, want %s", code, err, want)
		}
	}
	// A 404 whose repository confirmation fails with 5xx stays an
	// upstream error, never "no release".
	srv := ghServer(t, status(http.StatusNotFound), status(http.StatusInternalServerError))
	_, err := LatestTag(context.Background(), srv.Client(), srv.URL, testRepo)
	if IsNoRelease(err) || kindOf(t, err) != KindUpstream {
		t.Fatalf("404 + repo 500: err = %v, want KindUpstream", err)
	}
}

func TestLatestTagMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"not json":  `<html>oops</html>`,
		"no tag":    `{"name":"release"}`,
		"empty tag": `{"tag_name":"  "}`,
	} {
		srv := ghServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }, nil)
		tag, err := LatestTag(context.Background(), srv.Client(), srv.URL, testRepo)
		if tag != "" || kindOf(t, err) != KindMalformed {
			t.Fatalf("%s: tag=%q err=%v, want KindMalformed", name, tag, err)
		}
	}
}

func TestLatestTagTimeoutAndNetwork(t *testing.T) {
	release := make(chan struct{})
	srv := ghServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, nil)
	defer close(release)
	client := &http.Client{Timeout: 100 * time.Millisecond}
	_, err := LatestTag(context.Background(), client, srv.URL, testRepo)
	if IsNoRelease(err) || kindOf(t, err) != KindTimeout {
		t.Fatalf("slow API: err = %v, want KindTimeout", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = LatestTag(ctx, srv.Client(), srv.URL, testRepo)
	if kindOf(t, err) != KindTimeout {
		t.Fatalf("context deadline: err = %v, want KindTimeout", err)
	}
	// Nothing listens here: a network error, not a timeout or no-release.
	_, err = LatestTag(context.Background(), &http.Client{}, "http://127.0.0.1:1", testRepo)
	if kindOf(t, err) != KindNetwork {
		t.Fatalf("refused connection: err = %v, want KindNetwork", err)
	}
}
